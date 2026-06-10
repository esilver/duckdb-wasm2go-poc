package duckdb

import (
	"context"
	"strings"
	"testing"
)

// These tests pin down ERROR-MESSAGE FIDELITY: the driver must surface the
// same error text native DuckDB produces. Three historical failure modes:
//
//  1. "Unknown exception in ExecutorTask::Execute" — the exhost exception ABI
//     lost the exception's dynamic type across std::exception_ptr capture/
//     rethrow (__cxa_rethrow_primary_exception pushed typ=0) and across
//     throw-from-catch (a single in-flight stack popped the WRONG record in
//     __cxa_end_catch), so DuckDB's catch (std::exception &) stopped matching
//     and catch (...) downgraded the typed error.
//  2. Empty messages ("Invalid Input Error: " with nothing after) — same root
//     cause, surfacing through the convert-and-rethrow error path.
//  3. Missing query + caret context — native duckdb_result_error /
//     duckdb_prepare_error text ends with "LINE n: <query>\n<spaces>^"
//     (ClientContext::ProcessError -> ErrorData::AddErrorLocation); the driver
//     used to bypass result_error for the host's raw what() fallback.
func execErr(t *testing.T, sql string) string {
	t.Helper()
	_, c := openSingleConn(t, ":memory:")
	for _, setup := range []string{
		"CREATE TABLE strs(s VARCHAR)",
		"INSERT INTO strs VALUES ('12'), ('abc')",
	} {
		if _, err := c.ExecContext(context.Background(), setup); err != nil {
			t.Fatalf("setup %q: %v", setup, err)
		}
	}
	_, err := c.ExecContext(context.Background(), sql)
	if err == nil {
		t.Fatalf("expected error from %q, got success", sql)
	}
	return err.Error()
}

// TestExecutorTaskErrorPreserved: a runtime (execution-stage, inside an
// ExecutorTask) conversion error must surface DuckDB's typed message, not the
// catch(...) downgrade.
func TestExecutorTaskErrorPreserved(t *testing.T) {
	msg := execErr(t, "SELECT CAST(s AS INT) FROM strs")
	if strings.Contains(msg, "Unknown exception") {
		t.Fatalf("typed exception degraded to catch(...): %q", msg)
	}
	if !strings.Contains(msg, "Conversion Error") {
		t.Fatalf("missing exception type prefix: %q", msg)
	}
	if !strings.Contains(msg, "Could not convert string 'abc' to INT32") {
		t.Fatalf("missing engine message text: %q", msg)
	}
}

// TestPrepareErrorHasCaretContext: binder errors at prepare time carry the
// original query and a caret position line, exactly like native DuckDB.
func TestPrepareErrorHasCaretContext(t *testing.T) {
	msg := execErr(t, "SELECT nonexistent_col FROM strs")
	if !strings.Contains(msg, "Binder Error") {
		t.Fatalf("missing Binder Error prefix: %q", msg)
	}
	if !strings.Contains(msg, `"nonexistent_col" not found`) {
		t.Fatalf("missing binder message: %q", msg)
	}
	if !strings.Contains(msg, "LINE 1:") || !strings.Contains(msg, "^") {
		t.Fatalf("missing query+caret context: %q", msg)
	}
}

// TestCatalogErrorHasCaretContext: same for catalog errors, the most common
// "statement error" shape in the corpus.
func TestCatalogErrorHasCaretContext(t *testing.T) {
	msg := execErr(t, "SELECT * FROM no_such_table")
	if !strings.Contains(msg, "Catalog Error") {
		t.Fatalf("missing Catalog Error prefix: %q", msg)
	}
	if !strings.Contains(msg, "no_such_table") {
		t.Fatalf("missing table name: %q", msg)
	}
	if !strings.Contains(msg, "LINE 1: SELECT * FROM no_such_table") {
		t.Fatalf("missing query context line: %q", msg)
	}
}

// TestErrorMessageNeverEmpty: the "<Type> Error: " prefix must always be
// followed by text (the empty-message bucket: CSV reader and other
// convert-and-rethrow paths).
func TestErrorMessageNeverEmpty(t *testing.T) {
	for _, sql := range []string{
		"SELECT CAST(s AS INT) FROM strs",            // runtime conversion
		"SELECT * FROM read_csv('/no/such/file.csv')", // CSV reader IO error
		"SELECT nonexistent_col FROM strs",           // binder
	} {
		msg := execErr(t, sql)
		// Find "<word> Error: " and require a non-empty remainder.
		i := strings.Index(msg, " Error: ")
		if i < 0 {
			t.Errorf("%q: no '<Type> Error: ' prefix in %q", sql, msg)
			continue
		}
		rest := strings.TrimSpace(msg[i+len(" Error: "):])
		if rest == "" {
			t.Errorf("%q: empty message after error prefix: %q", sql, msg)
		}
	}
}

// TestErrorFidelityEngineUsableAfterErrors: the exception machinery must leave
// the engine usable after typed, rethrown, and runtime errors back-to-back.
func TestErrorFidelityEngineUsableAfterErrors(t *testing.T) {
	_, c := openSingleConn(t, ":memory:")
	ctx := context.Background()
	if _, err := c.ExecContext(ctx, "CREATE TABLE t(i INT)"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := c.ExecContext(ctx, "INSERT INTO t SELECT CAST('x' AS INT)"); err == nil {
			t.Fatal("expected conversion error")
		}
		if _, err := c.ExecContext(ctx, "SELECT * FROM missing_tbl"); err == nil {
			t.Fatal("expected catalog error")
		}
	}
	var n int
	if err := c.QueryRowContext(ctx, "SELECT 41+1").Scan(&n); err != nil || n != 42 {
		t.Fatalf("engine unusable after error storm: n=%d err=%v", n, err)
	}
}
