// Package alerts publishes Web Push notifications when new award
// availability opens, mirroring the site's model: browsers subscribe to
// topics rf_{ROUTE}_{ow|rt}_{M|W|C|F}, and the watcher encrypts one batched
// message per topic (RFC 8291, VAPID) whenever dates enter that topic's
// "on" set.
//
// Subscriptions live in a local store (internal/alertstore) owned by this
// same process: the watcher is the sender, so if the host is down alerts do
// not fire regardless of where subscriptions are kept.
//
// The watcher compares the previous cycle's availability bundle against the
// new one. All detection timing (cooldown, batching, pruning) is driven by
// the SOURCE commit timestamp embedded in the bundle — never the wall clock —
// so a replay of the same bundle sequence behaves identically, and the boot
// baseline produces no alerts. Cooldown/batch/pending state persists across
// restarts in a small JSON file.
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
	"time"

	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/alertstore"
	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/webpush"
)

// Config parameterizes an alert Watcher. Zero durations/window take the
// documented defaults; Publish and Logf default to the real Web Push
// publisher and stderr logging.
type Config struct {
	Store        *alertstore.Store // local subscription store (required unless Publish is injected)
	VapidKeyPath string            // file holding the VAPID P-256 private key (PEM or base64url scalar)
	VapidSubject string            // VAPID "sub" claim, e.g. mailto:alerts@rewardflights.lucy.sh
	StatePath    string            // JSON state file; empty keeps state in memory only
	Cooldown     time.Duration     // min off-time before a day re-alerts (default 3h)
	Batch        time.Duration     // min interval between publishes per topic (default 1h)
	Window       int               // round-trip return window in nights (default 30)
	Publish      PublishFunc       // injected in tests; nil builds the Web Push publisher
	Logf         func(format string, args ...any)
}

// Publication is one outgoing notification (one topic's drained batch).
type Publication struct {
	Topic string
	Title string
	Body  string
	URL   string
}

// PublishFunc delivers one publication; an error means "not delivered"
// (pending dates are carried forward and retried next cycle).
type PublishFunc func(Publication) error

// Watcher holds the cross-cycle alert state. Not safe for concurrent use;
// the watch loop calls it from a single goroutine.
type Watcher struct {
	cfg   Config
	prev  *bundleState // previous cycle's parsed bundle (nil until baseline)
	state *stateData
	dirty bool
}

// stateData is the persisted state file (schema 1).
type stateData struct {
	Schema  int                 `json:"schema"`
	LastOn  map[string]int64    `json:"lastOn"`  // "typ|ROUTE|cabin|YYYY-MM-DD" -> unix seconds last seen on
	LastPub map[string]int64    `json:"lastPub"` // topic -> unix seconds of last successful publish
	Pending map[string][]string `json:"pending"` // topic -> sorted unique ISO dates awaiting publish
}

// setKey identifies one alert stream: an availability type + route + cabin.
type setKey struct {
	typ   string // "ow" | "rt"
	route string
	cabin byte // 'M', 'W', 'C', 'F'
}

func (k setKey) topic() string {
	return "rf_" + k.route + "_" + k.typ + "_" + string(k.cabin)
}

func (k setKey) stateKey(day int) string {
	return k.typ + "|" + k.route + "|" + string(k.cabin) + "|" + dayDate(day)
}

var cabinNames = map[string]string{
	"M": "Economy", "W": "Premium Economy", "C": "Business", "F": "First",
}

// cabinBits is scanned in MWCF order for deterministic output.
var cabinBits = []struct {
	bit    byte
	letter byte
}{{1, 'M'}, {2, 'W'}, {4, 'C'}, {8, 'F'}}

// lastOnRetentionDays is how long a day's last-on timestamp outlives the day
// itself before being pruned from the state file.
const lastOnRetentionDays = 7

// NewWatcher builds a watcher, loading persisted state when StatePath names
// a readable, well-formed state file (anything else starts fresh). Without
// an injected Publish func it wires up the real Web Push publisher, which
// requires a subscription Store and a loadable VAPID key.
func NewWatcher(cfg Config) (*Watcher, error) {
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 3 * time.Hour
	}
	if cfg.Batch <= 0 {
		cfg.Batch = time.Hour
	}
	if cfg.Window <= 0 {
		cfg.Window = 30
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
	return &Watcher{cfg: cfg, state: loadState(cfg.StatePath)}, nil
}

// Baseline seeds the watcher from the bundle that is current at process
// start. It records the on-sets (so flaps across a restart stay inside the
// cooldown) and flushes any pending batches restored from the state file,
// but never alerts: nothing in the baseline is "new".
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
	w.markOn(computeOnSets(b, today, b.endDay, w.cfg.Window), b.t)
	w.pruneLastOn(today)
	w.flush(b.t, today)
	w.prev = b
	w.save()
}

