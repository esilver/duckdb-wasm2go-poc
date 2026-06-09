package wasitest

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"duckdbharness/exhost"
	"duckdbharness/wasishim"
	wp "duckdbharness/wasitest/wpgen"
)

// This package validates the wasishim layer against a wasm that ACTUALLY
// imports wasi_snapshot_preview1 (fd_write, clock_time_get) and the emscripten
// env growth notification - the residual surface a real in-memory DuckDB build
// presents and that the tiny poc.cc avoided. It also exercises the MULTI-IMPORT
// wasm2go shape: New(wasiArg, envArg) takes one arg per import module.

type wpModABI struct{ m *wp.Module }

func (a wpModABI) SetThrew(threw, value int32) { a.m.XsetThrew(threw, value) }
func (a wpModABI) TempretSet(v int32)          { a.m.X_emscripten_tempret_set(v) }
func (a wpModABI) Table() []any                { return *a.m.X__indirect_function_table() }
func (a wpModABI) CanCatch(c, e, s int32) int32 { return a.m.X__cxa_can_catch(c, e, s) }
func (a wpModABI) GetExceptionPtr(h int32) int32 { return a.m.X__cxa_get_exception_ptr(h) }
func (a wpModABI) DynamicCast(o, s, d, off int32) int32 { return 0 }
func (a wpModABI) Malloc(n int32) int32 { return a.m.Xmalloc(n) }
func (a wpModABI) Free(p int32)         { a.m.Xfree(p) }
func (a wpModABI) ReadU32(p int32) int32 {
	mem := *a.m.Xmemory().Slice()
	return int32(binary.LittleEndian.Uint32(mem[p:]))
}
func (a wpModABI) WriteU32(p, v int32) {
	mem := *a.m.Xmemory().Slice()
	binary.LittleEndian.PutUint32(mem[p:], uint32(v))
}

type wpMemABI struct{ m *wp.Module }

func (a wpMemABI) Mem() []byte { return *a.m.Xmemory().Slice() }
func (a wpMemABI) Grow(d int32) int32 {
	return int32(a.m.Xmemory().Grow(int64(d), 1<<31))
}

// envArg is the value passed as the wasm's "env" import: exception ABI
// (exhost.Host) PLUS the emscripten env methods (emscripten_notify_memory_growth
// etc, promoted from *wasishim.Shim). Its Init binds both adapters.
type envArg struct {
	*exhost.Host
	*wasishim.Shim
}

func (e *envArg) Init(m any) {
	mod := m.(*wp.Module)
	e.Host.Init(m)
	e.Shim.SetMem(wpMemABI{m: mod})
}

func TestWasiShimDrivesPrintfAndClock(t *testing.T) {
	var stdout bytes.Buffer
	host := exhost.New(func(mod any) exhost.ModuleABI { return wpModABI{m: mod.(*wp.Module)} })
	shim := wasishim.New(nil, &stdout, &stdout)

	ea := &envArg{Host: host, Shim: shim}

	// MULTI-IMPORT wiring: New(wasiArg, envArg). The wasi arg is the SAME shim
	// (it carries fd_write/clock_time_get); the env arg carries exceptions +
	// emscripten env. Both receive Init; the shim's memory is bound from the env
	// arg's Init (the wasi arg's Init would rebind it identically).
	m := wp.New(shim, ea)
	shim.SetMem(wpMemABI{m: m}) // ensure memory bound before any call

	got := m.Xtouch_io(3) // 3 MiB alloc forces heap growth; printf hits fd_write

	out := stdout.String()
	if !strings.Contains(out, "wasiprobe: n=3") {
		t.Fatalf("expected printf output via fd_write shim, got %q (shim log: %v)", out, shim.Log)
	}
	// touch_io returns sum of memset bytes (all 1s, one per 4096) + 1 for the
	// successful clock read. 3 MiB / 4096 = 768 sampled bytes of value 1, +1.
	if got != 768+1 {
		t.Fatalf("touch_io(3) = %d, want %d (heap-grow + clock path)", got, 768+1)
	}
	t.Logf("fd_write stdout=%q touch_io=%d shimStubLog=%v", strings.TrimSpace(out), got, shim.Log)
}
