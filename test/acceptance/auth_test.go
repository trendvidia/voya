package acceptance

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/trendvidia/voya/authn"
	"github.com/trendvidia/voya/authn/interceptor"
	voya "github.com/trendvidia/voya/client"
	"github.com/trendvidia/voya/internal/auth"
	"github.com/trendvidia/voya/internal/config"
	configv1 "github.com/trendvidia/voya/proto/voya/config/v1"
)

// --- #212: the interceptors, end to end through the proxy ---------------------

// writeScope is what [writePolicy] requires, and what tells the two personas
// apart: `has-write-scope` carries it, `no-scopes` carries nothing.
const writeScope = "write"

// writePolicy is the authorization a service supplies — "may this principal
// perform this operation" — in its smallest honest form: one scope, required
// on every procedure the policy sees.
func writePolicy(_ context.Context, spec connect.Spec, p *authn.Principal) error {
	if p == nil {
		return errors.New("no identity")
	}
	if !slices.Contains(p.Scopes, writeScope) {
		return fmt.Errorf("%s requires the %q scope; this principal has %v", spec.Procedure, writeScope, p.Scopes)
	}
	return nil
}

// TestATokenWithoutTheScopeIsDeniedThroughTheProxy is ADR 006's claim reduced
// to one test: a service validates voya's tokens with the shared library and
// its interceptors, a caller reaches it through voya's proxy on the fleet's
// own dial string, and the difference between a 200 and a 403 is which
// persona the token was minted for.
//
// That is what "reproducible locally" means in ADR 006 — testing a 403 is a
// one-flag change, and it exercises the real machinery rather than bypassing
// it. Every hop here is the real one: the token is minted the way `voya token`
// mints it, the JWKS is fetched over the client dialer from the reserved
// authority, and the call crosses the proxy.
//
// The two codes are asserted apart on purpose. A caller that cannot tell
// unauthenticated from permission_denied retries the wrong one, which is why
// #212 made them two interceptors rather than one.
func TestATokenWithoutTheScopeIsDeniedThroughTheProxy(t *testing.T) {
	v, cfg := startOnFreePorts(t)

	key, err := auth.LoadOrGenerate(config.Dir())
	if err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}
	// The validator a fleet service constructs at startup: Development,
	// because voya is the issuer, and the JWKS reached the way every service
	// reaches it — through the dialer, at the reserved authority.
	jwksURL := fmt.Sprintf("http://voya.internal:%d%s", v.proxyPort, auth.JWKSPath)
	validator, err := authn.New(authn.Development, cfg.GetAuth().GetIssuer(), jwksURL, cfg.GetAuth().GetAudience(),
		authn.WithHTTPClient(&http.Client{
			Transport: voya.Transport(voya.WithProxyPort(v.proxyPort)),
			Timeout:   callerTimeout,
		}))
	if err != nil {
		t.Fatalf("authn.New: %v", err)
	}
	guard, err := interceptor.New(validator)
	if err != nil {
		t.Fatalf("interceptor.New: %v", err)
	}

	startGuardedService(t, v, "guarded", guard, writePolicy)

	for _, tc := range []struct {
		persona  string
		wantErr  bool
		wantCode connect.Code
	}{
		{persona: "has-write-scope"},
		{persona: "no-scopes", wantErr: true, wantCode: connect.CodePermissionDenied},
		// Not a scope problem at all: no credential, so the remedy is a
		// token rather than a permission, and the code has to say so.
		{persona: "", wantErr: true, wantCode: connect.CodeUnauthenticated},
	} {
		t.Run(caseName(tc.persona), func(t *testing.T) {
			var bearer string
			if tc.persona != "" {
				bearer = "Bearer " + mintAs(t, key, cfg, tc.persona)
			}
			got, err := callGuarded(t, v.proxyPort, bearer)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("the %s call failed: %v", tc.persona, err)
				}
				// The handler answers with the subject it saw, so the 200
				// row proves which identity crossed the proxy rather than
				// merely that something got through.
				if want := personaNamed(t, cfg, tc.persona).GetSubject(); got != want {
					t.Fatalf("the handler saw subject %q, want %q", got, want)
				}
				return
			}
			if got := connect.CodeOf(err); got != tc.wantCode {
				t.Fatalf("code = %v (err %v), want %v", got, err, tc.wantCode)
			}
		})
	}
}

