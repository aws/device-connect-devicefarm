// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// presignedQuery ends each endpoint of the sorter's table: no cause may carry it.
const presignedQuery = "?X-Amz-Signature=" + testsupport.SignatureMarker

// failingFetch is one way a serial fetch fails. Nil dialer: the test dialer. Zero deadline: none.
type failingFetch struct {
	endpoint string
	dialer   *WSDialer
	deadline time.Duration
}

// closing is a server that closes each websocket with closeCode and reason once the request arrives.
func closing(closeCode int, reason string) func(*testing.T) failingFetch {
	return func(t *testing.T) failingFetch {
		mock := testsupport.NewSessionEchoWSS(t)
		mock.CloseNewConnections(closeCode, reason)
		return failingFetch{endpoint: mock.URL}
	}
}

// unserved is an endpoint with no server behind it.
func unserved(endpoint string) func(*testing.T) failingFetch {
	return func(*testing.T) failingFetch { return failingFetch{endpoint: endpoint} }
}

// replying is a server that answers the request with frames, then holds the websocket open.
func replying(frames ...[]byte) func(*testing.T) failingFetch {
	return func(t *testing.T) failingFetch { return failingFetch{endpoint: probeReplyServer(t, frames...)} }
}

// answeringTheUpgrade is a server whose handler answers the websocket upgrade, or never does.
func answeringTheUpgrade(handler http.HandlerFunc, deadline time.Duration) func(*testing.T) failingFetch {
	return func(t *testing.T) failingFetch {
		server := httptest.NewTLSServer(handler)
		t.Cleanup(server.Close)
		trust := x509.NewCertPool()
		trust.AddCert(server.Certificate())
		return failingFetch{endpoint: "wss://" + server.Listener.Addr().String(),
			dialer: NewWSDialer(testWait).TrustRoots(trust), deadline: deadline}
	}
}

