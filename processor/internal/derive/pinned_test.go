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
	var entries []map[string]any
	for i := 0; i < maxPinnedPerCabin+5; i++ {
		entries = append(entries, fe(fmt.Sprintf("A%02d-BBB", i), "BA", "2026-04-01", "opened", "F", int64(1000+i)))
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
