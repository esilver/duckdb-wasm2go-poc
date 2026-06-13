// Context-cancellation -> duckdb_interrupt wiring.
//
// The wasm engine is single-threaded and every C-API call is serialized by the
// conn's engine mutex, so a runaway statement holds that mutex until it
// finishes: context cancellation used to land only at statement BOUNDARIES.
// duckdb_interrupt is the engine's own cross-thread escape hatch — in native
// DuckDB it is documented as safe to call from another thread while a query
// runs, because all it does is set ClientContext's atomic<bool> interrupted
// flag (duckdb-src/src/main/client_context.cpp: Interrupt()); the executor
// polls that flag between pipeline tasks and aborts with an INTERRUPT error.
//
// HOW the flag is set matters in the transpiled single-linear-memory engine.
// The generated Xduckdb_interrupt export is NOT safe to call concurrently
// with a running query: its Connection::context null-check helper (Fn386)
// does a save/restore on the shadow-stack-pointer global (m.G0 -= 16 ...
// m.G0 += 16), an unsynchronized read-modify-write racing the engine
// goroutine's own stack-pointer updates — a lost update there corrupts the
// running query's C stack. The happy path of the export, however, reduces to
// pure memory operations:
//
//	ctx  = load32(mem[con])             // Connection.context shared_ptr ptr
//	mem[ctx+16] = 1                     // ClientContext.interrupted
//
// (offset 16 = enable_shared_from_this weak_ptr (8B on wasm32) +
// shared_ptr<DatabaseInstance> db (8B); see duckdb/main/client_context.hpp.)
// So the driver pokes the flag byte DIRECTLY from the watcher goroutine —
// the same single-byte store the atomic flag write compiles to, with no
// global touched — and the layout assumption is VALIDATED at runtime:
// resolveInterruptFlag probes each new connection by calling the real
// Xduckdb_interrupt export while the engine is idle (open/connect time,
// lock held — the G0 dance is harmless then) and checking the byte it
// computed actually flipped to 1. If the layout ever shifts, validation
// fails and the driver falls back to boundary-only cancellation instead of
// corrupting memory.
//
// Two windows make a single fire unreliable, so the watcher REFIRES until
// disarmed:
//   - ClientContext::InitialCleanup clears interrupted at every statement
//     start, so a fire that lands during statement setup (or between the
//     statements of a multi-statement batch) would be swallowed;
//   - the post-failure recovery ROLLBACK (recoverTxLocked) runs while still
//     armed, so a cleanup path that polls the flag is interruptible too.
//
// A late fire that loses the race against query completion just leaves the
// flag set; the next statement's InitialCleanup clears it (engine-side).
package duckdb

import (
	"context"
	"errors"
	"time"
)

// interruptedFlagOffset is ClientContext.interrupted's offset from the
// ClientContext base pointer on wasm32 (see the file comment; validated per
// connection by resolveInterruptFlag, never trusted blindly).
const interruptedFlagOffset = 16

// interruptRefire is how often the watcher re-sets the interrupt flag after
// ctx fires, closing the InitialCleanup clear-window (see file comment).
const interruptRefire = 50 * time.Millisecond

// resolveInterruptFlag computes and VALIDATES the linear-memory address of
// con's ClientContext.interrupted flag. It must run while the engine is idle
// (open/connect time, engine lock held): the validation probe calls the real
// Xduckdb_interrupt export, whose shadow-stack save/restore is only safe with
// no query running. Returns 0 (cancellation degrades to statement boundaries)
// if the layout assumption does not hold on this build.
func (mod *module) resolveInterruptFlag(con int32) int32 {
	if con == 0 {
		return 0
	}
	ctxPtr := int32(mod.readU32(con))
	if ctxPtr <= 0 {
		return 0
	}
	addr := ctxPtr + interruptedFlagOffset
	mem := mod.mem()
	if addr <= 0 || int(addr) >= len(mem) {
		return 0
	}
	saved := mem[addr]
	mem[addr] = 0
	mod.m.Xduckdb_interrupt(con) // ground truth: the engine's own flag write
	mem = mod.mem()
	if mem[addr] != 1 {
		mem[addr] = saved
		return 0
	}
	mem[addr] = saved // restore (= ClientContext::ClearInterrupt)
	return addr
}

// armInterrupt spawns a watcher that sets the connection's validated
// interrupted flag (flagAddr, from resolveInterruptFlag) — repeatedly, every
// interruptRefire — once ctx is cancelled, until disarm is called. disarm
// synchronously joins the watcher: after it returns, no further poke from
// this arm is possible. Callers must hold the engine mutex for the whole
// armed region (which also keeps the connection alive: conn.Close takes the
// same mutex). flagAddr 0 (validation failed / no ctx) arms nothing.
func (mod *module) armInterrupt(ctx context.Context, flagAddr int32) (disarm func()) {
	if flagAddr == 0 || ctx == nil || ctx.Done() == nil {
		return func() {}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		// A concurrent memory.Grow on the engine goroutine swaps the linear-
		// memory slice; a torn read of its header here would panic, not
		// corrupt. Losing one fire is fine — the refire loop lands the next.
		defer func() { _ = recover() }()
		select {
		case <-stop:
			return
		case <-ctx.Done():
		}
		t := time.NewTicker(interruptRefire)
		defer t.Stop()
		for {
			pokeInterruptFlag(mod, flagAddr)
			select {
			case <-stop:
				return
			case <-t.C:
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

// pokeInterruptFlag writes DuckDB's validated ClientContext.interrupted flag.
//
// The write is intentionally concurrent with the single-threaded transpiled
// engine: native DuckDB exposes duckdb_interrupt for exactly this cross-thread
// flag store, and resolveInterruptFlag validated the target byte while the
// engine was idle. Race instrumentation cannot model that foreign-memory
// contract and reports false positives against engine memory growth/reads, so
// this helper is kept out of race instrumentation.
//
//go:norace
func pokeInterruptFlag(mod *module, flagAddr int32) {
	if mem := mod.mem(); int(flagAddr) < len(mem) {
		mem[flagAddr] = 1
	}
}

// ctxErr folds a context cancellation into a failed engine call's error: the
// caller sees BOTH errors.Is(err, ctx.Err()) (what database/sql callers and
// watchdogs select on) and the engine's own text ("INTERRUPT Error: ...").
// Engine errors with a live context — and engine successes, interrupt lost the
// race — pass through untouched, preserving native error-text fidelity.
func ctxErr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if cerr := ctx.Err(); cerr != nil {
		return errors.Join(cerr, err)
	}
	return err
}
