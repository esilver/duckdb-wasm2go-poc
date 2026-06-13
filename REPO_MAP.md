# DuckDB Wasm2Go PoC Repo Map

This repository keeps the active engine path at the top level. Superseded
scripts, spikes, stale narrative notes, and unfiled draft material were removed
from the tree.

## Current entry points

- `rebuild_fs_all.sh`: current host-filesystem engine rebuild. With `GENOPT=1`
  it also emits and compile-checks the optimized sharded engine layout.
- `rebuild_parquet.sh`: parquet engine regeneration path for an already-staged
  parquet-flavored `duckdb_fs.wasm`. It is intentionally separate because the
  parquet wasm build itself is still a manual developer lane.
- `build_fs.sh`: current wasm build script used by `rebuild_fs_all.sh`.
- `verify_shape.sh`: checks the wasm shape before transpilation.
- `split_new.py` and `scripts/`: source transforms applied after wasm2go.
- `host_fs.cpp`, `register_core_functions.cpp`, `exports_arg.txt`: C++ inputs
  used by the current wasm build.
- `converge/`: Go host, driver surface, generated-engine selectors, tests, and
  sqllogictest runner.
- `harness/gen-invokes`: live build-time helper that regenerates exception
  trampoline wrappers for a wasm import set.

## Generated or local-only material

- `duckdb-src/`, `duckdb_fs.wasm`, `converge/genpkg/`, and `converge/genopt/`
  are intentionally gitignored and regenerated or staged locally.
- `.gocache/` is local cache only.
