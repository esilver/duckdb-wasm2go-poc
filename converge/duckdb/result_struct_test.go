package duckdb

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

// TestStructResultColumns: native STRUCT result columns must scan as
// map[string]any of decoded fields (duckdb-go's STRUCT shape). Regression:
// they decoded to nil (the flat decode path had no STRUCT case), so e.g.
// approx_top_k's LIST-of-STRUCT cells came back as [NULL, ...].
func TestStructResultColumns(t *testing.T) {
	c, done := bandConn(t)
	defer done()
	ctx := context.Background()

	var v any
	if err := c.QueryRowContext(ctx, "SELECT {'a': 1, 'b': 'x'}").Scan(&v); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"a": int64(1), "b": "x"}
	if !reflect.DeepEqual(v, want) {
		t.Fatalf("struct result: got %T %#v, want %#v", v, v, want)
	}

	// LIST of STRUCT recurses (the approx_top_k shape).
	if err := c.QueryRowContext(ctx, "SELECT [{'i': 1}, {'i': 2}]").Scan(&v); err != nil {
		t.Fatal(err)
	}
	wantList := []any{map[string]any{"i": int64(1)}, map[string]any{"i": int64(2)}}
	if !reflect.DeepEqual(v, wantList) {
		t.Fatalf("list-of-struct result: got %#v, want %#v", v, wantList)
	}

	// STRUCT with a LIST field recurses the other way.
	if err := c.QueryRowContext(ctx, "SELECT {'l': [1,2]}").Scan(&v); err != nil {
		t.Fatal(err)
	}
	wantNested := map[string]any{"l": []any{int64(1), int64(2)}}
	if !reflect.DeepEqual(v, wantNested) {
		t.Fatalf("struct-with-list result: got %#v, want %#v", v, wantNested)
	}

	// NULL struct cell decodes to nil; NULL field decodes to a nil map value.
	if err := c.QueryRowContext(ctx, "SELECT CAST(NULL AS STRUCT(a INT))").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != nil {
		t.Fatalf("NULL struct: got %#v, want nil", v)
	}
	if err := c.QueryRowContext(ctx, "SELECT {'a': NULL, 'b': 2}").Scan(&v); err != nil {
		t.Fatal(err)
	}
	wantNullField := map[string]any{"a": nil, "b": int64(2)}
	if !reflect.DeepEqual(v, wantNullField) {
		t.Fatalf("struct with NULL field: got %#v, want %#v", v, wantNullField)
	}

	t.Logf("STRUCT result columns scan as map[string]any (flat, nested, NULLs) ✓")
}

// TestStructResultApproxTopK: smoke the DuckDB-corpus shape that exposed the
// gap — approx_top_k returns a LIST of STRUCTs and previously scanned as
// [NULL, ...].
func TestStructResultApproxTopK(t *testing.T) {
	c, done := bandConn(t)
	defer done()
	var v any
	err := c.QueryRowContext(context.Background(),
		"SELECT approx_top_k({'i': i}, 2) FROM (SELECT 8 AS i FROM range(10))").Scan(&v)
	if err != nil {
		t.Skipf("approx_top_k unavailable in this build: %v", err)
	}
	l, ok := v.([]any)
	if !ok || len(l) == 0 {
		t.Fatalf("approx_top_k result: got %T %#v, want non-empty []any", v, v)
	}
	for i, e := range l {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("approx_top_k elem %d: got %T %#v, want map[string]any", i, e, e)
		}
		if m["i"] != int64(8) {
			t.Fatalf("approx_top_k elem %d: got %#v, want {'i': 8}", i, m)
		}
	}
	t.Logf("approx_top_k scans as []map[string]any: %v ✓", v)
}

// TestMapResultColumns: native MAP result columns scan as map[any]any (MAP is
// physically a LIST of {key, value} STRUCTs; decoding fell out of the STRUCT
// work).
func TestMapResultColumns(t *testing.T) {
	c, done := bandConn(t)
	defer done()
	ctx := context.Background()

	var v any
	if err := c.QueryRowContext(ctx, "SELECT MAP {'k1': 1, 'k2': 2}").Scan(&v); err != nil {
		t.Fatal(err)
	}
	want := map[any]any{"k1": int64(1), "k2": int64(2)}
	if !reflect.DeepEqual(v, want) {
		t.Fatalf("map result: got %T %#v, want %#v", v, v, want)
	}

	// NULL map and NULL value.
	if err := c.QueryRowContext(ctx, "SELECT CAST(NULL AS MAP(VARCHAR, INT))").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != nil {
		t.Fatalf("NULL map: got %#v, want nil", v)
	}
	if err := c.QueryRowContext(ctx, "SELECT MAP {'k': NULL}").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(v, map[any]any{"k": nil}) {
		t.Fatalf("map with NULL value: got %#v", v)
	}

	t.Logf("MAP result columns scan as map[any]any ✓")
}

// TestStructUDFArg: vecDecoder is shared with the UDF argument path, so STRUCT
// arguments now decode to map[string]any there too (they were nil before).
func TestStructUDFArg(t *testing.T) {
	mod := newModule()
	con, _, err := mod.open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	probe := func(args []any) (any, error) {
		m, ok := args[0].(map[string]any)
		if !ok {
			return fmt.Sprintf("BAD %T", args[0]), nil
		}
		return fmt.Sprintf("a=%v b=%v", m["a"], m["b"]), nil
	}
	if err := mod.registerScalarEx(con, "struct_probe", nil, dtVarchar, dtAny, true, false, probe); err != nil {
		t.Fatal(err)
	}
	res := mod.allocOut(sizeofDuckdbResult)
	sql := `SELECT struct_probe({'a': 7, 'b': 'q'})`
	if rc := mod.m.Xduckdb_query(con, mod.cstring(sql), res); rc != 0 {
		t.Fatalf("%s: %s", sql, mod.lastError())
	}
	if got := mod.goString(mod.m.Xduckdb_value_varchar(res, 0, 0)); got != "a=7 b=q" {
		t.Fatalf("struct UDF arg: got %q, want %q", got, "a=7 b=q")
	}
	t.Logf("STRUCT UDF argument decodes to map[string]any ✓")
}
