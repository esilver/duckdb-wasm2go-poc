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

	duckdb "duckdbconverge/duckdb"
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
			fmt.Printf("decoded:   %q\n", decodeEngineError(err.Error()))
			return
		}
		cols, _ := rows.Columns()
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
				fmt.Printf("col %d: %T %q -> %s\n", i+1, v, fmt.Sprint(v), valueToString(v))
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
	stack := []*[]record{&root}     // innermost loop body last
	loopStack := []*record{}        // parallel to stack[1:]
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
			// ignored (safe / not applicable to a fresh in-memory engine per file)
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
	case "ram", "disk_space":
		return "" // host has plenty
	}
	return "require " + param
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

	recRun        int
	recPassed     int
	recSkipped    int
	autoRollbacks int
}

type sub struct{ name, val string }

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
	st := &execState{
		db:          db,
		conns:       map[string]*sql.Conn{},
		inTxn:       map[string]bool{},
		labelHashes: map[string]string{},
		testDir:     scratch,
		testName:    relPath(path),
		testUUID:    fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
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
		case recLoop:
			for v := r.loopStart; v < r.loopEnd; v++ {
				err := st.execRecords(r.body, append(subs, sub{r.loopVar, strconv.Itoa(v)}))
				if err != nil {
					return err
				}
			}
		case recForeach:
			for _, tokv := range r.loopTokens {
				err := st.execRecords(r.body, append(subs, sub{r.loopVar, tokv}))
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
	}
	if rawErr == nil {
		if m := setTimeZoneRe.FindStringSubmatch(sqlText); m != nil {
			if loc, lerr := time.LoadLocation(m[1]); lerr == nil {
				st.tz = loc
			} else {
				st.tz = nil
			}
		} else if resetTimeZoneRe.MatchString(sqlText) {
			st.tz = nil
		}
	}
	var errMsg string
	if rawErr != nil {
		errMsg = decodeEngineError(rawErr.Error())
	}

	// Track explicit transactions, and work around an ENGINE/DRIVER issue:
	// a failed statement leaves the connection's (autocommit) transaction
	// open, so every later statement fails with "cannot start a transaction
	// within a transaction". When the test was NOT inside an explicit BEGIN,
	// issue a ROLLBACK to clear the leaked transaction. Counted+reported.
	upper := strings.ToUpper(strings.TrimSpace(sqlText))
	if rawErr == nil {
		switch {
		case strings.HasPrefix(upper, "BEGIN") || strings.HasPrefix(upper, "START TRANSACTION"):
			st.inTxn[r.conn] = true
		case strings.HasPrefix(upper, "COMMIT") || strings.HasPrefix(upper, "ROLLBACK") || strings.HasPrefix(upper, "ABORT"):
			st.inTxn[r.conn] = false
		}
	} else if !st.inTxn[r.conn] {
		// The leaked transaction makes the NEXT statement fail at txn-begin
		// ("cannot start a transaction within a transaction"), which clears
		// it. This sacrificial ROLLBACK absorbs that one-shot poison.
		st.autoRollbacks++
		_, _ = conn.ExecContext(noCtx, "ROLLBACK")
	}

	expErr := st.substitute(strings.Join(r.expected, "\n"), subs)

	if rawErr != nil && isInternalError(errMsg) {
		return failStop{failInternal, snip(sqlText) + "\n=> " + errMsg, r.line}
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

var jsonFieldRes = map[string]*regexp.Regexp{}
var jsonFieldMu sync.Mutex

// extractJSONField pulls one string field out of (possibly truncated/invalid)
// JSON and unescapes it best-effort.
func extractJSONField(payload, field string) string {
	jsonFieldMu.Lock()
	re := jsonFieldRes[field]
	if re == nil {
		re = regexp.MustCompile(`"` + field + `"\s*:\s*"((?:[^"\\]|\\.)*)"`)
		jsonFieldRes[field] = re
	}
	jsonFieldMu.Unlock()
	m := re.FindStringSubmatch(payload)
	if m == nil {
		return ""
	}
	var s string
	if err := json.Unmarshal([]byte(`"`+m[1]+`"`), &s); err != nil {
		return strings.NewReplacer(`\"`, `"`, `\n`, "\n", `\t`, "\t", `\\`, `\`).Replace(m[1])
	}
	return s
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

// decodeEngineError renders the driver's error the way DuckDB's own test
// runner sees it. The pure-Go driver surfaces the raw C++ exception as JSON
// ({"exception_type":"Binder","exception_message":"..."}); DuckDB renders that
// as "Binder Error: ...". This is purely error-FORMAT normalization so that
// the corpus' expected-error substrings/regexes are matched against the same
// text the C++ runner matches against.
func decodeEngineError(msg string) string {
	prefix := ""
	rest := msg
	for _, p := range []string{"duckdb prepare: ", "duckdb execute: ", "duckdb bind param "} {
		if i := strings.Index(rest, p); i >= 0 {
			prefix = rest[:i]
			rest = rest[i+len(p):]
			break
		}
	}
	j := strings.Index(rest, "{")
	if j < 0 {
		return msg
	}
	var exc struct {
		Type     string `json:"exception_type"`
		Message  string `json:"exception_message"`
		Position string `json:"position"`
	}
	payload := rest[j:]
	if err := json.Unmarshal([]byte(payload), &exc); err != nil {
		// The host truncates long throw messages, leaving unterminated JSON,
		// and raw control chars may appear inside strings. Fall back to
		// field extraction by regex.
		exc.Type = extractJSONField(payload, "exception_type")
		exc.Message = extractJSONField(payload, "exception_message")
	}
	if exc.Type == "" {
		return msg
	}
	cls := exc.Type
	if !strings.Contains(cls, "Error") {
		cls += " Error"
	}
	return prefix + rest[:j] + cls + ": " + exc.Message
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
			msg := decodeEngineError(err.Error())
			if isInternalError(msg) {
				return failStop{failInternal, snip(pre) + "\n=> " + msg, r.line}
			}
			return failStop{errorBucket(msg), snip(pre) + "\n=> " + msg, r.line}
		}
	}
	rows, qErr := conn.QueryContext(noCtx, segs[len(segs)-1])
	if qErr != nil {
		msg := decodeEngineError(qErr.Error())
		if isInternalError(msg) {
			return failStop{failInternal, snip(sqlText) + "\n=> " + msg, r.line}
		}
		return failStop{errorBucket(msg), snip(sqlText) + "\n=> " + msg, r.line}
	}
	cols, _ := rows.Columns()
	ncols := len(cols)
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
		msg := decodeEngineError(closeErr.Error())
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
			strs[i] = valueToString(v)
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
		actStrs[i] = valueToString(v)
	}
	sortedVals := resultVals
	if r.sortStyle != "" {
		// sort actual (values+strings together) and expected the same way
		sortedVals = append([]any(nil), resultVals...)
		applySortVals(r.sortStyle, actStrs, sortedVals, ncols)
		applySort(r.sortStyle, expValues, ncols)
	}

	for i := range expValues {
		if i >= len(sortedVals) {
			break
		}
		if !compareValue(sortedVals[i], actStrs[i], expValues[i], st.tz) {
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
				return nil, true, fmt.Sprintf("expected row %d has %d tab-separated values, want %d columns", i+1, len(parts), ncols)
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

// ---------------------------------------------------------------- value conversion + comparison

// valueToString mirrors SQLLogicTestConvertValue (result_helper.cpp): NULL,
// (empty), \0 escaping; everything else like DuckDB's VARCHAR cast.
func valueToString(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return formatFloat(x)
	case string:
		if x == "" {
			return "(empty)"
		}
		return strings.ReplaceAll(x, "\x00", "\\0")
	case []byte:
		// BLOB: rendered like DuckDB's BLOB->VARCHAR cast (Blob::ToString,
		// src/common/types/blob.cpp): printable ASCII except \ ' " stays,
		// everything else becomes \xNN (uppercase hex).
		if len(x) == 0 {
			return "(empty)"
		}
		return blobToString(x)
	case time.Time:
		return formatTimeValue(x)
	case *big.Int:
		return x.String()
	case []any:
		// DuckDB list rendering: [a, b, NULL]
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = nestedToString(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case duckdb.Struct:
		// STRUCT: DuckDB renders {'a': 1, 'b': x} in DECLARED field order; the
		// driver's Struct carrier preserves it.
		parts := make([]string, len(x.Names))
		for i, k := range x.Names {
			parts[i] = "'" + k + "': " + nestedToString(x.Values[i])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case duckdb.MapValue:
		// MAP: DuckDB renders {key=value, ...} in entry order; the driver's
		// MapValue carrier preserves it (and admits unhashable LIST/STRUCT keys).
		parts := make([]string, len(x.Keys))
		for i, k := range x.Keys {
			parts[i] = nestedToString(k) + "=" + nestedToString(x.Values[i])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case map[string]any:
		// Parsed JSON object (JSON result columns decode natively; STRUCTs now
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
			parts[i] = "'" + k + "': " + nestedToString(x[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		return fmt.Sprint(x)
	}
}

// nestedToString renders a value inside a list/struct (no "(empty)"
// placeholder; strings are single-quoted when they need it, like DuckDB).
func nestedToString(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case string:
		if x == "" {
			return "''"
		}
		if strings.ContainsAny(x, ",[]{}'\"") ||
			strings.TrimSpace(x) != x || strings.EqualFold(x, "null") {
			return "'" + strings.ReplaceAll(x, "'", "''") + "'"
		}
		return x
	default:
		return valueToString(v)
	}
}

func formatFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	// DuckDB renders integral doubles as "1.0", Go gives "1"
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
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
// VARCHAR: ±HH, with :MM only when non-zero minutes, :SS only when non-zero
// seconds (e.g. "+00", "-08", "+05:30", "+00:50:20").
func offsetSuffix(t time.Time) string {
	_, off := t.Zone()
	sign := "+"
	if off < 0 {
		sign = "-"
		off = -off
	}
	h, rem := off/3600, off%3600
	m, s := rem/60, rem%60
	out := fmt.Sprintf("%s%02d", sign, h)
	if m != 0 || s != 0 {
		out += fmt.Sprintf(":%02d", m)
	}
	if s != 0 {
		out += fmt.Sprintf(":%02d", s)
	}
	return out
}

// formatTimeValue renders a time.Time the way DuckDB casts DATE/TIMESTAMP to
// VARCHAR. The driver gives us no column type, so midnight values render as a
// bare date (matches DATE; a midnight TIMESTAMP renders differently in DuckDB
// — the comparator compensates).
func formatTimeValue(t time.Time) string {
	if s := specialDate(t); s != "" {
		return s
	}
	if t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 && t.Nanosecond() == 0 {
		return formatYMD(t)
	}
	return formatYMD(t) + " " + t.Format("15:04:05") + fracMicros(t)
}

func fracMicros(t time.Time) string {
	us := t.Nanosecond() / 1000
	if us == 0 {
		return ""
	}
	s := fmt.Sprintf(".%06d", us)
	return strings.TrimRight(s, "0")
}

// compareValue checks a single actual value against one expected token,
// mirroring TestResultHelper::CompareValues. tz is the tracked session
// TimeZone (nil = none): TIMESTAMPTZ values arrive from the driver as bare
// UTC instants with no type marker, so the comparator additionally accepts
// the rendering in the session zone with DuckDB's offset suffix.
func compareValue(actual any, actualStr, expected string, tz *time.Location) bool {
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
		ts := formatYMD(x) + " " + x.Format("15:04:05") + fracMicros(x)
		cands := []string{
			formatYMD(x),
			ts,
			ts + "+00",
			x.Format("15:04:05") + fracMicros(x),
		}
		if tz != nil {
			// TIMESTAMPTZ rendering in the session TimeZone (driver gives the
			// UTC instant; DuckDB renders in the session zone with offset)
			lx := x.In(tz)
			lts := formatYMD(lx) + " " + lx.Format("15:04:05") + fracMicros(lx) + offsetSuffix(lx)
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
		case string, map[string]any, []any:
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
