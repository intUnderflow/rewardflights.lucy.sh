package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTagBucketMath(t *testing.T) {
	if got := tagBucket(0); got != 0 {
		t.Errorf("bucket(0) = %d", got)
	}
	if got := tagBucket(29); got != 0 {
		t.Errorf("bucket(29) = %d", got)
	}
	if got := tagBucket(30); got != 1 {
		t.Errorf("bucket(30) = %d", got)
	}
	if tagName(59730150) != "t-59730150" {
		t.Errorf("tagName = %s", tagName(59730150))
	}
}

func TestParseTagBucketsAndStale(t *testing.T) {
	ls := strings.Join([]string{
		"abc123\trefs/tags/t-100",
		"def456\trefs/tags/t-200",
		"aaa111\trefs/tags/t-200^{}", // annotated peel: ignored
		"bbb222\trefs/tags/v1.0",     // not ours
		"ccc333\trefs/tags/t-nope",   // malformed
		"",
	}, "\n")
	tags := parseTagBuckets(ls)
	if len(tags) != 2 || tags["t-100"] != 100 || tags["t-200"] != 200 {
		t.Fatalf("parsed = %v", tags)
	}
	// At bucket 100's close + retention + a minute, only t-100 is stale.
	now := int64(100*tagBucketSecs) + int64(tagRetention/time.Second) + 60
	stale := staleTagNames(tags, now, tagRetention)
	if len(stale) != 1 || stale[0] != "t-100" {
		t.Fatalf("stale = %v", stale)
	}
}

// TestTaggerAgainstLocalOrigin exercises the real git plumbing: tag pushed at
// a boundary, skipped when HEAD unchanged, skipped when late (prompt-or-never),
// pruned when past retention.
func TestTaggerAgainstLocalOrigin(t *testing.T) {
	dir := t.TempDir()
	origin := filepath.Join(dir, "origin.git")
	work := filepath.Join(dir, "work")
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	run("init", "--bare", "-q", origin)
	run("init", "-q", work)
	run("-C", work, "remote", "add", "origin", origin)
	commit := func(msg string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte(msg), 0o644); err != nil {
			t.Fatal(err)
		}
		run("-C", work, "add", "-A")
		run("-C", work, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", msg)
	}
	commit("one")

	var logs []string
	tg := newTagger(work, "", func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) })
	clock := time.Unix(3000*tagBucketSecs, 0) // a boundary
	tg.now = func() time.Time { return clock }

	// 1. On-time boundary with a new HEAD: tags the just-closed bucket.
	tg.tick(clock)
	if got := run("ls-remote", "--tags", origin); !strings.Contains(got, "refs/tags/t-2999") {
		t.Fatalf("expected t-2999 on origin, got:\n%s", got)
	}

	// 2. Same HEAD at the next boundary: no new tag.
	clock = clock.Add(tagBucketSecs * time.Second)
	tg.tick(clock)
	if got := run("ls-remote", "--tags", origin); strings.Contains(got, "t-3000") {
		t.Fatalf("unchanged HEAD must not tag: %s", got)
	}

	// 3. New HEAD but the tick wakes late: prompt-or-never skips the bucket.
	commit("two")
	boundary := clock.Add(tagBucketSecs * time.Second)
	clock = boundary.Add(tagPromptWindow + time.Second) // woke 6s late
	tg.tick(boundary)
	if got := run("ls-remote", "--tags", origin); strings.Contains(got, "t-3001") {
		t.Fatalf("late tick must not tag: %s", got)
	}
	if !strings.Contains(strings.Join(logs, "|"), "skipping bucket") {
		t.Fatalf("expected a skip log, got %v", logs)
	}

	// 4. The next on-time boundary carries the change.
	boundary = boundary.Add(tagBucketSecs * time.Second)
	clock = boundary
	tg.tick(boundary)
	if got := run("ls-remote", "--tags", origin); !strings.Contains(got, "t-3002") {
		t.Fatalf("expected t-3002 after recovery, got:\n%s", got)
	}

	// 5. Prune: jump past retention; both tags are now stale and deleted.
	clock = clock.Add(tagRetention + time.Hour)
	tg.lastPrune = clock.Add(-2 * tagPruneEvery)
	tg.prune()
	if got := run("ls-remote", "--tags", origin); strings.Contains(got, "refs/tags/t-") {
		t.Fatalf("expected all t- tags pruned, got:\n%s", got)
	}
}
