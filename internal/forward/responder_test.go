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

var (
	anySelector           = adbwire.AdbDeviceSelector{Kind: adbwire.AdbSelectorAny}
	listForwardRequest    = adbwire.AdbForwardRequest{Action: adbwire.AdbActionListForward}
	killForwardAllRequest = adbwire.AdbForwardRequest{Action: adbwire.AdbActionKillForwardAll}
	errWriteRefused       = errors.New("write refused")
	deviceMissReply       = "FAIL" + protoString(adbwire.FailDeviceNotFound(otherDeviceSelector.Serial))
	otherDeviceSelector   = adbwire.AdbDeviceSelector{Kind: adbwire.AdbSelectorSerial, Serial: "other-device"}
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

func forwardRequest(local, remote string, noRebind bool) adbwire.AdbForwardRequest {
	return adbwire.AdbForwardRequest{Action: adbwire.AdbActionForward, Local: local, Remote: remote, NoRebind: noRebind}
}

func killForwardRequest(local string) adbwire.AdbForwardRequest {
	return adbwire.AdbForwardRequest{Action: adbwire.AdbActionKillForward, Local: local}
}

// respond answers request and returns the bytes that reached the wire.
func respond(t *testing.T, table *ForwardRulesTable, selector adbwire.AdbDeviceSelector, request adbwire.AdbForwardRequest, deviceSerial string) string {
	t.Helper()
	conn := &replyConn{}
	require.NoError(t, Respond(conn, selector, request, deviceSerial, table))
	require.Equal(t, 1, conn.closes, "the reply must close the connection")
	return conn.String()
}

func TestGivenAForwardWhenRespondedThenOnlyTcpZeroReportsAResolvedPort(t *testing.T) {
	table := newTestTable(t)

	reply := respond(t, table, anySelector, forwardRequest("tcp:0", "tcp:5555", false), attachedSerial)
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
	reply = respond(t, table, anySelector, forwardRequest(fmt.Sprintf("tcp:%s", resolvedPort), "tcp:8080", false), attachedSerial)
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
			reply := respond(t, table, anySelector, testCase.request, attachedSerial)
			assert.Equal(t, "FAIL"+protoString(testCase.failText), reply, "a refusal is one FAIL, never OKAY then FAIL")
		})
	}
	assert.Equal(t, 1, table.ruleCount(), "no refused install may leave a rule behind")
}

func TestGivenKillForwardWhenRespondedThenTheRuleGoesAndASecondRemovalMisses(t *testing.T) {
	table := newTestTable(t)
	local := tcpSpec(installResolved(t, table, "tcp:5555"))

	assert.Equal(t, "OKAYOKAY", respond(t, table, anySelector, killForwardRequest(local), attachedSerial))
	assert.Zero(t, table.ruleCount())

	reply := respond(t, table, anySelector, killForwardRequest(local), attachedSerial)
	assert.Equal(t, "FAIL"+protoString(adbwire.FailListenerNotFound(local)), reply)
}

func TestGivenKillForwardAllWhenAnotherDeviceIsSelectedThenEveryRuleStillGoes(t *testing.T) {
	table := newTestTable(t)
	installResolved(t, table, "tcp:5555")
	installResolved(t, table, "tcp:8080")

	// adb answers killforward-all without acquiring a transport, so the
	// selector cannot refuse it.
	assert.Equal(t, "OKAYOKAY", respond(t, table, otherDeviceSelector, killForwardAllRequest, attachedSerial))
	assert.Zero(t, table.ruleCount())
}

func TestGivenRulesWhenListedThenEveryRowCarriesTheAttachedSerial(t *testing.T) {
	table := newTestTable(t)
	first := installResolved(t, table, "tcp:5555")
	second := installResolved(t, table, "tcp:8080")

	// A selector naming another device is ignored: adb lists globally.
	reply := respond(t, table, otherDeviceSelector, listForwardRequest, attachedSerial)

	rows := fmt.Sprintf("%s tcp:%d tcp:5555\n%s tcp:%d tcp:8080\n", attachedSerial, first, attachedSerial, second)
	assert.Equal(t, "OKAY"+protoString(rows), reply)
}

