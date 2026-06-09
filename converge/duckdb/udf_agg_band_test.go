package duckdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// udf_agg_band_test.go — exercises RegisterAggregateBandConn end to end over the
// database/sql surface (the googlesqlite emulator's exact path): arity-band
// overloads via the function-set API, ANY-param runtime decode, DECIMAL→float64,
// numeric result columns, NULL results, finalize errors, and single-state
// window-path routing.

// bandState collects raw rows (the googlesqlite replay shape: Update appends,
// Combine concatenates, Finalize summarizes).
type bandState struct{ rows [][]any }

// bandCollect finalizes to "rows=N;types=t0,t1,..." where the types are the Go
// kinds of the FIRST collected row — proving both the arity that bound and the
// runtime decode of each ANY arg.
type bandCollect struct{}

func (bandCollect) NewState() any { return &bandState{} }
func (bandCollect) Update(state any, args []any) {
	s := state.(*bandState)
	s.rows = append(s.rows, args)
}
func (bandCollect) Combine(dst, src any) {
	d, s := dst.(*bandState), src.(*bandState)
	d.rows = append(d.rows, s.rows...)
}
func (bandCollect) Finalize(state any) (any, error) {
	s := state.(*bandState)
	if len(s.rows) == 0 {
		return "rows=0", nil
	}
	kinds := make([]string, len(s.rows[0]))
	for i, v := range s.rows[0] {
		kinds[i] = fmt.Sprintf("%T", v)
	}
	return fmt.Sprintf("rows=%d;types=%s", len(s.rows), strings.Join(kinds, ",")), nil
}

// bandSumDouble sums float64-coercible args into a DOUBLE result; NULL args are
// skipped; an empty group finalizes to nil (SQL NULL).
type bandSumDouble struct{}

func (bandSumDouble) NewState() any { return &bandState{} }
func (bandSumDouble) Update(state any, args []any) {
	s := state.(*bandState)
	s.rows = append(s.rows, args)
}
func (bandSumDouble) Combine(dst, src any) {
	d, s := dst.(*bandState), src.(*bandState)
	d.rows = append(d.rows, s.rows...)
}
func (bandSumDouble) Finalize(state any) (any, error) {
	s := state.(*bandState)
	sum, seen := 0.0, false
	for _, row := range s.rows {
		for _, v := range row {
			if f, ok := asFloat64(v); ok {
				sum += f
				seen = true
			}
		}
	}
	if !seen {
		return nil, nil
	}
	return sum, nil
}

// bandCountI64 counts rows into a BIGINT result.
type bandCountI64 struct{}

func (bandCountI64) NewState() any { return &bandState{} }
func (bandCountI64) Update(state any, args []any) {
	s := state.(*bandState)
	s.rows = append(s.rows, args)
}
func (bandCountI64) Combine(dst, src any) {
	d, s := dst.(*bandState), src.(*bandState)
	d.rows = append(d.rows, s.rows...)
}
func (bandCountI64) Finalize(state any) (any, error) {
	return int64(len(state.(*bandState).rows)), nil
}

// bandFail always errors at finalize.
type bandFail struct{}

func (bandFail) NewState() any             { return &bandState{} }
func (bandFail) Update(state any, _ []any) {}
func (bandFail) Combine(dst, src any)      {}
func (bandFail) Finalize(any) (any, error) { return nil, errors.New("band-finalize-boom") }

func bandConn(t *testing.T) (*sql.Conn, func()) {
	t.Helper()
	db, err := sql.Open("duckdb", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	c, err := db.Conn(context.Background())
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	return c, func() { c.Close(); db.Close() }
}

func TestAggBandArityAndAnyDecode(t *testing.T) {
	c, done := bandConn(t)
	defer done()

	err := RegisterAggregateBandConn(c, "band_collect",
		AggregateOptions{MinArgs: 1, MaxArgs: 3, ResultTypeID: dtVarchar}, bandCollect{})
	if err != nil {
		t.Fatalf("register band_collect: %v", err)
	}

	q := func(sql string) string {
		var s string
		if err := c.QueryRowContext(context.Background(), sql).Scan(&s); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return s
	}

	// Every arity in the band binds, and each ANY arg decodes by its RUNTIME type.
	if got := q("SELECT band_collect(x) FROM range(1,6) t(x)"); got != "rows=5;types=int64" {
		t.Fatalf("arity 1: got %q", got)
	}
	if got := q("SELECT band_collect(x, 'a') FROM range(1,6) t(x)"); got != "rows=5;types=int64,string" {
		t.Fatalf("arity 2: got %q", got)
	}
	if got := q("SELECT band_collect(x, 'a', 2.5e0) FROM range(1,6) t(x)"); got != "rows=5;types=int64,string,float64" {
		t.Fatalf("arity 3: got %q", got)
	}
	// DECIMAL literal (0.5 binds as DECIMAL) arrives as float64 (band convention).
	if got := q("SELECT band_collect(0.5)"); got != "rows=1;types=float64" {
		t.Fatalf("decimal arg: got %q", got)
	}
	// NULL rows reach Update as nil args (special handling always on).
	if got := q("SELECT band_collect(NULL)"); got != "rows=1;types=<nil>" {
		t.Fatalf("null arg: got %q", got)
	}
	t.Logf("arity band 1..3 binds; ANY args decode by runtime type; DECIMAL→float64; NULL→nil ✓")

	// GROUP BY: distinct per-group states + combine.
	rows, err := c.QueryContext(context.Background(),
		"SELECT x%2 g, band_collect(x) FROM range(1,11) t(x) GROUP BY x%2 ORDER BY g")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var g int64
		var s string
		if err := rows.Scan(&g, &s); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("g%d:%s", g, s))
	}
	sort.Strings(got)
	want := []string{"g0:rows=5;types=int64", "g1:rows=5;types=int64"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("grouped: got %v want %v", got, want)
	}
	t.Logf("grouped band aggregate: %v ✓", got)
}

