package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/beeradb/truss/internal/gates"
	"github.com/beeradb/truss/internal/ledger"
	"github.com/beeradb/truss/internal/render"
	"github.com/beeradb/truss/internal/repo"
)

// renderRunner is the subset of render.Runner the pass uses, as a local
// interface so a test can drive the delivery path without a kustomize
// binary -- the same reason gitDriver and tofuRunner exist.
type renderRunner interface {
	Build(ctx context.Context, dir string) ([]byte, error)
}

// renderFactory builds a runner for one pass. It mirrors tofuFactory.
type renderFactory func(env []string) renderRunner

// renderUnitsFor derives the render units a commit touches.
//
// ⚠️ treeUnits HERE IS THE RENDER UNITS OF THE TREE, NOT EVERY UNIT. It is
// fed from gitDriver.TreeRenderUnits, and only the render half of the result
// is read, so the shared-input branch of TouchedUnits -- which returns
// everything in the tree -- returns exactly the render units that exist.
// tofuUnitsFor (apply_cmd.go) is the sibling that reads the
// credentials/tofu half of the very same repo.TouchedUnits call, from its
// own tree listing (gitDriver.TreeTofuUnits). Neither call touches
// repo.TouchedRoots, which internal/parity still compares against
// recordings of the bash unchanged.
//
// A shared input therefore re-renders every unit rather than only the ones
// whose own files changed, and that is accepted cost rather than an
// oversight: CI derives the units by running `truss units --kind render`
// (cmd/truss/units_cmd.go), which feeds the same ChangedFiles and
// TreeRenderUnits into the same repo.TouchedUnits this function calls, so
// both sides agree on the set and the extra renders simply cost time.
//
// That subcommand is load-bearing, not decoration. Before it existed, no
// consumer's CI could import repo.TouchedUnits -- it lives under internal/ --
// so CI could only maintain its own approximation of the rule, and that
// approximation was narrower than unitSharedInput here: it fired on
// modules/, providers.allow and .opentofu-version (the tofu-side rule) but
// not on inventory/, .kustomize-version or .ansible-version. A commit
// touching only inventory/ then made the applier demand a filed digest for
// every render unit in the tree while CI, unaware inventory/ was a shared
// input on this side, filed one only for the units it thought had changed.
// The refusal that followed said "refusing to deliver a manifest nobody
// reviewed" (internal/gates), which names a review failure when the real
// cause was two sides deriving different sets of units -- the same wrong-
// cause shape TestCheckRenderDigestTellsAbsentFromMismatched exists for.
func renderUnitsFor(changedFiles, treeUnits []string) []string {
	var out []string
	for _, u := range repo.TouchedUnits(changedFiles, treeUnits) {
		if u.Kind == repo.KindRender {
			out = append(out, u.Path)
		}
	}
	return out
}

// renderOneUnit renders one delivery unit and refuses unless the bytes hash
// to what CI filed for this commit. It applies nothing: a reconciler does
// that, from a ref the applier advances only once every unit of the commit
// has passed here.
//
// The empty reason means "this unit is settled", which includes the case
// where it no longer exists -- see below.
func renderOneUnit(ctx context.Context, d applyDeps, r renderRunner, headSHA, unit string) (digest string, reason string) {
	// ⚠️ AN ABSENT RENDER UNIT IS A PRUNE, NOT A REFUSAL, AND THIS IS THE
	// ONE PLACE THE KINDS DELIBERATELY DIVERGE.
	//
	// applyOneRoot refuses a root that is not in the tree ("root %s does not
	// exist at %s"), and it is right to: an OpenTofu root's absence can mean
	// state holding live resources nobody is managing any more, which is a
	// question for a human. A delivery unit holds no state. Its absence at
	// this commit means the commit deleted it, and deleting it IS the
	// intended operation -- the reconciler removes what it previously
	// applied. Refusing here would make the ordinary act of retiring a
	// workload impossible without an operator override.
	if !d.Git.HasDir(unit) {
		d.logf("render: %s is gone at %s; the reconciler prunes what it applied", unit, headSHA)
		return "", ""
	}

	unitDir := d.Cfg.Workdir + "/" + unit
	out, err := r.Build(ctx, unitDir)
	if err != nil {
		return "", fmt.Sprintf("could not render %s at %s: %v", unit, headSHA, err)
	}

	mine := render.Digest(out)
	approved, err := d.Journal.ApprovedDigest(ctx, headSHA, unit)
	approvedFound := true
	if err != nil {
		if errors.Is(err, ledger.ErrNotFound) {
			approvedFound = false
		} else {
			return "", fmt.Sprintf("could not read the approved render digest for %s at %s: %v", unit, headSHA, err)
		}
	}

	key := d.Journal.Layout.DigestKey(headSHA, unit)
	if problems := gates.CheckRenderDigest(unit, headSHA, key, mine, approved, approvedFound); len(problems) > 0 {
		// Recorded before the return, the same way the plan digest gate
		// records its own refusal before returning: a refusal is a checked
		// gate, not a skipped one, and truss_render_units/truss_render_refusals
		// must count it.
		d.Obs.rendered(true)
		return "", strings.Join(problems, "; ")
	}

	d.Obs.rendered(false)
	d.logf("render for %s matches the one approved at %s", unit, headSHA)
	return mine, ""
}

