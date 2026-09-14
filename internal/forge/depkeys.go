package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Deploy keys are the credential half of this package that is NOT read-only,
// and that is the whole reason this file is separated from protection.go and
// rulesets.go: everything else here fetches a fact for internal/gates to
// judge, and declares nothing. Registering a key changes repository
// configuration, so it gets its own file, its own tests, and a comment on
// every call.
//
// ⚠️ WHICH CREDENTIAL THE APP RUNS UNDER MATTERS HERE. `POST
// /repos/{owner}/{repo}/keys` needs the App's administration permission. The
// applier's App holds it (that is what makes the ruleset read in rulesets.go
// return `bypass_actors` at all rather than omitting it). The proposer App on
// dev-1 deliberately does NOT: measured 2026-09-14, `GET
// /repos/unsaltedpw/platform/keys` and the same path on a repo outside the
// organization both answer 403 "Resource not accessible by integration" with an
// installation token from that App. So a caller that gets a 403 here is not
// looking at a bug in this file, it is looking at the wrong App, and
// TestA403NamesTheStatusAndBody keeps those two cases apart by carrying the
// status through rather than collapsing it into a generic failure.
//
// The key itself is minted by internal/keygen, not here, and never by GitHub:
// there is no API that hands back a keypair, the caller supplies the public
// half. That split is the reason the two packages do not import each other.

// DeployKey is one repository deploy key as GitHub reports it.
//
// ReadOnly is a pointer rather than a bool because of this package's first
// rule: absent is not false. A response that omits `readonly` has not said the
// key is read-only, it has said nothing, and "nothing" must not be decoded as
// the safer-looking answer.
type DeployKey struct {
	ID       int64
	Key      string
	Title    string
	ReadOnly *bool
}

// wireDeployKey mirrors GitHub's response. Every optional field is a pointer.
type wireDeployKey struct {
	KeyID    *int64  `json:"key_id"`
	ID       *int64  `json:"id"`
	Key      *string `json:"key"`
	Title    *string `json:"title"`
	ReadOnly *bool   `json:"readonly"`
}

func (w wireDeployKey) toDeployKey(path string) (DeployKey, error) {
	// GitHub returns `key_id` on the deploy-key endpoints and `id` on some
	// others. Both are accepted; neither is assumed. An answer carrying
	// neither has an identity this package cannot use, so it is a decode
	// error rather than a zero id -- a key registered with id 0 would be
	// undeletable by anything that takes the id from here.
	var id int64
	switch {
	case w.KeyID != nil:
		id = *w.KeyID
	case w.ID != nil:
		id = *w.ID
	default:
		return DeployKey{}, fmt.Errorf("forge: a deploy key in %s carries neither key_id nor id", path)
	}
	out := DeployKey{ID: id, ReadOnly: w.ReadOnly}
	if w.Key != nil {
		out.Key = *w.Key
	}
	if w.Title != nil {
		out.Title = *w.Title
	}
	return out, nil
}

// ListDeployKeys returns every deploy key registered on repo.
func (c *Client) ListDeployKeys(ctx context.Context, repo string) ([]DeployKey, error) {
	if err := validateRepo(repo); err != nil {
		return nil, err
	}
	path := "/repos/" + repo + "/keys"
	var wire []wireDeployKey
	if err := c.request(ctx, http.MethodGet, path, &wire); err != nil {
		return nil, err
	}
	out := make([]DeployKey, 0, len(wire))
	for _, w := range wire {
		dk, err := w.toDeployKey(path)
		if err != nil {
			return nil, err
		}
		out = append(out, dk)
	}
	return out, nil
}

// FindDeployKeyByPublic reports whether repo already carries exactly this
// public key.
//
// ⚠️ This exists so that minting is safe to attempt twice. Without it, every
// retried pass that reached CreateDeployKey would add another credential to the
// repository, and an accumulating set of deploy keys is not a leak but is
// indistinguishable from one on an inspection: nobody can say which of the
// eight "truss-applier" keys is live. The comparison is on the key material
// itself rather than the title, because a title is a label someone can reuse
// and a public key is the identity.
func (c *Client) FindDeployKeyByPublic(ctx context.Context, repo, publicKey string) (DeployKey, bool, error) {
	keys, err := c.ListDeployKeys(ctx, repo)
	if err != nil {
		return DeployKey{}, false, err
	}
	want := strings.TrimSpace(publicKey)
	for _, k := range keys {
		if strings.TrimSpace(k.Key) == want {
			return k, true, nil
		}
	}
	return DeployKey{}, false, nil
}

// CreateDeployKey registers a public key on repo and returns its id.
//
// This is the one write in this package. It is refused on an empty publicKey
// or title, and the repo is validated rather than interpolated blind, because
// the path is built from a caller-supplied string.
//
// read_only=false is passed deliberately by callers that need to push; a
// caller that wants a read credential must say so, which is why it is an
// argument here rather than a constant in the body.
func (c *Client) CreateDeployKey(ctx context.Context, repo, title, publicKey string, read bool) (DeployKey, error) {
	if err := validateRepo(repo); err != nil {
		return DeployKey{}, err
	}
	if strings.TrimSpace(title) == "" {
		return DeployKey{}, fmt.Errorf("forge: refusing to register a deploy key on %s with no title", repo)
	}
	if strings.TrimSpace(publicKey) == "" {
		return DeployKey{}, fmt.Errorf("forge: refusing to register an empty public key on %s", repo)
	}

	path := "/repos/" + repo + "/keys"
	body, err := json.Marshal(map[string]any{
		"title":    title,
		"key":      publicKey,
		"readonly": read,
	})
	if err != nil {
		return DeployKey{}, fmt.Errorf("forge: encoding the deploy key for %s: %w", repo, err)
	}

	var w wireDeployKey
	if err := c.requestBody(ctx, http.MethodPost, path, body, &w); err != nil {
		return DeployKey{}, err
	}
	dk, err := w.toDeployKey(path)
	if err != nil {
		return DeployKey{}, err
	}
	// A key GitHub accepted but did not describe cannot be reported to the
	// caller as usable: the title is the only thing that makes it findable
	// later, and the id is the only thing that makes it revocable.
	if dk.Title == "" {
		dk.Title = title
	}
	return dk, nil
}

// DeleteDeployKey removes a key by id. Revocation is named here because a
// credential with no way back out is not a credential you control -- see
// docs/credentials.md, "Revocation is a commit, not a dashboard".
func (c *Client) DeleteDeployKey(ctx context.Context, repo string, id int64) error {
	if err := validateRepo(repo); err != nil {
		return err
	}
	if id <= 0 {
		return fmt.Errorf("forge: refusing to delete deploy key id %d on %s: an id of zero would be a bug, not a target", id, repo)
	}
	return c.request(ctx, http.MethodDelete, fmt.Sprintf("/repos/%s/keys/%d", repo, id), nil)
}
