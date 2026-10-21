//go:build unix

package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/aws/device-connect-devicefarm/internal/config"
)

// detachedProcessAttributes puts the daemon child in its own session; a hangup
// on the parent's terminal does not reach it.
var detachedProcessAttributes = &syscall.SysProcAttr{Setsid: true}

// openNoFollow makes the log open refuse a symlink.
const openNoFollow = syscall.O_NOFOLLOW

// defaultRuntimeDir is devicefarm-connect under $XDG_RUNTIME_DIR, else
// devicefarm-connect-<uid> under the OS temp dir. $XDG_RUNTIME_DIR is already
// per user. The temp dir is shared, so the uid goes in the name: without it the
// first user's 0700 dir would lock every other user out. ensureRuntimeDir
// creates and owner-checks it.
func defaultRuntimeDir() (string, error) {
	uid := os.Getuid()
	var runtimeDir string
	if base := os.Getenv("XDG_RUNTIME_DIR"); base != "" {
		runtimeDir = filepath.Join(base, config.BinaryName)
	} else {
		runtimeDir = filepath.Join(os.TempDir(), fmt.Sprintf("%s-%d", config.BinaryName, uid))
	}
	if err := ensureRuntimeDir(runtimeDir, uid); err != nil {
		return "", err
	}
	return runtimeDir, nil
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
