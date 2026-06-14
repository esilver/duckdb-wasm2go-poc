# Historical validation harness

Runs a wasm2go-transpiled, C-API-shaped wasm in **pure Go (`CGO_ENABLED=0`)**.
Validated end-to-end on `poc.wasm` (a tiny standalone C++ module shaped like the
DuckDB C API) and on `wasitest/wasiprobe.wasm` (which forces the WASI/libc
surface).

The active DuckDB engine now lives in `../converge`. This directory is kept for
the exception/WASI harness pattern and the live `gen-invokes` helper.

## Re-running

The runnable root command depends on generated `genpkg` output and is excluded
from default builds. Recreate the artifacts first:

```sh
./build-poc.sh
CGO_ENABLED=0 go run -tags harness_generated .
go test -tags harness_generated ./...
go test ./...                              # plain run: validates helper pkgs, no cgo/wasm2go needed
```

The raw `.cc` files are tagged `ignore`, so a plain `go test ./...` validates the
helper packages without cgo or generated output.

## Building the wasm

Build DuckDB's C API the way `poc.cc` is built (see the `emcc` line in
`build-poc.sh`):

```
emcc ... -O1 -fexceptions -sDISABLE_EXCEPTION_CATCHING=0 \
     -sSTANDALONE_WASM -sFILESYSTEM=0 -mno-simd128 --no-entry \
     -sEXPORTED_FUNCTIONS='_duckdb_open,_duckdb_close,_duckdb_connect,\
       _duckdb_disconnect,_duckdb_query,_duckdb_destroy_result,\
       _duckdb_column_count,_duckdb_row_count,_duckdb_value_int64,\
       _duckdb_result_error,_malloc,_free'
```

> [!IMPORTANT]
> `_malloc` and `_free` **must** be exported: the harness allocates wasm memory
> through them for C-string args and out-params.

## 1. Transpile

```
wasm2go -pkg duck -o duckgen/gen.go duckdb.wasm
```

One Go file. Inspect the generated import interfaces and constructor:

```
sed -n '/type Xenv = interface/,/^}/p'                      duckgen/gen.go
sed -n '/type Xwasi_snapshot_preview1 = interface/,/^}/p'   duckgen/gen.go
grep -n '^func New('                                        duckgen/gen.go
```

wasm2go generates **one interface per import module** and `New` takes **one arg
per module, alphabetical**. A build importing both `env` and
`wasi_snapshot_preview1` yields `New(v0 Xwasi_snapshot_preview1, v1 Xenv)`.
`poc.wasm` imports only `env` (one arg), `wasiprobe.wasm` imports both (two).
Match whatever `New` is generated. Both args receive `Init(any)`, so bind
idempotently to guard against double-binding.

## 2. Wire the env

No code changes in `exhost`/`wasishim`. Copy the adapters from `run.go`
(`modABI`, `memABI`) and the `wasitest` `envArg`, swapping `poc.Module` →
`duck.Module`:

- **`env` arg** = a struct embedding `*exhost.Host` (exception ABI) **and**
  `*wasishim.Shim` (the emscripten `env` methods: `emscripten_resize_heap`,
  `emscripten_memcpy_js`, `emscripten_notify_memory_growth`, `__syscall_*`,
  time). Its `Init` binds `exhost.ModuleABI` and the shim's memory.
- **`wasi_snapshot_preview1` arg** (if present) = the **same** `*wasishim.Shim`
  (it also carries `fd_write`, `clock_time_get`, `proc_exit`, `random_get`,
  `environ_*`).

`exhost.ModuleABI` is satisfied by forwarding to these **module exports**:
`XsetThrew`, `X_emscripten_tempret_set`, `X__indirect_function_table`,
`X__cxa_can_catch`, `X__cxa_get_exception_ptr`, `Xmalloc`, `Xfree`, and direct
memory reads/writes.

> [!CAUTION]
> RTTI is the module's own exported `__cxa_can_catch`, never reimplemented in
> Go. If the build also exports `__dynamic_cast`, forward `DynamicCast` to it
> (the validation build did not, and single-inheritance catches route through
> `__cxa_can_catch`).

## 3. Check the residual import surface

```
wasm-objdump -j Import -x duckdb.wasm | grep -vE 'invoke_|__cxa_|llvm_eh|__resume'
```

