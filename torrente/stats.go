package torrente

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/alplix/digitalis/geo"
)

// MinuteStat is a one-minute sample of global activity.
type MinuteStat struct {
	Ts    time.Time `json:"ts"`
	Up    int64     `json:"up"`
	Down  int64     `json:"down"`
	Conns int64     `json:"conns"`
}

// DayStat aggregates one calendar day.
type DayStat struct {
	Up    int64 `json:"up"`
	Down  int64 `json:"down"`
	Conns int64 `json:"conns"`
}

// YearStat aggregates one calendar year.
type YearStat struct {
	Up    int64 `json:"up"`
	Down  int64 `json:"down"`
	Conns int64 `json:"conns"`
}

// CountryStat ranks countries by connection count.
type CountryStat struct {
	Code string `json:"code"`
	Name string `json:"name"`
	Conns int64 `json:"conns"`
}

// PersistedStats is what gets written to disk.
type PersistedStats struct {
	Since     time.Time         `json:"since"`
	Days      map[string]*DayStat   `json:"days"`
	Years     map[string]*YearStat  `json:"years"`
	Countries map[string]int64      `json:"countries"`
}

// StatsView is the JSON snapshot for the dashboard.
type StatsView struct {
	UpTotal          int64          `json:"up_total"`
	DownTotal        int64          `json:"down_total"`
	UpToday          int64          `json:"up_today"`
	DownToday        int64          `json:"down_today"`
	ConnActive       int            `json:"conn_active"`
	ConnsToday       int64          `json:"conns_today"`
	ConnsTotal       int64          `json:"conns_total"`
	DailyUploadLimit int64          `json:"daily_upload_limit"`
	UploadPaused     bool           `json:"upload_paused"`
	Since            time.Time      `json:"since"`
	Hourly           []StatPoint    `json:"hourly"`
	Daily            []StatPoint    `json:"daily"`
	Monthly          []StatPoint    `json:"monthly"`
	Yearly           []StatPoint    `json:"yearly"`
	Countries        []CountryStat  `json:"countries"`
}

// StatPoint is one plot point {label, up, down}.
type StatPoint struct {
	Label string `json:"label"`
	Up    int64  `json:"up"`
	Down  int64  `json:"down"`
}

// stats internals guarded by statsMu.
type stats struct {
	minutes []MinuteStat // rolling 30s samples for the day chart (kept 48h)
	day     DayStat      // today's live totals
	dayKey  string
	connsRun int64      // connections this run (unused but kept for later)

	days      map[string]*DayStat
	years     map[string]*YearStat
	countries map[string]int64
	since     time.Time
}

// per-tick sample buffers (also accumulated via atomics to keep hot paths lock-free)
var minBufUp, minBufDown, minBufConns int64

// setStatsPaths anchors the settings + stats persistence files.
func (e *Engine) setStatsPaths(configDir string) {
	e.mu.Lock()
	e.statsFile = filepath.Join(configDir, "stats.json")
	e.settingsFile = filepath.Join(configDir, "settings.json")
	e.mu.Unlock()
}

// InitStats loads persisted stats, applies settings and starts the stats loop.
func (e *Engine) InitStats(configDir string, fallbackBaseDir string) Settings {
	e.setStatsPaths(configDir)
	s := e.LoadSettings(fallbackBaseDir)
	e.mu.Lock()
	e.settings = s
	e.stats.days = make(map[string]*DayStat)
	e.stats.years = make(map[string]*YearStat)
	e.stats.countries = make(map[string]int64)
	e.stats.dayKey = time.Now().Format("2006-01-02")
	e.stats.since = time.Now()
	if data, err := os.ReadFile(e.statsFile); err == nil {
		var p PersistedStats
		if json.Unmarshal(data, &p) == nil {
			if !p.Since.IsZero() {
				e.stats.since = p.Since
			}
			for k, v := range p.Days {
				e.stats.days[k] = v
			}
			for k, v := range p.Years {
				e.stats.years[k] = v
			}
			for k, v := range p.Countries {
				e.stats.countries[k] = v
			}
		}
	}
	e.mu.Unlock()

	e.SetUploadLimit(s.UploadLimit)
	e.SetDownloadLimit(s.DownloadLimit)

	go e.statsLoop()
	go e.sequentialLoop()
	return s
}

// statsLoop samples activity every 30s and persists state every 60s.
func (e *Engine) statsLoop() {
	persistTick := 0
	for {
		time.Sleep(30 * time.Second)
		e.statsTick()
		persistTick++
		if persistTick >= 2 {
			persistTick = 0
			e.persistStats()
		}
	}
}

