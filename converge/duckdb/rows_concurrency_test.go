package duckdb

// Regression test for the "bind_varchar runaway": rows.Next/Close used to call
// into the engine WITHOUT the shared engine mutex. database/sql only reserves
// the rows' own connection, so on a pooled sql.DB (connector path: every conn
// shares ONE single-threaded wasm engine and ONE shadow-stack pointer global)
// a reader iterating rows raced writers executing prepared INSERTs binding
// varchar params on sibling conns. Two goroutines inside the engine at once
// corrupt the C shadow stack and heap; the classic late symptom was an
// infinite load32 spin inside a libc++ unordered_map bucket-chain walk
// (transpiled fn1662, reached from duckdb_bind_varchar's insert into the
// prepared statement's string-keyed parameter map) — i.e. walking a cycled
// chain. Also seen: dlmalloc aborts, "slice bounds out of range" panics.
//
// The fix gives rows the conn's engine mutex; this test is the watchdogged
// stress shape that reproduced the corruption within seconds pre-fix.

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPooledRowsIterationRacesBindExec(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test")
	}
	dbPath := filepath.Join(t.TempDir(), "rowsrace.db")
	db, err := sql.Open("duckdb", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(5)

	if _, err := db.Exec(`CREATE TABLE t (id BIGINT, a VARCHAR, b VARCHAR, c VARCHAR)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Seed enough rows that the reader's SELECT spans multiple chunks (so each
	// drain makes several unlocked-in-the-bad-build duckdb_fetch_chunk calls).
	seed, err := db.Prepare("INSERT INTO t VALUES (?, ?, ?, ?)")
	if err != nil {
		t.Fatalf("prepare seed: %v", err)
	}
	for i := 0; i < 5000; i++ {
		if _, err := seed.Exec(int64(i), fmt.Sprintf("seed-a-%06d", i), "in", fmt.Sprintf("heap-string-payload-%016d", i)); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}
	seed.Close()

	const loadFor = 8 * time.Second
	deadline := time.Now().Add(loadFor)
	var progress atomic.Int64
	errCh := make(chan error, 8)
	var wg sync.WaitGroup

	// 3 writers: per-row prepared INSERT with varchar binds inside small txs.
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			st, err := db.Prepare("INSERT INTO t VALUES (?, ?, ?, ?)")
			if err != nil {
				errCh <- fmt.Errorf("writer %d prepare: %w", w, err)
				return
			}
			defer st.Close()
			i := 0
			for time.Now().Before(deadline) {
				tx, err := db.Begin()
				if err != nil {
					errCh <- fmt.Errorf("writer %d begin: %w", w, err)
					return
				}
				ts := tx.Stmt(st)
				for j := 0; j < 20; j++ {
					i++
					if _, err := ts.Exec(int64(i), fmt.Sprintf("w%d-%09d", w, i), "inline9b", fmt.Sprintf("heap-string-payload-%d-%016d", w, i)); err != nil {
						ts.Close()
						tx.Rollback()
						errCh <- fmt.Errorf("writer %d exec: %w", w, err)
						return
					}
					progress.Add(1)
				}
				ts.Close()
				if err := tx.Commit(); err != nil {
					errCh <- fmt.Errorf("writer %d commit: %w", w, err)
					return
				}
			}
		}(w)
	}

	// 1 reader: iterate a multi-chunk result repeatedly (the unlocked path).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for time.Now().Before(deadline) {
			rows, err := db.Query("SELECT id, a, b, c FROM t LIMIT 5000")
			if err != nil {
				errCh <- fmt.Errorf("reader query: %w", err)
				return
			}
			var id int64
			var a, b, c string
			for rows.Next() {
				if err := rows.Scan(&id, &a, &b, &c); err != nil {
					rows.Close()
					errCh <- fmt.Errorf("reader scan: %w", err)
					return
				}
				progress.Add(1)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				errCh <- fmt.Errorf("reader rows: %w", err)
				return
			}
			rows.Close()
		}
	}()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	// Watchdog: the workers self-terminate at deadline; if the engine spins,
	// progress freezes and done never closes. Dump stacks for the evidence.
	last, lastT := int64(-1), time.Now()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-done:
			select {
			case err := <-errCh:
				t.Fatalf("worker error: %v", err)
			default:
			}
			t.Logf("ops=%d in %s", progress.Load(), loadFor)
			return
		case <-tick.C:
			cur := progress.Load()
			if cur != last {
				last, lastT = cur, time.Now()
				continue
			}
			if time.Since(lastT) > 30*time.Second {
				buf := make([]byte, 16<<20)
				n := runtime.Stack(buf, true)
				t.Fatalf("engine stalled: no progress for 30s at op=%d\n%s", cur, buf[:n])
			}
		}
	}
}
