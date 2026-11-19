//go:build unix

package cli

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/control"
)

// awsName is the cloud provider prefix, kept in one place so every user-facing
// message names it identically.
const awsName = "AWS"

// productName is the AWS product this tool fronts, kept in one place so every
// user-facing message names it identically.
const productName = "Device Farm"

// notListeningMessage is shown when a command needs a running daemon but none is
// listening.
const notListeningMessage = config.BinaryName + " not listening"

// sessionArnDescription is the shared --session-arn help text; proxy start
// appends its own suffix.
var sessionArnDescription = fmt.Sprintf("%s %s remote access session ARN", awsName, productName)

// addSessionArnFlag binds the --session-arn flag to the shared rootState on cmd.
// The three commands that take a session ARN (proxy start, device add, device
// remove) register it identically apart from the help text, so it lives here.
func addSessionArnFlag(cmd *cobra.Command, state *rootState, help string) {
	cmd.Flags().StringVar(&state.sessionARN, "session-arn", "", help)
}

// attachToDaemon sends an add request carrying the session ARN to the running
// daemon, which resolves the ARN to a websocket endpoint itself, and prints the
// listening line. A not-running daemon surfaces as the "not listening" message,
// and a resolution failure in the daemon surfaces as the add error.
func attachToDaemon(cmd *cobra.Command, state *rootState) error {
	if state.sessionARN == "" {
		return fmt.Errorf("--session-arn is required")
	}

	resp, err := state.controlClient().Send(control.Request{
		Op:  control.OpAdd,
		ARN: state.sessionARN,
	})
	if err != nil {
		if daemonNotRunning(err) {
			return notListeningError()
		}
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

// notListeningError is returned by commands that require a running daemon when
// none is listening, so the CLI exits non-zero with a clear message.
func notListeningError() error {
	return errors.New(notListeningMessage)
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
