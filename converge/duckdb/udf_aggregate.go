// udf_aggregate.go — generic aggregate-UDF registration over arbitrary Go state.
//
// DuckDB aggregates are vectorized and grouped: the engine allocates one fixed-size
// per-group "state" blob, then drives it through six C callbacks
// (state_size / init / update / combine / finalize / destroy). The proven mechanism
// (see udf_agg_test.go) injects each callback as a Go closure into the engine's
// indirect-function table and hands DuckDB the index as the C function pointer.
//
// That test stored the whole aggregate state (a single int64 sum) INLINE in the
// DuckDB state blob. To support an ARBITRARY Go object as the per-group state — a
// map, a struct with slices, anything with pointers — we cannot store the Go value
// in wasm linear memory. Instead we use a HANDLE TABLE: the DuckDB state blob holds
// just an int64 handle, and the real Go object lives in a Go-side map[int64]any.
// Every callback reads the handle out of the state blob and looks up the live Go
// object. This is the same indirection a cgo build would do with a Go pointer; here
// the handle is an int64 key because raw Go pointers can't cross into wasm memory.
//
// State-blob layout: exactly 8 bytes, a little-endian int64 handle (state_size==8).
// `states`/`source`/`target` callback arguments are arrays of 4-byte wasm pointers,
// one per group in the current vector; each points at that group's 8-byte blob.
package duckdb

import (
	"fmt"
	"sync"
)

// AggregateImpl is the Go-side implementation of one aggregate function. The engine
// creates one state per group via NewState, folds rows in via Update, merges partial
// states (parallel/spilled aggregation, and grouped re-hash) via Combine, and emits
// the per-group result via Finalize. All four run single-threaded per module (the
// driver serializes the engine), so an impl needs no internal locking.
//
//   - NewState returns a fresh zero-accumulator (e.g. &sumState{}). Never nil for a
//     value the other methods can use.
//   - Update folds one batch's worth of rows into state; args is one []any per row
//     ([]any{col0, col1, ...}), already decoded to driver.Value kinds (int64,
//     float64, string, []byte, bool, time.Time, or nil for SQL NULL).
//   - Combine merges src into dst (dst += src); src is left untouched/usable after.
//   - Finalize returns the aggregate's output value for state as a driver.Value kind
//     encodable by writeCell into retTypeID (or nil for SQL NULL). A non-nil error
//     aborts the query with that message (surfaced through DuckDB's aggregate error
//     channel) — the googlesqlite bridge needs this because its replay-at-finalize
//     model defers every per-row Step error to finalize time.
type AggregateImpl interface {
	NewState() any
	Update(state any, args []any)
	Combine(dst, src any)
	Finalize(state any) (any, error)
}

// ---- handle table ------------------------------------------------------------
//
// The map of live Go aggregate states must hang off the module, but module's fields
// are owned by module.go and off-limits here. We therefore keep a package-level
// registry keyed by the *module pointer; each module gets a lazily-created table the
// first time it registers an aggregate. Handles are unique per module (a monotonic
// counter) so a stale blob can never alias a fresh group. The table is freed entry-
// by-entry in the destroy callback; the per-module table object itself is tiny and
// lives as long as the module (a module is a single engine instance, not pooled).

type aggTable struct {
	mu     sync.Mutex
	next   int64
	states map[int64]any
}

func (t *aggTable) put(v any) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.next++
	h := t.next
	t.states[h] = v
	return h
}

func (t *aggTable) get(h int64) any {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.states[h]
}

func (t *aggTable) drop(h int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.states, h)
}

// aggTables maps each *module to its live aggregate-state table. A sync.Map avoids
// any shared lock across modules; per-module access is single-threaded anyway.
var aggTables sync.Map // map[*module]*aggTable

// aggTableFor returns (creating on first use) this module's handle table.
func (mod *module) aggTableFor() *aggTable {
	if v, ok := aggTables.Load(mod); ok {
		return v.(*aggTable)
	}
	t := &aggTable{states: make(map[int64]any)}
	actual, _ := aggTables.LoadOrStore(mod, t)
	return actual.(*aggTable)
}

