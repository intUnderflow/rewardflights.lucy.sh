package alerts

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/alertstore"
	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/webpush"
)

// The test calendar: epoch 2026-10-01, one nibble per day, 40 days. Index i is
// October (i+1) for i < 31. "Today" is 2026-10-01 unless a test says otherwise.
const (
	testEpoch = "2026-10-01"
	testDays  = 40
)

// oct returns the string index of an October day (1-based).
func oct(day int) int { return day - 1 }

// bundleAt builds an availability bundle. Each route's string is given as a
// map of October day -> cabin letters, so tests read like the calendar.
func bundleAt(t *testing.T, sourceDate string, routes map[string]map[int]string) []byte {
	t.Helper()
	return bundleWithDays(t, sourceDate, testDays, routes)
}

func bundleWithDays(t *testing.T, sourceDate string, days int, routes map[string]map[int]string) []byte {
	t.Helper()
	when, err := time.Parse("2006-01-02T15:04", sourceDate)
	if err != nil {
		t.Fatalf("bad source time %q: %v", sourceDate, err)
	}
	strs := map[string]any{}
	for route, avail := range routes {
		buf := make([]byte, days)
		for i := range buf {
			buf[i] = '0'
		}
		for day, cabins := range avail {
			i := oct(day)
			if i < 0 || i >= days {
				t.Fatalf("day %d outside the test calendar", day)
			}
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
				default:
					t.Fatalf("bad cabin %q", c)
				}
			}
			buf[i] = "0123456789ABCDEF"[bits]
		}
		strs[route] = map[string]any{"a": map[string]string{"BA": string(buf)}}
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

// capture records what would be pushed.
type capture struct {
	pubs []Publication
	subs []string
	fail bool
	logs []string
}

func (c *capture) publish(sub webpush.Subscription, p Publication) error {
	if c.fail {
		return fmt.Errorf("injected failure")
	}
	c.pubs = append(c.pubs, p)
	c.subs = append(c.subs, sub.Endpoint)
	return nil
}

func (c *capture) logf(format string, args ...any) {
	c.logs = append(c.logs, fmt.Sprintf(format, args...))
}

func (c *capture) bodies() []string {
	var out []string
	for _, p := range c.pubs {
		out = append(out, p.Body)
	}
	return out
}

// harness wires a store + watcher with an injected publisher.
type harness struct {
	t     *testing.T
	store *alertstore.Store
	w     *Watcher
	cap   *capture
}

func newHarness(t *testing.T, watches ...alertstore.Watch) *harness {
	t.Helper()
	return newHarnessWithSubs(t, map[string][]alertstore.Watch{testEndpoint: watches})
}

const (
	testEndpoint  = "https://fcm.googleapis.com/fcm/send/one"
	otherEndpoint = "https://fcm.googleapis.com/fcm/send/two"
)

