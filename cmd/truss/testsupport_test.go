package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beeradb/truss/internal/gates"
	"github.com/beeradb/truss/internal/handoff"
	"github.com/beeradb/truss/internal/secrets"
)

// --- fake publisher handoff -------------------------------------------------

// fakeHandoff stands in for internal/handoff.Send, letting apply-pass
// tests control the publisher's verdict (or its absence, via Err) without a
// real Unix socket or a real publisher process. It records every request it
// was given, in order -- the evidence
// TestEveryPassPathContactsThePublisherExactlyOnce needs to prove the send
// actually happened, rather than merely compiling.
type fakeHandoff struct {
	mu    sync.Mutex
	calls []handoff.Request

	// Resp and Err are returned verbatim from every call; Err simulates no
	// publisher answering (a dial failure), which internal/handoff.Send
	// itself returns rather than a zero Response.
	Resp handoff.Response
	Err  error
}

func (f *fakeHandoff) send(ctx context.Context, path string, timeout time.Duration, r handoff.Request) (handoff.Response, error) {
	f.mu.Lock()
	f.calls = append(f.calls, r)
	f.mu.Unlock()
	if f.Err != nil {
		return handoff.Response{}, f.Err
	}
	return f.Resp, nil
}

// callCount is how many times send was called.
func (f *fakeHandoff) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// lastRequest is the most recent request send was given, the zero Request
// if it was never called.
func (f *fakeHandoff) lastRequest() handoff.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return handoff.Request{}
	}
	return f.calls[len(f.calls)-1]
}

// --- in-process fake forgeGateway ------------------------------------------

// fakeForge implements forgeGateway directly, in memory, with no HTTP
// involved -- appropriate for apply-pass tests, which exist to check the
// PASS's own orchestration (the gate, the loop, rotation, the heartbeat),
// not the forge client's wire behaviour, which internal/forge's own tests
// already cover. Every *Err field, left nil, means "succeed with the
// paired result".
type fakeForge struct {
	ProtectionResult gates.Protection
	ProtectionErr    error

	// RulesetsResult, left at its zero value (an empty Applicable), answers
	// "no rulesets apply to this branch" -- itself compliant, since
	// CheckRulesets has nothing to refuse on an empty list. That is what
	// lets every existing fixture in this file that only sets
	// ProtectionResult keep passing: it was never making a claim about
	// rulesets, and a real repository with none is not a hole.
	RulesetsResult gates.Rulesets

	// RulesetsByBranch answers per ref where a test needs main and the
	// delivery ref to differ, which is most tests that touch delivery: the
	// applier asks about both in one pass and they are protected by
	// different rulesets in any real deployment.
	RulesetsByBranch map[string]gates.Rulesets
	RulesetsErr      error

	Token    string
	TokenErr error

	// PRNumbers is keyed by commit sha.
	PRNumbers    map[string][]int
	PRNumbersErr error

	// PR and ReviewsResult are keyed by PR number.
	PR            map[int]gates.PullRequest
	PRErr         error
	ReviewsResult map[int][]gates.Review
	ReviewsErr    error

	// CommitResult is keyed by commit sha.
	CommitResult map[string]gates.Commit
	CommitErr    error
}

func (f *fakeForge) Protection(ctx context.Context, branch string) (gates.Protection, error) {
	return f.ProtectionResult, f.ProtectionErr
}

func (f *fakeForge) Rulesets(ctx context.Context, branch string) (gates.Rulesets, error) {
	if rs, ok := f.RulesetsByBranch[branch]; ok {
		return rs, f.RulesetsErr
	}
	return f.RulesetsResult, f.RulesetsErr
}

func (f *fakeForge) InstallationToken(ctx context.Context) (string, time.Time, error) {
	tok := f.Token
	if tok == "" && f.TokenErr == nil {
		tok = "fake-inst-tok"
	}
	return tok, time.Now().Add(time.Hour), f.TokenErr
}

