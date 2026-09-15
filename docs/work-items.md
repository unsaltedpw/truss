# Work items

Wanted, scoped, deliberately not built yet. Each entry says what it is, why it
is deferred, and what has already been paid for so the next pass does not
re-derive it.

## Three repos, and today only two of them are real

**Decided 2026-09-08 by the user, in these words: "Infra goes in platform.
Truss goes in truss. Recipes go in recipes. VM configuration goes in platform
(this is new and can wait, for now at least if needed)."**

The state that prompted it, measured the same day:

- `beeradb/platform` **already exists and is the real infra repo** — nineteen
  merged PRs, branch protection, a CI plan workflow. Last commit
  2026-09-08 01:03.
- `beeradb/recipes` **still carries a full copy of the same tree** —
  `applier/`, `bootstrap/`, `cluster/`, `credentials/`, `modules/`,
  `platform/`, `projects/`, `vault/`, `tests/`, `providers.allow`. Not an old
  snapshot: a **fork that has since diverged**.
- Both repos carry `engine/`, a stale duplicate of this one. It declares
  `module github.com/beeradb/truss`, holds a single file that is an older
  copy of `internal/gates/gates.go`, and is described in the platform README
  as "in progress and deployed nowhere" — false twice, since truss is
  deployed and it is not from there.

⚠️ **THE DIVERGENCE IS ON THE PRODUCTION SIDE, WHICH IS THE PART THAT
MATTERS.** Everything applied to production after 2026-09-08 01:03 exists only
in the `recipes` copy, and at the time this was written none of it was pushed
anywhere:

| Stranded in `recipes`, absent from `platform` | Status in production |
| --- | --- |
| `applier/manifests/22-truss-cronjob.yaml` | **running** |
| `applier/manifests/23-truss-drift-cronjob.yaml` | **running** |
| `applier/manifests/expiries.json` | **read every daily pass** |
| `vault/grant-publisher.sh` | **already executed against the live Vault** |
| `credentials/vault-snapshot.tf`, `outputs.tf` | applied |
| `vault/41-tailnet-ingress.yaml`, `vault/tailscale/`, `cluster/tailscale/` | written, not applied |

plus divergent copies of `vault/10-vault.yaml`, `20-snapshot-cronjob.yaml`,
`30-networkpolicy.yaml`, `bootstrap-vault.sh`, both `kustomization.yaml`s,
`cluster/00-external-secrets-helm.yaml` and `credentials/variables.tf`.

**So the config for what is running was, briefly, in exactly one place: a
`/tmp` checkout on one VM.** This is the `nohup` crawl-loop failure in another
costume — working fine, and nothing anywhere saying it was one `rm -rf` from
being unreconstructable.

### What has to happen, in this order

1. **Push everything**, before any restructuring. Nothing may be only local.
2. **Port the stranded infra into `platform` as a PR.** ⚠️ Not a direct push:
   that repo has branch protection and a CI plan workflow, and the applier
   gates on plan digests from it. A deadlock caused by an unreportable status
   context has already happened once here.
3. **Delete the infra tree from `recipes`.** Two copies of a live config is
   the defect; deleting the wrong one is how it becomes an outage, so this
   step comes after 2 is merged and verified, never alongside it.
4. **Delete `engine/` from both.** It is this repository, three commits stale.
5. **Move VM configuration into `platform`.** Explicitly "can wait".

⚠️ **Do not treat this as a tidy-up.** The applier reads a repo, CI plans it,
and branch protection gates it — moving roots between repositories re-points
all three, and every one of them fails closed in a way that stops applies.

## Automating the Tailscale credentials

See `docs/decisions/after-launch.md` in the platform repo for the long form.
Short version: `tailscale_oauth_client` is a real provider resource, so the
per-cluster OAuth clients the Kubernetes operator needs can be minted by
`credentials/` with two generations and a real expiry, instead of one console
visit per cluster.

⚠️ The bootstrap client needs the broad `all` scope — the published scope list
has none for managing trust credentials. That is the same concentration
`credentials/` already runs on for `cf-token-mint`.

⚠️ **Minting is not delivery.** A rotated client still has to reach the
cluster's `operator-oauth` Secret. On hetzner-1 that is an ESO
`ExternalSecret`; the Vault cluster has no ESO, so there the Secret is
hand-updated and a rotation will break the operator when the old generation is
destroyed. Settle that before turning rotation on for these.

## A local `truss` you can point at a stuck applier

**Asked for 2026-09-08, in these words: "a binary on my computer I can use to
inspect the queue and unstick things."**

The applier was stuck for most of a day, and every step of understanding and
repairing it was hand-written:

- reading the ledger watermark meant a boto3 script and a 1Password lookup
- learning WHY a commit was refused meant kubectl logs and pattern-matching an
  error message
- advancing the watermark past a commit that could never apply meant a second
  hand-written script, pasted into a terminal, twice
- and both scripts failed the first time on an S3 checksum incompatibility
  that `applier/apply.sh` already documents and neither script knew about

None of that is exotic. It is `truss ledger get`, a `status`, and a guarded
`skip` -- over machinery truss already has: the ledger client, the S3 signing,
the plan digest, the config loading. What is missing is a front door.

Sketch, in the order today wanted them:

    truss status          what commit the applier is on, what HEAD is, the gap
                          between them, and if it is refusing something, WHY --
                          the digest comparison in words rather than two hashes
    truss ledger get      the watermark, without a boto3 script
    truss why <sha>       the recorded failure for one commit
    truss skip <sha>      advance past a commit that cannot apply, refusing
                          unless the reason is one it recognises, and writing
                          the break-glass record itself

⚠️ **THE SKIP IS THE DANGEROUS ONE AND MUST BE THE HARDEST.** Advancing the
watermark is editing the applier's memory of what it has done, by hand, out of
band. It was the right call twice on 2026-09-08 and it is exactly the
operation that, made convenient, gets reached for INSTEAD of understanding a
failure. It should demand the reason, record it, and refuse a commit whose
plan the applier has never actually tried.

⚠️ **AND IT MUST NOT NEED A CLUSTER.** The whole value is working from a
laptop when the cluster is the broken thing -- so it reads the ledger over S3
with the same credential the applier uses, never through kubectl.

⚠️ **It also inherits the checksum workaround, which is a reason to build it
rather than keep writing scripts.** Both hand-written scripts died on
`SignatureDoesNotMatch ... Invalid argument` against GCS -- which reads like a
bad credential and is not: recent botocore sends checksum headers Google's S3
API rejects. truss's own ledger client already handles it. In a CLI that
knowledge lives in one place instead of being rediscovered at 2am.

---

# From the 2026-09-09 survey of other appliers

Seven parallel research passes over competing appliers (Atlantis, Spacelift,
HCP Terraform, env0, Scalr, Terrateam, Digger, Terragrunt), the Kubernetes
GitOps controllers, the provenance and transparency-log standards, and the
published post-incident guidance. What follows is only what survived checking
against this tree. Ideas that did not survive are recorded at the bottom, so
the next pass does not re-derive them.

⚠️ **The headline of the survey is that nothing else does the central thing.**
Every competitor applies the plan artefact it just produced, in one process,
with one credential set: Atlantis applies its own `$PLANFILE`, HCP and
Spacelift gate between plan and apply inside a single run object, and
Digger/Terrateam run both on the same CI runner by design. The closest prior
art is not in this ecosystem at all — it is reproducible builds, where an
independent rebuilder re-derives an artefact and compares. This applier is a
rebuilder of one, and its re-derivation runs with the *privileged* identity
rather than a sandboxed approximation, which is why it caught the
read-only-vs-admin no-op divergence a byte-comparison rebuilder has no
category for.

## A partially applied multi-root commit wedges the queue

**FIXED 2026-09-09.** `cmd/truss/apply_partial_multiroot_test.go` reproduced
it first and is kept as the guard. What follows is the mechanism, because the
fix is a narrowing of the digest gate and the argument has to survive it.

`applyOneRoot` runs init, plan, digest-compare and **apply** for one root
before the next root is planned (apply_cmd.go:645), and `applied/<sha>` plus
`AdvanceHead` are written only after every root in the commit has succeeded.
So a failure in root 2 leaves root 1 applied to real infrastructure with HEAD
still on the previous commit.

The next pass re-derives the same commit and re-plans root 1 against
infrastructure that now already carries root 1's changes. That plan is a
no-op, `Canonical` drops no-ops, and the digest is the digest of `[]`. CI
filed the digest of a plan that changed something. They cannot match, and the
refusal that comes out says *"the world moved between review and apply"* —
which is not what happened. Every later pass repeats it identically.

    pass two: the plan for platform does not match the one approved at
    multirootsha (approved 5ad51e49…, ours 37517e5f…): the world moved
    between review and apply

⚠️ **The shared-input case is the common one, not the exotic one.** Touching
`modules/`, `providers.allow` or `.opentofu-version` plans EVERY root
(`repo.TouchedRoots`), so a provider bump is always a multi-root commit.

**The fix: a plan that changes nothing is not digest-gated, because there is
nothing to gate.** The digest proves that what is about to CHANGE is what the
approver read; a plan with no changes in it changes nothing, so it cannot
deviate from what was approved and an attacker gains nothing — the apply is a
no-op either way. It is the same argument `plan.Canonical` already makes for
dropping individual no-op resources, applied to a plan that is entirely
no-ops. It also settles the mirror-image case, which was equally stuck: a
change somebody had already made by hand, exactly as approved, left an empty
plan that was refused forever rather than recorded as already satisfied.

