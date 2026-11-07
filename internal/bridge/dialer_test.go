// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"bufio"
	"context"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"

	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// newTestDialer is NewWSDialer trusting the test CA behind every testsupport mock.
func newTestDialer(dialTimeout time.Duration) *WSDialer {
	return NewWSDialer(dialTimeout).TrustRoots(testsupport.TestRoots())
}

func TestGivenReachableMockServerWhenDialedThenInboundCapArmed(t *testing.T) {
	// The dialer arms the inbound per-message cap on the returned connection. The
	// cap is encapsulated behind WSConn, so it is asserted behaviorally: an inbound
	// message over the cap surfaces as websocket.ErrFrameTooLarge on Receive.
	oversized := make([]byte, maxInboundMessageBytes+1)
	wsURL := testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		ws.PayloadType = websocket.BinaryFrame
		websocket.Message.Send(ws, oversized)
		io.Copy(io.Discard, ws)
	})
	dialer := newTestDialer(testTimeout)

	ws, err := dialer.Dial(context.Background(), wsURL, RouteAdb)
	require.NoError(t, err)
	t.Cleanup(func() { ws.Close() })

	_, receiveErr := ws.Receive()
	assert.ErrorIs(t, receiveErr, websocket.ErrFrameTooLarge, "the inbound cap must be armed on the dialed connection")
}

func TestGivenUnreachableEndpointWhenDialedThenErrors(t *testing.T) {
	// Port 1 is not listening; the dial fails fast.
	dialer := newTestDialer(testWait)

	_, err := dialer.Dial(context.Background(), "wss://127.0.0.1:1/adb", RouteAdb)

	require.Error(t, err)
}

func TestGivenMalformedURLWhenDialedThenErrors(t *testing.T) {
	// Pre-dial checks fail closed: url.Parse, then the scheme check, which a bare path fails.
	tests := []struct {
		name     string
		endpoint string
		wantIs   error
	}{
		{name: "unparseable", endpoint: "://not-a-url"},
		{name: "relative path, no scheme", endpoint: "not-a-url", wantIs: errSchemeNotWSS},
		{name: "plaintext scheme", endpoint: "ws://127.0.0.1:1/adb", wantIs: errSchemeNotWSS},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dialer := newTestDialer(testWait)

			_, err := dialer.Dial(context.Background(), tc.endpoint, RouteAdb)

			require.Error(t, err)
			if tc.wantIs != nil {
				assert.ErrorIs(t, err, tc.wantIs)
			}
		})
	}
}

// queryRecordingWSS starts a mock that reports the query string of every accepted
// websocket on the returned channel and then holds the connection open.
func queryRecordingWSS(t *testing.T) (wsURL string, queries <-chan url.Values) {
	t.Helper()
	recorded := make(chan url.Values, 4)
	wsURL = testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		recorded <- ws.Request().URL.Query()
		io.Copy(io.Discard, ws)
	})
	return wsURL, recorded
}

// awaitQuery returns the next recorded query or fails the test.
func awaitQuery(t *testing.T, queries <-chan url.Values) url.Values {
	t.Helper()
	select {
	case query := <-queries:
		return query
	case <-time.After(5 * time.Second):
		t.Fatal("the mock server never saw the dial")
		return nil
	}
}

func TestGivenPathlessEndpointWhenDialedThenRouteAppliedAsPathParameter(t *testing.T) {
	// The resolved endpoint is path-less; the dial is where the route reaches the
	// wire, as the ?path= query parameter the websocket relay routes on.
	wsURL, queries := queryRecordingWSS(t)
	dialer := newTestDialer(testTimeout)

	ws, err := dialer.Dial(context.Background(), wsURL, RouteAdbReverse)
	require.NoError(t, err)
	t.Cleanup(func() { ws.Close() })

	assert.Equal(t, url.Values{"path": {"adbreverse"}}, awaitQuery(t, queries),
		"the named route must be the only query parameter added")
}

