# Spike T1: C++ exceptions through wasm2go (the exception host)

Status: complete, verified (2026-06-08). This repo builds a standalone DuckDB-core wasm that
`wasm2go` transpiles to pure Go. DuckDB's error path is C++ exceptions, so the transpiled engine
needs a Go host that makes a C++ `catch` actually fire. Spike T1 proves that host works and is small.
Context: the parent analysis `googlesqlite-wasm2go-spike.md` and the runbook
`duckdb-purego-poc-runbook.md` (both in the chicory `faro-docs/design/` workspace).

## The question

`wasm2go` has ZERO support for the WASM exception-handling opcodes (`try_table`/`throw`/`catch`). So
the make-or-break question for the whole transpile route: can a C++ exception be caught at runtime
through wasm2go-generated Go WITHOUT native EH opcodes, and is the Go-side machinery a small shim or
a "second port" of the Emscripten exception runtime? An earlier research lane feared the latter (an
L-XL reimplementation of Emscripten's `libexceptions.js` in Go, no precedent). T1 tested it
empirically.

## The setup (opcode-free legacy exceptions)

Build the C++ with EMSCRIPTEN LEGACY exceptions, NOT native WASM EH. The legacy lowering produces
`invoke_*` trampolines + `__cxa_*` imports, which are ordinary CALLS, with ZERO EH opcodes:

```sh
# try.cc: extern "C" int try_it(int x) { try { risky(x); return 0; }
#         catch (const std::exception&) { return 1; } }   // risky() throws std::runtime_error for some x
emcc try.cc -O1 -sDISABLE_EXCEPTION_CATCHING=0 -sSTANDALONE_WASM --no-entry \
  -sEXPORTED_FUNCTIONS=_try_it -o try_legacy.wasm
```

Static proof it is opcode-free: `wasm-tools print try_legacy.wasm | rg -c 'try_table|catch_all|rethrow'`
= 0. The exception machinery is ordinary imports: `invoke_iii/ii/v/...`, `__cxa_throw`,
`__cxa_find_matching_catch_2/3`, `__cxa_begin_catch`, `__cxa_end_catch`, `__resumeException`,
`llvm_eh_typeid_for`. `setThrew` is exported (a module-internal flag at a fixed memory offset).

## The result: the catch FIRES through wasm2go-generated Go

```sh
wasm2go -pkg trymod -o try_gen.go try_legacy.wasm   # exit 0, NO "unsupported opcode"
```

The generated `Xenv` interface demands exactly a 13-method exception ABI. Implementing it in ~108 LOC
of Go (`host.go`) reproduces the Emscripten ABI:

- `invoke_*` trampolines: look up `table[index]`, call it under `recover()`.
- `__cxa_throw`: record the exception, `panic` to unwind back to the trampoline, which then calls the
  module's exported `setThrew(1, excPtr)`.
- `__cxa_find_matching_catch_N`: report the match and set `tempRet0`. CRUCIAL - the RTTI type-walk is
  NOT reimplemented in Go. It delegates to the module's EXPORTED `__cxa_can_catch` /
  `__dynamic_cast` / `__cxa_get_exception_ptr` (the standard Emscripten `findMatchingCatch`
  strategy). The host calls back into the module's own compiled libc++abi.
- `__cxa_begin_catch` / `__cxa_end_catch` / `__resumeException` / `llvm_eh_typeid_for` plus the
  `tempRet0`/`setThrew` plumbing.

Test results (`go test`, `CGO_ENABLED=0`):

```
try_it(3)=1, try_it(7)=1   (catch fired on throw)
try_it(4)=0, try_it(0)=0   (no spurious catch)
```

The ABI trace shows the full real path: `__cxa_throw(St13runtime_error)` -> `setThrew` ->
`find_matching_catch(St9exception)` -> `llvm_eh_typeid_for` -> matched -> `begin_catch` -> `what()`
via vtable -> `end_catch` -> return 1.

## Falsification (genuinely ABI-gated, not green-by-luck)

- `TestFalsify_NoThrewFlag`: suppress `setThrew` on the trampoline the throw unwinds through -> wasm
  reads threw==0, skips the landing pad, hits `panic("unreachable")`. Proves the catch is gated on the
  threw-flag.
- `TestFalsify_NoTypeMatch`: make `find_matching_catch` return a non-matching type id -> wasm takes
  `__resumeException` instead of the catch. Proves the catch is gated on the type-id compare.

So the green result genuinely depends on the threw-flag AND the module's real RTTI.

## Why it stays BOUNDED at DuckDB scope (S-M, not a second port)

The fear was an L-XL `libexceptions.js`-in-Go port. T1 shows why that is too pessimistic: the complex
part (multi-type RTTI matching) is DELEGATED to the wasm's own exported
`__cxa_can_catch`/`__dynamic_cast`, NOT reimplemented in Go. So the host is a single small Go file:

- the `invoke_*` trampolines are trivial (lookup + call + recover), one per arity, generatable from
  the import list,
- the `__cxa_*` core is the ~108-LOC pattern proven here,
- longjmp adds `_emscripten_throw_longjmp` via the same panic/recover,
- multi-type RTTI delegates to the module.

Estimate: a few hundred LOC of Go for DuckDB's full `__cxa_*`/`invoke_*` surface, mostly boilerplate.
Effort S-M, confidence HIGH.

## DuckDB-scale validation

`wasm2go` ingested the real 41 MB `duckdb-mvp.wasm` cleanly (exit 0, no unsupported-opcode), producing
327 MB / 8.44 M-line Go that parses (90,816 decls). Its generated `Xenv` demanded the IDENTICAL
13-symbol `__cxa_*`/eh runtime plus 344 `invoke_*` trampolines, and the module EXPORTS the driver
surface the host needs (`setThrew`, `__cxa_can_catch`, `__dynamic_cast`, ...). So the proven host
applies directly at DuckDB scope. (The standalone DuckDB-core wasm this repo builds is even larger,
256,946 functions, and likewise carries only the legacy `invoke_*`/`__cxa_*` exception surface, no EH
opcodes, no SIMD.)

## Implication

The exception wall - the original spike's core NO-GO concern - is DOWN for the wasm2go transpile
route. C++ exceptions through wasm2go are bounded engineering (a small Go host that delegates RTTI to
the module), proven end to end with falsification. Combined with spike T2 (the Go compiler survives
the large transpiled file), this is what makes the standalone-DuckDB-wasm -> wasm2go -> pure-Go path
viable on its two hardest axes. The remaining caveat is performance, not feasibility: a SIMD-free
transpiled engine is pure-Go DuckDB SEMANTICS, not DuckDB SPEED (see the runbook's perf section).

## Reproduce

Original probe artifacts: `/tmp/wasm2go-duckdb-probe/t1/cpp/` (`host.go`, `try_gen.go`, `try_test.go`,
`falsify_test.go`). The generalized, PoC-integrated host (full invoke arity matrix + the extended
`__cxa_*` surface, ready for the real DuckDB wasm) lives in the PoC harness `exhost/` package.
