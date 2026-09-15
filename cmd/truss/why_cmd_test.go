package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/beeradb/truss/internal/ledger"
)

// TestWhyAppliedRecordNamesEachRootsCount: an applied record with two
// roots -- one with a real count, one with a null resource_changes (the
// summary_from_plan fallback for an unparseable `tofu show -json`) -- must
// print a real number for the first and "unknown" for the second, never 0.
func TestWhyAppliedRecordNamesEachRootsCount(t *testing.T) {
	dir, write := testSecretsDir(t)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	env := testFullEnv(dir.Root, t.TempDir(), nil)

	fl.put("applied/deadbeef", []byte(`{"roots":{"platform":{"resource_changes":3},"credentials":{"resource_changes":null}}}`))

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"why", "deadbeef"}, env, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "platform") || !strings.Contains(out, "3 resource change") {
		t.Errorf("stdout = %q, want it to name platform's 3 resource changes", out)
	}
	if !strings.Contains(out, "credentials") || !strings.Contains(out, "unknown") {
		t.Errorf("stdout = %q, want it to name credentials as unknown, not 0", out)
	}
	if strings.Contains(out, "credentials: 0") {
		t.Errorf("stdout = %q, printed 0 for a null resource_changes -- must print unknown", out)
	}
}

// TestWhyNoopRecordExitsZero: `{"noop":true}` is a clean pass that
// touched no root.
func TestWhyNoopRecordExitsZero(t *testing.T) {
	dir, write := testSecretsDir(t)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	env := testFullEnv(dir.Root, t.TempDir(), nil)

	fl.put("applied/deadbeef", []byte(`{"noop":true}`))

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"why", "deadbeef"}, env, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(strings.ToLower(stdout.String()), "noop") {
		t.Errorf("stdout = %q, want it to say noop", stdout.String())
	}
}

// TestWhySkippedRecordExitsZeroAndPrintsReason: a `truss skip` record
// lives under the applied key too, tagged "skipped":true.
func TestWhySkippedRecordExitsZeroAndPrintsReason(t *testing.T) {
	dir, write := testSecretsDir(t)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	env := testFullEnv(dir.Root, t.TempDir(), nil)

	fl.put("applied/deadbeef", []byte(`{"skipped":true,"reason":"known bad plan, never applies","at":"2026-09-08T12:30:45Z"}`))

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"why", "deadbeef"}, env, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "known bad plan, never applies") {
		t.Errorf("stdout = %q, want it to print the skip reason", out)
	}
	if !strings.Contains(strings.ToLower(out), "skip") {
		t.Errorf("stdout = %q, want it to say this was a skip", out)
	}
}

// TestWhyFailedRecordExitsZeroAndPrintsReasonAndTimestamp: nothing under
// applied/, but a failed/ record exists.
func TestWhyFailedRecordExitsZeroAndPrintsReasonAndTimestamp(t *testing.T) {
	dir, write := testSecretsDir(t)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	env := testFullEnv(dir.Root, t.TempDir(), nil)

	fl.put("failed/deadbeef", []byte(`{"reason":"tofu apply failed for platform","at":"2026-09-08T12:30:45Z"}`))

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"why", "deadbeef"}, env, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "tofu apply failed for platform") {
		t.Errorf("stdout = %q, want it to print the failure reason", out)
	}
	if !strings.Contains(out, "2026-09-08T12:30:45Z") {
		t.Errorf("stdout = %q, want it to print the recorded timestamp", out)
	}
}

// TestWhyAbsentRecordExitsTwo: neither key exists -- the queue has not
// reached this commit yet, matching `ledger get`'s own "exit 2 if absent".
func TestWhyAbsentRecordExitsTwo(t *testing.T) {
	dir, write := testSecretsDir(t)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	env := testFullEnv(dir.Root, t.TempDir(), nil)

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"why", "neverseen"}, env, nil, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (stderr: %s)", code, stderr.String())
	}
}

// --- §2: queue position ----------------------------------------------------

