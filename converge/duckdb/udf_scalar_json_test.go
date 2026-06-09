package duckdb

import (
	"fmt"
	"testing"
)

// TestScalarUDFJSONAliasWrapping: scalar-UDF argument cells coming from a
// JSON-alias column must arrive wrapped as JSONValue (carrying the raw JSON
// text), while plain VARCHAR arguments stay bare strings. This is what lets the
// duckdbcompat layer reproduce duckdb-go's scan-JSON-to-native-Go behavior —
// without it, the googlesqlite struct/UNNEST lowering (json_each feeding
// googlesqlite_get_struct_field) received quoted JSON text where the cgo
// backend's UDFs see the unquoted value, and every struct field decoded empty.
func TestScalarUDFJSONAliasWrapping(t *testing.T) {
	mod := newModule()
	con, _, err := mod.open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	echo := func(args []any) (any, error) {
		return fmt.Sprintf("%T|%v", args[0], args[0]), nil
	}
	if err := mod.registerScalarEx(con, "dbg_echo", nil, dtVarchar, dtAny, true, false, echo); err != nil {
		t.Fatal(err)
	}
	q := func(sql string) string {
		res := mod.allocOut(sizeofDuckdbResult)
		if rc := mod.m.Xduckdb_query(con, mod.cstring(sql), res); rc != 0 {
			t.Fatalf("%s: %s", sql, mod.lastError())
		}
		return mod.goString(mod.m.Xduckdb_value_varchar(res, 0, 0))
	}

	// Plain VARCHAR: bare string, no wrapping.
	if got := q(`SELECT dbg_echo('plain')`); got != "string|plain" {
		t.Fatalf("plain VARCHAR arg: got %q, want string|plain", got)
	}
	// JSON-alias column (json_each output): wrapped, raw JSON text preserved
	// (a JSON string element keeps its quotes — unquoting is the caller's call).
	if got := q(`SELECT dbg_echo(je.value) FROM json_each('["abc"]') je`); got != `duckdb.JSONValue|"abc"` {
		t.Fatalf("json_each string arg: got %q, want duckdb.JSONValue|\"abc\"", got)
	}
	if got := q(`SELECT dbg_echo(je.value) FROM json_each('[{"k":1}]') je`); got != `duckdb.JSONValue|{"k":1}` {
		t.Fatalf("json_each object arg: got %q, want duckdb.JSONValue|{\"k\":1}", got)
	}
	// An explicit ::VARCHAR cast strips the alias: bare string again.
	if got := q(`SELECT dbg_echo(je.value::VARCHAR) FROM json_each('["abc"]') je`); got != `string|"abc"` {
		t.Fatalf("cast-to-VARCHAR arg: got %q, want string|\"abc\"", got)
	}
	t.Logf("JSON-alias args wrapped as JSONValue; VARCHAR stays bare ✓")
}

// TestScalarUDFListArgs: LIST-typed arguments decode to []any of decoded child
// cells (recursively), with JSON-alias children wrapped as JSONValue.
// Regression: LIST args decoded to nil, and the googlesqlite STRING_AGG
// lowering (array_agg into googlesqlite_array_to_string(LIST, sep)) crashed on
// the nil and wedged the pool.
func TestScalarUDFListArgs(t *testing.T) {
	mod := newModule()
	con, _, err := mod.open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	echo := func(args []any) (any, error) {
		return fmt.Sprintf("%v", args[0]), nil
	}
	if err := mod.registerScalarEx(con, "dbg_list", nil, dtVarchar, dtAny, true, false, echo); err != nil {
		t.Fatal(err)
	}
	q := func(sql string) string {
		res := mod.allocOut(sizeofDuckdbResult)
		if rc := mod.m.Xduckdb_query(con, mod.cstring(sql), res); rc != 0 {
			t.Fatalf("%s: %s", sql, mod.lastError())
		}
		return mod.goString(mod.m.Xduckdb_value_varchar(res, 0, 0))
	}
	if got := q(`SELECT dbg_list(['a','b','c'])`); got != "[a b c]" {
		t.Fatalf("string list: got %q want [a b c]", got)
	}
	if got := q(`SELECT dbg_list([1,2,3])`); got != "[1 2 3]" {
		t.Fatalf("int list: got %q want [1 2 3]", got)
	}
	if got := q(`SELECT dbg_list([[1,2],[3]])`); got != "[[1 2] [3]]" {
		t.Fatalf("nested list: got %q want [[1 2] [3]]", got)
	}
	if got := q(`SELECT dbg_list(['x', NULL])`); got != "[x <nil>]" {
		t.Fatalf("list with NULL elem: got %q want [x <nil>]", got)
	}
	// array_agg over a json_each column: LIST of JSON-alias cells.
	if got := q(`SELECT dbg_list(list(je.value)) FROM json_each('["p","q"]') je`); got != `["p" "q"]` {
		t.Fatalf("list of JSON cells: got %q want [\"p\" \"q\"]", got)
	}
	t.Logf("LIST args decode: strings, ints, nested, NULL elem, JSON children ✓")
}
