#!/usr/bin/env python3
"""Split oversized wasm2go-transpiled functions so the Go compiler accepts them.

Why: the transpiled DuckDB engine contains functions up to 36.5k lines
(shard27.Fn6568). The Go compiler hard-caps functions at 65536 SSA blocks —
Fn6568 exceeds it even at -N -l, which bricks every GOOS=js/wasm build.
Oversized functions are also why the genopt shards need '-l' (inliner IR
explosion >50GB). This transform splits any function over a line threshold
into semantically identical part-functions.

Shape of the input (verified by census on Fn6568, 2026-06-10):
- ALL control flow is goto-based: zero `for` loops; nested bare `{}` blocks
  (wasm block scopes), `if cond {` bodies (wasm if, NO else anywhere), and
  atomic `switch expr { case N: goto lX ... }` dispatch (br_table). Labels
  `lN:` appear in bare blocks and inside if-bodies, never under a switch.
- Every declared name is unique (no shadowing). `tN`/`TN` := temporaries
  never live across a label; `var pN T` / the top `var vN... T` block do.

Transform (per oversized function):
1. Parse the body into a tree; assert the grammar above (skip + warn loudly
   on any violation — correctness over coverage).
2. Flatten: splice bare blocks; an `if cond {B}` whose B contains a label
   becomes `if !(cond) { goto xlK }` + flatten(B) + `xlK:`. Label-free ifs
   and switches stay atomic (their internal goto LINES are still rewritten).
3. Hoist params + all var-declared locals into `type <fn>Locals struct`
   (refs rewritten to l.<name>); `:=` temporaries stay local — each label
   segment is wrapped in a labeled bare block `lN: { ... }`, which scopes
   them so top-level gotos never jump over a declaration.
4. Greedy-pack consecutive segments into part funcs
   `func <fn>_part_K(m *Module, l *<fn>Locals, entry int) int` with an
   entry-dispatch switch (goto into the labeled blocks). Segment label ids
   are their textual order, so parts own contiguous code ranges. A
   `goto lX` whose target lives in another part becomes `return <id(lX)>`;
   the original `return E` becomes `l.ret = E; return -1`; falling off the
   end of a part returns the next part's first id. The replacement <fn> is
   a driver: init struct, loop `next = <fn>_part_K(m, &l, next)` selected
   by range compare, `next < 0` -> `return l.ret`.

Usage: split_giant_fns.py FILE [FILE...] [--threshold N] [--max-part N]
Rewrites FILE in place. Exit 1 if an oversized function could not be split.
"""

import re
import sys

sys.setrecursionlimit(200000)  # transpiled nesting reaches >1000 blocks (shard25)

THRESHOLD = 25000   # only split functions over this many body lines
MAX_PART = 11000    # target max lines per part function

RE_LABEL = re.compile(r'^(l\d+):$')
RE_GOTO = re.compile(r'^goto (l\d+|xl\d+)$')
RE_IF = re.compile(r'^if (.*) \{$')
RE_SWITCH = re.compile(r'^switch .*\{$')
RE_VAR = re.compile(r'^var ([A-Za-z_]\w*(?:, [A-Za-z_]\w*)*) ([\w.\[\]*]+)$')
RE_FUNC = re.compile(r'^func ([A-Za-z_]\w*)\(([^)]*)\)\s*([^{]*)\{$')
RE_RETURN = re.compile(r'^return\b\s*(.*)$')


class Node:
    __slots__ = ('kind', 'cond', 'children', 'orelse', 'lines')

    def __init__(self, kind, cond=None, children=None, orelse=None, lines=None):
        self.kind = kind          # 'bare' | 'if' | 'leaf'
        self.cond = cond          # if-condition text for 'if'
        self.children = children  # for 'bare'/'if'
        self.orelse = orelse      # else-branch children for 'if' (or None)
        self.lines = lines        # raw stripped lines for 'leaf' (switch/stmt)


class GrammarError(Exception):
    pass


