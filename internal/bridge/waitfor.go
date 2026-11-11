// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"context"
	"log/slog"
	"net"
	"time"

	"github.com/aws/device-connect-devicefarm/internal/adbwire"
	"github.com/aws/device-connect-devicefarm/internal/forward"
)

// serveWaitFor answers a lone wait-for request that names no attached device, as adb's wait
// service does: OKAY at once, a hold until a device it names attaches, then OKAY and the close. A
// spec that lists disconnect returns at once; a malformed spec gets adb's FAIL after the first
// OKAY. The device named is the request's serial or transport id prefix, else the spec's
// transport word. attached is the device at accept; nil means none.
//
// ctx is the daemon's: a detach cancels the connection's, and the wait outlives it. The hold ends
// with no more bytes when the client closes or writes, or when the daemon stops. The caller
// closes tcp.
//
// served false: not a lone wait-for, or attached is the device it names. The request is relayed
// and the server judges the device's state.
func serveWaitFor(ctx context.Context, tcp net.Conn, clientOpening opening, attached *DeviceInfo,
	device *ActiveDevice, logger *slog.Logger) (served bool) {
	if clientOpening.selected {
		return false
	}
	prefix, service, ok := adbwire.SplitHostPrefix(clientOpening.service)
	if !ok {
		return false
	}
	wait, isWaitFor, failReason := adbwire.ParseWaitFor(service)
	if !isWaitFor {
		return false
	}
	// As adb's transport lookup: a serial or a nonzero transport id outranks the transport word.
	selector := adbwire.AdbDeviceSelector{Kind: wait.Kind}
	if prefix.Kind == adbwire.AdbSelectorSerial || (prefix.Kind == adbwire.AdbSelectorTransportID && prefix.TransportID != 0) {
		selector = prefix
	}
	if failReason == "" && attached != nil && namesDevice(selector, *attached) {
		return false
	}
	if !writeWaitStatus(tcp, logger) {
		return true
	}
	if failReason != "" {
		refuseOpening(tcp, failReason, "could not refuse a wait-for request", logger)
		return true
	}
	// Bytes behind the request end the wait as a later write does; the seam already read them.
	if len(clientOpening.pending) != replyLengthDigits+len(clientOpening.service) {
		return true
	}

	holdCtx, endHold := context.WithCancel(ctx)
	defer endHold()
	// The hold's end closes the socket under the read below.
	defer closeOnCancel(holdCtx, tcp)()
	go func() {
		_, _ = tcp.Read(make([]byte, 1))
		endHold()
	}()
	logger.Debug("holding a wait-for request", "service", clientOpening.service)
	for {
		// The signal first: a Set or clear after the snapshot closes it.
		_, _, changed := device.snapshot()
		info, present := device.Current()
		named := present && namesDevice(selector, info)
		if named && wait.Online || !named && wait.Disconnect {
			writeWaitStatus(tcp, logger)
			return true
		}
		select {
		case <-changed:
		case <-holdCtx.Done():
			return true
		}
	}
}

// namesDevice reports whether selector names info's device (forward.SelectorMiss).
func namesDevice(selector adbwire.AdbDeviceSelector, info DeviceInfo) bool {
	_, missed := forward.SelectorMiss(selector, info.Serial, syntheticTransportID)
	return !missed
}

// writeWaitStatus writes one OKAY of a wait-for answer. No pump watches this socket, so the
// deadline stops a client that stops reading. false: the write failed.
func writeWaitStatus(tcp net.Conn, logger *slog.Logger) bool {
	_ = tcp.SetWriteDeadline(time.Now().Add(openingRequestTimeout))
	if err := adbwire.WriteOkay(tcp); err != nil {
		logger.Debug("could not answer a wait-for request", "error", err)
		return false
	}
	return true
}
