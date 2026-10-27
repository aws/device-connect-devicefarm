package doctor

import (
	"bytes"
	"context"
	"strings"
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
			Hint: "the running daemon listens on port 6000, not 5037. Run 'devicefarm-connect bridge stop && devicefarm-connect bridge start --port 5037'."}),
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
		"[WARN] local.daemon: running (pid 42) on port 6000, not 5037; fix: the running daemon listens on port 6000, not 5037. Run 'devicefarm-connect bridge stop && devicefarm-connect bridge start --port 5037'.\n"+
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

const (
	fixtureSessionArn         = "arn:aws:devicefarm:us-west-2:123456789012:session:11111111-2222-3333-4444-555555555555"
	fixtureNoSessionSkip      = "pass --session-arn to check the session, the device connection and adb versions"
	fixturePortHeldHint       = "an adb server holds the port the daemon would bind. Stop it with 'adb -P 35477 kill-server', then run 'devicefarm-connect bridge start --port 35477' before any other adb command, or adb restarts its server on that port. Or start the daemon on another port with 'devicefarm-connect bridge start --port <n>' and point adb at it with 'adb -P <n>'."
	fixtureReverseOffHint     = "the daemon runs with adb reverse off, so every 'adb reverse' fails. Run 'devicefarm-connect bridge stop && devicefarm-connect bridge start --port 40775 --allow-adb-reverse --session-arn " + fixtureSessionArn + "' to start it again with reverse on. '--allow-adb-reverse' lets the device open connections to this machine, so use it only with a device you trust; a public physical device can run software you do not control."
	fixtureVersionSkewHint    = "the local adb is 1.0.41 and the remote adb server is 1.0.39, so the first adb command would stop that server. Install platform-tools whose adb is 1.0.39, or put such an adb first on PATH, then rerun 'devicefarm-connect doctor --session-arn " + fixtureSessionArn + " --port 37537'. Until then every adb command through the daemon ends the remote adb server."
	fixtureCredentialsSummary = `this shell has no usable AWS credentials: failed to refresh cached credentials, no EC2 IMDS role found, operation error ec2imds: GetMetadata, access disabled to EC2 IMDS via client option, or "AWS_EC2_METADATA_DISABLED" environment variable`
	fixtureCredentialsHint    = "the AWS config did not load or its credential chain resolved nothing, so every Device Farm call fails. Set 'AWS_PROFILE' to a profile with Device Farm access, run 'aws sso login --profile <name>' for a profile that uses SSO, or export 'AWS_ACCESS_KEY_ID' and 'AWS_SECRET_ACCESS_KEY'. For more information, see https://docs.aws.amazon.com/cli/latest/userguide/cli-chap-authentication.html"
)

// tableChecks is a check table shaped like the doctor command's, with canned results. A check
// without a canned result passes.
func tableChecks(results map[string]Result) []Check {
	table := []struct {
		id, title string
		category  Category
		needs     []string
	}{
		{"local.runtime-dir", "working directory", CategoryLocal, nil},
		{"local.daemon", "bridge daemon", CategoryLocal, []string{"local.runtime-dir"}},
		{"local.listen-port", "listen port", CategoryLocal, []string{"local.daemon"}},
		{"local.forward-table", "adb forward", CategoryLocal, nil},
		{"aws.credentials", "AWS credentials", CategoryAWS, nil},
		{"session.resolve", "remote access session", CategoryAWS, []string{"aws.credentials"}},
		{"relay.session", "adb session to the device", CategoryNetwork, []string{"session.resolve"}},
		{"relay.reattach", "adb session reconnect", CategoryNetwork, []string{"relay.session"}},
		{"relay.reverse", "adb reverse", CategoryNetwork, []string{"session.resolve", "local.daemon"}},
		{"relay.adb-version", "adb versions", CategoryDependencies, []string{"relay.session"}},
	}
	checks := make([]Check, 0, len(table))
	for _, entry := range table {
		result, canned := results[entry.id]
		if !canned {
			result = Result{Status: Pass, Summary: "ok"}
		}
		checks = append(checks, Check{ID: entry.id, Title: entry.title, Category: entry.category, Needs: entry.needs,
			Run: func(context.Context) Result { return result }})
	}
	return checks
}

func renderTable(t *testing.T, checks []Check, width int) string {
	var out bytes.Buffer
	require.NoError(t, WriteTable(&out, Run(context.Background(), checks), width))
	return out.String()
}

