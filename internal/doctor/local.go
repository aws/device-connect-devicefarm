// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package doctor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/aws/device-connect-devicefarm/internal/adbwire"
	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/control"
	"github.com/aws/device-connect-devicefarm/internal/daemon"
	"github.com/aws/device-connect-devicefarm/internal/forward"
)

// Check ids, shared so Needs can name them.
const (
	checkRuntimeDir   = "local.runtime-dir"
	checkDaemon       = "local.daemon"
	checkListenPort   = "local.listen-port"
	checkForwardTable = "local.forward-table"
)

const (
	// localTimeout bounds a check that touches only this machine.
	localTimeout = 2 * time.Second
	// daemonTimeout fits the control client's dial plus request timeouts.
	daemonTimeout = 15 * time.Second
	// listenPortTimeout covers one bind plus one occupant round trip.
	listenPortTimeout = 5 * time.Second
	// occupantReplyTimeout bounds the host:version exchange with whatever holds the port.
	occupantReplyTimeout = time.Second
	// maxControlSocketPath is the longest socket path every supported OS binds; macOS sun_path
	// is 104 bytes with the NUL, Linux 108.
	maxControlSocketPath = 103
)

const (
	bridgeStopCommand  = config.BinaryName + " bridge stop"
	bridgeStartCommand = config.BinaryName + " bridge start"
	doctorCommand      = config.BinaryName + " doctor"
	// restartDaemonHint is the stop-then-start every restart hint quotes.
	restartDaemonHint = bridgeStopCommand + " && " + bridgeStartCommand
	// reverseCaveat follows every mention of --allow-adb-reverse.
	reverseCaveat = "'--allow-adb-reverse' lets the device open connections to this machine, so use it only with a " +
		"device you trust; a public physical device can run software you do not control."
	runtimeDirHint = "the client cannot use this working directory, so '" + bridgeStartCommand + "' cannot launch the bridge. " +
		"Set '" + daemon.RuntimeDirEnvVar + "' to a directory with a short path that only this user can write, then rerun '" + doctorCommand +
		"'. Every " + config.BinaryName + " command reads that variable, so 'bridge start' and the bridge it launches use the new directory too."
	deadDaemonHint = "a bridge is registered but did not report its status. Run '" + restartDaemonHint +
		"'. The stop ends that bridge and the start launches a new one."
)

// daemonFlags renders the listener flags a restart keeps.
func daemonFlags(port int, reverse bool) string {
	flags := fmt.Sprintf("--port %d", port)
	if reverse {
		flags += " --allow-adb-reverse"
	}
	return flags
}

// restartWith renders the restart command: stop, then start on port with reverse as asked and the
// session the daemon reported.
func restartWith(reply control.Response, port int, reverse bool) string {
	command := restartDaemonHint + " " + daemonFlags(port, reverse)
	if reply.ARN != "" {
		command += " --session-arn " + reply.ARN
	}
	return command
}

// caveatIf appends reverseCaveat when the quoted command carries --allow-adb-reverse.
func caveatIf(hint string, reverse bool) string {
	if reverse {
		return hint + " " + reverseCaveat
	}
	return hint
}

// safeValue renders a user-supplied value for a one-line row: a control character becomes its Go
// escape, so a stray newline cannot forge a report row; every other character stays, so a Windows
// path keeps its backslashes.
func safeValue(value string) string {
	var out strings.Builder
	for _, char := range value {
		if unicode.IsControl(char) {
			quoted := strconv.QuoteRune(char)
			out.WriteString(quoted[1 : len(quoted)-1])
			continue
		}
		out.WriteRune(char)
	}
	return out.String()
}

// reverseOffHint restarts the daemon with reverse on, its port and device kept, then states the caveat.
func reverseOffHint(reply control.Response) string {
	return caveatIf(fmt.Sprintf("the bridge runs with adb reverse off, so every 'adb reverse' fails. Run '%s' to start it again "+
		"with reverse on.", restartWith(reply, reply.Port, true)), true)
}

const (
	loopbackHint = "'adb forward' could not open or reach a port on 127.0.0.1, so every 'adb forward' would fail. Allow '" +
		config.BinaryName + "' to listen on 127.0.0.1 in the sandbox, container or firewall rules, then rerun '" + doctorCommand +
		"'. Each 'adb forward' rule opens one port on 127.0.0.1, and every forwarded connection enters through it."
	adbPointHint = "point adb at it with 'adb -P <n>'"
)

