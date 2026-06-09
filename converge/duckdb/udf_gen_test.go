package duckdb

import (
	"fmt"
	"strings"
	"testing"
)

// udf_gen_test.go — exercises the GENERIC UDF API (RegisterScalarUDF /
// RegisterAggregateUDF) end-to-end through real SQL, generalizing the proven
// hand-rolled spikes in udf_test.go and udf_agg_test.go.
//
// Where those spikes hand-wired the indirect-table closures and read the result
// with raw memory math, here we drive the generic registrars with arbitrary Go
// func(args []any)(any,error) / AggregateImpl values, covering: multi-arg +
// multi-type scalars, a VARCHAR->VARCHAR scalar, NULL passthrough, and a
// grouped/ungrouped aggregate.

// queryI64 runs sql and returns the int64 at (col,row) of the result.
func queryI64(t *testing.T, mod *module, con int32, sql string, col, row int64) int64 {
	t.Helper()
	m := mod.m
	res := mod.allocOut(sizeofDuckdbResult)
	if rc := m.Xduckdb_query(con, mod.cstring(sql), res); rc != 0 {
		t.Fatalf("query %q failed (rc=%d): %s", sql, rc, mod.lastError())
	}
	return m.Xduckdb_value_int64(res, col, row)
}

// queryVarchar runs sql and returns the VARCHAR at (col,row) as a Go string.
// Xduckdb_value_varchar mallocs a NUL-terminated C string we must free.
func queryVarchar(t *testing.T, mod *module, con int32, sql string, col, row int64) string {
	t.Helper()
	m := mod.m
	res := mod.allocOut(sizeofDuckdbResult)
	if rc := m.Xduckdb_query(con, mod.cstring(sql), res); rc != 0 {
		t.Fatalf("query %q failed (rc=%d): %s", sql, rc, mod.lastError())
	}
	ptr := m.Xduckdb_value_varchar(res, col, row)
	s := mod.goString(ptr)
	m.Xduckdb_free(ptr)
	return s
}

// TestGenericScalarUDFs registers several scalar UDFs through RegisterScalarUDF
// and checks them via SQL. Table-driven over (register, sql, want) so each row
// proves an independent shape: multi-arg multi-int, VARCHAR transform, and NULL
// passthrough.
func TestGenericScalarUDFs(t *testing.T) {
	mod := newModule()
	con, _, err := mod.open(":memory:")
	if err != nil {
		t.Fatal(err)
	}

	// my_axpb(a,x,b) = a*x + b over three BIGINTs -> BIGINT (multi-arg, multi-row).
	if err := mod.RegisterScalarUDF(con, "my_axpb",
		[]int32{dtBigint, dtBigint, dtBigint}, dtBigint,
		func(args []any) (any, error) {
			a, _ := asInt64(args[0])
			x, _ := asInt64(args[1])
			b, _ := asInt64(args[2])
			return a*x + b, nil
		}); err != nil {
		t.Fatalf("register my_axpb: %v", err)
	}

	// my_shout(s) = upper(s) + "!" over VARCHAR -> VARCHAR.
	if err := mod.RegisterScalarUDF(con, "my_shout",
		[]int32{dtVarchar}, dtVarchar,
		func(args []any) (any, error) {
			s, _ := asString(args[0])
			return strings.ToUpper(s) + "!", nil
		}); err != nil {
		t.Fatalf("register my_shout: %v", err)
	}

	// my_x(v) returns nil for nil input, else v+1 (BIGINT) — proves NULL handling
	// across the codec (NULL decode -> nil arg, nil result -> SQL NULL).
	if err := mod.RegisterScalarUDF(con, "my_x",
		[]int32{dtBigint}, dtBigint,
		func(args []any) (any, error) {
			if args[0] == nil {
				return nil, nil
			}
			v, _ := asInt64(args[0])
			return v + 1, nil
		}); err != nil {
		t.Fatalf("register my_x: %v", err)
	}

	// Scalar (int64-result) cases.
	intCases := []struct {
		name string
		sql  string
		want int64
	}{
		{"axpb_const", "SELECT my_axpb(2, 20, 2)", 42},
		{"axpb_vectorized", "SELECT sum(my_axpb(2, x, 1)) FROM range(4) t(x)", 2*(0+1+2+3) + 4*1}, // 12+4=16
		{"null_is_null", "SELECT CASE WHEN my_x(NULL) IS NULL THEN 1 ELSE 0 END", 1},
		{"null_nonnull", "SELECT my_x(41)", 42},
	}
	for _, c := range intCases {
		t.Run(c.name, func(t *testing.T) {
			if got := queryI64(t, mod, con, c.sql, 0, 0); got != c.want {
				t.Fatalf("%s: got %d, want %d", c.sql, got, c.want)
			}
		})
	}

	// VARCHAR-result cases.
	strCases := []struct {
		name string
		sql  string
		want string
	}{
		{"shout_go", "SELECT my_shout('go')", "GO!"},
		{"shout_mixed", "SELECT my_shout('Hello, World')", "HELLO, WORLD!"},
	}
	for _, c := range strCases {
		t.Run(c.name, func(t *testing.T) {
			if got := queryVarchar(t, mod, con, c.sql, 0, 0); got != c.want {
				t.Fatalf("%s: got %q, want %q", c.sql, got, c.want)
			}
		})
	}

	t.Logf("scalar UDFs ok: my_axpb(2,20,2)=42, my_shout('go')=GO!, my_x(NULL) IS NULL")
}

