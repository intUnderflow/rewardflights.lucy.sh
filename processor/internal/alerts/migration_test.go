package alerts

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/alertstore"
)

// legacyAlertSet is a REFERENCE implementation of the pre-watches semantics,
// written straight from the old detector's rules and deliberately independent
// of the new code:
//
//	ow topic rf_R_ow_c  -> day D in [today, prevHorizon) where R gained c
//	rt topic rf_R_rt_c  -> the same, joined with "some return in [D+1, D+30]
//	                       on the reverse route, inside the old horizon"
//
// It yields the set of (route, kind, cabin, outboundDay) that the OLD code
// would have alerted on. §9-21 asserts the new engine's alert set is identical
// for a migrated legacy subscription — which is the migration's real contract.
// (The published MESSAGES cannot be byte-identical: batching moved from
// per-topic to per-device by owner decision 3, so a device that had two topics
// fire now gets one digest instead of two pushes. The underlying news is what
// must not change.)
func legacyAlertSet(t *testing.T, prevRaw, nextRaw []byte, topics []string) map[string]bool {
	t.Helper()
	prev, err := parseBundle(prevRaw)
	if err != nil {
		t.Fatal(err)
	}
	next, err := parseBundle(nextRaw)
	if err != nil {
		t.Fatal(err)
	}
	today := unixDay(next.t)
	out := map[string]bool{}

	bit := map[string]byte{"M": 1, "W": 2, "C": 4, "F": 8}
	at := func(b *bundleState, route string, day int) byte {
		bits, ok := b.merged[route]
		if !ok || day < b.epochDay || day >= b.epochDay+len(bits) {
			return 0
		}
		return bits[day-b.epochDay]
	}

	for _, topic := range topics {
		parts := strings.Split(topic, "_")
		route, kind, cabin := parts[1], parts[2], parts[3]
		c := bit[cabin]
		rev := alertstore.ReverseRoute(route)

		// The old horizon clip: days at or beyond the previous bundle's end
		// were never "new" (this is what EC-3 revisits for bounded watches).
		hi := min(prev.endDay, next.endDay)
		for d := max(today, prev.epochDay, next.epochDay); d < hi; d++ {
			if at(next, route, d)&c == 0 || at(prev, route, d)&c != 0 {
				continue // not a gain on the outbound leg
			}
			if kind == "rt" {
				joined := false
				for r := d + 1; r <= min(d+30, hi-1); r++ {
					if at(next, rev, r)&c != 0 {
						joined = true
						break
					}
				}
				if !joined {
					continue
				}
			}
			out[fmt.Sprintf("%s|%s|%s|%s", route, kind, cabin, dayDate(d))] = true
		}
	}
	return out
}

