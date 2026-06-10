// result.go — reads a DuckDB query result into Go driver.Values using DuckDB's
// modern data-chunk / vector C API, for the pure-Go (CGO_ENABLED=0) wasm2go
// driver. It owns the database/sql/driver.Rows surface for a result.
//
// Streaming model: a duckdb_result is read one duckdb_data_chunk at a time via
// duckdb_fetch_chunk. Each chunk holds up to STANDARD_VECTOR_SIZE (2048) rows
// across the result's columns; each column is a duckdb_vector with a flat data
// buffer plus an optional validity bitmask. rows.Next walks rows within the
// current chunk, fetching the next chunk when the cursor exhausts it.
//
// ABI notes (see harness/PLUGIN.md):
//   - Several accessors (duckdb_column_count/name/logical_type, duckdb_fetch_chunk,
//     duckdb_destroy_result, ...) take a duckdb_result BY VALUE. Emscripten lowers a
//     by-value struct argument to a POINTER to the struct, so the generated method
//     takes the result POINTER (resPtr) directly — exactly as main.go calls
//     Xduckdb_column_count(resPtr)/Xduckdb_row_count(resPtr). We follow that pattern.
//   - idx_t is int64 in the generated ABI; handles/pointers are int32 offsets into
//     module linear memory; column data is read out of mod.mem().
package duckdb

import (
	"database/sql/driver"
	"io"
	"math"
	"math/big"
	"strconv"
	"time"
)

// duckdb_type enum values we decode (from amalg/duckdb.h). Kept local so result.go
// does not depend on any generated constant table.
const (
	dtInvalid     = 0
	dtBoolean     = 1
	dtTinyint     = 2
	dtSmallint    = 3
	dtInteger     = 4
	dtBigint      = 5
	dtUtinyint    = 6
	dtUsmallint   = 7
	dtUinteger    = 8
	dtUbigint     = 9
	dtFloat       = 10
	dtDouble      = 11
	dtTimestamp   = 12 // micros since epoch
	dtDate        = 13 // days since 1970-01-01
	dtTime        = 14 // micros since midnight
	dtInterval    = 15
	dtHugeint     = 16
	dtVarchar     = 17
	dtBlob        = 18
	dtDecimal     = 19
	dtTimestampS  = 20 // seconds since epoch
	dtTimestampMs = 21 // millis since epoch
	dtTimestampNs = 22 // nanos since epoch
	dtEnum        = 23
	dtUuid        = 27
	dtBit         = 29 // bitstring backed by a blob (first byte = padding bit count)
	dtTimeTz      = 30 // uint64: micros-since-midnight << 24 | encoded offset
	dtTimestampTz = 31 // micros since epoch
	dtUhugeint    = 32
	dtTimeNs      = 39 // nanos since midnight
)

// DuckDB date/timestamp ±infinity sentinels (duckdb-src/src/include/duckdb/common/
// types/timestamp.hpp + date.hpp): timestamp_t::infinity() == INT64_MAX,
// timestamp_t::ninfinity() == -INT64_MAX (NOT INT64_MIN); date_t::infinity() ==
// INT32_MAX, date_t::ninfinity() == -INT32_MAX. The same int64 sentinels apply to
// every timestamp width (s/ms/us/ns share timestamp_t's storage).
const (
	tsInfinity      = math.MaxInt64
	tsNegInfinity   = -math.MaxInt64
	dateInfinity    = math.MaxInt32
	dateNegInfinity = -math.MaxInt32
)

// epoch is 1970-01-01 UTC, the reference point for DATE/TIMESTAMP/TIME values.
var epoch = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)

