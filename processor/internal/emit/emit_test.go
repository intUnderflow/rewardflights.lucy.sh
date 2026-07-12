package emit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCanonical(t *testing.T) {
	got, err := Canonical(map[string]any{
		"b": 1,
		"a": "<x> & ü ©", // no HTML escaping, raw UTF-8
		"c": map[string]any{"z": []string{}, "y": nil},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n \"a\": \"<x> & ü ©\",\n \"b\": 1,\n \"c\": {\n  \"y\": null,\n  \"z\": []\n }\n}\n"
	if string(got) != want {
		t.Errorf("Canonical mismatch:\ngot  %q\nwant %q", got, want)
	}
	if !bytes.HasSuffix(got, []byte("}\n")) || bytes.HasSuffix(got, []byte("\n\n")) {
		t.Error("must end with exactly one trailing newline")
	}
}

// pseudoRandom fills n bytes with deterministic incompressible-ish hex.
func pseudoRandom(n int) []byte {
	const digits = "0123456789ABCDEF"
	buf := make([]byte, n)
	state := uint64(0x2545F4914F6CDD1D)
	for i := range buf {
		state = state*6364136223846793005 + 1442695040888963407
		buf[i] = digits[(state>>33)&0xF]
	}
	return buf
}

func TestCheckBudgets(t *testing.T) {
	small := []byte(`{"ok":true}`)
	if err := CheckBudgets(map[string][]byte{
		BundlePath: small, "origins/LON.json": small, "FORMAT.md": small,
	}); err != nil {
		t.Fatalf("small files must pass: %v", err)
	}

	// Random hex has ~4 bits entropy per byte: 800KB -> ~400KB gz (over the
	// 300KiB bundle budget), 200KB -> ~100KB gz (over the 50KiB file budget).
	err := CheckBudgets(map[string][]byte{
		BundlePath:         pseudoRandom(800 << 10),
		"origins/ZZZ.json": pseudoRandom(200 << 10),
		"places.json":      small,
	})
	if err == nil {
		t.Fatal("oversized files must fail")
	}
	msg := err.Error()
	for _, want := range []string{"availability.json", "origins/ZZZ.json", "over its"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q must mention %q", msg, want)
		}
	}
	// The bundle gets the 300KiB budget, not the 50KiB one.
	if !strings.Contains(msg, "307200-byte budget") || !strings.Contains(msg, "51200-byte budget") {
		t.Errorf("error %q must cite both budgets", msg)
	}
}

func TestBundleRawBudget(t *testing.T) {
	// 5 MiB of zeros gzips to a few KiB — the gz budget passes, but the raw
	// second net must catch it (a stretched nibble horizon looks like this).
	huge := bytes.Repeat([]byte{'0'}, 5<<20)
	err := CheckBudgets(map[string][]byte{BundlePath: huge})
	if err == nil {
		t.Fatal("compressible-but-huge bundle must fail the raw budget")
	}
	if !strings.Contains(err.Error(), "raw budget") || !strings.Contains(err.Error(), BundlePath) {
		t.Errorf("error %q must cite the raw budget and the bundle", err)
	}
	// Non-bundle files have no raw budget (they are gz-bounded already).
	if err := CheckBudgets(map[string][]byte{"origins/LON.json": bytes.Repeat([]byte{'0'}, 5<<20)}); err != nil {
		t.Errorf("raw budget must apply to the bundle only: %v", err)
	}
	if err := CheckBudgets(map[string][]byte{BundlePath: bytes.Repeat([]byte{'0'}, 4<<20)}); err != nil {
		t.Errorf("exactly 4 MiB must pass: %v", err)
	}
}

func TestManaged(t *testing.T) {
	for path, want := range map[string]bool{
		"manifest.json": true, "availability.json": true, "places.json": true,
		"FORMAT.md": true, "origins/LON.json": true, "flights/LON/TYO/2026-07.json": true,
		"changes/recent.json": true,
		"README.md":           false, "LICENSE": false, "DbCL-1.0.txt": false,
		".git/config": false, "originsX.json": false, "origins": false,
	} {
		if got := Managed(path); got != want {
			t.Errorf("Managed(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestSyncLifecycle(t *testing.T) {
	out := t.TempDir()
	readme := []byte("# hands off\n")
	if err := os.WriteFile(filepath.Join(out, "README.md"), readme, 0o644); err != nil {
		t.Fatal(err)
	}

	desired := map[string][]byte{
		"manifest.json":                []byte("m1\n"),
		"origins/LON.json":             []byte("lon\n"),
		"flights/LON/TYO/2026-07.json": []byte("f\n"),
	}
	stats, err := Sync(out, desired)
	if err != nil {
		t.Fatal(err)
	}
	if stats != (SyncStats{Written: 3}) {
		t.Fatalf("first sync stats = %+v, want 3 written", stats)
	}

	// A stray file inside a managed dir is managed territory: it gets removed.
	if err := os.WriteFile(filepath.Join(out, "origins", "STRAY.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Backdate manifest.json to prove identical content is not rewritten.
	past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(out, "manifest.json"), past, past); err != nil {
		t.Fatal(err)
	}

	stats, err = Sync(out, map[string][]byte{
		"manifest.json":    []byte("m1\n"),   // identical
		"origins/LON.json": []byte("lon2\n"), // changed
		// flights file gone from desired set
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats != (SyncStats{Written: 1, Deleted: 2, Unchanged: 1}) {
		t.Fatalf("second sync stats = %+v, want written=1 deleted=2 unchanged=1", stats)
	}
	info, err := os.Stat(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(past) {
		t.Error("identical manifest.json was rewritten (mtime changed)")
	}
	if _, err := os.Stat(filepath.Join(out, "flights")); !os.IsNotExist(err) {
		t.Error("emptied flights/ tree must be removed entirely")
	}
	if _, err := os.Stat(filepath.Join(out, "origins", "STRAY.txt")); !os.IsNotExist(err) {
		t.Error("stray file inside managed origins/ must be deleted")
	}
	got, err := os.ReadFile(filepath.Join(out, "README.md"))
	if err != nil || !bytes.Equal(got, readme) {
		t.Errorf("README.md must never be touched (got %q, %v)", got, err)
	}
}

func TestSyncReplacesSymlinkedManagedPaths(t *testing.T) {
	out, victim := t.TempDir(), t.TempDir()
	secret := filepath.Join(victim, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A symlinked managed root file, a symlinked managed dir, and a
	// symlinked subdirectory inside a real managed dir.
	if err := os.Symlink(secret, filepath.Join(out, "availability.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(out, "origins")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(out, "flights"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(out, "flights", "LON")); err != nil {
		t.Fatal(err)
	}

	desired := map[string][]byte{
		"availability.json":            []byte("bundle\n"),
		"origins/LON.json":             []byte("lon\n"),
		"flights/LON/TYO/2026-07.json": []byte("f\n"),
	}
	if _, err := Sync(out, desired); err != nil {
		t.Fatal(err)
	}

	for rel, want := range desired {
		p := filepath.Join(out, filepath.FromSlash(rel))
		// Every component below out must be a real file/dir, not a link.
		for sub := p; len(sub) > len(out); sub = filepath.Dir(sub) {
			info, err := os.Lstat(sub)
			if err != nil {
				t.Fatalf("missing %s: %v", sub, err)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				t.Errorf("%s is still a symlink", sub)
			}
		}
		got, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s content = %q, %v", rel, got, err)
		}
	}

	// Nothing escaped into the symlink target.
	if got, err := os.ReadFile(secret); err != nil || string(got) != "secret" {
		t.Errorf("victim file modified: %q, %v", got, err)
	}
	entries, err := os.ReadDir(victim)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "secret.txt" {
		t.Errorf("victim dir polluted: %v", entries)
	}
}

func TestSyncRejectsNonManagedPaths(t *testing.T) {
	out := t.TempDir()
	if _, err := Sync(out, map[string][]byte{"README.md": []byte("evil")}); err == nil {
		t.Fatal("Sync must refuse to write non-managed paths")
	}
	if _, err := Sync(out, map[string][]byte{"../escape.json": []byte("evil")}); err == nil {
		t.Fatal("Sync must refuse paths outside the managed set")
	}
}
