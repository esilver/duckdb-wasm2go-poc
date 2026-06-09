package duckdb

import (
	"context"
	"fmt"
	"testing"
)

// TestListResultColumns: native LIST result columns (a bare list()/ARRAY_AGG in
// the SELECT) must scan as []any of decoded elements, duckdb-go style.
// Regression: they decoded to nil (the flat decode path had no LIST case), so
// the BigQuery emulator's top-level ARRAY_AGG cells came back empty.
func TestListResultColumns(t *testing.T) {
	c, done := bandConn(t)
	defer done()
	var v any
	if err := c.QueryRowContext(context.Background(),
		"SELECT list(x ORDER BY x) FROM range(3) t(x)").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%v", v); got != "[0 1 2]" {
		t.Fatalf("int list result: got %T %q, want [0 1 2]", v, got)
	}
	if err := c.QueryRowContext(context.Background(),
		"SELECT list(s ORDER BY s) FROM (VALUES ('a'), ('b')) t(s)").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%v", v); got != "[a b]" {
		t.Fatalf("string list result: got %T %q, want [a b]", v, got)
	}
	// Nested list + NULL element survive.
	if err := c.QueryRowContext(context.Background(),
		"SELECT [[1,2],[3]]").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%v", v); got != "[[1 2] [3]]" {
		t.Fatalf("nested list result: got %q", got)
	}
	if err := c.QueryRowContext(context.Background(),
		"SELECT ['x', NULL]").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%v", v); got != "[x <nil>]" {
		t.Fatalf("list with NULL elem: got %q", got)
	}
	t.Logf("LIST result columns scan as []any (int, string, nested, NULL elem) ✓")
}
