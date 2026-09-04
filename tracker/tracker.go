// Package tracker implements the HTTP and UDP tracker announce protocols
// (BEP-3 HTTP tracker, BEP-15 UDP tracker).
package tracker

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/alplix/digitalis/bencode"
)

// Peer is an announced peer address.
type Peer struct {
	IP   string
	Port int
}

// Response is a tracker announce response.
type Response struct {
	Interval   int
	MinInterval int
	Leechers   int
	Seeders    int
	Peers      []Peer
	Failure    string
	Warning    string
	TrackerID  string
}

// Announce announces to a tracker.
// event: "", "started", "completed", "stopped"
func Announce(announceURL, infoHash, peerID string, port int, uploaded, downloaded, left int64, event string, numwant int) (*Response, error) {
	u, err := url.Parse(announceURL)
	if err != nil {
		return nil, fmt.Errorf("bad tracker url: %w", err)
	}

	if len(infoHash) != 40 {
		return nil, fmt.Errorf("invalid info hash (need hex): %q", infoHash)
	}
	// infoHash is hex; url-encode raw bytes
	ihRaw, err := hexDecode(infoHash)
	if err != nil {
		return nil, err
	}

	switch u.Scheme {
	case "http", "https":
		return announceHTTP(u, ihRaw, peerID, port, uploaded, downloaded, left, event, numwant)
	case "udp":
		return announceUDP(u, ihRaw, peerID, port, uploaded, downloaded, left, event, numwant)
	default:
		return nil, fmt.Errorf("unsupported tracker scheme %q", u.Scheme)
	}
}

func hexDecode(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("odd length hex")
	}
	out := make([]byte, len(s)/2)
	for i := 0; i < len(out); i++ {
		hi, ok1 := unhex(s[i*2])
		lo, ok2 := unhex(s[i*2+1])
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("invalid hex")
		}
		out[i] = hi<<4 | lo
	}
	return out, nil
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

func announceHTTP(u *url.URL, infoHash []byte, peerID string, port int, uploaded, downloaded, left int64, event string, numwant int) (*Response, error) {
	q := url.Values{}
	q.Set("info_hash", string(infoHash))
	q.Set("peer_id", peerID)
	q.Set("port", fmt.Sprintf("%d", port))
	q.Set("uploaded", fmt.Sprintf("%d", uploaded))
	q.Set("downloaded", fmt.Sprintf("%d", downloaded))
	q.Set("left", fmt.Sprintf("%d", left))
	q.Set("compact", "1")
	q.Set("numwant", fmt.Sprintf("%d", numwant))
	if event != "" {
		q.Set("event", event)
	}
	u.RawQuery = q.Encode()

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(u.String())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("tracker returned status %d", resp.StatusCode)
	}
	return parseBencodeResponse(body)
}

func parseBencodeResponse(body []byte) (*Response, error) {
	v, err := bencode.Decode(body)
	if err != nil {
		return nil, fmt.Errorf("tracker decode: %w", err)
	}
	dict, ok := v.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("tracker response not a dict")
	}
	r := &Response{}

	if f, ok := dict["failure reason"]; ok {
		r.Failure = f.(string)
		return r, nil
	}
	if w, ok := dict["warning message"]; ok {
		r.Warning = w.(string)
	}
	if i, ok := dict["interval"]; ok {
		r.Interval = int(toI64(i))
	}
	if i, ok := dict["min interval"]; ok {
		r.MinInterval = int(toI64(i))
	}
	if i, ok := dict["complete"]; ok {
		r.Seeders = int(toI64(i))
	}
	if i, ok := dict["incomplete"]; ok {
		r.Leechers = int(toI64(i))
	}
	if t, ok := dict["tracker id"]; ok {
		r.TrackerID = t.(string)
	}

	if peers, ok := dict["peers"]; ok {
		if s, ok := peers.(string); ok {
			r.Peers, _ = parseCompactPeers(s)
		} else if l, ok := peers.([]interface{}); ok {
			for _, p := range l {
				pm, ok := p.(map[string]interface{})
				if !ok {
					continue
				}
				peer := Peer{}
				if ip, ok := pm["ip"]; ok {
					peer.IP = ip.(string)
				}
				if port, ok := pm["port"]; ok {
					peer.Port = int(toI64(port))
				}
				r.Peers = append(r.Peers, peer)
			}
		}
	}
	return r, nil
}

func toI64(v interface{}) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