func (f *fakeForge) PullNumbersForCommit(ctx context.Context, sha string) ([]int, error) {
	if f.PRNumbersErr != nil {
		return nil, f.PRNumbersErr
	}
	return f.PRNumbers[sha], nil
}

func (f *fakeForge) PullRequest(ctx context.Context, number int) (gates.PullRequest, error) {
	if f.PRErr != nil {
		return gates.PullRequest{}, f.PRErr
	}
	return f.PR[number], nil
}

func (f *fakeForge) Reviews(ctx context.Context, number int) ([]gates.Review, error) {
	if f.ReviewsErr != nil {
		return nil, f.ReviewsErr
	}
	return f.ReviewsResult[number], nil
}

func (f *fakeForge) Commit(ctx context.Context, sha string) (gates.Commit, error) {
	if f.CommitErr != nil {
		return gates.Commit{}, f.CommitErr
	}
	return f.CommitResult[sha], nil
}

// compliantGatesProtection is a gates.Protection that clears
// CheckProtection's bar for requiredCheck "plan" -- the in-process
// equivalent of compliantProtection() above, for tests that build a
// fakeForge directly instead of a fake HTTP server.
func compliantGatesProtection() gates.Protection {
	one := 1
	yes := true
	no := false
	return gates.Protection{
		RequiredApprovals:     &one,
		RequireCodeOwners:     &yes,
		DismissStaleReviews:   &yes,
		EnforceAdmins:         &yes,
		AllowForcePushes:      &no,
		RequireUpToDateBranch: &yes,
		StatusChecks:          []string{"plan"},

		// ⚠️ EVERY FIELD CheckProtection READS MUST BE SET HERE, and adding
		// one there without adding it here turns every cmd/truss test that
		// expects a pass into "branch protection does not meet the bar" --
		// a failure that names the gate, not the test's own subject, so it
		// reads like an unrelated regression. That is the mirror of the
		// warning on gates.Protection itself.
		//
		// BypassPullRequestAllowances is deliberately left nil: nil is the
		// compliant value there (GitHub omits the key when nobody may
		// bypass), so a compliant fixture is one that does NOT set it.
		AllowDeletions:          &no,
		RequireLastPushApproval: &yes,
	}
}

// compliantCommitGate returns a fakeForge already wired so
// checkCommitGate(ctx, f, approver, sha) passes: one merged PR (number 1),
// approved by approver at headSHA, merged by a verified web-flow commit.
func compliantCommitGate(approver, sha, headSHA string) *fakeForge {
	yes := true
	webFlow := "web-flow"
	return &fakeForge{
		ProtectionResult: compliantGatesProtection(),
		PRNumbers:        map[string][]int{sha: {1}},
		PR: map[int]gates.PullRequest{
			1: {Number: 1, Merged: true, MergeCommitSHA: sha, HeadSHA: headSHA},
		},
		ReviewsResult: map[int][]gates.Review{
			1: {{User: approver, State: "APPROVED", CommitID: headSHA}},
		},
		CommitResult: map[string]gates.Commit{
			sha: {SHA: sha, Verified: &yes, CommitterLogin: &webFlow},
		},
	}
}

// --- secrets.Dir fixture -----------------------------------------------

// testSecretsDir builds a fresh temp-dir-backed secrets.Dir and returns it
// alongside a writer for planting item/field files, the same shape the
// vault-secrets init container renders in production.
func testSecretsDir(t *testing.T) (secrets.Dir, func(item, field, value string)) {
	t.Helper()
	root := t.TempDir()
	write := func(item, field, value string) {
		t.Helper()
		dir := filepath.Join(root, item)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, field), []byte(value), 0o600); err != nil {
			t.Fatalf("write %s/%s: %v", item, field, err)
		}
	}
	return secrets.Dir{Root: root}, write
}

// testRSAKeyPEM generates a fresh RSA key at test time (never a fixture on
// disk) and returns its PEM encoding, matching forge's own test convention
// so leakscan never sees a real-looking key.
func testRSAKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating test key: %v", err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}
	return string(pem.EncodeToMemory(block))
}