// queueFixture builds a real four-commit history (c0..c3) touching nothing
// but README.md -- every commit is a noop for §3/§4's purposes, which keeps
// these tests about queue position alone, and origin/main is set to c3
// directly with `update-ref` rather than a real remote and push, which is
// all git needs to resolve the name (no `git remote add` or network
// involved).
func queueFixture(t *testing.T) (dir string, c0, c1, c2, c3 string) {
	t.Helper()
	dir = t.TempDir()
	runGit(t, dir, "init", "--quiet")
	c0 = commitFiles(t, dir, map[string]string{"README.md": "c0\n"}, "c0")
	c1 = commitFiles(t, dir, map[string]string{"README.md": "c1\n"}, "c1")
	c2 = commitFiles(t, dir, map[string]string{"README.md": "c2\n"}, "c2")
	c3 = commitFiles(t, dir, map[string]string{"README.md": "c3\n"}, "c3")
	runGit(t, dir, "update-ref", "refs/remotes/origin/main", c3)
	return dir, c0, c1, c2, c3
}

// whyEnv builds the secrets dir, fake ledger and env a `why` test needs,
// with HEAD seeded to head. Shared by every §2-§4 test below so each one
// states only what makes it different.
func whyEnv(t *testing.T, head string) (env func(string) string, fl *fakeLedger) {
	t.Helper()
	sdir, write := testSecretsDir(t)
	fl = newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	fl.put("head", []byte(head))
	env = testFullEnv(sdir.Root, t.TempDir(), map[string]string{"APPROVER": "alice"})
	return env, fl
}

func TestWhyQueuePositionAtHead(t *testing.T) {
	dir, c0, c1, _, _ := queueFixture(t)
	env, _ := whyEnv(t, c1)

	var stdout, stderr bytes.Buffer
	runEnv(context.Background(), []string{"why", c1, "--dir", dir}, env, nil, &stdout, &stderr)
	out := stdout.String()
	if !strings.Contains(out, "at HEAD") {
		t.Errorf("stdout = %q, want it to say sha is at HEAD", out)
	}
	_ = c0
}

func TestWhyQueuePositionNextInQueue(t *testing.T) {
	dir, _, c1, c2, _ := queueFixture(t)
	env, _ := whyEnv(t, c1)

	var stdout, stderr bytes.Buffer
	runEnv(context.Background(), []string{"why", c2, "--dir", dir}, env, nil, &stdout, &stderr)
	out := stdout.String()
	if !strings.Contains(out, "ahead of HEAD: it is next in the queue") {
		t.Errorf("stdout = %q, want it to say c2 is next in the queue", out)
	}
}

// TestWhyQueuePositionAheadNamesTheBlocker is the incident's headline
// question, answered: how many commits are in front, and which ONE is
// actually blocking.
func TestWhyQueuePositionAheadNamesTheBlocker(t *testing.T) {
	dir, c0, c1, c2, _ := queueFixture(t)
	env, _ := whyEnv(t, c0)

	var stdout, stderr bytes.Buffer
	runEnv(context.Background(), []string{"why", c2, "--dir", dir}, env, nil, &stdout, &stderr)
	out := stdout.String()
	if !strings.Contains(out, "1 commit(s) are queued in front of it") {
		t.Errorf("stdout = %q, want it to count exactly one commit in front of c2", out)
	}
	if !strings.Contains(out, "currently blocking on "+c1) {
		t.Errorf("stdout = %q, want it to name %s as the blocker", out, c1)
	}
}

func TestWhyQueuePositionBehindHead(t *testing.T) {
	dir, c0, _, c2, _ := queueFixture(t)
	env, _ := whyEnv(t, c2)

	var stdout, stderr bytes.Buffer
	runEnv(context.Background(), []string{"why", c0, "--dir", dir}, env, nil, &stdout, &stderr)
	out := stdout.String()
	if !strings.Contains(out, "behind HEAD -- the queue has already passed this commit") {
		t.Errorf("stdout = %q, want it to say c0 is behind HEAD", out)
	}
}

