package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/alplix/digitalis/torrente"
)

// rssItem is a single matched feed entry.
type rssItem struct {
	GUID    string    `json:"guid"`
	Title   string    `json:"title"`
	Link    string    `json:"link"`
	Size    int64     `json:"size,omitempty"`
	AddedAt time.Time `json:"added_at"`
}

// rssFeed is a subscribed RSS/Atom feed that auto-downloads matching items.
type rssFeed struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Keywords  []string  `json:"keywords"`  // title must contain any of these
	Blacklist []string  `json:"blacklist"` // title must contain none of these
	Category  string    `json:"category"`  // target folder under the save dir
	Interval  int       `json:"interval"`  // minutes, 0 = 15
	SizeMax   int64     `json:"size_max"`  // bytes, 0 = unlimited
	Enabled   bool      `json:"enabled"`
	Seen      []string  `json:"seen"`    // item GUIDs already processed (capped)
	Recent    []rssItem `json:"recent"`  // last matched items (capped)
	LastPoll  time.Time `json:"last_poll"`
}

const (
	rssMaxSeen    = 200
	rssMaxRecent  = 12
	rssDefaultInt = 15
)

type rssXMLDoc struct {
	Channel struct {
		Items []rssRSSItem `xml:"item"`
	} `xml:"channel"`
	RootItems []rssRSSItem `xml:"item"`
	Atoms     []rssAtomItem `xml:"entry"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
}

type rssRSSItem struct {
	Title    string `xml:"title"`
	Link     string `xml:"link"`
	GUID     string `xml:"guid"`
	Enclosure *struct {
		URL    string `xml:"url,attr"`
		Length int64  `xml:"length,attr"`
	} `xml:"enclosure"`
	InfoHash  string `xml:"infohash"`
	MagnetURI string `xml:"magneturi"`
}

type rssAtomItem struct {
	Title string     `xml:"title"`
	ID    string     `xml:"id"`
	Links []atomLink `xml:"link"`
}

// feedItem is a normalized feed entry.
type feedItem struct {
	Title string
	GUID  string
	Srcs  []string
	Size  int64
}

func itemFromRSS(it rssRSSItem) feedItem {
	fi := feedItem{Title: strings.TrimSpace(it.Title), GUID: strings.TrimSpace(it.GUID)}
	fi.Srcs = append(fi.Srcs, strings.TrimSpace(it.MagnetURI), strings.TrimSpace(it.InfoHash))
	if it.Enclosure != nil {
		fi.Srcs = append(fi.Srcs, strings.TrimSpace(it.Enclosure.URL))
		fi.Size = it.Enclosure.Length
	}
	fi.Srcs = append(fi.Srcs, strings.TrimSpace(it.Link))
	return fi
}

func itemFromAtom(it rssAtomItem) feedItem {
	fi := feedItem{Title: strings.TrimSpace(it.Title), GUID: strings.TrimSpace(it.ID)}
	for _, l := range it.Links {
		fi.Srcs = append(fi.Srcs, strings.TrimSpace(l.Href))
	}
	return fi
}

// rssPath returns the RSS feed persistence file.
func rssPath(cfgDir string) string { return filepath.Join(cfgDir, "rss.json") }

// loadRSSFeeds reads persisted feeds from the config directory.
func loadRSSFeeds(cfgDir string) []rssFeed {
	if cfgDir == "" {
		return nil
	}
	data, err := os.ReadFile(rssPath(cfgDir))
	if err != nil {
		return nil
	}
	var feeds []rssFeed
	if err := json.Unmarshal(data, &feeds); err != nil {
		return nil
	}
	return feeds
}

func (s *Server) saveRSSFeeds() {
	if s.cfgDir == "" {
		return
	}
	data, err := json.MarshalIndent(s.feeds, "", "  ")
	if err != nil {
		return
	}
	tmp := rssPath(s.cfgDir) + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return
	}
	_ = os.Rename(tmp, rssPath(s.cfgDir))
}

func (s *Server) findFeed(id string) *rssFeed {
	for i := range s.feeds {
		if s.feeds[i].ID == id {
			return &s.feeds[i]
		}
	}
	return nil
}

// rssPollLoop periodically fetches every enabled feed.
func (s *Server) rssPollLoop() {
	go func() {
		for {
			time.Sleep(60 * time.Second)
			now := time.Now()
			s.rssMu.Lock()
			fc := append([]rssFeed(nil), s.feeds...)
			s.rssMu.Unlock()
			for i := range fc {
				f := &fc[i]
				if !f.Enabled {
					continue
				}
				iv := f.Interval
				if iv <= 0 {
					iv = rssDefaultInt
				}
				if now.Sub(f.LastPoll) < time.Duration(iv)*time.Minute {
					continue
				}
				s.pollFeed(f)
			}
		}
	}()
}

// pollFeed fetches a feed and adds torrents for new matching items.
func (s *Server) pollFeed(f *rssFeed) error {
	client := &http.Client{Timeout: 45 * time.Second}
	resp, err := client.Get(f.URL)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("fetch returned %s", resp.Status)
	}

	var doc rssXMLDoc
	if err := xml.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	var items []feedItem
	for _, it := range doc.Channel.Items {
		items = append(items, itemFromRSS(it))
	}
	for _, it := range doc.RootItems {
		items = append(items, itemFromRSS(it))
	}
	for _, it := range doc.Atoms {
		items = append(items, itemFromAtom(it))
	}

	now := time.Now()
	added := 0
	var matched []rssItem
	s.rssMu.Lock()
	live := s.findFeed(f.ID)
	if live == nil {
		s.rssMu.Unlock()
		return fmt.Errorf("feed removed")
	}
	seen := make(map[string]bool, len(live.Seen))
	for _, g := range live.Seen {
		seen[g] = true
	}
	s.rssMu.Unlock()

	for _, it := range items {
		guid := it.GUID
		if guid == "" && len(it.Srcs) > 0 {
			guid = it.Srcs[0]
		}
		if guid == "" {
			guid = it.Title
		}
		if guid == "" {
			continue
		}
		if seen[guid] {
			continue
		}
		src, size := rssSource(it, it.Title)
		if src == "" {
			continue
		}
		if !rssMatches(f, it.Title) {
			s.recordSeen(f.ID, guid)
			seen[guid] = true
			continue
		}
		if f.SizeMax > 0 && size > f.SizeMax {
			s.recordSeen(f.ID, guid)
			seen[guid] = true
			continue
		}
		s.recordSeen(f.ID, guid)
		seen[guid] = true
		go s.rssAdd(f, src, size)
		matched = append(matched, rssItem{GUID: guid, Title: it.Title, Link: src, Size: size, AddedAt: now})
		added++
		if len(matched) >= rssMaxRecent {
			break
		}
	}

	s.rssMu.Lock()
	f.LastPoll = now
	if l := s.findFeed(f.ID); l != nil {
		l.Recent = append(matched, l.Recent...)
		if len(l.Recent) > rssMaxRecent {
			l.Recent = l.Recent[:rssMaxRecent]
		}
	}
	s.rssMu.Unlock()
	s.saveRSSFeeds()
	s.engine.Logf("rss %q: %d new item(s) matched", f.Name, added)
	return nil
}

// rssAdd adds a torrent from a feed item and persists it.
func (s *Server) rssAdd(f *rssFeed, src string, size int64) {
	_, saveDir, err := s.resolveDir(f.Category)
	if err != nil {
		saveDir = s.saveDir
	}
	t, err := s.engineAdd(src, saveDir)
	if err != nil {
		s.engine.Logf("rss add failed (%s...): %v", src[:min(len(src), 80)], err)
		return
	}
	cat := f.Category
	if cat == "" && t.MetaInfo != nil {
		cat = torrente.Categorize(t.Name, t.MetaInfo.Info.Files)
	}
	t.SetCategory(cat)
	s.persistRecord(torrentRecord{Kind: rssKind(src), ID: t.ID, Source: src, Category: cat})
	s.engine.Logf("rss added %q", t.Name)
}

// rssKind returns the persistence kind for an RSS source.
func rssKind(src string) string {
	if strings.HasPrefix(src, "magnet:") {
		return "magnet"
	}
	return "url"
}

// rssMatches applies keyword/blacklist title filters.
func rssMatches(f *rssFeed, title string) bool {
	low := strings.ToLower(title)
	for _, kw := range f.Keywords {
		if kw != "" && strings.Contains(low, strings.ToLower(kw)) {
			return true
		}
	}
	if len(f.Keywords) > 0 {
		for _, kw := range f.Keywords {
			if kw != "" {
				return false
			}
		}
	}
	for _, bl := range f.Blacklist {
		if bl != "" && strings.Contains(low, strings.ToLower(bl)) {
			return false
		}
	}
	return true
}

// rssSource picks a usable source link from a normalized feed item.
func rssSource(it feedItem, title string) (string, int64) {
	cands := it.Srcs
	size := it.Size
	for _, c := range cands {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if strings.HasPrefix(c, "magnet:") {
			if size <= 0 {
				if xl := magnetSize(c); xl > 0 {
					size = xl
				}
			}
			return c, size
		}
	}
	for _, c := range cands {
		c = strings.TrimSpace(c)
		if strings.HasPrefix(c, "http://") || strings.HasPrefix(c, "https://") {
			if strings.HasSuffix(strings.ToLower(c), ".torrent") {
				return c, size
			}
		}
	}
	// a bare 40-char infohash yields a magnet link
	for _, c := range cands {
		c = strings.TrimSpace(c)
		if len(c) == 40 && isHex(c) {
			return "magnet:?xt=urn:btih:" + c + "&dn=" + url.QueryEscape(title), size
		}
	}
	return "", size
}

// cleanList trims entries and drops empty strings.
func cleanList(in []string) []string {
	var out []string
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// shortID returns a random lowercase hex id of n chars.
func shortID(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())[:n]
	}
	s := hex.EncodeToString(b)
	if len(s) > n {
		s = s[:n]
	}
	return s
}

// feedNameFromURL derives a display name from a feed URL's host.
func feedNameFromURL(u string) string {
	if parsed, err := url.Parse(u); err == nil && parsed.Host != "" {
		h := parsed.Host
		if i := strings.IndexByte(h, ':'); i >= 0 {
			h = h[:i]
		}
		return h
	}
	return "RSS feed"
}

func magnetSize(m string) int64 {
	low := strings.ToLower(m)
	i := strings.Index(low, "xl=")
	if i < 0 {
		return 0
	}
	rest := m[i+3:]
	if j := strings.IndexAny(rest, "&"); j >= 0 {
		rest = rest[:j]
	}
	var n int64
	if _, err := fmt.Sscanf(rest, "%d", &n); err == nil {
		return n
	}
	return 0
}

func isHex(s string) bool {
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// recordSeen marks a GUID as processed, capped.
func (s *Server) recordSeen(id, guid string) {
	s.rssMu.Lock()
	defer s.rssMu.Unlock()
	f := s.findFeed(id)
	if f == nil {
		return
	}
	for _, g := range f.Seen {
		if g == guid {
			return
		}
	}
	f.Seen = append(f.Seen, guid)
	if len(f.Seen) > rssMaxSeen {
		f.Seen = f.Seen[len(f.Seen)-rssMaxSeen:]
	}
}

// ---------- HTTP API ----------

type rssFeedView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Keywords  []string  `json:"keywords"`
	Blacklist []string  `json:"blacklist"`
	Category  string    `json:"category"`
	Interval  int       `json:"interval"`
	SizeMax   int64     `json:"size_max"`
	Enabled   bool      `json:"enabled"`
	Recent    []rssItem `json:"recent"`
	LastPoll  time.Time `json:"last_poll"`
}

func rssView(f *rssFeed) rssFeedView {
	return rssFeedView{
		ID: f.ID, Name: f.Name, URL: f.URL, Keywords: f.Keywords,
		Blacklist: f.Blacklist, Category: f.Category, Interval: f.Interval,
		SizeMax: f.SizeMax, Enabled: f.Enabled, Recent: f.Recent, LastPoll: f.LastPoll,
	}
}

func (s *Server) listRSS(w http.ResponseWriter, r *http.Request) {
	s.rssMu.Lock()
	feeds := append([]rssFeed(nil), s.feeds...)
	s.rssMu.Unlock()
	out := make([]rssFeedView, 0, len(feeds))
	for i := range feeds {
		out = append(out, rssView(&feeds[i]))
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	writeJSON(w, http.StatusOK, out)
}

type rssCreateReq struct {
	Name      string   `json:"name"`
	URL       string   `json:"url"`
	Keywords  []string `json:"keywords"`
	Blacklist []string `json:"blacklist"`
	Category  string   `json:"category"`
	Interval  int      `json:"interval"`
	SizeMax   int64    `json:"size_max"`
	Enabled   *bool    `json:"enabled"`
}

func (s *Server) createRSS(w http.ResponseWriter, r *http.Request) {
	var req rssCreateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	u := strings.TrimSpace(req.URL)
	if u == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty feed url"})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = feedNameFromURL(u)
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	f := rssFeed{
		ID: shortID(8), Name: name, URL: u,
		Keywords: cleanList(req.Keywords), Blacklist: cleanList(req.Blacklist),
		Category: strings.TrimSpace(req.Category), Interval: req.Interval,
		SizeMax: req.SizeMax, Enabled: enabled,
	}
	s.rssMu.Lock()
	s.feeds = append(s.feeds, f)
	s.rssMu.Unlock()
	s.saveRSSFeeds()
	// kick off an immediate poll in the background
	if enabled {
		fc := f
		go s.pollFeed(&fc)
	}
	writeJSON(w, http.StatusOK, rssView(&f))
}

func (s *Server) deleteRSS(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.rssMu.Lock()
	idx := -1
	for i := range s.feeds {
		if s.feeds[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.rssMu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "feed not found"})
		return
	}
	s.feeds = append(s.feeds[:idx], s.feeds[idx+1:]...)
	s.rssMu.Unlock()
	s.saveRSSFeeds()
	writeJSON(w, http.StatusOK, map[string]string{"ok": "deleted"})
}

func (s *Server) pollRSS(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.rssMu.Lock()
	f := s.findFeed(id)
	s.rssMu.Unlock()
	if f == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "feed not found"})
		return
	}
	if err := s.pollFeed(f); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	s.rssMu.Lock()
	v := rssView(s.findFeed(id))
	s.rssMu.Unlock()
	writeJSON(w, http.StatusOK, v)
}