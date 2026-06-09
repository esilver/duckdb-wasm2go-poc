package trymod

import "fmt"

// host implements the Xenv interface that the wasm2go-generated module
// (try_gen.go) requires. It reproduces the Emscripten "legacy" (opcode-free)
// C++ exception ABI:
//
//   - The compiled C++ wraps every potentially-throwing indirect call in an
//     invoke_* trampoline. The trampoline is a HOST function: it performs the
//     indirect call and, if a C++ exception is thrown, sets a "threw" flag in
//     linear memory (via the module's exported setThrew) and returns normally
//     so the wasm landing pad can run.
//   - __cxa_throw is also a HOST function. In native Emscripten it longjmps
//     back to the active invoke_* trampoline. Here, because BOTH the throw and
//     the trampoline are our Go code, we unwind with a Go panic/recover.
//   - __cxa_find_matching_catch_N performs the RTTI match (libc++abi does this
//     in native code). For this probe the thrown type (std::runtime_error) is
//     caught as its base std::exception, so we report a match and hand back the
//     exception pointer + set tempRet0 to the matched type id.
type host struct {
	mod *Module

	// inflight holds the exception object pointer + typeinfo pointer that the
	// most recent __cxa_throw recorded. Valid only between throw and the
	// landing pad's begin_catch/end_catch.
	inflightExc  int32
	inflightType int32
	haveInflight bool

	// log records the ABI calls in order so the test can show the path taken.
	log []string

	// resumed is set if __resumeException was reached (no catch matched).
	resumed bool
}

// thrownPanic is the sentinel we panic with from __cxa_throw to unwind back
// to the invoke_* trampoline that is on the Go stack.
type thrownPanic struct{ exc, typ int32 }

// Init is the hook wasm2go's New() calls, handing us the *Module so we can
// drive its exports (setThrew, the indirect function table, memory).
func (h *host) Init(m any) {
	h.mod = m.(*Module)
}

func (h *host) table() []any { return *h.mod.X__indirect_function_table() }

func (h *host) logf(f string, a ...any) { h.log = append(h.log, fmt.Sprintf(f, a...)) }

// ---- invoke_* trampolines -------------------------------------------------
//
// Each calls the indirect target at table[index]; on a thrown C++ exception
// it records the flag via setThrew(1, excPtr) and returns a zero value.

func (h *host) call(index int32, do func()) (threw bool) {
	defer func() {
		if r := recover(); r != nil {
			tp, ok := r.(thrownPanic)
			if !ok {
				panic(r) // not a C++ throw, propagate
			}
			h.logf("invoke caught throw exc=%d typ=%d -> setThrew(1,%d)", tp.exc, tp.typ, tp.exc)
			h.mod.XsetThrew(1, tp.exc)
			threw = true
		}
	}()
	do()
	return false
}

func (h *host) Xinvoke_iii(index, a0, a1 int32) int32 {
	var ret int32
	h.call(index, func() {
		f := h.table()[index].(func(int32, int32) int32)
		ret = f(a0, a1)
	})
	return ret
}

func (h *host) Xinvoke_ii(index, a0 int32) int32 {
	var ret int32
	h.call(index, func() {
		f := h.table()[index].(func(int32) int32)
		ret = f(a0)
	})
	return ret
}

func (h *host) Xinvoke_v(index int32) {
	h.call(index, func() {
		f := h.table()[index].(func())
		f()
	})
}

func (h *host) Xinvoke_vi(index, a0 int32) {
	h.call(index, func() {
		f := h.table()[index].(func(int32))
		f(a0)
	})
}

func (h *host) Xinvoke_vii(index, a0, a1 int32) {
	h.call(index, func() {
		f := h.table()[index].(func(int32, int32))
		f(a0, a1)
	})
}

func (h *host) Xinvoke_viii(index, a0, a1, a2 int32) {
	h.call(index, func() {
		f := h.table()[index].(func(int32, int32, int32))
		f(a0, a1, a2)
	})
}

// ---- C++ exception ABI ----------------------------------------------------

// __cxa_throw(ptr, typeinfo, dtor): record the in-flight exception and unwind
// to the active invoke_* trampoline via panic.
func (h *host) X__cxa_throw(ptr, typ, dtor int32) {
	h.logf("__cxa_throw ptr=%d typ=%d dtor=%d", ptr, typ, dtor)
	h.inflightExc = ptr
	h.inflightType = typ
	h.haveInflight = true
	panic(thrownPanic{exc: ptr, typ: typ})
}

// __cxa_find_matching_catch_3(catchType): in real libc++abi this walks the
// thrown object's RTTI to see if it derives from catchType. The probe's catch
// is `catch (const std::exception&)`, and we threw std::runtime_error (derived),
// so we report a match: return the (adjusted) exception pointer and stash the
// matched type id in tempRet0 (g1) for the wasm to read back.
func (h *host) X__cxa_find_matching_catch_3(catchType int32) int32 {
	id := h.typeID(h.inflightType)
	h.logf("__cxa_find_matching_catch_3 catchType=%d -> exc=%d tempRet0=%d", catchType, h.inflightExc, id)
	h.mod.X_emscripten_tempret_set(id)
	return h.inflightExc
}

func (h *host) X__cxa_find_matching_catch_2() int32 {
	id := h.typeID(h.inflightType)
	h.logf("__cxa_find_matching_catch_2 -> exc=%d tempRet0=%d", h.inflightExc, id)
	h.mod.X_emscripten_tempret_set(id)
	return h.inflightExc
}

// llvm_eh_typeid_for(typeinfo): return a stable, nonzero id for a typeinfo
// pointer. The wasm compares this against the tempRet0 value set by
// find_matching_catch; equal => this catch clause handles the exception.
func (h *host) Xllvm_eh_typeid_for(typ int32) int32 {
	id := h.typeID(typ)
	h.logf("llvm_eh_typeid_for typ=%d -> %d", typ, id)
	return id
}

// typeID maps a typeinfo pointer to the SAME id used in find_matching_catch so
// that, for a matching catch, the two agree. For this single-type probe the
// thrown std::runtime_error is caught as std::exception; we collapse both the
// thrown type and the catch's expected type to one id so the compare succeeds.
func (h *host) typeID(typeinfo int32) int32 { return 1 }

func (h *host) X__cxa_begin_catch(ptr int32) int32 {
	h.logf("__cxa_begin_catch ptr=%d", ptr)
	return ptr
}

func (h *host) X__cxa_end_catch() {
	h.logf("__cxa_end_catch")
	h.haveInflight = false
}

// __resumeException(ptr): reached only when no catch clause matched. In the
// probe a match always succeeds, so hitting this means the ABI is wrong.
func (h *host) X__resumeException(ptr int32) {
	h.logf("__resumeException ptr=%d (NO MATCH)", ptr)
	panic(thrownPanic{exc: ptr, typ: h.inflightType})
}
