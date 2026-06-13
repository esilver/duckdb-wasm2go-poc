# SWAP BLUEPRINT — `duckdbcompat`: running the GoogleSQL/BigQuery emulator on our pure-Go (CGO_ENABLED=0) DuckDB engine

**Target (READ-ONLY):** `/tmp/gsx-duckdb` — `esilver/googlesqlite` branch `duckdb-backend`. Imports `github.com/duckdb/duckdb-go/v2 v2.10503.1` (go.mod:56) over libduckdb v1.5.3.
**Engine (READ-ONLY):** `converge/duckdb` (this repository) — pure-Go DuckDB v1.5.3 transpiled by wasm2go; working `database/sql/driver` (driver.go/result.go/module.go) + proven scalar & aggregate UDF callback mechanism (udf_test.go, udf_agg_test.go both PASS).

> **Scope note.** This document is a blueprint synthesized from four dependency-surface catalogs. All `file:line` citations into `/tmp/gsx-duckdb` and `converge/duckdb` were spot-verified against the live trees while writing. No repo is modified by this document.
>
> **Outcome note (2026-06-10).** This blueprint was executed and the plan
> worked: the swap landed as `converge/duckdbcompat` + the
> `pure-go-duckdb-backend` branches, and the actual conformance run delivered
> **986 PASS / 0 FAIL / 8 SKIP of 994 specs** — exceeding the cgo baseline
> (972/14/8); see the Results section of [README.md](README.md). The tier
> tables below were pre-run estimates. Paths like `/tmp/gsx-duckdb` are
> machine-local snapshot references from the writing session, not links a
> repo visitor can follow.
>
> **Current status note (2026-06-13).** Several risk statements below are
> historical and have since been retired in the PoC and published driver:
> connector-scoped in-memory sharing is implemented through `module.connect`,
> aggregate finalize errors use DuckDB's aggregate error channel instead of
> panicking, aggregate special-null handling is wired, and the function-set
> overload path is present. The old cgo aggregate bridge is now archived behind
> `//go:build ignore`; normal and accidental `-tags duckdb_cgo` builds use the
> supported pure-Go bridge. Remaining design risks are now narrower: memory
> behavior under large grouped aggregates, full BigQuery aggregate-clause
> lowering, cancellation semantics at the emulator job layer, and continued
> parity sweeps against DuckDB's sqllogictest corpus.

---

## 0. The decisive structural fact that makes a clean swap possible

Every one of the **11** Go files that touch duckdb-go imports it under the **same alias**:

```go
duckdb "github.com/duckdb/duckdb-go/v2"
```

Verified import sites (all `/tmp/gsx-duckdb`): `driver.go:14`, `internal/decoder.go:6`, `duckdb_udf.go`, `duckdb_extra_scalar_udf.go`, `duckdb_struct_udf.go`, `duckdb_proto_udf.go`, `duckdb_json_udf.go`, `duckdb_geo_udf.go`, `duckdb_time_series_udf.go`, `duckdb_aggregate_udf.go`, `duckdb_aggregate_kll_extract.go`.

Because the package is always referenced as `duckdb.<Symbol>`, the **entire retarget reduces to: provide a package whose exported surface is API-identical to the subset of duckdb-go/v2 the emulator uses, and repoint the import path to it.** No call site changes. This is the `duckdbcompat` package.

The total external API surface consumed is astonishingly small — it is fully enumerated in §1. There is exactly **one** direct duckdb-go data-path API call (`driver.go:177`), **one** concrete value type that crosses on the data path (`duckdb.Decimal`), **~26 type/function symbols** for the scalar/aggregate/table UDF registration façade, and **one** cgo translation unit (`duckdb_aggregate_udf.go`) that reaches *past* duckdb-go straight to the C aggregate API — and that file's reflect+unsafe machinery **disappears entirely** because we own the driver conn.

---

## 1. The `duckdbcompat` package design — complete exported surface

A single Go package (proposed path: `github.com/goccy/googlesqlite/duckdbcompat`, or vendored as a replacement module) that the 11 files import **in place of** `github.com/duckdb/duckdb-go/v2`, keeping the `duckdb` alias. Backed by our engine's `(mod *module).RegisterScalarUDF` / `.RegisterAggregateUDF` and the `*Driver` in `converge/duckdb/driver.go`.

### 1.1 Driver / data path (Surface 1)

