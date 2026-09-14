package gates

import (
	"strings"
	"testing"
)

// refWithRules is a ref whose bypass list was actually READ, carrying the
// given rules and nobody listed to skip them. BypassActorsRead matters as much
// as the empty list: a ruleset this credential could not see is refused, so a
// fixture that left it unset would pass a refusal test for the blind-read
// reason rather than the one the test names.
func refWithRules(rules ...string) Rulesets {
	return Rulesets{Applicable: []Ruleset{{
		ID: 7, Name: "delivery ref", Enforcement: "active", Rules: rules,
		BypassActorsRead: true,
	}}}
}

// TestCheckDeliveryRefRefusesAnAppendOnlyRefThatAnyWriterMayPushTo is the hole
// this branch was created to close. Blocking force pushes and deletion keeps
// history append-only but leaves an ordinary fast-forward open to anyone with
// write access, and a reconciler applies this ref -- so an ungated commit becomes
// a deployed one. Ruled on 2026-09-14 that this is refused rather than accepted
// as a weaker-but-valid configuration, because a green pass here would certify
// push-exclusivity that the ref does not have.
func TestCheckDeliveryRefRefusesAnAppendOnlyRefThatAnyWriterMayPushTo(t *testing.T) {
	got := CheckDeliveryRef("queued", refWithRules(ruleNonFastForward, ruleDeletion), applierApp)
	if len(got) == 0 {
		t.Fatal("a ref any writer could fast-forward onto was accepted")
	}
	joined := strings.Join(got, "; ")
	if !strings.Contains(joined, ruleUpdate) {
		t.Errorf("problems = %q, want it to name the rule to add", joined)
	}
	// The two append-only rules ARE present, so the message must not send the
	// operator off to add them. Both refusals above share the phrase "do not
	// include", which is what makes this assertion able to tell them apart.
	if strings.Contains(joined, "append-only") {
		t.Errorf("problems = %q, want it not to claim the append-only rules are missing when they are set", joined)
	}
}

// TestCheckDeliveryRefRefusesAnUnprotectedRef is the case that matters most.
// An unprotected ref is not a weaker gate, it is a path to production nobody
// is watching, so publishing onto it must be refused rather than allowed with
// a warning.
func TestCheckDeliveryRefRefusesAnUnprotectedRef(t *testing.T) {
	got := CheckDeliveryRef("queued", Rulesets{}, 0)
	if len(got) == 0 {
		t.Fatal("a ref with no ruleset at all was accepted")
	}
	joined := strings.Join(got, "; ")
	if !strings.Contains(joined, "queued") {
		t.Errorf("problems = %q, want the ref named", joined)
	}
	for _, want := range []string{ruleNonFastForward, ruleDeletion} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems = %q, want it to say which rules to add (%s)", joined, want)
		}
	}
	// ⚠️ THE WORDING IS THE POINT, NOT JUST THE REFUSAL. With no ruleset the
	// rule-union check below also fires, so a broken gate still refuses here
	// -- but it would say "the rulesets applying to queued do not include..."
	// about rulesets that do not exist, sending an operator to edit something
	// that is not there. Both outcomes refuse; only one tells the truth.
	if !strings.Contains(joined, "no ruleset applies") {
		t.Errorf("problems = %q, want it to say no ruleset applies at all", joined)
	}
	if strings.Contains(joined, "do not include") {
		t.Errorf("problems = %q, want it not to describe rulesets that do not exist", joined)
	}
}

// deliveryRefMissing is a correctly-exclusive delivery ref with exactly one of
// its three rules removed. Building it by subtraction rather than hand-rolling
// each case keeps the applier listed on the bypass list, so the only refusal in
// play is the missing rule -- which is what lets the test below assert that the
// message names the missing rule and demands nothing that is already there.
func deliveryRefMissing(missing string) Rulesets {
	var present []string
	for _, want := range []string{ruleNonFastForward, ruleDeletion, ruleUpdate} {
		if want != missing {
			present = append(present, want)
		}
	}
	rs := exclusiveDeliveryRef(applierActor("always"))
	rs.Applicable[0].Rules = present
	return rs
}

