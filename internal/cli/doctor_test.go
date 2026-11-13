// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/daemon"
	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// The checks in run order: the ones that run without a session, then the ones that need one.
var (
	localAndCredentialChecks = []string{"local.runtime-dir", "local.daemon", "local.listen-port", "local.forward-table", "aws.credentials"}
	sessionAndRelayChecks    = []string{"session.resolve", "relay.session", "relay.reattach", "relay.reverse", "relay.adb-version"}
	doctorCheckIDs           = append(append([]string{}, localAndCredentialChecks...), sessionAndRelayChecks...)
	categoryTitles           = []string{"Local machine", "AWS", "Network", "Dependencies"}
)

// runDoctor runs `doctor` with args at 80 columns, the report on stdout and cobra's own output
// on the returned buffer.
func runDoctor(t *testing.T, stdout io.Writer, args ...string) (stderr *bytes.Buffer, err error) {
	t.Helper()
	return runDoctorWithColumns(t, stdout, "80", args...)
}

// runDoctorWithColumns is runDoctor with COLUMNS set to columns. A hermetic AWS environment
// makes the credentials check pass on any host; a free --port keeps a local adb server out of
// the report.
func runDoctorWithColumns(t *testing.T, stdout io.Writer, columns string, args ...string) (stderr *bytes.Buffer, err error) {
	t.Helper()
	testsupport.HermeticAWSEnv(t)
	t.Setenv("COLUMNS", columns)
	root := newRootCommand()
	stderr = &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs(append([]string{"doctor", "--port", strconv.Itoa(freePort(t))}, args...))
	return stderr, root.Execute()
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// parseTable reads a table report: the category titles in print order, each row's status, and
// the count line. Continuation lines and the What to do list carry no title.
func parseTable(t *testing.T, report string) (titles []string, statuses map[string]string, countLine string) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(report, "\n"), "\n")
	require.Greater(t, len(lines), 3, "a header, two rules and the count line at least")
	statuses = map[string]string{}
	for _, line := range lines[3 : len(lines)-1] {
		if !strings.HasPrefix(line, "| ") {
			continue
		}
		cells := strings.Split(line, "|")
		if title := strings.TrimSpace(cells[1]); title != "" {
			titles = append(titles, title)
			statuses[title] = strings.TrimSpace(cells[2])
		}
	}
	return titles, statuses, lines[len(lines)-1]
}

// parseText reads a --verbose report: the check ids in print order, each row's bracket tag, and
// the count line.
func parseText(t *testing.T, report string) (ids []string, tags map[string]string, countLine string) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(report, "\n"), "\n")
	require.NotEmpty(t, lines)
	tags = map[string]string{}
	for _, line := range lines[:len(lines)-1] {
		tag, rest, _ := strings.Cut(line, " ")
		id, _, _ := strings.Cut(rest, ":")
		require.Regexp(t, `^\[(PASS|WARN|FAIL|SKIP)\]$`, tag, "every line but the last is a row: %q", line)
		ids = append(ids, id)
		tags[id] = tag
	}
	return ids, tags, lines[len(lines)-1]
}

func TestGivenNoDaemonAndNoSessionArnWhenDoctorRunsThenTheTableAndTheCountLineGoToStdoutAndItExitsZero(t *testing.T) {
	shortRuntimeDir(t)
	stdout := &bytes.Buffer{}

	stderr, err := runDoctor(t, stdout, "--debug")

	require.NoError(t, err)
	assert.Empty(t, stderr.String(), "nothing reaches the Err stream: no cobra output, no log line")
	titles, statuses, countLine := parseTable(t, stdout.String())
	assert.Equal(t, categoryTitles, titles)
	assert.Equal(t, map[string]string{"Local machine": "PASS", "AWS": "PASS", "Network": "SKIP", "Dependencies": "SKIP"}, statuses)
	assert.Contains(t, stdout.String(), "| AWS           | PASS   | AWS credentials ")
	assert.Contains(t, stdout.String(), "|               |        | remote access session not checked: pass ")
	assert.Contains(t, stdout.String(), "| Network       | SKIP   | not checked: needs remote access session ")
	assert.NotContains(t, stdout.String(), "What to do")
	assert.Equal(t, "doctor: 2 of 4 categories passed, 2 not checked", countLine)
	for _, line := range strings.Split(stdout.String(), "\n") {
		assert.LessOrEqual(t, len(line), 80, line)
	}
	for _, id := range doctorCheckIDs {
		assert.NotContains(t, stdout.String(), id, "the table names no check id")
	}
}

