// udf_vec.go — per-vector cell decoding for UDF arguments, shared by the scalar
// and aggregate-band registrars.
//
// A vecDecoder snapshots one input vector's data/validity pointers and type
// metadata once per chunk (everything is stable for the chunk's lifetime) and
// then decodes individual rows. It generalizes the flat readCellT decode with
// the two pieces a registrar otherwise has to hand-roll:
//
//   - JSON-alias detection: JSON is VARCHAR-backed; cells of JSON columns are
//     wrapped as JSONValue so the duckdbcompat layer can apply duckdb-go's
//     scan-JSON-to-native behavior.
//   - LIST decoding (recursive): a LIST cell decodes to a []any of decoded
//     child cells — the shape duckdb-go delivers for LIST arguments. The list
//     vector's data buffer holds one duckdb_list_entry{offset, length uint64}
//     (16 bytes) per row indexing into the shared child vector. Nested lists
//     recurse. (The googlesqlite STRING_AGG lowering — array_agg into
//     googlesqlite_array_to_string(LIST, sep) — needs exactly this; without it
//     the LIST arg decoded to nil and the UDF body nil-dereferenced.)
//   - STRUCT decoding (recursive): a STRUCT cell decodes to a map[string]any of
//     decoded child cells — the shape duckdb-go scans STRUCT into. Struct child
//     vectors are PARALLEL arrays (no per-row offsets): row r of the struct is
//     row r of each child vector, reached via duckdb_struct_vector_get_child;
//     field names come from the logical type. LIST-of-STRUCT / STRUCT-with-LIST
//     nest for free through the recursion.
//   - MAP decoding: a MAP vector is physically a LIST whose child is a
//     STRUCT{key, value} vector, so a MAP cell decodes to a map[any]any by
//     walking the list entries and pairing the struct's key/value children
//     (duckdb-go's Map shape). Non-comparable keys (e.g. LIST keys) are not
//     supported and decode the cell to nil.
//
// UNION arguments remain undecoded (nil) — nothing in the googlesqlite surface
// passes them natively yet.
package duckdb

import "reflect"

// Nested duckdb_type ids (logical-only; the flat codec can't decode them,
// vecDecoder handles them structurally).
const (
	dtList   = 24
	dtStruct = 25
	dtMap    = 26
)

type vecDecoder struct {
	mod      *module
	dataPtr  int32
	validPtr int32
	typeID   int32
	width    int32 // DECIMAL only
	scale    int32 // DECIMAL only
	internal int32 // DECIMAL backing integer type id
	isJSON   bool  // VARCHAR with the JSON alias
	enum     *enumInfo // ENUM only: backing index type + dictionary strings
	// child is the shared child-vector decoder for LIST, and for MAP (where the
	// child is the {key, value} STRUCT vector backing the map's list layout).
	child *vecDecoder
	// fields are the named child decoders for STRUCT (parallel arrays, same row
	// index as the struct vector itself).
	fields []structFieldDec
}

type structFieldDec struct {
	name string
	dec  *vecDecoder
}

