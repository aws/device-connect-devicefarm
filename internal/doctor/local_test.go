// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package doctor

import (
	"cmp"
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
	"github.com/aws/device-connect-devicefarm/internal/session"
	"github.com/aws/device-connect-devicefarm/internal/wording"
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
		switch request, ok := readOccupantRequest(conn); {
		case !ok:
		case request == "000chost:version":
			_, _ = conn.Write([]byte(reply))
		default:
			_, _ = fmt.Fprintf(conn, "FAIL%04x%s", len("unknown host service"), "unknown host service")
		}
	}
}

// answeringVersionOnly answers host:version with reply and closes any other request unanswered.
func answeringVersionOnly(reply string) func(net.Conn) {
	return func(conn net.Conn) {
		defer conn.Close()
		if request, ok := readOccupantRequest(conn); ok && request == "000chost:version" {
			_, _ = conn.Write([]byte(reply))
		}
	}
}

// answeringAsABridge plays another bridge. The identity request gets pid. host:version gets a
// version reply (41 here): a bridge repeats the version its remote adb server reported.
func answeringAsABridge(pid int) func(net.Conn) {
	return func(conn net.Conn) {
		defer conn.Close()
		switch request, ok := readOccupantRequest(conn); {
		case !ok:
		case request == "000chost:version":
			_, _ = conn.Write([]byte("OKAY00040029"))
		case request == adbwire.BridgeIdentityRequest:
			_ = adbwire.WriteBridgeIdentity(conn, pid)
		}
	}
}

// readOccupantRequest reads one framed request, its length prefix kept.
func readOccupantRequest(conn net.Conn) (string, bool) {
	digits := make([]byte, 4)
	if _, err := io.ReadFull(conn, digits); err != nil {
		return "", false
	}
	length, err := strconv.ParseUint(string(digits), 16, 16)
	if err != nil {
		return "", false
	}
	service := make([]byte, length)
	if _, err := io.ReadFull(conn, service); err != nil {
		return "", false
	}
	return string(digits) + string(service), true
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
			assert.Equal(t, fmt.Sprintf(wording.DoctorHintDirectoryUnusableFormat, daemon.RuntimeDirEnvVar), result.Hint)
			assert.Contains(t, result.Hint, "'"+daemon.RuntimeDirEnvVar+"'", "the hint quotes the variable")
			assert.Empty(t, state.paths.RuntimeDir, "a failed row leaves no paths for later checks")
		})
	}
}

func TestGivenASocketPathOverTheLimitWhenCheckedThenTheSummaryCountsTheBytesOver(t *testing.T) {
	tests := []struct {
		name      string
		bytesOver int
		wantTail  string
	}{
		{name: "one byte over", bytesOver: 1, wantTail: " is 1 byte too long for this system"},
		{name: "two bytes over", bytesOver: 2, wantTail: " is 2 bytes too long for this system"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Padding directory level puts the socket path bytesOver past the limit.
			shortPaths := shortRuntimeDir(t)
			padding := strings.Repeat("x", maxControlSocketPath+tc.bytesOver-len(shortPaths.ControlSocket)-len("/"))
			runtimeBase := filepath.Join(filepath.Dir(shortPaths.RuntimeDir), padding)
			t.Setenv("XDG_RUNTIME_DIR", runtimeBase)

			result := runLocal(t, localInputs(t), &runState{}, checkRuntimeDir)

			assert.Equal(t, Fail, result.Status)
			assert.Equal(t, "working directory "+filepath.Join(runtimeBase, config.BinaryName)+tc.wantTail, result.Summary)
		})
	}
}

