// Command duckdbconverge drives the REAL DuckDB C-API wasm, transpiled to pure
// Go by wasm2go, entirely under CGO_ENABLED=0. It wires the generated *Module
// (package duckdbcore) to the validated exception host (exhost) and WASI/libc
// shim (wasishim), then calls duckdb_open / duckdb_connect / duckdb_query /
// duckdb_value_int64 the way PLUGIN.md prescribes: marshal C-strings into module
// memory, pass offsets, read scalars back out.
//
// This is the convergence payoff: the same harness proven on the standalone
// poc.wasm, now pointed at the full DuckDB engine.
//
// Build/run: CGO_ENABLED=0 go run .
package main

import (
	"encoding/binary"
	"fmt"
	"os"

	core "duckdbconverge/genpkg"

	"duckdbconverge/exhost"
	"duckdbconverge/wasishim"
)

// ---- ABI adapters: the generated *core.Module -> exhost/wasishim interfaces --

// modABI adapts *core.Module to exhost.ModuleABI by forwarding to the module's
// EXPORTED setThrew / tempret_set / table / RTTI / malloc / free / memory. RTTI
// stays the module's own __cxa_can_catch (libc++abi compiled into the wasm),
// never reimplemented in Go.
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

// DynamicCast: this DuckDB build does not export __dynamic_cast (verified), so
// single-inheritance catches route through CanCatch and this returns 0.
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

// memABI adapts *core.Module to wasishim.MemoryABI (live memory + heap growth).
type memABI struct{ m *core.Module }

func (a memABI) Mem() []byte { return *a.m.Xmemory().Slice() }
func (a memABI) Grow(deltaPages int32) int32 {
	return int32(a.m.Xmemory().Grow(int64(deltaPages), 1<<31))
}

// env is the Xenv value New() receives as its FIRST arg. It promotes the
// exception-ABI methods from *exhost.Host and the emscripten "env" methods
// (notify_memory_growth, the __syscall_* family, getaddrinfo/getnameinfo) from
// *wasishim.Shim. Its Init hook binds both adapters once the module exists.
type env struct {
	*exhost.Host
	*wasishim.Shim
	mod *core.Module
}

// Init is the hook the generated New() calls with the concrete *Module. It binds
// the exception host's ABI adapter and installs the shim's live memory view.
func (e *env) Init(m any) {
	e.mod = m.(*core.Module)
	e.Host.Init(m)
	e.Shim.SetMem(memABI{m: e.mod})
}

func newEnv() *env {
	host := exhost.New(func(mod any) exhost.ModuleABI {
		return modABI{m: mod.(*core.Module)}
	})
	shim := wasishim.New(nil, os.Stdout, os.Stderr)
	return &env{Host: host, Shim: shim}
}

// Compile-time proof the wiring matches the generated import interfaces exactly:
// the combined *env satisfies Xenv (exception ABI + emscripten env), and the
// *wasishim.Shim satisfies Xwasi (the 12 WASI calls). If a method is missing or
// mis-typed, the build fails here instead of at New().
var (
	_ core.Xenv                    = (*env)(nil)
	_ core.Xwasi_snapshot_preview1 = (*wasishim.Shim)(nil)
)

// ---- C-string / memory marshalling (carried over from PLUGIN.md) ------------

func cstring(m *core.Module, s string) int32 {
	ptr := m.Xmalloc(int32(len(s) + 1))
	if ptr == 0 {
		panic("malloc returned null")
	}
	mem := *m.Xmemory().Slice()
	copy(mem[ptr:], s)
	mem[ptr+int32(len(s))] = 0
	return ptr
}

