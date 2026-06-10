#!/usr/bin/env python3
"""Shard genpkg/gen.go's fnNNN methods into N packages of free functions so
the engine compiles FULLY OPTIMIZED (Go compiler memory is per-package; the
monolithic gen.go OOMs at >50GB with optimizations on, hence genpkg's -N -l).

Validated full-scale 2026-06-10 (chain A): 42.5k methods -> core + 29 shards,
cross-shard calls through TBL_FnN func-vars registered by shard init()s.
Compiles in ~131s / 2.75GB peak. Historical note: this transform alone needed
'-l' on the shards (default inlining re-expanded the IR and OOMed at ~50GB);
since pipeline step 4d (scripts/split_giant_fns.py splits every >8k-line
function) '-l' is NO LONGER NEEDED — every package compiles fully optimized,
and '-c=1' is optional RAM bounding, not a requirement. Delivers 2.3-2.9x on
query workloads vs the -N -l genpkg.

Mechanics (each proven against a real debug cycle):
- Renames ALL top-level lowercase decls (funcs, vars, types, consts, Module
  fields, non-fnNNN methods) to exported form so shards can reach them.
- Collision-safe renames: if Upper(name) already exists anywhere in gen.go
  (e.g. field `memory` vs type alias `Memory`), or two sources map to the
  same target, the rename gets a `G1` suffix.
- fnNNN methods become free functions fnN(m *Module, ...); cross-shard calls
  go through `var TBL_FnN func(...)` declared in core and assigned by the
  owning shard's init(). Bare method values (table elements / callbacks)
  become receiver-binding closures so call_indirect type asserts still match.
- gen.dat is copied next to core.go (the //go:embed gen.dat directive).
IMPORTANT — input must be the PRE-INLINE gen.go (rebuild_fs_all.sh runs this
step 4c BEFORE the textual-inlining step 4b, on the freshly split transpile).
scripts/inline_helpers.py pre-expands helper calls at the SOURCE level, which
is the same IR expansion '-l' exists to prevent: shards generated from an
inlined gen.go blow past 28GB on the same '-l -c=1' flags (measured 2026-06-10
on shard20/Fn1308; not predictable by function size — a 20k-line pre-expanded
function elsewhere compiled, a 12.6k-line one did not). Textual inlining only
helps the -N -l genpkg build, where the compiler cannot inline; here the
optimizer compiles each small package with full optimization instead.

Output: converge/genopt/{core,shardNN,all}; importers use genopt/core for
Module/New plus a blank import of genopt/all (pulls in every shard init()).

Build recipe (rebuild_fs_all.sh step 4c2, GENOPT=1 — after step 4d, no '-l'):
  go build -tags genopt -p 1 \
           -gcflags='duckdbconverge/genopt/...=-c=1' ./duckdb/...
  (-p 1 / -c=1 only bound compile RAM; a plain 'go build -tags genopt' works)

Run from anywhere: python3 scripts/transform_genopt.py [K] [SHARD_SIZE]
GENOPT_SRC=<dir> overrides the input dir (default converge/genpkg); it must
contain a pre-inline gen.go + gen.dat.
"""
import math, re, os, shutil, sys

here = os.path.dirname(os.path.abspath(__file__))  # .../scripts
repo = os.path.dirname(here)
root = os.path.join(repo, 'converge')
srcdir = os.environ.get('GENOPT_SRC') or os.path.join(root, 'genpkg')
src = os.path.join(srcdir, 'gen.go')
dat = os.path.join(srcdir, 'gen.dat')
text = open(src).read()

