package derive

import (
	"encoding/json"
	"fmt"
	"testing"
)

// feedFile builds a previous changes/recent.json with the given entries and
// pinned arrays.
func feedFile(t *testing.T, entries, pinned []map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"schema": 1, "entries": entries, "pinned": pinned})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fe(r, al, d, k, c string, ts int64) map[string]any {
	return map[string]any{"r": r, "al": al, "d": d, "k": k, "c": c, "t": ts}
}

func pinnedStrings(t *testing.T, pinned []any) []string {
	t.Helper()
	var out []string
	for _, e := range pinned {
		m := e.(map[string]any)
		out = append(out, fmt.Sprintf("%v %v %v %v t=%v", m["k"], m["r"], m["d"], m["c"], m["t"]))
	}
	return out
}

func TestPinnedKeepsRareCabinBeyondWindow(t *testing.T) {
	cutoff := day(t, "2026-01-01")
	// The window has only Economy news; an old First opening lives in the
	// previous feed's entries and must be pinned.
	window := []any{fe("AAA-BBB", "BA", "2026-03-01", "opened", "M", 100)}
	old := feedFile(t, []map[string]any{
		fe("CCC-DDD", "BA", "2026-04-01", "opened", "F", 50),
	}, nil)
	got := pinnedStrings(t, buildPinned(window, old, cutoff))
	want := []string{"opened CCC-DDD 2026-04-01 F t=50"}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("pinned = %v, want %v", got, want)
	}
}

func TestPinnedSupersededByWindow(t *testing.T) {
	cutoff := day(t, "2026-01-01")
	// The same (route, airline, date) is in the window with a newer event:
	// the old story must not be pinned alongside it.
	window := []any{fe("CCC-DDD", "BA", "2026-04-01", "closed", "F", 100)}
	old := feedFile(t, []map[string]any{
		fe("CCC-DDD", "BA", "2026-04-01", "opened", "F", 50),
	}, nil)
	if got := buildPinned(window, old, cutoff); len(got) != 0 {
		t.Fatalf("superseded entry pinned: %v", pinnedStrings(t, got))
	}
}

func TestPinnedNeverResurrectsReclosedDates(t *testing.T) {
	cutoff := day(t, "2026-01-01")
	// Old feed: the date opened (t=50) then closed (t=80); its newest known
	// event is "closed", so it must not be pinned as an opening.
	old := feedFile(t, []map[string]any{
		fe("CCC-DDD", "BA", "2026-04-01", "closed", "F", 80),
		fe("CCC-DDD", "BA", "2026-04-01", "opened", "F", 50),
	}, nil)
	if got := buildPinned(nil, old, cutoff); len(got) != 0 {
		t.Fatalf("re-closed date resurrected: %v", pinnedStrings(t, got))
	}
}

func TestPinnedDropsDepartedTravelDates(t *testing.T) {
	cutoff := day(t, "2026-06-01")
	old := feedFile(t, []map[string]any{
		fe("CCC-DDD", "BA", "2026-05-20", "opened", "F", 50), // travel date passed
		fe("EEE-FFF", "BA", "2026-07-01", "opened", "F", 40),
	}, nil)
	got := pinnedStrings(t, buildPinned(nil, old, cutoff))
	if len(got) != 1 || got[0] != "opened EEE-FFF 2026-07-01 F t=40" {
		t.Fatalf("pinned = %v", got)
	}
}

func TestPinnedCarriesForwardAcrossCycles(t *testing.T) {
	cutoff := day(t, "2026-01-01")
	// The opening lives only in the previous feed's PINNED array (long since
	// rolled off entries) — it must keep surviving cycle after cycle.
	old := feedFile(t, nil, []map[string]any{
		fe("CCC-DDD", "BA", "2026-04-01", "opened", "F", 50),
	})
	first := buildPinned(nil, old, cutoff)
	if len(first) != 1 {
		t.Fatalf("pinned lost from old pinned array: %v", pinnedStrings(t, first))
	}
	// Determinism: identical inputs reproduce identical bytes.
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(buildPinned(nil, old, cutoff))
	if string(a) != string(b) {
		t.Fatalf("non-deterministic pinned: %s vs %s", a, b)
	}
}

