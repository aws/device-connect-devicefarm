// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package doctor

import (
	"context"
	"log/slog"

	"github.com/aws/device-connect-devicefarm/internal/bridge"
	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/session"
)

// Inputs is what one run reads. Config is the listener config the daemon would start with;
// local.listen-port renders a Validate failure as its FAIL. An empty SessionARN means the
// session of the one device the running daemon holds; with no daemon, no device or several
// devices the session and relay rows skip. Nil Credentials means session.ResolveCredentials,
// nil Resolver the real client, nil Dialer one on Config.DialTimeout, nil Logger slog.Default.
type Inputs struct {
	Config      config.ProxyConfig
	SessionARN  string
	Credentials func(context.Context) (session.CredentialSummary, error)
	Resolver    session.RemoteAccessClient
	Dialer      *bridge.WSDialer
	Logger      *slog.Logger
}

// Checks builds the table in run order. Later checks read what earlier ones store in runState.
func Checks(inputs Inputs) []Check {
	checks, _ := checksWithState(inputs)
	return checks
}

// Diagnose builds the table, runs it, and records on the report the session the run checked.
func Diagnose(ctx context.Context, inputs Inputs) Report {
	checks, state := checksWithState(inputs)
	report := Run(ctx, checks)
	report.SessionARN, _, _ = sessionToCheck(inputs, state)
	return report
}

// checksWithState builds the table and the run state its checks share.
func checksWithState(inputs Inputs) ([]Check, *runState) {
	inputs = withDefaults(inputs)
	state := &runState{}
	return append(localChecks(inputs, state), remoteChecks(inputs, state)...), state
}

// withDefaults fills the nil seams.
func withDefaults(inputs Inputs) Inputs {
	if inputs.Logger == nil {
		inputs.Logger = slog.Default()
	}
	if inputs.Credentials == nil {
		inputs.Credentials = session.ResolveCredentials
	}
	if inputs.Dialer == nil {
		dialTimeout := inputs.Config.DialTimeout
		if dialTimeout <= 0 {
			dialTimeout = config.DefaultDialTimeout
		}
		inputs.Dialer = bridge.NewWSDialer(dialTimeout)
	}
	return inputs
}