// listenConfigHint names the listen flags to fix. On the default address only --port can be at
// fault, so a user who never passed --listen-addr is not told it exists.
func listenConfigHint(listenAddr string) string {
	if listenAddr == config.DefaultListenAddr {
		return "'--port' does not name a port the bridge can bind. Pass '--port' a number from 1 to 65535, as in '" + bridgeStartCommand +
			" --port 5037'. The bridge listens on exactly that port, so point adb at the same port."
	}
	return "the listen flags do not name an address the bridge can bind. Pass '--port' a number from 1 to 65535 and " +
		"'--listen-addr' a non-empty address with no port or brackets, as in '" + bridgeStartCommand + " --port 5037 --listen-addr 127.0.0.1'. The bridge " +
		"binds exactly that address, so point adb at the same port."
}

// adbServerHint: bridge start stops the adb server on the probed address before it starts the daemon;
// a program that takes the port back gets AdbServerKeepsPortFix.
func adbServerHint(listenAddr string, port int) string {
	return fmt.Sprintf("an adb server is listening on %s, so the bridge cannot bind it until that server stops. Run '%s': it stops that "+
		"server first, as 'adb kill-server' would. If a program that restarts adb, such as Android Studio, takes the port back, %s",
		net.JoinHostPort(listenAddr, strconv.Itoa(port)), daemon.StartCommand(listenAddr, port),
		daemon.AdbServerKeepsPortFix(listenAddr, port, daemon.LeadQuotesStart))
}

// lowPortHint: the bind was refused for want of permission.
func lowPortHint(port int) string {
	return fmt.Sprintf("this user may not bind port %d; ports below 1024 need administrator rights on most systems. "+
		"Run '%s --port <n>' with a port above 1023 and %s. The bridge then listens on that port instead of %d.",
		port, bridgeStartCommand, adbPointHint, port)
}

// reservedPortHint: Windows refused the bind with access denied: an excluded port range, or a
// wildcard exclusive holder that did not answer the probe.
func reservedPortHint(port int) string {
	return fmt.Sprintf("Windows refuses to bind port %d: the port sits in a range that Hyper-V, WSL or Docker reserved, or another "+
		"program holds it exclusively. See the reserved ranges with 'netsh int ipv4 show excludedportrange protocol=tcp', then run "+
		"'%s --port <n>' with a port outside them and %s. The bridge then listens on that port instead of %d.",
		port, bridgeStartCommand, adbPointHint, port)
}

// listenAddrHint: the listen address is not one of this machine's.
func listenAddrHint(address string, port int) string {
	return fmt.Sprintf("'%s' is not an address of this machine, so nothing can bind it. Pass '--listen-addr' an address this "+
		"machine owns, as in '%s --listen-addr 127.0.0.1 --port %d'. The bridge binds exactly that address, so adb must "+
		"reach it there.", safeValue(address), bridgeStartCommand, port)
}

// winsockAccessDenied is WSAEACCES, which a Windows bind error carries for an excluded range or an
// exclusive holder; os.ErrPermission does not match it there.
const winsockAccessDenied = syscall.Errno(10013)

// bindErrorHint picks the summary and fix for a bind error nobody answered; goos is a parameter so
// one OS tests every arm. Windows access denied blames a reserved range; in use blames another
// program, which may listen on an interface the loopback probe cannot reach; any other error (a
// name that does not resolve, an address not on this machine) blames the address.
func bindErrorHint(err error, goos string, listenAddr string, port int) (summary, hint string) {
	switch {
	case goos == "windows" && errors.Is(err, winsockAccessDenied):
		return safeValue(err.Error()), reservedPortHint(port)
	case daemon.HolderMayAnswer(err, goos):
		return heldPortPair(listenAddr, port)
	default:
		return safeValue(err.Error()), listenAddrHint(listenAddr, port)
	}
}

// heldPortPair is the summary and fix when another program holds the port; the fix is the text
// bridge start prints.
func heldPortPair(listenAddr string, port int) (summary, hint string) {
	listenAddr = safeValue(listenAddr)
	return fmt.Sprintf("cannot bind %s because another program is listening on it", net.JoinHostPort(listenAddr, strconv.Itoa(port))),
		daemon.HeldPortText(listenAddr, port)
}

// otherPortDaemonHint moves the running daemon to the probed port, its flags and device kept, or
// points adb at the port it holds.
func otherPortDaemonHint(reply control.Response, port int) string {
	return caveatIf(fmt.Sprintf("the running bridge listens on port %d, not %d. "+
		"Run '%s' to move it to port %d, or point adb at it with 'adb -P %d'.",
		reply.Port, port, restartWith(reply, port, reply.ReverseEnabled), port, reply.Port), reply.ReverseEnabled)
}