func TestGivenNoRulesWhenListedThenOkayAndAnEmptyTable(t *testing.T) {
	reply := respond(t, newTestTable(t), anySelector, listForwardRequest, attachedSerial)
	assert.Equal(t, "OKAY0000", reply, "an empty table is still OKAY plus a zero-length string")
}

func TestGivenADeviceMissWhenTheListenerWouldAlsoMissThenTheDeviceTextWins(t *testing.T) {
	table := newTestTable(t)
	local := tcpSpec(installResolved(t, table, "tcp:5555"))

	// adb acquires the transport first, so the device text outranks a missing
	// listener and a norebind collision, and the last two requests would have
	// succeeded for the attached device.
	assert.Equal(t, deviceMissReply, respond(t, table, otherDeviceSelector, killForwardRequest("tcp:1"), attachedSerial))
	assert.Equal(t, deviceMissReply, respond(t, table, otherDeviceSelector, forwardRequest(local, "tcp:5555", true), attachedSerial))
	assert.Equal(t, deviceMissReply, respond(t, table, otherDeviceSelector, killForwardRequest(local), attachedSerial))
	assert.Equal(t, deviceMissReply, respond(t, table, otherDeviceSelector, forwardRequest("tcp:0", "tcp:9999", false), attachedSerial))
	assert.Equal(t, 1, table.ruleCount())
	assert.Equal(t, local, table.List()[0].Local)
}

func TestGivenTransportIDSelectorWhenRespondedThenItMatchesTheSingleDevice(t *testing.T) {
	table := newTestTable(t)
	selector := adbwire.AdbDeviceSelector{Kind: adbwire.AdbSelectorTransportID, TransportID: 7}

	reply := respond(t, table, selector, forwardRequest("tcp:0", "tcp:5555", false), attachedSerial)
	assert.True(t, strings.HasPrefix(reply, "OKAYOKAY"), reply)
	assert.Equal(t, 1, table.ruleCount())
}

func TestGivenSelectorKindsWhenCheckedThenAdbsMissTextsFollowTheSerialAndKind(t *testing.T) {
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
		{"a transport id", adbwire.AdbDeviceSelector{Kind: adbwire.AdbSelectorTransportID, TransportID: 7}, attachedSerial, ""},
		{"the unset transport id", adbwire.AdbDeviceSelector{Kind: adbwire.AdbSelectorTransportID}, attachedSerial, ""},
		{"another serial", serialSelector("other-device"), attachedSerial, adbwire.FailDeviceNotFound("other-device")},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			failText, selectorMissed := SelectorMiss(testCase.selector, testCase.deviceSerial)
			assert.Equal(t, testCase.failText != "", selectorMissed)
			assert.Equal(t, testCase.failText, failText)
		})
	}
}

func TestGivenAConnThatRefusesWritesWhenRespondedThenTheErrorSurfacesAndItCloses(t *testing.T) {
	table := newTestTable(t)
	local := tcpSpec(installResolved(t, table, "tcp:5555"))

	tests := []struct {
		name     string
		selector adbwire.AdbDeviceSelector
		request  adbwire.AdbForwardRequest
	}{
		{"forward", anySelector, forwardRequest("tcp:0", "tcp:5555", false)},
		{"a refused forward", anySelector, forwardRequest("udp:1", "tcp:5555", false)},
		{"killforward", anySelector, killForwardRequest(local)},
		{"killforward-all", anySelector, killForwardAllRequest},
		{"list-forward", anySelector, listForwardRequest},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			conn := &brokenConn{}
			assert.ErrorIs(t, Respond(conn, testCase.selector, testCase.request, attachedSerial, table), errWriteRefused)
			assert.Equal(t, 1, conn.closes)
		})
	}
}

func TestGivenAnUnknownActionWhenRespondedThenNothingIsWritten(t *testing.T) {
	conn := &replyConn{}
	request := adbwire.AdbForwardRequest{Action: adbwire.AdbForwardAction(99)}

	assert.Error(t, Respond(conn, anySelector, request, attachedSerial, newTestTable(t)))
	assert.Empty(t, conn.String(), "an action with no reply must not half-answer")
	assert.Equal(t, 1, conn.closes)
}
