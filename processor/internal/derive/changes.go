package derive

import (
	"encoding/json"
	"maps"
	"slices"
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
			batch = append(batch, map[string]any{
				"al": pair.airline, "c": cabins, "d": dayDate(day),
				"k": kind, "r": pair.route, "t": sourceTime,
			})
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
