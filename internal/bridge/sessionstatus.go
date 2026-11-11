// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// statusRequest is the one frame the daemon sends on the control route: the web console's
// status request, verbatim.
const statusRequest = `{"message":"StatusMessage","parameters":{}}`

// maxStatusFrames bounds how many frames one read skips while waiting for the status reply:
// the route can carry other messages meant for the web console. Source: pick.
const maxStatusFrames = 8

// maxStatusReplyBytes bounds one frame of the status read; the reply is a small JSON object.
// Source: pick.
const maxStatusReplyBytes = 64 * 1024

var (
	errNoStatusReply      = errors.New("no status reply")
	errTimeLeftOutOfRange = errors.New("the reported time left is outside the session length")
	errStatusReplyTooBig  = errors.New("the status reply is over the size cap")
)

// FetchSessionTimeLeft dials endpoint on the server's status route, sends one status request and
// returns the session's time left: above zero and at most sessionLength, or an error. It reads at
// most maxStatusFrames frames, refuses a frame over maxStatusReplyBytes, and sends nothing but
// the one request. ctx bounds the whole read, teardown included: the exchange goroutine owns the
// socket's close, and a caller never waits on it (the fetchHostReply shape), so a wedged close
// cannot hold the add past its bound.
func (dialer *WSDialer) FetchSessionTimeLeft(ctx context.Context, endpoint string,
	sessionLength time.Duration) (time.Duration, error) {
	ws, err := dialer.Dial(ctx, endpoint, RouteControl)
	if err != nil {
		return 0, err
	}
	var closeOnce sync.Once
	closeWS := func() { closeOnce.Do(func() { ws.Close() }) }
	type statusResult struct {
		timeLeft time.Duration
		err      error
	}
	results := make(chan statusResult, 1)
	go func() {
		defer closeWS()
		timeLeft, exchangeErr := exchangeStatus(ws, sessionLength)
		results <- statusResult{timeLeft: timeLeft, err: exchangeErr}
	}()
	select {
	case result := <-results:
		return result.timeLeft, result.err
	case <-ctx.Done():
		// The close unblocks the goroutine's read; it is not waited for.
		go closeWS()
		return 0, fmt.Errorf("await the status reply: %w", ctx.Err())
	}
}

// exchangeStatus sends the one status request on ws and reads frames until the status reply.
func exchangeStatus(ws WSConn, sessionLength time.Duration) (time.Duration, error) {
	if err := ws.Send([]byte(statusRequest)); err != nil {
		return 0, fmt.Errorf("send the status request: %w", err)
	}
	for range maxStatusFrames {
		message, err := ws.Receive()
		if err != nil {
			return 0, fmt.Errorf("await the status reply: %w", err)
		}
		if len(message) > maxStatusReplyBytes {
			return 0, errStatusReplyTooBig
		}
		timeLeft, isStatus := timeLeftOf(message)
		if !isStatus {
			continue
		}
		if timeLeft <= 0 || timeLeft > sessionLength {
			return 0, fmt.Errorf("%w: %s", errTimeLeftOutOfRange, timeLeft)
		}
		return timeLeft, nil
	}
	return 0, fmt.Errorf("%w in %d frames", errNoStatusReply, maxStatusFrames)
}

// timeLeftOf reads a status reply: a JSON object whose value member holds a numeric timeLeft in
// seconds. The server encodes value twice, as JSON text inside the JSON object; a value that is
// already an object is accepted too. Any other frame is not a status reply.
func timeLeftOf(message []byte) (time.Duration, bool) {
	var reply struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(message, &reply); err != nil || len(reply.Value) == 0 {
		return 0, false
	}
	payload := []byte(reply.Value)
	var text string
	if json.Unmarshal(payload, &text) == nil {
		payload = []byte(text)
	}
	var value struct {
		TimeLeft *float64 `json:"timeLeft"`
	}
	if err := json.Unmarshal(payload, &value); err != nil || value.TimeLeft == nil {
		return 0, false
	}
	return time.Duration(*value.TimeLeft * float64(time.Second)), true
}
