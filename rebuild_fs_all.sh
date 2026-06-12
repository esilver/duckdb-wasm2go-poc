#!/bin/zsh
# Full Tier-2 pipeline: build the host-FS wasm (build_fs.sh) -> regen exhost
# invokes -> transpile -> split New -> go build. This is rebuild_all.sh for the
# duckdb_fs.wasm flavor the current converge/genpkg is generated from (host
# filesystem imports + the full C-API export list in exports_arg.txt).
set -eu
HERE=${0:a:h}
export PATH="$(go env GOPATH)/bin:/opt/homebrew/bin:$PATH"
export GOTOOLCHAIN=go1.26.2 CGO_ENABLED=0
WASM=$HERE/duckdb_fs.wasm

# --- transpiler pin + version gate (issue #1) -------------------------------
# Never run a bare `wasm2go` from PATH (the GOPATH/bin export above would win):
# v0.3.0..v0.4.6 had a lazy-evaluation output-corruption bug (upstream
# ncruces/wasm2go#31, fixed v0.4.7) that silently regenerates a corrupted
# engine and propagates into duckdb-go-pure via import_engine.sh.
WASM2GO_VERSION=${WASM2GO_VERSION:-v0.4.9}
WASM2GO_MIN=v0.4.7
if [[ "$(printf '%s\n' "$WASM2GO_MIN" "$WASM2GO_VERSION" | sort -V | head -n1)" != "$WASM2GO_MIN" ]]; then
  echo "FATAL: wasm2go $WASM2GO_VERSION < $WASM2GO_MIN — versions v0.3.0..v0.4.6 emit memory-corrupted Go (upstream issue 31). Set WASM2GO_VERSION=$WASM2GO_MIN or newer." >&2
  exit 1
fi
wasm2go() { go run "github.com/ncruces/wasm2go@$WASM2GO_VERSION" "$@"; }
# ----------------------------------------------------------------------------

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
echo "wasm2go $WASM2GO_VERSION" > "$HERE/converge/genpkg/TRANSPILER_VERSION"

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
  # level, which OOMs the optimizer on the unsplit shards (>28GB seen).
  python3 "$HERE/scripts/transform_genopt.py"

  echo "### 4d. split giant functions (scripts/split_giant_fns.py) — enables genopt WITHOUT '-l'"
  # Functions over ~8k lines explode the inliner's IR (>50GB OOM) and Fn6568
  # exceeds the compiler's 65536-SSA-block hard cap outright (bricks GOOS=js).
  # Splitting them into semantically identical part-functions lets every
  # genopt package compile fully optimized with NO '-l' (2.3-2.9x runtime).
  genopt_files=("$HERE"/converge/genopt/core/core.go "$HERE"/converge/genopt/shard*/shard.go)
  python3 "$HERE/scripts/split_giant_fns.py" --threshold 4000 ${genopt_files:#*/shard20/*}
  # shard20 special case: Fn1308 is the known IR bomb — at the 8k split it
  # still needs >12.7GB; at 4k it compiles in 1.19GB. Finer-grained split.
  python3 "$HERE/scripts/split_giant_fns.py" --threshold 4000 --max-part 4000 \
    "$HERE/converge/genopt/shard20/shard.go"
fi

echo "### 4b. inline hot wasm helpers (textual; ~1.7-2x runtime, see scripts/inline_helpers.py)"
python3 "$HERE/scripts/inline_helpers.py" "$HERE/converge/genpkg/gen.go"

if [[ "${GENOPT:-0}" == 1 ]]; then
  echo "### 4c2. genopt compile check (NO '-l' — step 4d made it unnecessary; -p 1/-c=1 are optional RAM bounding, not selectors)"
  cd "$HERE/converge"
  time go build -tags genopt -p 1 \
    -gcflags='duckdbconverge/genopt/...=-c=1' ./duckdb/...
  cd "$HERE"
fi

echo "### 5. go build (compile check)"
cd "$HERE/converge"
time go build -gcflags='duckdbconverge/genpkg=-N -l -c=16' ./...
echo "### rebuild_fs_all: DONE"
