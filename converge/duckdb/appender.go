// appender.go — bulk-ingestion Appender (the DuckDB Appender shape, issue #1 lever 2).
//
// WHY NOT duckdb_appender_create: this wasm build's export list (exports_arg.txt)
// carries the duckdb_append_* VALUE family (append_int64/double/varchar/...) but
// NONE of the appender lifecycle symbols — duckdb_appender_create / _end_row /
// _flush / _close / _destroy / _error are absent from the generated engine, so a
// handle can never be obtained and the value family is unreachable. (Verified:
// `grep -c appender converge/genpkg/gen.go` == 0.)
//
// INSTEAD the Appender is backed by a registered Go TABLE FUNCTION (that C-API
// family IS fully exported and uses the same proven inject/call_indirect
// trampoline as the scalar/aggregate UDFs): AppendRow buffers rows in Go, and
// Flush executes
//
//	INSERT INTO <table> SELECT * FROM "<feed-fn>"()
//
// where the feed function streams the buffered rows into the engine one output
// chunk (2048 rows) at a time via the shared writeCell encoder. This bypasses
// per-row statement execution entirely — one statement per flush, one trampoline
// call per 2048 rows — which is the property the real Appender provides.
//
// Concurrency: an Appender is bound to one *sql.Conn and is NOT safe for
// concurrent use. AppendRow only touches the Go-side buffer; Flush/Close take
// the connection's engine lock (the withConn idiom from udf_register.go).
package duckdb

