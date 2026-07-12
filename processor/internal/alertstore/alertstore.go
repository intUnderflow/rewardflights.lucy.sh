// Package alertstore is the watcher's local Web Push subscription store.
//
// The watcher is the sender, so it owns the subscriptions: if the host is
// down, alerts do not fire regardless of where the subscriptions live, and
// keeping them local costs nothing and avoids an external write-rate cap.
//
// A subscription holds Watches (ALERTS-SPEC §1): route + kind + cabins, with
// optional date ranges and a nights window. Schema 1 stored flat topic
// strings; those load losslessly into unbounded watches (§2.1) and survive
// only as an API projection for stale clients (§2.2).
//
// The store is a single JSON file mirrored in memory. Reads are cheap and
// concurrent; writes are serialized, debounced (at most one file write per
// debounce interval), and always flushed before the process exits.
//
// Knowing a push endpoint IS the capability to manage it (the standard Web
// Push model), which is why nothing else about a subscriber is stored and
// no authentication is required to manage a subscription.
package alertstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/webpush"
)

// Subscription is one browser push subscription and everything it watches.
//
// The delivery-telemetry fields exist because a push service returns 201
// whether the notification was shown or silently dropped by the OS, and no web
// API exposes that suppression — so a device's alerts can be dead for weeks
// with nobody the wiser. The service worker ACKs each push it actually
// receives (POST /ack); comparing lastAckAt against lastPushAt is the only
// signal we have that a device is still really reachable.
type Subscription struct {
	Endpoint string  `json:"endpoint"`
	P256dh   string  `json:"p256dh"`
	Auth     string  `json:"auth"`
	Watches  []Watch `json:"watches"`

	LastPushAt int64 `json:"lastPushAt,omitempty"` // last push accepted by the push service
	LastAckAt  int64 `json:"lastAckAt,omitempty"`  // last ACK from the service worker
	PushCount  int64 `json:"pushCount,omitempty"`
	AckCount   int64 `json:"ackCount,omitempty"`
}

// Schema is the current on-disk store schema.
const Schema = 2

const (
	DefaultMaxSubs = 100000
	// DefaultMaxBytes caps the on-disk subscription store. A backstop against a
	// runaway or hostile writer filling the host's disk, not a working limit:
	// 100k subscriptions is on the order of 50 MB, so this leaves ~40x headroom
	// while staying a small fraction of the host's free space.
	DefaultMaxBytes  = 2 << 30 // 2 GiB
	defaultDebounce  = time.Second
	subscriptionFile = 0o600 // subscriptions are personal data: owner-only
)

// pushHosts is the allowlist of push-service hosts we will ever send to.
// It bounds the store to real push services: an attacker cannot register a
// host they control and turn the watcher into an HTTP request amplifier.
var pushHosts = []string{
	"push.apple.com",            // *.push.apple.com
	"fcm.googleapis.com",        // exact
	"notify.windows.com",        // *.notify.windows.com
	"push.services.mozilla.com", // *.push.services.mozilla.com (incl. updates.)
}

// Validation errors. Callers map these to HTTP status codes.
var (
	ErrBadEndpoint   = errors.New("endpoint must be an https URL on a known push service")
	ErrBadTopic      = errors.New("topic must match rf_ORIG-DEST_(ow|rt)_[MWCF]")
	ErrTooManyTopics = fmt.Errorf("a subscription may hold at most %d topics", MaxTopicsPerSub)
	ErrTooManyWatch  = fmt.Errorf("a subscription may hold at most %d watches", MaxWatchesPerSub)
	ErrFull          = errors.New("subscription store is full")
	ErrStoreTooLarge = errors.New("subscription store has reached its size limit")
)

// Store is the concurrency-safe subscription store.
type Store struct {
	path     string
	maxSubs  int
	maxBytes int64
	bytes    int64            // running serialized size; guarded by mu
	subBytes map[string]int64 // per-subscription serialized size; guarded by mu
	debounce time.Duration
	now      func() time.Time
	logf     func(string, ...any)

	// LOCK ORDER: mu, then writeMu. Never the reverse.
	//
	// The mutating paths hold mu and then take writeMu (via touch → markDirty),
	// so anything that holds writeMu must not wait on mu — that is an AB-BA
	// deadlock, and it would wedge the whole watcher (not just alerts) the first
	// time a debounced flush landed during a subscribe. Flush therefore snapshots
	// under mu, releases it, and only then takes writeMu to do the file write.
	mu      sync.RWMutex
	subs    map[string]*Subscription // subKey -> subscription
	version uint64                   // bumped on every mutation; drives index rebuilds

	writeMu        sync.Mutex // serializes file writes; see LOCK ORDER above
	writtenVersion uint64     // last version persisted; dirty ⇔ version > this
	lastWrite      time.Time
	flushTimer     *time.Timer
	closed         bool
}

