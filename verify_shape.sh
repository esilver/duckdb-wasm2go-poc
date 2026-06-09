#!/bin/zsh
# Full shape verification per the deliverable spec. Arg1 = wasm file.
HERE=${0:a:h}
W=${1:-$HERE/duckdb_core.wasm}
echo "############ SHAPE VERIFY: $W ############"
echo "size: $(ls -la "$W" | awk '{print $5}') bytes"
echo
echo "=== (C1) sections - NO dylink.0 expected ==="
wasm-objdump -h "$W" 2>&1
echo
echo "=== (C2) memory/table/GOT - DEFINES+EXPORTS own, no GOT imports ==="
wasm-objdump -x "$W" 2>&1 | rg -i 'memory\[|table\[|GOT|-> "memory"|-> "__indirect|import.*memory|import.*table'
echo
echo "=== (C3) EH opcodes - expect 0 ==="
# Match opcodes at INSTRUCTION position (leading whitespace) only. A loose
# substring grep falsely hits the import NAMES __cxa_rethrow /
# __cxa_rethrow_primary_exception, which are the legacy ABI, not EH opcodes.
n=$(wasm-tools print "$W" 2>/dev/null | rg -c '^\s+(try_table|catch_all|rethrow|delegate|throw_ref)\b')
echo "real EH opcode count = ${n:-0}  (0 = opcode-free legacy lowering)"
echo
echo "=== (C4) validate WITHOUT exceptions - expect PASS ==="
wasm-tools validate --features=all,-exceptions "$W" 2>&1 && echo "VALIDATE: PASS (rc=0)" || echo "VALIDATE: FAIL (rc=$?)"
echo
echo "=== (D1) IMPORTS - EH family + libc/WASI residual ==="
wasm-objdump -j Import -x "$W" 2>&1 | rg 'func\[|global\[|<-' | head -80
echo
echo "=== (D2) EXPORTS - C-API duckdb_* present ==="
wasm-objdump -j Export -x "$W" 2>&1 | rg -i 'duckdb_|-> "memory"|__indirect|malloc|free'
