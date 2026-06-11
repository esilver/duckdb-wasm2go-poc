// udf_codec.go — the type codec shared by the scalar and aggregate UDF registrars.
//
// A UDF callback runs inside the engine and is handed raw duckdb_vector offsets:
// a flat data buffer plus an optional validity mask (input side), and an output
// vector to fill (return side). This file is the single place that knows how to
// turn ONE input-vector cell into a Go value, and ONE Go value back into an
// output-vector cell, for the driver.Value kinds the driver supports
// (bool, int64, float64, string, []byte, time.Time, nil).
//
// The wasm-memory layouts here (duckdb_string_t, the uint64 validity bitmask) are
// the SAME ones result.go's reader already proves against live query output; this
// file generalizes that reader for both decode AND encode. The duckdb_type enum
// constants (dtBoolean, dtBigint, ...) and `epoch` live in result.go — reused, not
// redefined.
package duckdb

import (
	"fmt"
	"math"
	"math/big"
	"time"
)

// elemSize returns the flat byte width of one element of typeID in a duckdb_vector
// data buffer. This is the stride used to index dataPtr+row*elemSize. Returns 0 for
// types that have no fixed flat width we encode/decode here (so callers can detect
// "not a numeric/flat cell"). string_t-backed types (VARCHAR/BLOB) report 16.
func (mod *module) elemSize(typeID int32) int32 {
	switch typeID {
	case dtBoolean, dtTinyint, dtUtinyint:
		return 1
	case dtSmallint, dtUsmallint:
		return 2
	case dtInteger, dtUinteger, dtFloat, dtDate:
		return 4
	case dtBigint, dtUbigint, dtDouble, dtTimestamp, dtTimestampTz:
		return 8
	case dtVarchar, dtBlob:
		return 16 // duckdb_string_t
	default:
		return 0
	}
}

// ---- validity helpers --------------------------------------------------------

// readValid reports whether the cell at row is non-NULL. validPtr==0 means the
// vector has no validity mask (all valid). Otherwise the mask is a uint64 array:
// bit (row%64) of word (row/64) is 1 when the row is valid. Mirrors result.go's
// rowValid, generalized to int64 row indices used by UDF callbacks.
func (mod *module) readValid(validPtr int32, row int64) bool {
	if validPtr == 0 {
		return true
	}
	word := mod.readU64(validPtr + int32(8*(row/64)))
	return (word>>(uint64(row)%64))&1 == 1
}

// clearValid clears (marks NULL) the validity bit for row in the mask at validPtr.
// validPtr MUST point at a writable validity mask — callers obtain one for an
// output vector via Xduckdb_vector_ensure_validity_writable before calling.
func (mod *module) clearValid(validPtr int32, row int64) {
	wordPtr := validPtr + int32(8*(row/64))
	word := mod.readU64(wordPtr)
	word &^= uint64(1) << (uint64(row) % 64)
	mod.writeU64(wordPtr, word)
}

// writeU64 writes a little-endian uint64 at ptr (the validity/numeric write
// counterpart to module.go's readU64; module.go only exposes writeU32).
func (mod *module) writeU64(ptr int32, v uint64) {
	mem := mod.mem()
	mem[ptr+0] = byte(v)
	mem[ptr+1] = byte(v >> 8)
	mem[ptr+2] = byte(v >> 16)
	mem[ptr+3] = byte(v >> 24)
	mem[ptr+4] = byte(v >> 32)
	mem[ptr+5] = byte(v >> 40)
	mem[ptr+6] = byte(v >> 48)
	mem[ptr+7] = byte(v >> 56)
}

// readDecimalUnscaled reads a DECIMAL cell's backing integer per the column's
// internal storage type id. Returns nil for an unrecognized backing type.
func (mod *module) readDecimalUnscaled(internalType, dataPtr int32, row int64) *big.Int {
	switch internalType {
	case dtSmallint:
		return big.NewInt(int64(int16(mod.readU32(dataPtr + int32(row*2)))))
	case dtInteger:
		return big.NewInt(int64(int32(mod.readU32(dataPtr + int32(row*4)))))
	case dtBigint:
		return big.NewInt(mod.readI64(dataPtr + int32(row*8)))
	case dtHugeint:
		base := dataPtr + int32(row*16)
		lower := mod.readU64(base)
		upper := mod.readI64(base + 8)
		unscaled := big.NewInt(upper)
		unscaled.Lsh(unscaled, 64)
		unscaled.Add(unscaled, new(big.Int).SetUint64(lower))
		return unscaled
	}
	return nil
}

// ---- decode: input vector cell -> Go value -----------------------------------

