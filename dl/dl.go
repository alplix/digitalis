// Package dl implements a resource-friendly download system. Each task
// streams a remote file through a small fixed buffer into one of three
// selectable sinks: discard (count only, no RAM no disk), a bounded RAM ring
// buffer (keeps the most recent N bytes), or a file on a storage root.
// Tasks may loop a fixed number of times or forever, optionally pausing
// between loops, and can be rate-limited so they never saturate the machine.
package dl

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
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
	RamMB      int    `json:"ram_mb"`              // RAM mode: ring buffer size (1-1024 MB)
	SaveDir    string `json:"save_dir,omitempty"`  // disk mode: absolute directory
	Filename   string `json:"filename,omitempty"`  // disk mode: override file name
	Loops      int    `json:"loops"`               // 0 = infinite
	Interval   int    `json:"interval"`            // seconds between loops
	SpeedCap   int64  `json:"speed_cap"`           // bytes/sec, 0 = unlimited
	DeleteEach bool   `json:"delete_each"`         // disk mode: remove the file after each loop
	NoVerify   bool   `json:"no_verify,omitempty"` // reserved
}

// Task is one download job with live counters.
type Task struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	Mode    Mode   `json:"mode"`
	RamMB   int    `json:"ram_mb,omitempty"`
	SaveDir string `json:"save_dir,omitempty"`
	Loops   int    `json:"loops"` // requested (0 = infinite)

	State      string `json:"state"` // running | done | error | stopped
	Bytes      int64  `json:"bytes"`
	Files      int64  `json:"files"`
	LoopsDone  int64  `json:"loops_done"`
	Speed      int64  `json:"speed"`
	UptimeSec  int64  `json:"uptime_sec"`
	Error      string `json:"error,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at,omitempty"`

	Current int64 `json:"-"` // bytes in the current loop

	opts      Options
	stop      chan struct{}
	once      sync.Once
	mu        sync.Mutex
	ring      *ring
	outPath   string
	started   time.Time

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

// Manager owns all download tasks.
type Manager struct {
	mu      sync.Mutex
	tasks   map[string]*Task
	order   []string
	started time.Time
}

// NewManager creates an empty download manager.
func NewManager() *Manager {
	return &Manager{tasks: make(map[string]*Task), started: time.Now()}
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
	if scheme != "http" && scheme != "https" && scheme != "ftp" {
		return nil, fmt.Errorf("unsupported scheme %q (use http, https or ftp)", u.Scheme)
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

	buf := make([]byte, 4)
	rand.Read(buf)
	t := &Task{
		ID:       hex.EncodeToString(buf),
		URL:      opts.URL,
		Mode:     opts.Mode,
		RamMB:    opts.RamMB,
		SaveDir:  opts.SaveDir,
		Loops:    opts.Loops,
		State:    "running",
		opts:     opts,
		stop:     make(chan struct{}),
		started:  time.Now(),
		StartedAt: time.Now(),
	}
	if opts.Mode == ModeRAM {
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
	for loop := int64(1); t.opts.Loops == 0 || loop <= int64(t.opts.Loops); loop++ {
		select {
		case <-t.stop:
			t.finish("stopped", "")
			return
		default:
		}
		atomic.StoreInt64(&t.Current, 0)
		t.winMu.Lock()
		t.winBytes = 0
		t.winStarted = time.Now()
		t.winMu.Unlock()

		err := m.fetchOnce(t)
		atomic.AddInt64(&t.LoopsDone, 1)
		if err != nil {
			t.finish("error", err.Error())
			return
		}
		if t.opts.DeleteEach && t.opts.Mode == ModeDisk && t.outPath != "" {
			os.Remove(t.outPath)
		}
		// pause between loops, in 1s slices so Stop reacts instantly
		if t.opts.Interval > 0 && (t.opts.Loops == 0 || loop < int64(t.opts.Loops)) {
			for s := 0; s < t.opts.Interval; s++ {
				select {
				case <-t.stop:
					t.finish("stopped", "")
					return
				case <-time.After(time.Second):
				}
			}
		}
	}
	t.finish("done", "")
}

func (t *Task) finish(state, errMsg string) {
	t.mu.Lock()
	if t.State == "running" {
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
	cw := &countingWriter{w: w, t: t, th: th}

	switch strings.ToLower(u.Scheme) {
	case "ftp":
		return ftpGet(u, cw)
	default:
		return httpGet(t.opts.URL, cw)
	}
}

func httpGet(rawURL string, w io.Writer) error {
	client := &http.Client{Timeout: 0} // no global timeout: infinite loops are legal
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
	elapsed := time.Since(t.winStarted).Seconds()
	if elapsed < 0.5 {
		elapsed = 0.5
	}
	if t.winStarted.IsZero() {
		return 0
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
	// re-baseline after the nap
	now = time.Now()
	th.last = now
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
	addr := net.JoinHostPort(u.Hostname(), orDefault(u.Port(), "21"))
	conn, err := net.DialTimeout("tcp", addr, 15*time.Second)
	if err != nil {
		return fmt.Errorf("ftp connect: %v", err)
	}
	defer conn.Close()
	c := &ftpConn{r: newBufReader(conn), w: conn}

	if _, _, err := c.respText(); err != nil { // greeting
		return err
	}
	user := u.User.Username()
	if user == "" {
		user = "anonymous"
	}
	pass, _ := u.User.Password()
	if pass == "" {
		pass = "digitalis@"
	}
	if _, err := c.cmd(230, "USER "+user); err != nil {
		if _, err := c.cmd(230, "PASS "+pass); err != nil {
			return err
		}
	}
	if _, err := c.cmd(0, "TYPE I"); err != nil {
		return err
	}
	pasv, err := c.pasv()
	if err != nil {
		return err
	}
	if u.Path == "" || u.Path == "/" {
		return fmt.Errorf("ftp: no path")
	}
	data, err := net.DialTimeout("tcp", pasv, 15*time.Second)
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