Everything the exception ABI needs is already covered by `exhost` (full invoke
arity matrix + the `__cxa_*` surface). For the rest:

- Import already in `wasishim` (implemented or stubbed): done.
- `invoke_<sig>` missing from `exhost/invokes.go`: regenerate trampolines
  **exactly** for this wasm:
  ```
  names=$(wasm-objdump -j Import -x duckdb.wasm | grep -oE 'invoke_[a-z]+' | sort -u | paste -sd,)
  go run ./gen-invokes -names "$names" -o exhost/invokes.go
  ```
  (Default baseline is a finite superset: all-i32 args 0..16 + single-wide-arg
  sigs, 400 trampolines. `-names` pins it to DuckDB's exact ~205-import shape:
  ~182 i32-ret, ~16 f64-ret, ~5 f32-ret, rest void.)
- A NEW import that is neither: add a method `X<importname>` to `wasishim.Shim`
  returning ENOSYS and logging. An in-memory `SELECT 1` should hit **zero**
  stubbed paths (`shim.Log` empty), as the `wasiprobe` validation showed.

## 4. Drive the C API

The cstring-marshalling primitives in `run.go` carry over unchanged:

| helper | purpose |
| --- | --- |
| `cstring(m, s)` | `malloc(len+1)`, write `s`+NUL into module memory, return offset (a wasm `char*`) |
| `goString(m, ptr)` | read a NUL-terminated string back out of module memory |
| `allocOut(m, n)` | reserve `n` zeroed bytes for an out-param (`int64*`, `char**`, `duckdb_database*`...) |
| `readU64(m, ptr)` / `readPtr(m, ptr)` | read an 8-byte value / 4-byte pointer out-param |

DuckDB scalar flow (replacing `poc.cc`'s `db_open`/`query_scalar`):

```go
dbSlot  := allocOut(m, 4)
m.Xduckdb_open(cstring(m, ":memory:"), dbSlot)         // -> DuckDBSuccess(0)
db := readPtr(m, dbSlot)

conSlot := allocOut(m, 4)
m.Xduckdb_connect(db, conSlot)
con := readPtr(m, conSlot)

resPtr := allocOut(m, /* sizeof(duckdb_result) */ 64)  // duckdb_result is a struct, by value
rc := m.Xduckdb_query(con, cstring(m, "SELECT 1"), resPtr)
if rc != 0 {
    errPtr := m.Xduckdb_result_error(resPtr)           // char* into wasm memory
    return errors.New(goString(m, errPtr))             // error, not a process abort
}
cols := m.Xduckdb_column_count(resPtr)
rows := m.Xduckdb_row_count(resPtr)
val  := m.Xduckdb_value_int64(resPtr, 0, 0)            // col 0, row 0 -> 1

m.Xduckdb_destroy_result(resPtr)
m.Xduckdb_disconnect(conSlot)
m.Xduckdb_close(dbSlot)
```

- `duckdb_result` is passed **by value**. Emscripten lowers a by-value struct
  return to a hidden first pointer arg (sret), so confirm `Xduckdb_query`'s
  arity: if it has an extra leading pointer param, `allocOut` a
  `sizeof(duckdb_result)` buffer and pass it first. Read field offsets from the
  generated accessor calls or `duckdb.h`.
- Call `m.X_initialize()` once after `New(...)` to run the wasm's
  constructors/start function before any C-API call.

## Proven vs. remaining

PROVEN end-to-end on standalone wasm, pure Go, CGO off:

- exception host (throw → invoke trampoline → setThrew → landing pad →
  `find_matching_catch` with **real RTTI via the module's `__cxa_can_catch`** →
  `begin_catch` → `what()` via vtable → `end_catch`), with falsification tests
  proving the catch depends on the threw-flag and the RTTI match.
- WASI/libc shim (`fd_write` stdout, `clock_time_get`, heap growth) against a
  real importer with zero stub hits.
- multi-import `New(wasiArg, envArg)` wiring.
- cstring/out-param marshalling for args and return values.

The DuckDB-specific work (exact `invoke_*` regeneration, `duckdb_result` layout
handling, residual syscall coverage) has moved to the top-level `converge`
module.
