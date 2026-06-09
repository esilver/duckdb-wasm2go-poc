// Package duckdb is a pure-Go (CGO_ENABLED=0) database/sql driver for DuckDB,
// driving the wasm2go-transpiled engine (package duckdbcore) through its C API.
// This file is the shared foundation: module/connection lifecycle, the exception
// host + WASI shim wiring, and low-level wasm-memory marshalling helpers that
// driver.go (the database/sql surface) and result.go (chunk/type reading) build on.
package duckdb

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"sync"

	core "duckdbconverge/genpkg"

	"duckdbconverge/exhost"
	"duckdbconverge/wasishim"
)

// ---- env wiring: generated *core.Module -> exhost/wasishim (mirrors main.go) ----

type modABI struct{ m *core.Module }

func (a modABI) SetThrew(threw, value int32) { a.m.XsetThrew(threw, value) }
func (a modABI) TempretSet(v int32)          { a.m.X_emscripten_tempret_set(v) }
func (a modABI) Table() []any                { return *a.m.X__indirect_function_table() }
func (a modABI) CanCatch(catchType, excType, adjustedPtrSlot int32) int32 {
	return a.m.X__cxa_can_catch(catchType, excType, adjustedPtrSlot)
}
func (a modABI) GetExceptionPtr(excHeader int32) int32 {
	return a.m.X__cxa_get_exception_ptr(excHeader)
}
func (a modABI) DynamicCast(obj, srcType, dstType, offset int32) int32 { return 0 }
func (a modABI) Malloc(n int32) int32                                  { return a.m.Xmalloc(n) }
func (a modABI) Free(ptr int32)                                        { a.m.Xfree(ptr) }
func (a modABI) ReadU32(ptr int32) int32 {
	mem := *a.m.Xmemory().Slice()
	return int32(binary.LittleEndian.Uint32(mem[ptr:]))
}
func (a modABI) WriteU32(ptr, v int32) {
	mem := *a.m.Xmemory().Slice()
	binary.LittleEndian.PutUint32(mem[ptr:], uint32(v))
}

type memABI struct{ m *core.Module }

func (a memABI) Mem() []byte { return *a.m.Xmemory().Slice() }
func (a memABI) Grow(deltaPages int32) int32 {
	return int32(a.m.Xmemory().Grow(int64(deltaPages), 1<<31))
}

type env struct {
	*exhost.Host
	*wasishim.Shim
	mod *core.Module
}

func (e *env) Init(m any) {
	e.mod = m.(*core.Module)
	e.Host.Init(m)
	e.Shim.SetMem(memABI{m: e.mod})
}

func newEnv() *env {
	host := exhost.New(func(mod any) exhost.ModuleABI { return modABI{m: mod.(*core.Module)} })
	shim := wasishim.New(nil, os.Stdout, os.Stderr)
	// Preopen the host root so DuckDB can open file-backed databases and read data
	// files by absolute path (Tier 2 persistence) through the WASI filesystem shim.
	shim.SetPreopen("/", "/")
	return &env{Host: host, Shim: shim}
}

// ---- module: one engine instance -------------------------------------------

// module is one instantiated DuckDB engine (one wasm linear memory). It is NOT
// safe for concurrent use; the driver serializes access per connection.
type module struct {
	m *core.Module
	e *env
	// mu serializes engine access for a standalone (raw Driver.Open) conn; the
	// pooled/connector path shares the connector's mutex instead. The wasm engine
	// is single-threaded, so exactly one of these guards every C-API call.
	mu sync.Mutex
	// pins keeps Go UDF callback closures alive for the module's lifetime: they
	// live in the engine's indirect-function table (which roots them too), but an
	// explicit pin documents intent and survives any table reallocation. Indices
	// are permanent handles; never shrink/reorder.
	pins []any
}

func newModule() *module {
	e := newEnv()
	m := core.New(e, e.Shim)
	m.X_initialize()
	mod := &module{m: m, e: e}
	return mod
}

// inject appends a Go closure to the engine's LIVE indirect-function table and
// returns its int32 index — the value DuckDB stores as the C "function pointer"
// and later call_indirects. The closure's dynamic type must match what the
// engine asserts (e.g. func(int32,int32,int32) for a scalar UDF callback). Pinned
// for the module's lifetime. This is the proven UDF-callback mechanism.
func (mod *module) inject(fn any) int32 {
	tbl := mod.m.X__indirect_function_table()
	idx := int32(len(*tbl))
	*tbl = append(*tbl, fn)
	mod.pins = append(mod.pins, fn)
	return idx
}

