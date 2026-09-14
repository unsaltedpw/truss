package secrets

import (
	"encoding/json"
	"strings"
)

// redact replaces every occurrence of each non-empty value in s with "***".
// Used everywhere an error message is built from a response body or a
// transport error that might, in principle, echo back the client token or
// the JWT this package logged in with -- neither may ever appear in
// anything this package returns (§4.7 "Refuses to": "log or return any
// credential value, the Vault token, or the JWT").
//
// ⚠️ BOTH WIRE FORMS OF A VALUE ARE REPLACED, NOT JUST THE Go STRING. A server
// that echoes a request body back has re-encoded it: a private key whose value
// contains real newlines comes back with `\n` as two characters, and a literal
// ReplaceAll of the Go string matches nothing in it. That is not hypothetical
// here -- this package writes JSON bodies (KV.PutValue, OP.PutValue), so every
// value it sends has a JSON-escaped form in flight, and a 4xx or 5xx response
// that quotes the request back is the failure mode that turns "no credential in
// any error" into a claim nobody tested. Measured by
// TestRedactCoversTheJSONEscapedForm, which fails against the single-form
// version of this loop.
func redact(s string, values ...string) string {
	for _, v := range values {
		if v == "" {
			continue
		}
		s = strings.ReplaceAll(s, v, "***")
		if esc := jsonEscapedForm(v); esc != "" {
			s = strings.ReplaceAll(s, esc, "***")
		}
	}
	return s
}

// jsonEscapedForm is how v appears inside a JSON string literal, without the
// surrounding quotes. Empty on any of the cases where it would add nothing: a
// marshal failure, or a value with nothing to escape.
func jsonEscapedForm(v string) string {
	b, err := json.Marshal(v)
	if err != nil || len(b) < 2 {
		return ""
	}
	inner := string(b[1 : len(b)-1])
	if inner == v {
		return ""
	}
	return inner
}
