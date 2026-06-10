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

echo "### 4b. inline hot wasm helpers (textual; ~1.7-2x runtime, see scripts/inline_helpers.py)"
python3 "$HERE/scripts/inline_helpers.py" "$HERE/converge/genpkg/gen.go"

echo "### 5. go build (compile check)"
cd "$HERE/converge"
time go build -gcflags='duckdbconverge/genpkg=-N -l -c=16' ./...
echo "### rebuild_fs_all: DONE"
