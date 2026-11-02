package auth

import (
	"crypto/rsa"
	"errors"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jws"
	"github.com/lestrrat-go/jwx/v2/jwt"
	"google.golang.org/protobuf/types/known/durationpb"

	configv1 "github.com/trendvidia/voya/proto/voya/config/v1"
)

// Every assertion in this file goes through a stock JOSE library's own
// verification and validation — jwx, the same code a service would run —
// because "voya's tokens validate with unmodified library code" is ADR 006's
// product, and a hand-rolled base64 walk would assert on shape rather than
// on that.

func mintAuth() *configv1.Auth {
	return &configv1.Auth{
		Issuer:        "http://voya.internal:9777",
		Audience:      "internal",
		TokenLifetime: durationpb.New(time.Hour),
	}
}

func mintKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := LoadOrGenerate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}
	return key
}

// keySetOf parses KeySet's document the way a validator that fetched it
// would.
func keySetOf(t *testing.T, key *rsa.PrivateKey) jwk.Set {
	t.Helper()
	doc, err := KeySet(key)
	if err != nil {
		t.Fatalf("KeySet: %v", err)
	}
	set, err := jwk.Parse(doc)
	if err != nil {
		t.Fatalf("parsing the key set: %v", err)
	}
	return set
}

func TestMintSignsWhatAStockValidatorAccepts(t *testing.T) {
	key, a := mintKey(t), mintAuth()
	p := &configv1.Persona{
		Name:    "has-tenant",
		Subject: "u-1",
		Scopes:  []string{"read", "write"},
		Claims:  map[string]string{"tenant": "acme"},
	}
	now := time.Now()
	signed, err := Mint(key, a, p, now, Valid)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	tok, err := jwt.Parse([]byte(signed), jwt.WithKeySet(keySetOf(t, key)),
		jwt.WithValidate(true), jwt.WithIssuer(a.GetIssuer()), jwt.WithAudience(a.GetAudience()))
	if err != nil {
		t.Fatalf("a stock validator rejects the minted token: %v", err)
	}
	if got := tok.Subject(); got != "u-1" {
		t.Errorf("sub = %q, want %q", got, "u-1")
	}
	if scope, ok := tok.Get("scope"); !ok || scope != "read write" {
		t.Errorf("scope = %v (present %v), want %q space-joined", scope, ok, "read write")
	}
	if tenant, ok := tok.Get("tenant"); !ok || tenant != "acme" {
		t.Errorf("tenant = %v (present %v), want the persona's extra claim", tenant, ok)
	}
	if got, want := tok.Expiration().Sub(tok.IssuedAt()), time.Hour; got != want {
		t.Errorf("exp - iat = %v, want the configured lifetime %v", got, want)
	}
	if tok.NotBefore().After(now.Add(time.Second)) {
		t.Errorf("nbf = %v is after minting time %v", tok.NotBefore(), now)
	}
}

// TestMintKidMatchesTheKeySet: the token's header names the JWKS entry, so
// a remote-JWKS validator selects the key with no configuration.
func TestMintKidMatchesTheKeySet(t *testing.T) {
	key := mintKey(t)
	signed, err := Mint(key, mintAuth(), &configv1.Persona{Name: "p", Subject: "s"}, time.Now(), Valid)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	msg, err := jws.Parse([]byte(signed))
	if err != nil {
		t.Fatalf("parsing the JWS envelope: %v", err)
	}
	kid := msg.Signatures()[0].ProtectedHeaders().KeyID()
	if kid == "" {
		t.Fatal("the token carries no kid; a validator cannot pick the key out of the set")
	}
	served, _ := keySetOf(t, key).Key(0)
	if kid != served.KeyID() {
		t.Errorf("token kid %q is not the key set's %q", kid, served.KeyID())
	}
}

