package main

import (
	"context"
	"fmt"
	"io"

	"github.com/beeradb/truss/internal/repo"
)

// cmdUnits prints the units a commit touches, one per line, as
// "<kind>\t<path>" -- kind being repo.Kind's String(): credentials or tofu.
// It derives a commit's units by feeding the exact same inputs --
// ChangedFiles and TreeTofuUnits -- into the exact same repo.TouchedUnits the
// apply pass calls, rather than a consumer's CI job maintaining its own
// approximation of the rule.
//
// ⚠️ IT TAKES NO config.Config, NO getenv, AND NO CREDENTIAL. CI runs this,
// and CI is read-only by construction: it needs a git checkout and nothing
// else. The working directory comes from --dir (default "."), never the
// applier's Workdir, and there is no forge client or ledger to wire up --
// cmd/truss/digest_cmd.go is the model for a subcommand shaped this way.
//
// Order is exactly repo.TouchedUnits' own order -- kind, then path -- because
// that order is the execution order and a CI job filing digests in some
// other order would itself be a second way to say one thing, the same class
// of drift internal/plan/digest.go's own comment warns about for the jq it
// mirrors.
func cmdUnits(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const usageMsg = "usage: truss units <sha> [--dir <path>] [--kind credentials|tofu]"

	dir := "."
	var sha, kindFlag string
	shaSet, kindSet := false, false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dir":
			if i+1 >= len(args) {
				fmt.Fprintln(stderr, usageMsg)
				return 2
			}
			i++
			dir = args[i]
		case "--kind":
			if i+1 >= len(args) {
				fmt.Fprintln(stderr, usageMsg)
				return 2
			}
			i++
			kindFlag, kindSet = args[i], true
		default:
			if shaSet {
				fmt.Fprintln(stderr, usageMsg)
				return 2
			}
			sha, shaSet = args[i], true
		}
	}
	if !shaSet {
		fmt.Fprintln(stderr, usageMsg)
		return 2
	}

	var wantKind repo.Kind
	if kindSet {
		k, ok := kindFromString(kindFlag)
		if !ok {
			fmt.Fprintf(stderr, "units: unknown --kind %q (want credentials or tofu)\n", kindFlag)
			return 2
		}
		wantKind = k
	}

	// The same driver and the same two calls tofuUnitsFor is fed from in the
	// apply pass (apply_cmd.go): ChangedFiles for what the commit changed,
	// TreeTofuUnits for what tofu units exist in its own tree. No token:
	// this reads a checkout already on disk and nothing on the network.
	g := execGit{Bin: "git", Dir: dir, Stderr: stderr}
	changedFiles, err := g.ChangedFiles(ctx, sha)
	if err != nil {
		fmt.Fprintf(stderr, "units: %v\n", err)
		return 1
	}
	treeTofuUnits, err := g.TreeTofuUnits(ctx, sha)
	if err != nil {
		fmt.Fprintf(stderr, "units: %v\n", err)
		return 1
	}

	for _, u := range repo.TouchedUnits(changedFiles, treeTofuUnits) {
		if kindSet && u.Kind != wantKind {
			continue
		}
		fmt.Fprintf(stdout, "%s\t%s\n", u.Kind, u.Path)
	}
	return 0
}

// kindFromString parses --kind's argument. It is the inverse of repo.Kind's
// String(), kept here rather than in internal/repo because it is a CLI
// concern (a flag value), not a fact about what a unit is.
func kindFromString(s string) (repo.Kind, bool) {
	switch s {
	case "credentials":
		return repo.KindCredentials, true
	case "tofu":
		return repo.KindTofu, true
	}
	return 0, false
}
