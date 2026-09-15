# Observability

Truss reports what it did, every pass, as Prometheus metrics — and the three
Grafana dashboards and the alerting rules in this directory read them.

    alerts/truss.rules.yml            every truss alert, with the reasoning inline
    alerts/vault.rules.yml            and the credential store's own
    dashboards/truss-overview.json    is it alive, what did it do, what broke
    dashboards/truss-timeline.json    what state it was in, when
    dashboards/truss-logs.json        what it actually said, line by line
    dashboards/vault.json             the credential store, and what truss can read of it

Nothing here is deployment-specific: no host, no bucket, no vault name. The one
thing you supply is where to push.

## ⚠️ Read this before writing a single query

**Truss is a CronJob. It pushes; nothing scrapes it.** The pass exists for the
length of one run, so by the time a scrape arrived the process holding the
numbers has exited. It pushes to a **Prometheus Pushgateway**, which is the
component for exactly this case.

Three consequences, and none of them are optional reading:

**1. A Pushgateway serves the last thing it was given, forever.** If truss
stops running entirely — the image will not pull, the CronJob was suspended,
the node is gone — every metric here keeps answering with whatever the last
healthy pass said. `truss_pass_success` stays `1`. A dashboard built on the
outcome series alone reports a dead applier as a healthy one.

> **`time() - truss_pass_timestamp_seconds` is the only expression that goes
> bad on its own when nothing pushes.** It is the dead man's switch, it is the
> first rule in `alerts/truss.rules.yml`, and it is the first tile on the
> overview dashboard. Anchor on it; read nothing else as evidence when it is
> red.

**2. Every sample is a gauge, and `rate()` does not mean what it usually
means.** A counter's meaning is "monotonically increasing since this process
started"; this process starts, counts to three and exits, so the next pass
would push a smaller number and Prometheus would read the drop as a counter
reset. What truss can honestly report is the state of **one pass**.

So `truss_pass_commits_applied` is "the last pass applied N commits", not a
running total. `increase(...[1d])` over it is wrong twice over: the gateway is
scraped every 15 seconds and serves the same value each time, so the same pass
is counted repeatedly. Query these as state — `truss_digest_refusals > 0`, not
`rate(truss_digest_refusals[5m])`.

**3. The two passes push under different grouping keys.** The frequent pass
(every five minutes) applies commits and never rotates; the daily pass rotates,
publishes and sweeps expiries and never applies. They arrive as
`pass="frequent"` and `pass="drift"`. Under one key the frequent pass would
overwrite the daily one's rotation and expiry series five minutes after they
were written, so **a query for anything rotation- or expiry-shaped must say
`{pass="drift"}`**.

## Wiring it up

### ⚠️ The order is forced, and getting it wrong looks like nothing happening

The image and the variable have to land **together**, in one apply.

`METRICS_PUSH_URL` set on an image that predates the metrics code is a **silent
no-op** — `config.Load` ignores a variable it does not know, so nothing errors,
nothing warns, and every dashboard reads "No data" with no clue why. The way to
tell a deployed image emits metrics at all is that `truss_build_info` exists.

1. Land the engine change and release an image from it.
2. Set the CronJobs' image digest **and** `METRICS_PUSH_URL` in the same apply.
3. Bring up the gateway, then load the dashboards and rules.
4. Add the scrape jobs — ⚠️ `honor_labels: true` is mandatory. Without it the
   gateway's own `job`/`instance` labels overwrite the ones truss pushed, and
   every selector in every dashboard and rule matches nothing.

⚠️ **The runbook for a particular cluster does not live here.** This repository
is the shareable engine and `scripts/leakscan` keeps it that way: a document
naming a host, a namespace or a Grafana URL is naming somebody's deployment,
and it belongs beside the manifests in that deployment's own repository.

### 1. Point truss at a gateway

`METRICS_PUSH_URL` is the gateway's root. Optional with no default: unset, the
feature is off and nothing about the pass changes.