⚠️ **An UNREADABLE plan is refused, by name.** `countResourceChanges` returns
an error for a document it cannot read as a plan, and treating that as "no
changes" would skip the gate on exactly the input nobody understands — absent
reading as compliant, which `internal/gates` exists to keep out. The
no-changes half is asserted in `TestAPlanThatChangesNothingIsNotGated`; the
unreadable half in `TestAPlanThatIsNotAPlanIsRefusedByName`, and the plan
that legitimately changes nothing — OpenTofu omits `resource_changes` when
there is none — in `TestAPlanWithTheChangesKeyOmittedIsNotRefused`.

⚠️ **Two fixtures were wrong and hid this.** `fakeTofu`'s default `show -json`
was `{"resource_changes":[]}`, so every digest-gate unit test drove a plan
that applies nothing — the one input the gate cannot apply to. The default now
changes something, and the tests were re-checked by deleting the gate and
watching them go red. The parity corpus has the same defect in
`test_a_plan_that_differs_from_the_approved_one_is_refused` and
`test_a_root_with_no_approved_plan_is_refused`, whose `tofu_show` is also
empty; those recordings cannot be regenerated from this repository, so the
difference is declared as `EMPTY-PLAN-IS-NOT-GATED` in
`internal/parity/divergences.go` rather than papered over.

⚠️ **This was very likely what `truss skip` was invented for.** The CLI item
above records advancing the watermark by hand, twice, on 2026-09-08, past "a
commit that could never apply" — this failure's signature. Re-read that item
before building `skip`; the reason for it may be gone.

⚠️ **It does NOT make the records write-once.** A commit that genuinely cannot
apply still rewrites `failed/<sha>` every pass, so the retention blocker below
stands.

## leakscan's older exemptions excuse a whole line, not a string

Every exemption in `scripts/leakscan` drops the entire `file:line:content` hit
that matched, so one written as a bare substring also excuses whatever else
shares that line. A real bucket URI on the same line as, say, the OpenTofu
release URL or the 1Password package host would go unreported.

The two exemptions added for `scripts/toolchain` on 2026-09-09 are anchored to
the whole manifest record instead — the line must be that record and nothing
else — so anything extra on it breaks the shape and is refused again.
`scripts/leakscan-test` has the case that proves it.

⚠️ **The broadest one is not an exemption at all**: `scripts/leakscan:36`
drops every hit whose line contains the word `leakscan`, so that the scanner
does not refuse its own patterns. Any line anywhere in the repository that
happens to mention it is exempt from every scan — and this file's own tests
live at a path containing it, so a fixture there is unexamined by
construction. `scripts/leakscan-test` generates its checksum-shaped strings
rather than writing real ones, so its cases no longer lean on this.

⚠️ **The obvious narrow fix does not work as written.** Replacing the word
match with `^scripts/leakscan[a-z-]*:` makes the scanner refuse the tree at
`cmd/truss/git_token_test.go`, where a line reading `token: scripts/leakscan`
is exactly the credential shape — so a file that is not the scanner's own also
depends on the hole today. Both have to move together: rename or reshape that
fixture line first, then narrow the match, with the pair of cases added
before either.

⚠️ **A second axis, and it is closed.** Every exemption here is a substring
match that drops the whole line, so a permitted address with `/../..` after it
walked out of the project it named while the line stayed exempt — measured
2026-09-09 against the release downloads, this repository's own module path,
and the provider registry, all of which scanned clean with a traversal
appended. Anchoring each exemption was tried first and closed exactly one of
them: the traversal simply moves further along the URL. What closed it is one
rule applied after every exemption — a line carrying a `..` path segment is
refused whatever else permitted it — because an address that walks upwards has
no legitimate use here. It covers the exemptions written later as well as the
ones written already.

**Not yet done for the older ones**: the module path, the SVG namespace, the
vendor API roots, the provider registry, the OpenTofu and 1Password
downloads, govulncheck, and — the one that matters most — the store-URI
placeholder, which permits a bucket named by a shell variable or an
angle-bracket placeholder. That exemption guards the highest-value class the
scanner has, and because it excuses the whole line, a line naming a bucket
through a variable AND naming a real one beside it passes clean. Measured
against the real scanner, 2026-09-09.

⚠️ **Writing that example out here is itself refused**, which is the scanner
working: the schemes cannot be named in prose except in the permitted shapes.
The measurement was made in a throwaway repository, not in this file.

Each of those needs the same treatment — an anchor to the line shape it is
written for — plus a refusing case pairing it with a real identifier, and they
should be converted one at a time with the pair added first. Deferred rather
than done because the conversion is mechanical and the window is narrow: it
needs a leak to land on the same line as a permitted string, in a repository
where the whole point is that nothing identifying a deployment is written
down at all.

## The toolchain's download cleanup is not watched

`scripts/toolchain`'s `fetch` installs four traps — EXIT, HUP, INT, TERM — so
an interrupted download does not leave a `.part.<pid>` file in the cache
forever. Nothing in `scripts/toolchain-test` fires them, so that cleanup is a
claim.

(The signal no trap can answer, SIGKILL, is covered by a different mechanism
that *is* watched: `install` sweeps part files whose owning process is gone
before it fetches anything, and `scripts/toolchain-test` proves it removes a
dead run's, leaves a live one's alone, and leaves alone one owned by a
process this user cannot signal — which `kill -0` could not tell from a dead
one, and deleted. That last case cannot be built as root, where everything is
signallable, and the suite counts it as skipped rather than passing it
silently.)

⚠️ **One half of that sweep is unwatched**: the ownership filter that keeps it
away from another user's part file on a shared cache. Deleting it leaves the
suite green, because a fixture needs a file owned by a second uid and that
needs root to arrange. What would close it is a case that runs only where a
second uid is available and is counted as skipped everywhere else, the way the
pid-1 case already is.

⚠️ **Attempted 2026-09-09 and removed.** The traps are installed inside the
command substitution `fetch` runs in, which is a different process from the
script, so only a signal delivered to the process GROUP reaches them — which
is what a Ctrl-C or a closed terminal does, and what a signal aimed at the
script's pid alone does not. Driving that from a test means the harness sends
group signals, and while writing it the harness twice killed the suite it was
part of, leaving a truncated run and no summary. A test that can kill the run
is worse than the gap.

**What would make it safe**: a session of its own that the harness can name
without guessing — `setsid`, with the installer's pid read back from the part
file it writes rather than from `ps`, and a refusal to signal any group that
is not the one just created. That much was working when it was removed; what
was not settled is why the suite exited 2 when the case was in it, and that
has to be understood rather than worked around.

**And two real behaviours, measured 2026-09-09 rather than reasoned about.** A
signal aimed at the script's own pid — rather than at its process group — does
not stop the download. With TERM or HUP the script dies at once and says so,
while the orphaned command substitution goes on fetching and finishes the job:
the archive lands, nothing is left behind, and work continued after whatever
launched it was told it was over. A supervisor that signals a pid rather than
a group, then deletes the cache directory, would be racing it. With INT the
script does not die at all until the download finishes — non-interactive bash
defers SIGINT while waiting on a foreground child — measured at four seconds
against a four-second transfer.

⚠️ **Two earlier versions of this entry were wrong**, both from reasoning
about where the traps live instead of watching what happens: the first said
such a signal leaks the part file (it does not), and the second said the
script "dies immediately", which is true of TERM and HUP and false of INT —
the signal the paragraph above it is about.

## The digest cannot see an import

`countResourceChanges` counts an entry carrying `importing` as a change, so a
plan holding one no longer takes the no-changes skip. That makes the digest
gate **run**; it does not make the import **compared**. `plan.theFilter` drops
every entry whose actions are exactly `["no-op"]` regardless of `importing`,
so CI and the applier both hash it away and a matching digest says nothing
about the import target.

Widening the filter is not available: it is byte-identical to the consumer's
jq and to every digest already recorded in the ledger, and
`internal/plan/digest.go` says what a single byte of divergence costs. What
running the gate does catch is the case that matters most — a root with no
approved digest recorded at all — plus an honest change count in the alert.

**Already paid for:** the shapes, measured 2026-09-09 from OpenTofu's own
struct tags in `internal/command/jsonplan` rather than from documentation.
`resource_changes` is `omitempty`, so a plan that changes nothing omits the
key entirely — refusing on its absence would have wedged, every pass forever,
a root whose last resource a commit destroys. `errored` is **not** omitempty,
so it is present in every plan document OpenTofu emits, which is what tells
an empty plan apart from a document that is not a plan at all. `importing` is
`*Importing, omitempty` on the change, which is why a raw-message test for
`null` is the right check.

⚠️ **What is still unmeasured is the combination**, not the fields: nobody
here has watched a real `tofu show -json` render an import block whose
resource already matches configuration. There is no `tofu` binary in this
environment. The dependence is safe in the meantime because it fails closed —
if the field never appears the check is inert, and if it does the plan is
gated rather than skipped.

## Gates the docs claim and the code does not check

`gates.Protection`'s own comment says adding a field without checking it is
the failure the type exists to make visible. The same failure runs in the
other direction: `scripts/protection`'s payload SETS fields that
`CheckProtection` never reads back, so the per-pass re-read — the whole
"never remembered, always re-read" premise — passes on a repository where
they have since been changed.

