package authn

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
)

// The one issuer and audience every test's validator trusts. Deliberately
// not voya's: this module must validate any issuer's tokens, and the tests
// minting their own keep the enforcement honest — authn cannot import
// voya's minting code (deps_test.go), and the proof that voya-minted
// tokens validate lives in the root module's tests.
const (
	issuer   = "https://idp.example"
	audience = "internal-api"
)

// newSigner draws an RSA key wrapped as a JWK with a kid and alg RS256 —
// the shape voya's JWKS serves (#209).
func newSigner(t *testing.T) jwk.Key {
	t.Helper()
	raw, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	key, err := jwk.FromRaw(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := jwk.AssignKeyID(key); err != nil {
		t.Fatal(err)
	}
	if err := key.Set(jwk.AlgorithmKey, jwa.RS256); err != nil {
		t.Fatal(err)
	}
	return key
}

// keyServer serves a JWKS document over HTTP and counts the fetches, so a
// test can assert not only what validated but whether a refetch ran.
type keyServer struct {
	t   *testing.T
	srv *httptest.Server

	// gate and entered exist once [keyServer.hold] has been called. Both are
	// written under mu before any fetch they apply to.
	gate    chan struct{}
	entered chan struct{}

	mu      sync.Mutex
	doc     []byte
	fetches int
}

func newKeyServer(t *testing.T, signers ...jwk.Key) *keyServer {
	t.Helper()
	s := &keyServer{t: t}
	s.serve(signers...)
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		gate, entered := s.gate, s.entered
		s.mu.Unlock()
		if gate != nil {
			// Held outside mu, or the fetch count and serve would wait too.
			entered <- struct{}{}
			<-gate
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.fetches++
		_, _ = w.Write(s.doc)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// hold makes every fetch from now on stop at the door, the shape of an issuer
// that accepted the connection and has not answered. Receiving from the
// returned channel says a fetch is in flight, which is the positive marker
// these tests need: without it a validation that finished promptly may only
// have run before the fetch began. release lets them through, and runs at
// cleanup regardless so the server can close.
func (s *keyServer) hold() (entered <-chan struct{}, release func()) {
	s.t.Helper()
	gate := make(chan struct{})
	arrivals := make(chan struct{}, 16)
	s.mu.Lock()
	s.gate, s.entered = gate, arrivals
	s.mu.Unlock()
	release = sync.OnceFunc(func() { close(gate) })
	s.t.Cleanup(release)
	return arrivals, release
}

// clock is a time a test moves while validations read it concurrently.
type clock struct{ nanos atomic.Int64 }

func newClock() *clock {
	c := new(clock)
	c.nanos.Store(time.Now().UnixNano())
	return c
}

func (c *clock) now() time.Time { return time.Unix(0, c.nanos.Load()) }

func (c *clock) advance(d time.Duration) { c.nanos.Add(int64(d)) }

// validating runs one validation on its own goroutine and hands the result
// back, so the test goroutine does the asserting.
func validating(ctx context.Context, v *Validator, tok []byte) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := v.Validate(ctx, tok)
		done <- err
	}()
	return done
}

// settled is what a validation reported, failing the test if it is still
// waiting. The limit is a hang detector rather than a latency claim: a
// validation stuck behind a held fetch would otherwise wait for the release
// this test is holding back.
func settled(t *testing.T, what string, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: still waiting", what)
		return nil
	}
}

// arrived waits for a fetch to reach the held door.
func arrived(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no fetch reached the issuer")
	}
}

// waiterSettle is how long a test gives a second caller to reach the wait
// before acting. Too short a value can only make such a test pass without
// exercising the wait, never fail one that should pass.
const waiterSettle = 50 * time.Millisecond

// serve replaces the document with the public halves of signers — a key
// rotation, from a client's point of view.
func (s *keyServer) serve(signers ...jwk.Key) {
	s.t.Helper()
	set := jwk.NewSet()
	for _, signer := range signers {
		pub, err := jwk.PublicKeyOf(signer)
		if err != nil {
			s.t.Fatal(err)
		}
		if err := set.AddKey(pub); err != nil {
			s.t.Fatal(err)
		}
	}
	doc, err := json.Marshal(set)
	if err != nil {
		s.t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.doc = doc
}

func (s *keyServer) fetchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetches
}

// token builds and signs a token that the fixture validator accepts, after
// mod has had its way with the builder — nil for a fully valid one.
func token(t *testing.T, signer jwk.Key, mod func(*jwt.Builder)) []byte {
	t.Helper()
	now := time.Now()
	b := jwt.NewBuilder().
		Issuer(issuer).
		Audience([]string{audience}).
		Subject("someone").
		IssuedAt(now).
		NotBefore(now).
		Expiration(now.Add(time.Hour)).
		Claim("scope", "read write")
	if mod != nil {
		mod(b)
	}
	tok, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256, signer))
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

