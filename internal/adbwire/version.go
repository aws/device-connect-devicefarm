// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package adbwire

import (
	"fmt"
	"strconv"
)

// HostVersionRequest is the framed host:version service, which an adb server answers itself
// (adb.cpp handle_host_request), as EncodeRequest frames it; a test pins the two.
const HostVersionRequest = "000chost:version"

// HostKillRequest is the framed host:kill service 'adb kill-server' sends, as EncodeRequest frames
// it; a test pins the two. The server answers OKAY and exits, or FAIL when started to reject kills
// (adb.cpp handle_host_request).
const HostKillRequest = "0009host:kill"

// HostVersionReplyLen is the byte count after OKAY in the reply: a length prefix of 0004 and
// four hex digits.
const HostVersionReplyLen = 8

// ParseHostVersionReply reads the bytes after OKAY and returns the version as the decimal
// number adb prints after "1.0." (0029 is 41). Anything but 0004 plus four hex digits fails.
func ParseHostVersionReply(reply []byte) (int, error) {
	if len(reply) < HostVersionReplyLen {
		return 0, fmt.Errorf("host:version reply has %d bytes after OKAY, not %d", len(reply), HostVersionReplyLen)
	}
	length, digits := reply[:4], reply[4:HostVersionReplyLen]
	if string(length) != "0004" {
		return 0, fmt.Errorf("host:version reply has length %q, not 0004", length)
	}
	version, err := strconv.ParseUint(string(digits), 16, 16)
	if err != nil {
		return 0, fmt.Errorf("host:version reply carries %q, not four hex digits", digits)
	}
	return int(version), nil
}
