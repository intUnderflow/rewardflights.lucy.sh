package derive

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/source"
	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/warnings"
)

var testRegistry = map[string]AirlineInfo{
	"british-airways": {ID: "BA", Name: "British Airways", Cabins: map[string]string{
		"1": "Economy", "2": "Premium Economy", "4": "Business", "8": "First",
	}},
}

var testPlaces = map[string]Place{
	"ABZ": {Name: "Aberdeen", Country: "United Kingdom", G: []float64{57.202, -2.198}},
	"LON": {Name: "London", Country: "United Kingdom", G: []float64{51.507, -0.128}},
}

// testDate is one fixture date for buildDatasetEntries: cabinsAvailable
// letters plus optional per-flight detail.
type testDate struct {
	cabins  []string
	flights []source.Flight
}

// buildDataset runs Build over a single BA route with the given date->cabins
// map and returns the output, the parsed bundle, and the warning lines.
func buildDataset(t *testing.T, dates map[string][]string, sourceTime int64) (*Output, map[string]any, []string) {
	t.Helper()
	full := map[string]testDate{}
	for date, cabins := range dates {
		full[date] = testDate{cabins: cabins}
	}
	return buildDatasetEntries(t, full, sourceTime)
}

// buildDatasetEntries is buildDataset with full per-date control (flights).
func buildDatasetEntries(t *testing.T, dates map[string]testDate, sourceTime int64) (*Output, map[string]any, []string) {
	t.Helper()
	route := &source.Route{Dates: map[string]source.DateEntry{}}
	for date, td := range dates {
		route.Dates[date] = source.DateEntry{Cabins: td.cabins, Flights: td.flights, Path: "src/" + date + ".json"}
	}
	ds := &source.Dataset{Airlines: map[string]*source.Airline{
		"british-airways": {Slug: "british-airways", Routes: map[string]*source.Route{"ABZ-LON": route}},
	}}
	var buf bytes.Buffer
	out, err := Build(Inputs{
		Dataset: ds, Places: testPlaces, Airlines: testRegistry,
		SHA: "sha", SourceTime: sourceTime, Log: warnings.New(&buf),
	})
	if err != nil {
		t.Fatal(err)
	}
	var bundle map[string]any
	if err := json.Unmarshal(out.Files["availability.json"], &bundle); err != nil {
		t.Fatal(err)
	}
	var lines []string
	if s := strings.TrimRight(buf.String(), "\n"); s != "" {
		lines = strings.Split(s, "\n")
	}
	return out, bundle, lines
}

func bundleString(t *testing.T, bundle map[string]any, route string) string {
	t.Helper()
	entry := bundle["routes"].(map[string]any)[route].(map[string]any)
	return entry["a"].(map[string]any)["BA"].(string)
}

// seatBundleString returns the route's BA "s" seat-threshold string and
// whether the "s" key is present at all.
func seatBundleString(t *testing.T, bundle map[string]any, route string) (string, bool) {
	t.Helper()
	entry := bundle["routes"].(map[string]any)[route].(map[string]any)
	s, ok := entry["s"].(map[string]any)
	if !ok {
		return "", false
	}
	return s["BA"].(string), true
}

// seatPair extracts the 2-hex-char byte for day index idx of an "s" string.
func seatPair(t *testing.T, s string, idx int) string {
	t.Helper()
	if 2*idx+2 > len(s) {
		t.Fatalf("seat string too short (%d) for day index %d", len(s), idx)
	}
	return s[2*idx : 2*idx+2]
}

func TestFutureDateCap(t *testing.T) {
	cutoff := day(t, "2026-02-02")
	sourceTime := int64(cutoff)*86400 + 3600
	out, bundle, warns := buildDataset(t, map[string][]string{
		dayDate(cutoff):       {"M"}, // commit day itself: kept
		dayDate(cutoff + 450): {"C"}, // boundary: kept
		dayDate(cutoff + 451): {"W"}, // one past the cap: dropped
		"2126-07-12":          {"F"}, // a century out: dropped, must not bloat
	}, sourceTime)

	if out.RouteDates != 2 {
		t.Errorf("routeDates = %d, want 2 (future dates dropped)", out.RouteDates)
	}
	if want := []string{"WARN dropped-future-dates 2"}; len(warns) != 1 || warns[0] != want[0] {
		t.Errorf("warnings = %v, want %v", warns, want)
	}
	// days runs from Jan 1 of the cutoff year through cutoff+450 only.
	wantDays := cutoff + 450 - day(t, "2026-01-01") + 1
	if got := int(bundle["days"].(float64)); got != wantDays {
		t.Errorf("days = %d, want %d (horizon must not stretch to 2126)", got, wantDays)
	}
	s := bundleString(t, bundle, "ABZ-LON")
	if len(s) != wantDays {
		t.Fatalf("string length = %d, want %d", len(s), wantDays)
	}
	if s[cutoff-day(t, "2026-01-01")] != '1' || s[len(s)-1] != '4' {
		t.Errorf("kept dates misencoded: first=%q last=%q", s[cutoff-day(t, "2026-01-01")], s[len(s)-1])
	}
}

