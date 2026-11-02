//go:build unix

package auth

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestKeyOwnedByAnotherUserIsRefused: the 0600 rule only protects the key if
// voya is the user those bits name. In a configuration directory another
// local user can write — `$VOYA_CONFIG_DIR` under /tmp, a `~/.config` some
// other tool made 0755 — they can leave a 0600 key of their own, and voya
// signing with it signs whatever they choose. The uid is a parameter of
// [load] precisely so this case is reachable without a second account.
func TestKeyOwnedByAnotherUserIsRefused(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrGenerate(dir); err != nil {
		t.Fatalf("generate: %v", err)
	}
	path := filepath.Join(dir, KeyFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Precondition: the owner the check reads is the real one on disk, not
	// a value the test made up.
	uid, known := fileOwner(info)
	if !known {
		t.Fatal("fileOwner does not know who owns a file this test just made; the check would pass on anything")
	}
	if uid != os.Geteuid() {
		t.Fatalf("the key this test generated is owned by uid %d, not by uid %d that wrote it", uid, os.Geteuid())
	}

	other := uid + 1
	key, err := load(path, other)
	if err == nil {
		t.Fatalf("a key owned by uid %d was accepted while running as uid %d", uid, other)
	}
	if key != nil {
		t.Error("a key was returned alongside the refusal")
	}
	for _, want := range []string{path, fmt.Sprint(uid), fmt.Sprint(other)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q:\n%s", want, err)
		}
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Error("refusal reads as a missing file, which would generate over the existing key")
	}

	// Positive control: the same file, read by the uid that owns it.
	if _, err := load(path, uid); err != nil {
		t.Errorf("the owner's own key was refused: %v", err)
	}
}
