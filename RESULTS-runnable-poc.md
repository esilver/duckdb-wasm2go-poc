# DuckDB → wasm2go → pure-Go: the RUNNABLE PoC (results)

This extends the original build/transpile PoC to a **running** pure-Go DuckDB:
`CGO_ENABLED=0`, no cgo, no wasm runtime. DuckDB v1.5.3 compiled to standalone
wasm, transpiled to Go by `wasm2go`, driven through its C API, executing real SQL
including aggregates and string functions.

Built and verified 2026-06-09 on macOS arm64, 64 GB.

## What runs (all CGO_ENABLED=0, pure Go)

```
duckdb_library_version() = "v1.5.3"
SELECT 42                                    -> 42
SELECT sum(x) FROM s                         -> 152
SELECT min(x)/max(x)/count(*)/avg(x)         -> 7 / 100 / 4 / 38
SELECT sum(x) FROM s GROUP BY g ORDER BY ... -> 42
SELECT upper('duckdb')                       -> "DUCKDB"
SELECT string_agg(g, ',') ...                -> "a,b"
SELECT printf('%d rows', count(*)) FROM s    -> "4 rows"
bad query  -> caught as a DuckDB error (C++ exception), NOT a process abort
```

Capabilities proven: scalars, arithmetic, DDL (`CREATE TABLE`), DML (`INSERT`),
aggregates (`sum/min/max/avg/count`), `GROUP BY`/`ORDER BY`/`LIMIT`, string
functions, and C++ exception handling end-to-end.

## The four walls and how each fell

1. **`go build` took ~30 min / OOM-risk** at `-O0` (197k functions).
   → Build the wasm with **`-Oz`** (size-optimized): 33–39k *small* functions
   instead of 197k. (Source: the wasm2go author's own "zeroperl" discussion —
   `-O2`/`-O3` create giant functions; `-Oz` prevents aggressive inlining.)

2. **`internal compiler error: NewBulk too big`** — wasm2go's generated `New()`
   initializes the ~31–38k-entry function table as ONE literal in one function,
   overflowing the Go compiler's liveness bitmap.
   → **`split_new.py`** rewrites it into ~38 chunked helper methods. (Re-runnable.)

3. **`sum`/`avg`/`min`/string fns "not in catalog"** — the `libduckdb-src.zip`
   amalgamation is **core-only**; these live in the `core_functions` extension,
   which the zip does not bundle.
   → Compile the **117 `core_functions` TUs** from the v1.5.3 source + a
   `register_core_functions(db)` C shim that calls
   `DuckDB(*instance).LoadStaticExtension<CoreFunctionsExtension>()`.

4. **`string_agg` hit a DuckDB debug assertion** (`__builtin_clzl` self-check, an
   ILP32 artifact of 32-bit wasm `long`).
   → Build release with **`-DNDEBUG`** (smaller, faster; matches production DuckDB).

## Reproducible pipeline

```
./build_with_core.sh duckdb_core_fn.wasm      # 118 TUs (amalgamation + core_functions), -Oz -DNDEBUG
# regen exhost/invokes.go for the wasm's exact invoke_* set (gen-invokes)
wasm2go -embed -unsafe -pkg duckdbcore -o converge/genpkg/gen.go duckdb_core_fn.wasm
python3 split_new.py converge/genpkg/gen.go    # chunk the giant New() table init
cd converge && CGO_ENABLED=0 go build -gcflags='duckdbconverge/genpkg=-N -l -c=16' -o duckdb_run_fn .
./duckdb_run_fn
```
One-shot: `./rebuild_all.sh`. Wall time end-to-end ≈ 7.5 min on this box.

Key flags: `wasm2go -embed -unsafe`; `go build
-gcflags='duckdbconverge/genpkg=-N -l -c=16'` — `-N -l` (no-opt) is REQUIRED on
`genpkg` (full Go optimization OOMs on the 142 MB package even at 64 GB), but
SCOPED to genpkg so the small host packages still optimize (`-c=16` = parallel
backend). Using `all=-N -l` instead is ~12% slower (see Benchmark).

## Benchmark

Query: `SELECT sum(i) FROM range(5_000_000) t(i) WHERE (i % 3) = 0`
(vectorized scan + filter + aggregate, best of 3, pure Go, CGO_ENABLED=0).