func TestGivenNoDaemonWhenCheckedThenItPassesAsNotRunning(t *testing.T) {
	state := &runState{paths: shortRuntimeDir(t)}

	result := runLocal(t, localInputs(t), state, checkDaemon)

	assert.Equal(t, Result{Status: Pass, Summary: "not running; bridge start, device add, or device list launches it"}, result)
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
			wantRestart: wording.DoctorRestartCommand + " --port 5037 --allow-adb-reverse --session-arn arn:2"},
		{name: "reverse off with no device", reply: control.Response{OK: true, Port: 6000},
			wantStatus: Warn, wantTail: "on port 6000; no device; reverse off", wantRestart: wording.DoctorRestartCommand + " --port 6000 --allow-adb-reverse"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := &runState{paths: fakeDaemon(t, tc.reply)}

			result := runLocal(t, localInputs(t), state, checkDaemon)

			assert.Equal(t, tc.wantStatus, result.Status)
			assert.Equal(t, fmt.Sprintf("running (PID %d) %s", os.Getpid(), tc.wantTail), result.Summary)
			if tc.wantStatus == Warn {
				assert.Contains(t, result.Hint, "'"+tc.wantRestart+"'", "the hint quotes the restart with the reported port and session")
				assert.Contains(t, result.Hint, wording.DoctorHintReverseCaveat)
			} else {
				assert.Empty(t, result.Hint)
			}
			reply, pid := state.daemon()
			assert.Equal(t, tc.reply, reply)
			assert.Equal(t, os.Getpid(), pid)
		})
	}
}

const (
	otherSessionARN = "arn:aws:devicefarm:us-west-2:123456789012:session:abc/def/11111"
	// returnedSessionARN is the ARN the service returns for testSessionARN when the two differ.
	returnedSessionARN = "arn:aws:devicefarm:us-west-2:111122223333:session:abc/def/00000"
	// endedReason is the bridge's detach reason for an ended session.
	endedReason = "remote access session " + testSessionARN + " has ended (COMPLETED); " +
		"start a new remote access session, then run 'devicefarm-connect device add --session-arn <new ARN>'"
)

// attachedReply is a status reply holding one device on sessionARN.
func attachedReply(sessionARN string, reverse bool) control.Response {
	return control.Response{OK: true, Port: 5037, DeviceLabel: "Pixel 9 (OS 16)", ARN: sessionARN, ReverseEnabled: reverse}
}

func TestGivenADetachReasonWhenTheBridgeIsCheckedThenTheRowWarnsAndTheHintQuotesTheReason(t *testing.T) {
	tests := []struct {
		name     string
		reverse  bool
		askedARN string
		wantTail string
	}{
		{name: "reverse on", reverse: true, wantTail: "no device; reverse on; the bridge detached the last device"},
		{name: "reverse off", wantTail: "no device; reverse off; the bridge detached the last device"},
		{name: "an asked session", reverse: true, askedARN: testSessionARN,
			wantTail: "no device; reverse on; the bridge detached the last device; session " + testSessionARN + " is not attached"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := &runState{paths: fakeDaemon(t, control.Response{OK: true, Port: 5037, ReverseEnabled: tc.reverse,
				LastDetachReason: endedReason})}
			inputs := localInputs(t)
			inputs.SessionARN = tc.askedARN

			result := runLocal(t, inputs, state, checkDaemon)

			assert.Equal(t, Result{Status: Warn, Summary: fmt.Sprintf("running (PID %d) on port 5037; %s", os.Getpid(), tc.wantTail),
				Hint: "the bridge detached the device, so adb finds no device: " + endedReason + "."}, result,
				"the detach outranks the asked session and reverse off")
		})
	}
}

