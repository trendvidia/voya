// Package auth is voya's local identity provider (ADR 006): the signing key
// it keeps between runs, the JWK Set the daemon serves for it under
// voya.internal (#209), and the minting `voya token` prints (#210) — here
// rather than in the daemon, so the CLI signs without `voya run`. The schema
// for the rest of it — issuer, audience, personas — is voya.config.v1.Auth,
// resolved by internal/config.
//
// The key is the first thing voya writes to the configuration directory,
// which it otherwise only reads. ADR 004 refused on-disk state for the
// registry because a stale copy outlives the process that made it; the key
// is the opposite case — a key regenerated every run would silently
// invalidate every token a developer already pasted into a terminal, and the
// 401 that follows reads as a bug in the service. ADR 006's amendment of
// 2026-08-19 records the exemption.
package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

const (
	// KeyFile is the signing key's name inside the configuration directory
	// — beside voya.pxf, so that `$VOYA_CONFIG_DIR` moves both together and
	// two configurations never share a key.
	KeyFile = "signing-key.pem"

	// KeyMode is the only mode a key file may have: readable and writable
	// by its owner, by nobody else. A signing key readable by other local
	// users is ADR 006's sharp edge pointed at the developer's own machine,
	// so [LoadOrGenerate] refuses a wider file rather than reading it.
	KeyMode fs.FileMode = 0o600

	// dirMode is what the configuration directory is created with when it
	// does not exist yet. 0700 is what XDG prescribes for the base
	// directories and is what the key needs; a directory that already
	// exists keeps whatever mode it has.
	dirMode fs.FileMode = 0o700

	// keyBits is the RSA modulus size. RS256 rather than anything newer
	// because the point of the key is that third-party JOSE libraries
	// validate against it unmodified, and RS256 is the one algorithm every
	// one of them implements; 2048 is the floor those libraries accept.
	keyBits = 2048

	// pemType is the PEM block the key is stored as: PKCS #8, the form
	// `openssl pkey` and every JOSE library read without being told the
	// algorithm.
	pemType = "PRIVATE KEY"
)

// ErrNoDir is returned when no configuration directory is known — [Dir] in
// internal/config answered "" because the home directory could not be
// determined. A key must live somewhere a later run finds it again, and the
// process working directory is not that, so the answer is an error and never
// a key in the CWD.
var ErrNoDir = errors.New("no configuration directory: the home directory could not be determined; set $VOYA_CONFIG_DIR")

// LoadOrGenerate returns the signing key in dir, generating it on first use.
// dir is the configuration directory — callers pass config.Dir() — and may
// not exist yet: voya runs with no configuration at all, so the directory is
// created on the way. The key is written at [KeyMode] and read back on every
// later call; a file that is wider than that, or that is not a key, is
// refused with a message naming the path, since the remedy is to fix or
// delete it.
//
// Two processes generating at once — `voya run` and `voya token` in another
// terminal on a fresh machine — each write their own key to a private
// temporary file and then link it into place; the link is atomic and fails
// for whichever comes second, which then discards its key and reads the
// winner's. No path leaves a half-written [KeyFile].
//
// Every error is either [ErrNoDir] or one that names the path; none of them
// satisfies errors.Is(err, fs.ErrNotExist), so a caller can never read a
// refusal as "generate one" and loop.
func LoadOrGenerate(dir string) (*rsa.PrivateKey, error) {
	if dir == "" {
		return nil, ErrNoDir
	}
	path := filepath.Join(dir, KeyFile)
	euid := os.Geteuid()

	key, err := load(path, euid)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	if err := generate(dir, path); err != nil {
		return nil, err
	}
	// Read back what is on disk rather than returning the key this process
	// made: if another process won the link, the file is its key, and the
	// read-back is also the proof that what was written is loadable.
	key, err = load(path, euid)
	if errors.Is(err, fs.ErrNotExist) {
		// The name is taken by something os.Stat cannot see and os.Link
		// would not overwrite — a dangling symlink is the usual way, the
		// link checked out of a dotfiles repo and the secret it points at
		// not — or another process removed the key between the link and
		// this read. Returning fs.ErrNotExist here would hand the caller a
		// bare "no such file" with no key and no remedy, and a caller that
		// retried on it would generate for ever.
		return nil, fmt.Errorf("auth: signing key %s is not there after voya placed one — a dangling symlink at that path, or another process deleting the key; move it aside and voya will generate one", path)
	}
	return key, err
}

