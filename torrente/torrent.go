// Package torrente contains the core torrent engine: peer management,
// piece picking, downloading and seeding.
package torrente

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alplix/digitalis/dht"
	"github.com/alplix/digitalis/metainfo"
	"github.com/alplix/digitalis/storage"
	"github.com/alplix/digitalis/tracker"
)

// State represents the lifecycle state of a torrent.
type State string

const (
	StateQueued      State = "queued"
	StateDownloading State = "downloading"
	StateSeeding     State = "seeding"
	StatePaused      State = "paused"
	StateStopped     State = "stopped"
	StateError       State = "error"
)

// Torrent is a single torrent managed by the engine.
type Torrent struct {
	mu *sync.Mutex

	ID string // deterministic short id (first 16 hex chars of info hash)
	InfoHash, Name string
	MetaInfo *metainfo.MetaInfo
	Magnet   *metainfo.Magnet

	// BEP-9 metadata exchange state (magnets). MetaChunks holds partial
	// metadata pieces gathered from peers; MetaSize is the advertised size.
	MetaSize   int64
	MetaChunks map[int][]byte

	State  State
	SaveDir string
	Category string // relative category folder under the base save dir ("" = root)
	AddedAt time.Time

	Downloaded int64
	Uploaded   int64
	TotalWanted int64
	Size       int64

	DownloadSpeed int64
	UploadSpeed   int64

	PeersConnected int
	PeersKnown     int
	Seeders        int
	Leechers       int

	PiecesHave  int
	PiecesTotal int

	// Advanced statistics
	SeededTo     int       `json:"seeded_to"`     // number of distinct peers we have uploaded to
	SeededFirst  time.Time `json:"seeded_first"`  // when we first uploaded any data
	LastSeen     time.Time `json:"last_seen"`     // last time we uploaded or downloaded activity
	FailedTrackers int     `json:"failed_trackers"` // trackers currently in a failing state
	WorkingTrackers int   `json:"working_trackers"` // trackers currently working
	Trackers     []*TrackerStat `json:"trackers"`   // per-tracker stats

	// CustomTrackers are user-added announce URLs; RemovedTrackers are
	// announce URLs (own or custom) the user has disabled for this torrent.
	CustomTrackers  []string          `json:"custom_trackers"`
	RemovedTrackers map[string]bool   `json:"removed_trackers"`
	AnnounceCount   map[string]int64   `json:"announce_count"`
	DownloadLimit   int64             `json:"download_limit"`
	RatioTarget     float64           `json:"ratio_target"` // per-torrent ratio goal; 0 = use global
	SeedDaysTarget  int64             `json:"seed_days_target"` // per-torrent max seeding days; 0 = use global
	Sequential      bool              `json:"sequential"`   // download pieces in order for early playback
	SeedSince       time.Time         `json:"seed_since"`   // when seeding was last (re)started

	// Automation flags (surface in the UI as badges).
	GuardPaused bool `json:"guard_paused"` // paused by the disk guard; resumes when space recovers
	Queued      bool `json:"queued"`       // waiting for a download slot

	// Selective download: file indices the user excluded. nil = want all.
	skippedFiles map[int]bool

	storage *storage.Storage
	engine  *Engine
	picker  *picker
	dlRate  *rateLimiter
}

// Ratio returns upload/download ratio (1.0 = even, >1 = more uploaded than downloaded).
func (t *Torrent) Ratio() float64 {
	if t.Downloaded <= 0 {
		if t.Uploaded > 0 {
			return float64(t.Uploaded)
		}
		return 0
	}
	return float64(t.Uploaded) / float64(t.Downloaded)
}

// Progress returns the fraction [0,1] of the download that is complete.
func (t *Torrent) Progress() float64 {
	if t.TotalWanted <= 0 {
		if t.State == StateSeeding {
			return 1
		}
		return 0
	}
	done := t.Downloaded
	if done > t.TotalWanted {
		done = t.TotalWanted
	}
	return float64(done) / float64(t.TotalWanted)
}

// Percent is a convenience rounding helper.
func (t *Torrent) Percent() float64 { return t.Progress() * 100 }

// MetaInfoComment returns the metainfo comment, if any.
func (t *Torrent) MetaInfoComment() string {
	if t.MetaInfo != nil {
		return t.MetaInfo.Comment
	}
	return ""
}

