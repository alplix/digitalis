package web

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/alplix/digitalis/dl"
)

// Downloader API: bandwidth-oriented download tasks with selectable sinks
// (discard / RAM ring / disk), loop counts and optional rate caps.

type dlRequest struct {
	URL        string `json:"url"`
	Mode       string `json:"mode"`
	RamMB      int    `json:"ram_mb"`
	Disk       string `json:"disk"`
	Category   string `json:"category"`
	Filename   string `json:"filename"`
	Loops      int    `json:"loops"`
	Interval   int    `json:"interval"`
	SpeedCap   int64  `json:"speed_cap"`
	DeleteEach bool   `json:"delete_each"`
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
		RamMB:      req.RamMB,
		Filename:   req.Filename,
		Loops:      req.Loops,
		Interval:   req.Interval,
		SpeedCap:   req.SpeedCap,
		DeleteEach: req.DeleteEach,
	}
	if mode == dl.ModeDisk {
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