func (e *Engine) statsTick() {
	e.statsMu.Lock()
	now := time.Now()

	// drain the 30s sample buffers into minute-bucketed samples
	up := atomic.SwapInt64(&minBufUp, 0)
	down := atomic.SwapInt64(&minBufDown, 0)
	conns := atomic.SwapInt64(&minBufConns, 0)
	if up > 0 || down > 0 || conns > 0 {
		e.stats.minutes = append(e.stats.minutes, MinuteStat{
			Ts:    now.Truncate(time.Minute),
			Up:    up,
			Down:  down,
			Conns: conns,
		})
		// keep ~48h of minute samples
		for len(e.stats.minutes) > 0 && now.Sub(e.stats.minutes[0].Ts) > 48*time.Hour {
			e.stats.minutes = e.stats.minutes[1:]
		}
	}

	// roll over to a new day
	ts := time.Now()
	key := ts.Format("2006-01-02")
	if key != e.stats.dayKey {
		if e.stats.day.Up > 0 || e.stats.day.Down > 0 || e.stats.day.Conns > 0 {
			d := e.stats.days[e.stats.dayKey]
			if d == nil {
				d = &DayStat{}
				e.stats.days[e.stats.dayKey] = d
			}
			d.Up += e.stats.day.Up
			d.Down += e.stats.day.Down
			d.Conns += e.stats.day.Conns
		}
		e.stats.day = DayStat{}
		e.stats.dayKey = key
		e.stats.minutes = nil
	}
	e.statsMu.Unlock()

	// evaluate the daily upload limit separately (atomic reads)
	e.mu.Lock()
	cur := e.settings
	e.mu.Unlock()
	if cur.DailyUploadLimit > 0 && e.upToday() >= cur.DailyUploadLimit {
		e.setUploadPaused(true)
	} else {
		e.setUploadPaused(false)
	}

	// apply the night schedule (rate caps) and ratio targets
	e.applySchedule(cur)
}

// applySchedule applies night-mode rate caps / full pauses and per-torrent
// ratio targets. It runs from the stats loop so timing changes happen without
// user action.
func (e *Engine) applySchedule(cur Settings) {
	inNight := cur.NightMode && nightIsActive(cur.NightStart, cur.NightEnd, time.Now())
	wantPause := inNight && cur.NightPause

	e.mu.Lock()
	applied := e.nightApplied
	e.mu.Unlock()

	if inNight && !applied {
		if cur.NightUpload > 0 {
			e.SetUploadLimit(cur.NightUpload)
		}
		if cur.NightDownload > 0 {
			e.SetDownloadLimit(cur.NightDownload)
		}
		e.mu.Lock()
		e.nightApplied = true
		e.mu.Unlock()
		e.Logf("night mode started (up=%d/down=%d)", cur.NightUpload, cur.NightDownload)
	} else if !inNight && applied {
		e.SetUploadLimit(cur.UploadLimit)
		e.SetDownloadLimit(cur.DownloadLimit)
		e.mu.Lock()
		e.nightApplied = false
		e.mu.Unlock()
		e.Logf("night mode ended, restored limits")
	}

	// Full pause during the night window. Upload is re-asserted every tick
	// because the daily-limit check may clear uploadPaused; on wake upload is
	// left to that same check so a daily cap is not bypassed.
	e.mu.Lock()
	paused := e.nightPaused
	e.mu.Unlock()
	if wantPause {
		e.setUploadPaused(true)
		if !paused {
			e.setDownloadPaused(true)
			e.mu.Lock()
			e.nightPaused = true
			e.mu.Unlock()
			e.Logf("night full pause engaged")
		}
	} else if paused {
		e.setDownloadPaused(false)
		e.mu.Lock()
		e.nightPaused = false
		e.mu.Unlock()
		e.Logf("night full pause released")
	}

	if cur.RatioTarget <= 0 && cur.SeedDays <= 0 && !hasSeedGoals(e) {
		return
	}
	e.mu.Lock()
	var list []*Torrent
	for _, t := range e.torrents {
		list = append(list, t)
	}
	e.mu.Unlock()
	for _, t := range list {
		t.mu.Lock()
		target := t.RatioTarget
		if target <= 0 {
			target = cur.RatioTarget
		}
		seed := t.SeedDaysTarget
		if seed <= 0 {
			seed = cur.SeedDays
		}
		seeding := t.State == StateSeeding
		up, down := t.Uploaded, t.Downloaded
		seedSince := t.SeedSince
		name := t.Name
		t.mu.Unlock()
		if !seeding {
			continue
		}
		reached := false
		reason := ""
		if target > 0 {
			ratio := float64(0)
			if down > 0 {
				ratio = float64(up) / float64(down)
			} else if up > 0 {
				ratio = float64(up)
			}
			if ratio >= target {
				reached = true
				reason = fmt.Sprintf("ratio %.2f >= %.2f", ratio, target)
			}
		}
		if !reached && seed > 0 && !seedSince.IsZero() {
			days := time.Since(seedSince).Hours() / 24
			if days >= float64(seed) {
				reached = true
				reason = fmt.Sprintf("seeded %.1f days >= %d", days, seed)
			}
		}
		if !reached {
			continue
		}
		t.mu.Lock()
		doRemove := cur.RatioRemove
		t.mu.Unlock()
		if doRemove {
			e.Logf("%s reached, removing %q", reason, name)
			if err := e.RemoveTorrent(t.ID); err != nil {
				e.Logf("goal remove %q failed: %v", name, err)
				continue
			}
			e.notify(NoticeRatio, t.ID, name)
		} else if cur.RatioStop {
			e.Logf("%s reached, stopping %q", reason, name)
			t.setState(StatePaused)
			e.notify(NoticeRatio, t.ID, name)
		}
	}
}