// MetaInfoCreatedBy returns the creator string from metainfo, if any.
func (t *Torrent) MetaInfoCreatedBy() string {
	if t.MetaInfo != nil {
		return t.MetaInfo.CreatedBy
	}
	return ""
}

// PieceStats returns (present, total) piece counts.
func (t *Torrent) PieceStats() (int, int) {
	if t.storage == nil {
		return 0, 0
	}
	n := t.storage.PieceCount()
	present := 0
	for i := 0; i < n; i++ {
		if t.storage.PiecePresent(i) {
			present++
		}
	}
	return present, n
}

// allPiecesPresent reports whether every piece is verified on disk.
func (t *Torrent) allPiecesPresent() bool {
	if t.storage == nil {
		return t.TotalWanted <= 0
	}
	n := t.storage.PieceCount()
	if n == 0 {
		return t.TotalWanted <= 0
	}
	for i := 0; i < n; i++ {
		if !t.storage.PiecePresent(i) {
			return false
		}
	}
	return true
}

// wantedComplete reports whether every needed (non-skipped) piece is on disk.
// With no selective-download mask this equals allPiecesPresent.
func (t *Torrent) wantedComplete() bool {
	if t.storage == nil {
		return t.TotalWanted <= 0
	}
	skip := t.picker.skipMask()
	n := t.storage.PieceCount()
	for i := 0; i < n; i++ {
		if skip != nil && skip[i] {
			continue
		}
		if !t.storage.PiecePresent(i) {
			return false
		}
	}
	return n > 0
}

func (t *Torrent) setState(s State) {
	t.mu.Lock()
	prev := t.State
	t.State = s
	if s == StateSeeding && prev != StateSeeding {
		t.SeedSince = time.Now()
	}
	t.mu.Unlock()
}

// Notice kinds delivered through Engine.OnNotice.
const (
	NoticeComplete string = "complete" // torrent finished downloading
	NoticeMetadata string = "metadata" // magnet resolved its metadata
	NoticeRatio    string = "ratio"    // torrent reached its ratio target (stopped/removed)
	NoticeRSS      string = "rss"      // an RSS feed item was added as a torrent
	NoticeDiskGuard string = "diskguard" // downloads paused because a disk is nearly full
)

// Engine manages all torrents and peer connections.
type Engine struct {
	mu       sync.Mutex
	torrents map[string]*Torrent
	sessions map[*peerSession]*Torrent
	prevDown map[string]int64
	prevUp   map[string]int64

	peerID        [20]byte
	clientPeerID  string
	port          int
	listener      net.Listener

	connecting map[string]bool
	selfAddrs  map[string]bool

	// settings
	UploadRateLimit   int64
	DownloadRateLimit int64
	MaxConnections    int

	uploadLimiter   *rateLimiter
	downloadLimiter *rateLimiter

	// global activity (dashboard stats), guarded by statsMu
	statsMu       sync.Mutex
	stats         stats
	statsFile     string
	lastStats     time.Time

	// settings persistence
	settingsFile string
	settings     Settings
	uploadPaused bool
	downloadPaused bool
	nightApplied bool
	nightPaused  bool
	trashMu      sync.Mutex // guards trash entry list (file on disk)

	// absolute byte counters for this process
	byteUpRun   int64
	byteDownRun int64

	// OnNotice is invoked for user-facing events (download completion, magnet
	// metadata resolution). It must not block the caller for long.
	OnNotice func(kind, id, name string)

	Logf func(format string, a ...interface{})

	dhtClient *dht.Client
}

// NewEngine creates a torrent engine with a random peer id.
func NewEngine(port int) *Engine {
	pid := make([]byte, 20)
	base := []byte("DS-ALP-0001-000000")
	copy(pid, base)
	ts := time.Now().UnixNano()
	for i := 14; i < 20; i++ {
		pid[i] = byte('0' + (ts % 10))
		ts /= 10
	}
	e := &Engine{
		torrents:        make(map[string]*Torrent),
		sessions:        make(map[*peerSession]*Torrent),
		prevDown:        make(map[string]int64),
		prevUp:          make(map[string]int64),
		connecting:      make(map[string]bool),
		selfAddrs:       make(map[string]bool),
		port:            port,
		MaxConnections:  200,
		clientPeerID:    string(pid),
		lastStats:       time.Now(),
	}
	copy(e.peerID[:], pid)
	e.Logf = func(format string, a ...interface{}) {
		fmt.Printf("digitalis: "+format+"\n", a...)
	}
	return e
}