func TestGivenAnAskedSessionTheBridgeDoesNotHoldWhenCheckedThenTheRowWarnsWithTheAddCommand(t *testing.T) {
	notAttached := "session " + testSessionARN + " is not attached to the bridge, so adb cannot reach its device. "
	add := "'devicefarm-connect device add --session-arn " + testSessionARN + "'"
	tests := []struct {
		name     string
		reply    control.Response
		wantTail string
		wantHint string
	}{
		{name: "no device", reply: control.Response{OK: true, Port: 5037, ReverseEnabled: true},
			wantTail: "no device; reverse on", wantHint: notAttached + "Run " + add + "."},
		{name: "no device and reverse off", reply: control.Response{OK: true, Port: 5037},
			wantTail: "no device; reverse off", wantHint: notAttached + "Run " + add + "."},
		{name: "another device attached", reply: attachedReply(otherSessionARN, true),
			wantTail: "device Pixel 9 (OS 16), session " + otherSessionARN + "; reverse on",
			wantHint: notAttached + "Run 'devicefarm-connect device remove', then " + add + ". The remove detaches Pixel 9 (OS 16)."},
		{name: "a detach reason about another session",
			reply: control.Response{OK: true, Port: 5037, ReverseEnabled: true,
				LastDetachReason: strings.ReplaceAll(endedReason, testSessionARN, otherSessionARN)},
			wantTail: "no device; reverse on; the bridge detached the last device", wantHint: notAttached + "Run " + add + "."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := bridgeRowResult(testSessionARN, testSessionARN, tc.reply, 4242)

			assert.Equal(t, Result{Status: Warn, Hint: tc.wantHint,
				Summary: "running (PID 4242) on port 5037; " + tc.wantTail + "; session " + testSessionARN + " is not attached"}, result,
				"the asked session outranks reverse off")
		})
	}
}

func TestGivenTheAskedSessionAttachedWhenCheckedThenTheRowKeepsItsReverseVerdict(t *testing.T) {
	tests := []struct {
		name       string
		askedARN   string
		reverse    bool
		wantStatus Status
		wantHint   string
	}{
		{name: "reverse on", askedARN: testSessionARN, reverse: true, wantStatus: Pass},
		{name: "reverse off", askedARN: testSessionARN, wantStatus: Warn, wantHint: reverseOffHint(attachedReply(testSessionARN, false))},
		{name: "spaces around the asked session", askedARN: " " + testSessionARN + " ", reverse: true, wantStatus: Pass},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := &runState{paths: fakeDaemon(t, attachedReply(testSessionARN, tc.reverse))}
			inputs := localInputs(t)
			inputs.SessionARN = tc.askedARN

			result := runLocal(t, inputs, state, checkDaemon)

			assert.Equal(t, tc.wantStatus, result.Status, result.Summary)
			assert.NotContains(t, result.Summary, "is not attached")
			assert.Equal(t, tc.wantHint, result.Hint)
		})
	}
}

func TestGivenTheServiceReturnsAnotherArnForTheAskedSessionWhenTheBridgeIsCheckedThenThatArnNamesTheAskedSession(t *testing.T) {
	// The session still runs: the bridge lost its credentials, this shell has its own.
	expiredReason := "the AWS credentials the bridge started with have expired, so it can no longer read remote access session " +
		returnedSessionARN + "; refresh the credentials, run 'devicefarm-connect bridge stop', start the bridge again from that " +
		"shell with the same options, then run 'devicefarm-connect device add --session-arn " + returnedSessionARN + "'"
	notAttached := "; session " + testSessionARN + " is not attached"
	tests := []struct {
		name        string
		reply       control.Response
		wantStatus  Status
		wantSummary string
		wantHint    string
	}{
		{name: "attached under the returned ARN", reply: attachedReply(returnedSessionARN, true), wantStatus: Pass,
			wantSummary: "device Pixel 9 (OS 16), session " + returnedSessionARN + "; reverse on"},
		{name: "attached under the returned ARN with reverse off", reply: attachedReply(returnedSessionARN, false), wantStatus: Warn,
			wantSummary: "device Pixel 9 (OS 16), session " + returnedSessionARN + "; reverse off",
			wantHint:    reverseOffHint(attachedReply(returnedSessionARN, false))},
		{name: "detached under the returned ARN",
			reply:      control.Response{OK: true, Port: 5037, ReverseEnabled: true, LastDetachReason: expiredReason},
			wantStatus: Warn, wantSummary: "no device; reverse on; the bridge detached the last device" + notAttached,
			wantHint: "the bridge detached the device, so adb finds no device: " + expiredReason + "."},
		{name: "another session attached", reply: attachedReply(otherSessionARN, true), wantStatus: Warn,
			wantSummary: "device Pixel 9 (OS 16), session " + otherSessionARN + "; reverse on" + notAttached,
			wantHint: "session " + testSessionARN + " is not attached to the bridge, so adb cannot reach its device. " +
				"Run 'devicefarm-connect device remove', then 'devicefarm-connect device add --session-arn " + testSessionARN +
				"'. The remove detaches Pixel 9 (OS 16)."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := &runState{paths: fakeDaemon(t, tc.reply)}
			resolver := &fakeResolver{info: session.RemoteAccessInfo{ARN: returnedSessionARN}}
			inputs := localInputs(t)
			inputs.SessionARN, inputs.Resolver = testSessionARN, resolver

			result := runLocal(t, inputs, state, checkDaemon)

			assert.Equal(t, Result{Status: tc.wantStatus, Hint: tc.wantHint,
				Summary: fmt.Sprintf("running (PID %d) on port 5037; %s", os.Getpid(), tc.wantSummary)}, result)
			assert.Equal(t, []string{testSessionARN}, resolver.arns, "one lookup, of the typed ARN")
		})
	}
}

