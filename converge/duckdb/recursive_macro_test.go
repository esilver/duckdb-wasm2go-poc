package duckdb

import (
	"context"
	"strings"
	"testing"
)

// TestSqllogicRecursiveMacroDepth: binding a self-recursive macro must fail
// cleanly with DuckDB's "Max expression depth limit" BinderException, not
// crash the engine. Regression for the "recursive-macro-shadow-stack-overflow"
// panic group (duckdb-src/test/sql/catalog/function/test_recursive_macro.test
// and test_recursive_macro_no_dependency.test):
//
//	CREATE MACRO "sum"(x) AS (CASE WHEN sum(x) IS NULL THEN 0 ELSE sum(x) END);
//	SELECT sum(1); -- panicked: slice bounds out of range [1852399989:23003136]
//
// DuckDB's depth guard is a LOGICAL counter (ExpressionBinder::StackCheck,
// fires at max_expression_depth=1000), which needs ~1.5MB of C shadow stack to
// reach — but the wasm build ships a 64KB shadow stack. Binder recursion ran
// the stack pointer down into the data segment (silently; wasm stack overflow
// doesn't trap), and a corrupted constant (ASCII garbage = 1852399989) was
// later used as a slice bound inside Xduckdb_prepare. The fix relocates the
// shadow stack onto a large malloc'd block at module init (module.go) so the
// logical guard fires first.
func TestSqllogicRecursiveMacroDepth(t *testing.T) {
	c, done := bandConn(t)
	defer done()
	ctx := context.Background()

	exec := func(q string) {
		t.Helper()
		if _, err := c.ExecContext(ctx, q); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	expectDepthErr := func(q string) {
		t.Helper()
		_, err := c.ExecContext(ctx, q)
		if err == nil {
			t.Fatalf("%q: expected 'Max expression depth limit' error, got success", q)
		}
		if !strings.Contains(err.Error(), "Max expression depth limit") {
			t.Fatalf("%q: expected 'Max expression depth limit' error, got: %v", q, err)
		}
		// KNOWN separate driver issue (worked around the same way by
		// cmd/sqllogic): a failed statement leaks the autocommit transaction;
		// a sacrificial ROLLBACK clears it.
		_, _ = c.ExecContext(ctx, "ROLLBACK")
	}

	// test_recursive_macro_no_dependency.test (default settings).
	exec(`CREATE MACRO "sum"(x) AS (CASE WHEN sum(x) IS NULL THEN 0 ELSE sum(x) END)`)
	expectDepthErr("SELECT sum(1)")
	expectDepthErr("SELECT sum(1) WHERE 42=0")
	exec("DROP MACRO sum")

	// The engine must still be healthy after the depth errors: a macro that
	// recurses into the QUALIFIED built-in resolves and runs.
	exec(`CREATE MACRO "sum"(x) AS (CASE WHEN system.main.sum(x) IS NULL THEN 0 ELSE system.main.sum(x) END)`)
	var v int64
	if err := c.QueryRowContext(ctx, "SELECT sum(1)").Scan(&v); err != nil {
		t.Fatalf("SELECT sum(1) after qualified macro: %v", err)
	}
	if v != 1 {
		t.Fatalf("SELECT sum(1) = %d, want 1", v)
	}
	if err := c.QueryRowContext(ctx, "SELECT sum(1) WHERE 42=0").Scan(&v); err != nil {
		t.Fatalf("SELECT sum(1) WHERE 42=0: %v", err)
	}
	if v != 0 {
		t.Fatalf("SELECT sum(1) WHERE 42=0 = %d, want 0", v)
	}
	exec("DROP MACRO sum")

	// test_recursive_macro.test variant: same shape with macro dependency
	// tracking enabled.
	exec("SET enable_macro_dependencies=true")
	exec(`CREATE MACRO "sum"(x) AS (CASE WHEN sum(x) IS NULL THEN 0 ELSE sum(x) END)`)
	expectDepthErr("SELECT sum(1)")
	exec("DROP MACRO sum")
}
