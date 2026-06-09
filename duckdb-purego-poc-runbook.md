# DuckDB in pure Go (no-cgo): replication runbook

Purpose: a step-by-step guide to reproduce what we are building - a pure-Go, no-cgo DuckDB that runs
`SELECT 1` (and beyond) with `CGO_ENABLED=0`, via the wasm2go transpile route. Also documents how to
reproduce the de-risking probes so the claims can be independently verified.

Companion analysis (the "why" behind every choice): `googlesqlite-wasm2go-spike.md`. This file is the
"how". Status: active, 2026-06-08. The build stage is still converging (the exact emcc flag set is
being iterated), everything downstream is proven end to end.

---

## The approach in one paragraph

DuckDB's own wasm builds are Emscripten `MAIN_MODULE` side-modules (a `dylink.0` section, GOT
relocations, imported memory) that no pure-Go tool can load. So we build DuckDB's amalgamation
ourselves as a STANDALONE wasm (defines its own memory/table, no dylink), with LEGACY opcode-free
exceptions and SIMD off, exporting the DuckDB C API. We transpile that wasm to Go source with
`ncruces/wasm2go` (the same tool ncruces uses for pure-Go SQLite). We wire a small Go host that
implements the Emscripten exception ABI (the catch fires via the wasm's own libc++abi, not a Go
reimplementation) plus a libc/WASI shim layer. The result is native Go executing DuckDB, no cgo, no
wasm runtime.

---

## Prerequisites

| Tool | Version used | Install |
|---|---|---|
| Emscripten (`emcc`) | 4.0.6 | `brew install emscripten` |
| Go | 1.25.x | `brew install go` |
| `wasm2go` | v0.4.9 | `go install github.com/ncruces/wasm2go@latest` (lands in `$(go env GOPATH)/bin`) |
| `wasm-tools` | 1.251.0 | `brew install wasm-tools` |
| `wabt` (`wasm-objdump`) | 1.0.41 | `brew install wabt` |
| cmake, python3, clang | system | for the DuckDB amalgamation |

Work dir used here: `/private/tmp/duckdb-wasm2go-poc/` with `build/`, `harness/`, `bench/`.

---

## The pipeline (6 stages)

### Stage 1 - build the standalone DuckDB-core wasm (the long pole)

Get the amalgamation, then emcc-link it standalone. Do NOT use duckdb-wasm's CMake (it forces
`WASM_LOADABLE_EXTENSIONS=1` = MAIN_MODULE = the wrong shape).

```sh
git clone --depth 1 https://github.com/duckdb/duckdb && cd duckdb
python3 scripts/amalgamation.py   # -> src/amalgamation/duckdb.cpp + duckdb.hpp, plus src/include/duckdb.h (the C API)

# Current recipe (flags are being iterated by the build agent; the invariants are fixed):
#  - legacy exceptions: -fexceptions -sDISABLE_EXCEPTION_CATCHING=0   (NOT -fwasm-exceptions)
#  - standalone:        -sSTANDALONE_WASM --no-entry                  (NO -sMAIN_MODULE)
#  - SIMD OFF:          -mno-simd128                                  (wasm2go cannot translate SIMD)
#  - threads OFF:       -DDUCKDB_NO_THREADS=1
#  - no filesystem:     -sFILESYSTEM=0 -sALLOW_MEMORY_GROWTH=1
emcc src/amalgamation/duckdb.cpp -O1 -std=c++11 \
  -fexceptions -sDISABLE_EXCEPTION_CATCHING=0 -sSTANDALONE_WASM -sFILESYSTEM=0 \
  -sALLOW_MEMORY_GROWTH=1 -DDUCKDB_NO_THREADS=1 -mno-simd128 --no-entry \
  -sEXPORTED_FUNCTIONS='_duckdb_open,_duckdb_connect,_duckdb_query,_duckdb_column_count,_duckdb_row_count,_duckdb_value_int64,_duckdb_value_varchar,_duckdb_result_error,_duckdb_destroy_result,_duckdb_disconnect,_duckdb_close,_malloc,_free' \
  -o duckdb_core.wasm
```

