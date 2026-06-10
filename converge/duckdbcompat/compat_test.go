package duckdbcompat_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sort"
	"strings"
	"testing"

	convergeduckdb "duckdbconverge/duckdb" // registers the "duckdb" database/sql driver
	dc "github.com/duckdb/duckdb-go/v2"
)

// addOne is a fixed-arity scalar UDF defined exactly the way the googlesqlite
// emulator defines its UDFs (Config()/Executor() with TypeInfos), to prove the
// compat façade drives our pure-Go engine.
type addOne struct{}

func (addOne) Config() dc.ScalarFuncConfig {
	bi, _ := dc.NewTypeInfo(dc.TYPE_BIGINT)
	return dc.ScalarFuncConfig{
		InputTypeInfos:      []dc.TypeInfo{bi},
		ResultTypeInfo:      bi,
		SpecialNullHandling: true,
	}
}
func (addOne) Executor() dc.ScalarFuncExecutor {
	return dc.ScalarFuncExecutor{RowExecutor: func(vals []driver.Value) (any, error) {
		if vals[0] == nil {
			return nil, nil
		}
		return vals[0].(int64) + 1, nil
	}}
}

// argCount is a VARIADIC-ANY scalar UDF — the shape 451/456 of the emulator's
// scalar UDFs use (VariadicTypeInfo: TYPE_ANY). Returns the actual arg count.
type argCount struct{}

func (argCount) Config() dc.ScalarFuncConfig {
	bi, _ := dc.NewTypeInfo(dc.TYPE_BIGINT)
	anyT, _ := dc.NewTypeInfo(dc.TYPE_ANY)
	return dc.ScalarFuncConfig{
		ResultTypeInfo:      bi,
		VariadicTypeInfo:    anyT,
		SpecialNullHandling: true,
	}
}
func (argCount) Executor() dc.ScalarFuncExecutor {
	return dc.ScalarFuncExecutor{RowExecutor: func(vals []driver.Value) (any, error) {
		return int64(len(vals)), nil
	}}
}

func TestCompatScalarFacade(t *testing.T) {
	db, err := sql.Open("duckdb", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := dc.RegisterScalarUDF(c, "compat_add", addOne{}); err != nil {
		t.Fatalf("register fixed scalar: %v", err)
	}
	if err := dc.RegisterScalarUDF(c, "compat_argcount", argCount{}); err != nil {
		t.Fatalf("register variadic scalar: %v", err)
	}

	q := func(sql string) int64 {
		var v int64
		if err := c.QueryRowContext(context.Background(), sql).Scan(&v); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return v
	}
	if got := q("SELECT compat_add(41)"); got != 42 {
		t.Fatalf("compat_add(41)=%d want 42", got)
	}
	if got := q("SELECT compat_argcount(10, 20, 30, 'x', 5.5)"); got != 5 {
		t.Fatalf("compat_argcount(5 args)=%d want 5", got)
	}
	// NULL propagation through SpecialNullHandling
	var isNull bool
	if err := c.QueryRowContext(context.Background(), "SELECT compat_add(NULL) IS NULL").Scan(&isNull); err != nil {
		t.Fatalf("null query: %v", err)
	}
	if !isNull {
		t.Fatal("compat_add(NULL) should be NULL")
	}
	fmt.Println("compat façade OK: fixed scalar=42, variadic-ANY count=5, NULL propagated")
	t.Logf("compat façade end-to-end: scalar + variadic-ANY + NULL ✓")
}

// shapeProbe is a VARIADIC-ANY scalar UDF that reports the Go shape its first
// argument arrives as. It proves the compat layer converts the engine's ordered
// Struct/MapValue carriers to duckdb-go's map shapes (googlesqlite's
// internal/value.DecodeValue switches on map[string]any), except for maps with
// unhashable keys, which keep the carrier.
type shapeProbe struct{}

func (shapeProbe) Config() dc.ScalarFuncConfig {
	vc, _ := dc.NewTypeInfo(dc.TYPE_VARCHAR)
	anyT, _ := dc.NewTypeInfo(dc.TYPE_ANY)
	return dc.ScalarFuncConfig{
		ResultTypeInfo:      vc,
		VariadicTypeInfo:    anyT,
		SpecialNullHandling: true,
	}
}
func (shapeProbe) Executor() dc.ScalarFuncExecutor {
	return dc.ScalarFuncExecutor{RowExecutor: func(vals []driver.Value) (any, error) {
		switch v := vals[0].(type) {
		case map[string]any:
			keys := make([]string, 0, len(v))
			for k := range v {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := make([]string, len(keys))
			for i, k := range keys {
				parts[i] = fmt.Sprintf("%s=%v", k, v[k])
			}
			return "struct-map{" + strings.Join(parts, " ") + "}", nil
		case map[any]any:
			parts := make([]string, 0, len(v))
			for k, val := range v {
				parts = append(parts, fmt.Sprintf("%v=%v", k, val))
			}
			sort.Strings(parts)
			return "map-map{" + strings.Join(parts, " ") + "}", nil
		case convergeduckdb.MapValue:
			return fmt.Sprintf("carrier%v", v), nil
		case []any:
			parts := make([]string, len(v))
			for i, e := range v {
				parts[i] = fmt.Sprintf("%T", e)
			}
			return "list[" + strings.Join(parts, " ") + "]", nil
		default:
			return fmt.Sprintf("other:%T", vals[0]), nil
		}
	}}
}

// TestCompatNestedCarrierConversion: the jsonAware/normalize path must hand
// duckdb-go consumers map shapes for STRUCT and (hashable-key) MAP arguments,
// and pass the MapValue carrier through for unhashable keys.
func TestCompatNestedCarrierConversion(t *testing.T) {
	db, err := sql.Open("duckdb", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := dc.RegisterScalarUDF(c, "compat_shape", shapeProbe{}); err != nil {
		t.Fatalf("register shape probe: %v", err)
	}

	q := func(sql string) string {
		var v string
		if err := c.QueryRowContext(context.Background(), sql).Scan(&v); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return v
	}
	if got := q("SELECT compat_shape({'b': 'x', 'a': 1})"); got != "struct-map{a=1 b=x}" {
		t.Fatalf("struct arg: got %q", got)
	}
	if got := q("SELECT compat_shape(MAP {'k1': 1, 'k2': 2})"); got != "map-map{k1=1 k2=2}" {
		t.Fatalf("map arg: got %q", got)
	}
	// Unhashable (LIST) keys: the ordered carrier passes through.
	if got := q("SELECT compat_shape(MAP {[1,2]: 'v'})"); got != "carrier{[1 2]=v}" {
		t.Fatalf("list-keyed map arg: got %q", got)
	}
	// Nested: a struct inside a LIST converts element-wise.
	if got := q("SELECT compat_shape([{'a': 1}])"); got != "list[map[string]interface {}]" {
		t.Fatalf("list-of-struct arg shape: got %q", got)
	}
	t.Logf("compat nested-carrier conversion: STRUCT->map[string]any, MAP->map[any]any, unhashable keys keep carrier ✓")
}
