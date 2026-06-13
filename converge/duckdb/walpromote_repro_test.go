package duckdb

import (
	"context"
	"path/filepath"
	"testing"
)

// TestWALPromoteVersionRepro reproduces test/sql/storage/wal/wal_promote_version.test:
// attach a file db, create a table (kept in the WAL — checkpoint-on-shutdown
// disabled), detach, then re-attach with STORAGE_VERSION 'latest' twice. The
// table must survive every replay/append cycle.
//
// ROOT CAUSE (2026-06-10, checkpoint lane): host_fs.cpp OpenFile dropped
// FILE_FLAGS_APPEND. Native maps that flag to O_APPEND, and the WAL's
// BufferedFileWriter relies on it (position-form FileSystem::Write). The old
// HostFileHandle started position=0, so re-attaching an existing WAL and
// appending OVERWROTE the WAL head in place (size stayed constant, the CREATE
// TABLE entry was destroyed, and the next replay found no table).
// Byte-level proof: after cycle1 the WAL is still 275 bytes but its first
// ~108 bytes are the cycle1 use_table/insert/flush entries; the tail is the
// old cycle0 content. Native grows 275 -> 383.
//
// The source fix is C++ (host_fs.cpp -> wasm -> transpile): initialize the
// handle position to the file size when flags.OpenForAppending(). Keep this
// regression always-on wherever a generated engine is present.
func TestWALPromoteVersionRepro(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "wal_promote.db")

	_, c := openSingleConn(t, ":memory:")
	mustExec := func(stage, q string) {
		t.Helper()
		if _, err := c.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %q failed: %v", stage, q, err)
		}
	}
	walSize := func() int64 {
		fi, err := os.Stat(dbPath + ".wal")
		if err != nil {
			return -1
		}
		return fi.Size()
	}

	mustExec("setup", "PRAGMA disable_checkpoint_on_shutdown")
	mustExec("setup", "SET checkpoint_threshold='1TB'")

	mustExec("cycle0", "ATTACH '"+dbPath+"'")
	mustExec("cycle0", "CREATE TABLE wal_promote.T AS (FROM range(10))")
	mustExec("cycle0", "DETACH wal_promote")
	size0 := walSize()

	mustExec("cycle1", "ATTACH '"+dbPath+"' (STORAGE_VERSION 'latest')")
	var n int
	if err := c.QueryRowContext(ctx, "SELECT count(*) FROM wal_promote.T").Scan(&n); err != nil {
		t.Fatalf("cycle1 count: %v", err)
	}
	if n != 10 {
		t.Fatalf("cycle1: expected 10 rows, got %d", n)
	}
	mustExec("cycle1", "INSERT INTO wal_promote.T VALUES (42)")
	mustExec("cycle1", "DETACH wal_promote")
	if size1 := walSize(); size1 <= size0 {
		t.Errorf("cycle1 WAL did not grow: %d -> %d bytes (append overwrote the WAL head)", size0, size1)
	}

	mustExec("cycle2", "ATTACH '"+dbPath+"' (STORAGE_VERSION 'latest')")
	if err := c.QueryRowContext(ctx, "SELECT count(*) FROM wal_promote.T").Scan(&n); err != nil {
		t.Fatalf("cycle2 count: %v", err)
	}
	if n != 11 {
		t.Fatalf("cycle2: expected 11 rows, got %d", n)
	}
	mustExec("cycle2", "INSERT INTO wal_promote.T VALUES (42)")
	mustExec("cycle2", "DETACH wal_promote")
}