func TestGivenNoReturnedArnForTheAskedSessionWhenTheBridgeIsCheckedThenTheTypedArnIsCompared(t *testing.T) {
	attached, other := attachedReply(returnedSessionARN, true), attachedReply(otherSessionARN, true)
	// An ended session gives no answer, so its detach is not taken as the asked session's.
	ended := control.Response{OK: true, Port: 5037, ReverseEnabled: true,
		LastDetachReason: strings.ReplaceAll(endedReason, testSessionARN, returnedSessionARN)}
	tests := []struct {
		name     string
		reply    control.Response
		resolver *fakeResolver
		wantTail string
	}{
		{name: "the lookup fails", reply: attached, resolver: &fakeResolver{err: errors.New("no usable AWS credentials")},
			wantTail: "device Pixel 9 (OS 16), session " + returnedSessionARN + "; reverse on"},
		{name: "the answer names no ARN", reply: other, resolver: &fakeResolver{},
			wantTail: "device Pixel 9 (OS 16), session " + otherSessionARN + "; reverse on"},
		{name: "the session has ended", reply: ended,
			resolver: &fakeResolver{err: &session.SessionEndedError{SessionARN: testSessionARN, Status: "COMPLETED"}},
			wantTail: "no device; reverse on; the bridge detached the last device"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := &runState{paths: fakeDaemon(t, tc.reply)}
			inputs := localInputs(t)
			inputs.SessionARN, inputs.Resolver = testSessionARN, tc.resolver

			result := runLocal(t, inputs, state, checkDaemon)

			assert.Equal(t, Result{Status: Warn, Hint: notAttachedHint(tc.reply, testSessionARN),
				Summary: fmt.Sprintf("running (PID %d) on port 5037; %s; session %s is not attached", os.Getpid(), tc.wantTail, testSessionARN)},
				result)
			assert.Equal(t, []string{testSessionARN}, tc.resolver.arns, "the lookup was tried")
		})
	}
}

