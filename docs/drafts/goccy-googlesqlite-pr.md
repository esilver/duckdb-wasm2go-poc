# PR description draft(s) for upstream goccy/googlesqlite

> DRAFT ONLY — not filed. Covers three change families currently living on
> https://github.com/esilver/googlesqlite (branch `pure-go-duckdb-backend`).
> Recommendation: split into **three PRs** as below — they are independently
> reviewable, independently revertable, and only PR 1 is conceptually coupled
> to the DuckDB backend work.
>
> Upstream-applicability note: upstream `main` is SQLite-backend only; the
> DuckDB backend (cgo + pure-Go) is fork-side. PRs 1 and 3 patch files on the
> DuckDB lowering path and assume the backend lands (or can be carried with
> it); PR 2 contains one fix (`dateValueFromLiteral`) that touches
> `internal/encoder.go` on upstream `main` and is backend-independent — it can
> be cherry-picked today even if nothing else is taken.

---

## PR 1 — Route SEARCH and OBJ.* builtins on the DuckDB path (fixes the 14 known conformance failures)

Fork commit: `4e9fb1c` (esilver/googlesqlite)

### What

The 14 `bigquery/functions/{search,objectref}` conformance failures —
historically written off as out-of-scope external services — are actually a
**wiring gap**, not missing functionality. The pure-Go bodies
(`textfn.BindSearch`, `extras.BindObjMakeRef` / `FetchMetadata` /
`GetAccessUrl` / `GetReadUrl`) have always existed and already run on the
SQLite path; the DuckDB path simply never registered them as UDFs nor routed
their bare resolved names.

Two-file patch:

- `internal/duckdb_string_udf.go` — add `search` and the four `obj_*` names to
  the DuckDB UDF allow-list. `SEARCH` declares a native `BOOL` result; the
  `OBJ.*` family stays on the envelope `VARCHAR` carrier and re-types at
  decode.
- `internal/formatter.go` — new `trySearchObjectRefFunc` hook: retargets the
  bare resolved names to the registered `googlesqlite_*` UDFs (same shape as
  the existing array value-layer hook).

(The commit also updates REPRODUCE-PURE-GO.md; docs-only.)

### Evidence

- Conformance (pure-Go DuckDB backend, engine `duckdb-wasm2go-poc@c1ce29c`):
  **986 PASS / 0 FAIL / 8 SKIP over 994** — zero failures, exceeding the cgo
  baseline (972/994). All 14 previously-failing search/objectref cases pass.
- The patch is backend-symmetric: the same two files would lift the **cgo**
  DuckDB build by the same 14 cases (the gap is in lowering/registration, not
  in either engine binding).

---

## PR 2 — Fix the storage-seam bug family (temporal columns, day-early dates, SAFE_DIVIDE coercion, prepare-failure tx poisoning)

Fork commit: `bcf0852` (esilver/googlesqlite) — 7 files, +278/−5.

### What

Four bugs found by real CLI usage, none covered by the literal-centric
conformance suite (they only bite once values cross the **storage seam** —
written to a table, then read back through functions):

1. **Temporal columns stored as TEXT while expression lowering emits native
   date functions.** `DATE`/`TIME`/`DATETIME`/`TIMESTAMP` columns now lower to
   native `DATE`/`TIME`/`TIMESTAMP`/`TIMESTAMPTZ` storage
   (`ColumnSpec.SQLiteSchema`, `internal/spec.go`). This is what made
   `FORMAT_DATE`/`EXTRACT`/`CAST` on a *stored* column fail while the same
   expression on a literal passed. Caveat for the changelog: `.db` files
   created before the fix keep their VARCHAR columns — recreate rather than
   reuse; no auto-migration is attempted.
2. **Dates stored one day early.** `dateValueFromLiteral`
   (`internal/encoder.go`) built the date in the local zone via `time.Unix`;
   now UTC. *Backend-independent; exists on upstream `main`; can be split out
   as its own one-line PR if preferred.*
3. **`SAFE_DIVIDE(SUM(x), COUNT(*))` Binder error.** Implicit numeric→DOUBLE
   coercions from raw-scalar numeric kinds now emit a native `CAST` instead of
   the VARCHAR-typed `googlesqlite_cast` UDF (envelope kinds stay on the UDF).
4. **Prepare-failure transaction poisoning.** A DuckDB prepare-stage failure
   leaves the engine connection's implicit transaction open (engine-side
   issue; this is a driver-side workaround): error-path defers in
   `Prepare`/`Exec`/`QueryContext` issue a best-effort `ROLLBACK`, skipped
   while an explicit user transaction is open.

### Evidence

- New regression tests (in `regression_test.go`):
  `TestRegression_StoredTemporalColumns`,
  `TestRegression_SafeDivideAggregateCoercion`,
  `TestRegression_PrepareFailureDoesNotPoisonConnection`.
- Conformance tally unchanged (986 / 0 / 8) **and 18 pre-existing
  non-spec test failures now pass**.
- Also included: `GSX_DUMP_SQL` debug dumping now covers DML statements
  (diagnostic aid that found bug 1).

---

## PR 3 — Expand composite strftime specifiers DuckDB lacks (%F %T %D %R %r %h)

Fork commit: `d101551` (esilver/googlesqlite) — `internal/formatter.go`, +51/−3.

### What

`FORMAT_TIMESTAMP("%F %H:%M", ...)` failed: DuckDB's strftime has no `%F`
(BigQuery shorthand for `%Y-%m-%d`). `translateBQDateFormat` is now a
`%%`-aware scanner that expands the composite shorthands to their definitions:

| spec | expansion |
|---|---|
| `%F` | `%Y-%m-%d` |
| `%T` | `%H:%M:%S` |
| `%D` | `%m/%d/%y` |
| `%R` | `%H:%M` |
| `%r` | `%I:%M:%S %p` |
| `%h` | `%b` |

`%Ez -> %z` mapping is preserved. Unknown specifiers deliberately pass through
so DuckDB raises a clear error rather than silently formatting wrong output.

### Evidence

- Verified: `%F`/`%T`/`%D` expansion, literal `%%` safety (`"100%% %R"`), and
  the googlesql temporal conformance subsets stay green.

---

## Shared reproduction

- Engine chain (all public, zero `replace` directives):
  `bigquery-emulator@5116fb4` → `googlesqlite@v0.2.4-pure-go` →
  `duckdb-go-pure@v0.1.0`; fresh-clone acceptance verified.
- Conformance runner and tallies: REPRODUCE-PURE-GO.md on the fork branch.
- `--debug` / `GSX_DUMP_SQL` print the lowered DuckDB SQL for every repro
  above.
