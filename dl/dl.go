// Package dl implements a resource-friendly download system. Each task
// streams a remote file through a small fixed buffer into one of three
// selectable sinks: discard (count only, no RAM no disk), a bounded RAM ring
// buffer (keeps the most recent N bytes), or a file on a storage root.
// Tasks may loop a fixed number of times or forever, run several parallel
// streams, retry on errors, verify a SHA-256 digest, wait for a start time,
// and be rate-limited so they never saturate the machine.
package dl

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Mode selects where downloaded bytes end up.
type Mode string

const (
	ModeDiscard Mode = "discard" // count bytes, throw them away (no RAM, no disk)
	ModeRAM     Mode = "ram"     // keep the most recent N bytes in a ring buffer
	ModeDisk    Mode = "disk"    // write to a file under a storage root
)

// Options configures one download task.
type Options struct {
	URL        string `json:"url"`
	Mode       Mode   `json:"mode"`
	Direction  string `json:"direction"`          // "down" (default) | "up"
	UploadMB   int    `json:"upload_mb"`          // up: generated payload size
	RamMB      int    `json:"ram_mb"`             // RAM mode: ring buffer size (1-1024 MB)
	SaveDir    string `json:"save_dir,omitempty"` // disk mode: absolute directory
	Filename   string `json:"filename,omitempty"` // disk mode: override file name
	Loops      int    `json:"loops"`              // 0 = infinite
	Interval   int    `json:"interval"`           // seconds between loops
	SpeedCap   int64  `json:"speed_cap"`          // bytes/sec, 0 = unlimited
	DeleteEach bool   `json:"delete_each"`        // disk mode: remove the file after each loop
	Conn       int    `json:"conn"`               // parallel streams (down, discard/ram), 1-16
	Retries    int    `json:"retries"`            // extra attempts per loop on error
	RetryWait  int    `json:"retry_wait"`         // seconds between attempts
	VerifySHA  string `json:"verify_sha256"`      // expected hex digest; verified in every mode
	Proxy      string `json:"proxy"`              // http(s):// or socks5:// proxy URL
	StartAt    string `json:"start_at"`           // "HH:MM": wait until the next occurrence
}

// Task is one download job with live counters.
type Task struct {
	ID        string `json:"id"`
	URL       string `json:"url"`
	Mode      Mode   `json:"mode"`
	RamMB     int    `json:"ram_mb,omitempty"`
	SaveDir   string `json:"save_dir,omitempty"`
	Loops     int    `json:"loops"` // requested (0 = infinite)
	Direction string `json:"direction,omitempty"`
	Conn      int    `json:"conn,omitempty"`
	Retries   int    `json:"retries,omitempty"`
	Proxy     string `json:"proxy,omitempty"`
	StartAt   string `json:"start_at,omitempty"`
	Verified  string `json:"verified,omitempty"` // "ok" | "mismatch" when a digest was checked

	State     string    `json:"state"` // running | done | error | stopped | scheduled
	Bytes     int64     `json:"bytes"`
	Files     int64     `json:"files"`
	LoopsDone int64     `json:"loops_done"`
	Speed     int64     `json:"speed"`
	UptimeSec int64     `json:"uptime_sec"`
	Error     string    `json:"error,omitempty"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`

	Current int64 `json:"-"` // bytes in the current loop

	opts    Options
	stop    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	ring    *ring
	outPath string
	started time.Time
	manager *Manager
	stopped atomic.Bool

	winMu      sync.Mutex
	winBytes   int64
	winStarted time.Time
}

// Totals aggregates every task for the dashboard strip.
type Totals struct {
	Bytes     int64     `json:"bytes"`
	Speed     int64     `json:"speed"`
	Loops     int64     `json:"loops"`
	Files     int64     `json:"files"`
	StartedAt time.Time `json:"started_at"`
}

// Manager owns all download tasks and the run history.
type Manager struct {
	mu       sync.Mutex
	tasks    map[string]*Task
	order    []string
	started  time.Time
	histMu   sync.Mutex
	history  []HistoryEntry
	histPath string
}

