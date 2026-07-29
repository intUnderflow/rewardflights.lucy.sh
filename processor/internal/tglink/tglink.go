// Package tglink holds pending Telegram link codes: the site configures a
// watch, the alerts API mints a code for it, and tapping the bot's
// t.me/<bot>?start=<code> deep link redeems it — proving chat ownership by
// the /start message itself, which is the only way a chat id may ever enter
// the subscription store.
//
// Codes are single-use, short-lived, and live in memory only: a watcher
// restart drops them, and the user just taps the site's button again.
package tglink

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/alertstore"
)

const (
	// TTL is how long a minted code stays redeemable. Long enough to open
	// Telegram and tap Start on a slow phone; short enough that a leaked
	// URL from a screenshot goes stale fast.
	TTL = 10 * time.Minute
	// MaxPending bounds the map — an unauthenticated endpoint mints these,
	// so a flood must exhaust its own rate limit, never our memory.
	MaxPending = 2000
)

var ErrFull = errors.New("too many pending links; try again in a minute")

type entry struct {
	watches []alertstore.Watch
	exp     time.Time
}

// Pending is the concurrency-safe code store.
type Pending struct {
	mu  sync.Mutex
	m   map[string]entry
	now func() time.Time
}

func New() *Pending {
	return &Pending{m: map[string]entry{}, now: time.Now}
}

// Put mints a single-use code for a validated watch list.
func (p *Pending) Put(watches []alertstore.Watch) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prune()
	if len(p.m) >= MaxPending {
		return "", ErrFull
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	// 22 chars of [A-Za-z0-9_-]: well inside Telegram's 64-char start-param
	// limit and exactly its allowed alphabet.
	code := base64.RawURLEncoding.EncodeToString(raw[:])
	p.m[code] = entry{watches: watches, exp: p.now().Add(TTL)}
	return code, nil
}

// Take redeems a code: the watches it was minted for, exactly once.
func (p *Pending) Take(code string) ([]alertstore.Watch, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.m[code]
	if !ok || p.now().After(e.exp) {
		delete(p.m, code)
		return nil, false
	}
	delete(p.m, code)
	return e.watches, true
}

// prune drops expired codes; called under mu.
func (p *Pending) prune() {
	now := p.now()
	for c, e := range p.m {
		if now.After(e.exp) {
			delete(p.m, c)
		}
	}
}
