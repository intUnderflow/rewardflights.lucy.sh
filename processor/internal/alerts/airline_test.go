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
	for route, byAl := range routes {
		a := map[string]string{}
		for al, avail := range byAl {
			als[al] = map[string]any{}
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
