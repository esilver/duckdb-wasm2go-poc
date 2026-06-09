package duckdbcompat

import (
	convergeduckdb "duckdbconverge/duckdb"
)

// Decimal is the compat mirror of duckdb-go's duckdb.Decimal. It is a TYPE
// ALIAS for the engine's Decimal — not a distinct type — because the engine
// driver delivers Decimal values directly on BOTH data paths (result-row scan
// in result.go and UDF argument decode in udf_vec.go), and the googlesqlite
// emulator type-switches on duckdb.Decimal (its row decoder calls String(),
// the geography/KLL argument normalizers call Float64()). With an alias the
// engine-made values match those switches with no conversion layer.
//
// Value is the unscaled integer; the represented number is Value / 10^Scale.
// Width is the total precision (digit count). String() renders the exact
// decimal text; Float64() collapses to float64 — both defined on the engine
// type (converge/duckdb/decimal.go).
type Decimal = convergeduckdb.Decimal
