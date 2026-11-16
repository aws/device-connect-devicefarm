// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/control"
	"github.com/aws/device-connect-devicefarm/internal/daemon"
	"github.com/aws/device-connect-devicefarm/internal/failure"
	"github.com/aws/device-connect-devicefarm/internal/version"
	"github.com/aws/device-connect-devicefarm/internal/wording"
)

// TestMain blocks the real bridge start and stop: a test that needs one uses
// fakeBridgeStart or fakeBridgeStop; any other test that reaches them has a bug.
func TestMain(m *testing.M) {
	startBridge = func(*daemon.DaemonManager) (daemon.StartResult, error) {
		panic("a test reached the real bridge start; use fakeBridgeStart")
	}
	stopBridge = func(*daemon.DaemonManager) (daemon.StopResult, error) {
		panic("a test reached the real bridge stop; use fakeBridgeStop")
	}
	os.Exit(m.Run())
}

// shortRuntimeDir points the runtime dir at a fresh dir under /tmp and returns the
// paths execRoot resolves there. Not t.TempDir() or $TMPDIR: t.TempDir embeds the long
// test name and the macOS $TMPDIR is long. DefaultPaths appends
// devicefarm-connect/devicefarm-connect.sock, and the whole path must stay under the
// ~104-byte Unix-domain socket path limit.
func shortRuntimeDir(t *testing.T) daemon.Paths {
	t.Helper()
	runtimeDir, err := os.MkdirTemp("/tmp", "adbcli")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	isolateRuntimeDir(t, runtimeDir)

	paths, err := daemon.DefaultPaths()
	require.NoError(t, err)
	return paths
}

// writeLivePidfile writes a live pidfile (this test process) at paths, so Manager.Status
// reports "running" and Manager.Start no-ops.
func writeLivePidfile(t *testing.T, paths daemon.Paths) {
	t.Helper()
	// The PidFile holds the lock and must stay reachable until cleanup.
	pidFile := daemon.NewPidFile(paths.PidFile, nil)
	require.NoError(t, pidFile.Write())
	t.Cleanup(func() { _ = pidFile.Remove() })
}

// fakeRuntimePaths is shortRuntimeDir plus a live pidfile: a daemon that looks running.
func fakeRuntimePaths(t *testing.T) daemon.Paths {
	t.Helper()
	paths := shortRuntimeDir(t)
	writeLivePidfile(t, paths)
	return paths
}

// serveControl serves handler on a real control socket at paths, so command tests run
// end to end over a Unix socket without forking the daemon. It reports this program's version.
func serveControl(t *testing.T, paths daemon.Paths, handler control.Handler) {
	t.Helper()
	serveControlOfProgramVersion(t, paths, version.String(), handler)
}

// serveControlOfProgramVersion is serveControl for a bridge that reports programVersion on every
// reply; "" reports none, as a bridge from before the field does.
func serveControlOfProgramVersion(t *testing.T, paths daemon.Paths, programVersion string, handler control.Handler) {
	t.Helper()
	server := control.NewLocalSocketServer(paths.ControlSocket, programVersion, slog.New(slog.DiscardHandler))
	require.NoError(t, server.Listen())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = server.Serve(ctx, handler)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = server.Close()
	})
}

// fakeDaemon is a daemon that looks running and answers handler on its control socket.
func fakeDaemon(t *testing.T, handler control.Handler) {
	t.Helper()
	serveControl(t, fakeRuntimePaths(t), handler)
}

// stubBridgeStart replaces the bridge start for one test.
func stubBridgeStart(t *testing.T, start func(*daemon.DaemonManager) (daemon.StartResult, error)) {
	t.Helper()
	previous := startBridge
	startBridge = start
	t.Cleanup(func() { startBridge = previous })
}

// fakeBridgeStop replaces the bridge stop for one test; returns a call count.
func fakeBridgeStop(t *testing.T, result daemon.StopResult, err error) *int {
	t.Helper()
	previous := stopBridge
	calls := 0
	stopBridge = func(*daemon.DaemonManager) (daemon.StopResult, error) {
		calls++
		return result, err
	}
	t.Cleanup(func() { stopBridge = previous })
	return &calls
}

// fakeBridgeStart makes a bridge start write the live pidfile and serve handler at paths
// instead of forking. The result names this process's pid and the paths' logfile,
// as the real Start would.
func fakeBridgeStart(t *testing.T, paths daemon.Paths, handler control.Handler) {
	t.Helper()
	fakeBridgeStartWithResult(t, paths, handler, daemon.StartResult{PID: os.Getpid(), LogFile: paths.LogFile})
}

// fakeBridgeStartWithResult is fakeBridgeStart with a caller-supplied result.
func fakeBridgeStartWithResult(t *testing.T, paths daemon.Paths, handler control.Handler, result daemon.StartResult) {
	t.Helper()
	stubBridgeStart(t, func(*daemon.DaemonManager) (daemon.StartResult, error) {
		writeLivePidfile(t, paths)
		serveControl(t, paths, handler)
		return result, nil
	})
}