| Field | Set by `scripts/protection` | Read back | What it means |
| --- | --- | --- | --- |
| `bypass_pull_request_allowances` | cleared, by omission on PUT | no | named users/teams/apps merge with no review at all |
| `allow_deletions` | `false` | no | `docs/design.md` lists it as required |
| `require_last_push_approval` | not set | no | the approver may also be the last pusher |

`bypass_pull_request_allowances` is the one that matters. It is a field on
the endpoint `forge.Protection` already calls, `wireProtection` does not
decode it, and a non-empty value makes `required_approving_review_count: 1`
and `require_code_owner_reviews: true` untrue for the named actors.
`CheckApproval` still refuses the resulting commit, so this fails closed —
but `docs/threat-model.md`'s "the applier refuses EVERYTHING and alerts" is
not what happens, and the difference between "refuses everything loudly" and
"refuses one commit for a reason that names the wrong cause" is the whole
value of that row.

Already paid for: three fields, three files, and
`TestProtectionScriptSatisfiesTheGate` already exists to keep the payload and
the gate from disagreeing. Absent must be its own case, as everywhere else.

## Rulesets: a hole after all, demonstrated 2026-09-09

⚠️ **AN EARLIER VERSION OF THIS SECTION SAID THIS WAS NOT A HOLE. IT IS, AND
THE EVIDENCE IS A PUSH TO THIS REPOSITORY'S OWN main.** The reasoning that
retired it was that rulesets are additive -- repo and org rulesets layer with
classic protection and the most restrictive rule wins -- so a bypass actor
cannot weaken what classic protection already forbids. That is true and it is
not the whole question. What it misses is the case where the requirement is
enforced by a **ruleset in the first place**, because then there is nothing in
classic protection for it to be more restrictive than.

Measured: a fast-forward push of four commits straight to `main` here, which
GitHub accepted and answered with

    remote: Bypassed rule violations for refs/heads/main:
    remote: - Changes must be made through a pull request.
    remote: - Required status check "check" is expected.

"Bypassed rule violations" is ruleset language, not classic-protection
language -- classic protection declines with a protected-branch hook error and
no push happens. So on this repository the pull-request requirement and the
required check live in a **ruleset**, and the pushing identity is a **bypass
actor** on it. Both rules were skipped and the push succeeded.

⚠️ **`forge.Protection` reads exactly one endpoint:**
`/repos/{o}/{r}/branches/{branch}/protection`. It has never read
`/rulesets` or `/rules/branches/{branch}`, and `gates.Protection` has no field
for a ruleset, an enforcement level, or a bypass actor. So the dangerous
arrangement is not exotic, it is the one in front of us: classic protection
configured and compliant, a ruleset carrying the real requirement, and named
actors permitted to skip it. `CheckProtection` returns no problems and the
applier runs, having satisfied itself about a control that is not the one
actually governing the branch.

⚠️ **The failure is quiet, which is the part that matters.** Turning classic
protection off makes the applier refuse everything and say so in every alert.
Adding a bypass actor to a ruleset changes nothing it can see.

**What closing it takes.** `GET /repos/{o}/{r}/rules/branches/{branch}` returns
the effective rules for a branch across org and repo rulesets already
flattened, which is the right first read -- but it does **not** carry
`bypass_actors`. That needs `GET /repos/{o}/{r}/rulesets?includes_parents=true`
and then each ruleset that targets the branch. A new `forge.Rulesets` reader
and a `gates.CheckRulesets`, mirroring the existing `Protection`/
`CheckProtection` pair, refusing on `enforcement != "active"` and on any
non-empty (or unreadable) `bypass_actors` -- absent must be its own case, as
everywhere else in that package. Never a relaxation of the existing gate: the
two are read together and both must pass.

⚠️ **Also still true, and now more pressing:** GitHub shipped automatic
classic-to-ruleset conversion in August 2026. On a converted repository
`GET /branches/main/protection` 404s, `forge.Protection` errors and the pass
refuses -- fails closed, correctly, but it means truss cannot run at all
against a repository whose owner accepted that migration.

**CLOSED 2026-09-09.** `internal/forge/rulesets.go` and
`internal/gates.CheckRulesets` exist now, wired into `runApplyPass` in
`cmd/truss/apply_cmd.go` beside the `Protection` read, joined into the same
refusal sentence. Verified against GitHub's REST API description (not
guessed): `rules/branches/{branch}` never carries `bypass_actors`, only
`ruleset_id` per entry, confirming the two-read shape above; `enforcement` is
`active` | `evaluate` | `disabled`; a bypass actor's `actor_type` also
includes `User` (not listed above) and `bypass_mode` also includes `exempt`
(likewise not listed above). `rules/branches/{branch}` itself documents that
it omits rules from an `evaluate` or `disabled` ruleset entirely, so a
ruleset reaching `gates.Rulesets.Applicable` at all is proof it was active
moments earlier; `CheckRulesets` refuses one whose *second* read (the
per-ruleset call, which is the only one carrying `bypass_actors`) disagrees
and no longer says `active`, treating that disagreement as a race or an
attempt to dodge the bypass-actor read rather than as "additive and inert".
A ruleset that was never active in the first place is not refused for
existing, matching the ruling above that a non-enforcing ruleset is not
automatically a hole.

⚠️ **OPERATIONAL NOTE: this can stop a live applier, and that is the point.**
If the managed repository has any bypass actor on any ruleset that applies to
`main`, truss now refuses every apply -- correctly, fail-closed, the same
class of stop `CheckProtection` already causes when classic protection is
misconfigured. `CheckRulesets`'s refusal names the ruleset, its id, and the
actor types (and bypass mode) so an operator goes straight to the GitHub UI
for that ruleset rather than re-deriving which one from a generic message.
Before turning this on against a repository nobody has audited for bypass
actors, check `GET /repos/{o}/{r}/rules/branches/main` and each ruleset it
names for a non-empty `bypass_actors` -- the gate will otherwise announce it
the hard way, by refusing the next pass.

## The provisioner / `helm_release` gate is built and wired; `data "external"` is not, and cannot be from here

⚠️ **THE PARITY CORPUS CANNOT REACH THIS GATE, AND THAT IS WORTH KNOWING
BEFORE READING 43 GREEN SCENARIOS AS COVERAGE.** The recordings predate the
gate, so none carries a `configuration` key, and `plan.Declarations` refuses a
document without one. `internal/parity/fakebin` therefore backfills an empty
`configuration` for every recorded `tofu show`, which is the honest answer --
none of those scenarios is about a provisioner -- but it means the fake hands
this gate "declares nothing" every time and could not tell a broken parser from
a working one.

What actually exercises it is `internal/plan`'s own tests, which generate real
plan JSON with a real `tofu` and read it back, including the module-nested case
the root-only reading misses. That is where a regression would surface; parity
would stay green through it.


`docs/threat-model.md` used to credit a grep for `provisioner` blocks and
`external` data sources that lived only in the consumer's CI — "and CI is not
where it matters most." Deferred twice: a regex over HCL source was refused
("gate on the field, never rendered text" — a `provisioner` inside a comment,
a string or a heredoc is the flow-style-YAML bug again), `hashicorp/hcl/v2`
was refused (port-plan.md §7.1 makes every package standard library only,
and a hand-written HCL tokenizer that gets comments, quoting and heredocs
subtly wrong fails OPEN, worse than the gap), and the plan-JSON route was
"the right shape but UNVERIFIED — no tofu binary here."

**Now measured against tofu 1.12.6, and built:** `internal/plan.Declarations`
reads `configuration.root_module.resources[].provisioners[].type` — present
only on a resource that actually declares one — and recurses
`module_calls[*].module` to any depth, because a provisioner inside a module
does NOT appear under `root_module.resources` at all; that array is empty for
it. Reading only the root module fails OPEN, and the platform this serves is
almost entirely modules, so that was the difference between a working gate
and a decorative one. `internal/gates.CheckDeclarations` refuses any
provisioner found this way (it runs arbitrary commands at apply time, on the
machine holding write credentials for four clouds, with no diff of what it
will do) and any `helm_release` resource (closing the gap recorded below),
naming the address and type of every offender, not just the first.

**Wired.** `applyOneRoot` (`cmd/truss/apply_cmd.go`) fetches the plan JSON once
per root — the same `tofu show -json` it already read for the digest gate,
reused rather than run twice — and calls `plan.Declarations` then
`gates.CheckDeclarations` for every root before it applies, credentials
included. Any problem refuses the commit, joined and filed the same shape the
digest refusal already uses beside it. Two things differ deliberately from the
digest gate: it runs even when the plan changes nothing (`changes == 0`) —
because a provisioner is a configuration fact ready to fire the next time
anything touches the resource carrying it, not something the "nothing to
gate" argument for an empty plan applies to — and `credentials` is NOT exempt
here, because the digest gate's exemption is about who could have reviewed
the plan (CI cannot plan that root), which says nothing about whether the
root may run arbitrary commands. `cmd/truss/apply_declarations_test.go`
exercises all four cases end to end: a provisioner refused by address, a
provisioner refused even with zero resource changes (proven by moving the
check inside the `changes > 0` branch and watching that specific test go
red), a clean plan applying, and `credentials` refused exactly like any other
root. The row in `docs/threat-model.md` now describes a check the running
pass performs.

