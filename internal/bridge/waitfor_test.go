// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	waitForDevice = "host:wait-for-any-device"
	waitSerial    = "WAIT-SERIAL-0001"
)

// startWaitingClient runs a proxy with no device attached, sends request and reads the first
// OKAY, returning the held client and the device to attach to.
func startWaitingClient(t *testing.T, request string) (client net.Conn, device *ActiveDevice) {
	t.Helper()
	device = NewActiveDevice()
	proxyAddress, _ := runProxy(t, device, testPolicy())
	client = sendOnly(t, proxyAddress, string(framed(request)))
	require.Equal(t, statusOkay, string(readN(t, client, statusWordLen)), "the first status goes out at once")
	return client, device
}

// attachSerial binds a device with serial, as a device add does.
func attachSerial(t *testing.T, device *ActiveDevice, serial string) {
	t.Helper()
	require.True(t, device.Set(DeviceInfo{ARN: "arn:wait", Endpoint: "wss://127.0.0.1:1", Label: "Wait Device", Serial: serial}))
}

// assertSecondOkayThenClose reads the second status and the close that must follow it, or adb hangs.
func assertSecondOkayThenClose(t *testing.T, client net.Conn) {
	t.Helper()
	assert.Equal(t, statusOkay, string(drainClosed(t, client)), "the second status, then the close")
}

func TestGivenNoDeviceWhenWaitForDeviceArrivesThenOkayIsWrittenAndTheConnectionIsHeld(t *testing.T) {
	client, _ := startWaitingClient(t, waitForDevice)

	assertHeldQuiet(t, client)
}

func TestGivenAHeldWaitWhenADeviceAttachesThenASecondOkayAndTheCloseFollow(t *testing.T) {
	client, device := startWaitingClient(t, waitForDevice)
	assertHeldQuiet(t, client)

	attachSerial(t, device, waitSerial)

	assertSecondOkayThenClose(t, client)
}

func TestGivenNoDeviceWhenWaitForDisconnectArrivesThenOkayOkayAndTheCloseComeAtOnce(t *testing.T) {
	proxyAddress, _ := runProxy(t, NewActiveDevice(), testPolicy())

	assert.Equal(t, statusOkay+statusOkay, askHost(t, proxyAddress, string(framed("host:wait-for-any-disconnect"))))
}

func TestGivenAWaitForASerialWhenADeviceWithAnotherSerialIsAttachedThenItIsHeldUntilThatSerialAttaches(t *testing.T) {
	// adb waits for -s S while another device is attached; the attached one does not count.
	device := NewActiveDevice()
	attachSerial(t, device, "OTHER-SERIAL")
	proxyAddress, _ := runProxy(t, device, testPolicy())
	client := sendOnly(t, proxyAddress, string(framed("host-serial:"+waitSerial+":wait-for-any-device")))
	require.Equal(t, statusOkay, string(readN(t, client, statusWordLen)), "the first status goes out at once")

	device.Clear()
	assertHeldQuiet(t, client)
	attachSerial(t, device, waitSerial)

	assertSecondOkayThenClose(t, client)
}

func TestGivenAHeldWaitForATransportIdWhenThatIdAttachesThenItReturns(t *testing.T) {
	client, device := startWaitingClient(t, "host-transport-id:1:wait-for-any-device")

	attachSerial(t, device, waitSerial)

	assertSecondOkayThenClose(t, client)
}

func TestGivenTransportIdZeroWhenAWaitForAKindArrivesThenTheSpecsKindStillDecides(t *testing.T) {
	// adb reads id 0 as unset, so the spec's transport word still filters the device.
	client, device := startWaitingClient(t, "host-transport-id:0:wait-for-local-device")

	attachSerial(t, device, waitSerial)
	assertHeldQuiet(t, client)
	device.Clear()
	attachSerial(t, device, "emulator-5554")

	assertSecondOkayThenClose(t, client)
}

func TestGivenAWaitForARecoveryStateWhenADeviceAttachesThenItStaysHeld(t *testing.T) {
	// The daemon's device is always in the device state, so no other state ever matches.
	client, device := startWaitingClient(t, "host:wait-for-any-recovery")

	attachSerial(t, device, waitSerial)

	assertHeldQuiet(t, client)
}

