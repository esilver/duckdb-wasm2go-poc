package duckdb

import (
	"encoding/binary"
	"testing"
)

// TestAggregateUDF proves a Go-implemented AGGREGATE can be registered into the
// wasm2go DuckDB via the 6 state callbacks (state_size/init/update/combine/
// finalize/destroy), each a Go closure injected into the engine's indirect table.
// my_sum(BIGINT)->BIGINT with an int64 running-sum state. Exercises ungrouped AND
// grouped aggregation (the latter drives multiple states + combine + multi-group
// finalize) — the shape the BigQuery emulator's 32 aggregates need.
func TestAggregateUDF(t *testing.T) {
	mod := newModule()
	con, _, err := mod.open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	m := mod.m
	rdU32 := func(p int32) int32 { return int32(binary.LittleEndian.Uint32(mod.mem()[p:])) }
	rdI64 := func(p int32) int64 { return int64(binary.LittleEndian.Uint64(mod.mem()[p:])) }
	wrI64 := func(p int32, v int64) { binary.LittleEndian.PutUint64(mod.mem()[p:], uint64(v)) }

	// state = one int64 sum (8 bytes). states args are arrays of void* (4-byte ptrs).
	stateSize := func(info int32) int64 { return 8 }
	initFn := func(info, state int32) { wrI64(state, 0) }
	updateFn := func(info, input, states int32) {
		n := m.Xduckdb_data_chunk_get_size(input)
		inData := m.Xduckdb_vector_get_data(m.Xduckdb_data_chunk_get_vector(input, 0))
		for r := int64(0); r < n; r++ {
			st := rdU32(states + int32(r*4)) // states[r] -> state ptr
			wrI64(st, rdI64(st)+rdI64(inData+int32(r*8)))
		}
	}
	combineFn := func(info, source, target int32, count int64) {
		for i := int64(0); i < count; i++ {
			s := rdU32(source + int32(i*4))
			tg := rdU32(target + int32(i*4))
			wrI64(tg, rdI64(tg)+rdI64(s))
		}
	}
	finalizeFn := func(info, source, result int32, count, offset int64) {
		out := m.Xduckdb_vector_get_data(result)
		for i := int64(0); i < count; i++ {
			s := rdU32(source + int32(i*4))
			wrI64(out+int32((offset+i)*8), rdI64(s))
		}
	}
	destroyFn := func(states int32, count int64) {} // no heap in state

	tbl := m.X__indirect_function_table()
	inject := func(fn any) int32 { idx := int32(len(*tbl)); *tbl = append(*tbl, fn); return idx }

	bigint := m.Xduckdb_create_logical_type(5) // BIGINT
	af := m.Xduckdb_create_aggregate_function()
	m.Xduckdb_aggregate_function_set_name(af, mod.cstring("my_sum"))
	m.Xduckdb_aggregate_function_add_parameter(af, bigint)
	m.Xduckdb_aggregate_function_set_return_type(af, bigint)
	m.Xduckdb_aggregate_function_set_functions(af,
		inject(stateSize), inject(initFn), inject(updateFn), inject(combineFn), inject(finalizeFn))
	m.Xduckdb_aggregate_function_set_destructor(af, inject(destroyFn))
	if rc := m.Xduckdb_register_aggregate_function(con, af); rc != 0 {
		t.Fatalf("register_aggregate_function failed (rc=%d): %s", rc, mod.lastError())
	}

	scalar := func(sql string, col, row int64) int64 {
		res := mod.allocOut(sizeofDuckdbResult)
		if rc := m.Xduckdb_query(con, mod.cstring(sql), res); rc != 0 {
			t.Fatalf("query %q failed (rc=%d): %s", sql, rc, mod.lastError())
		}
		return m.Xduckdb_value_int64(res, col, row)
	}

	// ungrouped: my_sum(1..10) = 55
	if got := scalar("SELECT my_sum(x) FROM range(1,11) t(x)", 0, 0); got != 55 {
		t.Fatalf("ungrouped my_sum = %d, want 55", got)
	} else {
		t.Logf("my_sum(1..10) = %d ✓", got)
	}

	// grouped by parity: even(2+4+6+8+10)=30 (g=0,row0), odd(1+3+5+7+9)=25 (g=1,row1)
	even := scalar("SELECT x%2 g, my_sum(x) s FROM range(1,11) t(x) GROUP BY x%2 ORDER BY g", 1, 0)
	odd := scalar("SELECT x%2 g, my_sum(x) s FROM range(1,11) t(x) GROUP BY x%2 ORDER BY g", 1, 1)
	t.Logf("grouped my_sum: even=%d odd=%d (want 30, 25)", even, odd)
	if even != 30 || odd != 25 {
		t.Fatalf("grouped my_sum even=%d odd=%d, want 30/25", even, odd)
	}
}
