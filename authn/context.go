package authn

import "context"

// principalKey is the context key the interceptors file the [Principal]
// under. Unexported, so the only ways in and out are the two functions
// below.
type principalKey struct{}

// WithPrincipal returns a context carrying p. The authn interceptor
// (authn/interceptor) is the normal writer; it is exported so a test — or a
// transport this module ships no interceptor for — can stage an
// authenticated context the same way.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the [Principal] a validated token put in ctx, and
// whether there is one. False means the request carried no identity: either
// no interceptor ran, or the procedure is exempt from authentication and the
// caller presented no token — the two are indistinguishable here on purpose,
// because a handler must not behave differently for them.
func PrincipalFrom(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(*Principal)
	return p, ok && p != nil
}
