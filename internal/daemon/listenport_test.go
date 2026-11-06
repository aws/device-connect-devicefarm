package daemon

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/adbwire"
	"github.com/aws/device-connect-devicefarm/internal/config"
)

// testStopWithin bounds each read of a stop in the StopAdbServer tests.
const testStopWithin = 200 * time.Millisecond

// adbRejectsKill is the FAIL reason from an adb server started to reject kills.
const adbRejectsKill = "kill-server rejected by remote server"

// occupyPort binds 127.0.0.1:port and serves every accepted connection with serve until cleanup.
func occupyPort(t *testing.T, port int, serve func(net.Conn)) net.Listener {
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
	return listener
}

// silentOccupant holds the connection open and never writes.
func silentOccupant(conn net.Conn) {
	defer conn.Close()
	_, _ = io.Copy(io.Discard, conn)
}

// killAnswer is how a fake adb server answers host:kill.
type killAnswer int

const (
	killExits         killAnswer = iota // OKAY, then its exit closes the connection and the listener
	killRestarted                       // OKAY and a close, but a restarted server holds the port
	killStaysUp                         // OKAY, then no close: the port stays held
	killRefused                         // FAIL with adb's reason for a server that rejects kills
	killMalformedFail                   // FAIL with a length prefix that is not hex
	killUnanswered                      // nothing
	killGarbled                         // four bytes that are neither OKAY nor FAIL
)

// fakeAdbServer plays an adb server on a loopback port: host:version gets adb 1.0.41's reply,
// host:kill its answer.
type fakeAdbServer struct {
	listener net.Listener
	answer   killAnswer
	// killFrames receives the bytes of each host:kill request.
	killFrames chan string
	// onVersion, when set, runs before each host:version reply.
	onVersion func()
}

func startFakeAdbServer(t *testing.T, port int, answer killAnswer) *fakeAdbServer {
	t.Helper()
	server := &fakeAdbServer{answer: answer, killFrames: make(chan string, 8)}
	server.listener = occupyPort(t, port, server.serve)
	return server
}

// serve answers one framed request on conn.
func (server *fakeAdbServer) serve(conn net.Conn) {
	defer conn.Close()
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return
	}
	length, err := strconv.ParseUint(string(header), 16, 16)
	if err != nil {
		return
	}
	service := make([]byte, length)
	if _, err := io.ReadFull(conn, service); err != nil {
		return
	}
	switch string(service) {
	case "host:version":
		if server.onVersion != nil {
			server.onVersion()
		}
		_, _ = conn.Write([]byte("OKAY00040029"))
	case "host:kill":
		server.killFrames <- string(header) + string(service)
		server.answerKill(conn)
	}
}

func (server *fakeAdbServer) answerKill(conn net.Conn) {
	switch server.answer {
	case killExits:
		_, _ = conn.Write([]byte("OKAY"))
		server.listener.Close()
	case killRestarted:
		_, _ = conn.Write([]byte("OKAY"))
	case killStaysUp:
		_, _ = conn.Write([]byte("OKAY"))
		_, _ = io.Copy(io.Discard, conn)
	case killRefused:
		_, _ = fmt.Fprintf(conn, "FAIL%04x%s", len(adbRejectsKill), adbRejectsKill)
	case killMalformedFail:
		_, _ = conn.Write([]byte("FAILzzzz"))
	case killUnanswered:
		_, _ = io.Copy(io.Discard, conn)
	case killGarbled:
		_, _ = conn.Write([]byte("WHAT"))
	}
}

// requireBindable fails the test unless 127.0.0.1:port binds.
func requireBindable(t *testing.T, port int) {
	t.Helper()
	require.NoError(t, bindListenPort("127.0.0.1", port), "the port must be free")
}

