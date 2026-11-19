package control

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noopLogger discards all log output so tests emit no noise.
func noopLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// shortConnDeadline shrinks the server's per-connection deadline for the duration
// of a test so the hung-client path resolves quickly, restoring it on cleanup.
func shortConnDeadline(t *testing.T, d time.Duration) {
	t.Helper()
	original := connDeadline
	connDeadline = d
	t.Cleanup(func() { connDeadline = original })
}

// startServer binds a control server on a temp socket and serves it with the
// given handler until the test ends. It returns the socket path.
func startServer(t *testing.T, handler Handler) string {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "control.sock")
	server := NewLocalSocketServer(socketPath, noopLogger())
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
		assert.NoError(t, server.Close(), "Close after Serve must be a no-op")
	})
	return socketPath
}

// leaveStaleSocketFile binds then closes a listener without unlinking, as an
// unclean daemon exit does.
func leaveStaleSocketFile(t *testing.T, socketPath string) {
	t.Helper()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	require.NoError(t, err)
	listener.SetUnlinkOnClose(false)
	require.NoError(t, listener.Close())
	info, err := os.Lstat(socketPath)
	require.NoError(t, err, "the stale socket file must exist before the test proceeds")
	require.NotZero(t, info.Mode()&os.ModeSocket, "the leftover must be a socket file")
}

func TestGivenServedRequestWhenClientSendsThenHandlerResponseReturned(t *testing.T) {
	socketPath := startServer(t, func(req Request) Response {
		return Response{OK: true, DeviceLabel: "Pixel - OS 16", Port: 5037}
	})

	resp, err := NewDaemonClient(socketPath).Send(Request{Op: OpAdd, ARN: "arn:x"})

	require.NoError(t, err)
	assert.True(t, resp.OK)
	assert.Equal(t, "Pixel - OS 16", resp.DeviceLabel)
	assert.Equal(t, 5037, resp.Port)
}

func TestGivenRequestFieldsWhenClientSendsThenHandlerSeesThem(t *testing.T) {
	var got Request
	socketPath := startServer(t, func(req Request) Response {
		got = req
		return Response{OK: true}
	})

	_, err := NewDaemonClient(socketPath).Send(Request{Op: OpRemove, ARN: "arn:device:1"})

	require.NoError(t, err)
	assert.Equal(t, OpRemove, got.Op)
	assert.Equal(t, "arn:device:1", got.ARN)
}

func TestGivenClientConnectsButNeverWritesWhenServedThenDeadlineFreesLoopForNextCommand(t *testing.T) {
	shortConnDeadline(t, 150*time.Millisecond)
	served := make(chan struct{}, 1)
	socketPath := startServer(t, func(req Request) Response {
		served <- struct{}{}
		return Response{OK: true}
	})

	// A raw connection that never writes a request. Without the per-connection
	// deadline this would block the inline accept loop forever.
	hung, err := net.Dial("unix", socketPath)
	require.NoError(t, err)
	defer hung.Close()

	// A normal command must still be served once the hung connection's deadline
	// fires and frees the loop.
	resp, err := NewDaemonClient(socketPath).Send(Request{Op: OpStatus})
	require.NoError(t, err)
	assert.True(t, resp.OK)
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("a hung client wedged the control loop; the deadline did not free it")
	}
}

func TestGivenMalformedRequestWhenServedThenErrorResponseAndLoopSurvives(t *testing.T) {
	socketPath := startServer(t, func(req Request) Response {
		return Response{OK: true}
	})

	// Write a non-JSON byte stream, then read the reply: the server must answer
	// with an error response rather than dropping the connection silently.
	bad, err := net.Dial("unix", socketPath)
	require.NoError(t, err)
	defer bad.Close()
	_, err = bad.Write([]byte("this is not json\n"))
	require.NoError(t, err)
	var resp Response
	require.NoError(t, json.NewDecoder(bad).Decode(&resp))
	assert.False(t, resp.OK)
	assert.NotEmpty(t, resp.Error)

	// And the loop must still serve a well-formed follow-up command.
	ok, err := NewDaemonClient(socketPath).Send(Request{Op: OpStatus})
	require.NoError(t, err)
	assert.True(t, ok.OK)
}

