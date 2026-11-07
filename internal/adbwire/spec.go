// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package adbwire

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// AdbSpec is a forward endpoint specification, "kind:value". adb's model: the
// kind:value shape of socket_spec.cpp. The struct is the daemon's own. The
// failure texts in this file are adb's; the client prints what it is sent.
// The split is purely structural; see AdbForwardRequest for who validates a
// spec.
type AdbSpec struct {
	Kind  string
	Value string
}

// FailUnknownSpec is what adb's listener path reports for a spec with no known
// kind (socket_spec_listen; adb emits no space after that colon).
func FailUnknownSpec(text string) string {
	return fmt.Sprintf("unknown socket specification:%s", text)
}

// ParseSpec splits a spec at its first colon; the kind must be non-empty, the
// value may be empty.
func ParseSpec(text string) (AdbSpec, error) {
	kind, value, found := strings.Cut(text, ":")
	if !found || kind == "" {
		return AdbSpec{}, errors.New(FailUnknownSpec(text))
	}
	return AdbSpec{Kind: kind, Value: value}, nil
}

// String renders the spec back to its wire form.
func (spec AdbSpec) String() string {
	return fmt.Sprintf("%s:%s", spec.Kind, spec.Value)
}

// TCPPort returns the port of a tcp spec. Port 0 is valid: it asks the
// listener to resolve a free port. The texts are adb's (socket_spec.cpp
// parse_tcp_socket_spec, socket_spec_listen): a value that does not parse as
// a C int is read as a hostname, which adb refuses unless it names loopback;
// an int outside 0..65535 is a bad port; an empty value has no host at all.
// The loopback hostname forms ("localhost:<port>", "[::1]:<port>") are not
// mirrored and get the hostname text; the CLI refuses them before sending.
func (spec AdbSpec) TCPPort() (int, error) {
	if spec.Kind != "tcp" {
		// Internal guard; adb's listen path never emits this text.
		return 0, fmt.Errorf("specification is not tcp: %s", spec)
	}
	if spec.Value == "" {
		return 0, errors.New("no host in ''")
	}
	port, ok := parseCInt(spec.Value)
	if !ok {
		return 0, errors.New("listening on specified hostname currently unsupported")
	}
	if port < 0 || port > 65535 {
		return 0, fmt.Errorf("bad port number '%d'", port)
	}
	return int(port), nil
}

// parseCInt reads a port the way adb's libbase ParseInt does: leading blanks
// are skipped, then a "0x" or "0X" prefix selects base 16 and anything else
// is base 10, so a leading zero is not octal. Trailing bytes fail. One byte is
// not mirrored: adb parses a C string, so it stops at an embedded NUL and reads
// "80\x00junk" as 80 where this refuses it. No adb CLI can send one; the spec
// text comes from argv.
func parseCInt(text string) (int64, bool) {
	digits := strings.TrimLeft(text, " \t\n\v\f\r")
	if len(digits) > 1 && digits[0] == '0' && (digits[1] == 'x' || digits[1] == 'X') {
		hexDigits := digits[2:]
		if strings.HasPrefix(hexDigits, "+") || strings.HasPrefix(hexDigits, "-") {
			// strtoll would stop at the sign and leave "x" unconsumed.
			return 0, false
		}
		port, err := strconv.ParseInt(hexDigits, 16, 32)
		return port, err == nil
	}
	port, err := strconv.ParseInt(digits, 10, 32)
	return port, err == nil
}
