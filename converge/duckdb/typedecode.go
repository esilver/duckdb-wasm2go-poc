// typedecode.go — decoding of TYPE result cells (get_type / make_type).
//
// LogicalTypeId::TYPE has no duckdb_type mapping (the C API reports
// DUCKDB_TYPE_INVALID); its cells are VARCHAR-physical string_t blobs holding
// a BinarySerializer-encoded LogicalType (Value::TYPE / TypeValue::GetType,
// src/common/types/value.cpp). The engine's TYPE -> VARCHAR cast
// (CastFromType, cast_operators.cpp:1433) deserializes the blob and renders
// LogicalType::ToString(); this file is a scoped Go port of both.
//
// Wire format (binary_serializer.cpp): properties are a raw little-endian
// uint16 field id followed by the value; objects end with the terminator id
// 0xFFFF; optional properties are omitted entirely when absent (the next
// field id disambiguates); nullable pointers carry a leading bool byte;
// lists a LEB128 count; varints are unsigned LEB128; strings a LEB128
// length + bytes; enums their underlying integer as a varint.
//
// Scope: plain type ids plus STRUCT/LIST/DECIMAL/ARRAY infos and the alias
// override — everything the corpus' TYPE results need. Unknown ids or info
// shapes decode to ok=false and the cell stays NULL (the previous behavior
// for the whole type).
package duckdb

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// readTypeCell reads a TYPE cell's string_t blob and renders it like the
// engine's TYPE -> VARCHAR cast. Defensive: a column that is NOT actually
// TYPE-backed may dereference garbage, so decode failures of any shape
// (including panics from wasm-memory slicing) report ok=false.
func readTypeCell(mod *module, ptr int32) (s string, ok bool) {
	defer func() {
		if recover() != nil {
			s, ok = "", false
		}
	}()
	_, b := readStringT(mod, ptr)
	if len(b) == 0 {
		return "", false
	}
	return typeBlobString(b)
}

// typeBlobString decodes a BinarySerializer-encoded LogicalType and renders
// LogicalType::ToString().
func typeBlobString(b []byte) (s string, ok bool) {
	defer func() {
		if recover() != nil {
			s, ok = "", false
		}
	}()
	d := &typeDec{b: b}
	t := d.readLogicalType()
	if d.bad || t == nil {
		return "", false
	}
	return t.String(), true
}

// LogicalTypeId values (duckdb/common/types.hpp).
const (
	ltSQLNull     = 1
	ltDecimal     = 21
	ltStruct      = 100
	ltList        = 101
	ltMap         = 102
	ltEnum        = 104
	ltUnion       = 107
	ltArray       = 108
	terminatorFID = 0xFFFF
)

// EnumUtil display names for plain ids (enum_util.cpp GetLogicalTypeIdValues).
var logicalTypeIdNames = map[uint64]string{
	1: "\"NULL\"", // LogicalType::ToString special-cases SQLNULL as quoted
	2: "UNKNOWN", 3: "ANY", 4: "UNBOUND", 5: "TEMPLATE", 6: "TYPE",
	10: "BOOLEAN", 11: "TINYINT", 12: "SMALLINT", 13: "INTEGER", 14: "BIGINT",
	15: "DATE", 16: "TIME", 17: "TIMESTAMP_S", 18: "TIMESTAMP_MS",
	19: "TIMESTAMP", 20: "TIMESTAMP_NS", 21: "DECIMAL", 22: "FLOAT",
	23: "DOUBLE", 24: "CHAR", 25: "VARCHAR", 26: "BLOB", 27: "INTERVAL",
	28: "UTINYINT", 29: "USMALLINT", 30: "UINTEGER", 31: "UBIGINT",
	32: "TIMESTAMP WITH TIME ZONE", 34: "TIME WITH TIME ZONE", 35: "TIME_NS",
	36: "BIT", 39: "BIGNUM", 49: "UHUGEINT", 50: "HUGEINT", 54: "UUID",
	60: "GEOMETRY", 100: "STRUCT", 101: "LIST", 102: "MAP", 104: "ENUM",
	107: "UNION", 108: "ARRAY", 109: "VARIANT",
}

// decodedType is the subset of LogicalType the decoder reconstructs.
type decodedType struct {
	id       uint64
	alias    string
	children []decodedChild // struct fields / list & array element / map kv
	width    uint64         // decimal
	scale    uint64         // decimal
	size     uint64         // array
	hasInfo  bool
}

type decodedChild struct {
	name string
	typ  *decodedType
}

// String ports the LogicalType::ToString cases the decoder can produce.
func (t *decodedType) String() string {
	if t.alias != "" {
		return t.alias
	}
	switch t.id {
	case ltStruct:
		if !t.hasInfo {
			return "STRUCT"
		}
		parts := make([]string, len(t.children))
		unnamed := true
		for _, c := range t.children {
			if c.name != "" {
				unnamed = false
				break
			}
		}
		for i, c := range t.children {
			if unnamed {
				parts[i] = c.typ.String()
			} else {
				parts[i] = sqlIdentifier(c.name) + " " + c.typ.String()
			}
		}
		return "STRUCT(" + strings.Join(parts, ", ") + ")"
	case ltList:
		if !t.hasInfo || len(t.children) == 0 {
			return "LIST"
		}
		return t.children[0].typ.String() + "[]"
	case ltMap:
		if !t.hasInfo || len(t.children) == 0 {
			return "MAP"
		}
		// MAP's info is a ListTypeInfo whose child is STRUCT(key, value).
		kv := t.children[0].typ
		if kv.id == ltStruct && len(kv.children) == 2 {
			return "MAP(" + kv.children[0].typ.String() + ", " + kv.children[1].typ.String() + ")"
		}
		return "MAP"
	case ltArray:
		if !t.hasInfo || len(t.children) == 0 {
			return "ARRAY"
		}
		if t.size == 0 {
			return t.children[0].typ.String() + "[ANY]"
		}
		return fmt.Sprintf("%s[%d]", t.children[0].typ.String(), t.size)
	case ltDecimal:
		if !t.hasInfo || t.width == 0 {
			return "DECIMAL"
		}
		return fmt.Sprintf("DECIMAL(%d,%d)", t.width, t.scale)
	}
	if n, ok := logicalTypeIdNames[t.id]; ok {
		return n
	}
	return ""
}

