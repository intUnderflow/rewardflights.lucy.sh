package alertstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	endpointA = "https://fcm.googleapis.com/fcm/send/aaa"
	endpointB = "https://web.push.apple.com/QWERTY"
	endpointC = "https://updates.push.services.mozilla.com/wpush/v2/zzz"
)

var fixedNow = time.Unix(1783810462, 0) // 2026-07-11

func openStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(Options{
		Path: path, Debounce: time.Millisecond,
		Now: func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// sub builds a subscription with real-looking (but fake) push keys.
func sub(endpoint string, watches ...Watch) Subscription {
	return Subscription{Endpoint: endpoint, P256dh: "cGsx", Auth: "YXV0aA", Watches: watches}
}

func rt(route string, cabins ...string) Watch {
	return Watch{Route: route, Kind: KindRT, Cabins: cabins}
}

func ow(route string, cabins ...string) Watch {
	return Watch{Route: route, Kind: KindOW, Cabins: cabins}
}

// constrained is the date-bounded watch the whole feature exists for.
func constrained() Watch {
	return Watch{
		Route: "LON-TYO", Kind: KindRT, Cabins: []string{"C"},
		Out:    &Range{From: "2026-10-01", To: "2026-10-20"},
		Ret:    &Range{From: "2026-10-10", To: "2026-10-31"},
		Nights: &Nights{Min: 7, Max: 14},
	}
}

func TestNormalizeAndID(t *testing.T) {
	w, err := Normalize(Watch{Route: "LON-TYO", Kind: KindRT, Cabins: []string{"F", "C", "C"}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(w.Cabins, []string{"C", "F"}) {
		t.Errorf("cabins = %v, want sorted M<W<C<F and deduped", w.Cabins)
	}
	if len(w.ID) != 8 {
		t.Errorf("id = %q, want 8 hex chars", w.ID)
	}
	// Content-addressed: same content -> same id, regardless of input order.
	same, _ := Normalize(Watch{Route: "LON-TYO", Kind: KindRT, Cabins: []string{"C", "F"}})
	if same.ID != w.ID {
		t.Errorf("ids differ for identical content: %s vs %s", same.ID, w.ID)
	}
	// Different constraints -> different id.
	other, _ := Normalize(constrained())
	if other.ID == w.ID {
		t.Error("constrained watch must not collide with the unbounded one")
	}
}

func TestNormalizeRejects(t *testing.T) {
	cases := []struct {
		name string
		w    Watch
		want string
	}{
		{"bad route", Watch{Route: "LONDON-TYO", Kind: KindRT, Cabins: []string{"C"}}, "route"},
		{"lowercase route", Watch{Route: "lon-tyo", Kind: KindRT, Cabins: []string{"C"}}, "route"},
		{"bad kind", Watch{Route: "LON-TYO", Kind: "xx", Cabins: []string{"C"}}, "kind"},
		{"no cabins", Watch{Route: "LON-TYO", Kind: KindRT}, "cabin"},
		{"bad cabin", Watch{Route: "LON-TYO", Kind: KindRT, Cabins: []string{"Z"}}, "cabin"},
		{"ret on ow", Watch{Route: "LON-TYO", Kind: KindOW, Cabins: []string{"C"},
			Ret: &Range{From: "2026-10-01"}}, "one-way"},
		{"nights on ow", Watch{Route: "LON-TYO", Kind: KindOW, Cabins: []string{"C"},
			Nights: &Nights{Min: 1, Max: 5}}, "one-way"},
		{"impossible date", Watch{Route: "LON-TYO", Kind: KindRT, Cabins: []string{"C"},
			Out: &Range{From: "2026-02-31", To: "2026-03-05"}}, "not a real date"},
		{"from after to", Watch{Route: "LON-TYO", Kind: KindRT, Cabins: []string{"C"},
			Out: &Range{From: "2026-10-20", To: "2026-10-01"}}, "starts after"},
		{"range too long", Watch{Route: "LON-TYO", Kind: KindRT, Cabins: []string{"C"},
			Out: &Range{From: "2026-01-01", To: "2026-12-01"}}, "at most 180 days"},
		{"nights too many", Watch{Route: "LON-TYO", Kind: KindRT, Cabins: []string{"C"},
			Nights: &Nights{Min: 1, Max: 61}}, "nights"},
		{"nights inverted", Watch{Route: "LON-TYO", Kind: KindRT, Cabins: []string{"C"},
			Nights: &Nights{Min: 9, Max: 2}}, "nights"},
		// EC-4: the return window ends before the outbound plus the minimum stay.
		{"impossible: ret too early", Watch{Route: "LON-TYO", Kind: KindRT, Cabins: []string{"C"},
			Out:    &Range{From: "2026-10-01", To: "2026-10-20"},
			Ret:    &Range{From: "2026-10-01", To: "2026-10-05"},
			Nights: &Nights{Min: 7, Max: 14}}, "ends before"},
		{"impossible: ret too late", Watch{Route: "LON-TYO", Kind: KindRT, Cabins: []string{"C"},
			Out:    &Range{From: "2026-10-01", To: "2026-10-05"},
			Ret:    &Range{From: "2026-11-01", To: "2026-11-10"},
			Nights: &Nights{Min: 1, Max: 14}}, "starts more than"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Normalize(tc.w)
			if err == nil {
				t.Fatalf("must be rejected")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q must mention %q", err, tc.want)
			}
		})
	}

	// The accept cases: unbounded, half-bounded, and the fully constrained one.
	for _, w := range []Watch{
		rt("LON-TYO", "C"),
		ow("NYC-LON", "M", "F"),
		{Route: "LON-TYO", Kind: KindRT, Cabins: []string{"C"}, Out: &Range{From: "2026-10-01"}},
		{Route: "LON-TYO", Kind: KindRT, Cabins: []string{"C"}, Nights: &Nights{Min: 2, Max: 4}},
		constrained(),
	} {
		if _, err := Normalize(w); err != nil {
			t.Errorf("%+v must be accepted: %v", w, err)
		}
	}
}

func TestUpsertReplacesAndCarriesHistory(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "subs.json"))

	saved, err := s.Upsert(sub(endpointA, rt("LON-TYO", "C"), constrained()))
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 2 {
		t.Fatalf("saved %d watches, want 2", len(saved))
	}
	for _, w := range saved {
		if w.CreatedAt != fixedNow.Unix() {
			t.Errorf("createdAt = %d, want %d", w.CreatedAt, fixedNow.Unix())
		}
	}
	s.MarkFired(endpointA, []string{saved[0].ID}, 1783900000)

	// Re-saving the same list preserves createdAt and lastFiredAt.
	again, err := s.Upsert(sub(endpointA, rt("LON-TYO", "C"), constrained()))
	if err != nil {
		t.Fatal(err)
	}
	if again[0].LastFiredAt != 1783900000 {
		t.Errorf("lastFiredAt was reset: %+v", again[0])
	}
	// Duplicates collapse for free (content-addressed ids).
	dup, err := s.Upsert(sub(endpointA, rt("LON-TYO", "C"), rt("LON-TYO", "C")))
	if err != nil {
		t.Fatal(err)
	}
	if len(dup) != 1 {
		t.Errorf("duplicate watches must collapse, got %d", len(dup))
	}
	// A v2 write REPLACES the whole list.
	if got := s.Watches(endpointA); len(got) != 1 || got[0].Route != "LON-TYO" {
		t.Errorf("v2 upsert must replace the list, got %+v", got)
	}
}

func TestTooManyWatches(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "subs.json"))
	many := make([]Watch, MaxWatchesPerSub+1)
	for i := range many {
		many[i] = ow(fmt.Sprintf("%s-TYO", string([]byte{byte('A' + i/26), byte('A' + i%26), 'X'})), "C")
	}
	if _, err := s.Upsert(sub(endpointA, many...)); err != ErrTooManyWatch {
		t.Errorf("err = %v, want ErrTooManyWatch", err)
	}
	if _, err := s.Upsert(sub(endpointA, many[:MaxWatchesPerSub]...)); err != nil {
		t.Errorf("exactly %d watches must be accepted: %v", MaxWatchesPerSub, err)
	}
}