// newAgg stores a fresh Go state object and returns its int64 handle (written into
// the DuckDB state blob by the init callback).
func (mod *module) newAgg(state any) int64 { return mod.aggTableFor().put(state) }

// aggState looks up the live Go state object for a handle.
func (mod *module) aggState(h int64) any { return mod.aggTableFor().get(h) }

// dropAgg removes a handle (called from destroy when DuckDB frees a group state).
func (mod *module) dropAgg(h int64) { mod.aggTableFor().drop(h) }

// ---- registration ------------------------------------------------------------

// RegisterAggregateUDF registers a Go-implemented aggregate function `name` on
// connection `con`, taking the columns described by paramTypeIDs and returning
// retTypeID (duckdb_type enum values, e.g. dtBigint). impl supplies the behavior.
//
// It builds the six required callbacks as closures over impl + this module's handle
// table, injects each into the engine's indirect-function table (mod.inject), and
// wires them to a duckdb_aggregate_function. Returns an error if DuckDB rejects the
// registration (e.g. a duplicate name), with the recovered engine error message.
func (mod *module) RegisterAggregateUDF(con int32, name string, paramTypeIDs []int32, retTypeID int32, impl AggregateImpl) error {
	m := mod.m

	// state_size: each group's DuckDB blob is exactly one int64 handle (8 bytes).
	stateSize := func(info int32) int64 { return 8 }

	// init: allocate a fresh Go state, store its handle into the 8-byte blob.
	initFn := func(info, state int32) {
		h := mod.newAgg(impl.NewState())
		mod.writeU64(state, uint64(h))
	}

	// update: for each row r in the input chunk, decode every argument column into a
	// []any, then fold it into the row's group state. `states` is an array of 4-byte
	// pointers (states[r] -> that row's 8-byte blob -> int64 handle).
	nCols := len(paramTypeIDs)
	updateFn := func(info, input, states int32) {
		n := m.Xduckdb_data_chunk_get_size(input)

		// Hoist per-column vector data/validity pointers out of the row loop.
		dataPtrs := make([]int32, nCols)
		validPtrs := make([]int32, nCols)
		for c := 0; c < nCols; c++ {
			vec := m.Xduckdb_data_chunk_get_vector(input, int64(c))
			dataPtrs[c] = m.Xduckdb_vector_get_data(vec)
			validPtrs[c] = m.Xduckdb_vector_get_validity(vec)
		}

		for r := int64(0); r < n; r++ {
			args := make([]any, nCols)
			for c := 0; c < nCols; c++ {
				args[c] = mod.readCell(paramTypeIDs[c], dataPtrs[c], validPtrs[c], r)
			}
			blob := mod.readPtr(states + int32(r*4)) // states[r] -> blob ptr
			h := mod.readI64(blob)                   // blob -> int64 handle
			impl.Update(mod.aggState(h), args)
		}
	}

	// combine: merge each source group's state into the matching target group.
	combineFn := func(info, source, target int32, count int64) {
		for i := int64(0); i < count; i++ {
			srcBlob := mod.readPtr(source + int32(i*4))
			dstBlob := mod.readPtr(target + int32(i*4))
			hs := mod.readI64(srcBlob)
			hd := mod.readI64(dstBlob)
			impl.Combine(mod.aggState(hd), mod.aggState(hs))
		}
	}

	// finalize: for each source group, compute the output value and write it into
	// the result vector at row offset+i (DuckDB finalizes groups in batches into a
	// shared output vector at a running offset).
	finalizeFn := func(info, source, result int32, count, offset int64) {
		out := m.Xduckdb_vector_get_data(result)
		for i := int64(0); i < count; i++ {
			blob := mod.readPtr(source + int32(i*4))
			h := mod.readI64(blob)
			v, ferr := impl.Finalize(mod.aggState(h))
			if ferr != nil {
				mod.setAggregateFunctionError(info, fmt.Sprintf("duckdb: aggregate %q finalize: %v", name, ferr))
				return
			}
			if err := mod.writeCell(retTypeID, result, out, offset+i, v); err != nil {
				// A misdeclared retTypeID vs Finalize value surfaces through DuckDB's
				// aggregate error channel (mirrors the cgo bridge's agg_set_error), which
				// aborts the query with this message rather than crashing the engine.
				mod.setAggregateFunctionError(info, fmt.Sprintf("duckdb: aggregate %q finalize: %v", name, err))
				return
			}
		}
	}

	// destroy: DuckDB is freeing a batch of group states — release their Go objects
	// so the handle table doesn't leak. (Some blobs may be zero/uninitialized if a
	// group was never init'd; handle 0 is never issued, so a 0 handle is a no-op.)
	destroyFn := func(states int32, count int64) {
		for i := int64(0); i < count; i++ {
			blob := mod.readPtr(states + int32(i*4))
			if blob == 0 {
				continue
			}
			h := mod.readI64(blob)
			if h != 0 {
				mod.dropAgg(h)
			}
		}
	}

	// Build the duckdb_aggregate_function and wire the injected callbacks.
	af := m.Xduckdb_create_aggregate_function()
	m.Xduckdb_aggregate_function_set_name(af, mod.cstring(name))
	for _, tid := range paramTypeIDs {
		lt := m.Xduckdb_create_logical_type(tid)
		m.Xduckdb_aggregate_function_add_parameter(af, lt)
	}
	m.Xduckdb_aggregate_function_set_return_type(af, m.Xduckdb_create_logical_type(retTypeID))
	// Special NULL handling: the engine feeds NULL rows to update/combine rather than
	// short-circuiting them, matching the cgo aggregate bridge (which always sets it).
	// The impls decide NULL semantics themselves (e.g. SUM skips, COUNT counts).
	m.Xduckdb_aggregate_function_set_special_handling(af)
	m.Xduckdb_aggregate_function_set_functions(af,
		mod.inject(stateSize),
		mod.inject(initFn),
		mod.inject(updateFn),
		mod.inject(combineFn),
		mod.inject(finalizeFn),
	)
	m.Xduckdb_aggregate_function_set_destructor(af, mod.inject(destroyFn))

	if rc := m.Xduckdb_register_aggregate_function(con, af); rc != 0 {
		return fmt.Errorf("duckdb: register aggregate %q failed (rc=%d): %s", name, rc, orUnknown(mod.lastError()))
	}
	return nil
}

