// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"errors"
	"time"
)

// Session control frames: the record, a bounded read and a send. controlcodec.go owns the
// wire layout.

// errNotControlFrame means the peer does not speak this client's wire: raw ADB bytes from a
// server without the session model, or another wire version's magic.
var errNotControlFrame = errors.New("first websocket message is not a control frame this client understands")

// errReceiveTimeout means no message arrived within a bounded read. The connection is
// already closed when this is returned.
var errReceiveTimeout = errors.New("timed out waiting for a websocket message")

// controlFrame is one control frame: the arm plus the fields it carries.
type controlFrame struct {
	arm controlArm
	// Session UUID (newSession, reattach), reason (error) or token (connectorHello).
	// Empty for other arms.
	payload string
	// reverseOpen only; zero otherwise.
	reverseToken string
	customerPort int
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
	return unmarshalControlFrame(message)
}

// sendControlFrame encodes frame and sends it as one whole message.
func sendControlFrame(destination WSConn, frame controlFrame) error {
	message, err := marshalControlFrame(frame)
	if err != nil {
		return err
	}
	return destination.Send(message)
}
