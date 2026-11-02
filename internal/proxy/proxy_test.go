package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trendvidia/voya/internal/auth"
	"github.com/trendvidia/voya/internal/registry"
	"github.com/trendvidia/voya/proto/voya/diag/v1/diagv1connect"
)

// --- harness -----------------------------------------------------------------

// backend serves h2c on an ephemeral loopback port, the shape every service
// has under ADR 002, and reports the port it actually got.
func backend(t *testing.T, handler http.Handler) uint32 {
	t.Helper()
	port, _, _ := backendServer(t, handler)
	return port
}

// backendServer is [backend] with the two extra handles the connection-pooling
// tests need: the server, for the one test that has to time its own shutdown
// rather than leave it to the cleanup, and a channel that reports when voya's
// pooled connection to this backend goes away.
//
// The cleanup still runs: Close after a completed Shutdown is a no-op, and a
// test that fails before shutting down still gets its port back.
//
// A backend is a *service*, so it builds its own server rather than borrowing
// [NewHTTPServer], which configures the traffic voya's proxy has to carry —
// test/acceptance's echoServer draws the same line. Borrowing it silently
// vacated [TestAProxiedToServiceShutsDownPromptly] the moment #72 added an
// inbound IdleTimeout: the backends inherited it, reaped voya's pooled
// upstream connection *themselves*, and the one test guarding #52 then passed
// with [UpstreamTransport]'s IdleConnTimeout deleted. What a backend needs
// from that constructor is h2c, and [h2cProtocols] is that part on its own.
//
// The separation is asserted where it is load-bearing rather than left to
// this paragraph — [timeShutdownAfterOneProxiedCall] checks the server it
// measures has no IdleTimeout of its own, because a comment is exactly what
// failed to prevent this the first time.
func backendServer(t *testing.T, handler http.Handler) (uint32, *http.Server, <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv := &http.Server{
		Handler:           handler,
		Protocols:         h2cProtocols(),
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: ReadHeaderTimeout,
	}
	// The proxy is the only thing that dials a backend in this package, so a
	// close on this channel is voya letting go of its pooled connection and
	// nothing else.
	//
	// Serving is set up before the port is read, not after: [portOf] can
	// t.Fatalf, and a Goexit before the cleanup was registered leaves the
	// listener bound for the rest of the binary.
	closed := serveReportingCloses(t, "backend", ln, srv, cancel)
	return portOf(t, ln), srv, closed
}

// serveReportingCloses starts srv on ln and reports every connection that
// server closes on the returned channel, tearing both down when the test ends.
//
// One helper for [backendServer] and [proxyServer] rather than the same
// twenty lines on each side of the proxy: the three subtleties it carries do
// not differ by side, and a copy is the thing that stops being updated — the
// argument internal/daemon's FreePorts makes, where the same helper in two
// files carried the same defect in both (#46). A ConnState hook installed
// after Serve races the first connection in, whose earlier transitions it
// was not there to see. A hook that blocked would stall the server's
// connection bookkeeping, so the channel is buffered and the send is not. And
// Serve's error goes back over a channel rather than to t.Errorf from this
// goroutine, which outlives the test body and would panic instead of failing
// it.
//
// what names the server in that last report, since a bare "Serve: …" from a
// package that stands up both sides says nothing about which one died.
func serveReportingCloses(t *testing.T, what string, ln net.Listener, srv *http.Server, cancel context.CancelFunc) <-chan struct{} {
	t.Helper()
	closed := make(chan struct{}, 4)
	srv.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}

	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
		if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("%s Serve: %v", what, err)
		}
	})
	return closed
}

func portOf(t *testing.T, ln net.Listener) uint32 {
	t.Helper()
	return portOfAddr(t, ln.Addr().String())
}

// deadPort is a loopback port nothing is listening on: bound to get one the
// machine agrees is free, then closed. It is what a service that registered
// and then died leaves in the registry, and the only fixture that reaches
// the proxy's ErrorHandler without a backend to arrange.
func deadPort(t *testing.T) uint32 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	port := portOf(t, ln)
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return port
}

func portOfAddr(t testing.TB, addr string) uint32 {
	t.Helper()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		t.Fatalf("ParseUint: %v", err)
	}
	return uint32(n)
}

// serveProxy runs the real proxy through the real Listen and NewHTTPServer,
// so what the tests exercise is what ships.
func serveProxy(t *testing.T, reg *registry.Registry, opts ...Option) string {
	t.Helper()
	addr, _ := proxyServer(t, reg, opts...)
	return addr
}

// proxyServer is [serveProxy] with the extra handle the caller-side pooling
// test needs, mirroring what [backendServer] is to [backend]: a channel
// reporting when a caller's connection to the proxy goes away.
//
// Only inbound connections are on the channel: the proxy's own upstream dials
// are the transport's, and ConnState sees what came off this server's
// listener and nothing else.
func proxyServer(t *testing.T, reg *registry.Registry, opts ...Option) (string, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ln, err := Listen(ctx, "127.0.0.1:0")
	if err != nil {
		cancel()
		t.Fatalf("Listen: %v", err)
	}
	srv := NewHTTPServer(ctx, New(reg, opts...))
	return ln.Addr().String(), serveReportingCloses(t, "proxy", ln, srv, cancel)
}

// client dials the proxy whatever host the URL names, which is exactly what
// voya's client dialer does in a service (ADR 002) — and what makes the URL,
// and therefore the authority, the thing under test.
func client(t testing.TB, proxyAddr string, h2Only bool) *http.Client {
	t.Helper()
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	if !h2Only {
		protocols.SetHTTP1(true)
	}
	transport := &http.Transport{
		Protocols: protocols,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, proxyAddr)
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 30 * time.Second}
}

// result is a completed exchange: status, headers, and the body already
// read. Returning them together keeps every caller off the closer, which is
// also what stops a test asserting on a body it forgot to drain.
type result struct {
	status int
	header http.Header
	body   string
}

// fetch issues a GET to an authority through the proxy and reads it out.
func fetch(t *testing.T, c *http.Client, authority, path string) result {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://"+authority+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET http://%s%s: %v", authority, path, err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return result{status: resp.StatusCode, header: resp.Header.Clone(), body: string(payload)}
}

// connectEnvelope issues a Connect unary POST to an authority through the
// proxy and returns the error envelope it answered with, alongside the
// exchange itself — the status is half of what a Connect caller reads, so a
// test that wants the code usually wants both.
//
// It is a hand-built request rather than a connect-go client because these
// tests are about what voya puts on the wire: connect-go would parse the
// envelope, and a body it rejected would arrive as an error about the
// transport instead of the thing under test. The content type is asserted
// here, once, because connect-go discards a body carrying anything else —
// which is the whole reason ADR 005's amendment exists.
func connectEnvelope(t *testing.T, c *http.Client, authority, procedure string) (envelope, result) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"http://"+authority+procedure, strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")

	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST http://%s%s: %v", authority, procedure, err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	got := result{status: resp.StatusCode, header: resp.Header.Clone(), body: string(payload)}

	if ct := got.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json — connect-go discards anything else", ct)
	}
	var env envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatalf("the body is not a Connect error envelope (%v):\n%s", err, got.body)
	}
	return env, got
}

// envelope is the Connect protocol's unary error body, which is the only
// JSON voya writes (ADR 003, ADR 005).
type envelope struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// registered puts a live backend in reg under name and returns the authority
// a caller dials.
func registered(t *testing.T, reg *registry.Registry, name string, handler http.Handler) string {
	t.Helper()
	port := backend(t, handler)
	if _, err := reg.Register(registry.Instance{Service: name, Port: port}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return registry.Authority(name)
}

// --- routing -----------------------------------------------------------------

func TestRoutesByAuthority(t *testing.T) {
	reg := registry.New()
	// Through a channel, not a variable: the handler runs on the server's
	// goroutine and nothing in an HTTP exchange orders its writes against
	// this one's reads.
	gotPath := make(chan string, 1)
	registered(t, reg, "payments", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath <- r.URL.Path
		fmt.Fprint(w, "payments here")
	}))
	registered(t, reg, "billing", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "billing here")
	}))

	proxyAddr := serveProxy(t, reg)
	c := client(t, proxyAddr, false)

	if got := fetch(t, c, "payments.internal:9777", "/v1/charge").body; got != "payments here" {
		t.Errorf("payments.internal reached %q", got)
	}
	select {
	case got := <-gotPath:
		if got != "/v1/charge" {
			t.Errorf("backend saw path %q, want /v1/charge — the proxy rewrote it", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the backend was never reached")
	}
	if got := fetch(t, c, "billing.internal:9777", "/").body; got != "billing here" {
		t.Errorf("billing.internal reached %q", got)
	}
}

// TestAuthoritySurvivesTheHop is the single most important assertion in the
// package. httputil's SetURL sets Out.Host = "", which makes the outbound
// authority the upstream's ephemeral address: the request still arrives, and
// the name the whole design routes on is gone. A backend that cannot see the
// name it was called by cannot serve reflection or multi-tenant routing, and
// the dev/prod parity ADR 002 exists for is broken.
func TestAuthoritySurvivesTheHop(t *testing.T) {
	reg := registry.New()
	gotHost := make(chan string, 1)
	registered(t, reg, "payments", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost <- r.Host
	}))

	c := client(t, serveProxy(t, reg), false)
	fetch(t, c, "payments.internal:9777", "/")

	select {
	case got := <-gotHost:
		if got != "payments.internal:9777" {
			t.Errorf("backend saw Host %q, want %q — the authority did not survive the hop",
				got, "payments.internal:9777")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the backend was never reached")
	}
}

