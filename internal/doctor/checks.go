package doctor

import (
	"log/slog"

	"github.com/aws/device-connect-devicefarm/internal/bridge"
	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/session"
)

// Inputs is what one run reads: the listener config the daemon would start with, the session
// to probe, the AWS boundary, the websocket dialer and the logger. Nil Logger means slog.Default.
type Inputs struct {
	Config     config.ProxyConfig
	SessionARN string
	Resolver   session.RemoteAccessClient
	Dialer     *bridge.WSDialer
	Logger     *slog.Logger
}

// Checks builds the table in run order. Later checks read what earlier ones store in runState.
func Checks(inputs Inputs) []Check {
	if inputs.Logger == nil {
		inputs.Logger = slog.Default()
	}
	return localChecks(inputs, &runState{})
}
