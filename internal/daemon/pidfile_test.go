package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func tempPidFile(t *testing.T) *PidFile {
	t.Helper()
	pf := NewPidFile(filepath.Join(t.TempDir(), "devicefarm-connect.pid"), nil)
	// Close the pidfile before TempDir's cleanup removes the directory: on
	// Windows the pidfile's open handle blocks its deletion.
	t.Cleanup(func() { _ = pf.Remove() })
	return pf
}

func TestGivenNoPidFileWhenRunningCheckedThenReportsNotRunning(t *testing.T) {
	pf := tempPidFile(t)

	pid, running := pf.RunningPID()

	assert.False(t, running)
	assert.Zero(t, pid)
}

func TestGivenLockedPidFileWhenRunningCheckedThenReportsRunning(t *testing.T) {
	pf := tempPidFile(t)
	// Write holds the lock in this process, as a live daemon would.
	require.NoError(t, pf.Write())

	pid, running := pf.RunningPID()

	assert.True(t, running)
	assert.Equal(t, os.Getpid(), pid)
}

func TestGivenPidFileWhoseWriterReleasedTheLockWhenRunningCheckedThenReportsNotRunningAndRemovesIt(t *testing.T) {
	// The pid is alive (this process) but the lock is gone: a dead daemon whose
	// pid was reused. The lock decides, not the pid.
	pf := tempPidFile(t)
	require.NoError(t, pf.Write())
	require.NoError(t, pf.lock.Close())

	pid, running := pf.RunningPID()

	assert.False(t, running)
	assert.Zero(t, pid)
	_, err := os.Lstat(pf.path)
	assert.ErrorIs(t, err, os.ErrNotExist, "an unlocked pidfile is stale and must be removed")
}

func TestGivenWrittenPidFileWhenRemovedThenFileIsGone(t *testing.T) {
	pf := tempPidFile(t)
	require.NoError(t, pf.Write())

	require.NoError(t, pf.Remove())

	_, err := os.Lstat(pf.path)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestGivenMissingRuntimeDirWhenWrittenThenErrors(t *testing.T) {
	pf := NewPidFile(filepath.Join(t.TempDir(), "no-such-dir", "devicefarm-connect.pid"), nil)

	err := pf.Write()

	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestGivenRecordContentWhenParsedThenOnlyPositiveIntegersPass(t *testing.T) {
	cases := []struct {
		raw     string
		wantPid int
		wantOK  bool
	}{
		{"123\n", 123, true},
		{" 7 ", 7, true},
		{"0", 0, false},
		{"-1", 0, false},
		{"abc", 0, false},
		{"", 0, false},
	}
	for _, testCase := range cases {
		t.Run(strconv.Quote(testCase.raw), func(t *testing.T) {
			pid, ok := parsePid([]byte(testCase.raw))

			assert.Equal(t, testCase.wantOK, ok)
			assert.Equal(t, testCase.wantPid, pid)
		})
	}
}
