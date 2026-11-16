// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/control"
	"github.com/aws/device-connect-devicefarm/internal/session"
	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// testTimeout bounds every blocking wait so a hung test fails instead of stalling the run.
const testTimeout = 5 * time.Second

// testPollInterval is the Eventually tick.
const testPollInterval = 10 * time.Millisecond

// newTestManager builds a DaemonManager backed by a temp runtime dir so tests
// never touch the real per-user runtime location. Its remote-access client is a
// default fake; lifecycle tests (Start/Stop/Status) never resolve, so its return
// is irrelevant to them. It uses a free port, not 5037: Start stops an adb server
// on its port, and a developer's machine may run a real one.
func newTestManager(t *testing.T) *DaemonManager {
	t.Helper()
	cfg := config.Default()
	cfg.ListenPort = freePort(t)
	return newTestManagerWithConfig(t, cfg)
}

// newTestManagerWithConfig is newTestManager with a caller-supplied config, used
// by the Stop tests to shrink the stop timeout so the kill escalation path runs
// quickly.
func newTestManagerWithConfig(t *testing.T, cfg config.ProxyConfig) *DaemonManager {
	t.Helper()
	return newTestManagerWithClient(t, cfg, newFakeClient(testEndpoint, "unused"))
}

// newTestManagerWithClient is newTestManagerWithConfig with a caller-supplied
// remote-access client, so add/refresh tests inject a fake whose endpoint, label,
// and error they control.
func newTestManagerWithClient(t *testing.T, cfg config.ProxyConfig, client session.RemoteAccessClient) *DaemonManager {
	t.Helper()
	return newTestManagerLogging(t, cfg, client, slog.DiscardHandler)
}

// newTestManagerLogging is newTestManagerWithClient writing the daemon log through loggerHandler.
func newTestManagerLogging(t *testing.T, cfg config.ProxyConfig, client session.RemoteAccessClient,
	loggerHandler slog.Handler) *DaemonManager {
	t.Helper()
	dir := t.TempDir()
	paths := Paths{
		RuntimeDir:    dir,
		PidFile:       filepath.Join(dir, "devicefarm-connect.pid"),
		LogFile:       filepath.Join(dir, "devicefarm-connect.log"),
		ControlSocket: filepath.Join(dir, "devicefarm-connect.sock"),
	}
	mgr := NewManager(cfg, paths, client, slog.New(loggerHandler))
	mgr.websocketRoots = testsupport.TestRoots()
	return mgr
}

// startRealChild starts a real, long-lived child process running the given shell
// script and returns its pid. The child is reaped by a background goroutine so
// that once Stop signals it, kill(pid, 0) returns ESRCH rather than observing a
// zombie (which a kill-0 probe reports as alive). This mirrors production, where
// the daemon is reparented to init and reaped there, and `devicefarm-connect bridge stop`
// signals a process that is not its own child. A cleanup force-kills the child so
// a failing test never leaks it.
func startRealChild(t *testing.T, script string) int {
	t.Helper()
	return startAndReap(t, exec.Command("sh", "-c", script))
}

// Environment for the child started by startDaemonLikeChild. Unset in a normal
// test run, where TestHelperProcess is a no-op.
const (
	helperPidFileEnv       = "DEVICEFARM_CONNECT_TEST_HELPER_PIDFILE"
	helperControlSocketEnv = "DEVICEFARM_CONNECT_TEST_HELPER_SOCKET"
	helperModeEnv          = "DEVICEFARM_CONNECT_TEST_HELPER_MODE"
)

// Helper modes: how the child answers a shutdown request.
const (
	helperModeExit    = "exit"    // exits, as the daemon does
	helperModeIgnore  = "ignore"  // replies OK, stays alive, keeps the lock
	helperModeRelease = "release" // drops its record and stays alive
	helperModeRefuse  = "refuse"  // replies with an error and stays alive
)

// TestHelperProcess is the daemon-like child: this test binary re-executed to
// write and lock the pidfile named in the environment, so the lock drops when
// the child dies, and to serve the control socket named there too. It prints
// "ready" once both are up, then parks until killed or, in exit mode, until
// shutdown arrives.
func TestHelperProcess(t *testing.T) {
	path := os.Getenv(helperPidFileEnv)
	if path == "" {
		return
	}
	pidFile := NewPidFile(path, nil)
	if err := pidFile.Write(); err != nil {
		t.Fatal(err)
	}
	// Keeps pidFile reachable through the park, so the GC cannot close the lock.
	defer pidFile.Remove()
	controlServer := control.NewLocalSocketServer(os.Getenv(helperControlSocketEnv), nil)
	if err := controlServer.Listen(); err != nil {
		t.Fatal(err)
	}
	defer controlServer.Close()
	// As in the daemon, the handler cancels this context before the reply is
	// written, so an exiting child may leave with the reply unsent.
	ctx, exit := context.WithCancel(context.Background())
	defer exit()
	mode := os.Getenv(helperModeEnv)
	go func() {
		_ = controlServer.Serve(ctx, func(req control.Request) control.Response {
			if req.Op != control.OpShutdown {
				return control.Response{OK: false, Error: "the helper answers shutdown only"}
			}
			switch mode {
			case helperModeExit:
				exit()
			case helperModeRelease:
				_ = pidFile.Remove()
			case helperModeRefuse:
				return control.Response{OK: false, Error: "refused for the test"}
			}
			return control.Response{OK: true}
		})
	}()
	os.Stdout.WriteString("ready\n")
	<-ctx.Done()
}

// startDaemonLikeChild runs TestHelperProcess as a child that holds the lock on
// mgr's pidfile and serves mgr's control socket in the given mode. It returns
// the pid once the child reports ready, so the lock is held and the socket
// bound before Stop runs. It stands in for a detached daemon in the Stop tests.
func startDaemonLikeChild(t *testing.T, mgr *DaemonManager, mode string) int {
	t.Helper()
	self, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.Command(self, "-test.run", "^TestHelperProcess$")
	cmd.Env = append(os.Environ(),
		helperPidFileEnv+"="+mgr.paths.PidFile,
		helperControlSocketEnv+"="+mgr.paths.ControlSocket,
		helperModeEnv+"="+mode,
	)
	// The child's failures and race reports must reach the test log.
	cmd.Stderr = os.Stderr
	readyRead, readyWrite, err := os.Pipe()
	require.NoError(t, err)
	// Open until cleanup: the harness output of an exiting child lands in the
	// pipe instead of killing it with SIGPIPE.
	t.Cleanup(func() { readyRead.Close() })
	cmd.Stdout = readyWrite
	pid := startAndReap(t, cmd)
	// Close the parent's write end so a child that dies before "ready" gives EOF,
	// not a hang.
	require.NoError(t, readyWrite.Close())
	require.NoError(t, readyRead.SetReadDeadline(time.Now().Add(testTimeout)))
	line, err := bufio.NewReader(readyRead).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "ready\n", line)
	return pid
}

// startAndReap starts cmd, reaps it in the background and force-kills it at
// cleanup; see startRealChild.
func startAndReap(t *testing.T, cmd *exec.Cmd) int {
	t.Helper()
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid

	reaped := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill() // no-op if already dead
		<-reaped
	})
	return pid
}

// freePort returns a currently-free local TCP port by binding to :0 and
// releasing it. A daemon needs a fixed, knowable port (config.Validate rejects
// port 0), so tests that run the real RunForeground path bind a real port.
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	return port
}

// startForeground runs RunForeground on a cancellable context and returns once the
// daemon wrote its pidfile, so it is serving. A startup failure ends the poll at
// once instead of burning its budget, which is generous for the -race slowdown.
// Eventually runs the condition on its own goroutine, so the failure is reported
// from the test goroutine afterwards; FailNow is not supported off it.
func startForeground(t *testing.T, mgr *DaemonManager) (done <-chan error, cancel context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	result := make(chan error, 1)
	go func() { result <- mgr.RunForeground(ctx) }()
	require.Eventually(t, func() bool {
		select {
		case err := <-result:
			result <- err
			return true
		default:
		}
		_, running := mgr.pidFile.RunningPID()
		return running
	}, 10*time.Second, 10*time.Millisecond, "daemon should write its pidfile while serving")
	select {
	case err := <-result:
		t.Fatalf("RunForeground exited before serving: %v", err)
	default:
	}
	return result, cancel
}

// stubSpawn replaces the package spawnDaemon var for the duration of a test,
// restoring it on cleanup. The provided function stands in for the detached
// child.
func stubSpawn(t *testing.T, fn func(self string, args []string, logOut *os.File) (*os.Process, <-chan struct{}, error)) {
	t.Helper()
	original := spawnDaemon
	spawnDaemon = fn
	t.Cleanup(func() { spawnDaemon = original })
}

