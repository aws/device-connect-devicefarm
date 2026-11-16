// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/websocket"

	"github.com/aws/device-connect-devicefarm/internal/wording"
)

// wsOrigin satisfies x/net's requirement for a syntactically valid Origin
// (DialContext rejects a nil one). The remote endpoint does not act on it.
const wsOrigin = "http://localhost/"

// Route is the server-side route a dial asks for, carried as the endpoint's "path"
// query parameter. The resolved endpoint is path-less (see session.GetSession),
// so every dial site names its route here; a forgotten one is a compile error
// and an empty one fails before any dial.
type Route string

const (
	// RouteAdb reaches the server's /adb session handler: the proxied adb stream.
	RouteAdb Route = "adb"
	// RouteAdbReverse reaches the server's /adbreverse handler: the reverse notifier
	// and its connectors.
	RouteAdbReverse Route = "adbreverse"
	// RouteControl reaches the server's status route, the one the web console uses. The daemon
	// sends one status request per device add there (see FetchSessionTimeLeft) and nothing else.
	RouteControl Route = "control"
)

// errNoRoute means a dial site passed an empty Route: a programming error, refused
// before any connection is attempted.
var errNoRoute = errors.New("no route named for the websocket dial")

// schemeError means the endpoint is not wss://, refused before dialing so the presigned
// query never travels in the clear. The error names the scheme, never the URL.
type schemeError struct{ scheme string }

func (schemeErr *schemeError) Error() string {
	return fmt.Sprintf(wording.RefusalEndpointNotWSSFormat, schemeErr.scheme)
}