// PeerID returns the 20-byte client peer id.
func (e *Engine) PeerID() string { return e.clientPeerID }

// Port returns the listening port.
func (e *Engine) Port() int { return e.port }

// SetUploadLimit sets the global upload limit in bytes/sec (0 = unlimited).
func (e *Engine) SetUploadLimit(n int64) {
	e.mu.Lock()
	e.UploadRateLimit = n
	if e.uploadLimiter == nil {
		e.uploadLimiter = newRateLimiter(n)
	} else {
		e.uploadLimiter.setRate(n)
	}
	e.mu.Unlock()
}

// SetDownloadLimit sets the global download limit in bytes/sec (0 = unlimited).
func (e *Engine) SetDownloadLimit(n int64) {
	e.mu.Lock()
	e.DownloadRateLimit = n
	if e.downloadLimiter == nil {
		e.downloadLimiter = newRateLimiter(n)
	} else {
		e.downloadLimiter.setRate(n)
	}
	e.mu.Unlock()
}

// AddTorrent adds a torrent from parsed metainfo and starts it.
func (e *Engine) AddTorrent(mi *metainfo.MetaInfo, saveDir string) (*Torrent, error) {
	t := &Torrent{
		mu:          new(sync.Mutex),
		InfoHash:    mi.InfoHash(),
		Name:        mi.Info.Name,
		MetaInfo:    mi,
		SaveDir:     saveDir,
		State:       StateQueued,
		AddedAt:     time.Now(),
		TotalWanted: mi.TotalLength(),
		Size:        mi.TotalLength(),
		RemovedTrackers: make(map[string]bool),
		AnnounceCount: make(map[string]int64),
		engine:      e,
	}
	if t.Name == "" {
		t.Name = t.InfoHash[:16]
	}
	t.ID = t.InfoHash[:16]
	t.initTrackers()

	e.mu.Lock()
	if _, exists := e.torrents[t.ID]; exists {
		e.mu.Unlock()
		return nil, fmt.Errorf("torrent already exists: %s", t.Name)
	}
	st, err := storage.New(mi, saveDir)
	if err != nil {
		e.mu.Unlock()
		return nil, fmt.Errorf("storage init: %w", err)
	}
	t.storage = st
	t.picker = newPicker(t)
	e.mu.Unlock()

	e.mu.Lock()
	e.torrents[t.ID] = t
	e.mu.Unlock()

	done := t.countDoneBytes()
	if t.wantedComplete() && t.TotalWanted > 0 {
		t.setState(StateSeeding)
	} else {
		t.setState(StateDownloading)
	}
	t.mu.Lock()
	t.Downloaded = done
	t.mu.Unlock()

	e.startAnnounceLoop(t)
	go e.Announce(t, "started")
	e.startPeerLoop(t)
	e.startDHTPeerLoop(t)
	e.Logf("added torrent %q (%s)", t.Name, t.ID)
	return t, nil
}

// AddMagnet adds a torrent by magnet URI. Metadata is fetched lazily from peers
// (accepted but not yet implemented for v1 metadata exchange).
func (e *Engine) AddMagnet(m *metainfo.Magnet, saveDir string) (*Torrent, error) {
	ihHex := m.InfoHash
	if len(ihHex) != 40 {
		return nil, fmt.Errorf("invalid infohash in magnet")
	}
	t := &Torrent{
		mu:          new(sync.Mutex),
		InfoHash:   ihHex,
		Name:       m.DisplayName,
		Magnet:     m,
		SaveDir:    saveDir,
		AddedAt:    time.Now(),
		TotalWanted: m.ExactLength,
		RemovedTrackers: make(map[string]bool),
		AnnounceCount: make(map[string]int64),
		engine:     e,
	}
	if t.Name == "" {
		t.Name = ihHex[:16]
	}
	t.Size = t.TotalWanted
	t.ID = ihHex[:16]
	t.initTrackers()

	e.mu.Lock()
	if _, exists := e.torrents[t.ID]; exists {
		e.mu.Unlock()
		return nil, fmt.Errorf("torrent already exists")
	}
	t.picker = newPicker(t)
	e.torrents[t.ID] = t
	e.mu.Unlock()

	t.setState(StateDownloading)
	e.startAnnounceLoop(t)
	go e.Announce(t, "started")
	e.startPeerLoop(t)
	e.startDHTPeerLoop(t)
	e.Logf("added magnet %q (%s)", t.Name, t.ID)
	return t, nil
}

