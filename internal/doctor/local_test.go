package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/control"
	"github.com/aws/device-connect-devicefarm/internal/daemon"
)

// shortRuntimeDir points XDG_RUNTIME_DIR at a short temp dir and returns the paths.
func shortRuntimeDir(t *testing.T) daemon.Paths {
	t.Helper()
	// A short runtime dir under /tmp, not t.TempDir() or $TMPDIR: t.TempDir embeds
	// the long test name and the macOS $TMPDIR is long. DefaultPaths appends
	// devicefarm-connect/devicefarm-connect.sock, and the whole path must stay under
	// maxControlSocketPath.
	runtimeDir, err := os.MkdirTemp("/tmp", "adbdoc")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	paths, err := daemon.DefaultPaths()
	require.NoError(t, err)
	return paths
}

// fakeRuntimePaths is shortRuntimeDir plus a pidfile held by this test process, so Status
// reports running.
func fakeRuntimePaths(t *testing.T) daemon.Paths {
	t.Helper()
	paths := shortRuntimeDir(t)
	pidFile := daemon.NewPidFile(paths.PidFile, nil)
	require.NoError(t, pidFile.Write())
	t.Cleanup(func() { _ = pidFile.Remove() })
	return paths
}

// fakeDaemon answers every status request with reply on a real control socket at fakeRuntimePaths.
func fakeDaemon(t *testing.T, reply control.Response) daemon.Paths {
	t.Helper()
	paths := fakeRuntimePaths(t)
	handler := func(request control.Request) control.Response {
		assert.Equal(t, control.OpStatus, request.Op, "the doctor must only ask for status")
		return reply
	}
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
	return paths
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// localInputs is the default config on a free port with a quiet logger.
func localInputs(t *testing.T) Inputs {
	t.Helper()
	cfg := config.Default()
	cfg.ListenPort = freePort(t)
	return Inputs{Config: cfg, Logger: slog.New(slog.DiscardHandler)}
}

// runLocal runs the one local check named by id over state.
func runLocal(t *testing.T, inputs Inputs, state *runState, id string) Result {
	t.Helper()
	for _, check := range localChecks(inputs, state) {
		if check.ID == id {
			return check.Run(context.Background())
		}
	}
	t.Fatalf("no local check %q", id)
	return Result{}
}

func TestGivenAShortRuntimeDirWhenCheckedThenItPassesNamingTheDirAndLeavesNoProbeFile(t *testing.T) {
	paths := shortRuntimeDir(t)
	state := &runState{}

	result := runLocal(t, localInputs(t), state, checkRuntimeDir)

	assert.Equal(t, Pass, result.Status)
	assert.Equal(t, "working directory "+paths.RuntimeDir+" is writable", result.Summary)
	assert.Equal(t, paths, state.paths)
	leftovers, err := filepath.Glob(filepath.Join(paths.RuntimeDir, ".doctor-*"))
	require.NoError(t, err)
	assert.Empty(t, leftovers, "the probe file must be removed")
}

func TestGivenAnUnusableRuntimeDirWhenCheckedThenItFailsNamingTheEnvVar(t *testing.T) {
	tests := []struct {
		name        string
		runtimeBase func(t *testing.T) string
		wantSummary string
	}{
		{name: "a regular file where the dir should be", runtimeBase: func(t *testing.T) string {
			base := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(base, config.BinaryName), []byte("not a dir"), 0o600))
			return base
		}, wantSummary: "cannot use the working directory: create runtime dir"},
		{name: "a socket path over the limit, with a newline in the path", runtimeBase: func(t *testing.T) string {
			return filepath.Join(t.TempDir(), strings.Repeat("x", maxControlSocketPath)+"\n[PASS] forged")
		}, wantSummary: `\n[PASS] forged/` + config.BinaryName + " is "},
		{name: "a read-only runtime dir", runtimeBase: func(t *testing.T) string {
			if os.Geteuid() == 0 {
				t.Skip("root writes into a read-only dir")
			}
			// Under /tmp, as shortRuntimeDir: the limit row must not fire first. The newline
			// reaches the OS error's path, so the row must escape the whole summary.
			base, err := os.MkdirTemp("/tmp", "adbdoc\n[PASS] forged")
			require.NoError(t, err)
			t.Cleanup(func() { _ = os.RemoveAll(base) })
			require.NoError(t, os.Mkdir(filepath.Join(base, config.BinaryName), 0o500))
			return base
		}, wantSummary: "cannot write a file in the working directory"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_RUNTIME_DIR", tc.runtimeBase(t))
			state := &runState{}

			result := runLocal(t, localInputs(t), state, checkRuntimeDir)

			assert.Equal(t, Fail, result.Status)
			assert.Contains(t, result.Summary, tc.wantSummary)
			assert.NotContains(t, result.Summary, "\n", "a row is one line")
			assert.Equal(t, runtimeDirHint, result.Hint)
			assert.Empty(t, state.paths.RuntimeDir, "a failed row leaves no paths for later checks")
		})
	}
}

func TestGivenNoDaemonWhenCheckedThenItPassesAsNotRunning(t *testing.T) {
	state := &runState{paths: shortRuntimeDir(t)}

	result := runLocal(t, localInputs(t), state, checkDaemon)

	assert.Equal(t, Result{Status: Pass, Summary: "not running; bridge start, device add or device list will launch it"}, result)
}

