package torrente

import (
	"encoding/hex"
	"time"

	"github.com/alplix/digitalis/dht"
)

// startDHT lazily creates the Mainline DHT client.
func (e *Engine) startDHT() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.dhtClient != nil {
		return
	}
	c, err := dht.New()
	if err != nil {
		e.Logf("dht disabled: %v", err)
		return
	}
	e.dhtClient = c
	nid := c.NodeID()
	e.Logf("dht node %x", nid[:6])
	// Periodically announce seeding torrents so the swarm can find us.
	go e.dhtAnnounceLoop()
}

// dhtAnnounceLoop announces each seeding torrent into the DHT every few
// minutes.
func (e *Engine) dhtAnnounceLoop() {
	tick := time.NewTicker(3 * time.Minute)
	defer tick.Stop()
	for range tick.C {
		e.mu.Lock()
		c := e.dhtClient
		var seeders []*Torrent
		for _, t := range e.torrents {
			t.mu.Lock()
			st := t.State
			hasMeta := t.storage != nil
			ihb := hexToBytes(t.InfoHash)
			t.mu.Unlock()
			if c == nil || st != StateSeeding || !hasMeta || len(ihb) != 20 {
				continue
			}
			seeders = append(seeders, t)
			if len(seeders) >= 8 {
				break
			}
		}
		e.mu.Unlock()
		for _, t := range seeders {
			if ihb := hexToBytes(t.InfoHash); len(ihb) == 20 {
				var ih [20]byte
				copy(ih[:], ihb)
				go c.Announce(ih, e.Port())
			}
		}
	}
}

// startDHTPeerLoop periodically crawls the DHT for extra peers for a torrent,
// which is essential for magnets whose trackers are dead.
func (e *Engine) startDHTPeerLoop(t *Torrent) {
	e.startDHT()
	go func() {
		time.Sleep(2 * time.Second)
		for {
			time.Sleep(90 * time.Second)
			e.mu.Lock()
			if e.torrents[t.ID] != t {
				e.mu.Unlock()
				return
			}
			c := e.dhtClient
			e.mu.Unlock()
			if c == nil {
				return
			}
			ihb := hexToBytes(t.InfoHash)
			if len(ihb) != 20 {
				return
			}
			var ih [20]byte
			copy(ih[:], ihb)
			peers, err := c.GetPeers(ih)
			if err != nil {
				continue
			}
			t.mu.Lock()
			st := t.State
			t.mu.Unlock()
			if st == StatePaused || st == StateStopped {
				continue
			}
			if len(peers) > 0 {
				t.mu.Lock()
				t.PeersKnown += len(peers)
				t.mu.Unlock()
				e.connectDiscovered(t, peers)
			}
		}
	}()
}

func hexToBytes(s string) []byte {
	if len(s) != 40 {
		return nil
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil
	}
	return b
}

// NodeID exposes the DHT node id for debug views (may be empty).
func (e *Engine) NodeID() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.dhtClient == nil {
		return ""
	}
	nid := e.dhtClient.NodeID()
	return hex.EncodeToString(nid[:])
}