// Cycle processes one new bundle: diff against the previous cycle's bundle,
// queue alerts for days entering an on-set (subject to the cooldown), then
// publish due batches. It never returns an error — alerting is best-effort
// and must not disturb the watch loop.
func (w *Watcher) Cycle(raw []byte) {
	b, err := parseBundle(raw)
	if err != nil {
		w.cfg.Logf("WARN alert-bundle-unparseable cycle: %v", err)
		return
	}
	if w.prev == nil {
		w.baseline(b)
		return
	}

	now := b.t
	today := unixDay(now)
	// The previous on-sets span the old bundle's own horizon; the new
	// on-sets are clipped to that SAME old horizon so booking-window growth
	// at the edge is never "newly opened" (next cycle it is part of prev).
	prevOn := computeOnSets(w.prev, today, w.prev.endDay, w.cfg.Window)
	newOn := computeOnSets(b, today, w.prev.endDay, w.cfg.Window)

	cooldown := int64(w.cfg.Cooldown / time.Second)
	for _, key := range sortedKeys(newOn) {
		prevSet := prevOn[key]
		var fresh []int
		for _, d := range sortedDays(newOn[key]) {
			if prevSet[d] {
				continue // was already on: not a transition
			}
			if last, ok := w.state.LastOn[key.stateKey(d)]; ok && now-last < cooldown {
				continue // flapped off and back within the cooldown
			}
			fresh = append(fresh, d)
		}
		if len(fresh) > 0 {
			w.addPending(key.topic(), fresh)
		}
	}

	w.markOn(newOn, now)
	w.pruneLastOn(today)
	w.flush(now, today)
	w.prev = b
	w.save()
}

// markOn refreshes the last-on timestamp of every current on-set member.
// A day's timestamp freezes when it closes, so "now - lastOn" measures how
// long it has been off.
func (w *Watcher) markOn(on map[setKey]map[int]bool, now int64) {
	for key, days := range on {
		for d := range days {
			w.state.LastOn[key.stateKey(d)] = now
			w.dirty = true
		}
	}
}

// pruneLastOn drops last-on entries more than lastOnRetentionDays past their
// date, bounding the state file.
func (w *Watcher) pruneLastOn(today int) {
	for k := range w.state.LastOn {
		date := k[strings.LastIndexByte(k, '|')+1:]
		day, err := parseDay(date)
		if err != nil || day+lastOnRetentionDays < today {
			delete(w.state.LastOn, k)
			w.dirty = true
		}
	}
}

// addPending merges newly alerting days into a topic's pending date list.
func (w *Watcher) addPending(topic string, days []int) {
	set := map[string]bool{}
	for _, date := range w.state.Pending[topic] {
		set[date] = true
	}
	for _, d := range days {
		set[dayDate(d)] = true
	}
	w.state.Pending[topic] = slices.Sorted(maps.Keys(set))
	w.dirty = true
}

// flush publishes every pending topic whose batch interval has elapsed,
// draining its dates into one message. Failed publishes are logged and the
// pending dates carried forward; stale (past) dates are dropped.
func (w *Watcher) flush(now int64, today int) {
	batch := int64(w.cfg.Batch / time.Second)
	for _, topic := range slices.Sorted(maps.Keys(w.state.Pending)) {
		if last, ok := w.state.LastPub[topic]; ok && now-last < batch {
			continue
		}
		var dates []string
		for _, date := range w.state.Pending[topic] {
			if day, err := parseDay(date); err == nil && day >= today {
				dates = append(dates, date)
			}
		}
		if len(dates) == 0 {
			delete(w.state.Pending, topic)
			w.dirty = true
			continue
		}
		pub, err := buildPublication(topic, dates)
		if err != nil {
			w.cfg.Logf("WARN alert-bad-topic %s: %v", topic, err)
			delete(w.state.Pending, topic)
			w.dirty = true
			continue
		}
		if err := w.cfg.Publish(pub); err != nil {
			w.cfg.Logf("WARN alert-publish-failed %s: %v", topic, err)
			continue // keep pending; retry next cycle
		}
		w.state.LastPub[topic] = now
		delete(w.state.Pending, topic)
		w.dirty = true
	}
}

// maxListedDates caps how many dates one message spells out.
const maxListedDates = 6

