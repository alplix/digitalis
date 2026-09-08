// Package wget implements a wget-style download manager with recursive HTTP
// crawling, recursive FTP mirroring, delete-after mode, periodic looping and
// live bandwidth statistics. It deliberately does not shell out to wget.
package wget

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/html"
)

// Options configures a new wget task (mutable-free snapshot).
type Options struct {
	URL         string `json:"url"`
	Recursive   bool   `json:"recursive"`
	Depth       int    `json:"depth"` // 0 = unlimited
	DeleteAfter bool   `json:"delete_after"`
	NoDNSCache  bool   `json:"no_dns_cache"`
	Loop        bool   `json:"loop"`
	Interval    int    `json:"interval"` // seconds between loops
	Disk        string `json:"disk"`
	Category    string `json:"category"`
}

// Task holds one download job plus its live statistics.
type Task struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	Recursive bool  `json:"recursive"`
	Depth    int    `json:"depth"` // 0 = unlimited
	DeleteAfter bool `json:"delete_after"`
	NoDNSCache  bool `json:"no_dns_cache"`
	Loop        bool `json:"loop"`
	Interval    int  `json:"interval"` // seconds between loops
	Disk        string `json:"disk"`
	Category    string `json:"category"`

	mu         sync.Mutex
	State      string    `json:"state"` // queued|running|done|error|stopped
	Err        string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at"`
	Bytes      int64     `json:"bytes"`
	Files      int64     `json:"files"`
	Loops      int64     `json:"loops"`
	CurrentURL string    `json:"current_url"`

	window  []int64
	lastNow time.Time

	cancel context.CancelFunc
}

// Manager coordinates running wget tasks and aggregates bandwidth totals.
type Manager struct {
	mu    sync.Mutex
	tasks map[string]*Task
	baseDir string

	TotalBytes  int64        `json:"total_bytes"`
	TotalFiles  int64        `json:"total_files"`
	StartedAt   time.Time    `json:"started_at"`
	totalWindow []int64
	totalTimes  []time.Time
}

// NewManager creates an empty wget task manager; baseDir is the default save
// directory used when no disk is selected.
func NewManager(baseDir string) *Manager {
	return &Manager{tasks: map[string]*Task{}, baseDir: baseDir, StartedAt: time.Now()}
}

// newID returns a short random id.
func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Add registers and starts a task.
func (m *Manager) Add(conf Options) (*Task, error) {
	if conf.URL == "" {
		return nil, fmt.Errorf("empty url")
	}
	u, err := url.Parse(conf.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "ftp") {
		return nil, fmt.Errorf("unsupported url (use http, https or ftp)")
	}
	t := &Task{
		ID: newID(), URL: conf.URL, Recursive: conf.Recursive, Depth: conf.Depth,
		DeleteAfter: conf.DeleteAfter, NoDNSCache: conf.NoDNSCache, Loop: conf.Loop,
		Interval: conf.Interval, Disk: conf.Disk, Category: conf.Category,
		State: "queued", StartedAt: time.Now(), window: []int64{},
	}
	m.mu.Lock()
	m.tasks[t.ID] = t
	m.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	go m.run(ctx, t)
	return t, nil
}

// Totals aggregates all tasks for the dashboard.
type Totals struct {
	Bytes     int64     `json:"bytes"`
	Files     int64     `json:"files"`
	StartedAt time.Time `json:"started_at"`
	Speed     int64     `json:"speed"`
}

// List returns all tasks ordered by start time, plus aggregate stats.
func (m *Manager) List() ([]*Task, Totals) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		out = append(out, t)
	}
	sortTasks(out)
	return out, Totals{
		Bytes:     atomic.LoadInt64(&m.TotalBytes),
		Files:     atomic.LoadInt64(&m.TotalFiles),
		StartedAt: m.StartedAt,
		Speed:     avgWindow(m.totalWindow),
	}
}

