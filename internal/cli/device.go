// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/aws/device-connect-devicefarm/internal/control"
	"github.com/aws/device-connect-devicefarm/internal/failure"
	"github.com/aws/device-connect-devicefarm/internal/report"
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
			return attachDevice(cmd, state, failure.Explained{Problem: "could not attach the device"})
		},
	}
	addSessionArnFlag(cmd, state, sessionArnDescription)
	return cmd
}

// newDeviceListCommand prints the attached devices on stdout: one table row per device, with
// --list one deviceListLine per device, with --json one object. One device attaches at a time;
// the per-device shapes hold when more are allowed.
func newDeviceListCommand(state *rootState) *cobra.Command {
	short := fmt.Sprintf("List the %s remote access devices attached to the bridge", productName)
	listView, jsonView := false, false
	cmd := &cobra.Command{
		Use:   "list",
		Short: short,
		Long: short + ".\n\nOne table row per device: the device name, the adb serial, how long it has been\n" +
			"attached, when its session started and how long until the session expires. The\n" +
			"table fits the terminal width, or COLUMNS when stdout is not a terminal. --list\n" +
			"prints one tab-separated line per device instead, the session ARN first; --json\n" +
			"prints one JSON object.\n\n" + bridgeAutoStartNote,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if listView && jsonView {
				return usageError(cmd, errors.New("--list and --json cannot be used together"))
			}
			resp, err := sendToBridge(cmd, state, control.Request{Op: control.OpList})
			if err != nil {
				return err
			}
			if !resp.OK {
				return &failure.Explained{Problem: "could not list the devices", Cause: daemonRefusal(resp)}
			}
			stdout, now := cmd.OutOrStdout(), time.Now()
			switch {
			case jsonView:
				return report.WriteJSON(stdout, deviceListJSON(resp))
			case len(resp.Devices) == 0:
				cmd.Println("no device attached; use 'device add' to connect one")
				if note := lastDetachNote(resp, now); note != "" {
					cmd.Println(note)
				}
				return nil
			case listView:
				for _, attached := range resp.Devices {
					if _, err := fmt.Fprintln(stdout, deviceListLine(attached, now)); err != nil {
						return err
					}
				}
				return nil
			default:
				_, err := io.WriteString(stdout, deviceTable(resp.Devices, now).Render(report.TerminalWidth(stdout)))
				return err
			}
		},
	}
	cmd.Flags().BoolVar(&listView, "list", false, "print one tab-separated line per device, the session ARN first, instead of the table")
	cmd.Flags().BoolVar(&jsonView, "json", false, "print one JSON object instead of the table")
	return cmd
}

// deviceTable is the default view: the device name takes the width the value columns leave.
// The Session cell is two paragraphs, the start age and the expiry.
func deviceTable(devices []control.AttachedDevice, now time.Time) report.Table {
	table := report.Table{Columns: []report.Column{
		{Title: "Device", Flexible: true}, {Title: "Serial"}, {Title: "Attached"}, {Title: "Session"},
	}}
	for _, attached := range devices {
		table.Rows = append(table.Rows, []report.Cell{
			{attached.Label},
			{cmp.Or(attached.Serial, "unknown")},
			{agoOrUnknown(attached.AttachedAt, now)},
			{"started " + agoOrUnknown(attached.SessionStartedAt, now), sessionExpiry(attached.SessionExpiresAt, now)},
		})
	}
	return table
}

// deviceListLine renders one attached device as unlabeled tab-separated values,
// ARN first. An empty serial or a zero time reads "unknown"; a passed expiry
// reads "expired".
func deviceListLine(attached control.AttachedDevice, now time.Time) string {
	return fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s",
		attached.SessionARN, attached.Label, cmp.Or(attached.Serial, "unknown"),
		agoOrUnknown(attached.AttachedAt, now),
		agoOrUnknown(attached.SessionStartedAt, now),
		expiryValue(attached.SessionExpiresAt, now))
}

// expiryValue is sessionExpiry as a bare value: "unknown", "expired" or "in <span>".
func expiryValue(expiresAt, now time.Time) string {
	switch {
	case expiresAt.IsZero():
		return "unknown"
	case !now.Before(expiresAt):
		return "expired"
	default:
		return "in " + humanSpan(expiresAt.Sub(now))
	}
}

// jsonDeviceList is the --json shape: the daemon's devices, always an array, and the last detach
// when the daemon reports one. A device is control.AttachedDevice, so its json tags are the public
// shape; the cli tests pin them.
type jsonDeviceList struct {
	Devices    []control.AttachedDevice `json:"devices"`
	LastDetach jsonLastDetach           `json:"lastDetach,omitzero"`
}

type jsonLastDetach struct {
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

func deviceListJSON(resp control.Response) jsonDeviceList {
	return jsonDeviceList{
		Devices:    report.NonNil(resp.Devices),
		LastDetach: jsonLastDetach{Reason: resp.LastDetachReason, At: resp.LastDetachAt},
	}
}

// agoOrUnknown words how long ago at was ("12 minutes ago"), or "unknown" for a zero time.
func agoOrUnknown(at, now time.Time) string {
	if at.IsZero() {
		return "unknown"
	}
	return humanSpan(now.Sub(at)) + " ago"
}

// sessionExpiry words the time left before the service ends the session, after "session ".
func sessionExpiry(expiresAt, now time.Time) string {
	switch {
	case expiresAt.IsZero():
		return "expiry unknown"
	case !now.Before(expiresAt):
		return "expired"
	default:
		return "expires in " + humanSpan(expiresAt.Sub(now))
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
		Long:  short + ".\n\nThis command needs a running bridge.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if blankSessionArn(cmd, state) {
				return usageError(cmd, errors.New("--session-arn is blank; omit it to detach the attached device"))
			}
			resp, err := state.send(control.Request{Op: control.OpRemove, ARN: state.sessionARN})
			if err != nil {
				return err
			}
			if !resp.OK {
				return &failure.Explained{Problem: "could not detach the device", Cause: daemonRefusal(resp)}
			}

			cmd.Printf("Removed %s from the bridge\n", resp.DeviceLabel)
			return nil
		},
	}
	addSessionArnFlag(cmd, state, sessionArnDescription+" (optional; without it, the attached device is detached)")
	return cmd
}