// rows implements database/sql/driver.Rows for one duckdb_result, streamed chunk
// by chunk through duckdb_fetch_chunk.
//
// Ownership: driver.go allocates resPtr (the duckdb_result struct buffer) and runs
// the query into it, then hands it to newRows. From that point rows.Close is
// responsible for releasing the engine-side result (duckdb_destroy_result) AND
// freeing the resPtr buffer (mod.free). driver.go must NOT free resPtr itself once
// newRows has succeeded.
type rows struct {
	mod    *module
	resPtr int32 // duckdb_result* (the by-value struct buffer in module memory)

	names   []string
	typeIDs []int32 // duckdb_type id per column
	// colJSON[col] marks a VARCHAR-backed column whose logical type carries the
	// "JSON" alias; its cells scan as the PARSED native Go value (duckdb-go
	// semantics, see DecodeJSONNative) rather than the raw JSON text.
	colJSON []bool
	// decimalMeta[col] is set for DECIMAL columns: width/scale + the internal
	// storage type id used to read the raw integer.
	decimalMeta map[int]decimalInfo
	// enumMeta[col] is set for ENUM columns: the unsigned integer type backing
	// the per-row dictionary INDEXES plus the dictionary strings themselves
	// (snapshotted once from the column's logical type — immutable for the
	// result's lifetime). ENUM vectors do NOT hold string_t cells.
	enumMeta map[int]enumInfo

	chunk    int32 // current duckdb_data_chunk handle (0 = none held)
	chunkLen int   // rows in the current chunk
	cursor   int   // next row index within the current chunk
	// nestedDecs lazily caches a vecDecoder per nested-typed (LIST/STRUCT/MAP)
	// column for the CURRENT chunk (nested cells need the vector handle to reach
	// child vectors, which the flat decode path doesn't carry). Reset whenever
	// the chunk is released.
	nestedDecs map[int]*vecDecoder

	closed bool
}

type decimalInfo struct {
	width    uint8
	scale    uint8
	internal int32 // duckdb_type id of the backing integer (SMALLINT/INTEGER/BIGINT/HUGEINT)
}

// enumInfo is the decode metadata for one ENUM column: the duckdb_type id of
// the unsigned integer holding each row's dictionary index (UTINYINT for <=255
// entries, USMALLINT for <=65535, UINTEGER beyond), and the dictionary values.
type enumInfo struct {
	internal int32 // dtUtinyint / dtUsmallint / dtUinteger
	dict     []string
}

// readEnumMeta snapshots an ENUM logical type's decode metadata. Dictionary
// value strings are malloc'd by the engine and must be freed with duckdb_free.
func readEnumMeta(mod *module, lt int32) enumInfo {
	info := enumInfo{internal: mod.m.Xduckdb_enum_internal_type(lt)}
	n := int(uint32(mod.m.Xduckdb_enum_dictionary_size(lt)))
	info.dict = make([]string, n)
	for i := 0; i < n; i++ {
		sp := mod.m.Xduckdb_enum_dictionary_value(lt, int64(i))
		if sp != 0 {
			info.dict[i] = mod.goString(sp)
			mod.m.Xduckdb_free(sp)
		}
	}
	return info
}

// value reads the row'th dictionary index out of an ENUM vector's flat data
// buffer (stride = the backing unsigned integer's width) and returns the
// dictionary string. Out-of-range indexes / unknown backing types yield nil.
func (info enumInfo) value(mod *module, dataPtr int32, row int) driver.Value {
	var idx int
	switch info.internal {
	case dtUtinyint:
		idx = int(mod.mem()[dataPtr+int32(row)])
	case dtUsmallint:
		idx = int(uint16(mod.readU32(dataPtr + int32(row*2))))
	case dtUinteger:
		idx = int(mod.readU32(dataPtr + int32(row*4)))
	default:
		return nil
	}
	if idx < 0 || idx >= len(info.dict) {
		return nil
	}
	return info.dict[idx]
}

