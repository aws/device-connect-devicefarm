// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/control"
	"github.com/aws/device-connect-devicefarm/internal/daemon"
	"github.com/aws/device-connect-devicefarm/internal/failure"
)

// awsName is the cloud provider prefix, kept in one place so every user-facing
// message names it identically.
const awsName = "AWS"

// productName is the AWS product this tool fronts, kept in one place so every
// user-facing message names it identically.
const productName = "Device Farm"

// notListeningMessage is what 'bridge stop' prints when no daemon runs.
const notListeningMessage = config.BinaryName + " not listening"

// errNotListening: no daemon answers the control socket, because none runs or
// one holds its pidfile over a dead socket.
var errNotListening = &failure.Explained{Problem: "the bridge is not listening"}

// bridgeAutoStartNote is the help paragraph of every command that starts the bridge
// daemon itself.
const bridgeAutoStartNote = "If the bridge is not running, this command starts it with default\n" +
	"settings, as 'adb' starts its own server. To pick a port or other options,\n" +
	"run 'bridge start' first."

// sessionArnDescription is the shared --session-arn help text; bridge start and
// device remove append their own suffixes.
var sessionArnDescription = fmt.Sprintf("%s %s remote access session ARN", awsName, productName)

// addSessionArnFlag binds the --session-arn flag to the shared rootState on cmd.
// Every command that takes a session ARN registers it identically apart from the
// help text, so it lives here. It trims the value, so a pasted ARN with stray
// spaces still matches.
func addSessionArnFlag(cmd *cobra.Command, state *rootState, help string) {
	cmd.Flags().Var(trimmedString{target: &state.sessionARN}, "session-arn", help)
}

// trimmedString is a pflag.Value that stores the flag's value with surrounding
// spaces removed.
type trimmedString struct{ target *string }

func (value trimmedString) String() string {
	if value.target == nil {
		return ""
	}
	return *value.target
}

func (value trimmedString) Set(raw string) error {
	*value.target = strings.TrimSpace(raw)
	return nil
}

func (trimmedString) Type() string { return "string" }

// blankSessionArn reports a --session-arn that was given but trimmed to empty.
// `device remove` without the flag detaches the attached device, so a blank
// value must not read as "no flag".
func blankSessionArn(cmd *cobra.Command, state *rootState) bool {
	return cmd.Flags().Changed("session-arn") && state.sessionARN == ""
}

// lastDetachNote words the daemon's record of its last detach for a no-device reply, or "" when
// the reply carries none: "the last device detached <age> ago: <reason>".
func lastDetachNote(response control.Response, now time.Time) string {
	if response.LastDetachReason == "" {
		return ""
	}
	return fmt.Sprintf("the last device detached %s ago: %s",
		humanAge(now.Sub(response.LastDetachAt)), response.LastDetachReason)
}

// humanAge renders an age in its largest whole unit up to hours: "42 seconds", "1 minute".
func humanAge(age time.Duration) string {
	age = max(age, 0)
	count, unit := int(age.Seconds()), "second"
	switch {
	case age >= time.Hour:
		count, unit = int(age.Hours()), "hour"
	case age >= time.Minute:
		count, unit = int(age.Minutes()), "minute"
	}
	return countNoun(count, unit)
}

// countNoun formats a count with its noun: "1 device", "2 devices".
func countNoun(count int, noun string) string {
	if count != 1 {
		noun += "s"
	}
	return fmt.Sprintf("%d %s", count, noun)
}

// humanSpan is humanAge plus the whole minutes past an hour: "2 hours 5 minutes", "1 hour".
func humanSpan(span time.Duration) string {
	minutesPastTheHour := (max(span, 0) % time.Hour) / time.Minute
	if span < time.Hour || minutesPastTheHour == 0 {
		return humanAge(span)
	}
	return humanAge(span) + " " + humanAge(minutesPastTheHour*time.Minute)
}

// startBridge starts the bridge daemon. A package var so a test can swap in a fake
// that does not fork.
var startBridge = func(manager *daemon.DaemonManager) (daemon.StartResult, error) { return manager.Start() }

// stopBridge stops the bridge daemon. A package var beside startBridge: the cli
// tests fake a running daemon with this process's own pid, so a real Stop there
// would kill the test binary.
var stopBridge = func(manager *daemon.DaemonManager) (daemon.StopResult, error) { return manager.Stop() }

// startBridgeIfNotRunning starts the bridge daemon with the default settings when none
// runs, as adb starts its server on the first command.
func startBridgeIfNotRunning(cmd *cobra.Command, state *rootState) error {
	manager := state.newManager()
	if _, running := manager.Status(); running {
		return nil
	}
	cmd.PrintErrln("* bridge not running; starting it now")
	return startAndReport(cmd, manager)
}

