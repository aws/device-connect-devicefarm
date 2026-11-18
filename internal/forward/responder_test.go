// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package forward

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/adbwire"
)

// attachedSerial is the bound device's serial.
const attachedSerial = "R3CN30XXXX"

// attachedTransportID is the transport id the daemon reports for the bound device.
const attachedTransportID = 1

var (
	anySelector           = adbwire.AdbDeviceSelector{Kind: adbwire.AdbSelectorAny}
	listForwardRequest    = adbwire.AdbForwardRequest{Action: adbwire.AdbActionListForward}
	killForwardAllRequest = adbwire.AdbForwardRequest{Action: adbwire.AdbActionKillForwardAll}
	errWriteRefused       = errors.New("write refused")
)

// replyConn collects the reply bytes and counts closes: every answer must close
// the connection, as adb does after a host service.
type replyConn struct {
	bytes.Buffer
	closes int
}

func (conn *replyConn) Write(p []byte) (int, error) {
	if conn.closes > 0 {
		return 0, errors.New("write after close")
	}
	return conn.Buffer.Write(p)
}

func (conn *replyConn) Close() error {
	conn.closes++
	return nil
}

// brokenConn refuses every write, so a test can prove the error surfaces.
type brokenConn struct {
	closes int
}

func (*brokenConn) Write([]byte) (int, error) { return 0, errWriteRefused }

func (conn *brokenConn) Close() error {
	conn.closes++
	return nil
}

// protoString frames payload as adb's protocol string: the byte length as four
// lowercase hex digits, then the bytes.
func protoString(payload string) string {
	return fmt.Sprintf("%04x%s", len(payload), payload)
}

func serialSelector(serial string) adbwire.AdbDeviceSelector {
	return adbwire.AdbDeviceSelector{Kind: adbwire.AdbSelectorSerial, Serial: serial}
}

func transportIDSelector(transportID uint64) adbwire.AdbDeviceSelector {
	return adbwire.AdbDeviceSelector{Kind: adbwire.AdbSelectorTransportID, TransportID: transportID}
}

func forwardRequest(local, remote string, noRebind bool) adbwire.AdbForwardRequest {
	return adbwire.AdbForwardRequest{Action: adbwire.AdbActionForward, Local: local, Remote: remote, NoRebind: noRebind}
}

func killForwardRequest(local string) adbwire.AdbForwardRequest {
	return adbwire.AdbForwardRequest{Action: adbwire.AdbActionKillForward, Local: local}
}

// respond answers request and returns the bytes that reached the wire.
func respond(t *testing.T, table *ForwardRulesTable, request adbwire.AdbForwardRequest, deviceSerial string) string {
	t.Helper()
	conn := &replyConn{}
	require.NoError(t, Respond(conn, request, deviceSerial, table))
	require.Equal(t, 1, conn.closes, "the reply must close the connection")
	return conn.String()
}

func TestGivenAForwardWhenRespondedThenOnlyTcpZeroReportsAResolvedPort(t *testing.T) {
	table := newTestTable(t)

	reply := respond(t, table, forwardRequest("tcp:0", "tcp:5555", false), attachedSerial)
	rows := table.List()
	require.Len(t, rows, 1)
	resolvedPort := strings.TrimPrefix(rows[0].Local, "tcp:")
	require.Equal(t, "OKAYOKAY"+protoString(resolvedPort), reply, "tcp:0 reports the port it resolved")

	// A client reads the port out of the reply, so that port must be listening.
	port, err := strconv.Atoi(reply[len("OKAYOKAY")+4:])
	require.NoError(t, err)
	target, _, _ := dialRule(t, port)
	assert.Equal(t, "tcp:5555", target)

	// Rebinding the port tcp:0 resolved is a client-chosen local port, and needs
	// no second socket to test the reply for one.
	reply = respond(t, table, forwardRequest(fmt.Sprintf("tcp:%s", resolvedPort), "tcp:8080", false), attachedSerial)
	assert.Equal(t, "OKAYOKAY", reply, "a client-chosen port gets no extra protocol string")
	assert.Equal(t, 1, table.ruleCount())
}

func TestGivenAnUninstallableForwardWhenRespondedThenOneFailWithAdbsText(t *testing.T) {
	table := newTestTable(t)
	installed := tcpSpec(installResolved(t, table, "tcp:5555"))
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { blocker.Close() })
	held := tcpSpec(blocker.Addr().(*net.TCPAddr).Port)

	tests := []struct {
		name     string
		request  adbwire.AdbForwardRequest
		failText string
	}{
		{"norebind over an existing rule", forwardRequest(installed, "tcp:5555", true), adbwire.MsgCannotRebind},
		{"a port another process holds", forwardRequest(held, "tcp:5555", false), adbwire.FailCannotBind("Address already in use")},
		{"a local kind this daemon never serves", forwardRequest("udp:1", "tcp:5555", false), adbwire.FailCannotBind("unknown socket specification:udp:1")},
		{"a remote kind this daemon never serves", forwardRequest("tcp:0", "localabstract:x", false), "unknown socket specification:localabstract:x"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			reply := respond(t, table, testCase.request, attachedSerial)
			assert.Equal(t, "FAIL"+protoString(testCase.failText), reply, "a refusal is one FAIL, never OKAY then FAIL")
		})
	}
	assert.Equal(t, 1, table.ruleCount(), "no refused install may leave a rule behind")
}

