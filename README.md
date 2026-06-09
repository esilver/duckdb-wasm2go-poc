# DuckDB-core -> standalone WASM -> wasm2go PoC

Proof of concept for compiling DuckDB to a **standalone** WebAssembly module that
[`ncruces/wasm2go`](https://github.com/ncruces/wasm2go) can transpile into pure Go,
so DuckDB can eventually run on a Go runtime with no CGo and no wazero.

**Result: it works - and it now RUNS.** The full DuckDB v1.5.3 C-API amalgamation
compiles to a standalone wasm in the required shape, wasm2go ingests it with zero
unsupported opcodes, the generated Go compiles, and **DuckDB executes real SQL in
pure Go with `CGO_ENABLED=0`** - no cgo, no wasm runtime - including aggregates,
`GROUP BY`/`ORDER BY`, string functions, and caught C++ exceptions. The runnable
results, the four build walls and how each fell, the benchmark, and error-message
handling are in **[RESULTS-runnable-poc.md](RESULTS-runnable-poc.md)**. This README
documents the build/transpile/shape step; see [Status](#status) for what's proven.

Built and verified 2026-06-08 on macOS arm64 (16 GB).

## Why this exact shape

wasm2go needs a **standalone** module: one that *defines and exports* its own
linear memory and function table, with **no `dylink.0` custom section** and **no
GOT imports**.

DuckDB's own wasm build (`Makefile` `wasm_mvp`/`wasm_eh`) uses
`-DWASM_LOADABLE_EXTENSIONS=1`, which produces an Emscripten `MAIN_MODULE`
*side-module* (has `dylink.0`, GOT imports, an *imported* memory) - the wrong
shape; wazero/wasm2go cannot consume it. So this PoC does **not** use
duckdb-wasm's CMake. It compiles the DuckDB **amalgamation** directly with `emcc`
as a standalone module.

Two further constraints:

- **No SIMD.** wasm2go has zero SIMD support (it errors on opcode `0xFD`), so the
  module must be built with `-mno-simd128` and no `simd128` target feature.
- **Legacy (opcode-free) exceptions.** Use `-fexceptions`
  /`-sDISABLE_EXCEPTION_CATCHING=0`, **not** `-fwasm-exceptions`. Legacy lowering
  keeps the module free of the EH-proposal opcodes (`try_table`, `catch_all`,
  `rethrow`, ...). The C++ `throw`/`catch` is realized through host-provided
  `invoke_*` trampolines and the `__cxa_*` ABI, which a Go host wires up (see the
  T1 reference host described under [Running it](#running-it-not-yet-done)).

## Layout

| Path | Committed? | What it is |
|------|-----------|------------|
| `README.md` | yes | this file |
| `build.sh` | yes | the exact emcc command that links the standalone wasm |
| `verify_shape.sh` | yes | shape verification (dylink/memory/table/GOT/EH/validate) on a wasm |
| `transpile.sh` | yes | run wasm2go + parsecheck on a wasm |
| `flagtest.cpp` | yes | tiny throwing C++ TU used to lock the flag set first |
| `flagtest.wasm` | yes | the tiny TU built standalone (20 KB) - minimal shape proof |
| `flagtest_gen.go` | yes | wasm2go output for the tiny TU (parses) |
| `parsecheck/main.go`,`go.mod` | yes | `go/parser` harness that checks generated Go parses |
| `amalg/` | **gitignored** | DuckDB v1.5.3 amalgamation (`duckdb.cpp`/`.h`/`.hpp`), from the release zip |
| `libduckdb-src.zip` | **gitignored** | the downloaded amalgamation zip (4.7 MB) |
| `duckdb/` | **gitignored** | shallow upstream clone (379 MB, used only to inspect scripts) |
| `duckdb_core.wasm` | **gitignored** | the deliverable standalone wasm (85.8 MB) |
| `duckdb_core_gen.go` | **gitignored** | wasm2go output for DuckDB (490 MB) |
| `parsecheck/parsecheck` | **gitignored** | compiled parsecheck binary |

The large binaries are **regenerable from the commands below** and are excluded
from git to keep the repo usable. `build.sh` reproduces `duckdb_core.wasm`;
`transpile.sh` reproduces `duckdb_core_gen.go`.

## Toolchain (versions used)

- emcc (Emscripten) **4.0.6** at `/opt/homebrew/bin/emcc`
- `wasm2go` **v0.4.9**
- Go **1.25.6** (darwin/arm64)
- `wasm-tools` **1.251.0**, `wasm-objdump` (wabt) **1.0.41**
- macOS arm64, 16 GB RAM

## Reproduce

### 1. Get the amalgamation (DuckDB v1.5.3)

The in-repo `scripts/amalgamation.py` in current DuckDB is a gutted library with
**no `__main__`** (its own header: "remnants of the once-proud amalgamation.py")
- running it does nothing. Use the release asset instead:

```sh
gh release download v1.5.3 --repo duckdb/duckdb --pattern 'libduckdb-src.zip'
mkdir -p amalg && (cd amalg && unzip -o ../libduckdb-src.zip)
# -> amalg/duckdb.cpp (24.4 MB, 652,557 lines), amalg/duckdb.h (C API), amalg/duckdb.hpp
```

### 2. Build the standalone wasm

```sh
./build.sh           # wraps the emcc command; writes ./duckdb_core.wasm
```

Key points the flags encode (do **not** change these or the shape breaks):

- `-O0` - **required on a 16 GB machine.** `-O1` whole-module optimization
  balloons clang past ~7 GB on the single 24 MB translation unit and thrashes
  swap; it did not finish in budget here. `-O0` skips those passes (peak ~7.9 GB,
  links in ~112 s). Shape is flag-driven, not opt-driven. For optimized builds,
  split the amalgamation into unity chunks or use a bigger box.
- `-std=c++17` - the amalgamation uses `std::string_view`/`std::optional`/
  `if constexpr`; `-std=c++11` fails.
- `-fexceptions -sDISABLE_EXCEPTION_CATCHING=0` - legacy exceptions. **Never**
  `-fwasm-exceptions`.
- `-mno-simd128` - wasm2go can't read SIMD.
- `-sSTANDALONE_WASM --no-entry -sFILESYSTEM=0 -sALLOW_MEMORY_GROWTH=1` -
  standalone shape, defines+exports its own memory/table.
- **No** `-sMAIN_MODULE` / `-sWASM_LOADABLE_EXTENSIONS` (those make a side-module).
- `-DDUCKDB_NO_THREADS=1 -DDUCKDB_DISABLE_EXTENSIONS`.

### 3. Verify the shape

```sh
./verify_shape.sh ./duckdb_core.wasm
```

### 4. Transpile with wasm2go + parse-check

```sh
./transpile.sh ./duckdb_core.wasm    # wasm2go -> duckdb_core_gen.go, then go/parser
```

## Verified results (this is the deliverable)

Shape of `duckdb_core.wasm` (85.8 MB, DuckDB v1.5.3):

- **No `dylink.0`** - only one custom section, `target_features`.
- **Defines + exports its own memory and table** - `memory[0] -> "memory"`
  (283 pages initial, grows to 32768), `table[0] -> "__indirect_function_table"`
  (47,438 entries). Neither is imported.
- **No GOT imports** - `GOT.func`/`GOT.mem` count = 0.
- **0 real EH opcodes** - strict instruction-position grep for
  `try_table|catch_all|rethrow|delegate|throw_ref` = 0. (A loose substring grep
  hits 2, but those are the *import names* `__cxa_rethrow` /
  `__cxa_rethrow_primary_exception`, part of the legacy ABI - not the wasm opcode.)
- **`wasm-tools validate --features=all,-exceptions` = PASS** - conclusive proof
  the module has no dependency on EH-proposal opcodes.
- `target_features`: bulk-memory(+opt), call-indirect-overlong, multivalue,
  mutable-globals, nontrapping-fptoint, reference-types, sign-ext. **No
  `simd128`, no `exception-handling`.**

Imports - **352 total = 340 `env.*` + 12 `wasi_snapshot_preview1.*`**:

- Legacy C++ EH family (host-wired): `__cxa_throw`,
  `__cxa_find_matching_catch_2`/`_3`, `__cxa_begin_catch`, `__cxa_end_catch`,
  `__resumeException`, `llvm_eh_typeid_for`, and a large `invoke_*` trampoline
  family.
- libc/WASI residual: 12 WASI fns (`fd_write`/`fd_read`/`fd_seek`/
  `clock_time_get`/`environ_*`/...), 21 `env.__syscall_*`
  (`socket`/`bind`/`connect`/`getcwd`/`ftruncate64`/`unlinkat`/...),
  `emscripten_notify_memory_growth`, `getaddrinfo`, `getnameinfo`. These are OS
  stubs the host provides; none are SIMD/EH blockers.

Exports (35) - **all 13 requested C-API symbols present**: `duckdb_open`,
`duckdb_connect`, `duckdb_query`, `duckdb_column_count`, `duckdb_row_count`,
`duckdb_value_int64`, `duckdb_value_varchar`, `duckdb_result_error`,
`duckdb_destroy_result`, `duckdb_disconnect`, `duckdb_close`,
`duckdb_library_version`, plus `malloc`/`free`. Also the EH-ABI support surface
(`setThrew`, `_emscripten_tempret_set`, `__cxa_*_exception_refcount`,
`__cxa_can_catch`, ...) and exported `memory` + `__indirect_function_table`.

wasm2go transpile:

- **No "unsupported opcode" / no `0xFD`.** All 256,946 functions translated.
  ~112 s, peak RSS 8.27 GB.
- `duckdb_core_gen.go` = **513,993,411 bytes (~490 MB), 18,130,309 lines**.
- Generated `Xenv` interface is exactly the legacy-EH ABI
  (`Xinvoke_iii`, `X__cxa_throw`, `Xllvm_eh_typeid_for`, `X__resumeException`, ...),
  so an existing legacy-EH Go host applies directly.
- **`go/parser` parses it: `PARSED OK: package duckdbcore, 257341 top-level
  decls, in 12.82 s`** (peak RSS 7.1 GB). Syntactically valid Go.

The tiny `flagtest.wasm` (a single throwing C++ function) was used to lock the
flag set first and shows the identical correct shape at 20 KB; its
`flagtest_gen.go` also parses.

## Status

**Proven (build/transpile - this README):** the wasm builds in the right shape,
validates without exceptions, has no SIMD, exports the C API, and wasm2go ingests
it into parseable Go.

**Proven (runnable - [RESULTS-runnable-poc.md](RESULTS-runnable-poc.md)):** the
generated Go compiles and **DuckDB executes real SQL end-to-end in pure Go
(`CGO_ENABLED=0`, no cgo, no wasm runtime)** - scalars, aggregates
(`sum/min/max/avg/count`), `GROUP BY`/`ORDER BY`/`LIMIT`, string functions, and C++
exception handling with full error-message text. All four items previously listed
here as "remaining" are now resolved (details in RESULTS):

1. **Memory/build wall** - building the wasm `-Oz` (33-39k *small* functions instead
   of 197k) plus `split_new.py` (chunks wasm2go's giant `New()` table-init, which
   overflowed the Go compiler's liveness bitmap) makes the 490 MB Go compile;
   `-N -l` is scoped to `genpkg` because full Go `-O` OOMs on the package even at
   64 GB.
2. **Host interfaces** - the legacy-EH `Xenv` + WASI/syscall host is implemented
   (`converge/exhost`, `converge/wasishim`).
3. **`go build` on the generated Go** - compiles end to end (~7.5 min one-shot).
4. **Execute a query** - done; aggregates, string functions, and caught exceptions
   all run.

**The one structural limit is performance.** No SIMD (wasm2go can't translate the
`0xFD` opcode family, so the wasm is `-mno-simd128`) plus forced `-N -l` Go put this
**~5-22x slower than native** (widest on SIMD-friendly scan/aggregate, narrower on
join/hash/string-heavy work). This route delivers pure-Go DuckDB **semantics, not
speed** - for real throughput, run native DuckDB out-of-process.
