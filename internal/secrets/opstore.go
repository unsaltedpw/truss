package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
)

// OP is a Store backed by the 1Password `op` CLI, covering a 1Password
// vault the way KV covers a Vault KV v2 mount. Sweep.Stores is a slice of
// the Store *interface*, not of KV, so this is simply the second thing
// that satisfies it -- no Vault mount, no new secret backend wiring, just
// a second way to answer "what items exist and what does each say about
// its own expiry".
//
// OP never dials 1Password's API itself; `op` does that, authenticating
// with a service-account token this store reads once from OPConfig.Token
// and hands to the subprocess over the environment -- never over argv (see
// the comment in Expiry for why a vault name and item title in argv are
// the item TITLE and never for a value).
type OP struct {
	cfg OPConfig

	// run executes one `op` invocation and reports what came back:
	// stdout, stderr and the error exec.Cmd.Run itself returned (nil on
	// exit 0, *exec.ExitError on a non-zero exit, something else on a
	// failure to start the process at all). Nil means execReal, which
	// actually starts cfg.Bin as a subprocess. Tests set this to a fake
	// that returns canned output with no process ever started -- the same
	// "inject the seam as a field" shape KV takes for its *http.Client and
	// Dir takes for OnFieldRead.
	run func(ctx context.Context, env []string, args ...string) (stdout, stderr []byte, err error)

	// http is the client the write path uses, defaulted by NewOP from
	// OPConfig.HTTP -- the same field shape KV carries, because the reads and
	// writes here deliberately do not share a transport.
	http *http.Client
}

// OPConfig is everything NewOP needs to run `op` against one 1Password
// vault.
type OPConfig struct {
	// Vault is the 1Password vault name to list and read, e.g.
	// "recipes-runtime". Never hardcoded inside this package
	// (TestNoVaultOrTokenIsHardcodedInOP), matching KVConfig.Mount.
	Vault string

	// Bin is the path to the op binary. Empty means "op", resolved via
	// PATH the way exec.Command always does; production points this at
	// /usr/bin/op, verified present in the deployed truss image (op
	// 2.39.0).
	Bin string

	// TokenFile is the path to a file holding the 1Password service
	// account token that feeds OP_SERVICE_ACCOUNT_TOKEN -- the same shape
	// as KVConfig.JWTPath and, one layer up, Config.OPTokenFile /
	// apply.sh:215-216 ("export OP_SERVICE_ACCOUNT_TOKEN;
	// OP_SERVICE_ACCOUNT_TOKEN=\"$(cat \"$OP_TOKEN_FILE\")\""): a file,
	// read once, never a value handed in directly. Required; NewOP refuses
	// an empty one the same way NewKV refuses an empty JWTPath.
	TokenFile string

	// HTTP is the client the write path uses (opwrite.go). The reads in this
	// file never touch it, because reads go through `op` and writes cannot:
	// the CLI takes a field value in argv, and argv is not a place a secret
	// belongs. Nil means a client with the package timeout; tests point it at
	// an httptest.Server.
	HTTP *http.Client

	// WritableItem is the single item PutValue and PatchExpiry may write, in
	// exactly the construction-time sense of KVConfig.WritableItem: empty
	// means this OP reads only. Every existing caller leaves it unset, so a
	// store built to sweep a vault gains no write ability by sharing a type
	// with one that writes.
	WritableItem string
}

// NewOP validates cfg and returns an OP, or refuses. Vault and TokenFile
// must both be non-empty: an OP built on either being empty would either
// talk to the wrong vault or run `op` with no way to authenticate at all.
func NewOP(cfg OPConfig) (*OP, error) {
	var problems []string
	if cfg.Vault == "" {
		problems = append(problems, "Vault is empty")
	}
	if cfg.TokenFile == "" {
		problems = append(problems, "TokenFile is empty")
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("secrets: refusing to construct an OP store: %s", strings.Join(problems, "; "))
	}
	if cfg.Bin == "" {
		cfg.Bin = "op"
	}
	o := &OP{cfg: cfg}
	o.http = cfg.HTTP
	if o.http == nil {
		o.http = &http.Client{Timeout: httpTimeout}
	}
	return o, nil
}

