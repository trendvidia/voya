package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/trendvidia/voya/authn"
	"github.com/trendvidia/voya/internal/auth"
	"github.com/trendvidia/voya/internal/config"
)

// TestAMintedTokenValidatesThroughAuthn closes ADR 006's loop end to end
// with voya's real pieces on both sides (#211): the token comes from the
// `voya token` command, the key set from the JWKS handler the daemon mounts
// (#209), and validation from the authn module a production service would
// import — configured Development, because the issuer is voya's and a
// Production validator refuses it at startup by design.
func TestAMintedTokenValidatesThroughAuthn(t *testing.T) {
	stdout, _, err := mintToken(t, "--as", "has-write-scope")
	if err != nil {
		t.Fatalf("voya token --as has-write-scope: %v", err)
	}
	token := []byte(strings.TrimSuffix(stdout, "\n"))

	// The handler the daemon serves at auth.JWKSPath, over a listener of its
	// own: what authn needs from voya is the document, not the proxy.
	srv := httptest.NewServer(auth.JWKSHandler(config.Dir()))
	defer srv.Close()
	cfg, err := config.Load(context.Background(), config.Dir())
	if err != nil {
		t.Fatal(err)
	}
	v, err := authn.New(authn.Development, cfg.GetAuth().GetIssuer(), srv.URL+auth.JWKSPath, cfg.GetAuth().GetAudience())
	if err != nil {
		t.Fatal(err)
	}

	p, err := v.Validate(context.Background(), token)
	if err != nil {
		t.Fatalf("authn rejected the token voya minted: %v", err)
	}
	if p.Subject != "has-write-scope" {
		t.Errorf("Subject = %q, want %q", p.Subject, "has-write-scope")
	}
	if strings.Join(p.Scopes, ",") != "read,write" {
		t.Errorf("Scopes = %v, want [read write]", p.Scopes)
	}
	if err := p.RequireScope("write"); err != nil {
		t.Errorf("RequireScope(write) = %v on the has-write-scope persona", err)
	}

	// The missing-scope shape is a persona, not a flag (#210), and authn is
	// where it lands as ErrMissingScope: the no-scopes token validates —
	// absent scopes are not invalid — and the scope gate is what refuses it.
	// tokenCmd directly rather than mintToken, which would move to a fresh
	// config directory and a key the validator has never seen.
	var out bytes.Buffer
	if err := tokenCmd([]string{"--as", "no-scopes"}, &out, io.Discard); err != nil {
		t.Fatalf("voya token --as no-scopes: %v", err)
	}
	p, err = v.Validate(context.Background(), []byte(strings.TrimSuffix(out.String(), "\n")))
	if err != nil {
		t.Fatalf("authn rejected the no-scopes token outright — missing scope is RequireScope's to refuse: %v", err)
	}
	if err := p.RequireScope("write"); !errors.Is(err, authn.ErrMissingScope) {
		t.Errorf("RequireScope(write) on no-scopes = %v, want ErrMissingScope", err)
	}
}