// newRows reads the result's column metadata and returns a streaming driver.Rows.
// It takes ownership of resPtr (see rows ownership comment).
func newRows(mod *module, resPtr int32) (driver.Rows, error) {
	n := int(mod.m.Xduckdb_column_count(resPtr)) // by-value result -> resPtr directly
	r := &rows{
		mod:         mod,
		resPtr:      resPtr,
		names:       make([]string, n),
		typeIDs:     make([]int32, n),
		colJSON:     make([]bool, n),
		decimalMeta: map[int]decimalInfo{},
		enumMeta:    map[int]enumInfo{},
	}
	for col := 0; col < n; col++ {
		// duckdb_column_name(result, idx_t col) -> const char* (do not free).
		namePtr := mod.m.Xduckdb_column_name(resPtr, int64(col))
		r.names[col] = mod.goString(namePtr)

		// duckdb_column_logical_type(result, idx_t col) -> duckdb_logical_type handle.
		// Must be destroyed with duckdb_destroy_logical_type.
		lt := mod.m.Xduckdb_column_logical_type(resPtr, int64(col))
		tid := mod.m.Xduckdb_get_type_id(lt)
		r.typeIDs[col] = tid
		if tid == dtDecimal {
			r.decimalMeta[col] = decimalInfo{
				width:    uint8(mod.m.Xduckdb_decimal_width(lt)),
				scale:    uint8(mod.m.Xduckdb_decimal_scale(lt)),
				internal: mod.m.Xduckdb_decimal_internal_type(lt),
			}
		}
		if tid == dtVarchar {
			// The JSON alias is the only way to tell a JSON column apart from a
			// plain VARCHAR (JSON is VARCHAR-backed).
			if ap := mod.m.Xduckdb_logical_type_get_alias(lt); ap != 0 {
				r.colJSON[col] = mod.goString(ap) == "JSON"
				mod.m.Xduckdb_free(ap)
			}
		}
		if tid == dtEnum {
			r.enumMeta[col] = readEnumMeta(mod, lt)
		}
		// Free the logical type handle (pointer-to-handle arg, like destroy_result).
		ltSlot := mod.allocOut(4)
		mod.writeU32(ltSlot, uint32(lt))
		mod.m.Xduckdb_destroy_logical_type(ltSlot)
		mod.free(ltSlot)
	}
	return r, nil
}

// Columns returns the column names in result order.
func (r *rows) Columns() []string { return r.names }

// Close releases the current chunk (if any), destroys the engine-side result, and
// frees the resPtr buffer. Safe to call more than once.
func (r *rows) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	r.releaseChunk()
	// duckdb_destroy_result(duckdb_result*) — by-value result lowered to pointer,
	// so the generated method takes resPtr directly.
	r.mod.m.Xduckdb_destroy_result(r.resPtr)
	r.mod.free(r.resPtr)
	r.resPtr = 0
	return nil
}

// releaseChunk destroys the held data chunk, if any. duckdb_destroy_data_chunk
// takes a duckdb_data_chunk* (pointer to the handle), so we write the handle into a
// scratch slot and pass its address.
func (r *rows) releaseChunk() {
	if r.chunk == 0 {
		return
	}
	slot := r.mod.allocOut(4)
	r.mod.writeU32(slot, uint32(r.chunk))
	r.mod.m.Xduckdb_destroy_data_chunk(slot)
	r.mod.free(slot)
	r.chunk = 0
	r.chunkLen = 0
	r.cursor = 0
	r.nestedDecs = nil
}

