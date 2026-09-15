package main

import (
	"context"
	"fmt"
	"io"
	"os"
)

// subcommands is the documented set from docs/port-plan.md §4.9, plus
// status, why and skip -- the local operator CLI docs/work-items.md:86-133
// asks for, which post-dates that table (§4.9 has been updated to say so).
// `units` post-dates that too: it prints the units a commit touches, so CI
// and the applier derive the set from one implementation instead of a
// consumer's CI reimplementing the rule.
// TestSubcommandsAreExactlyTheDocumentedSet reads this slice directly rather
// than re-deriving it, so adding a subcommand here is the one place that
// needs to change for that test to see it.
var subcommands = []string{
	"ledger",
	"plan-digest",
	"token",
	"gate",
	"expiry",
	"notify",
	"apply",
	"publish",
	"status",
	"why",
	"skip",
	"units",
}

func isSubcommand(name string) bool {
	for _, s := range subcommands {
		if s == name {
			return true
		}
	}
	return false
}

// usage is printed verbatim on stderr for an empty or unrecognised
// subcommand. It names every subcommand and nothing else -- in particular
// never any part of the environment or the arguments it was called with,
// which is exactly the thing TestNoSubcommandPrintsASecret exists to catch
// a regression of.
const usage = `usage: truss <subcommand> [args]

subcommands:
  ledger get <key>     print a ledger object to stdout (exit 2 if absent)
  ledger put <key>     write stdin to a ledger key
  plan-digest           read a tofu plan (stdin) and print its digest
  token                 mint a GitHub App installation token
  gate protection       check main's branch protection
  gate commit <sha>     check a commit's PR approval and merge provenance
  expiry                sweep credential expiry
  notify                compose and send a status report (stdin: JSON)
  apply                 run the applier pass
  publish                serve one publish request over a Unix socket
                         (internal; runs only in the publisher container)
  status                summarise the queue: HEAD, heartbeat age, failure,
                         expiring credentials -- ledger-only, no cluster
  why <sha> [--dir <path>]
                         explain everything the system knows about one
                         commit: the ledger record if any, its queue
                         position relative to HEAD, the tofu units it
                         selects, and whether CI filed an approved plan
                         digest for each -- reads the ledger, a local git
                         checkout (--dir, default ".") and the forge for
                         the PR head sha; never writes to the ledger, never
                         takes the state lock, never applies anything.
                         Exit 2 if the queue has not reached this commit
                         yet (no ledger record).
  skip <sha> --reason <text>
                         advance HEAD past a commit that cannot apply;
                         refuses without a failed record, a stated reason
                         and TRUSS_SKIP_I_UNDERSTAND=<sha>
  units <sha> [--dir <path>] [--kind <kind>]
                         print the units a commit touches, one per line as
                         "<kind>\t<path>" in execution order (credentials,
                         tofu); --kind filters to one kind
`

// run is the binary's only entry point besides main, and main's only job
// is to call this and hand its result to os.Exit -- so every subcommand's
// logic is reachable, and testable, without ending the test process.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runEnv(context.Background(), args, os.Getenv, stdin, stdout, stderr)
}

// runEnv is run's real body, taking an explicit getenv so tests can supply
// a fake environment without mutating the process's real one.
func runEnv(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || !isSubcommand(args[0]) {
		fmt.Fprint(stderr, usage)
		return 2
	}

	sub, rest := args[0], args[1:]
	switch sub {
	case "ledger":
		return cmdLedger(ctx, rest, getenv, stdin, stdout, stderr)
	case "plan-digest":
		return cmdPlanDigest(rest, stdin, stdout, stderr)
	case "token":
		return cmdToken(ctx, rest, getenv, stdout, stderr)
	case "gate":
		return cmdGate(ctx, rest, getenv, stdout, stderr)
	case "expiry":
		return cmdExpiry(ctx, rest, getenv, stdout, stderr)
	case "notify":
		return cmdNotify(ctx, rest, getenv, stdin, stdout, stderr)
	case "apply":
		return cmdApply(ctx, rest, getenv, stdout, stderr)
	case "publish":
		return cmdPublish(ctx, rest, getenv, stdout, stderr)
	case "status":
		return cmdStatus(ctx, rest, getenv, stdout, stderr)
	case "why":
		return cmdWhy(ctx, rest, getenv, stdout, stderr)
	case "skip":
		return cmdSkip(ctx, rest, getenv, stdout, stderr)
	case "units":
		return cmdUnits(ctx, rest, stdout, stderr)
	default:
		// Unreachable: isSubcommand already filtered args[0]. Kept as an
		// explicit refusal rather than a panic so a future subcommand
		// added to the slice above but not to this switch fails loudly
		// instead of silently falling through.
		fmt.Fprintf(stderr, "truss: %q is documented but not wired to a handler\n", sub)
		return 2
	}
}
