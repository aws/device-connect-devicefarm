package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/aws/device-connect-devicefarm/internal/adbwire"
)

// adb smart-socket status words as the adb host server writes them (adb_io.cpp
// SendOkay and SendFail): OKAY, or FAIL then 4 hex digits of reason length and
// the reason.
const (
	statusWordLen = 4
	failLenDigits = 4
	statusOkay    = "OKAY"
	statusFail    = "FAIL"
)

// errStatusWordMalformed means the bytes where a status word belongs are neither OKAY
// nor FAIL, or a FAIL's length is not hex. Fails closed: the stream position is unknown
// from here.
var errStatusWordMalformed = errors.New("malformed adb status word")

// statusFailure is adb's own FAIL refusal with its reason text, distinct from a transport
// or framing error so a caller can log the reason without matching strings.
type statusFailure struct {
	reason string
}

func (failure *statusFailure) Error() string {
	return fmt.Sprintf("adb refused the request: %s", failure.reason)
}

// readStatusWord reads one adb status word from ws, starting from buffered (bytes an
// earlier read consumed past its own word). Each Receive is bounded by within. OKAY
// returns the bytes after the word: the next status word, or the first app bytes, which
// the caller must deliver before anything else. FAIL returns a *statusFailure carrying
// the reason. Any other word is errStatusWordMalformed. On any error the leftover is nil.
//
// Words arrive split or coalesced: the server relays reads, not words. Never drop a byte.
func readStatusWord(ws WSConn, within time.Duration, buffered []byte) ([]byte, error) {
	// Private copy: fillTo appends, and must never write into the caller's leftover.
	receivedBytes := append([]byte(nil), buffered...)
	receivedBytes, err := fillTo(ws, within, receivedBytes, statusWordLen)
	if err != nil {
		return nil, err
	}
	switch string(receivedBytes[:statusWordLen]) {
	case statusOkay:
		return receivedBytes[statusWordLen:], nil
	case statusFail:
		return nil, readFailure(ws, within, receivedBytes[statusWordLen:])
	default:
		return nil, fmt.Errorf("%w: %q", errStatusWordMalformed, receivedBytes[:statusWordLen])
	}
}

// readFailure completes a FAIL: 4 hex digits of reason length, then the reason. Bytes
// after the reason are dropped; the connection ends with the refusal.
func readFailure(ws WSConn, within time.Duration, receivedBytes []byte) error {
	receivedBytes, err := fillTo(ws, within, receivedBytes, failLenDigits)
	if err != nil {
		return err
	}
	reasonLen, err := strconv.ParseUint(string(receivedBytes[:failLenDigits]), 16, 16)
	if err != nil {
		return fmt.Errorf("%w: FAIL length %q is not hex", errStatusWordMalformed, receivedBytes[:failLenDigits])
	}
	end := failLenDigits + int(reasonLen)
	if receivedBytes, err = fillTo(ws, within, receivedBytes, end); err != nil {
		return err
	}
	return &statusFailure{reason: string(receivedBytes[failLenDigits:end])}
}

// fillTo receives until receivedBytes holds at least minLength bytes.
func fillTo(ws WSConn, within time.Duration, receivedBytes []byte, minLength int) ([]byte, error) {
	for len(receivedBytes) < minLength {
		message, err := receiveWithin(ws, within)
		if err != nil {
			return nil, err
		}
		receivedBytes = append(receivedBytes, message...)
	}
	return receivedBytes, nil
}

// deviceTransportService switches a fresh session to the one device the remote adb
// server holds. adb's model: the older select spelling, which the adb server
// still answers with exactly OKAY (adb.cpp handle_host_request). Chosen over
// host:tport:, which appends 8 raw bytes of transport id this reader would take for
// app bytes.
const deviceTransportService = "host:transport-any"

// deviceTransportRequest is that service framed as a smart-socket request. The encoder
// cannot fail on a constant of this file, and the prologue test asserts the bytes on the wire.
var deviceTransportRequest, _ = adbwire.EncodeRequest(deviceTransportService)

