package alerts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/alertstore"
)

// cycle is shorthand: feed the watcher a bundle at a given source time.
func (h *harness) cycle(sourceTime string, routes map[string]map[int]string) {
	h.t.Helper()
	h.w.Cycle(bundleAt(h.t, sourceTime, routes))
}

func (h *harness) baseline(sourceTime string, routes map[string]map[int]string) {
	h.t.Helper()
	h.w.Baseline(bundleAt(h.t, sourceTime, routes))
}

// --- Cooldown / flap (§9, 10-12) -----------------------------------------

// TestClassicFlapSuppressed is §9-10: a leg blinking off and back within the
// cooldown, while its partner stays open, is not news twice.
func TestClassicFlapSuppressed(t *testing.T) {
	h := newHarness(t, watchRT(1, 30))
	open := map[string]map[int]string{"LON-TYO": {3: "C"}, "TYO-LON": {14: "C"}}
	shut := map[string]map[int]string{"LON-TYO": {}, "TYO-LON": {14: "C"}}

	h.baseline("2026-10-01T09:00", map[string]map[int]string{"LON-TYO": {}, "TYO-LON": {14: "C"}})
	h.cycle("2026-10-01T09:10", open) // opens -> alert #1
	if len(h.cap.pubs) != 1 {
		t.Fatalf("first open must alert: %v", h.cap.bodies())
	}

	h.cycle("2026-10-01T09:20", shut) // outbound closes
	h.cycle("2026-10-01T09:40", open) // reopens 20 min later
	if len(h.cap.pubs) != 1 {
		t.Fatalf("a flap inside the cooldown must not re-alert: %v", h.cap.bodies())
	}

	// Closed for good, then reopens 4h after the close: past the 3h cooldown.
	h.cycle("2026-10-01T10:00", shut)
	h.cycle("2026-10-01T14:30", open)
	if len(h.cap.pubs) != 2 {
		t.Fatalf("a genuine re-open after the cooldown must alert: %v", h.cap.bodies())
	}
}

// TestFlapWithNewPartnerMustAlert is §9-11 — THE test. The outbound blinks off;
// while it is off, a NEW return opens; the outbound comes back inside the
// cooldown. The pair (3 Oct, 14 Oct) has never been announced, so it must
// alert. A "cooldown on gains" implementation suppresses the outbound's
// re-open gain and the user never hears about this pair. Ever.
func TestFlapWithNewPartnerMustAlert(t *testing.T) {
	h := newHarness(t, watchRT(1, 30))

	// Start: outbound 3 Oct open (C), no returns at all.
	h.baseline("2026-10-01T09:00", map[string]map[int]string{
		"LON-TYO": {3: "C"}, "TYO-LON": {},
	})
	// The outbound blinks OFF.
	h.cycle("2026-10-01T09:20", map[string]map[int]string{
		"LON-TYO": {}, "TYO-LON": {},
	})
	// While it is off, the return 14 Oct opens. No pair is possible yet.
	h.cycle("2026-10-01T09:40", map[string]map[int]string{
		"LON-TYO": {}, "TYO-LON": {14: "C"},
	})
	if len(h.cap.pubs) != 0 {
		t.Fatalf("no pair exists yet, nothing to say: %v", h.cap.bodies())
	}
	// The outbound returns, 40 minutes after it closed — well inside the 3h
	// cooldown. The PAIR (3, 14) is brand new.
	h.cycle("2026-10-01T10:00", map[string]map[int]string{
		"LON-TYO": {3: "C"}, "TYO-LON": {14: "C"},
	})

	if len(h.cap.pubs) != 1 {
		t.Fatalf("the new pair 3–14 Oct MUST alert (the partner opened after the close, "+
			"so no joint run ended): got %v", h.cap.bodies())
	}
	if want := "1 new: 3–14 Oct"; h.cap.pubs[0].Body != want {
		t.Errorf("body = %q, want %q", h.cap.pubs[0].Body, want)
	}
}

