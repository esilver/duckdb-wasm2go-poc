package duckdb

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

// runawaySQL is a single statement that runs for many minutes if uninterrupted:
// a streaming COUNT over a ~10^13-row cross join (no memory growth, pure CPU).
const runawaySQL = "SELECT count(*) FROM range(10000000) a, range(1000000) b"

// interruptBudget is the wall-clock budget for a 150ms-timeout runaway query
// to return (issue #6 regression bound: < 2s).
const interruptBudget = 2 * time.Second

// wantInterrupted asserts the error of a cancelled statement: it must satisfy
// errors.Is on the context error (what watchdogs select on) AND carry the
// engine's own INTERRUPT text.
func wantInterrupted(t *testing.T, err, ctxWant error) {
	t.Helper()
	if err == nil {
		t.Fatal("cancelled runaway statement returned nil error")
	}
	if !errors.Is(err, ctxWant) {
		t.Errorf("errors.Is(err, %v) = false; err = %v", ctxWant, err)
	}
	if !strings.Contains(err.Error(), "INTERRUPT") {
		t.Errorf("error does not carry the engine INTERRUPT text: %v", err)
	}
}

// TestContextCancelInterruptsRunawayQuery is the issue #6 regression: a single
// runaway statement + a 150ms context timeout must return promptly (< 2s, not
// at the statement boundary minutes later) with an interrupt error, and the
// engine must stay healthy for the next query on the same connection.
func TestContextCancelInterruptsRunawayQuery(t *testing.T) {
	_, c := openSingleConn(t, ":memory:")

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	rows, err := c.QueryContext(ctx, runawaySQL)
	elapsed := time.Since(start)
	if err == nil {
		rows.Close()
	}
	wantInterrupted(t, err, context.DeadlineExceeded)
	if elapsed > interruptBudget {
		t.Fatalf("interrupt took %v (budget %v)", elapsed, interruptBudget)
	}

	// Engine healthy afterwards: same connection answers immediately.
	var v int
	if err := c.QueryRowContext(context.Background(), "SELECT 42").Scan(&v); err != nil || v != 42 {
		t.Fatalf("post-interrupt query: v=%d err=%v", v, err)
	}
}

// TestContextCancelInterruptsExec covers the Exec path (and explicit cancel
// rather than deadline): a runaway CTAS is interrupted promptly and nothing
// of it persists.
func TestContextCancelInterruptsExec(t *testing.T) {
	_, c := openSingleConn(t, ":memory:")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := c.ExecContext(ctx, "CREATE TABLE runaway AS "+runawaySQL)
	elapsed := time.Since(start)
	wantInterrupted(t, err, context.Canceled)
	if elapsed > interruptBudget {
		t.Fatalf("interrupt took %v (budget %v)", elapsed, interruptBudget)
	}

	var n int
	err = c.QueryRowContext(context.Background(),
		"SELECT count(*) FROM information_schema.tables WHERE table_name='runaway'").Scan(&n)
	if err != nil || n != 0 {
		t.Fatalf("interrupted CTAS left state: n=%d err=%v", n, err)
	}
}

// TestContextCancelInterruptsPrepared covers the prepared-statement path.
func TestContextCancelInterruptsPrepared(t *testing.T) {
	_, c := openSingleConn(t, ":memory:")

	// The parameter bounds a (pushed-down) filter on the first range: the full
	// bound is the runaway cross join, a tiny bound re-executes in milliseconds.
	st, err := c.PrepareContext(context.Background(),
		"SELECT count(*) FROM range(10000000) a, range(1000000) b WHERE a.range < ?")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	rows, err := st.QueryContext(ctx, int64(10000000))
	elapsed := time.Since(start)
	if err == nil {
		rows.Close()
	}
	wantInterrupted(t, err, context.DeadlineExceeded)
	if elapsed > interruptBudget {
		t.Fatalf("interrupt took %v (budget %v)", elapsed, interruptBudget)
	}

	// The statement itself stays usable for a small re-execution.
	var n int64
	rs, err := st.QueryContext(context.Background(), int64(2))
	if err != nil {
		t.Fatalf("post-interrupt reuse of prepared stmt: %v", err)
	}
	defer rs.Close()
	if !rs.Next() {
		t.Fatal("post-interrupt reuse: no row")
	}
	if err := rs.Scan(&n); err != nil || n != 2000000 {
		t.Fatalf("post-interrupt reuse: n=%d err=%v", n, err)
	}
}

// TestInterruptDoesNotPoisonSiblingConn: on a SHARED engine (pooled connector),
// interrupting one connection's statement must not break a sibling connection
// queued behind it on the engine mutex.
func TestInterruptDoesNotPoisonSiblingConn(t *testing.T) {
	db, err := sql.Open("duckdb", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(2)

	bg := context.Background()
	c1, err := db.Conn(bg)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	c2, err := db.Conn(bg)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()

	ctx, cancel := context.WithTimeout(bg, 150*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c1.ExecContext(ctx, runawaySQL)
		done <- err
	}()

	// c2 queues on the shared engine mutex behind the runaway; once c1 is
	// interrupted, c2's statement must run cleanly.
	time.Sleep(50 * time.Millisecond)
	var v int
	start := time.Now()
	if err := c2.QueryRowContext(bg, "SELECT 7").Scan(&v); err != nil || v != 7 {
		t.Fatalf("sibling conn after interrupt: v=%d err=%v", v, err)
	}
	if elapsed := time.Since(start); elapsed > interruptBudget {
		t.Fatalf("sibling conn waited %v (budget %v)", elapsed, interruptBudget)
	}
	wantInterrupted(t, <-done, context.DeadlineExceeded)
}

// TestPreCancelledContextShortCircuits: an already-cancelled ctx never reaches
// the engine (and returns the bare context error, no engine text).
func TestPreCancelledContextShortCircuits(t *testing.T) {
	_, c := openSingleConn(t, ":memory:")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.ExecContext(ctx, "SELECT 1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled Exec: %v", err)
	}
	var v int
	if err := c.QueryRowContext(context.Background(), "SELECT 1").Scan(&v); err != nil {
		t.Fatalf("conn unusable after pre-cancelled call: %v", err)
	}
}
