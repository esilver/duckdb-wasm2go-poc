// exotic.go — decode + DuckDB-exact VARCHAR rendering for the "exotic" scalar
// types the flat codec previously dropped to NULL, plus the VARIANT container:
//
//   - BIGNUM (duckdb_type 35, physical VARCHAR): a varint blob — 3-byte header
//     (sign bit + data-byte count) followed by big-endian magnitude bytes, all
//     bytes complemented for negatives (bignum.cpp Bignum::GetByteArray).
//     Decodes to the exact decimal string.
//   - GEOMETRY (40, physical VARCHAR): a little-endian WKB blob (geometry.cpp);
//     decodes to its WKT text exactly like Geometry::ToString — including
//     " Z "/" M "/" ZM " flags, EMPTY parts, and fmt-style coordinate
//     rendering with the trailing ".0" stripped.
//   - UUID (27, physical INT128): stored as a hugeint whose upper word has the
//     MSB flipped to preserve ordering (uuid.cpp BaseUUID::ToString); decodes
//     to the canonical lowercase 8-4-4-4-12 form.
//   - VARIANT (41, physical STRUCT(keys VARCHAR[], children STRUCT(keys_index
//     UINTEGER, values_index UINTEGER)[], values STRUCT(type_id UTINYINT,
//     byte_offset UINTEGER)[], data BLOB)): decoded straight out of the binary
//     encoding (variant.hpp VariantLogicalType + variant_utils.cpp) and
//     rendered to the exact string the upstream sqllogictest harness gets from
//     Value::CastAs(VARCHAR): scalars render as their VARCHAR casts, ARRAY
//     items render RAW (children are VARIANT-typed, the nested cast never
//     quotes them), OBJECT values quote-if-needed like any nested child.
package duckdb

