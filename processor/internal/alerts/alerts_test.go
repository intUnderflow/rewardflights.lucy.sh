package alerts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// t0 is 2026-01-01T00:00:00Z; the test epoch is 2026-01-01 throughout, so
// nibble index i = January 1 + i days.
const t0 = int64(20454 * 86400)

// rawBundle builds a minimal availability.json for the watcher.
func rawBundle(tb testing.TB, epoch string, unix int64, routes map[string]map[string]string, widths map[string]int) []byte {
	tb.Helper()
	airlines := map[string]any{}
	for route := range routes {
		for id := range routes[route] {
			airlines[id] = map[string]any{}
		}
	}
	for id, w := range widths {
		airlines[id] = map[string]any{"width": w}
	}
	routesJSON := map[string]any{}
	for route, a := range routes {
		routesJSON[route] = map[string]any{"a": a}
	}
	raw, err := json.Marshal(map[string]any{
		"epoch": epoch, "t": unix, "airlines": airlines, "routes": routesJSON,
	})
	if err != nil {
		tb.Fatal(err)
	}
	return raw
}

// ba wraps single-airline route strings.
func ba(routes map[string]string) map[string]map[string]string {
	out := map[string]map[string]string{}
	for r, s := range routes {
		out[r] = map[string]string{"BA": s}
	}
	return out
}

type capture struct {
	pubs []Publication
	fail bool
	logs []string
}

func (c *capture) publish(p Publication) error {
	if c.fail {
		return fmt.Errorf("injected failure")
	}
	c.pubs = append(c.pubs, p)
	return nil
}

func (c *capture) logf(format string, args ...any) {
	c.logs = append(c.logs, fmt.Sprintf(format, args...))
}