# ---- rename pass: export every lower-case identifier shards could touch ----
toplevel_fns = set(re.findall(r'^func ([a-z]\w*)[\(\[]', text, re.M))
struct_m = re.search(r'type Module struct \{(.*?)\n\}', text, re.S)
fields = set(re.findall(r'^\t([a-z_]\w*)\s', struct_m.group(1), re.M)) if struct_m else set()
methods = set(re.findall(r'^func \(m \*Module\) ([a-z]\w*)\(', text, re.M))
tvars = set(re.findall(r'^var ([a-z]\w*)', text, re.M))
ttypes = set(re.findall(r'^type ([a-z]\w*)', text, re.M))
tconsts = set(re.findall(r'^const ([a-z]\w*)', text, re.M))
for b in re.findall(r'^const \(\n(.*?)\n\)', text, re.S | re.M):
    tconsts |= set(re.findall(r'^\t([a-z]\w*)\s*=', b, re.M))
shardable = {n for n in methods if re.fullmatch(r'fn\d+', n)}
core_methods = methods - shardable

sources = toplevel_fns | fields | methods | tvars | ttypes | tconsts
idents = set(re.findall(r'[A-Za-z_]\w*', text))
ren, taken = {}, set()
collisions = []
for n in sorted(sources):
    t = 'X' + n if n[0] == '_' else n[0].upper() + n[1:]
    if t in idents or t in taken:
        collisions.append((n, t))
        t += 'G1'
        assert t not in idents and t not in taken, f'rename collision unsolved: {n} -> {t}'
    ren[n] = t
    taken.add(t)
print(f'renamed: helpers={len(toplevel_fns)} fields={len(fields)} '
      f'core-methods={len(core_methods)} vars/types/consts={len(tvars | ttypes | tconsts)} '
      f'suffixed-collisions={collisions}')
# Single linear pass: tokenize identifiers, dict-lookup rename (a 44k-name
# alternation regex over 158MB is computationally infeasible). Verified: no
# rename key occurs inside any string literal of gen.go, so the token pass
# cannot corrupt runtime strings.
ident = re.compile(r'[A-Za-z_]\w*')
text = ident.sub(lambda g: ren.get(g.group(0), g.group(0)), text)

# ---- split: extract fnNNN methods, keep the rest as core (order preserved) ----
lines = text.split('\n')
sig_re = re.compile(r'^func \(m \*Module\) (Fn\d+)\(([^)]*)\)\s*([^{]*)\{')
core_out, funcs = [], []
i = 0
while i < len(lines):
    line = lines[i]
    m = sig_re.match(line)
    if m:
        depth = line.count('{') - line.count('}')
        body = [line]
        while depth > 0:
            i += 1
            body.append(lines[i])
            depth += lines[i].count('{') - lines[i].count('}')
        funcs.append((m.group(1), m.group(2), m.group(3).strip(), body))
    else:
        core_out.append(line)
    i += 1
print(f'sharded funcs={len(funcs)} core lines={len(core_out)}')

K = int(sys.argv[1]) if len(sys.argv) > 1 else len(funcs)
SHARD_SIZE = int(sys.argv[2]) if len(sys.argv) > 2 else 1500
N = max(1, math.ceil(K / SHARD_SIZE))
kept = funcs[:K]
shards = [kept[j::N] for j in range(N)]
shard_names = [{f[0] for f in s} for s in shards]

call0 = re.compile(r'\bm\.(Fn\d+)\(\)')
call = re.compile(r'\bm\.(Fn\d+)\(')
bare = re.compile(r'\bm\.(Fn\d+)\b(?!\()')
sig = {f[0]: (f[1], f[2]) for f in funcs}


def bind_closure(g):
    # m.FnX used as a method VALUE (table element / callback): wrap the TBL
    # func-var in a receiver-binding closure with the same receiver-less func
    # type — the driver's call_indirect type asserts keep working unchanged.
    name = g.group(1)
    params, rets = sig[name]
    names = [p.strip().split()[0] for p in params.split(',')] if params.strip() else []
    callargs = ('m, ' + ', '.join(names)) if names else 'm'
    inner = f'TBL_{name}({callargs})'
    bodytxt = f'return {inner}' if rets else inner
    return f'func({params}) {rets} {{ {bodytxt} }}'


