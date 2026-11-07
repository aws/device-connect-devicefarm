// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/forward"
	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// installForward is the framed request of `adb forward tcp:0 tcp:9999`. adb sends it behind a
// select; the daemon answers it alone too, so one write installs a rule.
const installForward = "001bhost:forward:tcp:0;tcp:9999"

// startForwardingProxy runs a proxy whose attachment answers the forward family from its own
// table, in front of a mock that would answer the daemon's canonical select. It returns the
// address, so a test can bring a second client, and the table the answers come from.
func startForwardingProxy(t *testing.T) (proxyAddress string, mock *testsupport.SessionMockWSS, table *forward.ForwardRulesTable) {
	t.Helper()
	return startForwardingProxyWithSerial(t, "")
}

// startForwardingProxyWithSerial is startForwardingProxy with the device bound under serial.
func startForwardingProxyWithSerial(t *testing.T, serial string) (proxyAddress string,
	mock *testsupport.SessionMockWSS, table *forward.ForwardRulesTable) {
	t.Helper()
	proxyAddress, mock, device := startAttachedProxyWithSerial(t, testPolicy(), serial)
	return proxyAddress, mock, attachedFeatures(t, device).table
}

// framed renders one smart-socket request with its 4-hex length prefix.
func framed(request string) []byte {
	return []byte(fmt.Sprintf("%04x%s", len(request), request))
}

// forwardingFor builds an attachment's forwarding with no proxy in front of it, so a test
// can ask the seam directly.
func forwardingFor(t *testing.T) (*ActiveDevice, *attachmentFeatures) {
	t.Helper()
	device := newForwardingDevice(t)
	require.True(t, device.Set(DeviceInfo{ARN: "arn:test", Endpoint: "wss://unused/1"}))
	return device, attachedFeatures(t, device)
}

// readForwardSuccessPort reads an install's reply and returns the port adb would print,
// failing the test on any other reply shape.
func readForwardSuccessPort(t *testing.T, client net.Conn) int {
	t.Helper()
	require.Equal(t, "OKAYOKAY", string(readN(t, client, 8)), "an install is answered with two status words")
	size, err := strconv.ParseUint(string(readN(t, client, 4)), 16, 16)
	require.NoError(t, err, "the resolved port follows its own length prefix")
	port, err := strconv.Atoi(string(readN(t, client, int(size))))
	require.NoError(t, err)
	return port
}

func TestGivenAForwardRequestWhenItArrivesThenItIsAnsweredLocallyAndNoWebsocketIsDialed(t *testing.T) {
	// A lone host-prefixed forward request, as a raw client may send it. The rule and its
	// listening socket are local, so the device's own adb server could not serve this.
	proxyAddress, mock, _ := startForwardingProxy(t)
	client := dialClient(t, proxyAddress)

	writeAll(t, client, []byte(installForward))

	assert.NotZero(t, readForwardSuccessPort(t, client), "a tcp:0 install is answered with the port it got")
	// adb reads the optional resolved port until end of stream, as after any host service.
	_, err := client.Read(make([]byte, 1))
	assert.ErrorIs(t, err, io.EOF, "the daemon closes a connection it answered itself")
	assertNoWebsocketDialed(t, mock)
}

func TestGivenASelectThenAForwardRequestWhenReceivedThenAnsweredLocallyAndNoWebsocketIsDialed(t *testing.T) {
	// No real client sends a bare forward behind a select; the daemon still answers it, and
	// the selector must come from the select.
	proxyAddress, mock, table := startForwardingProxy(t)
	client := dialClient(t, proxyAddress)

	selectAnyTransport(t, client)
	writeAll(t, client, []byte("0016forward:tcp:0;tcp:9999"))

	assert.NotZero(t, readForwardSuccessPort(t, client), "a request behind a select is answered like any other")
	assert.Len(t, table.List(), 1, "the rule lives in the attachment's own table")
	assertNoWebsocketDialed(t, mock)
}

