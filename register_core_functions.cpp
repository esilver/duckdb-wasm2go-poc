// Static registration shim for DuckDB's core_functions extension (v1.5.3).
// The libduckdb-src amalgamation is core-only: sum/avg/min/string fns live in
// the core_functions extension, which we compile in alongside and register here.
// Call register_core_functions(db) once after duckdb_open.
#include "duckdb.hpp"
#include "duckdb/main/capi/capi_internal.hpp"
#include "core_functions_extension.hpp"

extern "C" void register_core_functions(duckdb_database db) {
	auto *wrapper = reinterpret_cast<duckdb::DatabaseWrapper *>(db);
	duckdb::DuckDB duck(*wrapper->database->instance);
	duck.LoadStaticExtension<duckdb::CoreFunctionsExtension>();
}
