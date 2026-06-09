package duckdbcompat_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"testing"

	_ "duckdbconverge/duckdb" // registers the "duckdb" database/sql driver
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
