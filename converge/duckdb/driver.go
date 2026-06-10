// This file is the database/sql surface of the pure-Go (CGO_ENABLED=0) DuckDB
// driver. It implements the database/sql/driver interfaces on top of the
// wasm-memory marshalling foundation in module.go: every C-API call goes through
// mod.m.Xduckdb_* with int32 (wasm32) pointers/handles and int64 idx_t params,
// and the engine is single-threaded so each conn serializes access with a mutex.
//
// Row reading lives in the sibling file result.go (newRows); this file only
// drives statement lifecycle, parameter binding, exec, and transactions.
package duckdb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

func init() {
	sql.Register("duckdb", &Driver{})
}

// Driver is the database/sql driver for DuckDB. The empty struct carries no
// state; every Open spins up a fresh engine instance (one wasm linear memory).
type Driver struct{}

var (
	_ driver.Driver        = (*Driver)(nil)
	_ driver.DriverContext = (*Driver)(nil)
)

// Open implements driver.Driver. The DSN is a database path; "" or ":memory:"
// opens an in-memory database. This raw path is standalone: the returned conn
// owns its own engine + database and closes both on Close.
func (d *Driver) Open(dsn string) (driver.Conn, error) {
	if dsn == "" {
		dsn = ":memory:"
	}
	mod := newModule()
	con, db, err := mod.open(dsn)
	if err != nil {
		return nil, err
	}
	return &conn{mod: mod, con: con, db: db, mu: &mod.mu, ownsDB: true}, nil
}

// OpenConnector implements driver.DriverContext. database/sql reuses ONE connector
// per sql.DB across the whole pool, so a connector is the right scope to share a
// single engine + database across all pooled connections.
func (d *Driver) OpenConnector(dsn string) (driver.Connector, error) {
	return newConnector(dsn), nil
}

// connector holds a DSN and a lazily-created SHARED engine. Every connection it
// hands out shares that one wasm engine + duckdb_database, so DDL on one pooled
// connection is visible to queries on another (matching DuckDB's real model and
// what callers like the googlesqlite emulator assume). c.mu both guards lazy init
// AND serializes all engine access (the wasm engine is single-threaded).
type connector struct {
	dsn  string
	mu   sync.Mutex
	mod  *module
	db   int32
	init bool
	err  error
}

var (
	_ driver.Connector = (*connector)(nil)
	_ io.Closer        = (*connector)(nil)
)

func newConnector(dsn string) *connector {
	if dsn == "" {
		dsn = ":memory:"
	}
	return &connector{dsn: dsn}
}

// Connect implements driver.Connector. The first call instantiates the shared
// engine and opens the database; every call (including the first) returns a fresh
// duckdb_connection against that shared database. All returned conns share c.mu,
// so concurrent pool use is serialized onto the single-threaded engine.
func (c *connector) Connect(_ context.Context) (driver.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.init {
		c.init = true
		mod := newModule()
		con, db, err := mod.open(c.dsn)
		if err != nil {
			c.err = err
			return nil, err
		}
		c.mod, c.db = mod, db
		// open() already made the first connection; reuse it.
		return &conn{mod: mod, con: con, db: db, mu: &c.mu, owner: c}, nil
	}
	if c.err != nil {
		return nil, c.err
	}
	con, err := c.mod.connect(c.db)
	if err != nil {
		return nil, err
	}
	return &conn{mod: c.mod, con: con, db: c.db, mu: &c.mu, owner: c}, nil
}

// Close implements io.Closer; database/sql calls it on sql.DB.Close. It closes the
// shared database handle once (conns only disconnect their own connection).
func (c *connector) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mod != nil && c.db != 0 {
		mod := c.mod
		dbSlot := mod.allocOut(4)
		mod.writeU32(dbSlot, uint32(c.db))
		mod.m.Xduckdb_close(dbSlot)
		mod.free(dbSlot)
		c.db = 0
	}
	return nil
}

// Driver implements driver.Connector.
func (c *connector) Driver() driver.Driver { return &Driver{} }

// conn is one DuckDB connection. Its engine may be SHARED with sibling conns from
// the same connector, so mu is a pointer to the shared engine lock (the wasm engine
// is single-threaded; mu serializes all C-API calls across every conn that shares
// it). owner!=nil means the database handle is connector-owned (closed by
// connector.Close); ownsDB means this standalone conn (raw Driver.Open) owns it.
type conn struct {
	mu     *sync.Mutex
	mod    *module
	con    int32 // duckdb_connection handle
	db     int32 // duckdb_database handle
	owner  *connector
	ownsDB bool
	closed bool
	// inTx tracks whether THIS duckdb_connection is inside an explicit
	// BEGIN..COMMIT/ROLLBACK (driven by BeginTx or by the user executing
	// transaction-control SQL directly). It gates automatic transaction
	// recovery after failed statements: see recoverTxLocked.
	inTx bool
}