// newVecDecoder snapshots vec's decode metadata (recursing into LIST children).
func (mod *module) newVecDecoder(vec int32) *vecDecoder {
	m := mod.m
	d := &vecDecoder{
		mod:      mod,
		dataPtr:  m.Xduckdb_vector_get_data(vec),
		validPtr: m.Xduckdb_vector_get_validity(vec),
		internal: dtBigint,
	}
	lt := m.Xduckdb_vector_get_column_type(vec)
	d.typeID = m.Xduckdb_get_type_id(lt)
	switch d.typeID {
	case dtDecimal:
		d.width = m.Xduckdb_decimal_width(lt)
		d.scale = m.Xduckdb_decimal_scale(lt)
		d.internal = m.Xduckdb_decimal_internal_type(lt)
	case dtVarchar:
		// The JSON alias is the only way to spot a JSON column (VARCHAR-backed).
		if ap := m.Xduckdb_logical_type_get_alias(lt); ap != 0 {
			d.isJSON = mod.goString(ap) == "JSON"
			m.Xduckdb_free(ap)
		}
	case dtEnum:
		// ENUM cells are dictionary indexes (uint8/16/32), not string_t; the
		// flat codec can't decode them. Snapshot the dictionary once.
		info := readEnumMeta(mod, lt)
		d.enum = &info
	case dtList:
		d.child = mod.newVecDecoder(m.Xduckdb_list_vector_get_child(vec))
	case dtStruct:
		n := m.Xduckdb_struct_type_child_count(lt)
		d.fields = make([]structFieldDec, n)
		for i := int64(0); i < n; i++ {
			namePtr := m.Xduckdb_struct_type_child_name(lt, i) // malloc'd char*
			name := mod.goString(namePtr)
			m.Xduckdb_free(namePtr)
			d.fields[i] = structFieldDec{
				name: name,
				dec:  mod.newVecDecoder(m.Xduckdb_struct_vector_get_child(vec, i)),
			}
		}
	case dtMap:
		// MAP is physically a LIST of STRUCT{key, value}; the same list-child
		// accessor reaches the struct vector, whose own column type recurses
		// into a STRUCT decoder with key/value fields.
		d.child = mod.newVecDecoder(m.Xduckdb_list_vector_get_child(vec))
	}
	destroyLogicalType(mod, lt)
	return d
}

// cell decodes the row'th cell. NULL cells decode to nil.
func (d *vecDecoder) cell(row int64) any {
	if d.typeID == dtList {
		if !d.mod.readValid(d.validPtr, row) {
			return nil
		}
		base := d.dataPtr + int32(row*16) // duckdb_list_entry{offset, length}
		off := int64(d.mod.readU64(base))
		n := int64(d.mod.readU64(base + 8))
		out := make([]any, n)
		for i := int64(0); i < n; i++ {
			out[i] = d.child.cell(off + i)
		}
		return out
	}
	if d.typeID == dtStruct {
		if !d.mod.readValid(d.validPtr, row) {
			return nil
		}
		// Child vectors are parallel arrays: row of the struct = row of each child.
		out := make(map[string]any, len(d.fields))
		for _, f := range d.fields {
			out[f.name] = f.dec.cell(row)
		}
		return out
	}
	if d.typeID == dtMap {
		if !d.mod.readValid(d.validPtr, row) {
			return nil
		}
		// List layout over the {key, value} struct child.
		base := d.dataPtr + int32(row*16) // duckdb_list_entry{offset, length}
		off := int64(d.mod.readU64(base))
		n := int64(d.mod.readU64(base + 8))
		if len(d.child.fields) != 2 {
			return nil // malformed map child; never expected
		}
		kd, vd := d.child.fields[0].dec, d.child.fields[1].dec
		out := make(map[any]any, n)
		for i := int64(0); i < n; i++ {
			k := kd.cell(off + i)
			if !comparableKey(k) {
				return nil // non-hashable key type (e.g. LIST key): unsupported
			}
			out[k] = vd.cell(off + i)
		}
		return out
	}
	if d.enum != nil {
		if !d.mod.readValid(d.validPtr, row) {
			return nil
		}
		return d.enum.value(d.mod, d.dataPtr, int(row))
	}
	if d.typeID == dtDecimal {
		// DECIMAL cells are delivered as the exact Decimal carrier (the same
		// type duckdb-go hands UDFs; numeric literals like 2.0 bind as DECIMAL).
		if !d.mod.readValid(d.validPtr, row) {
			return nil
		}
		unscaled := d.mod.readDecimalUnscaled(d.internal, d.dataPtr, row)
		if unscaled == nil {
			return nil
		}
		return Decimal{Width: uint8(d.width), Scale: uint8(d.scale), Value: unscaled}
	}
	v := d.mod.readCellT(d.typeID, d.scale, d.internal, d.dataPtr, d.validPtr, row)
	if d.isJSON {
		if s, ok := v.(string); ok {
			return JSONValue(s)
		}
	}
	return v
}

// comparableKey reports whether a decoded MAP key can be used as a Go map key
// (decoded LIST/STRUCT/BLOB keys are slices/maps and would panic on insert).
func comparableKey(k any) bool {
	if k == nil {
		return true
	}
	return reflect.TypeOf(k).Comparable()
}