// hasSeedGoals reports whether any torrent has a per-torrent ratio/seed goal.
func hasSeedGoals(e *Engine) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, t := range e.torrents {
		t.mu.Lock()
		has := t.RatioTarget > 0 || t.SeedDaysTarget > 0
		t.mu.Unlock()
		if has {
			return true
		}
	}
	return false
}

// nightIsActive reports whether now is inside the [start,end) night window.
// The window may wrap midnight (e.g. 23:00 -> 07:00).
func nightIsActive(start, end string, now time.Time) bool {
	if start == "" || end == "" {
		return false
	}
	sh, sm := parseClock(start)
	eh, em := parseClock(end)
	if sh < 0 || eh < 0 {
		return false
	}
	cur := now.Hour()*60 + now.Minute()
	s := sh*60 + sm
	f := eh*60 + em
	if s < f {
		return cur >= s && cur < f
	}
	return cur >= s || cur < f
}

// parseClock parses "HH:MM" into hour and minute (-1,-1 when invalid).
func parseClock(v string) (int, int) {
	if len(v) != 5 || v[2] != ':' {
		return -1, -1
	}
	h, err1 := strconv.Atoi(v[:2])
	m, err2 := strconv.Atoi(v[3:])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return -1, -1
	}
	return h, m
}

// upToday returns uploaded bytes in the current calendar day.
func (e *Engine) upToday() int64 {
	e.statsMu.Lock()
	defer e.statsMu.Unlock()
	return e.stats.day.Up
}

func (e *Engine) setUploadPaused(p bool) {
	e.mu.Lock()
	e.uploadPaused = p
	e.mu.Unlock()
}

// UploadBlocked reports whether the daily upload limit has been reached.
func (e *Engine) UploadBlocked() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.uploadPaused
}

// setDownloadPaused holds or releases downloads (night full pause only).
func (e *Engine) setDownloadPaused(p bool) {
	e.mu.Lock()
	e.downloadPaused = p
	e.mu.Unlock()
}

// DownloadBlocked reports whether downloads are held (e.g. night pause).
func (e *Engine) DownloadBlocked() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.downloadPaused
}

// recordUp/recordDown accumulate byte counters on the hot path.
func (e *Engine) recordUp(n int64) {
	if n <= 0 {
		return
	}
	atomic.AddInt64(&e.byteUpRun, n)
	atomic.AddInt64(&minBufUp, n)
	e.statsMu.Lock()
	e.stats.day.Up += n
	e.statsMu.Unlock()
}

func (e *Engine) recordDown(n int64) {
	if n <= 0 {
		return
	}
	atomic.AddInt64(&e.byteDownRun, n)
	atomic.AddInt64(&minBufDown, n)
	e.statsMu.Lock()
	e.stats.day.Down += n
	e.statsMu.Unlock()
}

// recordConn counts a new peer connection and its country.
func (e *Engine) recordConn(addrHostport string) {
	cc := geo.Lookup(addrHostport)
	atomic.AddInt64(&minBufConns, 1)
	e.statsMu.Lock()
	e.stats.day.Conns++
	e.stats.connsRun++
	if cc != "" {
		e.stats.countries[cc]++
	}
	e.statsMu.Unlock()
}

// connsToday returns connections opened in the current day (persist-respecting).
func (e *Engine) connsToday() int64 {
	e.statsMu.Lock()
	defer e.statsMu.Unlock()
	return e.stats.day.Conns
}

