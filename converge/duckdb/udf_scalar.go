// udf_scalar.go — the generic SCALAR UDF registrar.
//
// This generalizes the proven, hand-rolled spike in udf_test.go
// (my_add_one(BIGINT)->BIGINT) into a reusable registrar that takes an arbitrary
// Go function `fn(args []any) (any, error)` and exposes it to DuckDB SQL under a
// chosen name and signature. The mechanism is identical to the spike: a
// func(int32,int32,int32) callback closure is appended to the engine's live
// indirect-function table (via mod.inject) and its index is handed to
// duckdb_scalar_function_set_function as the C "function pointer"; DuckDB
// call_indirects it per data chunk during query execution.
//
// Per invocation the callback reads the input data chunk's row count, decodes each
// input column's cell for the current row into a Go value (via the shared codec's
// readCell), calls fn, and encodes the result back into the output vector (via
// writeCell). A returned error is surfaced to DuckDB via duckdb_scalar_function_set_error,
// which aborts the query with that message.
package duckdb

import "fmt"

// RegisterScalarUDF registers fn as a DuckDB scalar function `name` on connection
// con, with parameter types paramTypeIDs (duckdb_type enum values, in order) and
// return type retTypeID. fn receives one decoded Go value per parameter (the
// driver.Value kinds: bool, int64, float64, string, []byte, time.Time, nil) and
// returns the Go result value (or nil for SQL NULL) plus an optional error; a
// non-nil error aborts the query with that message.
//
// This is the fixed-arity, no-frills entry point: no varargs, no special NULL
// handling, not volatile. It delegates to registerScalarEx, which carries the full
// feature set the compat layer needs.
func (mod *module) RegisterScalarUDF(con int32, name string, paramTypeIDs []int32, retTypeID int32, fn func(args []any) (any, error)) error {
	return mod.registerScalarEx(con, name, paramTypeIDs, retTypeID, -1, false, false, fn)
}

