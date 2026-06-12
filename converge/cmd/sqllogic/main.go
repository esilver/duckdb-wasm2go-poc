// Command sqllogic runs DuckDB's sqllogictest corpus (duckdb-src/test/sql/**)
// against the pure-Go (wasm2go, CGO_ENABLED=0) DuckDB engine via the
// duckdbconverge/duckdb database/sql driver.
//
// It is a MEASUREMENT tool: each .test file gets a fresh in-memory database;
// the file either PASSes, FAILs (first failing record reported), or is
// SKIPped (unsupported directive / missing required extension / parse issue).
//
// Usage:
//
//	sqllogic -dir ../duckdb-src/test/sql [-glob 'pattern'] [-maxfiles N]
//	         [-timeout 30s] [-j 4] [-v]
//
// The dialect implemented follows duckdb-src/test/sqlite/sqllogic_parser.cpp,
// sqllogic_test_runner.cpp and result_helper.cpp:
//   - statement ok/error/maybe [conn]  (error/maybe may carry ---- + expected
//     error substring or <REGEX>:/<!REGEX>: pattern)
//   - query <TIR...> [nosort|rowsort|sort|valuesort|conn] [label] + ---- +
//     expected values (one value per line, or rows with tab-separated columns)
//     or "N values hashing to <md5>" (md5 of each converted value + "\n")
//   - NULL renders as "NULL", empty string as "(empty)", booleans true/false
//     (compared leniently against 1/0), floats compared approximately with
//     DuckDB's epsilon (|l-r| <= |r|*0.01 + 1e-8)
//   - loop/foreach/endloop with {var} and ${var} substitution and the
//     <integral>/<numeric>/<alltypes>/<signed>/<unsigned> token collections
//   - skipif/onlyif conditions, mode skip/unskip, halt, sleep, set/reset
//     (ignored), hash-threshold, require / require-env
//   - load/restart/reconnect/unzip/concurrentloop => file SKIP
package main

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	duckdb "github.com/esilver/duckdb-go-pure"
)

// ---------------------------------------------------------------- records

type recKind int

const (
	recStatement recKind = iota
	recQuery
	recLoop    // integer loop
	recForeach // token loop
	recMode    // mode skip / unskip
	recHalt
	recSleep
	recResetLabel // reset label <name>: forget the stored hash for a label
)

type condition struct {
	skipIf bool // true: skipif, false: onlyif
	expr   string
}

type record struct {
	kind recKind
	line int // 1-based line of the directive

	conds []condition

	// statement / query
	sql      string
	conn     string
	expected []string // query expected lines, or statement expected-error lines

	// statement
	expectKind string // "ok", "error", "maybe"

	// query
	typeChars string
	sortStyle string // "", "rowsort", "valuesort"
	label     string

	// loop / foreach
	loopVar    string
	loopStart  int
	loopEnd    int
	loopTokens []string
	body       []record

	// mode
	modeArg string

	// sleep
	sleepDur time.Duration
}

// ---------------------------------------------------------------- outcomes

type fileResult struct {
	path    string
	outcome string // "PASS", "FAIL", "SKIP"
	reason  string // skip reason or failure bucket key
	detail  string // failure detail (line, sql snippet, mismatch)
	line    int

	recRun        int
	recPassed     int
	recSkipped    int
	autoRollbacks int

	dur time.Duration
}

// failKind constants double as bucket keys (engine errors get their own keys).
const (
	failTimeout      = "timeout"
	failPanic        = "panic"
	failWrongResult  = "wrong result"
	failWrongRows    = "wrong row count"
	failWrongCols    = "wrong column count"
	failErrMismatch  = "statement error: message mismatch"
	failNoError      = "statement error: expected error, got success"
	failHashMismatch = "hash mismatch"
	failInternal     = "INTERNAL/fatal error"
	failScan         = "row scan error"
)

// ---------------------------------------------------------------- main

var (
	flagDir      = flag.String("dir", "duckdb-src/test/sql", "root directory to scan for .test files")
	flagGlob     = flag.String("glob", "", "only run files whose path matches this substring or glob")
	flagMaxFiles = flag.Int("maxfiles", 0, "max number of files to run (0 = all)")
	flagTimeout  = flag.Duration("timeout", 30*time.Second, "per-file timeout")
	flagJobs     = flag.Int("j", 1, "number of files to run in parallel")
	flagVerbose  = flag.Bool("v", false, "print per-file results as they complete")
)

var flagProbe = flag.String("probe", "", "run a single SQL statement against a fresh :memory: db, print the raw result/error, and exit")

func main() {
	flag.Parse()

	if *flagProbe != "" {
		db, err := sql.Open("duckdb", ":memory:")
		if err != nil {
			fmt.Printf("open: %v\n", err)
			os.Exit(1)
		}
		defer db.Close()
		rows, err := db.Query(*flagProbe)
		if err != nil {
			fmt.Printf("raw error: %q\n", err.Error())
			fmt.Printf("decoded:   %q\n", err.Error())
			return
		}
		cols, _ := rows.Columns()
		cts, _ := rows.ColumnTypes()
		types := parseColTypes(cts, len(cols))
		for i, ct := range cts {
			fmt.Printf("col %d type: %s\n", i+1, ct.DatabaseTypeName())
		}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				fmt.Printf("scan error: %v\n", err)
				break
			}
			for i, v := range vals {
				fmt.Printf("col %d: %T %q -> %s\n", i+1, v, fmt.Sprint(v), valueToString(v, types[i], renderCtx{}))
			}
		}
		rows.Close()
		return
	}

	files, err := collectFiles(*flagDir, *flagGlob)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sqllogic: %v\n", err)
		os.Exit(2)
	}
	total := len(files)
	if *flagMaxFiles > 0 && len(files) > *flagMaxFiles {
		files = files[:*flagMaxFiles]
	}

	start := time.Now()
	results := make([]fileResult, len(files))
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(1, *flagJobs))
	var printMu sync.Mutex
	for i, f := range files {
		wg.Add(1)
		go func(i int, f string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res := runFileWithTimeout(f, *flagTimeout)
			results[i] = res
			if *flagVerbose {
				printMu.Lock()
				tag := res.outcome
				if res.reason != "" {
					tag += "(" + res.reason + ")"
				}
				fmt.Printf("%-60s %s  [%d run, %d ok, %d skip] %.2fs\n",
					relPath(f), tag, res.recRun, res.recPassed, res.recSkipped, res.dur.Seconds())
				if res.outcome == "FAIL" && res.detail != "" {
					fmt.Printf("    %s\n", strings.ReplaceAll(res.detail, "\n", "\n    "))
				}
				printMu.Unlock()
			}
		}(i, f)
	}
	wg.Wait()

	report(results, total, time.Since(start))
}

func collectFiles(root, glob string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".test") { // .test_slow intentionally excluded
			return nil
		}
		if glob != "" {
			ok, _ := filepath.Match(glob, filepath.Base(path))
			if !ok && !strings.Contains(path, glob) {
				return nil
			}
		}
		files = append(files, path)
		return nil
	})
	sort.Strings(files)
	return files, err
}

func relPath(p string) string {
	if i := strings.Index(p, "test/sql/"); i >= 0 {
		return p[i:]
	}
	return p
}

// ---------------------------------------------------------------- per-file execution with timeout + recover

func runFileWithTimeout(path string, timeout time.Duration) fileResult {
	done := make(chan fileResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				msg := fmt.Sprintf("%v", r)
				stack := string(debug.Stack())
				if len(stack) > 600 {
					stack = stack[:600]
				}
				done <- fileResult{path: path, outcome: "FAIL", reason: failPanic,
					detail: "panic: " + msg + "\n" + stack}
			}
		}()
		done <- executeFile(path)
	}()
	startT := time.Now()
	select {
	case res := <-done:
		res.path = path
		res.dur = time.Since(startT)
		return res
	case <-time.After(timeout):
		// abandon the goroutine and its engine instance; do NOT reuse / close it
		return fileResult{path: path, outcome: "FAIL", reason: failTimeout,
			detail: fmt.Sprintf("file exceeded %s", timeout), dur: time.Since(startT)}
	}
}

// ---------------------------------------------------------------- parsing

type parseSkip struct{ reason string }

func (p parseSkip) Error() string { return p.reason }

type parser struct {
	lines []string
	pos   int // 0-based
}

func emptyOrComment(l string) bool {
	return l == "" || strings.HasPrefix(l, "#")
}

