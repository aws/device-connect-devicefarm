// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bufio"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/aws/device-connect-devicefarm/internal/bridge"
	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/control"
	"github.com/aws/device-connect-devicefarm/internal/failure"
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

// spawnDaemon launches the detached daemon child and returns its process handle and
// a channel that closes when the child exits. A package var: a test fake can play
// the child (write the pidfile) with no fork. A fake that spawns nothing returns a
// nil handle and a nil channel, which never closes.
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

// StartResult is what Start found and did. PID is the child's, or the running
// daemon's when AlreadyRunning. StoppedAdbServer is the host:port of an adb
// server Start stopped before the spawn, else empty.
type StartResult struct {
	PID              int
	LogFile          string
	AlreadyRunning   bool
	StoppedAdbServer string
}

// StopResult is what Stop found and did. WasRunning is false when Stop found no
// daemon to stop. Killed is true when Stop called Kill and the daemon was gone
// after it.
type StopResult struct {
	PID        int
	WasRunning bool
	Killed     bool
}

// Start launches the daemon as a detached background process and waits for it to
// report ready (by writing its pidfile). If a daemon is already running it is a
// no-op, matching adb's start-server behavior. Before the spawn it clears the
// listen port (clearListenPort). A child that exits or never reports ready is
// reported with the last error line it logged.
func (m *DaemonManager) Start() (StartResult, error) {
	if pid, running := m.pidFile.RunningPID(); running {
		m.logger.Info("daemon already running", "pid", pid)
		return StartResult{PID: pid, LogFile: m.paths.LogFile, AlreadyRunning: true}, nil
	}

	self, err := os.Executable()
	if err != nil {
		return StartResult{}, fmt.Errorf("locate own executable: %w", err)
	}

	// openNoFollow refuses a symlink where the OS can.
	logOut, err := os.OpenFile(m.paths.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND|openNoFollow, 0o600)
	if err != nil {
		return StartResult{}, fmt.Errorf("open bridge logfile %q: %w", m.paths.LogFile, err)
	}
	// The child inherits the underlying fd, so closing the parent's handle after
	// the spawn does not affect the child's logging.
	defer logOut.Close()

	stoppedAdbServer, err := m.clearListenPort()
	if err != nil {
		return m.unlessRunning("", err)
	}
	// The child appends after this offset, so an older run's error line is never read.
	offset, _ := logOut.Seek(0, io.SeekEnd)
	args := append([]string{DaemonSubcommand}, m.config.AsDaemonArgs()...)
	child, exited, err := spawnDaemon(self, args, logOut)
	if err != nil {
		return StartResult{StoppedAdbServer: stoppedAdbServer}, fmt.Errorf("spawn bridge: %w", err)
	}

	if err := m.waitUntilRunning(startupTimeout, exited); err != nil {
		// A hung child may still hold the listen port: kill it, best-effort, so the
		// next start is not wedged.
		killOrphan(child)
		return m.unlessRunning(stoppedAdbServer, m.startupError(err, offset))
	}
	// The ready pidfile may be a racing start's winner, not this child; report
	// the pid that serves.
	if pid, running := m.pidFile.RunningPID(); running && pid != child.Pid {
		m.logger.Info("daemon already running", "pid", pid)
		return StartResult{PID: pid, LogFile: m.paths.LogFile, AlreadyRunning: true,
			StoppedAdbServer: stoppedAdbServer}, nil
	}
	m.logger.Info("daemon started", "pid", child.Pid, "logfile", m.paths.LogFile)
	return StartResult{PID: child.Pid, LogFile: m.paths.LogFile, StoppedAdbServer: stoppedAdbServer}, nil
}

// unlessRunning swallows err when a daemon runs by now. Two starts can pass
// the pidfile check together; the loser's failure then means the winner serves.
// Both results carry stoppedAdbServer.
func (m *DaemonManager) unlessRunning(stoppedAdbServer string, err error) (StartResult, error) {
	if pid, running := m.pidFile.RunningPID(); running {
		m.logger.Info("daemon already running", "pid", pid)
		return StartResult{PID: pid, LogFile: m.paths.LogFile, AlreadyRunning: true,
			StoppedAdbServer: stoppedAdbServer}, nil
	}
	return StartResult{StoppedAdbServer: stoppedAdbServer}, err
}

// listenErrorPrefix starts the child's error when its TCP bind fails (bridge.AdbProxyServer.Listen).
// The address tells it from the control socket's own "listen on" error.
func listenErrorPrefix(listenAddr string, port int) string {
	return "listen on " + net.JoinHostPort(listenAddr, strconv.Itoa(port)) + ": "
}

