// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"
)

// errorSharingViolation is the Windows error ERROR_SHARING_VIOLATION, which
// the syscall package does not name. Windows returns it when a program opens a
// file in a way that another open handle on that file does not allow.
// errors.Is finds it both bare and inside an os.PathError.
const errorSharingViolation = syscall.Errno(32)

// publishRetryTimeout is how long Write keeps retrying after a sharing
// violation. A status check holds the file open for a moment; a live daemon
// holds it open until it exits. Write retries every startupPollInterval. That
// is how often the parent process checks whether the daemon is up, and that
// check is the open that collides with Write.
const publishRetryTimeout = time.Second

// Write creates the pidfile, writes this process's pid into it, and keeps the
// file open until Remove or exit. The open handle is the lock: it has write
// access and lets every other open read the file but not write or delete it.
// While it is open, Windows rejects every other open for writing or deleting,
// and every open that says no one else may write the file. Windows closes the
// handle when the process dies, so a new process that gets the same pid never
// looks like the daemon. CREATE_ALWAYS replaces a leftover file that nobody
// holds open. When a live daemon holds the file open, it fails with a sharing
// violation. A status check that runs at the same moment opens the file in a
// way that says no one else may write it, so it makes Write fail the same way;
// that is why Write retries for a short time. Write takes no pid argument: the
// process that holds the handle is always the one whose pid is in the file.
func (p *PidFile) Write() error {
	name, err := syscall.UTF16PtrFromString(p.path)
	if err != nil {
		return fmt.Errorf("publish pidfile %q: %w", p.path, err)
	}
	deadline := time.Now().Add(publishRetryTimeout)
	var handle syscall.Handle
	for {
		handle, err = syscall.CreateFile(name, syscall.GENERIC_WRITE, syscall.FILE_SHARE_READ, nil,
			syscall.CREATE_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
		if err == nil {
			break
		}
		if !errors.Is(err, errorSharingViolation) || !time.Now().Before(deadline) {
			return fmt.Errorf("publish pidfile %q: %w", p.path, err)
		}
		time.Sleep(startupPollInterval)
	}
	file := os.NewFile(uintptr(handle), p.path)
	if _, err := file.WriteString(strconv.Itoa(os.Getpid())); err != nil {
		file.Close()
		_ = os.Remove(p.path)
		return fmt.Errorf("write pidfile %q: %w", p.path, err)
	}
	p.lock = file
	return nil
}

// Remove closes this process's handle, then deletes the pidfile. In a process
// that did not write the file it does nothing. The close must come first: the
// open handle blocks deletes from every other handle, including a second one
// in this same process. Between the close and the delete a new daemon may
// publish its own pidfile, or a status check may hold the file for an instant
// and then remove the leftover itself; either way the delete fails with a
// sharing violation. That, and a file that a status check already removed, are
// not errors.
func (p *PidFile) Remove() error {
	if p.lock == nil {
		return nil
	}
	p.lock.Close()
	p.lock = nil
	err := os.Remove(p.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, errorSharingViolation) {
		return fmt.Errorf("remove pidfile %q: %w", p.path, err)
	}
	return nil
}

// RunningPID returns the daemon's pid and true when the daemon still holds the
// pidfile open and the file holds a positive pid. Otherwise it returns 0 and
// false, and removes a leftover file that nobody holds open. This is the one
// place that decides whether the daemon is running. To test the lock it opens
// the file for reading while allowing only other readers. A sharing violation
// then means another handle has write or delete access, or does not share read
// access. The daemon's handle is the normal cause: it has write access. A
// program that only reads the file does not cause one. The standard
// os.OpenFile always allows writers, so the test calls CreateFile directly.
// The pid is then read with a normal open, which the daemon's handle allows. A
// handle that lets nobody read, or one that is deleting the file, makes that
// read fail, and the file is left alone.
func (p *PidFile) RunningPID() (int, bool) {
	name, err := syscall.UTF16PtrFromString(p.path)
	if err != nil {
		p.logger.Warn("cannot tell whether the daemon is running", "pidfile", p.path, "error", err)
		return 0, false
	}
	handle, err := syscall.CreateFile(name, syscall.GENERIC_READ, syscall.FILE_SHARE_READ, nil,
		syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	switch {
	case err == nil:
		syscall.CloseHandle(handle)
		_ = os.Remove(p.path)
		return 0, false
	case errors.Is(err, os.ErrNotExist):
		return 0, false
	case !errors.Is(err, errorSharingViolation):
		// Not a sharing violation, so this says nothing about the daemon. Leave
		// the file.
		p.logger.Warn("cannot open the pidfile", "pidfile", p.path, "error", err)
		return 0, false
	}
	raw, err := os.ReadFile(p.path)
	if err != nil {
		// The file vanished since the lock test, or its holder does not allow
		// readers. Nothing to remove either way.
		if !errors.Is(err, os.ErrNotExist) {
			p.logger.Warn("cannot read the pidfile", "pidfile", p.path, "error", err)
		}
		return 0, false
	}
	pid, ok := parsePid(raw)
	if !ok {
		// The file is held open but does not hold a positive pid: another
		// program wrote it, or a daemon opened it and has not written yet. Leave
		// it alone.
		return 0, false
	}
	return pid, true
}

// processAlive reports whether the process with the given pid still runs.
// Zero and negative pids are rejected first: no daemon has one, and Windows
// takes the pid as a 32-bit unsigned number, so a negative value would become
// a large positive one that could match a live process. OpenProcess asks only
// for the right to wait on the process. Permission denied means the process
// exists but this user may not open it, so it counts as alive. A successful
// open does not prove the process is alive: Stop holds a handle to the daemon
// while it waits, and that handle keeps the exited process openable by pid.
// So processAlive waits on the new handle with a zero timeout. The wait
// returns at once: timeout means the process still runs, signaled means it
// has exited. Only the timeout result counts as alive.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return errors.Is(err, os.ErrPermission)
	}
	defer syscall.CloseHandle(handle)
	waitResult, err := syscall.WaitForSingleObject(handle, 0)
	return err == nil && waitResult == syscall.WAIT_TIMEOUT
}