def parse_block(lines, i):
    """Parse statements until the closing '}' at this level.
    Returns (children, next_i, had_else) — had_else when terminated by '} else {'."""
    children = []
    n = len(lines)
    while i < n:
        l = lines[i].strip()
        if l == '}':
            return children, i + 1, False
        if l == '} else {':
            return children, i + 1, True
        if l == '{':
            ch, i, he = parse_block(lines, i + 1)
            if he:
                raise GrammarError('else terminating bare block')
            children.append(Node('bare', children=ch))
            continue
        m = RE_IF.match(l)
        if m:
            ch, i, he = parse_block(lines, i + 1)
            orelse = None
            if he:
                orelse, i, he2 = parse_block(lines, i)
                if he2:
                    raise GrammarError('else-if chain')
            children.append(Node('if', cond=m.group(1), children=ch, orelse=orelse))
            continue
        if RE_SWITCH.match(l):
            # atomic: consume to matching brace, keep raw lines
            raw = [l]
            depth = 1
            i += 1
            while depth > 0:
                s = lines[i].strip()
                depth += s.count('{') - s.count('}')
                raw.append(s)
                if RE_LABEL.match(s):
                    raise GrammarError('label inside switch')
                i += 1
            children.append(Node('leaf', lines=raw))
            continue
        if l.count('{') != l.count('}'):
            raise GrammarError('unbalanced braces in statement: %r' % l)
        children.append(Node('leaf', lines=[l]))
        i += 1
    raise GrammarError('unexpected EOF in block')


def has_label(node):
    if node.kind == 'leaf':
        return bool(RE_LABEL.match(node.lines[0]))
    if any(has_label(c) for c in node.children):
        return True
    return bool(node.kind == 'if' and node.orelse and any(has_label(c) for c in node.orelse))


def flatten(children, fresh):
    """Yield flat items: ('label', name) | ('stmt', [lines]). fresh = counter list."""
    out = []
    for node in children:
        if node.kind == 'leaf':
            m = RE_LABEL.match(node.lines[0])
            if m:
                out.append(('label', m.group(1)))
            else:
                out.append(('stmt', node.lines))
        elif node.kind == 'bare':
            out.extend(flatten(node.children, fresh))
        elif node.kind == 'if':
            if has_label(node):
                fresh[0] += 1
                skip = 'xl%d' % fresh[0]
                if node.orelse is None:
                    out.append(('stmt', ['if !(%s) {' % node.cond, 'goto %s' % skip, '}']))
                    out.extend(flatten(node.children, fresh))
                    out.append(('label', skip))
                else:
                    fresh[0] += 1
                    end = 'xl%d' % fresh[0]
                    out.append(('stmt', ['if !(%s) {' % node.cond, 'goto %s' % skip, '}']))
                    out.extend(flatten(node.children, fresh))
                    out.append(('stmt', ['goto %s' % end]))
                    out.append(('label', skip))
                    out.extend(flatten(node.orelse, fresh))
                    out.append(('label', end))
            else:
                out.append(('stmt', render_atomic(node)))
    return out


def render_atomic(node):
    """Re-render a label-free if subtree back to raw lines."""
    if node.kind == 'leaf':
        return list(node.lines)
    lines = []
    if node.kind == 'if':
        lines.append('if %s {' % node.cond)
    else:
        lines.append('{')
    for c in node.children:
        lines.extend(render_atomic(c))
    if node.kind == 'if' and node.orelse is not None:
        lines.append('} else {')
        for c in node.orelse:
            lines.extend(render_atomic(c))
    lines.append('}')
    return lines


def collect_vars(children, decls):
    """Pull `var names type` decls out of the tree (anywhere). Mutates tree."""
    kept = []
    for node in children:
        if node.kind == 'leaf' and node.lines[0].startswith('var '):
            m = RE_VAR.match(node.lines[0])
            if not m:
                raise GrammarError('unparsable var decl: %r' % node.lines[0])
            for name in m.group(1).split(', '):
                decls.append((name, m.group(2)))
            continue
        if node.kind in ('bare', 'if'):
            node.children = collect_vars(node.children, decls)
            if node.kind == 'if' and node.orelse is not None:
                node.orelse = collect_vars(node.orelse, decls)
        kept.append(node)
    return kept


