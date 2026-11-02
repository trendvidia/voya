package interceptor_test

// The fixture here is a real connect-go server over HTTP/2, not a bag of
// mocked Specs: the streaming interceptor path is a different method from
// the unary one and this repo has already shipped a streaming hole that
// unary tests could not see (#173), so every assertion that matters runs
// against all four call shapes through the real dispatch.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/trendvidia/voya/authn"
	"github.com/trendvidia/voya/authn/interceptor"
)

const (
	issuer   = "https://idp.example"
	audience = "internal-api"

	unaryProc  = "/probe.v1.Probe/Unary"
	downProc   = "/probe.v1.Probe/Down"   // server-streaming
	upProc     = "/probe.v1.Probe/Up"     // client-streaming
	duplexProc = "/probe.v1.Probe/Duplex" // bidi-streaming
	openProc   = "/probe.v1.Probe/Open"   // the one procedure tests exempt
)

// newSigner draws an RSA key wrapped as a JWK with a kid and alg RS256, the
// shape voya's JWKS serves — the same fixture authn's own tests use.
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

// keyServer serves signer's public half as a JWKS document.
func keyServer(t *testing.T, signer jwk.Key) *httptest.Server {
	t.Helper()
	pub, err := jwk.PublicKeyOf(signer)
	if err != nil {
		t.Fatal(err)
	}
	set := jwk.NewSet()
	if err := set.AddKey(pub); err != nil {
		t.Fatal(err)
	}
	doc, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(doc)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// token signs a token the fixture validator accepts: subject "someone",
// scope "read write", an hour of life. mod may bend any of it.
func token(t *testing.T, signer jwk.Key, mod func(*jwt.Builder)) string {
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
	return string(signed)
}

// newAuth is a ready [interceptor.Auth]: a signer, its JWKS, a Development
// validator over both, with [openProc] exempt so the open path is reachable.
func newAuth(t *testing.T, opts ...authn.Option) (*interceptor.Auth, jwk.Key) {
	t.Helper()
	signer := newSigner(t)
	v, err := authn.New(authn.Development, issuer, keyServer(t, signer).URL, audience,
		append([]authn.Option{authn.WithRefetchInterval(0)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	a, err := interceptor.New(v, interceptor.Exempt(openProc))
	if err != nil {
		t.Fatal(err)
	}
	return a, signer
}

// identity is what every fixture handler answers with: the context's
// principal rendered as "sub=<subject>" or "anonymous", so a test asserts
// what the handler saw rather than merely that it ran.
func identity(ctx context.Context) *wrapperspb.StringValue {
	if p, ok := authn.PrincipalFrom(ctx); ok {
		return wrapperspb.String("sub=" + p.Subject)
	}
	return wrapperspb.String("anonymous")
}

// probeServer serves the five procedures over cleartext-free HTTP/2
// (httptest's TLS), each built with opts — which is where a test installs
// the interceptors under test.
func probeServer(t *testing.T, opts ...connect.HandlerOption) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(unaryProc, connect.NewUnaryHandler(unaryProc,
		func(ctx context.Context, _ *connect.Request[wrapperspb.StringValue]) (*connect.Response[wrapperspb.StringValue], error) {
			return connect.NewResponse(identity(ctx)), nil
		}, opts...))
	mux.Handle(openProc, connect.NewUnaryHandler(openProc,
		func(ctx context.Context, _ *connect.Request[wrapperspb.StringValue]) (*connect.Response[wrapperspb.StringValue], error) {
			return connect.NewResponse(identity(ctx)), nil
		}, opts...))
	mux.Handle(downProc, connect.NewServerStreamHandler(downProc,
		func(ctx context.Context, _ *connect.Request[wrapperspb.StringValue], stream *connect.ServerStream[wrapperspb.StringValue]) error {
			return stream.Send(identity(ctx))
		}, opts...))
	mux.Handle(upProc, connect.NewClientStreamHandler(upProc,
		func(ctx context.Context, stream *connect.ClientStream[wrapperspb.StringValue]) (*connect.Response[wrapperspb.StringValue], error) {
			for stream.Receive() {
			}
			if err := stream.Err(); err != nil {
				return nil, err
			}
			return connect.NewResponse(identity(ctx)), nil
		}, opts...))
	mux.Handle(duplexProc, connect.NewBidiStreamHandler(duplexProc,
		func(ctx context.Context, stream *connect.BidiStream[wrapperspb.StringValue, wrapperspb.StringValue]) error {
			for {
				if _, err := stream.Receive(); errors.Is(err, io.EOF) {
					return nil
				} else if err != nil {
					return err
				}
				if err := stream.Send(identity(ctx)); err != nil {
					return err
				}
			}
		}, opts...))
	srv := httptest.NewUnstartedServer(mux)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func client(srv *httptest.Server, proc string) *connect.Client[wrapperspb.StringValue, wrapperspb.StringValue] {
	return connect.NewClient[wrapperspb.StringValue, wrapperspb.StringValue](srv.Client(), srv.URL+proc)
}

// callUnary makes one unary call to proc with the given Authorization
// header values (none means no header) and reports what came back.
func callUnary(t *testing.T, srv *httptest.Server, proc string, authorization ...string) (string, error) {
	t.Helper()
	req := connect.NewRequest(wrapperspb.String("hello"))
	for _, v := range authorization {
		req.Header().Add("Authorization", v)
	}
	res, err := client(srv, proc).CallUnary(t.Context(), req)
	if err != nil {
		return "", err
	}
	return res.Msg.GetValue(), nil
}

// wantCode fails unless err is a *connect.Error with code, and returns it
// for further message assertions.
func wantCode(t *testing.T, err error, code connect.Code) *connect.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("the call succeeded, want %v", code)
	}
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("the call failed with %T (%v), not a *connect.Error", err, err)
	}
	if cerr.Code() != code {
		t.Fatalf("the call failed with %v, want %v:\n%v", cerr.Code(), code, cerr)
	}
	return cerr
}

// TestAnUnannotatedProcedureIsClosed is the default the whole design turns
// on: a procedure nobody exempted requires a token, on every one of the four
// call shapes — the streaming three through the handler-stream path a unary
// test cannot see (#173). The valid-token control in each case is what
// proves the interceptor was in the dispatch at all rather than the calls
// failing for some other reason.
func TestAnUnannotatedProcedureIsClosed(t *testing.T) {
	a, signer := newAuth(t)
	srv := probeServer(t, connect.WithInterceptors(a.Authn()))
	bearer := "Bearer " + token(t, signer, nil)

	t.Run("unary", func(t *testing.T) {
		_, err := callUnary(t, srv, unaryProc)
		wantCode(t, err, connect.CodeUnauthenticated)
		if got, err := callUnary(t, srv, unaryProc, bearer); err != nil || got != "sub=someone" {
			t.Fatalf("the control call = %q, %v; want sub=someone", got, err)
		}
	})
	t.Run("server-stream", func(t *testing.T) {
		stream, err := client(srv, downProc).CallServerStream(t.Context(), connect.NewRequest(wrapperspb.String("hello")))
		if err == nil {
			stream.Receive()
			err = stream.Err()
			_ = stream.Close()
		}
		wantCode(t, err, connect.CodeUnauthenticated)
	})
	t.Run("client-stream", func(t *testing.T) {
		stream := client(srv, upProc).CallClientStream(t.Context())
		_ = stream.Send(wrapperspb.String("hello"))
		_, err := stream.CloseAndReceive()
		wantCode(t, err, connect.CodeUnauthenticated)
	})
	t.Run("bidi-stream", func(t *testing.T) {
		stream := client(srv, duplexProc).CallBidiStream(t.Context())
		_ = stream.Send(wrapperspb.String("hello"))
		_, err := stream.Receive()
		wantCode(t, err, connect.CodeUnauthenticated)
		_ = stream.CloseRequest()
		_ = stream.CloseResponse()
	})
	t.Run("streams with a token", func(t *testing.T) {
		// The streaming controls, one per shape, so the three rejections
		// above are the interceptor's and not the fixture's.
		down, err := client(srv, downProc).CallServerStream(t.Context(), func() *connect.Request[wrapperspb.StringValue] {
			req := connect.NewRequest(wrapperspb.String("hello"))
			req.Header().Set("Authorization", bearer)
			return req
		}())
		if err != nil {
			t.Fatalf("server-stream with a token: %v", err)
		}
		if !down.Receive() {
			t.Fatalf("server-stream with a token received nothing: %v", down.Err())
		}
		if got := down.Msg().GetValue(); got != "sub=someone" {
			t.Errorf("server-stream handler saw %q, want sub=someone", got)
		}
		_ = down.Close()

		up := client(srv, upProc).CallClientStream(t.Context())
		up.RequestHeader().Set("Authorization", bearer)
		_ = up.Send(wrapperspb.String("hello"))
		res, err := up.CloseAndReceive()
		if err != nil {
			t.Fatalf("client-stream with a token: %v", err)
		}
		if got := res.Msg.GetValue(); got != "sub=someone" {
			t.Errorf("client-stream handler saw %q, want sub=someone", got)
		}

		duplex := client(srv, duplexProc).CallBidiStream(t.Context())
		duplex.RequestHeader().Set("Authorization", bearer)
		_ = duplex.Send(wrapperspb.String("hello"))
		msg, err := duplex.Receive()
		if err != nil {
			t.Fatalf("bidi-stream with a token: %v", err)
		}
		if got := msg.GetValue(); got != "sub=someone" {
			t.Errorf("bidi-stream handler saw %q, want sub=someone", got)
		}
		_ = duplex.CloseRequest()
		_ = duplex.CloseResponse()
	})
}

// TestTheHeaderShapesAreToldApart: missing, present-but-empty, wrong scheme,
// Bearer-with-nothing, and two headers are each CodeUnauthenticated with a
// message naming that shape — wrong-reason rejections are how auth bugs hide
// — and a lowercase "bearer" is not a shape at all: RFC 7235 makes the
// scheme case-insensitive, so it validates like any other credential.
func TestTheHeaderShapesAreToldApart(t *testing.T) {
	a, signer := newAuth(t)
	srv := probeServer(t, connect.WithInterceptors(a.Authn()))
	tok := token(t, signer, nil)

	for _, tc := range []struct {
		name    string
		headers []string
		phrase  string
	}{
		{"missing", nil, "no Authorization header"},
		{"present but empty", []string{""}, "present but empty"},
		{"another scheme", []string{"Basic dXNlcjpwYXNz"}, `scheme is "Basic"`},
		{"no prefix at all", []string{tok}, "this interceptor validates Bearer"},
		{"Bearer with nothing after it", []string{"Bearer "}, "no token after it"},
		{"two credentials", []string{"Bearer " + tok, "Bearer " + tok}, "2 Authorization headers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := callUnary(t, srv, unaryProc, tc.headers...)
			cerr := wantCode(t, err, connect.CodeUnauthenticated)
			if !strings.Contains(cerr.Message(), tc.phrase) {
				t.Errorf("the rejection does not name the shape (want %q):\n%s", tc.phrase, cerr.Message())
			}
			// Every row, not just the unschemed one: a message that quotes
			// the header back puts a live token in the caller's error and
			// in whatever renders it.
			if strings.Contains(cerr.Message(), tok) {
				t.Errorf("the rejection quotes the credential back:\n%s", cerr.Message())
			}
		})
	}
	t.Run("lowercase bearer is accepted", func(t *testing.T) {
		got, err := callUnary(t, srv, unaryProc, "bearer "+tok)
		if err != nil || got != "sub=someone" {
			t.Fatalf("lowercase bearer = %q, %v; want sub=someone — the scheme is case-insensitive (RFC 7235)", got, err)
		}
	})
}