func sortTasks(ts []*Task) {
	for i := 1; i < len(ts); i++ {
		for j := i; j > 0 && ts[j].StartedAt.Before(ts[j-1].StartedAt); j-- {
			ts[j], ts[j-1] = ts[j-1], ts[j]
		}
	}
}

// Stop cancels a running task (and removes it from the list).
func (m *Manager) Stop(id string) error {
	m.mu.Lock()
	t, ok := m.tasks[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("task not found")
	}
	t.mu.Lock()
	if t.cancel != nil {
		t.cancel()
	}
	t.mu.Unlock()
	m.Remove(id)
	return nil
}

// Remove drops a finished task from the list.
func (m *Manager) Remove(id string) {
	m.mu.Lock()
	delete(m.tasks, id)
	m.mu.Unlock()
}

// run drives a task, running loops as configured.
func (m *Manager) run(ctx context.Context, t *Task) {
	t.mu.Lock()
	t.State = "running"
	t.mu.Unlock()
	client := newClient(t.NoDNSCache)
	for {
		t.mu.Lock()
		t.Loops++
		loopNo := t.Loops
		t.mu.Unlock()
		m.fetch(ctx, t, client, t.URL, 0, t.Recursive)
		t.mu.Lock()
		t.CurrentURL = ""
		if !t.Loop {
			t.State = "done"
			t.EndedAt = time.Now()
			t.mu.Unlock()
			break
		}
		interval := time.Duration(t.Interval) * time.Second
		if t.Interval <= 0 {
			interval = 60 * time.Second
		}
		t.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		_ = loopNo
	}
}

// snapshotFreeze returns a copy of the task's stats for speed computation.
func (t *Task) pushBytes(n int64) {
	t.mu.Lock()
	if n > 0 {
		t.Bytes += n
	}
	now := time.Now()
	if t.lastNow.IsZero() {
		t.lastNow = now
	}
	dt := now.Sub(t.lastNow).Seconds()
	t.lastNow = now
	if dt > 0 {
		t.window = append(t.window, int64(float64(n)/dt))
		if len(t.window) > 30 {
			t.window = t.window[1:]
		}
	}
	t.mu.Unlock()
}

// totalPush adds to the global manager byte total and bandwidth window.
func (m *Manager) totalPush(n int64) {
	if n <= 0 {
		return
	}
	atomic.AddInt64(&m.TotalBytes, n)
	now := time.Now()
	m.mu.Lock()
	if len(m.totalTimes) == 0 {
		m.totalTimes = append(m.totalTimes, now)
		m.totalWindow = append(m.totalWindow, n)
	} else {
		last := m.totalTimes[len(m.totalTimes)-1]
		if now.Sub(last) > 50*time.Millisecond {
			m.totalTimes = append(m.totalTimes, now)
			m.totalWindow = append(m.totalWindow, n)
		} else {
			m.totalWindow[len(m.totalWindow)-1] += n
		}
		if len(m.totalWindow) > 120 {
			m.totalWindow = m.totalWindow[len(m.totalWindow)-120:]
			m.totalTimes = m.totalTimes[len(m.totalTimes)-120:]
		}
	}
	m.mu.Unlock()
}

// Speed returns bytes/sec averaged over the recent window.
func (t *Task) Speed() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return avgWindow(t.window)
}

// Uptime returns elapsed running time.
func (t *Task) Uptime() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	base := t.StartedAt
	if !t.EndedAt.IsZero() {
		base = t.EndedAt
	}
	d := time.Since(base)
	if d < 0 {
		d = 0
	}
	return d
}

func avgWindow(w []int64) int64 {
	if len(w) == 0 {
		return 0
	}
	var sum int64
	for _, v := range w {
		sum += v
	}
	return sum / int64(len(w))
}