// TestRestartedBackendGetsTraffic is ADR 002's promise from the caller's
// side: the service moved to a different ephemeral port and the client, which
// knows only the name, does not change.
func TestRestartedBackendGetsTraffic(t *testing.T) {
	reg := registry.New()
	registered(t, reg, "payments", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "first instance")
	}))
	proxyAddr := serveProxy(t, reg)
	c := client(t, proxyAddr, false)

	if got := fetch(t, c, "payments.internal:9777", "/").body; got != "first instance" {
		t.Fatalf("first instance not reached: %q", got)
	}

	// The service restarts: new process, new ephemeral port, same name.
	registered(t, reg, "payments", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "second instance")
	}))

	if got := fetch(t, c, "payments.internal:9777", "/").body; got != "second instance" {
		t.Errorf("after the restart the proxy reached %q, want the new instance", got)
	}
}

// --- diagnostics -------------------------------------------------------------

func TestUnknownNameExplainsItself(t *testing.T) {
	reg := registry.New()
	registered(t, reg, "payments", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	c := client(t, serveProxy(t, reg), false)

	res := fetch(t, c, "paymnets.internal:9777", "/v1/charge")
	got := res.body

	if res.status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.status)
	}
	// ADR 005: name what arrived, say why, list what would have worked.
	for _, want := range []string{`"paymnets"`, "Nothing has ever registered", "payments.internal"} {
		if !strings.Contains(got, want) {
			t.Errorf("the diagnostic omits %q:\n%s", want, got)
		}
	}
	if res.header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want plain text for a human", res.header.Get("Content-Type"))
	}
}

// TestKnownButNotRunningIsADifferentAnswer: a service whose stream dropped
// and a typo are different problems, and conflating them sends the developer
// to the wrong terminal.
func TestKnownButNotRunningIsADifferentAnswer(t *testing.T) {
	reg := registry.New()
	handle, err := reg.Register(registry.Instance{Service: "reports", Port: 51234})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	handle.Close() // its stream dropped

	c := client(t, serveProxy(t, reg), false)
	res := fetch(t, c, "reports.internal:9777", "/")
	got := res.body

	if res.status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 — the name is known, it is just not up", res.status)
	}
	if !strings.Contains(got, "known but has no running instance") {
		t.Errorf("the diagnostic reads like a typo rather than a stopped service:\n%s", got)
	}
	if !strings.Contains(got, "Known but not running: reports.internal") {
		t.Errorf("the summary does not list it as known:\n%s", got)
	}
}

// TestNilErrorLogIsTheDefault: WithErrorLog(nil) must leave the proxy with
// a callable logf, or the first message it writes — here ErrorHandler's,
// on the one path whose job is to explain rather than to crash — panics.
// The handler is driven directly, not through serveProxy: net/http recovers
// a handler's panic and closes the connection, which would turn the crash
// into a client-side transport error, while a recorder lets it reach this
// goroutine and fail the test outright (#128).
func TestNilErrorLogIsTheDefault(t *testing.T) {
	reg := registry.New()
	if _, err := reg.Register(registry.Instance{Service: "payments", Port: deadPort(t)}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	handler := New(reg, WithErrorLog(nil))

	// The field itself, before the request: what the exchange below observes
	// is a panic that did not happen, and the two assertions it does make are
	// [TestRegisteredButUnreachable]'s. An ErrorHandler that stopped calling
	// p.logf would leave both of them passing and this test pinning nothing.
	p, ok := handler.(*proxy)
	if !ok {
		t.Fatalf("New returned %T, not *proxy — this test reads the field the option sets", handler)
	}
	if p.logf == nil {
		t.Fatal("WithErrorLog(nil) left p.logf nil: the first message the proxy writes is a panic")
	}

	req := httptest.NewRequest(http.MethodGet, "http://payments.internal:9777/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	if got := rec.Body.String(); !strings.Contains(got, "the connection failed") {
		t.Errorf("the diagnostic did not reach the caller:\n%s", got)
	}
}

func TestRegisteredButUnreachable(t *testing.T) {
	reg := registry.New()
	dead := deadPort(t)
	if _, err := reg.Register(registry.Instance{Service: "payments", Port: dead}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	c := client(t, serveProxy(t, reg), false)
	res := fetch(t, c, "payments.internal:9777", "/")
	got := res.body

	if res.status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", res.status)
	}
	if !strings.Contains(got, "the connection failed") {
		t.Errorf("a bare 502 is what ADR 005 forbids; got:\n%s", got)
	}
	// Both causes, because the second one arrives as an HTTP/2 framing error
	// that reads as a bug in voya rather than a backend serving HTTP/1.1.
	for _, want := range []string{strconv.FormatUint(uint64(dead), 10), "HTTP/2 or HTTP/1.1"} {
		if !strings.Contains(got, want) {
			t.Errorf("the diagnostic omits %q:\n%s", want, got)
		}
	}
}

// TestRegisteringVoyasOwnPortIsRefused: a service that reports the port its
// callers dial (9777, copied out of fly.toml) rather than the port it bound
// makes the proxy route to itself. Left to httputil that is an unbounded
// chain of hops and a client that hangs — no status, no body, nothing to
// read — which is the one outcome ADR 005 rules out entirely.
func TestRegisteringVoyasOwnPortIsRefused(t *testing.T) {
	reg := registry.New()
	proxyAddr := serveProxy(t, reg)
	if _, err := reg.Register(registry.Instance{Service: "loop", Port: portOfAddr(t, proxyAddr)}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Both protocols: the guard reads the connection's local address out of
	// the request context, and HTTP/1.1 and h2c build that context by
	// different routes.
	for _, tc := range []struct {
		name   string
		h2Only bool
	}{{"http/1.1", false}, {"h2c", true}} {
		t.Run(tc.name, func(t *testing.T) {
			res := fetch(t, client(t, proxyAddr, tc.h2Only), "loop.internal:9777", "/")
			if res.status != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 — failed_precondition's status: the fleet's registration is wrong, not the request (#88)", res.status)
			}
			if !strings.Contains(res.body, "voya's own proxy port") {
				t.Errorf("the diagnostic does not name the cause:\n%s", res.body)
			}
			if !strings.Contains(res.body, "actually bound") {
				t.Errorf("the diagnostic does not say what to register instead:\n%s", res.body)
			}
		})
	}
}

// TestReservedAuthorityIsNotATypo: voya.internal is ADR 005's own endpoint
// and validName refuses it forever, so the "nothing has ever registered that
// name" answer is advice nobody can act on — handed to the one developer
// whose routing is already broken enough to reach for it.
//
// Since #149 the daemon serves DiagService there, so the pin is aimed at what
// the name does not serve: a browser's `GET /`, on a proxy that has the
// internal handler mounted. The row and its message survive the surface
// existing; only its "this build does not serve it yet" went.
func TestReservedAuthorityIsNotATypo(t *testing.T) {
	reg := registry.New()
	c := client(t, serveProxy(t, reg, WithInternalHandler(internalStub())), false)

	res := fetch(t, c, registry.Authority(registry.Reserved)+":9777", "/")
	if res.status != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 — reserved, and not a DiagService procedure", res.status)
	}
	if strings.Contains(res.body, "Nothing has ever registered") {
		t.Errorf("the reserved name reads as a typo:\n%s", res.body)
	}
	if !strings.Contains(res.body, "reserved") {
		t.Errorf("the diagnostic does not say the name is reserved:\n%s", res.body)
	}
	if strings.Contains(res.body, "does not serve it yet") {
		t.Errorf("the diagnostic still claims the build does not serve the name (#149):\n%s", res.body)
	}
	if !strings.Contains(res.body, "DiagService") {
		t.Errorf("the diagnostic does not say what the name does serve:\n%s", res.body)
	}
}

// internalStubServed is what [internalStub]'s handler writes, so a test can
// tell "the router claimed it" from any answer the proxy writes itself.
const internalStubServed = "served by the internal handler"

// internalStubWellKnown is what the stub's non-procedure path answers, kept
// distinct from [internalStubServed] so a test can tell which kind of
// claimed pattern served — the proxy treats the two differently in the
// request log (#209).
const internalStubWellKnown = "well-known document from the internal handler"

// internalStub is a router claiming exactly what the daemon's real one does:
// DiagService's procedure paths — the generated constants, so the pins aimed
// at "not one of its procedures" are aimed at the same set the daemon serves
// — and, since #209, the JWKS well-known path, the one claimed pattern that
// is not a procedure. What each answers is a stub: the surfaces' own
// behaviour is internal/diag's and internal/auth's to prove; the proxy's
// part is dispatching to them, and only for the paths they claim.
func internalStub() *http.ServeMux {
	mux := http.NewServeMux()
	served := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, internalStubServed)
	})
	for _, procedure := range []string{
		diagv1connect.DiagServiceGetStatusProcedure,
		diagv1connect.DiagServiceListRoutesProcedure,
		diagv1connect.DiagServiceListRequestsProcedure,
		diagv1connect.DiagServiceWatchRequestsProcedure,
	} {
		mux.Handle(procedure, served)
	}
	mux.Handle(auth.JWKSPath, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, internalStubWellKnown)
	}))
	return mux
}

