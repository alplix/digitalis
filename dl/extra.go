package dl

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
)// Upload direction, verification helpers, counters and run history.

// pushOnce generates (or reads) a payload and sends it to the target URL.
// HTTP targets receive a POST; FTP targets receive an FTP STOR.
func (t *Task) pushOnce() error {
	u, err := url.Parse(t.opts.URL)
	if err != nil {
		return err
	}
	th := &throttle{rate: t.opts.SpeedCap}
	size := int64(t.opts.UploadMB) * 1024 * 1024

	var hasher hash.Hash
	var body io.Reader = &zeroReader{remaining: size}
	if strings.TrimSpace(t.opts.VerifySHA) != "" {
		hasher = sha256.New()
		body = io.TeeReader(body, hasher)
	}
	cr := &countingReader{r: body, t: t, th: th}

	switch strings.ToLower(u.Scheme) {
	case "ftp":
		if err := ftpPut(u, size, cr); err != nil {
			return err
		}
	default:
		client := newClient(t.opts.Proxy)
		req, err := http.NewRequest("POST", t.opts.URL, cr)
		if err != nil {
			return err
		}
		req.ContentLength = size
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("HTTP %d", resp.StatusCode)
		}
	}
	atomic.AddInt64(&t.Files, 1)
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

// zeroReader yields size zero bytes.
type zeroReader struct{ remaining int64 }

func (z *zeroReader) Read(p []byte) (int, error) {
	if z.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > z.remaining {
		p = p[:z.remaining]
	}
	for i := range p {
		p[i] = 0
	}
	z.remaining -= int64(len(p))
	return len(p), nil
}

// countingReader counts bytes flowing out of a reader (upload side).
type countingReader struct {
	r  io.Reader
	t  *Task
	th *throttle
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.th.wait(n)
		atomic.AddInt64(&c.t.Bytes, int64(n))
		atomic.AddInt64(&c.t.Current, int64(n))
		c.t.winMu.Lock()
		c.t.winBytes += int64(n)
		c.t.winMu.Unlock()
	}
	return n, err
}

// ftpPut uploads size bytes to the FTP path via STOR.
func ftpPut(u *url.URL, size int64, body io.Reader) error {
	c, err := ftpDial(u)
	if err != nil {
		return err
	}
	defer c.close()

	path := u.Path
	if path == "" || path == "/" {
		return fmt.Errorf("ftp: no path")
	}
	pasv, err := c.pasv()
	if err != nil {
		return err
	}
	data, err := dialTimeout(pasv)
	if err != nil {
		return fmt.Errorf("ftp data connect: %v", err)
	}
	defer data.Close()
	if _, err := c.cmd(0, "STOR "+path); err != nil { // 125 or 150
		return err
	}
	buf := make([]byte, 64*1024)
	if _, err := io.CopyBuffer(data, body, buf); err != nil {
		return err
	}
	_, _, _ = c.respText() // 226 (best effort)
	_, _ = c.cmd(0, "QUIT")
	return nil
}

// ---------- history ----------

// HistoryEntry is one finished task kept for reporting.
type HistoryEntry struct {
	URL         string    `json:"url"`
	Mode        string    `json:"mode"`
	Direction   string    `json:"direction"`
	State       string    `json:"state"`
	Bytes       int64     `json:"bytes"`
	Files       int64     `json:"files"`
	Loops       int64     `json:"loops"`
	DurationSec int64     `json:"duration_sec"`
	AvgSpeed    int64     `json:"avg_speed"`
	Verified    string    `json:"verified,omitempty"`
	Error       string    `json:"error,omitempty"`
	FinishedAt  time.Time `json:"finished_at"`
}

const historyMax = 500

// persisted wrap: all-time base counters + run history.
type histFile struct {
	Bytes   int64          `json:"bytes"`
	Loops   int64          `json:"loops"`
	Files   int64          `json:"files"`
	History []HistoryEntry `json:"history"`
}

func (m *Manager) recordHistory(t *Task) {
	if m == nil {
		return
	}
	t.mu.Lock()
	duration := int64(0)
	if !t.EndedAt.IsZero() {
		duration = int64(t.EndedAt.Sub(t.StartedAt).Seconds())
	}
	entry := HistoryEntry{
		URL:         t.URL,
		Mode:        string(t.Mode),
		Direction:   t.Direction,
		State:       t.State,
		Bytes:       atomic.LoadInt64(&t.Bytes),
		Files:       atomic.LoadInt64(&t.Files),
		Loops:       atomic.LoadInt64(&t.LoopsDone),
		DurationSec: duration,
		Verified:    t.Verified,
		Error:       t.Error,
		FinishedAt:  t.EndedAt,
	}
	t.mu.Unlock()
	if entry.DurationSec > 0 {
		entry.AvgSpeed = entry.Bytes / entry.DurationSec
	}

	m.histMu.Lock()
	m.history = append(m.history, entry)
	if len(m.history) > historyMax {
		m.history = m.history[len(m.history)-historyMax:]
	}
	// fold the finished task into the all-time counters
	m.base.Bytes += entry.Bytes
	m.base.Loops += entry.Loops
	m.base.Files += entry.Files
	snap := histFile{Bytes: m.base.Bytes, Loops: m.base.Loops, Files: m.base.Files, History: m.history}
	path := m.histPath
	m.histMu.Unlock()

	if path != "" {
		if data, err := json.Marshal(snap); err == nil {
			tmp := path + ".tmp"
			if os.WriteFile(tmp, data, 0644) == nil {
				os.Rename(tmp, path)
			}
		}
	}
}

func (m *Manager) loadHistory() {
	if m.histPath == "" {
		return
	}
	data, err := os.ReadFile(m.histPath)
	if err != nil {
		return
	}
	var wrapped histFile
	if json.Unmarshal(data, &wrapped) == nil && wrapped.History != nil {
		m.histMu.Lock()
		m.history = wrapped.History
		m.base.Bytes = wrapped.Bytes
		m.base.Loops = wrapped.Loops
		m.base.Files = wrapped.Files
		m.histMu.Unlock()
		return
	}
	// legacy format: plain array of entries
	var legacy []HistoryEntry
	if json.Unmarshal(data, &legacy) == nil && len(legacy) > 0 {
		m.histMu.Lock()
		m.history = legacy
		m.histMu.Unlock()
	}
}

// History returns the stored run history (oldest first).
func (m *Manager) History() []HistoryEntry {
	m.histMu.Lock()
	defer m.histMu.Unlock()
	out := make([]HistoryEntry, len(m.history))
	copy(out, m.history)
	return out
}

// ClearHistory wipes the stored run history and all-time counters.
func (m *Manager) ClearHistory() {
	m.histMu.Lock()
	m.history = nil
	m.base.Bytes, m.base.Loops, m.base.Files = 0, 0, 0
	path := m.histPath
	m.histMu.Unlock()
	if path != "" {
		os.Remove(path)
	}
}
