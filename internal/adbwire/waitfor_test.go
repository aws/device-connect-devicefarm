// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package adbwire

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGivenWaitForSpecsWhenParsedThenTheTransportWordAndTheStatesAreRead(t *testing.T) {
	tests := []struct {
		service string
		want    AdbWaitFor
	}{
		{service: "wait-for-any-device", want: AdbWaitFor{Kind: AdbSelectorAny, Online: true}},
		{service: "wait-for-usb-device", want: AdbWaitFor{Kind: AdbSelectorUSB, Online: true}},
		{service: "wait-for-local-any", want: AdbWaitFor{Kind: AdbSelectorLocal, Online: true}},
		{service: "wait-for-any-disconnect", want: AdbWaitFor{Kind: AdbSelectorAny, Disconnect: true}},
		{service: "wait-for-any-recovery", want: AdbWaitFor{Kind: AdbSelectorAny}},
		{service: "wait-for-any-device-disconnect", want: AdbWaitFor{Kind: AdbSelectorAny, Online: true, Disconnect: true}},
	}
	for _, testCase := range tests {
		t.Run(testCase.service, func(t *testing.T) {
			wait, isWaitFor, failReason := ParseWaitFor(testCase.service)

			assert.True(t, isWaitFor)
			assert.Empty(t, failReason)
			assert.Equal(t, testCase.want, wait)
		})
	}
}

func TestGivenMalformedWaitForSpecsWhenParsedThenAdbsFailTextsAreReturned(t *testing.T) {
	tests := []struct {
		service    string
		failReason string
	}{
		{service: "wait-for-any", failReason: "short wait-for-: any"},
		{service: "wait-for-bogus-device", failReason: "bad wait-for- transport: bogus-device"},
		{service: "wait-for-any-bogus", failReason: "bad wait-for- state: any-bogus"},
		{service: "wait-for-bogus", failReason: "short wait-for-: bogus"},
	}
	for _, testCase := range tests {
		t.Run(testCase.service, func(t *testing.T) {
			_, isWaitFor, failReason := ParseWaitFor(testCase.service)

			assert.True(t, isWaitFor)
			assert.Equal(t, testCase.failReason, failReason)
		})
	}
}

func TestGivenAServiceThatIsNotAWaitForWhenParsedThenItIsDeclined(t *testing.T) {
	for _, service := range []string{"get-state", "wait-for", "track-devices", "shell:wait-for-any-device"} {
		t.Run(service, func(t *testing.T) {
			_, isWaitFor, failReason := ParseWaitFor(service)

			assert.False(t, isWaitFor)
			assert.Empty(t, failReason)
		})
	}
}
