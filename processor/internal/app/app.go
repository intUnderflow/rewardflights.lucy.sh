// Package app wires the processor pipeline: validate roots, load the source
// tree, derive the managed file set, enforce safety rails and size budgets,
// and sync into the derived repository checkout.
package app

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/intUnderflow/rewardflights.lucy.sh/processor/assets"
	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/derive"
	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/emit"
	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/source"
	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/warnings"
)

// Config is one processor invocation.
type Config struct {
	Src        string // rewardflights checkout (read)
	Out        string // derived data repo checkout (managed paths written)
	SourceSHA  string // embedded as "v"
	SourceTime int64  // embedded as "t" (unix seconds, source committer time)
	Force      bool   // override the 50%-shrink guardrail
	Warnings   io.Writer
}

// Result is the run summary.
type Result struct {
	Routes, RouteDates, Origins, Places int
	Warnings                            int
	Written, Deleted, Unchanged         int
	BundleGz                            int
}

// Run executes one full processor run. A returned error is a hard failure
// (nonzero exit); warnings alone do not fail the run.
func Run(cfg Config) (*Result, error) {
	if err := checkDir(cfg.Src, "-src"); err != nil {
		return nil, err
	}
	if err := checkDir(cfg.Out, "-out"); err != nil {
		return nil, err
	}

	log := warnings.New(cfg.Warnings)
	dataset, err := source.Load(cfg.Src, log)
	if err != nil {
		return nil, err
	}
	places, err := derive.ParsePlaces(assets.PlacesJSON)
	if err != nil {
		return nil, err
	}
	airlines, err := derive.ParseAirlines(assets.AirlinesJSON)
	if err != nil {
		return nil, err
	}

	out, err := derive.Build(derive.Inputs{
		Dataset:    dataset,
		Places:     places,
		Airlines:   airlines,
		SHA:        cfg.SourceSHA,
		SourceTime: cfg.SourceTime,
		OldBundle:  readIfExists(filepath.Join(cfg.Out, "availability.json")),
		OldChanges: readIfExists(filepath.Join(cfg.Out, "changes", "recent.json")),
		Log:        log,
	})
	if err != nil {
		return nil, err
	}
	files := out.Files
	files["FORMAT.md"] = assets.FormatMD

	// Safety rails and budgets run before anything is written or deleted, so
	// a failing run leaves the checkout untouched.
	if !cfg.Force {
		if prev, ok := previousRouteDates(filepath.Join(cfg.Out, "manifest.json")); ok && out.RouteDates*2 < prev {
			return nil, fmt.Errorf(
				"shrink guardrail: new routeDates %d is less than 50%% of previous %d; refusing to write (pass -force to override)",
				out.RouteDates, prev)
		}
	}
	if err := emit.CheckBudgets(files); err != nil {
		return nil, err
	}

	stats, err := emit.Sync(cfg.Out, files)
	if err != nil {
		return nil, err
	}
	bundleGz, err := emit.GzipSize(files[emit.BundlePath])
	if err != nil {
		return nil, err
	}
	return &Result{
		Routes: out.Routes, RouteDates: out.RouteDates,
		Origins: out.Origins, Places: out.Places,
		Warnings: log.Count(),
		Written:  stats.Written, Deleted: stats.Deleted, Unchanged: stats.Unchanged,
		BundleGz: bundleGz,
	}, nil
}

// Summary renders the machine-parseable one-line run summary.
func (r *Result) Summary() string {
	return fmt.Sprintf(
		"SUMMARY routes=%d routeDates=%d origins=%d places=%d warnings=%d written=%d deleted=%d unchanged=%d bundleGzBytes=%d",
		r.Routes, r.RouteDates, r.Origins, r.Places, r.Warnings,
		r.Written, r.Deleted, r.Unchanged, r.BundleGz)
}

func checkDir(path, flagName string) error {
	if path == "" {
		return fmt.Errorf("%s is required", flagName)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s %s: %w", flagName, path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s %s: not a directory", flagName, path)
	}
	return nil
}

// readIfExists returns the file's bytes, or nil for any read problem
// (absent previous state is a normal condition).
func readIfExists(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return b
}

// previousRouteDates extracts counts.routeDates from a previous
// manifest.json; ok=false when the manifest is absent or unparseable
// (the guardrail then has no baseline and does not apply).
func previousRouteDates(path string) (int, bool) {
	raw := readIfExists(path)
	if raw == nil {
		return 0, false
	}
	var manifest struct {
		Counts struct {
			RouteDates *int `json:"routeDates"`
		} `json:"counts"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil || manifest.Counts.RouteDates == nil {
		return 0, false
	}
	return *manifest.Counts.RouteDates, true
}
