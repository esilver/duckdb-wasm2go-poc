# DuckDB in pure Go: wasm2go engine pipeline

[![CI](https://github.com/esilver/duckdb-wasm2go-poc/actions/workflows/ci.yml/badge.svg)](https://github.com/esilver/duckdb-wasm2go-poc/actions/workflows/ci.yml)

A working **pure-Go DuckDB**: `CGO_ENABLED=0`, no cgo, no shared libraries, no
wasm runtime. DuckDB **v1.5.3** is compiled to a standalone WebAssembly module
and transpiled to plain Go via [`ncruces/wasm2go`](https://github.com/ncruces/wasm2go)
(pipeline: emcc → wasm → wasm2go → Go), then driven through the DuckDB C API.

This repo is the **engine build pipeline and engineering log**. If you just want
to run SQL from Go, use the published driver
[`duckdb-go-pure`](https://github.com/esilver/duckdb-go-pure). You never need
this pipeline unless you are regenerating the engine.

The engine is usable, not a demo: a go-gettable `database/sql` driver, scalar
**and** aggregate UDFs as Go closures, databases persisted to the host
filesystem, and large third-party corpora passing (below). The default lane
statically links `core_functions`, `json`, and `icu`. The parquet lane proves
`read_parquet` plus Parquet round-trip I/O.

## How it works

```
DuckDB v1.5.3 amalgamation + core_functions + json + icu     (C++)
        │  emcc: standalone wasm, -Oz -DNDEBUG, legacy exceptions, no SIMD
        ▼
duckdb_fs.wasm        (standalone module: owns its memory + function table)
        │  wasm2go -embed -unsafe          (zero unsupported opcodes)
        ▼
genpkg/gen.go         (one giant Go package, linear memory = []byte)
        │  split_new.py  (chunk the function-table init the Go compiler chokes on)
        ▼
converge/             (Go host + database/sql driver, CGO_ENABLED=0)
```

Three host-side ideas make it real:

1. **Legacy-EH exception host** (`converge/exhost`). The wasm uses *legacy*
   (opcode-free) Emscripten exceptions, so C++ `throw`/`catch` lowers to
   `invoke_*` trampolines plus the `__cxa_*` ABI, which the Go host implements
   with `panic`/`recover`. DuckDB treats every failed query as a caught C++
   exception, so error handling works end-to-end with full message text.
2. **WASI/syscall shim with a host filesystem** (`converge/wasishim`): clock,
   rng, stdout, and a file layer, so file-backed databases persist and reopen
   across processes.
3. **Go-closure UDFs via the indirect function table.** wasm2go renders the wasm
   function table as a Go `[]any`. The driver appends a Go closure and hands its
   index to `duckdb_create_scalar_function` as the "C function pointer", and
   DuckDB `call_indirect`s straight back into Go. This makes vectorized scalar
   and aggregate UDFs work with no C involved.

## Reproducing the engine

From a clean clone, bootstrap the gitignored DuckDB v1.5.3 inputs, then run the
heavy build:

```sh
./bootstrap_duckdb.sh   # fetch amalgamation, sparse duckdb-src/, output dirs
./rebuild_fs_all.sh     # build_fs.sh (emcc) -> regen exhost invokes ->
                        #   wasm2go -> split_new.py -> go build
                        # GENOPT=1: also shard (transform_genopt.py) +
                        #   split giant fns -> compile-check optimized layout
```

`GENOPT=1 ./rebuild_parquet.sh` regenerates the same layouts from a pre-staged
parquet-flavored `duckdb_fs.wasm` (the parquet wasm build itself is still a
manual lane). Latest local proof (2026-06-13): `read_parquet` over a fixture and
a `COPY ... TO (FORMAT 'parquet')` round trip both work in pure Go. Point
`DUCKDB_PARQUET_FIXTURE` at a fixture outside the default
`duckdb-src/data/parquet-testing` path. The full rebuild wants tens of GB of RAM
(macOS arm64).

The `GENOPT=1` layout is also what makes performance and browser builds work.
It shards the transpiled engine across packages and splits the giant functions
so the Go compiler can optimize each piece (compiler memory is per-package),
which compiles fully optimized, runs **2.3-2.9x faster** than the no-opt
reference, and unlocked `GOOS=js`/browser builds. The structural gap vs native
DuckDB is SIMD: wasm2go has none, so the wasm is built `-mno-simd128`. Mechanism
and RAM profile: [`module_engine_genopt.go`](converge/duckdb/module_engine_genopt.go).

Invariants the scripts encode:

- **Legacy exceptions only** (`-fexceptions`, never `-fwasm-exceptions`) and
  **no SIMD** (`-mno-simd128`): wasm2go cannot ingest EH-proposal or `0xFD`
  opcodes.
- **Standalone module shape** (`-sSTANDALONE_WASM`, no `-sMAIN_MODULE`): defines
  and exports its own memory and `__indirect_function_table`.
- **`-Oz -DNDEBUG`**: small functions keep the Go compile feasible, and release
  asserts avoid 32-bit-`long` debug-check artifacts.
- The **monolithic** engine (`genpkg`, default-tag reference) compiles only with
  `-gcflags='duckdbconverge/genpkg=-N -l -c=16'` (scoped no-opt) and tests need
  `-vet=off`. The sharded `genopt` layout has no such restriction.
- **wasm2go is pinned**: scripts run
  `go run github.com/ncruces/wasm2go@v0.4.9` (override `WASM2GO_VERSION`), and
  refuse versions below v0.4.7 (output-corruption bug in v0.3.0-v0.4.6, upstream
  issue 31). The version used is recorded in `converge/genpkg/TRANSPILER_VERSION`.

Step-by-step detail lives in the scripts themselves: `bootstrap_duckdb.sh`,
`build_fs.sh`, `rebuild_fs_all.sh`, `rebuild_parquet.sh`, `verify_shape.sh`. To
refresh the published library from a new `gen.go`/`gen.dat`, see "Regenerating
the engine" in the duckdb-go-pure README.

## Results

**DuckDB's own sqllogictest corpus** (`duckdb-src/test/sql/**`), measured
2026-06-12 with the runner in this repo against `duckdb-go-pure v0.3.13`:

> **2,522 PASS / 9 FAIL / 791 SKIP** of 3,322 files, **99.6% excluding skips**.
> Every remaining failure is a documented permanent exclusion that behaves
> byte-identically to the native v1.5.3 CLI, and none is core SQL execution.
> Skips are unsupported test *directives* (`load` restart,
> `require parquet/tpch/...`), not engine failures.

The runner ([`converge/cmd/sqllogic`](converge/cmd/sqllogic/README.md)) is a
standalone Go module implementing DuckDB's sqllogictest dialect (query/statement
records, sort modes, md5, loops, skipif/onlyif, float epsilon). It imports the
published driver, so a fresh checkout reruns the corpus without committing
generated engine packages. Per-failure root causes:
[`KNOWN-LIMITS.md`](converge/cmd/sqllogic/KNOWN-LIMITS.md).

**BigQuery dialect conformance** is owned downstream by
[`googlesqlite`](https://github.com/esilver/googlesqlite). Read its per-spec and
per-case coverage there, not here.

**Pure-Go BigQuery emulator** coverage lives with
[`bigquery-emulator`](https://github.com/esilver/bigquery-emulator/tree/main),
which builds out of the box from a fresh clone with `CGO_ENABLED=0`.

**Performance.** The optimized layout runs **2.3-2.9x faster** than the no-opt
reference (mechanism in "Reproducing the engine" above).

## The repo family

| Repo | What it is |
|---|---|
| **this repo** | the engine build pipeline (emcc → wasm2go → Go), the converge host/driver workspace, the sqllogictest runner, the engineering log |
| [duckdb-go-pure](https://github.com/esilver/duckdb-go-pure) | **the library to use**: go-gettable pure-Go DuckDB `database/sql` driver (v0.3.x, fully-optimized engine, flag-free build, transpiled engine committed in-repo) |
| [googlesqlite](https://github.com/esilver/googlesqlite) | BigQuery/GoogleSQL dialect on the pure-Go engine, with a CI spec-conformance gate and a REPL ([REPRODUCE-PURE-GO.md](https://github.com/esilver/googlesqlite/blob/main/REPRODUCE-PURE-GO.md)) |
| [bigquery-emulator](https://github.com/esilver/bigquery-emulator/tree/main) | the goccy BigQuery emulator running fully pure-Go, `bq`-CLI acceptance-tested |

## Repository map

The root holds the current regeneration path only. Superseded scripts, spikes,
and stale notes were removed (recover from git history if needed). File-by-file:
[`REPO_MAP.md`](REPO_MAP.md).

## Credits

- [DuckDB](https://github.com/duckdb/duckdb), the engine itself (MIT, Copyright
  Stichting DuckDB Foundation). This project compiles unmodified DuckDB v1.5.3
  sources, and all SQL correctness is theirs.
- [ncruces/wasm2go](https://github.com/ncruces/wasm2go), the wasm-to-Go
  transpiler that makes the whole approach possible, and
  [ncruces/go-sqlite3](https://github.com/ncruces/go-sqlite3), whose
  SQLite-via-wasm pattern this follows.
