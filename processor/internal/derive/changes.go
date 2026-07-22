package derive

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
)

// maxChangeEntries is the rolling cap of changes/recent.json.
const maxChangeEntries = 1000

// buildChanges produces the entry list for changes/recent.json: the diff of
// the previous availability.json (oldBundle, as found in -out) against the
// new dataset, prepended to the previous feed's entries, trimmed to
// maxChangeEntries.
//
// Rules:
//   - No parseable old bundle -> no diff is possible: emit no new entries but
//     preserve the previous feed's entries when that file is parseable.
//   - Dates before cutoffDay (the new source commit's UTC date) are excluded:
//     a date that merely rolled into the past is not "closed".
//   - The new batch is sorted by route, then date, then airline id; previous
//     entries keep their order (newest first overall).
//
// Deterministic given (old state, new state): an immediate re-run diffs
// nothing and reproduces the file byte-identically.
func buildChanges(oldBundle, oldChanges []byte, newBits map[string]map[string]map[int]int, cutoffDay int, sourceTime int64) []any {
	var prior []json.RawMessage
	if oldChanges != nil {
		var prev struct {
			Entries []json.RawMessage `json:"entries"`
		}
		if err := json.Unmarshal(oldChanges, &prev); err == nil {
			prior = prev.Entries
		}
	}

	entries := []any{}
	for _, e := range diffBundles(oldBundle, newBits, cutoffDay, sourceTime) {
		entries = append(entries, e)
	}
	for _, e := range prior {
		entries = append(entries, e)
	}
	if len(entries) > maxChangeEntries {
		entries = entries[:maxChangeEntries]
	}
	return entries
}

// maxPinnedPerCabin is the per-cabin floor of "opened" history the feed
// guarantees beyond the contiguous window. The 1000-entry window often spans
// only an hour or two on a busy day, and First openings are rare on that
// timescale — without a floor, a cabin-filtered "Recently opened" would
// vanish instead of reaching back to real news.
const maxPinnedPerCabin = 40

// changeEntry is the typed view of one feed entry (new-batch maps and prior
// RawMessages both normalize to this).
type changeEntry struct {
	Al string `json:"al"`
	C  string `json:"c"`
	D  string `json:"d"`
	G  string `json:"g,omitempty"`
	K  string `json:"k"`
	R  string `json:"r"`
	T  int64  `json:"t"`
}

// gainedCabins is what an event newly made available: an opening gains its
// whole cabin set, a change gains its "g" letters, everything else nothing.
func gainedCabins(ce changeEntry) string {
	switch ce.K {
	case "opened":
		return ce.C
	case "changed":
		return ce.G
	}
	return ""
}

// buildPinned computes the feed's "pinned" array: per cabin, the newest
// openings older than the contiguous window, carried forward from the
// previous feed (entries + pinned) so they survive roll-off. Truthfulness
// rules:
//   - A (route, airline, date) already present in the window is never pinned
//     — the window's newer event supersedes the old story.
//   - Only dates whose NEWEST known event is "opened" qualify: a date that
//     opened and later closed must not be resurrected.
//   - A travel date before cutoffDay is dead news and is dropped.
//
// Deterministic given (window, old feed): re-runs reproduce the array
// byte-identically (stable sort, canonical map keys).
func buildPinned(window []any, oldChanges []byte, cutoffDay int) []any {
	normalize := func(e any) (changeEntry, bool) {
		b, err := json.Marshal(e)
		if err != nil {
			return changeEntry{}, false
		}
		var ce changeEntry
		if json.Unmarshal(b, &ce) != nil || ce.R == "" || ce.D == "" {
			return changeEntry{}, false
		}
		return ce, true
	}
	ident := func(ce changeEntry) string { return ce.R + "|" + ce.Al + "|" + ce.D }

	inWindow := map[string]bool{}
	newest := map[string]changeEntry{}
	consider := func(ce changeEntry) {
		k := ident(ce)
		if cur, ok := newest[k]; !ok || ce.T > cur.T {
			newest[k] = ce
		}
	}
	for _, e := range window {
		if ce, ok := normalize(e); ok {
			inWindow[ident(ce)] = true
			consider(ce)
		}
	}
	if oldChanges != nil {
		var prev struct {
			Entries []json.RawMessage `json:"entries"`
			Pinned  []json.RawMessage `json:"pinned"`
		}
		if json.Unmarshal(oldChanges, &prev) == nil {
			for _, raw := range append(prev.Entries, prev.Pinned...) {
				if ce, ok := normalize(raw); ok {
					consider(ce)
				}
			}
		}
	}

	var pool []changeEntry
	for k, ce := range newest {
		// The newest event for the identity decides the current story: it must
		// itself be a gain (an opening, or a change that added cabins) — a date
		// whose latest event closed it or merely shuffled existing cabins is
		// not pinnable news.
		if inWindow[k] || gainedCabins(ce) == "" {
			continue
		}
		if d, err := parseDay(ce.D); err != nil || d < cutoffDay {
			continue // departed (or unparseable) travel date: dead news
		}
		pool = append(pool, ce)
	}
	slices.SortFunc(pool, func(a, b changeEntry) int {
		if a.T != b.T {
			if a.T > b.T {
				return -1
			}
			return 1
		}
		for _, pair := range [][2]string{{a.R, b.R}, {a.D, b.D}, {a.Al, b.Al}} {
			if pair[0] != pair[1] {
				if pair[0] < pair[1] {
					return -1
				}
				return 1
			}
		}
		return 0
	})

	picked := map[string]bool{}
	out := []any{} // non-nil: an empty floor serializes as [], matching entries
	for _, cabin := range []string{"M", "W", "C", "F"} {
		kept := 0
		for _, ce := range pool {
			if kept >= maxPinnedPerCabin {
				break
			}
			if !strings.Contains(gainedCabins(ce), cabin) {
				continue
			}
			kept++
			if picked[ident(ce)] {
				continue // already pinned for another cabin it also gained
			}
			picked[ident(ce)] = true
			m := map[string]any{
				"al": ce.Al, "c": ce.C, "d": ce.D, "k": ce.K, "r": ce.R, "t": ce.T,
			}
			if ce.G != "" {
				m["g"] = ce.G
			}
			out = append(out, m)
		}
	}
	// Re-sort the union newest-first so the array reads like the entries do.
	slices.SortFunc(out, func(a, b any) int {
		am, bm := a.(map[string]any), b.(map[string]any)
		at, bt := am["t"].(int64), bm["t"].(int64)
		if at != bt {
			if at > bt {
				return -1
			}
			return 1
		}
		for _, k := range []string{"r", "d", "al"} {
			av, bv := am[k].(string), bm[k].(string)
			if av != bv {
				if av < bv {
					return -1
				}
				return 1
			}
		}
		return 0
	})
	return out
}

