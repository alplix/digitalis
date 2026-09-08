package web

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// diskView is a single storage root with its space usage, per-category usage
// and per-torrent disk footprint, so the Storage page can sort torrents by how
// much space they occupy (largest first).
type diskView struct {
	Path   string          `json:"path"`
	Label  string          `json:"label"`
	Total  int64           `json:"total"`
	Free   int64           `json:"free"`
	Usage  int64           `json:"usage"`
	Cats   []categoryUsage `json:"categories"`
	Items  []diskTorrent   `json:"torrents"`
}

// diskTorrent is one torrent's footprint on a storage root.
type diskTorrent struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Category string  `json:"category"`
	State    string  `json:"state"`
	Progress float64 `json:"progress"`
	Size     int64   `json:"size"`
}

// listDisks reports every registered storage root. Each root's torrent list is
// sorted by on-disk size (largest first).
func (s *Server) listDisks(w http.ResponseWriter, r *http.Request) {
	roots := s.engine.Disks()
	out := make([]diskView, 0, len(roots))
	for _, root := range roots {
		out = append(out, s.diskSnapshot(root))
	}
	writeJSON(w, http.StatusOK, out)
}

// diskSnapshot builds the view for a single storage root.
func (s *Server) diskSnapshot(root string) diskView {
	dv := diskView{Path: filepath.Clean(root), Label: filepath.Base(filepath.Clean(root))}
	var st syscall.Statfs_t
	if err := syscall.Statfs(root, &st); err == nil {
		dv.Total = int64(st.Blocks) * int64(st.Bsize)
		dv.Free = int64(st.Bavail) * int64(st.Bsize)
	}
	entries, err := os.ReadDir(root)
	if err == nil {
		dirs := make([]string, 0, len(entries))
		for _, e := range entries {
			if e.IsDir() {
				dirs = append(dirs, e.Name())
			}
		}
		sort.Strings(dirs)
		var total int64
		for _, name := range dirs {
			sz := dirSize(filepath.Join(root, name))
			dv.Cats = append(dv.Cats, categoryUsage{Name: name, Size: sz})
			total += sz
		}
		dv.Usage = total
	}
	key := filepath.Clean(root)
	for _, t := range s.engine.Torrents() {
		sd := filepath.Clean(t.SaveDir)
		if !underDir(key, sd) {
			continue
		}
		dv.Items = append(dv.Items, diskTorrent{
			ID:       t.ID,
			Name:     t.Name,
			Category: t.Category,
			State:    string(t.State),
			Progress: t.Progress() * 100,
			Size:     dirSize(filepath.Join(sd, t.Name)),
		})
	}
	sort.Slice(dv.Items, func(i, j int) bool { return dv.Items[i].Size > dv.Items[j].Size })
	return dv
}

// underDir reports whether path lies inside root (not root itself).
func underDir(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// diskReq carries the path of a disk to add/remove.
type diskReq struct {
	Path string `json:"path"`
}

func (s *Server) addDisk(w http.ResponseWriter, r *http.Request) {
	var req diskReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	p := strings.TrimSpace(req.Path)
	if err := s.engine.AddDisk(p); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.diskSnapshot(p))
}

func (s *Server) removeDisk(w http.ResponseWriter, r *http.Request) {
	var req diskReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	p := strings.TrimSpace(req.Path)
	if err := s.engine.RemoveDisk(p); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "removed"})
}

// diskRootOf returns the registered storage root that contains saveDir, or ""
// when it is not under any extra root (i.e. it lives in the base dir).
func (s *Server) diskRootOf(saveDir string) string {
	if saveDir == "" {
		return ""
	}
	for _, d := range s.engine.Disks() {
		if filepath.Clean(d) == filepath.Clean(s.saveDir) {
			continue
		}
		if underDir(filepath.Clean(d), filepath.Clean(saveDir)) {
			return filepath.Clean(d)
		}
	}
	return ""
}