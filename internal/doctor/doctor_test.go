package doctor

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// constantCheck answers result at once.
func constantCheck(id string, result Result, needs ...string) Check {
	return Check{ID: id, Needs: needs, Run: func(context.Context) Result { return result }}
}

// recordingCheck passes and records that it ran.
func recordingCheck(id string, ran *bool, needs ...string) Check {
	return Check{ID: id, Needs: needs, Run: func(context.Context) Result {
		*ran = true
		return Result{Status: Pass, Summary: "ran"}
	}}
}

func TestGivenANeedThatDidNotPassWhenRunThenTheDependentIsSkippedNamingTheFirstBlocker(t *testing.T) {
	failed := constantCheck("local.runtime-dir", Result{Status: Fail, Summary: "no dir"})
	skipped := constantCheck("local.runtime-dir", Result{Status: Skip, Summary: "no flag"})
	passed := constantCheck("local.daemon", Result{Status: Pass, Summary: "not running"})
	alsoFailed := constantCheck("local.listen-port", Result{Status: Fail, Summary: "held"})
	tests := []struct {
		name        string
		earlier     []Check
		needs       []string
		wantSummary string
	}{
		{name: "failed need", earlier: []Check{failed}, needs: []string{"local.runtime-dir"}, wantSummary: "needs local.runtime-dir"},
		{name: "skipped need", earlier: []Check{skipped}, needs: []string{"local.runtime-dir"}, wantSummary: "needs local.runtime-dir"},
		{name: "need that never ran", needs: []string{"local.runtime-dir"}, wantSummary: "needs local.runtime-dir"},
		{name: "passed need before a failed one", earlier: []Check{passed, alsoFailed},
			needs: []string{"local.daemon", "local.listen-port"}, wantSummary: "needs local.listen-port"},
		{name: "two failed needs name the first", earlier: []Check{failed, alsoFailed},
			needs: []string{"local.runtime-dir", "local.listen-port"}, wantSummary: "needs local.runtime-dir"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ran := false
			checks := append(tc.earlier, recordingCheck("relay.reverse", &ran, tc.needs...))

			report := Run(context.Background(), checks)

			assert.False(t, ran, "a dependent must not run when a need did not pass")
			dependent := report.Rows[len(report.Rows)-1]
			assert.Equal(t, Skip, dependent.Status)
			assert.Equal(t, tc.wantSummary, dependent.Summary)
			assert.Empty(t, dependent.Hint)
		})
	}
}

func TestGivenAWarnNeedWithinItsTimeoutWhenRunThenItsResultStandsAndTheDependentRuns(t *testing.T) {
	ran := false
	warning := constantCheck("local.daemon", Result{Status: Warn, Summary: "running on port 6000, not 5037", Hint: "restart the daemon"})
	warning.Timeout = time.Minute
	checks := []Check{warning, recordingCheck("local.listen-port", &ran, "local.daemon")}

	report := Run(context.Background(), checks)

	assert.True(t, ran, "a WARN need must let the dependent run")
	assert.Equal(t, Result{Status: Warn, Summary: "running on port 6000, not 5037", Hint: "restart the daemon"}, report.Rows[0].Result)
	assert.Less(t, report.Rows[0].Elapsed, time.Minute)
	assert.Equal(t, Pass, report.Rows[1].Status)
	assert.Equal(t, 0, report.Failed())
}

func TestGivenASlowCheckWhenItsTimeoutPassesThenTheRowFailsWithTheTimeoutText(t *testing.T) {
	const timeout = 20 * time.Millisecond
	// The check parks on a channel the test never closes, so only the runner's own deadline
	// can end the row. The deadline observation travels over a channel: the runner returns
	// while the check's goroutine still runs.
	deadlines := make(chan bool, 1)
	parked := make(chan struct{})
	slow := Check{ID: "relay.session", Timeout: timeout, Run: func(ctx context.Context) Result {
		_, hasDeadline := ctx.Deadline()
		deadlines <- hasDeadline
		<-parked
		return Result{Status: Pass, Summary: "too late"}
	}}

	report := Run(context.Background(), []Check{slow})

	require.Len(t, report.Rows, 1)
	assert.True(t, <-deadlines, "the check's context must carry the timeout")
	assert.Equal(t, Fail, report.Rows[0].Status)
	assert.Equal(t, "timed out after 20ms", report.Rows[0].Summary)
	assert.GreaterOrEqual(t, report.Rows[0].Elapsed, timeout)
	assert.Equal(t, 1, report.Failed())
}

