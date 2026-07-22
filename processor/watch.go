package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/alertapi"
	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/alerts"
	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/alertstore"
	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/app"
	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/stats"
	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/webpush"
)

// watchConfig parameterizes the long-running watcher.
type watchConfig struct {
	Src      string        // source checkout to watch
	Out      string        // derived-repo checkout to write/commit/push
	Force    bool          // pass through the shrink-guardrail override
	Interval time.Duration // HEAD poll cadence
	Commit   bool          // commit -Out when it changes
	Push     bool          // push -Out after commit (implies Commit + pre-sync)
	TokenCmd string        // shell command printing a git token to stdout; empty -> plain git

	StatsState string // availability-climate state file; empty disables stats

	Alerts            alerts.Config // seat alerts; enabled when VapidKeyPath is set
	AlertsStore       string        // subscription store file
	AlertsMaxSubs     int
	AlertsMaxBytes    int64  // subscription cap
	AlertsListen      string // subscription API listen address; empty -> no API
	AlertsRate        int    // API requests/min per client IP
	AlertsBurst       int    // API rate-limit burst
	AlertsTestPerHour int    // POST /test sends per hour per subscription
}

// runWatch is the constantly-running mode: it watches the local source
// checkout's HEAD and regenerates the derived repo the moment a new source
// commit lands. Because the source data is produced on the same host, watching
// the local checkout is instant and needs no GitHub webhook. It never exits on
// a transient error — it logs and keeps going (launchd KeepAlive restarts it if
// the process itself dies).
func runWatch(cfg watchConfig) error {
	if cfg.Push {
		cfg.Commit = true
	}
	logf("watch: src=%s out=%s interval=%s commit=%t push=%t", cfg.Src, cfg.Out, cfg.Interval, cfg.Commit, cfg.Push)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	// Seat alerts (optional): the watcher owns the subscription store and
	// serves the subscription API itself. Baseline on the bundle that is
	// current at process start, so nothing already-visible alerts on restart.
	var alerter *alerts.Watcher
	var store *alertstore.Store
	apiCtx, stopAPI := context.WithCancel(context.Background())
	defer stopAPI()

	if cfg.Alerts.VapidKeyPath != "" {
		var err error
		store, err = alertstore.Open(alertstore.Options{
			Path: cfg.AlertsStore, MaxSubs: cfg.AlertsMaxSubs, MaxBytes: cfg.AlertsMaxBytes, Logf: logf,
		})
		if err != nil {
			return err
		}
		// The store holds subscriptions: never lose them to an abrupt exit.
		defer func() {
			if err := store.Close(); err != nil {
				logf("watch: alerts: flushing subscription store: %v", err)
			}
		}()

		cfg.Alerts.Store = store
		cfg.Alerts.Logf = logf
		alerter, err = alerts.NewWatcher(cfg.Alerts)
		if err != nil {
			return err
		}
		logf("watch: alerts enabled (store=%s subs=%d)", cfg.AlertsStore, store.Count())

		if raw, err := os.ReadFile(filepath.Join(cfg.Out, "availability.json")); err == nil {
			alerter.Baseline(raw)
			logf("watch: alerts baseline loaded")
		} else {
			logf("watch: alerts: no baseline yet (%v)", err)
		}

		if cfg.AlertsListen != "" {
			// The API's POST /test needs to send a push itself, so it gets
			// its own sender over the same VAPID key the watcher publishes with.
			vapid, err := webpush.LoadVapid(cfg.Alerts.VapidKeyPath, cfg.Alerts.VapidSubject)
			if err != nil {
				return err
			}
			api := alertapi.New(alertapi.Config{
				Addr: cfg.AlertsListen, Store: store, Sender: webpush.NewSender(vapid),
				RatePerMin: cfg.AlertsRate, Burst: cfg.AlertsBurst,
				TestPerHour: cfg.AlertsTestPerHour, Logf: logf,
				// The API reports each watch's status (expired / impossible /
				// unknown-route) against the data the watcher currently holds.
				Horizon: alerter.Horizon,
			})
			go func() {
				logf("watch: subscription API listening on %s", cfg.AlertsListen)
				if err := api.ListenAndServe(apiCtx); err != nil {
					logf("watch: subscription API stopped: %v", err)
				}
			}()
		}
	}

	// Freshness tags ride on push mode: rendezvous points are only meaningful
	// when the out repo is actually published.
	if cfg.Push {
		tg := newTagger(cfg.Out, cfg.TokenCmd, logf)
		tagStop := make(chan struct{})
		defer close(tagStop)
		go tg.run(tagStop)
		logf("watch: freshness tagger running (%ds buckets, %s retention)", tagBucketSecs, tagRetention)
	}

	// Availability-climate accumulator (stats.json): fed the old/new bundle
	// around each regeneration, emitted into the out repo at most hourly so
	// it rides an existing data commit without adding churn.
	var climate *stats.Accumulator
	if cfg.StatsState != "" {
		climate = stats.New(cfg.StatsState, logf)
		logf("watch: stats accumulator enabled (state=%s)", cfg.StatsState)
	}

	var lastProcessed string
	tick := time.NewTicker(cfg.Interval)
	defer tick.Stop()

	// Process immediately on startup (catch up on anything since we last ran),
	// then on every source-HEAD change.
	process := func() {
		sha, unix, err := gitHead(cfg.Src)
		if err != nil {
			logf("watch: cannot read source HEAD: %v", err)
			return
		}
		if sha == lastProcessed {
			return
		}
		logf("watch: source at %s — processing", short(sha))

		// Sync the out repo to origin first so our push is always a
		// fast-forward, even if README/etc. changed out-of-band.
		if cfg.Push {
			if err := gitSync(cfg.Out); err != nil {
				logf("watch: sync of out repo failed (continuing): %v", err)
			}
		}

		// The climate diff needs the PREVIOUS generation, which app.Run is
		// about to overwrite.
		var oldRaw []byte
		if climate != nil {
			oldRaw, _ = os.ReadFile(filepath.Join(cfg.Out, "availability.json"))
		}

		result, err := app.Run(app.Config{
			Src: cfg.Src, Out: cfg.Out,
			SourceSHA: sha, SourceTime: unix,
			Force:    cfg.Force,
			Warnings: os.Stderr,
		})
		if err != nil {
			logf("watch: processing failed (will retry): %v", err)
			return // do not advance lastProcessed; retry next tick
		}
		logf("watch: %s", result.Summary())

		if climate != nil {
			if newRaw, err := os.ReadFile(filepath.Join(cfg.Out, "availability.json")); err == nil {
				climate.Cycle(oldRaw, newRaw, unix)
				if climate.EmitIfDue(cfg.Out, unix) {
					logf("watch: stats.json refreshed")
				}
			}
		}

		if cfg.Commit {
			pushed, err := gitCommitPush(cfg.Out, sha, cfg.Push, cfg.TokenCmd)
			if err != nil {
				logf("watch: commit/push failed (will retry): %v", err)
				return // retry next tick
			}
			if pushed {
				logf("watch: %s derived data for source %s", pushVerb(cfg.Push), short(sha))
			} else {
				logf("watch: no derived-data change for source %s", short(sha))
			}
		}

		// Seat alerts run strictly after the data push and never fail the
		// cycle: a lost notification is acceptable, a stalled push is not.
		if alerter != nil {
			if raw, err := os.ReadFile(filepath.Join(cfg.Out, "availability.json")); err == nil {
				alerter.Cycle(raw)
			} else {
				logf("watch: alerts: cannot read new bundle: %v", err)
			}
		}
		lastProcessed = sha
	}

	process()
	for {
		select {
		case <-stop:
			logf("watch: signal received, exiting")
			stopAPI() // graceful HTTP shutdown; the deferred store.Close flushes
			return nil
		case <-tick.C:
			process()
		}
	}
}

