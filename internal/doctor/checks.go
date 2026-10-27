package doctor

import (
	"context"
	"log/slog"

	"github.com/aws/device-connect-devicefarm/internal/bridge"
	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/session"
)

// Inputs is what one run reads. Config is the listener config the daemon would start with;
// local.listen-port renders a Validate failure as its FAIL. Empty SessionARN skips the session
// and relay rows. Nil Credentials means session.ResolveCredentials, nil Resolver the real
// client, nil Dialer one on Config.DialTimeout, nil Logger slog.Default.
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
	inputs = withDefaults(inputs)
	state := &runState{}
	return append(localChecks(inputs, state), remoteChecks(inputs, state)...)
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