```yaml
- name: METRICS_PUSH_URL
  value: http://pushgateway.monitoring.svc:9091
```

Set it on **both** CronJobs — the frequent one and the daily drift one. A
gateway that is down or wrong cannot fail a pass: the push is the last thing a
pass does, its failure is a non-fatal `level=warn` line, and the alert and the
heartbeat have already been written by then.

⚠️ **On a truss build that predates this feature, the variable does nothing —
silently.** `config.Load` reads only the variables truss was built knowing by
name; one it has never heard of is ignored, with no error and no log line. The
pass runs exactly as before and pushes nothing, so every dashboard reads "No
data" — which is indistinguishable from an applier that has gone quiet, and is
the one thing this whole directory exists to refuse. **Deploy the variable and
the image together.**

`truss_build_info` is how to tell the two apart afterwards: it exists only if a
build that emits metrics actually ran. No truss series at all means the image,
not the gateway.

⚠️ **It is a bearer secret in the same sense the dead-man's-switch ping URL
is.** A gateway behind basic auth carries its credentials in the URL's
userinfo. Truss never logs it, never echoes it into an error, and never renders
it into the heartbeat or the chat message. Keep it out of anything else that
prints its environment.

### 2. Run a gateway

```yaml
# The gateway holds state in memory. --persistence.file survives a restart;
# without it, a restarted gateway serves nothing until the next pass pushes,
# and TrussIsNotReporting fires — which is correct, and avoidable.
args: ["--persistence.file=/data/pushgateway.store", "--persistence.interval=1m"]
```

### 3. Scrape it — with `honor_labels: true`

```yaml
scrape_configs:
  - job_name: pushgateway
    honor_labels: true          # ⚠️ NOT OPTIONAL. See below.
    static_configs:
      - targets: ['pushgateway.monitoring.svc:9091']
```

⚠️ **Without `honor_labels: true` nothing in this directory works, and nothing
says so.** The gateway derives `job="truss"` and `pass="frequent"` from the
push URL's path and exposes them as labels on every series. Prometheus's
default is to overwrite a target's `job` label with the scrape job's name — so
every series arrives as `job="pushgateway"`, every selector in the rules and
dashboards matches nothing, and every panel renders "No data" exactly as though
truss were quiet. Set it once; check one panel.

### 4. Load the rules and the dashboards

The rules file is a standard Prometheus `groups:` document — mount it wherever
your Prometheus reads rules from, or paste it into a `PrometheusRule` CR's
`spec` if you run the operator.

The dashboards import as-is. Each declares a `datasource` template variable
rather than baking a datasource UID, so they land in any Grafana without
editing; pick your Prometheus from the dropdown on first open.

## Alerts

`alerts/truss.rules.yml` carries its reasoning inline. The shape of it:

| Group | Fires when |
|---|---|
| `truss-liveness` | no pass in 15 minutes; no daily pass in 30 hours; no metrics at all |
| `truss-refusals` | a plan did not hash to the approved one; a commit did not pass the commit gate; branch protection or a ruleset no longer meets the bar |
| `truss-failures` | an apply, plan or credential failure; a ledger object that could not be written; an error-level log line; a pass slower than its own cadence |
| `truss-credentials` | a credential expired, expiring within 14 days, or recording no expiry; a sweep that could not run |
| `truss-rotation` | rotation failed; rotation is not running at all; the publisher did not confirm the write |
| `truss-queue` | the queue is deep and not draining |
| `truss-drift` | a root drifted; a root whose drift could not be checked |

`alerts/vault.rules.yml` covers the store itself, because truss can be
perfectly healthy and refusing everything simply because Vault is sealed:

| Group | Fires when |
|---|---|
| `vault-availability` | Vault is sealed; no Vault metrics exist at all; the audit log cannot be written |
| `vault-health` | leases climbing; goroutines climbing; mean latency above 250ms |

