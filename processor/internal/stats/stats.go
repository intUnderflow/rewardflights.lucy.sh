// Package stats accumulates per-(route, cabin) award-availability climate:
// how often seats APPEAR and how quickly they are SNAPPED UP. The site's
// availability views answer "what is open now?"; stats.json answers "what
// are my odds?" — the map's climate mode reads it.
//
// Rules (each pinned by a test):
//   - An APPEARANCE is a cabin bit turning on for a (route, date) inside the
//     previously-encoded horizon — churn/reopenings, mirroring the changes
//     feed. Horizon growth (the T-355 release crawl) is NOT an appearance...
//   - ...but frontier opens DO start a survival run at their release time:
//     "released at T-355, gone in 2h" is the sharpest snipe signal there is.
//   - A close completes a survival run only when the true open time is known.
//     Runs seeded at baseline (process start / new route) carry openedAt=0
//     and complete silently — measuring them would fabricate lifetimes.
//   - A date that rolls into the past while still open was NOT snapped up:
//     the run is censored (dropped), never counted as a close.
//   - Appearance events bucket by TRAVEL month (the month you'd fly), which
//     is what the map's month filter means.
//
// State persists across restarts (JSON, atomic writes). Emission into the
// data repo is throttled to at most once per emitEvery so the stats file
// never recreates per-cycle commit churn.
package stats

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	StateSchema = 1
	emitEvery   = time.Hour
	// Survival buckets, in seconds: <1h, 1-6h, 6-24h, 1-3d, 3d+.
	bucket1 = 3600
	bucket2 = 6 * 3600
	bucket3 = 24 * 3600
	bucket4 = 3 * 24 * 3600
)

var cabinLetters = []struct {
	bit    int
	letter string
}{{1, "M"}, {2, "W"}, {4, "C"}, {8, "F"}}

// Agg is one (route, cabin)'s accumulated climate.
type Agg struct {
	Total  int     `json:"n"` // appearance events observed
	Months [12]int `json:"m"` // appearances by travel month (0 = January)
	Surv   [5]int  `json:"s"` // completed-run lifetimes: <1h, 1-6h, 6-24h, 1-3d, 3d+
	Done   int     `json:"d"` // completed runs measured
}

type stateData struct {
	Schema   int              `json:"schema"`
	Since    int64            `json:"since"`    // observation start (source time)
	LastEmit int64            `json:"lastEmit"` // last stats.json write (source time)
	Open     map[string]int64 `json:"open"`     // route|cabin|day -> openedAt (0 = unknown/baseline)
	Agg      map[string]*Agg  `json:"agg"`      // route|cabin -> aggregates
}

// Accumulator ingests bundle generations. Not safe for concurrent use; the
// watch loop is its only caller.
type Accumulator struct {
	state     *stateData
	path      string // state file; empty = memory only (tests)
	deferSave bool   // backfill: skip per-cycle persistence, Flush at the end
	logf      func(format string, args ...any)
}

// DeferSaves suspends per-Cycle state persistence. The backfill replays tens
// of thousands of generations, and a multi-MB state marshal per cycle
// dominates the whole run (measured: ~115 gen/min saving vs thousands
// without). Pair with Flush.
func (a *Accumulator) DeferSaves() { a.deferSave = true }

// Flush re-enables persistence and writes the state once.
func (a *Accumulator) Flush() {
	a.deferSave = false
	a.save()
}

func New(statePath string, logf func(string, ...any)) *Accumulator {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	a := &Accumulator{path: statePath, logf: logf}
	a.state = loadState(statePath, logf)
	return a
}

// Cycle ingests one generation transition. oldRaw nil (or unparseable) means
// no previous state: everything open is baseline-seeded and nothing counts.
func (a *Accumulator) Cycle(oldRaw, newRaw []byte, sourceTime int64) {
	nb, _, err := parseBundle(newRaw)
	if err != nil {
		a.logf("stats: unparseable new bundle: %v", err)
		return
	}
	cutoffDay := int(sourceTime / 86400)

	if a.state.Since == 0 {
		a.state.Since = sourceTime
	}

	ob, oldHorizon, oldErr := parseBundle(oldRaw)

	seen := map[string]bool{}
	for route, newDays := range nb {
		oldDays := ob[route]
		routeKnown := oldErr == nil && oldDays != nil
		days := map[int]bool{}
		for day := range newDays {
			days[day] = true
		}
		for day := range oldDays {
			days[day] = true // a day that closed to zero exists only here
		}
		for day := range days {
			if day < cutoffDay {
				continue
			}
			newBits, oldBits := newDays[day], oldDays[day]
			for _, c := range cabinLetters {
				key := route + "|" + c.letter + "|" + strconv.Itoa(day)
				seen[key] = true
				has, had := newBits&c.bit != 0, oldBits&c.bit != 0
				switch {
				case has && !had:
					if !routeKnown {
						// Baseline / brand-new route: open run with unknown start.
						a.state.Open[key] = 0
					} else if day > oldHorizon {
						// Frontier: the booking window reaching this date is not
						// an "appearance", but its run starts NOW — release-day
						// snipe speed is measured from here.
						a.state.Open[key] = sourceTime
					} else {
						agg := a.agg(route, c.letter)
						agg.Total++
						agg.Months[monthOfDay(day)]++
						a.state.Open[key] = sourceTime
					}
				case has && had:
					if _, tracked := a.state.Open[key]; !tracked {
						a.state.Open[key] = 0 // restart mid-run: unknown start
					}
				case !has && had && routeKnown:
					if openedAt, ok := a.state.Open[key]; ok {
						if openedAt > 0 && sourceTime > openedAt {
							agg := a.agg(route, c.letter)
							agg.Surv[survBucket(sourceTime-openedAt)]++
							agg.Done++
						}
						delete(a.state.Open, key)
					}
				}
			}
		}
	}
	_ = fmt.Sprintf // fmt retained for error paths
	// Censor runs whose (route, date) vanished from view: dates that rolled
	// into the past were not snapped up, and routes dropped from the bundle
	// tell us nothing. seen covers every live (route, day) key.
	for key := range a.state.Open {
		if !seen[key] {
			delete(a.state.Open, key)
		}
	}
	if !a.deferSave {
		a.save()
	}
}

