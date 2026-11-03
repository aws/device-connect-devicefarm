package bridge

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Session control frame: magicPrefix + one status byte + UTF-8 payload, one whole message,
// no length field. On ?path=adb it is always the first server frame; the client consumes it
// and never forwards it. On ?path=adbreverse the first frame is the client's (a hello) and a
// notifier exchanges control frames for its whole life.
//
// Wire contract shared with the server. Change both together.
const (
	magicPrefix = "\x00DFADB\x00"

	// controlStatusNew announces a fresh session; payload is its UUID.
	controlStatusNew byte = 0x00
	// controlStatusReattach acks a reconnect; payload is the resumed session's UUID.
	controlStatusReattach byte = 0x01
	// controlStatusError refuses the connection; payload is a human-readable reason.
	controlStatusError byte = 0x02

	// 0x03 to 0x0F are reserved for /adb. The adbreverse route starts at 0x10 so a misrouted
	// frame is recognizable in a hex dump; /adb's callers refuse 0x10 and above.

	// controlStatusNotifierHello opens a notifier: client to server, first frame, empty payload.
	controlStatusNotifierHello byte = 0x10
	// controlStatusNotifierReady acks the notifier hello: server to client, empty payload.
	controlStatusNotifierReady byte = 0x11
	// controlStatusReverseOpen announces a parked device-originated connection: server to
	// client on the notifier; payload is "<token>:<customer_port>".
	controlStatusReverseOpen byte = 0x12
	// controlStatusPing is the notifier keepalive: client to server, empty payload.
	controlStatusPing byte = 0x13
	// controlStatusPong echoes one PING: server to client, empty payload.
	controlStatusPong byte = 0x14
	// controlStatusConnectorHello attaches a websocket to a parked connection: client to
	// server, first frame; payload is the token from REVERSE_OPEN.
	controlStatusConnectorHello byte = 0x15

	// sessionUUIDLen is the exact length the server requires of a reconnect's first frame.
	// A reverse token is UUIDv4 text of the same length.
	sessionUUIDLen = 36

	// maxTCPPort bounds a REVERSE_OPEN customer port.
	maxTCPPort = 65535
)