// NewManager creates an empty download manager. historyPath ("" = memory
// only) persists finished runs across restarts.
func NewManager(historyPath string) *Manager {
	m := &Manager{tasks: make(map[string]*Task), started: time.Now(), histPath: historyPath}
	m.loadHistory()
	return m
}

// Add validates the options and starts a new task.
func (m *Manager) Add(opts Options) (*Task, error) {
	opts.URL = strings.TrimSpace(opts.URL)
	if opts.URL == "" {
		return nil, fmt.Errorf("empty URL")
	}
	u, err := url.Parse(opts.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %v", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if opts.Direction == "" {
		opts.Direction = "down"
	}
	if opts.Direction != "down" && opts.Direction != "up" {
		return nil, fmt.Errorf("unknown direction %q", opts.Direction)
	}
	if scheme != "http" && scheme != "https" && scheme != "ftp" {
		return nil, fmt.Errorf("unsupported scheme %q (use http, https or ftp)", u.Scheme)
	}
	if opts.Direction == "up" && opts.Mode == ModeRAM {
		return nil, fmt.Errorf("upload does not support RAM mode")
	}
	if opts.Direction == "up" && opts.Mode == ModeDisk {
		return nil, fmt.Errorf("upload sends generated data; use discard mode")
	}
	if opts.Direction == "up" {
		if opts.UploadMB < 1 {
			opts.UploadMB = 10
		}
		if opts.UploadMB > 4096 {
			opts.UploadMB = 4096
		}
	}
	switch opts.Mode {
	case ModeDiscard:
	case ModeRAM:
		if opts.RamMB < 1 {
			opts.RamMB = 16
		}
		if opts.RamMB > 1024 {
			opts.RamMB = 1024
		}
	case ModeDisk:
		if strings.TrimSpace(opts.SaveDir) == "" {
			return nil, fmt.Errorf("disk mode needs a save directory")
		}
		if err := os.MkdirAll(opts.SaveDir, 0755); err != nil {
			return nil, fmt.Errorf("save dir: %v", err)
		}
	default:
		return nil, fmt.Errorf("unknown mode %q", opts.Mode)
	}
	if opts.Loops < 0 {
		opts.Loops = 0
	}
	if opts.Interval < 0 {
		opts.Interval = 0
	}
	if opts.Conn < 0 {
		opts.Conn = 0
	}
	if opts.Conn > 16 {
		opts.Conn = 16
	}
	if opts.Proxy != "" {
		if _, err := url.Parse(opts.Proxy); err != nil {
			return nil, fmt.Errorf("invalid proxy URL: %v", err)
		}
	}
	if opts.StartAt != "" && !validHHMM(opts.StartAt) {
		return nil, fmt.Errorf("start_at must be HH:MM")
	}

	buf := make([]byte, 4)
	rand.Read(buf)
	t := &Task{
		ID:        hex.EncodeToString(buf),
		URL:       opts.URL,
		Mode:      opts.Mode,
		RamMB:     opts.RamMB,
		SaveDir:   opts.SaveDir,
		Loops:     opts.Loops,
		Direction: opts.Direction,
		Conn:      opts.Conn,
		Retries:   opts.Retries,
		Proxy:     opts.Proxy,
		StartAt:   opts.StartAt,
		State:     "running",
		opts:      opts,
		stop:      make(chan struct{}),
		started:   time.Now(),
		StartedAt: time.Now(),
		manager:   m,
	}
	if t.opts.StartAt != "" {
		t.State = "scheduled"
	}
	if opts.Mode == ModeRAM && opts.Direction == "down" {
		t.ring = newRing(opts.RamMB * 1024 * 1024)
	}
	m.mu.Lock()
	m.tasks[t.ID] = t
	m.order = append(m.order, t.ID)
	m.mu.Unlock()
	go m.run(t)
	return t, nil
}

// Stop cancels a running task (state becomes "stopped"); the task stays listed.
func (m *Manager) Stop(id string) error {
	m.mu.Lock()
	t, ok := m.tasks[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("task not found")
	}
	t.stopped.Store(true)
	t.once.Do(func() { close(t.stop) })
	return nil
}

