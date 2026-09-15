package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/beeradb/truss/internal/config"
	"github.com/beeradb/truss/internal/ledger"
	"github.com/beeradb/truss/internal/repo"
)

// cmdWhy implements `truss why <sha>` (docs/work-items.md:86-133, extended
// by the 2026-09 incident write-up): explain everything the system knows
// about one commit's fate, in one report, so answering "why is commit X not
// applied?" stops meaning a hand correlation across five different sources
// (applier pod logs, truss_queue_depth, GCS ledger keys, GitHub commit
// statuses and Kubernetes job history).
//
// Four questions, always in this order, and each one is printed regardless
// of whether the ones before it found anything -- a commit the queue has
// not reached yet is the PRIMARY case this command exists for, and that is
// exactly the case where §1 has nothing to say and §2-§4 carry the answer:
//
//  1. Is it applied? -- the ledger record, if one exists.
//  2. Where is the queue relative to it? -- ledger HEAD, and ahead/at/behind.
//  3. Which units does it select? -- tofu roots, derived the same way the
//     apply pass derives them.
//  4. Is there an approved plan digest for it, per root?
//
// ⚠️ READ-ONLY, AND THAT IS THE WHOLE POINT OF A COMMAND MEANT TO BE SAFE TO
// RUN AGAINST A STUCK APPLIER. It never writes to the ledger (only
// ledger.Journal.Head, ledger.Journal.ApprovedDigest and the two raw
// store.Get calls in printLedgerRecord are ever called -- no Put*, no
// AdvanceHead), never takes the OpenTofu state lock (it never builds a
// tofuFactory or calls a tofuRunner at all -- there is no code path here
// that CAN reach `tofu init/plan/apply`), and never applies anything. §2 and
// §3 read a local git checkout (--dir, default ".", the same convention
// `truss units` already uses) and §4 reads the forge for the PR's head sha
// -- both GET-shaped, and gitDriver.PushRef (the one write that interface
// exposes) is never called from here. See why_cmd_test.go's
// TestWhyGitSectionsNeverWriteToGit and TestWhyNeverWritesToTheLedger.
//
// ⚠️ IT DOES NOT FETCH. §2 and §3 read whatever origin/main already resolves
// to in the checkout at --dir, the same assumption `truss units` already
// makes about its own <sha> argument. A stale checkout gives a stale answer
// -- run `git fetch` first if the queue position looks wrong.
//
// Exit code is driven by §1 alone, the same three-way contract `ledger get`
// already uses (§4.9): 0 a record was found, 2 no record under either key
// ("the queue has not reached this commit" -- information, not an error), 1
// anything else (a config problem, an unreachable ledger, or a record that
// exists but will not parse). §2-§4 are context around that answer, printed
// on stdout with an "unknown"/"n/a" line whenever they could not be
// answered -- never omitted, because an omitted line reads as a negative
// answer and that is the exact failure mode this command exists to fix.
func cmdWhy(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	sha, dir, usageErr := parseWhyArgs(args)
	if usageErr != "" {
		fmt.Fprintln(stderr, usageErr)
		return 2
	}

	cfg, problems := config.Load(getenv)
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(stderr, p)
		}
		return 1
	}
	store, err := buildLedgerStore(cfg)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	layout := layoutFor(cfg)
	journal := &ledger.Journal{Store: store, Layout: layout}
	g := gitDriver(execGit{Bin: "git", Dir: dir, Stderr: stderr})

	// §1 decides the exit code; §2-§4 are printed unconditionally below and
	// never change it -- a queue position or a missing digest is context
	// around the verdict, not a second verdict.
	exitCode := printLedgerRecord(ctx, sha, store, layout, stdout)

	fmt.Fprintln(stdout)
	printQueuePosition(ctx, sha, journal, g, stdout, stderr)

	fmt.Fprintln(stdout)
	roots, unitsErr := printUnitsSelected(ctx, sha, g, stdout)

	fmt.Fprintln(stdout)
	printApprovedDigests(ctx, sha, cfg, getenv, journal, roots, unitsErr, stdout)

	return exitCode
}

