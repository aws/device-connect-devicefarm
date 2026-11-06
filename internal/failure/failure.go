// Package failure words a command's failure for the person who typed it: the
// problem, its causes and the fix, each on its own line.
package failure

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// Explained is a failure in the user's words: Problem says what did not
// happen, Cause why, Fix what to do. Cause and Fix are optional. A layer that
// knows only the fix leaves Problem empty (WithFix); the command that reports
// the failure supplies it.
type Explained struct {
	Problem string
	Cause   error
	Fix     string
}

// WithFix attaches fix to cause. The command that reports the failure words the problem.
func WithFix(cause error, fix string) error {
	return &Explained{Cause: cause, Fix: fix}
}

// Error is the one-line form for logs and error-text readers: "problem: cause; fix".
func (explained *Explained) Error() string {
	var text strings.Builder
	text.WriteString(explained.Problem)
	if explained.Cause != nil {
		if text.Len() > 0 {
			text.WriteString(": ")
		}
		text.WriteString(explained.Cause.Error())
	}
	if explained.Fix != "" {
		if text.Len() > 0 {
			text.WriteString("; ")
		}
		text.WriteString(explained.Fix)
	}
	return text.String()
}

func (explained *Explained) Unwrap() error { return explained.Cause }

// Write renders err for a terminal: "program: error: problem", one cause line
// per segment of the chain, then the fixes, innermost first: the most specific
// step is the first to take. A nil err writes nothing.
func Write(writer io.Writer, program string, err error) {
	if err == nil {
		return
	}
	lines, fixes := unroll(err)
	fmt.Fprintf(writer, "%s: error: %s\n", program, lines[0])
	for _, cause := range lines[1:] {
		fmt.Fprintf(writer, "  cause: %s\n", cause)
	}
	for index := len(fixes) - 1; index >= 0; index-- {
		fmt.Fprintf(writer, "  fix:   %s\n", fixes[index])
	}
}

// unroll walks the chain outermost first and splits it at each Explained. An
// Explained yields its Problem and Fix. Any other error yields its text down to
// the next Explained below it, which is cut off the end: "invalid
// configuration: <explained>" becomes "invalid configuration". A plain chain
// with no Explained below prints whole, as one line, and the walk stops.
func unroll(err error) (lines []string, fixes []string) {
	for current := err; current != nil; {
		if explained, ok := current.(*Explained); ok {
			if explained.Problem != "" {
				lines = append(lines, explained.Problem)
			}
			if explained.Fix != "" {
				fixes = append(fixes, explained.Fix)
			}
			current = explained.Cause
			continue
		}
		text := current.Error()
		var nested *Explained
		if errors.As(current, &nested) {
			if own, found := strings.CutSuffix(text, nested.Error()); found {
				if own = strings.TrimRight(own, ":, "); own != "" {
					lines = append(lines, own)
				}
				current = nested
				continue
			}
		}
		lines = append(lines, text)
		break
	}
	if len(lines) == 0 {
		lines = []string{err.Error()}
	}
	return lines, fixes
}
