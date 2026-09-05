package torrente

import (
	"encoding/hex"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"github.com/alplix/digitalis/peerwire"
)

// peerSession represents a live connection to a peer for a specific torrent.
type peerSession struct {
	e             *Engine
	t             *Torrent
	conn          *peerwire.Conn
	addr          string
	peerBitfield  *bitfield

	amChoked       bool   // are we choked by the peer
	peerInterested bool   // is the peer interested in our data

	currentPiece int               // piece being downloaded (-1 = none)
	outstanding  map[int]struct{}  // block begins currently requested for currentPiece
	pendingBlocks int

	bytesDown int64
	bytesUp   int64
	seededPeer bool // we have uploaded at least one block to this peer

	stop chan struct{}
}

func newPeerSession(e *Engine, t *Torrent, c *peerwire.Conn, addr string) *peerSession {
	return &peerSession{
		e:            e,
		t:            t,
		conn:         c,
		addr:         addr,
		amChoked:     true,
		currentPiece: -1,
		outstanding:  make(map[int]struct{}),
		stop:         make(chan struct{}),
	}
}

// bitfield tracks which pieces a remote peer claims to have.
type bitfield struct {
	count int
	bits  []byte
}

func newBitfield(count int) *bitfield { return &bitfield{count: count} }

func (b *bitfield) setFrom(bits []byte) {
	b.bits = make([]byte, (b.count+7)/8)
	copy(b.bits, bits)
}

func (b *bitfield) Has(i int) bool {
	if b.bits == nil {
		return false
	}
	byteIdx := i / 8
	if byteIdx >= len(b.bits) {
		return false
	}
	return b.bits[byteIdx]&(1<<(7-uint(i%8))) != 0
}

func (b *bitfield) Set(i int) {
	if b.bits == nil {
		b.bits = make([]byte, (b.count+7)/8)
	}
	byteIdx := i / 8
	if byteIdx < len(b.bits) {
		b.bits[byteIdx] |= 1 << (7 - uint(i%8))
	}
}

// acceptLoop accepts incoming peer connections on the listener.
func (e *Engine) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go e.handleIncoming(conn)
	}
}

// handleIncoming handles a peer that connected to us. We need to read the
// peer's handshake to discover which torrent it wants, then reply with ours.
func (e *Engine) handleIncoming(conn net.Conn) {
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	pc := peerwire.NewConn(conn, [20]byte{}, e.peerID)

	// Read the remote handshake (they send first when connecting to us).
	hs, err := pc.ReadHandshake()
	if err != nil {
		conn.Close()
		return
	}
	ihHex := hex.EncodeToString(hs.InfoHash[:])

	e.mu.Lock()
	t := e.torrents[ihHex[:16]]
	e.mu.Unlock()
	if t == nil {
		conn.Close()
		return
	}
	if t.InfoHash != ihHex {
		conn.Close()
		return
	}

	// Send our handshake with the matching info hash (same conn/stream).
	pc.SetInfoHash(hs.InfoHash)
	if err := pc.HandshakeOutgoing(); err != nil {
		conn.Close()
		return
	}

	s := newPeerSession(e, t, pc, conn.RemoteAddr().String())
	e.registerSession(s)
	s.run()
}

// connectToPeer dials a peer and runs a download/seeding session.
func (e *Engine) connectToPeer(t *Torrent, ip string, port int) {
	addr := net.JoinHostPort(ip, fmt.Sprintf("%d", port))
	conn, err := net.DialTimeout("tcp", addr, 12*time.Second)
	if err != nil {
		return
	}
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	var ih [20]byte
	if _, err := hex.Decode(ih[:], []byte(t.InfoHash)); err != nil {
		conn.Close()
		return
	}
	pc := peerwire.NewConn(conn, ih, e.peerID)
	hs, err := pc.FullHandshake()
	if err != nil {
		conn.Close()
		return
	}
	if hs.InfoHash != ih {
		conn.Close()
		return
	}

	s := newPeerSession(e, t, pc, addr)
	e.registerSession(s)
	s.run()
}

