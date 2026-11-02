package acceptance

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"

	voya "github.com/trendvidia/voya/client"
	"github.com/trendvidia/voya/internal/auth"
	"github.com/trendvidia/voya/internal/config"
)

// TestAStockJOSEClientValidatesAMintedToken is #209's acceptance criterion
// in one paragraph: voya is running, a token is minted the way `voya token`
// mints one, and a stock JOSE library validates it end to end — the JWK Set
// fetched over the library's own remote-JWKS path from the well-known URL
// under voya.internal, through the client package's dialer, which is the
// resolution path every service uses (ADR 002). Nothing voya-specific is in
// the validating half; that it needs none is ADR 006's product.
func TestAStockJOSEClientValidatesAMintedToken(t *testing.T) {
	v, cfg := startOnFreePorts(t) // isolates the configuration directory too

	// Mint as `voya token --as has-write-scope` does: the key from the
	// configuration directory — the one the daemon minted at start (#219)
	// — and the persona from the committed defaults.
	key, err := auth.LoadOrGenerate(config.Dir())
	if err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}
	personas := cfg.GetAuth().GetPersonas()
	if len(personas) == 0 || personas[0].GetName() != "has-write-scope" {
		t.Fatalf("the committed defaults do not lead with the has-write-scope persona: %v", personas)
	}
	token, err := auth.Mint(key, cfg.GetAuth(), personas[0], time.Now(), auth.Valid)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), callerTimeout)
	defer cancel()
	client := &http.Client{
		Transport: voya.Transport(voya.WithProxyPort(v.proxyPort)),
		Timeout:   callerTimeout,
	}
	url := fmt.Sprintf("http://voya.internal:%d%s", v.proxyPort, auth.JWKSPath)
	set, err := jwk.Fetch(ctx, url, jwk.WithHTTPClient(client))
	if err != nil {
		t.Fatalf("the library's remote-JWKS fetch of %s failed: %v", url, err)
	}

	tok, err := jwt.Parse([]byte(token), jwt.WithKeySet(set), jwt.WithValidate(true),
		jwt.WithIssuer(cfg.GetAuth().GetIssuer()), jwt.WithAudience(cfg.GetAuth().GetAudience()))
	if err != nil {
		t.Fatalf("the stock validator rejected the minted token: %v", err)
	}
	if got, want := tok.Subject(), personas[0].GetSubject(); got != want {
		t.Errorf("sub = %q, want the persona's %q", got, want)
	}
	if scope, ok := tok.Get("scope"); !ok || scope != "read write" {
		t.Errorf("scope = %v (present %v), want %q", scope, ok, "read write")
	}
}

// TestTheSigningKeyLandsInTheHarnessDir is #219's harness proof, asserted
// rather than trusted: [startOnFreePorts] points $VOYA_CONFIG_DIR at a temp
// directory of the test's own, and the key the daemon mints at start is
// there — under the temp dir, at 0600, before anything has fetched anything.
// A green suite alone would not prove this: the suite was green while every
// daemon it started was one request away from writing into the developer's
// real ~/.config/voya.
func TestTheSigningKeyLandsInTheHarnessDir(t *testing.T) {
	v, _ := startOnFreePorts(t)

	if got := config.Dir(); got != v.configDir {
		t.Fatalf("config.Dir() = %q, want the harness's %q — the isolation did not take effect", got, v.configDir)
	}
	info, err := os.Stat(filepath.Join(v.configDir, auth.KeyFile))
	if err != nil {
		t.Fatalf("no signing key under the harness directory after Start: %v", err)
	}
	if perm := info.Mode().Perm(); perm != auth.KeyMode {
		t.Errorf("the minted key is at mode %#o, want %#o", perm, auth.KeyMode)
	}
}
