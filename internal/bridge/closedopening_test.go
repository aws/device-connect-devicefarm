// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

func TestGivenACodedCloseAtTheOpeningWhenAnAdbConnectionOpensThenTheClientReadsTheCodeAndReasonAndTheDeviceStays(t *testing.T) {
	tests := []struct {
		closeCode int
		reason    string
	}{
		{closeCode: 1011, reason: "device host error"},
		{closeCode: 1008, reason: "adb is not supported on this device host"},
		{closeCode: sessionEndedCloseCode, reason: "session terminating"},
	}
	for _, testCase := range tests {
		t.Run(fmt.Sprint(testCase.closeCode), func(t *testing.T) {
			mock := testsupport.NewSessionEchoWSS(t)
			mock.CloseNewConnections(testCase.closeCode, testCase.reason)
			lookup := &fakeLookup{}
			proxyAddress, device := runLookupProxy(t, mock.URL, lookup)

			reply := askHost(t, proxyAddress, string(framed("host:get-state")))

			want := fmt.Sprintf("the server closed the connection: code %d, reason \"%s\"", testCase.closeCode, testCase.reason)
			assert.Equal(t, "FAIL"+string(framed(want)), reply)
			_, present := device.Current()
			assert.True(t, present, "a coded close is no verdict on the session; the next command may work")
			assert.Empty(t, lookup.calls(), "no lookup")
			assert.Equal(t, 1, mock.ConnectionsAccepted(), "no second dial")
		})
	}
}

func TestGivenACloseReasonWithOtherBytesWhenWordedThenEachBecomesAQuestionMark(t *testing.T) {
	tests := map[string]struct{ reason, want string }{
		"a control byte":  {reason: "bad\nline", want: `reason "bad?line"`},
		"a UTF-8 pair":    {reason: "café", want: `reason "caf??"`},
		"an empty reason": {reason: "", want: `reason ""`},
	}
	for name, testCase := range tests {
		t.Run(name, func(t *testing.T) {
			text := closedOpeningText(&wsCloseError{code: 1011, reason: testCase.reason})

			assert.Equal(t, "the server closed the connection: code 1011, "+testCase.want, text)
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