// TestWhyQueuePositionUnresolvableSHA covers a sha that is neither HEAD, nor
// ahead of it, nor behind it -- because git cannot resolve it against
// origin/main at all. This must say so plainly rather than guess a side.
func TestWhyQueuePositionUnresolvableSHA(t *testing.T) {
	dir, c0, _, _, _ := queueFixture(t)
	env, _ := whyEnv(t, c0)

	var stdout, stderr bytes.Buffer
	// ⚠️ SHORT, AND scripts/leakscan IS WHY. Its "32+ character hex string"
	// rule cannot tell a fabricated sha from a real key id, and it should not
	// try -- so the fixture convention in this package is a short one
	// ("deadbeef", "0123456789abcdef"). A full-length fake sha here failed CI
	// with: leakscan: a 32+ character hex string (account id, key id, hash of
	// something real). This sha only has to be unresolvable, and it is.
	runEnv(context.Background(), []string{"why", "deadbeefdeadbeef", "--dir", dir}, env, nil, &stdout, &stderr)
	out := stdout.String()
	if !strings.Contains(out, "does not resolve against origin/main") {
		t.Errorf("stdout = %q, want it to say the sha does not resolve", out)
	}
}

// TestWhyQueuePositionNoHeadInLedger: nothing has ever run. §2 must say so
// rather than crash or silently omit itself -- the ledger command's own
// applied/failed lookup is unaffected (that is a separate read), so this
// checks §2's own line specifically.
func TestWhyQueuePositionNoHeadInLedger(t *testing.T) {
	dir, c0, _, _, _ := queueFixture(t)
	sdir, write := testSecretsDir(t)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	// Deliberately no fl.put("head", ...).
	env := testFullEnv(sdir.Root, t.TempDir(), nil)

	var stdout, stderr bytes.Buffer
	runEnv(context.Background(), []string{"why", c0, "--dir", dir}, env, nil, &stdout, &stderr)
	out := stdout.String()
	if !strings.Contains(out, "queue: unknown -- the ledger has no HEAD; nothing has ever run") {
		t.Errorf("stdout = %q, want the no-HEAD line", out)
	}
}

// --- §3: units selected ------------------------------------------------

func TestWhyUnitsSelectedMixedCommit(t *testing.T) {
	dir, _, mixed, _, _ := unitsFixture(t)
	runGit(t, dir, "update-ref", "refs/remotes/origin/main", mixed)
	env, _ := whyEnv(t, mixed)

	var stdout, stderr bytes.Buffer
	runEnv(context.Background(), []string{"why", mixed, "--dir", dir}, env, nil, &stdout, &stderr)
	out := stdout.String()
	for _, want := range []string{"tofu:    credentials", "tofu:    hosts/h", "tofu:    projects/foo"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q, want it to contain %q", out, want)
		}
	}
	if strings.Contains(out, "shared input") {
		t.Errorf("stdout = %q, an ordinary commit must not carry the shared-input warning", out)
	}
}

// TestWhyUnitsSelectedNamesTheSharedInputEffect is the case the task calls
// out by name: a commit touching only a shared input (modules/) selects
// EVERY unit in the tree, and that surprise must be stated explicitly
// rather than left for a reader to notice from an unusually long list.
func TestWhyUnitsSelectedNamesTheSharedInputEffect(t *testing.T) {
	dir, _, _, shared, _ := unitsFixture(t)
	runGit(t, dir, "update-ref", "refs/remotes/origin/main", shared)
	env, _ := whyEnv(t, shared)

	var stdout, stderr bytes.Buffer
	runEnv(context.Background(), []string{"why", shared, "--dir", dir}, env, nil, &stdout, &stderr)
	out := stdout.String()
	if !strings.Contains(out, "shared input") {
		t.Errorf("stdout = %q, want the shared-input warning", out)
	}
	for _, want := range []string{"tofu:    clusters/beta", "tofu:    hosts/h", "tofu:    projects/foo"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q, want it to contain %q (every unit in the tree)", out, want)
		}
	}
}

