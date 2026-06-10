// typename.go — DuckDB type-name rendering for driver.RowsColumnTypeDatabaseTypeName.
//
// database/sql surfaces no structured column-type metadata, only the
// DatabaseTypeName string (duckdb-go implements the same hook). Consumers that
// must reproduce DuckDB's exact VARCHAR rendering of a value (the sqllogictest
// runner) need the TEMPORAL FACE of a column — TIME, DATE and TIMESTAMP all
// decode to a bare time.Time, which renders three different ways natively — and
// need it RECURSIVELY for nested columns (a TIME inside a LIST renders
// '12:13:14' while a TIMESTAMP inside a LIST renders '2000-01-01 12:13:14').
// The name is rendered DuckDB-style: scalar names as duckdb_type names
// (TIMESTAMP WITH TIME ZONE etc.), LIST as `child[]`, ARRAY as `child[N]`,
// MAP(key, value), and STRUCT/UNION with ALWAYS-double-quoted field names
// (`STRUCT("a" TIME)`) so the consumer-side parser never has to guess whether
// an identifier was quoted.
package duckdb

import (
	"fmt"
	"strings"
)

// scalarTypeNames maps flat duckdb_type ids to DuckDB's canonical type names
// (LogicalTypeIdToString / typeof() spelling).
var scalarTypeNames = map[int32]string{
	dtBoolean:     "BOOLEAN",
	dtTinyint:     "TINYINT",
	dtSmallint:    "SMALLINT",
	dtInteger:     "INTEGER",
	dtBigint:      "BIGINT",
	dtUtinyint:    "UTINYINT",
	dtUsmallint:   "USMALLINT",
	dtUinteger:    "UINTEGER",
	dtUbigint:     "UBIGINT",
	dtFloat:       "FLOAT",
	dtDouble:      "DOUBLE",
	dtTimestamp:   "TIMESTAMP",
	dtDate:        "DATE",
	dtTime:        "TIME",
	dtInterval:    "INTERVAL",
	dtHugeint:     "HUGEINT",
	dtUhugeint:    "UHUGEINT",
	dtBlob:        "BLOB",
	dtTimestampS:  "TIMESTAMP_S",
	dtTimestampMs: "TIMESTAMP_MS",
	dtTimestampNs: "TIMESTAMP_NS",
	dtUuid:        "UUID",
	dtBit:         "BIT",
	dtTimeTz:      "TIME WITH TIME ZONE",
	dtTimestampTz: "TIMESTAMP WITH TIME ZONE",
	dtTimeNs:      "TIME_NS",
	dtBignum:      "BIGNUM",
	dtGeometry:    "GEOMETRY",
	dtVariant:     "VARIANT",
}

// typeName renders the logical type lt's DuckDB type name, recursing into
// nested types. Child logical-type handles created here are destroyed here;
// lt itself stays owned by the caller. Unknown type ids render as "".
func typeName(mod *module, lt int32) string {
	m := mod.m
	switch tid := m.Xduckdb_get_type_id(lt); tid {
	case dtDecimal:
		return fmt.Sprintf("DECIMAL(%d,%d)", m.Xduckdb_decimal_width(lt), m.Xduckdb_decimal_scale(lt))
	case dtVarchar:
		// JSON is VARCHAR-backed; the alias is its only marker.
		if ap := m.Xduckdb_logical_type_get_alias(lt); ap != 0 {
			alias := mod.goString(ap)
			m.Xduckdb_free(ap)
			if alias != "" {
				return alias
			}
		}
		return "VARCHAR"
	case dtEnum:
		// Members are irrelevant to consumers (cells decode to plain strings).
		return "ENUM"
	case dtList:
		child := m.Xduckdb_list_type_child_type(lt)
		s := typeName(mod, child) + "[]"
		destroyLogicalType(mod, child)
		return s
	case dtArray:
		child := m.Xduckdb_array_type_child_type(lt)
		s := fmt.Sprintf("%s[%d]", typeName(mod, child), m.Xduckdb_array_type_array_size(lt))
		destroyLogicalType(mod, child)
		return s
	case dtMap:
		kt := m.Xduckdb_map_type_key_type(lt)
		vt := m.Xduckdb_map_type_value_type(lt)
		s := "MAP(" + typeName(mod, kt) + ", " + typeName(mod, vt) + ")"
		destroyLogicalType(mod, kt)
		destroyLogicalType(mod, vt)
		return s
	case dtStruct:
		n := m.Xduckdb_struct_type_child_count(lt)
		parts := make([]string, n)
		for i := int64(0); i < n; i++ {
			namePtr := m.Xduckdb_struct_type_child_name(lt, i) // malloc'd char*
			name := mod.goString(namePtr)
			m.Xduckdb_free(namePtr)
			ct := m.Xduckdb_struct_type_child_type(lt, i)
			parts[i] = quoteTypeIdent(name) + " " + typeName(mod, ct)
			destroyLogicalType(mod, ct)
		}
		return "STRUCT(" + strings.Join(parts, ", ") + ")"
	case dtUnion:
		n := m.Xduckdb_union_type_member_count(lt)
		parts := make([]string, n)
		for i := int64(0); i < n; i++ {
			namePtr := m.Xduckdb_union_type_member_name(lt, i) // malloc'd char*
			name := mod.goString(namePtr)
			m.Xduckdb_free(namePtr)
			mt := m.Xduckdb_union_type_member_type(lt, i)
			parts[i] = quoteTypeIdent(name) + " " + typeName(mod, mt)
			destroyLogicalType(mod, mt)
		}
		return "UNION(" + strings.Join(parts, ", ") + ")"
	default:
		return scalarTypeNames[tid] // "" for unknown ids (incl. SQLNULL/ANY)
	}
}

// quoteTypeIdent double-quotes a STRUCT/UNION field name unconditionally,
// doubling embedded quotes, so type-string consumers can parse field entries
// without identifier-quoting heuristics.
func quoteTypeIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
