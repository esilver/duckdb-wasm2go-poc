# Known limitations: pure-Go DuckDB (sqllogictest corpus)

Root-cause analysis of every file still failing DuckDB's `test/sql/**` corpus.

## Baseline

Measured 2026-06-12 with the runner in this directory against
`github.com/esilver/duckdb-go-pure v0.3.13`, over DuckDB v1.5.3:

> **3,322 files, 2,522 PASS / 9 FAIL / 791 SKIP, 99.6% excluding skips.**
> 47,521 records run, 47,512 passed, wall time 1m49s.

Every remaining failure is a documented permanent exclusion (below). Each was
re-run in isolation against the official native v1.5.3 CLI at the same commit as
`duckdb-src` (14eca11), and behaves byte-identically. So each failure is a
property of the build's environmental constraints (single-threaded wasm32,
statically linked ICU/json) or of the upstream test, not an engine defect.

The 791 skips are unsupported test *directives*, not engine failures: `load`
(persistent-db restart), `require parquet/tpch/httpfs/...`, `require autocomplete`,
`concurrentloop`, and a long tail. Parquet is the largest feature-shaped hole.

## Permanent exclusions: the 9 failures

Verified against the official v1.5.3 CLI, same commit as duckdb-src.

| File | Justification |
|------|---------------|
| `aggregate/aggregates/test_quantile_disc.test` | **Upstream test bug at this tag.** Expects `1.2` from `quantile_disc(x, 0.8 ORDER BY x DESC)` over `(2),(1)`. The official binary returns `1` (int32, `1.2` not representable in the result type), bit-identical to us in every variant. The expectation matches newer dev behavior. |
| `catalog/test_extension_suggestion.test` | **Static json (native parity).** Expects `from_json` to be missing-but-suggested from the json extension. With json statically registered, the official CLI emits the identical `Binder Error: No function matches…`. Needs a no-autoload dynamic-extension environment. |
| `copy/csv/rejects/csv_rejects_read.test` | **Static ICU (native parity).** The dr_who.csv probe expects 5 CAST rejects. With ICU loaded the TIMESTAMPTZ/TIMETZ columns don't produce reject rows, and the official CLI also logs exactly the 3 we do. |
| `copy/csv/test_timestamptz_12926.test` | **Static ICU (native parity).** Expects `read_csv(dtypes=[TIMESTAMPTZ])` to reject `1/1/2020`. With ICU loaded the conversion succeeds, identically on the official CLI. |
| `function/generic/test_sleep.test` | **Single-threaded build.** `sleep_ms` raises "requires DuckDB to be compiled with thread support". The wasm build has no threads, by design. |
| `json/issues/read_json_memory_usage.test` | **Single-threaded build (native parity).** The expected OOM needs `threads=8` × per-thread JSON buffers to exceed 50MiB. Native v1.5.3 at `threads=1` succeeds at 50MiB exactly like us. This build is permanently 1 thread. |
| `limit/test_batch_limit_filters.test` | **Single-threaded build (native parity).** Expects batched LIMIT (no STREAMING_LIMIT). Native v1.5.3 at `threads=1` also plans `STREAMING_LIMIT`. Plan shape follows from thread count. |
| `timezone/disable_timestamptz_casts.test` | **Static ICU.** Asserts the setting "has no effect when ICU is not loaded". ICU is statically linked (mirroring the cgo libduckdb), so the disabled-cast Binder Error correctly fires. |
| `types/timestamp/test_timestamp_tz.test` | **Static ICU.** Expects `TIMESTAMPTZ::DATE` to error without ICU. With ICU linked the cast correctly succeeds. |

The static-ICU and static-json entries are exclusions because the official
binary the corpus ships against behaves byte-identically to this build. They
pass only under upstream's no-extension unittest configuration, which a
statically-linked build cannot emulate.

## Cross-cutting notes

- **Dangling transaction after failed statements.** The driver leaves an open
  transaction after any failed statement, so the next statement fails with
  "cannot start a transaction within a transaction". The runner absorbs this
  with a sacrificial `ROLLBACK` after every expected error outside an explicit
  `BEGIN`. Without it many more files would fail. The fix belongs in the
  driver/engine autocommit path.
- **Multi-statement records** (`BEGIN; stmt; stmt`) are split on `;` by the
  runner, because the driver prepares one statement at a time. Statement-level
  behavior is measured, but true single-round-trip multi-statement semantics are
  not.

## History

The baseline was reached through fixes that mostly landed in the runner
(`converge/cmd/sqllogic`), driver (`converge/duckdb`), exception host
(`converge/exhost`), and host filesystem (`host_fs.cpp`) rather than the
translated engine. The per-fix narrative was removed once the baseline
stabilized. Recover it from git history if needed.
