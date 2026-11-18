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
	"github.com/aws/device-connect-devicefarm/internal/version"
	"github.com/aws/device-connect-devicefarm/internal/wording"
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
	// listenPortTimeout covers one bind plus the two occupant round trips (identity, then version).
	listenPortTimeout = 5 * time.Second
	// occupantReplyTimeout bounds each exchange with whatever holds the port.
	occupantReplyTimeout = time.Second
	// maxControlSocketPath is the longest socket path every supported OS binds; macOS sun_path
	// is 104 bytes with the NUL, Linux 108.
	maxControlSocketPath = 103
)

// restartWith is the quoted restart on port with reverse as asked, then the add of the session
// the daemon reports, if any. The status reply names no listen address, so the command names none.
func restartWith(reply control.Response, port int, reverse bool) string {
	startCommand := daemon.StartCommand(config.DefaultListenAddr, port)
	if reverse {
		startCommand += wording.RestartReverseFlag
	}
	return daemon.RestartRemedy(startCommand, "", reply.ARN)
}

// caveatIf appends the reverse caveat when the quoted command carries --allow-adb-reverse.
func caveatIf(hint string, reverse bool) string {
	if reverse {
		return hint + " " + wording.DoctorHintReverseCaveat
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

// reverseOffHint restarts the daemon on its port with reverse on, adds its device again, then states the caveat.
func reverseOffHint(reply control.Response) string {
	return caveatIf(fmt.Sprintf(wording.DoctorHintDaemonReverseOffFormat, restartWith(reply, reply.Port, true)), true)
}

// listenConfigHint names the listen flags to fix. On the default address only --port can be at
// fault, so a user who never passed --listen-addr is not told it exists.
func listenConfigHint(listenAddr string) string {
	if listenAddr == config.DefaultListenAddr {
		return wording.DoctorHintPortInvalid
	}
	return wording.DoctorHintListenFlagsInvalid
}

// adbServerHint: bridge start stops the adb server on the probed address before it starts the daemon;
// a program that takes the port back gets AdbServerKeepsPortFix.
func adbServerHint(listenAddr string, port int) string {
	return fmt.Sprintf(wording.DoctorHintAdbServerOnPortFormat,
		net.JoinHostPort(listenAddr, strconv.Itoa(port)), daemon.StartCommand(listenAddr, port),
		daemon.AdbServerKeepsPortFix(listenAddr, port, daemon.LeadQuotesStart))
}

// lowPortHint: the bind was refused for want of permission.
func lowPortHint(port int) string {
	return fmt.Sprintf(wording.DoctorHintPortNeedsAdminFormat, port, port)
}

// reservedPortHint: Windows refused the bind with access denied: an excluded port range, or a
// wildcard exclusive holder that did not answer the probe.
func reservedPortHint(port int) string {
	return fmt.Sprintf(wording.DoctorHintPortReservedFormat, port, port)
}

// listenAddrHint: the listen address is not one of this machine's.
func listenAddrHint(address string, port int) string {
	return fmt.Sprintf(wording.DoctorHintAddressNotLocalFormat, safeValue(address), port)
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
	return fmt.Sprintf(wording.DoctorDetailPortHeldByProgramFormat, net.JoinHostPort(listenAddr, strconv.Itoa(port))),
		daemon.HeldPortText(listenAddr, port)
}

// otherPortDaemonHint moves the running daemon to the probed port, its reverse flag kept and its
// device added again, or points adb at the port it holds.
func otherPortDaemonHint(reply control.Response, port int) string {
	return caveatIf(fmt.Sprintf(wording.DoctorHintDaemonOnOtherPortFormat,
		reply.Port, port, port, restartWith(reply, port, reply.ReverseEnabled), reply.Port), reply.ReverseEnabled)
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
		{ID: checkRuntimeDir, Title: wording.DoctorTitleWorkingDirectory, Category: CategoryLocal, Timeout: localTimeout,
			Run: func(context.Context) Result { return checkRuntimeDirectory(state) }},
		{ID: checkDaemon, Title: wording.DoctorTitleBridge, Category: CategoryLocal, Needs: []string{checkRuntimeDir}, Timeout: daemonTimeout,
			Run: func(ctx context.Context) Result { return checkRunningDaemon(ctx, inputs, state) }},
		{ID: checkListenPort, Title: wording.DoctorTitleListenPort, Category: CategoryLocal, Needs: []string{checkDaemon}, Timeout: listenPortTimeout,
			Run: func(context.Context) Result { return checkListenPortFree(inputs, state) }},
		{ID: checkForwardTable, Title: wording.DoctorTitleAdbForward, Category: CategoryLocal, Timeout: localTimeout,
			Run: func(ctx context.Context) Result { return checkForwardTableBinds(ctx, inputs) }},
	}
}

// checkRuntimeDirectory resolves the paths (this creates the dir, as every command does),
// checks the socket path length, then writes and removes one probe file. Rows name the directory,
// never its files: the user controls only the directory.
func checkRuntimeDirectory(state *runState) Result {
	// The whole summary is escaped: an OS error repeats the path, newline and all.
	fail := func(summary string) Result {
		return Result{Status: Fail, Summary: safeValue(summary), Hint: fmt.Sprintf(wording.DoctorHintDirectoryUnusableFormat, daemon.RuntimeDirEnvVar)}
	}
	paths, err := daemon.DefaultPaths()
	if err != nil {
		return fail(wording.DoctorDetailDirectoryUnusableLead + err.Error())
	}
	switch over := len(paths.ControlSocket) - maxControlSocketPath; {
	case over == 1:
		return fail(fmt.Sprintf(wording.DoctorDetailDirectoryOneByteTooLongFormat, paths.RuntimeDir))
	case over > 1:
		return fail(fmt.Sprintf(wording.DoctorDetailDirectoryTooLongFormat, paths.RuntimeDir, over))
	}
	probe, err := os.CreateTemp(paths.RuntimeDir, ".doctor-")
	if err != nil {
		return fail(fmt.Sprintf(wording.DoctorDetailDirectoryNotWritableFormat, paths.RuntimeDir, err))
	}
	// Bytes, not an empty file: a pidfile write needs space, and a full disk still hands out inodes.
	_, writeErr := probe.WriteString("probe")
	if err := cmp.Or(writeErr, probe.Close(), os.Remove(probe.Name())); err != nil {
		return fail(fmt.Sprintf(wording.DoctorDetailDirectoryNotWritableFormat, paths.RuntimeDir, err))
	}
	state.paths = paths
	return Result{Status: Pass, Summary: fmt.Sprintf(wording.DoctorDetailDirectoryWritableFormat, safeValue(paths.RuntimeDir))}
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
		return Result{Status: Pass, Summary: wording.DoctorDetailDaemonNotRunning}
	}
	reply, err := control.NewDaemonClient(state.paths.ControlSocket).Send(control.Request{Op: control.OpStatus})
	askedARN := strings.TrimSpace(inputs.SessionARN)
	switch {
	case errors.Is(err, control.ErrDaemonNotRunning):
		return Result{Status: Fail, Summary: fmt.Sprintf(wording.DoctorDetailDaemonSilentFormat, pid, causeWithoutSocketPath(err)),
			Hint: noStatusHint(askedARN)}
	case err != nil:
		return Result{Status: Fail, Summary: fmt.Sprintf(wording.DoctorDetailDaemonNoStatusFormat, pid, causeWithoutSocketPath(err)),
			Hint: noStatusHint(askedARN)}
	case !reply.OK:
		return Result{Status: Fail, Summary: fmt.Sprintf(wording.DoctorDetailDaemonStatusRefusedFormat, pid, reply.Error),
			Hint: noStatusHint(askedARN)}
	}
	state.setDaemon(reply, pid)
	returnedARN := lookUpReturnedARN(ctx, inputs, state, askedARN, reply)
	return bridgeRowResult(askedARN, returnedARN, reply, pid)
}

