//go:build unix

package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGivenXDGRuntimeDirSetWhenDefaultPathsResolvedThenAllArtifactsUnderRuntimeDir(t *testing.T) {
	runtimeBase := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeBase)

	paths, err := DefaultPaths()

	require.NoError(t, err)
	wantDir := filepath.Join(runtimeBase, "devicefarm-connect")
	assert.Equal(t, wantDir, paths.RuntimeDir)
	assert.Equal(t, filepath.Join(wantDir, "devicefarm-connect.pid"), paths.PidFile)
	assert.Equal(t, filepath.Join(wantDir, "devicefarm-connect.sock"), paths.ControlSocket)
	// The logfile lives in the runtime dir alongside the other artifacts, not in
	// the shared system temp dir.
	assert.Equal(t, filepath.Join(wantDir, "devicefarm-connect.log"), paths.LogFile)

	info, statErr := os.Stat(wantDir)
	require.NoError(t, statErr, "the runtime dir must be created")
	assert.True(t, info.IsDir())
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "the runtime dir must be private to the user")
}

func TestGivenNoXDGRuntimeDirWhenDefaultPathsResolvedThenFallsBackToTempDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	// Point the temp dir at a throwaway location so the fallback's real MkdirAll
	// does not create the runtime dir under the machine's shared /tmp.
	t.Setenv("TMPDIR", t.TempDir())

	paths, err := DefaultPaths()

	require.NoError(t, err)
	// The temp-dir fallback folds in the uid to avoid a cross-user collision on
	// the shared temp dir.
	wantDir := filepath.Join(os.TempDir(), fmt.Sprintf("devicefarm-connect-%d", os.Getuid()))
	assert.Equal(t, wantDir, paths.RuntimeDir)
	assert.Equal(t, filepath.Join(wantDir, "devicefarm-connect.log"), paths.LogFile)
}

func TestGivenRuntimeDirPathOccupiedByFileWhenDefaultPathsResolvedThenErrors(t *testing.T) {
	// A regular file where the per-user runtime dir should be created makes
	// MkdirAll fail, which DefaultPaths must surface rather than swallow.
	base := t.TempDir()
	collision := filepath.Join(base, "devicefarm-connect")
	require.NoError(t, os.WriteFile(collision, []byte("not a dir"), 0o600))
	t.Setenv("XDG_RUNTIME_DIR", base)

	_, err := DefaultPaths()

	require.Error(t, err)
}

func TestGivenWideRuntimeDirOwnedByUserWhenDefaultPathsResolvedThenAcceptedAsFound(t *testing.T) {
	// A wide dir the user owns is accepted and left as found.
	base := t.TempDir()
	ownDir := filepath.Join(base, "devicefarm-connect")
	require.NoError(t, os.Mkdir(ownDir, 0o755))
	// Chmod: umask masks Mkdir's mode.
	require.NoError(t, os.Chmod(ownDir, 0o755))
	t.Setenv("XDG_RUNTIME_DIR", base)

	paths, err := DefaultPaths()

	require.NoError(t, err)
	assert.Equal(t, ownDir, paths.RuntimeDir)
	info, statErr := os.Stat(ownDir)
	require.NoError(t, statErr)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}

func TestGivenRuntimeDirOwnedByAnotherUidWhenEnsuredThenErrors(t *testing.T) {
	// No chown without root: expect a uid that is not the owner.
	dir := t.TempDir()

	err := ensureRuntimeDir(dir, os.Getuid()+1)

	require.ErrorContains(t, err, "owned by uid")
	assert.ErrorContains(t, err, "set XDG_RUNTIME_DIR")
}

func TestGivenRealExecutableWhenSpawnedThenReturnsLivePidAndDetaches(t *testing.T) {
	logOut, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	require.NoError(t, err)
	defer logOut.Close()

	// Spawn a real short-lived process through the production spawn path.
	pid, err := realSpawnDaemon("/bin/sh", []string{"-c", "sleep 30"}, logOut)
	require.NoError(t, err)
	require.Greater(t, pid, 0)
	t.Cleanup(func() {
		if proc, findErr := os.FindProcess(pid); findErr == nil {
			_ = proc.Signal(syscall.SIGKILL)
			_ = proc.Release()
		}
	})

	assert.True(t, processAlive(pid), "the spawned child must be alive")
	// Setsid detaches the child into its own session, so it leads a new process
	// group whose id equals its pid.
	pgid, err := syscall.Getpgid(pid)
	require.NoError(t, err)
	assert.Equal(t, pid, pgid, "the detached child must lead its own session (Setsid)")
}