func goString(m *core.Module, ptr int32) string {
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

func allocOut(m *core.Module, n int32) int32 {
	ptr := m.Xmalloc(n)
	mem := *m.Xmemory().Slice()
	for i := int32(0); i < n; i++ {
		mem[ptr+i] = 0
	}
	return ptr
}

func readPtr(m *core.Module, ptr int32) int32 {
	mem := *m.Xmemory().Slice()
	return int32(binary.LittleEndian.Uint32(mem[ptr:]))
}

// ---- the DuckDB C-API driving flow ------------------------------------------

// sizeofDuckdbResult is sizeof(duckdb_result) in the wasm32 ABI. The struct is
// { idx_t column_count; idx_t row_count; idx_t rows_changed; void* /*deprecated
// columns*/; char* error_message; void* internal_data; } -> 3*8 + 4 + 4 + 4,
// padded to 8-byte alignment. Over-allocating is harmless: duckdb_query writes
// only the fields it owns, and we read columns/rows via the accessor functions,
// not by hand. 256 bytes is a safe over-estimate across DuckDB versions.
const sizeofDuckdbResult = 256

// dbHandle opens an in-memory database and connects, returning the connection
// pointer plus the db/con out-slots (which double as the disconnect/close args).
type dbHandle struct {
	db, con int32 // duckdb_database / duckdb_connection values
}

func openConnect(m *core.Module) (dbHandle, error) {
	pathPtr := cstring(m, ":memory:")
	dbSlot := allocOut(m, 4)
	if rc := m.Xduckdb_open(pathPtr, dbSlot); rc != 0 {
		return dbHandle{}, fmt.Errorf("duckdb_open(:memory:) -> state=%d", rc)
	}
	db := readPtr(m, dbSlot)

	conSlot := allocOut(m, 4)
	if rc := m.Xduckdb_connect(db, conSlot); rc != 0 {
		return dbHandle{}, fmt.Errorf("duckdb_connect -> state=%d", rc)
	}
	con := readPtr(m, conSlot)

	m.Xfree(pathPtr)
	m.Xfree(dbSlot)
	m.Xfree(conSlot)
	return dbHandle{db: db, con: con}, nil
}

// queryInt64 runs sql and returns the scalar at (col0,row0) as int64. On a DuckDB
// error it returns the engine's error string (via duckdb_result_error) rather
// than aborting - the throw->Go-trampoline->catch path round-tripping.
func queryInt64(m *core.Module, h dbHandle, sql string) (int64, int64, int64, error) {
	sqlPtr := cstring(m, sql)
	resPtr := allocOut(m, sizeofDuckdbResult)
	rc := m.Xduckdb_query(h.con, sqlPtr, resPtr)
	m.Xfree(sqlPtr)

	if rc != 0 {
		errPtr := m.Xduckdb_result_error(resPtr)
		msg := goString(m, errPtr)
		m.Xduckdb_destroy_result(resPtr)
		m.Xfree(resPtr)
		return 0, 0, 0, fmt.Errorf("duckdb_query(%q) state=%d: %s", sql, rc, msg)
	}

	cols := m.Xduckdb_column_count(resPtr)
	rows := m.Xduckdb_row_count(resPtr)
	// duckdb_value_int64(result*, col, row): col/row are idx_t (i64) here.
	val := m.Xduckdb_value_int64(resPtr, 0, 0)

	m.Xduckdb_destroy_result(resPtr)
	m.Xfree(resPtr)
	return val, cols, rows, nil
}

func main() {
	e := newEnv()
	m := core.New(e, e.Shim) // arg0 Xenv = combined env; arg1 Xwasi = the shim
	m.X_initialize()         // run the wasm's ctors / start function

	// duckdb_library_version() -> const char* (static string in module memory).
	verPtr := m.Xduckdb_library_version()
	fmt.Printf("duckdb_library_version() = %q\n", goString(m, verPtr))

	h, err := openConnect(m)
	if err != nil {
		fmt.Println("OPEN/CONNECT FAILED:", err)
		os.Exit(1)
	}
	fmt.Printf("opened :memory:, db=%d con=%d\n", h.db, h.con)

	// The headline: SELECT 1 through pure Go.
	for _, sql := range []string{"SELECT 1", "SELECT 42"} {
		val, cols, rows, err := queryInt64(m, h, sql)
		if err != nil {
			fmt.Printf("%-44s -> ERROR: %v\n", sql, err)
			continue
		}
		fmt.Printf("%-44s -> value=%d (cols=%d rows=%d)\n", sql, val, cols, rows)
	}

	// A multi-statement aggregate to prove the engine, not just a constant fold.
	const agg = "CREATE TABLE t(x INT); INSERT INTO t VALUES (7),(35); SELECT sum(x) FROM t"
	if val, cols, rows, err := queryInt64(m, h, agg); err != nil {
		fmt.Printf("AGG -> ERROR: %v\n", err)
	} else {
		fmt.Printf("%-44s -> value=%d (cols=%d rows=%d)\n", "SELECT sum(x) FROM t  [=42]", val, cols, rows)
	}

	// A deliberately bad query: must come back as a DuckDB error string via the
	// caught C++ exception, NOT a process abort.
	if _, _, _, err := queryInt64(m, h, "SELECT * FROM no_such_table"); err != nil {
		fmt.Printf("bad query caught (not aborted): %v\n", err)
	} else {
		fmt.Println("bad query unexpectedly SUCCEEDED")
	}

	// Report any residual I/O the in-memory path touched (should be empty).
	if log := e.Shim.Log; len(log) > 0 {
		fmt.Printf("\nshim stub hits during run (%d):\n", len(log))
		for _, l := range log {
			fmt.Println("  ", l)
		}
	} else {
		fmt.Println("\nshim stub hits: NONE (in-memory path stayed clean)")
	}

	m.Xduckdb_disconnect(allocOutWith(m, h.con))
	m.Xduckdb_close(allocOutWith(m, h.db))
}

// allocOutWith writes a single pointer value into a fresh 4-byte slot and returns
// the slot offset. duckdb_disconnect/duckdb_close take duckdb_connection*/
// duckdb_database* (a pointer to the handle), so the handle value must live in
// module memory and we pass its address.
func allocOutWith(m *core.Module, v int32) int32 {
	slot := allocOut(m, 4)
	mem := *m.Xmemory().Slice()
	binary.LittleEndian.PutUint32(mem[slot:], uint32(v))
	return slot
}