// validator builds one against s in Development with the fixture issuer and
// audience; refetch interval zero so rotation tests need no waiting —
// TestTheRefetchIsRateLimited sets its own.
func validator(t *testing.T, s *keyServer, opts ...Option) *Validator {
	t.Helper()
	opts = append([]Option{WithRefetchInterval(0)}, opts...)
	v, err := New(Development, issuer, s.srv.URL, audience, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAValidTokenYieldsItsPrincipal(t *testing.T) {
	signer := newSigner(t)
	v := validator(t, newKeyServer(t, signer))
	tok := token(t, signer, func(b *jwt.Builder) {
		b.Claim("tenant", "acme").Claim("team", "billing")
	})

	p, err := v.Validate(context.Background(), tok)
	if err != nil {
		t.Fatalf("a valid token was rejected: %v", err)
	}
	if p.Subject != "someone" {
		t.Errorf("Subject = %q, want %q", p.Subject, "someone")
	}
	if strings.Join(p.Scopes, ",") != "read,write" {
		t.Errorf("Scopes = %v, want [read write]", p.Scopes)
	}
	if p.Tenant != "acme" {
		t.Errorf("Tenant = %q, want %q", p.Tenant, "acme")
	}
	if p.Claims["team"] != "billing" {
		t.Errorf("Claims[team] = %v, want %q", p.Claims["team"], "billing")
	}
	if err := p.RequireScope("write"); err != nil {
		t.Errorf("RequireScope(write) = %v on a token carrying it", err)
	}
}

// TestTheInvalidShapesAreDistinguishable: ADR 006's four invalid shapes,
// each rejected for its own reason and no other — the caller can tell them
// apart by errors.Is, and a rejection matching the wrong sentinel fails
// here even though it "rejected".
func TestTheInvalidShapesAreDistinguishable(t *testing.T) {
	signer := newSigner(t)
	v := validator(t, newKeyServer(t, signer))
	sentinels := map[string]error{
		"expired":        ErrExpired,
		"wrong audience": ErrWrongAudience,
		"bad signature":  ErrBadSignature,
		"missing scope":  ErrMissingScope,
		"wrong issuer":   ErrWrongIssuer,
		"not yet valid":  ErrNotYetValid,
	}
	// missing scope is not Validate's to reject — a token with no scopes is
	// valid, and the rejection is RequireScope's — so each case carries its
	// own way of turning a token into an error.
	cases := []struct {
		name string
		err  func() error
	}{
		{"expired", func() error {
			iat := time.Now().Add(-2 * time.Hour)
			_, err := v.Validate(context.Background(), token(t, signer, func(b *jwt.Builder) {
				b.IssuedAt(iat).NotBefore(iat).Expiration(iat.Add(time.Hour))
			}))
			return err
		}},
		{"wrong audience", func() error {
			_, err := v.Validate(context.Background(), token(t, signer, func(b *jwt.Builder) {
				b.Audience([]string{"not-this-service"})
			}))
			return err
		}},
		{"bad signature", func() error {
			_, err := v.Validate(context.Background(), corrupt(t, token(t, signer, nil)))
			return err
		}},
		{"missing scope", func() error {
			// mustBuild's token carries no scope claim at all — absent, not
			// empty, the shape voya mints for the no-scopes persona.
			noScope, err := jwt.Sign(mustBuild(t), jwt.WithKey(jwa.RS256, signer))
			if err != nil {
				return err
			}
			p, err := v.Validate(context.Background(), noScope)
			if err != nil {
				return fmt.Errorf("a token with no scope claim must validate — absent scopes are RequireScope's to refuse: %w", err)
			}
			return p.RequireScope("write")
		}},
		{"wrong issuer", func() error {
			_, err := v.Validate(context.Background(), token(t, signer, func(b *jwt.Builder) {
				b.Issuer("https://elsewhere.example")
			}))
			return err
		}},
		{"not yet valid", func() error {
			_, err := v.Validate(context.Background(), token(t, signer, func(b *jwt.Builder) {
				b.NotBefore(time.Now().Add(time.Hour))
			}))
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.err()
			if err == nil {
				t.Fatal("the invalid shape was accepted")
			}
			for name, sentinel := range sentinels {
				if is := errors.Is(err, sentinel); is != (name == tc.name) {
					t.Errorf("errors.Is(err, %s) = %v for the %s shape: %v", name, is, tc.name, err)
				}
			}
		})
	}
}

// corrupt flips one bit of the signature segment, re-encoded so the token
// still parses: the one thing wrong is the signature.
func corrupt(t *testing.T, tok []byte) []byte {
	t.Helper()
	s := string(tok)
	i := strings.LastIndexByte(s, '.')
	sig, err := base64.RawURLEncoding.DecodeString(s[i+1:])
	if err != nil || len(sig) == 0 {
		t.Fatalf("no signature segment to corrupt in %q", s)
	}
	sig[0] ^= 0x01
	return []byte(s[:i+1] + base64.RawURLEncoding.EncodeToString(sig))
}

// TestLeewayHoldsOnBothSides pins [Leeway] as a number, not a mood: one
// second inside it a skewed token passes, one second outside it fails, on
// both the exp and the nbf edge. The clock is injected, so nothing here
// waits or races the wall clock.
func TestLeewayHoldsOnBothSides(t *testing.T) {
	signer := newSigner(t)
	at := func(now time.Time) *Validator {
		return validator(t, newKeyServer(t, signer), WithNow(func() time.Time { return now }))
	}
	base := time.Now()
	expiresAtBase := func(b *jwt.Builder) {
		b.IssuedAt(base.Add(-time.Hour)).NotBefore(base.Add(-time.Hour)).Expiration(base)
	}

	if _, err := at(base.Add(Leeway-time.Second)).Validate(context.Background(), token(t, signer, expiresAtBase)); err != nil {
		t.Errorf("1s inside the exp leeway must pass: %v", err)
	}
	if _, err := at(base.Add(Leeway+time.Second)).Validate(context.Background(), token(t, signer, expiresAtBase)); !errors.Is(err, ErrExpired) {
		t.Errorf("1s outside the exp leeway must be ErrExpired, got: %v", err)
	}

	// iat sits an hour back so only nbf is on the edge: jwx validates iat
	// too, and an iat in the future would fail first and mask the case.
	notBeforeBase := func(b *jwt.Builder) {
		b.IssuedAt(base.Add(-time.Hour)).NotBefore(base).Expiration(base.Add(time.Hour))
	}
	if _, err := at(base.Add(-Leeway+time.Second)).Validate(context.Background(), token(t, signer, notBeforeBase)); err != nil {
		t.Errorf("1s inside the nbf leeway must pass: %v", err)
	}
	if _, err := at(base.Add(-Leeway-time.Second)).Validate(context.Background(), token(t, signer, notBeforeBase)); !errors.Is(err, ErrNotYetValid) {
		t.Errorf("1s outside the nbf leeway must be ErrNotYetValid, got: %v", err)
	}
}

// TestAnUnknownKidTriggersARefetch: rotation. The validator has cached the
// old key set when a token under the new key arrives; the unknown kid is
// what a rotation looks like, so it refetches and validates — no restart.
func TestAnUnknownKidTriggersARefetch(t *testing.T) {
	old, rotated := newSigner(t), newSigner(t)
	s := newKeyServer(t, old)
	v := validator(t, s)

	if _, err := v.Validate(context.Background(), token(t, old, nil)); err != nil {
		t.Fatalf("under the old key: %v", err)
	}
	s.serve(rotated)
	if _, err := v.Validate(context.Background(), token(t, rotated, nil)); err != nil {
		t.Fatalf("a token under the rotated key must trigger a refetch and validate: %v", err)
	}
	if got := s.fetchCount(); got != 2 {
		t.Errorf("fetches = %d, want 2 (the initial fetch and the rotation's)", got)
	}
}

// TestTheRefetchIsRateLimited: a stream of unknown kids is at most one
// fetch per interval — the rest are ErrUnknownKey, not a fetch storm an
// attacker can drive by inventing kids.
func TestTheRefetchIsRateLimited(t *testing.T) {
	signer, unknown := newSigner(t), newSigner(t)
	s := newKeyServer(t, signer)
	v := validator(t, s, WithRefetchInterval(time.Hour))

	if _, err := v.Validate(context.Background(), token(t, signer, nil)); err != nil {
		t.Fatalf("priming the cache: %v", err)
	}
	for range 5 {
		if _, err := v.Validate(context.Background(), token(t, unknown, nil)); !errors.Is(err, ErrUnknownKey) {
			t.Fatalf("an unknown kid inside the rate limit must be ErrUnknownKey, got: %v", err)
		}
	}
	if got := s.fetchCount(); got != 1 {
		t.Errorf("fetches = %d, want 1: the rate limit must hold however many unknown kids arrive", got)
	}
}

// TestAnUnreachableJWKSFailsClosed: the endpoint being down — or too slow
// for the caller's deadline — is a rejection, never an accept.
func TestAnUnreachableJWKSFailsClosed(t *testing.T) {
	signer := newSigner(t)
	tok := token(t, signer, nil)

	t.Run("down", func(t *testing.T) {
		s := newKeyServer(t, signer)
		s.srv.Close()
		if _, err := validator(t, s).Validate(context.Background(), tok); !errors.Is(err, ErrKeySetUnavailable) {
			t.Errorf("want ErrKeySetUnavailable with the endpoint down, got: %v", err)
		}
	})
	t.Run("slow", func(t *testing.T) {
		stall := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-stall }))
		defer srv.Close()
		// After srv.Close in source so it runs first: Close waits for the
		// stalled handler, which only this unblocks.
		defer close(stall)
		v, err := New(Development, issuer, srv.URL, audience, WithRefetchInterval(0))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if _, err := v.Validate(ctx, tok); !errors.Is(err, ErrKeySetUnavailable) {
			t.Errorf("want ErrKeySetUnavailable when the fetch outlives the deadline, got: %v", err)
		}
	})
}

