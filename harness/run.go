//go:build harness_generated

// Command run drives a wasm2go-transpiled, C-API-shaped wasm module entirely in
// pure Go (CGO_ENABLED=0). It wires the generated *Module to the exception host
// (exhost) and the WASI/libc shim (wasishim), then calls the module's exported
// C functions the same way it will call DuckDB's: marshal C-strings into module
// memory, pass offsets, read results back.
//
// For the validation POC the exported surface is poc.cc's db_open / query_scalar
// / echo_len. For real DuckDB the SAME flow calls duckdb_open / duckdb_connect /
// duckdb_query / duckdb_value_int64 (see PLUGIN.md); only the method names and
// the out-param layout change.
//
// Build/run: ./build-poc.sh && CGO_ENABLED=0 go run -tags harness_generated .
// (no cgo, no external runtime)
package main

import (
	"encoding/binary"
	"fmt"
	"os"

	"duckdbharness/exhost"
	poc "duckdbharness/genpkg"
	"duckdbharness/wasishim"
)

// modABI adapts the generated *poc.Module to exhost.ModuleABI: it forwards to
// the module's EXPORTED setThrew / tempret_set / table / RTTI methods. This is
// where the host's "RTTI is delegated to the module's exports" contract is
// physically wired - __cxa_can_catch and friends are the module's own code.
type modABI struct{ m *poc.Module }

func (a modABI) SetThrew(threw, value int32) { a.m.XsetThrew(threw, value) }
func (a modABI) TempretSet(v int32)          { a.m.X_emscripten_tempret_set(v) }
func (a modABI) Table() []any                { return *a.m.X__indirect_function_table() }
func (a modABI) CanCatch(catchType, excType, adjustedPtrSlot int32) int32 {
	return a.m.X__cxa_can_catch(catchType, excType, adjustedPtrSlot)
}
func (a modABI) GetExceptionPtr(excHeader int32) int32 {
	return a.m.X__cxa_get_exception_ptr(excHeader)
}

// DynamicCast: poc.wasm (like T1's) does not export __dynamic_cast - the
// single-inheritance catch paths route through CanCatch - so this adapter
// returns 0. A DuckDB build that exports __dynamic_cast wires it here.
func (a modABI) DynamicCast(obj, srcType, dstType, offset int32) int32 { return 0 }

func (a modABI) Malloc(n int32) int32 { return a.m.Xmalloc(n) }
func (a modABI) Free(ptr int32)       { a.m.Xfree(ptr) }
func (a modABI) ReadU32(ptr int32) int32 {
	mem := *a.m.Xmemory().Slice()
	return int32(binary.LittleEndian.Uint32(mem[ptr:]))
}
func (a modABI) WriteU32(ptr, v int32) {
	mem := *a.m.Xmemory().Slice()
	binary.LittleEndian.PutUint32(mem[ptr:], uint32(v))
}

// memABI adapts *poc.Module to wasishim.MemoryABI (live memory + heap growth).
type memABI struct{ m *poc.Module }

func (a memABI) Mem() []byte { return *a.m.Xmemory().Slice() }
func (a memABI) Grow(deltaPages int32) int32 {
	return int32(a.m.Xmemory().Grow(int64(deltaPages), 1<<31))
}

// env is the single Xenv value the generated New() receives. It satisfies both
// the exception-ABI methods (promoted from *exhost.Host) and the WASI/libc
// methods (promoted from *wasishim.Shim). Its Init hook binds both adapters
// once the module exists.
type env struct {
	*exhost.Host
	*wasishim.Shim
	mod *poc.Module
}

// Init is the hook the generated New() calls with the concrete *Module. We bind
// the exception host's ABI adapter and the shim's memory adapter here.
func (e *env) Init(m any) {
	e.mod = m.(*poc.Module)
	e.Host.Init(m)                  // exhost binds its ModuleABI via the binder
	e.Shim.SetMem(memABI{m: e.mod}) // shim gets live memory access
}

// newEnv builds the combined env. The exhost binder turns the *Module into a
// ModuleABI; the shim's memory is set later in Init (memory needs the module).
func newEnv() *env {
	host := exhost.New(func(mod any) exhost.ModuleABI {
		return modABI{m: mod.(*poc.Module)}
	})
	host.Trace = true
	shim := wasishim.New(nil, os.Stdout, os.Stderr)
	return &env{Host: host, Shim: shim}
}

// ---- C-string / memory marshalling ----------------------------------------

