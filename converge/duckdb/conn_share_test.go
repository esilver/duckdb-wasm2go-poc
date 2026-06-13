package duckdb

import (
	"context"
	"database/sql"
	"testing"
)

// TestCrossConnSharing is the gating acceptance test for cross-connection
// sharing:
// every connection from one *sql.DB must share ONE underlying in-memory database,
// so DDL on connection A is visible to a query on connection B. (The googlesqlite
// emulator hard-assumes this and deliberately does not pin MaxOpenConns(1).)
func TestCrossConnSharing(t *testing.T) {
	db, err := sql.Open("duckdb", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(4) // force distinct pooled driver conns

	ctx := context.Background()
	ca, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ca.Close()
	cb, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()

	// Separate statements: the prepared-statement path can't prepare multiple at once.
	if _, err := ca.ExecContext(ctx, "CREATE TABLE t(x INT)"); err != nil {
		t.Fatalf("conn A CREATE: %v", err)
	}
	if _, err := ca.ExecContext(ctx, "INSERT INTO t VALUES (10),(20),(12)"); err != nil {
		t.Fatalf("conn A INSERT: %v", err)
	}
	var sum int
	if err := cb.QueryRowContext(ctx, "SELECT sum(x) FROM t").Scan(&sum); err != nil {
		t.Fatalf("conn B could not see conn A's table (sharing broken): %v", err)
	}
	if sum != 42 {
		t.Fatalf("cross-conn sum=%d, want 42", sum)
	}
	t.Logf("cross-conn DDL visible: A created t, B read sum=%d ✓", sum)
}

// TestSeparateDBsIsolated: two independent sql.Open(":memory:") must NOT share
// state (each gets its own connector -> its own engine).
func TestSeparateDBsIsolated(t *testing.T) {
	d1, err := sql.Open("duckdb", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d1.Close()
	d2, err := sql.Open("duckdb", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()

	if _, err := d1.Exec("CREATE TABLE a(x INT)"); err != nil {
		t.Fatalf("d1 create: %v", err)
	}
	if _, err := d2.Exec("SELECT * FROM a"); err == nil {
		t.Fatal("expected separate in-memory DBs to be isolated, but d2 saw d1's table 'a'")
	}
	t.Logf("two sql.Open(:memory:) are isolated ✓")
}
