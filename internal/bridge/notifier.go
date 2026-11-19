package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// errNoNotifierEndpoint means no endpoint is bound for that ARN any more: the device was
// removed, or a different one was attached. Either way there is nothing to notify about.
var errNoNotifierEndpoint = errors.New("no endpoint bound for the notifier's device")

// errNotifierRefused is the server's ERROR answer to the hello, kept distinct from a
// transport failure. Both redial on the same schedule.
var errNotifierRefused = errors.New("the server refused the notifier")

// notifierSlotTakenReason is the server's refusal payload when the device's one notifier slot is
// taken by another live client's notifier, or by a stale registration only the server's no-PING
// timer clears. Must match the server's text byte for byte.
const notifierSlotTakenReason = "notifier already registered"

// errNotifierSlotTaken is the refusal carrying notifierSlotTakenReason.
var errNotifierSlotTaken = fmt.Errorf("%w: %s", errNotifierRefused, notifierSlotTakenReason)

// notifierConn is one attachment's notifier websocket on RouteAdbReverse: the server announces
// every device-originated reverse connection on it. A bare websocket, not an adbConn, because a
// notifier carries no session: a drop is answered by a redial, never a reattach.
//
// The zero value is not usable; construct with dialNotifier.
type notifierConn struct {
	ws     WSConn
	logger *slog.Logger
}

// dialNotifier opens a notifier and completes its hello exchange: NOTIFIER_HELLO as the first
// frame, then one bounded read that must be NOTIFIER_READY. ERROR is the server's refusal and
// carries its reason; any other status is a protocol violation. Every failure closes the
// websocket, so a caller holding an error owns nothing. The endpoint is read per dial, never
// cached: the daemon renews it every four minutes.
func dialNotifier(ctx context.Context, dialer *WSDialer, device *ActiveDevice, arn string,
	ackWithin time.Duration, logger *slog.Logger) (*notifierConn, error) {
	if logger == nil {
		logger = slog.Default()
	}

	endpoint, ok := device.EndpointFor(arn)
	if !ok {
		return nil, errNoNotifierEndpoint
	}
	ws, err := dialer.Dial(ctx, endpoint, RouteAdbReverse)
	if err != nil {
		return nil, fmt.Errorf("dial the notifier websocket: %w", err)
	}
	if err := ws.Send(encodeControlFrame(controlStatusNotifierHello, "")); err != nil {
		ws.Close()
		return nil, fmt.Errorf("send the notifier hello: %w", err)
	}

	frame, err := readControlFrameWithin(ws, ackWithin)
	if err != nil {
		ws.Close() // already closed on receiveWithin's timeout path; closing twice is inert
		return nil, fmt.Errorf("read the notifier hello ack: %w", err)
	}
	switch frame.status {
	case controlStatusNotifierReady:
	case controlStatusError:
		ws.Close()
		if frame.payload == notifierSlotTakenReason {
			return nil, errNotifierSlotTaken
		}
		return nil, fmt.Errorf("%w: %s", errNotifierRefused, frame.payload)
	default:
		ws.Close()
		return nil, fmt.Errorf("the notifier hello was answered with status 0x%02x, not NOTIFIER_READY",
			frame.status)
	}

	logger.Debug("reverse notifier ready", "device", arn)
	return &notifierConn{ws: ws, logger: logger}, nil
}

// run is the notifier's single reader: it loops until one frame is fatal, closes the socket and
// returns the error that ended it. Close stops it from outside. REVERSE_OPEN goes to
// onReverseOpen on its own goroutine, so opening a connector never stalls the next
// announcement; PONG runs onPong inline, so onPong must not block. Both callbacks must be
// non-nil and neither runs under a recover. Everything else ends the loop: the route carries
// nothing else, and the answer to all of it is the same, redial.
func (notifier *notifierConn) run(onReverseOpen func(token string, customerPort int), onPong func()) error {
	defer notifier.ws.Close()
	for {
		message, err := notifier.ws.Receive()
		if err != nil {
			return err
		}
		frame, err := parseControlFrame(message)
		if err != nil {
			return err
		}

		switch frame.status {
		case controlStatusReverseOpen:
			// The token is a bearer capability for that parked connection: never logged.
			notifier.logger.Debug("reverse connection announced", "customerPort", frame.customerPort)
			go onReverseOpen(frame.reverseToken, frame.customerPort)
		case controlStatusPong:
			onPong()
		case controlStatusError:
			return fmt.Errorf("the server ended the notifier: %s", frame.payload)
		default:
			return fmt.Errorf("unexpected control frame status 0x%02x on the notifier", frame.status)
		}
	}
}

// ping sends one keepalive. At the cap the server evicts the longest-idle connection first.
// Only frames on that connection, either direction, reset its idle clock. An idle notifier sends
// none and the server sends it none, so the PING goes client to server.
func (notifier *notifierConn) ping() error {
	return notifier.ws.Send(encodeControlFrame(controlStatusPing, ""))
}

// Close ends the notifier, which is what returns run's parked Receive. It can queue behind an
// in-flight ping for up to sendDeadline: x/net takes the write lock before it closes.
func (notifier *notifierConn) Close() error {
	return notifier.ws.Close()
}
