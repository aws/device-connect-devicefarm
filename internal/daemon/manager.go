package daemon

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"github.com/aws/device-connect-devicefarm/internal/bridge"
	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/control"
	"github.com/aws/device-connect-devicefarm/internal/session"
)

// DaemonSubcommand is the hidden subcommand the detached child runs to become
// the foreground bridge. The CLI registers a command under this exact name, and
// Start re-execs the binary with it; keeping the constant here makes the daemon
// package the single source of truth for the re-exec contract.
const DaemonSubcommand = "daemon"

// startupTimeout bounds how long Start waits for the detached child to report
// ready (write its pidfile). It is a package var, not a const, so tests can
// shrink it to exercise the startup-timeout cleanup path without a real wait.
var startupTimeout = 5 * time.Second

const (
	startupPollInterval = 20 * time.Millisecond
	stopPollInterval    = 20 * time.Millisecond
	killTimeout         = 2 * time.Second

	// maxKeepAliveCheckInterval caps how often the keep-alive loop wakes. The
	// effective interval is derived from the idle threshold (see
	// keepAliveCheckInterval) so a probe fires within one tick of a device
	// crossing it, even when the idle threshold is small.
	maxKeepAliveCheckInterval = 15 * time.Second

	// resolveTimeout bounds a single session lookup (the GetRemoteAccessSession
	// call with its retries). The SDK's retry budget caps
	// the number of attempts but not total wall time, and the add path runs on the
	// inline control loop, so a stalled resolve would otherwise wedge every later
	// control command. It sits below the control channel's 10s deadlines so a slow
	// resolve surfaces as a resolve error to the CLI rather than a dead socket.
	resolveTimeout = 8 * time.Second
)

// spawnDaemon launches the detached daemon child and returns its pid. It is a
// package var so tests can substitute a fake that simulates the child (writing
// the pidfile) without forking the real binary.
var spawnDaemon = realSpawnDaemon

// findProcess returns the handle Stop kills through. Package var: a test
// kills the daemon between Stop's first probe and this call, the window the
// re-probe covers.
var findProcess = os.FindProcess

// DaemonManager drives the daemon lifecycle: Start (detach a background daemon),
// Stop (shut it down and wait), Status (report it), and RunForeground (the daemon
// body itself). RunForeground is the composition root that wires config into the
// control channel, the active-device state, the bridge, the keep-alive loop, and
// the endpoint refresh loop. It holds the session.RemoteAccessClient it resolves
// ARNs through as a readonly member, injected at construction, so a test wires in
// a fake without a package-level seam.
type DaemonManager struct {
	config             config.ProxyConfig
	paths              Paths
	remoteAccessClient session.RemoteAccessClient
	pidFile            *PidFile
	logger             *slog.Logger
	// websocketRoots replaces the dialer's system roots. Same-package tests set it to the
	// test CA; production leaves it nil.
	websocketRoots *x509.CertPool
}

// NewManager builds a DaemonManager for the given configuration, runtime paths,
// and remote-access client (the daemon body passes a real
// session.NewRemoteAccessClient; tests pass a fake). A nil logger defaults to
// slog.Default. remoteAccessClient may be nil for a manager that only drives
// Start/Stop/Status, none of which resolve an ARN; RunForeground requires it.
func NewManager(
	cfg config.ProxyConfig,
	paths Paths,
	remoteAccessClient session.RemoteAccessClient,
	logger *slog.Logger,
) *DaemonManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &DaemonManager{
		config:             cfg,
		paths:              paths,
		remoteAccessClient: remoteAccessClient,
		pidFile:            NewPidFile(paths.PidFile, logger),
		logger:             logger,
	}
}