func TestAggBandNumericResults(t *testing.T) {
	c, done := bandConn(t)
	defer done()

	if err := RegisterAggregateBandConn(c, "band_fsum",
		AggregateOptions{MinArgs: 1, MaxArgs: 2, ResultTypeID: dtDouble}, bandSumDouble{}); err != nil {
		t.Fatalf("register band_fsum: %v", err)
	}
	if err := RegisterAggregateBandConn(c, "band_count",
		AggregateOptions{MinArgs: 1, MaxArgs: 1, ResultTypeID: dtBigint}, bandCountI64{}); err != nil {
		t.Fatalf("register band_count: %v", err)
	}

	var f float64
	if err := c.QueryRowContext(context.Background(),
		"SELECT band_fsum(x) FROM range(1,11) t(x)").Scan(&f); err != nil {
		t.Fatal(err)
	}
	if f != 55 {
		t.Fatalf("band_fsum(1..10) = %v, want 55", f)
	}

	var n int64
	if err := c.QueryRowContext(context.Background(),
		"SELECT band_count(x) FROM range(7) t(x)").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 7 {
		t.Fatalf("band_count(range 7) = %d, want 7", n)
	}

	// nil Finalize → SQL NULL (DOUBLE result over zero coercible values).
	var isNull bool
	if err := c.QueryRowContext(context.Background(),
		"SELECT band_fsum(s) IS NULL FROM (SELECT 'x' AS s WHERE false) t").Scan(&isNull); err != nil {
		t.Fatal(err)
	}
	if !isNull {
		t.Fatal("empty group should finalize to SQL NULL")
	}
	t.Logf("DOUBLE=55, BIGINT=7, empty→NULL ✓")
}

func TestAggBandFinalizeError(t *testing.T) {
	c, done := bandConn(t)
	defer done()

	if err := RegisterAggregateBandConn(c, "band_fail",
		AggregateOptions{MinArgs: 1, MaxArgs: 1, ResultTypeID: dtVarchar}, bandFail{}); err != nil {
		t.Fatalf("register band_fail: %v", err)
	}
	var s string
	err := c.QueryRowContext(context.Background(), "SELECT band_fail(1)").Scan(&s)
	if err == nil {
		t.Fatalf("band_fail query unexpectedly succeeded: %q", s)
	}
	if !strings.Contains(err.Error(), "band-finalize-boom") {
		t.Fatalf("finalize error not surfaced: %v", err)
	}
	t.Logf("finalize error surfaced through the query: %v ✓", err)
}

func TestAggBandSingleStateWindow(t *testing.T) {
	c, done := bandConn(t)
	defer done()

	// SingleState routes every frame row to states[0] — the tf_idf /
	// st_clusterdbscan shape, which only ever runs under OVER ().
	if err := RegisterAggregateBandConn(c, "band_wincount",
		AggregateOptions{MinArgs: 1, MaxArgs: 1, ResultTypeID: dtBigint, SingleState: true}, bandCountI64{}); err != nil {
		t.Fatalf("register band_wincount: %v", err)
	}

	rows, err := c.QueryContext(context.Background(),
		"SELECT band_wincount(x) OVER () FROM range(5) t(x)")
	if err != nil {
		t.Fatalf("window query: %v", err)
	}
	defer rows.Close()
	var vals []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		vals = append(vals, v)
	}
	if len(vals) != 5 {
		t.Fatalf("window rows: got %d, want 5", len(vals))
	}
	for i, v := range vals {
		if v != 5 {
			t.Fatalf("row %d: whole-partition count = %d, want 5 (vals=%v)", i, v, vals)
		}
	}
	t.Logf("single-state window aggregate: every row sees the whole partition (5) ✓")
}
