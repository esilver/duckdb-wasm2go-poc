#!/bin/zsh
# Drive Track C (gc-sections wasm) and watch both A and C to completion.
# Track A is already running detached -> /tmp/pipelineA.log (converge/).
set -u
HERE=${0:a:h}
export PATH="$(go env GOPATH)/bin:/opt/homebrew/bin:$PATH"

# 1. Wait for the Track C wasm to finish building.
echo "waiting for duckdb_core_gc.wasm ..."
while [ ! -s "$HERE/duckdb_core_gc.wasm" ] || pgrep -f 'emcc.py.*duckdb_core_gc' >/dev/null; do sleep 5; done
echo "gc wasm ready: $(ls -lah "$HERE/duckdb_core_gc.wasm" | awk '{print $5}')"
gcfuncs=$(wasm-objdump -x -j Function "$HERE/duckdb_core_gc.wasm" 2>/dev/null | grep -c 'func\[')
echo "gc wasm function count: $gcfuncs"

# 2. Stand up a separate module dir for Track C so transpiles don't collide.
if [ ! -d "$HERE/converge_c" ]; then
  mkdir -p "$HERE/converge_c"
  cp "$HERE/converge/go.mod" "$HERE/converge_c/"
  cp -R "$HERE/converge/exhost" "$HERE/converge/wasishim" "$HERE/converge_c/"
  cp "$HERE/converge/main.go" "$HERE/converge_c/"
  mkdir -p "$HERE/converge_c/genpkg"
fi

# 3. Launch Track C pipeline (transpile gc wasm -> build -> run) in converge_c.
( "$HERE/run_converge.sh" "$HERE/duckdb_core_gc.wasm" C "$HERE/converge_c" ) > /tmp/pipelineC.log 2>&1 &

# 4. Watch both logs; exit when either succeeds or both fail (or 100-min cap).
done_re='value=42|EXIT rc=|FAILED|internal compiler error'
for i in $(seq 1 600); do
  a=$(grep -hoE 'value=[0-9-]+|EXIT rc=[0-9]+|go build FAILED|wasm2go FAILED|internal compiler error' /tmp/pipelineA.log 2>/dev/null | tr '\n' ' ')
  c=$(grep -hoE 'value=[0-9-]+|EXIT rc=[0-9]+|go build FAILED|wasm2go FAILED|internal compiler error' /tmp/pipelineC.log 2>/dev/null | tr '\n' ' ')
  echo "[$i] A:{$a} C:{$c}"
  if echo "$a $c" | grep -q 'value=42'; then echo "SUCCESS detected"; break; fi
  if echo "$a" | grep -q 'EXIT rc=' && echo "$c" | grep -q 'EXIT rc='; then echo "both terminated"; break; fi
  sleep 30
done
echo "=== A tail ==="; tail -15 /tmp/pipelineA.log | grep -vE 'module wants'
echo "=== C tail ==="; tail -15 /tmp/pipelineC.log | grep -vE 'module wants'
