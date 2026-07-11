// Package source walks and parses a checkout of the rewardflights source
// repository (layout: airlines/<slug>/data/<ORIG>-<DEST>/<YYYY-MM-DD>.json,
// schema per the airline's FIELDS.md).
//
// Malformed entries — bad route directory names, bad date filenames, invalid
// JSON — are warned about and skipped; a single bad file never aborts the
// run. Only structural problems (an unreadable tree) return an error.
package source

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/warnings"
)

var (
	routeDirRe = regexp.MustCompile(`^[A-Z]{3}-[A-Z]{3}$`)
	dateFileRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}\.json$`)
)

// Flight mirrors one entry of the optional "flights" array in a source date
// file (see the airline's FIELDS.md).
type Flight struct {
	FlightNumbers     []string       `json:"flightNumbers"`
	Carriers          []string       `json:"carriers"`
	Via               []string       `json:"via"`
	Depart            string         `json:"depart"`
	Arrive            string         `json:"arrive"`
	Peak              *string        `json:"peak"` // "peak" | "off-peak" | null (unknown)
	RewardFlightSaver bool           `json:"rewardFlightSaver"`
	Seats             map[string]int `json:"seats"`
}

// DateEntry is one parsed <route>/<date>.json source file.
type DateEntry struct {
	Cabins  []string // raw cabin codes from cabinsAvailable (unvalidated)
	Flights []Flight // optional per-flight detail; nil/empty when absent
	Path    string   // source-relative path (slash-separated), for warnings
}

// Route holds every available date parsed for one directional metro route.
type Route struct {
	Dates map[string]DateEntry // "YYYY-MM-DD" -> entry
}

// Airline holds every route parsed for one airline directory slug.
type Airline struct {
	Slug   string
	Routes map[string]*Route // "ORIG-DEST" -> route
}

// Dataset is everything parsed from one source checkout.
type Dataset struct {
	Airlines map[string]*Airline // slug -> airline
}

// dateFile is the on-disk schema of a source date file. CabinsAvailable is a
// pointer so a missing key (malformed) is distinguishable from an empty list.
type dateFile struct {
	CabinsAvailable *[]string `json:"cabinsAvailable"`
	Flights         []Flight  `json:"flights"`
}

// Load parses the source tree under srcDir. It returns an error only when
// the tree itself is unreadable; per-file problems are warned and skipped.
func Load(srcDir string, log *warnings.Log) (*Dataset, error) {
	airlinesDir := filepath.Join(srcDir, "airlines")
	entries, err := os.ReadDir(airlinesDir)
	if err != nil {
		return nil, fmt.Errorf("reading source airlines directory: %w", err)
	}
	ds := &Dataset{Airlines: map[string]*Airline{}}
	for _, e := range entries {
		if !e.IsDir() || hidden(e.Name()) {
			continue
		}
		airline, err := loadAirline(srcDir, e.Name(), log)
		if err != nil {
			return nil, err
		}
		if airline != nil {
			ds.Airlines[airline.Slug] = airline
		}
	}
	return ds, nil
}

// loadAirline parses airlines/<slug>/data. A slug directory without a data
// subdirectory is skipped silently (docs, fixtures, …). Returns nil when the
// airline contributed no routes.
func loadAirline(srcDir, slug string, log *warnings.Log) (*Airline, error) {
	dataDir := filepath.Join(srcDir, "airlines", slug, "data")
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", dataDir, err)
	}
	airline := &Airline{Slug: slug, Routes: map[string]*Route{}}
	for _, e := range entries {
		name := e.Name()
		if hidden(name) {
			continue
		}
		rel := path.Join("airlines", slug, "data", name)
		if !e.IsDir() || !routeDirRe.MatchString(name) {
			log.Warn("bad-route-dir", rel)
			continue
		}
		route, err := loadRoute(srcDir, rel, log)
		if err != nil {
			return nil, err
		}
		if len(route.Dates) > 0 {
			airline.Routes[name] = route
		}
	}
	if len(airline.Routes) == 0 {
		return nil, nil
	}
	return airline, nil
}

// loadRoute parses every date file in one route directory.
func loadRoute(srcDir, routeRel string, log *warnings.Log) (*Route, error) {
	entries, err := os.ReadDir(filepath.Join(srcDir, filepath.FromSlash(routeRel)))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", routeRel, err)
	}
	route := &Route{Dates: map[string]DateEntry{}}
	for _, e := range entries {
		name := e.Name()
		if hidden(name) {
			continue
		}
		rel := path.Join(routeRel, name)
		date := strings.TrimSuffix(name, ".json")
		if e.IsDir() || !dateFileRe.MatchString(name) || !validDate(date) {
			log.Warn("bad-date-file", rel)
			continue
		}
		raw, err := os.ReadFile(filepath.Join(srcDir, filepath.FromSlash(rel)))
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", rel, err)
		}
		var df dateFile
		if err := json.Unmarshal(raw, &df); err != nil {
			log.Warn("bad-json", rel, err.Error())
			continue
		}
		if df.CabinsAvailable == nil {
			log.Warn("bad-json", rel, "missing cabinsAvailable")
			continue
		}
		route.Dates[date] = DateEntry{Cabins: *df.CabinsAvailable, Flights: df.Flights, Path: rel}
	}
	return route, nil
}

// validDate reports whether s is a real calendar date in YYYY-MM-DD form
// (rejects e.g. 2026-02-30, which matches the filename regexp).
func validDate(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

func hidden(name string) bool { return strings.HasPrefix(name, ".") }