func TestSingleFarFutureFileDropped(t *testing.T) {
	// The reproduced defect: one corrupt far-future file next to normal data
	// must be dropped with a warning, not stretch the dataset horizon.
	cutoff := day(t, "2026-07-11")
	out, bundle, warns := buildDataset(t, map[string][]string{
		"2026-07-12": {"M", "C"},
		"2126-07-12": {"M"},
	}, int64(cutoff)*86400)

	if out.RouteDates != 1 {
		t.Errorf("routeDates = %d, want 1", out.RouteDates)
	}
	if len(warns) != 1 || warns[0] != "WARN dropped-future-dates 1" {
		t.Errorf("warnings = %v", warns)
	}
	wantDays := day(t, "2026-07-12") - day(t, "2026-01-01") + 1 // 193
	if got := int(bundle["days"].(float64)); got != wantDays {
		t.Errorf("days = %d, want %d", got, wantDays)
	}
	if raw := len(bundleString(t, bundle, "ABZ-LON")); raw != wantDays {
		t.Errorf("nibble string length %d, want %d", raw, wantDays)
	}
}

func TestEmptyCabinsSkipped(t *testing.T) {
	cutoff := day(t, "2026-02-02")
	out, bundle, warns := buildDataset(t, map[string][]string{
		"2026-03-01": {},         // explicitly empty
		"2026-03-02": {"Z"},      // all-unknown -> effectively empty
		"2026-03-03": {"M", "F"}, // real availability
	}, int64(cutoff)*86400)

	if out.RouteDates != 1 {
		t.Errorf("routeDates = %d, want 1 (empty-cabin dates skipped)", out.RouteDates)
	}
	want := []string{
		"WARN empty-cabins src/2026-03-01.json",
		"WARN unknown-cabin Z src/2026-03-02.json",
		"WARN empty-cabins src/2026-03-02.json",
	}
	if len(warns) != len(want) {
		t.Fatalf("warnings = %v, want %v", warns, want)
	}
	for i := range want {
		if warns[i] != want[i] {
			t.Errorf("warning[%d] = %q, want %q", i, warns[i], want[i])
		}
	}
	s := bundleString(t, bundle, "ABZ-LON")
	// Skipped dates encode as '0'; only 03-03 carries bits (M|F = 9).
	e := day(t, "2026-01-01")
	if s[day(t, "2026-03-01")-e] != '0' || s[day(t, "2026-03-02")-e] != '0' {
		t.Error("skipped empty-cabin dates must encode as '0'")
	}
	if s[day(t, "2026-03-03")-e] != '9' {
		t.Errorf("2026-03-03 nibble = %q, want '9'", s[day(t, "2026-03-03")-e])
	}
	// The skipped dates must not extend the horizon either: 03-03 is last.
	if got := int(bundle["days"].(float64)); got != day(t, "2026-03-03")-e+1 {
		t.Errorf("days = %d", got)
	}
}