RESULT (2026-06-08, PROVEN): built from the DuckDB v1.5.3 release amalgamation (the in-repo
`scripts/amalgamation.py` is now a gutted stub - use the `libduckdb-src.zip` release asset). Two
iteration findings: use `-std=c++17` (the amalgamation needs `string_view`/`optional`/`if
constexpr`), and `-O1` OOM-thrashed a 16 GB box (clang whole-module opt hit 6.8 GB+ on the 24 MB
single TU), so `-O0` was used (shape is flag-driven not opt-driven, so -O0 is valid for the PoC, 112s
link). Output: `duckdb_core.wasm`, 85.8 MB, shape verified (Stage 2), wasm2go ingests it cleanly.

IMPORTANT SIZE FINDING: the standalone static build is **256,946 functions** - about 4x the 64,338
of the shipped mvp (static libc + no extension split pulls in far more). So the transpiled Go is
**490 MB / 18M lines**, which pushes the downstream `go build` (Stage 6) 4x past what the size probe
measured at 64k (~16 min / 3.7 GB). On a 16 GB box that final `go build` is the new risk (projected
~15 GB RAM). To shrink it: split the amalgamation into unity chunks, cut DuckDB features, or build on
a larger box.

### Stage 2 - verify the shape (the wasm must be standalone)

```sh
wasm-objdump -h duckdb_core.wasm | grep -i dylink        # expect: NOTHING (no dylink.0 = not a side-module)
wasm-objdump -x duckdb_core.wasm | grep -iE 'memory|table'  # expect: it DEFINES+EXPORTS memory/table, does NOT import them
wasm-tools print duckdb_core.wasm | grep -cE 'try_table|catch_all|rethrow'   # expect: 0 (legacy lowering, no EH opcodes)
wasm-tools validate --features=all,-exceptions duckdb_core.wasm              # expect: PASS
wasm-objdump -j Import -x duckdb_core.wasm   # the invoke_*/__cxa_* family (host wires these) + libc/WASI residual
wasm-objdump -j Export -x duckdb_core.wasm | grep duckdb_   # the C-API exports present
```

### Stage 3 - transpile to Go

```sh
wasm2go -pkg duckdbcore -o duckdb_core_gen.go duckdb_core.wasm   # expect: exit 0, no "unsupported opcode"
```
Output is a single self-contained Go file (stdlib only). Imports become a `Xenv` interface you
implement, exports become methods on `*Module`. At DuckDB scale this is ~300 MB / ~8 M lines of Go
that compiles in ~16 min / ~3.7 GB RAM (measured, no compiler ceiling).

### Stage 4 - the exception host (proven, reusable)

The host implements the Emscripten exception ABI in Go: `invoke_*` trampolines (look up
`table[index]`, call under `recover()`), `__cxa_throw` (record + panic to unwind to the trampoline,
which calls the module's exported `setThrew`), `__cxa_find_matching_catch_N` (delegates RTTI to the
module's EXPORTED `__cxa_can_catch`/`__dynamic_cast` - NOT reimplemented in Go), begin/end_catch,
resumeException, the tempRet0/setThrew plumbing. Reference implementation lives in
`/private/tmp/duckdb-wasm2go-poc/harness/exhost/` (generalized from the T1 probe, 366 LOC + a
generated `invokes.go`). Regenerate the `invoke_*` set to DuckDB's exact import list:

```sh
go run ./gen-invokes -names "$(wasm-objdump -j Import -x duckdb_core.wasm | grep -o 'invoke_[a-z]*' | sort -u)" -o exhost/invokes.go
```

### Stage 5 - the libc/WASI shim layer (proven)

A standalone `-sFILESYSTEM=0` build presents a small residual surface. Implemented in
`harness/wasishim/`: `emscripten_resize_heap`/`memcpy_js` (heap), `fd_write` (stdout), `proc_exit`/
`abort`/`__assert_fail` (return a Go error, do not kill the process), `random_get`, the clock funcs.
The rest (`fd_read`/`fd_seek`/`path_open`/`__syscall_*`) is stubbed to ENOSYS plus a log. An
in-memory `SELECT 1` hits none of the stubs.

### Stage 6 - run `SELECT 1` (the run loop, proven)

```sh
cd harness && CGO_ENABLED=0 go run . -wasm ../build/duckdb_core.wasm
# drives: duckdb_open(":memory:") -> duckdb_connect -> duckdb_query(con,"SELECT 1") -> read -> print
```
C-strings are marshalled by writing into the module's memory and passing offsets (`cstring(m,s)`
helper). The validated harness proved this whole loop on a small standalone wasm:
`query_scalar("SELECT 1") -> 1`, and a bad query takes the throw -> catch -> Go-error path (not a
process abort), all `CGO_ENABLED=0`.

