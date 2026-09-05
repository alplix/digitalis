package torrente

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Settings holds user-configurable client options. Persisted to disk so they
// survive restarts. All byte values use bytes/sec (0 = unlimited/off).
type Settings struct {
	BaseDir          string `json:"base_dir"`
	UploadLimit      int64  `json:"upload_limit"`
	DownloadLimit    int64  `json:"download_limit"`
	DailyUploadLimit int64  `json:"daily_upload_limit"`
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

// UpdateSettings applies and persists new settings. Empty fields keep their
// current value. BaseDir changes only affect new torrents.
func (e *Engine) UpdateSettings(patch Settings) (Settings, error) {
	e.mu.Lock()
	cur := e.settings
	if patch.BaseDir != "" && patch.BaseDir != cur.BaseDir {
		cur.BaseDir = patch.BaseDir
	}
	cur.UploadLimit = patch.UploadLimit
	cur.DownloadLimit = patch.DownloadLimit
	cur.DailyUploadLimit = patch.DailyUploadLimit
	e.settings = cur
	e.mu.Unlock()

	e.SetUploadLimit(cur.UploadLimit)
	e.SetDownloadLimit(cur.DownloadLimit)
	if err := e.storeSettings(cur); err != nil {
		return cur, err
	}
	return cur, nil
}