func TestWhyUnitsSelectedNoopCommit(t *testing.T) {
	dir, _, _, _, docsOnly := unitsFixture(t)
	runGit(t, dir, "update-ref", "refs/remotes/origin/main", docsOnly)
	env, _ := whyEnv(t, docsOnly)

	var stdout, stderr bytes.Buffer
	runEnv(context.Background(), []string{"why", docsOnly, "--dir", dir}, env, nil, &stdout, &stderr)
	out := stdout.String()
	if !strings.Contains(out, "units:\n  none -- this commit is a noop") {
		t.Errorf("stdout = %q, want the noop line", out)
	}
	if !strings.Contains(out, "digests: n/a -- this commit selects no tofu root") {
		t.Errorf("stdout = %q, want digests to report n/a for a commit with no units", out)
	}
}

// --- §4: approved plan digests ----------------------------------------

// TestWhyApprovedDigestsPresentAndAbsent is the central §4 case: the digest
// is filed under the PR's HEAD sha, not the commit sha in the queue, and
// this must report present for a root CI filed and absent for one it did
// not, without confusing the two shas.
func TestWhyApprovedDigestsPresentAndAbsent(t *testing.T) {
	dir, _, mixed, _, _ := unitsFixture(t)
	runGit(t, dir, "update-ref", "refs/remotes/origin/main", mixed)

	sdir, write := testSecretsDir(t)
	writeGitHubAppSecret(t, write)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	fl.put("head", []byte(mixed))

	const prHeadSHA = "prheadshaXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"
	// projects/foo has a digest filed; hosts/h does not.
	fl.put("digests/"+prHeadSHA+"/projects-foo.digest", []byte("deadbeefdigest"))

	srv := newFakeForge(t, mixed, fakeForgeScenario{
		Approver:       "alice",
		PRMerged:       true,
		CommitVerified: true,
		PRHeadSHA:      prHeadSHA,
	})
	env := testFullEnv(sdir.Root, t.TempDir(), map[string]string{"APPROVER": "alice"})
	env = withForgeBaseURL(env, srv.URL)

	var stdout, stderr bytes.Buffer
	runEnv(context.Background(), []string{"why", mixed, "--dir", dir}, env, nil, &stdout, &stderr)
	out := stdout.String()
	if !strings.Contains(out, "digests: filed against PR head "+prHeadSHA) {
		t.Errorf("stdout = %q, want it to name the PR head sha, not the merge commit", out)
	}
	if !strings.Contains(out, "  projects/foo: present") {
		t.Errorf("stdout = %q, want projects/foo reported present", out)
	}
	if !strings.Contains(out, "  hosts/h: absent") {
		t.Errorf("stdout = %q, want hosts/h reported absent", out)
	}
}

// TestWhyApprovedDigestsCredentialsIsExempt: credentials never gets a
// digest lookup -- CI never plans that root -- and this must say so rather
// than silently drop it from the report.
func TestWhyApprovedDigestsCredentialsIsExempt(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	// A base commit first: ChangedFiles diffs sha^..sha, which has nothing
	// to diff against on a repository's very first commit (git refuses
	// "HEAD^" with no parent) -- a condition the real applier never meets
	// either, since it always walks from an existing ledger HEAD.
	commitFiles(t, dir, map[string]string{"README.md": "base\n"}, "base")
	sha := commitFiles(t, dir, map[string]string{"credentials/main.tf": "# creds\n"}, "add credentials")
	runGit(t, dir, "update-ref", "refs/remotes/origin/main", sha)

	sdir, write := testSecretsDir(t)
	writeGitHubAppSecret(t, write)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	fl.put("head", []byte(sha))

	srv := newFakeForge(t, sha, fakeForgeScenario{Approver: "alice", PRMerged: true, CommitVerified: true})
	env := testFullEnv(sdir.Root, t.TempDir(), map[string]string{"APPROVER": "alice"})
	env = withForgeBaseURL(env, srv.URL)

	var stdout, stderr bytes.Buffer
	runEnv(context.Background(), []string{"why", sha, "--dir", dir}, env, nil, &stdout, &stderr)
	out := stdout.String()
	if !strings.Contains(out, "credentials: exempt") {
		t.Errorf("stdout = %q, want credentials reported exempt, not silently dropped", out)
	}
}