// Remove stops a task and drops it from the list.
func (m *Manager) Remove(id string) error {
	_ = m.Stop(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tasks[id]; !ok {
		return fmt.Errorf("task not found")
	}
	delete(m.tasks, id)
	for i, x := range m.order {
		if x == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	return nil
}

// List returns all tasks (oldest first) plus aggregated totals.
func (m *Manager) List() ([]*Task, Totals) {
	m.mu.Lock()
	ids := append([]string(nil), m.order...)
	m.mu.Unlock()

	out := make([]*Task, 0, len(ids))
	var tot Totals
	tot.StartedAt = m.started
	for _, id := range ids {
		m.mu.Lock()
		t := m.tasks[id]
		m.mu.Unlock()
		if t == nil {
			continue
		}
		out = append(out, t)
		tot.Bytes += atomic.LoadInt64(&t.Bytes)
		tot.Loops += atomic.LoadInt64(&t.LoopsDone)
		tot.Files += atomic.LoadInt64(&t.Files)
		tot.Speed += t.currentSpeed()
	}
	return out, tot
}

// RAMPreview returns the newest bytes of a RAM-mode task's ring buffer.
func (m *Manager) RAMPreview(id string) ([]byte, bool) {
	m.mu.Lock()
	t, ok := m.tasks[id]
	m.mu.Unlock()
	if !ok || t.ring == nil {
		return nil, false
	}
	return t.ring.Last(), true
}

// ---------- execution ----------

func (m *Manager) run(t *Task) {
	if err := t.waitScheduled(); err != nil {
		t.finish("stopped", "")
		return
	}
	conns := t.opts.Conn
	if conns < 1 {
		conns = 1
	}
	if conns > 16 {
		conns = 16
	}
	if conns == 1 {
		m.loopShard(t)
	} else {
		var wg sync.WaitGroup
		for i := 0; i < conns; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); m.loopShard(t) }()
		}
		wg.Wait()
	}

	t.mu.Lock()
	switch {
	case t.stopped.Load():
		t.State = "stopped"
	case t.Error != "":
		t.State = "error"
	default:
		t.State = "done"
	}
	t.EndedAt = time.Now()
	t.mu.Unlock()
	m.recordHistory(t)
}

// loopShard runs the full loop set; parallel tasks run several shards.
func (m *Manager) loopShard(t *Task) {
	for loop := int64(1); t.opts.Loops == 0 || loop <= int64(t.opts.Loops); loop++ {
		if t.stopped.Load() {
			return
		}
		atomic.StoreInt64(&t.Current, 0)
		t.winMu.Lock()
		t.winBytes = 0
		t.winStarted = time.Now()
		t.winMu.Unlock()

		var lastErr error
		tries := t.opts.Retries + 1
		for attempt := 1; attempt <= tries; attempt++ {
			var err error
			if t.opts.Direction == "up" {
				err = t.pushOnce()
			} else {
				err = m.fetchOnce(t)
			}
			if err == nil {
				lastErr = nil
				break
			}
			lastErr = err
			if attempt < tries && t.opts.RetryWait > 0 {
				for s := 0; s < t.opts.RetryWait; s++ {
					if t.stopped.Load() {
						return
					}
					time.Sleep(time.Second)
				}
			}
		}
		atomic.AddInt64(&t.LoopsDone, 1)
		if lastErr != nil {
			t.mu.Lock()
			if t.Error == "" {
				t.Error = lastErr.Error()
			}
			t.mu.Unlock()
			return // shard dies; run() marks the final state
		}
		if t.opts.DeleteEach && t.opts.Mode == ModeDisk && t.outPath != "" {
			os.Remove(t.outPath)
		}
		if t.opts.Interval > 0 && (t.opts.Loops == 0 || loop < int64(t.opts.Loops)) {
			for s := 0; s < t.opts.Interval; s++ {
				if t.stopped.Load() {
					return
				}
				time.Sleep(time.Second)
			}
		}
	}
}