var (
	_ driver.Conn               = (*conn)(nil)
	_ driver.ConnPrepareContext = (*conn)(nil)
	_ driver.ExecerContext      = (*conn)(nil)
	_ driver.QueryerContext     = (*conn)(nil)
	_ driver.Pinger             = (*conn)(nil)
)

// Prepare implements driver.Conn.

// guardEnginePanic converts a Go panic escaping the wasm engine (a transpiled-
// engine fault or a corrupted-state dereference) into a plain error. Without it
// the panic unwinds through database/sql's internals, which have no recover of
// their own — the connection's in-flight bookkeeping never settles and a later
// db.Close blocks forever on closemu (observed when a UDF argument decoded to
// nil and the UDF body panicked). The module's state after such a panic is
// undefined, so the connection should be treated as poisoned; an error makes
// that visible instead of hanging the pool.
func guardEnginePanic(errp *error) {
	if r := recover(); r != nil {
		*errp = fmt.Errorf("duckdb: engine panic: %v", r)
	}
}

func (c *conn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

// PrepareContext implements driver.ConnPrepareContext. It compiles query into a
// duckdb_prepared_statement; on failure it surfaces duckdb_prepare_error (or the
// last host exception) and still destroys the statement.
func (c *conn) PrepareContext(_ context.Context, query string) (st driver.Stmt, err error) {
	defer guardEnginePanic(&err)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.prepareLocked(query)
}

// prepareLocked compiles query. Callers must hold c.mu.
func (c *conn) prepareLocked(query string) (*stmt, error) {
	mod := c.mod
	sqlPtr := mod.cstring(query)
	defer mod.free(sqlPtr)

	stmtSlot := mod.allocOut(4)
	defer mod.free(stmtSlot)

	rc := mod.m.Xduckdb_prepare(c.con, sqlPtr, stmtSlot)
	st := mod.readPtr(stmtSlot)
	if rc != 0 {
		msg := ""
		if st != 0 {
			msg = mod.goString(mod.m.Xduckdb_prepare_error(st))
		}
		if msg == "" {
			msg = mod.lastError()
		}
		if st != 0 {
			destroyPrepare(mod, st)
		}
		// A failed PREPARE (parser/binder error) leaves the connection's
		// autocommit transaction dangling just like a failed execute does;
		// restore autocommit so the next statement is not poisoned.
		c.recoverTxLocked()
		return nil, fmt.Errorf("duckdb prepare: %s", orUnknown(msg))
	}
	return &stmt{c: c, handle: st, query: query}, nil
}

// ---- transaction-leak recovery (engine bug workaround, root-fixed here) -----
//
// ROOT CAUSE: in this wasm build, ANY failed statement (prepare-stage binder
// errors included) leaves the duckdb_connection's autocommit transaction open
// instead of rolling it back, so the NEXT statement on that connection fails
// with "cannot start a transaction within a transaction". The recovery is a
// ROLLBACK issued on the SAME connection, via mod.queryRaw (duckdb_query
// directly) so it cannot recurse back through the prepare/exec paths.

// recoverTxLocked restores autocommit on c after a failed statement. It is a
// no-op inside an explicit transaction: there the user owns COMMIT/ROLLBACK
// (DuckDB invalidates the transaction on failure; the user's ROLLBACK must
// still find it). Outside one, any transaction still open on this connection
// is the dangling autocommit transaction — ROLLBACK clears it; if nothing
// dangles the ROLLBACK fails harmlessly ("no transaction is active") and the
// error is ignored. Callers must hold c.mu.
func (c *conn) recoverTxLocked() {
	if c.inTx || c.closed {
		return
	}
	_, _ = c.mod.queryRaw(c.con, "ROLLBACK")
}

// noteTxSuccessLocked updates c.inTx after query executed SUCCESSFULLY,
// scanning every top-level statement (Exec may carry a multi-statement batch)
// for transaction-control keywords. Callers must hold c.mu.
func (c *conn) noteTxSuccessLocked(query string) {
	forEachStatement(query, func(stmt string) {
		switch txBoundary(stmt) {
		case 1:
			c.inTx = true
		case -1:
			c.inTx = false
		}
	})
}

// noteTxFailureLocked handles a FAILED execution of query: a failed
// COMMIT rolls the transaction back and a failed ROLLBACK means none was
// active, so either way no transaction remains; for anything else restore
// autocommit (no-op inside an explicit transaction). Callers must hold c.mu.
func (c *conn) noteTxFailureLocked(query string) {
	if txBoundary(query) < 0 {
		c.inTx = false
		return
	}
	c.recoverTxLocked()
}

// txBoundary classifies a statement by its leading keyword: +1 starts an
// explicit transaction (BEGIN/START), -1 ends one (COMMIT/ROLLBACK/ABORT),
// 0 otherwise.
func txBoundary(stmt string) int {
	var word string
	for _, f := range strings.Fields(stmt) {
		word = f
		break
	}
	switch strings.ToUpper(word) {
	case "BEGIN", "START":
		return 1
	case "COMMIT", "ROLLBACK", "ABORT":
		return -1
	}
	return 0
}

// forEachStatement calls fn for each ';'-separated top-level statement of
// query, skipping over single/double-quoted strings (dollar-quoting is not
// handled; transaction keywords do not hide in dollar-quoted bodies in
// practice, and a misclassification only perturbs the inTx hint).
func forEachStatement(query string, fn func(stmt string)) {
	start := 0
	for i := 0; i < len(query); i++ {
		switch query[i] {
		case '\'', '"':
			q := query[i]
			for i++; i < len(query) && query[i] != q; i++ {
			}
		case ';':
			fn(query[start:i])
			start = i + 1
		}
	}
	if start <= len(query) {
		fn(query[start:])
	}
}

// ExecContext implements driver.ExecerContext by preparing, executing, and
// destroying a one-shot statement. Argument-less multi-statement text (which
// duckdb_prepare rejects) falls back to direct duckdb_query execution.
func (c *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (res driver.Result, err error) {
	defer guardEnginePanic(&err)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.execLocked(query, args)
}

// execLocked is the shared Exec path (ExecContext, simpleExec). It prepares
// and executes query; when there are no bind args and duckdb_prepare rejected
// the text as multi-statement, it retries through mod.queryRaw (duckdb_query),
// which runs every statement and reports the last one's rows-changed count.
// QueryContext deliberately has NO such fallback: database/sql's Query is a
// single-result contract. Callers must hold c.mu.
func (c *conn) execLocked(query string, args []driver.NamedValue) (driver.Result, error) {
	st, err := c.prepareLocked(query)
	if err != nil {
		if len(args) == 0 && isMultiStatementErr(err) {
			n, qerr := c.mod.queryRaw(c.con, query)
			if qerr != nil {
				c.noteTxFailureLocked(query)
				return nil, qerr
			}
			c.noteTxSuccessLocked(query)
			return &result{rowsAffected: n}, nil
		}
		return nil, err
	}
	defer st.closeLocked()
	return st.execLocked(args)
}

// isMultiStatementErr matches duckdb_prepare's refusal of multi-statement SQL.
func isMultiStatementErr(err error) bool {
	return strings.Contains(err.Error(), "Cannot prepare multiple statements at once")
}

// QueryContext implements driver.QueryerContext by preparing and executing a
// one-shot statement; the returned Rows owns its statement and closes it on Close.
func (c *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (rws driver.Rows, err error) {
	defer guardEnginePanic(&err)
	c.mu.Lock()
	defer c.mu.Unlock()
	st, err := c.prepareLocked(query)
	if err != nil {
		return nil, err
	}
	rows, err := st.queryLocked(args)
	if err != nil {
		st.closeLocked()
		return nil, err
	}
	return &stmtRows{Rows: rows, st: st, c: c}, nil
}

// Ping implements driver.Pinger by running a trivial "SELECT 1".
func (c *conn) Ping(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return driver.ErrBadConn
	}
	st, err := c.prepareLocked("SELECT 1")
	if err != nil {
		return err
	}
	defer st.closeLocked()
	rows, err := st.queryLocked(nil)
	if err != nil {
		return err
	}
	return rows.Close()
}

// Begin implements driver.Conn.
func (c *conn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

// BeginTx implements driver.ConnBeginTx. DuckDB transactions are unnamed and do
// not honor isolation-level or read-only hints here, so they are ignored.
func (c *conn) BeginTx(_ context.Context, _ driver.TxOptions) (driver.Tx, error) {
	if err := c.simpleExec("BEGIN TRANSACTION"); err != nil {
		return nil, err
	}
	return &tx{c: c}, nil
}

// simpleExec runs an argument-less statement and discards the result, taking the
// conn lock itself. Used for transaction control SQL.
func (c *conn) simpleExec(query string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return driver.ErrBadConn
	}
	_, err := c.execLocked(query, nil)
	return err
}

// Close implements driver.Conn. It disconnects and closes the database. Both
// duckdb_disconnect and duckdb_close take a pointer to the handle, so the handle
// value is written into a memory slot whose address is passed.
func (c *conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	mod := c.mod

	conSlot := mod.allocOut(4)
	mod.writeU32(conSlot, uint32(c.con))
	mod.m.Xduckdb_disconnect(conSlot)
	mod.free(conSlot)

	// The database handle is shared when this conn came from a connector — it is
	// closed by connector.Close, not here. Only a standalone raw-Open conn owns it.
	if c.ownsDB {
		dbSlot := mod.allocOut(4)
		mod.writeU32(dbSlot, uint32(c.db))
		mod.m.Xduckdb_close(dbSlot)
		mod.free(dbSlot)
	}
	return nil
}

// stmt is a compiled duckdb_prepared_statement bound to its conn. All methods
// take the conn lock (or assume it is held, for the *Locked variants).
type stmt struct {
	c      *conn
	handle int32 // duckdb_prepared_statement
	query  string
	closed bool
}

var (
	_ driver.Stmt             = (*stmt)(nil)
	_ driver.StmtExecContext  = (*stmt)(nil)
	_ driver.StmtQueryContext = (*stmt)(nil)
)

// Close implements driver.Stmt.
func (s *stmt) Close() error {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	s.closeLocked()
	return nil
}

// closeLocked destroys the prepared statement. Callers must hold the conn lock.
func (s *stmt) closeLocked() {
	if s.closed {
		return
	}
	s.closed = true
	if s.handle != 0 {
		destroyPrepare(s.c.mod, s.handle)
		s.handle = 0
	}
}

// NumInput implements driver.Stmt, reporting the number of bind parameters.
func (s *stmt) NumInput() int {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	if s.closed || s.handle == 0 {
		return -1
	}
	return int(s.c.mod.m.Xduckdb_nparams(s.handle))
}

// Exec implements driver.Stmt.
func (s *stmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), valuesToNamed(args))
}

