package torrente

import (
	"encoding/binary"
	"net"
	"strconv"
	"time"

	"github.com/alplix/digitalis/bencode"
	"github.com/alplix/digitalis/peerwire"
)

// BEP-10 extension message ids. 0 is the extended handshake, 1 is reserved
// for LTEP message id announcements, so we use 2 (ut_metadata) and 3 (ut_pex).
const (
	extHandshakeID byte = 0
	utMetadataID   byte = 2
	utPexID        byte = 3
)

// metadataChunkSize is the BEP-9 chunk size for metadata pieces.
const metadataChunkSize = 16 * 1024

// maxMetaSize caps the accepted metadata size to keep memory bounded.
const maxMetaSize = 4 * 1024 * 1024

func toInt64(v interface{}) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case int32:
		return int64(x)
	case int16:
		return int64(x)
	case int8:
		return int64(x)
	case uint64:
		return int64(x)
	case uint32:
		return int64(x)
	case uint16:
		return int64(x)
	case uint8:
		return int64(x)
	}
	return 0
}

func toInt64Any(v interface{}) int64 {
	if v == nil {
		return 0
	}
	return toInt64(v)
}

// sendExtendedHandshake advertises our extended protocol capabilities.
func (s *peerSession) sendExtendedHandshake() error {
	d := map[string]interface{}{
		"m": map[string]interface{}{
			"ut_metadata": int64(utMetadataID),
			"ut_pex":      int64(utPexID),
		},
		"p":    int64(s.e.port),
		"v":    "Digitalis",
		"reqq": int64(64),
	}
	s.t.mu.Lock()
	if s.t.MetaInfo != nil {
		d["metadata_size"] = int64(len(s.t.MetaInfo.Info.Raw))
	}
	s.t.mu.Unlock()
	body, err := bencode.Encode(d)
	if err != nil {
		return err
	}
	return s.conn.SendExtended(extHandshakeID, body)
}

// handleExtended routes BEP-10 messages.
func (s *peerSession) handleExtended(msg *peerwire.Message) {
	switch msg.ExtendedID {
	case extHandshakeID:
		s.handlePeerExtHandshake(msg.Payload)
	case s.peerMetaID:
		s.handleUTMetadata(msg.Payload)
	case s.peerPexID:
		s.handlePex(msg.Payload)
	}
}

// handlePeerExtHandshake reads the peer's extended handshake dict.
func (s *peerSession) handlePeerExtHandshake(payload []byte) {
	v, err := bencode.Decode(payload)
	if err != nil {
		return
	}
	d, ok := v.(map[string]interface{})
	if !ok {
		return
	}
	if mm, ok := d["m"].(map[string]interface{}); ok {
		if id, ok := mm["ut_metadata"]; ok {
			s.peerMetaID = byte(toInt64(id))
		}
		if id, ok := mm["ut_pex"]; ok {
			s.peerPexID = byte(toInt64(id))
		}
	}
	if mv, ok := d["metadata_size"]; ok {
		size := toInt64(mv)
		s.t.mu.Lock()
		if s.t.MetaSize == 0 {
			s.t.MetaSize = size
		}
		s.t.mu.Unlock()
	}
	if s.t.storage == nil && s.peerMetaID != 0 {
		s.requestMetaPieces()
	}
	if s.t.storage != nil && s.peerPexID != 0 {
		s.sendPexMessage()
	}
}

// requestMetaPieces asks the peer for the BEP-9 metadata chunks we are missing.
func (s *peerSession) requestMetaPieces() {
	if s.t.storage != nil || s.peerMetaID == 0 {
		return
	}
	t := s.t
	t.mu.Lock()
	size := t.MetaSize
	if size <= 0 || size > maxMetaSize {
		t.mu.Unlock()
		return
	}
	have := make(map[int][]byte, len(t.MetaChunks))
	for k, v := range t.MetaChunks {
		have[k] = v
	}
	t.mu.Unlock()

	now := time.Now()
	for i, at := range s.metaReqAt {
		if now.Sub(at) > 15*time.Second {
			delete(s.metaReqAt, i)
		}
	}

	total := int((size + metadataChunkSize - 1) / metadataChunkSize)
	inflight := 0
	for i := 0; i < total && inflight < 4; i++ {
		if _, ok := have[i]; ok {
			continue
		}
		if _, pending := s.metaReqAt[i]; pending {
			continue
		}
		body, err := bencode.Encode(map[string]interface{}{
			"msg_type": int64(0),
			"piece":    int64(i),
		})
		if err != nil {
			return
		}
		if err := s.conn.SendExtended(s.peerMetaID, body); err != nil {
			return
		}
		s.metaReqAt[i] = now
		inflight++
	}
}

// handleUTMetadata handles a BEP-9 ut_metadata message.
func (s *peerSession) handleUTMetadata(payload []byte) {
	v, used, err := bencode.DecodePrefix(payload)
	if err != nil {
		return
	}
	d, ok := v.(map[string]interface{})
	if !ok {
		return
	}
	msgType := toInt64Any(d["msg_type"])
	piece := int(toInt64Any(d["piece"]))

	if mv, ok := d["metadata_size"]; ok && msgType == 1 {
		size := toInt64(mv)
		s.t.mu.Lock()
		if s.t.MetaSize == 0 {
			s.t.MetaSize = size
		}
		s.t.mu.Unlock()
	}

	switch msgType {
	case 0: // peer requests our metadata
		if s.t.storage != nil {
			s.serveMetaPiece(piece)
		}
	case 1: // data
		s.recvMetaPiece(piece, payload[used:])
	case 2: // reject
		delete(s.metaReqAt, piece)
		s.requestMetaPieces()
	}
}