// startupError words a child that did not report ready from the last error line it logged
// after offset. A failed bind is probed again.
func (m *DaemonManager) startupError(waitErr error, offset int64) error {
	port, logPath := m.config.ListenPort, m.paths.LogFile
	start := StartCommand(m.config.ListenAddr, port)
	childError := lastChildError(logPath, offset)
	switch {
	case strings.HasPrefix(childError, listenErrorPrefix(m.config.ListenAddr, port)):
		return lostBindError(m.config.ListenAddr, port)
	case childError != "":
		return fmt.Errorf("the bridge exited while starting: %s. Fix what it reports, then run '%s'. Its log is \"%s\".",
			childError, start, logPath)
	case errors.Is(waitErr, errChildExited):
		return fmt.Errorf("the bridge exited while starting and logged no error. Check \"%s\", then run '%s'.", logPath, start)
	}
	return fmt.Errorf("the bridge did not report ready within %s and logged no error, so it was stopped. Check \"%s\", "+
		"then run '%s'.", startupTimeout, logPath, start)
}

// lastChildError is the error value of the last level=ERROR line after offset in the log, or
// "" when there is none. slog's text handler quotes a value only when it must.
func lastChildError(logPath string, offset int64) string {
	logFile, err := os.Open(logPath)
	if err != nil {
		return ""
	}
	defer logFile.Close()
	last := ""
	scanner := bufio.NewScanner(io.NewSectionReader(logFile, offset, math.MaxInt64))
	for scanner.Scan() {
		if value, found := errorValue(scanner.Text()); found {
			last = value
		}
	}
	return last
}

// errorValue reads the error= value of a level=ERROR slog text line: unquoted when quoted, else
// up to the next space.
func errorValue(line string) (string, bool) {
	_, value, found := strings.Cut(line, " error=")
	if !found || !strings.Contains(line, "level=ERROR") {
		return "", false
	}
	if !strings.HasPrefix(value, `"`) {
		bare, _, _ := strings.Cut(value, " ")
		return bare, true
	}
	quoted, err := strconv.QuotedPrefix(value)
	if err != nil {
		return "", false
	}
	unquoted, err := strconv.Unquote(quoted)
	return unquoted, err == nil
}

// Stop sends shutdown over the control socket, waits up to the stop timeout for
// the daemon to exit, kills it if it stays, and clears the pidfile. No-op when
// no daemon runs.
func (m *DaemonManager) Stop() (StopResult, error) {
	pid, running := m.pidFile.RunningPID()
	if !running {
		m.logger.Info("daemon not running")
		return StopResult{}, nil
	}

	proc, err := findProcess(pid)
	if err != nil {
		// Windows fails here for a dead pid; Unix never does. The lock decides:
		// released means exited, held means out of reach.
		if m.stillRunning(pid) {
			lookupFailed := fmt.Errorf("find bridge process %d: %w", pid, err)
			if errors.Is(err, os.ErrPermission) {
				return StopResult{}, failure.WithFix(lookupFailed, "stop it from an elevated shell")
			}
			return StopResult{}, lookupFailed
		}
		m.logger.Info("daemon exited before shutdown was sent", "pid", pid)
		return StopResult{PID: pid}, nil
	}
	// os.FindProcess opens a pidfd on Linux (Go 1.23+); release it on return so
	// the fd is not leaked across the polling waits below.
	defer proc.Release()
	// Re-probe with the handle in hand. With a pidfd (Linux) a held lock proves
	// the handle names the writer, not a reused pid. A bare pid (macOS, or Linux
	// without pidfd) keeps a small window between this probe and the kill.
	if !m.stillRunning(pid) {
		m.logger.Info("daemon not running; nothing sent", "pid", pid)
		return StopResult{PID: pid}, nil
	}

	result := StopResult{PID: pid, WasRunning: true}
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
			return result, nil
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
			return StopResult{}, fmt.Errorf("bridge (pid %d) survived kill after %s; pidfile left in place", pid, killTimeout)
		}
		result.Killed = true
	}
	m.logger.Info("daemon stopped", "pid", pid)
	// A killed daemon leaves its record unlocked; clear it. The probe removes
	// only an unlocked file, so a successor's record stays.
	_, _ = m.pidFile.RunningPID()
	return result, nil
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
		bridge.WithRefusalHandler(m.refusalHandler(ctx)),
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
// status reports the listen port, the reverse setting and the attached devices, each with
// its session ARN and label. list reports the devices alone. With no device, both carry
// the daemon's last detach of its own, when there was one since the last add.
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
				Serial: serial, SessionStartedAt: resolved.StartedAt, SessionExpiresAt: resolved.ExpiresAt}) {
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
			reply := control.Response{OK: true, Port: m.config.ListenPort, ReverseEnabled: m.config.ReverseEnabled}
			current, present := device.Current()
			if !present {
				return withLastDetach(reply, device)
			}
			reply.DeviceLabel, reply.ARN = current.Label, current.ARN
			reply.Devices = attachedDevices(current)
			return reply

		case control.OpList:
			current, present := device.Current()
			if !present {
				return withLastDetach(control.Response{OK: true}, device)
			}
			return control.Response{OK: true, Devices: attachedDevices(current)}

		case control.OpShutdown:
			m.logger.Info("shutdown requested over control channel")
			shutdown()
			return control.Response{OK: true}

		default:
			return control.Response{OK: false, Error: fmt.Sprintf("unknown control op %q", req.Op)}
		}
	}
}