func TestGivenCannedResultsWhenWrittenAsATableAt80ColumnsThenTheOutputMatchesByteForByte(t *testing.T) {
	tests := []struct {
		name    string
		results map[string]Result
		want    string
	}{
		{name: "no session, everything local passes", results: map[string]Result{
			"session.resolve": {Status: Skip, Summary: fixtureNoSessionSkip},
		}, want: `+---------------+--------+-----------------------------------------------------+
| Category      | Status | Details                                             |
+---------------+--------+-----------------------------------------------------+
| Local machine | PASS   | working directory, bridge daemon, listen port, adb  |
|               |        |   forward                                           |
+---------------+--------+-----------------------------------------------------+
| AWS           | PASS   | AWS credentials                                     |
|               |        | remote access session not checked: pass             |
|               |        |   --session-arn to check the session, the device    |
|               |        |   connection and adb versions                       |
+---------------+--------+-----------------------------------------------------+
| Network       | SKIP   | not checked: needs remote access session            |
+---------------+--------+-----------------------------------------------------+
| Dependencies  | SKIP   | not checked: needs remote access session            |
+---------------+--------+-----------------------------------------------------+
doctor: 2 of 4 categories passed, 2 not checked
`},
		{name: "every check passes", results: nil, want: `+---------------+--------+-----------------------------------------------------+
| Category      | Status | Details                                             |
+---------------+--------+-----------------------------------------------------+
| Local machine | PASS   | working directory, bridge daemon, listen port, adb  |
|               |        |   forward                                           |
+---------------+--------+-----------------------------------------------------+
| AWS           | PASS   | AWS credentials, remote access session              |
+---------------+--------+-----------------------------------------------------+
| Network       | PASS   | adb session to the device, adb session reconnect,   |
|               |        |   adb reverse                                       |
+---------------+--------+-----------------------------------------------------+
| Dependencies  | PASS   | adb versions                                        |
+---------------+--------+-----------------------------------------------------+
doctor: 4 of 4 categories passed
`},
		{name: "an adb server holds the listen port", results: map[string]Result{
			"local.listen-port": {Status: Fail, Summary: "127.0.0.1:35477 is held by an adb server (version 41)", Hint: fixturePortHeldHint},
			"session.resolve":   {Status: Skip, Summary: fixtureNoSessionSkip},
		}, want: `+---------------+--------+-----------------------------------------------------+
| Category      | Status | Details                                             |
+---------------+--------+-----------------------------------------------------+
| Local machine | FAIL   | listen port: 127.0.0.1:35477 is held by an adb      |
|               |        |   server (version 41)                               |
|               |        | also passed: working directory, bridge daemon, adb  |
|               |        |   forward                                           |
+---------------+--------+-----------------------------------------------------+
| AWS           | PASS   | AWS credentials                                     |
|               |        | remote access session not checked: pass             |
|               |        |   --session-arn to check the session, the device    |
|               |        |   connection and adb versions                       |
+---------------+--------+-----------------------------------------------------+
| Network       | SKIP   | not checked: needs remote access session            |
+---------------+--------+-----------------------------------------------------+
| Dependencies  | SKIP   | not checked: needs remote access session            |
+---------------+--------+-----------------------------------------------------+

What to do
1. Local machine, listen port: an adb server holds the port the daemon would
   bind. Stop it with 'adb -P 35477 kill-server', then run
   'devicefarm-connect bridge start --port 35477' before any other adb command,
   or adb restarts its server on that port. Or start the daemon on another port
   with 'devicefarm-connect bridge start --port <n>' and point adb at it with
   'adb -P <n>'.
doctor: 1 of 4 categories passed, 1 failed, 2 not checked
`},
		{name: "reverse off and an adb version mismatch", results: map[string]Result{
			"local.daemon": {Status: Warn, Summary: "running (pid 17557) on port 40775; device Google Pixel 9 (OS 16), session " + fixtureSessionArn + "; reverse off",
				Hint: fixtureReverseOffHint},
			"relay.adb-version": {Status: Fail, Summary: "local adb 1.0.41 differs from the remote adb server (1.0.39); the first adb command would stop that server",
				Hint: fixtureVersionSkewHint},
		}, want: `+---------------+--------+-----------------------------------------------------+
| Category      | Status | Details                                             |
+---------------+--------+-----------------------------------------------------+
| Local machine | WARN   | bridge daemon: running (pid 17557) on port 40775;   |
|               |        |   device Google Pixel 9 (OS 16), session            |
|               |        |   arn:aws:devicefarm:us-west-2:123456789012:session |
|               |        |   :11111111-2222-3333-4444-555555555555; reverse    |
|               |        |   off                                               |
|               |        | also passed: working directory, listen port, adb    |
|               |        |   forward                                           |
+---------------+--------+-----------------------------------------------------+
| AWS           | PASS   | AWS credentials, remote access session              |
+---------------+--------+-----------------------------------------------------+
| Network       | PASS   | adb session to the device, adb session reconnect,   |
|               |        |   adb reverse                                       |
+---------------+--------+-----------------------------------------------------+
| Dependencies  | FAIL   | adb versions: local adb 1.0.41 differs from the     |
|               |        |   remote adb server (1.0.39); the first adb command |
|               |        |   would stop that server                            |
+---------------+--------+-----------------------------------------------------+

What to do
1. Local machine, bridge daemon: the daemon runs with adb reverse off, so every
   'adb reverse' fails. Run
   'devicefarm-connect bridge stop && devicefarm-connect bridge start --port 40775 --allow-adb-reverse --session-arn arn:aws:devicefarm:us-west-2:123456789012:session:11111111-2222-3333-4444-555555555555'
   to start it again with reverse on. '--allow-adb-reverse' lets the device open
   connections to this machine, so use it only with a device you trust; a public
   physical device can run software you do not control.
2. Dependencies, adb versions: the local adb is 1.0.41 and the remote adb server
   is 1.0.39, so the first adb command would stop that server. Install
   platform-tools whose adb is 1.0.39, or put such an adb first on PATH, then
   rerun
   'devicefarm-connect doctor --session-arn arn:aws:devicefarm:us-west-2:123456789012:session:11111111-2222-3333-4444-555555555555 --port 37537'.
   Until then every adb command through the daemon ends the remote adb server.
doctor: 2 of 4 categories passed, 1 warning, 1 failed
`},
		{name: "credentials fail and the rest cascades", results: map[string]Result{
			"aws.credentials": {Status: Fail, Summary: fixtureCredentialsSummary, Hint: fixtureCredentialsHint},
		}, want: `+---------------+--------+-----------------------------------------------------+
| Category      | Status | Details                                             |
+---------------+--------+-----------------------------------------------------+
| Local machine | PASS   | working directory, bridge daemon, listen port, adb  |
|               |        |   forward                                           |
+---------------+--------+-----------------------------------------------------+
| AWS           | FAIL   | AWS credentials: this shell has no usable AWS       |
|               |        |   credentials: failed to refresh cached             |
|               |        |   credentials, no EC2 IMDS role found, operation    |
|               |        |   error ec2imds: GetMetadata, access disabled to    |
|               |        |   EC2 IMDS via client option, or                    |
|               |        |   "AWS_EC2_METADATA_DISABLED" environment variable  |
|               |        | remote access session not checked: AWS credentials  |
|               |        |   failed                                            |
+---------------+--------+-----------------------------------------------------+
| Network       | SKIP   | not checked: AWS credentials failed                 |
+---------------+--------+-----------------------------------------------------+
| Dependencies  | SKIP   | not checked: AWS credentials failed                 |
+---------------+--------+-----------------------------------------------------+

What to do
1. AWS, AWS credentials: the AWS config did not load or its credential chain
   resolved nothing, so every Device Farm call fails. Set 'AWS_PROFILE' to a
   profile with Device Farm access, run 'aws sso login --profile <name>' for a
   profile that uses SSO, or export 'AWS_ACCESS_KEY_ID' and
   'AWS_SECRET_ACCESS_KEY'. For more information, see
   https://docs.aws.amazon.com/cli/latest/userguide/cli-chap-authentication.html
doctor: 1 of 4 categories passed, 1 failed, 2 not checked
`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, renderTable(t, tableChecks(tc.results), 80))
		})
	}
}

