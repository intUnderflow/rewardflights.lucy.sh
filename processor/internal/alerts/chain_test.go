package alerts

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/alertstore"
)

// The via test world: Billund is a short hop from the London hub, Tokyo is
// long-haul, so focusLegs puts the cabin filter on the LON↔TYO legs and the
// BLL↔LON hops only need any award space — the same split the site derives.
var viaPlaces = map[string][2]float64{
	"BLL": {55.74, 9.15},
	"LON": {51.5, -0.12},
	"TYO": {35.68, 139.77},
}

// bundleViaAt is bundleAt plus place coordinates (focusLegs needs them; the
// coord-less builder would couple every leg, hiding the hop/long-haul split).
func bundleViaAt(t *testing.T, sourceDate string, routes map[string]map[int]string) []byte {
	t.Helper()
	when, err := time.Parse("2006-01-02T15:04", sourceDate)
	if err != nil {
		t.Fatalf("bad source time %q: %v", sourceDate, err)
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
		strs[route] = map[string]any{"a": map[string]string{"BA": string(buf)}}
	}
	places := map[string]any{}
	for code, g := range viaPlaces {
		places[code] = map[string]any{"g": []float64{g[0], g[1]}}
	}
	raw, err := json.Marshal(map[string]any{
		"epoch": testEpoch, "t": when.Unix(),
		"airlines": map[string]any{"BA": map[string]any{}},
		"places":   places,
		"routes":   strs,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// watchVia is the running chain example: BLL-TYO via LON, Business on the
// long-haul legs, 7–14 nights in Tokyo, 1-night stops at the hub.
func watchVia(kind string) alertstore.Watch {
	w := alertstore.Watch{
		Route: "BLL-TYO", Kind: kind, Cabins: []string{"C"},
		Via: "LON", Conn: 1,
	}
	if kind == alertstore.KindRT {
		w.Nights = &alertstore.Nights{Min: 7, Max: 14}
	}
	return w
}

// The complete chain used throughout: BLL→LON 2 Oct, LON→TYO 3 Oct (C),
// TYO→LON 10 Oct (C, 7 nights), LON→BLL 11 Oct.
func chainComplete() map[string]map[int]string {
	return map[string]map[int]string{
		"BLL-LON": {2: "M"},
		"LON-TYO": {3: "C"},
		"TYO-LON": {10: "C"},
		"LON-BLL": {11: "M"},
	}
}

// TestChainLongHaulGainFires: the long-haul outbound opens and completes a
// chain whose other three legs were already open — the chain-generalized
// leg-gains theorem's basic case.
func TestChainLongHaulGainFires(t *testing.T) {
	h := newHarness(t, watchVia(alertstore.KindRT))
	prev := chainComplete()
	prev["LON-TYO"] = map[int]string{}
	h.w.Baseline(bundleViaAt(t, "2026-10-01T09:00", prev))
	h.w.Cycle(bundleViaAt(t, "2026-10-01T10:00", chainComplete()))

	if len(h.cap.pubs) != 1 {
		t.Fatalf("got %d publications, want 1: %v", len(h.cap.pubs), h.cap.bodies())
	}
	pub := h.cap.pubs[0]
	if want := "Business round trips open: BLL ⇄ TYO via LON"; pub.Title != want {
		t.Errorf("title = %q, want %q", pub.Title, want)
	}
	// News granularity matches the site's via calendar: OUT is the first-leg
	// (hop) departure day, RET the return long-haul day.
	if want := "1 new: 2–10 Oct"; pub.Body != want {
		t.Errorf("body = %q, want %q", pub.Body, want)
	}
	if want := "https://rewardflights.lucy.sh/trip/BLL-TYO?nights=7-14&out=2026-10-02&ret=2026-10-10"; pub.URL != want {
		t.Errorf("url = %q, want %q", pub.URL, want)
	}
}

// TestChainHopGainFires: the HOP going from no-space to some-space is what
// completes the journey — in any cabin, even one the watch doesn't name,
// because the hop is not a focus leg.
func TestChainHopGainFires(t *testing.T) {
	h := newHarness(t, watchVia(alertstore.KindRT))
	prev := chainComplete()
	prev["BLL-LON"] = map[int]string{}
	h.w.Baseline(bundleViaAt(t, "2026-10-01T09:00", prev))
	h.w.Cycle(bundleViaAt(t, "2026-10-01T10:00", chainComplete()))

	if len(h.cap.pubs) != 1 {
		t.Fatalf("hop opening must complete the chain: got %v", h.cap.bodies())
	}
	if want := "1 new: 2–10 Oct"; h.cap.pubs[0].Body != want {
		t.Errorf("body = %q, want %q", h.cap.pubs[0].Body, want)
	}
}

// TestChainHopCabinChurnSilent: a new cabin appearing NEXT TO existing space
// on the hop changes nothing the hop is asked for ("any award space") — the
// chain was bookable before, so there is no news.
func TestChainHopCabinChurnSilent(t *testing.T) {
	h := newHarness(t, watchVia(alertstore.KindRT))
	h.w.Baseline(bundleViaAt(t, "2026-10-01T09:00", chainComplete()))
	churn := chainComplete()
	churn["BLL-LON"] = map[int]string{2: "MC"} // C appears beside M
	h.w.Cycle(bundleViaAt(t, "2026-10-01T10:00", churn))

	if len(h.cap.pubs) != 0 {
		t.Fatalf("hop cabin churn is not news: got %v", h.cap.bodies())
	}
}

// TestChainCabinCoupling: the watched cabin must hold on BOTH long-haul legs.
// A Business gain outbound with an Economy-only return long-haul is silent.
func TestChainCabinCoupling(t *testing.T) {
	h := newHarness(t, watchVia(alertstore.KindRT))
	prev := chainComplete()
	prev["LON-TYO"] = map[int]string{}
	prev["TYO-LON"] = map[int]string{10: "M"} // no Business home
	h.w.Baseline(bundleViaAt(t, "2026-10-01T09:00", prev))
	next := chainComplete()
	next["TYO-LON"] = map[int]string{10: "M"}
	h.w.Cycle(bundleViaAt(t, "2026-10-01T10:00", next))

	if len(h.cap.pubs) != 0 {
		t.Fatalf("no shared long-haul cabin -> no chain: got %v", h.cap.bodies())
	}
}

// TestChainOvernightFloor: a same-day connection must never fire — the stop
// windows have a hard 1-night floor because flight times aren't in the data.
func TestChainOvernightFloor(t *testing.T) {
	h := newHarness(t, watchVia(alertstore.KindRT))
	sameDay := chainComplete()
	sameDay["BLL-LON"] = map[int]string{3: "M"} // hop on the LON-TYO day itself
	prev := map[string]map[int]string{}
	for k, v := range sameDay {
		prev[k] = v
	}
	prev["LON-TYO"] = map[int]string{}
	h.w.Baseline(bundleViaAt(t, "2026-10-01T09:00", prev))
	h.w.Cycle(bundleViaAt(t, "2026-10-01T10:00", sameDay))

	if len(h.cap.pubs) != 0 {
		t.Fatalf("same-day connection must not be promised: got %v", h.cap.bodies())
	}
}

// TestChainMissingLegSilent: without a hop-home route the journey can never
// complete, so even a perfect long-haul gain is silent (EC-7's chain cousin).
func TestChainMissingLegSilent(t *testing.T) {
	h := newHarness(t, watchVia(alertstore.KindRT))
	prev := chainComplete()
	delete(prev, "LON-BLL")
	prev["LON-TYO"] = map[int]string{}
	next := chainComplete()
	delete(next, "LON-BLL")
	h.w.Baseline(bundleViaAt(t, "2026-10-01T09:00", prev))
	h.w.Cycle(bundleViaAt(t, "2026-10-01T10:00", next))

	if len(h.cap.pubs) != 0 {
		t.Fatalf("missing leg route -> silence: got %v", h.cap.bodies())
	}
}

// TestChainBaselineSilent: the first bundle after a restart is a baseline,
// never news — chains included.
func TestChainBaselineSilent(t *testing.T) {
	h := newHarness(t, watchVia(alertstore.KindRT))
	h.w.Baseline(bundleViaAt(t, "2026-10-01T09:00", chainComplete()))
	if len(h.cap.pubs) != 0 {
		t.Fatalf("baseline must not alert: got %v", h.cap.bodies())
	}
}

// TestChainFlapSuppressed: a long-haul leg blinking off and on inside the
// cooldown is churn, not news — the chain flap check reads the same global
// ledger as pairs.
func TestChainFlapSuppressed(t *testing.T) {
	h := newHarness(t, watchVia(alertstore.KindRT))
	prev := chainComplete()
	prev["LON-TYO"] = map[int]string{}
	h.w.Baseline(bundleViaAt(t, "2026-10-01T09:00", prev))
	h.w.Cycle(bundleViaAt(t, "2026-10-01T10:00", chainComplete()))
	if len(h.cap.pubs) != 1 {
		t.Fatalf("setup: first opening should fire once: %v", h.cap.bodies())
	}
	// Blink: off at 11:30, back on at 12:00 — inside the 3h cooldown.
	h.w.Cycle(bundleViaAt(t, "2026-10-01T11:30", prev))
	h.w.Cycle(bundleViaAt(t, "2026-10-01T12:00", chainComplete()))
	if len(h.cap.pubs) != 1 {
		t.Fatalf("a flap inside the cooldown must not re-alert: got %v", h.cap.bodies())
	}
}

// TestChainOneWay: an ow via watch fires on the outbound chain alone and
// deep-links the one-way page.
func TestChainOneWay(t *testing.T) {
	h := newHarness(t, watchVia(alertstore.KindOW))
	h.w.Baseline(bundleViaAt(t, "2026-10-01T09:00", map[string]map[int]string{
		"BLL-LON": {2: "M"},
		"LON-TYO": {},
	}))
	h.w.Cycle(bundleViaAt(t, "2026-10-01T10:00", map[string]map[int]string{
		"BLL-LON": {2: "M"},
		"LON-TYO": {3: "C"},
	}))

	if len(h.cap.pubs) != 1 {
		t.Fatalf("got %d publications, want 1: %v", len(h.cap.pubs), h.cap.bodies())
	}
	pub := h.cap.pubs[0]
	if want := "Business seats open: BLL → TYO via LON"; pub.Title != want {
		t.Errorf("title = %q, want %q", pub.Title, want)
	}
	if want := "https://rewardflights.lucy.sh/route/BLL-TYO"; pub.URL != want {
		t.Errorf("url = %q, want %q", pub.URL, want)
	}
}

// TestChainFrontier: chain days loading beyond the previous horizon alert a
// BOUNDED-outbound watch (EC-3: "tell me when my dates load") and stay silent
// for an unbounded one.
func TestChainFrontier(t *testing.T) {
	bounded := watchVia(alertstore.KindRT)
	bounded.Out = &alertstore.Range{From: "2026-10-22", To: "2026-10-28"}
	free := watchVia(alertstore.KindRT)
	h := newHarnessWithSubs(t, map[string][]alertstore.Watch{
		testEndpoint:  {bounded},
		otherEndpoint: {free},
	})

	short := func(routes map[string]map[int]string) []byte {
		raw := bundleViaAt(t, "2026-10-01T09:00", routes)
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		// Truncate every availability string to 20 days: horizon 21 Oct.
		for _, r := range m["routes"].(map[string]any) {
			a := r.(map[string]any)["a"].(map[string]any)
			for k, v := range a {
				a[k] = v.(string)[:20]
			}
		}
		out, _ := json.Marshal(m)
		return out
	}
	// Baseline sees only 20 days; the full bundle then loads days 21-40 with
	// a complete chain at 24/25/32/33 Oct — all frontier gains.
	frontier := map[string]map[int]string{
		"BLL-LON": {24: "M"},
		"LON-TYO": {25: "C"},
		"TYO-LON": {32: "C"},
		"LON-BLL": {33: "M"},
	}
	h.w.Baseline(short(map[string]map[int]string{
		"BLL-LON": {}, "LON-TYO": {}, "TYO-LON": {}, "LON-BLL": {},
	}))
	h.w.Cycle(bundleViaAt(t, "2026-10-01T10:00", frontier))

	if len(h.cap.pubs) != 1 {
		t.Fatalf("exactly the bounded watch should fire on frontier days: %v", h.cap.bodies())
	}
	if h.cap.subs[0] != testEndpoint {
		t.Errorf("fired for %s, want the bounded watch's device", h.cap.subs[0])
	}
	if want := "1 new: 24 Oct–1 Nov"; h.cap.pubs[0].Body != want {
		t.Errorf("body = %q, want %q", h.cap.pubs[0].Body, want)
	}
}
