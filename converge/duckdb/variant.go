// variant.go — VARIANT (duckdb_type 41) cell decoding for the pure-Go driver.
//
// A VARIANT vector is physically STRUCT(keys VARCHAR[], children
// STRUCT(keys_index UINTEGER, values_index UINTEGER)[], values STRUCT(type_id
// UTINYINT, byte_offset UINTEGER)[], data BLOB) (types.cpp
// LogicalType::VARIANT). Each row's value tree lives in its `values` list (the
// ROOT is local value index 0) with scalar payloads serialized into the row's
// `data` blob at byte_offset (variant.hpp VariantLogicalType; fixed-width
// scalars little-endian, string-likes varint-length-prefixed, OBJECT/ARRAY a
// varint child_count [+ varint children_idx] indexing the row's `children`
// list, whose entries point at keys/values local indexes).
//
// Cells decode directly to the FINAL string DuckDB's own Value::CastAs(VARCHAR)
// produces (what the upstream sqllogictest harness compares): scalars render
// as their VARCHAR casts, ARRAY items render raw (VARIANT-typed children are
// never quoted by the nested cast), OBJECT values are quoted-if-needed like
// any nested scalar child, keys always single-quoted.
package duckdb

import (
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// VariantLogicalType ids (variant.hpp).
const (
	vtNull        = 0
	vtBoolTrue    = 1
	vtBoolFalse   = 2
	vtInt8        = 3
	vtInt16       = 4
	vtInt32       = 5
	vtInt64       = 6
	vtInt128      = 7
	vtUint8       = 8
	vtUint16      = 9
	vtUint32      = 10
	vtUint64      = 11
	vtUint128     = 12
	vtFloat       = 13
	vtDouble      = 14
	vtDecimal     = 15
	vtVarchar     = 16
	vtBlob        = 17
	vtUUID        = 18
	vtDate        = 19
	vtTimeMicros  = 20
	vtTimeNanos   = 21
	vtTimestampS  = 22
	vtTimestampMs = 23
	vtTimestampUs = 24
	vtTimestampNs = 25
	vtTimeTZ      = 26
	vtTimestampTZ = 27
	vtInterval    = 28
	vtObject      = 29
	vtArray       = 30
	vtBignum      = 31
	vtBitstring   = 32
	vtGeometry    = 33
)

// variantDec snapshots the data pointers of a VARIANT vector's physical
// children for the current chunk.
type variantDec struct {
	mod *module
	// list_entry{u64 offset, u64 length} buffers of the three list children.
	keysData, childrenData, valuesData int32
	// keys' child: string_t buffer (the per-row key dictionary).
	keysEntryData int32
	// children's child struct: u32 keys_index / values_index buffers.
	keysIdxData, valuesIdxData int32
	// values' child struct: u8 type_id / u32 byte_offset buffers.
	typeIDData, byteOffData int32
	// data child: string_t buffer (per-row payload blob).
	blobData int32
}

// newVariantDec walks the VARIANT vector's physical children (the C API's
// struct/list vector accessors operate on physical layout, so they work on
// VARIANT vectors) and snapshots all leaf data buffers.
func (mod *module) newVariantDec(vec int32) *variantDec {
	m := mod.m
	keysVec := m.Xduckdb_struct_vector_get_child(vec, 0)
	childrenVec := m.Xduckdb_struct_vector_get_child(vec, 1)
	valuesVec := m.Xduckdb_struct_vector_get_child(vec, 2)
	dataVec := m.Xduckdb_struct_vector_get_child(vec, 3)
	childEntry := m.Xduckdb_list_vector_get_child(childrenVec)
	valEntry := m.Xduckdb_list_vector_get_child(valuesVec)
	return &variantDec{
		mod:           mod,
		keysData:      m.Xduckdb_vector_get_data(keysVec),
		childrenData:  m.Xduckdb_vector_get_data(childrenVec),
		valuesData:    m.Xduckdb_vector_get_data(valuesVec),
		keysEntryData: m.Xduckdb_vector_get_data(m.Xduckdb_list_vector_get_child(keysVec)),
		keysIdxData:   m.Xduckdb_vector_get_data(m.Xduckdb_struct_vector_get_child(childEntry, 0)),
		valuesIdxData: m.Xduckdb_vector_get_data(m.Xduckdb_struct_vector_get_child(childEntry, 1)),
		typeIDData:    m.Xduckdb_vector_get_data(m.Xduckdb_struct_vector_get_child(valEntry, 0)),
		byteOffData:   m.Xduckdb_vector_get_data(m.Xduckdb_struct_vector_get_child(valEntry, 1)),
		blobData:      m.Xduckdb_vector_get_data(dataVec),
	}
}

func (d *variantDec) listEntry(dataPtr int32, row int64) (off, length int64) {
	base := dataPtr + int32(row*16)
	return int64(d.mod.readU64(base)), int64(d.mod.readU64(base + 8))
}

// render decodes row's VARIANT value tree to its VARCHAR-cast string.
func (d *variantDec) render(row int64) string {
	kOff, _ := d.listEntry(d.keysData, row)
	cOff, _ := d.listEntry(d.childrenData, row)
	vOff, vLen := d.listEntry(d.valuesData, row)
	if vLen == 0 {
		return "NULL" // malformed; a valid non-NULL row has a root value
	}
	_, blob := readStringT(d.mod, d.blobData+int32(row*16))
	s, _ := d.renderValue(kOff, cOff, vOff, blob, 0)
	return s
}

// renderValue renders the value at LOCAL index idx of the row's values list,
// returning the string plus whether it rendered raw at any nesting position
// (containers and NULL — these are never re-quoted by an enclosing OBJECT).
func (d *variantDec) renderValue(kOff, cOff, vOff int64, blob []byte, idx int64) (string, bool) {
	mod := d.mod
	tid := mod.mem()[d.typeIDData+int32(vOff+idx)]
	boff := int64(mod.readU32(d.byteOffData + int32(4*(vOff+idx))))
	if boff < 0 || boff > int64(len(blob)) {
		return "NULL", true
	}
	p := blob[boff:]

	switch tid {
	case vtNull:
		return "NULL", true
	case vtBoolTrue:
		return "true", false
	case vtBoolFalse:
		return "false", false
	case vtInt8:
		return strconv.FormatInt(int64(int8(leU8(p))), 10), false
	case vtInt16:
		return strconv.FormatInt(int64(int16(leU16(p))), 10), false
	case vtInt32:
		return strconv.FormatInt(int64(int32(leU32(p))), 10), false
	case vtInt64:
		return strconv.FormatInt(int64(leU64(p)), 10), false
	case vtInt128:
		return bigFrom128(p, true).String(), false
	case vtUint8:
		return strconv.FormatUint(uint64(leU8(p)), 10), false
	case vtUint16:
		return strconv.FormatUint(uint64(leU16(p)), 10), false
	case vtUint32:
		return strconv.FormatUint(uint64(leU32(p)), 10), false
	case vtUint64:
		return strconv.FormatUint(leU64(p), 10), false
	case vtUint128:
		return bigFrom128(p, false).String(), false
	case vtFloat:
		return formatFloatDuckDB(float64(math.Float32frombits(leU32(p)))), false
	case vtDouble:
		return formatDoubleDuckDB(math.Float64frombits(leU64(p))), false
	case vtDecimal:
		width, n1 := uvarint(p)
		scale, n2 := uvarint(p[n1:])
		v := p[n1+n2:]
		var unscaled *big.Int
		switch {
		case width > 18:
			unscaled = bigFrom128(v, true)
		case width > 9:
			unscaled = big.NewInt(int64(leU64(v)))
		case width > 4:
			unscaled = big.NewInt(int64(int32(leU32(v))))
		default:
			unscaled = big.NewInt(int64(int16(leU16(v))))
		}
		return formatDecimal(unscaled, uint8(scale)), false
	case vtVarchar:
		return string(varintBytes(p)), false
	case vtBlob:
		return blobLiteral(varintBytes(p)), false
	case vtUUID:
		return uuidFromWords(leU64(p), int64(leU64(p[8:]))), false
	case vtDate:
		return dateStringFromDays(int32(leU32(p))), false
	case vtTimeMicros:
		return timeStringMicros(int64(leU64(p))), false
	case vtTimeNanos:
		return timeStringNanos(int64(leU64(p))), false
	case vtTimestampS:
		return timestampString(int64(leU64(p)), time.Second, ""), false
	case vtTimestampMs:
		return timestampString(int64(leU64(p)), time.Millisecond, ""), false
	case vtTimestampUs:
		return timestampString(int64(leU64(p)), time.Microsecond, ""), false
	case vtTimestampNs:
		return timestampString(int64(leU64(p)), time.Nanosecond, ""), false
	case vtTimeTZ:
		return timeTZString(leU64(p)), false
	case vtTimestampTZ:
		return timestampString(int64(leU64(p)), time.Microsecond, "+00"), false
	case vtInterval:
		return Interval{
			Months: int32(leU32(p)),
			Days:   int32(leU32(p[4:])),
			Micros: int64(leU64(p[8:])),
		}.String(), false
	case vtBignum:
		return bignumString(varintBytes(p)), false
	case vtBitstring:
		return bitString(varintBytes(p)), false
	case vtGeometry:
		return geometryString(varintBytes(p)), false

	case vtArray:
		count, n := uvarint(p)
		if count == 0 {
			return "[]", true
		}
		childrenIdx, _ := uvarint(p[n:])
		parts := make([]string, count)
		for i := uint32(0); i < count; i++ {
			child := cOff + int64(childrenIdx) + int64(i)
			vIdx := int64(d.mod.readU32(d.valuesIdxData + int32(4*child)))
			// ARRAY items are VARIANT-typed children of the nested cast: raw.
			parts[i], _ = d.renderValue(kOff, cOff, vOff, blob, vIdx)
		}
		return "[" + strings.Join(parts, ", ") + "]", true

	case vtObject:
		count, n := uvarint(p)
		if count == 0 {
			return "{}", true
		}
		childrenIdx, _ := uvarint(p[n:])
		parts := make([]string, count)
		for i := uint32(0); i < count; i++ {
			child := cOff + int64(childrenIdx) + int64(i)
			keyIdx := int64(d.mod.readU32(d.keysIdxData + int32(4*child)))
			vIdx := int64(d.mod.readU32(d.valuesIdxData + int32(4*child)))
			key, _ := readStringT(d.mod, d.keysEntryData+int32(16*(kOff+keyIdx)))
			val, raw := d.renderValue(kOff, cOff, vOff, blob, vIdx)
			if !raw {
				val = quoteNestedDuckDB(val)
			}
			parts[i] = "'" + key + "': " + val
		}
		return "{" + strings.Join(parts, ", ") + "}", true
	}
	return "NULL", true
}

// ---- little helpers --------------------------------------------------------

func leU8(p []byte) byte {
	if len(p) < 1 {
		return 0
	}
	return p[0]
}

func leU16(p []byte) uint16 {
	if len(p) < 2 {
		return 0
	}
	return uint16(p[0]) | uint16(p[1])<<8
}

func leU32(p []byte) uint32 {
	if len(p) < 4 {
		return 0
	}
	return uint32(p[0]) | uint32(p[1])<<8 | uint32(p[2])<<16 | uint32(p[3])<<24
}

func leU64(p []byte) uint64 {
	if len(p) < 8 {
		return 0
	}
	var v uint64
	for i := 7; i >= 0; i-- {
		v = v<<8 | uint64(p[i])
	}
	return v
}

// bigFrom128 builds the 128-bit integer stored little-endian at p (lower u64
// then upper word, signed or unsigned).
func bigFrom128(p []byte, signed bool) *big.Int {
	lower := leU64(p)
	v := new(big.Int)
	if signed {
		v.SetInt64(int64(leU64(p[8:])))
	} else {
		v.SetUint64(leU64(p[8:]))
	}
	v.Lsh(v, 64)
	return v.Add(v, new(big.Int).SetUint64(lower))
}

// uvarint decodes DuckDB's serializer varint (7-bit little-endian groups,
// high bit = continuation; serializer/varint.hpp VarintDecode<uint32_t>).
func uvarint(p []byte) (uint32, int) {
	var v uint32
	var shift uint
	for i := 0; i < len(p); i++ {
		b := p[i]
		v |= uint32(b&0x7F) << shift
		if b&0x80 == 0 {
			return v, i + 1
		}
		shift += 7
		if shift > 32 {
			break
		}
	}
	return v, len(p)
}

// varintBytes returns the varint-length-prefixed byte run at p.
func varintBytes(p []byte) []byte {
	n, off := uvarint(p)
	end := off + int(n)
	if end > len(p) {
		end = len(p)
	}
	return p[off:end]
}

// quoteNestedDuckDB applies the nested cast's child-quoting rule
// (vector_cast_helpers.hpp): quote when empty, leading (or trailing at len>=2)
// whitespace, case-insensitive "null", or any of " ' ( ) , : = [ ] { };
// quoting backslash-escapes ' and \.
func quoteNestedDuckDB(s string) string {
	needs := s == ""
	if !needs && (isSpaceByteDD(s[0]) || (len(s) >= 2 && isSpaceByteDD(s[len(s)-1]))) {
		needs = true
	}
	if !needs && strings.EqualFold(s, "null") {
		needs = true
	}
	if !needs && strings.ContainsAny(s, "\"'(),:=[]{}") {
		needs = true
	}
	if !needs {
		return s
	}
	var sb strings.Builder
	sb.WriteByte('\'')
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\'' || c == '\\' {
			sb.WriteByte('\\')
		}
		sb.WriteByte(c)
	}
	sb.WriteByte('\'')
	return sb.String()
}

func isSpaceByteDD(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
