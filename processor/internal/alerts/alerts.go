// Package alerts publishes Web Push notifications when new award availability
// opens on something a subscriber is watching (ALERTS-SPEC).
//
// Three ideas carry the design:
//
//  1. A round-trip alert is about PAIRS, not days. The unit of news is
//     "(outbound D, return R) is now bookable in cabin c".
//  2. Pair-newness reduces to leg-gains: a pair is newly satisfiable iff it is
//     satisfiable now AND at least one of its legs GAINED that cabin this
//     cycle. So we never enumerate pairs — we expand the handful of changed
//     leg-days into their nights window. Critically, this catches the case a
//     day-diffing implementation silently drops: a RETURN opening today pairs
//     with an outbound that has been available for weeks.
//  3. Availability transitions are global; the notification decision is
//     per-subscriber — but the flap cooldown is a property of the DATA, so it
//     is computed from a global ledger and no per-subscriber cooldown state
//     exists at all.
//
// All timing comes from the SOURCE commit timestamp embedded in the bundle,
// never the wall clock, so replaying a bundle sequence is deterministic and a
// restart's baseline cycle never alerts.
package alerts

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/alertstore"
	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/webpush"
)

// Tunables (ALERTS-SPEC §3, §4, §5).
const (
	StateSchema     = 2
	maxGainDays     = 2000 // EC-13 circuit breaker: a bulk source rewrite pages nobody
	maxPendingItems = 64   // per subscriber; the rest becomes an overflow count
	maxDailyPushes  = 24   // hard cap per device per UTC day
	slowCycleWarn   = 500 * time.Millisecond
	defaultCooldown = 3 * time.Hour
	defaultBatch    = time.Hour
)

// Config parameterizes an alert Watcher.
type Config struct {
	Store        *alertstore.Store // subscription store (required unless Publish is injected)
	VapidKeyPath string            // VAPID P-256 private key (PEM or base64url scalar)
	VapidSubject string            // VAPID "sub" claim
	StatePath    string            // JSON state file; empty keeps state in memory only
	Cooldown     time.Duration     // min off-time before a pair re-alerts (default 3h)
	Batch        time.Duration     // min interval between pushes per DEVICE (default 1h)
	Publish      PublishFunc       // injected in tests; nil builds the Web Push publisher
	Logf         func(format string, args ...any)
}

// Publication is one outgoing notification (one device's drained batch).
type Publication struct {
	Title string
	Body  string
	URL   string
	Tag   string
}

// PublishFunc delivers one publication to one subscription. An error means
// "not delivered": the batch is carried forward and retried next cycle.
type PublishFunc func(webpush.Subscription, Publication) error

// Watcher holds the cross-cycle alert state. The watch loop is its only
// writer; mu exists solely because the API server reads the current horizon
// from an HTTP goroutine.
type Watcher struct {
	cfg   Config
	state *stateData
	idx   *watchIndex // rebuilt only when the store's version changes
	ver   uint64
	dirty bool

	mu   sync.RWMutex
	prev *bundleState // previous cycle's bundle (nil until baseline)
}

// setPrev publishes the cycle's bundle for the API's horizon reads.
func (w *Watcher) setPrev(b *bundleState) {
	w.mu.Lock()
	w.prev = b
	w.mu.Unlock()
}

// stateData is the persisted alert state (schema 2).
//
// Both ledgers are keyed ROUTE|cabin|YYYY-MM-DD and sized by EVENTS IN THE
// LAST COOLDOWN WINDOW, not by the dataset — a few thousand entries, versus
// the ~200k of the per-(route,cabin,date) map this replaces.
type stateData struct {
	Schema   int                      `json:"schema"`
	OpenedAt map[string]int64         `json:"openedAt"` // start of the current open run
	ClosedAt map[string]int64         `json:"closedAt"` // end of the last open run
	Pending  map[string]*pendingBatch `json:"pending"`  // subKey -> undelivered items
	LastPub  map[string]int64         `json:"lastPub"`  // subKey -> last successful push
	PubDay   map[string]*pubDay       `json:"pubDay"`   // subKey -> pushes sent today
}

type pubDay struct {
	Day int `json:"day"`
	N   int `json:"n"`
}