// TestLegacyTopicMigration is §2.1: a schema-1 file becomes watches with no
// loss of meaning, and the v1 file is preserved.
func TestLegacyTopicMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subs.json")
	v1 := `{"schema":1,"subs":[{"endpoint":"` + endpointA + `","p256dh":"cGsx","auth":"YXV0aA",
	  "topics":["rf_LON-TYO_rt_C","rf_LON-TYO_rt_F","rf_LON-TYO_ow_C","rf_NYC-LON_ow_M"]}]}`
	if err := os.WriteFile(path, []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}

	s := openStore(t, path)
	watches := s.Watches(endpointA)
	if len(watches) != 3 {
		t.Fatalf("got %d watches, want 3 (grouped by route+kind): %+v", len(watches), watches)
	}
	// Cabins are unioned per (route, kind); ranges stay unbounded; rt gets the
	// default nights window — which is exactly the old joint condition.
	want := map[string][]string{
		"LON-TYO|ow": {"C"},
		"LON-TYO|rt": {"C", "F"},
		"NYC-LON|ow": {"M"},
	}
	for _, w := range watches {
		key := w.Route + "|" + w.Kind
		if !slices.Equal(w.Cabins, want[key]) {
			t.Errorf("%s cabins = %v, want %v", key, w.Cabins, want[key])
		}
		if w.Out != nil || w.Ret != nil || w.Nights != nil {
			t.Errorf("%s must be unbounded, got %+v", key, w)
		}
		if !w.TopicRepresentable() {
			t.Errorf("%s must remain topic-representable", key)
		}
		if nmin, nmax := w.NightsWindow(); nmin != 1 || nmax != 30 {
			t.Errorf("%s nights = %d-%d, want the legacy 1-30", key, nmin, nmax)
		}
	}

	// §9-23: the v1 file is backed up, and the store rewrites itself as schema 2.
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(path + ".v1.bak")
	if err != nil {
		t.Fatalf("schema-1 backup missing: %v", err)
	}
	if !strings.Contains(string(backup), "rf_LON-TYO_rt_C") {
		t.Error("backup must hold the original topics")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file storeFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if file.Schema != Schema {
		t.Errorf("rewritten schema = %d, want %d", file.Schema, Schema)
	}
	if strings.Contains(string(raw), "topics") {
		t.Error("topics must not survive on disk")
	}

	// Reopening the migrated file is stable.
	s2 := openStore(t, path)
	if len(s2.Watches(endpointA)) != 3 {
		t.Error("migrated store must reload cleanly")
	}
}

