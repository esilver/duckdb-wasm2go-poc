package duckdbcompat

import (
	"database/sql"
	"fmt"
	"strings"

	convergeduckdb "duckdbconverge/duckdb"
)

// This file is the compat mirror of the duckdb-go table-UDF surface (SWAP-BLUEPRINT
// §1.4 — Surface 4). The emulator only ever registers two TVFs (`appends`,
// `changes`), both of which are degenerate zero-row stubs: their RowTableSource
// reports Cardinality {0, true} and FillRow always returns (false, nil). Faithful
// row-producing table functions are out of scope.
//
// Because no rows are ever produced, we do not implement DuckDB's
// create_table_function C-API at all. Instead RegisterTableUDF runs a plain
// `CREATE TABLE <name>(<col> <sqltype>, ...)` against the connection: a freshly
// created table with no INSERT is an empty queryable relation with exactly the
// schema the TVF would have produced — sufficient for the emulator's needs.

// Row is the placeholder element type passed to RowTableSource.FillRow. The
// emulator never writes into it (FillRow returns false immediately), so it carries
// no structure. It mirrors duckdb-go's Row type so call sites compile unchanged.
type Row any

// ColumnInfo describes one output column of a table function: its name and its
// logical type (carried as the opaque compat TypeInfo). The shim consults only
// these two fields to build the CREATE TABLE column list.
type ColumnInfo struct {
	Name string
	T    TypeInfo
}

// CardinalityInfo is the duckdb-go cardinality hint. The shim never consults it
// (the emulator's TVFs always report {0, true}); it exists so RowTableSource's
// method set matches duckdb-go exactly.
type CardinalityInfo struct {
	Cardinality uint
	Exact       bool
}

// RowTableSource is the per-bind row producer. Its four-method set mirrors
// duckdb-go. The shim consults only ColumnInfos() (to derive the table schema);
// Cardinality/Init/FillRow are part of the contract but unused here because the
// shim materializes an empty table rather than driving the row loop.
type RowTableSource interface {
	ColumnInfos() []ColumnInfo
	Cardinality() *CardinalityInfo
	Init()
	FillRow(Row) (bool, error)
}

// RowTableFunction is the compat mirror of duckdb-go's RowTableFunction. Only the
// BindArguments field is used: the shim invokes it once (with nil named args and
// no positional args) to obtain a RowTableSource whose ColumnInfos() defines the
// table schema.
type RowTableFunction struct {
	BindArguments func(named map[string]any, args ...any) (RowTableSource, error)
}

// RegisterTableUDF registers the named table function f on the connection con.
//
// Since the emulator's TVFs are zero-row stubs, this does not install a real
// table function. It instead binds f (with no arguments) to read the output
// schema and emits a `CREATE TABLE <name>(...)` so that `SELECT ... FROM <name>`
// resolves against an empty relation with the correct columns.
func RegisterTableUDF(con *sql.Conn, name string, f RowTableFunction) error {
	if f.BindArguments == nil {
		return fmt.Errorf("duckdbcompat: RegisterTableUDF %q: nil BindArguments", name)
	}

	src, err := f.BindArguments(nil)
	if err != nil {
		return fmt.Errorf("duckdbcompat: RegisterTableUDF %q: BindArguments: %w", name, err)
	}
	if src == nil {
		return fmt.Errorf("duckdbcompat: RegisterTableUDF %q: BindArguments returned nil source", name)
	}

	cols := src.ColumnInfos()
	if len(cols) == 0 {
		return fmt.Errorf("duckdbcompat: RegisterTableUDF %q: no columns", name)
	}

	var b strings.Builder
	// IF NOT EXISTS: the stub persists in file-backed databases, so reopening a
	// DB created by an earlier process must not fail the boot-time registration.
	b.WriteString("CREATE TABLE IF NOT EXISTS ")
	b.WriteString(quoteIdent(name))
	b.WriteString(" (")
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(quoteIdent(c.Name))
		b.WriteByte(' ')
		b.WriteString(sqlTypeName(c.T))
	}
	b.WriteByte(')')

	if err := convergeduckdb.ExecConn(con, b.String()); err != nil {
		return fmt.Errorf("duckdbcompat: RegisterTableUDF %q: %w", name, err)
	}
	return nil
}

// quoteIdent wraps a SQL identifier in double quotes, escaping any embedded double
// quote by doubling it (DuckDB / standard-SQL identifier quoting). This both lets
// names with special characters through and prevents the column/table names from
// being interpreted as SQL.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// sqlTypeName maps a compat TypeInfo to the DuckDB SQL type-name keyword used in
// the CREATE TABLE column list. Only the types the emulator's two TVFs use need a
// distinct mapping; anything unrecognized (including the zero-value/INVALID type)
// falls back to VARCHAR, which is the safe lossless envelope the value layer uses.
func sqlTypeName(ti TypeInfo) string {
	switch Type(ti.typeID()) {
	case TYPE_BOOLEAN:
		return "BOOLEAN"
	case TYPE_BIGINT:
		return "BIGINT"
	case TYPE_DOUBLE:
		return "DOUBLE"
	case TYPE_TIMESTAMP:
		return "TIMESTAMP"
	case TYPE_BLOB:
		return "BLOB"
	case TYPE_VARCHAR:
		return "VARCHAR"
	default:
		return "VARCHAR"
	}
}
