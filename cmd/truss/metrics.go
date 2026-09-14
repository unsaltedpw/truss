package main

import (
	"context"
	"runtime/debug"
	"sort"
	"time"

	"github.com/beeradb/truss/internal/metrics"
	"github.com/beeradb/truss/internal/notify"
)

// The failure classes. A pass's alert text says what went wrong in a
// sentence; a dashboard cannot filter on a sentence, and a rule that tried
// would be a regex over prose -- the mistake AGENTS.md already names ("gate
// on the field, never rendered text"). So every site that sets a failure also
// names its CLASS here, at the point where the cause is still known, and
// truss_pass_failure carries one series per class.
//
// ⚠️ THE CLASSES ARE NOT SEVERITIES AND MUST NOT BE COLLAPSED. classDigest
// means the plan truss was about to apply did not hash to the plan a human
// approved: the world moved between review and apply, or somebody moved it.
// classApply means `tofu apply` returned non-zero. Both are red and they want
// different people. Separating them is the entire reason this label exists
// rather than a single truss_pass_success.
const (
	classProtection  = "protection"  // branch protection does not meet the bar
	classRulesets    = "rulesets"    // a ruleset does not meet the bar
	classForge       = "forge"       // the forge would not answer or would not mint a token
	classRepo        = "repo"        // the working copy could not be cloned, fetched or read
	classCommit      = "commit"      // a commit did not get to main the way it must have
	classCredentials = "credentials" // a credential the apply needs is missing or unreadable
	classPlan        = "plan"        // tofu init/plan/show failed
	classDigest      = "digest"      // the plan did not hash to the approved one
	classApply       = "apply"       // tofu apply failed
	classRotation    = "rotation"    // rotating the credentials root failed
	classPublish     = "publish"     // the publisher handoff failed
	classLedger      = "ledger"      // a ledger object could not be written
	classConfig      = "config"      // the repository does not contain what it says it does
	classLock        = "lock"        // a state lock was held far longer than a pass can take
)

// failureClasses is every class, in the order the dashboard lists them.
// ⚠️ ALL OF THEM ARE EMITTED EVERY PASS, most of them as 0. A class that
// appeared only when it fired would leave a query returning "no data" for
// two different facts -- nothing went wrong, and nothing pushed at all --
// which is the ambiguity the whole staleness rule exists to remove.
var failureClasses = []string{
	classProtection, classRulesets, classForge, classRepo, classCommit,
	classCredentials, classPlan, classDigest, classApply, classRotation,
	classPublish, classLedger, classConfig, classLock,
}

// rootPhase is one timed step of one root's apply.
type rootPhase struct{ root, phase string }

// passObs is what the pass observed about itself, accumulated as it goes and
// rendered once at the tail. It holds only what the heartbeat and the alert
// do NOT already carry: notify.Report is passed to passMetrics alongside this
// rather than copied into it, because two records of the same fact drift.
//
// ⚠️ EVERY METHOD IS NIL-SAFE, AND runApplyPass INSTALLS THE ONE REAL
// RECORDER. A test that drives runCommitLoop or applyOneRoot directly is not
// running a pass and has no pass to describe, so it records nothing rather
// than needing a recorder it would never read. Nothing in the pass reads
// these fields back to make a decision -- they are write-only until
// passMetrics -- so an absent recorder cannot change what truss does, only
// what it reports about a step no pass ran.
type passObs struct {
	failures     map[string]bool
	gateOK       map[string]bool
	rootSeconds  map[rootPhase]float64
	rootChanges  map[string]int
	rootFailures map[string]int
	// queueDepth is how many commits the pass found waiting, and queueKnown
	// is whether it ever got to look. They are separate because "nothing is
	// waiting" and "we never reached the queue" are different facts and the
	// second one is what a failed gate produces.
	queueDepth    int
	queueKnown    bool
	digestChecks  int
	digestRefused int
	lockContended bool
	logEvents     map[string]int
	ledgerErrors  int

	// renderUnits and renderRefusals are the delivery side of digestChecks/
	// digestRefused above -- the same "did the gate run and agree" question,
	// asked of a rendered manifest instead of a tofu plan. Kept as separate
	// fields rather than folded into the digest ones because they gate
	// different artefacts: a bad manifest and a bad plan want different
	// people, and one counter could not tell a caller which broke.
	renderUnits    int
	renderRefusals int

	rotationRan     bool
	rotationOK      bool
	publishAttempt  bool
	publishOK       bool
	publishExpiries int
	driftRan        bool

	// deliveryPublished and deliveryRefUnprotected describe publishDeliveryRef's
	// one outcome. Both default false, which is correct for every path that
	// is not an actual publish: a tree with no delivery units, a forge or
	// push error, or a pass that never called it at all. Only the specific
	// "no ruleset protects the ref" gate failure sets the second one -- see
	// deliveryRefIsUnprotected's own comment for why that distinction is the
	// whole reason the field exists.
	deliveryPublished      bool
	deliveryRefUnprotected bool
}

