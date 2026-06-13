#!/bin/zsh
# Tier 2 host-FS wasm build. Compiles host_fs.cpp, a custom DuckDB FileSystem
# whose virtuals call imported host_* functions, and exports _register_host_fs.
# The undefined host_* extern "C" symbols become clean
# env.host_* wasm imports under -sERROR_ON_UNDEFINED_SYMBOLS=0; the pure-Go host
# (converge/wasishim/hostfs.go) implements them against the `os` package.
set -eu
HERE=${0:a:h}
DS=$HERE/duckdb-src
OUT=${1:-$HERE/duckdb_fs.wasm}

if [[ ! -f "$HERE/amalg/duckdb.cpp" || ! -d "$DS" ]]; then
  echo "missing DuckDB build inputs; run ./bootstrap_duckdb.sh first" >&2
  exit 1
fi
if [[ ! -f "$HERE/exports_arg.txt" ]]; then
  echo "missing exports_arg.txt" >&2
  exit 1
fi
EXPORTS="$(cat "$HERE/exports_arg.txt"),_register_host_fs,_host_fs_attach_to_config,_duckdb_open_ext,_duckdb_destroy_config"

CF_SRCS=($(find "$DS/extension/core_functions" -name '*.cpp'))
# json extension: json_each / json_extract / the JSON type. Statically linked
# like core_functions (the googlesqlite UNNEST lowering needs json_each). Its
# yyjson dependency is already inside the amalgamation — do NOT compile
# third_party/yyjson separately (duplicate symbols).
JSON_SRCS=($(find "$DS/extension/json" -name '*.cpp'))
# icu extension: timezone-aware TIMESTAMPTZ ops (timestamp_trunc/add/diff with
# zones, current_date/time/datetime, time-bucketing). The cgo libduckdb bundles
# it; without it DuckDB tries to AUTOLOAD "icu" at bind time and fails. The
# vendored icu4c subset (common/i18n/stubdata) self-configures via its patched
# uconfig.h, so no extra defines are needed — just the include dirs.
ICU_SRCS=($(find "$DS/extension/icu" -name '*.cpp'))
echo "core_functions TUs: ${#CF_SRCS[@]} ; json TUs: ${#JSON_SRCS[@]} ; icu TUs: ${#ICU_SRCS[@]} ; exported fns: $(echo $EXPORTS | tr ',' '\n' | wc -l)"

emcc \
  "$HERE/amalg/duckdb.cpp" \
  "${CF_SRCS[@]}" \
  "${JSON_SRCS[@]}" \
  "${ICU_SRCS[@]}" \
  "$HERE/register_core_functions.cpp" \
  "$HERE/host_fs.cpp" \
  -I"$DS/src/include" -I"$DS/extension/core_functions/include" \
  -I"$DS/extension/json/include" \
  -I"$DS/extension/icu/include" \
  -I"$DS/extension/icu/third_party/icu/common" \
  -I"$DS/extension/icu/third_party/icu/i18n" \
  -I"$DS/third_party/skiplist" -I"$DS/third_party/pcg" -I"$DS/third_party/tdigest" \
  -I"$DS/third_party/jaro_winkler" -I"$DS/third_party/utf8proc/include" \
  -I"$DS/third_party/fmt/include" -I"$DS/third_party/re2" -I"$DS/third_party/fast_float" \
  -I"$DS/third_party/fsst" -I"$DS/third_party/hyperloglog" -I"$DS/third_party/fastpforlib" \
  -I"$DS/third_party/concurrentqueue" -I"$DS/third_party/mbedtls/include" \
  -I"$DS/third_party/miniz" -I"$DS/third_party/yyjson/include" -I"$DS/third_party/utf8proc" \
  -I"$DS/third_party" \
  -Oz -std=c++17 -g0 -fexceptions -sDISABLE_EXCEPTION_CATCHING=0 \
  -sSTANDALONE_WASM -sALLOW_MEMORY_GROWTH=1 -sERROR_ON_UNDEFINED_SYMBOLS=0 \
  -DNDEBUG -DDUCKDB_NO_THREADS=1 -mno-simd128 --no-entry \
  -sEXPORTED_FUNCTIONS="$EXPORTS" \
  -o "$OUT"

echo "built: $OUT ($(ls -la "$OUT" | awk '{print $5}') bytes)"
echo "funcs: $(wasm-objdump -x -j Function "$OUT" 2>/dev/null | grep -c 'func\[')"
echo "=== env.host_* imports (the new Path-B surface) ==="
wasm-objdump -j Import -x "$OUT" 2>/dev/null | grep -oE 'env\.host_[A-Za-z0-9_]+' | sort -u
echo "=== other new WASI/env imports ==="
wasm-objdump -j Import -x "$OUT" 2>/dev/null | grep -oE '(wasi_snapshot_preview1|env)\.[A-Za-z0-9_]+' | grep -viE 'invoke_|__cxa|llvm_eh|__resume|host_' | sort -u