// TestBothLegsFlapTogetherSuppressed is §9-12: a synchronized scrape artifact.
func TestBothLegsFlapTogetherSuppressed(t *testing.T) {
	h := newHarness(t, watchRT(1, 30))
	open := map[string]map[int]string{"LON-TYO": {3: "C"}, "TYO-LON": {14: "C"}}
	shut := map[string]map[int]string{"LON-TYO": {}, "TYO-LON": {}}

	h.baseline("2026-10-01T09:00", shut)
	h.cycle("2026-10-01T09:10", open) // alert #1
	h.cycle("2026-10-01T09:20", shut) // both legs vanish
	h.cycle("2026-10-01T09:30", open) // both come back
	if len(h.cap.pubs) != 1 {
		t.Fatalf("a synchronized blink must not re-alert: %v", h.cap.bodies())
	}
}

func TestOneWayFlapSuppressed(t *testing.T) {
	h := newHarness(t, unbounded(alertstore.KindOW, "C"))
	open := map[string]map[int]string{"LON-TYO": {3: "C"}}
	shut := map[string]map[int]string{"LON-TYO": {}}

	h.baseline("2026-10-01T09:00", shut)
	h.cycle("2026-10-01T09:10", open)
	h.cycle("2026-10-01T09:20", shut)
	h.cycle("2026-10-01T09:50", open) // 30 min later: flap
	if len(h.cap.pubs) != 1 {
		t.Fatalf("one-way flap must be suppressed: %v", h.cap.bodies())
	}
	h.cycle("2026-10-01T10:00", shut)
	h.cycle("2026-10-01T14:00", open) // 4h later: genuine
	if len(h.cap.pubs) != 2 {
		t.Fatalf("a genuine re-open must alert: %v", h.cap.bodies())
	}
}

// --- Batching (§9, 13-14) ------------------------------------------------

// TestBatchingIsPerSubscriberNotPerTopic is §9-13: two watches fire minutes
// apart. The first goes out immediately (a quiet hour precedes it); the second
// is held and merged, and no pair appears twice.
func TestBatchingIsPerSubscriberNotPerTopic(t *testing.T) {
	h := newHarness(t,
		unbounded(alertstore.KindOW, "C"),
		alertstore.Watch{Route: "NYC-LON", Kind: alertstore.KindOW, Cabins: []string{"M"}},
	)
	h.baseline("2026-10-01T09:00", map[string]map[int]string{
		"LON-TYO": {}, "NYC-LON": {},
	})

	// Watch 1 fires.
	h.cycle("2026-10-01T09:05", map[string]map[int]string{
		"LON-TYO": {3: "C"}, "NYC-LON": {},
	})
	if len(h.cap.pubs) != 1 {
		t.Fatalf("the first alert after a quiet hour goes out immediately: %v", h.cap.bodies())
	}

	// Watch 2 fires 5 minutes later: inside the batch window, so it is HELD.
	h.cycle("2026-10-01T09:10", map[string]map[int]string{
		"LON-TYO": {3: "C"}, "NYC-LON": {5: "M"},
	})
	if len(h.cap.pubs) != 1 {
		t.Fatalf("a second push inside the batch hour is exactly what this feature "+
			"exists to prevent: %v", h.cap.bodies())
	}

	// After the hour, the held news goes out — and only the held news.
	h.cycle("2026-10-01T10:10", map[string]map[int]string{
		"LON-TYO": {3: "C"}, "NYC-LON": {5: "M"},
	})
	if len(h.cap.pubs) != 2 {
		t.Fatalf("the batch must flush after the hour: %v", h.cap.bodies())
	}
	second := h.cap.pubs[1]
	if !strings.Contains(second.Body, "Mon 5 Oct") {
		t.Errorf("second push = %q, want the held NYC-LON date", second.Body)
	}
	if strings.Contains(second.Body, "3 Oct") {
		t.Errorf("second push repeats news from the first: %q", second.Body)
	}
	// Distinct tags: notifications stack rather than replace (owner decision 2).
	if h.cap.pubs[0].Tag == second.Tag {
		t.Error("tags must be unique per send, or the tray collapses unread dates")
	}
}