type pendingBatch struct {
	Items    []item `json:"items"`
	Overflow int    `json:"overflow,omitempty"`
}

// item is one piece of news: a watch matched a (route, cabin, day[, return]).
// Days are stored as absolute ISO dates, never epoch offsets — the bundle
// epoch shifts at New Year and offsets would silently alias (EC-5).
type item struct {
	Watch string `json:"w"`
	Route string `json:"r"`
	Kind  string `json:"k"`
	Cabin string `json:"c"`
	Out   string `json:"o"`
	Ret   string `json:"e,omitempty"`
	NMin  int    `json:"nmin,omitempty"`
	NMax  int    `json:"nmax,omitempty"`
}

// dedupeKey identifies the NEWS, independent of which watch found it: two
// overlapping watches that match the same pair must not report it twice.
func (i item) dedupeKey() string {
	return i.Route + "|" + i.Kind + "|" + i.Cabin + "|" + i.Out + "|" + i.Ret
}

// watchIndex answers "which watches can be affected by a change on route R?".
// A round-trip watch on A-B is indexed under A-B *and* B-A, because a gain on
// the return leg creates new pairs for it.
type watchIndex struct {
	byRoute map[string][]watchRef
}

type watchRef struct {
	subKey   string
	endpoint string
	watch    alertstore.Watch
}

// NewWatcher builds a watcher, loading persisted state when StatePath names a
// readable, current-schema state file (anything else starts fresh).
func NewWatcher(cfg Config) (*Watcher, error) {
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = defaultCooldown
	}
	if cfg.Batch <= 0 {
		cfg.Batch = defaultBatch
	}
	if cfg.Logf == nil {
		cfg.Logf = func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
	}
	if cfg.Publish == nil {
		if cfg.Store == nil {
			return nil, fmt.Errorf("alerts: no subscription store and no injected publisher")
		}
		vapid, err := webpush.LoadVapid(cfg.VapidKeyPath, cfg.VapidSubject)
		if err != nil {
			return nil, fmt.Errorf("alerts: %w", err)
		}
		cfg.Publish = storePublisher(cfg.Store, vapid, cfg.Logf)
	}
	return &Watcher{cfg: cfg, state: loadState(cfg.StatePath, cfg.Logf)}, nil
}

// Baseline seeds the watcher from the bundle current at process start. It
// never alerts — nothing in the baseline is new — but it does flush any batch
// that was pending when we last exited.
func (w *Watcher) Baseline(raw []byte) {
	b, err := parseBundle(raw)
	if err != nil {
		w.cfg.Logf("WARN alert-bundle-unparseable baseline: %v", err)
		return
	}
	w.baseline(b)
}

func (w *Watcher) baseline(b *bundleState) {
	today := unixDay(b.t)
	w.refreshIndex()
	w.prune(b.t, today)
	w.flush(b.t, today)
	w.setPrev(b)
	w.save()
}

// Cycle processes one new bundle. It never returns an error: alerting is
// best-effort and must not disturb the watch loop.
func (w *Watcher) Cycle(raw []byte) {
	started := time.Now()
	b, err := parseBundle(raw)
	if err != nil {
		w.cfg.Logf("WARN alert-bundle-unparseable cycle: %v", err)
		return
	}
	if w.prev == nil {
		w.baseline(b)
		return
	}
	if b.t < w.prev.t {
		// EC-10: a rollback in the source repo. Re-baseline rather than do
		// arithmetic with a negative elapsed time.
		w.cfg.Logf("WARN alert-source-time-backwards %d -> %d; re-baselining", w.prev.t, b.t)
		w.baseline(b)
		return
	}

	now := b.t
	today := unixDay(now)
	w.refreshIndex()

	gains, losses, gainDays := diffBundles(w.prev, b, today)

	// EC-13: a source-repo rebuild or backfill must never page every user.
	if gainDays > maxGainDays {
		w.cfg.Logf("WARN alert-bulk-change %d gain-days (>%d): advancing ledger, publishing nothing",
			gainDays, maxGainDays)
		w.applyLosses(losses, now)
		w.applyGains(gains, now)
		w.prune(now, today)
		w.setPrev(b)
		w.save()
		return
	}

	// Order matters (§3.4): isFlap reads the ledger as of the START of this
	// cycle, so losses land first, evaluation runs, and only then do gains.
	w.applyLosses(losses, now)

	hits := map[string][]item{}
	for _, route := range slices.Sorted(maps.Keys(gains)) {
		for _, ref := range w.idx.byRoute[route] {
			for _, it := range w.evaluate(ref, route, gains, b, now, today) {
				hits[ref.subKey] = append(hits[ref.subKey], it)
			}
		}
	}

	w.applyGains(gains, now)

	for _, subKey := range slices.Sorted(maps.Keys(hits)) {
		w.addPending(subKey, hits[subKey])
	}
	w.prune(now, today)
	w.flush(now, today)
	w.setPrev(b)
	w.save()

	if elapsed := time.Since(started); elapsed > slowCycleWarn {
		w.cfg.Logf("WARN alert-slow-cycle %s (gain-days=%d routes=%d hits=%d)",
			elapsed, gainDays, len(gains), len(hits))
	}
}

