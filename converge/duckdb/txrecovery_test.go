package duckdb

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"testing"
)

// openSingleConn opens a fresh in-memory DB and pins ONE *sql.Conn so every
// statement in a test hits the same duckdb_connection (the dangling-transaction
// bug is per-connection; a pool hop would mask it).
func openSingleConn(t *testing.T, dsn string) (*sql.DB, *sql.Conn) {
	t.Helper()
	db, err := sql.Open("duckdb", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	c, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return db, c
}

// TestTxNotPoisonedAfterFailure covers the engine's transaction leak: any
// failed statement used to leave the connection's autocommit transaction open,
// so the NEXT statement failed with "cannot start a transaction within a
// transaction" (DuckDB's own corpus needed 5,193 sacrificial ROLLBACKs; the
// googlesqlite emulator carries a downstream workaround). The driver now
// restores autocommit on the failing connection itself.
func TestTxNotPoisonedAfterFailure(t *testing.T) {
	ctx := context.Background()

	t.Run("prepare-stage failure then success", func(t *testing.T) {
		_, c := openSingleConn(t, ":memory:")
		if _, err := c.ExecContext(ctx, "CREATE TABLE t1(i INT)"); err != nil {
			t.Fatalf("CREATE t1: %v", err)
		}
		// Binder error: caught at PREPARE time.
		if _, err := c.ExecContext(ctx, "SELECT nonexist FROM t1"); err == nil {
			t.Fatal("expected binder error, got success")
		}
		if _, err := c.ExecContext(ctx, "CREATE TABLE t2(j INT)"); err != nil {
			t.Fatalf("connection poisoned after prepare-stage failure: %v", err)
		}
	})

	t.Run("execute-stage failure then success", func(t *testing.T) {
		_, c := openSingleConn(t, ":memory:")
		if _, err := c.ExecContext(ctx, "CREATE TABLE t1(i INT)"); err != nil {
			t.Fatalf("CREATE t1: %v", err)
		}
		// Conversion error: prepares fine, fails during execution.
		if _, err := c.ExecContext(ctx, "INSERT INTO t1 SELECT CAST('abc' AS INT)"); err == nil {
			t.Fatal("expected execute-stage cast error, got success")
		}
		if _, err := c.ExecContext(ctx, "CREATE TABLE t2(j INT)"); err != nil {
			t.Fatalf("connection poisoned after execute-stage failure: %v", err)
		}
	})

	t.Run("explicit BEGIN failed stmt ROLLBACK still works", func(t *testing.T) {
		db, c := openSingleConn(t, ":memory:")
		if _, err := c.ExecContext(ctx, "CREATE TABLE t1(i INT)"); err != nil {
			t.Fatalf("CREATE t1: %v", err)
		}
		tx, err := c.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BEGIN: %v", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO t1 VALUES (1)"); err != nil {
			t.Fatalf("INSERT in tx: %v", err)
		}
		if _, err := tx.ExecContext(ctx, "SELECT nonexist FROM t1"); err == nil {
			t.Fatal("expected failure inside explicit tx")
		}
		// The driver must NOT have auto-rolled-back the user's transaction:
		// the user's own ROLLBACK owns cleanup and must still succeed.
		if err := tx.Rollback(); err != nil {
			t.Fatalf("explicit ROLLBACK after in-tx failure: %v", err)
		}
		var n int
		if err := c.QueryRowContext(ctx, "SELECT count(*) FROM t1").Scan(&n); err != nil {
			t.Fatalf("count after rollback: %v", err)
		}
		if n != 0 {
			t.Fatalf("rollback did not undo insert: count=%d, want 0", n)
		}
		// And the connection works normally afterwards.
		if _, err := c.ExecContext(ctx, "CREATE TABLE t2(j INT)"); err != nil {
			t.Fatalf("connection poisoned after explicit tx: %v", err)
		}
		_ = db
	})

	t.Run("explicit BEGIN via raw SQL then failed stmt then ROLLBACK", func(t *testing.T) {
		// The sqllogictest runner (and similar callers) drive transactions with
		// plain Exec("BEGIN TRANSACTION") rather than BeginTx; the driver's
		// keyword tracking must respect those too.
		_, c := openSingleConn(t, ":memory:")
		if _, err := c.ExecContext(ctx, "CREATE TABLE t1(i INT)"); err != nil {
			t.Fatalf("CREATE t1: %v", err)
		}
		if _, err := c.ExecContext(ctx, "BEGIN TRANSACTION"); err != nil {
			t.Fatalf("BEGIN: %v", err)
		}
		if _, err := c.ExecContext(ctx, "INSERT INTO t1 VALUES (1)"); err != nil {
			t.Fatalf("INSERT in tx: %v", err)
		}
		if _, err := c.ExecContext(ctx, "SELECT nonexist FROM t1"); err == nil {
			t.Fatal("expected failure inside explicit tx")
		}
		if _, err := c.ExecContext(ctx, "ROLLBACK"); err != nil {
			t.Fatalf("explicit ROLLBACK after in-tx failure: %v", err)
		}
		var n int
		if err := c.QueryRowContext(ctx, "SELECT count(*) FROM t1").Scan(&n); err != nil {
			t.Fatalf("count after rollback: %v", err)
		}
		if n != 0 {
			t.Fatalf("rollback did not undo insert: count=%d, want 0", n)
		}
	})

	t.Run("googlesqlite pattern failing stmt then DDL", func(t *testing.T) {
		_, c := openSingleConn(t, ":memory:")
		if _, err := c.ExecContext(ctx, "CREATE TABLE kv(k VARCHAR PRIMARY KEY, v VARCHAR)"); err != nil {
			t.Fatalf("CREATE kv: %v", err)
		}
		if _, err := c.ExecContext(ctx, "INSERT INTO kv VALUES ('a','1')"); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
		// Duplicate key: execute-stage constraint failure.
		if _, err := c.ExecContext(ctx, "INSERT INTO kv VALUES ('a','2')"); err == nil {
			t.Fatal("expected primary-key violation")
		}
		// DDL immediately after the failure (the pattern googlesqlite works
		// around with a sacrificial ROLLBACK).
		if _, err := c.ExecContext(ctx, "CREATE INDEX kv_v ON kv(v)"); err != nil {
			t.Fatalf("DDL after failed stmt: %v", err)
		}
		if _, err := c.ExecContext(ctx, "CREATE TABLE other(x INT)"); err != nil {
			t.Fatalf("second DDL after failed stmt: %v", err)
		}
		// Repeated failures must each be absorbed, not just the first.
		for i := 0; i < 3; i++ {
			if _, err := c.ExecContext(ctx, "SELECT nope FROM kv"); err == nil {
				t.Fatal("expected binder error")
			}
		}
		var n int
		if err := c.QueryRowContext(ctx, "SELECT count(*) FROM kv").Scan(&n); err != nil {
			t.Fatalf("query after repeated failures: %v", err)
		}
		if n != 1 {
			t.Fatalf("count=%d, want 1", n)
		}
	})
}

