package torrente

import (
	"fmt"
	"sort"
)

// Selective download: mark single files of a multi-file torrent as "not
// wanted". The picker then never requests their pieces, completion is
// evaluated over wanted pieces only, and the choice is persisted by the web
// layer so it survives restarts.

// SetSkippedFiles updates which file indices are excluded from downloading.
// Requires metadata. Re-evaluates the download-complete state afterwards.
func (e *Engine) SetSkippedFiles(id string, skipped []int) error {
	e.mu.Lock()
	t, ok := e.torrents[id]
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("torrent not found")
	}
	t.mu.Lock()
	if t.MetaInfo == nil || t.storage == nil {
		t.mu.Unlock()
		return fmt.Errorf("metadata not ready")
	}
	n := len(t.MetaInfo.Info.Files)
	if n == 0 {
		t.mu.Unlock()
		return fmt.Errorf("single-file torrent")
	}
	set := make(map[int]bool, len(skipped))
	for _, i := range skipped {
		if i >= 0 && i < n {
			set[i] = true
		}
	}
	t.skippedFiles = set
	mask := t.buildSkipMaskLocked()
	t.mu.Unlock()

	t.picker.setSkip(mask)

	t.mu.Lock()
	if t.State == StateDownloading && t.wantedComplete() {
		t.mu.Unlock()
		t.setState(StateSeeding)
	} else {
		t.mu.Unlock()
	}
	return nil
}

// SkippedFiles returns the excluded file indices sorted ascending.
func (e *Engine) SkippedFiles(id string) []int {
	e.mu.Lock()
	t, ok := e.torrents[id]
	e.mu.Unlock()
	if !ok {
		return []int{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]int, 0, len(t.skippedFiles))
	for i := range t.skippedFiles {
		out = append(out, i)
	}
	sort.Ints(out)
	return out
}

// buildSkipMaskLocked computes the piece-level "not wanted" mask from the
// skipped file set. A piece is skipped only when NO wanted byte overlaps it.
// Caller must hold t.mu.
func (t *Torrent) buildSkipMaskLocked() []bool {
	total := t.storage.PieceCount()
	if total <= 0 || t.MetaInfo == nil || len(t.MetaInfo.Info.Files) == 0 {
		return nil
	}
	pieceLen := t.MetaInfo.Info.PieceLength
	if pieceLen <= 0 {
		return nil
	}
	needed := make([]bool, total)
	var off int64
	for i, f := range t.MetaInfo.Info.Files {
		if !t.skippedFiles[i] && f.Length > 0 {
			start := off / pieceLen
			end := (off + f.Length - 1) / pieceLen
			for p := start; int(p) < total && p <= end; p++ {
				needed[p] = true
			}
		}
		off += f.Length
	}
	mask := make([]bool, total)
	for i := range mask {
		mask[i] = !needed[i]
	}
	return mask
}