func TestGivenKillForwardWhenRespondedThenTheRuleGoesAndASecondRemovalMisses(t *testing.T) {
	table := newTestTable(t)
	local := tcpSpec(installResolved(t, table, "tcp:5555"))

	assert.Equal(t, "OKAYOKAY", respond(t, table, killForwardRequest(local), attachedSerial))
	assert.Zero(t, table.ruleCount())

	reply := respond(t, table, killForwardRequest(local), attachedSerial)
	assert.Equal(t, "FAIL"+protoString(adbwire.FailListenerNotFound(local)), reply)
}

func TestGivenKillForwardAllWhenRespondedThenEveryRuleGoes(t *testing.T) {
	table := newTestTable(t)
	installResolved(t, table, "tcp:5555")
	installResolved(t, table, "tcp:8080")

	assert.Equal(t, "OKAYOKAY", respond(t, table, killForwardAllRequest, attachedSerial))
	assert.Zero(t, table.ruleCount())
}

func TestGivenRulesWhenListedThenEveryRowCarriesTheAttachedSerial(t *testing.T) {
	table := newTestTable(t)
	first := installResolved(t, table, "tcp:5555")
	second := installResolved(t, table, "tcp:8080")

	reply := respond(t, table, listForwardRequest, attachedSerial)

	rows := fmt.Sprintf("%s tcp:%d tcp:5555\n%s tcp:%d tcp:8080\n", attachedSerial, first, attachedSerial, second)
	assert.Equal(t, "OKAY"+protoString(rows), reply)
}

func TestGivenNoRulesWhenListedThenOkayAndAnEmptyTable(t *testing.T) {
	reply := respond(t, newTestTable(t), listForwardRequest, attachedSerial)
	assert.Equal(t, "OKAY0000", reply, "an empty table is still OKAY plus a zero-length string")
}

func TestGivenSelectorKindsWhenCheckedThenAdbsMissTextsFollowTheSerialKindAndTransportID(t *testing.T) {
	// The device's kind is read off its serial: emulator-<port> is local, anything else usb.
	const emulatorSerial = "emulator-5554"
	usbSelector := adbwire.AdbDeviceSelector{Kind: adbwire.AdbSelectorUSB}
	localSelector := adbwire.AdbDeviceSelector{Kind: adbwire.AdbSelectorLocal}
	tests := []struct {
		name         string
		selector     adbwire.AdbDeviceSelector
		deviceSerial string
		failText     string
	}{
		{"any device", anySelector, attachedSerial, ""},
		{"any device, an emulator attached", anySelector, emulatorSerial, ""},
		{"any usb device", usbSelector, attachedSerial, ""},
		{"any usb device, an emulator attached", usbSelector, emulatorSerial, "no devices found"},
		{"any local device", localSelector, attachedSerial, "no emulators found"},
		{"any local device, an emulator attached", localSelector, emulatorSerial, ""},
		{"the attached serial", serialSelector(attachedSerial), attachedSerial, ""},
		{"the device's transport id", transportIDSelector(attachedTransportID), attachedSerial, ""},
		{"the unset transport id", transportIDSelector(0), attachedSerial, ""},
		{"another transport id", transportIDSelector(7), attachedSerial, "no device with transport id '7'"},
		{"another serial", serialSelector("other-device"), attachedSerial, adbwire.FailDeviceNotFound("other-device")},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			failText, selectorMissed := SelectorMiss(testCase.selector, testCase.deviceSerial, attachedTransportID)
			assert.Equal(t, testCase.failText != "", selectorMissed)
			assert.Equal(t, testCase.failText, failText)
		})
	}
}

func TestGivenNoDeviceWhenEachSelectorIsCheckedThenAdbsMissTextNamesIt(t *testing.T) {
	tests := []struct {
		name     string
		selector adbwire.AdbDeviceSelector
		failText string
	}{
		{"any device", anySelector, "no devices/emulators found"},
		{"any usb device", adbwire.AdbDeviceSelector{Kind: adbwire.AdbSelectorUSB}, "no devices found"},
		{"any local device", adbwire.AdbDeviceSelector{Kind: adbwire.AdbSelectorLocal}, "no emulators found"},
		{"a serial", serialSelector("R3CN30XXXX"), "device 'R3CN30XXXX' not found"},
		{"a transport id", transportIDSelector(1), "no device with transport id '1'"},
		{"the unset transport id", transportIDSelector(0), "no devices/emulators found"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			assert.Equal(t, testCase.failText, MissReason(testCase.selector))
		})
	}
}

func TestGivenAConnThatRefusesWritesWhenRespondedThenTheErrorSurfacesAndItCloses(t *testing.T) {
	table := newTestTable(t)
	local := tcpSpec(installResolved(t, table, "tcp:5555"))

	tests := []struct {
		name    string
		request adbwire.AdbForwardRequest
	}{
		{"forward", forwardRequest("tcp:0", "tcp:5555", false)},
		{"a refused forward", forwardRequest("udp:1", "tcp:5555", false)},
		{"killforward", killForwardRequest(local)},
		{"killforward-all", killForwardAllRequest},
		{"list-forward", listForwardRequest},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			conn := &brokenConn{}
			assert.ErrorIs(t, Respond(conn, testCase.request, attachedSerial, table), errWriteRefused)
			assert.Equal(t, 1, conn.closes)
		})
	}
}

func TestGivenAnUnknownActionWhenRespondedThenNothingIsWritten(t *testing.T) {
	conn := &replyConn{}
	request := adbwire.AdbForwardRequest{Action: adbwire.AdbForwardAction(99)}

	assert.Error(t, Respond(conn, request, attachedSerial, newTestTable(t)))
	assert.Empty(t, conn.String(), "an action with no reply must not half-answer")
	assert.Equal(t, 1, conn.closes)
}
