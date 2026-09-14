package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/beeradb/truss/internal/forge"
	"github.com/beeradb/truss/internal/keygen"
)

const testPrivateKey = "-----BEGIN OPENSSH PRIVATE KEY-----\nMINTED\n-----END OPENSSH PRIVATE KEY-----\n"

// callLog records what the fakes were asked to do. The assertion that matters
// in this file is ORDER -- "store then register" and "register then store"
// differ by one line and only one of them can leave an unrevocable credential --
// so the log is a sequence rather than a set of counters.
//
// newDeps wires it into both fakes so no test can forget it: the first draft of
// this file passed a pointer field by hand and panicked in three tests, which is
// the shape a shared-by-construction log exists to prevent.
type callLog struct{ calls []string }

func (l *callLog) add(what string) { l.calls = append(l.calls, what) }

func (l *callLog) String() string { return strings.Join(l.calls, ",") }

type fakeLeaf struct {
	log      *callLog
	fields   map[string]string
	found    bool
	putErr   error
	putAfter int // fail this PutValue (1-based); 0 never fails
	puts     int
}

func (f *fakeLeaf) ReadFields(_ context.Context, item string) (map[string]string, bool, error) {
	f.log.add("read")
	if f.fields == nil {
		return nil, false, nil
	}
	return f.fields, f.found, nil
}

func (f *fakeLeaf) PutValue(_ context.Context, item string, fields map[string]string, _ int) error {
	f.log.add("store")
	f.puts++
	if f.putErr != nil {
		return f.putErr
	}
	if f.putAfter != 0 && f.puts == f.putAfter {
		return errors.New("simulated write failure")
	}
	if f.fields == nil {
		f.fields = map[string]string{}
	}
	for k, v := range fields {
		f.fields[k] = v
	}
	f.found = true
	return nil
}

type fakeReg struct {
	log       *callLog
	keys      []forge.DeployKey
	listErr   error
	createErr error
	deleteErr error
	lastTitle string
	created   int64
}

func (f *fakeReg) ListDeployKeys(_ context.Context, _ string) ([]forge.DeployKey, error) {
	f.log.add("list")
	return f.keys, f.listErr
}

func (f *fakeReg) CreateDeployKey(_ context.Context, _, title, publicKey string, _ bool) (forge.DeployKey, error) {
	f.log.add("create")
	f.lastTitle = title
	if f.createErr != nil {
		return forge.DeployKey{}, f.createErr
	}
	f.created++
	return forge.DeployKey{ID: 700 + f.created, Title: title, Key: publicKey}, nil
}

func (f *fakeReg) DeleteDeployKey(_ context.Context, _ string, _ int64) error {
	f.log.add("delete")
	return f.deleteErr
}

func target() deployKeyTarget {
	return deployKeyTarget{Repository: "unsaltedpw/fplarmband.com", Vault: "fpl-runtime",
		Item: "deploy-key-fpl", Comment: "dev-1"}
}

// newDeps builds the pair under test. The mint counter is returned so a test can
// assert nothing was minted, which is the whole point of several of them.
func newDeps(leaf *fakeLeaf, reg *fakeReg, t deployKeyTarget) (*callLog, *int, deployKeyDeps) {
	log := &callLog{}
	leaf.log, reg.log = log, log
	mints := 0
	return log, &mints, deployKeyDeps{
		reg: reg, leaf: leaf, target: t,
		now: func() time.Time { return time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC) },
		mint: func(context.Context) (keygen.Keypair, error) {
			mints++
			return keygen.Keypair{Public: "ssh-ed25519 AAAAMINTED truss-applier", Private: []byte(testPrivateKey)}, nil
		},
	}
}

func TestEnsureDeployKeyIsInertWhenUnconfigured(t *testing.T) {
	log, mints, d := newDeps(&fakeLeaf{}, &fakeReg{}, deployKeyTarget{})
	got, err := ensureDeployKey(context.Background(), d)
	if err != nil || got.Action != "not-configured" {
		t.Fatalf("res=%+v err=%v", got, err)
	}
	if log.String() != "" || *mints != 0 {
		t.Errorf("an unconfigured deployment touched the world: %s", log.String())
	}
}

