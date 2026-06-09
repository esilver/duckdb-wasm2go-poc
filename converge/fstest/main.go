// Command fstest exercises the Tier-2 "Path B" host filesystem: the custom
// DuckDB FileSystem compiled into duckdb_fs.wasm (../../host_fs.cpp), whose
// virtuals call env.host_* imports implemented by wasishim/hostfs.go against the
// real OS. Entirely CGO_ENABLED=0, wasm2go-transpiled.
//
// It proves two things:
//  1. PERSISTENCE: open a file-backed DB at a temp path, CREATE TABLE + INSERT,
//     close, then open a SECOND, fresh module (separate wasm memory) at the same
//     path and SELECT sum(x) — expect 42, proving the bytes hit the host disk.
//  2. read_csv_auto: write a CSV to the host, then SELECT count(*) — expect 3.
//
// Each "engine" is an independent *Module/env so the reopen is a genuine cold
// start, not a same-process cache hit.
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"

	core "duckdbconverge/fsgenpkg"

	"duckdbconverge/exhost"
	"duckdbconverge/wasishim"
)

// ---- env wiring: generated *core.Module -> exhost/wasishim (mirrors main.go) --

type modABI struct{ m *core.Module }

func (a modABI) SetThrew(threw, value int32) { a.m.XsetThrew(threw, value) }
func (a modABI) TempretSet(v int32)          { a.m.X_emscripten_tempret_set(v) }
func (a modABI) Table() []any                { return *a.m.X__indirect_function_table() }
func (a modABI) CanCatch(catchType, excType, adjustedPtrSlot int32) int32 {
	return a.m.X__cxa_can_catch(catchType, excType, adjustedPtrSlot)
}
func (a modABI) GetExceptionPtr(excHeader int32) int32 { return a.m.X__cxa_get_exception_ptr(excHeader) }
func (a modABI) DynamicCast(obj, srcType, dstType, offset int32) int32 { return 0 }
func (a modABI) Malloc(n int32) int32                  { return a.m.Xmalloc(n) }
func (a modABI) Free(ptr int32)                        { a.m.Xfree(ptr) }
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
	return &env{Host: host, Shim: shim}
}

// Compile-time proof the wiring satisfies the generated import interfaces. The
// combined *env supplies the exception ABI + emscripten env + env.host_* (the
// last via the embedded *wasishim.Shim); *wasishim.Shim supplies the WASI calls.
var (
	_ core.Xenv                    = (*env)(nil)
	_ core.Xwasi_snapshot_preview1 = (*wasishim.Shim)(nil)
)

// ---- one engine instance ----------------------------------------------------

type engine struct {
	m   *core.Module
	e   *env
	db  int32
	con int32
}

// newEngine builds a fresh module and opens+connects a database at path
// (":memory:" for in-memory), registering core_functions AND the host FS.
//
// CRITICAL: the database FILE is opened DURING duckdb_open(_ext), before any
// post-open hook runs, using config.file_system. So we must install the
// HostFileSystem on a DBConfig BEFORE open: create_config -> attach -> open_ext.
func newEngine(path string) (*engine, error) {
	e := newEnv()
	m := core.New(e, e.Shim)
	m.X_initialize()

	// Build a config whose file_system is our VirtualFileSystem+HostFileSystem.
	cfgSlot := allocOut(m, 4)
	if rc := m.Xduckdb_create_config(cfgSlot); rc != 0 {
		return nil, fmt.Errorf("duckdb_create_config state=%d", rc)
	}
	cfg := readPtr(m, cfgSlot)
	m.Xhost_fs_attach_to_config(cfg) // install HostFileSystem before open

	pathPtr := cstring(m, path)
	dbSlot := allocOut(m, 4)
	errSlot := allocOut(m, 4) // char** out-param for the error string
	rc := m.Xduckdb_open_ext(pathPtr, dbSlot, cfg, errSlot)
	m.Xduckdb_destroy_config(cfgSlot) // open_ext moved file_system out; free the shell
	if rc != 0 {
		msg := goString(m, readPtr(m, errSlot))
		if msg == "" {
			msg = extractMsg(e.Host.LastThrowMessage())
		}
		return nil, fmt.Errorf("duckdb_open_ext(%q) state=%d: %s", path, rc, msg)
	}
	db := readPtr(m, dbSlot)

	m.Xregister_core_functions(db) // sum/avg/strings

	conSlot := allocOut(m, 4)
	if rc := m.Xduckdb_connect(db, conSlot); rc != 0 {
		return nil, fmt.Errorf("duckdb_connect state=%d", rc)
	}
	con := readPtr(m, conSlot)

	m.Xfree(pathPtr)
	m.Xfree(dbSlot)
	m.Xfree(conSlot)
	m.Xfree(cfgSlot)
	m.Xfree(errSlot)
	en := &engine{m: m, e: e, db: db, con: con}
	// In wasm, GetSystemAvailableMemory reports a tiny value, so the buffer pool
	// is too small for the CSV scanner's read buffers. Raise the limit; the wasm
	// build has ALLOW_MEMORY_GROWTH so linear memory grows on demand.
	if err := en.exec("SET memory_limit='2GB'"); err != nil {
		return nil, fmt.Errorf("set memory_limit: %v", err)
	}
	return en, nil
}

func (en *engine) close() {
	en.m.Xduckdb_disconnect(allocOutWith(en.m, en.con))
	en.m.Xduckdb_close(allocOutWith(en.m, en.db))
}

