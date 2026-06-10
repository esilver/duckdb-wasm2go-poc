// nested.go — exported ordered carriers for nested STRUCT and MAP cells.
//
// STRUCT cells used to decode to map[string]any (duckdb-go's scan shape), which
// loses DuckDB's declared field order — renderers downstream (the sqllogic
// runner) had to sort keys and mismatched DuckDB's {declared order} output. MAP
// cells used to decode to map[any]any, which Go forbids for unhashable keys
// (MAP{[1,2,3]: 'test'} decoded to nil). Both decode paths (result rows and UDF
// arguments, see udf_vec.go) now deliver these order-preserving carriers
// instead. Consumers that want duckdb-go's map shapes convert explicitly
// (Struct.Map; the duckdbcompat layer does this for googlesqlite).
package duckdb

import (
	"fmt"
	"strings"
)

// Struct is the decoded form of a STRUCT cell: field names and values in
// DuckDB's declared order. Names and Values are parallel (len(Names) ==
// len(Values)); NULL fields carry a nil Value.
type Struct struct {
	Names  []string
	Values []any
}

// Map returns the fields as a map[string]any — duckdb-go's STRUCT scan shape —
// for consumers that key by field name and don't need declared order.
func (s Struct) Map() map[string]any {
	m := make(map[string]any, len(s.Names))
	for i, n := range s.Names {
		m[n] = s.Values[i]
	}
	return m
}

// String renders the struct DuckDB-style in declared order: {'a': 1, 'b': x}.
// Values render via fmt (nested Struct/MapValue recurse through their own
// String methods); it is a debug rendering, not DuckDB's exact VARCHAR cast.
func (s Struct) String() string {
	var sb strings.Builder
	sb.WriteByte('{')
	for i, n := range s.Names {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteByte('\'')
		sb.WriteString(n)
		sb.WriteString("': ")
		sb.WriteString(nestedDebugString(s.Values[i]))
	}
	sb.WriteByte('}')
	return sb.String()
}

// MapValue is the decoded form of a MAP cell: entries in the engine's order
// (DuckDB preserves map entry order). Keys and Values are parallel
// (len(Keys) == len(Values)). Unlike a Go map it admits unhashable keys
// (LIST/STRUCT-keyed maps) and preserves duplicate-free entry order.
type MapValue struct {
	Keys   []any
	Values []any
}

// String renders the map DuckDB-style in entry order: {k=v, k2=v2}. It is a
// debug rendering, not DuckDB's exact VARCHAR cast.
func (m MapValue) String() string {
	var sb strings.Builder
	sb.WriteByte('{')
	for i, k := range m.Keys {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(nestedDebugString(k))
		sb.WriteByte('=')
		sb.WriteString(nestedDebugString(m.Values[i]))
	}
	sb.WriteByte('}')
	return sb.String()
}

// nestedDebugString renders one nested value for the String methods (NULL for
// nil, fmt's default otherwise).
func nestedDebugString(v any) string {
	if v == nil {
		return "NULL"
	}
	return fmt.Sprint(v)
}
