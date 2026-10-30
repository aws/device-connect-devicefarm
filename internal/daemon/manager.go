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
)

// resolveTimeout bounds one session lookup (GetRemoteAccessSession with its retries) on
// refresh, and the lookup plus the serial fetch on add. The SDK's retry budget caps
// attempts, not wall time, and add runs on the inline control loop, so a stalled step would
// wedge every later control command. It sits below the control channel's deadline so a slow
// step surfaces as an add error to the CLI, not a dead socket. A var so tests can shrink it.
var resolveTimeout = 8 * time.Second

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
// control channel, the active-device state, the bridge, and the endpoint refresh
// loop. It holds the session.RemoteAccessClient it resolves ARNs through as a
// readonly member, injected at construction, so a test wires in a fake without a
// package-level seam.
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
// listener, owns the active-device state shared by the bridge and the endpoint
// refresh loop, writes the pidfile only after the control socket is bound (so a
// ready pidfile means the CLI can connect), and serves until ctx is cancelled
// (SIGINT/SIGTERM) or a control shutdown request arrives. This runs in the
// detached child.
func (m *DaemonManager) RunForeground(ctx context.Context) error {
	// Only the running daemon consumes the websocket-refresh cadence and the forward
	// and reverse knobs, so the hidden test overrides are applied here rather than in
	// the shared config seeding, keeping them off every foreground command's path.
	// Applied before Validate so an override cannot smuggle in a value the flags
	// could not.
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
	// server and the endpoint refresh loop the same way a signal does.
	ctx, shutdown := context.WithCancel(ctx)
	defer shutdown()
	// Deferred before the binds so the record drops last, after the control
	// socket is unlinked and the attachment's listeners close. A released record
	// must mean nothing is held.
	defer m.pidFile.Remove()

	dialer := m.newDialer()
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
	// Bind the TCP listener first. A second daemon racing for the same port fails
	// here and never touches the control socket the winner holds: the control
	// Listen below unlinks a socket file it finds. (A racer on another port still
	// comes up and unlinks the winner's socket; rare and accepted.) Binding before
	// the pidfile write makes a port clash a startup error the parent reports, not
	// a daemon that looks ready and then dies.
	if err := server.Listen(); err != nil {
		return err
	}
	defer server.Close()

	controlServer := control.NewLocalSocketServer(m.paths.ControlSocket, m.logger)
	if err := controlServer.Listen(); err != nil {
		return err
	}
	defer controlServer.Close()

	// The pidfile doubles as the readiness signal, so write it only after both the
	// TCP listener and the control socket are bound: once the parent sees the
	// pidfile, a CLI command can connect to the control socket without racing the
	// bind, and the "listening" line it prints is already true.
	if err := m.pidFile.Write(); err != nil {
		return err
	}

	handler := m.controlHandler(ctx, device, dialer, shutdown)

	m.logger.Info("starting "+config.BinaryName+" daemon", "port", m.config.ListenPort)

	// Run the control server and the endpoint refresh loop alongside the bridge; the
	// bridge server's return is the daemon's exit signal.
	go func() {
		if err := controlServer.Serve(ctx, handler); err != nil {
			m.logger.Error("control server stopped with error", "error", err)
		}
	}()
	go runPeriodic(ctx, m.config.EndpointRefreshInterval, func() { m.refreshEndpointOnce(ctx, device) })

	return server.Run(ctx)
}

