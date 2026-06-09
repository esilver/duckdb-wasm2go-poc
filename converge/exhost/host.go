// Package exhost is a reusable Go implementation of the Emscripten "legacy"
// (opcode-free) C++ exception ABI that a wasm2go-transpiled module imports from
// "env". It generalizes the ~108-LOC host proven in probe T1
// (/tmp/wasm2go-duckdb-probe/t1/cpp/host.go) to the full surface a real
// DuckDB -fexceptions build needs.
//
// Division of labor (the same one T1 proved):
//   - The compiled C++ wraps every potentially-throwing INDIRECT call in an
//     invoke_<sig> trampoline that is a HOST function. The trampoline performs
//     the indirect call; if a C++ exception propagates it records the "threw"
//     flag (via the module's exported setThrew) and returns a zero value so the
//     wasm landing pad runs.
//   - __cxa_throw is a HOST function. Natively it longjmps to the active
//     invoke_ trampoline; here, since both the throw and the trampoline are Go,
//     we unwind with panic/recover.
//   - RTTI (__cxa_can_catch / __dynamic_cast / __cxa_get_exception_ptr) is NOT
//     reimplemented in Go. It is DELEGATED to the module's own EXPORTED copies,
//     reached through the ModuleABI interface. __cxa_find_matching_catch_N here
//     just hands back the in-flight exception pointer and publishes the matched
//     type id into tempRet0, exactly as Emscripten's JS glue does.
//
// Reuse: the exception-state machine, the invoke trampoline recover pattern,
// __cxa_throw/find_matching_catch_2,3/begin_catch/end_catch/resumeException,
// llvm_eh_typeid_for and the tempRet0 plumbing are lifted from T1. New here:
// the full invoke arity matrix (invokes.go, generated), the find_matching_catch
// 4/5 variants, rethrow/primary-exception/uncaught-exceptions, the longjmp
// import, getTempRet0/setTempRet0 imports, and RTTI delegation through an
// interface instead of a concrete *Module.
package exhost

import (
	"fmt"
	"os"
	"sync"
)

// ModuleABI is the slice of the wasm2go-generated *Module that the exception
// host drives. The run harness adapts the concrete generated *Module to this
// interface (see the adapter in the main package) so this package stays
// independent of any particular generated package.
//
// Every method here is a wasm EXPORT of an emcc -fexceptions build:
//
//	setThrew, _emscripten_tempret_set, __indirect_function_table  (always)
//	__cxa_can_catch, __cxa_get_exception_ptr                      (RTTI)
//	__dynamic_cast                                                (RTTI, when present)
type ModuleABI interface {
	// SetThrew is the module's exported setThrew(threw, value). The landing
	// pad reads the flag it sets to decide whether to run catch logic.
	SetThrew(threw, value int32)
	// TempretSet is the module's exported _emscripten_tempret_set(v). Used to
	// publish the matched type id that the wasm compares with llvm_eh_typeid_for.
	TempretSet(v int32)
	// Table is the module's exported __indirect_function_table contents. The
	// invoke_ trampolines index into it to perform the wrapped indirect call.
	Table() []any

	// CanCatch delegates to the module's exported __cxa_can_catch(catchType,
	// excType, adjustedPtrSlot). Returns nonzero if excType is catchable as
	// catchType; reads the object pointer from *adjustedPtrSlot and writes the
	// (possibly base-class-adjusted) pointer back there on a match.
	CanCatch(catchType, excType, adjustedPtrSlot int32) int32
	// GetExceptionPtr delegates to the module's exported
	// __cxa_get_exception_ptr(excHeader) -> object pointer.
	GetExceptionPtr(excHeader int32) int32
	// DynamicCast delegates to the module's exported __dynamic_cast. May be a
	// no-op (return 0) if the build does not export it; the simple
	// single-inheritance catch paths DuckDB needs route through CanCatch.
	DynamicCast(obj, srcType, dstType, offset int32) int32

	// Malloc/Free are the module's exported allocator, used to obtain a scratch
	// pointer-slot for __cxa_can_catch's adjusted-object-pointer in/out arg.
	Malloc(n int32) int32
	Free(ptr int32)
	// ReadU32/WriteU32 access module linear memory (for the scratch slot).
	ReadU32(ptr int32) int32
	WriteU32(ptr, v int32)
}

