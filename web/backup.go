package web

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/alplix/digitalis/torrente"
)

// Backup / restore: a single JSON file containing settings, torrent records
// and RSS feeds. Tokens are included deliberately so a backup is complete;
// the endpoints require normal API authentication.

type backupFile struct {
	Version    string           `json:"version"`
	ExportedAt time.Time        `json:"exported_at"`
	Settings   interface{}      `json:"settings"`
	Records    []torrentRecord  `json:"records"`
	Feeds      []rssFeed        `json:"feeds"`
}

// exportBackup streams the backup JSON.
func (s *Server) exportBackup(w http.ResponseWriter, r *http.Request) {
	s.recordsMu.Lock()
	recs := append([]torrentRecord(nil), s.records...)
	s.recordsMu.Unlock()
	s.rssMu.Lock()
	feeds := append([]rssFeed(nil), s.feeds...)
	s.rssMu.Unlock()

	b := backupFile{
		Version:    "digitalis-1",
		ExportedAt: time.Now(),
		Settings:   s.engine.View(),
		Records:    recs,
		Feeds:      feeds,
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", "attachment; filename=\"digitalis-backup.json\"")
	json.NewEncoder(w).Encode(b)
}

// importBackup applies a previously exported backup file. Torrents referenced
// by the records are re-added; existing ones are left alone.
func (s *Server) importBackup(w http.ResponseWriter, r *http.Request) {
	var b backupFile
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	applied := 0

	if st, ok := b.Settings.(map[string]interface{}); ok {
		raw, _ := json.Marshal(st)
		var patch torrente.Settings
		if json.Unmarshal(raw, &patch) == nil {
			if _, err := s.engine.UpdateSettings(patch); err == nil {
				applied++
			} else {
				s.engine.Logf("backup import settings: %v", err)
			}
		}
	}

	if len(b.Records) > 0 {
		s.recordsMu.Lock()
		s.records = append([]torrentRecord(nil), b.Records...)
		recs := append([]torrentRecord(nil), s.records...)
		s.recordsMu.Unlock()
		if s.cfgDir != "" {
			_ = saveRecords(s.cfgDir, recs)
		}
		s.restoreRecords(recs)
		applied++
	}

	if len(b.Feeds) > 0 {
		s.rssMu.Lock()
		s.feeds = append([]rssFeed(nil), b.Feeds...)
		s.rssMu.Unlock()
		s.saveRSSFeeds()
		applied++
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": "imported", "applied": applied})
}
