package daemon

import (
	"path/filepath"

	"github.com/aws/device-connect-devicefarm/internal/config"
)

// Paths locates the per-user runtime files for the daemon: the pidfile (tracks
// the running daemon), the control socket (the CLI's channel to the running
// daemon), and the logfile (which captures the detached child's stdout/stderr)
// all live under a single per-user runtime directory. Colocating the logfile
// there (rather than in the shared system temp dir) avoids a cross-user collision
// on the fixed devicefarm-connect.log name and keeps every daemon artifact under one
// private directory.
type Paths struct {
	RuntimeDir    string
	PidFile       string
	LogFile       string
	ControlSocket string
}

// DefaultPaths puts the pidfile, control socket, and logfile under the per-user
// runtime directory from defaultRuntimeDir. A per-user location needs no
// elevated privileges. The CLI replaces LogFile when --log is set.
func DefaultPaths() (Paths, error) {
	runtimeDir, err := defaultRuntimeDir()
	if err != nil {
		return Paths{}, err
	}
	return Paths{
		RuntimeDir:    runtimeDir,
		PidFile:       filepath.Join(runtimeDir, config.BinaryName+".pid"),
		LogFile:       filepath.Join(runtimeDir, config.BinaryName+".log"),
		ControlSocket: filepath.Join(runtimeDir, config.BinaryName+".sock"),
	}, nil
}
