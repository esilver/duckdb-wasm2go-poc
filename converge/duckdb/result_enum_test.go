package duckdb

import (
	"context"
	"fmt"
	"testing"
)

// TestSqllogicEnumResult: ENUM result columns must decode to their dictionary
// string, not be misread as a 16-byte string_t. Regression for the
// "enum-result-decode-as-varchar" panic group (e.g.
// duckdb-src/test/sql/types/enum/test_enum_cast.test `select * from person`):
// ENUM vectors hold uint8/16/32 dictionary INDEXES, but rows.decode lumped
// dtEnum in with dtVarchar and sliced a garbage pointer/length out of wasm
// memory ("slice bounds out of range").
func TestSqllogicEnumResult(t *testing.T) {
	c, done := bandConn(t)
	defer done()
	ctx := context.Background()

	exec := func(q string) {
		t.Helper()
		if _, err := c.ExecContext(ctx, q); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}

	// Small enum -> uint8 (UTINYINT) dictionary indexes. Mirrors
	// test_enum_cast.test: CREATE TYPE mood; table person; select *.
	exec("CREATE TYPE mood AS ENUM ('sad', 'ok', 'happy')")
	exec("CREATE TABLE person (name TEXT, current_mood mood)")
	exec("INSERT INTO person VALUES ('Pedro', 'happy'), ('Mark', NULL), ('Pagliacci', 'sad'), ('Mr. Mackey', 'ok')")

	rows, err := c.QueryContext(ctx, "SELECT * FROM person ORDER BY name")
	if err != nil {
		t.Fatalf("select * from person: %v", err)
	}
	var got []string
	for rows.Next() {
		var name string
		var m any
		if err := rows.Scan(&name, &m); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		got = append(got, fmt.Sprintf("%s=%v", name, m))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	rows.Close()
	want := "[Mark=<nil> Mr. Mackey=ok Pagliacci=sad Pedro=happy]"
	if fmt.Sprintf("%v", got) != want {
		t.Fatalf("enum column decode: got %v, want %v", got, want)
	}

	// Enum comparisons / joins on enum columns (test_enum_to_numbers.test shape).
	var a, b any
	if err := c.QueryRowContext(ctx,
		"SELECT t1.current_mood, t2.current_mood FROM person t1, person t2 "+
			"WHERE t1.current_mood = t2.current_mood AND t1.current_mood = 'happy'").Scan(&a, &b); err != nil {
		t.Fatalf("enum join: %v", err)
	}
	if a != "happy" || b != "happy" {
		t.Fatalf("enum join decode: got %v / %v, want happy / happy", a, b)
	}

	// Larger enum (> 255 entries) -> uint16 (USMALLINT) dictionary indexes.
	exec("CREATE TYPE big_enum AS ENUM (SELECT 'v' || i::VARCHAR FROM range(300) t(i))")
	exec("CREATE TABLE bigt (e big_enum)")
	exec("INSERT INTO bigt VALUES ('v0'), ('v299'), ('v123')")
	var e string
	if err := c.QueryRowContext(ctx,
		"SELECT e FROM bigt ORDER BY e DESC LIMIT 1").Scan(&e); err != nil {
		t.Fatalf("usmallint-backed enum: %v", err)
	}
	if e != "v299" {
		t.Fatalf("usmallint-backed enum decode: got %q, want v299", e)
	}

	// LIST-of-ENUM goes through the vecDecoder path (test_enum.test line 115 /
	// test_enum_schema.test line 144 shapes): enum elements must decode via the
	// dictionary too, not to nil.
	var lv any
	if err := c.QueryRowContext(ctx,
		"SELECT [NULL, 'happy', 'sad']::mood[]").Scan(&lv); err != nil {
		t.Fatalf("list-of-enum: %v", err)
	}
	if got := fmt.Sprintf("%v", lv); got != "[<nil> happy sad]" {
		t.Fatalf("list-of-enum decode: got %q, want [<nil> happy sad]", got)
	}

	t.Logf("ENUM result columns decode via the dictionary (uint8 + uint16 indexes, NULLs, LIST-of-ENUM) ✓")
}
