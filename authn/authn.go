// Package authn turns a JWT into a [Principal], identically in development
// and production — ADR 006's "library, not a location". A service constructs
// one [Validator] at startup with the one value that differs between
// environments — the issuer and its JWKS URL — and everything else is the
// same code on the laptop and in production: signature verification, expiry
// and audience checks, claims-to-principal mapping, and scope derivation.
//
// This module deliberately imports nothing of voya's root module. voya is
// the development issuer, not a dependency: a production binary importing
// this package links a JOSE library and nothing else of voya's. A test in
// this package enforces that direction.
package authn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jws"
	"github.com/lestrrat-go/jwx/v2/jwt"
)

// Environment says whose tokens a build may trust. It is a required argument
// of [New] with no default and an invalid zero value, so a service cannot
// forget to choose: leaving it unset fails at startup, and the failure mode
// of forgetting is therefore a refused start rather than a production
// service that accepts voya-signed tokens (ADR 006's one dangerous
// consequence).
type Environment int

const (
	// environmentUnset is the zero value, and refusing it is the point:
	// an Environment that defaulted to anything could be silently absent.
	environmentUnset Environment = iota

	// Development trusts a development issuer — voya's, normally.
	Development

	// Production refuses an issuer inside voya's DNS zone at startup.
	Production
)

// Leeway is how far a token's exp and nbf may sit on the wrong side of now
// and still pass, absorbing clock skew between the issuer and the validator.
// 30 seconds is the decision: enough for unsynchronised laptop clocks, and
// well under the hour voya's token lifetime grants — a token accepted
// through leeway is one that expired moments ago, not one from yesterday.
const Leeway = 30 * time.Second

// DefaultRefetchInterval bounds how often the JWKS is refetched: at most one
// fetch per interval, however many unknown kids or failed fetches ask for
// one. It is also how long a fetched set is answered from before the issuer
// is asked again. It is deliberately short, because it bounds three things
// at once: how long an outage answer ("down", "empty") persists, how long a
// key *withdrawn* from the JWKS goes on validating tokens, plus the length of
// the fetch that finds it gone, and how long a rotation takes to propagate —
// seconds, in every case, not until a restart.
const DefaultRefetchInterval = 10 * time.Second

// jwksFetchTimeout bounds a JWKS fetch unless the client sets a Timeout of its
// own. A fetch belongs to no caller's context, and while one runs no other can
// start and a stale set goes on answering. So an issuer that accepts the
// connection and never answers must not get to hold it open forever, whatever
// client [WithHTTPClient] supplied.
const jwksFetchTimeout = 10 * time.Second

// The rejections a caller can tell apart with [errors.Is]. Distinguishable
// rejections are the requirement (ADR 006 names the invalid shapes one per
// case) — wrong-reason rejections are how auth bugs hide.
var (
	// ErrBadSignature: the signature does not verify, including the
	// confusion shapes — alg "none", an alg that disagrees with the key's,
	// and a signature by some other key over the claimed kid.
	ErrBadSignature = errors.New("authn: the token's signature does not verify")

	// ErrExpired: exp is more than [Leeway] in the past.
	ErrExpired = errors.New("authn: the token is expired")

	// ErrNotYetValid: nbf is more than [Leeway] in the future.
	ErrNotYetValid = errors.New("authn: the token is not valid yet")

	// ErrWrongIssuer: iss is not the one issuer this validator trusts.
	ErrWrongIssuer = errors.New("authn: the token names an issuer this validator does not trust")

	// ErrWrongAudience: aud does not include this validator's audience.
	ErrWrongAudience = errors.New("authn: the token is for a different audience")

	// ErrMissingScope: the principal lacks a scope [Principal.RequireScope]
	// demands.
	ErrMissingScope = errors.New("authn: the token is missing a required scope")

	// ErrUnknownKey: the token's kid — or its absence — matches nothing in
	// the key set, after a refetch where the rate limit allows one.
	ErrUnknownKey = errors.New("authn: the token's key is not in the issuer's key set")

	// ErrKeySetUnavailable: the JWKS could not be fetched, or held no keys.
	// Fail closed: an unreachable issuer is a rejection, never an accept.
	ErrKeySetUnavailable = errors.New("authn: the issuer's key set is unavailable")
)

