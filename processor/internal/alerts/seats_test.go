package alerts

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/alertstore"
)

// --- seats-layer bundle builder -------------------------------------------
//
// seatBundleAt extends bundleAt with the optional "s" layer: per route, per
// October day, per cabin letter, a SEAT COUNT (what a scraper would have
// seen). Counts are encoded to the 2-bit monotone threshold code exactly as
// the processor does: <2 -> 0, 2 -> 1, 3 -> 2, >=4 -> 3; M=bits 0-1, W=2-3,
// C=4-5, F=6-7; two uppercase hex chars per day.
func seatBundleAt(t *testing.T, sourceDate string, routes map[string]map[int]string,
	seats map[string]map[int]map[string]int) []byte {
	t.Helper()
	when, err := time.Parse("2006-01-02T15:04", sourceDate)
	if err != nil {
		t.Fatalf("bad source time %q: %v", sourceDate, err)
	}
	cabinShift := map[string]uint{"M": 0, "W": 2, "C": 4, "F": 6}
	code := func(count int) byte {
		switch {
		case count >= 4:
			return 3
		case count == 3:
			return 2
		case count == 2:
			return 1
		}
		return 0
	}
	strs := map[string]any{}
	for route, avail := range routes {
		buf := make([]byte, testDays)
		for i := range buf {
			buf[i] = '0'
		}
		for day, cabins := range avail {
			var bits byte
			for _, c := range cabins {
				switch c {
				case 'M':
					bits |= 1
				case 'W':
					bits |= 2
				case 'C':
					bits |= 4
				case 'F':
					bits |= 8
				}
			}
			buf[oct(day)] = "0123456789ABCDEF"[bits]
		}
		entry := map[string]any{"a": map[string]string{"BA": string(buf)}}
		if byDay, ok := seats[route]; ok {
			sbuf := make([]byte, 2*testDays)
			for i := range sbuf {
				sbuf[i] = '0'
			}
			for day, counts := range byDay {
				var b byte
				for cab, count := range counts {
					b |= code(count) << cabinShift[cab]
				}
				sbuf[2*oct(day)] = "0123456789ABCDEF"[b>>4]
				sbuf[2*oct(day)+1] = "0123456789ABCDEF"[b&15]
			}
			entry["s"] = map[string]string{"BA": string(sbuf)}
		}
		strs[route] = entry
	}
	raw, err := json.Marshal(map[string]any{
		"epoch": testEpoch, "t": when.Unix(),
		"airlines": map[string]any{"BA": map[string]any{}},
		"routes":   strs,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (h *harness) seatCycle(sourceTime string, routes map[string]map[int]string, seats map[string]map[int]map[string]int) {
	h.t.Helper()
	h.w.Cycle(seatBundleAt(h.t, sourceTime, routes, seats))
}

func (h *harness) seatBaseline(sourceTime string, routes map[string]map[int]string, seats map[string]map[int]map[string]int) {
	h.t.Helper()
	h.w.Baseline(seatBundleAt(h.t, sourceTime, routes, seats))
}

func partyRT(minSeats int) alertstore.Watch {
	w := unbounded(alertstore.KindRT, "C")
	w.MinSeats = minSeats
	return w
}

// --- threshold gains -------------------------------------------------------

// TestSeatThresholdCrossingFires: the count on an ALREADY-OPEN day rises past
// the watch's threshold. There is no presence-bit gain at all — the exact
// event the bit-level engine is blind to — yet for a party of two this is
// the moment the trip becomes real.
func TestSeatThresholdCrossingFires(t *testing.T) {
	h := newHarnessWithSubs(t, map[string][]alertstore.Watch{
		testEndpoint:  {partyRT(2)},
		otherEndpoint: {unbounded(alertstore.KindRT, "C")}, // 1-pax control
	})
	bits := map[string]map[int]string{"LON-TYO": {3: "C"}, "TYO-LON": {14: "C"}}

	// Baseline: outbound holds 1 seat (code 0), return already holds 2.
	h.seatBaseline("2026-10-01T09:00", bits, map[string]map[int]map[string]int{
		"LON-TYO": {3: {"C": 1}}, "TYO-LON": {14: {"C": 2}},
	})
	// The outbound goes 1 -> 2 seats. Bits are unchanged.
	h.seatCycle("2026-10-01T10:00", bits, map[string]map[int]map[string]int{
		"LON-TYO": {3: {"C": 2}}, "TYO-LON": {14: {"C": 2}},
	})

	if len(h.cap.pubs) != 1 {
		t.Fatalf("got %d publications, want exactly 1 (the party watch): %v", len(h.cap.pubs), h.cap.bodies())
	}
	if h.cap.subs[0] != testEndpoint {
		t.Errorf("fired for %s; a bit-level watch has no news here (no bit changed)", h.cap.subs[0])
	}
	pub := h.cap.pubs[0]
	if want := "Business (2+ seats) round trips open: LON ⇄ TYO"; pub.Title != want {
		t.Errorf("title = %q, want %q", pub.Title, want)
	}
	if want := "1 new: 3–14 Oct"; pub.Body != want {
		t.Errorf("body = %q, want %q", pub.Body, want)
	}
}

// TestUnknownCountsNeverFireParty: a presence-bit gain on a route with NO
// seats layer fires the single-passenger watch and must NOT fire the party
// watch — a push is a promise, and we have no evidence two seats exist.
func TestUnknownCountsNeverFireParty(t *testing.T) {
	h := newHarnessWithSubs(t, map[string][]alertstore.Watch{
		testEndpoint:  {partyRT(2)},
		otherEndpoint: {unbounded(alertstore.KindRT, "C")},
	})
	h.baseline("2026-10-01T09:00", map[string]map[int]string{"LON-TYO": {}, "TYO-LON": {}})
	h.cycle("2026-10-01T10:00", map[string]map[int]string{"LON-TYO": {3: "C"}, "TYO-LON": {14: "C"}})

	if len(h.cap.pubs) != 1 {
		t.Fatalf("got %d publications, want exactly 1 (the 1-pax watch): %v", len(h.cap.pubs), h.cap.bodies())
	}
	if h.cap.subs[0] != otherEndpoint {
		t.Errorf("fired for %s, want the single-passenger subscriber only", h.cap.subs[0])
	}
	if pub := h.cap.pubs[0]; pub.Title != "Business round trips open: LON ⇄ TYO" {
		t.Errorf("the 1-pax copy must carry no seat qualifier: %q", pub.Title)
	}
}

// TestPartnerLegMustHoldSeats: the dirty leg crossing the threshold is not
// enough — the PARTY must fit on both legs. A return holding only 2 seats
// (or none known) cannot complete a 3-seat round trip; when the return later
// reaches 3, loop (b) pairs it with the outbound that has held 3 for a while.
func TestPartnerLegMustHoldSeats(t *testing.T) {
	h := newHarness(t, partyRT(3))
	bits := map[string]map[int]string{"LON-TYO": {3: "C"}, "TYO-LON": {14: "C"}}

	h.seatBaseline("2026-10-01T09:00", bits, map[string]map[int]map[string]int{
		"LON-TYO": {3: {"C": 1}}, "TYO-LON": {14: {"C": 2}},
	})
	// Outbound crosses to 3. Return still holds only 2: no trip for three.
	h.seatCycle("2026-10-01T10:00", bits, map[string]map[int]map[string]int{
		"LON-TYO": {3: {"C": 3}}, "TYO-LON": {14: {"C": 2}},
	})
	if len(h.cap.pubs) != 0 {
		t.Fatalf("the return can only seat 2 of a party of 3; must not fire: %v", h.cap.bodies())
	}
	// The return catches up: NOW the pair exists for three.
	h.seatCycle("2026-10-01T11:30", bits, map[string]map[int]map[string]int{
		"LON-TYO": {3: {"C": 3}}, "TYO-LON": {14: {"C": 4}},
	})
	if len(h.cap.pubs) != 1 {
		t.Fatalf("return-leg crossing must pair with the standing outbound: %v", h.cap.bodies())
	}
	if want := "Business (3+ seats) round trips open: LON ⇄ TYO"; h.cap.pubs[0].Title != want {
		t.Errorf("title = %q, want %q", h.cap.pubs[0].Title, want)
	}
}

// TestSeatsLayerOnboardingBaselines pins the rollout rule: the seats layer's
// FIRST appearance for a route is a silent baseline (mirroring EC-11 for new
// routes). The scraper backfilling flight detail across the whole network in
// one publish must not flood gainDays past the EC-13 breaker — that would
// advance the ledger while publishing nothing, permanently silencing genuine
// same-cycle 1-pax openings. Party watches fire from the NEXT transition.
func TestSeatsLayerOnboardingBaselines(t *testing.T) {
	h := newHarness(t, partyRT(2))
	bits := map[string]map[int]string{"LON-TYO": {3: "C", 5: "C"}, "TYO-LON": {14: "C"}}

	h.baseline("2026-10-01T09:00", bits) // no "s" anywhere
	// Onboarding: the layer appears already deep on day 3 — silent baseline.
	h.seatCycle("2026-10-01T10:00", bits, map[string]map[int]map[string]int{
		"LON-TYO": {3: {"C": 2}}, "TYO-LON": {14: {"C": 2}},
	})
	if len(h.cap.pubs) != 0 {
		t.Fatalf("seats-layer onboarding must baseline silently, got %v", h.cap.bodies())
	}
	// The next genuine transition (day 5 deepens to >=2) DOES fire.
	h.seatCycle("2026-10-01T11:00", bits, map[string]map[int]map[string]int{
		"LON-TYO": {3: {"C": 2}, 5: {"C": 2}}, "TYO-LON": {14: {"C": 2}},
	})
	if len(h.cap.pubs) != 1 {
		t.Fatalf("post-onboarding threshold crossing must fire: %v", h.cap.bodies())
	}
}

// TestSeatChurnInvisibleToOnePax: counts moving around on already-open days
// is NOT news to a single passenger; the MinSeats <= 1 path must stay
// byte-identical to the pre-seats engine even when the bundle carries "s".
func TestSeatChurnInvisibleToOnePax(t *testing.T) {
	h := newHarness(t, unbounded(alertstore.KindRT, "C"))
	bits := map[string]map[int]string{"LON-TYO": {3: "C"}, "TYO-LON": {14: "C"}}

	h.seatBaseline("2026-10-01T09:00", bits, map[string]map[int]map[string]int{
		"LON-TYO": {3: {"C": 1}}, "TYO-LON": {14: {"C": 1}},
	})
	h.seatCycle("2026-10-01T10:00", bits, map[string]map[int]map[string]int{
		"LON-TYO": {3: {"C": 4}}, "TYO-LON": {14: {"C": 4}},
	})
	if len(h.cap.pubs) != 0 {
		t.Fatalf("a 1-pax watch must not hear about count churn: %v", h.cap.bodies())
	}
}

// TestSeatFlapSuppressed: a count sagging 2 -> 1 -> 2 never touches the
// presence bit, so only the threshold-qualified ledger plane can see the
// blink. Within the cooldown it is suppressed; a genuine re-deepening after
// the cooldown alerts again.
func TestSeatFlapSuppressed(t *testing.T) {
	w := alertstore.Watch{Route: "LON-TYO", Kind: alertstore.KindOW, Cabins: []string{"C"}, MinSeats: 2}
	h := newHarness(t, w)
	bits := map[string]map[int]string{"LON-TYO": {3: "C"}}
	deep := map[string]map[int]map[string]int{"LON-TYO": {3: {"C": 2}}}
	shallow := map[string]map[int]map[string]int{"LON-TYO": {3: {"C": 1}}}

	h.seatBaseline("2026-10-01T09:00", bits, shallow)
	h.seatCycle("2026-10-01T09:10", bits, deep) // alert #1
	h.seatCycle("2026-10-01T09:20", bits, shallow)
	h.seatCycle("2026-10-01T09:50", bits, deep) // 30 min later: flap
	if len(h.cap.pubs) != 1 {
		t.Fatalf("a seat-count blink must be suppressed: %v", h.cap.bodies())
	}
	h.seatCycle("2026-10-01T10:00", bits, shallow)
	h.seatCycle("2026-10-01T14:00", bits, deep) // 4h later: genuine
	if len(h.cap.pubs) != 2 {
		t.Fatalf("a genuine re-deepening must alert: %v", h.cap.bodies())
	}
}

// TestThresholdLedgerPrunes: the 4-segment threshold keys still end in the
// date, so prune's date parsing (and therefore state boundedness) survives
// the new plane.
func TestThresholdLedgerPrunes(t *testing.T) {
	today, err := parseDay("2026-10-05")
	if err != nil {
		t.Fatal(err)
	}
	if !ledgerDatePast("LON-TYO|C|S3|2026-10-04", today) {
		t.Error("a past-dated threshold key must prune")
	}
	if ledgerDatePast("LON-TYO|C|S3|2026-10-06", today) {
		t.Error("a future-dated threshold key must be kept")
	}
	if got := ledgerKey("LON-TYO", today, "C", 3); got != "LON-TYO|C|S3|2026-10-05" {
		t.Errorf("ledgerKey = %q", got)
	}
	if got := ledgerKey("LON-TYO", today, "C", 0); got != "LON-TYO|C|2026-10-05" {
		t.Errorf("bit-plane ledgerKey must keep the legacy spelling, got %q", got)
	}
}

// TestMixedPartyAndOnePaxNewsUnqualified: when the same pair is news to both
// a party watch and a 1-pax watch of ONE subscriber, the copy must not claim
// a seat threshold the 1-pax audience never asked for (dedupe keeps the
// lowest threshold).
func TestMixedPartyAndOnePaxNewsUnqualified(t *testing.T) {
	h := newHarness(t, partyRT(2), unbounded(alertstore.KindRT, "C"))
	shut := map[string]map[int]string{"LON-TYO": {}, "TYO-LON": {}}
	open := map[string]map[int]string{"LON-TYO": {3: "C"}, "TYO-LON": {14: "C"}}

	h.seatBaseline("2026-10-01T09:00", shut, nil)
	// Both legs open at 2+ seats in one cycle: bit gain AND threshold gain.
	h.seatCycle("2026-10-01T10:00", open, map[string]map[int]map[string]int{
		"LON-TYO": {3: {"C": 2}}, "TYO-LON": {14: {"C": 2}},
	})
	if len(h.cap.pubs) != 1 {
		t.Fatalf("one subscriber, one batch: %v", h.cap.bodies())
	}
	if pub := h.cap.pubs[0]; pub.Title != "Business round trips open: LON ⇄ TYO" {
		t.Errorf("mixed news must keep the unqualified copy, got %q", pub.Title)
	}
}