// Options configure a Store. Zero values take documented defaults.
type Options struct {
	Path     string // JSON file; required
	MaxSubs  int    // default DefaultMaxSubs
	MaxBytes int64  // default DefaultMaxBytes; hard ceiling on the store file
	Debounce time.Duration
	Now      func() time.Time // injected in tests
	Logf     func(string, ...any)
}

// Open loads the store from disk. A missing file starts empty; a corrupt file
// is moved aside (.corrupt) and the store starts empty, because losing
// subscriptions is recoverable (browsers re-subscribe) but refusing to boot is
// not. A schema-1 file is backed up, then migrated to watches (§2.1).
func Open(opts Options) (*Store, error) {
	if opts.Path == "" {
		return nil, errors.New("alertstore: no path")
	}
	if opts.MaxSubs <= 0 {
		opts.MaxSubs = DefaultMaxSubs
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.Debounce <= 0 {
		opts.Debounce = defaultDebounce
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	s := &Store{
		path:     opts.Path,
		maxSubs:  opts.MaxSubs,
		maxBytes: opts.MaxBytes,
		subBytes: map[string]int64{},
		debounce: opts.Debounce,
		now:      opts.Now,
		logf:     opts.Logf,
		subs:     map[string]*Subscription{},
	}
	if err := os.MkdirAll(filepath.Dir(opts.Path), 0o700); err != nil {
		return nil, fmt.Errorf("alertstore: %w", err)
	}
	raw, err := os.ReadFile(opts.Path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("alertstore: %w", err)
	}

	var file storeFile
	if err := json.Unmarshal(raw, &file); err != nil || (file.Schema != 1 && file.Schema != Schema) {
		aside := opts.Path + ".corrupt"
		if renameErr := os.Rename(opts.Path, aside); renameErr == nil {
			s.logf("WARN alert-store-corrupt moved %s aside; starting empty", opts.Path)
		}
		return s, nil
	}

	if file.Schema == 1 {
		// Preserve the v1 file before it can ever be rewritten (§1.2). A
		// failure here is fatal: quietly destroying subscriptions is not an
		// acceptable outcome of a migration.
		backup := opts.Path + ".v1.bak"
		if _, err := os.Stat(backup); os.IsNotExist(err) {
			if err := os.WriteFile(backup, raw, subscriptionFile); err != nil {
				return nil, fmt.Errorf("alertstore: backing up schema-1 store: %w", err)
			}
		}
		s.logf("alert-store-migrate schema 1 -> %d (%d subscriptions, backup %s)",
			Schema, len(file.Subs), backup)
	}

	for _, sub := range file.Subs {
		loaded, err := s.load(sub, file.Schema)
		if err != nil {
			s.logf("WARN alert-store-dropped %s: %v", sub.Endpoint, err)
			continue
		}
		lk := key(loaded.Endpoint)
		s.setSub(lk, loaded, subSize(loaded))
	}
	if file.Schema == 1 {
		s.version++ // version > writtenVersion(0) ⇒ rewritten in schema 2 at the next flush
	}
	return s, nil
}

// storeFile is the on-disk shape. Topics is the schema-1 remnant: read during
// migration, never written.
type storeFile struct {
	Schema int         `json:"schema"`
	Subs   []storedSub `json:"subs"`
}

type storedSub struct {
	Endpoint string   `json:"endpoint"`
	P256dh   string   `json:"p256dh"`
	Auth     string   `json:"auth"`
	Watches  []Watch  `json:"watches,omitempty"`
	Topics   []string `json:"topics,omitempty"` // schema 1 only

	LastPushAt int64 `json:"lastPushAt,omitempty"`
	LastAckAt  int64 `json:"lastAckAt,omitempty"`
	PushCount  int64 `json:"pushCount,omitempty"`
	AckCount   int64 `json:"ackCount,omitempty"`
}

// toStored projects a Subscription to its on-disk shape. Used by both Flush
// and subSize, so what we persist and what we byte-account never drift.
func toStored(sub *Subscription) storedSub {
	return storedSub{
		Endpoint: sub.Endpoint, P256dh: sub.P256dh, Auth: sub.Auth, Watches: sub.Watches,
		LastPushAt: sub.LastPushAt, LastAckAt: sub.LastAckAt,
		PushCount: sub.PushCount, AckCount: sub.AckCount,
	}
}

// load validates and (for schema 1) migrates one stored subscription.
func (s *Store) load(sub storedSub, schema int) (*Subscription, error) {
	if err := ValidEndpoint(sub.Endpoint); err != nil {
		return nil, err
	}
	if sub.P256dh == "" || sub.Auth == "" {
		return nil, errors.New("p256dh and auth are required")
	}
	out := &Subscription{
		Endpoint: sub.Endpoint, P256dh: sub.P256dh, Auth: sub.Auth,
		LastPushAt: sub.LastPushAt, LastAckAt: sub.LastAckAt,
		PushCount: sub.PushCount, AckCount: sub.AckCount,
	}

	if schema == 1 {
		watches, err := watchesFromTopics(sub.Topics, s.now().Unix())
		if err != nil {
			return nil, err
		}
		out.Watches = watches
		return out, nil
	}
	for _, w := range sub.Watches {
		// Re-validate on load: a hand-edited file must not smuggle in a bad
		// watch, and the rules may have tightened since it was written.
		normalized, err := Normalize(w)
		if err != nil {
			s.logf("WARN alert-store-dropped-watch %s: %v", sub.Endpoint, err)
			continue
		}
		normalized.CreatedAt = w.CreatedAt
		normalized.LastFiredAt = w.LastFiredAt
		out.Watches = append(out.Watches, normalized)
	}
	return out, nil
}

// watchesFromTopics is the schema-1 migration (§2.1): group topics by
// (route, kind), union their cabins, and emit one unbounded watch per group.
//
// This is an exact identity, not an approximation. An old rt topic meant
// "some return in [D+1, D+30]", which is precisely an unbounded watch with
// the default nights window; an old ow topic meant "day D has cabin c", which
// is an unbounded one-way watch.
func watchesFromTopics(topics []string, createdAt int64) ([]Watch, error) {
	type group struct{ route, kind string }
	cabins := map[group]map[string]bool{}
	for _, topic := range topics {
		route, kind, cabin, err := parseTopic(topic)
		if err != nil {
			return nil, err
		}
		g := group{route, kind}
		if cabins[g] == nil {
			cabins[g] = map[string]bool{}
		}
		cabins[g][cabin] = true
	}
	// Deterministic output regardless of the topic list's order.
	groups := slices.SortedFunc(maps.Keys(cabins), func(a, b group) int {
		if c := strings.Compare(a.route, b.route); c != 0 {
			return c
		}
		return strings.Compare(a.kind, b.kind)
	})

	var watches []Watch
	for _, g := range groups {
		w, err := Normalize(Watch{
			Route:  g.route,
			Kind:   g.kind,
			Cabins: slices.Collect(maps.Keys(cabins[g])),
		})
		if err != nil {
			return nil, err
		}
		w.CreatedAt = createdAt
		watches = append(watches, w)
	}
	return watches, nil
}

// parseTopic splits a legacy topic string rf_{ROUTE}_{ow|rt}_{cabin}.
func parseTopic(topic string) (route, kind, cabin string, err error) {
	parts := strings.Split(topic, "_")
	if len(parts) != 4 || parts[0] != "rf" ||
		!routeRe.MatchString(parts[1]) ||
		(parts[2] != KindOW && parts[2] != KindRT) ||
		len(parts[3]) != 1 || cabinBit[parts[3]] == 0 {
		return "", "", "", fmt.Errorf("%w: %q", ErrBadTopic, topic)
	}
	return parts[1], parts[2], parts[3], nil
}

// key derives the subscription key for an endpoint (hashed: opaque in logs,
// a stable identity, and the alert state keys off the same value).
func key(endpoint string) string {
	sum := sha256.Sum256([]byte(endpoint))
	return hex.EncodeToString(sum[:])
}

// SubKey exposes the subscription key for an endpoint (the alert state's key).
func SubKey(endpoint string) string { return key(endpoint) }

// Version reports a counter bumped on every mutation. The watcher rebuilds its
// watch index only when this changes.
func (s *Store) Version() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.version
}

// Snapshot returns a deep copy of every subscription, for index building.
func (s *Store) Snapshot() []Subscription {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Subscription, 0, len(s.subs))
	for _, k := range slices.Sorted(maps.Keys(s.subs)) {
		out = append(out, *cloneSub(s.subs[k]))
	}
	return out
}

