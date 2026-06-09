#!/usr/bin/env python3
# Post-process wasm2go output: the generated New() builds the function table as a
# single [][]any{{...30k entries...}} literal in ONE function, which overflows the
# Go compiler's liveness bitmap (internal compiler error: NewBulk too big).
# Fix: move the table fill into ~N/CHUNK small helper methods. Re-runnable.
import sys

CHUNK = 1000
path = sys.argv[1] if len(sys.argv) > 1 else "converge/genpkg/gen.go"

with open(path) as f:
    lines = f.readlines()

# Idempotency guard.
if any(l.startswith("func (m *Module) fillElems()") for l in lines):
    print("already split; nothing to do")
    sys.exit(0)

ei = next(i for i, l in enumerate(lines)
          if l.lstrip().startswith("m.elements = [][]any{{"))
stmt = lines[ei].strip()
inner = stmt[len("m.elements = [][]any{{"):]
inner = inner[:inner.rindex("}}")]
entries = [e.strip() for e in inner.split(",")]
N = len(entries)
print(f"elements line={ei+1} entries={N} chunk={CHUNK} helpers={(N+CHUNK-1)//CHUNK}")

# Replace the giant literal with an allocation + a fill call (runs before the
# subsequent copy(m.t0[...], m.elements[0]) that already follows in New()).
lines[ei] = f"\tm.elements = [][]any{{make([]any, {N})}}\n\tm.fillElems()\n"

with open(path, "w") as f:
    f.writelines(lines)

# Append the helper methods.
with open(path, "a") as f:
    calls = "\n".join(f"\tm.fe{k}()" for k in range((N + CHUNK - 1)//CHUNK))
    f.write(f"\nfunc (m *Module) fillElems() {{\n{calls}\n}}\n")
    for k, ci in enumerate(range(0, N, CHUNK)):
        f.write(f"\nfunc (m *Module) fe{k}() {{\n\te := m.elements[0]\n")
        for j in range(ci, min(ci + CHUNK, N)):
            f.write(f"\te[{j}] = {entries[j]}\n")
        f.write("}\n")
print("done")