func TestPinnedPerCabinCap(t *testing.T) {
	cutoff := day(t, "2026-01-01")
	// One origin flooding one cabin: the global bucket caps it, and the origin
	// pass adds nothing new (its newest is already pinned).
	var entries []map[string]any
	for i := 0; i < maxPinnedPerCabin+5; i++ {
		entries = append(entries, fe(fmt.Sprintf("AAA-B%02d", i), "BA", "2026-04-01", "opened", "F", int64(1000+i)))
	}
	got := buildPinned(nil, feedFile(t, entries, nil), cutoff)
	if len(got) != maxPinnedPerCabin {
		t.Fatalf("pinned = %d entries, want the %d cap", len(got), maxPinnedPerCabin)
	}
	// Newest-first: the cap keeps the newest, drops the oldest.
	first := got[0].(map[string]any)
	if first["t"].(int64) != int64(1000+maxPinnedPerCabin+4) {
		t.Fatalf("newest not kept first: t=%v", first["t"])
	}
}

// TestPinnedFloorKeepsPerOrigin: one busy origin's churn fills the global
// cabin bucket; a quieter origin's older gain in the same cabin must still be
// pinned (its newest per cabin), or an origin-filtered "Recently opened"
// runs dry.
func TestPinnedFloorKeepsPerOrigin(t *testing.T) {
	cutoff := day(t, "2026-01-01")
	var entries []map[string]any
	for i := 0; i < maxPinnedPerCabin+5; i++ {
		entries = append(entries, fe(fmt.Sprintf("LON-B%02d", i), "BA", "2026-04-01", "opened", "C", int64(1000+i)))
	}
	entries = append(entries,
		fe("DUB-LON", "BA", "2026-04-02", "opened", "C", 400), // newest DUB C
		fe("DUB-LON", "BA", "2026-04-03", "opened", "C", 300)) // older: not kept
	got := pinnedStrings(t, buildPinned(nil, feedFile(t, entries, nil), cutoff))
	var dub []string
	for _, s := range got {
		if len(s) > 7 && s[7:10] == "DUB" {
			dub = append(dub, s)
		}
	}
	if len(dub) != 1 || dub[0] != "opened DUB-LON 2026-04-02 C t=400" {
		t.Fatalf("origin floor wrong: %v", dub)
	}
}

func TestChangedEntriesEmitGainedCabins(t *testing.T) {
	// Old: 2026-01-02 = M (1). New: MF (9) — the change GAINED First.
	old := oldBundleJSON(t, "2026-01-01", map[string]map[string]string{
		"AAA-BBB": {"BA": "010"},
	})
	newBits := map[string]map[string]map[int]int{
		"AAA-BBB": {"BA": {day(t, "2026-01-02"): 9}},
	}
	entries := buildChanges(old, nil, newBits, day(t, "2026-01-01"), 42)
	if len(entries) != 1 {
		t.Fatalf("got %d entries: %v", len(entries), entries)
	}
	m := entries[0].(map[string]any)
	if m["k"] != "changed" || m["c"] != "MF" || m["g"] != "F" {
		t.Fatalf("entry = %v, want changed c=MF g=F", m)
	}
	// A shuffle that gains nothing (MF -> M) carries no g key.
	shrink := buildChanges(oldBundleJSON(t, "2026-01-01", map[string]map[string]string{
		"AAA-BBB": {"BA": "090"},
	}), nil, map[string]map[string]map[int]int{
		"AAA-BBB": {"BA": {day(t, "2026-01-02"): 1}},
	}, day(t, "2026-01-01"), 43)
	if m := shrink[0].(map[string]any); m["k"] != "changed" || m["g"] != nil {
		t.Fatalf("no-gain change carries g: %v", m)
	}
}

