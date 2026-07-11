package app

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "regenerate golden files")

const (
	shaA  = "0123456789abcdef0123456789abcdef01234567"
	timeA = 1770000000 // 2026-02-02T02:40:00Z -> cutoff date 2026-02-02
	shaB  = "89abcdef0123456789abcdef0123456789abcdef"
	timeB = 1770000100
)

func runFixture(t *testing.T, src, out, sha string, unix int64, force bool) (*Result, string, error) {
	t.Helper()
	var stderr bytes.Buffer
	res, err := Run(Config{
		Src: src, Out: out, SourceSHA: sha, SourceTime: unix,
		Force: force, Warnings: &stderr,
	})
	return res, stderr.String(), err
}

// treeFiles reads every regular file under root as relative slash path -> bytes.
func treeFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestGoldenEndToEnd(t *testing.T) {
	out := t.TempDir()
	res, stderr, err := runFixture(t, "testdata/src_a", out, shaA, timeA, false)
	if err != nil {
		t.Fatal(err)
	}

	// Exact warning sequence (prefix match: the bad-json line carries a
	// Go-version-dependent encoding/json error suffix).
	wantWarn := []string{
		"WARN bad-json airlines/british-airways/data/NYC-LON/2026-12-30.json",
		"WARN bad-date-file airlines/british-airways/data/NYC-LON/2026-13-40.json",
		"WARN bad-route-dir airlines/british-airways/data/badname",
		"WARN unknown-cabin Q airlines/british-airways/data/LON-TYO/2027-01-02.json",
		"WARN unknown-cabin Z airlines/british-airways/data/NYC-LON/2026-12-31.json",
		"WARN dropped-past-dates 1",
		"WARN unmapped-place-code XQP",
	}
	gotWarn := strings.Split(strings.TrimRight(stderr, "\n"), "\n")
	if len(gotWarn) != len(wantWarn) {
		t.Fatalf("got %d warning lines, want %d:\n%s", len(gotWarn), len(wantWarn), stderr)
	}
	for i, prefix := range wantWarn {
		if !strings.HasPrefix(gotWarn[i], prefix) {
			t.Errorf("warning[%d] = %q, want prefix %q", i, gotWarn[i], prefix)
		}
	}

	want := Result{
		Routes: 4, RouteDates: 5, Origins: 3, Places: 4, Warnings: 7,
		Written: 9, Deleted: 0, Unchanged: 0, BundleGz: res.BundleGz,
	}
	if *res != want {
		t.Errorf("result = %+v, want %+v", *res, want)
	}
	if res.BundleGz <= 0 {
		t.Error("bundle gz size must be positive")
	}

	got := treeFiles(t, out)
	golden := filepath.Join("testdata", "golden_a")
	if *update {
		if err := os.RemoveAll(golden); err != nil {
			t.Fatal(err)
		}
		for rel, b := range got {
			p := filepath.Join(golden, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("golden files updated under %s", golden)
	}
	wantFiles := treeFiles(t, golden)
	for rel := range got {
		if _, ok := wantFiles[rel]; !ok {
			t.Errorf("unexpected output file %s", rel)
		}
	}
	for rel, wantB := range wantFiles {
		gotB, ok := got[rel]
		if !ok {
			t.Errorf("missing output file %s", rel)
			continue
		}
		if !bytes.Equal(gotB, wantB) {
			t.Errorf("%s differs from golden:\ngot:\n%s\nwant:\n%s", rel, gotB, wantB)
		}
	}
}

func TestDeterminismCanary(t *testing.T) {
	out := t.TempDir()
	if _, _, err := runFixture(t, "testdata/src_a", out, shaA, timeA, false); err != nil {
		t.Fatal(err)
	}
	before := treeFiles(t, out)

	res, _, err := runFixture(t, "testdata/src_a", out, shaA, timeA, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Written != 0 || res.Deleted != 0 {
		t.Errorf("second run: written=%d deleted=%d, want 0/0", res.Written, res.Deleted)
	}
	if res.Unchanged != len(before) {
		t.Errorf("second run: unchanged=%d, want %d", res.Unchanged, len(before))
	}
	after := treeFiles(t, out)
	if len(after) != len(before) {
		t.Fatalf("file set changed: %d -> %d", len(before), len(after))
	}
	for rel, b := range before {
		if !bytes.Equal(after[rel], b) {
			t.Errorf("%s not byte-identical across runs", rel)
		}
	}
}

func TestStaleDeletionAndChanges(t *testing.T) {
	out := t.TempDir()
	readme := []byte("# derived data\n")
	if err := os.WriteFile(filepath.Join(out, "README.md"), readme, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runFixture(t, "testdata/src_a", out, shaA, timeA, false); err != nil {
		t.Fatal(err)
	}

	// Fixture B drops route LON-XQP, origin NYC, and LON-TYO's flight detail.
	res, stderr, err := runFixture(t, "testdata/src_b", out, shaB, timeB, false)
	if err != nil {
		t.Fatal(err)
	}
	if stderr != "" {
		t.Errorf("fixture B must be warning-free, got:\n%s", stderr)
	}
	if res.Routes != 2 || res.RouteDates != 3 || res.Origins != 2 || res.Places != 2 {
		t.Errorf("result = %+v", *res)
	}
	if res.Deleted != 2 { // origins/NYC.json + flights/LON/TYO/2027-01.json
		t.Errorf("deleted = %d, want 2", res.Deleted)
	}

	got := treeFiles(t, out)
	wantSet := []string{
		"README.md", "FORMAT.md", "manifest.json", "availability.json",
		"places.json", "changes/recent.json", "origins/LON.json", "origins/TYO.json",
	}
	if len(got) != len(wantSet) {
		t.Errorf("file set: %v", keys(got))
	}
	for _, rel := range wantSet {
		if _, ok := got[rel]; !ok {
			t.Errorf("missing %s", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "flights")); !os.IsNotExist(err) {
		t.Error("flights/ tree must be removed once no route has detail")
	}
	if !bytes.Equal(got["README.md"], readme) {
		t.Error("README.md was modified")
	}

	// The changes feed reports the two genuinely closed dates, route-sorted.
	var feed struct {
		Entries []map[string]any `json:"entries"`
		T       int64            `json:"t"`
		V       string           `json:"v"`
	}
	if err := json.Unmarshal(got["changes/recent.json"], &feed); err != nil {
		t.Fatal(err)
	}
	if feed.V != shaB || feed.T != timeB {
		t.Errorf("changes v/t = %s/%d", feed.V, feed.T)
	}
	wantEntries := []string{
		`closed LON-XQP BA 2026-12-31 W`,
		`closed NYC-LON BA 2026-12-31 M`,
	}
	if len(feed.Entries) != len(wantEntries) {
		t.Fatalf("entries = %v", feed.Entries)
	}
	for i, w := range wantEntries {
		e := feed.Entries[i]
		g := fmt.Sprintf("%v %v %v %v %v", e["k"], e["r"], e["al"], e["d"], e["c"])
		if g != w {
			t.Errorf("entry[%d] = %q, want %q", i, g, w)
		}
		if e["t"] != float64(timeB) {
			t.Errorf("entry[%d] t = %v, want %d", i, e["t"], timeB)
		}
	}
}

func TestShrinkGuardrail(t *testing.T) {
	out := t.TempDir()
	if _, _, err := runFixture(t, "testdata/src_a", out, shaA, timeA, false); err != nil {
		t.Fatal(err)
	}
	before := treeFiles(t, out)

	// src_small has 2 routeDates vs the previous 5: 2 < 2.5 -> refuse.
	_, _, err := runFixture(t, "testdata/src_small", out, shaB, timeB, false)
	if err == nil {
		t.Fatal("guardrail must hard-fail on a >50% routeDates shrink")
	}
	for _, want := range []string{"guardrail", "2", "5", "-force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must mention %q", err, want)
		}
	}
	after := treeFiles(t, out)
	if len(after) != len(before) {
		t.Fatal("guardrail failure must not write or delete anything")
	}
	for rel, b := range before {
		if !bytes.Equal(after[rel], b) {
			t.Errorf("guardrail failure modified %s", rel)
		}
	}

	// -force overrides.
	res, _, err := runFixture(t, "testdata/src_small", out, shaB, timeB, true)
	if err != nil {
		t.Fatalf("-force run: %v", err)
	}
	if res.RouteDates != 2 {
		t.Errorf("forced run routeDates = %d", res.RouteDates)
	}
}

func TestBudgetViolationNamesFile(t *testing.T) {
	src := t.TempDir()
	dir := filepath.Join(src, "airlines", "british-airways", "data", "LON-TYO")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// ~200KB of pseudo-random hex as a flight number: incompressible, so the
	// route-month detail file blows the 50KiB gzip budget through the real
	// pipeline.
	const digits = "0123456789ABCDEF"
	big := make([]byte, 200<<10)
	state := uint64(1)
	for i := range big {
		state = state*6364136223846793005 + 1442695040888963407
		big[i] = digits[(state>>33)&0xF]
	}
	payload := fmt.Sprintf(`{
  "cabinsAvailable": ["M"],
  "flights": [{
    "flightNumbers": ["%s"],
    "carriers": ["BA"],
    "via": [],
    "depart": "13:00",
    "arrive": "08:55",
    "peak": null,
    "rewardFlightSaver": false,
    "seats": {"M": 1}
  }]
}`, big)
	if err := os.WriteFile(filepath.Join(dir, "2026-12-30.json"), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	out := t.TempDir()
	_, stderr, err := runFixture(t, src, out, shaA, timeA, false)
	if err == nil {
		t.Fatal("oversized output must hard-fail")
	}
	if !strings.Contains(err.Error(), "flights/LON/TYO/2026-12.json") || !strings.Contains(err.Error(), "over its") {
		t.Errorf("error must name the offending file and overage, got: %v", err)
	}
	if stderr != "" {
		t.Errorf("unexpected warnings: %s", stderr)
	}
	if files := treeFiles(t, out); len(files) != 0 {
		t.Errorf("budget failure must not write anything, found %v", keys(files))
	}
}

func TestHardFailures(t *testing.T) {
	src, out := t.TempDir(), t.TempDir()

	// Missing roots.
	if _, _, err := runFixture(t, filepath.Join(src, "missing"), out, shaA, timeA, false); err == nil {
		t.Error("missing -src must fail")
	}
	if _, _, err := runFixture(t, "testdata/src_a", filepath.Join(out, "missing"), shaA, timeA, false); err == nil {
		t.Error("missing -out must fail")
	}

	// A source tree with no data at all.
	if err := os.MkdirAll(filepath.Join(src, "airlines", "british-airways", "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runFixture(t, src, out, shaA, timeA, false); err == nil {
		t.Error("empty dataset must fail")
	}

	// All data in the past (dropped by the cutoff) is also "no data".
	dir := filepath.Join(src, "airlines", "british-airways", "data", "LON-TYO")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "2020-01-01.json"), []byte(`{"cabinsAvailable":["M"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runFixture(t, src, out, shaA, timeA, false); err == nil {
		t.Error("dataset entirely in the past must fail")
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