// connsAllTime is the sum of all country connection counters.
func (e *Engine) connsAllTime() int64 {
	e.statsMu.Lock()
	defer e.statsMu.Unlock()
	var n int64
	for _, c := range e.stats.countries {
		n += c
	}
	return n
}

func (e *Engine) persistStats() {
	e.statsMu.Lock()
	p := PersistedStats{
		Since:     e.stats.since,
		Days:      e.stats.days,
		Years:     e.stats.years,
		Countries: e.stats.countries,
	}
	e.statsMu.Unlock()
	if e.statsFile == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(e.statsFile), 0755); err != nil {
		return
	}
	data, err := json.Marshal(p)
	if err != nil {
		return
	}
	tmp := e.statsFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err == nil {
		os.Rename(tmp, e.statsFile)
	}
}

// StatsSnapshot builds the dashboard stats payload.
func (e *Engine) StatsSnapshot() StatsView {
	e.statsMu.Lock()
	day := e.stats.day
	dayKey := e.stats.dayKey
	days := make(map[string]*DayStat, len(e.stats.days))
	for k, v := range e.stats.days {
		days[k] = v
	}
	years := make(map[string]*YearStat, len(e.stats.years))
	for k, v := range e.stats.years {
		years[k] = v
	}
	countries := make(map[string]int64, len(e.stats.countries))
	for k, v := range e.stats.countries {
		countries[k] = v
	}
	minutes := make([]MinuteStat, len(e.stats.minutes))
	copy(minutes, e.stats.minutes)
	e.statsMu.Unlock()

	e.mu.Lock()
	active := len(e.sessions)
	lim := e.settings.DailyUploadLimit
	paused := e.uploadPaused
	e.mu.Unlock()

	sv := StatsView{
		UpToday:          day.Up,
		DownToday:        day.Down,
		ConnsToday:       day.Conns,
		ConnActive:       active,
		ConnsTotal:       e.connsAllTime(),
		DailyUploadLimit: lim,
		UploadPaused:     paused,
		Since:            e.statsSince(),
	}

	// Totals: all-time = sum of years + today.
	for _, y := range years {
		sv.UpTotal += y.Up
		sv.DownTotal += y.Down
	}
	sv.UpTotal += day.Up
	sv.DownTotal += day.Down

	// Day view: group today's minute samples per 5 minutes (24 slots).
	sv.Hourly = hourlyPoints(minutes)

	// Month view: last 30 calendar days.
	sv.Daily = monthPoints(days, day, dayKey)

	// Year view: group days by calendar month (last 12) + today.
	sv.Monthly = yearPoints(days, day, dayKey)

	// All view: per-year totals.
	sv.Yearly = yearlyPoints(years, day, dayKey)

	// Countries, top 15.
	var list []cnt
	for cc, n := range countries {
		list = append(list, cnt{cc, n})
	}
	sortByConnsDesc(list)
	for i := 0; i < len(list) && i < 15; i++ {
		sv.Countries = append(sv.Countries, CountryStat{
			Code:  list[i].cc,
			Name:  countryName(list[i].cc),
			Conns: list[i].n,
		})
	}
	return sv
}

func (e *Engine) statsSince() time.Time {
	e.statsMu.Lock()
	defer e.statsMu.Unlock()
	return e.stats.since
}

type cnt struct{ cc string; n int64 }

func sortByConnsDesc(list []cnt) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j-1].n < list[j].n; j-- {
			list[j-1], list[j] = list[j], list[j-1]
		}
	}
}

// countryName maps an ISO code to a localized name.
var countryNames = map[string]string{
	"TR": "Türkiye", "DE": "Germany", "FR": "France", "US": "United States",
	"GB": "United Kingdom", "NL": "Netherlands", "RU": "Russia", "CN": "China",
	"JP": "Japan", "BR": "Brazil", "CA": "Canada", "AU": "Australia",
	"IN": "India", "IT": "Italy", "ES": "Spain", "PL": "Poland", "UA": "Ukraine",
	"SE": "Sweden", "NO": "Norway", "FI": "Finland", "DK": "Denmark",
	"CH": "Switzerland", "AT": "Austria", "BE": "Belgium", "PT": "Portugal",
	"IE": "Ireland", "CZ": "Czechia", "SK": "Slovakia", "HU": "Hungary",
	"RO": "Romania", "BG": "Bulgaria", "GR": "Greece", "IL": "Israel",
	"SA": "Saudi Arabia", "AE": "UAE", "IR": "Iran", "PK": "Pakistan",
	"BD": "Bangladesh", "KR": "South Korea", "TW": "Taiwan", "HK": "Hong Kong",
	"SG": "Singapore", "MY": "Malaysia", "ID": "Indonesia", "TH": "Thailand",
	"VN": "Vietnam", "PH": "Philippines", "MX": "Mexico", "AR": "Argentina",
	"CL": "Chile", "CO": "Colombia", "PE": "Peru", "VE": "Venezuela",
	"EG": "Egypt", "ZA": "South Africa", "NG": "Nigeria", "KE": "Kenya",
	"MA": "Morocco", "DZ": "Algeria", "NZ": "New Zealand",
}