func TestGivenEndpointAlreadyCarryingAQueryWhenDialedThenTheRouteReplacesOnlyThePathParameter(t *testing.T) {
	// A presigned endpoint carries its own query; the route must join it, and a stale
	// path value must be replaced rather than duplicated (two values would let the
	// websocket relay pick either).
	wsURL, queries := queryRecordingWSS(t)
	dialer := newTestDialer(testTimeout)

	ws, err := dialer.Dial(context.Background(), wsURL+"?token=xyz&path=stale", RouteAdb)
	require.NoError(t, err)
	t.Cleanup(func() { ws.Close() })

	assert.Equal(t, url.Values{"path": {"adb"}, "token": {"xyz"}}, awaitQuery(t, queries))
}

func TestGivenNoRouteWhenDialedThenErrorsBeforeDialing(t *testing.T) {
	// An unnamed route is a programming error: it must fail closed, so a misrouted
	// websocket never leaves the client. The sentinel is the proof; the counter only
	// adds that a reachable server saw nothing by the time the error returned.
	var accepted atomic.Int32
	wsURL := testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		accepted.Add(1)
		ws.Close()
	})
	dialer := newTestDialer(testWait)

	_, err := dialer.Dial(context.Background(), wsURL, "")

	assert.ErrorIs(t, err, errNoRoute)
	assert.Zero(t, accepted.Load())
}

func TestGivenAPrivateCAServerWhenDialedWithSystemRootsThenTheDialFailsAsUnknownAuthority(t *testing.T) {
	// Production path: no TrustRoots, so the private CA is untrusted. The
	// UnknownAuthorityError proves chain verification is on; errors.As reaches it only
	// because Dial unwraps DialError.
	wssURL := testsupport.NewMockWSS(t, func(serverConn *websocket.Conn) { serverConn.Close() })
	dialer := NewWSDialer(testTimeout)

	_, err := dialer.Dial(context.Background(), wssURL, RouteAdb)

	var unknownAuthority x509.UnknownAuthorityError
	require.ErrorAs(t, err, &unknownAuthority)
}

func TestGivenAPrivateCAServerWhenDialedTrustingItsRootThenTheDialSucceeds(t *testing.T) {
	wssURL := testsupport.NewMockWSS(t, func(serverConn *websocket.Conn) {
		serverConn.PayloadType = websocket.BinaryFrame
		io.Copy(serverConn, serverConn)
	})
	dialer := newTestDialer(testTimeout)

	websocketConn, err := dialer.Dial(context.Background(), wssURL, RouteAdb)
	require.NoError(t, err)
	t.Cleanup(func() { websocketConn.Close() })

	payload := []byte("000chost:devices")
	require.NoError(t, websocketConn.Send(payload))
	got, err := websocketConn.Receive()
	require.NoError(t, err)
	assert.Equal(t, payload, got, "the websocket must be live over TLS")
}

func TestGivenACertificateForAnotherNameWhenDialedThenHostnameVerificationRefusesIt(t *testing.T) {
	// Proves TrustRoots skips no name check (Go checks the name before the chain).
	wssURL := testsupport.NewMockWSSForName(t, func(serverConn *websocket.Conn) { serverConn.Close() }, "other.invalid")
	dialer := newTestDialer(testTimeout)

	_, err := dialer.Dial(context.Background(), wssURL, RouteAdb)

	var hostnameErr x509.HostnameError
	require.ErrorAs(t, err, &hostnameErr)
}

func TestGivenADialErrorWhenReturnedThenThePresignedURLIsAbsentFromItsText(t *testing.T) {
	// The presigned query is a credential. x/net's DialError and url.Error print the
	// whole URL; the error Dial returns must not, whichever step failed.
	const signature = "X-Amz-Signature=deadbeefcafe"
	tests := []struct {
		name     string
		endpoint string
		route    Route
	}{
		{name: "no route", endpoint: "wss://127.0.0.1:1/adb", route: ""},
		{name: "unparseable", endpoint: "://not-a-url", route: RouteAdb},
		{name: "no scheme", endpoint: "not-a-url", route: RouteAdb},
		{name: "tcp connect refused", endpoint: "wss://127.0.0.1:1/adb", route: RouteAdb},
		{name: "certificate refused", endpoint: testsupport.NewMockWSS(t, func(serverConn *websocket.Conn) { serverConn.Close() }), route: RouteAdb},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			// System roots: the certificate row needs the test CA untrusted.
			dialer := NewWSDialer(testTimeout)

			_, err := dialer.Dial(context.Background(), fmt.Sprintf("%s?%s", testCase.endpoint, signature), testCase.route)

			require.Error(t, err)
			assert.NotContains(t, err.Error(), signature)
			assert.NotContains(t, err.Error(), "path=", "the routed query must not leak either")
		})
	}
}

