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
	"encoding/hex"
	"fmt"
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
	Crawl      bool   `json:"crawl"`              // recursive mirror: walk all dirs, pull every file
	Depth      int    `json:"depth"`              // crawl depth limit (0 = unlimited)
	Include    string `json:"include"`            // filename regex filter while crawling
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
	Crawl     bool   `json:"crawl,omitempty"`
	Depth     int    `json:"depth,omitempty"`
	Include   string `json:"include,omitempty"`
	Verified  string `json:"verified,omitempty"` // "ok" | "mismatch" when a digest was checked

	State     string    `json:"state"` // running | done | error | stopped | scheduled
	Bytes     int64     `json:"bytes"`
	Files     int64     `json:"files"`
	Found     int64     `json:"found"`   // crawl: files discovered
	Crawled   int64     `json:"crawled"` // crawl: directories indexed
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

	crawlBase string

	logMu sync.Mutex
	logs  []LogLine

	winMu      sync.Mutex
	winBytes   int64
	winStarted time.Time
}

// LogLine is one terminal-style event line of a task.
type LogLine struct {
	Ts  string `json:"ts"`
	Lvl string `json:"lvl"` // info | ok | warn | err
	Msg string `json:"msg"`
}

const logMax = 300

// log appends a timestamped event to the task's terminal ring buffer.
func (t *Task) log(lvl, format string, args ...interface{}) {
	t.logMu.Lock()
	t.logs = append(t.logs, LogLine{
		Ts:  time.Now().Format("15:04:05"),
		Lvl: lvl,
		Msg: fmt.Sprintf(format, args...),
	})
	if len(t.logs) > logMax {
		t.logs = t.logs[len(t.logs)-logMax:]
	}
	t.logMu.Unlock()
}

// LogLines returns a copy of the task's terminal lines.
func (t *Task) LogLines() []LogLine {
	t.logMu.Lock()
	defer t.logMu.Unlock()
	out := make([]LogLine, len(t.logs))
	copy(out, t.logs)
	return out
}

// Totals aggregates every task for the dashboard strip.
type Totals struct {
	Bytes     int64     `json:"bytes"`
	Speed     int64     `json:"speed"`
	Loops     int64     `json:"loops"`
	Files     int64     `json:"files"`
	StartedAt time.Time `json:"started_at"`
	BaseBytes int64     `json:"base_bytes"` // all-time totals from finished tasks (persisted)
	BaseLoops int64     `json:"base_loops"`
	BaseFiles int64     `json:"base_files"`
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
	base     struct {
		Bytes int64 `json:"bytes"`
		Loops int64 `json:"loops"`
		Files int64 `json:"files"`
	}
}