def split_function(func_lines, fname, params, rettype):
    """params: [(name,type)] excluding the Module receiver param. Returns new source lines."""
    body = func_lines[1:-1]  # strip 'func ...{' and final '}'

    tree, end, had_else = parse_block(body + ['}'], 0)
    if end != len(body) + 1 or had_else:
        raise GrammarError('trailing content after body')

    decls = list(params)
    tree = collect_vars(tree, decls)
    fresh = [0]
    flat = flatten(tree, fresh)

    voidfn = not rettype

    # ---- segment at labels; ids are textual order ----
    segments = []  # (label_name, [stmt-lines...]) ; entry uses synthetic label
    cur_label, cur = 'xlentry', []
    for kind, val in flat:
        if kind == 'label':
            segments.append((cur_label, cur))
            cur_label, cur = val, []
        else:
            cur.extend(val)
    segments.append((cur_label, cur))
    seg_id = {lab: i for i, (lab, _) in enumerate(segments)}
    if len(seg_id) != len(segments):
        raise GrammarError('duplicate label')

    # A `name := ...` temporary referenced from another segment (each segment
    # becomes its own block scope) must be hoisted into the locals struct,
    # which needs its type: infer from the stereotyped RHS shapes (cast
    # prefix, known var, temp copy chain). Anything else fails loudly.
    def_seg = {}
    def_rhs = {}
    re_def = re.compile(r'^([A-Za-z_]\w*) := (.*)$')
    re_ident = re.compile(r'(?<![\w.])([A-Za-z_]\w*)\b')
    for i, (_, stmts) in enumerate(segments):
        for line in stmts:
            d = re_def.match(line)
            if d:
                def_seg[d.group(1)] = i
                def_rhs[d.group(1)] = d.group(2)
    crossing = set()
    for i, (_, stmts) in enumerate(segments):
        for line in stmts:
            rest = line.split(':=', 1)[-1]
            for ident in re_ident.findall(rest):
                if ident in def_seg and def_seg[ident] != i:
                    crossing.add(ident)

    decl_type = dict(decls)
    CASTS = {'int32(': 'int32', 'uint32(': 'uint32', 'int64(': 'int64',
             'uint64(': 'uint64', 'float32(': 'float32', 'float64(': 'float64',
             'I32(': 'int32', 'I64(': 'int64', 'F32(': 'float32', 'F64(': 'float64'}

    def infer_type(name, seen):
        if name in seen:
            raise GrammarError('type inference cycle on %s' % name)
        seen.add(name)
        rhs = def_rhs.get(name)
        if rhs is None:
            raise GrammarError('no RHS for crossing temporary %s' % name)
        if rhs in decl_type:
            return decl_type[rhs]
        if rhs == 'm.G0':
            return 'int32'
        for pfx, t in CASTS.items():
            if rhs.startswith(pfx):
                return t
        if re.fullmatch(r'[A-Za-z_]\w*', rhs):
            return infer_type(rhs, seen)
        raise GrammarError('cannot infer type of crossing temporary %s := %s' % (name, rhs))

    for name in sorted(crossing):
        decls.append((name, infer_type(name, set())))

    # ---- hoist declared vars (and crossing temps) into the locals struct ----
    names = [n for n, _ in decls]
    if not names:
        raise GrammarError('no locals found')
    # (?<![\w.]) keeps m.T0 / m.G0-style Module field accesses out of the match
    hoist_re = re.compile(r'(?<![\w.])(%s)\b' % '|'.join(sorted(names, key=len, reverse=True)))
    re_decl_fix = re.compile(r'^(l\.\w+) := ')

    def rewrite_refs(line):
        if '"' in line and 'unreachable' not in line and 'panic' not in line:
            raise GrammarError('string literal in stmt: %r' % line)
        line = hoist_re.sub(lambda m: 'l.' + m.group(1), line)
        # hoisted temp's definition `l.tN := x` must become plain assignment
        return re_decl_fix.sub(r'\1 = ', line)

    # ---- greedy partition into parts ----
    parts = []  # list of [seg_index...]
    cur_part, cur_lines = [], 0
    for i, (_, stmts) in enumerate(segments):
        if cur_part and cur_lines + len(stmts) > MAX_PART:
            parts.append(cur_part)
            cur_part, cur_lines = [], 0
        cur_part.append(i)
        cur_lines += len(stmts)
    parts.append(cur_part)

    part_of = {}
    for pi, segs in enumerate(parts):
        for s in segs:
            part_of[s] = pi

    lname = fname[0].lower() + fname[1:] + 'Locals'
    out = []

    # ---- locals struct ----
    out.append('type %s struct {' % lname)
    for n, t in decls:
        out.append('\t%s %s' % (n, t))
    if not voidfn:
        out.append('\tret %s' % rettype)
    out.append('}')
    out.append('')

    # ---- driver ----
    sig_params = ', '.join('%s %s' % (n, t) for n, t in params)
    head = 'func %s(m *Module%s) %s {' % (
        fname, (', ' + sig_params) if sig_params else '', rettype)
    out.append(head.replace('  ', ' ').rstrip())
    if params:
        out.append('\tl := %s{%s}' % (lname, ', '.join('%s: %s' % (n, n) for n, _ in params)))
    else:
        out.append('\tvar l %s' % lname)
    out.append('\tnext := 0')
    out.append('\tfor {')
    out.append('\t\tif next < 0 {')
    out.append('\t\t\treturn l.ret' if not voidfn else '\t\t\treturn')
    out.append('\t\t}')
    for pi, segs in enumerate(parts[:-1]):
        out.append('\t\tif next < %d {' % (parts[pi + 1][0]))
        out.append('\t\t\tnext = %s_part_%d(m, &l, next)' % (fname, pi))
        out.append('\t\t\tcontinue')
        out.append('\t\t}')
    out.append('\t\tnext = %s_part_%d(m, &l, next)' % (fname, len(parts) - 1))
    out.append('\t}')
    out.append('}')
    out.append('')

    # ---- parts ----
    for pi, segs in enumerate(parts):
        in_part = {segments[s][0] for s in segs}
        next_code = parts[pi + 1][0] if pi + 1 < len(parts) else -2

        def emit_stmt_line(line, acc):
            g = RE_GOTO.match(line)
            if g:
                tgt = g.group(1)
                if tgt in in_part:
                    acc.append('goto ' + tgt)
                else:
                    acc.append('return %d' % seg_id[tgt])
                return
            r = RE_RETURN.match(line)
            if r:
                if voidfn:
                    acc.append('return -1')
                else:
                    acc.append('l.ret = %s' % rewrite_refs(r.group(1)))
                    acc.append('return -1')
                return
            acc.append(rewrite_refs(line))

        out.append('func %s_part_%d(m *Module, l *%s, entry int) int {' % (fname, pi, lname))
        out.append('\tswitch entry {')
        for s in segs:
            out.append('\tcase %d:' % s)
            out.append('\t\tgoto %s' % segments[s][0])
        out.append('\t}')
        out.append('\tpanic("bad entry")')
        for s in segs:
            lab, stmts = segments[s]
            out.append('%s:' % lab)
            out.append('\t{')
            acc = []
            for line in stmts:
                emit_stmt_line(line, acc)
            for line in acc:
                out.append('\t\t' + line)  # indentation is cosmetic; compiler ignores
            out.append('\t}')
        if next_code == -2:
            out.append('\tpanic("unreachable")')
        else:
            out.append('\treturn %d' % next_code)
        out.append('}')
        out.append('')

    return out


