package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// commitFiles writes files (relative path -> content, creating parent
// directories as needed) into dir, stages everything and commits, returning
// the new commit's sha. It builds a real git history the way
// git_tofu_units_test.go's TestExecGitTreeTofuUnitsAgainstARealRepo already
// does, and reuses that file's runGit helper rather than defining a second
// one in the same package.
//
// ⚠️ A FIXED IDENTITY WITH NO @, NOT WHATEVER THE MACHINE HAS CONFIGURED.
// AGENTS.md's own rule is that a test must not depend on the machine running
// it, and git accepts an identity that is not a well-formed address (the
// commit lands and %ae reads it back verbatim) -- which matters here because
// scripts/leakscan refuses anything shaped like an email anywhere in this
// repository and cannot tell a fixture's address from a real one.
func commitFiles(t *testing.T, dir string, files map[string]string, msg string) string {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	runGit(t, dir, "add", "-A")
	cmd := exec.Command("git", "-C", dir, "commit", "--quiet", "-m", msg)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=truss-test", "GIT_AUTHOR_EMAIL=truss-test",
		"GIT_COMMITTER_NAME=truss-test", "GIT_COMMITTER_EMAIL=truss-test",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	return strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
}

// unitsFixture builds one real repository whose commit history exercises
// every case this file covers, so every sub-test shares one tree instead of
// each reconstructing its own.
//
//   - base:   a pre-existing tofu unit (clusters/beta), so later commits
//     have a tree that already contains something besides what they change.
//   - mixed:  adds one project root and one host root (both tofu) and
//     touches credentials in the same commit -- the ordinary, non-shared
//     case, and it exercises TouchedUnits' fixed kind-then-path order
//     (credentials before tofu, hosts/h before projects/foo).
//   - shared: touches only modules/vpc/main.tf, a shared input. The tree at
//     this commit still holds both tofu units (clusters/beta and
//     projects/foo), which is the asymmetry the whole command exists to
//     fix: CI must be told about both, not just the ones this commit's diff
//     names.
//   - docsOnly: touches only docs/, no unit at all.
func unitsFixture(t *testing.T) (dir string, base, mixed, shared, docsOnly string) {
	t.Helper()
	dir = t.TempDir()
	runGit(t, dir, "init", "--quiet")

	base = commitFiles(t, dir, map[string]string{
		"clusters/beta/main.tf": "# a cluster root\n",
		"README.md":             "root\n",
	}, "base")

	mixed = commitFiles(t, dir, map[string]string{
		"projects/foo/main.tf": "# a project root\n",
		"hosts/h/main.tf":      "# a host root\n",
		"credentials/aws.tf":   "# credentials\n",
	}, "mixed")

	shared = commitFiles(t, dir, map[string]string{
		"modules/vpc/main.tf": "# a shared module\n",
	}, "shared input")

	docsOnly = commitFiles(t, dir, map[string]string{
		"docs/notes.md": "just docs\n",
	}, "docs only")

	return dir, base, mixed, shared, docsOnly
}

// TestUnitsMixedCommitReturnsAllKindsInOrder covers the ordinary,
// non-shared-input path: a commit touching credentials and one project root
// must report both, in repo.TouchedUnits' fixed kind order (credentials,
// then tofu) -- never alphabetically, which is what the deliberate-break
// step below proves by breaking it.
func TestUnitsMixedCommitReturnsAllKindsInOrder(t *testing.T) {
	dir, _, mixed, _, _ := unitsFixture(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"units", mixed, "--dir", dir}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr.String())
	}

	want := "credentials\tcredentials\n" +
		"tofu\thosts/h\n" +
		"tofu\tprojects/foo\n"
	if stdout.String() != want {
		t.Fatalf("stdout =\n%q\nwant\n%q", stdout.String(), want)
	}
}

// TestUnitsKindFilterRestrictsToCredentialsOnly checks --kind credentials.
func TestUnitsKindFilterRestrictsToCredentialsOnly(t *testing.T) {
	dir, _, mixed, _, _ := unitsFixture(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"units", mixed, "--dir", dir, "--kind", "credentials"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr.String())
	}

	want := "credentials\tcredentials\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

// TestUnitsSharedInputReturnsEveryTofuUnitInTheTree is the case that
// motivated the command. modules/vpc/main.tf is a shared input
// (internal/repo/units.go's unitSharedInput), so the commit that only
// touches it must report every tofu unit that exists in the tree --
// clusters/beta (present since the base commit, untouched by this diff)
// and hosts/h and projects/foo (added by the previous commit) -- not only
// units this commit's own diff names. Without this case the command cannot
// be shown to close the asymmetry a widened selection depends on.
func TestUnitsSharedInputReturnsEveryTofuUnitInTheTree(t *testing.T) {
	dir, _, _, shared, _ := unitsFixture(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"units", shared, "--dir", dir}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr.String())
	}

	want := "tofu\tclusters/beta\n" +
		"tofu\thosts/h\n" +
		"tofu\tprojects/foo\n"
	if stdout.String() != want {
		t.Fatalf("stdout =\n%q\nwant\n%q (every tofu unit in the tree, not just the ones this commit's diff names)", stdout.String(), want)
	}
}

// TestUnitsDocsOnlyCommitPrintsNothing checks the "no unit touched" case
// exits 0 with empty output -- information, not an error.
func TestUnitsDocsOnlyCommitPrintsNothing(t *testing.T) {
	dir, _, _, _, docsOnly := unitsFixture(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"units", docsOnly, "--dir", dir}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

// TestUnitsBadShaExitsOne checks a sha git cannot resolve is a git failure
// (exit 1), not a usage error.
func TestUnitsBadShaExitsOne(t *testing.T) {
	dir, _, _, _, _ := unitsFixture(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"units", "nosuchcommit", "--dir", dir}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stdout: %q stderr: %q)", code, stdout.String(), stderr.String())
	}
}

// TestUnitsUsageErrorsExitTwo covers every usage mistake this subcommand
// recognises: no sha, two shas, and an unknown --kind value.
func TestUnitsUsageErrorsExitTwo(t *testing.T) {
	dir, _, mixed, _, _ := unitsFixture(t)

	cases := [][]string{
		{"units"},
		{"units", mixed, "extra-arg", "--dir", dir},
		{"units", mixed, "--dir", dir, "--kind", "bogus"},
	}
	for _, args := range cases {
		var stdout, stderr bytes.Buffer
		code := run(args, strings.NewReader(""), &stdout, &stderr)
		if code != 2 {
			t.Errorf("run(%v) = %d, want 2 (stdout: %q stderr: %q)", args, code, stdout.String(), stderr.String())
		}
	}
}
