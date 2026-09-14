package forge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	testPubA = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAAPublicKeyA truss-applier"
	testPubB = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAAPublicKeyB truss-applier"
)

// keysServer answers only the deploy-key endpoints, failing loudly on anything
// else so a path built wrongly shows up as a 404 rather than a pass.
func keysServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations/7654321/access_tokens":
			mintHandler("ghs_keys")(w, r)
		case strings.HasSuffix(r.URL.Path, "/keys"):
			handler(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestListDeployKeysDecodesEveryField(t *testing.T) {
	srv := keysServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("list used %s, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"key_id":11,"title":"dev-1","key":"`+testPubA+`","readonly":false},
		                           {"id":12,"title":"old","key":"`+testPubB+`","readonly":true}]`)
	})
	c := newTestClient(t, srv.URL)
	keys, err := c.ListDeployKeys(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatalf("ListDeployKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("got %d keys, want 2", len(keys))
	}
	// key_id on the first, plain id on the second: GitHub uses both shapes
	// across these endpoints and neither may be assumed.
	if keys[0].ID != 11 || keys[1].ID != 12 {
		t.Errorf("ids = %d, %d, want 11 and 12 from key_id and id respectively", keys[0].ID, keys[1].ID)
	}
	if keys[0].ReadOnly == nil || *keys[0].ReadOnly != false {
		t.Errorf("key 0 readonly = %v, want an explicit false", keys[0].ReadOnly)
	}
	if keys[1].ReadOnly == nil || *keys[1].ReadOnly != true {
		t.Errorf("key 1 readonly = %v, want true", keys[1].ReadOnly)
	}
}

// TestReadOnlyAbsentIsNotFalse is this package's first rule applied to a write
// target. A response that omits `readonly` has not promised the key cannot
// push, and a bool would have said that anyway -- the dangerous direction, on
// the one field that decides whether this credential writes.
func TestReadOnlyAbsentIsNotFalse(t *testing.T) {
	srv := keysServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"key_id":7,"title":"t","key":"`+testPubA+`"}]`)
	})
	c := newTestClient(t, srv.URL)
	keys, err := c.ListDeployKeys(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatalf("ListDeployKeys: %v", err)
	}
	if keys[0].ReadOnly != nil {
		t.Fatalf("ReadOnly = %v, want nil for a response that never mentioned it", *keys[0].ReadOnly)
	}
}

func TestADeployKeyWithNeitherIDIsARefusal(t *testing.T) {
	srv := keysServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"title":"t","key":"`+testPubA+`"}]`)
	})
	c := newTestClient(t, srv.URL)
	_, err := c.ListDeployKeys(context.Background(), "acme/widgets")
	if err == nil {
		t.Fatal("a deploy key with no id was accepted")
	}
	// An id is the only way back out. Reporting such a key as usable would
	// produce a credential that can never be revoked.
	if !strings.Contains(err.Error(), "neither key_id nor id") {
		t.Errorf("error %q does not say what is missing", err)
	}
}

func TestCreateDeployKeyPostsTheBodyGitHubAsksFor(t *testing.T) {
	var gotPath, gotCT string
	var gotBody map[string]any
	var gotAuth string
	srv := keysServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotCT, gotAuth = r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"key_id":99,"title":"dev-1","key":"`+testPubA+`","readonly":false}`)
	})
	c := newTestClient(t, srv.URL)
	dk, err := c.CreateDeployKey(context.Background(), "acme/widgets", "dev-1", testPubA, false)
	if err != nil {
		t.Fatalf("CreateDeployKey: %v", err)
	}
	if gotPath != "/repos/acme/widgets/keys" {
		t.Errorf("posted to %q", gotPath)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q; a POST without it is answered 415 and reads like a permissions failure", gotCT)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotBody["key"] != testPubA || gotBody["title"] != "dev-1" || gotBody["readonly"] != false {
		t.Errorf("body = %v", gotBody)
	}
	if dk.ID != 99 {
		t.Errorf("ID = %d, want the id GitHub assigned so the key stays revocable", dk.ID)
	}
}

// TestA403NamesTheStatusAndBody: 403 here means the wrong App is in use, not
// that the code is broken -- see the file comment. That distinction only holds
// if the status and GitHub's own message survive into the error.
func TestA403NamesTheStatusAndBody(t *testing.T) {
	srv := keysServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"message":"Resource not accessible by integration","status":"403"}`)
	})
	c := newTestClient(t, srv.URL)
	_, err := c.ListDeployKeys(context.Background(), "acme/widgets")
	if err == nil {
		t.Fatal("a 403 was reported as success")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "Resource not accessible by integration") {
		t.Errorf("error %q does not carry the status and GitHub's reason", err)
	}
	if strings.Contains(err.Error(), "ghs_keys") {
		t.Errorf("error leaked the installation token: %v", err)
	}
}

// TestFindDeployKeyByPublicMatchesOnKeyNotTitle is what makes a retried mint
// safe. Titles are labels and get reused; the public key is the identity.
func TestFindDeployKeyByPublicMatchesOnKeyNotTitle(t *testing.T) {
	srv := keysServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"key_id":1,"title":"truss-applier dev-1","key":"`+testPubB+`","readonly":false}]`)
	})
	c := newTestClient(t, srv.URL)
	ctx := context.Background()

	if _, found, err := c.FindDeployKeyByPublic(ctx, "acme/widgets", testPubA+"  \n"); err != nil || found {
		t.Fatalf("a different key with a similar title matched: found=%v err=%v", found, err)
	}
	got, found, err := c.FindDeployKeyByPublic(ctx, "acme/widgets", testPubB)
	if err != nil || !found || got.ID != 1 {
		t.Fatalf("exact key did not match: found=%v id=%d err=%v", found, got.ID, err)
	}
}

func TestCreateDeployKeyRefusesBeforeSendingAnything(t *testing.T) {
	called := false
	srv := keysServer(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
	})
	c := newTestClient(t, srv.URL)
	ctx := context.Background()

	for _, tc := range []struct{ name, repo, title, key string }{
		{"empty title", "acme/widgets", "", testPubA},
		{"empty key", "acme/widgets", "t", "   "},
		{"repo without owner", "widgets", "t", testPubA},
		{"path traversal", "..\\evil", "t", testPubA},
	} {
		if _, err := c.CreateDeployKey(ctx, tc.repo, tc.title, tc.key, false); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
	if called {
		t.Error("a refused call still reached the forge")
	}
}
