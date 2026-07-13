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
	// seats: route -> one packed byte per day since epoch, decoded from the
	// optional "s" layer (2 hex chars per day; 2-bit monotone threshold code
	// per cabin, M=bits 0-1, W=2-3, C=4-5, F=6-7; 0 = no evidence of >=2
	// seats, 1 = >=2, 2 = >=3, 3 = >=4). Codes are MAX-merged across airlines
	// (a party rides ONE flight, never a sum) and masked by the day's merged
	// presence bits — "a" is the sole presence authority. A route ABSENT from
	// this map has no seat data at all: "unknown", which is distinct from a
	// present route whose codes are 0.
	seats map[string][]byte
}

// seatNeed converts a watch's MinSeats to the threshold code it requires:
// a party of N fits iff code >= N-1.
func seatNeed(minSeats int) byte { return byte(minSeats - 1) }

// seatCode extracts one cabin's 2-bit threshold code from a packed seat byte.
// cabinIdx is the M W C F rank (0..3).
func seatCode(packed byte, cabinIdx int) byte {
	return (packed >> (2 * cabinIdx)) & 3
}

// seatByteAt returns the packed seat byte for an absolute day on a route, or
// 0 (no evidence) when the route has no seats layer or the day is outside it.
func (b *bundleState) seatByteAt(route string, day int) byte {
	s, ok := b.seats[route]
	if !ok {
		return 0
	}
	i := day - b.epochDay
	if i < 0 || i >= len(s) {
		return 0
	}
	return s[i]
}

// satisfiedBits returns the cabins (as a presence-style bitmask) that hold a
// day's cabin bit AND meet the seat threshold for a party of minSeats. For
// minSeats <= 1 it is exactly the presence bits — the seats layer never
// touches single-passenger semantics.
func (b *bundleState) satisfiedBits(route string, day int, minSeats int) byte {
	var pres byte
	if bits, ok := b.merged[route]; ok {
		if i := day - b.epochDay; i >= 0 && i < len(bits) {
			pres = bits[i]
		}
	}
	if minSeats <= 1 {
		return pres
	}
	packed := b.seatByteAt(route, day)
	need := seatNeed(minSeats)
	var out byte
	for i, c := range cabinOrder {
		if pres&c.bit != 0 && seatCode(packed, i) >= need {
			out |= c.bit
		}
	}
	return out
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
			S map[string]string `json:"s"`
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
	seats := map[string][]byte{}
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

		// Optional seats layer, mirroring the site's decoder: only airlines
		// whose "a" contributes presence may contribute codes; 2 hex chars per
		// day; MAX-merge per cabin across airlines (equivalently, since the
		// threshold test is monotone, a per-cabin max of codes); finally mask
		// by the merged presence bits so a (malformed) seat string can never
		// claim a cabin the presence layer doesn't show.
		var packed []byte
		for id, s := range entry.S {
			if a, ok := b.Airlines[id]; ok && a.Width != 0 && a.Width != 1 {
				continue
			}
			if _, ok := entry.A[id]; !ok {
				continue // "a" is the sole presence authority
			}
			days := len(s) / 2
			if days > len(packed) {
				grown := make([]byte, days)
				copy(grown, packed)
				packed = grown
			}
			for i := 0; i < days; i++ {
				hi, lo := hexBits(s[2*i]), hexBits(s[2*i+1])
				if hi < 0 || lo < 0 {
					continue // not hex: contribute nothing for this day
				}
				v := byte(hi<<4 | lo)
				if v == 0 {
					continue
				}
				for c := 0; c < 4; c++ {
					if code := seatCode(v, c); code > seatCode(packed[i], c) {
						packed[i] = (packed[i] &^ (3 << (2 * c))) | (code << (2 * c))
					}
				}
			}
		}
		if packed != nil {
			for i := range packed {
				var pres byte
				if i < len(bits) {
					pres = bits[i]
				}
				for c, cab := range cabinOrder {
					if pres&cab.bit == 0 {
						packed[i] &^= 3 << (2 * c)
					}
				}
			}
			seats[route] = packed
		}
	}
	return &bundleState{t: b.T, epochDay: epochDay, endDay: epochDay + maxLen, merged: merged, seats: seats}, nil
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
