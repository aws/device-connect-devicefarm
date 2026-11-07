package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
)

// AdbProxyServer presents itself to local adb clients as the adb host, piping each
// accepted connection over a websocket to the active device. The device is read from
// the shared *ActiveDevice at accept time and re-read on every websocket swap, so
// the daemon can attach, detach, or renew the endpoint while the listener stays up.
//
// The zero value is not usable; construct with NewAdbProxyServer.
type AdbProxyServer struct {
	listenAddr string
	listenPort int
	refresh    WSRefreshPolicy
	dialer     *WSDialer
	device     *ActiveDevice
	logger     *slog.Logger

	// mu guards listener, written by Listen and read by Addr/Run from other
	// goroutines.
	mu       sync.Mutex
	listener net.Listener
}

// NewAdbProxyServer errors on a nil dialer or device; a nil logger defaults to
// slog.Default. The listener opens only on Listen or Run; call Addr after either to
// learn the bound address (needed when listenPort is 0 for an ephemeral test port).
func NewAdbProxyServer(listenAddr string, listenPort int, refresh WSRefreshPolicy, dialer *WSDialer, device *ActiveDevice, logger *slog.Logger) (*AdbProxyServer, error) {
	if dialer == nil {
		return nil, errors.New("dialer must not be nil")
	}
	if device == nil {
		return nil, errors.New("device must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &AdbProxyServer{
		listenAddr: listenAddr,
		listenPort: listenPort,
		refresh:    refresh,
		dialer:     dialer,
		device:     device,
		logger:     logger,
	}, nil
}

// Addr returns the bound listen address, or nil before Listen/Run opens it. Safe
// to call from another goroutine.
func (s *AdbProxyServer) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

// Listen opens the TCP listener. Split from Run so the caller can claim the port
// and surface EADDRINUSE as a startup error; the daemon binds here before writing
// its readiness pidfile, so a port clash fails before the parent reports success.
// Idempotent, so Run calls it unconditionally.
func (s *AdbProxyServer) Listen() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil {
		return nil
	}
	address := net.JoinHostPort(s.listenAddr, strconv.Itoa(s.listenPort))
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", address, err)
	}
	s.listener = listener
	return nil
}

// Close releases the listener without serving, for a caller that binds with
// Listen but fails before reaching Run. Run also closes it, and a double close is
// a harmless no-op in Go.
func (s *AdbProxyServer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return nil
	}
	return s.listener.Close()
}

// Run serves until ctx is cancelled (SIGINT/SIGTERM), then stops accepting and
// drains in-flight connections before returning.
func (s *AdbProxyServer) Run(ctx context.Context) error {
	if err := s.Listen(); err != nil {
		return err
	}
	s.mu.Lock()
	listener := s.listener
	s.mu.Unlock()
	// Always release the listener, even on the genuine-Accept-error path; the
	// ctx.Done goroutine also closes it.
	defer listener.Close()
	s.logger.Info("adb proxy listening", "address", listener.Addr().String())

	// Closing the listener unblocks the Accept call below when ctx ends.
	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	var activeConns sync.WaitGroup
	for {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			// A cancelled context means the close above caused this: clean shutdown,
			// so drain and return nil rather than reporting an accept error.
			if ctx.Err() != nil {
				s.logger.Info("shutdown requested, draining active connections")
				activeConns.Wait()
				return nil
			}
			return fmt.Errorf("accept on %s: %w", listener.Addr(), acceptErr)
		}

		activeConns.Add(1)
		go func() {
			defer activeConns.Done()
			s.handleConn(ctx, conn)
		}()
	}
}