// stubFindProcess swaps findProcess for the test and restores it on cleanup.
func stubFindProcess(t *testing.T, fn func(pid int) (*os.Process, error)) {
	t.Helper()
	original := findProcess
	findProcess = fn
	t.Cleanup(func() { findProcess = original })
}

// killProcess kills pid and waits until it is gone.
func killProcess(t *testing.T, pid int) {
	t.Helper()
	target, err := os.FindProcess(pid)
	require.NoError(t, err)
	require.NoError(t, target.Kill())
	require.Eventually(t, func() bool { return !processAlive(pid) }, testTimeout, testPollInterval)
}

func TestGivenNoDaemonRunningWhenStartedThenSpawnsChildAndReportsRunning(t *testing.T) {
	mgr := newTestManager(t)
	// Stand in for the child: write and lock the pidfile from this process, as
	// the daemon does on boot.
	stubSpawn(t, func(_ string, _ []string, _ *os.File) (*os.Process, <-chan struct{}, error) {
		require.NoError(t, mgr.pidFile.Write())
		return processOf(t, os.Getpid()), nil, nil
	})

	_, err := mgr.Start()
	require.NoError(t, err)

	pid, running := mgr.Status()
	assert.True(t, running)
	assert.Equal(t, os.Getpid(), pid)
}

func TestGivenDaemonAlreadyRunningWhenStartedAgainThenIsNoOpAndDoesNotSpawn(t *testing.T) {
	mgr := newTestManager(t)
	require.NoError(t, mgr.pidFile.Write())

	spawnCalled := false
	stubSpawn(t, func(_ string, _ []string, _ *os.File) (*os.Process, <-chan struct{}, error) {
		spawnCalled = true
		return nil, nil, nil
	})

	_, err := mgr.Start()
	require.NoError(t, err)

	assert.False(t, spawnCalled, "Start must not spawn a second daemon when one is running")
}

func TestGivenNoDaemonRunningWhenStartedThenResultCarriesTheChildPidAndTheLogFile(t *testing.T) {
	mgr := newTestManager(t)
	stubSpawn(t, func(_ string, _ []string, _ *os.File) (*os.Process, <-chan struct{}, error) {
		require.NoError(t, mgr.pidFile.Write())
		return processOf(t, os.Getpid()), nil, nil
	})

	result, err := mgr.Start()

	require.NoError(t, err)
	assert.Equal(t, StartResult{PID: os.Getpid(), LogFile: mgr.paths.LogFile}, result)
}

func TestGivenDaemonAlreadyRunningWhenStartedThenResultSaysAlreadyRunningWithItsPid(t *testing.T) {
	mgr := newTestManager(t)
	require.NoError(t, mgr.pidFile.Write())

	result, err := mgr.Start()

	require.NoError(t, err)
	assert.Equal(t, StartResult{PID: os.Getpid(), LogFile: mgr.paths.LogFile, AlreadyRunning: true}, result)
}

func TestGivenAConcurrentStartThatWinsDuringTheWaitWhenStartedThenResultNamesTheWinner(t *testing.T) {
	// The ready pidfile is a racing winner's, not the spawned child's: the result
	// must name the pid that serves, as already running.
	mgr := newTestManager(t)
	loserPid := startRealChild(t, "exec sleep 60")
	stubSpawn(t, func(_ string, _ []string, _ *os.File) (*os.Process, <-chan struct{}, error) {
		require.NoError(t, mgr.pidFile.Write())
		return processOf(t, loserPid), nil, nil
	})

	result, err := mgr.Start()

	require.NoError(t, err)
	assert.Equal(t, StartResult{PID: os.Getpid(), LogFile: mgr.paths.LogFile, AlreadyRunning: true}, result,
		"the winner's pid, not the losing child's")
}

func TestGivenChildForwardedArgsWhenStartedThenDaemonSubcommandLeadsArgs(t *testing.T) {
	mgr := newTestManager(t)
	var capturedArgs []string
	stubSpawn(t, func(_ string, args []string, _ *os.File) (*os.Process, <-chan struct{}, error) {
		capturedArgs = args
		require.NoError(t, mgr.pidFile.Write())
		return processOf(t, os.Getpid()), nil, nil
	})

	_, err := mgr.Start()
	require.NoError(t, err)

	require.NotEmpty(t, capturedArgs)
	assert.Equal(t, DaemonSubcommand, capturedArgs[0], "daemon subcommand must lead the child args")
	assert.Contains(t, capturedArgs, "--port")
}

func TestGivenSpawnFailsWhenStartedThenResultIsZeroAndTheErrorIsReturned(t *testing.T) {
	mgr := newTestManager(t)
	stubSpawn(t, func(_ string, _ []string, _ *os.File) (*os.Process, <-chan struct{}, error) {
		return nil, nil, assert.AnError
	})

	result, err := mgr.Start()

	require.Error(t, err)
	assert.Equal(t, StartResult{}, result)
}

// shortStartupTimeout shrinks the package startup timeout for the duration of a
// test so the startup-timeout path runs without the multi-second production wait.
func shortStartupTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	original := startupTimeout
	startupTimeout = d
	t.Cleanup(func() { startupTimeout = original })
}

func TestGivenChildSpawnsButNeverReportsReadyWhenStartedThenErrorsAndKillsOrphan(t *testing.T) {
	mgr := newTestManager(t)
	shortStartupTimeout(t, 100*time.Millisecond)

	// The "child" is a real, long-lived process whose pid Start will try to clean
	// up. It never writes the pidfile, so the readiness wait times out.
	orphanPid := startRealChild(t, "exec sleep 60")
	stubSpawn(t, func(_ string, _ []string, _ *os.File) (*os.Process, <-chan struct{}, error) {
		return processOf(t, orphanPid), nil, nil
	})

	_, err := mgr.Start()

	require.Error(t, err, "a child that never reports ready must fail Start")
	assert.ErrorContains(t, err, "did not report ready within")
	assert.ErrorContains(t, err, `Check "`+mgr.paths.LogFile+`"`)
	assert.Eventually(t, func() bool { return !processAlive(orphanPid) }, 5*time.Second, 20*time.Millisecond,
		"the orphaned child must be killed on the startup-timeout path")
}

// stopRefusedText is what Start returns when the adb server on 127.0.0.1:port did not stop.
func stopRefusedText(port int, cause string) string {
	return fmt.Sprintf("an adb server on 127.0.0.1:%d was asked to stop (via 'adb kill-server') and did not (%s). Start the "+
		"bridge on another port with 'devicefarm-connect bridge start --port <n>' and point adb at it with 'adb -P <n>'. That adb "+
		"server keeps port %d and the bridge serves adb on the port you pass.", port, cause, port)
}

// heldPortText is what Start returns when another program holds 127.0.0.1:port.
func heldPortText(port int) string {
	return fmt.Sprintf("another program is listening on 127.0.0.1:%d, so the bridge cannot bind it. Start the bridge on another "+
		"port with 'devicefarm-connect bridge start --port <n>' and point adb at it with 'adb -P <n>', or stop that program and run "+
		"'devicefarm-connect bridge start --port %d'. The bridge listens on exactly the port you pass.", port, port)
}

// closingOccupant closes at once, so a probe does not wait out its timeout.
func closingOccupant(conn net.Conn) { conn.Close() }

// stubNoSpawn records whether Start spawned; the test asserts it did not.
func stubNoSpawn(t *testing.T) *bool {
	t.Helper()
	spawned := false
	stubSpawn(t, func(_ string, _ []string, _ *os.File) (*os.Process, <-chan struct{}, error) {
		spawned = true
		return nil, nil, assert.AnError
	})
	return &spawned
}

func TestGivenAnAdbServerOnThePortWhenStartedThenItIsStoppedBeforeTheChildSpawns(t *testing.T) {
	mgr := newTestManager(t)
	port := mgr.config.ListenPort
	logs := &lockedBuffer{}
	mgr.logger = slog.New(slog.NewTextHandler(logs, nil))
	server := startFakeAdbServer(t, port, killExits)
	// Stand in for the child: its own bind proves the stop freed the port.
	stubSpawn(t, func(_ string, _ []string, _ *os.File) (*os.Process, <-chan struct{}, error) {
		listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		require.NoError(t, err, "the adb server must be gone before the spawn")
		t.Cleanup(func() { listener.Close() })
		require.NoError(t, mgr.pidFile.Write())
		return processOf(t, os.Getpid()), nil, nil
	})

	result, err := mgr.Start()
	require.NoError(t, err)

	assert.Len(t, server.killFrames, 1, "one host:kill, as 'adb kill-server' sends")
	assert.Equal(t, "127.0.0.1:"+strconv.Itoa(port), result.StoppedAdbServer,
		"the stopped server's address comes back to the caller")
	assert.NotContains(t, logs.String(), "adb server", "the caller prints the notice; Start logs nothing")
	_, running := mgr.Status()
	assert.True(t, running)
}

