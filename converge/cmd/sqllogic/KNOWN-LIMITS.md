# Known limitations — pure-Go DuckDB engine (sqllogictest corpus)

Canonical root-cause analysis of every file still failing the upstream DuckDB
sqllogictest corpus.

**Provenance.** Corpus: `duckdb-src/test/sql` — 3,322 `.test` files
(`.test_slow` excluded). Runner: `converge/cmd/sqllogic` (binary snapshot
`/tmp/slt-salvage`), executed with cwd = `duckdb-src`, 30 s/file timeout.
Baseline report: `/tmp/sqllogic_salvage.txt` (2026-06-09):

| | files | % of executed |
|---|---|---|
| PASS | 2,309 | 69.5 % |
| FAIL | **224** | 6.7 % |
| SKIP (unsupported directives: `load`, `require parquet/tpch/httpfs/...`) | 789 | 23.8 % |
| **Pass rate excluding skips** | | **91.2 %** |

**2026-06-10 tail-sweep update.** The nested-fidelity lane took the corpus to
2,443 PASS / 90 FAIL; this lane re-classified all 90 and fixed the
runner/driver-side classes. New baseline (`/tmp/sqllogic_tail_final.txt`):
**2,489 PASS / 44 FAIL / 789 SKIP — 98.3 % pass rate excluding skips**; the
44 still-failing files are a strict subset of the previous 90 (zero new
failures) and all engine-side:

- **P (runner limitations) — FIXED.** Expected rows now split like upstream's
  `StringUtil::Split(line, "\t")` (empty fields dropped, applied only as a
  fallback when the strict split disagrees with the column count), and
  multi-variable `foreach type,min,max tok,tok,tok` iterates correctly. All 16
  P files plus `read_json_dates` (the third multi-var foreach user) pass.
- **D (BIGNUM decode) — FIXED** in the driver (`exotic.go bignumString`):
  varint blob → exact decimal string. All bignum files pass.
- **E (VARIANT decode) — FIXED** in the driver (`variant.go`): full binary
  decode of the physical 4-child struct; cells deliver the exact
  `Value::CastAs(VARCHAR)` string (ARRAY items raw, OBJECT values
  quoted-if-needed). The runner renders VARIANT children raw at nested
  positions (`faceVariant`). All variant files pass.
- **F (GEOMETRY decode) — FIXED** in the driver (`exotic.go geometryString`):
  WKB → WKT port of `Geometry::ToStringRecursive` (Z/M flags, EMPTY parts,
  fmt-style coordinates). All geo files pass.
- **G (UUID-as-INT128) — FIXED** in the driver: UUID cells render canonically
  (`BaseUUID::ToString` MSB flip) on every path (flat, nested, UDF args).
- **L (host-FS gaps) — PARTIAL.** `__syscall_getcwd` is now implemented in
  wasishim (fixes "Could not get working directory!");
  `allowed_directories_install` progresses to an extension-directory
  resolution gap (`Cannot access directory ""`). The remaining L items live in
  the C++ `host_fs.cpp` compiled into the wasm (a rebuild, not driver work):
  `~` expansion ignores the `home_directory` setting (opener not plumbed),
  `file://` URLs unsupported (attach_fsspec), persistent secrets hit the
  stubbed emscripten `__syscall_mkdirat`/`stat64` family,
  `disabled_filesystems`/local-FS-metadata are not enforced against
  HostFileSystem.