// Name returns the vault name, so a sweep error and a "reported in two
// stores" finding can both name where it came from.
func (o *OP) Name() string { return o.cfg.Vault }

// token reads and caches OPConfig.TokenFile. Held on the struct only long
// enough to redact it out of an `op` error -- the same lifetime KV.jwt has
// for the Kubernetes JWT.
func (o *OP) token() (string, error) {
	b, err := os.ReadFile(o.cfg.TokenFile)
	if err != nil {
		return "", fmt.Errorf("secrets: reading the 1Password service account token at %s: %w", o.cfg.TokenFile, err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("secrets: %s is empty -- the 1Password service account token is missing", o.cfg.TokenFile)
	}
	return tok, nil
}

// exec runs one `op` subcommand and returns its stdout and stderr,
// redacting the service-account token out of anything it hands back to a
// caller building an error. It reads the token fresh on every call rather
// than caching it on the struct across calls -- OP has no analogue to KV's
// "one login per Run", because there is no login step to amortise: `op`
// authenticates itself, per invocation, from the environment this method
// builds.
func (o *OP) exec(ctx context.Context, args ...string) (stdout, stderr []byte, err error) {
	tok, err := o.token()
	if err != nil {
		return nil, nil, err
	}

	run := o.run
	if run == nil {
		run = o.execReal
	}

	// ⚠️ EXPLICIT ENV, NOT INHERITED. Runner env is never inherited
	// implicitly elsewhere in this project (cmd/truss's buildBaseEnv makes
	// the same choice for tofu, and says why in its own comment): PATH so
	// `op` can find whatever it shells out to, and the token -- nothing
	// else this process happens to be holding crosses into the
	// subprocess's environment by accident.
	// ⚠️ AND A WRITABLE HOME, WHICH `op` REQUIRES AND WILL NOT TELL YOU ABOUT
	// UNTIL IT FAILS. The CLI creates $HOME/.config/op before doing anything,
	// so under readOnlyRootFilesystem it dies with `cannot create directory
	// "/home/applier/.config/op" ... read-only file system` -- measured on the
	// first real value publish, 2026-09-08.
	//
	// It is set HERE rather than in the pod spec precisely BECAUSE the env
	// above is explicit: a HOME in the manifest never reaches this subprocess,
	// which is the whole point of not inheriting. Setting it in the manifest
	// was tried first and changed nothing.
	//
	// A per-call temp dir, removed after: `op` writes only cache and config
	// there, never the token (that arrives in the environment), and a
	// directory this process owns for the length of one call cannot be
	// preloaded by anything else.
	home, err := os.MkdirTemp("", "op-home-")
	if err != nil {
		return nil, nil, fmt.Errorf("secrets: making a home directory for the 1Password CLI: %w", err)
	}
	defer os.RemoveAll(home)

	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"OP_SERVICE_ACCOUNT_TOKEN=" + tok,
	}

	out, errOut, runErr := run(ctx, env, args...)
	// Redacted on EVERY path, not just the error one: fieldIsMissing below
	// pattern-matches this same stderr, and a token substring replaced with
	// "***" cannot collide with any of its phrases, so redacting first
	// costs nothing and closes the path where a caller builds an error out
	// of stderr independently of whether run() itself returned one.
	errOut = redactBytes(errOut, tok)
	if runErr != nil {
		return out, errOut, fmt.Errorf("%s", redact(runErr.Error(), tok))
	}
	return out, errOut, nil
}

// redactBytes is redact for a []byte, since op's stderr is read as bytes
// and a caller may want to pattern-match it (fieldIsMissing) before it is
// ever turned into a string for an error message.
func redactBytes(b []byte, values ...string) []byte {
	return []byte(redact(string(b), values...))
}

