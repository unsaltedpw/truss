package forge

import (
	"context"
	"fmt"
	"net/url"

	"github.com/beeradb/truss/internal/gates"
)

// wireEffectiveRule is one entry of GET /repos/{o}/{r}/rules/branches/{branch},
// which returns the rules that apply to a branch, already flattened across
// org and repo rulesets. Verified against the schema this endpoint's
// response is documented to use (component "repository-rule-detailed",
// GitHub's REST API description, 2026-09-09): every entry carries
// ruleset_id, ruleset_source_type and ruleset_source alongside its own
// per-type fields, but never bypass_actors -- that is why Rulesets below
// makes a second call per ruleset id.
//
// ⚠️ RulesetID IS A POINTER DESPITE APPEARING ON EVERY DOCUMENTED EXAMPLE.
// The schema does not mark it (or ruleset_source_type/ruleset_source)
// required, so the same "absent must be its own case" rule this package
// applies everywhere else applies here: an entry with no ruleset_id cannot
// be attributed to a ruleset, and Rulesets refuses the whole read rather
// than silently skipping it.
type wireEffectiveRule struct {
	Type      string `json:"type"`
	RulesetID *int   `json:"ruleset_id"`
}

// wireRuleset is GET /repos/{o}/{r}/rulesets/{ruleset_id}, verified against
// the "repository-ruleset" and "repository-ruleset-bypass-actor" schemas in
// GitHub's REST API description (2026-09-09). enforcement is one of
// "active", "evaluate" or "disabled"; a bypass actor's actor_type is one of
// "Integration", "OrganizationAdmin", "RepositoryRole", "Team", "DeployKey"
// or "User" (the docs given for this task omitted "User"; the schema has
// it, so it is included here), and its bypass_mode is one of "always",
// "pull_request" or "exempt" (the docs given for this task omitted
// "exempt"; likewise included).
type wireRuleset struct {
	Name         *string `json:"name"`
	Enforcement  *string `json:"enforcement"`
	BypassActors []struct {
		ActorType  *string `json:"actor_type"`
		ActorID    *int64  `json:"actor_id"`
		BypassMode *string `json:"bypass_mode"`
	} `json:"bypass_actors"`
}

// Rulesets reads every ruleset that applies to branch, joining the two
// endpoints GitHub splits this fact across:
//
//  1. GET rules/branches/{branch} says WHICH rulesets apply -- it can name
//     the same ruleset more than once (once per rule it contributes) but
//     never carries bypass_actors, so knowing a ruleset applies is not
//     enough to know who can skip it.
//  2. GET rulesets/{id}, once per distinct id from (1), is the only read
//     that carries enforcement and bypass_actors.
//
// A payload that cannot be read at all -- a non-2xx, a malformed body, an
// entry missing a field this method depends on -- is an error, never a
// zero-valued gates.Rulesets that CheckRulesets would then have nothing to
// refuse. See gates.Rulesets's own doc comment for why that matters.
func (c *Client) Rulesets(ctx context.Context, branch string) (gates.Rulesets, error) {
	path := c.repoPath("/rules/branches/%s", url.PathEscape(branch))
	var rules []wireEffectiveRule
	if err := c.request(ctx, "GET", path, &rules); err != nil {
		return gates.Rulesets{}, err
	}

	// Distinct ruleset ids, in the order they first appeared -- rules for
	// the same ruleset are deduplicated here so it is read once regardless
	// of how many of its rules matched this branch.
	var ids []int
	seen := map[int]bool{}
	// The rule types each ruleset contributes to THIS ref, which is the only
	// place they are available: rulesets/{id} describes the ruleset, not
	// which of its rules reached this branch.
	types := map[int][]string{}
	for _, r := range rules {
		if r.RulesetID == nil {
			return gates.Rulesets{}, fmt.Errorf("forge: rules for branch %q: an entry has no ruleset_id", branch)
		}
		id := *r.RulesetID
		if r.Type == "" {
			return gates.Rulesets{}, fmt.Errorf("forge: rules for branch %q: an entry of ruleset %d has no type", branch, id)
		}
		types[id] = append(types[id], r.Type)
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}

	applicable := make([]gates.Ruleset, 0, len(ids))
	for _, id := range ids {
		var w wireRuleset
		if err := c.request(ctx, "GET", c.repoPath("/rulesets/%d", id), &w); err != nil {
			return gates.Rulesets{}, err
		}
		if w.Enforcement == nil {
			return gates.Rulesets{}, fmt.Errorf("forge: ruleset %d: response has no enforcement", id)
		}
		name := ""
		if w.Name != nil {
			name = *w.Name
		}
		gr := gates.Ruleset{ID: id, Name: name, Enforcement: *w.Enforcement, Rules: types[id]}
		for _, a := range w.BypassActors {
			if a.ActorType == nil {
				return gates.Rulesets{}, fmt.Errorf("forge: ruleset %d: a bypass actor has no actor_type", id)
			}
			// bypass_mode's documented default is "always"; GitHub sends it
			// explicitly in practice, but a decode that found the key absent
			// uses the same default the API itself defines rather than
			// treating an optional field as a malformed response.
			mode := "always"
			if a.BypassMode != nil {
				mode = *a.BypassMode
			}
			// actor_id's own documentation: "Required for Integration,
			// RepositoryRole, Team, and User actor types. If actor_type is
			// OrganizationAdmin, actor_id is ignored. If actor_type is
			// DeployKey, this should be null." So absent is only ever
			// legitimate for OrganizationAdmin and DeployKey; for Integration
			// specifically -- the one type gates.CheckDeliveryRef compares
			// against an App id -- an absent actor_id is a malformed
			// response, not a zero-valued match against no App.
			var actorID int64
			if a.ActorID != nil {
				actorID = *a.ActorID
			} else if *a.ActorType == "Integration" {
				return gates.Rulesets{}, fmt.Errorf("forge: ruleset %d: an Integration bypass actor has no actor_id", id)
			}
			gr.BypassActors = append(gr.BypassActors, gates.BypassActor{ActorType: *a.ActorType, ActorID: actorID, BypassMode: mode})
		}
		applicable = append(applicable, gr)
	}
	return gates.Rulesets{Applicable: applicable}, nil
}