// caseName labels the anonymous row, since a subtest called "" is unreadable
// in a failure.
func caseName(persona string) string {
	if persona == "" {
		return "no token at all"
	}
	return persona
}

// mintAs mints the named persona's token exactly as `voya token --as <name>`
// does — the same key, the same config, no defect.
func mintAs(t *testing.T, key *rsa.PrivateKey, cfg *configv1.Config, persona string) string {
	t.Helper()
	tok, err := auth.Mint(key, cfg.GetAuth(), personaNamed(t, cfg, persona), time.Now(), auth.Valid)
	if err != nil {
		t.Fatalf("Mint(%s): %v", persona, err)
	}
	return tok
}

// personaNamed is the committed default persona by that name — the one the
// token is minted from and the one whose subject the 200 row is checked
// against, so the two cannot drift apart.
func personaNamed(t *testing.T, cfg *configv1.Config, persona string) *configv1.Persona {
	t.Helper()
	for _, p := range cfg.GetAuth().GetPersonas() {
		if p.GetName() == persona {
			return p
		}
	}
	t.Fatalf("no persona named %q in the committed defaults", persona)
	return nil
}

// guardedProcedure is the one procedure the guarded fixture serves. A name of
// its own rather than the echo fixture's, so nothing in this file depends on
// the rest of the suite's handler staying unauthenticated.
const guardedProcedure = "/voya.acceptance.v1.Echo/Whoami"

// startGuardedService registers a service that runs both interceptors, so the
// call this test makes crosses the same two seams a fleet service puts in
// front of every procedure.
func startGuardedService(t *testing.T, v *voyad, name string, guard *interceptor.Auth, policy interceptor.Policy) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle(guardedProcedure, connect.NewUnaryHandler(guardedProcedure,
		func(ctx context.Context, _ *connect.Request[wrapperspb.StringValue]) (*connect.Response[wrapperspb.StringValue], error) {
			p, ok := authn.PrincipalFrom(ctx)
			if !ok {
				return nil, connect.NewError(connect.CodeInternal,
					errors.New("the handler ran with no principal in its context: authn let an unauthenticated call through"))
			}
			return connect.NewResponse(wrapperspb.String(p.Subject)), nil
		},
		connect.WithInterceptors(guard.Authn(), guard.Authz(policy))))

	srv := echoServer(mux)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	stop, err := voya.Join(context.Background(), name, ln.Addr(),
		voya.WithRegistrationPort(v.registrationPort),
		voya.WithProxyPort(v.proxyPort),
		voya.WithoutDialer(),
		voya.WithBackoff(10*time.Millisecond, 200*time.Millisecond),
		voya.WithLogf(func(format string, args ...any) { t.Logf(name+": "+format, args...) }),
		voya.WithPID(fakePID(ln)),
	)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	t.Cleanup(func() {
		stop()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("%s: draining: %v", name, err)
			_ = srv.Close()
		}
		if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("%s: Serve: %v", name, err)
		}
	})
	waitFor(t, name+" to register", func() bool {
		_, live := v.Registry().Lookup(name)
		return live
	})
}

// callGuarded calls the guarded service by name through the proxy, with the
// Authorization header the caller was given — "" meaning none at all.
func callGuarded(t *testing.T, proxyPort uint32, bearer string) (string, error) {
	t.Helper()
	transport := &http.Transport{
		Protocols:   httpProtocols(),
		DialContext: voya.Dialer(voya.WithProxyPort(proxyPort)),
	}
	t.Cleanup(transport.CloseIdleConnections)
	c := connect.NewClient[wrapperspb.StringValue, wrapperspb.StringValue](
		&http.Client{Transport: transport, Timeout: callerTimeout},
		fmt.Sprintf("http://guarded.internal:%d%s", proxyPort, guardedProcedure))

	req := connect.NewRequest(wrapperspb.String("ping"))
	if bearer != "" {
		req.Header().Set("Authorization", bearer)
	}
	res, err := c.CallUnary(context.Background(), req)
	if err != nil {
		return "", err
	}
	return res.Msg.GetValue(), nil
}
