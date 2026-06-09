package main

import (
	"encoding/binary"
	"encoding/json"
	"strings"

	core "duckdbconverge/genpkg"
)

// sizeofDuckdbResult over-allocates duckdb_result (see main.go in converge/).
const sizeofDuckdbResult = 256

// cstring writes s+NUL into module memory, returning the offset (a C char*).
func cstring(m *core.Module, s string) int32 {
	ptr := m.Xmalloc(int32(len(s) + 1))
	if ptr == 0 {
		panic("malloc returned null")
	}
	mem := *m.Xmemory().Slice()
	copy(mem[ptr:], s)
	mem[ptr+int32(len(s))] = 0
	return ptr
}

// goString reads a NUL-terminated C string from module memory.
func goString(m *core.Module, ptr int32) string {
	if ptr == 0 {
		return ""
	}
	mem := *m.Xmemory().Slice()
	end := ptr
	for int(end) < len(mem) && mem[end] != 0 {
		end++
	}
	return string(mem[ptr:end])
}

// allocOut reserves n zeroed bytes (out-params, result structs).
func allocOut(m *core.Module, n int32) int32 {
	ptr := m.Xmalloc(n)
	mem := *m.Xmemory().Slice()
	for i := int32(0); i < n; i++ {
		mem[ptr+i] = 0
	}
	return ptr
}

// allocOutWith writes one pointer value into a fresh 4-byte slot (disconnect/close).
func allocOutWith(m *core.Module, v int32) int32 {
	ptr := allocOut(m, 4)
	binary.LittleEndian.PutUint32((*m.Xmemory().Slice())[ptr:], uint32(v))
	return ptr
}

func readPtr(m *core.Module, ptr int32) int32 {
	mem := *m.Xmemory().Slice()
	return int32(binary.LittleEndian.Uint32(mem[ptr:]))
}

// extractMsg pulls the readable message out of DuckDB's JSON error envelope,
// falling back to the raw text.
func extractMsg(raw string) string {
	if raw == "" {
		return "unknown error"
	}
	var e struct {
		Type string `json:"exception_type"`
		Msg  string `json:"exception_message"`
	}
	if json.Unmarshal([]byte(raw), &e) == nil && e.Msg != "" {
		msg := strings.SplitN(e.Msg, "\n", 2)[0]
		if e.Type != "" {
			return e.Type + " Error: " + msg
		}
		return msg
	}
	return raw
}
