// Package doctor runs readiness checks: one table, run in order, each check under its own
// timeout, skipped when a check it needs did not pass. Report: one text row per check plus a
// count line.
package doctor

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// Status is a row's verdict, lowercase. Only FAIL fails the run.
type Status string

const (
	Pass Status = "pass"
	Warn Status = "warn"
	Fail Status = "fail"
	Skip Status = "skip"
)

// Tag is the bracket form the text report prints: "[PASS]".
func (status Status) Tag() string {
	return "[" + strings.ToUpper(string(status)) + "]"
}

// Check is one readiness check. Needs lists ids of earlier checks that must be PASS or WARN
// for this one to run. Timeout bounds Run through its context; zero means unbounded.
type Check struct {
	ID      string
	Title   string
	Needs   []string
	Timeout time.Duration
	Run     func(ctx context.Context) Result
}

// Result is what a check returns. Hint is the fix on a WARN or FAIL row: cause, quoted command
// with the row's values, what it changes. The runner drops it on PASS.
type Result struct {
	Status  Status
	Summary string
	Hint    string
}

// Row is one check's outcome in a report.
type Row struct {
	ID    string
	Title string
	Needs []string
	Result
	Elapsed time.Duration
}

// Report is one run's rows, in run order.
type Report struct {
	Rows    []Row
	Elapsed time.Duration
}

// Failed counts FAIL rows.
func (report Report) Failed() int {
	return report.count(Fail)
}

func (report Report) count(status Status) int {
	total := 0
	for _, row := range report.Rows {
		if row.Status == status {
			total++
		}
	}
	return total
}

// Run runs checks in order. A check whose need is not PASS or WARN is skipped; its summary
// names the first blocking need. A check that outlives its timeout fails.
func Run(ctx context.Context, checks []Check) Report {
	started := time.Now()
	report := Report{Rows: make([]Row, 0, len(checks))}
	statuses := make(map[string]Status, len(checks))
	for _, check := range checks {
		row := Row{ID: check.ID, Title: check.Title, Needs: check.Needs}
		if blocker := firstBlockingNeed(check.Needs, statuses); blocker != "" {
			row.Result = Result{Status: Skip, Summary: "needs " + blocker}
		} else {
			checkStarted := time.Now()
			row.Result = runBounded(ctx, check)
			row.Elapsed = time.Since(checkStarted)
		}
		if row.Status == Pass {
			row.Hint = ""
		}
		statuses[check.ID] = row.Status
		report.Rows = append(report.Rows, row)
	}
	report.Elapsed = time.Since(started)
	return report
}

// firstBlockingNeed returns the first need whose row is neither PASS nor WARN, or "" when
// none blocks. A need that never ran blocks too.
func firstBlockingNeed(needs []string, statuses map[string]Status) string {
	for _, need := range needs {
		if status := statuses[need]; status != Pass && status != Warn {
			return need
		}
	}
	return ""
}

// runBounded runs one check on its own goroutine under its timeout, so a check that ignores
// its context cannot hang the run. Once the context ends the verdict is the context's: a
// result that lands with or after the deadline is dropped. A dead run context starts nothing.
func runBounded(ctx context.Context, check Check) Result {
	if ctx.Err() != nil {
		return Result{Status: Fail, Summary: ctx.Err().Error()}
	}
	runCtx := ctx
	if check.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, check.Timeout)
		defer cancel()
	}

	results := make(chan Result, 1)
	go func() { results <- check.Run(runCtx) }()
	select {
	case result := <-results:
		if runCtx.Err() == nil {
			return result
		}
	case <-runCtx.Done():
	}
	if ctx.Err() != nil {
		return Result{Status: Fail, Summary: ctx.Err().Error()}
	}
	return Result{Status: Fail, Summary: fmt.Sprintf("timed out after %s", check.Timeout)}
}

// WriteText writes one row per check, `[STATUS] <id>: <summary>[; fix: <hint>]`, then the
// count line. ASCII only, no color.
func WriteText(writer io.Writer, report Report) error {
	var text strings.Builder
	for _, row := range report.Rows {
		fmt.Fprintf(&text, "%s %s: %s", row.Status.Tag(), row.ID, row.Summary)
		if row.Hint != "" {
			fmt.Fprintf(&text, "; fix: %s", row.Hint)
		}
		text.WriteByte('\n')
	}
	fmt.Fprintf(&text, "doctor: %d passed, %d warnings, %d failed, %d skipped\n",
		report.count(Pass), report.count(Warn), report.count(Fail), report.count(Skip))
	_, err := io.WriteString(writer, text.String())
	return err
}