// EmitIfDue writes stats.json under outDir at most once per emitEvery.
// Returns true when it wrote.
func (a *Accumulator) EmitIfDue(outDir string, sourceTime int64) bool {
	if a.state.Since == 0 || sourceTime-a.state.LastEmit < int64(emitEvery/time.Second) {
		return false
	}
	raw, err := a.Render(sourceTime)
	if err != nil {
		a.logf("stats: render: %v", err)
		return false
	}
	if err := os.WriteFile(filepath.Join(outDir, "stats.json"), raw, 0o644); err != nil {
		a.logf("stats: write: %v", err)
		return false
	}
	a.state.LastEmit = sourceTime
	a.save()
	return true
}

// Render produces the public stats.json bytes: per route, per cabin with any
// events, the weekly appearance rate (one decimal — rounding is also churn
// hygiene), raw counts for confidence display, survival histogram, and
// travel-month spread. Deterministic: map keys sort in encoding/json.
func (a *Accumulator) Render(sourceTime int64) ([]byte, error) {
	weeks := math.Max(float64(sourceTime-a.state.Since)/86400.0, 1) / 7.0
	type cabOut struct {
		W float64 `json:"w"`           // appearances per week
		N int     `json:"n"`           // appearance events observed
		D int     `json:"d,omitempty"` // completed survival runs
		S *[5]int `json:"s,omitempty"` // survival histogram
		M [12]int `json:"m"`           // appearances by travel month
	}
	routes := map[string]map[string]cabOut{}
	for key, agg := range a.state.Agg {
		if agg.Total == 0 && agg.Done == 0 {
			continue
		}
		sep := strings.LastIndexByte(key, '|')
		route, letter := key[:sep], key[sep+1:]
		out := cabOut{
			W: math.Round(float64(agg.Total)/weeks*10) / 10,
			N: agg.Total, M: agg.Months,
		}
		if agg.Done > 0 {
			out.D = agg.Done
			s := agg.Surv
			out.S = &s
		}
		if routes[route] == nil {
			routes[route] = map[string]cabOut{}
		}
		routes[route][letter] = out
	}
	return json.Marshal(map[string]any{
		"schema": 1, "since": a.state.Since, "t": sourceTime, "routes": routes,
	})
}

func (a *Accumulator) agg(route, letter string) *Agg {
	key := route + "|" + letter
	if a.state.Agg[key] == nil {
		a.state.Agg[key] = &Agg{}
	}
	return a.state.Agg[key]
}

func survBucket(secs int64) int {
	switch {
	case secs < bucket1:
		return 0
	case secs < bucket2:
		return 1
	case secs < bucket3:
		return 2
	case secs < bucket4:
		return 3
	}
	return 4
}

// monthOfDay is the UTC calendar month (0-11) of an absolute day number.
func monthOfDay(day int) int {
	return int(time.Unix(int64(day)*86400, 0).UTC().Month()) - 1
}

// parseBundle reduces availability.json bytes to route -> day -> merged cabin
// bits (absolute day numbers), plus the bundle's last encoded day.
func parseBundle(raw []byte) (map[string]map[int]int, int, error) {
	if raw == nil {
		return nil, 0, fmt.Errorf("no bundle")
	}
	var b struct {
		Epoch  string `json:"epoch"`
		Routes map[string]struct {
			A map[string]string `json:"a"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, 0, err
	}
	t, err := time.Parse("2006-01-02", b.Epoch)
	if err != nil {
		return nil, 0, fmt.Errorf("bundle epoch: %w", err)
	}
	epochDay := int(t.Unix() / 86400)
	out := map[string]map[int]int{}
	horizon := 0
	for route, entry := range b.Routes {
		days := map[int]int{}
		for _, s := range entry.A {
			for i := 0; i < len(s); i++ {
				if v := hexBits(s[i]); v > 0 {
					days[epochDay+i] |= v
				}
			}
			if h := epochDay + len(s) - 1; h > horizon {
				horizon = h
			}
		}
		out[route] = days
	}
	return out, horizon, nil
}

func hexBits(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	}
	return -1
}

// --- persistence ---------------------------------------------------------

func loadState(path string, logf func(string, ...any)) *stateData {
	fresh := &stateData{Schema: StateSchema, Open: map[string]int64{}, Agg: map[string]*Agg{}}
	if path == "" {
		return fresh
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fresh
	}
	var s stateData
	if json.Unmarshal(raw, &s) != nil || s.Schema != StateSchema {
		logf("stats: state reset (%s)", path)
		return fresh
	}
	if s.Open == nil {
		s.Open = map[string]int64{}
	}
	if s.Agg == nil {
		s.Agg = map[string]*Agg{}
	}
	return &s
}

func (a *Accumulator) save() {
	if a.path == "" {
		return
	}
	raw, err := json.Marshal(a.state)
	if err != nil {
		a.logf("stats: state marshal: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(a.path), 0o755); err != nil {
		a.logf("stats: state dir: %v", err)
		return
	}
	tmp := a.path + ".tmp"
	if os.WriteFile(tmp, raw, 0o644) == nil {
		if err := os.Rename(tmp, a.path); err != nil {
			os.Remove(tmp)
			a.logf("stats: state save: %v", err)
		}
	}
}