// runState is what earlier checks leave for later ones.
type runState struct {
	paths daemon.Paths
	// mu guards the daemon pair: a reader with no need on the daemon row may run while a daemon
	// check that outlived its timeout still writes.
	mu sync.Mutex
	// daemonStatus is the running daemon's status reply; OK is false when none answered.
	daemonStatus control.Response
	daemonPID    int
	// endpoint of the resolved session. It is a credential; no row prints it.
	endpoint string
	// returnedARN: what the service returned for the resolved session. It can differ from the
	// asked ARN.
	returnedARN string
	// lookupOnce guards lookup, the run's one session lookup (lookUpSession).
	lookupOnce sync.Once
	lookup     sessionLookup
	// sessionUUID: opened by relay.session, resumed by relay.reattach.
	sessionUUID string
}

// setDaemon records the running daemon's reply and pid.
func (state *runState) setDaemon(reply control.Response, pid int) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.daemonStatus, state.daemonPID = reply, pid
}

// daemon returns the running daemon's reply and pid; OK is false when none answered.
func (state *runState) daemon() (control.Response, int) {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.daemonStatus, state.daemonPID
}

// localChecks are the Local machine rows, in run order. They touch only this machine, but the
// bridge row can make the run's session lookup (lookUpReturnedARN).
func localChecks(inputs Inputs, state *runState) []Check {
	return []Check{
		{ID: checkRuntimeDir, Title: "working directory", Category: CategoryLocal, Timeout: localTimeout,
			Run: func(context.Context) Result { return checkRuntimeDirectory(state) }},
		{ID: checkDaemon, Title: "bridge", Category: CategoryLocal, Needs: []string{checkRuntimeDir}, Timeout: daemonTimeout,
			Run: func(ctx context.Context) Result { return checkRunningDaemon(ctx, inputs, state) }},
		{ID: checkListenPort, Title: "listen port", Category: CategoryLocal, Needs: []string{checkDaemon}, Timeout: listenPortTimeout,
			Run: func(context.Context) Result { return checkListenPortFree(inputs, state) }},
		{ID: checkForwardTable, Title: "adb forward", Category: CategoryLocal, Timeout: localTimeout,
			Run: func(ctx context.Context) Result { return checkForwardTableBinds(ctx, inputs) }},
	}
}

// checkRuntimeDirectory resolves the paths (this creates the dir, as every command does),
// checks the socket path length, then writes and removes one probe file. Rows name the directory,
// never its files: the user controls only the directory.
func checkRuntimeDirectory(state *runState) Result {
	// The whole summary is escaped: an OS error repeats the path, newline and all.
	fail := func(summary string) Result {
		return Result{Status: Fail, Summary: safeValue(summary), Hint: runtimeDirHint}
	}
	paths, err := daemon.DefaultPaths()
	if err != nil {
		return fail("cannot use the working directory: " + err.Error())
	}
	if over := len(paths.ControlSocket) - maxControlSocketPath; over > 0 {
		return fail(fmt.Sprintf("working directory %s is %d bytes too long for this system", paths.RuntimeDir, over))
	}
	probe, err := os.CreateTemp(paths.RuntimeDir, ".doctor-")
	if err != nil {
		return fail(fmt.Sprintf("cannot write a file in the working directory %s: %v", paths.RuntimeDir, err))
	}
	// Bytes, not an empty file: a pidfile write needs space, and a full disk still hands out inodes.
	_, writeErr := probe.WriteString("probe")
	if err := cmp.Or(writeErr, probe.Close(), os.Remove(probe.Name())); err != nil {
		return fail(fmt.Sprintf("cannot write a file in the working directory %s: %v", paths.RuntimeDir, err))
	}
	state.paths = paths
	return Result{Status: Pass, Summary: "working directory " + safeValue(paths.RuntimeDir) + " is writable"}
}

// causeWithoutSocketPath renders err without the control socket path: a *net.OpError in the chain
// names it, so only its inner cause stays; any other error reads verbatim.
func causeWithoutSocketPath(err error) string {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Err != nil {
		return opErr.Err.Error()
	}
	return err.Error()
}