func TestGivenAnAdbServerThatRefusesToStopWhenStartedThenNoSpawnAndTheTextNamesAnotherPort(t *testing.T) {
	tests := []struct {
		name      string
		answer    killAnswer
		wantCause string
	}{
		{name: "a server that rejects kills", answer: killRefused, wantCause: "it answered FAIL: " + adbRejectsKill},
		{name: "a server a program restarts at once", answer: killRestarted, wantCause: "it answered OKAY but port <port> is " +
			"still held after 1s; a program that restarts adb, such as Android Studio, can take it back at once"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mgr := newTestManager(t)
			port := mgr.config.ListenPort
			server := startFakeAdbServer(t, port, tc.answer)
			spawned := stubNoSpawn(t)

			_, err := mgr.Start()

			require.Error(t, err)
			assert.Equal(t, stopRefusedText(port, strings.ReplaceAll(tc.wantCause, "<port>", strconv.Itoa(port))), err.Error())
			assert.False(t, *spawned, "no child may spawn onto a held port")
			assert.Len(t, server.killFrames, 1)
		})
	}
}

func TestGivenAnAdbServerWasStoppedWhenTheSpawnFailsThenTheResultStillNamesIt(t *testing.T) {
	mgr := newTestManager(t)
	port := mgr.config.ListenPort
	startFakeAdbServer(t, port, killExits)
	stubSpawn(t, func(_ string, _ []string, _ *os.File) (*os.Process, <-chan struct{}, error) {
		return nil, nil, assert.AnError
	})

	result, err := mgr.Start()

	require.Error(t, err)
	assert.Equal(t, "127.0.0.1:"+strconv.Itoa(port), result.StoppedAdbServer,
		"a failed start must still report the adb server it stopped")
}

func TestGivenAnotherProgramOnThePortWhenStartedThenNoSpawnAndTheTextNamesAnotherPort(t *testing.T) {
	mgr := newTestManager(t)
	occupyPort(t, mgr.config.ListenPort, closingOccupant)
	spawned := stubNoSpawn(t)

	_, err := mgr.Start()

	require.Error(t, err)
	assert.Equal(t, heldPortText(mgr.config.ListenPort), err.Error())
	assert.False(t, *spawned, "no child may spawn onto a held port")
}

func TestGivenAnUnbindablePortWhenStartedThenNoSpawnAndTheTextQuotesTheDoctor(t *testing.T) {
	tests := []struct {
		name       string
		listenAddr string
		port       int
		// wantDoctor is the doctor command the text quotes, <port> for the port.
		wantDoctor string
	}{
		{name: "a privileged port", listenAddr: "127.0.0.1", port: 1, wantDoctor: "devicefarm-connect doctor --port 1"},
		{name: "an address not on this machine", listenAddr: "203.0.113.1", // TEST-NET-3: not this machine, never routed
			wantDoctor: "devicefarm-connect doctor --port <port> --listen-addr 203.0.113.1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.ListenAddr, cfg.ListenPort = tc.listenAddr, cmp.Or(tc.port, freePort(t))
			address := net.JoinHostPort(cfg.ListenAddr, strconv.Itoa(cfg.ListenPort))
			bindErr := bindListenPort(cfg.ListenAddr, cfg.ListenPort)
			if bindErr == nil {
				t.Skip("this user may bind " + address)
			}
			mgr := newTestManagerWithConfig(t, cfg)
			spawned := stubNoSpawn(t)

			_, err := mgr.Start()

			require.Error(t, err)
			assert.Equal(t, fmt.Sprintf("the bridge cannot bind %s: %v. Run '%s' for the fix, or start the bridge on another port "+
				"with 'devicefarm-connect bridge start --port <n>' and point adb at it with 'adb -P <n>'. The bridge listens on "+
				"exactly the port you pass.", address, bindErr, strings.ReplaceAll(tc.wantDoctor, "<port>", strconv.Itoa(cfg.ListenPort))),
				err.Error())
			assert.False(t, *spawned)
		})
	}
}

func TestGivenAConcurrentStartThatWinsWhileThePortIsHeldWhenStartedThenStartReportsSuccess(t *testing.T) {
	tests := []struct {
		name string
		// occupy holds the port as the winner's daemon and calls writePidFile when the probe reaches it.
		occupy func(t *testing.T, port int, writePidFile func()) *fakeAdbServer
	}{
		{name: "a winner with no device", occupy: func(t *testing.T, port int, writePidFile func()) *fakeAdbServer {
			occupyPort(t, port, func(conn net.Conn) {
				writePidFile()
				conn.Close()
			})
			return nil
		}},
		{name: "a winner whose device answers host:version", occupy: func(t *testing.T, port int, writePidFile func()) *fakeAdbServer {
			server := startFakeAdbServer(t, port, killExits)
			server.onVersion = writePidFile
			return server
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mgr := newTestManager(t)
			winner := NewPidFile(mgr.paths.PidFile, nil)
			written := make(chan error, 1)
			server := tc.occupy(t, mgr.config.ListenPort, func() { written <- winner.Write() })
			spawned := stubNoSpawn(t)

			_, err := mgr.Start()

			require.NoError(t, <-written)
			t.Cleanup(func() { _ = winner.Remove() })
			assert.NoError(t, err, "the pidfile re-read must find the winner")
			assert.False(t, *spawned)
			if server != nil {
				assert.Empty(t, server.killFrames, "the winner's daemon must never get host:kill")
			}
		})
	}
}

// childErrorLine is the line the child's entrypoint logs as it exits on err, through the same
// handler and call.
func childErrorLine(err string) string {
	var line bytes.Buffer
	slog.New(slog.NewTextHandler(&line, nil)).Error(config.BinaryName+" failed", "error", errors.New(err))
	return strings.TrimSuffix(line.String(), "\n")
}

// stubChildExit stands in for a child that logs line and exits before it reports ready.
// occupy runs first, so a test can take the port as a racer would.
func stubChildExit(t *testing.T, occupy func(), line string) {
	t.Helper()
	stubSpawn(t, func(_ string, _ []string, logOut *os.File) (*os.Process, <-chan struct{}, error) {
		occupy()
		_, err := fmt.Fprintln(logOut, line)
		require.NoError(t, err)
		exited := make(chan struct{})
		close(exited)
		return nil, exited, nil
	})
}

func TestGivenAChildThatExitsWithABindErrorWhenStartedThenThePortIsProbedAgainAndNamed(t *testing.T) {
	tests := []struct {
		name     string
		occupy   func(t *testing.T, port int) *fakeAdbServer
		wantText func(port int) string
	}{
		{name: "an adb server took the port", occupy: func(t *testing.T, port int) *fakeAdbServer {
			return startFakeAdbServer(t, port, killExits)
		}, wantText: func(port int) string {
			return fmt.Sprintf("an adb server took 127.0.0.1:%d before the bridge could bind it. Programs that restart adb "+
				"automatically, such as Android Studio, are known to do this. Start the bridge on another port with "+
				"'devicefarm-connect bridge start --port <n>' and point adb at it with 'adb -P <n>'. That adb server keeps port %d "+
				"and the bridge serves adb on the port you pass.", port, port)
		}},
		{name: "another program took the port", occupy: func(t *testing.T, port int) *fakeAdbServer {
			occupyPort(t, port, closingOccupant)
			return nil
		}, wantText: heldPortText},
		{name: "the port is free again", occupy: func(*testing.T, int) *fakeAdbServer { return nil }, wantText: func(port int) string {
			return fmt.Sprintf("another program held 127.0.0.1:%d while the bridge started and has released it. Run "+
				"'devicefarm-connect bridge start --port %d'. The bridge binds the port on that start.", port, port)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mgr := newTestManager(t)
			port := mgr.config.ListenPort
			var racer *fakeAdbServer
			stubChildExit(t, func() { racer = tc.occupy(t, port) },
				childErrorLine(fmt.Sprintf("listen on 127.0.0.1:%d: listen tcp 127.0.0.1:%d: bind: address already in use", port, port)))

			_, err := mgr.Start()

			require.Error(t, err)
			assert.Equal(t, tc.wantText(port), err.Error())
			if racer != nil {
				assert.Empty(t, racer.killFrames, "an adb server that took the port back is named, not stopped again")
			}
		})
	}
}