// newDialer builds the websocket dialer the daemon's dial sites share.
func (m *DaemonManager) newDialer() *bridge.WSDialer {
	return bridge.NewWSDialer(m.config.DialTimeout).TrustRoots(m.websocketRoots)
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

// runPeriodic calls tick every interval until ctx ends. Ticks run in sequence: a slow
// tick delays the next. tick (refreshEndpointOnce) holds the logic, so tests need no
// ticker.
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

// controlHandler builds the control.Handler that answers the CLI's control requests.
// Requests run one at a time on the daemon's inline control loop.
//
// add attaches a device. The daemon holds one at a time: a re-add of the attached device
// succeeds and changes nothing; add refuses a different one. Otherwise the handler
// resolves the session ARN to a websocket endpoint, fetches the device's adb serial over
// it and binds the device. A failed resolve or fetch fails the add and binds nothing: the
// local -s check and the forward list need the serial.
//
// remove detaches the attached device. When the request names an ARN, it must match the
// attached device. That keeps a stale remove from detaching a device the caller did not
// mean.
//
// status reports the listen port and the attached device, when there is one. list
// reports each attached device with its session ARN; the list is empty when none is
// attached.
//
// shutdown cancels the daemon context, then replies.
//
// ctx is the daemon context, and add's resolve and fetch run under it. So a SIGTERM or
// SIGINT that stops the daemon also cancels an add in flight. A control shutdown cannot
// do that: it waits on the inline loop until the add returns. `bridge stop` sends that
// shutdown, so it can wait behind an add for up to resolveTimeout.
func (m *DaemonManager) controlHandler(ctx context.Context, device *bridge.ActiveDevice, dialer *bridge.WSDialer,
	shutdown context.CancelFunc) control.Handler {
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

			resolved, serial, err := m.resolveForAttach(ctx, dialer, req.ARN)
			if err != nil {
				m.logger.Warn("failed to attach the device on add", "error", err, "arn", req.ARN)
				return control.Response{OK: false, Error: err.Error()}
			}
			// Control commands are serialized on the inline accept loop and nothing
			// else calls Set, so nothing can attach between the check above and here;
			// this Set is expected to succeed. The failure branch is kept only as
			// defense-in-depth against a future concurrent caller, and treats the
			// race like the already-attached path.
			if !device.Set(bridge.DeviceInfo{ARN: req.ARN, Endpoint: resolved.Endpoint, Label: resolved.Label,
				Serial: serial}) {
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

		case control.OpList:
			resp := control.Response{OK: true}
			if current, present := device.Current(); present {
				resp.Devices = []control.AttachedDevice{{SessionARN: current.ARN, Label: current.Label}}
			}
			return resp

		case control.OpShutdown:
			m.logger.Info("shutdown requested over control channel")
			shutdown()
			return control.Response{OK: true}

		default:
			return control.Response{OK: false, Error: fmt.Sprintf("unknown control op %q", req.Op)}
		}
	}
}

// resolveForAttach resolves sessionARN, then fetches the device's adb serial from the
// endpoint, both under one resolveTimeout so the add stays under the control deadline.
func (m *DaemonManager) resolveForAttach(ctx context.Context, dialer *bridge.WSDialer,
	sessionARN string) (session.RemoteAccessInfo, string, error) {
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	resolved, err := m.remoteAccessClient.GetSession(ctx, sessionARN)
	if err != nil {
		return session.RemoteAccessInfo{}, "", fmt.Errorf("resolve session: %w", err)
	}
	serial, err := dialer.FetchDeviceSerial(ctx, resolved.Endpoint)
	if err != nil {
		return session.RemoteAccessInfo{}, "", fmt.Errorf("fetch the device serial: %w", err)
	}
	return resolved, serial, nil
}

// refreshEndpointOnce performs a single refresh: if a device is attached, it
// re-resolves the session and updates the stored endpoint in place. Any error the
// SDK surfaces after its retry budget detaches the device: its session can no
// longer be resolved, so the stored endpoint is about to lapse (the URL is
// valid ~5 min, one refresh interval away) and every later dial would fail.
// Detaching on the first failed tick rather than tolerating a few (a credential
// expiry mid-session, e.g. ambient temporary credentials lapsing, presents this
// way) is the deliberate trade-off: surface a dead session promptly for a
// single-user local tool rather than keep a device that can no longer be renewed.
// The ARN guards on UpdateEndpoint and ClearMatching keep a refresh that races a
// remove or a switch to a DIFFERENT device from disturbing the newer device; a same-ARN remove-then-re-add inside
// the refresh window is an accepted limitation (see ClearMatching). It returns
// whether a device was refreshed (for tests).
func (m *DaemonManager) refreshEndpointOnce(ctx context.Context, device *bridge.ActiveDevice) bool {
	current, present := device.Current()
	if !present {
		return false
	}

	resolveCtx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	resolved, err := m.remoteAccessClient.GetSession(resolveCtx, current.ARN)
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
