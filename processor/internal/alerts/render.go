package alerts

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/alertstore"
)

// Copy limits (ALERTS-SPEC §5).
const (
	maxListedPairs   = 4
	maxListedDates   = 6
	maxDigestWatches = 3
	siteURL          = "https://rewardflights.lucy.sh"
)

var cabinNames = map[string]string{
	"M": "Economy", "W": "Premium Economy", "C": "Business", "F": "First",
}

// group is one route+kind's worth of news, the unit a message talks about.
type group struct {
	route string
	kind  string
	items []item
}

// render turns a device's drained batch into one notification.
//
// The tag is unique per send, so notifications STACK rather than replace. With
// one push per device per hour, collapsing them would silently destroy dates
// the user had not acted on yet.
func render(items []item, overflow int, subKey string, now int64, today int) Publication {
	items = dedupe(items)
	sortItems(items)
	groups := groupItems(items)

	pub := Publication{Tag: fmt.Sprintf("rf_%s_%d", subKey[:min(8, len(subKey))], now)}
	if len(groups) == 1 {
		g := groups[0]
		pub.Title = groupTitle(g)
		pub.Body = groupBody(g, overflow, today)
		pub.URL = groupURL(g)
		return pub
	}

	// Digest: several routes moved for this person in one batch.
	pub.Title = fmt.Sprintf("Award space on %d of your routes", len(groups))
	pub.URL = siteURL + "/alerts"
	parts := make([]string, 0, maxDigestWatches)
	for _, g := range groups[:min(len(groups), maxDigestWatches)] {
		parts = append(parts, fmt.Sprintf("%s: %s", routeLabel(g), digestCount(g)))
	}
	body := strings.Join(parts, " · ")
	if extra := len(groups) - maxDigestWatches; extra > 0 {
		body += fmt.Sprintf(" · +%d more", extra)
	}
	pub.Body = body
	return pub
}

// dedupe removes news that two overlapping watches both found. When the same
// pair satisfied both a single-passenger and a party watch, the kept item
// carries the LOWEST threshold: "seats open" is true for everyone, "3+ seats"
// would over-claim scope for the 1-pax watch's audience.
func dedupe(items []item) []item {
	idx := map[string]int{}
	out := items[:0:0]
	for _, it := range items {
		k := it.dedupeKey()
		if j, ok := idx[k]; ok {
			if it.MinSeats < out[j].MinSeats {
				out[j].MinSeats = it.MinSeats
			}
			continue
		}
		idx[k] = len(out)
		out = append(out, it)
	}
	return out
}

// sortItems fixes a deterministic order: route, kind, cabin (M W C F), then
// chronologically.
func sortItems(items []item) {
	slices.SortFunc(items, func(a, b item) int {
		if c := strings.Compare(a.Route, b.Route); c != 0 {
			return c
		}
		if c := strings.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		if c := cabinRank(a.Cabin) - cabinRank(b.Cabin); c != 0 {
			return c
		}
		if c := strings.Compare(a.Out, b.Out); c != 0 {
			return c
		}
		return strings.Compare(a.Ret, b.Ret)
	})
}

func cabinRank(cabin string) int {
	for i, c := range cabinOrder {
		if string(c.letter) == cabin {
			return i
		}
	}
	return len(cabinOrder)
}

// groupItems buckets sorted items by route+kind, preserving order.
func groupItems(items []item) []group {
	var groups []group
	for _, it := range items {
		if n := len(groups); n > 0 && groups[n-1].route == it.Route && groups[n-1].kind == it.Kind {
			groups[n-1].items = append(groups[n-1].items, it)
			continue
		}
		groups = append(groups, group{route: it.Route, kind: it.Kind, items: []item{it}})
	}
	return groups
}

// cabinsIn lists the distinct cabins in a group, in M W C F order.
func cabinsIn(g group) []string {
	seen := map[string]bool{}
	var out []string
	for _, it := range g.items {
		if !seen[it.Cabin] {
			seen[it.Cabin] = true
			out = append(out, it.Cabin)
		}
	}
	slices.SortFunc(out, func(a, b string) int { return cabinRank(a) - cabinRank(b) })
	return out
}

// routeLabel renders LON ⇄ TYO for round trips, LON → TYO for one-ways.
func routeLabel(g group) string {
	orig, dest := g.route[:3], g.route[4:]
	if g.kind == alertstore.KindRT {
		return orig + " ⇄ " + dest
	}
	return orig + " → " + dest
}

func groupTitle(g group) string {
	cabins := cabinsIn(g)
	subject := "Award"
	if len(cabins) == 1 {
		subject = cabinNames[cabins[0]]
	}
	// Party news gets its qualifier in the title — the push IS the promise,
	// so "we found 3 seats together" must be said out loud. Only when every
	// item in the group is party news (dedupe already keeps the lowest
	// threshold per pair, so a mixed group stays unqualified and true for
	// everyone).
	if n := groupMinSeats(g); n >= 2 {
		if g.kind == alertstore.KindRT {
			return fmt.Sprintf("%s (%d+ seats) round trips open: %s", subject, n, routeLabel(g))
		}
		return fmt.Sprintf("%s (%d+ seats) open: %s", subject, n, routeLabel(g))
	}
	if g.kind == alertstore.KindRT {
		return fmt.Sprintf("%s round trips open: %s", subject, routeLabel(g))
	}
	return fmt.Sprintf("%s seats open: %s", subject, routeLabel(g))
}

