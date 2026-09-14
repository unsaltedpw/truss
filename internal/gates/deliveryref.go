package gates

import (
	"fmt"
	"strings"
)

// Rule types this gate requires, in GitHub's own vocabulary rather than a
// normalised one. Read from the live API's published schema on 2026-09-10;
// the effective-rules endpoint reports the rules that apply to one ref, which
// is the question a gate about one ref wants asked.
const (
	ruleNonFastForward = "non_fast_forward"
	ruleDeletion       = "deletion"
	// ruleUpdate is restrict-updates, the only one of the three that decides
	// WHO may push rather than what shape a push may take. It is therefore the
	// rule that carries push-exclusivity, and the reason it is handled apart
	// from the append-only pair below.
	ruleUpdate = "update"
)

// CheckDeliveryRef refuses to publish a commit onto the ref a reconciler
// tracks unless that ref is append-only AND restricted to this deployment's own
// App. Both are required: the first keeps history the record of what a cluster
// was told to apply, the second keeps it the record of what was *gated*.
//
// The applier advances this ref only after it has gated a commit and matched
// every render against the digest CI filed. That makes the ref the boundary
// between "reviewed" and "running", and the property it has to keep is the
// one main keeps: history is the audit log. A force push would rewrite what a
// cluster was told to apply; a deletion would erase it.
//
// applierAppID is the GitHub App id of the identity allowed to bypass this
// ref's rules -- the applier's own App. Zero means the deployment named none,
// which is its own case below and never a wildcard.
//
// # WHAT AN `update` RULE ACTUALLY DOES, MEASURED 2026-09-14
//
// This file used to say push-exclusivity "could not be measured here" and stop
// there. It has now been measured, on a throwaway ruleset in the deployment's
// own repository, over a branch matching the pattern `probe-*`:
//
//	rules ["non_fast_forward","update"], no bypass actor:
//	  deploy key with contents:write, git push an update
//	    -> GH013 "Cannot update this protected ref.", push declined
//	  GitHub App installation token, PUT /repos/.../contents/...
//	    -> 409 "Cannot update this protected ref."
//	  same App, same call, once named in bypass_actors:
//	    -> accepted; commit e245eab, authored by that App
//
// So: the rule does refuse writers, a credential's write scope does not get it
// through, and being listed is the exact and only thing that does. An `update`
// rule whose sole bypass actor is the applier's App restricts the ref to the
// applier. That is no longer an assumption.
//
// Two edges of the same probe changed what is claimed here:
//
//   - `update` does NOT gate CREATION. The deploy key created the probe branch
//     freely and was refused only on the second push. So the bootstrap the
//     owner chose on 2026-09-13 still works with this rule required: the
//     approver opens the delivery ref by hand, from an identity that is not a
//     bypass actor, and nothing about that first push is gated. What the rule
//     then takes away is every push after it. Requiring `update` therefore
//     costs a deployment one ruleset edit before its first publish, not a
//     chicken-and-egg it cannot get out of.
//   - The bypass list can be READ-BACK BLIND rather than empty. With an App
//     token lacking Administration, a ruleset that did have an App listed
//     returned no `bypass_actors` key at all while reporting
//     current_user_can_bypass "always". Absent therefore means "this
//     credential cannot see who may skip these rules", never "nobody may", and
//     rulesetReadProblems refuses on it. The applier reads as an App that holds
//     Administration, so the normal path has the field; narrow that credential
//     and every publish is refused, which is the direction this project picks
//     on purpose -- a blind read on a path to production is not evidence of
//     safety.
//
// A ref nothing protects is refused rather than published to: an unprotected
// ref is not a weaker gate, it is a path to production nobody is watching.
func CheckDeliveryRef(ref string, rs Rulesets, applierAppID int) []string {
	problems := rulesetReadProblems(rs)

	if len(rs.Applicable) == 0 {
		return append(problems, fmt.Sprintf(
			"no ruleset applies to %s: refusing to publish onto a ref whose history nothing protects -- "+
				"create one targeting this ref with the %q and %q rules", ref, ruleNonFastForward, ruleDeletion))
	}

	// The union across every applicable ruleset, because two rulesets may
	// each contribute one of the rules and between them protect the ref.
	// Asking each ruleset to carry both would refuse a correct configuration.
	have := map[string]bool{}
	for _, r := range rs.Applicable {
		for _, t := range r.Rules {
			have[t] = true
		}
	}

	// Built from a fixed-order slice, so the message is deterministic without
	// sorting -- internal/gates may not import sort, and does not need to.
	var missing []string
	for _, want := range []string{ruleNonFastForward, ruleDeletion} {
		if !have[want] {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		problems = append(problems, fmt.Sprintf(
			"the rulesets applying to %s do not include %s: history on this ref is what a cluster was told to apply, "+
				"so it must be append-only -- add %s to a ruleset targeting it",
			ref, strings.Join(missing, " and "), strings.Join(missing, " and ")))
	}

	// `update` gets its own refusal rather than joining the list above because
	// its reason is different -- those two rules protect the SHAPE of history,
	// this one protects WHO may add to it -- and because an operator told only
	// to "add the update rule" would then be locked out of their own delivery
	// ref, since the rule refuses every writer including the applier. Naming the
	// App to list in the same breath is the difference between a message that
	// can be followed and one that has to be read twice.
	if !have[ruleUpdate] {
		if applierAppID != 0 {
			problems = append(problems, fmt.Sprintf(
				"the rulesets applying to %s do not include %q: an ordinary fast-forward push is open to anyone with "+
					"write access, and a reconciler applies this ref, so an ungated commit becomes a deployed one -- "+
					"add the %q rule to a ruleset targeting it, with App id %d as its only bypass actor so the "+
					"applier can still advance the ref",
				ref, ruleUpdate, ruleUpdate, applierAppID))
		} else {
			problems = append(problems, fmt.Sprintf(
				"the rulesets applying to %s do not include %q: an ordinary fast-forward push is open to anyone with "+
					"write access, and a reconciler applies this ref, so an ungated commit becomes a deployed one -- "+
					"add the %q rule to a ruleset targeting it, and set DELIVERY_BYPASS_ACTOR_ID to the applier's App "+
					"id so this gate can name which App has to be on its bypass list",
				ref, ruleUpdate, ruleUpdate))
		}
	}

	return append(problems, deliveryBypassProblems(ref, rs, applierAppID, have[ruleUpdate])...)
}

// deliveryBypassProblems states the delivery ref's own bypass policy, which is
// deliberately NOT CheckRulesets. On main any named actor is the hole; on this
// ref exactly one is required, because the applier has to advance it. The
// narrowest claim that holds both is that the only actor allowed here is the
// App this deployment named, and everything else is a problem.
//
// An `update` rule with no matching actor is refused even though it is strictly
// more restrictive than intended: truss's own push would be blocked, so
// accepting the configuration would only move the failure to the push, with a
// vaguer error and no word about which knob to turn.
func deliveryBypassProblems(ref string, rs Rulesets, applierAppID int, hasUpdate bool) []string {
	var problems []string
	matched := 0

	for _, r := range rs.Applicable {
		for _, a := range r.BypassActors {
			if !a.isApplier(applierAppID) {
				problems = append(problems, fmt.Sprintf(
					"ruleset %q (id %d) lets %s (%s, actor_id %d) bypass the rules on %s: on the delivery ref the "+
						"only actor allowed to skip anything is the applier's own App, because a bypass here advances a "+
						"cluster onto history nobody gated",
					r.Name, r.ID, a.ActorType, a.BypassMode, a.ActorID, ref))
				continue
			}
			matched++
		}
	}

	if hasUpdate && matched == 0 {
		if applierAppID == 0 {
			return append(problems, fmt.Sprintf(
				"%s is protected by an `update` rule, but this truss has no applier App id to match against, so it "+
					"cannot tell whether the applier may still advance the ref: set DELIVERY_BYPASS_ACTOR_ID to the "+
					"GitHub App id of the App the applier authenticates as", ref))
		}
		return append(problems, fmt.Sprintf(
			"%s has an `update` rule but no bypass actor for App id %d: the rule refuses every writer, including the "+
				"applier, so nothing could ever be published -- add that App to the ruleset's bypass list. Dropping the "+
				"rule is not a way out, it is refused above", ref, applierAppID))
	}
	return problems
}
