package gates

import (
	"os"
	"strings"
	"testing"
)

// githubActorTokens are the values GitHub's ruleset API puts in
// bypass_actors[].actor_type. They are a provider's vocabulary, not a fact
// about protection.
var githubActorTokens = []string{
	"Integration", "Machine", "DeployKey", "RepositoryRole", "OrganizationAdmin", "Team", "Bot",
}

// allowedActorTokens is the whole of this package's dependence on that
// vocabulary, by file. The goal is an empty map.
//
// ⚠️ It is a ratchet, not a budget. Every entry names a place where a gate
// decides something security-relevant by comparing a string GitHub invented,
// which means a second forge cannot satisfy the gate honestly: it would have to
// SEND one of these tokens to get a pass, or send the truth and be refused. The
// right fix is for internal/forge to decide whether an actor may be the applier
// and hand gates the answer, as isApplier already does internally -- then this
// map gets one line smaller and nothing new may join it.
var allowedActorTokens = map[string]bool{
	// rulesets.go's isApplier: `a.ActorType == "Integration"`, merged as part
	// of #36 on 2026-09-14. The one live use in this package, measured then:
	// one occurrence across every non-test file.
	"rulesets.go": true,
}

// TestNoNewForgeVocabularyReachesTheGates fails on any quoted GitHub actor
// token in this package outside the allow-list.
//
// The reason it is a grep and not a type is that the coupling being prevented
// is a string crossing a boundary. A named type would only catch the next
// author who bothers to write one; this catches the one who pastes a literal.
func TestNoNewForgeVocabularyReachesTheGates(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		for _, tok := range githubActorTokens {
			if !strings.Contains(string(src), `"`+tok+`"`) {
				continue
			}
			if !allowedActorTokens[name] {
				t.Errorf("%s compares a quoted %q, which is one forge's vocabulary arriving in a gate."+
					" Decide the fact in internal/forge and pass gates the answer; add no exceptions here", name, tok)
			}
		}
	}
}

// TestTheActorVocabularyAllowListIsStillNeeded is the half that makes the
// ratchet real. An allow-list nobody can shrink is a permanent exemption, so
// when isApplier's comparison moves into internal/forge this test goes red and
// whoever did the work comes back and deletes the entry.
func TestTheActorVocabularyAllowListIsStillNeeded(t *testing.T) {
	src, err := os.ReadFile("rulesets.go")
	if err != nil {
		t.Fatalf("reading rulesets.go: %v", err)
	}
	if !strings.Contains(string(src), `a.ActorType == "Integration"`) {
		t.Error(`rulesets.go no longer compares ActorType to "Integration", so allowedActorTokens still lists a file that has been cleaned up -- delete the entry`)
	}
}