| Compat symbol | Exact form required | Backing in our engine | Used by emulator at |
|---|---|---|---|
| `duckdb.Driver` (struct) | `type Driver struct{}` with method `Open(dsn string) (driver.Conn, error)` | Delegate to `convergeduckdb.Driver{}` (`converge/duckdb/driver.go:36`). | `driver.go:177` (`duckdb.Driver{}.Open(duckdbDSN(name))`) — the **only** direct data-path call. |
| `duckdb.Decimal` (type) | `type Decimal struct{ Width uint8; Scale uint8; Value *big.Int }` + methods `String() string`, `Float64() float64` | Our driver's row decoder (`converge/duckdb/result.go`) must **scan DECIMAL columns/literals into this type** (currently unverified — see §3 T0/§4). | `internal/decoder.go:26` (`.String()`), `duckdb_geo_udf.go:163` (`.Float64()`), `duckdb_aggregate_kll_extract.go:112` (`.Float64()`). |

Everything else on the data path is generic `database/sql` plumbing the emulator drives through stdlib interfaces (`*sql.DB/Conn/Tx/Stmt/Rows`, `sql.Result`); our driver already satisfies these (`driver.ConnPrepareContext`, `ExecerContext`, `QueryerContext`, `Pinger` are all asserted at `converge/duckdb/driver.go:227-232`). No compat symbol needed for those.

### 1.2 Scalar UDF façade (Surface 2) — 8 symbols

| Compat symbol | Exact form required (field/method names load-bearing) | Backing |
|---|---|---|
| `RegisterScalarUDF(con *sql.Conn, name string, f ScalarFunc) error` | first param **`*sql.Conn`** exact | `con.Raw(...)` → our `*conn` → `mod.RegisterScalarUDF(c.con, name, params, ret, fn)` (`converge/duckdb/udf_scalar.go:27`) **with the compat layer adding varargs/special-null/volatile** (see §2.3 gaps). |
| `ScalarFunc interface { Config() ScalarFuncConfig; Executor() ScalarFuncExecutor }` | exact 2-method set (17 emulator structs satisfy it implicitly) | n/a (interface) |
| `ScalarFuncConfig struct { InputTypeInfos []TypeInfo; ResultTypeInfo TypeInfo; VariadicTypeInfo TypeInfo; SpecialNullHandling bool; Volatile bool }` | all five field names+types exact (set by-name at 18 sites) | translated into `paramTypeIDs`/`retTypeID` + varargs/null/volatile flags |
| `ScalarFuncExecutor struct { RowExecutor func(values []driver.Value) (any, error) }` | field name `RowExecutor`, element type `driver.Value` exact | wrapped into our `func(args []any)(any,error)` (`[]driver.Value` ≡ `[]any`) |
| `TypeInfo` (opaque carrier) | interface w/ unexported `id() Type`, or `struct{ id Type }` | carries one `Type` → engine `DUCKDB_TYPE_*` |
| `NewTypeInfo(t Type) (TypeInfo, error)` | exact | trivial constructor, returns `TypeInfo{t}, nil` |
| `Type` (enum) + constants `TYPE_ANY, TYPE_VARCHAR, TYPE_BOOLEAN, TYPE_BIGINT, TYPE_DOUBLE, TYPE_BLOB, TYPE_TIMESTAMP` | exactly these 7 (complete set used across scalar+agg+table) | mapped to engine `DUCKDB_TYPE_*` numeric values. `TYPE_ANY` is **load-bearing** (native+envelope polymorphism). |
| `Decimal` | shared with §1.1 | shared |

### 1.3 Aggregate UDF (Surface 3)

The emulator's aggregate path lives in the **single cgo TU** `duckdb_aggregate_udf.go`, which does **not** use any duckdb-go aggregate symbol (duckdb-go/v2 exposes no aggregate registration). It calls the C aggregate API directly and claws the raw `duckdb_connection` out of duckdb-go's unexported `*duckdb.Conn.conn mapping.Connection` field via reflect+unsafe (`duckdb_aggregate_udf.go:563-577`).

**In `duckdbcompat` this file is rewritten, not aliased.** It needs zero new compat *symbols*; instead it becomes a pure-Go file that:
- deletes `rawDuckDBConn`/`connPtrFromDuckDBConn`/`liveAggHandles`/`cgo`/`unsafe`/`reflect`,
- unwraps `*sql.Conn` via `conn.Raw` to **our** `*conn` (exposes `mod *module` + `con int32` directly — `converge/duckdb/driver.go:78-85`),
- copies `duckdbAggSpecs()` (33 specs, `duckdb_aggregate_udf.go:371-476`) verbatim, swapping `C.idx_t`/`C.duckdb_type` field types for `int64`/`int32`,
- registers via `mod.RegisterAggregateUDF(con, name, paramTypeIDs, retTypeID, impl)` (`converge/duckdb/udf_aggregate.go:120`), mapping the collect/concat/replay state model onto our `AggregateImpl{NewState/Update/Combine/Finalize}` (`udf_aggregate.go:42-47`).