func cloneSub(sub *Subscription) *Subscription {
	clone := *sub
	clone.Watches = make([]Watch, len(sub.Watches))
	for i, w := range sub.Watches {
		clone.Watches[i] = cloneWatch(w)
	}
	return &clone
}

func cloneWatch(w Watch) Watch {
	clone := w
	clone.Cabins = slices.Clone(w.Cabins)
	if w.Out != nil {
		r := *w.Out
		clone.Out = &r
	}
	if w.Ret != nil {
		r := *w.Ret
		clone.Ret = &r
	}
	if w.Nights != nil {
		n := *w.Nights
		clone.Nights = &n
	}
	return clone
}

// Upsert registers a subscription, REPLACING its entire watch list (the v2
// write). createdAt/lastFiredAt carry over for watch ids that already exist,
// so editing one watch does not reset the others' history.
func (s *Store) Upsert(sub Subscription) ([]Watch, error) {
	if err := ValidEndpoint(sub.Endpoint); err != nil {
		return nil, err
	}
	if sub.P256dh == "" || sub.Auth == "" {
		return nil, errors.New("p256dh and auth are required")
	}
	if len(sub.Watches) > MaxWatchesPerSub {
		return nil, ErrTooManyWatch
	}
	watches, err := normalizeAll(sub.Watches)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(sub.Endpoint)
	existing, exists := s.subs[k]
	if !exists && len(s.subs) >= s.maxSubs {
		return nil, ErrFull
	}
	watches = s.carryHistory(existing, watches)
	next := &Subscription{
		Endpoint: sub.Endpoint, P256dh: sub.P256dh, Auth: sub.Auth, Watches: watches,
	}

	// Hard disk backstop: never let the store grow past its byte ceiling. A
	// write that shrinks (or is size-neutral) is always allowed, so a subscriber
	// can still edit or delete their way out of a full store — only growth is
	// refused.
	size := subSize(next)
	if grown := storeWrapperBytes + s.bytes - s.subBytes[k] + size; grown > s.maxBytes && size > s.subBytes[k] {
		s.logf("WARN alert-store-size-limit %d bytes (limit %d): refusing to grow", s.bytes, s.maxBytes)
		return nil, ErrStoreTooLarge
	}
	s.setSub(k, next, size)
	s.touch()
	return slices.Clone(watches), nil
}