func newPassObs() *passObs {
	return &passObs{
		failures:     map[string]bool{},
		gateOK:       map[string]bool{},
		rootSeconds:  map[rootPhase]float64{},
		rootChanges:  map[string]int{},
		rootFailures: map[string]int{},
		logEvents:    map[string]int{},
	}
}

// failed records that something of this class went wrong. It is called at the
// site that KNOWS the class, never derived afterwards from the message.
func (o *passObs) failed(class string) {
	if o == nil {
		return
	}
	o.failures[class] = true
}

// gate records whether one named gate passed. Distinct from failed: a gate
// that passed is a fact worth graphing, and "no protection failure this pass"
// and "protection was never read" are different states.
func (o *passObs) gate(name string, ok bool) {
	if o == nil {
		return
	}
	o.gateOK[name] = ok
}

func (o *passObs) rootTook(root, phase string, seconds float64) {
	if o == nil {
		return
	}
	o.rootSeconds[rootPhase{root, phase}] += seconds
}

func (o *passObs) rootChanged(root string, n int) {
	if o == nil {
		return
	}
	o.rootChanges[root] = n
}

func (o *passObs) rootFailed(root string) {
	if o == nil {
		return
	}
	o.rootFailures[root]++
}

// queued records how many commits are waiting to be applied, at the moment
// the pass looked. Called once, by runCommitLoop, which is the only thing
// that knows.
func (o *passObs) queued(n int) {
	if o == nil {
		return
	}
	o.queueDepth, o.queueKnown = n, true
}

func (o *passObs) digestChecked(refused bool) {
	if o == nil {
		return
	}
	o.digestChecks++
	if refused {
		o.digestRefused++
	}
}

func (o *passObs) logged(level string) {
	if o == nil {
		return
	}
	o.logEvents[level]++
}

// rendered records that this pass built one delivery unit and compared its
// bytes against the digest CI filed -- called once per unit that actually
// reached that comparison. A unit the tree no longer has is a prune
// (renderOneUnit returns before this is ever called for it), not a render,
// and must not count here the way a root skipped for lack of a plan does not
// count toward digestChecked.
func (o *passObs) rendered(refused bool) {
	if o == nil {
		return
	}
	o.renderUnits++
	if refused {
		o.renderRefusals++
	}
}

// The remaining recorders. ⚠️ THESE ARE METHODS AND NOT FIELD ASSIGNMENTS
// FOR ONE REASON: a nil-safe recorder is only nil-safe through its methods,
// and `d.Obs.publishOK = true` in runHandoff panicked every test that drove
// that function directly. The type's contract is that recording is always
// safe; a bare field write is a second way to record that does not honour it.
func (o *passObs) rotated(ran, ok bool) {
	if o == nil {
		return
	}
	o.rotationRan, o.rotationOK = ran, ok
}

func (o *passObs) publishAttempted() {
	if o == nil {
		return
	}
	o.publishAttempt = true
}

