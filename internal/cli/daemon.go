//go:build unix

package cli

import (
	"context"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/aws/device-connect-devicefarm/internal/daemon"
)

// newDaemonCommand is the hidden foreground daemon body. `proxy start` re-execs
// the binary with this subcommand as a detached process; it is not intended for
// direct use, so it is hidden from help output. It runs the bridge until
// SIGINT/SIGTERM, which the start/stop lifecycle delivers.
func newDaemonCommand(state *rootState) *cobra.Command {
	cmd := &cobra.Command{
		Use:    daemon.DaemonSubcommand,
		Short:  "Run the proxy in the foreground (internal; used by 'proxy start')",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			manager, err := state.newDaemonManager(ctx)
			if err != nil {
				return err
			}
			return manager.RunForeground(ctx)
		},
	}
	// The daemon is re-execed with the listener flags (config.AsDaemonArgs), so it
	// must accept them even though it is hidden. --log is not forwarded: the parent
	// redirects the child's stdout/stderr into the logfile, so the child needs no
	// logfile path of its own.
	bindListenerFlags(cmd, &state.cfg)
	bindReverseFlag(cmd, &state.cfg)
	return cmd
}