// TestPerDeviceHourlyCapDigest is §9-14: many watches fire at once -> ONE push,
// a digest.
func TestPerDeviceHourlyCapDigest(t *testing.T) {
	var watches []alertstore.Watch
	routes := []string{"LON-TYO", "NYC-LON", "SFO-LON", "LON-SIN"}
	for _, route := range routes {
		watches = append(watches, alertstore.Watch{
			Route: route, Kind: alertstore.KindOW, Cabins: []string{"C"},
		})
	}
	h := newHarness(t, watches...)

	before := map[string]map[int]string{}
	after := map[string]map[int]string{}
	for _, route := range routes {
		before[route] = map[int]string{}
		after[route] = map[int]string{3: "C", 5: "C"}
	}
	h.baseline("2026-10-01T09:00", before)
	h.cycle("2026-10-01T09:05", after)

	if len(h.cap.pubs) != 1 {
		t.Fatalf("4 watches firing at once must be ONE digest, got %d: %v",
			len(h.cap.pubs), h.cap.bodies())
	}
	pub := h.cap.pubs[0]
	if want := "Award space on 4 of your routes"; pub.Title != want {
		t.Errorf("title = %q, want %q", pub.Title, want)
	}
	// Capped at 3 routes, then "+N more".
	if !strings.Contains(pub.Body, "+1 more") {
		t.Errorf("digest body = %q, want a +1 more suffix", pub.Body)
	}
	if !strings.Contains(pub.Body, "LON → SIN: 2 new Business") {
		t.Errorf("digest body = %q, want per-route counts", pub.Body)
	}
	if want := "https://rewardflights.lucy.sh/alerts"; pub.URL != want {
		t.Errorf("digest url = %q, want %q", pub.URL, want)
	}
}

func TestDailyCapHoldsBatch(t *testing.T) {
	h := newHarness(t, unbounded(alertstore.KindOW, "C"))
	h.baseline("2026-10-01T00:00", map[string]map[int]string{"LON-TYO": {}})

	// Force the device to its daily cap.
	subKey := alertstore.SubKey(testEndpoint)
	h.w.state.PubDay[subKey] = &pubDay{Day: mustDay(t, "2026-10-01"), N: maxDailyPushes}

	h.cycle("2026-10-01T09:00", map[string]map[int]string{"LON-TYO": {3: "C"}})
	if len(h.cap.pubs) != 0 {
		t.Fatalf("the daily cap must hold the push: %v", h.cap.bodies())
	}
	if len(h.w.state.Pending[subKey].Items) == 0 {
		t.Fatal("capped news must stay pending, not be dropped")
	}
	// The next UTC day, the held batch goes out.
	h.cycle("2026-10-02T09:00", map[string]map[int]string{"LON-TYO": {3: "C"}})
	if len(h.cap.pubs) != 1 {
		t.Fatalf("the held batch must flush after midnight: %v", h.cap.bodies())
	}
}

// --- State (§9, 16-18) ---------------------------------------------------

