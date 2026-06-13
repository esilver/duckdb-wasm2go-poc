//go:build ignore

// poc.cc - a tiny standalone wasm that mimics the shape of the DuckDB C API
// surface we will drive through wasm2go-generated Go. It is built EXACTLY like
// the real DuckDB target (-sSTANDALONE_WASM -sFILESYSTEM=0 -fexceptions
// -sDISABLE_EXCEPTION_CATCHING=0) so the same Go host + shim layer that runs
// this also runs DuckDB once its wasm lands.
//
// The point of this file is to exercise EVERY moving part of the harness:
//   1. a C-string argument passed in from Go (sql), proving cstring marshalling
//   2. a normal return value read back into Go (the int64 scalar)
//   3. a C++ exception thrown across an invoke_* trampoline and CAUGHT inside
//      the wasm, proving the Emscripten exception ABI implemented in Go fires
//      (returns an error code instead of aborting the "process").
//   4. heap use (std::string / std::runtime_error) so the build actually pulls
//      in libc++ / exception runtime, the same machinery DuckDB needs.

#include <cstring>
#include <cstdint>
#include <string>
#include <stdexcept>
#include <cstdio>

extern "C" {

// db_open / db_close model duckdb_open(":memory:") / duckdb_close. There is no
// real database here; we just hand back a non-null "handle" so the Go run loop
// follows the same open -> query -> close shape it will use for DuckDB.
int db_open(const char* path, void** out_handle) {
    // Touch the path string so the argument marshalling is genuinely exercised.
    static int handle_storage = 0;
    if (path != nullptr && std::strcmp(path, ":memory:") == 0) {
        handle_storage = 0xDB;
    } else {
        handle_storage = 0x01;
    }
    if (out_handle) *out_handle = &handle_storage;
    return 0; // 0 == success, mirroring DuckDBSuccess
}

void db_close(void** handle) {
    if (handle) *handle = nullptr;
}

// scalar_or_throw is the core: it interprets a SQL string. For "SELECT 1" it
// returns 1. For anything it does not understand it THROWS std::runtime_error.
// This is deliberately a separate, EXPORTED, potentially-throwing function so
// that the wasm wraps the call site in an invoke_* trampoline -> the Go host's
// trampoline + __cxa_throw + find_matching_catch path is what unwinds it.
long long scalar_or_throw(const char* sql) {
    std::string q = (sql ? sql : "");
    // Trim a trailing semicolon/space the way a real driver might.
    while (!q.empty() && (q.back() == ';' || q.back() == ' ')) q.pop_back();
    if (q == "SELECT 1") return 1;
    if (q == "SELECT 42") return 42;
    throw std::runtime_error(std::string("unknown query: ") + q);
}

// query_scalar is the CAUGHT wrapper, modeling duckdb_query returning an error
// state rather than crashing. It returns the scalar via out_val and a status:
//   0  -> success, *out_val holds the value
//   1  -> a std::exception was caught (bad query); *out_err points to the
//         exception's what() message inside wasm memory (Go can read it).
// Catching here forces the FULL ABI: throw -> invoke trampoline sets threw ->
// landing pad -> __cxa_find_matching_catch -> __cxa_begin_catch -> what() ->
// __cxa_end_catch.
int query_scalar(const char* sql, long long* out_val, const char** out_err) {
    static std::string last_err; // keep what() alive for Go to read back
    try {
        long long v = scalar_or_throw(sql);
        if (out_val) *out_val = v;
        if (out_err) *out_err = nullptr;
        return 0;
    } catch (const std::exception& e) {
        last_err = e.what();
        if (out_err) *out_err = last_err.c_str();
        if (out_val) *out_val = 0;
        return 1;
    }
}

// echo_len is a trivial extra export to double-check plain (non-throwing)
// C-string marshalling independent of the exception path.
int echo_len(const char* s) {
    return s ? (int)std::strlen(s) : -1;
}

} // extern "C"