// load reads and parses the key at path, refusing a file that is not
// exactly a private key, at exactly [KeyMode], owned by euid. The error for
// a missing file satisfies errors.Is(err, fs.ErrNotExist) and nothing else
// does, so the caller can tell "generate one" from "refuse to".
//
// The effective uid is a parameter rather than read here so a test can ask
// what load does with a key it does not own; the only caller passes
// os.Geteuid().
func load(path string, euid int) (*rsa.PrivateKey, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("auth: signing key %s: %w", path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("auth: signing key %s is a directory — move it aside and voya will generate a key", path)
	}
	if err := checkMode(path, info.Mode()); err != nil {
		return nil, err
	}
	if err := checkOwner(path, info, euid); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path) //nolint:gosec // G304: path is KeyFile under the configuration directory, which is $VOYA_CONFIG_DIR or the home directory by design.
	if err != nil {
		return nil, fmt.Errorf("auth: signing key %s: %w", path, err)
	}
	key, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("auth: signing key %s is not a key (%w) — delete it and voya will generate a new one; tokens already minted with the old key stop validating", path, err)
	}
	return key, nil
}

// checkMode is the 0600 rule. On Windows the permission bits Go reports are
// synthesised from the read-only attribute — 0666 or 0444, never 0600 — and
// the owner-only protection comes from the profile directory's ACL, so the
// check would refuse every key there and is skipped.
func checkMode(path string, mode fs.FileMode) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if perm := mode.Perm(); perm&^KeyMode != 0 {
		return fmt.Errorf("auth: signing key %s has mode %#o, wider than %#o — a key readable by other users signs tokens for them too; chmod %o %s, or delete it and voya will generate a new one",
			path, perm, KeyMode, KeyMode, path)
	}
	return nil
}

// checkOwner is the other half of the 0600 rule: mode bits only protect the
// key if voya is the user they name. A configuration directory that voya did
// not create can be group- or world-writable — `$VOYA_CONFIG_DIR=/tmp/voya`
// on a shared box, or a `~/.config` some other tool made 0755 — and another
// local user can then leave a 0600 key of their own there for voya to sign
// every token with. Off unix [fileOwner] has no answer and the check passes;
// on Windows the profile directory's ACL is what protects the file, as in
// [checkMode].
func checkOwner(path string, info fs.FileInfo, euid int) error {
	uid, known := fileOwner(info)
	if !known || uid == euid {
		return nil
	}
	return fmt.Errorf("auth: signing key %s is owned by uid %d, not by uid %d running voya — a key another user owns is a key they can replace, and voya would sign tokens with theirs; move it aside and voya will generate one",
		path, uid, euid)
}

// parse decodes one PEM-encoded PKCS #8 RSA private key.
func parse(data []byte) (*rsa.PrivateKey, error) {
	if len(data) == 0 {
		return nil, errors.New("the file is empty")
	}
	block, rest := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	if block.Type != pemType {
		return nil, fmt.Errorf("PEM block is %q, want %q", block.Type, pemType)
	}
	if len(rest) != 0 {
		return nil, errors.New("trailing data after the PEM block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%T, want an RSA key", parsed)
	}
	return key, nil
}

// generate makes a key and puts it at path, creating dir as needed. The key
// is written to a private temporary file in dir first and then linked into
// place: os.Link fails if path already exists, which is what makes two
// concurrent generators converge on one key instead of the second
// overwriting the first's after the first has handed it out.
func generate(dir, path string) error {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("auth: creating %s for the signing key: %w", dir, err)
	}

	key, err := rsa.GenerateKey(rand.Reader, keyBits)
	if err != nil {
		return fmt.Errorf("auth: generating signing key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("auth: encoding signing key: %w", err)
	}
	data := pem.EncodeToMemory(&pem.Block{Type: pemType, Bytes: der})

	tmp, err := os.CreateTemp(dir, "."+KeyFile+".*")
	if err != nil {
		return fmt.Errorf("auth: writing signing key in %s: %w", dir, err)
	}
	// Remove the temporary on every exit: after a successful link the key
	// is reachable by its own name and the temporary is just a second one;
	// on every other path it is the half-written file this design exists to
	// never leave behind. A failure to remove it is not a failure to hand
	// out the key, so it is not reported.
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := writeAndClose(tmp, data); err != nil {
		return fmt.Errorf("auth: writing signing key %s: %w", tmp.Name(), err)
	}

	switch err := os.Link(tmp.Name(), path); {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrExist):
		// Lost the race to another process; its key is the one. The
		// caller reads it back.
		return nil
	default:
		// os.Link is what makes the placement atomic and refuse to
		// clobber, so a filesystem without hard links — a FAT32 stick,
		// some network shares and bind mounts — cannot hold the key at
		// all, and the errno alone (EPERM, EOPNOTSUPP) says nothing about
		// why. The remedy is the only knob there is.
		return fmt.Errorf("auth: placing signing key at %s: %w — the configuration directory must be on a filesystem that supports hard links; point $VOYA_CONFIG_DIR at one that does", path, err)
	}
}

// writeAndClose sets the key's mode before a byte of it is written —
// CreateTemp already opens at 0600, but the umask is not what decides that
// and saying so explicitly costs one syscall — then writes, syncs and
// closes. Any failure leaves the temporary file for the caller's deferred
// remove; the close error on the failure paths is dropped because the write
// error it would mask is the one that matters.
func writeAndClose(f *os.File, data []byte) (err error) {
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	if err := f.Chmod(KeyMode); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}
