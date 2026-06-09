// Static registration shim for DuckDB's in-tree extensions (v1.5.3).
// The libduckdb-src amalgamation is core-only: sum/avg/min/string fns live in
// the core_functions extension, and json_each / json_extract / the JSON type
// live in the json extension (which the googlesqlite backend needs because its
// UNNEST lowering rides json_each). Both are compiled in alongside the
// amalgamation (see build_fs.sh) and registered here.
// Call register_core_functions(db) once after duckdb_open.
#include "duckdb.hpp"
#include "duckdb/main/capi/capi_internal.hpp"
#include "core_functions_extension.hpp"
#include "json_extension.hpp"

extern "C" void register_core_functions(duckdb_database db) {
	auto *wrapper = reinterpret_cast<duckdb::DatabaseWrapper *>(db);
	duckdb::DuckDB duck(*wrapper->database->instance);
	duck.LoadStaticExtension<duckdb::CoreFunctionsExtension>();
	duck.LoadStaticExtension<duckdb::JsonExtension>();
}