func (o *passObs) publishResult(ok bool, expiries int) {
	if o == nil {
		return
	}
	o.publishOK, o.publishExpiries = ok, expiries
}

func (o *passObs) contended() {
	if o == nil {
		return
	}
	o.lockContended = true
}

// delivered records whether this pass fast-forwarded the delivery ref.
// False covers every path that is not a successful publish -- a refusal, a
// forge or push error, and a tree with no delivery units at all -- because
// publishDeliveryRef's own doc says the last of those must never be read as
// the ref being broken.
func (o *passObs) delivered(ok bool) {
	if o == nil {
		return
	}
	o.deliveryPublished = ok
}

// deliveryRefIsUnprotected records the one gate failure that means gated
// commits are piling up with no cluster receiving them: the rulesets on the
// delivery ref do not add up to a gated path to production. Since 2026-09-14
// that covers four causes, not two -- no ruleset applies, a rule is missing
// (non_fast_forward, deletion, or the update rule that restricts the pusher),
// the bypass list names an actor other than the applier's own App, or the list
// could not be read at all. It is never called for a tree with no delivery
// units -- "this deployment does not use delivery" and "delivery is broken"
// are different facts, and this metric exists to alarm only on the second.
func (o *passObs) deliveryRefIsUnprotected() {
	if o == nil {
		return
	}
	o.deliveryRefUnprotected = true
}

func (o *passObs) drifting() {
	if o == nil {
		return
	}
	o.driftRan = true
}

func (o *passObs) ledgerError() {
	if o == nil {
		return
	}
	o.ledgerErrors++
}

// buildFacts is what the binary can say about itself. Passed in rather than
// read here so passMetrics stays a function a test calls with known inputs.
type buildFacts struct {
	GoVersion string
	Revision  string
}

