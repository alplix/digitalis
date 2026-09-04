package torrente

import (
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
	return urls
}

// initTrackers seeds the tracker stats list from the metainfo.
func (t *Torrent) initTrackers() {
	urls := t.trackerURLs()
	if len(urls) == 0 {
		return
	}
	seen := make(map[string]bool)
	for _, u := range urls {
		if seen[u] {
			continue
		}
		seen[u] = true
		t.Trackers = append(t.Trackers, &TrackerStat{URL: u})
	}
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