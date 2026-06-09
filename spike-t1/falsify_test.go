package trymod

import "testing"

// hostNoThrew is a deliberately-broken host that performs the indirect call but
// NEVER reports the throw to the wasm (it swallows the panic and skips
// setThrew). __cxa_throw is dispatched through invoke_viii (table[2]), so that
// is the trampoline the throw unwinds through. If the catch still "fired" with
// the threw-flag suppressed, the green result would be an artifact. We expect
// try_it to NOT return 1, proving the result depends on the threw-flag.
type hostNoThrew struct{ host }

func (h *hostNoThrew) Xinvoke_viii(index, a0, a1, a2 int32) {
	defer func() { recover() }() // swallow throw, do NOT setThrew
	f := h.table()[index].(func(int32, int32, int32))
	f(a0, a1, a2)
}

func TestFalsify_NoThrewFlag(t *testing.T) {
	h := &hostNoThrew{}
	m := New(h)
	m.X_initialize()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected try_it to mis-behave (not reach the catch) when the threw-flag is suppressed, but it returned normally")
		}
		// With setThrew suppressed the wasm reads threw==0, skips the landing
		// pad, and falls into panic("unreachable"). That proves the catch is
		// gated on the threw-flag, so the green result is genuine, not luck.
		t.Logf("threw-flag suppressed -> wasm did NOT take the catch (got panic %v): catch genuinely depends on setThrew", r)
	}()

	got := m.Xtry_it(3) // odd -> C++ throws
	t.Fatalf("expected panic/mis-behavior with threw-flag suppressed, but try_it(3) returned %d", got)
}

// hostNoMatch reports NO matching catch (returns type id 0 from
// find_matching_catch, which will not equal llvm_eh_typeid_for's nonzero id).
// The wasm should then take the __resumeException path instead of the catch.
type hostNoMatch struct{ host }

func (h *hostNoMatch) X__cxa_find_matching_catch_3(catchType int32) int32 {
	h.mod.X_emscripten_tempret_set(0) // id 0 -> will NOT match typeid 1
	return h.inflightExc
}
func (h *hostNoMatch) X__cxa_find_matching_catch_2() int32 {
	h.mod.X_emscripten_tempret_set(0)
	return h.inflightExc
}

func (h *hostNoMatch) X__resumeException(ptr int32) {
	h.resumed = true
	panic(thrownPanic{exc: ptr, typ: h.inflightType})
}

func TestFalsify_NoTypeMatch(t *testing.T) {
	h := &hostNoMatch{}
	m := New(h)
	m.X_initialize()

	defer func() {
		r := recover()
		if !h.resumed {
			t.Fatalf("expected __resumeException to be taken on type mismatch; resumed=%v recover=%v", h.resumed, r)
		}
		t.Logf("type mismatch -> __resumeException taken (recover=%v): catch is genuinely gated on the type-id compare", r)
	}()

	got := m.Xtry_it(3)
	t.Fatalf("expected resume/panic on mismatch, but try_it returned %d normally", got)
}