// parseFile returns the record tree, or a parseSkip error for files using
// unsupported machinery.
func parseFile(path string) ([]record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r", ""), "\n")
	p := &parser{lines: lines}

	var root []record
	stack := []*[]record{&root} // innermost loop body last
	loopStack := []*record{}    // parallel to stack[1:]
	var pendingConds []condition

	appendRec := func(r record) {
		r.conds = pendingConds
		pendingConds = nil
		cur := stack[len(stack)-1]
		*cur = append(*cur, r)
	}

	for p.pos < len(p.lines) {
		line := p.lines[p.pos]
		if emptyOrComment(line) {
			p.pos++
			continue
		}
		fields := strings.Fields(line)
		tok := fields[0]
		args := fields[1:]
		lineNo := p.pos + 1

		switch tok {
		case "skipif", "onlyif":
			pendingConds = append(pendingConds, condition{skipIf: tok == "skipif", expr: strings.Join(args, "")})
			p.pos++

		case "statement":
			if len(args) < 1 {
				return nil, parseSkip{"parse: statement without argument"}
			}
			kind := args[0]
			switch kind {
			case "ok", "error", "maybe":
			default:
				return nil, parseSkip{"parse: statement " + kind}
			}
			r := record{kind: recStatement, line: lineNo, expectKind: kind}
			if len(args) >= 2 {
				r.conn = args[1]
			}
			p.pos++
			r.sql = p.extractStatement()
			r.expected = p.extractResultLines()
			appendRec(r)

		case "query":
			if len(args) < 1 {
				return nil, parseSkip{"parse: query without type string"}
			}
			for _, c := range args[0] {
				if c != 'T' && c != 'I' && c != 'R' {
					return nil, parseSkip{"parse: bad query type char"}
				}
			}
			r := record{kind: recQuery, line: lineNo, typeChars: args[0]}
			if len(args) >= 2 {
				switch args[1] {
				case "nosort", "none":
				case "rowsort", "sort":
					r.sortStyle = "rowsort"
				case "valuesort":
					r.sortStyle = "valuesort"
				default:
					r.conn = args[1]
				}
			}
			if len(args) >= 3 {
				r.label = args[2]
			}
			p.pos++
			r.sql = p.extractStatement()
			r.expected = p.extractResultLines()
			appendRec(r)

		case "require":
			if len(args) < 1 {
				return nil, parseSkip{"parse: bare require"}
			}
			if reason := checkRequire(args); reason != "" {
				return nil, parseSkip{reason}
			}
			p.pos++

		case "require-env":
			return nil, parseSkip{"require-env " + strings.Join(args, " ")}

		case "load", "restart", "reconnect", "unzip":
			return nil, parseSkip{"directive: " + tok}

		case "concurrentloop", "concurrentforeach":
			return nil, parseSkip{"directive: " + tok}

		case "loop":
			if len(args) != 3 {
				return nil, parseSkip{"parse: loop wants 3 args"}
			}
			start, err1 := strconv.Atoi(args[1])
			end, err2 := strconv.Atoi(args[2])
			if err1 != nil || err2 != nil {
				return nil, parseSkip{"parse: non-integer loop bounds"}
			}
			r := record{kind: recLoop, line: lineNo, loopVar: args[0], loopStart: start, loopEnd: end}
			appendRec(r)
			cur := stack[len(stack)-1]
			loopStack = append(loopStack, &(*cur)[len(*cur)-1])
			stack = append(stack, &(*cur)[len(*cur)-1].body)
			p.pos++

		case "foreach":
			if len(args) < 1 {
				return nil, parseSkip{"parse: foreach without var"}
			}
			tokens, ok := expandForeachTokens(args[1:])
			if !ok {
				return nil, parseSkip{"parse: foreach token collection"}
			}
			r := record{kind: recForeach, line: lineNo, loopVar: args[0], loopTokens: tokens}
			appendRec(r)
			cur := stack[len(stack)-1]
			loopStack = append(loopStack, &(*cur)[len(*cur)-1])
			stack = append(stack, &(*cur)[len(*cur)-1].body)
			p.pos++

		case "endloop":
			if len(stack) <= 1 {
				return nil, parseSkip{"parse: endloop without loop"}
			}
			stack = stack[:len(stack)-1]
			loopStack = loopStack[:len(loopStack)-1]
			p.pos++

		case "mode":
			if len(args) >= 1 && (args[0] == "skip" || args[0] == "unskip") {
				appendRec(record{kind: recMode, line: lineNo, modeArg: args[0]})
			}
			// other modes (output_hash, output_result, debug) ignored
			p.pos++

		case "halt":
			appendRec(record{kind: recHalt, line: lineNo})
			p.pos++

		case "sleep":
			d := parseSleep(args)
			appendRec(record{kind: recSleep, line: lineNo, sleepDur: d})
			p.pos++

		case "set", "reset", "tags", "test_env", "hash-threshold", "continue":
			// "set seed <v>" seeds the RNG so random()-dependent expected values
			// reproduce (duckdb's runner translates it to SELECT SETSEED(<v>)).
			// "reset label <name>" forgets a stored label hash (upstream's
			// ResetLabel command). Everything else is ignored (safe / not
			// applicable to a fresh in-memory engine per file).
			if tok == "reset" && len(args) == 2 && strings.EqualFold(args[0], "label") {
				appendRec(record{kind: recResetLabel, line: lineNo, label: args[1]})
				p.pos++
				continue
			}
			if tok == "set" && len(args) == 2 && args[0] == "seed" {
				if _, err := strconv.ParseFloat(args[1], 64); err == nil {
					appendRec(record{
						kind:       recStatement,
						line:       lineNo,
						sql:        "SELECT SETSEED(" + args[1] + ")",
						expectKind: "ok",
					})
				}
			}
			p.pos++

		default:
			return nil, parseSkip{"parse: unknown directive '" + tok + "'"}
		}
	}
	if len(stack) != 1 {
		return nil, parseSkip{"parse: unterminated loop"}
	}
	return root, nil
}

func (p *parser) extractStatement() string {
	var sb strings.Builder
	first := true
	for p.pos < len(p.lines) && !emptyOrComment(p.lines[p.pos]) {
		if p.lines[p.pos] == "----" {
			break
		}
		if !first {
			sb.WriteString("\n")
		}
		sb.WriteString(p.lines[p.pos])
		first = false
		p.pos++
	}
	return sb.String()
}

func (p *parser) extractResultLines() []string {
	if p.pos >= len(p.lines) || p.lines[p.pos] != "----" {
		return nil
	}
	p.pos++
	out := []string{} // non-nil: "---- present but empty" means expect 0 rows
	for p.pos < len(p.lines) && p.lines[p.pos] != "" {
		out = append(out, p.lines[p.pos])
		p.pos++
	}
	return out
}

func parseSleep(args []string) time.Duration {
	if len(args) < 1 {
		return 0
	}
	n, err := strconv.Atoi(args[0])
	if err != nil {
		return 0
	}
	unit := "second"
	if len(args) >= 2 {
		unit = strings.TrimSuffix(strings.ToLower(args[1]), "s")
	}
	var d time.Duration
	switch unit {
	case "second", "sec":
		d = time.Duration(n) * time.Second
	case "millisecond", "milli":
		d = time.Duration(n) * time.Millisecond
	case "microsecond", "micro":
		d = time.Duration(n) * time.Microsecond
	case "nanosecond", "nano":
		d = time.Duration(n) * time.Nanosecond
	default:
		d = time.Duration(n) * time.Second
	}
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	return d
}

// builtinExtensions: per the build, json and icu are compiled in. Everything
// else (parquet, httpfs, tpch, ...) is unavailable in the pure-Go engine.
var builtinExtensions = map[string]bool{"json": true, "icu": true}

// requirements that are satisfied in this environment (mirrors CheckRequire in
// sqllogic_test_runner.cpp for a darwin host, default test configuration).
var requirePresent = map[string]bool{
	"notmusl": true, "notmingw": true, "notwindows": true, "longdouble": true,
	"nothreadsan": true, "strinline": true,
	"skip_reload": true, "noforcestorage": true, "no_force_storage": true,
	"no_alternative_verify": true, "no_vector_verification": true,
	"no_extension_autoloading": true, "no_latest_storage": true,
	"allow_unsigned_extensions": true,
}

// checkRequire returns "" if the requirement is met, else a skip reason.
func checkRequire(args []string) string {
	param := strings.ToLower(args[0])
	if requirePresent[param] {
		return ""
	}
	if builtinExtensions[param] {
		return ""
	}
	switch param {
	case "vector_size":
		if len(args) >= 2 {
			if n, err := strconv.Atoi(args[1]); err == nil && n <= 2048 {
				return ""
			}
		}
		return "require vector_size " + strings.Join(args[1:], " ")
	case "exact_vector_size":
		if len(args) >= 2 && args[1] == "2048" {
			return ""
		}
		return "require exact_vector_size"
	case "block_size":
		if len(args) >= 2 && args[1] == "262144" {
			return ""
		}
		return "require block_size " + strings.Join(args[1:], " ")
	case "ram":
		// Upstream compares the requirement against the memory available to
		// the ENGINE (FileSystem::GetAvailableMemory). Our engine is a wasm32
		// module: its linear memory can never exceed the 4 GiB address space
		// (and the runner caps it at 512MB), so requirements above that are
		// MISSING and the file skips — exactly what upstream does on a small
		// machine.
		if len(args) >= 2 {
			if gb := parseRamGB(args[1]); gb > 4 {
				return "require ram " + strings.Join(args[1:], " ") + " (wasm32 4GiB ceiling)"
			}
		}
		return ""
	case "disk_space":
		return "" // host has plenty
	}
	return "require " + param
}

// parseRamGB parses a "require ram" size argument ("16gb", "8GB", "500mb")
// into (possibly fractional, rounded-down) whole GiB; 0 when unparseable.
func parseRamGB(s string) float64 {
	t := strings.ToLower(strings.TrimSpace(s))
	mult := 0.0
	switch {
	case strings.HasSuffix(t, "gb"):
		mult, t = 1, t[:len(t)-2]
	case strings.HasSuffix(t, "mb"):
		mult, t = 1.0/1024, t[:len(t)-2]
	case strings.HasSuffix(t, "tb"):
		mult, t = 1024, t[:len(t)-2]
	default:
		return 0
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
	if err != nil {
		return 0
	}
	return v * mult
}

// expandForeachTokens expands <integral> etc. (sqllogic_test_runner.cpp
// ForEachTokenReplace).
func expandForeachTokens(params []string) ([]string, bool) {
	var out []string
	for _, p := range params {
		t := strings.ToLower(strings.TrimSpace(p))
		signed := []string{"tinyint", "smallint", "integer", "bigint", "hugeint"}
		unsigned := []string{"utinyint", "usmallint", "uinteger", "ubigint", "uhugeint"}
		switch t {
		case "<signed>":
			out = append(out, signed...)
		case "<unsigned>":
			out = append(out, unsigned...)
		case "<integral>":
			out = append(out, signed...)
			out = append(out, unsigned...)
		case "<numeric>":
			out = append(out, signed...)
			out = append(out, unsigned...)
			out = append(out, "float", "double")
		case "<alltypes>":
			out = append(out, signed...)
			out = append(out, unsigned...)
			out = append(out, "float", "double", "bool", "interval", "varchar")
		case "<compression>", "<all_types_columns>":
			return nil, false // depends on build flags / giant column list
		default:
			if strings.HasPrefix(t, "!") {
				// remove token from list
				needle := p[1:]
				kept := out[:0]
				for _, v := range out {
					if v != needle {
						kept = append(kept, v)
					}
				}
				out = kept
				continue
			}
			out = append(out, p)
		}
	}
	return out, true
}

// ---------------------------------------------------------------- execution

type failStop struct {
	bucket string
	detail string
	line   int
}

func (f failStop) Error() string { return f.bucket }

type haltStop struct{}

func (haltStop) Error() string { return "halt" }

type execState struct {
	db          *sql.DB
	conns       map[string]*sql.Conn
	inTxn       map[string]bool // conn name -> inside explicit BEGIN..COMMIT/ROLLBACK
	labelHashes map[string]string
	skipMode    bool
	testDir     string // per-file scratch dir (__TEST_DIR__)
	testName    string
	testUUID    string
	tz          *time.Location // session TimeZone (tracked from successful SET TimeZone)
	calendar    string         // non-default SET Calendar name ("" = gregorian default)

	recRun        int
	recPassed     int
	recSkipped    int
	autoRollbacks int
}

type sub struct{ name, val string }

// renderCtx carries the session state value rendering depends on: the
// TimeZone for TIMESTAMPTZ and which non-default ICU Calendar is active
// (cal == "" means the default proleptic-Gregorian cast).
type renderCtx struct {
	tz  *time.Location
	cal string
}

func (st *execState) rctx() renderCtx { return renderCtx{st.tz, st.calendar} }

func executeFile(path string) fileResult {
	recs, err := parseFile(path)
	if err != nil {
		if ps, ok := err.(parseSkip); ok {
			return fileResult{outcome: "SKIP", reason: ps.reason}
		}
		return fileResult{outcome: "SKIP", reason: "read error: " + err.Error()}
	}

	db, err := sql.Open("duckdb", ":memory:")
	if err != nil {
		return fileResult{outcome: "FAIL", reason: "open", detail: err.Error()}
	}
	// Raise the engine's memory limit: the wasm build's auto-detected default
	// is ~17.5 MiB, far below what the C++ test harness runs with. (Documented
	// in the report; failures to set it are ignored.)
	_, _ = db.Exec("SET memory_limit='512MB'")

	scratch, serr := os.MkdirTemp("", "sqllogic-*")
	if serr != nil {
		scratch = os.TempDir()
	} else {
		defer os.RemoveAll(scratch)
	}
	// Native-runner parity (sqllogic_test_runner.cpp Reconnect): redirect
	// persistent secrets into the per-file scratch dir so CREATE PERSISTENT
	// SECRET never touches the real ~/.duckdb/stored_secrets.
	_, _ = db.Exec("SET secret_directory='" + scratch + "/test_secret_dir'")
	st := &execState{
		db:          db,
		conns:       map[string]*sql.Conn{},
		inTxn:       map[string]bool{},
		labelHashes: map[string]string{},
		testDir:     scratch,
		testName:    relPath(path),
		testUUID: fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
			time.Now().UnixNano()&0xffffffff, os.Getpid()&0xffff, 0x4000, 0x8000, time.Now().UnixNano()),
	}
	defer func() {
		for _, c := range st.conns {
			c.Close()
		}
		db.Close()
	}()

	err = st.execRecords(recs, nil)
	res := fileResult{recRun: st.recRun, recPassed: st.recPassed, recSkipped: st.recSkipped,
		autoRollbacks: st.autoRollbacks}
	switch e := err.(type) {
	case nil:
		res.outcome = "PASS"
	case haltStop:
		res.outcome = "PASS"
	case failStop:
		res.outcome = "FAIL"
		res.reason = e.bucket
		res.detail = fmt.Sprintf("line %d: %s", e.line, e.detail)
		res.line = e.line
	default:
		res.outcome = "FAIL"
		res.reason = "runner error"
		res.detail = err.Error()
	}
	return res
}

