# DuckDB in pure Go: wasm2go engine pipeline

This repo started as a proof of concept ("can DuckDB survive a trip through
[`ncruces/wasm2go`](https://github.com/ncruces/wasm2go)?"). It is now the
**build pipeline and engineering log for a working pure-Go DuckDB**:
DuckDB **v1.5.3** (with the `core_functions`, `json` and `icu` extensions
statically linked) compiled to a standalone WebAssembly module, transpiled to
plain Go, and driven through the DuckDB C API — **`CGO_ENABLED=0`, no cgo, no
shared libraries, no wasm runtime**.

The result is not a toy. It ships as a go-gettable `database/sql` driver
([`duckdb-go-pure`](https://github.com/esilver/duckdb-go-pure)), supports
scalar and aggregate UDFs written as Go closures, persists databases to the
host filesystem, and passes large third-party test corpora (numbers below).

## Results

### BigQuery dialect conformance (googlesqlite spec suite, 994 specs)

| Backend | PASS | FAIL | SKIP |
|---|---|---|---|
| **pure-Go DuckDB (this engine)** | **986** | **0** | 8 |
| native DuckDB via cgo (baseline) | 972 | 14 | 8 |

The pure-Go backend **exceeds the cgo baseline with zero failures**. The cgo
build's 14 `search`/`objectref` failures turned out to be a routing gap in the
emulator (the pure-Go function bodies always existed but were never wired on
the DuckDB path); the fix applies to both backends. The 8 skips are
proto/graph features with no assertable cases. See
[`googlesqlite` REPRODUCE-PURE-GO.md](https://github.com/esilver/googlesqlite/blob/pure-go-duckdb-backend/REPRODUCE-PURE-GO.md).

### DuckDB's own sqllogictest corpus (`duckdb-src/test/sql/**`)

| Metric | Result |
|---|---|
| Test files | **2,513 PASS / 20 FAIL / 789 SKIP** of 3,322 — **99.2% pass rate excluding skips** (2026-06-10) |
| Skips | unsupported test *directives* (`load` restart protocol, `require parquet/tpch/httpfs/...`), not engine failures |

Measured with the runner committed in this repo at
[`converge/cmd/sqllogic`](converge/cmd/sqllogic/main.go) (a Go implementation
of DuckDB's sqllogictest dialect: query/statement records, sort modes, md5
hashing, loops, skipif/onlyif, float epsilon comparison). The canonical
root-cause classification of every remaining failure — error-message
fidelity, logging parity, a handful of deep-semantics singles — is
[`converge/cmd/sqllogic/KNOWN-LIMITS.md`](converge/cmd/sqllogic/KNOWN-LIMITS.md);
none of the 20 is core SQL execution.

### Downstream: a pure-Go BigQuery emulator

[`bigquery-emulator`](https://github.com/esilver/bigquery-emulator/tree/pure-go-duckdb-backend)
builds **out of the box from a fresh clone** with `CGO_ENABLED=0` (one `replace`
directive in `go.mod`, pointing `goccy/googlesqlite` at the pure-Go fork tag),
and is acceptance-tested end-to-end with the **real `bq` CLI** against the
running emulator.

### Performance

This route delivers pure-Go DuckDB semantics first; speed is now respectable
rather than painful. The **optimized engine layout** (`-tags genopt` in this
workspace; what `duckdb-go-pure` ships since v0.3.x) compiles **fully
optimized** — no `-N`, no `-l` — runs queries **2.3–2.9× faster** than the
no-opt reference build, and is what made `GOOS=js`/browser builds possible at
all. The remaining structural gap vs native DuckDB is SIMD: wasm2go has no
SIMD support, so the wasm is built `-mno-simd128`.

*History:* the original engine package could only be compiled with Go
optimization disabled (`-N -l` — full optimization OOMed the Go compiler on a
package that size), which put that build at roughly **5–22× slower than
native DuckDB** (widest on SIMD-friendly scan/aggregate, narrower on
join/hash/string-heavy work). Benchmarks and the tuning-lever log are in
[RESULTS-runnable-poc.md](RESULTS-runnable-poc.md).

**How the optimized engine works:** two transforms run via
`GENOPT=1 ./rebuild_fs_all.sh`: a multi-package shard
([scripts/transform_genopt.py](scripts/transform_genopt.py), step 4c) — the
42.5k generated `fnNNN` methods become free functions across
`converge/genopt/{core,shardNN}` (cross-shard calls through `TBL_FnN`
func-vars), so the Go optimizer runs per-package — followed by a function
splitter ([scripts/split_giant_fns.py](scripts/split_giant_fns.py), step 4d)
that splits every >8k-line transpiled function (4k in shard20) into
semantically identical part-functions, which keeps the inliner's IR bounded.
With both, every genopt package compiles **fully optimized, no `-l`**
(0.4–3.4GB peak per package vs >50GB OOM before) and queries run
**2.3–2.9× faster** than the `-N -l` build of the same source (1.9× on the
sqllogictest corpus wall clock). In this workspace the engine is selected
with `-tags genopt` alone (see
[`converge/duckdb/module_engine_genopt.go`](converge/duckdb/module_engine_genopt.go));
`-gcflags='duckdbconverge/genopt/...=-c=1'` is **optional** RAM bounding for
the compile, not a requirement. Note the transform
runs on the **pre-inline** transpile: the step-4b textual inliner pre-expands
the IR at source level, which OOMs the optimizer the same way (>28GB
observed) — the two optimizations do not compose. The splitter is also what
makes `GOOS=js` builds possible at all: the largest function (36.5k lines)
exceeded the Go compiler's 65536-SSA-block hard cap. The monolithic `genpkg`
(default tag, textually inlined, `-N -l`) remains the reference engine. The
optimized layout ships as `duckdb-go-pure` **v0.3.2** — flag-free for
consumers since v0.3.1 (plain `go build`, no tags, no gcflags).

## How it works

```
DuckDB v1.5.3 amalgamation + core_functions + json + icu     (C++)
        │  emcc: standalone wasm, -Oz -DNDEBUG, legacy exceptions, no SIMD
        ▼
duckdb_fs.wasm        (standalone module: owns its memory + function table)
        │  wasm2go -embed -unsafe          (zero unsupported opcodes)
        ▼
genpkg/gen.go         (one giant Go package; linear memory = []byte)
        │  split_new.py  (chunk the function-table init the Go compiler chokes on)
        ▼
converge/             (Go host + database/sql driver, CGO_ENABLED=0)
```

Three host-side ideas make it real:

1. **Legacy-EH exception host** (`converge/exhost`). The wasm is built with
   *legacy* (opcode-free) Emscripten exceptions, so C++ `throw`/`catch`
   becomes `invoke_*` trampolines + the `__cxa_*` ABI — which a Go host
   implements with `panic`/`recover`. DuckDB's error handling (every failed
   query is a caught C++ exception) works end-to-end with full message text.
2. **WASI/syscall shim with a host filesystem** (`converge/wasishim`):
   clock, rng, stdout, and a file layer so file-backed databases persist and
   reopen across processes.
3. **Go-closure UDF callbacks via the indirect function table.** wasm2go
   renders the wasm function table as a Go `[]any` of funcs. The driver
   appends a Go closure to that table and hands its index to
   `duckdb_create_scalar_function` as the "C function pointer"; DuckDB
   `call_indirect`s straight back into Go. This is what makes vectorized
   scalar **and aggregate** UDFs possible with no C involved.

The wasm-shape requirements (standalone module, no `dylink.0`, no GOT
imports, no SIMD, no EH-proposal opcodes) and how each build wall fell
(`-Oz` vs the 197k-function `-O0` build, the `NewBulk too big` compiler
limit, bundling `core_functions`, `-DNDEBUG`) are written up in
[RESULTS-runnable-poc.md](RESULTS-runnable-poc.md) and the spike notes
([SPIKE-T1](SPIKE-T1-cpp-exceptions.md), [SPIKE-T2](SPIKE-T2-size-wall.md),
[SWAP-BLUEPRINT.md](SWAP-BLUEPRINT.md)).

## The repo family

| Repo | What it is |
|---|---|
| **this repo** | the engine build pipeline (emcc → wasm2go → Go), the converge host/driver workspace, the sqllogictest runner, and the engineering log |
| [esilver/duckdb-go-pure](https://github.com/esilver/duckdb-go-pure) | **the library to use**: go-gettable pure-Go DuckDB `database/sql` driver (v0.3.x, fully-optimized engine, flag-free build), transpiled engine committed in-repo |
| [esilver/googlesqlite](https://github.com/esilver/googlesqlite) (branch `pure-go-duckdb-backend`) | BigQuery/GoogleSQL dialect on the pure-Go engine — 986/994 conformance, plus an interactive REPL ([CLI-PURE-GO.md](https://github.com/esilver/googlesqlite/blob/pure-go-duckdb-backend/CLI-PURE-GO.md)) |
| [esilver/bigquery-emulator](https://github.com/esilver/bigquery-emulator/tree/pure-go-duckdb-backend) | the goccy BigQuery emulator running fully pure-Go, `bq`-CLI acceptance-tested |

If you just want to run SQL from Go, start at **duckdb-go-pure** — you never
need this repo's pipeline unless you are regenerating the engine.

## Reproducing the engine

One command rebuilds everything from the DuckDB v1.5.3 sources on this
machine class (macOS arm64; the transpile/compile steps want tens of GB of
RAM):

```sh
./rebuild_fs_all.sh   # build_fs.sh (emcc) -> regen exhost invokes ->
                      # wasm2go -> split_new.py -> go build
                      # GENOPT=1: also transform_genopt.py (shard) ->
                      # split_giant_fns.py (no '-l' needed) -> compile check
```

Key invariants the scripts encode:

- **Legacy exceptions only** (`-fexceptions`, never `-fwasm-exceptions`) and
  **no SIMD** (`-mno-simd128`) — wasm2go cannot ingest EH-proposal or `0xFD`
  opcodes.
- **Standalone module shape** (`-sSTANDALONE_WASM`, no `-sMAIN_MODULE`):
  defines and exports its own memory and `__indirect_function_table`.
- **`-Oz -DNDEBUG`** on the wasm: small functions keep the Go compile
  feasible; release asserts avoid 32-bit-`long` debug-check artifacts.
- The **monolithic** engine package (`genpkg`, the default-tag reference
  engine) compiles only with `-gcflags='duckdbconverge/genpkg=-N -l -c=16'`
  (scoped no-opt; everything else optimizes normally) and tests need
  `-vet=off`. The sharded `genopt` layout has no such restriction — it
  compiles fully optimized.
- **wasm2go is pinned**: the scripts invoke
  `go run github.com/ncruces/wasm2go@v0.4.9` (override with
  `WASM2GO_VERSION`), the rebuild scripts refuse versions below v0.4.7
  (output-corruption bug in v0.3.0–v0.4.6, upstream issue 31), and the
  version used is recorded in `converge/genpkg/TRANSPILER_VERSION`.

Step-by-step detail, the exact emcc flags, shape verification
(`verify_shape.sh`), and the build-wall narrative live in
[RESULTS-runnable-poc.md](RESULTS-runnable-poc.md) and
[duckdb-purego-poc-runbook.md](duckdb-purego-poc-runbook.md). To refresh the
published library from a new `gen.go`/`gen.dat`, see "Regenerating the
engine" in the duckdb-go-pure README.

## Credits

- [DuckDB](https://github.com/duckdb/duckdb) — the engine itself (MIT,
  Copyright Stichting DuckDB Foundation). This project compiles unmodified
  DuckDB v1.5.3 sources; all SQL correctness is theirs.
- [ncruces/wasm2go](https://github.com/ncruces/wasm2go) — the wasm-to-Go
  transpiler that makes the whole approach possible, and
  [ncruces/go-sqlite3](https://github.com/ncruces/go-sqlite3), whose
  SQLite-via-wasm pattern this follows.
