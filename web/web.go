// Package web implements the Digitalis web UI and JSON API.
package web

import (
	"embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/alplix/digitalis/metainfo"
	"github.com/alplix/digitalis/torrente"
)

//go:embed templates
var templatesFS embed.FS

// Server serves the web dashboard and the JSON API.
type Server struct {
	engine  *torrente.Engine
	saveDir string
}

// NewServer creates a web server around an engine.
func NewServer(engine *torrente.Engine, saveDir string) *Server {
	return &Server{engine: engine, saveDir: saveDir}
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
	SeededTo       int       `json:"seeded_to"`
	SeededFirst    time.Time `json:"seeded_first"`
	LastSeen       time.Time `json:"last_seen"`
	WorkingTrackers int      `json:"working_trackers"`
	FailedTrackers  int      `json:"failed_trackers"`
	Trackers       []trackerView `json:"trackers"`
	Category       string        `json:"category"`
	SaveDir        string        `json:"save_dir"`
	AddedAt        time.Time     `json:"added_at"`
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
		SeededTo:       t.SeededTo,
		SeededFirst:    t.SeededFirst,
		LastSeen:       t.LastSeen,
		WorkingTrackers: t.WorkingTrackers,
		FailedTrackers:  t.FailedTrackers,
		Trackers:       trackerViews(t.Trackers),
		Category:       t.Category,
		SaveDir:        t.SaveDir,
		AddedAt:        t.AddedAt,
	}
	return v
}

// Handler returns the http.Handler for the server.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /", s.index)

	mux.HandleFunc("GET /api/torrents", s.listTorrents)
	mux.HandleFunc("POST /api/torrents", s.addTorrent)
	mux.HandleFunc("POST /api/torrents/{id}/pause", s.pause)
	mux.HandleFunc("POST /api/torrents/{id}/resume", s.resume)
	mux.HandleFunc("POST /api/torrents/{id}/delete", s.delete)
	mux.HandleFunc("POST /api/torrents/{id}/move", s.moveTorrent)

	mux.HandleFunc("GET /api/categories", s.listCategories)
	mux.HandleFunc("POST /api/categories", s.createCategory)
	mux.HandleFunc("DELETE /api/categories", s.deleteCategory)

	return s.withLogging(mux)
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
	w.Write(html)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
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
// (relative path such as "unix/linux/debian") created under the base save dir.
type addRequest struct {
	Source string `json:"source"`
	Dir    string `json:"dir"`
}

// resolveDir resolves a user-supplied category path into an absolute directory
// under the base save dir, creating nested folders as needed. It returns the
// cleaned category path ("" for the base dir itself) and the absolute path.
func (s *Server) resolveDir(dir string) (string, string, error) {
	d := strings.TrimSpace(dir)
	if d == "" || d == "." || d == "/" {
		return "", s.saveDir, nil
	}
	if strings.HasPrefix(d, "/") || filepath.IsAbs(d) {
		return "", "", errors.New("category must be a relative path")
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(d)))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", "", errors.New("invalid category path")
	}
	full := filepath.Join(s.saveDir, filepath.FromSlash(clean))
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
	category, saveDir, err := s.resolveDir(req.Dir)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	addResp := func(t *torrente.Torrent) {
		writeJSON(w, http.StatusOK, s.snapshot(t))
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
		t.SetCategory(category)
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
		t.SetCategory(category)
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
	if err := s.engine.RemoveTorrent(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "deleted"})
}

// moveRequest relocates a torrent into a category folder.
type moveRequest struct {
	Category string `json:"category"`
}

func (s *Server) moveTorrent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req moveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	category, newDir, err := s.resolveDir(req.Category)
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