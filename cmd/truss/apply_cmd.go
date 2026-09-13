package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/beeradb/truss/internal/config"
	"github.com/beeradb/truss/internal/deadman"
	"github.com/beeradb/truss/internal/forge"
	"github.com/beeradb/truss/internal/gates"
	"github.com/beeradb/truss/internal/handoff"
	"github.com/beeradb/truss/internal/ledger"
	"github.com/beeradb/truss/internal/notify"
	"github.com/beeradb/truss/internal/plan"
	"github.com/beeradb/truss/internal/repo"
	"github.com/beeradb/truss/internal/secrets"
)

// tofuRunner is the subset of plan.Runner's methods the apply pass calls.
// plan.Runner satisfies this by its method set alone (Go's structural
// typing needs no explicit declaration); tests supply a fake instead of
// shelling out to a real `tofu` binary.
type tofuRunner interface {
	Init(ctx context.Context, dir string) error
	Plan(ctx context.Context, dir, outFile string) error
	PlanDetailed(ctx context.Context, dir string) (bool, error)
	Apply(ctx context.Context, dir, planFile string) error
	ShowJSON(ctx context.Context, dir, planFile string) ([]byte, error)
}

// tofuFactory builds a tofuRunner for one root apply, given that root's
// exact child environment (§2 item 9: Runner.Env is never inherited
// implicitly, so building it explicitly, per call, is this factory's job).
type tofuFactory func(env []string) tofuRunner

// forgeGateway is the subset of *forge.Client the pass calls, named so
// tests can see at a glance what apply asks of the forge without importing
// the whole client.
type forgeGateway interface {
	Protection(ctx context.Context, branch string) (gates.Protection, error)
	Rulesets(ctx context.Context, branch string) (gates.Rulesets, error)
	// AppID performs no I/O: it returns the id this client was already
	// configured with. gates.CheckDeliveryRef needs it to recognise the
	// applier's own App as a ruleset's bypass actor, and internal/gates does
	// no I/O of its own.
	AppID() int64
	InstallationToken(ctx context.Context) (string, time.Time, error)
	PullNumbersForCommit(ctx context.Context, sha string) ([]int, error)
	PullRequest(ctx context.Context, number int) (gates.PullRequest, error)
	Reviews(ctx context.Context, number int) ([]gates.Review, error)
	Commit(ctx context.Context, sha string) (gates.Commit, error)
}

var _ forgeGateway = (*forge.Client)(nil)

// applyDeps bundles everything the pass needs, real or faked. cmdApply
// builds the real set; tests build their own.
type applyDeps struct {
	Cfg      config.Config
	Dir      secrets.Dir
	Journal  *ledger.Journal
	Forge    forgeGateway
	Telegram notify.Telegram
	Git      gitDriver
	NewTofu  tofuFactory
	// NewRender builds the Kustomize runner for a delivery unit. Separate
	// from NewTofu because the two kinds are different executors with
	// different environments -- a render gets no credentials at all, since
	// rendering reads nothing but the tree.
	NewRender renderFactory
	// NewAnsible builds the ansible-playbook runner for a play. Separate
	// from NewRender and NewTofu for the same reason those are separate
	// from each other: three kinds, three executors, three environments --
	// and this one's is the narrowest, because a play's tasks run on
	// somebody else's machine (see ansibleEnv).
	NewAnsible ansibleFactory
	// Tailnet lists the devices on the tailnet. It is what backs ONE
	// provider of host evidence for the ansible target gate -- see
	// hostEvidence, and evidenceProviders, which is what turns this into
	// one. Nil means no tailscale credential is mounted, in which case the
	// tailscale provider simply does not exist this pass, and any host
	// that is reached that way is refused rather than treated as having no
	// unknown devices near it: a gate with no evidence is a gate that
	// passes.
	Tailnet tailnetLister
	// Dial is how the declared-address evidence provider reaches a
	// machine. Nil means a real net.Dialer, which is what production uses;
	// a test supplies its own so that "this host is down" costs neither a
	// DNS lookup nor a real timeout, and so the suite does not depend on
	// what the machine running it can route to.
	//
	// ⚠️ IT IS A SEAM, NOT A KNOB. Nothing reads it from configuration and
	// nothing should: which addresses the applier may dial is decided by
	// the reviewed inventory, and HOW it dials them has one correct answer.
	Dial        dialer
	Now         func() time.Time
	Stderr      io.Writer
	VaultConfig secrets.KVConfig
	// CloudflareBaseURL overrides the Cloudflare API host for the expiry
	// sweep's probe; empty is the real one. See runExpirySweep.
	CloudflareBaseURL string
	// Token is the GitHub App installation token minted once per pass. It
	// reaches git through the environment (gitDriver.WithToken) and tofu as
	// GH_TOKEN (buildBaseEnv). Set by runApplyPass; empty before that.
	Token string
	// PATH and HOME are copied explicitly from the environment cmdApply was
	// given, so tofu's own child process (Runner.Env, which never inherits
	// implicitly -- §2 item 9) can still find the tofu binary and its
	// plugin cache.
	PATH, HOME string
	// CollectionsPath is ANSIBLE_COLLECTIONS_PATH, passed through to a
	// play by name. See ansibleCollectionsPath for why an image's own ENV
	// cannot do this job.
	CollectionsPath string
	// HandoffSocket is the path to the publisher's Unix socket
	// (design publisher-identity-design.md §3). Set by loadHandoffConfig,
	// which refuses to start rather than leave it empty on the one pass
	// that needs it -- see that function's own doc. Empty on a drift run,
	// where it must never be read at all.
	HandoffSocket string
	// Handoff sends one publish request and returns the publisher's
	// verdict, matching internal/handoff.Send's signature exactly so
	// cmdApply can wire the real function and a test can fake it without a
	// real socket.
	Handoff func(ctx context.Context, path string, timeout time.Duration, r handoff.Request) (handoff.Response, error)

	// Obs accumulates what this pass observed, for the metrics written
	// beside the heartbeat at its tail. runApplyPass installs it; every
	// method on it is nil-safe, so a test driving one step of a pass
	// directly records nothing rather than needing a recorder for a pass it
	// is not running. Nothing reads it back to make a decision.
	Obs *passObs
}

// handoffTimeout bounds one publish exchange from the truss side. It
// matches internal/handoff's own connDeadline (30s), which that package's
// doc ties to the timeout internal/secrets already uses for a single Vault
// HTTP call -- the publisher's own work is at most a handful of those.
const handoffTimeout = 30 * time.Second

// lockLeakAfter is how long a state lock may be held before truss stops
// reading it as another applier working and starts reading it as one that
// died holding it.
//
// ⚠️ CONTENTION AND A LEAK LOOK IDENTICAL, AND THE WRONG READING COSTS DAYS.
// A backend lock is an object with no lease and no expiry: nothing reclaims
// it when the holder is killed. Deferring is right for a live holder and
// wrong for a corpse -- measured 2026-09-07, a lock taken by a pod that no
// longer existed blocked every pass for sixteen hours while each one
// reported a plausible reason to wait and exited 0.
//
// Thirty minutes is longer than any pass this has been run against and far
// shorter than that incident. Truss still never breaks a lock by itself:
// deciding one is stale means deciding nobody is mid-apply, and being wrong
// corrupts state. It says so, loudly, and leaves the breaking to a person.
const lockLeakAfter = 30 * time.Minute

// staleLockReason returns the failure for a lock held past lockLeakAfter, or
// empty for contention that is still plausibly live -- including a lock whose
// message carried no readable age, which must stay contention rather than
// become a guess.
func (d applyDeps) staleLockReason(root string, err error) string {
	var busy *plan.LockBusyError
	if !errors.As(err, &busy) || busy.Info.Created.IsZero() {
		return ""
	}
	held := d.now().Sub(busy.Info.Created)
	if held < lockLeakAfter {
		return ""
	}
	// The holder goes to the log, not to the reason: the reason is written
	// to the ledger and sent to a chat, and Who is a hostname.
	d.logf("state lock on %s was taken by %s and has not been released for %s", root, busy.Info.Who, held.Round(time.Minute))
	d.Obs.failed(classLock)
	return fmt.Sprintf("the state lock for %s has been held for %s, far longer than a pass takes -- its holder is gone, not working. Confirm nothing is mid-apply, then clear lock %s with `tofu force-unlock`; the next pass applies it.",
		root, held.Round(time.Minute), busy.Info.ID)
}