func TestGivenACancelledRunContextWhenRunThenTheRowFailsWithTheCancelReasonAndNothingRuns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := false

	report := Run(ctx, []Check{recordingCheck("aws.credentials", &ran)})

	assert.False(t, ran, "a dead run context must start no probe")
	assert.Equal(t, Fail, report.Rows[0].Status)
	assert.Equal(t, context.Canceled.Error(), report.Rows[0].Summary)
}

func TestGivenARunContextCancelledDuringACheckWhenRunThenTheRowFailsWithTheCancelReason(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	parked := make(chan struct{})
	selfCancelling := Check{ID: "aws.credentials", Timeout: time.Minute, Run: func(context.Context) Result {
		cancel()
		<-parked
		return Result{Status: Pass, Summary: "too late"}
	}}

	report := Run(ctx, []Check{selfCancelling})

	assert.Equal(t, Fail, report.Rows[0].Status)
	assert.Equal(t, context.Canceled.Error(), report.Rows[0].Summary)
}

func TestGivenAPassResultWithAHintWhenRunThenTheRowCarriesNoHint(t *testing.T) {
	passing := constantCheck("local.forward-table", Result{Status: Pass, Summary: "port 40000", Hint: "stray fix"})

	report := Run(context.Background(), []Check{passing})

	assert.Equal(t, Pass, report.Rows[0].Status)
	assert.Empty(t, report.Rows[0].Hint)
}

func TestGivenMixedResultsWhenWrittenAsTextThenOneLinePerRowPlusTheCountLine(t *testing.T) {
	var deadlineSet bool
	checks := []Check{
		{ID: "local.runtime-dir", Title: "runtime dir", Run: func(ctx context.Context) Result {
			_, deadlineSet = ctx.Deadline()
			return Result{Status: Pass, Summary: "/run/user/1000/devicefarm-connect (socket path 41 bytes)"}
		}},
		constantCheck("local.daemon", Result{Status: Warn, Summary: "running (pid 42) on port 6000, not 5037",
			Hint: "the running daemon listens on port 6000, not 5037. Run 'devicefarm-connect proxy stop && devicefarm-connect proxy start --port 5037'."}),
		constantCheck("local.listen-port", Result{Status: Fail, Summary: "held by an adb server (version 41)",
			Hint: "adb kill-server"}),
		constantCheck("relay.session", Result{Status: Pass, Summary: "unused"}, "local.listen-port"),
	}
	var out bytes.Buffer

	report := Run(context.Background(), checks)
	require.NoError(t, WriteText(&out, report))

	assert.False(t, deadlineSet, "a check with no timeout runs unbounded")
	assert.Equal(t, []string{"local.runtime-dir", "local.daemon", "local.listen-port", "relay.session"},
		[]string{report.Rows[0].ID, report.Rows[1].ID, report.Rows[2].ID, report.Rows[3].ID})
	assert.Equal(t, "runtime dir", report.Rows[0].Title)
	assert.Equal(t, []string{"local.listen-port"}, report.Rows[3].Needs)
	assert.Equal(t, 1, report.Failed())
	assert.Equal(t, "[PASS] local.runtime-dir: /run/user/1000/devicefarm-connect (socket path 41 bytes)\n"+
		"[WARN] local.daemon: running (pid 42) on port 6000, not 5037; fix: the running daemon listens on port 6000, not 5037. Run 'devicefarm-connect proxy stop && devicefarm-connect proxy start --port 5037'.\n"+
		"[FAIL] local.listen-port: held by an adb server (version 41); fix: adb kill-server\n"+
		"[SKIP] relay.session: needs local.listen-port\n"+
		"doctor: 1 passed, 1 warnings, 1 failed, 1 skipped\n", out.String())
}

func TestGivenAnEmptyTableWhenRunThenTheReportIsEmptyAndTheCountLineIsAllZero(t *testing.T) {
	var out bytes.Buffer

	report := Run(context.Background(), nil)
	require.NoError(t, WriteText(&out, report))

	assert.Empty(t, report.Rows)
	assert.Equal(t, "doctor: 0 passed, 0 warnings, 0 failed, 0 skipped\n", out.String())
}
