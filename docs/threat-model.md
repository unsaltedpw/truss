# Threat model

## What stops each attack

| If someone… | What happens | Enforced by |
| --- | --- | --- |
| opens a PR that edits the plan workflow to exfiltrate its secrets | the workflow that runs is `main`'s; the PR is refused a plan | `pull_request_target` + path refusal |
| adds a malicious provider, a `provisioner "local-exec"`, or a `helm_release` resource | provider refused at init; `applyOneRoot` refuses the provisioner/`helm_release` case, naming the address and type, for every root including `credentials` | `-plugin-dir` allowlist; provisioner and `helm_release` detected by `internal/plan.Declarations` (reads `configuration` at every module depth) and refused by `internal/gates.CheckDeclarations`, called from `cmd/truss/apply_cmd.go`'s `applyOneRoot` on every plan, changed or not — see `docs/work-items.md`. `data "external"` / `data "http"` are NOT covered by either: they execute during the applier's own `tofu plan`, before any gate runs, so nothing downstream can prevent them |
| gets an agent's forge login | can open PRs. Cannot approve, cannot merge | CODEOWNERS + required code-owner review |
| approves their own PR with any other account | approval ignored | code-owner review required |
| pushes a new commit after the approval | approval dismissed; the applier also checks the sha | dismiss-stale + `pr.head.sha` check |
| turns branch protection off, using the approver's own account | the applier refuses EVERYTHING and alerts | runtime protection check |
| pushes directly to `main` (protection off) | not a forge merge commit → refused | signature/committer check |
| gets the plan job's secrets | reads configuration and state. Changes nothing, reads no token | plan-tier credentials are read-only, and those states hold no secret |
| swaps what would be applied between review and merge — a compromised CI, a resource that moved, a root applied earlier in the same pass | the applier's own plan stops matching the digest CI filed, so the root is refused rather than applied | the plan-digest gate |
| merges a commit that touches no OpenTofu root without an approval | refused; the queue stops. The gate runs for every commit, not only the ones that apply something | approval + merge-provenance check, before the roots are derived |
| edits a delivery unit's manifests after CI rendered them | the applier's own render stops matching the digest CI filed, so the commit is refused | the render-digest gate |
| deletes a delivery unit instead of editing it | not gated at all: `renderOneUnit` reads the absence as the commit retiring the unit and skips the render gate for it, on purpose — the reconciler removes what it applied, and refusing here would make retiring a workload impossible | — (deliberate exemption, `renderOneUnit`) |
| pushes a commit straight onto the delivery ref | refused. Force pushes and deletion are blocked, and so is an ordinary fast-forward: the gate requires an `update` rule whose only bypass actor is the applier's own App. Measured 2026-09-14 — a deploy key with `contents:write` got GH013 on that ref, and the same App was accepted only once listed. ⚠️ The *creation* of the ref is not gated, which is how the approver opens it by hand | a ruleset the applier re-reads every pass |
| gets root on the box the applier runs on | has everything. **This is the trust root**, stated, not hidden | — |

## What this does NOT protect against

- **Root on the machine the applier runs on** reads its vault credential and
  therefore everything. That node is the trust root. It is why it should run
  the applier and nothing else — a machine that can change everything must not
  also be the machine parsing untrusted input off the open internet.
- **The approver's accounts** are the root of everything above. Both the forge
  and the vault login belong behind hardware keys.
- **Secrets set straight on a provider by hand** — a platform's own
  `secret put` command, for instance — are outside this design. Nothing here
  knows they exist, rotates them, or watches them expire.
- **Branch protection may cost money.** On some plans it cannot be enabled on
  a private repository at all, and the applier then refuses everything —
  correct, but check before the first day.
- **A structurally exceeded rate limit is not fixable by backoff.** If a pass
  reads more from the vault than the account is allowed per hour, retrying
  with backoff only spreads the failure out. Budget the reads, then set the
  cadence from the budget.
- **A single stuck gate now has a narrow override, and everything else still
  has only a blunt one.** `truss skip <sha> --reason <text>`
  (`cmd/truss/skip_cmd.go`) advances HEAD past exactly one commit, which the
  blunt hatch below cannot be aimed at, and it needs the ledger write
  credential rather than repository admin. All four guards are checked before
  anything is written: `--reason` must be non-empty; a `failed/<sha>` record
  must already exist, because the applier has to have actually tried and
  refused this commit — skipping one it never reached would be guessing about
  work nobody attempted; the sha must not already be HEAD; and
  `TRUSS_SKIP_I_UNDERSTAND` must equal that exact sha, because a flag a script
  can set reflexively on every retry is not a confirmation.

  It announces before it acts, and a skip it cannot announce does not happen:
  the alert is sent first, and if the send fails, nothing is written and HEAD
  does not move. That is what earns this hatch the same "can never do it
  quietly" property the blunt one has — enforced by that ordering, not assumed
  from it.

  ⚠️ **What it can walk past is exactly what gate 1 above refuses** — a commit
  with no approval, or no forge merge commit signed by the forge. That is the
  reason it exists: a commit whose plan can never apply still has to be got
  past somehow. It is also the danger the guards above are narrowing, not
  removing: this command applies what the approval gate refused.

  The other override is still all-or-nothing. No gate can be waived, skipped
  or forced individually by touching protection: a check refusing for a
  reason that turns out to be wrong cannot be argued with that way. What
  exists instead is turning protection off entirely, fixing the condition, and
  turning it back on — `scripts/protection off` / `on`, described in
  [docs/operations.md](operations.md).

  That is a real escape hatch and it is deliberately a blunt one. It cannot be
  aimed at one commit or one rule, it takes the whole repository out of
  enforcement while it is open, and the applier refuses every commit and says
  so in every alert for as long as it lasts. An operator with repository admin
  can therefore always get unstuck, which is the point — and can never do it
  quietly, which is the other point.