// TestStateStaysBounded is §9-16: the ledger is sized by EVENTS IN THE LAST
// COOLDOWN, not by the dataset. This is the test that would have caught the
// unbounded growth of the map this design replaces.
func TestStateStaysBounded(t *testing.T) {
	h := newHarness(t, unbounded(alertstore.KindOW, "C"))

	routes := make([]string, 0, 40)
	for i := range 40 {
		routes = append(routes, fmt.Sprintf("%s-TYO", string([]byte{byte('A' + i/26), byte('A' + i%26), 'X'})))
	}
	build := func(cycle int) map[string]map[int]string {
		out := map[string]map[int]string{}
		for r, route := range routes {
			days := map[int]string{}
			// Churn: every route flips a handful of days every cycle.
			for d := 1; d <= 39; d++ {
				if (d+r+cycle)%3 == 0 {
					days[d] = "C"
				}
			}
			out[route] = days
		}
		return out
	}

	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	h.w.Baseline(bundleAt(t, start.Format("2006-01-02T15:04"), build(0)))
	for i := 1; i <= 1000; i++ {
		// Two minutes per cycle: 1000 cycles spans ~33 hours, so the ledger is
		// pruned many times over.
		when := start.Add(time.Duration(i*2) * time.Minute)
		h.w.Cycle(bundleAt(t, when.Format("2006-01-02T15:04"), build(i)))
	}

	total := len(h.w.state.OpenedAt) + len(h.w.state.ClosedAt)
	if total >= 5000 {
		t.Errorf("ledger has %d entries after 1000 churny cycles; it must stay bounded "+
			"by events in the cooldown window, not grow with the dataset", total)
	}
	// And nothing in the past lingers.
	today := unixDay(h.w.prev.t)
	for _, ledger := range []map[string]int64{h.w.state.OpenedAt, h.w.state.ClosedAt} {
		for k := range ledger {
			if ledgerDatePast(k, today) {
				t.Errorf("ledger key %q is for a past date and should have been pruned", k)
			}
		}
	}
	t.Logf("ledger after 1000 cycles: %d opened + %d closed = %d entries",
		len(h.w.state.OpenedAt), len(h.w.state.ClosedAt), total)
}

// TestRestartNoDuplicates is §9-17.
func TestRestartNoDuplicates(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	storePath := filepath.Join(dir, "subs.json")

	newWatcher := func() (*Watcher, *capture) {
		store, err := alertstore.Open(alertstore.Options{Path: storePath, Debounce: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		if _, err := store.Upsert(alertstore.Subscription{
			Endpoint: testEndpoint, P256dh: "cGsx", Auth: "YXV0aA",
			Watches: []alertstore.Watch{watchRT(1, 30)},
		}); err != nil {
			t.Fatal(err)
		}
		cap := &capture{}
		w, err := NewWatcher(Config{
			Store: store, StatePath: statePath, Publish: cap.publish, Logf: cap.logf,
		})
		if err != nil {
			t.Fatal(err)
		}
		return w, cap
	}

	open := map[string]map[int]string{"LON-TYO": {3: "C"}, "TYO-LON": {14: "C"}}
	w1, cap1 := newWatcher()
	w1.Baseline(bundleAt(t, "2026-10-01T09:00", map[string]map[int]string{
		"LON-TYO": {3: "C"}, "TYO-LON": {},
	}))
	w1.Cycle(bundleAt(t, "2026-10-01T09:10", open))
	if len(cap1.pubs) != 1 {
		t.Fatalf("expected the pair to publish before the restart: %v", cap1.bodies())
	}

	// Restart: fresh watcher, same state file, re-baseline on the current bundle.
	w2, cap2 := newWatcher()
	w2.Baseline(bundleAt(t, "2026-10-01T09:10", open))
	if len(cap2.pubs) != 0 {
		t.Fatalf("a restart's baseline must never publish: %v", cap2.bodies())
	}
	// And the already-published pair is not republished on the next cycle.
	w2.Cycle(bundleAt(t, "2026-10-01T09:20", open))
	if len(cap2.pubs) != 0 {
		t.Fatalf("an already-announced pair must not be republished after a restart: %v", cap2.bodies())
	}
}

// TestPublishFailureRetains is §9-18.
func TestPublishFailureRetains(t *testing.T) {
	h := newHarness(t, unbounded(alertstore.KindOW, "C"))
	h.cap.fail = true

	h.baseline("2026-10-01T09:00", map[string]map[int]string{"LON-TYO": {}})
	h.cycle("2026-10-01T09:10", map[string]map[int]string{"LON-TYO": {3: "C"}})

	if len(h.cap.pubs) != 0 {
		t.Fatal("a failed publish must not count as delivered")
	}
	subKey := alertstore.SubKey(testEndpoint)
	if h.w.state.Pending[subKey] == nil || len(h.w.state.Pending[subKey].Items) != 1 {
		t.Fatal("failed news must stay pending")
	}
	if !strings.Contains(strings.Join(h.cap.logs, "\n"), "alert-publish-failed") {
		t.Errorf("must warn: %v", h.cap.logs)
	}
	// The ledger still advanced: the same day is not re-detected as a gain.
	if _, ok := h.w.state.OpenedAt["LON-TYO|C|2026-10-03"]; !ok {
		t.Error("the ledger must advance even when the push failed")
	}

	// Delivery recovers: the pending item goes out, exactly once.
	h.cap.fail = false
	h.cycle("2026-10-01T09:20", map[string]map[int]string{"LON-TYO": {3: "C"}})
	if len(h.cap.pubs) != 1 {
		t.Fatalf("the retained batch must be retried: %v", h.cap.bodies())
	}
	if want := "1 new date: Sat 3 Oct"; h.cap.pubs[0].Body != want {
		t.Errorf("body = %q, want %q", h.cap.pubs[0].Body, want)
	}
}

// TestStateSchemaMismatchStartsFresh is §2.3.
func TestStateSchemaMismatchStartsFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	// A schema-1 state file (the old LastOn/lastPub shape).
	old := `{"schema":1,"lastOn":{"ow|LON-TYO|C|2026-10-03":1770000000},"lastPub":{}}`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	var logs []string
	state := loadState(path, func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) })
	if state.Schema != StateSchema || len(state.OpenedAt) != 0 || len(state.Pending) != 0 {
		t.Errorf("a schema mismatch must start fresh, got %+v", state)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "alert-state-reset") {
		t.Errorf("must log the reset: %v", logs)
	}
	// A corrupt file likewise.
	if err := os.WriteFile(path, []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s := loadState(path, func(string, ...any) {}); s.Schema != StateSchema {
		t.Error("a corrupt state file must start fresh")
	}
}