func TestGivenAnUnspecifiedListenAddressWhenTheOccupantIsDialedThenLoopbackIsUsed(t *testing.T) {
	assert.Equal(t, "127.0.0.1:5037", OccupantAddress("0.0.0.0", 5037))
	assert.Equal(t, "[::1]:5037", OccupantAddress("::", 5037))
	assert.Equal(t, "[::1]:5037", OccupantAddress("::1", 5037))
	assert.Equal(t, "localhost:5037", OccupantAddress("localhost", 5037))
}

func TestGivenAnAdbServerOnThePortWhenProbedThenItsVersionIsRead(t *testing.T) {
	port := freePort(t)
	server := startFakeAdbServer(t, port, killExits)

	version, connected, adbServer := ProbeOccupant(OccupantAddress("127.0.0.1", port), time.Second)

	assert.Equal(t, 41, version)
	assert.True(t, connected)
	assert.True(t, adbServer)
	assert.Empty(t, server.killFrames, "a probe never asks the server to stop")
}

func TestGivenASilentOrMalformedOccupantWhenProbedThenItIsNotAnAdbServer(t *testing.T) {
	tests := []struct {
		name          string
		serve         func(net.Conn)
		wantConnected bool
	}{
		{name: "a silent occupant", serve: silentOccupant, wantConnected: true},
		{name: "a malformed version", serve: func(conn net.Conn) {
			defer conn.Close()
			_, _ = conn.Write([]byte("OKAY0004zz29"))
		}, wantConnected: true},
		{name: "a FAIL", serve: func(conn net.Conn) {
			defer conn.Close()
			_, _ = conn.Write([]byte("FAIL0004nope"))
		}, wantConnected: true},
		{name: "nobody"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			port := freePort(t)
			if tc.serve != nil {
				occupyPort(t, port, tc.serve)
			}

			_, connected, adbServer := ProbeOccupant(OccupantAddress("127.0.0.1", port), 200*time.Millisecond)

			assert.Equal(t, tc.wantConnected, connected)
			assert.False(t, adbServer)
		})
	}
}

func TestGivenAnAdbServerWhenStoppedThenItReceivesHostKillAsEncodeRequestFramesIt(t *testing.T) {
	encoded, err := adbwire.EncodeRequest("host:kill")
	require.NoError(t, err)
	port := freePort(t)
	server := startFakeAdbServer(t, port, killExits)

	require.NoError(t, StopAdbServer("127.0.0.1", port, testStopWithin))

	require.Len(t, server.killFrames, 1)
	assert.Equal(t, string(encoded), <-server.killFrames)
	requireBindable(t, port)
}

func TestGivenAnAdbServerThatAnswersFailOrNothingOrStaysUpWhenStoppedThenTheCauseIsNamed(t *testing.T) {
	tests := []struct {
		name      string
		answer    killAnswer
		wantCause string
	}{
		{name: "a server that rejects kills", answer: killRefused, wantCause: "it answered FAIL: " + adbRejectsKill},
		{name: "a FAIL with no readable reason", answer: killMalformedFail, wantCause: "it answered FAIL"},
		{name: "no answer", answer: killUnanswered, wantCause: "it answered nothing within 200ms"},
		{name: "neither OKAY nor FAIL", answer: killGarbled, wantCause: `it answered "WHAT", not OKAY or FAIL`},
		{name: "OKAY and no exit", answer: killStaysUp, wantCause: "it answered OKAY but port <port> is still held after 200ms; " +
			"a program that restarts adb, such as Android Studio, can take it back at once"},
		{name: "OKAY and a restart that keeps the port", answer: killRestarted, wantCause: "it answered OKAY but port <port> is " +
			"still held after 200ms; a program that restarts adb, such as Android Studio, can take it back at once"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			port := freePort(t)
			server := startFakeAdbServer(t, port, tc.answer)

			err := StopAdbServer("127.0.0.1", port, testStopWithin)

			require.Error(t, err)
			assert.Equal(t, strings.ReplaceAll(tc.wantCause, "<port>", strconv.Itoa(port)), err.Error())
			assert.Len(t, server.killFrames, 1, "one host:kill, as 'adb kill-server' sends")
		})
	}
}