**`data "external"` and `data "http"` remain unaddressed, and this gate
cannot be extended to cover them.** Terraform and OpenTofu read a data source
**during plan**, deferring to apply only when an argument is unknown, so
`data "external"` and `data "http"` in a merged tree execute with the
applier's credentials and network position on every pass — and the daily
drift pass re-plans EVERY root, so an untouched root's data source fires once
a day forever. `provisioner` and `helm_release` are configuration facts a
plan JSON exposes as fields, checkable AFTER the plan runs and BEFORE apply;
a data source has no such checkpoint, because the thing being guarded against
has already executed by the time any plan JSON exists to read. Only a
source-level check — the same HCL-parse this entry ruled out as a dependency,
or a read of the consumer's CI workflow (what ref it checks out, whether
`permissions:` are narrowed, whether its refusal runs before `tofu init`) —
could catch it before it runs, and neither is built. This half is still open
and belongs in this register until one of those is.

## `helm_release` had no enforcer on the tofu side — closed above

A `helm_release` resource in an OpenTofu root reached a chart repository at
*apply* time, with nothing in this tree looking for it.
`internal/gates.CheckDeclarations` above now refuses it, by the same
field-level check over `configuration.root_module.resources[]` (and every
module beneath it) that closes the provisioner gate — no HCL parser needed,
since `type` is a plain field in the plan JSON. It carries the same wiring
as the entry above: called from `applyOneRoot` for every root, on every
plan.

## The ledger records history it cannot prove

`Put` has no precondition, no `Delete` exists, and nothing links one record
to the next. Whoever holds the write credential can roll `HEAD` backwards
(replaying commits) or forwards (silently passing over them), and the
ledger's own bytes carry no evidence of it. `docs/threat-model.md` has no row
for this. The gates are a monitor — they check before the fact. There is no
auditor.

The whole answer is small: a signed checkpoint. `{seq, prev_hash, sha}`,
chained, signed with the `note` package from `x/mod`'s sumdb (Ed25519, a few lines),
written alongside each record and published on the heartbeat that already
goes out every pass — so "who remembers the tip" is answered by machinery
that already runs. A `truss ledger verify` that re-walks the bucket and
refuses a broken chain or an unexpected `seq` is the auditor.

⚠️ **Take the chain, not the tree.** A Merkle tree earns its proof machinery
when the log is too big to re-download. This one is a few thousand rows and
by design will never be the reason to scale. Sigstore's Rekor v2 moved to
exactly this shape — signed checkpoints on plain object storage, no database.
Trillian, tiles and witness networks are all for a different size of problem.

⚠️ **The `seq` doubles as a fencing token.** "One pass at a time" is a
CronJob concurrency policy, not a fence: two passes can both read `HEAD`,
both apply, and both `Put` a record, and only OpenTofu's state lock catches
the apply half. A stale writer's chain will not join at `current+1`, which
turns a silent overwrite into a refusal.

## The create-if-absent measurement is narrower than the comment says

`internal/ledger/store.go`'s note is accurate about what was measured and
draws a conclusion one step too wide. SigV4 obliges every request to carry
`x-amz-*`, the endpoint refuses mixing header families, and so
`x-goog-if-generation-match: 0` is unreachable — **to an AWS-SigV4-signing
client**. GCS's native V4 HMAC signing (`GOOG4-HMAC-SHA256`) exists precisely
to send `x-goog-*` headers, and the generation precondition is a real
create-if-absent under it.

Not a reason to reach for one today — the checkpoint chain above gives
mutual exclusion without a storage primitive. It is a reason to correct the
comment, because "this endpoint offers no create-if-absent primitive" will be
read later as a fact about the bucket rather than about the signer.

## Features other appliers have that this one does not

**Declared ordering between roots.** `TouchedRoots` returns `credentials`,
then `platform`, then `projects/<name>` alphabetically. There are no declared
edges. Two projects where one consumes the other's output apply in the right
order by alphabetical luck. Terragrunt's `dependency` blocks and Spacelift's
stack dependencies exist for exactly this. ⚠️ **The current failure mode is
safe and that is why this is deferred**: an out-of-order apply makes a later
root's plan disagree with its digest, so it is refused rather than applied
wrong. Declared edges would let it succeed instead of refuse. Build it when a
refusal traced to ordering has actually happened; the shape is a topological
sort inside `internal/repo`, pure, no I/O.

**A dead man's switch, instead of 288 messages a day.** The pass runs every
five minutes and sends a Telegram message on every one — "nothing to apply",
288 times, so that silence is detectable. The reasoning is sound and the
mechanism is the wrong one: `compose.go`'s own comment already records why,
that "an alert channel nobody reads is where a real digest-gate refusal goes
to die". The standard answer separates the two signals. The heartbeat pings
an external monitor (Healthchecks.io, Cronitor, Dead Man's Snitch, or a
Prometheus `absent()` rule) which alerts when the ping STOPS; the chat channel
carries only refusals, drift and expiries. Liveness stays provable and the
channel becomes worth reading. ⚠️ This adds a dependency whose *absence* is
the alarm, which is the one kind of dependency that fails safe.

⚠️ **The pushed-metric half of that parenthesis now exists.** Every pass
pushes `truss_pass_timestamp_seconds`, and the first rule in
`observability/alerts/truss.rules.yml` is the staleness check on it.
`HEARTBEAT_PING_URL` stays the mechanism for a deployment with no Prometheus
of its own, and the two are NOT a fallback for each other: one answers "is it
still running" to somebody else's monitor, the other to yours, and neither is
consulted when the other is absent.

**Per-root change counts in the alert.** The counts are already computed.
`platform: +0/~2/-1` per root reads better than one aggregate number, and
costs the breakdown rather than a new mechanism.

**Queue depth in the heartbeat.** Nothing surfaces that the applier is N
commits behind an unresolved failure until somebody goes looking. "Report the
counter that moves" argues for it directly, and until the local CLI exists
the heartbeat is the only place it could show up.

DONE, as `truss_queue_depth`, and it went exactly where this entry said it
would: `runCommitLoop` knew `len(commits)` before it started and now says so.
`TrussQueueIsNotDraining` alerts on the wedge -- deep and not moving -- rather
than on depth alone, because a deep queue that is draining is a busy afternoon.

⚠️ **It is emitted only when the pass actually reached the queue, and the
absence is load-bearing.** A pass refused at the branch-protection gate knows
nothing about how much work is waiting; reporting 0 would be a claim it did
not earn and would read identically to a genuinely empty queue.

⚠️ **It is still not in the heartbeat**, which is what this entry asked for
literally. The heartbeat is read by a human opening an object in a bucket; the
metric is read by a rule. Both are worth having and only one exists.

**Plan-comment length.** Atlantis chunks its PR comment fence-aware because
GitHub truncates. A `platform` plan touching hundreds of resources is the
case; a reviewer who cannot see the whole diff is approving less than the
digest covers, which touches the gate's evidentiary basis rather than just
tidiness. The same limit applies to Telegram for a drift report naming many
roots.

**Artifact attestation on the release.** DONE. Pinning by digest inside the
reviewed diff proves the diff NAMES a digest; it does not prove that digest
came from this CI rather than being typed in. `actions/attest-build-provenance`
plus `gh attestation verify` closes it using infrastructure GitHub already
hosts and this project already trusts for merge-commit verification. ⚠️ It
does NOT belong on the plan digest, where independent re-execution is already
the stronger proof and a signature would be a second way to prove one fact.

To verify an artifact, run: `gh attestation verify <artifact> --repo <owner>/<repo>`

**`govulncheck ./...`** next to `go vet` in the pre-commit chain and in
`ci.yml`. It reports only reachable vulnerabilities, so it does not bring the
noise a scanner would.

**`log/slog`.** PARTLY DONE, and the remaining half is the half this entry
was about. The pass now narrates in logfmt with a level -- `time=… level=warn
msg="…"` -- so "every warning this week" is a field selector instead of a grep
for whichever words a message happened to use, and the counts reach
`truss_pass_log_events{level=…}` so the error and warning panels work with no
log pipeline at all.

⚠️ **The structured ATTRIBUTES are still not there.** The message remains one
quoted prose value; `commit=`, `root=` and `duration=` per call site are what
this entry asked for and are not what landed. The duration part has a partial
answer elsewhere -- `truss_root_duration_seconds{root,phase}` times every step
of every root -- which weakens the case for `duration=` on a log line but not
for the other two.

## `ci.yml` runs `go test ./...` without `-count=1` — DONE

Both `ci.yml` and `release.yml` carry it now, on every test step including
the fuzz one. `actions/setup-go` caches by default and that cache includes
`GOCACHE`, which is where test results live — so without the flag a PR that
does not touch `go.mod` can restore cached results and pass tests it never
ran.

## Nothing can complete a change from outside the cluster

**Raised 2026-09-09 by the question "can't truss itself generate this? why
would I do it".** It is the right question and the answer is a gap.

truss mints GitHub App installation tokens — that is what `truss token` and
`forge.Client.InstallationToken` are — so the system does hold a real GitHub
credential and does refresh it on a clock. But it exists **only inside the
cluster**: the App private key arrives through the mounted credential mirror,
which is populated from the applier's vault using a 1Password service-account
token. A checkout on any other machine has none of that.

So an agent or a maintainer working on this repository from outside can push a
branch — the deploy keys allow it — and can do nothing else. Opening a pull
request, reading a check's status, or merging one all need the API, and the
only identity with API access is a pod that runs for ninety seconds every five
minutes and has no reason to be doing any of it.

