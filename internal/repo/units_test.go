package repo

import (
	"reflect"
	"testing"
)

func TestTouchedUnitsOrdersByKindThenPath(t *testing.T) {
	got := TouchedUnits([]string{
		"projects/wren/main.tf",
		"credentials/cloudflare.tf",
		"clusters/beta/main.tf",
		"platform/dns.tf",
		"hosts/dev-beta/main.tf",
	}, nil)

	want := []Unit{
		{KindCredentials, "credentials"},
		{KindTofu, "clusters/beta"},
		{KindTofu, "hosts/dev-beta"},
		{KindTofu, "platform"},
		{KindTofu, "projects/wren"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TouchedUnits =\n%v\nwant\n%v", got, want)
	}
}

// TestSharedInputTouchedAgreesWithTouchedUnitsWidening pins the exported
// predicate against the behaviour it exists to explain: whenever a shared
// input makes TouchedUnits widen to the whole tree, SharedInputTouched must
// say so, and whenever it does not, SharedInputTouched must not either --
// same input, same regex, no daylight between them.
func TestSharedInputTouchedAgreesWithTouchedUnitsWidening(t *testing.T) {
	tree := []string{"platform", "projects/wren"}

	shared := []string{"modules/vpc/main.tf"}
	if !SharedInputTouched(shared) {
		t.Errorf("SharedInputTouched(%v) = false, want true", shared)
	}
	if got := TouchedUnits(shared, tree); len(got) != len(tree) {
		t.Errorf("TouchedUnits(%v, %v) = %v, want every unit in the tree (SharedInputTouched said this was shared)", shared, tree, got)
	}

	notShared := []string{"projects/wren/main.tf"}
	if SharedInputTouched(notShared) {
		t.Errorf("SharedInputTouched(%v) = true, want false", notShared)
	}
	if got := TouchedUnits(notShared, tree); len(got) != 1 {
		t.Errorf("TouchedUnits(%v, %v) = %v, want just the one unit named in the diff", notShared, tree, got)
	}
}

// TestUnitKindsAreCompiledIn pins that a directory cannot declare what it is.
// TestRootsAreNotConfigurable stops the ENVIRONMENT naming a root; this stops
// the TREE naming a kind, which is the same property one layer along: what
// gets executed, and with which credentials, is a fact about the engine and
// not about the commit being applied.
func TestUnitKindsAreCompiledIn(t *testing.T) {
	changed := []string{
		"somewhere/unit.json",
		"somewhere/kustomization.yaml",
		"somewhere/main.tf",
	}
	if got := TouchedUnits(changed, nil); len(got) != 0 {
		t.Fatalf("TouchedUnits(%v) = %v, want none: a directory may not declare its own kind", changed, got)
	}
}

func TestPathsThatMerelyContainAUnitNameAreIgnored(t *testing.T) {
	changed := []string{
		"docs/clusters/beta/notes.md",
		"vendor/hosts/h/thing.tf",
		"README-projects/wren.md",
	}
	if got := TouchedUnits(changed, nil); len(got) != 0 {
		t.Fatalf("TouchedUnits(%v) = %v, want none: every pattern is anchored", changed, got)
	}
}

func TestKindOfRejectsAnUnknownPath(t *testing.T) {
	for _, p := range []string{"", "docs", "modules/vpc", "deliveries/beta", "ansible/plays/base", "scripts"} {
		if k, ok := KindOf(p); ok {
			t.Errorf("KindOf(%q) = %v, true; want it rejected", p, k)
		}
	}
}

func TestASharedInputWithCredentialsIncludesItFirstForUnits(t *testing.T) {
	got := TouchedUnits(
		[]string{"modules/vpc/main.tf", "credentials/cloudflare.tf"},
		[]string{"clusters/beta", "platform"},
	)
	want := []Unit{
		{KindCredentials, "credentials"},
		{KindTofu, "clusters/beta"},
		{KindTofu, "platform"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TouchedUnits =\n%v\nwant\n%v", got, want)
	}
}

// TestAFileDirectlyUnderAUnitPrefixIsNotAUnit checks that a file sitting
// beside the unit directories -- a README -- names no unit, and reading it
// as one would plan something that is not there.
func TestAFileDirectlyUnderAUnitPrefixIsNotAUnit(t *testing.T) {
	for _, f := range []string{
		"clusters/README.md",
		"hosts/README.md",
	} {
		if got := TouchedUnits([]string{f}, nil); len(got) != 0 {
			t.Errorf("TouchedUnits([%q]) = %v, want none", f, got)
		}
	}
}

// TestTouchedUnitsTofuHalfMatchesTouchedRoots is the property that makes
// runCommitLoop's swap from repo.TouchedRoots to the credentials+tofu half
// of repo.TouchedUnits safe: for a commit whose changed files and tree
// contain no clusters/<name> and no hosts/<name>, the two functions must
// return the identical root set, in the identical order.
//
// ⚠️ unitSharedInput AND sharedInput COINCIDE TODAY (both match only
// modules/, providers.allow and .opentofu-version), so this holds for every
// commit right now -- but the two patterns are deliberately kept separate
// (see unitSharedInput's own doc), so they are free to diverge again the
// moment either side needs a pattern the other must not follow. This test
// is what would catch that divergence breaking the swap.
//
// This is what internal/parity's 43 recorded scenarios have always been, so
// pinning it here is what lets the swap happen without widening what the
// bash-parity corpus already answers for.
func TestTouchedUnitsTofuHalfMatchesTouchedRoots(t *testing.T) {
	cases := []struct {
		name    string
		changed []string
		tree    []string // fed to both TouchedRoots and TouchedUnits verbatim
	}{
		{
			name:    "a single project root",
			changed: []string{"projects/recipes/main.tf"},
		},
		{
			name:    "platform and a project together",
			changed: []string{"platform/main.tf", "projects/recipes/main.tf"},
		},
		{
			name:    "credentials alone",
			changed: []string{"credentials/cloudflare.tf"},
		},
		{
			name:    "credentials with a project",
			changed: []string{"credentials/cloudflare.tf", "projects/recipes/main.tf"},
		},
		{
			name:    "a shared tofu input plans the whole tree",
			changed: []string{"modules/vpc/main.tf"},
			tree:    []string{"platform", "projects/alpha", "projects/beta"},
		},
		{
			name:    "the provider allowlist is a shared input",
			changed: []string{"providers.allow"},
			tree:    []string{"platform"},
		},
		{
			name:    "the pinned opentofu version is a shared input",
			changed: []string{".opentofu-version"},
			tree:    []string{"projects/alpha"},
		},
		{
			name:    "a shared input alongside credentials",
			changed: []string{"modules/vpc/main.tf", "credentials/cloudflare.tf"},
			tree:    []string{"platform"},
		},
		{
			name:    "a path outside every root or unit",
			changed: []string{"docs/README.md"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantRoots := TouchedRoots(c.changed, c.tree)

			var got []string
			for _, u := range TouchedUnits(c.changed, c.tree) {
				if u.Kind == KindCredentials || u.Kind == KindTofu {
					got = append(got, u.Path)
				}
			}

			if !reflect.DeepEqual(got, wantRoots) {
				t.Fatalf("credentials+tofu half of TouchedUnits(%v, %v) = %v, want TouchedRoots' answer %v", c.changed, c.tree, got, wantRoots)
			}
		})
	}
}

// TestASubdirectoryInsideAUnitIsNotItselfAUnit is the regression for the
// wedge of 2026-09-12: KindOf classified by PREFIX, so every directory
// NESTED inside a unit answered "yes, I am a unit too". A host's own
// .terraform/ cache directory, or a cluster's manifests/ subdirectory, would
// have hit the identical trap the first time either was committed.
func TestASubdirectoryInsideAUnitIsNotItselfAUnit(t *testing.T) {
	for _, p := range []string{
		"clusters/beta/manifests",
		"hosts/h/.terraform",
	} {
		if k, ok := KindOf(p); ok {
			t.Errorf("KindOf(%q) = %v, true; want it rejected -- it is a directory INSIDE a unit, not a unit", p, k)
		}
	}

	// And the units themselves must still be units, or the fix has simply
	// stopped truss seeing anything at all.
	for p, want := range map[string]Kind{
		"clusters/beta": KindTofu,
		"hosts/h":       KindTofu,
		"platform":      KindTofu,
		"credentials":   KindCredentials,
	} {
		k, ok := KindOf(p)
		if !ok || k != want {
			t.Errorf("KindOf(%q) = %v, %v; want %v, true", p, k, ok, want)
		}
	}
}

// TestAFileInAUnitsSubdirectoryStillSelectsThatUnit is the other half, and it
// exists to stop the fix above being made the wrong way. The same patterns
// map a CHANGED FILE to the unit that owns it, and there the prefix shape is
// correct: a file nested under a host belongs to that host.
func TestAFileInAUnitsSubdirectoryStillSelectsThatUnit(t *testing.T) {
	for _, tc := range []struct {
		file string
		want Unit
	}{
		{"hosts/h/nested/main.tf", Unit{KindTofu, "hosts/h"}},
		{"clusters/beta/nested/deep/main.tf", Unit{KindTofu, "clusters/beta"}},
	} {
		got := TouchedUnits([]string{tc.file}, nil)
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("TouchedUnits(%q) = %v; want exactly [%v]", tc.file, got, tc.want)
		}
	}
}
