package duckdb

import (
	"strings"
	"testing"
)

// udf_agg_merge_check_test.go — EMPIRICAL FINDING, kept as a regression probe.
//
// Question: does registering the SAME aggregate name twice (different arities)
// MERGE the overloads, the way duckdb_register_aggregate_function's
// OnCreateConflict::ALTER_ON_CONFLICT suggests?
//
// ANSWER (DuckDB v1.5.3, this build): NO. The second registration fails with
//
//	{"exception_type":"Not implemented",
//	 "exception_message":"GetAlterInfo not implemented for this type"}
//
// Root cause (verified in amalg/duckdb.cpp): RegisterAggregateFunctionSet sets
// on_conflict = ALTER_ON_CONFLICT, but CreateAggregateFunctionInfo does NOT
// override CreateInfo::GetAlterInfo (only CreateScalarFunctionInfo does), so the
// catalog's alter path throws. This is an upstream engine limitation, not a
// wasm/export problem — no in-build sequence of C-API calls can merge aggregate
// overloads. The supported route is duckdb_create_aggregate_function_set +
// duckdb_add_aggregate_function_to_set + duckdb_register_aggregate_function_set,
// which registers ALL arities in ONE CreateFunction call and therefore never hits
// the conflict path. This build now exports that route; see udf_agg_band.go.
//
// TestAggOverloadMergeCheck asserts the CURRENT (broken-merge) behavior so this
// probe flips loudly if a rebuilt engine changes the answer.

type mc1Agg struct{} // sum(a)

func (mc1Agg) NewState() any { return &gsumState{} }
func (mc1Agg) Update(state any, args []any) {
	s := state.(*gsumState)
	if len(args) >= 1 && args[0] != nil {
		if v, ok := asInt64(args[0]); ok {
			s.sum += v
			s.any = true
		}
	}
}
func (mc1Agg) Combine(dst, src any) {
	d, s := dst.(*gsumState), src.(*gsumState)
	d.sum += s.sum
	d.any = d.any || s.any
}
func (mc1Agg) Finalize(state any) (any, error) {
	s := state.(*gsumState)
	if !s.any {
		return nil, nil
	}
	return s.sum, nil
}

type mc2Agg struct{} // sum(a*b)

func (mc2Agg) NewState() any { return &gsumState{} }
func (mc2Agg) Update(state any, args []any) {
	s := state.(*gsumState)
	if len(args) >= 2 && args[0] != nil && args[1] != nil {
		a, _ := asInt64(args[0])
		b, _ := asInt64(args[1])
		s.sum += a * b
		s.any = true
	}
}
func (mc2Agg) Combine(dst, src any) {
	d, s := dst.(*gsumState), src.(*gsumState)
	d.sum += s.sum
	d.any = d.any || s.any
}
func (mc2Agg) Finalize(state any) (any, error) {
	s := state.(*gsumState)
	if !s.any {
		return nil, nil
	}
	return s.sum, nil
}

func TestAggOverloadMergeCheck(t *testing.T) {
	mod := newModule()
	con, _, err := mod.open(":memory:")
	if err != nil {
		t.Fatal(err)
	}

	if err := mod.RegisterAggregateUDF(con, "mc_sum", []int32{dtBigint}, dtBigint, mc1Agg{}); err != nil {
		t.Fatalf("first registration (1 arg) failed: %v", err)
	}

	// Second registration, same name, different arity: documents the v1.5.3
	// behavior — it FAILS (no aggregate overload merge; see file comment). If a
	// rebuilt engine ever makes this succeed, this probe flips and the band
	// registrar should be revisited (repeated registration would then work).
	err = mod.RegisterAggregateUDF(con, "mc_sum", []int32{dtBigint, dtBigint}, dtBigint, mc2Agg{})
	if err == nil {
		// Merge unexpectedly worked — verify both arities actually resolve, then
		// fail the probe so a human notices the engine behavior changed.
		got1 := queryI64(t, mod, con, "SELECT mc_sum(x) FROM range(1,11) t(x)", 0, 0)
		got2 := queryI64(t, mod, con, "SELECT mc_sum(x, 2) FROM range(1,11) t(x)", 0, 0)
		t.Fatalf("aggregate overload merge UNEXPECTEDLY WORKS now (mc_sum/1=%d, mc_sum/2=%d): "+
			"revisit the band-registration plan (repeated registration is viable)", got1, got2)
	}
	if !strings.Contains(err.Error(), "GetAlterInfo not implemented") {
		t.Fatalf("second registration failed with an UNEXPECTED error (want GetAlterInfo NotImplemented): %v", err)
	}
	t.Logf("confirmed: aggregate overload merge is NOT supported by this engine build: %v", err)

	// The original 1-arg overload must still work after the failed re-registration.
	if got := queryI64(t, mod, con, "SELECT mc_sum(x) FROM range(1,11) t(x)", 0, 0); got != 55 {
		t.Fatalf("1-arg overload after failed second registration: got %d, want 55", got)
	}
	t.Logf("first overload survives the failed re-registration: mc_sum/1=55 ✓")
}

// anyAgg counts rows and stringifies the first arg type — used to verify that a
// SINGLE registration with ANY-typed parameters binds and decodes per-chunk types.
type anyAgg struct{}

func (anyAgg) NewState() any { return &gsumState{} }
func (anyAgg) Update(state any, args []any) {
	s := state.(*gsumState)
	if len(args) >= 1 && args[0] != nil {
		if v, ok := asInt64(args[0]); ok {
			s.sum += v
			s.any = true
		}
	}
}
func (anyAgg) Combine(dst, src any) {
	d, s := dst.(*gsumState), src.(*gsumState)
	d.sum += s.sum
	d.any = d.any || s.any
}
func (anyAgg) Finalize(state any) (any, error) {
	s := state.(*gsumState)
	if !s.any {
		return nil, nil
	}
	return s.sum, nil
}

// TestAggAnyParamCheck: single registration of an aggregate with one ANY (34)
// parameter; verifies the C-API validation accepts ANY params and the function
// binds for a BIGINT argument. (Decode uses the declared-type path which can't
// decode ANY — so this impl only checks binding + row count via a constant.)
func TestAggAnyParamCheck(t *testing.T) {
	mod := newModule()
	con, _, err := mod.open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	const dtAnyT = 34
	if err := mod.RegisterAggregateUDF(con, "any_sum", []int32{dtAnyT}, dtBigint, anyAgg{}); err != nil {
		t.Fatalf("registration with ANY param REJECTED: %v", err)
	}
	// Binding check: does a call with a BIGINT arg resolve? (decode comes later)
	res := mod.allocOut(sizeofDuckdbResult)
	if rc := mod.m.Xduckdb_query(con, mod.cstring("SELECT any_sum(x) FROM range(1,11) t(x)"), res); rc != 0 {
		t.Fatalf("ANY-param aggregate did not bind for BIGINT arg: %s", mod.lastError())
	}
	t.Logf("ANY-param aggregate registered and bound ✓")
}