// Next decodes the next row into dest. Returns io.EOF when the result is drained.
func (r *rows) Next(dest []driver.Value) (err error) {
	defer guardEnginePanic(&err)
	mod := r.mod
	// Advance to a chunk that still has rows.
	for r.chunk == 0 || r.cursor >= r.chunkLen {
		r.releaseChunk()
		// duckdb_fetch_chunk(duckdb_result) -> duckdb_data_chunk. By-value result
		// lowered to a pointer, so this takes resPtr directly and returns the chunk
		// handle (int32; 0 == NULL == end of result).
		chunk := mod.m.Xduckdb_fetch_chunk(r.resPtr)
		if chunk == 0 {
			return io.EOF
		}
		size := int(mod.m.Xduckdb_data_chunk_get_size(chunk))
		if size == 0 {
			// Empty chunk: release and try the next one.
			r.chunk = chunk
			r.releaseChunk()
			continue
		}
		r.chunk = chunk
		r.chunkLen = size
		r.cursor = 0
	}

	row := r.cursor
	r.cursor++

	for col := 0; col < len(r.typeIDs); col++ {
		if col >= len(dest) {
			break
		}
		// vector = duckdb_data_chunk_get_vector(chunk, col)
		vec := mod.m.Xduckdb_data_chunk_get_vector(r.chunk, int64(col))
		dataPtr := mod.m.Xduckdb_vector_get_data(vec)      // int32 offset into mem
		validPtr := mod.m.Xduckdb_vector_get_validity(vec) // int32; 0 => all valid

		if !rowValid(mod, validPtr, row) {
			dest[col] = nil
			continue
		}
		switch r.typeIDs[col] {
		case dtList, dtStruct, dtMap, dtUnion, dtArray:
			// Nested result column (LIST/STRUCT/MAP/UNION/ARRAY, incl. arbitrary
			// nesting): decode recursively via vecDecoder — LIST/ARRAY -> []any,
			// STRUCT -> Struct (declared field order), MAP -> MapValue (entry
			// order, unhashable keys OK), UNION -> the active member's value,
			// the same shapes the UDF argument path delivers (duckdbcompat
			// converts the carriers to duckdb-go's map shapes).
			d := r.nestedDecs[col]
			if d == nil {
				d = mod.newVecDecoder(vec)
				if r.nestedDecs == nil {
					r.nestedDecs = make(map[int]*vecDecoder)
				}
				r.nestedDecs[col] = d
			}
			dest[col] = d.cell(int64(row))
			continue
		}
		dest[col] = r.decode(col, dataPtr, row)
	}
	return nil
}

// rowValid reports whether row is non-NULL. validPtr==0 means the vector has no
// validity mask (all valid). Otherwise the mask is a uint64 array; bit (row%64) of
// word (row/64) is 1 when valid.
func rowValid(mod *module, validPtr int32, row int) bool {
	if validPtr == 0 {
		return true
	}
	word := mod.readU64(validPtr + int32(8*(row/64)))
	return (word>>(uint(row)%64))&1 == 1
}

