package duckdbcompat

import (
	"database/sql/driver"

	convergeduckdb "duckdbconverge/duckdb"
)

// Driver is the compat mirror of duckdb-go's duckdb.Driver. It is the only direct
// data-path API the emulator calls (driver.go:177:
// `duckdb.Driver{}.Open(duckdbDSN(name))`). Open delegates straight to our pure-Go
// engine driver, so the DSN, connection-sharing, and database/sql plumbing are all
// the engine's (which already shares one in-memory database across every
// connection minted from a connector, matching the emulator's assumption).
//
// The empty struct carries no state and is constructed by value (duckdb.Driver{}),
// matching duckdb-go.
type Driver struct{}

// Compile-time check that Driver satisfies driver.Driver.
var _ driver.Driver = Driver{}

// Open implements driver.Driver by forwarding to the engine driver's Open. An
// empty dsn is handled by the engine (it maps "" to an in-memory database).
func (Driver) Open(dsn string) (driver.Conn, error) {
	return (&convergeduckdb.Driver{}).Open(dsn)
}
