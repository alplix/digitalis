// Package peerwire implements the BitTorrent Peer Wire Protocol (BEP-3).
package peerwire

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

const (
	ProtocolString = "BitTorrent protocol"

	MsgChoke         = 0
	MsgUnchoke       = 1
	MsgInterested    = 2
	MsgNotInterested = 3
	MsgHave          = 4
	MsgBitfield      = 5
	MsgRequest       = 6
	MsgPiece         = 7
	MsgCancel        = 8
	MsgPort          = 9
	MsgExtended      = 20

	MessageIDExtended = 20
	MessageIDEHD      = 21

	BlockSize = 16384 // 16 KiB
)

// BEP-10 extension protocol reserved-bit (bit 20, byte 5 bit 0x10).
const ExtensionBit byte = 0x10

// Message is a decapsulated peer wire message.
type Message struct {
	ID      byte
	Payload []byte
	// ExtendedID is the BEP-10 extension message id (valid when ID == MsgExtended).
	ExtendedID byte
	// For Request / Piece
	Index  uint32
	Begin  uint32
	Length uint32
	// For Have
	HavePiece uint32
}

// Handshake is the initial connection handshake.
type Handshake struct {
	InfoHash [20]byte
	PeerID   [20]byte
}

// Conn represents a peer wire connection stream.
type Conn struct {
	conn             net.Conn
	r                *bufio.Reader
	w                *bufio.Writer
	infoHash         [20]byte
	peerID           [20]byte
	readTimeout      time.Duration
	extensionEnabled bool
}

// NewConn creates a new connection wrapper. The handshake should be performed
// by calling one of the Handshake methods.
func NewConn(c net.Conn, infoHash [20]byte, peerID [20]byte) *Conn {
	return &Conn{
		conn:             c,
		r:                bufio.NewReader(c),
		w:                bufio.NewWriter(c),
		infoHash:         infoHash,
		peerID:           peerID,
		readTimeout:      60 * time.Second,
		extensionEnabled: true,
	}
}

// SetDeadline sets read/write deadline on the underlying connection.
func (c *Conn) SetDeadline(d time.Time) error      { return c.conn.SetDeadline(d) }
func (c *Conn) SetReadDeadline(d time.Time) error  { return c.conn.SetReadDeadline(d) }
func (c *Conn) SetWriteDeadline(d time.Time) error { return c.conn.SetWriteDeadline(d) }

// RemoteAddr returns the remote address.
func (c *Conn) RemoteAddr() net.Addr { return c.conn.RemoteAddr() }

// PeerID returns the remote peer id learned from the handshake.
func (c *Conn) PeerID() [20]byte { return c.peerID }

// Close closes the underlying connection.
func (c *Conn) Close() error { return c.conn.Close() }

// SetInfoHash overrides the info hash used for an outgoing handshake.
func (c *Conn) SetInfoHash(ih [20]byte) { c.infoHash = ih }

// HandshakeOutgoing writes out the handshake to the remote peer.
func (c *Conn) HandshakeOutgoing() error {
	buf := make([]byte, 0, 68)
	buf = append(buf, byte(len(ProtocolString)))
	buf = append(buf, ProtocolString...)
	reserved := make([]byte, 8)
	if c.extensionEnabled {
		reserved[5] |= ExtensionBit
	}
	buf = append(buf, reserved...)
	buf = append(buf, c.infoHash[:]...)
	buf = append(buf, c.peerID[:]...)
	if _, err := c.w.Write(buf); err != nil {
		return err
	}
	return c.w.Flush()
}

