package gates

import (
	"strings"
	"testing"
)

// testAppID stands in for the applier's own App id. Its value is opaque to
// the gate -- what matters is that it is the one CheckDeliveryRef is told to
// compare against, and that a different number is a different App.
const testAppID = 4922051

func protectedRuleset(rules ...string) Rulesets {
	return Rulesets{Applicable: []Ruleset{{
		ID: 7, Name: "delivery ref", Enforcement: "active", Rules: rules,
	}}}
}

func TestCheckDeliveryRefAcceptsAnAppendOnlyRef(t *testing.T) {
	rs := protectedRuleset(ruleUpdate, ruleNonFastForward, ruleDeletion)
	if got := CheckDeliveryRef("queued", rs, testAppID); len(got) != 0 {
		t.Fatalf("a protected ref was refused: %v", got)
	}
}

// TestCheckDeliveryRefRefusesAnUnprotectedRef is the case that matters most.
// An unprotected ref is not a weaker gate, it is a path to production nobody
// is watching, so publishing onto it must be refused rather than allowed with
// a warning.
func TestCheckDeliveryRefRefusesAnUnprotectedRef(t *testing.T) {
	got := CheckDeliveryRef("queued", Rulesets{}, testAppID)
	if len(got) == 0 {
		t.Fatal("a ref with no ruleset at all was accepted")
	}
	joined := strings.Join(got, "; ")
	if !strings.Contains(joined, "queued") {
		t.Errorf("problems = %q, want the ref named", joined)
	}
	for _, want := range []string{ruleUpdate, ruleNonFastForward, ruleDeletion} {
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

// TestCheckDeliveryRefNamesTheRuleThatIsMissing covers all three rules this
// gate now requires: update (added 2026-09-13, so an exact App bypass means
// something) alongside the original non_fast_forward and deletion.
func TestCheckDeliveryRefNamesTheRuleThatIsMissing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		present []string
		missing string
	}{
		{"update missing", []string{ruleNonFastForward, ruleDeletion}, ruleUpdate},
		{"force pushes still allowed", []string{ruleUpdate, ruleDeletion}, ruleNonFastForward},
		{"deletion still allowed", []string{ruleUpdate, ruleNonFastForward}, ruleDeletion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckDeliveryRef("queued", protectedRuleset(tc.present...), testAppID)
			joined := strings.Join(got, "; ")
			if len(got) == 0 {
				t.Fatalf("a ref missing %s was accepted", tc.missing)
			}
			if !strings.Contains(joined, tc.missing) {
				t.Errorf("problems = %q, want it to name %s", joined, tc.missing)
			}
			for _, present := range tc.present {
				if strings.Contains(joined, present) {
					t.Errorf("problems = %q, want it not to demand %s, which is already there", joined, present)
				}
			}
		})
	}
}

// TestCheckDeliveryRefTakesTheUnionAcrossRulesets: three rulesets may each
// contribute one rule and between them protect the ref. Asking a single
// ruleset to carry all three would refuse a correct configuration, and an
// operator told to fix something that is not broken learns to distrust the
// message.
func TestCheckDeliveryRefTakesTheUnionAcrossRulesets(t *testing.T) {
	rs := Rulesets{Applicable: []Ruleset{
		{ID: 1, Name: "restrict updates", Enforcement: "active", Rules: []string{ruleUpdate}},
		{ID: 2, Name: "no force push", Enforcement: "active", Rules: []string{ruleNonFastForward}},
		{ID: 3, Name: "no deletion", Enforcement: "active", Rules: []string{ruleDeletion}},
	}}
	if got := CheckDeliveryRef("queued", rs, testAppID); len(got) != 0 {
		t.Fatalf("three rulesets that between them protect the ref were refused: %v", got)
	}
}

// TestCheckDeliveryRefInheritsTheEnforcementRefusal: an inactive ruleset
// protects nothing, whatever rules it lists -- the same fact CheckRulesets
// establishes for main, reused here via checkEnforcement.
func TestCheckDeliveryRefInheritsTheEnforcementRefusal(t *testing.T) {
	inactive := protectedRuleset(ruleUpdate, ruleNonFastForward, ruleDeletion)
	inactive.Applicable[0].Enforcement = "evaluate"
	if got := CheckDeliveryRef("queued", inactive, testAppID); len(got) == 0 {
		t.Error("a ruleset that is not active was accepted")
	}
}