// TestAnExemptProcedureIsOpenButNotBlind pins three decisions at once: an
// exempt procedure answers with no token; a valid token on it still puts the
// Principal in the context, because "no token required" is not "identity
// forbidden"; and an *invalid* token on it is still rejected, because a
// caller who presented a credential asked to be authenticated, and silently
// dropping an expired token would hide exactly the bug ADR 006 wants
// reproducible.
func TestAnExemptProcedureIsOpenButNotBlind(t *testing.T) {
	a, signer := newAuth(t)
	srv := probeServer(t, connect.WithInterceptors(a.Authn()))

	if got, err := callUnary(t, srv, openProc); err != nil || got != "anonymous" {
		t.Fatalf("the exempt procedure without a token = %q, %v; want anonymous", got, err)
	}
	if got, err := callUnary(t, srv, openProc, "Bearer "+token(t, signer, nil)); err != nil || got != "sub=someone" {
		t.Fatalf("the exempt procedure with a valid token = %q, %v; want sub=someone", got, err)
	}
	expired := token(t, signer, func(b *jwt.Builder) {
		b.Expiration(time.Now().Add(-time.Hour))
	})
	_, err := callUnary(t, srv, openProc, "Bearer "+expired)
	wantCode(t, err, connect.CodeUnauthenticated)
}