// handleConn services one local adb client. All errors are logged and swallowed; one bad
// connection must never take the daemon down.
//
// The connection context has no deadline; websocket age is bounded by the refresh cadence.
// Its cancel func is registered with the active device so a `device remove` severs traffic.
//
// Nothing is dialed until the client's service request has been read (readOpening), so a
// client that only selects a transport, or says nothing, mints no session. A forward-family
// request is answered from the local table and never dials at all. host:version, the device
// lists and the features probe are answered from cached server replies. A device tracker gets
// the current list, cached or empty, then is held and pushed the list at every attach and detach,
// with or without a device (serveDeviceTracker). With no device, every other request gets the
// close alone. While reverse is off, a reverse-family request gets a local FAIL and never dials
// either. With reverse on, a reverse-family request waits until the attachment's clear of the
// server's reverse rules settles, or gets a local FAIL at the bound. While the server's notifier
// slot is held elsewhere, it gets a local FAIL at once, on arrival or mid-wait. Every other
// request takes one of the attachment's device-stream slots before the dial. No free slot, no
// dial. A reverse install past the attachment's rule limit is refused after that, still with no
// dial.
func (s *AdbProxyServer) handleConn(ctx context.Context, tcp net.Conn) {
	connLogger := s.logger.With("client", tcp.RemoteAddr().String())
	defer tcp.Close()

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Read and register in one locked step (see trackConn). ok=false means no device bound; the
	// request is still read, since a device tracker is served without one. The forwarding is this
	// attachment's own table, so a rule cannot outlive the device it was installed for.
	info, forwarding, untrack, ok := s.device.trackConn(cancel)
	var attached *DeviceInfo
	if ok {
		defer untrack()
		attached = &info
		connLogger.Debug("local adb client connected", "device", info.Label)
	}

	// The serial comes from the same snapshot as the forwarding.
	clientOpening, err := readOpening(connCtx, tcp, attached)
	if err != nil {
		if errors.Is(err, errSelectRefused) {
			connLogger.Warn("refused a transport select; closing", "error", err)
		} else {
			connLogger.Debug("no request completed before the client went away; closing", "error", err)
		}
		return
	}

	// A tracker is held on the daemon's context: a detach cancels connCtx, and the tracker
	// outlives the detach. One accepted with a device stays registered until the first detach
	// drops it, then waits unregistered.
	if serveDeviceTracker(ctx, tcp, clientOpening, s.device, connLogger) {
		connLogger.Debug("device tracker ended", "service", clientOpening.service)
		return
	}
	if !ok {
		// A device attached since the accept is not this connection's; the client's next one sees it.
		connLogger.Warn("local adb client connected while no device was attached; closing", "service", clientOpening.service)
		return
	}
	if respondForwardLocally(tcp, clientOpening, forwarding, info.Serial, connLogger) {
		connLogger.Debug("answered the forward request locally", "service", clientOpening.service)
		return
	}
	if answerHostQuery(connCtx, tcp, clientOpening, forwarding, info.Serial, connLogger) {
		connLogger.Debug("answered the host request locally", "service", clientOpening.service)
		return
	}
	if refuseReverseFamily(tcp, clientOpening, forwarding, connLogger) {
		connLogger.Debug("refused a reverse request: reverse is off", "service", clientOpening.service)
		return
	}
	if !awaitServerRulesCleared(connCtx, tcp, clientOpening, forwarding, reverseClearTimeout, connLogger) {
		return
	}

	// Budget check sits after the steps that never dial (forward family, reverse-off refusal,
	// reverse hold) and before the dial, so a refusal costs no websocket. A swap redials inside
	// the session and takes no second slot. An attachment built without features has no budget.
	if forwarding != nil {
		if !forwarding.budget.admit() {
			refuseOverBudget(tcp, clientOpening, forwarding, connLogger)
			return
		}
		defer forwarding.budget.release()
	}

	// The reverse family is relayed but counted. An install's hold on its slot ends with the
	// connection, before the budget slot returns; release says what an unanswered one counts as.
	countedRequest, refused := screenReverseRule(tcp, clientOpening, forwarding, connLogger)
	if refused {
		return
	}
	defer countedRequest.release()

	session, err := s.openClientSession(connCtx, tcp, clientOpening, info.ARN, forwarding, connLogger)
	if err != nil {
		return
	}
	countedRequest.markRelayed()
	refresher := func(done <-chan struct{}) {
		runRefresher(connCtx, session, s.refresh, done)
	}

	// The reply's first status word settles a counted reverse request on its way to the client.
	if pipeErr := pipeConn(connCtx, tcp, countedRequest.watching(session), refresher); pipeErr != nil {
		connLogger.Error("pipe ended with error", "error", pipeErr)
		return
	}
	connLogger.Debug("connection closed cleanly")
}

// openClientSession dials the device (the parked websocket when live) and relays the client's
// opening. A failed dial or a refused session goes through openSession. On a final failure a
// client that sent a request reads the reason as its status word; bare bytes get the close alone.
// Logs every failure; the caller only ends the connection.
func (s *AdbProxyServer) openClientSession(ctx context.Context, tcp net.Conn, clientOpening opening, arn string,
	forwarding *attachmentFeatures, logger *slog.Logger) (*adbConn, error) {
	open := func(endpoint string, useParked bool) (*adbConn, error) {
		var warm *warmSocketKeeper
		if useParked {
			warm = forwarding.warmSockets()
		}
		ws, err := warm.adbSocket(ctx, s.dialer, endpoint)
		if err != nil {
			return nil, err
		}
		session := newAdbConn(s.dialer, s.device, arn, RouteAdb, ws, s.refresh, newActivityTracker(nil), logger)
		if err := relayRequest(ctx, tcp, session, clientOpening.pending, clientOpening.selected); err != nil {
			session.Close()
			return nil, fmt.Errorf("hand the client's request to the device: %w", err)
		}
		return session, nil
	}
	session, clientReason, err := openSession(ctx, s.device, arn, forwarding.refusalGate(), open, logger)
	if err == nil {
		return session, nil
	}
	// A detach by another connection's lookup, or by the refresh, cancels ctx mid-opening; its
	// recorded reason is this client's too. A remove by the user records none.
	if clientReason == "" && ctx.Err() != nil {
		if last, detached := s.device.LastDetach(); detached {
			clientReason = last.Reason
		}
	}
	var deviceRefusal *statusFailure
	switch {
	case clientReason != "":
		// A detach cancels ctx on its way here, so the reason goes out before any look at it.
		logger.Warn("refused a local adb connection; answering with the reason", "reason", clientReason, "error", err)
		if clientOpening.service != "" {
			refuseOpening(tcp, clientReason, "could not answer a refused local adb connection", logger)
		}
	case errors.As(err, &deviceRefusal):
		// sendOpening already gave the client adb's own FAIL.
		logger.Warn("adb refused the request", "reason", deviceRefusal.reason)
	case ctx.Err() != nil:
		logger.Debug("the connection ended before the device session opened; closing client", "error", err)
	default:
		logger.Error("failed to open the device session; closing client", "error", err)
	}
	return nil, err
}