// Horizon reports the data horizon the watcher currently holds: today's
// absolute day, one past the last encoded day, and the bundle's route set. The
// API uses it to compute each watch's status at read time. It is safe to call
// before the first bundle arrives (everything is zero / nil, and the API then
// skips the data-dependent checks).
//
// The returned route set is built fresh, so the caller cannot mutate the
// watcher's state — this is read from an HTTP goroutine while the watch loop
// may be writing.
func (w *Watcher) Horizon() (today, endDay int, routes map[string]bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.prev == nil {
		return 0, 0, nil
	}
	routes = make(map[string]bool, len(w.prev.merged))
	for route := range w.prev.merged {
		routes[route] = true
	}
	return unixDay(w.prev.t), w.prev.endDay, routes
}

// PurgeExpired drops long-dead watches (EC-1). The watch loop calls it once
// per cycle; it is cheap and bounded by the number of watches.
func (w *Watcher) PurgeExpired() {
	if w.cfg.Store == nil || w.prev == nil {
		return
	}
	if watches, subs := w.cfg.Store.PurgeExpired(unixDay(w.prev.t)); watches > 0 {
		w.cfg.Logf("alert-purge-expired removed %d watch(es), %d subscription(s)", watches, subs)
	}
}

// refreshIndex rebuilds the route -> watches index when the store has changed.
func (w *Watcher) refreshIndex() {
	if w.cfg.Store == nil {
		if w.idx == nil {
			w.idx = &watchIndex{byRoute: map[string][]watchRef{}}
		}
		return
	}
	ver := w.cfg.Store.Version()
	if w.idx != nil && ver == w.ver {
		return
	}
	idx := &watchIndex{byRoute: map[string][]watchRef{}}
	for _, sub := range w.cfg.Store.Snapshot() {
		subKey := alertstore.SubKey(sub.Endpoint)
		for _, watch := range sub.Watches {
			ref := watchRef{subKey: subKey, endpoint: sub.Endpoint, watch: watch}
			idx.byRoute[watch.Route] = append(idx.byRoute[watch.Route], ref)
			if watch.Kind == alertstore.KindRT {
				// A gain on the return leg makes new pairs for this watch.
				if rev := alertstore.ReverseRoute(watch.Route); rev != "" {
					idx.byRoute[rev] = append(idx.byRoute[rev], ref)
				}
			}
		}
	}
	w.idx = idx
	w.ver = ver
}

// gain is one day's worth of newly-available cabins on one route.
type gain struct {
	day  int
	bits byte
}