// renderEnv is the whole environment a render gets: PATH so the binary can
// be found, HOME because some tools write a cache there, and nothing else.
//
// ⚠️ NO CREDENTIAL IS PASSED, DELIBERATELY, AND THAT IS THE PROPERTY THE
// DELIVERY GATE RESTS ON. A render reads the tree and nothing else, which is
// what lets a read-only CI job and the applier compute the same bytes. The
// moment a render could read a credential it could also produce output that
// depends on who ran it, and the two sides would stop agreeing -- the same
// failure internal/plan had to filter no-op entries to escape.
func renderEnv(d applyDeps) []string {
	return []string{"PATH=" + d.PATH, "HOME=" + d.HOME}
}

// kustomizeBin resolves the renderer, optional-with-default in the shape of
// SECRETS_DIR and TF_PLUGIN_DIR -- but deliberately not a config.Config
// field, because config_test.go pins the exact count of those and this knob
// is not the applier's to grow.
//
// One definition with two callers: cmdRenderDigest resolves it for CI and
// cmdApply for the pass. The two MUST agree, for the same reason
// internal/plan/digest.go and the consumer's jq must -- when the two sides
// of a digest gate disagree about the tool, every apply is refused.
func kustomizeBin(getenv func(string) string) string {
	if bin := getenv("KUSTOMIZE_BIN"); bin != "" {
		return bin
	}
	return defaultKustomizeBin
}

// newRenderFactory builds the pass's render factory. It lives here rather
// than inline in cmdApply so apply_cmd.go needs no render import and the
// renderer's configuration sits beside the code that uses it.
func newRenderFactory(getenv func(string) string, stderr io.Writer) renderFactory {
	bin := kustomizeBin(getenv)
	return func(env []string) renderRunner {
		return render.Runner{Bin: bin, Stderr: stderr, Env: env}
	}
}

// deliveryRef is the ref a reconciler tracks. It is compiled in rather than
// configured, for the reason roots are: what the applier publishes to is a
// property of the engine, and a deployment that could rename it could point a
// cluster at a ref nothing gates.
//
// ⚠️ IT IS `queued` AND NOT `delivered`, BECAUSE DELIVERED IMPLIES DONE.
// Advancing this ref means truss gated the commit, matched every render
// against the digest CI filed, and applied every root -- it does not mean a
// cluster has the manifests, which truss cannot know at the moment it hands
// them over. A second ref advanced from observed reconciler status would be
// the honest answer to "what is actually running", and is not built.
const deliveryRef = "queued"

// publishDeliveryRef fast-forwards the delivery ref to sha, refusing unless
// that ref's history is append-only. It returns the reason to fail the pass,
// or empty.
//
// The gate runs before the push, not after: a ref nothing protects is a path
// to production nobody is watching, and discovering that after publishing to
// it would be discovering it too late.
func publishDeliveryRef(ctx context.Context, d applyDeps, sha string) string {
	// ⚠️ A DEPLOYMENT WITH NO DELIVERY UNITS IS NOT ASKED TO PROTECT A REF IT
	// DOES NOT USE. Without this, requiring a ruleset on the delivery ref
	// stopped every pass everywhere -- including a tree that is pure
	// OpenTofu and has no manifests at all -- because the gate refuses an
	// unprotected ref and an unused ref is unprotected. That would make
	// delivery a tax on people who never asked for it.
	//
	// The tree is what decides, not a configuration flag: a deployment
	// starts using delivery by committing a unit, and the pass notices on
	// the commit that does it. An empty listing therefore means "nothing to
	// publish", never "publish without checking" -- the failure direction is
	// a ref that stays put, which is visible, rather than one that advances
	// unwatched.
	units, err := d.Git.TreeRenderUnits(ctx, sha)
	if err != nil {
		return fmt.Sprintf("could not read the render units at %s: %v", sha, err)
	}
	if len(units) == 0 {
		// ⚠️ NOT PUBLISHED, AND DELIBERATELY NOT UNPROTECTED. A tree with no
		// delivery units never asks the forge about the ref at all, so it has
		// no basis to report on its protection. Recording it as unprotected
		// here would be exactly the conflation truss_delivery_ref_unprotected
		// exists to prevent: "this deployment does not use delivery" reading
		// as "delivery is broken and gated commits are stuck".
		d.Obs.delivered(false)
		return ""
	}

	rs, err := d.Forge.Rulesets(ctx, deliveryRef)
	if err != nil {
		return fmt.Sprintf("could not read the rulesets protecting %s: %v", deliveryRef, err)
	}
	if problems := gates.CheckDeliveryRef(deliveryRef, rs, d.Cfg.DeliveryBypassActorID); len(problems) > 0 {
		d.Obs.deliveryRefIsUnprotected()
		return strings.Join(problems, "; ")
	}
	if err := d.Git.PushRef(ctx, sha, deliveryRef); err != nil {
		return fmt.Sprintf("could not publish %s to %s: %v", sha, deliveryRef, err)
	}
	d.Obs.delivered(true)
	d.logf("published %s to %s", sha, deliveryRef)
	return ""
}
