// jsonnative.go — JSON-cell-to-native-Go conversion (duckdb-go scan semantics).
//
// JSON is VARCHAR-backed; the raw cell text is JSON-encoded ("abc" arrives
// quoted and escaped, objects as {"k":...} text, SQL-NULL-producing aggregates
// as the text "null"). duckdb-go delivers JSON RESULT columns (and the
// duckdbcompat layer delivers JSON UDF ARGUMENTS) as the PARSED native value:
// string unquoted, numbers as int64/float64, objects as map[string]any, arrays
// as []any, null as nil. The googlesqlite value layer depends on that — its
// envelope strings must arrive unquoted to base64-decode, and a JSON "null"
// must arrive as Go nil (an all-NULL ANY_VALUE over an UNNEST column otherwise
// scans as the literal string "null").
package duckdb

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// DecodeJSONNative parses one JSON cell's text into the native Go value
// duckdb-go would deliver. Text that is not valid JSON passes through as the
// raw string, so a value that was not actually JSON is left intact.
func DecodeJSONNative(s string) any {
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	dec.UseNumber() // keep integers exact; coerced below
	var v any
	if err := dec.Decode(&v); err != nil {
		return s
	}
	return coerceJSONNumbers(v)
}

// coerceJSONNumbers rewrites json.Number nodes (recursively) into int64 when
// the literal has no decimal point / exponent, else float64.
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
