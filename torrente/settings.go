package torrente

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// diskContains reports whether path lies under root.
func diskContains(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "."
}

// CatRule is one auto-categorization rule: when the (case-insensitive) regex
// Pattern matches a torrent's name, the torrent is filed under Category.
type CatRule struct {
	Pattern  string `json:"pattern"`
	Category string `json:"category"`
}

// Settings holds user-configurable client options. Persisted to disk so they
// survive restarts. All byte values use bytes/sec (0 = unlimited/off).
type Settings struct {
	BaseDir          string   `json:"base_dir"`
	Disks            []string `json:"disks,omitempty"` // extra storage roots (BaseDir excluded)
	UploadLimit      int64    `json:"upload_limit"`
	DownloadLimit    int64    `json:"download_limit"`
	DailyUploadLimit int64    `json:"daily_upload_limit"`

	// Disk guard: pause downloading torrents automatically when the storage
	// root holding them has less than DiskGuardMinGB free. Torrents resume
	// (through the queue) once free space recovers by a small hysteresis.
	DiskGuard      bool  `json:"disk_guard"`
	DiskGuardMinGB int64 `json:"disk_guard_min_gb"`

	// Download queue: keep at most MaxActiveDownloads torrents in the
	// downloading state; the rest wait paused and start in add order.
	// 0 = unlimited.
	MaxActiveDownloads int `json:"max_active_downloads"`

	// Telegram notifications for download completion, goal reached and disk
	// guard events.
	TelegramEnabled bool   `json:"telegram_enabled"`
	TelegramToken   string `json:"telegram_token,omitempty"`
	TelegramChat    string `json:"telegram_chat,omitempty"`

	// Trash: when a torrent is removed with its files, the data moves to
	// <root>/.trash and purges automatically after TrashDays. 0 deletes
	// immediately.
	TrashDays int `json:"trash_days"`

	// Auto-category rules evaluated in order before the heuristic classifier.
	CatRules []CatRule `json:"cat_rules,omitempty"`

	// Night mode: during [NightStart,NightEnd) the transfer rate is capped at
	// NightUpload/NightDownload (0 = keep the daytime limit unchanged). When
	// NightPause is set, transfers stop completely during the window.
	NightMode     bool   `json:"night_mode"`
	NightStart    string `json:"night_start"`   // "HH:MM"
	NightEnd      string `json:"night_end"`     // "HH:MM", may wrap midnight
	NightUpload   int64  `json:"night_upload"`  // bytes/sec during night
	NightDownload int64  `json:"night_download"`
	NightPause    bool   `json:"night_pause"`

	// Ratio target: once a fully-downloaded torrent's ratio reaches the target
	// it is stopped or removed. 0 disables. Per-torrent overrides win.
	RatioTarget float64 `json:"ratio_target"`
	RatioStop   bool    `json:"ratio_stop"`
	RatioRemove bool    `json:"ratio_remove"`

	// SeedDays: once a torrent has been seeding for this many days it is
	// stopped or removed (same stop/remove policy as the ratio target).
	// 0 disables. Per-torrent overrides win.
	SeedDays int64 `json:"seed_days"`

	// ServerToken, when non-empty, requires every web/API request to present
	// it (Bearer header, X-Auth-Token, or ?token=). Never exposed to clients.
	ServerToken string `json:"server_token"`
}

// DefaultSettings returns sane defaults.
func DefaultSettings(baseDir string) Settings {
	return Settings{BaseDir: baseDir}
}

func (e *Engine) settingsPath() string { return e.settingsFile }

// LoadSettings reads settings from disk. Missing/corrupt files fall back to
// defaults without error.
func (e *Engine) LoadSettings(baseDir string) Settings {
	s := DefaultSettings(baseDir)
	data, err := os.ReadFile(e.settingsPath())
	if err != nil {
		return s
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s
	}
	if s.BaseDir == "" {
		s.BaseDir = baseDir
	}
	return s
}

// StoreSettings persists the current settings atomically.
func (e *Engine) StoreSettings() error {
	e.mu.Lock()
	s := e.settings
	e.mu.Unlock()
	return e.storeSettings(s)
}

func (e *Engine) storeSettings(s Settings) error {
	if e.settingsFile == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(e.settingsFile), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := e.settingsFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, e.settingsFile)
}

// View returns a copy of the current settings.
func (e *Engine) View() Settings {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.settings
}

// ServerToken exposes whether access protection is enabled.
func (e *Engine) ServerToken() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.settings.ServerToken
}

// SetServerToken enables access protection and persists it.
func (e *Engine) SetServerToken(tok string) error {
	e.mu.Lock()
	e.settings.ServerToken = tok
	s := e.settings
	e.mu.Unlock()
	return e.storeSettings(s)
}

