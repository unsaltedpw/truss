package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// OP implements Publisher as well as Store: the leaf vaults a minted credential
// has to land in are 1Password vaults, and this package already reaches them.
//
// ⚠️ WRITTEN AS HTTP, NOT AS `op`. The CLI takes a field value as
// `name=value` in ARGV, and argv is visible in `ps`, in /proc/<pid>/cmdline and
// in any exec audit log -- which is why this package execs `op` only for reads
// (List, Expiry, Field) and why every value-carrying path elsewhere in the
// project (KV.PutValue, Vault login, the Cloudflare probe) speaks HTTP with the
// credential in a header and the value in a body. A write that put a private key
// in argv would be a worse version of the thing this file exists to store.
//
// The account host comes from the token's own `iss` claim rather than from a
// second config value, because a token that names an account and a URL that
// names an account are one fact, and two copies of one fact is how you get a
// write that succeeds against the wrong vault.
//
// ---------------------------------------------------------------------------
// ⚠️ UNVERIFIED AGAINST THE LIVE SERVICE ACCOUNT, and stated here rather than
// assumed in a caller. dev-1 has no `op` and no 1Password token, so every
// assertion below about endpoints and body shape is from 1Password's published
// Developer API contract, exercised here only against a fake server. Before the
// daily pass is allowed to call this, run on the applier pod (which has both):
//
//	truss deploy-key --probe-only        // resolves the vault name, no write
//	truss deploy-key --probe-only --item <throwaway>
//
// If either answers 401/403, the service account lacks write on that vault and
// the answer is a grant decision in 1Password, not a code change here -- the
// same shape as the pending Vault write decision in docs/port-plan.md §step 7.
// ---------------------------------------------------------------------------

// opAPIPath is the Developer API root appended to the account host. Named once
// so a change of API version is one edit and not a search.
const opAPIPath = "/developer/api/v2"

// PutValue writes fields into item, creating the item if the vault has no such
// title and replacing the named fields if it does.
//
// Like KV.PutValue it refuses any item other than the one this OP was
// constructed to write: a Store built to sweep a vault must not acquire the
// ability to overwrite credentials in it by having a Publisher method on its
// type. cfg.WritableItem is unset for every existing caller, so today every
// read-only OP keeps refusing here exactly as KV does.
func (o *OP) PutValue(ctx context.Context, item string, fields map[string]string, cas int) error {
	if o.cfg.WritableItem == "" {
		return fmt.Errorf("secrets: this 1Password store was built to read %q and refuses to write: WritableItem is unset", o.cfg.Vault)
	}
	if item != o.cfg.WritableItem {
		return fmt.Errorf("secrets: refusing to write item %q: this store may write only %q", item, o.cfg.WritableItem)
	}
	if len(fields) == 0 {
		return fmt.Errorf("secrets: refusing to write %q with no fields", item)
	}

	tok, err := o.token()
	if err != nil {
		return err
	}
	base, err := apiBaseFromToken(tok)
	if err != nil {
		return err
	}
	vaultID, err := o.findVault(ctx, base, tok)
	if err != nil {
		return err
	}
	// Collect the values so the redaction list covers them: an error from this
	// function that echoed a response body must not carry a private key.
	values := make([]string, 0, len(fields))
	for _, v := range fields {
		values = append(values, v)
	}

	existing, found, err := o.findItem(ctx, base, tok, vaultID, item)
	if err != nil {
		return err
	}

	var method, path string
	var body []byte
	if found {
		merged := applyFields(existing, fields)
		// `version` is 1Password's own compare-and-set, and cas is the
		// caller's: -1 means "write whatever is there", anything else must
		// match the version just read or the write is refused rather than
		// silently clobbering a value another pass minted.
		if cas >= 0 && existing.Version != cas {
			return fmt.Errorf("secrets: refusing to write %q: it is version %d and cas asked for %d", item, existing.Version, cas)
		}
		method = http.MethodPut
		path = fmt.Sprintf("/vaults/%s/items/%s", vaultID, existing.ID)
		body, err = json.Marshal(merged)
	} else {
		if cas > 0 {
			return fmt.Errorf("secrets: refusing to write %q with cas=%d: the item does not exist, so there is no version to match", item, cas)
		}
		method = http.MethodPost
		path = "/vaults/" + vaultID + "/items"
		body, err = json.Marshal(newOpItem(item, fields))
	}
	if err != nil {
		return fmt.Errorf("secrets: encoding the write for %q: %s", item, redact(err.Error(), append(values, tok)...))
	}

	respBody, err := o.doJSON(ctx, method, base+path, tok, body, values)
	if err != nil {
		return err
	}
	_ = respBody
	return nil
}