// execReal actually starts cfg.Bin as a subprocess. This is the only place
// in this package that ever exec's a real process.
func (o *OP) execReal(ctx context.Context, env []string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, o.cfg.Bin, args...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// List runs `op item list --vault <vault> --format json` and returns every
// item's title.
//
// ⚠️ A FAILED LIST IS ALWAYS AN ERROR, NEVER AN EMPTY SLICE. This is the
// exact bug in the deployed bash this store replaces: `mapfile < <(OP item
// list ...)` ran the list in a subshell, so a rate-limited or otherwise
// failed list produced zero titles rather than a propagated failure -- the
// loop over titles never ran, and the pass reported a clean bill of health
// for a question it never got to ask. Go has no subshell for an error to
// get lost in, but the same failure is reachable here just as easily by
// swallowing runErr instead of returning it, so this method returns
// (nil, err) on ANY non-zero exit and (nil, nil) only when `op` itself
// said, with a clean exit, that the vault holds no items.
func (o *OP) List(ctx context.Context) ([]string, error) {
	stdout, stderr, err := o.exec(ctx, "item", "list", "--vault", o.cfg.Vault, "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("secrets: listing 1Password vault %q: %s: %s", o.cfg.Vault, err, string(stderr))
	}

	var items []struct {
		Title string `json:"title"`
	}
	if jsonErr := json.Unmarshal(stdout, &items); jsonErr != nil {
		return nil, fmt.Errorf("secrets: parsing the item list for 1Password vault %q: %w", o.cfg.Vault, jsonErr)
	}
	if len(items) == 0 {
		return nil, nil
	}
	titles := make([]string, 0, len(items))
	for _, it := range items {
		titles = append(titles, it.Title)
	}
	return titles, nil
}

// Expiry reads one item's `expires` field.
//
// ⚠️ THE FLAG FORM, NOT THE URI FORM, AND THAT IS A DELIBERATE DEVIATION
// FROM THE BASH. vault/bootstrap-vault.sh and apply.sh both use
// `op read <scheme>://<vault>/<item>/expires`. This uses
// `op item get <item> --vault <vault> --fields expires --format json`,
// which performs the identical read. The reason is scripts/leakscan: it
// refuses that URI scheme outright, because a reference INTO a store names
// a deployment, and this repository is written to be public. Constructing
// the scheme from fragments to slip past the scanner would be obfuscation,
// and the flag form is a genuine alternative rather than a dodge.
//
// THE VAULT NAME AND ITEM TITLE GO IN ARGV, AND THAT IS FINE: they name a
// vault and an item TITLE, never a value, which is exactly the
// line vault/bootstrap-vault.sh draws for its own `op read` calls and its
// own reasoning for refusing argv elsewhere in that script -- a value in
// argv is visible in `ps` on the operator's machine for the call's
// duration, in /proc inside the container, and in the exec request URI
// that lands in an API audit log. A vault name and an item title carry
// none of that: they identify a credential, they are not one. The value
// this call reads back -- the `expires` field's contents, always a date or
// "never" -- travels over stdout, never argv, and is the only thing this
// method treats as data rather than as identity.
//
// `op` uses exactly two exit codes for every outcome, 0 and 1 -- 1Password's
// published CLI reference documents no finer-grained set (the URL is left out
// deliberately: scripts/leakscan refuses a URL path into a host, and a
// citation is not worth widening that rule) -- so a failed read cannot be
// told apart from a missing
// field by its exit code -- only by matching its stderr text. This mirrors
// established, already-deployed practice in this same repository:
// vault/bootstrap-vault.sh's own read loop distinguishes "genuinely
// absent" from a real failure with `case "$err" in *"isn't an item"*|
// *"not found"*|*"no such"*|*"doesn't have"*) …`, and fieldIsMissing below
// matches the same four substrings for the same reason. Nothing else has
// been captured against a live account from this sandbox, which has no
// 1Password credentials to exercise; if a future `op` release phrases this
// differently, the fail-closed direction still holds, because an
// unrecognised message falls through to the error branch rather than being
// swallowed as "no expiry recorded" (§4.7's rule, matched here: a
// genuine failure is never allowed to read as "found nothing").
func (o *OP) Expiry(ctx context.Context, item string) (raw string, recorded bool, err error) {
	stdout, stderr, err := o.exec(ctx, "item", "get", item,
		"--vault", o.cfg.Vault, "--fields", "expires", "--format", "json")
	if err != nil {
		if fieldIsMissing(stderr) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("secrets: reading the expiry of %q in 1Password vault %q: %s: %s", item, o.cfg.Vault, err, string(stderr))
	}

	// ⚠️ JSON RATHER THAN THE BARE VALUE, BECAUSE THE BARE FORM IS AMBIGUOUS.
	// `op item get --fields` has printed both "value" and "label: value"
	// across 2.x releases depending on flags, and this sandbox has no
	// 1Password account to settle which. `--format json` is unambiguous in
	// every version that supports it. An unparseable answer is an ERROR, never
	// "no expiry recorded" -- the fail-closed direction §4.7 requires, and the
	// reason this does not simply fall back to trimming stdout.
	raw, err = fieldValueFromJSON(stdout)
	if err != nil {
		return "", false, fmt.Errorf("secrets: reading the expiry of %q in 1Password vault %q: %w", item, o.cfg.Vault, err)
	}
	if raw == "" {
		// A present-but-empty field is not a date and must not be read as one.
		return "", false, nil
	}
	return raw, true, nil
}

// Field reads one field of one item -- the credential value itself, as
// opposed to Expiry's metadata read. It exists for the publisher fetching
// the Cloudflare token it is about to write into Vault rather than
// receiving it over the handoff socket. Its contract is deliberately
// stricter than Expiry's: a missing OR present-but-empty field is always an
// error, never a value quietly treated as absent. Expiry's "unrecorded"
// outcome is correct for a sweep walking arbitrary items, most of which
// legitimately carry no expiry; there is no equivalent legitimate reason
// for the field this method reads to come back empty, so returning "" here
// would be exactly the vacuous-pass mistake Expiry's own doc warns about,
// one fetch at a time instead of one item at a time.
func (o *OP) Field(ctx context.Context, item, field string) (string, error) {
	stdout, stderr, err := o.exec(ctx, "item", "get", item,
		"--vault", o.cfg.Vault, "--fields", field, "--format", "json")
	if err != nil {
		return "", fmt.Errorf("secrets: reading %q.%q from 1Password vault %q: %s: %s", item, field, o.cfg.Vault, err, string(stderr))
	}
	value, err := fieldValueFromJSON(stdout)
	if err != nil {
		return "", fmt.Errorf("secrets: reading %q.%q from 1Password vault %q: %w", item, field, o.cfg.Vault, err)
	}
	if value == "" {
		return "", fmt.Errorf("secrets: %q.%q in 1Password vault %q is empty -- refusing to treat that as a silent skip", item, field, o.cfg.Vault)
	}
	return value, nil
}

// fieldValueFromJSON pulls the field value out of `op item get --format
// json`. It accepts both shapes op has emitted for --fields: a single field
// object, and an array of them. Shared by Expiry and Field -- both read a
// field's value identically and differ only in what an empty answer means.
func fieldValueFromJSON(b []byte) (string, error) {
	type field struct {
		Value string `json:"value"`
	}
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 {
		return "", fmt.Errorf("op returned no output")
	}
	if trimmed[0] == '[' {
		var fs []field
		if err := json.Unmarshal(trimmed, &fs); err != nil {
			return "", fmt.Errorf("op returned JSON this does not understand: %w", err)
		}
		if len(fs) == 0 {
			return "", nil
		}
		return strings.TrimSpace(fs[0].Value), nil
	}
	var f field
	if err := json.Unmarshal(trimmed, &f); err != nil {
		return "", fmt.Errorf("op returned JSON this does not understand: %w", err)
	}
	return strings.TrimSpace(f.Value), nil
}

// fieldIsMissing reports whether stderr is `op read`'s way of saying the
// item exists but carries no `expires` field or section at all, as opposed
// to a genuine failure to reach 1Password (a spent rate limit, a bad or
// expired token, a network failure, a vault that does not exist). See the
// doc comment on Expiry for where these four substrings come from.
func fieldIsMissing(stderr []byte) bool {
	s := string(stderr)
	return strings.Contains(s, "isn't an item") ||
		strings.Contains(s, "not found") ||
		strings.Contains(s, "no such") ||
		strings.Contains(s, "doesn't have")
}
