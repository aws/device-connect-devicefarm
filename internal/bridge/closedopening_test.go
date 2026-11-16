// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

func TestGivenACodedCloseAtTheOpeningWhenAnAdbConnectionOpensThenTheClientReadsTheReasonAndCodeAndTheDeviceStays(t *testing.T) {
	tests := []struct {
		closeCode int
		reason    string
		want      string
	}{
		{closeCode: 1011, reason: "An internal error has occurred",
			want: "Device Farm closed the connection to the device: An internal error has occurred (close code 1011)"},
		{closeCode: 1008, reason: "Device Connect is not supported on this device",
			want: "Device Farm refused the connection to the device: Device Connect is not supported on this device (close code 1008)"},
		{closeCode: sessionEndedCloseCode, reason: "session terminating",
			want: "Device Farm refused the connection to the device: session terminating (close code 4000)"},
		{closeCode: 1008, reason: "", want: "Device Farm closed the connection to the device (close code 1008)"},
	}
	for _, testCase := range tests {
		t.Run(testCase.want, func(t *testing.T) {
			mock := testsupport.NewSessionEchoWSS(t)
			mock.CloseNewConnections(testCase.closeCode, testCase.reason)
			lookup := &fakeLookup{}
			proxyAddress, device := runLookupProxy(t, mock.URL, lookup)

			reply := askHost(t, proxyAddress, string(framed("host:get-state")))

			assert.Equal(t, "FAIL"+string(framed(testCase.want)), reply)
			_, present := device.Current()
			assert.True(t, present, "a coded close is no verdict on the session; the next command may work")
			assert.Empty(t, lookup.calls(), "no lookup")
			assert.Equal(t, 1, mock.ConnectionsAccepted(), "no second dial")
		})
	}
}

func TestGivenAnAbruptCloseAtTheOpeningWhenAnAdbConnectionOpensThenTheClientGetsTheCloseAlone(t *testing.T) {
	// No close frame, so no code to print.
	mock := testsupport.NewSessionEchoWSS(t)
	mock.CloseNewConnections(testsupport.NoCloseFrame, "")
	lookup := &fakeLookup{}
	proxyAddress, device := runLookupProxy(t, mock.URL, lookup)

	reply := askHost(t, proxyAddress, string(framed("host:get-state")))

	assert.Empty(t, reply)
	_, present := device.Current()
	require.True(t, present)
	assert.Empty(t, lookup.calls(), "no lookup")
	assert.Equal(t, 1, mock.ConnectionsAccepted(), "no second dial")
}
