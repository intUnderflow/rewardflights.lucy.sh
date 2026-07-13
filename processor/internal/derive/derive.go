// Package derive transforms a parsed source dataset into the derived data
// repository's file set: manifest, availability bundle, origin shards, place
// table, per-route-month flight detail, and the rolling changes feed.
//
// Everything here is deterministic: given the same inputs (source data,
// source sha/time, and previous derived state for the changes feed) the
// produced bytes are identical. No wall clock is consulted.
package derive

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/emit"
	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/source"
	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/warnings"
)

// Place is one curated place-table entry.
type Place struct {
	Name    string    `json:"name"`
	Country string    `json:"country"`
	Search  []string  `json:"search"` // autocomplete aliases for multi-airport metros
	G       []float64 `json:"g"`      // [lat, lon]; city itself for metro codes, else the airport
}

// AirlineInfo is one entry of the append-only airline registry
// (assets/airlines.json), keyed there by source directory slug.
type AirlineInfo struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Cabins map[string]string `json:"cabins"` // bitmask value (decimal string) -> label
}

// futureCapDays bounds how far past the source commit date an availability
// date may lie. A single corrupt far-future file (e.g. 2999-12-31) would
// otherwise stretch every nibble string to that horizon, ballooning memory
// and output size. Dates beyond cutoff+futureCapDays are dropped with an
// aggregated warning, mirroring the past-date filter. BA's booking window is
// ~355 days, so 450 leaves generous headroom.
const futureCapDays = 450

// provenanceBlock is embedded (as "source") in manifest.json and every
// availability file. The dataset carries no license of its own; this is
// provenance + a no-warranty note for downstream consumers.
var provenanceBlock = map[string]any{
	"repo": "https://github.com/intUnderflow/rewardflights",
	"note": "Derived from github.com/intUnderflow/rewardflights. Availability facts provided as-is, with no warranty and no guarantee of accuracy or bookability.",
}

// ParsePlaces parses the embedded curated place table.
func ParsePlaces(raw []byte) (map[string]Place, error) {
	var m map[string]Place
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parsing embedded places.json: %w", err)
	}
	return m, nil
}

// ParseAirlines parses the embedded airline registry (slug -> info).
func ParseAirlines(raw []byte) (map[string]AirlineInfo, error) {
	var m map[string]AirlineInfo
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parsing embedded airlines.json: %w", err)
	}
	return m, nil
}

// Inputs carries everything Build needs.
type Inputs struct {
	Dataset    *source.Dataset
	Places     map[string]Place       // curated place table
	Airlines   map[string]AirlineInfo // airline registry, keyed by slug
	SHA        string                 // source commit sha -> "v"
	SourceTime int64                  // source commit unix seconds -> "t"
	OldBundle  []byte                 // previous availability.json from -out, nil if absent
	OldChanges []byte                 // previous changes/recent.json from -out, nil if absent
	Log        *warnings.Log
}

// Output is the desired derived file set plus summary statistics.
type Output struct {
	Files      map[string][]byte // out-relative slash path -> exact bytes
	Routes     int
	RouteDates int
	Origins    int
	Places     int
}