func TestGivenRunningDaemonWhenAddDeviceInvokedThenPrintsListeningLineWithLabelAndPort(t *testing.T) {
	// The daemon resolves the ARN and returns the device label; the CLI just
	// forwards the ARN and prints what comes back.
	fakeDaemon(t, func(req control.Request) control.Response {
		assert.Equal(t, control.OpAdd, req.Op)
		assert.Equal(t, "arn:aws:devicefarm:us-west-2:1:session:a/b/0", req.ARN)
		return control.Response{OK: true, DeviceLabel: "Google Pixel 9 (OS 16)", Port: 5037}
	})

	out, err := execRoot(t, "device", "add", "--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0")

	require.NoError(t, err)
	// The port comes from the daemon's response (device add no longer carries a
	// --port/--listen-addr flag), and the address is not printed.
	assert.Contains(t, out.String(), "listening on port 5037")
	assert.Contains(t, out.String(), "connected device: Google Pixel 9 (OS 16)")
	assert.NotContains(t, out.String(), "device (Google Pixel 9 (OS 16))",
		"the label already carries parentheses; it must not be wrapped in another set")
}

func TestGivenDaemonRejectsAddWhenAddDeviceInvokedThenErrorsWithDaemonReason(t *testing.T) {
	fakeDaemon(t, func(control.Request) control.Response {
		return control.Response{OK: false, Error: "a different device is already attached (Other)"}
	})

	_, err := execRoot(t, "device", "add", "--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "already attached")
}

func TestGivenDaemonCannotResolveWhenAddDeviceInvokedThenSurfacesResolveError(t *testing.T) {
	// Resolution now happens in the daemon, so a bad session surfaces as the add
	// response error rather than a pre-spawn CLI failure.
	fakeDaemon(t, func(control.Request) control.Response {
		return control.Response{OK: false, Error: "session not RUNNING"}
	})

	_, err := execRoot(t, "device", "add", "--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "session not RUNNING")
}

func TestGivenNoControlSocketWhenAddDeviceInvokedThenReturnsNotListening(t *testing.T) {
	// The live pidfile says a daemon runs, so add starts none; the socket is unbound.
	fakeRuntimePaths(t)

	_, err := execRoot(t, "device", "add", "--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0")

	require.ErrorIs(t, err, errNotListening)
}

func TestGivenNoBridgeWhenAddDeviceInvokedThenStartsTheBridgeAndAttaches(t *testing.T) {
	paths := shortRuntimeDir(t)
	fakeBridgeStart(t, paths, func(req control.Request) control.Response {
		assert.Equal(t, control.OpAdd, req.Op)
		return control.Response{OK: true, DeviceLabel: "Google Pixel 9 (OS 16)", Port: 5037}
	})

	out, err := execRoot(t, "device", "add", "--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0")

	require.NoError(t, err)
	assert.Contains(t, out.String(), "* bridge not running; starting it now")
	assert.Contains(t, out.String(), "listening on port 5037 with connected device: Google Pixel 9 (OS 16)")
}

func TestGivenBridgeStartFailsWhenAddDeviceInvokedThenErrorsWithTheStartFailure(t *testing.T) {
	shortRuntimeDir(t)
	startFailure := errors.New("spawn bridge: exec format error")
	stubBridgeStart(t, func(*daemon.DaemonManager) (daemon.StartResult, error) { return daemon.StartResult{}, startFailure })

	out, err := execRoot(t, "device", "add", "--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0")

	require.ErrorIs(t, err, startFailure)
	assert.ErrorContains(t, err, "could not start the bridge")
	assert.NotContains(t, out.String(), "listening on port")
}

func TestGivenRunningDaemonWithDeviceWhenRemoveDeviceInvokedThenPrintsRemovedLine(t *testing.T) {
	fakeDaemon(t, func(req control.Request) control.Response {
		assert.Equal(t, control.OpRemove, req.Op)
		return control.Response{OK: true, DeviceLabel: "Google Pixel 9 (OS 16)"}
	})

	out, err := execRoot(t, "device", "remove", "--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0")

	require.NoError(t, err)
	assert.Contains(t, out.String(), "Removed Google Pixel 9 (OS 16) from the bridge")
}

func TestGivenRunningDaemonWithDeviceWhenRemoveDeviceInvokedWithoutARNThenRemovesTheAttachedDevice(t *testing.T) {
	fakeDaemon(t, func(req control.Request) control.Response {
		assert.Equal(t, control.OpRemove, req.Op)
		assert.Empty(t, req.ARN, "no --session-arn must reach the daemon as an empty ARN")
		return control.Response{OK: true, DeviceLabel: "Google Pixel 9 (OS 16)"}
	})

	out, err := execRoot(t, "device", "remove")

	require.NoError(t, err)
	assert.Contains(t, out.String(), "Removed Google Pixel 9 (OS 16) from the bridge")
}

func TestGivenSessionArnHelpWhenPrintedThenOnlyRemoveCallsItOptional(t *testing.T) {
	remove, err := runRoot(t, "device", "remove", "--help")
	require.NoError(t, err)
	assert.Contains(t, remove.String(), "(optional; without it, the attached device is detached)")

	add, err := runRoot(t, "device", "add", "--help")
	require.NoError(t, err)
	assert.NotContains(t, add.String(), "optional", "device add still requires --session-arn")
}

func TestGivenDaemonHasNoDeviceWhenRemoveDeviceInvokedThenErrorsWithDaemonReason(t *testing.T) {
	fakeDaemon(t, func(control.Request) control.Response {
		return control.Response{OK: false, Error: "no device is attached"}
	})

	_, err := execRoot(t, "device", "remove", "--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no device is attached")
}

func TestGivenNoBridgeWhenRemoveDeviceInvokedThenReturnsNotListeningAndStartsNone(t *testing.T) {
	// No pidfile and no socket. TestMain panics on a real start, so this also proves
	// remove starts nothing.
	shortRuntimeDir(t)

	out, err := execRoot(t, "device", "remove", "--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0")

	require.ErrorIs(t, err, errNotListening)
	assert.NotContains(t, out.String(), "starting it now")
}

func TestGivenNoControlSocketWhenRemoveDeviceInvokedThenReturnsNotListening(t *testing.T) {
	// A live pidfile with no socket bound. Remove never starts a daemon, so the dead
	// socket is what it reports.
	fakeRuntimePaths(t)

	_, err := execRoot(t, "device", "remove", "--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0")

	require.ErrorIs(t, err, errNotListening)
}

func TestGivenDaemonClosesWithoutReplyWhenRemoveDeviceInvokedThenSurfacesTransportError(t *testing.T) {
	// A daemon that reads the request then hangs up is not "not running".
	// Decoding the request first lets the client's write land, so the client
	// sees io.EOF, not a reset. That transport error must reach the user unchanged.
	paths := fakeRuntimePaths(t)
	listener, err := net.Listen("unix", paths.ControlSocket)
	require.NoError(t, err)
	served := make(chan struct{})
	t.Cleanup(func() {
		_ = listener.Close()
		<-served
	})
	go func() {
		defer close(served)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		var request control.Request
		_ = json.NewDecoder(conn).Decode(&request)
		_ = conn.Close()
	}()

	_, err = execRoot(t, "device", "remove", "--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0")

	require.Error(t, err)
	assert.NotErrorIs(t, err, control.ErrDaemonNotRunning)
	assert.ErrorIs(t, err, io.EOF)
	assert.ErrorContains(t, err, "the bridge did not answer: read control response")
}

// fakeDaemonWithPixel is a daemon with one attached device. Ago offsets only grow; the expiry
// offset sits mid-minute. A few seconds of test time keep every floored minute.
func fakeDaemonWithPixel(t *testing.T) {
	t.Helper()
	now := time.Now()
	fakeDaemon(t, func(req control.Request) control.Response {
		assert.Equal(t, control.OpList, req.Op)
		return control.Response{OK: true, Devices: []control.AttachedDevice{
			{SessionARN: "arn:aws:devicefarm:us-west-2:1:session:a/b/0", Label: "Google Pixel 9 (OS 16)",
				Serial: "R3CN30ABCDE", AttachedAt: now.Add(-12 * time.Minute),
				SessionStartedAt: now.Add(-25 * time.Minute), SessionExpiresAt: now.Add(125*time.Minute + 30*time.Second)},
		}}
	})
}

func TestGivenRunningDaemonWithDeviceWhenListDevicesInvokedThenPrintsATableWithoutTheSessionArn(t *testing.T) {
	fakeDaemonWithPixel(t)
	t.Setenv("COLUMNS", "80") // the buffer is no terminal; a developer's export must not move the width

	out, err := execRoot(t, "device", "list")

	require.NoError(t, err)
	assert.Equal(t, `+----------------+-------------+----------------+------------------------------+
| Device         | Serial      | Attached       | Session                      |
+----------------+-------------+----------------+------------------------------+
| Google Pixel 9 | R3CN30ABCDE | 12 minutes ago | started 25 minutes ago       |
|   (OS 16)      |             |                | expires in 2 hours 5 minutes |
+----------------+-------------+----------------+------------------------------+
`, out.String())
}

func TestGivenRunningDaemonWithDeviceWhenListDevicesInvokedWithListThenPrintsOneLinePerDevice(t *testing.T) {
	fakeDaemonWithPixel(t)

	out, err := execRoot(t, "device", "list", "--list")

	require.NoError(t, err)
	assert.Equal(t, "arn:aws:devicefarm:us-west-2:1:session:a/b/0\tGoogle Pixel 9 (OS 16)\tR3CN30ABCDE\t"+
		"12 minutes ago\t25 minutes ago\tin 2 hours 5 minutes\n", out.String())
}

func TestGivenAStdoutThatRefusesWritesWhenListDevicesInvokedThenEveryViewReturnsTheWriteError(t *testing.T) {
	// A list nobody received must not exit 0.
	for _, flags := range [][]string{nil, {"--list"}, {"--json"}} {
		t.Run(fmt.Sprint(flags), func(t *testing.T) {
			fakeDaemonWithPixel(t)
			root := newRootCommand()
			root.SetOut(refusingWriter{})
			root.SetErr(io.Discard)
			root.SetArgs(append([]string{"device", "list"}, flags...))

			require.EqualError(t, root.Execute(), "stdout is closed")
		})
	}
}

func TestGivenRunningDaemonWithoutDeviceWhenListDevicesInvokedThenSaysNoDevice(t *testing.T) {
	fakeDaemon(t, func(control.Request) control.Response {
		return control.Response{OK: true}
	})

	out, err := execRoot(t, "device", "list")

	require.NoError(t, err)
	assert.Equal(t, "no device attached; use 'device add' to connect one\n", out.String())
}

func TestGivenDaemonRefusesListWhenListDevicesInvokedThenErrorsWithDaemonReason(t *testing.T) {
	fakeDaemon(t, func(control.Request) control.Response {
		return control.Response{OK: false, Error: "unknown control operation \"list\""}
	})

	_, err := execRoot(t, "device", "list")

	require.ErrorContains(t, err, "could not list the devices: unknown control operation \"list\"")
}

func TestGivenNoBridgeWhenListDevicesInvokedWithJSONThenStdoutHoldsOnlyTheObject(t *testing.T) {
	// --json must pipe: the start notice goes to the error stream.
	paths := shortRuntimeDir(t)
	fakeBridgeStart(t, paths, func(control.Request) control.Response {
		return control.Response{OK: true}
	})
	root := newRootCommand()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs([]string{"device", "list", "--json"})

	require.NoError(t, root.Execute())

	assert.Equal(t, `{"devices":[]}`+"\n", stdout.String())
	assert.Equal(t, "* bridge not running; starting it now\n"+
		fmt.Sprintf("* bridge started (PID %d); log file: %s\n", os.Getpid(), paths.LogFile), stderr.String())
}

func TestGivenNoBridgeWhenListDevicesInvokedThenStartsTheBridge(t *testing.T) {
	paths := shortRuntimeDir(t)
	fakeBridgeStart(t, paths, func(control.Request) control.Response {
		return control.Response{OK: true}
	})

	out, err := execRoot(t, "device", "list")

	require.NoError(t, err)
	assert.Contains(t, out.String(), "* bridge not running; starting it now")
}

// statusRoot runs `bridge status` with the given flags, stdout and stderr
// captured apart.
func statusRoot(t *testing.T, flags ...string) (stdout, stderr *bytes.Buffer, err error) {
	t.Helper()
	root := newRootCommand()
	stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs(append([]string{"bridge", "status"}, flags...))
	return stdout, stderr, root.Execute()
}

func TestGivenRunningDaemonWithDevicesWhenStatusInvokedThenTheCountLineIsOnStdoutAndStderrIsEmpty(t *testing.T) {
	pixel := control.AttachedDevice{SessionARN: "arn:aws:devicefarm:us-west-2:1:session:a/b/0", Label: "Google Pixel 8 (OS 15)"}
	tests := []struct {
		name    string
		devices []control.AttachedDevice
		want    string
	}{
		{name: "one device", devices: []control.AttachedDevice{pixel}, want: "1 device attached"},
		{name: "two devices", devices: []control.AttachedDevice{pixel, pixel}, want: "2 devices attached"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeDaemon(t, func(req control.Request) control.Response {
				assert.Equal(t, control.OpStatus, req.Op)
				return control.Response{OK: true, DeviceLabel: pixel.Label, ARN: pixel.SessionARN, Devices: tc.devices}
			})

			stdout, stderr, err := statusRoot(t)

			require.NoError(t, err)
			assert.Equal(t, fmt.Sprintf("devicefarm-connect bridge is running (PID %d), %s; run 'device list' for details\n",
				os.Getpid(), tc.want), stdout.String())
			assert.Empty(t, stderr.String())
		})
	}
}

func TestGivenRunningDaemonWithoutDeviceWhenStatusInvokedThenTheNoDeviceLineAndNoteAreOnStdout(t *testing.T) {
	detachedAt := time.Now().Add(-42 * time.Second)
	fakeDaemon(t, func(control.Request) control.Response {
		return control.Response{OK: true, LastDetachReason: "the session has ended", LastDetachAt: detachedAt}
	})

	stdout, stderr, err := statusRoot(t)

	require.NoError(t, err)
	// Literal on purpose: a changed BinaryName must fail here.
	assert.Contains(t, stdout.String(), "devicefarm-connect bridge is running (PID ")
	assert.Contains(t, stdout.String(), "no device attached; the last device detached")
	assert.Contains(t, stdout.String(), "the session has ended")
	assert.Empty(t, stderr.String())
}

func TestGivenPidfileButNoControlSocketWhenStatusInvokedThenNotRunningIsOnStdout(t *testing.T) {
	// Pidfile says running, no socket bound: status reports not running, no error.
	fakeRuntimePaths(t)

	stdout, stderr, err := statusRoot(t)

	require.NoError(t, err)
	assert.Equal(t, config.BinaryName+" bridge is not running\n", stdout.String())
	assert.Empty(t, stderr.String())
}

func TestGivenRunningDaemonWhenStatusInvokedWithJSONThenOneObjectCarriesRunningPidPortReverseAndDevices(t *testing.T) {
	pixel := control.AttachedDevice{SessionARN: "arn:aws:devicefarm:us-west-2:1:session:a/b/0", Label: "Google Pixel 8 (OS 15)"}
	fakeDaemon(t, func(control.Request) control.Response {
		return control.Response{OK: true, Port: 5037, ReverseEnabled: false, Devices: []control.AttachedDevice{pixel}}
	})

	stdout, stderr, err := statusRoot(t, "--json")

	require.NoError(t, err)
	var status map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &status))
	assert.Equal(t, true, status["running"])
	assert.Equal(t, float64(os.Getpid()), status["pid"])
	assert.Equal(t, float64(5037), status["port"])
	reverse, present := status["reverseEnabled"]
	assert.True(t, present, "reverseEnabled appears also when false")
	assert.Equal(t, false, reverse)
	assert.Len(t, status["devices"], 1)
	assert.NotContains(t, status, "lastDetach")
	assert.Empty(t, stderr.String())
}