func TestGivenAChildThatExitsWithAnotherErrorWhenStartedThenThatErrorIsTheTextAtOnce(t *testing.T) {
	mgr := newTestManager(t)
	// An older run's error stays in the log; only what this child logs counts.
	require.NoError(t, os.WriteFile(mgr.paths.LogFile, []byte(childErrorLine("an older run's error")+"\n"), 0o600))
	const childError = "invalid configuration: max device streams 999 must stay under the remote access service's admission ceiling 32"
	// A real child through the real spawn: the exit watcher must end the wait.
	stubSpawn(t, func(_ string, _ []string, logOut *os.File) (*os.Process, <-chan struct{}, error) {
		return realSpawnDaemon("/bin/sh", []string{"-c", "echo '" + strings.ReplaceAll(childErrorLine(childError), "'", `'\''`) + "'; exit 1"}, logOut)
	})

	started := time.Now()
	_, err := mgr.Start()

	require.Error(t, err)
	assert.Less(t, time.Since(started), startupTimeout, "a child that exits ends the wait at once")
	assert.Equal(t, fmt.Sprintf("the bridge exited while starting: %s. Fix what it reports, then run 'devicefarm-connect bridge "+
		"start --port %d'. Its log is \"%s\".", childError, mgr.config.ListenPort, mgr.paths.LogFile), err.Error())
}

func TestGivenAChildWhoseControlSocketFailsWhenStartedThenItsErrorIsNotTakenForALostPort(t *testing.T) {
	mgr := newTestManager(t)
	// The control socket's bind error starts with "listen on" too; only the TCP address marks a lost port.
	controlError := fmt.Sprintf("listen on control socket %q: bind: address already in use", mgr.paths.ControlSocket)
	stubChildExit(t, func() {}, childErrorLine(controlError))

	_, err := mgr.Start()

	require.Error(t, err)
	assert.Equal(t, fmt.Sprintf("the bridge exited while starting: %s. Fix what it reports, then run 'devicefarm-connect bridge "+
		"start --port %d'. Its log is \"%s\".", controlError, mgr.config.ListenPort, mgr.paths.LogFile), err.Error())
}

func TestGivenAChildThatLogsNoErrorWhenItExitsOrTimesOutThenTheLogPathIsNamed(t *testing.T) {
	t.Run("the child exits", func(t *testing.T) {
		mgr := newTestManager(t)
		// A real child through the real spawn that logs only an INFO line and exits.
		stubSpawn(t, func(_ string, _ []string, logOut *os.File) (*os.Process, <-chan struct{}, error) {
			return realSpawnDaemon("/bin/sh", []string{"-c", `echo 'level=INFO msg="starting"'; exit 1`}, logOut)
		})

		_, err := mgr.Start()

		require.Error(t, err)
		assert.Equal(t, fmt.Sprintf("the bridge exited while starting and logged no error. Check \"%s\", then run "+
			"'devicefarm-connect bridge start --port %d'.", mgr.paths.LogFile, mgr.config.ListenPort), err.Error())
	})
	t.Run("the child never reports ready", func(t *testing.T) {
		mgr := newTestManager(t)
		shortStartupTimeout(t, 100*time.Millisecond)
		// No handle: the timeout's kill is a no-op, so no reused pid is signaled.
		stubSpawn(t, func(_ string, _ []string, _ *os.File) (*os.Process, <-chan struct{}, error) {
			return nil, nil, nil
		})

		_, err := mgr.Start()

		require.Error(t, err)
		assert.Equal(t, fmt.Sprintf("the bridge did not report ready within 100ms and logged no error, so it was stopped. Check "+
			"\"%s\", then run 'devicefarm-connect bridge start --port %d'.", mgr.paths.LogFile, mgr.config.ListenPort), err.Error())
	})
}

func TestGivenASlogErrorLineWhenParsedThenQuotedAndBareValuesBothRead(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		wantValue string
		wantFound bool
	}{
		{name: "a quoted value", line: `level=ERROR msg="x failed" error="listen on 127.0.0.1:1: \"busy\""`,
			wantValue: `listen on 127.0.0.1:1: "busy"`, wantFound: true},
		{name: "a bare value", line: `level=ERROR msg=failed error=boom pid=4`, wantValue: "boom", wantFound: true},
		{name: "an INFO line", line: `level=INFO msg="daemon started" error=boom`},
		{name: "no error value", line: `level=ERROR msg="control server stopped"`},
		{name: "a broken quote", line: `level=ERROR msg=failed error="unterminated`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			value, found := errorValue(tc.line)

			assert.Equal(t, tc.wantFound, found)
			assert.Equal(t, tc.wantValue, value)
		})
	}
}

func TestGivenALogWithErrorLinesWhenReadFromAnOffsetThenTheLastLaterOneIsTheChildsError(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "daemon.log")
	older := childErrorLine("an older run's error") + "\n"
	content := older + childErrorLine("first") + "\nlevel=INFO msg=next\n" + childErrorLine("second") + "\n"
	require.NoError(t, os.WriteFile(logPath, []byte(content), 0o600))
	assert.Equal(t, "second", lastChildError(logPath, int64(len(older))), "the last error after the offset wins")
	assert.Empty(t, lastChildError(logPath, int64(len(content))), "nothing was logged after the offset")
	assert.Empty(t, lastChildError(filepath.Join(t.TempDir(), "missing.log"), 0))
}

func TestGivenAlreadyDeadOrphanWhenKillOrphanThenDoesNotPanic(t *testing.T) {
	// A child already reaped (and no child at all) must be handled silently: the
	// process may have exited on its own before cleanup runs, and its handle then
	// signals nothing.
	reaped := exec.Command("true")
	require.NoError(t, reaped.Run())
	killOrphan(reaped.Process)
	killOrphan(nil)
}

func TestGivenDetachedChildWhenSpawnedThenLogfileIsItsStdoutAndStderr(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "daemon.log")
	logOut, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	require.NoError(t, err)
	defer logOut.Close()

	// The child writes one line per stream through the production spawn path.
	child, exited, err := realSpawnDaemon("/bin/sh", []string{"-c", "echo out-marker; echo err-marker >&2"}, logOut)
	require.NoError(t, err)
	require.Greater(t, child.Pid, 0)
	select {
	case <-exited:
	case <-time.After(testTimeout):
		t.Fatal("the exit channel must close when the child exits")
	}

	require.Eventually(t, func() bool {
		content, readErr := os.ReadFile(logPath)
		return readErr == nil &&
			strings.Contains(string(content), "out-marker") &&
			strings.Contains(string(content), "err-marker")
	}, testTimeout, testPollInterval, "both streams of the child must land in the logfile")
}

func TestGivenMissingExecutableWhenSpawnedThenErrors(t *testing.T) {
	logOut, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	require.NoError(t, err)
	defer logOut.Close()

	_, _, err = realSpawnDaemon(filepath.Join(t.TempDir(), "does-not-exist"), nil, logOut)

	require.Error(t, err)
}

func TestGivenChildNeverWritesPidFileWhenWaitedForThenTimesOut(t *testing.T) {
	mgr := newTestManager(t)

	// The child never writes the pidfile, so the readiness wait must time out.
	// Call waitUntilRunning directly with a tiny timeout to avoid the multi-second
	// production startup wait.
	err := mgr.waitUntilRunning(time.Millisecond, nil)

	require.Error(t, err)
}

func TestGivenNoDaemonRunningWhenStoppedThenResultSaysNotRunning(t *testing.T) {
	mgr := newTestManager(t)

	result, err := mgr.Stop()

	require.NoError(t, err)
	assert.Equal(t, StopResult{}, result, "no daemon to stop reads as not running and not killed")
}

func TestGivenDaemonAnsweringControlSocketWhenStoppedThenShutdownRequestedAndProcessGone(t *testing.T) {
	mgr := newTestManager(t)
	// The child exits on the shutdown request, as the daemon does.
	pid := startDaemonLikeChild(t, mgr, helperModeExit)

	started := time.Now()
	result, err := mgr.Stop()
	require.NoError(t, err)

	assert.Equal(t, StopResult{PID: pid, WasRunning: true}, result, "an answered shutdown is not a kill")
	assert.Less(t, time.Since(started), mgr.config.StopTimeout,
		"a daemon that exits on request must not wait out the stop timeout")
	assert.False(t, processAlive(pid), "process should be gone after Stop")
	_, err = os.Stat(mgr.paths.PidFile)
	assert.ErrorIs(t, err, os.ErrNotExist, "pidfile should be removed after a successful stop")
	// Only a clean exit unlinks the socket; a killed child leaves it.
	_, err = os.Stat(mgr.paths.ControlSocket)
	assert.ErrorIs(t, err, os.ErrNotExist, "the child must have exited on the request, not been killed")
}