// TestReservedNonProcedurePathReachesTheRouter is #209's half of the seam:
// a claimed pattern that is not a Connect procedure — the JWKS document —
// is served by the router like any other claimed path, and without an
// internal router the same path answers the reserved row, which is what a
// proxy built without a daemon does.
func TestReservedNonProcedurePathReachesTheRouter(t *testing.T) {
	reg := registry.New()
	reserved := registry.Authority(registry.Reserved) + ":9777"

	c := client(t, serveProxy(t, reg, WithInternalHandler(internalStub())), false)
	res := fetch(t, c, reserved, auth.JWKSPath)
	if res.status != http.StatusOK || res.body != internalStubWellKnown {
		t.Errorf("with a router: status %d body %q, want the router's answer %q",
			res.status, res.body, internalStubWellKnown)
	}

	bare := client(t, serveProxy(t, reg), false)
	res = fetch(t, bare, reserved, auth.JWKSPath)
	if res.status != http.StatusNotImplemented {
		t.Errorf("without a router: status = %d, want the reserved row's 501 — a proxy built without a daemon serves nothing under the name", res.status)
	}
	if !strings.Contains(res.body, "reserved") {
		t.Errorf("without a router, the diagnostic does not say the name is reserved:\n%s", res.body)
	}
}

// TestInternalHandlerServesTheReservedNamesProcedures is #149's seam from
// the proxy's side: a request under voya.internal for a path the router
// claims reaches the handler, through the same authority routing every
// service takes; one for a path it does not claim answers from the reserved
// row. Both directions, because a dispatch that claimed everything under the
// name would leave `GET /` a 404 from the mux, and one that claimed nothing
// would be the pre-#149 proxy with an option that does nothing.
func TestInternalHandlerServesTheReservedNamesProcedures(t *testing.T) {
	reg := registry.New()
	c := client(t, serveProxy(t, reg, WithInternalHandler(internalStub())), false)
	authority := registry.Authority(registry.Reserved) + ":9777"

	served := fetch(t, c, authority, diagv1connect.DiagServiceGetStatusProcedure)
	if served.status != http.StatusOK || served.body != internalStubServed {
		t.Errorf("GET %s = %d %q, want 200 %q — the router claims that path",
			diagv1connect.DiagServiceGetStatusProcedure, served.status, served.body, internalStubServed)
	}

	unclaimed := fetch(t, c, authority, "/voya.diag.v1.DiagService/NoSuchProcedure")
	if unclaimed.status != http.StatusNotImplemented {
		t.Errorf("GET NoSuchProcedure = %d, want 501 — the reserved row, not the mux's 404", unclaimed.status)
	}
	if strings.Contains(unclaimed.body, internalStubServed) {
		t.Error("a path the router does not claim reached the handler")
	}

	// And a name that is not the reserved one never reaches the router,
	// however DiagService-shaped its path: the option is voya.internal's
	// alone.
	other := fetch(t, c, "payments.internal:9777", diagv1connect.DiagServiceGetStatusProcedure)
	if other.status != http.StatusNotFound || strings.Contains(other.body, internalStubServed) {
		t.Errorf("GET payments.internal%s = %d %q, want the never-registered row",
			diagv1connect.DiagServiceGetStatusProcedure, other.status, other.body)
	}
}

// TestWithoutAnInternalHandlerTheReservedNameAnswersTheRow: a proxy built
// with no daemon behind it — every other test in this package — serves
// nothing under voya.internal, and a DiagService path answers the reserved
// row rather than a nil dereference.
func TestWithoutAnInternalHandlerTheReservedNameAnswersTheRow(t *testing.T) {
	reg := registry.New()
	c := client(t, serveProxy(t, reg), false)

	res := fetch(t, c, registry.Authority(registry.Reserved)+":9777", diagv1connect.DiagServiceGetStatusProcedure)
	if res.status != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", res.status)
	}
}

// TestMultiLabelInternalNameSaysWhy: the client dialer captures every
// *.internal host on this port, so a multi-label name arrives here rather
// than at DNS. Telling its sender that voya "routes *.internal names" —
// which is what it just used — answers a question nobody asked.
func TestMultiLabelInternalNameSaysWhy(t *testing.T) {
	reg := registry.New()
	c := client(t, serveProxy(t, reg), false)

	res := fetch(t, c, "api.payments.internal:9777", "/")
	if res.status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — the name can never register, so the request as sent can never come good (#88)", res.status)
	}
	if !strings.Contains(res.body, "single label") {
		t.Errorf("the diagnostic does not say why the name does not route:\n%s", res.body)
	}
}

func TestAuthorityThatIsNotOurs(t *testing.T) {
	reg := registry.New()
	c := client(t, serveProxy(t, reg), false)

	res := fetch(t, c, "example.com:9777", "/")
	got := res.body
	if res.status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — the name can never register, so the request as sent can never come good (#88)", res.status)
	}
	if !strings.Contains(got, "not a name voya routes") {
		t.Errorf("the diagnostic does not say the name is not voya's:\n%s", got)
	}
}

// TestUnregistrableSingleLabelIsNotATypo is the third shape #88's criterion
// covers and the shape it is easiest to miss: a single label under
// .internal, which [registry.ServiceFromAuthority] accepts on shape and
// [registry.ValidName] refuses forever. It is not a typo — no registration
// can ever make the request as sent come good — so it must not answer from
// the retryable never-registered row, which is the loop #88 exists to close.
//
// Each row asserts its own setup, because the whole test is about a name the
// registry rejects: a row whose name the registry would in fact accept would
// be testing the never-registered path under this test's name and passing on
// the wrong branch. The control is the last row — a label the registry does
// accept still gets the retryable 404 — so a change that sent every
// unregistered name to 400 fails here rather than reading as a pass.
func TestUnregistrableSingleLabelIsNotATypo(t *testing.T) {
	for _, tc := range []struct {
		name       string
		label      string
		wantStatus int
		wantBody   string
	}{
		{"a character no service name may hold", "pay_ments", http.StatusBadRequest, "lowercase letters, digits and hyphens"},
		{"a leading hyphen", "-payments", http.StatusBadRequest, "start or end with a hyphen"},
		{"longer than a DNS label", strings.Repeat("p", 64), http.StatusBadRequest, "63 characters"},
		{"the control: a label that could register tomorrow", "paymnets", http.StatusNotFound, "Nothing has ever registered"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The setup, asserted: the registry's verdict on this label is
			// what the row is about, so read it rather than assume it.
			refused := registry.ValidName(tc.label) != nil
			if want := tc.wantStatus == http.StatusBadRequest; refused != want {
				t.Fatalf("registry.ValidName(%q) refuses = %v, want %v — the row is testing the other branch", tc.label, refused, want)
			}

			reg := registry.New()
			c := client(t, serveProxy(t, reg), false)

			res := fetch(t, c, tc.label+registry.Suffix+":9777", "/")
			if res.status != tc.wantStatus {
				t.Errorf("status = %d, want %d — the retryable row is for names that can still register (#88)", res.status, tc.wantStatus)
			}
			if !strings.Contains(res.body, tc.wantBody) {
				t.Errorf("the diagnostic omits %q:\n%s", tc.wantBody, res.body)
			}
			if refused && strings.Contains(res.body, "Nothing has ever registered") {
				t.Errorf("a name nothing may ever register reads as a typo:\n%s", res.body)
			}
		})
	}
}

// TestNoAuthority goes straight at the handler: a request with no Host is
// malformed on the wire, and net/http will not send one.
func TestNoAuthority(t *testing.T) {
	reg := registry.New()
	handler := New(reg)

	req := httptest.NewRequest(http.MethodPost, "http://placeholder/v1/charge", nil)
	req.Host = ""
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no :authority") {
		t.Errorf("the diagnostic does not name the missing header:\n%s", rec.Body.String())
	}
}