// groupMinSeats is the smallest MinSeats across a group's items: >= 2 only
// when every item is party news.
func groupMinSeats(g group) int {
	n := g.items[0].MinSeats
	for _, it := range g.items {
		if it.MinSeats < n {
			n = it.MinSeats
		}
	}
	return n
}

// groupBody renders the dates. Round trips list PAIRS (the unit of news);
// one-ways list single dates with weekdays, which a round-trip pair has no
// room for.
func groupBody(g group, overflow, today int) string {
	cabins := cabinsIn(g)
	total := len(g.items) + overflow

	// Several cabins: label each run so "which cabin?" needs no tap.
	if len(cabins) > 1 {
		var parts []string
		shown := 0
		limit := maxListedPairs
		if g.kind == alertstore.KindOW {
			limit = maxListedDates
		}
		for _, cabin := range cabins {
			var dates []string
			for _, it := range g.items {
				if it.Cabin != cabin || shown >= limit {
					continue
				}
				dates = append(dates, renderItem(it, g.kind, today))
				shown++
			}
			if len(dates) > 0 {
				parts = append(parts, fmt.Sprintf("%s: %s", cabinNames[cabin], strings.Join(dates, ", ")))
			}
		}
		body := strings.Join(parts, " · ")
		if extra := total - shown; extra > 0 {
			body += fmt.Sprintf(", +%d more", extra)
		}
		return body
	}

	limit := maxListedPairs
	noun := "new"
	if g.kind == alertstore.KindOW {
		limit = maxListedDates
		noun = "new dates"
		if total == 1 {
			noun = "new date"
		}
	}
	var dates []string
	for _, it := range g.items[:min(len(g.items), limit)] {
		dates = append(dates, renderItem(it, g.kind, today))
	}
	body := fmt.Sprintf("%d %s: %s", total, noun, strings.Join(dates, ", "))
	if extra := total - len(dates); extra > 0 {
		body += fmt.Sprintf(", +%d more", extra)
	}
	return body
}

// digestCount renders "3 new Business" for one route in a digest.
func digestCount(g group) string {
	cabins := cabinsIn(g)
	label := "award"
	if len(cabins) == 1 {
		label = cabinNames[cabins[0]]
	}
	return fmt.Sprintf("%d new %s", len(g.items), label)
}

// groupURL deep-links the FIRST pair, so one tap lands on the pair-picker with
// that trip selected — the whole point of the message.
func groupURL(g group) string {
	first := g.items[0]
	if g.kind == alertstore.KindOW {
		return siteURL + "/route/" + g.route
	}
	return fmt.Sprintf("%s/trip/%s?nights=%d-%d&out=%s&ret=%s",
		siteURL, g.route, first.NMin, first.NMax, first.Out, first.Ret)
}

// renderItem renders one piece of news: a pair for round trips, a single
// weekday-prefixed date for one-ways.
func renderItem(it item, kind string, today int) string {
	if kind == alertstore.KindOW {
		return renderDate(it.Out, today)
	}
	return renderPair(it.Out, it.Ret, today)
}

// renderDate renders "Sun 12 Oct", appending the year only when it differs
// from today's (a date in another year is otherwise ambiguous).
func renderDate(iso string, today int) string {
	t, err := time.Parse("2006-01-02", iso)
	if err != nil {
		return iso
	}
	out := t.Format("Mon 2 Jan")
	if t.Year() != yearOf(today) {
		out += fmt.Sprintf(" %d", t.Year())
	}
	return out
}

// renderPair renders "12–19 Oct", "28 Sep–5 Oct" across a month boundary, and
// "28 Dec–4 Jan 2027" when the return lands in another year. Weekdays are
// dropped: on a pair they double the length and halve the readability.
func renderPair(outISO, retISO string, today int) string {
	out, err1 := time.Parse("2006-01-02", outISO)
	ret, err2 := time.Parse("2006-01-02", retISO)
	if err1 != nil || err2 != nil {
		return outISO + "–" + retISO
	}
	left := fmt.Sprintf("%d", out.Day())
	if out.Month() != ret.Month() || out.Year() != ret.Year() {
		left = out.Format("2 Jan")
	}
	right := ret.Format("2 Jan")
	if ret.Year() != yearOf(today) {
		right += fmt.Sprintf(" %d", ret.Year())
	}
	return left + "–" + right
}

// yearOf is the calendar year of an absolute day number.
func yearOf(day int) int {
	return time.Unix(int64(day)*secondsPerDay, 0).UTC().Year()
}