// checkRunningDaemon reads the pidfile, then asks a running daemon for its status. A locked
// pidfile with a dead socket is a FAIL: device add and device list start nothing and fail.
func checkRunningDaemon(ctx context.Context, inputs Inputs, state *runState) Result {
	pid, running := daemon.NewManager(inputs.Config, state.paths, nil, inputs.Logger).Status()
	if !running {
		return Result{Status: Pass, Summary: "not running; bridge start, device add or device list will launch it"}
	}
	reply, err := control.NewDaemonClient(state.paths.ControlSocket).Send(control.Request{Op: control.OpStatus})
	switch {
	case errors.Is(err, control.ErrDaemonNotRunning):
		return Result{Status: Fail, Summary: fmt.Sprintf("a bridge (pid %d) is registered but does not answer: %s", pid, causeWithoutSocketPath(err)),
			Hint: deadDaemonHint}
	case err != nil:
		return Result{Status: Fail, Summary: fmt.Sprintf("a bridge (pid %d) is registered but did not report its status: %s", pid, causeWithoutSocketPath(err)),
			Hint: deadDaemonHint}
	case !reply.OK:
		return Result{Status: Fail, Summary: fmt.Sprintf("a bridge (pid %d) is registered but refused to report its status: %s", pid, reply.Error),
			Hint: deadDaemonHint}
	}
	state.setDaemon(reply, pid)
	askedARN := strings.TrimSpace(inputs.SessionARN)
	returnedARN := lookUpReturnedARN(ctx, inputs, state, askedARN, reply)
	return bridgeRowResult(askedARN, returnedARN, reply, pid)
}

// lookUpReturnedARN is the ARN the service returns for the asked session; askedARN with no
// answer. It asks for the run's session lookup only when the bridge holds a session or a detach
// reason that the asked ARN does not name. The row needs no credentials or network: with no
// answer it compares the asked ARN. A session that is not RUNNING gives no answer.
func lookUpReturnedARN(ctx context.Context, inputs Inputs, state *runState, askedARN string, reply control.Response) string {
	nothingHeld := reply.ARN == "" && reply.LastDetachReason == ""
	askedNamesIt := askedARN == reply.ARN || strings.Contains(reply.LastDetachReason, askedARN)
	if askedARN == "" || nothingHeld || askedNamesIt {
		return askedARN
	}
	// Ask only while the row's deadline leaves the lookup its whole bound. A lookup cut short
	// would fail this row, and the session row would read the cut answer.
	if deadline, ok := ctx.Deadline(); ctx.Err() != nil || ok && time.Until(deadline) < remoteTimeout+probeMargin {
		return askedARN
	}
	if lookup := state.lookUpSession(ctx, inputs); lookup.err == nil {
		return lookup.info.ReturnedARN(askedARN)
	}
	return askedARN
}

// attachedIsAsked reports whether the attached session is the asked one. The bridge attaches
// under the ARN the service returns, so either ARN matches.
func attachedIsAsked(attachedARN, askedARN, returnedARN string) bool {
	return attachedARN != "" && (attachedARN == askedARN || attachedARN == returnedARN)
}

// bridgeRowResult builds the row for a bridge that answered status. WARN on the first match: a
// detached device (unless the reason is about another session than the asked one), the asked
// session not attached, reverse off. Never FAIL: the bridge runs. askedARN and returnedARN (see
// lookUpReturnedARN) both name the asked session.
func bridgeRowResult(askedARN, returnedARN string, reply control.Response, pid int) Result {
	device := "no device"
	if reply.DeviceLabel != "" {
		device = fmt.Sprintf("device %s, session %s", safeValue(reply.DeviceLabel), safeValue(reply.ARN))
	}
	reverse := "reverse on"
	if !reply.ReverseEnabled {
		reverse = "reverse off"
	}
	summary := fmt.Sprintf("running (pid %d) on port %d; %s; %s", pid, reply.Port, device, reverse)
	// The reply carries a detach reason only while no device is attached.
	if reply.LastDetachReason != "" {
		summary += "; the bridge detached the last device"
	}
	askedMissing := askedARN != "" && !attachedIsAsked(reply.ARN, askedARN, returnedARN)
	if askedMissing {
		summary += fmt.Sprintf("; session %s is not attached", safeValue(askedARN))
	}
	// Every detach reason names its session.
	detachAnswersAsk := askedARN == "" || strings.Contains(reply.LastDetachReason, askedARN) ||
		returnedARN != "" && strings.Contains(reply.LastDetachReason, returnedARN)
	switch {
	case reply.LastDetachReason != "" && detachAnswersAsk:
		return Result{Status: Warn, Summary: summary, Hint: detachedHint(reply.LastDetachReason)}
	case askedMissing:
		return Result{Status: Warn, Summary: summary, Hint: notAttachedHint(reply, askedARN)}
	case !reply.ReverseEnabled:
		return Result{Status: Warn, Summary: summary, Hint: reverseOffHint(reply)}
	}
	return Result{Status: Pass, Summary: summary}
}

// detachedHint quotes the bridge's reason, which names the fix.
func detachedHint(reason string) string {
	return fmt.Sprintf("the bridge detached the device, so adb finds no device: %s.", safeValue(reason))
}