// newClient builds an HTTP client; no_dns_cache keeps a fresh transport per
// request set (disabling connection reuse across dials implies re-resolution).
func newClient(noDNSCache bool) *http.Client {
	if noDNSCache {
		dialer := &net.Dialer{Timeout: 20 * time.Second, KeepAlive: 10 * time.Second}
		tr := &http.Transport{DialContext: dialer.DialContext, DisableKeepAlives: true}
		return &http.Client{Timeout: 5 * time.Minute, Transport: tr}
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

// fetch downloads one URL; when recursive is set it also walks links.
func (m *Manager) fetch(ctx context.Context, t *Task, client *http.Client, u string, depth int, recursive bool) {
	if ctx.Err() != nil {
		return
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return
	}
	switch parsed.Scheme {
	case "ftp":
		m.fetchFTP(ctx, t, u, depth, recursive)
	default:
		m.fetchHTTP(ctx, t, client, u, depth, recursive)
	}
}

// fetchHTTP GETs a URL; for recursive mode an HTML page's links are traversed
// while same-host file links are downloaded body-only or to disk.
func (m *Manager) fetchHTTP(ctx context.Context, t *Task, client *http.Client, u string, depth int, recursive bool) {
	if ctx.Err() != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", "digitalis-wget/1.0")
	resp, err := client.Do(req)
	if err != nil {
		m.fail(t, "http: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		m.fail(t, "http status "+resp.Status)
		return
	}
	ct := resp.Header.Get("Content-Type")
	isHTML := strings.Contains(ct, "text/html") || (strings.Contains(ct, "text/") && strings.HasSuffix(strings.ToLower(u), ".html"))
	t.mu.Lock()
	t.CurrentURL = u
	t.mu.Unlock()
	if recursive && isHTML && (t.Depth == 0 || depth < t.Depth) {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			m.fail(t, err.Error())
			return
		}
		m.addBytes(t, int64(len(body)))
		baseURL, err := url.Parse(u)
		if err != nil {
			return
		}
		links := extractLinks(string(body))
		for _, link := range links {
			if ctx.Err() != nil {
				break
			}
			lu, err := url.Parse(link)
			if err != nil || lu.Scheme == "mailto" || lu.Scheme == "javascript" || lu.Scheme == "data" {
				continue
			}
			// Resolve relative links against the page URL, stay on the same host.
			abs, err := baseURL.Parse(link)
			if err != nil || (abs.Scheme != "http" && abs.Scheme != "https" && abs.Scheme != "ftp") {
				continue
			}
			if abs.Host != baseURL.Host {
				continue
			}
			m.fetch(ctx, t, client, abs.String(), depth+1, recursive)
		}
		return
	}
	// Plain file: honor delete_after (measure bandwidth, write nothing) or save.
	if t.DeleteAfter {
		_, err := io.Copy(io.Discard, &countingReader{r: resp.Body, t: t, m: m})
		if err != nil && ctx.Err() == nil {
			m.fail(t, err.Error())
			return
		}
		m.bumpFiles(t)
	} else {
		if err := m.saveHTTP(t, u, resp); err != nil && ctx.Err() == nil {
			m.fail(t, err.Error())
		}
	}
}

// saveHTTP writes the body to the disk/category destination.
func (m *Manager) saveHTTP(t *Task, u string, resp *http.Response) error {
	dest := filepath.Join(m.resolveDest(t), filepath.Base(path.Base(u)))
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, &countingReader{r: resp.Body, t: t, m: m})
	cerr := f.Close()
	if err != nil {
		os.Remove(dest)
		return err
	}
	if cerr == nil {
		m.bumpFiles(t)
	}
	return cerr
}

// resolveDest returns the base directory for the task's disk+category target.
func (m *Manager) resolveDest(t *Task) string {
	base := m.baseDir
	if len(t.Disk) > 0 {
		base = t.Disk
	}
	if t.Category != "" {
		base = filepath.Join(base, filepath.FromSlash(t.Category))
	}
	return base
}

// countingReader feeds per-task and global byte counters.
type countingReader struct {
	r io.Reader
	t *Task
	m *Manager
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	if n > 0 {
		cr.t.pushBytes(int64(n))
		cr.m.totalPush(int64(n))
	}
	return n, err
}

func (m *Manager) addBytes(t *Task, n int64) {
	if n <= 0 {
		return
	}
	t.pushBytes(n)
	m.totalPush(n)
}

// bumpFiles records one completed downloaded file on the task and globally.
func (m *Manager) bumpFiles(t *Task) {
	t.mu.Lock()
	t.Files++
	t.mu.Unlock()
	atomic.AddInt64(&m.TotalFiles, 1)
}

// fail records a task error (only once; loops keep going in loop mode).
func (m *Manager) fail(t *Task, msg string) {
	t.mu.Lock()
	if t.State == "running" && t.Err == "" {
		t.Err = msg
	}
	t.mu.Unlock()
}

// extractLinks pulls href/src URLs out of an HTML document.
func extractLinks(body string) []string {
	var out []string
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		return out
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			for _, a := range n.Attr {
				if (a.Key == "href" || a.Key == "src") && a.Val != "" {
					v := strings.TrimSpace(a.Val)
					if v != "" && !strings.HasPrefix(v, "#") {
						out = append(out, v)
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return out
}

// fetchFTP implements a minimal passive-mode FTP client supporting recursive
// directory mirrors and single-file downloads.
func (m *Manager) fetchFTP(ctx context.Context, t *Task, u string, depth int, recursive bool) {
	parsed, err := url.Parse(u)
	if err != nil {
		m.fail(t, "ftp: bad url")
		return
	}
	user, pass := "anonymous", ""
	if parsed.User != nil {
		user = parsed.User.Username()
		pass, _ = parsed.User.Password()
		if pass == "" {
			pass = "anonymous@"
		}
	}
	host := parsed.Host
	remotePath := parsed.Path
	if remotePath == "" {
		remotePath = "/"
	}
	fc, err := ftpConnect(ctx, host, user, pass)
	if err != nil {
		m.fail(t, "ftp: "+err.Error())
		return
	}
	defer fc.Close()
	cwd := remotePath
	// Normalize: strip trailing slash except root.
	if len(cwd) > 1 && strings.HasSuffix(cwd, "/") {
		cwd = strings.TrimSuffix(cwd, "/")
	}
	if recursive {
		m.ftpWalk(ctx, t, fc, cwd, depth)
	} else {
		fname := path.Base(cwd)
		if fname == "" || fname == "." || fname == "/" {
			m.fail(t, "ftp: no filename in url")
			return
		}
		dest := filepath.Join(m.resolveDest(t), fname)
		if err := m.ftpRetr(ctx, t, fc, cwd, dest); err != nil {
			m.fail(t, "ftp: "+err.Error())
		}
	}
}

// ftpWalk lists a directory and recurses into subdirectories.
func (m *Manager) ftpWalk(ctx context.Context, t *Task, fc *ftpConn, dir string, depth int) {
	if ctx.Err() != nil {
		return
	}
	t.mu.Lock()
	t.CurrentURL = "ftp://" + fc.host + dir
	t.mu.Unlock()
	list, err := fc.List(dir)
	if err != nil {
		m.fail(t, "ftp list "+dir+": "+err.Error())
		return
	}
	for _, e := range list {
		if ctx.Err() != nil {
			return
		}
		if strings.HasPrefix(e.Name, ".") {
			continue
		}
		child := joinURLPath(dir, e.Name)
		if e.IsDir {
			if t.Depth == 0 || depth < t.Depth {
				m.ftpWalk(ctx, t, fc, child, depth+1)
			}
			continue
		}
		if t.DeleteAfter {
			dest := ""
			if err := m.ftpRetr(ctx, t, fc, child, dest); err != nil && ctx.Err() == nil {
				m.fail(t, "ftp "+child+": "+err.Error())
			}
			continue
		}
		dest := filepath.Join(m.resolveDest(t), filepath.FromSlash(strings.TrimPrefix(child, "/")))
		if err := m.ftpRetr(ctx, t, fc, child, dest); err != nil && ctx.Err() == nil {
			m.fail(t, "ftp "+child+": "+err.Error())
		}
	}
}

func joinURLPath(dir, name string) string {
	if dir == "/" {
		return "/" + name
	}
	return strings.TrimSuffix(dir, "/") + "/" + name
}

// ftpRetr downloads one file from dir path to dest ("" = discard).
func (m *Manager) ftpRetr(ctx context.Context, t *Task, fc *ftpConn, remote, dest string) error {
	if dest != "" {
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return err
		}
	}
	rc, err := fc.Retr(remote)
	if err != nil {
		return err
	}
	defer rc.Close()
	cr := &countingReader{r: rc, t: t}
	if dest == "" {
		_, err = io.Copy(io.Discard, cr)
		return err
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, cr)
	cerr := f.Close()
	if err != nil {
		os.Remove(dest)
		return err
	}
	return cerr
}

// --- minimal FTP client -----------------------------------------------------

type ftpConn struct {
	conn net.Conn
	host string
}

const ftpTimeout = 30 * time.Second

func ftpConnect(ctx context.Context, host, user, pass string) (*ftpConn, error) {
	d := net.Dialer{Timeout: ftpTimeout}
	c, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, err
	}
	fc := &ftpConn{conn: c, host: host}
	if err := fc.readReplyExpect(220); err != nil {
		c.Close()
		return nil, err
	}
	if err := fc.cmd("USER " + user); err != nil {
		c.Close()
		return nil, err
	}
	if err := fc.readReplyExpect(331); err != nil {
		// some servers accept login immediately
		fc.conn.Close()
		return nil, fmt.Errorf("user rejected")
	}
	if err := fc.cmd("PASS " + pass); err != nil {
		c.Close()
		return nil, err
	}
	if err := fc.readReplyExpect(230); err != nil {
		fc.conn.Close()
		return nil, fmt.Errorf("login failed")
	}
	if err := fc.cmd("TYPE I"); err != nil {
		c.Close()
		return nil, err
	}
	fc.readReply()
	return fc, nil
}

func (fc *ftpConn) Close() {
	fc.conn.Close()
}

func (fc *ftpConn) cmd(s string) error {
	fc.conn.SetDeadline(time.Now().Add(ftpTimeout))
	_, err := fc.conn.Write([]byte(s + "\r\n"))
	return err
}

func (fc *ftpConn) readLine() (string, error) {
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 1)
	for {
		n, err := fc.conn.Read(tmp)
		if n > 0 {
			if tmp[0] == '\n' {
				break
			}
			buf = append(buf, tmp[0])
		}
		if err != nil {
			return "", err
		}
	}
	return strings.TrimSpace(string(buf)), nil
}

func (fc *ftpConn) readReply() error {
	fc.conn.SetDeadline(time.Now().Add(ftpTimeout))
	line, err := fc.readLine()
	if err != nil {
		return err
	}
	// Multi-line replies "123-..." keep reading until a plain "123 " line.
	if len(line) >= 4 && line[3] == '-' {
		code := line[:3]
		for {
			next, err := fc.readLine()
			if err != nil {
				return err
			}
			if len(next) >= 4 && next[:3] == code && next[3] == ' ' {
				break
			}
		}
	}
	return nil
}

func (fc *ftpConn) readReplyExpect(code int) error {
	fc.conn.SetDeadline(time.Now().Add(ftpTimeout))
	line, err := fc.readLine()
	if err != nil {
		return err
	}
	got := 0
	for i := 0; i < len(line) && line[i] >= '0' && line[i] <= '9'; i++ {
		got = got*10 + int(line[i]-'0')
	}
	if got != code {
		return fmt.Errorf("ftp: expected %d got %d (%s)", code, got, line)
	}
	return nil
}

// List fetches a directory listing via PASV LIST.
type ftpEntry struct {
	Name string
	IsDir bool
}

func (fc *ftpConn) List(dir string) ([]ftpEntry, error) {
	dc, err := fc.pasv()
	if err != nil {
		return nil, err
	}
	defer dc.Close()
	if err := fc.cmd("LIST " + dir); err != nil {
		return nil, err
	}
	if err := fc.readReplyExpect(150); err != nil {
		return nil, fmt.Errorf("list refused")
	}
	data, err := io.ReadAll(dc)
	if err != nil {
		return nil, err
	}
	if err := fc.readReply(); err != nil {
		return nil, err
	}
	var out []ftpEntry
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		name, isDir, ok := parseListLine(line)
		if ok && name != "" {
			out = append(out, ftpEntry{Name: name, IsDir: isDir})
		}
	}
	return out, nil
}

