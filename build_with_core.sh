#!/bin/zsh
# Build standalone DuckDB wasm INCLUDING the core_functions extension (sum/avg/...).
# -Oz keeps functions small (Go-compilable); legacy exceptions; no SIMD.
set -eu
HERE=${0:a:h}
DS=$HERE/duckdb-src
OUT=${1:-$HERE/duckdb_core_fn.wasm}

CF_SRCS=($(find "$DS/extension/core_functions" -name '*.cpp'))
echo "core_functions TUs: ${#CF_SRCS[@]}"

emcc \
  "$HERE/amalg/duckdb.cpp" \
  "${CF_SRCS[@]}" \
  "$HERE/register_core_functions.cpp" \
  -I"$DS/src/include" \
  -I"$DS/extension/core_functions/include" \
  -I"$DS/third_party/skiplist" \
  -I"$DS/third_party/pcg" \
  -I"$DS/third_party/tdigest" \
  -I"$DS/third_party/jaro_winkler" \
  -I"$DS/third_party/utf8proc/include" \
  -I"$DS/third_party/fmt/include" \
  -I"$DS/third_party/re2" \
  -I"$DS/third_party/fast_float" \
  -I"$DS/third_party/fsst" \
  -I"$DS/third_party/hyperloglog" \
  -I"$DS/third_party/fastpforlib" \
  -I"$DS/third_party/concurrentqueue" \
  -I"$DS/third_party/mbedtls/include" \
  -I"$DS/third_party/miniz" \
  -I"$DS/third_party/yyjson/include" \
  -I"$DS/third_party/utf8proc" \
  -I"$DS/third_party" \
  -Oz -std=c++17 -g0 -fexceptions -sDISABLE_EXCEPTION_CATCHING=0 \
  -sSTANDALONE_WASM -sFILESYSTEM=0 -sALLOW_MEMORY_GROWTH=1 \
  -DNDEBUG -DDUCKDB_NO_THREADS=1 -mno-simd128 --no-entry \
  -sEXPORTED_FUNCTIONS='_duckdb_open,_duckdb_connect,_duckdb_query,_duckdb_column_count,_duckdb_row_count,_duckdb_value_int64,_duckdb_value_varchar,_duckdb_result_error,_duckdb_destroy_result,_duckdb_disconnect,_duckdb_close,_duckdb_library_version,_register_core_functions,_malloc,_free' \
  -o "$OUT"

echo "built: $OUT ($(ls -la "$OUT" | awk '{print $5}') bytes)"
echo "funcs: $(wasm-objdump -x -j Function "$OUT" 2>/dev/null | grep -c 'func\[')"
