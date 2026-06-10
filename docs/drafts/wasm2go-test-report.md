# DuckDB wasm2go Test Report

> DRAFT — "Show and tell" discussion post for github.com/ncruces/wasm2go, in the
> spirit of the zeroperl report (#15). Not yet posted.

I transpiled **DuckDB v1.5.3** — a full C++ analytical SQL engine — with wasm2go
and drove it through its C API from pure Go. In short, **it works and it runs**:
`CGO_ENABLED=0`, no cgo, no wasm runtime, executing real SQL including
aggregates, window functions, nested types, and caught C++ exceptions. Tested
against DuckDB's own sqllogictest corpus it passes **99.49% of records**
(43,565 / 43,789), and behind a BigQuery-dialect emulator it scores **986/994
with zero failures** — beating the cgo (native libduckdb) baseline of 972/994
on the same suite.

I'm adding this here as an FYI and a thank-you, and because two pieces may be
useful to others: the build recipe that gets a 42k-function C++ module through
the Go compiler, and a small Go host that makes Emscripten **legacy C++
exception handling** work through wasm2go — which I believe answers the "C++
needs exception handling, dead in the water" concern raised earlier in the
zeroperl thread (e.g. for exiv2).

All repos referenced are public:

- Engine + build pipeline: https://github.com/esilver/duckdb-wasm2go-poc
- Fetchable `database/sql` driver: https://github.com/esilver/duckdb-go-pure
- BigQuery-dialect layer (DuckDB backend branch): https://github.com/esilver/googlesqlite (branch `pure-go-duckdb-backend`)
- End-to-end consumer: https://github.com/esilver/bigquery-emulator

---

## Summary

**Hypothesis:** DuckDB compiled to a *standalone* wasm module (its official wasm
build is an Emscripten side-module — the wrong shape) can be transpiled by
wasm2go into Go that actually compiles and runs, with C++ exceptions handled by
a host-side implementation of the `__cxa_*` ABI.

**Implementation:** compile the DuckDB v1.5.3 amalgamation + the
`core_functions`/`json`/`icu` extensions (118 TUs, statically registered) with
`emcc` as a standalone module, `-Oz -DNDEBUG -mno-simd128 -fexceptions`;
transpile with `wasm2go -embed -unsafe`; add a ~560-line Go exception host and
a WASI shim; wrap the C API in a `database/sql` driver.

**Result:** it compiles in ~7.5 minutes end-to-end and runs everything the
native engine runs, modulo a handful of genuine divergences under
investigation. The two structural costs are compile flags (the generated
package must build with `-N -l`) and speed (no SIMD + unoptimized Go ≈ 5–22×
slower than native, workload-dependent).

---

## Generated code statistics

| Metric | Value |
|---|---|
| wasm module (standalone, `-Oz -DNDEBUG`) | 24 MB |
| wasm functions | ~42.5k |
| `gen.go` (wasm2go `-embed -unsafe`) | 158 MB |
| `go build` of the generated package | requires `-gcflags='<genpkg>=-N -l -c=8'` |
| end-to-end rebuild (emcc → wasm2go → go build) | ≈ 7.5 min (M-series, 64 GB) |

### Module shape

wasm2go needs a standalone module (own memory + table, no `dylink.0`, no GOT
imports), so this does **not** use duckdb-wasm's CMake (`MAIN_MODULE`
side-module). The amalgamation is compiled directly with `emcc`. Two extra
constraints:

- **No SIMD** — `-mno-simd128` (wasm2go errors on the `0xFD` family).
- **Legacy (opcode-free) exceptions** — `-fexceptions` /
  `-sDISABLE_EXCEPTION_CATCHING=0`, *not* `-fwasm-exceptions`. Legacy lowering
  keeps the module free of EH-proposal opcodes; `throw`/`catch` is realized
  through `invoke_*` trampolines and the `__cxa_*` ABI, which a Go host wires
  up (details below).

### The build walls, in order of appearance

1. **`-O0`/`-O2` are unbuildable.** At `-O0` the module had ~197k functions
   (~257k for a full static build); the transpiled 490 MB / 18.1M-line package
   was still compiling at the 40-minute mark when killed. Following the advice
   ncruces gave in the zeroperl thread (`-Oz`, avoid aggressive inlining and
   giant functions) cut it to ~42.5k *small* functions — the single most
   important lever.