// ExecContext implements driver.StmtExecContext.
func (s *stmt) ExecContext(_ context.Context, args []driver.NamedValue) (res driver.Result, err error) {
	defer guardEnginePanic(&err)
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	return s.execLocked(args)
}

// Query implements driver.Stmt.
func (s *stmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), valuesToNamed(args))
}

// QueryContext implements driver.StmtQueryContext.
func (s *stmt) QueryContext(_ context.Context, args []driver.NamedValue) (rws driver.Rows, err error) {
	defer guardEnginePanic(&err)
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	return s.queryLocked(args)
}

// execLocked binds args, executes the statement, and returns the affected-row
// count. Callers must hold the conn lock.
func (s *stmt) execLocked(args []driver.NamedValue) (driver.Result, error) {
	if s.closed {
		return nil, driver.ErrBadConn
	}
	mod := s.c.mod
	if err := s.bindAll(args); err != nil {
		return nil, err
	}
	resPtr := mod.allocOut(sizeofDuckdbResult)
	rc := mod.m.Xduckdb_execute_prepared(s.handle, resPtr)
	if rc != 0 {
		err := fmt.Errorf("duckdb execute: %s", orUnknown(mod.lastError()))
		mod.m.Xduckdb_destroy_result(resPtr)
		mod.free(resPtr)
		s.c.noteTxFailureLocked(s.query)
		return nil, err
	}
	affected := mod.m.Xduckdb_rows_changed(resPtr)
	mod.m.Xduckdb_destroy_result(resPtr)
	mod.free(resPtr)
	s.c.noteTxSuccessLocked(s.query)
	return &result{rowsAffected: affected}, nil
}

