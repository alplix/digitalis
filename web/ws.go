package web

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"time"
)

// wsGUID is the RFC 6455 fixed GUID used to compute Sec-WebSocket-Accept.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// wsClient is a single connected WebSocket peer.
type wsClient struct {
	conn net.Conn
	send chan []byte
}

// wsHub tracks connected WebSocket clients.
type wsHub struct {
	mu      sync.Mutex
	clients map[*wsClient]bool
}

func newWSHub() *wsHub {
	return &wsHub{clients: make(map[*wsClient]bool)}
}

func (h *wsHub) register(c *wsClient) {
	h.mu.Lock()
	h.clients[c] = true
	h.mu.Unlock()
}

func (h *wsHub) unregister(c *wsClient) {
	h.mu.Lock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c.send)
	}
	h.mu.Unlock()
}

func (h *wsHub) broadcast(payload []byte) {
	frame := frameText(payload)
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		select {
		case c.send <- frame:
		default: // slow client, drop tick
		}
	}
}

// wsTorrent is the compact per-torrent live entry.
type wsTorrent struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	State     string  `json:"state"`
	Progress  float64 `json:"progress"`
	Downloaded int64  `json:"downloaded"`
	Uploaded  int64   `json:"uploaded"`
	Size      int64   `json:"size"`
	DL        int64   `json:"dl"`
	UL        int64   `json:"ul"`
	Seeders   int     `json:"seeders"`
	Leechers  int     `json:"leechers"`
	Peers     int     `json:"peers"`
	PH        int     `json:"ph"`
	PT        int     `json:"pt"`
	Ratio     float64 `json:"ratio"`
	Cat       string  `json:"cat"`
}

// wsMsg is the JSON payload broadcast every tick.
type wsMsg struct {
	Type       string       `json:"type"`
	Up         int64        `json:"up"`
	Down       int64        `json:"down"`
	UpTotal    int64        `json:"up_total"`
	DownTotal  int64        `json:"down_total"`
	UpToday    int64        `json:"up_today"`
	DownToday  int64        `json:"down_today"`
	Conns      int64        `json:"conns"`
	Active     int          `json:"active"`
	Paused     bool         `json:"paused"`
	DailyLimit int64        `json:"daily_limit"`
	Torrents   []wsTorrent  `json:"torrents"`
}

// wsTicker builds the live snapshot sent to connected clients.
func (s *Server) wsTicker() []byte {
	ts := s.engine.Torrents()
	var sumUp, sumDn int64
	list := make([]wsTorrent, 0, len(ts))
	for _, t := range ts {
		sumUp += t.UploadSpeed
		sumDn += t.DownloadSpeed
		list = append(list, wsTorrent{
			ID: t.ID, Name: t.Name, State: string(t.State), Progress: t.Progress(),
			Downloaded: t.Downloaded, Uploaded: t.Uploaded, Size: t.Size,
			DL: t.DownloadSpeed, UL: t.UploadSpeed, Seeders: t.Seeders,
			Leechers: t.Leechers, Peers: t.PeersConnected,
			PH: t.PiecesHave, PT: t.PiecesTotal, Ratio: t.Ratio() / 1, Cat: t.Category,
		})
	}
	st := s.engine.StatsSnapshot()
	m := wsMsg{
		Type: "tick", Up: sumUp, Down: sumDn,
		UpTotal: st.UpTotal, DownTotal: st.DownTotal,
		UpToday: st.UpToday, DownToday: st.DownToday,
		Conns: st.ConnsTotal, Active: st.ConnActive,
		Paused: st.UploadPaused, DailyLimit: st.DailyUploadLimit,
		Torrents: list,
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}

// broadcastLoop pushes a live snapshot to all WebSocket clients every second.
func (s *Server) broadcastLoop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for range t.C {
		payload := s.wsTicker()
		if payload == nil {
			continue
		}
		s.hub.broadcast(payload)
	}
}

