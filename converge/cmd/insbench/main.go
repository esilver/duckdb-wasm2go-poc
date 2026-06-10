// insbench reproduces issue #1's phase-B INSERT shape against the pure-Go DuckDB
// driver and measures the two unowned levers:
//
//   - GC churn (lever 3): runtime.madvise was 26% of phase-B cycles. The -gc flag
//     applies a GC posture INSIDE the binary (debug.SetGCPercent /
//     debug.SetMemoryLimit) so each configuration is self-contained — run the
//     binary once per configuration, no env vars needed.
//   - Appender (lever 2): -path=appender loads the same table through
//     duckdb.NewAppender (buffer all rows, one Flush) instead of per-row
//     prepared exec, the headline comparison.
//
// Shape mirrors the issue's harness: table t(id BIGINT, grp BIGINT, val DOUBLE)
// (BigQuery INT64/INT64/FLOAT64), file-backed DB, 50-row warmup outside the
// timed region, then 3,000 timed rows — exec path inside one transaction with
// one prepared statement (the bigquery-emulator insertAll repository shape).
//
// Usage:
//
//	insbench -path=exec     -gc=default|gogc400|memlimit [-rows=3000] [-dsn=...]
//	insbench -path=appender -gc=default|gogc400|memlimit [-rows=3000] [-dsn=...]
//
// Output (one line, machine-greppable):
//
//	RESULT path=exec gc=default rows=3000 total_ms=4650.123 ms_per_row=1.5500
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"duckdbconverge/duckdb"
)

func must(err error, what string) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL %s: %v\n", what, err)
		os.Exit(1)
	}
}

func main() {
	path := flag.String("path", "exec", "load path: exec (prepared per-row Exec in one tx) | appender (duckdb.Appender, one flush)")
	gcMode := flag.String("gc", "default", "GC posture: default | gogc400 | memlimit (GOGC=off + 4GiB soft limit)")
	rows := flag.Int("rows", 3000, "timed row count")
	dsn := flag.String("dsn", "", "DSN; default is a fresh file DB in TMPDIR (issue phase B shape)")
	flag.Parse()

	switch *gcMode {
	case "default":
	case "gogc400":
		debug.SetGCPercent(400)
	case "memlimit":
		debug.SetGCPercent(-1)
		debug.SetMemoryLimit(4 << 30)
	default:
		must(fmt.Errorf("unknown -gc %q", *gcMode), "flags")
	}

	d := *dsn
	if d == "" {
		f := filepath.Join(os.TempDir(), fmt.Sprintf("insbench_%d.db", os.Getpid()))
		os.Remove(f)
		defer os.Remove(f)
		d = f
	}

	db, err := sql.Open("duckdb", d)
	must(err, "open")
	defer db.Close()
	_, err = db.Exec("CREATE TABLE t (id BIGINT, grp BIGINT, val DOUBLE)")
	must(err, "create table")

	// Warmup outside the timed region (catalog/first-statement one-time costs).
	loadExec(db, 0, 50)

	start := time.Now()
	switch *path {
	case "exec":
		loadExec(db, 50, *rows)
	case "appender":
		loadAppender(db, 50, *rows)
	default:
		must(fmt.Errorf("unknown -path %q", *path), "flags")
	}
	el := time.Since(start)

	var n int64
	must(db.QueryRow("SELECT count(*) FROM t").Scan(&n), "count")
	if want := int64(50 + *rows); n != want {
		must(fmt.Errorf("table has %d rows, want %d", n, want), "verify")
	}

	fmt.Printf("RESULT path=%s gc=%s rows=%d total_ms=%.3f ms_per_row=%.4f\n",
		*path, *gcMode, *rows, float64(el.Microseconds())/1000.0,
		float64(el.Microseconds())/1000.0/float64(*rows))
}

// loadExec is the issue's phase-B repository shape: one transaction, one
// prepared INSERT, n single-row Execs.
func loadExec(db *sql.DB, base, n int) {
	tx, err := db.Begin()
	must(err, "begin")
	st, err := tx.Prepare("INSERT INTO t VALUES (?, ?, ?)")
	must(err, "prepare")
	for i := base; i < base+n; i++ {
		_, err := st.Exec(int64(i), int64(i%1000), float64(i)/7.0)
		must(err, "exec")
	}
	must(st.Close(), "stmt close")
	must(tx.Commit(), "commit")
}

// loadAppender loads the same rows through the table-function-backed Appender:
// buffer everything, one Flush at the end.
func loadAppender(db *sql.DB, base, n int) {
	c, err := db.Conn(context.Background())
	must(err, "conn")
	defer c.Close()
	a, err := duckdb.NewAppender(c, "", "t")
	must(err, "NewAppender")
	for i := base; i < base+n; i++ {
		must(a.AppendRow(int64(i), int64(i%1000), float64(i)/7.0), "AppendRow")
	}
	must(a.Close(), "appender close") // Close flushes
}