func TestPinnedViaGainedCabin(t *testing.T) {
	cutoff := day(t, "2026-01-01")
	// First arrived on an already-open date (changed, g=F): pinnable for F.
	gained := fe("CCC-DDD", "BA", "2026-04-01", "changed", "MF", 50)
	gained["g"] = "F"
	// A cabin shuffle with no gains is never pinnable.
	shuffle := fe("EEE-FFF", "BA", "2026-04-01", "changed", "MC", 60)
	old := feedFile(t, []map[string]any{shuffle, gained}, nil)
	got := pinnedStrings(t, buildPinned(nil, old, cutoff))
	if len(got) != 1 || got[0] != "changed CCC-DDD 2026-04-01 MF t=50" {
		t.Fatalf("pinned = %v", got)
	}
}

func TestNewPairsAreBaselineNotNews(t *testing.T) {
	// Old bundle: one route, one airline. New bundle: the same, plus a new
	// airline onboarding onto the existing route AND a brand-new route —
	// neither may emit a single entry (a genuine later change still does).
	old := oldBundleJSON(t, "2026-01-01", map[string]map[string]string{
		"AAA-BBB": {"BA": "010"},
	})
	newBits := map[string]map[string]map[int]int{
		"AAA-BBB": {
			"BA": {day(t, "2026-01-02"): 1},                            // unchanged
			"EI": {day(t, "2026-01-02"): 4, day(t, "2026-01-03"): 4}, // onboarding
		},
		"CCC-DDD": {"EI": {day(t, "2026-01-02"): 1}}, // brand-new route
	}
	entries := buildChanges(old, nil, newBits, day(t, "2026-01-01"), 42)
	if len(entries) != 0 {
		t.Fatalf("onboarding produced %d entries: %v", len(entries), entries)
	}
	// Next cycle: the onboarded airline gains a day — that IS news.
	old2 := oldBundleJSON(t, "2026-01-01", map[string]map[string]string{
		"AAA-BBB": {"BA": "010", "EI": "044"},
	})
	newBits2 := map[string]map[string]map[int]int{
		"AAA-BBB": {"BA": {day(t, "2026-01-02"): 1},
			"EI": {day(t, "2026-01-01"): 4, day(t, "2026-01-02"): 4, day(t, "2026-01-03"): 4}},
	}
	entries2 := buildChanges(old2, nil, newBits2, day(t, "2026-01-01"), 43)
	if len(entries2) != 1 || entryString(t, entries2[0]) != "opened AAA-BBB EI 2026-01-01 C t=43" {
		t.Fatalf("post-onboarding gain not news: %v", entries2)
	}
}

// TestPinnedFloorIsPerAirline: one airline's churn must not starve another's
// floor. BA fills a cabin's bucket with newer openings; EI's older rolled-off
// opening in the SAME cabin must still be pinned, or an airline-lens
// "Recently opened" goes permanently blank.
func TestPinnedFloorIsPerAirline(t *testing.T) {
	cutoff := day(t, "2026-01-01")
	window := []any{fe("AAA-BBB", "BA", "2026-03-01", "opened", "M", 1000)}
	var old []map[string]any
	// More BA Economy openings than one bucket holds, all newer than EI's.
	for i := 0; i < maxPinnedPerCabin+3; i++ {
		old = append(old, fe(fmt.Sprintf("BA%d-CCC", i), "BA", "2026-04-02", "opened", "M", int64(500+i)))
	}
	old = append(old, fe("DUB-BOS", "EI", "2026-04-01", "opened", "M", 100))
	got := pinnedStrings(t, buildPinned(window, feedFile(t, old, nil), cutoff))
	found := false
	for _, s := range got {
		if s == "opened DUB-BOS 2026-04-01 M t=100" {
			found = true
		}
	}
	if !found {
		t.Fatalf("EI opening starved out of the floor: %v", got)
	}
}