// TestStaleClientCannotDeleteConstrainedWatch is §9-22 and the load-bearing
// rule of §2.2: a cached app.js posts the whole topic set it understands, and
// must not be able to destroy what it cannot express.
func TestStaleClientCannotDeleteConstrainedWatch(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "subs.json"))
	if _, err := s.Upsert(sub(endpointA, rt("LON-TYO", "C"), constrained())); err != nil {
		t.Fatal(err)
	}

	// GET /topics hides the constrained watch entirely.
	topics := s.Topics(endpointA)
	if !slices.Equal(topics, []string{"rf_LON-TYO_rt_C"}) {
		t.Fatalf("legacy projection = %v, want only the unbounded watch", topics)
	}

	// The stale client edits what it can see and saves the whole list.
	saved, err := s.UpsertTopics(sub(endpointA), []string{"rf_LON-TYO_rt_C", "rf_NYC-LON_ow_M"})
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 3 {
		t.Fatalf("saved %d watches, want 3 (2 legacy + the preserved constrained one)", len(saved))
	}

	var found bool
	for _, w := range s.Watches(endpointA) {
		if w.ID == mustNormalize(t, constrained()).ID {
			found = true
			if w.Out == nil || w.Out.To != "2026-10-20" || w.Nights.Max != 14 {
				t.Errorf("constrained watch was mangled: %+v", w)
			}
		}
	}
	if !found {
		t.Fatal("the date-constrained watch was DELETED by a legacy write")
	}
	// And the legacy write did replace the topic-representable ones.
	if got := s.Topics(endpointA); !slices.Equal(got, []string{"rf_LON-TYO_rt_C", "rf_NYC-LON_ow_M"}) {
		t.Errorf("legacy topics = %v", got)
	}
}

