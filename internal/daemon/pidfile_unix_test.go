// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package daemon

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loggingPidFile returns a PidFile in a temp dir whose warnings land in the
// returned buffer.
func loggingPidFile(t *testing.T) (*PidFile, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	return NewPidFile(filepath.Join(t.TempDir(), "devicefarm-connect.pid"), logger), &logs
}

// lockedFile creates path if needed and holds flock(LOCK_EX) on it, as another
// process would. The lock drops when the returned handle closes, at cleanup at
// the latest.
func lockedFile(t *testing.T, path string) *os.File {
	t.Helper()
	holder, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Close() })
	require.NoError(t, syscall.Flock(int(holder.Fd()), syscall.LOCK_EX))
	return holder
}

// holdRecord writes content at the pidfile path and holds its lock, as another
// process's live writer would.
func holdRecord(t *testing.T, pf *PidFile, content string) {
	t.Helper()
	_, err := lockedFile(t, pf.path).WriteString(content)
	require.NoError(t, err)
}

// dirNames lists the entries beside the pidfile.
func dirNames(t *testing.T, pf *PidFile) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(pf.path))
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestGivenLockedButCorruptPidFileWhenRunningCheckedThenReportsNotRunningAndKeepsIt(t *testing.T) {
	// A live writer holds the lock on content that is not a pid. It is not ours
	// to clear, and a nonpositive number must never become a signal target.
	for _, content := range []string{"not-a-pid", "0", "-1234"} {
		t.Run(content, func(t *testing.T) {
			pf := tempPidFile(t)
			holdRecord(t, pf, content)

			pid, running := pf.RunningPID()

			assert.False(t, running)
			assert.Zero(t, pid)
			_, err := os.Lstat(pf.path)
			assert.NoError(t, err, "a locked file has a live writer and is never removed")
		})
	}
}

func TestGivenRecordThisPidFileDidNotWriteWhenRemovedThenRecordIsKept(t *testing.T) {
	// Only the writer unlinks its record. A PidFile that never wrote has no lock
	// and must not unlink what it finds, locked or not.
	plants := map[string]func(t *testing.T, pf *PidFile){
		"locked by another writer": func(t *testing.T, pf *PidFile) { holdRecord(t, pf, "4242") },
		"unlocked leftover": func(t *testing.T, pf *PidFile) {
			require.NoError(t, os.WriteFile(pf.path, []byte("4242"), 0o600))
		},
	}
	for name, plant := range plants {
		t.Run(name, func(t *testing.T) {
			pf := tempPidFile(t)
			plant(t, pf)

			require.NoError(t, pf.Remove())

			_, err := os.Lstat(pf.path)
			assert.NoError(t, err, "a PidFile without the lock must leave the record in place")
		})
	}
}

func TestGivenAnotherWritersLockedRecordWhenWrittenThenRefusedAndNoTempFileLeft(t *testing.T) {
	pf := tempPidFile(t)
	first := strconv.Itoa(os.Getpid() + 1)
	holdRecord(t, pf, first)

	err := pf.Write()

	require.ErrorIs(t, err, os.ErrExist, "a second daemon must not overwrite the first's record")
	content, readErr := os.ReadFile(pf.path)
	require.NoError(t, readErr)
	assert.Equal(t, first, string(content))
	assert.ElementsMatch(t, []string{"devicefarm-connect.pid", "devicefarm-connect.pid.lock"}, dirNames(t, pf),
		"the private temp file must not be left behind")
}

func TestGivenPidFileWhoseWriterReleasedTheLockWhenWrittenThenReplacesIt(t *testing.T) {
	// A dead daemon's leftover is in place: nothing probed before this start.
	// Write must clear it, not fail.
	pf := tempPidFile(t)
	require.NoError(t, os.WriteFile(pf.path, []byte(strconv.Itoa(os.Getpid()+1)), 0o600))

	require.NoError(t, pf.Write())

	pid, running := pf.RunningPID()
	assert.True(t, running)
	assert.Equal(t, os.Getpid(), pid)
	assert.ElementsMatch(t, []string{"devicefarm-connect.pid", "devicefarm-connect.pid.lock"}, dirNames(t, pf),
		"the private temp name must be gone after the link")
}

// assertWaitsForPathLock runs op under a foreign path lock and checks op
// returns only after the lock drops.
func assertWaitsForPathLock(t *testing.T, pf *PidFile, op func()) {
	t.Helper()
	holder := lockedFile(t, pf.path+pathLockSuffix)
	done := make(chan struct{})
	go func() {
		op()
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("the operation must wait for the path lock")
	case <-time.After(50 * time.Millisecond):
	}
	require.NoError(t, holder.Close())
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the operation must proceed once the path lock drops")
	}
}

func TestGivenAnotherProcessHoldsThePathLockWhenRunningCheckedThenWaitsForIt(t *testing.T) {
	// Without this wait a probe could unlink a record another process published
	// between its lock test and its unlink.
	pf := tempPidFile(t)
	require.NoError(t, pf.Write())
	var pid int
	var running bool

	assertWaitsForPathLock(t, pf, func() { pid, running = pf.RunningPID() })

	assert.True(t, running)
	assert.Equal(t, os.Getpid(), pid)
}

