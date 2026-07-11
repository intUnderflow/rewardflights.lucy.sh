// Package warnings collects and prints machine-greppable processor warnings.
//
// Warnings mark data that was skipped or substituted (malformed source files,
// unknown codes). They never abort a run; the process still exits 0. Each
// warning is one stderr line of the form "WARN <kind> <detail…>" so CI can
// grep for specific kinds (e.g. "WARN unmapped-place-code").
package warnings

import (
	"fmt"
	"io"
	"strings"
)

// Log writes warning lines to an output stream and counts them.
type Log struct {
	out   io.Writer
	count int
}

// New returns a Log writing to out (pass io.Discard to silence).
func New(out io.Writer) *Log {
	if out == nil {
		out = io.Discard
	}
	return &Log{out: out}
}

// Warn prints one warning line: "WARN <kind> <parts joined by spaces>".
func (l *Log) Warn(kind string, parts ...string) {
	l.count++
	if len(parts) == 0 {
		fmt.Fprintf(l.out, "WARN %s\n", kind)
		return
	}
	fmt.Fprintf(l.out, "WARN %s %s\n", kind, strings.Join(parts, " "))
}

// Count reports the number of warnings issued so far.
func (l *Log) Count() int { return l.count }