func newHarnessWithSubs(t *testing.T, subs map[string][]alertstore.Watch) *harness {
	t.Helper()
	store, err := alertstore.Open(alertstore.Options{
		Path: filepath.Join(t.TempDir(), "subs.json"), Debounce: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	for endpoint, watches := range subs {
		if _, err := store.Upsert(alertstore.Subscription{
			Endpoint: endpoint, P256dh: "cGsx", Auth: "YXV0aA", Watches: watches,
		}); err != nil {
			t.Fatalf("upsert %s: %v", endpoint, err)
		}
	}
	cap := &capture{}
	w, err := NewWatcher(Config{Store: store, Publish: cap.publish, Logf: cap.logf})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, store: store, w: w, cap: cap}
}

// watchRT is the spec's running example: LON-TYO round trip, Business,
// out 1–20 Oct, back 10–31 Oct.
func watchRT(nmin, nmax int) alertstore.Watch {
	return alertstore.Watch{
		Route: "LON-TYO", Kind: alertstore.KindRT, Cabins: []string{"C"},
		Out:    &alertstore.Range{From: "2026-10-01", To: "2026-10-20"},
		Ret:    &alertstore.Range{From: "2026-10-10", To: "2026-10-31"},
		Nights: &alertstore.Nights{Min: nmin, Max: nmax},
	}
}

func unbounded(kind string, cabins ...string) alertstore.Watch {
	return alertstore.Watch{Route: "LON-TYO", Kind: kind, Cabins: cabins}
}

// --- Pair detection (§9, 1-9) --------------------------------------------

// TestRetGainPairsWithOldOutbound is §9-1, the crux: the RETURN opens today
// and pairs with an outbound that has been available for weeks. A day-diffing
// implementation emits nothing here — the outbound didn't change, and the
// return isn't an outbound. The PAIR is what is new.
func TestRetGainPairsWithOldOutbound(t *testing.T) {
	h := newHarness(t, watchRT(1, 30))

	// Prev: LON-TYO 3 Oct has had Business for weeks. TYO-LON 14 Oct is shut.
	h.w.Baseline(bundleAt(t, "2026-10-01T09:00", map[string]map[int]string{
		"LON-TYO": {3: "C"},
		"TYO-LON": {},
	}))
	// New: TYO-LON 14 Oct gains Business. Nothing changed on the outbound.
	h.w.Cycle(bundleAt(t, "2026-10-01T10:00", map[string]map[int]string{
		"LON-TYO": {3: "C"},
		"TYO-LON": {14: "C"},
	}))

	if len(h.cap.pubs) != 1 {
		t.Fatalf("got %d publications, want exactly 1 (the new pair 3–14 Oct): %v",
			len(h.cap.pubs), h.cap.bodies())
	}
	pub := h.cap.pubs[0]
	if want := "1 new: 3–14 Oct"; pub.Body != want {
		t.Errorf("body = %q, want %q", pub.Body, want)
	}
	if want := "Business round trips open: LON ⇄ TYO"; pub.Title != want {
		t.Errorf("title = %q, want %q", pub.Title, want)
	}
	// The deep link lands on the pair-picker with THAT trip selected.
	if want := "https://rewardflights.lucy.sh/trip/LON-TYO?nights=1-30&out=2026-10-03&ret=2026-10-14"; pub.URL != want {
		t.Errorf("url = %q, want %q", pub.URL, want)
	}
}

// TestProductionEngineMissedReturnGainPairs pins the bug the pre-2026-07
// engine shipped with, using the shape a MIGRATED LEGACY watch has (unbounded,
// default 1-30 nights) — i.e. this is not a new-feature-only case, it is
// availability that existing subscribers were entitled to and never received.
//
// The deployed detector recomputed each route's "on set" of outbound days and
// diffed those. An outbound that had been open for weeks was in the on-set
// before and after, so it produced no event — even though a return leg opening
// today makes the ROUND TRIP newly bookable for the first time.
func TestProductionEngineMissedReturnGainPairs(t *testing.T) {
	h := newHarness(t, unbounded(alertstore.KindRT, "C")) // a migrated legacy watch

	// LON-TYO 3 Oct has held Business for weeks; no return has ever been open.
	h.w.Baseline(bundleAt(t, "2026-10-01T09:00", map[string]map[int]string{
		"LON-TYO": {3: "C"},
		"TYO-LON": {},
	}))
	// A return opens. The outbound does not change at all.
	h.w.Cycle(bundleAt(t, "2026-10-01T10:00", map[string]map[int]string{
		"LON-TYO": {3: "C"},
		"TYO-LON": {14: "C"},
	}))

	if len(h.cap.pubs) != 1 {
		t.Fatalf("a legacy subscriber must be told about the new round trip 3–14 Oct; "+
			"the old engine said nothing here: got %v", h.cap.bodies())
	}
	if want := "1 new: 3–14 Oct"; h.cap.pubs[0].Body != want {
		t.Errorf("body = %q, want %q", h.cap.pubs[0].Body, want)
	}
}

// TestOutGainPairsWithOldReturn is §9-2, the mirror image.
func TestOutGainPairsWithOldReturn(t *testing.T) {
	h := newHarness(t, watchRT(1, 30))
	h.w.Baseline(bundleAt(t, "2026-10-01T09:00", map[string]map[int]string{
		"LON-TYO": {},
		"TYO-LON": {14: "C"},
	}))
	h.w.Cycle(bundleAt(t, "2026-10-01T10:00", map[string]map[int]string{
		"LON-TYO": {3: "C"},
		"TYO-LON": {14: "C"},
	}))
	if len(h.cap.pubs) != 1 || h.cap.pubs[0].Body != "1 new: 3–14 Oct" {
		t.Fatalf("want the pair 3–14 Oct, got %v", h.cap.bodies())
	}
}

// TestConstraintsExclude covers §9 3, 4, 5 and 8: the same return-leg gain as
// the crux test, but the watch's constraints rule the pair out.
func TestConstraintsExclude(t *testing.T) {
	cases := []struct {
		name  string
		watch alertstore.Watch
	}{
		{"nights window excludes (11 nights does not fit 1-5)", watchRT(1, 5)},
		{"ret range excludes (back by 13 Oct)", func() alertstore.Watch {
			w := watchRT(1, 30)
			w.Ret = &alertstore.Range{From: "2026-10-10", To: "2026-10-13"}
			return w
		}()},
		{"out range excludes (3 Oct is before the window)", func() alertstore.Watch {
			w := watchRT(1, 30)
			w.Out = &alertstore.Range{From: "2026-10-05", To: "2026-10-20"}
			return w
		}()},
		{"cabin not watched", func() alertstore.Watch {
			w := watchRT(1, 30)
			w.Cabins = []string{"F"}
			return w
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.watch)
			h.w.Baseline(bundleAt(t, "2026-10-01T09:00", map[string]map[int]string{
				"LON-TYO": {3: "C"}, "TYO-LON": {},
			}))
			h.w.Cycle(bundleAt(t, "2026-10-01T10:00", map[string]map[int]string{
				"LON-TYO": {3: "C"}, "TYO-LON": {14: "C"},
			}))
			if len(h.cap.pubs) != 0 {
				t.Errorf("must not alert, got %v", h.cap.bodies())
			}
		})
	}
}

