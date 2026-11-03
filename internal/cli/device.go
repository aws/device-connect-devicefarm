package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/aws/device-connect-devicefarm/internal/control"
)

// newDeviceCommand groups the device subcommands under one parent: "device add"
// attaches a device to the bridge daemon and "device remove" detaches it. Only add
// starts the daemon when none runs.
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

// newDeviceAddCommand attaches a device to the bridge daemon, starting it with the
// default settings when none runs. The daemon resolves the session ARN to a websocket
// endpoint itself, so this command just forwards the ARN.
func newDeviceAddCommand(state *rootState) *cobra.Command {
	short := fmt.Sprintf("Attach a %s remote access device to the bridge", productName)
	cmd := &cobra.Command{
		Use:   "add",
		Short: short,
		Long:  short + ".\n\n" + bridgeAutoStartNote,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return attachToDaemon(cmd, state)
		},
	}
	addSessionArnFlag(cmd, state, sessionArnDescription)
	return cmd
}

// newDeviceRemoveCommand detaches the active device. The listener stays up; only the
// device binding is cleared. The daemon matches the device by ARN, so the request
// carries just the ARN. It needs a running daemon and reports not listening otherwise.
func newDeviceRemoveCommand(state *rootState) *cobra.Command {
	short := fmt.Sprintf("Detach the active %s remote access device from the bridge", productName)
	cmd := &cobra.Command{
		Use:   "remove",
		Short: short,
		Long:  short + ".\n\nThis command needs a running bridge daemon.",
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
					return errors.New(notListeningMessage)
				}
				return err
			}
			if !resp.OK {
				return fmt.Errorf("remove device: %s", resp.Error)
			}

			cmd.Printf("Removed %s from the bridge\n", resp.DeviceLabel)
			return nil
		},
	}
	addSessionArnFlag(cmd, state, sessionArnDescription)
	return cmd
}