func TestGivenLastDetachWhenStatusInvokedWithJSONThenLastDetachCarriesReasonAndTime(t *testing.T) {
	detachedAt := time.Now().UTC().Truncate(time.Second)
	fakeDaemon(t, func(control.Request) control.Response {
		return control.Response{OK: true, Port: 5037, LastDetachReason: "the session has ended", LastDetachAt: detachedAt}
	})

	stdout, _, err := statusRoot(t, "--json")

	require.NoError(t, err)
	var status struct {
		LastDetach struct {
			Reason string    `json:"reason"`
			At     time.Time `json:"at"`
		} `json:"lastDetach"`
	}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &status))
	assert.Equal(t, "the session has ended", status.LastDetach.Reason)
	assert.True(t, status.LastDetach.At.Equal(detachedAt))
}

func TestGivenPidfileButNoControlSocketWhenStatusInvokedWithJSONThenRunningIsFalse(t *testing.T) {
	// The trickiest state: a live pidfile over a dead socket must fold into the
	// same exact object as no daemon at all, with no pid member.
	fakeRuntimePaths(t)

	stdout, stderr, err := statusRoot(t, "--json")

	require.NoError(t, err)
	assert.Equal(t, `{"running":false,"devices":[]}`+"\n", stdout.String())
	assert.Empty(t, stderr.String())
}