def xform(body, local):
    out = []
    for ln in body:
        mm = sig_re.match(ln)
        if mm:
            p = mm.group(2)
            sep = ', ' if p.strip() else ''
            ln = f'func {mm.group(1)}(m *Module{sep}{p}) {mm.group(3)} {{'
        ln = call0.sub(lambda g: (f'{g.group(1)}(m)' if g.group(1) in local else f'TBL_{g.group(1)}(m)'), ln)
        ln = call.sub(lambda g: (f'{g.group(1)}(m, ' if g.group(1) in local else f'TBL_{g.group(1)}(m, '), ln)
        ln = bare.sub(bind_closure, ln)
        out.append(ln)
    return out


def sigvars(f):
    p = [x.strip() for x in f[1].split(',')] if f[1].strip() else []
    sep = ', ' if p else ''
    return p, sep


outdir = os.path.join(root, 'genopt')
shutil.rmtree(outdir, ignore_errors=True)  # stale shardNN dirs would break ./... builds
coredir = os.path.join(outdir, 'core')
os.makedirs(coredir, exist_ok=True)
shutil.copyfile(dat, os.path.join(coredir, 'gen.dat'))
with open(os.path.join(coredir, 'core.go'), 'w') as out:
    body = '\n'.join(core_out)
    body = call0.sub(lambda g: f'TBL_{g.group(1)}(m)', body)
    body = call.sub(lambda g: f'TBL_{g.group(1)}(m, ', body)
    body = bare.sub(bind_closure, body)
    pkg_done = False
    for ln in body.split('\n'):
        if not pkg_done and ln.startswith('package '):
            out.write('package core\n')
            pkg_done = True
            continue
        out.write(ln + '\n')
    for f in funcs:
        p, sep = sigvars(f)
        out.write(f'var TBL_{f[0]} func(m *Module{sep}{", ".join(p)}) {f[2]}\n')
    if funcs[K:]:
        out.write('func init() {\n')
        for f in funcs[K:]:
            p, sep = sigvars(f)
            out.write(f'\tTBL_{f[0]} = func(m *Module{sep}{", ".join(p)}) {f[2]} {{ panic("stub") }}\n')
        out.write('}\n')

for j, s in enumerate(shards):
    sdir = os.path.join(outdir, f'shard{j}')
    os.makedirs(sdir, exist_ok=True)
    with open(os.path.join(sdir, 'shard.go'), 'w') as out:
        out.write(f'// Code generated by genopt/transform.py. DO NOT EDIT.\npackage shard{j}\n\n')
        out.write('import (\n\t. "duckdbconverge/genopt/core"\n'
                  '\t"encoding/binary"\n\t"math"\n\t"math/bits"\n\t"unsafe"\n)\n\n')
        out.write('var _ = binary.LittleEndian\nvar _ = math.Pi\nvar _ = bits.UintSize\nvar _ unsafe.Pointer\n\n')
        for f in s:
            out.write('\n'.join(xform(f[3], shard_names[j])) + '\n')
        out.write('func init() {\n')
        for f in s:
            out.write(f'\tTBL_{f[0]} = {f[0]}\n')
        out.write('}\n')

alldir = os.path.join(outdir, 'all')
os.makedirs(alldir, exist_ok=True)
with open(os.path.join(alldir, 'all.go'), 'w') as out:
    out.write('// Code generated by genopt/transform.py. DO NOT EDIT.\n')
    out.write('// Blank-imports every shard so their init()s register the TBL_FnX\n')
    out.write('// func-vars in core; importers need only core + this package.\n')
    out.write('package all\n\nimport (\n')
    out.write('\t_ "duckdbconverge/genopt/core"\n')
    for j in range(N):
        out.write(f'\t_ "duckdbconverge/genopt/shard{j}"\n')
    out.write(')\n')
print(f'written: core + {N} shards + all (K={K}, shard sizes ~{len(shards[0])})')
