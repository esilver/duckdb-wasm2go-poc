// appender_test.go — Appender correctness: 10k mixed-type rows verified by count
// and checksum queries, plus the error paths (wrong arity, wrong type, unsupported
// column type, append/flush after Close).
package duckdb

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"
)

// openAppenderTestDB opens a fresh :memory: db plus a pinned *sql.Conn on it.
func openAppenderTestDB(t *testing.T) (*sql.DB, *sql.Conn) {
	t.Helper()
	db, err := sql.Open("duckdb", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	c, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return db, c
}

func TestAppender10kMixedRows(t *testing.T) {
	db, c := openAppenderTestDB(t)
	if _, err := db.Exec(`CREATE TABLE app_t (
		id BIGINT, grp INTEGER, val DOUBLE, name VARCHAR, ok BOOLEAN, ts TIMESTAMP)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	a, err := NewAppender(c, "", "app_t")
	if err != nil {
		t.Fatalf("NewAppender: %v", err)
	}

	const n = 10000
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for i := 0; i < n; i++ {
		var name any = fmt.Sprintf("row-%d", i)
		var ts any = base.Add(time.Duration(i) * time.Second)
		if i%100 == 99 { // sprinkle NULLs
			name = nil
			ts = nil
		}
		if err := a.AppendRow(int64(i), int64(i%1000), float64(i)/7.0, name, i%2 == 0, ts); err != nil {
			t.Fatalf("AppendRow(%d): %v", i, err)
		}
	}
	if err := a.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Count.
	var got int64
	if err := db.QueryRow(`SELECT count(*) FROM app_t`).Scan(&got); err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != n {
		t.Fatalf("count = %d, want %d", got, n)
	}

	// Checksums over every column.
	var sumID, sumGrp, nNames, nTrue, nTS int64
	var sumVal float64
	row := db.QueryRow(`SELECT sum(id), sum(grp), sum(val),
		count(name), count(CASE WHEN ok THEN 1 END), count(ts) FROM app_t`)
	if err := row.Scan(&sumID, &sumGrp, &sumVal, &nNames, &nTrue, &nTS); err != nil {
		t.Fatalf("checksum scan: %v", err)
	}
	var wantID, wantGrp, wantNames, wantTrue int64
	var wantVal float64
	for i := 0; i < n; i++ {
		wantID += int64(i)
		wantGrp += int64(i % 1000)
		wantVal += float64(i) / 7.0
		if i%100 != 99 {
			wantNames++
		}
		if i%2 == 0 {
			wantTrue++
		}
	}
	if sumID != wantID || sumGrp != wantGrp || nNames != wantNames || nTrue != wantTrue || nTS != wantNames {
		t.Fatalf("checksums: got id=%d grp=%d names=%d true=%d ts=%d, want id=%d grp=%d names=%d true=%d ts=%d",
			sumID, sumGrp, nNames, nTrue, nTS, wantID, wantGrp, wantNames, wantTrue, wantNames)
	}
	if diff := sumVal - wantVal; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("sum(val) = %v, want %v", sumVal, wantVal)
	}

	// Spot-check one row's values round-tripped exactly.
	var name string
	var ts time.Time
	if err := db.QueryRow(`SELECT name, ts FROM app_t WHERE id = 42`).Scan(&name, &ts); err != nil {
		t.Fatalf("spot row: %v", err)
	}
	if name != "row-42" || !ts.Equal(base.Add(42*time.Second)) {
		t.Fatalf("row 42 = (%q, %v), want (row-42, %v)", name, ts, base.Add(42*time.Second))
	}

	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestAppenderMultipleFlushes verifies the feed function re-scans correctly
// across flushes (init rewinds the cursor; the buffer clears each time).
func TestAppenderMultipleFlushes(t *testing.T) {
	db, c := openAppenderTestDB(t)
	if _, err := db.Exec(`CREATE TABLE app_m (id BIGINT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	a, err := NewAppender(c, "", "app_m")
	if err != nil {
		t.Fatalf("NewAppender: %v", err)
	}
	total := 0
	for batch := 0; batch < 3; batch++ {
		for i := 0; i < 2500; i++ { // crosses the 2048 chunk boundary
			if err := a.AppendRow(int64(total)); err != nil {
				t.Fatalf("AppendRow: %v", err)
			}
			total++
		}
		if err := a.Flush(); err != nil {
			t.Fatalf("Flush %d: %v", batch, err)
		}
	}
	if err := a.Flush(); err != nil { // empty flush is a no-op
		t.Fatalf("empty Flush: %v", err)
	}
	var got, sum int64
	if err := db.QueryRow(`SELECT count(*), sum(id) FROM app_m`).Scan(&got, &sum); err != nil {
		t.Fatalf("count: %v", err)
	}
	want := int64(total)
	if got != want || sum != want*(want-1)/2 {
		t.Fatalf("count,sum = %d,%d want %d,%d", got, sum, want, want*(want-1)/2)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestAppenderSchemaQualified(t *testing.T) {
	db, c := openAppenderTestDB(t)
	if _, err := db.Exec(`CREATE SCHEMA s2`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE s2.tt (v VARCHAR)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	a, err := NewAppender(c, "s2", "tt")
	if err != nil {
		t.Fatalf("NewAppender: %v", err)
	}
	if err := a.AppendRow("hello"); err != nil {
		t.Fatalf("AppendRow: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var v string
	if err := db.QueryRow(`SELECT v FROM s2.tt`).Scan(&v); err != nil || v != "hello" {
		t.Fatalf("got (%q, %v), want hello", v, err)
	}
}

func TestAppenderErrorPaths(t *testing.T) {
	db, c := openAppenderTestDB(t)
	if _, err := db.Exec(`CREATE TABLE app_e (id BIGINT, name VARCHAR)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	// Missing table.
	if _, err := NewAppender(c, "", "no_such_table"); err == nil {
		t.Fatalf("NewAppender(no_such_table) succeeded, want error")
	}

	// Unsupported column type (LIST is outside the appendable set).
	if _, err := db.Exec(`CREATE TABLE app_l (xs BIGINT[])`); err != nil {
		t.Fatalf("create list table: %v", err)
	}
	if _, err := NewAppender(c, "", "app_l"); err == nil {
		t.Fatalf("NewAppender(app_l) succeeded, want unsupported-type error")
	}

	a, err := NewAppender(c, "", "app_e")
	if err != nil {
		t.Fatalf("NewAppender: %v", err)
	}

	// Wrong arity.
	if err := a.AppendRow(int64(1)); err == nil {
		t.Fatalf("AppendRow with 1 of 2 values succeeded, want arity error")
	}
	if err := a.AppendRow(int64(1), "x", "extra"); err == nil {
		t.Fatalf("AppendRow with 3 of 2 values succeeded, want arity error")
	}

	// Wrong type: a Go int does not coerce to VARCHAR.
	if err := a.AppendRow(int64(1), 12345); err == nil {
		t.Fatalf("AppendRow(int into VARCHAR) succeeded, want type error")
	}

	// Failed AppendRows must not have buffered anything: a good row still works.
	if err := a.AppendRow(int64(7), "seven"); err != nil {
		t.Fatalf("good AppendRow: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var n int64
	if err := db.QueryRow(`SELECT count(*) FROM app_e`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("count = %d (%v), want 1", n, err)
	}

	// After Close.
	if err := a.AppendRow(int64(8), "eight"); err == nil {
		t.Fatalf("AppendRow after Close succeeded, want error")
	}
	if err := a.Flush(); err == nil {
		t.Fatalf("Flush after Close succeeded, want error")
	}
	if err := a.Close(); err != nil { // idempotent
		t.Fatalf("second Close: %v", err)
	}
}
