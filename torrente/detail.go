package torrente

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// PieceCell is one cell of a torrent's piece map.
type PieceCell struct {
	Index  int  `json:"i"`
	Have   bool `json:"have"`
	Active bool `json:"active"`
}

// FileRow is one file of a torrent with its verified progress.
type FileRow struct {
	Index int     `json:"index"`
	Path  string  `json:"path"`
	Size  int64   `json:"size"`
	Done  int64   `json:"done"`
	Pct   float64 `json:"pct"`
	Skip  bool    `json:"skip"` // excluded via selective download
}

// Detail is the detailed per-torrent view used by the detail panel.
type Detail struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	InfoHash       string    `json:"info_hash"`
	State          string    `json:"state"`
	Progress       float64   `json:"progress"`
	Size           int64     `json:"size"`
	Downloaded     int64     `json:"downloaded"`
	Uploaded       int64     `json:"uploaded"`
	DownloadSpeed  int64     `json:"download_speed"`
	UploadSpeed    int64     `json:"upload_speed"`
	Seeders        int       `json:"seeders"`
	Leechers       int       `json:"leechers"`
	PeersConnected int       `json:"peers_connected"`
	PiecesHave     int       `json:"pieces_have"`
	PiecesTotal    int       `json:"pieces_total"`
	Ratio          float64   `json:"ratio"`
	RatioTarget    float64   `json:"ratio_target"`
	SeedDays       int64     `json:"seed_days"`
	Sequential     bool      `json:"sequential"`
	GuardPaused    bool      `json:"guard_paused"`
	Queued         bool      `json:"queued"`
	SeedSince      time.Time `json:"seed_since"`
	SeededTo       int       `json:"seeded_to"`
	SeededFirst    time.Time `json:"seeded_first"`
	LastSeen       time.Time `json:"last_seen"`
	AddedAt        time.Time `json:"added_at"`
	WorkingTrackers int      `json:"working_trackers"`
	FailedTrackers  int      `json:"failed_trackers"`
	Category       string    `json:"category"`
	SaveDir        string    `json:"save_dir"`
	Comment        string    `json:"comment"`
	CreatedBy      string    `json:"created_by"`
	Trackers       []TrackerStat `json:"trackers"`
	Pieces         []PieceCell   `json:"pieces"`
	Files          []FileRow     `json:"files"`
}

// Detail builds the detailed view of a torrent.
func (e *Engine) Detail(id string) (*Detail, error) {
	e.mu.Lock()
	t, ok := e.torrents[id]
	if !ok {
		e.mu.Unlock()
		return nil, fmt.Errorf("torrent not found")
	}
	tsCopy := make([]TrackerStat, 0, len(t.Trackers))
	for _, ts := range t.Trackers {
		tsCopy = append(tsCopy, *ts)
	}
	t.mu.Lock()
	d := &Detail{
		ID:              t.ID,
		Name:            t.Name,
		InfoHash:        t.InfoHash,
		State:           string(t.State),
		Progress:        t.Progress(),
		Size:            t.Size,
		Downloaded:      t.Downloaded,
		Uploaded:        t.Uploaded,
		DownloadSpeed:   t.DownloadSpeed,
		UploadSpeed:     t.UploadSpeed,
		Seeders:         t.Seeders,
		Leechers:        t.Leechers,
		PiecesHave:      t.PiecesHave,
		PiecesTotal:     t.PiecesTotal,
		Ratio:           t.Ratio(),
		RatioTarget:     t.RatioTarget,
		SeedDays:        t.SeedDaysTarget,
		Sequential:      t.Sequential,
		GuardPaused:     t.GuardPaused,
		Queued:          t.Queued,
		SeedSince:       t.SeedSince,
		SeededTo:        t.SeededTo,
		SeededFirst:     t.SeededFirst,
		LastSeen:        t.LastSeen,
		AddedAt:         t.AddedAt,
		WorkingTrackers: t.WorkingTrackers,
		FailedTrackers:  t.FailedTrackers,
		Category:        t.Category,
		SaveDir:         t.SaveDir,
		Trackers:        tsCopy,
	}
	if t.MetaInfo != nil {
		d.Comment = t.MetaInfo.Comment
		d.CreatedBy = t.MetaInfo.CreatedBy
	}
	if t.storage != nil {
		ph, pt := t.PieceStats()
		d.PiecesHave = ph
		d.PiecesTotal = pt
		snaps := t.storage.PiecesSnapshot()
		d.Pieces = make([]PieceCell, len(snaps))
		for i, p := range snaps {
			d.Pieces[i] = PieceCell{Index: i, Have: p.Present, Active: p.InFlight > 0 || p.Verifying}
		}
		for i, f := range t.storage.FilesSnapshot() {
			rel := f.Path
			if strings.HasPrefix(rel, d.SaveDir) {
				rel = strings.TrimPrefix(rel, d.SaveDir)
				rel = strings.TrimPrefix(rel, string(filepath.Separator))
			}
			pct := 0.0
			if f.Length > 0 {
				pct = float64(f.Done) / float64(f.Length)
			}
			skip := t.skippedFiles != nil && t.skippedFiles[i]
			d.Files = append(d.Files, FileRow{Index: i, Path: rel, Size: f.Length, Done: f.Done, Pct: pct, Skip: skip})
		}
	}
	d.Ratio = t.Ratio()
	t.mu.Unlock()

	// current live session count for this torrent
	active := 0
	for s := range e.sessions {
		if s.t == t {
			active++
		}
	}
	d.PeersConnected = active
	e.mu.Unlock()
	return d, nil
}