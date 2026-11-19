//go:build unix

package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/aws/device-connect-devicefarm/internal/config"
)

// Paths locates the per-user runtime files for the daemon: the pidfile (tracks
// the running daemon), the control socket (the CLI's channel to the running
// daemon), and the logfile (which captures the detached child's stdout/stderr)
// all live under a single per-user runtime directory. Colocating the logfile
// there (rather than in the shared system temp dir) avoids a cross-user collision
// on the fixed devicefarm-connect.log name and keeps every daemon artifact under one
// 0700 directory.
type Paths struct {
	RuntimeDir    string
	PidFile       string
	LogFile       string
	ControlSocket string
}

// DefaultPaths resolves the runtime paths. The pidfile, control socket, and
// logfile all live under the user's runtime directory, preferring
// $XDG_RUNTIME_DIR and falling back to the OS temp dir; the directory is created
// at 0700 if absent and refused if another user owns it. A per-user location
// (rather than a fixed /var/run path) keeps the tool runnable without elevated
// privileges, and colocating the logfile there (rather than in the shared temp
// dir) avoids a cross-user collision on the fixed devicefarm-connect.log name. An
// explicit --log overrides the logfile path.
//
// $XDG_RUNTIME_DIR is already per-user, so the directory name is fixed under it.
// The temp-dir fallback is shared across users, so the uid is folded into the
// directory name there: without it the first user to run the tool would create
// the 0700 devicefarm-connect dir and every other user would be refused as not its
// owner.
func DefaultPaths() (Paths, error) {
	uid := os.Getuid()
	var runtimeDir string
	if base := os.Getenv("XDG_RUNTIME_DIR"); base != "" {
		runtimeDir = filepath.Join(base, config.BinaryName)
	} else {
		runtimeDir = filepath.Join(os.TempDir(), fmt.Sprintf("%s-%d", config.BinaryName, uid))
	}
	if err := ensureRuntimeDir(runtimeDir, uid); err != nil {
		return Paths{}, err
	}
	return Paths{
		RuntimeDir:    runtimeDir,
		PidFile:       filepath.Join(runtimeDir, config.BinaryName+".pid"),
		LogFile:       filepath.Join(runtimeDir, config.BinaryName+".log"),
		ControlSocket: filepath.Join(runtimeDir, config.BinaryName+".sock"),
	}, nil
}

// ensureRuntimeDir creates dir at 0700 and refuses it when another uid owns it.
// MkdirAll accepts an existing directory, and on a shared temp dir another user
// can pre-create one. Lstat judges a symlink by its own owner, who can retarget
// it. Mode and type are not checked: only the owner or root can change them.
func ensureRuntimeDir(dir string, uid int) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create runtime dir %q: %w", dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("stat runtime dir %q: %w", dir, err)
	}
	// Sys is *syscall.Stat_t on unix.
	owner := int(info.Sys().(*syscall.Stat_t).Uid)
	if owner != uid {
		return fmt.Errorf("runtime dir %q is owned by uid %d, not %d; set XDG_RUNTIME_DIR to a private directory", dir, owner, uid)
	}
	return nil
}
