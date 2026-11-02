package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"

	configv1 "github.com/trendvidia/voya/proto/voya/config/v1"
)

// Defect is the deliberate flaw Mint puts in a token, or Valid for none.
//
// Only the two flaws a persona cannot express are here: ADR 006 names four
// invalid shapes, and `missing scope` and `wrong audience` are fields of
// [configv1.Persona], so the no-scopes and wrong-audience fixtures carry
// them with no flag at all. Expiry and the signature are stamped by Mint
// itself, so breaking them is Mint's to do.
type Defect int

const (
	// Valid is a token with nothing wrong with it.
	Valid Defect = iota

	// Expired is a token whose exp — and iat and nbf with it, so the
	// timestamps stay coherent — is an hour in the past, beyond any leeway
	// a validator allows. Everything else about it is right: a validator
	// that rejects it for any reason but expiry has a bug this fixture
	// exists to catch.
	Expired

	// BadSignature is a token whose signature bytes are corrupted after
	// signing — one bit flipped, so the envelope still parses, the header
	// still names the real key, and the one thing wrong is that the
	// signature does not verify. Corrupting the header or claims instead
	// would fail as malformed input before signature verification ran.
	BadSignature
)

// Mint signs a token for persona p under a's issuer, audience and lifetime,
// with the key — the claims voya stamps (iss, sub, aud, exp, iat, nbf, jti,
// scope) from the config and the persona, and the persona's extra claims
// beside them. Validation (config.Validate) has already refused a persona
// that restates a stamped claim, an empty subject, or a scope that breaks
// the space-joined wire form; Mint trusts that and does not re-check.
//
// The `scope` claim is the space-joined scopes, and is absent — not empty —
// when the persona has none: an absent claim is what "missing scope" looks
// like to a validator, and an empty string is a second shape for the same
// case.
//
// The kid header names the JWKS entry ([KeySet]) for the same key, so a
// stock validator's remote-JWKS path selects it with no configuration.
func Mint(key *rsa.PrivateKey, a *configv1.Auth, p *configv1.Persona, now time.Time, defect Defect) (string, error) {
	iat := now
	if defect == Expired {
		iat = now.Add(-a.GetTokenLifetime().AsDuration() - time.Hour)
	}
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return "", fmt.Errorf("auth: drawing a token id: %w", err)
	}

	audience := p.GetAudience()
	if audience == "" {
		audience = a.GetAudience()
	}
	b := jwt.NewBuilder().
		Issuer(a.GetIssuer()).
		Subject(p.GetSubject()).
		Audience([]string{audience}).
		IssuedAt(iat).
		NotBefore(iat).
		Expiration(iat.Add(a.GetTokenLifetime().AsDuration())).
		JwtID(hex.EncodeToString(jti))
	if scopes := p.GetScopes(); len(scopes) > 0 {
		b.Claim("scope", strings.Join(scopes, " "))
	}
	for name, value := range p.GetClaims() {
		b.Claim(name, value)
	}
	tok, err := b.Build()
	if err != nil {
		return "", fmt.Errorf("auth: assembling the claims for persona %q: %w", p.GetName(), err)
	}

	// Sign with the key as a JWK rather than the raw *rsa.PrivateKey,
	// because the JWK carries the kid — the same RFC 7638 thumbprint KeySet
	// serves, jwx computes it from the public members alone — and jws puts a
	// signing key's kid in the protected header.
	k, err := jwk.FromRaw(key)
	if err != nil {
		return "", fmt.Errorf("auth: wrapping the signing key: %w", err)
	}
	if err := jwk.AssignKeyID(k); err != nil {
		return "", fmt.Errorf("auth: computing the key id: %w", err)
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256, k))
	if err != nil {
		return "", fmt.Errorf("auth: signing the token for persona %q: %w", p.GetName(), err)
	}
	if defect == BadSignature {
		return corruptSignature(string(signed))
	}
	return string(signed), nil
}

// corruptSignature flips one bit of a compact JWS's signature segment,
// re-encoded so the segment still decodes: a different, wrong signature over
// the same bytes, rather than garbage a validator rejects as malformed
// before it verifies anything.
func corruptSignature(token string) (string, error) {
	i := strings.LastIndexByte(token, '.')
	if i < 0 {
		return "", fmt.Errorf("auth: signed token has no signature segment to corrupt: %q", token)
	}
	sig, err := base64.RawURLEncoding.DecodeString(token[i+1:])
	if err != nil {
		return "", fmt.Errorf("auth: decoding the signature segment to corrupt it: %w", err)
	}
	if len(sig) == 0 {
		return "", fmt.Errorf("auth: the signed token has an empty signature segment, so there is no bit to flip: %q", token)
	}
	sig[0] ^= 0x01
	return token[:i+1] + base64.RawURLEncoding.EncodeToString(sig), nil
}