So the only *compat-package* symbols the aggregate path needs are the **shared** scalar ones (`Type`, `TypeInfo`, `NewTypeInfo`, `Decimal`) plus, for the sibling `duckdb_aggregate_kll_extract.go`, the scalar façade (`RegisterScalarUDF`, `ScalarFuncConfig`, `ScalarFuncExecutor`).

### 1.4 Table UDF (Surface 4) — 6 symbols (compile-only or thin shim)

| Compat symbol | Exact form | Backing |
|---|---|---|
| `RegisterTableUDF(con *sql.Conn, name string, f RowTableFunction) error` | exact | **Recommended shim:** run `CREATE TABLE <name>(...)` / empty view on the conn; never touch `duckdb_create_table_function`. The only two TVFs (`appends`, `changes`) are degenerate zero-row stubs (`duckdb_time_series_udf.go:60-66`: `Cardinality={0,true}`, `FillRow→false,nil`). |
| `RowTableFunction struct { BindArguments func(map[string]any, ...any) (RowTableSource, error) }` | field `BindArguments` exact | compat invokes `BindArguments` to read `ColumnInfos()` → derive the `CREATE TABLE` schema |
| `RowTableSource interface { ColumnInfos() []ColumnInfo; Cardinality() *CardinalityInfo; Init(); FillRow(Row) (bool, error) }` | exact 4-method set | only `ColumnInfos()` consulted by the shim |
| `ColumnInfo struct { Name string; T TypeInfo }` | exact | schema → column DDL |
| `CardinalityInfo struct { Cardinality uint; Exact bool }` | exact | unused by shim (always 0) |
| `Row` (type) | any type satisfying the `FillRow` param | never written |

### 1.5 Spatial extension (Surface 4c)

No compat *symbol* — `loadSpatialExtension`/`doLoadSpatialExtension` live **inside the emulator** (`duckdb_geo_udf.go:55-83`). In the `duckdbcompat` build they become **no-ops returning `nil`**: all geography is served by pure-Go `googlesqlite_st_*`/`s2_*` scalar UDFs (the code itself documents the native `ST_*` load as a dead parity probe, `duckdb_geo_udf.go:24-42,85-92`). No `INSTALL`/`LOAD`, no static-linking of the C++ spatial extension.

### 1.6 Complete symbol count

| Category | Count | Notes |
|---|---|---|
| Data-path direct API | 1 (`Driver.Open`) + 1 type (`Decimal`) | `driver.go:177`; Decimal at 3 sites |
| Scalar façade | 8 symbols | §1.2 |
| Aggregate | 0 new compat symbols | cgo TU rewritten in pure Go; reuses scalar symbols |
| Table | 6 symbols | §1.4, mostly compile-only |
| **Distinct compat exports total** | **~17 types/functions + 7 enum constants** | (`Decimal`, `Type`+7 consts, `TypeInfo`, `NewTypeInfo`, `ScalarFunc`, `ScalarFuncConfig`, `ScalarFuncExecutor`, `RegisterScalarUDF`, `Driver`, `RegisterTableUDF`, `RowTableFunction`, `RowTableSource`, `ColumnInfo`, `CardinalityInfo`, `Row`) |

---

## 2. Minimal changes to the emulator itself — how invasive?

**Goal: make the emulator compile + run CGO_ENABLED=0 by changing as close to zero emulator lines as possible.** Two strategies; (B) is recommended.

### 2.1 Strategy A — vendor a replacement module (zero emulator source edits)

Add a `replace` directive in the emulator's `go.mod`:
```
replace github.com/duckdb/duckdb-go/v2 => ./duckdbcompat   // or a published module
```
Then `duckdbcompat` *is* package `duckdb-go/v2` for the build. **Zero edits to any of the 11 emulator files.** Drawback: `duckdb_aggregate_udf.go` is a cgo file that must NOT compile in the pure-Go build — but `replace` doesn't help there because that file lives in the **emulator**, not in duckdb-go. So Strategy A alone is insufficient for the aggregate TU; it must be paired with build tags (below).

