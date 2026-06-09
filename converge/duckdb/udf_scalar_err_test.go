package duckdb

import (
	"errors"
	"strings"
	"testing"
)

// TestScalarUDFErrorPath: a scalar UDF returning a Go error must abort the
// query with that message (duckdb_scalar_function_set_error), not crash the
// engine. Regression: the googlesql TRANSLATE spec (error-raising case)
// panicked with a wild slice index while the resulting C++ exception unwound.
func TestScalarUDFErrorPath(t *testing.T) {
	mod := newModule()
	con, _, err := mod.open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	boom := func(args []any) (any, error) {
		return nil, errors.New("scalar-boom: duplicated source character")
	}
	if err := mod.registerScalarEx(con, "dbg_boom", nil, dtVarchar, dtAny, true, false, boom); err != nil {
		t.Fatal(err)
	}
	res := mod.allocOut(sizeofDuckdbResult)
	rc := mod.m.Xduckdb_query(con, mod.cstring("SELECT dbg_boom('x')"), res)
	if rc == 0 {
		t.Fatalf("query unexpectedly succeeded")
	}
	// duckdb_result_error can be empty here (the convert-and-rethrow path eats
	// the message); the driver's real error path falls back to the captured
	// throw message - use the same fallback.
	msg := mod.goString(mod.m.Xduckdb_result_error(res))
	if msg == "" {
		msg = mod.lastError()
	}
	t.Logf("query failed as expected: %s", msg)
	if !strings.Contains(msg, "scalar-boom") {
		t.Fatalf("error message lost: %q", msg)
	}
	// The engine must still be usable afterwards.
	res2 := mod.allocOut(sizeofDuckdbResult)
	if rc := mod.m.Xduckdb_query(con, mod.cstring("SELECT 41+1"), res2); rc != 0 {
		t.Fatalf("engine unusable after scalar error: %s", mod.lastError())
	}
	if got := mod.m.Xduckdb_value_int64(res2, 0, 0); got != 42 {
		t.Fatalf("post-error query: got %d want 42", got)
	}
	t.Logf("engine alive after scalar error ✓")
}