func TestDiagnosticListsNothingRegistered(t *testing.T) {
	reg := registry.New()
	c := client(t, serveProxy(t, reg), false)

	got := fetch(t, c, "payments.internal:9777", "/").body
	if !strings.Contains(got, "nothing has registered with voya yet") {
		t.Errorf("an empty registry should say so plainly:\n%s", got)
	}
}

// --- the Connect envelope (ADR 005's amendment) ------------------------------

// TestConnectUnaryGetsAReadableEnvelope is the amendment's point: connect-go
// discards a non-JSON error body, so the diagnostic has to arrive inside the
// protocol's own envelope or the entire fleet never sees it.
func TestConnectUnaryGetsAReadableEnvelope(t *testing.T) {
	reg := registry.New()
	c := client(t, serveProxy(t, reg), false)

	env, res := connectEnvelope(t, c, "paymnets.internal:9777", "/payments.v1.PaymentService/Charge")

	if env.Code != "unavailable" {
		t.Errorf("code = %q, want unavailable — a name that does not resolve to an endpoint", env.Code)
	}
	if !strings.Contains(env.Message, "paymnets") {
		t.Errorf("the message lost the diagnostic:\n%s", env.Message)
	}
	if res.status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", res.status)
	}
}

// TestReservedAuthorityIsUnimplementedForConnect is #69's fourth edge. The
// gRPC and gRPC-Web rows are in grpc_test.go and the plain-text one is
// [TestReservedAuthorityIsNotATypo]; all four read the same status argument,
// which is why one missing row was one bug in four places.
//
// The HTTP status is asserted alongside the envelope code because Connect
// pairs unimplemented with 501 — the same status the routing decision already
// produced — and a writer that changed the code without the status would
// hand connect-go an envelope disagreeing with its own transport.
//
// Aimed, since #149, at a procedure DiagService does not have, on a proxy
// that serves the ones it does: unimplemented is now literally true of the
// call, and it is the internal handler's dispatch that has to leave the row
// standing for it.
func TestReservedAuthorityIsUnimplementedForConnect(t *testing.T) {
	reg := registry.New()
	c := client(t, serveProxy(t, reg, WithInternalHandler(internalStub())), false)
	authority := registry.Authority(registry.Reserved) + ":9777"

	env, res := connectEnvelope(t, c, authority, "/voya.diag.v1.DiagService/NoSuchProcedure")

	if env.Code != "unimplemented" {
		t.Errorf("code = %q, want unimplemented — %s serves DiagService and has no such procedure, "+
			"so no retry can ever satisfy the call", env.Code, authority)
	}
	if res.status != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 — Connect maps unimplemented to it, and it is what the "+
			"routing decision produced", res.status)
	}
	if !strings.Contains(env.Message, "reserved") {
		t.Errorf("the diagnostic does not say the name is reserved:\n%s", env.Message)
	}
}

// TestNeverRoutableNameIsInvalidArgumentForConnect reads #88's row for a
// name that can never register off the wire as a Connect unary call. Before
// #88 the row was the never-registered one — unavailable, 503 — which told
// a caller with a retry policy to keep retrying a request no registration
// can end. [TestTrailersOnlyShapeOnTheWire] pins the gRPC field of the same
// row and [TestCodeForHasARowForEveryOutcome] the struct; this is the only
// place writeConnectError's two fields for it are exercised (#128).
func TestNeverRoutableNameIsInvalidArgumentForConnect(t *testing.T) {
	reg := registry.New()
	c := client(t, serveProxy(t, reg), false)
	authority := "api.payments.internal:9777"

	env, res := connectEnvelope(t, c, authority, "/payments.v1.PaymentService/Charge")

	if env.Code != "invalid_argument" {
		t.Errorf("code = %q, want invalid_argument — %s is not a name voya can ever route, "+
			"so no retry of the request as sent can succeed", env.Code, authority)
	}
	if res.status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — Connect's canonical status for invalid_argument", res.status)
	}
	if !strings.Contains(env.Message, "single label") {
		t.Errorf("the diagnostic does not say what a service name looks like:\n%s", env.Message)
	}
}

// TestRoutingLoopIsFailedPreconditionForConnect reads #88's row for a
// service registered on voya's own port off the wire as a Connect unary
// call. Before #88 the loop shared the unreachable row, which reaches a
// Connect caller as unavailable under 503 — the retryable answer to a
// registration isSelf refuses identically until a human fixes it, and the
// pairing Connect itself specifies for that code, so nothing in the envelope
// looked wrong. What #88 changed is the decision, not a mismatched status.
// The fixture registers the proxy's own address once the listener is up, the
// way [TestTrailersOnlyShapeOnTheWire]'s loop row does (#128).
func TestRoutingLoopIsFailedPreconditionForConnect(t *testing.T) {
	reg := registry.New()
	proxyAddr := serveProxy(t, reg)
	if _, err := reg.Register(registry.Instance{Service: "loop", Port: portOfAddr(t, proxyAddr)}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	c := client(t, proxyAddr, false)

	env, res := connectEnvelope(t, c, "loop.internal:9777", "/loop.v1.LoopService/Spin")

	if env.Code != "failed_precondition" {
		t.Errorf("code = %q, want failed_precondition — the registration is wrong, not the request, "+
			"and no retry fixes it", env.Code)
	}
	if res.status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — Connect's canonical status for failed_precondition", res.status)
	}
	if !strings.Contains(env.Message, "voya's own proxy port") {
		t.Errorf("the diagnostic does not name the loop:\n%s", env.Message)
	}
}

// TestNoAuthorityIsInvalidArgumentNotUnavailable: a caller with a retry
// policy must not be told to retry something no retry can fix.
func TestNoAuthorityIsInvalidArgumentNotUnavailable(t *testing.T) {
	reg := registry.New()
	handler := New(reg)

	req := httptest.NewRequest(http.MethodPost, "http://placeholder/svc/Method", strings.NewReader("{}"))
	req.Host = ""
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var envelope struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("not an envelope (%v): %s", err, rec.Body.String())
	}
	if envelope.Code != "invalid_argument" {
		t.Errorf("code = %q, want invalid_argument: no retry supplies a missing authority", envelope.Code)
	}
}

func TestIsConnectUnary(t *testing.T) {
	cases := []struct {
		name   string
		build  func() *http.Request
		expect bool
	}{
		{
			name: "connect unary POST",
			build: func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "http://h/s/M", nil)
				r.Header.Set("Connect-Protocol-Version", "1")
				r.Header.Set("Content-Type", "application/json")
				return r
			},
			expect: true,
		},
		{
			name: "connect streaming POST",
			build: func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "http://h/s/M", nil)
				r.Header.Set("Connect-Protocol-Version", "1")
				r.Header.Set("Content-Type", "application/connect+proto")
				return r
			},
			expect: false, // errors go in the end-of-stream envelope, not a JSON body
		},
		{
			name: "connect GET with both markers",
			build: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "http://h/s/M?connect=v1&encoding=json", nil)
			},
			expect: true,
		},
		{
			name: "browser URL that happens to carry connect=v1",
			build: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "http://h/page?connect=v1", nil)
			},
			expect: false, // no encoding: an ordinary query parameter name
		},
		{
			name: "plain JSON POST from a webhook",
			build: func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "http://h/hook", nil)
				r.Header.Set("Content-Type", "application/json")
				return r
			},
			expect: false, // no version marker: a human must get the readable body
		},
		{
			name: "gRPC POST",
			build: func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "http://h/s/M", nil)
				r.Header.Set("Content-Type", "application/grpc")
				return r
			},
			expect: false, // outcome belongs in trailers: isGRPC claims it
		},
		{
			name: "connect POST with no content type",
			build: func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "http://h/s/M", nil)
				r.Header.Set("Connect-Protocol-Version", "1")
				return r
			},
			expect: false, // unrecognisable: plain text at least leaves a body
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isConnectUnary(tc.build()); got != tc.expect {
				t.Errorf("isConnectUnary = %v, want %v", got, tc.expect)
			}
		})
	}
}

// --- headers -----------------------------------------------------------------

// TestTraceHeadersSurviveUnchanged: voya creates no spans and collects
// nothing, but breaking propagation would leave the fleet's trace plumbing
// unexercised until deploy (ADR 005).
func TestTraceHeadersSurviveUnchanged(t *testing.T) {
	const (
		traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
		tracestate  = "vendor1=value1,vendor2=value2"
	)
	reg := registry.New()
	seen := make(chan http.Header, 1)
	registered(t, reg, "payments", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
	}))

	c := client(t, serveProxy(t, reg), false)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://payments.internal:9777/", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Traceparent", traceparent)
	req.Header.Set("Tracestate", tracestate)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	select {
	case got := <-seen:
		if got.Get("Traceparent") != traceparent {
			t.Errorf("traceparent = %q, want %q", got.Get("Traceparent"), traceparent)
		}
		if got.Get("Tracestate") != tracestate {
			t.Errorf("tracestate = %q, want %q", got.Get("Tracestate"), tracestate)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the backend was never reached")
	}
}