// serveMetaPiece answers a peer's ut_metadata request with a chunk of our info
// dict.
func (s *peerSession) serveMetaPiece(piece int) {
	s.t.mu.Lock()
	raw := append([]byte(nil), s.t.MetaInfo.Info.Raw...)
	s.t.mu.Unlock()
	if len(raw) == 0 {
		return
	}
	start := piece * metadataChunkSize
	if start >= len(raw) {
		return
	}
	end := start + metadataChunkSize
	if end > len(raw) {
		end = len(raw)
	}
	header, err := bencode.Encode(map[string]interface{}{
		"msg_type": int64(1),
		"piece":    int64(piece),
	})
	if err != nil {
		return
	}
	body := append(header, raw[start:end]...)
	_ = s.conn.SendExtended(s.peerMetaID, body)
}

// recvMetaPiece stores a metadata chunk and, when complete, upgrades the magnet
// into a fully metadata'd torrent.
func (s *peerSession) recvMetaPiece(piece int, raw []byte) {
	delete(s.metaReqAt, piece)
	t := s.t

	t.mu.Lock()
	if t.storage != nil {
		t.mu.Unlock()
		return
	}
	size := t.MetaSize
	if size <= 0 {
		t.mu.Unlock()
		return
	}
	if piece < 0 || int64(piece)*metadataChunkSize >= size || len(raw) > metadataChunkSize {
		t.mu.Unlock()
		return
	}
	// The final chunk is shorter than metadataChunkSize.
	suffix := size - int64(piece)*metadataChunkSize
	expected := metadataChunkSize
	if suffix < metadataChunkSize {
		expected = int(suffix)
	}
	if len(raw) != expected {
		t.mu.Unlock()
		return
	}
	if t.MetaChunks == nil {
		t.MetaChunks = make(map[int][]byte)
	}
	if _, dup := t.MetaChunks[piece]; dup {
		t.mu.Unlock()
		s.requestMetaPieces()
		return
	}
	t.MetaChunks[piece] = append([]byte(nil), raw...)
	complete := t.metaAssembledUnlocked()
	t.mu.Unlock()

	if complete != nil {
		if err := s.e.upgradeMagnet(t, complete); err != nil {
			s.e.Logf("magnet metadata from %s invalid: %v", s.addr, err)
			// A corrupt assembly is dropped so other peers can serve us fresh
			// chunks instead of looping forever on bad bytes.
			t.mu.Lock()
			t.MetaChunks = nil
			s.metaReqAt = make(map[int]time.Time)
			t.mu.Unlock()
		}
	}
	s.requestMetaPieces()
}

// metaAssembledUnlocked returns the full metadata if every chunk is present.
func (t *Torrent) metaAssembledUnlocked() []byte {
	size := t.MetaSize
	if size <= 0 || t.MetaChunks == nil {
		return nil
	}
	total := int((size + metadataChunkSize - 1) / metadataChunkSize)
	if len(t.MetaChunks) < total {
		return nil
	}
	buf := make([]byte, size)
	off := 0
	for i := int64(0); i < int64(total); i++ {
		ch, ok := t.MetaChunks[int(i)]
		if !ok {
			return nil
		}
		copy(buf[off:], ch)
		off += len(ch)
	}
	return buf
}

// sendPexMessage advertises peers we are connected to via BEP-11 ut_pex.
func (s *peerSession) sendPexMessage() {
	if s.peerPexID == 0 {
		return
	}
	e := s.e
	e.mu.Lock()
	var added []byte
	for sess, tr := range e.sessions {
		if tr.ID != s.t.ID || sess == s {
			continue
		}
		ipa := compactPeerAddr(sess.addr)
		if ipa == nil {
			continue
		}
		added = append(added, ipa...)
	}
	e.mu.Unlock()
	if len(added) == 0 {
		return
	}
	payload, err := bencode.Encode(map[string]interface{}{
		"added": added,
	})
	if err != nil {
		return
	}
	_ = s.conn.SendExtended(s.peerPexID, payload)
}

// handlePex consumes a BEP-11 ut_pex message and schedules dials.
func (s *peerSession) handlePex(payload []byte) {
	v, _, err := bencode.DecodePrefix(payload)
	if err != nil {
		return
	}
	d, ok := v.(map[string]interface{})
	if !ok {
		return
	}
	var addrs []string
	if av, ok := d["added"].(string); ok {
		for i := 0; i+6 <= len(av); i += 6 {
			b := av[i : i+6]
			ip := net.IPv4(b[0], b[1], b[2], b[3])
			port := binary.BigEndian.Uint16([]byte{b[4], b[5]})
			addrs = append(addrs, net.JoinHostPort(ip.String(), strconv.Itoa(int(port))))
		}
	}
	if len(addrs) > 0 {
		s.e.connectDiscovered(s.t, addrs)
	}
}

// compactPeerAddr encodes "ip:port" as BEP-11 compact 6-byte form (IPv4 only).
func compactPeerAddr(addr string) []byte {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil
	}
	ip := net.ParseIP(host)
	ip4 := ip.To4()
	if ip4 == nil {
		return nil
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return nil
	}
	out := make([]byte, 6)
	copy(out, ip4)
	binary.BigEndian.PutUint16(out[4:], uint16(port))
	return out
}