import (
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// ---- BIGNUM ----------------------------------------------------------------

// bignumString renders a BIGNUM varint blob as its decimal string.
// Layout (bignum.cpp): 3 header bytes whose MSB is SET for non-negative
// values; data bytes are the big-endian magnitude, ALL bytes (header + data)
// complemented when negative.
func bignumString(blob []byte) string {
	if len(blob) < 4 {
		return "0" // malformed; a valid bignum has 3 header bytes + >=1 data byte
	}
	negative := blob[0]&0x80 == 0
	mag := make([]byte, len(blob)-3)
	copy(mag, blob[3:])
	if negative {
		for i := range mag {
			mag[i] = ^mag[i]
		}
	}
	v := new(big.Int).SetBytes(mag)
	if negative {
		v.Neg(v)
	}
	return v.String()
}

// ---- UUID ------------------------------------------------------------------

// uuidString renders the 16-byte UUID cell at dataPtr[row]. Storage is a
// hugeint (lower uint64 @0, upper int64 @8) with the upper word's sign bit
// flipped relative to the textual form (BaseUUID::ToString).
func uuidString(mod *module, dataPtr int32, row int) string {
	base := dataPtr + int32(row*16)
	return uuidFromWords(mod.readU64(base), mod.readI64(base+8))
}

// uuidFromWords renders the canonical lowercase 8-4-4-4-12 UUID from the
// stored hugeint words.
func uuidFromWords(lower uint64, upperRaw int64) string {
	upper := uint64(upperRaw) ^ (uint64(1) << 63)
	const hexd = "0123456789abcdef"
	var b [36]byte
	pos := 0
	put := func(v uint64, bytes int) {
		for i := bytes - 1; i >= 0; i-- {
			by := byte(v >> (uint(i) * 8))
			b[pos] = hexd[by>>4]
			b[pos+1] = hexd[by&0xF]
			pos += 2
		}
	}
	put(upper>>32, 4)
	b[pos] = '-'
	pos++
	put(upper>>16, 2)
	b[pos] = '-'
	pos++
	put(upper, 2)
	b[pos] = '-'
	pos++
	put(lower>>48, 2)
	b[pos] = '-'
	pos++
	put(lower, 6)
	return string(b[:])
}

// ---- GEOMETRY (WKB -> WKT) ---------------------------------------------------

// geometryString renders a GEOMETRY WKB blob as WKT, mirroring
// Geometry::ToString / ToStringRecursive (geometry.cpp). Returns "" only for
// malformed blobs (never expected from the engine).
func geometryString(blob []byte) string {
	r := &wkbReader{b: blob}
	var sb strings.Builder
	if !wktRecursive(r, &sb, 0) {
		return ""
	}
	return sb.String()
}

type wkbReader struct {
	b   []byte
	pos int
	bad bool
}

func (r *wkbReader) u8() byte {
	if r.pos+1 > len(r.b) {
		r.bad = true
		return 0
	}
	v := r.b[r.pos]
	r.pos++
	return v
}

func (r *wkbReader) u32() uint32 {
	if r.pos+4 > len(r.b) {
		r.bad = true
		return 0
	}
	v := uint32(r.b[r.pos]) | uint32(r.b[r.pos+1])<<8 | uint32(r.b[r.pos+2])<<16 | uint32(r.b[r.pos+3])<<24
	r.pos += 4
	return v
}

func (r *wkbReader) f64() float64 {
	if r.pos+8 > len(r.b) {
		r.bad = true
		return 0
	}
	var u uint64
	for i := 7; i >= 0; i-- {
		u = u<<8 | uint64(r.b[r.pos+i])
	}
	r.pos += 8
	return math.Float64frombits(u)
}

// wktCoord renders one coordinate like geometry.cpp's TextWriter::Write(double):
// duckdb_fmt "{}" (shortest round-trip, fixed for -4 <= exp < 16) with a
// trailing ".0" stripped when not in scientific notation.
func wktCoord(sb *strings.Builder, v float64) {
	s := formatDoubleDuckDB(v)
	if !strings.ContainsAny(s, "eE") {
		s = strings.TrimSuffix(s, ".0")
	}
	sb.WriteString(s)
}

// wkbHeader reads the byte order + meta word, returning geometry type id,
// has_z/has_m. Only little-endian (1) is produced by the engine.
func (r *wkbReader) wkbHeader() (gtype uint32, hasZ, hasM bool) {
	if bo := r.u8(); bo != 1 {
		r.bad = true
		return 0, false, false
	}
	meta := r.u32()
	gtype = (meta & 0xFFFF) % 1000
	flag := (meta & 0xFFFF) / 1000
	return gtype, flag&1 != 0, flag&2 != 0
}

// wktVerts writes "(x y[, x y]...)" for n vertices of dims dimensions.
func wktVerts(r *wkbReader, sb *strings.Builder, n uint32, dims int) {
	sb.WriteByte('(')
	for v := uint32(0); v < n; v++ {
		if v > 0 {
			sb.WriteString(", ")
		}
		for d := 0; d < dims; d++ {
			if d > 0 {
				sb.WriteByte(' ')
			}
			wktCoord(sb, r.f64())
		}
	}
	sb.WriteByte(')')
}

func wktRecursive(r *wkbReader, sb *strings.Builder, depth int) bool {
	if depth > 16 || r.bad {
		return false
	}
	gtype, hasZ, hasM := r.wkbHeader()
	if r.bad {
		return false
	}
	dims := 2
	if hasZ {
		dims++
	}
	if hasM {
		dims++
	}
	flagStr := " "
	switch {
	case hasZ && hasM:
		flagStr = " ZM "
	case hasZ:
		flagStr = " Z "
	case hasM:
		flagStr = " M "
	}
	names := map[uint32]string{1: "POINT", 2: "LINESTRING", 3: "POLYGON",
		4: "MULTIPOINT", 5: "MULTILINESTRING", 6: "MULTIPOLYGON", 7: "GEOMETRYCOLLECTION"}
	name, ok := names[gtype]
	if !ok {
		return false
	}
	sb.WriteString(name)
	sb.WriteString(flagStr)

	// readPoint writes "x y" (no parens), returning false when all dims are NaN.
	readPoint := func() bool {
		vals := make([]float64, dims)
		allNaN := true
		for d := 0; d < dims; d++ {
			vals[d] = r.f64()
			allNaN = allNaN && math.IsNaN(vals[d])
		}
		if allNaN {
			return false
		}
		for d := 0; d < dims; d++ {
			if d > 0 {
				sb.WriteByte(' ')
			}
			wktCoord(sb, vals[d])
		}
		return true
	}
	// polyBody writes "((ring), (ring))" given ring_count already consumed != 0.
	polyBody := func(ringCount uint32) {
		sb.WriteByte('(')
		for ri := uint32(0); ri < ringCount; ri++ {
			if ri > 0 {
				sb.WriteString(", ")
			}
			vc := r.u32()
			if vc == 0 {
				sb.WriteString("EMPTY")
				continue
			}
			wktVerts(r, sb, vc, dims)
		}
		sb.WriteByte(')')
	}

	switch gtype {
	case 1: // POINT
		mark := sb.Len()
		sb.WriteByte('(')
		if !readPoint() {
			truncateBuilder(sb, mark)
			sb.WriteString("EMPTY")
			return !r.bad
		}
		sb.WriteByte(')')
	case 2: // LINESTRING
		n := r.u32()
		if n == 0 {
			sb.WriteString("EMPTY")
			return !r.bad
		}
		wktVerts(r, sb, n, dims)
	case 3: // POLYGON
		rc := r.u32()
		if rc == 0 {
			sb.WriteString("EMPTY")
			return !r.bad
		}
		polyBody(rc)
	case 4: // MULTIPOINT
		n := r.u32()
		if n == 0 {
			sb.WriteString("EMPTY")
			return !r.bad
		}
		sb.WriteByte('(')
		for i := uint32(0); i < n; i++ {
			pt, pz, pm := r.wkbHeader()
			if r.bad || pt != 1 || pz != hasZ || pm != hasM {
				return false
			}
			if i > 0 {
				sb.WriteString(", ")
			}
			if !readPoint() {
				sb.WriteString("EMPTY")
			}
		}
		sb.WriteByte(')')
	case 5: // MULTILINESTRING
		n := r.u32()
		if n == 0 {
			sb.WriteString("EMPTY")
			return !r.bad
		}
		sb.WriteByte('(')
		for i := uint32(0); i < n; i++ {
			pt, pz, pm := r.wkbHeader()
			if r.bad || pt != 2 || pz != hasZ || pm != hasM {
				return false
			}
			if i > 0 {
				sb.WriteString(", ")
			}
			vc := r.u32()
			if vc == 0 {
				sb.WriteString("EMPTY")
				continue
			}
			wktVerts(r, sb, vc, dims)
		}
		sb.WriteByte(')')
	case 6: // MULTIPOLYGON
		n := r.u32()
		if n == 0 {
			sb.WriteString("EMPTY")
			return !r.bad
		}
		sb.WriteByte('(')
		for i := uint32(0); i < n; i++ {
			if i > 0 {
				sb.WriteString(", ")
			}
			pt, pz, pm := r.wkbHeader()
			if r.bad || pt != 3 || pz != hasZ || pm != hasM {
				return false
			}
			rc := r.u32()
			if rc == 0 {
				sb.WriteString("EMPTY")
				continue
			}
			polyBody(rc)
		}
		sb.WriteByte(')')
	case 7: // GEOMETRYCOLLECTION
		n := r.u32()
		if n == 0 {
			sb.WriteString("EMPTY")
			return !r.bad
		}
		sb.WriteByte('(')
		for i := uint32(0); i < n; i++ {
			if i > 0 {
				sb.WriteString(", ")
			}
			if !wktRecursive(r, sb, depth+1) {
				return false
			}
		}
		sb.WriteByte(')')
	}
	return !r.bad
}

// truncateBuilder rewinds sb to length n (strings.Builder has no Truncate;
// rebuild from the prefix — only used on the rare POINT EMPTY path).
func truncateBuilder(sb *strings.Builder, n int) {
	s := sb.String()[:n]
	sb.Reset()
	sb.WriteString(s)
}

// ---- shared DuckDB-exact scalar rendering -------------------------------------

// formatDoubleDuckDB / formatFloatDuckDB mirror DuckDB's float -> VARCHAR cast
// (duckdb_fmt "{}"): shortest-roundtrip digits, FIXED notation iff
// -4 <= floor(log10|v|) < 16, scientific otherwise, integral values with a
// trailing ".0".
func formatDoubleDuckDB(f float64) string { return formatFloatBitsDuckDB(f, 64) }
func formatFloatDuckDB(f float64) string  { return formatFloatBitsDuckDB(f, 32) }

func formatFloatBitsDuckDB(f float64, bits int) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	if bits == 32 {
		f = float64(float32(f))
	}
	e := strconv.FormatFloat(f, 'e', -1, bits)
	d, _ := strconv.Atoi(e[strings.IndexByte(e, 'e')+1:])
	if d >= -4 && d < 16 {
		s := strconv.FormatFloat(f, 'f', -1, bits)
		if !strings.ContainsRune(s, '.') {
			s += ".0"
		}
		return s
	}
	return e
}

