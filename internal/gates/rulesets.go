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

	// BypassActorsRead records whether rulesets/{id} carried a bypass_actors
	// KEY at all. Absent is not the same as empty: measured 2026-09-14, a token
	// without Administration received no key for a ruleset that did name an App,
	// while the same credential was told current_user_can_bypass "always". So
	// false means BLIND, never "nobody may skip these rules", and both gates
	// refuse on it -- see rulesetReadProblems.
	BypassActorsRead bool

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
type BypassActor struct {
	// ActorID is GitHub's numeric actor id. It is what tells one App from
	// another: ActorType alone says "an Integration", which names a CLASS of
	// credential rather than the single one a deployment named, so a bypass list
	// holding some other App would still read as compliant without it.
	ActorID int

	ActorType  string
	BypassMode string
}

// isApplier reports whether this actor is the App the deployment named.
//
// applierAppID 0 means the deployment named nobody, which answers false rather
// than acting as a wildcard -- the absence of a configured App is not a licence
// for every App.
//
// BypassMode "exempt" is refused even on an id match, because GitHub documents
// it as skipping the rules with no audit entry. An unlogged bypass on the ref
// between "reviewed" and "running" is worse than a logged one: the audit log is
// the entire reason that ref exists.
func (a BypassActor) isApplier(applierAppID int) bool {
	if applierAppID == 0 || a.ActorID != applierAppID {
		return false
	}
	return a.ActorType == "Integration" && a.BypassMode != "exempt"
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
	problems := rulesetReadProblems(rs)
	for _, r := range rs.Applicable {
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

// rulesetReadProblems is what every ruleset gate has to believe before it can
// reason about contents: the two reads agree on enforcement, and the bypass
// list was actually read.
//
// The second one has no effect on the verdict a naive reader would expect,
// which is exactly why it is refused. A credential that cannot see
// bypass_actors produces the same empty list as a ruleset that has none, and
// the difference between those two is the difference between "protected" and
// "unauditable". CheckRulesets would otherwise conclude that a ref nobody can
// inspect is a ref nobody can bypass.
func rulesetReadProblems(rs Rulesets) []string {
	var problems []string
	for _, r := range rs.Applicable {
		if r.Enforcement != "active" {
			problems = append(problems, fmt.Sprintf(
				"ruleset %q (id %d) applies to this branch but no longer reads as active (now %q): "+
					"the two reads of it disagree, which this gate treats as a race or an attempt to dodge it",
				r.Name, r.ID, r.Enforcement))
		}
		if !r.BypassActorsRead {
			problems = append(problems, fmt.Sprintf(
				"ruleset %q (id %d) returned no bypass_actors at all, so this credential cannot see who may skip its "+
					"rules: reading it as an empty list would turn a blind read into a clean verdict -- measured "+
					"2026-09-14, an App token without Administration omits the key even when a bypass actor is set",
				r.Name, r.ID))
		}
	}
	return problems
}
