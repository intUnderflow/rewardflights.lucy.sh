package alerts

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/alertstore"
)

// bundleMultiAt builds a bundle with per-airline strings:
// route -> airline -> October day -> cabin letters.
func bundleMultiAt(t *testing.T, sourceDate string, routes map[string]map[string]map[int]string) []byte {
	t.Helper()
	when, err := time.Parse("2006-01-02T15:04", sourceDate)
	if err != nil {
		t.Fatal(err)
	}
	als := map[string]any{}
	strs := map[string]any{}
	names := map[string]string{"BA": "British Airways", "EI": "Aer Lingus"}
	for route, byAl := range routes {
		a := map[string]string{}
		for al, avail := range byAl {
			als[al] = map[string]any{"name": names[al]}
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
			a[al] = string(buf)
		}
		strs[route] = map[string]any{"a": a}
	}
	raw, err := json.Marshal(map[string]any{
		"epoch": testEpoch, "t": when.Unix(), "airlines": als, "routes": strs,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestNewAirlineOnboardingIsSilent: a second airline's entire network arriving
// in one generation is baseline — no publications, no EC-13 trip — and from
// the NEXT cycle its genuine gains alert normally.
func TestNewAirlineOnboardingIsSilent(t *testing.T) {
	h := newHarness(t, unbounded(alertstore.KindRT, "C"))

	// BA-only world: outbound Business on 3 Oct, return shut.
	h.w.Baseline(bundleMultiAt(t, "2026-10-01T09:00", map[string]map[string]map[int]string{
		"LON-TYO": {"BA": {3: "C"}},
		"TYO-LON": {"BA": {}},
	}))
	// EI onboards with Business on BOTH legs across many days — pairs that
	// would fire hard if treated as gains.
	h.w.Cycle(bundleMultiAt(t, "2026-10-01T10:00", map[string]map[string]map[int]string{
		"LON-TYO": {"BA": {3: "C"}, "EI": {4: "C", 5: "C", 6: "C"}},
		"TYO-LON": {"BA": {}, "EI": {14: "C", 15: "C", 16: "C"}},
	}))
	if len(h.cap.pubs) != 0 {
		t.Fatalf("onboarding published: %v", h.cap.bodies())
	}
	for _, line := range h.cap.logs {
		if strings.Contains(line, "alert-bulk-change") {
			t.Fatalf("onboarding tripped EC-13: %s", line)
		}
	}

	// Next cycle: EI (now known) gains a return day — pairs with BA's old
	// outbound. That IS news.
	h.w.Cycle(bundleMultiAt(t, "2026-10-01T11:30", map[string]map[string]map[int]string{
		"LON-TYO": {"BA": {3: "C"}, "EI": {4: "C", 5: "C", 6: "C"}},
		"TYO-LON": {"BA": {}, "EI": {14: "C", 15: "C", 16: "C", 20: "C"}},
	}))
	if len(h.cap.pubs) != 1 {
		t.Fatalf("post-onboarding gain must alert: %v", h.cap.bodies())
	}
}

// TestScopedWatchSeesShadowedGain is the scoped crux: EI gains Business on a
// day BA already held, so the MERGED planes do not move at all. The EI-scoped
// watch must fire from EI's own transition plane; the BA-scoped and unscoped
// watches must stay silent.
func TestScopedWatchSeesShadowedGain(t *testing.T) {
	ei := unbounded(alertstore.KindRT, "C")
	ei.Airline = "EI"
	ba := unbounded(alertstore.KindRT, "C")
	ba.Airline = "BA"
	h := newHarness(t, unbounded(alertstore.KindRT, "C"), ei, ba)

	// Both airlines known on both legs; EI already holds the return day BA has.
	h.w.Baseline(bundleMultiAt(t, "2026-10-01T09:00", map[string]map[string]map[int]string{
		"LON-TYO": {"BA": {3: "C"}, "EI": {}},
		"TYO-LON": {"BA": {14: "C"}, "EI": {14: "C"}},
	}))
	// EI gains outbound 3 Oct — a day BA already held. Merged is unchanged.
	h.w.Cycle(bundleMultiAt(t, "2026-10-01T10:00", map[string]map[string]map[int]string{
		"LON-TYO": {"BA": {3: "C"}, "EI": {3: "C"}},
		"TYO-LON": {"BA": {14: "C"}, "EI": {14: "C"}},
	}))

	if len(h.cap.pubs) != 1 {
		t.Fatalf("got %d publications, want exactly 1 (EI-scoped only): %v",
			len(h.cap.pubs), h.cap.bodies())
	}
	pub := h.cap.pubs[0]
	if want := "Business round trips open: LON ⇄ TYO on Aer Lingus"; pub.Title != want {
		t.Errorf("title = %q, want %q", pub.Title, want)
	}
	if want := "1 new: 3–14 Oct"; pub.Body != want {
		t.Errorf("body = %q, want %q", pub.Body, want)
	}
}

// TestScopedPartnerLegPurity: a scoped watch's BOTH legs must hold on the
// scoped airline. An EI outbound gain with only a BA return is not an EI round
// trip; when EI later gains the return (shadowed by BA), the pair fires.
func TestScopedPartnerLegPurity(t *testing.T) {
	ei := unbounded(alertstore.KindRT, "C")
	ei.Airline = "EI"
	h := newHarness(t, ei)

	h.w.Baseline(bundleMultiAt(t, "2026-10-01T09:00", map[string]map[string]map[int]string{
		"LON-TYO": {"BA": {}, "EI": {}},
		"TYO-LON": {"BA": {14: "C"}, "EI": {}},
	}))
	// EI gains the outbound; the only return is BA's. No EI trip exists.
	h.w.Cycle(bundleMultiAt(t, "2026-10-01T10:00", map[string]map[string]map[int]string{
		"LON-TYO": {"BA": {}, "EI": {5: "C"}},
		"TYO-LON": {"BA": {14: "C"}, "EI": {}},
	}))
	if len(h.cap.pubs) != 0 {
		t.Fatalf("BA return must not complete an EI-scoped pair: %v", h.cap.bodies())
	}
	// EI gains the return — invisible to merged (BA already held 14 Oct).
	h.w.Cycle(bundleMultiAt(t, "2026-10-01T11:00", map[string]map[string]map[int]string{
		"LON-TYO": {"BA": {}, "EI": {5: "C"}},
		"TYO-LON": {"BA": {14: "C"}, "EI": {14: "C"}},
	}))
	if len(h.cap.pubs) != 1 {
		t.Fatalf("EI return gain completes the EI pair: %v", h.cap.bodies())
	}
	if want := "1 new: 5–14 Oct"; h.cap.pubs[0].Body != want {
		t.Errorf("body = %q, want %q", h.cap.pubs[0].Body, want)
	}
}

// TestScopedFlapSuppressed: the flap cooldown runs on the AIRLINE's own ledger
// plane. BA holds the day throughout, so every EI transition is invisible to
// merged — the scoped ledger alone must absorb the blink and honour the 3h
// cooldown.
func TestScopedFlapSuppressed(t *testing.T) {
	ei := unbounded(alertstore.KindOW, "C")
	ei.Airline = "EI"
	h := newHarness(t, ei)

	world := func(eiDays map[int]string) map[string]map[string]map[int]string {
		return map[string]map[string]map[int]string{
			"LON-TYO": {"BA": {5: "C"}, "EI": eiDays},
		}
	}
	open := map[int]string{5: "C"}

	h.w.Baseline(bundleMultiAt(t, "2026-10-01T09:00", world(map[int]string{})))
	h.w.Cycle(bundleMultiAt(t, "2026-10-01T09:10", world(open)))
	if len(h.cap.pubs) != 1 {
		t.Fatalf("first EI open must alert: %v", h.cap.bodies())
	}
	if want := "Business seats open: LON → TYO on Aer Lingus"; h.cap.pubs[0].Title != want {
		t.Errorf("title = %q, want %q", h.cap.pubs[0].Title, want)
	}

	h.w.Cycle(bundleMultiAt(t, "2026-10-01T09:20", world(map[int]string{})))
	h.w.Cycle(bundleMultiAt(t, "2026-10-01T09:40", world(open)))
	if len(h.cap.pubs) != 1 {
		t.Fatalf("an EI blink inside the cooldown must not re-alert: %v", h.cap.bodies())
	}

	h.w.Cycle(bundleMultiAt(t, "2026-10-01T10:00", world(map[int]string{})))
	h.w.Cycle(bundleMultiAt(t, "2026-10-01T14:30", world(open)))
	if len(h.cap.pubs) != 2 {
		t.Fatalf("a genuine EI re-open past the cooldown must alert: %v", h.cap.bodies())
	}
}

// TestScopedWatchSilentDuringOnboarding: an EI-scoped watch must treat EI's
// arrival on a route as baseline exactly like everyone else — and fire on EI's
// first genuine gain afterwards.
func TestScopedWatchSilentDuringOnboarding(t *testing.T) {
	ei := unbounded(alertstore.KindRT, "C")
	ei.Airline = "EI"
	h := newHarness(t, ei)

	h.w.Baseline(bundleMultiAt(t, "2026-10-01T09:00", map[string]map[string]map[int]string{
		"LON-TYO": {"BA": {3: "C"}},
		"TYO-LON": {"BA": {}},
	}))
	// EI onboards holding a complete pair. Baseline, not news.
	h.w.Cycle(bundleMultiAt(t, "2026-10-01T10:00", map[string]map[string]map[int]string{
		"LON-TYO": {"BA": {3: "C"}, "EI": {4: "C"}},
		"TYO-LON": {"BA": {}, "EI": {15: "C"}},
	}))
	if len(h.cap.pubs) != 0 {
		t.Fatalf("scoped watch fired on onboarding: %v", h.cap.bodies())
	}
	// Known from now on: EI gains a second return day — that IS EI news.
	h.w.Cycle(bundleMultiAt(t, "2026-10-01T11:30", map[string]map[string]map[int]string{
		"LON-TYO": {"BA": {3: "C"}, "EI": {4: "C"}},
		"TYO-LON": {"BA": {}, "EI": {15: "C", 20: "C"}},
	}))
	if len(h.cap.pubs) != 1 {
		t.Fatalf("post-onboarding EI gain must alert the scoped watch: %v", h.cap.bodies())
	}
}