// Start launches the daemon as a detached background process and waits for it to
// report ready (by writing its pidfile). If a daemon is already running it is a
// no-op, matching adb's start-server behavior.
func (m *DaemonManager) Start() error {
	if pid, running := m.pidFile.RunningPID(); running {
		m.logger.Info("daemon already running", "pid", pid)
		return nil
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate own executable: %w", err)
	}

	// openNoFollow refuses a symlink where the OS can.
	logOut, err := os.OpenFile(m.paths.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND|openNoFollow, 0o600)
	if err != nil {
		return fmt.Errorf("open daemon logfile %q: %w", m.paths.LogFile, err)
	}
	// The child inherits the underlying fd, so closing the parent's handle after
	// the spawn does not affect the child's logging.
	defer logOut.Close()

	args := append([]string{DaemonSubcommand}, m.config.AsDaemonArgs()...)
	pid, err := spawnDaemon(self, args, logOut)
	if err != nil {
		// spawnDaemon can return a live pid alongside an error (the child forked but
		// a post-fork step like Process.Release failed), which would otherwise leak
		// a detached child that reports failure to the user. Best-effort kill it,
		// mirroring the startup-timeout path; killOrphan is a no-op for pid <= 0, so
		// this also safely covers the plain cmd.Start() failure.
		m.killOrphan(pid)
		return fmt.Errorf("spawn daemon: %w", err)
	}

	if err := m.waitUntilRunning(startupTimeout); err != nil {
		// The child was spawned and detached (Release means we hold no handle to
		// it) but never reported ready: it may have crashed before writing the
		// pidfile, or it may be hung and still holding the listen port. Best-effort
		// kill it so it cannot wedge the next start; a failure here is tolerated
		// because the process may already be gone.
		m.killOrphan(pid)
		return fmt.Errorf("daemon failed to start (check %q): %w", m.paths.LogFile, err)
	}
	m.logger.Info("daemon started", "pid", pid, "logfile", m.paths.LogFile)
	return nil
}

// Stop sends shutdown over the control socket, waits up to the stop timeout for
// the daemon to exit, kills it if it stays, and clears the pidfile. No-op when
// no daemon runs.
func (m *DaemonManager) Stop() error {
	pid, running := m.pidFile.RunningPID()
	if !running {
		m.logger.Info("daemon not running")
		return nil
	}

	proc, err := findProcess(pid)
	if err != nil {
		// Windows fails here for a dead pid; Unix never does. The lock decides:
		// released means exited, held means out of reach.
		if m.stillRunning(pid) {
			if errors.Is(err, os.ErrPermission) {
				return fmt.Errorf("find daemon process %d: %w; stop it from an elevated shell", pid, err)
			}
			return fmt.Errorf("find daemon process %d: %w", pid, err)
		}
		m.logger.Info("daemon exited before shutdown was sent", "pid", pid)
		return nil
	}
	// os.FindProcess opens a pidfd on Linux (Go 1.23+); release it on return so
	// the fd is not leaked across the polling waits below.
	defer proc.Release()
	// Re-probe with the handle in hand. With a pidfd (Linux) a held lock proves
	// the handle names the writer, not a reused pid. A bare pid (macOS, or Linux
	// without pidfd) keeps a small window between this probe and the kill.
	if !m.stillRunning(pid) {
		m.logger.Info("daemon not running; nothing sent", "pid", pid)
		return nil
	}

	resp, err := control.NewDaemonClient(m.paths.ControlSocket).Send(control.Request{Op: control.OpShutdown})
	grace := m.config.StopTimeout
	switch {
	case errors.Is(err, control.ErrDaemonNotRunning):
		// Lock held, dial failed: shutdown cannot reach it. Skip the stop wait.
		m.logger.Warn("control socket unreachable; killing daemon", "pid", pid)
		grace = 0
	case err != nil:
		// The handler cancels the daemon context before it encodes the reply, so
		// a clean exit can lose the reply. Any other error counts as acknowledged.
		m.logger.Warn("shutdown not confirmed; waiting for the daemon to exit", "pid", pid, "error", err)
	case !resp.OK:
		// Refused: the daemon is reachable, so it keeps the stop timeout before the kill.
		m.logger.Warn("daemon refused shutdown; waiting for it to exit", "pid", pid, "error", resp.Error)
	}
	// A zero grace still probes once, so a daemon already gone is not killed.
	if !m.waitUntilStopped(pid, grace) {
		if !m.stillRunning(pid) {
			m.logger.Info("daemon record released; not escalating", "pid", pid)
			return nil
		}
		if grace > 0 {
			m.logger.Warn("daemon did not stop gracefully; killing it", "pid", pid)
		}
		// Kill may fail because the process just exited; the wait below decides
		// whether the stop succeeded.
		_ = proc.Kill()
		if !m.waitUntilStopped(pid, killTimeout) {
			// Keep the pidfile: the process lives and holds the listen port, so
			// status must not report "not running".
			return fmt.Errorf("daemon %d survived kill after %s; pidfile left in place", pid, killTimeout)
		}
	}
	m.logger.Info("daemon stopped", "pid", pid)
	// A killed daemon leaves its record unlocked; clear it. The probe removes
	// only an unlocked file, so a successor's record stays.
	_, _ = m.pidFile.RunningPID()
	return nil
}

