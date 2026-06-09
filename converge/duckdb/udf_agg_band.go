// udf_agg_band.go — arity-band aggregate registration (the googlesqlite shape).
//
// The BigQuery emulator's aggregates register one NAME with a RANGE of arities
// (GoogleSQL's analyzer injects default trailing arguments, so e.g.
// KLL_QUANTILES.INIT_INT64(x) arrives as init_int64(x, 1000, 1)), every
// parameter typed ANY so the binder inserts no casts and the callback sees each
// argument's native form. RegisterAggregateUDF can't express that:
//
//   - DuckDB v1.5.3 cannot merge aggregate overloads by repeated registration —
//     duckdb_register_aggregate_function registers with ALTER_ON_CONFLICT, but
//     CreateAggregateFunctionInfo never implemented GetAlterInfo upstream, so
//     the second same-name registration throws "GetAlterInfo not implemented"
//     (see udf_agg_merge_check_test.go). All arities must therefore go into ONE
//     duckdb_aggregate_function_set registered in ONE call.
//   - Its update callback decodes by the DECLARED param types; ANY params need
//     the per-chunk runtime type resolution the scalar registrar already does.
//
// The six callbacks are injected ONCE per band and shared by every arity
// overload: the update callback reads the live column count from the input
// chunk, so the same closure serves all arities.
package duckdb

import (
	"fmt"
)

// dtAny is DUCKDB_TYPE_ANY — valid for PARAMETERS (binder matches any input
// without casting); the C API rejects it as a RETURN type. Lives here rather
// than result.go's enum block because no result column ever carries it.
const dtAny = 34

// JSONValue wraps a cell from a column whose logical type carries the "JSON"
// alias. DuckDB's JSON type is VARCHAR-backed, so the type id alone cannot
// distinguish a JSON column (whose scalars arrive JSON-encoded, e.g. a quoted
// and escaped envelope string from json_each/UNNEST) from a plain VARCHAR; the
// alias is the only signal. Callers type-switch on JSONValue and decide how to
// interpret the JSON text (the googlesqlite bridge unquotes scalar strings).
type JSONValue string

// AggregateOptions configures one RegisterAggregateBandConn registration.
type AggregateOptions struct {
	// MinArgs..MaxArgs (inclusive) is the arity band: one overload is added to
	// the function set per arity, each with that many ANY-typed parameters.
	MinArgs, MaxArgs int
	// ResultTypeID is the duckdb_type of the result column
	// (17=VARCHAR, 11=DOUBLE, 5=BIGINT).
	ResultTypeID int32
	// SingleState routes every input row to states[0] instead of scattering row
	// r to states[r]. Set it for aggregates that only ever run through DuckDB's
	// window path under OVER () (tf_idf, st_clusterdbscan): there the states
	// array holds a single frame accumulator that is shorter than the input
	// chunk, so the default per-row scatter would read past it. Plain GROUP BY
	// aggregates must leave this false (DuckDB fills states[] one entry per
	// input row, and multi-group chunks need the scatter).
	SingleState bool
}