func TestCheckDeliveryRefNamesTheRuleThatIsMissing(t *testing.T) {
	for _, missing := range []string{ruleNonFastForward, ruleDeletion, ruleUpdate} {
		name := map[string]string{
			ruleNonFastForward: "force pushes still allowed",
			ruleDeletion:       "deletion still allowed",
			ruleUpdate:         "any writer may still push",
		}[missing]
		t.Run(name, func(t *testing.T) {
			got := CheckDeliveryRef("queued", deliveryRefMissing(missing), applierApp)
			joined := strings.Join(got, "; ")
			if len(got) == 0 {
				t.Fatalf("a ref missing %s was accepted", missing)
			}
			if !strings.Contains(joined, missing) {
				t.Errorf("problems = %q, want it to name %s", joined, missing)
			}
			for _, present := range []string{ruleNonFastForward, ruleDeletion, ruleUpdate} {
				if present == missing {
					continue
				}
				if strings.Contains(joined, present) {
					t.Errorf("problems = %q, want it not to demand %s, which is already there", joined, present)
				}
			}
		})
	}
}

// TestCheckDeliveryRefTakesTheUnionAcrossRulesets: rulesets are additive and an
// operator may reasonably split one rule per ruleset. Asking a single ruleset to
// carry all three would refuse a correct configuration, and an operator told to
// fix something that is not broken learns to distrust the message. The applier
// rides on the ruleset that carries `update`, which is the pairing that actually
// has to hold for the push to be accepted.
func TestCheckDeliveryRefTakesTheUnionAcrossRulesets(t *testing.T) {
	rs := Rulesets{Applicable: []Ruleset{
		{ID: 1, Name: "no force push", Enforcement: "active", Rules: []string{ruleNonFastForward}, BypassActorsRead: true},
		{ID: 2, Name: "no deletion", Enforcement: "active", Rules: []string{ruleDeletion}, BypassActorsRead: true},
		{ID: 3, Name: "applier only", Enforcement: "active", Rules: []string{ruleUpdate},
			BypassActorsRead: true, BypassActors: []BypassActor{applierActor("always")}},
	}}
	if got := CheckDeliveryRef("queued", rs, applierApp); len(got) != 0 {
		t.Fatalf("three rulesets that between them protect the ref were refused: %v", got)
	}
}

// TestCheckDeliveryRefInheritsTheRulesetRefusals: an inactive ruleset or one
// anybody can bypass protects nothing, whatever rules it lists.
func TestCheckDeliveryRefInheritsTheRulesetRefusals(t *testing.T) {
	inactive := refWithRules(ruleNonFastForward, ruleDeletion)
	inactive.Applicable[0].Enforcement = "evaluate"
	if got := CheckDeliveryRef("queued", inactive, 0); len(got) == 0 {
		t.Error("a ruleset that is not active was accepted")
	}

	bypassed := refWithRules(ruleNonFastForward, ruleDeletion)
	bypassed.Applicable[0].BypassActors = []BypassActor{{ActorType: "User", BypassMode: "always"}}
	got := CheckDeliveryRef("queued", bypassed, 0)
	if len(got) == 0 {
		t.Fatal("a ruleset with a bypass actor was accepted")
	}
	if !strings.Contains(strings.Join(got, "; "), "User") {
		t.Errorf("problems = %v, want the bypass named", got)
	}
}

// applierApp is the App id these fixtures name as the deployment's own. It is
// deliberately not 0, which is the "the deployment named nobody" case and has a
// test of its own below.
const applierApp = 555

// exclusiveDeliveryRef is a delivery ref carrying all three rules, so the
// `update` rule that decides WHO may push is present, with whatever bypass
// actors the case under test supplies.
func exclusiveDeliveryRef(actors ...BypassActor) Rulesets {
	return Rulesets{Applicable: []Ruleset{{
		ID: 7, Name: "delivery ref", Enforcement: "active",
		Rules:            []string{ruleNonFastForward, ruleDeletion, ruleUpdate},
		BypassActorsRead: true,
		BypassActors:     actors,
	}}}
}

func applierActor(mode string) BypassActor {
	return BypassActor{ActorID: applierApp, ActorType: "Integration", BypassMode: mode}
}

// TestCheckDeliveryRefAcceptsTheApplierAsSoleBypassActor is the configuration
// this whole gate exists to require, and the one the 2026-09-14 probe measured:
// an `update` rule whose only bypass actor is the applier's own App. Refusing it
// would be worse than useless -- truss would block its own push and nothing
// could ever be published.
func TestCheckDeliveryRefAcceptsTheApplierAsSoleBypassActor(t *testing.T) {
	rs := exclusiveDeliveryRef(applierActor("always"))
	if got := CheckDeliveryRef("queued", rs, applierApp); len(got) != 0 {
		t.Fatalf("the required configuration was refused: %v", got)
	}
}