// algorithm is the one signature algorithm this validator accepts. Pinning
// it — rather than believing the token's own alg header — is the standard
// defence against the JWT confusion attacks: alg "none" and an HMAC alg fed
// the public key as its secret both die here by name.
const algorithm = jwa.RS256

// Principal is what a validated token says about its bearer: the mapping
// from claims, including scope derivation, lives here in one place because
// ADR 006 names it as code that breaks.
type Principal struct {
	// Subject is the sub claim.
	Subject string

	// Scopes is the scope claim split on spaces (RFC 8693's form, the one
	// voya mints); nil when the claim is absent.
	Scopes []string

	// Tenant is the tenant claim, if the token carries one — voya's
	// personas put tenant ids in their extra claims.
	Tenant string

	// Claims is everything else the token carried beyond the registered
	// claims, tenant and scope included, for whatever the service needs
	// that this struct does not name.
	Claims map[string]any
}

// HasScope reports whether the principal carries scope.
func (p *Principal) HasScope(scope string) bool {
	for _, s := range p.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// RequireScope is HasScope as a gate: nil, or [ErrMissingScope] naming what
// was demanded and what the token carried.
func (p *Principal) RequireScope(scope string) error {
	if p.HasScope(scope) {
		return nil
	}
	return fmt.Errorf("%w: need %q, token carries %q", ErrMissingScope, scope, strings.Join(p.Scopes, " "))
}

// Validator validates tokens from exactly one issuer. The issuer is a single
// string by construction — the type cannot hold an allowlist — because a
// service that accepted two issuers would accept voya-signed tokens in
// production (ADR 006).
type Validator struct {
	issuer   string
	audience string
	jwksURL  string
	client   *http.Client
	now      func() time.Time
	refetch  time.Duration

	// fetchTimeout is [jwksFetchTimeout]. A field so a test can wait for it.
	fetchTimeout time.Duration

	// mu guards the key set and the bookkeeping of the fetch that fills it,
	// and is never held across that fetch: a validation whose key is cached
	// must not wait on a slow issuer. fetching is non-nil while a fetch runs,
	// so callers that need its answer wait for that one rather than racing
	// many.
	mu        sync.Mutex
	keys      jwk.Set
	lastFetch time.Time
	fetching  *keyFetch
}

// keyFetch is one trip to the issuer, shared by everyone waiting on it. err is
// written before done closes, so close is the edge every reader of it crosses.
type keyFetch struct {
	done chan struct{}
	err  error
}

// Option adjusts a Validator; the defaults are the production behaviour.
type Option func(*Validator)

// WithHTTPClient sets the client the JWKS is fetched with — a development
// process reaching voya.internal routes it through voya's dialer this way.
func WithHTTPClient(c *http.Client) Option { return func(v *Validator) { v.client = c } }

// WithRefetchInterval overrides [DefaultRefetchInterval]; zero means every
// unknown kid may fetch and the cached set never goes stale of its own
// accord — it turns the rate limit off, not the cache. For tests, mostly.
func WithRefetchInterval(d time.Duration) Option { return func(v *Validator) { v.refetch = d } }

// WithNow sets the clock, for tests that put now on either side of [Leeway].
func WithNow(now func() time.Time) Option { return func(v *Validator) { v.now = now } }

// New builds the validator for one environment, one issuer, one audience.
// Every argument is load-bearing and checked here, at startup, because a
// misconfigured validator must refuse to exist rather than misvalidate
// later: an unset environment, an empty issuer, JWKS URL or audience, and —
// the assertion ADR 006 exists for — a Production environment pointed at an
// issuer inside voya's DNS zone, or at a JWKS URL that is not https, all fail
// closed, naming what they were given.
func New(env Environment, issuer, jwksURL, audience string, opts ...Option) (*Validator, error) {
	switch env {
	case Development, Production:
	default:
		return nil, fmt.Errorf("authn: no environment chosen (got %d): pass authn.Development or authn.Production — a default here would be an auth bypass waiting for the build that forgets it", env)
	}
	if issuer == "" || jwksURL == "" || audience == "" {
		return nil, fmt.Errorf("authn: issuer (%q), JWKS URL (%q) and audience (%q) must all be set", issuer, jwksURL, audience)
	}
	if _, err := url.Parse(issuer); err != nil {
		return nil, fmt.Errorf("authn: the issuer is not a URL: %w", err)
	}
	if _, err := url.Parse(jwksURL); err != nil {
		return nil, fmt.Errorf("authn: the JWKS URL is not a URL: %w", err)
	}
	if env == Production {
		// voya holds a signing key, and a service that trusted it in
		// production would accept tokens anyone's laptop can mint. Anything
		// under .internal is voya's zone — resolvable only through voya's
		// DNS — so a production build given such an issuer refuses to start
		// (ADR 006: fail closed at startup, not a warning). The JWKS URL is
		// held to the same rule because it is the actual trust anchor: an
		// impeccable issuer string is worth nothing if the keys behind it
		// come from a laptop's voya.
		for _, field := range []struct{ what, raw string }{{"issuer", issuer}, {"JWKS URL", jwksURL}} {
			if host := hostOf(field.raw); inVoyaZone(host) {
				return nil, fmt.Errorf("authn: a Production validator must not trust %s %q — %q is voya's development zone, and accepting it would accept tokens any laptop can mint (ADR 006)", field.what, field.raw, host)
			}
		}
		// The same anchor again: keys fetched in cleartext are whatever the
		// network says they are, so anyone on the path could sign tokens.
		if u, err := url.Parse(jwksURL); err != nil || u.Scheme != "https" || u.Hostname() == "" {
			return nil, fmt.Errorf("authn: a Production validator must fetch keys over https, got JWKS URL %q: anyone on the network path could replace plain http keys and forge tokens (ADR 006)", jwksURL)
		}
	}

	v := &Validator{
		issuer:   issuer,
		audience: audience,
		jwksURL:  jwksURL,
		client:   &http.Client{Timeout: jwksFetchTimeout},
		now:      time.Now,
		refetch:  DefaultRefetchInterval,

		fetchTimeout: jwksFetchTimeout,
	}
	for _, opt := range opts {
		opt(v)
	}
	return v, nil
}

// hostOf is the host [New]'s Production check compares, in the one spelling
// DNS would resolve: lower-cased, without the port, and without the trailing
// dot an FQDN may carry. A string with no scheme is parsed as an authority
// rather than left to [url.Parse], which reads "voya.internal:9777" as a
// scheme with an opaque part and hands back no host at all — the shape that
// walked three ordinary spellings of voya's issuer straight past the check.
// Anything that parses as no host anywhere is returned as it was given, so
// it is compared rather than waved through.
func hostOf(raw string) string {
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
	for _, candidate := range []string{host, "//" + host} {
		if u, err := url.Parse(candidate); err == nil && u.Host != "" {
			return strings.TrimSuffix(u.Hostname(), ".")
		}
	}
	return host
}

// inVoyaZone reports whether host is a name only voya's DNS serves: the
// .internal zone, the bare zone name included.
func inVoyaZone(host string) bool {
	return host == "internal" || strings.HasSuffix(host, ".internal")
}

// Validate turns token into a [Principal], or into a rejection: every reason
// this package names is one of the sentinels above, distinguishable with
// [errors.Is], and a token too malformed to be a JWS at all fails before any
// of them can apply. Everything a validator must check happens here, in
// order: the envelope and its alg, the key (fetching or refetching the JWKS
// as needed), the signature, then the claims — issuer, audience, and exp,
// nbf and iat with [Leeway] on each. exp is required: a signed token with no
// expiry would otherwise validate forever.
func (v *Validator) Validate(ctx context.Context, token []byte) (*Principal, error) {
	msg, err := jws.Parse(token)
	if err != nil {
		return nil, fmt.Errorf("authn: parsing the token: %w", err)
	}
	sigs := msg.Signatures()
	if len(sigs) != 1 {
		return nil, fmt.Errorf("%w: the token carries %d signatures, not 1", ErrBadSignature, len(sigs))
	}
	hdr := sigs[0].ProtectedHeaders()
	if alg := hdr.Algorithm(); alg != algorithm {
		return nil, fmt.Errorf("%w: the token says alg %q and this validator accepts only %q", ErrBadSignature, alg, algorithm)
	}
	kid := hdr.KeyID()
	if kid == "" {
		return nil, fmt.Errorf("%w: the token's header names no kid at all", ErrUnknownKey)
	}
	key, err := v.keyFor(ctx, kid)
	if err != nil {
		return nil, err
	}
	if keyAlg := key.Algorithm(); keyAlg != nil && keyAlg.String() != "" && keyAlg != algorithm {
		return nil, fmt.Errorf("%w: the token says alg %q and the key it names is for %q", ErrBadSignature, algorithm, keyAlg)
	}

	tok, err := jwt.Parse(token, jwt.WithKey(algorithm, key), jwt.WithValidate(false))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadSignature, err)
	}
	if tok.Expiration().IsZero() {
		return nil, fmt.Errorf("%w: the token carries no exp claim, and one that never expires is one this validator will not accept", ErrExpired)
	}
	err = jwt.Validate(tok,
		jwt.WithClock(jwt.ClockFunc(v.now)),
		jwt.WithAcceptableSkew(Leeway),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience))
	switch {
	case err == nil:
	case errors.Is(err, jwt.ErrTokenExpired()):
		return nil, fmt.Errorf("%w: %w", ErrExpired, err)
	case errors.Is(err, jwt.ErrTokenNotYetValid()), errors.Is(err, jwt.ErrInvalidIssuedAt()):
		// iat in the future is jwx's own validator and its own error; to a
		// caller it is the same rejection as nbf — the token is from a clock
		// ahead of this one, and is not valid yet.
		return nil, fmt.Errorf("%w: %w", ErrNotYetValid, err)
	case errors.Is(err, jwt.ErrInvalidIssuer()):
		return nil, fmt.Errorf("%w: token says %q, this validator trusts %q", ErrWrongIssuer, tok.Issuer(), v.issuer)
	case errors.Is(err, jwt.ErrInvalidAudience()):
		return nil, fmt.Errorf("%w: token says %q, this validator serves %q", ErrWrongAudience, tok.Audience(), v.audience)
	default:
		return nil, fmt.Errorf("authn: validating the claims: %w", err)
	}

	return principalFrom(tok), nil
}