func TestGivenASelectThenAHostPrefixedForwardWhenReceivedThenAnsweredLocallyAndNoWebsocketIsDialed(t *testing.T) {
	// `adb forward`: the select, then the request with its host prefix kept. The prefix
	// names no device; the select did.
	proxyAddress, mock, table := startForwardingProxy(t)
	client := dialClient(t, proxyAddress)

	selectAnyTransport(t, client)
	writeAll(t, client, []byte(installForward))

	assert.NotZero(t, readForwardSuccessPort(t, client), "a host-prefixed request behind a select is answered like any other")
	assert.Len(t, table.List(), 1, "the rule lives in the attachment's own table")
	assertNoWebsocketDialed(t, mock)
}

func TestGivenASelectThenAHostPrefixedKillforwardWhenReceivedThenTheRuleIsRemovedAndNoWebsocketIsDialed(t *testing.T) {
	// `adb forward --remove`: the select, then host:killforward:<local>.
	proxyAddress, mock, table := startForwardingProxy(t)
	installer := dialClient(t, proxyAddress)
	writeAll(t, installer, []byte(installForward))
	port := readForwardSuccessPort(t, installer)

	remover := dialClient(t, proxyAddress)
	selectAnyTransport(t, remover)
	writeAll(t, remover, framed(fmt.Sprintf("host:killforward:tcp:%d", port)))

	assert.Equal(t, "OKAYOKAY", string(readN(t, remover, 8)), "a removal behind a select is answered like any other")
	assert.Empty(t, table.List(), "the rule is gone from the attachment's table")
	assertNoWebsocketDialed(t, mock)
}

func TestGivenAFragmentedForwardRequestWhenReceivedThenStillAnsweredLocally(t *testing.T) {
	// A request is classified only once it is whole, so one that dribbles in a byte at a
	// time is answered the same way.
	proxyAddress, mock, _ := startForwardingProxy(t)
	client := dialClient(t, proxyAddress)

	for _, requestByte := range []byte(installForward) {
		writeAll(t, client, []byte{requestByte})
		time.Sleep(time.Millisecond)
	}

	assert.NotZero(t, readForwardSuccessPort(t, client), "a reassembled request is still a forward request")
	assertNoWebsocketDialed(t, mock)
}

func TestGivenAMalformedForwardServiceWhenReceivedThenAdbFailTextAndNoDial(t *testing.T) {
	// `adb forward tcp:1` with no destination. A local adb refuses it without reaching a
	// device, which is what makes the command behave the same while the network is down.
	proxyAddress, mock, table := startForwardingProxy(t)
	client := dialClient(t, proxyAddress)

	writeAll(t, client, []byte("0012host:forward:tcp:1"))

	want := "FAIL0012bad forward: tcp:1"
	assert.Equal(t, want, string(readN(t, client, len(want))), "the client reads adb's own reason")
	assert.Empty(t, table.List(), "a refused request installs nothing")
	assertNoWebsocketDialed(t, mock)
}

func TestGivenARuleInstalledOnOneConnectionWhenAnotherListsThenTheTableIsRendered(t *testing.T) {
	// The table belongs to the attachment, not to the connection that installed the rule.
	proxyAddress, mock, _ := startForwardingProxyWithSerial(t, "R3CN30ABCDE")
	installer := dialClient(t, proxyAddress)
	writeAll(t, installer, []byte(installForward))
	port := readForwardSuccessPort(t, installer)

	lister := dialClient(t, proxyAddress)
	writeAll(t, lister, []byte("0011host:list-forward"))

	row := fmt.Sprintf("R3CN30ABCDE tcp:%d tcp:9999\n", port)
	want := fmt.Sprintf("OKAY%04x%s", len(row), row)
	assert.Equal(t, want, string(readN(t, lister, len(want))), "one row per rule, three fields each")
	assertNoWebsocketDialed(t, mock)
}