// Coordinates: a curated "g" flows into the bundle's place table; a curated
// entry WITHOUT one warns (the map view would silently skip that place).
func TestPlaceCoordsEmittedAndMissingCoordsWarn(t *testing.T) {
	route := &source.Route{Dates: map[string]source.DateEntry{
		"2026-03-01": {Cabins: []string{"M"}, Path: "src/2026-03-01.json"},
	}}
	ds := &source.Dataset{Airlines: map[string]*source.Airline{
		"british-airways": {Slug: "british-airways", Routes: map[string]*source.Route{"ABZ-LON": route}},
	}}
	places := map[string]Place{
		"ABZ": {Name: "Aberdeen", Country: "United Kingdom", G: []float64{57.202, -2.198}},
		"LON": {Name: "London", Country: "United Kingdom"}, // no coords -> warn
	}
	var buf bytes.Buffer
	out, err := Build(Inputs{
		Dataset: ds, Places: places, Airlines: testRegistry,
		SHA: "sha", SourceTime: int64(day(t, "2026-03-01")) * 86400, Log: warnings.New(&buf),
	})
	if err != nil {
		t.Fatal(err)
	}
	var bundle map[string]any
	if err := json.Unmarshal(out.Files["availability.json"], &bundle); err != nil {
		t.Fatal(err)
	}
	pl := bundle["places"].(map[string]any)
	abz := pl["ABZ"].(map[string]any)
	g, ok := abz["g"].([]any)
	if !ok || len(g) != 2 || g[0].(float64) != 57.202 || g[1].(float64) != -2.198 {
		t.Errorf("ABZ g = %v, want [57.202 -2.198]", abz["g"])
	}
	if _, has := pl["LON"].(map[string]any)["g"]; has {
		t.Error("LON must not carry g when the curated entry has none")
	}
	if !strings.Contains(buf.String(), "place-missing-coords LON") {
		t.Errorf("warnings = %q, want place-missing-coords LON", buf.String())
	}
	if strings.Contains(buf.String(), "place-missing-coords ABZ") {
		t.Error("ABZ has coords and must not warn")
	}
}

// Threshold-code buckets at every boundary: 1 seat is NOT evidence of >=2
// (code 0), 2 -> 1, 3 -> 2, 4 -> 3, and anything above caps at 3.
func TestSeatBucketBoundaries(t *testing.T) {
	sourceTime := int64(day(t, "2026-02-02")) * 86400
	dates := map[string]testDate{}
	seatDays := map[string]int{
		"2026-03-01": 1, "2026-03-02": 2, "2026-03-03": 3,
		"2026-03-04": 4, "2026-03-05": 9,
	}
	for date, n := range seatDays {
		dates[date] = testDate{cabins: []string{"F"}, flights: []source.Flight{{Seats: map[string]int{"F": n}}}}
	}
	_, bundle, warns := buildDatasetEntries(t, dates, sourceTime)
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	s, ok := seatBundleString(t, bundle, "ABZ-LON")
	if !ok {
		t.Fatal("route with flight detail must carry an \"s\" key")
	}
	days := int(bundle["days"].(float64))
	if len(s) != 2*days {
		t.Fatalf("seat string length = %d, want 2*days = %d", len(s), 2*days)
	}
	e := day(t, "2026-01-01")
	// F codes live in bits 6-7: code<<6 -> bytes 00, 40, 80, C0, C0.
	want := map[string]string{
		"2026-03-01": "00", "2026-03-02": "40", "2026-03-03": "80",
		"2026-03-04": "C0", "2026-03-05": "C0",
	}
	for date, pair := range want {
		if got := seatPair(t, s, day(t, date)-e); got != pair {
			t.Errorf("%s (%d seats): seat byte = %q, want %q", date, seatDays[date], got, pair)
		}
	}
	// The presence layer is untouched: every fixture day still shows F.
	a := bundleString(t, bundle, "ABZ-LON")
	for date := range seatDays {
		if a[day(t, date)-e] != '8' {
			t.Errorf("%s: nibble = %q, want '8'", date, a[day(t, date)-e])
		}
	}
}

// A day's per-cabin value is the MAX across its flights — a party must fit
// on ONE flight — never the sum.
func TestSeatMaxAcrossFlightsNotSum(t *testing.T) {
	sourceTime := int64(day(t, "2026-02-02")) * 86400
	_, bundle, warns := buildDatasetEntries(t, map[string]testDate{
		"2026-03-01": {cabins: []string{"C"}, flights: []source.Flight{
			{Seats: map[string]int{"C": 2}},
			{Seats: map[string]int{"C": 2}},
			{Seats: map[string]int{"C": 1}},
		}},
	}, sourceTime)
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	s, ok := seatBundleString(t, bundle, "ABZ-LON")
	if !ok {
		t.Fatal("missing \"s\" key")
	}
	// MAX = 2 -> code 1 in bits 4-5 -> 0x10. A summed implementation would
	// see 5 seats and emit 0x30.
	if got := seatPair(t, s, day(t, "2026-03-01")-day(t, "2026-01-01")); got != "10" {
		t.Errorf("seat byte = %q, want \"10\" (MAX across flights, not SUM)", got)
	}
}

