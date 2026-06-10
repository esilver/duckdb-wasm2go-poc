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
	"database/sql/driver"
	"reflect"

	convergeduckdb "duckdbconverge/duckdb"
)

// jsonNative parses one JSON-typed cell's text into the native Go value
// duckdb-go would deliver. The implementation lives in the engine package
// (DecodeJSONNative, also used for JSON RESULT columns); this thin alias
// keeps the compat call sites readable.
func jsonNative(s string) any { return convergeduckdb.DecodeJSONNative(s) }

// jsonAwareExecutor wraps a RowExecutor so JSONValue arguments arrive parsed,
// duckdb-go style, and the engine's ordered nested carriers (Struct/MapValue,
// engine nested.go) arrive as the Go maps duckdb-go delivers (STRUCT ->
// map[string]any, MAP -> map[any]any — googlesqlite's internal/value
// DecodeValue switches on exactly those shapes). The normalization recurses
// into []any arguments (LIST cells decode to []any and their elements can be
// JSON-alias cells — e.g. array_agg over a json_each column, the STRING_AGG
// lowering's shape) and into the carriers' field/entry values. Everything else
// passes through untouched.
func jsonAwareExecutor(fn func(values []driver.Value) (any, error)) func(values []driver.Value) (any, error) {
	return func(values []driver.Value) (any, error) {
		for i, v := range values {
			values[i] = normalizeJSONArg(v)
		}
		return fn(values)
	}
}

// normalizeJSONArg rewrites JSONValue nodes (recursively through []any and the
// nested carriers) to their parsed native form, and converts the engine's
// ordered Struct/MapValue carriers to duckdb-go's map shapes. A MapValue whose
// (normalized) keys are not all hashable cannot become a map[any]any; it passes
// through as the carrier (best effort — duckdb-go itself cannot scan such maps).
func normalizeJSONArg(v driver.Value) driver.Value {
	switch t := v.(type) {
	case convergeduckdb.JSONValue:
		return jsonNative(string(t))
	case []any:
		for i := range t {
			t[i] = normalizeJSONArg(t[i])
		}
		return t
	case convergeduckdb.Struct:
		m := make(map[string]any, len(t.Names))
		for i, n := range t.Names {
			m[n] = normalizeJSONArg(t.Values[i])
		}
		return m
	case convergeduckdb.MapValue:
		for i := range t.Keys {
			t.Keys[i] = normalizeJSONArg(t.Keys[i])
			t.Values[i] = normalizeJSONArg(t.Values[i])
		}
		m := make(map[any]any, len(t.Keys))
		for i, k := range t.Keys {
			if !hashableKey(k) {
				return t // unhashable key (LIST/STRUCT/...): keep the carrier
			}
			m[k] = t.Values[i]
		}
		return m
	}
	return v
}

// hashableKey reports whether a normalized MAP key can be used as a Go map key
// (decoded LIST/STRUCT/BLOB keys are slices/maps and would panic on insert).
func hashableKey(k any) bool {
	if k == nil {
		return true
	}
	return reflect.TypeOf(k).Comparable()
}