// TestBothLegsGainSameCycle is §9-6: the pair is found by loop (a) AND loop
// (b) and must be emitted once.
func TestBothLegsGainSameCycle(t *testing.T) {
	h := newHarness(t, watchRT(1, 30))
	h.w.Baseline(bundleAt(t, "2026-10-01T09:00", map[string]map[int]string{
		"LON-TYO": {}, "TYO-LON": {},
	}))
	h.w.Cycle(bundleAt(t, "2026-10-01T10:00", map[string]map[int]string{
		"LON-TYO": {3: "C"}, "TYO-LON": {14: "C"},
	}))
	if len(h.cap.pubs) != 1 {
		t.Fatalf("got %d pubs, want 1", len(h.cap.pubs))
	}
	if want := "1 new: 3–14 Oct"; h.cap.pubs[0].Body != want {
		t.Errorf("body = %q, want %q (the pair must appear exactly once)", h.cap.pubs[0].Body, want)
	}
}

// TestAlreadySatisfiablePairIsNotNews is §9-7.
func TestAlreadySatisfiablePairIsNotNews(t *testing.T) {
	h := newHarness(t, watchRT(1, 30))
	h.w.Baseline(bundleAt(t, "2026-10-01T09:00", map[string]map[int]string{
		"LON-TYO": {3: "C"}, "TYO-LON": {14: "C"},
	}))
	// An unrelated day on the same route changes; the existing pair stands.
	h.w.Cycle(bundleAt(t, "2026-10-01T10:00", map[string]map[int]string{
		"LON-TYO": {3: "C", 25: "M"}, "TYO-LON": {14: "C"},
	}))
	if len(h.cap.pubs) != 0 {
		t.Errorf("an already-satisfiable pair is not news, got %v", h.cap.bodies())
	}
}

