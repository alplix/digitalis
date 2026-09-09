// Package web implements the Digitalis web UI and JSON API.
package web

import (
	"bytes"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alplix/digitalis/dl"
	"github.com/alplix/digitalis/metainfo"
	"github.com/alplix/digitalis/torrente"
)

//go:embed templates
var templatesFS embed.FS

// Server serves the web dashboard and the JSON API.
type Server struct {
	engine  *torrente.Engine
	saveDir string
	hub     *wsHub

	cfgDir    string
	recordsMu sync.Mutex
	records   []torrentRecord

	rssMu      sync.Mutex
	feeds      []rssFeed
	rssSpawned bool

	dl *dl.Manager
}

// NewServer creates a web server around an engine. cfgDir is the persistent
// config directory used to restore torrents across restarts ("" disables).
func NewServer(engine *torrente.Engine, saveDir, cfgDir string) *Server {
	s := &Server{
		engine:  engine,
		saveDir: saveDir,
		hub:     newWSHub(),
		cfgDir:  cfgDir,
		dl:      dl.NewManager(dlmgrPath(cfgDir)),
	}
	go s.broadcastLoop()

	// Streaming notifications (download complete, magnet metadata resolved).
	engine.OnNotice = func(kind, id, name string) {
		s.broadcastNotice(kind, id, name)
		switch kind {
		case torrente.NoticeMetadata:
			s.autoFileMagnet(id)
			s.applyPendingSkippedFiles(id)
			s.syncSmartCategory(id)
		case torrente.NoticeRatio:
			// a ratio-autoremoved torrent must not come back on restart
			s.dropRecord(id)
			s.sendTelegram(fmt.Sprintf("✅ %s reached its share goal.", name))
			s.sendWebhook(kind, name, fmt.Sprintf("%s reached its share goal.", name))
		case torrente.NoticeComplete:
			s.sendTelegram(fmt.Sprintf("⬇️ %s finished downloading.", name))
			s.sendWebhook(kind, name, fmt.Sprintf("%s finished downloading.", name))
		case torrente.NoticeDiskGuard:
			s.sendTelegram(fmt.Sprintf("⚠️ Disk space low — downloads paused (min %s free).", name))
			s.sendWebhook(kind, name, fmt.Sprintf("Disk space low — downloads paused (min %s free).", name))
		}
	}

	// Restore persisted torrents.
	if cfgDir != "" {
		s.records = loadRecords(cfgDir)
		s.restoreRecords(s.records)
	}

	// Load RSS subscriptions and start the poller.
	if cfgDir != "" {
		s.feeds = loadRSSFeeds(cfgDir)
	}
	if !s.rssSpawned {
		s.rssSpawned = true
		s.rssPollLoop()
	}
	return s
}

// torrentView is the JSON representation of a torrent.
type torrentView struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	InfoHash       string    `json:"info_hash"`
	State          string    `json:"state"`
	Progress       float64   `json:"progress"`
	Downloaded     int64     `json:"downloaded"`
	Uploaded       int64     `json:"uploaded"`
	Size           int64     `json:"size"`
	DownloadSpeed  int64     `json:"download_speed"`
	UploadSpeed    int64     `json:"upload_speed"`
	Seeders        int       `json:"seeders"`
	Leechers       int       `json:"leechers"`
	PeersConnected int       `json:"peers_connected"`
	PiecesHave     int       `json:"pieces_have"`
	PiecesTotal    int       `json:"pieces_total"`
	Ratio          float64   `json:"ratio"`
	RatioTarget    float64   `json:"ratio_target"`
	SeededTo       int       `json:"seeded_to"`
	SeededFirst    time.Time `json:"seeded_first"`
	LastSeen       time.Time `json:"last_seen"`
	WorkingTrackers int      `json:"working_trackers"`
	FailedTrackers  int      `json:"failed_trackers"`
	Trackers       []trackerView `json:"trackers"`
	Category       string        `json:"category"`
	SaveDir        string        `json:"save_dir"`
	AddedAt        time.Time     `json:"added_at"`
	Comment        string        `json:"comment,omitempty"`
	CreatedBy      string        `json:"created_by,omitempty"`
	DownloadLimit  int64         `json:"download_limit"`
	SeedDays       int64         `json:"seed_days"`
	Sequential     bool          `json:"sequential"`
	GuardPaused    bool          `json:"guard_paused"`
	Queued         bool          `json:"queued"`
}