func TestGivenControlSocketUnreachableWhenStoppedThenKilledWithoutWaitingStopTimeout(t *testing.T) {
	cfg := config.Default()
	cfg.StopTimeout = 30 * time.Second
	mgr := newTestManagerWithConfig(t, cfg)
	pid := startDaemonLikeChild(t, mgr, helperModeIgnore)
	// Lock held, but nothing listens at the socket path.
	require.NoError(t, os.Remove(mgr.paths.ControlSocket))

	started := time.Now()
	result, err := mgr.Stop()
	require.NoError(t, err)

	assert.Equal(t, StopResult{PID: pid, WasRunning: true, Killed: true}, result)
	assert.Less(t, time.Since(started), 5*time.Second, "an unreachable socket must skip the stop wait")
	assert.False(t, processAlive(pid), "a daemon nothing can reach must be killed")
	_, err = os.Stat(mgr.paths.PidFile)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestGivenDaemonAcknowledgesShutdownButLingersWhenStoppedThenEscalatesToKill(t *testing.T) {
	cfg := config.Default()
	cfg.StopTimeout = 200 * time.Millisecond // keep the graceful wait short
	mgr := newTestManagerWithConfig(t, cfg)
	// The child replies OK and stays, so only the kill can end it.
	pid := startDaemonLikeChild(t, mgr, helperModeIgnore)

	result, err := mgr.Stop()
	require.NoError(t, err)

	assert.Equal(t, StopResult{PID: pid, WasRunning: true, Killed: true}, result, "a lingering daemon reads as killed")
	assert.False(t, processAlive(pid), "process should be killed after the stop timeout")
	_, err = os.Stat(mgr.paths.PidFile)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestGivenDaemonRefusesShutdownWhenStoppedThenLogsRefusalAndEscalatesToKill(t *testing.T) {
	cfg := config.Default()
	cfg.StopTimeout = 200 * time.Millisecond // keep the graceful wait short
	mgr := newTestManagerWithConfig(t, cfg)
	logs := &lockedBuffer{}
	mgr.logger = slog.New(slog.NewTextHandler(logs, nil))
	// The child refuses and stays, so only the kill can end it.
	pid := startDaemonLikeChild(t, mgr, helperModeRefuse)

	started := time.Now()
	_, err := mgr.Stop()
	require.NoError(t, err)

	assert.GreaterOrEqual(t, time.Since(started), cfg.StopTimeout, "a refused shutdown keeps the stop timeout")
	assert.Regexp(t, `level=WARN msg="daemon refused shutdown[^\n]*error="refused for the test"`, logs.String(),
		"the daemon's error text must reach the warn line")
	assert.False(t, processAlive(pid), "process should be killed after the stop timeout")
	_, err = os.Stat(mgr.paths.PidFile)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestGivenNoDaemonRunningWhenStatusCheckedThenReportsNotRunning(t *testing.T) {
	mgr := newTestManager(t)

	pid, running := mgr.Status()

	assert.False(t, running)
	assert.Zero(t, pid)
}

func TestGivenInvalidConfigWhenRunForegroundThenErrorsBeforeServing(t *testing.T) {
	cfg := config.Default()
	cfg.ListenPort = 0 // out of the valid 1..65535 range: fails Validate
	mgr := newTestManagerWithConfig(t, cfg)

	err := mgr.RunForeground(context.Background())

	require.Error(t, err)
	// The pidfile must not be left behind by a config that never started serving.
	_, readErr := os.Stat(mgr.paths.PidFile)
	assert.ErrorIs(t, readErr, os.ErrNotExist)
}

func TestGivenListenPortAlreadyInUseWhenRunForegroundThenErrorsWithoutTouchingPidFile(t *testing.T) {
	// Occupy a port, then point the daemon's listener at it. RunForeground must
	// surface the bind failure and, crucially, must not have gone anywhere near the
	// pidfile: the pidfile is the parent's "daemon is up and listening" signal, so
	// the listener must be bound BEFORE the pidfile is written.
	//
	// The pidfile is pre-seeded with a sentinel and asserted untouched. This is what
	// discriminates the fix from the old ordering: the old code wrote its own pid
	// and then removed the file via defer when Run failed, so a terminal "pidfile
	// absent" check would pass on the unfixed code too. An untouched sentinel proves
	// the write/remove block was never reached because Listen failed first.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer occupied.Close()

	cfg := config.Default()
	cfg.ListenPort = occupied.Addr().(*net.TCPAddr).Port
	mgr := newTestManagerWithConfig(t, cfg)
	const sentinelRecord = "sentinel" // RunForeground never reads the pidfile on entry
	require.NoError(t, os.WriteFile(mgr.paths.PidFile, []byte(sentinelRecord), 0o600))

	err = mgr.RunForeground(context.Background())

	require.Error(t, err, "binding an in-use listen port must fail RunForeground")
	assert.True(t, strings.HasPrefix(err.Error(), listenErrorPrefix("127.0.0.1", cfg.ListenPort)),
		"Start reads a failed bind by this prefix: %v", err)
	got, readErr := os.ReadFile(mgr.paths.PidFile)
	require.NoError(t, readErr, "the fix must leave the pidfile untouched, not rewrite then remove it")
	assert.Equal(t, sentinelRecord, string(got), "RunForeground must not have written its own pid before the listener bound")
}

func TestGivenServingDaemonWhenSecondBodyRacesForItsPortThenTheFirstStaysReachable(t *testing.T) {
	// Two `device add` runs with no daemon can both spawn a child. The loser must fail
	// its TCP bind before it touches the control socket, or its stale-socket removal
	// unlinks the winner's socket and the winner turns unreachable.
	cfg := config.Default()
	cfg.ListenPort = freePort(t)
	winner := newTestManagerWithConfig(t, cfg)
	done, cancel := startForeground(t, winner)
	loser := NewManager(cfg, winner.paths, newFakeClient(testEndpoint, "unused"), slog.New(slog.DiscardHandler))

	err := loser.RunForeground(context.Background())

	require.Error(t, err, "the loser must fail on the occupied port")
	resp, sendErr := control.NewDaemonClient(winner.paths.ControlSocket).Send(control.Request{Op: control.OpStatus})
	require.NoError(t, sendErr, "the winner's control socket must still answer")
	assert.True(t, resp.OK)
	_, running := winner.pidFile.RunningPID()
	assert.True(t, running, "the winner's pidfile must stay")
	cancel()
	require.NoError(t, <-done)
}

func TestGivenServingDaemonWhenAddAndShutdownSentOverControlSocketThenAttachesAndStops(t *testing.T) {
	// Exercises RunForeground's wiring end to end: the control handler is served
	// on the real Unix socket, an add resolves (stubbed) and attaches, and a
	// control shutdown cancels the daemon context so RunForeground returns
	// cleanly. This is the daemon-side counterpart of the CLI's fakeDaemon tests,
	// which fake this side.
	mock := testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{OnMessage: answerAttach})
	cfg := config.Default()
	cfg.ListenPort = freePort(t)
	mgr := newTestManagerWithClient(t, cfg, newFakeClient(mock.URL, "Pixel 10 (OS 17)"))

	// The pidfile is written only after both sockets are bound, so once it
	// appears the control socket is safe to dial.
	done, cancel := startForeground(t, mgr)
	defer cancel()

	client := control.NewDaemonClient(mgr.paths.ControlSocket)
	addResp, err := client.Send(control.Request{Op: control.OpAdd, ARN: testSessionArnOne})
	require.NoError(t, err)
	require.True(t, addResp.OK, "add over the live control socket must succeed: %s", addResp.Error)
	assert.Equal(t, "Pixel 10 (OS 17)", addResp.DeviceLabel)
	assert.Equal(t, cfg.ListenPort, addResp.Port)

	shutdownResp, err := client.Send(control.Request{Op: control.OpShutdown})
	require.NoError(t, err)
	assert.True(t, shutdownResp.OK)
	select {
	case err := <-done:
		assert.NoError(t, err, "a control shutdown is a clean exit")
	case <-time.After(5 * time.Second):
		t.Fatal("RunForeground did not return after a control shutdown")
	}
}

func TestGivenValidConfigWhenRunForegroundContextCancelledThenServesThenTearsDownCleanly(t *testing.T) {
	cfg := config.Default()
	cfg.ListenPort = freePort(t) // a real, free port (Validate rejects port 0)
	mgr := newTestManagerWithConfig(t, cfg)

	done, cancel := startForeground(t, mgr)

	cancel()
	// Stop treats a released record as "nothing held", so the control socket must
	// be unlinked before the record drops: a start that saw the record gone binds
	// the same path, and a late unlink would take its socket. Deterministic on
	// this order; the old order's window is too short for this to detect.
	require.Eventually(t, func() bool {
		_, err := os.Lstat(mgr.paths.PidFile)
		return errors.Is(err, os.ErrNotExist)
	}, testTimeout, testPollInterval, "the record is released on exit")
	_, socketErr := os.Lstat(mgr.paths.ControlSocket)
	assert.ErrorIs(t, socketErr, os.ErrNotExist, "control socket still present after the record was released")
	select {
	case err := <-done:
		assert.NoError(t, err, "a context-cancelled shutdown is clean")
	case <-time.After(testTimeout):
		t.Fatal("RunForeground did not return after context cancellation")
	}
}

func TestGivenNoRemoteAccessClientWhenRunForegroundThenRefusesToStart(t *testing.T) {
	// A manager built for the lifecycle commands has no remote access client. If one
	// ever reached the daemon body, every add and refresh would nil-panic inside a
	// detached process, so RunForeground must refuse up front, before it binds
	// anything or writes the pidfile.
	cfg := config.Default()
	cfg.ListenPort = freePort(t)
	mgr := newTestManagerWithClient(t, cfg, nil)

	err := mgr.RunForeground(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "remote access client")
	_, readErr := os.Stat(mgr.paths.PidFile)
	assert.ErrorIs(t, readErr, os.ErrNotExist, "a refused daemon must not leave a pidfile")
}

func TestGivenForwardReverseEnvOverridesWhenTheDaemonBodyRunsThenItAppliesThem(t *testing.T) {
	// The overrides are applied only in the daemon body, beside the cadence overrides,
	// so this asserts RunForeground reads them; the consumers land in later changes.
	t.Setenv("DEVICEFARM_CONNECT_DEVICE_STREAM_STATUS_MS", "1234")
	t.Setenv("DEVICEFARM_CONNECT_MAX_DEVICE_STREAMS", "5")
	t.Setenv("DEVICEFARM_CONNECT_MAX_FORWARD_RULES", "6")
	t.Setenv("DEVICEFARM_CONNECT_MAX_REVERSE_RULES", "7")
	t.Setenv("DEVICEFARM_CONNECT_NOTIFIER_KEEPALIVE_MS", "2345")
	t.Setenv("DEVICEFARM_CONNECT_NOTIFIER_BACKOFF_MS", "345")
	t.Setenv("DEVICEFARM_CONNECT_PARK_PING_MS", "456")
	cfg := config.Default()
	cfg.ListenPort = freePort(t)
	mgr := newTestManagerWithConfig(t, cfg)

	done, cancel := startForeground(t, mgr)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("RunForeground did not return after context cancellation")
	}

	assert.Equal(t, 1234*time.Millisecond, mgr.config.DeviceStreamStatusTimeout)
	assert.Equal(t, 5, mgr.config.MaxDeviceStreams)
	assert.Equal(t, 6, mgr.config.MaxForwardRules)
	assert.Equal(t, 6, mgr.attachmentParams().MaxForwardRules, "the limit must reach the attachment params")
	assert.Equal(t, 7, mgr.config.MaxReverseRules)
	assert.Equal(t, 7, mgr.attachmentParams().MaxReverseRules, "the reverse limit must reach the attachment params")
	assert.Equal(t, 2345*time.Millisecond, mgr.config.NotifierKeepAlive)
	assert.Equal(t, 345*time.Millisecond, mgr.config.NotifierBackoffInitial)
	assert.Equal(t, config.DefaultNotifierBackoffMax, mgr.config.NotifierBackoffMax,
		"the backoff cap has no override")
	assert.Equal(t, 456*time.Millisecond, mgr.attachmentParams().ParkPingInterval,
		"the park ping must reach the attachment params")
}

func TestGivenAnEnvOverrideThatBreaksValidationWhenTheDaemonBodyRunsThenItRefusesToStart(t *testing.T) {
	// Overrides are applied before Validate, so a hidden variable cannot smuggle in a
	// value the flags could not: an initial backoff above the cap inverts the range.
	t.Setenv("DEVICEFARM_CONNECT_NOTIFIER_BACKOFF_MS", strconv.FormatInt(config.DefaultNotifierBackoffMax.Milliseconds()+1, 10))
	cfg := config.Default()
	cfg.ListenPort = freePort(t)
	mgr := newTestManagerWithConfig(t, cfg)

	err := mgr.RunForeground(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid configuration")
	_, readErr := os.Stat(mgr.paths.PidFile)
	assert.ErrorIs(t, readErr, os.ErrNotExist, "a refused daemon must not leave a pidfile")
}

// lockedBuffer collects log lines the daemon goroutine writes while the test
// goroutine reads them.
type lockedBuffer struct {
	contentsMutex sync.Mutex
	contents      bytes.Buffer
}

func (buffer *lockedBuffer) Write(p []byte) (int, error) {
	buffer.contentsMutex.Lock()
	defer buffer.contentsMutex.Unlock()
	return buffer.contents.Write(p)
}

func (buffer *lockedBuffer) String() string {
	buffer.contentsMutex.Lock()
	defer buffer.contentsMutex.Unlock()
	return buffer.contents.String()
}

// The add's serial fetch and the attach's host reply prefetch, framed.
const (
	hostVersionRequest     = "000chost:version"
	hostDevicesRequest     = "000chost:devices"
	hostDevicesLongRequest = "000ehost:devices-l"
	hostFeaturesRequest    = "000dhost:features"
)

// attachFetches is what an add sends, one session per request, in byte order: the serial
// fetch's host:devices, then one fetch of each cached host reply.
var attachFetches = []string{hostDevicesRequest, hostDevicesRequest, hostVersionRequest, hostFeaturesRequest,
	hostDevicesLongRequest}

// transportSelectRequest is the framed select the park and the reverse clear send first.
const transportSelectRequest = "0012host:transport-any"

// attachSessions is attachFetches plus the park's session, in byte order.
var attachSessions = append(slices.Clone(attachFetches), transportSelectRequest)

// statusRequestFrame is the add's status read. A plain session mock serves it as one more
// session; the server mock routes it to the status mock instead.
const statusRequestFrame = `{"message":"StatusMessage","parameters":{}}`

// statusTimeLeft and statusReplyFrame are answerAttach's status answer, in the server's
// double-encoded shape, so a plain-mock add completes its status read at once.
const statusTimeLeft = 140 * time.Minute

var statusReplyFrame = fmt.Sprintf(
	`{"status": "SUCCESS", "message": null, "value": "{\"status\": \"running\", \"timeLeft\": %.1f}"}`,
	statusTimeLeft.Seconds())

// attachSessionsWithStatus is attachSessions plus the status read, in byte order.
var attachSessionsWithStatus = append(slices.Clone(attachSessions), statusRequestFrame)

// attachSerial is the serial the mocks' one device row names.
const attachSerial = "R3CN30ABCDE"

// hostDevicesReply is the host:devices reply naming attachSerial, framed.
var hostDevicesReply = fmt.Sprintf("OKAY%04x%s", len(attachSerial+"\tdevice\n"), attachSerial+"\tdevice\n")

// answerAttach plays the server for an attach and a relayed command: one device row to
// host:devices, OKAY to a select, the status reply to the status request, echo for the rest.
func answerAttach(_ *testsupport.MockSession, data []byte) []byte {
	switch string(data) {
	case hostDevicesRequest:
		return []byte(hostDevicesReply)
	case transportSelectRequest:
		return []byte("OKAY")
	case statusRequestFrame:
		return []byte(statusReplyFrame)
	}
	return data
}

// answerReverseClear is answerAttach plus both OKAYs to the reverse clear's kill.
func answerReverseClear(session *testsupport.MockSession, data []byte) []byte {
	if string(data) == "0017reverse:killforward-all" {
		return []byte("OKAYOKAY")
	}
	return answerAttach(session, data)
}

// sessionInbounds returns what each session the mock minted received, oldest first.
func sessionInbounds(mock *testsupport.SessionMockWSS) []string {
	var inbounds []string
	for _, session := range mock.Sessions() {
		inbounds = append(inbounds, string(session.Inbound()))
	}
	return inbounds
}

// sortedInbounds is sessionInbounds in byte order, for sessions that race each other.
func sortedInbounds(mock *testsupport.SessionMockWSS) []string {
	inbounds := sessionInbounds(mock)
	slices.Sort(inbounds)
	return inbounds
}

func TestGivenARunningDaemonWhenADeviceIsAddedThenANotifierOpensAndFollowsTheAttachment(t *testing.T) {
	// The whole wiring in one test: the daemon body hands the dialer to the attachment, the
	// add fetches the serial, the attach fetches the host replies, opens a notifier on the
	// reverse route and clears the server's reverse rules once it is ready, and the shutdown
	// detach closes the notifier.
	sessions, mock, _ := testsupport.NewServerMockWSS(t, testsupport.SessionMockConfig{OnMessage: answerReverseClear})
	cfg := config.Default()
	cfg.ListenPort = freePort(t)
	cfg.ReverseEnabled = true
	mgr := newTestManagerWithClient(t, cfg, newFakeClient(mock.URL, "Pixel 10 (OS 17)"))

	done, cancel := startForeground(t, mgr)
	client := control.NewDaemonClient(mgr.paths.ControlSocket)
	addResp, err := client.Send(control.Request{Op: control.OpAdd, ARN: testSessionArnOne})
	require.NoError(t, err)
	require.True(t, addResp.OK, "add over the live control socket must succeed: %s", addResp.Error)
	require.Eventually(t, func() bool { return mock.NotifiersAccepted() == 1 }, testTimeout,
		testPollInterval, "attaching a device must open exactly one notifier")
	// The clear's kill follows its select's reply, so wait for the whole of both sessions.
	require.Eventually(t, func() bool {
		return slices.Equal(sortedInbounds(sessions),
			append(slices.Clone(attachSessions), transportSelectRequest+"0017reverse:killforward-all"))
	}, testTimeout, testPollInterval,
		"the add must fetch each host reply once, park one pre-selected websocket and clear the server's reverse rules once")

	cancel()

	select {
	case runErr := <-done:
		require.NoError(t, runErr)
	case <-time.After(testTimeout):
		t.Fatal("RunForeground did not return after context cancellation")
	}
	assert.Eventually(t, func() bool { return mock.NotifiersClosed() == 1 }, testTimeout,
		testPollInterval, "the shutdown detach must close the notifier")
	assert.Empty(t, mock.Refusals(), "the daemon's hello must satisfy the server")
}

func TestGivenAnAttachedDeviceWhenIdleThenItsOnlySessionsAreTheHostFetchesAndThePark(t *testing.T) {
	// After the add's serial fetch and the attach's host reply fetches, an idle attachment holds
	// only its parked websocket, pre-selected.
	mock := testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{OnMessage: answerAttach})
	cfg := config.Default()
	cfg.ListenPort = freePort(t)
	mgr := newTestManagerWithClient(t, cfg, newFakeClient(mock.URL, "Pixel 10 (OS 17)"))

	done, cancel := startForeground(t, mgr)
	client := control.NewDaemonClient(mgr.paths.ControlSocket)
	addResp, err := client.Send(control.Request{Op: control.OpAdd, ARN: testSessionArnOne})
	require.NoError(t, err)
	require.True(t, addResp.OK, "add over the live control socket must succeed: %s", addResp.Error)
	sessions := len(attachSessionsWithStatus)
	require.Eventually(t, func() bool {
		return mock.SessionsOpened() == sessions && mock.ConnectionsAccepted() == sessions
	}, testTimeout, testPollInterval, "the add must fetch each host reply and park a websocket")

	assert.Never(t, func() bool { return mock.SessionsOpened() > sessions || mock.ConnectionsAccepted() > sessions },
		500*time.Millisecond, testPollInterval, "an idle attachment dials nothing more")
	assert.Equal(t, attachSessionsWithStatus, sortedInbounds(mock),
		"each fetch sends its request alone, the park its select, the status read its request")

	cancel()

	select {
	case runErr := <-done:
		require.NoError(t, runErr)
	case <-time.After(testTimeout):
		t.Fatal("RunForeground did not return after context cancellation")
	}
}

func TestGivenAnAttachedDeviceWhenAnAdbCommandArrivesThenItRidesTheWebsocketParkedAhead(t *testing.T) {
	// Full wiring: attach parks a pre-selected websocket, the first command takes it, the slot
	// refills.
	mock := testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{OnMessage: answerAttach})
	cfg := config.Default()
	cfg.ListenPort = freePort(t)
	mgr := newTestManagerWithClient(t, cfg, newFakeClient(mock.URL, "Pixel 10 (OS 17)"))

	done, cancel := startForeground(t, mgr)
	client := control.NewDaemonClient(mgr.paths.ControlSocket)
	addResp, err := client.Send(control.Request{Op: control.OpAdd, ARN: testSessionArnOne})
	require.NoError(t, err)
	require.True(t, addResp.OK, "add over the live control socket must succeed: %s", addResp.Error)
	sessions := len(attachSessionsWithStatus)
	require.Eventually(t, func() bool {
		return mock.ConnectionsAccepted() == sessions && mock.SessionsOpened() == sessions
	}, testTimeout, testPollInterval, "attaching a device must park one adb websocket ahead of any command")

	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.ListenPort)))
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(testTimeout)))
	service := "000chost:version"
	_, err = fmt.Fprintf(conn, "000ehost:tport:any%s", service)
	require.NoError(t, err)
	selectReply := make([]byte, 12)
	_, err = io.ReadFull(conn, selectReply)
	require.NoError(t, err, "the daemon answers the select")
	echoed := make([]byte, len(service))
	_, err = io.ReadFull(conn, echoed)
	require.NoError(t, err, "the relayed command comes back through the parked websocket")
	assert.Equal(t, service, string(echoed))
	assert.Contains(t, sessionInbounds(mock), transportSelectRequest+service,
		"the command rides the parked session behind its select, with no select of its own")
	require.Eventually(t, func() bool {
		return mock.ConnectionsAccepted() == sessions+1 && mock.SessionsOpened() == sessions+1
	}, testTimeout, testPollInterval, "the slot must refill behind the take, pre-selected on a session of its own")

	cancel()

	select {
	case runErr := <-done:
		require.NoError(t, runErr)
	case <-time.After(testTimeout):
		t.Fatal("RunForeground did not return after context cancellation")
	}
}