// ReadHandshake reads and validates a handshake from the peer.
func (c *Conn) ReadHandshake() (*Handshake, error) {
	// first byte is protocol name length
	lenByte, err := c.r.ReadByte()
	if err != nil {
		return nil, err
	}
	if int(lenByte) != len(ProtocolString) {
		return nil, fmt.Errorf("unexpected protocol length %d", lenByte)
	}
	prot := make([]byte, len(ProtocolString))
	if _, err := io.ReadFull(c.r, prot); err != nil {
		return nil, err
	}
	if string(prot) != ProtocolString {
		return nil, fmt.Errorf("unexpected protocol %q", string(prot))
	}
	// 8 reserved bytes
	reserved := make([]byte, 8)
	if _, err := io.ReadFull(c.r, reserved); err != nil {
		return nil, err
	}
	// BEP-10: bit 20 (byte 5, 0x10) announces extension protocol support.
	c.extensionEnabled = reserved[5]&ExtensionBit != 0
	infoHash := make([]byte, 20)
	if _, err := io.ReadFull(c.r, infoHash); err != nil {
		return nil, err
	}
	peerID := make([]byte, 20)
	if _, err := io.ReadFull(c.r, peerID); err != nil {
		return nil, err
	}
	hs := &Handshake{}
	copy(hs.InfoHash[:], infoHash)
	copy(hs.PeerID[:], peerID)
	return hs, nil
}

// PeerHandshake does a full incoming handshake: reads peer's, then sends ours.
func (c *Conn) PeerHandshake() (*Handshake, error) {
	hs, err := c.ReadHandshake()
	if err != nil {
		return nil, err
	}
	if err := c.HandshakeOutgoing(); err != nil {
		return nil, err
	}
	return hs, nil
}

// FullHandshake performs a full outgoing handshake exchange.
func (c *Conn) FullHandshake() (*Handshake, error) {
	if err := c.HandshakeOutgoing(); err != nil {
		return nil, err
	}
	return c.ReadHandshake()
}

// ErrClosed is returned when the connection is closed.
var ErrClosed = errors.New("peerwire: connection closed")

// ReadMessage reads a single message. Handshake must have been done.
func (c *Conn) ReadMessage() (*Message, error) {
	c.SetReadDeadline(time.Now().Add(c.readTimeout))
	var lenBuf [4]byte
	if _, err := io.ReadFull(c.r, lenBuf[:]); err != nil {
		if err == io.EOF {
			return nil, ErrClosed
		}
		return nil, err
	}
	msgLen := binary.BigEndian.Uint32(lenBuf[:])
	if msgLen == 0 {
		// keep-alive
		return &Message{ID: 0xFF}, nil
	}
	// Guard against absurd lengths
	if msgLen > 2*1024*1024 {
		return nil, fmt.Errorf("message too large: %d", msgLen)
	}
	payload := make([]byte, msgLen)
	if _, err := io.ReadFull(c.r, payload); err != nil {
		return nil, err
	}
	msg := &Message{
		ID:      payload[0],
		Payload: payload[1:],
	}
	switch msg.ID {
	case MsgHave:
		if len(msg.Payload) >= 4 {
			msg.HavePiece = binary.BigEndian.Uint32(msg.Payload[:4])
		}
	case MsgRequest, MsgCancel:
		if len(msg.Payload) >= 12 {
			msg.Index = binary.BigEndian.Uint32(msg.Payload[0:4])
			msg.Begin = binary.BigEndian.Uint32(msg.Payload[4:8])
			msg.Length = binary.BigEndian.Uint32(msg.Payload[8:12])
		}
	case MsgPiece:
		if len(msg.Payload) >= 8 {
			msg.Index = binary.BigEndian.Uint32(msg.Payload[0:4])
			msg.Begin = binary.BigEndian.Uint32(msg.Payload[4:8])
			msg.Payload = msg.Payload[8:]
		}
	case MsgExtended:
		if len(msg.Payload) >= 1 {
			msg.ExtendedID = msg.Payload[0]
			msg.Payload = msg.Payload[1:]
		}
	}
	return msg, nil
}

