package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/beeradb/truss/internal/gates"
)

// TestApplyRefusesOnARulesetBypassActor wires gates.CheckRulesets into the
// pass exactly where classic protection is checked, reproducing the hole
// docs/work-items.md's "Rulesets: a hole after all" section records: on
// 2026-09-09 a push straight to this repository's own main was ACCEPTED by a
// ruleset bypass actor while branch protection alone read compliant.
// CheckProtection could not see it; this test drives runApplyPass with a
// compliant Protection AND a ruleset carrying a bypass actor, and the pass
// must refuse anyway.
func TestApplyRefusesOnARulesetBypassActor(t *testing.T) {
	forgeFake := &fakeForge{
		ProtectionResult: compliantGatesProtection(),
		RulesetsResult: gates.Rulesets{Applicable: []gates.Ruleset{{
			ID:               42,
			Name:             "require a pull request",
			Enforcement:      "active",
			BypassActorsRead: true,
			BypassActors: []gates.BypassActor{
				{ActorType: "DeployKey", BypassMode: "always"},
			},
		}}},
	}
	git := &fakeGit{HasDirFn: func(string) bool { return false }}
	newTofu := func(env []string) tofuRunner { return &fakeTofu{} }

	deps, _, _ := buildTestDeps(t, forgeFake, git, newTofu)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, "headsha1")

	if result.failure == "" {
		t.Fatal("a compliant Protection alongside a ruleset bypass actor was not refused")
	}
	if !strings.Contains(result.failure, "require a pull request") {
		t.Fatalf("failure = %q, want it to name the ruleset", result.failure)
	}
	if !strings.Contains(result.failure, "DeployKey") {
		t.Fatalf("failure = %q, want it to name the bypass actor's type", result.failure)
	}
}

// TestApplyRefusesWhenRulesetsCannotBeRead: mirrors the existing contract
// for a Protection read failure -- an endpoint that could not be asked at
// all must refuse the pass, never be treated as "no rulesets".
func TestApplyRefusesWhenRulesetsCannotBeRead(t *testing.T) {
	forgeFake := &fakeForge{
		ProtectionResult: compliantGatesProtection(),
		RulesetsErr:      errors.New("rulesets endpoint unreachable"),
	}
	git := &fakeGit{HasDirFn: func(string) bool { return false }}
	newTofu := func(env []string) tofuRunner { return &fakeTofu{} }

	deps, _, _ := buildTestDeps(t, forgeFake, git, newTofu)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, "headsha1")

	if result.failure == "" {
		t.Fatal("an unreadable ruleset list was not refused")
	}
	if !strings.Contains(result.failure, "could not read rulesets") {
		t.Fatalf("failure = %q, want it to name the unreadable rulesets endpoint", result.failure)
	}
}

// TestApplyPassesWithACompliantRulesetAlongsideProtection: the gate must
// not become impossible to satisfy -- a repository with no ruleset (the
// fakeForge default, an empty RulesetsResult) alongside compliant
// protection is a clean pass, same as before this gate existed.
func TestApplyPassesWithNoRulesetsAlongsideCompliantProtection(t *testing.T) {
	forgeFake := &fakeForge{ProtectionResult: compliantGatesProtection()}
	git := &fakeGit{
		CommitsList: nil, // nothing to do
		HasDirFn:    func(string) bool { return false },
	}
	newTofu := func(env []string) tofuRunner { return &fakeTofu{} }

	deps, _, _ := buildTestDeps(t, forgeFake, git, newTofu)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, "headsha1")

	if result.failure != "" {
		t.Fatalf("a repository with no rulesets and compliant protection was refused: %q", result.failure)
	}
}
