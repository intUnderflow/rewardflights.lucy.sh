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

// buildDataset runs Build over a single BA route with the given date->cabins
// map and returns the output, the parsed bundle, and the warning lines.
func buildDataset(t *testing.T, dates map[string][]string, sourceTime int64) (*Output, map[string]any, []string) {
	t.Helper()
	route := &source.Route{Dates: map[string]source.DateEntry{}}
	for date, cabins := range dates {
		route.Dates[date] = source.DateEntry{Cabins: cabins, Path: "src/" + date + ".json"}
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