func TestGivenAnotherProcessHoldsThePathLockWhenWrittenThenWaitsForIt(t *testing.T) {
	pf := tempPidFile(t)
	var err error

	assertWaitsForPathLock(t, pf, func() { err = pf.Write() })

	require.NoError(t, err)
	_, running := pf.RunningPID()
	assert.True(t, running)
}

func TestGivenSymlinkAtPathLockWhenWrittenOrCheckedThenRefused(t *testing.T) {
	// The lock file is opened with O_CREATE. O_NOFOLLOW stops a planted link from
	// creating a file elsewhere.
	pf, logs := loggingPidFile(t)
	require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), pf.path+pathLockSuffix))

	err := pf.Write()
	_, running := pf.RunningPID()

	require.ErrorIs(t, err, syscall.ELOOP)
	assert.False(t, running)
	_, err = os.Lstat(pf.path)
	assert.ErrorIs(t, err, os.ErrNotExist, "nothing may be published without the path lock")
	assert.Contains(t, logs.String(), "cannot tell whether the daemon is running",
		"path lock failure must warn, not pass as not running")
	assert.Contains(t, logs.String(), pf.path+pathLockSuffix, "the warning names the lock file")
}

func TestGivenSymlinkAtPidFilePathWhenWrittenThenLinkReplacedAndTargetUntouched(t *testing.T) {
	// A planted symlink must not redirect the record. Write clears it like a
	// leftover, links the record in its place, and never opens the target.
	pf := tempPidFile(t)
	target := filepath.Join(t.TempDir(), "elsewhere")
	require.NoError(t, os.WriteFile(target, []byte("untouched"), 0o600))
	require.NoError(t, os.Symlink(target, pf.path))

	require.NoError(t, pf.Write())

	content, readErr := os.ReadFile(target)
	require.NoError(t, readErr)
	assert.Equal(t, "untouched", string(content))
	pid, running := pf.RunningPID()
	assert.True(t, running)
	assert.Equal(t, os.Getpid(), pid)
}

func TestGivenUnreadablePidFileWhenRunningCheckedThenNotRunningAndFileKept(t *testing.T) {
	// An open error other than ELOOP may hide a live record (EMFILE, EACCES).
	// Only a symlink is cleared.
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode 0000 file")
	}
	pf, logs := loggingPidFile(t)
	require.NoError(t, os.WriteFile(pf.path, []byte(strconv.Itoa(os.Getpid())), 0o000))

	_, running := pf.RunningPID()

	assert.False(t, running)
	_, err := os.Lstat(pf.path)
	assert.NoError(t, err, "a file that cannot be opened is never removed")
	assert.Contains(t, logs.String(), "cannot open the pidfile", "non-ELOOP open error must warn")
}

func TestGivenSymlinkAtPidFilePathWhenRunningCheckedThenNotRunningAndLinkRemoved(t *testing.T) {
	// The link targets a file that names a live pid. Following it would make the
	// daemon look running. O_NOFOLLOW refuses it; RunningPID clears it.
	pf := tempPidFile(t)
	target := filepath.Join(t.TempDir(), "planted")
	require.NoError(t, os.WriteFile(target, []byte(strconv.Itoa(os.Getpid())), 0o600))
	require.NoError(t, os.Symlink(target, pf.path))

	_, running := pf.RunningPID()

	assert.False(t, running)
	_, err := os.Lstat(pf.path)
	assert.ErrorIs(t, err, os.ErrNotExist, "the planted link must be removed")
	_, err = os.Stat(target)
	assert.NoError(t, err, "only the link is removed, never its target")
}

func TestGivenUnremovablePidFileWhenRemovedThenErrorsAndLockReleased(t *testing.T) {
	// The file cannot be deleted, but the lock is still released, so the
	// leftover counts as stale instead of making a dead daemon look running.
	if os.Geteuid() == 0 {
		t.Skip("root unlinks from a read-only directory")
	}
	pf := tempPidFile(t)
	require.NoError(t, pf.Write())
	dir := filepath.Dir(pf.path)
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := pf.Remove()

	require.ErrorIs(t, err, os.ErrPermission)
	_, running := pf.RunningPID()
	assert.False(t, running, "a failed unlink must still release the lock")
	_, statErr := os.Lstat(pf.path)
	assert.NoError(t, statErr, "the record stays until something can unlink it")
}

func TestGivenNonpositivePidWhenAliveCheckedThenReportsNotAlive(t *testing.T) {
	// kill(2) with signal 0 treats pid 0 and a negative pid as a process group,
	// the caller's own group included, so without the guard such a pid would
	// count as alive.
	for _, pid := range []int{0, -1, -syscall.Getpgrp()} {
		assert.False(t, processAlive(pid), "pid %d", pid)
	}
}
