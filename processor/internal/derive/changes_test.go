package derive

import (
	"encoding/json"
	"fmt"
	"testing"
)

// oldBundle builds a minimal previous availability.json for diffing.
func oldBundleJSON(t *testing.T, epoch string, routes map[string]map[string]string) []byte {
	t.Helper()
	type entry struct {
		A map[string]string `json:"a"`
	}
	wrapped := map[string]entry{}
	for r, a := range routes {
		wrapped[r] = entry{A: a}
	}
	b, err := json.Marshal(map[string]any{"epoch": epoch, "routes": wrapped})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func entryString(t *testing.T, e any) string {
	t.Helper()
	m, ok := e.(map[string]any)
	if !ok {
		t.Fatalf("entry %v is %T, not a new-batch entry", e, e)
	}
	return fmt.Sprintf("%s %s %s %s %s t=%v", m["k"], m["r"], m["al"], m["d"], m["c"], m["t"])
}

func TestChangesOpenedClosedChanged(t *testing.T) {
	// Old state (epoch 2026-01-01):
	//   AAA-BBB/BA: 2026-01-02 = 5 (MC)
	//   CCC-DDD/BA: 2026-01-01 = 1 (M)
	old := oldBundleJSON(t, "2026-01-01", map[string]map[string]string{
		"AAA-BBB": {"BA": "050"},
		"CCC-DDD": {"BA": "1"},
	})
	// New state: AAA-BBB 01-02 changed to WC (6), 01-03 opened F (8);
	// CCC-DDD 01-01 gone (closed, old cabins M).
	newBits := map[string]map[string]map[int]int{
		"AAA-BBB": {"BA": {day(t, "2026-01-02"): 6, day(t, "2026-01-03"): 8}},
	}
	entries := buildChanges(old, nil, newBits, day(t, "2026-01-01"), 42)

	want := []string{
		"changed AAA-BBB BA 2026-01-02 WC t=42",
		"opened AAA-BBB BA 2026-01-03 F t=42",
		"closed CCC-DDD BA 2026-01-01 M t=42",
	}
	if len(entries) != len(want) {
		t.Fatalf("got %d entries, want %d: %v", len(entries), len(want), entries)
	}
	for i, w := range want {
		if got := entryString(t, entries[i]); got != w {
			t.Errorf("entry[%d] = %q, want %q", i, got, w)
		}
	}
}

func TestChangesRollOffNotClosed(t *testing.T) {
	old := oldBundleJSON(t, "2026-01-01", map[string]map[string]string{
		"AAA-BBB": {"BA": "105"}, // 01-01 M, 01-03 MC
	})
	// New: 01-01 gone (past), 01-03 gone (future -> genuinely closed).
	newBits := map[string]map[string]map[int]int{}
	entries := buildChanges(old, nil, newBits, day(t, "2026-01-02"), 43)
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1 (past roll-off excluded): %v", len(entries), entries)
	}
	if got, want := entryString(t, entries[0]), "closed AAA-BBB BA 2026-01-03 MC t=43"; got != want {
		t.Errorf("entry = %q, want %q", got, want)
	}
}

func TestChangesHorizonGrowthNotOpened(t *testing.T) {
	// Old bundle encodes through 2026-01-02 (string length 2). The new run's
	// horizon advanced to 01-04 with availability at the new edge — exactly
	// what every scraper booking-window advance looks like. None of it is
	// "recently opened".
	old := oldBundleJSON(t, "2026-01-01", map[string]map[string]string{
		"AAA-BBB": {"BA": "11"},
		"CCC-DDD": {"BA": "01"},
	})
	newBits := map[string]map[string]map[int]int{
		"AAA-BBB": {"BA": {
			day(t, "2026-01-01"): 1, day(t, "2026-01-02"): 1, // unchanged
			day(t, "2026-01-03"): 1, day(t, "2026-01-04"): 15, // horizon growth
		}},
		"CCC-DDD": {"BA": {
			day(t, "2026-01-02"): 1, // unchanged
			day(t, "2026-01-03"): 8, // horizon growth
		}},
	}
	entries := buildChanges(old, nil, newBits, day(t, "2026-01-01"), 46)
	if len(entries) != 0 {
		t.Fatalf("horizon extension must emit no entries, got %v", entries)
	}
}

