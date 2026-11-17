// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/control"
	"github.com/aws/device-connect-devicefarm/internal/daemon"
	"github.com/aws/device-connect-devicefarm/internal/failure"
	"github.com/aws/device-connect-devicefarm/internal/version"
)

// runExecute runs the command tree through execute against the current runtime dir:
// the exit code and everything written to the command's streams, in one buffer.
func runExecute(t *testing.T, args ...string) (int, string) {
	t.Helper()
	state := &rootState{cfg: config.Default()}
	root := newRootCommandWithState(state)
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)
	return execute(root, state), out.String()
}

func TestGivenUnknownSubcommandWhenExecutedThenNamesTheCommandsHelpAndExitsOne(t *testing.T) {
	isolateRuntimeDir(t, t.TempDir())

	code, out := runExecute(t, "bridge", "bogus")

	assert.Equal(t, 1, code)
	// Literal on purpose: a changed BinaryName must fail here.
	assert.Equal(t,
		"devicefarm-connect: error: unknown command \"bogus\" for \"devicefarm-connect bridge\"\n"+
			"  fix:   run 'devicefarm-connect bridge --help' for usage\n",
		out)
}

func TestGivenUnknownFlagWhenExecutedThenNamesTheCommandsHelp(t *testing.T) {
	isolateRuntimeDir(t, t.TempDir())

	code, out := runExecute(t, "bridge", "status", "--port", "5555")

	assert.Equal(t, 1, code)
	assert.Contains(t, out, config.BinaryName+": error: unknown flag: --port\n")
	assert.Contains(t, out, "  fix:   run '"+config.BinaryName+" bridge status --help' for usage\n")
}

func TestGivenNoSessionArnWhenDeviceAddExecutedThenNamesDeviceAddHelp(t *testing.T) {
	isolateRuntimeDir(t, t.TempDir())

	code, out := runExecute(t, "device", "add")

	assert.Equal(t, 1, code)
	assert.Contains(t, out, config.BinaryName+": error: --session-arn is required\n")
	assert.Contains(t, out, "  fix:   run '"+config.BinaryName+" device add --help' for usage\n")
}

func TestGivenInvalidListenAddrWhenBridgeStartExecutedThenPrintsTheFlagFixThenTheUsageFix(t *testing.T) {
	isolateRuntimeDir(t, t.TempDir())

	code, out := runExecute(t, "bridge", "start", "--listen-addr", "[::1]")

	assert.Equal(t, 1, code)
	assert.Equal(t,
		config.BinaryName+": error: --listen-addr must be an IP address or hostname with no port or brackets, got \"[::1]\"\n"+
			"  fix:   set the port with --port\n"+
			"  fix:   run '"+config.BinaryName+" bridge start --help' for usage\n",
		out)
}

func TestGivenCommandLineMistakesWhenExecutedThenEachNamesTheCommandsHelp(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		problem string
		helpOf  string
	}{
		{name: "stray argument on a leaf", args: []string{"bridge", "status", "extra"},
			problem: `unknown command "extra" for "` + config.BinaryName + ` bridge status"`, helpOf: "bridge status"},
		{name: "flag value of the wrong type", args: []string{"bridge", "start", "--port", "abc"},
			problem: `invalid argument "abc" for "--port" flag: strconv.ParseInt: parsing "abc": invalid syntax`, helpOf: "bridge start"},
		{name: "flag value out of range", args: []string{"bridge", "start", "--port", "0"},
			problem: "--port must be from 1 to 65535, got 0", helpOf: "bridge start"},
		{name: "flag on the hidden help command names the root", args: []string{"help", "--bogus"},
			problem: "unknown flag: --bogus", helpOf: ""},
		{name: "unknown flag on a leaf", args: []string{"device", "list", "--list"},
			problem: "unknown flag: --list", helpOf: "device list"},
		{name: "a session on bridge start", args: []string{"bridge", "start", "--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0"},
			problem: "unknown flag: --session-arn", helpOf: "bridge start"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			isolateRuntimeDir(t, t.TempDir())

			code, out := runExecute(t, testCase.args...)

			assert.Equal(t, 1, code)
			helpOf := strings.TrimSpace(config.BinaryName + " " + testCase.helpOf)
			assert.Equal(t,
				config.BinaryName+": error: "+testCase.problem+"\n"+
					"  fix:   run '"+helpOf+" --help' for usage\n",
				out)
		})
	}
}