// exec runs a statement, returning a DuckDB error message on failure.
func (en *engine) exec(sql string) error {
	m := en.m
	sqlPtr := cstring(m, sql)
	resPtr := allocOut(m, sizeofDuckdbResult)
	rc := m.Xduckdb_query(en.con, sqlPtr, resPtr)
	m.Xfree(sqlPtr)
	defer func() { m.Xduckdb_destroy_result(resPtr); m.Xfree(resPtr) }()
	if rc != 0 {
		return fmt.Errorf("%s", en.resultError(resPtr))
	}
	return nil
}

// queryInt64 runs sql and returns the scalar at (col0,row0).
func (en *engine) queryInt64(sql string) (int64, error) {
	m := en.m
	sqlPtr := cstring(m, sql)
	resPtr := allocOut(m, sizeofDuckdbResult)
	rc := m.Xduckdb_query(en.con, sqlPtr, resPtr)
	m.Xfree(sqlPtr)
	defer func() { m.Xduckdb_destroy_result(resPtr); m.Xfree(resPtr) }()
	if rc != 0 {
		return 0, fmt.Errorf("%s", en.resultError(resPtr))
	}
	return m.Xduckdb_value_int64(resPtr, 0, 0), nil
}

func (en *engine) resultError(resPtr int32) string {
	if p := en.m.Xduckdb_result_error(resPtr); p != 0 {
		if s := goString(en.m, p); s != "" {
			return extractMsg(s)
		}
	}
	return extractMsg(en.e.Host.LastThrowMessage())
}

func main() {
	exhost.DebugThrow = false

	dir, err := os.MkdirTemp("", "duckdb-fstest-*")
	if err != nil {
		fmt.Println("mkdtemp:", err)
		os.Exit(1)
	}
	dbPath := filepath.Join(dir, "x.duckdb")
	csvPath := filepath.Join(dir, "data.csv")
	// Allow an external cross-check: if FSTEST_DB is set, persist there too (and
	// leave it on disk) so a NATIVE duckdb CLI can independently read it back.
	if p := os.Getenv("FSTEST_DB"); p != "" {
		dbPath = p
	}
	fmt.Printf("temp dir: %s\n", dir)
	fmt.Printf("db path:  %s\n", dbPath)

	fail := false

	// ===== (1) PERSISTENCE: write, close, reopen in a fresh engine =====
	fmt.Println("\n== persistence test ==")
	{
		en, err := newEngine(dbPath)
		if err != nil {
			fmt.Println("open#1 FAILED:", err)
			os.Exit(1)
		}
		for _, sql := range []string{
			"CREATE TABLE t(x INTEGER)",
			"INSERT INTO t VALUES (10),(20),(12)",
			"CHECKPOINT",
		} {
			if err := en.exec(sql); err != nil {
				fmt.Printf("  write %q -> ERROR: %v\n", sql, err)
				fail = true
			} else {
				fmt.Printf("  write %q -> ok\n", sql)
			}
		}
		en.close()
	}
	if fi, err := os.Stat(dbPath); err == nil {
		fmt.Printf("  host file on disk: %s (%d bytes)\n", dbPath, fi.Size())
	} else {
		fmt.Printf("  host file MISSING after close: %v\n", err)
		fail = true
	}
	{
		en, err := newEngine(dbPath) // brand-new module + wasm memory == cold reopen
		if err != nil {
			fmt.Println("reopen#2 FAILED:", err)
			os.Exit(1)
		}
		got, err := en.queryInt64("SELECT sum(x) FROM t")
		if err != nil {
			fmt.Printf("  SELECT sum(x) -> ERROR: %v\n", err)
			fail = true
		} else {
			ok := got == 42
			fmt.Printf("  SELECT sum(x) FROM t -> %d (expect 42) %s\n", got, passfail(ok))
			fail = fail || !ok
		}
		en.close()
	}

	// ===== (2) read_csv_auto over a host file =====
	fmt.Println("\n== read_csv_auto test ==")
	csv := "x,y\n1,a\n2,b\n3,c\n"
	if err := os.WriteFile(csvPath, []byte(csv), 0o644); err != nil {
		fmt.Println("write csv:", err)
		os.Exit(1)
	}
	fmt.Printf("  wrote host CSV: %s (%d bytes)\n", csvPath, len(csv))
	{
		en, err := newEngine(":memory:")
		if err != nil {
			fmt.Println("open(:memory:) FAILED:", err)
			os.Exit(1)
		}
		q := fmt.Sprintf("SELECT count(*) FROM read_csv_auto('%s')", csvPath)
		got, err := en.queryInt64(q)
		if err != nil {
			fmt.Printf("  %s -> ERROR: %v\n", q, err)
			fail = true
		} else {
			ok := got == 3
			fmt.Printf("  count(*) -> %d (expect 3) %s\n", got, passfail(ok))
			fail = fail || !ok
		}
		// Bonus: aggregate a CSV column to prove real row data flows through.
		if sum, err := en.queryInt64(fmt.Sprintf("SELECT sum(x) FROM read_csv_auto('%s')", csvPath)); err == nil {
			fmt.Printf("  sum(x) -> %d (expect 6) %s\n", sum, passfail(sum == 6))
			fail = fail || sum != 6
		} else {
			fmt.Printf("  sum(x) -> ERROR: %v\n", err)
			fail = true
		}
		en.close()
	}

	os.RemoveAll(dir)
	fmt.Println()
	if fail {
		fmt.Println("RESULT: FAIL")
		os.Exit(1)
	}
	fmt.Println("RESULT: PASS — host file persistence + read_csv_auto both work")
}

func passfail(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}