// TestWhyApprovedDigestsGateWouldRefuse: the commit gate itself would
// refuse this commit (no approval), so there is no PR head sha to check
// digests against. That refusal must be reported, not swallowed into an
// empty digest section.
func TestWhyApprovedDigestsGateWouldRefuse(t *testing.T) {
	dir, _, mixed, _, _ := unitsFixture(t)
	runGit(t, dir, "update-ref", "refs/remotes/origin/main", mixed)

	sdir, write := testSecretsDir(t)
	writeGitHubAppSecret(t, write)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	fl.put("head", []byte(mixed))

	srv := newFakeForge(t, mixed, fakeForgeScenario{
		Approver:       "alice",
		PRMerged:       true,
		CommitVerified: true,
		ReviewState:    "COMMENTED", // no APPROVED review
	})
	env := testFullEnv(sdir.Root, t.TempDir(), map[string]string{"APPROVER": "alice"})
	env = withForgeBaseURL(env, srv.URL)

	var stdout, stderr bytes.Buffer
	runEnv(context.Background(), []string{"why", mixed, "--dir", dir}, env, nil, &stdout, &stderr)
	out := stdout.String()
	if !strings.Contains(out, "digests: unknown -- the commit gate would refuse") {
		t.Errorf("stdout = %q, want the gate refusal reported", out)
	}
}

// TestWhyApprovedDigestsForgeUnreachable: no GitHub App secret mounted at
// all -- buildForgeClient fails before any network call, and §4 must say
// so rather than silently print nothing.
func TestWhyApprovedDigestsForgeUnreachable(t *testing.T) {
	dir, _, mixed, _, _ := unitsFixture(t)
	runGit(t, dir, "update-ref", "refs/remotes/origin/main", mixed)
	env, _ := whyEnv(t, mixed) // no writeGitHubAppSecret

	var stdout, stderr bytes.Buffer
	runEnv(context.Background(), []string{"why", mixed, "--dir", dir}, env, nil, &stdout, &stderr)
	out := stdout.String()
	if !strings.Contains(out, "digests: unknown -- could not reach the forge") {
		t.Errorf("stdout = %q, want the forge-unreachable line", out)
	}
}

// --- degrading honestly -------------------------------------------------

// TestWhyLedgerUnreachableIsReportedNotOmitted: §1 must say the read
// failed, distinctly from "absent" (a real ErrNotFound), and exit 1 rather
// than the "queue has not reached this commit" exit 2 -- a read failure
// must never be misread as a clean negative answer.
func TestWhyLedgerUnreachableIsReportedNotOmitted(t *testing.T) {
	dir, write := testSecretsDir(t)
	write(itemLedger, fieldLedgerEndpoint, "https://ledger.invalid") // reserved TLD, never resolves
	write(itemLedger, fieldLedgerAccessKey, "AKIAFAKEACCESSKEYID")
	write(itemLedger, fieldLedgerSecretKey, "fakesecretaccesskeyfakesecretaccesskey")
	env := testFullEnv(dir.Root, t.TempDir(), nil)

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"why", "deadbeef"}, env, nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stdout: %s, stderr: %s)", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "unknown -- could not read the applied record") {
		t.Errorf("stdout = %q, want the read failure reported explicitly", stdout.String())
	}
}

// TestWhyBadDirDegradesSectionsIndependently: a --dir that is not a git
// checkout at all must not crash the command or silently blank out every
// section -- §2 and §3 each report their own failure, and §4 reports that
// it could not proceed without a resolved unit set.
func TestWhyBadDirDegradesSectionsIndependently(t *testing.T) {
	env, _ := whyEnv(t, "somehead")

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"why", "somesha", "--dir", "/nonexistent/not-a-checkout"}, env, nil, &stdout, &stderr)
	if code != 2 { // §1 is still absent -- unaffected by §2/§3's git problem
		t.Fatalf("exit code = %d, want 2 (stdout: %s)", code, stdout.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "unknown -- could not list commits") {
		t.Errorf("stdout = %q, want §2 to report its own git failure", out)
	}
	if !strings.Contains(out, "units: unknown -- could not read changed files") {
		t.Errorf("stdout = %q, want §3 to report its own git failure", out)
	}
	if !strings.Contains(out, "digests: unknown -- the units this commit selects could not be determined") {
		t.Errorf("stdout = %q, want §4 to defer to §3's failure rather than guess", out)
	}
}

