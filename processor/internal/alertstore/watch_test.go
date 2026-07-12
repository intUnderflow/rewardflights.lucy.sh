package alertstore

import "testing"

// TestLeadDaysRollingFloor is the "I can travel any time, but I need N days'
// notice" case. The floor is stored as a relative offset and resolved against
// the current day, so a trip 3 days out is excluded today AND still excluded
// tomorrow — it must never freeze to a fixed date.
func TestLeadDaysRollingFloor(t *testing.T) {
	w, err := Normalize(Watch{Route: "LON-TYO", Kind: "ow", Cabins: []string{"C"}, LeadDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	const endDay = 1000

	// today=100: outbound floor must be 107, not 100.
	if got := w.OutDays(100, endDay); got.From != 107 {
		t.Errorf("with 7 days' notice at today=100, floor = %d, want 107", got.From)
	}
	// today=200 (a week later): the floor rolled to 207 on its own — the whole
	// point. A fixed date would still say 107.
	if got := w.OutDays(200, endDay); got.From != 207 {
		t.Errorf("floor did not roll forward: at today=200 got %d, want 207", got.From)
	}
	// It only raises the floor; the ceiling stays the horizon (unbounded above).
	if got := w.OutDays(100, endDay); got.To != endDay-1 {
		t.Errorf("lead time must not bound the ceiling: got %d, want %d", got.To, endDay-1)
	}
	// Unbounded above => not entitled to horizon-frontier alerts (EC-3): a lead
	// watch must not get pinged daily as the booking window rolls out.
	if w.BoundedOut() {
		t.Error("a lead-time watch is unbounded above and must report BoundedOut()==false")
	}
	// Never expires (no absolute upper date) and never impossible.
	if w.Expired(999999) || w.Impossible() {
		t.Error("a lead-time watch should neither expire nor be impossible")
	}
}

func TestLeadDaysValidation(t *testing.T) {
	base := Watch{Route: "LON-TYO", Kind: "ow", Cabins: []string{"C"}}
	// Valid boundaries.
	for _, n := range []int{0, 1, MaxLeadDays} {
		w := base
		w.LeadDays = n
		if _, err := Normalize(w); err != nil {
			t.Errorf("leadDays=%d should be valid: %v", n, err)
		}
	}
	// Too large / negative.
	for _, n := range []int{-1, MaxLeadDays + 1} {
		w := base
		w.LeadDays = n
		if _, err := Normalize(w); err == nil {
			t.Errorf("leadDays=%d should be rejected", n)
		}
	}
	// Mutually exclusive with an explicit outbound range.
	w := base
	w.LeadDays = 7
	w.Out = &Range{From: "2026-10-01", To: "2026-10-20"}
	if _, err := Normalize(w); err == nil {
		t.Error("lead time combined with explicit dates must be rejected")
	}
	// Different lead times are different watches (distinct ids).
	a, _ := Normalize(Watch{Route: "LON-TYO", Kind: "ow", Cabins: []string{"C"}, LeadDays: 7})
	b, _ := Normalize(Watch{Route: "LON-TYO", Kind: "ow", Cabins: []string{"C"}, LeadDays: 14})
	if a.ID == b.ID {
		t.Error("7-day and 14-day lead watches must have distinct ids")
	}
}
