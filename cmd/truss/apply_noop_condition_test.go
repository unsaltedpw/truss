package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// This file pins the noop condition runCommitLoop uses after truss stopped
// managing ansible/plays/ and deliveries/: `len(roots) == 0` alone, where
// roots is the credentials+tofu half of repo.TouchedUnits. A commit that
// touches only a path under one of those two removed trees now changes
// nothing truss applies -- plays run on each host via ansible-pull, and
// delivery was never built -- so it must be recorded as a noop and HEAD
// must still advance past it, the same as a commit touching only docs/.

// TestACommitTouchingOnlyAPlayIsANoopThatAdvancesHead pins the case above:
// truss no longer executes anything under ansible/plays/, so a commit that
// touches only a play must not wedge the queue -- it is recorded as
// uneventful and HEAD moves past it, exactly like a commit touching only
// documentation.
func TestACommitTouchingOnlyAPlayIsANoopThatAdvancesHead(t *testing.T) {
	const sha = "commitsha-play-only"
	const head = sha

	forgeFake := compliantCommitGate("alice", sha, head)
	forgeFake.ProtectionResult = compliantGatesProtection()

	git := &fakeGit{
		CommitsList: []string{sha},
		ChangedByCommit: map[string][]string{
			sha: {"ansible/plays/x/site.yml"},
		},
		TreeRootsByCommit:     map[string][]string{sha: nil},
		TreeTofuUnitsByCommit: map[string][]string{sha: nil},
		HasDirFn:              func(root string) bool { return false },
	}
	tofu := &fakeTofu{}
	deps, fl, _ := buildTestDeps(t, forgeFake, git, func(env []string) tofuRunner { return tofu })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, "startsha")

	if result.failure != "" {
		t.Fatalf("result.failure = %q, want empty -- a play-only commit is a noop, not a refusal", result.failure)
	}
	if got := tofu.appliedDirs(); len(got) != 0 {
		t.Fatalf("tofu applied %v, want no apply at all for a commit touching only a play", got)
	}
	body, ok := fl.get("applied/" + sha)
	if !ok {
		t.Fatalf("applied/%s was not written -- a noop must still be recorded", sha)
	}
	if !strings.Contains(string(body), "noop") {
		t.Fatalf("applied/%s = %s, want the bare noop record", sha, body)
	}
	headBytes, ok := fl.get("head")
	if !ok {
		t.Fatalf("head was not written at all")
	}
	if string(headBytes) != sha {
		t.Fatalf("head = %q, want %q -- HEAD must advance past a noop commit", headBytes, sha)
	}
}

// TestACommitTouchingATofuRootStillApplies is the noop condition's other
// half: a commit that DOES touch a tofu root must still be planned and
// applied, not swept into the noop path by an over-widened condition.
func TestACommitTouchingATofuRootStillApplies(t *testing.T) {
	const sha = "commitsha-tofu-root"
	const head = sha

	forgeFake := compliantCommitGate("alice", sha, head)
	forgeFake.ProtectionResult = compliantGatesProtection()

	git := &fakeGit{
		CommitsList: []string{sha},
		ChangedByCommit: map[string][]string{
			sha: {"projects/recipes/main.tf"},
		},
		TreeRootsByCommit: map[string][]string{sha: nil},
		HasDirFn:          func(root string) bool { return root == "projects/recipes" },
	}
	tofu := &fakeTofu{PlanDetailedChanged: true, ShowJSONBytes: noopPlanJSON}
	deps, fl, _ := buildTestDeps(t, forgeFake, git, func(env []string) tofuRunner { return tofu })
	seedPlainLockfile(t, deps.Cfg.Workdir, "projects/recipes")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, "startsha")

	if result.failure != "" {
		t.Fatalf("result.failure = %q, want empty", result.failure)
	}
	if got := tofu.appliedDirs(); len(got) != 1 || !strings.Contains(got[0], "projects/recipes") {
		t.Fatalf("tofu applied %v, want exactly one apply of projects/recipes -- the root was swept into the noop path", got)
	}
	body, ok := fl.get("applied/" + sha)
	if !ok {
		t.Fatalf("applied/%s was not written at all", sha)
	}
	if !strings.Contains(string(body), "projects/recipes") {
		t.Fatalf("applied/%s = %s, want it to name projects/recipes, not the bare noop record", sha, body)
	}
}
