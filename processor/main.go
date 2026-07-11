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

	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/app"
)

func main() {
	src := flag.String("src", "", "path to a checkout of github.com/intUnderflow/rewardflights")
	out := flag.String("out", "", "path to a checkout of the derived data repository")
	sourceSHA := flag.String("source-sha", "", "source commit SHA embedded as \"v\" (default: HEAD of -src)")
	sourceTime := flag.Int64("source-time", 0, "source commit unix timestamp embedded as \"t\" (default: committer time of HEAD of -src)")
	force := flag.Bool("force", false, "override the 50% routeDates shrink guardrail")
	flag.Parse()

	if *src == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: processor -src <rewardflights checkout> -out <derived repo checkout> [-source-sha SHA] [-source-time UNIX] [-force]")
		os.Exit(2)
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
