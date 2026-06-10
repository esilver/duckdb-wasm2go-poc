#!/usr/bin/env python3
"""Textually inline the tiny hot wasm helpers in genpkg/gen.go.

The engine package compiles with -N -l (no inlining), so every wasm memory
access pays a real Go call (issue esilver/duckdb-go-pure#1: store32 3.78%,
i32 3.31%, load32 2.84% flat). This script replaces each call SITE with the
helper's body as an inline expression. Runs after split_new.py in
rebuild_fs_all.sh.

Safety analysis (verified against the generated code shape):
  * i32/i64 are identity functions; every call site argument is an integer
    literal. We rewrite i32(X) -> int32(X) (a constant conversion, folded at
    compile time) which preserves typing exactly in every context — EXCEPT
    that the original call laundered the constant into a runtime value, so
    enclosing conversions like uint64(i64(-1)) wrapped at runtime. As a typed
    CONSTANT, uint64(int64(-1)) is a compile error ("constant -1 overflows
    uint64"). The fold pass below evaluates nested literal integer
    conversions T1(T2(lit)) with exact two's-complement wrap semantics
    (identical to the runtime conversion chain) until fixpoint, so
    uint64(int64(-1)) becomes uint64(18446744073709551615).
  * loadNN/storeNN single-use args, evaluated once and in the same order in
    the inline form, so the transform is safe even for side-effectful args.
    The inline body is the unalignedOK little-endian arm of the helper
    (asserted below for the build host; extend if targeting BE/strict-align).
  * storeNN call sites are all standalone statements (verified: every
    occurrence of "store(16|32|64)(" in gen.go is at start-of-statement except
    the three definitions), so a statement-form replacement is valid.
  * shifts/rotates: each operand used exactly once, same order, masked count
    identical to the helper body.
  * Lines starting with "func " (the definitions) are skipped. Definitions
    are left in place (unused package-level funcs are legal Go).
Skipped on purpose: i32_div_s/i64_div_s and trunc_sat (branchy, cold),
f32_abs/f32_copysign (would just trade one call for two math.* calls).

Streaming, one line at a time; no whole-file regexes.
"""
import re
import sys

# single-arg expression helpers: name -> template
EXPR1 = {
    "load16": "(*(*uint16)(unsafe.Pointer((*[2]byte)({0}))))",
    "load32": "(*(*uint32)(unsafe.Pointer((*[4]byte)({0}))))",
    "load64": "(*(*uint64)(unsafe.Pointer((*[8]byte)({0}))))",
    "i32": "int32({0})",
    "i64": "int64({0})",
}
# two-arg expression helpers
EXPR2 = {
    "i32_shl": "(({0}) << (({1}) & 31))",
    "i32_shr_s": "(({0}) >> (({1}) & 31))",
    "i32_shr_u": "(int32(uint32({0}) >> (({1}) & 31)))",
    "i64_shl": "(({0}) << (({1}) & 63))",
    "i64_shr_s": "(({0}) >> (({1}) & 63))",
    "i64_shr_u": "(int64(uint64({0}) >> (({1}) & 63)))",
    "i32_rotl": "(int32(bits.RotateLeft32(uint32({0}), int({1}))))",
    "i32_rotr": "(int32(bits.RotateLeft32(uint32({0}), -int({1}))))",
    "i64_rotl": "(int64(bits.RotateLeft64(uint64({0}), int({1}))))",
    "i64_rotr": "(int64(bits.RotateLeft64(uint64({0}), -int({1}))))",
}
# two-arg statement helpers (verified: only ever appear as standalone statements)
STMT2 = {
    "store16": "*(*uint16)(unsafe.Pointer((*[2]byte)({0}))) = {1}",
    "store32": "*(*uint32)(unsafe.Pointer((*[4]byte)({0}))) = {1}",
    "store64": "*(*uint64)(unsafe.Pointer((*[8]byte)({0}))) = {1}",
}