func TestGivenALocalabstractRuleWhenAConnectionIsAcceptedThenTheDeviceServiceIsTheSocketSpec(t *testing.T) {
	// The remote spec goes to the device as the stream's service, as the client spelled
	// it, after the daemon's own select.
	mock := newPrologueMock(t, "OKAY", "OKAY")
	proxyAddress, _ := attachProxyTo(t, mock.SessionMockWSS, testPolicy(), "")
	installer := dialClient(t, proxyAddress)
	writeAll(t, installer, framed("host:forward:tcp:0;localabstract:chrome_devtools_remote"))
	port := readForwardSuccessPort(t, installer)

	dialClient(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))

	waitFor(t, func() bool { return len(mock.Frames()) == 2 }, "the forwarded stream's prologue never arrived")
	assert.Equal(t, []string{canonicalSelect, string(framed("localabstract:chrome_devtools_remote"))}, mock.Frames())
	assert.Equal(t, 1, mock.SessionsOpened(), "one forwarded connection is one session")
}

func TestGivenASerialSelectorNamingAnotherDeviceWhenForwardingThenItFailsWithDeviceNotFound(t *testing.T) {
	// The seam is asked directly: this pins the refusal itself, the end-to-end case below
	// pins the wiring that reaches it.
	_, forwarding := forwardingFor(t)
	client := newScriptedConn("")
	clientOpening := opening{service: "host-serial:other:forward:tcp:0;tcp:9999"}

	answered := respondForwardLocally(client, clientOpening, forwarding, "dev-1", noopLogger())

	assert.True(t, answered, "a request for another device is refused here, not sent to the device")
	assert.Equal(t, "FAIL0018device 'other' not found", client.replies.String())
	assert.Empty(t, forwarding.table.List(), "a refused request installs nothing")
}

func TestGivenAMalformedForwardBehindAnotherDevicesSerialWhenAnsweredThenTheDeviceMissOutranksIt(t *testing.T) {
	// adb acquires the transport before it reads a request's pieces, so the reason the
	// client prints is the device miss, not the malformed request.
	_, forwarding := forwardingFor(t)
	client := newScriptedConn("")
	clientOpening := opening{service: "host-serial:other:forward:tcp:1"}

	answered := respondForwardLocally(client, clientOpening, forwarding, "dev-1", noopLogger())

	assert.True(t, answered, "a malformed forward request is still answered locally")
	assert.Equal(t, "FAIL0018device 'other' not found", client.replies.String())
}

func TestGivenABoundSerialWhenTheTableIsListedThenTheSerialColumnNamesTheDevice(t *testing.T) {
	// The lister is a second connection, as `adb forward --list` always is.
	proxyAddress, _, _ := startForwardingProxyWithSerial(t, "R3CN30ABCDE")
	installer := dialClient(t, proxyAddress)
	writeAll(t, installer, []byte(installForward))
	port := readForwardSuccessPort(t, installer)

	lister := dialClient(t, proxyAddress)
	writeAll(t, lister, framed("host:list-forward"))

	row := fmt.Sprintf("R3CN30ABCDE tcp:%d tcp:9999\n", port)
	want := fmt.Sprintf("OKAY%04x%s", len(row), row)
	assert.Equal(t, want, string(readN(t, lister, len(want))), "the bound serial fills the column")
}

func TestGivenABoundSerialWhenAForwardNamesAnotherDeviceThenItFailsButTheOwnSerialIsServed(t *testing.T) {
	// A raw client names the device in the host prefix; adb names it in the select, which the
	// seam checks the same way.
	proxyAddress, _, table := startForwardingProxyWithSerial(t, "R3CN30ABCDE")
	otherSerialClient := dialClient(t, proxyAddress)

	writeAll(t, otherSerialClient, framed("host-serial:other:forward:tcp:0;tcp:9999"))

	want := "FAIL0018device 'other' not found"
	assert.Equal(t, want, string(readN(t, otherSerialClient, len(want))), "the client reads adb's own reason")
	assert.Empty(t, table.List(), "a refused request installs nothing")

	ownSerialClient := dialClient(t, proxyAddress)
	writeAll(t, ownSerialClient, framed("host-serial:R3CN30ABCDE:forward:tcp:0;tcp:9999"))
	assert.NotZero(t, readForwardSuccessPort(t, ownSerialClient), "the attached device's own serial is served")
}

