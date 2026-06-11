package duckdb

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestDeadOnArrivalStatementSkipsEngine is the writer-lock fairness
// regression (googlesqlite #18 follow-up): a statement whose context expires
// WHILE QUEUED behind the connection mutex must return the context error
// without dispatching to the engine. Before the post-lock re-check, every
// such doomed statement occupied the lock for its full lifetime until the
// interrupt landed, and under cancellation storms honest callers starved
// behind a stream of statements whose clients were already gone.
func TestDeadOnArrivalStatementSkipsEngine(t *testing.T) {
	db, c := openSingleConn(t, ":memory:")
	if _, err := c.ExecContext(context.Background(),
		"CREATE TABLE doa(v INTEGER)"); err != nil {
		t.Fatal(err)
	}

	// Goroutine A holds the connection mutex with a runaway query for ~400ms
	// (its own deadline), far longer than B's deadline below.
	var wg sync.WaitGroup
	wg.Add(1)
	holderStarted := make(chan struct{})
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		defer cancel()
		close(holderStarted)
		rows, err := c.QueryContext(ctx, runawaySQL)
		if err == nil {
			rows.Close()
		}
	}()
	<-holderStarted
	time.Sleep(50 * time.Millisecond) // let A reach the engine and hold c.mu

	// B: an INSERT with a deadline that expires while queued behind A. It
	// must come back with the context error and MUST NOT have executed.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.ExecContext(ctx, "INSERT INTO doa VALUES (1)")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued-dead statement: want DeadlineExceeded, got %v", err)
	}
	wg.Wait()

	var n int
	if err := db.QueryRow("SELECT count(*) FROM doa").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("dead-on-arrival INSERT reached the engine: %d row(s) landed", n)
	}

	// The connection stays healthy for live work.
	var v int
	if err := c.QueryRowContext(context.Background(), "SELECT 7").Scan(&v); err != nil || v != 7 {
		t.Fatalf("post-storm query: v=%d err=%v", v, err)
	}
}

// TestDeadOnArrivalBeginSkipsEngine: BeginTx with a context that expires while
// queued must not issue the engine BEGIN (or leave transaction state behind) —
// otherwise every doomed request still costs two queued engine statements
// (BEGIN + ROLLBACK) under the connection mutex.
func TestDeadOnArrivalBeginSkipsEngine(t *testing.T) {
	_, c := openSingleConn(t, ":memory:")

	var wg sync.WaitGroup
	wg.Add(1)
	holderStarted := make(chan struct{})
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		defer cancel()
		close(holderStarted)
		rows, err := c.QueryContext(ctx, runawaySQL)
		if err == nil {
			rows.Close()
		}
	}()
	<-holderStarted
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.BeginTx(ctx, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued-dead BeginTx: want DeadlineExceeded, got %v", err)
	}
	wg.Wait()

	// No transaction state may linger: a fresh explicit transaction must
	// open, work, and commit on the same connection.
	tx, err := c.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("post-storm BeginTx: %v", err)
	}
	var v int
	if err := tx.QueryRowContext(context.Background(), "SELECT 11").Scan(&v); err != nil || v != 11 {
		t.Fatalf("in-tx query: v=%d err=%v", v, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
