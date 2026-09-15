// Command fakebin is the PATH shim internal/parity puts in front of `tofu`
// and `git`, dispatching on the name it was invoked as. It is the Go
// counterpart of tests/applier/bin/{tofu,git} in the reference repository
// and answers out of the SAME scenario fixtures, so both implementations
// are handed the same answers a real tofu and git would give.
//
// It is a separate binary rather than a function because the pass under
// test is driven as a real subprocess: plan.Runner execs `tofu` and
// execGit execs `git`, and a fake reachable only by dependency injection
// would leave both of those code paths -- the argv they build, the exit
// codes they read, the output they scan for a held state lock -- untested
// by the harness that exists to test the whole pass (docs/port-plan.md
// §5.5, "a fake `tofu` on PATH").
//
// ⚠️ IT WRITES NOTHING TO STDOUT THAT THE REAL TOOL WOULD NOT. `tofu plan`
// and `tofu apply` print to stdout because the real ones do; a silent shim
// makes §2 item 17 (tofu's output never touches the JSON channel) vacuous,
// which is the bug the reference shim's own comment records finding on
// 2026-09-07.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// fixtures is the subset of the scenario the two shims answer from. It is
// read fresh on every invocation, exactly as the Python shims do, so there
// is no state to keep in step between calls.
type fixtures struct {
	Commits       []string            `json:"commits"`
	FilesChanged  map[string][]string `json:"files_changed"`
	Tree          map[string][]string `json:"tree"`
	TofuShow      json.RawMessage     `json:"tofu_show"`
	TofuPlanFail  bool                `json:"tofu_plan_fail"`
	TofuApplyFail bool                `json:"tofu_apply_fail"`
}

func main() {
	os.Exit(run(filepath.Base(os.Args[0]), os.Args[1:]))
}

// fixturesPath is the scenario file this invocation answers from: always
// "fixtures.json" beside the binary itself.
//
// ⚠️ IT CANNOT BE AN ENVIRONMENT VARIABLE, AND THAT IS THE PASS BEHAVING
// CORRECTLY. plan.Runner builds its child's environment EXPLICITLY and
// never inherits os.Environ (§2 item 9 -- the leak fixed on 2026-09-07),
// so `tofu` is handed PATH, HOME and the root's own credentials and
// nothing else. A shim that needed a variable would therefore have needed
// the pass to leak one to reach it, which is precisely the property under
// test. Beside the binary is the one channel that survives: the harness
// gives each scenario its own bin directory.

func run(name string, args []string) int {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakebin: %v\n", err)
		return 127
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(self), "fixtures.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakebin: %v\n", err)
		return 127
	}
	var f fixtures
	if err := json.Unmarshal(raw, &f); err != nil {
		fmt.Fprintf(os.Stderr, "fakebin: %v\n", err)
		return 127
	}

	switch name {
	case "tofu":
		return tofu(f, args)
	case "git":
		return git(f, args)
	default:
		fmt.Fprintf(os.Stderr, "fakebin: invoked as %q, which is neither tofu nor git\n", name)
		return 127
	}
}

func tofu(f fixtures, args []string) int {
	if len(args) == 0 {
		return 0
	}
	switch args[0] {
	case "plan":
		if f.TofuPlanFail {
			fmt.Fprintln(os.Stderr, "tofu shim: simulated plan failure")
			return 1
		}
		// The drift path asks for -detailed-exitcode, where 0 means "no
		// changes" and 2 means "the code and the deployed state disagree".
		// The reference shim answers 0, so a drift scenario reports no
		// drift unless plan is made to fail outright; matching it here
		// keeps the two harnesses answering the same question.
		fmt.Println("OpenTofu will perform the following actions:\n\nPlan: 1 to add, 0 to change, 0 to destroy.")
		return 0
	case "apply":
		if f.TofuApplyFail {
			fmt.Fprintln(os.Stderr, "tofu shim: simulated apply failure")
			return 1
		}
		fmt.Println("Apply complete! Resources: 1 added, 0 changed, 0 destroyed.")
		return 0
	case "show":
		show := f.TofuShow
		if len(show) == 0 {
			show = json.RawMessage(`{"resource_changes":[]}`)
		}
		// plan.Declarations refuses a document with no "configuration" key
		// outright -- real `tofu show -json` always carries one -- and the
		// corpus was recorded before that gate existed, so no fixture in it
		// carries one either. None of these recordings are about a
		// provisioner or a forbidden resource type, so the honest answer for
		// every one of them is "declares nothing", supplied here rather than
		// by editing 43 recorded fixtures to say the same thing by hand.
		show = ensureConfiguration(show)
		os.Stdout.Write(show)
		fmt.Println()
		return 0
	default: // init, validate, fmt
		return 0
	}
}

