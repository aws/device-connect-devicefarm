package cli

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/control"
	"github.com/aws/device-connect-devicefarm/internal/daemon"
)

// awsName is the cloud provider prefix, kept in one place so every user-facing
// message names it identically.
const awsName = "AWS"

// productName is the AWS product this tool fronts, kept in one place so every
// user-facing message names it identically.
const productName = "Device Farm"

// notListeningMessage is what 'bridge stop' prints when no daemon runs, and the error
// 'device remove' and a live pidfile with a dead control socket report.
const notListeningMessage = config.BinaryName + " not listening"

// bridgeAutoStartNote is the help paragraph of every command that starts the bridge
// daemon itself.
const bridgeAutoStartNote = "If the bridge daemon is not running, this command starts it with default\n" +
	"settings, as 'adb' starts its own server. To pick a port or other options,\n" +
	"run 'bridge start' first."

// sessionArnDescription is the shared --session-arn help text; bridge start
// appends its own suffix.
var sessionArnDescription = fmt.Sprintf("%s %s remote access session ARN", awsName, productName)

// addSessionArnFlag binds the --session-arn flag to the shared rootState on cmd.
// The three commands that take a session ARN (bridge start, device add, device
// remove) register it identically apart from the help text, so it lives here.
func addSessionArnFlag(cmd *cobra.Command, state *rootState, help string) {
	cmd.Flags().StringVar(&state.sessionARN, "session-arn", "", help)
}

// startBridge starts the bridge daemon. A package var so a test can swap in a fake
// that does not fork.
var startBridge = func(manager *daemon.DaemonManager) error { return manager.Start() }

// startBridgeIfNotRunning starts the bridge daemon with the default settings when none
// runs, as adb starts its server on the first command.
func startBridgeIfNotRunning(cmd *cobra.Command, state *rootState) error {
	manager := state.newManager()
	if _, running := manager.Status(); running {
		return nil
	}
	cmd.PrintErrln("* bridge not running; starting it now")
	if err := startBridge(manager); err != nil {
		return fmt.Errorf("start the bridge: %w", err)
	}
	return nil
}

// sendToBridge starts the bridge daemon when none runs, then sends req to it. A live
// pidfile whose socket does not answer surfaces as the "not listening" error.
func sendToBridge(cmd *cobra.Command, state *rootState, req control.Request) (control.Response, error) {
	if err := startBridgeIfNotRunning(cmd, state); err != nil {
		return control.Response{}, err
	}
	resp, err := state.controlClient().Send(req)
	if err != nil {
		if daemonNotRunning(err) {
			return control.Response{}, errors.New(notListeningMessage)
		}
		return control.Response{}, err
	}
	return resp, nil
}

// attachToDaemon sends an add request for the session ARN to the bridge daemon, starting
// it when none runs, and prints the listening line. The daemon resolves the ARN itself;
// a resolution failure surfaces as the add error.
func attachToDaemon(cmd *cobra.Command, state *rootState) error {
	if state.sessionARN == "" {
		return fmt.Errorf("--session-arn is required")
	}

	resp, err := sendToBridge(cmd, state, control.Request{Op: control.OpAdd, ARN: state.sessionARN})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("attach device: %s", resp.Error)
	}
	printListeningLine(cmd, resp)
	return nil
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

// daemonNotRunning reports whether err means no daemon answered the control
// socket. The dial cause goes to the debug log; the caller prints the plain
// not-running line.
func daemonNotRunning(err error) bool {
	if !errors.Is(err, control.ErrDaemonNotRunning) {
		return false
	}
	slog.Debug("control dial failed", "error", err)
	return true
}
