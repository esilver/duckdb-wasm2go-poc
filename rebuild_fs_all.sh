#!/bin/zsh
# Full Tier-2 pipeline: build the host-FS wasm (build_fs.sh) -> regen exhost
# invokes -> transpile -> split New -> go build. This is rebuild_all.sh for the
# duckdb_fs.wasm flavor the current converge/genpkg is generated from (host
# filesystem imports + the full C-API export list in exports_arg.txt).
set -eu
HERE=${0:a:h}
export PATH="$(go env GOPATH)/bin:/opt/homebrew/bin:$PATH"
export GOTOOLCHAIN=go1.25.6 CGO_ENABLED=0
WASM=$HERE/duckdb_fs.wasm

echo "### 1. build wasm (core_functions + host FS, NDEBUG, -Oz)"
"$HERE/build_fs.sh" "$WASM"

echo "### 2. regen exhost invokes for exact set"
wasm-objdump -j Import -x "$WASM" 2>/dev/null | grep -oE 'invoke_[A-Za-z]+' | sort -u > /tmp/ra_want.txt
NAMES=$(paste -sd, /tmp/ra_want.txt)
# GOWORK=off: the harness module is intentionally not in go.work (it is a
# build-time tool, not part of the converge workspace).
(cd "$HERE/harness" && GOWORK=off go run ./gen-invokes -names "$NAMES" -o "$HERE/converge/exhost/invokes.go")

echo "### 3. transpile (-embed -unsafe)"
rm -f "$HERE/converge/genpkg/gen.go" "$HERE/converge/genpkg/gen.dat"
wasm2go -embed -unsafe -pkg duckdbcore -o "$HERE/converge/genpkg/gen.go" "$WASM"

echo "### 4. split New()"
python3 "$HERE/split_new.py" "$HERE/converge/genpkg/gen.go"

if [[ "${GENOPT:-0}" == 1 ]]; then
  echo "### 4c. GENOPT=1: shard into converge/genopt (fully-optimized engine)"
  # Multi-package transform (scripts/transform_genopt.py): 42.5k fnNNN methods
  # -> free functions in genopt/core + ~29 shards; cross-shard calls go through
  # TBL_FnN func-vars. Per-package compiler memory makes full optimization
  # feasible (~131s / 2.75GB peak vs >50GB OOM on the monolith); the engine is
  # selected with -tags genopt (see converge/duckdb/module_engine_genopt.go).
  # MUST run BEFORE step 4b: the textual inliner pre-expands the IR at source
  # level, which defeats the shards' '-l' and OOMs the optimizer (>28GB seen).
  python3 "$HERE/scripts/transform_genopt.py"
fi

echo "### 4b. inline hot wasm helpers (textual; ~1.7-2x runtime, see scripts/inline_helpers.py)"
python3 "$HERE/scripts/inline_helpers.py" "$HERE/converge/genpkg/gen.go"

if [[ "${GENOPT:-0}" == 1 ]]; then
  echo "### 4c2. genopt compile check (serial; core -c=1, shards -l -c=1 — '-l' mandatory)"
  cd "$HERE/converge"
  time go build -tags genopt -p 1 \
    -gcflags='duckdbconverge/genopt/...=-l -c=1' \
    -gcflags='duckdbconverge/genopt/core=-c=1' ./duckdb/...
  cd "$HERE"
fi

echo "### 5. go build (compile check)"
cd "$HERE/converge"
time go build -gcflags='duckdbconverge/genpkg=-N -l -c=16' ./...
echo "### rebuild_fs_all: DONE"