// decode reads the cell at (col, row) out of the column's flat data buffer and maps
// it to a driver.Value (int64, float64, bool, string, []byte, time.Time, or nil).
// Unknown / unsupported types fall back to a best-effort string or nil; never panics.
func (r *rows) decode(col int, dataPtr int32, row int) driver.Value {
	mod := r.mod
	mem := mod.mem()
	tid := r.typeIDs[col]

	switch tid {
	case dtBoolean:
		return mem[dataPtr+int32(row)] != 0

	case dtTinyint:
		return int64(int8(mem[dataPtr+int32(row)]))
	case dtSmallint:
		return int64(int16(mod.readU32(dataPtr + int32(row*2)))) // low 16 bits
	case dtInteger:
		return int64(int32(mod.readU32(dataPtr + int32(row*4))))
	case dtBigint:
		return mod.readI64(dataPtr + int32(row*8))

	case dtUtinyint:
		return int64(mem[dataPtr+int32(row)])
	case dtUsmallint:
		return int64(uint16(mod.readU32(dataPtr + int32(row*2))))
	case dtUinteger:
		return int64(mod.readU32(dataPtr + int32(row*4)))
	case dtUbigint:
		// uint64 may overflow int64; widen via big.Int -> string when it does.
		u := mod.readU64(dataPtr + int32(row*8))
		if u <= uint64(^uint64(0)>>1) {
			return int64(u)
		}
		return new(big.Int).SetUint64(u).String()

	case dtHugeint:
		return hugeintValue(mod, dataPtr, row, true)
	case dtUhugeint:
		return hugeintValue(mod, dataPtr, row, false)

	case dtFloat:
		return float64(mod.readF32(dataPtr + int32(row*4)))
	case dtDouble:
		return mod.readF64(dataPtr + int32(row*8))

	case dtVarchar:
		s, _ := readStringT(mod, dataPtr+int32(row*16))
		if r.colJSON[col] {
			// JSON column: deliver the parsed native value (duckdb-go semantics).
			return DecodeJSONNative(s)
		}
		return s
	case dtEnum:
		// ENUM cells are dictionary INDEXES (uint8/16/32 per the enum's size),
		// not string_t — decode the index and look up the dictionary string.
		// (Decoding these 16-byte-stride as string_t sliced garbage pointers
		// out of wasm memory: "slice bounds out of range" panics.)
		info := r.enumMeta[col]
		return info.value(mod, dataPtr, row)
	case dtBlob:
		_, b := readStringT(mod, dataPtr+int32(row*16))
		return b

	case dtDate:
		return dateValue(int32(mod.readU32(dataPtr + int32(row*4))))
	case dtTimestamp, dtTimestampTz:
		return timestampValue(mod.readI64(dataPtr+int32(row*8)), time.Microsecond)
	case dtTimestampS:
		return timestampValue(mod.readI64(dataPtr+int32(row*8)), time.Second)
	case dtTimestampMs:
		return timestampValue(mod.readI64(dataPtr+int32(row*8)), time.Millisecond)
	case dtTimestampNs:
		return timestampValue(mod.readI64(dataPtr+int32(row*8)), time.Nanosecond)
	case dtTime:
		return epoch.Add(time.Duration(mod.readI64(dataPtr+int32(row*8))) * time.Microsecond)
	case dtTimeNs:
		return epoch.Add(time.Duration(mod.readI64(dataPtr+int32(row*8))) * time.Nanosecond)
	case dtTimeTz:
		return timeTZString(mod.readU64(dataPtr + int32(row*8)))

	case dtInterval:
		return readInterval(mod, dataPtr, int64(row))

	case dtBit:
		_, b := readStringT(mod, dataPtr+int32(row*16))
		return bitString(b)

	case dtDecimal:
		return decimalValue(mod, r.decimalMeta[col], dataPtr, row)

	case dtUuid:
		// UUID is stored as a hugeint (16 bytes). Surface as its decimal-ish string
		// form via the big.Int path; callers needing canonical UUID can reparse.
		return hugeintValue(mod, dataPtr, row, true)

	case dtInvalid:
		return nil

	default:
		// Unsupported type id: don't guess a width. Return nil rather than read
		// garbage at an unknown stride.
		return nil
	}
}

// readStringT decodes a 16-byte duckdb_string_t at ptr. Layout (amalg/duckdb.h):
//
//	union {
//	  struct { uint32 length; char prefix[4]; char *ptr;   } pointer;   // length > 12
//	  struct { uint32 length; char inlined[12];            } inlined;   // length <= 12
//	}
//
// length is at offset 0. If length <= 12 the bytes are inlined at offset 4; else a
// 4-byte data pointer at offset 8 (wasm32) points to `length` bytes. Returns both
// the string and []byte views (callers pick one by column type).
func readStringT(mod *module, ptr int32) (string, []byte) {
	mem := mod.mem()
	length := int(mod.readU32(ptr))
	if length < 0 {
		return "", nil
	}
	if length <= 12 {
		b := make([]byte, length)
		copy(b, mem[ptr+4:ptr+4+int32(length)])
		return string(b), b
	}
	dataAt := mod.readPtr(ptr + 8)
	b := make([]byte, length)
	copy(b, mem[dataAt:dataAt+int32(length)])
	return string(b), b
}

