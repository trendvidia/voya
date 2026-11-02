package daemon

// --- #219: the signing key is minted when the daemon starts ------------------
//
// ADR 006 says voya "generates a signing key on first run", and the amendment
// of 2026-08-19 (#219) reads that literally: Start calls auth.LoadOrGenerate
// once before anything serves, and every failure of that call — a file auth
// refuses, no configuration directory at all — refuses the start. These tests
// pin both halves; the per-request regeneration the JWKS handler keeps is
// internal/auth's and TestTheReservedNameServesTheJWKS's.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/trendvidia/voya/internal/auth"
	"github.com/trendvidia/voya/internal/config"
)

// TestStartMintsTheSigningKeyBeforeAnyRequest: `voya run` on a machine with
// no key leaves one on disk, at 0600, before any request arrives — the stat
// happens with nothing having fetched anything. The directory is read back
// from the environment testConfig set, which is also the proof the harness
// isolates it: the key landed in this test's temp dir, not the developer's
// ~/.config/voya.
func TestStartMintsTheSigningKeyBeforeAnyRequest(t *testing.T) {
	cfg := testConfig(t, time.Second)
	dir := os.Getenv(config.DirEnv)
	if dir == "" || config.Dir() != dir {
		t.Fatalf("testConfig did not isolate the configuration directory: env %q, Dir() %q", dir, config.Dir())
	}

	d, err := Start(context.Background(), cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("stopping voya: %v", err)
		}
	})

	info, err := os.Stat(filepath.Join(dir, auth.KeyFile))
	if err != nil {
		t.Fatalf("Start returned with no signing key on disk: %v", err)
	}
	// On Windows the permission bits are synthesised from the read-only
	// attribute and are never 0600 (auth.checkMode says why it skips there).
	if perm := info.Mode().Perm(); runtime.GOOS != "windows" && perm != auth.KeyMode {
		t.Errorf("the minted key is at mode %#o, want %#o", perm, auth.KeyMode)
	}
}

// TestStartRefusesAnUnusableSigningKey: a key file auth refuses — here, one
// wider than 0600 — stops `voya run` entirely, with auth's own refusal
// naming the file and the remedy. Deliberate (#219): a local IdP that cannot
// secure its key must say so rather than serve as if it could, even at the
// price of losing the proxy, DNS and the registry over a file none of them
// use.
func TestStartRefusesAnUnusableSigningKey(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the 0600 rule is skipped on Windows, where the bits are synthesised (auth.checkMode)")
	}
	cfg := testConfig(t, time.Second)
	path := filepath.Join(os.Getenv(config.DirEnv), auth.KeyFile)
	if err := os.WriteFile(path, []byte("not a key"), 0o644); err != nil {
		t.Fatal(err)
	}

	d, err := Start(context.Background(), cfg, func(string, ...any) {})
	if err == nil {
		_ = d.Close()
		t.Fatal("voya started over a signing key file it should refuse")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the failure does not name the key file %s:\n%s", path, err)
	}
	if !strings.Contains(err.Error(), "wider than") {
		t.Errorf("the failure does not carry auth's mode refusal:\n%s", err)
	}
}

// TestStartRefusesToRunWithNoConfigDir: when no configuration directory can
// be determined at all — no $VOYA_CONFIG_DIR, no home — there is nowhere a
// key would survive to, and `voya run` refuses to start rather than running
// as an identity provider with no identity to keep (#219; decided over the
// alternative of serving without auth). The error names $VOYA_CONFIG_DIR as
// the remedy.
func TestStartRefusesToRunWithNoConfigDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("config.Dir falls back to os.UserConfigDir on Windows, which clearing HOME does not empty")
	}
	cfg := testConfig(t, time.Second)
	// After testConfig, which needs a working Load; clearing all three is
	// what makes config.Dir answer "".
	t.Setenv(config.DirEnv, "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")

	d, err := Start(context.Background(), cfg, func(string, ...any) {})
	if err == nil {
		_ = d.Close()
		t.Fatal("voya started with no configuration directory to keep a signing key in")
	}
	if !errors.Is(err, auth.ErrNoDir) {
		t.Errorf("Start = %v, want auth.ErrNoDir", err)
	}
	if !strings.Contains(err.Error(), config.DirEnv) {
		t.Errorf("the failure does not name %s as the remedy:\n%s", config.DirEnv, err)
	}
}