// gsumState is a fresh AggregateImpl (independent of the package's SumInt64Agg) to
// prove the generic aggregate path works with a caller-supplied state object and
// handle table. SQL SUM semantics: NULLs skipped, empty/all-NULL group -> NULL.
type gsumState struct {
	sum int64
	any bool
}

type gsumAgg struct{}

func (gsumAgg) NewState() any { return &gsumState{} }

func (gsumAgg) Update(state any, args []any) {
	s := state.(*gsumState)
	if len(args) == 0 || args[0] == nil {
		return
	}
	if v, ok := asInt64(args[0]); ok {
		s.sum += v
		s.any = true
	}
}

func (gsumAgg) Combine(dst, src any) {
	d, s := dst.(*gsumState), src.(*gsumState)
	d.sum += s.sum
	d.any = d.any || s.any
}

func (gsumAgg) Finalize(state any) any {
	s := state.(*gsumState)
	if !s.any {
		return nil
	}
	return s.sum
}

// TestGenericAggregateUDF registers my_gsum(BIGINT)->BIGINT via RegisterAggregateUDF
// and checks both ungrouped (sum 1..10 = 55) and grouped-by-parity
// (even=30, odd=25) aggregation, exercising init/update/combine/finalize/destroy.
func TestGenericAggregateUDF(t *testing.T) {
	mod := newModule()
	con, _, err := mod.open(":memory:")
	if err != nil {
		t.Fatal(err)
	}

	if err := mod.RegisterAggregateUDF(con, "my_gsum",
		[]int32{dtBigint}, dtBigint, gsumAgg{}); err != nil {
		t.Fatalf("register my_gsum: %v", err)
	}

	cases := []struct {
		name     string
		sql      string
		col, row int64
		want     int64
	}{
		{"ungrouped", "SELECT my_gsum(x) FROM range(1,11) t(x)", 0, 0, 55},
		// GROUP BY x%2 ORDER BY g: g=0 even(2+4+6+8+10)=30 at row0, g=1 odd(1+3+5+7+9)=25 at row1.
		{"grouped_even", "SELECT x%2 g, my_gsum(x) s FROM range(1,11) t(x) GROUP BY x%2 ORDER BY g", 1, 0, 30},
		{"grouped_odd", "SELECT x%2 g, my_gsum(x) s FROM range(1,11) t(x) GROUP BY x%2 ORDER BY g", 1, 1, 25},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := queryI64(t, mod, con, c.sql, c.col, c.row); got != c.want {
				t.Fatalf("%s [%d,%d]: got %d, want %d", c.sql, c.col, c.row, got, c.want)
			}
		})
	}

	t.Log(fmt.Sprintf("aggregate my_gsum ok: ungrouped=55, grouped even/odd=30/25"))
}