func TestGivenNoDaemonWhenStatusInvokedWithJSONThenRunningIsFalseAndDevicesIsAnEmptyArray(t *testing.T) {
	shortRuntimeDir(t)

	stdout, stderr, err := statusRoot(t, "--json")

	require.NoError(t, err)
	assert.Equal(t, `{"running":false,"devices":[]}`+"\n", stdout.String(), "exact bytes: nothing else may appear")
	assert.Empty(t, stderr.String())
}

func TestGivenStdoutRefusesWritesWhenStatusInvokedThenTheWriteErrorIsReturned(t *testing.T) {
	fakeDaemon(t, func(control.Request) control.Response {
		return control.Response{OK: true}
	})
	root := newRootCommand()
	root.SetOut(refusingWriter{})
	root.SetErr(io.Discard)
	root.SetArgs([]string{"bridge", "status"})

	require.EqualError(t, root.Execute(), "stdout is closed")
}

// fakeRestart fakes the whole restart under a running daemon: a stop that
// returns result, a start of pid 4343, and handler on the control socket. It
// returns the stop-call and start-call counters.
func fakeRestart(t *testing.T, handler control.Handler, result daemon.StopResult) (stops *int, starts *int) {
	t.Helper()
	fakeDaemon(t, handler)
	return fakeBridgeStop(t, result, nil), countingBridgeStart(t)
}