// Routes without any flight detail must not carry an "s" key at all — with
// 0% flights coverage the bundle is byte-identical to the pre-seats format.
func TestSeatKeyAbsentWithoutFlights(t *testing.T) {
	sourceTime := int64(day(t, "2026-02-02")) * 86400
	_, bundle, warns := buildDataset(t, map[string][]string{
		"2026-03-01": {"M", "F"},
		"2026-03-02": {"C"},
	}, sourceTime)
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	if _, ok := seatBundleString(t, bundle, "ABZ-LON"); ok {
		t.Error("route without flight detail must not carry an \"s\" key")
	}
}

// One flight-carrying day is enough to emit "s"; days without flight detail
// (and every other day in the window) encode as "00" = counts unknown.
func TestSeatPartialCoverage(t *testing.T) {
	sourceTime := int64(day(t, "2026-02-02")) * 86400
	_, bundle, warns := buildDatasetEntries(t, map[string]testDate{
		"2026-03-01": {cabins: []string{"M"}, flights: []source.Flight{{Seats: map[string]int{"M": 4}}}},
		"2026-03-02": {cabins: []string{"M"}},
	}, sourceTime)
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	s, ok := seatBundleString(t, bundle, "ABZ-LON")
	if !ok {
		t.Fatal("missing \"s\" key")
	}
	e := day(t, "2026-01-01")
	if got := seatPair(t, s, day(t, "2026-03-01")-e); got != "03" {
		t.Errorf("flight day byte = %q, want \"03\" (M code 3)", got)
	}
	if got := seatPair(t, s, day(t, "2026-03-02")-e); got != "00" {
		t.Errorf("cabins-only day byte = %q, want \"00\" (counts unknown)", got)
	}
	for i := 0; i < len(s); i += 2 {
		if d := e + i/2; d != day(t, "2026-03-01") && s[i:i+2] != "00" {
			t.Fatalf("day %s byte = %q, want \"00\"", dayDate(d), s[i:i+2])
		}
	}
}

// Contradictory source data: seats claimed for a cabin that isn't in
// cabinsAvailable resolve to the "a" bit (code 0) with a warning; an
// available cabin whose flights show no seats warns once per file; negative
// counts warn and are ignored everywhere.
func TestSeatContradictionWarnings(t *testing.T) {
	sourceTime := int64(day(t, "2026-02-02")) * 86400
	out, bundle, warns := buildDatasetEntries(t, map[string]testDate{
		// M:3 contradicts cabinsAvailable=[F]: M code stays 0, F encodes.
		"2026-03-01": {cabins: []string{"F"}, flights: []source.Flight{{Seats: map[string]int{"M": 3, "F": 2}}}},
		// C is available but no flight shows a C seat: counts unknown, warn.
		"2026-03-02": {cabins: []string{"M", "C"}, flights: []source.Flight{{Seats: map[string]int{"M": 2}}}},
		// Negative count: warned, dropped from flights/, never bucketed.
		"2026-03-03": {cabins: []string{"W"}, flights: []source.Flight{{Seats: map[string]int{"W": -2}}}},
	}, sourceTime)
	want := []string{
		"WARN seats-cabin-not-available M src/2026-03-01.json",
		"WARN available-cabin-zero-seats C src/2026-03-02.json",
		"WARN negative-seats W src/2026-03-03.json",
		"WARN available-cabin-zero-seats W src/2026-03-03.json",
	}
	if len(warns) != len(want) {
		t.Fatalf("warnings = %v, want %v", warns, want)
	}
	for i := range want {
		if warns[i] != want[i] {
			t.Errorf("warning[%d] = %q, want %q", i, warns[i], want[i])
		}
	}

	s, ok := seatBundleString(t, bundle, "ABZ-LON")
	if !ok {
		t.Fatal("missing \"s\" key")
	}
	e := day(t, "2026-01-01")
	for date, pair := range map[string]string{
		"2026-03-01": "40", // F code 1 only; contradicted M stays 0
		"2026-03-02": "01", // M code 1; available-but-unseen C stays 0
		"2026-03-03": "00", // negative ignored: counts unknown
	} {
		if got := seatPair(t, s, day(t, date)-e); got != pair {
			t.Errorf("%s: seat byte = %q, want %q", date, got, pair)
		}
	}

	// The negative count must not leak into the flights/ detail file either.
	var detail struct {
		Days map[string]map[string][]struct {
			Seats map[string]int `json:"seats"`
		} `json:"days"`
	}
	if err := json.Unmarshal(out.Files["flights/ABZ/LON/2026-03.json"], &detail); err != nil {
		t.Fatal(err)
	}
	if got := detail.Days["03"]["BA"][0].Seats; len(got) != 0 {
		t.Errorf("negative seat count leaked into flights detail: %v", got)
	}
}
