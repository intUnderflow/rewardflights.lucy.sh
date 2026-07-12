// Command processor transforms a rewardflights source checkout into the
// derived rewardflights.lucy.sh-data repository file set.
//
// Usage:
//
//	processor -src <rewardflights checkout> -out <derived repo checkout>
//	          [-source-sha SHA] [-source-time UNIX] [-force]
//
// When -source-sha / -source-time are not given they are read from the HEAD
// commit of -src via git. The process exits 0 on success (including success
// with warnings; warnings are greppable "WARN …" stderr lines) and nonzero
// only on hard failures (unreadable roots, no data, size budget breach,
// shrink guardrail).
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/alerts"
	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/app"
)

func main() {
	src := flag.String("src", "", "path to a checkout of github.com/intUnderflow/rewardflights")
	out := flag.String("out", "", "path to a checkout of the derived data repository")
	sourceSHA := flag.String("source-sha", "", "source commit SHA embedded as \"v\" (default: HEAD of -src)")
	sourceTime := flag.Int64("source-time", 0, "source commit unix timestamp embedded as \"t\" (default: committer time of HEAD of -src)")
	force := flag.Bool("force", false, "override the 50% routeDates shrink guardrail")
	watch := flag.Bool("watch", false, "run continuously: watch -src HEAD and regenerate on every new source commit")
	interval := flag.Duration("interval", 2*time.Second, "watch mode: how often to poll -src HEAD")
	commit := flag.Bool("commit", false, "watch mode: git commit -out when the derived data changes")
	push := flag.Bool("push", false, "watch mode: git push -out after committing (implies -commit)")
	tokenCmd := flag.String("token-cmd", "", "watch mode: shell command printing a git token to stdout for the push (e.g. the GitHub App mint script); empty uses the ambient git credentials")
	alertsWorker := flag.String("alerts-worker", "", "watch mode: subscription-store Worker base URL for seat alerts (empty disables alerting)")
	alertsPullSecret := flag.String("alerts-pull-secret", "", "watch mode: bearer token for pulling subscriptions from the Worker")
	alertsVapidKey := flag.String("alerts-vapid-key", "", "watch mode: file holding the VAPID P-256 private key (PEM or base64url scalar)")
	alertsVapidSubject := flag.String("alerts-vapid-subject", "", "watch mode: VAPID sub claim, e.g. mailto:alerts@rewardflights.lucy.sh")
	alertsState := flag.String("alerts-state", "", "watch mode: path of the alerts state file (cooldown/batch persistence)")
	alertsCooldown := flag.Duration("alerts-cooldown", 3*time.Hour, "watch mode: minimum off-time before a day re-alerts")
	alertsBatch := flag.Duration("alerts-batch", time.Hour, "watch mode: minimum interval between publishes per topic")
	alertsWindow := flag.Int("alerts-window", 30, "watch mode: round-trip return window in nights")
	flag.Parse()

	if *src == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: processor -src <rewardflights checkout> -out <derived repo checkout> [-source-sha SHA] [-source-time UNIX] [-force]")
		fmt.Fprintln(os.Stderr, "       processor -watch -src <...> -out <...> [-interval 2s] [-push] [-token-cmd '<cmd>']")
		os.Exit(2)
	}

	if *watch {
		if err := runWatch(watchConfig{
			Src: *src, Out: *out, Force: *force,
			Interval: *interval, Commit: *commit, Push: *push, TokenCmd: *tokenCmd,
			Alerts: alerts.Config{
				Worker:       *alertsWorker,
				PullSecret:   *alertsPullSecret,
				VapidKeyPath: *alertsVapidKey,
				VapidSubject: *alertsVapidSubject,
				StatePath:    *alertsState,
				Cooldown:     *alertsCooldown,
				Batch:        *alertsBatch,
				Window:       *alertsWindow,
			},
		}); err != nil {
			fatal(err)
		}
		return
	}

	sha, unix := *sourceSHA, *sourceTime
	if sha == "" || unix == 0 {
		gitSHA, gitTime, err := gitHead(*src)
		if err != nil {
			fatal(err)
		}
		if sha == "" {
			sha = gitSHA
		}
		if unix == 0 {
			unix = gitTime
		}
	}

	result, err := app.Run(app.Config{
		Src: *src, Out: *out,
		SourceSHA: sha, SourceTime: unix,
		Force:    *force,
		Warnings: os.Stderr,
	})
	if err != nil {
		fatal(err)
	}
	fmt.Println(result.Summary())
}

// gitHead reads the HEAD commit SHA and committer timestamp of the source
// checkout.
func gitHead(src string) (string, int64, error) {
	cmd := exec.Command("git", "-C", src, "log", "-1", "--format=%H %ct")
	raw, err := cmd.Output()
	if err != nil {
		detail := err.Error()
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			detail = strings.TrimSpace(string(exitErr.Stderr))
		}
		return "", 0, fmt.Errorf(
			"cannot determine source commit: `git -C %s log -1` failed (%s); is -src a git checkout? Otherwise pass -source-sha and -source-time explicitly",
			src, detail)
	}
	fields := strings.Fields(strings.TrimSpace(string(raw)))
	if len(fields) != 2 {
		return "", 0, fmt.Errorf("cannot determine source commit: unexpected git output %q", strings.TrimSpace(string(raw)))
	}
	unix, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("cannot determine source commit: bad committer timestamp %q: %w", fields[1], err)
	}
	return fields[0], unix, nil
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "processor: error: %v\n", err)
	os.Exit(1)
}