func TestGivenAWarnBesideAFailWhenGroupedThenTheCategoryIsFailAndAWarnBesideAPassIsWarn(t *testing.T) {
	report := Report{Rows: []Row{
		{ID: "local.daemon", Category: CategoryLocal, Result: Result{Status: Warn}},
		{ID: "local.listen-port", Category: CategoryLocal, Result: Result{Status: Fail}},
		{ID: "aws.credentials", Category: CategoryAWS, Result: Result{Status: Pass}},
		{ID: "session.resolve", Category: CategoryAWS, Result: Result{Status: Warn}},
	}}

	categories := report.Categories()

	require.Len(t, categories, 4)
	assert.Equal(t, CategoryReport{ID: CategoryLocal, Title: "Local machine", Status: Fail, Rows: report.Rows[:2]}, categories[0])
	assert.Equal(t, CategoryReport{ID: CategoryAWS, Title: "AWS", Status: Warn, Rows: report.Rows[2:]}, categories[1])
	assert.Equal(t, CategoryReport{ID: CategoryNetwork, Title: "Network", Status: Skip}, categories[2])
	assert.Equal(t, CategoryReport{ID: CategoryDependencies, Title: "Dependencies", Status: Skip}, categories[3])
}

func TestGivenSkippedChecksWhenGroupedThenAnAllSkipCategoryIsSkipAndAPartlySkippedOneKeepsWhatRan(t *testing.T) {
	report := Run(context.Background(), tableChecks(map[string]Result{
		"local.daemon":    {Status: Warn, Summary: "reverse off"},
		"session.resolve": {Status: Skip, Summary: fixtureNoSessionSkip},
	}))

	categories := report.Categories()

	assert.Equal(t, []Status{Warn, Pass, Skip, Skip},
		[]Status{categories[0].Status, categories[1].Status, categories[2].Status, categories[3].Status})
	assert.Equal(t, CategoryLocal, report.Rows[0].Category, "Run copies the category onto the row")
}

