package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/beeradb/truss/internal/config"
	"github.com/beeradb/truss/internal/forge"
	"github.com/beeradb/truss/internal/keygen"
	"github.com/beeradb/truss/internal/secrets"
)

// deployKeyRequiredEnv names the four variables this subcommand needs, in the
// shape loadVaultConfig established: cmd/truss reads them itself, because
// internal/config refuses to carry vaults, mounts and item names (§4.1). Every
// problem is reported at once, none is defaulted or guessed.
//
// ⚠️ ALL FOUR OR NONE. A half-configured deploy key is the dangerous state, not
// the recovering one: a target that knows the repository but not the vault
// would mint a credential with nowhere durable to put it, which is the exact
// orphan the ordering in ensureDeployKey exists to prevent. Setting none of them
// is a real answer -- this deployment mints no key -- and is not a problem, the
// same way DELIVERY_BYPASS_ACTOR_ID=0 means "the deployment named nobody" rather
// than "unconfigured, guess something".
var deployKeyRequiredEnv = []string{
	"DEPLOY_KEY_REPOSITORY", // "owner/name" the public half is registered on
	"DEPLOY_KEY_VAULT",      // the 1Password leaf vault holding the private half
	"DEPLOY_KEY_ITEM",       // the item title within it
	"DEPLOY_KEY_COMMENT",    // the key's comment; the registry's only label for it
}

type deployKeyTarget struct {
	Repository, Vault, Item, Comment string
}

func loadDeployKeyTarget(getenv func(string) string) (deployKeyTarget, []string) {
	var set []string
	for _, name := range deployKeyRequiredEnv {
		if getenv(name) != "" {
			set = append(set, name)
		}
	}
	if len(set) == 0 {
		return deployKeyTarget{}, nil
	}
	if len(set) != len(deployKeyRequiredEnv) {
		sort.Strings(set)
		var missing []string
		for _, name := range deployKeyRequiredEnv {
			if !strings.Contains(" "+strings.Join(set, " ")+" ", " "+name+" ") {
				missing = append(missing, "$"+name)
			}
		}
		return deployKeyTarget{}, []string{fmt.Sprintf(
			"refusing to configure a deploy key: $%s are set but %s are not -- a key with no vault to store its private half in is an orphan by construction",
			strings.Join(set, ", $"), strings.Join(missing, ", "))}
	}
	return deployKeyTarget{
		Repository: getenv("DEPLOY_KEY_REPOSITORY"),
		Vault:      getenv("DEPLOY_KEY_VAULT"),
		Item:       getenv("DEPLOY_KEY_ITEM"),
		Comment:    getenv("DEPLOY_KEY_COMMENT"),
	}, nil
}

// deployKeyResult is what ensureDeployKey decided. A struct rather than a
// sentence because this is marshalled into the pass report, and a struct cannot
// accidentally gain a field holding a key: only the labels below exist, and
// none of them is the private half.
type deployKeyResult struct {
	Action  string `json:"action"` // not-configured | already-provisioned | minted | probe
	KeyID   string `json:"key_id,omitempty"`
	Repo    string `json:"repository,omitempty"`
	Vault   string `json:"vault,omitempty"`
	Item    string `json:"item,omitempty"`
	Expires string `json:"expires,omitempty"`
	Note    string `json:"note,omitempty"`
}

// keyRegistrar is the forge's deploy-key surface; leafStore is the 1Password
// half. Both are one step above the calls themselves so ensureDeployKey is
// testable by calling it, which is the same reason internal/gates does no I/O:
// the ordering below is the security property, and a property you can only
// exercise against a live API is a property nobody verifies.
type keyRegistrar interface {
	ListDeployKeys(ctx context.Context, repo string) ([]forge.DeployKey, error)
	CreateDeployKey(ctx context.Context, repo, title, publicKey string, read bool) (forge.DeployKey, error)
	DeleteDeployKey(ctx context.Context, repo string, id int64) error
}

type leafStore interface {
	ReadFields(ctx context.Context, item string) (map[string]string, bool, error)
	PutValue(ctx context.Context, item string, fields map[string]string, cas int) error
}

type deployKeyDeps struct {
	reg    keyRegistrar
	leaf   leafStore
	mint   func(context.Context) (keygen.Keypair, error)
	target deployKeyTarget
	now    func() time.Time
}

