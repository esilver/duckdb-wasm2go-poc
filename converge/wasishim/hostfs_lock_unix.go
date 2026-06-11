//go:build unix

package wasishim

import (
	"errors"
	"os"
	"syscall"
)

// hostLockFile takes the database-file lock on an already-open file:
// exclusive for a database opened read-write, shared for read-only — DuckDB's
// native WRITE_LOCK/READ_LOCK semantics (local_file_system.cpp). The lock has
// two layers:
//
//  1. the process-global path registry (hostfs_lockreg.go) refuses a
//     conflicting open by another engine instance in the SAME process —
//     deterministically, on every filesystem;
//  2. flock(2) refuses conflicting opens by OTHER processes. flock is used
//     instead of native's fcntl(F_SETLK) because fcntl record locks merge
//     per-process; flock attaches to the open file description and usually
//     conflicts in-process too, but on flock-emulating filesystems
//     (NFS/SMB/FUSE map it onto fcntl) that in-process guarantee evaporates —
//     which is why layer 1 exists (duckdb-go-pure#5's repro is two sql.DB
//     handles in one process).
//
// Released via hostUnlockFile on close (the flock half also dies with the fd).
// Returns 0 on success or one of the hosteLock* sentinels.
func hostLockFile(f *os.File, exclusive bool) int32 {
	if code := hostLockRegister(f, exclusive); code != 0 {
		return code
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
	if err == nil {
		return 0
	}
	if errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) ||
		errors.Is(err, syscall.ENOSYS) {
		// Native parity: file systems without lock support are tolerated for
		// read locks (read-only is safe anyway) and refused for write locks.
		// The registry entry is KEPT on the tolerated path, so the in-process
		// guarantee survives even where the OS lock cannot.
		if exclusive {
			hostLockUnregister(f)
			return hosteLockNotSup
		}
		return 0
	}
	hostLockUnregister(f)
	// EWOULDBLOCK/EAGAIN (or anything else): a conflicting lock is held by
	// another process (in-process conflicts were caught by the registry).
	if exclusive {
		// Native appends a "you could open read-only instead" hint when a read
		// lock would succeed; probe with LOCK_SH on the same fd (replaces the
		// failed request; released below and again at f.Close on failure).
		if syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) == nil {
			syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			return hosteLockRdOK
		}
		return hosteLockConflict
	}
	return hosteLockConflict
}

// hostUnlockFile releases the registry entry; the flock half dies with the fd
// (callers close f right after).
func hostUnlockFile(f *os.File) { hostLockUnregister(f) }
