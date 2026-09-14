package secrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	opSecret  = "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaA\n-----END OPENSSH PRIVATE KEY-----"
	opPubLine = "ssh-ed25519 AAAAC3Nza truss-applier"
)

// opFake is a stand-in for 1Password's Developer API. It records every request
// so the assertions are about what went on the wire, not about internal state.
type opFake struct {
	t          *testing.T
	vaultID    string
	items      map[string]opItem
	requests   []string
	lastBody   string
	lastRawURL string
	nextID     int
}

func newOPFake(t *testing.T) (*opFake, *httptest.Server) {
	t.Helper()
	f := &opFake{t: t, vaultID: "vlt001", items: map[string]opItem{}, nextID: 90}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		f.lastBody, f.lastRawURL = string(raw), r.URL.String()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/developer/api/v2/vaults":
			_ = json.NewEncoder(w).Encode(map[string]any{"vaults": []opVault{{ID: f.vaultID, Name: "fpl-runtime"}, {ID: "vlt002", Name: "platform"}}})

		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/items"):
			var list []opItem
			for _, it := range f.items {
				list = append(list, opItem{ID: it.ID, Title: it.Title, Version: it.Version})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": list})

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/items"):
			var it opItem
			_ = json.Unmarshal([]byte(raw), &it)
			f.nextID++
			it.ID = fmt.Sprintf("itm%03d", f.nextID)
			it.Version = 1
			f.items[it.Title] = it
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(it)

		case r.Method == http.MethodPut:
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			var it opItem
			_ = json.Unmarshal([]byte(raw), &it)
			for title, existing := range f.items {
				if existing.ID == id {
					it.ID, it.Title = existing.ID, title
					it.Version = existing.Version + 1
					f.items[title] = it
					_ = json.NewEncoder(w).Encode(it)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "no such item"})

		case r.Method == http.MethodGet:
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			for _, it := range f.items {
				if it.ID == id {
					_ = json.NewEncoder(w).Encode(it)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "no such item"})
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

// opTestClient returns an OP whose token names this fake server as its account
// host. The token is JWT-shaped with only what apiBaseFromToken reads, so the
// derivation is exercised rather than stubbed.
func opTestClient(t *testing.T, srv *httptest.Server, writable string) *OP {
	t.Helper()
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"` + srv.URL + `"}`))
	tok := "at." + payload + ".sig"
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte(tok), 0o600); err != nil {
		t.Fatal(err)
	}
	o, err := NewOP(OPConfig{Vault: "fpl-runtime", TokenFile: path, WritableItem: writable, HTTP: srv.Client()})
	if err != nil {
		t.Fatalf("NewOP: %v", err)
	}
	return o
}

func TestPutValueRefusesOnAReadOnlyStore(t *testing.T) {
	f, srv := newOPFake(t)
	o := opTestClient(t, srv, "")
	if err := o.PutValue(context.Background(), "deploy-key", map[string]string{"password": opSecret}, -1); err == nil {
		t.Fatal("a store with no WritableItem wrote anyway")
	}
	if len(f.requests) != 0 {
		t.Errorf("a refused write still reached the network: %v", f.requests)
	}
}

func TestPutValueRefusesAnItemOtherThanItsOwn(t *testing.T) {
	f, srv := newOPFake(t)
	o := opTestClient(t, srv, "deploy-key-fpl")
	err := o.PutValue(context.Background(), "registry-pull", map[string]string{"password": opSecret}, -1)
	if err == nil || !strings.Contains(err.Error(), "deploy-key-fpl") {
		t.Fatalf("err = %v, want a refusal naming the one writable item", err)
	}
	if len(f.requests) != 0 {
		t.Errorf("refused before the network? requests = %v", f.requests)
	}
}

func TestPutValueCreatesWithTheSecretInTheBodyOnly(t *testing.T) {
	f, srv := newOPFake(t)
	o := opTestClient(t, srv, "deploy-key-fpl-armband")
	fields := map[string]string{"password": opSecret, "public_key": opPubLine, "repository": "beeradb/FPL-Armband"}
	if err := o.PutValue(context.Background(), "deploy-key-fpl-armband", fields, -1); err != nil {
		t.Fatalf("PutValue: %v", err)
	}

	// The vault name had to be resolved to an id first: no URL here carries a
	// vault *name*, so a typo in config cannot silently write somewhere else.
	if f.requests[0] != "GET /developer/api/v2/vaults" {
		t.Errorf("first request = %q, want the vault list", f.requests[0])
	}
	var posted bool
	for _, r := range f.requests {
		if strings.HasPrefix(r, "POST ") {
			posted = true
		}
	}
	if !posted {
		t.Fatalf("no create was posted; requests = %v", f.requests)
	}
	if !strings.Contains(f.lastBody, "OPENSSH PRIVATE KEY") {
		t.Errorf("the body does not carry the value: %s", f.lastBody)
	}
	if strings.Contains(f.lastRawURL, "OPENSSH") || strings.Contains(f.lastRawURL, "ssh-ed25519") {
		t.Errorf("a secret or key appears in the URL: %s", f.lastRawURL)
	}
	var got opItem
	if err := json.Unmarshal([]byte(f.lastBody), &got); err != nil {
		t.Fatalf("decoding the posted body: %v", err)
	}
	if len(got.Fields) != 1 || got.Fields[0].Purpose != "PASSWORD" {
		t.Errorf("the secret must sit in the category's own password field (1Password rejects a PASSWORD item with none): %+v", got.Fields)
	}
	if len(got.Sections) != 1 || len(got.Sections[0].Fields) != 2 {
		t.Errorf("non-secret fields should be one sorted section, got %+v", got.Sections)
	}
	if got.Sections[0].Fields[0].Label != "public_key" || got.Sections[0].Fields[1].Label != "repository" {
		t.Errorf("section fields not sorted: %+v", got.Sections[0].Fields)
	}
}

func TestPutValueUpdatesInPlaceAndKeepsWhatItWasNotToldToTouch(t *testing.T) {
	f, srv := newOPFake(t)
	o := opTestClient(t, srv, "deploy-key-fpl-armband")
	ctx := context.Background()
	if err := o.PutValue(ctx, "deploy-key-fpl-armband", map[string]string{"password": opSecret, "key_id": "41"}, -1); err != nil {
		t.Fatalf("create: %v", err)
	}
	before := len(f.requests)
	if err := o.PutValue(ctx, "deploy-key-fpl-armband", map[string]string{"password": "rotated"}, -1); err != nil {
		t.Fatalf("update: %v", err)
	}
	var put bool
	for _, r := range f.requests[before:] {
		if strings.HasPrefix(r, "PUT ") {
			put = true
		}
	}
	if !put {
		t.Fatalf("an existing item was not updated via PUT; requests = %v", f.requests[before:])
	}
	saved := f.items["deploy-key-fpl-armband"]
	var keyID string
	for _, s := range saved.Sections {
		for _, fl := range s.Fields {
			if fl.Label == "key_id" {
				keyID = fl.Value
			}
		}
	}
	if keyID != "41" {
		t.Errorf("key_id was lost by a write that never named it: %q", keyID)
	}
}

// TestPutValueHonoursTheVersionItRead is CAS with teeth: two passes that both
// minted a key would otherwise have the later one quietly destroy the private
// half of the key that is actually registered.
func TestPutValueHonoursTheVersionItRead(t *testing.T) {
	f, srv := newOPFake(t)
	o := opTestClient(t, srv, "deploy-key-fpl-armband")
	ctx := context.Background()
	if err := o.PutValue(ctx, "deploy-key-fpl-armband", map[string]string{"password": opSecret}, -1); err != nil {
		t.Fatal(err)
	}
	before := len(f.requests)
	err := o.PutValue(ctx, "deploy-key-fpl-armband", map[string]string{"password": "other"}, 999)
	if err == nil {
		t.Fatal("a cas that does not match the item's version was accepted")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Errorf("err %q should name the version conflict", err)
	}
	for _, r := range f.requests[before:] {
		if strings.HasPrefix(r, "PUT ") {
			t.Error("a cas mismatch still wrote")
		}
	}
}

func TestOPPatchExpiryCannotCreateAnItem(t *testing.T) {
	_, srv := newOPFake(t)
	o := opTestClient(t, srv, "deploy-key-fpl-armband")
	err := o.PatchExpiry(context.Background(), "deploy-key-fpl-armband", "2026-11-01")
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("err = %v, want a refusal to invent a credential slot", err)
	}
}

func TestPatchExpirySetsTheFieldAndNothingElse(t *testing.T) {
	f, srv := newOPFake(t)
	o := opTestClient(t, srv, "deploy-key-fpl-armband")
	ctx := context.Background()
	if err := o.PutValue(ctx, "deploy-key-fpl-armband", map[string]string{"password": opSecret}, -1); err != nil {
		t.Fatal(err)
	}
	if err := o.PatchExpiry(ctx, "deploy-key-fpl-armband", "2026-11-01"); err != nil {
		t.Fatalf("PatchExpiry: %v", err)
	}
	saved := f.items["deploy-key-fpl-armband"]
	if len(saved.Fields) != 1 || saved.Fields[0].Value != opSecret {
		t.Errorf("the credential value was altered by an expiry write: %+v", saved.Fields)
	}
	var exp string
	for _, s := range saved.Sections {
		for _, fl := range s.Fields {
			if fl.Label == "expires" {
				exp = fl.Value
			}
		}
	}
	if exp != "2026-11-01" {
		t.Errorf("expires = %q", exp)
	}
}

func TestErrorsNeverCarryTheTokenOrTheValue(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		// A server that echoed the request body back is the leak case: 1Password
		// is not expected to do this, and the guard is cheap.
		b, _ := io.ReadAll(r.Body)
		_, _ = w.Write([]byte("denied: " + string(b)))
	}))
	t.Cleanup(srv.Close)
	o := opTestClient(t, srv, "deploy-key-fpl-armband")
	err := o.PutValue(context.Background(), "deploy-key-fpl-armband", map[string]string{"password": opSecret}, -1)
	if err == nil {
		t.Fatal("a 403 was reported as success")
	}
	if strings.Contains(err.Error(), "at.") {
		t.Errorf("the service account token appears in the error: %v", err)
	}
	if strings.Contains(err.Error(), "OPENSSH") {
		t.Errorf("the minted private key appears in the error: %v", err)
	}
}

func TestVaultTheTokenCannotSeeIsNamedNotGuessed(t *testing.T) {
	_, srv := newOPFake(t)
	o := opTestClient(t, srv, "deploy-key-fpl-armband")
	o.cfg.Vault = "recipes-runtime" // real, but not in the fake's list
	err := o.PutValue(context.Background(), "deploy-key-fpl-armband", map[string]string{"password": opSecret}, -1)
	if err == nil || !strings.Contains(err.Error(), "recipes-runtime") {
		t.Fatalf("err = %v, want the unreadable vault named", err)
	}
	if !strings.Contains(err.Error(), "fpl-runtime") {
		t.Errorf("the error should also list what the token can see, so 'wrong vault' is distinguishable from 'no grant': %v", err)
	}
}

func TestAPIDerivesTheHostFromTheToken(t *testing.T) {
	for _, tc := range []struct {
		name, tok string
		ok        bool
	}{
		{"good", jwtWithIss(t, "https://acme.apt.1password.com"), true},
		{"not a jwt", "opaque-token", false},
		{"plain http iss", jwtWithIss(t, "http://acme.apt.1password.com"), false},
		{"no iss", base64.RawURLEncoding.EncodeToString([]byte("{}")) + " padding", false},
	} {
		_, err := apiBaseFromToken(tc.tok)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
	got, err := apiBaseFromToken(jwtWithIss(t, "https://acme.apt.1password.com/"))
	if err != nil || got != "https://acme.apt.1password.com"+opAPIPath {
		t.Errorf("base = %q, err = %v; a trailing slash in iss must not double the path", got, err)
	}
}

func jwtWithIss(t *testing.T, iss string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"iss": iss})
	if err != nil {
		t.Fatal(err)
	}
	return "hdr." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}
