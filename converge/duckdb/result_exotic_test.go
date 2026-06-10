package duckdb

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// result_exotic_test.go — engine-level database/sql round trips for the "exotic"
// scalar types that previously decoded to nil (INTERVAL, UNION, fixed-size ARRAY,
// TIMETZ, BIT, timestamp/date ±infinity, UHUGEINT), plus nested recursion through
// LIST/STRUCT so the vecDecoder path is covered too.

// queryOne runs a single-value query and returns the scanned driver value.
func queryOne(t *testing.T, q string) any {
	t.Helper()
	c, done := bandConn(t)
	defer done()
	var v any
	if err := c.QueryRowContext(context.Background(), q).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

// expectString asserts the query's single value renders (fmt %v) as want.
func expectString(t *testing.T, q, want string) {
	t.Helper()
	v := queryOne(t, q)
	if got := fmt.Sprintf("%v", v); got != want {
		t.Fatalf("%s: got %T %q, want %q", q, v, got, want)
	}
}

// TestIntervalString checks the pure-Go port of IntervalToStringCast::Format
// against DuckDB's documented rendering rules (plurals, time formatting,
// negative components, the empty-interval default).
func TestIntervalString(t *testing.T) {
	cases := []struct {
		iv   Interval
		want string
	}{
		{Interval{}, "00:00:00"},
		{Interval{Months: 525, Days: 27}, "43 years 9 months 27 days"}, // 525 = 43*12+9
		{Interval{Days: 2}, "2 days"},
		{Interval{Days: 1}, "1 day"},
		{Interval{Days: -1}, "-1 day"},
		{Interval{Months: 1}, "1 month"},
		{Interval{Months: 14}, "1 year 2 months"},
		{Interval{Months: -13}, "-1 year -1 month"},
		{Interval{Micros: 1500000}, "00:00:01.5"},
		{Interval{Micros: -100}, "-00:00:00.0001"},
		{Interval{Micros: 100 * microsPerHour}, "100:00:00"},
		{Interval{Months: -1, Micros: -100}, "-1 month -00:00:00.0001"},
		{Interval{Months: 1, Days: 1, Micros: microsPerSec}, "1 month 1 day 00:00:01"},
		{Interval{Micros: 86400*microsPerSec + 30*microsPerMinute}, "24:30:00"},
	}
	for _, c := range cases {
		if got := c.iv.String(); got != c.want {
			t.Errorf("Interval%+v.String() = %q, want %q", c.iv, got, c.want)
		}
	}
}

// TestIntervalResult: INTERVAL result columns decode to Interval values whose
// rendering matches DuckDB's VARCHAR cast (interval_constants.test shapes).
func TestIntervalResult(t *testing.T) {
	v := queryOne(t, "SELECT INTERVAL 43 YEARS + INTERVAL 9 MONTHS + INTERVAL 27 DAYS")
	iv, ok := v.(Interval)
	if !ok {
		t.Fatalf("interval scans as %T, want duckdb.Interval", v)
	}
	if iv != (Interval{Months: 525, Days: 27}) {
		t.Fatalf("interval fields: got %+v, want {525 27 0}", iv)
	}
	if got := iv.String(); got != "43 years 9 months 27 days" {
		t.Fatalf("interval render: got %q", got)
	}
	expectString(t, "SELECT INTERVAL 2 DAYS", "2 days")
	expectString(t, "SELECT INTERVAL '1.5' SECONDS", "00:00:01.5")
	expectString(t, "SELECT INTERVAL '0' SECONDS", "00:00:00")
	expectString(t, "SELECT INTERVAL '-1 month' - INTERVAL '0.0001' SECONDS",
		"-1 month -00:00:00.0001")
	// engine VARCHAR cast and Go-side String() must agree
	var ivAny, s any
	c, done := bandConn(t)
	defer done()
	if err := c.QueryRowContext(context.Background(),
		"SELECT i, i::VARCHAR FROM (SELECT INTERVAL '90' MINUTES + INTERVAL '1' DAY AS i)").
		Scan(&ivAny, &s); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%v", ivAny) != s.(string) {
		t.Fatalf("Go render %v != engine cast %v", ivAny, s)
	}
}