// parseWhyArgs reads the positional <sha> and the optional "--dir <path>"
// flag, matching cmdUnits' own flag shape (units_cmd.go) so the two
// git-reading subcommands agree on how a caller points them at a checkout.
// dir defaults to ".": run `truss why <sha>` from inside your own clone of
// the platform repository, the same way you would run `truss units`.
func parseWhyArgs(args []string) (sha, dir, usageErr string) {
	const usageMsg = "usage: truss why <sha> [--dir <path>]"
	dir = "."
	shaSet := false
	for i := 0; i < len(args); i++ {
		if args[i] == "--dir" {
			if i+1 >= len(args) {
				return "", "", usageMsg
			}
			i++
			dir = args[i]
			continue
		}
		if shaSet {
			return "", "", usageMsg
		}
		sha, shaSet = args[i], true
	}
	if !shaSet {
		return "", "", usageMsg
	}
	return sha, dir, ""
}

// printLedgerRecord answers §1, "is it applied?": the ledger record under
// applied/<sha> (a normal apply, a noop, or a `truss skip` record -- see
// printWhyApplied) or failed/<sha>, whichever exists, or an explicit
// "absent" line when neither does. Returns the three-way exit code
// described on cmdWhy's own doc.
func printLedgerRecord(ctx context.Context, sha string, store *ledger.Store, layout ledger.Layout, stdout io.Writer) int {
	appliedBody, appliedErr := store.Get(ctx, layout.AppliedKey(sha))
	if appliedErr == nil {
		return printWhyApplied(sha, appliedBody, stdout)
	}
	if !errors.Is(appliedErr, ledger.ErrNotFound) {
		fmt.Fprintf(stdout, "%s: unknown -- could not read the applied record: %v\n", sha, appliedErr)
		return 1
	}

	failedBody, failedErr := store.Get(ctx, layout.FailedKey(sha))
	if failedErr == nil {
		return printWhyFailed(sha, failedBody, stdout)
	}
	if !errors.Is(failedErr, ledger.ErrNotFound) {
		fmt.Fprintf(stdout, "%s: unknown -- could not read the failed record: %v\n", sha, failedErr)
		return 1
	}

	// Neither applied/<sha> nor failed/<sha> exists: the queue simply has
	// not reached this commit yet. That is information about where the
	// queue is, not a failure of this command -- the same exit code
	// `ledger get` already uses for "absent" (§4.9) -- and it is the
	// headline case §2-§4 below exist to explain.
	fmt.Fprintf(stdout, "%s: absent -- no record under %s or %s; the queue has not reached this commit\n", sha, layout.AppliedPrefix, layout.FailedPrefix)
	return 2
}

// printWhyApplied decodes applied/<sha>'s body, which is one of three
// shapes truss itself ever writes there -- a normal apply (`{"roots":...}`,
// ledger.RootSummary per root), a noop (`{"noop":true}`), or a skip
// (`{"skipped":true,"reason":...,"at":...}`, written by `truss skip`) --
// and prints whichever it finds.
func printWhyApplied(sha string, body []byte, stdout io.Writer) int {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		fmt.Fprintf(stdout, "%s: unknown -- could not parse the applied record: %v\n", sha, err)
		return 1
	}

	if _, ok := probe["skipped"]; ok {
		var rec struct {
			Reason string `json:"reason"`
			At     string `json:"at"`
		}
		if err := json.Unmarshal(body, &rec); err != nil {
			fmt.Fprintf(stdout, "%s: unknown -- could not parse the skipped record: %v\n", sha, err)
			return 1
		}
		fmt.Fprintf(stdout, "%s: skipped\n", sha)
		fmt.Fprintf(stdout, "  reason: %s\n", rec.Reason)
		fmt.Fprintf(stdout, "  at:     %s\n", rec.At)
		return 0
	}

	if _, ok := probe["noop"]; ok {
		fmt.Fprintf(stdout, "%s: applied, no changes (noop)\n", sha)
		return 0
	}

	if rootsRaw, ok := probe["roots"]; ok {
		var roots map[string]ledger.RootSummary
		if err := json.Unmarshal(rootsRaw, &roots); err != nil {
			fmt.Fprintf(stdout, "%s: unknown -- could not parse the applied record's roots: %v\n", sha, err)
			return 1
		}
		fmt.Fprintf(stdout, "%s: applied\n", sha)
		names := make([]string, 0, len(roots))
		for name := range roots {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			rs := roots[name]
			// A nil ResourceChanges is summary_from_plan's own "could not
			// parse `tofu show -json`" fallback (ledger.RootSummary's doc) --
			// it must print as unknown, never as 0, because 0 means "plan
			// had no changes" and nil means "nobody knows what the plan had".
			if rs.ResourceChanges == nil {
				fmt.Fprintf(stdout, "  %s: unknown resource changes\n", name)
			} else {
				fmt.Fprintf(stdout, "  %s: %d resource change(s)\n", name, *rs.ResourceChanges)
			}
		}
		return 0
	}

	fmt.Fprintf(stdout, "%s: unknown -- the applied record has none of the shapes truss writes there (skipped, noop, roots)\n", sha)
	return 1
}