// Status reports the daemon's pid and whether it is running. It is the Manager's
// public accessor over its private pidfile.
func (m *DaemonManager) Status() (pid int, running bool) {
	return m.pidFile.RunningPID()
}

// RunForeground is the daemon body: it binds the control socket and the TCP
// listener, owns the active-device state shared by the bridge and the keep-alive
// loop, writes the pidfile only after the control socket is bound (so a ready
// pidfile means the CLI can connect), and serves until ctx is cancelled
// (SIGINT/SIGTERM) or a control shutdown request arrives. This runs in the
// detached child.
func (m *DaemonManager) RunForeground(ctx context.Context) error {
	// Only the running daemon consumes KeepAliveIdle (via the keep-alive loop), the
	// websocket-refresh cadence and the forward and reverse knobs, so the hidden
	// test overrides are applied here rather than in the shared config seeding,
	// keeping them off every foreground command's path. Applied before Validate so
	// an override cannot smuggle in a value the flags could not.
	m.config.KeepAliveIdle = config.KeepAliveIdleFromEnv(m.config.KeepAliveIdle)
	m.config = m.config.WithRefreshOverridesFromEnv().WithForwardReverseOverridesFromEnv()

	if err := m.config.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	// The add handler and the refresh loop both resolve through this client, so a
	// daemon without one could only fail later, mid-request, inside a detached
	// process. Refuse to start instead.
	if m.remoteAccessClient == nil {
		return fmt.Errorf("daemon requires a remote access client")
	}

	// A control shutdown request cancels this context, which tears down the bridge
	// server and the keep-alive loop the same way a signal does.
	ctx, shutdown := context.WithCancel(ctx)
	defer shutdown()
	// Deferred before the binds so the record drops last, after the control
	// socket is unlinked and the attachment's listeners close. A released record
	// must mean nothing is held.
	defer m.pidFile.Remove()

	controlServer := control.NewLocalSocketServer(m.paths.ControlSocket, m.logger)
	if err := controlServer.Listen(); err != nil {
		return err
	}
	defer controlServer.Close()

	dialer := bridge.NewWSDialer(m.config.DialTimeout).TrustRoots(m.websocketRoots)
	device := bridge.NewActiveDevice(
		bridge.WithAttachmentFeatures(dialer, m.refreshPolicy(), m.attachmentParams(), m.logger),
	)
	// Detaching on the way out closes the attachment's forward listeners, severs its
	// streams and closes its notifier, which nothing else does once the accept loop has
	// stopped. A control add still in flight here could re-attach after this; the daemon
	// exits at once, so the window is bounded by process exit.
	defer func() {
		if info, cleared := device.Clear(); cleared {
			m.logger.Info("detached device on shutdown", "device", info.Label)
		}
	}()
	server, err := bridge.NewAdbProxyServer(
		m.config.ListenAddr, m.config.ListenPort, m.refreshPolicy(), dialer, device, m.logger,
	)
	if err != nil {
		return err
	}
	// Bind the TCP listener before writing the pidfile so a bind failure (e.g. the
	// listen port is already in use) surfaces as a startup error the parent
	// reports, instead of the daemon writing its readiness pidfile and only then
	// dying when Run tries to bind.
	if err := server.Listen(); err != nil {
		return err
	}
	defer server.Close()

	// The pidfile doubles as the readiness signal, so write it only after both the
	// control socket and the TCP listener are bound: once the parent sees the
	// pidfile, a CLI command can connect to the control socket without racing the
	// bind, and the "listening" line it prints is already true.
	if err := m.pidFile.Write(); err != nil {
		return err
	}

	handler := m.controlHandler(ctx, device, shutdown)

	m.logger.Info("starting "+config.BinaryName+" daemon", "port", m.config.ListenPort)

	// Run the control server and the two periodic maintenance loops (keep-alive,
	// endpoint refresh) alongside the bridge; the bridge server's return is the
	// daemon's exit signal.
	go func() {
		if err := controlServer.Serve(ctx, handler); err != nil {
			m.logger.Error("control server stopped with error", "error", err)
		}
	}()
	go runPeriodic(ctx, m.keepAliveCheckInterval(), func() { m.keepAliveOnce(ctx, device, dialer) })
	go runPeriodic(ctx, m.config.EndpointRefreshInterval, func() { m.refreshEndpointOnce(ctx, device) })

	return server.Run(ctx)
}

