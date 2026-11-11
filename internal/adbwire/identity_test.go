// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package adbwire

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGivenTheIdentityServiceWhenFramedThenItMatchesTheRequestConstant(t *testing.T) {
	framed, err := EncodeRequest(BridgeIdentityService)
	require.NoError(t, err)
	assert.Equal(t, BridgeIdentityRequest, string(framed))
}

func TestGivenAPidWhenTheIdentityIsWrittenThenTheParserReadsItBack(t *testing.T) {
	var buffer bytes.Buffer

	require.NoError(t, WriteBridgeIdentity(&buffer, 4242))

	assert.Equal(t, "OKAY0022devicefarm-connect bridge pid=4242", buffer.String())
	pid, ok := ParseBridgeIdentity(buffer.String()[len(okayReply)+requestLengthDigits:])
	assert.True(t, ok)
	assert.Equal(t, 4242, pid)
}

func TestGivenAnIdentityPayloadWhenParsedThenOnlyTheExactShapeWithOneToTenDigitsIsAccepted(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantPid int
		wantOK  bool
	}{
		{name: "the exact shape", payload: "devicefarm-connect bridge pid=1", wantPid: 1, wantOK: true},
		{name: "ten digits", payload: "devicefarm-connect bridge pid=2147483647", wantPid: 2147483647, wantOK: true},
		{name: "a leading zero", payload: "devicefarm-connect bridge pid=07"},
		{name: "pid zero", payload: "devicefarm-connect bridge pid=0"},
		{name: "eleven digits", payload: "devicefarm-connect bridge pid=12345678901"},
		{name: "no digits", payload: "devicefarm-connect bridge pid="},
		{name: "a sign", payload: "devicefarm-connect bridge pid=-1"},
		{name: "trailing bytes", payload: "devicefarm-connect bridge pid=12 "},
		{name: "another program", payload: "another-tool bridge pid=12"},
		{name: "an empty payload", payload: ""},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			pid, ok := ParseBridgeIdentity(testCase.payload)

			assert.Equal(t, testCase.wantOK, ok)
			assert.Equal(t, testCase.wantPid, pid)
		})
	}
}