// writeGitHubAppSecret plants a github-app item with a freshly generated
// key, returning the key's PEM so a test can also sign fixtures with it if
// it ever needs to.
func writeGitHubAppSecret(t *testing.T, write func(item, field, value string)) {
	t.Helper()
	write(itemGitHubApp, fieldGitHubAppID, "123456")
	write(itemGitHubApp, fieldGitHubInstallationID, "7654321")
	write(itemGitHubApp, fieldGitHubPrivateKey, testRSAKeyPEM(t))
}

// --- fake ledger (S3-compatible) server ----------------------------------

// fakeLedger is a minimal in-memory stand-in for the state bucket: GET and
// PUT only, path-style (bucket as the first path segment), a 404/NoSuchKey
// body for a missing key -- enough for ledger.Store to work against
// without a real bucket.
type fakeLedger struct {
	mu      sync.Mutex
	bucket  string
	objects map[string][]byte
	srv     *httptest.Server

	// failPutKeys names keys whose PUT answers 500 instead of succeeding --
	// for a test that needs to observe WRITE ORDER (skip_cmd_test.go's
	// TestSkipWritesTheRecordBeforeAdvancingHead: PutSkipped must land even
	// when the following AdvanceHead's PUT to the HEAD key fails). Nil
	// means every PUT succeeds, matching every other test in this package.
	failPutKeys map[string]bool

	// putRequests counts every PUT that arrived over HTTP -- i.e. through a
	// real ledger.Store call, never through the .put() seeding helper below,
	// which writes f.objects directly and does not touch this handler at
	// all. why_cmd_test.go's read-only guarantee asserts this stays zero
	// across a full `truss why` run: the one thing a command meant to be
	// safe to point at a stuck applier must never do is write to the ledger
	// it is inspecting.
	putRequests int
}

// puts reports how many PUT requests this fake ledger has actually
// received over HTTP.
func (f *fakeLedger) puts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.putRequests
}

func newFakeLedger(t *testing.T, bucket string) *fakeLedger {
	t.Helper()
	f := &fakeLedger{bucket: bucket, objects: make(map[string][]byte)}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLedger) endpoint() string { return f.srv.URL }

func (f *fakeLedger) get(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objects[key]
	return b, ok
}

// put seeds an object directly, without going through a signed request --
// for a test that needs something already IN the bucket, such as the plan
// digest CI would have filed.
func (f *fakeLedger) put(key string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = append([]byte(nil), body...)
}

// failPutOn makes every future PUT to key answer 500 instead of writing,
// leaving any object already there untouched -- see failPutKeys.
func (f *fakeLedger) failPutOn(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPutKeys == nil {
		f.failPutKeys = make(map[string]bool)
	}
	f.failPutKeys[key] = true
}