func mustNormalize(t *testing.T, w Watch) Watch {
	t.Helper()
	n, err := Normalize(w)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestExpiryAndPurge(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "subs.json"))
	today, err := ParseDay("2026-07-11")
	if err != nil {
		t.Fatal(err)
	}
	old := Watch{Route: "SFO-LON", Kind: KindOW, Cabins: []string{"C"},
		Out: &Range{From: "2026-03-03", To: "2026-03-10"}}
	if _, err := s.Upsert(sub(endpointA, rt("LON-TYO", "C"), old)); err != nil {
		t.Fatal(err)
	}
	if !mustNormalize(t, old).Expired(today) {
		t.Error("a past range must read as expired")
	}
	if mustNormalize(t, rt("LON-TYO", "C")).Expired(today) {
		t.Error("an unbounded watch never expires")
	}

	// Expired > 30 days ago -> purged; the live watch survives.
	watches, subs := s.PurgeExpired(today)
	if watches != 1 || subs != 0 {
		t.Errorf("purged %d watches / %d subs, want 1/0", watches, subs)
	}
	if got := s.Watches(endpointA); len(got) != 1 || got[0].Route != "LON-TYO" {
		t.Errorf("purge removed the wrong watch: %+v", got)
	}

	// A subscription left with nothing is removed entirely.
	if _, err := s.Upsert(sub(endpointB, old)); err != nil {
		t.Fatal(err)
	}
	if watches, subs := s.PurgeExpired(today); watches != 1 || subs != 1 {
		t.Errorf("purged %d/%d, want 1 watch and 1 subscription", watches, subs)
	}
	if _, ok := s.Lookup(endpointB); ok {
		t.Error("a subscription with nothing left to watch must be removed")
	}
}

func TestStatus(t *testing.T) {
	today, _ := ParseDay("2026-07-11")
	routes := map[string]bool{"LON-TYO": true, "TYO-LON": true, "ANU-SKB": true}

	cases := []struct {
		name string
		w    Watch
		want string
	}{
		{"active", rt("LON-TYO", "C"), StatusActive},
		{"unknown route", rt("XXX-YYY", "C"), StatusUnknownRoute},
		{"no reverse", rt("ANU-SKB", "C"), StatusNoReturn},
		{"expired", Watch{Route: "LON-TYO", Kind: KindOW, Cabins: []string{"C"},
			Out: &Range{From: "2026-03-01", To: "2026-03-10"}}, StatusExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mustNormalize(t, tc.w).Status(today, routes); got != tc.want {
				t.Errorf("status = %q, want %q", got, tc.want)
			}
		})
	}
	// A nil route set (no bundle yet) skips the data-dependent checks.
	if got := mustNormalize(t, rt("XXX-YYY", "C")).Status(today, nil); got != StatusActive {
		t.Errorf("without a bundle, status = %q, want active", got)
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "subs.json")
	s := openStore(t, path)
	if _, err := s.Upsert(sub(endpointA, constrained())); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Upsert(sub(endpointC, ow("LON-SIN", "F"))); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file left behind")
	}
	if info, err := os.Stat(path); err == nil && info.Mode().Perm() != 0o600 {
		t.Errorf("subscriptions must be owner-only, got %v", info.Mode().Perm())
	}

	s2 := openStore(t, path)
	if s2.Count() != 2 || s2.WatchCount() != 2 {
		t.Fatalf("after restart: %d subs / %d watches, want 2/2", s2.Count(), s2.WatchCount())
	}
	got := s2.Watches(endpointA)
	if len(got) != 1 || got[0].Out.To != "2026-10-20" || got[0].Nights.Min != 7 {
		t.Errorf("constrained watch did not survive: %+v", got)
	}
}

