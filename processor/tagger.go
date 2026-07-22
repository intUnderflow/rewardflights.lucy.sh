package main

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Freshness tags — the "design-time agreement" protocol (SPEC §freshness).
//
// The site discovers new data by polling raw.githubusercontent.com, whose CDN
// caches BOTH 200s and 404s for ~300s, making every mutable URL up to five
// minutes stale. The way out is to never ask a question whose answer can
// change: time is chopped into fixed 30-second buckets, and a bucket whose
// close saw a new data-repo HEAD gets an immutable lightweight tag
// t-<unix/30>. Clients poll the JUST-CLOSED bucket's tag URL: a 200 is fresh
// by construction (nobody could have requested the URL before the tag
// existed), and a 404 is permanently true (the bucket is closed), so the
// CDN's aggressive caching works for freshness instead of against it.
//
// Three invariants keep the scheme sound:
//  1. PROMPT OR NEVER: a tag is pushed within tagPromptWindow of the bucket
//     boundary or not at all — a late tag would race the pollers (who poll at
//     close+10s) and poison edges with cached 404s. A skipped bucket's change
//     simply rides into the next bucket's tag.
//  2. TAGS POINT AT HEAD, so any missed bucket is fully recovered by the next
//     successful one; the site's ordinary manifest poll remains the floor.
//  3. Tags older than tagRetention are pruned promptly — they are rendezvous
//     points, not history (the commits stay), and ten minutes comfortably
//     outlives the client's 5-minute catch-up probe window.
const (
	tagBucketSecs   = 30
	tagPromptWindow = 5 * time.Second
	tagRetention    = 10 * time.Minute
	tagPruneEvery   = 5 * time.Minute
	tagPruneBatch   = 100 // refs per delete push — bounds command-line length
)

// tagBucket is the bucket number containing the given unix second.
func tagBucket(unix int64) int64 { return unix / tagBucketSecs }

// tagName renders a bucket's tag ref name.
func tagName(bucket int64) string { return fmt.Sprintf("t-%d", bucket) }

// parseTagBuckets extracts bucket numbers from `git ls-remote --tags` output,
// ignoring anything that isn't a t-<n> tag (annotated ^{} peels included).
func parseTagBuckets(lsRemote string) map[string]int64 {
	out := map[string]int64{}
	for _, line := range strings.Split(lsRemote, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		name, ok := strings.CutPrefix(fields[1], "refs/tags/")
		if !ok || strings.HasSuffix(name, "^{}") {
			continue
		}
		num, ok := strings.CutPrefix(name, "t-")
		if !ok {
			continue
		}
		if b, err := strconv.ParseInt(num, 10, 64); err == nil {
			out[name] = b
		}
	}
	return out
}

// staleTagNames lists the tags whose bucket closed more than retention ago.
func staleTagNames(tags map[string]int64, nowUnix int64, retention time.Duration) []string {
	cutoff := tagBucket(nowUnix - int64(retention/time.Second))
	var out []string
	for name, bucket := range tags {
		if bucket < cutoff {
			out = append(out, name)
		}
	}
	return out
}

// tagger publishes freshness tags for the out repo. It runs its own boundary
// loop, entirely decoupled from the processing cycle: it tags whatever HEAD
// is at each boundary, which is exactly the "state as of the bucket close"
// the clients rendezvous on.
type tagger struct {
	out       string
	tokenCmd  string
	lastSHA   string // HEAD at the last successful tag — unchanged HEAD tags nothing
	lastPrune time.Time
	logf      func(format string, args ...any)
	now       func() time.Time // injectable clock for tests
}

func newTagger(out, tokenCmd string, logf func(string, ...any)) *tagger {
	return &tagger{out: out, tokenCmd: tokenCmd, logf: logf, now: time.Now, lastPrune: time.Now()}
}

// run sleeps to each bucket boundary and ticks. Call in a goroutine.
func (t *tagger) run(stop <-chan struct{}) {
	for {
		now := t.now()
		next := now.Truncate(tagBucketSecs * time.Second).Add(tagBucketSecs * time.Second)
		select {
		case <-stop:
			return
		case <-time.After(next.Sub(now)):
		}
		t.tick(next)
	}
}

// tick runs one boundary: tag the just-closed bucket if HEAD moved, prune
// occasionally. boundary is the scheduled wall-clock boundary this tick is
// for; the just-closed bucket is the one ending at it.
func (t *tagger) tick(boundary time.Time) {
	woke := t.now()
	if late := woke.Sub(boundary); late > tagPromptWindow {
		// PROMPT OR NEVER: a tag this late could land after pollers asked and
		// freeze 404s into edges. Skip; the next bucket carries the change.
		t.logf("tagger: woke %.1fs after the boundary — skipping bucket %d", late.Seconds(), tagBucket(boundary.Unix())-1)
		return
	}
	sha, err := git(t.out, "rev-parse", "HEAD")
	if err != nil {
		t.logf("tagger: cannot read out HEAD: %v", err)
		return
	}
	if sha != t.lastSHA {
		bucket := tagBucket(boundary.Unix()) - 1 // the bucket that just closed
		started := t.now()
		if err := t.push("HEAD:refs/tags/" + tagName(bucket)); err != nil {
			t.logf("tagger: tag push failed (bucket %d): %v", bucket, err)
		} else {
			t.lastSHA = sha
			// Visibility timing feeds the client poll-offset choice (Δ=10s):
			// if this tail grows, Δ moves.
			t.logf("tagger: tagged bucket %d at %s (+%.1fs after boundary, push %.1fs)",
				bucket, short(sha), started.Sub(boundary).Seconds(), t.now().Sub(started).Seconds())
		}
	}
	if t.now().Sub(t.lastPrune) >= tagPruneEvery {
		t.lastPrune = t.now()
		t.prune()
	}
}

// prune deletes tags whose bucket closed more than tagRetention ago.
func (t *tagger) prune() {
	lsRemote, err := git(t.out, "ls-remote", "--tags", "origin", "t-*")
	if err != nil {
		t.logf("tagger: prune ls-remote failed: %v", err)
		return
	}
	stale := staleTagNames(parseTagBuckets(lsRemote), t.now().Unix(), tagRetention)
	for len(stale) > 0 {
		batch := stale[:min(len(stale), tagPruneBatch)]
		stale = stale[len(batch):]
		refs := make([]string, len(batch))
		for i, name := range batch {
			refs[i] = ":refs/tags/" + name
		}
		if err := t.push(refs...); err != nil {
			t.logf("tagger: prune push failed: %v", err)
			return
		}
		t.logf("tagger: pruned %d tag(s)", len(batch))
	}
}

// push runs `git push origin <refspecs...>` with the same token minting the
// data push uses (no credential state is touched).
func (t *tagger) push(refspecs ...string) error {
	args := append([]string{"push", "origin"}, refspecs...)
	if t.tokenCmd != "" {
		tok, err := runToken(t.tokenCmd)
		if err != nil {
			return fmt.Errorf("minting token: %w", err)
		}
		auth := "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+tok))
		args = append([]string{"-c", "credential.helper=", "-c", "http.extraheader=" + auth}, args...)
	}
	_, err := git(t.out, args...)
	return err
}