// ---- example impl ------------------------------------------------------------

// SumInt64Agg is a tiny reference AggregateImpl: a running int64 sum, the Go analog
// of the inline-state my_sum in udf_agg_test.go but using the generic handle path.
// Register it with paramTypeIDs=[]int32{dtBigint}, retTypeID=dtBigint. NULL inputs
// are skipped (SQL SUM semantics); an all-NULL/empty group finalizes to NULL.
//
//	impl := SumInt64Agg{}
//	err := mod.RegisterAggregateUDF(con, "my_sum", []int32{dtBigint}, dtBigint, impl)
type SumInt64Agg struct{}

type sumState struct {
	sum int64
	any bool // true once at least one non-NULL value has been folded in
}

func (SumInt64Agg) NewState() any { return &sumState{} }

func (SumInt64Agg) Update(state any, args []any) {
	s := state.(*sumState)
	if len(args) == 0 || args[0] == nil {
		return
	}
	if v, ok := asInt64(args[0]); ok {
		s.sum += v
		s.any = true
	}
}

func (SumInt64Agg) Combine(dst, src any) {
	d, s := dst.(*sumState), src.(*sumState)
	d.sum += s.sum
	d.any = d.any || s.any
}

func (SumInt64Agg) Finalize(state any) (any, error) {
	s := state.(*sumState)
	if !s.any {
		return nil, nil // empty/all-NULL group -> SQL NULL
	}
	return s.sum, nil
}
