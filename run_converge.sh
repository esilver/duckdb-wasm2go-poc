#!/bin/zsh
# Transpile a standalone DuckDB wasm and run the convergence driver to SELECT 1.
# Usage: ./run_converge.sh <wasm> <tag>   (tag labels logs, e.g. A or B)
set -u
HERE=${0:a:h}
W=${1:?wasm path}
TAG=${2:-X}
CDIR=${3:-$HERE/converge}
GEN=$CDIR/genpkg/gen.go
export GOTOOLCHAIN=go1.25.6
export CGO_ENABLED=0
export PATH="$(go env GOPATH)/bin:/opt/homebrew/bin:$PATH"

# Pinned transpiler (issue #1): never run a bare `wasm2go` from PATH — binaries
# built from v0.3.0..v0.4.6 had a lazy-evaluation output-corruption bug
# (upstream ncruces/wasm2go#31, fixed v0.4.7) that silently poisons gen.go.
WASM2GO_VERSION=${WASM2GO_VERSION:-v0.4.9}
wasm2go() { go run "github.com/ncruces/wasm2go@$WASM2GO_VERSION" "$@"; }

echo "[$TAG] === transpile $W ==="
time wasm2go -pkg duckdbcore -o "$GEN" "$W" || { echo "[$TAG] wasm2go FAILED"; exit 1; }
echo "wasm2go $WASM2GO_VERSION" > "$CDIR/genpkg/TRANSPILER_VERSION"
ls -lah "$GEN"

echo "[$TAG] === invoke-set check (regen if mismatch) ==="
WANT=$(wasm-objdump -j Import -x "$W" | grep -oE 'invoke_[A-Za-z]+' | sort -u | paste -sd, -)
echo "[$TAG] module wants invokes: $WANT"

echo "[$TAG] === go build (-N -l, no-opt: correctness not speed) ==="
cd "$CDIR"
time go build -gcflags=all='-N -l' -p 16 -o duckdb_run_$TAG . || { echo "[$TAG] go build FAILED"; exit 1; }

echo "[$TAG] === RUN ==="
time ./duckdb_run_$TAG
echo "[$TAG] EXIT rc=$?"
