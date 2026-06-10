// bindstress is the regression harness for the "bind_varchar runaway": under
// storage-write load (thousands of prepared INSERT executions binding string
// params), the engine was observed to spin forever inside a transpiled hash-
// table walk (load32 loop over a corrupted libc++ unordered_map bucket chain).
//
// Two modes:
//
//   - serial: ONE goroutine hammering prepared bind_varchar/exec/clear cycles
//     with varied string sizes (inline <=12B and heap >12B), transaction
//     boundaries, several prepared statements, and interleaved SELECT row
//     iteration. This is the literal per-row INSERT shape from the
//     bigquery-emulator TestStorageWrite path.
//
//   - concurrent: the same write load from -workers goroutines on a pooled
//     sql.DB (connector path: ALL pooled conns share ONE single-threaded wasm
//     engine), plus one reader goroutine iterating SELECT results. Rows
//     iteration (driver rows.Next/Close) is the suspected unlocked engine
//     entry: database/sql only reserves the rows' OWN conn, so another pooled
//     conn's locked Exec can enter the shared engine concurrently.
//
// A watchdog tracks a global progress counter; if no progress happens for
// -stall, it dumps every goroutine stack (the evidence: which fnNNNN the
// engine spins in) and exits 2. Memory-corruption panics also exit non-zero.
//
// Usage:
//
//	bindstress -mode=serial     -duration=2m
//	bindstress -mode=concurrent -duration=30s -workers=4
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "duckdbconverge/duckdb"
)

var progress atomic.Int64

func fatal(what string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL %s: %v\n", what, err)
		os.Exit(1)
	}
}

// dumpAllStacks writes every goroutine's stack to stderr (SIGQUIT-equivalent,
// but works on a deadline without external signaling).
func dumpAllStacks() {
	buf := make([]byte, 64<<20)
	n := runtime.Stack(buf, true)
	os.Stderr.Write(buf[:n])
}

// varied string payloads: inline (<=12 bytes fits duckdb string_t inline) and
// heap-allocated (>12 bytes), plus a large one to churn the engine allocator.
func payload(rng *rand.Rand, i int) string {
	switch i % 4 {
	case 0:
		return fmt.Sprintf("in%07d", i) // 9 bytes: inline
	case 1:
		return fmt.Sprintf("heap-string-%016d-%08x", i, rng.Uint32()) // 37B: heap
	case 2:
		return strings.Repeat("x", 100+rng.Intn(400)) // larger heap churn
	default:
		return "" // empty string edge
	}
}

func setupSchema(db *sql.DB) {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS t (
		id BIGINT, a VARCHAR, b VARCHAR, c VARCHAR, d DOUBLE)`)
	fatal("create table", err)
}

// writeLoad runs prepared bind/exec cycles until stop. Each outer iteration:
// BEGIN, N per-row execs through one of several prepared statements, COMMIT,
// then (serial mode) one SELECT iterating rows.
func writeLoad(db *sql.DB, stop <-chan struct{}, seed int64, interleaveSelect bool) error {
	rng := rand.New(rand.NewSource(seed))
	stmts := make([]*sql.Stmt, 3)
	for i := range stmts {
		st, err := db.Prepare("INSERT INTO t VALUES (?, ?, ?, ?, ?)")
		if err != nil {
			return fmt.Errorf("prepare: %w", err)
		}
		defer st.Close()
		stmts[i] = st
	}
	i := 0
	for {
		select {
		case <-stop:
			return nil
		default:
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin: %w", err)
		}
		batch := 25 + rng.Intn(50)
		for j := 0; j < batch; j++ {
			i++
			st := tx.Stmt(stmts[i%len(stmts)])
			_, err := st.Exec(int64(i), payload(rng, i), payload(rng, i+1),
				payload(rng, i+2), rng.Float64())
			st.Close()
			if err != nil {
				tx.Rollback()
				return fmt.Errorf("exec row %d: %w", i, err)
			}
			progress.Add(1)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit: %w", err)
		}
		if interleaveSelect && i%500 < batch {
			if err := drainSelect(db); err != nil {
				return err
			}
		}
	}
}

// drainSelect iterates a multi-chunk result through the driver rows path
// (rows.Next -> duckdb_fetch_chunk under the hood).
func drainSelect(db *sql.DB) error {
	rows, err := db.Query("SELECT id, a, b, c, d FROM t ORDER BY id DESC LIMIT 5000")
	if err != nil {
		return fmt.Errorf("select: %w", err)
	}
	defer rows.Close()
	var id int64
	var a, b, c string
	var d float64
	for rows.Next() {
		if err := rows.Scan(&id, &a, &b, &c, &d); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		progress.Add(1)
	}
	return rows.Err()
}

func main() {
	mode := flag.String("mode", "serial", "serial | concurrent")
	duration := flag.Duration("duration", 60*time.Second, "how long to hammer")
	workers := flag.Int("workers", 4, "writer goroutines (concurrent mode)")
	stall := flag.Duration("stall", 30*time.Second, "watchdog: max time without progress before stack dump + exit 2")
	dsn := flag.String("dsn", "", "DSN; default fresh file DB in TMPDIR")
	flag.Parse()

	d := *dsn
	if d == "" {
		f := filepath.Join(os.TempDir(), fmt.Sprintf("bindstress_%d.db", os.Getpid()))
		os.Remove(f)
		defer os.Remove(f)
		d = f
	}
	db, err := sql.Open("duckdb", d)
	fatal("open", err)
	defer db.Close()
	setupSchema(db)

	stop := make(chan struct{})
	time.AfterFunc(*duration, func() { close(stop) })

	// Watchdog: any -stall window without progress means the engine is spinning.
	watchdogDone := make(chan struct{})
	go func() {
		last, lastT := progress.Load(), time.Now()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-watchdogDone:
				return
			case <-tick.C:
				cur := progress.Load()
				if cur != last {
					last, lastT = cur, time.Now()
					continue
				}
				if time.Since(lastT) > *stall {
					fmt.Fprintf(os.Stderr, "WATCHDOG: no progress for %s at op=%d — dumping stacks\n", *stall, cur)
					dumpAllStacks()
					os.Exit(2)
				}
			}
		}
	}()

	var wg sync.WaitGroup
	errCh := make(chan error, *workers+1)
	switch *mode {
	case "serial":
		db.SetMaxOpenConns(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := writeLoad(db, stop, 1, true); err != nil {
				errCh <- err
			}
		}()
	case "concurrent":
		db.SetMaxOpenConns(*workers + 2)
		for w := 0; w < *workers; w++ {
			wg.Add(1)
			go func(seed int64) {
				defer wg.Done()
				if err := writeLoad(db, stop, seed, false); err != nil {
					errCh <- err
				}
			}(int64(w + 1))
		}
		// Dedicated reader: drives the driver rows.Next path concurrently with
		// the writers' locked Execs on sibling pooled conns.
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := drainSelect(db); err != nil {
					errCh <- err
					return
				}
			}
		}()
	default:
		fatal("flags", fmt.Errorf("unknown -mode %q", *mode))
	}

	wg.Wait()
	close(watchdogDone)
	select {
	case err := <-errCh:
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	default:
	}
	fmt.Printf("OK mode=%s ops=%d duration=%s\n", *mode, progress.Load(), *duration)
}
