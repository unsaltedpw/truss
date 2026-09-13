package gates

import (
	"fmt"
	"strings"
)

// Rule types this gate requires, in GitHub's own vocabulary rather than a
// normalised one. Read from the live API's published schema on 2026-09-10;
// the effective-rules endpoint reports the rules that apply to one ref, which
// is the question a gate about one ref wants asked.
//
// ruleUpdate was added 2026-09-13: it is GitHub's "Restrict updates" rule --
// "Only allow users with bypass permission to update matching refs" -- and is
// what makes an exact, single App bypass actor mean something. Without it,
// non_fast_forward and deletion alone leave an ordinary fast-forward push
// open to anyone with write access regardless of bypass_actors, which is the
// gap docs/work-items.md recorded as "never measured".
const (
	ruleUpdate         = "update"
	ruleNonFastForward = "non_fast_forward"
	ruleDeletion       = "deletion"
)

// bypassModeAlways is the one bypass_mode this gate accepts on the delivery
// ref's App bypass actor. GitHub's documented enum (repository-ruleset-
// bypass-actor schema, GitHub's REST API description, verified 2026-09-13)
// also has "pull_request" (skip only on a PR merge -- meaningless for a ref
// nothing ever merges a pull request onto) and "exempt" (skipped with no
// audit entry); either would let the applier's push through without being
// the deliberate, auditable exception "always" is.
const bypassModeAlways = "always"

// actorTypeIntegration is GitHub's bypass_actors[].actor_type string for a
// GitHub App, verified against the repository-ruleset-bypass-actor schema
// (GitHub's REST API description, 2026-09-13; actor_type enum: Integration,
// OrganizationAdmin, RepositoryRole, Team, DeployKey, User). See
// BypassActor.ActorID's own doc comment for why its value is the App's own
// id rather than an installation id.
const actorTypeIntegration = "Integration"

// CheckDeliveryRef refuses to publish a commit onto the ref a reconciler
// tracks unless that ref's history is append-only, and unless the only party
// who can move it at all is the applier's own GitHub App.
//
// The applier advances this ref only after it has gated a commit and matched
// every render against the digest CI filed. That makes the ref the boundary
// between "reviewed" and "running", and the property it has to keep is the
// one main keeps: history is the audit log. A force push would rewrite what
// a cluster was told to apply; a deletion would erase it; and an ordinary
// fast-forward push from anyone else would move it to a commit approval
// never touched.
//
// ⚠️ MAIN AND THE DELIVERY REF WANT OPPOSITE ANSWERS ON bypass_actors, WHICH
// IS WHY THIS DOES NOT CALL CheckRulesets. On main, any bypass actor at all
// is the hole (CheckRulesets's own doc comment, and the 2026-09-09 push that
// proved it): approval is supposed to be the only door, so a named actor who
// can skip it is a second door nobody watches. On the delivery ref, approval
// already happened upstream of `main`; what CheckDeliveryRef defends is that
// the ref only ever advances to what the applier gated, and the only way to
// make an `update` rule (below) mean that is to name the applier's own App as
// its exact, sole bypass actor with bypass_mode "always" -- anything else,
// including naming NO bypass actor at all, would leave `update` blocking
// everyone (including the applier) or blocking no one who matters.
//
// The deployment this was built for accepts that push-exclusivity was, until
// 2026-09-13, held by who has write access rather than by a gate:
// docs/threat-model.md places the approver's own accounts out of scope, so on
// a repository whose only writers were the approver and the applier that was
// a real if unmeasured property. It stopped being one the moment a second
// writer (a dev box's deploy key, or any future CI job) got write access to
// the platform repository, and nothing noticed when it did -- which is the
// gap this closes. docs/work-items.md carries the history.
//
// A ref nothing protects is refused rather than published to: an unprotected
// ref is not a weaker gate, it is a path to production nobody is watching.
func CheckDeliveryRef(ref string, rs Rulesets, appID int64) []string {
	var problems []string
	for _, r := range rs.Applicable {
		problems = append(problems, checkEnforcement(r)...)
		problems = append(problems, checkDeliveryRefBypass(ref, r, appID)...)
	}

	if len(rs.Applicable) == 0 {
		return append(problems, fmt.Sprintf(
			"no ruleset applies to %s: refusing to publish onto a ref whose history nothing protects -- "+
				"create one targeting this ref with the %q, %q and %q rules",
			ref, ruleUpdate, ruleNonFastForward, ruleDeletion))
	}

	// The union across every applicable ruleset, because two rulesets may
	// each contribute one of the rules and between them protect the ref.
	// Asking each ruleset to carry all three would refuse a correct
	// configuration.
	have := map[string]bool{}
	for _, r := range rs.Applicable {
		for _, t := range r.Rules {
			have[t] = true
		}
	}

	// Built from a fixed-order slice, so the message is deterministic without
	// sorting -- internal/gates may not import sort, and does not need to.
	var missing []string
	for _, want := range []string{ruleUpdate, ruleNonFastForward, ruleDeletion} {
		if !have[want] {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		problems = append(problems, fmt.Sprintf(
			"the rulesets applying to %s do not include %s: history on this ref is what a cluster was told to apply, "+
				"so it must be append-only and only the applier may move it -- add %s to a ruleset targeting it",
			ref, strings.Join(missing, " and "), strings.Join(missing, " and ")))
	}
	return problems
}

// checkDeliveryRefBypass refuses a ruleset applying to the delivery ref
// unless its bypass_actors is either empty, or exactly one entry naming the
// applier's own App (actorTypeIntegration, ActorID == appID) with
// bypass_mode "always". That single, exact entry is the restriction itself:
// it is what lets an `update` rule mean "only the applier may move this ref"
// rather than "nobody may" or "anyone named here may". Anything looser --
// a second actor alongside it, a different app's id, any non-Integration
// actor type, or a bypass_mode other than "always" -- lets a party the
// commit gate never approved move a ref a reconciler applies from, so it is
// refused and named the same way CheckRulesets names one.
func checkDeliveryRefBypass(ref string, r Ruleset, appID int64) []string {
	if len(r.BypassActors) == 0 {
		return nil
	}
	if len(r.BypassActors) == 1 {
		a := r.BypassActors[0]
		if a.ActorType == actorTypeIntegration && a.ActorID == appID && a.BypassMode == bypassModeAlways {
			return nil
		}
	}
	var who []string
	for _, a := range r.BypassActors {
		who = append(who, fmt.Sprintf("%s id %d (%s)", a.ActorType, a.ActorID, a.BypassMode))
	}
	return []string{fmt.Sprintf(
		"ruleset %q (id %d) applies to %s but its bypass_actors is not exactly one entry naming this applier's own App "+
			"(%s id %d, bypass_mode %q): got %s -- on the delivery ref a single, exact App bypass is the restriction "+
			"itself, so anything else lets a party the commit gate never approved move %s",
		r.Name, r.ID, ref, actorTypeIntegration, appID, bypassModeAlways, strings.Join(who, ", "), ref)}
}