// storeWrapperBytes is the file's fixed overhead outside the subscription list
// ({"schema": 2, "subs": [...]}), reserved against the ceiling.
const storeWrapperBytes = 64

// subSize is a subscription's cost IN THE FILE, used for the store's byte
// ceiling. It must never under-count — an accounting that runs low would let the
// file exceed the limit, which defeats the point of a disk guard — so it
// measures the same indented encoding Flush writes, plus the separator.
func subSize(sub *Subscription) int64 {
	b, err := json.MarshalIndent(storedSub{
		Endpoint: sub.Endpoint, P256dh: sub.P256dh, Auth: sub.Auth, Watches: sub.Watches,
	}, "  ", " ")
	if err != nil {
		return 0
	}
	return int64(len(b)) + 4 // ",\n" + the next entry's leading indent
}

// setSub installs a subscription and keeps the running byte total in step.
// Caller holds mu.
func (s *Store) setSub(k string, sub *Subscription, size int64) {
	s.bytes += size - s.subBytes[k]
	s.subBytes[k] = size
	s.subs[k] = sub
}

// reaccount refreshes the byte tally for a subscription mutated in place
// (MarkFired stamps LastFiredAt; PurgeExpired drops watches). Without this the
// running total would drift away from the file and the ceiling would be wrong.
// Caller holds mu.
func (s *Store) reaccount(k string) {
	sub, ok := s.subs[k]
	if !ok {
		return
	}
	size := subSize(sub)
	s.bytes += size - s.subBytes[k]
	s.subBytes[k] = size
}