// diffBundles computes per-route cabin gains and losses between two bundles.
//
// Losses are only meaningful for days present in both bundles. Gains extend
// to the new bundle's full horizon: days past the old horizon are "frontier"
// gains, which EC-3 admits only for watches with an explicitly bounded
// outbound range (the user's own signal that they care about those dates).
func diffBundles(prev, b *bundleState, today int) (gains, losses map[string][]gain, gainDays int) {
	gains, losses = map[string][]gain{}, map[string][]gain{}
	lo := max(today, b.epochDay, prev.epochDay)
	lossHi := min(prev.endDay, b.endDay)

	for _, route := range slices.Sorted(maps.Keys(b.merged)) {
		newBits := b.merged[route]
		oldBits, known := prev.merged[route]
		if !known {
			continue // EC-11: a brand-new route baselines, it does not alert
		}
		for d := lo; d < b.endDay; d++ {
			var o byte
			if d < prev.endDay {
				o = oldBits[d-prev.epochDay]
			}
			n := newBits[d-b.epochDay]
			if g := n &^ o; g != 0 {
				gains[route] = append(gains[route], gain{day: d, bits: g})
				gainDays++
			}
			if d < lossHi {
				if l := o &^ n; l != 0 {
					losses[route] = append(losses[route], gain{day: d, bits: l})
				}
			}
		}
		if len(gains[route]) == 0 {
			delete(gains, route)
		}
		if len(losses[route]) == 0 {
			delete(losses, route)
		}
	}
	return gains, losses, gainDays
}

// evaluate expands the gains on one dirty route into the news one watch cares
// about. This is the heart of the detector (§3.3).
func (w *Watcher) evaluate(ref watchRef, dirty string, gains map[string][]gain, b *bundleState, now int64, today int) []item {
	watch := ref.watch
	if watch.Expired(today) || watch.Impossible() {
		return nil
	}
	out, ok := b.merged[watch.Route]
	if !ok {
		return nil // unknown route (EC-11)
	}
	mask := watch.Mask()
	outRange := watch.OutDays(today, b.endDay)

	if watch.Kind == alertstore.KindOW {
		if dirty != watch.Route {
			return nil
		}
		var items []item
		for _, g := range gains[dirty] {
			if !w.frontierOK(g.day, watch) || g.day < outRange.From || g.day > outRange.To {
				continue
			}
			for _, cabin := range cabinsOf(g.bits & mask) {
				if !w.isFlap(watch.Route, g.day, "", 0, cabin, now) {
					items = append(items, item{
						Watch: watch.ID, Route: watch.Route, Kind: watch.Kind,
						Cabin: cabin, Out: dayDate(g.day),
					})
				}
			}
		}
		return items
	}

	// Round trip: both legs must hold the SAME cabin, and the return must land
	// inside the nights window.
	rev := alertstore.ReverseRoute(watch.Route)
	ret, hasRet := b.merged[rev]
	if !hasRet {
		return nil // EC-7: no reverse route, can never fire
	}
	retRange := watch.RetDays(today, b.endDay)
	nmin, nmax := watch.NightsWindow()
	seen := map[string]bool{}
	var items []item

	add := func(d, r int, cabin string) {
		if w.isFlap(watch.Route, d, rev, r, cabin, now) {
			return
		}
		it := item{
			Watch: watch.ID, Route: watch.Route, Kind: watch.Kind, Cabin: cabin,
			Out: dayDate(d), Ret: dayDate(r), NMin: nmin, NMax: nmax,
		}
		// A pair whose BOTH legs gained is found by loop (a) and loop (b).
		if k := it.dedupeKey(); !seen[k] {
			seen[k] = true
			items = append(items, it)
		}
	}

	// (a) the OUTBOUND leg gained: pair each gained day with returns that
	// already hold the same cabin.
	if dirty == watch.Route {
		for _, g := range gains[dirty] {
			d := g.day
			if !w.frontierOK(d, watch) || d < outRange.From || d > outRange.To {
				continue
			}
			gained := g.bits & mask
			if gained == 0 {
				continue
			}
			from := max(d+nmin, retRange.From)
			to := min(d+nmax, retRange.To, b.epochDay+len(ret)-1)
			for r := from; r <= to; r++ {
				for _, cabin := range cabinsOf(gained & ret[r-b.epochDay]) {
					add(d, r, cabin)
				}
			}
		}
	}

	// (b) the RETURN leg gained: pair each gained return with outbound days
	// that may have held the cabin for WEEKS. This is the crux case — nothing
	// "opened" on the outbound, yet the PAIR is new.
	//
	// The engine deployed before 2026-07 had no equivalent of this loop: it
	// diffed each route's set of available outbound days, so an outbound that
	// was already open produced no event and the round trip a return-leg
	// opening had just made bookable was dropped silently, forever. Those were
	// real trips real subscribers never heard about. See
	// TestProductionEngineMissedReturnGainPairs.
	if dirty == rev {
		for _, g := range gains[dirty] {
			r := g.day
			if !w.frontierOK(r, watch) || r < retRange.From || r > retRange.To {
				continue
			}
			gained := g.bits & mask
			if gained == 0 {
				continue
			}
			from := max(r-nmax, outRange.From)
			to := min(r-nmin, outRange.To, b.epochDay+len(out)-1)
			for d := from; d <= to; d++ {
				for _, cabin := range cabinsOf(gained & out[d-b.epochDay]) {
					add(d, r, cabin)
				}
			}
		}
	}
	return items
}

