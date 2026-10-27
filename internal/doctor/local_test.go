package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/adbwire"
	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/control"
	"github.com/aws/device-connect-devicefarm/internal/daemon"
	"github.com/aws/device-connect-devicefarm/internal/forward"
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

// checkByID finds the check named by id in checks.
func checkByID(t *testing.T, checks []Check, id string) Check {
	t.Helper()
	for _, check := range checks {
		if check.ID == id {
			return check
		}
	}
	t.Fatalf("no check %q", id)
	return Check{}
}

// runLocal runs the one local check named by id over state.
func runLocal(t *testing.T, inputs Inputs, state *runState, id string) Result {
	t.Helper()
	return checkByID(t, localChecks(inputs, state), id).Run(context.Background())
}

// occupyPort binds the port and serves every accepted connection with serve until cleanup.
func occupyPort(t *testing.T, port int, serve func(net.Conn)) {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	require.NoError(t, err)
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go serve(conn)
		}
	}()
}

// answering serves one host:version request with reply. "OKAY00040029" is adb 1.0.41's answer.
func answering(reply string) func(net.Conn) {
	return func(conn net.Conn) {
		defer conn.Close()
		request := make([]byte, len("000chost:version"))
		if _, err := io.ReadFull(conn, request); err == nil && string(request) == "000chost:version" {
			_, _ = conn.Write([]byte(reply))
		}
	}
}