// TestCheckDeliveryRefRefusesASecondApp is the reason BypassActor carries an
// ActorID at all. Before it, a bypass actor was identified by ActorType, which
// says only "an Integration" -- a class, not an identity. A ruleset listing some
// other App would have matched on type and read as exactly the configuration
// above, while handing push-exclusivity to a second App the deployment never
// named. This is the test that would have passed wrongly a week ago.
func TestCheckDeliveryRefRefusesASecondApp(t *testing.T) {
	rs := exclusiveDeliveryRef(
		applierActor("always"),
		BypassActor{ActorID: 999, ActorType: "Integration", BypassMode: "always"},
	)
	got := CheckDeliveryRef("queued", rs, applierApp)
	if len(got) == 0 {
		t.Fatal("a second App on the delivery ref's bypass list was accepted")
	}
	joined := strings.Join(got, "; ")
	if !strings.Contains(joined, "999") {
		t.Errorf("problems = %q, want the foreign actor_id named so an operator knows which to delete", joined)
	}
	if strings.Contains(joined, "555") {
		t.Errorf("problems = %q, want the applier's own App not blamed for a bypass it did not add", joined)
	}
}

// TestCheckDeliveryRefRefusesAnExemptBypassEvenFromTheApplier: bypass_mode
// "exempt" is documented as skipping the rules with no audit entry. The delivery
// ref exists because its history is the audit log, so an unlogged bypass is the
// one mode that defeats the ref's entire purpose -- even when the identity is
// correct.
func TestCheckDeliveryRefRefusesAnExemptBypassEvenFromTheApplier(t *testing.T) {
	rs := exclusiveDeliveryRef(applierActor("exempt"))
	got := CheckDeliveryRef("queued", rs, applierApp)
	if len(got) == 0 {
		t.Fatal("an exempt bypass by the applier's own App was accepted")
	}
	if !strings.Contains(strings.Join(got, "; "), "exempt") {
		t.Errorf("problems = %v, want the mode named as the reason", got)
	}
}

// TestCheckDeliveryRefRefusesAnUpdateRuleThatLocksTheApplierOut covers the
// strictly-more-restricted configuration. An `update` rule with no bypass actor
// is safer than intended, but nothing could ever be published, and the operator
// would learn that only from GitHub's GH013 on the push -- which says nothing
// about which knob to turn. Refusing here turns a confusing runtime failure into
// a startup refusal that names the App to add.
func TestCheckDeliveryRefRefusesAnUpdateRuleThatLocksTheApplierOut(t *testing.T) {
	got := CheckDeliveryRef("queued", exclusiveDeliveryRef(), applierApp)
	if len(got) == 0 {
		t.Fatal("an `update` rule with no bypass actor was accepted")
	}
	joined := strings.Join(got, "; ")
	if !strings.Contains(joined, "555") {
		t.Errorf("problems = %q, want it to name the App id to add", joined)
	}
}

// TestCheckDeliveryRefRefusesWhenTheDeploymentNamesNoApp is the other half: the
// rule is there, but truss cannot tell whether the applier may still advance the
// ref, because it was never told which App it authenticates as. 0 is not a
// wildcard and not an "anything goes" -- it is an unverifiable case, which this
// project refuses rather than guesses at.
func TestCheckDeliveryRefRefusesWhenTheDeploymentNamesNoApp(t *testing.T) {
	got := CheckDeliveryRef("queued", exclusiveDeliveryRef(), 0)
	if len(got) == 0 {
		t.Fatal("an `update` rule was accepted with no applier App configured")
	}
	joined := strings.Join(got, "; ")
	if !strings.Contains(joined, "DELIVERY_BYPASS_ACTOR_ID") {
		t.Errorf("problems = %q, want it to name the variable to set", joined)
	}
}

// TestCheckDeliveryRefRefusesABypassListItCannotRead: a credential that cannot
// see bypass_actors gets the key omitted, which decodes to the same empty list
// as "nobody may skip this". Every refusal above rests on being able to read the
// list, so a blind read must stop the publish rather than report compliance.
func TestCheckDeliveryRefRefusesABypassListItCannotRead(t *testing.T) {
	rs := exclusiveDeliveryRef(applierActor("always"))
	rs.Applicable[0].BypassActorsRead = false
	got := CheckDeliveryRef("queued", rs, applierApp)
	if len(got) == 0 {
		t.Fatal("a ruleset whose bypass list could not be read was accepted as compliant")
	}
	if !strings.Contains(strings.Join(got, "; "), "bypass_actors") {
		t.Errorf("problems = %v, want the unreadable bypass list named", got)
	}
}
