// Package interceptor runs the authn library from a Connect handler — ADR
// 006's "a Connect interceptor runs it". Whoever installs these runs the
// same code in development and production; the only thing that differs is
// the [authn.Validator] they are built around.
//
// There are two interceptors, not one, because the second needs something
// the first cannot know and because the distinction is exactly what the
// status codes have to tell apart: [Auth.Authn] turns the request's Bearer
// token into an [authn.Principal] in the context and rejects with
// CodeUnauthenticated — or, when the issuer's key set could not be fetched,
// CodeUnavailable — knowing nothing about the operation; [Auth.Authz]
// reads that Principal and asks the service's [Policy] whether it may
// perform this operation, rejecting with CodePermissionDenied — never
// CodeUnauthenticated — when a valid identity is simply not allowed. A
// caller that cannot tell the two apart retries the wrong thing.
//
// The decisions a reader will look for, in one place:
//
//   - The default is closed. Every procedure requires a token unless it is
//     named in [Exempt] — explicitly, one procedure at a time. A design
//     where forgetting an annotation leaves a procedure open is the same
//     failure class as the trusted-header bypass ADR 006 rejected.
//   - A presented token is always validated, exempt procedure or not, and an
//     invalid one is rejected. "No token required" is not "identity
//     forbidden" — a valid token on an exempt procedure still puts the
//     Principal in the context — and it is not "garbage accepted" either:
//     silently ignoring an expired token would turn "my token expired" into
//     a success with no identity, which is how auth bugs hide.
//   - The two install in either order. Whichever runs first authenticates
//     and files the Principal in the context; the second finds it there and
//     does not validate again. That short-circuit reads the context alone,
//     so it is one Auth per handler: stacking a second Auth — two
//     validators, two issuers — does not give a procedure two gates, it
//     gives it whichever gate ran first — pinned by TestOneAuthPerHandler.
//   - A key set that could not be fetched is CodeUnavailable, not
//     CodeUnauthenticated. The issuer being down is not a bad credential:
//     the remedy is to retry, and telling a caller to get a new token for
//     an outage is the same wrong-retry bug that makes authn and authz two
//     interceptors rather than one.
//   - A stream is authenticated when it is accepted, for its whole life — a
//     token that expires mid-stream does not cut the stream, the same way a
//     TLS session outlives the handshake that keyed it. A service that needs
//     mid-stream revocation needs shorter streams, not a second validator.
//   - On a Connect *client* these interceptors do nothing: there is no
//     incoming credential to check. Attaching tokens to outgoing calls is
//     the caller's business.
package interceptor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	"github.com/trendvidia/voya/authn"
)

// Policy is the authorization decision a service supplies to [Auth.Authz]:
// may this principal perform this operation. It depends on domain state,
// which is why it was never an edge concern (ADR 006) and why this package
// cannot write it. p is nil exactly when the procedure is exempt from
// authentication and the caller presented no token; a policy that requires
// an identity returns an error for nil, and the interceptor turns that into
// CodeUnauthenticated — "get a token" — rather than CodePermissionDenied,
// which would tell an anonymous caller their identity lacks a permission
// they never presented.
//
// A non-nil error denies. The interceptor owns the status code — that the
// code is the diagnosis is the whole reason authn and authz are two
// interceptors — so the error's text travels and any code it carries does
// not.
type Policy func(ctx context.Context, spec connect.Spec, p *authn.Principal) error

// Auth is one configuration — a validator and the procedures exempt from it
// — that both interceptors are built from. One value rather than two
// constructors so the exemption list has one home, and so the pair shares
// the authentication step that makes installation order not matter.
type Auth struct {
	v      *authn.Validator
	exempt map[string]bool
}

// Option adjusts an [Auth] at construction.
type Option func(*Auth) error