// Announce performs a single announce to the tracker and connects to peers.
func (e *Engine) Announce(t *Torrent, event string) error {
	t.mu.Lock()
	uploaded := t.Uploaded
	downloaded := t.Downloaded
	left := t.TotalWanted - downloaded
	if left < 0 {
		left = 0
	}
	t.mu.Unlock()

	var urls []string
	if t.MetaInfo != nil {
		if t.MetaInfo.Announce != "" {
			urls = append(urls, t.MetaInfo.Announce)
		}
		for _, tier := range t.MetaInfo.AnnounceList {
			urls = append(urls, tier...)
		}
	}
	if t.Magnet != nil {
		urls = append(urls, t.Magnet.Trackers...)
	}
	urls = append(urls, t.CustomTrackers...)
	active := urls[:0]
	for _, u := range urls {
		if t.RemovedTrackers[u] {
			continue
		}
		active = append(active, u)
	}
	urls = active
	if len(urls) == 0 {
		return fmt.Errorf("no trackers configured")
	}

	var lastErr error
	t.mu.Lock()
	trackers := make([]*TrackerStat, 0, len(urls))
	for _, u := range urls {
		ts := t.trackerByURL(u)
		if ts == nil {
			ts = &TrackerStat{URL: u}
			t.Trackers = append(t.Trackers, ts)
		}
		trackers = append(trackers, ts)
	}
	t.mu.Unlock()

	for i, u := range urls {
		resp, err := tracker.Announce(u, t.InfoHash, e.PeerID(), e.port, uploaded, downloaded, left, event, 50)
		connected := e.sessionCount(t.ID)
		t.mu.Lock()
		ts := trackers[i]
		if err != nil {
			t.noteTrackerFailure(ts, err)
			lastErr = err
		} else if resp.Failure != "" {
			t.noteTrackerFailure(ts, fmt.Errorf("%s", resp.Failure))
			lastErr = fmt.Errorf("%s", resp.Failure)
		} else {
			t.noteTrackerSuccess(ts, resp.Seeders, resp.Leechers, len(resp.Peers))
			t.Seeders = resp.Seeders
			t.Leechers = resp.Leechers
			t.PeersKnown += len(resp.Peers)
			t.PeersConnected = connected
			t.countTrackersActive()
			t.mu.Unlock()
			e.connectPeers(t, resp.Peers)
			t.mu.Lock()
		}
		t.countTrackersActive()
		t.LastSeen = time.Now()
		t.mu.Unlock()
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("no tracker responded")
}

// sessionCount returns number of open sessions for a torrent id.
func (e *Engine) sessionCount(id string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, tr := range e.sessions {
		if tr.ID == id {
			n++
		}
	}
	return n
}

// connectPeers opens outgoing connections to announced peers.
func (e *Engine) connectPeers(t *Torrent, peers []tracker.Peer) {
	e.mu.Lock()
	room := e.MaxConnections - len(e.sessions) - len(e.connecting)
	if room <= 0 {
		e.mu.Unlock()
		return
	}
	for _, p := range peers {
		if room <= 0 {
			break
		}
		if p.Port == 0 || p.IP == "" {
			continue
		}
		key := t.ID + "@" + p.IP + ":" + fmt.Sprint(p.Port)
		if _, dup := e.connecting[key]; dup {
			continue
		}
		e.connecting[key] = true
		room--
		e.mu.Unlock()
		ip := p.IP
		port := p.Port
		go func() {
			e.connectToPeer(t, ip, port)
			e.mu.Lock()
			delete(e.connecting, key)
			e.mu.Unlock()
		}()
		e.mu.Lock()
	}
	e.mu.Unlock()
}

// startAnnounceLoop periodically announces a torrent to its trackers.
func (e *Engine) startAnnounceLoop(t *Torrent) {
	go func() {
		interval := 45 * time.Second
		for {
			time.Sleep(interval)
			e.mu.Lock()
			if e.torrents[t.ID] != t {
				e.mu.Unlock()
				return
			}
			t.mu.Lock()
			st := t.State
			t.mu.Unlock()
			e.mu.Unlock()
			if st == StatePaused || st == StateStopped {
				continue
			}
			_ = e.Announce(t, "")
		}
	}()
}

// startPeerLoop periodically refreshes peer lists if connected peers are low.
func (e *Engine) startPeerLoop(t *Torrent) {
	go func() {
		for {
			time.Sleep(25 * time.Second)
			e.mu.Lock()
			if e.torrents[t.ID] != t {
				e.mu.Unlock()
				return
			}
			t.mu.Lock()
			st := t.State
			connected := t.PeersConnected
			t.mu.Unlock()
			e.mu.Unlock()
			if st == StatePaused || st == StateStopped {
				continue
			}
			if connected < 3 {
				_ = e.Announce(t, "")
			}
		}
	}()
}

// Pause pauses a torrent.
func (e *Engine) Pause(id string) error {
	t, ok := e.GetTorrent(id)
	if !ok {
		return fmt.Errorf("torrent not found")
	}
	t.setState(StatePaused)
	return nil
}

// Resume resumes a paused torrent.
func (e *Engine) Resume(id string) error {
	t, ok := e.GetTorrent(id)
	if !ok {
		return fmt.Errorf("torrent not found")
	}
	t.mu.Lock()
	if t.wantedComplete() {
		t.State = StateSeeding
	} else {
		t.State = StateDownloading
	}
	t.mu.Unlock()
	go e.Announce(t, "")
	return nil
}

// MoveTorrent relocates a torrent's files to a new directory and updates its
// category. The new directory must be on the same filesystem.
func (e *Engine) MoveTorrent(id, newSaveDir, category string) error {
	e.mu.Lock()
	t, ok := e.torrents[id]
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("torrent not found")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.storage != nil {
		if err := t.storage.Move(newSaveDir); err != nil {
			return fmt.Errorf("move failed: %w", err)
		}
	} else if newSaveDir != "" {
		if err := os.MkdirAll(newSaveDir, 0755); err != nil {
			return err
		}
	}
	t.SaveDir = newSaveDir
	t.Category = category
	e.Logf("moved torrent %s to %s", id, newSaveDir)
	return nil
}

// RemoveTorrent stops and removes a torrent.
func (e *Engine) RemoveTorrent(id string) error {
	e.mu.Lock()
	t, ok := e.torrents[id]
	if !ok {
		e.mu.Unlock()
		return fmt.Errorf("torrent not found")
	}
	delete(e.torrents, id)
	// close its sessions
	for s, tr := range e.sessions {
		if tr.ID == id {
			select {
			case s.stop <- struct{}{}:
			default:
			}
		}
	}
	e.mu.Unlock()
	t.setState(StateStopped)
	t.mu.Lock()
	if t.storage != nil {
		t.storage.Close()
	}
	t.mu.Unlock()
	e.Logf("removed torrent %s", id)
	return nil
}

// RemoveTorrentWithFiles stops and removes a torrent, then deletes its
// downloaded data from disk. The on-disk footprint (a torrent-named folder for
// multi-file torrents, a single file for single-file torrents) is removed only
// if it lies safely under a registered storage root.
func (e *Engine) RemoveTorrentWithFiles(id string) error {
	e.mu.Lock()
	t, ok := e.torrents[id]
	if !ok {
		e.mu.Unlock()
		return fmt.Errorf("torrent not found")
	}
	t.mu.Lock()
	saveDir, name := t.SaveDir, t.Name
	t.mu.Unlock()
	e.mu.Unlock()

	if err := e.RemoveTorrent(id); err != nil {
		return err
	}
	if saveDir == "" || name == "" {
		return nil
	}
	root := filepath.Join(saveDir, name)
	if !e.underAnyRoot(root) {
		e.Logf("remove files %s: refusing %s (outside storage roots)", id, root)
		return nil
	}
	if err := e.trashOrRemove(root, name); err != nil {
		e.Logf("remove files %s: %v", id, err)
		return err
	}
	e.Logf("removed torrent %s with files", id)
	return nil
}

// underAnyRoot reports whether path lives inside the base save dir or any
// registered storage root (but is not the root itself). Used before deleting
// on-disk files so a tampered save dir can never escape the storage roots.
func (e *Engine) underAnyRoot(path string) bool {
	for _, d := range e.Disks() {
		if d == "" {
			continue
		}
		if diskContains(d, path) {
			return true
		}
	}
	return false
}

// SetCategory updates the torrent's category label without moving files.
func (t *Torrent) SetCategory(category string) {
	t.mu.Lock()
	t.Category = category
	t.mu.Unlock()
}

// SetPriorityPieces sets an inclusive piece window that the piece picker will
// prioritize for streaming. After the window is fully downloaded it clears.
func (e *Engine) SetPriorityPieces(id string, lo, hi int) error {
	e.mu.Lock()
	t, ok := e.torrents[id]
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("torrent not found")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.picker == nil {
		return fmt.Errorf("picker unavailable")
	}
	t.picker.SetPriority(lo, hi)
	return nil
}

// PriorityPieces returns the current streaming priority window as (lo, hi),
// or (-1, -1) when no window is active.
func (e *Engine) PriorityPieces(id string) (int, int) {
	e.mu.Lock()
	t, ok := e.torrents[id]
	e.mu.Unlock()
	if !ok {
		return -1, -1
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.picker == nil {
		return -1, -1
	}
	return t.picker.PriorityWindow()
}

// StorageAvailable reports whether on-disk storage exists (metadata resolved).
func (t *Torrent) StorageAvailable() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.storage != nil
}

// ReadPieceRange reads length bytes starting at begin inside piece, straight
// from disk. Missing pieces yield zero bytes so media playback can start early.
func (t *Torrent) ReadPieceRange(piece int, begin int64, length int) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.storage == nil {
		return nil, fmt.Errorf("storage unavailable")
	}
	return t.storage.ReadBlock(piece, begin, length)
}