func (st *execState) execRecords(recs []record, subs []sub) error {
	for i := range recs {
		r := &recs[i]
		switch r.kind {
		case recMode:
			st.skipMode = r.modeArg == "skip"
		case recHalt:
			return haltStop{}
		case recSleep:
			time.Sleep(r.sleepDur)
		case recResetLabel:
			// Upstream's ResetLabel command (sqllogic_test_runner.cpp): forget
			// the label's stored hash so foreach iterations with different
			// results can reuse the same label.
			delete(st.labelHashes, r.label)
		case recLoop:
			for v := r.loopStart; v < r.loopEnd; v++ {
				err := st.execRecords(r.body, append(subs, sub{r.loopVar, strconv.Itoa(v)}))
				if err != nil {
					return err
				}
			}
		case recForeach:
			// Multi-variable form (sqllogic_test_runner.cpp ReplaceLoopIterator):
			// `foreach type,min,max tinyint,-128,127 ...` — comma-separated names,
			// each token comma-split into the same arity (StringUtil::Split drops
			// empty fields, which the corpus exploits to embed trailing commas).
			names := strings.Split(r.loopVar, ",")
			for _, tokv := range r.loopTokens {
				ns := subs
				if len(names) > 1 {
					parts := dropEmpty(strings.Split(tokv, ","))
					if len(parts) != len(names) {
						return failStop{"runner error",
							fmt.Sprintf("foreach loop: iterator %q arity does not match token %q", r.loopVar, tokv), r.line}
					}
					for i := range names {
						ns = append(ns, sub{names[i], parts[i]})
					}
				} else {
					ns = append(ns, sub{r.loopVar, tokv})
				}
				err := st.execRecords(r.body, ns)
				if err != nil {
					return err
				}
			}
		case recStatement, recQuery:
			if st.skipMode {
				st.recSkipped++
				continue
			}
			if !st.condsPass(r.conds, subs) {
				st.recSkipped++
				continue
			}
			st.recRun++
			var err error
			if r.kind == recStatement {
				err = st.execStatement(r, subs)
			} else {
				err = st.execQuery(r, subs)
			}
			if err != nil {
				return err
			}
			st.recPassed++
		}
	}
	return nil
}

func (st *execState) substitute(s string, subs []sub) string {
	for _, sb := range subs {
		s = strings.ReplaceAll(s, "${"+sb.name+"}", sb.val)
		s = strings.ReplaceAll(s, "{"+sb.name+"}", sb.val)
	}
	// {WORKING_DIR}/__TEST_DIR__ composes a single path in the upstream harness
	// because its __TEST_DIR__ is RELATIVE to the working dir (TestDirectoryPath
	// returns "duckdb_unittest_tempdir/<pid>"). Ours is absolute, so the literal
	// concatenation would be bogus — substitute the combined token with the test
	// dir itself, which is the exact path the native expansion denotes.
	// (Used by test/sql/attach/attach_fsspec.test: 'file://{WORKING_DIR}/__TEST_DIR__/...'.)
	s = strings.ReplaceAll(s, "${WORKING_DIR}/__TEST_DIR__", st.testDir)
	s = strings.ReplaceAll(s, "{WORKING_DIR}/__TEST_DIR__", st.testDir)
	// environment keywords per duckdb-src/test/README.md "Test Contract"
	for _, kv := range [][2]string{
		{"DATA_DIR", dataDir},
		{"TEMP_DIR", st.testDir},
		{"TEMP_BASE", st.testDir},
		{"TEST_DIR", st.testDir},
		{"WORKING_DIR", workingDir},
		{"TEST_NAME", st.testName},
		{"TEST_NAME_NO_SLASH", strings.ReplaceAll(st.testName, "/", "_")},
		{"TEST_UUID", st.testUUID},
		{"BUILD_DIR", workingDir + "/build/release"},
	} {
		s = strings.ReplaceAll(s, "${"+kv[0]+"}", kv[1])
		s = strings.ReplaceAll(s, "{"+kv[0]+"}", kv[1])
	}
	s = strings.ReplaceAll(s, "__TEST_DIR__", st.testDir)
	s = strings.ReplaceAll(s, "__WORKING_DIRECTORY__", workingDir)
	s = strings.ReplaceAll(s, "__BUILD_DIRECTORY__", workingDir+"/build/release")
	return s
}

var workingDir = func() string {
	wd, _ := os.Getwd()
	return wd
}()

var dataDir = func() string {
	if d := os.Getenv("DATA_DIR"); d != "" {
		return d
	}
	return workingDir + "/data"
}()

// condsPass evaluates skipif/onlyif conditions after loop substitution.
func (st *execState) condsPass(conds []condition, subs []sub) bool {
	for _, c := range conds {
		v := evalCond(substituteBareLoopVars(st.substitute(c.expr, subs), subs))
		if c.skipIf && v {
			return false
		}
		if !c.skipIf && !v {
			return false
		}
	}
	return true
}

// substituteBareLoopVars replaces condition operands that exactly match a
// loop-variable name with its value, mirroring duckdb's CheckLoopCondition:
// `onlyif compression=dict_fsst` inside `foreach compression ...` compares the
// VARIABLE's value, not the literal word "compression". Only whole operands
// are replaced (split on && and the comparison operator), never substrings.
func substituteBareLoopVars(expr string, subs []sub) string {
	if len(subs) == 0 {
		return expr
	}
	terms := strings.Split(expr, "&&")
	for ti, term := range terms {
		op, i := "", -1
		for _, cand := range condOps {
			if j := strings.Index(term, cand); j >= 0 {
				op, i = cand, j
				break
			}
		}
		if i < 0 {
			continue
		}
		l, r := term[:i], term[i+len(op):]
		for _, sb := range subs {
			if strings.TrimSpace(l) == sb.name {
				l = sb.val
			}
			if strings.TrimSpace(r) == sb.name {
				r = sb.val
			}
		}
		terms[ti] = l + op + r
	}
	return strings.Join(terms, "&&")
}

var condOps = []string{"<>", ">=", "<=", "=", ">", "<"}

func evalCond(expr string) bool {
	for _, part := range strings.Split(expr, "&&") {
		if !evalCondTerm(strings.TrimSpace(part)) {
			return false
		}
	}
	return true
}

func evalCondTerm(term string) bool {
	for _, op := range condOps {
		i := strings.Index(term, op)
		if i < 0 {
			continue
		}
		l, r := term[:i], term[i+len(op):]
		li, lerr := strconv.ParseFloat(l, 64)
		ri, rerr := strconv.ParseFloat(r, 64)
		if lerr == nil && rerr == nil {
			switch op {
			case "=":
				return li == ri
			case "<>":
				return li != ri
			case ">":
				return li > ri
			case "<":
				return li < ri
			case ">=":
				return li >= ri
			case "<=":
				return li <= ri
			}
		}
		switch op {
		case "=":
			return l == r
		case "<>":
			return l != r
		}
		return false
	}
	// bare token: "onlyif duckdb" runs, "skipif duckdb" skips
	return term == "duckdb"
}

func (st *execState) getConn(name string) (*sql.Conn, error) {
	if c, ok := st.conns[name]; ok {
		return c, nil
	}
	c, err := st.db.Conn(noCtx)
	if err != nil {
		return nil, err
	}
	st.conns[name] = c
	return c, nil
}

var noCtx = context.Background()

// ---------------------------------------------------------------- statement

// threadsStmtRe matches statements that (re)configure the number of threads.
// The wasm build is inherently single-threaded ("DuckDB was compiled without
// threads!"); the upstream C++ harness runs a threaded build where these
// succeed as plain settings. They have no semantic effect on any result this
// runner checks, so a `statement ok` thread reconfiguration is treated as a
// no-op SUCCESS. (`statement error` thread statements still execute: value
// validation errors fire before the threads check and must be observed.)
var threadsStmtRe = regexp.MustCompile(`(?is)^\s*(?:pragma\s+(?:threads|worker_threads)\s*(?:=|\()|set\s+(?:session\s+|global\s+|local\s+)?(?:threads|total_threads|worker_threads)\s*(?:=|\s+to\s+))`)

// setTimeZoneRe captures the zone name of a SET TimeZone statement so query
// comparison can accept TIMESTAMPTZ renderings in the session zone (the driver
// returns instants; it exposes no column type info to render with).
var (
	setTimeZoneRe   = regexp.MustCompile(`(?is)^\s*set\s+(?:session\s+|local\s+|global\s+)?time\s*zone\s*(?:=|to)?\s*'([^']*)'`)
	resetTimeZoneRe = regexp.MustCompile(`(?is)^\s*reset\s+time\s*zone`)
	// SET Calendar / PRAGMA CALENDAR: a non-default calendar switches the
	// engine's TIMESTAMPTZ->VARCHAR cast to ICU's hybrid Julian/Gregorian
	// calendars (test_icu_calendar); the default cast is proleptic Gregorian.
	setCalendarRe = regexp.MustCompile(`(?is)^\s*(?:set|pragma)\s+(?:session\s+|local\s+|global\s+)?calendar\s*(?:=|to)?\s*'([^']*)'`)
)

