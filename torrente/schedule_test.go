package torrente

import (
	"sync"
	"testing"
	"time"
)

func TestNightIsActive(t *testing.T) {
	cases := []struct {
		start, end string
		at         time.Time
		want       bool
	}{
		{"23:00", "07:00", time.Date(2026, 9, 6, 3, 0, 0, 0, time.UTC), true},
		{"23:00", "07:00", time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC), false},
		{"23:00", "07:00", time.Date(2026, 9, 6, 22, 30, 0, 0, time.UTC), false},
		{"23:00", "07:00", time.Date(2026, 9, 6, 23, 30, 0, 0, time.UTC), true},
		{"23:00", "07:00", time.Date(2026, 9, 6, 6, 59, 0, 0, time.UTC), true},
		{"23:00", "07:00", time.Date(2026, 9, 6, 7, 0, 0, 0, time.UTC), false},
		{"00:00", "23:59", time.Date(2026, 9, 6, 14, 0, 0, 0, time.UTC), true},
		{"", "07:00", time.Date(2026, 9, 6, 3, 0, 0, 0, time.UTC), false},
		{"25:00", "07:00", time.Date(2026, 9, 6, 3, 0, 0, 0, time.UTC), false},
	}
	for _, c := range cases {
		if got := nightIsActive(c.start, c.end, c.at); got != c.want {
			t.Errorf("nightIsActive(%q,%q, at %s) = %v, want %v", c.start, c.end, c.at, got, c.want)
		}
	}
	if h, m := parseClock("23:05"); h != 23 || m != 5 {
		t.Errorf("parseClock(23:05) = %d:%d", h, m)
	}
	if h, m := parseClock("abc"); h != -1 || m != -1 {
		t.Errorf("parseClock(abc) = %d:%d", h, m)
	}
}

func newTestEngine() *Engine {
	e := NewEngine(0)
	e.mu.Lock()
	for id, t := range map[string]*Torrent{
		"t1": {mu: new(sync.Mutex), ID: "t1", Name: "s1", TotalWanted: 1000, Downloaded: 1000, Uploaded: 2000, State: StateSeeding},
		"t2": {mu: new(sync.Mutex), ID: "t2", Name: "s2", TotalWanted: 9000, Downloaded: 3000, Uploaded: 9000, State: StateSeeding},
		"t3": {mu: new(sync.Mutex), ID: "t3", Name: "d1", TotalWanted: 9999, Downloaded: 500, Uploaded: 0, State: StateDownloading},
	} {
		e.torrents[id] = t
	}
	e.mu.Unlock()
	return e
}

func waitNotice(t *testing.T, notices chan string, id string) bool {
	t.Helper()
	for i := 0; i < 5; i++ {
		select {
		case n := <-notices:
			if n == id {
				return true
			}
		case <-time.After(time.Second):
			t.Fatal("notice channel timed out")
		}
	}
	return false
}

func TestRatioStop(t *testing.T) {
	e := newTestEngine()
	notices := make(chan string, 4)
	e.OnNotice = func(kind, id, name string) {
		if kind == NoticeRatio {
			notices <- id
		}
	}
	e.mu.Lock()
	e.settings = Settings{RatioTarget: 1.5, RatioStop: true}
	e.mu.Unlock()

	e.applySchedule(e.settings)
	if !waitNotice(t, notices, "t1") {
		t.Fatal("expected ratio notice for t1")
	}
	t1, _ := e.GetTorrent("t1")
	if t1.State != StatePaused {
		t.Errorf("t1 state = %s, want paused", t1.State)
	}
}

func TestRatioRemove(t *testing.T) {
	e := newTestEngine()
	notices := make(chan string, 4)
	e.OnNotice = func(kind, id, name string) {
		if kind == NoticeRatio {
			notices <- id
		}
	}
	e.mu.Lock()
	e.settings = Settings{RatioTarget: 1.5, RatioRemove: true}
	e.mu.Unlock()

	e.applySchedule(e.settings)
	if !waitNotice(t, notices, "t1") {
		t.Fatal("expected ratio notice for t1")
	}
	if _, ok := e.GetTorrent("t1"); ok {
		t.Error("t1 should have been removed")
	}
}