// requireWrite is the fixture policy: the write scope, or a refusal. The
// nil-principal branch is reachable only on an exempt procedure.
func requireWrite(_ context.Context, _ connect.Spec, p *authn.Principal) error {
	if p == nil {
		return errors.New("this operation writes, and anonymous callers may not")
	}
	return p.RequireScope("write")
}

// TestAuthzTellsTheTwoCodesApart is the reason there are two interceptors: a
// valid principal the policy refuses is CodePermissionDenied and never
// CodeUnauthenticated — a caller that cannot tell the two apart retries the
// wrong one — while no credential at all stays CodeUnauthenticated. On every
// call shape, for the #173 reason.
func TestAuthzTellsTheTwoCodesApart(t *testing.T) {
	a, signer := newAuth(t)
	srv := probeServer(t, connect.WithInterceptors(a.Authn(), a.Authz(requireWrite)))
	readOnly := "Bearer " + token(t, signer, func(b *jwt.Builder) {
		b.Subject("no-scopes").Claim("scope", "")
	})
	writer := "Bearer " + token(t, signer, nil)

	if got, err := callUnary(t, srv, unaryProc, writer); err != nil || got != "sub=someone" {
		t.Fatalf("the permitted call = %q, %v; want sub=someone", got, err)
	}
	cerr := wantCode(t, func() error { _, err := callUnary(t, srv, unaryProc, readOnly); return err }(), connect.CodePermissionDenied)
	if !errorsContains(cerr, "missing a required scope") {
		t.Errorf("the denial does not carry the policy's reason:\n%s", cerr.Message())
	}
	_, err := callUnary(t, srv, unaryProc)
	wantCode(t, err, connect.CodeUnauthenticated)

	// The same distinction through the streaming path.
	stream := client(srv, duplexProc).CallBidiStream(t.Context())
	stream.RequestHeader().Set("Authorization", readOnly)
	_ = stream.Send(wrapperspb.String("hello"))
	_, err = stream.Receive()
	wantCode(t, err, connect.CodePermissionDenied)
	_ = stream.CloseRequest()
	_ = stream.CloseResponse()

	// An anonymous caller on an exempt procedure the policy refuses gets
	// CodeUnauthenticated — the remedy is a token, not a permission.
	_, err = callUnary(t, srv, openProc)
	wantCode(t, err, connect.CodeUnauthenticated)
	// And with a permitted token the same exempt procedure answers: the
	// refusal above was the policy's, not the exemption breaking.
	if got, err := callUnary(t, srv, openProc, writer); err != nil || got != "sub=someone" {
		t.Fatalf("the exempt procedure with a permitted token = %q, %v; want sub=someone", got, err)
	}
}