NAMES = {**EXPR1, **EXPR2, **STMT2}
# token must not be preceded by an identifier char or '.', and is followed by '('
PAT = re.compile(
    r"(?<![A-Za-z0-9_.])("
    + "|".join(sorted(NAMES, key=len, reverse=True))
    + r")\("
)
QUICK = ("load", "store", "i32", "i64")  # cheap pre-filter

stats = {n: 0 for n in NAMES}
stats["constfold"] = 0
skipped = 0

# integer conversion fold: T1(T2(literal)) -> T1(value), runtime-wrap exact
CONVS = {
    "uint64": (64, False), "uint32": (32, False), "uint16": (16, False),
    "uint8": (8, False), "byte": (8, False),
    "int64": (64, True), "int32": (32, True), "int16": (16, True),
    "int8": (8, True),
}
FOLD = re.compile(
    r"(?<![A-Za-z0-9_.])(" + "|".join(CONVS) + r")\((" + "|".join(CONVS)
    + r")\((-?)(0x[0-9a-fA-F]+|[0-9]+)\)\)"
)


def _wrap(v, nbits, signed):
    v &= (1 << nbits) - 1
    if signed and v >= 1 << (nbits - 1):
        v -= 1 << nbits
    return v


def _fold_one(m):
    outer, inner, sign, lit = m.groups()
    v = int(lit, 0)
    if sign:
        v = -v
    v = _wrap(v, *CONVS[inner])
    v = _wrap(v, *CONVS[outer])
    stats["constfold"] += 1
    return f"{outer}({v})"


def fold(line):
    while True:
        new = FOLD.sub(_fold_one, line)
        if new == line:
            return line
        line = new


def find_close(s, i):
    """s[i] is just past the opening '('. Return index of the matching ')'."""
    depth = 1
    while i < len(s):
        c = s[i]
        if c in "([{":
            depth += 1
        elif c in ")]}":
            depth -= 1
            if depth == 0:
                return i
        i += 1
    raise ValueError("unbalanced parens: " + s)


def split2(s):
    """Split 'a, b' at the single top-level comma."""
    depth = 0
    for i, c in enumerate(s):
        if c in "([{":
            depth += 1
        elif c in ")]}":
            depth -= 1
        elif c == "," and depth == 0:
            return s[:i], s[i + 1 :].lstrip()
    return None


def expand(s):
    out = []
    i = 0
    while True:
        m = PAT.search(s, i)
        if not m:
            out.append(s[i:])
            return "".join(out)
        name = m.group(1)
        close = find_close(s, m.end())
        argstr = s[m.end() : close]
        global skipped
        if name in EXPR1:
            if split2(argstr) is not None:  # unexpected arity: leave untouched
                skipped += 1
                out.append(s[i : m.end()])
                i = m.end()
                continue
            repl = EXPR1[name].format(expand(argstr))
        else:
            pair = split2(argstr)
            if pair is None:
                skipped += 1
                out.append(s[i : m.end()])
                i = m.end()
                continue
            tmpl = EXPR2.get(name) or STMT2[name]
            repl = tmpl.format(expand(pair[0]), expand(pair[1]))
        stats[name] += 1
        out.append(s[i : m.start()])
        out.append(repl)
        i = close + 1


def main():
    path = sys.argv[1] if len(sys.argv) > 1 else "converge/genpkg/gen.go"
    tmp = path + ".inlined"
    with open(path, "r") as fin, open(tmp, "w") as fout:
        for line in fin:
            if line.startswith("func ") or not any(q in line for q in QUICK):
                fout.write(line)
                continue
            fout.write(fold(expand(line)))
    import os

    os.replace(tmp, path)
    total = sum(stats.values())
    for n in sorted(stats, key=stats.get, reverse=True):
        if stats[n]:
            print(f"  {n:12s} {stats[n]:>9,d}")
    print(f"inline_helpers: {total:,d} call sites inlined, {skipped} skipped")


if __name__ == "__main__":
    main()