// lastError returns the message of the most recent C++ exception (DuckDB's
// convert-and-rethrow loses it from duckdb_result_error; we recover it from the
// host). Returns "" if none.
func (mod *module) lastError() string { return mod.e.Host.LastThrowMessage() }

// ---- low-level wasm-memory marshalling (shared by driver.go + result.go) -----

func (mod *module) mem() []byte { return *mod.m.Xmemory().Slice() }

// cstring writes s+NUL into module memory and returns the offset (a C char*).
func (mod *module) cstring(s string) int32 {
	ptr := mod.m.Xmalloc(int32(len(s) + 1))
	if ptr == 0 {
		panic("duckdb: malloc returned null")
	}
	mem := mod.mem()
	copy(mem[ptr:], s)
	mem[ptr+int32(len(s))] = 0
	return ptr
}

// goString reads a NUL-terminated C string out of module memory.
func (mod *module) goString(ptr int32) string {
	if ptr == 0 {
		return ""
	}
	mem := mod.mem()
	end := ptr
	for int(end) < len(mem) && mem[end] != 0 {
		end++
	}
	return string(mem[ptr:end])
}

// allocOut reserves n zeroed bytes (for out-params: handles, result structs).
func (mod *module) allocOut(n int32) int32 {
	ptr := mod.m.Xmalloc(n)
	mem := mod.mem()
	for i := int32(0); i < n; i++ {
		mem[ptr+i] = 0
	}
	return ptr
}

func (mod *module) free(ptr int32) { mod.m.Xfree(ptr) }

func (mod *module) readU32(ptr int32) uint32  { return binary.LittleEndian.Uint32(mod.mem()[ptr:]) }
func (mod *module) readU64(ptr int32) uint64  { return binary.LittleEndian.Uint64(mod.mem()[ptr:]) }
func (mod *module) readI64(ptr int32) int64   { return int64(mod.readU64(ptr)) }
func (mod *module) readPtr(ptr int32) int32   { return int32(mod.readU32(ptr)) }
func (mod *module) readF64(ptr int32) float64 { return math.Float64frombits(mod.readU64(ptr)) }
func (mod *module) readF32(ptr int32) float32 {
	return math.Float32frombits(mod.readU32(ptr))
}
func (mod *module) writeU32(ptr int32, v uint32) {
	binary.LittleEndian.PutUint32(mod.mem()[ptr:], v)
}

// sizeofDuckdbResult over-allocates duckdb_result (its true size is version-
// dependent; 256 is a safe upper bound — we read fields via C-API accessors).
const sizeofDuckdbResult = 256

// open opens a database at path (":memory:" for in-memory) and connects,
// registering the statically-linked core_functions extension first.
// Returns the connection handle (duckdb_connection) and the db handle.
func (mod *module) open(path string) (con int32, db int32, err error) {
	pathPtr := mod.cstring(path)
	dbSlot := mod.allocOut(4)
	if rc := mod.m.Xduckdb_open(pathPtr, dbSlot); rc != 0 {
		return 0, 0, fmt.Errorf("duckdb_open(%q): %s", path, orUnknown(mod.lastError()))
	}
	db = mod.readPtr(dbSlot)
	mod.m.Xregister_core_functions(db) // core-only amalgamation: register sum/avg/strings

	conSlot := mod.allocOut(4)
	if rc := mod.m.Xduckdb_connect(db, conSlot); rc != 0 {
		return 0, 0, fmt.Errorf("duckdb_connect: %s", orUnknown(mod.lastError()))
	}
	con = mod.readPtr(conSlot)

	mod.free(pathPtr)
	mod.free(dbSlot)
	mod.free(conSlot)
	return con, db, nil
}

// connect opens an additional duckdb_connection against an already-open database
// handle (from a prior open()). Used so every pooled connection from one connector
// shares the SAME in-memory database/catalog (DDL on one conn is visible to others)
// — matching native DuckDB's model. Caller must serialize engine access.
func (mod *module) connect(db int32) (con int32, err error) {
	conSlot := mod.allocOut(4)
	defer mod.free(conSlot)
	if rc := mod.m.Xduckdb_connect(db, conSlot); rc != 0 {
		return 0, fmt.Errorf("duckdb_connect: %s", orUnknown(mod.lastError()))
	}
	return mod.readPtr(conSlot), nil
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown error"
	}
	return s
}
