// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"context"
	"errors"
	"strings"
)

var errNoDeviceSerial = errors.New("the remote adb server lists no device")

// FetchDeviceSerial dials endpoint, sends one host:devices and returns the first device row's
// serial. The control add calls it before Set, so a bound device always has a serial. ctx
// bounds the dial; ctx and hostReplyTimeout bound the exchange; see fetchHostReply.
func (dialer *WSDialer) FetchDeviceSerial(ctx context.Context, endpoint string) (string, error) {
	reply, err := dialer.fetchHostReply(ctx, endpoint, hostDevicesRequest)
	if err != nil {
		return "", err
	}
	serial := firstDeviceSerial(string(reply[hostReplyHeaderLen:]))
	if serial == "" {
		return "", errNoDeviceSerial
	}
	return serial, nil
}

// firstDeviceSerial returns the serial column of the first "<serial>\t<state>" row of a
// devices payload. A line without a state column is not a device row, so it is skipped
// instead of read as a serial. The proxied session serves exactly one device.
func firstDeviceSerial(payload string) string {
	for _, line := range strings.Split(payload, "\n") {
		serial, state, found := strings.Cut(strings.TrimSpace(line), "\t")
		if found && state != "" {
			return serial
		}
	}
	return ""
}
