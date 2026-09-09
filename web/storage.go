package web

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"syscall"
)

// storageInfo reports free space on the save volume together with the size of
// every category folder under the save root, so the dashboard can surface
// "disk nearly full" at a glance.
type storageInfo struct {
	Free       int64           `json:"free"`
	Total      int64           `json:"total"`
	Usage      int64           `json:"usage"`
	Categories []categoryUsage `json:"categories"`
}

type categoryUsage struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

func (s *Server) storageInfo(w http.ResponseWriter, r *http.Request) {
	root := s.saveDir
	info := storageInfo{}
	var st syscall.Statfs_t
	if err := syscall.Statfs(root, &st); err == nil {
		info.Total = int64(st.Blocks) * int64(st.Bsize)
		info.Free = int64(st.Bavail) * int64(st.Bsize)
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
			info.Categories = append(info.Categories, categoryUsage{Name: name, Size: sz})
			total += sz
		}
		info.Usage = total
	}
	writeJSON(w, http.StatusOK, info)
}

// dirSize sums the sizes of all regular files below dir (without following
// symlinks, so we never escape the torrent store).
func dirSize(dir string) int64 {
	var total int64
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type() == os.ModeSymlink {
			return nil
		}
		if !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				total += fi.Size()
			}
		}
		return nil
	})
	return total
}

func (s *Server) reannounce(w http.ResponseWriter, r *http.Request) {
	if err := s.engine.ReAnnounce(r.PathValue("id")); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "announced"})
}

func (s *Server) reannounceAll(w http.ResponseWriter, r *http.Request) {
	n := s.engine.ReAnnounceAll()
	writeJSON(w, http.StatusOK, map[string]string{"ok": "announced", "count": strconv.Itoa(n)})
}