// parseCompactPeers parses the compact (6-byte) peers representation.
func parseCompactPeers(s string) ([]Peer, error) {
	if len(s)%6 != 0 {
		return nil, fmt.Errorf("compact peers length not multiple of 6")
	}
	var peers []Peer
	for i := 0; i < len(s); i += 6 {
		ip := net.IPv4(s[i], s[i+1], s[i+2], s[i+3]).String()
		port := int(binary.BigEndian.Uint16([]byte{s[i+4], s[i+5]}))
		peers = append(peers, Peer{IP: ip, Port: port})
	}
	return peers, nil
}

// --- UDP tracker (BEP-15) ---

const (
	udpProtocolID uint64 = 0x41727101980
	udpConnect     = 0
	udpAnnounce    = 1
	udpScrape      = 2
)

func announceUDP(u *url.URL, infoHash []byte, peerID string, port int, uploaded, downloaded, left int64, event string, numwant int) (*Response, error) {
	host := u.Host
	if u.Port() == "" {
		host += ":80"
		fmt.Println("udp tracker without port, defaulting to 80")
	}
	conn, err := net.DialTimeout("udp", host, 5*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	// Connect request
	connectReq := make([]byte, 16)
	binary.BigEndian.PutUint64(connectReq[0:8], udpProtocolID)
	binary.BigEndian.PutUint32(connectReq[8:12], udpConnect)
	transactionID := uint32(time.Now().UnixNano() & 0xFFFFFFFF)
	binary.BigEndian.PutUint32(connectReq[12:16], transactionID)
	if _, err := conn.Write(connectReq); err != nil {
		return nil, err
	}

	resp := make([]byte, 65536)
	n, err := conn.Read(resp)
	if err != nil {
		return nil, err
	}
	if n < 16 {
		return nil, fmt.Errorf("udp connect response too short")
	}
	respAction := binary.BigEndian.Uint32(resp[0:4])
	respTransID := binary.BigEndian.Uint32(resp[4:8])
	if respAction != 0 || respTransID != transactionID {
		return nil, fmt.Errorf("udp connect failed (action=%d)", respAction)
	}
	connectionID := binary.BigEndian.Uint64(resp[8:16])

	// Announce request
	announceReq := make([]byte, 98)
	binary.BigEndian.PutUint64(announceReq[0:8], connectionID)
	binary.BigEndian.PutUint32(announceReq[8:12], udpAnnounce)
	binary.BigEndian.PutUint32(announceReq[12:16], transactionID)
	copy(announceReq[16:36], infoHash)
	copy(announceReq[36:56], peerID)
	binary.BigEndian.PutUint64(announceReq[56:64], uint64(downloaded))
	binary.BigEndian.PutUint64(announceReq[64:72], uint64(left))
	binary.BigEndian.PutUint64(announceReq[72:80], uint64(uploaded))
	// event: 0 none, 1 completed, 2 started, 3 stopped
	var eventCode uint32
	switch event {
	case "completed":
		eventCode = 1
	case "started":
		eventCode = 2
	case "stopped":
		eventCode = 3
	}
	binary.BigEndian.PutUint32(announceReq[80:84], eventCode)
	// IP (0 = default)
	binary.BigEndian.PutUint32(announceReq[84:88], 0)
	// key (random)
	binary.BigEndian.PutUint32(announceReq[88:92], transactionID)
	// numwant
	binary.BigEndian.PutUint32(announceReq[92:96], uint32(numwant))
	binary.BigEndian.PutUint16(announceReq[96:98], uint16(port))
	if _, err := conn.Write(announceReq); err != nil {
		return nil, err
	}

	n, err = conn.Read(resp)
	if err != nil {
		return nil, err
	}
	if n < 20 {
		return nil, fmt.Errorf("udp announce response too short")
	}
	respAction = binary.BigEndian.Uint32(resp[0:4])
	if respAction == 3 { // error
		return nil, fmt.Errorf("udp tracker error: %s", string(resp[8:n]))
	}
	if respAction != 1 {
		return nil, fmt.Errorf("udp announce unexpected action %d", respAction)
	}
	r := &Response{}
	r.Interval = int(binary.BigEndian.Uint32(resp[8:12]))
	r.Leechers = int(binary.BigEndian.Uint32(resp[12:16]))
	r.Seeders = int(binary.BigEndian.Uint32(resp[16:20]))
	if n > 20 {
		peersData := resp[20:n]
		for i := 0; i+6 <= len(peersData); i += 6 {
			ip := net.IPv4(peersData[i], peersData[i+1], peersData[i+2], peersData[i+3]).String()
			port := int(binary.BigEndian.Uint16(peersData[i+4 : i+6]))
			r.Peers = append(r.Peers, Peer{IP: ip, Port: port})
		}
	}
	return r, nil
}
