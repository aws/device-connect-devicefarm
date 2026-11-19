//go:build unix

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/aws/device-connect-devicefarm/internal/control"
)

// newDeviceCommand groups the device subcommands under one parent: "device add"
// attaches a device to the running daemon and "device remove" detaches it. Both
// live here so the two halves of the device lifecycle sit together.
func newDeviceCommand(state *rootState) *cobra.Command {
	device := &cobra.Command{
		Use:   "device",
		Short: fmt.Sprintf("Attach or detach a %s remote access device", productName),
		Args:  cobra.NoArgs,
		// A RunE makes the parent runnable, so cobra validates Args (NoArgs) rather
		// than short-circuiting to help for a non-runnable command: `device` alone
		// prints help and exits 0, while `device <unknown>` is rejected with a
		// non-zero exit instead of silently printing help.
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	device.AddCommand(
		newDeviceAddCommand(state),
		newDeviceRemoveCommand(state),
	)
	return device
}

// newDeviceAddCommand attaches a device to an already-running daemon. It does not
// start the daemon: a "not listening" error from the control client surfaces when
// no daemon is running, matching the user-facing contract that "proxy start" and
// "device add" are distinct steps. The daemon resolves the session ARN to a
// websocket endpoint itself, so this command just forwards the ARN.
func newDeviceAddCommand(state *rootState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add",
		Short: fmt.Sprintf("Attach a %s remote access device to the running proxy", productName),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return attachToDaemon(cmd, state)
		},
	}
	addSessionArnFlag(cmd, state, sessionArnDescription)
	return cmd
}

// newDeviceRemoveCommand detaches the active device from the running daemon. The
// listener stays up; only the device binding is cleared. Resolving the ARN is not
// needed: the daemon matches the device by ARN, so the request carries just the
// ARN.
func newDeviceRemoveCommand(state *rootState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove",
		Short: fmt.Sprintf("Detach the active %s remote access device from the proxy", productName),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if state.sessionARN == "" {
				return fmt.Errorf("--session-arn is required")
			}

			resp, err := state.controlClient().Send(control.Request{
				Op:  control.OpRemove,
				ARN: state.sessionARN,
			})
			if err != nil {
				if daemonNotRunning(err) {
					return notListeningError()
				}
				return err
			}
			if !resp.OK {
				return fmt.Errorf("remove device: %s", resp.Error)
			}

			cmd.Printf("Removed %s from the proxy\n", resp.DeviceLabel)
			return nil
		},
	}
	addSessionArnFlag(cmd, state, sessionArnDescription)
	return cmd
}
