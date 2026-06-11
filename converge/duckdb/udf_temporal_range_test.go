package duckdb

import (
	"testing"
	"time"
)

// TestScalarUDFTemporalResultRange: DATE / TIMESTAMP results returned from a
// scalar UDF must survive the full BigQuery temporal range (years 1-9999).
// Regression: writeCell encoded both via t.Sub(epoch), whose intermediate
// time.Duration is int64 nanoseconds and saturates ±292 years from 1970 —
// so a UDF returning year 3 (googlesqlite's PARSE_DATETIME lowering) came
// back as 1677-09-21 00:12:43.145225, the MinInt64-nanos clamp.
func TestScalarUDFTemporalResultRange(t *testing.T) {
	mod := newModule()
	con, _, err := mod.open(":memory:")
	if err != nil {
		t.Fatal(err)
	}

	cases := []time.Time{
		time.Date(3, time.January, 11, 0, 0, 0, 0, time.UTC),
		time.Date(1582, time.October, 4, 23, 59, 59, 0, time.UTC),
		time.Date(2026, time.June, 11, 12, 34, 56, 789000000, time.UTC),
		time.Date(9999, time.December, 31, 23, 59, 59, 999999000, time.UTC),
	}
	idx := 0
	pick := func(args []any) (any, error) { return cases[idx], nil }

	if err := mod.registerScalarEx(con, "tr_ts", nil, dtTimestamp, dtAny, true, false, pick); err != nil {
		t.Fatal(err)
	}
	if err := mod.registerScalarEx(con, "tr_date", nil, dtDate, dtAny, true, false, pick); err != nil {
		t.Fatal(err)
	}

	q := func(sql string) string {
		res := mod.allocOut(sizeofDuckdbResult)
		if rc := mod.m.Xduckdb_query(con, mod.cstring(sql), res); rc != 0 {
			t.Fatalf("%s: %s", sql, mod.lastError())
		}
		return mod.goString(mod.m.Xduckdb_value_varchar(res, 0, 0))
	}

	wantTS := []string{
		"0003-01-11 00:00:00",
		"1582-10-04 23:59:59",
		"2026-06-11 12:34:56.789",
		"9999-12-31 23:59:59.999999",
	}
	wantDate := []string{"0003-01-11", "1582-10-04", "2026-06-11", "9999-12-31"}

	for i := range cases {
		idx = i
		if got := q(`SELECT tr_ts()::VARCHAR`); got != wantTS[i] {
			t.Errorf("TIMESTAMP result %d: got %q, want %q", i, got, wantTS[i])
		}
		if got := q(`SELECT tr_date()::VARCHAR`); got != wantDate[i] {
			t.Errorf("DATE result %d: got %q, want %q", i, got, wantDate[i])
		}
	}
}
