package keygen

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	fakePublic  = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPubkeymaterialplaceholder truss-test"
	fakePrivate = "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEA\n-----END OPENSSH PRIVATE KEY-----\n"
)

// fakeSSHKeygen returns a Run that writes canned key files wherever it is told
// to, and records what it was asked to do.
//
// ⚠️ Nothing here execs anything. A test that shelled out to a real ssh-keygen
// would be measuring the machine, not the code -- it would go red on a box
// without openssh-client and green on one with it, and neither result says
// anything about truss. That is the same mistake as a test that depended on a
// kubectl context named `vault` existing on a laptop.
func fakeSSHKeygen(t *testing.T, pub, priv string, fail error) (Run func(context.Context, string, ...string) ([]byte, []byte, error), args *[][]string) {
	t.Helper()
	recorded := [][]string{}
	run := func(_ context.Context, name string, a ...string) ([]byte, []byte, error) {
		full := append([]string{name}, a...)
		recorded = append(recorded, full)
		if fail != nil {
			return nil, []byte("Saving key \"" + filepath.Join(os.TempDir(), "id_ed25519") + "\" failed"), fail
		}
		out := a[len(a)-1]
		if pub != "" {
			if err := os.WriteFile(out+".pub", []byte(pub+"\n"), 0o644); err != nil {
				t.Fatalf("fake writing public key: %v", err)
			}
		}
		if priv != "" {
			if err := os.WriteFile(out, []byte(priv), 0o600); err != nil {
				t.Fatalf("fake writing private key: %v", err)
			}
		}
		return nil, nil, nil
	}
	return run, &recorded
}

func TestMintReturnsBothHalvesFromOneCall(t *testing.T) {
	run, _ := fakeSSHKeygen(t, fakePublic, fakePrivate, nil)
	got, err := Mint(context.Background(), Config{Comment: "truss@dev-1", Run: run})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if got.Public != fakePublic {
		t.Errorf("Public = %q, want the openssh line the binary wrote", got.Public)
	}
	if string(got.Private) != fakePrivate {
		t.Errorf("Private is not the bytes the binary wrote")
	}
}

// TestMintRefusesAnUnlabelledKeypair: the comment is the only thing a forge's
// own settings page shows beside a key. A key that cannot be attributed is a key
// that cannot be safely revoked, so an empty comment is a refusal, not a default.
func TestMintRefusesAnUnlabelledKeypair(t *testing.T) {
	for _, comment := range []string{"", "   "} {
		run, args := fakeSSHKeygen(t, fakePublic, fakePrivate, nil)
		if _, err := Mint(context.Background(), Config{Comment: comment, Run: run}); err == nil {
			t.Fatalf("an empty Comment was accepted: %q", comment)
		}
		if len(*args) != 0 {
			t.Errorf("Mint ran a subprocess (%v) after deciding to refuse", *args)
		}
	}
}

func TestMintAsksForEd25519WithNoPassphraseAndNoSecretInArgv(t *testing.T) {
	run, args := fakeSSHKeygen(t, fakePublic, fakePrivate, nil)
	if _, err := Mint(context.Background(), Config{Comment: "truss@dev-1", Run: run}); err != nil {
		t.Fatalf("Mint: %v", err)
	}
	call := (*args)[0]
	joined := strings.Join(call, " ")
	for _, want := range []string{"-t ed25519", "-N ", "-C truss@dev-1", "ssh-keygen"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %q lacks %q", joined, want)
		}
	}
	// The empty passphrase is the only argument that could hold a secret, and it
	// must hold none: an /proc or exec-audit log records argv in clear text.
	for i, a := range call {
		if strings.Contains(a, "PRIVATE KEY") {
			t.Errorf("argv[%d] carries key material", i)
		}
	}
}

// TestMintRemovesThePrivateKeyEvenWhenItFails is the reason the cleanup is
// deferred once at the top rather than repeated on each success path. The
// interesting exit is the one where the read or the shape check fails, because
// that is the case where the file is on disk and nobody is looking at it.
func TestMintRemovesThePrivateKeyEvenWhenItFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		pub  string
		priv string
		fail error
	}{
		{"wrong public shape", "ssh-rsa AAAAB3Nza comment", fakePrivate, nil},
		{"no public key", "", fakePrivate, nil},
		{"subprocess failed", fakePublic, "", errors.New("exit status 1")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mintedDir string
			run := func(_ context.Context, _ string, a ...string) ([]byte, []byte, error) {
				path := a[len(a)-1]
				mintedDir = filepath.Dir(path)
				if tc.priv != "" {
					_ = os.WriteFile(path, []byte(tc.priv), 0o600)
				}
				if tc.pub != "" {
					_ = os.WriteFile(path+".pub", []byte(tc.pub+"\n"), 0o644)
				}
				return nil, nil, tc.fail
			}
			if _, err := Mint(context.Background(), Config{Comment: "c", Run: run}); err == nil {
				t.Fatal("expected a refusal")
			}
			if mintedDir == "" {
				t.Fatal("the fake never saw a path")
			}
			if _, err := os.Stat(mintedDir); !os.IsNotExist(err) {
				t.Errorf("%s left %q on disk after failing; key material outlives nothing", tc.name, mintedDir)
			}
		})
	}
}

// TestMintFailureDoesNotEchoKeyMaterial: the stderr of a failed keygen names a
// file, and the private key is the one thing in the vicinity. Both the
// subprocess error and the file bytes are therefore kept out of the returned
// error text.
func TestMintFailureDoesNotEchoKeyMaterial(t *testing.T) {
	run, _ := fakeSSHKeygen(t, fakePublic, fakePrivate, errors.New("exit status 255"))
	_, err := Mint(context.Background(), Config{Comment: "c", Run: run})
	if err == nil {
		t.Fatal("a failing subprocess was reported as success")
	}
	if strings.Contains(err.Error(), "PRIVATE KEY") || strings.Contains(err.Error(), "OPENSSH") {
		t.Errorf("error text carries key material: %v", err)
	}
	if !strings.Contains(err.Error(), "ssh-keygen") {
		t.Errorf("error %q does not name the binary that failed", err)
	}
}

func TestProbeRefusesABinaryThatIsNotThere(t *testing.T) {
	err := Probe(context.Background(), "definitely-not-ssh-keygen-on-this-box")
	if err == nil {
		t.Fatal("Probe accepted a missing binary")
	}
	// The refusal has to name the thing to install, because the answer is an
	// image change and the person reading this is not the one who wrote it.
	if !strings.Contains(err.Error(), "openssh-client") {
		t.Errorf("error %q does not name the package that provides it", err)
	}
}
