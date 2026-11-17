// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"cmp"
	"errors"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/aws/device-connect-devicefarm/internal/control"
	"github.com/aws/device-connect-devicefarm/internal/failure"
	"github.com/aws/device-connect-devicefarm/internal/report"
	"github.com/aws/device-connect-devicefarm/internal/wording"
)

// newDeviceCommand groups the device subcommands under one parent: "device add"
// attaches a device to the bridge daemon, "device list" lists the attached devices and
// "device remove" detaches one. Add and list start the daemon when none runs; remove
// needs a running one.
func newDeviceCommand(state *rootState) *cobra.Command {
	device := &cobra.Command{
		Use:   "device",
		Short: wording.HelpDevice,
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
	cmd := &cobra.Command{
		Use:   "add",
		Short: wording.HelpDeviceAdd,
		Long:  wording.HelpDeviceAddLong,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return attachDevice(cmd, state)
		},
	}
	addSessionArnFlag(cmd, state, wording.FlagSessionARNUsage)
	return cmd
}

// newDeviceListCommand prints the attached devices on stdout: one table row per device, with
// --json one object. One device attaches at a time; the per-device shapes hold when more are
// allowed.
func newDeviceListCommand(state *rootState) *cobra.Command {
	jsonView := false
	cmd := &cobra.Command{
		Use:   "list",
		Short: wording.HelpDeviceList,
		Long:  wording.HelpDeviceListLong,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			resp, err := sendToBridge(cmd, state, control.Request{Op: control.OpList})
			if err != nil {
				return err
			}
			if !resp.OK {
				return &failure.Explained{Problem: wording.ProblemListFailed, Cause: daemonRefusal(resp)}
			}
			stdout, now := cmd.OutOrStdout(), time.Now()
			switch {
			case jsonView:
				return report.WriteJSON(stdout, deviceListJSON(resp))
			case len(resp.Devices) == 0:
				cmd.Println(wording.OutputNoDeviceAttached)
				if note := lastDetachNote(resp, now); note != "" {
					cmd.Println(note)
				}
				return nil
			default:
				_, err := io.WriteString(stdout, deviceTable(resp.Devices, now).Render(report.TerminalWidth(stdout)))
				return err
			}
		},
	}
	cmd.Flags().BoolVar(&jsonView, "json", false, wording.FlagDeviceListJSONUsage)
	return cmd
}

// deviceTable is the default view: the device name takes the width the value columns leave.
// The Session cell is two paragraphs, the start age and the expiry.
func deviceTable(devices []control.AttachedDevice, now time.Time) report.Table {
	table := report.Table{Columns: []report.Column{
		{Title: wording.OutputColumnDevice, Flexible: true}, {Title: wording.OutputColumnSerial},
		{Title: wording.OutputColumnAttached}, {Title: wording.OutputColumnSession},
	}}
	for _, attached := range devices {
		table.Rows = append(table.Rows, []report.Cell{
			{attached.Label},
			{cmp.Or(attached.Serial, wording.OutputUnknownSerial)},
			{agoOrUnknown(attached.AttachedAt, now)},
			{wording.OutputStartedLead + agoOrUnknown(attached.SessionStartedAt, now), sessionExpiry(attached.SessionExpiresAt, now)},
		})
	}
	return table
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
		return wording.OutputUnknownTime
	}
	return humanSpan(now.Sub(at)) + wording.OutputAgoTail
}

// sessionExpiry words the time left before the service ends the session, after "session ".
func sessionExpiry(expiresAt, now time.Time) string {
	switch {
	case expiresAt.IsZero():
		return wording.OutputExpiryUnknown
	case !now.Before(expiresAt):
		return wording.OutputExpired
	default:
		return wording.OutputExpiresInLead + humanSpan(expiresAt.Sub(now))
	}
}

// newDeviceRemoveCommand detaches the attached device; the listener stays up. With
// --session-arn the daemon detaches only when the ARN matches; without it, whichever
// device is attached (one at a time today). It needs a running daemon and reports not
// listening otherwise.
func newDeviceRemoveCommand(state *rootState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove",
		Short: wording.HelpDeviceRemove,
		Long:  wording.HelpDeviceRemoveLong,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if blankSessionArn(cmd, state) {
				return usageError(cmd, errors.New(wording.ProblemSessionARNBlankRemove))
			}
			resp, err := state.send(control.Request{Op: control.OpRemove, ARN: state.sessionARN})
			if err != nil {
				return err
			}
			if !resp.OK {
				return &failure.Explained{Problem: wording.ProblemDetachFailed, Cause: daemonRefusal(resp)}
			}

			cmd.Printf(wording.OutputRemovedFormat+"\n", resp.DeviceLabel)
			return nil
		},
	}
	addSessionArnFlag(cmd, state, wording.FlagSessionARNRemoveUsage)
	return cmd
}
