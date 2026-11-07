// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
)

// PidFile manages the daemon's pidfile: the file on disk that holds the
// daemon's process id. The daemon that writes the file keeps it locked until
// it exits, so "the daemon is running" means "the file is locked". A new
// process that gets a dead daemon's pid does not hold the lock, so RunningPID
// never takes it for the daemon. A file that is not locked is left over from a
// crash and is removed. Only the writer removes a locked file. Each OS locks
// the file in its own way: Write, Remove, RunningPID and processAlive live in
// pidfile_unix.go and pidfile_windows.go.
type PidFile struct {
	path   string
	logger *slog.Logger
	// lock is the open pidfile in the daemon that wrote it; nil in every other
	// process. The lock ends when Remove closes the file, when the process
	// exits, or when the garbage collector closes a file that nothing points to
	// anymore, so the daemon must keep a reference to this PidFile for as long
	// as it runs.
	lock *os.File
}

// NewPidFile returns a PidFile bound to the given path. A nil logger means
// slog.Default.
func NewPidFile(path string, logger *slog.Logger) *PidFile {
	if logger == nil {
		logger = slog.Default()
	}
	return &PidFile{path: path, logger: logger}
}

// parsePid reads the pid from the file content. Zero and negative numbers are
// rejected: kill(2) treats pid 0 and negative pids as a process group or as
// every process, never as one daemon.
func parsePid(raw []byte) (int, bool) {
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}