// Build derives the complete managed file set (except FORMAT.md, which is a
// static asset added by the caller). It hard-fails only on internal errors
// or a dataset with no usable data at all.
func Build(in Inputs) (*Output, error) {
	cutoffDay := unixDay(in.SourceTime)

	// route -> airline id -> day number -> cabin bits
	routeBits := map[string]map[string]map[int]int{}
	// route -> airline id -> day number -> packed per-cabin seat-threshold
	// byte (see SeatString). A route+airline appears here only when at least
	// one of its days carries flight detail — that presence gates the "s"
	// key, so a dataset without flights[] emits byte-identical output.
	routeSeats := map[string]map[string]map[int]int{}
	// route -> "YYYY-MM-DD" -> airline id -> flight objects
	flightDays := map[string]map[string]map[string][]map[string]any{}
	airlinesJSON := map[string]any{}
	minDate, maxDate := "", ""
	routeDates, droppedPast, droppedFuture := 0, 0, 0

	for _, slug := range slices.Sorted(maps.Keys(in.Dataset.Airlines)) {
		reg, ok := in.Airlines[slug]
		if !ok {
			in.Log.Warn("unknown-airline", slug)
			continue
		}
		airline := in.Dataset.Airlines[slug]
		airlinesJSON[reg.ID] = map[string]any{"cabins": reg.Cabins, "name": reg.Name, "slug": slug}
		for _, route := range slices.Sorted(maps.Keys(airline.Routes)) {
			for _, date := range slices.Sorted(maps.Keys(airline.Routes[route].Dates)) {
				entry := airline.Routes[route].Dates[date]
				day, err := parseDay(date)
				if err != nil {
					return nil, fmt.Errorf("internal: %w", err) // filenames validated by source walker
				}
				if day < cutoffDay {
					droppedPast++
					continue
				}
				if day > cutoffDay+futureCapDays {
					droppedFuture++
					continue
				}
				bits := 0
				for _, cabin := range entry.Cabins {
					b, ok := CabinBits[cabin]
					if !ok {
						in.Log.Warn("unknown-cabin", cabin, entry.Path)
						continue
					}
					bits |= b
				}
				if bits == 0 {
					// A source file exists iff its date has availability; an
					// empty (or entirely unrecognized) cabin list would skew
					// routeDates and the shrink guardrail. Skip the date.
					in.Log.Warn("empty-cabins", entry.Path)
					continue
				}
				if minDate == "" || date < minDate {
					minDate = date
				}
				if date > maxDate {
					maxDate = date
				}
				byAirline, ok := routeBits[route]
				if !ok {
					byAirline = map[string]map[int]int{}
					routeBits[route] = byAirline
				}
				byDay, ok := byAirline[reg.ID]
				if !ok {
					byDay = map[int]int{}
					byAirline[reg.ID] = byDay
				}
				byDay[day] |= bits
				routeDates++
				if len(entry.Flights) > 0 {
					flights := make([]map[string]any, 0, len(entry.Flights))
					for _, f := range entry.Flights {
						flights = append(flights, flightJSON(f, entry.Path, in.Log))
					}
					byDate, ok := flightDays[route]
					if !ok {
						byDate = map[string]map[string][]map[string]any{}
						flightDays[route] = byDate
					}
					if byDate[date] == nil {
						byDate[date] = map[string][]map[string]any{}
					}
					byDate[date][reg.ID] = append(byDate[date][reg.ID], flights...)

					// Seat-threshold byte for the day. Per cabin the count is
					// the MAX across the day's flights (a party must fit on
					// ONE flight — never a sum); negative counts are ignored
					// (flightJSON already warned) and unknown cabin codes are
					// skipped (ditto). "a" is the sole presence authority: a
					// cabin's code may be nonzero only when its bit is set in
					// the day's bitmask, and contradictions resolve to the
					// bit with a warning.
					seatByte := 0
					noEvidence := make([]byte, 0, 4)
					for i, c := range cabinOrder {
						maxN := 0
						for _, f := range entry.Flights {
							if n := f.Seats[string(c.letter)]; n > maxN {
								maxN = n
							}
						}
						if bits&c.bit == 0 {
							if maxN > 0 {
								in.Log.Warn("seats-cabin-not-available", string(c.letter), entry.Path)
							}
							continue
						}
						if maxN == 0 {
							noEvidence = append(noEvidence, c.letter)
							continue
						}
						seatByte |= seatCode(maxN) << (2 * i)
					}
					if len(noEvidence) > 0 {
						// The opposite contradiction: cabinsAvailable claims
						// the cabin but no flight shows a single seat in it.
						// The code stays 0 (count unknown, NOT "0 seats").
						in.Log.Warn("available-cabin-zero-seats", string(noEvidence), entry.Path)
					}
					bySeatAirline, ok := routeSeats[route]
					if !ok {
						bySeatAirline = map[string]map[int]int{}
						routeSeats[route] = bySeatAirline
					}
					if bySeatAirline[reg.ID] == nil {
						bySeatAirline[reg.ID] = map[int]int{}
					}
					bySeatAirline[reg.ID][day] = seatByte
				}
			}
		}
	}
	if droppedPast > 0 {
		in.Log.Warn("dropped-past-dates", strconv.Itoa(droppedPast))
	}
	if droppedFuture > 0 {
		in.Log.Warn("dropped-future-dates", strconv.Itoa(droppedFuture))
	}
	if routeDates == 0 {
		return nil, errors.New("no availability data found in source tree (after past-date filtering)")
	}

	epochStr, epochDay, days, err := Epoch(minDate, maxDate)
	if err != nil {
		return nil, err
	}

	// Flight-detail months per route (feeds both "fm" and the file set).
	flightMonths := map[string][]string{}
	for route, byDate := range flightDays {
		months := map[string]bool{}
		for date := range byDate {
			months[date[:7]] = true
		}
		flightMonths[route] = slices.Sorted(maps.Keys(months))
	}

	// Route entries: {"a": {id: nibbles}, "fm": [months]?, "s": {id: seats}?}.
	// The optional "s" key must be added HERE, before the origin-shard loop:
	// shards reuse these entry maps by reference, so mutating one later would
	// corrupt the bundle.
	routesJSON := map[string]any{}
	for route, byAirline := range routeBits {
		strings := map[string]any{}
		for id, byDay := range byAirline {
			strings[id] = NibbleString(days, epochDay, byDay)
		}
		entry := map[string]any{"a": strings}
		if months := flightMonths[route]; len(months) > 0 {
			entry["fm"] = months
		}
		if bySeatAirline := routeSeats[route]; len(bySeatAirline) > 0 {
			seats := map[string]any{}
			for id, byDay := range bySeatAirline {
				seats[id] = SeatString(days, epochDay, byDay)
			}
			entry["s"] = seats
		}
		routesJSON[route] = entry
	}

	// Place table: every code referenced by a route key.
	codes := map[string]bool{}
	for route := range routeBits {
		codes[route[:3]] = true
		codes[route[4:]] = true
	}
	placesJSON := map[string]any{}
	for _, code := range slices.Sorted(maps.Keys(codes)) {
		if p, ok := in.Places[code]; ok {
			placesJSON[code] = placeJSON(p)
			if len(p.G) != 2 {
				// The map view silently skips coordinate-less places, so a
				// curated entry missing "g" should surface, not hide.
				in.Log.Warn("place-missing-coords", code)
			}
		} else {
			placesJSON[code] = map[string]any{"name": code}
			in.Log.Warn("unmapped-place-code", code)
		}
	}

	files := map[string][]byte{}
	put := func(path string, v any) error {
		b, err := emit.Canonical(v)
		if err != nil {
			return fmt.Errorf("serializing %s: %w", path, err)
		}
		files[path] = b
		return nil
	}

	availability := func(routes, places map[string]any) map[string]any {
		return map[string]any{
			"airlines": airlinesJSON, "days": days, "epoch": epochStr,
			"source": provenanceBlock, "places": places, "routes": routes,
			"schema": 1, "t": in.SourceTime, "v": in.SHA,
		}
	}
	if err := put("availability.json", availability(routesJSON, placesJSON)); err != nil {
		return nil, err
	}

	// Origin shards: identical shape, routes filtered to the origin, places
	// filtered to the codes those routes reference.
	origins := map[string]bool{}
	for route := range routeBits {
		origins[route[:3]] = true
	}
	for origin := range origins {
		oRoutes, oPlaces := map[string]any{}, map[string]any{}
		for route := range routeBits {
			if route[:3] != origin {
				continue
			}
			oRoutes[route] = routesJSON[route]
			oPlaces[route[:3]] = placesJSON[route[:3]]
			oPlaces[route[4:]] = placesJSON[route[4:]]
		}
		if err := put("origins/"+origin+".json", availability(oRoutes, oPlaces)); err != nil {
			return nil, err
		}
	}

	if err := put("places.json", map[string]any{
		"places": placesJSON, "schema": 1, "t": in.SourceTime, "v": in.SHA,
	}); err != nil {
		return nil, err
	}

	if err := put("manifest.json", map[string]any{
		"bundle": "availability.json", "changes": "changes/recent.json",
		"counts": map[string]any{
			"airlines": len(airlinesJSON), "places": len(placesJSON),
			"routeDates": routeDates, "routes": len(routesJSON),
		},
		"epoch": epochStr, "source": provenanceBlock, "mode": "bundle",
		"schema": 1, "t": in.SourceTime, "v": in.SHA,
	}); err != nil {
		return nil, err
	}

	// Flight detail: flights/<ORIG>/<DEST>/<YYYY-MM>.json, days keyed by
	// zero-padded day-of-month, all airlines merged per route-month.
	for route, byDate := range flightDays {
		byMonth := map[string]map[string]any{}
		for date, byAirline := range byDate {
			month, dayOfMonth := date[:7], date[8:10]
			if byMonth[month] == nil {
				byMonth[month] = map[string]any{}
			}
			dayJSON := map[string]any{}
			for id, flights := range byAirline {
				dayJSON[id] = flights
			}
			byMonth[month][dayOfMonth] = dayJSON
		}
		for month, daysMap := range byMonth {
			p := "flights/" + route[:3] + "/" + route[4:] + "/" + month + ".json"
			if err := put(p, map[string]any{
				"days": daysMap, "route": route, "schema": 1, "t": in.SourceTime, "v": in.SHA,
			}); err != nil {
				return nil, err
			}
		}
	}

	// Rolling changes feed (the only state-dependent output).
	changes := buildChanges(in.OldBundle, in.OldChanges, routeBits, cutoffDay, in.SourceTime)
	if err := put("changes/recent.json", map[string]any{
		"entries": changes, "schema": 1, "t": in.SourceTime, "v": in.SHA,
	}); err != nil {
		return nil, err
	}

	return &Output{
		Files:      files,
		Routes:     len(routesJSON),
		RouteDates: routeDates,
		Origins:    len(origins),
		Places:     len(placesJSON),
	}, nil
}

