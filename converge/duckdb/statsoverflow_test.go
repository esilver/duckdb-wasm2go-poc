package duckdb

import (
	"context"
	"testing"
)

// TestStatsRangeOverflowCaught covers the exhost catch-all/typed-clause id
// collision (KNOWN-LIMITS class J). CompressedMaterialization's
// GetIntegralRangeValue evaluates `stats_max - stats_min` through
// ExpressionExecutor::TryEvaluateScalar, whose landing pad is
//
//	catch (InternalException &) { throw; } catch (...) { return false; }
//
// exhost reserved id 1 for the catch-all but ALSO handed id 1 to the first
// real typeinfo (InternalException), so the catch-all match entered the
// InternalException clause and rethrew: queries whose column stats span more
// than the type's range errored with "Out of Range Error: Overflow in
// subtraction of INT64 (max - min)!" where native DuckDB v1.5.3 silently
// skips the compression. Exercises the three corpus repro shapes:
// ORDER BY (compress_order), GROUP BY (compress_aggregate), and LAG with an
// extreme SMALLINT offset column.
func TestStatsRangeOverflowCaught(t *testing.T) {
	ctx := context.Background()
	_, c := openSingleConn(t, ":memory:")
	mustExec := func(q string) {
		t.Helper()
		if _, err := c.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// ORDER BY over full-range BIGINT stats (hugeint_order_by_extremes shape).
	mustExec("CREATE TABLE so_big(a BIGINT)")
	mustExec("INSERT INTO so_big VALUES (-9223372036854775808), (9223372036854775807), (0)")
	rows, err := c.QueryContext(ctx, "SELECT a FROM so_big ORDER BY a")
	if err != nil {
		t.Fatalf("ORDER BY with extreme stats: %v", err)
	}
	var got []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		got = append(got, v)
	}
	rows.Close()
	if len(got) != 3 || got[0] != -9223372036854775808 || got[2] != 9223372036854775807 {
		t.Fatalf("wrong order result: %v", got)
	}

	// HUGEINT extremes (the exact corpus file shape).
	mustExec("CREATE TABLE so_huge(a HUGEINT)")
	mustExec("INSERT INTO so_huge VALUES ((-170141183460469231731687303715884105728)::HUGEINT), (1111::HUGEINT)")
	if _, err := c.ExecContext(ctx, "SELECT a FROM so_huge ORDER BY a"); err != nil {
		t.Fatalf("hugeint ORDER BY: %v", err)
	}

	// GROUP BY over full-range stats (test_null_aggregates shape).
	if _, err := c.ExecContext(ctx,
		"SELECT a, count(*) FROM so_big GROUP BY a ORDER BY a"); err != nil {
		t.Fatalf("GROUP BY with extreme stats: %v", err)
	}

	// LAG with a SMALLINT offset column spanning ±32767 (test_lead_lag shape).
	mustExec("CREATE TABLE so_lag(c1 INT, c2 SMALLINT, c3 BIT)")
	mustExec("INSERT INTO so_lag VALUES (0, NULL, NULL), (1, 32767, '101'), (2, -32767, '101'), (3, 0, '000'), (4, NULL, NULL)")
	if _, err := c.ExecContext(ctx,
		"SELECT c1, LAG(c3, c2, BIT'010101010') OVER (PARTITION BY c1 ORDER BY c3) FROM so_lag ORDER BY c1"); err != nil {
		t.Fatalf("LAG with extreme offset stats: %v", err)
	}
}
