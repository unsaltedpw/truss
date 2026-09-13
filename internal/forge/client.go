// Package forge is the GitHub client the security gates depend on: it fetches
// the facts (branch protection, a PR's merge state, its reviews, a commit's
// verification) that internal/gates turns into a pass or a refusal. Nothing
// here decides anything -- that is gates' job -- this package only fetches
// and decodes.
//
// Its failure mode is a gate that passes when it should refuse, so three
// rules run through every file here:
//
//   - Absent is not false. Every optional field in a GitHub payload decodes
//     into a pointer, all the way down; a missing key must produce nil, never
//     a defaulted zero value.
//   - The list endpoint `commits/{sha}/pulls` cannot carry `merged` -- it has
//     no such field, only `merged_at` -- so PullNumbersForCommit returns
//     `[]int` and nothing else. A struct that could carry `Merged` from that
//     endpoint would reintroduce the bug that refused this platform's own
//     first merge on 2026-09-07.
//   - No credential ever reaches an error string. The GitHub App private key
//     never touches disk, and any response body or transport error that
//     might echo a token back is redacted before it is wrapped.
package forge

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config is everything a Client needs. The private key is bytes, never a
// path: this package never writes it to disk (see TestTheKeyNeverTouchesDisk).
type Config struct {
	BaseURL        string // defaults to https://api.github.com
	Repo           string // "owner/name"
	AppID          int64
	InstallationID int64
	PrivateKeyPEM  []byte
	HTTP           *http.Client // defaults to a client with a 30s timeout
}

// Client is a GitHub App client scoped to one repo and one installation.
type Client struct {
	baseURL        string
	repo           string
	appID          int64
	installationID int64
	key            *rsa.PrivateKey
	http           *http.Client

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
}