// deployKeyExpiryDays sets how far out the stored item's `expires` is written.
//
// ⚠️ Nothing rotates this key yet, so the date WILL lapse with nothing coming
// to replace it, and the expiry sweep will nag daily once it does. That is the
// intended shape rather than an oversight: docs/credentials.md's rule is that a
// credential nobody can rotate is exactly the one that must carry a real date,
// because the nag is the only thing separating "unused for years" from
// "somebody renewed it recently". Writing `never` here would delete the sole
// signal that rotation is still unbuilt.
const deployKeyExpiryDays = 90

// ensureDeployKey provisions the configured key, or says why it did not.
//
// ⚠️ THE ORDER IS THE SECURITY PROPERTY, and it is stated in full because every
// reordering is plausible and only this one is safe:
//
//  1. Read the leaf's `key_id`. Anything there means a machine is authenticating
//     with this credential: do nothing, and above all do not re-mint.
//  2. Delete any key on the forge carrying this title. Reaching here means the
//     leaf holds no id, so such a key's private half exists nowhere -- the pass
//     died between steps 4 and 5. Left in place, every retry would add another
//     credential, and eight keys titled "truss-applier" on one repository is not
//     distinguishable from a leak by whoever finds it.
//  3. Mint. 4. STORE the private half. 5. Register the public half. 6. Record
//     the id beside it.
//
// Storing before registering is the point of the whole sequence. The reverse
// order can reach the one state nothing recovers: a key live on a repository
// whose private half no longer exists, which can push, cannot be used, and
// cannot be revoked by anything that reads this code. Store first and every
// failure leaves a state the next pass recognises -- an unregistered secret is
// inert, and step 2 cleans up its successor.
func ensureDeployKey(ctx context.Context, d deployKeyDeps) (deployKeyResult, error) {
	if d.target.Repository == "" {
		return deployKeyResult{Action: "not-configured"}, nil
	}
	res := deployKeyResult{Repo: d.target.Repository, Vault: d.target.Vault, Item: d.target.Item}
	title := deployKeyTitle(d.target)

	fields, found, err := d.leaf.ReadFields(ctx, d.target.Item)
	if err != nil {
		return res, fmt.Errorf("deploy-key: cannot read %q from the leaf vault, so nothing will be minted: %w", d.target.Item, err)
	}
	if found && strings.TrimSpace(fields["key_id"]) != "" {
		res.Action = "already-provisioned"
		res.KeyID = strings.TrimSpace(fields["key_id"])
		res.Expires = fields["expires"]
		return res, nil
	}

	if err := deleteOrphanKeys(ctx, d.reg, d.target, title); err != nil {
		return res, err
	}

	pair, err := d.mint(ctx)
	if err != nil {
		return res, fmt.Errorf("deploy-key: minting failed: %w", err)
	}
	expires := d.now().UTC().AddDate(0, 0, deployKeyExpiryDays).Format("2006-01-02")
	write := func(keyID string) error {
		f := map[string]string{
			"password":   string(pair.Private),
			"public_key": pair.Public,
			"repository": d.target.Repository,
			"expires":    expires,
		}
		if keyID != "" {
			f["key_id"] = keyID
		}
		return d.leaf.PutValue(ctx, d.target.Item, f, -1)
	}
	if err := write(""); err != nil {
		return res, fmt.Errorf("deploy-key: refusing to register anything: storing the private half in %q failed first, so no credential was created: %w", d.target.Vault, err)
	}

	created, err := d.reg.CreateDeployKey(ctx, d.target.Repository, title, pair.Public, false)
	if err != nil {
		res.Action = "stored-unregistered"
		return res, fmt.Errorf("deploy-key: a keypair is stored in %q and was NOT registered (%v) -- inert, and the next pass retries it", d.target.Vault, err)
	}
	idText := fmt.Sprintf("%d", created.ID)
	if err := write(idText); err != nil {
		res.Action = "minted"
		res.KeyID = idText
		return res, fmt.Errorf("deploy-key: registered %s on %s (title %q) and could not record the id in %q: revoke it by hand, since the next pass will otherwise delete it as an orphan: %w",
			idText, d.target.Repository, title, d.target.Vault, err)
	}
	res.Action, res.KeyID, res.Expires = "minted", idText, expires
	return res, nil
}

