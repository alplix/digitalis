package dl

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Profile is a named, reusable set of download settings (URL stays out of it).
type Profile struct {
	Name      string `json:"name"`
	Mode      string `json:"mode,omitempty"`
	RamMB     int    `json:"ram_mb,omitempty"`
	Loops     int    `json:"loops,omitempty"`
	Interval  int    `json:"interval,omitempty"`
	SpeedCap  int64  `json:"speed_cap,omitempty"`
	Conn      int    `json:"conn,omitempty"`
	Retries   int    `json:"retries,omitempty"`
	RetryWait int    `json:"retry_wait,omitempty"`
	Proxy     string `json:"proxy,omitempty"`
	StartAt   string `json:"start_at,omitempty"`
	Repeat    string `json:"repeat,omitempty"`
	Depth     int    `json:"depth,omitempty"`
	Include   string `json:"include,omitempty"`
	MinSizeMB int64  `json:"min_size_mb,omitempty"`
	MaxSizeMB int64  `json:"max_size_mb,omitempty"`
	StopMB    int64  `json:"stop_mb,omitempty"`
	StopFiles int64  `json:"stop_files,omitempty"`
	StopMin   int64  `json:"stop_min,omitempty"`
	VerifySHA string `json:"verify_sha256,omitempty"`
}

type profileFile struct {
	Profiles []Profile `json:"profiles"`
}

// Profiles lists all saved profiles ordered by name.
func (m *Manager) Profiles() []Profile {
	m.profMu.Lock()
	out := make([]Profile, 0, len(m.profiles))
	for _, p := range m.profiles {
		out = append(out, p)
	}
	m.profMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SaveProfile stores (or replaces) a named profile.
func (m *Manager) SaveProfile(p Profile) error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return fmt.Errorf("profile name is required")
	}
	m.profMu.Lock()
	m.profiles[p.Name] = p
	err := m.storeProfiles()
	m.profMu.Unlock()
	return err
}

// DeleteProfile removes a profile by name (no error if it does not exist).
func (m *Manager) DeleteProfile(name string) error {
	m.profMu.Lock()
	delete(m.profiles, name)
	err := m.storeProfiles()
	m.profMu.Unlock()
	return err
}

func (m *Manager) loadProfiles() {
	if m.profPath == "" {
		return
	}
	raw, err := os.ReadFile(m.profPath)
	if err != nil {
		return
	}
	var pf profileFile
	if err := json.Unmarshal(raw, &pf); err != nil {
		return
	}
	for _, p := range pf.Profiles {
		if p.Name != "" {
			m.profiles[p.Name] = p
		}
	}
}

// storeProfiles persists the profile map to disk atomically. Caller holds profMu.
func (m *Manager) storeProfiles() error {
	if m.profPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(m.profPath), 0755); err != nil {
		return err
	}
	list := make([]Profile, 0, len(m.profiles))
	for _, p := range m.profiles {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	data, err := json.MarshalIndent(profileFile{Profiles: list}, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.profPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, m.profPath)
}