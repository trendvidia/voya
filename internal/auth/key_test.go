package auth

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// TestFirstUseGeneratesAndLaterUsesReuse is the ADR 006 contract: one key per
// configuration directory, made on first use and the same on every later
// one, at 0600, in a directory that did not exist yet — voya runs with no
// configuration at all, and the key is the first thing it writes there.
func TestFirstUseGeneratesAndLaterUsesReuse(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "xdg", "voya")
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("precondition: %s exists before the first use (stat err = %v)", dir, err)
	}

	first, err := LoadOrGenerate(dir)
	if err != nil {
		t.Fatalf("first use: %v", err)
	}
	path := filepath.Join(dir, KeyFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("key file after first use: %v", err)
	}
	if runtime.GOOS != "windows" {
		if got := info.Mode().Perm(); got != KeyMode {
			t.Errorf("key file mode = %#o, want %#o", got, KeyMode)
		}
	}
	if got := first.N.BitLen(); got != keyBits {
		t.Errorf("key is %d bits, want %d", got, keyBits)
	}
	if err := first.Validate(); err != nil {
		t.Errorf("generated key does not validate: %v", err)
	}

	// Nothing else in the directory: the temporary file the key was
	// written through is gone.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != KeyFile {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only %s", names, KeyFile)
	}

	second, err := LoadOrGenerate(dir)
	if err != nil {
		t.Fatalf("second use: %v", err)
	}
	if !first.Equal(second) {
		t.Error("second use returned a different key — a token minted before it would stop validating")
	}

	// What is on disk is a PKCS #8 PEM any JOSE library or `openssl pkey`
	// reads — the format is part of the contract, since the file outlives
	// this version of the code.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" || len(rest) != 0 {
		t.Fatalf("key file is not one PEM %q block", "PRIVATE KEY")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("key file does not parse as PKCS #8: %v", err)
	}
	if rk, ok := parsed.(*rsa.PrivateKey); !ok || !rk.Equal(first) {
		t.Errorf("key file holds %T, not the key LoadOrGenerate returned", parsed)
	}
}

// TestNoDirectoryIsAnErrorNotACWDKey: config.Dir() answers "" when the home
// directory cannot be determined, and Load reads that as "no user layer".
// Key generation must not read it as "here": an empty dir is an error, and
// nothing lands in the working directory.
func TestNoDirectoryIsAnErrorNotACWDKey(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)

	key, err := LoadOrGenerate("")
	if !errors.Is(err, ErrNoDir) {
		t.Fatalf("LoadOrGenerate(\"\") = %v, %v; want ErrNoDir", key, err)
	}
	if key != nil {
		t.Error("a key was returned with no directory to keep it in")
	}
	entries, err := os.ReadDir(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("working directory now holds %v; nothing may be written there", entries)
	}
}

// TestWiderModeIsRefused: a key readable by other local users is ADR 006's
// sharp edge on the developer's own machine. The message names the file and
// the mode it found, because the remedy is a chmod or a delete on that path.
func TestWiderModeIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no mode bits to check; the ACL on the profile directory is what protects the key")
	}
	dir := t.TempDir()
	if _, err := LoadOrGenerate(dir); err != nil {
		t.Fatalf("generate: %v", err)
	}
	path := filepath.Join(dir, KeyFile)
	for _, mode := range []fs.FileMode{0o644, 0o640, 0o604, 0o666, 0o700} {
		t.Run(fmt.Sprintf("%#o", mode), func(t *testing.T) {
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			key, err := LoadOrGenerate(dir)
			if err == nil {
				t.Fatalf("a %#o key was accepted", mode)
			}
			if key != nil {
				t.Error("a key was returned alongside the refusal")
			}
			for _, want := range []string{path, fmt.Sprintf("%#o", mode)} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not name %q:\n%s", want, err)
				}
			}
			if errors.Is(err, fs.ErrNotExist) {
				t.Error("refusal reads as a missing file, which would trigger generation over the existing key")
			}
		})
	}
	// A narrower mode is not "wider": 0400 is a read-only key and is fine.
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrGenerate(dir); err != nil {
		t.Errorf("a 0400 key was refused: %v", err)
	}
}