// Host implements the env-imported exception ABI. Construct with New, register
// the module via Bind (called from the module's Init hook), then pass *Host as
// the Xenv to the generated New().
type Host struct {
	mu  sync.Mutex
	abi ModuleABI

	// binder lets Init(any) build the ModuleABI from the concrete *Module the
	// generated New() hands back, without this package importing that package.
	binder func(mod any) ModuleABI

	// tempRet0 mirrors Emscripten's getTempRet0/setTempRet0 register for the
	// builds that import them (most route through the exported tempret_set
	// instead, which writes the module's own register, but we keep a mirror so
	// a get import has something coherent to return).
	tempRet0 int32

	// inflight is the exception currently unwinding: object header pointer +
	// its std::type_info pointer, captured by __cxa_throw. A small stack of
	// these supports rethrow / nested catch.
	inflight []excRecord

	// typeIDs assigns a stable nonzero id per std::type_info pointer so that
	// find_matching_catch and llvm_eh_typeid_for agree for the same type.
	typeIDs  map[int32]int32
	nextType int32

	// uncaught counts exceptions thrown but not yet caught, for
	// __cxa_uncaught_exceptions().
	uncaught int32

	// Trace, when true, records the ABI call order for tests/debugging.
	Trace bool
	log   []string

	// lastThrowMsg is the what() message of the most recently thrown exception,
	// decoded from the object at __cxa_throw time. DuckDB's convert-and-rethrow
	// error path loses the message from the C-API result (duckdb_result_error
	// returns null); the driver falls back to this so failed queries still report
	// why. Best-effort: assumes the std::runtime_error (libc++) layout.
	lastThrowMsg string
}

// LastThrowMessage returns the message of the most recently thrown C++ exception
// (best-effort; std::runtime_error-derived, which includes all duckdb::Exception).
func (h *Host) LastThrowMessage() string { return h.lastThrowMsg }

type excRecord struct {
	exc int32 // exception object header pointer (what __cxa_throw was given)
	typ int32 // std::type_info pointer
}

// thrownPanic is the sentinel panicked from __cxa_throw / __resumeException to
// unwind the Go stack back to the active invoke_ trampoline.
type thrownPanic struct{ exc, typ int32 }

// New returns a Host. binder converts the generated *Module (received in the
// Init hook) into a ModuleABI. Pass nil only in tests that set abi by hand.
func New(binder func(mod any) ModuleABI) *Host {
	return &Host{
		binder:   binder,
		typeIDs:  map[int32]int32{},
		nextType: 1,
	}
}

// Init is the hook wasm2go's New() invokes, handing us the concrete *Module.
func (h *Host) Init(mod any) {
	if h.binder != nil {
		h.abi = h.binder(mod)
	}
}

// SetABI lets tests inject a ModuleABI directly (bypassing the Init hook).
func (h *Host) SetABI(a ModuleABI) { h.abi = a }

// Log returns the recorded ABI trace (only populated when Trace is true).
func (h *Host) Log() []string { return h.log }

func (h *Host) logf(f string, a ...any) {
	if h.Trace {
		h.log = append(h.log, fmt.Sprintf(f, a...))
	}
}

func (h *Host) table() []any { return h.abi.Table() }

// DebugThrow, when set, logs every __cxa_throw's C++ type_info name to stderr.
var DebugThrow = false

// cstrU32 reads a NUL-terminated C string from module memory 4 bytes at a time
// (diagnostic only; used to decode std::type_info names).
func (h *Host) cstrU32(ptr int32) string {
	if ptr == 0 {
		return ""
	}
	var b []byte
	for i := int32(0); i < 512; i += 4 {
		w := uint32(h.abi.ReadU32(ptr + i))
		for s := 0; s < 4; s++ {
			c := byte(w >> (8 * s))
			if c == 0 {
				return string(b)
			}
			b = append(b, c)
		}
	}
	return string(b)
}

// typeID maps a std::type_info pointer to a stable nonzero id. A zero typeinfo
// (the "catch (...)" / unknown case) collapses to id 1 so a catch-all matches.
func (h *Host) typeID(typeinfo int32) int32 {
	if typeinfo == 0 {
		return 1
	}
	if id, ok := h.typeIDs[typeinfo]; ok {
		return id
	}
	id := h.nextType
	h.nextType++
	h.typeIDs[typeinfo] = id
	return id
}

func (h *Host) top() (excRecord, bool) {
	if len(h.inflight) == 0 {
		return excRecord{}, false
	}
	return h.inflight[len(h.inflight)-1], true
}

// trampoline runs do() (the wrapped indirect call) and, on a thrown C++
// exception, sets the module threw flag and reports threw=true. Non-throw Go
// panics propagate unchanged. This is the recover pattern proven in T1,
// factored out so every invoke_ arity shares it.
func (h *Host) trampoline(do func()) {
	defer func() {
		if r := recover(); r != nil {
			tp, ok := r.(thrownPanic)
			if !ok {
				panic(r)
			}
			h.logf("invoke caught throw exc=%d typ=%d -> setThrew(1,%d)", tp.exc, tp.typ, tp.exc)
			h.abi.SetThrew(1, tp.exc)
		}
	}()
	do()
}

