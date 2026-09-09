// Package dht implements a minimal but functional Mainline DHT client over
// UDP (BEP-5 / KRPC). It can look up peers for an info hash by crawling the
// closest routing-table nodes and announce a listening port back into the
// network.
package dht

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/alplix/digitalis/bencode"
)

// DefaultBootstrapNodes are well-known DHT routers.
var DefaultBootstrapNodes = []string{
	"router.bittorrent.com:6881",
	"router.utorrent.com:6881",
	"dht.transmissionbt.com:6881",
	"router.bitcomet.com:6881",
	"dht.libtorrent.org:25401",
}

const (
	k            = 8   // nodes considered per round
	maxRounds    = 7   // crawl depth
	maxScrape    = 200 // maximum peers to gather
	queryTimeout = 1500 * time.Millisecond
)

// Client is a DHT node speaking KRPC over UDP.
type Client struct {
	id   [20]byte
	conn *net.UDPConn
	mu   sync.Mutex
	next uint16
	pend map[uint16]chan dhtResp
}

type dhtResp struct {
	r     map[string]interface{}
	token string
	err   error
}

// New creates a DHT client bound to a random local UDP port.
func New() (*Client, error) {
	var id [20]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return nil, err
	}
	c := &Client{id: id, conn: conn, next: 1, pend: make(map[uint16]chan dhtResp)}
	go c.readLoop()
	return c, nil
}

// Close shuts the client down.
func (c *Client) Close() error { return c.conn.Close() }

// NodeID returns the 20-byte node id.
func (c *Client) NodeID() [20]byte { return c.id }

// readLoop dispatches KRPC responses to their pending transactions.
func (c *Client) readLoop() {
	buf := make([]byte, 65536)
	for {
		n, _, err := c.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		v, _, err := bencode.DecodePrefix(buf[:n])
		if err != nil {
			continue
		}
		d, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		tv, ok := d["t"].(string)
		if !ok || len(tv) != 2 {
			continue
		}
		tx := binary.BigEndian.Uint16([]byte{tv[0], tv[1]})
		c.mu.Lock()
		ch := c.pend[tx]
		delete(c.pend, tx)
		c.mu.Unlock()
		if ch == nil {
			continue
		}
		resp := dhtResp{}
		if rv, ok := d["r"].(map[string]interface{}); ok {
			resp.r = rv
			if tok, ok := rv["token"].(string); ok {
				resp.token = tok
			}
		} else {
			resp.err = fmt.Errorf("KRPC error response")
		}
		ch <- resp
	}
}

// query sends a single KRPC query and waits for the response.
func (c *Client) query(addr *net.UDPAddr, q string, args map[string]interface{}) (map[string]interface{}, string, error) {
	c.mu.Lock()
	c.next++
	if c.next == 0 {
		c.next = 1
	}
	tx := c.next
	ch := make(chan dhtResp, 1)
	c.pend[tx] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pend, tx)
		c.mu.Unlock()
	}()

	t := []byte{byte(tx >> 8), byte(tx & 0xff)}
	body := make(map[string]interface{})
	body["t"] = string(t)
	body["y"] = "q"
	body["q"] = q
	body["a"] = args
	packet, err := bencode.Encode(body)
	if err != nil {
		return nil, "", err
	}
	if _, err := c.conn.WriteToUDP(packet, addr); err != nil {
		return nil, "", err
	}

	select {
	case resp := <-ch:
		return resp.r, resp.token, resp.err
	case <-time.After(queryTimeout):
		return nil, "", fmt.Errorf("dht query timeout")
	}
}

func (c *Client) nodeID() string { return string(c.id[:]) }