// passMetrics is the whole exposition one pass pushes, built from what the
// pass already tells the heartbeat and the alert plus what passObs watched.
//
// It is a pure function over its arguments -- no clock, no I/O, no globals --
// for the reason internal/gates gives for itself: "which series does a pass
// that refused everything emit" is then a test that calls a function and
// reads its answer.
func passMetrics(finished time.Time, duration time.Duration, driftRun bool, rep notify.Report, o *passObs, build buildFacts) metrics.Set {
	if o == nil {
		o = newPassObs()
	}

	one := func(b bool) float64 {
		if b {
			return 1
		}
		return 0
	}

	set := metrics.Set{
		// ⚠️ THE MOST IMPORTANT SERIES IN THIS FILE, AND THE ONLY ONE EVERY
		// ALERT MUST BE ANCHORED TO. A Pushgateway serves the last thing it
		// was given, forever. If truss stops running entirely -- the image
		// will not pull, the CronJob was suspended, the node is gone --
		// every other metric here keeps answering with whatever the last
		// healthy pass said, and truss_pass_success stays 1. A dashboard
		// built on those alone reports a dead applier as a healthy one,
		// which is the fail-open-with-a-receipt shape this project refuses
		// by construction. `time() - truss_pass_timestamp_seconds` is the
		// only thing here that goes bad on its own when nothing pushes, so
		// it is the dead man's switch and observability/alerts states it
		// first.
		{
			Name: "truss_pass_timestamp_seconds",
			Help: "Unix time at which this pass finished. Staleness of this value is the only liveness signal; every other metric here is the last pass's opinion and survives the applier's death.",
			Samples: []metrics.Sample{
				{Value: float64(finished.Unix())},
			},
		},
		{
			Name:    "truss_pass_duration_seconds",
			Help:    "How long this pass took, gate to alert.",
			Samples: []metrics.Sample{{Value: duration.Seconds()}},
		},
		{
			Name:    "truss_pass_success",
			Help:    "1 when the pass reported no failure. Meaningless on its own -- see truss_pass_timestamp_seconds.",
			Samples: []metrics.Sample{{Value: one(rep.Failure == "")}},
		},
		{
			Name:    "truss_pass_commits_applied",
			Help:    "Commits this pass applied.",
			Samples: []metrics.Sample{{Value: float64(rep.Applied)}},
		},
		{
			Name:    "truss_pass_commits_noop",
			Help:    "Commits this pass recorded as touching no root.",
			Samples: []metrics.Sample{{Value: float64(rep.Noop)}},
		},
		{
			Name:    "truss_pass_lock_contended",
			Help:    "1 when the pass stopped early because another holder had the state lock. Not a failure: HEAD does not move and nothing is filed.",
			Samples: []metrics.Sample{{Value: one(o.lockContended)}},
		},
		{
			Name:    "truss_pass_ledger_errors",
			Help:    "Ledger objects this pass could not write. Every one of them is a durable record that no longer exists.",
			Samples: []metrics.Sample{{Value: float64(o.ledgerErrors)}},
		},
	}

	// ⚠️ EMITTED ONLY WHEN THE PASS ACTUALLY REACHED THE QUEUE, and the
	// absence is the point rather than a gap. A pass refused at the branch
	// protection gate never ran the commit loop and knows nothing about how
	// much work is waiting; reporting 0 for it would be a claim it did not
	// earn, and would be indistinguishable from a queue that is genuinely
	// empty. On a drift pass it is absent for the same reason -- that pass
	// never walks the queue at all.
	//
	// ⚠️ THIS IS THE COUNTER docs/work-items.md ASKED FOR. Nothing surfaced
	// that the applier was N commits behind an unresolved failure until
	// somebody went looking, and a refusal that wedges the queue looks
	// identical to a quiet week from every other series here: HEAD does not
	// move, the same refusal repeats, and the pile behind it is invisible.
	if o.queueKnown {
		set = append(set, metrics.Family{
			Name:    "truss_queue_depth",
			Help:    "Commits waiting to be applied when this pass looked. Absent when the pass never reached the queue -- a refused gate knows nothing about it, and reporting zero would be a claim it did not earn.",
			Samples: []metrics.Sample{{Value: float64(o.queueDepth)}},
		})
	}

	set = append(set, metrics.Family{
		Name:    "truss_pass_failure",
		Help:    "1 for each class of thing that went wrong this pass. A pass can set more than one.",
		Samples: classSamples(o.failures),
	})

	if len(o.gateOK) > 0 {
		set = append(set, metrics.Family{
			Name:    "truss_gate_ok",
			Help:    "1 when the named gate was read and met the bar this pass.",
			Samples: boolSamples("gate", o.gateOK),
		})
	}

	set = append(set,
		metrics.Family{
			Name:    "truss_digest_checks",
			Help:    "Roots whose re-planned digest was compared against the approved one this pass.",
			Samples: []metrics.Sample{{Value: float64(o.digestChecks)}},
		},
		metrics.Family{
			Name:    "truss_digest_refusals",
			Help:    "Roots refused this pass because the plan did not hash to the approved one. Any value above zero is the gate doing the job it exists for.",
			Samples: []metrics.Sample{{Value: float64(o.digestRefused)}},
		},
		metrics.Family{
			Name:    "truss_render_units",
			Help:    "Delivery units this pass rendered and compared against the digest CI filed.",
			Samples: []metrics.Sample{{Value: float64(o.renderUnits)}},
		},
		metrics.Family{
			// ⚠️ NOT truss_digest_refusals. That series is the OpenTofu plan
			// gate; this one is the rendered-manifest gate, over a different
			// artefact CI files a different digest for. Folding them together
			// would leave a dashboard unable to tell a bad manifest from a bad
			// plan -- the same reason the failure classes are never collapsed.
			Name:    "truss_render_refusals",
			Help:    "Renders this pass refused because the rendered bytes did not hash to the digest CI filed for that unit. Distinct from truss_digest_refusals, which is the OpenTofu plan gate over a different artefact.",
			Samples: []metrics.Sample{{Value: float64(o.renderRefusals)}},
		},
	)

	if len(o.rootSeconds) > 0 {
		phases := make([]rootPhase, 0, len(o.rootSeconds))
		for k := range o.rootSeconds {
			phases = append(phases, k)
		}
		sort.Slice(phases, func(i, j int) bool {
			if phases[i].root != phases[j].root {
				return phases[i].root < phases[j].root
			}
			return phases[i].phase < phases[j].phase
		})
		samples := make([]metrics.Sample, 0, len(phases))
		for _, p := range phases {
			samples = append(samples, metrics.Sample{
				Labels: []metrics.Label{{Name: "root", Value: p.root}, {Name: "phase", Value: p.phase}},
				Value:  o.rootSeconds[p],
			})
		}
		set = append(set, metrics.Family{
			Name:    "truss_root_duration_seconds",
			Help:    "Seconds one root spent in one step of its apply (init, plan, show, apply). The counter that moves when a pass is slow rather than stuck.",
			Samples: samples,
		})
	}

	if len(o.rootChanges) > 0 {
		set = append(set, metrics.Family{
			Name:    "truss_root_resource_changes",
			Help:    "Resource changes in the plan truss applied for this root.",
			Samples: intSamples("root", o.rootChanges),
		})
	}
	if len(o.rootFailures) > 0 {
		set = append(set, metrics.Family{
			Name:    "truss_root_failures",
			Help:    "Times this root failed or was refused this pass.",
			Samples: intSamples("root", o.rootFailures),
		})
	}

	set = append(set,
		metrics.Family{
			Name:    "truss_rotation_ran",
			Help:    "1 when this pass re-applied the credentials root. Rotation is an empty plan until a generation boundary is crossed, so this being 1 does not mean anything rotated -- truss_rotation_changes says that.",
			Samples: []metrics.Sample{{Value: one(o.rotationRan)}},
		},
		metrics.Family{
			Name:    "truss_rotation_ok",
			Help:    "1 when rotation ran and reported no error.",
			Samples: []metrics.Sample{{Value: one(o.rotationOK)}},
		},
		metrics.Family{
			Name:    "truss_rotation_changes",
			Help:    "Resource changes the rotation apply made. Above zero on the day a generation boundary is crossed, and zero on every other day.",
			Samples: []metrics.Sample{{Value: float64(rep.RotatedChanges)}},
		},
		metrics.Family{
			Name:    "truss_publish_attempted",
			Help:    "1 when this pass contacted the publisher sidecar.",
			Samples: []metrics.Sample{{Value: one(o.publishAttempt)}},
		},
		metrics.Family{
			Name:    "truss_publish_ok",
			Help:    "1 when the publisher answered without an error.",
			Samples: []metrics.Sample{{Value: one(o.publishOK)}},
		},
		metrics.Family{
			Name:    "truss_publish_expiries",
			Help:    "Expiry dates the publisher recorded on the value it wrote.",
			Samples: []metrics.Sample{{Value: float64(o.publishExpiries)}},
		},
		metrics.Family{
			Name:    "truss_delivery_published",
			Help:    "1 when this pass fast-forwarded the delivery ref to the commit it just gated. 0 covers a refusal, a forge or push error, and a tree with no delivery units at all -- see truss_delivery_ref_unprotected for the one of those that matters.",
			Samples: []metrics.Sample{{Value: one(o.deliveryPublished)}},
		},
		metrics.Family{
			// ⚠️ THIS IS THE ONE THAT MATTERS: gated commits keep applying and
			// nothing is telling a reconciler about any of them. Never 1 for a
			// tree with no delivery units -- deliveryRefIsUnprotected's own
			// comment says why conflating the two would be the wrong alarm for
			// somebody who never asked for the feature.
			Name:    "truss_delivery_ref_unprotected",
			Help:    "1 when this pass refused to publish because the rulesets on the delivery ref do not make it a gated path to production: none applies, one lacks non_fast_forward, deletion or update, its bypass list names an actor other than the applier's own App, or that list could not be read. Never 1 for a tree with no delivery units -- that is a different fact from delivery being broken.",
			Samples: []metrics.Sample{{Value: one(o.deliveryRefUnprotected)}},
		},
	)

	// ⚠️ THERE IS NO truss_pass_drift_run, AND THERE WAS. It said "this is
	// the daily pass", which the grouping key already says as pass="drift" --
	// two ways to ask one question, and the one that could disagree with the
	// other. truss_drift_ran is a different fact and is why it survived: the
	// daily pass SKIPS drift when the gate failed or the repository could not
	// be prepared, so "this is the drift pass" and "drift actually ran" come
	// apart on exactly the passes worth looking at.
	set = append(set, metrics.Family{
		Name:    "truss_drift_ran",
		Help:    "1 when this pass planned every root and applied none of them. Not the same as being the daily pass: a daily pass that could not prepare the repository skips drift entirely.",
		Samples: []metrics.Sample{{Value: one(o.driftRan)}},
	})
	if drift := nameSamples("root", rep.Drifted); len(drift) > 0 {
		set = append(set, metrics.Family{
			Name:    "truss_root_drifted",
			Help:    "1 for each root whose live infrastructure no longer matches the configuration. Nothing is applied for it: drift is reported, never reconciled.",
			Samples: drift,
		})
	}
	if errored := nameSamples("root", rep.Errored); len(errored) > 0 {
		set = append(set, metrics.Family{
			Name:    "truss_root_drift_errored",
			Help:    "1 for each root the drift pass could not plan at all. Not the same fact as drifted, and never reported as one.",
			Samples: errored,
		})
	}

	// The credential sweep. ⚠️ THE SWEEP REPORTS ONLY WHAT IS WITHIN
	// EXPIRY_WARN_DAYS OR HAS NO RECORDED EXPIRY, so a credential with
	// plenty of life left has no series here at all. That is why the alert
	// is written on the series that EXIST rather than on an absence -- and
	// why truss_expiry_sweep_ok has to be read first: a sweep that could not
	// run reports no findings, which looks exactly like a clean one.
	set = append(set, metrics.Family{
		Name:    "truss_expiry_sweep_ok",
		Help:    "1 when the daily sweep completed and earned its answer. 0 means the findings below are incomplete, not that nothing is expiring.",
		Samples: []metrics.Sample{{Value: one(driftRun && rep.ExpiryUnavailable == "")}},
	})
	set = append(set, metrics.Family{
		Name:    "truss_expiry_findings",
		Help:    "Credentials the sweep reported: within the warning window, past it, or with no recorded expiry at all.",
		Samples: []metrics.Sample{{Value: float64(len(rep.Expiring))}},
	})

	var daysLeft, unrecorded []metrics.Sample
	for _, e := range rep.Expiring {
		l := []metrics.Label{{Name: "credential", Value: e.Name}}
		if e.DaysLeft == nil {
			unrecorded = append(unrecorded, metrics.Sample{Labels: l, Value: 1})
			continue
		}
		daysLeft = append(daysLeft, metrics.Sample{Labels: l, Value: float64(*e.DaysLeft)})
	}
	if len(daysLeft) > 0 {
		set = append(set, metrics.Family{
			Name:    "truss_credential_days_left",
			Help:    "Days until this credential expires. Negative means it already has.",
			Samples: daysLeft,
		})
	}
	if len(unrecorded) > 0 {
		set = append(set, metrics.Family{
			Name:    "truss_credential_expiry_unrecorded",
			Help:    "1 for each credential with no expiry recorded at all. Not a synonym for `does not expire`: `never` is a recorded value and does not appear here.",
			Samples: unrecorded,
		})
	}

	set = append(set, metrics.Family{
		Name: "truss_pass_log_events",
		Help: "Lines this pass logged at each level. The counter behind the error and warning panels, so a pass that warned about something it then recovered from is still visible.",
		Samples: []metrics.Sample{
			{Labels: []metrics.Label{{Name: "level", Value: levelWarn}}, Value: float64(o.logEvents[levelWarn])},
			{Labels: []metrics.Label{{Name: "level", Value: levelError}}, Value: float64(o.logEvents[levelError])},
		},
	})

	set = append(set, metrics.Family{
		Name: "truss_build_info",
		Help: "The build that produced these numbers. Always 1; the labels are the payload.",
		Samples: []metrics.Sample{{
			Labels: []metrics.Label{
				{Name: "go_version", Value: build.GoVersion},
				{Name: "revision", Value: build.Revision},
			},
			Value: 1,
		}},
	})

	return set
}