// readCell decodes ONE input-vector cell at row into a Go value, given the cell's
// duckdb_type, the vector's flat data buffer (dataPtr) and its validity mask
// (validPtr; 0 == all valid). Returns nil for a NULL cell or an unsupported type.
//
// Supported: BOOLEAN->bool; (U)TINYINT/SMALLINT/INTEGER/BIGINT->int64 (signed are
// sign-extended); FLOAT/DOUBLE->float64; VARCHAR->string; BLOB->[]byte;
// DATE(int32 days)->time.Time UTC; TIMESTAMP(int64 micros)->time.Time UTC.
//
// DECIMAL (type id 19) cannot be decoded by readCell alone: the per-cell data
// vector carries only the backing integer, not the width/scale/internal-storage
// type that live in the column's logical type. readCell therefore decodes DECIMAL
// best-effort as a raw int64 with scale 0 (no decimal point), which is wrong for
// scale>0. Callers that have the column logical type at hand (the scalar/aggregate
// registrars, which capture per-param scale + internal type) MUST use readCellT
// instead so DECIMAL args arrive as an exact decimal string. See readCellT.
func (mod *module) readCell(typeID, dataPtr, validPtr int32, row int64) any {
	return mod.readCellT(typeID, 0, dtBigint, dataPtr, validPtr, row)
}

// readCellT is readCell with the extra DECIMAL metadata (scale + internal storage
// type id) that the bare data vector lacks. For every non-DECIMAL type id scale and
// internalType are ignored and behavior is identical to readCell.
//
// For DECIMAL (type id 19) the backing integer is read per internalType
// (SMALLINT/INTEGER/BIGINT/HUGEINT) and rendered as the EXACT decimal string
// `unscaled / 10^scale` (e.g. internalType=INTEGER, scale=2, raw=150 -> "1.50").
// A string is chosen over float64 so NUMERIC precision is never lost; the emulator's
// value layer (and the compat duckdb.Decimal carrier) parse it losslessly. If
// internalType is not a recognized DECIMAL backing integer the cell decodes to nil.
func (mod *module) readCellT(typeID, scale, internalType, dataPtr, validPtr int32, row int64) any {
	if !mod.readValid(validPtr, row) {
		return nil
	}
	mem := mod.mem()
	switch typeID {

	case dtDecimal:
		unscaled := mod.readDecimalUnscaled(internalType, dataPtr, row)
		if unscaled == nil {
			return nil
		}
		return formatDecimal(unscaled, uint8(scale))

	case dtBoolean:
		return mem[dataPtr+int32(row)] != 0

	case dtTinyint:
		return int64(int8(mem[dataPtr+int32(row)]))
	case dtSmallint:
		return int64(int16(mod.readU32(dataPtr + int32(row*2))))
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
		// May overflow int64; callers that need the full range should special-case,
		// but driver.Value semantics here keep it int64 (matches scalar arg coercion).
		return int64(mod.readU64(dataPtr + int32(row*8)))

	case dtFloat:
		return float64(mod.readF32(dataPtr + int32(row*4)))
	case dtDouble:
		return mod.readF64(dataPtr + int32(row*8))

	case dtHugeint:
		return hugeintValue(mod, dataPtr, int(row), true)
	case dtUhugeint:
		return hugeintValue(mod, dataPtr, int(row), false)

	case dtVarchar:
		s, _ := readStringT(mod, dataPtr+int32(row*16))
		return s
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
		return readInterval(mod, dataPtr, row)

	case dtBit:
		_, b := readStringT(mod, dataPtr+int32(row*16))
		return bitString(b)

	case dtUuid:
		return uuidString(mod, dataPtr, int(row))

	case dtBignum:
		_, b := readStringT(mod, dataPtr+int32(row*16))
		return bignumString(b)

	case dtGeometry:
		_, b := readStringT(mod, dataPtr+int32(row*16))
		return geometryString(b)

	default:
		return nil
	}
}

// ---- encode: Go value -> output vector cell ----------------------------------

