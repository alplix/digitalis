package torrente

import (
	"fmt"
	"net"

	"github.com/alplix/digitalis/metainfo"
	"github.com/alplix/digitalis/storage"
)

// markSelf records a local socket address so we never dial ourselves.
func (e *Engine) markSelf(addr string) {
	e.mu.Lock()
	e.selfAddrs[addr] = true
	e.mu.Unlock()
}

// notify invokes the user-facing event hook without blocking.
func (e *Engine) notify(kind, id, name string) {
	if e.OnNotice != nil {
		go e.OnNotice(kind, id, name)
	}
}

// Notify delivers a user-facing event to OnNotice subscribers (the web layer
// uses it to surface web/OS notifications for external activity such as RSS).
func (e *Engine) Notify(kind, id, name string) { e.notify(kind, id, name) }

// connectDiscovered schedules outgoing dials for peers learned over PEX or DHT.
func (e *Engine) connectDiscovered(t *Torrent, addrs []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	room := e.MaxConnections - len(e.sessions) - len(e.connecting)
	if room <= 0 {
		return
	}
	for _, a := range addrs {
		if room <= 0 {
			break
		}
		if e.selfAddrs[a] {
			continue
		}
		key := t.ID + "@" + a
		if e.connecting[key] {
			continue
		}
		already := false
		for sess, tr := range e.sessions {
			if tr.ID == t.ID && sess.addr == a {
				already = true
				break
			}
		}
		if already {
			continue
		}
		e.connecting[key] = true
		room--
		go func(addr, k string) {
			host, portStr, err := net.SplitHostPort(addr)
			if err != nil {
				e.mu.Lock()
				delete(e.connecting, k)
				e.mu.Unlock()
				return
			}
			var port int
			_, err = fmt.Sscanf(portStr, "%d", &port)
			if err != nil || port <= 0 || port > 65535 {
				e.mu.Lock()
				delete(e.connecting, k)
				e.mu.Unlock()
				return
			}
			e.connectToPeer(t, host, port)
			e.mu.Lock()
			delete(e.connecting, k)
			e.mu.Unlock()
		}(a, key)
	}
}

// upgradeMagnet turns a metadata-less magnet into a full torrent once its
// metadata has been fetched and verified from peers.
func (e *Engine) upgradeMagnet(t *Torrent, rawInfo []byte) error {
	mi, err := metainfo.ParseInfo(rawInfo)
	if err != nil {
		return fmt.Errorf("parse info dict: %w", err)
	}
	if mi.InfoHash() != t.InfoHash {
		return fmt.Errorf("metadata hash does not match magnet")
	}
	st, err := storage.New(mi, t.SaveDir)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}

	t.mu.Lock()
	if t.storage != nil {
		t.mu.Unlock()
		st.Close()
		return nil
	}
	t.storage = st
	t.MetaInfo = mi
	t.Size = mi.TotalLength()
	t.TotalWanted = mi.TotalLength()
	t.State = StateDownloading
	t.mu.Unlock()

	t.mu.Lock()
	done := t.countDoneBytes()
	t.Downloaded = done
	if done >= t.TotalWanted && t.TotalWanted > 0 {
		t.State = StateSeeding
	}
	if t.Category == "" {
		t.Category = Categorize(t.Name, mi.Info.Files)
	}
	t.mu.Unlock()

	go e.Announce(t, "started")
	e.notify(NoticeMetadata, t.ID, t.Name)
	e.Logf("metadata acquired for %q (%s, %d bytes)", t.Name, t.ID, mi.TotalLength())
	return nil
}

// SetTorrentDownloadLimit sets a per-torrent download limit in bytes/sec
// (0 disables). It only affects future requests from this point on.
func (e *Engine) SetTorrentDownloadLimit(id string, n int64) error {
	t, ok := e.GetTorrent(id)
	if !ok {
		return fmt.Errorf("torrent not found")
	}
	if n < 0 {
		n = 0
	}
	t.mu.Lock()
	if t.dlRate == nil {
		t.dlRate = newRateLimiter(n)
	} else {
		t.dlRate.setRate(n)
	}
	t.DownloadLimit = n
	t.mu.Unlock()
	return nil
}

// TorrentDownloadLimit returns the effective per-torrent download limit.
func (e *Engine) TorrentDownloadLimit(id string) int64 {
	t, ok := e.GetTorrent(id)
	if !ok {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.DownloadLimit
}

// SetTorrentRatioTarget sets a per-torrent ratio goal (0 = use global setting).
func (e *Engine) SetTorrentRatioTarget(id string, ratio float64) error {
	t, ok := e.GetTorrent(id)
	if !ok {
		return fmt.Errorf("torrent not found")
	}
	if ratio < 0 {
		ratio = 0
	}
	t.mu.Lock()
	t.RatioTarget = ratio
	t.mu.Unlock()
	return nil
}

// TorrentRatioTarget returns the per-torrent ratio goal (0 = use global).
func (e *Engine) TorrentRatioTarget(id string) float64 {
	t, ok := e.GetTorrent(id)
	if !ok {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.RatioTarget
}

// SetTorrentSeedDays sets a per-torrent max seeding days (0 = use global).
func (e *Engine) SetTorrentSeedDays(id string, days int64) error {
	t, ok := e.GetTorrent(id)
	if !ok {
		return fmt.Errorf("torrent not found")
	}
	if days < 0 {
		days = 0
	}
	t.mu.Lock()
	t.SeedDaysTarget = days
	t.mu.Unlock()
	return nil
}

// TorrentSeedDays returns the per-torrent seeding days goal (0 = use global).
func (e *Engine) TorrentSeedDays(id string) int64 {
	t, ok := e.GetTorrent(id)
	if !ok {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.SeedDaysTarget
}

// SetTorrentSequential toggles in-order (stream-friendly) piece picking.
// While enabled the engine keeps a small priority window that advances as
// pieces complete so the beginning of the torrent is always available first.
func (e *Engine) SetTorrentSequential(id string, on bool) error {
	t, ok := e.GetTorrent(id)
	if !ok {
		return fmt.Errorf("torrent not found")
	}
	t.mu.Lock()
	t.Sequential = on
	p := t.picker
	t.mu.Unlock()
	if !on && p != nil {
		p.SetPriority(-1, -1)
	}
	e.Logf("sequential mode for %s is %v", id, on)
	return nil
}

// TorrentSequential returns whether the torrent is in sequential mode.
func (e *Engine) TorrentSequential(id string) bool {
	t, ok := e.GetTorrent(id)
	if !ok {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.Sequential
}

// ReAnnounce forces an immediate tracker announce for one torrent.
func (e *Engine) ReAnnounce(id string) error {
	t, ok := e.GetTorrent(id)
	if !ok {
		return fmt.Errorf("torrent not found")
	}
	go e.Announce(t, "started")
	e.Logf("manual announce for %s", id)
	return nil
}

// ReAnnounceAll re-announces every torrent, returning how many succeeded.
func (e *Engine) ReAnnounceAll() int {
	e.mu.Lock()
	list := make([]*Torrent, 0, len(e.torrents))
	for _, t := range e.torrents {
		list = append(list, t)
	}
	e.mu.Unlock()
	for _, t := range list {
		go e.Announce(t, "started")
	}
	e.Logf("manual announce for %d torrents", len(list))
	return len(list)
}