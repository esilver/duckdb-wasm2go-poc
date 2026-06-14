# DuckDB Wasm2Go PoC Repo Map

This repository keeps the active engine path at the top level. Superseded
scripts, spikes, stale narrative notes, and unfiled draft material were removed
from the tree.

## Vocabulary

These terms recur in code comments (cmd/demo, fstest, module.go, wasishim,
main.go):

- **Tier 1**: the in-memory `database/sql` driver surface.
- **Tier 2**: host-filesystem persistence (file-backed databases, reading files
  off disk) through the WASI filesystem shim.
- **Path B**: the custom DuckDB `FileSystem` route compiled into the wasm
  (`host_fs.cpp`), whose virtuals call `env.host_*` imports that
  `wasishim/hostfs.go` services against the real OS. It is how Tier 2 reaches
  the host.

## Current entry points

- `rebuild_fs_all.sh`: current host-filesystem engine rebuild. With `GENOPT=1`
  it also emits and compile-checks the optimized sharded engine layout.
- `bootstrap_duckdb.sh`: clean-clone setup for the gitignored DuckDB source
  inputs and generated-output directories consumed by `rebuild_fs_all.sh`.
- `rebuild_parquet.sh`: parquet engine regeneration path for an already-staged
  parquet-flavored `duckdb_fs.wasm`. It is intentionally separate because the
  parquet wasm build itself is still a manual developer lane.
- `build_fs.sh`: current wasm build script used by `rebuild_fs_all.sh`.
- `verify_shape.sh`: checks the wasm shape before transpilation.
- Python source transforms applied to the wasm2go output:
  - `split_new.py`: chunks the one-function table init that overflows the Go
    compiler's liveness bitmap into many small helper methods.
  - `scripts/inline_helpers.py`: textually inlines the hot wasm memory helpers
    (load32/store32/i32) so the `-N -l` genpkg build stops paying a Go call per
    memory access.
  - `scripts/transform_genopt.py`: shards the `fnNNN` methods across
    `converge/genopt/{core,shardNN}` so each package is small enough to compile
    fully optimized (Go compiler memory is per-package).
  - `scripts/split_giant_fns.py`: splits transpiled functions over a line
    threshold so they stay under the Go compiler's 65536-SSA-block cap and the
    genopt inliner's IR stays bounded.
- `host_fs.cpp`, `register_core_functions.cpp`, `exports_arg.txt`: C++ inputs
  used by the current wasm build.
- `converge/`: Go host, driver surface, generated-engine selectors, tests, and
  sqllogictest runner.
- `harness/gen-invokes`: live build-time helper that regenerates exception
  trampoline wrappers for a wasm import set.
- `harness/`: historical exception/WASI validation harness plus the live
  `gen-invokes` helper. Its runnable demo requires `./build-poc.sh` and the
  `harness_generated` build tag. `harness/` is excluded from `go.work`.
- `harness/exhost` and `harness/wasishim`: frozen snapshots from the validation
  era, not maintained copies. The live versions the engine actually builds
  against are `converge/exhost` and `converge/wasishim`, which have since
  diverged from these forks.

## Generated or local-only material

- `amalg/`, `duckdb-src/`, `duckdb_fs.wasm`, `converge/genpkg/`, and
  `converge/genopt/` are intentionally gitignored and regenerated or staged
  locally.
- `.gocache/` is local cache only.

## Engine seam: three names, one thing

At the driver-to-engine seam, one generated engine wears three names. The
default-build engine in `converge/genpkg/gen.go` declares `package duckdbcore`,
is imported by its directory path `duckdbconverge/genpkg` (so call sites and
comments say "genpkg"), and is aliased to `engineModule` in
`converge/duckdb/module_engine_genpkg.go` for the rest of the driver. So
`duckdbcore` (package name), `genpkg` (import path / dir), and `engineModule`
(driver-side alias) all point at the same package.