func errorsContains(err error, phrase string) bool {
	return err != nil && strings.Contains(err.Error(), phrase)
}

// TestEitherInstallationOrderWorks: whichever of the pair runs first
// authenticates and files the Principal, so authz-before-authn behaves
// exactly like authn-before-authz — every outcome, both orders.
func TestEitherInstallationOrderWorks(t *testing.T) {
	a, signer := newAuth(t)
	writer := "Bearer " + token(t, signer, nil)
	readOnly := "Bearer " + token(t, signer, func(b *jwt.Builder) { b.Claim("scope", "read") })

	for name, order := range map[string][]connect.Interceptor{
		"authn then authz": {a.Authn(), a.Authz(requireWrite)},
		"authz then authn": {a.Authz(requireWrite), a.Authn()},
	} {
		t.Run(name, func(t *testing.T) {
			srv := probeServer(t, connect.WithInterceptors(order...))
			if got, err := callUnary(t, srv, unaryProc, writer); err != nil || got != "sub=someone" {
				t.Fatalf("the permitted call = %q, %v; want sub=someone", got, err)
			}
			_, err := callUnary(t, srv, unaryProc, readOnly)
			wantCode(t, err, connect.CodePermissionDenied)
			_, err = callUnary(t, srv, unaryProc)
			wantCode(t, err, connect.CodeUnauthenticated)
		})
	}
}

