package torrente

import (
	"math/rand"
	"sync"
)

// picker selects pieces for each peer session to download.
// A piece is "reserved" to at most one session at a time so that
// multiple peers do not duplicate the same piece; this also lets us
// pipeline all blocks of a piece over a single unchoked connection.
type picker struct {
	mu            sync.Mutex
	t             *Torrent
	reserved      map[int]*peerSession              // piece index -> session downloading it
	peerHaveCount map[int]int                       // piece index -> count of connected peers that have it
	coveredBy     map[*peerSession]map[int]struct{} // reverse index for cleanup
	priorityLo    int                               // inclusive active streaming window (-1 = off)
	priorityHi    int
	skip          []bool // piece -> not wanted (selective download); nil = all wanted
}

func newPicker(t *Torrent) *picker {
	return &picker{
		t:             t,
		reserved:      make(map[int]*peerSession),
		peerHaveCount: make(map[int]int),
		coveredBy:     make(map[*peerSession]map[int]struct{}),
		priorityLo:    -1,
		priorityHi:    -1,
	}
}

// SetPriority marks an inclusive piece range as the active streaming priority.
// The picker will prefer missing pieces of this window in order until the whole
// window is downloaded, then the window is cleared automatically.
func (p *picker) SetPriority(lo, hi int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if hi < lo {
		p.priorityLo, p.priorityHi = -1, -1
		return
	}
	total := p.t.storage.PieceCount()
	if total > 0 {
		if lo < 0 {
			lo = 0
		}
		if hi >= total {
			hi = total - 1
		}
	}
	p.priorityLo, p.priorityHi = lo, hi
}

// PriorityWindow returns the current priority range, or (-1,-1) if off.
func (p *picker) PriorityWindow() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.priorityLo, p.priorityHi
}

// setSkip installs the "not wanted" piece mask (selective download).
func (p *picker) setSkip(mask []bool) {
	p.mu.Lock()
	p.skip = mask
	p.mu.Unlock()
}

// skipMask returns the current mask (nil = everything wanted).
func (p *picker) skipMask() []bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.skip
}

// addPeer registers a session. After the session's bitfield/have messages
// it should call peerBitfield/peerHave to populate piece coverage.
func (p *picker) addPeer(s *peerSession) {
	p.mu.Lock()
	if _, exists := p.coveredBy[s]; !exists {
		p.coveredBy[s] = make(map[int]struct{})
	}
	p.mu.Unlock()
}

// delPeer removes a session and all its piece coverage.
func (p *picker) delPeer(s *peerSession) {
	p.mu.Lock()
	defer p.mu.Unlock()
	set, ok := p.coveredBy[s]
	if !ok {
		return
	}
	for piece := range set {
		p.peerHaveCount[piece]--
		if p.peerHaveCount[piece] < 0 {
			p.peerHaveCount[piece] = 0
		}
	}
	delete(p.coveredBy, s)
	// release reservation if it was this session's
	if cur, reserved := p.reservedFor(s); reserved {
		delete(p.reserved, cur)
	}
}

// reservedFor finds the piece reserved to s, or -1.
func (p *picker) reservedFor(s *peerSession) (int, bool) {
	for i, owner := range p.reserved {
		if owner == s {
			return i, true
		}
	}
	return -1, false
}

// peerBitfield registers all pieces a peer has from its bitfield.
func (p *picker) peerBitfield(s *peerSession, bitfield []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	set := p.coveredBy[s]
	if set == nil {
		set = make(map[int]struct{})
		p.coveredBy[s] = set
	}
	n := p.t.storage.PieceCount()
	for i := 0; i < n; i++ {
		byteIdx := i / 8
		if byteIdx >= len(bitfield) {
			break
		}
		if bitfield[byteIdx]&(1<<(7-uint(i%8))) != 0 {
			if _, exists := set[i]; !exists {
				set[i] = struct{}{}
				p.peerHaveCount[i]++
			}
		}
	}
}

// peerHave registers a single piece from a have message.
func (p *picker) peerHave(s *peerSession, idx int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	set := p.coveredBy[s]
	if set == nil {
		set = make(map[int]struct{})
		p.coveredBy[s] = set
	}
	if _, exists := set[idx]; !exists {
		set[idx] = struct{}{}
		p.peerHaveCount[idx]++
	}
}

// acquire selects the best piece for a session: the rarest piece that the
// peer has, that we don't have, and that isn't already reserved. Returns -1
// if none available.
func (p *picker) acquire(s *peerSession) int {
	if s.peerBitfield == nil {
		return -1
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	total := p.t.storage.PieceCount()
	if total < 30 {
		for i := 0; i < total; i++ {
			if p.skip != nil && p.skip[i] {
				continue
			}
			if !p.t.storage.PiecePresent(i) {
				if _, reserved := p.reserved[i]; !reserved {
					if pr := s.peerBitfield.Has(i); pr {
						p.reserved[i] = s
						return i
					}
				}
			}
		}
		return -1
	}

	// Streaming priority window first: prefer the earliest missing piece in order.
	if p.priorityLo >= 0 {
		all := true
		for i := p.priorityLo; i <= p.priorityHi; i++ {
			if p.skip != nil && p.skip[i] {
				continue
			}
			if p.t.storage.PiecePresent(i) {
				continue
			}
			all = false
			if _, reserved := p.reserved[i]; reserved {
				continue
			}
			if s.peerBitfield.Has(i) {
				p.reserved[i] = s
				return i
			}
		}
		if all {
			p.priorityLo, p.priorityHi = -1, -1 // window completed -> clear
		}
	}

	// Rare-first for larger torrents.
	var candidates []int
	best := int(^uint(0) >> 1)
	for i := 0; i < total; i++ {
		if p.skip != nil && p.skip[i] {
			continue
		}
		if p.t.storage.PiecePresent(i) {
			continue
		}
		if _, reserved := p.reserved[i]; reserved {
			continue
		}
		if !s.peerBitfield.Has(i) {
			continue
		}
		c := p.peerHaveCount[i]
		if c < best {
			best = c
			candidates = candidates[:0]
			candidates = append(candidates, i)
		} else if c == best && len(candidates) > 0 {
			if rand.Intn(len(candidates)+1) == 0 {
				candidates = append(candidates, i)
			}
		}
	}
	if len(candidates) == 0 {
		return -1
	}
	idx := candidates[rand.Intn(len(candidates))]
	p.reserved[idx] = s
	return idx
}

// release frees the reservation on a piece if held.
func (p *picker) release(idx int) {
	p.mu.Lock()
	delete(p.reserved, idx)
	p.mu.Unlock()
}
