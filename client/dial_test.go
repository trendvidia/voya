package voya

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// acceptAll listens on loopback and signals every connection it accepts.
// Standing in for the proxy: what matters is which socket the dialer chose,
// not what happens after.
//
// It keeps accepting rather than taking one connection: a single-shot version
// made the second subtest of a table look like a dialer failure when the
// dial had in fact succeeded.
func acceptAll(t *testing.T) (port uint32, accepted <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	got := make(chan struct{}, 16)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // the listener was closed by cleanup
			}
			select {
			case got <- struct{}{}:
			default:
			}
			_ = conn.Close()
		}
	}()
	return portOfListener(t, ln), got
}

func portOfListener(t *testing.T, ln net.Listener) uint32 {
	t.Helper()
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	n, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		t.Fatalf("ParseUint: %v", err)
	}
	return uint32(n)
}

func TestDialerSendsInternalNamesToTheProxy(t *testing.T) {
	proxyPort, accepted := acceptAll(t)
	dial := Dialer(WithProxyPort(proxyPort))

	// Every spelling of the same name. Hostnames are case-insensitive and a
	// resolver-style trailing dot names the same host, so a client library
	// that normalises differently must still reach the proxy.
	for _, host := range []string{"payments.internal", "PAYMENTS.INTERNAL", "payments.internal."} {
		t.Run(host, func(t *testing.T) {
			addr := net.JoinHostPort(host, strconv.FormatUint(uint64(proxyPort), 10))
			conn, err := dial(context.Background(), "tcp", addr)
			if err != nil {
				t.Fatalf("dial %q: %v — the name was not redirected to loopback", addr, err)
			}
			defer conn.Close()

			select {
			case <-accepted:
			case <-time.After(5 * time.Second):
				t.Fatalf("dialing %q did not reach the proxy", addr)
			}
		})
	}
}

// TestDialerPreservesTheAuthority is the mechanism ADR 002 turns on: only the
// TCP destination changes, so Host still names the service and the proxy can
// route by it. If this ever regressed the proxy would receive requests it
// could not attribute.
func TestDialerPreservesTheAuthority(t *testing.T) {
	var gotHost string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
	}))
	t.Cleanup(proxy.Close)

	proxyPort := portOfListener(t, proxy.Listener)
	client := &http.Client{Transport: Transport(WithProxyPort(proxyPort))}

	url := "http://payments.internal:" + strconv.FormatUint(uint64(proxyPort), 10) + "/v1/charge"
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	want := "payments.internal:" + strconv.FormatUint(uint64(proxyPort), 10)
	if gotHost != want {
		t.Errorf("proxy saw Host %q, want %q — the dialer rewrote the URL, not just the destination", gotHost, want)
	}
}

// TestDialerLeavesOtherTrafficAlone: databases, third-party APIs and
// everything else must dial exactly as they would with no voya in the
// process.
func TestDialerLeavesOtherTrafficAlone(t *testing.T) {
	proxyPort, proxyAccepted := acceptAll(t)
	otherPort, otherAccepted := acceptAll(t)
	dial := Dialer(WithProxyPort(proxyPort))

	addr := net.JoinHostPort("127.0.0.1", strconv.FormatUint(uint64(otherPort), 10))
	conn, err := dial(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial %q: %v", addr, err)
	}
	defer conn.Close()

	select {
	case <-otherAccepted:
	case <-time.After(5 * time.Second):
		t.Fatal("an ordinary address did not reach its destination")
	}
	select {
	case <-proxyAccepted:
		t.Fatal("an ordinary address was redirected to the proxy")
	default:
	}
}

// TestDialerIgnoresInternalOnOtherPorts documents the deliberate narrowing:
// voya's proxy speaks HTTP on one port and routes by authority, so it has no
// way to carry payments.internal:5432. Redirecting that would hand Postgres
// bytes to an HTTP proxy and produce a failure unrelated to the cause.
func TestDialerIgnoresInternalOnOtherPorts(t *testing.T) {
	proxyPort, proxyAccepted := acceptAll(t)
	dial := Dialer(WithProxyPort(proxyPort))

	other := strconv.FormatUint(uint64(proxyPort)+1, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := dial(ctx, "tcp", "payments.internal:"+other)

	// The load-bearing assertion, and the only one that is about the dialer:
	// whatever became of that address, it was not handed to voya.
	select {
	case <-proxyAccepted:
		t.Fatal("a non-proxy port was redirected to the proxy")
	default:
	}

	if err == nil {
		_ = conn.Close()
		// A machine with ADR 002's /etc/resolver/internal entry installed
		// (`voya dns install` writes it) answers every
		// *.internal query with 127.0.0.1, so this address can
		// genuinely connect to whatever holds that port — which dial.go's
		// own comment anticipates ("or reaches the real thing, if DNS is
		// installed and something is there"). The dialer still did its job;
		// there is simply nothing left for this test to prove here.
		t.Skip("*.internal resolves on this machine, so the address connected without the dialer's help")
	}
	// The failure should be resolution failing, which is the honest outcome
	// for a name nothing serves.
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) {
		t.Logf("dial failed with %v (not a DNS error, which is fine as long as it did not reach the proxy)", err)
	}
}