func TestGivenAMalformedWaitForWhenItArrivesThenOkayThenAdbsFailText(t *testing.T) {
	tests := []struct {
		request    string
		failReason string
	}{
		{request: "host:wait-for-any", failReason: "short wait-for-: any"},
		{request: "host:wait-for-bogus-device", failReason: "bad wait-for- transport: bogus-device"},
		{request: "host:wait-for-any-bogus", failReason: "bad wait-for- state: any-bogus"},
	}
	for _, testCase := range tests {
		for _, withDevice := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, device attached %t", testCase.request, withDevice), func(t *testing.T) {
				// adb parses the spec before it looks for a device, so an attached one changes nothing.
				device := NewActiveDevice()
				if withDevice {
					attachSerial(t, device, waitSerial)
				}
				proxyAddress, _ := runProxy(t, device, testPolicy())

				want := statusOkay + "FAIL" + string(framed(testCase.failReason))
				assert.Equal(t, want, askHost(t, proxyAddress, string(framed(testCase.request))))
			})
		}
	}
}

func TestGivenBytesBehindAWaitForRequestWhenItArrivesThenOkayAndTheCloseFollowWithNoHold(t *testing.T) {
	proxyAddress, _ := runProxy(t, NewActiveDevice(), testPolicy())

	assert.Equal(t, statusOkay, askHost(t, proxyAddress, string(framed(waitForDevice))+"x"),
		"a client that writes ends the wait, also when the bytes came with the request")
}

func TestGivenAWaitForAnotherTransportIdWhenTheDeviceIsAttachedThenItStaysHeld(t *testing.T) {
	device := NewActiveDevice()
	attachSerial(t, device, waitSerial)
	proxyAddress, _ := runProxy(t, device, testPolicy())
	client := sendOnly(t, proxyAddress, string(framed("host-transport-id:2:wait-for-any-device")))
	require.Equal(t, statusOkay, string(readN(t, client, statusWordLen)), "the first status goes out at once")

	assertHeldQuiet(t, client)
}

func TestGivenAHeldWaitWhenTheClientClosesThenTheHandlerReturns(t *testing.T) {
	clientSide, daemonSide := localTCPPair(t)
	served := make(chan bool, 1)
	go func() {
		lone := opening{service: waitForDevice, pending: framed(waitForDevice)}
		served <- serveWaitFor(context.Background(), daemonSide, lone, nil, NewActiveDevice(), noopLogger())
	}()
	require.Equal(t, statusOkay, string(readN(t, clientSide, statusWordLen)))

	require.NoError(t, clientSide.Close())

	select {
	case result := <-served:
		assert.True(t, result, "a held wait is served, whatever ends it")
	case <-time.After(testWait):
		t.Fatal("the hold did not end when the client closed")
	}
}

func TestGivenAHeldWaitWhenTheDaemonStopsThenTheConnectionClosesAndTheDrainEnds(t *testing.T) {
	server := newProxyServer(t, 0, testPolicy(), newTestDialer(testWait), NewActiveDevice())
	daemonCtx, stopDaemon := context.WithCancel(context.Background())
	defer stopDaemon()
	stopped := make(chan error, 1)
	go func() { stopped <- server.Run(daemonCtx) }()
	client := sendOnly(t, waitForAddr(t, server), string(framed(waitForDevice)))
	require.Equal(t, statusOkay, string(readN(t, client, statusWordLen)))

	stopDaemon()

	assert.Empty(t, drainClosed(t, client), "the stop ends the hold with no more bytes")
	select {
	case err := <-stopped:
		assert.NoError(t, err)
	case <-time.After(testWait):
		t.Fatal("a held wait kept the daemon from draining")
	}
}

func TestGivenAnAttachedDeviceWhenWaitForDeviceArrivesThenItIsRelayedToTheServer(t *testing.T) {
	client, mock := startSelectAwareProxy(t)

	writeAll(t, client, framed(waitForDevice))

	assertSessionReceived(t, mock, string(framed(waitForDevice)))
	assert.Equal(t, string(framed(waitForDevice)), string(readN(t, client, len(framed(waitForDevice)))),
		"the server's reply, here the mock's echo, is the client's; no local status went first")
}
