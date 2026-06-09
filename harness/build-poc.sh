#!/usr/bin/env bash
# Rebuild the validation wasms and regenerate their Go, exactly as the real
# DuckDB target will be built. Run from the harness dir.
set -euo pipefail
cd "$(dirname "$0")"

EMCC_FLAGS=(-O1 -fexceptions -sDISABLE_EXCEPTION_CATCHING=0 -sSTANDALONE_WASM
            -sFILESYSTEM=0 -mno-simd128 --no-entry)

# poc.wasm: C-API-shaped surface, minimal imports (env exception ABI only).
emcc poc.cc "${EMCC_FLAGS[@]}" \
  -sEXPORTED_FUNCTIONS='_db_open,_db_close,_query_scalar,_scalar_or_throw,_echo_len,_malloc,_free' \
  -o poc.wasm
wasm2go -pkg poc -o genpkg/gen.go poc.wasm

# wasiprobe.wasm: forces the wasi_snapshot_preview1 + emscripten env surface
# (fd_write, clock_time_get, memory growth) that an in-memory DuckDB build hits.
emcc wasitest/wasiprobe.cc "${EMCC_FLAGS[@]}" \
  -sALLOW_MEMORY_GROWTH=1 -sINITIAL_MEMORY=2MB \
  -sEXPORTED_FUNCTIONS='_touch_io,_malloc,_free' \
  -o wasitest/wasiprobe.wasm
wasm2go -pkg wp -o wasitest/wpgen/gen.go wasitest/wasiprobe.wasm

# Regenerate the invoke trampoline baseline (or pass -names for an exact set).
go run ./gen-invokes -o exhost/invokes.go

echo "OK: wasms + generated Go rebuilt"
