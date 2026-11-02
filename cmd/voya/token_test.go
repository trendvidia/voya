package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"

	"github.com/trendvidia/voya/internal/auth"
	"github.com/trendvidia/voya/internal/config"
)

// mintToken runs `voya token` with args in a fresh config directory — no
// voya.pxf, no key, no daemon, the state of a terminal that has never run
// `voya run` — and returns the command's error and both streams. The fresh
// directory is the point of most callers: #210 requires minting to work with
// nothing but the binary.
func mintToken(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	t.Setenv(config.DirEnv, t.TempDir())
	var out, errs bytes.Buffer
	err = tokenCmd(args, &out, &errs)
	return out.String(), errs.String(), err
}

// TestTokenMintsWhatAStockValidatorAccepts: `voya token --as has-write-scope`
// in a directory with no config and no key mints a token that jwt.Parse —
// stock jwx, keyed by the JWKS document, validation on — accepts, with the
// persona's subject and scopes. The token is alone on stdout, one line, so
// `$(voya token --as ...)` is a token; stderr stays empty on success.
func TestTokenMintsWhatAStockValidatorAccepts(t *testing.T) {
	stdout, stderr, err := mintToken(t, "--as", "has-write-scope")
	if err != nil {
		t.Fatalf("voya token --as has-write-scope: %v", err)
	}
	if stderr != "" {
		t.Errorf("stderr on success should be empty, got %q", stderr)
	}
	token, nl := strings.CutSuffix(stdout, "\n")
	if !nl || strings.ContainsAny(token, "\n ") {
		t.Fatalf("stdout must be the token alone on one line, got %q", stdout)
	}

	// The same key the command generated on its way through, published the
	// way #209 serves it, so the validation below is the remote-JWKS path a
	// service would run.
	key, err := auth.LoadOrGenerate(config.Dir())
	if err != nil {
		t.Fatalf("re-loading the key the command generated: %v", err)
	}
	doc, err := auth.KeySet(key)
	if err != nil {
		t.Fatal(err)
	}
	set, err := jwk.Parse(doc)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(context.Background(), config.Dir())
	if err != nil {
		t.Fatal(err)
	}
	tok, err := jwt.Parse([]byte(token), jwt.WithKeySet(set), jwt.WithValidate(true),
		jwt.WithIssuer(cfg.GetAuth().GetIssuer()), jwt.WithAudience(cfg.GetAuth().GetAudience()))
	if err != nil {
		t.Fatalf("a stock validator rejected the minted token: %v", err)
	}
	if got := tok.Subject(); got != "has-write-scope" {
		t.Errorf("subject = %q, want %q", got, "has-write-scope")
	}
	scope, ok := tok.Get("scope")
	if !ok || scope != "read write" {
		t.Errorf("scope claim = %v (present %v), want %q", scope, ok, "read write")
	}
}

// TestTokenWithoutAPersonaListsThem: both ways of not naming a persona — no
// --as at all, and a name the config does not have — end in an error listing
// the personas that do exist, because the config is the only place the
// answer is.
func TestTokenWithoutAPersonaListsThem(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "missing", args: nil},
		{name: "unknown", args: []string{"--as", "admin"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, _, err := mintToken(t, tc.args...)
			if err == nil {
				t.Fatalf("voya token %v minted something: %q", tc.args, stdout)
			}
			if stdout != "" {
				t.Errorf("stdout must stay empty on failure so it never half-looks like a token, got %q", stdout)
			}
			// The four base personas, so the error is the discovery mechanism.
			for _, name := range []string{"has-write-scope", "has-read-scope", "no-scopes", "wrong-audience"} {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("the error does not list persona %q:\n%v", name, err)
				}
			}
		})
	}
}