// refreshPolicy builds the bridge's websocket-refresh policy from config.
// ReattachTimeout is seeded from the dial timeout because both bound one round
// trip to the same endpoint.
func (m *DaemonManager) refreshPolicy() bridge.WSRefreshPolicy {
	return bridge.WSRefreshPolicy{
		Interval:        m.config.WSRefreshInterval,
		FreeSecondWait:  m.config.FreeSecondWait,
		QuietWindow:     m.config.QuietWindow,
		PollInterval:    m.config.QuiescePoll,
		Jitter:          config.DefaultRefreshJitter,
		MaxFailures:     config.DefaultMaxRefreshFailures,
		ReattachTimeout: m.config.DialTimeout,
	}
}

// attachmentParams builds the per-attachment feature limits from config, so the
// bridge takes primitives and never reads the config package.
func (manager *DaemonManager) attachmentParams() bridge.AttachmentParams {
	return bridge.AttachmentParams{
		DeviceStreamStatusTimeout: manager.config.DeviceStreamStatusTimeout,
		MaxDeviceStreams:          manager.config.MaxDeviceStreams,
		MaxForwardRules:           manager.config.MaxForwardRules,
		MaxReverseRules:           manager.config.MaxReverseRules,
		NotifierKeepAlive:         manager.config.NotifierKeepAlive,
		NotifierBackoffInitial:    manager.config.NotifierBackoffInitial,
		NotifierBackoffMax:        manager.config.NotifierBackoffMax,
		ReverseEnabled:            manager.config.ReverseEnabled,
		ListenPort:                manager.config.ListenPort,
	}
}

// runPeriodic invokes tick every interval until ctx is cancelled. It is the
// shared skeleton of the daemon's always-on maintenance loops; each loop's real
// behavior lives in a single-tick decision function (keepAliveOnce,
// refreshEndpointOnce) that reads the shared device state and no-ops when
// nothing is attached, so it stays unit-testable without a ticker. Ticks run
// sequentially on one goroutine per loop: a slow tick delays only its own loop.
func runPeriodic(ctx context.Context, interval time.Duration, tick func()) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick()
		}
	}
}