// silentOccupant holds the connection open and never writes.
func silentOccupant(conn net.Conn) {
	defer conn.Close()
	_, _ = io.Copy(io.Discard, conn)
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

func TestGivenTheListenPortWhenCheckedThenTheRowNamesItsOccupant(t *testing.T) {
	// The occupant exchange must settle inside the row's own bound, so a silent occupant renders
	// the bind error, never the runner's timeout text.
	require.Less(t, 2*occupantReplyTimeout, listenPortTimeout)
	tests := []struct {
		name        string
		setup       func(t *testing.T, inputs *Inputs, state *runState)
		wantStatus  Status
		wantSummary string
		wantHint    func(port int) string
	}{
		{name: "free", setup: func(*testing.T, *Inputs, *runState) {}, wantStatus: Pass, wantSummary: "is free"},
		{name: "an adb server", setup: func(t *testing.T, inputs *Inputs, _ *runState) {
			occupyPort(t, inputs.Config.ListenPort, answering("OKAY00040029"))
		}, wantStatus: Fail, wantSummary: "is held by an adb server (version 41)", wantHint: adbServerHint},
		{name: "an occupant with a malformed version", setup: func(t *testing.T, inputs *Inputs, _ *runState) {
			occupyPort(t, inputs.Config.ListenPort, answering("OKAY0004zz29"))
		}, wantStatus: Fail, wantSummary: "address already in use", wantHint: heldPortHint},
		{name: "a silent occupant", setup: func(t *testing.T, inputs *Inputs, _ *runState) {
			occupyPort(t, inputs.Config.ListenPort, silentOccupant)
		}, wantStatus: Fail, wantSummary: "address already in use", wantHint: heldPortHint},
		{name: "an address nobody answers", setup: func(_ *testing.T, inputs *Inputs, _ *runState) {
			inputs.Config.ListenAddr = "203.0.113.1" // TEST-NET-3: not this machine, never routed
		}, wantStatus: Fail, wantSummary: "assign requested address", wantHint: func(port int) string { return listenAddrHint("203.0.113.1", port) }},
		{name: "a privileged port", setup: func(t *testing.T, inputs *Inputs, _ *runState) {
			inputs.Config.ListenPort = 1
			if listener, err := net.Listen("tcp", "127.0.0.1:1"); !errors.Is(err, os.ErrPermission) {
				if err == nil {
					listener.Close()
				}
				t.Skip("this user may bind privileged ports")
			}
		}, wantStatus: Fail, wantSummary: "permission denied", wantHint: lowPortHint},
		{name: "the running daemon on this port", setup: func(t *testing.T, inputs *Inputs, state *runState) {
			occupyPort(t, inputs.Config.ListenPort, silentOccupant) // a bind attempt would fail
			state.daemonStatus, state.daemonPID = control.Response{OK: true, Port: inputs.Config.ListenPort}, 4242
		}, wantStatus: Pass, wantSummary: "is held by the running daemon (pid 4242)"},
		{name: "the running daemon on another port", setup: func(_ *testing.T, inputs *Inputs, state *runState) {
			state.daemonStatus = control.Response{OK: true, Port: inputs.Config.ListenPort + 1, ARN: "arn:1", ReverseEnabled: true}
			state.daemonPID = 4242
		}, wantStatus: Warn, wantSummary: "the running daemon (pid 4242) listens on port", wantHint: func(port int) string {
			return otherPortDaemonHint(control.Response{OK: true, Port: port + 1, ARN: "arn:1", ReverseEnabled: true}, port)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inputs := localInputs(t)
			state := &runState{}
			tc.setup(t, &inputs, state)

			result := runLocal(t, inputs, state, checkListenPort)

			assert.Equal(t, tc.wantStatus, result.Status)
			assert.Contains(t, result.Summary, tc.wantSummary)
			if tc.wantStatus != Warn {
				assert.Contains(t, result.Summary, net.JoinHostPort(inputs.Config.ListenAddr, strconv.Itoa(inputs.Config.ListenPort)))
			}
			if tc.wantHint != nil {
				assert.Equal(t, tc.wantHint(inputs.Config.ListenPort), result.Hint)
			} else {
				assert.Empty(t, result.Hint)
			}
		})
	}
}

func TestGivenAListenAddressWithControlCharactersWhenCheckedThenTheSummaryStaysOnOneLine(t *testing.T) {
	inputs := localInputs(t)
	inputs.Config.ListenAddr = "a\nb"

	result := runLocal(t, inputs, &runState{}, checkListenPort)

	assert.Equal(t, Fail, result.Status)
	assert.Contains(t, result.Summary, `a\nb`, "the address shows escaped")
	assert.NotContains(t, result.Summary, "\n", "a row is one line")
	assert.NotContains(t, result.Hint, "\n")
}

func TestGivenAPortProblemWhenItsHintIsBuiltThenEveryCommandCarriesTheRealPortOnOneLine(t *testing.T) {
	const port = 5555
	reverseDaemon := control.Response{OK: true, Port: 6000, ARN: "arn:1", ReverseEnabled: true}
	tests := []struct {
		name       string
		hint       string
		wantQuoted []string
		wantAbsent string
	}{
		{name: "an adb server", hint: adbServerHint(port),
			wantQuoted: []string{"'adb -P 5555 kill-server'", "'" + bridgeStartCommand + " --port 5555'", otherPortHint}},
		{name: "an invalid listen config on the default address", hint: listenConfigHint(config.DefaultListenAddr),
			wantQuoted: []string{"'--port' a number from 1 to 65535", "'" + bridgeStartCommand + " --port 5037'"}, wantAbsent: "--listen-addr"},
		{name: "an invalid listen config on another address", hint: listenConfigHint("0.0.0.0"), wantQuoted: []string{"'--port' a number from 1 to 65535",
			"'--listen-addr' a non-empty address with no port or brackets", "'" + bridgeStartCommand + " --port 5037 --listen-addr 127.0.0.1'"}},
		{name: "another occupant", hint: heldPortHint(port), wantQuoted: []string{"'" + bridgeStartCommand + " --port 5555'", otherPortHint}},
		{name: "a privileged port", hint: lowPortHint(port), wantQuoted: []string{"'" + bridgeStartCommand + " --port <n>'", "'adb -P <n>'"}},
		{name: "a Windows reserved port", hint: reservedPortHint(port), wantQuoted: []string{"port 5555",
			"'netsh int ipv4 show excludedportrange protocol=tcp'", "'" + bridgeStartCommand + " --port <n>'", "'adb -P <n>'"}, wantAbsent: "--listen-addr"},
		{name: "an address not on this machine", hint: listenAddrHint("203.0.113.1", port),
			wantQuoted: []string{"'203.0.113.1'", "'" + bridgeStartCommand + " --listen-addr 127.0.0.1 --port 5555'"}},
		{name: "an address with control characters", hint: listenAddrHint("203.0.113.1\n[PASS] forged\x1b[0m", port),
			wantQuoted: []string{`'203.0.113.1\n[PASS] forged\x1b[0m'`}, wantAbsent: "\x1b"},
		{name: "the daemon on another port with reverse on", hint: otherPortDaemonHint(reverseDaemon, port),
			wantQuoted: []string{"'" + restartDaemonHint + " --port 5555 --allow-adb-reverse --session-arn arn:1'", "'adb -P 6000'", reverseCaveat}},
		{name: "the daemon on another port with reverse off", hint: otherPortDaemonHint(control.Response{OK: true, Port: 6000}, port),
			wantQuoted: []string{"'" + restartDaemonHint + " --port 5555'", "'adb -P 6000'"}, wantAbsent: reverseCaveat},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, quoted := range tc.wantQuoted {
				assert.Contains(t, tc.hint, quoted)
			}
			if tc.wantAbsent != "" {
				assert.NotContains(t, tc.hint, tc.wantAbsent)
			}
			assert.NotContains(t, tc.hint, "\n", "a row is one line")
		})
	}
}

