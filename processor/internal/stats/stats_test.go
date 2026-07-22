package stats

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// bundle builds availability.json bytes: route -> one merged hex string.
func bundle(t *testing.T, routes map[string]string) []byte {
	t.Helper()
	wrapped := map[string]any{}
	for r, s := range routes {
		wrapped[r] = map[string]any{"a": map[string]string{"BA": s}}
	}
	raw, err := json.Marshal(map[string]any{"epoch": "2026-01-01", "routes": wrapped})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

var epochUnix = func() int64 {
	tt, _ := time.Parse("2006-01-02", "2026-01-01")
	return tt.Unix()
}()

func aggOf(a *Accumulator, key string) Agg {
	if g := a.state.Agg[key]; g != nil {
		return *g
	}
	return Agg{}
}

func TestBaselineSeedsSilently(t *testing.T) {
	a := New("", nil)
	// First cycle: no old bundle. Nothing counts, runs seed with unknown start.
	a.Cycle(nil, bundle(t, map[string]string{"AAA-BBB": "088"}), epochUnix)
	if g := aggOf(a, "AAA-BBB|F"); g.Total != 0 {
		t.Fatalf("baseline counted appearances: %+v", g)
	}
	// The baseline-seeded run closes 2h later: no survival is fabricated.
	a.Cycle(bundle(t, map[string]string{"AAA-BBB": "088"}),
		bundle(t, map[string]string{"AAA-BBB": "080"}), epochUnix+7200)
	if g := aggOf(a, "AAA-BBB|F"); g.Done != 0 {
		t.Fatalf("baseline close measured a lifetime: %+v", g)
	}
}

func TestAppearanceAndSurvival(t *testing.T) {
	a := New("", nil)
	a.Cycle(nil, bundle(t, map[string]string{"AAA-BBB": "000"}), epochUnix)
	// Day 1 (2026-01-02) gains F within the horizon: one appearance, January.
	a.Cycle(bundle(t, map[string]string{"AAA-BBB": "000"}),
		bundle(t, map[string]string{"AAA-BBB": "080"}), epochUnix+1000)
	g := aggOf(a, "AAA-BBB|F")
	if g.Total != 1 || g.Months[0] != 1 {
		t.Fatalf("appearance not counted: %+v", g)
	}
	// Gone 2h later: survival lands in the 1-6h bucket.
	a.Cycle(bundle(t, map[string]string{"AAA-BBB": "080"}),
		bundle(t, map[string]string{"AAA-BBB": "000"}), epochUnix+1000+2*3600)
	g = aggOf(a, "AAA-BBB|F")
	if g.Done != 1 || g.Surv != [5]int{0, 1, 0, 0, 0} {
		t.Fatalf("survival not bucketed: %+v", g)
	}
	// Reopens: a second appearance for the same date.
	a.Cycle(bundle(t, map[string]string{"AAA-BBB": "000"}),
		bundle(t, map[string]string{"AAA-BBB": "080"}), epochUnix+30000)
	if g := aggOf(a, "AAA-BBB|F"); g.Total != 2 {
		t.Fatalf("reopen not counted: %+v", g)
	}
}

func TestFrontierIsNotAnAppearanceButRunsSurvival(t *testing.T) {
	a := New("", nil)
	a.Cycle(nil, bundle(t, map[string]string{"AAA-BBB": "08"}), epochUnix)
	// The horizon grows by one day that arrives already holding F.
	a.Cycle(bundle(t, map[string]string{"AAA-BBB": "08"}),
		bundle(t, map[string]string{"AAA-BBB": "088"}), epochUnix+1000)
	if g := aggOf(a, "AAA-BBB|F"); g.Total != 0 {
		t.Fatalf("frontier counted as appearance: %+v", g)
	}
	// Snapped 30 minutes after release: survival <1h — release-snipe speed.
	a.Cycle(bundle(t, map[string]string{"AAA-BBB": "088"}),
		bundle(t, map[string]string{"AAA-BBB": "080"}), epochUnix+1000+1800)
	if g := aggOf(a, "AAA-BBB|F"); g.Done != 1 || g.Surv[0] != 1 {
		t.Fatalf("release snipe not measured: %+v", g)
	}
}

func TestExpiredDatesAreCensored(t *testing.T) {
	a := New("", nil)
	a.Cycle(nil, bundle(t, map[string]string{"AAA-BBB": "000"}), epochUnix)
	// Day 0 (2026-01-01) gains F...
	a.Cycle(bundle(t, map[string]string{"AAA-BBB": "000"}),
		bundle(t, map[string]string{"AAA-BBB": "800"}), epochUnix+1000)
	if g := aggOf(a, "AAA-BBB|F"); g.Total != 1 {
		t.Fatalf("setup: %+v", g)
	}
	// ...then the clock rolls past it while it is still open (and the
	// generator zeroes the past day). Not snapped up: no survival run.
	a.Cycle(bundle(t, map[string]string{"AAA-BBB": "800"}),
		bundle(t, map[string]string{"AAA-BBB": "000"}), epochUnix+86400+1000)
	if g := aggOf(a, "AAA-BBB|F"); g.Done != 0 {
		t.Fatalf("expired run counted as snapped up: %+v", g)
	}
	if len(a.state.Open) != 0 {
		t.Fatalf("expired run not censored: %v", a.state.Open)
	}
}

func TestRenderAndEmitGate(t *testing.T) {
	dir := t.TempDir()
	a := New(filepath.Join(dir, "state.json"), nil)
	a.Cycle(nil, bundle(t, map[string]string{"AAA-BBB": "000"}), epochUnix)
	a.Cycle(bundle(t, map[string]string{"AAA-BBB": "000"}),
		bundle(t, map[string]string{"AAA-BBB": "088"}), epochUnix+1000)

	// One week of observation, 2 appearances -> w = 2.0.
	raw, err := a.Render(epochUnix + 7*86400)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Routes map[string]map[string]struct {
			W float64 `json:"w"`
			N int     `json:"n"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	f := got.Routes["AAA-BBB"]["F"]
	if f.N != 2 || f.W != 2.0 {
		t.Fatalf("render = %+v (raw %s)", f, raw)
	}

	// Emission gate: first write goes, a second within the hour does not.
	if !a.EmitIfDue(dir, epochUnix+7*86400) {
		t.Fatal("first emit suppressed")
	}
	if a.EmitIfDue(dir, epochUnix+7*86400+1800) {
		t.Fatal("second emit inside the hour not suppressed")
	}
	if a.EmitIfDue(dir, epochUnix+7*86400+3700) != true {
		t.Fatal("emit after the hour suppressed")
	}

	// Persistence: a fresh accumulator on the same path keeps the aggregates.
	b := New(filepath.Join(dir, "state.json"), nil)
	if g := aggOf(b, "AAA-BBB|F"); g.Total != 2 {
		t.Fatalf("state not persisted: %+v", g)
	}
	// Determinism: identical render twice.
	r1, _ := a.Render(epochUnix + 8*86400)
	r2, _ := a.Render(epochUnix + 8*86400)
	if string(r1) != string(r2) {
		t.Fatal("non-deterministic render")
	}
}
