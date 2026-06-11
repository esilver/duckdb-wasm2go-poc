package wasishim

import (
	"os"
	"path/filepath"
	"sync"
)

// In-process database-file lock registry (duckdb-go-pure#5, in-process shape).
//
// The unix build takes a real flock(2) for cross-process protection, and flock
// SHOULD also conflict between two open file descriptions inside one process —
// but that guarantee is filesystem-dependent: on filesystems that emulate
// flock with POSIX record locks (NFS, SMB, some FUSE), locks MERGE per process,
// so a second in-process engine instance acquires the "conflicting" lock
// silently and two buffer managers double-write one database file. This
// registry is the environment-independent half of the lock: a process-global
// map keyed by canonical path, consulted before (unix) or instead of (!unix,
// GOOS=js) the OS lock. Same-process conflicts are refused here with the same
// sentinel codes the flock path uses, so the engine reports native DuckDB's
// exact "Could not set lock on file" error shape either way.
var (
	hostLockMu    sync.Mutex
	hostLockTable = map[string]int{} // canonical path -> -1 exclusive, >0 reader count
	hostLockPath  = map[*os.File]string{}
	hostLockMode  = map[*os.File]bool{} // true = exclusive
)

// hostLockKey canonicalizes the file's path so aliases (symlinks, relative
// paths, /tmp vs /private/tmp) of one database file share a registry slot.
// The file exists by the time this runs (hostLockFile is called on an open
// handle), so EvalSymlinks normally succeeds; fall back to Abs, then Clean.
func hostLockKey(f *os.File) string {
	name := f.Name()
	if r, err := filepath.EvalSymlinks(name); err == nil {
		name = r
	}
	if a, err := filepath.Abs(name); err == nil {
		return a
	}
	return filepath.Clean(name)
}

// hostLockRegister records a live locked handle for f's canonical path, or
// refuses: a write lock is refused while ANY holder is live (hosteLockRdOK if
// the holders are all readers — native's "you could open read-only" hint —
// else hosteLockConflict), a read lock is refused only while a writer is live.
// Returns 0 on success; the entry must be released via hostLockUnregister.
func hostLockRegister(f *os.File, exclusive bool) int32 {
	key := hostLockKey(f)
	hostLockMu.Lock()
	defer hostLockMu.Unlock()
	n := hostLockTable[key]
	if exclusive {
		if n != 0 {
			if n > 0 {
				return hosteLockRdOK // readers hold it; read-only would work
			}
			return hosteLockConflict
		}
		hostLockTable[key] = -1
	} else {
		if n < 0 {
			return hosteLockConflict
		}
		hostLockTable[key] = n + 1
	}
	hostLockPath[f] = key
	hostLockMode[f] = exclusive
	return 0
}

// hostLockUnregister releases a registry entry taken by hostLockRegister
// (no-op for files that never registered, e.g. opened without lock flags).
func hostLockUnregister(f *os.File) {
	hostLockMu.Lock()
	defer hostLockMu.Unlock()
	key, ok := hostLockPath[f]
	if !ok {
		return
	}
	delete(hostLockPath, f)
	exclusive := hostLockMode[f]
	delete(hostLockMode, f)
	if exclusive {
		delete(hostLockTable, key)
		return
	}
	if n := hostLockTable[key]; n > 1 {
		hostLockTable[key] = n - 1
	} else {
		delete(hostLockTable, key)
	}
}