// TestMultiStatementExec covers the duckdb_query fallback: duckdb_prepare
// rejects multi-statement text ("Cannot prepare multiple statements at
// once!"), so argument-less Exec falls back to direct duckdb_query, which runs
// every statement.
func TestMultiStatementExec(t *testing.T) {
	ctx := context.Background()
	_, c := openSingleConn(t, ":memory:")

	if _, err := c.ExecContext(ctx,
		"CREATE TABLE a(i INT); INSERT INTO a VALUES (1); CREATE TABLE b(j INT);"); err != nil {
		t.Fatalf("multi-statement Exec: %v", err)
	}
	var n int
	if err := c.QueryRowContext(ctx, "SELECT count(*) FROM a").Scan(&n); err != nil {
		t.Fatalf("query a: %v", err)
	}
	if n != 1 {
		t.Fatalf("a count=%d, want 1", n)
	}
	if err := c.QueryRowContext(ctx, "SELECT count(*) FROM b").Scan(&n); err != nil {
		t.Fatalf("query b: %v", err)
	}
	if n != 0 {
		t.Fatalf("b count=%d, want 0", n)
	}

	// A failing statement inside the batch surfaces the error AND must not
	// poison the connection (tx recovery applies to the fallback path too).
	if _, err := c.ExecContext(ctx,
		"INSERT INTO a VALUES (2); SELECT nonexist FROM a; INSERT INTO a VALUES (3);"); err == nil {
		t.Fatal("expected error from failing statement in batch")
	}
	if _, err := c.ExecContext(ctx, "CREATE TABLE c(x INT)"); err != nil {
		t.Fatalf("connection poisoned after failed batch: %v", err)
	}

	// Bind args + multi-statement text must still fail (no fallback).
	if _, err := c.ExecContext(ctx, "INSERT INTO a VALUES (?); INSERT INTO a VALUES (?);", 4, 5); err == nil {
		t.Fatal("expected multi-statement error with bind args")
	}
}

