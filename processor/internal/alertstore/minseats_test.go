package alertstore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// preFeatureWatchID replicates the id formula EXACTLY as it stood before
// MinSeats existed (route|kind|cabins|out|ret|nights|[L<lead>]). It is the
// oracle for TestMinSeatsIDStability: everything keyed off a watch id —
// carryHistory (createdAt/lastFiredAt), MarkFired, pending alert items, and
// the client's rf:seen:v1 baselines — survives the deploy only if a
// MinSeats-less watch still hashes to this.
func preFeatureWatchID(w Watch) string {
	var b strings.Builder
	b.WriteString(w.Route)
	b.WriteByte('|')
	b.WriteString(w.Kind)
	b.WriteByte('|')
	b.WriteString(strings.Join(w.Cabins, ""))
	b.WriteByte('|')
	b.WriteString(rangeKey(w.Out))
	b.WriteByte('|')
	b.WriteString(rangeKey(w.Ret))
	b.WriteByte('|')
	if w.Nights != nil {
		fmt.Fprintf(&b, "%d-%d", w.Nights.Min, w.Nights.Max)
	}
	b.WriteByte('|')
	if w.LeadDays > 0 {
		fmt.Fprintf(&b, "L%d", w.LeadDays)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:8]
}

func TestMinSeatsIDStability(t *testing.T) {
	lead := rt("LON-TYO", "C")
	lead.LeadDays = 14
	// Literal ids, computed from the pre-feature formula and pinned here so
	// the oracle itself cannot drift along with the code under test.
	pinned := []struct {
		name string
		w    Watch
		id   string
	}{
		{"unbounded rt", rt("LON-TYO", "C"), "220c73da"},
		{"constrained rt", constrained(), "0e796494"},
		{"leadDays (the conditional-fold precedent)", lead, "580c2309"},
		{"one-way multi-cabin", ow("NYC-LON", "M", "F"), "d5d85bc6"},
	}
	for _, tc := range pinned {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Normalize(tc.w)
			if err != nil {
				t.Fatal(err)
			}
			if got.ID != tc.id {
				t.Errorf("id = %s, want the pre-feature id %s — a stored watch would be orphaned", got.ID, tc.id)
			}
			if oracle := preFeatureWatchID(got); got.ID != oracle {
				t.Errorf("id = %s, oracle = %s", got.ID, oracle)
			}
		})
	}

	// A party constraint DOES mint a new id (editing the party size is an
	// edit), and each threshold is distinct.
	base, _ := Normalize(rt("LON-TYO", "C"))
	seen := map[string]string{base.ID: "no constraint"}
	for n := 2; n <= MaxMinSeats; n++ {
		w := rt("LON-TYO", "C")
		w.MinSeats = n
		got, err := Normalize(w)
		if err != nil {
			t.Fatalf("minSeats %d: %v", n, err)
		}
		if prev, dup := seen[got.ID]; dup {
			t.Errorf("minSeats %d collides with %s", n, prev)
		}
		seen[got.ID] = fmt.Sprintf("minSeats %d", n)
	}
}

func TestMinSeatsNormalize(t *testing.T) {
	rejects := []struct {
		minSeats int
		want     string
	}{
		{-1, "negative"},
		{1, "default"}, // one spelling of "one passenger": the zero value
		{MaxMinSeats + 1, "between 2 and 4"},
	}
	for _, tc := range rejects {
		w := rt("LON-TYO", "C")
		w.MinSeats = tc.minSeats
		if _, err := Normalize(w); err == nil {
			t.Errorf("minSeats %d must be rejected", tc.minSeats)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("minSeats %d error %q must mention %q", tc.minSeats, err, tc.want)
		}
	}
	for _, n := range []int{0, 2, 3, MaxMinSeats} {
		for _, w := range []Watch{rt("LON-TYO", "C"), ow("LON-TYO", "C"), constrained()} {
			w.MinSeats = n
			got, err := Normalize(w)
			if err != nil {
				t.Errorf("minSeats %d on %s must be accepted: %v", n, w.Kind, err)
				continue
			}
			if got.MinSeats != n {
				t.Errorf("minSeats %d must be preserved, got %d", n, got.MinSeats)
			}
		}
	}
}

// TestMinSeatsInvisibleToLegacyClients: a stale cached app.js speaks only
// topics; UpsertTopics REPLACES every topic-representable watch. A party
// watch must be outside that blast radius (§2.2's protection, extended).
func TestMinSeatsInvisibleToLegacyClients(t *testing.T) {
	w := rt("LON-TYO", "C")
	w.MinSeats = 3
	if w.TopicRepresentable() {
		t.Fatal("a party watch must not be topic-representable")
	}

	s := openStore(t, filepath.Join(t.TempDir(), "subs.json"))
	if _, err := s.Upsert(sub(endpointA, rt("LON-TYO", "C"), w)); err != nil {
		t.Fatal(err)
	}
	// The stale client sees only the unconstrained watch...
	if topics := s.Topics(endpointA); len(topics) != 1 {
		t.Fatalf("legacy projection = %v, want just the unconstrained watch", topics)
	}
	// ...and its whole-list topic write cannot destroy the party watch.
	if _, err := s.UpsertTopics(Subscription{Endpoint: endpointA, P256dh: "cGsx", Auth: "YXV0aA"},
		[]string{"rf_NYC-LON_ow_M"}); err != nil {
		t.Fatal(err)
	}
	var survived bool
	for _, got := range s.Watches(endpointA) {
		if got.MinSeats == 3 && got.Route == "LON-TYO" {
			survived = true
		}
	}
	if !survived {
		t.Fatal("a stale client's topic write DESTROYED a party watch")
	}
}

// TestPreFeatureFileLoads: a subscriptions file written before MinSeats
// existed must reload bit-identically — same watches, same ids, same
// history. Normalize accepting the zero value is what this rests on.
func TestPreFeatureFileLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subs.json")
	s := openStore(t, path)
	saved, err := s.Upsert(sub(endpointA, rt("LON-TYO", "C"), constrained()))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// The file on disk has no minSeats key anywhere (omitempty); reopening
	// re-runs Normalize on every stored watch.
	re := openStore(t, path)
	got := re.Watches(endpointA)
	if len(got) != len(saved) {
		t.Fatalf("reloaded %d watches, want %d", len(got), len(saved))
	}
	want := map[string]bool{}
	for _, w := range saved {
		want[w.ID] = true
	}
	for _, w := range got {
		if !want[w.ID] {
			t.Errorf("id %s changed across the deploy boundary", w.ID)
		}
		if w.MinSeats != 0 {
			t.Errorf("a pre-feature watch must load with MinSeats 0, got %d", w.MinSeats)
		}
		if w.CreatedAt == 0 {
			t.Error("createdAt lost on reload")
		}
	}
}