// placeJSON renders one curated place entry, omitting empty optional fields.
func placeJSON(p Place) map[string]any {
	m := map[string]any{"name": p.Name}
	if p.Country != "" {
		m["country"] = p.Country
	}
	if len(p.Search) > 0 {
		m["search"] = p.Search
	}
	if len(p.G) == 2 {
		m["g"] = p.G
	}
	return m
}

// flightJSON maps one source flight object to the derived schema. Cabins
// with zero seats are omitted; unknown cabin codes and negative seat counts
// are warned and skipped.
func flightJSON(f source.Flight, srcPath string, log *warnings.Log) map[string]any {
	seats := map[string]any{}
	for _, cabin := range slices.Sorted(maps.Keys(f.Seats)) {
		if _, ok := CabinBits[cabin]; !ok {
			log.Warn("unknown-cabin", cabin, srcPath)
			continue
		}
		if f.Seats[cabin] < 0 {
			// A negative count would otherwise poison both this file and
			// the bundle's "s" seat-threshold layer.
			log.Warn("negative-seats", cabin, srcPath)
			continue
		}
		if f.Seats[cabin] > 0 {
			seats[cabin] = f.Seats[cabin]
		}
	}
	var peak any
	if f.Peak != nil {
		peak = *f.Peak
	}
	return map[string]any{
		"arr": f.Arrive, "car": orEmpty(f.Carriers), "dep": f.Depart,
		"fn": orEmpty(f.FlightNumbers), "peak": peak, "rfs": f.RewardFlightSaver,
		"seats": seats, "via": orEmpty(f.Via),
	}
}

// orEmpty keeps absent string lists as [] rather than null in the output.
func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