func TestGivenANonWSSSchemeWhenDialedThenItIsRefusedBeforeAnyDial(t *testing.T) {
	// A ws:// dial would send the presigned query in the clear, and the check is not a ws://
	// denylist: anything but wss is refused, an absent scheme included. The sentinel proves
	// the refusal; the counter adds that nothing connected before the error returned.
	counterAddress, accepted := testsupport.NewAcceptCounter(t)
	tests := []struct {
		name   string
		scheme string
	}{
		{name: "plaintext websocket", scheme: "ws:"},
		{name: "http", scheme: "http:"},
		{name: "https", scheme: "https:"},
		{name: "no scheme", scheme: ""},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			dialer := NewWSDialer(testWait)

			_, err := dialer.Dial(context.Background(), fmt.Sprintf("%s//%s/adb", testCase.scheme, counterAddress), RouteAdb)

			require.ErrorIs(t, err, errSchemeNotWSS)
			assert.Zero(t, accepted(), "a refused scheme must cost no connection")
		})
	}
}

func TestGivenARefusedSchemeWhenTheErrorIsFormattedThenTheURLIsAbsent(t *testing.T) {
	const host = "127.0.0.1:1"
	const signature = "X-Amz-Signature=deadbeefcafe"
	dialer := NewWSDialer(testWait)

	_, err := dialer.Dial(context.Background(), fmt.Sprintf("ws://%s/adb?%s", host, signature), RouteAdb)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `refused scheme "ws"`)
	assert.NotContains(t, err.Error(), host)
	assert.NotContains(t, err.Error(), signature)
	assert.NotContains(t, err.Error(), "path=")
}

// websocketAcceptGUID is the fixed suffix the opening handshake hashes into its accept key.
const websocketAcceptGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// scriptedPayloadCap is the per-message cap the scripted streams run under: under 64 KiB, so a
// frame can exceed it in both length forms.
const scriptedPayloadCap = 300

// maxScriptedReceives bounds one scripted stream's Receive calls, so a reader that never ends
// fails the test instead of hanging it.
const maxScriptedReceives = 16

// serverFrame encodes one frame as a server sends it: unmasked, its payload length in the
// shortest form.
func serverFrame(final bool, opcode byte, payload []byte) []byte {
	firstByte := opcode
	if final {
		firstByte |= 0x80
	}
	frame := []byte{firstByte}
	switch {
	case len(payload) <= 125:
		frame = append(frame, byte(len(payload)))
	case len(payload) <= math.MaxUint16:
		frame = binary.BigEndian.AppendUint16(append(frame, 126), uint16(len(payload)))
	default:
		frame = binary.BigEndian.AppendUint64(append(frame, 127), uint64(len(payload)))
	}
	return append(frame, payload...)
}

func binaryFrame(payload []byte) []byte {
	return serverFrame(true, websocket.BinaryFrame, payload)
}

func closeFrame(code int, reason string) []byte {
	return serverFrame(true, websocket.CloseFrame, append(binary.BigEndian.AppendUint16(nil, uint16(code)), reason...))
}

// maskedFrame is a final frame with a mask, which a server must never send.
func maskedFrame(opcode byte, payload []byte) []byte {
	maskKey := []byte{1, 2, 3, 4}
	frame := append([]byte{0x80 | opcode, 0x80 | byte(len(payload))}, maskKey...)
	for index, value := range payload {
		frame = append(frame, value^maskKey[index%len(maskKey)])
	}
	return frame
}

