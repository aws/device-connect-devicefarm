// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package adbwire

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGivenForwardFamilyServicesWhenParsedThenRequestReturned(t *testing.T) {
	tests := []struct {
		name    string
		service string
		request AdbForwardRequest
	}{
		{
			name:    "forward",
			service: "forward:tcp:1234;tcp:9090",
			request: AdbForwardRequest{Action: AdbActionForward, Local: "tcp:1234", Remote: "tcp:9090"},
		},
		{
			name:    "forward with norebind",
			service: "forward:norebind:tcp:1234;tcp:9090",
			request: AdbForwardRequest{Action: AdbActionForward, NoRebind: true, Local: "tcp:1234", Remote: "tcp:9090"},
		},
		{
			name:    "forward to a named target",
			service: "forward:tcp:9222;localabstract:chrome_devtools_remote",
			request: AdbForwardRequest{Action: AdbActionForward, Local: "tcp:9222", Remote: "localabstract:chrome_devtools_remote"},
		},
		{
			name:    "forward strips one norebind only",
			service: "forward:norebind:norebind:tcp:1;tcp:2",
			request: AdbForwardRequest{Action: AdbActionForward, NoRebind: true, Local: "norebind:tcp:1", Remote: "tcp:2"},
		},
		{
			name:    "forward to a remote with a star inside",
			service: "forward:tcp:1;localabstract:a*b",
			request: AdbForwardRequest{Action: AdbActionForward, Local: "tcp:1", Remote: "localabstract:a*b"},
		},
		{
			name:    "killforward",
			service: "killforward:tcp:1234",
			request: AdbForwardRequest{Action: AdbActionKillForward, Local: "tcp:1234"},
		},
		{
			name:    "killforward does not strip norebind",
			service: "killforward:norebind:tcp:1",
			request: AdbForwardRequest{Action: AdbActionKillForward, Local: "norebind:tcp:1"},
		},
		{
			name:    "killforward-all",
			service: "killforward-all",
			request: AdbForwardRequest{Action: AdbActionKillForwardAll},
		},
		{
			name:    "list-forward",
			service: "list-forward",
			request: AdbForwardRequest{Action: AdbActionListForward},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			request, found, err := ParseForwardService(testCase.service)

			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, testCase.request, request)
		})
	}
}

func TestGivenMalformedForwardServicesWhenParsedThenAdbExactFailureText(t *testing.T) {
	// The failure text is what adb sends after stripping the forward: and
	// norebind: prefixes (adb.cpp handle_forward_request).
	tests := []struct {
		name    string
		service string
		failure string
	}{
		{name: "forward alone keeps the trailing space", service: "forward:", failure: "bad forward: "},
		{name: "forward norebind alone", service: "forward:norebind:", failure: "bad forward: "},
		{name: "forward missing remote", service: "forward:tcp:1234", failure: "bad forward: tcp:1234"},
		{name: "forward lone separator", service: "forward:;", failure: "bad forward: ;"},
		{name: "forward trailing separator", service: "forward:tcp:1;tcp:2;", failure: "bad forward: tcp:1;tcp:2;"},
		{name: "forward to a bare star", service: "forward:tcp:1234;*", failure: "bad forward: tcp:1234;*"},
		{name: "forward empty local", service: "forward:;tcp:9090", failure: "bad forward: ;tcp:9090"},
		{name: "forward empty remote", service: "forward:tcp:1234;", failure: "bad forward: tcp:1234;"},
		{name: "forward three pieces", service: "forward:tcp:1;tcp:2;tcp:3", failure: "bad forward: tcp:1;tcp:2;tcp:3"},
		{name: "forward to a smart-socket listener", service: "forward:tcp:1234;*smartsocket*", failure: "bad forward: tcp:1234;*smartsocket*"},
		{name: "forward norebind strips both prefixes", service: "forward:norebind:tcp:1234", failure: "bad forward: tcp:1234"},
		{name: "killforward with two pieces", service: "killforward:tcp:1;tcp:2", failure: "bad killforward: tcp:1;tcp:2"},
		{name: "killforward empty", service: "killforward:", failure: "bad killforward: "},
		{name: "killforward trailing separator", service: "killforward:tcp:1;", failure: "bad killforward: tcp:1;"},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			_, found, err := ParseForwardService(testCase.service)

			require.True(t, found)
			require.Error(t, err)
			assert.Equal(t, testCase.failure, err.Error())
		})
	}
}

func TestGivenNonForwardServicesWhenParsedThenNotFound(t *testing.T) {
	// Everything else stays byte-blind; in particular the reverse family is
	// the server's to intercept, never the daemon's.
	services := []string{
		"version",
		"devices",
		"tport:any",
		"transport-any", // an older select spelling; not a forward service
		"tport:serial:R3CN30XXXX",
		"reverse:forward:tcp:1;tcp:2",
		"reverse:killforward:tcp:1",
		"list-forward-x",
		"list-forward:",
		"killforward-all:",
		"forward",
		"killforward",
		"Forward:tcp:1;tcp:2",
		"host:forward:tcp:1;tcp:2", // host prefix not yet split off
	}

	for _, service := range services {
		_, found, err := ParseForwardService(service)
		require.NoError(t, err, "service %q", service)
		assert.False(t, found, "service %q", service)
	}
}
