// Package roundtriptest answers one question with evidence: does rewriting
// only the TCP destination preserve the authority all the way to a service
// that identifies itself by name?
//
// That is the seam ADR 002 rests on. A client dials
// http://payments.internal:9777; a DialContext sends the connection to
// 127.0.0.1:9777 without touching the URL; the proxy routes on the
// :authority header it finds there. Every link in that chain is somewhere the
// authority could quietly be rewritten — and if it is, requests still arrive,
// just at the wrong service or at no service at all.
//
// This is a spike, so the registry is a hardcoded table: no registration
// stream, no DNS, no config. The components here are prototypes of #16
// (client dialer) and #17 (proxy) and are expected to be superseded by them.
// Everything lives in _test.go files so that nothing here can be mistaken for,
// or linked into, the real thing.
//
// Run with `go test -v ./test/roundtrip/`.
package roundtriptest

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// procAuthority is a Connect procedure that reports back the :authority the
// service saw. Asserting on what the backend observed — rather than on what
// the client sent — is the only way to catch a rewrite in the middle.
const procAuthority = "/voya.spike.v1.Backend/Authority"

// observation is one request as the backend saw it.
type observation struct {
	authority string // r.Host: the :authority pseudo-header on HTTP/2
	proto     string // "HTTP/2.0" or "HTTP/1.1"
	path      string
}

// observer records what reached a backend. Concurrent by construction: the
// gRPC-Web leg and the Connect leg can share one.
type observer struct {
	mu   sync.Mutex
	seen []observation
}

func (o *observer) record(r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, observation{authority: r.Host, proto: r.Proto, path: r.URL.Path})
}

func (o *observer) all() []observation {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]observation(nil), o.seen...)
}

func (o *observer) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.seen)
}

func (o *observer) last(t *testing.T) observation {
	t.Helper()
	all := o.all()
	if len(all) == 0 {
		t.Fatal("backend saw no requests at all")
	}
	return all[len(all)-1]
}

// authorityKey carries r.Host into the Connect handler. connect.Request
// exposes headers, but Go lifts Host out of the header map onto the request,
// so a plain middleware is the way to see it.
type authorityKey struct{}

// observed wraps a handler so a test can assert on what reached it.
//
// The proxy's inbound leg needs one as much as a backend does. The proxy
// dials upstream over h2c whatever the client spoke, so every backend-side
// "HTTP/2.0" assertion holds even if the client transport quietly downgraded
// to HTTP/1.1 — the inbound observation is the only place that shows.
func observed(obs *observer, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		obs.record(r)
		h.ServeHTTP(w, r)
	})
}

// backend is a connect-go service. connect-go serves Connect, gRPC and
// gRPC-Web from the same handler, so one backend covers all three protocols.
func backend(obs *observer) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(procAuthority, connect.NewUnaryHandler(procAuthority,
		func(ctx context.Context, _ *connect.Request[emptypb.Empty]) (*connect.Response[wrapperspb.StringValue], error) {
			authority, _ := ctx.Value(authorityKey{}).(string)
			return connect.NewResponse(wrapperspb.String(authority)), nil
		}))

	return observed(obs, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authorityKey{}, r.Host)))
	}))
}

// h2cProtocols is cleartext HTTP/2 alongside HTTP/1.1 on one port. Go 1.24+
// serves this from the standard library, which supersedes
// golang.org/x/net/http2/h2c (issue #21).
func h2cProtocols() *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	return p
}