// printWhyFailed decodes failed/<sha>'s body -- reason then at, matching
// ledger.Journal.PutFailed -- and prints it.
func printWhyFailed(sha string, body []byte, stdout io.Writer) int {
	var rec struct {
		Reason string `json:"reason"`
		At     string `json:"at"`
	}
	if err := json.Unmarshal(body, &rec); err != nil {
		fmt.Fprintf(stdout, "%s: unknown -- could not parse the failed record: %v\n", sha, err)
		return 1
	}
	fmt.Fprintf(stdout, "%s: failed\n", sha)
	fmt.Fprintf(stdout, "  reason: %s\n", rec.Reason)
	fmt.Fprintf(stdout, "  at:     %s\n", rec.At)
	return 0
}

// printQueuePosition answers §2: where is the queue relative to this
// commit? It reads the ledger's own HEAD, and unless sha IS head, walks the
// SAME rev-list the apply pass itself walks -- runCommitLoop's own
// `d.Git.Commits(ctx, last, "origin/main")` call (apply_cmd.go) -- so
// "ahead" here means exactly what it means to the pass, never a second
// notion of it.
//
// If sha is not HEAD and not found ahead of it, the only other honest
// possibility is that the queue already passed it, and that is CONFIRMED --
// by checking whether HEAD itself shows up walking forward from sha, the
// same Commits call run the other direction -- rather than assumed from an
// absence. A sha that resolves as neither says so plainly instead of
// guessing a side.
func printQueuePosition(ctx context.Context, sha string, journal *ledger.Journal, g gitDriver, stdout, stderr io.Writer) {
	head, err := journal.Head(ctx)
	if err != nil {
		if errors.Is(err, ledger.ErrNotFound) {
			fmt.Fprintln(stdout, "queue: unknown -- the ledger has no HEAD; nothing has ever run")
		} else {
			fmt.Fprintf(stdout, "queue: unknown -- could not read HEAD from the ledger: %v\n", err)
		}
		return
	}
	fmt.Fprintf(stdout, "queue: HEAD is %s\n", head)

	if sha == head {
		fmt.Fprintln(stdout, "  at HEAD -- this is the last commit the applier advanced past")
		return
	}

	ahead, err := g.Commits(ctx, head, "origin/main")
	if err != nil {
		fmt.Fprintf(stdout, "  unknown -- could not list commits from HEAD to origin/main: %v\n", err)
		return
	}
	for i, c := range ahead {
		if c != sha {
			continue
		}
		if i == 0 {
			fmt.Fprintln(stdout, "  ahead of HEAD: it is next in the queue")
		} else {
			fmt.Fprintf(stdout, "  ahead of HEAD: %d commit(s) are queued in front of it, currently blocking on %s\n", i, ahead[0])
		}
		return
	}

	afterSHA, err := g.Commits(ctx, sha, "origin/main")
	if err != nil {
		fmt.Fprintf(stdout, "  unknown -- %s does not resolve against origin/main: %v\n", sha, err)
		return
	}
	for _, c := range afterSHA {
		if c == head {
			fmt.Fprintln(stdout, "  behind HEAD -- the queue has already passed this commit")
			return
		}
	}
	fmt.Fprintln(stdout, "  unknown -- this commit is not on the first-parent chain between HEAD and origin/main in either direction; cannot place it relative to the queue")
}