// reverseOffFail is the framed FAIL a reverse request reads from a daemon that did not opt in.
const reverseOffFail = "FAIL0047adb reverse is off for this client; restart it with --allow-adb-reverse"

// adbCommand sends service behind a `host:tport:any` select and returns what the daemon writes
// after the select reply, up to the close.
func adbCommand(t *testing.T, listenPort int, service string) string {
	t.Helper()
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(listenPort)))
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(testTimeout)))
	_, err = fmt.Fprintf(conn, "000ehost:tport:any%04x%s", len(service), service)
	require.NoError(t, err)
	selectReply := make([]byte, 12)
	_, err = io.ReadFull(conn, selectReply)
	require.NoError(t, err, "the daemon answers the select")
	reply, err := io.ReadAll(conn)
	require.NoError(t, err)
	return string(reply)
}

func TestGivenReverseOffWhenTheDaemonAttachesThenNoNotifierOpensAndOnlyReverseIsRefused(t *testing.T) {
	// The default must reach the attachment, not just the config: the proof is a server that sees
	// no notifier while the daemon still installs a forward rule and refuses a reverse one itself.
	_, mock, _ := testsupport.NewServerMockWSS(t, testsupport.SessionMockConfig{OnMessage: answerAttach})
	cfg := config.Default()
	cfg.ListenPort = freePort(t)
	mgr := newTestManagerWithClient(t, cfg, newFakeClient(mock.URL, "Pixel 10 (OS 17)"))

	done, cancel := startForeground(t, mgr)
	client := control.NewDaemonClient(mgr.paths.ControlSocket)
	addResp, err := client.Send(control.Request{Op: control.OpAdd, ARN: testSessionArnOne})
	require.NoError(t, err)
	require.True(t, addResp.OK, "add over the live control socket must succeed: %s", addResp.Error)

	assert.Never(t, func() bool { return mock.NotifiersAccepted() > 0 }, 500*time.Millisecond,
		testPollInterval, "a daemon that did not opt in must never dial a notifier")
	assert.True(t, strings.HasPrefix(adbCommand(t, cfg.ListenPort, "forward:tcp:0;tcp:9999"), "OKAYOKAY"),
		"a forward rule must still install, or the absent notifier proves nothing")
	assert.Equal(t, reverseOffFail, adbCommand(t, cfg.ListenPort, "reverse:forward:tcp:4;tcp:5"),
		"a reverse request must get the reverse-off FAIL from the daemon itself")

	cancel()

	select {
	case runErr := <-done:
		require.NoError(t, runErr)
	case <-time.After(testTimeout):
		t.Fatal("RunForeground did not return after context cancellation")
	}
}