// controlHandler builds the control.Handler that mutates the daemon's device
// state. add resolves the request ARN to a websocket endpoint and binds it,
// enforcing the one-device limit except that re-adding the same ARN is an
// idempotent no-op; remove drops the device only if its ARN matches (so a stale
// remove cannot disconnect a different device); shutdown cancels the daemon
// context, then replies. ctx is the daemon context and is the parent of the
// add path's bounded resolve, so a SIGTERM/SIGINT that cancels it also cancels an
// add in flight (a control shutdown, being serialized behind the add on the
// inline loop, cannot). `bridge stop` is that shutdown: it queues behind an
// in-flight add, bounded by resolveTimeout.
func (m *DaemonManager) controlHandler(ctx context.Context, device *bridge.ActiveDevice, shutdown context.CancelFunc) control.Handler {
	return func(req control.Request) control.Response {
		switch req.Op {
		case control.OpAdd:
			// Enforce the one-device limit BEFORE resolving, so a re-add of the current
			// device (an idempotent no-op) or a rejected different device never makes a
			// needless AWS call. Only a genuinely new attachment reaches resolution.
			if current, present := device.Current(); present {
				if req.ARN != "" && req.ARN == current.ARN {
					m.logger.Info("device already attached; add is a no-op", "device", current.Label)
					return control.Response{OK: true, DeviceLabel: current.Label, Port: m.config.ListenPort}
				}
				return control.Response{
					OK:    false,
					Error: fmt.Sprintf("a different device is already attached (%s); remove it first", current.Label),
				}
			}

			resolved, err := m.resolveSession(ctx, req.ARN)
			if err != nil {
				m.logger.Warn("failed to resolve session on add", "error", err, "arn", req.ARN)
				return control.Response{OK: false, Error: fmt.Sprintf("resolve session: %v", err)}
			}
			// Control commands are serialized on the inline accept loop and nothing
			// else calls Set, so nothing can attach between the check above and here;
			// this Set is expected to succeed. The failure branch is kept only as
			// defense-in-depth against a future concurrent caller, and treats the
			// race like the already-attached path.
			if !device.Set(bridge.DeviceInfo{ARN: req.ARN, Endpoint: resolved.Endpoint, Label: resolved.Label}) {
				current, _ := device.Current()
				return control.Response{
					OK:    false,
					Error: fmt.Sprintf("a different device is already attached (%s); remove it first", current.Label),
				}
			}
			m.logger.Info("device attached", "device", resolved.Label)
			return control.Response{OK: true, DeviceLabel: resolved.Label, Port: m.config.ListenPort}

		case control.OpRemove:
			current, present := device.Current()
			if !present {
				return control.Response{OK: false, Error: "no device is attached"}
			}
			if req.ARN != "" && req.ARN != current.ARN {
				return control.Response{OK: false, Error: "the attached device does not match the requested session"}
			}
			// Before Clear: the server needs the notifier up to take the request. The control loop
			// is inline, so the next attachment cannot install under the clear.
			device.ClearReverseRulesAtServer(ctx)
			removed, _ := device.Clear()
			m.logger.Info("device removed", "device", removed.Label)
			return control.Response{OK: true, DeviceLabel: removed.Label, Port: m.config.ListenPort}

		case control.OpStatus:
			current, present := device.Current()
			if !present {
				return control.Response{OK: true, Port: m.config.ListenPort}
			}
			return control.Response{OK: true, DeviceLabel: current.Label, Port: m.config.ListenPort}

		case control.OpShutdown:
			m.logger.Info("shutdown requested over control channel")
			shutdown()
			return control.Response{OK: true}

		default:
			return control.Response{OK: false, Error: fmt.Sprintf("unknown control op %q", req.Op)}
		}
	}
}

// resolveSession resolves sessionARN to the adb websocket endpoint and device
// label through the injected remote-access client, shared by the add handler and
// the refresh loop. The whole resolve is bounded by resolveTimeout: the SDK's
// retry budget caps attempts, not wall time, and the add handler runs on the
// inline control loop, so an unbounded resolve would wedge every later control
// command.
func (m *DaemonManager) resolveSession(ctx context.Context, sessionARN string) (session.RemoteAccessInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	return m.remoteAccessClient.GetSession(ctx, sessionARN)
}

// refreshEndpointOnce performs a single refresh: if a device is attached, it
// re-resolves the session and updates the stored endpoint in place. Any error the
// SDK surfaces after its retry budget detaches the device: its session can no
// longer be resolved, so the stored endpoint is about to lapse (the URL is
// valid ~5 min, one refresh interval away), the next keep-alive dial would fail,
// and the remote session would idle out regardless. Detaching on the first failed
// tick rather than tolerating a few (a credential expiry mid-session, e.g. ambient
// temporary credentials lapsing, presents this way) is the deliberate trade-off:
// surface a dead session promptly for a single-user local tool rather than keep a
// device that can no longer be renewed. The ARN guards on UpdateEndpoint and
// ClearMatching keep a refresh that races a remove or a switch to a DIFFERENT
// device from disturbing the newer device; a same-ARN remove-then-re-add inside
// the refresh window is an accepted limitation (see ClearMatching). It returns
// whether a device was refreshed (for tests).
func (m *DaemonManager) refreshEndpointOnce(ctx context.Context, device *bridge.ActiveDevice) bool {
	current, present := device.Current()
	if !present {
		return false
	}

	resolved, err := m.resolveSession(ctx, current.ARN)
	if err != nil {
		// Do not detach on a cancelled context: that is the daemon shutting down,
		// not a dead session, and the device state is about to be torn down anyway.
		if ctx.Err() != nil {
			return false
		}
		if _, cleared := device.ClearMatching(current.ARN); cleared {
			m.logger.Warn("session refresh failed; detached device", "error", err, "device", current.Label)
		}
		return false
	}

	if device.UpdateEndpoint(current.ARN, resolved.Endpoint, resolved.Label) {
		m.logger.Debug("session endpoint refreshed", "device", resolved.Label)
		return true
	}
	// The device changed under us (removed or replaced) between the read and the
	// update; the newer state wins and this refresh is a no-op.
	return false
}