import (
	"database/sql"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// appenderSeq numbers the per-Appender feed table functions (registration is
// catalog-scoped and DuckDB has no unregister API, so each Appender gets a
// unique, collision-free name).
var appenderSeq atomic.Uint64

// appenderCol is one target-table column: its name and duckdb_type id.
type appenderCol struct {
	name   string
	typeID int32
}

// Appender is a bulk-load channel into one table, mirroring the C API's
// duckdb_appender surface (AppendRow / Flush / Close). Rows accumulate in Go
// memory until Flush (or Close) pushes them into the table in one statement.
//
// Supported column types (the writeCell encoder's flat set): BOOLEAN, all
// signed/unsigned integers up to BIGINT, FLOAT, DOUBLE, VARCHAR, BLOB, DATE,
// TIMESTAMP, TIMESTAMPTZ. NewAppender rejects tables with other column types.
type Appender struct {
	c         *sql.Conn
	qualified string // quoted schema.table target
	tfName    string // the registered feed table function's name
	cols      []appenderCol
	rows      [][]any
	cursor    int   // scan position while a flush's INSERT..SELECT is running
	scanErr   error // first writeCell error raised inside the feed callback
	closed    bool
}

// NewAppender creates an Appender for schema.table (schema "" means the default
// search path) over c. It introspects the table's columns, validates every
// column type is appendable, and registers the Appender's private feed table
// function on the engine behind c. The Appender must be used with this same c;
// call Close when done (Close flushes pending rows).
func NewAppender(c *sql.Conn, schema, table string) (*Appender, error) {
	qualified := quoteIdent(table)
	if schema != "" {
		qualified = quoteIdent(schema) + "." + qualified
	}
	a := &Appender{
		c:         c,
		qualified: qualified,
		tfName:    fmt.Sprintf("__go_appender_%d", appenderSeq.Add(1)),
	}
	err := withConn(c, func(co *conn) error {
		if co.closed {
			return fmt.Errorf("duckdb: appender: connection is closed")
		}
		cols, err := tableColumns(co, qualified)
		if err != nil {
			return err
		}
		for _, col := range cols {
			if !appendableType(col.typeID) {
				return fmt.Errorf("duckdb: appender: column %q has unsupported type id %d", col.name, col.typeID)
			}
		}
		a.cols = cols
		return a.registerFeedLocked(co)
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

// AppendRow buffers one row. vals must carry exactly one value per table column,
// in table column order; nil appends SQL NULL. Values are type-checked eagerly
// against the column types (same coercions as the UDF codec: integers from any
// Go int kind, VARCHAR from string/[]byte, DATE/TIMESTAMP from time.Time).
// The vals slice is retained until the next Flush — do not mutate it after.
func (a *Appender) AppendRow(vals ...any) error {
	if a.closed {
		return fmt.Errorf("duckdb: appender: append after Close")
	}
	if len(vals) != len(a.cols) {
		return fmt.Errorf("duckdb: appender: row has %d values, table %s has %d columns", len(vals), a.qualified, len(a.cols))
	}
	for i, v := range vals {
		if err := checkAppendValue(a.cols[i], v); err != nil {
			return err
		}
	}
	a.rows = append(a.rows, vals)
	return nil
}

// Flush pushes all buffered rows into the table in one INSERT..SELECT over the
// feed table function and clears the buffer. On error the buffer is kept so the
// caller can inspect or retry. A Flush with nothing buffered is a no-op.
func (a *Appender) Flush() error {
	if a.closed {
		return fmt.Errorf("duckdb: appender: flush after Close")
	}
	return a.flush()
}

// Close flushes pending rows and retires the Appender; further AppendRow/Flush
// calls error. Close is idempotent. (The feed table function necessarily stays
// registered in the catalog — the C API has no unregister — but it is inert and
// returns zero rows once the Appender is closed.)
func (a *Appender) Close() error {
	if a.closed {
		return nil
	}
	err := a.flush()
	a.closed = true
	a.rows = nil
	return err
}

func (a *Appender) flush() error {
	if len(a.rows) == 0 {
		return nil
	}
	want := int64(len(a.rows))
	insertSQL := "INSERT INTO " + a.qualified + " SELECT * FROM " + quoteIdent(a.tfName) + "()"
	err := withConn(a.c, func(co *conn) error {
		if co.closed {
			return fmt.Errorf("duckdb: appender: connection is closed")
		}
		a.scanErr = nil
		st, err := co.prepareLocked(insertSQL)
		if err != nil {
			return err
		}
		defer st.closeLocked()
		res, err := st.execLocked(nil)
		if err != nil {
			if a.scanErr != nil {
				return fmt.Errorf("duckdb: appender flush: %w (engine: %v)", a.scanErr, err)
			}
			return err
		}
		if n, _ := res.RowsAffected(); n != want {
			return fmt.Errorf("duckdb: appender flush: inserted %d of %d buffered rows", n, want)
		}
		return nil
	})
	if err != nil {
		return err
	}
	a.rows = a.rows[:0]
	return nil
}

// registerFeedLocked builds and registers the Appender's feed table function.
// The three callbacks are injected into the engine's indirect-function table
// exactly like the scalar/aggregate UDF callbacks (module.inject); their Go
// signatures mirror the C typedefs' wasm shapes:
//
//	bind:     void (*)(duckdb_bind_info)                      -> func(int32)
//	init:     void (*)(duckdb_init_info)                      -> func(int32)
//	function: void (*)(duckdb_function_info, duckdb_data_chunk) -> func(int32, int32)
//
// Callers must hold the engine lock (withConn provides it).
func (a *Appender) registerFeedLocked(co *conn) error {
	mod := co.mod
	m := mod.m

	// bind: declare one result column per table column (this runs once per
	// flush's INSERT..SELECT, when the statement is bound).
	bindFn := func(info int32) {
		for _, col := range a.cols {
			lt := m.Xduckdb_create_logical_type(col.typeID)
			namePtr := mod.cstring(col.name)
			m.Xduckdb_bind_add_result_column(info, namePtr, lt)
			mod.free(namePtr)
			destroyLogicalType(mod, lt)
		}
		m.Xduckdb_bind_set_cardinality(info, int64(len(a.rows)), 1)
	}

	// init: rewind the scan over the buffered rows.
	initFn := func(info int32) {
		a.cursor = 0
	}

	// function: emit the next chunk of buffered rows (up to the engine's vector
	// size, 2048). Setting the chunk size to 0 ends the scan.
	fillFn := func(info, out int32) {
		remaining := len(a.rows) - a.cursor
		n := int(m.Xduckdb_vector_size())
		if remaining < n {
			n = remaining
		}
		if n <= 0 {
			m.Xduckdb_data_chunk_set_size(out, 0)
			return
		}
		for c, col := range a.cols {
			vec := m.Xduckdb_data_chunk_get_vector(out, int64(c))
			data := m.Xduckdb_vector_get_data(vec)
			for r := 0; r < n; r++ {
				if err := mod.writeCell(col.typeID, vec, data, int64(r), a.rows[a.cursor+r][c]); err != nil {
					a.scanErr = fmt.Errorf("row %d, column %q: %w", a.cursor+r, col.name, err)
					mod.setTableFunctionError(info, a.scanErr.Error())
					return
				}
			}
		}
		a.cursor += n
		m.Xduckdb_data_chunk_set_size(out, int64(n))
	}

	tf := m.Xduckdb_create_table_function()
	defer destroyTableFunction(mod, tf)
	namePtr := mod.cstring(a.tfName)
	m.Xduckdb_table_function_set_name(tf, namePtr)
	mod.free(namePtr)
	m.Xduckdb_table_function_set_bind(tf, mod.inject(bindFn))
	m.Xduckdb_table_function_set_init(tf, mod.inject(initFn))
	m.Xduckdb_table_function_set_function(tf, mod.inject(fillFn))
	if rc := m.Xduckdb_register_table_function(co.con, tf); rc != 0 {
		return fmt.Errorf("duckdb: appender: registering feed function %q failed (rc=%d): %s",
			a.tfName, rc, orUnknown(mod.lastError()))
	}
	return nil
}

// tableColumns introspects qualified's column names and type ids by executing
// `SELECT * FROM <qualified> LIMIT 0` and reading the result's column metadata
// (duckdb_column_count/_name/_type — the same accessors result.go proves daily).
// Callers must hold the engine lock.
func tableColumns(co *conn, qualified string) ([]appenderCol, error) {
	mod := co.mod
	st, err := co.prepareLocked("SELECT * FROM " + qualified + " LIMIT 0")
	if err != nil {
		return nil, fmt.Errorf("duckdb: appender: cannot open table %s: %w", qualified, err)
	}
	defer st.closeLocked()

	resPtr := mod.allocOut(sizeofDuckdbResult)
	defer func() {
		mod.m.Xduckdb_destroy_result(resPtr)
		mod.free(resPtr)
	}()
	if rc := mod.m.Xduckdb_execute_prepared(st.handle, resPtr); rc != 0 {
		return nil, engineErr("appender introspect", mod.resultError(resPtr))
	}
	n := mod.m.Xduckdb_column_count(resPtr)
	cols := make([]appenderCol, n)
	for i := int64(0); i < n; i++ {
		cols[i] = appenderCol{
			name:   mod.goString(mod.m.Xduckdb_column_name(resPtr, i)),
			typeID: mod.m.Xduckdb_column_type(resPtr, i),
		}
	}
	return cols, nil
}

// appendableType reports whether writeCell can encode into a column of typeID.
func appendableType(typeID int32) bool {
	switch typeID {
	case dtBoolean,
		dtTinyint, dtSmallint, dtInteger, dtBigint,
		dtUtinyint, dtUsmallint, dtUinteger, dtUbigint,
		dtFloat, dtDouble,
		dtVarchar, dtBlob,
		dtDate, dtTimestamp, dtTimestampTz:
		return true
	}
	return false
}

// checkAppendValue eagerly validates that v can encode into col (the same
// coercions writeCell applies at flush time), so type errors surface at
// AppendRow with row context instead of mid-flush.
func checkAppendValue(col appenderCol, v any) error {
	if v == nil {
		return nil
	}
	ok := false
	switch col.typeID {
	case dtBoolean:
		_, ok = asBool(v)
	case dtTinyint, dtSmallint, dtInteger, dtBigint,
		dtUtinyint, dtUsmallint, dtUinteger, dtUbigint:
		_, ok = asInt64(v)
	case dtFloat, dtDouble:
		_, ok = asFloat64(v)
	case dtVarchar:
		_, ok = asString(v)
	case dtBlob:
		_, ok = asBytes(v)
	case dtDate, dtTimestamp, dtTimestampTz:
		_, ok = v.(time.Time)
	}
	if !ok {
		return fmt.Errorf("duckdb: appender: cannot append %T into column %q (type id %d)", v, col.name, col.typeID)
	}
	return nil
}

// quoteIdent double-quotes a SQL identifier, doubling embedded quotes.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