func TestDialerRejectsMalformedAddresses(t *testing.T) {
	dial := Dialer()
	if conn, err := dial(context.Background(), "tcp", "no-port-here"); err == nil {
		_ = conn.Close()
		t.Fatal("a malformed address dialed successfully")
	}
}

// TestTransportKeepsDefaultTransportSettings: Transport clones
// http.DefaultTransport rather than building a bare one, so a service does
// not silently lose proxy settings, connection limits or timeouts by calling
// Join.
func TestTransportKeepsDefaultTransportSettings(t *testing.T) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Skip("http.DefaultTransport has been replaced in this binary")
	}
	got := Transport()
	if got.MaxIdleConns != base.MaxIdleConns {
		t.Errorf("MaxIdleConns = %d, want the default %d", got.MaxIdleConns, base.MaxIdleConns)
	}
	if got.IdleConnTimeout != base.IdleConnTimeout {
		t.Errorf("IdleConnTimeout = %v, want the default %v", got.IdleConnTimeout, base.IdleConnTimeout)
	}
	if got.DialContext == nil {
		t.Error("the clone has no DialContext, so *.internal would not be routed")
	}
	if got.Proxy == nil {
		t.Error("the clone has no Proxy, so the service's HTTP_PROXY configuration was dropped")
	}
	// DialContext is the one field the clone overwrites, so it is the one
	// place a default can be lost without the checks above noticing. These
	// are http.DefaultTransport's own values (net/http's transport.go); a
	// zero net.Dialer here would mean no dial timeout at all on every
	// outbound connection in a process that called Join.
	const stdlib = 30 * time.Second
	if netDialer.Timeout != stdlib {
		t.Errorf("voya's dialer has Timeout %v, want http.DefaultTransport's %v", netDialer.Timeout, stdlib)
	}
	if netDialer.KeepAlive != stdlib {
		t.Errorf("voya's dialer has KeepAlive %v, want http.DefaultTransport's %v", netDialer.KeepAlive, stdlib)
	}
}

// TestDialerAndTransportAreNoOpsOnFly is the other half of
// TestJoinIsANoOpOnFly, and the half that was missing: Join is not the only
// entry point. The README tells a service that builds its own http.Client to
// call Transport directly, and ADR 002 says the dialer is a no-op when
// FLY_APP_NAME is set. Redirecting there would send every Machine's
// service-to-service traffic to its own loopback instead of the 6PN peer.
func TestDialerAndTransportAreNoOpsOnFly(t *testing.T) {
	onFly := withFlyAppName(func() string { return "payments" })
	offFly := withFlyAppName(func() string { return "" })

	// The decision itself, rather than a dial: a machine with
	// /etc/resolver/internal installed resolves *.internal to 127.0.0.1 on
	// its own, so a connection reaching loopback would prove nothing.
	o := resolve([]Option{WithProxyPort(9777), onFly})
	if target, ok := o.redirect("payments.internal:9777"); ok {
		t.Errorf("redirect sent payments.internal to %q with %s set; 6PN already resolves it", target, FlyAppEnv)
	}

	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Skip("http.DefaultTransport has been replaced in this binary")
	}
	fly := Transport(onFly)
	if funcAddr(fly.DialContext) != funcAddr(base.DialContext) {
		t.Errorf("Transport replaced DialContext with %s set", FlyAppEnv)
	}
	if funcAddr(fly.Proxy) != funcAddr(base.Proxy) {
		t.Errorf("Transport replaced Proxy with %s set", FlyAppEnv)
	}

	// And the control: off Fly it must still do both, or this test would
	// pass just as well against a Transport that never routes anything.
	dev := Transport(offFly)
	if funcAddr(dev.DialContext) == funcAddr(base.DialContext) {
		t.Error("Transport did not install the *.internal dialer off Fly")
	}
	if funcAddr(dev.Proxy) == funcAddr(base.Proxy) {
		t.Error("Transport did not wrap Proxy off Fly, so HTTP_PROXY would swallow *.internal")
	}
}

