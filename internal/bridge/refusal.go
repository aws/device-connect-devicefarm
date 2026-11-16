// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/aws/device-connect-devicefarm/internal/wording"
)

// A refused fresh session or a failed dial asks the daemon about the session before the client
// gets an answer. The bridge applies the verdict and dials once more when the device stays.

// SessionVerdict is the daemon's answer about a device's session after a refused fresh adb
// connection. A RUNNING session carries its renewed Endpoint and Label. A session that is gone
// for good sets Detach, with the one-line Reason the user gets. Neither: a transient failure of
// the lookup, so the device stays as it is.
type SessionVerdict struct {
	Endpoint string
	Label    string
	Detach   bool
	Reason   string
}

// RefusalHandler looks up the session named by sessionARN. The daemon supplies it. It runs
// under the daemon context with its own timeout, so one refusal's detach cannot cut a later
// lookup short. The gate runs calls one at a time per attachment.
type RefusalHandler func(sessionARN string) SessionVerdict

// refusalGate runs one attachment's lookups one at a time and applies each verdict to the device.
// Nil, or a nil handler, consults nothing: the connection just closes.
type refusalGate struct {
	handler RefusalHandler
	device  *ActiveDevice
	mutex   sync.Mutex
}

// consult looks the session up under the gate's mutex and applies the verdict: a RUNNING answer
// renews the endpoint, a dead session detaches with its reason. retry: the caller may dial once
// more. reason: set only on a detach.
func (gate *refusalGate) consult(arn string) (retry bool, reason string) {
	if gate == nil || gate.handler == nil {
		return false, ""
	}
	gate.mutex.Lock()
	defer gate.mutex.Unlock()
	verdict := gate.handler(arn)
	if verdict.Detach {
		gate.device.ClearMatching(arn, verdict.Reason)
		return false, verdict.Reason
	}
	if verdict.Endpoint != "" {
		gate.device.UpdateEndpoint(arn, verdict.Endpoint, verdict.Label)
	}
	return true, ""
}

// sessionRefusal is the server's ERROR to a fresh session's first frame. It matches
// ErrSessionRefused, and keeps the reason apart for the text an adb client gets.
type sessionRefusal struct {
	reason string
}

func (refusal *sessionRefusal) Error() string {
	return ErrSessionRefused.Error() + ": " + refusal.reason
}

func (*sessionRefusal) Is(target error) bool { return target == ErrSessionRefused }

// dialFailure marks a failed dial of the adb route: openSession tells it from a failed exchange,
// and the warm socket keeper from a refused pre-select. Its text is the dial's own.
type dialFailure struct {
	cause error
}

func (failure *dialFailure) Error() string { return failure.cause.Error() }

func (failure *dialFailure) Unwrap() error { return failure.cause }

// isRefusedOpening reports whether err is one of the two failures that ask the daemon: the dial
// failed, or the server refused the session.
func isRefusedOpening(err error) bool {
	var failure *dialFailure
	return errors.Is(err, ErrSessionRefused) || errors.As(err, &failure)
}

// sessionOpener dials endpoint and plays one connection's opening on the fresh session. useParked
// allows the attachment's parked websocket in place of a dial.
type sessionOpener func(endpoint string, useParked bool) (*adbConn, error)

// openSession opens a fresh session at the device's current endpoint. A refusal that leaves the
// device attached earns one more dial, a fresh one: a socket parked during the lookup was dialed
// at the old endpoint. clientReason is the adb client's one line: the detach reason, else the
// second refusal's text, else a coded close's cause, else empty. A canceled ctx asks nothing.
func openSession(ctx context.Context, device *ActiveDevice, arn string, gate *refusalGate, open sessionOpener,
	logger *slog.Logger) (conn *adbConn, clientReason string, err error) {
	for attempt := 1; ; attempt++ {
		endpoint, attached := device.EndpointFor(arn)
		if !attached {
			return nil, "", fmt.Errorf("%w: the device detached before the dial", errAdbConnClosed)
		}
		conn, err = open(endpoint, attempt == 1)
		if err == nil {
			return conn, "", nil
		}
		// The detach cancels ctx, so check the stopping close before any ctx check.
		if reason, stopping := device.detachIfSessionStopping(arn, err); stopping {
			return nil, reason, err
		}
		if ctx.Err() != nil {
			return nil, "", err
		}
		if !isRefusedOpening(err) {
			// Any other coded close before the first reply: adb reads its cause, the device stays.
			failed := SortConnectionError(err, arn)
			if failed.Kind == FailureServiceRefused || failed.Kind == FailureServiceClosed {
				return nil, failed.Cause, err
			}
			return nil, "", err
		}
		if attempt > 1 {
			return nil, SortConnectionError(err, arn).refusalText(), err
		}
		retry, reason := gate.consult(arn)
		if !retry {
			return nil, reason, err
		}
		logger.Debug("the device session was refused; dialing once more", "error", err)
	}
}

// refusalText is what an adb client reads after a refused opening: the server's own reason, or
// the dial's cause without the route and host that Dial names.
func (failed ConnectionFailure) refusalText() string {
	if failed.Kind == FailureSessionRefused {
		return wording.AdbFailRefusedLead + failed.detail
	}
	return wording.AdbFailUnreachableLead + failed.detail
}
