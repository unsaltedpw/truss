package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/beeradb/truss/internal/config"
	"github.com/beeradb/truss/internal/handoff"
	"github.com/beeradb/truss/internal/ledger"
	"github.com/beeradb/truss/internal/notify"
	"github.com/beeradb/truss/internal/secrets"
)

// seedRootLockfiles gives the fixture a WORKING TREE, which it did not have:
// Workdir was "" and no root existed on disk, so any code reading a checked-out
// file was untestable here and, worse, looked fine.
//
// ⚠️ THE FIXTURE MUST MATCH PRODUCTION, and the contents matter rather than
// merely the file existing. `platform/` genuinely declares only the github
// provider -- verified against the real repo's committed lockfile -- and that
// is exactly why it must not be handed a Cloudflare token. A fixture that gave
// every root an identical lockfile would let the gate pass while doing nothing.
func seedRootLockfiles(t *testing.T, workdir string) {
	t.Helper()
	const cloudflare = "provider \"registry.opentofu.org/cloudflare/cloudflare\" {\n  version = \"5.0.0\"\n}\n"
	const github = "provider \"registry.opentofu.org/integrations/github\" {\n  version = \"6.0.0\"\n}\n"
	for root, body := range map[string]string{
		"projects/recipes": cloudflare,
		"projects/other":   cloudflare,
		"credentials":      cloudflare + github,
		"platform":         github, // no Cloudflare, on purpose -- see above
	} {
		dir := filepath.Join(workdir, root)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("seeding %s: %v", root, err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".terraform.lock.hcl"), []byte(body), 0o644); err != nil {
			t.Fatalf("seeding %s lockfile: %v", root, err)
		}
	}
}

// buildTestDeps assembles applyDeps against a fake ledger (real
// ledger.Journal/Store over httptest), a fake Vault (real secrets.KV over
// httptest, empty mount), a fake Telegram reached through a redirecting
// HTTP client, and whatever gitDriver/forgeGateway/tofuFactory the caller
// supplies. It returns the deps plus the fake ledger and fake Telegram so
// a test can inspect what was written and sent.
//
// Stderr defaults to io.Discard -- most tests here do not care about
// narration (logf, apply_cmd.go) and a nil Writer would panic the first time
// any of the nine call sites fired. A test that DOES want to inspect
// narration overrides deps.Stderr with its own *bytes.Buffer after this
// returns; see apply_narration_test.go.
func buildTestDeps(t *testing.T, forgeFake *fakeForge, git gitDriver, newTofu tofuFactory) (applyDeps, *fakeLedger, *fakeTelegram) {
	t.Helper()

	dir, write := testSecretsDir(t)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	// ⚠️ THE GITHUB APP ITEM IS PART OF THE MIRROR AND THIS FIXTURE OMITTED
	// IT. Every tofu run gets GITHUB_APP_ID/INSTALLATION_ID/PEM_FILE, which
	// the `github` provider's app_auth block requires; leaving them out of
	// the fixture is what let buildBaseEnv ship without them at all. Found
	// by the second shadow run against the real cluster, 2026-09-08.
	writeGitHubAppSecret(t, write)
	write(itemCFTokenMint, fieldCFCredential, "fake-mint-token")
	write(itemGCPApply, fieldGCPCredentials, "fake-google-creds")
	write(itemTofuEncryption, fieldTofuPassphrase, "fake-passphrase")
	write(itemCFInfraAdmin, fieldCFPassword, "fake-infra-tok")

	vaultSrv := productionShapedVault(t, "platform")

	store, err := ledger.New(ledger.Config{
		Endpoint:        fl.endpoint(),
		Bucket:          "state-bucket",
		AccessKeyID:     "AKIAFAKEACCESSKEYID",
		SecretAccessKey: "fakesecretaccesskeyfakesecretaccesskey",
	})
	if err != nil {
		t.Fatalf("ledger.New: %v", err)
	}
	journal := &ledger.Journal{Store: store, Layout: ledger.Layout{
		AppliedPrefix: "applied", FailedPrefix: "failed",
		HeadKey: "head", HeartbeatKey: "heartbeat", PlanDigestPrefix: "digests",
	}}

	ft := newFakeTelegram(t)

	workdir := t.TempDir()
	seedRootLockfiles(t, workdir)
	cfg := testConfig()
	cfg.Workdir = workdir

	deps := applyDeps{
		Cfg:     cfg,
		Dir:     dir,
		Journal: journal,
		Forge:   forgeFake,
		Telegram: notify.Telegram{
			BotToken: "fake-bot-token",
			ChatID:   "-100200300",
			HTTP:     redirectingHTTPClient(ft.srv.URL),
		},
		Git:     git,
		NewTofu: newTofu,
		Now:     func() time.Time { return time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC) },
		VaultConfig: secrets.KVConfig{
			Addr: vaultSrv.URL, Mount: "platform", Role: "applier", JWTPath: testJWTFile(t),
		},
		PATH: "/usr/bin", HOME: "/root",
		Stderr: io.Discard,
		// A default that answers "nothing to publish" without a real
		// socket -- most tests here do not care about the handoff, and a
		// nil Handoff func would panic the first of them that runs a
		// non-drift pass, which is nearly all of them (DriftOnly defaults
		// false). A test that DOES care overrides deps.Handoff and
		// deps.HandoffSocket with its own fakeHandoff after this returns,
		// the same way apply_daily_pass_test.go overrides deps.Cfg.DriftOnly.
		HandoffSocket: "unused-in-tests",
		Handoff: func(ctx context.Context, path string, timeout time.Duration, r handoff.Request) (handoff.Response, error) {
			return handoff.Response{Value: handoff.ValueSkipped}, nil
		},
	}
	return deps, fl, ft
}

