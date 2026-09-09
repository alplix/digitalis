package dl

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Recursive mirror crawling: BFS over directory autoindexes, streaming every
// discovered file into the task's sink — the rsync-like "pull everything"
// mode. With discard mode this generates repo-shaped traffic without ever
// touching the disk.

// startCrawl enumerates every file under the crawl base and downloads it.
func (m *Manager) startCrawl(t *Task) {
	base := strings.TrimSuffix(t.crawlBase, "/") + "/"
	files := make(chan string, 512)
	var crawlWG, dlWG sync.WaitGroup
	var downloader = func() {
		defer dlWG.Done()
		for furl := range files {
			if t.stopped.Load() || t.stopReached() {
				continue // drain the queue without pulling more data
			}
			m.fetchCrawlFile(t, base, furl)
		}
	}

	workers := t.opts.Conn
	if workers < 2 {
		workers = 6
	}
	if workers > 16 {
		workers = 16
	}
	for i := 0; i < workers; i++ {
		dlWG.Add(1)
		go downloader()
	}

	crawlWG.Add(1)
	go func() {
		defer crawlWG.Done()
		defer close(files)
		type item struct {
			url   string
			depth int
		}
		queue := []item{{base, 0}}
		visited := map[string]bool{base: true}
		var re *regexp.Regexp
		if strings.TrimSpace(t.opts.Include) != "" {
			re, _ = regexp.Compile(`(?i)` + t.opts.Include)
		}
		for len(queue) > 0 {
			if t.stopped.Load() || t.stopReached() {
				return
			}
			it := queue[0]
			queue = queue[1:]
			if t.opts.Depth > 0 && it.depth > t.opts.Depth {
				continue
			}
			atomic.AddInt64(&t.Crawled, 1)
			entries, err := BrowseRepo(it.url, t.opts.Proxy)
			if err != nil {
				t.log("warn", "index failed: %s (%v)", it.url, err)
				continue
			}
			var foundHere int
			for _, e := range entries {
				if e.IsDir {
					if _, seen := visited[e.URL]; !seen {
						visited[e.URL] = true
						queue = append(queue, item{e.URL, it.depth + 1})
					}
					continue
				}
				if re != nil && !re.MatchString(e.Name) {
					continue
				}
				if t.opts.MinSize > 0 && e.Size > 0 && e.Size < t.opts.MinSize {
					continue
				}
				if t.opts.MaxSize > 0 && e.Size > 0 && e.Size > t.opts.MaxSize {
					continue
				}
				foundHere++
				atomic.AddInt64(&t.Found, 1)
				files <- e.URL
			}
			t.log("info", "index ok: %s (%d files)", it.url, foundHere)
		}
	}()

	crawlWG.Wait()
	dlWG.Wait()
}

// fetchCrawlFile downloads one crawled file, preserving its relative path in
// disk mode. A single retry round applies per file.
func (m *Manager) fetchCrawlFile(t *Task, base, furl string) {
	tries := t.opts.Retries + 1
	var lastErr error
	for attempt := 1; attempt <= tries; attempt++ {
		if t.stopped.Load() {
			return
		}
		lastErr = m.fetchFileInto(t, furl, base)
		if lastErr == nil {
			atomic.AddInt64(&t.Files, 1)
			if n := atomic.LoadInt64(&t.Files); n%25 == 0 {
				t.log("ok", "progress: %d files, %s", n, humanBytes(atomic.LoadInt64(&t.Bytes)))
			}
			return
		}
		if t.stopped.Load() {
			return
		}
		if attempt < tries && t.opts.RetryWait > 0 {
			time.Sleep(time.Duration(t.opts.RetryWait) * time.Second)
		}
	}
	t.log("err", "file failed: %s (%v)", furl, lastErr)
	t.mu.Lock()
	if t.Error == "" {
		t.Error = lastErr.Error()
	}
	t.mu.Unlock()
}

// fetchFileInto downloads one file URL into the task's sink. base ("" = not
// crawling) makes disk mode preserve the URL path relative to the crawl root.
func (m *Manager) fetchFileInto(t *Task, rawURL, base string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	th := &throttle{rate: t.opts.SpeedCap}
	var w io.Writer
	switch t.opts.Mode {
	case ModeDiscard:
		w = io.Discard
	case ModeRAM:
		if t.ring == nil {
			t.ring = newRing(t.opts.RamMB * 1024 * 1024)
		}
		w = t.ring
	case ModeDisk:
		rel := u.Path
		if base != "" {
			bu, berr := url.Parse(base)
			if berr == nil {
				rel = strings.TrimPrefix(u.Path, bu.Path)
			}
		}
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			rel = baseName(u)
		}
		t.outPath = filepath.Join(t.opts.SaveDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(t.outPath), 0755); err != nil {
			return err
		}
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
		xferErr = ftpGet(u, cw, t.dlStopped)
	default:
		xferErr = httpGet(rawURL, t.opts.Proxy, cw, t.dlStopped)
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
	}
	return nil
}

var _ = sync.Once{}
var _ = os.PathSeparator