func deployKeyTitle(t deployKeyTarget) string {
	return fmt.Sprintf("truss-applier %s", t.Comment)
}

// deleteOrphanKeys removes keys carrying this title while the leaf records no
// id for them. It refuses on a listing error rather than minting alongside a
// duplicate it could not see -- the same reason CheckRulesets refuses on an
// unreadable bypass list instead of reading blindness as compliance.
func deleteOrphanKeys(ctx context.Context, reg keyRegistrar, t deployKeyTarget, title string) error {
	keys, err := reg.ListDeployKeys(ctx, t.Repository)
	if err != nil {
		return fmt.Errorf("deploy-key: cannot list %q's deploy keys before minting, so nothing will be minted: %w", t.Repository, err)
	}
	for _, k := range keys {
		if k.Title != title {
			continue
		}
		if err := reg.DeleteDeployKey(ctx, t.Repository, k.ID); err != nil {
			return fmt.Errorf("deploy-key: an orphan (id %d, title %q) is on %s and could not be removed: %w", k.ID, title, t.Repository, err)
		}
	}
	return nil
}

func cmdDeployKey(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("deploy-key", flag.ContinueOnError)
	fs.SetOutput(stderr)
	probeOnly := fs.Bool("probe-only", false, "resolve the vault and read the item; write nothing")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	target, problems := loadDeployKeyTarget(getenv)
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintf(stderr, "%s\n", p)
		}
		return 1
	}
	if target.Repository == "" {
		return emit(stdout, deployKeyResult{Action: "not-configured",
			Note: "no DEPLOY_KEY_* variable is set, so this deployment mints no deploy key"})
	}

	// Before any network call: if the binary that produces the key is absent,
	// say so now rather than mid-mint. keygen.Probe's comment carries why the
	// check is here (openssh-client is in this image for ansible's sake).
	if err := keygen.Probe(ctx, getenv("SSH_KEYGEN_PATH")); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}

	cfg, cfgProblems := config.Load(getenv)
	if len(cfgProblems) > 0 {
		for _, p := range cfgProblems {
			fmt.Fprintf(stderr, "deploy-key: %s\n", p)
		}
		return 1
	}
	leaf, err := secrets.NewOP(secrets.OPConfig{
		Vault: target.Vault, TokenFile: cfg.OPTokenFile, WritableItem: target.Item,
	})
	if err != nil {
		fmt.Fprintf(stderr, "deploy-key: %v\n", err)
		return 1
	}

	if *probeOnly {
		fields, found, err := leaf.ReadFields(ctx, target.Item)
		if err != nil {
			fmt.Fprintf(stderr, "deploy-key: probe failed: %v\n", err)
			return 1
		}
		res := deployKeyResult{Action: "probe", Vault: target.Vault, Item: target.Item,
			Repo: target.Repository, KeyID: fields["key_id"], Expires: fields["expires"]}
		switch {
		case !found:
			res.Note = "the vault is reachable and has no such item: the write path works and nothing is provisioned yet"
		case res.KeyID == "":
			res.Note = "the item exists and carries no key_id"
		default:
			res.Note = "a key_id is recorded"
		}
		return emit(stdout, res)
	}

	reg, err := buildForgeClient(cfg, getenv)
	if err != nil {
		fmt.Fprintf(stderr, "deploy-key: %v\n", err)
		return 1
	}
	d := deployKeyDeps{
		reg: reg, leaf: leaf, target: target,
		now: func() time.Time { return time.Now().UTC() },
		mint: func(c context.Context) (keygen.Keypair, error) {
			return keygen.Mint(c, keygen.Config{Bin: getenv("SSH_KEYGEN_PATH"), Comment: target.Comment})
		},
	}
	res, err := ensureDeployKey(ctx, d)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		if res.Action != "" {
			_, _ = stdout.Write(append(mustJSON(res), '\n'))
		}
		return 1
	}
	return emit(stdout, res)
}

func emit(w io.Writer, r deployKeyResult) int {
	fmt.Fprintln(w, string(mustJSON(r)))
	return 0
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"action":"error","note":"the result could not be encoded"}`)
	}
	return b
}