// TestAnEmptyKeySetFailsClosedAndIsNotCached: an issuer serving no keys can
// validate nothing — and "no keys" is not an answer worth keeping, so the
// moment the endpoint recovers the next allowed fetch sees the real set.
func TestAnEmptyKeySetFailsClosedAndIsNotCached(t *testing.T) {
	signer := newSigner(t)
	s := newKeyServer(t) // no keys at all: {"keys":[]}
	v := validator(t, s)
	tok := token(t, signer, nil)

	if _, err := v.Validate(context.Background(), tok); !errors.Is(err, ErrKeySetUnavailable) {
		t.Fatalf("want ErrKeySetUnavailable on an empty key set, got: %v", err)
	}
	s.serve(signer)
	if _, err := v.Validate(context.Background(), tok); err != nil {
		t.Fatalf("the empty answer was cached: after the endpoint recovered, validation still fails: %v", err)
	}
}

// TestATokenWithoutAKidIsRejected: no kid, no key selection, no accept —
// and no fetch either, since there is nothing to look for.
func TestATokenWithoutAKidIsRejected(t *testing.T) {
	raw, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer := newSigner(t)
	s := newKeyServer(t, signer)

	tok, err := jwt.Sign(mustBuild(t), jwt.WithKey(jwa.RS256, raw)) // a raw key carries no kid
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validator(t, s).Validate(context.Background(), tok); !errors.Is(err, ErrUnknownKey) || !strings.Contains(err.Error(), "no kid") {
		t.Errorf("want ErrUnknownKey naming the absent kid, got: %v", err)
	}
	if got := s.fetchCount(); got != 0 {
		t.Errorf("fetches = %d, want 0: a kid-less token names nothing a fetch could find", got)
	}
}