// hugeintValue reads a 16-byte (u)hugeint at dataPtr[row]. lower is a uint64 at
// offset 0, upper is int64 (signed) / uint64 (unsigned) at offset 8. Returns int64
// when it fits, otherwise the decimal string of the full 128-bit value.
func hugeintValue(mod *module, dataPtr int32, row int, signed bool) driver.Value {
	base := dataPtr + int32(row*16)
	lower := mod.readU64(base)
	v := new(big.Int)
	if signed {
		upper := mod.readI64(base + 8)
		v.SetInt64(upper)
		v.Lsh(v, 64)
		v.Add(v, new(big.Int).SetUint64(lower))
	} else {
		upper := mod.readU64(base + 8)
		v.SetUint64(upper)
		v.Lsh(v, 64)
		v.Add(v, new(big.Int).SetUint64(lower))
	}
	if v.IsInt64() {
		return v.Int64()
	}
	return v.String()
}

// decimalValue reads a DECIMAL cell as an exact Decimal (unscaled big.Int +
// width/scale) — the same carrier duckdb-go delivers, which the googlesqlite
// row decoder type-switches on. The backing integer is read per the logical
// type's internal storage type.
func decimalValue(mod *module, info decimalInfo, dataPtr int32, row int) driver.Value {
	var unscaled *big.Int
	switch info.internal {
	case dtSmallint:
		unscaled = big.NewInt(int64(int16(mod.readU32(dataPtr + int32(row*2)))))
	case dtInteger:
		unscaled = big.NewInt(int64(int32(mod.readU32(dataPtr + int32(row*4)))))
	case dtBigint:
		unscaled = big.NewInt(mod.readI64(dataPtr + int32(row*8)))
	case dtHugeint:
		base := dataPtr + int32(row*16)
		lower := mod.readU64(base)
		upper := mod.readI64(base + 8)
		unscaled = big.NewInt(upper)
		unscaled.Lsh(unscaled, 64)
		unscaled.Add(unscaled, new(big.Int).SetUint64(lower))
	default:
		return nil
	}
	return Decimal{Width: info.width, Scale: info.scale, Value: unscaled}
}

// ---- INTERVAL ------------------------------------------------------------------

// Interval is a DuckDB INTERVAL value: the duckdb_interval storage struct
// (amalg duckdb.h: int32 months, int32 days, int64 micros — 16 bytes/cell).
// Its String form reproduces DuckDB's own interval->VARCHAR cast exactly
// (IntervalToStringCast::Format, duckdb-src/src/include/duckdb/common/types/
// cast_helpers.hpp), e.g. "43 years 9 months 27 days", "2 days", "00:00:01.5",
// "-1 month 00:00:00.0001".
type Interval struct {
	Months int32
	Days   int32
	Micros int64
}

// Interval micro conversions (duckdb Interval::MICROS_PER_*).
const (
	microsPerSec    = int64(1000000)
	microsPerMinute = 60 * microsPerSec
	microsPerHour   = 60 * microsPerMinute
)

// String ports IntervalToStringCast::Format faithfully:
//   - months are split into years+months; each non-zero component prints as
//     "<n> <name>" with an "s" suffix unless n is 1 or -1 ("1 year", "-1 year",
//     "2 years"), components separated by single spaces;
//   - a non-zero time part prints as [-]HH:MM:SS[.ffffff] (hours not capped at
//     two digits; micros are 6 digits with trailing zeros trimmed, min 1 digit);
//   - the all-zero interval prints "00:00:00".
func (iv Interval) String() string {
	var b []byte
	appendComponent := func(value int32, name string) {
		if value == 0 {
			return
		}
		if len(b) != 0 {
			b = append(b, ' ')
		}
		b = strconv.AppendInt(b, int64(value), 10)
		b = append(b, name...)
		if value != 1 && value != -1 {
			b = append(b, 's')
		}
	}
	if iv.Months != 0 {
		years := iv.Months / 12
		months := iv.Months - years*12
		appendComponent(years, " year")
		appendComponent(months, " month")
	}
	if iv.Days != 0 {
		appendComponent(iv.Days, " day")
	}
	if iv.Micros != 0 {
		if len(b) != 0 {
			b = append(b, ' ')
		}
		micros := iv.Micros
		if micros < 0 {
			// negative time: append the sign, then work in negative space so
			// INT64_MIN cannot overflow on negation (mirrors the C++ code).
			b = append(b, '-')
		} else {
			micros = -micros
		}
		hour := -(micros / microsPerHour)
		micros += hour * microsPerHour
		min := -(micros / microsPerMinute)
		micros += min * microsPerMinute
		sec := -(micros / microsPerSec)
		micros += sec * microsPerSec
		micros = -micros

		if hour < 10 {
			b = append(b, '0')
		}
		b = strconv.AppendInt(b, hour, 10)
		b = append(b, ':')
		b = appendTwoDigits(b, min)
		b = append(b, ':')
		b = appendTwoDigits(b, sec)
		if micros != 0 {
			b = append(b, '.')
			b = appendMicrosTrimmed(b, int32(micros))
		}
	} else if len(b) == 0 {
		return "00:00:00"
	}
	return string(b)
}