// PatchExpiry sets the item's `expires` custom metadata. 1Password's API has no
// metadata-only endpoint the way Vault's does, so this is a read-modify-write
// of one section field. It is separate from PutValue deliberately: the expiry
// sweep writes a date and must never be able to overwrite a credential while
// doing it, which is the same reason KV.PatchExpiry cannot touch a value.
func (o *OP) PatchExpiry(ctx context.Context, item, expires string) error {
	if o.cfg.WritableItem == "" {
		return fmt.Errorf("secrets: this 1Password store was built to read %q and refuses to write metadata: WritableItem is unset", o.cfg.Vault)
	}
	if item != o.cfg.WritableItem {
		return fmt.Errorf("secrets: refusing to write expiry on item %q: this store may write only %q", item, o.cfg.WritableItem)
	}
	if strings.TrimSpace(expires) == "" {
		return fmt.Errorf("secrets: refusing to write an empty expiry on %q", item)
	}
	tok, err := o.token()
	if err != nil {
		return err
	}
	base, err := apiBaseFromToken(tok)
	if err != nil {
		return err
	}
	vaultID, err := o.findVault(ctx, base, tok)
	if err != nil {
		return err
	}
	existing, found, err := o.findItem(ctx, base, tok, vaultID, item)
	if err != nil {
		return err
	}
	// PATCH cannot create, matching KV: a metadata write for a title nobody has
	// ever minted would invent a credential slot for a value to be mistaken for.
	if !found {
		return fmt.Errorf("secrets: refusing to set an expiry on %q: the item does not exist", item)
	}
	body, err := json.Marshal(setExpiryField(existing, expires))
	if err != nil {
		return fmt.Errorf("secrets: encoding the expiry for %q: %s", item, redact(err.Error(), expires, tok))
	}
	_, err = o.doJSON(ctx, http.MethodPut, fmt.Sprintf("%s/vaults/%s/items/%s", base, vaultID, existing.ID), tok, body, []string{expires})
	return err
}

// --- the wire shapes actually used -----------------------------------------

type opVault struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type opField struct {
	Type    string `json:"type"`
	Value   string `json:"value,omitempty"`
	Label   string `json:"label,omitempty"`
	Purpose string `json:"purpose,omitempty"`
}

type opSection struct {
	Label  string    `json:"label"`
	Fields []opField `json:"fields"`
}

type opItem struct {
	ID       string      `json:"id,omitempty"`
	Title    string      `json:"title"`
	Category string      `json:"category"`
	Version  int         `json:"version,omitempty"`
	Fields   []opField   `json:"fields,omitempty"`
	Sections []opSection `json:"sections,omitempty"`
	Vault    *struct {
		ID string `json:"id"`
	} `json:"vault,omitempty"`
}

const (
	opSectionGenerated = "generated"
	opFieldExpires     = "expires"
)

// newOpItem builds the create body. The secret goes in the category's own
// `password` field -- credentials/main.tf's cf_infra_admin records why:
// 1Password's validator rejects a PASSWORD item whose ps value is absent, and
// it was measured on the first real apply, 2026-09-07.
func newOpItem(title string, fields map[string]string) opItem {
	it := opItem{Title: title, Category: "PASSWORD"}
	if v, ok := fields["password"]; ok {
		it.Fields = append(it.Fields, opField{Type: "CONCEALED", Purpose: "PASSWORD", Value: v})
	}
	var labels []string
	for k := range fields {
		if k == "password" {
			continue
		}
		labels = append(labels, k)
	}
	if len(labels) > 0 {
		// Sorted so the same input produces the same body: an unsorted map
		// iteration would make every write a diff nobody can read.
		sort.Strings(labels)
		sec := opSection{Label: opSectionGenerated}
		for _, k := range labels {
			sec.Fields = append(sec.Fields, opField{Type: "STRING", Label: k, Value: fields[k]})
		}
		it.Sections = append(it.Sections, sec)
	}
	return it
}