func (f *fakeLedger) handle(w http.ResponseWriter, r *http.Request) {
	prefix := "/" + f.bucket + "/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, prefix)

	switch r.Method {
	case http.MethodGet:
		f.mu.Lock()
		body, ok := f.objects[key]
		f.mu.Unlock()
		if !ok {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			xml.NewEncoder(w).Encode(struct {
				XMLName xml.Name `xml:"Error"`
				Code    string   `xml:"Code"`
				Message string   `xml:"Message"`
			}{Code: "NoSuchKey", Message: "The specified key does not exist."})
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	case http.MethodPut:
		f.mu.Lock()
		shouldFail := f.failPutKeys[key]
		f.putRequests++
		f.mu.Unlock()
		if shouldFail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.objects[key] = append([]byte(nil), body...)
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// writeLedgerSecret plants the gcs-ledger item pointing at a fake ledger
// server.
func writeLedgerSecret(t *testing.T, write func(item, field, value string), fl *fakeLedger) {
	t.Helper()
	write(itemLedger, fieldLedgerEndpoint, fl.endpoint())
	write(itemLedger, fieldLedgerAccessKey, "AKIAFAKEACCESSKEYID")
	write(itemLedger, fieldLedgerSecretKey, "fakesecretaccesskeyfakesecretaccesskey")
}

// --- fake GitHub (forge) server -------------------------------------------

// fakeForgeScenario configures a fake GitHub API server that answers every
// endpoint forge.Client calls, defaulting to a fully-compliant pass: one
// merged PR, approved by Approver at the head sha, merged by a verified
// web-flow commit, and branch protection that clears CheckProtection's bar.
type fakeForgeScenario struct {
	Approver string

	Protection map[string]any // nil uses a compliant default

	PRNumbers []int // pull numbers for commits/<sha>/pulls; defaults to [1]

	PRMerged         bool
	PRMergeCommitSHA string // defaults to the sha the test asks about
	PRHeadSHA        string

	ReviewState    string // defaults "APPROVED"
	ReviewUser     string // defaults Approver
	ReviewCommitID string // defaults PRHeadSHA

	CommitVerified  bool
	CommitCommitter string // defaults "web-flow"

	MintToken string // defaults "fake-inst-tok"
}

func newFakeForge(t *testing.T, sha string, s fakeForgeScenario) *httptest.Server {
	t.Helper()
	if s.PRNumbers == nil {
		s.PRNumbers = []int{1}
	}
	if s.PRMergeCommitSHA == "" {
		s.PRMergeCommitSHA = sha
	}
	if s.PRHeadSHA == "" {
		s.PRHeadSHA = sha
	}
	if s.ReviewState == "" {
		s.ReviewState = "APPROVED"
	}
	if s.ReviewUser == "" {
		s.ReviewUser = s.Approver
	}
	if s.ReviewCommitID == "" {
		s.ReviewCommitID = s.PRHeadSHA
	}
	if s.CommitCommitter == "" {
		s.CommitCommitter = "web-flow"
	}
	if s.MintToken == "" {
		s.MintToken = "fake-inst-tok"
	}
	if s.Protection == nil {
		s.Protection = compliantProtection()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"token":      s.MintToken,
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(p, "/branches/main/protection"):
			json.NewEncoder(w).Encode(s.Protection)
		case strings.HasSuffix(p, "/rules/branches/main"):
			// No scenario here sets up a ruleset, so this always answers "no
			// rulesets apply" -- CheckRulesets has nothing to refuse on that,
			// matching the zero-value RulesetsResult the in-process fakeForge
			// above defaults to.
			_, _ = w.Write([]byte(`[]`))
		case strings.Contains(p, "/commits/") && strings.HasSuffix(p, "/pulls"):
			items := make([]map[string]any, len(s.PRNumbers))
			for i, n := range s.PRNumbers {
				items[i] = map[string]any{"number": n}
			}
			json.NewEncoder(w).Encode(items)
		case strings.Contains(p, "/pulls/") && strings.HasSuffix(p, "/reviews"):
			json.NewEncoder(w).Encode([]map[string]any{
				{
					"user":         map[string]any{"login": s.ReviewUser},
					"state":        s.ReviewState,
					"commit_id":    s.ReviewCommitID,
					"submitted_at": time.Now().UTC().Format(time.RFC3339),
				},
			})
		case strings.Contains(p, "/pulls/"):
			json.NewEncoder(w).Encode(map[string]any{
				"number":           s.PRNumbers[0],
				"merged":           s.PRMerged,
				"merge_commit_sha": s.PRMergeCommitSHA,
				"head":             map[string]any{"sha": s.PRHeadSHA},
			})
		case strings.Contains(p, "/commits/"):
			json.NewEncoder(w).Encode(map[string]any{
				"sha": sha,
				"commit": map[string]any{
					"verification": map[string]any{"verified": s.CommitVerified},
				},
				"committer": map[string]any{"login": s.CommitCommitter},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func compliantProtection() map[string]any {
	return map[string]any{
		"required_pull_request_reviews": map[string]any{
			"required_approving_review_count": 1,
			"require_code_owner_reviews":      true,
			"dismiss_stale_reviews":           true,
			"require_last_push_approval":      true,
		},
		"enforce_admins":     map[string]any{"enabled": true},
		"allow_force_pushes": map[string]any{"enabled": false},
		"allow_deletions":    map[string]any{"enabled": false},
		"required_status_checks": map[string]any{
			"strict":   true,
			"contexts": []string{"plan"},
		},
	}
}

// --- fake Vault (KV v2) server --------------------------------------------

// newFakeVault answers a Kubernetes-auth login and a metadata LIST holding
// `items`, each with the custom_metadata given (nil for "no expires
// recorded", which is what production looks like today).
//
// ⚠️ THIS USED TO ANSWER THE LIST WITH A BARE 404 -- "zero items" -- AND
// THAT IS WHY THE WHOLE SUITE WAS GREEN OVER A DEFECT THAT WOULD HAVE FIRED
// ON EVERY PRODUCTION RUN. With zero items the sweep's `eligible > 0` alarm
// is unreachable, so "clean pass exits 0" was only ever true of a vault
// shaped unlike the real one. Found 2026-09-08 by the code audit, and it is
// the repo's own rule: a fixture more forgiving than production invents
// failures, one stricter hides them -- here, on a security alert path.
func newFakeVault(t *testing.T, mount string, items map[string]map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/kubernetes/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"auth": map[string]any{"client_token": "fake-vault-tok"},
		})
	})
	mux.HandleFunc("/v1/"+mount+"/metadata", func(w http.ResponseWriter, r *http.Request) {
		if len(items) == 0 {
			// Vault's own way of saying "zero items".
			w.WriteHeader(http.StatusNotFound)
			return
		}
		keys := make([]string, 0, len(items))
		for k := range items {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"keys": keys},
		})
	})
	mux.HandleFunc("/v1/"+mount+"/metadata/", func(w http.ResponseWriter, r *http.Request) {
		item := strings.TrimPrefix(r.URL.Path, "/v1/"+mount+"/metadata/")
		cm, ok := items[item]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		custom := map[string]any{}
		for k, v := range cm {
			custom[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"custom_metadata": custom},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// productionShapedVault is what the platform mount looks like TODAY: real
// items, none of them carrying an `expires`. Tests that drive a whole pass
// use this, so the sweep's alarm is reachable rather than designed away.
func productionShapedVault(t *testing.T, mount string) *httptest.Server {
	t.Helper()
	return newFakeVault(t, mount, map[string]map[string]string{
		"gcs-ledger":    nil,
		"github-app":    nil,
		"cf-token-mint": nil,
	})
}

// testJWTFile writes a throwaway JWT-shaped file for secrets.KVConfig's
// JWTPath -- its content is never a real credential, only a path secrets.KV
// reads at login time against the fake server above.
func testJWTFile(t *testing.T) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(f, []byte("fake.jwt.token"), 0o600); err != nil {
		t.Fatalf("writing fake jwt: %v", err)
	}
	return f
}

// --- fake Telegram server -------------------------------------------------

type fakeTelegram struct {
	mu   sync.Mutex
	last string
	srv  *httptest.Server
}

func newFakeTelegram(t *testing.T) *fakeTelegram {
	t.Helper()
	ft := &fakeTelegram{}
	ft.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		ft.mu.Lock()
		ft.last = r.Form.Get("text")
		ft.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ft.srv.Close)
	return ft
}

func (ft *fakeTelegram) lastText() string {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return ft.last
}

// redirectingHTTPClient returns an *http.Client that sends every request to
// targetBaseURL regardless of the URL the caller built, keeping the
// original path and method -- notify.Telegram has no base-URL override
// (its endpoint is a literal api.telegram.org format string), so this is
// the seam that lets a test point Telegram.Send at a fake server instead.
func redirectingHTTPClient(targetBaseURL string) *http.Client {
	target, err := url.Parse(targetBaseURL)
	if err != nil {
		panic(err)
	}
	return &http.Client{Transport: redirectTransport{target: target}}
}

type redirectTransport struct{ target *url.URL }

func (t redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = t.target.Scheme
	clone.URL.Host = t.target.Host
	clone.Host = t.target.Host
	return http.DefaultTransport.RoundTrip(clone)
}

// writeTelegramSecret plants the telegram-alert item. Send still targets
// the real api.telegram.org host (notify.Telegram has no base-URL override
// for tests), so tests exercising notify.Telegram.Send point BotToken at a
// value that will fail fast against the real host in a network-denied
// sandbox -- notify.Telegram.Send already treats that as a non-fatal,
// redacted error (see notify's own tests), which is exactly the behaviour
// this binary relies on.
func writeTelegramSecret(t *testing.T, write func(item, field, value string), token string) {
	t.Helper()
	write(itemTelegram, fieldTelegramBotToken, token)
	write(itemTelegram, fieldTelegramChatID, "-100200300")
}

// --- full environment fixture ---------------------------------------------

// testFullEnv returns a getenv function carrying every variable
// config.Load and loadVaultConfig require, all pointed at harmless
// defaults, with overrides applied last. secretsDir should be a
// testSecretsDir's Root.
//
// ⚠️ HANDOFF_SOCKET IS DELIBERATELY ABSENT. loadHandoffConfig requires it on
// the DRIFT pass and REFUSES it on a frequent one, because rotation -- and so
// anything to publish -- happens only under DRIFT_CHECK=1. This fixture sets
// no DRIFT_CHECK, so it is a frequent pass, so setting the socket here would
// make every test built on it refuse to start.
//
// An earlier version set it and explained at length why that was required,
// which was true of the inverted wiring it was written against. A fixture that
// disagrees with production is a bug this project has already paid a day for,
// so the comment goes with the value. A drift test that needs the socket sets
// DRIFT_CHECK and HANDOFF_SOCKET together through the overrides argument.
func testFullEnv(secretsDir, workdir string, overrides map[string]string) func(string) string {
	base := map[string]string{
		"REPO":                  "acme/platform",
		"APPROVER":              "alice",
		"LEDGER_BUCKET":         "state-bucket",
		"LEDGER_APPLIED_PREFIX": "applied",
		"LEDGER_FAILED_PREFIX":  "failed",
		"LEDGER_HEAD_KEY":       "head",
		"HEARTBEAT_KEY":         "heartbeat",
		"PLAN_DIGEST_PREFIX":    "digests",
		"WORKDIR":               workdir,
		"OP_TOKEN_FILE":         mustWriteOPTokenFile(),
		"SECRETS_DIR":           secretsDir,
		"VAULT_ADDR":            "http://vault.invalid", // overridden per-test
		"VAULT_ROLE":            "applier",
		"VAULT_JWT_PATH":        "/nonexistent",
		"VAULT_MOUNT":           "platform",
	}
	for k, v := range overrides {
		base[k] = v
	}
	return func(name string) string { return base[name] }
}

// withForgeBaseURL wraps a getenv function, overriding GITHUB_API_BASE_URL
// -- the one optional variable loadForgeConfig reads so a test can point
// the forge client at a fake server instead of the real GitHub API.
func withForgeBaseURL(env func(string) string, baseURL string) func(string) string {
	return func(name string) string {
		if name == "GITHUB_API_BASE_URL" {
			return baseURL
		}
		return env(name)
	}
}

// withVaultAddr wraps a getenv function, overriding VAULT_ADDR.
func withVaultAddr(env func(string) string, addr string) func(string) string {
	return func(name string) string {
		if name == "VAULT_ADDR" {
			return addr
		}
		return env(name)
	}
}

var opTokenFileOnce sync.Once
var opTokenFilePath string

func mustWriteOPTokenFile() string {
	opTokenFileOnce.Do(func() {
		f, err := os.CreateTemp("", "op-token")
		if err != nil {
			panic(err)
		}
		defer f.Close()
		f.WriteString("fake-op-token")
		opTokenFilePath = f.Name()
	})
	return opTokenFilePath
}

// --- fake gitDriver --------------------------------------------------------

// fakeGit is an in-memory gitDriver for exercising the apply pass without a
// real git binary or network. Each field, where set, is returned verbatim
// or via the given function; the zero value behaves like an empty repo
// with no history.
type fakeGit struct {
	CommitsList       []string
	ChangedByCommit   map[string][]string
	TreeRootsByCommit map[string][]string

	// TreeTofuUnitsByCommit is the credentials/tofu-unit listing runCommitLoop
	// feeds repo.TouchedUnits from, in place of TreeRootsByCommit -- separate
	// because TreeRoots reproduces the bash's exact ls-tree and must not
	// widen. Most fixtures leave this nil (an empty tree), which is correct
	// whenever the scenario names its unit directly in ChangedByCommit
	// rather than relying on the shared-input branch.
	TreeTofuUnitsByCommit map[string][]string

	HasDirFn    func(root string) bool
	CheckoutErr error
	CommitsErr  error

	// DirsAtRef makes HasDir depend on WHICH TREE IS CHECKED OUT, which the
	// real one does and this fake did not. Without it no test could see a
	// HasDir asked before its Checkout -- the 2026-09-08 audit's finding
	// about runRotation. Keyed by ref; the empty-string key is the tree
	// before any checkout.
	DirsAtRef map[string][]string

	mu        sync.Mutex
	checkouts []string
	current   string
	token     string
	clones    int
	fetches   int
}

// WithToken records the token so a test can assert every git call carries
// one -- the 2026-09-08 audit's finding was precisely that they did not.
func (g *fakeGit) WithToken(token string) gitDriver {
	g.mu.Lock()
	g.token = token
	g.mu.Unlock()
	return g
}

// tokenSeen is the token WithToken was last given, empty if never called.
func (g *fakeGit) tokenSeen() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.token
}

func (g *fakeGit) EnsureClone(ctx context.Context, repoURL string) error {
	g.mu.Lock()
	g.clones++
	g.mu.Unlock()
	return nil
}

func (g *fakeGit) Fetch(ctx context.Context, remote, branch string) error {
	g.mu.Lock()
	g.fetches++
	g.mu.Unlock()
	return nil
}

// cloned and fetched are the only thing a fake CAN observe about the clone:
// a fake filesystem succeeds whether or not one happened, which is exactly
// why the missing drift-run clone reached production.
func (g *fakeGit) cloned() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.clones > 0
}

func (g *fakeGit) fetched() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fetches > 0
}