// newAlertSet runs the real engine over a migrated legacy subscription and
// reports the same (route, kind, cabin, outboundDay) set.
func newAlertSet(t *testing.T, prevRaw, nextRaw []byte, topics []string) map[string]bool {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "subs.json")

	// A genuine schema-1 store file, migrated on load.
	quoted := make([]string, len(topics))
	for i, topic := range topics {
		quoted[i] = fmt.Sprintf("%q", topic)
	}
	v1 := fmt.Sprintf(`{"schema":1,"subs":[{"endpoint":%q,"p256dh":"cGsx","auth":"YXV0aA","topics":[%s]}]}`,
		testEndpoint, strings.Join(quoted, ","))
	if err := os.WriteFile(path, []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := alertstore.Open(alertstore.Options{Path: path, Debounce: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Delivery is made to fail on purpose: the news then stays in the pending
	// batch, where it can be compared as DATA rather than as message copy.
	cap := &capture{fail: true}
	w, err := NewWatcher(Config{Store: store, Publish: cap.publish, Logf: cap.logf})
	if err != nil {
		t.Fatal(err)
	}
	w.Baseline(prevRaw)
	w.Cycle(nextRaw)

	out := map[string]bool{}
	for _, batch := range w.state.Pending {
		for _, it := range batch.Items {
			out[fmt.Sprintf("%s|%s|%s|%s", it.Route, it.Kind, it.Cabin, it.Out)] = true
		}
	}
	return out
}

// TestLegacyTopicsAlertSupersetOfOldEngine is §9-21, with its premise
// corrected.
//
// The spec asked for "identical output" from the old and new engines for a
// migrated legacy subscription. That premise is WRONG, and the fixture below
// proves it: the old engine only ever diffed OUTBOUND days, so a round trip
// created by a RETURN-leg gain pairing with an already-open outbound was
// silently dropped. Those are real, bookable round trips that existing
// subscribers should have been told about and never were.
//
// So the invariant is a superset, not an identity:
//
//   - new ⊇ old: every alert the deployed engine produced is still produced,
//     so no existing subscriber loses anything in the migration; and
//   - every EXTRA alert is a genuine return-gain pair — asserted as a PROPERTY
//     of the data (the return leg gained the cabin this cycle while the
//     outbound was already open), not by hardcoding the string, so the test
//     explains why the extra is legitimate rather than merely tolerating it.
//
// The one-way alerts and the old engine's round-trip alerts are additionally
// pinned as a golden set, so a genuine regression still fails loudly.
func TestLegacyTopicsAlertSupersetOfOldEngine(t *testing.T) {
	topics := []string{
		"rf_LON-TYO_rt_C", "rf_LON-TYO_rt_F", "rf_LON-TYO_ow_C", "rf_NYC-LON_ow_M",
	}

	prev := bundleAt(t, "2026-10-01T09:00", map[string]map[int]string{
		"LON-TYO": {3: "C", 8: "F"},
		"TYO-LON": {14: "C", 20: "F"},
		"NYC-LON": {2: "M"},
		"LON-NYC": {9: "M"},
	})
	next := bundleAt(t, "2026-10-01T10:00", map[string]map[int]string{
		"LON-TYO": {3: "C", 5: "C", 8: "F", 11: "F"}, // 5 Oct C, 11 Oct F: outbound gains
		"TYO-LON": {14: "C", 20: "F", 25: "C"},       // 25 Oct C: a RETURN gain
		"NYC-LON": {2: "M", 6: "M"},                  // 6 Oct M: a one-way gain
		"LON-NYC": {9: "M"},
	})

	old := legacyAlertSet(t, prev, next, topics)
	got := newAlertSet(t, prev, next, topics)

	// Golden: exactly what the deployed engine finds. A regression here is a
	// real regression.
	wantOld := []string{
		"LON-TYO|ow|C|2026-10-05",
		"LON-TYO|rt|C|2026-10-05",
		"LON-TYO|rt|F|2026-10-11",
		"NYC-LON|ow|M|2026-10-06",
	}
	if !slices.Equal(sortedKeys(old), wantOld) {
		t.Fatalf("the reference (old-engine) set drifted:\n got: %v\nwant: %v", sortedKeys(old), wantOld)
	}

	// 1. No regressions: everything the old engine said is still said.
	for _, k := range wantOld {
		if !got[k] {
			t.Errorf("migration LOST an alert the old engine produced: %s", k)
		}
	}

	// 2. Every extra is a legitimate return-gain pair, proven from the data.
	extras := []string{}
	for _, k := range sortedKeys(got) {
		if !old[k] {
			extras = append(extras, k)
		}
	}
	if len(extras) == 0 {
		t.Fatal("the fixture must contain a return-gain pair, or it proves nothing " +
			"about the bug this design fixes")
	}
	for _, k := range extras {
		assertReturnGainPair(t, prev, next, k)
	}
	t.Logf("old engine: %d alerts; new engine: %d (extras, all return-gain pairs the "+
		"deployed engine silently dropped: %v)", len(old), len(got), extras)
}

// assertReturnGainPair proves an extra alert is a round trip whose RETURN leg
// gained the cabin this cycle while the outbound was ALREADY open — i.e. the
// pair is genuinely new, and genuinely invisible to a day-diffing engine.
func assertReturnGainPair(t *testing.T, prevRaw, nextRaw []byte, alertKey string) {
	t.Helper()
	parts := strings.Split(alertKey, "|")
	route, kind, cabin, outISO := parts[0], parts[1], parts[2], parts[3]

	if kind != alertstore.KindRT {
		t.Errorf("%s: only round trips can be new without an outbound gain", alertKey)
		return
	}
	prev, err := parseBundle(prevRaw)
	if err != nil {
		t.Fatal(err)
	}
	next, err := parseBundle(nextRaw)
	if err != nil {
		t.Fatal(err)
	}
	bit := map[string]byte{"M": 1, "W": 2, "C": 4, "F": 8}[cabin]
	at := func(b *bundleState, r string, day int) byte {
		bits, ok := b.merged[r]
		if !ok || day < b.epochDay || day >= b.epochDay+len(bits) {
			return 0
		}
		return bits[day-b.epochDay]
	}
	outDay, err := parseDay(outISO)
	if err != nil {
		t.Fatal(err)
	}

	// The outbound did NOT gain: it was already open. This is exactly why the
	// old engine, which diffs outbound days, could never emit this pair.
	if at(prev, route, outDay)&bit == 0 {
		t.Errorf("%s: the outbound gained this cycle, so the old engine should have "+
			"found it too — this extra is unexplained", alertKey)
		return
	}

	// Some return inside the nights window gained the cabin this cycle.
	rev := alertstore.ReverseRoute(route)
	for r := outDay + 1; r <= outDay+30; r++ {
		gained := at(next, rev, r)&bit != 0 && at(prev, rev, r)&bit == 0
		if gained {
			t.Logf("%s is legitimate: outbound %s was already open; return %s gained %s "+
				"this cycle, creating a brand-new pair", alertKey, outISO, dayDate(r), cabin)
			return
		}
	}
	t.Errorf("%s: no return leg gained %s in the nights window — this extra is unexplained",
		alertKey, cabin)
}

// TestLegacySupersetOnRealBundles runs the same superset invariant against two
// real consecutive availability bundles when they are provided.
func TestLegacySupersetOnRealBundles(t *testing.T) {
	oldPath, newPath := os.Getenv("RF_DRYRUN_OLD"), os.Getenv("RF_DRYRUN_NEW")
	if oldPath == "" || newPath == "" {
		t.Skip("set RF_DRYRUN_OLD / RF_DRYRUN_NEW to real bundle files")
	}
	prev, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	next, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatal(err)
	}
	topics := []string{
		"rf_LON-SFO_rt_C", "rf_LON-SFO_ow_C", "rf_LON-TYO_rt_C", "rf_LON-TYO_ow_C",
	}
	old := legacyAlertSet(t, prev, next, topics)
	got := newAlertSet(t, prev, next, topics)

	// No regressions on real data...
	for _, k := range sortedKeys(old) {
		if !got[k] {
			t.Errorf("migration LOST a real alert the old engine produced: %s", k)
		}
	}
	// ...and every extra is a return-gain pair the old engine could not see.
	var extras []string
	for _, k := range sortedKeys(got) {
		if !old[k] {
			extras = append(extras, k)
			assertReturnGainPair(t, prev, next, k)
		}
	}
	t.Logf("real bundles: old engine %d alerts, new engine %d (extras: %v)",
		len(old), len(got), extras)
}

// TestDryRunRealBundles prints what different subscribers receive from the SAME
// real commit pair: a legacy unconstrained subscription, a date-constrained one
// whose window does not cover the change, and a date-constrained one whose
// window does. It is the date filter, on production data.
func TestDryRunRealBundles(t *testing.T) {
	oldPath, newPath := os.Getenv("RF_DRYRUN_OLD"), os.Getenv("RF_DRYRUN_NEW")
	if oldPath == "" || newPath == "" {
		t.Skip("set RF_DRYRUN_OLD / RF_DRYRUN_NEW to real bundle files")
	}
	prev, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	next, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatal(err)
	}

	const (
		legacyEP   = "https://fcm.googleapis.com/fcm/send/legacy"
		octoberEP  = "https://fcm.googleapis.com/fcm/send/october"
		matchingEP = "https://fcm.googleapis.com/fcm/send/july"
	)
	labels := map[string]string{
		legacyEP:   "LEGACY      (unconstrained: LON-TYO rt C, any dates)",
		octoberEP:  "CONSTRAINED (out 2026-10-01..20, ret 2026-10-10..31, 7-14 nights)",
		matchingEP: "CONSTRAINED (out 2026-07-01..29, ret 2026-07-25..08-05, 1-14 nights)",
	}

	h := newHarnessWithSubs(t, map[string][]alertstore.Watch{
		// A migrated legacy subscriber: unbounded, default nights.
		legacyEP: {{Route: "LON-TYO", Kind: alertstore.KindRT, Cabins: []string{"C"}}},
		// The spec's worked example. Its window is nowhere near this change.
		octoberEP: {{
			Route: "LON-TYO", Kind: alertstore.KindRT, Cabins: []string{"C"},
			Out:    &alertstore.Range{From: "2026-10-01", To: "2026-10-20"},
			Ret:    &alertstore.Range{From: "2026-10-10", To: "2026-10-31"},
			Nights: &alertstore.Nights{Min: 7, Max: 14},
		}},
		// A subscriber whose window DOES cover it, so the filter is shown to
		// discriminate rather than merely to silence.
		matchingEP: {{
			Route: "LON-TYO", Kind: alertstore.KindRT, Cabins: []string{"C"},
			Out:    &alertstore.Range{From: "2026-07-01", To: "2026-07-29"},
			Ret:    &alertstore.Range{From: "2026-07-25", To: "2026-08-05"},
			Nights: &alertstore.Nights{Min: 1, Max: 14},
		}},
	})
	h.w.Baseline(prev)
	h.w.Cycle(next)

	got := map[string]Publication{}
	for i, pub := range h.cap.pubs {
		got[h.cap.subs[i]] = pub
	}
	fmt.Printf("\n=== DRY RUN on two real consecutive bundles\n")
	for _, endpoint := range []string{legacyEP, octoberEP, matchingEP} {
		fmt.Printf("\n  %s\n", labels[endpoint])
		pub, ok := got[endpoint]
		if !ok {
			fmt.Printf("     -> NOTHING (filtered out by its dates)\n")
			continue
		}
		fmt.Printf("     -> title: %s\n        body:  %s\n        url:   %s\n",
			pub.Title, pub.Body, pub.URL)
	}
	fmt.Println()

	// The whole point of the feature: a change outside your dates is not your
	// notification.
	if pub, ok := got[octoberEP]; ok {
		t.Errorf("the October-constrained subscriber must NOT be alerted about a "+
			"change outside their dates, got %+v", pub)
	}
}

func equalSets(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