// StoragePieceCount returns the number of pieces once metadata is resolved.
func (t *Torrent) StoragePieceCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.storage == nil {
		return 0
	}
	return t.storage.PieceCount()
}

// GetTorrent returns a torrent by id.
func (e *Engine) GetTorrent(id string) (*Torrent, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, ok := e.torrents[id]
	return t, ok
}

// Torrents returns a snapshot of all torrents with live speed estimates.
func (e *Engine) Torrents() []*Torrent {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.statsMu.Lock()
	defer e.statsMu.Unlock()
	now := time.Now()
	elapsed := now.Sub(e.lastStats).Seconds()
	if elapsed < 0.1 {
		elapsed = 0.1
	}

	per := make(map[string][2]int64)
	for s := range e.sessions {
		d := atomic.LoadInt64(&s.bytesDown)
		u := atomic.LoadInt64(&s.bytesUp)
		p := per[s.t.ID]
		p[0] += d
		p[1] += u
		per[s.t.ID] = p
	}

	out := make([]*Torrent, 0, len(e.torrents))
	for id, t := range e.torrents {
		tot := per[id]
		rateD := int64(float64(tot[0]-e.prevDown[id]) / elapsed)
		rateU := int64(float64(tot[1]-e.prevUp[id]) / elapsed)
		if rateD < 0 {
			rateD = 0
		}
		if rateU < 0 {
			rateU = 0
		}
		e.prevDown[id] = tot[0]
		e.prevUp[id] = tot[1]

		t.mu.Lock()
		c := *t
		c.DownloadSpeed = rateD
		c.UploadSpeed = rateU
		ph, pt := t.PieceStats()
		c.PiecesHave = ph
		c.PiecesTotal = pt
		c.engine = nil
		c.storage = nil
		c.picker = nil
		t.mu.Unlock()
		out = append(out, &c)
	}
	e.lastStats = now
	return out
}

func (t *Torrent) countDoneBytes() int64 {
	if t.storage == nil {
		return 0
	}
	var done int64
	for i := 0; i < t.storage.PieceCount(); i++ {
		if t.storage.PiecePresent(i) {
			done += t.storage.PieceLength(i)
		}
	}
	return done
}