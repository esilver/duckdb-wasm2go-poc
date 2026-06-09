// Demo: pure-Go DuckDB via database/sql (CGO_ENABLED=0). Exercises Tier 1 (driver,
// prepared statements, typed Scan, aggregates) and Tier 2 (file-backed DB persisted
// and reopened, plus reading a CSV file off disk through the WASI filesystem shim).
package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "duckdbconverge/duckdb"
)

func must(err error, what string) {
	if err != nil {
		fmt.Printf("FATAL %s: %v\n", what, err)
		os.Exit(1)
	}
}

func main() {
	// ---- Tier 1: in-memory, database/sql, prepared statements, typed Scan ----
	db, err := sql.Open("duckdb", ":memory:")
	must(err, "open :memory:")
	defer db.Close()

	var ver string
	must(db.QueryRow("SELECT version()").Scan(&ver), "version")
	fmt.Println("DuckDB version:", ver)

	_, err = db.Exec(`CREATE TABLE people(id INTEGER, name VARCHAR, score DOUBLE, active BOOLEAN, born DATE)`)
	must(err, "create")

	ins, err := db.Prepare(`INSERT INTO people VALUES (?, ?, ?, ?, ?)`)
	must(err, "prepare insert")
	rows := []struct {
		id          int
		name        string
		score       float64
		active      bool
		born        string
	}{
		{1, "ada", 9.5, true, "1815-12-10"},
		{2, "alan", 8.25, false, "1912-06-23"},
		{3, "grace", 9.99, true, "1906-12-09"},
	}
	for _, r := range rows {
		_, err = ins.Exec(r.id, r.name, r.score, r.active, r.born)
		must(err, "insert "+r.name)
	}
	ins.Close()

	fmt.Println("\n-- typed Scan over a SELECT --")
	rs, err := db.Query(`SELECT id, name, score, active, born FROM people ORDER BY score DESC`)
	must(err, "query")
	for rs.Next() {
		var id int
		var name string
		var score float64
		var active bool
		var born time.Time
		must(rs.Scan(&id, &name, &score, &active, &born), "scan")
		fmt.Printf("  id=%d name=%-6s score=%.2f active=%-5v born=%s\n",
			id, name, score, active, born.Format("2006-01-02"))
	}
	must(rs.Err(), "rows.Err")

	fmt.Println("\n-- aggregates + NULL handling --")
	var n int
	var avg float64
	var maxName sql.NullString
	must(db.QueryRow(`SELECT count(*), avg(score), max(name) FROM people`).Scan(&n, &avg, &maxName), "agg")
	fmt.Printf("  count=%d avg=%.3f max(name)=%v\n", n, avg, maxName.String)

	var nullv sql.NullInt64
	must(db.QueryRow(`SELECT NULL::INTEGER`).Scan(&nullv), "null")
	fmt.Printf("  NULL::INTEGER -> valid=%v\n", nullv.Valid)

	// parameterized query
	var got string
	must(db.QueryRow(`SELECT name FROM people WHERE id = ?`, 3).Scan(&got), "param query")
	fmt.Printf("  name where id=3 -> %q\n", got)

	// ---- Tier 2: file-backed DB, persisted and reopened ----
	fmt.Println("\n-- Tier 2: persistent file DB --")
	dir, _ := os.MkdirTemp("", "duckdbdemo")
	defer os.RemoveAll(dir)
	dbpath := filepath.Join(dir, "test.duckdb")

	fdb, err := sql.Open("duckdb", dbpath)
	must(err, "open file db handle")
	if _, err = fdb.Exec(`CREATE TABLE t(x INTEGER); INSERT INTO t VALUES (10),(20),(12)`); err != nil {
		fmt.Printf("  file DB NOT supported by this build: %v\n", err)
		fmt.Println("  (emscripten opens files in-module/MEMFS; host persistence needs a wasi-sdk build)")
		fdb.Close()
	} else {
		fdb.Close()
		if fi, e := os.Stat(dbpath); e == nil {
			fmt.Printf("  wrote %s (%d bytes on disk)\n", filepath.Base(dbpath), fi.Size())
		}
		fdb2, _ := sql.Open("duckdb", dbpath)
		var sum int
		if e := fdb2.QueryRow(`SELECT sum(x) FROM t`).Scan(&sum); e == nil {
			fmt.Printf("  reopened -> SELECT sum(x) = %d (expect 42)\n", sum)
		}
		fdb2.Close()
	}

	// ---- Tier 2: read a CSV file off disk ----
	fmt.Println("\n-- Tier 2: read_csv off disk --")
	csv := filepath.Join(dir, "data.csv")
	must(os.WriteFile(csv, []byte("a,b\n1,foo\n2,bar\n3,baz\n"), 0o644), "write csv")
	var rowsN int
	if err := db.QueryRow(fmt.Sprintf(`SELECT count(*) FROM read_csv_auto('%s')`, csv)).Scan(&rowsN); err != nil {
		fmt.Printf("  read_csv ERROR: %v\n", err)
	} else {
		fmt.Printf("  read_csv_auto rows=%d (expect 3)\n", rowsN)
	}

	fmt.Println("\nDONE — pure-Go DuckDB via database/sql, CGO_ENABLED=0")
}