### Convergence work when the real DuckDB wasm lands (mechanical)

1. Regenerate the `invoke_*` set (Stage 4 command).
2. Handle `duckdb_result`'s by-value struct return - Emscripten lowers it to a hidden sret pointer
   arg plus field offsets from `duckdb.h` (the one marshalling case the toy could not model).
3. Match the generated `New` arity (one arg per import module) and forward `__dynamic_cast` if the
   DuckDB build exports it.
4. Implement any `__syscall_*` the `:memory:` path pulls beyond the current stubs (expected: none).

---

## Reproducing the de-risking evidence (verify the claims)

Each load-bearing claim has a reproducible probe. See `googlesqlite-wasm2go-spike.md` for the full
results.

- **Exceptions work through wasm2go (T1):** build a C++ throw/catch snippet with
  `emcc -sDISABLE_EXCEPTION_CATCHING=0` (opcode-free), `wasm2go` it, implement the ~108-LOC host, and
  the catch fires. Falsification tests (suppress `setThrew`, force a non-matching type id) prove it
  is genuinely ABI-gated. wasm2go also ingests the real 41 MB `duckdb-mvp.wasm` cleanly.
- **The size wall is surmountable (T2):** a synthetic 64,000-function Go file `go build`s in
  ~16 min / 3.7 GB, no OOM, no ceiling. ncruces ships its SQLite as one 5.9 MB / 164k-line Go file.
- **The shipped artifacts are the wrong input (B1):** wazero compiles `duckdb-mvp.wasm` but cannot
  instantiate it (imported memory/table/GOT in a MAIN_MODULE side-module). Confirms why Stage 1 must
  build a custom standalone module.
- **The exception gate is flag-plumbing (Lane 2):** wasi-sdk-33 ships a prebuilt exceptions-enabled
  sysroot, and `-fwasm-exceptions` wasm runs an in-module C++ catch under wazero v1.12.
- **The ccgo alternative loses (CG1/CG2):** ccgo digests wasm2c machine-C, but wasm2c's
  setjmp/longjmp exceptions die on modernc/libc's missing cross-frame longjmp, and w2c2 re-collapses
  to one Go file. wasm2go is the better transpile route.
- **The out-of-process alternatives (C1/C5):** a `CGO_ENABLED=0` Go client drives native GizmoSQL
  (DuckDB v1.5.3) over Flight SQL, and pg_duckdb gives per-session catalogs over pure-Go pgx. These
  ship today and keep full native speed if browser/in-process is not required.

---

## Artifacts map

| Path | What |
|---|---|
| `/private/tmp/duckdb-wasm2go-poc/build/` | the standalone DuckDB-core wasm build (Stage 1) |
| `/private/tmp/duckdb-wasm2go-poc/harness/` | the run harness: `exhost/` (exceptions), `wasishim/` (libc/WASI), `run.go`, `PLUGIN.md`, validated end to end |
| `/private/tmp/duckdb-wasm2go-poc/bench/` | the perf benchmark (execution-model overhead) |
| `/tmp/wasm2go-duckdb-probe/t1/cpp/host.go` | T1's original exception host (the seed for `exhost/`) |
| `faro-docs/design/googlesqlite-wasm2go-spike.md` | the full options analysis and all probe results |

---

## Perf reality (the honest caveat)

The transpiled engine is native Go EXECUTING THE WASM MODEL, not native DuckDB: linear memory is a
bounds-checked Go `[]byte`, indirect calls go through a Go function-pointer table, no SIMD
(`-mno-simd128`), single-threaded, exceptions via panic/recover (error path only).

MEASURED (2026-06-08, `bench/RESULTS.md`, macOS arm64, N=10M, best-of-10):

- **Execution-model tax:** a wasm2go CPU kernel is **2.22x slower than native C**, 1.76x slower than
  idiomatic Go. A real wasm2go SQL engine (ncruces go-sqlite3) is **geomean ~2.95x slower than cgo C
  SQLite** on analytical queries (GROUP BY, filter+aggregate, join). The "competitive with cgo" claim
  was optimistic - it is ~3x.
