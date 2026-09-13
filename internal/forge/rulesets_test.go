package forge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// --- Rulesets ----------------------------------------------------------------

// TestRulesetsJoinsTheTwoReads is the load-bearing shape test: the effective
// list names ruleset 42 twice (one entry per rule it contributes to this
// branch) and never carries bypass_actors -- if this method read only that
// endpoint, the hole in docs/work-items.md's "Rulesets: a hole after all"
// section would still be open. The per-ruleset read is what supplies
// enforcement and bypass_actors, and 42 must be fetched exactly once despite
// appearing twice in the first list.
func TestRulesetsJoinsTheTwoReads(t *testing.T) {
	var rulesetFetches int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations/7654321/access_tokens":
			mintHandler("ghs_x")(w, r)
		case r.URL.Path == "/repos/acme/widgets/rules/branches/main":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{"type": "pull_request", "ruleset_source_type": "Repository", "ruleset_source": "acme/widgets", "ruleset_id": 42},
				{"type": "required_status_checks", "ruleset_source_type": "Repository", "ruleset_source": "acme/widgets", "ruleset_id": 42}
			]`))
		case r.URL.Path == "/repos/acme/widgets/rulesets/42":
			rulesetFetches++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"id": 42,
				"name": "require a pull request",
				"enforcement": "active",
				"bypass_actors": [
					{"actor_id": 99, "actor_type": "DeployKey", "bypass_mode": "always"}
				]
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	rs, err := c.Rulesets(context.Background(), "main")
	if err != nil {
		t.Fatalf("Rulesets: %v", err)
	}
	if rulesetFetches != 1 {
		t.Fatalf("rulesets/42 was fetched %d times, want 1 (it was named twice in the effective list)", rulesetFetches)
	}
	if len(rs.Applicable) != 1 {
		t.Fatalf("Applicable = %+v, want exactly one ruleset", rs.Applicable)
	}
	got := rs.Applicable[0]
	if got.ID != 42 || got.Name != "require a pull request" || got.Enforcement != "active" {
		t.Fatalf("Applicable[0] = %+v, want id 42, name set, enforcement active", got)
	}
	if len(got.BypassActors) != 1 || got.BypassActors[0].ActorType != "DeployKey" || got.BypassActors[0].BypassMode != "always" {
		t.Fatalf("BypassActors = %+v, want one DeployKey actor with bypass_mode always", got.BypassActors)
	}
	if got.BypassActors[0].ActorID != 99 {
		t.Fatalf("ActorID = %d, want 99 (the actor_id the wire response carried)", got.BypassActors[0].ActorID)
	}
}

// TestRulesetsWithNoneApplyingIsEmptyNotAnError: an empty effective-rules
// list is a real, compliant fact (no ruleset governs this branch), not the
// same thing as a read that failed. gates.CheckRulesets relies on this: a
// nil/empty Applicable must not be confused with "unreadable".
func TestRulesetsWithNoneApplyingIsEmptyNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/7654321/access_tokens":
			mintHandler("ghs_x")(w, r)
		case "/repos/acme/widgets/rules/branches/main":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	rs, err := c.Rulesets(context.Background(), "main")
	if err != nil {
		t.Fatalf("Rulesets: %v", err)
	}
	if len(rs.Applicable) != 0 {
		t.Fatalf("Applicable = %+v, want empty", rs.Applicable)
	}
}

// TestRulesetsRefusesAnEntryWithNoRulesetID: the schema does not mark
// ruleset_id required, so an entry missing it cannot be attributed to any
// ruleset -- absent must be its own case, the same rule protection.go's own
// bypass_pull_request_allowances comment states, applied here to a field
// where absent means "unreadable" rather than "compliant".
func TestRulesetsRefusesAnEntryWithNoRulesetID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/7654321/access_tokens":
			mintHandler("ghs_x")(w, r)
		case "/repos/acme/widgets/rules/branches/main":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"type": "pull_request", "ruleset_source_type": "Repository", "ruleset_source": "acme/widgets"}]`))
		// ⚠️ rulesets/0 ANSWERS SUCCESSFULLY, DELIBERATELY. Without this, a
		// broken implementation that defaulted a missing ruleset_id to 0
		// would still fail this test for the WRONG reason -- id 0 hitting an
		// unmocked endpoint and 404ing -- and the refusal this test exists to
		// pin (an explicit "no ruleset_id" error) would never actually be
		// exercised. Answering 0 here forces the only way to fail to be the
		// nil check itself.
		case "/repos/acme/widgets/rulesets/0":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id": 0, "name": "should never be reached", "enforcement": "active"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	rs, err := c.Rulesets(context.Background(), "main")
	if err == nil {
		t.Fatalf("a rule with no ruleset_id was accepted; got %+v", rs)
	}
}

