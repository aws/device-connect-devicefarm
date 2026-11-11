// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/control"
	"github.com/aws/device-connect-devicefarm/internal/failure"
	"github.com/aws/device-connect-devicefarm/internal/report"
	"github.com/aws/device-connect-devicefarm/internal/session"
)

// newBridgeCommand groups the daemon-lifecycle subcommands under one parent:
// "bridge start" launches the background daemon, "bridge stop" stops it, and
// "bridge status" reports on it. Grouping them (rather than top-level
// start-bridge/stop-bridge/status commands) keeps the verb tied to the noun it acts
// on.
func newBridgeCommand(state *rootState) *cobra.Command {
	bridge := &cobra.Command{
		Use:   "bridge",
		Short: "Manage the background bridge",
		Args:  cobra.NoArgs,
		// A RunE makes the parent runnable, so cobra validates Args (NoArgs) rather
		// than short-circuiting to help for a non-runnable command: `bridge` alone
		// prints help and exits 0, while `bridge <unknown>` is rejected with a
		// non-zero exit instead of silently printing help.
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	bridge.AddCommand(
		newBridgeStartCommand(state),
		newBridgeStopCommand(state),
		newBridgeStatusCommand(state),
	)
	return bridge
}

// newBridgeStartCommand launches the background daemon and, when a session ARN is
// supplied, attaches that device. It first stops a running bridge in this runtime
// directory (restartBridge), so the flags and this shell's AWS settings always
// apply. It validates the configuration up front so a misconfigured
// invocation fails before a child is ever spawned. --session-arn is optional:
// with it, "bridge start" runs the attach logic immediately after the daemon is
// up (the daemon resolves the ARN); without it, this command attaches nothing
// and a later "device add" connects one.
//
// A malformed --session-arn is a usage error before anything stops or starts, so it
// never stops a working bridge. The daemon resolves the ARN, so a session that does
// not resolve fails only after the listener is up. By design the listener stays up
// with no device, as on the no-ARN path, and the error tells the user to fix the
// issue and run this command again.
func newBridgeStartCommand(state *rootState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: fmt.Sprintf("Start the bridge, optionally attaching a %s remote access device", productName),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := state.cfg.Validate(); err != nil {
				return usageError(cmd, err)
			}
			if blankSessionArn(cmd, state) {
				return usageError(cmd, errors.New("--session-arn is blank"))
			}
			if state.sessionARN != "" {
				if err := session.ValidateSessionARN(state.sessionARN); err != nil {
					return usageError(cmd, err)
				}
			}
			if err := restartBridge(cmd, state); err != nil {
				return err
			}

			if state.sessionARN != "" {
				return attachDevice(cmd, state, failure.Explained{
					Problem: "the bridge started but could not attach the device",
					Fix:     "fix the issue, then run this command again; 'bridge start' restarts the bridge, so it reads this shell's AWS settings",
				})
			}

			// The port comes from the daemon, not the flags: only the daemon knows
			// what it bound, and a racing start may have won.
			statusResponse, err := sendToBridge(cmd, state, control.Request{Op: control.OpStatus})
			if err != nil {
				return err
			}
			printListeningLine(cmd, statusResponse)
			return nil
		},
	}
	// The listener and logfile flags live here, not on the root command: `bridge
	// start` is the only command that configures the daemon. `device add` and `device
	// list` start one with the defaults; every other command talks to a running one, and
	// `doctor` takes the address flags to check what the daemon would bind.
	bindListenerFlags(cmd, &state.cfg)
	bindReverseFlag(cmd, &state.cfg)
	cmd.Flags().StringVar(&state.cfg.LogPath, "log", state.cfg.LogPath, "bridge logfile path (defaults to the per-user runtime directory)")
	addSessionArnFlag(cmd, state, sessionArnDescription+" (optional; attaches the device on start)")
	return cmd
}

// newBridgeStopCommand stops the running daemon. If none is running it reports that
// cleanly rather than failing, so "bridge stop" is idempotent.
func newBridgeStopCommand(state *rootState) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the running bridge",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := stopBridge(state.newManager())
			if err != nil {
				return &failure.Explained{Problem: "could not stop the bridge", Cause: err}
			}
			if !result.WasRunning {
				cmd.PrintErrln(notListeningMessage)
				return nil
			}
			if result.Killed {
				cmd.PrintErrf("* the bridge (pid %d) did not stop cleanly and was killed\n", result.PID)
			}
			cmd.PrintErrln(config.BinaryName + " stopped")
			return nil
		},
	}
}

// newBridgeStatusCommand reports whether the daemon is running and how many devices
// are attached; "device list" names them. The report goes to stdout, as lines or
// one --json object. It exits 0 in every bridge state (running, stopped, stale
// pidfile).
func newBridgeStatusCommand(state *rootState) *cobra.Command {
	jsonView := false
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report whether the bridge is running and how many devices are attached",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			stdout := cmd.OutOrStdout()
			pid, running := state.newManager().Status()
			var resp control.Response
			var err error
			if running {
				// The daemon is up; ask it over the control channel what is attached so
				// status reflects live state, not just liveness.
				resp, err = state.send(control.Request{Op: control.OpStatus})
				switch {
				case errors.Is(err, errNotListening):
					// A live pidfile over a dead socket reads as not running.
					running = false
				case err != nil:
					return err
				}
			}
			if jsonView {
				return report.WriteJSON(stdout, bridgeStatusJSON(running, pid, resp))
			}
			if !running {
				_, err = fmt.Fprintln(stdout, config.BinaryName+" bridge is not running")
				return err
			}
			if len(resp.Devices) == 0 {
				line := fmt.Sprintf("%s bridge is running (pid %d), no device attached", config.BinaryName, pid)
				if note := lastDetachNote(resp, time.Now()); note != "" {
					line += "; " + note
				}
				_, err = fmt.Fprintln(stdout, line)
				return err
			}
			_, err = fmt.Fprintf(stdout, "%s bridge is running (pid %d), %s attached; run 'device list' for details\n",
				config.BinaryName, pid, countNoun(len(resp.Devices), "device"))
			return err
		},
	}
	cmd.Flags().BoolVar(&jsonView, "json", false, "print one JSON object instead of the report line")
	return cmd
}

// jsonBridgeStatus is the status --json shape. The running-only members are
// pointers so a stopped bridge prints exactly {"running":false,"devices":[]},
// while reverseEnabled still appears when it is false on a running one.
type jsonBridgeStatus struct {
	Running        bool                     `json:"running"`
	PID            *int                     `json:"pid,omitempty"`
	Port           *int                     `json:"port,omitempty"`
	ReverseEnabled *bool                    `json:"reverseEnabled,omitempty"`
	Devices        []control.AttachedDevice `json:"devices"`
	LastDetach     jsonLastDetach           `json:"lastDetach,omitzero"`
}

func bridgeStatusJSON(running bool, pid int, resp control.Response) jsonBridgeStatus {
	status := jsonBridgeStatus{Running: running, Devices: report.NonNil(resp.Devices)}
	if running {
		status.PID, status.Port, status.ReverseEnabled = &pid, &resp.Port, &resp.ReverseEnabled
		status.LastDetach = jsonLastDetach{Reason: resp.LastDetachReason, At: resp.LastDetachAt}
	}
	return status
}