// writeMessage writes a message to the wire.
func (c *Conn) writeMessage(id byte, payload []byte) error {
	length := uint32(len(payload) + 1) // +1 for id byte
	buf := make([]byte, 4+length)
	binary.BigEndian.PutUint32(buf[0:4], length)
	buf[4] = id
	copy(buf[5:], payload)
	if _, err := c.w.Write(buf); err != nil {
		return err
	}
	return c.w.Flush()
}

// SendChoke sends a choke message.
func (c *Conn) SendChoke() error { return c.writeMessage(MsgChoke, nil) }

// SendUnchoke sends an unchoke message.
func (c *Conn) SendUnchoke() error { return c.writeMessage(MsgUnchoke, nil) }

// SendInterested sends an interested message.
func (c *Conn) SendInterested() error { return c.writeMessage(MsgInterested, nil) }

// SendNotInterested sends a not-interested message.
func (c *Conn) SendNotInterested() error { return c.writeMessage(MsgNotInterested, nil) }

// SendHave sends a have message for the given piece index.
func (c *Conn) SendHave(piece uint32) error {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, piece)
	return c.writeMessage(MsgHave, payload)
}

// SendBitfield sends a bitfield message.
func (c *Conn) SendBitfield(bitfield []byte) error {
	return c.writeMessage(MsgBitfield, bitfield)
}

// SendRequest sends a request message.
func (c *Conn) SendRequest(index, begin, length uint32) error {
	payload := make([]byte, 12)
	binary.BigEndian.PutUint32(payload[0:4], index)
	binary.BigEndian.PutUint32(payload[4:8], begin)
	binary.BigEndian.PutUint32(payload[8:12], length)
	return c.writeMessage(MsgRequest, payload)
}

// SendPiece sends a piece message.
func (c *Conn) SendPiece(index, begin uint32, data []byte) error {
	payload := make([]byte, 8+len(data))
	binary.BigEndian.PutUint32(payload[0:4], index)
	binary.BigEndian.PutUint32(payload[4:8], begin)
	copy(payload[8:], data)
	return c.writeMessage(MsgPiece, payload)
}

// SendCancel sends a cancel message.
func (c *Conn) SendCancel(index, begin, length uint32) error {
	payload := make([]byte, 12)
	binary.BigEndian.PutUint32(payload[0:4], index)
	binary.BigEndian.PutUint32(payload[4:8], begin)
	binary.BigEndian.PutUint32(payload[8:12], length)
	return c.writeMessage(MsgCancel, payload)
}

// SendKeepAlive sends a keep-alive message (zero length).
func (c *Conn) SendKeepAlive() error {
	buf := make([]byte, 4)
	if _, err := c.w.Write(buf); err != nil {
		return err
	}
	return c.w.Flush()
}

// SendExtended sends a BEP-10 extended message to the peer.
func (c *Conn) SendExtended(extID byte, body []byte) error {
	payload := make([]byte, 0, len(body)+1)
	payload = append(payload, extID)
	payload = append(payload, body...)
	return c.writeMessage(MsgExtended, payload)
}

// SupportsExtension reports whether the handshake negotiated BEP-10.
func (c *Conn) SupportsExtension() bool { return c.extensionEnabled }

// EnableExtension opts into sending the BEP-10 reserved bit in our handshake.
func (c *Conn) EnableExtension() { c.extensionEnabled = true }

// BitfieldFromIndexes builds a bitfield from a sorted list of piece indexes.
func BitfieldFromIndexes(count int, have map[int]bool) []byte {
	bitfieldLen := (count + 7) / 8
	bf := make([]byte, bitfieldLen)
	for i := 0; i < count; i++ {
		if have[i] {
			bf[i/8] |= 1 << (7 - uint(i%8))
		}
	}
	return bf
}

// BitfieldHasPiece reports whether bitfield has piece i (i 0-based).
func BitfieldHasPiece(bitfield []byte, i int) bool {
	byteIdx := i / 8
	if byteIdx >= len(bitfield) {
		return false
	}
	return bitfield[byteIdx]&(1<<(7-uint(i%8))) != 0
}
