//go:build unix

package resolverfile

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The modes this package argues for — 0755 on the directory it creates, 0644
// on the file it writes — are what lands on disk even when the process umask
// would take those bits off, which is the only interesting case: under the
// usual 022 both modes come out right whether or not the package sets them
// again. Not hypothetical either. sudo runs with the invoking developer's
// umask unless sudoers says otherwise, so a `umask 077` in a shell profile
// would leave /etc/resolver unlistable and the file unreadable to that same
// developer's later non-root `voya dns check` and `voya status`, both of which
// stat it: a file reported unreadable immediately after a successful install
// (#104).
func TestInstallSetsTheModesPastTheUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })

	dir := filepath.Join(t.TempDir(), "resolver")
	w := mustFor(t, "darwin")
	w.Path = filepath.Join(dir, "internal")
	if res, err := w.Install(9778); err != nil || res.Outcome != Installed {
		t.Fatalf("Install: %+v, %v", res, err)
	}
	for _, tc := range []struct {
		what string
		path string
		want os.FileMode
	}{
		{"the directory install created", dir, 0o755},
		{"the file install wrote", w.Path, 0o644},
	} {
		fi, err := os.Stat(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != tc.want {
			t.Errorf("%s is %04o, want %04o", tc.what, got, tc.want)
		}
	}

	// A rewrite of voya's own file lands on the same mode: it goes through a
	// fresh temporary file, which is created 0600 whatever the umask.
	if res, err := w.Install(5353); err != nil || res.Outcome != Updated {
		t.Fatalf("Install over voya's own file: %+v, %v", res, err)
	}
	fi, err := os.Stat(w.Path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("the rewritten file is %04o, want 0644", got)
	}
}

// A directory that was already there is left as its owner made it: voya did
// not create it, and 0755 is what voya wants for its own, not something to
// impose on a developer's /etc/resolver.
func TestInstallLeavesAnExistingDirectorysMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "resolver")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil { // past the umask, whatever it is
		t.Fatal(err)
	}
	w := mustFor(t, "darwin")
	w.Path = filepath.Join(dir, "internal")
	if res, err := w.Install(9778); err != nil || res.Outcome != Installed {
		t.Fatalf("Install: %+v, %v", res, err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Errorf("install changed the existing directory to %04o, want it left at 0700", got)
	}
}