// gitSync makes the out working tree match origin/main (if that ref exists),
// discarding local state so the subsequent regenerate+commit is a clean
// fast-forward.
func gitSync(out string) error {
	if _, err := git(out, "fetch", "origin"); err != nil {
		return err
	}
	// Only reset if origin/main exists (it won't on a brand-new empty remote).
	if _, err := git(out, "rev-parse", "--verify", "origin/main"); err == nil {
		if _, err := git(out, "reset", "--hard", "origin/main"); err != nil {
			return err
		}
	}
	return nil
}

// gitCommitPush stages the out repo; if nothing changed it returns
// (false, nil). Otherwise it commits, optionally pushes (using tokenCmd's
// token via an http.extraheader so no credential state is touched), and
// returns (true, nil).
func gitCommitPush(out, sourceSHA string, push bool, tokenCmd string) (bool, error) {
	if _, err := git(out, "add", "-A"); err != nil {
		return false, err
	}
	// Nothing staged -> nothing to do (idempotent regeneration).
	if _, err := git(out, "diff", "--cached", "--quiet"); err == nil {
		return false, nil
	}
	msg := "data: source " + short(sourceSHA)
	if _, err := git(out, "-c", "user.name=rewardflights-processor", "-c", "user.email=processor@rewardflights.lucy.sh",
		"commit", "-m", msg); err != nil {
		return false, err
	}
	if !push {
		return true, nil
	}
	pushArgs := []string{"push", "origin", "HEAD:main"}
	if tokenCmd != "" {
		tok, err := runToken(tokenCmd)
		if err != nil {
			return true, fmt.Errorf("minting token: %w", err)
		}
		auth := "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+tok))
		pushArgs = append([]string{"-c", "credential.helper=", "-c", "http.extraheader=" + auth}, pushArgs...)
	}
	if _, err := git(out, pushArgs...); err != nil {
		return true, err
	}
	return true, nil
}