func mustBuild(t *testing.T) jwt.Token {
	t.Helper()
	now := time.Now()
	tok, err := jwt.NewBuilder().Issuer(issuer).Audience([]string{audience}).Subject("someone").
		IssuedAt(now).NotBefore(now).Expiration(now.Add(time.Hour)).Build()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// TestAlgNoneIsRejectedByName: the classic. A token claiming alg "none"
// dies on the pinned algorithm, with "none" in the error, before any key is
// consulted.
func TestAlgNoneIsRejectedByName(t *testing.T) {
	signer := newSigner(t)
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	claims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(
		`{"iss":%q,"aud":%q,"sub":"someone","exp":%d}`, issuer, audience, time.Now().Add(time.Hour).Unix())))
	tok := []byte(header + "." + claims + ".")

	_, err := validator(t, newKeyServer(t, signer)).Validate(context.Background(), tok)
	if !errors.Is(err, ErrBadSignature) || !strings.Contains(err.Error(), `"none"`) {
		t.Errorf("want ErrBadSignature naming alg none, got: %v", err)
	}
}

// TestAlgConfusionIsRejected: the other classic, both halves. A token whose
// alg is HS256 — hoping the public key becomes the HMAC secret — dies on
// the pinned algorithm by name; and a token saying RS256 over a key the set
// declares for a different algorithm dies on the disagreement.
func TestAlgConfusionIsRejected(t *testing.T) {
	signer := newSigner(t)

	t.Run("HS256 against the pin", func(t *testing.T) {
		secret, err := jwk.FromRaw([]byte("the-public-key-as-hmac-secret"))
		if err != nil {
			t.Fatal(err)
		}
		if err := secret.Set(jwk.KeyIDKey, signer.KeyID()); err != nil {
			t.Fatal(err)
		}
		tok, err := jwt.Sign(mustBuild(t), jwt.WithKey(jwa.HS256, secret))
		if err != nil {
			t.Fatal(err)
		}
		_, err = validator(t, newKeyServer(t, signer)).Validate(context.Background(), tok)
		if !errors.Is(err, ErrBadSignature) || !strings.Contains(err.Error(), "HS256") {
			t.Errorf("want ErrBadSignature naming HS256, got: %v", err)
		}
	})
	t.Run("key declared for another alg", func(t *testing.T) {
		other := newSigner(t)
		if err := other.Set(jwk.AlgorithmKey, jwa.PS256); err != nil {
			t.Fatal(err)
		}
		tok, err := jwt.Sign(mustBuild(t), jwt.WithKey(jwa.RS256, other))
		if err != nil {
			t.Fatal(err)
		}
		_, err = validator(t, newKeyServer(t, other)).Validate(context.Background(), tok)
		if !errors.Is(err, ErrBadSignature) || !strings.Contains(err.Error(), "PS256") {
			t.Errorf("want ErrBadSignature naming the key's PS256, got: %v", err)
		}
	})
}

// TestADifferentKeyBehindTheSameKidIsRejected: same issuer, same kid,
// different key — a forged token wearing the real key's name. The kid
// selects the real key and the signature then does not verify.
func TestADifferentKeyBehindTheSameKidIsRejected(t *testing.T) {
	real, forger := newSigner(t), newSigner(t)
	if err := forger.Set(jwk.KeyIDKey, real.KeyID()); err != nil {
		t.Fatal(err)
	}
	tok := token(t, forger, nil)
	if _, err := validator(t, newKeyServer(t, real)).Validate(context.Background(), tok); !errors.Is(err, ErrBadSignature) {
		t.Errorf("want ErrBadSignature for a different key behind the real kid, got: %v", err)
	}
}

// TestConcurrentValidationDuringARefetch: many validations racing the very
// first fetch — the cache is empty, so every one of them wants it. All must
// succeed, exactly one fetch may run, and the race detector (make test)
// holds the cache honest.
func TestConcurrentValidationDuringARefetch(t *testing.T) {
	signer := newSigner(t)
	s := newKeyServer(t, signer)
	v := validator(t, s)
	tok := token(t, signer, nil)

	const validations = 32
	errs := make(chan error, validations)
	var wg sync.WaitGroup
	for range validations {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := v.Validate(context.Background(), tok)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("a concurrent validation failed: %v", err)
		}
	}
	if got := s.fetchCount(); got != 1 {
		t.Errorf("fetches = %d, want 1: concurrent misses share one fetch", got)
	}
}

// TestARefreshInFlightDoesNotDelayACachedKey is #308: a validation whose key
// is cached has no use for the issuer's answer, so an issuer that has stopped
// answering must not stall it. The trigger is the cheap one an attacker has:
// any token naming a kid nobody issued starts a fetch.
func TestARefreshInFlightDoesNotDelayACachedKey(t *testing.T) {
	signer := newSigner(t)
	s := newKeyServer(t, signer)
	v := validator(t, s)
	tok := token(t, signer, nil)
	if _, err := v.Validate(context.Background(), tok); err != nil {
		t.Fatalf("priming the cache: %v", err)
	}

	entered, release := s.hold()
	forged := validating(context.Background(), v, token(t, newSigner(t), nil))
	arrived(t, entered)

	if err := settled(t, "a cached key during a refresh", validating(context.Background(), v, tok)); err != nil {
		t.Errorf("a cached key must validate while a refresh is in flight: %v", err)
	}

	select {
	case err := <-forged:
		t.Fatalf("the forged token did not wait for the fetch it started: %v", err)
	default:
	}
	release()
	if err := settled(t, "the forged token", forged); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("the forged token, once the issuer answered: %v, want ErrUnknownKey", err)
	}
}

