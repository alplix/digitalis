package web

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/alplix/digitalis/dl"
)

// dlmgrPath anchors the downloader history file inside the config dir.
func dlmgrPath(cfgDir string) string {
	if cfgDir == "" {
		return ""
	}
	return filepath.Join(cfgDir, "dl_history.json")
}

// Downloader API: bandwidth-oriented download tasks with selectable sinks
// (discard / RAM ring / disk), loop counts, retries, verification, scheduling
// and optional rate caps — plus repo browsing and run history.

// dlRepos serves the curated public repository list.
func (s *Server) dlRepos(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, dl.Repos())
}

// dlRepoBrowse lists a repository directory via its autoindex page.
func (s *Server) dlRepoBrowse(w http.ResponseWriter, r *http.Request) {
	u := r.URL.Query().Get("url")
	if strings.TrimSpace(u) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing url"})
		return
	}
	entries, err := dl.BrowseRepo(u, "")
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"url": u, "entries": entries})
}

// dlHistory serves the stored run history.
func (s *Server) dlHistory(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.dl.History())
}

// dlHistoryClear wipes the run history.
func (s *Server) dlHistoryClear(w http.ResponseWriter, r *http.Request) {
	s.dl.ClearHistory()
	writeJSON(w, http.StatusOK, map[string]string{"ok": "cleared"})
}

// dlHistoryCSV exports the run history as CSV.
func (s *Server) dlHistoryCSV(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\"dl-history.csv\"")
	io.WriteString(w, "finished_at,url,mode,direction,state,bytes,loops,duration_sec,avg_speed,verified,error\n")
	for _, h := range s.dl.History() {
		row := []string{
			h.FinishedAt.Format("2006-01-02 15:04:05"),
			csvField(h.URL), h.Mode, h.Direction, h.State,
			strconv.FormatInt(h.Bytes, 10),
			strconv.FormatInt(h.Loops, 10),
			strconv.FormatInt(h.DurationSec, 10),
			strconv.FormatInt(h.AvgSpeed, 10),
			h.Verified, csvField(h.Error),
		}
		io.WriteString(w, strings.Join(row, ",")+"\n")
	}
}

// csvField quotes a field when needed.
func csvField(s string) string {
	if strings.ContainsAny(s, ",\"\n") {
		s = "\"" + strings.ReplaceAll(s, "\"", "\"\"") + "\""
	}
	return s
}

type dlRequest struct {
	URL        string `json:"url"`
	Mode       string `json:"mode"`
	Direction  string `json:"direction"`
	UploadMB   int    `json:"upload_mb"`
	RamMB      int    `json:"ram_mb"`
	Disk       string `json:"disk"`
	Category   string `json:"category"`
	Filename   string `json:"filename"`
	Loops      int    `json:"loops"`
	Interval   int    `json:"interval"`
	SpeedCap   int64  `json:"speed_cap"`
	DeleteEach bool   `json:"delete_each"`
	Conn       int    `json:"conn"`
	Retries    int    `json:"retries"`
	RetryWait  int    `json:"retry_wait"`
	VerifySHA  string `json:"verify_sha256"`
	Proxy      string `json:"proxy"`
	StartAt    string `json:"start_at"`
}

// dlList serves every task plus aggregate totals.
func (s *Server) dlList(w http.ResponseWriter, r *http.Request) {
	tasks, tot := s.dl.List()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"tasks": tasks,
		"total": tot,
	})
}

// dlStart begins a new download task.
func (s *Server) dlStart(w http.ResponseWriter, r *http.Request) {
	var req dlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	mode := dl.Mode(strings.ToLower(strings.TrimSpace(req.Mode)))
	opts := dl.Options{
		URL:        req.URL,
		Mode:       mode,
		Direction:  req.Direction,
		UploadMB:   req.UploadMB,
		RamMB:      req.RamMB,
		Filename:   req.Filename,
		Loops:      req.Loops,
		Interval:   req.Interval,
		SpeedCap:   req.SpeedCap,
		DeleteEach: req.DeleteEach,
		Conn:       req.Conn,
		Retries:    req.Retries,
		RetryWait:  req.RetryWait,
		VerifySHA:  req.VerifySHA,
		Proxy:      req.Proxy,
		StartAt:    req.StartAt,
	}
	if mode == dl.ModeDisk && opts.Direction != "up" {
		category, saveDir, err := s.resolveDiskDir(req.Disk, req.Category)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		_ = category
		opts.SaveDir = saveDir
	}
	t, err := s.dl.Add(opts)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// dlStop stops a running task but keeps it listed.
func (s *Server) dlStop(w http.ResponseWriter, r *http.Request) {
	if err := s.dl.Stop(r.PathValue("id")); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "stopped"})
}

// dlRemove stops (if needed) and drops a task from the list.
func (s *Server) dlRemove(w http.ResponseWriter, r *http.Request) {
	if err := s.dl.Remove(r.PathValue("id")); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "removed"})
}

// dlTaskLog serves the terminal-style event log of a task.
func (s *Server) dlTaskLog(w http.ResponseWriter, r *http.Request) {
	lines, ok := s.dl.TaskLog(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"lines": lines})
}

// dlRAMPreview returns the newest bytes held in a RAM-mode task's ring buffer
// (handy to sanity-check what a server actually sent).
func (s *Server) dlRAMPreview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	data, ok := s.dl.RAMPreview(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no RAM buffer for this task"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"id":    id,
		"bytes": strconv.Itoa(len(data)),
		"head":  safeHead(data, 512),
	})
}

func safeHead(data []byte, n int) string {
	if len(data) > n {
		data = data[:n]
	}
	var sb strings.Builder
	for _, b := range data {
		if b >= 32 && b < 127 {
			sb.WriteByte(b)
		} else {
			sb.WriteByte('.')
		}
	}
	return sb.String()
}