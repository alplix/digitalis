package torrente

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestQueueTick(t *testing.T) {
	e := NewEngine(0)
	base := time.Now().Add(-time.Hour)
	e.mu.Lock()
	e.torrents["a"] = &Torrent{ID: "a", State: StateDownloading, AddedAt: base}
	e.torrents["b"] = &Torrent{ID: "b", State: StateDownloading, AddedAt: base.Add(time.Minute)}
	e.torrents["c"] = &Torrent{ID: "c", State: StateDownloading, AddedAt: base.Add(2 * time.Minute), TotalWanted: 100}
	e.mu.Unlock()

	e.queueTick(Settings{MaxActiveDownloads: 2})

	got := map[string]State{}
	e.mu.Lock()
	for id, t := range e.torrents {
		got[id] = t.State
	}
	e.mu.Unlock()
	if got["a"] != StateDownloading || got["b"] != StateDownloading {
		t.Fatalf("oldest two must stay downloading, got %v", got)
	}
	if got["c"] != StatePaused {
		t.Fatalf("newest must be queued/paused, got %v", got)
	}
	if !e.torrents["c"].IsQueued() {
		t.Fatal("c must carry the queued flag")
	}

	// free a slot; the queued torrent must start again
	e.mu.Lock()
	e.torrents["a"].State = StateSeeding
	e.mu.Unlock()
	e.queueTick(Settings{MaxActiveDownloads: 2})
	if e.torrents["c"].State != StateDownloading || e.torrents["c"].IsQueued() {
		t.Fatalf("c must resume downloading, got %v queued=%v", e.torrents["c"].State, e.torrents["c"].IsQueued())
	}
}

func TestQueueUnlimited(t *testing.T) {
	e := NewEngine(0)
	e.mu.Lock()
	e.torrents["q1"] = &Torrent{ID: "q1", State: StatePaused, Queued: true, TotalWanted: 100}
	e.torrents["q2"] = &Torrent{ID: "q2", State: StatePaused, Queued: true, TotalWanted: 100}
	e.mu.Unlock()

	e.queueTick(Settings{})
	if e.torrents["q1"].State != StateDownloading || e.torrents["q2"].State != StateDownloading {
		t.Fatalf("unlimited queue must start everything, got %v/%v", e.torrents["q1"].State, e.torrents["q2"].State)
	}
}

func TestDiskGuard(t *testing.T) {
	e := NewEngine(0)
	tmp := t.TempDir()
	e.mu.Lock()
	e.settings = Settings{BaseDir: tmp}
	e.torrents["d1"] = &Torrent{ID: "d1", State: StateDownloading, SaveDir: tmp, TotalWanted: 100}
	e.mu.Unlock()

	// absurdly high threshold: everything must pause and flag
	e.diskGuardTick(Settings{DiskGuard: true, DiskGuardMinGB: 1 << 20})
	if e.torrents["d1"].State != StatePaused || !e.torrents["d1"].IsGuardPaused() {
		t.Fatal("guard must pause the torrent")
	}

	// guard disabled: the pause must release back into the queue
	e.diskGuardTick(Settings{})
	if e.torrents["d1"].IsGuardPaused() || !e.torrents["d1"].IsQueued() {
		t.Fatal("disabled guard must release the torrent to the queue")
	}
	e.queueTick(Settings{})
	if e.torrents["d1"].State != StateDownloading {
		t.Fatalf("queue must resume released torrent, got %v", e.torrents["d1"].State)
	}
}

func TestMatchCatRule(t *testing.T) {
	e := NewEngine(0)
	e.mu.Lock()
	e.settings = Settings{
		CatRules: []CatRule{
			{Pattern: `ubuntu|debian`, Category: "linux"},
			{Pattern: `(?i)FREEBSD`, Category: "bsd"},
		},
	}
	e.mu.Unlock()

	if got := e.MatchCatRule("ubuntu-24.04.iso"); got != "linux" {
		t.Fatalf("ubuntu rule = %q", got)
	}
	if got := e.MatchCatRule("freebsd-15-release"); got != "bsd" {
		t.Fatalf("case-insensitive rule = %q", got)
	}
	if got := e.MatchCatRule("nothing-matches"); got != "" {
		t.Fatalf("no match must be empty, got %q", got)
	}
	// invalid pattern must not panic
	e.mu.Lock()
	e.settings.CatRules = append(e.settings.CatRules, CatRule{Pattern: "([", Category: "x"})
	e.mu.Unlock()
	if got := e.MatchCatRule("anything"); got != "" {
		t.Fatalf("invalid pattern must be skipped, got %q", got)
	}
}

func TestTrashAndPurge(t *testing.T) {
	e := NewEngine(0)
	tmp := t.TempDir()
	e.mu.Lock()
	e.settingsFile = filepath.Join(tmp, "settings.json")
	e.settings = Settings{BaseDir: tmp, TrashDays: 7}
	e.mu.Unlock()

	payload := tmp + "/data.txt"
	if err := os.WriteFile(payload, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := e.trashOrRemove(payload, "data.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(payload); err == nil {
		t.Fatal("payload must have moved to trash")
	}
	entries := e.loadTrash()
	if len(entries) != 1 {
		t.Fatalf("one trash entry expected, got %d", len(entries))
	}
	if _, err := os.Stat(entries[0].Path); err != nil {
		t.Fatalf("trashed file missing: %v", err)
	}

	// purge with zero retention removes it
	if n := e.EmptyTrash(tmp); n != 1 {
		t.Fatalf("empty trash removed %d", n)
	}
	if len(e.loadTrash()) != 0 {
		t.Fatal("trash list must be empty after emptying")
	}
}