type trackerView struct {
	URL          string    `json:"url"`
	Working      bool      `json:"working"`
	Announces    int       `json:"announces"`
	Successes    int       `json:"successes"`
	Failures     int       `json:"failures"`
	LastSuccess  time.Time `json:"last_success"`
	LastFailure  time.Time `json:"last_failure"`
	LastError    string    `json:"last_error,omitempty"`
	LastSeeders  int       `json:"last_seeders"`
	LastLeechers int       `json:"last_leechers"`
	LastPeers    int       `json:"last_peers"`
}

func trackerViews(ts []*torrente.TrackerStat) []trackerView {
	out := make([]trackerView, 0, len(ts))
	for _, x := range ts {
		out = append(out, trackerView{
			URL: x.URL, Working: x.Working, Announces: x.Announces,
			Successes: x.Successes, Failures: x.Failures,
			LastSuccess: x.LastSuccess, LastFailure: x.LastFailure,
			LastError: x.LastError, LastSeeders: x.LastSeeders,
			LastLeechers: x.LastLeechers, LastPeers: x.LastPeers,
		})
	}
	return out
}

func (s *Server) snapshot(t *torrente.Torrent) torrentView {
	v := torrentView{
		ID:             t.ID,
		Name:           t.Name,
		InfoHash:       t.InfoHash,
		State:          string(t.State),
		Progress:       t.Progress(),
		Downloaded:     t.Downloaded,
		Uploaded:       t.Uploaded,
		Size:           t.Size,
		DownloadSpeed:  t.DownloadSpeed,
		UploadSpeed:    t.UploadSpeed,
		Seeders:        t.Seeders,
		Leechers:       t.Leechers,
		PeersConnected: t.PeersConnected,
		PiecesHave:     t.PiecesHave,
		PiecesTotal:    t.PiecesTotal,
		Ratio:          t.Ratio(),
		RatioTarget:    s.engine.TorrentRatioTarget(t.ID),
		SeededTo:       t.SeededTo,
		SeededFirst:    t.SeededFirst,
		LastSeen:       t.LastSeen,
		WorkingTrackers: t.WorkingTrackers,
		FailedTrackers:  t.FailedTrackers,
		Trackers:       trackerViews(t.Trackers),
		Category:       t.Category,
		SaveDir:        t.SaveDir,
		AddedAt:        t.AddedAt,
		Comment:        t.MetaInfoComment(),
		CreatedBy:      t.MetaInfoCreatedBy(),
		DownloadLimit:  s.engine.TorrentDownloadLimit(t.ID),
		SeedDays:       s.engine.TorrentSeedDays(t.ID),
		Sequential:     s.engine.TorrentSequential(t.ID),
		GuardPaused:    t.IsGuardPaused(),
		Queued:         t.IsQueued(),
	}
	return v
}

