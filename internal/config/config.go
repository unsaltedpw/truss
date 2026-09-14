// Package config loads and validates environment configuration.
package config

import (
	"fmt"
	"strconv"
)

// Config holds all configuration knobs loaded from the environment.
type Config struct {
	Repo, Approver                                string
	LedgerBucket                                  string
	LedgerAppliedPrefix, LedgerFailedPrefix       string
	LedgerHeadKey, HeartbeatKey, PlanDigestPrefix string
	Workdir, OPTokenFile                          string
	SecretsDir, PluginDir                         string // defaulted
	RequiredCheck                                 string // "plan"
	ExpiryWarnDays                                int    // defaulted 30
	DriftOnly                                     bool
	// DeliveryBypassActorID is the GitHub App id of the one identity allowed to
	// bypass the delivery ref's rules -- the applier's own App. Zero means the
	// deployment named nobody, which is a real answer rather than a value to
	// default: without an `update` rule on the ref nothing needs it, and
	// gates.CheckDeliveryRef treats it as "cannot verify", so a rule arriving
	// unaccompanied is refused with a message naming this variable.
	//
	// It is an identifier, not a credential. Like the tailnet name on the
	// applier side, it belongs stated in the manifest where the diff shows it,
	// because the thing it decides is who may skip a path to production.
	DeliveryBypassActorID int
	// HeartbeatPingURL is a dead-man's-switch monitor (Healthchecks.io,
	// Cronitor, ...) the pass pings on every completion, success or not.
	// Optional with NO default: empty means the feature is off and nothing
	// about the pass changes. It is a bearer secret -- the ping URL alone
	// authenticates to the monitor -- so it must never be logged, and it is
	// never rendered into the heartbeat or the chat message.
	HeartbeatPingURL string
	// MetricsPushURL is the root of a Prometheus Pushgateway
	// ("http://pushgateway.monitoring.svc:9091"), which the pass pushes its
	// whole metric set to as its last act. Optional with NO default: empty
	// means the feature is off and nothing about the pass changes.
	//
	// ⚠️ A PUSHGATEWAY, NOT A PROMETHEUS. The pass is a CronJob and is gone
	// before any scrape could reach it; the gateway is the component that
	// holds a batch job's last numbers for the scrape to find. What that
	// means for anybody writing a query is stated in observability/README.md
	// and is not optional reading: the gateway keeps serving the last push
	// forever, so an applier that has stopped running entirely still reports
	// its final healthy state.
	//
	// It is a bearer secret in the same sense HeartbeatPingURL is -- it can
	// carry credentials in its userinfo -- so it is never logged, never
	// echoed into a problem string, and never rendered into the heartbeat or
	// the chat message.
	MetricsPushURL string
}

// Load reads and validates configuration from the environment via the provided
// getenv function. It returns a Config and a list of all problems found.
// If any problems are found, the Config is not usable and the problem list is non-empty.
func Load(getenv func(string) string) (Config, []string) {
	var problems []string
	cfg := Config{RequiredCheck: "plan"}

	// Load required variables
	requiredVars := []string{
		"REPO", "APPROVER", "LEDGER_BUCKET",
		"LEDGER_APPLIED_PREFIX", "LEDGER_FAILED_PREFIX", "LEDGER_HEAD_KEY",
		"HEARTBEAT_KEY", "PLAN_DIGEST_PREFIX", "WORKDIR", "OP_TOKEN_FILE",
	}

	values := make(map[string]string)
	for _, name := range requiredVars {
		values[name] = getenv(name)
		if values[name] == "" {
			problems = append(problems, fmt.Sprintf("refusing to start: %s is unset", name))
		}
	}

	// Set required fields only if they were provided
	if values["REPO"] != "" {
		cfg.Repo = values["REPO"]
	}
	if values["APPROVER"] != "" {
		cfg.Approver = values["APPROVER"]
	}
	if values["LEDGER_BUCKET"] != "" {
		cfg.LedgerBucket = values["LEDGER_BUCKET"]
	}
	if values["LEDGER_APPLIED_PREFIX"] != "" {
		cfg.LedgerAppliedPrefix = values["LEDGER_APPLIED_PREFIX"]
	}
	if values["LEDGER_FAILED_PREFIX"] != "" {
		cfg.LedgerFailedPrefix = values["LEDGER_FAILED_PREFIX"]
	}
	if values["LEDGER_HEAD_KEY"] != "" {
		cfg.LedgerHeadKey = values["LEDGER_HEAD_KEY"]
	}
	if values["HEARTBEAT_KEY"] != "" {
		cfg.HeartbeatKey = values["HEARTBEAT_KEY"]
	}
	if values["PLAN_DIGEST_PREFIX"] != "" {
		cfg.PlanDigestPrefix = values["PLAN_DIGEST_PREFIX"]
	}
	if values["WORKDIR"] != "" {
		cfg.Workdir = values["WORKDIR"]
	}
	if values["OP_TOKEN_FILE"] != "" {
		cfg.OPTokenFile = values["OP_TOKEN_FILE"]
	}

	// Load optional variables with defaults
	if secretsDir := getenv("SECRETS_DIR"); secretsDir != "" {
		cfg.SecretsDir = secretsDir
	} else {
		cfg.SecretsDir = "/secrets"
	}

	if pluginDir := getenv("TF_PLUGIN_DIR"); pluginDir != "" {
		cfg.PluginDir = pluginDir
	} else {
		cfg.PluginDir = "/opt/tofu-providers"
	}

	if expiryWarnDays := getenv("EXPIRY_WARN_DAYS"); expiryWarnDays != "" {
		if days, err := strconv.Atoi(expiryWarnDays); err == nil {
			cfg.ExpiryWarnDays = days
		} else {
			problems = append(problems, fmt.Sprintf("refusing to start: EXPIRY_WARN_DAYS is not a valid integer: %v", err))
		}
	} else {
		cfg.ExpiryWarnDays = 30
	}

	// HEARTBEAT_PING_URL is optional with no default: absent or empty means
	// the dead-man's-switch feature is off, full stop. Never validated or
	// echoed back into a problem string -- it is a secret, and a rejected
	// value would otherwise print it into a log line the moment it is wrong.
	if pingURL := getenv("HEARTBEAT_PING_URL"); pingURL != "" {
		cfg.HeartbeatPingURL = pingURL
	}

	// METRICS_PUSH_URL is optional with no default, and never validated or
	// echoed back into a problem string, for the same reasons
	// HEARTBEAT_PING_URL above is not: absent means the feature is off, and
	// a rejected value would print a possibly-credentialled URL into a log
	// line the moment somebody got it wrong.
	if pushURL := getenv("METRICS_PUSH_URL"); pushURL != "" {
		cfg.MetricsPushURL = pushURL
	}

	if v := getenv("DELIVERY_BYPASS_ACTOR_ID"); v != "" {
		id, err := strconv.Atoi(v)
		if err != nil || id <= 0 {
			problems = append(problems, fmt.Sprintf(
				"refusing to start: DELIVERY_BYPASS_ACTOR_ID must be a positive GitHub App id, not %q", v))
		} else {
			cfg.DeliveryBypassActorID = id
		}
	}

	// Load DRIFT_CHECK - accepts only 0, 1, or unset
	if driftCheck := getenv("DRIFT_CHECK"); driftCheck != "" {
		switch driftCheck {
		case "0":
			cfg.DriftOnly = false
		case "1":
			cfg.DriftOnly = true
		default:
			problems = append(problems, fmt.Sprintf("refusing to start: DRIFT_CHECK accepts only 0, 1, or unset, not %q", driftCheck))
		}
	}

	return cfg, problems
}