⚠️ **`VaultAuditLogIsFailing` is the one that looks like nothing.** Vault
refuses any request it cannot write an audit record for, so with every device
failing it serves nothing while remaining unsealed, healthy and `up` by every
other measure — and truss's failures then read as permission problems.

⚠️ **`VaultIsNotReporting` is what makes the rest of that file honest.** Vault
emits nothing without a `telemetry` stanza, and without `disable_hostname =
true` every metric is prefixed with the pod's hostname so no selector matches.
Either way the other five rules are silent — they cannot fire on data that
does not exist. This is the rule that says so.

Two of these deserve naming outright:

**`TrussPlanDigestRefused` is the alert this project exists to send.** The plan
truss re-planned did not hash to the plan a human approved. It repeats every
pass until somebody resolves it — the queue does not advance past a refused
commit — so it stays firing rather than flapping once and resolving itself.

**`TrussRotationIsNotRunning` catches the silence.** A rotation that *fails*
alerts loudly. A rotation that is *skipped* — no credentials root, the state
lock held elsewhere — reports no failure at all, and the applier looks entirely
healthy while the 45-day clock keeps running.

## What truss emits

Every series carries `job="truss"` and `pass="frequent"|"drift"` from the
grouping key, on top of the labels below.

| Metric | Labels | What it is |
|---|---|---|
| `truss_pass_timestamp_seconds` | | when the pass finished — **the liveness signal** |
| `truss_pass_duration_seconds` | | how long it took, gate to alert |
| `truss_pass_success` | | 1 when the pass reported no failure |
| `truss_pass_failure` | `class` | 1 per class of thing that went wrong; a pass can set several |
| `truss_pass_commits_applied` | | commits this pass applied |
| `truss_pass_commits_noop` | | commits recorded as touching no root |
| `truss_queue_depth` | | commits waiting when the pass looked — **absent** when it never reached the queue |
| `truss_pass_lock_contended` | | 1 when another holder had the state lock — a lock held past `lockLeakAfter` is a `lock`-class failure instead, not contention |
| `truss_pass_ledger_errors` | | ledger objects that could not be written |
| `truss_pass_log_events` | `level` | lines logged at `warn` and `error` |
| `truss_gate_ok` | `gate` | 1 when `protection` / `rulesets` met the bar |
| `truss_digest_checks` | | roots whose digest was compared against the approved one |
| `truss_digest_refusals` | | roots refused because the plan did not match |
| `truss_root_duration_seconds` | `root`, `phase` | seconds in `init`, `plan`, `show`, `apply`, `drift` |
| `truss_root_resource_changes` | `root` | resource changes in the plan that was applied |
| `truss_root_failures` | `root` | times this root failed or was refused |
| `truss_root_drifted` | `root` | 1 per root that no longer matches its configuration |
| `truss_root_drift_errored` | `root` | 1 per root whose drift could **not** be checked |
| `truss_drift_ran` | | 1 when the pass actually planned every root |
| `truss_rotation_ran` | | 1 when the credentials root was re-applied |
| `truss_rotation_ok` | | 1 when rotation reported no error |
| `truss_rotation_changes` | | resource changes rotation made — above zero only on a boundary day |
| `truss_publish_attempted` | | 1 when the publisher sidecar was contacted |
| `truss_publish_ok` | | 1 when it answered without an error |
| `truss_publish_expiries` | | expiry dates the publisher recorded |
| `truss_expiry_sweep_ok` | | 1 when the daily sweep completed and **earned** its answer |
| `truss_expiry_findings` | | credentials the sweep reported |
| `truss_credential_days_left` | `credential` | days until expiry; negative means it went |
| `truss_credential_expiry_unrecorded` | `credential` | 1 per credential recording no expiry at all |
| `truss_build_info` | `go_version`, `revision` | always 1; the labels are the payload |

### The four that are easy to misread

