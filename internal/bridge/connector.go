package bridge

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// connectorLoopbackIPv4 and connectorLoopbackIPv6 are the only hosts dialed, in this order. adb's
// model: the host server's network_loopback_client (sysdeps/posix/network.cpp, 1.0.39 and 1.0.41
// alike) dials IPv4 first and falls back to IPv6, so a server bound to ::1 alone is reached here as
// adb reaches it. adb falls back on any failure; this daemon on a refused connect only, so a
// timeout is never paid twice. Never 0.0.0.0, never a hostname: the port is on this machine, and
// anything wider lets a forged announcement reach the network.
const (
	connectorLoopbackIPv4 = "127.0.0.1"
	connectorLoopbackIPv6 = "::1"
)

// portDialer dials one address. The production one is a net.Dialer with one deadline for both
// loopbacks.
type portDialer func(ctx context.Context, network, address string) (net.Conn, error)

// openConnector carries one device-originated reverse connection to the customer's local port and
// blocks until it ends. token is a bearer capability: it rides in the hello's body and never in a
// URL, and a verbatim copy of it is scrubbed out of the server's refusal reason below.
//
// Admit before dial, and the order is load-bearing: the hello is the websocket's first frame, so
// nothing local is dialed until the server acks it. An announcement the server never minted
// therefore reaches no port. Which port it may name is screened before this, in announce.
func openConnector(ctx context.Context, cfg *featureConfig, device *ActiveDevice, arn string,
	token string, customerPort int) (err error) {
	// One round trip to the endpoint just dialed, as the notifier's own ack is. The local dial is
	// loopback and takes the same.
	timeout := cfg.dialer.dialTimeout

	// Read per open, never cached: the daemon renews the endpoint. A miss means the device
	// detached, which makes the announcement moot.
	endpoint, ok := device.EndpointFor(arn)
	if !ok {
		return fmt.Errorf("%w: the device detached before the connector opened", errAdbConnClosed)
	}
	ws, err := cfg.dialer.Dial(ctx, endpoint, RouteAdbReverse)
	if err != nil {
		return fmt.Errorf("dial the connector websocket: %w", err)
	}
	// Wrap before sending anything: only this reader reads the session control frame. Its tracker
	// is fresh, not shared, so only this connection's writes gate its swap, and it marks device
	// activity because connector bytes are the device's traffic, unlike a notifier's keepalives.
	stream := newAdbConn(cfg.dialer, device, arn, RouteAdbReverse, ws, cfg.policy,
		newActivityTracker(nil, device.MarkActivity), cfg.logger, withTerminalSessionGone())
	// Every failure past here closes what it opened, so the device's connection ends with the
	// websocket rather than waiting behind a half-spliced one.
	defer func() {
		if err != nil {
			stream.Close()
		}
	}()

	if err := stream.Send(encodeControlFrame(controlStatusConnectorHello, token)); err != nil {
		return fmt.Errorf("send the connector hello: %w", err)
	}
	// The hello is the first frame, so the server answers with NEW (mints the session) or ERROR
	// (refuses with a reason). That reason is server-written text and may echo the token, so scrub
	// it here, while the token is in hand, rather than trust every caller to scrub before logging.
	// Returned as text, not wrapped, so the unscrubbed error cannot be reached through the chain.
	if err := stream.awaitHandshake(timeout); err != nil {
		return fmt.Errorf("redeem the announced reverse connection: %s",
			strings.ReplaceAll(err.Error(), token, "<token>"))
	}

	// One deadline for both loopbacks, so a late refusal cannot double the wait.
	dialer := &net.Dialer{Deadline: time.Now().Add(timeout)}
	targetConn, err := dialCustomerPort(ctx, dialer.DialContext, customerPort)
	if err != nil {
		return fmt.Errorf("dial the customer's local port %d: %w", customerPort, err)
	}
	cfg.logger.Info("reverse connection spliced", "customerPort", customerPort,
		"session", stream.sessionUUID())

	refresher := func(done <-chan struct{}) { runRefresher(ctx, stream, cfg.policy, done) }
	return pipeConn(ctx, targetConn, stream, refresher)
}

// dialCustomerPort connects to customerPort on the IPv4 loopback, or on the IPv6 one when the IPv4
// connect is refused. Any other IPv4 failure ends the dial. If both fail, the error names both
// attempts and wraps the first, the address a customer expects. On Windows a refused connect
// never matches syscall.ECONNREFUSED, so the IPv6 attempt never runs there.
func dialCustomerPort(ctx context.Context, dial portDialer, customerPort int) (net.Conn, error) {
	port := strconv.Itoa(customerPort)
	conn, ipv4Err := dial(ctx, "tcp", net.JoinHostPort(connectorLoopbackIPv4, port))
	if !errors.Is(ipv4Err, syscall.ECONNREFUSED) {
		return conn, ipv4Err
	}
	conn, ipv6Err := dial(ctx, "tcp", net.JoinHostPort(connectorLoopbackIPv6, port))
	if ipv6Err != nil {
		return nil, fmt.Errorf("%w; then %v", ipv4Err, ipv6Err)
	}
	return conn, nil
}