// TestHorizonGrowth is §9-9 / EC-3: a new day arrives at the end of the string
// already open. An unbounded watch must NOT be pinged (the frontier crawls
// daily, forever); a watch whose outbound range covers that day MUST be —
// that is the whole "tell me when BA loads next October" use case.
func TestHorizonGrowth(t *testing.T) {
	prev := bundleWithDays(t, "2026-10-01T09:00", 20, map[string]map[int]string{
		"LON-TYO": {3: "C"}, "TYO-LON": {14: "C"},
	})
	// Day 21 (2026-10-21) arrives, already holding Business on both legs.
	next := bundleWithDays(t, "2026-10-01T10:00", 21, map[string]map[int]string{
		"LON-TYO": {3: "C", 21: "C"}, "TYO-LON": {14: "C", 21: "C"},
	})

	t.Run("unbounded watch is not pinged by the frontier", func(t *testing.T) {
		h := newHarness(t, unbounded(alertstore.KindOW, "C"))
		h.w.Baseline(prev)
		h.w.Cycle(next)
		if len(h.cap.pubs) != 0 {
			t.Errorf("horizon growth must be silent for an unbounded watch, got %v", h.cap.bodies())
		}
	})

	t.Run("bounded watch hears its dates load", func(t *testing.T) {
		h := newHarness(t, alertstore.Watch{
			Route: "LON-TYO", Kind: alertstore.KindOW, Cabins: []string{"C"},
			Out: &alertstore.Range{From: "2026-10-15", To: "2026-10-25"},
		})
		h.w.Baseline(prev)
		h.w.Cycle(next)
		if len(h.cap.pubs) != 1 {
			t.Fatalf("a bounded watch must hear the frontier reach its dates, got %v", h.cap.bodies())
		}
		if want := "1 new date: Wed 21 Oct"; h.cap.pubs[0].Body != want {
			t.Errorf("body = %q, want %q", h.cap.pubs[0].Body, want)
		}
	})
}

func TestOneWayDetection(t *testing.T) {
	h := newHarness(t, unbounded(alertstore.KindOW, "C", "F"))
	h.w.Baseline(bundleAt(t, "2026-10-01T09:00", map[string]map[int]string{"LON-TYO": {}}))
	h.w.Cycle(bundleAt(t, "2026-10-01T10:00", map[string]map[int]string{
		"LON-TYO": {3: "C", 5: "F", 7: "M"}, // M is not watched
	}))
	if len(h.cap.pubs) != 1 {
		t.Fatalf("got %v", h.cap.bodies())
	}
	pub := h.cap.pubs[0]
	if want := "Award seats open: LON → TYO"; pub.Title != want {
		t.Errorf("title = %q, want %q", pub.Title, want)
	}
	if want := "Business: Sat 3 Oct · First: Mon 5 Oct"; pub.Body != want {
		t.Errorf("body = %q, want %q", pub.Body, want)
	}
	if want := "https://rewardflights.lucy.sh/route/LON-TYO"; pub.URL != want {
		t.Errorf("url = %q", pub.URL)
	}
}

func TestNoReverseRouteNeverFires(t *testing.T) {
	h := newHarness(t, alertstore.Watch{
		Route: "ANU-SKB", Kind: alertstore.KindRT, Cabins: []string{"C"},
	})
	h.w.Baseline(bundleAt(t, "2026-10-01T09:00", map[string]map[int]string{"ANU-SKB": {}}))
	h.w.Cycle(bundleAt(t, "2026-10-01T10:00", map[string]map[int]string{"ANU-SKB": {3: "C"}}))
	if len(h.cap.pubs) != 0 {
		t.Errorf("a round trip with no reverse route can never fire, got %v", h.cap.bodies())
	}
}

func TestExpiredWatchIsSkipped(t *testing.T) {
	h := newHarness(t, alertstore.Watch{
		Route: "LON-TYO", Kind: alertstore.KindOW, Cabins: []string{"C"},
		Out: &alertstore.Range{From: "2026-09-01", To: "2026-09-10"}, // all in the past
	})
	h.w.Baseline(bundleAt(t, "2026-10-01T09:00", map[string]map[int]string{"LON-TYO": {}}))
	h.w.Cycle(bundleAt(t, "2026-10-01T10:00", map[string]map[int]string{"LON-TYO": {3: "C"}}))
	if len(h.cap.pubs) != 0 {
		t.Errorf("an expired watch must not fire, got %v", h.cap.bodies())
	}
}