func TestGivenSkipRowsWhenTheirReasonsAreResolvedThenRunnerSkipsReadTheRootAndCheckAuthoredSkipsKeepTheirSummary(t *testing.T) {
	authored := Check{ID: "session.resolve", Title: "remote access session", Run: func(context.Context) Result {
		return Result{Status: Skip, Summary: fixtureNoSessionSkip}
	}}
	failed := Check{ID: "aws.credentials", Title: "AWS credentials", Run: func(context.Context) Result {
		return Result{Status: Fail, Summary: "no credentials"}
	}}
	tests := []struct {
		name   string
		checks []Check
		want   map[string]string
	}{
		{name: "a check-authored skip keeps its summary", checks: []Check{authored},
			want: map[string]string{"session.resolve": fixtureNoSessionSkip}},
		{name: "one and two hops to a check-authored root read needs the root title", checks: []Check{authored,
			constantCheck("relay.session", Result{}, "session.resolve"), constantCheck("relay.reattach", Result{}, "relay.session")},
			want: map[string]string{"session.resolve": fixtureNoSessionSkip, "relay.session": "needs remote access session",
				"relay.reattach": "needs remote access session"}},
		{name: "a failed root reads the root title failed", checks: []Check{failed,
			constantCheck("session.resolve", Result{}, "aws.credentials"), constantCheck("relay.session", Result{}, "session.resolve")},
			want: map[string]string{"session.resolve": "AWS credentials failed", "relay.session": "AWS credentials failed"}},
		{name: "a blocker with no row reads nothing", checks: []Check{constantCheck("relay.session", Result{}, "ghost")},
			want: map[string]string{"relay.session": ""}},
		{name: "a needs cycle reads nothing", checks: []Check{constantCheck("relay.session", Result{}, "relay.reattach"),
			constantCheck("relay.reattach", Result{}, "relay.session")},
			want: map[string]string{"relay.session": "", "relay.reattach": ""}},
		{name: "a need named before it runs keeps the blocker the runner saw", checks: []Check{failed,
			{ID: "relay.session", Title: "adb session to the device", Needs: []string{"relay.reverse", "aws.credentials"}},
			{ID: "relay.reverse", Title: "adb reverse", Run: func(context.Context) Result { return Result{Status: Pass} }},
			constantCheck("relay.reattach", Result{}, "relay.session")},
			want: map[string]string{"relay.session": "needs adb reverse", "relay.reattach": "needs adb reverse"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, skipReasons(Run(context.Background(), tc.checks)))
		})
	}
}