⚠️ **The applier is emphatically the wrong thing to reach for here.** Its App
is the identity that reads branch protection and applies approved changes; a
token minted from it merging a pull request would be the applier approving its
own work, which is the inversion this whole project exists to refuse. Whatever
closes this gap has to be a *different* identity with a *smaller* grant.

The shape of an answer, none of it started:

- **A scoped token for the working machine**, minted by `credentials/` like
  everything else and rotated on the same 45-day clock — `pull_requests:
  write` and `checks: read`, nothing more. It is a credential, so it wants
  seeding, sweeping and an entry in the expiry table; that is the whole cost
  and it is the ordinary cost of every other credential here.
- **Or accept it**, and say so where somebody hits it rather than leaving them
  to rediscover it. A branch that is ready and a human who clicks merge is a
  legitimate design; what is not legitimate is it being an accident.

⚠️ **It is worth measuring before building.** This repository requires
`required_approving_review_count: 0` (scripts/repo-protection, and its comment
explains why: one maintainer cannot approve their own pull request). So a merge
here needs green CI and nothing else, and the gap costs one click. In
`../platform`, where code-owner review IS required, the same gap costs nothing
at all — a human has to look regardless. That asymmetry is the argument for
recording this rather than building it today.

Related in kind: `../platform`'s `docs/decisions/tailnet-as-code.md`, which is
the same shape — a control this platform depends on and does not manage.

## `scripts/check` is weaker than it looks on work that is not staged yet

`scripts/leakscan` enumerates with `git ls-files` (`scripts/leakscan:26`), so
it scans **tracked files only**. New work sits untracked until `git add`, which
means a full `scripts/check` run over a branch's worth of new files reports
`check: clean` without having read any of them.

Measured 2026-09-10: three 32+ character hex literals in two new test files
survived several clean `scripts/check` runs and were refused by the very next
run, immediately after the commit that tracked them. Nothing leaked — they were
a fabricated sha and the sha256 of no bytes — but the scan that would have
caught a real one had not looked.

The commit path is not itself broken: by the time anything is committed the
files are tracked, so the scan before the *next* commit sees them. What is
misleading is the habit the checklist encourages — run `scripts/check`, read
`clean`, then commit — because on a first commit of new files those are the
wrong way round.

⚠️ **Do not "fix" this by scanning the working tree.** `git ls-files` is also
what keeps the scan from reading build output, editor droppings and anything
else `.gitignore` covers, and a scanner that refuses a file nobody is
publishing trains people to ignore it. The candidates are: scan `git ls-files`
plus `git diff --cached --name-only`, so staged-but-new files are included; or
have `scripts/check` say out loud how many files it scanned, so "clean" over
zero new files is visibly not the same as "clean" over forty. The second is
smaller and does not change what is refused.

## ~~The kind layer classifies directories the pass never executes~~ (closed)

`internal/repo/units.go` defines `KindTofu` for `clusters/<name>` and
`hosts/<name>`, and `KindAnsible` for `ansible/plays/<name>`, and its own
doc says a kind is "how the applier treats it: what binary renders or plans
it". `runCommitLoop` used to read only `KindRender` out of `TouchedUnits`
(`renderUnitsFor`, `cmd/truss/render_unit.go`); tofu work came from
`TouchedRoots` alone, which only ever names `credentials`, `platform` and
`projects/<name>`. So a commit touching only `clusters/beta/main.tf` was
recorded as a noop and HEAD advanced past it, while the kind layer said that
path was an OpenTofu root that should have been planned and applied.

⚠️ **The behaviour was unchanged from before the kind layer existed — what
was new was a comment claiming otherwise.** `KindTofu` covering clusters and
hosts was added to `KindOf` without the pass gaining a second reader for it,
so the doc comment described a capability the code did not have.

**What closed it.** `TouchedRoots` was not widened — `internal/parity`
compares it, byte for byte, against recordings of the bash it ports, so it
stays exactly as it was. Instead `runCommitLoop` (`cmd/truss/apply_cmd.go`)
now derives tofu work from `tofuUnitsFor`, the credentials+tofu half of
`repo.TouchedUnits` — the same function `renderUnitsFor` already read the
render half of — fed from the new `gitDriver.TreeTofuUnits`
(`cmd/truss/git.go`, `execGit`'s sibling of `TreeRenderUnits`) rather than
`TreeRoots`. A commit touching only `clusters/beta/main.tf` or
`hosts/dev-beta/main.tf` is now planned and applied like any other root.

The swap is safe because, for every commit shape `internal/parity`'s 43
recorded scenarios cover, the credentials+tofu half of `TouchedUnits` and
`TouchedRoots` return the identical set in the identical order: none of the
scenarios carry a `clusters/` or `hosts/` directory, and none touch a path
that is a shared input for units (`inventory/`, `.kustomize-version`,
`.ansible-version`) but not for roots.
`TestTouchedUnitsTofuHalfMatchesTouchedRoots`
(`internal/repo/units_test.go`) pins that equivalence directly.

`internal/parity` did go red once while building this, but not from the two
functions disagreeing: `internal/parity/fakebin`'s `git ls-tree` shim read
the sha at a fixed offset from the `ls-tree` argument, a position that
happens to be correct for `TreeRoots`' two-flag shape (`-d --name-only`) and
wrong for the three-flag shape `TreeRenderUnits` already used and
`TreeTofuUnits` now also uses (`-d -r --name-only`). The wrong sha missed
the scenario's declared tree and fell back to the shim's hard-coded default,
which happened to still satisfy every scenario that only exercised render
units (filtered to `KindRender` the wrong answer and the right one were both
empty) — until the scenario named for exactly this case, whose declared
tree carries a third tofu root the default does not, made the wrong answer
visibly wrong. Per AGENTS.md, the fixture was the bug: fixed in the shim by
finding the sha relative to the `--` pathspec separator instead of a fixed
offset, which is correct for all three shapes.
No entry in `internal/parity/divergences.go` was needed for `TouchedRoots`
vs. `TouchedUnits` itself — the 43 recorded scenarios agree with both
functions once the shim answers the right question.

`TestAClustersRootIsPlannedAndApplied` and `TestAHostsRootIsPlannedAndApplied`
(`cmd/truss/apply_clusters_hosts_test.go`) reproduce the original defect —
both failed with `tofu applied []` against the unfixed code — and pin the
fix, alongside a multi-root ordering test, a shared-input test, and a noop
regression test. `TestExecGitTreeTofuUnitsAgainstARealRepo`
(`cmd/truss/git_tofu_units_test.go`) is the direct test of the new tree
listing against a real git binary.

## Checked and deliberately not wanted

Recorded so the next survey does not re-derive them.

- **OPA/Conftest, Sentinel, Checkov/Trivy/Terrascan.** Every rule this
  applier needs is a small Go function over structured plan JSON, and
  `internal/gates` is already that mechanism; a policy engine is a second
  substrate for one job. Sentinel's *soft-mandatory* level — pass unless
  overridden — is the fails-open-with-a-receipt pattern this project refuses
  by construction, and is worth naming as a class rather than a product.
- **Cost estimation gates.** Needs I/O, which `internal/gates` forbids.
- **SPIFFE/SPIRE.** `internal/secrets/kv.go` already authenticates with a
  projected ServiceAccount JWT and the publisher is audience-scoped into its
  own container. SPIRE's node attestation proves the node is what the
  platform says it is and says nothing about its current integrity, so it
  moves the attestation authority without moving the trust root that
  `docs/threat-model.md` already names.
- **in-toto layouts, full TUF role separation, SBOM generation.** One
  attested step does not need a layout; TUF's role separation solves a
  multi-party registry compromise that does not exist here. TUF's
  expiry-as-refusal discipline is already independently in the sweep.
- **Auto-reconciling drift** (Flux/Spacelift). Already refused on purpose,
  and every hardened-GitOps writeup agrees: undoing a change made
  mid-incident is its own outage.
- **Retry loops.** `internal/secrets/publish.go` already states the rule.
  A failed commit does not advance HEAD, so the five-minute cadence IS the
  retry, and it is automatic — except when the commit can never succeed,
  which is the wedge above, and is a defect rather than a missing retry.
- **Web UI, stack locking, contexts, blueprints, module registries,
  environment TTL, sync waves, health assessment, apply-before-merge.** All
  solve a multi-team or multi-repo problem this does not have, or contradict
  a stated line.
- **OpenTelemetry.** No collector here, no context to propagate across
  services, and a short-lived process is the case it serves worst. Still true
  after metrics landed: what shipped is a Prometheus exposition written by
  hand into a Pushgateway (`internal/metrics`, no dependency), because a batch
  job that exits before any scrape reaches it is exactly the case the
  Pushgateway exists for and exactly the case a tracing SDK serves worst.
  ⚠️ The cost of that choice is written down where it bites: a gateway serves
  the last push forever, so an applier that has stopped reports its final
  healthy state indefinitely, and every alert is anchored on
  `time() - truss_pass_timestamp_seconds` for that reason. See
  `observability/README.md`.
- **A notification abstraction (`shoutrrr`, `notify`).** One transport is
  one way to do things. Revisit only if a second is actually wanted.

## Making the ledger tamper-evident: WORM first, a witness second

**Raised 2026-09-09: "what about something like Hedera HCS? It's cryptographically
secure pubsub more or less. It also creates an immutable audit log."**

The question found a real hole in the signed-hash-chain proposal above, and the
hole is worth stating before the answer. **A locally-signed chain does not
defend the boundary this project already concedes.** It stops an attacker
holding the bucket write credential; it does nothing against root on the
applier's node, because that attacker holds the signing key too and re-forges a
perfectly consistent chain over whatever they rewrote. `docs/threat-model.md`
names that node as the trust root, so the chain protects everything except the
one case the threat model says is the dangerous one.

⚠️ **The prior art agrees, and is blunt about it.** SEC 17a-4's electronic
recordkeeping rule has never accepted a hash chain on its own, because a chain
proves internal consistency and not resistance to the operator who holds the
keys. Compliance-grade systems combine storage-level immutability with an
independently-controlled copy. Two halves, not one.

### The first half is configuration, not code

A **locked** GCS bucket retention policy. From Google's own docs: *"Once you
lock a policy, you cannot remove it or reduce the retention period it has"*,
*"Locking a bucket's retention policy is an irreversible action"*, and objects
*"can only be deleted or replaced once their age is greater than the retention
period"* — enforced by Cloud Storage itself rather than by IAM, so it holds
against the credential holder, the project owner and an org admin alike.
Locking also places a project lien, so the project cannot be deleted out from
under it.

⚠️ **Object holds are NOT this and must not be mistaken for it.** A temporary
or event-based hold is released by whoever has the write credential — the same
identity the attacker has. Only a *locked retention policy* has no release
mechanism for anyone.

⚠️ **Retention forbids replace as well as delete**, so `HEAD` and `heartbeat`
cannot live under it: overwriting either returns `403 retentionPolicyNotMet`.
That forces a design change, and the change is an improvement rather than a
tax. Three options, cleanest last:

1. Two buckets — append-only locked, mutable unlocked. Protects the evidence
   but leaves `HEAD` freely rewritable.
2. Versioning under retention — every historical `HEAD` survives, undeletable.
   Turns a rewrite into something *detectable*, not something prevented.
3. **Delete the mutable pointers.** `HEAD` becomes derived from the immutable
   `applied/` prefix; `heartbeat/<timestamp>` becomes one write-once object per
   pass, liveness being the newest one, aged out by an ordinary lifecycle rule.
   Nothing is left to overwrite, so retention covers the whole ledger and the
   two-bucket split disappears.

⚠️ **Option 3's price is a `List`.** `ledger.Store` has exactly `Get` and
`Put` today, and deriving HEAD needs to enumerate a prefix. That widens the
contract the design doc deliberately keeps narrow — *"the ledger row claims
durable storage and nothing more"* — so it is a real decision, not a detail.
It is still the option that removes a mechanism instead of adding one.

The chain is still worth its ~50 stdlib lines on top, for ordering and
completeness and so `truss ledger verify` is a local computation. But with
WORM underneath, the storage is doing the work and the signature is corroboration.

⚠️ **Two research passes disagreed about whether GCS's newer PER-OBJECT
retention lock is reachable through the S3 interop API** — one says its
`x-goog-object-lock-*` headers cannot be mixed with SigV4's `x-amz-*` (the same
family collision already measured for `x-goog-if-generation-match`), the other
says the S3 XML surface supports it. **It does not matter for this decision**:
the bucket-level policy is configured once out of band with `gcloud` and
enforced server-side against every write path, so it sidesteps the dispute
entirely. If per-object locking is ever wanted, measure it the way the
conditional-write question was measured.