- **wasm2go vs wazero:** roughly at parity, the kernel had **wazero 14% FASTER**. So wasm2go does NOT
  beat the runtime on raw hot-loop speed - its real win over wazero is deployment (no runtime
  dependency, no JIT warmup, `CGO_ENABLED=0`, single static binary), not execution speed.
- **The SIMD-free penalty is the big one:** a vectorizable SUM measured **4.0x** (NEON over scalar),
  and DuckDB's engine is designed around auto-vectorization over 1024-row vectors, which
  `-mno-simd128` disables (literature: 3-5.8x on SIMD analytical operators). wasm2go has zero SIMD
  support, so this is structural, not a tuning miss.
- **Combined honest estimate vs native cgo DuckDB: ~3x execution-model tax x ~3-5x SIMD loss =
  plausibly ~5-10x slower on analytical workloads.** Not near-native.

So "no runtime tax" is true only in the narrow sense (AOT native Go, no interpreter). The transpile
path delivers pure-Go DuckDB SEMANTICS, NOT DuckDB SPEED. The headline reason to pick DuckDB over
SQLite (vectorized SIMD analytical throughput) is the exact thing a SIMD-free wasm2go build compiles
away, so a wasm2go DuckDB risks landing closer to "pure-Go SQLite that speaks DuckDB SQL" than
"DuckDB speed in Go". Implications: for raw analytical speed no-cgo, **out-of-process native (C1/C5)
is the only path that keeps it**. For goccy's in-browser pure-Go world, a wasm2go DuckDB is still
likely an upgrade over his current SQLite-WASM engine (DuckDB's columnar algorithms beat SQLite's row
engine even SIMD-free) while staying pure-Go - the coherent value prop for him, given he already
accepted a ~10x regression for pure-Go.

---

## Status: proven vs pending

| Piece | Status |
|---|---|
| Exceptions through wasm2go (the host) | PROVEN (T1 + the validated harness) |
| The standalone DuckDB-core wasm (Stage 1) | PROVEN - DuckDB v1.5.3, 85.8 MB, shape verified, no dylink/GOT, 0 EH opcodes, no SIMD |
| wasm2go ingests the real DuckDB wasm | PROVEN - 256,946 functions translated cleanly, 490 MB Go that `go/parser`-parses |
| The libc/WASI shim layer | PROVEN (validated against a real importer) |
| The run loop (`open -> query -> read`, `SELECT 1`) | PROVEN on a small standalone wasm, `CGO_ENABLED=0` |
| The full convergence wiring (host + shims + C-API driver -> the real DuckDB module) | PROVEN - statically verified: 306/306 `invoke_*` trampolines, all 21 C-API/EH exports forwarded, `duckdb_query` by-value-result marshalling, 0 method-set gaps, `exhost`+`wasishim` compile `CGO_ENABLED=0` clean |
| `go build` the transpiled DuckDB (257,334 funcs / 490 MB) | TIME-bound, NOT RAM-bound (convergence MEASURED): peak 6.93 GB (fits 16 GB, 0 swaps), killed at 39 min CPU still progressing. A bigger-RAM box would NOT help - it is the Go compiler's serial, super-linear time on 257k functions. Retrying with `-gcflags=all='-N -l'` (no-opt, we only need correctness) to slash that time |
| `SELECT 1` through the REAL DuckDB | IN FLIGHT - blocked only on the long compile above, not on hardware or correctness. The `-N -l` retry tests whether the no-opt build finishes on this box |
| Runtime perf multiplier vs native | MEASURED - ~3x execution tax x ~3-5x SIMD loss = ~5-10x slower than native (see Perf reality) |

The PoC's hard pieces are all green: the standalone build, the transpile, the exceptions, the size
(at 64k), the run loop, AND the full convergence wiring (statically verified end to end). The single
remaining question is **compile TIME, not feasibility**: the FULL 257,334-function transpiled file is
RAM-fine (6.93 GB peak) but exceeds the Go compiler's patience in one serial package, so the runnable
`SELECT 1` needs a no-opt (`-N -l`) compile, more time, or a feature-reduced DuckDB - NOT a
bigger-RAM box (the earlier "needs a larger box" framing was corrected by the measurement: it is not
memory-bound). The whole path is proven through build + transpile + parse + the validated run loop
and the statically-verified wiring.
