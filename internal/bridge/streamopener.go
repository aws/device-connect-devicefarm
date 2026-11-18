// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/aws/device-connect-devicefarm/internal/adbwire"
)

// deviceStreamOpener turns each connection a forward listener accepts into its own adb
// session on the device. One per attachment, holding its ARN, context and stream budget,
// so a stream never reaches a device attached later, never outlives its attachment, and
// never spends another attachment's slot.
type deviceStreamOpener struct {
	device   *ActiveDevice
	cfg      *featureConfig
	arn      string
	ctx      context.Context
	budget   *streamBudget       // the attachment's; see streamBudget
	features *attachmentFeatures // the attachment's, for its parked websocket and its refusal gate

	refusals refusalThrottle
}

// OpenStream carries one accepted forward connection onto its own adb session. It owns
// conn and closes it, and it runs on the accept loop's per-connection goroutine, so it
// blocks for the life of the stream.
func (opener *deviceStreamOpener) OpenStream(conn net.Conn, target adbwire.AdbSpec) {
	defer conn.Close()
	logger := opener.cfg.logger.With("client", conn.RemoteAddr().String(), "target", target.String())

	// Budget first: a refusal costs no lock, no dial, no byte.
	if !opener.budget.admit() {
		if opener.refusals.allow(time.Now()) {
			logger.Warn("refused a forwarded connection: every device-stream slot is taken",
				"maxDeviceStreams", opener.budget.limit)
		}
		return
	}
	defer opener.budget.release()

	// The attachment's context is what severs a live stream on a detach. Registering this
	// stream's cancel buys the locked presence read: it refuses a connection the accept loop
	// handed off before a detach closed the listener, rather than opening a stream to a
	// device that is gone. The device and its forwarding are this opener's own, so neither
	// is read back here.
	streamCtx, cancel := context.WithCancel(opener.ctx)
	defer cancel()
	_, _, untrack, ok := opener.device.trackConn(cancel)
	if !ok {
		logger.Debug("closed a forwarded connection: no device is attached")
		return
	}
	defer untrack()

	activity := newActivityTracker(nil)
	stream, leftover, err := openDeviceStream(streamCtx, opener.cfg.dialer, opener.device, opener.arn, opener.features,
		target, opener.cfg.policy, opener.cfg.params.DeviceStreamStatusTimeout, activity, logger)
	if err != nil {
		// Zero bytes written either way, so the application sees a refused port rather
		// than a peer that hung up mid-conversation.
		var refusal *statusFailure
		if errors.As(err, &refusal) {
			// Routine and retried in a loop: an application asking for a device port
			// nothing listens on.
			logger.Warn("adb refused a forwarded device port", "reason", refusal.reason)
			return
		}
		logger.Error("failed to open a forwarded device stream", "error", err)
		return
	}
	defer stream.Close()

	// The last status word may have carried the application's first bytes. They go out
	// before the pipe's first read can get ahead of them.
	if writeErr := writeWithin(conn, leftover, opener.cfg.params.DeviceStreamStatusTimeout); writeErr != nil {
		logger.Error("failed to deliver a forwarded stream's first bytes", "error", writeErr)
		return
	}

	refresher := func(done <-chan struct{}) { runRefresher(streamCtx, stream, opener.cfg.policy, done) }
	if err := pipeConn(streamCtx, conn, stream, refresher); err != nil {
		logger.Error("forwarded stream ended with an error", "error", err)
		return
	}
	logger.Debug("forwarded stream closed")
}