// attachedDevices builds the Devices list that status and list replies share: everything but
// the endpoint, which never crosses the control channel.
func attachedDevices(current bridge.DeviceInfo) []control.AttachedDevice {
	return []control.AttachedDevice{{
		SessionARN: current.ARN, Label: current.Label, Serial: current.Serial, AttachedAt: current.AttachedAt,
		SessionStartedAt: current.SessionStartedAt, SessionExpiresAt: current.SessionExpiresAt}}
}

// withLastDetach adds the daemon's last detach of its own to a no-device reply.
func withLastDetach(resp control.Response, device *bridge.ActiveDevice) control.Response {
	if last, detached := device.LastDetach(); detached {
		resp.LastDetachReason, resp.LastDetachAt = last.Reason, last.At
	}
	return resp
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

// lookUpSession resolves sessionARN under resolveTimeout and judges the answer for the attached
// device: a RUNNING session carries its renewed endpoint; a failure carries the one-line reason
// the user gets and whether the session is gone for good (see detachReason). The refresh tick
// and the refusal handler share it, and differ only in what they do with a kept device.
func (m *DaemonManager) lookUpSession(ctx context.Context, sessionARN string) (bridge.SessionVerdict, error) {
	resolveCtx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	resolved, err := m.remoteAccessClient.GetSession(resolveCtx, sessionARN)
	if err != nil {
		reason, detach := detachReason(sessionARN, err)
		return bridge.SessionVerdict{Detach: detach, Reason: reason}, err
	}
	return bridge.SessionVerdict{Endpoint: resolved.Endpoint, Label: resolved.Label}, nil
}

// reattachCommand is how the detach reasons tell the user to attach a session again.
const reattachCommand = "run '" + config.BinaryName + " device add --session-arn"

// classifiedFailures are the lookup failures that detach beside not found and ended: the daemon
// can no longer read the session, and a retry would not change that.
var classifiedFailures = []error{session.ErrAccessDenied, session.ErrInvalidARN, session.ErrUnsupportedDevice,
	session.ErrNoEndpointYet, session.ErrRequestRejected}

// detachReason words one failed lookup for the user and says whether the device must go. Not
// found, ended, expired credentials, or unreadable (classifiedFailures) detaches. Anything else
// keeps the device: the next lookup may succeed.
func detachReason(sessionARN string, err error) (reason string, detach bool) {
	switch {
	case errors.Is(err, session.ErrSessionNotFound):
		return fmt.Sprintf("remote access session %s no longer exists; start a new session and %s <new ARN>'",
			sessionARN, reattachCommand), true
	case errors.Is(err, session.ErrSessionEnded):
		return fmt.Sprintf("remote access session %s %s; start a new session and %s <new ARN>'",
			sessionARN, endedState(err), reattachCommand), true
	}
	// No flags in the restart: a FAIL line must not quote --allow-adb-reverse without its caveat.
	var rejected *session.RequestRejectedError
	if errors.As(err, &rejected) && rejected.CredentialsExpired() {
		return fmt.Sprintf("the AWS credentials the bridge started with have expired, so it can no longer read remote access "+
			"session %s; refresh the credentials, run '%s bridge stop', start the bridge again from that shell with the same "+
			"options, then %s %s'", sessionARN, config.BinaryName, reattachCommand, sessionARN), true
	}
	for _, class := range classifiedFailures {
		if errors.Is(err, class) {
			return fmt.Sprintf("the bridge can no longer read remote access session %s: %s; restart the bridge from a shell with working AWS credentials",
				sessionARN, classCause(class, err)), true
		}
	}
	return fmt.Sprintf("the bridge could not look up remote access session %s: %v; %s %s' to attach it again",
		sessionARN, err, reattachCommand, sessionARN), false
}

// classCause words a classified failure for the user: the class, plus the service's error code
// when the request was rejected.
func classCause(class, err error) string {
	var rejected *session.RequestRejectedError
	if errors.As(err, &rejected) {
		return fmt.Sprintf("%v (%s)", class, rejected.Code)
	}
	return class.Error()
}

// endedState words an ended session with its status word when the error carries one.
func endedState(err error) string {
	var ended *session.SessionEndedError
	switch {
	case !errors.As(err, &ended):
		return "has ended"
	case ended.Status == "STOPPING":
		return "is stopping (STOPPING)"
	}
	return "has ended (" + ended.Status + ")"
}

// refusalHandler is the bridge's lookup after a refused fresh adb connection. It runs under the
// daemon context, not the connection's: a detach by one refusal must not cut short the lookup
// of another refused behind it, which gets the same reason.
func (m *DaemonManager) refusalHandler(ctx context.Context) bridge.RefusalHandler {
	return func(sessionARN string) bridge.SessionVerdict {
		verdict, err := m.lookUpSession(ctx, sessionARN)
		switch {
		case err == nil:
			m.logger.Info("a refused adb connection found the session running; endpoint refreshed", "arn", sessionARN)
		case verdict.Detach:
			m.logger.Warn("a refused adb connection found the session gone; detaching", "arn", sessionARN, "error", err)
		default:
			m.logger.Warn("a refused adb connection could not look the session up; keeping the device", "arn", sessionARN, "error", err)
		}
		return verdict
	}
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
// The verdict only words the reason the detach records; it does not decide it here.
// The ARN guards on UpdateEndpoint and ClearMatching keep a refresh that races a
// remove or a switch to a DIFFERENT device from disturbing the newer device; a same-ARN remove-then-re-add inside
// the refresh window is an accepted limitation (see ClearMatching). It returns
// whether a device was refreshed (for tests).
func (m *DaemonManager) refreshEndpointOnce(ctx context.Context, device *bridge.ActiveDevice) bool {
	current, present := device.Current()
	if !present {
		return false
	}

	verdict, err := m.lookUpSession(ctx, current.ARN)
	if err != nil {
		// Do not detach on a cancelled context: that is the daemon shutting down,
		// not a dead session, and the device state is about to be torn down anyway.
		if ctx.Err() != nil {
			return false
		}
		if _, cleared := device.ClearMatching(current.ARN, verdict.Reason); cleared {
			m.logger.Warn("session refresh failed; detached device", "error", err, "device", current.Label)
		}
		return false
	}

	if device.UpdateEndpoint(current.ARN, verdict.Endpoint, verdict.Label) {
		m.logger.Debug("session endpoint refreshed", "device", verdict.Label)
		return true
	}
	// The device changed under us (removed or replaced) between the read and the
	// update; the newer state wins and this refresh is a no-op.
	return false
}

// errChildExited is waitUntilRunning's error when the child exits before it reports ready.
var errChildExited = errors.New("the daemon exited before it reported ready")

// waitUntilRunning polls until the daemon reports ready (pidfile present and
// live), the child exits (exited closes) or the timeout elapses.
func (m *DaemonManager) waitUntilRunning(timeout time.Duration, exited <-chan struct{}) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, running := m.pidFile.RunningPID(); running {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("the daemon did not report ready within %s", timeout)
		}
		select {
		case <-exited:
			return errChildExited
		case <-time.After(startupPollInterval):
		}
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

// killOrphan best-effort terminates a spawned-but-not-ready daemon child. It kills
// through the handle the exit watcher waits on: once that watcher has reaped the
// child, the handle reports done and signals nothing, so a reused pid is never hit.
// Every error is intentionally swallowed: the process may already have exited, and
// a failure to kill it must not mask the startup error the caller is about to
// return.
func killOrphan(child *os.Process) {
	if child != nil {
		_ = child.Kill()
	}
}

// realSpawnDaemon starts the binary at self with the given args as a detached
// background process, redirecting its stdout/stderr to logOut. A goroutine waits
// on the child and closes exited when Wait returns clean, so a child that dies
// while starting ends the readiness wait at once. The daemon outlives the CLI.
func realSpawnDaemon(self string, args []string, logOut *os.File) (*os.Process, <-chan struct{}, error) {
	cmd := exec.Command(self, args...)
	cmd.Stdout = logOut
	cmd.Stderr = logOut
	cmd.SysProcAttr = detachedProcessAttributes
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	exited := make(chan struct{})
	go func() {
		if _, err := cmd.Process.Wait(); err == nil {
			close(exited)
		}
	}()
	return cmd.Process, exited, nil
}