// countingBridgeStart installs a start stub that returns pid 4343 with /tmp/d.log
// and counts its calls. It serves no control socket.
func countingBridgeStart(t *testing.T) *int {
	t.Helper()
	startCalls := 0
	stubBridgeStart(t, func(*daemon.DaemonManager) (daemon.StartResult, error) {
		startCalls++
		return daemon.StartResult{PID: 4343, LogFile: "/tmp/d.log"}, nil
	})
	return &startCalls
}

func TestGivenNoSessionARNWhenStartBridgeInvokedThenStartsWithoutAttaching(t *testing.T) {
	// The daemon's control handler must never see an add when no ARN is given.
	fakeRestart(t, func(req control.Request) control.Response {
		if req.Op == control.OpAdd {
			t.Error("bridge start without --session-arn must not attach a device")
		}
		return control.Response{OK: true, Port: 6000}
	}, daemon.StopResult{PID: 4242, WasRunning: true})

	out, err := execRoot(t, "bridge", "start", "--port", "5038")

	require.NoError(t, err, "bridge start must not require --session-arn")
	assert.Contains(t, out.String(), "listening on port 6000 with no device attached")
	assert.NotContains(t, out.String(), "5038", "the port must come from the daemon, not the --port flag")
}

func TestGivenRunningDaemonWhenStartBridgeInvokedThenStopsItSaysSoThenStarts(t *testing.T) {
	stops, starts := fakeRestart(t, func(control.Request) control.Response {
		return control.Response{OK: true, Port: 5037}
	}, daemon.StopResult{PID: 4242, WasRunning: true})

	out, err := execRoot(t, "bridge", "start")

	require.NoError(t, err)
	stopped := strings.Index(out.String(),
		"* a bridge was running (PID 4242) and was stopped; its attached devices and forward rules were dropped, and its reverse rules no longer reach this machine")
	started := strings.Index(out.String(), "* bridge started (PID 4343); log file: /tmp/d.log")
	listening := strings.Index(out.String(), "listening on port 5037")
	require.GreaterOrEqual(t, stopped, 0, out.String())
	require.GreaterOrEqual(t, started, 0, out.String())
	require.GreaterOrEqual(t, listening, 0, out.String())
	assert.Less(t, stopped, started, "the stopped line comes first")
	assert.Less(t, started, listening, "the listening line comes last")
	assert.Equal(t, 1, *stops)
	assert.Equal(t, 1, *starts)
}

