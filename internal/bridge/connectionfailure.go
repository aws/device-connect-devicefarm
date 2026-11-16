// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/aws/device-connect-devicefarm/internal/wording"
)

// FailureKind names what failed on a connection to a device, so a caller can pick a remedy.
type FailureKind int

const (
	// FailureOther is a failure no kind below names. Its cause is the error's own text.
	FailureOther FailureKind = iota
	// FailureServiceRefused: a close frame with a reason, on any code but the catch-all (1011).
	FailureServiceRefused
	// FailureServiceClosed: a close frame with the catch-all code, or with no reason.
	FailureServiceClosed
	// FailureSessionRefused: the server answered the first frame with an ERROR control frame.
	FailureSessionRefused
	// FailureDropped: the connection ended with no close frame.
	FailureDropped
	// FailureReplyTimeout: no whole reply came within the reply timer.
	FailureReplyTimeout
	// FailureAttachDeadline: the attach's deadline ended the wait, in the dial's handshake or after.
	FailureAttachDeadline
	// FailureUnreachable: the dial failed with no HTTP error: DNS, TCP, TLS, a connect timeout.
	FailureUnreachable
	// FailureUpgradeRefused: the service answered the websocket upgrade with an HTTP error.
	FailureUpgradeRefused
	// FailureNoDevice: the remote adb server lists no device.
	FailureNoDevice
	// FailureNotWSS: the session's address is not wss.
	FailureNotWSS
	// FailureOtherProtocol: the service's control frames are on another wire version.
	FailureOtherProtocol
	// FailureSessionStopping: the relay's session-stopping close.
	FailureSessionStopping
)

const (
	// catchAllCloseCode is the websocket close code for an internal error. Its reason names no cause.
	catchAllCloseCode = 1011
	// defaultWSSPort is the port of a session address that names none.
	defaultWSSPort = "443"
)

// ConnectionFailure is a failed connection to a device, sorted for the person who reads it.
type ConnectionFailure struct {
	Kind FailureKind
	// Cause is what failed, in the customer's words.
	Cause string
	// CloseCode is the close code that FailureServiceRefused and FailureServiceClosed name. Else zero.
	CloseCode int
	// Host is the host of a failed dial, with the port when the session's address names one:
	// FailureUnreachable, FailureUpgradeRefused, and FailureAttachDeadline in the dial. Else empty.
	Host string
	// detail is what adb's refusal texts quote: the server's reason, the dial's cause, else Cause.
	detail string
}

// DialedHostPort splits Host into the host and the port, as the dial does. The port is 443 when
// Host names none. Both are empty when Host is.
func (failed ConnectionFailure) DialedHostPort() (host, port string) {
	if failed.Host == "" {
		return "", ""
	}
	host, port, err := net.SplitHostPort(failed.Host)
	if err != nil {
		return failed.Host, defaultWSSPort
	}
	return host, port
}

// plainFailure is a failure whose cause has no smaller part to quote.
func plainFailure(kind FailureKind, cause string) ConnectionFailure {
	return ConnectionFailure{Kind: kind, Cause: cause, detail: cause}
}

// SortConnectionError sorts the error of a failed connection to a device into its kind and the
// cause a customer reads. sessionARN names the session in the session-stopping cause.
func SortConnectionError(err error, sessionARN string) ConnectionFailure {
	var (
		closed     *wsCloseError
		refusal    *sessionRefusal
		dialErr    *dialError
		schemeErr  *schemeError
		versionErr *wireVersionError
		replyErr   *hostReplyError
	)
	switch {
	// Before the dropped connection: a close frame matches io.EOF too.
	case errors.As(err, &closed):
		return sortClose(closed, sessionARN)
	case errors.As(err, &refusal):
		return ConnectionFailure{Kind: FailureSessionRefused, detail: refusal.reason,
			Cause: fmt.Sprintf(wording.RefusalSessionRefusedFormat, refusal.reason)}
	case errors.As(err, &dialErr):
		systemText := dialErr.cause.Error()
		failed := ConnectionFailure{Host: dialErr.host, detail: systemText}
		switch {
		case errors.Is(dialErr.cause, ErrUpgradeRefused):
			failed.Kind, failed.Cause = FailureUpgradeRefused, fmt.Sprintf(wording.RefusalUpgradeRefusedAtHostFormat, dialErr.host)
		// The bare deadline has no system text: time ran out in the TLS handshake or the upgrade.
		case dialErr.cause == context.DeadlineExceeded:
			failed.Kind, failed.Cause = FailureAttachDeadline, wording.RefusalAttachDeadline
		default:
			failed.Kind, failed.Cause = FailureUnreachable, fmt.Sprintf(wording.RefusalUnreachableFormat, dialErr.host, systemText)
		}
		return failed
	case errors.As(err, &schemeErr):
		return plainFailure(FailureNotWSS, schemeErr.Error())
	case errors.As(err, &versionErr):
		return plainFailure(FailureOtherProtocol, versionErr.Error())
	case errors.Is(err, errNoDeviceSerial):
		return plainFailure(FailureNoDevice, errNoDeviceSerial.Error())
	// Before the deadline: the reply timer's error matches it too.
	case errors.As(err, &replyErr) && replyErr.timedOutAfter > 0:
		return plainFailure(FailureReplyTimeout, fmt.Sprintf(wording.RefusalReplyTimeoutFormat, replyErr.timedOutAfter))
	case errors.Is(err, context.DeadlineExceeded):
		return plainFailure(FailureAttachDeadline, wording.RefusalAttachDeadline)
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, errPeerReset):
		return plainFailure(FailureDropped, wording.RefusalConnectionDropped)
	}
	return plainFailure(FailureOther, err.Error())
}

// sortClose sorts the service's close frame by its code and reason.
func sortClose(closed *wsCloseError, sessionARN string) ConnectionFailure {
	reason := printableReason(closed.reason)
	var failed ConnectionFailure
	switch {
	case closed.code == sessionStoppingCloseCode:
		return plainFailure(FailureSessionStopping, defaultStoppingReason(sessionARN))
	case reason == "":
		failed = plainFailure(FailureServiceClosed, fmt.Sprintf(wording.RefusalServiceClosedNoReasonFormat, closed.code))
	case closed.code == catchAllCloseCode:
		failed = plainFailure(FailureServiceClosed, fmt.Sprintf(wording.RefusalServiceClosedFormat, reason, closed.code))
	default:
		failed = plainFailure(FailureServiceRefused, fmt.Sprintf(wording.RefusalServiceRefusedFormat, reason, closed.code))
	}
	failed.CloseCode = closed.code
	return failed
}

// printableReason keeps a close reason's printable ASCII bytes; every other byte becomes '?'.
// The reason is the service's bytes, and a terminal prints it.
func printableReason(reason string) string {
	filtered := []byte(reason)
	for index, value := range filtered {
		if value < ' ' || value > '~' {
			filtered[index] = '?'
		}
	}
	return string(filtered)
}