// appendTwoDigits renders 0 <= v <= 99 as exactly two digits when v < 10
// (TimeToStringCast::FormatTwoDigits); larger values print all their digits.
func appendTwoDigits(b []byte, v int64) []byte {
	if v >= 0 && v < 10 {
		b = append(b, '0')
	}
	return strconv.AppendInt(b, v, 10)
}

// appendMicrosTrimmed renders 0 <= micros <= 999999 as 6 zero-padded digits with
// trailing zeros removed, keeping at least one digit (TimeToStringCast::FormatMicros
// trims indexes 5..1, never index 0: 500000 -> "5", 100 -> "0001").
func appendMicrosTrimmed(b []byte, micros int32) []byte {
	var buf [6]byte
	for i := 5; i >= 0; i-- {
		buf[i] = byte('0' + micros%10)
		micros /= 10
	}
	n := 6
	for n > 1 && buf[n-1] == '0' {
		n--
	}
	return append(b, buf[:n]...)
}

// readInterval reads the 16-byte duckdb_interval at dataPtr[row]:
// int32 months @0, int32 days @4, int64 micros @8 (amalg duckdb.h).
func readInterval(mod *module, dataPtr int32, row int64) Interval {
	base := dataPtr + int32(row*16)
	return Interval{
		Months: int32(mod.readU32(base)),
		Days:   int32(mod.readU32(base + 4)),
		Micros: mod.readI64(base + 8),
	}
}

// ---- TIMETZ / BIT / infinity-aware DATE & TIMESTAMP ------------------------------

// timeTZString renders a TIMETZ cell. Storage (duckdb.h + datetime.hpp dtime_tz_t):
// one uint64 whose upper 40 bits are micros-since-midnight and lower 24 bits the
// REVERSED, BIASED utc offset: stored = MAX_OFFSET - offset, MAX_OFFSET = 57599
// (= 15:59:59). Rendering ports StringCastTZ::Operation(dtime_tz_t)
// (duckdb-src/src/common/operator/string_cast.cpp): HH:MM:SS[.ffffff] then
// sign + HH, then ":MM" only when MM != 0 and ":SS" only when SS != 0.
func timeTZString(bits uint64) string {
	const offsetMask = uint64(1)<<24 - 1 // dtime_tz_t::OFFSET_MASK (~0 >> TIME_BITS)
	const maxOffset = int64(16*60*60 - 1)
	micros := int64(bits >> 24)
	offset := maxOffset - int64(bits&offsetMask)

	// Time part: HH:MM:SS plus optional trimmed micros.
	hour := micros / microsPerHour
	micros -= hour * microsPerHour
	min := micros / microsPerMinute
	micros -= min * microsPerMinute
	sec := micros / microsPerSec
	micros -= sec * microsPerSec
	var b []byte
	b = appendTwoDigits(b, hour)
	b = append(b, ':')
	b = appendTwoDigits(b, min)
	b = append(b, ':')
	b = appendTwoDigits(b, sec)
	if micros != 0 {
		b = append(b, '.')
		b = appendMicrosTrimmed(b, int32(micros))
	}

	// Offset part.
	if offset < 0 {
		b = append(b, '-')
		offset = -offset
	} else {
		b = append(b, '+')
	}
	hh := offset / 3600
	b = appendTwoDigits(b, hh)
	offset %= 3600
	mm := offset / 60
	ss := offset % 60
	if mm != 0 {
		b = append(b, ':')
		b = appendTwoDigits(b, mm)
	}
	if ss != 0 {
		b = append(b, ':')
		b = appendTwoDigits(b, ss)
	}
	return string(b)
}

