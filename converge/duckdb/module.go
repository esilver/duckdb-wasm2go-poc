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
	"net/url"
	"os"
	"strings"
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
	relocateShadowStack(m)
	mod := &module{m: m, e: e}
	return mod
}

// shadowStackSize is the size of the relocated C shadow stack (see
// relocateShadowStack). DuckDB's recursion guards are LOGICAL counters tuned
// for native ~8MB thread stacks: e.g. ExpressionBinder::StackCheck only throws
// "Max expression depth limit" once stack_depth reaches max_expression_depth
// (default 1000), which takes ~1.5-2MB of real stack to bind. 32MB gives the
// same headroom native DuckDB assumes, with margin for the parser/optimizer
// recursions that share the guard.
const shadowStackSize = 32 << 20

// relocateShadowStack moves the engine's C shadow stack from the 64KB region
// the wasm binary was linked with onto a large block malloc'd from the
// engine's own heap. The emscripten build ships STACK_SIZE=64KB placed right
// above the data segment; wasm stack overflow does NOT trap — the stack
// pointer just runs down into the data segment, silently corrupting globals
// (seen as "slice bounds out of range [<ASCII garbage>:...]" panics when a
// trashed constant is later used as a pointer, e.g. binding a self-recursive
// macro: duckdb-src/test/sql/catalog/function/test_recursive_macro*.test).
// With a 32MB stack, DuckDB's logical depth guards fire (clean BinderException)
// long before the stack can overflow.
//
// Safe because: it runs between exported calls (wasm stack is empty, the stack
// pointer global g0 sits at its initial top, so nothing references the old
// region); the block comes from the module's own dlmalloc heap, so the heap
// never collides with it; and it is pinned for the module's lifetime (never
// freed). The stack pointer must stay 16-byte aligned per the wasm C ABI.
func relocateShadowStack(m *core.Module) {
	base := m.Xmalloc(shadowStackSize)
	if base == 0 {
		panic("duckdb: cannot allocate shadow stack")
	}
	top := (base + shadowStackSize) &^ 0xF // grows down from top; 16-byte aligned
	m.X_emscripten_stack_restore(top)
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

// defaultMaxMemory is the engine memory limit applied on open. The wasm build
// self-detects only ~17.5MB (emscripten reports the initial linear-memory size,
// not host RAM), which makes even modest queries spill/abort. 1GiB matches what
// a native DuckDB on a small host would pick. Overridable per database with a
// `?max_memory=...` (alias `?memory_limit=...`) DSN query parameter, e.g.
// "file.db?max_memory=4GB" or ":memory:?max_memory=256MB".
const defaultMaxMemory = "1GiB"

// parseDSN splits a DSN into the database path and the memory limit to apply.
// Query parameters follow the path after '?'; only max_memory/memory_limit are
// recognized (unknown parameters are ignored). "" means ":memory:".
func parseDSN(dsn string) (path, maxMemory string) {
	path, maxMemory = dsn, defaultMaxMemory
	if i := strings.IndexByte(dsn, '?'); i >= 0 {
		path = dsn[:i]
		if vals, err := url.ParseQuery(dsn[i+1:]); err == nil {
			if v := vals.Get("max_memory"); v != "" {
				maxMemory = v
			} else if v := vals.Get("memory_limit"); v != "" {
				maxMemory = v
			}
		}
	}
	if path == "" {
		path = ":memory:"
	}
	return path, maxMemory
}

// open opens a database at the DSN's path (":memory:" for in-memory) and
// connects, registering the statically-linked core_functions extension first.
// Returns the connection handle (duckdb_connection) and the db handle.
// The dsn may carry query parameters (see parseDSN); the memory limit is set
// via SET max_memory right after connecting (the C API's duckdb_set_config is
// not among the wasm exports, but max_memory is a runtime-settable GLOBAL
// option, so SQL on the first connection configures the whole database).
func (mod *module) open(dsn string) (con int32, db int32, err error) {
	path, maxMemory := parseDSN(dsn)
	// Install the Tier-2 host filesystem on a DBConfig BEFORE open (the database
	// file is opened DURING duckdb_open_ext using config.file_system; a post-open
	// hook is too late). Without this, the instance falls back to DuckDB's local
	// FS, whose directory syscalls are ENOSYS under wasm — file-backed DBs can't
	// persist and even :memory: DBs fail when the engine spills (the temp
	// directory is created through the instance's filesystem).
	cfgSlot := mod.allocOut(4)
	if rc := mod.m.Xduckdb_create_config(cfgSlot); rc != 0 {
		mod.free(cfgSlot)
		return 0, 0, fmt.Errorf("duckdb_create_config: %s", orUnknown(mod.lastError()))
	}
	cfg := mod.readPtr(cfgSlot)
	mod.m.Xhost_fs_attach_to_config(cfg)

	pathPtr := mod.cstring(path)
	dbSlot := mod.allocOut(4)
	errSlot := mod.allocOut(4) // char** out-param for the open error string
	rc := mod.m.Xduckdb_open_ext(pathPtr, dbSlot, cfg, errSlot)
	mod.m.Xduckdb_destroy_config(cfgSlot) // open_ext moved file_system out; free the shell
	mod.free(cfgSlot)
	if rc != 0 {
		msg := ""
		if ep := mod.readPtr(errSlot); ep != 0 {
			msg = mod.goString(ep)
		}
		if msg == "" {
			msg = mod.lastError()
		}
		mod.free(errSlot)
		return 0, 0, fmt.Errorf("duckdb_open_ext(%q): %s", path, orUnknown(msg))
	}
	mod.free(errSlot)
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

	// Apply the memory limit (global option; see defaultMaxMemory). A bad
	// user-supplied value must surface, not be swallowed.
	setSQL := "SET max_memory='" + strings.ReplaceAll(maxMemory, "'", "''") + "'"
	if _, err := mod.queryRaw(con, setSQL); err != nil {
		return 0, 0, fmt.Errorf("duckdb open: setting max_memory=%q: %w", maxMemory, err)
	}
	return con, db, nil
}

// queryRaw runs sql on con through duckdb_query — the direct (non-prepared)
// C API entry point, which accepts MULTI-STATEMENT text (all statements run;
// the result is the last one's). Returns the rows-changed count of that final
// result. Used for statements that cannot go through duckdb_prepare (multi-
// statement Exec fallback), for transaction recovery (ROLLBACK that must not
// recurse into the prepare path), and for open-time SET. Caller must hold the
// engine lock (or be in single-threaded setup code).
func (mod *module) queryRaw(con int32, sql string) (rowsChanged int64, err error) {
	sqlPtr := mod.cstring(sql)
	defer mod.free(sqlPtr)
	resPtr := mod.allocOut(sizeofDuckdbResult)
	defer func() {
		mod.m.Xduckdb_destroy_result(resPtr)
		mod.free(resPtr)
	}()
	if rc := mod.m.Xduckdb_query(con, sqlPtr, resPtr); rc != 0 {
		msg := mod.goString(mod.m.Xduckdb_result_error(resPtr))
		if msg == "" {
			msg = mod.lastError()
		}
		return 0, fmt.Errorf("duckdb query: %s", orUnknown(msg))
	}
	return mod.m.Xduckdb_rows_changed(resPtr), nil
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
