package bridge

import (
	"context"
	"crypto/x509"
	"fmt"
	"io"
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