func TestForEachStatementSkipsEscapedQuotes(t *testing.T) {
	query := "BEGIN; SELECT 'it'';s open'; SELECT \"a\"\";b\"; ROLLBACK"
	var got []string
	forEachStatement(query, func(stmt string) {
		trimmed := strings.TrimSpace(stmt)
		if trimmed != "" {
			got = append(got, trimmed)
		}
	})
	want := []string{
		"BEGIN",
		"SELECT 'it'';s open'",
		"SELECT \"a\"\";b\"",
		"ROLLBACK",
	}
	if len(got) != len(want) {
		t.Fatalf("statement count = %d (%v); want %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("statement[%d] = %q; want %q", i, got[i], want[i])
		}
	}
}

// TestMemoryLimitDefault checks fix 3: the wasm engine self-detects only
// ~17.5MB; module.open now applies a 1GB default (DSN-overridable via
// ?max_memory=...).
func TestMemoryLimitDefault(t *testing.T) {
	db, err := sql.Open("duckdb", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var limit string
	if err := db.QueryRow("SELECT current_setting('memory_limit')").Scan(&limit); err != nil {
		t.Fatalf("current_setting: %v", err)
	}
	bytes := parseMemSize(t, limit)
	// Default is 1GiB; the human-readable rendering rounds, so accept >= 0.99 GiB.
	if bytes < (1<<30)*99/100 {
		t.Fatalf("memory_limit=%q (%d bytes), want >= 1GiB", limit, bytes)
	}
	t.Logf("default memory_limit=%s", limit)
}

// TestMemoryLimitDSNOverride checks the ?max_memory= DSN query parameter.
func TestMemoryLimitDSNOverride(t *testing.T) {
	db, err := sql.Open("duckdb", ":memory:?max_memory=256MB")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var limit string
	if err := db.QueryRow("SELECT current_setting('memory_limit')").Scan(&limit); err != nil {
		t.Fatalf("current_setting: %v", err)
	}
	bytes := parseMemSize(t, limit)
	if bytes < 200<<20 || bytes > 300<<20 {
		t.Fatalf("memory_limit=%q (%d bytes), want ~256MB", limit, bytes)
	}
}

// parseMemSize parses DuckDB's human-readable memory size ("953.6 MiB",
// "1.0 GiB", "476.8MiB") into bytes.
func parseMemSize(t *testing.T, s string) int64 {
	t.Helper()
	s = strings.TrimSpace(s)
	units := []struct {
		suffix string
		mult   float64
	}{
		{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40},
		{"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"TB", 1e12},
		{"B", 1},
	}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			num := strings.TrimSpace(strings.TrimSuffix(s, u.suffix))
			f, err := strconv.ParseFloat(num, 64)
			if err != nil {
				t.Fatalf("cannot parse memory size %q: %v", s, err)
			}
			return int64(f * u.mult)
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("cannot parse memory size %q: %v", s, err)
	}
	return int64(f)
}
