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
