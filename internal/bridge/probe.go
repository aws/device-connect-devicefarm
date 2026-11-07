// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/aws/device-connect-devicefarm/internal/adbwire"
)

// One-shot exchanges on a fresh websocket: the diagnostic probes and the session opener they
// share with the reverse clear. No connection state: a failure is reported, never recovered. For
// the probes, ctx's deadline bounds each reply read (else the dial timeout); ctx's end closes the
// socket and frees a parked read. Neither cuts a stalled send: Close waits behind it for the send
// deadline, so a caller that must return on time runs a probe on its own goroutine, as
// runReverseClear does. No error carries the endpoint, so all are safe to log.

// ErrSessionRefused is the server's ERROR to a fresh session's first frame; sessionRefusal carries
// its reason. Exported for ProbeSession's callers.
var ErrSessionRefused = errors.New("the remote access service refused the session")

// sessionUUIDLen is the exact length the server requires of a reconnect's first frame.
const sessionUUIDLen = 36

// uuidV4Pattern is the anchored UUIDv4 pattern the server matches a reconnect's first frame
// against.
var uuidV4Pattern = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-4[0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

// isUUIDv4 reports whether text is exactly one canonical UUIDv4: ProbeReattach's check of its
// caller's session UUID before the dial.
func isUUIDv4(text string) bool {
	return len(text) == sessionUUIDLen && uuidV4Pattern.MatchString(text)
}

// replyWithin bounds one reply read: what is left of ctx's deadline, else the dial timeout. A
// spent deadline falls back too: closeOnCancel ends that read at once.
func (dialer *WSDialer) replyWithin(ctx context.Context) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 {
			return remaining
		}
	}
	return dialer.dialTimeout
}

// readNewSession reads a fresh session's first server frame, which must be NEW, and returns the
// UUID the server minted.
func readNewSession(ws WSConn, within time.Duration) (string, error) {
	frame, err := readControlFrameWithin(ws, within)
	if err != nil {
		return "", fmt.Errorf("read the session control frame: %w", err)
	}
	switch frame.arm {
	case armNewSession:
		return frame.payload, nil
	case armError:
		return "", &sessionRefusal{reason: frame.payload}
	default:
		// Only the arm: another arm's body is not this caller's to log.
		return "", fmt.Errorf("no session was minted: unexpected control frame %s", frame.arm)
	}
}

// mintSelectedSession sends the transport select as a fresh websocket's first frame, which mints
// the session, then reads the NEW frame and the select's status word. Returns the minted UUID and
// any bytes after the select's OKAY, which the caller must deliver first.
func mintSelectedSession(ws WSConn, within time.Duration) (string, []byte, error) {
	if err := ws.Send(deviceTransportRequest); err != nil {
		return "", nil, fmt.Errorf("send the transport select: %w", err)
	}
	// The session control frame comes first, then the select's status word.
	sessionUUID, err := readNewSession(ws, within)
	if err != nil {
		return "", nil, err
	}
	leftover, err := readStatusWord(ws, within, nil)
	if err != nil {
		var refusal *statusFailure
		if errors.As(err, &refusal) {
			return "", nil, fmt.Errorf("the device adb server refused the transport select: %s", refusal.reason)
		}
		return "", nil, fmt.Errorf("read the transport select's status word: %w", err)
	}
	return sessionUUID, leftover, nil
}

// openSelectedSession dials RouteAdb and runs mintSelectedSession: a proxied connection's first
// exchange. Returns the websocket, the minted UUID and the leftover bytes. within bounds each
// reply read; ctx's end closes the websocket mid-exchange. On error nothing stays open.
// ProbeSession and the reverse clear share it, so the diagnostic runs the clear's own code.
func (dialer *WSDialer) openSelectedSession(ctx context.Context, endpoint string, within time.Duration) (
	WSConn, string, []byte, error) {
	ws, err := dialer.Dial(ctx, endpoint, RouteAdb)
	if err != nil {
		return nil, "", nil, err
	}
	defer closeOnCancel(ctx, ws)()
	sessionUUID, leftover, err := mintSelectedSession(ws, within)
	if err != nil {
		ws.Close()
		return nil, "", nil, err
	}
	return ws, sessionUUID, leftover, nil
}