// dialScriptedServer connects an x/net client to a loopback server that answers the opening
// handshake by hand, writes frames verbatim, then half-closes, so the client reads a plain end
// of stream after them. The server reads until the client hangs up, so a pong never blocks.
func dialScriptedServer(t *testing.T, frames [][]byte, maxPayloadBytes int) *websocket.Conn {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { listener.Close() })
	go func() {
		serverConn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer serverConn.Close()
		reader := bufio.NewReader(serverConn)
		request, readErr := http.ReadRequest(reader)
		if readErr != nil {
			return
		}
		acceptKey := sha1.Sum([]byte(request.Header.Get("Sec-WebSocket-Key") + websocketAcceptGUID))
		fmt.Fprintf(serverConn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(acceptKey[:]))
		for _, frame := range frames {
			serverConn.Write(frame)
		}
		serverConn.(*net.TCPConn).CloseWrite()
		io.Copy(io.Discard, reader)
	}()

	config, err := websocket.NewConfig("ws://"+listener.Addr().String()+"/", wsOrigin)
	require.NoError(t, err)
	clientConn, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	websocketConn, err := websocket.NewClient(config, clientConn)
	require.NoError(t, err)
	t.Cleanup(func() { websocketConn.Close() })
	websocketConn.MaxPayloadBytes = maxPayloadBytes
	return websocketConn
}

// receiveUntilEnd calls receive until it fails with anything but ErrFrameTooLarge, which the
// next call recovers from, and returns every outcome in order.
func receiveUntilEnd(t *testing.T, receive func() ([]byte, error)) []receivedMessage {
	t.Helper()
	var outcomes []receivedMessage
	for range maxScriptedReceives {
		message, err := receive()
		outcomes = append(outcomes, receivedMessage{message: message, err: err})
		if err != nil && !errors.Is(err, websocket.ErrFrameTooLarge) {
			return outcomes
		}
	}
	t.Fatal("the scripted stream never ended")
	return nil
}