// buildPublication renders the ntfy message for a topic and its dates.
func buildPublication(topic string, dates []string) (Publication, error) {
	parts := strings.Split(topic, "_")
	if len(parts) != 4 || parts[0] != "rf" || len(parts[1]) != 7 {
		return Publication{}, fmt.Errorf("malformed topic")
	}
	route, typ, cabin := parts[1], parts[2], parts[3]
	name, ok := cabinNames[cabin]
	if !ok || (typ != "ow" && typ != "rt") {
		return Publication{}, fmt.Errorf("malformed topic")
	}
	orig, dest := route[:3], route[4:]

	pub := Publication{Topic: topic}
	if typ == "rt" {
		pub.Title = name + " round trip open: " + orig + " ⇄ " + dest
		pub.URL = "https://rewardflights.lucy.sh/trip/" + route
	} else {
		pub.Title = name + " seats open: " + orig + " → " + dest
		pub.URL = "https://rewardflights.lucy.sh/route/" + route
	}

	shown := make([]string, 0, min(len(dates), maxListedDates))
	for _, date := range dates[:min(len(dates), maxListedDates)] {
		day, err := parseDay(date)
		if err != nil {
			return Publication{}, fmt.Errorf("bad pending date %q", date)
		}
		shown = append(shown, time.Unix(int64(day)*86400, 0).UTC().Format("Mon 2 Jan"))
	}
	noun := "new dates"
	if len(dates) == 1 {
		noun = "new date"
	}
	pub.Body = fmt.Sprintf("%d %s: %s", len(dates), noun, strings.Join(shown, ", "))
	if extra := len(dates) - maxListedDates; extra > 0 {
		pub.Body += fmt.Sprintf(", +%d more", extra)
	}
	return pub, nil
}

// pushSender delivers to the push services. It is a package var so tests can
// point its transport at a local server while exercising real endpoint URLs.
var pushSender = &http.Client{Timeout: webpush.SendTimeout}

// storePublisher returns the real Web Push publish func: it reads the
// topic's subscribers straight from the local store, encrypts the payload per
// RFC 8291 for each, and POSTs to each push endpoint with VAPID auth.
//
// Error contract: publishing never returns an error, because there is no
// longer a fallible "fetch the subscribers" step — the store is in-process.
// Per-endpoint failures are logged; dead subscriptions (404/410 from the push
// service) are removed from the store; a rare missed notification is preferred
// over a re-send storm, so the batch is always considered drained.
func storePublisher(store *alertstore.Store, vapid *webpush.Vapid, logf func(string, ...any)) PublishFunc {
	sender := &webpush.Sender{Client: pushSender, Vapid: vapid}
	return func(p Publication) error {
		subs := store.Get(p.Topic)
		if len(subs) == 0 {
			return nil // nobody subscribed to this topic
		}
		payload, err := json.Marshal(map[string]string{
			"title": p.Title, "body": p.Body, "url": p.URL, "tag": p.Topic,
		})
		if err != nil {
			return err
		}
		for _, sub := range subs {
			status, err := sender.Send(sub, payload)
			switch {
			case err != nil:
				logf("WARN alert-push-failed %s: %v", sub.Endpoint, err)
			case webpush.Expired(status):
				// The browser is gone for good: the push service tells us so.
				logf("alert-push-expired %s (status %d), removing", sub.Endpoint, status)
				store.Remove(sub.Endpoint)
			case status < 200 || status >= 300:
				logf("WARN alert-push-failed %s: status %d", sub.Endpoint, status)
			}
		}
		return nil
	}
}

// loadState reads the persisted state; anything missing or malformed starts
// fresh (the next cycle then behaves like a boot baseline for batching).
func loadState(path string) *stateData {
	fresh := &stateData{
		Schema:  1,
		LastOn:  map[string]int64{},
		LastPub: map[string]int64{},
		Pending: map[string][]string{},
	}
	if path == "" {
		return fresh
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fresh
	}
	var s stateData
	if json.Unmarshal(raw, &s) != nil || s.Schema != 1 {
		return fresh
	}
	if s.LastOn == nil {
		s.LastOn = map[string]int64{}
	}
	if s.LastPub == nil {
		s.LastPub = map[string]int64{}
	}
	if s.Pending == nil {
		s.Pending = map[string][]string{}
	}
	return &s
}

// save writes the state file atomically (tmp+rename), at most once per cycle
// and only when something changed.
func (w *Watcher) save() {
	if !w.dirty || w.cfg.StatePath == "" {
		return
	}
	raw, err := json.Marshal(w.state)
	if err != nil {
		w.cfg.Logf("WARN alert-state-save %s: %v", w.cfg.StatePath, err)
		return
	}
	tmp := w.cfg.StatePath + ".tmp"
	if err := os.MkdirAll(filepath.Dir(w.cfg.StatePath), 0o755); err != nil {
		w.cfg.Logf("WARN alert-state-save %s: %v", w.cfg.StatePath, err)
		return
	}
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		w.cfg.Logf("WARN alert-state-save %s: %v", w.cfg.StatePath, err)
		return
	}
	if err := os.Rename(tmp, w.cfg.StatePath); err != nil {
		w.cfg.Logf("WARN alert-state-save %s: %v", w.cfg.StatePath, err)
		return
	}
	w.dirty = false
}

func sortedKeys(on map[setKey]map[int]bool) []setKey {
	keys := make([]setKey, 0, len(on))
	for k := range on {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b setKey) int {
		if c := strings.Compare(a.route, b.route); c != 0 {
			return c
		}
		if c := strings.Compare(a.typ, b.typ); c != 0 {
			return c
		}
		return int(a.cabin) - int(b.cabin)
	})
	return keys
}

func sortedDays(set map[int]bool) []int {
	return slices.Sorted(maps.Keys(set))
}
