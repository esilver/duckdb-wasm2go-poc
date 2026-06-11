//go:build !unix

package wasishim

import (
	"os"
	"path/filepath"
	"sync"
)

// Non-unix fallback (GOOS=js browser builds, windows): no flock(2) available,
// so enforce the database-file lock with a process-global registry keyed by
// cleaned absolute path. This still delivers the duckdb-go-pure#5 guarantee —
// a second engine instance in the SAME process fails the open — but offers no
// cross-process protection (matching the pre-fix status quo there).
var (
	hostLockMu    sync.Mutex
	hostLockTable = map[string]int{} // path -> -1 exclusive, >0 shared count
	hostLockPath  = map[*os.File]string{}
	hostLockMode  = map[*os.File]bool{} // true = exclusive
)

func hostLockKey(f *os.File) string {
	p, err := filepath.Abs(f.Name())
	if err != nil {
		return filepath.Clean(f.Name())
	}
	return filepath.Clean(p)
}

func hostLockFile(f *os.File, exclusive bool) int32 {
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

// hostUnlockFile releases a registry lock taken by hostLockFile. Called from
// Xhost_close before the file is closed (no-op for files opened without lock).
func hostUnlockFile(f *os.File) {
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