// TestEnsureDeployKeyStoresBeforeItRegisters is the ordering property. Anything
// that moves Create ahead of the first Store fails here, in review, instead of
// producing a repository full of keys nobody holds.
func TestEnsureDeployKeyStoresBeforeItRegisters(t *testing.T) {
	log, mints, d := newDeps(&fakeLeaf{}, &fakeReg{}, target())
	got, err := ensureDeployKey(context.Background(), d)
	if err != nil {
		t.Fatalf("ensureDeployKey: %v", err)
	}
	if want := "read,list,store,create,store"; log.String() != want {
		t.Errorf("sequence = %q, want %q", log.String(), want)
	}
	if got.Action != "minted" || got.KeyID != "701" {
		t.Errorf("res = %+v", got)
	}
	if *mints != 1 {
		t.Errorf("mints = %d, want 1", *mints)
	}
	if d.reg.(*fakeReg).lastTitle != "truss-applier dev-1" {
		t.Errorf("title = %q, want it to name who made the key", d.reg.(*fakeReg).lastTitle)
	}
}

// TestEnsureDeployKeyNeverRemintsUnderANonEmptyKeyID: a machine is authenticating
// with this credential. Re-minting would overwrite the private half it holds
// while the old public half stays registered.
func TestEnsureDeployKeyNeverRemintsUnderANonEmptyKeyID(t *testing.T) {
	log, mints, d := newDeps(
		&fakeLeaf{fields: map[string]string{"key_id": "55", "expires": "2026-11-01"}, found: true},
		&fakeReg{}, target())
	got, err := ensureDeployKey(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != "already-provisioned" || got.KeyID != "55" || got.Expires != "2026-11-01" {
		t.Errorf("res = %+v", got)
	}
	if *mints != 0 {
		t.Error("a provisioned key was re-minted")
	}
	if log.String() != "read" {
		t.Errorf("beyond the read it did: %s", log.String())
	}
}

// TestEnsureDeployKeyClearsAnOrphanBeforeMinting: an item that exists but
// records no key_id is the residue of a pass that died between store and
// record. Its registered key is unusable and untracked, so it goes before a new
// one is made -- otherwise every retry leaves another credential behind.
func TestEnsureDeployKeyClearsAnOrphanBeforeMinting(t *testing.T) {
	log, _, d := newDeps(
		&fakeLeaf{fields: map[string]string{"public_key": "ssh-ed25519 OLD"}, found: true},
		&fakeReg{keys: []forge.DeployKey{{ID: 12, Title: "truss-applier dev-1"}}}, target())
	if _, err := ensureDeployKey(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if want := "read,list,delete,store,create,store"; log.String() != want {
		t.Errorf("sequence = %q, want %q", log.String(), want)
	}
}

func TestEnsureDeployKeyIgnoresKeysItDidNotMake(t *testing.T) {
	log, _, d := newDeps(
		&fakeLeaf{},
		&fakeReg{keys: []forge.DeployKey{{ID: 12, Title: "someone else's laptop"}}}, target())
	if _, err := ensureDeployKey(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(log.String(), "delete") {
		t.Fatalf("deleted a key it did not create: %s", log.String())
	}
}

// TestEnsureDeployKeyRefusesToRegisterWhenTheStoreFails is the failure the
// ordering exists for: no credential may exist whose private half was never
// written down.
//
// ⚠️ A keypair IS minted by then, and that is fine. The property under test is
// that nothing is REGISTERED -- an unused keypair that never reached a vault is
// random bytes, while a registered key whose half is lost is an unrevocable
// credential. Asserting "no mint" here would encode the wrong invariant and make
// someone "fix" the order that matters.
func TestEnsureDeployKeyRefusesToRegisterWhenTheStoreFails(t *testing.T) {
	log, _, d := newDeps(&fakeLeaf{putErr: errors.New("vault unreachable")}, &fakeReg{}, target())
	_, err := ensureDeployKey(context.Background(), d)
	if err == nil {
		t.Fatal("a failed store still allowed a registration")
	}
	if !strings.Contains(err.Error(), "refusing to register") {
		t.Errorf("err = %v, want the refusal stated plainly", err)
	}
	if strings.Contains(log.String(), "create") {
		t.Errorf("CreateDeployKey ran after the store failed: %s", log.String())
	}
}

// TestEnsureDeployKeyMintsNothingWhenTheListFails: a forge that cannot be
// consulted is a forge that may already hold this title. Reading blindness as
// "no keys" is the mistake CheckRulesets refuses to make about bypass_actors.
func TestEnsureDeployKeyMintsNothingWhenTheListFails(t *testing.T) {
	log, mints, d := newDeps(&fakeLeaf{}, &fakeReg{listErr: errors.New("403")}, target())
	if _, err := ensureDeployKey(context.Background(), d); err == nil {
		t.Fatal("minted without being able to check for a duplicate")
	}
	if *mints != 0 || log.String() != "read,list" {
		t.Errorf("mints=%d sequence=%s, want nothing after the failed list", *mints, log.String())
	}
}

func TestEnsureDeployKeyReportsAnUnregisteredStoredKeyAsInert(t *testing.T) {
	_, _, d := newDeps(&fakeLeaf{}, &fakeReg{createErr: errors.New("403 Resource not accessible by integration")}, target())
	got, err := ensureDeployKey(context.Background(), d)
	if err == nil {
		t.Fatal("a registration failure was reported as success")
	}
	if got.Action != "stored-unregistered" {
		t.Errorf("res = %+v", got)
	}
	// The distinction that matters operationally: a stored, unregistered key
	// authenticates nothing. Saying so keeps a failed pass off an incident
	// path it does not belong on.
	if !strings.Contains(err.Error(), "inert") {
		t.Errorf("err = %v, want it to say the stored key grants nothing", err)
	}
}

// TestEnsureDeployKeyNamesTheOrphanItCouldNotRecord closes the last gap. The key
// IS registered and its id is NOT recorded, so the next pass will delete it as an
// orphan -- the message has to say revoke, not merely that a write failed.
func TestEnsureDeployKeyNamesTheOrphanItCouldNotRecord(t *testing.T) {
	_, _, d := newDeps(&fakeLeaf{putAfter: 2}, &fakeReg{}, target())
	got, err := ensureDeployKey(context.Background(), d)
	if err == nil {
		t.Fatal("an unrecorded id was reported as success")
	}
	if !strings.Contains(err.Error(), "revoke") || !strings.Contains(err.Error(), "701") {
		t.Errorf("err = %v, want the id and the action it needs", err)
	}
	if got.KeyID != "701" {
		t.Errorf("res = %+v, want the id surfaced alongside the failure", got)
	}
}

func TestEnsureDeployKeyReportsAnUndeletableOrphan(t *testing.T) {
	_, mints, d := newDeps(&fakeLeaf{}, &fakeReg{
		keys: []forge.DeployKey{{ID: 12, Title: "truss-applier dev-1"}}, deleteErr: errors.New("403")}, target())
	if _, err := ensureDeployKey(context.Background(), d); err == nil {
		t.Fatal("proceeded alongside a key it could not remove")
	}
	if *mints != 0 {
		t.Error("minted a second key while the first was still registered")
	}
}

// TestDeployKeyResultNeverMarshalsAKey: this struct is what the pass reports.
func TestDeployKeyResultNeverMarshalsAKey(t *testing.T) {
	_, _, d := newDeps(&fakeLeaf{}, &fakeReg{}, target())
	got, err := ensureDeployKey(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"PRIVATE KEY", "MINTED", "ssh-ed25519"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("the reported result carries %q: %s", leak, b)
		}
	}
}

func TestLoadDeployKeyTargetIsAllOrNothing(t *testing.T) {
	full := map[string]string{
		"DEPLOY_KEY_REPOSITORY": "a/b", "DEPLOY_KEY_VAULT": "v",
		"DEPLOY_KEY_ITEM": "i", "DEPLOY_KEY_COMMENT": "c",
	}
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}

	if got, problems := loadDeployKeyTarget(env(map[string]string{})); len(problems) != 0 || got.Repository != "" {
		t.Errorf("an absent configuration was treated as a problem: %+v %v", got, problems)
	}
	if _, problems := loadDeployKeyTarget(env(full)); len(problems) != 0 {
		t.Errorf("a complete configuration was refused: %v", problems)
	}
	for _, drop := range []string{"DEPLOY_KEY_VAULT", "DEPLOY_KEY_ITEM", "DEPLOY_KEY_COMMENT", "DEPLOY_KEY_REPOSITORY"} {
		partial := map[string]string{}
		for k, v := range full {
			if k != drop {
				partial[k] = v
			}
		}
		_, problems := loadDeployKeyTarget(env(partial))
		if len(problems) != 1 {
			t.Fatalf("missing %s produced %v, want exactly one refusal", drop, problems)
		}
		if !strings.Contains(problems[0], "$"+drop) {
			t.Errorf("missing %s: the refusal %q does not name the variable to set", drop, problems[0])
		}
	}
}