func TestGivenNothingOnThePortWhenStoppedThenTheStopReportsNothingToDo(t *testing.T) {
	assert.NoError(t, StopAdbServer("127.0.0.1", freePort(t), testStopWithin),
		"a refused dial means the server is gone, as adb's own client takes it")
}

func TestGivenABindErrorWhenTestedForInUseThenEaddrinuseAndWinsock10048Match(t *testing.T) {
	bindError := func(errno syscall.Errno) error {
		return &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", errno)}
	}
	tests := []struct {
		name      string
		err       error
		goos      string
		wantInUse bool
		// wantProbe: a local socket holds the port, so the probe may dial its holder.
		wantProbe bool
	}{
		{name: "EADDRINUSE on Linux", err: bindError(syscall.EADDRINUSE), goos: "linux", wantInUse: true, wantProbe: true},
		{name: "EADDRNOTAVAIL on Linux", err: bindError(syscall.EADDRNOTAVAIL), goos: "linux"},
		{name: "permission denied on Linux", err: bindError(syscall.EACCES), goos: "linux"},
		{name: "WSAEADDRINUSE on Windows", err: bindError(winsockAddrInUse), goos: "windows", wantInUse: true, wantProbe: true},
		{name: "WSAEACCES on Windows", err: bindError(winsockAccessDenied), goos: "windows", wantProbe: true},
		{name: "WSAEADDRNOTAVAIL on Windows", err: bindError(syscall.Errno(10049)), goos: "windows"},
		{name: "WSAEADDRINUSE on Linux", err: bindError(winsockAddrInUse), goos: "linux"},
		{name: "WSAEACCES on Linux", err: bindError(winsockAccessDenied), goos: "linux"},
		{name: "another error", err: errors.New("boom"), goos: "windows"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantInUse, bindErrorInUse(tc.err, tc.goos))
			assert.Equal(t, tc.wantProbe, HolderMayAnswer(tc.err, tc.goos))
		})
	}
}

func TestGivenAListenAddressWhenItsHostIsResolvedThenTheBindAndTheDialShareOneAddress(t *testing.T) {
	for listenAddr, wantHost := range map[string]string{"127.0.0.1": "127.0.0.1", "::1": "::1", "fe80::1%lo": "fe80::1%lo",
		"localhost": "127.0.0.1"} {
		host, err := listenHost(listenAddr)

		require.NoError(t, err, listenAddr)
		assert.Equal(t, wantHost, host, "a literal keeps its zone; a name resolves as net.Listen resolves it")
	}
	_, err := listenHost("a\nb")
	assert.Error(t, err, "a name that does not resolve is the bind error")
}

func TestGivenANonDefaultListenAddressWhenAStartCommandIsQuotedThenItCarriesTheAddress(t *testing.T) {
	assert.Equal(t, "devicefarm-connect bridge start --port 5037", StartCommand("127.0.0.1", 5037),
		"a user on the default address never sees --listen-addr")
	assert.Equal(t, "devicefarm-connect bridge start --port 5555 --listen-addr 127.0.0.2", StartCommand("127.0.0.2", 5555))
	assert.Contains(t, HeldPortText("127.0.0.2", 5555), "127.0.0.2:5555, so the daemon cannot bind it")
	assert.Contains(t, HeldPortText("127.0.0.2", 5555), "run 'devicefarm-connect bridge start --port 5555 --listen-addr 127.0.0.2'")
}

