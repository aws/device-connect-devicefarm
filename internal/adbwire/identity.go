// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package adbwire

import (
	"io"
	"strconv"
	"strings"
)

// The daemon's own host service, not adb's: a bridge answers it with its pid, so a probe of the
// listen port tells another bridge from an adb server. adb's server answers it with FAIL.

// BridgeIdentityService is the host service a bridge answers with its identity.
const BridgeIdentityService = "host:devicefarm-connect-bridge"

// BridgeIdentityRequest is BridgeIdentityService as EncodeRequest frames it; a test pins the two.
const BridgeIdentityRequest = "001e" + BridgeIdentityService

// MaxBridgeIdentityLen caps the identity payload a probe reads.
const MaxBridgeIdentityLen = 64

// bridgeIdentityLead opens the identity payload; the pid follows it in decimal.
const bridgeIdentityLead = "devicefarm-connect bridge pid="

// maxPidDigits bounds the pid an identity payload may carry.
const maxPidDigits = 10

// WriteBridgeIdentity answers BridgeIdentityService in one write: OKAY, then
// "devicefarm-connect bridge pid=<pid>" as a protocol string. The caller closes the connection.
func WriteBridgeIdentity(writer io.Writer, pid int) error {
	return writeFramed(writer, okayReply, bridgeIdentityLead+strconv.Itoa(pid))
}

// ParseBridgeIdentity reads an identity payload. ok only for the exact shape WriteBridgeIdentity
// writes: 1 to 10 decimal digits, no leading zero, nothing after them. The pid came from a socket
// any local program can hold: print it, never act on it.
func ParseBridgeIdentity(payload string) (pid int, ok bool) {
	digits, found := strings.CutPrefix(payload, bridgeIdentityLead)
	if !found || digits == "" || digits[0] == '0' || len(digits) > maxPidDigits ||
		strings.Trim(digits, "0123456789") != "" {
		return 0, false
	}
	pid, err := strconv.Atoi(digits)
	if err != nil {
		return 0, false
	}
	return pid, true
}