// yourOptionsRestart is the restart a command quotes: it cannot read the bridge's flags.
const yourOptionsRestart = "'devicefarm-connect bridge start' with the options that you started it with"

// otherVersionFixLine is the command's own fix line for a refusal with no fix, unless the bridge
// reports this program's version.
func otherVersionFixLine(restart string) string {
	return "  fix:   The running bridge is another version of devicefarm-connect than this command. Restart it: run " +
		restart + ". If the error remains, install the latest devicefarm-connect, then run 'devicefarm-connect doctor'. " +
		"If the doctor does not find the cause, contact AWS Support.\n"
}

func TestGivenTheBridgeRefusesWhenEachCommandExecutesThenItPrintsTheBridgesFixOrTheRestartFixForAnotherVersionWithNone(t *testing.T) {
	const sessionARN = "arn:aws:devicefarm:us-west-2:1:session:a/b/0"
	commands := []struct {
		args    []string
		problem string
		// restart is the restart the command's own fix quotes; an add's also quotes the add of its session.
		restart string
	}{
		{[]string{"device", "add", "--session-arn", sessionARN}, "could not attach the device",
			yourOptionsRestart + ", then run 'devicefarm-connect device add --session-arn " + sessionARN + "'"},
		{[]string{"device", "list"}, "could not list the devices", yourOptionsRestart},
		{[]string{"device", "remove"}, "could not detach the device", yourOptionsRestart},
	}
	const noVersion, otherVersion = "", "v0.0.1"
	replies := []struct {
		name           string
		programVersion string
		fixes          []string
		// ownFix: the command prints its own restart fix. Else it prints the bridge's fix, if any.
		ownFix bool
	}{
		{"a fix from a bridge of this version", version.String(), []string{"Run this."}, false},
		{"a fix from a bridge that reports no version", noVersion, []string{"Run this."}, false},
		{"a fix from a bridge of another version", otherVersion, []string{"Run this."}, false},
		{"no fix from a bridge that reports no version, as an older bridge", noVersion, nil, true},
		{"no fix from a bridge of another version", otherVersion, nil, true},
		{"no fix from a bridge of this version", version.String(), nil, false},
	}
	for _, command := range commands {
		for _, reply := range replies {
			fixLines := ""
			switch {
			case reply.ownFix:
				fixLines = otherVersionFixLine(command.restart)
			case len(reply.fixes) > 0:
				fixLines = "  fix:   Run this.\n"
			}
			t.Run(strings.Join(command.args[:2], " ")+", "+reply.name, func(t *testing.T) {
				serveControlOfProgramVersion(t, fakeRuntimePaths(t), reply.programVersion, func(control.Request) control.Response {
					return control.Response{OK: false, Error: "refused", Fixes: reply.fixes}
				})

				code, out := runExecute(t, command.args...)

				assert.Equal(t, 1, code)
				assert.Equal(t, config.BinaryName+": error: "+command.problem+"\n"+"  cause: refused\n"+fixLines, out)
			})
		}
	}
}

func TestGivenDaemonRefusesAddWithFixesWhenDeviceAddExecutedThenEachFixIsAFixLineInTheDaemonsOrder(t *testing.T) {
	fakeDaemon(t, func(control.Request) control.Response {
		return control.Response{OK: false, Error: "a different device is already attached (Other)",
			Fixes: []string{"run '" + config.BinaryName + " device remove' first", "then add this device"}}
	})

	code, out := runExecute(t, "device", "add", "--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0")

	assert.Equal(t, 1, code)
	assert.Equal(t,
		config.BinaryName+": error: could not attach the device\n"+
			"  cause: a different device is already attached (Other)\n"+
			"  fix:   run '"+config.BinaryName+" device remove' first\n"+
			"  fix:   then add this device\n",
		out)
}

