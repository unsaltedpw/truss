package gates

import (
	"fmt"
	"strings"
)

// Ruleset is one repository ruleset the forge reported as applying to the
// branch being applied. It is assembled from two reads, because GitHub
// splits the two facts this gate needs across two endpoints:
// GET /repos/{o}/{r}/rules/branches/{branch} says WHICH rulesets apply,
// already flattened across org and repo rulesets, but that endpoint's own
// documentation is explicit that it never carries bypass_actors; only
// GET /repos/{o}/{r}/rulesets/{id} does. See forge.Rulesets for the join.
//
// ⚠️ THE HOLE THIS EXISTS TO CLOSE IS NOT THEORETICAL. On 2026-09-09, four
// commits were pushed straight to this repository's own main with an SSH
// deploy key, and GitHub ACCEPTED the push, answering:
//
//	remote: Bypassed rule violations for refs/heads/main:
//	remote: - Changes must be made through a pull request.
//	remote: - Required status check "check" is expected.
//
// "Bypassed rule violations" is ruleset language, not classic
// branch-protection language -- classic protection declines with a
// protected-branch hook error and no push happens at all. So the
// pull-request requirement lived in a ruleset, and the pushing identity was
// a bypass actor on it. forge.Protection reads only
// branches/{branch}/protection, so CheckProtection returned no problems
// while this was true. Recorded in docs/work-items.md.
type Ruleset struct {
	ID   int
	Name string

	// Enforcement is what the SECOND read (rulesets/{id}) reports, taken
	// fresh rather than assumed from membership in the first read's list.
	//
	// ⚠️ A Ruleset REACHING THIS TYPE AT ALL IS ALREADY PROOF IT WAS ACTIVE.
	// GET /rules/branches/{branch}'s own documentation: "Rules in rulesets
	// with 'evaluate' or 'disabled' enforcement statuses are not returned."
	// So the only way a ruleset ends up in Rulesets.Applicable is that, at
	// the moment of the FIRST read, it was enforcement=active and carried
	// at least one rule targeting this branch. See CheckRulesets for what
	// it means when the second read disagrees.
	Enforcement string

	BypassActors []BypassActor

	// Rules is the rule types this ruleset contributes to the ref in
	// question -- "deletion", "non_fast_forward", "pull_request" and so on,
	// in GitHub's own vocabulary rather than a normalised one. CheckRulesets
	// ignores it; CheckDeliveryRef is what reads it.
	//
	// ⚠️ IT IS THE RULES THAT APPLY TO ONE REF, NOT EVERY RULE THE RULESET
	// DECLARES. The read that produces it asks which rules apply to a
	// branch, so a ruleset whose conditions exclude this ref contributes
	// nothing here even though it exists. That is the question a gate about
	// one ref actually wants, and asking the other one would let a rule
	// protecting some other branch read as protecting this one.
	Rules []string
}

// BypassActor is one actor permitted to skip a ruleset's rules, from
// rulesets/{id}'s bypass_actors[]. ActorType is one of RepositoryRole,
// Team, Integration, OrganizationAdmin, DeployKey or User; BypassMode is
// "always" (skips everything), "pull_request" (skips only on a PR merge)
// or "exempt" (skipped silently, no audit entry) -- GitHub's documented
// values, verified against the REST reference rather than assumed.
//
// ActorID is read from the same object: "The ID of the actor that can
// bypass a ruleset. Required for Integration, RepositoryRole, Team, and
// User actor types. If actor_type is OrganizationAdmin, actor_id is
// ignored. If actor_type is DeployKey, this should be null." -- the
// repository-ruleset-bypass-actor schema, GitHub's REST API description,
// verified 2026-09-13.
//
// ⚠️ FOR ActorType "Integration" THIS IS THE APP'S OWN ID, NOT AN
// INSTALLATION ID. The same API description, one schema over, documents a
// SEPARATE actor-type vocabulary for a pull_request rule's
// dismissal_restriction.allowed_actors[].type: "User", "Team",
// "IntegrationInstallation" or "RepositoryRole" -- where GitHub means an
// installation, it says so with a distinct string. bypass_actors says
// plain "Integration", so ActorID here is the App's id: the same value
// truss's own `github-app`/`app_id` credential holds and already sends as
// the JWT `iss` claim (internal/forge/client.go). Nothing in actor_id's own
// prose disambiguates this; the two sibling enums living in one schema do.
type BypassActor struct {
	ActorType  string
	ActorID    int64
	BypassMode string
}