// endpointWithRoute returns endpoint with its "path" query parameter set to route, and
// refuses any scheme but wss. url.Values keeps the result correct whether or not the
// endpoint already carries a query string, and replaces a stale path value rather than
// appending a second one.
func endpointWithRoute(endpoint string, route Route) (string, error) {
	if route == "" {
		return "", errNoRoute
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse endpoint: %w", withoutEndpoint(err))
	}
	if parsed.Scheme != "wss" {
		return "", &schemeError{scheme: parsed.Scheme}
	}
	query := parsed.Query()
	query.Set("path", string(route))
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// tcpKeepAliveInterval is path liveness only: it reaps a connection whose network
// path silently died. A proxied connection's application liveness is its refresh
// cycle plus the reactive reconnect, so it needs no websocket ping. Only the parked
// adb websocket is pinged (see warmSocketKeeper).
const tcpKeepAliveInterval = 30 * time.Second

// sendDeadline bounds one outbound websocket write. Without it a peer that stops reading
// wedges the whole connection, because adbConn.Send holds seam.RLock across the write.
// Generous on purpose: a tighter deadline tears down slow but live links.
const sendDeadline = 90 * time.Second

// WSConn is the narrow websocket surface the pipe consumes. Returned instead of
// *websocket.Conn so it stays mockable and the codec details (binary opcode,
// inbound per-message cap) stay encapsulated.
type WSConn interface {
	// Send writes data as a single binary websocket message.
	Send(data []byte) error
	// Receive returns the next inbound message, or websocket.ErrFrameTooLarge if
	// it exceeds the per-message cap. A whole, unmasked close frame with a status code is a
	// *wsCloseError; any other close is io.EOF.
	Receive() ([]byte, error)
	Close() error
}

// maxCloseFramePayload is the websocket protocol's cap on a control frame's payload.
const maxCloseFramePayload = 125

// closeCodeLength is the size of the big-endian status code that opens a close frame's payload.
const closeCodeLength = 2

// A frame header's second byte holds the mask bit and the 7-bit payload length.
const (
	headerLengthByte = 1
	headerMaskBit    = 0x80
	// headerShortLength masks the 7-bit length; 126 and 127 announce an extended one.
	headerShortLength = 0x7f
)

// wsCloseError is a peer's close frame with a status code. It matches io.EOF under errors.Is, as
// x/net reports every close, so callers that stop on io.EOF stop on it too.
type wsCloseError struct {
	code   int
	reason string
}

func (closeError *wsCloseError) Error() string {
	return fmt.Sprintf("websocket closed by the peer: code %d, reason %q", closeError.code, closeError.reason)
}

func (closeError *wsCloseError) Is(target error) bool {
	return target == io.EOF
}

// readCloseFrame reads the payload of the close frame that header opens. Only a whole, unmasked
// payload that starts with a status code is a *wsCloseError. Any other close is a plain io.EOF,
// as x/net reports every close: a code-less one has no code, a code cut short is unknown, and a
// server never masks or exceeds the control frame cap, so a code in either is not the server's.
func readCloseFrame(header []byte, frame io.Reader) error {
	if len(header) <= headerLengthByte || header[headerLengthByte]&headerMaskBit != 0 {
		return io.EOF
	}
	payloadLength := int(header[headerLengthByte] & headerShortLength)
	if payloadLength < closeCodeLength || payloadLength > maxCloseFramePayload {
		return io.EOF
	}
	payload := make([]byte, payloadLength)
	if _, err := io.ReadFull(frame, payload); err != nil {
		return io.EOF
	}
	return &wsCloseError{
		code:   int(binary.BigEndian.Uint16(payload)),
		reason: string(payload[closeCodeLength:]),
	}
}

// pinger is a websocket that sends pings and counts pongs. Not on WSConn, so WSConn doubles and
// wrappers need not implement it.
type pinger interface {
	// Ping writes one ping frame with no payload.
	Ping() error
	// PongsSeen counts the pongs Receive has read: no running Receive, no count.
	PongsSeen() uint64
}

// pingCodec writes a ping frame. Codec.Send holds the x/net write lock, so a ping never interleaves
// with a message or with x/net's own pong.
var pingCodec = websocket.Codec{Marshal: func(any) ([]byte, byte, error) { return nil, websocket.PingFrame, nil }}

// wsConn wraps an x/net *websocket.Conn. Send emits a BinaryFrame (the Message codec
// picks the opcode from the []byte type), so adb's binary stream is never coerced to
// text. Receive is bounded by MaxPayloadBytes, armed at dial time.
type wsConn struct {
	conn          *websocket.Conn
	writeDeadline time.Duration
	// writeMutex holds one write's deadline from arm to clear, so a Ping and a Send never lift
	// each other's bound.
	writeMutex sync.Mutex
	// receiveMutex serializes Receive, in place of the x/net read lock that Receive bypasses.
	receiveMutex sync.Mutex
	// oversized is the frame Receive refused for its size; the next Receive drains it first.
	oversized io.Reader
	pongs     atomic.Uint64
}

// writeBounded runs write under the per-write deadline, then clears it: x/net answers a server
// ping with a pong on its own, long after the last write, and a stale deadline would fail that
// pong and drop the connection.
func (c *wsConn) writeBounded(write func() error) error {
	c.writeMutex.Lock()
	defer c.writeMutex.Unlock()
	if err := c.conn.SetWriteDeadline(time.Now().Add(c.writeDeadline)); err != nil {
		return err
	}
	if err := write(); err != nil {
		return err
	}
	// An error here means the connection closed after the write went out; the write succeeded.
	c.conn.SetWriteDeadline(time.Time{})
	return nil
}

func (c *wsConn) Send(data []byte) error {
	return c.writeBounded(func() error { return websocket.Message.Send(c.conn, data) })
}

func (c *wsConn) Ping() error {
	return c.writeBounded(func() error { return pingCodec.Send(c.conn, nil) })
}

func (c *wsConn) PongsSeen() uint64 {
	return c.pongs.Load()
}

// Receive is x/net's Message.Receive into a []byte, except at a close frame: x/net returns a
// bare io.EOF and drops the code, and Receive reads the payload HandleFrame leaves unread (see
// readCloseFrame).
func (c *wsConn) Receive() ([]byte, error) {
	c.receiveMutex.Lock()
	defer c.receiveMutex.Unlock()
	if c.oversized != nil {
		if _, err := io.Copy(io.Discard, c.oversized); err != nil {
			return nil, err
		}
		c.oversized = nil
	}
	maxPayloadBytes := c.conn.MaxPayloadBytes
	if maxPayloadBytes == 0 {
		maxPayloadBytes = websocket.DefaultMaxPayloadBytes
	}
	for {
		frame, err := c.conn.NewFrameReader()
		if err != nil {
			return nil, err
		}
		// HandleFrame discards the header, so read it first. Len counts header plus payload, and
		// the cap is on the payload. Len is an int, so the check is exact only on 64-bit. A close
		// frame's header carries its mask bit and payload length.
		var header []byte
		if headerReader := frame.HeaderReader(); headerReader != nil {
			header, _ = io.ReadAll(headerReader)
		}
		handled, err := c.conn.HandleFrame(frame)
		if err != nil {
			if frame.PayloadType() == websocket.CloseFrame {
				return nil, readCloseFrame(header, frame)
			}
			return nil, err
		}
		// A ping, answered, or a pong.
		if handled == nil {
			if frame.PayloadType() == websocket.PongFrame {
				c.pongs.Add(1)
			}
			continue
		}
		if int64(handled.Len()-len(header)) > int64(maxPayloadBytes) {
			c.oversized = handled
			return nil, websocket.ErrFrameTooLarge
		}
		message, err := io.ReadAll(handled)
		if err != nil {
			return nil, err
		}
		return message, nil
	}
}

// Close arms the deadline for the close frame x/net writes: a successful write cleared the
// deadline, and a peer that stops reading must not hang the teardown. writeMutex keeps a
// write's clear from lifting it.
func (c *wsConn) Close() error {
	c.writeMutex.Lock()
	defer c.writeMutex.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(c.writeDeadline))
	return c.conn.Close()
}