func TestGivenBridgeStartFailsWhenDeviceAddExecutedThenPrintsTheStartNoteProblemCauseAndLogFix(t *testing.T) {
	shortRuntimeDir(t)
	stubBridgeStart(t, func(*daemon.DaemonManager) (daemon.StartResult, error) {
		return daemon.StartResult{}, failure.WithFix(
			errors.New(`timed out after 5s waiting for pidfile "/tmp/d.pid"`), "read the bridge log: /tmp/d.log")
	})

	code, out := runExecute(t, "device", "add", "--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0")

	assert.Equal(t, 1, code)
	assert.Equal(t,
		"* bridge not running; starting it now\n"+
			config.BinaryName+": error: could not start the bridge\n"+
			"  cause: timed out after 5s waiting for pidfile \"/tmp/d.pid\"\n"+
			"  fix:   read the bridge log: /tmp/d.log\n",
		out)
}

func TestGivenNoDaemonWhenDeviceRemoveExecutedThenSaysNotListening(t *testing.T) {
	shortRuntimeDir(t)

	code, out := runExecute(t, "device", "remove")

	assert.Equal(t, 1, code)
	assert.Equal(t, config.BinaryName+": error: the bridge is not listening\n", out)
}

func TestGivenDaemonBodyFailsWhenExecutedThenLogsTheFailureAndPrintsNoBlock(t *testing.T) {
	// The daemon's stderr is its logfile, so the failure stays a timestamped log line.
	isolateRuntimeDir(t, t.TempDir())
	pinAWSEnv(t)

	code, out := runExecute(t, "daemon", "--port", "0")

	assert.Equal(t, 1, code)
	assert.Contains(t, out, `level=ERROR msg="`+config.BinaryName+` daemon failed"`)
	assert.Contains(t, out, "invalid configuration")
	assert.NotContains(t, out, config.BinaryName+": error:")
}

func TestGivenDaemonBodyCannotLoadAwsConfigWhenExecutedThenLogsTheFailureAndPrintsNoBlock(t *testing.T) {
	isolateRuntimeDir(t, t.TempDir())
	pinAWSEnv(t)
	t.Setenv("AWS_PROFILE", "no-such-profile")

	code, out := runExecute(t, "daemon")

	assert.Equal(t, 1, code)
	assert.Contains(t, out, `level=ERROR msg="`+config.BinaryName+` daemon failed"`)
	assert.Contains(t, out, "cannot load the AWS configuration")
	assert.NotContains(t, out, config.BinaryName+": error:")
}

func TestGivenUnusableRuntimeDirWhenExecutedThenPrintsTheWorkingDirectoryProblemItsCauseAndTheDoctorFix(t *testing.T) {
	// A regular file where the runtime dir's parent must be: the dir cannot be created.
	blocker := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))
	isolateRuntimeDir(t, blocker)

	code, out := runExecute(t, "bridge", "status")

	assert.Equal(t, 1, code)
	assert.Equal(t, config.BinaryName+": error: cannot use the working directory\n"+
		"  cause: cannot create the working directory \""+filepath.Join(blocker, config.BinaryName)+"\": mkdir "+blocker+": not a directory\n"+
		"  fix:   run 'devicefarm-connect doctor' to see how to fix the working directory\n", out)
}

func TestGivenAWorkingDirectoryCauseWithItsOwnFixWhenWordedThenOnlyThatFixPrints(t *testing.T) {
	cause := failure.WithFix(errors.New(`working directory "/tmp/d" is owned by UID 0, not 1000`), "set XDG_RUNTIME_DIR to a private directory")

	var out strings.Builder
	failure.Write(&out, config.BinaryName, workingDirectoryFailure(cause))

	assert.Equal(t, config.BinaryName+": error: cannot use the working directory\n"+
		"  cause: working directory \"/tmp/d\" is owned by UID 0, not 1000\n"+
		"  fix:   set XDG_RUNTIME_DIR to a private directory\n", out.String())
}

func TestGivenCommandSucceedsWhenExecutedThenReturnsZero(t *testing.T) {
	isolateRuntimeDir(t, t.TempDir())

	code, out := runExecute(t, "bridge", "status")

	require.Equal(t, 0, code)
	assert.Equal(t, config.BinaryName+" bridge is not running\n", out)
}