func TestCORS(t *testing.T) {
	reg := registry.New()
	registered(t, reg, "payments", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	c := client(t, serveProxy(t, reg), false)

	t.Run("preflight is answered by voya", func(t *testing.T) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodOptions,
			"http://payments.internal:9777/payments.v1.PaymentService/Charge", nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Origin", "http://localhost:3000")
		req.Header.Set("Access-Control-Request-Method", "POST")
		req.Header.Set("Access-Control-Request-Headers", "content-type,connect-protocol-version")

		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("status = %d, want 204", resp.StatusCode)
		}
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
			t.Errorf("Allow-Origin = %q, want the reflected origin", got)
		}
		if got := resp.Header.Get("Access-Control-Allow-Headers"); !strings.Contains(got, "connect-protocol-version") {
			t.Errorf("Allow-Headers = %q, want the requested headers", got)
		}
		// Without these a browser cannot read a gRPC-Web status at all.
		if got := resp.Header.Get("Access-Control-Expose-Headers"); !strings.Contains(got, "Grpc-Status") {
			t.Errorf("Expose-Headers = %q, want Grpc-Status", got)
		}
	})

	t.Run("a plain OPTIONS is the service's to answer", func(t *testing.T) {
		// Not a preflight: no Origin, no Access-Control-Request-Method. A
		// service that advertises Allow: or serves REST discovery has to
		// receive it, or voya is the one hop on the machine that eats it.
		reached := make(chan struct{}, 1)
		reg := registry.New()
		registered(t, reg, "payments", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodOptions {
				reached <- struct{}{}
				w.Header().Set("Allow", "GET, POST, OPTIONS")
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		c := client(t, serveProxy(t, reg), false)

		req, err := http.NewRequestWithContext(context.Background(), http.MethodOptions,
			"http://payments.internal:9777/v1/charge", nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer resp.Body.Close()

		select {
		case <-reached:
		case <-time.After(5 * time.Second):
			t.Fatal("voya answered a non-preflight OPTIONS itself; the service never saw it")
		}
		if got := resp.Header.Get("Allow"); got != "GET, POST, OPTIONS" {
			t.Errorf("Allow = %q, want the backend's", got)
		}
	})

	t.Run("the backend's own CORS headers are not doubled", func(t *testing.T) {
		// A browser-facing service ships CORS middleware, because on Fly it
		// must. httputil copies upstream response headers in with Add, and a
		// response carrying two Access-Control-Allow-Origin values is
		// rejected by every browser — so voya's have to replace them.
		reg := registry.New()
		registered(t, reg, "payments", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Expose-Headers", "X-Backend")
			fmt.Fprint(w, "ok")
		}))
		c := client(t, serveProxy(t, reg), false)

		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
			"http://payments.internal:9777/", nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Origin", "http://localhost:3000")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)

		if got := resp.Header.Values("Access-Control-Allow-Origin"); len(got) != 1 {
			t.Errorf("Allow-Origin = %v; a browser rejects a response carrying more than one", got)
		} else if got[0] != "http://localhost:3000" {
			t.Errorf("Allow-Origin = %q, want voya's reflected origin", got[0])
		}
		if got := resp.Header.Values("Access-Control-Expose-Headers"); len(got) != 1 {
			t.Errorf("Expose-Headers = %v, want only voya's", got)
		}
	})

	t.Run("with CORS off the backend's headers are left alone", func(t *testing.T) {
		reg := registry.New()
		registered(t, reg, "payments", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			fmt.Fprint(w, "ok")
		}))
		c := client(t, serveProxy(t, reg, WithCORS(false)), false)

		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
			"http://payments.internal:9777/", nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Origin", "http://localhost:3000")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)

		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("Allow-Origin = %q, want the backend's own — voya was told to stay out", got)
		}
	})

	t.Run("no Origin means no CORS headers", func(t *testing.T) {
		res := fetch(t, c, "payments.internal:9777", "/")
		if got := res.header.Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Allow-Origin = %q on a non-browser request", got)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		plain := client(t, serveProxy(t, reg, WithCORS(false)), false)
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
			"http://payments.internal:9777/", nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Origin", "http://localhost:3000")
		resp, err := plain.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer resp.Body.Close()
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Allow-Origin = %q with CORS off", got)
		}
	})
}

// --- bodies and streams ------------------------------------------------------

// TestLargeBodiesRoundTrip in both directions. The assertion that matters is
// that they arrive intact; the one below covers not buffering them.
func TestLargeBodiesRoundTrip(t *testing.T) {
	const size = 4 << 20 // 4 MiB
	payload := strings.Repeat("voya", size/4)

	reg := registry.New()
	registered(t, reg, "payments", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("backend reading request: %v", err)
			return
		}
		if len(got) != len(payload) {
			t.Errorf("backend got %d bytes, want %d", len(got), len(payload))
		}
		_, _ = w.Write(got)
	}))

	c := client(t, serveProxy(t, reg), false)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"http://payments.internal:9777/echo", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response: %v", err)
	}
	if len(got) != len(payload) {
		t.Errorf("response was %d bytes, want %d", len(got), len(payload))
	}
}

// TestAnEarlyResponseReachesACallerStillSending is the other half of
// [TestResponseIsNotBuffered], and the half httputil does not cover on its
// own: a *fixed-length* response — a 404, a diagnostic, anything with a
// Content-Length — written while the caller's request body is still open.
//
// A bidi caller sends its next message only once it has been answered, so an
// answer held until the request body ends is an answer that never arrives:
// the caller waits for the response, the proxy waits for the body, and the
// deadline breaks the tie. #158 met it as `voya call` taking its whole
// 10-second budget to be told a service serves no reflection.
//
// The body here is a pipe with a byte in it and no end, which is what a bidi
// stream looks like on the wire, and the assertion is only that the response
// arrives at all while it is open. Drop FlushInterval from [New] and this
// waits for the context instead.
func TestAnEarlyResponseReachesACallerStillSending(t *testing.T) {
	reg := registry.New()
	registered(t, reg, "payments", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Fixed length, and written without reading the request body: the
		// shape of every "I do not serve that" a backend answers with.
		http.Error(w, "no such procedure", http.StatusNotFound)
	}))

	// h2 only: an open request body on HTTP/1.1 is a different exchange, and
	// bidi streaming is HTTP/2 by definition.
	c := client(t, serveProxy(t, reg), true)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	body, sending := io.Pipe()
	defer body.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://payments.internal:9777/pkg.Svc/Chat", body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	go func() { _, _ = sending.Write([]byte("first message")) }()

	// The status comes back rather than the response: the goroutine that made
	// the call owns the body and closes it, and the assertion belongs to the
	// test goroutine.
	answered := make(chan int, 1)
	failed := make(chan error, 1)
	go func() {
		resp, err := c.Do(req)
		if err != nil {
			failed <- err
			return
		}
		defer resp.Body.Close()
		answered <- resp.StatusCode
	}()

	select {
	case status := <-answered:
		if status != http.StatusNotFound {
			t.Errorf("status = %d, want %d", status, http.StatusNotFound)
		}
	case err := <-failed:
		t.Fatalf("the caller never saw the backend's answer: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the backend's answer did not reach a caller whose request body is still open " +
			"— the proxy is holding it until the request ends")
	}
	_ = sending.Close()
}

// TestResponseIsNotBuffered: a long-lived stream is the normal case behind
// this proxy, and buffering a whole response would break every one of them.
// The proof is that the client reads a chunk while the backend is still
// holding the response open.
func TestResponseIsNotBuffered(t *testing.T) {
	reg := registry.New()
	release := make(chan struct{})
	registered(t, reg, "payments", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "first chunk\n")
		w.(http.Flusher).Flush()
		<-release // the response is deliberately unfinished
		fmt.Fprint(w, "second chunk\n")
	}))

	c := client(t, serveProxy(t, reg), false)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://payments.internal:9777/stream", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	defer close(release)

	first := make([]byte, len("first chunk\n"))
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatalf("reading the first chunk: %v — the proxy is buffering the response", err)
	}
	if string(first) != "first chunk\n" {
		t.Errorf("first chunk = %q", first)
	}
}

// TestBackendClosesMidResponse: the client must see a failure rather than the
// proxy hanging or panicking.
func TestBackendClosesMidResponse(t *testing.T) {
	reg := registry.New()
	registered(t, reg, "payments", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1024")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "partial")
		w.(http.Flusher).Flush()
		// Abandon the response having promised 1024 bytes. ErrAbortHandler
		// rather than hijacking the connection: hijacking is HTTP/1.1 only,
		// so on an h2c hop the assertion would panic, net/http would recover
		// it, and this test would pass without ever truncating anything.
		panic(http.ErrAbortHandler)
	}))

	c := client(t, serveProxy(t, reg), false)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://payments.internal:9777/", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return // failing at the response is a fine outcome too
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Error("a truncated response read as complete")
	}
}

