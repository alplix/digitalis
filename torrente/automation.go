package torrente

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/alplix/digitalis/metainfo"
)

// Automation: disk guard, download queue, trash and rule-based categorization.
// Everything here is driven from the stats loop so it keeps working without
// any user interaction.

// guardHysteresisGB is the extra free space (in GB) required before a
// disk-guard paused torrent may resume, so torrents do not flap on and off.
const guardHysteresisGB = 2

// TrashEntry records one trashed torrent's data directory.
type TrashEntry struct {
	Path      string    `json:"path"`
	Name      string    `json:"name"`
	DeletedAt time.Time `json:"deleted_at"`
}

// underRoot reports whether path equals root or lies inside it.
func underRoot(root, path string) bool {
	root, path = filepath.Clean(root), filepath.Clean(path)
	if root == path {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// diskFreeBytes reports the available bytes on the filesystem holding root.
func diskFreeBytes(root string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(root, &st); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

// automationTick runs the disk guard and the download queue. Called from the
// stats loop (every 30s).
func (e *Engine) automationTick(cur Settings) {
	e.diskGuardTick(cur)
	e.queueTick(cur)
}

// diskGuardTick pauses downloading torrents whose storage root is running out
// of free space, and hands previously paused torrents back to the queue once
// space recovers.
func (e *Engine) diskGuardTick(cur Settings) {
	if !cur.DiskGuard || cur.DiskGuardMinGB <= 0 {
		// Guard disabled: release every guard pause back into the queue.
		e.mu.Lock()
		var released []*Torrent
		for _, t := range e.torrents {
			t.mu.Lock()
			if t.GuardPaused {
				t.GuardPaused = false
				if t.State == StatePaused {
					t.Queued = true
					released = append(released, t)
				}
			}
			t.mu.Unlock()
		}
		e.mu.Unlock()
		return
	}
	minFree := cur.DiskGuardMinGB << 30
	resumeFree := (cur.DiskGuardMinGB + guardHysteresisGB) << 30

	type rootFree struct{ root string; free int64 }
	var spaces []rootFree
	for _, root := range e.Disks() {
		if f := diskFreeBytes(root); f >= 0 {
			spaces = append(spaces, rootFree{filepath.Clean(root), f})
		}
	}

	e.mu.Lock()
	var toPause []*Torrent
	var justGuarded bool
	for _, t := range e.torrents {
		t.mu.Lock()
		if t.SaveDir == "" {
			t.mu.Unlock()
			continue
		}
		var free int64 = -1
		for _, sf := range spaces {
			if underRoot(sf.root, t.SaveDir) {
				free = sf.free
				break
			}
		}
		if free < 0 {
			t.mu.Unlock()
			continue
		}
		switch {
		case t.State == StateDownloading && free < minFree:
			justGuarded = justGuarded || !t.GuardPaused
			t.GuardPaused = true
			t.Queued = false
			toPause = append(toPause, t)
		case t.GuardPaused && t.State == StatePaused && free > resumeFree:
			// recovered: hand back to the queue
			t.GuardPaused = false
			t.Queued = true
		}
		t.mu.Unlock()
	}
	e.mu.Unlock()

	for _, t := range toPause {
		t.setState(StatePaused)
		t.mu.Lock()
		name := t.Name
		t.mu.Unlock()
		e.Logf("disk guard: paused %q (free space below %d GB)", name, cur.DiskGuardMinGB)
	}
	if justGuarded {
		e.notify(NoticeDiskGuard, "", fmt.Sprintf("%d GB", cur.DiskGuardMinGB))
	}
}

// queueTick keeps at most MaxActiveDownloads torrents downloading. Excess
// torrents pause and start again in add order as slots free up. A limit of
// zero means unlimited: every queued torrent starts.
func (e *Engine) queueTick(cur Settings) {
	max := cur.MaxActiveDownloads

	e.mu.Lock()
	var active, waiting []*Torrent
	for _, t := range e.torrents {
		t.mu.Lock()
		st, q := t.State, t.Queued
		t.mu.Unlock()
		switch {
		case st == StateDownloading:
			active = append(active, t)
		case q && st == StatePaused:
			waiting = append(waiting, t)
		}
	}
	e.mu.Unlock()

	// pause the newest torrents beyond the limit
	if max > 0 && len(active) > max {
		sortByAdded(active)
		for _, t := range active[max:] {
			t.mu.Lock()
			t.Queued = true
			t.mu.Unlock()
			t.setState(StatePaused)
		}
	}

	// start waiting torrents while slots remain
	slots := 0
	if max <= 0 {
		slots = len(waiting)
	} else {
		n := 0
		for _, t := range active {
			t.mu.Lock()
			st := t.State
			t.mu.Unlock()
			if st == StateDownloading {
				n++
			}
		}
		if n < max {
			slots = max - n
		}
	}
	if slots <= 0 || len(waiting) == 0 {
		return
	}
	sortByAdded(waiting)
	for _, t := range waiting {
		if slots <= 0 {
			break
		}
		t.mu.Lock()
		t.Queued = false
		t.mu.Unlock()
		if err := e.Resume(t.ID); err == nil {
			slots--
		}
	}
}

func sortByAdded(list []*Torrent) {
	sort.Slice(list, func(i, j int) bool { return list[i].AddedAt.Before(list[j].AddedAt) })
}

// IsGuardPaused/IsQueued are lock-safe flag readers for the API layer.
func (t *Torrent) IsGuardPaused() bool { t.mu.Lock(); defer t.mu.Unlock(); return t.GuardPaused }
func (t *Torrent) IsQueued() bool      { t.mu.Lock(); defer t.mu.Unlock(); return t.Queued }

// ---------- trash ----------

func (e *Engine) trashPath() string {
	if e.settingsFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(e.settingsFile), "trash.json")
}

func (e *Engine) loadTrash() []TrashEntry {
	p := e.trashPath()
	if p == "" {
		return nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var out []TrashEntry
	if json.Unmarshal(data, &out) != nil {
		return nil
	}
	return out
}

func (e *Engine) saveTrash(entries []TrashEntry) {
	p := e.trashPath()
	if p == "" {
		return
	}
	if entries == nil {
		entries = []TrashEntry{}
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, data, 0644) == nil {
		os.Rename(tmp, p)
	}
}

// trashOrRemove moves path (a torrent's data) into <root>/.trash when a trash
// retention is configured, otherwise deletes it outright. A failed move falls
// back to deletion so removal never leaves half-state behind.
func (e *Engine) trashOrRemove(path, name string) error {
	days := e.View().TrashDays
	if days <= 0 {
		return os.RemoveAll(path)
	}
	root := e.rootOf(path)
	if root == "" {
		return os.RemoveAll(path)
	}
	trashDir := filepath.Join(root, ".trash")
	if err := os.MkdirAll(trashDir, 0755); err != nil {
		return e.removeFallback(path, err)
	}
	dst := filepath.Join(trashDir, fmt.Sprintf("%d_%s", time.Now().UnixNano(), filepath.Base(name)))
	e.trashMu.Lock()
	if err := os.Rename(path, dst); err != nil {
		e.trashMu.Unlock()
		return e.removeFallback(path, err)
	}
	entries := append(e.loadTrash(), TrashEntry{Path: dst, Name: name, DeletedAt: time.Now()})
	e.saveTrash(entries)
	e.trashMu.Unlock()
	e.Logf("trashed %q (%d day retention)", name, days)
	return nil
}

func (e *Engine) removeFallback(path string, cause error) error {
	e.Logf("trash move failed (%v), deleting outright", cause)
	return os.RemoveAll(path)
}

// rootOf returns the storage root containing path ("" when none).
func (e *Engine) rootOf(path string) string {
	for _, d := range e.Disks() {
		if underRoot(filepath.Clean(d), path) {
			return filepath.Clean(d)
		}
	}
	return ""
}

// purgeTrash deletes trashed data older than the configured retention and
// forgets entries whose directory vanished.
func (e *Engine) purgeTrash() {
	days := e.View().TrashDays
	e.trashMu.Lock()
	defer e.trashMu.Unlock()
	entries := e.loadTrash()
	if len(entries) == 0 {
		return
	}
	kept := entries[:0]
	changed := false
	for _, en := range entries {
		expired := days <= 0 || time.Since(en.DeletedAt) > time.Duration(days)*24*time.Hour
		if expired {
			if err := os.RemoveAll(en.Path); err != nil {
				e.Logf("trash purge %s: %v", en.Path, err)
				kept = append(kept, en)
				continue
			}
			e.Logf("trash purged %q", en.Name)
			changed = true
			continue
		}
		if _, err := os.Stat(en.Path); err != nil {
			changed = true // directory gone; drop the entry
			continue
		}
		kept = append(kept, en)
	}
	if changed || len(kept) != len(entries) {
		e.saveTrash(kept)
	}
}

// EmptyTrash deletes every trash entry under root ("" = all roots) right away.
func (e *Engine) EmptyTrash(root string) int {
	e.trashMu.Lock()
	defer e.trashMu.Unlock()
	entries := e.loadTrash()
	kept := entries[:0]
	removed := 0
	for _, en := range entries {
		if root == "" || underRoot(filepath.Clean(root), en.Path) {
			if err := os.RemoveAll(en.Path); err != nil {
				e.Logf("trash empty %s: %v", en.Path, err)
				kept = append(kept, en)
				continue
			}
			removed++
			continue
		}
		kept = append(kept, en)
	}
	e.saveTrash(kept)
	return removed
}

// ---------- rule-based categorization ----------

// MatchCatRule returns the category of the first auto-category rule whose
// (case-insensitive) pattern matches name. Invalid patterns are skipped.
// Returns "" when no rule matches.
func (e *Engine) MatchCatRule(name string) string {
	rules := e.View().CatRules
	for _, r := range rules {
		if r.Pattern == "" || r.Category == "" {
			continue
		}
		re, err := regexp.Compile(`(?i)` + r.Pattern)
		if err != nil {
			continue
		}
		if re.MatchString(name) {
			return r.Category
		}
	}
	return ""
}

// SetCategory updates only a torrent's category label (no file move). Used to
// auto-file magnets once their metadata arrives.
func (e *Engine) SetCategory(id, category string) error {
	e.mu.Lock()
	t, ok := e.torrents[id]
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("torrent not found")
	}
	t.mu.Lock()
	t.Category = category
	t.mu.Unlock()
	return nil
}

// AutoFileMagnet fills in the folder of a magnet torrent the moment its
// metadata arrives: when the user gave no explicit folder, classify decides.
func (e *Engine) AutoFileMagnet(id string, classify func(name string, files []metainfo.File) string) error {
	e.mu.Lock()
	t, ok := e.torrents[id]
	e.mu.Unlock()
	if !ok || classify == nil {
		return fmt.Errorf("torrent not found")
	}
	t.mu.Lock()
	if t.Category != "" || t.Name == "" {
		t.mu.Unlock()
		return nil
	}
	var files []metainfo.File
	if t.MetaInfo != nil {
		files = t.MetaInfo.Info.Files
	}
	name := t.Name
	t.mu.Unlock()
	t.mu.Lock()
	t.Category = classify(name, files)
	t.mu.Unlock()
	return nil
}