// printUnitsSelected answers §3: which units does this commit select? --
// the tofu roots, derived by feeding the SAME changed-files diff and the
// SAME tree listing runCommitLoop reads (apply_cmd.go) through the SAME
// function, tofuUnitsFor. There is no second derivation here to drift from
// the pass's own -- see that function's own doc for why it reads its own
// tree listing rather than a combined one.
//
// It names the unitSharedInput effect explicitly when it fires: a commit
// touching only modules/foo.tf selects every unit that exists in the tree,
// not just the ones whose own files changed, and that is a surprise worth
// a reader being told rather than left to notice from an unusually long
// list.
//
// Returns the tofu-and-credentials roots so §4 can ask about their digests
// without re-deriving them, and the error from whichever git read failed
// first (nil when both the tree listing and the diff were read).
func printUnitsSelected(ctx context.Context, sha string, g gitDriver, stdout io.Writer) (roots []string, err error) {
	changedFiles, err := g.ChangedFiles(ctx, sha)
	if err != nil {
		fmt.Fprintf(stdout, "units: unknown -- could not read changed files for %s: %v\n", sha, err)
		return nil, err
	}
	treeTofu, err := g.TreeTofuUnits(ctx, sha)
	if err != nil {
		fmt.Fprintf(stdout, "units: unknown -- could not read the tofu units at %s: %v\n", sha, err)
		return nil, err
	}

	roots = tofuUnitsFor(changedFiles, treeTofu)

	if repo.SharedInputTouched(changedFiles) {
		fmt.Fprintln(stdout, "units: this commit touches a shared input (modules/, providers.allow or .opentofu-version) -- EVERY unit that exists in this commit's own tree is selected, not only the ones whose files changed")
	} else {
		fmt.Fprintln(stdout, "units:")
	}

	if len(roots) == 0 {
		fmt.Fprintln(stdout, "  none -- this commit is a noop")
		return roots, nil
	}
	for _, r := range roots {
		fmt.Fprintf(stdout, "  tofu:    %s\n", r)
	}
	return roots, nil
}

// printApprovedDigests answers §4: is there an approved plan digest for
// this commit, per root? The digest CI files is keyed by the PR's HEAD sha,
// never the merge commit sha in the queue -- checkCommitGate's own doc:
// "CI planned the PR's HEAD, not the merge commit, and the plan digest is
// filed under that sha" -- so this runs the exact commit gate the apply
// pass itself runs before it will even derive roots (apply_cmd.go's
// runCommitLoop), purely to learn headSHA. If that gate would refuse the
// commit outright, THAT is reported here instead of a digest lookup that
// would only mislead: a commit with no approved, merged, verified PR has no
// PR head sha to check digests against, and no digest will ever apply it
// regardless of what is filed.
//
// credentials is named present-or-absent from nothing: §2 item 10 exempts
// it from the digest gate entirely, because CI never plans that root.
func printApprovedDigests(ctx context.Context, sha string, cfg config.Config, getenv func(string) string, journal *ledger.Journal, roots []string, unitsErr error, stdout io.Writer) {
	if unitsErr != nil {
		fmt.Fprintln(stdout, "digests: unknown -- the units this commit selects could not be determined (see above)")
		return
	}

	var tofuRoots []string
	credentialsTouched := false
	for _, r := range roots {
		if r == "credentials" {
			credentialsTouched = true
			continue
		}
		tofuRoots = append(tofuRoots, r)
	}
	if len(tofuRoots) == 0 && !credentialsTouched {
		fmt.Fprintln(stdout, "digests: n/a -- this commit selects no tofu root")
		return
	}

	client, err := buildForgeClient(cfg, getenv)
	if err != nil {
		fmt.Fprintf(stdout, "digests: unknown -- could not reach the forge to resolve the PR head sha: %v\n", err)
		return
	}
	headSHA, _, reason, ioErr := checkCommitGate(ctx, client, cfg.Approver, sha)
	if ioErr != nil {
		fmt.Fprintf(stdout, "digests: unknown -- could not determine the PR head sha for %s: %v\n", sha, ioErr)
		return
	}
	if reason != "" {
		fmt.Fprintf(stdout, "digests: unknown -- the commit gate would refuse %s before any digest is checked: %s\n", sha, reason)
		return
	}

	fmt.Fprintf(stdout, "digests: filed against PR head %s\n", headSHA)
	if credentialsTouched {
		fmt.Fprintln(stdout, "  credentials: exempt -- CI never plans this root, so there is nothing to have approved")
	}
	for _, r := range tofuRoots {
		printOneDigest(ctx, journal, headSHA, r, "", stdout)
	}
}

// printOneDigest reports whether journal.ApprovedDigest finds a digest for
// unit at headSHA -- present, absent (ledger.ErrNotFound, the ordinary "CI
// has not filed one yet" case), or unknown (any other read error, reported
// rather than folded into "absent" so a broken ledger read never looks like
// a clean absence).
func printOneDigest(ctx context.Context, journal *ledger.Journal, headSHA, unit, label string, stdout io.Writer) {
	_, err := journal.ApprovedDigest(ctx, headSHA, unit)
	switch {
	case err == nil:
		fmt.Fprintf(stdout, "  %s%s: present\n", label, unit)
	case errors.Is(err, ledger.ErrNotFound):
		fmt.Fprintf(stdout, "  %s%s: absent\n", label, unit)
	default:
		fmt.Fprintf(stdout, "  %s%s: unknown -- could not read: %v\n", label, unit, err)
	}
}