// --- upstream connection pooling ---------------------------------------------

// TestAProxiedToServiceShutsDownPromptly is #52. voya kept a pooled h2c
// connection to every service it had ever routed to and nothing reaped it, so
// Go's HTTP/2 graceful shutdown — which sends GOAWAY and then waits about a
// second for the peer to let go — found voya still holding on. A service that
// had served exactly one proxied call took 1.049s to shut down against 74µs
// for one that had never been routed to, and paid it on every restart for the
// life of the process: the workflow ADR 004 is written around.
//
// Gated on the median, because one run measures the machine as much as the
// code. The bound is deliberately loose — what was removed is a wait about a
// second long, so half of that is something only a reaped connection can meet,
// and it leaves room for a host as contended as the ones the grace period's
// own numbers were taken on.
func TestAProxiedToServiceShutsDownPromptly(t *testing.T) {
	const runs = 3
	const bound = 500 * time.Millisecond

	// The bound only means anything while the constant it is bounding stays
	// under it, and nothing else in the package checks that: this test's wait
	// and [TestASilentStreamSurvivesTheIdleTimeout]'s silence are both derived
	// from IdleUpstreamTimeout, so they follow the value upwards and stay green
	// while a raised one hands most of #52's second back. Asserted here rather
	// than as a bare equality, because what has to hold is not that the value
	// is 250ms but that a restart cannot cost more than this test bounds it at.
	if IdleUpstreamTimeout > bound {
		t.Fatalf("IdleUpstreamTimeout is %s, over the %s bound this test holds a restart to. "+
			"A service stopped inside the window waits out the remainder, so a value above the "+
			"bound gives back the wait #52 removed — and every test in this package would still pass.",
			IdleUpstreamTimeout, bound)
	}

	took := make([]time.Duration, 0, runs)
	for range runs {
		took = append(took, timeShutdownAfterOneProxiedCall(t))
	}
	slices.Sort(took)

	if median := took[len(took)/2]; median > bound {
		t.Errorf("a service that served one proxied call took %s to shut down (median of %d: %v), "+
			"over the %s bound. voya is holding a pooled connection to it past "+
			"IdleUpstreamTimeout (%s), so the service is waiting out Go's HTTP/2 GOAWAY grace.",
			median, runs, took, bound, IdleUpstreamTimeout)
	}
}

// reapAllowance is the headroom every test here allows a reap on top of the
// timeout that governs it. It bounds a wait, it does not time one: a working
// reap lands at that timeout and nothing waits the rest. Sized well clear of
// the scheduling tail ADR 004 records for this project's CI host — 66.8ms at
// p99 and 106.7ms worst sample — because the cost of being wrong is a flake
// in the tests guarding #52.
//
// Two tests spend it and they spend it differently, so shrinking it on the
// sizing argument alone weakens one of them. [timeShutdownAfterOneProxiedCall]
// waits IdleUpstreamTimeout plus this and then measures anyway, deliberately
// non-fatal, for the reason written there. [TestWithIdleUpstreamTimeoutGovernsTheReap]
// waits the value its option set plus this, and expiry there *is* the failure:
// nothing was reaped, so there is nothing to measure.
const reapAllowance = 2 * time.Second

// timeShutdownAfterOneProxiedCall stages one service, routes one call to it
// through the proxy, waits for the pooled connection to be reaped, and reports
// what shutting the service down then costs.
//
// Everything is built fresh per call. The pool that matters is the proxy's and
// it is keyed by upstream address, so reusing either end across runs would
// measure the second run against the first one's connection.
func timeShutdownAfterOneProxiedCall(t *testing.T) time.Duration {
	t.Helper()

	reg := registry.New()
	port, srv, upstreamGone := backendServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	if _, err := reg.Register(registry.Instance{Service: "payments", Port: port}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// The measurement only means anything if voya's pooled connection is
	// reaped by voya. A backend with an inbound IdleTimeout reaps it from the
	// far end instead, and this test then passes with [UpstreamTransport]'s
	// IdleConnTimeout deleted — #52 unfixed and green, which is what #72 did
	// for one commit by building backends out of [NewHTTPServer].
	// [backendServer] tells the story; this is what would have caught it.
	if srv.IdleTimeout != 0 {
		t.Fatalf("the backend has IdleTimeout %s, so it reaps voya's pooled connection itself "+
			"and this test no longer measures IdleUpstreamTimeout. A backend is a service: it "+
			"gets h2c from [H2CProtocols], never the proxy server's timeouts (#52, #72).",
			srv.IdleTimeout)
	}

	// The response is asserted, not merely drained. A proxy that answered its
	// own diagnostic instead of routing — a broken lookup, an isSelf false
	// positive, a name that never registered — returns a status and a body
	// through c.Do without error and never dials upstream, so it never pools
	// the connection this test exists to find. That shuts down promptly for
	// the one reason the measurement must not accept.
	c := client(t, serveProxy(t, reg), false)
	if got := fetch(t, c, "payments.internal:9777", "/"); got.status != http.StatusOK || got.body != "ok" {
		t.Fatalf("the call did not reach the backend: status %d, body %q — want 200 and %q. "+
			"Nothing was proxied, so there is no pooled connection to measure the shutdown against.",
			got.status, got.body, "ok")
	}

	// Wait for the reap itself rather than for a duration that ought to cover
	// it. A fixed margin over IdleUpstreamTimeout is a bet on the scheduler,
	// and ADR 004 measures a wait of the same shape losing it on this
	// project's own CI host: 66.8ms at p99, worst sample 106.7ms, so the
	// 100ms this used to allow sat between the two and the worst run already
	// exceeded it. Losing the bet silently turns a run into the 1.05s
	// measurement the bound exists to catch, which is a flake reported as the
	// bug.
	//
	// Deliberately not fatal when it does not arrive. A connection that is
	// never reaped *is* the #52 regression, so the run carries on and times
	// the shutdown anyway: the failure then arrives as the second it costs,
	// against the bound, which is the evidence the fix was measured with,
	// rather than as the absence of a signal. The allowance is only ever paid
	// on the way to failing.
	select {
	case <-upstreamGone:
	case <-time.After(reapAllowance):
	}

	// What is asserted is the cost of shutting down once the connection has
	// certainly been reaped. A service stopped *inside* the window still waits
	// out the remainder, which is the documented limit of fixing this with a
	// timer rather than with an eviction hook.
	start := time.Now()
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	return time.Since(start)
}

// TestASilentStreamSurvivesTheIdleTimeout pins the claim that kept the idle
// timeout off until #52 measured it — "a backend may legitimately hold a
// stream open for hours, and a timeout here would cut long-lived streams for
// no reason voya knows about".
//
// It would not. IdleConnTimeout reaps only connections the transport has
// booked back in as idle, and one carrying a stream is not idle however quiet
// the stream goes. That distinction is the whole basis of [IdleUpstreamTimeout]
// existing, and nothing else in the repo holds it.
//
// What it pins is that invariant and not the constant's value, which is worth
// being exact about: no setting of IdleUpstreamTimeout can reap a connection
// with a stream on it, so this test stays green however far the value is
// lowered — checked at 1ms against 3s of silence, three thousand times the
// timeout. Lowering the value is [TestAProxiedToServiceShutsDownPromptly]'s
// business, not this one's. What this catches is the upstream leg acquiring
// something that *does* cut a quiet stream — a ResponseHeaderTimeout, a read
// deadline, a different HTTP/2 transport — which is what would actually make
// ADR 004's debugger-pause case stop being true.
func TestASilentStreamSurvivesTheIdleTimeout(t *testing.T) {
	const times = 3
	silence := times * IdleUpstreamTimeout

	reg := registry.New()
	registered(t, reg, "payments", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "first chunk\n")
		w.(http.Flusher).Flush()
		time.Sleep(silence) // the debugger pause, staged
		fmt.Fprint(w, "second chunk\n")
		w.(http.Flusher).Flush()
	}))

	c := client(t, serveProxy(t, reg), false)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://payments.internal:9777/stream", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	first := make([]byte, len("first chunk\n"))
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatalf("reading the first chunk: %v", err)
	}
	// Checked before the silence, so that a first chunk which is not what the
	// handler wrote fails here rather than misaligning the read below and
	// reporting itself as a reaped connection.
	if string(first) != "first chunk\n" {
		t.Fatalf("first chunk = %q, want %q", first, "first chunk\n")
	}

	// This read is the test. It spans the silence, so it is the one that
	// fails if the proxy's connection was reaped out from under the stream.
	second := make([]byte, len("second chunk\n"))
	if _, err := io.ReadFull(resp.Body, second); err != nil {
		t.Fatalf("after %s of silence — %d times IdleUpstreamTimeout — the second chunk did not "+
			"arrive: %v. The proxy's pooled connection was reaped while it was still carrying "+
			"the stream, and a debugger pause now breaks a proxied call.", silence, times, err)
	}
	if string(second) != "second chunk\n" {
		t.Errorf("second chunk = %q, want %q", second, "second chunk\n")
	}
}