// ---- C++ exception ABI (non-invoke) ---------------------------------------

// X__cxa_throw(ptr, typeinfo, dtor): record the in-flight exception and unwind
// to the active invoke_ trampoline via panic. (T1)
func (h *Host) X__cxa_throw(ptr, typ, dtor int32) {
	h.logf("__cxa_throw ptr=%d typ=%d dtor=%d", ptr, typ, dtor)
	// Capture the what() message (libc++ runtime_error refstring char* at +4) so the
	// driver can recover it when DuckDB's convert-and-rethrow loses it from the
	// C-API result.
	h.lastThrowMsg = h.cstrU32(h.abi.ReadU32(ptr + 4))
	if DebugThrow {
		name := ""
		if typ != 0 {
			name = h.cstrU32(h.abi.ReadU32(typ + 4)) // type_info::__name (Itanium, wasm32)
		}
		fmt.Fprintf(os.Stderr, "[exhost] __cxa_throw type=%q msg=%q\n", name, h.lastThrowMsg)
	}
	h.inflight = append(h.inflight, excRecord{exc: ptr, typ: typ})
	h.uncaught++
	panic(thrownPanic{exc: ptr, typ: typ})
}

// X__cxa_rethrow re-raises the exception at the top of the catch stack.
func (h *Host) X__cxa_rethrow() {
	rec, ok := h.top()
	if !ok {
		h.logf("__cxa_rethrow with no in-flight exception")
		panic(thrownPanic{})
	}
	h.logf("__cxa_rethrow exc=%d typ=%d", rec.exc, rec.typ)
	h.uncaught++
	panic(thrownPanic{exc: rec.exc, typ: rec.typ})
}

// X__resumeException(ptr): no catch clause matched; keep unwinding. (T1)
func (h *Host) X__resumeException(ptr int32) {
	rec, _ := h.top()
	h.logf("__resumeException ptr=%d", ptr)
	panic(thrownPanic{exc: ptr, typ: rec.typ})
}

// _emscripten_throw_longjmp is how a STANDALONE -fexceptions build models
// longjmp: it raises a sentinel that unwinds to the invoke_ trampoline guarding
// the matching setjmp, identical machinery to a C++ throw. We reuse the same
// panic so the trampoline's setThrew path handles it.
func (h *Host) X_emscripten_throw_longjmp() {
	h.logf("_emscripten_throw_longjmp")
	panic(thrownPanic{})
}

// X__cxa_find_matching_catch_2 / _3 / _4 / _5: given the active exception and a
// list of candidate catch-clause type_info pointers, decide which clause (if
// any) catches it, then PUBLISH that matched clause's type id into tempRet0 and
// return the (base-class-adjusted) object pointer. The wasm landing pad then
// does `getTempRet0() == llvm_eh_typeid_for(catchType)` to select the clause,
// so the id we publish MUST be the id of the MATCHED CATCH type, not the thrown
// type. This is the RTTI-delegation contract: the actual is-catchable test is
// the module's exported __cxa_can_catch (libc++abi's own RTTI), never Go code.
//
// The _N variants pass N-1 candidate catch types (highest-priority first); we
// return on the first that __cxa_can_catch accepts. _2 passes none, which means
// "catch (...)" / cleanup: it matches unconditionally.
func (h *Host) findMatch(catchTypes ...int32) int32 {
	rec, ok := h.top()
	if !ok {
		h.abi.TempretSet(0)
		return 0
	}

	// Scratch slot holding the candidate object pointer that __cxa_can_catch may
	// adjust for a base-class catch. Seed it with the exception's object pointer
	// (the C++ object, obtained from the exception header via the module's
	// __cxa_get_exception_ptr; falls back to the header pointer itself).
	objPtr := h.abi.GetExceptionPtr(rec.exc)
	if objPtr == 0 {
		objPtr = rec.exc
	}
	slot := h.abi.Malloc(4)
	h.abi.WriteU32(slot, objPtr)

	matchedType := int32(0) // 0 typeinfo -> catch-all id (1)
	found := len(catchTypes) == 0 // _2 (no candidates) is a catch-all

	for _, ct := range catchTypes {
		if ct == 0 {
			// A catch (...) clause arrives as a NULL typeinfo candidate and
			// matches every exception unconditionally (Emscripten's
			// findMatchingCatch does the same). Probing the module's
			// __cxa_can_catch with 0 instead virtual-calls through address 0
			// and crashes — seen on the scalar-UDF error path, whose landing
			// pad is {InvalidInputException-cleanup, catch(...)}.
			matchedType = 0
			found = true
			break
		}
		h.abi.WriteU32(slot, objPtr)
		cc := h.abi.CanCatch(ct, rec.typ, slot)
		if DebugThrow {
			fmt.Fprintf(os.Stderr, "[exhost] findMatch thrown=%q candidate catch=%q canCatch=%d\n",
				h.cstrU32(h.abi.ReadU32(rec.typ+4)), h.cstrU32(h.abi.ReadU32(ct+4)), cc)
		}
		if cc != 0 {
			matchedType = ct
			found = true
			break
		}
	}
	if DebugThrow && len(catchTypes) == 0 {
		fmt.Fprintf(os.Stderr, "[exhost] findMatch thrown=%q candidates=NONE (catch-all/cleanup)\n",
			h.cstrU32(h.abi.ReadU32(rec.typ+4)))
	}
	h.abi.Free(slot)

	if !found {
		// No candidate clause catches this type. Publish 0 so the wasm's
		// id-compare fails and it takes __resumeException.
		h.tempRet0 = 0
		h.abi.TempretSet(0)
		h.logf("__cxa_find_matching_catch NO MATCH (thrown typ=%d) -> resume", rec.typ)
		return rec.exc
	}

	// Publish the MATCHED CATCH type's id (so it equals the wasm's
	// llvm_eh_typeid_for(catchType)). Return the EXCEPTION pointer that was
	// thrown - __cxa_begin_catch performs the object/base-class adjustment, the
	// same division libc++abi uses. Returning the pre-adjusted object here would
	// break begin_catch's vtable access.
	id := h.typeID(matchedType)
	h.tempRet0 = id
	h.abi.TempretSet(id)
	h.logf("__cxa_find_matching_catch matched catchType=%d -> exc=%d tempRet0=%d",
		matchedType, rec.exc, id)
	return rec.exc
}

