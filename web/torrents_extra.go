package web

import (
	"encoding/json"
	"net/http"
)

// Detail-drawer extras: peer list, .torrent export and selective download.

// listPeers serves the live peer list of one torrent.
func (s *Server) listPeers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.engine.Peers(r.PathValue("id")))
}

// exportTorrent serves the torrent as a .torrent file download.
func (s *Server) exportTorrent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	data, err := s.engine.ExportTorrent(id)
	if err != nil {
		if err.Error() == "torrent not found" {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/x-bittorrent")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+id+".torrent\"")
	w.Write(data)
}

// getSkipped returns the file indices excluded from downloading.
func (s *Server) getSkipped(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"files": s.engine.SkippedFiles(r.PathValue("id")),
	})
}

// setSkipped applies a selective-download choice and persists it.
func (s *Server) setSkipped(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Files []int `json:"files"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if err := s.engine.SetSkippedFiles(id, req.Files); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	s.persistSkippedFiles(id, req.Files)
	writeJSON(w, http.StatusOK, map[string]string{"ok": "skipped"})
}
