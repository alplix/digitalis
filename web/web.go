// Package web implements the Digitalis web UI and JSON API.
package web

import (
	"embed"
	"encoding/json"
	"io"
	"net/http"
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
	AddedAt        time.Time `json:"added_at"`
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
// URL, or raw base64 of a torrent file.
type addRequest struct {
	Source string `json:"source"`
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
		t, err := s.engine.AddMagnet(m, s.saveDir)
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, s.snapshot(t))
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
		t, err := s.engine.AddTorrent(mi, s.saveDir)
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, s.snapshot(t))
		return
	}

	// try raw bencode (base64/json encoded as string)
	if mi, err := metainfo.Parse([]byte(src)); err == nil {
		t, err := s.engine.AddTorrent(mi, s.saveDir)
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, s.snapshot(t))
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