// principalFrom is the claims-to-principal mapping and the scope derivation,
// in the one place ADR 006 asks for.
func principalFrom(tok jwt.Token) *Principal {
	p := &Principal{
		Subject: tok.Subject(),
		Claims:  tok.PrivateClaims(),
	}
	if scope, ok := p.Claims["scope"].(string); ok {
		p.Scopes = strings.Fields(scope)
	}
	if tenant, ok := p.Claims["tenant"].(string); ok {
		p.Tenant = tenant
	}
	return p
}

// stale reports whether the cached set has outlived the refetch interval, so
// a lookup may be answered out of it only while a fetch is asking the issuer
// again. This is what stops a key withdrawn from the JWKS — the revoked one —
// from being honoured until the process restarts. A zero interval keeps the
// cache indefinitely, which is [WithRefetchInterval]'s documented meaning: no
// rate limit, not no cache.
func (v *Validator) stale() bool {
	return v.refetch > 0 && !v.lastFetch.IsZero() && v.now().Sub(v.lastFetch) >= v.refetch
}

// keyFor returns the key kid names, fetching or refetching the JWKS when it
// has to: on the first call, whenever kid is not in the cached set — a
// rotation's new key looks exactly like that — and whenever the cached set
// has gone [Validator.stale]. At most one fetch runs per refetch interval
// either way, so a stream of forged kids cannot turn into a fetch storm.
//
// The fetch runs outside the lock, so an issuer that has stopped answering
// cannot stop every validation in the process, which forged kids could
// otherwise arrange: each one can start a fetch. A caller that needs the answer
// waits for the fetch in flight, for as long as its own context allows. That
// includes the caller that noticed the set was stale, even though its key is
// cached, because waiting for the issuer is how a withdrawn key gets refused.
// Others are answered from the old set until then, so a withdrawn key outlives
// the interval by the length of one fetch.
//
// The fetch belongs to no caller. It is everyone's answer, so it runs on a
// context none of them can cancel, with [Validator.fetch]'s bound in place of
// their deadlines. If the caller that started it could end it, a client hanging
// up could keep a withdrawn key alive by discarding each answer before it
// landed, and could leave the next caller answering "unavailable" out of a
// cache the fetch never got to fill.
//
// A fetch that fails or returns an empty set stores nothing, so the next
// allowed attempt retries rather than trusting a cached outage. A set already
// cached goes on answering while the issuer is unreachable, because "I could
// not reach the JWKS" is no evidence that a key it served seconds ago was
// withdrawn, and refusing every token for the length of someone else's outage
// is not failing closed, it is an outage of one's own.
func (v *Validator) keyFor(ctx context.Context, kid string) (jwk.Key, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	for {
		if v.keys != nil && (!v.stale() || v.fetching != nil) {
			if key, ok := v.keys.LookupKeyID(kid); ok {
				return key, nil
			}
		}
		if v.fetching == nil {
			break
		}
		// Back round afterwards rather than on to the rate limit: the fetch
		// may have found this kid.
		if err := v.awaitFetch(ctx, v.fetching); err != nil {
			return nil, err
		}
	}
	if !v.lastFetch.IsZero() && v.now().Sub(v.lastFetch) < v.refetch {
		if v.keys == nil {
			return nil, fmt.Errorf("%w: the last fetch of %s failed and the next may not run yet", ErrKeySetUnavailable, v.jwksURL)
		}
		return nil, fmt.Errorf("%w: kid %q, and the rate limit defers a refetch", ErrUnknownKey, kid)
	}

	f := v.startFetch(ctx)
	waited := v.awaitFetch(ctx, f)
	if v.keys != nil {
		// The new set if the fetch landed. Otherwise the old one, for the
		// reason above if it failed, or because this caller stopped waiting
		// and now stands where everyone else does while a fetch runs.
		if key, ok := v.keys.LookupKeyID(kid); ok {
			return key, nil
		}
	}
	switch {
	case waited != nil:
		return nil, waited
	case f.err != nil:
		return nil, f.err
	}
	return nil, fmt.Errorf("%w: kid %q is not in the %d-key set %s serves", ErrUnknownKey, kid, v.keys.Len(), v.jwksURL)
}