// TestNewRouteBaselinesNotAlerts is EC-11.
func TestNewRouteBaselinesNotAlerts(t *testing.T) {
	h := newHarness(t, unbounded(alertstore.KindOW, "C"))
	h.w.Baseline(bundleAt(t, "2026-10-01T09:00", map[string]map[int]string{"NYC-LON": {3: "C"}}))
	// LON-TYO appears for the first time, already open.
	h.w.Cycle(bundleAt(t, "2026-10-01T10:00", map[string]map[int]string{
		"NYC-LON": {3: "C"}, "LON-TYO": {3: "C"},
	}))
	if len(h.cap.pubs) != 0 {
		t.Errorf("a brand-new route baselines, it does not alert; got %v", h.cap.bodies())
	}
}

// TestSourceTimeBackwards is EC-10.
func TestSourceTimeBackwards(t *testing.T) {
	h := newHarness(t, unbounded(alertstore.KindOW, "C"))
	h.w.Baseline(bundleAt(t, "2026-10-01T10:00", map[string]map[int]string{"LON-TYO": {}}))
	h.w.Cycle(bundleAt(t, "2026-10-01T09:00", map[string]map[int]string{"LON-TYO": {3: "C"}}))
	if len(h.cap.pubs) != 0 {
		t.Errorf("a source rollback must re-baseline, not alert: %v", h.cap.bodies())
	}
	if !strings.Contains(strings.Join(h.cap.logs, "\n"), "alert-source-time-backwards") {
		t.Errorf("must warn; logs = %v", h.cap.logs)
	}
	// ...and it re-baselined, so the next genuine gain still works.
	h.w.Cycle(bundleAt(t, "2026-10-01T11:00", map[string]map[int]string{"LON-TYO": {3: "C", 4: "C"}}))
	if len(h.cap.pubs) != 1 {
		t.Errorf("after re-baselining, a real gain must alert: %v", h.cap.bodies())
	}
}

// TestBulkChangeCircuitBreaker is §9-15 / EC-13: a source rebuild must not
// page a thousand people.
func TestBulkChangeCircuitBreaker(t *testing.T) {
	h := newHarness(t, unbounded(alertstore.KindOW, "C"))

	empty := map[string]map[int]string{}
	full := map[string]map[int]string{}
	for i := range 100 { // 100 routes x 39 days = 3900 gain-days > 2000
		route := fmt.Sprintf("%s-TYO", string([]byte{byte('A' + i/26), byte('A' + i%26), 'X'}))
		empty[route] = map[int]string{}
		days := map[int]string{}
		for d := 1; d <= 39; d++ {
			days[d] = "C"
		}
		full[route] = days
	}
	empty["LON-TYO"] = map[int]string{}
	full["LON-TYO"] = map[int]string{3: "C"}

	h.w.Baseline(bundleAt(t, "2026-10-01T09:00", empty))
	h.w.Cycle(bundleAt(t, "2026-10-01T10:00", full))

	if len(h.cap.pubs) != 0 {
		t.Fatalf("a bulk change must publish nothing, got %d publications", len(h.cap.pubs))
	}
	if !strings.Contains(strings.Join(h.cap.logs, "\n"), "alert-bulk-change") {
		t.Errorf("must WARN; logs = %v", h.cap.logs)
	}
	// The ledger advanced: the same data next cycle is not "new".
	h.w.Cycle(bundleAt(t, "2026-10-01T11:00", full))
	if len(h.cap.pubs) != 0 {
		t.Errorf("the ledger must have advanced through the breaker, got %v", h.cap.bodies())
	}
}

// --- Multi-subscriber (§9, 19-20) ----------------------------------------

