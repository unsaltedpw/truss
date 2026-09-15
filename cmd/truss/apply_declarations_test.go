package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/beeradb/truss/internal/plan"
)

// ⚠️ THE DECLARATIONS GATE WAS BUILT AND TESTED IN internal/gates AND
// internal/plan, AND NOTHING IN cmd/truss CALLED THEM. gates.CheckDeclarations
// and plan.Declarations had their own thorough unit tests -- generated
// against a real tofu binary, per that package's own doc -- and zero
// coverage of the one thing that makes them matter: that applyOneRoot
// actually asks them, for every root, on every plan. This file is that
// wiring test, the same shape apply_digest_gate_test.go is for the digest
// gate.

// declTree gives a plan document a "configuration" block declaring one
// resource with the given provisioners (nil for none), alongside a real
// resource_changes entry so a test can also control whether the plan
// changes anything.
func declPlanJSON(address, resourceType string, provisioners []string, changesNothing bool) []byte {
	var provJSON strings.Builder
	provJSON.WriteString("[")
	for i, p := range provisioners {
		if i > 0 {
			provJSON.WriteString(",")
		}
		provJSON.WriteString(`{"type":"` + p + `"}`)
	}
	provJSON.WriteString("]")

	resourceChanges := `[{"address":"` + address + `","change":{"actions":["create"],"before":null,"after":{}}}]`
	if changesNothing {
		resourceChanges = `[]`
	}

	return []byte(`{"resource_changes":` + resourceChanges + `,` +
		`"configuration":{"root_module":{"resources":[{"address":"` + address +
		`","type":"` + resourceType + `","provisioners":` + provJSON.String() + `}]}}}`)
}

// declDeps drives one commit touching one root, with tofu's ShowJSON
// answering planJSON.
func declDeps(t *testing.T, sha, root string, planJSON []byte) (applyDeps, *fakeLedger, *fakeTofu) {
	t.Helper()
	forgeFake := compliantCommitGate("alice", sha, sha)
	forgeFake.ProtectionResult = compliantGatesProtection()
	git := &fakeGit{
		CommitsList:       []string{sha},
		ChangedByCommit:   map[string][]string{sha: {root + "/main.tf"}},
		TreeRootsByCommit: map[string][]string{sha: nil},
		HasDirFn:          func(r string) bool { return r == root },
	}
	tofu := &fakeTofu{PlanDetailedChanged: true, ShowJSONBytes: planJSON}
	deps, fl, _ := buildTestDeps(t, forgeFake, git, func([]string) tofuRunner { return tofu })
	return deps, fl, tofu
}

// TestDeclarationsGateRefusesAProvisioner: a plan declaring a provisioner is
// refused, and the refusal names the resource's address -- the wiring
// equivalent of gates.TestCheckDeclarationsRefusesARootProvisioner.
func TestDeclarationsGateRefusesAProvisioner(t *testing.T) {
	const sha = "declsha1"
	planJSON := declPlanJSON("terraform_data.evil", "terraform_data", []string{"local-exec"}, false)
	deps, fl, tofu := declDeps(t, sha, "projects/recipes", planJSON)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, "startsha")
	if result.failure == "" {
		t.Fatal("result.failure is empty, want a refusal -- the plan declares a provisioner")
	}
	if !strings.Contains(result.failure, "terraform_data.evil") {
		t.Errorf("refusal %q does not name the address", result.failure)
	}
	if !strings.Contains(result.failure, "local-exec") {
		t.Errorf("refusal %q does not name the provisioner type", result.failure)
	}
	if got := tofu.appliedDirs(); len(got) != 0 {
		t.Fatalf("tofu Apply was reached %d time(s) despite the gate refusing: %v", len(got), got)
	}
	if _, ok := fl.get("applied/" + sha); ok {
		t.Errorf("applied/%s was written for a refused commit", sha)
	}
}

// TestDeclarationsGateRefusesAProvisionerEvenWithZeroResourceChanges: the
// deliberate difference from the digest gate. A plan that changes nothing
// still declares the provisioner it carries -- it is ready to run the next
// time anything touches that resource -- so it must still be refused, unlike
// the digest gate which has nothing to check on an empty plan.
func TestDeclarationsGateRefusesAProvisionerEvenWithZeroResourceChanges(t *testing.T) {
	const sha = "declsha2"
	planJSON := declPlanJSON("terraform_data.evil", "terraform_data", []string{"local-exec"}, true)
	deps, _, tofu := declDeps(t, sha, "projects/recipes", planJSON)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, "startsha")
	if result.failure == "" {
		t.Fatal("result.failure is empty, want a refusal -- a provisioner is declared even though this plan changes nothing")
	}
	if !strings.Contains(result.failure, "terraform_data.evil") {
		t.Errorf("refusal %q does not name the address", result.failure)
	}
	if got := tofu.appliedDirs(); len(got) != 0 {
		t.Fatalf("tofu Apply was reached %d time(s) despite the gate refusing: %v", len(got), got)
	}
}

// TestDeclarationsGateAppliesACleanPlan: a plan with no provisioner and no
// forbidden resource type applies normally -- the gate does not refuse on
// no evidence.
func TestDeclarationsGateAppliesACleanPlan(t *testing.T) {
	const sha = "declsha3"
	planJSON := declPlanJSON("terraform_data.clean", "terraform_data", nil, false)
	deps, fl, tofu := declDeps(t, sha, "projects/recipes", planJSON)
	digest, err := plan.Digest(planJSON)
	if err != nil {
		t.Fatalf("plan.Digest: %v", err)
	}
	fl.put("digests/"+sha+"/projects-recipes.digest", []byte(digest))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, "startsha")
	if result.failure != "" {
		t.Fatalf("result.failure = %q, want empty -- the plan declares nothing forbidden", result.failure)
	}
	if got := tofu.appliedDirs(); len(got) != 1 {
		t.Fatalf("tofu Apply was called %d times, want exactly 1: %v", len(got), got)
	}
}

// TestDeclarationsGateRefusesTheCredentialsRootToo: §2 item 10 exempts
// credentials from the DIGEST gate because CI cannot plan that root, which
// says nothing about whether it may run arbitrary commands. The declarations
// gate must not copy that exemption.
func TestDeclarationsGateRefusesTheCredentialsRootToo(t *testing.T) {
	const sha = "declsha4"
	planJSON := declPlanJSON("terraform_data.evil", "terraform_data", []string{"local-exec"}, false)
	deps, _, tofu := declDeps(t, sha, "credentials", planJSON)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, "startsha")
	if result.failure == "" {
		t.Fatal("result.failure is empty, want a refusal -- credentials is not exempt from the declarations gate")
	}
	if !strings.Contains(result.failure, "terraform_data.evil") {
		t.Errorf("refusal %q does not name the address", result.failure)
	}
	if got := tofu.appliedDirs(); len(got) != 0 {
		t.Fatalf("tofu Apply was reached %d time(s) for credentials despite the gate refusing: %v", len(got), got)
	}
}