func TestGivenARuntimeDirVariableNamingARegularFileWhenDoctorRunsThenTheLocalRowFailsOnStdoutAndTheCommandErrors(t *testing.T) {
	// The root hook would fail the command before RunE with no row printed; the doctor's
	// own hook lets the check report the refusal.
	notADir := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(notADir, nil, 0o600))
	isolateRuntimeDir(t, notADir)
	stdout := &bytes.Buffer{}

	_, err := runDoctor(t, stdout)

	require.EqualError(t, err, "doctor: 1 checks failed")
	titles, statuses, countLine := parseTable(t, stdout.String())
	assert.Equal(t, categoryTitles, titles, "a failed check stops nothing but its dependents")
	assert.Equal(t, "FAIL", statuses["Local machine"])
	assert.Contains(t, stdout.String(), "| Local machine | FAIL   | working directory: cannot use the working")
	assert.Contains(t, stdout.String(), "also passed: adb forward")
	assert.Contains(t, stdout.String(), "| bridge not checked: working directory")
	assert.Contains(t, stdout.String(), "| listen port not checked: working directory failed")
	assert.Contains(t, stdout.String(), "\nWhat to do\n1. Local machine, working directory: the client cannot use this working")
	assert.Contains(t, stdout.String(), "'"+daemon.RuntimeDirEnvVar+"'", "the hint names this OS's variable")
	assert.Equal(t, "doctor: 1 of 4 categories passed, 1 failed, 2 not checked", countLine)
	for _, id := range doctorCheckIDs {
		assert.NotContains(t, stdout.String(), id, "the table and the list name no check id")
	}
}

func TestGivenAFailedCheckWhenDoctorIsExecutedThenStderrNamesTheFailureWithoutAUsageFix(t *testing.T) {
	// execute calls any failure before the command line is accepted a usage mistake. The
	// doctor's hook must accept it, or a failed check would point at --help.
	notADir := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(notADir, nil, 0o600))
	isolateRuntimeDir(t, notADir)
	testsupport.HermeticAWSEnv(t)
	state := &rootState{cfg: config.Default()}
	root := newRootCommandWithState(state)
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs([]string{"doctor", "--port", strconv.Itoa(freePort(t))})

	exitCode := execute(root, state)

	assert.Equal(t, 1, exitCode)
	assert.Equal(t, config.BinaryName+": error: doctor: 1 checks failed\n", stderr.String())
	assert.Contains(t, stdout.String(), "doctor: 1 of 4 categories passed, 1 failed, 2 not checked")
}

func TestGivenVerboseWhenDoctorRunsThenOneLinePerCheckWithItsIdAndTheCountLineGoToStdout(t *testing.T) {
	shortRuntimeDir(t)
	stdout := &bytes.Buffer{}

	stderr, err := runDoctor(t, stdout, "--verbose")

	require.NoError(t, err)
	assert.Empty(t, stderr.String())
	ids, tags, countLine := parseText(t, stdout.String())
	assert.Equal(t, doctorCheckIDs, ids)
	for _, id := range localAndCredentialChecks {
		assert.Equal(t, "[PASS]", tags[id], id)
	}
	for _, id := range sessionAndRelayChecks {
		assert.Equal(t, "[SKIP]", tags[id], "the session and relay checks need --session-arn: %s", id)
	}
	assert.Equal(t, "doctor: 5 passed, 0 warnings, 0 failed, 5 skipped", countLine)
}