// TestTwoSubsSameRouteDifferentRanges is §9-19: the same return opens; only
// the subscriber whose outbound window can reach it is told.
func TestTwoSubsSameRouteDifferentRanges(t *testing.T) {
	october := watchRT(1, 30) // out 1–20 Oct
	november := watchRT(1, 30)
	november.Out = &alertstore.Range{From: "2026-11-01", To: "2026-11-20"}
	november.Ret = &alertstore.Range{From: "2026-11-10", To: "2026-11-30"}

	h := newHarnessWithSubs(t, map[string][]alertstore.Watch{
		testEndpoint:  {october},
		otherEndpoint: {november},
	})
	h.w.Baseline(bundleAt(t, "2026-10-01T09:00", map[string]map[int]string{
		"LON-TYO": {3: "C"}, "TYO-LON": {},
	}))
	h.w.Cycle(bundleAt(t, "2026-10-01T10:00", map[string]map[int]string{
		"LON-TYO": {3: "C"}, "TYO-LON": {14: "C"},
	}))

	if len(h.cap.pubs) != 1 {
		t.Fatalf("got %d publications, want 1 (only the October subscriber)", len(h.cap.pubs))
	}
	if h.cap.subs[0] != testEndpoint {
		t.Errorf("published to %s, want the October subscriber", h.cap.subs[0])
	}
}

// TestSameSubTwoOverlappingWatches is §9-20: a pair matching two of the user's
// watches appears once.
func TestSameSubTwoOverlappingWatches(t *testing.T) {
	broad := watchRT(1, 30)
	narrow := watchRT(7, 14) // 3–14 Oct is 11 nights: inside both
	h := newHarness(t, broad, narrow)

	h.w.Baseline(bundleAt(t, "2026-10-01T09:00", map[string]map[int]string{
		"LON-TYO": {3: "C"}, "TYO-LON": {},
	}))
	h.w.Cycle(bundleAt(t, "2026-10-01T10:00", map[string]map[int]string{
		"LON-TYO": {3: "C"}, "TYO-LON": {14: "C"},
	}))

	if len(h.cap.pubs) != 1 {
		t.Fatalf("got %d publications, want 1", len(h.cap.pubs))
	}
	if want := "1 new: 3–14 Oct"; h.cap.pubs[0].Body != want {
		t.Errorf("body = %q, want %q (the pair must not be listed twice)", h.cap.pubs[0].Body, want)
	}
	// Both watches are recorded as having fired.
	for _, w := range h.store.Watches(testEndpoint) {
		if w.LastFiredAt == 0 {
			t.Errorf("watch %s (%+v) should have been marked fired", w.ID, w.Nights)
		}
	}
}

// TestDeterminism: replaying the same sequence produces identical output.
func TestDeterminism(t *testing.T) {
	run := func() []Publication {
		h := newHarness(t, watchRT(1, 30), unbounded(alertstore.KindOW, "C", "F"))
		h.w.Baseline(bundleAt(t, "2026-10-01T09:00", map[string]map[int]string{
			"LON-TYO": {3: "C"}, "TYO-LON": {},
		}))
		h.w.Cycle(bundleAt(t, "2026-10-01T10:00", map[string]map[int]string{
			"LON-TYO": {3: "C", 5: "F"}, "TYO-LON": {14: "C"},
		}))
		return h.cap.pubs
	}
	first, second := run(), run()
	if len(first) != len(second) {
		t.Fatalf("run lengths differ: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("publication %d differs:\n%+v\n%+v", i, first[i], second[i])
		}
	}
}

func TestItemsSortedDeterministically(t *testing.T) {
	items := []item{
		{Route: "NYC-LON", Kind: "ow", Cabin: "M", Out: "2026-10-05"},
		{Route: "LON-TYO", Kind: "rt", Cabin: "F", Out: "2026-10-03", Ret: "2026-10-10"},
		{Route: "LON-TYO", Kind: "rt", Cabin: "C", Out: "2026-10-04", Ret: "2026-10-11"},
		{Route: "LON-TYO", Kind: "rt", Cabin: "C", Out: "2026-10-03", Ret: "2026-10-10"},
	}
	sortItems(items)
	got := make([]string, len(items))
	for i, it := range items {
		got[i] = it.Route + "/" + it.Cabin + "/" + it.Out
	}
	want := []string{
		"LON-TYO/C/2026-10-03", "LON-TYO/C/2026-10-04", "LON-TYO/F/2026-10-03", "NYC-LON/M/2026-10-05",
	}
	if !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v (route, then cabin M<W<C<F, then date)", got, want)
	}
}
