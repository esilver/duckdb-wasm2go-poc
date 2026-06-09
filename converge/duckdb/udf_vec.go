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
//
// STRUCT/MAP/UNION arguments remain undecoded (nil) — nothing in the
// googlesqlite surface passes them natively yet (structs travel as value-layer
// envelopes or JSON).
package duckdb

// dtList is DUCKDB_TYPE_LIST (only a logical type; the flat codec can't decode
// it, vecDecoder handles it structurally).
const dtList = 24

type vecDecoder struct {
	mod      *module
	dataPtr  int32
	validPtr int32
	typeID   int32
	width    int32 // DECIMAL only
	scale    int32 // DECIMAL only
	internal int32 // DECIMAL backing integer type id
	isJSON   bool  // VARCHAR with the JSON alias
	child    *vecDecoder
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
	case dtList:
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