// restartBridge stops any running bridge in this runtime directory, then starts one
// with the given flags. It restarts even when the flags match. "device add" and
// "device list" keep a running bridge (startBridgeIfNotRunning).
func restartBridge(cmd *cobra.Command, state *rootState) error {
	manager := state.newManager()
	if _, running := manager.Status(); !running {
		cmd.PrintErrln("* bridge not running; starting it now")
		return startAndReport(cmd, manager)
	}
	result, err := stopBridge(manager)
	if err != nil {
		// A fix on the stop error (the elevated-shell one) rides the chain.
		return &failure.Explained{Problem: "could not stop the running bridge", Cause: err}
	}
	if result.WasRunning {
		line := fmt.Sprintf("* a bridge was running (pid %d) and was stopped", result.PID)
		if result.Killed {
			line = fmt.Sprintf("* a bridge was running (pid %d), did not stop cleanly and was killed", result.PID)
		}
		cmd.PrintErrln(line + "; its attached devices, forward rules and reverse rules were dropped")
	}
	return startAndReport(cmd, manager)
}

// startAndReport starts the daemon and prints the result. It prints the adb-server
// notice even when the start fails.
func startAndReport(cmd *cobra.Command, manager *daemon.DaemonManager) error {
	result, err := startBridge(manager)
	printAdbServerStopped(cmd, result.StoppedAdbServer)
	if err != nil {
		return &failure.Explained{Problem: "could not start the bridge", Cause: err}
	}
	printStartResult(cmd, result)
	return nil
}

// printAdbServerStopped tells the user that Start stopped their adb server.
func printAdbServerStopped(cmd *cobra.Command, address string) {
	if address != "" {
		cmd.PrintErrln("* stopped the adb server on " + address + ", as 'adb kill-server' does")
	}
}

// printStartResult prints a new daemon's pid and log file. On success no other
// line names the log file.
func printStartResult(cmd *cobra.Command, result daemon.StartResult) {
	if !result.AlreadyRunning {
		cmd.PrintErrf("* bridge started (pid %d); log file: %s\n", result.PID, result.LogFile)
	}
}

// send sends req to the daemon. A dial failure returns errNotListening and logs
// its cause at debug; any other failure says the daemon did not answer.
func (s *rootState) send(req control.Request) (control.Response, error) {
	resp, err := s.controlClient().Send(req)
	if err != nil {
		if errors.Is(err, control.ErrDaemonNotRunning) {
			slog.Debug("control dial failed", "error", err)
			return control.Response{}, errNotListening
		}
		return control.Response{}, &failure.Explained{Problem: "the bridge did not answer", Cause: err}
	}
	return resp, nil
}

// sendToBridge starts the bridge daemon when none runs, then sends req to it.
func sendToBridge(cmd *cobra.Command, state *rootState, req control.Request) (control.Response, error) {
	if err := startBridgeIfNotRunning(cmd, state); err != nil {
		return control.Response{}, err
	}
	return state.send(req)
}

// attachDevice sends the add for --session-arn, starting the daemon when none
// runs, and prints the listening line. The daemon resolves the ARN; its refusal
// becomes the cause under refused, the command's own problem and fix.
func attachDevice(cmd *cobra.Command, state *rootState, refused failure.Explained) error {
	if state.sessionARN == "" {
		return usageError(cmd, errors.New("--session-arn is required"))
	}
	resp, err := sendToBridge(cmd, state, control.Request{Op: control.OpAdd, ARN: state.sessionARN})
	if err != nil {
		return err
	}
	if !resp.OK {
		refused.Cause = daemonRefusal(resp)
		if len(resp.Fixes) > 0 {
			refused.Fix = "" // the daemon's fix is the specific one
		}
		return &refused
	}
	printListeningLine(cmd, resp)
	if resp.ARN != "" && resp.ARN != state.sessionARN {
		cmd.PrintErrln("* the service names this session " + resp.ARN + "; use that ARN with 'device remove'")
	}
	return nil
}

// daemonRefusal rebuilds the daemon's error from a refused reply: its text wrapped in each
// fix, so failure.Write prints them as fix lines.
func daemonRefusal(resp control.Response) error {
	refusal := errors.New(resp.Error)
	for _, fix := range resp.Fixes {
		refusal = failure.WithFix(refusal, fix)
	}
	return refusal
}

// printListeningLine prints the daemon's listen port and device label from a
// control response. Port comes from the daemon, not a flag: only the daemon knows
// the address it bound, and `device add` has no --port/--listen-addr. The label
// carries its own parentheses (e.g. "Google Pixel 9 (OS 16)"), so it follows a
// colon; wrapping it would read "...device (Google Pixel 9 (OS 16))".
func printListeningLine(cmd *cobra.Command, response control.Response) {
	if response.DeviceLabel == "" {
		cmd.Printf(
			"%s listening on port %d with no device attached; use 'device add' to connect one\n",
			config.BinaryName, response.Port,
		)
		return
	}
	cmd.Printf(
		"%s listening on port %d with connected device: %s\n",
		config.BinaryName, response.Port, response.DeviceLabel,
	)
}
