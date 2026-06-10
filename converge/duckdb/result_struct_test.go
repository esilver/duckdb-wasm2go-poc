package duckdb

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

// TestStructResultColumns: native STRUCT result columns must scan as the
// ordered Struct carrier (declared field order — Go maps lose it and the
// sqllogic runner rendered fields sorted). Regression history: STRUCT cells
// first decoded to nil (the flat decode path had no STRUCT case), then to
// map[string]any (order lost).
func TestStructResultColumns(t *testing.T) {
	c, done := bandConn(t)
	defer done()
	ctx := context.Background()

	var v any
	if err := c.QueryRowContext(ctx, "SELECT {'b': 'x', 'a': 1}").Scan(&v); err != nil {
		t.Fatal(err)
	}
	// Declared order ('b' before 'a') must be preserved.
	want := Struct{Names: []string{"b", "a"}, Values: []any{"x", int64(1)}}
	if !reflect.DeepEqual(v, want) {
		t.Fatalf("struct result: got %T %#v, want %#v", v, v, want)
	}
	// Map() is the duckdb-go convenience shape.
	if m := v.(Struct).Map(); !reflect.DeepEqual(m, map[string]any{"a": int64(1), "b": "x"}) {
		t.Fatalf("Struct.Map(): got %#v", m)
	}

	// LIST of STRUCT recurses (the approx_top_k shape).
	if err := c.QueryRowContext(ctx, "SELECT [{'i': 1}, {'i': 2}]").Scan(&v); err != nil {
		t.Fatal(err)
	}
	wantList := []any{
		Struct{Names: []string{"i"}, Values: []any{int64(1)}},
		Struct{Names: []string{"i"}, Values: []any{int64(2)}},
	}
	if !reflect.DeepEqual(v, wantList) {
		t.Fatalf("list-of-struct result: got %#v, want %#v", v, wantList)
	}

	// STRUCT with a LIST field recurses the other way.
	if err := c.QueryRowContext(ctx, "SELECT {'l': [1,2]}").Scan(&v); err != nil {
		t.Fatal(err)
	}
	wantNested := Struct{Names: []string{"l"}, Values: []any{[]any{int64(1), int64(2)}}}
	if !reflect.DeepEqual(v, wantNested) {
		t.Fatalf("struct-with-list result: got %#v, want %#v", v, wantNested)
	}

	// NULL struct cell decodes to nil; NULL field decodes to a nil field value.
	if err := c.QueryRowContext(ctx, "SELECT CAST(NULL AS STRUCT(a INT))").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != nil {
		t.Fatalf("NULL struct: got %#v, want nil", v)
	}
	if err := c.QueryRowContext(ctx, "SELECT {'a': NULL, 'b': 2}").Scan(&v); err != nil {
		t.Fatal(err)
	}
	wantNullField := Struct{Names: []string{"a", "b"}, Values: []any{nil, int64(2)}}
	if !reflect.DeepEqual(v, wantNullField) {
		t.Fatalf("struct with NULL field: got %#v, want %#v", v, wantNullField)
	}

	t.Logf("STRUCT result columns scan as ordered Struct (flat, nested, NULLs) ✓")
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
		s, ok := e.(Struct)
		if !ok {
			t.Fatalf("approx_top_k elem %d: got %T %#v, want Struct", i, e, e)
		}
		if s.Map()["i"] != int64(8) {
			t.Fatalf("approx_top_k elem %d: got %#v, want {'i': 8}", i, s)
		}
	}
	t.Logf("approx_top_k scans as []Struct: %v ✓", v)
}

// TestMapResultColumns: native MAP result columns scan as the ordered MapValue
// carrier (MAP is physically a LIST of {key, value} STRUCTs). The carrier
// preserves DuckDB's entry order and admits unhashable keys, which the previous
// map[any]any shape could not (LIST-keyed maps decoded to nil).
func TestMapResultColumns(t *testing.T) {
	c, done := bandConn(t)
	defer done()
	ctx := context.Background()

	var v any
	if err := c.QueryRowContext(ctx, "SELECT MAP {'k2': 2, 'k1': 1}").Scan(&v); err != nil {
		t.Fatal(err)
	}
	// Entry order preserved ('k2' first).
	want := MapValue{Keys: []any{"k2", "k1"}, Values: []any{int64(2), int64(1)}}
	if !reflect.DeepEqual(v, want) {
		t.Fatalf("map result: got %T %#v, want %#v", v, v, want)
	}

	// Unhashable (LIST) keys are fine in the carrier.
	if err := c.QueryRowContext(ctx, "SELECT MAP {[1,2,3]: 'test'}").Scan(&v); err != nil {
		t.Fatal(err)
	}
	wantListKey := MapValue{
		Keys:   []any{[]any{int64(1), int64(2), int64(3)}},
		Values: []any{"test"},
	}
	if !reflect.DeepEqual(v, wantListKey) {
		t.Fatalf("list-keyed map result: got %T %#v, want %#v", v, v, wantListKey)
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
	if !reflect.DeepEqual(v, MapValue{Keys: []any{"k"}, Values: []any{nil}}) {
		t.Fatalf("map with NULL value: got %#v", v)
	}

	t.Logf("MAP result columns scan as ordered MapValue ✓")
}

// TestStructUDFArg: vecDecoder is shared with the UDF argument path, so STRUCT
// arguments decode to the ordered Struct carrier there too (they were nil
// before vecDecoder, then map[string]any).
func TestStructUDFArg(t *testing.T) {
	mod := newModule()
	con, _, err := mod.open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	probe := func(args []any) (any, error) {
		s, ok := args[0].(Struct)
		if !ok {
			return fmt.Sprintf("BAD %T", args[0]), nil
		}
		m := s.Map()
		return fmt.Sprintf("a=%v b=%v order=%v", m["a"], m["b"], s.Names), nil
	}
	if err := mod.registerScalarEx(con, "struct_probe", nil, dtVarchar, dtAny, true, false, probe); err != nil {
		t.Fatal(err)
	}
	res := mod.allocOut(sizeofDuckdbResult)
	sql := `SELECT struct_probe({'b': 'q', 'a': 7})`
	if rc := mod.m.Xduckdb_query(con, mod.cstring(sql), res); rc != 0 {
		t.Fatalf("%s: %s", sql, mod.lastError())
	}
	want := "a=7 b=q order=[b a]"
	if got := mod.goString(mod.m.Xduckdb_value_varchar(res, 0, 0)); got != want {
		t.Fatalf("struct UDF arg: got %q, want %q", got, want)
	}
	t.Logf("STRUCT UDF argument decodes to ordered Struct ✓")
}
