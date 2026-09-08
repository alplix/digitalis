package torrente

import (
	"fmt"
	"time"
)

// TrackerStat tracks per-tracker announce status for a torrent.
type TrackerStat struct {
	URL           string    `json:"url"`
	Working       bool      `json:"working"`
	Announces     int       `json:"announces"`
	Successes     int       `json:"successes"`
	Failures      int       `json:"failures"`
	LastSuccess   time.Time `json:"last_success"`
	LastFailure   time.Time `json:"last_failure"`
	LastError     string    `json:"last_error,omitempty"`
	LastSeeders   int       `json:"last_seeders"`
	LastLeechers  int       `json:"last_leechers"`
	LastPeers     int       `json:"last_peers"`
}

// trackerURLs returns the list of tracker announce URLs for a torrent.
func (t *Torrent) trackerURLs() []string {
	var urls []string
	if t.MetaInfo != nil {
		if t.MetaInfo.Announce != "" {
			urls = append(urls, t.MetaInfo.Announce)
		}
		for _, tier := range t.MetaInfo.AnnounceList {
			urls = append(urls, tier...)
		}
	}
	if t.Magnet != nil {
		urls = append(urls, t.Magnet.Trackers...)
	}
	urls = append(urls, t.CustomTrackers...)
	return urls
}

// initTrackers seeds the tracker stats list from the metainfo and custom list.
// Existing per-tracker state (working, successes, ...) is preserved by URL.
func (t *Torrent) initTrackers() {
	urls := t.trackerURLs()
	if len(urls) == 0 {
		t.Trackers = nil
		return
	}
	seen := make(map[string]*TrackerStat)
	for _, ts := range t.Trackers {
		seen[ts.URL] = ts
	}
	var rebuilt []*TrackerStat
	for _, u := range urls {
		if t.RemovedTrackers[u] {
			continue
		}
		if ts := seen[u]; ts != nil {
			rebuilt = append(rebuilt, ts)
			continue
		}
		rebuilt = append(rebuilt, &TrackerStat{URL: u})
	}
	t.Trackers = rebuilt
}

// trackerByURL finds a tracker stat by URL.
func (t *Torrent) trackerByURL(url string) *TrackerStat {
	for _, ts := range t.Trackers {
		if ts.URL == url {
			return ts
		}
	}
	return nil
}

// noteTrackerSuccess records a successful announce.
func (t *Torrent) noteTrackerSuccess(ts *TrackerStat, seeders, leechers, peers int) {
	ts.Working = true
	ts.Announces++
	ts.Successes++
	ts.LastSuccess = time.Now()
	ts.LastSeeders = seeders
	ts.LastLeechers = leechers
	ts.LastPeers = peers
	ts.LastError = ""
	if t.AnnounceCount != nil {
		t.AnnounceCount[ts.URL]++
	}
}

// noteTrackerFailure records a failed announce.
func (t *Torrent) noteTrackerFailure(ts *TrackerStat, err error) {
	ts.Announces++
	ts.Failures++
	ts.LastFailure = time.Now()
	if err != nil {
		ts.LastError = err.Error()
	}
	if ts.Successes == 0 || ts.Failures > ts.Successes {
		ts.Working = false
	}
}

// countTrackersActive recomputes working/failing tracker tallies.
func (t *Torrent) countTrackersActive() {
	w, f := 0, 0
	for _, ts := range t.Trackers {
		if ts.Working {
			w++
		} else {
			f++
		}
	}
	t.WorkingTrackers = w
	t.FailedTrackers = f
}

func goodTrackerScheme(u string) bool {
	if len(u) < 7 {
		return false
	}
	switch u[:6] {
	case "http:/", "udp://", "wss://", "ws://":
		return true
	}
	if len(u) >= 7 && u[:7] == "https:/" {
		return true
	}
	return false
}

// AddTracker adds a private announce URL to a torrent and re-announces.
func (e *Engine) AddTracker(id, url string) error {
	t, ok := e.GetTorrent(id)
	if !ok {
		return fmt.Errorf("torrent not found")
	}
	if !goodTrackerScheme(url) {
		return fmt.Errorf("unsupported tracker scheme: %s", url)
	}
	t.mu.Lock()
	if t.RemovedTrackers == nil {
		t.RemovedTrackers = make(map[string]bool)
	}
	if t.AnnounceCount == nil {
		t.AnnounceCount = make(map[string]int64)
	}
	for _, u := range t.CustomTrackers {
		if u == url {
			t.mu.Unlock()
			return fmt.Errorf("tracker already added")
		}
	}
	t.CustomTrackers = append(t.CustomTrackers, url)
	delete(t.RemovedTrackers, url)
	t.initTrackers()
	t.countTrackersActive()
	t.mu.Unlock()
	go e.Announce(t, "started")
	return nil
}

// CustomTrackerList returns a copy of the torrent's user-added announce URLs.
func (e *Engine) CustomTrackerList(id string) []string {
	t, ok := e.GetTorrent(id)
	if !ok {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.CustomTrackers...)
}

// RemoveTracker disables an announce URL for a torrent and re-announces.
func (e *Engine) RemoveTracker(id, url string) error {
	t, ok := e.GetTorrent(id)
	if !ok {
		return fmt.Errorf("torrent not found")
	}
	t.mu.Lock()
	if t.RemovedTrackers == nil {
		t.RemovedTrackers = make(map[string]bool)
	}
	custom := false
	for i, u := range t.CustomTrackers {
		if u == url {
			t.CustomTrackers = append(t.CustomTrackers[:i], t.CustomTrackers[i+1:]...)
			custom = true
			break
		}
	}
	if !custom {
		t.RemovedTrackers[url] = true
	}
	t.initTrackers()
	t.countTrackersActive()
	t.mu.Unlock()
	go e.Announce(t, "started")
	return nil
}