func TestStateRoundTrips(t *testing.T) {
	dir := t.TempDir()
	h := newHarness(t, unbounded(alertstore.KindOW, "C"))
	h.w.cfg.StatePath = filepath.Join(dir, "state.json")

	h.baseline("2026-10-01T09:00", map[string]map[int]string{"LON-TYO": {}})
	h.cycle("2026-10-01T09:10", map[string]map[int]string{"LON-TYO": {3: "C"}})

	raw, err := os.ReadFile(h.w.cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	var state stateData
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("state file is not valid JSON: %v", err)
	}
	if state.Schema != StateSchema {
		t.Errorf("schema = %d, want %d", state.Schema, StateSchema)
	}
	if _, ok := state.OpenedAt["LON-TYO|C|2026-10-03"]; !ok {
		t.Errorf("openedAt not persisted: %+v", state.OpenedAt)
	}
	if len(state.LastPub) != 1 {
		t.Errorf("lastPub not persisted: %+v", state.LastPub)
	}
}

// TestPrunesDeadSubscription is EC-9: a removed subscription's state goes too.
func TestPrunesDeadSubscription(t *testing.T) {
	h := newHarness(t, unbounded(alertstore.KindOW, "C"))
	h.cap.fail = true // leave something pending
	h.baseline("2026-10-01T09:00", map[string]map[int]string{"LON-TYO": {}})
	h.cycle("2026-10-01T09:10", map[string]map[int]string{"LON-TYO": {3: "C"}})

	subKey := alertstore.SubKey(testEndpoint)
	if h.w.state.Pending[subKey] == nil {
		t.Fatal("expected pending news")
	}
	// The push service reported the subscription gone.
	h.store.Remove(testEndpoint)
	h.cap.fail = false
	h.cycle("2026-10-01T09:20", map[string]map[int]string{"LON-TYO": {3: "C", 5: "C"}})

	if len(h.cap.pubs) != 0 {
		t.Errorf("a removed subscription must not be pushed to: %v", h.cap.bodies())
	}
	if _, ok := h.w.state.Pending[subKey]; ok {
		t.Error("a removed subscription's pending state must be pruned")
	}
}

func mustDay(t *testing.T, iso string) int {
	t.Helper()
	day, err := parseDay(iso)
	if err != nil {
		t.Fatal(err)
	}
	return day
}
