#!/bin/zsh
# Transpile a standalone wasm with wasm2go, then check the Go parses.
# Usage: ./transpile.sh [duckdb_core.wasm]
set -u
HERE=${0:a:h}
W=${1:-$HERE/duckdb_core.wasm}
GEN=$HERE/duckdb_core_gen.go

echo "=== wasm2go transpile (watch for 'unsupported opcode') ==="
wasm2go -pkg duckdbcore -o "$GEN" "$W"
echo "produced: $GEN"
ls -la "$GEN" 2>&1 | awk '{print "  size:", $5, "bytes"}'
echo "  lines: $(wc -l < "$GEN" 2>/dev/null)"

echo "=== parsecheck (go/parser) ==="
# Build the parsecheck binary, then pass the file as a runtime arg.
# (`go run main.go <path>` would mis-read <path> as a second source file.)
(cd "$HERE/parsecheck" && go build -o parsecheck main.go) && \
  "$HERE/parsecheck/parsecheck" "$GEN"