// noStatusHint words the restart of a bridge that reports no status, then the add of the asked
// session, if any. The doctor cannot read that bridge's flags or device.
func noStatusHint(askedARN string) string {
	return fmt.Sprintf(wording.DoctorHintDaemonNoStatusFormat,
		daemon.RestartRemedy(wording.BridgeStartCommand, wording.YourOptionsClause, safeValue(askedARN)))
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

// programVersionClause is the summary clause for a bridge that runs another version of this
// program than the command, "" for the same one. A bridge that reports none is an earlier one.
func programVersionClause(bridgeProgramVersion string) string {
	switch bridgeProgramVersion {
	case version.String():
		return ""
	case "":
		return fmt.Sprintf(wording.DoctorDetailDaemonEarlierProgramVersionClauseFormat, version.String())
	}
	return fmt.Sprintf(wording.DoctorDetailDaemonOtherProgramVersionClauseFormat, safeValue(bridgeProgramVersion), version.String())
}

// otherProgramVersionHint restarts the bridge with its own port and reverse setting and adds its
// device again, so it runs this command's version.
func otherProgramVersionHint(reply control.Response) string {
	return caveatIf(fmt.Sprintf(wording.DoctorHintDaemonOtherProgramVersionFormat,
		restartWith(reply, reply.Port, reply.ReverseEnabled)), reply.ReverseEnabled)
}

// bridgeRowResult builds the row for a bridge that answered status. WARN on the first match:
// another version of this program than the command (such a bridge can lack the fields the later
// matches read), a detached device (unless the reason is about another session than the asked
// one), the asked session not attached, reverse off. Never FAIL: the bridge runs. askedARN and
// returnedARN (see lookUpReturnedARN) both name the asked session.
func bridgeRowResult(askedARN, returnedARN string, reply control.Response, pid int) Result {
	device := wording.DoctorDetailDaemonNoDeviceClause
	if reply.DeviceLabel != "" {
		device = fmt.Sprintf(wording.DoctorDetailDaemonDeviceClauseFormat, safeValue(reply.DeviceLabel), safeValue(reply.ARN))
	}
	reverse := wording.DoctorDetailDaemonReverseOnClause
	if !reply.ReverseEnabled {
		reverse = wording.DoctorDetailDaemonReverseOffClause
	}
	otherProgramVersion := programVersionClause(reply.ProgramVersion)
	summary := fmt.Sprintf(wording.DoctorDetailDaemonRunningFormat, pid, reply.Port, device, reverse) + otherProgramVersion
	// The reply carries a detach reason only while no device is attached.
	if reply.LastDetachReason != "" {
		summary += wording.DoctorDetailDaemonDetachedClause
	}
	askedMissing := askedARN != "" && !attachedIsAsked(reply.ARN, askedARN, returnedARN)
	if askedMissing {
		summary += fmt.Sprintf(wording.DoctorDetailDaemonNotAttachedClauseFormat, safeValue(askedARN))
	}
	// Every detach reason names its session.
	detachAnswersAsk := askedARN == "" || strings.Contains(reply.LastDetachReason, askedARN) ||
		returnedARN != "" && strings.Contains(reply.LastDetachReason, returnedARN)
	switch {
	case otherProgramVersion != "":
		return Result{Status: Warn, Summary: summary, Hint: otherProgramVersionHint(reply)}
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
	return fmt.Sprintf(wording.DoctorHintDaemonDetachedFormat, safeValue(reason))
}

// notAttachedHint names the add command for the asked session. The bridge holds one device, so
// the hint puts 'device remove' first when a device is attached.
func notAttachedHint(reply control.Response, askedARN string) string {
	arn := safeValue(askedARN)
	add := fmt.Sprintf(wording.DoctorDeviceAddCommandFormat, arn)
	if reply.DeviceLabel == "" {
		return fmt.Sprintf(wording.DoctorHintSessionNotAttachedFormat, arn, add)
	}
	return fmt.Sprintf(wording.DoctorHintSessionNotAttachedRemoveFirstFormat, arn, add, safeValue(reply.DeviceLabel))
}

// checkListenPortFree binds the listen address once and frees it; a running daemon on the port
// passes with no bind. A bind refused for want of permission fails at once. When a local socket
// holds the port (daemon.HolderMayAnswer), the bridge identity request, then one host:version,
// tell another bridge, an adb server and any other occupant apart; otherwise, or with no answer,
// the bind error itself picks the hint. Another bridge is a FAIL: no command here stops it. An
// adb server is a WARN: bridge start stops it, the doctor never does.
func checkListenPortFree(inputs Inputs, state *runState) Result {
	cfg := inputs.Config
	if err := cfg.Validate(); err != nil {
		return Result{Status: Fail, Summary: err.Error(), Hint: listenConfigHint(cfg.ListenAddr)}
	}
	address := net.JoinHostPort(cfg.ListenAddr, strconv.Itoa(cfg.ListenPort))
	if daemonReply, daemonPID := state.daemon(); daemonReply.OK {
		if daemonReply.Port == cfg.ListenPort {
			return Result{Status: Pass, Summary: fmt.Sprintf(wording.DoctorDetailPortHeldByDaemonFormat, address, daemonPID)}
		}
		// A daemon on another port is the finding; no bind.
		return Result{Status: Warn, Summary: fmt.Sprintf(wording.DoctorDetailDaemonOnOtherPortFormat,
			daemonPID, daemonReply.Port, cfg.ListenPort), Hint: otherPortDaemonHint(daemonReply, cfg.ListenPort)}
	}
	listener, err := net.Listen("tcp", address)
	if err == nil {
		listener.Close()
		return Result{Status: Pass, Summary: address + wording.DoctorDetailPortFreeTail}
	}
	if errors.Is(err, os.ErrPermission) {
		return Result{Status: Fail, Summary: safeValue(err.Error()), Hint: lowPortHint(cfg.ListenPort)}
	}
	summary, hint := bindErrorHint(err, runtime.GOOS, cfg.ListenAddr, cfg.ListenPort)
	if daemon.HolderMayAnswer(err, runtime.GOOS) {
		occupantAddress := daemon.OccupantAddress(cfg.ListenAddr, cfg.ListenPort)
		pid, isBridge, identityRefused := daemon.ProbeBridge(occupantAddress, occupantReplyTimeout)
		if isBridge {
			return Result{Status: Fail,
				Summary: fmt.Sprintf(wording.DoctorDetailOtherBridgeOnPortFormat, pid, safeValue(address)),
				Hint:    daemon.BridgeHoldsPortText(safeValue(cfg.ListenAddr), cfg.ListenPort, pid)}
		}
		adbServerVersion, connected, adbServer := daemon.ProbeOccupant(occupantAddress, occupantReplyTimeout)
		if adbServer && identityRefused {
			return Result{Status: Warn, Summary: fmt.Sprintf(wording.DoctorDetailAdbServerOnPortFormat, adbServerVersion, safeValue(address)),
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

func (closingOpener) OpenStream(conn net.Conn, _ adbwire.AdbSpec) { conn.Close() }

// checkForwardTableBinds installs one tcp:0 rule, dials its listener and closes the table. No IPv6
// row: forward binds 127.0.0.1 only; the reverse connector tries ::1 only after a refused IPv4 connect.
func checkForwardTableBinds(ctx context.Context, inputs Inputs) Result {
	table := forward.NewForwardRulesTable(closingOpener{}, 1, inputs.Logger)
	defer table.Close()
	installed := table.Install("tcp:0", "tcp:0", false)
	if installed.Status != forward.AdbInstallOK {
		return Result{Status: Fail, Summary: wording.DoctorDetailForwardCannotListenLead + installed.FailReason, Hint: wording.DoctorHintForwardBlocked}
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(installed.ResolvedPort))
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return Result{Status: Fail, Summary: fmt.Sprintf(wording.DoctorDetailForwardUnreachableFormat, err), Hint: wording.DoctorHintForwardBlocked}
	}
	conn.Close()
	return Result{Status: Pass, Summary: fmt.Sprintf(wording.DoctorDetailForwardListensFormat, installed.ResolvedPort)}
}