func TestGivenAPlaintextInteractiveEndpointWhenAddIsSentThenTheAddFailsAndNoDialReachesTheEndpoint(t *testing.T) {
	// The resolver, not the dialer, picks the scheme, so the refusal must hold at the daemon's
	// dial sites. The add's serial fetch dials first, and its refusal fails the add before the
	// notifier or the warm socket keeper can dial.
	counterAddress, accepted := testsupport.NewAcceptCounter(t)
	cfg := config.Default()
	cfg.ListenPort = freePort(t)
	cfg.ReverseEnabled = true
	mgr := newTestManagerWithClient(t, cfg, newFakeClient(fmt.Sprintf("ws://%s", counterAddress), "Pixel 10 (OS 17)"))

	done, cancel := startForeground(t, mgr)
	client := control.NewDaemonClient(mgr.paths.ControlSocket)
	addResp, err := client.Send(control.Request{Op: control.OpAdd, ARN: testSessionArnOne})
	require.NoError(t, err)
	require.False(t, addResp.OK, "the serial fetch's refused dial must fail the add")
	assert.Contains(t, addResp.Error, "refused scheme")

	statusResp, err := client.Send(control.Request{Op: control.OpStatus})
	require.NoError(t, err)
	assert.Empty(t, statusResp.DeviceLabel, "a failed add must leave nothing attached")

	cancel()

	select {
	case runErr := <-done:
		require.NoError(t, runErr)
	case <-time.After(testTimeout):
		t.Fatal("RunForeground did not return after context cancellation")
	}
	assert.Zero(t, accepted(), "nothing may reach a plaintext endpoint")
}

