# Spike T2: the size wall (does the Go compiler survive a DuckDB-scale transpiled file?)

Status: complete, verified (2026-06-08), with a real-scale follow-up from convergence (2026-06-09).
Companion to `SPIKE-T1-cpp-exceptions.md` (the other hard axis, exceptions) and the runbook
`duckdb-purego-poc-runbook.md`.

## The question

`wasm2go` emits a SINGLE self-contained Go source file (no multi-file mode - confirmed in its source
and by the shipping ncruces SQLite, which is one file). A DuckDB-scale module is tens to hundreds of
thousands of functions. So the question: can the Go COMPILER ingest one transpiled file that large,
or is there a ceiling (OOM, hang, hard limit)?

## T2 result: no compiler ceiling at 64k, measured

Built synthetic Go packages of wasm2go-style functions (bounds-checked memory ops, goto/labels, a
call edge) at 2k / 8k / 16k / 32k / 64k functions and measured `go build` (clean cache each,
`/usr/bin/time -l`, macOS arm64):

| funcs | LOC | single-file MB | compile wall | peak RSS |
|------:|----:|---:|---:|---:|
| 2,000 | 48k | 1.0 | 2.8 s | 0.24 GB |
| 16,000 | 384k | 8.5 | 25.2 s | 1.10 GB |
| 32,000 | 768k | 17.0 | 209 s | 2.05 GB |
| **64,000** | **1.54M** | **34** | **953 s (~16 min)** | **3.66 GB** |

- 64k completed cleanly (exit 0), no OOM, no hang. Memory scales ~linearly (~56-67 KB/func), time is
  super-linear (~N^2.2) but NOT runaway (the per-doubling ratio relaxed from 8.3x to 4.6x at the top).
- The synthetic wires all functions into one call cycle (a pessimistic upper bound). A control with
  no inter-function calls was ~2x faster, and disabling inlining (`-gcflags=-l`) cut ~30% more.
- Real production proof: ncruces ships its SQLite (wasm2go output) as ONE 5.94 MB / 164k-line Go file
  (1,842 funcs) that `go build`s in ~6 s / 1.4 GB.

Verdict at 64k: the size wall is real but surmountable, a tolerable batch/CI cost.

## The real-scale follow-up (from convergence, 2026-06-09): the wall is TIME, not RAM

When the actual standalone DuckDB-core wasm was built and transpiled, it came in at **256,946 wasm
functions** (the PoC's wired module reports 257,334), about 4x the 64,338 of the shipped mvp - because
a standalone static build pulls in the full libc with no extension-loading split. The transpiled Go is
490 MB / 18.1 M lines. The convergence `go build` of that single package (`-gcflags=all='-N -l' -p 1`):

```
duckdbconverge/genpkg: compile: signal: terminated
     2326.68 real    2386.72 user    256.13 sys     (38m47s CPU, ~50 min wall)
     6.93 GB maximum resident set size      0 swaps
```

The decisive finding: **peak RAM was 6.93 GB on a 16 GB box, with zero swaps for the build process.**
RAM was never the wall. The compiler ran at 99% CPU, cycling RSS between ~1.2 GB and 6.9 GB across many
full per-function SSA backend passes, genuinely progressing, killed at the 40-min hard wall while still
advancing. The bottleneck is the Go compiler's SERIAL, super-linear compile TIME on 257k functions in
ONE package, not memory.

Implications:

- A bigger-RAM box would NOT help (6.93 GB << 16 GB). This corrects the earlier "needs a larger box"
  framing - it is not memory-bound.
- The levers that DO help: (i) more compile time (it was progressing), (ii) `-gcflags=all='-N -l'`
  (no optimization) to skip the expensive SSA opt passes that dominate the super-linear time - valid
  here because the benchmark proved we want correctness, not runtime speed, and (iii) a
  feature-reduced DuckDB (fewer functions), though the amalgamation has no clean minimal cut.

## Net

At 64k functions the Go compiler is fine (~16 min). The FULL standalone DuckDB at 257k functions is
RAM-fine but compile-TIME-heavy in one serial package, so the practical gate for a runnable build is
compile time (addressed by `-N -l` / patience / fewer functions), not a hardware memory limit. The
transpile path's size axis is therefore tractable with a known, non-fatal caveat. Combined with
T1 (exceptions are a small Go host) and the standalone build (proven), this is what makes the
wasm2go-DuckDB pipeline feasible. The remaining caveat is performance, not feasibility: a SIMD-free
transpiled engine is pure-Go DuckDB SEMANTICS, ~5-10x slower than native (see the runbook perf
section), not DuckDB SPEED.

## Reproduce

The 64k synthetic sweep and the SQLite-scale measurement are scriptable from the runbook. The
real-scale `go build` record is `/tmp/conv_genbuild.log` (the killed `-l` build) and
`/tmp/conv_NL_build.log` (the `-N -l` retry). The transpiled module regenerates via `build.sh` +
`transpile.sh` in this repo.