// notAttachedHint names the add command for the asked session. The bridge holds one device, so
// the hint puts 'device remove' first when a device is attached.
func notAttachedHint(reply control.Response, askedARN string) string {
	arn := safeValue(askedARN)
	add := fmt.Sprintf("'%s device add --session-arn %s'", config.BinaryName, arn)
	if reply.DeviceLabel == "" {
		return fmt.Sprintf("session %s is not attached to the bridge, so adb cannot reach its device. Run %s.", arn, add)
	}
	return fmt.Sprintf("session %s is not attached to the bridge, so adb cannot reach its device. Run '%s device remove', "+
		"then %s. The remove detaches %s.", arn, config.BinaryName, add, safeValue(reply.DeviceLabel))
}

// checkListenPortFree binds the listen address once and frees it; a running daemon on the port
// passes with no bind. A bind refused for want of permission fails at once. When a local socket
// holds the port (daemon.HolderMayAnswer), one host:version tells an adb server from any other
// occupant; otherwise, or with no answer, the bind error itself picks the hint. An adb server is
// a WARN: bridge start stops it, the doctor never does.
func checkListenPortFree(inputs Inputs, state *runState) Result {
	cfg := inputs.Config
	if err := cfg.Validate(); err != nil {
		return Result{Status: Fail, Summary: err.Error(), Hint: listenConfigHint(cfg.ListenAddr)}
	}
	address := net.JoinHostPort(cfg.ListenAddr, strconv.Itoa(cfg.ListenPort))
	if daemonReply, daemonPID := state.daemon(); daemonReply.OK {
		if daemonReply.Port == cfg.ListenPort {
			return Result{Status: Pass, Summary: fmt.Sprintf("%s is held by the running bridge (pid %d)", address, daemonPID)}
		}
		// A daemon on another port is the finding; no bind.
		return Result{Status: Warn, Summary: fmt.Sprintf("the running bridge (pid %d) listens on port %d, not %d",
			daemonPID, daemonReply.Port, cfg.ListenPort), Hint: otherPortDaemonHint(daemonReply, cfg.ListenPort)}
	}
	listener, err := net.Listen("tcp", address)
	if err == nil {
		listener.Close()
		return Result{Status: Pass, Summary: address + " is free"}
	}
	if errors.Is(err, os.ErrPermission) {
		return Result{Status: Fail, Summary: safeValue(err.Error()), Hint: lowPortHint(cfg.ListenPort)}
	}
	summary, hint := bindErrorHint(err, runtime.GOOS, cfg.ListenAddr, cfg.ListenPort)
	if daemon.HolderMayAnswer(err, runtime.GOOS) {
		version, connected, adbServer := daemon.ProbeOccupant(daemon.OccupantAddress(cfg.ListenAddr, cfg.ListenPort), occupantReplyTimeout)
		if adbServer {
			return Result{Status: Warn, Summary: fmt.Sprintf("an adb server (version %d) is listening on %s", version, safeValue(address)),
				Hint: adbServerHint(safeValue(cfg.ListenAddr), cfg.ListenPort)}
		}
		if connected {
			summary, hint = heldPortPair(cfg.ListenAddr, cfg.ListenPort)
		}
	}
	return Result{Status: Fail, Summary: summary, Hint: hint}
}

// closingOpener stands in for the device-stream opener: it closes every accepted connection.
type closingOpener struct{}

func (closingOpener) OpenStream(conn net.Conn, _ adbwire.AdbSpec, turn forward.OpenTurn) {
	conn.Close()
	turn.Done()
}

// checkForwardTableBinds installs one tcp:0 rule, dials its listener and closes the table. No IPv6
// row: forward binds 127.0.0.1 only; the reverse connector tries ::1 only after a refused IPv4 connect.
func checkForwardTableBinds(ctx context.Context, inputs Inputs) Result {
	table := forward.NewForwardRulesTable(closingOpener{}, 1, inputs.Logger)
	defer table.Close()
	installed := table.Install("tcp:0", "tcp:0", false)
	if installed.Status != forward.AdbInstallOK {
		return Result{Status: Fail, Summary: "adb forward cannot listen on 127.0.0.1: " + installed.FailReason, Hint: loopbackHint}
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(installed.ResolvedPort))
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return Result{Status: Fail, Summary: fmt.Sprintf("cannot reach the port adb forward opened on 127.0.0.1: %v", err), Hint: loopbackHint}
	}
	conn.Close()
	return Result{Status: Pass, Summary: fmt.Sprintf("adb forward can listen on 127.0.0.1 (tried port %d)", installed.ResolvedPort)}
}