// appBypassRuleset is a ruleset carrying all three required rules, so only
// bypass_actors is under test.
func appBypassRuleset(actors ...BypassActor) Rulesets {
	rs := protectedRuleset(ruleUpdate, ruleNonFastForward, ruleDeletion)
	rs.Applicable[0].BypassActors = actors
	return rs
}

// TestCheckDeliveryRefAcceptsTheApplierAppAsTheSoleBypassActor is the
// accepted shape: on the delivery ref, unlike main, a bypass actor is not
// automatically a hole -- naming the applier's own App, alone, with
// bypass_mode "always" IS the restriction an `update` rule needs to mean
// "only the applier may move this ref".
func TestCheckDeliveryRefAcceptsTheApplierAppAsTheSoleBypassActor(t *testing.T) {
	rs := appBypassRuleset(BypassActor{ActorType: actorTypeIntegration, ActorID: testAppID, BypassMode: bypassModeAlways})
	if got := CheckDeliveryRef("queued", rs, testAppID); len(got) != 0 {
		t.Fatalf("the applier's own App, alone, with bypass_mode always, was refused: %v", got)
	}
}

// TestCheckDeliveryRefAcceptsNoBypassActorAtAll: an `update` rule with an
// empty bypass_actors blocks every push, including the applier's own -- not
// the shape this gate exists to require, but not a hole either, so it must
// not be refused here. (Whether that configuration lets the applier publish
// at all is a fact the applier discovers at push time, not this gate's
// question.)
func TestCheckDeliveryRefAcceptsNoBypassActorAtAll(t *testing.T) {
	rs := appBypassRuleset()
	if got := CheckDeliveryRef("queued", rs, testAppID); len(got) != 0 {
		t.Fatalf("a ruleset with no bypass actor at all was refused: %v", got)
	}
}

// TestCheckDeliveryRefRefusesEveryOtherBypassShape is the refusal side of the
// same rule: anything other than exactly one Integration entry naming this
// App with bypass_mode "always" lets a party the commit gate never approved
// move the ref, and must be refused by name.
func TestCheckDeliveryRefRefusesEveryOtherBypassShape(t *testing.T) {
	correctApp := BypassActor{ActorType: actorTypeIntegration, ActorID: testAppID, BypassMode: bypassModeAlways}
	for _, tc := range []struct {
		name   string
		actors []BypassActor
	}{
		{"a second actor alongside the App", []BypassActor{
			correctApp,
			{ActorType: "Team", ActorID: 1, BypassMode: bypassModeAlways},
		}},
		{"two Apps, neither alone", []BypassActor{correctApp, correctApp}},
		{"a different App's id", []BypassActor{
			{ActorType: actorTypeIntegration, ActorID: testAppID + 1, BypassMode: bypassModeAlways},
		}},
		{"a User actor", []BypassActor{{ActorType: "User", ActorID: testAppID, BypassMode: bypassModeAlways}}},
		{"a Team actor", []BypassActor{{ActorType: "Team", ActorID: testAppID, BypassMode: bypassModeAlways}}},
		{"a RepositoryRole actor", []BypassActor{{ActorType: "RepositoryRole", ActorID: testAppID, BypassMode: bypassModeAlways}}},
		{"an OrganizationAdmin actor", []BypassActor{{ActorType: "OrganizationAdmin", BypassMode: bypassModeAlways}}},
		{"a DeployKey actor", []BypassActor{{ActorType: "DeployKey", BypassMode: bypassModeAlways}}},
		{"pull_request bypass_mode", []BypassActor{{ActorType: actorTypeIntegration, ActorID: testAppID, BypassMode: "pull_request"}}},
		{"exempt bypass_mode", []BypassActor{{ActorType: actorTypeIntegration, ActorID: testAppID, BypassMode: "exempt"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := appBypassRuleset(tc.actors...)
			got := CheckDeliveryRef("queued", rs, testAppID)
			if len(got) == 0 {
				t.Fatalf("%s was accepted", tc.name)
			}
			joined := strings.Join(got, "; ")
			if !strings.Contains(joined, "delivery ref") {
				t.Errorf("problems = %q, want the ruleset named", joined)
			}
		})
	}
}