// TestWithIdleUpstreamTimeoutGovernsTheReap is #73: the reap is measured
// against a value a test set, not against the shipped constant. Before the
// option existed nothing could do this — both of #71's tests derive their
// waits from IdleUpstreamTimeout, so they follow the value wherever it goes,
// and the guard in [TestAProxiedToServiceShutsDownPromptly] bounds the
// constant rather than observing the reap.
//
// The option is set to twice the default so that each way of losing it has
// its own signature: an option that never reaches the transport leaves the
// default reaping at [IdleUpstreamTimeout], well before the lower bound
// asserted here, and an option that arrives as zero disables the reap
// outright, which the allowance catches.
//
// The lower bound needs no median, unlike the package's other timing
// assertions: contention can only push a reap later, so a busy machine
// cannot fail it.
func TestWithIdleUpstreamTimeoutGovernsTheReap(t *testing.T) {
	timeout := 2 * IdleUpstreamTimeout

	reg := registry.New()
	port, srv, upstreamGone := backendServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	if _, err := reg.Register(registry.Instance{Service: "payments", Port: port}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Same guard as [timeShutdownAfterOneProxiedCall], load-bearing the same
	// way: a backend with an inbound IdleTimeout closes the connection from
	// the far end at its own schedule, and the close this test times is then
	// the backend's rather than the option's.
	if srv.IdleTimeout != 0 {
		t.Fatalf("the backend has IdleTimeout %s, so it reaps voya's pooled connection itself "+
			"and this test no longer measures WithIdleUpstreamTimeout (#52, #72).", srv.IdleTimeout)
	}

	c := client(t, serveProxy(t, reg, WithIdleUpstreamTimeout(timeout)), false)

	// start is taken before the call, so the connection cannot have been
	// booked idle before it: a reap governed by the option then lands
	// strictly after start+timeout, and the lower bound cannot flake on the
	// gap between the body draining and fetch returning.
	start := time.Now()

	// Asserted, not merely issued, for [timeShutdownAfterOneProxiedCall]'s
	// reason: a proxy that answered its own diagnostic never dialled
	// upstream, so there is no pooled connection and the failure would
	// arrive as the allowance expiring — the wrong finding.
	if got := fetch(t, c, "payments.internal:9777", "/"); got.status != http.StatusOK || got.body != "ok" {
		t.Fatalf("the call did not reach the backend: status %d, body %q — want 200 and %q. "+
			"Nothing was proxied, so there is no pooled connection to measure the reap of.",
			got.status, got.body, "ok")
	}
	awaitProbeClosed(t, upstreamGone)

	select {
	case <-upstreamGone:
	case <-time.After(timeout + reapAllowance):
		t.Fatalf("the pooled connection was never reaped: %s past the %s the option set, and "+
			"still open. The value reached the transport as zero — which disables the reap — "+
			"rather than as the one set here, and a proxy built with WithIdleUpstreamTimeout "+
			"has #52 back. (An option that never arrived at all reaps at the %s default "+
			"instead, and fails the lower bound below.)", reapAllowance, timeout, IdleUpstreamTimeout)
	}
	if elapsed := time.Since(start); elapsed < timeout {
		t.Errorf("the pooled connection was reaped %s after the call, before the %s the option "+
			"set — the transport is still carrying the default (%s), so WithIdleUpstreamTimeout "+
			"is not wired through.", elapsed, timeout, IdleUpstreamTimeout)
	}
}

// TestWithIdleUpstreamTimeoutZeroDisablesTheReap pins the other half of
// [WithIdleUpstreamTimeout]'s contract: zero is not "unset", it is
// http.Transport's own way of spelling no reap at all, which is the pre-#52
// behaviour and hands back the second #52 measured.
//
// It is worth a test rather than a sentence because the tidy-up that breaks
// it is the obvious one — folding a zero back to [IdleUpstreamTimeout] after
// the options run, the way a nil logf or an unset field would be defaulted —
// and no other test in the package can tell. Mutation-proved with exactly
// that: `if p.idleUpstream == 0 { p.idleUpstream = IdleUpstreamTimeout }`
// added to [New], which this test then fails on the reap it observes.
//
// The assertion is an absence, so its direction is the safe one: contention
// can only delay a reap, so a busy machine cannot invent one inside the
// window. What it cannot do is prove the connection is held forever — only
// that it outlives a window the default would not have.
func TestWithIdleUpstreamTimeoutZeroDisablesTheReap(t *testing.T) {
	// Four times the default, so a reap governed by the default — or by any
	// value below this — lands well inside the window and is caught. Not
	// derived from reapAllowance: that is headroom on a reap this test
	// expects never to come.
	const times = 4
	held := times * IdleUpstreamTimeout

	reg := registry.New()
	port, srv, upstreamGone := backendServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	if _, err := reg.Register(registry.Instance{Service: "payments", Port: port}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Load-bearing harder here than in the tests that expect a reap: this one
	// asserts nothing closes, so a backend reaping voya's connection from the
	// far end would report itself as the option having been ignored — a
	// failure naming the wrong side of the dial.
	if srv.IdleTimeout != 0 {
		t.Fatalf("the backend has IdleTimeout %s, so it closes voya's pooled connection itself "+
			"and this test would blame WithIdleUpstreamTimeout for the backend's reap (#52, #72).",
			srv.IdleTimeout)
	}

	c := client(t, serveProxy(t, reg, WithIdleUpstreamTimeout(0)), false)

	// Asserted rather than merely issued, for [timeShutdownAfterOneProxiedCall]'s
	// reason and with more at stake: a proxy that answered its own diagnostic
	// never dialled upstream, so there is no pooled connection — and a test
	// whose whole assertion is that no connection closes passes trivially.
	if got := fetch(t, c, "payments.internal:9777", "/"); got.status != http.StatusOK || got.body != "ok" {
		t.Fatalf("the call did not reach the backend: status %d, body %q — want 200 and %q. "+
			"Nothing was proxied, so there is no pooled connection to hold open and this test "+
			"would pass without exercising the option at all.", got.status, got.body, "ok")
	}
	awaitProbeClosed(t, upstreamGone)

	select {
	case <-upstreamGone:
		t.Errorf("the pooled connection was reaped inside %s — %d times IdleUpstreamTimeout (%s) — "+
			"though the option set zero, which disables the reap. Something is putting a non-zero "+
			"IdleConnTimeout on the transport anyway: most likely a zero read as \"unset\" and "+
			"defaulted back to the constant, which hands a caller of WithIdleUpstreamTimeout(0) "+
			"the opposite of what it asked for.", held, times, IdleUpstreamTimeout)
	case <-time.After(held):
	}
}

// --- caller connection pooling -------------------------------------------------

// TestAQuietHTTP11KeepAliveCallerKeepsItsConnection pins the absence #94
// bought: no timer sits on the proxy's inbound leg, so a pooled HTTP/1.1
// keep-alive connection stays up however long its caller goes quiet, and the
// next request on it cannot lose a race with a server-side close. The 250ms
// IdleTimeout #72 put here was one field for both protocols, and on this leg
// — the one client.Transport() actually speaks — it closed without the GOAWAY
// that makes an h2c teardown graceful: a caller pausing at exactly the window
// lost 193 of 400 POSTs to the race (#94). The shutdown problem the timer
// existed for moved off the data path — internal/daemon's Close skips the
// drain when nothing is active — so nothing may close a quiet caller's
// connection out from under it again, and this is the test that notices a
// timer coming back.
//
// A raw connection rather than an http.Transport, deliberately. A pool's own
// read loop notices a server-side close and quietly redials, so at most
// cadences a pooled client turns the close into a rescue and a test through
// one into a coin flip — #94 measured the band where the rescue loses at
// 48%, and either side of it at zero. Holding the socket the way the pool
// holds it, minus the rescue, makes the close deterministic to observe: with
// a timer back, the quiet wait below meets the close every run.
//
// A POST, because that is the request the race actually costs a caller:
// Go's transport replays only idempotent requests, so a pooled GET that
// meets the close is retried by itself and a POST arrives as an error.
func TestAQuietHTTP11KeepAliveCallerKeepsItsConnection(t *testing.T) {
	// The structural half first, the way the backend-side guards check the
	// server they measure ([timeShutdownAfterOneProxiedCall] and its two
	// neighbours): the shipped constructor sets no IdleTimeout at all. The
	// quiet wait below is one second, so a timer set longer than that would
	// pass the behaviour pin unnoticed; this is what says so at once (#130).
	if srv := NewHTTPServer(context.Background(), nil); srv.IdleTimeout != 0 {
		t.Fatalf("NewHTTPServer sets IdleTimeout %s: an inbound idle timer is back on the proxy "+
			"(#94), and one longer than this test's quiet wait would pass the wait below unnoticed",
			srv.IdleTimeout)
	}

	reg := registry.New()
	// The backend echoes the request body, so each assertion below is on a
	// full round trip — a diagnostic the proxy answered by itself would fail
	// on the body, not just the status.
	registered(t, reg, "payments", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, _ = w.Write(body)
	}))
	addr, callerGone := proxyServer(t, reg)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dialing the proxy: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	br := bufio.NewReader(conn)

	post := func(when string) {
		t.Helper()
		if _, err := io.WriteString(conn,
			"POST / HTTP/1.1\r\nHost: payments.internal:9777\r\nContent-Length: 3\r\n\r\nabc"); err != nil {
			t.Fatalf("writing the POST %s: %v", when, err)
		}
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("reading the response %s: %v — on a kept-alive connection this arrives only "+
				"if the proxy closed it", when, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("reading the body %s: %v", when, err)
		}
		if resp.StatusCode != http.StatusOK || string(body) != "abc" {
			t.Fatalf("the POST %s got status %d, body %q — want 200 and the echoed %q, so the "+
				"request never made the round trip", when, resp.StatusCode, body, "abc")
		}
		if resp.Close {
			t.Fatalf("the response %s carries Connection: close, so there is no keep-alive "+
				"connection here and the quiet wait watches nothing", when)
		}
	}

	// The first exchange is the setup assertion: the connection is real,
	// routed, and keep-alive. Without it the wait below would hold a socket
	// nothing serves and pass on the silence.
	post("before the quiet")

	// The quiet gap. One second is four times the window the removed timer
	// allowed, and the close that timer armed on this leg was immediate —
	// no GOAWAY, no teardown sequence to wait out — so a timer that came
	// back could not sit out this wait on scheduling either: ADR 004's worst
	// recorded scheduling sample for this project's CI host is 106.7ms.
	select {
	case <-callerGone:
		t.Fatal("the proxy closed an HTTP/1.1 keep-alive connection that had been quiet under a " +
			"second: an inbound idle timer is back (#94), and a pooled caller pausing at about " +
			"its window is back to losing non-replayable requests to the close race")
	case <-time.After(time.Second):
	}

	// The request the timer used to lose.
	post("after the quiet")
}