// TestNotAKeyIsRefusedByPath: an existing file that is not a key — empty,
// truncated, garbage, the wrong kind of key, or a directory — is refused,
// not silently replaced, and the message names the path since the remedy is
// to delete it.
func TestNotAKeyIsRefusedByPath(t *testing.T) {
	valid := func(t *testing.T) []byte {
		t.Helper()
		dir := t.TempDir()
		if _, err := LoadOrGenerate(dir); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(dir, KeyFile))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	cases := []struct {
		name  string
		write func(t *testing.T, path string)
	}{
		{"empty file", func(t *testing.T, path string) { writeFile(t, path, nil) }},
		{"truncated key", func(t *testing.T, path string) {
			data := valid(t)
			writeFile(t, path, data[:len(data)/2])
		}},
		{"garbage", func(t *testing.T, path string) { writeFile(t, path, []byte("not a key\n")) }},
		{"trailing data", func(t *testing.T, path string) {
			writeFile(t, path, append(valid(t), "garbage\n"...))
		}},
		{"wrong PEM type", func(t *testing.T, path string) {
			block, _ := pem.Decode(valid(t))
			block.Type = "RSA PRIVATE KEY"
			writeFile(t, path, pem.EncodeToMemory(block))
		}},
		{"directory", func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, KeyFile)
			tc.write(t, path)
			before, _ := os.Stat(path)

			key, err := LoadOrGenerate(dir)
			if err == nil {
				t.Fatalf("accepted: %v", key)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error does not name %s:\n%s", path, err)
			}
			if errors.Is(err, fs.ErrNotExist) {
				t.Error("refusal reads as a missing file")
			}
			after, _ := os.Stat(path)
			if before.ModTime() != after.ModTime() || before.Size() != after.Size() || before.IsDir() != after.IsDir() {
				t.Error("the file was changed by the refusal; a not-a-key file is the developer's to delete")
			}
		})
	}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, KeyMode); err != nil {
		t.Fatal(err)
	}
}

// TestConcurrentFirstUseConvergesOnOneKey is `voya run` and `voya token` in
// another terminal on a fresh machine: both generate, one links first, and
// every caller ends up holding the key that is on disk — with no half-written
// file and no leftover temporary beside it. Goroutines stand in for
// processes; the mechanism (CreateTemp then Link) is the filesystem's, so
// what it proves is the same.
func TestConcurrentFirstUseConvergesOnOneKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "voya")
	const n = 8
	keys := make([]*rsa.PrivateKey, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			keys[i], errs[i] = LoadOrGenerate(dir)
		}()
	}
	wg.Wait()

	onDisk, err := LoadOrGenerate(dir)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	for i := range n {
		if errs[i] != nil {
			t.Errorf("caller %d: %v", i, errs[i])
			continue
		}
		if !keys[i].Equal(onDisk) {
			t.Errorf("caller %d holds a key that is not the one on disk — a token it minted would not validate against the JWKS", i)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != KeyFile {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v after %d concurrent first uses, want only %s", names, n, KeyFile)
	}
}

// TestReadOnlyDirectoryIsAnError: $VOYA_CONFIG_DIR pointing somewhere voya
// cannot write is reported, naming the directory, rather than a key
// somewhere else or a retry loop.
func TestReadOnlyDirectoryIsAnError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory modes do not deny root-less writes on Windows the same way")
	}
	if os.Geteuid() == 0 {
		t.Skip("root writes through 0500")
	}
	t.Run("existing", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o700) })
		_, err := LoadOrGenerate(dir)
		if err == nil {
			t.Fatal("a key was generated in a read-only directory")
		}
		if !strings.Contains(err.Error(), dir) {
			t.Errorf("error does not name %s:\n%s", dir, err)
		}
	})
	t.Run("uncreatable", func(t *testing.T) {
		parent := t.TempDir()
		if err := os.Chmod(parent, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(parent, 0o700) })
		dir := filepath.Join(parent, "voya")
		_, err := LoadOrGenerate(dir)
		if err == nil {
			t.Fatal("a key was generated under a read-only parent")
		}
		if !strings.Contains(err.Error(), dir) {
			t.Errorf("error does not name %s:\n%s", dir, err)
		}
	})
}

// TestDanglingSymlinkIsNotAMissingKey: a link checked out of a dotfiles repo
// whose target is not there is the one path where the key is placed and then
// still not readable — os.Stat follows the link and answers ENOENT, os.Link
// sees the link itself and answers EEXIST. The refusal must name the path
// like every other one, and must not read as a missing file: LoadOrGenerate's
// contract is that a caller can never take an error for "generate one".
func TestDanglingSymlinkIsNotAMissingKey(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink on Windows needs a privilege the test runner may not have")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, KeyFile)
	target := filepath.Join(dir, "elsewhere", KeyFile)
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	key, err := LoadOrGenerate(dir)
	if err == nil {
		t.Fatalf("a dangling symlink was accepted: %v", key)
	}
	if key != nil {
		t.Error("a key was returned alongside the refusal")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Errorf("refusal reads as a missing file, so a caller that generates on that loops for ever:\n%s", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error does not name %s:\n%s", path, err)
	}
	// The link is the developer's to fix: nothing is written through it,
	// and no temporary is left beside it.
	if _, err := os.Stat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a key was written through the link to %s (stat err = %v)", target, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != KeyFile {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("%s holds %v, want just the symlink", dir, names)
	}
}
