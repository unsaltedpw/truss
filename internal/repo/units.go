package repo

import (
	"regexp"
	"sort"
)

// Kind is what a directory IS, and therefore how the applier treats it: what
// binary plans it, what evidence the reviewer read, and what a refusal
// means.
//
// ⚠️ A KIND IS DETECTED FROM THE TREE AND NEVER FROM CONFIGURATION. The
// alternative -- a marker file in each directory declaring its own kind --
// is the flexible design and it is the wrong one, because it moves the
// decision "what will be executed, and with which credentials" out of the
// engine and into the tree being changed. "This directory holds a script
// that runs as root on the applier's own node" is not a deployment value.
// docs/port-plan.md draws that line as engine versus deployment values, and
// TestUnitKindsAreCompiledIn pins it.
type Kind int

const (
	// KindCredentials is the one root whose state holds token values. It
	// runs first and it is the only digest-gate exemption in the system.
	KindCredentials Kind = iota
	// KindTofu is an OpenTofu root: planned, digest-gated, applied. This
	// covers "platform", every projects/<name>, clusters/<name> and
	// hosts/<name> -- clusters and hosts are tofu roots that provision the
	// machine, not a configuration or delivery layer on top of it.
	KindTofu
)

func (k Kind) String() string {
	switch k {
	case KindCredentials:
		return "credentials"
	case KindTofu:
		return "tofu"
	}
	return "unknown"
}

// Unit is one directory the applier acts on, and how.
type Unit struct {
	Kind Kind
	Path string
}

// The unit patterns, all anchored at the start of the path for the reason
// sharedInput already gives: a commit touching "docs/clusters/README.md"
// must not be read as touching a cluster.
// ⚠️ EVERY PATTERN REQUIRES A TRAILING SLASH, WHICH IS WHAT MAKES IT MATCH A
// DIRECTORY RATHER THAN A NAME. An earlier draft ended them with `(/|$)` so
// that one pattern could serve both a changed file and a bare unit path, and
// it read the FILE "clusters/README.md" as a cluster root named README.md.
// Caught before it shipped.
//
// KindOf appends the slash instead, which is the idiom projectPath already
// uses (roots.go:19, matched against path+"/"). One shape, two callers.
var (
	clusterUnit = regexp.MustCompile(`^clusters/([^/]+)/`)
	hostUnit    = regexp.MustCompile(`^hosts/([^/]+)/`)
)

// The same prefixes, anchored, for the OTHER question. The patterns above
// answer "which unit owns this file", where a prefix is right. These answer
// "is this path itself a unit", where a prefix is WRONG -- a directory
// nested inside a unit must not itself read as a second unit.
var (
	clusterUnitExact = regexp.MustCompile(`^clusters/[^/]+$`)
	hostUnitExact    = regexp.MustCompile(`^hosts/[^/]+$`)
	projectUnitExact = regexp.MustCompile(`^projects/[^/]+$`)
)

// unitSharedInput is a path that is an input to EVERY unit.
//
// ⚠️ IT IS DELIBERATELY NOT sharedInput, EVEN THOUGH TODAY THE TWO PATTERNS
// COINCIDE. TouchedRoots reproduces derive_touched_roots byte for byte and
// the parity corpus compares against recordings of the bash; widening the
// pattern it reads would change what that function returns for commits the
// corpus already has answers for. So the unit layer keeps its own, and
// TouchedRoots keeps its exact body -- the two are free to diverge again the
// moment either side needs to.
var unitSharedInput = regexp.MustCompile(`^(modules/|providers\.allow$|\.opentofu-version$)`)