// registerSession adds a session to the engine and tracks its torrent.
func (e *Engine) registerSession(s *peerSession) {
	e.mu.Lock()
	e.sessions[s] = s.t
	e.mu.Unlock()
	s.t.picker.addPeer(s)

	e.recordConn(s.addr)

	s.t.mu.Lock()
	s.t.PeersConnected = e.sessionCount(s.t.ID)
	s.t.mu.Unlock()
}

// unregisterSession removes a session and cleans up.
func (e *Engine) unregisterSession(s *peerSession) {
	e.mu.Lock()
	if _, ok := e.sessions[s]; ok {
		delete(e.sessions, s)
	}
	e.mu.Unlock()
	s.t.picker.delPeer(s)
	if s.currentPiece >= 0 {
		s.t.picker.release(s.currentPiece)
	}

	s.t.mu.Lock()
	s.t.PeersConnected = e.sessionCount(s.t.ID)
	s.t.mu.Unlock()
}

// run is the message loop for a session.
func (s *peerSession) run() {
	defer func() {
		e := s.e
		e.unregisterSession(s)
		s.conn.Close()
	}()

	// Magnets without fetched metadata have no storage; there is nothing to
	// download or seed yet, so hold the connection without a message loop.
	if s.t.storage == nil {
		return
	}

	s.peerBitfield = newBitfield(s.t.storage.PieceCount())

	// Announce ourselves as interested and unchoke the peer (we share freely).
	if err := s.conn.SendInterested(); err != nil {
		return
	}
	if err := s.conn.SendUnchoke(); err != nil {
		return
	}

	// Send our bitfield so peers know what we have (important for seeding).
	bf := s.t.storage.Bitfield()
	if len(bf) > 0 {
		if err := s.conn.SendBitfield(bf); err != nil {
			return
		}
	}

	keepAlive := time.NewTimer(60 * time.Second)
	defer keepAlive.Stop()

	for {
		select {
		case <-s.stop:
			return
		default:
		}

		s.conn.SetDeadline(time.Now().Add(120 * time.Second))
		msg, err := s.conn.ReadMessage()
		if err != nil {
			return
		}

		switch msg.ID {
		case peerwire.MsgChoke:
			s.amChoked = true

		case peerwire.MsgUnchoke:
			s.amChoked = false
			s.pumpRequests()

		case peerwire.MsgInterested:
			s.peerInterested = true

		case peerwire.MsgNotInterested:
			s.peerInterested = false

		case peerwire.MsgHave:
			if msg.Payload != nil && len(msg.Payload) >= 4 {
				idx := int(msg.Payload[0])<<24 | int(msg.Payload[1])<<16 | int(msg.Payload[2])<<8 | int(msg.Payload[3])
				s.peerBitfield.Set(idx)
				s.t.picker.peerHave(s, idx)
				if s.currentPiece == -1 {
					s.pumpRequests()
				}
			}

		case peerwire.MsgBitfield:
			s.peerBitfield.setFrom(msg.Payload)
			s.t.picker.peerBitfield(s, msg.Payload)
			if s.currentPiece == -1 {
				s.pumpRequests()
			}

		case peerwire.MsgRequest:
			s.handleRequest(msg)

		case peerwire.MsgPiece:
			s.handlePiece(msg)

		case peerwire.MsgCancel:
			// drop the outstanding block tracking for it
			delete(s.outstanding, int(msg.Begin))
			s.pendingBlocks--
		}

		// keep-alive timer reset
		keepAlive.Reset(60 * time.Second)
	}
}

const requestWindow = 16 // blocks in flight per peer (16 * 16KiB = 256 KiB)

// pumpRequests keeps a fixed-size window of requests flowing. It should be
// called whenever we are unchoked or learned about new availability.
func (s *peerSession) pumpRequests() {
	if s.amChoked {
		return
	}
	for s.pendingBlocks < requestWindow {
		if s.currentPiece == -1 {
			idx := s.t.picker.acquire(s)
			if idx < 0 {
				// nothing to download from this peer
				return
			}
			s.currentPiece = idx
		}

		pieceLen := int(s.t.storage.PieceLength(s.currentPiece))
		begin := -1
		for off := 0; off < pieceLen; off += peerwire.BlockSize {
			if _, pending := s.outstanding[off]; pending {
				continue
			}
			begin = off
			break
		}
		if begin == -1 {
			// all blocks requested; wait for pieces to arrive
			return
		}

		length := peerwire.BlockSize
		if begin+length > pieceLen {
			length = pieceLen - begin
		}

		if err := s.conn.SendRequest(uint32(s.currentPiece), uint32(begin), uint32(length)); err != nil {
			return
		}
		s.outstanding[begin] = struct{}{}
		s.pendingBlocks++
	}
}