// ClearServerToken removes access protection and persists it.
func (e *Engine) ClearServerToken() error {
	return e.SetServerToken("")
}

// UpdateSettings applies and persists new settings. Empty fields keep their
// current value. BaseDir changes only affect new torrents. The Telegram token
// is kept unless a new non-empty value arrives (the UI never echoes it back).
func (e *Engine) UpdateSettings(patch Settings) (Settings, error) {
	e.mu.Lock()
	cur := e.settings
	if patch.BaseDir != "" && patch.BaseDir != cur.BaseDir {
		cur.BaseDir = patch.BaseDir
	}
	cur.Disks = patch.Disks
	cur.UploadLimit = patch.UploadLimit
	cur.DownloadLimit = patch.DownloadLimit
	cur.DailyUploadLimit = patch.DailyUploadLimit
	cur.NightMode = patch.NightMode
	cur.NightStart = patch.NightStart
	cur.NightEnd = patch.NightEnd
	cur.NightUpload = patch.NightUpload
	cur.NightDownload = patch.NightDownload
	cur.NightPause = patch.NightPause
	cur.RatioTarget = patch.RatioTarget
	cur.RatioStop = patch.RatioStop
	cur.RatioRemove = patch.RatioRemove
	cur.SeedDays = patch.SeedDays
	cur.DiskGuard = patch.DiskGuard
	cur.DiskGuardMinGB = patch.DiskGuardMinGB
	cur.MaxActiveDownloads = patch.MaxActiveDownloads
	cur.TelegramEnabled = patch.TelegramEnabled
	if patch.TelegramToken != "" {
		cur.TelegramToken = patch.TelegramToken
	}
	cur.TelegramChat = patch.TelegramChat
	cur.TrashDays = patch.TrashDays
	if patch.CatRules == nil {
		patch.CatRules = []CatRule{}
	}
	cur.CatRules = patch.CatRules
	e.settings = cur
	e.mu.Unlock()

	e.SetUploadLimit(cur.UploadLimit)
	e.SetDownloadLimit(cur.DownloadLimit)
	if err := e.storeSettings(cur); err != nil {
		return cur, err
	}
	return cur, nil
}

// Disks returns the configured storage roots. The base save dir is always
// first; extra roots from Settings.Disks follow (deduplicated).
func (e *Engine) Disks() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	seen := map[string]bool{}
	out := []string{}
	for _, d := range append([]string{e.settings.BaseDir}, e.settings.Disks...) {
		if d == "" {
			continue
		}
		if seen[filepath.Clean(d)] {
			continue
		}
		seen[filepath.Clean(d)] = true
		out = append(out, d)
	}
	return out
}

// AddDisk registers a new storage root. The path must exist and be a
// directory, and must not already be registered.
func (e *Engine) AddDisk(path string) error {
	path = filepath.Clean(path)
	if path == "" {
		return fmt.Errorf("empty disk path")
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("disk path not accessible: %v", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("disk path is not a directory")
	}
	e.mu.Lock()
	for _, d := range append([]string{e.settings.BaseDir}, e.settings.Disks...) {
		if d != "" && filepath.Clean(d) == path {
			e.mu.Unlock()
			return fmt.Errorf("disk already registered")
		}
	}
	e.settings.Disks = append(e.settings.Disks, path)
	cur := e.settings
	e.mu.Unlock()
	return e.storeSettings(cur)
}

// RemoveDisk unregisters a storage root. The base dir cannot be removed, and a
// root that still holds torrents is refused.
func (e *Engine) RemoveDisk(path string) error {
	path = filepath.Clean(path)
	e.mu.Lock()
	if e.settings.BaseDir != "" && filepath.Clean(e.settings.BaseDir) == path {
		e.mu.Unlock()
		return fmt.Errorf("cannot remove the base disk")
	}
	found := -1
	for i, d := range e.settings.Disks {
		if filepath.Clean(d) == path {
			found = i
			break
		}
	}
	if found < 0 {
		e.mu.Unlock()
		return fmt.Errorf("disk not registered")
	}
	for _, t := range e.torrents {
		t.mu.Lock()
		ok := t.SaveDir != "" && diskContains(path, t.SaveDir)
		t.mu.Unlock()
		if ok {
			e.mu.Unlock()
			return fmt.Errorf("disk still holds torrents")
		}
	}
	e.settings.Disks = append(e.settings.Disks[:found], e.settings.Disks[found+1:]...)
	cur := e.settings
	e.mu.Unlock()
	return e.storeSettings(cur)
}