**`truss_queue_depth` absent is not `truss_queue_depth` zero.** A pass refused
at the branch-protection gate never runs the commit loop, so it knows nothing
about how much work is waiting and reports nothing rather than claiming a
number it did not measure. Empty beside a red *Last pass* means "we are not
looking"; zero means "nothing to do". `TrussQueueIsNotDraining` fires on the
wedge — a commit at the head of the queue that can never succeed, which every
pass re-attempts and re-refuses while the pile behind it grows. Every other
series looks like a steady state while that happens.



**`truss_expiry_sweep_ok` has to be read before any finding.** The sweep
refuses to claim a clean bill it did not earn — but a sweep that *could not
run* reports no findings, which is exactly what a clean sweep looks like. Zero
findings means nothing until this is `1`.

**`truss_credential_expiry_unrecorded` is not "does not expire".** `never` is a
recorded value and does not appear here. What appears here is a credential
whose lifetime nothing is watching, so its lapse gets discovered by an outage.

**`truss_root_drifted` and `truss_root_drift_errored` are different claims.**
"We looked and it drifted" and "we could not tell" are not the same fact, and
the second is the one that hides a broken provider credential. Nothing folds
them together; neither should a panel.

### The failure classes

`truss_pass_failure{class="..."}` exists because the alert text is a sentence
and a rule cannot filter on a sentence — one that tried would be a regex over
prose. Every class is pushed every pass, most of them as `0`, so a query
returning "no data" means *nothing pushed* rather than *nothing went wrong*.

| Class | Means |
|---|---|
| `protection` | branch protection does not meet the bar |
| `rulesets` | a ruleset does not meet the bar |
| `commit` | a commit did not get to `main` the way it must have |
| `digest` | **the plan did not hash to the approved one** |
| `plan` | `tofu init` / `plan` / `show` failed |
| `apply` | `tofu apply` failed |
| `credentials` | a credential the apply needs is missing or unreadable |
| `repo` | the working copy could not be cloned, fetched or read |
| `forge` | the forge would not answer, or would not mint a token |
| `rotation` | rotating the credentials root failed |
| `publish` | the publisher handoff failed |
| `ledger` | a ledger object could not be written |
| `config` | the repository does not contain what it says it does |

`digest` and `commit` are the two that mean something got past the review path.
The rest are operational. They are never collapsed into one severity, because
they want different people.

## Logs

Every pass narrates to **stderr in logfmt**, with a level:

    time=09:31:04 level=info msg="considering 4f2a91c"
    time=09:31:07 level=info msg="plan for platform matches the one approved at 4f2a91c"
    time=09:32:15 level=warn msg="telegram send failed (non-fatal): ..."

`level=warn` is something that was noticed and survived. `level=error` is
something that was **lost** — a ledger object that was not written, a durable
record that no longer exists.

Both are counted into `truss_pass_log_events{level=...}`, so **the error and
warning panels and their alerts work with no log pipeline at all** — and do
not stop working when one breaks.

`dashboards/truss-logs.json` is the other half: the lines themselves. It
queries **Loki**, not Prometheus, and declares its own `loki` datasource
variable for that reason.

⚠️ **Ship these logs somewhere, or you do not have them.** The frequent pass
runs every five minutes with `successfulJobsHistoryLimit: 3`, so a successful
pass's pod logs are deleted with its Job roughly **fifteen minutes** later.
Every incident this project has had was investigated from logs; an incident
noticed an hour later currently has none. That is what the collector is for,
and it is why this is not a nicety.

The collector promotes `level` to a real Loki label, so `{level="error"}` is
an index lookup rather than a substring scan. Lines that are not logfmt —
`tofu`, `git` and Vault all write prose to the same stream — keep their
content and simply carry no level; a pipeline that dropped them would lose the
plan output that explains a failure.

⚠️ **The pod name is deliberately not a label.** Every CronJob run has a new
pod name, so labelling by it would mint a Loki stream per pass — 288 a day for
the frequent pass alone, which is the standard way a small Loki falls over.
It travels as structured metadata instead: queryable and displayable, not an
index key.