func TestGivenAFailedSerialFetchWhenItsErrorIsSortedThenTheKindAndTheCauseNameWhatFailedAndNoStep(t *testing.T) {
	shortReplyTimer := newTestDialer(testWait)
	shortReplyTimer.hostReplyTimeout = 100 * time.Millisecond
	tests := []struct {
		name    string
		arrange func(*testing.T) failingFetch
		kind    FailureKind
		// closeCode is the CloseCode the failure must carry.
		closeCode int
		// cause is the exact text; <host> stands for the endpoint's host, which Host must then carry.
		cause string
	}{
		{"a close with the service's reason", closing(1008, "Device Connect is not supported on this device"), FailureServiceRefused, 1008,
			"Device Farm refused the connection to the device: Device Connect is not supported on this device (close code 1008)"},
		{"a close reason with bytes a terminal must not print", closing(1008, "bad\nline caf\xc3\xa9"), FailureServiceRefused, 1008,
			"Device Farm refused the connection to the device: bad?line caf?? (close code 1008)"},
		{"a close with the catch-all code", closing(1011, "An internal error has occurred"), FailureServiceClosed, 1011,
			"Device Farm closed the connection to the device: An internal error has occurred (close code 1011)"},
		{"a close with no reason", closing(1008, ""), FailureServiceClosed, 1008,
			"Device Farm closed the connection to the device (close code 1008)"},
		{"the session-stopping close", closing(sessionStoppingCloseCode, "remote access session is stopping"),
			FailureSessionStopping, 0, "remote access session " + testDeviceARN + " is stopping"},
		{"an ERROR control frame", func(t *testing.T) failingFetch {
			return failingFetch{endpoint: testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{RefuseNewSessions: -1}).URL}
		}, FailureSessionRefused, 0, "Device Farm refused the connection to the device: " + testsupport.NewSessionRefusalReason},
		{"a connection dropped with no close frame", closing(testsupport.NoCloseFrame, ""), FailureDropped, 0,
			"the connection to Device Farm ended before the device answered"},
		{"no reply within the reply timer", func(t *testing.T) failingFetch {
			return failingFetch{endpoint: probeReplyServer(t), dialer: shortReplyTimer}
		}, FailureReplyTimeout, 0, "the device did not answer within 100ms"},
		{"no reply within the attach's deadline", func(t *testing.T) failingFetch {
			return failingFetch{endpoint: probeReplyServer(t), deadline: 100 * time.Millisecond}
		}, FailureAttachDeadline, 0, "the attach ran out of time before the device answered"},
		// Port 1 is not listening.
		{"a refused TCP connect", unserved("wss://127.0.0.1:1"), FailureUnreachable, 0,
			"cannot connect to Device Farm at <host>: dial tcp 127.0.0.1:1: connect: connection refused"},
		{"a certificate no trusted root signs", func(t *testing.T) failingFetch {
			return failingFetch{endpoint: probeReplyServer(t), dialer: NewWSDialer(testWait)}
		}, FailureUnreachable, 0,
			"cannot connect to Device Farm at <host>: tls: failed to verify certificate: x509: certificate signed by unknown authority"},
		{"an HTTP error at the upgrade", answeringTheUpgrade(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusForbidden)
		}, 0), FailureUpgradeRefused, 0, "Device Farm at <host> rejected the connection request"},
		{"no answer to the upgrade within the attach's deadline", answeringTheUpgrade(func(_ http.ResponseWriter, request *http.Request) {
			<-request.Context().Done()
		}, 100*time.Millisecond), FailureAttachDeadline, 0, "the attach ran out of time before the device answered"},
		{"a device list with no device", replying(controlFrameNew(), okayDevicesReply("")), FailureNoDevice, 0,
			"the remote adb server lists no device"},
		{"an address that is not wss", unserved("ws://127.0.0.1:1"), FailureNotWSS, 0,
			`only wss endpoints are supported for connections to devices: "ws"`},
		{"a control frame on another wire version", replying(decodeHex(t, frameVector(t, "v1-magic-new-frame").Hex)),
			FailureOtherProtocol, 0,
			"first websocket message is not a control frame this client understands: wire protocol 0x00, this client speaks 0x01"},
		{"a failure no kind names", replying(controlFrameNew(), []byte("FAIL0014something went wrong")), FailureOther, 0,
			"the remote adb server answered FAIL instead of OKAY"},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fetch := testCase.arrange(t)
			if fetch.dialer == nil {
				fetch.dialer = newTestDialer(testWait)
			}
			ctx := context.Background()
			if fetch.deadline > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, fetch.deadline)
				defer cancel()
			}
			endpoint, err := url.Parse(fetch.endpoint)
			require.NoError(t, err)
			wantCause := strings.ReplaceAll(testCase.cause, "<host>", endpoint.Host)
			wantHost := ""
			if wantCause != testCase.cause {
				wantHost = endpoint.Host
			}

			_, fetchErr := fetch.dialer.FetchDeviceSerial(ctx, fetch.endpoint+presignedQuery)

			require.Error(t, fetchErr)
			failed := SortConnectionError(fetchErr, testDeviceARN)
			assert.Equal(t, ConnectionFailure{Kind: testCase.kind, Cause: wantCause, CloseCode: testCase.closeCode, Host: wantHost},
				ConnectionFailure{Kind: failed.Kind, Cause: failed.Cause, CloseCode: failed.CloseCode, Host: failed.Host})
		})
	}
}

func TestGivenAReadCutShortWhenSortedThenTheConnectionWasDropped(t *testing.T) {
	// No mock resets a TLS connection or cuts a TLS record on demand, so the read's error is built by hand.
	cuts := map[string]error{
		"a peer reset":          &net.OpError{Op: "read", Net: "tcp", Err: errPeerReset},
		"an end inside a frame": io.ErrUnexpectedEOF,
	}
	for name, cut := range cuts {
		t.Run(name, func(t *testing.T) {
			failed := SortConnectionError(&hostReplyError{cause: fmt.Errorf("read the devices reply: %w", cut)}, testDeviceARN)

			assert.Equal(t, FailureDropped, failed.Kind)
			assert.Equal(t, "the connection to Device Farm ended before the device answered", failed.Cause)
		})
	}
}