func countryName(cc string) string {
	if n, ok := countryNames[cc]; ok {
		return n
	}
	return cc
}

// hourlyPoints aggregates minute samples into the last 24 hour slots.
func hourlyPoints(minutes []MinuteStat) []StatPoint {
	type acc struct{ up, down int64 }
	buckets := make(map[string]*acc)
	var keys []string
	for _, m := range minutes {
		key := m.Ts.Truncate(time.Hour).Format("2006-01-02 15")
		a := buckets[key]
		if a == nil {
			a = &acc{}
			buckets[key] = a
			keys = append(keys, key)
		}
		a.up += m.Up
		a.down += m.Down
	}
	sortStrings(keys)
	out := make([]StatPoint, 0, len(keys))
	for _, k := range keys {
		a := buckets[k]
		out = append(out, StatPoint{Label: k[11:13] + ":00", Up: a.up, Down: a.down})
	}
	return out
}

// monthPoints lists the last 30 calendar days including today.
func monthPoints(days map[string]*DayStat, today DayStat, todayKey string) []StatPoint {
	keys := make([]string, 0, len(days))
	for k := range days {
		keys = append(keys, k)
	}
	sortStrings(keys)
	var out []StatPoint
	for _, k := range keys[lastN(keys, 29):] {
		d := days[k]
		out = append(out, StatPoint{Label: monthLabel(k), Up: d.Up, Down: d.Down})
	}
	out = append(out, StatPoint{Label: monthLabel(todayKey), Up: today.Up, Down: today.Down})
	return out
}

// yearPoints groups days into the last 12 calendar months (including today).
func yearPoints(days map[string]*DayStat, today DayStat, todayKey string) []StatPoint {
	monthAgg := make(map[string]*DayStat)
	for k, d := range days {
		key := k[:7]
		m := monthAgg[key]
		if m == nil {
			m = &DayStat{}
			monthAgg[key] = m
		}
		m.Up += d.Up
		m.Down += d.Down
	}
	if len(todayKey) >= 7 {
		key := todayKey[:7]
		m := monthAgg[key]
		if m == nil {
			m = &DayStat{}
			monthAgg[key] = m
		}
		m.Up += today.Up
		m.Down += today.Down
	}
	keys := make([]string, 0, len(monthAgg))
	for k := range monthAgg {
		keys = append(keys, k)
	}
	sortStrings(keys)
	var out []StatPoint
	for _, k := range keys[lastN(keys, 12):] {
		d := monthAgg[k]
		out = append(out, StatPoint{Label: monthShortLabel(k), Up: d.Up, Down: d.Down})
	}
	return out
}

// yearlyPoints totals per calendar year.
func yearlyPoints(years map[string]*YearStat, today DayStat, todayKey string) []StatPoint {
	y := make(map[string]*YearStat)
	for k, v := range years {
		yy := y[k]
		if yy == nil {
			yy = &YearStat{}
			y[k] = yy
		}
		yy.Up += v.Up
		yy.Down += v.Down
	}
	if len(todayKey) >= 4 {
		yy := y[todayKey[:4]]
		if yy == nil {
			yy = &YearStat{}
			y[todayKey[:4]] = yy
		}
		yy.Up += today.Up
		yy.Down += today.Down
	}
	keys := make([]string, 0, len(y))
	for k := range y {
		keys = append(keys, k)
	}
	sortStrings(keys)
	var out []StatPoint
	for _, k := range keys {
		d := y[k]
		out = append(out, StatPoint{Label: k, Up: d.Up, Down: d.Down})
	}
	return out
}

func lastN(keys []string, n int) int {
	if len(keys) <= n {
		return 0
	}
	return len(keys) - n
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// monthLabel turns "2026-09-04" into "04.09".
func monthLabel(k string) string {
	if len(k) < 10 {
		return k
	}
	return k[8:10] + "." + k[5:7]
}

// monthShortLabel turns "2026-09" into "09".
func monthShortLabel(k string) string {
	if len(k) < 7 {
		return k
	}
	return k[5:7]
}