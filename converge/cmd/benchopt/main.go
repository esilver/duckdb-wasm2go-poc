// benchopt times three representative workloads (group-by aggregate, self-join,
// string/distinct) on :memory: through the database/sql driver. Build it twice —
// against genopt (default flags) and against genpkg (-N -l) — to measure the
// multi-package optimization speedup. Prints one line per run plus medians.
package main

import (
	"database/sql"
	"fmt"
	"log"
	"sort"
	"time"

	_ "duckdbconverge/duckdb"
)

const runs = 5

func main() {
	db, err := sql.Open("duckdb", ":memory:")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	start := time.Now()
	if _, err := db.Exec("CREATE TABLE t AS SELECT range x FROM range(3000000)"); err != nil {
		log.Fatalf("setup: %v", err)
	}
	fmt.Printf("setup (3M rows): %.3fs\n", time.Since(start).Seconds())

	queries := []struct{ name, sql string }{
		{"a_groupby", "SELECT sum(x), avg(x), count(*) FROM t GROUP BY x % 1000"},
		{"b_join", "SELECT count(*) FROM t a JOIN t b ON a.x = b.x WHERE a.x < 200000"},
		{"c_string", "SELECT count(DISTINCT md5(x::VARCHAR)) FROM t WHERE x < 300000"},
	}
	for _, q := range queries {
		var times []float64
		for i := 0; i < runs; i++ {
			t0 := time.Now()
			rows, err := db.Query(q.sql)
			if err != nil {
				log.Fatalf("%s: %v", q.name, err)
			}
			n := 0
			for rows.Next() {
				n++
			}
			if err := rows.Err(); err != nil {
				log.Fatalf("%s rows: %v", q.name, err)
			}
			rows.Close()
			d := time.Since(t0).Seconds()
			times = append(times, d)
			fmt.Printf("%s run%d: %.3fs (%d rows)\n", q.name, i+1, d, n)
		}
		sort.Float64s(times)
		fmt.Printf("%s MEDIAN: %.3fs\n", q.name, times[runs/2])
	}
}