// classSamples emits one series per known class, present or not -- see
// failureClasses' own note on why the absent ones are written as 0.
func classSamples(seen map[string]bool) []metrics.Sample {
	samples := make([]metrics.Sample, 0, len(failureClasses))
	for _, c := range failureClasses {
		v := 0.0
		if seen[c] {
			v = 1
		}
		samples = append(samples, metrics.Sample{
			Labels: []metrics.Label{{Name: "class", Value: c}}, Value: v,
		})
	}
	return samples
}

func boolSamples(label string, m map[string]bool) []metrics.Sample {
	keys := sortedKeys(m)
	samples := make([]metrics.Sample, 0, len(keys))
	for _, k := range keys {
		v := 0.0
		if m[k] {
			v = 1
		}
		samples = append(samples, metrics.Sample{
			Labels: []metrics.Label{{Name: label, Value: k}}, Value: v,
		})
	}
	return samples
}

func intSamples(label string, m map[string]int) []metrics.Sample {
	keys := sortedKeys(m)
	samples := make([]metrics.Sample, 0, len(keys))
	for _, k := range keys {
		samples = append(samples, metrics.Sample{
			Labels: []metrics.Label{{Name: label, Value: k}}, Value: float64(m[k]),
		})
	}
	return samples
}

// nameSamples turns a list of names into one 1-valued series each. The list
// is deduplicated: the same root named twice would otherwise render the same
// series twice and Render would refuse the whole push.
func nameSamples(label string, names []string) []metrics.Sample {
	seen := map[string]bool{}
	var samples []metrics.Sample
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		samples = append(samples, metrics.Sample{
			Labels: []metrics.Label{{Name: label, Value: n}}, Value: 1,
		})
	}
	return samples
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// pushPassMetrics renders and pushes the set, and reports what went wrong so
// the caller can log it.
//
// ⚠️ THE GROUPING KEY SEPARATES THE TWO PASSES, AND IT HAS TO. The frequent
// pass and the daily drift pass describe different work: one applies commits
// and never rotates, the other rotates and never applies. Pushed under one
// grouping key they would overwrite each other every five minutes, so the
// rotation and expiry series -- written once a day -- would survive for the
// length of one frequent pass and then vanish. The pass is part of the key,
// not a label in the body, because the Pushgateway derives the body's labels
// from the key and refuses a body that contradicts it.
func pushPassMetrics(ctx context.Context, baseURL string, driftRun bool, set metrics.Set) error {
	body, err := metrics.Render(set)
	if err != nil {
		return err
	}
	pass := "frequent"
	if driftRun {
		pass = "drift"
	}
	return metrics.Push(ctx, baseURL, []metrics.Label{
		{Name: "job", Value: "truss"},
		{Name: "pass", Value: pass},
	}, body)
}

// buildInfo reads what the toolchain stamped into this binary.
//
// ⚠️ vcs.revision IS ONLY PRESENT WHEN THE BUILD SAW A GIT CHECKOUT, and the
// release image builds from a copied tree, so it is routinely absent. It is
// reported as the empty string rather than as a guess: "which commit is
// running" is answered authoritatively by the image digest pinned in the
// manifest, and a plausible-looking wrong revision on a dashboard is worse
// than no revision at all.
func buildInfo() buildFacts {
	facts := buildFacts{}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return facts
	}
	facts.GoVersion = info.GoVersion
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			facts.Revision = s.Value
		}
	}
	return facts
}
