package alerts

import (
	"encoding/json"
	"fmt"
	"time"
)

// bundleState is the parsed availability bundle reduced to what alerting
// needs: per-route cabin bits per day, merged across airlines, in absolute
// day numbers (days since 1970-01-01 UTC).
type bundleState struct {
	t        int64             // source commit unix seconds ("data as of")
	epochDay int               // absolute day of the bundle epoch
	endDay   int               // one past the last encoded day (the horizon)
	merged   map[string][]byte // route -> cabin bits per day since epoch
}

// parseBundle extracts a bundleState from availability.json bytes. Cabin
// bits are OR-merged across airlines; airlines with a nibble width other
// than 1 are skipped, mirroring the site's decoder.
func parseBundle(raw []byte) (*bundleState, error) {
	var b struct {
		Epoch    string `json:"epoch"`
		T        int64  `json:"t"`
		Airlines map[string]struct {
			Width int `json:"width"`
		} `json:"airlines"`
		Routes map[string]struct {
			A map[string]string `json:"a"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	epochDay, err := parseDay(b.Epoch)
	if err != nil {
		return nil, fmt.Errorf("bundle epoch: %w", err)
	}
	merged := map[string][]byte{}
	maxLen := 0
	for route, entry := range b.Routes {
		var bits []byte
		for id, s := range entry.A {
			if a, ok := b.Airlines[id]; ok && a.Width != 0 && a.Width != 1 {
				continue // multi-nibble legend: not decodable here (or by the site)
			}
			if len(s) > len(bits) {
				grown := make([]byte, len(s))
				copy(grown, bits)
				bits = grown
			}
			for i := 0; i < len(s); i++ {
				if v := hexBits(s[i]); v > 0 {
					bits[i] |= byte(v)
				}
			}
		}
		merged[route] = bits
		if len(bits) > maxLen {
			maxLen = len(bits)
		}
	}
	return &bundleState{t: b.T, epochDay: epochDay, endDay: epochDay + maxLen, merged: merged}, nil
}

// cabinOrder is the canonical M W C F ordering, used everywhere cabins are
// enumerated so that output is deterministic.
var cabinOrder = []struct {
	bit    byte
	letter byte
}{{1, 'M'}, {2, 'W'}, {4, 'C'}, {8, 'F'}}

// reverseRoute flips ORIG-DEST; empty when the key is not route-shaped.
func reverseRoute(route string) string {
	if len(route) != 7 || route[3] != '-' {
		return ""
	}
	return route[4:] + "-" + route[:3]
}

const secondsPerDay = 86400

// parseDay converts "YYYY-MM-DD" to an absolute day number.
func parseDay(date string) (int, error) {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return 0, err
	}
	return int(t.Unix() / secondsPerDay), nil
}

// dayDate is the inverse of parseDay.
func dayDate(day int) string {
	return time.Unix(int64(day)*secondsPerDay, 0).UTC().Format("2006-01-02")
}

// unixDay returns the absolute day of the UTC date containing unix.
func unixDay(unix int64) int {
	if unix < 0 {
		return int(unix/secondsPerDay) - 1
	}
	return int(unix / secondsPerDay)
}

// hexBits decodes one availability nibble; -1 for a non-hex character.
func hexBits(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	}
	return -1
}
