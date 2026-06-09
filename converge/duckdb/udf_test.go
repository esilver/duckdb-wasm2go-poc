package duckdb

import (
	"encoding/binary"
	"testing"
)

// TestScalarUDF proves a Go closure can be registered as a DuckDB SCALAR UDF in
// the wasm2go-transpiled engine (CGO_ENABLED=0): we append the closure to the
// engine's live indirect-function table and pass its index to
// duckdb_scalar_function_set_function as the C "function pointer". DuckDB then
// call_indirects it during query execution. This is the mechanism the cgo
// BigQuery emulator needs (hundreds of scalar + 32 aggregate UDFs) to run pure-Go.
func TestScalarUDF(t *testing.T) {
	mod := newModule()
	con, _, err := mod.open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	m := mod.m

	// The UDF callback: out[r] = in[r] + 1, over the input data chunk.
	// Signature MUST be func(int32,int32,int32) to match the engine's
	// call_indirect type assertion (info, input_chunk, output_vector offsets).
	called := 0
	cb := func(info, input, output int32) {
		called++
		n := m.Xduckdb_data_chunk_get_size(input)
		inVec := m.Xduckdb_data_chunk_get_vector(input, 0)
		inData := m.Xduckdb_vector_get_data(inVec)
		outData := m.Xduckdb_vector_get_data(output)
		mem := mod.mem()
		for r := int64(0); r < n; r++ {
			v := int64(binary.LittleEndian.Uint64(mem[inData+int32(r*8):]))
			binary.LittleEndian.PutUint64(mem[outData+int32(r*8):], uint64(v+1))
		}
	}

	// Inject the closure into the live indirect table; its index is the fn ptr.
	tbl := m.X__indirect_function_table()
	idx := int32(len(*tbl))
	*tbl = append(*tbl, cb)

	// Build + register the scalar function: my_add_one(BIGINT) -> BIGINT.
	const DUCKDB_TYPE_BIGINT = 5
	bigint := m.Xduckdb_create_logical_type(DUCKDB_TYPE_BIGINT)
	sf := m.Xduckdb_create_scalar_function()
	namePtr := mod.cstring("my_add_one")
	m.Xduckdb_scalar_function_set_name(sf, namePtr)
	m.Xduckdb_scalar_function_add_parameter(sf, bigint)
	m.Xduckdb_scalar_function_set_return_type(sf, bigint)
	m.Xduckdb_scalar_function_set_function(sf, idx)
	if rc := m.Xduckdb_register_scalar_function(con, sf); rc != 0 {
		t.Fatalf("register_scalar_function failed (rc=%d): %s", rc, mod.lastError())
	}

	// Execute a query that calls the Go UDF.
	sqlPtr := mod.cstring("SELECT my_add_one(41)")
	resPtr := mod.allocOut(sizeofDuckdbResult)
	if rc := m.Xduckdb_query(con, sqlPtr, resPtr); rc != 0 {
		t.Fatalf("query failed (rc=%d): %s", rc, mod.lastError())
	}
	got := m.Xduckdb_value_int64(resPtr, 0, 0)
	t.Logf("SELECT my_add_one(41) = %d (callback invoked %d time(s))", got, called)
	if got != 42 {
		t.Fatalf("got %d, want 42", got)
	}
	if called == 0 {
		t.Fatalf("UDF callback was never invoked by DuckDB")
	}

	// A second call over multiple rows, to exercise the vectorized path.
	sql2 := mod.cstring("SELECT sum(my_add_one(x)) FROM range(5) t(x)") // (0..4)+1 summed = 15
	res2 := mod.allocOut(sizeofDuckdbResult)
	if rc := m.Xduckdb_query(con, sql2, res2); rc != 0 {
		t.Fatalf("vectorized query failed (rc=%d): %s", rc, mod.lastError())
	}
	got2 := m.Xduckdb_value_int64(res2, 0, 0)
	t.Logf("SELECT sum(my_add_one(x)) FROM range(5) = %d (want 15)", got2)
	if got2 != 15 {
		t.Fatalf("vectorized: got %d, want 15", got2)
	}
}