### 2.2 Strategy B — build-tagged duplication of the 3 engine-coupled files + import alias (recommended)

Three categories of emulator change:

1. **Repoint the import** (the bulk: 8 of 11 files — all pure scalar/table/decoder files). Either via the `go.mod replace` (Strategy A, zero source edits) **or** a mechanical alias change `duckdb "github.com/duckdb/duckdb-go/v2"` → `duckdb ".../duckdbcompat"`. With `replace`, **these 8 files are untouched.**

2. **The data-path one-liner** — `driver.go:177`:
   ```go
   func (internalDuckDBDriver) Open(name string) (driver.Conn, error) {
       return duckdb.Driver{}.Open(duckdbDSN(name))
   }
   ```
   With `replace`, `duckdb.Driver{}` resolves to the compat `Driver`, so **even this is untouched**. (Confirm `duckdbDSN` conformance — §3 T0.)

3. **The aggregate bridge** — this was the one emulator file that could not be
   handled by import replacement alone. The executed implementation keeps the
   historical cgo file as an archive-only reference:
   - `duckdb_aggregate_udf.go` → `//go:build ignore`, not selectable by normal
     builds or accidental `-tags duckdb_cgo` builds.
   - `duckdb_aggregate_udf_purego.go` → the supported bridge, calling the pure
     engine's aggregate registration surface. Both expose the same package-level
     `registerDuckDBAggregates(db *sql.DB) error` shape, so `driver.go:208`
     remains unchanged.

**Invasiveness verdict:** with `go.mod replace`, the emulator changes reduced to
**adding ONE new file** (`duckdb_aggregate_udf_purego.go`) and archiving the
existing cgo TU behind `//go:build ignore`. Optionally, also flip
`loadSpatialExtension` to a no-op (1 file, ~3 lines, or done inside the
compat-aware build via tag). **Net: ~1 new file + 1 archive tag + 1 optional
no-op edit. The other 10 duckdb-go-touching files compile verbatim.**

> The same `con.Raw`-based registration path the compat `RegisterScalarUDF`/`RegisterTableUDF` use is also what `duckdb_aggregate_udf_purego.go` uses — they all unwrap `*sql.Conn` → our `*conn` (which exposes `mod`+`con` at `converge/duckdb/driver.go:78-85`), so there is **one** unwrap helper shared across the whole UDF surface.

### 2.3 Engine-side work the compat layer triggers (NOT emulator edits, but prerequisite)

The compat layer cannot be purely declarative; it drives these engine additions (all in `converge/duckdb`, our repo, not the emulator):

| Need | Current state (verified) | Action |
|---|---|---|
| Varargs registration | `udf_scalar.go:72-77` wires name/return/function only — **no varargs** | Compat must call `m.Xduckdb_scalar_function_set_varargs(...)` when `VariadicTypeInfo != nil`. **451/456 scalar registrations are variadic-ANY.** Verify the symbol exists in the transpiled C-API. |
| `SpecialNullHandling` (scalar) | not wired in `udf_scalar.go` | call `m.Xduckdb_scalar_function_set_special_handling`; set `true` on every emulator UDF. |
| `Volatile` (scalar) | not wired | call `m.Xduckdb_scalar_function_set_volatile`; only `normalFuncUDF` sets it (`duckdb_udf.go:649`) — UUID/crypto correctness. |
| `set_special_handling` (aggregate) | **not called** in `udf_aggregate.go` (confirmed: only name/return/functions/destructor wired at `udf_aggregate.go:205-218`) | add `m.Xduckdb_aggregate_function_set_special_handling(af)` — present in C-API, just uncalled. |
| `add_aggregate_function_to_set` | **ABSENT from transpiled core** (the single C-API gap, Surface 3) | use the **loop-register-per-arity shim** (§3 T2): register each name once per arity in `[minParams,maxParams]` with N copies of `DUCKDB_TYPE_ANY`; DuckDB resolves same-name/diff-arity as overloads at the catalog level (the function-set is an optimization, not a requirement). |
| `readCell` DECIMAL + JSON-alias | `udf_codec.go` covers BOOL/ints/FLOAT/DOUBLE/VARCHAR/BLOB/DATE/TIMESTAMP; **no DECIMAL case, no JSON-alias sniff** | add `dtDecimal` branch (engine already exports `Xduckdb_decimal_width/_scale/_internal_type/_to_double`) + `Xduckdb_logical_type_get_alias` JSON sniff; port `normalizeJSONScalar` verbatim (pure Go). |
| Aggregate result re-encode | cgo `writeResult` (`duckdb_aggregate_udf.go:809-886`) does value-layer round-trip | move the `value.ValueFromGoValue`/`EncodeValueString` (VARCHAR envelope) and `DecodeValue.To{Float64,Int64}` (DP DOUBLE/BIGINT) logic into the Go `impl.Finalize`. |
| Aggregate error path | `udf_aggregate.go:178-182` **panics** on Finalize encode error | switch to `Xduckdb_aggregate_function_set_error` to match cgo `agg_set_error` (`duckdb_aggregate_udf.go:277`). |
| `Decimal` on Scan | unverified | row decoder must produce compat `Decimal` for DECIMAL (else NUMERIC precision + geo-arg decode silently degrade). |