// dropSub removes a subscription and its byte accounting. Caller holds mu.
func (s *Store) dropSub(k string) {
	s.bytes -= s.subBytes[k]
	delete(s.subBytes, k)
	delete(s.subs, k)
}

// UpsertTopics is the LEGACY write path (§2.2): it replaces only the
// topic-representable watches and preserves date-constrained ones untouched.
//
// This is load-bearing. The SPA is cached, so a stale app.js keeps POSTing the
// full topic set it knows about. Without this rule it would silently delete
// every watch it cannot express — exactly the watches the user took the most
// care over.
func (s *Store) UpsertTopics(sub Subscription, topics []string) ([]Watch, error) {
	if err := ValidEndpoint(sub.Endpoint); err != nil {
		return nil, err
	}
	if sub.P256dh == "" || sub.Auth == "" {
		return nil, errors.New("p256dh and auth are required")
	}
	if len(topics) > MaxTopicsPerSub {
		return nil, ErrTooManyTopics
	}
	fresh, err := watchesFromTopics(topics, s.now().Unix())
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(sub.Endpoint)
	existing, exists := s.subs[k]
	if !exists && len(s.subs) >= s.maxSubs {
		return nil, ErrFull
	}

	// Keep everything the legacy client cannot see; replace only what it can.
	var merged []Watch
	if existing != nil {
		for _, w := range existing.Watches {
			if !w.TopicRepresentable() {
				merged = append(merged, cloneWatch(w))
			}
		}
	}
	merged = append(merged, fresh...)
	merged = s.carryHistory(existing, merged)

	next := &Subscription{
		Endpoint: sub.Endpoint, P256dh: sub.P256dh, Auth: sub.Auth, Watches: merged,
	}
	size := subSize(next)
	if grown := storeWrapperBytes + s.bytes - s.subBytes[k] + size; grown > s.maxBytes && size > s.subBytes[k] {
		s.logf("WARN alert-store-size-limit %d bytes (limit %d): refusing to grow", s.bytes, s.maxBytes)
		return nil, ErrStoreTooLarge
	}
	s.setSub(k, next, size)
	s.touch()
	return slices.Clone(merged), nil
}

// carryHistory copies createdAt/lastFiredAt from an existing subscription's
// watches onto the incoming list, matching on content id, and stamps
// createdAt on genuinely new watches. Caller holds the write lock.
func (s *Store) carryHistory(existing *Subscription, watches []Watch) []Watch {
	prior := map[string]Watch{}
	if existing != nil {
		for _, w := range existing.Watches {
			prior[w.ID] = w
		}
	}
	now := s.now().Unix()
	for i := range watches {
		if old, ok := prior[watches[i].ID]; ok {
			if old.CreatedAt != 0 {
				watches[i].CreatedAt = old.CreatedAt
			}
			watches[i].LastFiredAt = old.LastFiredAt
		}
		if watches[i].CreatedAt == 0 {
			watches[i].CreatedAt = now
		}
	}
	return watches
}

// normalizeAll validates every watch and collapses duplicate ids.
func normalizeAll(watches []Watch) ([]Watch, error) {
	seen := map[string]bool{}
	out := make([]Watch, 0, len(watches))
	for _, w := range watches {
		normalized, err := Normalize(w)
		if err != nil {
			return nil, err
		}
		if seen[normalized.ID] {
			continue // content-addressed ids make duplicates free to collapse
		}
		seen[normalized.ID] = true
		out = append(out, normalized)
	}
	return out, nil
}