// diffBundles decodes the previous bundle and diffs it against the new
// route/airline/day bit state, returning the new batch of change entries.
//
// Days outside the OLD bundle's encoded window are excluded on both edges:
// days before cutoffDay merely rolled into the past (not "closed"), and days
// beyond the old bundle's last encoded day are horizon growth — the scraper's
// booking window advancing — not "recently opened". Without the latter guard
// every daily horizon advance would flood ~one noise entry per route into
// the capped feed, evicting genuine changes within days.
func diffBundles(oldBundle []byte, newBits map[string]map[string]map[int]int, cutoffDay int, sourceTime int64) []map[string]any {
	oldState, oldHorizon, ok := decodeBundle(oldBundle)
	if !ok {
		return nil
	}

	type key struct{ route, airline string }
	pairs := map[key]bool{}
	for route, byAirline := range oldState {
		for id := range byAirline {
			pairs[key{route, id}] = true
		}
	}
	for route, byAirline := range newBits {
		for id := range byAirline {
			pairs[key{route, id}] = true
		}
	}

	var batch []map[string]any
	for pair := range pairs {
		oldDays := oldState[pair.route][pair.airline]
		newDays := newBits[pair.route][pair.airline]
		days := map[int]bool{}
		for d := range oldDays {
			days[d] = true
		}
		for d := range newDays {
			days[d] = true
		}
		for day := range days {
			if day < cutoffDay {
				continue // rolled into the past, not a change
			}
			if day > oldHorizon {
				continue // beyond the old encoded horizon: growth, not a change
			}
			oldB, newB := oldDays[day], newDays[day]
			if oldB == newB {
				continue
			}
			kind, cabins := "changed", cabinLetters(newB)
			switch {
			case oldB == 0:
				kind = "opened"
			case newB == 0:
				kind, cabins = "closed", cabinLetters(oldB)
			}
			entry := map[string]any{
				"al": pair.airline, "c": cabins, "d": dayDate(day),
				"k": kind, "r": pair.route, "t": sourceTime,
			}
			// A "changed" event names what it GAINED ("g"): rare cabins (First
			// above all) almost never open a date from nothing — they get added
			// to dates other cabins already hold, and without g a cabin-filtered
			// "Recently opened" would structurally never see them.
			if kind == "changed" {
				if gained := cabinLetters(newB &^ oldB); gained != "" {
					entry["g"] = gained
				}
			}
			batch = append(batch, entry)
		}
	}
	slices.SortFunc(batch, func(a, b map[string]any) int {
		for _, k := range []string{"r", "d", "al"} {
			av, bv := a[k].(string), b[k].(string)
			if av != bv {
				if av < bv {
					return -1
				}
				return 1
			}
		}
		return 0
	})
	return batch
}

// decodeBundle extracts route -> airline id -> day number -> bits from a
// previous availability.json, along with the bundle's last encoded day
// (horizon = old epoch day + longest nibble string - 1, in the OLD epoch).
// Any parse problem means "no previous state" (ok=false); nibble digits
// outside 1..F are ignored.
func decodeBundle(raw []byte) (state map[string]map[string]map[int]int, horizonDay int, ok bool) {
	if raw == nil {
		return nil, 0, false
	}
	var bundle struct {
		Epoch  string `json:"epoch"`
		Routes map[string]struct {
			A map[string]string `json:"a"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(raw, &bundle); err != nil {
		return nil, 0, false
	}
	epochDay, err := parseDay(bundle.Epoch)
	if err != nil {
		return nil, 0, false
	}
	maxLen := 0
	state = map[string]map[string]map[int]int{}
	for _, route := range slices.Sorted(maps.Keys(bundle.Routes)) {
		byAirline := map[string]map[int]int{}
		for id, s := range bundle.Routes[route].A {
			if len(s) > maxLen {
				maxLen = len(s)
			}
			byDay := map[int]int{}
			for i := 0; i < len(s); i++ {
				if bits := hexBits(s[i]); bits > 0 {
					byDay[epochDay+i] = bits
				}
			}
			byAirline[id] = byDay
		}
		state[route] = byAirline
	}
	return state, epochDay + maxLen - 1, true
}
