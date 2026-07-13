package alertstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Watch limits (ALERTS-SPEC §1.3, §6).
const (
	MaxWatchesPerSub = 20  // v2 watches per subscription
	MaxTopicsPerSub  = 60  // legacy topic path, retained for stale clients
	MaxRangeDays     = 180 // span of a bounded date range
	MaxFutureDays    = 800 // absurdity guard on range starts
	MaxLeadDays      = 365 // "I need up to a year's notice" — cap on lead time
	MaxMinSeats      = 4   // top party-size threshold the seats layer encodes ("4+")
	MinNights        = 1
	MaxNights        = 60
	DefaultNightsMin = 1  // matches NIGHTS_DEFAULT in app.js
	DefaultNightsMax = 30 // ...and the legacy Window=30
	ExpiryGraceDays  = 30 // expired this long ago -> purged
)

// Watch kinds.
const (
	KindRT = "rt"
	KindOW = "ow"
)

var (
	routeRe = regexp.MustCompile(`^[A-Z]{3}-[A-Z]{3}$`)
	dateRe  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// cabinBit maps a cabin letter to its availability bit; cabinRank gives the
// canonical M<W<C<F ordering used everywhere a cabin list is rendered.
var (
	cabinBit  = map[string]byte{"M": 1, "W": 2, "C": 4, "F": 8}
	cabinRank = map[string]int{"M": 0, "W": 1, "C": 2, "F": 3}
)

// Range is an inclusive date range. An omitted endpoint means "unbounded on
// that side" — expressed by omitting the key, never by a sentinel date.
type Range struct {
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

// Nights is the round-trip length window, in nights.
type Nights struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// Watch is one thing a person wants to be told about.
type Watch struct {
	ID     string   `json:"id"`
	Route  string   `json:"route"`
	Kind   string   `json:"kind"`   // "rt" | "ow"
	Cabins []string `json:"cabins"` // non-empty subset of M W C F, sorted M<W<C<F
	Out    *Range   `json:"out,omitempty"`
	Ret    *Range   `json:"ret,omitempty"` // rt only
	Nights *Nights  `json:"nights,omitempty"`
	// LeadDays: "I can travel any time, but not sooner than N days from now" —
	// a ROLLING floor on the outbound date. Stored as a relative offset, never
	// an absolute date, so it re-anchors to "today" on every detection cycle
	// instead of going stale (which is why it's safe where a fixed rolling
	// window would not be). 0 = no lead requirement. Mutually exclusive with an
	// explicit outbound range.
	LeadDays int `json:"leadDays,omitempty"`
	// MinSeats: "we travel as a party of N" — the watch fires only when there
	// is evidence of at least N award seats together, in the same cabin, on
	// ONE flight, on BOTH legs. 0 (the only stored spelling of "no
	// constraint") means today's presence-bit behaviour; valid constrained
	// values are 2..MaxMinSeats. 1 is rejected at Normalize so the zero value
	// stays the single canonical spelling and content ids stay stable.
	MinSeats    int   `json:"minSeats,omitempty"`
	CreatedAt   int64 `json:"createdAt,omitempty"`
	LastFiredAt int64 `json:"lastFiredAt,omitempty"`
}

// Watch statuses, computed at read time (never stored).
const (
	StatusActive       = "active"
	StatusExpired      = "expired"
	StatusImpossible   = "impossible"
	StatusNoReturn     = "no-return"
	StatusUnknownRoute = "unknown-route"
)

// Normalize validates a watch's structure, canonicalizes it, and derives its
// content-addressed id. Date-relative rules (not in the past, not absurdly
// far out) belong to the API layer, which owns the clock; everything here is
// time-independent so a stored watch always reloads cleanly.
func Normalize(w Watch) (Watch, error) {
	if !routeRe.MatchString(w.Route) {
		return Watch{}, fmt.Errorf("route %q must look like LON-TYO", w.Route)
	}
	if w.Kind != KindRT && w.Kind != KindOW {
		return Watch{}, fmt.Errorf("kind must be %q or %q", KindRT, KindOW)
	}

	cabins := map[string]bool{}
	for _, c := range w.Cabins {
		if _, ok := cabinBit[c]; !ok {
			return Watch{}, fmt.Errorf("cabin %q must be one of M, W, C, F", c)
		}
		cabins[c] = true
	}
	if len(cabins) == 0 {
		return Watch{}, errors.New("at least one cabin is required")
	}
	w.Cabins = slices.SortedFunc(maps.Keys(cabins), func(a, b string) int {
		return cabinRank[a] - cabinRank[b]
	})

	if w.Kind == KindOW {
		if w.Ret != nil {
			return Watch{}, errors.New("a one-way watch cannot have a return range")
		}
		if w.Nights != nil {
			return Watch{}, errors.New("a one-way watch cannot have a nights window")
		}
	}
	if w.Nights != nil {
		if w.Nights.Min < MinNights || w.Nights.Max > MaxNights || w.Nights.Min > w.Nights.Max {
			return Watch{}, fmt.Errorf("nights must satisfy %d <= min <= max <= %d", MinNights, MaxNights)
		}
	}
	if w.LeadDays < 0 || w.LeadDays > MaxLeadDays {
		return Watch{}, fmt.Errorf("lead time must be between 0 and %d days", MaxLeadDays)
	}
	switch {
	case w.MinSeats < 0:
		return Watch{}, errors.New("minSeats cannot be negative")
	case w.MinSeats == 1:
		// The zero value is the only spelling of "one passenger": accepting an
		// explicit 1 would mint a second id for the same watch content.
		return Watch{}, errors.New("minSeats 1 is the default; omit it for a single passenger")
	case w.MinSeats > MaxMinSeats:
		return Watch{}, fmt.Errorf("minSeats must be between 2 and %d (%d means \"%d or more\")",
			MaxMinSeats, MaxMinSeats, MaxMinSeats)
	}
	if w.LeadDays > 0 && w.Out != nil {
		// "Any time with N days' notice" and "specific outbound dates" are two
		// ways of saying when you can travel; combining them is contradictory.
		return Watch{}, errors.New("lead time cannot be combined with specific outbound dates")
	}

	for name, r := range map[string]*Range{"out": w.Out, "ret": w.Ret} {
		if r == nil {
			continue
		}
		if *r == (Range{}) {
			return Watch{}, fmt.Errorf("%s range is empty; omit it for \"any time\"", name)
		}
		from, hasFrom, err := parseRangeDay(r.From, name, "from")
		if err != nil {
			return Watch{}, err
		}
		to, hasTo, err := parseRangeDay(r.To, name, "to")
		if err != nil {
			return Watch{}, err
		}
		if hasFrom && hasTo {
			if from > to {
				return Watch{}, fmt.Errorf("%s range starts after it ends", name)
			}
			if to-from > MaxRangeDays {
				return Watch{}, fmt.Errorf("%s range must span at most %d days; use \"any time\" if you are flexible", name, MaxRangeDays)
			}
		}
	}

	// Feasibility (EC-4): a watch that can never fire is a bug, not a
	// preference. Reject it at save rather than with eternal silence.
	if err := w.feasible(); err != nil {
		return Watch{}, err
	}

	w.ID = watchID(w)
	return w, nil
}

// feasible reports whether the out/ret/nights constraints can ever coincide.
func (w Watch) feasible() error {
	if w.Kind != KindRT || w.Out == nil || w.Ret == nil {
		return nil
	}
	nmin, nmax := w.NightsWindow()
	outFrom, hasOutFrom, _ := parseRangeDay(w.Out.From, "out", "from")
	outTo, hasOutTo, _ := parseRangeDay(w.Out.To, "out", "to")
	retFrom, hasRetFrom, _ := parseRangeDay(w.Ret.From, "ret", "from")
	retTo, hasRetTo, _ := parseRangeDay(w.Ret.To, "ret", "to")

	if hasOutFrom && hasRetTo && retTo < outFrom+nmin {
		return fmt.Errorf("your return window ends before your outbound plus %d nights", nmin)
	}
	if hasOutTo && hasRetFrom && retFrom > outTo+nmax {
		return fmt.Errorf("your return window starts more than %d nights after your last outbound day", nmax)
	}
	return nil
}

// Impossible re-checks feasibility (defence in depth for stored watches).
func (w Watch) Impossible() bool { return w.feasible() != nil }

// watchID is sha256(content)[:8] — content-addressed, so re-saving the same
// watch is idempotent and duplicates collapse without a dedupe pass.
func watchID(w Watch) string {
	var b strings.Builder
	b.WriteString(w.Route)
	b.WriteByte('|')
	b.WriteString(w.Kind)
	b.WriteByte('|')
	b.WriteString(strings.Join(w.Cabins, ""))
	b.WriteByte('|')
	b.WriteString(rangeKey(w.Out))
	b.WriteByte('|')
	b.WriteString(rangeKey(w.Ret))
	b.WriteByte('|')
	if w.Nights != nil {
		fmt.Fprintf(&b, "%d-%d", w.Nights.Min, w.Nights.Max)
	}
	b.WriteByte('|')
	if w.LeadDays > 0 {
		fmt.Fprintf(&b, "L%d", w.LeadDays)
	}
	// Conditional fold, separator INCLUDED (the LeadDays "L%d" precedent):
	// a MinSeats-less watch hashes byte-identically to the pre-feature
	// formula, so every stored id — and everything keyed off it (carryHistory,
	// MarkFired, pending items, the client's rf:seen baselines) — survives the
	// deploy. Pinned by TestMinSeatsIDStability.
	if w.MinSeats >= 2 {
		fmt.Fprintf(&b, "|S%d", w.MinSeats)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:8]
}

func rangeKey(r *Range) string {
	if r == nil {
		return ""
	}
	return r.From + ".." + r.To
}

// Mask is the watch's cabin bitmask.
func (w Watch) Mask() byte {
	var m byte
	for _, c := range w.Cabins {
		m |= cabinBit[c]
	}
	return m
}

// NightsWindow returns the effective nights window (defaults applied).
func (w Watch) NightsWindow() (int, int) {
	if w.Nights != nil {
		return w.Nights.Min, w.Nights.Max
	}
	return DefaultNightsMin, DefaultNightsMax
}

// BoundedOut reports whether the user pinned an explicit outbound end date.
// This is the signal that they care about specific dates, and it is what
// entitles the watch to horizon-frontier alerts (EC-3).
func (w Watch) BoundedOut() bool { return w.Out != nil && w.Out.To != "" }

// TopicRepresentable reports whether a watch can be expressed as legacy
// topics: fully unbounded, with the default nights window. Constrained
// watches are invisible to the legacy API, which is what protects them from
// a stale client (§2.2).
func (w Watch) TopicRepresentable() bool {
	if w.Out != nil || w.Ret != nil {
		return false
	}
	if w.MinSeats >= 2 {
		// A stale client cannot express a party constraint, so it must not be
		// able to see — or, via UpsertTopics' replace, destroy — such a watch.
		return false
	}
	if w.Nights == nil {
		return true
	}
	return w.Nights.Min == DefaultNightsMin && w.Nights.Max == DefaultNightsMax
}

// Topics projects a topic-representable watch to legacy topic strings.
func (w Watch) Topics() []string {
	out := make([]string, 0, len(w.Cabins))
	for _, c := range w.Cabins {
		out = append(out, "rf_"+w.Route+"_"+w.Kind+"_"+c)
	}
	return out
}

// Expired reports whether every range in the watch lies in the past.
func (w Watch) Expired(today int) bool {
	if w.Out != nil && w.Out.To != "" {
		if to, ok, _ := parseRangeDay(w.Out.To, "out", "to"); ok && to < today {
			return true
		}
	}
	if w.Kind == KindRT && w.Ret != nil && w.Ret.To != "" {
		if to, ok, _ := parseRangeDay(w.Ret.To, "ret", "to"); ok && to < today {
			return true
		}
	}
	return false
}

// ExpiredSince reports how many days ago the watch expired (0 if it has not).
func (w Watch) ExpiredSince(today int) int {
	last := 0
	for _, r := range []*Range{w.Out, w.Ret} {
		if r == nil || r.To == "" {
			continue
		}
		if to, ok, _ := parseRangeDay(r.To, "r", "to"); ok && (last == 0 || to > last) {
			last = to
		}
	}
	if last == 0 || last >= today {
		return 0
	}
	return today - last
}

// Status computes the watch's state against the current data. today/endDay
// are absolute day numbers and routes is the bundle's route set; a nil routes
// map means "no bundle yet", so route-dependent states are skipped.
func (w Watch) Status(today int, routes map[string]bool) string {
	if routes != nil {
		if !routes[w.Route] {
			return StatusUnknownRoute
		}
		if w.Kind == KindRT && !routes[ReverseRoute(w.Route)] {
			return StatusNoReturn
		}
	}
	if w.Impossible() {
		return StatusImpossible
	}
	if w.Expired(today) {
		return StatusExpired
	}
	return StatusActive
}

// ReverseRoute flips ORIG-DEST; empty when the key is not route-shaped.
func ReverseRoute(route string) string {
	if len(route) != 7 || route[3] != '-' {
		return ""
	}
	return route[4:] + "-" + route[:3]
}

// DayRange is an inclusive range of absolute day numbers.
type DayRange struct{ From, To int }

// Empty reports whether the range contains no days.
func (r DayRange) Empty() bool { return r.From > r.To }

// OutDays returns the watch's outbound range clamped to [today, endDay-1].
// An unbounded side takes the clamp bound, which is exactly the legacy
// "any time" semantics. A LeadDays requirement raises the floor to
// today+LeadDays — resolved here against the current "today", so the window
// rolls forward each cycle rather than freezing at save time.
func (w Watch) OutDays(today, endDay int) DayRange {
	r := clampRange(w.Out, today, endDay-1)
	if w.LeadDays > 0 {
		if floor := today + w.LeadDays; floor > r.From {
			r.From = floor
		}
	}
	return r
}

// RetDays returns the watch's return range clamped to [today, endDay-1]. An
// absent return range is unbounded: the nights window alone constrains the
// pairing, which is precisely the legacy joint condition.
func (w Watch) RetDays(today, endDay int) DayRange {
	return clampRange(w.Ret, today, endDay-1)
}

func clampRange(r *Range, lo, hi int) DayRange {
	out := DayRange{From: lo, To: hi}
	if r == nil {
		return out
	}
	if from, ok, _ := parseRangeDay(r.From, "r", "from"); ok && from > out.From {
		out.From = from
	}
	if to, ok, _ := parseRangeDay(r.To, "r", "to"); ok && to < out.To {
		out.To = to
	}
	return out
}

// parseRangeDay parses an optional YYYY-MM-DD bound into an absolute day.
// The ok result distinguishes "absent" from "present"; a present-but-invalid
// date is an error (rejects 2026-02-31, which the regexp alone accepts).
func parseRangeDay(s, field, side string) (int, bool, error) {
	if s == "" {
		return 0, false, nil
	}
	if !dateRe.MatchString(s) {
		return 0, false, fmt.Errorf("%s.%s must be a YYYY-MM-DD date", field, side)
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return 0, false, fmt.Errorf("%s.%s is not a real date", field, side)
	}
	return int(t.Unix() / 86400), true, nil
}

// ParseDay converts YYYY-MM-DD to an absolute day number (days since epoch).
func ParseDay(s string) (int, error) {
	day, ok, err := parseRangeDay(s, "date", "value")
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, errors.New("empty date")
	}
	return day, nil
}

// DayDate is the inverse of ParseDay.
func DayDate(day int) string {
	return time.Unix(int64(day)*86400, 0).UTC().Format("2006-01-02")
}
