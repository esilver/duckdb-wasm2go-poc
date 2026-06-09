package duckdbcompat

import (
	"math/big"
	"strings"
)

// Decimal is the compat mirror of duckdb-go's duckdb.Decimal. It is the one
// concrete value type that crosses the emulator's data path: the row decoder
// (internal/decoder.go) calls String(), and the geography / KLL UDF argument
// normalizers (duckdb_geo_udf.go, duckdb_aggregate_kll_extract.go) call Float64()
// to collapse DECIMAL-bound numeric literals (e.g. the 1.5 in ST_GEOGPOINT(1.5,
// 2.5)) to float64.
//
// Value is the unscaled integer; the represented number is Value / 10^Scale.
// Width is the total precision (digit count) and is carried for fidelity but not
// consulted by String()/Float64().
type Decimal struct {
	Width uint8
	Scale uint8
	Value *big.Int
}

// String renders the exact decimal value (Value scaled by Scale) as a textual
// number with a single '.' separating the integer and fractional parts. The
// output is exact (no rounding) so the emulator's decoder can parse it losslessly
// into a big.Rat for NUMERIC columns. This mirrors the engine's formatDecimal
// (converge/duckdb/result.go) so DECIMAL result columns and DECIMAL scalar args
// stringify identically.
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

// Float64 returns the decimal as a float64 (Value / 10^Scale). Precision is
// limited to float64; the emulator only uses this for geography/quantile rank
// literals where float64 is the target type anyway. A nil Value yields 0.
func (d Decimal) Float64() float64 {
	if d.Value == nil {
		return 0
	}
	// numerator (the unscaled integer) over denominator 10^Scale, evaluated as a
	// big.Rat so large unscaled values don't lose magnitude before the final
	// float conversion.
	num := new(big.Float).SetInt(d.Value)
	if d.Scale == 0 {
		f, _ := num.Float64()
		return f
	}
	denom := new(big.Float).SetInt(pow10(int(d.Scale)))
	f, _ := new(big.Float).Quo(num, denom).Float64()
	return f
}

// pow10 returns 10^n as a big.Int.
func pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}