// ProbeSession opens a fresh session on RouteAdb, selects the device transport and closes the
// websocket. Returns the minted UUID. The close frees the connection's server slot at once. The
// session, with its idle connection to the device's adb server, lives until the server's
// reconnect grace period ends: no client frame or close code ends it sooner.
func (dialer *WSDialer) ProbeSession(ctx context.Context, endpoint string) (string, error) {
	ws, sessionUUID, _, err := dialer.openSelectedSession(ctx, endpoint, dialer.replyWithin(ctx))
	if err != nil {
		return "", err
	}
	ws.Close()
	return sessionUUID, nil
}

// ProbeAdbServerVersion asks the remote adb server for its protocol version on a session of its
// own: an adb server closes the connection after a host service, so no probe can share it.
func (dialer *WSDialer) ProbeAdbServerVersion(ctx context.Context, endpoint string) (int, error) {
	ws, err := dialer.Dial(ctx, endpoint, RouteAdb)
	if err != nil {
		return 0, err
	}
	defer ws.Close()
	defer closeOnCancel(ctx, ws)()
	within := dialer.replyWithin(ctx)
	if err := ws.Send([]byte(adbwire.HostVersionRequest)); err != nil {
		return 0, fmt.Errorf("send the host:version request: %w", err)
	}
	if _, err := readNewSession(ws, within); err != nil {
		return 0, err
	}
	leftover, err := readStatusWord(ws, within, nil)
	if err != nil {
		var refusal *statusFailure
		if errors.As(err, &refusal) {
			return 0, fmt.Errorf("the device adb server refused host:version: %s", refusal.reason)
		}
		return 0, fmt.Errorf("read host:version's status word: %w", err)
	}
	reply, err := fillTo(ws, within, leftover, adbwire.HostVersionReplyLen)
	if err != nil {
		return 0, fmt.Errorf("read the host:version reply: %w", err)
	}
	return adbwire.ParseHostVersionReply(reply)
}

// ProbeReattach dials RouteAdb with sessionUUID as the whole first frame and requires REATTACH
// naming it. Proves the server keeps a session across websockets, which every swap relies on.
// ErrSessionGone means the server refused: the session is unknown, expired or ending. The close
// starts the session's grace period again.
func (dialer *WSDialer) ProbeReattach(ctx context.Context, endpoint, sessionUUID string) error {
	// Any other first frame mints a fresh session, so refuse the caller's mistake before the dial.
	if !isUUIDv4(sessionUUID) {
		return fmt.Errorf("%q is not a session uuid", sessionUUID)
	}
	ws, err := dialer.Dial(ctx, endpoint, RouteAdb)
	if err != nil {
		return err
	}
	defer ws.Close()
	defer closeOnCancel(ctx, ws)()
	if err := ws.Send([]byte(sessionUUID)); err != nil {
		return fmt.Errorf("send the session uuid: %w", err)
	}
	frame, err := readControlFrameWithin(ws, dialer.replyWithin(ctx))
	if err != nil {
		return fmt.Errorf("read the reattach ack: %w", err)
	}
	switch frame.arm {
	case armReattach:
		if frame.payload != sessionUUID {
			return fmt.Errorf("the reattach ack names session %s, not %s", frame.payload, sessionUUID)
		}
		return nil
	case armError:
		return fmt.Errorf("%w: %s", ErrSessionGone, frame.payload)
	default:
		return fmt.Errorf("the reattach ack carried %s, not reattach", frame.arm)
	}
}

// ProbeNotifier registers a reverse notifier and closes it on the ack. The server unregisters a
// notifier as its websocket closes, so the device's one slot is free again at once.
// ErrNotifierSlotTaken means another notifier holds it: the server refuses a second hello rather
// than replace the first.
func (dialer *WSDialer) ProbeNotifier(ctx context.Context, endpoint string) error {
	ws, err := dialer.Dial(ctx, endpoint, RouteAdbReverse)
	if err != nil {
		return err
	}
	defer ws.Close()
	defer closeOnCancel(ctx, ws)()
	return helloNotifier(ws, dialer.replyWithin(ctx))
}