func TestGivenRunningDaemonThatHadToBeKilledWhenStartBridgeInvokedThenTheLineSaysKilled(t *testing.T) {
	fakeRestart(t, func(control.Request) control.Response {
		return control.Response{OK: true, Port: 5037}
	}, daemon.StopResult{PID: 4242, WasRunning: true, Killed: true})

	out, err := execRoot(t, "bridge", "start")

	require.NoError(t, err)
	assert.Contains(t, out.String(),
		"* a bridge was running (PID 4242), did not stop cleanly, and was killed; its attached devices and forward rules were dropped, and its reverse rules no longer reach this machine")
}

func TestGivenRunningDaemonThatSurvivesTheStopWhenStartBridgeInvokedThenErrorsAndStartsNothing(t *testing.T) {
	fakeRuntimePaths(t)
	stopFailure := failure.WithFix(errors.New("find bridge process 4242: access is denied"), "stop it from an elevated shell")
	fakeBridgeStop(t, daemon.StopResult{}, stopFailure)
	starts := countingBridgeStart(t)

	_, err := execRoot(t, "bridge", "start")

	require.ErrorIs(t, err, stopFailure)
	assert.ErrorContains(t, err, "could not stop the running bridge")
	assert.ErrorContains(t, err, "stop it from an elevated shell", "the stop error's fix must ride the chain")
	assert.Equal(t, 0, *starts, "a failed stop must start nothing")
}

func TestGivenTheDaemonExitedBeforeTheStopWhenStartBridgeInvokedThenNoStoppedLineAndStarts(t *testing.T) {
	_, starts := fakeRestart(t, func(control.Request) control.Response {
		return control.Response{OK: true, Port: 5037}
	}, daemon.StopResult{PID: 4242})

	out, err := execRoot(t, "bridge", "start")

	require.NoError(t, err)
	assert.NotContains(t, out.String(), "a bridge was running")
	assert.Equal(t, 1, *starts)
}

func TestGivenNoBridgeWhenStartBridgeInvokedThenStopIsNeverCalled(t *testing.T) {
	paths := shortRuntimeDir(t)
	stops := fakeBridgeStop(t, daemon.StopResult{}, nil)
	fakeBridgeStart(t, paths, func(control.Request) control.Response {
		return control.Response{OK: true, Port: 5037}
	})

	_, err := execRoot(t, "bridge", "start")

	require.NoError(t, err)
	assert.Equal(t, 0, *stops, "with no running bridge there is nothing to stop")
}

func TestGivenRunningDaemonWhenAddDeviceInvokedThenNeitherStopNorStartIsCalled(t *testing.T) {
	fakeDaemon(t, func(control.Request) control.Response {
		return control.Response{OK: true, DeviceLabel: "Pixel", Port: 5037}
	})
	stops := fakeBridgeStop(t, daemon.StopResult{}, nil)
	starts := countingBridgeStart(t)

	_, err := execRoot(t, "device", "add", "--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0")

	require.NoError(t, err)
	assert.Equal(t, 0, *stops, "device add keeps the running bridge")
	assert.Equal(t, 0, *starts)
}

func TestGivenRunningDaemonWhenListDevicesInvokedThenNeitherStopNorStartIsCalled(t *testing.T) {
	fakeDaemon(t, func(control.Request) control.Response {
		return control.Response{OK: true}
	})
	stops := fakeBridgeStop(t, daemon.StopResult{}, nil)
	starts := countingBridgeStart(t)

	_, err := execRoot(t, "device", "list")

	require.NoError(t, err)
	assert.Equal(t, 0, *stops, "device list keeps the running bridge")
	assert.Equal(t, 0, *starts)
}

func TestGivenRunningDaemonWhenStartBridgeInvokedWithoutARNThenPrintsTheNewDaemonsStatus(t *testing.T) {
	fakeRestart(t, func(request control.Request) control.Response {
		assert.Equal(t, control.OpStatus, request.Op)
		return control.Response{OK: true, DeviceLabel: "Google Pixel 9 (OS 16)", Port: 6000}
	}, daemon.StopResult{PID: 4242, WasRunning: true})

	out, err := execRoot(t, "bridge", "start", "--port", "5038")

	require.NoError(t, err)
	assert.Contains(t, out.String(), "listening on port 6000 with connected device: Google Pixel 9 (OS 16)")
}

func TestGivenNoBridgeWhenStartBridgeInvokedThenStartsItAndPrintsTheListeningLine(t *testing.T) {
	paths := shortRuntimeDir(t)
	fakeBridgeStart(t, paths, func(req control.Request) control.Response {
		assert.Equal(t, control.OpStatus, req.Op)
		return control.Response{OK: true, Port: 5037}
	})

	out, err := execRoot(t, "bridge", "start")

	require.NoError(t, err)
	assert.Contains(t, out.String(), "* bridge not running; starting it now")
	assert.Contains(t, out.String(), "listening on port 5037 with no device attached")
}