func TestGivenAScriptedFrameStreamWhenReceivedThenOnlyACloseReadsDifferentlyFromXNetsMessageCodec(t *testing.T) {
	// x/net's Message.Receive is the reference: every stream must yield the same messages and
	// errors from both, except that a whole, unmasked close with a code yields a *wsCloseError
	// where x/net yields a bare io.EOF.
	atCap := make([]byte, scriptedPayloadCap)
	overCap := make([]byte, scriptedPayloadCap+1)
	overCapIn64Bits := make([]byte, math.MaxUint16+1)
	cutShortClose := closeFrame(4000, "session closing")
	tests := []struct {
		name   string
		frames [][]byte
		// uncapped leaves MaxPayloadBytes zero, so x/net's default cap applies.
		uncapped bool
		// wantOutcomes counts x/net's results, the final error included, so a stream that never
		// arrived cannot pass.
		wantOutcomes int
		wantClose    *wsCloseError
	}{
		{name: "binary messages, then a plain end", frames: [][]byte{binaryFrame([]byte("one")), binaryFrame([]byte("two"))}, wantOutcomes: 3},
		{name: "a text message", frames: [][]byte{serverFrame(true, websocket.TextFrame, []byte("text"))}, wantOutcomes: 2},
		{name: "an empty message", frames: [][]byte{binaryFrame(nil)}, wantOutcomes: 2},
		{name: "a fragmented message", frames: [][]byte{
			serverFrame(false, websocket.BinaryFrame, []byte("ab")),
			serverFrame(false, websocket.ContinuationFrame, []byte("cd")),
			serverFrame(true, websocket.ContinuationFrame, []byte("ef")),
		}, wantOutcomes: 4},
		{name: "a ping before a message", frames: [][]byte{serverFrame(true, websocket.PingFrame, []byte("probe")), binaryFrame([]byte("after"))}, wantOutcomes: 2},
		{name: "a pong before a message", frames: [][]byte{serverFrame(true, websocket.PongFrame, nil), binaryFrame([]byte("after"))}, wantOutcomes: 2},
		{name: "a message at the cap", frames: [][]byte{binaryFrame(atCap)}, wantOutcomes: 2},
		{name: "a message over the cap, then one under it", frames: [][]byte{binaryFrame(overCap), binaryFrame([]byte("after"))}, wantOutcomes: 3},
		{name: "a message over the cap in a 64-bit length", frames: [][]byte{binaryFrame(overCapIn64Bits)}, wantOutcomes: 2},
		{name: "a message over the cap with no cap set", frames: [][]byte{binaryFrame(overCap)}, uncapped: true, wantOutcomes: 2},
		{name: "a masked frame", frames: [][]byte{maskedFrame(websocket.BinaryFrame, []byte("masked"))}, wantOutcomes: 1},
		{name: "a close with no code", frames: [][]byte{serverFrame(true, websocket.CloseFrame, nil)}, wantOutcomes: 1},
		{
			name:         "a close with a code and a reason",
			frames:       [][]byte{closeFrame(4000, "session closing")},
			wantOutcomes: 1,
			wantClose:    &wsCloseError{code: 4000, reason: "session closing"},
		},
		{
			name:         "a message, then a close with a code",
			frames:       [][]byte{binaryFrame([]byte("last")), closeFrame(1011, "")},
			wantOutcomes: 2,
			wantClose:    &wsCloseError{code: 1011},
		},
		{
			name:         "drained messages, then the server's superseded close and the end of the connection",
			frames:       [][]byte{binaryFrame([]byte("drained")), closeFrame(supersededCloseCode, "superseded by reconnect")},
			wantOutcomes: 2,
			wantClose:    &wsCloseError{code: supersededCloseCode, reason: "superseded by reconnect"},
		},
		{name: "a masked close with a code", frames: [][]byte{maskedFrame(websocket.CloseFrame, binary.BigEndian.AppendUint16(nil, 4000))}, wantOutcomes: 1},
		{name: "a close with a one-byte payload", frames: [][]byte{serverFrame(true, websocket.CloseFrame, []byte{0x0f})}, wantOutcomes: 1},
		{name: "a close over the control frame cap", frames: [][]byte{closeFrame(4000, string(make([]byte, maxCloseFramePayload)))}, wantOutcomes: 1},
		// The connection drops inside the close frame, so the code is unknown.
		{name: "a close cut short after its header", frames: [][]byte{cutShortClose[:2]}, wantOutcomes: 1},
		{name: "a close cut short inside its code", frames: [][]byte{cutShortClose[:3]}, wantOutcomes: 1},
		{name: "a close cut short inside its reason", frames: [][]byte{cutShortClose[:6]}, wantOutcomes: 1},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			maxPayloadBytes := scriptedPayloadCap
			if testCase.uncapped {
				maxPayloadBytes = 0
			}
			xnetConn := dialScriptedServer(t, testCase.frames, maxPayloadBytes)
			want := receiveUntilEnd(t, func() ([]byte, error) {
				var message []byte
				err := websocket.Message.Receive(xnetConn, &message)
				return message, err
			})
			got := receiveUntilEnd(t, (&wsConn{conn: dialScriptedServer(t, testCase.frames, maxPayloadBytes)}).Receive)

			require.Len(t, want, testCase.wantOutcomes, "x/net's outcomes: the stream must have arrived")
			require.Len(t, got, len(want))
			for index := range want {
				assert.Equal(t, want[index].message, got[index].message, "message %d", index)
				assert.ErrorIs(t, got[index].err, want[index].err, "error %d", index)
			}
			gotEnd, wantEnd := got[len(got)-1].err, want[len(want)-1].err
			if testCase.wantClose == nil {
				assert.Equal(t, wantEnd, gotEnd)
				return
			}
			assert.Equal(t, io.EOF, wantEnd, "x/net reads every close as a bare EOF")
			assert.Equal(t, testCase.wantClose, gotEnd)
		})
	}
}

func TestGivenAPingFromTheServerWhenReceivedThenItIsAnsweredWithAPongAndTheNextMessageReturns(t *testing.T) {
	pongs := make(chan []byte, 1)
	wssURL := testsupport.NewMockWSS(t, func(serverConn *websocket.Conn) {
		pinger, err := serverConn.NewFrameWriter(websocket.PingFrame)
		if err != nil {
			return
		}
		pinger.Write([]byte("probe"))
		serverConn.PayloadType = websocket.BinaryFrame
		websocket.Message.Send(serverConn, []byte("after"))
		answer, err := serverConn.NewFrameReader()
		if err == nil && answer.PayloadType() == websocket.PongFrame {
			payload, _ := io.ReadAll(answer)
			pongs <- payload
		}
		io.Copy(io.Discard, serverConn)
	})
	websocketConn, err := newTestDialer(testTimeout).Dial(context.Background(), wssURL, RouteAdb)
	require.NoError(t, err)
	t.Cleanup(func() { websocketConn.Close() })

	message, err := websocketConn.Receive()

	require.NoError(t, err)
	assert.Equal(t, []byte("after"), message, "the ping must not surface as a message")
	assert.Equal(t, []byte("probe"), await(t, pongs, "the pong"), "the pong must echo the ping's payload")
}