def process_file(path):
    src = open(path).read()
    lines = src.split('\n')
    out = []
    i = 0
    n = len(lines)
    split_count = 0
    failures = []
    while i < n:
        line = lines[i]
        m = RE_FUNC.match(line)
        if not m:
            out.append(line)
            i += 1
            continue
        # find end of function (next '^}' at col 0)
        j = i + 1
        while j < n and lines[j] != '}':
            j += 1
        body_len = j - i
        if body_len <= THRESHOLD:
            out.extend(lines[i:j + 1])
            i = j + 1
            continue
        fname = m.group(1)
        rettype = m.group(3).strip()
        raw_params = [p.strip() for p in m.group(2).split(',')]
        # expand grouped params: "m *Module, v0, v1 int32" style
        params = []
        pending = []
        for p in raw_params:
            bits = p.split()
            if len(bits) == 1:
                pending.append(bits[0])
            else:
                pending.append(bits[0])
                ptype = ' '.join(bits[1:])
                for name in pending:
                    params.append((name, ptype))
                pending = []
        if pending:
            raise GrammarError('%s: unparsable params' % fname)
        if not params or params[0] != ('m', '*Module'):
            print('SKIP %s: first param not (m *Module)' % fname, file=sys.stderr)
            out.extend(lines[i:j + 1])
            i = j + 1
            continue
        try:
            new = split_function(lines[i:j + 1], fname, params[1:], rettype)
        except GrammarError as e:
            failures.append('%s: %s' % (fname, e))
            out.extend(lines[i:j + 1])
            i = j + 1
            continue
        print('split %s: %d lines -> %d parts' % (fname, body_len, len([
            l for l in new if l.startswith('func %s_part_' % fname)])), file=sys.stderr)
        out.extend(new)
        split_count += 1
        i = j + 1
    if split_count or failures:
        open(path, 'w').write('\n'.join(out))
    for f in failures:
        print('FAILED %s' % f, file=sys.stderr)
    return split_count, failures


def main():
    global THRESHOLD, MAX_PART
    args = []
    it = iter(sys.argv[1:])
    for a in it:
        if a == '--threshold':
            THRESHOLD = int(next(it))
        elif a == '--max-part':
            MAX_PART = int(next(it))
        else:
            args.append(a)
    total, bad = 0, []
    for path in args:
        c, f = process_file(path)
        total += c
        bad.extend(f)
    print('split %d function(s), %d failure(s)' % (total, len(bad)), file=sys.stderr)
    sys.exit(1 if bad else 0)


if __name__ == '__main__':
    main()