func TestGivenAllSkippedChecksWithDifferentReasonsWhenWrittenAsATableThenTheCellNamesEachCheck(t *testing.T) {
	out := renderTable(t, tableChecks(map[string]Result{
		"relay.session": {Status: Skip, Summary: "no device transport yet"},
		"relay.reverse": {Status: Skip, Summary: "the reverse probe is off for this run"},
	}), 120)

	assert.Contains(t, out, "| Network       | SKIP   | adb session to the device not checked: no device transport yet")
	assert.Contains(t, out, "|               |        | adb session reconnect not checked: needs adb session to the device")
	assert.Contains(t, out, "|               |        | adb reverse not checked: the reverse probe is off for this run")
	assert.Contains(t, out, "| Dependencies  | SKIP   | not checked: needs adb session to the device")
}

func TestGivenANeedWithNoRowBesideAPassWhenWrittenAsATableThenTheCellReadsNotCheckedWithNoReason(t *testing.T) {
	checks := []Check{
		{ID: "local.runtime-dir", Title: "working directory", Category: CategoryLocal, Run: func(context.Context) Result { return Result{Status: Pass} }},
		{ID: "local.daemon", Title: "bridge daemon", Category: CategoryLocal, Needs: []string{"ghost"}},
	}

	out := renderTable(t, checks, 80)

	assert.Contains(t, out, "| Local machine | PASS   | working directory                                   |\n"+
		"|               |        | bridge daemon not checked                           |\n")
}

func TestGivenAFailWithoutAHintWhenWrittenAsATableThenNoWhatToDoFollows(t *testing.T) {
	out := renderTable(t, tableChecks(map[string]Result{
		"relay.session": {Status: Fail, Summary: "timed out after 20s"},
	}), 120)

	assert.NotContains(t, out, "What to do")
	assert.Contains(t, out, "| Network       | FAIL   | adb session to the device: timed out after 20s")
	assert.Contains(t, out, "|               |        | also passed: adb reverse")
	assert.Contains(t, out, "|               |        | adb session reconnect not checked: adb session to the device failed")
	assert.Contains(t, out, "| Dependencies  | SKIP   | not checked: adb session to the device failed")
	assert.True(t, strings.HasSuffix(out, "+\ndoctor: 2 of 4 categories passed, 1 failed, 1 not checked\n"), out)
}

func TestGivenACheckInAnUnknownCategoryWhenWrittenAsATableThenItRendersAsAnExtraRowCutToTheColumn(t *testing.T) {
	checks := append(tableChecks(nil), Check{ID: "extra.check", Title: "extra check", Category: "extra.long-category-id",
		Run: func(context.Context) Result { return Result{Status: Pass, Summary: "ok"} }})

	out := renderTable(t, checks, 80)

	lines := strings.Split(out, "\n")
	assert.Equal(t, "| Dependencies  | PASS   | adb versions                                        |", lines[11])
	assert.Equal(t, "| extra.long-ca | PASS   | extra check                                         |", lines[13])
	assert.Equal(t, "| tegory-id     |        |                                                     |", lines[14])
	assert.Equal(t, "doctor: 5 of 5 categories passed", lines[16])
}

// Fails the build when a check in the table names a category outside the table order.
func TestGivenTheCheckTableWhenListedThenEveryCheckNamesAKnownCategory(t *testing.T) {
	for _, check := range Checks(Inputs{}) {
		assert.Contains(t, categoryOrder, check.Category, "check %s: category %q is not a table row", check.ID, check.Category)
	}
}

func TestGivenAWidthOutsideTheBoundsWhenWrittenAsATableThenTheWidthIsClamped(t *testing.T) {
	narrow := renderTable(t, tableChecks(nil), 40)
	wide := renderTable(t, tableChecks(nil), 200)

	assert.Len(t, strings.Split(narrow, "\n")[0], tableMinWidth)
	assert.Len(t, strings.Split(wide, "\n")[0], tableMaxWidth)
	assert.Contains(t, narrow, "| Local machine | PASS   | working directory, bridge       |\n|               |        |   daemon, listen port, adb      |\n")
	assert.Contains(t, wide, "| Local machine | PASS   | working directory, bridge daemon, listen port, adb forward                                  |\n")
}

