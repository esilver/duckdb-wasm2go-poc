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

echo "[$TAG] === transpile $W ==="
time wasm2go -pkg duckdbcore -o "$GEN" "$W" || { echo "[$TAG] wasm2go FAILED"; exit 1; }
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
