package main

import (
	"bytes"
	"strings"
	"testing"

	"duckdbharness/exhost"
	poc "duckdbharness/genpkg"
	"duckdbharness/wasishim"
)

// newTestModule builds a fresh module + env for one test, returning both so the
// test can inspect the host's ABI trace.
func newTestModule(t *testing.T) (*poc.Module, *env) {
	t.Helper()
	var out bytes.Buffer
	host := exhost.New(func(mod any) exhost.ModuleABI { return modABI{m: mod.(*poc.Module)} })
	host.Trace = true
	shim := wasishim.New(nil, &out, &out)
	e := &env{Host: host, Shim: shim}
	m := poc.New(e)
	m.X_initialize()
	return m, e
}

// TestSelect1 is the headline: "SELECT 1" returns 1 through pure Go.
func TestSelect1(t *testing.T) {
	m, _ := newTestModule(t)
	res := runScalar(m, "SELECT 1")
	if res.status != 0 || res.value != 1 {
		t.Fatalf("SELECT 1: status=%d value=%d, want status=0 value=1", res.status, res.value)
	}
	res = runScalar(m, "SELECT 42")
	if res.status != 0 || res.value != 42 {
		t.Fatalf("SELECT 42: status=%d value=%d, want status=0 value=42", res.status, res.value)
	}
}

// TestBadQueryCatches proves the throw->catch path returns an error (not an
// abort) AND that the exception ABI was actually walked (throw, find-matching,
// begin/end catch all appear in the trace). This rules out a green-by-luck
// result where query_scalar simply returned 1 without exercising exceptions.
func TestBadQueryCatches(t *testing.T) {
	m, e := newTestModule(t)
	res := runScalar(m, "SELECT bogus")
	if res.status == 0 {
		t.Fatalf("bad query unexpectedly succeeded with value=%d", res.value)
	}
	if !strings.Contains(res.errMsg, "unknown query") {
		t.Fatalf("bad query error message = %q, want it to contain the wasm's what()", res.errMsg)
	}

	trace := strings.Join(e.Host.Log(), "\n")
	for _, want := range []string{"__cxa_throw", "__cxa_find_matching_catch", "__cxa_begin_catch", "__cxa_end_catch"} {
		if !strings.Contains(trace, want) {
			t.Fatalf("expected %s in ABI trace, got:\n%s", want, trace)
		}
	}
	t.Logf("bad-query ABI path:\n%s", trace)
}

// brokenThrew embeds env but suppresses setThrew via a broken ABI adapter: the
// invoke trampoline still recovers the throw but never tells the wasm. If the
// catch still "fired" the green result would be an artifact. We expect the wasm
// to NOT take the landing pad (it reads threw==0) and instead crash into its
// unreachable/abort path -> a panic. (Mirrors T1's TestFalsify_NoThrewFlag.)
type noThrewABI struct{ modABI }

func (a noThrewABI) SetThrew(threw, value int32) { /* swallow: never report */ }

func TestFalsify_NoThrewFlag(t *testing.T) {
	var out bytes.Buffer
	host := exhost.New(func(mod any) exhost.ModuleABI { return noThrewABI{modABI{m: mod.(*poc.Module)}} })
	shim := wasishim.New(nil, &out, &out)
	e := &env{Host: host, Shim: shim}
	m := poc.New(e)
	m.X_initialize()

	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("with setThrew suppressed the bad query should NOT reach the catch and should mis-behave, but runScalar returned normally")
		} else {
			t.Logf("setThrew suppressed -> wasm did not take the catch (panic %v): catch genuinely depends on the threw-flag", r)
		}
	}()
	res := runScalar(m, "SELECT bogus")
	t.Fatalf("expected mis-behavior with threw suppressed, but got status=%d value=%d", res.status, res.value)
}

// noMatchABI forces __cxa_can_catch to report NO catch, so find_matching_catch
// publishes a non-matching id and the wasm must take __resumeException, which
// our host turns into a panic. Proves the catch is gated on the real RTTI
// decision (the module's __cxa_can_catch), not hardcoded. (Mirrors T1's
// TestFalsify_NoTypeMatch.)
type noMatchABI struct{ modABI }

func (a noMatchABI) CanCatch(catchType, excType, slot int32) int32 { return 0 }

func TestFalsify_NoTypeMatch(t *testing.T) {
	var out bytes.Buffer
	host := exhost.New(func(mod any) exhost.ModuleABI { return noMatchABI{modABI{m: mod.(*poc.Module)}} })
	shim := wasishim.New(nil, &out, &out)
	e := &env{Host: host, Shim: shim}
	m := poc.New(e)
	m.X_initialize()

	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("with __cxa_can_catch forced to 0 the wasm should take __resumeException and panic, but runScalar returned normally")
		} else {
			t.Logf("can_catch=0 -> __resumeException taken (panic %v): catch is gated on the module's real RTTI", r)
		}
	}()
	res := runScalar(m, "SELECT bogus")
	t.Fatalf("expected resume/panic on forced no-match, but got status=%d value=%d", res.status, res.value)
}

// TestEchoLen guards the plain (non-throwing) cstring marshalling path.
func TestEchoLen(t *testing.T) {
	m, _ := newTestModule(t)
	for _, s := range []string{"", "x", "hello-duckdb", strings.Repeat("z", 500)} {
		p := cstring(m, s)
		if got := m.Xecho_len(p); int(got) != len(s) {
			t.Fatalf("echo_len(%q)=%d want %d", s, got, len(s))
		}
		m.Xfree(p)
	}
}