// WSDialer opens websocket connections to a remote adb host. The endpoint and route
// are per Dial because the active device can change at runtime and one endpoint
// serves several routes. Only wss:// is dialed, with Go's default TLS config;
// TrustRoots is the one override.
type WSDialer struct {
	dialTimeout time.Duration
	// writeDeadline bounds each Send on a dialed connection (see sendDeadline).
	writeDeadline time.Duration
	// hostReplyTimeout bounds a host request's exchange, the serial fetch's included; a
	// field so tests can shorten it.
	hostReplyTimeout time.Duration
	// trustRoots replaces the system roots (see TrustRoots).
	trustRoots *x509.CertPool
}

func NewWSDialer(dialTimeout time.Duration) *WSDialer {
	return &WSDialer{dialTimeout: dialTimeout, writeDeadline: sendDeadline, hostReplyTimeout: defaultHostReplyTimeout}
}

// TrustRoots verifies server chains against pool, not the system roots; a nil pool keeps
// the system roots. Test seam; production passes nil. Sets RootCAs only, so the chain
// check and hostname verification stay on.
func (dialer *WSDialer) TrustRoots(pool *x509.CertPool) *WSDialer {
	dialer.trustRoots = pool
	return dialer
}

// withoutEndpoint strips the wrappers whose text prints the endpoint: x/net's
// DialError (no Unwrap, so errors.As would also miss the x509 cause behind it) and
// url.Error. The presigned query is a credential; no error Dial returns may carry it.
func withoutEndpoint(err error) error {
	var dialErr *websocket.DialError
	if errors.As(err, &dialErr) {
		err = dialErr.Err
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	return err
}

// ErrUpgradeRefused: the server answered the websocket upgrade with an HTTP
// error. The websocket library does not expose the status, so the text names the
// possible causes instead.
var ErrUpgradeRefused = errors.New(wording.RefusalUpgradeRefused)

// dialError is a dial that failed after the endpoint checks. cause is scrubbed of the endpoint.
type dialError struct {
	host  string
	route Route
	cause error
}

func (dialErr *dialError) Error() string {
	// endpointWithRoute let only wss through.
	return fmt.Sprintf("websocket dial wss://%s route %s: %v", dialErr.host, dialErr.route, dialErr.cause)
}

func (dialErr *dialError) Unwrap() error { return dialErr.cause }

// Dial opens one websocket to endpoint on route, with its inbound per-message cap
// armed. The caller owns closing it.
func (d *WSDialer) Dial(ctx context.Context, endpoint string, route Route) (WSConn, error) {
	routed, err := endpointWithRoute(endpoint, route)
	if err != nil {
		return nil, err
	}
	cfg, err := websocket.NewConfig(routed, wsOrigin)
	if err != nil {
		return nil, fmt.Errorf("build websocket config: %w", withoutEndpoint(err))
	}

	// DialContext uses this as the tls.Dialer's NetDialer, so keepalive covers TLS dials.
	cfg.Dialer = &net.Dialer{KeepAlive: tcpKeepAliveInterval}
	if d.trustRoots != nil {
		cfg.TlsConfig = &tls.Config{RootCAs: d.trustRoots}
	}

	// x/net's websocket.Config has no dial-timeout field, so bound the connect
	// with our own context deadline.
	dialCtx, cancel := context.WithTimeout(ctx, d.dialTimeout)
	defer cancel()

	ws, err := cfg.DialContext(dialCtx)
	if err != nil {
		cause := withoutEndpoint(err)
		if errors.Is(cause, websocket.ErrBadStatus) {
			// "bad status" reaches no user; the sentinel names the causes instead.
			cause = ErrUpgradeRefused
		}
		return nil, &dialError{host: cfg.Location.Host, route: route, cause: cause}
	}
	ws.MaxPayloadBytes = maxInboundMessageBytes
	return &wsConn{conn: ws, writeDeadline: d.writeDeadline}, nil
}
