package duckdb

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the database-file lock semantics (duckdb-go-pure issue #5).
// Native DuckDB's storage layer takes an OS file lock at open (fcntl F_SETLK in
// local_file_system.cpp): a second read-write open of the same database file by
// another engine instance fails with "Could not set lock on file". The wasm
// port used to drop that lock entirely in HostFileSystem — two engines could
// open and double-write one file (two buffer managers + two WAL writers, silent
// corruption). The host enforces the lock in two layers: a process-global
// path registry (wasishim/hostfs_lockreg.go) refuses conflicting opens by
// another engine instance in the SAME process on every filesystem, and a real
// flock(2) refuses other processes. (flock alone is NOT a reliable in-process
// guard: filesystems that emulate it with POSIX record locks — NFS/SMB/FUSE —
// merge locks per process. Native only avoids the same-process case via the
// C-API instance cache, which cannot exist across separate wasm instances, so
// refusing is the only safe parity.)

const lockErrShape = "Could not set lock on file"

// TestFileLockSecondOpenRefused is the issue #5 repro: two sql.DB handles on
// one database file, second writer must fail with the native lock-error shape,
// and must succeed once the first engine closes.
func TestFileLockSecondOpenRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock.db")

	db1, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db1.Exec("CREATE TABLE t (x INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db1.Exec("INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}

	db2, err := sql.Open("duckdb", path) // lazy: the engine opens on first use
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if _, err := db2.Exec("INSERT INTO t VALUES (2)"); err == nil {
		t.Fatal("second engine instance opened and wrote a locked database file")
	} else if !strings.Contains(err.Error(), lockErrShape) {
		t.Fatalf("second open failed with the wrong error shape: %v", err)
	}

	// After the first engine closes, the SAME handle must work (the open error
	// is not latched; database/sql retries Connect).
	if err := db1.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := db2.Exec("INSERT INTO t VALUES (3)"); err != nil {
		t.Fatalf("open after lock release failed: %v", err)
	}
	var n, sum int
	if err := db2.QueryRow("SELECT count(*), sum(x) FROM t").Scan(&n, &sum); err != nil {
		t.Fatal(err)
	}
	// the locked-out write never landed: rows are exactly {1, 3}
	if n != 2 || sum != 4 {
		t.Fatalf("expected rows {1,3}, got count=%d sum=%d", n, sum)
	}
}

// TestFileLockSymlinkAliasRefused: the in-process registry canonicalizes paths
// (EvalSymlinks+Abs), so an alias of a held database file is refused too — and
// usable once the holder closes.
func TestFileLockSymlinkAliasRefused(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.db")
	link := filepath.Join(dir, "alias.db")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink not supported here: %v", err)
	}

	db1, err := sql.Open("duckdb", real)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db1.Exec("CREATE TABLE t (x INTEGER)"); err != nil {
		t.Fatal(err)
	}

	db2, err := sql.Open("duckdb", link)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if _, err := db2.Exec("INSERT INTO t VALUES (2)"); err == nil {
		t.Fatal("symlink alias of a locked database file opened silently")
	} else if !strings.Contains(err.Error(), lockErrShape) {
		t.Fatalf("alias open failed with the wrong error shape: %v", err)
	}

	if err := db1.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := db2.Exec("INSERT INTO t VALUES (3)"); err != nil {
		t.Fatalf("alias open after lock release failed: %v", err)
	}
}

// TestFileLockCrossProcessRefused pins the flock(2) half of the lock: a CHILD
// process opening the held database file must fail with the native error
// shape. (The in-process registry cannot see other processes; this guards the
// OS-lock layer staying wired after registry changes.) Re-execs the test
// binary in child mode via FILELOCK_CHILD_PATH.
func TestFileLockCrossProcessRefused(t *testing.T) {
	if path := os.Getenv("FILELOCK_CHILD_PATH"); path != "" {
		// child mode: report the open outcome on stdout and never fail the run
		db, err := sql.Open("duckdb", path)
		if err == nil {
			_, err = db.Exec("INSERT INTO t VALUES (99)")
			db.Close()
		}
		fmt.Printf("CHILD-RESULT: %v\n", err)
		return
	}

	path := filepath.Join(t.TempDir(), "xp.db")
	db1, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db1.Close()
	if _, err := db1.Exec("CREATE TABLE t (x INTEGER)"); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestFileLockCrossProcessRefused$", "-test.v")
	cmd.Env = append(os.Environ(), "FILELOCK_CHILD_PATH="+path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child run failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "CHILD-RESULT: ") {
		t.Fatalf("child never reported a result:\n%s", out)
	}
	if !strings.Contains(string(out), lockErrShape) {
		t.Fatalf("child process opened the locked database (flock layer dead):\n%s", out)
	}
}

// TestFileLockMemoryUnaffected: :memory: engines take no file lock and never
// conflict with each other.
func TestFileLockMemoryUnaffected(t *testing.T) {
	db1, err := sql.Open("duckdb", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db1.Close()
	db2, err := sql.Open("duckdb", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	for _, db := range []*sql.DB{db1, db2} {
		if _, err := db.Exec("CREATE TABLE t (x INTEGER)"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO t VALUES (7)"); err != nil {
			t.Fatal(err)
		}
	}
}

// TestFileLockReadOnly pins native read-lock semantics (READ_LOCK is shared):
//   - two engines may ATTACH the same file READ_ONLY concurrently;
//   - a read-write open is refused while readers hold the file, and the error
//     carries native's "you could open read-only" hint;
//   - once the readers close, the writer opens cleanly.
func TestFileLockReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ro.db")

	seed, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec("CREATE TABLE t (x INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec("INSERT INTO t VALUES (42)"); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	attach := "ATTACH '" + path + "' AS r (READ_ONLY)"
	r1, err := sql.Open("duckdb", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer r1.Close()
	r2, err := sql.Open("duckdb", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	for _, db := range []*sql.DB{r1, r2} {
		if _, err := db.Exec(attach); err != nil {
			t.Fatalf("concurrent READ_ONLY attach should share the read lock: %v", err)
		}
		var x int
		if err := db.QueryRow("SELECT x FROM r.t").Scan(&x); err != nil || x != 42 {
			t.Fatalf("read through shared lock: x=%d err=%v", x, err)
		}
	}

	// a writer must be refused while the readers hold shared locks
	w, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Exec("INSERT INTO t VALUES (1)"); err == nil {
		t.Fatal("read-write open succeeded while read locks were held")
	} else {
		if !strings.Contains(err.Error(), lockErrShape) {
			t.Fatalf("writer-vs-readers failed with the wrong error shape: %v", err)
		}
		if !strings.Contains(err.Error(), "read-only mode") {
			t.Fatalf("writer-vs-readers error lacks native's read-only hint: %v", err)
		}
	}

	// readers gone -> the writer opens cleanly (same handle, retried open)
	if err := r1.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r2.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Exec("INSERT INTO t VALUES (1)"); err != nil {
		t.Fatalf("write after readers closed: %v", err)
	}
}