// handlePiece handles an incoming piece block and verifies whole pieces.
func (s *peerSession) handlePiece(msg *peerwire.Message) {
	if int(msg.Index) != s.currentPiece {
		return
	}
	if _, expected := s.outstanding[int(msg.Begin)]; !expected {
		return
	}
	delete(s.outstanding, int(msg.Begin))
	s.pendingBlocks--

	atomic.AddInt64(&s.bytesDown, int64(len(msg.Payload)))
	e := s.e
	e.recordDown(int64(len(msg.Payload)))
	s.t.mu.Lock()
	s.t.Downloaded += int64(len(msg.Payload))
	if s.t.Downloaded > s.t.TotalWanted {
		s.t.Downloaded = s.t.TotalWanted
	}
	s.t.mu.Unlock()

	if err := s.t.storage.WriteBlock(s.currentPiece, int64(msg.Begin), msg.Payload); err != nil {
		s.abortPiece()
		return
	}

	pieceIndex := s.currentPiece
	if len(s.outstanding) == 0 && s.pendingBlocks == 0 {
		// all blocks of this piece received; verify
		if ok := s.t.storage.VerifyPiece(pieceIndex); !ok {
			e := s.e
			e.Logf("piece %d of %q hash mismatch", pieceIndex, s.t.Name)
			s.abortPiece()
			s.pumpRequests()
			return
		}

		// piece verified
		s.t.picker.release(pieceIndex)
		s.currentPiece = -1

		// tell peers we have it
		_ = s.conn.SendHave(uint32(pieceIndex))

		// notify picker so new downloads continue
		s.t.mu.Lock()
		done := s.t.StoredBytes()
		if done >= s.t.TotalWanted && s.t.TotalWanted > 0 && s.t.State == StateDownloading {
			s.t.State = StateSeeding
			s.t.mu.Unlock()
			go s.e.Announce(s.t, "completed")
			s.e.Logf("%q seeding complete (%d bytes)", s.t.Name, s.t.TotalWanted)
		} else {
			s.t.mu.Unlock()
		}

		s.pumpRequests()
	}
}

// abortPiece frees the current piece reservation so another peer can retry.
func (s *peerSession) abortPiece() {
	if s.currentPiece >= 0 {
		s.t.picker.release(s.currentPiece)
	}
	s.currentPiece = -1
	s.outstanding = make(map[int]struct{})
	s.pendingBlocks = 0
}

// handleRequest serves a block request (we are seeding or sharing).
func (s *peerSession) handleRequest(msg *peerwire.Message) {
	// daily upload limit reached: refuse new uploads until the day resets.
	if s.e.UploadBlocked() {
		return
	}
	idx := int(msg.Index)
	begin := int(msg.Begin)
	length := int(msg.Length)
	if length <= 0 || length > peerwire.BlockSize {
		return
	}
	if !s.t.storage.PiecePresent(idx) {
		return
	}

	data, err := s.t.storage.ReadBlock(idx, int64(begin), length)
	if err != nil {
		return
	}

	// upload rate limiting
	s.e.waitUpload(len(data))

	if err := s.conn.SendPiece(msg.Index, msg.Begin, data); err != nil {
		return
	}
	atomic.AddInt64(&s.bytesUp, int64(len(data)))
	s.e.recordUp(int64(len(data)))
	s.t.mu.Lock()
	s.t.Uploaded += int64(len(data))
	if !s.seededPeer {
		s.seededPeer = true
		s.t.SeededTo++
		if s.t.SeededFirst.IsZero() {
			s.t.SeededFirst = time.Now()
		}
	}
	s.t.LastSeen = time.Now()
	s.t.mu.Unlock()
}