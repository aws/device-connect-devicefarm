//go:build unix

package daemon

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// pathLockSuffix names the lock file beside the pidfile. Never removed: an
// unlinked lock file would let two processes lock different inodes.
const pathLockSuffix = ".lock"

// Write records this process's pid in a new pidfile and holds its lock until
// Remove or exit. No pid parameter: the lock always vouches for the recorded
// pid. The record is built under a private name, locked, written, then linked
// into place, so a poller never sees it half-written or unlocked. Under the
// path lock, Write first clears a leftover or symlink at the path; link then
// fails if the path exists, so a live record is never overwritten and a planted
// symlink is never followed. The daemon child calls this on boot, not the
// parent, so one process owns the file.
func (p *PidFile) Write() error {
	file, err := os.CreateTemp(filepath.Dir(p.path), "."+filepath.Base(p.path)+".")
	if err != nil {
		return fmt.Errorf("create pidfile %q: %w", p.path, err)
	}
	linked := false
	defer func() {
		if !linked {
			file.Close()
		}
		// Drop the temp name; the link at p.path carries the record. A failed drop
		// leaks one hidden temp file, as a hard kill before the link would.
		_ = os.Remove(file.Name())
	}()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("lock pidfile %q: %w", p.path, err)
	}
	if _, err := file.WriteString(strconv.Itoa(os.Getpid())); err != nil {
		return fmt.Errorf("write pidfile %q: %w", p.path, err)
	}
	pathLock, err := p.lockPath()
	if err != nil {
		return err
	}
	defer pathLock.Close()
	p.probe() // clears a leftover so link can take the path
	if err := os.Link(file.Name(), p.path); err != nil {
		return fmt.Errorf("publish pidfile %q: %w", p.path, err)
	}
	linked = true
	p.lock = file
	return nil
}

// Remove unlinks this writer's record, then releases its lock. Without the lock
// it does nothing: only the writer unlinks a locked record. While the lock is
// held nothing in this binary changes what the path names, so Remove needs no
// path lock. Unlink before release: until the lock drops the path names this
// writer's file, so the unlink cannot hit a successor's record. A missing file
// is not an error.
func (p *PidFile) Remove() error {
	if p.lock == nil {
		return nil
	}
	err := os.Remove(p.path)
	p.lock.Close()
	p.lock = nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove pidfile %q: %w", p.path, err)
	}
	return nil
}

// RunningPID returns the daemon's pid and true when the pidfile's writer still
// holds its lock and the file holds a positive pid. Otherwise it returns 0 and
// false, and removes a symlink or an unlocked file at the path. This is the
// single source of truth for "is the daemon running".
func (p *PidFile) RunningPID() (int, bool) {
	pathLock, err := p.lockPath()
	if err != nil {
		p.logger.Warn("cannot tell whether the daemon is running", "pidfile", p.path, "error", err)
		return 0, false
	}
	defer pathLock.Close()
	return p.probe()
}

// lockPath takes the path lock, which serializes changes to what the pidfile
// path names. It blocks with no deadline: holds are short and nothing sleeps
// under it. Closing the handle releases it. Not reentrant: flock is per open
// file description, so a second take in one process deadlocks. A holder calls
// probe, not RunningPID.
func (p *PidFile) lockPath() (*os.File, error) {
	lockFilePath := p.path + pathLockSuffix
	// O_NOFOLLOW: a planted link cannot make O_CREATE create a file elsewhere.
	file, err := os.OpenFile(lockFilePath, os.O_RDONLY|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open pidfile lock %q: %w", lockFilePath, err)
	}
	// A signal interrupts a blocking flock; retry.
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("lock pidfile lock %q: %w", lockFilePath, err)
	}
	return file, nil
}

// probe checks the record without taking the path lock. RunningPID and Write
// call it while they hold that lock, so the file it unlinks is the one it found
// unlocked.
func (p *PidFile) probe() (int, bool) {
	file, err := os.OpenFile(p.path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		// Clear a symlink so it cannot wedge a start. O_NOFOLLOW yields ELOOP on linux
		// and darwin, the built targets. Any other error (EMFILE, EACCES) may hide a
		// live record; keep it.
		switch {
		case errors.Is(err, os.ErrNotExist):
		case errors.Is(err, syscall.ELOOP):
			_ = os.Remove(p.path)
		default:
			p.logger.Warn("cannot open the pidfile", "pidfile", p.path, "error", err)
		}
		return 0, false
	}
	defer file.Close()

	// LOCK_SH succeeds only when no writer holds the lock: the file is a
	// leftover. An error other than EWOULDBLOCK says nothing about the writer,
	// so the file stays.
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	switch {
	case err == nil:
		_ = os.Remove(p.path)
		return 0, false
	case !errors.Is(err, syscall.EWOULDBLOCK):
		p.logger.Warn("cannot probe the pidfile lock", "pidfile", p.path, "error", err)
		return 0, false
	}
	raw, err := io.ReadAll(file)
	pid, ok := parsePid(raw)
	if err != nil || !ok {
		// The file is locked but does not hold a positive pid: a live program
		// that is not this binary wrote it. Leave it alone.
		return 0, false
	}
	return pid, true
}

// processAlive reports whether a process with the given pid currently exists.
// Signal 0 performs the kernel's existence-and-permission check without
// delivering a signal. A nil error means the process exists; EPERM means it
// exists but is owned by another user (still alive). A dead process surfaces as
// os.ErrProcessDone on both Go paths, pidfd and kill(2): os maps ESRCH itself.
// Keep treating any error but EPERM as "not alive", or a dead process reads as
// live.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Since Go 1.23 os.FindProcess opens a pidfd on Linux; release it so the fd
	// is not leaked, which matters because waitUntilStopped polls this in a loop.
	defer proc.Release()
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}
