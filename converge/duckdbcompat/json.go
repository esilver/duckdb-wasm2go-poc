// json.go — duckdb-go JSON-argument semantics for the compat façade.
//
// The engine (duckdbconverge/duckdb) wraps scalar/aggregate UDF argument cells
// that come from JSON-alias columns as convergeduckdb.JSONValue, because JSON is
// VARCHAR-backed and the raw cell text is JSON-encoded ("abc" arrives quoted and
// escaped, objects arrive as {"k":...} text). duckdb-go delivers such arguments
// to a RowExecutor as the PARSED native Go value (string unquoted, numbers as
// int64/float64, objects as map[string]any, arrays as []any, null as nil) — the
// googlesqlite value layer depends on that (its envelope strings must arrive
// unquoted to base64-decode; see internal/value.DecodeValue). This file restores
// that behavior for the pure-Go engine.
package duckdbcompat

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"strconv"
	"strings"

	convergeduckdb "duckdbconverge/duckdb"
)

// jsonNative parses one JSON-typed cell's text into the native Go value
// duckdb-go would deliver. Invalid JSON passes through as the raw string so a
// value that was not actually JSON is left intact.
func jsonNative(s string) any {
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	dec.UseNumber() // keep integers exact; coerced below
	var v any
	if err := dec.Decode(&v); err != nil {
		return s
	}
	return coerceJSONNumbers(v)
}

// coerceJSONNumbers rewrites json.Number nodes (recursively) into int64 when the
// literal has no decimal point / exponent, else float64 — the widths duckdb-go
// (and database/sql driver.Value) traffic in.
func coerceJSONNumbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		s := string(t)
		if !strings.ContainsAny(s, ".eE") {
			if i, err := strconv.ParseInt(s, 10, 64); err == nil {
				return i
			}
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
		return s
	case []any:
		for i := range t {
			t[i] = coerceJSONNumbers(t[i])
		}
		return t
	case map[string]any:
		for k := range t {
			t[k] = coerceJSONNumbers(t[k])
		}
		return t
	}
	return v
}

// jsonAwareExecutor wraps a RowExecutor so JSONValue arguments arrive parsed,
// duckdb-go style. The normalization recurses into []any arguments (LIST cells
// decode to []any and their elements can be JSON-alias cells — e.g. array_agg
// over a json_each column, the STRING_AGG lowering's shape). Non-JSON
// arguments pass through untouched.
func jsonAwareExecutor(fn func(values []driver.Value) (any, error)) func(values []driver.Value) (any, error) {
	return func(values []driver.Value) (any, error) {
		for i, v := range values {
			values[i] = normalizeJSONArg(v)
		}
		return fn(values)
	}
}

// normalizeJSONArg rewrites JSONValue nodes (recursively through []any) to
// their parsed native form.
func normalizeJSONArg(v driver.Value) driver.Value {
	switch t := v.(type) {
	case convergeduckdb.JSONValue:
		return jsonNative(string(t))
	case []any:
		for i := range t {
			t[i] = normalizeJSONArg(t[i])
		}
		return t
	}
	return v
}
