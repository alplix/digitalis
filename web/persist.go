package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/alplix/digitalis/metainfo"
	"github.com/alplix/digitalis/torrente"
)

// torrentRecord captures how a torrent was added so it can be restored across
// service restarts. Kind is "magnet" or "url"; Source is the magnet URI or the
// .torrent URL used at add time.
type torrentRecord struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Source   string `json:"source"`
	Category string `json:"category"`
}

// recordsPath returns the persistence file inside the config directory.
func recordsPath(cfgDir string) string {
	return filepath.Join(cfgDir, "torrents.json")
}

// loadRecords reads the saved torrent records from disk.
func loadRecords(cfgDir string) []torrentRecord {
	data, err := os.ReadFile(recordsPath(cfgDir))
	if err != nil {
		return nil
	}
	var recs []torrentRecord
	if err := json.Unmarshal(data, &recs); err != nil {
		return nil
	}
	return recs
}

// saveRecords atomically writes the records file.
func saveRecords(cfgDir string, recs []torrentRecord) error {
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	tmp := recordsPath(cfgDir) + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, recordsPath(cfgDir))
}

// persistRecord adds/updates a record in memory and to disk.
func (s *Server) persistRecord(rec torrentRecord) {
	s.recordsMu.Lock()
	replaced := false
	for i := range s.records {
		if s.records[i].ID == rec.ID {
			s.records[i] = rec
			replaced = true
			break
		}
	}
	if !replaced {
		s.records = append(s.records, rec)
	}
	recs := append([]torrentRecord(nil), s.records...)
	s.recordsMu.Unlock()
	if s.cfgDir != "" {
		if err := saveRecords(s.cfgDir, recs); err != nil {
			s.engine.Logf("persist records: %v", err)
		}
	}
}

// dropRecord removes a persisted record by torrent id.
func (s *Server) dropRecord(id string) {
	s.recordsMu.Lock()
	out := s.records[:0]
	for _, r := range s.records {
		if r.ID != id {
			out = append(out, r)
		}
	}
	s.records = out
	recs := append([]torrentRecord(nil), s.records...)
	s.recordsMu.Unlock()
	if s.cfgDir != "" {
		if err := saveRecords(s.cfgDir, recs); err != nil {
			s.engine.Logf("persist records: %v", err)
		}
	}
}

// restoreRecords re-adds persisted torrents into the engine. It runs in the
// background so a slow .torrent URL fetch never blocks startup.
func (s *Server) restoreRecords(recs []torrentRecord) {
	go func() {
		for _, rec := range recs {
			if rec.Source == "" {
				continue
			}
			s.addTorrentRecord(rec)
		}
	}()
}

// addTorrentRecord adds a torrent from a persisted record (without re-saving).
func (s *Server) addTorrentRecord(rec torrentRecord) error {
	_, saveDir, err := s.resolveDir(rec.Category)
	if err != nil {
		saveDir = s.saveDir
	}
	t, err := s.engineAdd(rec.Source, saveDir)
	if err != nil {
		s.engine.Logf("restore %s (%s) failed: %v", rec.ID, rec.Kind, err)
		return err
	}
	t.SetCategory(rec.Category)
	return nil
}

// engineAdd mirrors the add logic of POST /api/torrents minus JSON handling.
func (s *Server) engineAdd(src, saveDir string) (*torrente.Torrent, error) {
	if err := os.MkdirAll(saveDir, 0755); err != nil {
		return nil, err
	}
	if len(src) >= 7 && src[:7] == "magnet:" {
		m, err := metainfo.ParseMagnet(src)
		if err != nil {
			return nil, err
		}
		return s.engine.AddMagnet(m, saveDir)
	}
	if len(src) >= 7 && src[:7] == "http://" || len(src) >= 8 && src[:8] == "https://" {
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Get(src)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, errors.New("fetch returned " + resp.Status)
		}
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		mi, err := metainfo.Parse(data)
		if err != nil {
			return nil, err
		}
		return s.engine.AddTorrent(mi, saveDir)
	}
	mi, err := metainfo.Parse([]byte(src))
	if err != nil {
		return nil, errors.New("unsupported source")
	}
	return s.engine.AddTorrent(mi, saveDir)
}