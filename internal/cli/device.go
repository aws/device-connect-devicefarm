package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/aws/device-connect-devicefarm/internal/control"
)

// newDeviceCommand groups the device subcommands under one parent: "device add"
// attaches a device to the bridge daemon, "device list" lists the attached devices and
// "device remove" detaches one. Add and list start the daemon when none runs; remove
// needs a running one.
func newDeviceCommand(state *rootState) *cobra.Command {
	device := &cobra.Command{
		Use:   "device",
		Short: fmt.Sprintf("Attach, list or detach %s remote access devices", productName),
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
		newDeviceListCommand(state),
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

// newDeviceListCommand prints one line per attached device: the session ARN, a tab,
// the device label. Only one device can be attached today. The one-line-per-device
// output keeps its shape when more are allowed.
func newDeviceListCommand(state *rootState) *cobra.Command {
	short := fmt.Sprintf("List the %s remote access devices attached to the bridge", productName)
	return &cobra.Command{
		Use:   "list",
		Short: short,
		Long:  short + ".\n\nOne line per device: the session ARN, then the device name.\n\n" + bridgeAutoStartNote,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			resp, err := sendToBridge(cmd, state, control.Request{Op: control.OpList})
			if err != nil {
				return err
			}
			if !resp.OK {
				return fmt.Errorf("list devices: %s", resp.Error)
			}
			if len(resp.Devices) == 0 {
				cmd.Println("no device attached; use 'device add' to connect one")
				if note := lastDetachNote(resp, time.Now()); note != "" {
					cmd.Println(note)
				}
				return nil
			}
			for _, attached := range resp.Devices {
				cmd.Printf("%s\t%s\n", attached.SessionARN, attached.Label)
			}
			return nil
		},
	}
}

// newDeviceRemoveCommand detaches the attached device; the listener stays up. With
// --session-arn the daemon detaches only when the ARN matches; without it, whichever
// device is attached (one at a time today). It needs a running daemon and reports not
// listening otherwise.
func newDeviceRemoveCommand(state *rootState) *cobra.Command {
	short := fmt.Sprintf("Detach the attached %s remote access device from the bridge", productName)
	cmd := &cobra.Command{
		Use:   "remove",
		Short: short,
		Long:  short + ".\n\nThis command needs a running bridge daemon.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
	addSessionArnFlag(cmd, state, sessionArnDescription+" (optional; without it, the attached device is detached)")
	return cmd
}
