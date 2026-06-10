// qbench times a SELECT-heavy aggregation query (lane 1-B benchmark):
// SELECT sum(x),avg(x),count(*) FROM range(3000000) GROUP BY range%1000.
package main

import (
	"database/sql"
	"fmt"
	"os"
	"time"

	_ "duckdbconverge/duckdb"
)

func main() {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer db.Close()

	// warmup: touch the query path once with a small range
	if _, err := db.Exec("SELECT sum(range),avg(range),count(*) FROM range(1000) GROUP BY range%10"); err != nil {
		fmt.Fprintln(os.Stderr, "warmup:", err)
		os.Exit(1)
	}

	const q = "SELECT sum(range),avg(range),count(*) FROM range(3000000) GROUP BY range%1000"
	start := time.Now()
	rows, err := db.Query(q)
	if err != nil {
		fmt.Fprintln(os.Stderr, "query:", err)
		os.Exit(1)
	}
	n := 0
	var s, c int64
	var a float64
	for rows.Next() {
		if err := rows.Scan(&s, &a, &c); err != nil {
			fmt.Fprintln(os.Stderr, "scan:", err)
			os.Exit(1)
		}
		n++
	}
	rows.Close()
	el := time.Since(start)
	fmt.Printf("RESULT query=groupby3m groups=%d total_ms=%.3f\n", n, float64(el.Microseconds())/1000.0)
}