// parseListLine handles both unix and Windows-style LIST output.
func parseListLine(line string) (name string, isDir bool, ok bool) {
	// unix: drwxr-xr-x   1 user group 4096 Jan 01 12:00 name
	if strings.HasPrefix(line, "d") && len(line) > 4 && strings.ContainsAny(line[1:4], "rwx") {
		return parseUnix(line[0] == 'd', line)
	}
	if strings.HasPrefix(line, "-") && len(line) > 4 && strings.ContainsAny(line[1:4], "rwx") {
		return parseUnix(false, line)
	}
	// windows: 01-01-26  12:00PM       <DIR>  name   or   file size
	if strings.Contains(line, " <DIR>") || strings.HasPrefix(line, "d") {
		idx := strings.Index(line, "<DIR>")
		if idx >= 0 {
			rest := strings.TrimSpace(line[idx+5:])
			return rest, true, true
		}
	}
	fields := strings.Fields(line)
	if len(fields) >= 4 {
		// windows file: 09-08-26  12:34PM   12345 name.txt
		name = fields[len(fields)-1]
		if !strings.HasPrefix(fields[0], "-") {
			return name, false, true
		}
	}
	return "", false, false
}

func parseUnix(isDir bool, line string) (string, bool, bool) {
	idx := strings.Index(line, ":")
	if idx < 0 {
		return "", false, false
	}
	name := strings.TrimSpace(line[idx+2:])
	return name, isDir, name != ""
}