// TestAFetchOutlastingTheIntervalDoesNotDelayACachedKey: the fetch is stamped
// when it starts, so a slow one can outlive the interval and leave the set
// stale with the refresh still running. Starting no second fetch is right;
// making everyone wait for the first is the stall again.
func TestAFetchOutlastingTheIntervalDoesNotDelayACachedKey(t *testing.T) {
	signer := newSigner(t)
	s := newKeyServer(t, signer)
	clk := newClock()
	v := validator(t, s, WithRefetchInterval(time.Minute), WithNow(clk.now))
	tok := token(t, signer, nil)
	if _, err := v.Validate(context.Background(), tok); err != nil {
		t.Fatalf("priming the cache: %v", err)
	}

	entered, release := s.hold()
	clk.advance(time.Minute + time.Second)
	first := validating(context.Background(), v, tok)
	arrived(t, entered)
	clk.advance(time.Minute + time.Second)

	if err := settled(t, "a cached key during a slow refresh", validating(context.Background(), v, tok)); err != nil {
		t.Errorf("a cached key must validate while a slow refresh runs: %v", err)
	}
	release()
	if err := settled(t, "the validation that started the refresh", first); err != nil {
		t.Errorf("the refresh found the key still served, so this validates: %v", err)
	}
	if got := s.fetchCount(); got != 2 {
		t.Errorf("fetches = %d, want 2: the priming fetch and one refresh", got)
	}
}

// TestAWithdrawnKeyLastsOnlyUntilTheFetchInFlightAnswers is the price of the
// two tests above, stated: the validation that noticed the stale set waits for
// the issuer, and that is what still withdraws a key. Others are answered from
// the old set meanwhile, so a withdrawn key outlives the interval by one fetch
// and no more.
func TestAWithdrawnKeyLastsOnlyUntilTheFetchInFlightAnswers(t *testing.T) {
	withdrawn, kept := newSigner(t), newSigner(t)
	s := newKeyServer(t, withdrawn, kept)
	clk := newClock()
	v := validator(t, s, WithRefetchInterval(time.Minute), WithNow(clk.now))
	tok := token(t, withdrawn, nil)
	if _, err := v.Validate(context.Background(), tok); err != nil {
		t.Fatalf("priming the cache: %v", err)
	}

	s.serve(kept)
	entered, release := s.hold()
	clk.advance(time.Minute + time.Second)
	noticed := validating(context.Background(), v, tok)
	arrived(t, entered)

	if err := settled(t, "during the fetch", validating(context.Background(), v, tok)); err != nil {
		t.Errorf("during the fetch the old set still answers, by design: %v", err)
	}
	release()
	if err := settled(t, "the validation that noticed", noticed); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("the validation that noticed the stale set waits for the issuer's verdict: %v, want ErrUnknownKey", err)
	}
	if _, err := v.Validate(context.Background(), tok); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("once the issuer has answered the key is gone: %v, want ErrUnknownKey", err)
	}
}

// TestAnUnknownKidWaitsForTheFetchInFlight: a rotation's new key looks like a
// miss, and the fetch that could answer it is already running. Telling this
// caller "rate limited" would refuse a good token over a fetch that was about
// to find its key.
func TestAnUnknownKidWaitsForTheFetchInFlight(t *testing.T) {
	old, rotated := newSigner(t), newSigner(t)
	s := newKeyServer(t, old)
	clk := newClock()
	v := validator(t, s, WithRefetchInterval(time.Minute), WithNow(clk.now))
	if _, err := v.Validate(context.Background(), token(t, old, nil)); err != nil {
		t.Fatalf("priming the cache: %v", err)
	}

	s.serve(old, rotated)
	entered, release := s.hold()
	clk.advance(time.Minute + time.Second)
	tok := token(t, rotated, nil)
	first := validating(context.Background(), v, tok)
	arrived(t, entered)

	second := validating(context.Background(), v, tok)
	select {
	case err := <-second:
		t.Fatalf("the second caller answered without the fetch in flight: %v", err)
	case <-time.After(waiterSettle):
	}
	release()

	if err := settled(t, "the caller that started the fetch", first); err != nil {
		t.Errorf("the caller that started the fetch: %v", err)
	}
	if err := settled(t, "the caller that waited", second); err != nil {
		t.Errorf("the caller that waited: %v", err)
	}
	if got := s.fetchCount(); got != 2 {
		t.Errorf("fetches = %d, want 2: the priming fetch and one shared by both callers", got)
	}
}

// TestAWaiterLeavesWhenItsOwnContextEnds: the fetch is shared, and waiting on
// it must not outlast the waiter's own deadline.
func TestAWaiterLeavesWhenItsOwnContextEnds(t *testing.T) {
	signer := newSigner(t)
	s := newKeyServer(t, signer)
	v := validator(t, s)
	tok := token(t, signer, nil)

	entered, release := s.hold()
	first := validating(context.Background(), v, tok)
	arrived(t, entered)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiter := validating(ctx, v, tok)
	time.Sleep(waiterSettle)
	cancel()

	err := settled(t, "the cancelled waiter", waiter)
	if !errors.Is(err, ErrKeySetUnavailable) || !errors.Is(err, context.Canceled) {
		t.Errorf("the cancelled waiter got %v, want ErrKeySetUnavailable wrapping context.Canceled", err)
	}
	release()
	if err := settled(t, "the caller that started the fetch", first); err != nil {
		t.Errorf("the fetch was not the waiter's to end: %v", err)
	}
}

