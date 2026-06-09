// udf_register.go — exported, *sql.Conn-level registration entry points.
//
// The duckdbcompat package (which gives the googlesqlite emulator a pure-Go,
// CGO_ENABLED=0 stand-in for github.com/duckdb/duckdb-go/v2) lives in a DIFFERENT
// Go package and therefore cannot reach this package's unexported *module / *conn,
// nor their RegisterScalarUDF / RegisterAggregateUDF methods. These exported helpers
// are the bridge: each takes a *sql.Conn, unwraps it to our *conn via Conn.Raw, takes
// the connection's engine lock, and registers (or executes) against the underlying
// engine handles.
//
// All three follow the same shape:
//
//	c.Raw(func(dc any) error {
//	    co := dc.(*conn)         // our driver.Conn
//	    co.mu.Lock(); defer co.mu.Unlock()   // serialize the single-threaded engine
//	    ... use co.mod / co.con ...
//	})
//
// Conn.Raw holds the connection out of the pool for the callback's duration, so the
// engine lock plus Raw's exclusivity make this safe even though the engine is shared
// across the connector's connections.
package duckdb

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
)

// RegisterScalarConn registers a Go scalar function under `name` on the engine behind
// c, visible to every connection sharing c's database (registration is catalog-scoped).
//
//   - paramTypeIDs are the fixed (leading) parameter duckdb_type ids, in order.
//   - retTypeID is the result duckdb_type id.
//   - varargsTypeID >= 0 makes the function variadic over that trailing type
//     (e.g. DUCKDB_TYPE_ANY = 34); varargsTypeID < 0 means non-variadic.
//   - specialNull requests special NULL handling (the function is invoked for NULL
//     args instead of being short-circuited).
//   - volatile marks the result non-deterministic (never constant-folded).
//   - fn receives one decoded argument per ACTUAL call argument (fixed params followed
//     by the expanded variadic tail) as a []driver.Value, and returns the result value
//     (or nil for SQL NULL) plus an optional error; a non-nil error aborts the query.
//
// fn's parameter is []driver.Value to match the emulator's ScalarFuncExecutor.RowExecutor
// signature; []driver.Value is assignable to the []any the engine callback passes.
func RegisterScalarConn(c *sql.Conn, name string, paramTypeIDs []int32, retTypeID int32, varargsTypeID int32, specialNull, volatile bool, fn func(args []driver.Value) (any, error)) error {
	// Adapt fn(args []driver.Value) to the engine's fn(args []any). []any and
	// []driver.Value are DISTINCT slice types in Go (driver.Value is a defined type,
	// not an alias for any), so the slice must be rebuilt element-wise (each any is
	// assignable to driver.Value). Allocated once per chunk callback — negligible.
	wrapped := func(args []any) (any, error) {
		dv := make([]driver.Value, len(args))
		for i, a := range args {
			dv[i] = a
		}
		return fn(dv)
	}
	return withConn(c, func(co *conn) error {
		return co.mod.registerScalarEx(co.con, name, paramTypeIDs, retTypeID, varargsTypeID, specialNull, volatile, wrapped)
	})
}

// RegisterAggregateConn registers a Go aggregate function under `name` on the engine
// behind c. paramTypeIDs / retTypeID are duckdb_type ids; impl supplies the
// NewState/Update/Combine/Finalize behavior. Registration is catalog-scoped (visible
// to all connections sharing c's database).
func RegisterAggregateConn(c *sql.Conn, name string, paramTypeIDs []int32, retTypeID int32, impl AggregateImpl) error {
	return withConn(c, func(co *conn) error {
		return co.mod.RegisterAggregateUDF(co.con, name, paramTypeIDs, retTypeID, impl)
	})
}

// RegisterAggregateBandConn registers a Go aggregate function under `name` for every
// arity in opts' [MinArgs, MaxArgs] band, parameters typed ANY (see AggregateOptions
// and registerAggregateBand for the full conventions: runtime-typed decode, DECIMAL
// as float64, JSON-alias cells as JSONValue, NULL rows delivered as nil args). This
// is the registration shape the googlesqlite emulator's 32 aggregate specs need.
// Registration is catalog-scoped (visible to all connections sharing c's database).
func RegisterAggregateBandConn(c *sql.Conn, name string, opts AggregateOptions, impl AggregateImpl) error {
	return withConn(c, func(co *conn) error {
		return co.mod.registerAggregateBand(co.con, name, opts, impl)
	})
}

// ExecConn runs an argument-less SQL statement on the engine behind c and discards
// any result. It is the backing for the compat table-UDF shim, which emits a
// `CREATE TABLE <name>(...)` (a zero-row stub) instead of a real table function.
func ExecConn(c *sql.Conn, sqlText string) error {
	return withConn(c, func(co *conn) error {
		if co.closed {
			return driver.ErrBadConn
		}
		st, err := co.prepareLocked(sqlText)
		if err != nil {
			return err
		}
		defer st.closeLocked()
		_, err = st.execLocked(nil)
		return err
	})
}

// withConn unwraps c to our *conn, takes the engine lock for the duration of f, and
// runs f. It centralizes the Conn.Raw + type-assert + lock dance shared by the three
// exported entry points above. The lock is the connection's shared engine mutex
// (co.mu), so f runs with exclusive access to the single-threaded wasm engine.
func withConn(c *sql.Conn, f func(co *conn) error) error {
	if c == nil {
		return fmt.Errorf("duckdb: nil *sql.Conn")
	}
	return c.Raw(func(dc any) error {
		co, ok := dc.(*conn)
		if !ok {
			return fmt.Errorf("duckdb: *sql.Conn is not backed by this driver (got %T)", dc)
		}
		co.mu.Lock()
		defer co.mu.Unlock()
		return f(co)
	})
}