func TestGivenColumnsWhenDoctorRunsThenTheTableWidthFollowsItAndAnUnusableValueFallsBackTo80(t *testing.T) {
	tests := []struct {
		columns   string
		wantWidth int
	}{
		{columns: "100", wantWidth: 100},
		{columns: "abc", wantWidth: 80},
		{columns: "0", wantWidth: 80},
	}
	for _, tc := range tests {
		t.Run("COLUMNS="+tc.columns, func(t *testing.T) {
			shortRuntimeDir(t)
			stdout := &bytes.Buffer{}

			_, err := runDoctorWithColumns(t, stdout, tc.columns)

			require.NoError(t, err)
			assert.Len(t, strings.Split(stdout.String(), "\n")[0], tc.wantWidth)
		})
	}
}

func TestGivenAStrayArgumentWhenDoctorRunsThenItIsRejectedBeforeAnyCheck(t *testing.T) {
	shortRuntimeDir(t)
	stdout := &bytes.Buffer{}

	_, err := runDoctor(t, stdout, "extra")

	require.Error(t, err)
	assert.Empty(t, stdout.String())
}

type refusingWriter struct{}

func (refusingWriter) Write([]byte) (int, error) { return 0, errors.New("stdout is closed") }

func TestGivenAStdoutThatRefusesWritesWhenDoctorRunsThenTheWriteErrorIsTheExitError(t *testing.T) {
	// A report nobody received must not exit 0.
	shortRuntimeDir(t)

	_, err := runDoctor(t, refusingWriter{})

	require.EqualError(t, err, "stdout is closed")
}

func TestGivenListenerAndSessionFlagsWhenParsedOnDoctorThenTheyBindToTheStateAndStayOffBridgeStatus(t *testing.T) {
	state := &rootState{cfg: config.Default()}
	doctorCommand := findChild(t, newRootCommandWithState(state), "doctor")

	require.NoError(t, doctorCommand.ParseFlags([]string{"--port", "5555", "--listen-addr", "::1",
		"--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0"}))

	assert.Equal(t, 5555, state.cfg.ListenPort)
	assert.Equal(t, "::1", state.cfg.ListenAddr)
	assert.Equal(t, "arn:aws:devicefarm:us-west-2:1:session:a/b/0", state.sessionARN)
	// Bound on the leaf, not the root: a command that finds the daemon through its socket
	// still refuses the port.
	_, err := runRoot(t, "bridge", "status", "--port", "5555")
	require.Error(t, err)
}

func TestGivenJSONRequestedWhenDoctorRunsThenStdoutIsExactlyOneObjectWithTheSameExitCode(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(notADir, nil, 0o600))
	tests := []struct {
		name       string
		runtimeDir func(t *testing.T)
		wantErr    string
		wantFails  int
	}{
		{name: "exit 0", runtimeDir: func(t *testing.T) { shortRuntimeDir(t) }},
		{name: "exit 1", runtimeDir: func(t *testing.T) { isolateRuntimeDir(t, notADir) }, wantErr: "doctor: 1 checks failed", wantFails: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.runtimeDir(t)
			stdout := &bytes.Buffer{}

			stderr, err := runDoctor(t, stdout, "--json")

			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.EqualError(t, err, tc.wantErr)
			}
			assert.Empty(t, stderr.String())
			var decoded struct {
				OK         bool              `json:"ok"`
				SessionARN string            `json:"sessionArn"`
				Checks     []json.RawMessage `json:"checks"`
				Summary    map[string]int    `json:"summary"`
			}
			require.NoError(t, json.Unmarshal(stdout.Bytes(), &decoded), "stdout must be one object: %q", stdout.String())
			assert.Equal(t, 1, strings.Count(stdout.String(), "\n"), "one line, nothing else on stdout")
			assert.Equal(t, err == nil, decoded.OK, "ok follows the exit code")
			assert.Empty(t, decoded.SessionARN)
			assert.Len(t, decoded.Checks, len(doctorCheckIDs))
			assert.Equal(t, tc.wantFails, decoded.Summary["fail"])
		})
	}
}