// sqlIdentifier mirrors KeywordHelper::WriteOptionallyQuoted for struct field
// names: bare when the name is a plain lowercase identifier, double-quoted
// (with embedded quotes doubled) otherwise. (Keyword detection is out of
// scope; corpus TYPE results use plain names.)
func sqlIdentifier(name string) string {
	plain := name != ""
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' || c == '_' || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		plain = false
		break
	}
	if plain {
		return name
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// ---- the binary reader ------------------------------------------------------

type typeDec struct {
	b   []byte
	pos int
	bad bool
}

func (d *typeDec) fail() {
	d.bad = true
}

func (d *typeDec) u16() uint16 {
	if d.pos+2 > len(d.b) {
		d.fail()
		return terminatorFID
	}
	v := binary.LittleEndian.Uint16(d.b[d.pos:])
	d.pos += 2
	return v
}

func (d *typeDec) peekU16() uint16 {
	if d.pos+2 > len(d.b) {
		return terminatorFID
	}
	return binary.LittleEndian.Uint16(d.b[d.pos:])
}

func (d *typeDec) varint() uint64 {
	var v uint64
	var shift uint
	for {
		if d.pos >= len(d.b) || shift > 63 {
			d.fail()
			return 0
		}
		c := d.b[d.pos]
		d.pos++
		v |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			return v
		}
		shift += 7
	}
}

func (d *typeDec) str() string {
	n := d.varint()
	if d.bad || d.pos+int(n) > len(d.b) {
		d.fail()
		return ""
	}
	s := string(d.b[d.pos : d.pos+int(n)])
	d.pos += int(n)
	return s
}

func (d *typeDec) boolByte() bool {
	if d.pos >= len(d.b) {
		d.fail()
		return false
	}
	v := d.b[d.pos] != 0
	d.pos++
	return v
}

func (d *typeDec) expectField(id uint16) {
	if d.u16() != id {
		d.fail()
	}
}

func (d *typeDec) objectEnd() {
	if d.u16() != terminatorFID {
		d.fail()
	}
}

// readLogicalType reads one serialized LogicalType object:
//
//	100 "id" (enum varint), optional 101 "type_info" (nullable ExtraTypeInfo
//	object), terminator.
func (d *typeDec) readLogicalType() *decodedType {
	if d.bad {
		return nil
	}
	t := &decodedType{}
	d.expectField(100)
	t.id = d.varint()
	if d.peekU16() == 101 {
		d.u16()
		if d.boolByte() { // nullable: present
			d.readTypeInfo(t)
		}
	}
	d.objectEnd()
	if d.bad {
		return nil
	}
	return t
}

// ExtraTypeInfoType values (extra_type_info.hpp).
const (
	etiDecimal = 2
	etiList    = 4
	etiStruct  = 5
	etiArray   = 9
)

// readTypeInfo reads one ExtraTypeInfo object into t: base fields
// (100 "type" enum, optional 101 "alias" string, optional 103
// "extension_info" — unsupported, fails the decode) plus the subclass fields
// at 200+ for the supported kinds, then the object terminator.
func (d *typeDec) readTypeInfo(t *decodedType) {
	t.hasInfo = true
	d.expectField(100)
	infoType := d.varint()
	if d.peekU16() == 101 {
		d.u16()
		t.alias = d.str()
	}
	if d.peekU16() == 103 {
		// extension_info: tolerate an explicit null (defaults-serialized
		// blobs); a present object (type modifiers) is out of scope.
		d.u16()
		if d.boolByte() {
			d.fail()
			return
		}
	}
	switch infoType {
	case etiDecimal:
		if d.peekU16() == 200 {
			d.u16()
			t.width = d.varint()
		}
		if d.peekU16() == 201 {
			d.u16()
			t.scale = d.varint()
		}
	case etiList:
		if d.peekU16() == 200 {
			d.u16()
			ct := d.readLogicalType()
			t.children = append(t.children, decodedChild{typ: ct})
		}
	case etiStruct:
		if d.peekU16() == 200 {
			d.u16()
			n := d.varint()
			if d.bad || n > 1<<20 {
				d.fail()
				return
			}
			for i := uint64(0); i < n && !d.bad; i++ {
				// pair<string, LogicalType>: object {0 "first", 1 "second"}.
				d.expectField(0)
				name := d.str()
				d.expectField(1)
				ct := d.readLogicalType()
				d.objectEnd()
				t.children = append(t.children, decodedChild{name: name, typ: ct})
			}
		}
	case etiArray:
		if d.peekU16() == 200 {
			d.u16()
			ct := d.readLogicalType()
			t.children = append(t.children, decodedChild{typ: ct})
		}
		if d.peekU16() == 201 {
			d.u16()
			t.size = d.varint()
		}
	default:
		// ENUM/UNBOUND/AGGREGATE_STATE/... — out of scope.
		d.fail()
		return
	}
	d.objectEnd()
}