func TestGivenTheTypedArnNamesWhatTheBridgeHoldsOrItHoldsNothingWhenCheckedThenNoLookupIsMade(t *testing.T) {
	tests := []struct {
		name     string
		askedARN string
		reply    control.Response
	}{
		{name: "no asked session", reply: attachedReply(testSessionARN, true)},
		{name: "the asked session attached", askedARN: testSessionARN, reply: attachedReply(testSessionARN, true)},
		{name: "a detach reason naming the asked session", askedARN: testSessionARN,
			reply: control.Response{OK: true, Port: 5037, LastDetachReason: endedReason}},
		{name: "no device and no detach reason", askedARN: testSessionARN, reply: control.Response{OK: true, Port: 5037}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := &runState{paths: fakeDaemon(t, tc.reply)}
			resolver := &fakeResolver{}
			inputs := localInputs(t)
			inputs.SessionARN, inputs.Resolver = tc.askedARN, resolver

			result := runLocal(t, inputs, state, checkDaemon)

			assert.Contains(t, result.Summary, "running (PID ", "the row reached the bridge")
			assert.Empty(t, resolver.arns, "the row must not call the service")
		})
	}
}

func TestGivenTooLittleOfItsTimeLeftForTheLookupWhenTheBridgeIsCheckedThenTheTypedArnIsComparedAndNoLookupIsMade(t *testing.T) {
	reply := attachedReply(returnedSessionARN, true)
	state := &runState{paths: fakeDaemon(t, reply)}
	resolver := &fakeResolver{info: session.RemoteAccessInfo{ARN: returnedSessionARN}}
	inputs := localInputs(t)
	inputs.SessionARN, inputs.Resolver = testSessionARN, resolver
	ctx, cancel := context.WithTimeout(context.Background(), remoteTimeout)
	defer cancel()

	result := checkByID(t, localChecks(inputs, state), checkDaemon).Run(ctx)

	assert.Equal(t, Warn, result.Status, result.Summary)
	assert.Equal(t, notAttachedHint(reply, testSessionARN), result.Hint)
	assert.Empty(t, resolver.arns, "the session row makes the lookup, with its whole bound")
}