⚠️ **And measure the bucket-level enforcement too, before relying on it.**
That it applies to SigV4 interop writes is an architectural inference — the
interop surface is a front end onto the same objects — not a sentence anyone
found in Google's docs. A scratch bucket and a one-second retention period
answers it in five minutes. This codebase has been wrong about GCS interop
twice.

### Hedera HCS: the mechanism is real, the audit log is not what it says

Recorded in full because the idea is sound and the reasons against it are
specific rather than reflexive.

What is true: a topic's **running hash** is a genuine SHA-384 chain over
(previous hash, topic id, consensus timestamp, sequence number, message,
payer), so deletion or reordering of an earlier message is detectable. A
`submitKey` restricts writes to one key. Messages cannot be edited, and a topic
created without an `adminKey` cannot be deleted.

What the pitch glosses over, and what two counter-arguments raised the same
day correctly fix:

- **History: this was NOT a real objection, and an earlier draft of this
  section said it was.** ⚠️ **The mirror node's "60 days" is a default query
  window, not a retention limit.** Hedera's wording: the API server "adds an
  implicit timestamp range [now — 60d, now] to query the database. However, a
  user can still view data older than 60 days using a timestamp range [T1, T2]
  provided in the query." Older data is retained and retrievable; you supply
  explicit ranges of at most 60 days each. And the limit applies only to the
  Hedera-operated mirror node — "users of third-party mirror node services are
  not affected."

  For this use case it would not bite at all: an audit reads back rarely and
  already knows roughly when a checkpoint was written, so it would pass an
  explicit range regardless.

  ⚠️ **And a mirror node is not the only way back to the data.** Consensus
  nodes push record files *and* signature files to AWS S3 and GCS buckets
  readable under **requester-pays** credentials — the same buckets every mirror
  node ingests from. Anyone willing to pay their own egress can pull them.
  **Durability of the record was never the problem with HCS.**

  ⚠️ **What remains true about Block Nodes is narrower.** HIP-1081 makes
  retention a **deployment tier**: a *Full History* node (genesis onward; the
  "expected Tier 1 configuration", explicitly not mandatory), a *Partial
  History* node (may prune after a configured window), and an *Archive Server*
  role the tier taxonomy notes is **not deployable as a standard profile**. And
  a Block Node serves **blocks, not topics** — `BlockAccessService` and
  `BlockStreamSubscribeService` return raw blocks, and decoding them back to
  one topic's messages is the indexing job mirror nodes exist to do. There is
  no Go client for those APIs. None of that is an argument against HCS; it is
  an argument that **the mirror node, not the Block Node, is what you would
  actually read from**, now and after cutover.

  Status: **Beta**, release candidates only (no 1.0), Java 25 and gRPC,
  council-operated private preview since November 2025, community access from
  Q1 2026, no public endpoint and no announced GA.

  ⚠️ **Open, and worth re-checking after November 2026:** whether the
  Hedera-operated mirror node keeps the same implicit window once block streams
  become canonical and mirror operators re-point at the new buckets. Nothing
  found either way; do not assume it carries over unchanged.
- **Confidentiality: real, and there is a better answer than encryption.**
  `submitKey` is a write ACL only; topic contents are globally readable,
  permanently. Client-side encryption fixes that. ⚠️ **Submitting only the
  checkpoint HASH fixes it better**, because it removes a credential instead of
  adding one. An encryption key must outlive the audit log — lose it in year
  three and the log is gone; leak it and every past payload is readable
  forever, irrevocably, because nothing published there can be withdrawn.
  Ciphertext on a permanent public ledger is a harvest-now-decrypt-later
  surface; a hash is not. The content stays in the operator's own storage,
  which is what the WORM half is already for, and the witness proves only what
  a witness is for: that this checkpoint existed at this time, in this order.
- **Verification: possible today, much cheaper from November 2026.** ⚠️ **An
  earlier draft here said a verifier is "taking a mirror node's word". That is
  true of the REST API and false of the underlying data.** The procedure mirror
  nodes themselves run is documented and anyone can run it: download the
  signature files, verify them against each node's public key from the address
  book, check that at least 1/3 by stake signed the same record-file hash,
  download the record files and verify their hashes, follow the chain hash to
  prove no file is missing, and validate the address book back to genesis.
  Independent verification does not depend on Hedera shipping anything.

  What it depends on is *work*: that procedure has no Go implementation to
  inherit (the mirror node is Java), and it is the price of not trusting a
  REST answer. From the **November 2026** cutover (consensus node
  v0.79) every block carries "a single aggregated Threshold Signature Scheme
  (TSS) signature from the majority of the network by consensus weight" —
  verification that does not depend on whoever served you the data. ⚠️ Block
  Nodes are the plumbing; **TSS is the simplification** — one aggregate
  signature to check instead of collecting a third of the network's. ⚠️ **It is
  not live yet, even in preview**: the private-preview announcement states that
  blocks are not yet signed with HIP-1200 hinTS threshold signatures. So
  November is when verification gets cheap, not when it becomes possible.
  (Staged: v0.75 in July 2026 began wrapped record blocks; the legacy record
  buckets remain readable after cutover, with no stated retention deadline.)
- ⚠️ **The write path is untouched by any of the above, and this is what
  remains.** There is no REST submit path; `TopicMessageSubmitTransaction` is
  gRPC and protobuf. The Go SDK (`hiero-sdk-go`, Linux Foundation
  Decentralized Trust) brings ~21 modules, against a `go.mod` that is three
  lines and a `go.sum` that does not exist. This is architectural rather than
  a current limitation: Block Nodes ingest only from consensus nodes and have
  no transaction-ingress role at all, so nothing downstream will ever open a
  submit path.

  Hand-rolling it is *feasible* — `net/http` speaks HTTP/2, unary gRPC is a
  content-type and a five-byte length prefix, and protobuf encoding is
  mechanical — and that is the same move as the hand-rolled SigV4. ⚠️ **But it
  inverts §7.1's reasoning rather than following it.** SigV4 was worth owning
  *because it is frozen*: "a vendor cannot change a default underneath a signer
  we own." Hedera's wire format is mid-migration through November 2026. Owning
  a moving protocol buys the maintenance burden without the stability that made
  owning the static one correct.