// funcAddr identifies which function a field holds. Enough to tell voya's
// from the standard library's, which is all these tests ask.
func funcAddr(fn any) uintptr {
	if fn == nil {
		return 0
	}
	return reflect.ValueOf(fn).Pointer()
}

// TestTransportDoesNotSendInternalNamesThroughAnHTTPProxy: http.Transport
// consults Proxy before DialContext, so on a machine with HTTP_PROXY set the
// redirect never gets a say — the request goes to the corporate proxy with
// an absolute-form URI for a name it cannot resolve, and every service on
// that machine fails with an error naming the proxy.
func TestTransportDoesNotSendInternalNamesThroughAnHTTPProxy(t *testing.T) {
	var voyaHost string
	voyaProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		voyaHost = r.Host
	}))
	t.Cleanup(voyaProxy.Close)
	proxyPort := portOfListener(t, voyaProxy.Listener)

	var httpProxy atomicCounter
	corp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpProxy.inc()
	}))
	t.Cleanup(corp.Close)
	corpURL, err := url.Parse(corp.URL)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// A stub in place of ProxyFromEnvironment: http.ProxyFromEnvironment
	// reads the environment once per process behind a sync.Once, so
	// t.Setenv("HTTP_PROXY") after any earlier use would set nothing.
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Skip("http.DefaultTransport has been replaced in this binary")
	}
	var consulted asked
	withProxy := base.Clone()
	withProxy.Proxy = func(req *http.Request) (*url.URL, error) {
		consulted.add(req.URL.Host)
		return corpURL, nil
	}
	saved := http.DefaultTransport
	http.DefaultTransport = withProxy
	t.Cleanup(func() { http.DefaultTransport = saved })

	client := &http.Client{Transport: Transport(WithProxyPort(proxyPort))}
	port := strconv.FormatUint(uint64(proxyPort), 10)

	resp, err := client.Get("http://payments.internal:" + port + "/v1/charge")
	if err != nil {
		t.Fatalf("GET payments.internal: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if n := httpProxy.get(); n != 0 {
		t.Errorf("the HTTP proxy served %d requests; *.internal must go straight to voya", n)
	}
	if consulted.has("payments.internal:" + port) {
		t.Error("the proxy function was consulted for a *.internal name it would have hijacked")
	}
	if want := "payments.internal:" + port; voyaHost != want {
		t.Errorf("voya's proxy saw Host %q, want %q", voyaHost, want)
	}

	// The control: everything else still obeys the service's own proxy
	// configuration, which is the setting this must not quietly disable.
	resp, err = client.Get("http://api.example.com:" + port + "/v1/charges")
	if err != nil {
		t.Fatalf("GET api.example.com: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if n := httpProxy.get(); n != 1 {
		t.Errorf("the HTTP proxy served %d non-internal requests, want 1 — voya disabled a proxy it does not own", n)
	}
}

// recorded is an ordered, concurrency-safe log of strings a test needs to
// assert on — hosts a proxy function was consulted for, lines voya wrote.
// The transport
// calls it on its own goroutine.
type asked struct {
	mu    sync.Mutex
	lines []string
}

func (a *asked) add(host string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lines = append(a.lines, host)
}

func (a *asked) has(host string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Contains(a.lines, host)
}

func (a *asked) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.lines)
}

func (a *asked) last() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.lines) == 0 {
		return ""
	}
	return a.lines[len(a.lines)-1]
}

func TestRedirectDecision(t *testing.T) {
	o := resolve([]Option{WithProxyPort(9777)})
	cases := []struct {
		addr string
		want bool
	}{
		{"payments.internal:9777", true},
		{"PAYMENTS.INTERNAL:9777", true},
		{"payments.internal.:9777", true},
		{"voya.internal:9777", true}, // ADR 005's diagnostics authority
		{"payments.internal:5432", false},
		{"payments.local:9777", false},
		{"127.0.0.1:9777", false},
		{"api.example.com:9777", false},
		{"internal:9777", false}, // the suffix is not a name on its own
		{"malformed", false},
	}
	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			target, got := o.redirect(tc.addr)
			if got != tc.want {
				t.Fatalf("redirect(%q) = %v, want %v", tc.addr, got, tc.want)
			}
			if got && !strings.HasPrefix(target, proxyHost+":") {
				t.Errorf("redirect target = %q, want a %s address", target, proxyHost)
			}
		})
	}
}
