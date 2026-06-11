//go:build !unix

package wasishim

import "os"

// Non-unix fallback (GOOS=js browser builds, windows): no flock(2) available,
// so the database-file lock is the process-global path registry alone
// (hostfs_lockreg.go). This still delivers the duckdb-go-pure#5 guarantee —
// a second engine instance in the SAME process fails the open — but offers no
// cross-process protection (matching the pre-fix status quo there).
func hostLockFile(f *os.File, exclusive bool) int32 {
	return hostLockRegister(f, exclusive)
}

// hostUnlockFile releases a registry lock taken by hostLockFile. Called from
// Xhost_close before the file is closed (no-op for files opened without lock).
func hostUnlockFile(f *os.File) { hostLockUnregister(f) }
