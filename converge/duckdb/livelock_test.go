package duckdb

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRollbackCleanupLivelock is the duckdb-go-pure issue #6 item-1 repro
// harness (from the bqe#14 close): pooled connections + mid-transaction
// statement failures + concurrent request-cancellation churn + rollback
// traffic against a FILE database. The field failure was an engine cleanup
// path (queryRaw("ROLLBACK") -> Fe10/Fe23/Fe25 invoke storm) livelocking
// while holding the shared engine mutex.
//
// Gate: every statement completes (success OR error) within opBudget — with
// the interrupt hook a runaway/livelocked statement on a cancelled context
// must come back as a failed query, not a wedge — and the harness makes
// overall progress. STRESS test: ~LIVELOCK_SECS (default 90) seconds of
// churn; skipped unless LIVELOCK=1.
func TestRollbackCleanupLivelock(t *testing.T) {
	if os.Getenv("LIVELOCK") == "" {
		t.Skip("stress repro harness; set LIVELOCK=1 to run")
	}
	dur := 90 * time.Second
	if s := os.Getenv("LIVELOCK_SECS"); s != "" {
		if n, err := time.ParseDuration(s + "s"); err == nil {
			dur = n
		}
	}
	const opBudget = 30 * time.Second

	dir := t.TempDir()
	db, err := sql.Open("duckdb", filepath.Join(dir, "livelock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(4)

	bg := context.Background()
	if _, err := db.ExecContext(bg,
		"CREATE TABLE t(id BIGINT, v VARCHAR); INSERT INTO t SELECT range, 'seed' FROM range(100)"); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(dur)
	var ops, fails, interrupted atomic.Int64
	var wg sync.WaitGroup

	// lastBeat[w] is the worker's most recent statement-start time; a stuck
	// statement freezes it, and the monitor converts that into a goroutine
	// dump + failure instead of letting the test hang to its own timeout.
	const workers = 6
	var beatMu sync.Mutex
	lastBeat := make([]time.Time, workers)
	beat := func(w int) {
		beatMu.Lock()
		lastBeat[w] = time.Now()
		beatMu.Unlock()
	}

	type execer interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	}
	run := func(w int, on execer, ctx context.Context, sqlText string, args ...any) {
		beat(w)
		ops.Add(1)
		_, err := on.ExecContext(ctx, sqlText, args...)
		if err != nil {
			fails.Add(1)
			if strings.Contains(err.Error(), "INTERRUPT") {
				interrupted.Add(1)
			}
		}
	}

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(w) * 7919))
			for time.Now().Before(deadline) {
				switch w % 3 {
				case 0: // tx churn with mid-tx failures + explicit ROLLBACK
					ctx, cancel := context.WithTimeout(bg, time.Duration(50+rng.Intn(400))*time.Millisecond)
					c, err := db.Conn(bg)
					if err != nil {
						cancel()
						continue
					}
					beat(w)
					_, _ = c.ExecContext(bg, "BEGIN")
					run(w, c, ctx, fmt.Sprintf("INSERT INTO t VALUES (%d, 'w%d')", rng.Intn(1<<30), w))
					// dialect-error injection (stands in for the broken MERGE
					// lowering): aborts the open transaction
					run(w, c, ctx, "UPDATE t SET v = nonexistent_fn(v) WHERE id = 1")
					// statements against the aborted tx ("Current transaction
					// is aborted" storm), then rollback traffic
					run(w, c, ctx, "INSERT INTO t VALUES (1, 'aborted')")
					beat(w)
					_, _ = c.ExecContext(bg, "ROLLBACK")
					c.Close()
					cancel()
				case 1: // cancellation churn on long-ish statements
					ctx, cancel := context.WithTimeout(bg, time.Duration(20+rng.Intn(150))*time.Millisecond)
					beat(w)
					ops.Add(1)
					rows, err := db.QueryContext(ctx,
						"SELECT count(*) FROM t a, t b, range(20000) r WHERE a.id <> b.id")
					if err != nil {
						fails.Add(1)
						if strings.Contains(err.Error(), "INTERRUPT") {
							interrupted.Add(1)
						}
					} else {
						rows.Close()
					}
					cancel()
				case 2: // reader + write pressure (checkpoint/WAL traffic)
					run(w, db, bg, fmt.Sprintf("INSERT INTO t SELECT range, 'bulk' FROM range(%d)", 50+rng.Intn(200)))
					beat(w)
					ops.Add(1)
					var n int64
					if err := db.QueryRowContext(bg, "SELECT count(*) FROM t").Scan(&n); err != nil {
						fails.Add(1)
					}
					if rng.Intn(4) == 0 {
						run(w, db, bg, "DELETE FROM t WHERE v = 'bulk'")
					}
				}
			}
		}(w)
	}

	// Monitor: any worker whose statement has been in flight > opBudget is the
	// wedge. Dump all goroutines (the engine stack fingerprint) and fail.
	wedged := make(chan string, 1)
	go func() {
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				if time.Now().After(deadline.Add(opBudget + 10*time.Second)) {
					return // workers should all be done; let wg.Wait settle it
				}
				beatMu.Lock()
				var stuck int = -1
				for i, b := range lastBeat {
					if !b.IsZero() && time.Since(b) > opBudget {
						stuck = i
						break
					}
				}
				beatMu.Unlock()
				if stuck >= 0 {
					buf := make([]byte, 1<<22)
					n := runtime.Stack(buf, true)
					dump := filepath.Join(os.TempDir(), "dgp6-livelock-dump.txt")
					_ = os.WriteFile(dump, buf[:n], 0o644)
					select {
					case wedged <- fmt.Sprintf("worker %d statement in flight > %v; goroutine dump: %s", stuck, opBudget, dump):
					default:
					}
					return
				}
			}
		}
	}()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case msg := <-wedged:
		t.Fatalf("LIVELOCK reproduced: %s", msg)
	case <-time.After(dur + opBudget + 30*time.Second):
		t.Fatal("harness itself hung (workers never finished)")
	}

	t.Logf("ops=%d fails=%d interrupted=%d (no wedge; every statement bounded by %v)",
		ops.Load(), fails.Load(), interrupted.Load(), opBudget)
	if ops.Load() < 100 {
		t.Fatalf("too little progress: ops=%d", ops.Load())
	}
}
