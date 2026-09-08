package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// fileEntry is one entry of the file browser.
type fileEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

// fileRootOf resolves a disk parameter ("" = base save dir, otherwise a
// registered storage root) into the absolute directory listing is relative to.
func (s *Server) fileRootOf(disk string) (string, error) {
	if strings.TrimSpace(disk) == "" {
		return s.saveDir, nil
	}
	for _, d := range s.engine.Disks() {
		if filepath.Clean(d) == filepath.Clean(disk) {
			return filepath.Clean(d), nil
		}
	}
	return "", errors.New("unknown disk")
}

// resolveFilePath turns a request rel path into an absolute path that is
// guaranteed to live under root. Paths may be absolute or relative but are
// normalized and must not escape root.
func resolveFilePath(root, rel string) (string, error) {
	base, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	d := strings.TrimSpace(rel)
	full := base
	if d != "" && d != "." && d != "/" {
		abs := d
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(base, filepath.FromSlash(d))
		}
		full, err = filepath.Abs(abs)
		if err != nil {
			return "", err
		}
	}
	if full != base && !strings.HasPrefix(full, base+string(os.PathSeparator)) {
		return "", errors.New("outside disk root")
	}
	return full, nil
}

// listFiles browses a directory on the base save dir or a registered storage
// root. Query params: disk (optional root), path (optional rel dir).
func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) {
	root, err := s.fileRootOf(r.URL.Query().Get("disk"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	full, err := resolveFilePath(root, r.URL.Query().Get("path"))
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	rel := ""
	if r.URL.Query().Get("path") != "" {
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(r.URL.Query().Get("path"))))
		rel = clean
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	out := make([]fileEntry, 0, len(entries))
	for _, e := range entries {
		info, _ := e.Info()
		var size int64
		if info != nil {
			size = info.Size()
		}
		p := e.Name()
		if rel != "" && rel != "." {
			p = rel + "/" + e.Name()
		}
		out = append(out, fileEntry{Name: e.Name(), Path: p, IsDir: e.IsDir(), Size: size})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	writeJSON(w, http.StatusOK, out)
}

// downloadFile streams a file as an attachment. Only files are streamable;
// directories return an error.
func (s *Server) downloadFile(w http.ResponseWriter, r *http.Request) {
	root, err := s.fileRootOf(r.URL.Query().Get("disk"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	full, err := resolveFilePath(root, r.URL.Query().Get("path"))
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	info, err := os.Stat(full)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if info.IsDir() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot download a directory"})
		return
	}
	f, err := os.Open(full)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("Content-Disposition", "attachment; filename=\""+url.PathEscape(filepath.Base(full))+"\"")
	io.Copy(w, f)
}

// uploadFile saves an uploaded file into the requested directory.
func (s *Server) uploadFile(w http.ResponseWriter, r *http.Request) {
	root, err := s.fileRootOf(r.URL.Query().Get("disk"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	dir, err := resolveFilePath(root, r.URL.Query().Get("path"))
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing file field"})
		return
	}
	defer file.Close()
	dst, err := resolveFilePath(dir, header.Filename)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	out, err := os.Create(dst)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	defer out.Close()
	if _, err := io.Copy(out, file); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": header.Filename})
}

// makeDir creates a new directory (nested allowed) on a storage root.
func (s *Server) makeDir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Disk string `json:"disk"`
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	root, err := s.fileRootOf(req.Disk)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	full, err := resolveFilePath(root, req.Path)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	if err := os.MkdirAll(full, 0755); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.fileListJSON(root, full))
}

// moveFile renames or relocates a file/dir. src and dst are both relative to
// the same disk root; both may include a leading path.
func (s *Server) moveFile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Disk string `json:"disk"`
		Src  string `json:"src"`
		Dst  string `json:"dst"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	root, err := s.fileRootOf(req.Disk)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	src, err := resolveFilePath(root, req.Src)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	dst, err := resolveFilePath(root, req.Dst)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	if err := os.Rename(src, dst); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "moved"})
}

// deleteFile removes a file or a directory tree (os.RemoveAll). Note: torrent
// payloads are better removed through the torrent API so engine state and disk
// stay consistent; the file manager delete is for stray files/folders.
func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Disk string `json:"disk"`
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	root, err := s.fileRootOf(req.Disk)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	full, err := resolveFilePath(root, req.Path)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	if full == root {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cannot delete disk root"})
		return
	}
	if err := os.RemoveAll(full); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "deleted"})
}

// fileListJSON renders the directory listing for full as a JSON array.
func (s *Server) fileListJSON(root, full string) []fileEntry {
	rel, _ := filepath.Rel(root, full)
	relSlash := filepath.ToSlash(rel)
	if relSlash == "." {
		relSlash = ""
	}
	entries, _ := os.ReadDir(full)
	out := []fileEntry{}
	for _, e := range entries {
		info, _ := e.Info()
		var size int64
		if info != nil {
			size = info.Size()
		}
		p := e.Name()
		if relSlash != "" {
			p = relSlash + "/" + e.Name()
		}
		out = append(out, fileEntry{Name: e.Name(), Path: p, IsDir: e.IsDir(), Size: size})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}