// queryLocked binds args, executes the statement, and hands the result buffer to
// result.go's newRows. Callers must hold the conn lock. Ownership of resPtr (and
// its destroy/free) transfers to the returned Rows.
func (s *stmt) queryLocked(args []driver.NamedValue) (driver.Rows, error) {
	if s.closed {
		return nil, driver.ErrBadConn
	}
	mod := s.c.mod
	if err := s.bindAll(args); err != nil {
		return nil, err
	}
	resPtr := mod.allocOut(sizeofDuckdbResult)
	rc := mod.m.Xduckdb_execute_prepared(s.handle, resPtr)
	if rc != 0 {
		err := fmt.Errorf("duckdb execute: %s", orUnknown(mod.lastError()))
		mod.m.Xduckdb_destroy_result(resPtr)
		mod.free(resPtr)
		s.c.noteTxFailureLocked(s.query)
		return nil, err
	}
	s.c.noteTxSuccessLocked(s.query)
	return newRows(mod, resPtr)
}

// bindAll clears any prior bindings, then binds each NamedValue to its 1-indexed
// DuckDB parameter according to the Go dynamic type. Callers must hold the lock.
func (s *stmt) bindAll(args []driver.NamedValue) error {
	mod := s.c.mod
	mod.m.Xduckdb_clear_bindings(s.handle)
	for _, a := range args {
		// DuckDB parameters are 1-indexed; database/sql Ordinals start at 1 too.
		if err := s.bindOne(int64(a.Ordinal), a.Value); err != nil {
			return err
		}
	}
	return nil
}