func (g *fakeGit) Commits(ctx context.Context, from, to string) ([]string, error) {
	if g.CommitsErr != nil {
		return nil, g.CommitsErr
	}
	return g.CommitsList, nil
}

func (g *fakeGit) ChangedFiles(ctx context.Context, sha string) ([]string, error) {
	return g.ChangedByCommit[sha], nil
}

func (g *fakeGit) TreeRoots(ctx context.Context, sha string) ([]string, error) {
	return g.TreeRootsByCommit[sha], nil
}

func (g *fakeGit) TreeTofuUnits(ctx context.Context, sha string) ([]string, error) {
	return g.TreeTofuUnitsByCommit[sha], nil
}

func (g *fakeGit) Checkout(ctx context.Context, ref string) error {
	if g.CheckoutErr != nil {
		return g.CheckoutErr
	}
	g.mu.Lock()
	g.checkouts = append(g.checkouts, ref)
	g.current = ref
	g.mu.Unlock()
	return nil
}

// checkedOut reports whether ref was ever checked out.
func (g *fakeGit) checkedOut(ref string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, c := range g.checkouts {
		if c == ref {
			return true
		}
	}
	return false
}

func (g *fakeGit) HasDir(root string) bool {
	if g.DirsAtRef != nil {
		g.mu.Lock()
		ref := g.current
		g.mu.Unlock()
		for _, d := range g.DirsAtRef[ref] {
			if d == root {
				return true
			}
		}
		return false
	}
	if g.HasDirFn != nil {
		return g.HasDirFn(root)
	}
	return true
}