// uuidV4Pattern is the anchored UUIDv4 pattern the server matches a reconnect's first frame
// against. Also applied to NEW/REATTACH payloads and to reverse tokens.
var uuidV4Pattern = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-4[0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

// customerPortPattern is the REVERSE_OPEN port grammar: 1 to 5 decimal digits, no sign, no
// leading zero. strconv.Atoi alone would accept "+80" and "0080".
var customerPortPattern = regexp.MustCompile(`^[1-9][0-9]{0,4}$`)

// errNotControlFrame means the peer does not speak the session model: a pre-session-model
// server answers with raw ADB bytes here.
var errNotControlFrame = errors.New("first websocket message is not a session control frame")

// errReceiveTimeout means no message arrived within a bounded read. The connection is
// already closed when this is returned.
var errReceiveTimeout = errors.New("timed out waiting for a websocket message")

// controlFrame is a decoded control frame: a status plus its payload.
type controlFrame struct {
	status  byte
	payload string
	// arm is the key the JSON codec decoded; zero from the byte codec.
	arm controlArm
	// REVERSE_OPEN only; zero otherwise.
	reverseToken string
	customerPort int
}

// isUUIDv4 reports whether text is exactly one canonical UUIDv4.
func isUUIDv4(text string) bool {
	return len(text) == sessionUUIDLen && uuidV4Pattern.MatchString(text)
}

// parseControlFrame decodes one whole websocket message as a control frame. Fails closed:
// anything malformed errors. Which statuses a caller accepts is the caller's check.
func parseControlFrame(message []byte) (controlFrame, error) {
	if len(message) < len(magicPrefix)+1 || string(message[:len(magicPrefix)]) != magicPrefix {
		return controlFrame{}, errNotControlFrame
	}

	status := message[len(magicPrefix)]
	payload := string(message[len(magicPrefix)+1:])
	frame := controlFrame{status: status, payload: payload}
	switch status {
	case controlStatusNew, controlStatusReattach, controlStatusConnectorHello:
		if !isUUIDv4(payload) {
			return controlFrame{}, fmt.Errorf(
				"control frame status 0x%02x carries %q, which is not a UUIDv4", status, payload)
		}
	case controlStatusError:
		// A variable-length reason, surfaced to the caller as-is.
	case controlStatusNotifierHello, controlStatusNotifierReady, controlStatusPing, controlStatusPong:
		if payload != "" {
			return controlFrame{}, fmt.Errorf(
				"control frame status 0x%02x carries %q, expected an empty payload", status, payload)
		}
	case controlStatusReverseOpen:
		token, customerPort, err := parseReverseOpen(payload)
		if err != nil {
			return controlFrame{}, fmt.Errorf("control frame status 0x%02x: %w", status, err)
		}
		frame.reverseToken, frame.customerPort = token, customerPort
	default:
		return controlFrame{}, fmt.Errorf("unknown control frame status 0x%02x", status)
	}
	return frame, nil
}

// encodeControlFrame builds one control frame: magicPrefix, status, payload. It validates
// nothing; the peer's parser does.
func encodeControlFrame(status byte, payload string) []byte {
	frame := make([]byte, 0, len(magicPrefix)+1+len(payload))
	frame = append(frame, magicPrefix...)
	frame = append(frame, status)
	return append(frame, payload...)
}

// parseReverseOpen splits a REVERSE_OPEN payload, "<token>:<customer_port>", into its
// validated halves. Splits once on the first colon; anything else fails closed. A payload
// with no separator may be a whole token, so that error does not echo it.
func parseReverseOpen(payload string) (token string, port int, err error) {
	token, portText, found := strings.Cut(payload, ":")
	if !found {
		return "", 0, errors.New("reverse open payload has no port separator")
	}
	if !isUUIDv4(token) {
		return "", 0, fmt.Errorf("reverse open token %q is not a UUIDv4", token)
	}
	if !customerPortPattern.MatchString(portText) {
		return "", 0, fmt.Errorf("reverse open port %q is not 1 to 5 digits with no sign or leading zero", portText)
	}
	port, _ = strconv.Atoi(portText) // cannot fail: the pattern admits only 1 to 5 ASCII digits
	if port > maxTCPPort {
		return "", 0, fmt.Errorf("reverse open port %d is above %d", port, maxTCPPort)
	}
	return token, port, nil
}

// receivedMessage is one Receive's raw outcome, handed from a reader goroutine to the code
// that waits for it.
type receivedMessage struct {
	message []byte
	err     error
}

// receiveWithin does one Receive on ws, bounded by within. On timeout it closes ws and
// returns errReceiveTimeout.
//
// The Receive runs on its own goroutine because x/net has no deadline-aware read. The
// result channel is buffered so a late Receive exits instead of leaking.
func receiveWithin(ws WSConn, within time.Duration) ([]byte, error) {
	results := make(chan receivedMessage, 1)
	go func() {
		message, err := ws.Receive()
		results <- receivedMessage{message: message, err: err}
	}()

	timer := time.NewTimer(within)
	defer timer.Stop()
	select {
	case result := <-results:
		return result.message, result.err
	case <-timer.C:
		// Keep this re-check. select picks a ready case at random, so a frame that
		// arrived before the timer can lose the pick; discarding it poisons a
		// predecessor the server did ack. Narrows the race, does not close it.
		select {
		case result := <-results:
			return result.message, result.err
		default:
		}
		ws.Close()
		return nil, errReceiveTimeout
	}
}

// readControlFrameWithin is receiveWithin plus the control-frame decode.
func readControlFrameWithin(ws WSConn, within time.Duration) (controlFrame, error) {
	message, err := receiveWithin(ws, within)
	if err != nil {
		return controlFrame{}, err
	}
	return parseControlFrame(message)
}