// NewManager creates an empty download manager. historyPath ("" = memory
// only) persists finished runs and all-time totals across restarts.
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
	if opts.Depth < 0 {
		opts.Depth = 0
	}
	if opts.Crawl && opts.Direction == "up" {
		return nil, fmt.Errorf("crawl works with downloads only")
	}
	// Directory URLs (trailing slash) mean "mirror this whole tree":
	// automatically switch them into recursive crawl mode.
	if opts.Direction == "down" && !opts.Crawl && (scheme == "http" || scheme == "https") &&
		(strings.HasSuffix(opts.URL, "/") || strings.HasSuffix(opts.URL, "\\")) {
		opts.Crawl = true
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
		Crawl:     opts.Crawl,
		Depth:     opts.Depth,
		Include:   opts.Include,
		State:     "running",
		opts:      opts,
		stop:      make(chan struct{}),
		started:   time.Now(),
		StartedAt: time.Now(),
		manager:   m,
	}
	if opts.Crawl {
		t.crawlBase = strings.TrimSuffix(strings.TrimSpace(opts.URL), "/") + "/"
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
	t.once.Do(func() {
		close(t.stop)
		t.log("warn", "stopped by user")
	})
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

// List returns all tasks (oldest first) plus aggregated totals. The totals
// combine persisted all-time counters with the bytes of currently running
// tasks, so the numbers survive restarts.
func (m *Manager) List() ([]*Task, Totals) {
	m.mu.Lock()
	ids := append([]string(nil), m.order...)
	m.mu.Unlock()

	out := make([]*Task, 0, len(ids))
	var tot Totals
	tot.StartedAt = m.started
	m.histMu.Lock()
	tot.BaseBytes = m.base.Bytes
	tot.BaseLoops = m.base.Loops
	tot.BaseFiles = m.base.Files
	m.histMu.Unlock()
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
		t.Speed = t.currentSpeed()
		tot.Speed += t.Speed
	}
	tot.Bytes += tot.BaseBytes
	tot.Loops += tot.BaseLoops
	tot.Files += tot.BaseFiles
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

// TaskLog returns the terminal lines of a task.
func (m *Manager) TaskLog(id string) ([]LogLine, bool) {
	m.mu.Lock()
	t, ok := m.tasks[id]
	m.mu.Unlock()
	if !ok {
		return nil, false
	}
	return t.LogLines(), true
}

// ---------- execution ----------

func (m *Manager) run(t *Task) {
	t.log("info", "task started · mode=%s dir=%s loops=%d conn=%d", t.opts.Mode, t.opts.Direction, t.opts.Loops, t.opts.Conn)
	if t.opts.StartAt != "" {
		t.log("warn", "scheduled: waiting for %s", t.opts.StartAt)
	}
	if err := t.waitScheduled(); err != nil {
		t.finish("stopped", "")
		t.log("warn", "stopped while waiting for schedule")
		return
	}
	if t.opts.StartAt != "" {
		t.log("ok", "schedule reached, starting")
	}
	if t.opts.Crawl {
		t.log("ok", "crawl started: %s (depth %s, filter %q)", t.crawlBase, depthStr(t.opts.Depth), t.opts.Include)
		m.startCrawl(t)
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
		t.log("ok", "crawl finished: %d files, %s", atomic.LoadInt64(&t.Files), humanBytes(atomic.LoadInt64(&t.Bytes)))
		m.recordHistory(t)
		return
	}
	if t.opts.VerifySHA != "" {
		t.log("info", "sha256 verification enabled (%s…)", strings.ToLower(t.opts.VerifySHA)[:8])
	}
	conns := t.opts.Conn
	if conns < 1 {
		conns = 1
	}
	if conns > 16 {
		conns = 16
	}
	if t.opts.Mode == ModeDisk && conns > 1 {
		// parallel streams would clobber the same output file
		conns = 1
		t.log("warn", "disk mode writes a single stream (parallel ignored)")
	}
	if conns > 1 {
		t.log("info", "spawning %d parallel streams", conns)
	}
	if conns == 1 {
		m.loopShard(t)
	} else {
		var wg sync.WaitGroup
		for i := 0; i < conns; i++ {
			wg.Add(1)
			go func(id int) { defer wg.Done(); t.log("info", "stream #%d open", id+1); m.loopShard(t) }(i)
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
	t.log("info", "task finished: %s", t.State)
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
		action := "GET"
		if t.opts.Direction == "up" {
			action = "POST"
		}
		loopTag := fmt.Sprintf("#%d", loop)
		if t.opts.Loops > 0 {
			loopTag = fmt.Sprintf("#%d/%d", loop, t.opts.Loops)
		}
		t.log("info", "loop %s: %s %s", loopTag, action, t.opts.URL)

		loopStart := time.Now()
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
			if attempt < tries {
				t.log("warn", "attempt %d/%d failed: %v — retrying in %ds", attempt, tries, err, t.opts.RetryWait)
				if attempt < tries && t.opts.RetryWait > 0 {
					for s := 0; s < t.opts.RetryWait; s++ {
						if t.stopped.Load() {
							return
						}
						time.Sleep(time.Second)
					}
				}
			} else {
				t.log("err", "attempt %d/%d failed: %v", attempt, tries, err)
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
		dur := time.Since(loopStart).Seconds()
		cur := atomic.LoadInt64(&t.Current)
		t.log("ok", "loop %s complete: %s in %.1fs", loopTag, humanBytes(cur), dur)
		if t.opts.VerifySHA != "" {
			t.mu.Lock()
			v := t.Verified
			t.mu.Unlock()
			if v == "ok" {
				t.log("ok", "sha256 verified")
			} else if v == "mismatch" {
				t.log("err", "sha256 MISMATCH")
			}
		}
		if t.opts.DeleteEach && t.opts.Mode == ModeDisk && t.outPath != "" {
			os.Remove(t.outPath)
			t.log("info", "file removed (delete_each): %s", t.outPath)
		}
		if t.opts.Interval > 0 && (t.opts.Loops == 0 || loop < int64(t.opts.Loops)) {
			t.log("info", "pausing %ds before next loop", t.opts.Interval)
			for s := 0; s < t.opts.Interval; s++ {
				if t.stopped.Load() {
					return
				}
				time.Sleep(time.Second)
			}
		}
	}
}

// humanBytes renders a byte count compactly for terminal lines.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
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
	return m.fetchFileInto(t, t.opts.URL, "")
}

func depthStr(d int) string {
	if d <= 0 {
		return "∞"
	}
	return strconv.Itoa(d)
}

// copyStop copies src→w in small chunks, aborting as soon as stopped() turns
// true so a "stop" click interrupts a huge in-flight download within ~64 KB.
func copyStop(w io.Writer, src io.Reader, stopped func() bool) error {
	buf := make([]byte, 64*1024)
	for {
		if stopped != nil && stopped() {
			return fmt.Errorf("stopped")
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

func httpGet(rawURL, proxy string, w io.Writer, stopped func() bool) error {
	client := newClient(proxy)
	resp, err := client.Get(rawURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return copyStop(w, resp.Body, stopped)
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
		if c.t.winStarted.IsZero() {
			c.t.winStarted = time.Now()
		}
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

func ftpGet(u *url.URL, w io.Writer, stopped func() bool) error {
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
	if err := copyStop(w, data, stopped); err != nil {
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