- Price rose to **$0.0008 per submit in January 2026**; at one checkpoint per
  five-minute pass that is roughly $84/year. Small, but it makes an HBAR
  balance a root credential whose failure mode is **depletion** — which the
  expiry sweep does not model, because it watches dates and not balances.
- No SLA.

### HashIO and the JSON-RPC path: checked 2026-09-09, does not exist yet

Checked because if a plain HTTPS-and-JSON submit path existed, the one
remaining objection — gRPC and protobuf against a stdlib-only rule — would
disappear outright. It does not, today.

- **HashIO is real and supports `eth_sendRawTransaction`.** It is Hashgraph's
  hosted deployment of the open-source Hiero JSON-RPC Relay, presenting an
  Ethereum-style JSON-RPC API over Hedera's native transactions. ⚠️ **Its own
  documentation calls it development and testing only**, with "significantly
  restrictive rate limits", describes it as "less reliable", and directs
  production users to a commercial relay or to self-hosting the relay. ⚠️ "Less
  reliable" is a poor property for the one component whose job is to still be
  there afterwards; a commercial relay puts a paid third party in the write
  path; and self-hosting the relay reintroduces gRPC anyway — inside the relay,
  a Node.js service talking to consensus nodes, rather than inside truss.

  It cannot forge a submission, since the transaction is signed locally. It can
  drop, throttle or be down, which shows up as a missing checkpoint — visible,
  and the right failure direction. So it is not something to depend on; self-hosting it is another
  service to run, which is the notariser question again in a different costume.
