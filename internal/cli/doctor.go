package cli

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/aws/device-connect-devicefarm/internal/doctor"
)

// defaultReportWidth is the table width when stdout is not a terminal and COLUMNS is unset.
const defaultReportWidth = 80

// newDoctorCommand runs the readiness checks and prints the report on stdout. Its own
// PersistentPreRunE replaces the root hook, which would fail on a refused runtime dir before
// any row prints; it still accepts the command line and sets up logging. state.paths stays unset.
func newDoctorCommand(state *rootState) *cobra.Command {
	verbose := false
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check this machine, the AWS credentials and a session before 'bridge start'",
		Long: fmt.Sprintf(`Run readiness checks for 'bridge start', 'device add', an adb connection, a
reconnect to the device, 'adb forward' and 'adb reverse', and print one table
row per category:

  Local machine  the working directory, the daemon, the ports the bridge needs
  AWS            this shell's AWS credentials and the remote access session
  Network        the adb connection to the device, its reconnect and adb reverse
  Dependencies   the adb on this machine against the device's adb server

A row is FAIL when a check in it failed, WARN when one works with a caveat, SKIP
when none ran, else PASS. The Details column names each check that did not pass
and why. A "What to do" list under the table gives the fix for each WARN or
FAIL, with the command to run. The table fits the terminal width, or COLUMNS
when stdout is not a terminal. --verbose prints one line per check instead.
Exit code 0 unless a check is FAIL. Without --session-arn the session, the
connection to the device and the adb versions are not checked. Run the doctor
from the shell that will run 'bridge start': the daemon inherits that shell's
environment. A running daemon is reported, not changed.

Side effects:
  creates the client's working directory if absent; forgets a daemon that no
  longer runs
  binds and frees the listen address when no daemon holds it, and one port on
  127.0.0.1 for 'adb forward'
  asks whatever holds the listen address for its adb version
  makes one GetRemoteAccessSession call
  opens one short connection to the %s %s device per Network and
  Dependencies check
  runs 'adb version' on each adb it finds on PATH, in ANDROID_HOME or
  ANDROID_SDK_ROOT, or in Android Studio's default SDK location; none starts an
  adb server
Starts no daemon; installs no forward or reverse rule. Each check has its own
timeout; a run in which every check times out ends in under two minutes.`, awsName, productName),
		Args: cobra.NoArgs,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			state.commandLineAccepted = true
			initLogging(cmd.ErrOrStderr(), state.cfg.Debug)
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Ctrl-C ends the current check and prints what ran so far.
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			report := doctor.Run(ctx, doctor.Checks(doctor.Inputs{Config: state.cfg, SessionARN: state.sessionARN}))
			var writeErr error
			if verbose {
				writeErr = doctor.WriteText(cmd.OutOrStdout(), report)
			} else {
				writeErr = doctor.WriteTable(cmd.OutOrStdout(), report, reportWidth(cmd.OutOrStdout()))
			}
			if writeErr != nil {
				return writeErr
			}
			if failed := report.Failed(); failed > 0 {
				return fmt.Errorf("doctor: %d checks failed", failed)
			}
			return nil
		},
	}
	// The listen-port check binds the address bridge start would, so it takes the same flags.
	bindListenerFlags(cmd, &state.cfg)
	addSessionArnFlag(cmd, state, sessionArnDescription+" (optional; also checks the session, the connection to the device and the adb versions)")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "print one line per check, with its id, instead of the table")
	return cmd
}

// reportWidth is the table width: a positive terminal size when stdout is a terminal, else
// COLUMNS when it is a positive integer, else defaultReportWidth. The writer clamps it.
func reportWidth(stdout io.Writer) int {
	if file, isFile := stdout.(*os.File); isFile && term.IsTerminal(int(file.Fd())) {
		if width, _, err := term.GetSize(int(file.Fd())); err == nil && width > 0 {
			return width
		}
	}
	if columns, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && columns > 0 {
		return columns
	}
	return defaultReportWidth
}
