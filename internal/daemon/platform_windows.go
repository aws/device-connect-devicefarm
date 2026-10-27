package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/aws/device-connect-devicefarm/internal/config"
)

// detachedProcessAttributes gives the daemon child no console; closing the
// parent's console does not kill it.
var detachedProcessAttributes = &syscall.SysProcAttr{
	CreationFlags: 0x00000008, // winbase.h DETACHED_PROCESS, not in syscall
}

// openNoFollow is zero: os.OpenFile has no Windows flag that refuses a symlink.
const openNoFollow = 0

// RuntimeDirEnvVar is the environment variable that sets the runtime dir's parent; os.UserCacheDir reads it.
const RuntimeDirEnvVar = "LocalAppData"

// defaultRuntimeDir is devicefarm-connect under %LocalAppData%. MkdirAll
// ignores the mode on Windows; the dir inherits the per-user profile ACL, so no
// owner check. An unset %LocalAppData% is an error, no fallback.
func defaultRuntimeDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve runtime dir: %w", err)
	}
	runtimeDir := filepath.Join(base, config.BinaryName)
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return "", fmt.Errorf("create runtime dir %q: %w", runtimeDir, err)
	}
	return runtimeDir, nil
}