// cstring allocates len(s)+1 bytes via the module's malloc, writes s + a NUL
// terminator into module memory, and returns the offset (a wasm char*). Free it
// with m.Xfree when done. This is THE marshalling primitive the DuckDB driver
// will reuse for every const char* argument (paths, SQL text, identifiers).
func cstring(m *poc.Module, s string) int32 {
	ptr := m.Xmalloc(int32(len(s) + 1))
	if ptr == 0 {
		panic("malloc returned null")
	}
	mem := *m.Xmemory().Slice()
	copy(mem[ptr:], s)
	mem[ptr+int32(len(s))] = 0
	return ptr
}

// goString reads a NUL-terminated C-string at ptr out of module memory.
func goString(m *poc.Module, ptr int32) string {
	if ptr == 0 {
		return ""
	}
	mem := *m.Xmemory().Slice()
	end := ptr
	for int(end) < len(mem) && mem[end] != 0 {
		end++
	}
	return string(mem[ptr:end])
}

// allocOut reserves n zeroed bytes for an out-parameter (e.g. int64*, char**)
// and returns the offset. Read it back with readU64/readPtr.
func allocOut(m *poc.Module, n int32) int32 {
	ptr := m.Xmalloc(n)
	mem := *m.Xmemory().Slice()
	for i := int32(0); i < n; i++ {
		mem[ptr+i] = 0
	}
	return ptr
}

func readU64(m *poc.Module, ptr int32) int64 {
	mem := *m.Xmemory().Slice()
	return int64(binary.LittleEndian.Uint64(mem[ptr:]))
}
func readPtr(m *poc.Module, ptr int32) int32 {
	mem := *m.Xmemory().Slice()
	return int32(binary.LittleEndian.Uint32(mem[ptr:]))
}

// ---- the C-API driving flow -----------------------------------------------

// queryResult mirrors the shape a DuckDB query yields: a status, a scalar value,
// and (on error) the message DuckDB/our wasm put in memory.
type queryResult struct {
	status int32
	value  int64
	errMsg string
}

// runScalar drives open -> query -> read the same way the DuckDB path will:
//
//	poc.cc:  db_open(":memory:", &handle); query_scalar(sql, &val, &err)
//	DuckDB:  duckdb_open(":memory:", &db); duckdb_connect(db,&con);
//	         duckdb_query(con, sql, &res); duckdb_value_int64(&res,0,0)
//
// Returns the scalar for a good query and an error STATUS (not a process abort)
// for a bad one, proving the throw -> Go-trampoline -> catch path round-trips.
func runScalar(m *poc.Module, sql string) queryResult {
	// open(":memory:") into an out handle slot.
	pathPtr := cstring(m, ":memory:")
	handleSlot := allocOut(m, 4)
	if rc := m.Xdb_open(pathPtr, handleSlot); rc != 0 {
		m.Xfree(pathPtr)
		m.Xfree(handleSlot)
		return queryResult{status: rc, errMsg: "db_open failed"}
	}
	m.Xfree(pathPtr)

	// query_scalar(sql, &val, &err)
	sqlPtr := cstring(m, sql)
	valSlot := allocOut(m, 8)
	errSlot := allocOut(m, 4)
	status := m.Xquery_scalar(sqlPtr, valSlot, errSlot)

	res := queryResult{status: status}
	if status == 0 {
		res.value = readU64(m, valSlot)
	} else {
		res.errMsg = goString(m, readPtr(m, errSlot))
	}

	m.Xfree(sqlPtr)
	m.Xfree(valSlot)
	m.Xfree(errSlot)

	// close the handle (mirrors duckdb_disconnect/duckdb_close)
	m.Xdb_close(handleSlot)
	m.Xfree(handleSlot)
	return res
}

func main() {
	e := newEnv()
	m := poc.New(e)  // calls e.Init(m): binds exception ABI + shim memory
	m.X_initialize() // run the module's ctors / start function

	queries := []string{"SELECT 1", "SELECT 42", "SELECT bogus"}
	for _, q := range queries {
		res := runScalar(m, q)
		if res.status == 0 {
			fmt.Printf("query_scalar(%q) -> value=%d (status=0)\n", q, res.value)
		} else {
			fmt.Printf("query_scalar(%q) -> ERROR status=%d msg=%q (caught, not aborted)\n", q, res.status, res.errMsg)
		}
	}

	// echo_len exercises plain (non-throwing) cstring marshalling end-to-end.
	s := "hello-duckdb"
	p := cstring(m, s)
	fmt.Printf("echo_len(%q) -> %d (want %d)\n", s, m.Xecho_len(p), len(s))
	m.Xfree(p)
}