func TestGivenARunningDaemonWhenItStopsThenTheAttachmentIsCleared(t *testing.T) {
	// A device still attached at shutdown must be detached by the daemon body, which
	// is what will close the attachment's forward listeners once the accept loop has
	// stopped. The detach line is the only observable: the daemon's ActiveDevice is
	// not reachable from here and the control socket is gone by then.
	mock := testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{OnMessage: answerAttach})
	cfg := config.Default()
	cfg.ListenPort = freePort(t)
	mgr := newTestManagerWithClient(t, cfg, newFakeClient(mock.URL, "Pixel 10 (OS 17)"))
	logs := &lockedBuffer{}
	mgr.logger = slog.New(slog.NewTextHandler(logs, nil))

	done, cancel := startForeground(t, mgr)
	client := control.NewDaemonClient(mgr.paths.ControlSocket)
	addResp, err := client.Send(control.Request{Op: control.OpAdd, ARN: testSessionArnOne})
	require.NoError(t, err)
	require.True(t, addResp.OK, "add over the live control socket must succeed: %s", addResp.Error)

	cancel()

	select {
	case runErr := <-done:
		require.NoError(t, runErr)
	case <-time.After(testTimeout):
		t.Fatal("RunForeground did not return after context cancellation")
	}
	// Line-scoped: the attach line carries the same label, so a bare Contains would
	// pass on it alone.
	assert.Regexp(t, `detached device on shutdown.*Pixel 10 \(OS 17\)`, logs.String(),
		"a stopping daemon must detach the device it still holds, and name it")
}

func TestGivenUnlockedPidFileNamingLiveProcessWhenStoppedThenNothingIsSignaled(t *testing.T) {
	mgr := newTestManager(t)
	// The daemon died without cleanup; an unrelated process now has its pid. No
	// lock holder, so Stop must clear the record and not signal the process.
	foreign := startRealChild(t, "exec sleep 60")
	require.NoError(t, os.WriteFile(mgr.paths.PidFile, []byte(strconv.Itoa(foreign)), 0o600))

	_, err := mgr.Stop()
	require.NoError(t, err)

	assert.True(t, processAlive(foreign), "a process that did not write the pidfile must never be signaled")
	_, err = os.Stat(mgr.paths.PidFile)
	assert.ErrorIs(t, err, os.ErrNotExist, "the stale pidfile is cleared")
}

func TestGivenDaemonPidReusedBetweenProbeAndHandleWhenStoppedThenSuccessorNotSignaledAndRecordCleared(t *testing.T) {
	mgr := newTestManager(t)
	daemonPid := startDaemonLikeChild(t, mgr, helperModeIgnore)
	// Stands in for the process that took the daemon's pid.
	successorPid := startRealChild(t, "exec sleep 60")
	// The daemon dies between Stop's first probe and the handle, and the handle
	// lands on the successor. The re-probe must see the lock gone.
	stubFindProcess(t, func(target int) (*os.Process, error) {
		require.Equal(t, daemonPid, target)
		killProcess(t, target)
		return os.FindProcess(successorPid)
	})

	_, err := mgr.Stop()
	require.NoError(t, err)

	// Signal delivery is asynchronous: Never gives a wrongly signaled successor
	// time to die.
	assert.Never(t, func() bool { return !processAlive(successorPid) }, 100*time.Millisecond, 10*time.Millisecond,
		"a process that took the daemon's pid must never be signaled")
	_, err = os.Stat(mgr.paths.PidFile)
	assert.ErrorIs(t, err, os.ErrNotExist, "the dead daemon's record is cleared")
}

func TestGivenDaemonReleasesItsRecordOnShutdownButStaysAliveWhenStoppedThenNotKilled(t *testing.T) {
	cfg := config.Default()
	cfg.StopTimeout = time.Second
	mgr := newTestManagerWithConfig(t, cfg)
	// After the OK reply the lock is gone but the pid lives: a pid reused during
	// the stop wait. Stop must not kill it.
	pid := startDaemonLikeChild(t, mgr, helperModeRelease)

	_, err := mgr.Stop()
	require.NoError(t, err)

	assert.True(t, processAlive(pid), "a pid whose writer released the record must not be killed")
	_, err = os.Stat(mgr.paths.PidFile)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestGivenProcessHandleUnavailableAndRecordReleasedWhenStoppedThenReportsStopped(t *testing.T) {
	mgr := newTestManager(t)
	daemonPid := startDaemonLikeChild(t, mgr, helperModeIgnore)
	// The daemon dies after Stop's first probe; the handle lookup then fails, as
	// on Windows for a dead pid. A released record must read as stopped, not as
	// an error.
	stubFindProcess(t, func(target int) (*os.Process, error) {
		require.Equal(t, daemonPid, target)
		killProcess(t, target)
		return nil, assert.AnError
	})

	_, err := mgr.Stop()
	require.NoError(t, err)

	_, err = os.Stat(mgr.paths.PidFile)
	assert.ErrorIs(t, err, os.ErrNotExist, "the dead daemon's record is cleared")
}

func TestGivenProcessHandleUnavailableAndRecordHeldWhenStoppedThenErrorReturnedAndDaemonLeftAlone(t *testing.T) {
	cases := []struct {
		name      string
		lookupErr error
		wantHint  bool
	}{
		{name: "permission denied names an elevated shell", lookupErr: fmt.Errorf("open process: %w", os.ErrPermission), wantHint: true},
		{name: "any other failure carries no hint", lookupErr: assert.AnError, wantHint: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			mgr := newTestManager(t)
			// Release mode: a shutdown request Stop must not send would drop the
			// record and fail the held-record assertion below.
			pid := startDaemonLikeChild(t, mgr, helperModeRelease)
			// The handle lookup fails while the child holds its record, as on
			// Windows when an elevated daemon is stopped from a normal shell.
			// Stop must report the failure and leave the daemon alone.
			stubFindProcess(t, func(target int) (*os.Process, error) {
				require.Equal(t, pid, target)
				return nil, testCase.lookupErr
			})

			_, err := mgr.Stop()

			require.ErrorIs(t, err, testCase.lookupErr)
			if testCase.wantHint {
				assert.Contains(t, err.Error(), "stop it from an elevated shell")
			} else {
				assert.NotContains(t, err.Error(), "stop it from an elevated shell")
			}
			assert.True(t, processAlive(pid), "a daemon out of reach must not be killed")
			current, running := mgr.pidFile.RunningPID()
			assert.True(t, running, "the held record stays")
			assert.Equal(t, pid, current)
		})
	}
}

// processOf is a handle on pid, for a stub child.
func processOf(t *testing.T, pid int) *os.Process {
	t.Helper()
	process, err := os.FindProcess(pid)
	require.NoError(t, err)
	return process
}