⚠️ **The message is one quoted prose value on purpose.** Structured attributes
per call site — `commit=`, `root=`, `duration=` — remain the entry in
`docs/work-items.md` they were; this change adds the level field the panels
filter on and does not pretend to be that work.

## The vault dashboard

`dashboards/vault.json` is in two halves.

The **top** is Vault's own telemetry, written from Vault's documented metric
names. Vault emits none of it until its config has

```hcl
telemetry {
  prometheus_retention_time = "24h"
  disable_hostname          = true
}
```

and the endpoint is readable by the scraper — either
`unauthenticated_metrics_access = true` on the listener's `telemetry` block, or
a scrape carrying a token with `read` on `sys/metrics`. Verify the names once,
from somewhere that can reach it:

    curl -s "$VAULT_ADDR/v1/sys/metrics?format=prometheus" | grep '^# TYPE'

Running **OpenBao**? Every metric is `bao_*` rather than `vault_*` — one
find-and-replace on the JSON.

The **bottom row** is truss's own view of that vault, and is the part no
generic Vault dashboard can give you: whether the applier's daily read
completed, and how long the credentials it can see have left. The applier's
role can list an item and read one metadata field — when it expires — and
nothing else. It cannot read a secret's value and it cannot write one, so that
row is the whole of what it can honestly say.

### `push_time_seconds` is the gateway's clock, not truss's

A Pushgateway adds `push_time_seconds` and `push_failure_time_seconds` to
every group it holds, and they look like they answer the same question
`truss_pass_timestamp_seconds` does. They do not, quite: the gateway's is when
the push ARRIVED, truss's is when the pass FINISHED by its own clock. Alert on
truss's — it is the one that keeps meaning the same thing if the transport
ever changes, and it is what every rule here selects.

## Keeping this directory honest

A dashboard that names a metric the code no longer emits renders "No data",
which looks exactly like a quiet week. `cmd/truss/metrics_contract_test.go`
fails the build when that happens, in both directions:

- every `truss_*` series a dashboard or rule names must be one `passMetrics`
  can emit (the `vault_*` series are Vault's, not truss's, and are checked by
  parsing rather than by this);
- every series `passMetrics` emits must be named by at least one dashboard or
  rule — an unwatched metric is one nobody will notice the absence of either.

So renaming a metric fails `go test` until the artifacts here move with it.
This is the same hazard `internal/plan/digest.go` carries against the
consumer's `plan-digest` jq, answered the same way.

A metric name being real is not the same as a query being valid, though, and
`go test` cannot tell the difference — a syntactically broken expression names
perfectly good metrics. `scripts/check-observability` parses every alerting
rule and every panel query, routing each one to the parser for the datasource
its panel actually names — PromQL to `promtool`, LogQL to `logcli`:

    scripts/check-observability          # skips loudly without promtool

It runs in CI with `TRUSS_REQUIRE_PROMTOOL=1`, which turns that skip into a
failure for both tools. Worth knowing what each half stops: a bad panel query
renders "No data", which is what a quiet week looks like; a bad rule makes
Prometheus reject the **whole file**, silently disarming every other rule
beside it.

⚠️ **Routing by datasource is the part that is easy to get wrong.** LogQL fed
to `promtool` is a syntax error on every Loki panel, and the obvious fix —
ignoring what `promtool` rejects — would have silently stopped checking real
PromQL errors too. `logcli query --stdin` cannot execute an aggregation and
answers `Metrics Query: not supported` for a well-formed one; it parses first,
so the discriminator is the *parse error*, and that is what the script tests
for. Measured: a bad duration unit, an empty label matcher, a misspelt
function, an unbalanced paren and an unknown parser stage each produce one
from inside the same aggregation.

### What has actually been run against real components

Not asserted — measured on 2026-09-09, against real Pushgateway v1.11.1,
Prometheus v3.5.0 and Grafana v11.6.1, with truss pushing real passes:

