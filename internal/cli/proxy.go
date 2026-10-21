package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/control"
)

// newProxyCommand groups the daemon-lifecycle subcommands under one parent:
// "proxy start" launches the background daemon, "proxy stop" stops it, and
// "proxy status" reports on it. Grouping them (rather than top-level
// start-proxy/kill-proxy/status commands) keeps the verb tied to the noun it acts
// on.
func newProxyCommand(state *rootState) *cobra.Command {
	proxy := &cobra.Command{
		Use:   "proxy",
		Short: "Manage the " + config.BinaryName + " daemon",
		Args:  cobra.NoArgs,
		// A RunE makes the parent runnable, so cobra validates Args (NoArgs) rather
		// than short-circuiting to help for a non-runnable command: `proxy` alone
		// prints help and exits 0, while `proxy <unknown>` is rejected with a
		// non-zero exit instead of silently printing help.
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	proxy.AddCommand(
		newProxyStartCommand(state),
		newProxyStopCommand(state),
		newProxyStatusCommand(state),
	)
	return proxy
}

// newProxyStartCommand launches the background daemon and, when a session ARN is
// supplied, attaches that device. It validates the configuration up front so a
// misconfigured invocation fails before a child is ever spawned. --session-arn is
// optional: with it, "proxy start" runs the attach logic immediately after the
// daemon is up (the daemon resolves the ARN); without it, this command attaches
// nothing and a later "device add" connects one.
//
// Resolution now happens in the daemon, not before the spawn, so a bad ARN or an
// unreachable session no longer fails before the listener exists: the daemon
// starts and the attach then fails. That is intentional: the listener is left
// running and the error tells the user to retry "device add", matching the
// no-ARN path that also leaves the daemon up with no device.
func newProxyStartCommand(state *rootState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: fmt.Sprintf("Start the proxy daemon, optionally attaching a %s remote access device", productName),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := state.cfg.Validate(); err != nil {
				return err
			}

			attachRequested := state.sessionARN != ""

			if err := state.newManager().Start(); err != nil {
				return err
			}

			if attachRequested {
				if err := attachToDaemon(cmd, state); err != nil {
					return fmt.Errorf(
						"%s daemon started but attaching the device failed; fix the issue and retry 'device add': %w",
						config.BinaryName, err,
					)
				}
				return nil
			}

			// Start no-ops if a daemon already runs; its port and device may differ
			// from the flags, so ask it.
			statusResponse, err := state.controlClient().Send(control.Request{Op: control.OpStatus})
			if err != nil {
				if daemonNotRunning(err) {
					return notListeningError()
				}
				return err
			}
			printListeningLine(cmd, statusResponse)
			return nil
		},
	}
	// The listener and logfile flags live here, not on the root command: `proxy
	// start` is the only user-facing command that stands up the daemon, so it is
	// the only one for which the port, address, and logfile are meaningful.
	bindListenerFlags(cmd, &state.cfg)
	bindReverseFlag(cmd, &state.cfg)
	cmd.Flags().StringVar(&state.cfg.LogPath, "log", state.cfg.LogPath, "daemon logfile path (defaults to the per-user runtime directory)")
	addSessionArnFlag(cmd, state, sessionArnDescription+" (optional; attaches the device on start)")
	return cmd
}

// newProxyStopCommand stops the running daemon. If none is running it reports that
// cleanly rather than failing, so "proxy stop" is idempotent.
func newProxyStopCommand(state *rootState) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the running proxy daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, running := state.newManager().Status(); !running {
				cmd.Println(notListeningMessage)
				return nil
			}
			if err := state.newManager().Stop(); err != nil {
				return err
			}
			cmd.Println(config.BinaryName + " stopped")
			return nil
		},
	}
}

// newProxyStatusCommand reports whether the daemon is running and which device,
// if any, is attached. It prints a human-readable line to stdout (status output,
// not a log event).
func newProxyStatusCommand(state *rootState) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report whether the proxy daemon is running and the attached device",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pid, running := state.newManager().Status()
			if !running {
				cmd.Println(config.BinaryName + " daemon is not running")
				return nil
			}

			// The daemon is up; ask it over the control channel which device is
			// attached so status reflects live state, not just liveness.
			resp, err := state.controlClient().Send(control.Request{Op: control.OpStatus})
			if err != nil {
				if daemonNotRunning(err) {
					cmd.Println(config.BinaryName + " daemon is not running")
					return nil
				}
				return err
			}
			if resp.DeviceLabel == "" {
				cmd.Printf("%s daemon is running (pid %d), no device attached\n", config.BinaryName, pid)
			} else {
				cmd.Printf("%s daemon is running (pid %d), device attached: %s\n", config.BinaryName, pid, resp.DeviceLabel)
			}
			return nil
		},
	}
}