func (h *Host) X__cxa_find_matching_catch_2() int32         { return h.findMatch() }
func (h *Host) X__cxa_find_matching_catch_3(t0 int32) int32 { return h.findMatch(t0) }
func (h *Host) X__cxa_find_matching_catch_4(t0, t1 int32) int32 {
	return h.findMatch(t0, t1)
}
func (h *Host) X__cxa_find_matching_catch_5(t0, t1, t2 int32) int32 {
	return h.findMatch(t0, t1, t2)
}

// Xllvm_eh_typeid_for(typeinfo): stable nonzero id, must agree with the id
// find_matching_catch published for the same type. (T1)
func (h *Host) Xllvm_eh_typeid_for(typ int32) int32 {
	id := h.typeID(typ)
	h.logf("llvm_eh_typeid_for typ=%d -> %d", typ, id)
	return id
}

// X__cxa_begin_catch(ptr): enter a catch; the exception stops being uncaught.
// Returns the adjusted object pointer (the wasm passes the header; the simple
// path returns it unchanged, matching T1). (T1)
func (h *Host) X__cxa_begin_catch(ptr int32) int32 {
	if h.uncaught > 0 {
		h.uncaught--
	}
	h.logf("__cxa_begin_catch ptr=%d", ptr)
	return ptr
}

// X__cxa_end_catch(): leave the catch, popping the handled exception. (T1)
func (h *Host) X__cxa_end_catch() {
	if n := len(h.inflight); n > 0 {
		h.inflight = h.inflight[:n-1]
	}
	h.logf("__cxa_end_catch")
}

// X__cxa_uncaught_exceptions returns the count of thrown-not-yet-caught
// exceptions (std::uncaught_exceptions()).
func (h *Host) X__cxa_uncaught_exceptions() int32 { return h.uncaught }

// X__cxa_current_primary_exception returns the top in-flight exception object
// (used by std::current_exception). Zero when none is active.
func (h *Host) X__cxa_current_primary_exception() int32 {
	rec, ok := h.top()
	if !ok {
		return 0
	}
	return rec.exc
}

// X__cxa_rethrow_primary_exception re-raises a captured primary exception
// (std::rethrow_exception). A zero pointer is a no-op.
func (h *Host) X__cxa_rethrow_primary_exception(excPtr int32) {
	if excPtr == 0 {
		return
	}
	h.logf("__cxa_rethrow_primary_exception exc=%d", excPtr)
	h.inflight = append(h.inflight, excRecord{exc: excPtr})
	h.uncaught++
	panic(thrownPanic{exc: excPtr})
}

// ---- tempRet0 register (imports, for builds that use them) ----------------

func (h *Host) XgetTempRet0() int32     { return h.tempRet0 }
func (h *Host) XsetTempRet0(v int32)    { h.tempRet0 = v }
func (h *Host) Xg_getTempRet0() int32   { return h.tempRet0 } // alt name some toolchains emit
func (h *Host) Xg_setTempRet0(v int32)  { h.tempRet0 = v }