// TestAHangUpDoesNotEndTheSharedFetch: the fetch is everyone's answer, so the
// caller that happened to start it leaving must not take it away from the one
// waiting on it. One fetch in total is the assertion that matters: a fetch tied
// to the first caller's context would be abandoned here and the waiter would
// need its own.
func TestAHangUpDoesNotEndTheSharedFetch(t *testing.T) {
	signer := newSigner(t)
	s := newKeyServer(t, signer)
	// The real interval: the rate limit has to be on for this to test it.
	v, err := New(Development, issuer, s.srv.URL, audience)
	if err != nil {
		t.Fatal(err)
	}
	tok := token(t, signer, nil)

	entered, release := s.hold()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := validating(ctx, v, tok)
	arrived(t, entered)

	waiter := validating(context.Background(), v, tok)
	time.Sleep(waiterSettle)
	cancel()

	if err := settled(t, "the caller that hung up", first); !errors.Is(err, ErrKeySetUnavailable) {
		t.Errorf("the caller that hung up: %v, want ErrKeySetUnavailable", err)
	}
	release()
	if err := settled(t, "the waiter", waiter); err != nil {
		t.Errorf("the waiter must get the shared fetch's answer: %v", err)
	}
	if got := s.fetchCount(); got != 1 {
		t.Errorf("fetches = %d, want 1: hanging up must not abandon the fetch", got)
	}
}

// TestAHangUpCannotKeepAWithdrawnKeyAlive is what the test above is worth to
// an attacker. While a fetch runs a stale set still answers, so a client that
// could start a fetch with a forged kid and then hang up, over and over, would
// discard each answer before it landed and keep a withdrawn key validating
// past the interval. The fetch has to land, and the key has to be gone, with
// no further fetch.
func TestAHangUpCannotKeepAWithdrawnKeyAlive(t *testing.T) {
	withdrawn, kept := newSigner(t), newSigner(t)
	s := newKeyServer(t, withdrawn, kept)
	clk := newClock()
	v := validator(t, s, WithRefetchInterval(time.Minute), WithNow(clk.now))
	tok := token(t, withdrawn, nil)
	if _, err := v.Validate(context.Background(), tok); err != nil {
		t.Fatalf("priming the cache: %v", err)
	}

	s.serve(kept)
	entered, release := s.hold()
	clk.advance(time.Minute + time.Second)
	forgedKid := token(t, newSigner(t), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attacker := validating(ctx, v, forgedKid)
	arrived(t, entered)
	cancel()
	if err := settled(t, "the client that hung up", attacker); !errors.Is(err, context.Canceled) {
		t.Fatalf("the client that hung up: %v, want context.Canceled", err)
	}

	// A kid in no set waits for the fetch in flight, which makes it the marker
	// that the fetch has landed rather than still being open.
	marker := validating(context.Background(), v, forgedKid)
	time.Sleep(waiterSettle)
	release()
	if err := settled(t, "the waiting caller", marker); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("the waiting caller: %v, want ErrUnknownKey", err)
	}

	if _, err := v.Validate(context.Background(), tok); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("the fetch the client hung up on has answered, so the key is gone: %v, want ErrUnknownKey", err)
	}
	if got := s.fetchCount(); got != 2 {
		t.Errorf("fetches = %d, want 2: the priming fetch and the one the client hung up on", got)
	}
}

// TestACallerOutOfTimeIsAnsweredLikeEveryoneElse: the caller that noticed the
// stale set waits for the issuer, but not past its own deadline. Once it stops
// waiting it stands where every other caller stands while a fetch runs, and
// refusing a cached key to the one caller that started the fetch would turn a
// slow issuer into a failed request.
func TestACallerOutOfTimeIsAnsweredLikeEveryoneElse(t *testing.T) {
	signer := newSigner(t)
	s := newKeyServer(t, signer)
	clk := newClock()
	v := validator(t, s, WithRefetchInterval(time.Minute), WithNow(clk.now))
	tok := token(t, signer, nil)
	if _, err := v.Validate(context.Background(), tok); err != nil {
		t.Fatalf("priming the cache: %v", err)
	}

	entered, _ := s.hold()
	clk.advance(time.Minute + time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	noticed := validating(ctx, v, tok)
	arrived(t, entered)
	cancel()
	if err := settled(t, "the validation that noticed", noticed); err != nil {
		t.Errorf("the key is cached and the fetch has not refused it: %v", err)
	}
}

// TestAFetchEndsWithoutADeadlineFromAnyone: [WithHTTPClient] may supply a
// client with no Timeout and a caller may set no deadline. An issuer that then
// never answers must not hold the fetch open, because while one runs no other
// can start and a stale set goes on answering. The handler's own context is
// the positive marker that the request was abandoned at the issuer's end, and
// not merely by this package: [jwk.Fetch] cannot do that.
func TestAFetchEndsWithoutADeadlineFromAnyone(t *testing.T) {
	signer := newSigner(t)
	aborted := make(chan struct{}, 1)
	giveUp := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			aborted <- struct{}{}
		case <-giveUp:
		}
	}))
	defer srv.Close()
	// After srv.Close in source so it runs first: Close waits for the handler,
	// and a fetch that never ends is the failure this test exists to report.
	defer close(giveUp)

	v, err := New(Development, issuer, srv.URL, audience, WithHTTPClient(&http.Client{}))
	if err != nil {
		t.Fatal(err)
	}
	v.fetchTimeout = 50 * time.Millisecond

	err = settled(t, "a validation against a silent issuer", validating(context.Background(), v, token(t, signer, nil)))
	if !errors.Is(err, ErrKeySetUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("got %v, want ErrKeySetUnavailable wrapping context.DeadlineExceeded", err)
	}
	select {
	case <-aborted:
	case <-time.After(5 * time.Second):
		t.Error("the issuer never saw the request abandoned, so the fetch is still running")
	}
}