func TestRatioBelowThreshold(t *testing.T) {
	e := newTestEngine()
	notices := make(chan string, 4)
	e.OnNotice = func(kind, id, name string) {
		if kind == NoticeRatio {
			notices <- id
		}
	}
	e.mu.Lock()
	e.settings = Settings{RatioTarget: 9.0, RatioStop: true}
	e.mu.Unlock()

	e.applySchedule(e.settings)
	select {
	case id := <-notices:
		t.Errorf("unexpected notice for %s (threshold not reached)", id)
	case <-time.After(300 * time.Millisecond):
	}
	for _, id := range []string{"t1", "t2", "t3"} {
		if _, ok := e.GetTorrent(id); !ok {
			t.Errorf("%s was removed despite low ratio", id)
		}
	}
}

func TestRatioPerTorrentOverride(t *testing.T) {
	e := newTestEngine()
	// global 9.0, but t2 overrides with 2.0; t2 ratio = 9000/3000 = 3.0 >= 2.0
	e.mu.Lock()
	e.settings = Settings{RatioTarget: 9.0, RatioStop: true}
	e.torrents["t2"].RatioTarget = 2.0
	e.mu.Unlock()

	notices := make(chan string, 4)
	e.OnNotice = func(kind, id, name string) {
		if kind == NoticeRatio {
			notices <- id
		}
	}
	e.applySchedule(e.settings)
	if !waitNotice(t, notices, "t2") {
		t.Fatal("expected ratio notice for t2 via per-torrent override")
	}
	t2, _ := e.GetTorrent("t2")
	if t2.State != StatePaused {
		t.Errorf("t2 state = %s, want paused", t2.State)
	}
}

func TestNightFullPause(t *testing.T) {
	e := newTestEngine()
	// window covering "now" with full pause
	night := Settings{NightMode: true, NightStart: "00:00", NightEnd: "23:59", NightPause: true}
	e.mu.Lock()
	e.settings = night
	e.mu.Unlock()
	e.applySchedule(e.settings)
	if !e.UploadBlocked() {
		t.Error("upload should be blocked during night full pause")
	}
	if !e.DownloadBlocked() {
		t.Error("download should be blocked during night full pause")
	}

	// the daily-limit check may clear uploadPaused mid-night; applySchedule
	// must re-assert it on the next tick
	e.setUploadPaused(false)
	e.applySchedule(e.settings)
	if !e.UploadBlocked() {
		t.Error("upload pause must be re-asserted after daily check clears it")
	}

	// disable pause while still in the window
	nightNoPause := night
	nightNoPause.NightPause = false
	e.mu.Lock()
	e.settings = nightNoPause
	e.mu.Unlock()
	e.applySchedule(e.settings)
	if e.DownloadBlocked() {
		t.Error("download should be released when night pause is off")
	}
}

func TestNightCapsWithoutPause(t *testing.T) {
	e := newTestEngine()
	e.mu.Lock()
	e.settings = Settings{NightMode: true, NightStart: "00:00", NightEnd: "23:59",
		NightUpload: 2048, NightDownload: 4096}
	e.mu.Unlock()
	e.applySchedule(e.settings)
	if e.UploadBlocked() {
		t.Error("caps-only night mode must not block uploads")
	}
	if e.DownloadBlocked() {
		t.Error("caps-only night mode must not block downloads")
	}
	if e.UploadRateLimit != 2048 {
		t.Errorf("night upload cap = %d, want 2048", e.UploadRateLimit)
	}
	if e.DownloadRateLimit != 4096 {
		t.Errorf("night download cap = %d, want 4096", e.DownloadRateLimit)
	}
}