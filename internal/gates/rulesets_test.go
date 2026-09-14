package gates

import "testing"

// compliantRuleset is a Ruleset that clears CheckRulesets's bar: active,
// carrying no bypass actor, and read by a credential that could actually see
// the bypass list. The last of the three is what makes "no bypass actor" mean
// "nobody may skip this" rather than "we were not told".
func compliantRuleset(id int, name string) Ruleset {
	return Ruleset{ID: id, Name: name, Enforcement: "active", BypassActorsRead: true}
}

// TestCheckRulesetsAcceptsNoRulesets: a branch with no ruleset at all is not
// a hole -- classic branch protection is a separate control, gated by
// CheckProtection, and docs/work-items.md's own ruling is that rulesets are
// additive on top of it. This is the one input where the ZERO VALUE of
// Rulesets is compliant, unlike every other gate in this package: an empty
// Applicable is a fact, not an absence, because forge.Rulesets returns an
// error rather than a zero value when the read itself failed (see
// gates.Rulesets's own doc comment). So this test is deliberately not part
// of TestEveryGateRefusesTheZeroValue in gates_test.go.
func TestCheckRulesetsAcceptsNoRulesets(t *testing.T) {
	if problems := CheckRulesets(Rulesets{}); len(problems) != 0 {
		t.Fatalf("an empty Rulesets was refused: %v", problems)
	}
}

// TestCheckRulesetsAcceptsAnActiveRulesetWithNoBypassActor: an active
// ruleset that nobody can skip is exactly what closing this hole requires,
// and must not itself be treated as suspicious.
func TestCheckRulesetsAcceptsAnActiveRulesetWithNoBypassActor(t *testing.T) {
	rs := Rulesets{Applicable: []Ruleset{compliantRuleset(1, "require a pull request")}}
	if problems := CheckRulesets(rs); len(problems) != 0 {
		t.Fatalf("a compliant ruleset was refused: %v", problems)
	}
}

// TestCheckRulesetsRefusesANonEmptyBypassActors is the actual hole this gate
// exists to close, reproduced from docs/work-items.md's own evidence: a
// deploy key was a bypass actor on the ruleset carrying "changes must be
// made through a pull request", and that push to this repository's own main
// on 2026-09-09 was accepted while classic branch protection read
// compliant. CheckProtection cannot see this at all; only this gate can.
func TestCheckRulesetsRefusesANonEmptyBypassActors(t *testing.T) {
	rs := Rulesets{Applicable: []Ruleset{{
		ID:               7,
		Name:             "require a pull request",
		Enforcement:      "active",
		BypassActorsRead: true,
		BypassActors: []BypassActor{
			{ActorType: "DeployKey", BypassMode: "always"},
		},
	}}}
	problems := CheckRulesets(rs)
	if !hasProblemContaining(problems, `ruleset "require a pull request" (id 7)`) {
		t.Fatalf("problems %v do not name the ruleset", problems)
	}
	if !hasProblemContaining(problems, "DeployKey") {
		t.Fatalf("problems %v do not name the bypass actor's type", problems)
	}
}

// TestCheckRulesetsNamesEveryDistinctActorType: an operator who goes to look
// needs to know every kind of actor that can skip the rule, not just the
// first one found.
func TestCheckRulesetsNamesEveryDistinctActorType(t *testing.T) {
	rs := Rulesets{Applicable: []Ruleset{{
		ID:               7,
		Name:             "require a pull request",
		Enforcement:      "active",
		BypassActorsRead: true,
		BypassActors: []BypassActor{
			{ActorType: "DeployKey", BypassMode: "always"},
			{ActorType: "Team", BypassMode: "pull_request"},
		},
	}}}
	problems := CheckRulesets(rs)
	if !hasProblemContaining(problems, "DeployKey") || !hasProblemContaining(problems, "Team") {
		t.Fatalf("problems %v do not name both bypass actor types", problems)
	}
}

// TestCheckRulesetsRefusesABypassListThatWasNotRead is the same claim as the
// delivery-ref gate's, on the branch that actually was bypassed on 2026-09-09.
// A credential that cannot see bypass_actors decodes to the same empty list as a
// ruleset that has none, so without this refusal CheckRulesets would report a ref
// nobody can inspect as a ref nobody can bypass -- the one reading of "no
// problems" that is not evidence of anything.
func TestCheckRulesetsRefusesABypassListThatWasNotRead(t *testing.T) {
	blind := compliantRuleset(7, "require a pull request")
	blind.BypassActorsRead = false
	problems := CheckRulesets(Rulesets{Applicable: []Ruleset{blind}})
	if len(problems) == 0 {
		t.Fatal("a ruleset whose bypass list could not be read was reported as compliant")
	}
	if !hasProblemContaining(problems, "bypass_actors") {
		t.Fatalf("problems %v do not name the unreadable bypass list as the reason", problems)
	}
}

// TestCheckRulesetsRefusesWhenTheSecondReadDisagreesOnEnforcement:
// rules/branches/{branch} only ever returns a rule from a ruleset whose
// enforcement was "active" at that moment (GitHub's own documented
// behaviour for that endpoint), so a Ruleset reaching this gate at all is
// proof it was active moments earlier. If the second read (the one that
// supplies bypass_actors) now disagrees, that is not "additive and inert" --
// it is exactly what an actor flipping enforcement off around the query
// window to dodge this gate's own bypass_actors read would produce, so it
// is refused rather than silently trusted.
func TestCheckRulesetsRefusesWhenTheSecondReadDisagreesOnEnforcement(t *testing.T) {
	for _, enforcement := range []string{"evaluate", "disabled", ""} {
		t.Run(enforcement, func(t *testing.T) {
			rs := Rulesets{Applicable: []Ruleset{{ID: 3, Name: "r", Enforcement: enforcement, BypassActorsRead: true}}}
			problems := CheckRulesets(rs)
			if !hasProblemContaining(problems, "no longer reads as active") {
				t.Fatalf("an applicable ruleset reading back as %q was not refused: %v", enforcement, problems)
			}
		})
	}
}

// TestCheckRulesetsChecksEveryApplicableRuleset: a compliant ruleset
// alongside a non-compliant one must not let the non-compliant one hide.
func TestCheckRulesetsChecksEveryApplicableRuleset(t *testing.T) {
	rs := Rulesets{Applicable: []Ruleset{
		compliantRuleset(1, "fine"),
		{ID: 2, Name: "not fine", Enforcement: "active", BypassActorsRead: true, BypassActors: []BypassActor{{ActorType: "Team", BypassMode: "always"}}},
	}}
	problems := CheckRulesets(rs)
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want exactly one (the non-compliant ruleset)", problems)
	}
	if !hasProblemContaining(problems, "not fine") {
		t.Fatalf("problems %v do not name the non-compliant ruleset", problems)
	}
}
