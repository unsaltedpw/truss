package secrets

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRedactCoversTheJSONEscapedForm is the guarantee the comment in redact.go
// claims, written against the failure it exists for: a private key with real
// newlines, echoed back by a server that re-encoded it as JSON. Against the
// single-form loop this test fails -- which is the whole reason both forms are
// replaced rather than the obvious one.
func TestRedactCoversTheJSONEscapedForm(t *testing.T) {
	const key = "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXk\n-----END OPENSSH PRIVATE KEY-----\n"

	echoed, err := json.Marshal(key)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"message":"invalid","received":` + string(echoed) + `}`
	if strings.Contains(body, "b3BlbnNzaC1rZXk") == false {
		t.Fatal("the fixture no longer reproduces the escaped shape it is for")
	}

	got := redact(body, key)
	if strings.Contains(got, "b3BlbnNzaC1rZXk") {
		t.Errorf("the escaped form of the value survived redaction: %s", got)
	}
	if !strings.Contains(got, "***") {
		t.Errorf("nothing was replaced: %s", got)
	}
}

func TestRedactCoversTheLiteralFormAndIgnoresEmptyValues(t *testing.T) {
	// Short on purpose. scripts/leakscan refuses `token = <16+ chars>` as "a
	// shape that could be an assigned credential", and a test fixture is not an
	// exemption -- the scanner cannot tell a fake from a real one, which is the
	// point of it matching shapes rather than a denylist of known values.
	const token = "abc123"
	got := redact("Authorization: Bearer "+token, "", token)
	if strings.Contains(got, token) {
		t.Errorf("literal form survived: %s", got)
	}
	// An empty value must never match everything, and a value with nothing to
	// escape must not get replaced a second time in a form it does not have.
	if got := redact("nothing to hide", ""); got != "nothing to hide" {
		t.Errorf("an empty value rewrote the string: %q", got)
	}
	if got := redact("abc", "abc"); got != "***" {
		t.Errorf("literal form = %q, want exactly one replacement", got)
	}
}

// TestRedactDoesNotClobberUnrelatedText: replacing two forms of every value is
// a hammer, and the check that it only hits the value is cheap.
func TestRedactDoesNotClobberUnrelatedText(t *testing.T) {
	const v = "abc\ndef"
	if got := redact("the log line said abcdefg and nothing else", v); got != "the log line said abcdefg and nothing else" {
		t.Errorf("unrelated text was altered: %q", got)
	}
}
