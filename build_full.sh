#!/bin/zsh
# Tier1+2 build: standalone DuckDB wasm with core_functions, the FULL C-API export
# surface (prepared statements, chunk/vector reading, type introspection, 166 fns),
# and the FILESYSTEM ENABLED (file I/O -> WASI imports the Go wasishim implements).
set -eu
HERE=${0:a:h}
DS=$HERE/duckdb-src
OUT=${1:-$HERE/duckdb_full.wasm}
EXPORTS=$(cat /tmp/exports_arg.txt)

CF_SRCS=($(find "$DS/extension/core_functions" -name '*.cpp'))
echo "core_functions TUs: ${#CF_SRCS[@]} ; exported fns: $(echo $EXPORTS | tr ',' '\n' | wc -l)"

emcc \
  "$HERE/amalg/duckdb.cpp" \
  "${CF_SRCS[@]}" \
  "$HERE/register_core_functions.cpp" \
  -I"$DS/src/include" -I"$DS/extension/core_functions/include" \
  -I"$DS/third_party/skiplist" -I"$DS/third_party/pcg" -I"$DS/third_party/tdigest" \
  -I"$DS/third_party/jaro_winkler" -I"$DS/third_party/utf8proc/include" \
  -I"$DS/third_party/fmt/include" -I"$DS/third_party/re2" -I"$DS/third_party/fast_float" \
  -I"$DS/third_party/fsst" -I"$DS/third_party/hyperloglog" -I"$DS/third_party/fastpforlib" \
  -I"$DS/third_party/concurrentqueue" -I"$DS/third_party/mbedtls/include" \
  -I"$DS/third_party/miniz" -I"$DS/third_party/yyjson/include" -I"$DS/third_party/utf8proc" \
  -I"$DS/third_party" \
  -Oz -std=c++17 -g0 -fexceptions -sDISABLE_EXCEPTION_CATCHING=0 \
  -sSTANDALONE_WASM -sALLOW_MEMORY_GROWTH=1 \
  -DNDEBUG -DDUCKDB_NO_THREADS=1 -mno-simd128 --no-entry \
  -sEXPORTED_FUNCTIONS="$EXPORTS" \
  -o "$OUT"

echo "built: $OUT ($(ls -la "$OUT" | awk '{print $5}') bytes)"
echo "funcs: $(wasm-objdump -x -j Function "$OUT" 2>/dev/null | grep -c 'func\[')"
echo "=== new WASI/fs imports (wasishim must implement) ==="
wasm-objdump -j Import -x "$OUT" 2>/dev/null | grep -oE '(wasi_snapshot_preview1|env)\.[A-Za-z0-9_]+' | grep -viE 'invoke_|__cxa|llvm_eh|__resume' | sort -u