// --- concurrency -------------------------------------------------------------

// TestConcurrentRequestsDoNotCrossRoutes: one ReverseProxy serves every
// service, and the upstream it dials rides on the request context. Anything
// that resolved the upstream once and reused it — hoisting it into the proxy
// struct, say — would still pass every sequential test in this file while
// sending payments traffic to billing.
func TestConcurrentRequestsDoNotCrossRoutes(t *testing.T) {
	reg := registry.New()
	const services = 4
	for i := range services {
		name := fmt.Sprintf("service-%d", i)
		registered(t, reg, name, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, name)
		}))
	}

	c := client(t, serveProxy(t, reg), false)
	const rounds = 25
	// Answers land in their own slot and are read after the join: t.Fatalf
	// belongs to the test goroutine, and fetch calls it.
	type answer struct {
		want, got string
		err       error
	}
	answers := make([]answer, rounds*services)
	var wg sync.WaitGroup
	for round := range rounds {
		for i := range services {
			wg.Add(1)
			go func() {
				defer wg.Done()
				name := fmt.Sprintf("service-%d", i)
				got, err := get(c, "http://"+registry.Authority(name)+":9777/")
				answers[round*services+i] = answer{want: name, got: got, err: err}
			}()
		}
	}
	wg.Wait()

	for _, a := range answers {
		switch {
		case a.err != nil:
			t.Errorf("GET %s.internal: %v", a.want, a.err)
		case a.got != a.want:
			t.Errorf("%s.internal answered %q — the proxy crossed routes", a.want, a.got)
		}
	}
}

// get is fetch without a *testing.T, for callers that are not the test
// goroutine.
func get(c *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

// TestConcurrentChurnWhileProxying is the acceptance criterion: registration
// churn and traffic at the same time, under -race.
func TestConcurrentChurnWhileProxying(t *testing.T) {
	reg := registry.New()
	port := backend(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	if _, err := reg.Register(registry.Instance{Service: "payments", Port: port}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	c := client(t, serveProxy(t, reg), false)
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Churn: the same service restarting into the same port, over and over.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			handle, err := reg.Register(registry.Instance{Service: "payments", Port: port})
			if err != nil {
				t.Errorf("Register: %v", err)
				return
			}
			handle.Close()
		}
	}()

	// Traffic throughout. Some requests will legitimately find the name
	// absent — the churn deregisters it — so the assertion is that every
	// outcome is a *decided* one rather than a panic, a hang or a 502 from
	// routing to a port nobody holds.
	for range 60 {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
			"http://payments.internal:9777/", nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		resp, err := c.Do(req)
		if err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("request failed outright during churn: %v", err)
		}
		status := resp.StatusCode
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		switch status {
		case http.StatusOK, http.StatusServiceUnavailable:
		default:
			close(stop)
			wg.Wait()
			t.Fatalf("status %d during churn: want 200 (routed) or 503 (momentarily deregistered)", status)
		}
	}
	close(stop)
	wg.Wait()
}

// --- listener ----------------------------------------------------------------

func TestListenRefusesAnythingButLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", "192.168.1.10:0", ":0", "localhost:0"} {
		t.Run(addr, func(t *testing.T) {
			ln, err := Listen(context.Background(), addr)
			if err == nil {
				_ = ln.Close()
				t.Fatalf("Listen(%q) bound a non-loopback address; the proxy fronts every service on the machine", addr)
			}
			if !strings.Contains(err.Error(), "loopback") {
				t.Errorf("the error does not say why: %v", err)
			}
		})
	}
}

func TestListenHonoursACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ln, err := Listen(ctx, "127.0.0.1:0")
	if err == nil {
		_ = ln.Close()
		t.Fatal("Listen bound a port for an already-cancelled context, leaking it")
	}
}

// TestCORSAllowsOnlyLocalOrConfiguredOrigins is the line between a local
// frontend and any website the developer has open: only the first may read
// responses. The local rows are the control that the headers appear at all.
func TestCORSAllowsOnlyLocalOrConfiguredOrigins(t *testing.T) {
	p := &proxy{corsOrigins: map[string]bool{}}
	WithCORSOrigins([]string{"https://App.Example.test:8443"})(p)

	for origin, want := range map[string]bool{
		"http://localhost:3000":         true,
		"https://localhost":             true,
		"http://127.0.0.1:5173":         true,
		"http://[::1]:5173":             true,
		"http://app.localhost:3000":     true,
		"http://frontend.internal:9777": true,
		"HTTP://LOCALHOST:3000":         true,
		"https://app.example.test:8443": true,

		"https://evil.example":              false,
		"http://localhost.evil.example":     false,
		"http://frontend.internal.evil.com": false,
		"http://evilinternal":               false,
		"http://127.0.0.1.evil.example":     false,
		"https://app.example.test":          false,
		"https://app.example.test:8444":     false,
		"null":                              false,
		"file://":                           false,
		"chrome-extension://abc.internal":   false,
	} {
		if got := p.originAllowed(origin); got != want {
			t.Errorf("originAllowed(%q) = %v, want %v", origin, got, want)
		}
	}
}

// TestARefusedOriginGetsNoCORSHeaders is what a browser enforces on: no
// Access-Control-Allow-Origin, on a preflight or on a response, even when the
// backend sends its own.
func TestARefusedOriginGetsNoCORSHeaders(t *testing.T) {
	reg := registry.New()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
	}))
	t.Cleanup(backend.Close)
	port, err := strconv.ParseUint(backend.URL[strings.LastIndex(backend.URL, ":")+1:], 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Register(registry.Instance{Service: "payments", Port: uint32(port), PID: 1}); err != nil {
		t.Fatal(err)
	}
	h := New(reg)

	for _, tc := range []struct {
		name    string
		method  string
		preflit bool
	}{
		{"preflight", http.MethodOptions, true},
		{"request", http.MethodGet, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for origin, want := range map[string]string{
				"https://evil.example":  "",
				"http://localhost:3000": "http://localhost:3000",
			} {
				req := httptest.NewRequest(tc.method, "http://payments.internal:9777/x", nil)
				req.Header.Set("Origin", origin)
				if tc.preflit {
					req.Header.Set("Access-Control-Request-Method", "POST")
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if got := rec.Header().Get("Access-Control-Allow-Origin"); got != want {
					t.Errorf("origin %s: Allow-Origin = %q, want %q", origin, got, want)
				}
				if origin != "http://localhost:3000" && rec.Header().Get("Access-Control-Allow-Credentials") != "" {
					t.Errorf("origin %s was granted credentials", origin)
				}
			}
		})
	}
}
