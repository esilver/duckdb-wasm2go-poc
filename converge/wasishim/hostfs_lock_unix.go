//go:build unix

package wasishim

import (
	"errors"
	"os"
	"syscall"
)

// hostLockFile takes a real OS lock on an already-open file: exclusive for a
// database opened read-write, shared for read-only — DuckDB's native
// WRITE_LOCK/READ_LOCK semantics (local_file_system.cpp). flock(2) is used
// instead of native's fcntl(F_SETLK) deliberately: flock locks attach to the
// open file description, so a SECOND engine instance in the same process
// conflicts too (POSIX record locks merge per-process and would let two
// in-process engines double-write the file — duckdb-go-pure#5's repro is two
// sql.DB handles in one process). Cross-process behavior matches native: the
// second opener fails. The lock is released when the fd closes.
//
// Returns 0 on success or one of the hosteLock* sentinels.
func hostLockFile(f *os.File, exclusive bool) int32 {
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
		if exclusive {
			return hosteLockNotSup
		}
		return 0
	}
	// EWOULDBLOCK/EAGAIN (or anything else): a conflicting lock is held.
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

// hostUnlockFile: flock locks die with the fd; nothing to do on unix. (The
// !unix build keeps a path registry that needs explicit release on close.)
func hostUnlockFile(*os.File) {}
