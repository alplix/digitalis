package web

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Storage roots API: every registered root with its volume-level usage
// (total/free/used), the size of managed content (categories), the torrent
// count, the trash footprint and the live disk-guard state.

type diskView struct {
	Path             string          `json:"path"`
	Label            string          `json:"label"`
	Base             bool            `json:"base"`
	Total            int64           `json:"total"`
	Free             int64           `json:"free"`
	Used             int64           `json:"used"`
	Content          int64           `json:"content"`
	Count            int             `json:"count"`
	TrashBytes       int64           `json:"trash_bytes"`
	Categories       []categoryUsage `json:"categories"`
	Guard            bool            `json:"guard"`
	GuardPausedCount int             `json:"guard_paused"`
}

// diskCache caches the expensive on-disk walk (dirSize over every folder) for
// a short while so the Storage page stays snappy.
type diskCacheEntry struct {
	at   time.Time
	dv   diskView
}

var (
	diskCacheMu sync.Mutex
	diskCache   = map[string]diskCacheEntry{}
)

const diskCacheTTL = 30 * time.Second

func (s *Server) listDisks(w http.ResponseWriter, r *http.Request) {
	roots := s.engine.Disks()
	out := make([]diskView, 0, len(roots))
	for i, root := range roots {
		dv := s.cachedDiskView(root)
		dv.Base = i == 0
		s.applyLiveDiskState(&dv, root)
		out = append(out, dv)
	}
	writeJSON(w, http.StatusOK, out)
}

// cachedDiskView returns the disk view from cache when fresh, else rebuilds it.
func (s *Server) cachedDiskView(root string) diskView {
	key := filepath.Clean(root)
	diskCacheMu.Lock()
	if ent, ok := diskCache[key]; ok && time.Since(ent.at) < diskCacheTTL {
		diskCacheMu.Unlock()
		return ent.dv
	}
	diskCacheMu.Unlock()

	dv := s.buildDiskView(root)

	diskCacheMu.Lock()
	diskCache[key] = diskCacheEntry{at: time.Now(), dv: dv}
	diskCacheMu.Unlock()
	return dv
}

// applyLiveDiskState overlays cheap, always-current numbers (counts, guard).
func (s *Server) applyLiveDiskState(dv *diskView, root string) {
	key := filepath.Clean(root)
	v := s.engine.View()
	minFree := v.DiskGuardMinGB << 30
	for _, t := range s.engine.Torrents() {
		sd := filepath.Clean(t.SaveDir)
		if sd == "" || sd == "." || !underDir(key, sd) {
			continue
		}
		dv.Count++
		if t.IsGuardPaused() {
			dv.GuardPausedCount++
		}
	}
	dv.Guard = v.DiskGuard && v.DiskGuardMinGB > 0 && dv.Free >= 0 && dv.Free < minFree
}

// buildDiskView walks the root: volume stats, per-folder sizes, trash size.
func (s *Server) buildDiskView(root string) diskView {
	clean := filepath.Clean(root)
	dv := diskView{
		Path:       clean,
		Label:      filepath.Base(clean),
		Categories: []categoryUsage{},
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(root, &st); err == nil {
		dv.Total = int64(st.Blocks) * int64(st.Bsize)
		dv.Free = int64(st.Bavail) * int64(st.Bsize)
		dv.Used = dv.Total - dv.Free
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return dv
	}
	dirs := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() && e.Name() != ".trash" {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)
	var total int64
	for _, name := range dirs {
		sz := dirSize(filepath.Join(root, name))
		dv.Categories = append(dv.Categories, categoryUsage{Name: name, Size: sz})
		total += sz
	}
	dv.Content = total
	dv.TrashBytes = dirSize(filepath.Join(root, ".trash"))
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
	dv := s.buildDiskView(p)
	dv.Base = false
	s.applyLiveDiskState(&dv, p)
	writeJSON(w, http.StatusOK, dv)
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
	diskCacheMu.Lock()
	delete(diskCache, filepath.Clean(p))
	diskCacheMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"ok": "removed"})
}

// emptyDiskTrash deletes every trash entry under one root (base disk = all).
func (s *Server) emptyDiskTrash(w http.ResponseWriter, r *http.Request) {
	var req diskReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	p := strings.TrimSpace(req.Path)
	roots := s.engine.Disks()
	if len(roots) == 0 {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "no disks"})
		return
	}
	if p == "" || filepath.Clean(p) == filepath.Clean(roots[0]) {
		// the base view aggregates nothing special: empty only its own trash
		p = roots[0]
	}
	n := s.engine.EmptyTrash(p)
	diskCacheMu.Lock()
	delete(diskCache, filepath.Clean(p))
	diskCacheMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": "emptied", "removed": n})
}

// invalidateDiskCache drops one root from the cache (used after mutations).
func (s *Server) invalidateDiskCache(root string) {
	diskCacheMu.Lock()
	delete(diskCache, filepath.Clean(root))
	diskCacheMu.Unlock()
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