// getPeersOne performs a single get_peers query.
func (c *Client) getPeersOne(addr *net.UDPAddr, infoHash string) ([]string, []string, string, error) {
	args := map[string]interface{}{
		"id":        c.nodeID(),
		"info_hash": infoHash,
	}
	r, token, err := c.query(addr, "get_peers", args)
	if err != nil {
		return nil, nil, "", err
	}
	var peers []string
	if vs, ok := r["values"].([]interface{}); ok {
		for _, v := range vs {
			if b, ok := v.(string); ok && len(b) == 6 {
				ip := net.IP(b[0:4]).String()
				port := binary.BigEndian.Uint16([]byte{b[4], b[5]})
				peers = append(peers, net.JoinHostPort(ip, fmt.Sprintf("%d", port)))
			}
		}
	}
	var nodes []string
	if ns, ok := r["nodes"].(string); ok {
		for i := 0; i+26 <= len(ns); i += 26 {
			nn := ns[i : i+26]
			ip := net.IP(nn[20:24]).String()
			port := binary.BigEndian.Uint16([]byte{nn[24], nn[25]})
			if port != 0 {
				nodes = append(nodes, net.JoinHostPort(ip, fmt.Sprintf("%d", port)))
			}
		}
	}
	return peers, nodes, token, nil
}

// GetPeers crawls the DHT for peers sharing the given info hash.
func (c *Client) GetPeers(infohash [20]byte) ([]string, error) {
	found := make(map[string]bool)
	var peers []string
	deadline := time.Now().Add(8 * time.Second)

	// nodeAddr picks the k "closest" candidate addresses to query this round.
	var frontier []string
	for _, r := range DefaultBootstrapNodes {
		frontier = append(frontier, r)
	}

	for rounds := 0; rounds < maxRounds && len(peers) < maxScrape && time.Now().Before(deadline); rounds++ {
		var wg sync.WaitGroup
		var mu sync.Mutex
		var next []string
		limit := k
		if len(frontier) < limit {
			limit = len(frontier)
		}
		for i := 0; i < limit; i++ {
			addrStr := frontier[i]
			wg.Add(1)
			go func(a string) {
				defer wg.Done()
				addr, err := net.ResolveUDPAddr("udp4", a)
				if err != nil {
					return
				}
				ps, nodes, _, err := c.getPeersOne(addr, string(infohash[:]))
				if err != nil {
					return
				}
				mu.Lock()
				for _, p := range ps {
					if !found[p] {
						found[p] = true
						peers = append(peers, p)
					}
				}
				for _, n := range nodes {
					next = append(next, n)
				}
				mu.Unlock()
			}(addrStr)
		}
		wg.Wait()
		if len(peers) > 0 {
			break
		}
		frontier = dedupeAddrs(next)
	}

	return peers, nil
}

// dedupeAddrs removes duplicate host:port strings preserving order.
func dedupeAddrs(in []string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(in))
	for _, a := range in {
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}

// Announce best-effort announces the given info hash and port into the DHT so
// other nodes can find us as a peer for it.
func (c *Client) Announce(infohash [20]byte, port int) {
	targets := make(map[string]string) // addr -> token
	for _, r := range DefaultBootstrapNodes {
		addr, err := net.ResolveUDPAddr("udp4", r)
		if err != nil {
			continue
		}
		_, nodes, token, err := c.getPeersOne(addr, string(infohash[:]))
		if err != nil {
			continue
		}
		if len(nodes) == 0 {
			if token != "" {
				targets[r] = token
			}
			continue
		}
		// Probe a few newly discovered nodes for their tokens too.
		probed := 0
		for _, n := range nodes {
			if probed >= 8 {
				break
			}
			probed++
			naddr, err := net.ResolveUDPAddr("udp4", n)
			if err != nil {
				continue
			}
			_, _, tok, err := c.getPeersOne(naddr, string(infohash[:]))
			if err != nil || tok == "" {
				continue
			}
			targets[n] = tok
		}
	}
	if len(targets) == 0 {
		return
	}
	count := 0
	for addrStr, token := range targets {
		if count >= 8 {
			break
		}
		addr, err := net.ResolveUDPAddr("udp4", addrStr)
		if err != nil {
			continue
		}
		args := map[string]interface{}{
			"id":           c.nodeID(),
			"info_hash":    string(infohash[:]),
			"port":         int64(port),
			"token":        token,
			"implied_port": int64(0),
		}
		_, _, _ = c.query(addr, "announce_peer", args)
		count++
	}
}
