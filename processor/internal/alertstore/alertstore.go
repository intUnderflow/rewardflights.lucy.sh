// Package alertstore is the watcher's local Web Push subscription store.
//
// The watcher is the sender, so it owns the subscriptions: if the host is
// down, alerts do not fire regardless of where the subscriptions live, and
// keeping them local costs nothing and avoids an external write-rate cap.
//
// The store is a single JSON file, mirrored in memory with a topic index for
// O(1) sender lookups. Reads are cheap and concurrent; writes are serialized,
// debounced (at most one file write per debounce interval), and always
// flushed before the process exits.
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
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/webpush"
)

// Subscription is one browser push subscription and the topics it wants.
type Subscription struct {
	Endpoint string   `json:"endpoint"`
	P256dh   string   `json:"p256dh"`
	Auth     string   `json:"auth"`
	Topics   []string `json:"topics"`
}

// Limits and defaults.
const (
	MaxTopicsPerSub  = 60
	DefaultMaxSubs   = 100000
	defaultDebounce  = time.Second
	subscriptionFile = 0o600 // subscriptions are personal data: owner-only
)

// topicRe is the subscribable topic grammar: rf_{ROUTE}_{ow|rt}_{cabin}.
var topicRe = regexp.MustCompile(`^rf_[A-Z]{3}-[A-Z]{3}_(ow|rt)_[MWCF]$`)

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
	ErrFull          = errors.New("subscription store is full")
)

// Store is the concurrency-safe subscription store.
type Store struct {
	path     string
	maxSubs  int
	debounce time.Duration
	logf     func(string, ...any)

	mu    sync.RWMutex
	subs  map[string]*Subscription   // endpoint key (sha256 hex) -> subscription
	index map[string][]*Subscription // topic -> subscriptions

	writeMu    sync.Mutex // serializes file writes
	dirty      bool
	lastWrite  time.Time
	flushTimer *time.Timer
	closed     bool
}

// Options configure a Store. Zero values take documented defaults.
type Options struct {
	Path     string // JSON file; required
	MaxSubs  int    // default DefaultMaxSubs
	Debounce time.Duration
	Logf     func(string, ...any)
}

// Open loads the store from disk. A missing file starts empty; a corrupt
// file is moved aside (.corrupt) and the store starts empty, because losing
// subscriptions is recoverable (browsers re-subscribe) but refusing to boot
// is not.
func Open(opts Options) (*Store, error) {
	if opts.Path == "" {
		return nil, errors.New("alertstore: no path")
	}
	if opts.MaxSubs <= 0 {
		opts.MaxSubs = DefaultMaxSubs
	}
	if opts.Debounce <= 0 {
		opts.Debounce = defaultDebounce
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	s := &Store{
		path:     opts.Path,
		maxSubs:  opts.MaxSubs,
		debounce: opts.Debounce,
		logf:     opts.Logf,
		subs:     map[string]*Subscription{},
		index:    map[string][]*Subscription{},
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
	if err := json.Unmarshal(raw, &file); err != nil || file.Schema != 1 {
		aside := opts.Path + ".corrupt"
		if renameErr := os.Rename(opts.Path, aside); renameErr == nil {
			s.logf("WARN alert-store-corrupt moved %s aside; starting empty", opts.Path)
		}
		return s, nil
	}
	for _, sub := range file.Subs {
		// Re-validate on load: the allowlist may have tightened, and a
		// hand-edited file must not smuggle in a bad endpoint.
		if err := validate(sub); err != nil {
			s.logf("WARN alert-store-dropped %s: %v", sub.Endpoint, err)
			continue
		}
		s.put(sub)
	}
	return s, nil
}

// storeFile is the on-disk shape.
type storeFile struct {
	Schema int            `json:"schema"`
	Subs   []Subscription `json:"subs"`
}

// key derives the map key for an endpoint (hashed: the file is smaller and
// the key is opaque in logs, while remaining a stable identity).
func key(endpoint string) string {
	sum := sha256.Sum256([]byte(endpoint))
	return hex.EncodeToString(sum[:])
}

// Get returns the subscriptions currently registered for a topic. The
// returned slice and its elements are copies: senders may hold them across
// mutations without racing.
func (s *Store) Get(topic string) []webpush.Subscription {
	s.mu.RLock()
	defer s.mu.RUnlock()
	subs := s.index[topic]
	out := make([]webpush.Subscription, 0, len(subs))
	for _, sub := range subs {
		out = append(out, webpush.Subscription{
			Endpoint: sub.Endpoint, P256dh: sub.P256dh, Auth: sub.Auth,
		})
	}
	return out
}

// Upsert registers a subscription, REPLACING that endpoint's topic set.
// Returns the stored (normalized, deduplicated, sorted) topics.
func (s *Store) Upsert(sub Subscription) ([]string, error) {
	if err := validate(sub); err != nil {
		return nil, err
	}
	stored := normalizeTopics(sub.Topics)
	sub.Topics = stored

	s.mu.Lock()
	k := key(sub.Endpoint)
	if _, exists := s.subs[k]; !exists && len(s.subs) >= s.maxSubs {
		s.mu.Unlock()
		return nil, ErrFull
	}
	s.remove(k)
	s.put(sub)
	s.mu.Unlock()

	s.markDirty()
	return stored, nil
}

// Remove deletes a subscription by endpoint. Removing an unknown endpoint is
// a no-op (idempotent unsubscribe).
func (s *Store) Remove(endpoint string) {
	s.mu.Lock()
	_, existed := s.subs[key(endpoint)]
	s.remove(key(endpoint))
	s.mu.Unlock()
	if existed {
		s.markDirty()
	}
}

// Lookup returns the subscription registered for an endpoint. The bool
// distinguishes "not a known subscription" from "known but subscribed to no
// topics" — Topics cannot, since both yield an empty list.
func (s *Store) Lookup(endpoint string) (webpush.Subscription, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sub, ok := s.subs[key(endpoint)]
	if !ok {
		return webpush.Subscription{}, false
	}
	return webpush.Subscription{
		Endpoint: sub.Endpoint, P256dh: sub.P256dh, Auth: sub.Auth,
	}, true
}

// Topics reports the topics an endpoint is subscribed to (nil if unknown).
func (s *Store) Topics(endpoint string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sub, ok := s.subs[key(endpoint)]
	if !ok {
		return nil
	}
	return slices.Clone(sub.Topics)
}

// ActiveTopics lists every topic with at least one subscriber, sorted. The
// sender uses this to skip work for topics nobody wants.
func (s *Store) ActiveTopics() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Sorted(maps.Keys(s.index))
}

// Count reports the number of stored subscriptions.
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.subs)
}