func (d applyDeps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// logf narrates one step of the pass to stderr. The deployed bash applier
// emits the same nine lines, and they are the only per-step visibility into a
// pass that runs unattended every five minutes -- without them a hung
// `tofu apply` and a pass that did nothing look identical in a pod log.
// See log for the line's shape and why it has a level in it.
func (d applyDeps) logf(format string, args ...any) {
	d.log(levelInfo, format, args...)
}

// warnf narrates something that went wrong and did not stop the pass: an
// alert that could not be sent, a monitor that did not answer. Every one of
// these was previously indistinguishable from narration in a pod log, and
// counted for nothing.
func (d applyDeps) warnf(format string, args ...any) {
	d.log(levelWarn, format, args...)
}

// errorf narrates something that went wrong and LOST SOMETHING: a ledger
// object that was not written, a durable record that now does not exist. The
// pass may still finish -- most of these are best-effort writes on a path
// where the alert matters more than the object -- but the evidence is gone
// either way, so it is not a warning.
func (d applyDeps) errorf(format string, args ...any) {
	d.log(levelError, format, args...)
}

// The three levels. Their whole job is to be a FIELD rather than a tone of
// voice, so "show me every warning this week" is a query instead of a grep
// for words somebody happened to choose.
const (
	levelInfo  = "info"
	levelWarn  = "warn"
	levelError = "error"
)

// log writes one narration line and counts it.
//
// ⚠️ LOGFMT, NOT PROSE WITH A TIMESTAMP IN FRONT OF IT, AND THE LEVEL IS THE
// REASON. The old line was `[15:04:05] <message>`: readable, and carrying
// nothing a log pipeline can filter on, so "every warning today" was a grep
// for whichever words the message happened to use. `level=warn` is a field
// two of these lines have and the other twelve do not. The message stays one
// quoted prose value rather than being broken into attributes, so `kubectl
// logs` is still read by a person -- structured attributes per call site
// (commit, root, duration) remain the docs/work-items.md entry they were,
// and this change does not pretend to be it.
//
// ⚠️ STDERR, NOT STDOUT, WHICH IS A DELIBERATE DIVERGENCE FROM THE BASH.
// apply.sh's log() writes to stdout; truss's stdout carries the notify
// payload (apply_cmd.go prints result.notifyText there), so narration on
// stdout would contaminate a machine-readable channel. internal/parity
// compares ledger writes and exec calls, never stdio, so nothing in the
// corpus depends on this.
func (d applyDeps) log(level, format string, args ...any) {
	d.Obs.logged(level)
	fmt.Fprintf(d.Stderr, "time=%s level=%s msg=%s\n",
		d.now().UTC().Format("15:04:05"), level,
		strconv.Quote(fmt.Sprintf(format, args...)))
}

// loadHandoffConfig reads $HANDOFF_SOCKET, whose presence depends on which
// pass this is (design publisher-identity-design.md §3, §8).
//
// ⚠️ IT IS THE DRIFT PASS THAT PUBLISHES, BECAUSE IT IS THE DRIFT PASS THAT
// MINTS. Rotation runs under DRIFT_CHECK=1 and nowhere else -- see runRotation
// and its "ROTATION BELONGS TO THE DAILY PASS" comment -- so the minted value
// exists only there, and the publisher sidecar belongs on that CronJob.
//
// An earlier version had this exactly inverted: the socket was required on the
// FREQUENT pass and forbidden on the drift one, which put the publisher on the
// only pass that never mints anything. It would have run indefinitely, sending
// an empty request every five minutes and publishing nothing, while the daily
// pass that actually rotates had no publisher to hand its value to -- and the
// failure mode is silence, which is the one this whole system is built to
// refuse. Caught before the manifests baked it in.
//
// So: REQUIRED on a drift run, FORBIDDEN otherwise. A manifest that dropped
// the sidecar must be a refusal to start, never a silent stop of publishing;
// and a frequent CronJob that somehow inherited the variable must not sit
// dialling a socket nobody will answer.
//
// Matches config.Load's own fail-closed contract: every problem reported,
// nothing defaulted or guessed.
func loadHandoffConfig(getenv func(string) string, driftOnly bool) (string, []string) {
	socket := getenv("HANDOFF_SOCKET")
	if !driftOnly {
		if socket != "" {
			return "", []string{"refusing to start: $HANDOFF_SOCKET must not be set on a frequent pass -- only the drift pass rotates, so only it has a publisher container to contact"}
		}
		return "", nil
	}
	if socket == "" {
		return "", []string{"refusing to start: $HANDOFF_SOCKET is unset -- the drift pass rotates and must hand its result to the publisher"}
	}
	return socket, nil
}

// cmdApply replaces apply.sh in full (§4.9, landing at step 5). Everything
// up to and including finding HEAD is a boot-time refusal with no
// heartbeat -- §2 item 5, "applied/HEAD is never guessed", is a refusal to
// START, the same class as an invalid config, and the reference bash
// itself never writes a heartbeat for either. Once HEAD is known, the pass
// always writes one (§2 item 8), which is runApplyPass's job and why this
// function stops doing its own error handling the moment that call is
// made.
func cmdApply(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: truss apply")
		return 2
	}

	cfg, problems := config.Load(getenv)
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(stderr, p)
		}
		return 1
	}

	dir := secrets.Dir{Root: cfg.SecretsDir}

	store, err := buildLedgerStore(cfg)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	forgeClient, err := buildForgeClient(cfg, getenv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	tg, err := loadTelegram(dir, getenv("TELEGRAM_API_BASE_URL"))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	vcfg, vproblems := loadVaultConfig(getenv)
	if len(vproblems) > 0 {
		for _, p := range vproblems {
			fmt.Fprintln(stderr, p)
		}
		return 1
	}
	handoffSocket, hproblems := loadHandoffConfig(getenv, cfg.DriftOnly)
	if len(hproblems) > 0 {
		for _, p := range hproblems {
			fmt.Fprintln(stderr, p)
		}
		return 1
	}

	journal := &ledger.Journal{Store: store, Layout: layoutFor(cfg)}
	last, err := journal.Head(ctx)
	if err != nil {
		if errors.Is(err, ledger.ErrNotFound) {
			fmt.Fprintf(stderr, "refusing to start: no %s in the ledger -- bootstrap/bootstrap.sh writes it, and it must never be guessed\n", cfg.LedgerHeadKey)
		} else {
			fmt.Fprintf(stderr, "refusing to start: could not read HEAD from the ledger: %v\n", err)
		}
		return 1
	}

	// ⚠️ A MISSING TAILSCALE CREDENTIAL IS NOT AN ERROR HERE, AND IS ALSO
	// NOT FORGIVEN LATER. Most deployments have no tailnet and no plays, and
	// demanding the credential at startup would stop them on upgrade for a
	// feature they never asked for. So the client is nil, and
	// runAnsibleUnits refuses on nil the moment a play exists -- the refusal
	// lands on the commit that introduces a play rather than on every pass
	// of every deployment, which is where it is both correct and actionable.
	tsClient, err := loadTailnetClient(dir, getenv("TAILSCALE_API_BASE_URL"))
	if err != nil {
		fmt.Fprintf(stderr, "refusing to start: %v\n", err)
		return 1
	}

	deps := applyDeps{
		Cfg:      cfg,
		Dir:      dir,
		Journal:  journal,
		Forge:    forgeClient,
		Telegram: tg,
		Git:      execGit{Bin: "git", Dir: cfg.Workdir, Stderr: stderr},
		NewTofu: func(env []string) tofuRunner {
			return plan.Runner{Bin: "tofu", PluginDir: cfg.PluginDir, Stderr: stderr, Env: env}
		},
		NewRender:         newRenderFactory(getenv, stderr),
		NewAnsible:        newAnsibleFactory(getenv, stderr),
		Now:               time.Now,
		Stderr:            stderr,
		VaultConfig:       vcfg,
		CloudflareBaseURL: getenv("CLOUDFLARE_API_BASE_URL"),
		PATH:              getenv("PATH"),
		HOME:              getenv("HOME"),
		CollectionsPath:   ansibleCollectionsPath(getenv),
		HandoffSocket:     handoffSocket,
		Handoff:           handoff.Send,
	}
	// Assigned separately rather than in the literal: a nil *tailnet.Client
	// stored in a non-nil interface is not nil, and runAnsibleUnits' whole
	// refusal turns on `d.Tailnet == nil`. Writing `Tailnet: tsClient` with
	// tsClient a typed nil pointer would sail past that check and then
	// panic, or worse, return an empty device list that reads as "no
	// unknown devices".
	if tsClient != nil {
		deps.Tailnet = tsClient
	}

	result := runApplyPass(ctx, deps, last)
	if result.notifyText != "" {
		fmt.Fprintln(stdout, result.notifyText)
	}
	if result.failure != "" {
		return 1
	}
	return 0
}

// applyResult is everything cmdApply needs to compute an exit code, after
// runApplyPass has already written the heartbeat and sent the alert.
// passFailure is a refusal and the two facts a reader needs to act on it.
//
// ⚠️ IT EXISTS BECAUSE THE ALERT NAMED THE WRONG COMMIT, AND THE BASH DID
// TOO. Both reported the LEDGER POSITION -- the last commit successfully
// applied -- under the words "FAILED at <sha>", so a failure while
// processing the NEXT commit pointed a reader at the previous one. Observed
// 2026-09-10 on a live applier: it reported FAILED at f42f97f while choking
// on a `provider "tailscale" {}` block that exists only in the commit after
// it, and the person debugging it had to diff two trees to find that out.
// The recorded bash corpus does the same thing (every failure alert in
// internal/parity/testdata says "at base"), so correcting it is a declared
// divergence rather than a bug fix -- see WRONG-COMMIT-IN-FAILURE-ALERT.
//
// Head is separate from SHA because the applier PLANS AT THE BRANCH HEAD
// while working through the queue one commit at a time, so the tree that
// produced a tofu error is not in general the tree of the commit whose turn
// it was. Naming only one of the two is what made the live case take a
// two-tree diff to explain.
type passFailure struct {
	// Reason is the refusal text. Empty means no failure.
	Reason string
	// SHA is the commit whose turn it was. Empty when the failure belongs
	// to no commit -- a protection gate that refused before the loop, a
	// credential that would not mount, a sweep that could not report. An
	// empty SHA makes the alert say "FAILED:" with no commit at all, which
	// is the honest shape: there is no commit to name.
	SHA string
	// Head is the branch head whose tree was checked out and planned.
	// Empty when nothing was planned -- the commit gate refuses before any
	// checkout.
	Head string
}

type applyResult struct {
	failure    string
	notifyText string
}

