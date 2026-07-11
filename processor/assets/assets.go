// Package assets holds static files embedded into the processor binary:
// the curated place table, the append-only airline registry, and the
// FORMAT.md documentation emitted into the derived data repository.
//
// This package is deliberately dumb: it exposes raw bytes only. Parsing
// lives with the code that defines the corresponding types.
package assets

import _ "embed"

// PlacesJSON is the curated IATA metro/city code -> place table
// (name, country, optional "search" alias list for multi-airport metros).
//
//go:embed places.json
var PlacesJSON []byte

// AirlinesJSON is the APPEND-ONLY airline registry: source directory slug ->
// {id, name, cabins}. Once assigned, an airline id is frozen forever (paths
// and keys in the derived repo depend on it); a later colliding airline gets
// its slug as its id.
//
//go:embed airlines.json
var AirlinesJSON []byte

// FormatMD is the FORMAT.md documentation file emitted verbatim into the
// derived data repository for consumers of the data.
//
//go:embed FORMAT.md
var FormatMD []byte