func TestGivenLongTokensAndSpansWhenWrappedThenTheCellCutsAndTheListKeepsThemWhole(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		width     int
		indent    int
		keepWhole bool
		want      []string
	}{
		{name: "a span longer than the cell line wraps as prose", text: "see 'adb -P 5037 kill-server' then rerun", width: 20, indent: 2,
			want: []string{"see 'adb -P 5037", "  kill-server' then", "  rerun"}},
		{name: "a span longer than the list line stands alone", text: "see 'adb -P 5037 kill-server' then rerun", width: 20, indent: 3, keepWhole: true,
			want: []string{"see", "   'adb -P 5037 kill-server'", "   then rerun"}},
		{name: "a span that fits a continuation line moves whole", text: "stop it with 'adb kill-server' now", width: 20, indent: 2,
			want: []string{"stop it with", "  'adb kill-server'", "  now"}},
		{name: "a spaceless token longer than the cell line is cut at the edge", text: "pid 123456789012345", width: 10, indent: 2,
			want: []string{"pid", "  12345678", "  9012345"}},
		{name: "a spaceless token on the first cell line uses the full width", text: "123456789012345", width: 10, indent: 2,
			want: []string{"1234567890", "  12345"}},
		{name: "a spaceless token longer than the list line overflows", text: "pid 123456789012345", width: 10, indent: 3, keepWhole: true,
			want: []string{"pid", "   123456789012345"}},
		{name: "runes count, not bytes", text: "José José José", width: 9, indent: 2,
			want: []string{"José José", "  José"}},
		{name: "empty text is one empty line", text: "", width: 10, indent: 2, want: []string{""}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, wrap(tc.text, tc.width, tc.indent, tc.keepWhole))
		})
	}
}

func TestGivenQuotesInProseWhenTokenizedThenOnlySpansBetweenSpacesStayWhole(t *testing.T) {
	assert.Equal(t, []string{"the", "server's", "adb;", "run", "'adb kill-server',", "then", "'x'."},
		tokens("the  server's adb; run 'adb kill-server', then 'x'.", 80, false))
	assert.Equal(t, []string{"'abc", "def", "'abc'd", "ef"}, tokens("'abc def 'abc'd ef", 80, false))
	assert.Equal(t, []string{"'abc", "def"}, tokens("'abc def", 80, false), "an unclosed quote opens no span")
	assert.Equal(t, []string{"run", "'--log \"/tmp/Build  Logs/b.log\"'", "now"}, tokens("run\t'--log \"/tmp/Build  Logs/b.log\"'\nnow", 80, false),
		"a span keeps its inner spaces; prose whitespace is a separator")
	assert.Equal(t, []string{"'a  b'", "c"}, tokens("'a  b'\u00a0c", 80, false), "a multibyte space closes a span too")
	assert.Equal(t, []string{"'a b c'"}, tokens("'a b c'", 7, false), "a span at the limit stays whole in a cell")
	assert.Equal(t, []string{"'a", "b", "c'"}, tokens("'a b c'", 6, false), "a span over the limit splits in a cell")
	assert.Equal(t, []string{"'a b c'"}, tokens("'a b c'", 6, true), "a span over the limit stays whole in the list")
}

func TestGivenTwoWarningsWhenWrittenAsATableThenTheCountLineInflectsWarnings(t *testing.T) {
	out := renderTable(t, tableChecks(map[string]Result{
		"local.daemon":    {Status: Warn, Summary: "reverse off", Hint: "restart it"},
		"aws.credentials": {Status: Warn, Summary: "expires soon", Hint: "log in again"},
	}), 80)

	assert.True(t, strings.HasSuffix(out, "\nWhat to do\n1. Local machine, bridge daemon: restart it\n2. AWS, AWS credentials: log in again\n"+
		"doctor: 2 of 4 categories passed, 2 warnings\n"), out)
}

func TestGivenAnEmptyReportWhenWrittenAsATableThenEveryRowIsNotCheckedAndTheCountLineSaysSo(t *testing.T) {
	var out bytes.Buffer

	require.NoError(t, WriteTable(&out, Report{}, 80))

	assert.Equal(t, `+---------------+--------+-----------------------------------------------------+
| Category      | Status | Details                                             |
+---------------+--------+-----------------------------------------------------+
| Local machine | SKIP   | not checked                                         |
+---------------+--------+-----------------------------------------------------+
| AWS           | SKIP   | not checked                                         |
+---------------+--------+-----------------------------------------------------+
| Network       | SKIP   | not checked                                         |
+---------------+--------+-----------------------------------------------------+
| Dependencies  | SKIP   | not checked                                         |
+---------------+--------+-----------------------------------------------------+
doctor: 0 of 4 categories passed, 4 not checked
`, out.String())
}