// frontierOK implements EC-3: a gain beyond the previous horizon is the
// booking window crawling forward, not news — EXCEPT for a watch whose
// outbound range is explicitly bounded, where "the day my dates load" is
// exactly what the user asked to hear about. The 180-day range cap bounds the
// blast radius.
func (w *Watcher) frontierOK(day int, watch alertstore.Watch) bool {
	if day < w.prev.endDay {
		return true
	}
	return watch.BoundedOut()
}

// cabinsOf lists the cabin letters in a bitmask, in canonical M W C F order.
func cabinsOf(bits byte) []string {
	var out []string
	for _, c := range cabinOrder {
		if bits&c.bit != 0 {
			out = append(out, string(c.letter))
		}
	}
	return out
}

// --- the global transition ledger (§3.4) --------------------------------

func ledgerKey(route string, day int, cabin string) string {
	return route + "|" + cabin + "|" + dayDate(day)
}

func (w *Watcher) applyLosses(losses map[string][]gain, now int64) {
	for _, route := range slices.Sorted(maps.Keys(losses)) {
		for _, l := range losses[route] {
			for _, cabin := range cabinsOf(l.bits) {
				k := ledgerKey(route, l.day, cabin)
				w.state.ClosedAt[k] = now
				delete(w.state.OpenedAt, k)
				w.dirty = true
			}
		}
	}
}

func (w *Watcher) applyGains(gains map[string][]gain, now int64) {
	for _, route := range slices.Sorted(maps.Keys(gains)) {
		for _, g := range gains[route] {
			for _, cabin := range cabinsOf(g.bits) {
				w.state.OpenedAt[ledgerKey(route, g.day, cabin)] = now
				w.dirty = true
			}
		}
	}
}

// isFlap reports whether this pair's joint availability was merely blinking:
// it ended a previous joint run less than Cooldown ago.
//
// The subtlety that makes this correct — and that a naive "cooldown on gains"
// gets catastrophically wrong — is the partner check. If a leg closed at t1
// but its partner only OPENED after t1, then no joint run ended at t1: the
// pair has never been announced, and suppressing it would silence it forever.
func (w *Watcher) isFlap(outRoute string, d int, retRoute string, r int, cabin string, now int64) bool {
	l1 := ledgerKey(outRoute, d, cabin)
	l2 := ""
	if retRoute != "" {
		l2 = ledgerKey(retRoute, r, cabin)
	}

	var end int64
	found := false
	consider := func(leg, other string) {
		closed, ok := w.state.ClosedAt[leg]
		if !ok {
			return // this leg has no recent close
		}
		if other != "" {
			// Was the partner open when this leg closed? If it opened later,
			// the two were never jointly available, so no joint run ended here.
			if opened, ok := w.state.OpenedAt[other]; ok && opened > closed {
				return
			}
		}
		if !found || closed > end {
			end, found = closed, true
		}
	}
	consider(l1, l2)
	if l2 != "" {
		consider(l2, l1)
	}
	return found && now-end < int64(w.cfg.Cooldown/time.Second)
}

// --- pending, batching, publishing (§4, §5) ------------------------------

