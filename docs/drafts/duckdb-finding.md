# DuckDB divergence note (draft, NOT filed) — status: bisect pending

While running DuckDB's own sqllogictest corpus (3,322 `.test` files; 43,565 /
43,789 records pass = 99.49%) against a pure-Go build of DuckDB v1.5.3
(amalgamation + core_functions/json/icu, compiled to standalone wasm with
`emcc -Oz -DNDEBUG -mno-simd128 -fexceptions`, transpiled by ncruces/wasm2go,
Go-compiled with `-N -l` on the generated package), two failure buckets look
like **genuine engine-behavior divergences** rather than harness or decode
issues:

1. **UHUGEINT -> FLOAT cast saturation.**
   `test/sql/types/numeric/uhugeint_try_cast.test` line 111:
   `SELECT i::FLOAT FROM uhugeints ORDER BY i` — the expected output prints
   `340282366920938463463374607431768211455` (2^128−1 round-tripped through
   FLOAT); the transpiled build saturates/diverges on the max value. Suspect
   the u128→f32 soft-float lowering path (`__floatuntisf`-equivalent) under
   the ILP32 `-Oz` wasm build.

2. **Storage compression codec selection.**
   Six `test/sql/storage/compression/string/*.test` files (`simple`, `blob`,
   `empty`, `big_strings`, `filter_pushdown`, `index_fetch`) fail the final
   check `SELECT lower(compression)='${compression}' FROM
   pragma_storage_info('test') ...` — the engine picks a different string
   codec than the test forces/expects. Data round-trips correctly; only the
   chosen codec differs (possibly float-cost tie-breaking in the analyze
   phase under soft-float).

**Status: bisect pending.** Neither bucket should be reported upstream to
duckdb/duckdb until the layer is isolated — candidate culprits, in order:
(a) the `-Oz`/`-mno-simd128`/ILP32 emcc build itself (reproducible under a
real wasm runtime?), (b) the wasm2go translation, (c) the `-N -l` Go compile.
The no-opt-lab bisect (chainA, expected at `/tmp/nooptlab/ISSUE.md`) had not
produced results at the time of writing; this note should be updated with the
bisect outcome before anything is filed. If (a) reproduces under wazero or
Emscripten's own runtime, it becomes a legitimate duckdb/duckdb wasm-build
finding; if only (b)/(c), it is ours/wasm2go's, not DuckDB's.

Repro assets: engine repo https://github.com/esilver/duckdb-wasm2go-poc
(`converge/cmd/sqllogic` runner; `rebuild_fs_all.sh` regenerates the build);
corpus report saved at `/tmp/sqllogic_salvage.txt`.