func TestGivenAnAdbServerThatKeepsThePortWhenTheFixIsWordedThenTheDefaultPortQuitsTheProgramAndAnotherPortMovesTheDaemon(t *testing.T) {
	const otherPortFix = "Start the daemon on another port with 'devicefarm-connect bridge start --port <n>' and point adb at it with " +
		"'adb -P <n>'. That adb server keeps port 5555 and the daemon serves adb on the port you pass."
	tests := []struct {
		name       string
		listenAddr string
		port       int
		lead       KeptPortLead
		want       string
	}{
		{name: "the default port, nothing named", listenAddr: config.DefaultListenAddr, port: config.DefaultListenPort,
			lead: LeadNamesNothing, want: "Quit the program that started adb, such as Android Studio, run 'devicefarm-connect bridge " +
				"start --port 5037', then open that program again: it then uses the bridge as its adb server on port 5037."},
		{name: "the default port, the program named", listenAddr: config.DefaultListenAddr, port: config.DefaultListenPort,
			lead: LeadNamesProgram, want: "Quit that program, run 'devicefarm-connect bridge start --port 5037', then open it again: it " +
				"then uses the bridge as its adb server on port 5037."},
		{name: "the default port, the start command quoted", listenAddr: config.DefaultListenAddr, port: config.DefaultListenPort,
			lead: LeadQuotesStart, want: "quit that program, run 'devicefarm-connect bridge start --port 5037' again, then open it: it " +
				"then uses the bridge as its adb server on port 5037."},
		{name: "the default port off the default address", listenAddr: "0.0.0.0", port: config.DefaultListenPort,
			lead: LeadNamesProgram, want: "Quit that program, run 'devicefarm-connect bridge start --port 5037 --listen-addr 0.0.0.0', " +
				"then open it again: it then uses the bridge as its adb server on port 5037."},
		{name: "another port, nothing named", listenAddr: config.DefaultListenAddr, port: 5555, lead: LeadNamesNothing, want: otherPortFix},
		{name: "another port, the program named", listenAddr: config.DefaultListenAddr, port: 5555, lead: LeadNamesProgram, want: otherPortFix},
		{name: "another port, the start command quoted", listenAddr: config.DefaultListenAddr, port: 5555, lead: LeadQuotesStart,
			want: "start the daemon on another port with 'devicefarm-connect bridge start --port <n>' and point adb at it with 'adb -P <n>'."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, AdbServerKeepsPortFix(tc.listenAddr, tc.port, tc.lead))
		})
	}
}

func TestGivenEachOccupantWhenTheListenPortIsProbedThenItIsClassified(t *testing.T) {
	tests := []struct {
		name         string
		setup        func(t *testing.T, port int) (listenAddr string)
		wantOccupant portOccupant
		// wantHost is the address the bind tried and a stop would dial.
		wantHost    string
		wantBindErr bool
	}{
		{name: "free", setup: func(*testing.T, int) string { return "127.0.0.1" }, wantOccupant: portFree, wantHost: "127.0.0.1"},
		{name: "an adb server", setup: func(t *testing.T, port int) string {
			startFakeAdbServer(t, port, killExits)
			return "127.0.0.1"
		}, wantOccupant: portAdbServer, wantHost: "127.0.0.1", wantBindErr: true},
		{name: "an adb server behind a hostname", setup: func(t *testing.T, port int) string {
			startFakeAdbServer(t, port, killExits)
			return "localhost"
		}, wantOccupant: portAdbServer, wantHost: "127.0.0.1", wantBindErr: true},
		{name: "a silent occupant", setup: func(t *testing.T, port int) string {
			occupyPort(t, port, silentOccupant)
			return "127.0.0.1"
		}, wantOccupant: portHeld, wantHost: "127.0.0.1", wantBindErr: true},
		{name: "an address not on this machine", setup: func(*testing.T, int) string {
			return "203.0.113.1" // TEST-NET-3: not this machine, never routed
		}, wantOccupant: portUnbindable, wantHost: "203.0.113.1", wantBindErr: true},
		{name: "a name that does not resolve", setup: func(*testing.T, int) string { return "a\nb" },
			wantOccupant: portUnbindable, wantBindErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			port := freePort(t)
			listenAddr := tc.setup(t, port)

			occupant, host, bindErr := probeListenPort(listenAddr, port)

			assert.Equal(t, tc.wantOccupant, occupant)
			assert.Equal(t, tc.wantHost, host)
			assert.Equal(t, tc.wantBindErr, bindErr != nil)
		})
	}
}