// applyFields returns existing with the named fields replaced, leaving anything
// the caller did not name exactly as it is.
func applyFields(existing opItem, fields map[string]string) opItem {
	out := existing
	out.Sections = nil
	merged := map[string]string{}
	for _, s := range existing.Sections {
		if s.Label != opSectionGenerated {
			out.Sections = append(out.Sections, s)
			continue
		}
		for _, f := range s.Fields {
			merged[f.Label] = f.Value
		}
	}
	for k, v := range fields {
		if k == "password" {
			continue
		}
		merged[k] = v
	}
	if len(merged) > 0 {
		var labels []string
		for k := range merged {
			labels = append(labels, k)
		}
		sort.Strings(labels)
		sec := opSection{Label: opSectionGenerated}
		for _, k := range labels {
			sec.Fields = append(sec.Fields, opField{Type: "STRING", Label: k, Value: merged[k]})
		}
		out.Sections = append(out.Sections, sec)
	}
	if v, ok := fields["password"]; ok {
		replaced := false
		out.Fields = nil
		for _, f := range existing.Fields {
			if f.Purpose == "PASSWORD" && !replaced {
				f.Value = v
				replaced = true
			}
			out.Fields = append(out.Fields, f)
		}
		if !replaced {
			out.Fields = append(out.Fields, opField{Type: "CONCEALED", Purpose: "PASSWORD", Value: v})
		}
	}
	return out
}

func setExpiryField(existing opItem, expires string) opItem {
	out := applyFields(existing, map[string]string{opFieldExpires: expires})
	return out
}

// --- transport --------------------------------------------------------------

func (o *OP) doJSON(ctx context.Context, method, url string, tok string, body []byte, secrets []string) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, fmt.Errorf("secrets: building the 1Password request: %s", redact(err.Error(), append([]string{tok}, secrets...)...))
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("secrets: calling 1Password: %s", redact(err.Error(), append([]string{tok}, secrets...)...))
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("secrets: reading 1Password's response: %s", redact(err.Error(), append([]string{tok}, secrets...)...))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		all := append([]string{tok}, secrets...)
		return nil, fmt.Errorf("secrets: 1Password %s %s: %d: %s", method, redact(url, all...), resp.StatusCode, redact(string(respBody), all...))
	}
	return respBody, nil
}

func (o *OP) findVault(ctx context.Context, base, tok string) (string, error) {
	body, err := o.doJSON(ctx, http.MethodGet, base+"/vaults", tok, nil, nil)
	if err != nil {
		return "", err
	}
	var out struct {
		Vaults []opVault `json:"vaults"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("secrets: decoding 1Password's vault list: %s", redact(err.Error(), tok))
	}
	var names []string
	for _, v := range out.Vaults {
		if v.Name == o.cfg.Vault {
			return v.ID, nil
		}
		names = append(names, v.Name)
	}
	return "", fmt.Errorf("secrets: the service account cannot see the vault %q (it returned %s) -- a vault this token cannot read is a grant, not a typo to retry", o.cfg.Vault, strings.Join(names, ", "))
}

func (o *OP) findItem(ctx context.Context, base, tok, vaultID, title string) (opItem, bool, error) {
	body, err := o.doJSON(ctx, http.MethodGet, fmt.Sprintf("%s/vaults/%s/items", base, vaultID), tok, nil, nil)
	if err != nil {
		return opItem{}, false, err
	}
	var list struct {
		Items []opItem `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return opItem{}, false, fmt.Errorf("secrets: decoding 1Password's item list: %s", redact(err.Error(), tok))
	}
	for _, it := range list.Items {
		if it.Title == title {
			full, err := o.getItem(ctx, base, tok, vaultID, it.ID)
			if err != nil {
				return opItem{}, false, err
			}
			return full, true, nil
		}
	}
	return opItem{}, false, nil
}

func (o *OP) getItem(ctx context.Context, base, tok, vaultID, id string) (opItem, error) {
	body, err := o.doJSON(ctx, http.MethodGet, fmt.Sprintf("%s/vaults/%s/items/%s", base, vaultID, id), tok, nil, nil)
	if err != nil {
		return opItem{}, err
	}
	var it opItem
	if err := json.Unmarshal(body, &it); err != nil {
		return opItem{}, fmt.Errorf("secrets: decoding 1Password's item %s: %s", id, redact(err.Error(), tok))
	}
	return it, nil
}

// apiBaseFromToken reads the account host out of the service-account token's
// `iss` claim. No signature check: this is a URL being read out of a credential
// we already hold and are about to send, not an assertion being trusted.
func apiBaseFromToken(tok string) (string, error) {
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return "", fmt.Errorf("secrets: the 1Password token is not a JWT, so its account host cannot be determined")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("secrets: decoding the 1Password token's claims: %s", redact(err.Error(), tok))
	}
	var claims struct {
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return "", fmt.Errorf("secrets: reading the 1Password token's iss: %s", redact(err.Error(), tok))
	}
	host := strings.TrimRight(claims.Iss, "/")
	if host == "" || !strings.HasPrefix(host, "https://") {
		return "", fmt.Errorf("secrets: the 1Password token's iss is %q, which is not an https account host", claims.Iss)
	}
	return host + opAPIPath, nil
}
