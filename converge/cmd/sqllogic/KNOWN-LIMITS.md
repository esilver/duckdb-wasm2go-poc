# Known limitations - pure-Go DuckDB engine (sqllogictest corpus)

Root-cause analysis of every file still failing the upstream DuckDB
sqllogictest corpus.

## Current baseline

Measured 2026-06-12 with the standalone runner in this directory, importing
`github.com/esilver/duckdb-go-pure v0.3.13`, over DuckDB v1.5.3 `test/sql/**`:

**3,322 files, 2,522 PASS / 9 FAIL / 791 SKIP, a 99.6 % pass rate excluding
skips.** 47,521 records run, 47,512 passed, wall time 1m49s.

Every remaining failure is a documented permanent exclusion (table below).
Each was re-run in isolation and compared against the official native DuckDB
v1.5.3 CLI at the same commit (14eca11) as `duckdb-src`. In each case this
build behaves byte-identically to the native binary, so the failure is a
property of the build's environmental constraints (single-threaded wasm32,
statically linked ICU/json) or of the upstream test itself rather than an
engine defect.

The 791 skips are unsupported test *directives*, not engine failures:
`load` (persistent-db restart protocol), `require parquet/tpch/httpfs/...`,
`require autocomplete`, `concurrentloop`, and a long tail. Parquet is the
largest feature-shaped hole in the corpus.

## Permanent exclusions - the 9 remaining failures

| File | Justification (verified against the official v1.5.3 CLI, same commit as duckdb-src) |
|------|------------------------------------------------------------------------------------------|
| `test/sql/aggregate/aggregates/test_quantile_disc.test` | **Upstream test bug at this tag.** Expects `1.2` from `quantile_disc(x, 0.8 ORDER BY x DESC)` over `(2),(1)`. The official v1.5.3 binary returns `1` (int32, since `1.2` is not representable in the result type), bit-identical to us, in every variant (DESC modifier, negative quantile, optimizer off). The expectation matches a different, newer development behavior. |
| `test/sql/catalog/test_extension_suggestion.test` | **Static json extension (native parity).** Expects `from_json` to be "not in the catalog, but it exists in the json extension". Because json is statically registered in this build, the official CLI emits the identical `Binder Error: No function matches…` we do. The test requires a no-autoload dynamic-extension environment. |
| `test/sql/copy/csv/rejects/csv_rejects_read.test` | **Static ICU (native parity).** The dr_who.csv rejects probe expects 5 CAST rejects. With ICU loaded the TIMESTAMPTZ/TIMETZ column errors do not produce reject rows, and the official CLI also logs exactly the 3 we log (date/time/timestamp). |
| `test/sql/copy/csv/test_timestamptz_12926.test` | **Static ICU (native parity).** Expects `read_csv(dtypes=[TIMESTAMPTZ])` to reject `1/1/2020`. With ICU loaded the conversion succeeds, and the official CLI succeeds identically. |
| `test/sql/function/generic/test_sleep.test` | **Single-threaded build.** `sleep_ms` raises "requires DuckDB to be compiled with thread support". The wasm build has no threads, by design. |
| `test/sql/json/issues/read_json_memory_usage.test` | **Single-threaded build (native parity).** The expected OOM needs `SET threads=8` times the per-thread JSON buffers to exceed 50MiB. Native v1.5.3 with `threads=1` succeeds at 50MiB exactly like us (probe: count=5). This build is permanently 1 thread. |
| `test/sql/limit/test_batch_limit_filters.test` | **Single-threaded build (native parity).** Expects the batched LIMIT (no STREAMING_LIMIT). Native v1.5.3 with `SET threads=1` also plans `STREAMING_LIMIT`. Plan shape follows from the thread count. |
| `test/sql/timezone/disable_timestamptz_casts.test` | **Static ICU.** The test asserts the setting "has no effect when ICU is not loaded". ICU is statically linked (mirroring the cgo libduckdb), so the disabled-cast Binder Error correctly fires. |
| `test/sql/types/timestamp/test_timestamp_tz.test` | **Static ICU.** Expects `TIMESTAMPTZ::DATE` to error without ICU. With ICU linked the cast correctly succeeds. |

The two ICU entries above (and the json one) are exclusions because the
official native binary the corpus ships against behaves byte-identically to
this build. The tests only pass under upstream's no-extension unittest
configuration, which a statically-linked build cannot emulate.

## Cross-cutting notes

- **Dangling transaction after failed statements.** The engine/driver leaves an
  open transaction after any failed statement, so the next statement on that
  connection fails with "cannot start a transaction within a transaction". The
  runner absorbs this with a sacrificial `ROLLBACK` after every expected error
  outside explicit `BEGIN`. Without that workaround, many additional files
  would fail. The fix belongs in the driver/engine autocommit path.
- **Multi-statement records** (`BEGIN; stmt; stmt`) are split on `;` by the
  runner before execution, because the driver prepares one statement at a time.
  Statement-level behavior is therefore measured, but true single-round-trip
  multi-statement semantics are not.

## History

The corpus reached this baseline through a series of fixes, most of which
landed in the runner (`converge/cmd/sqllogic`), the driver
(`converge/duckdb`), the exception host (`converge/exhost`), and the
hand-written host filesystem (`host_fs.cpp`) rather than in the translated
engine. The per-fix narrative was removed from this document once the baseline
stabilized. Recover it from git history if a specific investigation needs it.