// TestTokenWithNoPersonasSaysSo: `personas = []` in a voya.pxf is valid
// configuration, so the state reaches `voya token`. There is no list left for
// the error to be the discovery mechanism with, so it says where to add one.
func TestTokenWithNoPersonasSaysSo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, config.UserFile), []byte("auth { personas = [] }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.DirEnv, dir)
	var out, errs bytes.Buffer
	err := tokenCmd([]string{"--as", "has-write-scope"}, &out, &errs)
	if err == nil {
		t.Fatalf("voya token minted something with no personas: %q", out.String())
	}
	if !strings.Contains(err.Error(), "the configuration has none") {
		t.Errorf("the error does not say the list is empty:\n%v", err)
	}
	if out.Len() != 0 {
		t.Errorf("stdout must stay empty on failure, got %q", out.String())
	}
}

// TestTokenExpiredFailsForExpiry: --expired mints a token a stock validator
// rejects for expiry by name — jwt.ErrTokenExpired, not a signature or
// audience error — so the defect demonstrates the rejection it exists to
// demonstrate. The signature stays valid: verification alone (validation
// off) accepts it, the control that distinguishes "expired" from "broken".
func TestTokenExpiredFailsForExpiry(t *testing.T) {
	stdout, _, err := mintToken(t, "--as", "has-write-scope", "--expired")
	if err != nil {
		t.Fatalf("voya token --expired: %v", err)
	}
	token := []byte(strings.TrimSuffix(stdout, "\n"))
	set := keySetFor(t)

	_, err = jwt.Parse(token, jwt.WithKeySet(set), jwt.WithValidate(true))
	if !errors.Is(err, jwt.ErrTokenExpired()) {
		t.Fatalf("want rejection for expiry by name, got: %v", err)
	}
	if _, err := jwt.Parse(token, jwt.WithKeySet(set), jwt.WithValidate(false)); err != nil {
		t.Fatalf("the expired token must still verify — everything but exp stays valid: %v", err)
	}
}

// TestTokenBadSignatureFailsVerification: --bad-signature mints a token that
// fails signature verification, and only that — jwt.ParseInsecure plus
// jwt.Validate accepts the claims, the control that pins the defect to the
// signature rather than to a malformed or expired token.
func TestTokenBadSignatureFailsVerification(t *testing.T) {
	stdout, _, err := mintToken(t, "--as", "has-write-scope", "--bad-signature")
	if err != nil {
		t.Fatalf("voya token --bad-signature: %v", err)
	}
	token := []byte(strings.TrimSuffix(stdout, "\n"))

	if _, err := jwt.Parse(token, jwt.WithKeySet(keySetFor(t))); err == nil {
		t.Fatal("a token minted with --bad-signature verified against the key set")
	}
	tok, err := jwt.ParseInsecure(token)
	if err != nil {
		t.Fatalf("the corrupted token must still parse — only the signature is broken: %v", err)
	}
	if err := jwt.Validate(tok); err != nil {
		t.Fatalf("the corrupted token's claims must still validate — only the signature is broken: %v", err)
	}
}

// TestTokenRefusesTwoDefectsAtOnce: one deliberate defect at a time — a
// token broken two ways cannot show which rejection fired.
func TestTokenRefusesTwoDefectsAtOnce(t *testing.T) {
	stdout, _, err := mintToken(t, "--as", "has-write-scope", "--expired", "--bad-signature")
	if err == nil {
		t.Fatalf("minted a doubly-broken token: %q", stdout)
	}
	if !strings.Contains(err.Error(), "not both") {
		t.Errorf("the error should say one defect at a time, got: %v", err)
	}
}

// TestTokenRefusesArguments: the persona travels in --as; a bare word is a
// mistake worth naming, not a persona to guess at.
func TestTokenRefusesArguments(t *testing.T) {
	_, _, err := mintToken(t, "has-write-scope")
	if err == nil || !strings.Contains(err.Error(), "--as") {
		t.Fatalf("want a refusal pointing at --as, got: %v", err)
	}
}

// keySetFor parses the JWKS for the key in the current config directory, the
// document #209 serves.
func keySetFor(t *testing.T) jwk.Set {
	t.Helper()
	key, err := auth.LoadOrGenerate(config.Dir())
	if err != nil {
		t.Fatal(err)
	}
	doc, err := auth.KeySet(key)
	if err != nil {
		t.Fatal(err)
	}
	set, err := jwk.Parse(doc)
	if err != nil {
		t.Fatal(err)
	}
	return set
}