// openDeviceStream opens the device stream of one accepted forwarded connection: an
// ordinary adb session carrying a fabricated two-request prologue, so the remote screens
// it exactly as it screens the interactive adb client.
//
// Returns the ready connection plus whatever bytes the second status word arrived
// coalesced with. Those are the application's first bytes and must reach it before the
// pipe starts. On any failure the connection is closed and nothing is returned, so the
// caller closes the app socket with zero bytes written, as a real adb host does.
//
// ctx bounds the dial only; each prologue read is bounded by statusTimeout. warm is the
// attachment's keeper; nil dials. target is the forward rule's device spec. activity is one
// tracker per stream, never shared: a write blocked on one stream would otherwise hold every
// sharer's refresher off its quiet window. It must not be nil, because every mark
// dereferences it. errAdbConnClosed also covers a detach before the dial: the caller closes
// the app socket as it does for a refusal.
func openDeviceStream(ctx context.Context, dialer *WSDialer, device *ActiveDevice, arn string,
	warm *warmSocketKeeper, target adbwire.AdbSpec, policy WSRefreshPolicy, statusTimeout time.Duration,
	activity *activityTracker, logger *slog.Logger) (*adbConn, []byte, error) {
	// target.String(), not a hardcoded "tcp:" prefix: the request the remote adb server
	// must see is the spec the customer named, whatever its kind. The error arm is a guard,
	// not a path: no parsed spec is empty or 64 KiB.
	deviceService, err := adbwire.EncodeRequest(target.String())
	if err != nil {
		return nil, nil, fmt.Errorf("frame the device service request: %w", err)
	}

	// Read per open, never cached: the daemon renews the endpoint every few minutes and
	// an accept lands long after the attach.
	endpoint, ok := device.EndpointFor(arn)
	if !ok {
		return nil, nil, fmt.Errorf("%w: the device detached before the stream opened", errAdbConnClosed)
	}

	ws, err := warm.adbSocket(ctx, dialer, endpoint)
	if err != nil {
		return nil, nil, fmt.Errorf("dial device stream websocket: %w", err)
	}
	// Wrap before sending anything: the control frame must be read by this reader. The
	// fresh-session fallback is refused; withTerminalSessionGone says why.
	conn := newAdbConn(dialer, device, arn, RouteAdb, ws, policy, activity, logger,
		withTerminalSessionGone())

	leftover, err := playPrologue(conn, deviceService, statusTimeout)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	conn.logger.Debug("device stream open", "target", target, "session", conn.sessionUUID())
	return conn, leftover, nil
}

// selectTransport plays the transport select on a freshly wrapped connection: the select as
// the connection's first frame, then the session control frame, then the select's status
// word. Returns the bytes the word arrived coalesced with. Its own function because a leg
// that carries the client's own request needs the switch without the device service.
//
// conn, not the raw websocket, is handed to readStatusWord: conn is the connection's single
// reader, so reading the socket directly would race it.
func selectTransport(conn *adbConn, within time.Duration) ([]byte, error) {
	// First frame on the websocket, which is what mints the session. 22 bytes, so it can
	// never be misread as a 36-byte reattach uuid.
	if err := conn.Send(deviceTransportRequest); err != nil {
		return nil, fmt.Errorf("send the transport select: %w", err)
	}
	if err := conn.awaitHandshake(within); err != nil {
		return nil, err
	}
	leftover, err := readStatusWord(conn, within, nil)
	if err != nil {
		return nil, fmt.Errorf("transport select: %w", err)
	}
	return leftover, nil
}

// playPrologue plays the transport select and then the device service, consuming a status
// word for each, and returns the bytes left after the second word.
func playPrologue(conn *adbConn, deviceService []byte, within time.Duration) ([]byte, error) {
	leftover, err := selectTransport(conn, within)
	if err != nil {
		return nil, err
	}

	// A second frame, never pipelined into the first: the real adb server strands the
	// device service behind the transport switch.
	if err := conn.Send(deviceService); err != nil {
		return nil, fmt.Errorf("send the device service request: %w", err)
	}
	leftover, err = readStatusWord(conn, within, leftover)
	if err != nil {
		return nil, fmt.Errorf("device service: %w", err)
	}
	return leftover, nil
}