// --- fake tofuRunner --------------------------------------------------------

// fakeTofu is an in-memory tofuRunner. planJSON is returned by ShowJSON;
// errors and PlanDetailed's result are configurable per call via the
// function fields, defaulting to a clean, no-op success.
type fakeTofu struct {
	InitErr, PlanErr, ApplyErr error
	ShowJSONBytes              []byte
	ShowJSONErr                error
	PlanDetailedChanged        bool
	PlanDetailedErr            error

	mu      sync.Mutex
	applies []string
}

func (f *fakeTofu) Init(ctx context.Context, dir string) error          { return f.InitErr }
func (f *fakeTofu) Plan(ctx context.Context, dir, outFile string) error { return f.PlanErr }

// Apply records that it was reached. Whether Apply ran AT ALL is the
// property the digest gate exists to control, so a test asserting a refusal
// has to be able to see it -- "the pass failed" is also true of a pass that
// applied and then failed afterwards.
func (f *fakeTofu) Apply(ctx context.Context, dir, planFile string) error {
	f.mu.Lock()
	f.applies = append(f.applies, dir)
	f.mu.Unlock()
	return f.ApplyErr
}

// appliedDirs is what Apply was called with, in order.
func (f *fakeTofu) appliedDirs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.applies...)
}
func (f *fakeTofu) PlanDetailed(ctx context.Context, dir string) (bool, error) {
	return f.PlanDetailedChanged, f.PlanDetailedErr
}
func (f *fakeTofu) ShowJSON(ctx context.Context, dir, planFile string) ([]byte, error) {
	if f.ShowJSONErr != nil {
		return nil, f.ShowJSONErr
	}
	if f.ShowJSONBytes != nil {
		return f.ShowJSONBytes, nil
	}
	return changingPlanJSON, nil
}