func (st *execState) execStatement(r *record, subs []sub) error {
	sqlText := st.substitute(r.sql, subs)
	conn, err := st.getConn(r.conn)
	if err != nil {
		return failStop{"runner: conn", err.Error(), r.line}
	}
	if r.expectKind == "ok" && threadsStmtRe.MatchString(sqlText) {
		return nil // single-threaded build: thread-count changes are no-op successes
	}
	// The driver can only prepare one statement at a time
	// ("Cannot prepare multiple statements at once!"), while the sqllogictest
	// dialect allows several ;-separated statements per record. Split and run
	// sequentially; the first error wins (matches the C++ batch semantics).
	parts := splitStatements(sqlText)
	if len(parts) == 0 {
		// comment-only / whitespace-only statement: DuckDB's Query() of such
		// text succeeds with an empty result, our driver's prepare errors with
		// "No statement to prepare!". Match the C++ behavior.
		if r.expectKind == "error" {
			parts = []string{sqlText}
		} else {
			return nil
		}
	}
	var rawErr error
	for _, part := range parts {
		if _, rawErr = conn.ExecContext(noCtx, part); rawErr != nil {
			break
		}
		// Track explicit transactions PER PART: a multi-statement record like
		// "BEGIN; <failing stmt>" enters an explicit transaction even though
		// the record as a whole errors. Native DuckDB leaves that transaction
		// in the aborted state ("Current transaction is aborted"); issuing the
		// sacrificial ROLLBACK below would destroy it and make the next record
		// succeed where it must fail (the three
		// multistatement_is_transactional_chained_* tests).
		st.trackTxn(r.conn, part)
	}
	if rawErr == nil {
		if m := setTimeZoneRe.FindStringSubmatch(sqlText); m != nil {
			if loc, lerr := time.LoadLocation(m[1]); lerr == nil {
				st.tz = loc
			} else if loc := fixedUTCZone(m[1]); loc != nil {
				st.tz = loc
			} else {
				st.tz = nil
			}
		} else if resetTimeZoneRe.MatchString(sqlText) {
			st.tz = nil
		}
		if m := setCalendarRe.FindStringSubmatch(sqlText); m != nil {
			st.calendar = strings.ToLower(m[1])
			if strings.EqualFold(m[1], "gregorian") {
				st.calendar = ""
			}
		}
	}
	var errMsg string
	if rawErr != nil {
		errMsg = rawErr.Error()
	}

	// Work around an ENGINE/DRIVER issue: a failed statement leaves the
	// connection's (autocommit) transaction open, so every later statement
	// fails with "cannot start a transaction within a transaction". When the
	// test was NOT inside an explicit BEGIN (tracked per part above), issue a
	// ROLLBACK to clear the leaked transaction. Counted+reported.
	if rawErr != nil && !st.inTxn[r.conn] {
		// The leaked transaction makes the NEXT statement fail at txn-begin
		// ("cannot start a transaction within a transaction"), which clears
		// it. This sacrificial ROLLBACK absorbs that one-shot poison.
		st.autoRollbacks++
		_, _ = conn.ExecContext(noCtx, "ROLLBACK")
	}

	expErr := st.substitute(strings.Join(r.expected, "\n"), subs)

	if rawErr != nil && isInternalError(errMsg) {
		// Native parity: DuckDB's own runner matches `statement error` by plain
		// substring containment with NO FATAL/INTERNAL carve-out
		// (test/sqlite/result_helper.cpp:311-316). The checkpoint fault-injection
		// tests (test_checkpoint_failure_*) EXPECT a FATAL ("Checkpoint aborted
		// before header write..."); native v1.5.3 throws the byte-identical FATAL
		// and passes. Only treat a FATAL/INTERNAL error as an automatic failure
		// when the record did not expect that exact error. (After an expected
		// FATAL the database is invalidated natively too, so any later record
		// touching it fails on both sides — no extra handling needed.)
		expectedFatal := (r.expectKind == "error" || r.expectKind == "maybe") &&
			expErr != "" && errorMatches(errMsg, expErr)
		if !expectedFatal {
			return failStop{failInternal, snip(sqlText) + "\n=> " + errMsg, r.line}
		}
	}

	switch r.expectKind {
	case "ok":
		if rawErr != nil {
			return failStop{errorBucket(errMsg), snip(sqlText) + "\n=> " + errMsg, r.line}
		}
	case "error":
		if rawErr == nil {
			return failStop{failNoError, snip(sqlText), r.line}
		}
		if expErr != "" && !errorMatches(errMsg, expErr) {
			return failStop{failErrMismatch,
				snip(sqlText) + "\nexpected: " + snip(expErr) + "\nactual:   " + snip(errMsg), r.line}
		}
	case "maybe":
		if rawErr != nil && expErr != "" && !errorMatches(errMsg, expErr) {
			return failStop{failErrMismatch,
				snip(sqlText) + "\nexpected: " + snip(expErr) + "\nactual:   " + snip(errMsg), r.line}
		}
	}
	return nil
}

// trackTxn updates the per-connection explicit-transaction state after a
// SUCCESSFULLY executed statement (failed statements never change DuckDB's
// explicit-tx membership; a failure inside BEGIN aborts the tx but stays in
// it until ROLLBACK).
func (st *execState) trackTxn(connName, stmt string) {
	upper := strings.ToUpper(strings.TrimSpace(stmt))
	switch {
	case strings.HasPrefix(upper, "BEGIN") || strings.HasPrefix(upper, "START TRANSACTION"):
		st.inTxn[connName] = true
	case strings.HasPrefix(upper, "COMMIT") || strings.HasPrefix(upper, "ROLLBACK") || strings.HasPrefix(upper, "ABORT"):
		st.inTxn[connName] = false
	}
}

