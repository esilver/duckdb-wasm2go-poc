package duckdb

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// udf_agg_bigstr_test.go — regression probe for the HLL_COUNT panic seen through
// the googlesqlite bridge: nested aggregation where the INNER band aggregate
// finalizes a LARGE VARCHAR (an HLL sketch envelope is tens of KB of base64)
// into a GROUP BY result vector, and an OUTER band aggregate re-reads those
// large strings. Exercises duckdb_string_t heap strings + wasm memory growth
// across the aggregate callback boundary, engine-only (no emulator code).

// bigEmit finalizes to a deterministic large string: prefix + '#'-padding.
type bigEmit struct{ size int }

func (b bigEmit) NewState() any { return &bandState{} }
func (b bigEmit) Update(state any, args []any) {
	s := state.(*bandState)
	s.rows = append(s.rows, args)
}
func (b bigEmit) Combine(dst, src any) {
	d, s := dst.(*bandState), src.(*bandState)
	d.rows = append(d.rows, s.rows...)
}
func (b bigEmit) Finalize(state any) (any, error) {
	s := state.(*bandState)
	head := fmt.Sprintf("n=%d;", len(s.rows))
	return head + strings.Repeat("#", b.size-len(head)), nil
}

// bigFold consumes the large strings and finalizes to "count:totalbytes".
type bigFold struct{}

func (bigFold) NewState() any { return &bandState{} }
func (bigFold) Update(state any, args []any) {
	s := state.(*bandState)
	s.rows = append(s.rows, args)
}
func (bigFold) Combine(dst, src any) {
	d, s := dst.(*bandState), src.(*bandState)
	d.rows = append(d.rows, s.rows...)
}
func (bigFold) Finalize(state any) (any, error) {
	s := state.(*bandState)
	total := 0
	for _, row := range s.rows {
		if str, ok := row[0].(string); ok {
			total += len(str)
		}
	}
	return fmt.Sprintf("%d:%d", len(s.rows), total), nil
}

func TestAggBandLargeStringNested(t *testing.T) {
	c, done := bandConn(t)
	defer done()

	const sketchSize = 64 * 1024 // ~an HLL sketch envelope
	if err := RegisterAggregateBandConn(c, "big_emit",
		AggregateOptions{MinArgs: 1, MaxArgs: 1, ResultTypeID: dtVarchar}, bigEmit{size: sketchSize}); err != nil {
		t.Fatalf("register big_emit: %v", err)
	}
	if err := RegisterAggregateBandConn(c, "big_fold",
		AggregateOptions{MinArgs: 1, MaxArgs: 1, ResultTypeID: dtVarchar}, bigFold{}); err != nil {
		t.Fatalf("register big_fold: %v", err)
	}

	// The HLL INIT+MERGE shape: inner grouped aggregate emits one large string
	// per group; outer aggregate folds them.
	var out string
	err := c.QueryRowContext(context.Background(), `
		SELECT big_fold(s) FROM (
			SELECT big_emit(x) AS s FROM range(0, 600) t(x) GROUP BY x % 6
		)`).Scan(&out)
	if err != nil {
		t.Fatalf("nested large-string aggregation: %v", err)
	}
	want := fmt.Sprintf("6:%d", 6*sketchSize)
	if out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
	t.Logf("nested aggregation over 6 × %dKB strings ✓ (%s)", sketchSize/1024, out)
}
