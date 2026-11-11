// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// endedReason stands in for the daemon's reason after a session it could no longer resolve.
const endedReason = "remote access session arn:gone has ended (COMPLETED); start a new session"

// detachedDevice is a device the daemon attached, then detached itself with reason.
func detachedDevice(t *testing.T, reason string) *ActiveDevice {
	t.Helper()
	device := NewActiveDevice()
	require.True(t, device.Set(DeviceInfo{ARN: "arn:gone", Endpoint: "wss://127.0.0.1:1", Label: "Gone Device"}))
	_, detached := device.ClearMatching("arn:gone", reason)
	require.True(t, detached)
	return device
}

func TestGivenADaemonMadeDetachWhenARequestMissesThenTheFailIsAdbsTextThenTheReason(t *testing.T) {
	// Both FAIL sites: a select the seam refuses, and a lone request no device serves.
	tests := []struct {
		name    string
		request string
		adbText string
	}{
		{name: "a lone host request", request: "host:get-state", adbText: "no devices/emulators found"},
		{name: "the features probe", request: "host:features", adbText: "no devices/emulators found"},
		{name: "a serial prefix", request: "host-serial:dev-1:get-state", adbText: "device 'dev-1' not found"},
		{name: "a bare device service", request: "shell:true", adbText: "no devices/emulators found"},
		{name: "an any select", request: "host:tport:any", adbText: "no devices/emulators found"},
		{name: "a usb select", request: "host:tport:usb", adbText: "no devices found"},
		{name: "a local select", request: "host:transport-local", adbText: "no emulators found"},
		{name: "a serial select", request: "host:transport:dev-1", adbText: "device 'dev-1' not found"},
		{name: "a transport id select", request: "host:transport-id:7", adbText: "no device with transport id '7'"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			proxyAddress, _ := runProxy(t, detachedDevice(t, endedReason), testPolicy())

			want := "FAIL" + string(framed(testCase.adbText+"; "+endedReason))
			assert.Equal(t, want, askHost(t, proxyAddress, string(framed(testCase.request))))
		})
	}
}

func TestGivenAUserRemoveAndNoDaemonMadeDetachWhenARequestMissesThenTheFailIsAdbsTextAlone(t *testing.T) {
	device := NewActiveDevice()
	require.True(t, device.Set(DeviceInfo{ARN: "arn:removed", Endpoint: "wss://127.0.0.1:1", Label: "Removed Device"}))
	_, removed := device.Clear()
	require.True(t, removed)
	proxyAddress, _ := runProxy(t, device, testPolicy())

	assert.Equal(t, "FAIL"+string(framed("no devices/emulators found")), askHost(t, proxyAddress, string(framed("host:get-state"))))
}

func TestGivenADetachRecordWhenAnyDeviceIsAttachedThenTheReasonIsGone(t *testing.T) {
	device := detachedDevice(t, endedReason)
	require.True(t, device.Set(DeviceInfo{ARN: "arn:other", Endpoint: "wss://127.0.0.1:1", Label: "Other Device"}))
	device.Clear()

	assert.Equal(t, "no devices/emulators found", device.withDetachReason("no devices/emulators found"),
		"the next attach of any session clears the record, and a user remove writes none")
}

func TestGivenADetachRecordWhenBytesFormNoRequestThenTheConnectionClosesWithNoReply(t *testing.T) {
	proxyAddress, _ := runProxy(t, detachedDevice(t, endedReason), testPolicy())

	assert.Empty(t, askHost(t, proxyAddress, "hello world"), "no reply shape is known for bytes that are no request")
}

func TestGivenAMissTextNearTheFailCapWhenDecoratedThenAdbsTextIsKeptWhole(t *testing.T) {
	// adb's FAIL length is four hex digits; a joined line past it would write nothing at all.
	device := detachedDevice(t, endedReason)
	longText := "device '" + strings.Repeat("s", math.MaxUint16-20) + "' not found"

	assert.Equal(t, longText, device.withDetachReason(longText), "past the cap adb's text stands alone")
	assert.Equal(t, "no devices found; "+endedReason, device.withDetachReason("no devices found"))
}