// runApplyPass is the pass itself: the gate, the commit loop, rotation,
// drift, the expiry sweep, then ALWAYS a heartbeat and an alert (§2 item
// 8) -- there is no return path out of this function that skips them.
func runApplyPass(ctx context.Context, d applyDeps, last string) applyResult {
	// The recorder is installed here and nowhere else, so every function
	// this pass calls shares one -- d is passed by value and Obs is a
	// pointer, which is the same shape credCache already uses to be
	// "at most once per pass".
	started := d.now()
	d.Obs = newPassObs()

	var (
		appliedCount, noopCount int
		failure                 string
		// failedSHA is the commit the failure belongs to, and plannedSHA
		// the branch head whose tree was planned. Both stay empty unless
		// the commit loop is what failed -- see passFailure.
		failedSHA, plannedSHA string
		rotationSummary       any = map[string]string{"skipped": "not a drift run"}
		driftSummary          any = map[string]string{"skipped": "not a drift run"}
		rotatedChanges        int
		// rotationApplied is true only when runRotation actually re-applied
		// the credentials root on THIS pass and reported no error -- never
		// on a skip (no credentials root, state lock held elsewhere) and
		// never on a failure. It is the one signal that decides
		// handoff.Request.PublishValue below: a freshly minted value exists
		// to publish only when rotation itself ran and succeeded.
		rotationApplied  bool
		drifted, errored []string
		driftRun         = d.Cfg.DriftOnly
		queueAdvanced    bool
		driftSkipped     string
	)

	gateOK := false
	var problems []string
	prot, err := d.Forge.Protection(ctx, "main")
	protProblems := 0
	if err != nil {
		problems = append(problems, fmt.Sprintf("could not read branch protection for main: %v", err))
		protProblems++
	} else {
		found := gates.CheckProtection(prot, d.Cfg.RequiredCheck)
		problems = append(problems, found...)
		protProblems += len(found)
	}
	d.Obs.gate("protection", protProblems == 0)
	if protProblems > 0 {
		d.Obs.failed(classProtection)
	}
	// ⚠️ RULESETS ARE READ AND CHECKED ALONGSIDE PROTECTION, NEVER INSTEAD OF
	// IT -- the two are independent controls and either can be the one
	// actually governing the branch. A push straight to this repository's
	// own main on 2026-09-09 was ACCEPTED by a ruleset bypass actor while
	// classic protection read compliant; CheckProtection alone would still
	// say so today. See gates.Ruleset's doc comment for the evidence and
	// docs/work-items.md's ruleset section for the reasoning. So a failure
	// reading rulesets, or a problem CheckRulesets finds, refuses the same
	// way a protection problem does -- same tail, same "refuses everything
	// and alerts" behaviour -- joined into the one sentence below rather
	// than a second, easier-to-miss failure path.
	rulesets, rsErr := d.Forge.Rulesets(ctx, "main")
	rsProblems := 0
	if rsErr != nil {
		problems = append(problems, fmt.Sprintf("could not read rulesets for main: %v", rsErr))
		rsProblems++
	} else {
		found := gates.CheckRulesets(rulesets)
		problems = append(problems, found...)
		rsProblems += len(found)
	}
	d.Obs.gate("rulesets", rsProblems == 0)
	if rsProblems > 0 {
		d.Obs.failed(classRulesets)
	}
	if len(problems) > 0 {
		failure = "branch protection on main does not meet the bar: " + strings.Join(problems, "; ")
	} else {
		gateOK = true
	}
	// protectionOK remembers WHY gateOK went false, so the skip reason names
	// the real cause rather than blaming branch protection for a clone that
	// failed.
	protectionOK := gateOK

	// ⚠️ THE CLONE HAPPENS HERE, BEFORE THE DRIFT BRANCH, AND IT USED TO LIVE
	// INSIDE runCommitLoop -- WHICH A DRIFT RUN SKIPS ENTIRELY. So a
	// drift-only pass never cloned, and the checkout it then tried failed
	// with "chdir /work/repo: no such file or directory". apply.sh calls
	// ensure_workdir unconditionally at top level (apply.sh:207-212), before
	// its own DRIFT_ONLY branch, which is why the bash's drift job works.
	//
	// ⚠️ NO UNIT TEST COULD SEE THIS AND THE PARITY HARNESS COULD NOT EITHER:
	// every fake git succeeds whether or not a clone happened, so "check out
	// a ref in a directory that does not exist" has no counterpart in a fake.
	// It was found by the FIRST SHADOW RUN against the real cluster on
	// 2026-09-08, which is the whole argument for running one.
	// ⚠️ ONE CREDENTIAL CACHE FOR THE WHOLE PASS. runCommitLoop and
	// runRotation each used to build their own, so a pass that applied and
	// then rotated read the same mounted files twice -- against this type's
	// own promise of "at most once per pass". The deployed bash uses a
	// process-wide flag for the same reason. runDrift deliberately does NOT
	// use it: it re-reads cf-infra-admin fresh, which is correct.
	cc := &credCache{dir: d.Dir}

	if gateOK {
		tok, _, err := d.Forge.InstallationToken(ctx)
		if err != nil {
			failure = fmt.Sprintf("could not mint an installation token: %v", err)
			d.Obs.failed(classForge)
			gateOK = false
		} else {
			// Every git call from here on carries the token -- the clone is
			// --filter=blob:none, so checkout and diff lazily fetch blobs and
			// are network operations too. See gitDriver.WithToken.
			d.Git = d.Git.WithToken(tok)
			d.Token = tok
			repoURL := "https://github.com/" + d.Cfg.Repo + ".git"
			if err := d.Git.EnsureClone(ctx, repoURL); err != nil {
				failure = fmt.Sprintf("could not clone %s: %v", d.Cfg.Repo, err)
				d.Obs.failed(classRepo)
				gateOK = false
			} else if err := d.Git.Fetch(ctx, "origin", "main"); err != nil {
				failure = fmt.Sprintf("could not fetch origin main: %v", err)
				d.Obs.failed(classRepo)
				gateOK = false
			}
		}
	}

	if !gateOK {
		skipped := "branch protection gate failed"
		if protectionOK {
			skipped = "the repository could not be prepared"
		}
		rotationSummary = map[string]string{"skipped": skipped}
		driftSummary = map[string]string{"skipped": skipped}
	} else if driftRun {
		// ⚠️ ROTATION BELONGS TO THE DAILY PASS. The deployed applier runs
		// rotate_credentials and then check_drift together under
		// DRIFT_CHECK=1; the daily pass never runs the commit loop, so
		// rotation has nothing from this run to have already applied.
		summary, changes, applied, rotErr := runRotation(ctx, d, last, cc)
		rotationSummary = summary
		rotatedChanges = changes
		rotationApplied = applied && rotErr == nil
		d.Obs.rotated(applied, rotErr == nil)
		if rotErr != nil {
			reason := fmt.Sprintf("rotation of credentials at %s: %v", last, rotErr)

			// ⚠️ FILED UNDER ITS OWN KEY, AND THIS WAS MISSING ENTIRELY.
			// apply.sh:697 writes failed/rotation-<UTC timestamp>. The key
			// is deliberately not a commit sha: rotation is not caused by
			// any particular commit, so filing it against one would blame a
			// commit that did nothing wrong. And it must be durable --
			// the heartbeat carries the same reason but is overwritten five
			// minutes later, so without this the only record of a failed
			// rotation is a Telegram message and a pod log that expires.
			// Found by internal/parity on 2026-09-08.
			//
			// Best-effort, like every other ledger write on the failure
			// path: a bucket that cannot be written must not stop the alert,
			// which is the channel that still reaches somebody when the
			// ledger itself is what broke.
			//
			// The record carries rotation's OWN error, not the prefixed
			// sentence: apply.sh:697 files "$out" and apply.sh:698 adds the
			// "rotation of credentials at <sha>:" prefix only to the failure
			// that becomes the alert. The key already says it was rotation.
			rotKey := "rotation-" + d.now().UTC().Format("20060102T150405Z")
			if err := d.Journal.PutFailed(ctx, rotKey, rotErr.Error()); err != nil {
				d.Obs.ledgerError()
				d.Obs.failed(classLedger)
				d.errorf("could not file %s: %v", rotKey, err)
			}

			d.Obs.failed(classRotation)
			if failure == "" {
				failure = reason
			}
		}

		d.Obs.drifting()
		drifted, errored, driftSkipped = runDrift(ctx, d, last)
		if driftSkipped != "" {
			driftSummary = map[string]string{"skipped": driftSkipped}
		} else {
			driftSummary = map[string]any{"drifted": orEmpty(drifted), "errored": orEmpty(errored)}
		}
	} else {
		// lockContended is deliberately not surfaced beyond stopping the
		// loop early: §2 item 7 says contention files no failed/<sha>,
		// sends no failure alert and leaves HEAD unmoved, which
		// runCommitLoop already guarantees by returning an empty failure
		// and the pre-contention HEAD.
		newLast, applied, noop, loopFailure, contended := runCommitLoop(ctx, d, last, cc)
		if contended {
			d.Obs.contended()
		}
		queueAdvanced = newLast != last
		last = newLast
		appliedCount = applied
		noopCount = noop
		if loopFailure.Reason != "" {
			failure = loopFailure.Reason
			// ⚠️ THE COMMIT AND THE HEAD TRAVEL WITH THE REASON, so the
			// alert can name the commit that failed instead of the ledger
			// position. Only set here, and only from the loop: a failure
			// arising anywhere else in this pass belongs to no commit, and
			// leaving these empty is what makes the alert say so rather
			// than pointing at whichever commit happened to be last.
			failedSHA, plannedSHA = loopFailure.SHA, loopFailure.Head
		}

		// ⚠️ THE FREQUENT PASS DOES NOT ROTATE, AND TRUSS HAD THIS INVERTED.
		// The deployed applier rotates on the DAILY pass and skips here, for
		// a stated cost: three credential reads and a full plan of
		// credentials/ every fifteen minutes was "most of the daily budget,
		// spent to re-derive a date". Rotating here ran 288 plans a day
		// instead of one, and left the daily pass never rotating at all.
		// The skip string is the bash's, byte for byte -- it lands in the
		// heartbeat.
		rotationSummary = map[string]string{"skipped": "rotation runs on the daily pass"}
		driftSummary = map[string]string{"skipped": "not a drift run"}
	}

	// Publishing the delivery ref is the last thing the queue does, and only
	// when the queue is clean: a reconciler tracking this ref must never see
	// a commit this pass refused.
	//
	// ⚠️ NOT ON EVERY PASS, AND THE REASON IS A BUDGET RATHER THAN TIDINESS.
	// Reading the rulesets that protect the ref is a forge call, and the
	// frequent pass runs every five minutes -- asking 288 times a day to
	// re-answer a question that only changes when somebody edits repository
	// settings is the shape of spending that exhausted a service account's
	// hourly allowance on 2026-09-07. So it runs when the queue actually
	// moved, plus once on the daily pass, which is what makes a ref left
	// behind by an earlier failure heal itself rather than wait for the next
	// commit to arrive.
	//
	// A push that fails is a pass failure: the commits applied, and the
	// cluster was not told. HEAD has already advanced, so nothing will retry
	// those commits -- the daily publish is what closes that, and the alert
	// is what makes somebody look before then.
	if failure == "" && (queueAdvanced || driftRun) {
		if reason := publishDeliveryRef(ctx, d, last); reason != "" {
			failure = reason
		}
	}

	// The publisher handoff (design publisher-identity-design.md §3, §9):
	// required on this pass, forbidden on a drift run -- loadHandoffConfig
	// already refused to start otherwise, so d.HandoffSocket is exactly one
	// of "set" or "this branch never runs". Every branch above, including
	// the gate-failure one, falls through to here with no early return, so
	// this call is reached on every path a DRIFT pass can take: gate failed,
	// the repo could not be prepared, rotation succeeded, or rotation failed.
	// See runHandoff's own doc for why that guarantee is the whole point --
	// a pass that finishes without contacting the publisher leaves it waiting
	// until its deadline, and concurrencyPolicy: Forbid then silently
	// suppresses every pass after it.
	if driftRun {
		// PublishValue is the one signal rotationApplied exists to carry:
		// true only when runRotation actually re-applied credentials/ on
		// THIS pass and reported no error. A skip (no credentials root, the
		// state lock held elsewhere) or a failure both leave it false --
		// there is nothing freshly minted to publish either way, and
		// publishing whatever rotation last succeeded at, on a pass where
		// THIS attempt failed, would publish a value nothing here has just
		// verified against Cloudflare.
		req := handoff.Request{PublishValue: rotationApplied}
		d.Obs.publishAttempted()
		if pubFailure := runHandoff(ctx, d, req, last); pubFailure != "" {
			// Filed under its own key for the same reason a rotation
			// failure already is (see the rotKey comment above): not a
			// commit's fault, and the heartbeat alone is overwritten five
			// minutes later.
			rotKey := "rotation-" + d.now().UTC().Format("20060102T150405Z")
			if err := d.Journal.PutFailed(ctx, rotKey, pubFailure); err != nil {
				d.Obs.ledgerError()
				d.Obs.failed(classLedger)
				d.errorf("could not file %s: %v", rotKey, err)
			}
			d.Obs.failed(classPublish)
			if failure == "" {
				failure = pubFailure
			}
		}
	}

	// The expiry sweep runs on the DAILY pass only, which is what the
	// deployed applier does -- see the ⚠️ below for why, and for what
	// running it every pass cost. §4.7, §2 item 16: the sweep never reports
	// a clean bill it did not earn. Its problem is REPORTED, never
	// swallowed as "nothing is expiring" -- but it does not set failure.
	//
	// ⚠️ IT USED TO SET failure, AND THAT WOULD HAVE MADE EVERY PRODUCTION
	// PASS RED. Nothing seeds `expires` into Vault yet, so the sweep's
	// "lists N items but not one records an expiry" fires on every run:
	// exit 1 and a Telegram FAILED every five minutes, ~288 a day. Both
	// 2026-09-08 reviewers called that a security cost rather than noise --
	// an alert channel nobody reads is where a real digest-gate refusal goes
	// to die -- and it also undid §2 item 7, because a contended pass
	// correctly leaves failure empty and this then filled it in. The
	// reference bash never set failure for it either.
	//
	// ⚠️ AND THE FINDINGS ARE TAKEN EVEN WHEN THE SWEEP ERRORED. Sweep.Run's
	// contract (and TestAPartialSweepReturnsBothItsFindingsAndItsError) is
	// that a caller gets BOTH; the Cloudflare probe runs first precisely so
	// the hand-made mint token's expiry survives an unreadable mount, and
	// the previous code then discarded it. That threw away the one
	// credential whose lapse takes the applier down, exactly when the vault
	// was misbehaving.
	// ⚠️ DAILY, AS THE DEPLOYED APPLIER IS: apply.sh:1110 is
	// `[ "$DRIFT_ONLY" != "1" ] || check_credential_lifetimes`. Truss ran it
	// every pass instead, which is the pattern that caused an outage -- the
	// sweep lists both vaults and reads every item's `expires`, a question
	// whose answer cannot change inside a day, and apply.sh:1081 records
	// that asking it 288 times a day "is most of what rate-limited the
	// service account on 2026-09-07".
	//
	// ⚠️ DAILY IS NOT THE SAME AS GATED. It sits outside the gate_ok
	// branching, so it still runs on a daily pass whose branch-protection
	// gate failed -- a gate failure and an unusable sweep appear in the same
	// alert without either explaining the other, which
	// TestAnUnusableExpirySweepIsReportedAndDoesNotFailThePass pins.
	var expiring []secrets.Expiring
	var expiryUnavailable string
	if driftRun {
		var sweepErr error
		expiring, sweepErr = runExpirySweep(ctx, d.Cfg, d.Dir, d.VaultConfig, d.CloudflareBaseURL, d.now)
		if sweepErr != nil {
			expiryUnavailable = sweepErr.Error()
		}
	}

	rotationJSON, _ := json.Marshal(rotationSummary)
	driftJSON, _ := json.Marshal(driftSummary)
	var failurePtr *string
	if failure != "" {
		failurePtr = &failure
	}
	hb := ledger.Heartbeat{
		Time:     d.now().UTC().Format("2006-01-02T15:04:05Z"),
		LastSHA:  last,
		Applied:  appliedCount,
		Noop:     noopCount,
		Failure:  failurePtr,
		Rotation: rotationJSON,
		Drift:    driftJSON,
		Expiring: toLedgerExpiring(expiring),
	}
	// The heartbeat write itself is best-effort in the sense that matters
	// here: a failure to WRITE it must not stop the alert from being sent,
	// because the alert is the one channel that still reaches somebody
	// when the ledger itself is the thing that broke.
	if err := d.Journal.PutHeartbeat(ctx, hb); err != nil {
		d.Obs.ledgerError()
		d.Obs.failed(classLedger)
		if failure == "" {
			failure = fmt.Sprintf("writing the heartbeat: %v", err)
		}
	}

	report := notify.Report{
		Subject:           defaultAlertSubject,
		LastSHA:           last,
		FailedSHA:         failedSHA,
		PlannedSHA:        plannedSHA,
		Applied:           appliedCount,
		Noop:              noopCount,
		Failure:           failure,
		DriftRun:          driftRun,
		DriftSkipped:      driftSkipped,
		Drifted:           drifted,
		Errored:           errored,
		RotatedChanges:    rotatedChanges,
		Expiring:          toNotifyExpiring(expiring),
		ExpiryUnavailable: expiryUnavailable,
	}
	text := notify.Compose(report)

	// silent asks notify itself whether Compose has anything at all to say --
	// no failure, nothing applied or no-opped, and not one of the appended
	// clauses. Asked of the Report rather than recomputed here, and never
	// sniffed out of the rendered text, which is the mistake AGENTS.md warns
	// against ("gate on the field, never rendered text").
	//
	// ⚠️ IT WAS "DID ANYTHING APPLY", AND THAT DISCARDED THE DAILY REPORT. A
	// drift pass applies nothing by definition, so with a ping URL configured
	// every DRIFT, EXPIRING and EXPIRY NOT CHECKED clause went in the bin
	// while the monitor stayed green -- the exact shape of failure this
	// repository calls worse than none, since it reports success.
	silent := report.Silent()

	// The dead-man's-switch ping: every completed pass reaches this tail
	// exactly once (same guarantee as the heartbeat and the alert below), so
	// pinging here, unconditionally on d.Cfg.HeartbeatPingURL being set,
	// covers success, refusal and idle alike -- it answers only "did the
	// pass finish", never what it found, which is what the chat message is
	// for.
	//
	// ⚠️ NON-FATAL, LIKE THE TELEGRAM SEND BELOW: a monitor that did not hear
	// from us is the monitor's own problem to alert on, and failing this pass
	// over it would make the liveness signal less reliable than the thing it
	// exists to make more reliable.
	//
	// Safe to log the error: deadman.Ping never returns one that carries the
	// URL (see its own doc) -- the URL is a bearer secret and must never
	// reach a log line.
	pinged := false
	if d.Cfg.HeartbeatPingURL != "" {
		if err := deadman.Ping(ctx, d.Cfg.HeartbeatPingURL); err != nil {
			d.warnf("heartbeat ping failed (non-fatal): %v", err)
		}
		pinged = true
	}

	// The one-sentence rule: when a dead-man's-switch is configured, a pass
	// with nothing to report pings it instead of messaging. Every other pass
	// -- a failure, one that applied or no-opped something, or one carrying a
	// drift, rotation or expiry clause -- still messages exactly as it does
	// today, and a pass with no HeartbeatPingURL configured always messages,
	// unconditionally, which is the regression guard for every deployment
	// that has not opted in.
	//
	// ⚠️ A FAILED PING STILL SUPPRESSES THE IDLE MESSAGE, AND THAT IS NOT AN
	// OVERSIGHT. `pinged` means the ping was ATTEMPTED, not that it landed.
	// Messaging when it fails would be a fallback path -- a second way to
	// report the same fact, taken only sometimes -- and this project has one
	// correct path or none. It is also unnecessary: a ping that did not
	// arrive is a monitor that heard nothing, which is the exact condition a
	// dead-man's-switch exists to alert on. The silence IS the signal, and
	// re-routing it to the channel the switch was adopted to quieten would
	// undo the reason for adopting it.
	if !(pinged && silent) {
		// ⚠️ NON-FATAL, BUT NOT SILENT -- and this line used to be both, under a
		// comment claiming a parity with the bash that it did not have.
		// apply.sh:447 ends its curl with
		// `|| echo "telegram send failed (non-fatal)" >&2`.
		//
		// Non-fatal is right: a broken alert channel must not fail a pass that
		// otherwise succeeded. Silent is not, because Telegram is the channel that
		// reports every OTHER failure -- so a send that dies without a trace is
		// the one signal whose absence looks exactly like good news.
		//
		// Safe to log the error: Telegram.Send redacts the bot token from every
		// error it returns (internal/notify/telegram.go, redactToken), so the URL
		// carrying it cannot arrive here.
		if err := d.Telegram.Send(ctx, text); err != nil {
			d.warnf("telegram send failed (non-fatal): %v", err)
		}
	}

	// The metrics push is the pass's LAST act, after the alert, so the set
	// it renders includes the warning a failed send just produced. Same
	// non-fatal contract as the ping and the send above, and for a stronger
	// reason: a monitoring endpoint that is down must never be able to fail
	// a pass that applied infrastructure correctly.
	//
	// Safe to log the error: metrics.Push never returns one carrying the
	// gateway URL (see its own doc), and metrics.Render's errors name a
	// metric, not a value.
	if d.Cfg.MetricsPushURL != "" {
		set := passMetrics(d.now(), d.now().Sub(started), driftRun, report, d.Obs, buildInfo())
		if err := pushPassMetrics(ctx, d.Cfg.MetricsPushURL, driftRun, set); err != nil {
			d.warnf("metrics push failed (non-fatal): %v", err)
		}
	}

	return applyResult{failure: failure, notifyText: text}
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// toLedgerExpiring copies the sweep's findings into the heartbeat's shape.
// Both are Name + DaysLeft (*int, nil for "no expiry recorded"), so this is
// a copy rather than a reformat -- see the ⚠️ on ledger.Expiring for why it
// used to stringify the number into an "expires" field, and what that broke.
func toLedgerExpiring(in []secrets.Expiring) []ledger.Expiring {
	out := make([]ledger.Expiring, len(in))
	for i, e := range in {
		out[i] = ledger.Expiring{Name: e.Name, DaysLeft: e.DaysLeft}
	}
	return out
}

// tofuUnitsFor derives the credentials/tofu roots a commit touches, the
// sibling of renderUnitsFor (render_unit.go) reading the other half of the
// same repo.TouchedUnits call.
//
// ⚠️ treeUnits HERE IS THE CREDENTIALS/TOFU UNITS OF THE TREE, NOT EVERY
// UNIT -- fed from gitDriver.TreeTofuUnits, which never lists "ansible/plays"
// or a render unit, the same way TreeRenderUnits never lists a tofu one. The
// shared-input branch of TouchedUnits returns everything in whichever tree
// it was handed, so this one returns exactly the credentials/tofu units that
// exist and renderUnitsFor returns exactly the render ones -- the two calls
// partition TouchedUnits' output by construction, never by filtering a
// shared listing after the fact.
func tofuUnitsFor(changedFiles, treeUnits []string) []string {
	var out []string
	for _, u := range repo.TouchedUnits(changedFiles, treeUnits) {
		if u.Kind == repo.KindCredentials || u.Kind == repo.KindTofu {
			out = append(out, u.Path)
		}
	}
	return out
}

// runCommitLoop walks every commit from last (exclusive) to origin/main
// (inclusive), applying each one's touched roots in order. It returns the
// new HEAD, the applied/noop counts, a failure reason (if the pass must
// stop), and whether it stopped because of state-lock contention -- which
// is not a failure (§2 item 7): no failed/<sha> is filed, HEAD is not
// advanced past the contended commit, and the returned failure is empty.
func runCommitLoop(ctx context.Context, d applyDeps, last string, cc *credCache) (newLast string, applied, noop int, failure passFailure, lockContended bool) {
	// The clone, the fetch and the installation token are runApplyPass's job
	// now, done BEFORE the drift branch so both paths get a repository -- see
	// the note there. This function is handed a Git that already carries the
	// token.

	// §3 item 1: a failed rev-list is a refusal here, not the silent empty
	// queue the bash's `mapfile` produced -- the vacuous-pass shape
	// AGENTS.md already has a standing rule against.
	commits, err := d.Git.Commits(ctx, last, "origin/main")
	if err != nil {
		d.Obs.failed(classRepo)
		return last, 0, 0, passFailure{Reason: fmt.Sprintf("could not list commits from %s to origin/main: %v", last, err)}, false
	}

	// Recorded before anything is applied, so it is the depth the pass FOUND
	// rather than what it left behind. A pass that applies three of five
	// commits and then fails reports 5, which is the number somebody wants.
	d.Obs.queued(len(commits))

	baseEnv, err := buildBaseEnv(d, d.Token)
	if err != nil {
		d.Obs.failed(classCredentials)
		return last, 0, 0, passFailure{Reason: err.Error()}, false
	}

	for _, sha := range commits {
		d.logf("considering %s", sha)

		// ⚠️ THE GATE RUNS BEFORE THE ROOTS ARE DERIVED, AND THE ORDER IS THE
		// POINT. It used to run after: a commit whose touched-root set came
		// back empty was recorded as a noop and HEAD advanced past it without
		// the approval, the merged-PR check or the merge-commit signature ever
		// being asked for.
		//
		// That was harmless only for as long as truss was the sole reader of
		// this repository, because a commit touching no root changes nothing
		// truss applies. It is not a statement that nothing was done -- a noop
		// record says TRUSS did nothing -- so the moment anything else reads
		// the tree (a reconciler tracking a ref this applier advances, or a
		// human trusting `applied/` as the record of what reached main), an
		// unreviewed commit was being waved through and filed as uneventful.
		//
		// The cost is three forge calls for every commit, including the ones
		// that touch only docs. That is the honest price of the queue's
		// records meaning what they say.
		headSHA, _, reason, gateErr := checkCommitGate(ctx, d.Forge, d.Cfg.Approver, sha)
		if gateErr != nil {
			reason = gateErr.Error()
		}
		if reason != "" {
			// ⚠️ classCommit, NOT classProtection. Protection is what the
			// branch requires; this is what one commit actually did --
			// merged by exactly one pull request, approved at that precise
			// head sha, signed by the forge. A dashboard that showed them
			// as one number could not tell "somebody weakened the rules"
			// from "somebody got a commit past them".
			d.Obs.failed(classCommit)
			if err := d.Journal.PutFailed(ctx, sha, reason); err != nil {
				d.Obs.ledgerError()
				d.Obs.failed(classLedger)
				reason = reason + fmt.Sprintf(" (and could not record the failure: %v)", err)
			}
			return last, applied, noop, passFailure{Reason: reason, SHA: sha, Head: headSHA}, false
		}

		// ⚠️ THE INVENTORY GATE RUNS HERE, BEFORE ANY ROOT IS DERIVED OR ANY
		// UNIT RENDERED, FOR THE SAME REASON THE COMMIT GATE MOVED AHEAD OF
		// THEM: an inventory-only commit -- one that touches inventory/ or
		// deliveries/ and nothing under platform/ or projects/ -- touches no
		// root and no render unit, so running this after the noop check below
		// would let it be recorded as uneventful and waved through unchecked.
		// See checkInventoryAtCommit's own doc for what it refuses and the
		// two cases it deliberately skips rather than refuses.
		//
		// ⚠️ sha, NOT headSHA. checkInventoryAtCommit diffs whatever commit
		// it is handed against THAT COMMIT'S OWN git first parent
		// (d.Git.Parent), so the commit handed in has to be the one actually
		// sitting in main's history -- sha, the value this loop is walking.
		// headSHA is checkCommitGate's pr.HeadSHA, a PR branch's own head
		// commit: correct for the approval check, which must ask "did the
		// approver review this exact content," but wrong here, where its
		// first parent is whatever the PR branch's own history says came
		// before it, not sha's real predecessor on main. A PR resynced with
		// more than one `git merge origin/main` builds exactly that trap:
		// headSHA's parent is the PR's own prior sync commit, so a stateful
		// placement change that landed on main INSIDE that merge bubble is
		// invisible to headSHA's parent and can surface as a false move
		// against a completely unrelated later commit instead. Measured
		// live 2026-09-12 (platform PR #120, TestInventoryGateComparesMainsCommitNotThePRHeadSHA).
		if reason := checkInventoryAtCommit(ctx, d, sha); reason != "" {
			d.Obs.failed(classConfig)
			if err := d.Journal.PutFailed(ctx, sha, reason); err != nil {
				d.Obs.ledgerError()
				d.Obs.failed(classLedger)
				reason = reason + fmt.Sprintf(" (and could not record the failure: %v)", err)
			}
			return last, applied, noop, passFailure{Reason: reason, SHA: sha, Head: headSHA}, false
		}

		changedFiles, err := d.Git.ChangedFiles(ctx, sha)
		if err != nil {
			d.Obs.failed(classRepo)
			return last, applied, noop, passFailure{Reason: fmt.Sprintf("could not read changed files for %s: %v", sha, err), SHA: sha, Head: headSHA}, false
		}
		// ⚠️ roots COMES FROM repo.TouchedUnits, NOT repo.TouchedRoots.
		// TouchedRoots only ever names credentials, platform and
		// projects/<name> -- it reproduces derive_touched_roots exactly and
		// internal/parity compares it against recordings of that bash
		// function, so it is never widened. But repo.KindOf has classified
		// clusters/<name> and hosts/<name> as KindTofu since the kind layer
		// was added, with no second reader for them: a commit touching only
		// clusters/beta/main.tf produced no roots from TouchedRoots and was
		// filed as a noop with HEAD advanced past it. tofuUnitsFor
		// (render_unit.go's sibling, below) reads the credentials+tofu half
		// of TouchedUnits instead, which does know about them -- see
		// TestTouchedUnitsTofuHalfMatchesTouchedRoots (internal/repo) for why
		// this is a safe swap: for every commit shape the parity corpus
		// covers, the two produce the identical set.
		treeTofuUnits, err := d.Git.TreeTofuUnits(ctx, sha)
		if err != nil {
			d.Obs.failed(classRepo)
			return last, applied, noop, passFailure{Reason: fmt.Sprintf("could not read the tree for %s: %v", sha, err), SHA: sha, Head: headSHA}, false
		}
		roots := tofuUnitsFor(changedFiles, treeTofuUnits)

		// The plays are derived from their own tree listing, the third
		// sibling of the same pattern -- see ansibleUnitsFor on why the
		// three listings partition repo.TouchedUnits rather than filtering
		// one combined set.
		treeAnsibleUnits, err := d.Git.TreeAnsibleUnits(ctx, sha)
		if err != nil {
			d.Obs.failed(classRepo)
			return last, applied, noop, passFailure{Reason: fmt.Sprintf("could not read the plays at %s: %v", sha, err), SHA: sha, Head: headSHA}, false
		}
		plays := ansibleUnitsFor(changedFiles, treeAnsibleUnits)

		// The render units are derived from their own tree listing
		// (gitDriver.TreeRenderUnits), the tofu half's sibling -- both read
		// repo.TouchedUnits and neither touches repo.TouchedRoots, which
		// stays reserved for internal/parity's comparison against the bash.
		treeRenderUnits, err := d.Git.TreeRenderUnits(ctx, sha)
		if err != nil {
			return last, applied, noop, passFailure{Reason: fmt.Sprintf("could not read the render units for %s: %v", sha, err), SHA: sha, Head: headSHA}, false
		}
		renderUnits := renderUnitsFor(changedFiles, treeRenderUnits)

		// ⚠️ plays IS COUNTED HERE, AND FORGETTING IT WAS THE WHOLE DEFECT
		// THIS KIND EXISTS TO CLOSE. repo.KindOf classified ansible/plays/
		// <name> as KindAnsible from the day the kind layer landed, with no
		// executor reading it: a commit touching only ansible/plays/dev-vm/
		// produced no roots and no render units, was logged as "touches no
		// root", and had HEAD advanced past it -- so the machine it was
		// meant to configure was never configured, and nothing said so.
		// Same shape as the clusters/ and hosts/ gap above it, and the same
		// shape as the commit-gate ordering defect before that: a kind the
		// tree understands and the pass does not.
		if len(roots) == 0 && len(renderUnits) == 0 && len(plays) == 0 {
			d.logf("noop: %s touches no root", sha)
			if err := d.Journal.PutNoop(ctx, sha); err != nil {
				d.Obs.ledgerError()
				d.Obs.failed(classLedger)
				return last, applied, noop, passFailure{Reason: fmt.Sprintf("could not record noop for %s: %v", sha, err), SHA: sha, Head: headSHA}, false
			}
			if err := d.Journal.AdvanceHead(ctx, sha); err != nil {
				d.Obs.ledgerError()
				d.Obs.failed(classLedger)
				return last, applied, noop, passFailure{Reason: fmt.Sprintf("could not advance head to %s: %v", sha, err), SHA: sha, Head: headSHA}, false
			}
			last = sha
			noop++
			continue
		}

		if err := d.Git.Checkout(ctx, headSHA); err != nil {
			reason := fmt.Sprintf("could not check out %s: %v", headSHA, err)
			d.Obs.failed(classRepo)
			if err := d.Journal.PutFailed(ctx, sha, reason); err != nil {
				d.Obs.ledgerError()
				d.Obs.failed(classLedger)
			}
			return last, applied, noop, passFailure{Reason: reason, SHA: sha, Head: headSHA}, false
		}

		summaries := map[string]ledger.RootSummary{}
		for _, root := range roots {
			d.logf("applying %s at %s (head %s)", root, sha, headSHA)
			summary, lockBusy, reason := applyOneRoot(ctx, d, cc, baseEnv, headSHA, root)
			if lockBusy {
				d.logf("state lock held elsewhere; ending this pass without recording a failure")
				return last, applied, noop, passFailure{}, true
			}
			if reason != "" {
				d.Obs.rootFailed(root)
				if err := d.Journal.PutFailed(ctx, sha, reason); err != nil {
					d.Obs.ledgerError()
					d.Obs.failed(classLedger)
					reason = reason + fmt.Sprintf(" (and could not record the failure: %v)", err)
				}
				return last, applied, noop, passFailure{Reason: reason, SHA: sha, Head: headSHA}, false
			}
			if summary.ResourceChanges != nil {
				d.Obs.rootChanged(root, *summary.ResourceChanges)
			}
			summaries[root] = summary
		}

		// ⚠️ PLAYS RUN AFTER EVERY ROOT AND BEFORE EVERY RENDER. The order
		// between kinds is fixed and compiled in -- credentials, tofu,
		// ansible, render -- and it is the natural one rather than an
		// invented one: infrastructure makes the machine, configuration
		// configures it, delivery ships onto it. Running a play before its
		// root would configure a host whose cloud resources, DNS record or
		// tailnet auth key this same commit has not created yet.
		if len(plays) > 0 {
			if reason := runAnsibleUnits(ctx, d, d.NewAnsible(ansibleEnv(d)), headSHA, plays); reason != "" {
				d.Obs.failed(classApply)
				if err := d.Journal.PutFailed(ctx, sha, reason); err != nil {
					d.Obs.ledgerError()
					d.Obs.failed(classLedger)
					reason = reason + fmt.Sprintf(" (and could not record the failure: %v)", err)
				}
				return last, applied, noop, passFailure{Reason: reason, SHA: sha, Head: headSHA}, false
			}
		}

		// ⚠️ RENDERS RUN AFTER EVERY ROOT AND BEFORE THE COMMIT IS RECORDED.
		// The order between kinds is fixed -- credentials, then tofu, then
		// render -- and it is the natural one: infrastructure makes the
		// cluster, delivery ships onto it. Rendering first would verify
		// manifests against a namespace or a secret that the same commit's
		// OpenTofu has not created yet.
		//
		// Nothing is applied here. The applier renders, compares against
		// what CI filed, and refuses on a mismatch; a reconciler is what
		// actually applies the manifests, from a ref this pass advances only
		// once every unit has passed.
		for _, unit := range renderUnits {
			d.logf("rendering %s at %s (head %s)", unit, sha, headSHA)
			_, reason := renderOneUnit(ctx, d, d.NewRender(renderEnv(d)), headSHA, unit)
			if reason != "" {
				if err := d.Journal.PutFailed(ctx, sha, reason); err != nil {
					reason = reason + fmt.Sprintf(" (and could not record the failure: %v)", err)
				}
				return last, applied, noop, passFailure{Reason: reason, SHA: sha, Head: headSHA}, false
			}
		}

		if err := d.Journal.PutApplied(ctx, sha, summaries); err != nil {
			d.Obs.ledgerError()
			d.Obs.failed(classLedger)
			return last, applied, noop, passFailure{Reason: fmt.Sprintf("could not record %s as applied: %v", sha, err), SHA: sha, Head: headSHA}, false
		}
		if err := d.Journal.AdvanceHead(ctx, sha); err != nil {
			d.Obs.ledgerError()
			d.Obs.failed(classLedger)
			return last, applied, noop, passFailure{Reason: fmt.Sprintf("could not advance head to %s: %v", sha, err), SHA: sha, Head: headSHA}, false
		}
		last = sha
		applied++
	}

	return last, applied, noop, passFailure{}, false
}

// credCache lazily reads the "applying credentials" -- Google's key, the
// state-encryption passphrase and the Cloudflare mint token -- at most
// once per pass (§2 item 12: read only when the pass has work, which by
// the time credCache exists it does). cf-infra-admin is deliberately NOT
// cached here: apply_root re-reads it fresh before every non-credentials
// root, because credentials/ may have re-minted it earlier in this same
// pass (apply.sh's own comment on this, preserved in loadCFInfraAdminToken's
// caller).
type credCache struct {
	dir        secrets.Dir
	google     *string
	passphrase *string
	mint       *string
}

func (c *credCache) googleCreds() (string, error) {
	if c.google == nil {
		v, err := loadGCPCredentials(c.dir)
		if err != nil {
			return "", err
		}
		c.google = &v
	}
	return *c.google, nil
}

func (c *credCache) passphraseVal() (string, error) {
	if c.passphrase == nil {
		v, err := loadTofuPassphrase(c.dir)
		if err != nil {
			return "", err
		}
		c.passphrase = &v
	}
	return *c.passphrase, nil
}

func (c *credCache) mintToken() (string, error) {
	if c.mint == nil {
		v, err := loadCFMintToken(c.dir)
		if err != nil {
			return "", err
		}
		c.mint = &v
	}
	return *c.mint, nil
}

// buildBaseEnv is the part of every tofu invocation's environment that
// does not depend on the root: PATH and HOME, copied explicitly from the
// process environment because Runner.Env is never inherited implicitly
// (§2 item 9) -- so cmd/truss itself decides what crosses that boundary,
// rather than plan.Runner defaulting to os.Environ() by accident.
// buildBaseEnv is the environment EVERY tofu run gets, matching what
// apply.sh:147 exports: PATH and HOME, plus the GitHub App identity the
// `github` provider authenticates with.
//
// ⚠️ THE FOUR GITHUB VARIABLES WERE MISSING AND EVERY ROOT USING THE GITHUB
// PROVIDER FAILED. The provider's `app_auth {}` block takes its arguments
// from the environment, so without them tofu refuses at init with
// "Missing required argument ... pem_file / id / installation_id" -- which
// reads like a bug in the platform's own versions.tf rather than a missing
// export. Found by the second shadow run, 2026-09-08: drift reported UNKNOWN
// for platform and projects/recipes, and the reason was this.
//
// ⚠️ GITHUB_APP_PEM_FILE IS THE KEY'S CONTENTS, NOT A PATH, despite the name
// -- that is the provider's own convention, and apply.sh carries the same
// warning. Passing a path here would fail in a way that looks like an
// unreadable file.
//
// ⚠️ These reach tofu through the ENVIRONMENT rather than argv, which is the
// same trade apply.sh documents at its own credential block: this process
// runs one pass and exits, there is no second tenant to leak to, and every
// one of these tools reads its credentials from the environment by
// convention.
func buildBaseEnv(d applyDeps, token string) ([]string, error) {
	env := []string{"PATH=" + d.PATH, "HOME=" + d.HOME}
	if token != "" {
		env = append(env, "GH_TOKEN="+token)
	}

	// ⚠️ THE 1PASSWORD SERVICE ACCOUNT TOKEN, AND IT WAS MISSING. The
	// credentials root declares a `onepassword` provider, which reads its
	// credentials from the environment; without this tofu fails at plan with
	// "Invalid provider configuration. Either Connect credentials … or
	// Service Account … should be set." apply.sh:99-100 exports it for every
	// root. Found by the first non-drift trial against production,
	// 2026-09-08.
	//
	// ⚠️ AND THIS IS WHY $OP_TOKEN_FILE IS A REQUIRED VARIABLE. I had
	// recorded it as "required by config.Load and read by nothing", which
	// was wrong: it is read to feed exactly this, and truss demanded the
	// variable while never using it -- the worst of both.
	if d.Cfg.OPTokenFile != "" {
		b, err := os.ReadFile(d.Cfg.OPTokenFile)
		if err != nil {
			return nil, fmt.Errorf("refusing to continue: reading %s: %w", d.Cfg.OPTokenFile, err)
		}
		opToken := strings.TrimRight(string(b), "\n")
		if opToken == "" {
			return nil, fmt.Errorf("refusing to continue: %s is empty -- the credentials root's onepassword provider cannot authenticate", d.Cfg.OPTokenFile)
		}
		env = append(env, "OP_SERVICE_ACCOUNT_TOKEN="+opToken)
	}
	appID, err := d.Dir.Field(itemGitHubApp, fieldGitHubAppID)
	if err != nil {
		return nil, err
	}
	installationID, err := d.Dir.Field(itemGitHubApp, fieldGitHubInstallationID)
	if err != nil {
		return nil, err
	}
	pem, err := d.Dir.Field(itemGitHubApp, fieldGitHubPrivateKey)
	if err != nil {
		return nil, err
	}
	env = append(env,
		"GITHUB_APP_ID="+appID,
		"GITHUB_APP_INSTALLATION_ID="+installationID,
		"GITHUB_APP_PEM_FILE="+pem,
	)

	// ⚠️ OPTIONAL, AND ABSENT MUST NOT BE FATAL. Only a consumer whose roots
	// create repositories mounts this; one that adopts existing repositories
	// with import blocks never needs it, and making it required would stop
	// every such deployment on an upgrade for a credential it has no use for.
	//
	// ⚠️ IT REACHES TOFU AS TF_VAR_, NOT AS GITHUB_TOKEN. The github provider
	// reads GITHUB_TOKEN from the environment, so exporting it would silently
	// re-authenticate EVERY github provider in the root -- including the
	// default one that authenticates as the App, whose whole point is that it
	// is not a person. A variable is passed to one aliased provider
	// explicitly, so the PAT's reach is what the config says it is rather
	// than whatever happens to read the environment first.
	token, ok, err := d.Dir.FieldIfPresent(itemGitHubRepoAdmin, fieldGitHubRepoToken)
	if err != nil {
		return nil, err
	}
	if ok {
		env = append(env, "TF_VAR_github_repo_admin_token="+token)
	}

	// ⚠️ THE TAILSCALE PROVIDER READS THESE TWO NAMES FROM THE ENVIRONMENT,
	// which is why they are exported rather than passed as TF_VAR_. That is
	// the opposite of the choice made for the repo-admin PAT above, and the
	// difference is real: there is exactly one tailscale provider in a root,
	// so an environment variable cannot silently re-authenticate a second
	// one. The github provider has two -- the App and the PAT -- and
	// GITHUB_TOKEN would capture both.
	//
	// Optional for the same reason as the token above: a consumer with no
	// tailnet mounts neither, and requiring them would stop it on upgrade.
	tsKey, ok, err := d.Dir.FieldIfPresent(itemTailscale, fieldTailscaleKey)
	if err != nil {
		return nil, err
	}
	if ok {
		env = append(env, "TAILSCALE_API_KEY="+tsKey)
		// ⚠️ STATED, NOT INFERRED. With no tailnet the provider falls back to
		// "the tailnet that owns the credential" -- correct today, and
		// silently a different answer the first time a credential from
		// another tailnet is used.
		if net, ok, err := d.Dir.FieldIfPresent(itemTailscale, fieldTailscaleNet); err != nil {
			return nil, err
		} else if ok {
			env = append(env, "TAILSCALE_TAILNET="+net)
		}
	}

	// The Hetzner project token, for a consumer whose machines live there.
	// Optional and read the same way, so a deployment with no Hetzner
	// project mounts nothing and plans exactly as before.
	//
	// ⚠️ ONE TOKEN, READ-WRITE, AND THERE IS NO NARROWER SCOPE TO ASK FOR.
	// Hetzner Cloud tokens are per-project and are either read or
	// read-write; there is no per-resource permission and no way to grant
	// "may resize, may not delete". So the thing standing between a bad
	// plan and a deleted machine is not the credential -- it is the plan
	// digest a human approved, plus `prevent_destroy` on the server
	// resources themselves, which makes tofu refuse the destroy at plan
	// time rather than at apply time.
	if token, ok, err := d.Dir.FieldIfPresent(itemHetzner, fieldHetznerToken); err != nil {
		return nil, err
	} else if ok {
		env = append(env, "HCLOUD_TOKEN="+token)
	}
	return env, nil
}

// applyOneRoot runs init, plan, (for non-credentials roots) the digest
// gate, then apply for one root at one commit's head sha. It returns
// lockBusy=true, with no reason, exactly when tofu reported the state
// lock held elsewhere (§2 item 7) -- the caller must not file a failure
// for that case.
func applyOneRoot(ctx context.Context, d applyDeps, cc *credCache, baseEnv []string, headSHA, root string) (summary ledger.RootSummary, lockBusy bool, reason string) {
	if !d.Git.HasDir(root) {
		d.Obs.failed(classConfig)
		return ledger.RootSummary{}, false, fmt.Sprintf("root %s does not exist at %s", root, headSHA)
	}
	rootDir := filepath.Join(d.Cfg.Workdir, root)

	// timed runs one step of this root's apply and records how long it took,
	// whether it succeeded or not. A failed step's duration is the
	// interesting one: it is how a `tofu apply` that died after nine minutes
	// is told apart from one that was refused instantly.
	timed := func(phase string, f func() error) error {
		start := d.now()
		err := f()
		d.Obs.rootTook(root, phase, d.now().Sub(start).Seconds())
		return err
	}

	env := append([]string{}, baseEnv...)
	google, err := cc.googleCreds()
	if err != nil {
		d.Obs.failed(classCredentials)
		return ledger.RootSummary{}, false, err.Error()
	}
	env = append(env, "GOOGLE_CREDENTIALS="+google)

	if root == "credentials" {
		mint, err := cc.mintToken()
		if err != nil {
			d.Obs.failed(classCredentials)
			return ledger.RootSummary{}, false, err.Error()
		}
		pass, err := cc.passphraseVal()
		if err != nil {
			d.Obs.failed(classCredentials)
			return ledger.RootSummary{}, false, err.Error()
		}
		env = append(env, "CLOUDFLARE_API_TOKEN="+mint, "TF_VAR_encryption_passphrase="+pass)
	} else {
		declares, err := rootDeclaresCloudflare(d.Cfg.Workdir, root)
		if err != nil {
			d.Obs.failed(classConfig)
			return ledger.RootSummary{}, false, err.Error()
		}
		if declares {
			infra, ok, err := loadCFInfraAdminToken(cc.dir)
			if err != nil {
				d.Obs.failed(classCredentials)
				return ledger.RootSummary{}, false, err.Error()
			}
			if !ok {
				d.Obs.failed(classCredentials)
				return ledger.RootSummary{}, false, fmt.Sprintf(
					"cf-infra-admin is not mounted: it is minted by credentials/, so that root must be applied before %s can be", root)
			}
			env = append(env, "CLOUDFLARE_API_TOKEN="+infra)
		}
	}

	runner := d.NewTofu(env)
	const planFile = "tfplan"

	if err := timed("init", func() error { return runner.Init(ctx, rootDir) }); err != nil {
		if errors.Is(err, plan.ErrLockBusy) {
			if reason := d.staleLockReason(root, err); reason != "" {
				return ledger.RootSummary{}, false, reason
			}
			return ledger.RootSummary{}, true, ""
		}
		d.Obs.failed(classPlan)
		return ledger.RootSummary{}, false, fmt.Sprintf("tofu init failed for %s: %v", root, err)
	}
	if err := timed("plan", func() error { return runner.Plan(ctx, rootDir, planFile) }); err != nil {
		if errors.Is(err, plan.ErrLockBusy) {
			if reason := d.staleLockReason(root, err); reason != "" {
				return ledger.RootSummary{}, false, reason
			}
			return ledger.RootSummary{}, true, ""
		}
		d.Obs.failed(classPlan)
		return ledger.RootSummary{}, false, fmt.Sprintf("tofu plan failed for %s: %v", root, err)
	}

	// ⚠️ THE PLAN JSON IS FETCHED ONCE, HERE, FOR EVERY ROOT -- CREDENTIALS
	// INCLUDED -- AND SHARED BY BOTH GATES BELOW. It used to be fetched only
	// for a non-credentials root, inside the digest-gate branch, because the
	// digest gate is what credentials is exempt from (§2 item 10: CI never
	// plans that root, so there is nothing for a human to have approved).
	// The declarations gate below is a different question -- does this plan
	// run a provisioner or a forbidden resource type -- and credentials can
	// run tofu exactly like any other root, so it is not exempt from that.
	// Fetching once and sharing it is also just not running tofu twice.
	var planJSON []byte
	err = timed("show", func() error {
		var showErr error
		planJSON, showErr = runner.ShowJSON(ctx, rootDir, planFile)
		return showErr
	})
	if err != nil {
		if errors.Is(err, plan.ErrLockBusy) {
			if reason := d.staleLockReason(root, err); reason != "" {
				return ledger.RootSummary{}, false, reason
			}
			return ledger.RootSummary{}, true, ""
		}
		d.Obs.failed(classPlan)
		return ledger.RootSummary{}, false, fmt.Sprintf("could not digest our own plan for %s: %v", root, err)
	}

	// The declarations gate: refuse a plan that would run a provisioner or a
	// forbidden resource type, whether or not this pass has any resource
	// change to apply.
	//
	// ⚠️ RUN UNCONDITIONALLY, NOT INSIDE THE "changes == 0" SKIP BELOW. The
	// digest gate skips an empty plan because there is nothing to gate --
	// what would change was never shown to anyone, so there is nothing to
	// have deviated from what was approved. A provisioner is a different
	// kind of fact: it is a configuration fact that will run the next time
	// ANYTHING touches the resource carrying it, whether or not THIS plan
	// changes that resource. A plan with zero resource changes still
	// declares it, ready to fire later, so this gate is not narrowed by the
	// same "nothing to gate" argument the digest gate makes for itself.
	//
	// ⚠️ CREDENTIALS IS NOT EXEMPT HERE. §2 item 10 exempts credentials from
	// the DIGEST gate specifically, because CI cannot plan that root -- a
	// fact about who could have reviewed it, not about whether it may run
	// arbitrary commands at apply time. Copying the exemption across would
	// make credentials/ the one root where a provisioner could run
	// unreviewed forever.
	decls, err := plan.Declarations(planJSON)
	if err != nil {
		d.Obs.failed(classPlan)
		return ledger.RootSummary{}, false, fmt.Sprintf(
			"could not read our own plan's declarations for %s: %v", root, err)
	}
	if problems := gates.CheckDeclarations(root, decls); len(problems) > 0 {
		d.Obs.failed(classPlan)
		return ledger.RootSummary{}, false, strings.Join(problems, "; ")
	}

	// §2 item 10: only credentials is exempt from the digest gate, because
	// CI never plans it.
	if root != "credentials" {
		// ⚠️ A PLAN THAT CHANGES NOTHING IS NOT GATED, BECAUSE THERE IS
		// NOTHING TO GATE. The digest proves that what we are about to
		// change is what the approver read. A plan with no changes in it
		// changes nothing, so it cannot deviate from what was approved --
		// the same argument Canonical already makes for dropping
		// individual no-op resources, applied to a plan that is entirely
		// no-ops.
		//
		// ⚠️ WITHOUT THIS, A COMMIT TOUCHING TWO ROOTS COULD WEDGE THE
		// QUEUE FOREVER. Roots are applied one at a time and applied/<sha>
		// is written only after all of them succeed, so a failure in the
		// second root leaves the first one APPLIED with HEAD unmoved. The
		// next pass re-plans that first root against infrastructure that
		// now already carries its changes, gets an empty plan, and hashes
		// it to the digest of `[]` -- which cannot match the digest CI
		// filed for a plan that changed something. Every later pass
		// repeated it identically, and the refusal blamed "the world moved
		// between review and apply" when what had moved it was the
		// previous pass. Reproduced in apply_partial_multiroot_test.go.
		//
		// It also settles the case this gate got wrong in the other
		// direction: a change somebody had already made by hand, exactly
		// as approved, left an empty plan that was refused forever rather
		// than recorded as already satisfied.
		//
		// ⚠️ THE ERROR IS LOAD-BEARING AND MUST NOT BE DROPPED.
		// countResourceChanges refuses a document it cannot read as a plan,
		// and treating that as "no changes" would skip the gate on exactly
		// the input nobody understands -- the absent-reads-as-compliant bug
		// that internal/gates exists to keep out of this codebase. A plan
		// that legitimately changes nothing is a different answer, and the
		// two are told apart on a field: see countResourceChanges.
		changes, err := countResourceChanges(planJSON)
		if err != nil {
			// ⚠️ ITS OWN REFUSAL, CARRYING ITS OWN CAUSE. Falling through to
			// the digest comparison would refuse too -- correct -- but under
			// "does not match the one approved at", which blames the world
			// for moving when what actually happened is that this plan could
			// not be read. The reason travels with it, because "not a plan
			// document" and "resource_changes is not a list" are different
			// repairs.
			return ledger.RootSummary{}, false, fmt.Sprintf(
				"could not read our own plan for %s: %v", root, err)
		}
		if changes == 0 {
			d.logf("plan for %s changes nothing; no digest to check, because there is nothing to apply", root)
		} else {
			mine, err := plan.Digest(planJSON)
			if err != nil {
				d.Obs.failed(classPlan)
				return ledger.RootSummary{}, false, fmt.Sprintf("could not digest our own plan for %s: %v", root, err)
			}
			approved, err := d.Journal.ApprovedDigest(ctx, headSHA, root)
			approvedFound := true
			if err != nil {
				if errors.Is(err, ledger.ErrNotFound) {
					approvedFound = false
				} else {
					d.Obs.ledgerError()
					d.Obs.failed(classLedger)
					return ledger.RootSummary{}, false, fmt.Sprintf("could not read the approved plan digest for %s at %s: %v", root, headSHA, err)
				}
			}
			if problems := gates.CheckPlanDigest(root, headSHA, d.Journal.Layout.DigestKey(headSHA, root), mine, approved, approvedFound); len(problems) > 0 {
				// ⚠️ THE SERIES THIS PROJECT EXISTS TO PRODUCE. A refusal
				// here means the plan truss was about to run did not hash to
				// the plan a human read. It is recorded before the return so
				// that a refusal is counted even though the pass is over.
				d.Obs.digestChecked(true)
				d.Obs.failed(classDigest)
				return ledger.RootSummary{}, false, strings.Join(problems, "; ")
			}
			d.Obs.digestChecked(false)
			d.logf("plan for %s matches the one approved at %s", root, headSHA)
		}
	}

	if err := timed("apply", func() error { return runner.Apply(ctx, rootDir, planFile) }); err != nil {
		if errors.Is(err, plan.ErrLockBusy) {
			if reason := d.staleLockReason(root, err); reason != "" {
				return ledger.RootSummary{}, false, reason
			}
			return ledger.RootSummary{}, true, ""
		}
		d.Obs.failed(classApply)
		return ledger.RootSummary{}, false, fmt.Sprintf("tofu apply failed for %s: %v", root, err)
	}

	var n *int
	if pj, err := runner.ShowJSON(ctx, rootDir, planFile); err == nil {
		if count, err := countResourceChanges(pj); err == nil {
			n = &count
		}
	}
	return ledger.RootSummary{ResourceChanges: n}, false, ""
}

// rootDeclaresCloudflare reports whether a root's COMMITTED lockfile declares
// the Cloudflare provider.
//
// ⚠️ ROOT CREDENTIALS MUST NOT GO WHERE THEY CANNOT BE USED. Every root that
// was not `credentials` used to be handed cf-infra-admin, which carries
// account-wide R2 write, account-wide Access "Apps and Policies" AND
// "Organizations, Identity Providers, and Groups" write, and zone DNS write.
// `platform/` does not use Cloudflare at all -- its lockfile declares only the
// github provider and it contains no cloudflare_ resource -- and was still
// given that token on every apply. This mints nothing and needs nothing new:
// it just stops handing the account's broadest credential to a process that
// cannot use it.
//
// The LOCKFILE is the authority rather than a list we maintain, because
// `tofu init` runs with -lockfile=readonly: the committed lockfile IS the
// reviewed provider set, so a root cannot use a provider it does not name
// there. A list beside it would be a second copy, and the one that goes stale.
//
// ⚠️ A MISSING LOCKFILE IS AN ERROR, NOT A "no". Defaulting to no-token would
// silently strip the credential from a root that genuinely needs one, and the
// failure would surface later as a confusing provider error. A root with no
// lockfile cannot init under -lockfile=readonly anyway.
func rootDeclaresCloudflare(workdir, root string) (bool, error) {
	lock := filepath.Join(workdir, root, ".terraform.lock.hcl")
	b, err := os.ReadFile(lock)
	if err != nil {
		return false, fmt.Errorf("could not read %s to decide which credentials %s needs: %w", lock, root, err)
	}
	return bytes.Contains(b, []byte(`provider "registry.opentofu.org/cloudflare/cloudflare"`)), nil
}

// countResourceChanges is a DELIBERATE DIVERGENCE from summary_from_plan
// (apply.sh:598), which counts every entry in resource_changes, no-ops
// included -- so a plan that touches nothing still reports "N changes"
// every single night. See parity.Divergences["COUNT-EXCLUDES-NOOP"].
//
// OpenTofu/Terraform mark a resource the plan will not touch with
// `"change":{"actions":["no-op"]}` (verified against a real `tofu show
// -json`, not assumed: a genuine no-op, an in-place update and a
// forced replace were produced from a scratch root and inspected -- the
// replace comes back as the two-element `["delete","create"]`, one
// resource, not two). Only an entry whose actions are exactly that single
// element is excluded; everything else, including replace, counts as one
// changed resource.
//
// An entry with a missing or empty actions array is counted as a change
// rather than skipped: that shape is not one OpenTofu is known to emit, and
// silently treating an unanticipated shape as "no change" is exactly the
// kind of guess the "if we put a number somewhere, we must be sure it is
// right" rule forbids. Fail loud by counting it, not by swallowing it.
//
// Returns an error, never a zero count, for a document it cannot read as a
// plan: one that does not parse, one carrying no usable `errored` and no
// usable `resource_changes` -- usable meaning the field carries a value of
// the type OpenTofu writes, not merely that the name is present -- or one
// whose `resource_changes` is not a list. The caller refuses on it;
// RootSummary.ResourceChanges is left nil.
func countResourceChanges(planJSON []byte) (int, error) {
	// ⚠️ MEASURED FROM OPENTOFU'S OWN STRUCT TAGS, 2026-09-09
	// (internal/command/jsonplan, the plan representation this consumes):
	// `resource_changes` is omitempty and `errored` is NOT. So a plan that
	// changes nothing omits the key entirely -- refusing on its absence
	// would wedge, forever and every pass, a root whose last resource a
	// commit destroys -- while `errored` is written by every plan document
	// OpenTofu emits, empty ones included.
	//
	// That is what tells "an empty plan" apart from "not a plan". `{}` and
	// `null` unmarshal without error and carry neither key; reading those as
	// "nothing to change" let the one caller skip the digest gate on exactly
	// the input nobody understands, which is absent reading as compliant in
	// the one place this codebase exists to prevent it.
	//
	// resource_changes alone also counts as a plan, because every fixture in
	// this tree and in the recorded corpus is written that way.
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(planJSON, &doc); err != nil {
		return 0, fmt.Errorf("the plan JSON does not parse: %v", err)
	}
	raw, hasChanges := doc["resource_changes"]
	// ⚠️ A null VALUE IS NOT A PRESENT KEY, for the purpose of deciding
	// whether this is a plan at all. `{"resource_changes":null}` carries the
	// name of a plan field and nothing else, and accepting it as evidence
	// let a document with no `errored` either be read as an empty plan and
	// applied ungated -- the same hole one layer in.
	if string(raw) == "null" {
		hasChanges = false
	}
	// ⚠️ AND THE SAME RULE FOR errored: PRESENT IS NOT ENOUGH. OpenTofu
	// declares it a plain bool, so a real plan document carries exactly
	// `true` or `false`. `{"errored":null}` -- or a string, or an object --
	// carries the name and not the fact, and accepting it as evidence is the
	// hole this rule closes twice over.
	errored, hasErrored := doc["errored"]
	if hasErrored && string(errored) != "true" && string(errored) != "false" {
		hasErrored = false
	}
	if !hasErrored && !hasChanges {
		return 0, errors.New("it carries neither errored nor resource_changes, so it is not a plan document")
	}
	// ⚠️ AND errored:true IS NOT ZERO CHANGES. It is the one value of that
	// field meaning "this plan is not applyable", and with resource_changes
	// omitted alongside it the document otherwise reads as a valid plan that
	// changes nothing -- which skips the digest gate. Unreachable today,
	// because Runner.Plan returns early on a non-zero tofu exit and ShowJSON
	// is never called; refused anyway, because it costs nothing and no plan
	// worth applying carries it.
	if string(errored) == "true" {
		return 0, errors.New("the plan itself reports errored")
	}
	if !hasChanges {
		return 0, nil
	}

	var changes []struct {
		Change struct {
			Actions []string `json:"actions"`
			// ⚠️ AN IMPORT IS RENDERED AS A no-op AND STILL MUTATES STATE.
			// OpenTofu carries `importing` on the change (measured in the
			// same struct tags: *Importing, omitempty), and applying an
			// import block whose resource already matches configuration
			// writes that resource into state. It does not "change
			// nothing", so it must not take the skip.
			//
			// ⚠️ WHAT THIS BUYS IS THE GATE RUNNING, NOT THE IMPORT BEING
			// COMPARED. plan.theFilter drops every entry whose actions are
			// exactly ["no-op"] regardless of importing, so both CI and the
			// applier hash it away and a matching digest says nothing about
			// it. Running the gate still catches the case that matters most
			// -- no approved digest recorded at all -- and the count stops
			// being a lie. The filter cannot be widened: it is
			// byte-identical to the consumer's jq and to every digest
			// already in the ledger. Recorded in docs/work-items.md.
			Importing json.RawMessage `json:"importing"`
		} `json:"change"`
	}
	if err := json.Unmarshal(raw, &changes); err != nil {
		return 0, fmt.Errorf("its resource_changes does not read as a list of changes: %v", err)
	}
	count := 0
	for _, rc := range changes {
		importing := len(rc.Change.Importing) > 0 && !bytes.Equal(rc.Change.Importing, []byte("null"))
		if len(rc.Change.Actions) == 1 && rc.Change.Actions[0] == "no-op" && !importing {
			continue
		}
		count++
	}
	return count, nil
}

// runRotation re-plans and, if a generation boundary passed, re-applies
// the credentials root at last -- never at origin/main (§2 item 14). It
// returns a JSON-marshalable summary (mirroring rotation_summary's shapes:
// {"skipped": "..."} or a RootSummary-shaped success, or {"failed": "..."}),
// the number of resource changes rotation made, applied (true only on the
// success path -- never on a skip or a failure, and the caller's sole
// signal for whether a freshly minted value exists to hand the publisher),
// and an error only when rotation itself failed (never for a skip, which is
// not a failure).
func runRotation(ctx context.Context, d applyDeps, last string, cc *credCache) (summary any, changes int, applied bool, err error) {
	// ⚠️ THE "ALREADY APPLIED THIS RUN" SKIP IS GONE, BECAUSE IT BECAME
	// UNREACHABLE. It existed so a pass that had just applied credentials/ in
	// the commit loop would not immediately re-plan it here. Rotation now
	// runs only on the daily pass, and the daily pass skips the commit loop
	// entirely -- so the two can no longer happen in one run and the branch
	// could never be taken. A guard that cannot fire reads like protection
	// and is not. See applier/apply.sh:870-875 for the same removal on the
	// bash side.

	// ⚠️ CHECK OUT FIRST, THEN TEST FOR THE DIRECTORY. HasDir reads the
	// working tree, so asking before the checkout asked about whatever tree
	// the commit loop happened to leave behind -- not about `last`.
	// apply.sh:673-675 checks out $LAST first and then tests, and both
	// directions of getting this wrong bite: a false skip silently stalls a
	// 45-day rotation window, and the inverse produces a hard failure in
	// applyOneRoot where the bash skipped cleanly. runDrift already had the
	// order right, so the inconsistency was within one file. Found by the
	// 2026-09-08 code audit.
	if err := d.Git.Checkout(ctx, last); err != nil {
		return map[string]string{"failed": err.Error()}, 0, false, fmt.Errorf("could not check out %s for rotation: %w", last, err)
	}
	if !d.Git.HasDir("credentials") {
		d.logf("rotation: no credentials root at %s, nothing to rotate", last)
		return map[string]string{"skipped": "no credentials root at last applied commit"}, 0, false, nil
	}

	baseEnv, err := buildBaseEnv(d, d.Token)
	if err != nil {
		return map[string]string{"failed": err.Error()}, 0, false, err
	}
	d.logf("rotation: re-planning credentials at %s", last)
	result, lockBusy, reason := applyOneRoot(ctx, d, cc, baseEnv, last, "credentials")
	if result.ResourceChanges != nil {
		d.Obs.rootChanged("credentials", *result.ResourceChanges)
	}
	if lockBusy {
		d.logf("rotation: state lock held elsewhere; next pass will re-plan")
		return map[string]string{"skipped": "state lock held elsewhere"}, 0, false, nil
	}
	if reason != "" {
		return map[string]string{"failed": reason}, 0, false, errors.New(reason)
	}
	if result.ResourceChanges != nil {
		changes = *result.ResourceChanges
	}
	return result, changes, true, nil
}

// runDrift plans every root at last WITHOUT applying (§2 item 15), under
// cf-infra-admin -- the same credential the roots apply under, so a plan
// failure here is drift or a real error, never a rejected credential. It
// returns the drifted and errored root lists, and -- when the check could
// not run at all -- a skipped reason, matching check_drift's own three
// outcomes (no roots, no cf-infra-admin yet, or a real sweep).
func runDrift(ctx context.Context, d applyDeps, last string) (drifted, errored []string, skipped string) {
	if err := d.Git.Checkout(ctx, last); err != nil {
		return nil, nil, fmt.Sprintf("could not check out %s: %v", last, err)
	}

	var roots []string
	if d.Git.HasDir("platform") {
		roots = append(roots, "platform")
	}
	treeRoots, err := d.Git.TreeRoots(ctx, last)
	if err != nil {
		return nil, nil, fmt.Sprintf("could not read the tree at %s: %v", last, err)
	}
	for _, r := range treeRoots {
		if strings.HasPrefix(r, "projects/") {
			roots = append(roots, r)
		}
	}
	if len(roots) == 0 {
		return nil, nil, "no roots at last applied commit"
	}

	google, err := loadGCPCredentials(d.Dir)
	if err != nil {
		return nil, nil, fmt.Sprintf("could not read gcp-apply credentials: %v", err)
	}
	base, err := buildBaseEnv(d, d.Token)
	if err != nil {
		return nil, nil, err.Error()
	}
	baseEnv := append(base, "GOOGLE_CREDENTIALS="+google)

	// ⚠️ WHICH ROOTS NEED CLOUDFLARE IS DECIDED BEFORE THE LOOP; WHETHER THE
	// TOKEN IS MISSING STILL SKIPS THE WHOLE PASS. Two things had to hold at
	// once here and the obvious shape breaks one of them.
	//
	// The credential must be chosen PER ROOT, because runDrift used to read it
	// once for every root -- so gating applyOneRoot alone leaves the drift pass
	// still exporting the account's broadest Cloudflare token to roots that
	// cannot use it, which is a fix that looks complete and covers one of the
	// two places a root gets planned.
	//
	// But apply.sh:1044 treats an unmounted cf-infra-admin as
	// `drift_summary={"skipped":"cf-infra-admin is not mounted yet"}` -- the
	// whole check skipped, with a reason -- NOT as every root erroring. A first
	// version of this moved the read inside the loop and marked each root
	// errored, which reads on the heartbeat as "drift UNKNOWN for:
	// projects/recipes" where the bash says "DRIFT NOT CHECKED". Those are
	// different claims: one says we looked and could not tell, the other says
	// we never looked. internal/parity caught it.
	declaresCF := make(map[string]bool, len(roots))
	needCF := false
	for _, root := range roots {
		declares, err := rootDeclaresCloudflare(d.Cfg.Workdir, root)
		if err != nil {
			return nil, nil, fmt.Sprintf("could not decide which credentials %s needs: %v", root, err)
		}
		declaresCF[root] = declares
		if declares {
			needCF = true
		}
	}

	var infra string
	if needCF {
		tok, ok, err := loadCFInfraAdminToken(d.Dir)
		if err != nil {
			return nil, nil, fmt.Sprintf("could not read cf-infra-admin: %v", err)
		}
		if !ok {
			return nil, nil, "cf-infra-admin is not mounted yet"
		}
		infra = tok
	}

	for _, root := range roots {
		rootDir := filepath.Join(d.Cfg.Workdir, root)
		d.logf("drift: planning %s at %s", root, last)

		env := baseEnv
		if declaresCF[root] {
			env = append(append([]string{}, baseEnv...), "CLOUDFLARE_API_TOKEN="+infra)
		}

		runner := d.NewTofu(env)
		start := d.now()
		if err := runner.Init(ctx, rootDir); err != nil {
			d.Obs.rootTook(root, "drift", d.now().Sub(start).Seconds())
			d.warnf("drift: could not init %s: %v", root, err)
			errored = append(errored, root)
			continue
		}
		changed, err := runner.PlanDetailed(ctx, rootDir)
		d.Obs.rootTook(root, "drift", d.now().Sub(start).Seconds())
		if err != nil {
			// ⚠️ A WARNING, AND THE DISTINCTION IT PRESERVES IS THE POINT.
			// This root goes into `errored`, never into `drifted`: we looked
			// and could not tell, which is not the same claim as "this
			// drifted". The heartbeat already keeps them apart; without a
			// level on this line the pod log did not.
			d.warnf("drift: could not plan %s: %v", root, err)
			errored = append(errored, root)
			continue
		}
		if changed {
			drifted = append(drifted, root)
		}
	}
	return drifted, errored, ""
}

// runHandoff sends req to the publisher exactly once and narrates the
// exchange via d.logf. It NEVER returns before calling d.Handoff -- there
// is no early return above the call -- because a truss that finishes
// without contacting the publisher leaves the publisher container blocked
// until its own deadline, and concurrencyPolicy: Forbid then silently
// suppresses every later pass until somebody notices
// (design publisher-identity-design.md §3, §9's "operational hazard", and
// worse than any single failed publish). The caller (runApplyPass) already
// guarantees this function itself is reached on every return path of a
// non-drift pass; this function's own job is not adding a return path that
// skips the send.
//
// The mapping to a failure is design §9's table, and it turns entirely on
// req.PublishValue -- what TRUSS ITSELF decided to send -- never on the
// shape of the response. A dial failure and a Response.Error are folded
// the same way deliberately: from here, both mean "the publisher did not
// confirm the write". When nothing was minted this pass (req.PublishValue
// is false -- every gate failure, every skipped or failed rotation; see
// the caller for exactly which), ANY answer, including the publisher's own
// refusal of a vacuous pass or nobody listening at all, is narrated and
// never fails this pass: the same reasoning already governs
// the expiry sweep elsewhere in this file -- an alert channel nobody reads
// is where a real gate refusal goes to die, and the underlying problem
// (an unpatched expiry table, say) is still shouted about on its own
// channel. Only a failed publish OF A MINTED VALUE returns a non-empty
// failure, and its sentence says the apply itself succeeded -- the token
// was minted, Cloudflare has it, 1Password and the state hold it -- which
// is the same lesson PublishInProgress already taught: an error that
// names a step it did not check is claiming to know something it does
// not.
//
// Nothing here writes the ledger, the heartbeat or the Telegram alert
// directly -- the caller folds a returned failure into the pass's existing
// failure/PutFailed machinery, the same path a rotation failure already
// takes. Reporting through narration (stderr) rather than those channels
// for the non-failing cases is deliberate: internal/parity compares ledger
// writes and alert text against a bash reference that has no concept of a
// publisher at all, and stdio is the one channel that comparison never
// touches (see logf's own doc).
func runHandoff(ctx context.Context, d applyDeps, req handoff.Request, last string) (failure string) {
	d.logf("publish: contacting the publisher")
	resp, err := d.Handoff(ctx, d.HandoffSocket, handoffTimeout, req)
	if err != nil {
		// ⚠️ A WARNING EVEN WHEN IT IS NOT A FAILURE. A pass with nothing
		// to publish returns "" below and the pass succeeds -- but the
		// publisher sidecar not answering is still the thing that would
		// have swallowed a real rotation had one happened, and it left no
		// trace anywhere before this line had a level on it.
		d.warnf("publish: no publisher answered: %v", err)
		if req.PublishValue {
			return fmt.Sprintf(
				"credentials applied at %s; publishing to vault failed: %v. Vault still holds the previous value; the next daily pass republishes.",
				last, err)
		}
		return ""
	}
	if resp.Error != "" {
		d.Obs.publishResult(false, resp.Expiries)
		d.warnf("publish: %s (expiries=%d skipped=%v): %s", resp.Value, resp.Expiries, resp.Skipped, resp.Error)
		if req.PublishValue {
			return fmt.Sprintf(
				"credentials applied at %s; publishing to vault failed: %v. Vault still holds the previous value; the next daily pass republishes.",
				last, resp.Error)
		}
		return ""
	}
	d.Obs.publishResult(true, resp.Expiries)
	d.logf("publish: %s (expiries=%d)", resp.Value, resp.Expiries)
	return ""
}