func TestVersionBumps(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "subs.json"))
	v0 := s.Version()
	if _, err := s.Upsert(sub(endpointA, rt("LON-TYO", "C"))); err != nil {
		t.Fatal(err)
	}
	v1 := s.Version()
	if v1 == v0 {
		t.Fatal("Upsert must bump the version (the watcher's index depends on it)")
	}
	s.MarkFired(endpointA, []string{s.Watches(endpointA)[0].ID}, 1)
	if s.Version() == v1 {
		t.Error("MarkFired must bump the version")
	}
	v2 := s.Version()
	s.Remove(endpointA)
	if s.Version() == v2 {
		t.Error("Remove must bump the version")
	}
	// A no-op remove does not.
	v3 := s.Version()
	s.Remove(endpointA)
	if s.Version() != v3 {
		t.Error("removing an unknown endpoint must not bump the version")
	}
}

func TestEndpointAllowlist(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "subs.json"))
	for _, bad := range []string{
		"http://fcm.googleapis.com/x",
		"https://evil.example.com/x",
		"https://fcm.googleapis.com.evil.com/x",
		"::nonsense::",
		"",
	} {
		if _, err := s.Upsert(sub(bad, rt("LON-TYO", "C"))); err == nil {
			t.Errorf("endpoint %q must be rejected", bad)
		}
	}
	for _, good := range []string{
		endpointB, endpointC,
		"https://push.services.mozilla.com/wpush/v1/x",
		"https://sg2p.notify.windows.com/w/?token=abc",
		"https://api.push.apple.com/3/device/x",
	} {
		if _, err := s.Upsert(sub(good, rt("LON-TYO", "C"))); err != nil {
			t.Errorf("allowlisted host %s rejected: %v", good, err)
		}
	}
}

func TestCorruptFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subs.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := openStore(t, path)
	if s.Count() != 0 {
		t.Errorf("corrupt file must start empty, got %d", s.Count())
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Error("corrupt file must be kept aside for forensics")
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "subs.json"))
	const workers, iterations = 8, 50

	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range iterations {
				endpoint := fmt.Sprintf("https://fcm.googleapis.com/fcm/send/%d-%d", w, i)
				if _, err := s.Upsert(sub(endpoint, rt("LON-TYO", "C"), constrained())); err != nil {
					t.Errorf("upsert: %v", err)
					return
				}
				s.Watches(endpoint)
				s.Topics(endpoint)
				s.Version()
				s.Count()
				s.MarkFired(endpoint, []string{"deadbeef"}, 1)
				if i%3 == 0 {
					s.Remove(endpoint)
				}
			}
		}(w)
	}
	// Readers, as the watcher's index rebuild would be.
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				for _, snapshot := range s.Snapshot() {
					for _, watch := range snapshot.Watches {
						if watch.Route == "" || len(watch.Cabins) == 0 {
							t.Error("snapshot read a torn watch")
							return
						}
					}
				}
			}
		}()
	}
	wg.Wait()
}

