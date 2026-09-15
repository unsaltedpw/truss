package main

import (
	"reflect"
	"testing"
)

// TestSubcommandsAreExactlyTheDocumentedSet pins the dispatch table to
// docs/port-plan.md §4.9's table exactly: ledger, plan-digest, token, gate,
// expiry, notify, apply, publish -- no more, no fewer. "ledger get"/"ledger
// put" and "gate protection"/"gate commit" collapse to one top-level verb
// each, which is why this is eight entries against the doc's nine rows.
//
// `publish` is the separate publisher identity's entry point: the same binary,
// run in a container that mounts a DIFFERENT audience-scoped Vault token, so
// the applier keeps its read-only policy. Shipping it here does not weaken
// that -- the separation is the kubelet projecting a token into one container
// and the kernel keeping mount namespaces apart, and code presence is not a
// capability.
//
// `status`, `why` and `skip` are not in §4.9's table -- they post-date it,
// added for the local operator CLI docs/work-items.md:86-133 asks for
// ("a binary on my computer I can use to inspect the queue and unstick
// things"), over the same ledger machinery §4.9's rows already built.
//
// `units` post-dates all of the above: it prints the units a commit touches
// so CI and the applier derive the set from one implementation.
func TestSubcommandsAreExactlyTheDocumentedSet(t *testing.T) {
	want := []string{
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
	if !reflect.DeepEqual(subcommands, want) {
		t.Fatalf("subcommands = %v, want %v", subcommands, want)
	}

	// Every documented subcommand must actually dispatch -- isSubcommand is
	// the guard runEnv uses to decide between "usage" and "route it", so a
	// name present in the slice but absent from the switch in runEnv would
	// otherwise fail silently at the "unreachable" default case.
	for _, name := range want {
		if !isSubcommand(name) {
			t.Errorf("isSubcommand(%q) = false, want true", name)
		}
	}
	if isSubcommand("bogus") {
		t.Errorf("isSubcommand(%q) = true, want false", "bogus")
	}
}