func (s *Server) getTorrentDetail(w http.ResponseWriter, r *http.Request) {
	d, err := s.engine.Detail(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// Handler returns the http.Handler for the server.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /", s.index)

	mux.HandleFunc("GET /logo.svg", s.serveTpl("logo.svg", "image/svg+xml"))
	mux.HandleFunc("GET /favicon.svg", s.serveTpl("logo.svg", "image/svg+xml"))
	mux.HandleFunc("GET /manifest.webmanifest", s.serveTpl("manifest.webmanifest", "application/manifest+json"))
	mux.HandleFunc("GET /sw.js", s.serveTpl("sw.js", "text/javascript"))
	mux.HandleFunc("GET /qrcode.min.js", s.serveTpl("qrcode.min.js", "text/javascript"))

	mux.HandleFunc("GET /api/torrents", s.listTorrents)
	mux.HandleFunc("GET /api/torrents/{id}", s.getTorrentDetail)
	mux.HandleFunc("GET /api/torrents/{id}/stream/{idx}", s.streamFile)
	mux.HandleFunc("POST /api/torrents", s.addTorrent)
	mux.HandleFunc("PATCH /api/torrents/{id}", s.patchTorrent)
	mux.HandleFunc("POST /api/torrents/{id}/pause", s.pause)
	mux.HandleFunc("POST /api/torrents/{id}/resume", s.resume)
	mux.HandleFunc("POST /api/torrents/{id}/delete", s.delete)
	mux.HandleFunc("POST /api/torrents/{id}/move", s.moveTorrent)
	mux.HandleFunc("POST /api/torrents/{id}/announce", s.reannounce)
	mux.HandleFunc("POST /api/trackers/announce", s.reannounceAll)
	mux.HandleFunc("GET /api/storage", s.storageInfo)
	mux.HandleFunc("GET /api/disks", s.listDisks)
	mux.HandleFunc("POST /api/disks", s.addDisk)
	mux.HandleFunc("DELETE /api/disks", s.removeDisk)
	mux.HandleFunc("POST /api/disks/trash/empty", s.emptyDiskTrash)
	mux.HandleFunc("POST /api/files/share", s.createShare)
	mux.HandleFunc("GET /s/{token}", s.serveShare)
	mux.HandleFunc("POST /api/telegram/test", s.testTelegram)
	mux.HandleFunc("POST /api/webhook/test", s.testWebhook)
	mux.HandleFunc("GET /api/torrents/{id}/peers", s.listPeers)
	mux.HandleFunc("GET /api/torrents/{id}/export", s.exportTorrent)
	mux.HandleFunc("GET /api/torrents/{id}/skipped", s.getSkipped)
	mux.HandleFunc("POST /api/torrents/{id}/skipped", s.setSkipped)
	mux.HandleFunc("GET /api/portcheck", s.portCheck)
	mux.HandleFunc("GET /api/backup", s.exportBackup)
	mux.HandleFunc("POST /api/backup", s.importBackup)
	mux.HandleFunc("GET /api/disk-history", s.diskHistory)
	mux.HandleFunc("GET /api/dl", s.dlList)
	mux.HandleFunc("POST /api/dl", s.dlStart)
	mux.HandleFunc("DELETE /api/dl/{id}", s.dlRemove)
	mux.HandleFunc("POST /api/dl/{id}/stop", s.dlStop)
	mux.HandleFunc("GET /api/dl/{id}/ram", s.dlRAMPreview)
	mux.HandleFunc("GET /api/dl/repos", s.dlRepos)
	mux.HandleFunc("GET /api/dl/repos/browse", s.dlRepoBrowse)
	mux.HandleFunc("GET /api/dl/history", s.dlHistory)
	mux.HandleFunc("DELETE /api/dl/history", s.dlHistoryClear)
	mux.HandleFunc("GET /api/dl/history.csv", s.dlHistoryCSV)
	mux.HandleFunc("POST /api/bulk", s.bulkOp)
	mux.HandleFunc("GET /api/files", s.listFiles)
	mux.HandleFunc("GET /api/files/download", s.downloadFile)
	mux.HandleFunc("POST /api/files/upload", s.uploadFile)
	mux.HandleFunc("POST /api/files/mkdir", s.makeDir)
	mux.HandleFunc("POST /api/files/move", s.moveFile)
	mux.HandleFunc("POST /api/files/delete", s.deleteFile)

	mux.HandleFunc("GET /ws", s.wsHandler)

	mux.HandleFunc("GET /api/categories", s.listCategories)
	mux.HandleFunc("POST /api/categories", s.createCategory)
	mux.HandleFunc("DELETE /api/categories", s.deleteCategory)

	mux.HandleFunc("GET /api/stats", s.getStats)
	mux.HandleFunc("GET /api/settings", s.getSettings)
	mux.HandleFunc("POST /api/settings", s.postSettings)
	mux.HandleFunc("GET /api/trackers", s.listGlobalTrackers)
	mux.HandleFunc("POST /api/torrents/{id}/trackers", s.addTorrentTracker)
	mux.HandleFunc("DELETE /api/torrents/{id}/trackers", s.removeTorrentTracker)

	mux.HandleFunc("GET /api/rss", s.listRSS)
	mux.HandleFunc("POST /api/rss", s.createRSS)
	mux.HandleFunc("DELETE /api/rss/{id}", s.deleteRSS)
	mux.HandleFunc("POST /api/rss/{id}/poll", s.pollRSS)
	mux.HandleFunc("POST /api/rss/poll", s.pollAllRSS)

	mux.HandleFunc("POST /api/auth", s.checkAuth)
	mux.HandleFunc("POST /api/settings/auth", s.postAuth)

	return s.withLogging(s.withAuth(mux))
}

// authPublic routes never require the access token: the shell page and its
// static assets carry no secrets, /api/auth verifies the token itself, and
// /s/ share links are deliberate handouts to other people/devices.
func authPublic(p string) bool {
	if strings.HasPrefix(p, "/s/") {
		return true
	}
	switch p {
	case "/", "/logo.svg", "/favicon.svg", "/manifest.webmanifest", "/sw.js", "/qrcode.min.js", "/api/auth":
		return true
	}
	return false
}

// withAuth guards every non-public route with the configured access token.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := s.engine.ServerToken()
		if tok == "" || authPublic(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if s.tokenMatches(r, tok) {
			next.ServeHTTP(w, r)
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
	})
}