// TestUnionResult: UNION cells deliver the ACTIVE member's decoded value
// (duckdb-go scan semantics; types/union corpus).
func TestUnionResult(t *testing.T) {
	if v := queryOne(t, "SELECT union_value(num := 1)"); v != int64(1) {
		t.Fatalf("union_value(num := 1): got %T %v, want int64 1", v, v)
	}
	if v := queryOne(t, "SELECT union_value(str := 'two')"); v != "two" {
		t.Fatalf("union_value(str := 'two'): got %T %v, want \"two\"", v, v)
	}

	// Mixed members + NULL through a typed column.
	c, done := bandConn(t)
	defer done()
	ctx := context.Background()
	for _, stmt := range []string{
		"CREATE TABLE tbl(u UNION(num INTEGER, str VARCHAR))",
		"INSERT INTO tbl VALUES (1), ('two'), (NULL)",
	} {
		if _, err := c.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := c.QueryContext(ctx, "SELECT u FROM tbl")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []any
	for rows.Next() {
		var v any
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		got = append(got, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []any{int64(1), "two", nil}
	if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
		t.Fatalf("union column: got %v, want %v", got, want)
	}
}

// TestArrayResult: fixed-size ARRAY columns decode to []any like LIST
// (nested/array corpus, array_limit_offset.test shape).
func TestArrayResult(t *testing.T) {
	expectString(t, "SELECT [1,2,3]::INT[3]", "[1 2 3]")
	expectString(t, "SELECT ['a','b']::VARCHAR[2]", "[a b]")
	// ARRAY nested in LIST, and ARRAY of ARRAY.
	expectString(t, "SELECT [[1,2,3]::INT[3]]", "[[1 2 3]]")
	expectString(t, "SELECT [[1,2],[3,4]]::INT[2][2]", "[[1 2] [3 4]]")
	// NULL array and NULL element.
	if v := queryOne(t, "SELECT NULL::INT[3]"); v != nil {
		t.Fatalf("NULL array: got %v", v)
	}
	expectString(t, "SELECT [1,NULL,3]::INT[3]", "[1 <nil> 3]")
}

// TestTimeTZResult: TIMETZ decodes to DuckDB's exact VARCHAR rendering
// (micros<<24 | reverse-biased offset packing).
func TestTimeTZResult(t *testing.T) {
	expectString(t, "SELECT '00:00:00+15:59'::TIMETZ", "00:00:00+15:59")
	expectString(t, "SELECT '11:30:00.123456-02:00'::TIMETZ", "11:30:00.123456-02")
	expectString(t, "SELECT '10:00:00+05:30'::TIMETZ", "10:00:00+05:30")
	expectString(t, "SELECT '01:02:03.5+00:00'::TIMETZ", "01:02:03.5+00")
	expectString(t, "SELECT '23:59:59-15:59:59'::TIMETZ", "23:59:59-15:59:59")
}

// TestBitResult: BIT decodes to the "0101..." string DuckDB's VARCHAR cast
// produces (first blob byte = padding bit count; bits MSB-first).
func TestBitResult(t *testing.T) {
	expectString(t, "SELECT '0101011'::BIT", "0101011")
	expectString(t, "SELECT '1'::BIT", "1")
	expectString(t, "SELECT '00000000'::BIT", "00000000")
	expectString(t, "SELECT '0000000000000000001'::BIT", "0000000000000000001")
	expectString(t, "SELECT '10101'::BIT | '01010'::BIT", "11111")
}

// TestTimestampInfinity: the ±infinity sentinels (INT64_MAX / -INT64_MAX micros;
// INT32_MAX / -INT32_MAX days for DATE) deliver the strings DuckDB renders,
// while normal values stay time.Time (test_infinite_time.test).
func TestTimestampInfinity(t *testing.T) {
	expectString(t, "SELECT 'infinity'::TIMESTAMP", "infinity")
	expectString(t, "SELECT '-infinity'::TIMESTAMP", "-infinity")
	expectString(t, "SELECT 'infinity'::TIMESTAMPTZ", "infinity")
	expectString(t, "SELECT 'infinity'::TIMESTAMP_S", "infinity")
	expectString(t, "SELECT '-infinity'::TIMESTAMP_MS", "-infinity")
	expectString(t, "SELECT 'infinity'::DATE", "infinity")
	expectString(t, "SELECT '-infinity'::DATE", "-infinity")
	// Normal values still decode as time.Time.
	v := queryOne(t, "SELECT TIMESTAMP '2024-03-05 06:07:08'")
	ts, ok := v.(time.Time)
	if !ok || !ts.Equal(time.Date(2024, 3, 5, 6, 7, 8, 0, time.UTC)) {
		t.Fatalf("normal timestamp: got %T %v", v, v)
	}
	if v := queryOne(t, "SELECT DATE '2024-03-05'"); !v.(time.Time).Equal(
		time.Date(2024, 3, 5, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("normal date: got %v", v)
	}
}

// TestHugeintResults: HUGEINT/UHUGEINT result columns decode (int64 when they
// fit, decimal string beyond — uhugeint_try_cast.test range).
func TestHugeintResults(t *testing.T) {
	if v := queryOne(t, "SELECT 42::HUGEINT"); v != int64(42) {
		t.Fatalf("small hugeint: got %T %v", v, v)
	}
	expectString(t, "SELECT 170141183460469231731687303715884105727::HUGEINT",
		"170141183460469231731687303715884105727")
	expectString(t, "SELECT -170141183460469231731687303715884105727::HUGEINT",
		"-170141183460469231731687303715884105727")
	if v := queryOne(t, "SELECT 42::UHUGEINT"); v != int64(42) {
		t.Fatalf("small uhugeint: got %T %v", v, v)
	}
	expectString(t, "SELECT 340282366920938463463374607431768211455::UHUGEINT",
		"340282366920938463463374607431768211455")
}

// TestExoticNested: the new flat decodes recurse through LIST/STRUCT children
// (vecDecoder readCellT path) — LIST<INTERVAL>, STRUCT{BIT}, LIST<UNION>,
// LIST<TIMESTAMP-infinity>, ARRAY<INTERVAL>.
func TestExoticNested(t *testing.T) {
	expectString(t, "SELECT [INTERVAL 2 DAYS]", "[2 days]")
	expectString(t, "SELECT {b: '0101'::BIT}", "{'b': 0101}")
	expectString(t, "SELECT [union_value(num := 1)]", "[1]")
	expectString(t, "SELECT ['infinity'::TIMESTAMP]", "[infinity]")
	expectString(t, "SELECT [INTERVAL 2 DAYS]::INTERVAL[1]", "[2 days]")
	expectString(t, "SELECT ['00:00:00+15:59'::TIMETZ]", "[00:00:00+15:59]")
	expectString(t, "SELECT [170141183460469231731687303715884105727::HUGEINT]",
		"[170141183460469231731687303715884105727]")
}

// TestUUIDResult: UUID columns decode to the canonical 8-4-4-4-12 form
// (BaseUUID::ToString MSB-flip), flat and nested.
func TestUUIDResult(t *testing.T) {
	expectString(t, "SELECT '00112233-4455-6677-8899-aabbccddeeff'::UUID",
		"00112233-4455-6677-8899-aabbccddeeff")
	expectString(t, "SELECT 'ffffffff-ffff-ffff-ffff-ffffffffffff'::UUID",
		"ffffffff-ffff-ffff-ffff-ffffffffffff")
	expectString(t, "SELECT ['00000000-0000-0000-0000-000000000000'::UUID]",
		"[00000000-0000-0000-0000-000000000000]")
}

// TestBignumResult: BIGNUM (varint) columns decode to the exact decimal string
// (test_bignum_sum.test shapes), flat and nested.
func TestBignumResult(t *testing.T) {
	expectString(t, "SELECT 9223372036854775808::BIGNUM + 1::BIGNUM", "9223372036854775809")
	expectString(t, "SELECT (-10)::BIGNUM + (-1)::BIGNUM", "-11")
	expectString(t, "SELECT 0::BIGNUM", "0")
	expectString(t, "SELECT [1::BIGNUM]", "[1]")
}

// TestGeometryResult: GEOMETRY columns decode WKB to WKT (Geometry::ToString),
// including EMPTY parts and collections.
func TestGeometryResult(t *testing.T) {
	expectString(t, "SELECT 'POINT (1 2)'::GEOMETRY", "POINT (1 2)")
	expectString(t, "SELECT 'POINT EMPTY'::GEOMETRY", "POINT EMPTY")
	expectString(t, "SELECT 'LINESTRING (0 0, 1 1)'::GEOMETRY", "LINESTRING (0 0, 1 1)")
	expectString(t, "SELECT 'POLYGON ((0 0, 0 1, 1 1, 1 0, 0 0))'::GEOMETRY",
		"POLYGON ((0 0, 0 1, 1 1, 1 0, 0 0))")
	expectString(t, "SELECT 'POINT Z (1 2 3)'::GEOMETRY", "POINT Z (1 2 3)")
	expectString(t, "SELECT 'GEOMETRYCOLLECTION (POINT (1 2), LINESTRING EMPTY)'::GEOMETRY",
		"GEOMETRYCOLLECTION (POINT (1 2), LINESTRING EMPTY)")
	expectString(t, "SELECT 'POINT (0.5 -1.25)'::GEOMETRY", "POINT (0.5 -1.25)")
}

// TestVariantResult: VARIANT cells decode to the exact Value::CastAs(VARCHAR)
// string — scalars bare, ARRAY items raw, OBJECT values quoted-if-needed
// (json_cast.test / test_all_types.test shapes).
func TestVariantResult(t *testing.T) {
	expectString(t, `SELECT '"test"'::JSON::VARIANT`, "test")
	expectString(t, "SELECT 42::VARIANT", "42")
	expectString(t, "SELECT {'a': true, 'b': 42}::VARIANT", "{'a': true, 'b': 42}")
	expectString(t, "SELECT [1, 2, 3]::VARIANT", "[1, 2, 3]")
	expectString(t, `SELECT '{"hello": [1, true, null]}'::JSON::VARIANT`,
		"{'hello': [1, true, NULL]}")
	expectString(t, "SELECT {'t': '00:11:22'::TIME}::VARIANT", "{'t': '00:11:22'}")
	expectString(t, "SELECT 1.5::FLOAT::VARIANT", "1.5")
	expectString(t, "SELECT '2020-05-05'::DATE::VARIANT", "2020-05-05")
	// VARIANT inside a regular LIST goes through the vecDecoder path.
	expectString(t, "SELECT [42::VARIANT, NULL]", "[42 <nil>]")
}
