// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"

	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

func TestGivenAWholeReplyWhenAHostReplyIsFetchedThenItReturnsByteForByte(t *testing.T) {
	reply := okayDevicesReply("emulator-5554\tdevice\n")
	testCases := []struct {
		name   string
		frames [][]byte
	}{
		{name: "after the control frame", frames: [][]byte{controlFrameNew(), reply}},
		{name: "split across messages", frames: [][]byte{controlFrameNew(), reply[:2], reply[2:6], reply[6:]}},
		// Leniency: a first message that is already the adb reply parses anyway.
		{name: "with no control frame", frames: [][]byte{reply}},
		// The length prefix ends the reply.
		{name: "with bytes past it", frames: [][]byte{controlFrameNew(), append(append([]byte{}, reply...), "OKAY"...)}},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			wsURL := probeReplyServer(t, testCase.frames...)

			got, err := newTestDialer(testWait).fetchHostReply(context.Background(), wsURL, hostDevicesRequest)

			require.NoError(t, err)
			assert.Equal(t, reply, got)
		})
	}
}

func TestGivenAReplyThatIsNoWholeOkayWhenAHostReplyIsFetchedThenTheErrorIsAReplyError(t *testing.T) {
	// A reply error lets the cache relay the request; every other error closes the client.
	neverCompletes := []byte("OKAYffff")
	filler := bytes.Repeat([]byte("x"), maxHostReplyBytes+1-len(neverCompletes))
	testCases := []struct {
		name     string
		frames   [][]byte
		timeout  time.Duration
		wantErr  error
		wantText string
	}{
		{name: "a FAIL reply", frames: [][]byte{controlFrameNew(), []byte("FAIL0014something went wrong")},
			timeout: time.Hour, wantText: "answered FAIL instead of OKAY"},
		{name: "an unparsable length", frames: [][]byte{controlFrameNew(), []byte("OKAYzzzzR3CN30ABCDE\tdevice\n")},
			timeout: time.Hour, wantText: `malformed version reply: length "zzzz"`},
		{name: "a signed length", frames: [][]byte{controlFrameNew(), []byte("OKAY+01aR3CN30ABCDE\tdevice\n")},
			timeout: time.Hour, wantText: `malformed version reply: length "+01a"`},
		{name: "a length never satisfied", frames: [][]byte{controlFrameNew(), neverCompletes, filler},
			timeout: time.Hour, wantText: "the version reply runs past its byte cap"},
		// The server sends nothing back, so the reply timeout ends the read.
		{name: "no reply", timeout: 100 * time.Millisecond, wantText: "await the version reply"},
		// Fixture vector v1-magic-new-frame: a byte-protocol server's first frame.
		{name: "a control frame on another wire version", frames: [][]byte{decodeHex(t, frameVector(t, "v1-magic-new-frame").Hex)},
			timeout: time.Hour, wantErr: errNotControlFrame, wantText: "read the session control frame"},
		// The server refuses the session in place of the reply; its reason is the failure.
		{name: "an ERROR control frame", frames: [][]byte{testsupport.FrameError("device is busy")},
			timeout: time.Hour, wantErr: ErrSessionRefused, wantText: "device is busy"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			wsURL := probeReplyServer(t, testCase.frames...)
			dialer := newTestDialer(testWait)
			dialer.hostReplyTimeout = testCase.timeout

			reply, err := dialer.fetchHostReply(context.Background(), wsURL, hostVersionRequest)

			var replyErr *hostReplyError
			require.ErrorAs(t, err, &replyErr)
			if testCase.wantErr != nil {
				assert.ErrorIs(t, err, testCase.wantErr)
			}
			assert.ErrorContains(t, err, testCase.wantText)
			assert.Nil(t, reply)
		})
	}
}

func TestGivenAServerThatClosesWithNoReplyWhenAHostReplyIsFetchedThenTheErrorIsAReplyError(t *testing.T) {
	wsURL := testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		var request []byte
		_ = websocket.Message.Receive(ws, &request)
		ws.Close()
	})

	_, err := newTestDialer(testWait).fetchHostReply(context.Background(), wsURL, hostVersionRequest)

	var replyErr *hostReplyError
	require.ErrorAs(t, err, &replyErr)
	assert.ErrorContains(t, err, "read the version reply")
}

func TestGivenARequestThatNeverReachesTheServerWhenAHostReplyIsFetchedThenTheErrorIsNoReplyError(t *testing.T) {
	testCases := []struct {
		name     string
		endpoint func(t *testing.T) string
		dialer   func() *WSDialer
	}{
		// Port 1 is not listening.
		{name: "a failed dial", endpoint: func(*testing.T) string { return "wss://127.0.0.1:1/adb" },
			dialer: func() *WSDialer { return newTestDialer(500 * time.Millisecond) }},
		{name: "a failed send", endpoint: func(t *testing.T) string { return probeReplyServer(t) },
			dialer: func() *WSDialer {
				dialer := newTestDialer(testWait)
				dialer.writeDeadline = -time.Second
				return dialer
			}},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := testCase.dialer().fetchHostReply(context.Background(), testCase.endpoint(t), hostVersionRequest)

			var replyErr *hostReplyError
			require.Error(t, err)
			assert.False(t, errors.As(err, &replyErr), "a request the server never read is no reply error: %v", err)
		})
	}
}