func TestGivenABoundSerialWhenATransportSelectNamesAnotherDeviceThenItIsRefused(t *testing.T) {
	// The select seam takes the same serial, so `adb -s other shell` is refused here
	// instead of being switched to the device the attachment does hold.
	proxyAddress, mock, _ := startForwardingProxyWithSerial(t, "R3CN30ABCDE")
	client := dialClient(t, proxyAddress)

	writeAll(t, client, framed("host:tport:serial:other"))

	want := "FAIL0018device 'other' not found"
	assert.Equal(t, want, string(readN(t, client, len(want))), "a select for another device is refused")
	_, err := client.Read(make([]byte, 1))
	assert.ErrorIs(t, err, io.EOF, "the daemon closes the connection it refused")
	assertNoWebsocketDialed(t, mock)
}

func TestGivenADetachedAttachmentWhenTheForwardFamilyIsAnsweredThenOnlyTransportRequestsAreRefused(t *testing.T) {
	// A detach cannot reach the forwarding a connection already took, and an emptied table
	// still installs, so the seam owes the check. adb with no device refuses what acquires a
	// transport and still answers the two global requests, over nothing.
	noDevices := "FAIL001ano devices/emulators found"
	testCases := []struct {
		name    string
		service string
		want    string
	}{
		{name: "an install", service: "host:forward:tcp:0;tcp:9999", want: noDevices},
		{name: "a malformed install", service: "host:forward:tcp:0", want: noDevices},
		{name: "a killforward", service: "host:killforward:tcp:1", want: noDevices},
		{name: "killforward-all", service: "host:killforward-all", want: "OKAYOKAY"},
		{name: "list-forward", service: "host:list-forward", want: "OKAY0000"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			device, forwarding := forwardingFor(t)
			client := newScriptedConn("")
			device.Clear()

			answered := respondForwardLocally(client, opening{service: testCase.service}, forwarding, "", noopLogger())

			assert.True(t, answered, "the request is answered locally after the detach")
			assert.Equal(t, testCase.want, client.replies.String())
			assert.Empty(t, forwarding.table.List(), "nothing may bind for a device that is gone")
		})
	}
}

func TestGivenNoForwardingWhenAForwardRequestArrivesThenItReachesTheDevice(t *testing.T) {
	// An attachment built without forwarding has no table to answer from, so the request
	// stays on the byte pipe instead of being answered from nothing.
	client, mock := startSelectAwareProxy(t)

	writeAll(t, client, []byte(installForward))

	assertSessionReceived(t, mock, installForward)
}

func TestGivenAReverseRequestWhenForwardingIsOnThenItStaysOnTheBytePipeVerbatim(t *testing.T) {
	// Reverse on (the fixture's setting), so reverse:* is relayed verbatim: its listener lives on the
	// device. The daemon only counts it.
	proxyAddress, mock, _ := startForwardingProxy(t)
	client := dialClient(t, proxyAddress)

	selectAnyTransport(t, client)
	writeAll(t, client, []byte("001breverse:forward:tcp:4;tcp:5"))

	assertSessionReceived(t, mock, canonicalSelect+"001breverse:forward:tcp:4;tcp:5")
}

func TestGivenARequestWithNoHostPrefixWhenForwardingIsOnThenItStaysOnTheBytePipe(t *testing.T) {
	// A service with no prefix at all cannot name the forward family, and a client that
	// sent no select carries no selector either.
	proxyAddress, mock, _ := startForwardingProxy(t)
	client := dialClient(t, proxyAddress)

	writeAll(t, client, []byte("0008shell:ls"))

	assertSessionReceived(t, mock, "0008shell:ls")
}

func TestGivenAnUppercaseHexLengthPrefixWhenInterceptionDeclinesThenTheSessionSeesTheOriginalCase(t *testing.T) {
	// The daemon relays the client's own bytes, never a reframed copy: adb writes lowercase
	// hex, but nothing here may depend on that.
	proxyAddress, mock, _ := startForwardingProxy(t)
	client := dialClient(t, proxyAddress)

	writeAll(t, client, []byte("000Ehost:get-state"))

	assertSessionReceived(t, mock, "000Ehost:get-state")
}