func git(f fixtures, args []string) int {
	switch {
	case has(args, "rev-list"):
		// execGit asks `rev-list --reverse --first-parent <from>..<to>`.
		// The reference shim ignores the range too: a scenario declares the
		// queue directly, because what is being tested is what the pass
		// does with a queue, not git's revision walk.
		for _, c := range f.Commits {
			fmt.Println(c)
		}
		return 0
	case has(args, "diff") && has(args, "--name-only"):
		// `diff --name-only <sha>^ <sha>` -- the sha is the last argument.
		sha := args[len(args)-1]
		for _, p := range f.FilesChanged[sha] {
			fmt.Println(p)
		}
		return 0
	case has(args, "ls-tree"):
		// The sha sits immediately before the "--" pathspec separator,
		// whatever the flag count -- execGit shapes this call two
		// different ways across its two callers (TreeRoots: `-d --name-only`;
		// TreeTofuUnits: `-d -r --name-only`, cmd/truss/git.go), and a fixed
		// offset from "ls-tree" read the flag itself as the sha the moment a
		// second one (`-r`) was added.
		//
		// ⚠️ THIS WAS WRONG, AND NOTHING CAUGHT IT AT FIRST. The wrong "sha"
		// missed f.Tree and fell back to the hard-coded default (platform,
		// projects/recipes) -- filtered to KindTofu (TreeTofuUnits) both
		// default entries pass the filter, so the wrong answer looked like a
		// plausible one instead of an empty one: it took a scenario whose
		// declared tree carried a THIRD tofu root to surface it, because
		// only then did the default and the real answer disagree in a way
		// downstream filtering could not hide. Found by
		// test_shared_input_change_plans_and_applies_every_root_in_the_tree
		// going red the moment TreeTofuUnits started asking `-r` questions.
		sep := index(args, "--")
		if sep < 1 {
			fmt.Fprintln(os.Stderr, "fakebin: ls-tree call carries no -- pathspec separator")
			return 1
		}
		sha := args[sep-1]
		roots, ok := f.Tree[sha]
		if !ok {
			// The roots the harness creates on disk, matching the
			// reference shim's own default: a scenario only declares
			// `tree` when it is about a project appearing or vanishing.
			//
			// ⚠️ THE PATHSPEC AFTER `--` IS DELIBERATELY IGNORED, AND THAT
			// IS SAFE ONLY BECAUSE EVERY CALLER FILTERS WITH repo.KindOf.
			// This shim answers every ls-tree with the same listing, so
			// TreeRoots gets "platform" and "projects/recipes" verbatim and
			// TreeTofuUnits filters them through KindOf, which is the right
			// answer for every scenario this corpus records. A caller that
			// trusted the pathspec instead of the filter would get a wrong
			// answer here and nothing would say so.
			roots = []string{"platform", "projects/recipes"}
		}
		for _, r := range roots {
			fmt.Println(r)
		}
		return 0
	default: // clone, fetch, checkout: nothing reads their output
		return 0
	}
}

// ensureConfiguration adds an empty "configuration" key to raw when it does
// not already carry one, leaving every other key untouched. Used only to
// backfill the recorded corpus's `tofu show` fixtures -- see the ⚠️ at the
// one call site.
func ensureConfiguration(raw json.RawMessage) json.RawMessage {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return raw
	}
	if _, ok := doc["configuration"]; ok {
		return raw
	}
	doc["configuration"] = json.RawMessage(`{"root_module":{}}`)
	out, err := json.Marshal(doc)
	if err != nil {
		return raw
	}
	return out
}

func has(args []string, want string) bool { return index(args, want) >= 0 }

func index(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}