// testConfig is a minimal config.Config, built by hand rather than through
// config.Load -- these tests exercise runApplyPass directly, below the
// layer that reads the environment.
func testConfig() config.Config {
	return config.Config{
		Repo:                "acme/platform",
		Approver:            "alice",
		LedgerBucket:        "state-bucket",
		LedgerAppliedPrefix: "applied",
		LedgerFailedPrefix:  "failed",
		LedgerHeadKey:       "head",
		HeartbeatKey:        "heartbeat",
		PlanDigestPrefix:    "digests",
		Workdir:             "",
		RequiredCheck:       "plan",
		ExpiryWarnDays:      30,
	}
}

// TestApplyAlwaysWritesAHeartbeatAndAlerts drives a pass that fails at the
// branch-protection gate -- the simplest failure this binary can produce,
// needing no git or tofu activity at all -- and checks that a heartbeat
// still lands in the ledger and a message still reaches Telegram (§2 item
// 8): "the pass always writes a heartbeat and always sends a message...
// and exits 1 if and only if failure is non-empty."
func TestApplyAlwaysWritesAHeartbeatAndAlerts(t *testing.T) {
	forgeFake := &fakeForge{
		// Zero-value gates.Protection: every pointer nil, which
		// CheckProtection refuses (absent is not compliant).
	}
	git := &fakeGit{HasDirFn: func(string) bool { return false }}
	newTofu := func(env []string) tofuRunner { return &fakeTofu{} }

	deps, fl, ft := buildTestDeps(t, forgeFake, git, newTofu)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, "headsha1")

	if result.failure == "" {
		t.Fatalf("result.failure is empty, want the branch-protection refusal")
	}

	hbBytes, ok := fl.get("heartbeat")
	if !ok {
		t.Fatalf("no heartbeat object was written to the ledger")
	}
	var hb ledger.Heartbeat
	if err := json.Unmarshal(hbBytes, &hb); err != nil {
		t.Fatalf("heartbeat did not parse as JSON: %v\n%s", err, hbBytes)
	}
	if hb.Failure == nil || *hb.Failure == "" {
		t.Fatalf("heartbeat.Failure = %v, want the refusal reason", hb.Failure)
	}
	if hb.Time == "" {
		t.Fatalf("heartbeat.Time is empty")
	}

	text := ft.lastText()
	if text == "" {
		t.Fatalf("no message reached the fake Telegram server")
	}
	if !strings.Contains(text, "FAILED") {
		t.Fatalf("telegram text = %q, want it to lead with FAILED", text)
	}
}