// MarkFired records that watches fired, for the UI's "last alert" line. The
// debounced writer absorbs it, so it costs no extra file write.
func (s *Store) MarkFired(endpoint string, ids []string, t int64) {
	if len(ids) == 0 {
		return
	}
	fired := map[string]bool{}
	for _, id := range ids {
		fired[id] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.subs[key(endpoint)]
	if !ok {
		return
	}
	changed := false
	for i, w := range sub.Watches {
		if fired[w.ID] {
			sub.Watches[i].LastFiredAt = t
			changed = true
		}
	}
	if changed {
		s.reaccount(key(endpoint))
		s.touch()
	}
}

// MarkPushed records that a push was ACCEPTED by the push service (a 2xx).
// It is not evidence the notification was shown — only that it was handed off
// — which is exactly why we also track ACKs. A no-op for an unknown endpoint.
func (s *Store) MarkPushed(endpoint string, t int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.subs[key(endpoint)]
	if !ok {
		return
	}
	sub.LastPushAt = t
	sub.PushCount++
	s.reaccount(key(endpoint))
	s.touch()
}

// MarkAcked records that the service worker confirmed it received a push.
// A no-op for an unknown endpoint.
func (s *Store) MarkAcked(endpoint string, t int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.subs[key(endpoint)]
	if !ok {
		return
	}
	sub.LastAckAt = t
	sub.AckCount++
	s.reaccount(key(endpoint))
	s.touch()
}

// DeliveryStatus returns a subscription's push/ack telemetry. ok is false for
// an unknown endpoint.
func (s *Store) DeliveryStatus(endpoint string) (lastPushAt, lastAckAt, pushCount, ackCount int64, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sub, found := s.subs[key(endpoint)]
	if !found {
		return 0, 0, 0, 0, false
	}
	return sub.LastPushAt, sub.LastAckAt, sub.PushCount, sub.AckCount, true
}

// AckSlack is how far before the last push an ACK may land and still count as
// "the device acknowledged our most recent send" — absorbing clock skew and
// send/ack ordering (the ACK for push N can race ahead of MarkPushed for
// push N+1).
const AckSlack = 300

// Reachable reports whether a device still appears to be receiving pushes:
// it has acked at least once, at or after (within AckSlack of) our most recent
// send. A device we have never pushed to is reachable by default — nothing has
// been sent, so nothing can be concluded. Everything else is the "we've been
// sending pushes this device never acknowledges" state the UI surfaces.
func Reachable(lastPushAt, lastAckAt, pushCount, ackCount int64) bool {
	if pushCount == 0 {
		return true
	}
	return ackCount > 0 && lastAckAt >= lastPushAt-AckSlack
}

// PurgeExpired drops watches that expired more than ExpiryGraceDays ago and
// removes any subscription the purge leaves with nothing to watch (EC-1).
func (s *Store) PurgeExpired(today int) (watches, subs int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, sub := range s.subs {
		kept := make([]Watch, 0, len(sub.Watches))
		purged := 0
		for _, w := range sub.Watches {
			if w.ExpiredSince(today) > ExpiryGraceDays {
				purged++
				continue
			}
			kept = append(kept, w)
		}
		if purged == 0 {
			continue
		}
		sub.Watches = kept
		watches += purged
		// Only a subscription emptied BY the purge is removed; one that is
		// merely empty (a client that saved zero watches) is left alone.
		if len(sub.Watches) == 0 {
			s.dropSub(k)
			subs++
		} else {
			s.reaccount(k)
		}
	}
	if watches > 0 {
		s.touch()
	}
	return watches, subs
}

// Remove deletes a subscription by endpoint. Removing an unknown endpoint is a
// no-op (idempotent unsubscribe).
func (s *Store) Remove(endpoint string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.subs[key(endpoint)]; !ok {
		return
	}
	s.dropSub(key(endpoint))
	s.touch()
}

// Lookup returns the push credentials registered for an endpoint. The bool
// distinguishes "not a known subscription" from "known but watching nothing".
func (s *Store) Lookup(endpoint string) (webpush.Subscription, bool) {
	return s.LookupKey(key(endpoint))
}

// LookupKey is Lookup by subscription key (the alert state's key).
func (s *Store) LookupKey(subKey string) (webpush.Subscription, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sub, ok := s.subs[subKey]
	if !ok {
		return webpush.Subscription{}, false
	}
	return webpush.Subscription{
		Endpoint: sub.Endpoint, P256dh: sub.P256dh, Auth: sub.Auth,
	}, true
}

// Watches returns an endpoint's watches (nil if the endpoint is unknown).
func (s *Store) Watches(endpoint string) []Watch {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sub, ok := s.subs[key(endpoint)]
	if !ok {
		return nil
	}
	out := make([]Watch, len(sub.Watches))
	for i, w := range sub.Watches {
		out[i] = cloneWatch(w)
	}
	return out
}

// Topics is the legacy projection (§2.2): only topic-representable watches are
// visible, so a stale client can neither see nor destroy the rest.
func (s *Store) Topics(endpoint string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sub, ok := s.subs[key(endpoint)]
	if !ok {
		return nil
	}
	var topics []string
	for _, w := range sub.Watches {
		if w.TopicRepresentable() {
			topics = append(topics, w.Topics()...)
		}
	}
	slices.Sort(topics)
	return slices.Compact(topics)
}

// Count reports the number of stored subscriptions.
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.subs)
}