// tokenMatches checks the request credentials (Bearer header, X-Auth-Token
// header, or ?token= query — the latter lets media streams and WebSocket
// connections authenticate) against the configured token in constant time.
func (s *Server) tokenMatches(r *http.Request, tok string) bool {
	var given string
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		given = strings.TrimSpace(a[len("Bearer "):])
	}
	if given == "" {
		if x := r.Header.Get("X-Auth-Token"); x != "" {
			given = x
		}
	}
	if given == "" {
		given = r.URL.Query().Get("token")
	}
	if given == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(given), []byte(tok)) == 1
}

// checkAuth validates a client-provided token. Exempt from the middleware via
// authPublic so it can bootstrap the login screen.
func (s *Server) checkAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	tok := s.engine.ServerToken()
	if tok == "" {
		writeJSON(w, http.StatusOK, map[string]string{"ok": "open"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.Token), []byte(tok)) == 1 {
		writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
		return
	}
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token"})
}

// postAuth sets, changes or clears the access token. It sits behind the
// middleware, so reaching it already proves knowledge of the current token
// (or an open server, in which case the first token can be bootstrapped).
func (s *Server) postAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
		Clear bool   `json:"clear"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Clear {
		if err := s.engine.ClearServerToken(); err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		s.engine.Logf("server access protection removed")
		writeJSON(w, http.StatusOK, map[string]string{"ok": "cleared"})
		return
	}
	req.Token = strings.TrimSpace(req.Token)
	if req.Token == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "token required"})
		return
	}
	if len(req.Token) < 6 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "token too short"})
		return
	}
	if err := s.engine.SetServerToken(req.Token); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	s.engine.Logf("server access protection enabled")
	writeJSON(w, http.StatusOK, map[string]string{"ok": "set"})
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		s.engine.Logf("web %s %s (%s)", r.Method, r.URL.Path, time.Since(start))
	})
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	html, err := templatesFS.ReadFile("templates/index.html")
	if err != nil {
		http.Error(w, "template missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(html)
}

// serveTpl returns a handler that serves an embedded template asset.
func (s *Server) serveTpl(name, ctype string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := templatesFS.ReadFile("templates/" + name)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", ctype)
		if name == "sw.js" || name == "manifest.webmanifest" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		w.Write(data)
	}
}

func (s *Server) getStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.engine.StatsSnapshot())
}

// syncSmartCategory persists the auto-assigned category once a magnet's
// metadata arrives, so the folder choice survives a restart.
func (s *Server) syncSmartCategory(id string) {
	t, ok := s.engine.GetTorrent(id)
	if !ok {
		return
	}
	s.recordsMu.Lock()
	defer s.recordsMu.Unlock()
	changed := false
	for i := range s.records {
		if s.records[i].ID == id && s.records[i].Category != t.Category {
			s.records[i].Category = t.Category
			changed = true
		}
	}
	if changed && s.cfgDir != "" {
		_ = saveRecords(s.cfgDir, s.records)
	}
}

// settingsView is the settings JSON used by the UI.
type settingsView struct {
	BaseDir          string   `json:"base_dir"`
	Disks            []string `json:"disks"`
	UploadLimit      int64   `json:"upload_limit"`
	DownloadLimit    int64   `json:"download_limit"`
	DailyUploadLimit int64   `json:"daily_upload_limit"`
	NightMode        bool    `json:"night_mode"`
	NightStart       string  `json:"night_start"`
	NightEnd         string  `json:"night_end"`
	NightUpload      int64   `json:"night_upload"`
	NightDownload    int64   `json:"night_download"`
	NightPause       bool    `json:"night_pause"`
	RatioTarget      float64 `json:"ratio_target"`
	RatioStop        bool    `json:"ratio_stop"`
	RatioRemove      bool    `json:"ratio_remove"`
	SeedDays         int64   `json:"seed_days"`
	DiskGuard        bool    `json:"disk_guard"`
	DiskGuardMinGB   int64   `json:"disk_guard_min_gb"`
	MaxActiveDl      int     `json:"max_active_downloads"`
	TelegramEnabled  bool    `json:"telegram_enabled"`
	TelegramTokenSet bool    `json:"telegram_token_set"`
	TelegramChat     string  `json:"telegram_chat"`
	WebhookEnabled   bool    `json:"webhook_enabled"`
	WebhookURL       string  `json:"webhook_url"`
	TrashDays        int     `json:"trash_days"`
	CatRules         []torrente.CatRule `json:"cat_rules"`
	ServerTokenSet   bool    `json:"server_token_set"`
	PeerPort         int     `json:"peer_port"`
	Version          string  `json:"version"`
}

func (s *Server) settingsView() settingsView {
	v := s.engine.View()
	return settingsView{
		BaseDir:          v.BaseDir,
		Disks:            s.engine.Disks(),
		UploadLimit:      v.UploadLimit,
		DownloadLimit:    v.DownloadLimit,
		DailyUploadLimit: v.DailyUploadLimit,
		NightMode:        v.NightMode,
		NightStart:       v.NightStart,
		NightEnd:         v.NightEnd,
		NightUpload:      v.NightUpload,
		NightDownload:    v.NightDownload,
		NightPause:       v.NightPause,
		RatioTarget:      v.RatioTarget,
		RatioStop:        v.RatioStop,
		RatioRemove:      v.RatioRemove,
		SeedDays:         v.SeedDays,
		DiskGuard:        v.DiskGuard,
		DiskGuardMinGB:   v.DiskGuardMinGB,
		MaxActiveDl:      v.MaxActiveDownloads,
		TelegramEnabled:  v.TelegramEnabled,
		TelegramTokenSet: v.TelegramToken != "",
		TelegramChat:     v.TelegramChat,
		WebhookEnabled:   v.WebhookEnabled,
		WebhookURL:       v.WebhookURL,
		TrashDays:        v.TrashDays,
		CatRules:         catRuleList(v.CatRules),
		ServerTokenSet:   v.ServerToken != "",
		PeerPort:         s.engine.Port(),
		Version:          "digitalis 0.9",
	}
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.settingsView())
}

// catRuleList always returns a non-nil slice so the JSON is an array.
func catRuleList(rules []torrente.CatRule) []torrente.CatRule {
	if rules == nil {
		return []torrente.CatRule{}
	}
	return rules
}

// autoCategory picks the folder for a torrent: rule-based patterns win, then
// the built-in heuristic classifier.
func (s *Server) autoCategory(name string, files []metainfo.File) string {
	if cat := s.engine.MatchCatRule(name); cat != "" {
		return cat
	}
	return torrente.Categorize(name, files)
}

// autoFileMagnet assigns a rule-based/heuristic folder to a magnet torrent as
// soon as its metadata arrives, so magnets land in the right library folder
// without user action.
func (s *Server) autoFileMagnet(id string) {
	_ = s.engine.AutoFileMagnet(id, s.autoCategory)
}

func (s *Server) postSettings(w http.ResponseWriter, r *http.Request) {
	var patch torrente.Settings
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	cur, err := s.engine.UpdateSettings(patch)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	if cur.BaseDir != "" && filepath.Clean(cur.BaseDir) != filepath.Clean(s.saveDir) {
		if err := os.MkdirAll(cur.BaseDir, 0755); err == nil {
			s.saveDir = cur.BaseDir
		}
	}
	writeJSON(w, http.StatusOK, s.settingsView())
}

// globalTrackerView aggregates a tracker URL across all torrents.
type globalTrackerView struct {
	URL          string    `json:"url"`
	Working      bool      `json:"working"`
	Torrents     int       `json:"torrents"`
	Announces    int     `json:"announces"`
	Successes    int     `json:"successes"`
	Failures     int     `json:"failures"`
	LastSuccess  time.Time `json:"last_success"`
	LastFailure  time.Time `json:"last_failure"`
	LastSeeders  int       `json:"last_seeders"`
	LastLeechers int       `json:"last_leechers"`
}

func (s *Server) listGlobalTrackers(w http.ResponseWriter, r *http.Request) {
	m := make(map[string]*globalTrackerView)
	order := make([]string, 0)
	for _, t := range s.engine.Torrents() {
		for _, ts := range t.Trackers {
			g := m[ts.URL]
			if g == nil {
				g = &globalTrackerView{URL: ts.URL}
				m[ts.URL] = g
				order = append(order, ts.URL)
			}
			g.Torrents++
			g.Announces += ts.Announces
			g.Successes += ts.Successes
			g.Failures += ts.Failures
			if ts.Working {
				g.Working = true
			}
			if ts.LastSuccess.After(g.LastSuccess) {
				g.LastSuccess = ts.LastSuccess
			}
			if ts.LastFailure.After(g.LastFailure) {
				g.LastFailure = ts.LastFailure
			}
			if ts.LastSeeders > g.LastSeeders {
				g.LastSeeders = ts.LastSeeders
			}
			if ts.LastLeechers > g.LastLeechers {
				g.LastLeechers = ts.LastLeechers
			}
		}
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := m[order[i]], m[order[j]]
		if a.Announces != b.Announces {
			return a.Announces > b.Announces
		}
		return order[i] < order[j]
	})
	out := make([]globalTrackerView, 0, len(order))
	for _, u := range order {
		out = append(out, *m[u])
	}
	writeJSON(w, http.StatusOK, out)
}

// trackerReq carries a tracker URL for torrent add/remove.
type trackerReq struct {
	URL string `json:"url"`
}

func (s *Server) addTorrentTracker(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req trackerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	u := strings.TrimSpace(req.URL)
	if u == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty tracker url"})
		return
	}
	if err := s.engine.AddTracker(id, u); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	s.persistTrackerChanges(id)
	writeJSON(w, http.StatusOK, map[string]string{"ok": "added"})
}

func (s *Server) removeTorrentTracker(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req trackerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	u := strings.TrimSpace(req.URL)
	if u == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty tracker url"})
		return
	}
	if err := s.engine.RemoveTracker(id, u); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	s.persistTrackerChanges(id)
	writeJSON(w, http.StatusOK, map[string]string{"ok": "removed"})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (s *Server) listTorrents(w http.ResponseWriter, r *http.Request) {
	ts := s.engine.Torrents()
	out := make([]torrentView, 0, len(ts))
	for _, t := range ts {
		out = append(out, s.snapshot(t))
	}
	writeJSON(w, http.StatusOK, out)
}

// addRequest accepts {"source": "..."} where source is a magnet URI, a .torrent
// URL, or raw base64 of a torrent file. "dir" is an optional category folder
// (relative path such as "unix/linux/debian") created under the base save dir,
// and "disk" optionally selects a storage root other than the base dir.
type addRequest struct {
	Source string `json:"source"`
	Dir    string `json:"dir"`
	Disk   string `json:"disk"`
}

// captureResponse records a handler's response so addTorrent can recurse for
// each line of a bulk multi-line source.
type captureResponse struct {
	header http.Header
	status int
	buf    bytes.Buffer
}

func (c *captureResponse) Header() http.Header {
	if c.header == nil {
		c.header = make(http.Header)
	}
	return c.header
}
func (c *captureResponse) Write(p []byte) (int, error) { return c.buf.Write(p) }
func (c *captureResponse) WriteHeader(status int)      { c.status = status }

// resolveDir resolves a user-supplied category path into an absolute directory
// under the base save dir, creating nested folders as needed. It returns the
// cleaned category path ("" for the base dir itself) and the absolute path.
func (s *Server) resolveDir(dir string) (string, string, error) {
	return resolveDirOn(s.saveDir, dir)
}

// resolveDiskDir resolves a category path against an explicit storage root.
// diskPath is relative to path of a registered root.
func (s *Server) resolveDiskDir(diskPath, dir string) (string, string, error) {
	root := s.saveDir
	if diskPath != "" {
		for _, d := range s.engine.Disks() {
			if filepath.Clean(d) == filepath.Clean(diskPath) {
				root = filepath.Clean(d)
				break
			}
		}
	}
	return resolveDirOn(root, dir)
}

func resolveDirOn(root, dir string) (string, string, error) {
	d := strings.TrimSpace(dir)
	if d == "" || d == "." || d == "/" {
		return "", root, nil
	}
	if strings.HasPrefix(d, "/") || filepath.IsAbs(d) {
		return "", "", errors.New("category must be a relative path")
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(d)))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", "", errors.New("invalid category path")
	}
	full := filepath.Join(root, filepath.FromSlash(clean))
	if err := os.MkdirAll(full, 0755); err != nil {
		return "", "", err
	}
	return clean, full, nil
}

func (s *Server) addTorrent(w http.ResponseWriter, r *http.Request) {
	var req addRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	src := strings.TrimSpace(req.Source)
	if src == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty source"})
		return
	}
	// Bulk tolerance: a source containing line breaks is split and each line
	// is added on its own, so a pasted multi-link blob never fails as a whole.
	if strings.ContainsAny(src, "\n\r") {
		lines := strings.FieldsFunc(src, func(rn rune) bool { return rn == '\n' || rn == '\r' })
		out := struct {
			Added  int      `json:"added"`
			Failed int      `json:"failed"`
			Errors []string `json:"errors,omitempty"`
		}{}
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			body, _ := json.Marshal(addRequest{Source: line, Dir: req.Dir, Disk: req.Disk})
			rec := &captureResponse{}
			r2 := r.Clone(r.Context())
			r2.Body = io.NopCloser(bytes.NewReader(body))
			r2.ContentLength = int64(len(body))
			r2.Header.Set("Content-Type", "application/json")
			s.addTorrent(rec, r2)
			if rec.status >= 200 && rec.status < 300 {
				out.Added++
			} else {
				out.Failed++
				out.Errors = append(out.Errors, strings.TrimSpace(rec.buf.String()))
			}
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	category, saveDir, err := s.resolveDiskDir(req.Disk, req.Dir)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	addResp := func(t *torrente.Torrent) {
		writeJSON(w, http.StatusOK, s.snapshot(t))
	}

	kind := "url"
	if strings.HasPrefix(src, "magnet:") {
		kind = "magnet"
	}
	persist := func(t *torrente.Torrent) {
		s.persistRecord(torrentRecord{Kind: kind, ID: t.ID, Source: src, Category: category, Disk: s.diskRootOf(t.SaveDir)})
	}

	if strings.HasPrefix(src, "magnet:") {
		m, err := metainfo.ParseMagnet(src)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if len(m.InfoHash) != 40 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "magnet must have a btih v1 hash"})
			return
		}
		t, err := s.engine.AddMagnet(m, saveDir)
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		t.SetCategory(category)
		persist(t)
		addResp(t)
		return
	}

	// assume http(s) .torrent URL
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Get(src)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to fetch torrent: " + err.Error()})
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "fetch returned " + resp.Status})
			return
		}
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		mi, err := metainfo.Parse(data)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a valid torrent: " + err.Error()})
			return
		}
		t, err := s.engine.AddTorrent(mi, saveDir)
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		if category == "" {
			category = s.autoCategory(t.Name, t.MetaInfo.Info.Files)
		}
		t.SetCategory(category)
		persist(t)
		addResp(t)
		return
	}

	// try raw bencode (base64/json encoded as string)
	if mi, err := metainfo.Parse([]byte(src)); err == nil {
		t, err := s.engine.AddTorrent(mi, saveDir)
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		if category == "" {
			category = s.autoCategory(t.Name, t.MetaInfo.Info.Files)
		}
		t.SetCategory(category)
		persist(t)
		addResp(t)
		return
	}

	writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported source (use magnet:, http(s):, or raw .torrent payload)"})
}

func (s *Server) pause(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.engine.Pause(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "paused"})
}

func (s *Server) resume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.engine.Resume(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "resumed"})
}

func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.engine.RemoveTorrentWithFiles(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	s.dropRecord(id)
	writeJSON(w, http.StatusOK, map[string]string{"ok": "deleted"})
}

// moveRequest relocates a torrent into a category folder, optionally on another
// storage root.
type moveRequest struct {
	Category string `json:"category"`
	Disk     string `json:"disk"`
}

func (s *Server) moveTorrent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req moveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	category, newDir, err := s.resolveDiskDir(req.Disk, req.Category)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.engine.MoveTorrent(id, newDir, category); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	t, ok := s.engine.GetTorrent(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "torrent not found"})
		return
	}
	s.recordsMu.Lock()
	for i := range s.records {
		if s.records[i].ID == id {
			s.records[i].Category = category
			s.records[i].Disk = s.diskRootOf(t.SaveDir)
			recs := append([]torrentRecord(nil), s.records...)
			s.recordsMu.Unlock()
			if s.cfgDir != "" {
				_ = saveRecords(s.cfgDir, recs)
			}
			writeJSON(w, http.StatusOK, s.snapshot(t))
			return
		}
	}
	s.recordsMu.Unlock()
	writeJSON(w, http.StatusOK, s.snapshot(t))
}

// categoryView is one node of the category tree.
type categoryView struct {
	Path     string `json:"path"`
	Name     string `json:"name"`
	Depth    int    `json:"depth"`
	Torrents int    `json:"torrents"`
}

func (s *Server) listCategories(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.categoryTree())
}

func (s *Server) categoryTree() []categoryView {
	// A torrent's own root folder (multi-file torrents create a folder with
	// the torrent name) must not appear as a category. Build the set of those
	// roots relative to the base save dir.
	torrentRoots := make(map[string]bool)
	for _, t := range s.engine.Torrents() {
		if t.Name == "" || t.SaveDir == "" {
			continue
		}
		rel, err := filepath.Rel(s.saveDir, filepath.Join(t.SaveDir, t.Name))
		if err == nil && rel != "." {
			torrentRoots[filepath.ToSlash(rel)] = true
		}
	}

	var dirs []string
	filepath.Walk(s.saveDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.IsDir() {
			return nil
		}
		if path == s.saveDir {
			return nil
		}
		rel, err := filepath.Rel(s.saveDir, path)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		for root := range torrentRoots {
			if relSlash == root || strings.HasPrefix(relSlash, root+"/") {
				return filepath.SkipDir
			}
		}
		dirs = append(dirs, relSlash)
		return nil
	})
	sort.Strings(dirs)

	counts := make(map[string]int)
	for _, t := range s.engine.Torrents() {
		if t.Category != "" {
			counts[t.Category]++
		}
	}

	out := make([]categoryView, 0, len(dirs))
	for _, d := range dirs {
		out = append(out, categoryView{
			Path:     d,
			Name:     filepath.Base(d),
			Depth:    strings.Count(d, "/"),
			Torrents: counts[d],
		})
	}
	return out
}

// categoryReq carries a category path for create/delete.
type categoryReq struct {
	Path string `json:"path"`
}

func (s *Server) createCategory(w http.ResponseWriter, r *http.Request) {
	var req categoryReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	_, _, err := s.resolveDir(req.Path)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.categoryTree())
}

func (s *Server) deleteCategory(w http.ResponseWriter, r *http.Request) {
	var req categoryReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	p := strings.TrimSpace(req.Path)
	if p == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty category"})
		return
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(p)))
	if clean == ".." || strings.HasPrefix(clean, "../") || clean == "." {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid category path"})
		return
	}
	full := filepath.Join(s.saveDir, filepath.FromSlash(clean))
	for _, t := range s.engine.Torrents() {
		if t.Category == clean {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "category not empty: torrents inside"})
			return
		}
	}
	if err := os.Remove(full); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.categoryTree())
}

// patchRequest carries the per-torrent settings accepted by PATCH.
type patchRequest struct {
	DownloadLimit *int64   `json:"download_limit"`
	RatioTarget   *float64 `json:"ratio_target"`
	SeedDays      *int64   `json:"seed_days"`
	Sequential    *bool    `json:"sequential"`
}

func (s *Server) patchTorrent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req patchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if _, ok := s.engine.GetTorrent(id); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "torrent not found"})
		return
	}
	if req.DownloadLimit != nil {
		if err := s.engine.SetTorrentDownloadLimit(id, *req.DownloadLimit); err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
	}
	if req.RatioTarget != nil {
		if err := s.engine.SetTorrentRatioTarget(id, *req.RatioTarget); err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
	}
	if req.SeedDays != nil {
		if err := s.engine.SetTorrentSeedDays(id, *req.SeedDays); err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
	}
	if req.Sequential != nil {
		if err := s.engine.SetTorrentSequential(id, *req.Sequential); err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
	}
	t, _ := s.engine.GetTorrent(id)
	writeJSON(w, http.StatusOK, s.snapshot(t))
}

// bulkRequest runs an operation across many torrents at once.
type bulkRequest struct {
	Action string   `json:"action"` // pause | resume | delete
	IDs    []string `json:"ids"`
}

func (s *Server) bulkOp(w http.ResponseWriter, r *http.Request) {
	var req bulkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	ok := 0
	var failed []string
	for _, id := range req.IDs {
		var err error
		switch req.Action {
		case "pause":
			err = s.engine.Pause(id)
		case "resume":
			err = s.engine.Resume(id)
		case "delete":
			err = s.engine.RemoveTorrentWithFiles(id)
			if err == nil {
				s.dropRecord(id)
			}
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown action"})
			return
		}
		if err != nil {
			failed = append(failed, id)
		} else {
			ok++
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": ok, "failed": failed})
}

// fileEntry is one entry of the file browser
// (defined in files.go).