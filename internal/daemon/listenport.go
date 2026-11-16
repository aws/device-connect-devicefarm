// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/aws/device-connect-devicefarm/internal/adbwire"
	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/wording"
)

// stopReplyTimeout bounds the occupant probe's reply and each wait of an adb server stop.
const stopReplyTimeout = time.Second

// Winsock codes a Windows bind error carries; syscall.EADDRINUSE never matches one.
const (
	winsockAccessDenied = syscall.Errno(10013) // WSAEACCES: an exclusive holder, or a reserved range
	winsockAddrInUse    = syscall.Errno(10048) // WSAEADDRINUSE
)

// errStartedMeanwhile: a concurrent start's daemon took the port before the stop. Start's pidfile
// re-read turns it into success.
var errStartedMeanwhile = errors.New(wording.PortStartedMeanwhile)

// portOccupant is what a bind of the listen address found.
type portOccupant int

const (
	portFree portOccupant = iota
	portAdbServer
	// portBridge: another bridge answered the identity request, from another runtime directory or
	// user. It answers host:version too, so the probe asks the identity first.
	portBridge
	// portHeld: a program that is not an adb server answered, or nobody answered an in-use bind.
	portHeld
	// portUnbindable: any other bind error.
	portUnbindable
)

// OccupantAddress is where to dial the port's occupant: the loopback of the family when the
// listen address is unspecified, which Windows refuses to dial.
func OccupantAddress(listenAddr string, port int) string {
	host := listenAddr
	if ip := net.ParseIP(listenAddr); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
		if ip.To4() == nil {
			host = "::1"
		}
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// statusOkay opens every answer a probe accepts; statusFail opens adb's refusal.
const (
	statusOkay = "OKAY"
	statusFail = "FAIL"
)

// askOccupant dials address, sends request and reads replyLength bytes, each step bounded by
// within. conn is nil when the dial failed; else the caller closes it. err: no whole reply came.
func askOccupant(address, request string, replyLength int, within time.Duration) (conn net.Conn, reply []byte, err error) {
	conn, err = net.DialTimeout("tcp", address, within)
	if err != nil {
		return nil, nil, err
	}
	reply = make([]byte, replyLength)
	if err = conn.SetDeadline(time.Now().Add(within)); err == nil {
		if _, err = conn.Write([]byte(request)); err == nil {
			_, err = io.ReadFull(conn, reply)
		}
	}
	return conn, reply, err
}

// ProbeOccupant sends host:version to the port's occupant, each step bounded by within.
// adbServer: the reply was OKAY, 0004 and four hex digits. connected: the dial succeeded.
func ProbeOccupant(address string, within time.Duration) (version int, connected bool, adbServer bool) {
	conn, reply, err := askOccupant(address, adbwire.HostVersionRequest, len(statusOkay)+adbwire.HostVersionReplyLen, within)
	if conn == nil {
		return 0, false, false
	}
	defer conn.Close()
	if err != nil || string(reply[:len(statusOkay)]) != statusOkay {
		return 0, true, false
	}
	parsed, err := adbwire.ParseHostVersionReply(reply[len(statusOkay):])
	return parsed, true, err == nil
}

// ProbeBridge asks the port's occupant for its bridge identity (adbwire.BridgeIdentityService),
// each step bounded by within. isBridge only for OKAY and the exact identity payload, read up to
// adbwire.MaxBridgeIdentityLen bytes. refused: the occupant answered FAIL, as adb's server does
// for a service it does not serve. Any other outcome (a failed dial, a close, silence, another
// reply) is neither, and the caller must not take the occupant for an adb server: a bridge cut
// mid-answer answers host:version too. The pid came from a socket any local program can hold:
// print it, never signal or stop it.
func ProbeBridge(address string, within time.Duration) (pid int, isBridge, refused bool) {
	conn, status, err := askOccupant(address, adbwire.BridgeIdentityRequest, len(statusOkay), within)
	if conn == nil {
		return 0, false, false
	}
	defer conn.Close()
	if err != nil || string(status) != statusOkay {
		return 0, false, err == nil && string(status) == statusFail
	}
	digits := make([]byte, 4)
	if _, err = io.ReadFull(conn, digits); err != nil {
		return 0, false, false
	}
	length, err := strconv.ParseUint(string(digits), 16, 16)
	if err != nil || length > adbwire.MaxBridgeIdentityLen {
		return 0, false, false
	}
	payload := make([]byte, length)
	if _, err = io.ReadFull(conn, payload); err != nil {
		return 0, false, false
	}
	pid, isBridge = adbwire.ParseBridgeIdentity(string(payload))
	return pid, isBridge, false
}

// StopAdbServer stops the adb server on the listen port as 'adb kill-server' does: host:kill,
// OKAY, then the close its exit brings. A failed dial means it is gone. After OKAY the port must
// bind within the bound: the listener can close just after the connection, and a program that
// restarts adb can take the port back. The error is the cause, worded for the caller's text.
func StopAdbServer(listenAddr string, port int, within time.Duration) error {
	conn, err := net.DialTimeout("tcp", OccupantAddress(listenAddr, port), within)
	if err != nil {
		return nil
	}
	defer conn.Close()
	status := make([]byte, 4)
	if err = conn.SetDeadline(time.Now().Add(within)); err == nil {
		_, err = conn.Write([]byte(adbwire.HostKillRequest))
	}
	if err == nil {
		_, err = io.ReadFull(conn, status)
	}
	switch {
	case err != nil:
		return fmt.Errorf(wording.PortStopNoAnswerFormat, within)
	case string(status) == "FAIL":
		return errors.New(failCause(conn))
	case string(status) != "OKAY":
		return fmt.Errorf(wording.PortStopBadReplyFormat, status)
	}
	deadline := time.Now().Add(within)
	if conn.SetDeadline(deadline) == nil {
		_, _ = io.Copy(io.Discard, conn)
	}
	for bindListenPort(listenAddr, port) != nil {
		if !time.Now().Before(deadline) {
			return fmt.Errorf(wording.PortStopStillHeldFormat, port, within)
		}
		time.Sleep(startupPollInterval)
	}
	return nil
}

// failCause reads the length-prefixed reason after FAIL; a malformed one leaves the bare FAIL.
func failCause(conn net.Conn) string {
	digits := make([]byte, 4)
	_, err := io.ReadFull(conn, digits)
	var length uint64
	if err == nil {
		length, err = strconv.ParseUint(string(digits), 16, 16)
	}
	reason := make([]byte, length)
	if err == nil {
		_, err = io.ReadFull(conn, reason)
	}
	if err != nil {
		return wording.PortStopFailed
	}
	return wording.PortStopFailedLead + string(reason)
}

// HeldPortText is the fix when another program holds the listen port.
func HeldPortText(listenAddr string, port int) string {
	return fmt.Sprintf(wording.PortHeldFormat, net.JoinHostPort(listenAddr, strconv.Itoa(port)), StartCommand(listenAddr, port))
}

// BridgeHoldsPortText is the fix when another bridge holds the listen port; the pid is its own answer.
func BridgeHoldsPortText(listenAddr string, port, pid int) string {
	return fmt.Sprintf(wording.PortHeldByBridgeFormat, pid, net.JoinHostPort(listenAddr, strconv.Itoa(port)), pid,
		StartCommand(listenAddr, port))
}

// StartCommand is 'bridge start' on port, with --listen-addr only off the default address: a user
// on the default address never sees that flag, and any other user keeps the address they passed.
func StartCommand(listenAddr string, port int) string {
	command := fmt.Sprintf(wording.PortStartCommandFormat, port)
	if listenAddr != config.DefaultListenAddr {
		command += wording.ListenAddrFlagLead + listenAddr
	}
	return command
}

// KeptPortLead: what the text before AdbServerKeepsPortFix already told the user.
type KeptPortLead int

const (
	// LeadNamesNothing: no program named yet.
	LeadNamesNothing KeptPortLead = iota
	// LeadNamesProgram: the program that restarts adb is named.
	LeadNamesProgram
	// LeadQuotesStart: program named, start command quoted; the fix ends the caller's sentence.
	LeadQuotesStart
)

// AdbServerKeepsPortFix is the fix for an adb server that keeps or retakes the port. Default port:
// quit the program that restarts adb, run bridge start, then open it again. Else: another port.
func AdbServerKeepsPortFix(listenAddr string, port int, lead KeptPortLead) string {
	if port != config.DefaultListenPort {
		if lead == LeadQuotesStart {
			return wording.PortStartOnOtherPortTail
		}
		return fmt.Sprintf(wording.PortStartOnOtherPortFormat, port)
	}
	command := StartCommand(listenAddr, port)
	switch lead {
	case LeadNamesProgram:
		return fmt.Sprintf(wording.PortQuitThatProgramFormat, command, port)
	case LeadQuotesStart:
		return fmt.Sprintf(wording.PortQuitThatProgramTailFormat, command, port)
	}
	return fmt.Sprintf(wording.PortQuitProgramThatStartedAdbFormat, command, port)
}

// bindListenPort binds the daemon's own address once and frees it.
func bindListenPort(listenAddr string, port int) error {
	listener, err := net.Listen("tcp", net.JoinHostPort(listenAddr, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	return listener.Close()
}

// listenHost is the IP net.Listen binds for listenAddr: the literal itself, zone kept, or the one
// address a hostname resolves to by the same rule. The probe binds and dials that one address, so
// a name with several addresses never sends host:kill to an address the bind did not try.
func listenHost(listenAddr string) (string, error) {
	if _, err := netip.ParseAddr(listenAddr); err == nil {
		return listenAddr, nil
	}
	resolved, err := net.ResolveTCPAddr("tcp", net.JoinHostPort(listenAddr, "0"))
	if err != nil {
		return "", err
	}
	return resolved.IP.String(), nil
}

// probeListenPort binds the daemon's own address and frees it. When a local socket holds the
// port, the bridge identity request, then one host:version, tell another bridge, an adb server
// and any other occupant apart. host is the address it tried, for the stop; pid is a bridge's.
func probeListenPort(listenAddr string, port int) (occupant portOccupant, host string, pid int, bindErr error) {
	if host, bindErr = listenHost(listenAddr); bindErr == nil {
		bindErr = bindListenPort(host, port)
	}
	switch {
	case bindErr == nil:
		return portFree, host, 0, nil
	case !HolderMayAnswer(bindErr, runtime.GOOS):
		return portUnbindable, host, 0, bindErr
	}
	address := OccupantAddress(host, port)
	pid, isBridge, identityRefused := ProbeBridge(address, stopReplyTimeout)
	if isBridge {
		return portBridge, host, pid, bindErr
	}
	_, connected, adbServer := ProbeOccupant(address, stopReplyTimeout)
	switch {
	// Only adb's FAIL to the identity request makes a version answer an adb server's: any other
	// occupant may be a bridge, so it is held, never stopped.
	case adbServer && identityRefused:
		return portAdbServer, host, 0, bindErr
	case connected || bindErrorInUse(bindErr, runtime.GOOS):
		return portHeld, host, 0, bindErr
	}
	return portUnbindable, host, 0, bindErr
}

// HolderMayAnswer reports whether a bind error means a socket on this machine holds the port, so
// a probe reaches that holder: in use, or on Windows access denied, which an exclusive holder
// causes. Any other error names no local holder, and a dial could reach another machine. goos is
// a parameter so one OS tests every arm.
func HolderMayAnswer(err error, goos string) bool {
	return bindErrorInUse(err, goos) || (goos == "windows" && errors.Is(err, winsockAccessDenied))
}

// bindErrorInUse reports an address-in-use bind error.
func bindErrorInUse(err error, goos string) bool {
	if goos == "windows" {
		return errors.Is(err, winsockAddrInUse)
	}
	return errors.Is(err, syscall.EADDRINUSE)
}

// clearListenPort runs before the spawn: a free port passes, an adb server is stopped and its
// address returned, anything else is Start's error. A bridge never gets host:kill.
func (m *DaemonManager) clearListenPort() (stoppedAdbServer string, err error) {
	listenAddr, port := m.config.ListenAddr, m.config.ListenPort
	occupant, host, pid, bindErr := probeListenPort(listenAddr, port)
	if occupant != portAdbServer && occupant != portBridge {
		return "", portError(occupant, bindErr, listenAddr, port, pid)
	}
	// A concurrent start's daemon in this directory answers too. Never stop it.
	if _, running := m.pidFile.RunningPID(); running {
		return "", errStartedMeanwhile
	}
	if occupant == portBridge {
		return "", portError(occupant, bindErr, listenAddr, port, pid)
	}
	address := net.JoinHostPort(listenAddr, strconv.Itoa(port))
	if cause := StopAdbServer(host, port, stopReplyTimeout); cause != nil {
		return "", fmt.Errorf(wording.PortAdbServerDidNotStopFormat, address, cause,
			AdbServerKeepsPortFix(listenAddr, port, LeadNamesNothing))
	}
	return address, nil
}

// lostBindError words a child whose bind failed after the pre-check passed: the port is probed
// again.
func lostBindError(listenAddr string, port int) error {
	address := net.JoinHostPort(listenAddr, strconv.Itoa(port))
	occupant, _, pid, bindErr := probeListenPort(listenAddr, port)
	switch occupant {
	case portAdbServer:
		return fmt.Errorf(wording.PortAdbServerTookPortFormat, address, AdbServerKeepsPortFix(listenAddr, port, LeadNamesProgram))
	case portFree:
		return fmt.Errorf(wording.PortReleasedFormat, address, StartCommand(listenAddr, port))
	}
	return portError(occupant, bindErr, listenAddr, port, pid)
}

// portError words a held or unbindable listen port; nil when it is free. pid is a bridge's. The
// rare bind errors point at the doctor, which names their fixes.
func portError(occupant portOccupant, bindErr error, listenAddr string, port, pid int) error {
	address := net.JoinHostPort(listenAddr, strconv.Itoa(port))
	switch occupant {
	case portFree:
		return nil
	case portBridge:
		return errors.New(BridgeHoldsPortText(listenAddr, port, pid))
	case portHeld:
		return errors.New(HeldPortText(listenAddr, port))
	}
	doctorCommand := fmt.Sprintf(wording.PortDoctorCommandFormat, port)
	if listenAddr != config.DefaultListenAddr {
		doctorCommand += wording.ListenAddrFlagLead + listenAddr
	}
	return fmt.Errorf(wording.PortUnbindableFormat, address, bindErr, doctorCommand)
}