---

## 3. Tiered build order with effort estimates and conformance recovery

> Effort = engineer-days, rough order-of-magnitude. Conformance % is *cumulative share of the emulator's test surface expected to pass* once the tier lands, given the emulator is a façade and most BigQuery semantics live in pure-Go bodies that transfer verbatim.

### **T0 — Boot the data path (CGO off, trivial SQL)** — ~3-5 days → **~15-25% conformance**

What ships: emulator compiles & runs CGO_ENABLED=0 on our engine for all queries that bind **no `googlesqlite_*` UDF** — the "tracer bullet" set (`SELECT 1`, `CREATE/INSERT/COUNT`, scalar columns, native DuckDB SQL), exactly the Milestone-0 spine the emulator already carves out (`driver.go:167-178` comment).

Tasks:
1. Stand up the `duckdbcompat` package skeleton: `Driver` (delegates to `convergeduckdb.Driver`), `Decimal` (`String()`/`Float64()`), `Type`+7 constants, `TypeInfo`/`NewTypeInfo`, and **stub** `RegisterScalarUDF`/`RegisterAggregateUDF`/`RegisterTableUDF` that return `nil` without registering (so registration call sites compile and no-op). Add `go.mod replace`.
2. Archive the cgo aggregate TU behind `//go:build ignore`; add the pure bridge
   file. The executed branch went past the original no-op stub and now registers
   real aggregate callbacks through the pure engine.
3. DSN conformance: confirm `duckdbDSN` (`driver.go:150-165`) output (`""`→in-memory, bare path→file) matches our connector (`converge/duckdb/driver.go:55-62`: `""`→`:memory:`, else path). **Already compatible.**
4. **Connection-sharing decision (the load-bearing T0 risk — see §4).** Resolve the shared-in-memory-DB requirement.
5. Scan-type conformance audit: guarantee our typed Scan returns `time.Time` / `*big.Int` (HUGEINT — hot for COUNT/SUM) / `[]any` (LIST) / `map[string]any` (STRUCT) / narrow ints / `[]byte` / **`Decimal`**, mirroring `internal/value/decoder.go`'s switch. Table-driven test.
6. Tx options: confirm `ConnBeginTx` accepts non-zero isolation + ReadOnly without erroring (`driver.go:540,555`).

### **T1 — Scalar UDFs (the bulk unlock)** — ~6-10 days → **~70-85% conformance**

What ships: all **456 scalar registrations per inner DB** (7 fixed + 11 envelope-array + 4 native-array + 16 net + 4 struct + 9 proto + 1 extra + 8 range + 202 normal/string family + 118 JSON + 72 geography + 4 KLL-extract). This is where the GoogleSQL builtin coverage lives — string/JSON/geo/proto/struct/net/numeric scalar functions are nearly the entire conformance surface.

