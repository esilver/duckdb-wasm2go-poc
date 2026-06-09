#!/bin/zsh
# Build the STANDALONE DuckDB-core wasm (the shape wasm2go can ingest).
#
# Requires amalg/duckdb.cpp + amalg/duckdb.h (DuckDB v1.5.3). Get them with:
#   gh release download v1.5.3 --repo duckdb/duckdb --pattern 'libduckdb-src.zip'
#   mkdir -p amalg && (cd amalg && unzip -o ../libduckdb-src.zip)
#
# Flag rationale: see README.md. Do not switch to -fwasm-exceptions, -msimd128,
# or -sMAIN_MODULE - any of those breaks the required shape. -O0 is deliberate:
# -O1 whole-module optimization exhausts memory on a 16 GB host.
set -eu
HERE=${0:a:h}
SRC=${1:-$HERE/amalg/duckdb.cpp}
OUT=${2:-$HERE/duckdb_core.wasm}

emcc "$SRC" -O0 -std=c++17 -fexceptions -sDISABLE_EXCEPTION_CATCHING=0 \
  -sSTANDALONE_WASM -sFILESYSTEM=0 -sALLOW_MEMORY_GROWTH=1 \
  -DDUCKDB_NO_THREADS=1 -DDUCKDB_DISABLE_EXTENSIONS -mno-simd128 --no-entry \
  -sEXPORTED_FUNCTIONS='_duckdb_open,_duckdb_connect,_duckdb_query,_duckdb_column_count,_duckdb_row_count,_duckdb_value_int64,_duckdb_value_varchar,_duckdb_result_error,_duckdb_destroy_result,_duckdb_disconnect,_duckdb_close,_duckdb_library_version,_malloc,_free' \
  -o "$OUT"

echo "built: $OUT ($(ls -la "$OUT" | awk '{print $5}') bytes)"
