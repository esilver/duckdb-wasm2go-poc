# Repo map

The active engine path lives at the top level. Superseded scripts, spikes, and
stale notes were removed from the tree.

## Vocabulary

Terms that recur in code comments (cmd/demo, fstest, module.go, wasishim,
main.go):

- **Tier 1**: the in-memory `database/sql` driver surface.
- **Tier 2**: host-filesystem persistence (file-backed databases, reading files
  off disk) through the WASI filesystem shim.
- **Path B**: the custom DuckDB `FileSystem` compiled into the wasm
  (`host_fs.cpp`), whose virtuals call `env.host_*` imports that
  `wasishim/hostfs.go` services against the real OS. This is how Tier 2 reaches
  the host.

## Entry points

- `bootstrap_duckdb.sh`: clean-clone setup of the gitignored DuckDB inputs and
  output dirs consumed by `rebuild_fs_all.sh`.
- `rebuild_fs_all.sh`: host-filesystem engine rebuild. `GENOPT=1` also emits and
  compile-checks the optimized sharded layout.
- `rebuild_parquet.sh`: parquet regeneration from an already-staged parquet
  `duckdb_fs.wasm` (separate because that wasm build is still a manual lane).
- `build_fs.sh`: the wasm build script `rebuild_fs_all.sh` calls.
- `verify_shape.sh`: checks the wasm shape before transpilation.

Source transforms applied to the wasm2go output:

- `split_new.py`: chunks the one function-table init that overflows the Go
  compiler's liveness bitmap into small helper methods.
- `scripts/inline_helpers.py`: inlines the hot memory helpers (load32/store32/i32)
  so the `-N -l` genpkg build stops paying a Go call per memory access.
- `scripts/transform_genopt.py`: shards the `fnNNN` methods across
  `converge/genopt/{core,shardNN}` so each package compiles fully optimized (Go
  compiler memory is per-package).
- `scripts/split_giant_fns.py`: splits functions over a line threshold to stay
  under the Go compiler's 65536-SSA-block cap and bound the genopt inliner's IR.

C++ inputs to the wasm build: `host_fs.cpp`, `register_core_functions.cpp`,
`exports_arg.txt`.

`converge/`: Go host, driver surface, generated-engine selectors, tests, and the
sqllogictest runner.

`harness/`: historical exception/WASI validation harness plus the live
`gen-invokes` helper (regenerates exception trampolines for an import set). Its
demo needs `./build-poc.sh` and the `harness_generated` tag, and `harness/` is
excluded from `go.work`. `harness/exhost` and `harness/wasishim` are frozen
snapshots from the validation era. The live versions are `converge/exhost` and
`converge/wasishim`, which have since diverged.

## Generated or local-only

`amalg/`, `duckdb-src/`, `duckdb_fs.wasm`, `converge/genpkg/`, and
`converge/genopt/` are gitignored and regenerated or staged locally. `.gocache/`
is local cache only.

## Engine seam: three names, one thing

At the driver-to-engine seam, one generated engine wears three names:
`gen.go` declares `package duckdbcore`, is imported by its directory path
`duckdbconverge/genpkg` (so call sites say "genpkg"), and is aliased to
`engineModule` in `converge/duckdb/module_engine_genpkg.go`. All three point at
the same package.