// registerScalarEx is the general scalar registrar. Beyond RegisterScalarUDF it
// wires three pieces of registration metadata the googlesqlite emulator depends on:
//
//   - varargsTypeID >= 0 makes the function variadic over that trailing type
//     (duckdb_scalar_function_set_varargs). 451/456 emulator UDFs are variadic over
//     DUCKDB_TYPE_ANY (id 34). varargsTypeID < 0 means non-variadic (fixed arity).
//   - specialNull (duckdb_scalar_function_set_special_handling) stops DuckDB from
//     short-circuiting rows with NULL args; the UDF is invoked for NULL inputs and
//     decides NULL semantics itself. Every emulator UDF sets this.
//   - volatile (duckdb_scalar_function_set_volatile) marks the result
//     non-deterministic so the optimizer never constant-folds it — required for
//     correctness of UUID / crypto / keyset functions.
//
// Variadic dispatch: because the call's actual argument count is NOT len(paramTypeIDs)
// (the variadic tail expands per call site), the callback reads the live column count
// from the input chunk (duckdb_data_chunk_get_column_count) and decodes EACH column
// with its runtime type id (duckdb_get_type_id of the vector's column logical type).
// This is essential for ANY-typed varargs, whose concrete type is only known per call.
// DECIMAL columns additionally read their scale + internal storage type from the
// per-column logical type so DECIMAL args decode to an exact decimal string (readCellT)
// rather than nil.
func (mod *module) registerScalarEx(con int32, name string, paramTypeIDs []int32, retTypeID int32, varargsTypeID int32, specialNull, volatile bool, fn func(args []any) (any, error)) error {
	m := mod.m

	// The per-chunk callback. Signature MUST be func(int32,int32,int32) to match
	// the engine's call_indirect type assertion (info, input_chunk, output_vector).
	cb := func(info, input, output int32) {
		n := m.Xduckdb_data_chunk_get_size(input)
		// Read the ACTUAL per-call argument count from the chunk, not len(paramTypeIDs):
		// a variadic call passes more columns than the declared fixed params.
		nCols := int(m.Xduckdb_data_chunk_get_column_count(input))

		// Snapshot each input column's vector data buffer, validity mask, and runtime
		// type metadata once per chunk (all stable for the chunk's lifetime), then
		// decode row-by-row. The runtime type id (from the vector's column logical
		// type) is used instead of the declared paramTypeIDs so ANY-typed varargs and
		// any implicit coercions decode correctly. DECIMAL columns also capture scale +
		// internal storage type for exact decoding via readCellT.
		dataPtrs := make([]int32, nCols)
		validPtrs := make([]int32, nCols)
		colTypes := make([]int32, nCols)
		colScales := make([]int32, nCols)
		colInternals := make([]int32, nCols)
		colJSON := make([]bool, nCols)
		for c := 0; c < nCols; c++ {
			vec := m.Xduckdb_data_chunk_get_vector(input, int64(c))
			dataPtrs[c] = m.Xduckdb_vector_get_data(vec)
			validPtrs[c] = m.Xduckdb_vector_get_validity(vec)
			// duckdb_vector_get_column_type allocates a fresh logical type handle that
			// must be destroyed (same contract as result.go's column types).
			lt := m.Xduckdb_vector_get_column_type(vec)
			colTypes[c] = m.Xduckdb_get_type_id(lt)
			colInternals[c] = dtBigint
			if colTypes[c] == dtDecimal {
				colScales[c] = m.Xduckdb_decimal_scale(lt)
				colInternals[c] = m.Xduckdb_decimal_internal_type(lt)
			}
			if colTypes[c] == dtVarchar {
				// JSON columns are VARCHAR-backed; the alias is the only signal. Cells
				// arrive wrapped as JSONValue so callers (the duckdbcompat layer, which
				// mimics duckdb-go's scan-JSON-to-native-Go behavior) can tell JSON text
				// apart from a plain string. Mirrors registerAggregateBand.
				if ap := m.Xduckdb_logical_type_get_alias(lt); ap != 0 {
					colJSON[c] = mod.goString(ap) == "JSON"
					m.Xduckdb_free(ap)
				}
			}
			destroyLogicalType(mod, lt)
		}

		outData := m.Xduckdb_vector_get_data(output)
		args := make([]any, nCols)

		for r := int64(0); r < n; r++ {
			for c := 0; c < nCols; c++ {
				v := mod.readCellT(colTypes[c], colScales[c], colInternals[c], dataPtrs[c], validPtrs[c], r)
				if colJSON[c] {
					if s, ok := v.(string); ok {
						v = JSONValue(s)
					}
				}
				args[c] = v
			}
			res, err := fn(args)
			if err != nil {
				// Surface the error to DuckDB; it aborts the query with this message.
				m.Xduckdb_scalar_function_set_error(info, mod.cstring(err.Error()))
				return
			}
			if werr := mod.writeCell(retTypeID, output, outData, r, res); werr != nil {
				m.Xduckdb_scalar_function_set_error(info, mod.cstring(werr.Error()))
				return
			}
		}
	}

	// Inject the closure into the live indirect table; its index is the fn ptr.
	idx := mod.inject(cb)

	// Build + register the scalar function.
	sf := m.Xduckdb_create_scalar_function()
	namePtr := mod.cstring(name)
	m.Xduckdb_scalar_function_set_name(sf, namePtr)
	for _, t := range paramTypeIDs {
		m.Xduckdb_scalar_function_add_parameter(sf, m.Xduckdb_create_logical_type(t))
	}
	m.Xduckdb_scalar_function_set_return_type(sf, m.Xduckdb_create_logical_type(retTypeID))
	if varargsTypeID >= 0 {
		m.Xduckdb_scalar_function_set_varargs(sf, m.Xduckdb_create_logical_type(varargsTypeID))
	}
	if specialNull {
		m.Xduckdb_scalar_function_set_special_handling(sf)
	}
	if volatile {
		m.Xduckdb_scalar_function_set_volatile(sf)
	}
	m.Xduckdb_scalar_function_set_function(sf, idx)
	if rc := m.Xduckdb_register_scalar_function(con, sf); rc != 0 {
		return fmt.Errorf("duckdb: register scalar UDF %q failed (rc=%d): %s", name, rc, orUnknown(mod.lastError()))
	}
	return nil
}

// destroyLogicalType frees a duckdb_logical_type handle. duckdb_destroy_logical_type
// takes a pointer to the handle, so the value is staged in a scratch memory slot
// (the same pattern result.go uses for column logical types).
func destroyLogicalType(mod *module, lt int32) {
	slot := mod.allocOut(4)
	mod.writeU32(slot, uint32(lt))
	mod.m.Xduckdb_destroy_logical_type(slot)
	mod.free(slot)
}
