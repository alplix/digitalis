// Package torrente contains the core torrent engine: peer management,
// piece picking, downloading and seeding.
package torrente

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

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
	mu sync.Mutex

	ID string // deterministic short id (first 16 hex chars of info hash)
	InfoHash, Name string
	MetaInfo *metainfo.MetaInfo
	Magnet   *metainfo.Magnet

	State  State
	SaveDir string
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

	storage *storage.Storage
	engine  *Engine
	picker  *picker
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

func (t *Torrent) setState(s State) {
	t.mu.Lock()
	t.State = s
	t.mu.Unlock()
}

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

	// settings
	UploadRateLimit   int64
	DownloadRateLimit int64
	MaxConnections    int

	uploadLimiter   *rateLimiter
	downloadLimiter *rateLimiter

	statsMu  sync.Mutex
	lastStats time.Time

	Logf func(format string, a ...interface{})
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
		InfoHash:    mi.InfoHash(),
		Name:        mi.Info.Name,
		MetaInfo:    mi,
		SaveDir:     saveDir,
		State:       StateQueued,
		AddedAt:     time.Now(),
		TotalWanted: mi.TotalLength(),
		Size:        mi.TotalLength(),
		engine:      e,
	}
	if t.Name == "" {
		t.Name = t.InfoHash[:16]
	}
	t.ID = t.InfoHash[:16]

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
	if done >= t.TotalWanted && t.TotalWanted > 0 {
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
		InfoHash:   ihHex,
		Name:       m.DisplayName,
		Magnet:     m,
		SaveDir:    saveDir,
		AddedAt:    time.Now(),
		TotalWanted: m.ExactLength,
		engine:     e,
	}
	if t.Name == "" {
		t.Name = ihHex[:16]
	}
	t.Size = t.TotalWanted
	t.ID = ihHex[:16]

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
	if len(urls) == 0 {
		return fmt.Errorf("no trackers configured")
	}

	for _, u := range urls {
		resp, err := tracker.Announce(u, t.InfoHash, e.PeerID(), e.port, uploaded, downloaded, left, event, 50)
		if err != nil {
			continue
		}
		if resp.Failure != "" {
			e.Logf("tracker %s: %s", u, resp.Failure)
			continue
		}
		t.mu.Lock()
		t.Seeders = resp.Seeders
		t.Leechers = resp.Leechers
		t.PeersKnown += len(resp.Peers)
		t.PeersConnected = e.sessionCount(t.ID)
		t.mu.Unlock()
		e.connectPeers(t, resp.Peers)
		return nil
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
	for _, p := range peers {
		if p.Port == 0 || p.IP == "" {
			continue
		}
		key := t.ID + "@" + p.IP + ":" + fmt.Sprint(p.Port)
		if _, dup := e.connecting[key]; dup {
			continue
		}
		e.connecting[key] = true
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
	if t.allPiecesPresent() {
		t.State = StateSeeding
	} else {
		t.State = StateDownloading
	}
	t.mu.Unlock()
	go e.Announce(t, "")
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