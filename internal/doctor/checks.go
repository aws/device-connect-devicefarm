package doctor

import (
	"context"
	"log/slog"

	"github.com/aws/device-connect-devicefarm/internal/bridge"
	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/session"
)

// Inputs is what one run reads: the listener config the daemon would start with
// (local.listen-port renders a Validate failure as its FAIL), the session to probe, the AWS
// boundary, the websocket dialer and the logger. Nil Credentials means
// session.ResolveCredentials, nil Logger slog.Default.
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
	if inputs.Logger == nil {
		inputs.Logger = slog.Default()
	}
	if inputs.Credentials == nil {
		inputs.Credentials = session.ResolveCredentials
	}
	state := &runState{}
	return append(localChecks(inputs, state), remoteChecks(inputs, state)...)
}