// TestATokenExpiringMidStreamDoesNotCutTheStream pins the documented
// decision: a stream is authenticated when it is accepted, for its whole
// life. The control at the end — a fresh call with the same token is
// rejected — is what proves the clock really moved past the expiry, so the
// surviving stream is the decision at work and not a test that never staged
// the expiry at all.
func TestATokenExpiringMidStreamDoesNotCutTheStream(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	a, signer := newAuth(t, authn.WithNow(clock))
	srv := probeServer(t, connect.WithInterceptors(a.Authn()))
	bearer := "Bearer " + token(t, signer, func(b *jwt.Builder) {
		b.IssuedAt(now).NotBefore(now).Expiration(now.Add(time.Minute))
	})

	stream := client(srv, duplexProc).CallBidiStream(t.Context())
	stream.RequestHeader().Set("Authorization", bearer)
	_ = stream.Send(wrapperspb.String("first"))
	if _, err := stream.Receive(); err != nil {
		t.Fatalf("the stream did not open with a live token: %v", err)
	}

	mu.Lock()
	now = now.Add(time.Hour) // far past the minute of life and the leeway
	mu.Unlock()

	_ = stream.Send(wrapperspb.String("second"))
	if _, err := stream.Receive(); err != nil {
		t.Fatalf("the token expired mid-stream and the stream was cut: %v — the decision is that it is not", err)
	}
	_ = stream.CloseRequest()
	_ = stream.CloseResponse()

	_, err := callUnary(t, srv, unaryProc, bearer)
	wantCode(t, err, connect.CodeUnauthenticated) // the control: the expiry really happened
}

// TestOnAClientTheInterceptorsAreInert: installed on a Connect client, both
// interceptors pass through — there is no incoming credential to check, and
// a client whose calls started failing CodeUnauthenticated locally would be
// authenticating itself to nobody.
func TestOnAClientTheInterceptorsAreInert(t *testing.T) {
	a, _ := newAuth(t)
	srv := probeServer(t) // no interceptors server-side, and no token sent
	c := connect.NewClient[wrapperspb.StringValue, wrapperspb.StringValue](
		srv.Client(), srv.URL+unaryProc,
		connect.WithInterceptors(a.Authn(), a.Authz(requireWrite)))
	res, err := c.CallUnary(t.Context(), connect.NewRequest(wrapperspb.String("hello")))
	if err != nil {
		t.Fatalf("a client-side install rejected an outgoing call: %v", err)
	}
	if got := res.Msg.GetValue(); got != "anonymous" {
		t.Errorf("the handler saw %q, want anonymous", got)
	}
}

// TestConstructionFailsClosed: the misconfigurations are refused at startup,
// naming what was wrong — not discovered per call.
func TestConstructionFailsClosed(t *testing.T) {
	if _, err := interceptor.New(nil); err == nil {
		t.Error("New accepted a nil validator")
	}
	v, err := authn.New(authn.Development, issuer, "http://127.0.0.1:1/jwks", audience)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"probe.v1.Probe/Open", "/probe.v1.Probe", "/probe.v1.Probe/Open/extra", ""} {
		if _, err := interceptor.New(v, interceptor.Exempt(bad)); err == nil {
			t.Errorf("Exempt accepted %q, which is not a procedure", bad)
		}
	}
	a, err := interceptor.New(v)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recover() == nil {
			t.Error("Authz accepted a nil policy")
		}
	}()
	_ = a.Authz(nil)
}