// registerAggregateBand registers impl under `name` for every arity in
// [opts.MinArgs, opts.MaxArgs], parameters typed ANY, via the function-set API.
// NULL special handling is always on (NULL rows reach Update as nil args — the
// googlesqlite bodies own their NULL semantics). Argument cells decode by their
// RUNTIME column type, with two band-specific conventions on top of readCellT:
// DECIMAL arrives as float64 (matching the cgo bridge's decimal_to_double; the
// exact-string form readCellT produces is for the typed scalar path), and cells
// of JSON-alias columns arrive as JSONValue.
func (mod *module) registerAggregateBand(con int32, name string, opts AggregateOptions, impl AggregateImpl) error {
	m := mod.m
	if opts.MinArgs < 0 || opts.MaxArgs < opts.MinArgs {
		return fmt.Errorf("duckdb: aggregate %q: invalid arity band [%d, %d]", name, opts.MinArgs, opts.MaxArgs)
	}

	// state_size / init / combine / destroy: identical to RegisterAggregateUDF —
	// the blob is one int64 handle into the module's live-state table.
	stateSize := func(info int32) int64 { return 8 }
	initFn := func(info, state int32) {
		mod.writeU64(state, uint64(mod.newAgg(impl.NewState())))
	}
	combineFn := func(info, source, target int32, count int64) {
		for i := int64(0); i < count; i++ {
			hs := mod.readI64(mod.readPtr(source + int32(i*4)))
			hd := mod.readI64(mod.readPtr(target + int32(i*4)))
			impl.Combine(mod.aggState(hd), mod.aggState(hs))
		}
	}
	destroyFn := func(states int32, count int64) {
		for i := int64(0); i < count; i++ {
			blob := mod.readPtr(states + int32(i*4))
			if blob == 0 {
				continue
			}
			if h := mod.readI64(blob); h != 0 {
				mod.dropAgg(h)
			}
		}
	}

	// update: the ANY-typed band variant. Column count comes from the chunk (the
	// one closure serves every arity overload), and each column's decode metadata
	// is resolved per chunk via vecDecoder, mirroring registerScalarEx (runtime
	// types, JSON-alias wrapping as JSONValue, LIST cells as []any).
	updateFn := func(info, input, states int32) {
		n := m.Xduckdb_data_chunk_get_size(input)
		if n == 0 {
			return
		}
		nCols := int(m.Xduckdb_data_chunk_get_column_count(input))

		decs := make([]*vecDecoder, nCols)
		for c := 0; c < nCols; c++ {
			decs[c] = mod.newVecDecoder(m.Xduckdb_data_chunk_get_vector(input, int64(c)))
		}

		// Resolve the target state(s) once. In single-state mode every row folds
		// into states[0]'s accumulator (window path; see AggregateOptions).
		var singleSt any
		if opts.SingleState {
			singleSt = mod.aggState(mod.readI64(mod.readPtr(states)))
		}

		for r := int64(0); r < n; r++ {
			// args is freshly allocated per row: Update is allowed to retain it
			// (the googlesqlite bridge collects the slices for replay at finalize).
			args := make([]any, nCols)
			for c := 0; c < nCols; c++ {
				v := decs[c].cell(r)
				// Band convention: DECIMAL -> float64 (the aggregate bodies expect
				// the cgo bridge's decimal_to_double behavior, not the exact
				// Decimal carrier the scalar path delivers).
				if d, ok := v.(Decimal); ok {
					v = d.Float64()
				}
				args[c] = v
			}
			st := singleSt
			if !opts.SingleState {
				st = mod.aggState(mod.readI64(mod.readPtr(states + int32(r*4))))
			}
			impl.Update(st, args)
		}
	}

	finalizeFn := func(info, source, result int32, count, offset int64) {
		out := m.Xduckdb_vector_get_data(result)
		for i := int64(0); i < count; i++ {
			h := mod.readI64(mod.readPtr(source + int32(i*4)))
			v, ferr := impl.Finalize(mod.aggState(h))
			if ferr != nil {
				m.Xduckdb_aggregate_function_set_error(info, mod.cstring(fmt.Sprintf("duckdb: aggregate %q finalize: %v", name, ferr)))
				return
			}
			if err := mod.writeCell(opts.ResultTypeID, result, out, offset+i, v); err != nil {
				m.Xduckdb_aggregate_function_set_error(info, mod.cstring(fmt.Sprintf("duckdb: aggregate %q finalize: %v", name, err)))
				return
			}
		}
	}

	// Inject each callback ONCE; every arity overload shares the same indices.
	idxSize := mod.inject(stateSize)
	idxInit := mod.inject(initFn)
	idxUpdate := mod.inject(updateFn)
	idxCombine := mod.inject(combineFn)
	idxFinalize := mod.inject(finalizeFn)
	idxDestroy := mod.inject(destroyFn)

	// One function set carrying every arity, registered in a single CreateFunction
	// call (which therefore never hits the broken aggregate alter/merge path).
	// Handles are not destroyed afterwards — consistent with the rest of the
	// driver (registration is once-per-process-boot; the leak is a few dozen
	// small engine objects).
	set := m.Xduckdb_create_aggregate_function_set(mod.cstring(name))
	for p := opts.MinArgs; p <= opts.MaxArgs; p++ {
		af := m.Xduckdb_create_aggregate_function()
		m.Xduckdb_aggregate_function_set_name(af, mod.cstring(name))
		for i := 0; i < p; i++ {
			m.Xduckdb_aggregate_function_add_parameter(af, m.Xduckdb_create_logical_type(dtAny))
		}
		m.Xduckdb_aggregate_function_set_return_type(af, m.Xduckdb_create_logical_type(opts.ResultTypeID))
		// NULL rows must reach Update (the impls own their NULL semantics).
		m.Xduckdb_aggregate_function_set_special_handling(af)
		m.Xduckdb_aggregate_function_set_functions(af, idxSize, idxInit, idxUpdate, idxCombine, idxFinalize)
		m.Xduckdb_aggregate_function_set_destructor(af, idxDestroy)
		if rc := m.Xduckdb_add_aggregate_function_to_set(set, af); rc != 0 {
			return fmt.Errorf("duckdb: aggregate %q: add arity-%d overload to set failed (rc=%d): %s", name, p, rc, orUnknown(mod.lastError()))
		}
	}
	if rc := m.Xduckdb_register_aggregate_function_set(con, set); rc != 0 {
		return fmt.Errorf("duckdb: register aggregate set %q failed (rc=%d): %s", name, rc, orUnknown(mod.lastError()))
	}
	return nil
}