// noticeMsg is a one-off WebSocket notification (completion, metadata fetched).
type noticeMsg struct {
	Type string `json:"type"`
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

// broadcastNotice pushes a notification to all connected clients.
func (s *Server) broadcastNotice(kind, id, name string) {
	b, err := json.Marshal(noticeMsg{Type: "notice", Kind: kind, ID: id, Name: name})
	if err != nil {
		return
	}
	s.hub.broadcast(b)
}

// frameText wraps a payload in a single FIN text frame (RFC 6455).
func frameText(payload []byte) []byte {
	n := len(payload)
	buf := make([]byte, 0, n+10)
	buf = append(buf, 0x81)
	switch {
	case n < 126:
		buf = append(buf, byte(n))
	case n < 65536:
		buf = append(buf, 126, byte(n>>8), byte(n))
	default:
		buf = append(buf, 127)
		var l [8]byte
		binary.BigEndian.PutUint64(l[:], uint64(n))
		buf = append(buf, l[:]...)
	}
	buf = append(buf, payload...)
	return buf
}

// framePong builds a FIN pong frame echoing a ping payload.
func framePong(payload []byte) []byte {
	n := len(payload)
	buf := make([]byte, 0, n+10)
	buf = append(buf, 0x8A)
	if n < 126 {
		buf = append(buf, byte(n))
	} else if n < 65536 {
		buf = append(buf, 126, byte(n>>8), byte(n))
	} else {
		buf = append(buf, 127)
		var l [8]byte
		binary.BigEndian.PutUint64(l[:], uint64(n))
		buf = append(buf, l[:]...)
	}
	buf = append(buf, payload...)
	return buf
}

// wsHandler upgrades an HTTP request to a WebSocket connection.
func (s *Server) wsHandler(w http.ResponseWriter, r *http.Request) {
	if !headerContains(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "expected websocket upgrade", http.StatusBadRequest)
		return
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "missing Sec-WebSocket-Key", http.StatusBadRequest)
		return
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	accept := base64.StdEncoding.EncodeToString(sum[:])

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
		return
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		http.Error(w, "hijack failed", http.StatusInternalServerError)
		return
	}
	bufrw.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n")
	bufrw.Flush()

	c := &wsClient{conn: conn, send: make(chan []byte, 8)}
	s.hub.register(c)
	defer s.hub.unregister(c)
	defer conn.Close()

	go c.writeLoop()

	// Read loop: handle ping/pong/close frames; ignore others.
	br := bufrw.Reader
	for {
		conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		b0, err := br.ReadByte()
		if err != nil {
			return
		}
		b1, err := br.ReadByte()
		if err != nil {
			return
		}
		opcode := b0 & 0x0F
		masked := b1&0x80 != 0
		length := uint64(b1 & 0x7F)
		switch length {
		case 126:
			var hb [2]byte
			if _, err := br.Read(hb[:]); err != nil {
				return
			}
			length = uint64(binary.BigEndian.Uint16(hb[:]))
		case 127:
			var hb [8]byte
			if _, err := br.Read(hb[:]); err != nil {
				return
			}
			length = binary.BigEndian.Uint64(hb[:])
		}
		if masked {
			var mk [4]byte
			if _, err := br.Read(mk[:]); err != nil {
				return
			}
		}
		payload := make([]byte, length)
		if length > 0 {
			if _, err := readFull(br, payload); err != nil {
				return
			}
		}
		switch opcode {
		case 0x8: // close
			return
		case 0x9: // ping
			c.sendPong(payload)
		}
	}
}

// readFull is io.ReadFull for *bufio.Reader.
func readFull(br *bufio.Reader, p []byte) (int, error) {
	total := 0
	for total < len(p) {
		n, err := br.Read(p[total:])
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

func headerContains(v, sub string) bool {
	for i := 0; i+len(sub) <= len(v); i++ {
		if v[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// writeLoop serializes frames to a connected client.
func (c *wsClient) writeLoop() {
	for payload := range c.send {
		n := 0
		for n < len(payload) {
			w, err := c.conn.Write(payload[n:])
			if err != nil {
				return
			}
			n += w
		}
	}
}

// sendPong writes a pong frame synchronously (peer is alive).
func (c *wsClient) sendPong(payload []byte) {
	f := framePong(payload)
	n := 0
	for n < len(f) {
		w, err := c.conn.Write(f[n:])
		if err != nil {
			return
		}
		n += w
	}
}