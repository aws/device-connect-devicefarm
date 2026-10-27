package doctor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/control"
	"github.com/aws/device-connect-devicefarm/internal/daemon"
)

// Check ids, shared so Needs can name them.
const (
	checkRuntimeDir = "local.runtime-dir"
	checkDaemon     = "local.daemon"
)

const (
	// localTimeout bounds a check that touches only this machine.
	localTimeout = 2 * time.Second
	// daemonTimeout fits the control client's dial plus request timeouts.
	daemonTimeout = 15 * time.Second
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
	runtimeDirHint = "the client cannot use this working directory, so '" + bridgeStartCommand + "' cannot launch the daemon. " +
		"Set 'XDG_RUNTIME_DIR' to a directory with a short path that only this user can write, then rerun '" + doctorCommand +
		"'. Every " + config.BinaryName + " command reads that variable, so 'bridge start' and the daemon use the new directory too."
	deadDaemonHint = "a daemon is registered but did not report its status, so '" + bridgeStartCommand + "' starts nothing. Run '" +
		restartDaemonHint + "'. The stop ends that daemon and the start launches a new one."
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
	return caveatIf(fmt.Sprintf("the daemon runs with adb reverse off, so every 'adb reverse' fails. Run '%s' to start it again "+
		"with reverse on.", restartWith(reply, reply.Port, true)), true)
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

// localChecks are the rows that touch only this machine, in run order.
func localChecks(inputs Inputs, state *runState) []Check {
	return []Check{
		{ID: checkRuntimeDir, Title: "working directory", Category: CategoryLocal, Timeout: localTimeout,
			Run: func(context.Context) Result { return checkRuntimeDirectory(state) }},
		{ID: checkDaemon, Title: "bridge daemon", Category: CategoryLocal, Needs: []string{checkRuntimeDir}, Timeout: daemonTimeout,
			Run: func(context.Context) Result { return checkRunningDaemon(inputs, state) }},
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
// pidfile with a dead socket is a FAIL: bridge start sees the pidfile and no-ops.
func checkRunningDaemon(inputs Inputs, state *runState) Result {
	pid, running := daemon.NewManager(inputs.Config, state.paths, nil, inputs.Logger).Status()
	if !running {
		return Result{Status: Pass, Summary: "not running; bridge start, device add or device list will launch it"}
	}
	reply, err := control.NewDaemonClient(state.paths.ControlSocket).Send(control.Request{Op: control.OpStatus})
	switch {
	case errors.Is(err, control.ErrDaemonNotRunning):
		return Result{Status: Fail, Summary: fmt.Sprintf("a daemon (pid %d) is registered but does not answer: %s", pid, causeWithoutSocketPath(err)),
			Hint: deadDaemonHint}
	case err != nil:
		return Result{Status: Fail, Summary: fmt.Sprintf("a daemon (pid %d) is registered but did not report its status: %s", pid, causeWithoutSocketPath(err)),
			Hint: deadDaemonHint}
	case !reply.OK:
		return Result{Status: Fail, Summary: fmt.Sprintf("a daemon (pid %d) is registered but refused to report its status: %s", pid, reply.Error),
			Hint: deadDaemonHint}
	}
	state.setDaemon(reply, pid)
	device := "no device"
	if reply.DeviceLabel != "" {
		device = fmt.Sprintf("device %s, session %s", reply.DeviceLabel, reply.ARN)
	}
	summary := fmt.Sprintf("running (pid %d) on port %d; %s", pid, reply.Port, device)
	if !reply.ReverseEnabled {
		return Result{Status: Warn, Summary: summary + "; reverse off", Hint: reverseOffHint(reply)}
	}
	return Result{Status: Pass, Summary: summary + "; reverse on"}
}