func newTestWatcher(t *testing.T, cap *capture, window int, statePath string) *Watcher {
	t.Helper()
	w, err := NewWatcher(Config{
		Window: window, StatePath: statePath,
		Publish: cap.publish, Logf: cap.logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func topics(pubs []Publication) []string {
	var out []string
	for _, p := range pubs {
		out = append(out, p.Topic)
	}
	return out
}

// TestTransitions is the table-driven core: baseline bundle -> next bundle,
// expected publications (topic -> body).
func TestTransitions(t *testing.T) {
	// All strings are 8 days long (epoch 2026-01-01, horizon 2026-01-09).
	cases := []struct {
		name     string
		window   int
		baseline map[string]map[string]string
		widths   map[string]int
		next     map[string]map[string]string
		want     map[string]string // topic -> exact body
	}{
		{
			name:     "one-way open, no reverse route",
			window:   30,
			baseline: ba(map[string]string{"LON-TYO": "00000000"}),
			next:     ba(map[string]string{"LON-TYO": "00400000"}),
			want: map[string]string{
				"rf_LON-TYO_ow_C": "1 new date: Sat 3 Jan",
			},
		},
		{
			name:   "round trip joins when the return leg opens",
			window: 30,
			baseline: ba(map[string]string{
				"LON-TYO": "04000000", "TYO-LON": "00000000",
			}),
			next: ba(map[string]string{
				"LON-TYO": "04000000", "TYO-LON": "00040000",
			}),
			want: map[string]string{
				"rf_LON-TYO_rt_C": "1 new date: Fri 2 Jan", // outbound day joins the rt set
				"rf_TYO-LON_ow_C": "1 new date: Sun 4 Jan", // the return day is a new one-way
			},
		},
		{
			name:   "window edge: return at d+window qualifies",
			window: 3,
			baseline: ba(map[string]string{
				"LON-TYO": "00000000", "TYO-LON": "00002000",
			}),
			next: ba(map[string]string{
				"LON-TYO": "02000000", "TYO-LON": "00002000",
			}),
			want: map[string]string{
				"rf_LON-TYO_ow_W": "1 new date: Fri 2 Jan",
				"rf_LON-TYO_rt_W": "1 new date: Fri 2 Jan", // return Jan 5 = d+3 = d+window
			},
		},
		{
			name:   "window edge: return at d+window+1 does not qualify",
			window: 3,
			baseline: ba(map[string]string{
				"LON-TYO": "00000000", "TYO-LON": "00000200",
			}),
			next: ba(map[string]string{
				"LON-TYO": "02000000", "TYO-LON": "00000200",
			}),
			want: map[string]string{
				"rf_LON-TYO_ow_W": "1 new date: Fri 2 Jan", // rt absent: Jan 6 is d+4
			},
		},
		{
			name:   "same-cabin joint condition: mismatched cabins never pair",
			window: 30,
			baseline: ba(map[string]string{
				"LON-TYO": "00000000", "TYO-LON": "00020000",
			}),
			next: ba(map[string]string{
				"LON-TYO": "04000000", "TYO-LON": "00020000", // out C, back W only
			}),
			want: map[string]string{
				"rf_LON-TYO_ow_C": "1 new date: Fri 2 Jan",
			},
		},
		{
			name:     "horizon growth is not an open",
			window:   30,
			baseline: ba(map[string]string{"LON-TYO": "00000008"}),
			next:     ba(map[string]string{"LON-TYO": "000000088"}), // day 9 beyond old horizon
			want:     map[string]string{},
		},
		{
			name:     "mid-horizon open still alerts when the horizon also grows",
			window:   30,
			baseline: ba(map[string]string{"LON-TYO": "00000008"}),
			next:     ba(map[string]string{"LON-TYO": "008000088"}),
			want: map[string]string{
				"rf_LON-TYO_ow_F": "1 new date: Sat 3 Jan",
			},
		},
		{
			name:   "width != 1 airline is skipped entirely",
			window: 30,
			baseline: map[string]map[string]string{
				"LON-TYO": {"XX": "00000000"},
			},
			widths: map[string]int{"XX": 2},
			next: map[string]map[string]string{
				"LON-TYO": {"XX": "04400000"},
			},
			want: map[string]string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cap := &capture{}
			w := newTestWatcher(t, cap, tc.window, "")
			w.Baseline(rawBundle(t, "2026-01-01", t0, tc.baseline, tc.widths))
			w.Cycle(rawBundle(t, "2026-01-01", t0+1800, tc.next, tc.widths))

			got := map[string]string{}
			for _, p := range cap.pubs {
				got[p.Topic] = p.Body
			}
			if len(got) != len(tc.want) {
				t.Fatalf("published %v, want topics %v", got, tc.want)
			}
			for topic, body := range tc.want {
				if got[topic] != body {
					t.Errorf("%s body = %q, want %q", topic, got[topic], body)
				}
			}
		})
	}
}

func TestTitlesAndURLs(t *testing.T) {
	cap := &capture{}
	w := newTestWatcher(t, cap, 30, "")
	w.Baseline(rawBundle(t, "2026-01-01", t0, ba(map[string]string{
		"LON-TYO": "00000000", "TYO-LON": "04000000",
	}), nil))
	w.Cycle(rawBundle(t, "2026-01-01", t0+1800, ba(map[string]string{
		"LON-TYO": "00400000", "TYO-LON": "04000000",
	}), nil))

	want := map[string][2]string{
		"rf_LON-TYO_ow_C": {"Business seats open: LON → TYO", "https://rewardflights.lucy.sh/route/LON-TYO"},
		// TYO-LON out Jan 2 now has a C return Jan 3 on LON-TYO.
		"rf_TYO-LON_rt_C": {"Business round trip open: TYO ⇄ LON", "https://rewardflights.lucy.sh/trip/TYO-LON"},
	}
	if len(cap.pubs) != len(want) {
		t.Fatalf("pubs = %v", topics(cap.pubs))
	}
	for _, p := range cap.pubs {
		exp, ok := want[p.Topic]
		if !ok {
			t.Errorf("unexpected topic %s", p.Topic)
			continue
		}
		if p.Title != exp[0] || p.URL != exp[1] {
			t.Errorf("%s: title %q url %q, want %q %q", p.Topic, p.Title, p.URL, exp[0], exp[1])
		}
	}
}

func TestBodyFormatCapsAtSix(t *testing.T) {
	cap := &capture{}
	w := newTestWatcher(t, cap, 30, "")
	w.Baseline(rawBundle(t, "2026-01-01", t0, ba(map[string]string{"LON-TYO": "0000000000"}), nil))
	w.Cycle(rawBundle(t, "2026-01-01", t0+1800, ba(map[string]string{"LON-TYO": "0111111110"}), nil))
	if len(cap.pubs) != 1 {
		t.Fatalf("pubs = %v", topics(cap.pubs))
	}
	want := "8 new dates: Fri 2 Jan, Sat 3 Jan, Sun 4 Jan, Mon 5 Jan, Tue 6 Jan, Wed 7 Jan, +2 more"
	if cap.pubs[0].Body != want {
		t.Errorf("body = %q, want %q", cap.pubs[0].Body, want)
	}
}

func TestBaselineStartNoAlerts(t *testing.T) {
	cap := &capture{}
	w := newTestWatcher(t, cap, 30, "")
	full := rawBundle(t, "2026-01-01", t0, ba(map[string]string{
		"LON-TYO": "0F4F0000", "TYO-LON": "00F4F000",
	}), nil)
	w.Baseline(full)
	if len(cap.pubs) != 0 {
		t.Fatalf("baseline published %v", topics(cap.pubs))
	}
	// A no-change cycle stays silent too.
	w.Cycle(rawBundle(t, "2026-01-01", t0+1800, ba(map[string]string{
		"LON-TYO": "0F4F0000", "TYO-LON": "00F4F000",
	}), nil))
	if len(cap.pubs) != 0 {
		t.Fatalf("no-change cycle published %v", topics(cap.pubs))
	}
}

func TestCooldownFlap(t *testing.T) {
	cap := &capture{}
	w := newTestWatcher(t, cap, 30, "")
	off := ba(map[string]string{"LON-TYO": "00000000"})
	on := ba(map[string]string{"LON-TYO": "01000000"})

	w.Baseline(rawBundle(t, "2026-01-01", t0, off, nil))
	w.Cycle(rawBundle(t, "2026-01-01", t0+600, on, nil)) // opens: alert #1
	if len(cap.pubs) != 1 {
		t.Fatalf("first open: pubs = %v", topics(cap.pubs))
	}
	w.Cycle(rawBundle(t, "2026-01-01", t0+1800, off, nil))      // closes
	w.Cycle(rawBundle(t, "2026-01-01", t0+600+2*3600, on, nil)) // reopens 2h after last-on: suppressed
	if len(cap.pubs) != 1 {
		t.Fatalf("flap within cooldown must not re-alert: %v", topics(cap.pubs))
	}
	w.Cycle(rawBundle(t, "2026-01-01", t0+600+3*3600, off, nil))       // closes again
	w.Cycle(rawBundle(t, "2026-01-01", t0+600+2*3600+4*3600, on, nil)) // 4h after last-on: alerts
	if len(cap.pubs) != 2 {
		t.Fatalf("re-open after cooldown must alert: %v", topics(cap.pubs))
	}
}

func TestBatchingMergesAndSurvivesRestart(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "alerts.json")
	cap := &capture{}
	w := newTestWatcher(t, cap, 30, statePath)

	w.Baseline(rawBundle(t, "2026-01-01", t0, ba(map[string]string{"LON-TYO": "00000000"}), nil))
	// Cycle 1: day 2 opens -> immediate publish (no prior lastPub).
	w.Cycle(rawBundle(t, "2026-01-01", t0+60, ba(map[string]string{"LON-TYO": "01000000"}), nil))
	// Cycles 2+3 land inside the 1h batch window: day 3 then day 4 open.
	w.Cycle(rawBundle(t, "2026-01-01", t0+600, ba(map[string]string{"LON-TYO": "01100000"}), nil))
	w.Cycle(rawBundle(t, "2026-01-01", t0+1200, ba(map[string]string{"LON-TYO": "01110000"}), nil))
	if len(cap.pubs) != 1 {
		t.Fatalf("batching: pubs = %v", topics(cap.pubs))
	}
	if cap.pubs[0].Body != "1 new date: Fri 2 Jan" {
		t.Errorf("first publish body = %q", cap.pubs[0].Body)
	}

	// Simulated restart: fresh watcher, same state file, same current bundle.
	cap2 := &capture{}
	w2 := newTestWatcher(t, cap2, 30, statePath)
	w2.Baseline(rawBundle(t, "2026-01-01", t0+1200, ba(map[string]string{"LON-TYO": "01110000"}), nil))
	if len(cap2.pubs) != 0 {
		t.Fatalf("restart baseline must not publish inside the batch window: %v", topics(cap2.pubs))
	}
	// Next cycle after the batch interval: ONE publish with both merged dates.
	w2.Cycle(rawBundle(t, "2026-01-01", t0+60+3700, ba(map[string]string{"LON-TYO": "01110000"}), nil))
	if len(cap2.pubs) != 1 {
		t.Fatalf("merged flush: pubs = %v", topics(cap2.pubs))
	}
	if want := "2 new dates: Sat 3 Jan, Sun 4 Jan"; cap2.pubs[0].Body != want {
		t.Errorf("merged body = %q, want %q", cap2.pubs[0].Body, want)
	}
}

func TestEpochShiftDoesNotRealert(t *testing.T) {
	cap := &capture{}
	w := newTestWatcher(t, cap, 30, "")

	// Late-December generation: epoch 2026-01-01, 2027-01-10 (index 374) is
	// on, horizon index 379 (2027-01-15). "Today" is 2026-12-30.
	tDec := t0 + int64(363*86400)
	oldStr := strings.Repeat("0", 374) + "4" + strings.Repeat("0", 5)
	w.Baseline(rawBundle(t, "2026-01-01", tDec, ba(map[string]string{"LON-TYO": oldStr}), nil))

	// Year rolled over: epoch shifts to 2027-01-01; the SAME absolute date
	// 2027-01-10 is now index 9. Also 2027-01-12 (index 11) newly opens —
	// proving the diff is live while the unchanged date stays silent.
	newStr := "000000000404000"
	w.Cycle(rawBundle(t, "2027-01-01", tDec+1800, ba(map[string]string{"LON-TYO": newStr}), nil))

	if len(cap.pubs) != 1 {
		t.Fatalf("pubs = %v (want exactly the genuinely-new date)", topics(cap.pubs))
	}
	if want := "1 new date: Tue 12 Jan"; cap.pubs[0].Body != want {
		t.Errorf("body = %q, want %q", cap.pubs[0].Body, want)
	}
}

func TestPublishFailureKeepsPendingAndLastOnAdvances(t *testing.T) {
	cap := &capture{fail: true}
	w := newTestWatcher(t, cap, 30, "")
	w.Baseline(rawBundle(t, "2026-01-01", t0, ba(map[string]string{"LON-TYO": "00000000"}), nil))
	w.Cycle(rawBundle(t, "2026-01-01", t0+600, ba(map[string]string{"LON-TYO": "04000000"}), nil))
	if len(cap.pubs) != 0 {
		t.Fatal("failed publish must not record a publication")
	}
	if len(cap.logs) == 0 || !strings.Contains(cap.logs[0], "WARN alert-publish-failed") {
		t.Fatalf("logs = %v", cap.logs)
	}
	// Delivery recovers: the SAME pending date goes out exactly once, even
	// though the day never transitioned again.
	cap.fail = false
	w.Cycle(rawBundle(t, "2026-01-01", t0+1200, ba(map[string]string{"LON-TYO": "04000000"}), nil))
	if len(cap.pubs) != 1 || cap.pubs[0].Body != "1 new date: Fri 2 Jan" {
		t.Fatalf("recovery pubs = %+v", cap.pubs)
	}
	// And last-on advanced through the failure: a flap right after does not
	// queue a duplicate.
	w.Cycle(rawBundle(t, "2026-01-01", t0+1800, ba(map[string]string{"LON-TYO": "00000000"}), nil))
	w.Cycle(rawBundle(t, "2026-01-01", t0+2400, ba(map[string]string{"LON-TYO": "04000000"}), nil))
	if len(cap.pubs) != 1 {
		t.Fatalf("flap after failure alerted again: %v", topics(cap.pubs))
	}
}

func TestCorruptStateStartsFresh(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "alerts.json")
	if err := os.WriteFile(statePath, []byte("{corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	cap := &capture{}
	w := newTestWatcher(t, cap, 30, statePath)
	w.Baseline(rawBundle(t, "2026-01-01", t0, ba(map[string]string{"LON-TYO": "00000000"}), nil))
	if len(cap.pubs) != 0 {
		t.Fatal("fresh start must not alert on baseline")
	}
	w.Cycle(rawBundle(t, "2026-01-01", t0+600, ba(map[string]string{"LON-TYO": "08000000"}), nil))
	if len(cap.pubs) != 1 {
		t.Fatalf("post-baseline transition must alert: %v", topics(cap.pubs))
	}
	// State was rewritten as valid JSON.
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var s stateData
	if err := json.Unmarshal(raw, &s); err != nil || s.Schema != 1 {
		t.Fatalf("state file not repaired: %s (%v)", raw, err)
	}
}

func TestLastOnPruning(t *testing.T) {
	cap := &capture{}
	w := newTestWatcher(t, cap, 30, "")
	w.Baseline(rawBundle(t, "2026-01-01", t0, ba(map[string]string{"LON-TYO": "10000000"}), nil))
	if len(w.state.LastOn) == 0 {
		t.Fatal("baseline must seed lastOn")
	}
	// 20 days later the Jan-1 entry is >7 days past its date: pruned.
	w.Cycle(rawBundle(t, "2026-01-01", t0+20*86400, ba(map[string]string{"LON-TYO": "10000000000000000000000"}), nil))
	for k := range w.state.LastOn {
		if strings.HasSuffix(k, "2026-01-01") {
			t.Errorf("stale lastOn entry survived: %s", k)
		}
	}
}

func TestDryRunRealBundles(t *testing.T) {
	oldPath, newPath := os.Getenv("RF_DRYRUN_OLD"), os.Getenv("RF_DRYRUN_NEW")
	if oldPath == "" || newPath == "" {
		t.Skip("set RF_DRYRUN_OLD / RF_DRYRUN_NEW to bundle files")
	}
	oldRaw, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	newRaw, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatal(err)
	}
	cap := &capture{}
	w := newTestWatcher(t, cap, 30, "")
	w.Baseline(oldRaw)
	w.Cycle(newRaw)
	fmt.Printf("dry-run: %d publication(s)\n", len(cap.pubs))
	for _, p := range cap.pubs {
		fmt.Printf("  %s\n    title: %s\n    body:  %s\n    url:   %s\n", p.Topic, p.Title, p.Body, p.URL)
	}
}