// TestRulesetsRefusesAResponseWithNoEnforcement: enforcement is a required
// field on the real endpoint (GitHub's REST API description marks it so on
// "repository-ruleset"); a response missing it is malformed, and a malformed
// body must be an error, never a zero-valued Ruleset that CheckRulesets
// would read as "not active" without any evidence that is true.
func TestRulesetsRefusesAResponseWithNoEnforcement(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/7654321/access_tokens":
			mintHandler("ghs_x")(w, r)
		case "/repos/acme/widgets/rules/branches/main":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"type": "pull_request", "ruleset_id": 1}]`))
		case "/repos/acme/widgets/rulesets/1":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id": 1, "name": "no enforcement here"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	rs, err := c.Rulesets(context.Background(), "main")
	if err == nil {
		t.Fatalf("a ruleset response with no enforcement was accepted; got %+v", rs)
	}
}

// TestRulesetsRefusesABypassActorWithNoActorType mirrors the ruleset_id and
// enforcement cases above for the field that matters most: a bypass actor
// this gate cannot name is not a bypass actor safely ignored, it is a
// response this method must refuse rather than silently drop the entry for.
func TestRulesetsRefusesABypassActorWithNoActorType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/7654321/access_tokens":
			mintHandler("ghs_x")(w, r)
		case "/repos/acme/widgets/rules/branches/main":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"type": "pull_request", "ruleset_id": 1}]`))
		case "/repos/acme/widgets/rulesets/1":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id": 1, "name": "r", "enforcement": "active", "bypass_actors": [{"actor_id": 1}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	rs, err := c.Rulesets(context.Background(), "main")
	if err == nil {
		t.Fatalf("a bypass actor with no actor_type was accepted; got %+v", rs)
	}
}

// TestRulesetsDefaultsAnAbsentBypassModeToAlways: GitHub's own schema
// documents "always" as bypass_mode's default. Treating an absent key as
// that documented default is following the spec, not guessing a value the
// wire never sent -- the same class of decision protection.go's status-check
// merge makes, just for a default rather than a merge.
func TestRulesetsDefaultsAnAbsentBypassModeToAlways(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/7654321/access_tokens":
			mintHandler("ghs_x")(w, r)
		case "/repos/acme/widgets/rules/branches/main":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"type": "pull_request", "ruleset_id": 1}]`))
		case "/repos/acme/widgets/rulesets/1":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id": 1, "name": "r", "enforcement": "active", "bypass_actors": [{"actor_id": 1, "actor_type": "Team"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	rs, err := c.Rulesets(context.Background(), "main")
	if err != nil {
		t.Fatalf("Rulesets: %v", err)
	}
	if len(rs.Applicable) != 1 || len(rs.Applicable[0].BypassActors) != 1 {
		t.Fatalf("Applicable = %+v, want one ruleset with one bypass actor", rs.Applicable)
	}
	if got := rs.Applicable[0].BypassActors[0].BypassMode; got != "always" {
		t.Fatalf("BypassMode = %q, want the documented default %q for an absent key", got, "always")
	}
}

// TestRulesetsDecodesAnIntegrationBypassActorsID: gates.CheckDeliveryRef
// compares an Integration bypass actor's id against the applier's own App
// id, so the wire decode has to carry actor_id through, not just actor_type
// and bypass_mode.
func TestRulesetsDecodesAnIntegrationBypassActorsID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/7654321/access_tokens":
			mintHandler("ghs_x")(w, r)
		case "/repos/acme/widgets/rules/branches/queued":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"type": "update", "ruleset_id": 1}]`))
		case "/repos/acme/widgets/rulesets/1":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id": 1, "name": "queued", "enforcement": "active", "bypass_actors": [
				{"actor_id": 4922051, "actor_type": "Integration", "bypass_mode": "always"}
			]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	rs, err := c.Rulesets(context.Background(), "queued")
	if err != nil {
		t.Fatalf("Rulesets: %v", err)
	}
	if len(rs.Applicable) != 1 || len(rs.Applicable[0].BypassActors) != 1 {
		t.Fatalf("Applicable = %+v, want one ruleset with one bypass actor", rs.Applicable)
	}
	a := rs.Applicable[0].BypassActors[0]
	if a.ActorType != "Integration" || a.ActorID != 4922051 || a.BypassMode != "always" {
		t.Fatalf("BypassActors[0] = %+v, want Integration id 4922051 mode always", a)
	}
}

// TestRulesetsRefusesAnIntegrationBypassActorWithNoActorID: the schema marks
// actor_id "Required for Integration" -- absent is legitimate only for
// OrganizationAdmin (ignored) and DeployKey (documented null), so an
// Integration entry missing it is a malformed response, never a silent
// zero-valued id that would never compare equal to a real App id anyway but
// would do so for the wrong reason (a decode gap, not a mismatch).
func TestRulesetsRefusesAnIntegrationBypassActorWithNoActorID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/7654321/access_tokens":
			mintHandler("ghs_x")(w, r)
		case "/repos/acme/widgets/rules/branches/queued":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"type": "update", "ruleset_id": 1}]`))
		case "/repos/acme/widgets/rulesets/1":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id": 1, "name": "queued", "enforcement": "active", "bypass_actors": [
				{"actor_type": "Integration", "bypass_mode": "always"}
			]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	rs, err := c.Rulesets(context.Background(), "queued")
	if err == nil {
		t.Fatalf("an Integration bypass actor with no actor_id was accepted; got %+v", rs)
	}
}

// TestRulesetsANon2xxIsAnErrorNotAZeroValue mirrors
// TestANon2xxIsAnErrorNotAZeroValue for Protection: this endpoint's failure
// mode must be a returned error, never a zero-valued gates.Rulesets that
// CheckRulesets would read as "no rulesets apply" -- a compliant-looking
// answer for a repository nobody could actually ask.
func TestRulesetsANon2xxIsAnErrorNotAZeroValue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/7654321/access_tokens":
			mintHandler("ghs_x")(w, r)
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message": "internal error"}`))
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	rs, err := c.Rulesets(context.Background(), "main")
	if err == nil {
		t.Fatalf("a 500 was not reported as an error; got zero-value Rulesets = %+v", rs)
	}
}