// runToken runs the token command via the shell and returns its trimmed stdout.
func runToken(tokenCmd string) (string, error) {
	cmd := exec.Command("/bin/bash", "-c", tokenCmd)
	outBytes, err := cmd.Output()
	if err != nil {
		return "", gitErr(err)
	}
	tok := strings.TrimSpace(string(outBytes))
	if tok == "" {
		return "", errors.New("token command produced no output")
	}
	return tok, nil
}

// git runs a git subcommand in dir and returns trimmed stdout.
func git(dir string, args ...string) (string, error) {
	full := append([]string{"-C", dir}, args...)
	out, err := exec.Command("git", full...).Output()
	if err != nil {
		return "", gitErr(err)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitErr surfaces a subprocess stderr tail when present.
func gitErr(err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
	}
	return err
}

func short(sha string) string {
	if len(sha) > 9 {
		return sha[:9]
	}
	return sha
}

func pushVerb(push bool) string {
	if push {
		return "pushed"
	}
	return "committed"
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s "+format+"\n", append([]any{time.Now().UTC().Format(time.RFC3339)}, args...)...)
}

// runStatsBackfill replays the out repo's git history of availability.json
// through the climate accumulator — a one-time seeding so stats.json starts
// with every generation since launch instead of an empty ledger.
func runStatsBackfill(out, statePath string) error {
	acc := stats.New(statePath, logf)
	log, err := git(out, "log", "--reverse", "--format=%H %ct", "main", "--", "availability.json")
	if err != nil {
		return err
	}
	var prev []byte
	n := 0
	for _, line := range strings.Split(log, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		unix, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		raw, err := git(out, "show", fields[0]+":availability.json")
		if err != nil {
			logf("backfill: skipping %s: %v", short(fields[0]), err)
			continue
		}
		acc.Cycle(prev, []byte(raw), unix)
		prev = []byte(raw)
		n++
		if n%200 == 0 {
			logf("backfill: %d generations ingested", n)
		}
	}
	logf("backfill: done — %d generations", n)
	return nil
}