// splitStatements splits a SQL batch on top-level semicolons, respecting
// single/double quotes, $$ dollar-quoting and -- line comments.
//
// Statement TEXT follows DuckDB's parser (src/parser/parser.cpp): every
// statement but the last ends just BEFORE its ';'; the LAST statement's text
// extends verbatim to the end of the batch — including its terminating ';'
// and any trailing comments (current_query() echoes exactly that text, and
// the query log records the per-statement slices). Segments with no
// meaningful content (only whitespace, comments, semicolons) never become
// statements; trailing ones merge into the last statement's text. May return
// an EMPTY slice when the whole text is comment/whitespace-only.
func splitStatements(s string) []string {
	type piece struct {
		start, end int  // [start,end): segment text excluding the ';'
		meaningful bool // contains something besides whitespace/comments
	}
	var pieces []piece
	n := len(s)
	i := 0
	segStart := 0
	meaningful := false
	endPiece := func(end int) {
		pieces = append(pieces, piece{segStart, end, meaningful})
		meaningful = false
	}
	for i < n {
		c := s[i]
		switch c {
		case '\'', '"':
			q := c
			meaningful = true
			i++
			for i < n {
				if s[i] == q {
					if i+1 < n && s[i+1] == q { // escaped quote
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			continue
		case '-':
			if i+1 < n && s[i+1] == '-' { // line comment (kept in text, not "meaningful")
				for i < n && s[i] != '\n' {
					i++
				}
				continue
			}
		case '$':
			if i+1 < n && s[i+1] == '$' { // dollar-quoted block
				meaningful = true
				i += 2
				for i < n {
					if s[i] == '$' && i+1 < n && s[i+1] == '$' {
						i += 2
						break
					}
					i++
				}
				continue
			}
		case ';':
			endPiece(i)
			i++
			segStart = i
			continue
		}
		if !meaningful && c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			meaningful = true
		}
		i++
	}
	endPiece(n)

	last := -1 // last meaningful piece: its text runs to the end of s
	for idx := range pieces {
		if pieces[idx].meaningful {
			last = idx
		}
	}
	if last < 0 {
		return nil
	}
	var out []string
	for _, p := range pieces[:last] {
		if !p.meaningful {
			continue
		}
		if t := strings.TrimSpace(stripLeadingComments(s[p.start:p.end])); t != "" {
			out = append(out, t)
		}
	}
	// last statement: verbatim to end of batch (TrimSpace only decides whether
	// any token survives, e.g. unicode-space-only statements must vanish)
	if t := stripLeadingComments(s[pieces[last].start:]); strings.TrimSpace(t) != "" {
		out = append(out, t)
	}
	return out
}

// stripLeadingComments removes leading whitespace and full -- comment lines
// (DuckDB's statement text starts at the first token; leading comments are
// not part of it).
func stripLeadingComments(s string) string {
	for {
		t := strings.TrimLeft(s, " \t\n\r")
		if strings.HasPrefix(t, "--") {
			if j := strings.IndexByte(t, '\n'); j >= 0 {
				s = t[j+1:]
				continue
			}
			return ""
		}
		return t
	}
}

func isInternalError(msg string) bool {
	return strings.Contains(msg, "INTERNAL Error") || strings.Contains(msg, "FATAL Error")
}

func errorMatches(actual, expected string) bool {
	if strings.HasPrefix(expected, "<REGEX>:") || strings.HasPrefix(expected, "<!REGEX>:") {
		return regexMatches(actual, expected)
	}
	return strings.Contains(actual, expected)
}

func regexMatches(value, pattern string) bool {
	want := strings.HasPrefix(pattern, "<REGEX>:")
	p := strings.TrimPrefix(strings.TrimPrefix(pattern, "<REGEX>:"), "<!REGEX>:")
	re, err := regexp.Compile(`(?s)\A(?:` + p + `)\z`)
	if err != nil {
		return false
	}
	m := re.MatchString(value)
	return m == want
}

// ---------------------------------------------------------------- query

var hashRe = regexp.MustCompile(`^(\d+) values hashing to ([0-9a-f]{32})$`)

func (st *execState) execQuery(r *record, subs []sub) error {
	sqlText := st.substitute(r.sql, subs)
	conn, err := st.getConn(r.conn)
	if err != nil {
		return failStop{"runner: conn", err.Error(), r.line}
	}
	// run any leading ;-separated statements, then query the last segment
	segs := splitStatements(sqlText)
	if len(segs) == 0 {
		segs = []string{sqlText}
	}
	for _, pre := range segs[:len(segs)-1] {
		if _, err := conn.ExecContext(noCtx, pre); err != nil {
			msg := err.Error()
			if isInternalError(msg) {
				return failStop{failInternal, snip(pre) + "\n=> " + msg, r.line}
			}
			return failStop{errorBucket(msg), snip(pre) + "\n=> " + msg, r.line}
		}
		st.trackTxn(r.conn, pre) // "BEGIN; SELECT …" query records enter a tx
	}
	rows, qErr := conn.QueryContext(noCtx, segs[len(segs)-1])
	if qErr != nil {
		msg := qErr.Error()
		if isInternalError(msg) {
			return failStop{failInternal, snip(sqlText) + "\n=> " + msg, r.line}
		}
		return failStop{errorBucket(msg), snip(sqlText) + "\n=> " + msg, r.line}
	}
	cols, _ := rows.Columns()
	ncols := len(cols)
	// Column type names (driver typename.go) carry the temporal faces the
	// decoded values lack; must be read while rows are open.
	cts, _ := rows.ColumnTypes()
	colTypes := parseColTypes(cts, ncols)
	var resultVals []any
	for rows.Next() {
		ptrs := make([]any, ncols)
		vals := make([]any, ncols)
		for i := range ptrs {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			rows.Close()
			return failStop{failScan, snip(sqlText) + "\n=> " + err.Error(), r.line}
		}
		for i := range vals {
			if b, ok := vals[i].([]byte); ok { // defensive copy (driver reuses buffers)
				// kept as []byte: the driver delivers BLOB cells (and only those)
				// as []byte; VARCHAR arrives as string. The []byte type is the
				// only BLOB marker we have (no column type metadata).
				vals[i] = append([]byte(nil), b...)
			}
		}
		resultVals = append(resultVals, vals...)
	}
	closeErr := rows.Err()
	rows.Close()
	if closeErr != nil {
		msg := closeErr.Error()
		if isInternalError(msg) {
			return failStop{failInternal, snip(sqlText) + "\n=> " + msg, r.line}
		}
		return failStop{errorBucket(msg), snip(sqlText) + "\n=> " + msg, r.line}
	}

	nrows := 0
	if ncols > 0 {
		nrows = len(resultVals) / ncols
	}

	expLines := make([]string, len(r.expected))
	for i, l := range r.expected {
		expLines[i] = st.substitute(l, subs)
	}

	// NOTE on column counts: like the C++ runner (result_helper.cpp), the
	// declared query width (len(typeChars)) is NOT enforced for hash/label
	// comparisons at all (the hash flattens values, e.g. `query I nosort lbl`
	// over a 2-column SELECT is legal and common in the corpus). For value
	// comparisons the ACTUAL column count drives the comparison; a declared
	// width that differs only fails after the values were checked
	// (ColumnCountMismatchCorrectResult).

	// hash-based comparison?
	resultIsHash := len(expLines) == 1 && hashRe.MatchString(expLines[0])
	if resultIsHash || r.label != "" {
		// convert all values, apply sort, hash
		strs := make([]string, len(resultVals))
		for i, v := range resultVals {
			strs[i] = valueToString(v, colTypes[i%ncols], st.rctx())
		}
		applySort(r.sortStyle, strs, ncols)
		h := md5.New()
		for _, s := range strs {
			h.Write([]byte(s))
			h.Write([]byte("\n"))
		}
		digest := hex.EncodeToString(h.Sum(nil))
		if resultIsHash {
			m := hashRe.FindStringSubmatch(expLines[0])
			wantCount, _ := strconv.Atoi(m[1])
			if wantCount != len(resultVals) || m[2] != digest {
				return failStop{failHashMismatch,
					fmt.Sprintf("%s\nexpected %s\ngot      %d values hashing to %s",
						snip(sqlText), expLines[0], len(resultVals), digest), r.line}
			}
		}
		if r.label != "" {
			full := fmt.Sprintf("%d:%s", len(resultVals), digest)
			if prev, ok := st.labelHashes[r.label]; ok {
				if prev != full {
					return failStop{failHashMismatch,
						fmt.Sprintf("%s\nlabel %q result differs from earlier query with same label", snip(sqlText), r.label), r.line}
				}
			} else {
				st.labelHashes[r.label] = full
			}
			return nil // label queries compare via hash only (matches C++ runner)
		}
		return nil
	}

	// value-based comparison
	expValues, rowWise, perr := normalizeExpected(expLines, ncols, nrows)
	if perr != "" {
		return failStop{failWrongRows, snip(sqlText) + "\n" + perr, r.line}
	}
	expRows := 0
	if ncols > 0 {
		expRows = len(expValues) / ncols
	}
	if expRows != nrows {
		_ = rowWise
		return failStop{failWrongRows,
			fmt.Sprintf("%s\nexpected %d rows, got %d", snip(sqlText), expRows, nrows), r.line}
	}

	actStrs := make([]string, len(resultVals))
	for i, v := range resultVals {
		actStrs[i] = valueToString(v, colTypes[i%ncols], st.rctx())
	}
	sortedVals := resultVals
	if r.sortStyle != "" {
		// Sort the ACTUAL result only (values+strings together). Upstream's
		// runner never sorts the expected block (result_helper.cpp
		// CheckQueryResult: SortQueryResult applies to result_values_string
		// alone) — expected blocks are WRITTEN in converted-sorted order
		// ("1"/"0" booleans), so sorting them too diverges whenever the
		// expected spelling sorts differently than the converted actual
		// (e.g. "True" vs "1" in test_null_type_propagation).
		sortedVals = append([]any(nil), resultVals...)
		applySortVals(r.sortStyle, actStrs, sortedVals, ncols)
	}

	for i := range expValues {
		if i >= len(sortedVals) {
			break
		}
		if !compareValue(sortedVals[i], actStrs[i], expValues[i], st.rctx()) {
			row, col := i/ncols, i%ncols
			return failStop{failWrongResult,
				fmt.Sprintf("%s\nmismatch row %d col %d: %s <> %s",
					snip(sqlText), row+1, col+1, snip(actStrs[i]), snip(expValues[i])), r.line}
		}
	}
	if len(r.typeChars) != ncols {
		// values matched, declared width differs: C++ still fails the record
		// (ColumnCountMismatchCorrectResult)
		return failStop{failWrongCols,
			fmt.Sprintf("%s\nvalues match but query declares %d columns, result has %d",
				snip(sqlText), len(r.typeChars), ncols), r.line}
	}
	return nil
}

// normalizeExpected flattens expected lines into one value per entry,
// detecting row-wise (tab-separated) vs value-wise layout like the C++ runner.
func normalizeExpected(lines []string, ncols, nrows int) (vals []string, rowWise bool, errMsg string) {
	if len(lines) == 0 {
		return nil, false, ""
	}
	rowWise = ncols > 1 && len(lines) == nrows
	if !rowWise {
		allTabs := true
		for _, l := range lines {
			if !strings.Contains(l, "\t") {
				allTabs = false
				break
			}
		}
		rowWise = allTabs
	}
	if rowWise {
		for i, l := range lines {
			parts := strings.Split(l, "\t")
			if len(parts) != ncols {
				// The C++ runner splits with StringUtil::Split(line, "\t"), which
				// DROPS empty fields — upstream expected blocks legitimately contain
				// alignment tabs ("Bob\t\t6.5") and trailing tabs ("...\t89\t").
				// An intentionally empty cell can't be expressed that way upstream
				// either (it renders "(empty)"), so dropping empties is lossless.
				parts = dropEmpty(parts)
				if len(parts) != ncols {
					return nil, true, fmt.Sprintf("expected row %d has %d tab-separated values, want %d columns", i+1, len(parts), ncols)
				}
			}
			vals = append(vals, parts...)
		}
		return vals, true, ""
	}
	if ncols > 0 && len(lines)%ncols != 0 {
		return nil, false, fmt.Sprintf("%d expected values not divisible by %d columns", len(lines), ncols)
	}
	return lines, false, ""
}

// dropEmpty filters empty strings out of parts, mirroring DuckDB's
// StringUtil::Split(str, "\t") / Split(str, ",") semantics.
func dropEmpty(parts []string) []string {
	out := parts[:0:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// applySort sorts a flat value-string slice per sqllogictest sort styles.
func applySort(style string, vals []string, ncols int) {
	switch style {
	case "valuesort":
		sort.Strings(vals)
	case "rowsort":
		if ncols <= 0 {
			return
		}
		n := len(vals) / ncols
		rows := make([][]string, n)
		for i := 0; i < n; i++ {
			rows[i] = vals[i*ncols : (i+1)*ncols]
		}
		sort.Slice(rows, func(a, b int) bool { return rowLess(rows[a], rows[b]) })
		out := make([]string, 0, len(vals))
		for _, r := range rows {
			out = append(out, r...)
		}
		copy(vals, out)
	}
}

// applySortVals sorts both the string forms and the typed values in tandem.
func applySortVals(style string, strs []string, vals []any, ncols int) {
	if ncols <= 0 {
		return
	}
	switch style {
	case "valuesort":
		idx := make([]int, len(strs))
		for i := range idx {
			idx[i] = i
		}
		sort.SliceStable(idx, func(a, b int) bool { return strs[idx[a]] < strs[idx[b]] })
		reorder(strs, vals, idx)
	case "rowsort":
		n := len(strs) / ncols
		rowIdx := make([]int, n)
		for i := range rowIdx {
			rowIdx[i] = i
		}
		sort.SliceStable(rowIdx, func(a, b int) bool {
			ra := strs[rowIdx[a]*ncols : rowIdx[a]*ncols+ncols]
			rb := strs[rowIdx[b]*ncols : rowIdx[b]*ncols+ncols]
			return rowLess(ra, rb)
		})
		idx := make([]int, 0, len(strs))
		for _, ri := range rowIdx {
			for c := 0; c < ncols; c++ {
				idx = append(idx, ri*ncols+c)
			}
		}
		reorder(strs, vals, idx)
	}
}

func reorder(strs []string, vals []any, idx []int) {
	ns := make([]string, len(strs))
	nv := make([]any, len(vals))
	for to, from := range idx {
		ns[to] = strs[from]
		nv[to] = vals[from]
	}
	copy(strs, ns)
	copy(vals, nv)
}

func rowLess(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// ---------------------------------------------------------------- column types

// Temporal faces a result position can have. TIME, DATE and every TIMESTAMP
// width all decode to a bare time.Time, but DuckDB renders each differently
// ('12:13:14' / 2000-01-01 / '2000-01-01 00:00:00' inside containers, and a
// TIMESTAMPTZ in the session zone with an offset suffix). The face comes from
// the column's DatabaseTypeName (driver typename.go), parsed into a colType.
const (
	faceNone = iota
	faceDate
	faceTime
	faceTimestamp
	faceTimestampTZ
	// faceFloat32: FLOAT columns decode widened to float64 (the driver has no
	// float32 carrier); rendering must round back through float32 to get
	// DuckDB's shortest-roundtrip form (-3.4e+38, not -3.39999995e+38).
	faceFloat32
	// faceVariant: VARIANT cells decode to their final VARCHAR-cast string in
	// the driver; at nested positions they render RAW (the nested cast never
	// quotes VARIANT children).
	faceVariant
)

// colType is the parsed shape of one column's DuckDB type name: the temporal
// face at scalar positions plus the container structure needed to recurse in
// step with the decoded value ([]any / duckdb.Struct / duckdb.MapValue).
type colType struct {
	face   int
	elem   *colType   // LIST/ARRAY element
	key    *colType   // MAP key
	val    *colType   // MAP value
	fields []*colType // STRUCT fields in declared order
}

// nil-tolerant child accessors: an unknown/unparsed type renders with the
// face-less fallback heuristics at every position.
func (t *colType) elemType() *colType {
	if t == nil {
		return nil
	}
	return t.elem
}
func (t *colType) keyType() *colType {
	if t == nil {
		return nil
	}
	return t.key
}
func (t *colType) valType() *colType {
	if t == nil {
		return nil
	}
	return t.val
}
func (t *colType) fieldType(i int) *colType {
	if t == nil || i >= len(t.fields) {
		return nil
	}
	return t.fields[i]
}

// parseColType parses the driver's DatabaseTypeName rendering (typename.go):
// scalar names, `child[]` (LIST), `child[N]` (ARRAY), MAP(key, value),
// STRUCT("name" TYPE, ...). UNION members are deliberately not modeled — the
// decoded value is the ACTIVE member, which the type alone cannot identify —
// so positions under a UNION keep face heuristics. Unknown names parse to a
// face-less scalar.
func parseColType(s string) *colType {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if strings.HasSuffix(s, "]") {
		if open := strings.LastIndexByte(s, '['); open > 0 && allDigits(s[open+1:len(s)-1]) {
			return &colType{elem: parseColType(s[:open])}
		}
	}
	switch {
	case strings.HasPrefix(s, "MAP(") && strings.HasSuffix(s, ")"):
		if parts := splitTopLevel(s[4 : len(s)-1]); len(parts) == 2 {
			return &colType{key: parseColType(parts[0]), val: parseColType(parts[1])}
		}
		return &colType{}
	case strings.HasPrefix(s, "STRUCT(") && strings.HasSuffix(s, ")"):
		parts := splitTopLevel(s[7 : len(s)-1])
		t := &colType{fields: make([]*colType, len(parts))}
		for i, p := range parts {
			t.fields[i] = parseColType(stripFieldName(p))
		}
		return t
	}
	switch s {
	case "DATE":
		return &colType{face: faceDate}
	case "TIME", "TIME_NS":
		return &colType{face: faceTime}
	case "TIMESTAMP", "TIMESTAMP_S", "TIMESTAMP_MS", "TIMESTAMP_NS":
		return &colType{face: faceTimestamp}
	case "TIMESTAMP WITH TIME ZONE":
		return &colType{face: faceTimestampTZ}
	case "FLOAT":
		return &colType{face: faceFloat32}
	case "VARIANT":
		return &colType{face: faceVariant}
	}
	return &colType{}
}

// parseColTypes parses every column's DatabaseTypeName; a nil/short slice is
// returned as all-nil entries so callers can index unconditionally.
func parseColTypes(cts []*sql.ColumnType, ncols int) []*colType {
	types := make([]*colType, ncols)
	for i, ct := range cts {
		if i >= ncols {
			break
		}
		types[i] = parseColType(ct.DatabaseTypeName())
	}
	return types
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// splitTopLevel splits a type-argument list on commas at nesting depth 0,
// tracking ()/[] depth and double-quoted identifiers ("" doubling).
func splitTopLevel(s string) []string {
	var parts []string
	depth, start := 0, 0
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuote {
			if c == '"' {
				if i+1 < len(s) && s[i+1] == '"' {
					i++
				} else {
					inQuote = false
				}
			}
			continue
		}
		switch c {
		case '"':
			inQuote = true
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	return append(parts, strings.TrimSpace(s[start:]))
}

// stripFieldName drops the leading double-quoted field name (driver typename.go
// quotes unconditionally) from one STRUCT field entry, returning the type part.
func stripFieldName(p string) string {
	if len(p) == 0 || p[0] != '"' {
		if sp := strings.IndexByte(p, ' '); sp >= 0 { // defensive: unquoted name
			return p[sp+1:]
		}
		return p
	}
	for i := 1; i < len(p); i++ {
		if p[i] == '"' {
			if i+1 < len(p) && p[i+1] == '"' {
				i++
				continue
			}
			return strings.TrimSpace(p[i+1:])
		}
	}
	return ""
}

// ---------------------------------------------------------------- value conversion + comparison

// valueToString mirrors SQLLogicTestConvertValue (result_helper.cpp): NULL,
// (empty), \0 escaping; everything else like DuckDB's VARCHAR cast. t is the
// column's parsed type (nil = unknown: temporal faces fall back to heuristics)
// and tz the tracked session TimeZone for TIMESTAMPTZ rendering.
func valueToString(v any, t *colType, rc renderCtx) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case bool:
		// Top-level BOOLEAN cells convert to "1"/"0" like the C++ runner
		// (result_helper.cpp SQLLogicTestConvertValue) — this string feeds
		// rowsort/valuesort ordering and result hashing, so "true"/"false"
		// here corrupted sort order and md5 hashes. Booleans NESTED inside
		// containers still render "true"/"false" (the VARCHAR cast,
		// nestedToString).
		if x {
			return "1"
		}
		return "0"
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return formatFloatFace(x, t)
	case string:
		if x == "" {
			return "(empty)"
		}
		return strings.ReplaceAll(x, "\x00", "\\0")
	case duckdb.JSONValue:
		// JSON result cells arrive as the RAW JSON text (duckdb renders JSON
		// columns verbatim); \0 sanitization like plain strings.
		if string(x) == "" {
			return "(empty)"
		}
		return strings.ReplaceAll(string(x), "\x00", "\\0")
	case []byte:
		// BLOB: rendered like DuckDB's BLOB->VARCHAR cast (Blob::ToString,
		// src/common/types/blob.cpp): printable ASCII except \ ' " stays,
		// everything else becomes \xNN (uppercase hex).
		if len(x) == 0 {
			return "(empty)"
		}
		return blobToString(x)
	case time.Time:
		return formatTemporal(x, t, rc)
	case *big.Int:
		return x.String()
	case []any, duckdb.Struct, duckdb.MapValue, map[string]any:
		// The framework's \0 sanitization applies to the final converted
		// string, AFTER container quoting/escaping (a NUL is not a quote
		// trigger natively, so escaping never sees the substitute text).
		return strings.ReplaceAll(containerToString(v, t, rc), "\x00", "\\0")
	default:
		return fmt.Sprint(x)
	}
}

// containerToString renders a LIST/ARRAY ([]any), STRUCT or MAP cell the way
// DuckDB's nested-to-VARCHAR casts do (list_casts.cpp / struct_cast.cpp /
// map_cast.cpp): children that are themselves nested render raw; scalar
// children render via their VARCHAR cast and are then quoted/escaped per
// NestedToVarcharCast's lookup table (see quoteNested).
func containerToString(v any, t *colType, rc renderCtx) string {
	switch x := v.(type) {
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			if jv, ok := e.(duckdb.JSONValue); ok {
				// LIST(JSON) -> VARCHAR is the json extension's special cast
				// (CastJSONListToVarchar): children render RAW (no quoting).
				// JSON children of STRUCT/MAP go through the generic quoted
				// path instead (only the list cast is specialized).
				parts[i] = string(jv)
				continue
			}
			parts[i] = nestedToString(e, t.elemType(), rc)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case duckdb.Struct:
		// Unnamed structs (all field names empty) render as (v1, v2); named
		// ones as {'name': v, ...} with ALWAYS-quoted, backslash-escaped keys
		// (CalculateEscapedStringLength<STRUCT_KEY=true>).
		unnamed := len(x.Names) > 0
		for _, n := range x.Names {
			if n != "" {
				unnamed = false
				break
			}
		}
		parts := make([]string, len(x.Names))
		for i := range x.Names {
			parts[i] = nestedToString(x.Values[i], t.fieldType(i), rc)
			if !unnamed {
				parts[i] = escapeNestedQuoted(x.Names[i]) + ": " + parts[i]
			}
		}
		if unnamed {
			return "(" + strings.Join(parts, ", ") + ")"
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case duckdb.MapValue:
		parts := make([]string, len(x.Keys))
		for i, k := range x.Keys {
			parts[i] = nestedToString(k, t.keyType(), rc) + "=" + nestedToString(x.Values[i], t.valType(), rc)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case map[string]any:
		// Parsed JSON object (JSON result columns decode natively; STRUCTs
		// arrive as duckdb.Struct, not maps). Key order is JSON-object
		// arbitrary; sort for a stable rendering — JSON expected tokens are
		// compared structurally (jsonEquivalent), not via this string.
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = "'" + k + "': " + nestedToString(x[k], nil, rc)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

// nestedToString renders one child value inside a LIST/STRUCT/MAP: nested
// children recurse unquoted, scalar children render via their VARCHAR cast and
// then pass through DuckDB's quote-if-needed rule.
func nestedToString(v any, t *colType, rc renderCtx) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []any, duckdb.Struct, duckdb.MapValue, map[string]any:
		return containerToString(v, t, rc)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return formatFloatFace(x, t) // never contains quote triggers
	case string:
		if t != nil && t.face == faceVariant {
			return x // VARIANT child: pre-rendered, never quoted by the nested cast
		}
		return quoteNested(x)
	case duckdb.JSONValue:
		return quoteNested(string(x))
	case []byte:
		return quoteNested(blobToString(x))
	case time.Time:
		return quoteNested(formatTemporal(x, t, rc))
	default:
		// Interval, Decimal, *big.Int, ... — fmt.Stringer/VARCHAR-cast forms,
		// quote-checked like any scalar ('00:00:01.5' has a ':').
		return quoteNested(fmt.Sprint(x))
	}
}

// quoteNested applies NestedToVarcharCast's child-quoting rule
// (vector_cast_helpers.hpp CalculateEscapedStringLength/WriteEscapedString):
// quote when the string is empty, leads (or, at length >= 2, trails) with
// whitespace, equals "null" case-insensitively, or contains any character in
// the lookup table — " ' ( ) , : = [ ] { } . Quoting backslash-escapes ' and \.
func quoteNested(s string) string {
	if !nestedNeedsQuotes(s) {
		return s
	}
	return escapeNestedQuoted(s)
}

func nestedNeedsQuotes(s string) bool {
	if s == "" {
		return true
	}
	if isSpaceByte(s[0]) || (len(s) >= 2 && isSpaceByte(s[len(s)-1])) {
		return true
	}
	if strings.EqualFold(s, "null") {
		return true
	}
	return strings.ContainsAny(s, "\"'(),:=[]{}")
}

func isSpaceByte(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// escapeNestedQuoted single-quotes s, backslash-escaping embedded ' and \
// (WriteEscapedString writes \' and \\, NOT SQL quote-doubling).
func escapeNestedQuoted(s string) string {
	var sb strings.Builder
	sb.WriteByte('\'')
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\'' || c == '\\' {
			sb.WriteByte('\\')
		}
		sb.WriteByte(c)
	}
	sb.WriteByte('\'')
	return sb.String()
}

// formatTemporal renders a time.Time per the position's temporal face:
// DATE bare, TIME as time-of-day, TIMESTAMP always with a time part (DuckDB
// prints midnight timestamps as "... 00:00:00"), TIMESTAMPTZ in the session
// zone with DuckDB's offset suffix. Unknown face falls back to the legacy
// midnight-is-a-date heuristic (formatTimeValue).
func formatTemporal(x time.Time, t *colType, rc renderCtx) string {
	face := faceNone
	if t != nil {
		face = t.face
	}
	switch face {
	case faceDate:
		if s := specialDate(x); s != "" {
			return s
		}
		return formatYMD(x)
	case faceTime:
		return timeOfDayString(x)
	case faceTimestamp:
		return formatYMD(x) + " " + x.Format("15:04:05") + fracStr(x)
	case faceTimestampTZ:
		tz := rc.tz
		if tz == nil {
			tz = time.UTC
		}
		lx := x.In(tz)
		ymd := formatYMD(lx)
		switch rc.cal {
		case "":
			// The default cast is proleptic Gregorian (test_infinite_time).
		case "indian":
			// ICU's IndianCalendar (civil Saka): year = gregorian-78,
			// year starts at gregorian day-of-year 80; computed over the
			// PROLEPTIC Gregorian fields (ICU Grego:: helpers).
			ymd = formatYMDIndian(lx)
		default:
			// Any other non-default SET Calendar routes the engine's
			// TIMESTAMPTZ -> VARCHAR cast through ICU, whose calendars are
			// hybrid Julian/Gregorian (pre-1582 instants render as Julian
			// dates) — correct for 'japanese' and the other hybrid ones.
			ymd = formatYMDHybrid(lx)
		}
		return ymd + " " + lx.Format("15:04:05") + fracStr(lx) + offsetSuffix(lx)
	}
	return formatTimeValue(x)
}

func formatFloat(f float64) string { return formatFloatBits(f, 64) }

// formatFloatFace renders a float64 cell honoring the column face: FLOAT
// columns decode widened to float64, so shortest-roundtrip rendering must go
// back through float32 to match DuckDB (-3.4e+38, not -3.3999999521443642e+38).
func formatFloatFace(f float64, t *colType) string {
	if t != nil && t.face == faceFloat32 {
		return formatFloatBits(f, 32)
	}
	return formatFloatBits(f, 64)
}

// formatFloatBits mirrors DuckDB's float/double -> VARCHAR cast, which is
// duckdb_fmt::format("{}", v) (string_cast.cpp): shortest-roundtrip digits,
// FIXED notation iff -4 <= floor(log10|v|) < 16, scientific otherwise
// (float_writer's general-format rule, fmt format.h), integral values with a
// trailing ".0". Go's shortest 'g' would switch to scientific at e+06.
func formatFloatBits(f float64, bits int) string {
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
	e := strconv.FormatFloat(f, 'e', -1, bits) // shortest digits, exponent form
	d, _ := strconv.Atoi(e[strings.IndexByte(e, 'e')+1:])
	if d >= -4 && d < 16 {
		s := strconv.FormatFloat(f, 'f', -1, bits)
		if !strings.ContainsRune(s, '.') {
			s += ".0" // DuckDB renders integral doubles as "1.0", Go gives "1"
		}
		return s
	}
	// Scientific: Go's 'e' shortest form matches fmt (no decimal point for a
	// single digit, sign + >=2 exponent digits).
	return e
}

// blobToString mirrors Blob::ToString (duckdb-src/src/common/types/blob.cpp):
// bytes 32..126 except backslash, single and double quote render as-is, all
// other bytes as \xNN with uppercase hex.
func blobToString(b []byte) string {
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

// specialDate detects the engine's DATE ±infinity sentinels as delivered by
// the driver (date::infinity = 5881580-07-11, date::ninfinity = -5877641-06-24
// in Go's astronomical years; both outside the valid finite DATE range).
// DuckDB renders these as "infinity"/"-infinity".
func specialDate(t time.Time) string {
	if t.Hour() != 0 || t.Minute() != 0 || t.Second() != 0 || t.Nanosecond() != 0 {
		return ""
	}
	y, m, d := t.Date()
	if y == 5881580 && m == time.July && d == 11 {
		return "infinity"
	}
	if y == -5877641 && m == time.June && d == 24 {
		return "-infinity"
	}
	return ""
}

// fixedUTCZone parses the UTC±HH[:MM] / GMT±HH[:MM] fixed-offset TimeZone
// spellings ICU accepts ("UTC-0800", "UTC-08", "UTC-8", "UTC-08:00") that are
// not tzdata names. The sign is the literal UTC offset (UTC-8 == Etc/GMT+8).
func fixedUTCZone(name string) *time.Location {
	s := name
	switch {
	case strings.HasPrefix(s, "UTC"), strings.HasPrefix(s, "GMT"):
		s = s[3:]
	default:
		return nil
	}
	if len(s) < 2 || (s[0] != '+' && s[0] != '-') {
		return nil
	}
	neg := s[0] == '-'
	s = s[1:]
	s = strings.ReplaceAll(s, ":", "")
	if !allDigits(s) || len(s) == 0 || len(s) > 4 {
		return nil
	}
	var hh, mm int
	if len(s) <= 2 {
		hh, _ = strconv.Atoi(s)
	} else {
		hh, _ = strconv.Atoi(s[:len(s)-2])
		mm, _ = strconv.Atoi(s[len(s)-2:])
	}
	if hh > 15 || mm > 59 {
		return nil
	}
	off := hh*3600 + mm*60
	if neg {
		off = -off
	}
	return time.FixedZone(name, off)
}

// formatYMDHybrid renders the date part the way ICU's default (hybrid
// Julian/Gregorian) calendar does — DuckDB's TIMESTAMPTZ -> VARCHAR cast goes
// through ICU, which switches to the JULIAN calendar for instants before the
// 1582-10-15 Gregorian cutover (JDN 2299161). Go's time.Time is proleptic
// Gregorian, so earlier dates must be re-derived from the Julian day number.
func formatYMDHybrid(t time.Time) string {
	y, m, d := t.Date()
	// Gregorian -> JDN (valid for all astronomical years, floor semantics).
	a := floorDiv(int64(14-int(m)), 12)
	y2 := int64(y) + 4800 - a
	m2 := int64(int(m)) + 12*a - 3
	jdn := int64(d) + floorDiv(153*m2+2, 5) + 365*y2 + floorDiv(y2, 4) - floorDiv(y2, 100) + floorDiv(y2, 400) - 32045
	if jdn >= 2299161 {
		return formatYMD(t)
	}
	// JDN -> Julian calendar date.
	c := jdn + 32082
	d2 := floorDiv(4*c+3, 1461)
	e := c - floorDiv(1461*d2, 4)
	m3 := floorDiv(5*e+2, 153)
	day := e - floorDiv(153*m3+2, 5) + 1
	month := m3 + 3 - 12*floorDiv(m3, 10)
	year := d2 - 4800 + floorDiv(m3, 10)
	if year <= 0 {
		return fmt.Sprintf("%04d-%02d-%02d (BC)", 1-year, month, day)
	}
	return fmt.Sprintf("%04d-%02d-%02d", year, month, day)
}

// formatYMDIndian renders the date part the way ICU's IndianCalendar does
// (icu indiancalendar.cpp handleComputeFields): the civil Saka calendar,
// computed from the PROLEPTIC Gregorian date (ICU's Grego:: helpers are
// proleptic): Saka year = gregorian year - 78, starting at gregorian
// day-of-year 80 (1-based: day 81); month 1 has 30 days (31 in a Gregorian
// leap year), months 2-6 have 31 days, months 7-12 have 30.
func formatYMDIndian(t time.Time) string {
	gy, _, _ := t.Date()
	year := int64(gy) - 78
	yday := int64(t.YearDay() - 1) // 0-based Gregorian day of year
	const indianYearStart = 80
	var leapMonth int64
	if yday < indianYearStart {
		year--
		leapMonth = 30
		if isGregorianLeap(int64(gy) - 1) {
			leapMonth = 31
		}
		yday += leapMonth + 31*5 + 30*3 + 10
	} else {
		leapMonth = 30
		if isGregorianLeap(int64(gy)) {
			leapMonth = 31
		}
		yday -= indianYearStart
	}
	var month, day int64
	if yday < leapMonth {
		month = 1
		day = yday + 1
	} else {
		mday := yday - leapMonth
		if mday < 31*5 {
			month = mday/31 + 2
			day = mday%31 + 1
		} else {
			mday -= 31 * 5
			month = mday/30 + 7
			day = mday%30 + 1
		}
	}
	if year <= 0 {
		return fmt.Sprintf("%04d-%02d-%02d (BC)", 1-year, month, day)
	}
	return fmt.Sprintf("%04d-%02d-%02d", year, month, day)
}

// isGregorianLeap is the proleptic Gregorian leap rule over astronomical years
// (floor semantics for negatives).
func isGregorianLeap(y int64) bool {
	mod := func(a, b int64) int64 { return ((a % b) + b) % b }
	return mod(y, 4) == 0 && (mod(y, 100) != 0 || mod(y, 400) == 0)
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// formatYMD renders the date part like DuckDB's DATE->VARCHAR cast, including
// the " (BC)" suffix for non-positive (astronomical) years.
func formatYMD(t time.Time) string {
	y, m, d := t.Date()
	if y <= 0 {
		return fmt.Sprintf("%04d-%02d-%02d (BC)", 1-y, int(m), d)
	}
	return fmt.Sprintf("%04d-%02d-%02d", y, int(m), d)
}

// offsetSuffix renders the zone offset the way DuckDB casts TIMESTAMPTZ to
// VARCHAR (Time::ToUTCOffset via the ICU cast): the offset is truncated to
// whole MINUTES (toward zero — LMT zones like early America/Los_Angeles carry
// seconds, which native drops), rendered ±HH with :MM only when non-zero
// (e.g. "+00", "-08", "+05:30", "-07:52").
func offsetSuffix(t time.Time) string {
	_, off := t.Zone()
	mins := off / 60 // truncation toward zero matches C++ integer division
	sign := "+"
	if mins < 0 {
		sign = "-"
		mins = -mins
	}
	h, m := mins/60, mins%60
	out := fmt.Sprintf("%s%02d", sign, h)
	if m != 0 {
		out += fmt.Sprintf(":%02d", m)
	}
	return out
}

// formatTimeValue renders a time.Time the way DuckDB casts DATE/TIMESTAMP to
// VARCHAR when the column's face is UNKNOWN (no type metadata, e.g. under a
// UNION): midnight values render as a bare date (matches DATE; a midnight
// TIMESTAMP renders differently in DuckDB — the comparator compensates).
func formatTimeValue(t time.Time) string {
	if s := specialDate(t); s != "" {
		return s
	}
	if t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 && t.Nanosecond() == 0 {
		return formatYMD(t)
	}
	return formatYMD(t) + " " + t.Format("15:04:05") + fracStr(t)
}

// timeOfDayString renders a TIME cell. The driver decodes TIME as an offset
// from the 1970-01-01 epoch, so the legal extreme TIME '24:00:00' lands on
// 1970-01-02 and would render "00:00:00" through Format; computing hours from
// the epoch offset keeps DuckDB's "24:00:00".
func timeOfDayString(x time.Time) string {
	if ns := x.UnixNano(); ns >= 0 && ns <= 24*3600*1e9 {
		secs := ns / 1e9
		return fmt.Sprintf("%02d:%02d:%02d", secs/3600, secs/60%60, secs%60) + fracStr(x)
	}
	return x.Format("15:04:05") + fracStr(x)
}

// fracStr renders the fractional-seconds suffix, trailing-zero trimmed at full
// nanosecond width (micro-precision values print identically to a 6-digit
// render; TIMESTAMP_NS/TIME_NS keep their sub-microsecond digits).
func fracStr(t time.Time) string {
	ns := t.Nanosecond()
	if ns == 0 {
		return ""
	}
	return strings.TrimRight(fmt.Sprintf(".%09d", ns), "0")
}

// compareValue checks a single actual value against one expected token,
// mirroring TestResultHelper::CompareValues. tz is the tracked session
// TimeZone (nil = none): TIMESTAMPTZ values arrive from the driver as bare
// UTC instants with no type marker, so the comparator additionally accepts
// the rendering in the session zone with DuckDB's offset suffix.
func compareValue(actual any, actualStr, expected string, rc renderCtx) bool {
	if actualStr == expected {
		return true
	}
	if strings.HasPrefix(expected, "<REGEX>:") || strings.HasPrefix(expected, "<!REGEX>:") {
		return regexMatches(actualStr, expected)
	}
	// numeric/text fallbacks tolerate surrounding whitespace in the expected
	// token (DuckDB's comparator casts, which trims)
	expTrim := strings.TrimSpace(expected)
	switch x := actual.(type) {
	case nil:
		// "null" (lowercase) is accepted because the driver parses JSON cells
		// natively: a JSON null arrives as Go nil while the corpus expects the
		// raw JSON text "null" (no column type metadata to tell JSON apart).
		return expTrim == "NULL" || expTrim == "null"
	case bool:
		e := strings.ToLower(expTrim)
		if x {
			return e == "true" || e == "1"
		}
		return e == "false" || e == "0"
	case int64:
		if ei, err := strconv.ParseInt(expTrim, 10, 64); err == nil {
			return ei == x
		}
		if ef, err := strconv.ParseFloat(expTrim, 64); err == nil {
			return ef == float64(x)
		}
		if strings.EqualFold(expTrim, "true") {
			return x == 1
		}
		if strings.EqualFold(expTrim, "false") {
			return x == 0
		}
		return false
	case float64:
		if ef, err := strconv.ParseFloat(expTrim, 64); err == nil {
			if approxEqual(x, ef) {
				return true
			}
			// FLOAT columns arrive widened to float64 (the driver has no
			// float32 carrier), while the C++ runner casts the expected string
			// to the COLUMN type before comparing. Accept the 32-bit reading
			// too: e.g. uhugeint_max::FLOAT is +Inf (f32 demote of 2^128-1),
			// and ParseFloat(expected, 32) overflows to +Inf, matching native.
			if ef32, err32 := strconv.ParseFloat(expTrim, 32); err32 == nil || errors.Is(err32, strconv.ErrRange) {
				return approxEqual(float64(float32(x)), ef32)
			}
			return false
		}
		if strings.EqualFold(expTrim, "nan") {
			return math.IsNaN(x)
		}
		if strings.EqualFold(expTrim, "inf") || expTrim == "infinity" {
			return math.IsInf(x, 1)
		}
		if strings.EqualFold(expTrim, "-inf") || expTrim == "-infinity" {
			return math.IsInf(x, -1)
		}
		return false
	case time.Time:
		// accept date / timestamp / timestamptz / time renderings
		if s := specialDate(x); s != "" {
			return s == expTrim
		}
		ts := formatYMD(x) + " " + x.Format("15:04:05") + fracStr(x)
		cands := []string{
			formatYMD(x),
			ts,
			ts + "+00",
			timeOfDayString(x),
		}
		if tz := rc.tz; tz != nil {
			// TIMESTAMPTZ rendering in the session TimeZone (driver gives the
			// UTC instant; DuckDB renders in the session zone with offset)
			lx := x.In(tz)
			lts := formatYMD(lx) + " " + lx.Format("15:04:05") + fracStr(lx) + offsetSuffix(lx)
			cands = append(cands, lts)
		}
		for _, c := range cands {
			if c == expTrim {
				return true
			}
		}
		return false
	case *big.Int:
		if er, ok := new(big.Rat).SetString(expTrim); ok {
			return new(big.Rat).SetInt(x).Cmp(er) == 0
		}
		return false
	}
	// strings, decimals (fmt.Stringer), anything else rendered as text:
	// compare numerically when both sides parse as exact rationals
	if ar, ok := new(big.Rat).SetString(strings.TrimSpace(actualStr)); ok {
		if er, ok2 := new(big.Rat).SetString(expTrim); ok2 {
			return ar.Cmp(er) == 0
		}
	}
	// JSON columns: the driver delivers parsed native values (maps/slices,
	// unquoted strings) while the corpus expects the raw JSON text. The driver
	// exposes no column type metadata, so when the expected token itself is
	// JSON, compare structurally.
	if len(expTrim) > 0 && (expTrim[0] == '{' || expTrim[0] == '[' || expTrim[0] == '"') {
		switch actual.(type) {
		case string, map[string]any, []any, duckdb.JSONValue:
			if jsonEquivalent(actual, expTrim) {
				return true
			}
		}
	}
	return false
}

// jsonEquivalent reports whether the expected token, parsed as JSON, is
// structurally equal to the actual driver value (both normalized through
// encoding/json so numeric types and key order are canonical).
func jsonEquivalent(actual any, expected string) bool {
	var expVal any
	if err := json.Unmarshal([]byte(expected), &expVal); err != nil {
		return false
	}
	if jv, ok := actual.(duckdb.JSONValue); ok {
		// Raw JSON text: parse it rather than marshalling the wrapper string.
		var actVal any
		if err := json.Unmarshal([]byte(jv), &actVal); err != nil {
			return false
		}
		return reflect.DeepEqual(actVal, expVal)
	}
	actBytes, err := json.Marshal(actual)
	if err != nil {
		return false
	}
	var actVal any
	if err := json.Unmarshal(actBytes, &actVal); err != nil {
		return false
	}
	return reflect.DeepEqual(actVal, expVal)
}

// approxEqual is DuckDB's ApproxEqual for doubles (src/common/types.cpp).
func approxEqual(l, r float64) bool {
	if math.IsNaN(l) && math.IsNaN(r) {
		return true
	}
	if math.IsInf(l, 0) || math.IsInf(r, 0) {
		return l == r
	}
	eps := math.Abs(r)*0.01 + 0.00000001
	return math.Abs(l-r) <= eps
}

// ---------------------------------------------------------------- failure bucketing

// errorBucket normalizes an engine error message into a bucket key.
func errorBucket(msg string) string {
	msg = strings.TrimPrefix(msg, "duckdb prepare: ")
	msg = strings.TrimPrefix(msg, "duckdb execute: ")
	// keep the DuckDB error class if present ("Binder Error", "IO Error",
	// "Invalid Input Error", ...)
	if i := strings.Index(msg, " Error: "); i > 0 && i < 40 && !strings.Contains(msg[:i], "\n") {
		cls := msg[:i+6]
		rest := strings.TrimSpace(msg[i+8:])
		return "unexpected error: " + cls + ": " + normalizeMsg(rest, 60)
	}
	return "unexpected error: " + normalizeMsg(msg, 60)
}

var (
	numRe   = regexp.MustCompile(`\d+`)
	quoteRe = regexp.MustCompile(`"[^"]*"|'[^']*'`)
)

func normalizeMsg(s string, n int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = quoteRe.ReplaceAllString(s, "?")
	s = numRe.ReplaceAllString(s, "#")
	if len(s) > n {
		s = s[:n]
	}
	return s
}

func snip(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}

// ---------------------------------------------------------------- report

func report(results []fileResult, totalFound int, wall time.Duration) {
	var pass, fail, skip int
	skipReasons := map[string]int{}
	type bucket struct {
		count    int
		examples []string
	}
	buckets := map[string]*bucket{}
	var failingFiles []string
	var recRun, recPassed, recSkipped, autoRollbacks int

	for _, r := range results {
		recRun += r.recRun
		recPassed += r.recPassed
		recSkipped += r.recSkipped
		autoRollbacks += r.autoRollbacks
		switch r.outcome {
		case "PASS":
			pass++
		case "SKIP":
			skip++
			skipReasons[skipReasonKey(r.reason)]++
		case "FAIL":
			fail++
			b := buckets[r.reason]
			if b == nil {
				b = &bucket{}
				buckets[r.reason] = b
			}
			b.count++
			if len(b.examples) < 2 {
				ex := relPath(r.path)
				if r.detail != "" {
					d := r.detail
					if len(d) > 300 {
						d = d[:300] + "..."
					}
					ex += "\n        " + strings.ReplaceAll(d, "\n", "\n        ")
				}
				b.examples = append(b.examples, ex)
			}
			failingFiles = append(failingFiles, fmt.Sprintf("%s  [%s] line %d", relPath(r.path), r.reason, r.line))
		}
	}

	fmt.Printf("\n================ SQLLOGICTEST REPORT (pure-Go DuckDB) ================\n")
	fmt.Printf("corpus files found:  %d (.test only, .test_slow excluded)\n", totalFound)
	fmt.Printf("files executed:      %d\n", len(results))
	fmt.Printf("  PASS:              %d (%.1f%% of executed)\n", pass, pct(pass, len(results)))
	fmt.Printf("  FAIL:              %d (%.1f%%)\n", fail, pct(fail, len(results)))
	fmt.Printf("  SKIP:              %d (%.1f%%)\n", skip, pct(skip, len(results)))
	if runAttempted := pass + fail; runAttempted > 0 {
		fmt.Printf("  PASS rate of run (excl. skipped): %.1f%%\n", pct(pass, runAttempted))
	}
	fmt.Printf("records: %d run, %d passed, %d skipped (records after a file's first failure are not counted)\n",
		recRun, recPassed, recSkipped)
	fmt.Printf("sacrificial ROLLBACKs issued by runner: %d\n"+
		"  (engine/driver leaves a dangling open transaction after a failed statement;\n"+
		"  the NEXT statement on that connection fails with 'cannot start a transaction\n"+
		"  within a transaction'. The runner absorbs that one-shot poison with a ROLLBACK\n"+
		"  after every expected error outside explicit BEGIN, so later records measure\n"+
		"  the engine rather than the leak.)\n", autoRollbacks)
	fmt.Printf("wall time: %s\n", wall.Round(time.Second))

	fmt.Printf("\n---- skip reasons ----\n")
	printSorted(skipReasons)

	fmt.Printf("\n---- failure buckets (top 20) ----\n")
	type kv struct {
		k string
		v *bucket
	}
	var kvs []kv
	for k, v := range buckets {
		kvs = append(kvs, kv{k, v})
	}
	sort.Slice(kvs, func(a, b int) bool {
		if kvs[a].v.count != kvs[b].v.count {
			return kvs[a].v.count > kvs[b].v.count
		}
		return kvs[a].k < kvs[b].k
	})
	for i, e := range kvs {
		if i >= 20 {
			fmt.Printf("  ... and %d more buckets\n", len(kvs)-20)
			break
		}
		fmt.Printf("%4d  %s\n", e.v.count, e.k)
		for _, ex := range e.v.examples {
			fmt.Printf("      e.g. %s\n", ex)
		}
	}

	fmt.Printf("\n---- failing files (%d) ----\n", len(failingFiles))
	sort.Strings(failingFiles)
	for _, f := range failingFiles {
		fmt.Println(f)
	}
}

func skipReasonKey(r string) string {
	for _, p := range []string{"require-env", "require ", "directive: ", "parse: "} {
		if strings.HasPrefix(r, p) {
			if p == "require " {
				return r // keep extension name
			}
			return r
		}
	}
	return r
}

func printSorted(m map[string]int) {
	type kv struct {
		k string
		v int
	}
	var kvs []kv
	for k, v := range m {
		kvs = append(kvs, kv{k, v})
	}
	sort.Slice(kvs, func(a, b int) bool {
		if kvs[a].v != kvs[b].v {
			return kvs[a].v > kvs[b].v
		}
		return kvs[a].k < kvs[b].k
	})
	for _, e := range kvs {
		fmt.Printf("%4d  %s\n", e.v, e.k)
	}
}

func pct(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return 100 * float64(n) / float64(d)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