// TestAClientsOwnTimeoutOverridesTheBound: the bound is for a client that sets
// no Timeout. One that does has said how long it is willing to wait.
func TestAClientsOwnTimeoutOverridesTheBound(t *testing.T) {
	signer := newSigner(t)
	s := newKeyServer(t, signer)
	v := validator(t, s, WithHTTPClient(&http.Client{Timeout: time.Minute}))
	v.fetchTimeout = time.Nanosecond
	if _, err := v.Validate(context.Background(), token(t, signer, nil)); err != nil {
		t.Errorf("a client with a Timeout of its own is not subject to the bound: %v", err)
	}
}

// TestNewRefusesAnUnchosenEnvironment: the zero value is invalid by design
// — there is no default environment, so a build cannot leave the choice
// silently absent (ADR 006's failure mode of forgetting).
func TestNewRefusesAnUnchosenEnvironment(t *testing.T) {
	for _, env := range []Environment{environmentUnset, Environment(7)} {
		if _, err := New(env, issuer, "https://idp.example/jwks", audience); err == nil ||
			!strings.Contains(err.Error(), "environment") {
			t.Errorf("New(%d, ...) = %v, want a refusal naming the missing environment", env, err)
		}
	}
}

// TestProductionRefusesVoyasIssuer is the test behind ADR 006's sharp edge:
// a Production validator pointed at an issuer in voya's zone must fail
// closed at startup, naming the issuer — voya holds a signing key, and
// accepting its issuer in production accepts tokens any laptop can mint.
// The same issuer in Development is the entire point of voya, so that half
// is the control.
func TestProductionRefusesVoyasIssuer(t *testing.T) {
	// voya's default issuer, and another name in the same zone: the refusal
	// is about the zone voya's DNS serves, not one spelling of it. The rest
	// are the spellings that reach the same nameserver and used to walk
	// past the check — DNS is case-insensitive, a trailing dot is the root
	// label an FQDN may carry, and a string with no scheme is one url.Parse
	// reads as a scheme with an opaque part, reporting no host at all.
	for _, iss := range []string{
		"http://voya.internal:9777",
		"http://idp.internal:9777",
		"http://VOYA.INTERNAL:9777",
		"http://Voya.Internal:9777",
		"http://voya.internal.:9777",
		"voya.internal:9777",
		"voya.internal",
	} {
		_, err := New(Production, iss, "https://cdn.example/jwks", audience)
		if err == nil || !strings.Contains(err.Error(), iss) {
			t.Errorf("New(Production, %q, ...) = %v, want a startup refusal naming the issuer", iss, err)
		}
		if _, err := New(Development, iss, "https://cdn.example/jwks", audience); err != nil {
			t.Errorf("New(Development, %q, ...) = %v: the same issuer in development is what voya is for", iss, err)
		}
	}
	if _, err := New(Production, "https://idp.example", "https://idp.example/jwks", audience); err != nil {
		t.Errorf("New(Production, ...) with a real issuer = %v, want nil", err)
	}
}

// TestNewRefusesAnEmptyConfiguration: every argument is load-bearing; a
// blank one fails at startup rather than misvalidating later.
func TestNewRefusesAnEmptyConfiguration(t *testing.T) {
	for _, tc := range []struct{ issuer, jwksURL, audience string }{
		{"", "https://idp.example/jwks", audience},
		{issuer, "", audience},
		{issuer, "https://idp.example/jwks", ""},
	} {
		if _, err := New(Development, tc.issuer, tc.jwksURL, tc.audience); err == nil {
			t.Errorf("New(Development, %q, %q, %q) accepted a blank", tc.issuer, tc.jwksURL, tc.audience)
		}
	}
}

// TestProductionRefusesVoyasJWKSURL: the key set is the trust anchor, so an
// impeccable issuer string does not save a production build whose keys come
// from a laptop's voya — what validates a signature is the key, not the iss
// claim it is compared against.
func TestProductionRefusesVoyasJWKSURL(t *testing.T) {
	const jwksURL = "http://voya.internal:9777/.well-known/jwks.json"
	_, err := New(Production, "https://idp.example", jwksURL, audience)
	if err == nil || !strings.Contains(err.Error(), jwksURL) {
		t.Errorf("New(Production, ..., %q, ...) = %v, want a startup refusal naming the JWKS URL", jwksURL, err)
	}
	if _, err := New(Development, "https://idp.example", jwksURL, audience); err != nil {
		t.Errorf("the same JWKS URL in development = %v: that is what voya is for", err)
	}
}

// TestProductionRefusesAJWKSURLThatIsNotHTTPS: keys fetched in cleartext can be
// replaced by anyone on the network path. "HTTPS" is the accepted control, since
// a URL's scheme is case-insensitive.
func TestProductionRefusesAJWKSURLThatIsNotHTTPS(t *testing.T) {
	for _, jwksURL := range []string{
		"http://idp.example/jwks",
		"ftp://idp.example/jwks",
		"idp.example/jwks",
		"https:///jwks",
	} {
		_, err := New(Production, "https://idp.example", jwksURL, audience)
		if err == nil || !strings.Contains(err.Error(), "over https") || !strings.Contains(err.Error(), jwksURL) {
			t.Errorf("New(Production, ..., %q, ...) = %v, want a refusal naming the URL", jwksURL, err)
		}
		if _, err := New(Development, "https://idp.example", jwksURL, audience); err != nil {
			t.Errorf("the same JWKS URL in development = %v: only Production is held to https", err)
		}
	}
	if _, err := New(Production, "https://idp.example", "HTTPS://idp.example/jwks", audience); err != nil {
		t.Errorf("an upper-case https scheme = %v, want nil", err)
	}
}

