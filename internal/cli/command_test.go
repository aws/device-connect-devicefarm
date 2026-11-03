package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/control"
	"github.com/aws/device-connect-devicefarm/internal/daemon"
)

// TestMain blocks the real bridge start: a test that needs one uses fakeBridgeStart;
// any other test that reaches it has a bug.
func TestMain(m *testing.M) {
	startBridge = func(*daemon.DaemonManager) error {
		panic("a test reached the real bridge start; use fakeBridgeStart")
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
// end to end over a Unix socket without forking the daemon.
func serveControl(t *testing.T, paths daemon.Paths, handler control.Handler) {
	t.Helper()
	server := control.NewLocalSocketServer(paths.ControlSocket, slog.New(slog.DiscardHandler))
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
func stubBridgeStart(t *testing.T, start func(*daemon.DaemonManager) error) {
	t.Helper()
	previous := startBridge
	startBridge = start
	t.Cleanup(func() { startBridge = previous })
}

// fakeBridgeStart makes a bridge start write the live pidfile and serve handler at paths
// instead of forking.
func fakeBridgeStart(t *testing.T, paths daemon.Paths, handler control.Handler) {
	t.Helper()
	stubBridgeStart(t, func(*daemon.DaemonManager) error {
		writeLivePidfile(t, paths)
		serveControl(t, paths, handler)
		return nil
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
		return control.Response{OK: false, Error: "a different device is already attached (Other); remove it first"}
	})

	_, err := execRoot(t, "device", "add", "--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "already attached")
}

func TestGivenDaemonCannotResolveWhenAddDeviceInvokedThenSurfacesResolveError(t *testing.T) {
	// Resolution now happens in the daemon, so a bad session surfaces as the add
	// response error rather than a pre-spawn CLI failure.
	fakeDaemon(t, func(control.Request) control.Response {
		return control.Response{OK: false, Error: "resolve session: session not RUNNING"}
	})

	_, err := execRoot(t, "device", "add", "--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "session not RUNNING")
}

func TestGivenNoControlSocketWhenAddDeviceInvokedThenReturnsNotListening(t *testing.T) {
	// The live pidfile says a daemon runs, so add starts none; the socket is unbound.
	fakeRuntimePaths(t)

	_, err := execRoot(t, "device", "add", "--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0")

	require.EqualError(t, err, notListeningMessage)
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
	startFailure := errors.New(`daemon failed to start (check "/tmp/d.log")`)
	stubBridgeStart(t, func(*daemon.DaemonManager) error { return startFailure })

	out, err := execRoot(t, "device", "add", "--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0")

	require.ErrorIs(t, err, startFailure)
	assert.ErrorContains(t, err, "start the bridge")
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

	require.EqualError(t, err, notListeningMessage)
	assert.NotContains(t, out.String(), "starting it now")
}

func TestGivenNoControlSocketWhenRemoveDeviceInvokedThenReturnsNotListening(t *testing.T) {
	// A live pidfile with no socket bound. Remove never starts a daemon, so the dead
	// socket is what it reports.
	fakeRuntimePaths(t)

	_, err := execRoot(t, "device", "remove", "--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0")

	require.EqualError(t, err, notListeningMessage)
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
}

func TestGivenRunningDaemonWithDeviceWhenListDevicesInvokedThenPrintsOneLinePerDevice(t *testing.T) {
	fakeDaemon(t, func(req control.Request) control.Response {
		assert.Equal(t, control.OpList, req.Op)
		return control.Response{OK: true, Devices: []control.AttachedDevice{
			{SessionARN: "arn:aws:devicefarm:us-west-2:1:session:a/b/0", Label: "Google Pixel 9 (OS 16)"},
		}}
	})

	out, err := execRoot(t, "device", "list")

	require.NoError(t, err)
	assert.Equal(t, "arn:aws:devicefarm:us-west-2:1:session:a/b/0\tGoogle Pixel 9 (OS 16)\n", out.String())
}

func TestGivenRunningDaemonWithoutDeviceWhenListDevicesInvokedThenSaysNoDevice(t *testing.T) {
	fakeDaemon(t, func(control.Request) control.Response {
		return control.Response{OK: true}
	})

	out, err := execRoot(t, "device", "list")

	require.NoError(t, err)
	assert.Equal(t, "no device attached; use 'device add' to connect one\n", out.String())
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

func TestGivenRunningDaemonWithDeviceWhenStatusInvokedThenReportsAttachedDevice(t *testing.T) {
	fakeDaemon(t, func(req control.Request) control.Response {
		assert.Equal(t, control.OpStatus, req.Op)
		return control.Response{OK: true, DeviceLabel: "Google Pixel 8 (OS 15)"}
	})

	out, err := execRoot(t, "bridge", "status")

	require.NoError(t, err)
	assert.Contains(t, out.String(), "device attached: Google Pixel 8 (OS 15)")
}

func TestGivenRunningDaemonWithoutDeviceWhenStatusInvokedThenReportsNoDevice(t *testing.T) {
	fakeDaemon(t, func(control.Request) control.Response {
		return control.Response{OK: true}
	})

	out, err := execRoot(t, "bridge", "status")

	require.NoError(t, err)
	// Literal on purpose: a changed BinaryName must fail here.
	assert.Contains(t, out.String(), "devicefarm-connect daemon is running (pid ")
	assert.Contains(t, out.String(), "no device attached")
}

func TestGivenPidfileButNoControlSocketWhenStatusInvokedThenReportsNotRunning(t *testing.T) {
	// Pidfile says running, no socket bound: status reports not running, no error.
	fakeRuntimePaths(t)

	out, err := execRoot(t, "bridge", "status")

	require.NoError(t, err)
	assert.Equal(t, config.BinaryName+" daemon is not running\n", out.String())
}

func TestGivenNoSessionARNWhenStartBridgeInvokedThenStartsWithoutAttaching(t *testing.T) {
	// fakeDaemon writes a live pidfile, so Manager.Start short-circuits to a no-op
	// (a daemon is "already running") rather than forking a real child. This lets
	// the no-ARN bridge start path be exercised at the unit level. The daemon's
	// control handler must never see an add when no ARN is given.
	fakeDaemon(t, func(req control.Request) control.Response {
		if req.Op == control.OpAdd {
			t.Error("bridge start without --session-arn must not attach a device")
		}
		return control.Response{OK: true, Port: 6000}
	})

	out, err := execRoot(t, "bridge", "start", "--port", "5038")

	require.NoError(t, err, "bridge start must not require --session-arn")
	assert.Contains(t, out.String(), "listening on port 6000 with no device attached")
	assert.NotContains(t, out.String(), "5038", "the port must come from the daemon, not the --port flag")
}

func TestGivenRunningDaemonWithDeviceWhenStartBridgeInvokedWithoutARNThenPrintsAttachedDevice(t *testing.T) {
	fakeDaemon(t, func(request control.Request) control.Response {
		assert.Equal(t, control.OpStatus, request.Op)
		return control.Response{OK: true, DeviceLabel: "Google Pixel 9 (OS 16)", Port: 6000}
	})

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
	// The start error must not be dressed up as "daemon started but attaching failed".
	shortRuntimeDir(t)
	startFailure := errors.New(`daemon failed to start (check "/tmp/d.log")`)
	stubBridgeStart(t, func(*daemon.DaemonManager) error { return startFailure })

	_, err := execRoot(t, "bridge", "start", "--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0")

	require.ErrorIs(t, err, startFailure)
	assert.NotContains(t, err.Error(), "daemon started")
}

func TestGivenPidfileButNoControlSocketWhenStartBridgeInvokedThenReturnsNotListening(t *testing.T) {
	// Start no-ops on the live pidfile; the status call finds no socket.
	fakeRuntimePaths(t)

	out, err := execRoot(t, "bridge", "start")

	require.EqualError(t, err, notListeningMessage)
	assert.NotContains(t, out.String(), "listening on port")
}

func TestGivenSessionARNWhenStartBridgeInvokedThenStartsAndAttachesDevice(t *testing.T) {
	fakeDaemon(t, func(req control.Request) control.Response {
		assert.Equal(t, control.OpAdd, req.Op)
		return control.Response{OK: true, DeviceLabel: "Google Pixel 9 (OS 16)", Port: 5037}
	})

	out, err := execRoot(t, "bridge", "start", "--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0")

	require.NoError(t, err)
	assert.Contains(t, out.String(), "listening on port 5037")
	assert.Contains(t, out.String(), "Google Pixel 9 (OS 16)")
}

func TestGivenSessionARNButDaemonCannotResolveWhenStartBridgeInvokedThenErrorsButReportsDaemonUp(t *testing.T) {
	// With resolution in the daemon, a bad ARN no longer fails before the spawn:
	// the daemon starts and the attach fails. The error must make clear the daemon
	// is up so the user retries device add rather than assuming nothing started.
	fakeDaemon(t, func(control.Request) control.Response {
		return control.Response{OK: false, Error: "resolve session: session not RUNNING"}
	})

	_, err := execRoot(t, "bridge", "start", "--session-arn", "arn:aws:devicefarm:us-west-2:1:session:a/b/0")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "daemon started")
	assert.Contains(t, err.Error(), "retry 'device add'")
	assert.Contains(t, err.Error(), "session not RUNNING")
}
