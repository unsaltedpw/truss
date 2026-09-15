// Package keygen mints SSH keypairs by asking OpenSSH to mint them.
//
// ⚠️ THE POINT OF THIS PACKAGE IS THAT IT CONTAINS NO KEY FORMAT. OpenSSH's
// private key container is a format with a magic, a cipher name, a KDF name and
// a padding rule; the moment a passphrase is wanted it becomes bcrypt-pbkdf plus
// a MAC. None of that is reimplemented here, because a keypair whose container is
// subtly wrong is not rejected where it is made -- it is rejected on the machine
// that receives it, during a rotation, while nobody is watching. `ssh-keygen`
// produces the format OpenSSH reads, by construction.
//
// Nor does this package speak SSH. truss never opens a connection: git on the
// receiving box uses the system ssh client. What truss does is produce two bytes
// -- a public line to register with a forge and a private file to store -- and
// both come from the same subprocess.
//
// The dependency direction is deliberate: internal/keygen knows about a binary
// and a filesystem, and nothing about any forge. Registering the public half is
// internal/forge's job, so a forge that provisions credentials another way (a
// GitLab deploy key, an authorized_keys line) changes nothing here.
package keygen

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Keypair is one minted Ed25519 keypair.
type Keypair struct {
	// Public is the OpenSSH one-line form, `ssh-ed25519 AAAA... <comment>`,
	// exactly what a forge's deploy-key API takes.
	Public string
	// Private is the OpenSSH private key file's bytes, including the
	// `-----BEGIN OPENSSH PRIVATE KEY-----` line. It is a secret: never put it
	// in an error, a log line, or a metric label. Callers pass it onward to
	// exactly one sink.
	Private []byte
}

// Config is everything Mint needs.
type Config struct {
	// Bin is the ssh-keygen binary. Empty means "ssh-keygen", resolved through
	// PATH the way exec.Command always does -- the same convention
	// secrets.OPConfig.Bin uses for `op`, and for the same reason: production
	// pins the path, tests do not need a binary at all.
	Bin string

	// Comment is the key's trailing comment, which is also the only thing a
	// forge shows beside a key in its own settings page. Mint nothing without
	// one: an unlabelled key cannot be attributed when it is found later.
	Comment string

	// Run starts the subprocess and returns its stdout, stderr and error. Nil
	// means the real exec. Tests set this to a fake that writes canned files and
	// starts no process -- which is not a convenience: a test that shelled out
	// to a real ssh-keygen would pass or fail according to what happens to be
	// installed on the machine running it, and scripts/check is run on more than
	// one kind of box.
	Run func(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)
}

// Probe reports whether the binary is usable, without generating anything.
//
// ⚠️ Called at startup, not at the moment of a mint, and the reason is
// discoverability. Nothing in the applier's image installs openssh-client
// today, so `ssh-keygen` is not on PATH until whatever wires this package in
// adds it back -- refusing early, with the binary named, is the difference
// between a lint finding at startup and a failed rotation weeks later, in a
// message about a subprocess.
func Probe(ctx context.Context, bin string) error {
	if bin == "" {
		bin = "ssh-keygen"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return fmt.Errorf("keygen: %q is not usable (%s); this image is expected to carry openssh-client", bin, err)
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("keygen: %q resolved to %q, which cannot be stat'd: %w", bin, path, err)
	}
	return nil
}

// Mint generates one Ed25519 keypair and returns it without ever letting it live
// anywhere longer than the call.
func Mint(ctx context.Context, cfg Config) (Keypair, error) {
	if strings.TrimSpace(cfg.Comment) == "" {
		return Keypair{}, fmt.Errorf("keygen: refusing to mint an unlabelled keypair: Comment is empty")
	}
	bin := cfg.Bin
	if bin == "" {
		bin = "ssh-keygen"
	}
	run := cfg.Run
	if run == nil {
		run = execRun
	}

	// 0700 before anything exists in it, so the private file is never created
	// somewhere another user could open it, not even for the instant between
	// creation and the read below.
	dir, err := os.MkdirTemp("", "truss-keygen-")
	if err != nil {
		return Keypair{}, fmt.Errorf("keygen: cannot create a private directory to mint into: %w", err)
	}
	// Deferred once, before any error path returns: every exit from this
	// function removes the directory, because the alternative is a private key
	// left on disk by the one code path that failed.
	defer os.RemoveAll(dir)

	if err := os.Chmod(dir, 0o700); err != nil {
		return Keypair{}, fmt.Errorf("keygen: cannot secure the mint directory: %w", err)
	}

	priv := filepath.Join(dir, "id_ed25519")
	// -N '' is an empty passphrase, and it is the only sensible answer here:
	// nobody is going to type a passphrase into an unattended rotation. The
	// secret's protection is the vault it is written to, not a key on this file.
	// Note that argv carries a path and an empty string, never a secret value.
	// -q suppresses ssh-keygen's own progress output, so stdout is discarded:
	// nothing here decides anything from it.
	_, stderr, err := run(ctx, bin, "-q", "-t", "ed25519", "-N", "", "-C", cfg.Comment, "-f", priv)
	if err != nil {
		return Keypair{}, fmt.Errorf("keygen: %s failed: %w: %s", bin, err, redact(strings.TrimSpace(string(stderr))))
	}

	private, err := os.ReadFile(priv)
	if err != nil {
		return Keypair{}, fmt.Errorf("keygen: %s wrote no private key at the path it was given: %w", bin, err)
	}
	public, err := os.ReadFile(priv + ".pub")
	if err != nil {
		return Keypair{}, fmt.Errorf("keygen: %s wrote no public key beside the private one: %w", bin, err)
	}

	line := strings.TrimSpace(string(public))
	if !strings.HasPrefix(line, "ssh-ed25519 ") {
		return Keypair{}, fmt.Errorf("keygen: the public key is not an openssh ed25519 line (it begins %q)", firstWord(line))
	}
	if len(private) == 0 {
		return Keypair{}, fmt.Errorf("keygen: %s produced an empty private key", bin)
	}
	return Keypair{Public: line, Private: private}, nil
}

// execRun is the real seam: a subprocess, with no value in argv and no output
// captured into anything that outlives the call.
func execRun(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	return []byte(out.String()), []byte(errBuf.String()), err
}

// redact keeps a stray path fragment from being quoted back in full and, more
// importantly, makes the shape of this error consistent with the rest of the
// project, where no error may carry a secret. ssh-keygen's stderr names files,
// not key material; the assumption is cheap to hold and expensive to be wrong
// about, so the value is truncated rather than trusted.
func redact(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	const keep = 200
	if len(s) > keep {
		return s[:keep] + "…"
	}
	return s
}

func firstWord(s string) string {
	fields := strings.Fields(s)
	if len(fields) > 2 {
		fields = fields[:2]
	}
	return strings.Join(fields, " ")
}
