#!/bin/zsh
# Full pipeline: build core_functions wasm -> regen invokes -> transpile -> split New
# -> go build -> run. One shot. Output binary: converge/duckdb_run_fn.
set -eu
HERE=${0:a:h}
export PATH="$(go env GOPATH)/bin:/opt/homebrew/bin:$PATH"
export GOTOOLCHAIN=go1.25.6 CGO_ENABLED=0
WASM=$HERE/duckdb_core_fn.wasm

echo "### 1. build wasm (core_functions, NDEBUG, -Oz)"
"$HERE/build_with_core.sh" "$WASM"

echo "### 2. regen exhost invokes for exact set"
wasm-objdump -j Import -x "$WASM" 2>/dev/null | grep -oE 'invoke_[A-Za-z]+' | sort -u > /tmp/ra_want.txt
NAMES=$(paste -sd, /tmp/ra_want.txt)
(cd "$HERE/harness" && go run ./gen-invokes -names "$NAMES" -o "$HERE/converge/exhost/invokes.go")

echo "### 3. transpile (-embed -unsafe)"
rm -f "$HERE/converge/genpkg/gen.go" "$HERE/converge/genpkg/gen.dat"
wasm2go -embed -unsafe -pkg duckdbcore -o "$HERE/converge/genpkg/gen.go" "$WASM"

echo "### 4. split New()"
python3 "$HERE/split_new.py" "$HERE/converge/genpkg/gen.go"

echo "### 5. go build"
cd "$HERE/converge"
time go build -gcflags='duckdbconverge/genpkg=-N -l -c=16' -o duckdb_run_fn .

echo "### 6. run"
./duckdb_run_fn
