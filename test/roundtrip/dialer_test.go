package roundtriptest

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/emptypb"
)

// defaultProxyTarget is where every *.internal name goes. Fixed by ADR 001,
// and by the fleet convention that every service declares 9777 in its
// fly.toml so the dial string is the same in both environments.
const defaultProxyTarget = "127.0.0.1:9777"

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// internalDialer is ADR 002's client dialer: it sends any *.internal host to
// the proxy and leaves everything else alone. The URL is never involved, so
// Host / :authority still names the service and the proxy can route on it —
// only the TCP destination changes.
//
// FLY_APP_NAME is read once, here, rather than per dial. On Fly the answer
// cannot change while the process runs, and returning the base dialer
// unwrapped is the most literal form of "the same code, doing ordinary
// resolution".
func internalDialer(proxyTarget string, base dialFunc) dialFunc {
	if os.Getenv("FLY_APP_NAME") != "" {
		return base
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			// No port to split. Not a shape net/http produces; pass it through
			// rather than guess.
			return base(ctx, network, addr)
		}
		if !isInternal(host) {
			return base(ctx, network, addr)
		}
		// The source port is deliberately discarded. voya listens on one port
		// and routes on the hostname alone, so payments.internal:8080 is the
		// same destination as payments.internal:9777 — and the authority the
		// proxy sees still carries whichever port the caller wrote.
		return base(ctx, network, proxyTarget)
	}
}

// isInternal reports whether host is a name under the .internal TLD.
//
// Case-insensitive, and tolerant of the fully-qualified trailing dot, because
// both forms reach a dialer in practice and neither is a different service.
// A bare "internal" is not matched: it is a TLD, not a service.
//
// The folding itself is normalizeHost, shared with the proxy's routing key so
// the two cannot come to disagree about what one name is.
func isInternal(host string) bool {
	return strings.HasSuffix(normalizeHost(host), ".internal")
}

// recordingDialer captures the address a dial was actually attempted against
// without opening a connection.
type recordingDialer struct{ addrs []string }

func (d *recordingDialer) dial(_ context.Context, _, addr string) (net.Conn, error) {
	d.addrs = append(d.addrs, addr)
	return nil, errNotDialed
}

var errNotDialed = errors.New("recording dialer: no connection made")

// TestDialerRewritesOnlyTheDestination pins which names are diverted to voya
// and which are left to ordinary resolution. Divert too much and a service
// loses access to the internet; divert too little and the whole scheme is
// silently bypassed.
func TestDialerRewritesOnlyTheDestination(t *testing.T) {
	for _, tc := range []struct {
		name string
		addr string
		want string
	}{
		{"fleet convention", "payments.internal:9777", defaultProxyTarget},
		{"other port in the dial string", "payments.internal:8080", defaultProxyTarget},
		{"uppercase", "PAYMENTS.INTERNAL:9777", defaultProxyTarget},
		{"mixed case", "Payments.Internal:9777", defaultProxyTarget},
		{"fully qualified trailing dot", "payments.internal.:9777", defaultProxyTarget},
		{"deeper label", "eu.payments.internal:9777", defaultProxyTarget},

		// Pinned, not endorsed. ADR 002 says "any *.internal host", and this
		// is what that costs: a Fly 6PN address that is not one of our HTTP
		// services — a Postgres cluster, Redis — diverts too, and lands on
		// voya's h2c port, where it fails as an HTTP error rather than a
		// Postgres one. On Fly the same dial reaches the database, so this is
		// the dev/prod divergence class voya exists to remove. Narrowing the
		// dialer means deciding what it narrows on (a port allowlist, a
		// registry lookup, an explicit opt-out list) — a design call for #16,
		// not something a spike should settle by picking one.
		{"non-HTTP fly service diverts too", "myapp-db.internal:5432", defaultProxyTarget},

		{"public name untouched", "api.stripe.com:443", "api.stripe.com:443"},
		{"loopback untouched", "127.0.0.1:5432", "127.0.0.1:5432"},
		{"bare TLD is not a service", "internal:9777", "internal:9777"},
		{"lookalike suffix untouched", "notinternal:9777", "notinternal:9777"},
		{"internal as a label, not the TLD", "internal.example.com:443", "internal.example.com:443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingDialer{}
			dial := internalDialer(defaultProxyTarget, rec.dial)
			_, _ = dial(context.Background(), "tcp", tc.addr)

			if len(rec.addrs) != 1 {
				t.Fatalf("base dialer called %d times, want 1", len(rec.addrs))
			}
			if rec.addrs[0] != tc.want {
				t.Errorf("dial %q went to %q, want %q", tc.addr, rec.addrs[0], tc.want)
			}
		})
	}
}

// TestDialerIsNoOpUnderFlyAppName covers ADR 002's requirement that the same
// binary does ordinary 6PN resolution in production. If this ever regresses,
// every deployed service starts dialing its own loopback.
func TestDialerIsNoOpUnderFlyAppName(t *testing.T) {
	t.Setenv("FLY_APP_NAME", "payments")

	rec := &recordingDialer{}
	dial := internalDialer(defaultProxyTarget, rec.dial)
	_, _ = dial(context.Background(), "tcp", "payments.internal:9777")

	if len(rec.addrs) != 1 {
		t.Fatalf("base dialer called %d times, want 1", len(rec.addrs))
	}
	if got := rec.addrs[0]; got != "payments.internal:9777" {
		t.Errorf("under FLY_APP_NAME the dial went to %q, want the address untouched", got)
	}
}

// TestNoOpDialerNeverReachesTheProxy is the same requirement from the other
// side: with FLY_APP_NAME set, a real client must not land on voya even
// though voya is running and listening on the very port it would use.
func TestNoOpDialerNeverReachesTheProxy(t *testing.T) {
	f := newFleet(t)
	t.Setenv("FLY_APP_NAME", "payments")

	// Built here rather than taken from the fleet: internalDialer reads
	// FLY_APP_NAME once at construction — which is the behaviour under test —
	// so the fleet's own client, built before the environment was set, still
	// carries the diverting dialer.
	c := authorityClient(voyaClient(t, f.proxyAddr), "http://payments.internal:9777")
	_, err := c.CallUnary(testContext(t), connect.NewRequest(&emptypb.Empty{}))

	// This is the load-bearing assertion, and it holds whatever ordinary
	// resolution does: the fleet's proxy is reachable only on its ephemeral
	// port, so one request there means the dialer diverted.
	if n := f.inbound.count(); n != 0 {
		t.Errorf("the proxy saw %d request(s); traffic reached voya despite FLY_APP_NAME", n)
	}
	if n := f.payments.count(); n != 0 {
		t.Errorf("backend saw %d request(s); traffic reached voya despite FLY_APP_NAME", n)
	}

	// The outcome of ordinary resolution is a property of the machine, not of
	// the dialer. Usually it fails — NXDOMAIN, or connection refused where a
	// wildcard resolver is installed. But a developer running the real voya
	// has 127.0.0.1:9777 answering and payments registered, and then it
	// succeeds while the dialer behaved correctly. Skipping there rather than
	// failing follows TestDefaultTargetRoundTrip, which makes the same
	// allowance for the same reason.
	if err == nil {
		t.Skipf("ordinary resolution of payments.internal:9777 succeeded — something is answering on %s, plausibly a real voya; the dialer stayed out of it either way", defaultProxyTarget)
	}
	t.Logf("ordinary resolution of payments.internal:9777 failed with: %v", err)
}