**The exposition.** Parses with `prometheus/common/expfmt`, the parser
Prometheus itself uses: 29 families, 43 series, escaping round-trips exactly.
A real Pushgateway accepted two passes and re-exposed 78 series under
`job="truss"` with `pass="frequent"` and `pass="drift"` intact and **not**
overwriting each other — which is the whole reason the grouping key carries
the pass.

**`PUT` versus `POST`.** Watched: a family the second push stopped emitting
was gone from the gateway afterwards. That was a claim in
`internal/metrics/push.go`; it is now something somebody has seen happen.

**Why `Render` always writes `# TYPE`.** Found the hard way, pushing a
hand-written body without them: the gateway answered *"is not a GAUGE"* and
**rejected the entire push** — four families discarded for one omission. That
is the "one 400 for the WHOLE push" this file warns about, observed rather
than reasoned about.

**`honor_labels: true`.** Scraped through a real Prometheus with it set, and
the series arrive as `{job="truss", pass="drift"}`. This is the setting whose
absence breaks every selector here while looking like nothing.

**The alerting rules.** All 26 — 20 truss and 6 vault — load into Prometheus
across all 9 groups and evaluate against real data with no `lastError`. With
no Vault in the lab, `VaultIsNotReporting` is the one that goes pending and
the other five stay inactive, which is exactly what that rule is for. Better: they were
watched going **red and then green**. The test fixture's clock runs behind
real time, so the first push left `TrussApplierStopped`, `TrussDriftPassStopped`,
`TrussRotationIsNotRunning` and `TrussExpirySweepCouldNotRun` all pending —
the dead man's switch firing on a stale timestamp, which is the one behaviour
this whole file is built around. Pushing a current, healthy state returned
every one of them to inactive.

**The dashboards.** All four load into Grafana with no provisioning error:
71 panels across `stat`, `timeseries`, `state-timeline`, `table`, `logs` and
`text`, every one accepted. All 58 PromQL panel queries were run against real
data: **0 errored**, 37 returned data, and every empty one is a `vault_*`
series (no Vault in that lab) or a family truss only emits when there is
something to report — no drifted root, no expiring credential, no failed root.
A query driven through Grafana's own datasource path returned
`truss_pass_commits_applied{pass="frequent"} 1` and `{pass="drift"} 0`, which
is exactly what those two passes did.

**The logs half.** Loki v3.4.2 was loaded with lines shaped the way the
collector produces them and all **8 LogQL panel queries ran against it: 0
errored, every one returned data.** Two results are worth stating because they
are the design, not the plumbing:

- *Full narration* returned every stream including the level-less `tofu`
  prose. A pipeline that dropped what it could not parse would have lost the
  plan output that explains a failure, and this is the query that would have
  shown it.
- *Every error and warning line* returned exactly the `warn` and `error`
  streams and nothing else — `level` as an index lookup, which is what the
  collector promoting it buys.

Driven through Grafana's own datasource path, the same query returned truss's
actual narration:

    time=09:32:15 level=warn  msg="telegram send failed (non-fatal): i/o timeout"
    time=09:33:01 level=error msg="could not file rotation-…: permission denied"

**The collector config.** Passes `alloy fmt`, `alloy validate` and a real
`alloy run` against Alloy v1.10.0 — the only errors from the last being "not
running in a cluster". `alloy validate` does **not** catch a dangling
component reference; loading it does, and both were checked. And `kustomize
build` refused an earlier version of the manifests outright, because the
applier overlay's `namespace: infra` had silently rewritten a `vault`-namespace
Role.

**What still has not been run.** The `vault_*` half of `dashboards/vault.json`
has never met a Vault — those metric names come from Vault's documentation,
and the one `curl` that settles them is in that dashboard's own first panel.
Nothing here has touched the deployment: no manifest applied, no tailnet tag
claimed, no scrape against your Prometheus.
