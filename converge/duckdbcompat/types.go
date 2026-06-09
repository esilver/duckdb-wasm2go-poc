// Package duckdbcompat is a pure-Go (CGO_ENABLED=0) stand-in for the subset of
// github.com/duckdb/duckdb-go/v2 that the GoogleSQL/BigQuery emulator
// (googlesqlite) consumes. Its exported surface is API-identical to that subset,
// so the emulator's 11 duckdb-go-touching files compile and run verbatim against
// our pure-Go DuckDB engine (import path duckdbconverge/duckdb) when the import
// path is repointed (or a go.mod replace is added).
//
// This file holds the type-carrier core (Type enum + TypeInfo) that the scalar,
// aggregate, and table UDF façades build on. See decimal.go and driver.go for the
// data-path core, and the façade files for the UDF registration surface.
package duckdbcompat

// Type is the compat mirror of duckdb-go's logical type enum. Its values are the
// numeric DuckDB type ids (the duckdb_type C enum), so a Type converts directly to
// the int32 duckdb_type id our engine's registration entry points expect.
type Type int

// The seven Type constants the emulator uses across the scalar, aggregate, and
// table UDF surfaces. Values are the verified DuckDB duckdb_type enum numbers.
//
// TYPE_ANY (34) is load-bearing: 451/456 scalar registrations are variadic over
// TYPE_ANY, and the value layer relies on ANY args arriving un-coerced.
const (
	TYPE_BOOLEAN   Type = 1  // DUCKDB_TYPE_BOOLEAN
	TYPE_BIGINT    Type = 5  // DUCKDB_TYPE_BIGINT
	TYPE_DOUBLE    Type = 11 // DUCKDB_TYPE_DOUBLE
	TYPE_TIMESTAMP Type = 12 // DUCKDB_TYPE_TIMESTAMP
	TYPE_VARCHAR   Type = 17 // DUCKDB_TYPE_VARCHAR
	TYPE_BLOB      Type = 18 // DUCKDB_TYPE_BLOB
	TYPE_ANY       Type = 34 // DUCKDB_TYPE_ANY
)

// TypeInfo is the opaque type carrier the emulator threads through
// ScalarFuncConfig (InputTypeInfos / ResultTypeInfo / VariadicTypeInfo) and
// ColumnInfo. It wraps exactly one Type. The emulator only ever constructs it via
// NewTypeInfo and never inspects its internals, so a single-field struct suffices.
//
// The zero value (TypeInfo{}) carries Type(0), which is DUCKDB_TYPE_INVALID. The
// façade treats a zero-value VariadicTypeInfo as "not variadic" (see the façade's
// non-positive / invalid handling), so a zero TypeInfo maps sanely to "no type".
type TypeInfo struct {
	t Type
}

// NewTypeInfo wraps t in a TypeInfo. It never fails (the error return matches
// duckdb-go's signature so call sites compile unchanged).
func NewTypeInfo(t Type) (TypeInfo, error) {
	return TypeInfo{t: t}, nil
}

// typeID returns the underlying DuckDB type id as the int32 the engine's
// registration entry points (duckdbconverge/duckdb RegisterScalarConn /
// RegisterAggregateConn) consume. It is unexported because only the façade files
// in this same package translate TypeInfo into engine type ids.
func (ti TypeInfo) typeID() int32 {
	return int32(ti.t)
}