// SharedInputTouched reports whether changedFiles includes a shared input --
// the same test TouchedUnits makes internally (the `shared` loop above) to
// decide whether to widen its selection to every unit that exists in the
// tree, rather than only the ones whose own files changed.
//
// It exists for a caller that already knows it is about to call
// TouchedUnits and needs to explain a WIDENED result to a reader -- `truss
// why` (docs/work-items.md:86-133) names this explicitly, because a commit
// touching only modules/foo.tf selecting every tofu root in the repository
// is a surprise worth stating, not one to leave a reader to infer from an
// unusually long unit list. Re-deriving the test with a
// second copy of unitSharedInput would be the two-copies-of-one-fact
// mistake AGENTS.md already warns against; this reads the same regex
// TouchedUnits itself is built on, so the two can never disagree about what
// counts as shared.
func SharedInputTouched(changedFiles []string) bool {
	for _, f := range changedFiles {
		if unitSharedInput.MatchString(f) {
			return true
		}
	}
	return false
}

// KindOf reports what kind of unit path is, if it is a unit at all. It is
// the single place a directory's meaning is decided, so the tree listing and
// the commit diff can never disagree about what something is.
func KindOf(path string) (Kind, bool) {
	switch {
	case path == "credentials":
		return KindCredentials, true
	case path == "platform":
		return KindTofu, true
	case projectUnitExact.MatchString(path):
		return KindTofu, true
	case clusterUnitExact.MatchString(path), hostUnitExact.MatchString(path):
		return KindTofu, true
	}
	return 0, false
}

// TouchedUnits derives every unit a commit touches, in the order the applier
// must act on them.
//
// The order is by kind, then by path: credentials, then tofu. That is a
// TOTAL ORDER BETWEEN KINDS and not a dependency graph, and the distinction
// matters -- docs/work-items.md refuses declared edges between roots on the
// grounds that the current failure mode is safe and edges would let a
// mis-ordered apply succeed instead of refuse. Nothing here lets that
// happen: the sequence is fixed, compiled in, and identical for every
// commit.
//
// treeUnits is every unit present in the commit's own tree, supplied by
// whatever read the tree; this function does no filesystem or git I/O of its
// own, the same rule TouchedRoots keeps.
func TouchedUnits(changedFiles, treeUnits []string) []Unit {
	shared := false
	credentials := false
	for _, f := range changedFiles {
		if unitSharedInput.MatchString(f) {
			shared = true
		}
		if credentialsPath.MatchString(f) {
			credentials = true
		}
	}

	paths := make(map[string]bool)
	if credentials {
		paths["credentials"] = true
	}

	if shared {
		// A shared input plans everything that exists, which is blunt on
		// purpose: over-planning costs a slow pass, under-planning means a
		// unit built on a fact that is no longer true.
		for _, u := range treeUnits {
			if _, ok := KindOf(u); ok {
				paths[u] = true
			}
		}
		return sortUnits(paths)
	}

	for _, f := range changedFiles {
		if f == "platform" || platformPath.MatchString(f) {
			paths["platform"] = true
		}
		if m := projectPath.FindStringSubmatch(f); m != nil {
			paths["projects/"+m[1]] = true
		}
		if m := clusterUnit.FindStringSubmatch(f); m != nil {
			paths["clusters/"+m[1]] = true
		}
		if m := hostUnit.FindStringSubmatch(f); m != nil {
			paths["hosts/"+m[1]] = true
		}
	}
	return sortUnits(paths)
}

// sortUnits turns the set into the fixed kind-then-path order.
func sortUnits(paths map[string]bool) []Unit {
	units := make([]Unit, 0, len(paths))
	for p := range paths {
		k, ok := KindOf(p)
		if !ok {
			// Unreachable: nothing is added to the set without KindOf
			// having accepted it. Dropping it rather than guessing a kind
			// keeps "a directory of unknown kind is never executed" true by
			// construction.
			continue
		}
		units = append(units, Unit{Kind: k, Path: p})
	}
	sort.Slice(units, func(i, j int) bool {
		if units[i].Kind != units[j].Kind {
			return units[i].Kind < units[j].Kind
		}
		return units[i].Path < units[j].Path
	})
	return units
}
