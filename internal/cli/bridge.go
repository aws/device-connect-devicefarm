// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/aws/device-connect-devicefarm/internal/control"
	"github.com/aws/device-connect-devicefarm/internal/failure"
	"github.com/aws/device-connect-devicefarm/internal/report"
	"github.com/aws/device-connect-devicefarm/internal/session"
	"github.com/aws/device-connect-devicefarm/internal/wording"
)

// newBridgeCommand groups the daemon-lifecycle subcommands under one parent:
// "bridge start" launches the background daemon, "bridge stop" stops it, and
// "bridge status" reports on it. Grouping them (rather than top-level
// start-bridge/stop-bridge/status commands) keeps the verb tied to the noun it acts
// on.
func newBridgeCommand(state *rootState) *cobra.Command {
	bridge := &cobra.Command{
		Use:   "bridge",
		Short: wording.HelpBridge,
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
// A malformed --session-arn is refused with the ARN fix before anything stops or starts, so it
// never stops a working bridge. The daemon resolves the ARN, so a session that does
// not resolve fails only after the listener is up. By design the listener stays up
// with no device, as on the no-ARN path, and the error carries the daemon's fix.
func newBridgeStartCommand(state *rootState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: wording.HelpBridgeStart,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := state.cfg.Validate(); err != nil {
				return usageError(cmd, err)
			}
			if blankSessionArn(cmd, state) {
				return usageError(cmd, errors.New(wording.ProblemSessionARNBlank))
			}
			if state.sessionARN != "" {
				if err := session.ValidateSessionARN(state.sessionARN); err != nil {
					return failure.WithFix(err, wording.FixPassRunningSessionARN)
				}
			}
			if err := restartBridge(cmd, state); err != nil {
				return err
			}

			if state.sessionARN != "" {
				return attachDevice(cmd, state, wording.ProblemStartedButNotAttached)
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
	cmd.Flags().StringVar(&state.cfg.LogPath, "log", state.cfg.LogPath, wording.FlagLogUsage)
	addSessionArnFlag(cmd, state, wording.FlagSessionARNStartUsage)
	return cmd
}

// newBridgeStopCommand stops the running daemon. If none is running it reports that
// cleanly rather than failing, so "bridge stop" is idempotent.
func newBridgeStopCommand(state *rootState) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: wording.HelpBridgeStop,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := stopBridge(state.newManager())
			if err != nil {
				return &failure.Explained{Problem: wording.ProblemStopFailed, Cause: err}
			}
			if !result.WasRunning {
				cmd.PrintErrln(wording.OutputNotListening)
				return nil
			}
			if result.Killed {
				cmd.PrintErrf(wording.OutputStopKilledFormat+"\n", result.PID)
			}
			cmd.PrintErrln(wording.OutputStopped)
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
		Short: wording.HelpBridgeStatus,
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
				_, err = fmt.Fprintln(stdout, wording.OutputDaemonNotRunning)
				return err
			}
			if len(resp.Devices) == 0 {
				line := fmt.Sprintf(wording.OutputDaemonRunningNoDeviceFormat, pid)
				if note := lastDetachNote(resp, time.Now()); note != "" {
					line += "; " + note
				}
				_, err = fmt.Fprintln(stdout, line)
				return err
			}
			_, err = fmt.Fprintf(stdout, wording.OutputDaemonRunningDevicesFormat+"\n",
				pid, countNoun(len(resp.Devices), wording.OutputDeviceNoun))
			return err
		},
	}
	cmd.Flags().BoolVar(&jsonView, "json", false, wording.FlagBridgeStatusJSONUsage)
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