// waitScheduled sleeps until the HH:MM start time, reacting to Stop.
func (t *Task) waitScheduled() error {
	if t.opts.StartAt == "" {
		return nil
	}
	for {
		now := time.Now()
		target := nextHHMM(t.opts.StartAt, now)
		for now.Before(target) {
			if t.stopped.Load() {
				return fmt.Errorf("stopped")
			}
			time.Sleep(time.Second)
			now = time.Now()
		}
		return nil
	}
}

// nextHHMM returns the next occurrence of "HH:MM" after now.
func nextHHMM(v string, now time.Time) time.Time {
	h, m := parseHHMM(v)
	t := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if !t.After(now) {
		t = t.Add(24 * time.Hour)
	}
	return t
}

func parseHHMM(v string) (int, int) {
	parts := strings.Split(v, ":")
	if len(parts) != 2 {
		return 0, 0
	}
	h, _ := strconv.Atoi(parts[0])
	m, _ := strconv.Atoi(parts[1])
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0
	}
	return h, m
}

func validHHMM(v string) bool {
	if v == "00:00" {
		return true
	}
	h, m := parseHHMM(v)
	return h != 0 || m != 0
}

func (t *Task) finish(state, errMsg string) {
	t.mu.Lock()
	if t.State == "running" || t.State == "scheduled" {
		t.State = state
		t.Error = errMsg
		t.EndedAt = time.Now()
	}
	t.mu.Unlock()
}

// fetchOnce downloads the URL once into the mode's sink.
func (m *Manager) fetchOnce(t *Task) error {
	u, err := url.Parse(t.opts.URL)
	if err != nil {
		return err
	}
	th := &throttle{rate: t.opts.SpeedCap}
	var w io.Writer
	switch t.opts.Mode {
	case ModeDiscard:
		w = io.Discard
	case ModeRAM:
		w = t.ring // bounded; keeps the most recent bytes only
	case ModeDisk:
		name := strings.TrimSpace(t.opts.Filename)
		if name == "" {
			name = baseName(u)
		}
		t.outPath = filepath.Join(t.opts.SaveDir, name)
		f, ferr := os.Create(t.outPath)
		if ferr != nil {
			return ferr
		}
		defer f.Close()
		w = f
	default:
		return fmt.Errorf("unknown mode")
	}
	var hasher hash.Hash
	if strings.TrimSpace(t.opts.VerifySHA) != "" {
		hasher = sha256.New()
		w = io.MultiWriter(w, hasher)
	}
	cw := &countingWriter{w: w, t: t, th: th}

	var xferErr error
	switch strings.ToLower(u.Scheme) {
	case "ftp":
		xferErr = ftpGet(u, cw)
	default:
		xferErr = httpGet(t.opts.URL, t.opts.Proxy, cw)
	}
	if xferErr != nil {
		return xferErr
	}
	if hasher != nil {
		got := hex.EncodeToString(hasher.Sum(nil))
		want := strings.ToLower(strings.TrimSpace(t.opts.VerifySHA))
		if got != want {
			t.mu.Lock()
			t.Verified = "mismatch"
			t.mu.Unlock()
			return fmt.Errorf("sha256 mismatch: got %s", got)
		}
		t.mu.Lock()
		t.Verified = "ok"
		t.mu.Unlock()
	}
	return nil
}