// TestFlushDuringMutationDoesNotDeadlock pins the lock order (mu, then writeMu).
//
// The mutating paths hold mu and take writeMu via touch→markDirty. An earlier
// Flush held writeMu and then waited on mu — an AB-BA inversion that wedged the
// whole watcher (not just alerts) the first time a debounced flush landed while
// someone was subscribing. Hammer both paths concurrently; a regression hangs
// here rather than failing subtly in production.
func TestFlushDuringMutationDoesNotDeadlock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := Open(Options{Path: filepath.Join(dir, "subs.json"), Debounce: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	done := make(chan struct{})
	go func() { // writers: each Upsert takes mu then writeMu
		defer close(done)
		for i := 0; i < 300; i++ {
			_, _ = s.Upsert(Subscription{
				Endpoint: fmt.Sprintf("https://fcm.googleapis.com/fcm/send/dl-%d", i),
				P256dh:   "p", Auth: "a",
				Watches: []Watch{{Route: "LON-TYO", Kind: "rt", Cabins: []string{"C"}}},
			})
		}
	}()
	go func() { // flushers: the debounce timer does exactly this
		for i := 0; i < 300; i++ {
			_ = s.Flush()
		}
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("deadlock: Upsert and Flush acquired mu/writeMu in opposite orders")
	}
}

// TestCloseFlushesPendingWrite guards the version-based dirty tracking: a
// mutation that is still sitting behind the debounce timer MUST reach disk on
// shutdown. Losing it loses a subscription, which the user cannot recover
// without re-subscribing from the browser.
func TestCloseFlushesPendingWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subs.json")
	s, err := Open(Options{
		Path: path, Debounce: time.Hour, // long enough that only Close can save us
		Now: func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Upsert(sub(endpointA, constrained())); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("the write should still be debounced at this point")
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Close must flush the pending write: %v", err)
	}
	if !strings.Contains(string(raw), endpointA) {
		t.Errorf("flushed file is missing the subscription: %s", raw)
	}
}

// TestFlushIsIdempotent pins the other half of the version comparison: with
// nothing changed, a flush must not rewrite the file (the debounced writer
// fires on a timer and would otherwise churn the disk every second).
func TestFlushIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subs.json")
	s := openStore(t, path)
	if _, err := s.Upsert(sub(endpointA, rt("LON-TYO", "C"))); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// Backdate the file: a genuine rewrite would reset the mtime.
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil { // nothing changed since the last write
		t.Fatal(err)
	}
	again, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !again.ModTime().Equal(past) {
		t.Error("an unchanged store must not be rewritten")
	}

	// ...but a real mutation does write again.
	s.MarkFired(endpointA, []string{s.Watches(endpointA)[0].ID}, 1783900000)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	final, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if final.ModTime().Equal(past) {
		t.Error("a mutation after the last write must be persisted")
	}
	_ = first
}

// TestByteCeilingProtectsTheDisk pins the store's hard size limit. It exists to
// protect the host: this store lives on a machine that also runs the data
// pipeline, so an unbounded subscription file could take the whole thing down.
// Growth past the ceiling is refused; shrinking out of it always works.
func TestByteCeilingProtectsTheDisk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := Open(Options{
		Path:     filepath.Join(dir, "subs.json"),
		MaxBytes: 1200, // tiny, so a handful of subscriptions reaches it
		Debounce: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	sub := func(i int) Subscription {
		return Subscription{
			Endpoint: fmt.Sprintf("https://fcm.googleapis.com/fcm/send/cap-%03d", i),
			P256dh:   "p256dh-value", Auth: "auth-value",
			Watches: []Watch{{Route: "LON-TYO", Kind: "rt", Cabins: []string{"C"}}},
		}
	}

	accepted := 0
	var lastErr error
	for i := 0; i < 50; i++ {
		if _, err := s.Upsert(sub(i)); err != nil {
			lastErr = err
			break
		}
		accepted++
	}
	if !errors.Is(lastErr, ErrStoreTooLarge) {
		t.Fatalf("expected the ceiling to stop growth, got %v after %d subs", lastErr, accepted)
	}
	if accepted == 0 {
		t.Fatal("ceiling rejected everything; it must admit what fits")
	}

	// The file it would write must actually be within the ceiling.
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "subs.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > 1200 {
		t.Fatalf("store file %d bytes exceeds the %d-byte ceiling", fi.Size(), 1200)
	}

	// A full store must not trap its users: removing frees room, and an existing
	// subscriber can still shrink (edit) even while full.
	s.Remove(sub(0).Endpoint)
	if _, err := s.Upsert(sub(accepted)); err != nil {
		t.Fatalf("after freeing a slot, a new subscription must fit again: %v", err)
	}
	shrink := sub(1)
	shrink.Watches = nil // strictly smaller: always allowed
	if _, err := s.Upsert(shrink); err != nil {
		t.Fatalf("shrinking a subscription must be allowed even at the ceiling: %v", err)
	}
}