2. **`internal compiler error: NewBulk too big`** — `New()` initializes the
   ~38k-entry function table as one literal in one function, overflowing the
   compiler's liveness bitmap. A small re-runnable script
   (`split_new.py`) chunks it into ~38 helper methods. (This was on an older
   wasm2go; #11-era `-embed` already helps.)
3. **Full Go optimization is a memory wall, `-N -l` is not.** Compiling the
   generated package with normal optimization costs roughly **1.2 GB of
   compiler memory per 1,000 functions** — ~53 GB projected for this package,
   and in practice it OOMs a 64 GB machine. With `-N -l` it builds in minutes
   at single-digit GB. The flag must be **scoped to the generated package
   only** (`-gcflags='<modpath>/genpkg=-N -l -c=8'`) so the small host packages
   (the `invoke_*` trampolines are on the hottest path) still optimize — that
   scoping alone was a +12% runtime win over `all=-N -l`. A multi-package
   sharding transform that restores full optimization at ~3 GB per shard
   proved out at prototype scale; happy to share details if there's interest
   in an official multi-file/multi-package output mode.
4. **Debug assertions are ILP32 traps.** `string_agg` died on a
   `__builtin_clzl` self-check (32-bit `long` artifact). `-DNDEBUG` (i.e., a
   release build, matching production DuckDB) fixed it and shrank the module.

---

## The legacy-EH Go host

wasm2go has zero support for the EH-proposal opcodes, but it doesn't need any:
with legacy lowering, all of C++ EH crosses the boundary as ordinary imports
(`invoke_*`, `__cxa_throw`, `__cxa_find_matching_catch_N`, `__resumeException`,
...). The host is one small Go file (~560 lines incl. comments) plus generated
`invoke_*` wrappers, and it deliberately **reimplements no RTTI**: type
matching delegates to the module's own exported `__cxa_can_catch` /
`__dynamic_cast`. DuckDB is an aggressive consumer of EH (it uses exceptions
for control flow, `std::exception_ptr`, convert-and-rethrow), so it flushed
out behaviors a hello-world thrower never hits. Findings, in case anyone else
builds one of these:

1. **`catch (...)` arrives as a NULL typeinfo candidate.** In
   `__cxa_find_matching_catch_N`, a catch-all clause is a candidate whose
   `type_info*` is 0. It must match *unconditionally* — never probe
   `__cxa_can_catch` with a 0 typeinfo. Get this wrong and every `catch (...)`
   in the module (DuckDB has many) aborts instead of catching.
2. **Keep "currently unwinding" separate from "currently caught".** Emscripten
   keeps `exceptionLast` (set by `__cxa_throw`/`__cxa_rethrow`/
   `__resumeException`) distinct from the `exceptionCaught` *stack* (pushed by
   `__cxa_begin_catch`, popped by `__cxa_end_catch`). The separation is
   essential for `throw B` inside a `catch (A)` handler: `__cxa_end_catch`
   runs while B is unwinding and must end *A's* handler, not pop B. A single
   conflated stack corrupts the thrown type on any catch-and-rethrow — the
   symptom in DuckDB was `catch (std::exception &)` silently not matching
   ("Unknown exception in ExecutorTask::Execute").
3. **Exception refcounting must be real.** `std::exception_ptr` capture and
   `std::rethrow_exception` (DuckDB uses both) go through
   `__cxa_increment/decrement_exception_refcount` on the 24-byte
   `__cxa_exception` header that sits in linear memory immediately before the
   thrown object (`{u32 refcount; type_info*; dtor; caught; rethrown;
   adjustedPtr}` — offsets verifiable against the module's own exported
   refcount functions). The host initializes the header at `__cxa_throw` time
   (mirroring Emscripten's `ExceptionInfo.init`) and delegates inc/dec to the
   module's exports so the object is genuinely destroyed at refcount 0;
   keeping the dynamic type in the header is what lets a later
   `std::rethrow_exception` recover it.
4. **A convert-and-rethrow bonus.** DuckDB's C API sometimes loses the error
   message on a failed query (`duckdb_result_error` returns null) because its
   internal `ErrorData::Throw()` rethrows a freshly-built exception. The host
   records the `what()` message at `__cxa_throw` time, so the driver can still
   report *why* a query failed. Cheap and very worth it.

Falsification tests (force `threw=0`, force a non-matching type id) confirm
the catch is genuinely gated on the unwind flag and the type-id compare — it's
not "catch fires by accident".

---

## Results

### DuckDB's own sqllogictest corpus (3,322 `.test` files)

| Metric | Value |
|---|---|
| files PASS | 2,309 (69.5%) |
| files FAIL | 224 (6.7%) |
| files SKIP | 789 (23.8% — `load`/`require parquet`/tpch/httpfs/etc.) |
| pass rate of files actually run | 91.2% |
| **records passed** | **43,565 / 43,789 = 99.49%** |
| wall time | 1m48s |

Most remaining failures are fidelity-of-error-message, struct field ordering,
and harness-shaped issues; a small number look like genuine engine divergences
(e.g. `UHUGEINT -> FLOAT` cast behavior, storage compression codec selection)
that I'm bisecting across the emcc/wasm2go/`-N -l` layers before reporting
anything upstream to DuckDB.

### BigQuery-dialect conformance (via googlesqlite)

Swapping this pure-Go engine in as the backend of a GoogleSQL emulation layer:
**986 PASS / 0 FAIL / 8 SKIP over 994** — zero failures, *exceeding* the cgo
(native libduckdb) baseline of 972/994 on the identical suite. (The cgo
build's 14 failures turned out to be a wiring gap fixable on both backends,
but "the wasm2go transpile is not the weakest link in the chain" was the point
of the exercise.)

### Performance

`SELECT sum(i) FROM range(5_000_000) t(i) WHERE (i % 3) = 0` (native DuckDB's
SIMD-vectorized best case, our worst): 434 ms pure Go vs ~20 ms native — ~22×.
Join/hash/string-heavy workloads narrow toward the ~5–10× range. The two
structural caps were forced `-N -l` on the generated package and `-mno-simd128`;
both are wasm2go-adjacent rather than engine problems. The first has since
been REMOVED (package sharding + function splitting — see the addendum below);
the second still awaits Go's SIMD experiment.

---

## Lessons learned

1. **wasm2go handles "very large C++" fine if the wasm is built `-Oz`.** The
   zeroperl thread's advice generalizes: small functions are everything. 42.5k
   functions / 158 MB of generated Go is routine *with `-N -l` scoped to the
   generated package*.
2. **Full Go optimization scales at ~1.2 GB per 1,000 functions** on this kind
   of generated code — that, not parse/typecheck, is the wall. Scoped `-N -l`
   sidesteps it; multi-package output would remove it.
3. **Legacy EH + a small Go host is a complete answer for C++ exceptions
   today.** No EH opcodes needed. The three subtleties above (NULL-typeinfo
   catch-all, last-vs-caught separation, real refcounting) are the entire
   hard part.
4. **The transpiled engine is semantically faithful at depth.** 99.49% of
   DuckDB's own test records, and dialect-suite parity *better* than cgo, was
   far beyond what I expected from a 158 MB generated file.

Thanks @ncruces — wasm2go made a pure-Go DuckDB go from "obviously impossible"
to a week of plumbing.

---

## Addendum (2026-06-10, internal): where this stands now

Everything above was measured on the unoptimized (-N -l) engine. Since then,
on the same transpiled output:

- Corpus: **2,513 PASS / 20 FAIL / 789 SKIP files** (99.2% excluding skips;
  canonical tally in `converge/cmd/sqllogic/KNOWN-LIMITS.md`); every
  previously suspected transpiler/engine divergence was investigated and
  **exonerated** (all were our glue: test runner, driver locking,
  exception-host catch dispatch, hand-written FS shim).
- Engine speed: ~10x compound vs the -N -l build (textual inlining -> sharded
  full optimization -> function splitting enabling the Go inliner).
- The function splitter (scripts/split_giant_fns.py, pipeline step 4d) removes
  the two Go-compiler-limit blockers (65536-SSA-block cap; inliner IR
  explosion) — zero-flag `go build ./...` and GOOS=js both work; V8
  instantiates the 461MB module in ~250ms and runs the full query battery.
