package wasishim

import (
	"os"
	"path/filepath"
	"testing"
)

// These tests pin the in-process database-file lock registry on its own,
// independent of flock(2) — the registry is the only line of defense on
// filesystems that emulate flock with per-process-merging fcntl locks
// (duckdb-go-pure#5, in-process shape) and on !unix builds.

func openLockTestFile(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestLockRegistryExclusiveConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	f1 := openLockTestFile(t, path)
	f2 := openLockTestFile(t, path)

	if code := hostLockRegister(f1, true); code != 0 {
		t.Fatalf("first exclusive register failed: %d", code)
	}
	if code := hostLockRegister(f2, true); code != hosteLockConflict {
		t.Fatalf("second exclusive register: want hosteLockConflict, got %d", code)
	}
	// a read lock is refused too while a writer is live
	if code := hostLockRegister(f2, false); code != hosteLockConflict {
		t.Fatalf("read register vs writer: want hosteLockConflict, got %d", code)
	}
	hostLockUnregister(f1)
	if code := hostLockRegister(f2, true); code != 0 {
		t.Fatalf("register after release failed: %d", code)
	}
	hostLockUnregister(f2)
}

func TestLockRegistryReaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.db")
	f1 := openLockTestFile(t, path)
	f2 := openLockTestFile(t, path)
	f3 := openLockTestFile(t, path)

	// multiple readers share
	if code := hostLockRegister(f1, false); code != 0 {
		t.Fatalf("reader 1: %d", code)
	}
	if code := hostLockRegister(f2, false); code != 0 {
		t.Fatalf("reader 2: %d", code)
	}
	// a writer is refused with the "read-only would work" sentinel
	if code := hostLockRegister(f3, true); code != hosteLockRdOK {
		t.Fatalf("writer vs readers: want hosteLockRdOK, got %d", code)
	}
	// still refused while ONE reader remains
	hostLockUnregister(f1)
	if code := hostLockRegister(f3, true); code != hosteLockRdOK {
		t.Fatalf("writer vs last reader: want hosteLockRdOK, got %d", code)
	}
	hostLockUnregister(f2)
	if code := hostLockRegister(f3, true); code != 0 {
		t.Fatalf("writer after readers gone: %d", code)
	}
	hostLockUnregister(f3)
}

func TestLockRegistrySymlinkAlias(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.db")
	link := filepath.Join(dir, "alias.db")
	f1 := openLockTestFile(t, real)
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink not supported here: %v", err)
	}
	f2 := openLockTestFile(t, link)

	if code := hostLockRegister(f1, true); code != 0 {
		t.Fatalf("register real path: %d", code)
	}
	if code := hostLockRegister(f2, true); code != hosteLockConflict {
		t.Fatalf("register via symlink alias: want hosteLockConflict, got %d", code)
	}
	hostLockUnregister(f1)
	hostLockUnregister(f2) // no-op: f2 never registered
	if code := hostLockRegister(f2, true); code != 0 {
		t.Fatalf("register via alias after release: %d", code)
	}
	hostLockUnregister(f2)
}

func TestLockRegistryUnregisterUnknownNoop(t *testing.T) {
	f := openLockTestFile(t, filepath.Join(t.TempDir(), "n.db"))
	hostLockUnregister(f) // never registered: must not panic or disturb state
	if code := hostLockRegister(f, true); code != 0 {
		t.Fatalf("register after stray unregister: %d", code)
	}
	hostLockUnregister(f)
}