// Rulesets is every ruleset the forge reported as applying to the branch
// being applied.
//
// ⚠️ AN EMPTY Applicable IS A FACT, NOT AN ABSENCE. forge.Rulesets returns
// an error -- never a zero-valued Rulesets -- when the read failed, the
// same contract forge.Protection already keeps; the caller (runApplyPass)
// never has a Rulesets to hand CheckRulesets unless the read actually
// succeeded, so "unreadable" is its own case there, exactly the discipline
// every other gate in this file is built on. A Rulesets with a nil or
// empty Applicable, by contrast, means this branch has no ruleset at all,
// which is not itself a problem: classic branch protection is a separate
// control CheckProtection already gates, and the two are read together (see
// apply_cmd.go) rather than one relaxing the other.
type Rulesets struct {
	Applicable []Ruleset
}

// CheckRulesets refuses a bypass actor on any ruleset that applies to the
// branch being applied, and a ruleset whose second read no longer agrees
// that it is active.
//
// ⚠️ THE BYPASS-ACTOR CHECK IS THE ACTUAL HOLE, NOT A THEORETICAL ONE --
// see Ruleset's own doc comment for the push that proved it. Turning off
// classic branch protection makes CheckProtection refuse everything,
// loudly; naming a bypass actor on a ruleset changes nothing
// CheckProtection can see, which is what makes it worth a gate of its own.
//
// A ruleset in "evaluate" or "disabled" enforcement is NOT refused merely
// for existing. docs/work-items.md's ruling on rulesets stands for that
// case: a rule nobody enforces protects nothing, which is the same fact as
// no ruleset at all, and classic protection is gated separately -- so a
// non-enforcing ruleset is not automatically a hole. What IS refused is a
// ruleset that reads back as anything other than "active" HERE, in the
// second read, after membership in Applicable already proved it was active
// moments earlier (Ruleset.Enforcement's own doc comment). That is not
// "additive and inert": it is two reads of the same fact disagreeing, and
// the disagreeing direction is exactly what an actor would produce by
// disabling a ruleset around the query window to dodge the very
// bypass_actors read this gate depends on. Fail closed rather than decide
// which story is true.
func CheckRulesets(rs Rulesets) []string {
	var problems []string
	for _, r := range rs.Applicable {
		problems = append(problems, checkEnforcement(r)...)
		if len(r.BypassActors) == 0 {
			continue
		}
		var who []string
		seen := map[string]bool{}
		for _, a := range r.BypassActors {
			label := fmt.Sprintf("%s (%s)", a.ActorType, a.BypassMode)
			if seen[label] {
				continue
			}
			seen[label] = true
			who = append(who, label)
		}
		problems = append(problems, fmt.Sprintf(
			"ruleset %q (id %d) has a non-empty bypass_actors (%s): a named actor can skip its rules entirely -- "+
				"this is how a push straight to main was accepted on 2026-09-09 despite branch protection reading compliant",
			r.Name, r.ID, strings.Join(who, ", ")))
	}
	return problems
}

// checkEnforcement refuses a ruleset whose second read (rulesets/{id}) no
// longer agrees that it is active, for the reason given on Ruleset's own
// Enforcement field: reaching this gate at all is proof the first read saw
// "active" moments earlier, so a second read disagreeing is a race or an
// attempt to dodge the bypass_actors read, never "additive and inert".
// Shared by CheckRulesets and CheckDeliveryRef, which otherwise disagree
// completely about what bypass_actors is allowed to contain.
func checkEnforcement(r Ruleset) []string {
	if r.Enforcement == "active" {
		return nil
	}
	return []string{fmt.Sprintf(
		"ruleset %q (id %d) applies to this branch but no longer reads as active (now %q): "+
			"the two reads of it disagree, which this gate treats as a race or an attempt to dodge it",
		r.Name, r.ID, r.Enforcement)}
}
