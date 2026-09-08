package web

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/alplix/digitalis/wget"
)

// wgetView decorates a wget.Task with computed speed/uptime.
type wgetView struct {
	ID        string `json:"id"`
	URL       string `json:"url"`
	Recursive bool   `json:"recursive"`
	Depth     int    `json:"depth"`
	DeleteAfter bool `json:"delete_after"`
	NoDNSCache  bool `json:"no_dns_cache"`
	Loop        bool `json:"loop"`
	Interval    int  `json:"interval"`
	Disk        string `json:"disk"`
	Category    string `json:"category"`
	State       string `json:"state"`
	Error       string `json:"error,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	EndedAt     time.Time `json:"ended_at"`
	Bytes       int64     `json:"bytes"`
	Files       int64     `json:"files"`
	Loops       int64     `json:"loops"`
	CurrentURL  string    `json:"current_url"`
	Speed       int64     `json:"speed"`
	UptimeSec   int64     `json:"uptime_sec"`
}

func wgetViews(ts []*wget.Task) []wgetView {
	out := make([]wgetView, 0, len(ts))
	for _, t := range ts {
		out = append(out, wgetView{
			ID: t.ID, URL: t.URL, Recursive: t.Recursive, Depth: t.Depth,
			DeleteAfter: t.DeleteAfter, NoDNSCache: t.NoDNSCache, Loop: t.Loop,
			Interval: t.Interval, Disk: t.Disk, Category: t.Category,
			State: t.State, Error: t.Err, StartedAt: t.StartedAt, EndedAt: t.EndedAt,
			Bytes: t.Bytes, Files: t.Files, Loops: t.Loops, CurrentURL: t.CurrentURL,
			Speed: t.Speed(), UptimeSec: int64(t.Uptime().Seconds()),
		})
	}
	return out
}

// wgetList reports all tasks plus aggregate bandwidth stats.
func (s *Server) wgetList(w http.ResponseWriter, r *http.Request) {
	tasks, total := s.wget.List()
	writeJSON(w, http.StatusOK, map[string]any{
		"tasks": wgetViews(tasks),
		"total": map[string]any{
			"bytes":      total.Bytes,
			"files":      total.Files,
			"started_at": total.StartedAt,
			"speed":      total.Speed,
		},
	})
}

// wgetNew starts a new download task.
func (s *Server) wgetNew(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL         string `json:"url"`
		Recursive   bool   `json:"recursive"`
		Depth       int    `json:"depth"`
		DeleteAfter bool   `json:"delete_after"`
		NoDNSCache  bool   `json:"no_dns_cache"`
		Loop        bool   `json:"loop"`
		Interval    int    `json:"interval"`
		Disk        string `json:"disk"`
		Category    string `json:"category"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.URL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url is required"})
		return
	}
	// Validate disk against registered roots.
	disk := ""
	for _, d := range s.engine.Disks() {
		if d == req.Disk {
			disk = d
			break
		}
	}
	t, err := s.wget.Add(wget.Options{
		URL: req.URL, Recursive: req.Recursive, Depth: req.Depth,
		DeleteAfter: req.DeleteAfter, NoDNSCache: req.NoDNSCache,
		Loop: req.Loop, Interval: req.Interval, Disk: disk, Category: req.Category,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": t.ID})
}

// wgetStop cancels and removes a task.
func (s *Server) wgetStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.wget.Stop(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "stopped"})
}