func TestChangesMidHorizonOpenStillEmitted(t *testing.T) {
	// Horizon reaches 01-03; a gap at 01-02 filling in IS a genuine open,
	// while 01-04 (beyond the old horizon) is not.
	old := oldBundleJSON(t, "2026-01-01", map[string]map[string]string{
		"AAA-BBB": {"BA": "101"},
	})
	newBits := map[string]map[string]map[int]int{
		"AAA-BBB": {"BA": {
			day(t, "2026-01-01"): 1,
			day(t, "2026-01-02"): 4, // opened mid-horizon
			day(t, "2026-01-03"): 1,
			day(t, "2026-01-04"): 1, // horizon growth, suppressed
		}},
	}
	entries := buildChanges(old, nil, newBits, day(t, "2026-01-01"), 47)
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1: %v", len(entries), entries)
	}
	if got, want := entryString(t, entries[0]), "opened AAA-BBB BA 2026-01-02 C t=47"; got != want {
		t.Errorf("entry = %q, want %q", got, want)
	}
}

func TestChangesTrimAndPriorOrder(t *testing.T) {
	prior := make([]map[string]any, 999)
	for i := range prior {
		prior[i] = map[string]any{"marker": i}
	}
	priorFile, err := json.Marshal(map[string]any{"entries": prior})
	if err != nil {
		t.Fatal(err)
	}
	old := oldBundleJSON(t, "2026-01-01", map[string]map[string]string{
		"AAA-BBB": {"BA": "11"}, // two dates that will close
	})
	entries := buildChanges(old, priorFile, map[string]map[string]map[int]int{}, day(t, "2026-01-01"), 44)
	if len(entries) != maxChangeEntries {
		t.Fatalf("got %d entries, want trim to %d", len(entries), maxChangeEntries)
	}
	// Newest first: the 2 new entries, then priors 0..997 (998 dropped).
	if got := entryString(t, entries[0]); got != "closed AAA-BBB BA 2026-01-01 M t=44" {
		t.Errorf("entry[0] = %q", got)
	}
	first, last := entries[2].(json.RawMessage), entries[999].(json.RawMessage)
	if string(first) != `{"marker":0}` {
		t.Errorf("first prior = %s, want marker 0", first)
	}
	if string(last) != `{"marker":997}` {
		t.Errorf("last prior = %s, want marker 997 (marker 998 trimmed)", last)
	}
}

func TestChangesNoPreviousBundle(t *testing.T) {
	priorFile := []byte(`{"entries":[{"marker":7}]}`)
	newBits := map[string]map[string]map[int]int{
		"AAA-BBB": {"BA": {day(t, "2026-01-01"): 1}},
	}
	// No old bundle -> no diff possible -> preserve prior entries only.
	entries := buildChanges(nil, priorFile, newBits, day(t, "2026-01-01"), 45)
	if len(entries) != 1 || string(entries[0].(json.RawMessage)) != `{"marker":7}` {
		t.Fatalf("entries = %v, want just the preserved prior entry", entries)
	}
	// Unparseable old bundle behaves like no old bundle.
	entries = buildChanges([]byte("{nope"), priorFile, newBits, day(t, "2026-01-01"), 45)
	if len(entries) != 1 {
		t.Fatalf("unparseable old bundle: entries = %v, want 1 preserved entry", entries)
	}
	// Nothing at all -> empty (not nil semantics matter for JSON: [] not null).
	entries = buildChanges(nil, nil, newBits, day(t, "2026-01-01"), 45)
	if entries == nil || len(entries) != 0 {
		t.Fatalf("entries = %#v, want empty non-nil slice", entries)
	}
}
