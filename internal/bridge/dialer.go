package bridge

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"time"

	"golang.org/x/net/websocket"
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
)

// errNoRoute means a dial site passed an empty Route: a programming error, refused
// before any connection is attempted.
var errNoRoute = errors.New("no route named for the websocket dial")

// errSchemeNotWSS means the endpoint is not wss://, refused before dialing so the presigned
// query never travels in the clear. The error names the scheme, never the URL.
var errSchemeNotWSS = errors.New("only wss endpoints are dialed")

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
		return "", fmt.Errorf("%w: refused scheme %q", errSchemeNotWSS, parsed.Scheme)
	}
	query := parsed.Query()
	query.Set("path", string(route))
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// tcpKeepAliveInterval is path liveness only: it reaps a connection whose network
// path silently died. A proxied connection's application liveness is its refresh
// cycle plus the reactive reconnect, so it needs no websocket ping; x/net cannot
// send one.
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
	// it exceeds the per-message cap.
	Receive() ([]byte, error)
	Close() error
}

// wsConn wraps an x/net *websocket.Conn. Send emits a BinaryFrame (the Message codec
// picks the opcode from the []byte type), so adb's binary stream is never coerced to
// text. Receive is bounded by MaxPayloadBytes, armed at dial time.
type wsConn struct {
	conn          *websocket.Conn
	writeDeadline time.Duration
}

// Send arms the deadline per write, not once at dial time.
func (c *wsConn) Send(data []byte) error {
	if err := c.conn.SetWriteDeadline(time.Now().Add(c.writeDeadline)); err != nil {
		return err
	}
	return websocket.Message.Send(c.conn, data)
}

func (c *wsConn) Receive() ([]byte, error) {
	var message []byte
	err := websocket.Message.Receive(c.conn, &message)
	return message, err
}

func (c *wsConn) Close() error {
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
	// probeReplyTimeout bounds a keep-alive probe's reply read; a field so tests can
	// shorten it.
	probeReplyTimeout time.Duration
	// trustRoots replaces the system roots (see TrustRoots).
	trustRoots *x509.CertPool
}

func NewWSDialer(dialTimeout time.Duration) *WSDialer {
	return &WSDialer{dialTimeout: dialTimeout, writeDeadline: sendDeadline, probeReplyTimeout: defaultProbeReplyTimeout}
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
		return nil, fmt.Errorf("websocket dial %s://%s route %s: %w",
			cfg.Location.Scheme, cfg.Location.Host, route, withoutEndpoint(err))
	}
	ws.MaxPayloadBytes = maxInboundMessageBytes
	return &wsConn{conn: ws, writeDeadline: d.writeDeadline}, nil
}