// blobLiteral mirrors Blob::ToString: printable ASCII except \ ' " renders
// as-is, everything else as \xNN uppercase.
func blobLiteral(b []byte) string {
	const hexTable = "0123456789ABCDEF"
	var sb strings.Builder
	for _, c := range b {
		if c >= 32 && c <= 126 && c != '\\' && c != '\'' && c != '"' {
			sb.WriteByte(c)
		} else {
			sb.WriteByte('\\')
			sb.WriteByte('x')
			sb.WriteByte(hexTable[c>>4])
			sb.WriteByte(hexTable[c&0x0F])
		}
	}
	return sb.String()
}

// dateStringFromDays renders a DATE payload (days since epoch) like DuckDB's
// DATE -> VARCHAR cast, including ±infinity sentinels and the " (BC)" suffix.
func dateStringFromDays(days int32) string {
	switch days {
	case dateInfinity:
		return "infinity"
	case dateNegInfinity:
		return "-infinity"
	}
	t := epoch.AddDate(0, 0, int(days))
	return ymdString(t)
}

func ymdString(t time.Time) string {
	y, m, d := t.Date()
	var b []byte
	bc := y <= 0
	if bc {
		y = 1 - y
	}
	ys := strconv.Itoa(y)
	for len(ys) < 4 {
		ys = "0" + ys
	}
	b = append(b, ys...)
	b = append(b, '-')
	b = appendTwoDigits(b, int64(m))
	b = append(b, '-')
	b = appendTwoDigits(b, int64(d))
	if bc {
		b = append(b, " (BC)"...)
	}
	return string(b)
}