func TestGivenABindErrorNobodyAnswersWhenTheHintIsPickedThenOnlyAddrNotAvailBlamesTheAddress(t *testing.T) {
	bindError := func(errno syscall.Errno) error {
		return &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", errno)}
	}
	tests := []struct {
		name     string
		err      error
		goos     string
		wantHint string
	}{
		{name: "EADDRNOTAVAIL", err: bindError(syscall.EADDRNOTAVAIL), goos: "linux", wantHint: listenAddrHint("203.0.113.1", 5037)},
		{name: "EADDRINUSE", err: bindError(syscall.EADDRINUSE), goos: "linux", wantHint: heldPortHint(5037)},
		{name: "another error", err: errors.New("boom"), goos: "linux", wantHint: heldPortHint(5037)},
		{name: "WSAEADDRNOTAVAIL on Windows", err: bindError(winsockAddrNotAvail), goos: "windows", wantHint: listenAddrHint("203.0.113.1", 5037)},
		{name: "WSAEACCES on Windows", err: bindError(winsockAccessDenied), goos: "windows", wantHint: reservedPortHint(5037)},
		{name: "WSAEACCES on Linux", err: bindError(winsockAccessDenied), goos: "linux", wantHint: heldPortHint(5037)},
		{name: "WSAEADDRINUSE on Windows", err: bindError(syscall.Errno(10048)), goos: "windows", wantHint: heldPortHint(5037)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantHint, bindErrorHint(tc.err, tc.goos, "203.0.113.1", 5037))
		})
	}
}

func TestGivenTheHostVersionRequestWhenFramedByTheEncoderThenTheConstantMatches(t *testing.T) {
	framed, err := adbwire.EncodeRequest("host:version")
	require.NoError(t, err)
	assert.Equal(t, hostVersionRequest, string(framed))
}

func TestGivenAnUnspecifiedListenAddressWhenTheOccupantIsDialedThenLoopbackIsUsed(t *testing.T) {
	assert.Equal(t, "127.0.0.1:5037", occupantAddress("0.0.0.0", 5037))
	assert.Equal(t, "[::1]:5037", occupantAddress("::", 5037))
	assert.Equal(t, "[::1]:5037", occupantAddress("::1", 5037))
	assert.Equal(t, "localhost:5037", occupantAddress("localhost", 5037))
}

func TestGivenAnInvalidListenConfigWhenCheckedThenItFailsWithTheValidateText(t *testing.T) {
	tests := []struct {
		name               string
		mutate             func(cfg *config.ProxyConfig)
		wantListenAddrFlag bool
	}{
		{name: "port zero on the default address", mutate: func(cfg *config.ProxyConfig) { cfg.ListenPort = 0 }},
		{name: "an address with a port", mutate: func(cfg *config.ProxyConfig) { cfg.ListenAddr = "127.0.0.1:5037" }, wantListenAddrFlag: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inputs := localInputs(t)
			tc.mutate(&inputs.Config)

			result := runLocal(t, inputs, &runState{}, checkListenPort)

			assert.Equal(t, Fail, result.Status)
			assert.Equal(t, inputs.Config.Validate().Error(), result.Summary)
			assert.Contains(t, result.Hint, "'--port' a number from 1 to 65535")
			if tc.wantListenAddrFlag {
				assert.Contains(t, result.Hint, "'--listen-addr' a non-empty address")
			} else {
				assert.NotContains(t, result.Hint, "--listen-addr")
			}
		})
	}
}

func TestGivenAForwardTableWhenCheckedThenItBindsReachesAndFreesALoopbackPort(t *testing.T) {
	result := runLocal(t, localInputs(t), &runState{}, checkForwardTable)

	require.Equal(t, Pass, result.Status, result.Summary)
	var port int
	_, err := fmt.Sscanf(result.Summary, "adb forward can listen on 127.0.0.1 (tried port %d)", &port)
	require.NoError(t, err)
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	require.NoError(t, err, "the table must free its port when the check ends")
	listener.Close()
}

func TestGivenACancelledContextWhenTheForwardTableIsCheckedThenTheDialFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := checkByID(t, localChecks(localInputs(t), &runState{}), checkForwardTable).Run(ctx)

	assert.Equal(t, Fail, result.Status)
	assert.Contains(t, result.Summary, "dial tcp 127.0.0.1:")
	assert.Equal(t, loopbackHint, result.Hint)
}

func TestGivenTheClosingOpenerWhenAConnectionArrivesThenItIsClosedAtOnce(t *testing.T) {
	table := forward.NewForwardRulesTable(closingOpener{}, 1, slog.New(slog.DiscardHandler))
	installed := table.Install("tcp:0", "tcp:0", false)
	require.Equal(t, forward.AdbInstallOK, installed.Status, installed.FailReason)
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(installed.ResolvedPort))
	conn, err := net.DialTimeout("tcp", address, time.Second)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))

	_, err = conn.Read(make([]byte, 1))

	assert.ErrorIs(t, err, io.EOF, "the opener must close the accepted connection")
	table.Close()
	listener, err := net.Listen("tcp", address)
	require.NoError(t, err, "Close must free the port")
	listener.Close()
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
	assert.Equal(t, []string{checkRuntimeDir, checkDaemon, checkListenPort, checkForwardTable}, ids)
}