// startFetch books the interval's fetch and runs it in the background. The
// caller holds the lock.
func (v *Validator) startFetch(ctx context.Context) *keyFetch {
	f := &keyFetch{done: make(chan struct{})}
	v.lastFetch = v.now()
	v.fetching = f
	go func() {
		set, err := v.fetch(context.WithoutCancel(ctx))

		v.mu.Lock()
		defer v.mu.Unlock()
		switch {
		case err != nil:
			f.err = fmt.Errorf("%w: fetching %s: %w", ErrKeySetUnavailable, v.jwksURL, err)
		case set.Len() == 0:
			f.err = fmt.Errorf("%w: %s served an empty key set, which is not cached — an issuer with no keys can validate nothing", ErrKeySetUnavailable, v.jwksURL)
		default:
			v.keys = set
		}
		v.fetching = nil
		close(f.done)
	}()
	return f
}

// fetch asks the issuer for the key set, for at most the client's own Timeout
// or, without one, [jwksFetchTimeout].
//
// It makes the request itself rather than calling [jwk.Fetch]. That function
// hands the request to a pool of three workers shared by the whole process, and
// they call client.Get with no context: a deadline abandons the wait while the
// request runs on. An issuer that never answers would hold a worker for good,
// and three such fetches would stop every validator in the process from ever
// fetching again.
func (v *Validator) fetch(ctx context.Context) (jwk.Set, error) {
	bound := v.fetchTimeout
	if v.client.Timeout > 0 {
		bound = v.client.Timeout
	}
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return nil, err
	}
	res, err := v.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	return jwk.Parse(body)
}

// awaitFetch waits for f to end or for ctx to, with the lock released while it
// does. The caller holds the lock on entry and again on return, and must look
// at the cache again before trusting anything it read beforehand.
func (v *Validator) awaitFetch(ctx context.Context, f *keyFetch) error {
	v.mu.Unlock()
	defer v.mu.Lock()
	select {
	case <-f.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%w: waiting for the fetch of %s: %w", ErrKeySetUnavailable, v.jwksURL, ctx.Err())
	}
}