Tasks:
1. Implement the real `RegisterScalarUDF` compat: `f.Config()`/`f.Executor()` → `paramTypeIDs`/`retTypeID` + variadic/null/volatile flags → `mod.RegisterScalarUDF` (extended) or direct C-API. `con.Raw` unwrap to our `*conn`.
2. Engine: wire **varargs** (`set_varargs`), **special-null-handling**, **volatile** into the scalar registration (§2.3). Confirm `DUCKDB_TYPE_ANY` is creatable via `Xduckdb_create_logical_type` and arrives un-coerced in `RowExecutor`.
3. Engine: ensure scalar registration is **catalog-scoped** (visible across the DB's connections) — the emulator registers once per inner DB and assumes all conns see it (`duckdb_udf.go:24-27`).
4. `Decimal` value-codec: ensure DECIMAL-typed scalar **arguments** arrive as compat `Decimal` (else `ST_GEOGPOINT(1.5,2.5)` / `EXTRACT_POINT(s,0.5)` decode silently fails at `duckdb_geo_udf.go:163`, `duckdb_aggregate_kll_extract.go:112`).
5. `loadSpatialExtension` → no-op; keep `registerGeographyUDFs`.

The rich-type value layer (`internal/value/*`) has **zero duckdb-go imports** and rides a base64-JSON VARCHAR envelope — it **passes through our codec unchanged**. This is why T1 recovers so much: the bodies are already pure Go.

### **T2 — Aggregate/window UDFs** — ~4-6 days → **~88-94% conformance**

What ships: all **33 aggregate specs** (`duckdbAggSpecs()`, `duckdb_aggregate_udf.go:371-476`): KLL_QUANTILES (7), HLL_COUNT (2), HAVING modifier (1), property-graph MEASURE (1), geography (4, incl. `st_clusterdbscan` singleState), `tf_idf` (1, singleState), ANON_ DP (8), `googlesqlite_differential_privacy_*` (8). Unlocks GROUP BY / OVER / differential-privacy / approximate-aggregate conformance.

Tasks (engine + the pure-Go rewrite of `duckdb_aggregate_udf_purego.go`):
1. Confirm `dtAny` creatable; add `dtDecimal` + JSON-alias to `readCell` (§2.3).
2. Add `set_special_handling` to `RegisterAggregateUDF`; switch finalize panic → `set_error`.
3. Port `normalizeJSONScalar` + value-layer result re-encode into the Go `impl`/`aggState.finalize`.
4. **Overload-band shim** (the one real C-API gap): loop-register per arity (option 1, no codegen). Fall back to concrete-typed band if `ANY` doesn't resolve.
5. Port the 33-spec table verbatim; map collect/concat/replay onto `AggregateImpl`; delete reflect/unsafe/cgo.Handle (replaced by our per-module Go handle table, `udf_aggregate.go`).

All aggregate **math** (`helper.Aggregator` Step/Done, `kll.*`/`hll.*`/`geofn.*`/`aggregate.*` bodies) transfers **verbatim** — pure Go, cgo-free.

### **T3 — Table UDFs + spatial cleanup** — ~1-2 days → **~95%+ conformance**

What ships: the 2 change-history TVFs (`appends`, `changes`).

Tasks:
1. `RegisterTableUDF` shim: call `BindArguments` to read `ColumnInfos()`, emit `CREATE TABLE <name>(_CHANGE_TYPE VARCHAR, _CHANGE_TIMESTAMP TIMESTAMP[, _CHANGE_SEQUENCE_NUMBER VARCHAR])` (zero-row stub). **No new engine C-API.**
2. Confirm spatial no-op is wired.

Faithful row-producing table functions (`duckdb_create_table_function` + bind/init/main callbacks) are **out of scope** — the only two TVFs produce zero rows by design (`FillRow→false,nil`).

### Cumulative picture

| Tier | Adds | Cumulative conformance | Effort (eng-days) |
|---|---|---|---|
| T0 | data path, trivial SQL | ~15-25% | 3-5 |
| T1 | 456 scalar UDFs | ~70-85% | 6-10 |
| T2 | 33 aggregate UDFs | ~88-94% | 4-6 |
| T3 | 2 table UDFs + spatial | ~95%+ | 1-2 |
| **Total** | | | **~14-23 days** |

(These conformance figures were pre-run estimates. The actual run, after all tiers landed, delivered **986/994 with zero failures** — see the Outcome note at the top.)

---

## 4. Single biggest risk per tier + retirement

> **Foundational de-risking fact:** the UDF execution ABI — appending a Go closure to the engine's indirect-function table and registering it via `duckdb_create_scalar_function` / `duckdb_create_aggregate_function`, then having DuckDB call back into it with chunk pointers — **is already PROVEN.** `udf_test.go` (scalar) and `udf_agg_test.go` (aggregate) both PASS. So across all tiers, the risk is **never "can the callback mechanism work"** — that question is closed. The residual risks below are about *registration metadata*, *type coercion*, and *connection topology*, not the call ABI.

### T0 risk — **Connection sharing / shared in-memory DB (the one genuine engineering risk on the whole data path)**

The emulator hard-assumes (verified, `driver.go:194-198`): *"duckdb-go shares one underlying in-memory database across every connection minted from the same connector, so DDL on one pooled connection is visible to queries on another. We deliberately do NOT pin `MaxOpenConns(1)`: the engine issues nested statements during query execution, and a single-connection pool deadlocks them."*

**Our engine VIOLATES this today.** `converge/duckdb/driver.go:64-71` (`connector.Connect`) calls `newModule()` — **a brand-new wasm engine instance with a private linear memory** — for **every** connection. Each pooled `driver.Conn` therefore gets an **isolated in-memory database**: DDL on conn A is invisible to conn B, and the emulator's cross-conn catalog visibility breaks. (For file-backed DSNs the OS file is shared, but in-memory — the dominant emulator mode after `duckdbDSN` collapses every in-memory variant to `""` — is broken.)

**Retire by** choosing a connection-sharing model:
- **(a) Connector-scoped single engine + multiplexed connections:** one `newModule()` per `connector`, and `Connect` issues a fresh `duckdb_connect` against the **shared `duckdb_database`** (the engine already separates `db` and `con` handles — `mod.open` returns both, `module.go:171`; `conn{mod,con,db}` at `driver.go:78-85`). All conns from one connector share `db` (→ shared catalog) while each holds its own `con`. This matches DuckDB's real model and the emulator's assumption exactly.
- **(b) Re-entrancy:** the engine is single-threaded per module and conns serialize via `conn.mu` (`driver.go:80`). Under model (a), nested statements on **different** `con` handles against the same engine must not deadlock — verify the wasm engine tolerates re-entrant `duckdb_query` across connections (or serialize at module granularity with a non-reentrant guard that the nested-statement pattern won't trip). If model (a)'s shared engine can't be re-entered safely, the emulator's "no `MaxOpenConns(1)`" requirement is unsatisfiable and needs an engine-level fix.

**Test to retire:** open `*sql.DB`, `Conn` A does `CREATE TABLE t`; `Conn` B does `SELECT * FROM t` and **sees** it; a query that triggers a nested statement does not deadlock. This is the gating T0 acceptance test.

### T1 risk — **Variadic-ANY registration + un-coerced ANY args**

451/456 scalar registrations are `VariadicTypeInfo: TYPE_ANY` with `SpecialNullHandling: true`. If `DUCKDB_TYPE_ANY` is not a valid input to `Xduckdb_create_logical_type` in the v1.5.3 transpile, or if ANY-typed args arrive **coerced** (e.g. envelope VARCHAR forced to a number), the entire value-layer breaks (CONCAT, decode_array, every geo/json/proto function).

**Retire by** a spike *before* bulk registration: register one variadic-ANY UDF that echoes its raw args, bind a mix of int64 + envelope-VARCHAR, and assert the `RowExecutor` receives them un-coerced. If `create_logical_type(ANY)` is unsupported, fabricate via `set_varargs` with the C-API's "any" sentinel. Confirm `set_special_handling` actually suppresses NULL short-circuit. (The callback ABI itself is already proven — this spike only validates the *type metadata* path.)

### T2 risk — **The missing `add_aggregate_function_to_set` → overload resolution**

`Xduckdb_add_aggregate_function_to_set` is **absent** from the transpiled core (the lone C-API gap). The cgo bridge registers each aggregate as a function-set spanning `minParams..maxParams` to absorb the analyzer's default-argument injection. Without the set-builder, multi-arity overloads might not resolve.

**Retire by** the loop-register-per-arity shim (§3 T2.4): register the name once per arity with N copies of `ANY`. **Validate the assumption that DuckDB resolves same-name/different-arity aggregates as catalog-level overloads** with a 2-arity spike (e.g. `anon_count(x)` and `anon_count(x,eps)`). If catalog-level overloading fails, fall back to (b) regenerating `add_aggregate_function_to_set` into core (a thin C-API wrapper; its `_create/_register/_destroy_…_set` siblings are already present, so it was merely dropped during transpilation) and add `RegisterAggregateUDFSet`.

### T3 risk — **Table-function schema fidelity**

Low. The only risk is the emulated `CREATE TABLE` schema not matching what the formatter/analyzer expects for `APPENDS`/`CHANGES` column references. **Retire by** deriving the DDL directly from `ColumnInfos()` returned by the emulator's own `BindArguments` (so the schema is by-construction identical), and asserting an `APPENDS(...)` query parses and returns zero rows.

---

## 5. What's NOT covered / open questions

1. **`Decimal` production on Scan (unverified engine behavior).** The compat `Decimal` type is trivial, but our driver's row decoder (`converge/duckdb/result.go`) must actually *emit* it for DECIMAL columns/literals. **Not yet confirmed.** If it currently returns `float64`/`string`, NUMERIC precision and geo DECIMAL args silently degrade. Mitigated for *result columns* by the TEXT-affinity string fallback (`rows.go:131-146`) but **not** for the `duckdb.Decimal`-typed scalar-argument assertions. **Action: verify and, if needed, add the DECIMAL→`Decimal` Scan path.**

2. **`DUCKDB_TYPE_ANY` semantics in the v1.5.3 transpile.** Whether `create_logical_type(ANY)` is valid and whether ANY-typed args bypass coercion is the T1 gating unknown. Untested.

3. **Re-entrancy of the wasm engine under shared-DB multiplexing.** The "no `MaxOpenConns(1)`, nested statements during execution" requirement (§4 T0) presumes the engine tolerates concurrent/re-entrant statements across connections sharing one `duckdb_database`. The single-threaded wasm engine's behavior under this pattern is the biggest open *engine-capability* question.

4. **`set_varargs` / `set_volatile` / `set_special_handling` symbol presence** in the transpiled C-API. The catalog asserts `m.Xduckdb_scalar_function_set_*` should exist; only `set_name`/`set_return_type`/`set_function` are confirmed wired today (`udf_scalar.go:72-77`). **Grep the generated core to confirm each `X`-method exists before T1.**

5. **`Volatile` correctness.** If `set_volatile` is absent, UUID/keyset/AEAD functions get constant-folded → wrong results. No fallback identified; would need engine support.

6. **Concurrency / goroutine safety.** Our engine is "not shareable across goroutines" (`converge/duckdb/driver.go:48`). `database/sql` may dispatch pool connections from multiple goroutines. Under the shared-engine model (§4 T0 option a), goroutine isolation must be re-examined — the current per-conn `newModule()` *provides* isolation precisely because it's what we're proposing to remove. **The shared-DB requirement and the goroutine-isolation requirement are in tension; resolving §4-T0 must not reintroduce data races.** This is the deepest open design question.

7. **Faithful table functions** (`duckdb_create_table_function` + bind/init/main callback ABI) — deliberately out of scope (the 2 TVFs are zero-row stubs). If real change-history rows are ever needed, this is net-new callback-ABI work (analogous to but not covered by the proven scalar/aggregate spikes).

8. **Native spatial extension** — out of scope; static-linking DuckDB's C++ spatial into the wasm2go build is a large, separate work item triggered only by a native `ST_*` requirement that **does not exist** in this codebase.

9. **`LastInsertId`** — DuckDB has no rowid; confirm our `Result.LastInsertId()` returns `(0, nil)`/benign error and never panics (`result.go:14-19`). Low risk, unverified.

10. **Per-`sql.Open(":memory:")` freshness.** The emulator mints unique in-memory DBs per `sql.Open` via `freshMemoryDSN`/`memoryConnectorSeq` (`driver.go:323-359`) then collapses to `""`. Under the shared-engine model, confirm each *connector* (not each conn) still gets an independent in-memory catalog so two `sql.Open(":memory:")` calls don't collide.

---

## Appendix — primary citations

**Emulator (`/tmp/gsx-duckdb`):** import alias `driver.go:14` (+ 10 more files, all `duckdb "github.com/duckdb/duckdb-go/v2"`); data-path call `driver.go:177`; DSN xlate `driver.go:150-165`; shared-in-memory assumption `driver.go:194-198`; `duckdb.Decimal` `internal/decoder.go:26`, `duckdb_geo_udf.go:163`, `duckdb_aggregate_kll_extract.go:112`; scalar registrations `duckdb_udf.go:40-626` (456 total); 33 aggregate specs `duckdb_aggregate_udf.go:371-476`; reflect/unsafe conn hack `duckdb_aggregate_udf.go:539-577`; the missing `add_aggregate_function_to_set` `duckdb_aggregate_udf.go:121`; table UDF `duckdb_time_series_udf.go:40-68`; spatial load `duckdb_geo_udf.go:55-83`.

**Engine (`converge/duckdb`):** `Driver.Open` `driver.go:36`; connector minting fresh engine per conn `driver.go:64-71`; `conn{mod,con,db}` `driver.go:78-85`; interface assertions `driver.go:227-232`; `RegisterScalarUDF` `udf_scalar.go:27` (no varargs/null/volatile, `:72-77`); `RegisterAggregateUDF` `udf_aggregate.go:120` (no `set_special_handling`, finalize panics `:178-182`); `AggregateImpl` `udf_aggregate.go:42-47`; `mod.open` returns `con,db` `module.go:171`.