func TestGivenBridgeStartFailsWhenStartBridgeInvokedWithARNThenErrorsWithTheStartFailureOnly(t *testing.T) {
	// The start error must not be dressed up as "bridge started but attaching failed".
	shortRuntimeDir(t)
	startFailure := errors.New("spawn bridge: exec format error")
	stubBridgeStart(t, func(*daemon.DaemonManager) (daemon.StartResult, error) { return daemon.StartResult{}, startFailure })

	_, err := execRoot(t, "bridge", "start", "--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0")

	require.ErrorIs(t, err, startFailure)
	assert.NotContains(t, err.Error(), "bridge started")
}

func TestGivenPidfileButNoControlSocketWhenStartBridgeInvokedThenReturnsNotListening(t *testing.T) {
	// The restart stops the stale record's daemon and starts anew; the status
	// call then finds no socket.
	fakeRuntimePaths(t)
	fakeBridgeStop(t, daemon.StopResult{PID: 4242, WasRunning: true}, nil)
	countingBridgeStart(t)

	out, err := execRoot(t, "bridge", "start")

	require.ErrorIs(t, err, errNotListening)
	assert.NotContains(t, out.String(), "listening on port")
}

func TestGivenPidfileButNoControlSocketWhenStartBridgeInvokedWithARNThenReturnsNotListeningOnly(t *testing.T) {
	// The restarted daemon never binds its socket; the add finds none. Not a refused attach.
	fakeRuntimePaths(t)
	fakeBridgeStop(t, daemon.StopResult{PID: 4242, WasRunning: true}, nil)
	countingBridgeStart(t)

	_, err := execRoot(t, "bridge", "start", "--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0")

	require.ErrorIs(t, err, errNotListening)
	assert.NotContains(t, err.Error(), "bridge started")
}

func TestGivenSessionARNWhenStartBridgeInvokedThenStartsAndAttachesDevice(t *testing.T) {
	fakeRestart(t, func(req control.Request) control.Response {
		assert.Equal(t, control.OpAdd, req.Op)
		return control.Response{OK: true, DeviceLabel: "Google Pixel 9 (OS 16)", Port: 5037}
	}, daemon.StopResult{PID: 4242, WasRunning: true})

	out, err := execRoot(t, "bridge", "start", "--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0")

	require.NoError(t, err)
	assert.Contains(t, out.String(), "listening on port 5037")
	assert.Contains(t, out.String(), "Google Pixel 9 (OS 16)")
}

