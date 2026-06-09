// decimal.go — the exact-DECIMAL carrier type.
//
// DuckDB DECIMAL cells (columns, and numeric literals like 1.5 or 2.0, which
// DuckDB binds as DECIMAL rather than DOUBLE) are delivered as a Decimal: the
// unscaled integer plus scale/width. This mirrors duckdb-go's duckdb.Decimal —
// the googlesqlite emulator type-switches on that exact type in its row decoder
// (internal/decoder.go: Decimal -> NumericValue via String()) and its UDF
// argument normalizers (geo/KLL: Decimal -> Float64()) — so the duckdbcompat
// package aliases this type as its Decimal. It lives in the engine package
// because BOTH delivery paths originate here: the database/sql result decode
// (result.go) and the UDF argument decode (udf_vec.go).
package duckdb

import (
	"math/big"
	"strings"
)

// Decimal is an exact DECIMAL value: the represented number is Value / 10^Scale.
// Width is the total precision (digit count), carried for fidelity but not
// consulted by String()/Float64(). Field names/types match duckdb-go.
type Decimal struct {
	Width uint8
	Scale uint8
	Value *big.Int
}

// String renders the exact decimal value with a single '.' separating the
// integer and fractional parts. Exact (no rounding), so NUMERIC consumers can
// parse it losslessly into a big.Rat.
//
//	{Value: 150,  Scale: 2} -> "1.50"
//	{Value: -5,   Scale: 3} -> "-0.005"
//	{Value: 42,   Scale: 0} -> "42"
//	{Value: nil}            -> "0"
func (d Decimal) String() string {
	if d.Value == nil {
		return "0"
	}
	if d.Scale == 0 {
		return d.Value.String()
	}
	neg := d.Value.Sign() < 0
	digits := new(big.Int).Abs(d.Value).String()
	// Pad so there is at least one integer digit before the point.
	for len(digits) <= int(d.Scale) {
		digits = "0" + digits
	}
	cut := len(digits) - int(d.Scale)
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	b.WriteString(digits[:cut])
	b.WriteByte('.')
	b.WriteString(digits[cut:])
	return b.String()
}

// Float64 returns the decimal as a float64 (Value / 10^Scale), evaluated as a
// big.Float quotient so large unscaled values don't lose magnitude before the
// final conversion. A nil Value yields 0.
func (d Decimal) Float64() float64 {
	if d.Value == nil {
		return 0
	}
	num := new(big.Float).SetInt(d.Value)
	if d.Scale == 0 {
		f, _ := num.Float64()
		return f
	}
	denom := new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(d.Scale)), nil))
	f, _ := new(big.Float).Quo(num, denom).Float64()
	return f
}