// TestPrincipalFromRoundTrip pins the context contract the two interceptors
// meet through, including that a stored nil is not an identity.
func TestPrincipalFromRoundTrip(t *testing.T) {
	if p, ok := authn.PrincipalFrom(context.Background()); ok || p != nil {
		t.Errorf("an empty context answered a principal: %v, %v", p, ok)
	}
	want := &authn.Principal{Subject: "someone"}
	ctx := authn.WithPrincipal(context.Background(), want)
	if p, ok := authn.PrincipalFrom(ctx); !ok || p != want {
		t.Errorf("PrincipalFrom = %v, %v; want the stored principal", p, ok)
	}
	if p, ok := authn.PrincipalFrom(authn.WithPrincipal(context.Background(), nil)); ok || p != nil {
		t.Errorf("a stored nil answered as an identity: %v, %v", p, ok)
	}
}

// TestAKeySetOutageIsNotABadCredential: when the issuer's JWKS cannot be
// fetched, the caller holds a token nobody could check rather than a bad
// one. The remedy is to retry, not to go and get a new token, so the code
// is CodeUnavailable — the same "a caller that cannot tell them apart
// retries the wrong thing" argument that makes authn and authz two
// interceptors. The control is a second Auth whose key set is up: the same
// token, the same procedure, a 200 — so the Unavailable above is the outage
// and not the fixture.
func TestAKeySetOutageIsNotABadCredential(t *testing.T) {
	signer := newSigner(t)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "the key set is not being served", http.StatusServiceUnavailable)
	}))
	t.Cleanup(down.Close)
	v, err := authn.New(authn.Development, issuer, down.URL, audience, authn.WithRefetchInterval(0))
	if err != nil {
		t.Fatal(err)
	}
	a, err := interceptor.New(v)
	if err != nil {
		t.Fatal(err)
	}
	bearer := "Bearer " + token(t, signer, nil)

	_, err = callUnary(t, probeServer(t, connect.WithInterceptors(a.Authn())), unaryProc, bearer)
	cerr := wantCode(t, err, connect.CodeUnavailable)
	// By message rather than errors.Is: the error the caller holds was
	// rebuilt from the wire, so the chain the handler wrapped is gone.
	if !strings.Contains(cerr.Message(), authn.ErrKeySetUnavailable.Error()) {
		t.Errorf("the rejection does not name the outage:\n%v", cerr)
	}

	// The control, with the same signer behind a key set that answers.
	up, err := authn.New(authn.Development, issuer, keyServer(t, signer).URL, audience, authn.WithRefetchInterval(0))
	if err != nil {
		t.Fatal(err)
	}
	b, err := interceptor.New(up)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := callUnary(t, probeServer(t, connect.WithInterceptors(b.Authn())), unaryProc, bearer); err != nil || got != "sub=someone" {
		t.Fatalf("the control call = %q, %v; want sub=someone — the token itself is good", got, err)
	}
}

// TestOneAuthPerHandler pins the documented constraint, because it is a
// divergence from what stacking middleware usually means: the short-circuit
// that makes Authn and Authz order-irrelevant reads the context alone, not
// which Auth filed the Principal, so a second Auth installed on the same
// handler is not a second gate. The control is the second Auth alone, which
// does reject the token — so the acceptance above is the short-circuit and
// not two validators that happen to agree.
func TestOneAuthPerHandler(t *testing.T) {
	a, signer := newAuth(t)
	bearer := "Bearer " + token(t, signer, nil)

	// A second Auth that trusts a different issuer entirely: nothing this
	// signer mints could ever pass it.
	other, err := authn.New(authn.Development, issuer+"/other", keyServer(t, newSigner(t)).URL, audience,
		authn.WithRefetchInterval(0))
	if err != nil {
		t.Fatal(err)
	}
	b, err := interceptor.New(other)
	if err != nil {
		t.Fatal(err)
	}

	stacked := probeServer(t, connect.WithInterceptors(a.Authn(), b.Authn()))
	if got, err := callUnary(t, stacked, unaryProc, bearer); err != nil || got != "sub=someone" {
		t.Fatalf("stacked = %q, %v; want sub=someone — the first Auth files the Principal and the second finds it there", got, err)
	}

	alone := probeServer(t, connect.WithInterceptors(b.Authn()))
	_, err = callUnary(t, alone, unaryProc, bearer)
	wantCode(t, err, connect.CodeUnauthenticated) // the control: b really would have refused
}
