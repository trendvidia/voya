package auth

import (
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
)

// JWKSPath is where the daemon serves the public half of the signing key
// under voya.internal (#209). The name is RFC 8615's registered well-known
// path for a JWK Set, because the whole point of the endpoint is that an
// ordinary IdP client fetches it with no voya-specific configuration beyond
// the URL. Exactly this path is claimed: the trailing-slash variant and the
// parent /.well-known/ stay unclaimed and answer ADR 005's reserved row.
const JWKSPath = "/.well-known/jwks.json"

// JWKSContentType is what a JWK Set document is served as (RFC 7517 §8.5.1).
const JWKSContentType = "application/jwk-set+json"

// publicJWK is the public half of key as a JWK: kty and the RSA members from
// the key, kid by RFC 7638 thumbprint — so the id survives voya restarting
// and changes exactly when the key does — alg and use set so a validator
// that filters on either finds the key.
func publicJWK(key *rsa.PrivateKey) (jwk.Key, error) {
	k, err := jwk.FromRaw(key.Public())
	if err != nil {
		return nil, fmt.Errorf("auth: building the public JWK: %w", err)
	}
	if err := jwk.AssignKeyID(k); err != nil {
		return nil, fmt.Errorf("auth: computing the key id: %w", err)
	}
	if err := k.Set(jwk.AlgorithmKey, jwa.RS256); err != nil {
		return nil, fmt.Errorf("auth: setting alg: %w", err)
	}
	if err := k.Set(jwk.KeyUsageKey, jwk.ForSignature); err != nil {
		return nil, fmt.Errorf("auth: setting use: %w", err)
	}
	return k, nil
}

// KeySet renders the public half of key as an RFC 7517 JWK Set.
//
// JSON, in a repo whose convention bans it — because this is the convention's
// own exception, a file at an external boundary (README — Conventions). The
// document's shape is RFC 7517's and its readers are third-party JOSE
// libraries voya does not control: the value of ADR 006's design is that a
// service validates voya's tokens with unmodified library code, and that
// code reads exactly this.
func KeySet(key *rsa.PrivateKey) ([]byte, error) {
	k, err := publicJWK(key)
	if err != nil {
		return nil, err
	}
	set := jwk.NewSet()
	if err := set.AddKey(k); err != nil {
		return nil, fmt.Errorf("auth: assembling the key set: %w", err)
	}
	doc, err := json.Marshal(set)
	if err != nil {
		return nil, fmt.Errorf("auth: encoding the key set: %w", err)
	}
	return doc, nil
}

// JWKSHandler serves [KeySet] for the key in dir, loading it on every
// request: the key is [LoadOrGenerate]'s to own, so a key deleted between
// requests is regenerated rather than served from a stale copy, and the
// first request on a fresh machine mints the key it answers with.
//
// A key that cannot be loaded is a 500 carrying LoadOrGenerate's own
// diagnosis, which names the file and the remedy — never an empty key set,
// which a JOSE client caches as "this issuer has no keys" and then fails
// closed on every token for its whole TTL.
//
// GET and HEAD are the document's methods; anything else is 405 with Allow,
// decided here rather than left to the mux, because the proxy's internal
// router treats an unclaimed (method, path) as ADR 005's reserved row and a
// POST to a path that exists is not that.
//
// Cache-Control is no-store: the key changes whenever the file is deleted
// (#208), the fetch is a loopback round-trip, and a cached copy that
// outlives the key turns key rotation into signature failures for the
// cache's whole lifetime.
func JWKSHandler(dir string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, fmt.Sprintf("voya: %s %s: the JWK Set is fetched with GET", r.Method, JWKSPath),
				http.StatusMethodNotAllowed)
			return
		}
		key, err := LoadOrGenerate(dir)
		if err != nil {
			http.Error(w, fmt.Sprintf("voya: the signing key is unavailable, so no key set follows: %v", err),
				http.StatusInternalServerError)
			return
		}
		doc, err := KeySet(key)
		if err != nil {
			http.Error(w, fmt.Sprintf("voya: the signing key did not render as a JWK Set, so no key set follows: %v", err),
				http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", JWKSContentType)
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(doc)
	})
}