// writeCell encodes Go value v into the OUTPUT vector vec's cell at row, where
// dataPtr is vec's flat data buffer (Xduckdb_vector_get_data(vec)) and typeID is
// the vector's declared duckdb_type.
//
//   - v == nil writes SQL NULL: it makes vec's validity mask writable and clears
//     the bit for row.
//   - VARCHAR uses Xduckdb_vector_assign_string_element (engine copies the C
//     string into its own string heap). BLOB uses the _len variant so embedded
//     NULs survive.
//   - numerics are written little-endian into dataPtr+row*elemSize, coercing
//     int/int64/float64/bool/time.Time to the target type as sensible.
//
// Returns an error if v's dynamic type cannot be encoded into typeID.
func (mod *module) writeCell(typeID, vec, dataPtr int32, row int64, v any) error {
	if v == nil {
		mod.m.Xduckdb_vector_ensure_validity_writable(vec)
		validPtr := mod.m.Xduckdb_vector_get_validity(vec)
		if validPtr != 0 {
			mod.clearValid(validPtr, row)
		}
		return nil
	}

	switch typeID {
	case dtVarchar:
		s, ok := asString(v)
		if !ok {
			return fmt.Errorf("duckdb: cannot encode %T into VARCHAR", v)
		}
		cs := mod.cstring(s)
		mod.m.Xduckdb_vector_assign_string_element(vec, row, cs)
		mod.free(cs)
		return nil

	case dtBlob:
		b, ok := asBytes(v)
		if !ok {
			return fmt.Errorf("duckdb: cannot encode %T into BLOB", v)
		}
		// Copy the bytes into module memory and assign by length (handles embedded
		// NULs, unlike the NUL-terminated assign_string_element).
		ptr := mod.allocOut(int32(len(b)) + 1)
		copy(mod.mem()[ptr:], b)
		mod.m.Xduckdb_vector_assign_string_element_len(vec, row, ptr, int64(len(b)))
		mod.free(ptr)
		return nil

	case dtBoolean:
		b, ok := asBool(v)
		if !ok {
			return fmt.Errorf("duckdb: cannot encode %T into BOOLEAN", v)
		}
		bb := byte(0)
		if b {
			bb = 1
		}
		mod.mem()[dataPtr+int32(row)] = bb
		return nil

	case dtTinyint, dtUtinyint:
		i, ok := asInt64(v)
		if !ok {
			return fmt.Errorf("duckdb: cannot encode %T into 1-byte integer", v)
		}
		mod.mem()[dataPtr+int32(row)] = byte(i)
		return nil

	case dtSmallint, dtUsmallint:
		i, ok := asInt64(v)
		if !ok {
			return fmt.Errorf("duckdb: cannot encode %T into SMALLINT", v)
		}
		mod.writeU16(dataPtr+int32(row*2), uint16(i))
		return nil

	case dtInteger, dtUinteger:
		i, ok := asInt64(v)
		if !ok {
			return fmt.Errorf("duckdb: cannot encode %T into INTEGER", v)
		}
		mod.writeU32(dataPtr+int32(row*4), uint32(i))
		return nil

	case dtBigint, dtUbigint:
		i, ok := asInt64(v)
		if !ok {
			return fmt.Errorf("duckdb: cannot encode %T into BIGINT", v)
		}
		mod.writeU64(dataPtr+int32(row*8), uint64(i))
		return nil

	case dtFloat:
		f, ok := asFloat64(v)
		if !ok {
			return fmt.Errorf("duckdb: cannot encode %T into FLOAT", v)
		}
		mod.writeU32(dataPtr+int32(row*4), math.Float32bits(float32(f)))
		return nil

	case dtDouble:
		f, ok := asFloat64(v)
		if !ok {
			return fmt.Errorf("duckdb: cannot encode %T into DOUBLE", v)
		}
		mod.writeU64(dataPtr+int32(row*8), math.Float64bits(f))
		return nil

	case dtDate:
		t, ok := v.(time.Time)
		if !ok {
			return fmt.Errorf("duckdb: cannot encode %T into DATE", v)
		}
		// Floor-divide Unix seconds rather than Sub(epoch): a Duration is
		// int64 nanos and saturates ±292y from epoch, silently corrupting
		// any date outside 1678-2262 (BigQuery's range is 0001-9999).
		secs := t.Unix()
		days := secs / 86400
		if secs%86400 < 0 {
			days--
		}
		mod.writeU32(dataPtr+int32(row*4), uint32(int32(days)))
		return nil

	case dtTimestamp, dtTimestampTz:
		t, ok := v.(time.Time)
		if !ok {
			return fmt.Errorf("duckdb: cannot encode %T into TIMESTAMP", v)
		}
		// UnixMicro, not Sub(epoch).Microseconds(): the intermediate
		// Duration saturates ±292y from epoch (see dtDate above); micros
		// in int64 are good to year ±294246.
		mod.writeU64(dataPtr+int32(row*8), uint64(t.UnixMicro()))
		return nil

	default:
		return fmt.Errorf("duckdb: writeCell: unsupported output type id %d", typeID)
	}
}

// writeU16 writes a little-endian uint16 at ptr.
func (mod *module) writeU16(ptr int32, v uint16) {
	mem := mod.mem()
	mem[ptr+0] = byte(v)
	mem[ptr+1] = byte(v >> 8)
}

// ---- Go value coercion helpers -----------------------------------------------

func asInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case int32:
		return int64(x), true
	case int16:
		return int64(x), true
	case int8:
		return int64(x), true
	case uint64:
		return int64(x), true
	case uint:
		return int64(x), true
	case uint32:
		return int64(x), true
	case float64:
		return int64(x), true
	case float32:
		return int64(x), true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}

func asFloat64(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int64:
		return float64(x), true
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	default:
		return 0, false
	}
}

func asBool(v any) (bool, bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	case int64:
		return x != 0, true
	case int:
		return x != 0, true
	default:
		return false, false
	}
}

func asString(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case []byte:
		return string(x), true
	default:
		return "", false
	}
}

func asBytes(v any) ([]byte, bool) {
	switch x := v.(type) {
	case []byte:
		return x, true
	case string:
		return []byte(x), true
	default:
		return nil, false
	}
}