func TestGivenAReasonOrAnAskedSessionWithAControlCharacterWhenRenderedThenTheRowStaysOnOneLine(t *testing.T) {
	forged := "gone\n[PASS] forged\x1b[0m"
	detached := bridgeRowResult(forged, forged, control.Response{OK: true, Port: 5037, LastDetachReason: forged}, 4242)
	notAttached := bridgeRowResult(forged, forged, control.Response{OK: true, Port: 5037, DeviceLabel: forged, ARN: otherSessionARN}, 4242)

	for name, text := range map[string]string{"detach summary": detached.Summary, "detach hint": detached.Hint,
		"add summary": notAttached.Summary, "add hint": notAttached.Hint} {
		assert.NotContains(t, text, "\n", name)
		assert.NotContains(t, text, "\x1b", name)
		assert.Contains(t, text, `gone\n[PASS] forged\x1b[0m`, name)
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
			wantSummary: fmt.Sprintf("a bridge (PID %d) is registered but does not answer: connect: ", os.Getpid())},
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
			assert.Equal(t, wording.DoctorHintDaemonNoStatus, result.Hint)
			assert.Contains(t, result.Hint, "'"+wording.DoctorRestartCommand+"'", "the hint quotes the restart")
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
		wantHint    func(listenAddr string, port int) string
		// summaryNamesNoAddress: the summary names bare ports, no host:port.
		summaryNamesNoAddress bool
	}{
		{name: "free", setup: func(*testing.T, *Inputs, *runState) {}, wantStatus: Pass, wantSummary: "is free"},
		{name: "an adb server", setup: func(t *testing.T, inputs *Inputs, _ *runState) {
			occupyPort(t, inputs.Config.ListenPort, answering("OKAY00040029"))
		}, wantStatus: Warn, wantSummary: "an adb server (version 41) is listening on", wantHint: adbServerHint},
		{name: "another bridge", setup: func(t *testing.T, inputs *Inputs, _ *runState) {
			// A bridge answers host:version too; the identity request tells it from an adb server.
			occupyPort(t, inputs.Config.ListenPort, answeringAsABridge(4242))
		}, wantStatus: Fail, wantSummary: "another devicefarm-connect bridge (PID 4242) is listening on",
			wantHint: func(listenAddr string, port int) string { return daemon.BridgeHoldsPortText(listenAddr, port, 4242) }},
		{name: "an occupant silent to the identity request", setup: func(t *testing.T, inputs *Inputs, _ *runState) {
			// It may be a bridge cut mid-answer, so its version reply is not taken for an adb server.
			occupyPort(t, inputs.Config.ListenPort, answeringVersionOnly("OKAY00040029"))
		}, wantStatus: Fail, wantSummary: "because another program is listening on it", wantHint: daemon.HeldPortText},
		{name: "an occupant with a malformed version", setup: func(t *testing.T, inputs *Inputs, _ *runState) {
			occupyPort(t, inputs.Config.ListenPort, answering("OKAY0004zz29"))
		}, wantStatus: Fail, wantSummary: "because another program is listening on it", wantHint: daemon.HeldPortText},
		{name: "a silent occupant", setup: func(t *testing.T, inputs *Inputs, _ *runState) {
			occupyPort(t, inputs.Config.ListenPort, silentOccupant)
		}, wantStatus: Fail, wantSummary: "because another program is listening on it", wantHint: daemon.HeldPortText},
		{name: "an address nobody answers", setup: func(_ *testing.T, inputs *Inputs, _ *runState) {
			inputs.Config.ListenAddr = "203.0.113.1" // TEST-NET-3: not this machine, never routed
		}, wantStatus: Fail, wantSummary: "assign requested address", wantHint: func(_ string, port int) string { return listenAddrHint("203.0.113.1", port) }},
		{name: "a privileged port", setup: func(t *testing.T, inputs *Inputs, _ *runState) {
			inputs.Config.ListenPort = 1
			if listener, err := net.Listen("tcp", "127.0.0.1:1"); !errors.Is(err, os.ErrPermission) {
				if err == nil {
					listener.Close()
				}
				t.Skip("this user may bind privileged ports")
			}
		}, wantStatus: Fail, wantSummary: "permission denied", wantHint: func(_ string, port int) string { return lowPortHint(port) }},
		{name: "the running daemon on this port", setup: func(t *testing.T, inputs *Inputs, state *runState) {
			occupyPort(t, inputs.Config.ListenPort, silentOccupant) // a bind attempt would fail
			state.daemonStatus, state.daemonPID = control.Response{OK: true, Port: inputs.Config.ListenPort}, 4242
		}, wantStatus: Pass, wantSummary: "is held by the running bridge (PID 4242)"},
		{name: "the running daemon on another port", setup: func(_ *testing.T, inputs *Inputs, state *runState) {
			state.daemonStatus = control.Response{OK: true, Port: inputs.Config.ListenPort + 1, ARN: "arn:1", ReverseEnabled: true}
			state.daemonPID = 4242
		}, wantStatus: Warn, wantSummary: "the running bridge (PID 4242) listens on port", wantHint: func(_ string, port int) string {
			return otherPortDaemonHint(control.Response{OK: true, Port: port + 1, ARN: "arn:1", ReverseEnabled: true}, port)
		}, summaryNamesNoAddress: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inputs := localInputs(t)
			state := &runState{}
			tc.setup(t, &inputs, state)

			result := runLocal(t, inputs, state, checkListenPort)

			assert.Equal(t, tc.wantStatus, result.Status)
			assert.Contains(t, result.Summary, tc.wantSummary)
			if !tc.summaryNamesNoAddress {
				assert.Contains(t, result.Summary, net.JoinHostPort(inputs.Config.ListenAddr, strconv.Itoa(inputs.Config.ListenPort)))
			}
			if tc.wantHint != nil {
				assert.Equal(t, tc.wantHint(inputs.Config.ListenAddr, inputs.Config.ListenPort), result.Hint)
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

func TestGivenAnAdbServerOnTheDefaultPortWhenItsHintIsBuiltThenTheFixQuitsTheProgramThatRestartsAdb(t *testing.T) {
	assert.Equal(t, "an adb server is listening on 127.0.0.1:5037, so the bridge cannot bind it until that server stops. Run "+
		"'devicefarm-connect bridge start --port 5037': it stops that server first, as 'adb kill-server' would. If a program that "+
		"restarts adb, such as Android Studio, takes the port back, quit that program, run 'devicefarm-connect bridge start --port 5037' "+
		"again, then open it: it then uses the bridge as its adb server on port 5037.",
		adbServerHint(config.DefaultListenAddr, config.DefaultListenPort))
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
		{name: "an adb server", hint: adbServerHint("127.0.0.1", port), wantQuoted: []string{"127.0.0.1:5555",
			"'" + wording.BridgeStartCommand + " --port 5555':", "'adb kill-server'", "'" + wording.BridgeStartCommand + " --port <n>'", "'adb -P <n>'"},
			wantAbsent: "--listen-addr"},
		{name: "an adb server on another address", hint: adbServerHint("127.0.0.2", port), wantQuoted: []string{"127.0.0.2:5555",
			"'" + wording.BridgeStartCommand + " --port 5555 --listen-addr 127.0.0.2':"}},
		{name: "an invalid listen config on the default address", hint: listenConfigHint(config.DefaultListenAddr),
			wantQuoted: []string{"'--port' a number from 1 to 65535", "'" + wording.BridgeStartCommand + " --port 5037'"}, wantAbsent: "--listen-addr"},
		{name: "an invalid listen config on another address", hint: listenConfigHint("0.0.0.0"), wantQuoted: []string{"'--port' a number from 1 to 65535",
			"'--listen-addr' a non-empty address with no port or brackets", "'" + wording.BridgeStartCommand + " --port 5037 --listen-addr 127.0.0.1'"}},
		{name: "another occupant", hint: daemon.HeldPortText("127.0.0.1", port), wantQuoted: []string{"127.0.0.1:5555",
			"'" + wording.BridgeStartCommand + " --port 5555'", "'" + wording.BridgeStartCommand + " --port <n>'", "'adb -P <n>'"}, wantAbsent: "--listen-addr"},
		{name: "a privileged port", hint: lowPortHint(port), wantQuoted: []string{"'" + wording.BridgeStartCommand + " --port <n>'", "'adb -P <n>'"}},
		{name: "a Windows reserved port", hint: reservedPortHint(port), wantQuoted: []string{"port 5555",
			"'netsh int ipv4 show excludedportrange protocol=tcp'", "'" + wording.BridgeStartCommand + " --port <n>'", "'adb -P <n>'"}, wantAbsent: "--listen-addr"},
		{name: "an address not on this machine", hint: listenAddrHint("203.0.113.1", port),
			wantQuoted: []string{"'203.0.113.1'", "'" + wording.BridgeStartCommand + " --listen-addr 127.0.0.1 --port 5555'"}},
		{name: "an address with control characters", hint: listenAddrHint("203.0.113.1\n[PASS] forged\x1b[0m", port),
			wantQuoted: []string{`'203.0.113.1\n[PASS] forged\x1b[0m'`}, wantAbsent: "\x1b"},
		{name: "the daemon on another port with reverse on", hint: otherPortDaemonHint(reverseDaemon, port),
			wantQuoted: []string{"'" + wording.DoctorRestartCommand + " --port 5555 --allow-adb-reverse --session-arn arn:1'", "'adb -P 6000'", wording.DoctorHintReverseCaveat}},
		{name: "the daemon on another port with reverse off", hint: otherPortDaemonHint(control.Response{OK: true, Port: 6000}, port),
			wantQuoted: []string{"'" + wording.DoctorRestartCommand + " --port 5555'", "'adb -P 6000'"}, wantAbsent: wording.DoctorHintReverseCaveat},
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

func TestGivenABindErrorNobodyAnswersWhenTheHintIsPickedThenOnlyAnInUseErrorBlamesAnotherProgram(t *testing.T) {
	bindError := func(errno syscall.Errno) error {
		return &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", errno)}
	}
	const heldSummary = "cannot bind 203.0.113.1:5037 because another program is listening on it"
	heldHint := daemon.HeldPortText("203.0.113.1", 5037)
	tests := []struct {
		name        string
		err         error
		goos        string
		wantSummary string
		wantHint    string
	}{
		{name: "EADDRNOTAVAIL", err: bindError(syscall.EADDRNOTAVAIL), goos: "linux", wantHint: listenAddrHint("203.0.113.1", 5037)},
		{name: "EADDRINUSE", err: bindError(syscall.EADDRINUSE), goos: "linux", wantSummary: heldSummary, wantHint: heldHint},
		{name: "a name that does not resolve", err: &net.DNSError{Err: "no such host", Name: "devbox.invalid", IsNotFound: true},
			goos: "linux", wantHint: listenAddrHint("203.0.113.1", 5037)},
		{name: "another error", err: errors.New("boom"), goos: "linux", wantHint: listenAddrHint("203.0.113.1", 5037)},
		{name: "WSAEADDRNOTAVAIL on Windows", err: bindError(syscall.Errno(10049)), goos: "windows", wantHint: listenAddrHint("203.0.113.1", 5037)},
		{name: "WSAEACCES on Windows", err: bindError(winsockAccessDenied), goos: "windows", wantHint: reservedPortHint(5037)},
		{name: "WSAEACCES on Linux", err: bindError(winsockAccessDenied), goos: "linux", wantHint: listenAddrHint("203.0.113.1", 5037)},
		{name: "WSAEADDRINUSE on Windows", err: bindError(syscall.Errno(10048)), goos: "windows", wantSummary: heldSummary, wantHint: heldHint},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			summary, hint := bindErrorHint(tc.err, tc.goos, "203.0.113.1", 5037)

			// Only the held-port arm replaces the bind error as the summary.
			assert.Equal(t, cmp.Or(tc.wantSummary, tc.err.Error()), summary)
			assert.Equal(t, tc.wantHint, hint)
		})
	}
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
	assert.Equal(t, wording.DoctorHintForwardBlocked, result.Hint)
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

func TestGivenNoSessionArnWhenTheTableRunsThenNothingFailsEvenWithAnAdbServerOnTheListenPort(t *testing.T) {
	tests := []struct {
		name           string
		occupyPortWith func(net.Conn)
		wantListenPort Status
	}{
		{name: "a free listen port", wantListenPort: Pass},
		{name: "an adb server on the listen port", occupyPortWith: answering("OKAY00040029"), wantListenPort: Warn},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			shortRuntimeDir(t)
			inputs := localInputs(t)
			inputs.Logger = nil
			inputs.Credentials = func(context.Context) (session.CredentialSummary, error) {
				return session.CredentialSummary{Source: "StaticCredentials"}, nil
			}
			if tc.occupyPortWith != nil {
				occupyPort(t, inputs.Config.ListenPort, tc.occupyPortWith)
			}

			report := Run(context.Background(), Checks(inputs))

			ids := make([]string, 0, len(report.Rows))
			statuses := make([]Status, 0, len(report.Rows))
			for _, row := range report.Rows {
				ids = append(ids, row.ID)
				statuses = append(statuses, row.Status)
			}
			assert.Equal(t, []string{checkRuntimeDir, checkDaemon, checkListenPort, checkForwardTable, checkCredentials, checkSession,
				checkRelaySession, checkReattach, checkReverse, checkAdbVersion}, ids)
			assert.Equal(t, []Status{Pass, Pass, tc.wantListenPort, Pass, Pass, Skip, Skip, Skip, Skip, Skip}, statuses, "%v", report.Rows)
			assert.Equal(t, 0, report.Failed(), "exit code 0")
		})
	}
}
