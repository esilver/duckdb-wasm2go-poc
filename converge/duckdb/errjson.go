// errjson.go — native-shaped rendering of raw C++ exception payloads.
//
// DuckDB's C++ exceptions carry their payload as TRANSPORT JSON: what() of a
// duckdb::Exception is `{"exception_type":"Binder","exception_message":"..."}`
// (plus extra fields). On the normal result path the engine itself parses that
// back into ErrorData and ClientContext::ProcessError renders the native text
// ("Binder Error: ..." with the LINE context, or — under SET errors_as_json —
// the intended JSON), which duckdb_result_error/duckdb_prepare_error then
// surface verbatim. The HOST-side fallback (exhost's LastThrowMessage, used
// when the C result carries no message) sees the raw transport JSON instead;
// decodeExceptionJSON renders it the way native DuckDB would, so every error
// this driver returns is native-shaped and the intended errors_as_json output
// is never confused with transport JSON (it only ever arrives via the result
// error, which is left untouched).
package duckdb

import (
	"encoding/json"
	"regexp"
	"strings"
	"sync"
)

// decodeExceptionJSON renders a raw exception transport-JSON payload as the
// native "<Type> Error: <message>" text. Text without a JSON object, or whose
// object has no exception_type, passes through unchanged.
func decodeExceptionJSON(msg string) string {
	j := strings.Index(msg, "{")
	if j < 0 {
		return msg
	}
	var exc struct {
		Type    string `json:"exception_type"`
		Message string `json:"exception_message"`
	}
	payload := msg[j:]
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
	return msg[:j] + cls + ": " + exc.Message
}

var (
	jsonFieldRes = map[string]*regexp.Regexp{}
	jsonFieldMu  sync.Mutex
)

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