// pasv opens a passive data connection.
func (fc *ftpConn) pasv() (net.Conn, error) {
	if err := fc.cmd("PASV"); err != nil {
		return nil, err
	}
	line, err := fc.readLine()
	if err != nil {
		return nil, err
	}
	start := strings.Index(line, "(")
	end := strings.Index(line, ")")
	if start < 0 || end < 0 {
		return nil, fmt.Errorf("pasv reply malformed")
	}
	parts := strings.Split(line[start+1:end], ",")
	if len(parts) < 6 {
		return nil, fmt.Errorf("pasv reply malformed")
	}
	var nums [6]int
	for i := 0; i < 6; i++ {
		fmt.Sscanf(parts[i], "%d", &nums[i])
	}
	target := fmt.Sprintf("%d.%d.%d.%d:%d", nums[0], nums[1], nums[2], nums[3], nums[4]*256+nums[5])
	d := net.Dialer{Timeout: ftpTimeout}
	dc, err := d.Dial("tcp", target)
	if err != nil {
		return nil, err
	}
	return dc, nil
}

// Retr downloads remote path over a passive data connection.
func (fc *ftpConn) Retr(remote string) (io.ReadCloser, error) {
	dc, err := fc.pasv()
	if err != nil {
		return nil, err
	}
	if err := fc.cmd("RETR " + remote); err != nil {
		dc.Close()
		return nil, err
	}
	if err := fc.readReplyExpect(150); err != nil {
		dc.Close()
		return nil, fmt.Errorf("retr refused")
	}
	return &ftpDataCloser{dc: dc, conn: fc.conn}, nil
}

type ftpDataCloser struct {
	dc   net.Conn
	conn net.Conn
}

func (f *ftpDataCloser) Read(p []byte) (int, error) { return f.dc.Read(p) }

func (f *ftpDataCloser) Close() error {
	err := f.dc.Close()
	f.conn.SetDeadline(time.Now().Add(ftpTimeout))
	f.conn.Read(make([]byte, 512)) // drain the "226 transfer complete" reply
	return err
}