func TestGivenSessionARNButDaemonCannotResolveWhenStartBridgeInvokedThenErrorsButReportsDaemonUp(t *testing.T) {
	// The daemon resolves the ARN, so a session that does not resolve fails after the
	// start. The error says the bridge started.
	fakeRestart(t, func(control.Request) control.Response {
		return control.Response{OK: false, Error: "session not RUNNING"}
	}, daemon.StopResult{PID: 4242, WasRunning: true})

	_, err := execRoot(t, "bridge", "start", "--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "bridge started")
	assert.Contains(t, err.Error(), "session not RUNNING")
}

func TestGivenNoBridgeWhenStartBridgeInvokedThenPrintsThePidAndLogFileAsAPlainLine(t *testing.T) {
	paths := shortRuntimeDir(t)
	fakeBridgeStart(t, paths, func(control.Request) control.Response {
		return control.Response{OK: true, Port: 5037}
	})
	root := newRootCommand()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs([]string{"bridge", "start"})

	require.NoError(t, root.Execute())

	assert.Contains(t, stderr.String(), fmt.Sprintf("* bridge started (PID %d); log file: %s\n", os.Getpid(), paths.LogFile))
	assert.NotContains(t, stdout.String(), "* bridge started", "result lines stay off stdout")
	assert.NotContains(t, stderr.String(), "level=", "a plain line, not a log record")
}

func TestGivenAnAdbServerWasStoppedWhenStartBridgeInvokedThenSaysSoBeforeTheStartedLine(t *testing.T) {
	paths := shortRuntimeDir(t)
	fakeBridgeStartWithResult(t, paths, func(control.Request) control.Response {
		return control.Response{OK: true, Port: 5037}
	}, daemon.StartResult{PID: 4242, LogFile: paths.LogFile, StoppedAdbServer: "127.0.0.1:5037"})

	out, err := execRoot(t, "bridge", "start")

	require.NoError(t, err)
	stopped := strings.Index(out.String(), "* stopped the adb server on 127.0.0.1:5037 (via 'adb kill-server')")
	started := strings.Index(out.String(), "* bridge started (PID 4242); log file: "+paths.LogFile)
	require.GreaterOrEqual(t, stopped, 0, out.String())
	require.GreaterOrEqual(t, started, 0, out.String())
	assert.Less(t, stopped, started, "the stop notice comes before the started line")
}

func TestGivenTheBridgeAlreadyRanWhenStartReturnsThenNoStartedLineIsPrinted(t *testing.T) {
	// Two starts can race: Start then reports the winner. No started line, nothing new to say.
	paths := shortRuntimeDir(t)
	fakeBridgeStartWithResult(t, paths, func(control.Request) control.Response {
		return control.Response{OK: true, Port: 5037}
	}, daemon.StartResult{PID: 4242, LogFile: paths.LogFile, AlreadyRunning: true})

	out, err := execRoot(t, "bridge", "start")

	require.NoError(t, err)
	assert.NotContains(t, out.String(), "* bridge started")
}

func TestGivenTheBridgeWasKilledWhenStopBridgeInvokedThenSaysSoBeforeStopped(t *testing.T) {
	fakeBridgeStop(t, daemon.StopResult{PID: 4242, WasRunning: true, Killed: true}, nil)

	out, err := runRoot(t, "bridge", "stop")

	require.NoError(t, err)
	killed := strings.Index(out.String(), "* the bridge (PID 4242) did not stop cleanly and was killed")
	stopped := strings.Index(out.String(), config.BinaryName+" stopped")
	require.GreaterOrEqual(t, killed, 0, out.String())
	require.GreaterOrEqual(t, stopped, 0, out.String())
	assert.Less(t, killed, stopped)
}

func TestGivenTheBridgeExitedMeanwhileWhenStopBridgeInvokedThenSaysNotListening(t *testing.T) {
	fakeBridgeStop(t, daemon.StopResult{PID: 4242}, nil)

	out, err := runRoot(t, "bridge", "stop")

	require.NoError(t, err)
	assert.Contains(t, out.String(), wording.OutputNotListening)
	assert.NotContains(t, out.String(), "stopped\n")
}

func TestGivenStopFailsWhenStopBridgeInvokedThenTheFailureNamesTheProblem(t *testing.T) {
	fakeBridgeStop(t, daemon.StopResult{PID: 4242, WasRunning: true}, errors.New("bridge (PID 4242) survived kill after 2s; PID file left in place"))

	_, err := runRoot(t, "bridge", "stop")

	require.Error(t, err)
	assert.ErrorContains(t, err, "could not stop the bridge")
	assert.ErrorContains(t, err, "survived kill")
}

func TestGivenAnAdbServerWasStoppedWhenTheStartThenFailsThenTheStopNoticeStillPrints(t *testing.T) {
	shortRuntimeDir(t)
	startFailure := errors.New("spawn bridge: exec format error")
	stubBridgeStart(t, func(*daemon.DaemonManager) (daemon.StartResult, error) {
		return daemon.StartResult{StoppedAdbServer: "127.0.0.1:5037"}, startFailure
	})

	out, err := execRoot(t, "bridge", "start")

	require.ErrorIs(t, err, startFailure)
	assert.Contains(t, out.String(), "* stopped the adb server on 127.0.0.1:5037 (via 'adb kill-server')")
	assert.NotContains(t, out.String(), "* bridge started")
}

func TestGivenAPaddedSessionArnWhenAddDeviceInvokedThenTheDaemonGetsTheTrimmedArn(t *testing.T) {
	fakeDaemon(t, func(req control.Request) control.Response {
		assert.Equal(t, "arn:aws:devicefarm:us-west-2:1:session:a/b/0", req.ARN, "the flag is stored trimmed")
		return control.Response{OK: true, DeviceLabel: "Pixel", Port: 5037}
	})

	_, err := execRoot(t, "device", "add", "--session-arn", "  arn:aws:devicefarm:us-west-2:1:session:a/b/0  ")

	require.NoError(t, err)
}

func TestGivenOnlySpacesAsSessionArnWhenAddDeviceInvokedThenTheFlagIsRequired(t *testing.T) {
	shortRuntimeDir(t)

	_, err := execRoot(t, "device", "add", "--session-arn", "   ")

	require.ErrorContains(t, err, "--session-arn is required")
}

func TestGivenTheDaemonAttachedUnderAnotherArnWhenAddDeviceInvokedThenPrintsThatArn(t *testing.T) {
	const returned = "arn:aws:devicefarm:us-west-2:1:session:x/y/9"
	fakeDaemon(t, func(control.Request) control.Response {
		return control.Response{OK: true, DeviceLabel: "Pixel", ARN: returned, Port: 5037}
	})

	out, err := execRoot(t, "device", "add", "--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0")

	require.NoError(t, err)
	assert.Contains(t, out.String(), "* the service names this session "+returned+"; use that ARN with 'device remove'")
}

func TestGivenTheDaemonAttachedUnderTheTypedArnWhenAddDeviceInvokedThenPrintsNoArnLine(t *testing.T) {
	const typed = "arn:aws:devicefarm:us-west-2:1:session:a/b/0"
	fakeDaemon(t, func(control.Request) control.Response {
		return control.Response{OK: true, DeviceLabel: "Pixel", ARN: typed, Port: 5037}
	})

	out, err := execRoot(t, "device", "add", "--session-arn", typed)

	require.NoError(t, err)
	assert.NotContains(t, out.String(), "names this session")
}

func TestGivenABlankSessionArnWhenRemoveDeviceInvokedThenRefusedAndNothingRemoved(t *testing.T) {
	fakeDaemon(t, func(req control.Request) control.Response {
		t.Errorf("no request may reach the daemon; got %q", req.Op)
		return control.Response{OK: true}
	})

	_, err := execRoot(t, "device", "remove", "--session-arn", "   ")

	require.ErrorContains(t, err, "--session-arn is blank")
}

func TestGivenABlankSessionArnWhenStartBridgeInvokedThenRefused(t *testing.T) {
	shortRuntimeDir(t)

	_, err := execRoot(t, "bridge", "start", "--session-arn", "   ")

	require.ErrorContains(t, err, "--session-arn is blank")
}