// TestATokenWithNoExpClaimIsRejected: jwx's expiry validator passes a token
// carrying no exp at all, so requiring one is this package's job. A signed
// bearer token that never expires is the shape a stolen one most wants to
// be, and it is refused as ErrExpired.
func TestATokenWithNoExpClaimIsRejected(t *testing.T) {
	signer := newSigner(t)
	v := validator(t, newKeyServer(t, signer))
	now := time.Now()
	tok, err := jwt.NewBuilder().Issuer(issuer).Audience([]string{audience}).Subject("someone").
		IssuedAt(now).NotBefore(now).Build()
	if err != nil {
		t.Fatal(err)
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256, signer))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Validate(context.Background(), signed); !errors.Is(err, ErrExpired) {
		t.Errorf("a token with no exp claim must be refused as ErrExpired, got: %v", err)
	}
}

// TestAnIssuedAtInTheFutureIsNotYetValid: jwx validates iat beside nbf and
// has its own error for it. To a caller the two are the same rejection —
// the token is from a clock ahead of this one — so iat maps to the same
// sentinel rather than reaching the caller as an opaque claims error
// matching nothing.
func TestAnIssuedAtInTheFutureIsNotYetValid(t *testing.T) {
	signer := newSigner(t)
	v := validator(t, newKeyServer(t, signer))
	tok := token(t, signer, func(b *jwt.Builder) { b.IssuedAt(time.Now().Add(2 * time.Hour)) })
	if _, err := v.Validate(context.Background(), tok); !errors.Is(err, ErrNotYetValid) {
		t.Errorf("an iat two hours ahead must be ErrNotYetValid, got: %v", err)
	}
}

// TestACancelledFetchDoesNotStrandTheNextCaller: one caller hanging up must
// not spend the interval's only fetch. The rate limit exists for forged kids,
// not for the caller's own context — a fetch abandoned with the caller answers
// every other caller ErrKeySetUnavailable for a full interval out of a cache
// it never got to fill, which one aborted request every ten seconds turns into
// a total outage.
func TestACancelledFetchDoesNotStrandTheNextCaller(t *testing.T) {
	signer := newSigner(t)
	s := newKeyServer(t, signer)
	// The real interval, not the fixture's zero: the rate limit has to be on
	// for this to be a test of it.
	v, err := New(Development, issuer, s.srv.URL, audience)
	if err != nil {
		t.Fatal(err)
	}
	tok := token(t, signer, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := v.Validate(ctx, tok); !errors.Is(err, ErrKeySetUnavailable) {
		t.Fatalf("the cancelling caller still fails closed, got: %v", err)
	}
	if _, err := v.Validate(context.Background(), tok); err != nil {
		t.Errorf("the next caller must get the fetch's answer: %v", err)
	}
}

// TestAWithdrawnKeyStopsValidatingWhenTheCacheGoesStale: a cache hit is only
// a hit while the set is fresh. Refetching solely on an unknown kid means a
// key the issuer *removed* — the revoked one — is honoured until the process
// restarts, which is the one direction of key rotation that matters for
// security.
func TestAWithdrawnKeyStopsValidatingWhenTheCacheGoesStale(t *testing.T) {
	withdrawn, kept := newSigner(t), newSigner(t)
	s := newKeyServer(t, withdrawn, kept)
	// now is read only from this goroutine's Validate calls.
	now := time.Now()
	v, err := New(Development, issuer, s.srv.URL, audience,
		WithRefetchInterval(time.Minute), WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	tok := token(t, withdrawn, nil)
	if _, err := v.Validate(context.Background(), tok); err != nil {
		t.Fatalf("priming the cache: %v", err)
	}

	s.serve(kept) // the issuer withdraws the key
	if _, err := v.Validate(context.Background(), tok); err != nil {
		t.Fatalf("inside the interval the cached set still answers, by design: %v", err)
	}
	now = now.Add(time.Minute + time.Second)
	if _, err := v.Validate(context.Background(), tok); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("a withdrawn key must stop validating once the cached set is older than the refetch interval, got: %v", err)
	}
	if got := s.fetchCount(); got != 2 {
		t.Errorf("fetches = %d, want 2: the initial fetch and the one the stale cache forced", got)
	}
}

// TestAStaleCacheOutlivesTheIssuersOutage is the other half of the rule
// above, and the reason it is not simply "expire the cache": an unreachable
// JWKS is no evidence that a key it served seconds ago was withdrawn.
// Refusing every token for the length of the issuer's outage would not be
// failing closed, it would be an outage of voya's own — so the last good set
// answers, and the fetch is retried each interval.
func TestAStaleCacheOutlivesTheIssuersOutage(t *testing.T) {
	signer := newSigner(t)
	s := newKeyServer(t, signer)
	now := time.Now()
	v, err := New(Development, issuer, s.srv.URL, audience,
		WithRefetchInterval(time.Minute), WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	tok := token(t, signer, nil)
	if _, err := v.Validate(context.Background(), tok); err != nil {
		t.Fatalf("priming the cache: %v", err)
	}

	s.srv.Close()
	now = now.Add(time.Minute + time.Second)
	if _, err := v.Validate(context.Background(), tok); err != nil {
		t.Errorf("a key the issuer served must go on validating while the issuer is down: %v", err)
	}
}

// TestDefaultClientHasATimeout pins the bound on a JWKS fetch: http.DefaultClient
// has none, and a hung issuer then blocks validation without limit.
func TestDefaultClientHasATimeout(t *testing.T) {
	v, err := New(Production, "https://issuer.example", "https://issuer.example/jwks.json", "aud")
	if err != nil {
		t.Fatal(err)
	}
	if v.client.Timeout != jwksFetchTimeout {
		t.Errorf("client timeout = %v, want %v", v.client.Timeout, jwksFetchTimeout)
	}
}