- ⚠️ **There is no Consensus Service system contract on mainnet.** That is what
  would let an ordinary EVM transaction submit a topic message.
  **HIP-1208, "Consensus Service Precompiled Contract for Smart Contract
  Service", is the proposal for it, and its own front matter says
  `status: Deferred`** — an unmerged pull request opened 2025-06-02, last
  touched 2026-06-23, with no reference implementation (its text says one "will
  be required as part of making this HIP Final") and the gas cost still listed
  as an open issue. The live system contracts are exchange rate, token service,
  account service, schedule service and PRNG. There is no consensus-service
  entry.

  ⚠️ Even if it shipped, its child-transaction model means an EVM success
  receipt would confirm only that the submit was *staged*; finality would still
  be a mirror-node query. Deferred is the honest reading: do not plan around it.
- ⚠️ **HIP-478 is not this, despite its title.** "Interoperability Between
  Smart Contracts and HCS" is `category: Application`, `needs-council-approval:
  No`, last updated 2022, and proposes an **oracle network proxy** between the
  two services — a third party in the write path, which is worse than the SDK,
  not better.
- **What does work today** is deploying an ordinary contract that emits a
  32-byte hash as an event and calling it via `eth_sendRawTransaction`. An
  event log is consensus-timestamped and lands in the block stream, so it
  witnesses a checkpoint about as well as a topic message does.

⚠️ **THE TRANSPORT WAS NEVER THE PROBLEM, AND SAYING "gRPC" MADE IT SOUND LIKE
IT WAS.** JSON-RPC is an HTTPS POST with a JSON body: `net/http` and
`encoding/json` reach HashIO with nothing added. The dependency question is
about *signing* and about *what can be said*, and the two candidate paths each
fail a different half:

| | Signing key | Transport | Can it say "submit a topic message"? |
| --- | --- | --- | --- |
| Native HAPI | Ed25519 — **in the standard library** | protobuf over gRPC — **not** | yes |
| EVM over JSON-RPC | secp256k1 — **not in the standard library** | HTTPS + JSON — **yes** | no: HIP-1208 is unmerged, so only contract events |

Go's `crypto/elliptic` is P-224 through P-521; there is no secp256k1. So the
EVM path needs a curve dependency plus RLP, nonces and gas — one small module
rather than twenty-one, but not zero, and curve arithmetic is precisely the
thing not to hand-roll.

Cost is not the discriminator either way: a contract call emitting one 32-byte
event is roughly 25k-40k gas, about $0.002-0.003, against $0.0008 for a native
submit. Three or four times more, and both are noise.

⚠️ **Which inverts the intuition.** The *native* path is the one closer to
being owned outright: Ed25519 is stdlib, `net/http` speaks HTTP/2, and unary
gRPC is a content-type plus a five-byte length prefix over mechanical protobuf
encoding — the same shape as the hand-rolled SigV4. What argues against owning
it is not difficulty but §7.1's actual reasoning: SigV4 was worth owning
because it is frozen, and Hedera's wire format is mid-migration until November
2026.

**Conclusion: this changes nothing about the plan, because of the notariser.**
If the witness runs as a separate component off the applier's machine, the SDK
lives there and the dependency question never reaches truss — in which case the
supported native path is simply the right one and JSON-RPC buys nothing. HashIO
would only matter if truss submitted directly, and for that it is dev-only,
still needs a non-stdlib curve, and yields an EVM event rather than an HCS
message.

⚠️ **Worth watching: HIP-1208.** If it merges and ships, `eth_sendRawTransaction`
into the HCS precompile becomes a native topic submit over plain JSON, and the
calculus for truss submitting directly changes materially. That is the one
Hedera roadmap item that would.

### Whichever witness, it runs elsewhere

⚠️ **A notariser beside the applier witnesses nothing**, because the same root
owns it. The shape that works is a separate component, off that machine,
reading the append-only prefix and submitting checkpoints — which also keeps
truss stdlib-only, since the SDK lives in the other binary. That is a real
architecture and a real operational cost, and this document already opens with
a section about things living in too many places.

Sigstore's Rekor is the alternative worth pricing against HCS: an append-only
Merkle log with **client-verifiable inclusion and consistency proofs today**,
HTTP/JSON rather than gRPC, self-hostable. Its advantage is entirely one of
timing — after the November TSS cutover, HCS's verification story is arguably
the stronger of the two, and the comparison should be made again then rather
than settled now.

### ⚠️ Locking is blocked on a fact about this ledger, measured 2026-09-09

**Retention refuses a REPLACE, not only a delete, so it protects only a bucket
whose objects are written exactly once. Two of this ledger's writes are not.**

- `failed/<sha>` is rewritten **every pass** while a commit stays refused
  (`apply_cmd.go:632, 640, 653` all write the same key). A wedged commit
  rewrites it 288 times a day.
- `applied/<sha>` is rewritten whenever `AdvanceHead` fails after it was
  written, because the next pass redoes the same commit.

Locking today would turn the two failure modes most worth recording into a
bucket that refuses to record them — a gate that breaks precisely when it
matters, which is worse than the gap it was closing.

⚠️ **A prefix split cannot fix this; only a bucket split can.** Retention is
bucket-scoped, and `LEDGER_HEAD_KEY` is `applied/HEAD` in the real
configuration (`internal/parity/harness.go`) — the mutable pointer already
lives *inside* the append-only prefix.

**What makes the records write-once is timestamped keys** —
`failed/<sha>/<ts>`, `applied/<sha>/<ts>`. That is cheap on the read side and
was checked: **nothing reads `applied/` or `failed/`.** They are write-only
records; only `HEAD`, `heartbeat` and `digests/` are ever read back. So no
`List` is needed for this, unlike deriving HEAD.

⚠️ **But it collides with `internal/parity`,** which compares ledger objects
against the bash by key (`d.Key == "failed/sha1"` in `divergences.go`).
Changing the key shape diverges every failure scenario at once, and parity is
the mechanism validating the port. **So this waits for the cutover** — it is
not hard, it is badly timed, and doing it now would spend the corpus that
exists to prove the port is faithful.

**Plan digests stay in the mutable bucket, deliberately.** They are write-once
per (head sha, root) only until CI re-runs a job, and a re-run re-filing the
same digest would be refused by retention — CI would fail for a reason that is
not about CI. They also do not need WORM: "CI is a witness, never an
instruction" means a rewritten digest can force a refusal and can never cause
an apply. The audit trail that needs to be immutable is what the applier *did*,
which is `applied/` and `failed/`.

### The order

1. **Now, and done:** `scripts/ledger-retention` to set, inspect and lock a
   policy, with `lock` refusing unless the bucket is named back to it; and
   `check-writes`, which lists objects written more than once and is the gate
   that must come up empty before anyone locks anything.
2. **Now, and unanswered:** run
   `TestRetentionIsEnforcedAgainstSigV4Writes` (`internal/ledger`) against a
   scratch bucket. Nobody knows whether Cloud Storage enforces bucket
   retention against SigV4 interop writes; the reasoning that says yes is
   architectural and this package has been wrong twice on exactly that kind of
   reasoning. ⚠️ **If it fails, the whole WORM design is void** and the answer
   belongs in that test's comment before anything else changes.
3. **After the port cutover:** timestamp the record keys, split the records
   into their own bucket, then `set`, watch, `check-writes`, and only then
   `lock`.
4. Later: remove `HEAD` and `heartbeat` as mutable keys, accepting `List`.
5. Later: chain the records, stdlib `crypto/ed25519` and `crypto/sha256`, no
   dependency.
6. Revisit the witness. ⚠️ **Two of the three objections recorded against HCS
   did not survive checking, both corrected 2026-09-09 after the owner
   challenged them**: history is retained (the 60 days is a query window) and
   independent verification is possible today (from the requester-pays
   signature and record files, not from a REST answer). Confidentiality is
   answered by submitting a hash. **What genuinely remains is one thing: the
   write path is gRPC and protobuf, permanently, and that collides with
   port-plan.md §7.1.** The off-machine notariser resolves it by keeping the
   SDK out of truss entirely — so the decision is about whether to run and fund
   a second component, not about whether Hedera is sound.

   Waiting for November 2026 is now only worth it because TSS turns
   verification from "collect a third of the network's signatures and walk the
   address book back to genesis" into "check one aggregate signature". That is
   a large difference in how much verification code has to exist, and none of
   it has to exist before then.

⚠️ **Block Node preview access is obtainable here** — stated 2026-09-09, from a
former Hedera employee. That removes the availability objection and nothing
else: a preview node whose blocks are not yet hinTS-signed hands over the data
while still requiring you to trust its operator, which is the property a mirror
node already gives and is precisely what choosing HCS over Rekor was meant to
avoid.

The right use of that access is **measurement, not adoption**. Three of the
four unknowns above are answerable by someone with a node in front of them and
by nobody else: what full history actually costs in disk, whether a block
stream can be decoded back to one topic's messages without standing up a mirror
node, and when TSS signing genuinely lands. This repository settles questions
like these by measuring — the create-if-absent precondition and the GCS
checksum incompatibility were both settled that way, and both answers were the
opposite of what the documentation implied.

⚠️ **The dependency should also not be a relationship.** A control that exists
to be trustworthy after a compromise cannot rest on a preview programme and
knowing the right people; that has to become an ordinary, documented way to
run or reach a full-history node before anything depends on it. ⚠️ Steps 1-3 are not a substitute for this and do not foreclose
   it — 17a-4's lesson is that the two halves are complementary, and the
   storage half is the one available now.

## A pluggable ledger backend

Today the ledger is object storage over the S3 API, and `ledger.New` returns a
concrete `*ledger.Store` that callers depend on by type. Making the backend an
interface is wanted, with `cmd/truss/ledger_cmd.go`'s `buildLedgerStore` as the
one construction point that already exists to become the seam.

### Why, and it is not abstraction for its own sake

⚠️ **THE CREDENTIAL IS IN THE HOT PATH.** `gcs-ledger` is three of the twelve
fields the applier fetches before it can do anything, and on 2026-09-10 a
DIFFERENT missing credential wedged the applier for hours. Every credential on
the startup path is a way for the applier to be unable to report that it is
unable to work. A backend needing no credential -- a CRD read with the pod's
own ServiceAccount -- removes one.

Kubernetes-native also buys `kubectl get`, RBAC per resource, and **Events**,
which is the part worth having on its own: a pass that did nothing currently
looks identical to a pass that did not happen, and an Event per outcome
(applied, refused, no-op, skipped, drift, expiry-not-checked) says which.

### Candidates, with what each actually costs

| Backend | Gains | Costs |
| --- | --- | --- |
| object storage (today) | survives the cluster entirely; portable; no k8s dependency | a credential on the startup path; S3 signing; GCS rejects botocore checksum headers |
| CRD | no credential; `kubectl get`; Events; RBAC | lives in the cluster's datastore -- and single-server k3s is SQLite on one node's disk, so backups become load-bearing; couples truss to Kubernetes |
| external database | durable, queryable, independent of both | a credential AND a service to run; the most operational surface of the three |
| Hedera Consensus Service | append-only and tamper-EVIDENT, and a topic IS the shape of a journal; consensus timestamps are attested rather than claimed; READS need no credential | only half the ledger fits (see below); reconstructing state means replay; a credential to write |

### ⚠️ What any backend has to satisfy, and the first one is the trap

1. **It must be readable when the thing it records is broken.** The CLI exists
   to be pointed at a stuck applier. A backend reachable only through the
   cluster is unavailable in a share of the incidents it exists for -- though
   note the 2026-09-10 incidents had a healthy API server throughout, so this
   is weaker than it first sounds and should be argued from real outages
   rather than assumed.
2. **It records what ALREADY HAPPENED.** That rules out git: the applier would
   commit to the repository it applies from, and its own writes become commits
   it must then process. Considered and rejected.
3. **Append-mostly.** The watermark moves and entries are added; entries are
   not edited. A backend whose natural operation is mutation is a poor fit.
4. **A backup that fails must be loud.** Moving from "the ledger is object
   storage" to "the ledger is in-cluster, backed up to object storage" turns a
   loud failure (the applier cannot start) into a quiet one (history is being
   lost and nothing says so). `vault-snapshot` failed for over two days
   unnoticed; the same shape here loses the audit record instead of the
   backups.

### ⚠️ HCS FITS HALF THE LEDGER, AND WHICH HALF IS THE WHOLE DESIGN

A Consensus Service topic is an ordered, timestamped, append-only message
stream. That is exactly what `applied/<sha>` and `failed/<sha>` are, and the
consensus timestamp makes "this commit was applied at T" attested rather than
asserted by the process whose behaviour is in question -- which no other
candidate offers.

It is a poor fit for `applied/HEAD`. The watermark is a MUTABLE POINTER, and a
stream has no update. Reading it means replaying the topic, or checkpointing
and replaying the tail.

So the interface probably is not one thing. The ledger is a **journal**
(append-only, HCS fits perfectly) and a **state pointer** (mutable, HCS does
not). Splitting those two before choosing backends is the actual design work,
and it can be done today against the existing object-storage implementation
without committing to anything.

⚠️ **SUBMIT A HASH, NOT THE ENTRY, AND THE PRIVACY OBJECTION GOES AWAY.** The
row above used to say a public record leaks repository names, shas and timing.
It need not: submit `sha256(entry)` and keep the entry itself wherever it
already lives. The public record then proves an entry existed at a time and has
not changed, and says nothing about what it contains. Timing is still visible,
which is a real if smaller leak -- a pass cadence is inferable.

⚠️ **AND READS NEED NO CREDENTIAL, WHICH SERVES THE CASE THE CLI EXISTS FOR.**
A mirror node is public. `truss status` and `truss why <sha>` could verify the
journal from a laptop holding nothing at all -- which is precisely the
laptop-ergonomics blocker recorded above, where the CLI today demands ten
environment variables and a mounted credential tree for commands that only read.
Only writing needs a key, and only the applier writes.

### ⚠️ ANCHOR THE LEDGER; DO NOT NECESSARILY MOVE IT

Tamper-evidence only matters if the writer is not trusted. The writer is the
applier, and a compromised applier IS in this threat model -- it is why the
applier can administer nothing in Vault. Today it holds write access to the
bucket, so it could rewrite its own history. That is a real gap. Moving the
whole journal to a DLT is one way to close it and the most expensive one.

**1. Retention lock on the store that already exists.** GCS Bucket Lock, or
object retention on the ledger prefix, makes entries physically unrewritable --
by the applier, by an operator, by anyone holding the key. No new system, no
new credential, no new failure mode. It does not defend against the storage
provider, which is not the threat; it completely removes "a compromised applier
rewrote history", which is. Cheapest thing on this page and it can be done
today.

**2. Anchor the head; do not relocate the journal.** Hash-chain the entries and
publish only the periodic HEAD hash externally. One small message rather than
every entry: less cost, less leakage, and the ledger stays a store that can be
read without a replay. This gets the tamper-evidence property without the
migration, and it is compatible with any backend -- which makes the choice of
attester a late, reversible decision at one call site rather than a storage
change.

**3. An external attester is already in this repository.** `.github/workflows/
release.yml` runs `actions/attest-build-provenance`, which writes to Sigstore's
Rekor -- an append-only, publicly verifiable transparency log, free, keyless
over OIDC. For "prove this entry existed at a time and has not changed", Rekor
and HCS do the same job, and one of them is already wired in.

⚠️ **WHERE HCS GENUINELY WINS, AND IT IS NOT NOTHING: ORDERING.** A topic is a
consensus-ordered, timestamped sequence as a first-class primitive. Rekor gives
inclusion proofs over independently signed entries; it is a set with proofs,
not a sequence. A ledger IS an ordered journal, so HCS is the better semantic
fit for the journal half. Against that: an account, a key, a mirror-node
dependency and a new failure mode, added to a system whose 2026-09-10 outage
was one credential failing to arrive.

**Suggested order:** do 1 now. Design 2 as part of the journal/state split,
which has to happen anyway. Leave Rekor-versus-HCS as a later decision at the
anchor point, where it is a few lines rather than a migration.

### Not to be done as part of this

A projection is not a plug-in. If the applier writes state into a CR **for
observability** while object storage stays authoritative, that is two copies of
one fact and it is only safe while the direction is strictly one-way. The
moment anything branches on the CR rather than the ledger there are two sources
of truth, and the disagreement will be silent.

## ci.yml and release.yml state the same fact twice

`release.yml` had no tofu install, so every release after #12 failed on
`internal/plan`'s declaration tests -- and nothing noticed, because nothing was
tagged in between. v0.1.4 found it.

⚠️ THE FIX WAS TO COPY THE TWO LINES, WHICH IS THE SAME BUG DEFERRED. ci.yml
runs `scripts/check` precisely so that one definition serves both a developer
and CI; release.yml restates the steps instead. The next test dependency added
to one will be missing from the other in exactly this way.

release.yml should run `scripts/check` too. It cannot simply call it today:
the release job needs `TRUSS_REQUIRE_JQ` set the same way, and it runs
`go vet`/`go test` before a matrix that builds per tofu version, so the order
is not identical. Worth one pass to make it so.
