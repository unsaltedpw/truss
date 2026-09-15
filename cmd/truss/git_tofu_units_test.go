package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestExecGitTreeTofuUnitsAgainstARealRepo is the one direct test of
// execGit.TreeTofuUnits against an actual `git` binary, for the
// credentials/tofu half of the tree listing. Before TreeTofuUnits existed,
// no gitDriver method could answer "what tofu units does this tree contain"
// for clusters/<name> or hosts/<name> at all -- runCommitLoop derived tofu
// work from repo.TouchedRoots alone, which only ever asked git about
// "platform" and "projects/".
//
// ⚠️ IT RUNS THE REAL BINARY AND MUST FAIL, NEVER SKIP, IF GIT IS ABSENT: a
// check nobody has watched fail is a claim, and a t.Skip on a missing tool
// would report "passing" on a machine where this never ran.
//
// The tree exercises every prefix TreeTofuUnits lists: clusters/beta and
// hosts/h are real KindTofu units, platform and projects/p round out the
// other two, and clusters/README.md is a decoy FILE directly under
// clusters/ -- not a directory, so `-d` must already exclude it, and even
// if it were read it names no unit (repo.KindOf requires a name after the
// prefix).
func TestExecGitTreeTofuUnitsAgainstARealRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is not on PATH: %v -- this check must fail, not skip, when its own tool is missing", err)
	}

	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")

	write := func(rel, body string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("clusters/beta/main.tf", "# a tofu root\n")
	write("hosts/h/main.tf", "# a tofu root\n")
	write("platform/x.tf", "# a tofu root\n")
	write("projects/p/main.tf", "# a tofu root\n")
	write("clusters/README.md", "not a unit, a file directly under clusters/\n") // decoy: file, names no unit

	runGit(t, dir, "add", "-A")
	// Env rather than a repo-local `git config`, and a fixed identity rather
	// than whatever happens to be configured -- AGENTS.md's rule is that a
	// test must not depend on the machine running it, and GIT_AUTHOR_EMAIL
	// carries no "@" because scripts/leakscan refuses anything shaped like an
	// email address anywhere in this repository, fixtures included.
	commit := exec.Command("git", "-C", dir, "commit", "--quiet", "-m", "fixture")
	commit.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=truss-test", "GIT_AUTHOR_EMAIL=truss-test",
		"GIT_COMMITTER_NAME=truss-test", "GIT_COMMITTER_EMAIL=truss-test",
	)
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	sha := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))

	g := execGit{Bin: "git", Dir: dir, Stderr: io.Discard}
	got, err := g.TreeTofuUnits(context.Background(), sha)
	if err != nil {
		t.Fatalf("TreeTofuUnits: %v", err)
	}
	sort.Strings(got)
	want := []string{"clusters/beta", "hosts/h", "platform", "projects/p"}
	if len(got) != len(want) {
		t.Fatalf("TreeTofuUnits = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("TreeTofuUnits = %v, want exactly %v", got, want)
		}
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