// ⚠️ THE DEFAULT PLAN CHANGES SOMETHING, AND IT USED TO NOT.
// This fixture was `{"resource_changes":[]}` -- a plan that applies nothing --
// and every digest-gate test drove it. When applyOneRoot stopped gating a plan
// with no changes in it (there is nothing to gate), those tests went green
// while asserting refusals that could no longer happen: they had been
// exercising the gate on the one input it does not apply to. The fixture was
// the bug, so the default is now a plan with a real change in it and the
// empty one is asked for explicitly by the tests that mean it.
//
// ⚠️ "configuration" IS PRESENT AND DECLARES NOTHING, AND THAT IS NOT
// DECORATION. plan.Declarations refuses a document with no "configuration"
// key outright -- real `tofu show -json` always carries one -- so once
// applyOneRoot started calling it on every plan, this fixture (and every
// test that never overrides ShowJSONBytes) would have been refused by the
// declarations gate instead of exercising whatever it actually means to
// test. A fixture that does not carry what a real plan carries is the bug
// here, not the gate; see AGENTS.md, "when a fixture and production
// disagree, the fixture is the bug."
var changingPlanJSON = []byte(`{"resource_changes":[{"address":"null_resource.a","change":{"actions":["create"],"before":null,"after":{}}}],"configuration":{"root_module":{}}}`)

// noopPlanJSON is a plan whose resource_changes is empty -- the shape a
// clean, no-op apply produces, and what a root that has ALREADY been applied
// re-plans to.
var noopPlanJSON = []byte(`{"resource_changes":[],"configuration":{"root_module":{}}}`)

// noopAfterApplyJSON is the same resource as changingPlanJSON once that plan
// has been applied: OpenTofu still emits the entry, with actions ["no-op"],
// which Canonical drops and countResourceChanges does not count.
var noopAfterApplyJSON = []byte(`{"resource_changes":[{"address":"null_resource.a","change":{"actions":["no-op"],"before":{},"after":{}}}],"configuration":{"root_module":{}}}`)