// addPending queues news for a device. Items are deduped per (watch, news):
// the same pair found by two of the user's overlapping watches is kept once
// PER WATCH — the message collapses it (render dedupes on the news alone), but
// both watches must still be recorded as having fired, which is what the
// /alerts page's "last alert" line reads.
func (w *Watcher) addPending(subKey string, items []item) {
	batch := w.state.Pending[subKey]
	if batch == nil {
		batch = &pendingBatch{}
		w.state.Pending[subKey] = batch
	}
	seen := map[string]bool{}
	for _, it := range batch.Items {
		seen[it.Watch+"|"+it.dedupeKey()] = true
	}
	for _, it := range items {
		k := it.Watch + "|" + it.dedupeKey()
		if seen[k] {
			continue
		}
		seen[k] = true
		if len(batch.Items) >= maxPendingItems {
			batch.Overflow++
			continue
		}
		batch.Items = append(batch.Items, it)
	}
	w.dirty = true
}

// prune keeps the state bounded: ledger entries older than the cooldown or for
// past dates, and pending/lastPub for subscriptions that no longer exist.
func (w *Watcher) prune(now int64, today int) {
	cooldown := int64(w.cfg.Cooldown / time.Second)
	for _, ledger := range []map[string]int64{w.state.OpenedAt, w.state.ClosedAt} {
		for k, t := range ledger {
			if now-t > cooldown || ledgerDatePast(k, today) {
				delete(ledger, k)
				w.dirty = true
			}
		}
	}
	batch := int64(w.cfg.Batch / time.Second)
	for subKey := range w.state.Pending {
		if !w.known(subKey) {
			delete(w.state.Pending, subKey)
			w.dirty = true
		}
	}
	for subKey, t := range w.state.LastPub {
		if !w.known(subKey) || now-t > batch {
			delete(w.state.LastPub, subKey)
			w.dirty = true
		}
	}
	for subKey, pd := range w.state.PubDay {
		if !w.known(subKey) || pd.Day != today {
			delete(w.state.PubDay, subKey)
			w.dirty = true
		}
	}
}

// known reports whether a subscription key is still in the store (a 404/410
// removal or an unsubscribe makes its state garbage).
func (w *Watcher) known(subKey string) bool {
	if w.cfg.Store == nil {
		return true
	}
	_, ok := w.cfg.Store.LookupKey(subKey)
	return ok
}

// ledgerDatePast reports whether a ledger key's date has rolled into the past.
func ledgerDatePast(k string, today int) bool {
	i := strings.LastIndexByte(k, '|')
	if i < 0 {
		return true
	}
	day, err := parseDay(k[i+1:])
	return err != nil || day < today
}

// flush publishes every pending batch whose device is due one: at most one
// push per device per Batch, first-after-a-quiet-hour immediately, and never
// more than maxDailyPushes in a UTC day.
func (w *Watcher) flush(now int64, today int) {
	batchSecs := int64(w.cfg.Batch / time.Second)
	for _, subKey := range slices.Sorted(maps.Keys(w.state.Pending)) {
		pending := w.state.Pending[subKey]

		sub, ok := w.subscription(subKey)
		if !ok {
			delete(w.state.Pending, subKey)
			w.dirty = true
			continue
		}
		if last, ok := w.state.LastPub[subKey]; ok && now-last < batchSecs {
			continue // inside the batch window: merge and wait
		}
		if pd := w.state.PubDay[subKey]; pd != nil && pd.Day == today && pd.N >= maxDailyPushes {
			continue // daily cap: hold the batch, it goes out after midnight
		}

		// Drop news whose date has passed while it sat in the batch.
		items := make([]item, 0, len(pending.Items))
		for _, it := range pending.Items {
			if day, err := parseDay(it.Out); err == nil && day >= today {
				items = append(items, it)
			}
		}
		if len(items) == 0 {
			delete(w.state.Pending, subKey)
			w.dirty = true
			continue
		}

		pub := render(items, pending.Overflow, subKey, now, today)
		if err := w.cfg.Publish(sub, pub); err != nil {
			w.cfg.Logf("WARN alert-publish-failed %s: %v", sub.Endpoint, err)
			continue // keep pending; the ledger still advanced (today's contract)
		}

		w.state.LastPub[subKey] = now
		pd := w.state.PubDay[subKey]
		if pd == nil || pd.Day != today {
			pd = &pubDay{Day: today}
			w.state.PubDay[subKey] = pd
		}
		pd.N++
		delete(w.state.Pending, subKey)
		w.dirty = true

		if w.cfg.Store != nil {
			w.cfg.Store.MarkFired(sub.Endpoint, watchIDs(items), now)
		}
	}
}