// timeStringMicros renders micros-since-midnight as HH:MM:SS[.ffffff]
// (Time::ToString; hours may exceed 23, e.g. TIME '24:00:00').
func timeStringMicros(micros int64) string {
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
	return string(b)
}

// timeStringNanos renders nanos-since-midnight with a 9-digit trimmed fraction.
func timeStringNanos(nanos int64) string {
	const nanosPerSec = int64(1000000000)
	secs := nanos / nanosPerSec
	frac := nanos % nanosPerSec
	s := timeStringMicros(secs * microsPerSec)
	if frac != 0 {
		var buf [9]byte
		f := frac
		for i := 8; i >= 0; i-- {
			buf[i] = byte('0' + f%10)
			f /= 10
		}
		n := 9
		for n > 1 && buf[n-1] == '0' {
			n--
		}
		s += "." + string(buf[:n])
	}
	return s
}

// timestampString renders a raw timestamp payload in `unit` since epoch like
// DuckDB's casts: "<date> <time>[frac]", ±infinity sentinels as text. tzSuffix
// is appended for TIMESTAMPTZ ("+00": the engine renders UTC without ICU
// session offsets in the VARIANT path).
func timestampString(raw int64, unit time.Duration, tzSuffix string) string {
	switch raw {
	case tsInfinity:
		return "infinity"
	case tsNegInfinity:
		return "-infinity"
	}
	// Decompose into days + intra-day remainder in `unit` ticks (floor division).
	perSec := int64(time.Second / unit)
	perDay := perSec * 86400
	days := raw / perDay
	rem := raw % perDay
	if rem < 0 {
		days--
		rem += perDay
	}
	var timePart string
	if unit == time.Nanosecond {
		timePart = timeStringNanos(rem)
	} else {
		// rem is in `unit` ticks; s/ms/us all convert to micros exactly.
		timePart = timeStringMicros(rem * (int64(unit) / int64(time.Microsecond)))
	}
	if days > math.MaxInt32 || days < math.MinInt32 {
		return "" // out of DATE range; never expected
	}
	return dateStringFromDays(int32(days)) + " " + timePart + tzSuffix
}
