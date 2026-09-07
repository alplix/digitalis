package torrente

import "time"

// sequentialLoop advances the piece-priority window of every torrent in
// sequential mode so downloads proceed roughly in order. It runs continuously
// and only touches torrents that opt in, so normal (rare-first) picking is
// unaffected. When a sequential torrent finishes, the window clears itself in
// the picker and the loop resets it to the first still-missing piece, which
// is no piece, which is fine.
func (e *Engine) sequentialLoop() {
	for {
		time.Sleep(3 * time.Second)
		e.mu.Lock()
		torrents := make([]*Torrent, 0, len(e.torrents))
		for _, t := range e.torrents {
			torrents = append(torrents, t)
		}
		e.mu.Unlock()
		for _, t := range torrents {
			e.advanceSequential(t)
		}
	}
}

// advanceSequential updates the priority window of one torrent to start at the
// earliest missing piece, covering a small look-ahead (sequWin pieces). The
// picker itself is responsible for clearing the window once fully present.
func (e *Engine) advanceSequential(t *Torrent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.Sequential || t.picker == nil || t.storage == nil {
		if t.Sequential && t.picker != nil {
			t.picker.SetPriority(-1, -1)
		}
		return
	}
	if t.State != StateDownloading && t.State != StateQueued {
		return
	}
	total := t.storage.PieceCount()
	if total <= 0 {
		return
	}
	win := 24
	if win > total {
		win = total
	}
	// Find the first missing piece. If none are missing (fully downloaded,
	// metadata just resolved), hold the window open at piece 0.
	start := -1
	for i := 0; i < total; i++ {
		if t.storage.PiecePresent(i) {
			continue
		}
		start = i
		break
	}
	if start < 0 {
		t.picker.SetPriority(0, 0)
		return
	}
	hi := start + win - 1
	if hi >= total {
		hi = total - 1
	}
	t.picker.SetPriority(start, hi)
}