// put inserts a subscription into both maps. Caller holds the write lock.
func (s *Store) put(sub Subscription) {
	stored := &sub
	s.subs[key(sub.Endpoint)] = stored
	for _, topic := range sub.Topics {
		s.index[topic] = append(s.index[topic], stored)
	}
}

// remove deletes a subscription from both maps. Caller holds the write lock.
func (s *Store) remove(k string) {
	sub, ok := s.subs[k]
	if !ok {
		return
	}
	delete(s.subs, k)
	for _, topic := range sub.Topics {
		list := s.index[topic]
		for i, candidate := range list {
			if candidate == sub {
				list = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(list) == 0 {
			delete(s.index, topic)
		} else {
			s.index[topic] = list
		}
	}
}

// markDirty schedules a debounced flush: writes land at most once per
// debounce interval, so a burst of subscribes costs one file write.
func (s *Store) markDirty() {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed {
		return
	}
	s.dirty = true
	if s.flushTimer != nil {
		return // a flush is already scheduled
	}
	wait := s.debounce - time.Since(s.lastWrite)
	if wait < 0 {
		wait = 0
	}
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
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !s.dirty {
		return nil
	}

	s.mu.RLock()
	file := storeFile{Schema: 1, Subs: make([]Subscription, 0, len(s.subs))}
	for _, k := range slices.Sorted(maps.Keys(s.subs)) {
		file.Subs = append(file.Subs, *s.subs[k])
	}
	s.mu.RUnlock()

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
	s.dirty = false
	s.lastWrite = time.Now()
	return nil
}

// Close cancels any pending debounce and flushes synchronously. It is safe
// to call more than once; the watcher calls it on SIGTERM/SIGINT.
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

// validate enforces the endpoint allowlist and topic grammar.
func validate(sub Subscription) error {
	if err := ValidEndpoint(sub.Endpoint); err != nil {
		return err
	}
	if sub.P256dh == "" || sub.Auth == "" {
		return errors.New("p256dh and auth are required")
	}
	topics := normalizeTopics(sub.Topics)
	if len(topics) > MaxTopicsPerSub {
		return ErrTooManyTopics
	}
	for _, topic := range topics {
		if !topicRe.MatchString(topic) {
			return fmt.Errorf("%w: %q", ErrBadTopic, topic)
		}
	}
	return nil
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

// normalizeTopics deduplicates and sorts a topic list (nil-safe).
func normalizeTopics(topics []string) []string {
	set := map[string]bool{}
	for _, t := range topics {
		if t != "" {
			set[t] = true
		}
	}
	return slices.Sorted(maps.Keys(set))
}
