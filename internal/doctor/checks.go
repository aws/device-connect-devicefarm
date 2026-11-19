package doctor

import (
	"log/slog"

	"github.com/aws/device-connect-devicefarm/internal/bridge"
	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/session"
)

// Inputs is what one run reads: the listener config the daemon would start with, the session
// to probe, the AWS boundary, the websocket dialer and the logger.
type Inputs struct {
	Config     config.ProxyConfig
	SessionARN string
	Resolver   session.RemoteAccessClient
	Dialer     *bridge.WSDialer
	Logger     *slog.Logger
}

// Checks returns the check table, in run order. No check is registered yet.
func Checks(_ Inputs) []Check {
	return nil
}
