// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/aws/device-connect-devicefarm/internal/doctor"
	"github.com/aws/device-connect-devicefarm/internal/report"
	"github.com/aws/device-connect-devicefarm/internal/wording"
)

// newDoctorCommand runs the readiness checks and prints the report on stdout. Its own
// PersistentPreRunE replaces the root hook, which would fail on a refused runtime dir before
// any row prints; it still accepts the command line and sets up logging. state.paths stays unset.
func newDoctorCommand(state *rootState) *cobra.Command {
	verbose := false
	jsonReport := false
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: wording.HelpDoctor,
		Long:  wording.HelpDoctorLong,
		Args:  cobra.NoArgs,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			state.commandLineAccepted = true
			initLogging(cmd, state.cfg.Debug)
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Ctrl-C ends the current check and prints what ran so far.
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			results := doctor.Diagnose(ctx, doctor.Inputs{Config: state.cfg, SessionARN: state.sessionARN})
			var writeErr error
			switch {
			case jsonReport:
				writeErr = doctor.WriteJSON(cmd.OutOrStdout(), results)
			case verbose:
				writeErr = doctor.WriteText(cmd.OutOrStdout(), results)
			default:
				writeErr = doctor.WriteTable(cmd.OutOrStdout(), results, report.TerminalWidth(cmd.OutOrStdout()))
			}
			if writeErr != nil {
				return writeErr
			}
			switch failed := results.Failed(); {
			case failed == 1:
				return errors.New(wording.ProblemDoctorOneCheckFailed)
			case failed > 1:
				return fmt.Errorf(wording.ProblemDoctorChecksFailedFormat, failed)
			}
			return nil
		},
	}
	// The listen-port check binds the address bridge start would, so it takes the same flags.
	bindListenerFlags(cmd, &state.cfg)
	addSessionArnFlag(cmd, state, wording.FlagSessionARNDoctorUsage)
	cmd.Flags().BoolVar(&verbose, "verbose", false, wording.FlagVerboseUsage)
	cmd.Flags().BoolVar(&jsonReport, "json", false, wording.FlagDoctorJSONUsage)
	return cmd
}