// repoPattern accepts exactly "owner/name" with the character set GitHub
// itself allows in an owner or repo name. It exists alongside the explicit
// ".." check below rather than in place of it: ".." is made entirely of
// characters this class permits, so the class alone would not catch it.
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?/[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$`)

func validateRepo(repo string) error {
	if repo == "" {
		return errors.New("forge: repo is empty")
	}
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("forge: repo %q is not owner/name", repo)
	}
	for _, p := range parts {
		if strings.Contains(p, "..") {
			return fmt.Errorf("forge: repo %q contains a path traversal segment", repo)
		}
	}
	if !repoPattern.MatchString(repo) {
		return fmt.Errorf("forge: repo %q contains characters that are not safe in a URL path", repo)
	}
	return nil
}

// parsePrivateKey accepts either PKCS#1 ("RSA PRIVATE KEY", what GitHub's
// own "generate a private key" button produces) or PKCS#8 ("PRIVATE KEY").
// It never writes the input anywhere; parsing is entirely in memory.
func parsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("forge: private key is not PEM-encoded")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("forge: could not parse private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("forge: private key is not RSA")
	}
	return key, nil
}

// New validates cfg and returns a Client. It refuses an empty app ID,
// installation ID, repo or key rather than deferring the failure to the
// first call that needs one.
func New(cfg Config) (*Client, error) {
	if cfg.AppID <= 0 {
		return nil, errors.New("forge: AppID must be positive")
	}
	if cfg.InstallationID <= 0 {
		return nil, errors.New("forge: InstallationID must be positive")
	}
	if err := validateRepo(cfg.Repo); err != nil {
		return nil, err
	}
	if len(cfg.PrivateKeyPEM) == 0 {
		return nil, errors.New("forge: PrivateKeyPEM is empty")
	}
	key, err := parsePrivateKey(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, err
	}

	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	baseURL = strings.TrimRight(baseURL, "/")

	httpClient := cfg.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	return &Client{
		baseURL:        baseURL,
		repo:           cfg.Repo,
		appID:          cfg.AppID,
		installationID: cfg.InstallationID,
		key:            key,
		http:           httpClient,
	}, nil
}

// --- the App JWT and the installation token --------------------------------

type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

type jwtPayload struct {
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
	Iss string `json:"iss"`
}

func base64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// signAppJWT builds the RS256 App JWT gh-app-token:28-32 specifies: iat
// backdated 60s for clock skew, exp ten minutes out (540s, matching the
// bash's own arithmetic exactly rather than rounding to "ten minutes").
func (c *Client) signAppJWT(now time.Time) (string, error) {
	header, err := json.Marshal(jwtHeader{Alg: "RS256", Typ: "JWT"})
	if err != nil {
		return "", fmt.Errorf("forge: encoding jwt header: %w", err)
	}
	payload, err := json.Marshal(jwtPayload{
		Iat: now.Unix() - 60,
		Exp: now.Unix() + 540,
		Iss: strconv.FormatInt(c.appID, 10),
	})
	if err != nil {
		return "", fmt.Errorf("forge: encoding jwt payload: %w", err)
	}

	signingInput := base64url(header) + "." + base64url(payload)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("forge: signing jwt: %w", err)
	}
	return signingInput + "." + base64url(sig), nil
}

type installTokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
	Message   string `json:"message"`
}

// mintInstallationToken always performs a fresh POST -- it never reads or
// writes the cache. InstallationToken and the internal cachedToken helper
// each call this for a different reason: one for a caller that explicitly
// wants a current token (mirroring the gh-app-token binary), the other only
// when the cache is empty or nearly expired.
func (c *Client) mintInstallationToken(ctx context.Context) (string, time.Time, error) {
	jwt, err := c.signAppJWT(time.Now())
	if err != nil {
		return "", time.Time{}, err
	}

	path := fmt.Sprintf("/app/installations/%s/access_tokens", url.PathEscape(strconv.FormatInt(c.installationID, 10)))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, nil)
	if err != nil {
		return "", time.Time{}, sanitizeErr(err, jwt)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", time.Time{}, sanitizeErr(err, jwt)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", time.Time{}, sanitizeErr(err, jwt)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", time.Time{}, fmt.Errorf("forge: minting installation token: %d: %s",
			resp.StatusCode, redact(string(body), jwt))
	}

	var out installTokenResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return "", time.Time{}, fmt.Errorf("forge: decoding installation token response: %w", err)
	}
	if out.Token == "" {
		reason := out.Message
		if reason == "" {
			reason = redact(string(body), jwt)
		}
		return "", time.Time{}, fmt.Errorf("forge: no token in response: %s", redact(reason, jwt))
	}

	expiry := time.Time{}
	if out.ExpiresAt != "" {
		expiry, err = time.Parse(time.RFC3339, out.ExpiresAt)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("forge: parsing token expiry: %w", err)
		}
	}
	return out.Token, expiry, nil
}

// InstallationToken mints a fresh installation token and returns it with its
// expiry. It is what `truss token` (replacing gh-app-token) calls, and it
// always performs a real mint -- it is the source of truth the internal
// cache below refreshes from, never the other way around.
func (c *Client) InstallationToken(ctx context.Context) (string, time.Time, error) {
	return c.mintInstallationToken(ctx)
}

// AppID returns the App id this Client was configured with -- the same
// value it already sends as the JWT `iss` claim (signAppJWT). It performs no
// I/O: the id was read once, at construction, from the same github-app
// credential every other call here authenticates with. gates.CheckDeliveryRef
// needs it to recognise the applier's own App as a ruleset's bypass actor,
// and gates does no I/O of its own, so the value has to be handed in rather
// than fetched.
func (c *Client) AppID() int64 {
	return c.appID
}

// cachedToken is what every other authenticated call in this package uses:
// mint lazily, reuse until close to expiry, so a pass that reads protection,
// several pull requests and their reviews does not mint a token per call.
func (c *Client) cachedToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.token != "" && !c.tokenExpiry.IsZero() && time.Now().Before(c.tokenExpiry.Add(-60*time.Second)) {
		return c.token, nil
	}
	tok, expiry, err := c.mintInstallationToken(ctx)
	if err != nil {
		return "", err
	}
	c.token, c.tokenExpiry = tok, expiry
	return c.token, nil
}

// --- shared request plumbing -------------------------------------------------

// redact replaces every occurrence of each non-empty secret with a fixed
// marker. It is applied to response bodies and to stringified errors alike,
// because net/http embeds the request URL in *url.Error and a naive %w would
// carry a credential straight through if one ever ended up there.
func redact(s string, secrets ...string) string {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		s = strings.ReplaceAll(s, secret, "[REDACTED]")
	}
	return s
}

// sanitizeErr rebuilds an error from its redacted text rather than wrapping
// the original. Wrapping is not enough: %w keeps the original error's
// Error() reachable (directly, or via errors.Unwrap and fmt's %v/%s), and a
// *url.Error's Error() method re-renders the request URL every time it is
// called, so any redaction applied to a one-off .Error() string would not
// survive being wrapped.
func sanitizeErr(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	return errors.New(redact(err.Error(), secrets...))
}

func (c *Client) request(ctx context.Context, method, path string, out interface{}) error {
	token, err := c.cachedToken(ctx)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	if err != nil {
		return sanitizeErr(err, token)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.http.Do(req)
	if err != nil {
		return sanitizeErr(err, token)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return sanitizeErr(err, token)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("forge: %s %s: %d: %s", method, path, resp.StatusCode, redact(string(body), token))
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("forge: decoding %s: %w", path, err)
	}
	return nil
}

func (c *Client) repoPath(format string, a ...interface{}) string {
	return "/repos/" + c.repo + fmt.Sprintf(format, a...)
}