// keepAliveCheckInterval returns how often the keep-alive loop should wake:
// half the idle threshold (so a device is probed within ~one half-threshold of
// crossing it), capped at maxKeepAliveCheckInterval and floored at 1s so a tiny
// idle threshold cannot spin the loop.
func (m *DaemonManager) keepAliveCheckInterval() time.Duration {
	interval := m.config.KeepAliveIdle / 2
	if interval > maxKeepAliveCheckInterval {
		interval = maxKeepAliveCheckInterval
	}
	if interval < time.Second {
		interval = time.Second
	}
	return interval
}

// keepAliveOnce performs a single keep-alive check: if a device is attached and
// has been idle past the configured threshold, it sends a synthetic host:devices
// request so the remote Device Farm session does not go idle, caches the device
// serial the reply names (best-effort), then resets the idle clock via
// MarkActivity. It returns whether a probe was sent (for tests). A failed probe
// is logged and not retried until the next tick.
func (m *DaemonManager) keepAliveOnce(ctx context.Context, device *bridge.ActiveDevice, dialer *bridge.WSDialer) bool {
	info, idle, tracked, present := device.IdleProbe()
	// A tracked connection already holds a websocket, which is what keeps the remote
	// session out of IDLE, so probing then would mint a session for nothing.
	if !present || tracked > 0 || idle < m.config.KeepAliveIdle {
		return false
	}
	serial, err := dialer.SendKeepAlive(ctx, info.Endpoint)
	if err != nil {
		m.logger.Warn("keep-alive probe failed", "error", err, "device", info.Label)
		return false
	}
	if serial != "" {
		device.SetSerial(info.ARN, serial)
	}
	device.MarkActivity()
	m.logger.Debug("keep-alive probe sent", "device", info.Label)
	return true
}

// waitUntilRunning polls until the daemon reports ready (pidfile present and
// live) or the timeout elapses.
func (m *DaemonManager) waitUntilRunning(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, running := m.pidFile.RunningPID(); running {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("timed out after %s waiting for pidfile %q", timeout, m.paths.PidFile)
		}
		time.Sleep(startupPollInterval)
	}
}

// stillRunning reports whether the pidfile still names pid under its writer's
// lock. Stop calls it before the shutdown and each kill to skip a reused pid.
// RunningPID clears a stale file on the way.
func (m *DaemonManager) stillRunning(pid int) bool {
	current, running := m.pidFile.RunningPID()
	return running && current == pid
}

// waitUntilStopped polls until the given process is gone or the timeout elapses,
// returning whether it stopped.
func (m *DaemonManager) waitUntilStopped(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !processAlive(pid) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(stopPollInterval)
	}
}

// killOrphan best-effort terminates a spawned-but-not-ready daemon child by pid.
// Start calls it on the startup-timeout path, where the parent holds no process
// handle (the child was Released on spawn). Every error is intentionally
// swallowed: the process may already have exited, and a failure to kill it must
// not mask the startup error the caller is about to return.
func (m *DaemonManager) killOrphan(pid int) {
	if pid <= 0 {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	defer proc.Release()
	_ = proc.Kill()
}

// realSpawnDaemon starts the binary at self with the given args as a detached
// background process, redirecting its stdout/stderr to logOut.
// It returns the child's pid after fully releasing it (the parent never reaps
// it), mirroring adb's fork-server detachment.
func realSpawnDaemon(self string, args []string, logOut *os.File) (int, error) {
	cmd := exec.Command(self, args...)
	cmd.Stdout = logOut
	cmd.Stderr = logOut
	cmd.SysProcAttr = detachedProcessAttributes
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Release(); err != nil {
		return pid, err
	}
	return pid, nil
}