func TestGivenARunningDaemonWhenCheckedThenTheRowReportsItsStatusAndReverseState(t *testing.T) {
	tests := []struct {
		name       string
		reply      control.Response
		wantStatus Status
		wantTail   string
		// wantRestart is the command the WARN hint quotes.
		wantRestart string
	}{
		{name: "reverse on with a device",
			reply:      control.Response{OK: true, Port: 5037, DeviceLabel: "Pixel 9 (OS 16)", ARN: "arn:1", ReverseEnabled: true},
			wantStatus: Pass, wantTail: "on port 5037; device Pixel 9 (OS 16), session arn:1; reverse on"},
		{name: "reverse on with no device", reply: control.Response{OK: true, Port: 6000, ReverseEnabled: true},
			wantStatus: Pass, wantTail: "on port 6000; no device; reverse on"},
		{name: "reverse off with a device", reply: control.Response{OK: true, Port: 5037, DeviceLabel: "Pixel 9 (OS 16)", ARN: "arn:2"},
			wantStatus: Warn, wantTail: "on port 5037; device Pixel 9 (OS 16), session arn:2; reverse off",
			wantRestart: restartDaemonHint + " --port 5037 --allow-adb-reverse --session-arn arn:2"},
		{name: "reverse off with no device", reply: control.Response{OK: true, Port: 6000},
			wantStatus: Warn, wantTail: "on port 6000; no device; reverse off", wantRestart: restartDaemonHint + " --port 6000 --allow-adb-reverse"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := &runState{paths: fakeDaemon(t, tc.reply)}

			result := runLocal(t, localInputs(t), state, checkDaemon)

			assert.Equal(t, tc.wantStatus, result.Status)
			assert.Equal(t, fmt.Sprintf("running (pid %d) %s", os.Getpid(), tc.wantTail), result.Summary)
			if tc.wantStatus == Warn {
				assert.Contains(t, result.Hint, "'"+tc.wantRestart+"'", "the hint quotes the restart with the reported port and session")
				assert.Contains(t, result.Hint, reverseCaveat)
			} else {
				assert.Empty(t, result.Hint)
			}
			reply, pid := state.daemon()
			assert.Equal(t, tc.reply, reply)
			assert.Equal(t, os.Getpid(), pid)
		})
	}
}

func TestGivenAControlErrorWhenTheSocketPathIsDroppedThenTheCauseStays(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), config.BinaryName+".sock")
	timeout := fmt.Errorf("read control response: %w", &net.OpError{Op: "read", Net: "unix",
		Addr: &net.UnixAddr{Name: socketPath, Net: "unix"}, Err: os.ErrDeadlineExceeded})
	require.Contains(t, timeout.Error(), socketPath, "the raw error names the socket")

	cause := causeWithoutSocketPath(timeout)
	assert.Contains(t, cause, "i/o timeout")
	assert.NotContains(t, cause, socketPath)
	assert.Equal(t, "read control response: EOF", causeWithoutSocketPath(fmt.Errorf("read control response: %w", io.EOF)),
		"an error with no socket address reads verbatim")
}

func TestGivenALockedPidfileWhoseDaemonDoesNotAnswerOrRefusesStatusWhenCheckedThenItFails(t *testing.T) {
	tests := []struct {
		name        string
		paths       func(t *testing.T) daemon.Paths
		wantSummary string
	}{
		{name: "no control socket", paths: fakeRuntimePaths,
			wantSummary: fmt.Sprintf("a daemon (pid %d) is registered but does not answer: connect: ", os.Getpid())},
		{name: "status refused", paths: func(t *testing.T) daemon.Paths {
			return fakeDaemon(t, control.Response{OK: false, Error: "boom"})
		}, wantSummary: "is registered but refused to report its status: boom"},
		{name: "socket closes without a reply", paths: func(t *testing.T) daemon.Paths {
			paths := fakeRuntimePaths(t)
			listener, err := net.Listen("unix", paths.ControlSocket)
			require.NoError(t, err)
			t.Cleanup(func() { listener.Close() })
			go func() {
				for {
					conn, acceptErr := listener.Accept()
					if acceptErr != nil {
						return
					}
					// Read the request first, so only the client's read fails.
					_ = json.NewDecoder(conn).Decode(&control.Request{})
					conn.Close()
				}
			}()
			return paths
		}, wantSummary: "is registered but did not report its status: read control response"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := &runState{paths: tc.paths(t)}

			result := runLocal(t, localInputs(t), state, checkDaemon)

			assert.Equal(t, Fail, result.Status)
			assert.Contains(t, result.Summary, tc.wantSummary)
			assert.NotContains(t, result.Summary, state.paths.ControlSocket, "the row names no path")
			assert.Equal(t, deadDaemonHint, result.Hint)
			assert.Contains(t, result.Hint, "'"+restartDaemonHint+"'", "the hint quotes the restart")
			reply, _ := state.daemon()
			assert.False(t, reply.OK, "a failed row leaves no status for later checks")
		})
	}
}

func TestGivenAFreshMachineWhenTheTableRunsThenEveryLocalRowPasses(t *testing.T) {
	shortRuntimeDir(t)
	inputs := localInputs(t)
	inputs.Logger = nil

	report := Run(context.Background(), Checks(inputs))

	ids := make([]string, 0, len(report.Rows))
	for _, row := range report.Rows {
		ids = append(ids, row.ID)
		assert.Equal(t, Pass, row.Status, "%s: %s", row.ID, row.Summary)
	}
	assert.Equal(t, []string{checkRuntimeDir, checkDaemon}, ids)
}