| build | time | throughput |
|---|---|---|
| `-Oz` wasm, `-N -l` everywhere (first baseline) | 495 ms | 10.1 M rows/s |
| **`-Oz` wasm, `-N -l` ONLY on genpkg (best)** | **434 ms** | **11.5 M rows/s** |
| `wasm-opt -O4` wasm, `-N -l` Go | 522 ms | 9.6 M rows/s (no gain) |
| full Go `-O` (all packages) | — | OOM (won't compile) |
| `GOGC` tuning | 510–513 ms | no effect |
| **native DuckDB v1.5.3 (CLI)** | **~20 ms wall** (incl. startup) | reference |

**The +12% win:** scope `-N -l` to ONLY the giant `genpkg`
(`-gcflags='duckdbconverge/genpkg=-N -l'`) so the small `exhost`/`wasishim`/`main`
packages compile WITH optimization. `exhost`'s `invoke_*` trampolines wrap *every*
indirect call DuckDB makes (its whole vtable/function-pointer dispatch), so they
are on the hottest path — leaving them unoptimized (as `all=-N -l` did) was pure
overhead. genpkg itself still can't be optimized (OOM), which caps further gains.

vs native: this pure-Go build is **~22× slower wall-clock** on this query. The gap
is widest here because a tight integer `% / sum` scan is native's SIMD-vectorized
best case and our `-N -l`-scalar worst case; for join/hash/string-heavy workloads
the gap narrows toward the ~5–10× range the runbook estimated.

**Benchmark levers are exhausted.** The runtime is bottlenecked by two structural
limits, not tuning:
- **Forced `-N -l` Go** — full Go optimization exhausts memory on this package
  size, so the translated code runs unoptimized. This is why `wasm-opt -O4` does
  not help: the wasm is faster, but the Go that executes it is not optimized.
- **No SIMD** — wasm2go cannot translate the `0xFD` opcode family, so the wasm is
  built `-mno-simd128`. DuckDB's vectorized engine is designed around SIMD; this
  is its single biggest performance lever and it is structurally unavailable on
  this route (maintainer: "doable once Go's SIMD experiment stabilizes").

So this delivers pure-Go DuckDB **semantics**, not DuckDB **speed**.

> **Addendum (2026-06-10): the first structural limit fell.** "Benchmark
> levers are exhausted" was true for the monolithic `-N -l` genpkg, but the
> genopt pipeline (`GENOPT=1 ./rebuild_fs_all.sh`: package sharding via
> `scripts/transform_genopt.py` + giant-function splitting via
> `scripts/split_giant_fns.py`) now compiles the engine **fully optimized** —
> no `-N`, no `-l`, no OOM (0.4–3.4 GB peak per package) — for a **2.3–2.9×**
> speedup over the `-N -l` build measured here, and makes `GOOS=js` builds
> possible. Only the SIMD limit remains structural. See the Performance
> section of [README.md](README.md); ships as `duckdb-go-pure` v0.3.x.

## Error messages (was a limitation — resolved)

`duckdb_result_error` returns null on a failed query: DuckDB's internal error
handling uses a convert-and-**rethrow** pattern (`ErrorData::Throw()`); the rethrow
escapes to `duckdb_query`'s outer `catch(...)`, which returns `DuckDBError` WITHOUT
storing the message in the result. (The C++ RTTI itself is verified working —
`CanCatch(std::exception, duckdb::Exception) = 1` — so this is a narrow rethrow-
propagation detail, not a fundamental flaw.)

**Resolved** without touching the deep rethrow path: `exhost` records every thrown
exception's `what()` message (libc++ `runtime_error` refstring at obj+4) at
`__cxa_throw` time (`Host.LastThrowMessage()`). When `duckdb_result_error` returns
null, the driver falls back to that captured message and surfaces the human part of
DuckDB's JSON error envelope. A bad query now reports e.g.
`Catalog Error: Table with name no_such_table does not exist!` — full error text,
pure Go, exception caught (not aborted).

## Files

| Path | What |
|---|---|
| `build_with_core.sh` | build the standalone wasm WITH core_functions (-Oz -DNDEBUG) |
| `register_core_functions.cpp` | C shim that statically registers core_functions |
| `split_new.py` | chunk wasm2go's giant `New()` table-init (fixes NewBulk) |
| `rebuild_all.sh` | one-shot: wasm → invokes → transpile → split → build → run |
| `converge/main.go` | the C-API driver + breadth tests + benchmark |
| `converge/exhost/`, `converge/wasishim/` | exception host + libc/WASI shim |
| `duckdb-src/` | DuckDB v1.5.3 source (for core_functions); gitignored |