// bindOne dispatches a single driver.Value to the matching duckdb_bind_* call.
func (s *stmt) bindOne(idx int64, v driver.Value) error {
	mod := s.c.mod
	var rc int32
	switch val := v.(type) {
	case nil:
		rc = mod.m.Xduckdb_bind_null(s.handle, idx)
	case int64:
		rc = mod.m.Xduckdb_bind_int64(s.handle, idx, val)
	case float64:
		rc = mod.m.Xduckdb_bind_double(s.handle, idx, val)
	case bool:
		var b int32
		if val {
			b = 1
		}
		rc = mod.m.Xduckdb_bind_boolean(s.handle, idx, b)
	case string:
		ptr := mod.cstring(val)
		rc = mod.m.Xduckdb_bind_varchar(s.handle, idx, ptr)
		mod.free(ptr)
	case []byte:
		ptr := mod.allocOut(int32(len(val)))
		copy(mod.mem()[ptr:], val)
		rc = mod.m.Xduckdb_bind_blob(s.handle, idx, ptr, int64(len(val)))
		mod.free(ptr)
	case time.Time:
		// Bind times as RFC3339Nano varchar rather than duckdb_bind_timestamp:
		// duckdb_timestamp is a by-value {int64 micros} struct whose wasm32 ABI
		// lowering is uncertain through wasm2go, so the string path is the safe
		// choice. DuckDB casts the literal to TIMESTAMP on use.
		ptr := mod.cstring(val.Format(time.RFC3339Nano))
		rc = mod.m.Xduckdb_bind_varchar(s.handle, idx, ptr)
		mod.free(ptr)
	default:
		return fmt.Errorf("duckdb: unsupported bind type %T at param %d", v, idx)
	}
	if rc != 0 {
		return fmt.Errorf("duckdb bind param %d: %s", idx, orUnknown(mod.lastError()))
	}
	return nil
}

// stmtRows wraps a Rows whose lifetime owns a one-shot statement (from
// conn.QueryContext): closing the rows also destroys that statement.
type stmtRows struct {
	driver.Rows
	st *stmt
	c  *conn
}

// Close closes the underlying rows and then the owned statement.
func (r *stmtRows) Close() error {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	err := r.Rows.Close()
	r.st.closeLocked()
	return err
}

// tx is a DuckDB transaction driven entirely by SQL (BEGIN/COMMIT/ROLLBACK).
type tx struct {
	c *conn
}

var _ driver.Tx = (*tx)(nil)

// Commit implements driver.Tx.
func (t *tx) Commit() error { return t.c.simpleExec("COMMIT") }

// Rollback implements driver.Tx.
func (t *tx) Rollback() error { return t.c.simpleExec("ROLLBACK") }

// result reports the outcome of an Exec. DuckDB has no auto-increment last-insert
// id concept exposed here, so LastInsertId is unsupported.
type result struct {
	rowsAffected int64
}

var _ driver.Result = (*result)(nil)

// LastInsertId implements driver.Result. DuckDB does not provide one.
func (r *result) LastInsertId() (int64, error) {
	return 0, errors.New("duckdb: LastInsertId is not supported")
}

// RowsAffected implements driver.Result.
func (r *result) RowsAffected() (int64, error) {
	return r.rowsAffected, nil
}

// destroyPrepare destroys a prepared statement handle. duckdb_destroy_prepare
// takes a pointer to the handle, so the value is staged in a memory slot.
func destroyPrepare(mod *module, handle int32) {
	slot := mod.allocOut(4)
	mod.writeU32(slot, uint32(handle))
	mod.m.Xduckdb_destroy_prepare(slot)
	mod.free(slot)
}

// valuesToNamed adapts the legacy positional Exec/Query path to NamedValue,
// assigning 1-based ordinals as database/sql does.
func valuesToNamed(args []driver.Value) []driver.NamedValue {
	named := make([]driver.NamedValue, len(args))
	for i, v := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return named
}
