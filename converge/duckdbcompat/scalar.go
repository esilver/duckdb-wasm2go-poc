// scalar.go — the scalar-UDF façade (SWAP-BLUEPRINT §1.2), API-identical to the
// subset of github.com/duckdb/duckdb-go/v2 the googlesqlite emulator consumes.
//
// The emulator declares ~17 structs that implement ScalarFunc implicitly and
// registers them via RegisterScalarUDF; it sets ScalarFuncConfig fields by name
// (InputTypeInfos / ResultTypeInfo / VariadicTypeInfo / SpecialNullHandling /
// Volatile) and supplies a RowExecutor whose signature is exactly
// func([]driver.Value) (any, error). This façade translates that config into the
// int32 duckdb_type ids + flags our engine's RegisterScalarConn entry point
// (duckdbconverge/duckdb, udf_register.go) consumes, then hands the RowExecutor
// straight through — its signature matches RegisterScalarConn's fn parameter
// verbatim, so no adapter closure is needed.
package duckdbcompat

import (
	"database/sql"
	"database/sql/driver"

	convergeduckdb "duckdbconverge/duckdb"
)

// ScalarFunc is the compat mirror of duckdb-go's duckdb.ScalarFunc. The emulator's
// UDF structs satisfy it implicitly by exposing Config and Executor. We never call
// either method more than once per registration.
type ScalarFunc interface {
	Config() ScalarFuncConfig
	Executor() ScalarFuncExecutor
}

// ScalarFuncConfig is the compat mirror of duckdb-go's duckdb.ScalarFuncConfig.
// All five exported fields (names and types) match duckdb-go exactly because the
// emulator sets them positionally-by-name at its registration sites:
//
//   - InputTypeInfos: the fixed (leading) parameter types, in order.
//   - ResultTypeInfo: the result type.
//   - VariadicTypeInfo: the trailing variadic element type. In duckdb-go this field
//     is an interface whose nil value means "not variadic"; here it is a value-type
//     TypeInfo, so the zero value (TypeInfo{}, carrying Type(0) =
//     DUCKDB_TYPE_INVALID) is the "not variadic" sentinel — see RegisterScalarUDF.
//   - SpecialNullHandling: invoke the function for NULL args instead of
//     short-circuiting (the emulator sets this true on ~all UDFs so it can decide
//     NULL semantics itself).
//   - Volatile: mark the result non-deterministic (no constant-folding).
type ScalarFuncConfig struct {
	InputTypeInfos      []TypeInfo
	ResultTypeInfo      TypeInfo
	VariadicTypeInfo    TypeInfo
	SpecialNullHandling bool
	Volatile            bool
}

// ScalarFuncExecutor is the compat mirror of duckdb-go's duckdb.ScalarFuncExecutor.
// RowExecutor is invoked once per result row with one decoded value per ACTUAL call
// argument (fixed params followed by the expanded variadic tail) and returns the
// result (nil for SQL NULL) plus an optional error. Its signature is identical to
// the fn parameter of RegisterScalarConn, so it is forwarded unwrapped.
type ScalarFuncExecutor struct {
	RowExecutor func(values []driver.Value) (any, error)
}

// RegisterScalarUDF registers f under name on the engine behind con. It mirrors
// duckdb-go's duckdb.RegisterScalarUDF (first param *sql.Conn, exact).
//
// It reads f.Config() once, translates it to the engine's int32 type ids + flags,
// and delegates to convergeduckdb.RegisterScalarConn, which performs the Conn.Raw
// unwrap + engine-lock + catalog-scoped registration.
func RegisterScalarUDF(con *sql.Conn, name string, f ScalarFunc) error {
	cfg := f.Config()

	// Fixed parameter type ids, in order. nil InputTypeInfos yields an empty
	// (non-nil-required) slice, which the engine treats as a 0-arity fixed signature.
	paramTypeIDs := make([]int32, len(cfg.InputTypeInfos))
	for i, ti := range cfg.InputTypeInfos {
		paramTypeIDs[i] = ti.typeID()
	}

	retTypeID := cfg.ResultTypeInfo.typeID()

	// Decide whether the function is variadic. VariadicTypeInfo is a value-type
	// TypeInfo, so "set" is encoded by a meaningful (positive) duckdb_type id:
	//
	//   - Unset/zero value (TypeInfo{}) -> typeID() == 0 (DUCKDB_TYPE_INVALID).
	//   - duckdb_type ids are all positive (BOOLEAN=1 ... ANY=34), and the emulator
	//     only ever sets VariadicTypeInfo via NewTypeInfo(TYPE_ANY) (451/456 sites)
	//     — never with the id-0 INVALID type — so a non-positive id unambiguously
	//     means "not variadic".
	//
	// RegisterScalarConn's contract is: varargsTypeID >= 0 => variadic over that
	// type; < 0 => non-variadic. We therefore pass the id when it is > 0 and the -1
	// sentinel otherwise (guarding against an accidental 0 reaching the engine,
	// which it would otherwise misread as a valid variadic type).
	varargsTypeID := int32(-1)
	if vid := cfg.VariadicTypeInfo.typeID(); vid > 0 {
		varargsTypeID = vid
	}

	// RowExecutor's signature (func([]driver.Value) (any, error)) is exactly
	// RegisterScalarConn's fn parameter type — pass it through with no wrapper.
	return convergeduckdb.RegisterScalarConn(
		con,
		name,
		paramTypeIDs,
		retTypeID,
		varargsTypeID,
		cfg.SpecialNullHandling,
		cfg.Volatile,
		f.Executor().RowExecutor,
	)
}