func httpGet(rawURL, proxy string, w io.Writer) error {
	client := newClient(proxy)
	resp, err := client.Get(rawURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	buf := make([]byte, 64*1024) // small fixed buffer: tiny RAM footprint
	_, err = io.CopyBuffer(w, resp.Body, buf)
	return err
}

// newClient builds an HTTP client with an optional proxy and long timeouts
// (infinite loops are legal, so no global deadline).
func newClient(proxy string) *http.Client {
	tr := &http.Transport{
		MaxIdleConns:        8,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	if proxy != "" {
		if pu, err := url.Parse(proxy); err == nil {
			tr.Proxy = http.ProxyURL(pu)
		}
	}
	return &http.Client{Transport: tr}
}

type countingWriter struct {
	w  io.Writer
	t  *Task
	th *throttle
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.th.wait(len(p))
	n, err := c.w.Write(p)
	if n > 0 {
		atomic.AddInt64(&c.t.Bytes, int64(n))
		atomic.AddInt64(&c.t.Current, int64(n))
		c.t.winMu.Lock()
		c.t.winBytes += int64(n)
		c.t.winMu.Unlock()
	}
	return n, err
}

// currentSpeed returns bytes/sec over the current (or last) measurement window.
func (t *Task) currentSpeed() int64 {
	t.winMu.Lock()
	defer t.winMu.Unlock()
	if t.winStarted.IsZero() {
		return 0
	}
	elapsed := time.Since(t.winStarted).Seconds()
	if elapsed < 0.5 {
		elapsed = 0.5
	}
	return int64(float64(t.winBytes) / elapsed)
}

// ---------- helpers ----------

// ring is a fixed-size byte ring buffer. Writes overwrite the oldest bytes;
// after the initial allocation it never allocates again, so RAM use stays
// exactly RamMB no matter how much data streams through.
type ring struct {
	buf []byte
	w   int
	n   int64
}

func newRing(capBytes int) *ring {
	if capBytes < 1 {
		capBytes = 1
	}
	return &ring{buf: make([]byte, capBytes)}
}

func (r *ring) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		c := copy(r.buf[r.w:], p)
		r.w = (r.w + c) % len(r.buf)
		p = p[c:]
	}
	r.n += int64(total)
	return total, nil
}

// Last returns a copy of the most recent min(n, cap) bytes.
func (r *ring) Last() []byte {
	n := int64(len(r.buf))
	if r.n < n {
		n = r.n
	}
	out := make([]byte, n)
	start := (r.w - int(n)) % len(r.buf)
	if start < 0 {
		start += len(r.buf)
	}
	c1 := copy(out, r.buf[start:])
	copy(out[c1:], r.buf[:])
	return out
}

// throttle is a simple byte bucket limiting throughput to rate bytes/sec
// (0 or negative = unlimited). Single-goroutine use.
type throttle struct {
	rate int64
	acc  int64
	last time.Time
}

func (th *throttle) wait(n int) {
	if th.rate <= 0 {
		return
	}
	now := time.Now()
	if th.last.IsZero() {
		th.last = now
		return
	}
	th.acc += int64(n)
	allowed := int64(now.Sub(th.last).Seconds() * float64(th.rate))
	if th.acc <= allowed {
		return
	}
	excess := th.acc - allowed
	time.Sleep(time.Duration(float64(excess)/float64(th.rate)) * time.Second)
	th.last = time.Now()
	th.acc = 0
}

func baseName(u *url.URL) string {
	name := filepath.Base(u.Path)
	name = strings.TrimPrefix(name, "/")
	if name == "" || name == "." || name == "/" {
		name = u.Hostname() + "_" + strconv.FormatInt(time.Now().Unix(), 10)
	}
	return name
}

// ---------- minimal FTP client (passive mode, binary transfers) ----------

func ftpGet(u *url.URL, w io.Writer) error {
	c, err := ftpDial(u)
	if err != nil {
		return err
	}
	defer c.close()

	pasv, err := c.pasv()
	if err != nil {
		return err
	}
	if u.Path == "" || u.Path == "/" {
		return fmt.Errorf("ftp: no path")
	}
	data, err := dialTimeout(pasv)
	if err != nil {
		return fmt.Errorf("ftp data connect: %v", err)
	}
	defer data.Close()
	if _, err := c.cmd(0, "RETR "+u.Path); err != nil { // 125 or 150
		return err
	}
	buf := make([]byte, 64*1024)
	if _, err := io.CopyBuffer(w, data, buf); err != nil {
		return err
	}
	_, _, _ = c.respText() // 226 transfer complete (best effort)
	_, _ = c.cmd(0, "QUIT")
	return nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
