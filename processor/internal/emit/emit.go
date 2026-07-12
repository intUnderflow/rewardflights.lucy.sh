package emit

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Managed output set: the processor owns exactly these paths inside -out and
// never touches anything else (README.md, LICENSE, .git, …).
var (
	managedRootFiles = []string{"FORMAT.md", "availability.json", "manifest.json", "places.json"}
	managedDirs      = []string{"changes", "flights", "origins"}
)

// Size budgets (hard failure when exceeded). The gzip budgets bound what
// clients download; the raw budget on the bundle is a second net against
// pathological-but-compressible output (e.g. a nibble horizon stretched by a
// bad far-future date gzips to almost nothing yet parses to hundreds of MB).
const (
	BundlePath     = "availability.json"
	BundleLimitGz  = 300 << 10 // 300 KiB gzipped
	FileLimitGz    = 50 << 10  // 50 KiB gzipped, every non-bundle file
	BundleLimitRaw = 4 << 20   // 4 MiB uncompressed, bundle only
)

// GzipSize returns the size of data after gzip at best compression.
func GzipSize(data []byte) (int, error) {
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return 0, err
	}
	if _, err := w.Write(data); err != nil {
		return 0, err
	}
	if err := w.Close(); err != nil {
		return 0, err
	}
	return buf.Len(), nil
}

// CheckBudgets gzips every desired output in memory and fails when any file
// exceeds its budget, naming each offender and the overage.
func CheckBudgets(files map[string][]byte) error {
	var violations []string
	for _, path := range slices.Sorted(maps.Keys(files)) {
		if raw := len(files[path]); path == BundlePath && raw > BundleLimitRaw {
			violations = append(violations,
				fmt.Sprintf("%s is %d bytes raw, %d over its %d-byte raw budget", path, raw, raw-BundleLimitRaw, BundleLimitRaw))
		}
		gz, err := GzipSize(files[path])
		if err != nil {
			return fmt.Errorf("gzipping %s: %w", path, err)
		}
		limit := FileLimitGz
		if path == BundlePath {
			limit = BundleLimitGz
		}
		if gz > limit {
			violations = append(violations,
				fmt.Sprintf("%s is %d bytes gzipped, %d over its %d-byte budget", path, gz, gz-limit, limit))
		}
	}
	if violations != nil {
		return fmt.Errorf("size budget exceeded: %s", strings.Join(violations, "; "))
	}
	return nil
}

// SyncStats reports what Sync did.
type SyncStats struct {
	Written   int // files created or rewritten (content differed)
	Deleted   int // stale managed files removed
	Unchanged int // desired files already byte-identical (left untouched)
}

// Sync makes the managed portion of outDir exactly match desired
// (out-relative slash path -> bytes): write-if-different (identical files
// are not rewritten, preserving mtime and git status), delete managed files
// absent from desired, and prune emptied managed directories. Non-managed
// paths are never touched; desired paths outside the managed set are
// rejected.
func Sync(outDir string, desired map[string][]byte) (SyncStats, error) {
	var stats SyncStats
	for path := range desired {
		if !Managed(path) {
			return stats, fmt.Errorf("internal: refusing to write non-managed path %q", path)
		}
	}

	// Never write through a symlink: a link planted at a managed path would
	// redirect writes (and dodge stale deletion) outside the managed tree.
	if err := sanitizeManaged(outDir); err != nil {
		return stats, err
	}

	existing, err := listManaged(outDir)
	if err != nil {
		return stats, err
	}

	for _, path := range slices.Sorted(maps.Keys(desired)) {
		abs := filepath.Join(outDir, filepath.FromSlash(path))
		current, err := os.ReadFile(abs)
		switch {
		case err == nil && bytes.Equal(current, desired[path]):
			stats.Unchanged++
			continue
		case err != nil && !os.IsNotExist(err):
			return stats, fmt.Errorf("reading existing %s: %w", path, err)
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return stats, fmt.Errorf("creating directory for %s: %w", path, err)
		}
		if err := os.WriteFile(abs, desired[path], 0o644); err != nil {
			return stats, fmt.Errorf("writing %s: %w", path, err)
		}
		stats.Written++
	}

	for _, path := range existing {
		if _, ok := desired[path]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(outDir, filepath.FromSlash(path))); err != nil {
			return stats, fmt.Errorf("deleting stale %s: %w", path, err)
		}
		stats.Deleted++
	}

	for _, dir := range managedDirs {
		if err := pruneEmptyDirs(filepath.Join(outDir, dir)); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// Managed reports whether an out-relative slash path is processor-owned.
func Managed(path string) bool {
	if slices.Contains(managedRootFiles, path) {
		return true
	}
	for _, dir := range managedDirs {
		if strings.HasPrefix(path, dir+"/") {
			return true
		}
	}
	return false
}

// sanitizeManaged removes anything at a managed path that is not what the
// emitter expects to find there — symlinks anywhere (root files, managed
// dirs, or any component inside them), a directory squatting on a root file
// path, or a plain file squatting on a managed dir path. These paths are
// processor-owned, so removal is safe; failure to remove is a hard error
// because writing would otherwise follow the link outside the managed tree.
func sanitizeManaged(outDir string) error {
	for _, name := range managedRootFiles {
		p := filepath.Join(outDir, name)
		info, err := os.Lstat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspecting %s: %w", name, err)
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			if err := os.Remove(p); err != nil {
				return fmt.Errorf("removing symlink at managed path %s: %w", name, err)
			}
		case info.IsDir():
			if err := os.RemoveAll(p); err != nil {
				return fmt.Errorf("removing directory at managed file path %s: %w", name, err)
			}
		}
	}
	for _, dir := range managedDirs {
		if err := sanitizeTree(filepath.Join(outDir, dir)); err != nil {
			return err
		}
	}
	return nil
}

// sanitizeTree ensures root, if present, is a real directory containing no
// symlinks at any depth (symlinked entries are removed, real subdirectories
// are recursed into).
func sanitizeTree(root string) error {
	info, err := os.Lstat(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspecting %s: %w", root, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		if err := os.Remove(root); err != nil {
			return fmt.Errorf("removing non-directory at managed dir path %s: %w", root, err)
		}
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := filepath.Join(root, e.Name())
		switch {
		case e.Type()&os.ModeSymlink != 0:
			if err := os.Remove(p); err != nil {
				return fmt.Errorf("removing symlink at managed path %s: %w", p, err)
			}
		case e.IsDir():
			if err := sanitizeTree(p); err != nil {
				return err
			}
		}
	}
	return nil
}

// listManaged returns every managed file currently present in outDir, as
// out-relative slash paths.
func listManaged(outDir string) ([]string, error) {
	var found []string
	for _, name := range managedRootFiles {
		if info, err := os.Stat(filepath.Join(outDir, name)); err == nil && info.Mode().IsRegular() {
			found = append(found, name)
		}
	}
	for _, dir := range managedDirs {
		root := filepath.Join(outDir, dir)
		err := filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) && p == root {
					return nil // managed dir doesn't exist yet
				}
				return err
			}
			if entry.Type().IsRegular() {
				rel, err := filepath.Rel(outDir, p)
				if err != nil {
					return err
				}
				found = append(found, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("scanning %s: %w", root, err)
		}
	}
	return found, nil
}

// pruneEmptyDirs removes dir if it is (recursively) empty, depth-first.
// A missing dir is fine.
func pruneEmptyDirs(dir string) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			if err := pruneEmptyDirs(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	entries, err = os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return os.Remove(dir)
	}
	return nil
}
