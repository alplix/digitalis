package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"
)

// Share links: short-lived tokenized download URLs for single files. Tokens
// live in memory (a restart revokes them all) and are exempt from the access
// token on purpose — they are deliberate handouts.

type shareLink struct {
	root    string
	full    string
	name    string
	expires time.Time
}

var (
	shareMu     sync.Mutex
	shareTokens = map[string]*shareLink{}
)

// createShare issues a share link for one file under a storage root.
func (s *Server) createShare(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Disk  string `json:"disk"`
		Path  string `json:"path"`
		Hours int    `json:"hours"`
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
	fi, err := os.Stat(full)
	if err != nil || fi.IsDir() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not a file"})
		return
	}
	hours := req.Hours
	if hours <= 0 || hours > 24*30 {
		hours = 24
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	token := hex.EncodeToString(buf)
	exp := time.Now().Add(time.Duration(hours) * time.Hour)

	shareMu.Lock()
	sharePruneLocked()
	shareTokens[token] = &shareLink{root: root, full: full, name: fi.Name(), expires: exp}
	shareMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"url":     "/s/" + token,
		"expires": exp.Format(time.RFC3339),
	})
}

// sharePruneLocked drops expired tokens; caller holds shareMu.
func sharePruneLocked() {
	now := time.Now()
	for k, sl := range shareTokens {
		if now.After(sl.expires) {
			delete(shareTokens, k)
		}
	}
}

// serveShare serves one shared file by token, without access-token auth.
func (s *Server) serveShare(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	shareMu.Lock()
	sl := shareTokens[token]
	shareMu.Unlock()
	if sl == nil || time.Now().After(sl.expires) {
		http.Error(w, "link expired", http.StatusGone)
		return
	}
	f, err := os.Open(sl.full)
	if err != nil {
		http.Error(w, "file gone", http.StatusNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+url.PathEscape(sl.name)+"\"")
	http.ServeContent(w, r, sl.name, modTime(sl.full), f)
}

func modTime(path string) time.Time {
	if fi, err := os.Stat(path); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}