// --- read-only guarantees ------------------------------------------------

// TestWhyNeverWritesToTheLedger drives a full, successful `why` report --
// every section finds something real to say, which is deliberately the
// scenario most likely to tempt a future change into "recording" what it
// found -- and asserts not one PUT ever reached the ledger. This is the
// negative-testable half of cmdWhy's own read-only doc comment.
func TestWhyNeverWritesToTheLedger(t *testing.T) {
	dir, _, mixed, _, _ := unitsFixture(t)
	runGit(t, dir, "update-ref", "refs/remotes/origin/main", mixed)

	sdir, write := testSecretsDir(t)
	writeGitHubAppSecret(t, write)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	fl.put("head", []byte(mixed)) // seeded directly -- not an HTTP PUT
	fl.put("applied/"+mixed, []byte(`{"roots":{"projects/foo":{"resource_changes":1}}}`))
	fl.put("digests/prheadsha/projects-foo.digest", []byte("deadbeefdigest"))

	srv := newFakeForge(t, mixed, fakeForgeScenario{
		Approver: "alice", PRMerged: true, CommitVerified: true, PRHeadSHA: "prheadsha",
	})
	env := testFullEnv(sdir.Root, t.TempDir(), map[string]string{"APPROVER": "alice"})
	env = withForgeBaseURL(env, srv.URL)

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"why", mixed, "--dir", dir}, env, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stdout: %s, stderr: %s)", code, stdout.String(), stderr.String())
	}
	if got := fl.puts(); got != 0 {
		t.Errorf("fakeLedger recorded %d PUT request(s), want 0 -- `truss why` must never write to the ledger", got)
	}
}

// TestWhyGitSectionsNeverWriteToGit drives §2 and §3 directly against a
// fakeGit that already instruments every write-shaped gitDriver call
// (Checkout, EnsureClone, Fetch) for the apply pass's own tests, and
// asserts none of them fired. `why` has no reason to ever call any of them
// -- it never checks out a tree and never clones -- and this is the
// negative-testable proof of that half of the read-only contract cmdWhy's
// own doc comment makes. gitDriver carries no push method at all, so "why
// must never push" is enforced by the type system rather than a counter
// here.
func TestWhyGitSectionsNeverWriteToGit(t *testing.T) {
	fl := newFakeLedger(t, "state-bucket")
	fl.put("head", []byte("c0"))
	store, err := ledger.New(ledger.Config{
		Endpoint:        fl.endpoint(),
		Bucket:          "state-bucket",
		AccessKeyID:     "AKIAFAKEACCESSKEYID",
		SecretAccessKey: "fakesecretaccesskeyfakesecretaccesskey",
	})
	if err != nil {
		t.Fatalf("ledger.New: %v", err)
	}
	journal := &ledger.Journal{Store: store, Layout: ledger.Layout{
		HeadKey: "head", AppliedPrefix: "applied", FailedPrefix: "failed", PlanDigestPrefix: "digests",
	}}

	g := &fakeGit{
		CommitsList:           []string{"c1", "c2"},
		ChangedByCommit:       map[string][]string{"c2": {"projects/foo/main.tf"}},
		TreeTofuUnitsByCommit: map[string][]string{"c2": {"projects/foo"}},
	}

	var stdout, stderr bytes.Buffer
	printQueuePosition(context.Background(), "c2", journal, g, &stdout, &stderr)
	printUnitsSelected(context.Background(), "c2", g, &stdout)

	if g.cloned() {
		t.Errorf("git was cloned, want no clone from a read-only command")
	}
	if g.fetched() {
		t.Errorf("git was fetched, want no fetch: why reads the checkout as it stands")
	}
	if len(g.checkouts) != 0 {
		t.Errorf("checkouts = %v, want none: why never checks out a tree", g.checkouts)
	}
}