- **C (temporal) — `test_icu_timezone` FIXED** (runner now accepts
  `UTC±HH[:MM]` fixed-offset TimeZone spellings and renders pre-1582
  TIMESTAMPTZ dates with ICU's hybrid Julian/Gregorian calendar).
  `test_icu_calendar` remains: rendering under non-Gregorian SESSION CALENDARS
  (`SET Calendar='indian'/'islamic-umalqura'`) would require reimplementing
  those ICU calendars in the runner; the japanese (hybrid) rows now match.

Everything else still failing is engine-side and keeps its classification
below (I, J, K remnants, N, O, Q, R singles, A remnants:
`read_csv_glob` relative `glob('*/*.csv')` count and `csv_rejects_read`
rejects-table row count, plus M remnants logging_csv/logging_types).

**The 44 remaining files** (first-failure symptom):

- `test/sql/aggregate/aggregates/test_null_aggregates.test` — [unexpected error: Invalid Error: Unoptimized statement differs from original result!] line 314
- `test/sql/aggregate/aggregates/test_quantile_disc.test` — [wrong result] line 97
- `test/sql/attach/attach_fsspec.test` — [unexpected error: IO Error: HostFileSystem: failed to open ? (errno #)] line 13
- `test/sql/attach/attach_home_directory.test` — [statement error: message mismatch] line 20
- `test/sql/catalog/test_extension_suggestion.test` — [statement error: message mismatch] line 9
- `test/sql/catalog/view/test_loosely_qualified_view_sql.test` — [hash mismatch] line 43
- `test/sql/copy/csv/csv_home_directory.test` — [unexpected error: IO Error: No files found that match the pattern ?] line 17
- `test/sql/copy/csv/glob/read_csv_glob.test` — [wrong result] line 211
- `test/sql/copy/csv/rejects/csv_rejects_read.test` — [wrong row count] line 238
- `test/sql/copy/csv/test_timestamptz_12926.test` — [statement error: expected error, got success] line 8
- `test/sql/error/error_position.test` — [statement error: message mismatch] line 9
- `test/sql/extensions/allowed_directories_install.test` — [statement error: message mismatch] line 15
- `test/sql/function/generic/test_sleep.test` — [unexpected error: Invalid Input Error: ThreadUtil::SleepMs requires DuckDB to be compiled with thre] line 6
- `test/sql/function/numeric/set_seed_for_sample.test` — [hash mismatch] line 16
- `test/sql/function/operator/test_in_empty_table.test` — [unexpected error: Conversion Error: Could not convert string ? to INT#] line 8
- `test/sql/join/iejoin/iejoin_projection_maps.test` — [wrong result] line 23
- `test/sql/join/iejoin/test_iejoin_events.test` — [wrong result] line 48
- `test/sql/json/issues/read_json_memory_usage.test` — [statement error: expected error, got success] line 25
- `test/sql/json/test_json_serialize_plan.test` — [wrong result] line 10
- `test/sql/limit/test_batch_limit_filters.test` — [wrong result] line 14
- `test/sql/logging/logging_csv.test` — [wrong result] line 18
- `test/sql/logging/logging_types.test` — [wrong row count] line 15
- `test/sql/optimizer/predicate_factoring.test` — [wrong result] line 92
- `test/sql/optimizer/test_in_rewrite_rule.test` — [unexpected error: Conversion Error: Could not convert string ? to INT#] line 15
- `test/sql/order/hugeint_order_by_extremes.test` — [unexpected error: Invalid Error: Unoptimized statement differs from original result!] line 14
- `test/sql/sample/test_sample_too_big.test` — [unexpected error: Out of Memory Error: Allocation failure] line 28
- `test/sql/secrets/create_secret_expression.test` — [unexpected error: IO Error: Failed to initialize persistent storage directory. (original] line 21
- `test/sql/settings/errors_as_json.test` — [statement error: message mismatch] line 11
- `test/sql/settings/test_disabled_file_systems.test` — [statement error: expected error, got success] line 37
- `test/sql/settings/test_disabled_local_filesystem_metadata.test` — [statement error: expected error, got success] line 22
- `test/sql/storage/checkpoint/test_checkpoint_failure_delayed_commit.test` — [INTERNAL/fatal error] line 32
- `test/sql/storage/checkpoint/test_checkpoint_failure_delayed_rollback.test` — [INTERNAL/fatal error] line 32
- `test/sql/storage/checkpoint/test_checkpoint_failure_on_detach.test` — [INTERNAL/fatal error] line 20
- `test/sql/storage/wal/wal_promote_version.test` — [unexpected error: Catalog Error: Table with name T does not exist!] line 32
- `test/sql/timezone/disable_timestamptz_casts.test` — [unexpected error: Binder Error: Casting from TIMESTAMP to TIMESTAMP WITH TIME ZONE without a] line 22
- `test/sql/timezone/test_icu_calendar.test` — [wrong result] line 110
- `test/sql/transactions/statement-preprocessor/multistatement_is_transactional_chained_BEGIN.test` — [statement error: expected error, got success] line 24
- `test/sql/transactions/statement-preprocessor/multistatement_is_transactional_chained_BEGIN_body_COMMIT.test` — [statement error: expected error, got success] line 24
- `test/sql/transactions/statement-preprocessor/multistatement_is_transactional_chained_PRAGMA_BEGIN.test` — [statement error: expected error, got success] line 21
- `test/sql/types/nested/map/map_from_entries/data_types.test` — [statement error: message mismatch] line 125
- `test/sql/types/timestamp/test_timestamp_tz.test` — [statement error: expected error, got success] line 24
- `test/sql/types/type/test_make_get_type.test` — [wrong result] line 4
- `test/sql/window/test_lead_lag.test` — [unexpected error: Out of Range Error: Overflow in subtraction of INT# (# - -#)!] line 121
- `test/sql/window/test_volatile_independence.test` — [wrong result] line 10

---

Every one of the 224 failing files was re-run and classified for this document
(verbose rerun 2026-06-09, reproduced 224/224; two extra files appeared only
under `-j 4` load and are listed under "Flaky" at the end). Classification is
by the file's *first* failing record; many files additionally contain later
records that would hit other buckets.

**Reading the fixability column**

- `runner` — false failure; the engine is right (or right enough), the test
  harness is too strict. Fix in `converge/cmd/sqllogic`.
- `driver` — fixable in the Go driver / result-decode / host layer
  (`converge/duckdb`, `converge/exhost`) without touching the engine.
- `engine` — needs a change in the translated engine (genpkg) or a wasm
  rebuild with patched C++.
- `in-flight` — a fix is already being worked in parallel.
- `wontfix` — intentional divergence of this build (documented, not planned).

---

## Summary table

| # | Bucket | Files | Root cause | Fixability | Example |
|---|--------|------:|------------|------------|---------|
| A | FS glob / multi-file path resolution | 43 | `HostFileSystem` glob never matches: `read_csv('…/*.csv')`, `glob()`, directory-as-glob, `[ab]`/`?` patterns and multi-file readers all return "No files found that match the pattern" (or 0 rows from `glob()`), and two error-message tests fail only because the glob errors first. | driver (**in-flight**) | `test/sql/copy/csv/glob/read_csv_glob.test` |
| B | Nested-value rendering fidelity | 46 | Driver-side stringification of LIST/STRUCT/MAP differs from DuckDB's VARCHAR cast: strings inside nested values are not single-quoted (`[utm_source=]` vs `['utm_source=']`), embedded quotes are doubled instead of backslash-escaped (`'''hello'''` vs `'\'hello\''`), unnamed structs render `{'': 1}` instead of `(1)`, FLOAT renders with double precision (`0.8999999761581421` vs `0.9`), DOUBLE renders scientific (`-6.3517824e+10` vs `-63517824000.0`), TIME/TIMESTAMP inside nested values render as bare/`1970-01-01`-prefixed/date-only values. | driver | `test/sql/cast/string_to_list_cast.test` |
| C | Temporal decode: range wrap, precision, TZ offset | 25 | Scalar TIME/TIMESTAMP/TIMESTAMPTZ decode goes through Go `time.Time` nanoseconds: timestamps outside ±292 years of 1970 wrap (BC dates and `290309-12-22 (BC)` come back as 1696–2262 garbage), `TIMESTAMP_NS`/`%n`/`TIMESTAMP(7)`/`TIME_NS` are truncated to µs, TIMESTAMPTZ renders as UTC with no `±HH` offset, `TIME '24:00:00'` renders as `1970-01-02`. | driver | `test/sql/types/timestamp/timestamp_limits.test` |
| D | BIGNUM decode | 10 | Every `BIGNUM` (varint) value decodes to `NULL` in the result path; engine-side arithmetic itself is exercised but nothing survives decode. | driver | `test/sql/types/bignum/test_bignum_sum.test` |
| E | VARIANT decode | 6 | `VARIANT` values decode to `NULL` (top-level, inside lists, after storage round-trip, and from `variant_extract`). | driver | `test/sql/types/variant/json_cast.test` |
| F | GEOMETRY decode | 6 | `GEOMETRY` values decode to `NULL` (`POINT (1 2)` expected); includes WKB round-trip hash mismatch and the v1.4.3 storage-compat file. | driver | `test/sql/types/geo/geometry_crs.test` |
| G | UUID-as-INT128 paths | 4 | On some paths (JSON→UUID cast, BLOB→UUID try_cast, UUID through window/unnest) UUID values render as the raw HUGEINT (`-170141183…`) instead of `00000000-…`. | driver | `test/sql/types/uuid/test_uuid_cast.test` |
| H | PIVOT multi-statement prepare | 12 | Top-level `PIVOT` without an explicit IN-list expands (inside DuckDB) into multiple statements; the driver's prepare path rejects it: "Cannot prepare multiple statements at once!". | driver | `test/sql/pivot/top_level_pivot_syntax.test` |
| I | Explicit-tx aborted-state semantics | 3 | After an error inside `BEGIN; <failing stmt>` the transaction must enter the aborted state ("Current transaction is aborted (please ROLLBACK)"); ours lets the next query succeed. | engine | `test/sql/transactions/statement-preprocessor/multistatement_is_transactional_chained_BEGIN.test` |
| J | Stats-range `max − min` overflow | 5 | A shared statistics-range computation does unchecked `max - min`: full-range INT16/INT64/INT128 columns abort GROUP BY, ORDER BY (hugeint radix) and LAG/LEAD with "Out of Range Error: Overflow in subtraction of INT16 (32767 - -32768)!". | engine | `test/sql/aggregate/group/group_by_limits.test` |
| K | Error-text fidelity tail | 7 | Right rejection, wrong message/shape: prepared-parameter count text, missing "exists in the json extension" suggestion, `errors_as_json` MISSING_ENTRY type, error `position` field, error-precedence (duplicate-map-key fires before catalog "already exists"). | engine | `test/sql/catalog/table/create_table_parameters.test` |
| L | Host-FS / sandbox environment gaps | 7 | `~` not expanded, `file://` URLs unsupported, `getcwd` unimplemented, `mkdir .duckdb` unimplemented (persistent secrets), `SET disabled_filesystems`/local-FS-metadata not enforced against HostFileSystem. Also a real driver bug: HostFileSystem errors print literal placeholders — `failed to open "{}" (errno {})`. | driver | `test/sql/attach/attach_home_directory.test` |
| M | Logging subsystem parity | 5 | `current_query_id()` returns `UINT64_MAX` (then `+1` overflows), `duckdb_logs` contains extra QueryLog rows, FileSystem TRACE ops are never logged (host FS bypasses the engine logger), logged-CSV column types differ. | driver+engine | `test/sql/logging/logging_context_ids.test` |
| N | Checkpoint/WAL deep storage semantics | 4 | Delayed CHECKPOINT after DETACH escalates to `FATAL Error: Detached database 'db', but CHECKPOINT during DETACH failed` instead of the expected scoped error; `wal_promote_version` loses the table on WAL replay. | engine | `test/sql/storage/checkpoint/test_checkpoint_failure_on_detach.test` |
| O | ICU statically built in | 2 | These tests assume a build *without* ICU ("Setting has no effect when ICU is not loaded"); our build links ICU in, so TIMESTAMPTZ casts behave like upstream-with-ICU and the no-ICU expectations fail. | wontfix | `test/sql/timezone/disable_timestamptz_casts.test` |
| P | Runner limitations | 16 | (a) 14 files: upstream expected blocks contain alignment/trailing tabs (`Bob\t\t6.5`, `2\t12\t`); the runner splits strictly on single tabs → "expected row has N tab-separated values". (b) 2 files: multi-variable `foreach type,min,max …` is not parsed, so `${type}` reaches the parser. | runner | `test/sql/aggregate/aggregates/test_weighted_avg.test` |
| Q | RNG sequence parity | 2 | After `set seed`, `random()`/`USING SAMPLE` do not reproduce DuckDB's pcg sequence (and window sharing of volatile expressions evaluates in a different order). | engine | `test/sql/function/numeric/set_seed_for_sample.test` |
| R | Deep-semantics singles | 15 | One-off engine behaviors — itemized below. | mixed | `test/sql/join/iejoin/iejoin_projection_maps.test` |
| S | String compression codec selection | 6 | VARCHAR/BLOB segments checkpoint as `uncompressed`; `dict_fsst` is never selected, so `pragma_storage_info` checks fail (data itself round-trips correctly — every failure is the codec-name probe). | engine | `test/sql/storage/compression/string/simple.test` |
| | **Total** | **224** | | | |

### R — deep-semantics singles, itemized

| File | Root cause | Fixability |
|------|------------|------------|
| `test/sql/join/iejoin/iejoin_projection_maps.test` | **Correctness bug**: IEJoin returns wrong aggregate over join result (`256987` vs `252652`). Highest-priority item in this doc. | engine |
| `test/sql/optimizer/predicate_factoring.test` | **Correctness bug**: factoring `(a=1 AND b>3) OR (a=1 AND c<5)` yields `NULL` where DuckDB yields `false`. | engine |
| `test/sql/aggregate/aggregates/test_quantile_disc.test` | `quantile_disc`/`percentile_disc` with `ORDER BY … DESC` modifier: DuckDB returns its descending-interval result (`1.2`), ours returns the plain discrete element (`1`). | engine |
| `test/sql/function/operator/test_in_empty_table.test` | `int_col IN ('a','b','c','d','e')` (≥5 elements): DuckDB compares as VARCHAR collection; ours eagerly casts the literals to INT32 and errors. | engine |
| `test/sql/optimizer/test_in_rewrite_rule.test` | Same IN-list VARCHAR-fallback gap. | engine |
| `test/sql/types/numeric/uhugeint_try_cast.test` | `UHUGEINT_MAX::FLOAT` → `inf` (cast bug; should be ≈3.4e38). | engine |
| `test/sql/sample/test_sample_too_big.test` | `TABLESAMPLE RESERVOIR(1000000000)` allocates the full reservoir up front → "Out of Memory Error: Allocation failure"; DuckDB clamps to input size. | engine |
| `test/sql/json/issues/read_json_memory_usage.test` | Opposite memory problem: `SET memory_limit='50MiB'` is not enforced — the read *succeeds* where DuckDB raises Out of Memory. | engine |
| `test/sql/function/generic/test_sleep.test` | `sleep_ms` raises "requires DuckDB to be compiled with thread support" — single-threaded wasm build. | wasm-rebuild / wontfix |
| `test/sql/catalog/view/test_loosely_qualified_view_sql.test` | View SQL is not re-qualified against the view's own database/schema when read via `db1.v1` from another catalog. | engine |
| `test/sql/types/type/test_make_get_type.test` | `get_type(NULL)` returns SQL NULL instead of JSON `"NULL"`. | engine |
| `test/sql/copy/csv/test_timestamptz_12926.test` | CSV reader with `dtypes=[TIMESTAMPTZ]` accepts `1/1/2020` (sniffer date format applied) where DuckDB requires a strict TIMESTAMPTZ cast and errors. | engine |
| `test/sql/limit/test_batch_limit_filters.test` | EXPLAIN plan-shape: we keep `STREAMING_LIMIT` where upstream's batched path avoids it. | engine / wontfix |
| `test/sql/json/test_json_serialize_plan.test` | `json_serialize_plan` output schema differs from upstream's (`LOGICAL_PROJECTION`/`LOGICAL_GET` names absent). | engine / wontfix |
| `test/sql/pragma/profiling/test_custom_profiling_optimizer_settings.test` | Custom profiling metrics for optimizer phases (`optimizer_join_order`) are never > 0. | engine |

---

## Cross-cutting notes (not counted above)

- **Dangling transaction after failed statements.** The engine/driver leaves an
  open transaction after any failed statement; the very next statement on that
  connection fails with "cannot start a transaction within a transaction". The
  runner absorbs this with a sacrificial `ROLLBACK` after every expected error
  outside explicit `BEGIN` (6,017 issued per corpus run). Without that
  workaround, hundreds of additional files would fail. Fix belongs in the
  driver/engine autocommit path; bucket I is the visible remnant of the same
  area inside explicit transactions.
- **Multi-statement records** (`BEGIN; stmt; stmt`) are split on `;` by the
  runner before execution, because the driver cannot prepare multiple
  statements at once (the same restriction behind bucket H). Statement-level
  behavior is therefore measured, but true single-round-trip multi-statement
  semantics are not.
- **789 skips** are unsupported test *directives*, not engine failures:
  `load` (persistent-db restart protocol, 386), `require parquet` (224),
  `require autocomplete` (79), `require tpch/tpcds` (34), `require httpfs`
  (19), `concurrentloop` (15), and a long tail. They overstate nothing about
  engine quality but should not be forgotten: parquet is the largest
  feature-shaped hole in the corpus.

## Flaky under parallel load (`-j 4`), not in the canonical 224

| File | Symptom |
|------|---------|
| `test/sql/index/art/nodes/test_art_prefix_transform_deprecated_create.test` | exceeded the 30 s/file timeout under load; passes in the baseline run |
| `test/sql/join/iejoin/test_iejoin_events.test` | nondeterministic wrong COUNT (`6` vs `2`) — same IEJoin code as `iejoin_projection_maps`; passed in the baseline run, so treat as evidence for the bucket-R IEJoin bug |

---

## Appendix — every failing file, by bucket

Format: file — `[first-failure symptom] line N` (line numbers refer to the
`.test` file; symptom text is the runner's classification from the baseline
report).

### A — FS glob / multi-file path resolution (43 files, driver, in-flight)

| file | first failure |
|------|---------------|
| `test/sql/copy/csv/18579.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 8 |
| `test/sql/copy/csv/21248.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 5 |
| `test/sql/copy/csv/afl/fuzz_20250226.test` | [wrong result] line 10 |
| `test/sql/copy/csv/afl/test_fuzz_3977.test` | [wrong result] line 7 |
| `test/sql/copy/csv/auto_glob_directory.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 8 |
| `test/sql/copy/csv/csv_dtypes_union_by_name.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 35 |
| `test/sql/copy/csv/csv_hive.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 13 |
| `test/sql/copy/csv/csv_hive_filename_union.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 9 |
| `test/sql/copy/csv/glob/copy_csv_glob.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 12 |
| `test/sql/copy/csv/glob/read_csv_glob.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 8 |
| `test/sql/copy/csv/glob/test_unmatch_globs.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 8 |
| `test/sql/copy/csv/parallel/parallel_csv_hive_partitioning.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 13 |
| `test/sql/copy/csv/parallel/parallel_csv_union_by_name.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 15 |
| `test/sql/copy/csv/parallel/test_multiple_files.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 12 |
| `test/sql/copy/csv/read_csv_variable.test` | [wrong result] line 14 |
| `test/sql/copy/csv/recursive_csv_union_by_name.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 8 |
| `test/sql/copy/csv/rejects/csv_incorrect_columns_amount_rejects.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 107 |
| `test/sql/copy/csv/rejects/csv_rejects_auto.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 10 |
| `test/sql/copy/csv/rejects/csv_rejects_maximum_line.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 71 |
| `test/sql/copy/csv/rejects/csv_rejects_read.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 52 |
| `test/sql/copy/csv/rejects/csv_rejects_two_tables.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 10 |
| `test/sql/copy/csv/rejects/test_invalid_parameters.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 103 |
| `test/sql/copy/csv/test_9005.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 11 |
| `test/sql/copy/csv/test_csv_projection_pushdown_glob.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 17 |
| `test/sql/copy/csv/test_filename_filter.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 8 |
| `test/sql/copy/csv/test_glob_reorder_null.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 30 |
| `test/sql/copy/csv/test_glob_type.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 11 |
| `test/sql/copy/csv/test_insert_into_types.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 17 |
| `test/sql/copy/csv/test_null_padding_union.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 11 |
| `test/sql/copy/csv/test_sniff_csv_options.test` | [statement error: message mismatch] line 118 |
| `test/sql/copy/csv/test_union_by_name.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 102 |
| `test/sql/copy/csv/test_union_by_name_types.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 8 |
| `test/sql/copy/csv/unicode_filename.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 16 |
| `test/sql/json/issues/issue13725.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 10 |
| `test/sql/json/issues/issue15601.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 11 |
| `test/sql/json/issues/issue18301.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 13 |
| `test/sql/json/table/auto_glob_directory.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 10 |
| `test/sql/json/table/json_multi_file_reader.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 16 |
| `test/sql/json/table/multi_file_hang.test` | [statement error: message mismatch] line 12 |
| `test/sql/json/table/read_json_objects.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 135 |
| `test/sql/storage/read_duckdb/read_duckdb_basic.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 40 |
| `test/sql/storage/read_duckdb/read_duckdb_top_n.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 18 |
| `test/sql/storage/read_duckdb/read_duckdb_virtual_columns.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 18 |

### B — Nested-value rendering fidelity (46 files, driver)

| file | first failure |
|------|---------------|
| `test/sql/aggregate/aggregates/test_approx_quantile.test` | [wrong result] line 119 |
| `test/sql/aggregate/aggregates/test_binned_histogram.test` | [wrong result] line 85 |
| `test/sql/aggregate/aggregates/test_histogram.test` | [wrong result] line 82 |
| `test/sql/aggregate/aggregates/test_quantile_cont_list.test` | [wrong result] line 19 |
| `test/sql/aggregate/aggregates/test_quantile_disc_list.test` | [wrong result] line 94 |
| `test/sql/cast/string_to_list_cast.test` | [wrong result] line 208 |
| `test/sql/cast/string_to_list_escapes.test` | [wrong result] line 75 |
| `test/sql/cast/string_to_list_roundtrip.test` | [wrong result] line 30 |
| `test/sql/cast/string_to_map_escapes.test` | [wrong result] line 29 |
| `test/sql/cast/string_to_struct_cast.test` | [wrong result] line 55 |
| `test/sql/cast/string_to_struct_escapes.test` | [wrong result] line 24 |
| `test/sql/cast/string_to_struct_roundtrip.test` | [wrong result] line 47 |
| `test/sql/cast/string_to_unnamed_struct.test` | [wrong result] line 5 |
| `test/sql/cast/struct_to_map.test` | [wrong result] line 307 |
| `test/sql/copy/csv/test_headers_12089.test` | [wrong result] line 8 |
| `test/sql/function/blob/create_sort_key.test` | [wrong result] line 145 |
| `test/sql/function/date/test_date_part.test` | [wrong result] line 498 |
| `test/sql/function/interval/test_date_part.test` | [wrong result] line 165 |
| `test/sql/function/list/aggregates/histogram.test` | [wrong result] line 54 |
| `test/sql/function/list/aggregates/minmax_nested.test` | [wrong result] line 57 |
| `test/sql/function/list/generate_series_timestamp.test` | [wrong result] line 6 |
| `test/sql/function/list/icu_generate_series_timestamptz.test` | [wrong result] line 14 |
| `test/sql/function/list/lambdas/arrow/expression_iterator_cases_deprecated.test` | [wrong result] line 92 |
| `test/sql/function/list/lambdas/arrow/rhs_parameters_deprecated.test` | [wrong result] line 74 |
| `test/sql/function/list/lambdas/expression_iterator_cases.test` | [wrong result] line 89 |
| `test/sql/function/list/lambdas/rhs_parameters.test` | [wrong result] line 74 |
| `test/sql/function/list/list_distinct.test` | [wrong result] line 171 |
| `test/sql/function/list/list_value_nested_lists.test` | [wrong result] line 58 |
| `test/sql/function/list/list_value_structs.test` | [wrong result] line 105 |
| `test/sql/function/list/list_where.test` | [wrong result] line 129 |
| `test/sql/function/list/list_zip.test` | [wrong result] line 27 |
| `test/sql/function/nested/test_struct_update.test` | [wrong result] line 93 |
| `test/sql/function/string/null_byte.test` | [wrong result] line 59 |
| `test/sql/function/string/regex_extract_all.test` | [wrong result] line 590 |
| `test/sql/function/timestamp/test_icu_datepart.test` | [wrong result] line 571 |
| `test/sql/json/scalar/json_nested_casts.test` | [wrong result] line 41 |
| `test/sql/json/scalar/test_json_transform.test` | [wrong result] line 162 |
| `test/sql/json/table/read_json_dates.test` | [wrong result] line 11 |
| `test/sql/prepared/test_prepare_ambiguous_type.test` | [wrong result] line 180 |
| `test/sql/subquery/scalar/test_issue_6136.test` | [wrong result] line 45 |
| `test/sql/types/nested/struct/test_struct_values.test` | [wrong result] line 5 |
| `test/sql/types/struct/struct_concat.test` | [wrong result] line 77 |
| `test/sql/types/struct/struct_contains.test` | [wrong result] line 123 |
| `test/sql/types/struct/struct_position.test` | [wrong result] line 129 |
| `test/sql/types/struct/unnamed_struct_casts.test` | [wrong result] line 13 |
| `test/sql/types/timestamp/test_infinite_time.test` | [wrong result] line 193 |

### C — Temporal decode: range wrap / precision / TZ offset (25 files, driver)

| file | first failure |
|------|---------------|
| `test/sql/function/date/test_date_trunc.test` | [wrong result] line 127 |
| `test/sql/function/operator/test_date_arithmetic.test` | [wrong result] line 31 |
| `test/sql/function/timestamp/current_time.test` | [wrong result] line 61 |
| `test/sql/function/timestamp/make_date.test` | [wrong result] line 233 |
| `test/sql/function/timestamp/test_date_part.test` | [wrong result] line 61 |
| `test/sql/function/timestamp/test_icu_dateadd.test` | [wrong result] line 101 |
| `test/sql/function/timestamp/test_icu_datetrunc.test` | [wrong result] line 66 |
| `test/sql/function/timestamp/test_icu_makedate.test` | [wrong result] line 60 |
| `test/sql/function/timestamp/test_icu_strftime.test` | [wrong result] line 50 |
| `test/sql/function/timestamp/test_icu_strptime.test` | [wrong result] line 72 |
| `test/sql/function/timestamp/test_icu_time_bucket_timestamptz.test` | [wrong result] line 1163 |
| `test/sql/function/timestamp/test_strptime.test` | [wrong result] line 271 |
| `test/sql/function/timestamp/test_time_bucket_timestamp.test` | [wrong result] line 45 |
| `test/sql/parser/test_value_functions.test` | [wrong result] line 53 |
| `test/sql/timezone/test_icu_calendar.test` | [wrong result] line 81 |
| `test/sql/timezone/test_icu_timezone.test` | [wrong result] line 213 |
| `test/sql/types/date/date_limits.test` | [wrong result] line 37 |
| `test/sql/types/date/date_try_cast.test` | [wrong result] line 108 |
| `test/sql/types/date/test_bc_dates.test` | [wrong result] line 70 |
| `test/sql/types/time/test_time_ns.test` | [wrong result] line 9 |
| `test/sql/types/time/time_try_cast.test` | [wrong result] line 60 |
| `test/sql/types/timestamp/test_timestamp_types.test` | [wrong result] line 14 |
| `test/sql/types/timestamp/timestamp_limits.test` | [wrong result] line 15 |
| `test/sql/types/timestamp/timestamp_precision.test` | [wrong result] line 52 |
| `test/sql/types/timestamp/timestamp_try_cast.test` | [wrong result] line 59 |

### D — BIGNUM decode to NULL (10 files, driver)

| file | first failure |
|------|---------------|
| `test/sql/types/bignum/test_big_bignum.test` | [wrong result] line 11 |
| `test/sql/types/bignum/test_bignum_comparisons.test` | [wrong result] line 31 |
| `test/sql/types/bignum/test_bignum_hugeint.test` | [wrong result] line 8 |
| `test/sql/types/bignum/test_bignum_implicit_cast.test` | [wrong result] line 14 |
| `test/sql/types/bignum/test_bignum_subtract.test` | [wrong result] line 8 |
| `test/sql/types/bignum/test_bignum_sum.test` | [wrong result] line 8 |
| `test/sql/types/bignum/test_double_bignum.test` | [wrong result] line 8 |
| `test/sql/types/bignum/test_int_bignum_conversion.test` | [wrong result] line 26 |
| `test/sql/types/bignum/test_varchar_bignum_conversion.test` | [wrong result] line 10 |
| `test/sql/types/bignum/test_varchar_bignum_unhappy.test` | [wrong result] line 8 |

### E — VARIANT decode to NULL (6 files, driver)

| file | first failure |
|------|---------------|
| `test/sql/function/variant/variant_extract.test` | [wrong result] line 44 |
| `test/sql/storage/types/variant/index_fetch.test` | [wrong result] line 10 |
| `test/sql/storage/types/variant/update.test` | [wrong result] line 13 |
| `test/sql/types/variant/implicit_cast_from_variant.test` | [wrong result] line 4 |
| `test/sql/types/variant/json_cast.test` | [wrong result] line 8 |
| `test/sql/types/variant/test_all_types.test` | [wrong result] line 13 |

### F — GEOMETRY decode to NULL (6 files, driver)

| file | first failure |
|------|---------------|
| `test/sql/types/geo/geometry_compatability.test` | [wrong result] line 29 |
| `test/sql/types/geo/geometry_crs.test` | [wrong result] line 43 |
| `test/sql/types/geo/geometry_persist_wal.test` | [wrong result] line 22 |
| `test/sql/types/geo/geometry_shred_fetch.test` | [wrong result] line 26 |
| `test/sql/types/geo/geometry_shred_list.test` | [wrong result] line 21 |
| `test/sql/types/geo/geometry_wkb.test` | [hash mismatch] line 77 |

### G — UUID rendered as INT128 (4 files, driver)

| file | first failure |
|------|---------------|
| `test/sql/json/issues/issue16684.test` | [wrong result] line 23 |
| `test/sql/json/test_json_cast.test` | [wrong result] line 155 |
| `test/sql/types/uuid/test_uuid_cast.test` | [wrong result] line 9 |
| `test/sql/window/test_window_constant_aggregate.test` | [wrong result] line 210 |

### H — PIVOT multi-statement prepare (12 files, driver)

| file | first failure |
|------|---------------|
| `test/sql/pivot/optional_pivots.test` | [unexpected error: Invalid Input Error: Cannot prepare multiple statements at once!] line 55 |
| `test/sql/pivot/pivot_6390.test` | [unexpected error: Invalid Input Error: Cannot prepare multiple statements at once!] line 25 |
| `test/sql/pivot/pivot_empty.test` | [unexpected error: Invalid Input Error: Cannot prepare multiple statements at once!] line 11 |
| `test/sql/pivot/pivot_expressions.test` | [unexpected error: Invalid Input Error: Cannot prepare multiple statements at once!] line 38 |
| `test/sql/pivot/pivot_in_boolean.test` | [unexpected error: Invalid Input Error: Cannot prepare multiple statements at once!] line 38 |
| `test/sql/pivot/pivot_in_subquery.test` | [unexpected error: Invalid Input Error: Cannot prepare multiple statements at once!] line 39 |
| `test/sql/pivot/pivot_operator_expression.test` | [unexpected error: Invalid Input Error: Cannot prepare multiple statements at once!] line 29 |
| `test/sql/pivot/pivot_struct_aggregate.test` | [unexpected error: Invalid Input Error: Cannot prepare multiple statements at once!] line 11 |
| `test/sql/pivot/pivot_subquery.test` | [unexpected error: Invalid Input Error: Cannot prepare multiple statements at once!] line 28 |
| `test/sql/pivot/test_pivot_duplicate_aggregates.test` | [unexpected error: Invalid Input Error: Cannot prepare multiple statements at once!] line 11 |
| `test/sql/pivot/top_level_pivot_syntax.test` | [unexpected error: Invalid Input Error: Cannot prepare multiple statements at once!] line 32 |
| `test/sql/transactions/statement-preprocessor/pivot_is_handled_correctly.test` | [unexpected error: Invalid Input Error: Cannot prepare multiple statements at once!] line 8 |

### I — Explicit-tx aborted-state semantics (3 files, engine)

| file | first failure |
|------|---------------|
| `test/sql/transactions/statement-preprocessor/multistatement_is_transactional_chained_BEGIN.test` | [statement error: expected error, got success] line 24 |
| `test/sql/transactions/statement-preprocessor/multistatement_is_transactional_chained_BEGIN_body_COMMIT.test` | [statement error: expected error, got success] line 24 |
| `test/sql/transactions/statement-preprocessor/multistatement_is_transactional_chained_PRAGMA_BEGIN.test` | [statement error: expected error, got success] line 21 |

### J — Stats-range max−min overflow (5 files, engine)

| file | first failure |
|------|---------------|
| `test/sql/aggregate/aggregates/test_null_aggregates.test` | [unexpected error: Out of Range Error: Overflow in subtraction of INT# (# - -#)!] line 314 |
| `test/sql/aggregate/group/group_by_limits.test` | [unexpected error: Out of Range Error: Overflow in subtraction of INT# (# - -#)!] line 31 |
| `test/sql/order/hugeint_order_by_extremes.test` | [unexpected error: Out of Range Error: Overflow in subtraction of INT# (# - -#)!] line 14 |
| `test/sql/window/test_lead_lag.test` | [unexpected error: Out of Range Error: Overflow in subtraction of INT# (# - -#)!] line 121 |
| `test/sql/window/test_leadlag_orderby.test` | [unexpected error: Out of Range Error: Overflow in subtraction of INT# (# - -#)!] line 70 |

### K — Error-text fidelity tail (7 files, engine)

| file | first failure |
|------|---------------|
| `test/sql/catalog/table/create_table_parameters.test` | [statement error: message mismatch] line 11 |
| `test/sql/catalog/test_extension_suggestion.test` | [statement error: message mismatch] line 9 |
| `test/sql/error/error_position.test` | [statement error: message mismatch] line 9 |
| `test/sql/order/test_limit_parameter.test` | [statement error: message mismatch] line 8 |
| `test/sql/settings/errors_as_json.test` | [statement error: message mismatch] line 11 |
| `test/sql/types/map/map_empty.test` | [statement error: message mismatch] line 5 |
| `test/sql/types/nested/map/map_from_entries/data_types.test` | [statement error: message mismatch] line 125 |

### L — Host-FS / sandbox environment gaps (7 files, driver)

| file | first failure |
|------|---------------|
| `test/sql/attach/attach_fsspec.test` | [unexpected error: IO Error: HostFileSystem: failed to open ? (errno {})] line 13 |
| `test/sql/attach/attach_home_directory.test` | [statement error: message mismatch] line 20 |
| `test/sql/copy/csv/csv_home_directory.test` | [unexpected error: IO Error: No files found that match the pattern ?] line 17 |
| `test/sql/extensions/allowed_directories_install.test` | [unexpected error: IO Error: Could not get working directory!] line 8 |
| `test/sql/secrets/create_secret_expression.test` | [unexpected error: IO Error: Failed to initialize persistent storage directory. (original] line 21 |
| `test/sql/settings/test_disabled_file_systems.test` | [statement error: expected error, got success] line 37 |
| `test/sql/settings/test_disabled_local_filesystem_metadata.test` | [statement error: expected error, got success] line 22 |

### M — Logging subsystem parity (5 files, driver+engine)

| file | first failure |
|------|---------------|
| `test/sql/logging/logging.test` | [wrong row count] line 34 |
| `test/sql/logging/logging_context_ids.test` | [unexpected error: Out of Range Error: Overflow in addition of UINT# (# + #)!] line 14 |
| `test/sql/logging/logging_csv.test` | [wrong result] line 18 |
| `test/sql/logging/logging_types.test` | [wrong row count] line 15 |
| `test/sql/logging/test_logging_function.test` | [unexpected error: Out of Range Error: Overflow in addition of UINT# (# + #)!] line 29 |

### N — Checkpoint/WAL deep storage semantics (4 files, engine)

| file | first failure |
|------|---------------|
| `test/sql/storage/checkpoint/test_checkpoint_failure_delayed_commit.test` | [INTERNAL/fatal error] line 32 |
| `test/sql/storage/checkpoint/test_checkpoint_failure_delayed_rollback.test` | [INTERNAL/fatal error] line 32 |
| `test/sql/storage/checkpoint/test_checkpoint_failure_on_detach.test` | [INTERNAL/fatal error] line 20 |
| `test/sql/storage/wal/wal_promote_version.test` | [unexpected error: Catalog Error: Table with name T does not exist!] line 32 |

### O — ICU statically built in (2 files, wontfix)

| file | first failure |
|------|---------------|
| `test/sql/timezone/disable_timestamptz_casts.test` | [unexpected error: Binder Error: Casting from TIMESTAMP to TIMESTAMP WITH TIME ZONE without a] line 22 |
| `test/sql/types/timestamp/test_timestamp_tz.test` | [statement error: expected error, got success] line 24 |

### P — Runner limitations (16 files, runner)

| file | first failure |
|------|---------------|
| `test/sql/aggregate/aggregates/test_weighted_avg.test` | [wrong row count] line 38 |
| `test/sql/aggregate/qualify/test_qualify.test` | [wrong row count] line 74 |
| `test/sql/constraints/foreignkey/test_fk_multiple.test` | [wrong row count] line 96 |
| `test/sql/constraints/foreignkey/test_fk_self_referencing.test` | [wrong row count] line 75 |
| `test/sql/constraints/foreignkey/test_foreignkey.test` | [wrong row count] line 55 |
| `test/sql/function/list/aggregates/var_stddev.test` | [wrong row count] line 31 |
| `test/sql/generated_columns/virtual/foreign_key_extensive.test` | [wrong row count] line 60 |
| `test/sql/join/positional/test_positional_join.test` | [wrong row count] line 95 |
| `test/sql/optimizer/expression/test_move_constants.test` | [unexpected error: Parser Error: syntax error at or near ?] line 13 |
| `test/sql/optimizer/expression/test_negation_limits.test` | [unexpected error: Parser Error: syntax error at or near ?] line 11 |
| `test/sql/pg_catalog/pg_constraint.test` | [wrong row count] line 20 |
| `test/sql/types/nested/array/array_joins.test` | [wrong row count] line 71 |
| `test/sql/types/nested/map/test_map_vector_types.test` | [wrong row count] line 83 |
| `test/sql/types/union/union_cast.test` | [wrong row count] line 53 |
| `test/sql/types/union/union_extract.test` | [wrong row count] line 57 |
| `test/sql/types/union/union_join.test` | [wrong row count] line 24 |

### Q — RNG sequence parity (2 files, engine)

| file | first failure |
|------|---------------|
| `test/sql/function/numeric/set_seed_for_sample.test` | [hash mismatch] line 16 |
| `test/sql/window/test_volatile_independence.test` | [wrong result] line 10 |

### R — Deep-semantics singles (15 files, mixed — see itemized table above)

| file | first failure |
|------|---------------|
| `test/sql/aggregate/aggregates/test_quantile_disc.test` | [wrong result] line 97 |
| `test/sql/catalog/view/test_loosely_qualified_view_sql.test` | [unexpected error: Catalog Error: Table with name v# does not exist!] line 43 |
| `test/sql/copy/csv/test_timestamptz_12926.test` | [statement error: expected error, got success] line 8 |
| `test/sql/function/generic/test_sleep.test` | [unexpected error: Invalid Input Error: ThreadUtil::SleepMs requires DuckDB to be compiled with thre] line 6 |
| `test/sql/function/operator/test_in_empty_table.test` | [unexpected error: Conversion Error: Could not convert string ? to INT#] line 8 |
| `test/sql/join/iejoin/iejoin_projection_maps.test` | [wrong result] line 23 |
| `test/sql/json/issues/read_json_memory_usage.test` | [statement error: expected error, got success] line 25 |
| `test/sql/json/test_json_serialize_plan.test` | [wrong result] line 10 |
| `test/sql/limit/test_batch_limit_filters.test` | [wrong result] line 14 |
| `test/sql/optimizer/predicate_factoring.test` | [wrong result] line 92 |
| `test/sql/optimizer/test_in_rewrite_rule.test` | [unexpected error: Conversion Error: Could not convert string ? to INT#] line 15 |
| `test/sql/pragma/profiling/test_custom_profiling_optimizer_settings.test` | [wrong result] line 78 |
| `test/sql/sample/test_sample_too_big.test` | [unexpected error: Out of Memory Error: Allocation failure] line 28 |
| `test/sql/types/numeric/uhugeint_try_cast.test` | [wrong result] line 111 |
| `test/sql/types/type/test_make_get_type.test` | [wrong result] line 4 |

### S — String compression codec selection (6 files, engine)

| file | first failure |
|------|---------------|
| `test/sql/storage/compression/string/big_strings.test` | [wrong result] line 44 |
| `test/sql/storage/compression/string/blob.test` | [wrong result] line 57 |
| `test/sql/storage/compression/string/empty.test` | [wrong result] line 51 |
| `test/sql/storage/compression/string/filter_pushdown.test` | [wrong result] line 44 |
| `test/sql/storage/compression/string/index_fetch.test` | [wrong result] line 49 |
| `test/sql/storage/compression/string/simple.test` | [wrong result] line 51 |