// DeliveryTotals reports the aggregate reachability picture for /healthz:
// how many subscriptions have ever acked, and how many are currently
// unreachable (pushes sent, none acknowledged at/after the last send). Watching
// unreachable climb is how we notice a platform silently eating our pushes.
func (s *Store) DeliveryTotals() (acked, unreachable int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sub := range s.subs {
		if sub.AckCount > 0 {
			acked++
		}
		if !Reachable(sub.LastPushAt, sub.LastAckAt, sub.PushCount, sub.AckCount) {
			unreachable++
		}
	}
	return acked, unreachable
}

// WatchCount reports the total number of watches across all subscriptions.
func (s *Store) WatchCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	total := 0
	for _, sub := range s.subs {
		total += len(sub.Watches)
	}
	return total
}

// touch bumps the version and schedules a write. Caller holds the write lock.
func (s *Store) touch() {
	s.version++
	s.markDirty()
}

// markDirty schedules a debounced flush: writes land at most once per debounce
// interval, so a burst of subscribes costs one file write. Called with mu held
// (see LOCK ORDER), so it must never wait on mu.
func (s *Store) markDirty() {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed {
		return
	}
	if s.flushTimer != nil {
		return // a flush is already scheduled
	}
	wait := max(s.debounce-time.Since(s.lastWrite), 0)
	s.flushTimer = time.AfterFunc(wait, func() {
		s.writeMu.Lock()
		s.flushTimer = nil
		s.writeMu.Unlock()
		if err := s.Flush(); err != nil {
			s.logf("WARN alert-store-write %s: %v", s.path, err)
		}
	})
}

// Flush writes the store to disk now (atomic tmp+rename) if anything changed.
func (s *Store) Flush() error {
	// Snapshot under mu FIRST and release it before taking writeMu — holding
	// writeMu while waiting on mu would invert the lock order and deadlock
	// against any concurrent mutation (see LOCK ORDER on the struct).
	// The deep copy is also what keeps the encoder off the live slices, which
	// the watch loop mutates via MarkFired.
	s.mu.RLock()
	version := s.version
	file := storeFile{Schema: Schema, Subs: make([]storedSub, 0, len(s.subs))}
	for _, k := range slices.Sorted(maps.Keys(s.subs)) {
		sub := cloneSub(s.subs[k])
		file.Subs = append(file.Subs, storedSub{
			Endpoint: sub.Endpoint, P256dh: sub.P256dh, Auth: sub.Auth,
			Watches:    sub.Watches,
			LastPushAt: sub.LastPushAt, LastAckAt: sub.LastAckAt,
			PushCount: sub.PushCount, AckCount: sub.AckCount,
		})
	}
	s.mu.RUnlock()

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// Dirtiness is a version comparison, not a flag: a mutation that lands after
	// our snapshot bumps the version and schedules its own flush, whose snapshot
	// supersedes ours. That also means a slow write can never clear the dirty
	// state of a change it didn't persist.
	if version <= s.writtenVersion {
		return nil
	}

	raw, err := json.MarshalIndent(file, "", " ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, subscriptionFile); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return err
	}
	s.writtenVersion = version
	s.lastWrite = time.Now()
	return nil
}

// Close cancels any pending debounce and flushes synchronously. It is safe to
// call more than once; the watcher calls it on SIGTERM/SIGINT.
func (s *Store) Close() error {
	s.writeMu.Lock()
	if s.flushTimer != nil {
		s.flushTimer.Stop()
		s.flushTimer = nil
	}
	s.closed = true
	s.writeMu.Unlock()
	return s.Flush()
}

// ValidEndpoint reports whether an endpoint is an https URL on an allowlisted
// push service.
func ValidEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return ErrBadEndpoint
	}
	host := strings.ToLower(u.Hostname())
	for _, allowed := range pushHosts {
		if host == allowed || strings.HasSuffix(host, "."+allowed) {
			return nil
		}
	}
	return fmt.Errorf("%w: %q", ErrBadEndpoint, host)
}