// TestMintPersonaAudienceOverridesTheDefault: the wrong-audience fixture is
// a persona field, not a flag (ADR 006).
func TestMintPersonaAudienceOverridesTheDefault(t *testing.T) {
	key, a := mintKey(t), mintAuth()
	p := &configv1.Persona{Name: "wrong-audience", Subject: "s", Audience: "not-this-service"}
	signed, err := Mint(key, a, p, time.Now(), Valid)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	_, err = jwt.Parse([]byte(signed), jwt.WithKeySet(keySetOf(t, key)),
		jwt.WithValidate(true), jwt.WithAudience(a.GetAudience()))
	if !errors.Is(err, jwt.ErrInvalidAudience()) {
		t.Errorf("a validator expecting %q answered %v, want its audience rejection", a.GetAudience(), err)
	}
}

// TestMintNoScopesMeansNoScopeClaim: the missing-scope fixture is an absent
// claim, not an empty string — a second shape for the same case would make
// a validator's "no scope" branch untestable against the first.
func TestMintNoScopesMeansNoScopeClaim(t *testing.T) {
	key := mintKey(t)
	signed, err := Mint(key, mintAuth(), &configv1.Persona{Name: "no-scopes", Subject: "s"}, time.Now(), Valid)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	tok, err := jwt.Parse([]byte(signed), jwt.WithKeySet(keySetOf(t, key)), jwt.WithValidate(true))
	if err != nil {
		t.Fatalf("a stock validator rejects the no-scopes token: %v", err)
	}
	if scope, ok := tok.Get("scope"); ok {
		t.Errorf("scope = %v, want the claim absent", scope)
	}
}

// TestMintExpiredFailsOnExpiryAlone: the deliberately expired token is
// rejected for expiry — and for nothing else, proved by the same token
// passing signature verification and every other check.
func TestMintExpiredFailsOnExpiryAlone(t *testing.T) {
	key, a := mintKey(t), mintAuth()
	signed, err := Mint(key, a, &configv1.Persona{Name: "p", Subject: "s"}, time.Now(), Expired)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	set := keySetOf(t, key)
	_, err = jwt.Parse([]byte(signed), jwt.WithKeySet(set), jwt.WithValidate(true))
	if !errors.Is(err, jwt.ErrTokenExpired()) {
		t.Fatalf("rejection = %v, want the validator's expiry error", err)
	}
	// The control: signature, issuer and audience all hold, so expiry is
	// the one thing wrong — a token that also failed verification would
	// "prove" the expiry path while exercising another.
	tok, err := jwt.Parse([]byte(signed), jwt.WithKeySet(set), jwt.WithValidate(false))
	if err != nil {
		t.Fatalf("the expired token does not verify, so its rejection proves nothing about expiry: %v", err)
	}
	if err := jwt.Validate(tok, jwt.WithIssuer(a.GetIssuer()), jwt.WithAudience(a.GetAudience()),
		jwt.WithClock(jwt.ClockFunc(func() time.Time { return time.Now().Add(-2 * time.Hour) }))); err != nil {
		t.Errorf("rewinding the clock past exp does not make the token valid, so something besides expiry is wrong: %v", err)
	}
}

// TestMintBadSignatureFailsOnTheSignatureAlone: same shape as the expired
// case — verification fails, while the claims validate on their own.
func TestMintBadSignatureFailsOnTheSignatureAlone(t *testing.T) {
	key, a := mintKey(t), mintAuth()
	signed, err := Mint(key, a, &configv1.Persona{Name: "p", Subject: "s"}, time.Now(), BadSignature)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := jwt.Parse([]byte(signed), jwt.WithKeySet(keySetOf(t, key)), jwt.WithValidate(false)); err == nil {
		t.Fatal("a stock validator verified the deliberately corrupted signature")
	}
	// The control: the claims are otherwise a valid token's.
	tok, err := jwt.ParseInsecure([]byte(signed))
	if err != nil {
		t.Fatalf("the corrupted token no longer parses, so it fails as malformed rather than as badly signed: %v", err)
	}
	if err := jwt.Validate(tok, jwt.WithIssuer(a.GetIssuer()), jwt.WithAudience(a.GetAudience())); err != nil {
		t.Errorf("the claims do not validate, so the rejection is not the signature's alone: %v", err)
	}
}