// bitString renders a BIT cell's backing blob as DuckDB's "0101..." VARCHAR cast.
// Layout (duckdb-src/src/common/types/bit.cpp, Bit::ToString/GetBitPadding): the
// blob's first byte holds the count of padding bits in the FIRST data byte; data
// bytes follow MSB-first, so the bitstring is bits [padding..8) of blob[1] then
// all 8 bits of each subsequent byte.
func bitString(blob []byte) string {
	if len(blob) < 2 {
		return ""
	}
	padding := int(blob[0])
	if padding > 8 {
		return "" // malformed; never expected
	}
	out := make([]byte, 0, (len(blob)-1)*8-padding)
	appendByte := func(v byte, from int) {
		for bit := from; bit < 8; bit++ {
			if v&(1<<(7-bit)) != 0 {
				out = append(out, '1')
			} else {
				out = append(out, '0')
			}
		}
	}
	appendByte(blob[1], padding)
	for i := 2; i < len(blob); i++ {
		appendByte(blob[i], 0)
	}
	return string(out)
}

// timestampValue maps a raw timestamp payload (int64 in `unit` since epoch) to a
// time.Time, EXCEPT the ±infinity sentinels which DuckDB renders as the strings
// "infinity"/"-infinity" (test_infinite_time.test). The sentinels are the same
// int64 values for every timestamp width.
func timestampValue(raw int64, unit time.Duration) driver.Value {
	switch raw {
	case tsInfinity:
		return "infinity"
	case tsNegInfinity:
		return "-infinity"
	}
	// Split into whole seconds + sub-second remainder instead of
	// epoch.Add(time.Duration(raw)*unit): the Duration multiplication is int64
	// NANOSECONDS and silently overflows beyond ±292 years from epoch, mangling
	// valid far-range timestamps like 290309-12-22 (BC) 00:00:00
	// (timestamp_limits.test). time.Unix normalizes a negative remainder and
	// covers DuckDB's full TIMESTAMP range.
	perSec := int64(time.Second / unit)
	return time.Unix(raw/perSec, (raw%perSec)*int64(unit)).UTC()
}

// dateValue maps a raw DATE payload (int32 days since epoch) to a time.Time,
// except the ±infinity sentinels (INT32_MAX / -INT32_MAX), delivered as strings.
func dateValue(days int32) driver.Value {
	switch days {
	case dateInfinity:
		return "infinity"
	case dateNegInfinity:
		return "-infinity"
	}
	return epoch.AddDate(0, 0, int(days))
}

// formatDecimal renders unscaled / 10^scale as an exact decimal string.
func formatDecimal(unscaled *big.Int, scale uint8) string {
	if scale == 0 {
		return unscaled.String()
	}
	neg := unscaled.Sign() < 0
	digits := new(big.Int).Abs(unscaled).String()
	for len(digits) <= int(scale) {
		digits = "0" + digits // pad so there is at least one integer digit
	}
	intPart := digits[:len(digits)-int(scale)]
	fracPart := digits[len(digits)-int(scale):]
	s := intPart + "." + fracPart
	if neg {
		s = "-" + s
	}
	return s
}