// Exempt names procedures reachable without a token, in Connect's spelling:
// "/package.Service/Method". Health checks (grpc.health.v1, ADR 004's fleet
// obligation), reflection, and whatever a service exposes publicly go here.
// Exemption is per procedure and never per service, and a name that does not
// look like a procedure is refused at construction — a typo here would
// otherwise leave the procedure silently closed forever, which fails safe
// but unfindably.
func Exempt(procedures ...string) Option {
	return func(a *Auth) error {
		for _, p := range procedures {
			if !strings.HasPrefix(p, "/") || strings.Count(p, "/") != 2 {
				return fmt.Errorf("interceptor: %q is not a procedure — the spelling is \"/package.Service/Method\", connect.Spec's", p)
			}
			a.exempt[p] = true
		}
		return nil
	}
}

// New builds the configuration both interceptors run. The validator is
// required: an Auth with nothing to validate against could only fail every
// call or pass every call, and either is a bug better caught at startup.
func New(v *authn.Validator, opts ...Option) (*Auth, error) {
	if v == nil {
		return nil, fmt.Errorf("interceptor: New needs a validator — build one with authn.New")
	}
	a := &Auth{v: v, exempt: make(map[string]bool)}
	for _, opt := range opts {
		if err := opt(a); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// Authn is the authentication interceptor: credential to [authn.Principal]
// in the context, or CodeUnauthenticated. It knows nothing about the
// operation beyond whether it is exempt.
func (a *Auth) Authn() connect.Interceptor {
	return authnInterceptor{a}
}

// Authz is the authorization interceptor: [authn.Principal] and operation to
// policy, or CodePermissionDenied. It authenticates first when [Auth.Authn]
// has not already — that is what lets the two install in either order — so a
// rejection for a bad credential still leaves as CodeUnauthenticated,
// whichever interceptor met it.
//
// policy is required; nil panics here, at service startup, rather than
// deciding every call one way at runtime — either way it decided would be a
// bug, and this one is loud.
func (a *Auth) Authz(policy Policy) connect.Interceptor {
	if policy == nil {
		panic("interceptor: Authz needs a Policy — a nil one would have to allow everything or deny everything, and either is a bug")
	}
	return authzInterceptor{a, policy}
}

// authenticate is the one authentication step both interceptors run, which
// is what makes their installation order irrelevant: whichever runs first
// validates and files the Principal in the context, and the other finds it
// there. The returned error is always a *connect.Error, CodeUnauthenticated
// for every credential fault and CodeUnavailable for the one fault that is
// not the credential's — the issuer's key set out of reach; the returned
// Principal is nil exactly when the procedure is exempt and no token was
// presented.
func (a *Auth) authenticate(ctx context.Context, spec connect.Spec, header http.Header) (context.Context, *authn.Principal, error) {
	if p, ok := authn.PrincipalFrom(ctx); ok {
		return ctx, p, nil
	}
	token, present, err := bearerToken(header)
	if err != nil {
		return ctx, nil, err
	}
	if !present {
		if a.exempt[spec.Procedure] {
			return ctx, nil, nil
		}
		return ctx, nil, connect.NewError(connect.CodeUnauthenticated,
			fmt.Errorf("no Authorization header: %s requires a Bearer token", spec.Procedure))
	}
	p, verr := a.v.Validate(ctx, token)
	if verr != nil {
		if errors.Is(verr, authn.ErrKeySetUnavailable) {
			return ctx, nil, connect.NewError(connect.CodeUnavailable, verr)
		}
		return ctx, nil, connect.NewError(connect.CodeUnauthenticated, verr)
	}
	return authn.WithPrincipal(ctx, p), p, nil
}

// bearerToken reads the one Bearer credential out of header, telling apart
// the shapes a rejection has to name: no Authorization header at all
// (present == false — the only shape that can be exempt), a header that is
// present but empty, a credential of some other scheme, and a Bearer with
// nothing after it. The scheme is matched case-insensitively — "bearer" is
// as good as "Bearer" — because RFC 7235 says auth-scheme comparison is
// case-insensitive, and a validator stricter than the RFC rejects clients
// that are in the right.
func bearerToken(header http.Header) (token []byte, present bool, err error) {
	values := header.Values("Authorization")
	switch {
	case len(values) == 0:
		return nil, false, nil
	case len(values) > 1:
		return nil, true, connect.NewError(connect.CodeUnauthenticated,
			fmt.Errorf("the request carries %d Authorization headers, and this interceptor will not choose between credentials", len(values)))
	}
	raw := strings.TrimSpace(values[0])
	if raw == "" {
		return nil, true, connect.NewError(connect.CodeUnauthenticated,
			fmt.Errorf("the Authorization header is present but empty"))
	}
	scheme, rest, found := strings.Cut(raw, " ")
	if !strings.EqualFold(scheme, "Bearer") {
		if !found {
			// With no space at all the whole header value is the
			// credential, so naming it back would put a live token in the
			// caller's error and in every log that renders one. Name the
			// shape, not the value.
			return nil, true, connect.NewError(connect.CodeUnauthenticated,
				fmt.Errorf("the Authorization header carries no scheme, and this interceptor validates Bearer tokens"))
		}
		return nil, true, connect.NewError(connect.CodeUnauthenticated,
			fmt.Errorf("the Authorization scheme is %q, and this interceptor validates Bearer tokens", clip(scheme)))
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return nil, true, connect.NewError(connect.CodeUnauthenticated,
			fmt.Errorf("the Authorization header says Bearer with no token after it"))
	}
	return []byte(rest), true, nil
}

// clip bounds what a rejection quotes back. A scheme is a short word, so
// anything longer is not one — a credential separated by something other
// than a space, most likely — and quoting it whole would leak it into the
// error the same way naming an unschemed header would.
func clip(s string) string {
	const limit = 16
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}

// authnInterceptor is [Auth.Authn]'s value. Unary and streaming-handler are
// two different methods — a hole in one is invisible to tests of the other,
// which this repo has shipped once already (#173) — so both run the same
// [Auth.authenticate] and differ only in where the header comes from.
type authnInterceptor struct{ a *Auth }

func (i authnInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().IsClient {
			return next(ctx, req)
		}
		ctx, _, err := i.a.authenticate(ctx, req.Spec(), req.Header())
		if err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (i authnInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i authnInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx, _, err := i.a.authenticate(ctx, conn.Spec(), conn.RequestHeader())
		if err != nil {
			return err
		}
		return next(ctx, conn)
	}
}

// authzInterceptor is [Auth.Authz]'s value; the same four-method shape as
// [authnInterceptor], for the same #173 reason.
type authzInterceptor struct {
	a      *Auth
	policy Policy
}

// authorize is authenticate-then-policy, with the code decided by what there
// was to deny: an identity the policy refused is CodePermissionDenied, and
// an anonymous caller the policy refused — reachable only on an exempt
// procedure — is CodeUnauthenticated, because the remedy is a token, not a
// permission.
func (i authzInterceptor) authorize(ctx context.Context, spec connect.Spec, header http.Header) (context.Context, error) {
	ctx, p, err := i.a.authenticate(ctx, spec, header)
	if err != nil {
		return ctx, err
	}
	if perr := i.policy(ctx, spec, p); perr != nil {
		if p == nil {
			return ctx, connect.NewError(connect.CodeUnauthenticated,
				fmt.Errorf("%s is open without a token, but this operation's policy requires an identity: %w", spec.Procedure, perr))
		}
		return ctx, connect.NewError(connect.CodePermissionDenied, perr)
	}
	return ctx, nil
}

func (i authzInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().IsClient {
			return next(ctx, req)
		}
		ctx, err := i.authorize(ctx, req.Spec(), req.Header())
		if err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (i authzInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i authzInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx, err := i.authorize(ctx, conn.Spec(), conn.RequestHeader())
		if err != nil {
			return err
		}
		return next(ctx, conn)
	}
}