func TestGivenBoundControlSocketWhenInspectedThenOnlyTheOwnerCanReachIt(t *testing.T) {
	// net.Listen creates the socket file under the umask, and connect(2) needs
	// write permission on it; Listen must force owner-only.
	socketPath := startServer(t, func(Request) Response { return Response{OK: true} })

	info, err := os.Stat(socketPath)

	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSocket, "the path must be the bound socket")
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestGivenSocketModeCannotBeSetWhenListenedThenErrorsAndLeavesNoSocket(t *testing.T) {
	// A socket that cannot be made owner-only must not be served: Listen fails
	// and the bound file is unlinked again.
	original := chmod
	t.Cleanup(func() { chmod = original })
	chmod = func(string, os.FileMode) error { return assert.AnError }
	socketPath := filepath.Join(t.TempDir(), "control.sock")
	server := NewLocalSocketServer(socketPath, noopLogger())

	err := server.Listen()

	require.ErrorIs(t, err, assert.AnError)
	_, statErr := os.Lstat(socketPath)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "a socket the daemon will not serve must not linger")
}

func TestGivenSocketPathInMissingDirWhenListenedThenErrors(t *testing.T) {
	// A socket path under a directory that does not exist cannot be bound; Listen
	// must surface that rather than panic.
	server := NewLocalSocketServer(filepath.Join(t.TempDir(), "no-such-dir", "control.sock"), noopLogger())

	require.Error(t, server.Listen())
}

func TestGivenNoListeningDaemonWhenClientSendsThenReturnsNotRunning(t *testing.T) {
	// A path with no server bound: the dial must classify as "not running".
	socketPath := filepath.Join(t.TempDir(), "absent.sock")

	_, err := NewDaemonClient(socketPath).Send(Request{Op: OpStatus})

	assert.ErrorIs(t, err, ErrDaemonNotRunning)
}

func TestGivenStaleSocketFileWhenClientSendsThenReturnsNotRunning(t *testing.T) {
	// Stale file, no acceptor. The refused connect must classify as "not running".
	socketPath := filepath.Join(t.TempDir(), "stale.sock")
	leaveStaleSocketFile(t, socketPath)

	_, err := NewDaemonClient(socketPath).Send(Request{Op: OpStatus})

	assert.ErrorIs(t, err, ErrDaemonNotRunning)
}

func TestGivenDialFailureWhenClientSendsThenNotRunningErrorCarriesCause(t *testing.T) {
	// The error must keep the dial error in its chain and name the path and
	// the cause. A raw dial supplies the expected cause text.
	socketPath := filepath.Join(t.TempDir(), "absent.sock")
	_, dialErr := net.DialTimeout("unix", socketPath, time.Second)
	require.Error(t, dialErr)

	_, err := NewDaemonClient(socketPath).Send(Request{Op: OpStatus})

	require.ErrorIs(t, err, ErrDaemonNotRunning)
	var dialError *net.OpError
	assert.ErrorAs(t, err, &dialError)
	assert.ErrorContains(t, err, socketPath)
	assert.ErrorContains(t, err, dialErr.Error())
}

func TestGivenBoundServerWhenRelistenedAfterStaleFileThenSucceeds(t *testing.T) {
	// Listen must clear a stale socket file so a restart binds cleanly.
	socketPath := filepath.Join(t.TempDir(), "control.sock")
	leaveStaleSocketFile(t, socketPath)

	server := NewLocalSocketServer(socketPath, noopLogger())
	require.NoError(t, server.Listen(), "Listen should clear a stale socket and bind")
	t.Cleanup(func() { assert.NoError(t, server.Close()) })
}

func TestGivenListeningServerWhenClosedThenSocketFileRemoved(t *testing.T) {
	// Close must unlink the socket file so a restart binds cleanly.
	socketPath := filepath.Join(t.TempDir(), "control.sock")
	server := NewLocalSocketServer(socketPath, noopLogger())
	require.NoError(t, server.Listen())

	require.NoError(t, server.Close())

	_, statErr := os.Lstat(socketPath)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestGivenServerNeverListenedWhenClosedThenReturnsNil(t *testing.T) {
	// Close before Listen has nothing to release.
	server := NewLocalSocketServer(filepath.Join(t.TempDir(), "control.sock"), noopLogger())

	require.NoError(t, server.Close())
}