func (w *Watcher) subscription(subKey string) (webpush.Subscription, bool) {
	if w.cfg.Store == nil {
		return webpush.Subscription{Endpoint: subKey}, true // test-only path
	}
	return w.cfg.Store.LookupKey(subKey)
}

func watchIDs(items []item) []string {
	seen := map[string]bool{}
	var ids []string
	for _, it := range items {
		if !seen[it.Watch] {
			seen[it.Watch] = true
			ids = append(ids, it.Watch)
		}
	}
	return ids
}

// --- persistence ---------------------------------------------------------

func loadState(path string, logf func(string, ...any)) *stateData {
	fresh := &stateData{
		Schema:   StateSchema,
		OpenedAt: map[string]int64{},
		ClosedAt: map[string]int64{},
		Pending:  map[string]*pendingBatch{},
		LastPub:  map[string]int64{},
		PubDay:   map[string]*pubDay{},
	}
	if path == "" {
		return fresh
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fresh
	}
	var s stateData
	if json.Unmarshal(raw, &s) != nil || s.Schema != StateSchema {
		// A schema bump starts fresh: cooldown history and any pending batch
		// are lost exactly once, which is bounded and one-off.
		logf("alert-state-reset %s (schema %d -> %d)", path, s.Schema, StateSchema)
		return fresh
	}
	if s.OpenedAt == nil {
		s.OpenedAt = map[string]int64{}
	}
	if s.ClosedAt == nil {
		s.ClosedAt = map[string]int64{}
	}
	if s.Pending == nil {
		s.Pending = map[string]*pendingBatch{}
	}
	if s.LastPub == nil {
		s.LastPub = map[string]int64{}
	}
	if s.PubDay == nil {
		s.PubDay = map[string]*pubDay{}
	}
	return &s
}

// save writes the state file atomically, at most once per cycle and only when
// something changed.
func (w *Watcher) save() {
	if !w.dirty || w.cfg.StatePath == "" {
		return
	}
	raw, err := json.Marshal(w.state)
	if err != nil {
		w.cfg.Logf("WARN alert-state-save %s: %v", w.cfg.StatePath, err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(w.cfg.StatePath), 0o755); err != nil {
		w.cfg.Logf("WARN alert-state-save %s: %v", w.cfg.StatePath, err)
		return
	}
	tmp := w.cfg.StatePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		w.cfg.Logf("WARN alert-state-save %s: %v", w.cfg.StatePath, err)
		return
	}
	if err := os.Rename(tmp, w.cfg.StatePath); err != nil {
		os.Remove(tmp)
		w.cfg.Logf("WARN alert-state-save %s: %v", w.cfg.StatePath, err)
		return
	}
	w.dirty = false
}

// --- the push transport --------------------------------------------------

// pushSender delivers to the push services. It is a package var so tests can
// point its transport at a local server while exercising real endpoint URLs.
var pushSender = &http.Client{Timeout: webpush.SendTimeout}

// storePublisher returns the real Web Push publish func. A dead subscription
// (404/410) is removed from the store; any other failure is reported so the
// batch is retried.
func storePublisher(store *alertstore.Store, vapid *webpush.Vapid, logf func(string, ...any)) PublishFunc {
	sender := &webpush.Sender{Client: pushSender, Vapid: vapid}
	return func(sub webpush.Subscription, p Publication) error {
		payload, err := json.Marshal(map[string]string{
			"title": p.Title, "body": p.Body, "url": p.URL, "tag": p.Tag,
		})
		if err != nil {
			return err
		}
		status, err := sender.Send(sub, payload)
		switch {
		case err != nil:
			return err
		case webpush.Expired(status):
			logf("alert-push-expired %s (status %d), removing", sub.Endpoint, status)
			store.Remove(sub.Endpoint)
			return nil // gone for good: nothing to retry
		case status < 200 || status >= 300:
			return fmt.Errorf("push service returned %d", status)
		}
		return nil
	}
}