// serve starts h with h2c + HTTP/1.1 on an ephemeral loopback port — the shape
// every service in the fleet has under ADR 002, where the OS picks the port.
func serve(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.Config.Protocols = h2cProtocols()
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// clientTransport dials cleartext HTTP/2 with prior knowledge through dial.
//
// UnencryptedHTTP2 without HTTP1 is what makes this h2c rather than an
// upgrade dance, and it is why the dialer can sit on the ordinary
// DialContext: no TLS-shaped hook is involved. gRPC needs HTTP/2 end to end,
// so the client leg has to be h2c for all three protocols to share a client.
func clientTransport(dial dialFunc) *http.Transport {
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	return &http.Transport{DialContext: dial, Protocols: p}
}

// voyaClient is what a service gets from the client library: an *http.Client
// whose dialer sends *.internal to voya, and nothing else.
//
// Each one owns an *http.Transport holding live h2c connections and their
// read and write goroutines, and nothing else in the suite closes them — so
// construction registers the cleanup rather than leaving it to whoever
// remembers.
func voyaClient(t *testing.T, proxyAddr string) *http.Client {
	t.Helper()
	hc := &http.Client{
		Transport: clientTransport(internalDialer(proxyAddr, (&net.Dialer{}).DialContext)),
		Timeout:   10 * time.Second,
	}
	t.Cleanup(hc.CloseIdleConnections)
	return hc
}

// authorityClient builds a Connect client for procAuthority against baseURL.
// opts selects the protocol: none for Connect, WithGRPC, or WithGRPCWeb.
func authorityClient(hc *http.Client, baseURL string, opts ...connect.ClientOption) *connect.Client[emptypb.Empty, wrapperspb.StringValue] {
	return connect.NewClient[emptypb.Empty, wrapperspb.StringValue](hc, baseURL+procAuthority, opts...)
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// fleet stands up two backends behind a proxy and returns the proxy's address
// plus the observers. reports.internal is deliberately declared with no
// instance: "known but not running" has to be distinguishable from a typo.
//
// client is one *http.Client for the whole fleet, which is the shape a real
// service has: the client library is constructed once and every name it dials
// shares its transport and connection pool. Per-call clients would hide
// whether Go keys that pool on the URL authority or on the dial destination —
// and since every name here dials the same destination, the second would
// multiplex billing.internal onto a connection opened for payments.internal.
type fleet struct {
	proxyAddr string
	client    *http.Client
	inbound   *observer // what reached the proxy
	payments  *observer
	billing   *observer
}

func newFleet(t *testing.T) *fleet { return newFleetWithDiagnostics(t, plainText) }

func newFleetWithDiagnostics(t *testing.T, style diagnosticStyle) *fleet {
	t.Helper()
	f := &fleet{inbound: &observer{}, payments: &observer{}, billing: &observer{}}
	reg := &registry{
		upstream: map[string]string{
			"payments.internal": serve(t, backend(f.payments)),
			"billing.internal":  serve(t, backend(f.billing)),
		},
		declared: []string{"payments.internal", "billing.internal", "reports.internal"},
	}
	f.proxyAddr = serve(t, observed(f.inbound, newProxyWithDiagnostics(reg, style)))
	f.client = voyaClient(t, f.proxyAddr)
	return f
}

// call makes one request through the fleet's proxy and returns the authority
// the backend reported seeing. Safe to call concurrently.
func (f *fleet) call(t *testing.T, dialTarget string, opts ...connect.ClientOption) (string, error) {
	t.Helper()
	c := authorityClient(f.client, "http://"+dialTarget, opts...)
	res, err := c.CallUnary(testContext(t), connect.NewRequest(&emptypb.Empty{}))
	if err != nil {
		return "", err
	}
	return res.Msg.GetValue(), nil
}

// TestAuthorityArrivesUnmodified is the headline case, and the whole reason
// this spike exists.
//
// A client dials payments.internal:9777. The dialer sends the TCP connection
// to the proxy on loopback. The URL is never touched, so the request must
// arrive at the backend still saying payments.internal:9777 — not the proxy's
// address, and not the backend's ephemeral port.
func TestAuthorityArrivesUnmodified(t *testing.T) {
	f := newFleet(t)

	got, err := f.call(t, "payments.internal:9777")
	if err != nil {
		t.Fatalf("call through the proxy: %v", err)
	}
	if want := "payments.internal:9777"; got != want {
		t.Errorf("backend saw authority %q, want %q — something in the chain rewrote it", got, want)
	}

	// Both legs, and the inbound one is not redundant: the proxy dials
	// upstream over h2c whatever the client spoke, so the backend reports
	// HTTP/2.0 even when the client transport has silently downgraded. Only
	// this assertion pins the design note #16 has to carry — the client
	// transport is UnencryptedHTTP2 *without* HTTP1, prior-knowledge h2c
	// rather than an upgrade dance.
	if in := f.inbound.last(t); in.proto != "HTTP/2.0" {
		t.Errorf("client leg reached the proxy as %s, want HTTP/2.0 — the client transport is not prior-knowledge h2c", in.proto)
	}

	obs := f.payments.last(t)
	if obs.proto != "HTTP/2.0" {
		t.Errorf("backend leg was %s, want HTTP/2.0 — h2c did not happen", obs.proto)
	}
	if obs.path != procAuthority {
		t.Errorf("backend saw path %q, want %q", obs.path, procAuthority)
	}
	if n := f.billing.count(); n != 0 {
		t.Errorf("billing received %d request(s); routing sent traffic to the wrong service", n)
	}
}

// TestThreeProtocolsFromOneClient covers ADR 003's protocol set over the same
// *http.Client, which is what a service actually holds. All three must reach
// the backend with the authority intact; gRPC in particular carries its
// outcome in trailers, so a broken hop shows up as Internal rather than as a
// wrong answer.
func TestThreeProtocolsFromOneClient(t *testing.T) {
	f := newFleet(t)
	hc := f.client

	for _, tc := range []struct {
		name string
		opts []connect.ClientOption
	}{
		{"connect", nil},
		{"grpc", []connect.ClientOption{connect.WithGRPC()}},
		{"grpc-web", []connect.ClientOption{connect.WithGRPCWeb()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := authorityClient(hc, "http://payments.internal:9777", tc.opts...)
			res, err := c.CallUnary(testContext(t), connect.NewRequest(&emptypb.Empty{}))
			if err != nil {
				t.Fatalf("%s call: %v (code %v)", tc.name, err, connect.CodeOf(err))
			}
			if want := "payments.internal:9777"; res.Msg.GetValue() != want {
				t.Errorf("%s: backend saw authority %q, want %q", tc.name, res.Msg.GetValue(), want)
			}
		})
	}

	// Both legs for all three. The inbound check is what would catch a
	// client transport that fell back to HTTP/1.1: gRPC needs HTTP/2 end to
	// end, and the backend leg cannot report on the client's half.
	for _, o := range f.inbound.all() {
		if o.proto != "HTTP/2.0" {
			t.Errorf("a client leg reached the proxy as %s, want HTTP/2.0 for all three protocols", o.proto)
		}
	}
	for _, o := range f.payments.all() {
		if o.proto != "HTTP/2.0" {
			t.Errorf("a leg arrived as %s, want HTTP/2.0 for all three protocols", o.proto)
		}
	}
}

// TestNamesRouteToTheirOwnService guards the case a single front door cannot
// serve: two services, one procedure path. Only the authority tells them
// apart, which is also what makes gRPC reflection work per-service (ADR 003).
//
// Both calls go through the fleet's one client, so they also share a
// transport and its connection pool. That is the arrangement a real service
// has, and the one where the property matters: every *.internal name dials
// the same TCP destination, so a pool keyed on the destination rather than on
// the URL authority would send billing's request down the connection opened
// for payments.
func TestNamesRouteToTheirOwnService(t *testing.T) {
	f := newFleet(t)

	if _, err := f.call(t, "payments.internal:9777"); err != nil {
		t.Fatalf("payments: %v", err)
	}
	if _, err := f.call(t, "billing.internal:9777"); err != nil {
		t.Fatalf("billing: %v", err)
	}

	if n := f.payments.count(); n != 1 {
		t.Errorf("payments saw %d requests, want 1", n)
	}
	if n := f.billing.count(); n != 1 {
		t.Errorf("billing saw %d requests, want 1", n)
	}
	if got := f.billing.last(t).authority; got != "billing.internal:9777" {
		t.Errorf("billing saw authority %q", got)
	}
}

// TestConcurrentRequestsDoNotCrossRoutes puts two requests in flight at once,
// which is the only condition under which the proxy's routing state means
// anything.
//
// The upstream is resolved in the handler and handed to the Rewrite hook
// through the request context — correct precisely because nothing is stored
// on the proxy. Hoisting it to a field is the obvious simplification, it
// looks identical under every sequential test in this package, and it
// corrupts routing the moment two requests overlap. Without this test the
// suite runs strictly one request at a time, so -race has nothing to observe
// and the observer's own "concurrent by construction" comment describes
// concurrency that never happens.
func TestConcurrentRequestsDoNotCrossRoutes(t *testing.T) {
	f := newFleet(t)

	const perName = 30
	names := []string{"payments.internal:9777", "billing.internal:9777"}

	var wg sync.WaitGroup
	failures := make(chan string, len(names)*perName)
	for _, authority := range names {
		for range perName {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got, err := f.call(t, authority)
				switch {
				case err != nil:
					failures <- fmt.Sprintf("call to %s: %v", authority, err)
				case got != authority:
					failures <- fmt.Sprintf("caller dialed %s but the backend saw %q", authority, got)
				}
			}()
		}
	}
	wg.Wait()
	close(failures)
	for msg := range failures {
		t.Error(msg)
	}

	// Counts alone would miss a swap in both directions, so check where each
	// request landed: every observation at a backend must name that backend.
	for _, svc := range []struct {
		name string
		obs  *observer
	}{
		{"payments.internal", f.payments},
		{"billing.internal", f.billing},
	} {
		if n := svc.obs.count(); n != perName {
			t.Errorf("%s saw %d requests, want %d", svc.name, n, perName)
		}
		for _, o := range svc.obs.all() {
			if hostname(o.authority) != svc.name {
				t.Errorf("%s received a request for %q — routing crossed under concurrency", svc.name, o.authority)
			}
		}
	}
}

// TestDefaultTargetRoundTrip is the one case that binds the literal
// 127.0.0.1:9777 from ADR 001, rather than an ephemeral test port. Everything
// else here proves the mechanism; this proves the constant.
//
// Skipped rather than failed when the port is busy: on a developer's machine
// something else — plausibly voya itself — may already hold it.
func TestDefaultTargetRoundTrip(t *testing.T) {
	ln, err := net.Listen("tcp", defaultProxyTarget)
	if err != nil {
		t.Skipf("cannot bind %s (%v) — the mechanism is covered by the ephemeral-port tests", defaultProxyTarget, err)
	}

	obs := &observer{}
	reg := &registry{
		upstream: map[string]string{"payments.internal": serve(t, backend(obs))},
		declared: []string{"payments.internal"},
	}
	srv := &http.Server{
		Handler:           newProxy(reg),
		Protocols:         h2cProtocols(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	// No proxy address injected anywhere: the dialer's own default is what
	// has to land on this listener. Built through voyaClient rather than by
	// hand, so the one test that binds ADR 001's literal port is exercising
	// the same client the rest of the suite does — #16 will add options to
	// it, and this is where a divergence would go unnoticed longest.
	c := authorityClient(voyaClient(t, defaultProxyTarget), "http://payments.internal:9777")
	res, err := c.CallUnary(testContext(t), connect.NewRequest(&emptypb.Empty{}))
	if err != nil {
		t.Fatalf("round trip via %s: %v", defaultProxyTarget, err)
	}
	if want := "payments.internal:9777"; res.Msg.GetValue() != want {
		t.Errorf("backend saw authority %q, want %q", res.Msg.GetValue(), want)
	}
	if n := obs.count(); n != 1 {
		t.Errorf("backend saw %d requests, want 1", n)
	}
}

// TestNegativeControl_SetURLDestroysTheAuthority is the trap this spike
// exists to find, demonstrated live.
//
// httputil.ProxyRequest.SetURL sets Out.Host = "", so the outbound authority
// becomes the upstream's ephemeral address. Everything still works — requests
// arrive, responses come back — and the one property the whole design rests
// on is silently gone. The proxy under test restores Out.Host; this control
// shows what the omission costs, and keeps the assertions above honest.
func TestNegativeControl_SetURLDestroysTheAuthority(t *testing.T) {
	obs := &observer{}
	upstreamAddr := serve(t, backend(obs))
	reg := &registry{
		upstream: map[string]string{"payments.internal": upstreamAddr},
		declared: []string{"payments.internal"},
	}
	naiveAddr := serve(t, newNaiveProxy(reg))

	c := authorityClient(voyaClient(t, naiveAddr), "http://payments.internal:9777")
	res, err := c.CallUnary(testContext(t), connect.NewRequest(&emptypb.Empty{}))
	if err != nil {
		t.Fatalf("naive proxy call failed outright: %v — expected it to succeed with a rewritten authority", err)
	}

	got := res.Msg.GetValue()
	if got == "payments.internal:9777" {
		t.Fatal("SetURL preserved the authority — the hazard is gone and the real proxy's Out.Host restore may be dead code; re-check net/http/httputil")
	}
	// Compared against the upstream's actual address rather than a
	// "127.0.0.1:" prefix: httptest falls back to the IPv6 loopback where
	// IPv4 is unavailable, and on such a host the hazard would be
	// demonstrated exactly as intended while this assertion failed.
	if got != upstreamAddr {
		t.Errorf("naive proxy produced authority %q, want the upstream address %q", got, upstreamAddr)
	}
	t.Logf("with SetURL alone the backend saw authority %q instead of payments.internal:9777", got)
}
