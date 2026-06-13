#!/bin/zsh
# Fetch the local-only DuckDB inputs needed by rebuild_fs_all.sh.
#
# This script is intentionally separate from rebuild_fs_all.sh: fetching DuckDB
# sources is networked setup, while rebuild_fs_all.sh is the RAM-heavy compiler
# path. The fetched directories are gitignored.
set -eu

HERE=${0:a:h}
DUCKDB_VERSION=${DUCKDB_VERSION:-v1.5.3}
DUCKDB_REPO=${DUCKDB_REPO:-https://github.com/duckdb/duckdb.git}
LIBDUCKDB_SRC_URL=${LIBDUCKDB_SRC_URL:-https://github.com/duckdb/duckdb/releases/download/$DUCKDB_VERSION/libduckdb-src.zip}

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required command: $1" >&2
    exit 1
  fi
}

require_cmd git
require_cmd curl
require_cmd unzip

mkdir -p "$HERE/amalg" "$HERE/converge/genpkg"

if [[ ! -f "$HERE/amalg/duckdb.cpp" ]]; then
  echo "### fetching DuckDB amalgamation: $LIBDUCKDB_SRC_URL"
  tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/duckdb-src-amalg.XXXXXX")
  trap 'rm -rf "$tmpdir"' EXIT
  zip="$tmpdir/libduckdb-src.zip"
  curl -L --fail --retry 3 -o "$zip" "$LIBDUCKDB_SRC_URL"
  unzip -q "$zip" -d "$tmpdir/unpacked"
  duck_cpp=$(find "$tmpdir/unpacked" -name duckdb.cpp -type f | head -n 1)
  if [[ -z "$duck_cpp" ]]; then
    echo "libduckdb-src zip did not contain duckdb.cpp" >&2
    exit 1
  fi
  cp "$duck_cpp" "$HERE/amalg/duckdb.cpp"
else
  echo "### amalg/duckdb.cpp already present"
fi

if [[ ! -d "$HERE/duckdb-src/.git" ]]; then
  echo "### cloning DuckDB source tree: $DUCKDB_VERSION"
  git clone --depth=1 --branch "$DUCKDB_VERSION" --filter=blob:none --sparse \
    "$DUCKDB_REPO" "$HERE/duckdb-src"
  (
    cd "$HERE/duckdb-src"
    git sparse-checkout set \
      src/include \
      extension/core_functions \
      extension/json \
      extension/icu \
      third_party \
      data \
      test/sql
  )
else
  echo "### duckdb-src already present"
fi

echo "### bootstrap complete"
echo "Next: ./rebuild_fs_all.sh"
