package voya

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Dialer returns a DialContext that sends *.internal addresses on the proxy
// port to the local proxy, and leaves every other address alone.
//
// On Fly it redirects nothing: [FlyAppEnv] is set, each Machine has its own
// 6PN address, and *.internal already resolves to the peer rather than to
// loopback. The returned function stays usable so that a service installs it
// the same way in both environments (ADR 002).
//
// The URL is never touched, which is the whole mechanism: Host and
// :authority still carry the service name, so the proxy can route by it and
// application code keeps one dial string for both environments (ADR 002).
// Only the TCP destination changes.
//
// # What is and is not captured
//
// Captured: a host ending in .internal whose port is the proxy port. That is
// the fleet's dial string, so it is the case that has to work.
//
// Not captured, and this is deliberate: a .internal host on any other port.
// voya's proxy speaks HTTP on one port and routes by authority; it has no
// way to carry, say, payments.internal:5432. Rewriting that would hand
// Postgres bytes to an HTTP proxy and produce a failure with no relationship
// to the cause, where leaving it alone produces an honest "no such host"
// (or reaches the real thing, if DNS is installed and something is there).
//
// Also not captured: everything that is not .internal. Databases, third-party
// APIs and metrics endpoints dial exactly as they would without voya.
//
// Note that .internal is captured wholesale within that port — voya assumes
// every *.internal name on the proxy port is a fleet service. If a genuinely
// external .internal name ever matters, this is the function that has to
// learn the difference.
func Dialer(opts ...Option) func(ctx context.Context, network, addr string) (net.Conn, error) {
	o := resolve(opts)
	return o.dialContext
}

func (o *options) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if target, ok := o.redirect(addr); ok {
		return netDialer.DialContext(ctx, network, target)
	}
	return netDialer.DialContext(ctx, network, addr)
}

// netDialer carries the same bounds http.DefaultTransport's own dialer does
// (net/http's transport.go). [Transport] replaces DialContext wholesale and
// this one dials every address, not only the redirected ones — so a zero
// net.Dialer here would quietly trade the standard library's 30s dial
// timeout and TCP keepalive for none, on every outbound connection in a
// process that called Join. Losing a timeout by installing a dialer that was
// supposed to be transparent is exactly the kind of surprise this package
// must not hand a service.
var netDialer = &net.Dialer{
	Timeout:   30 * time.Second,
	KeepAlive: 30 * time.Second,
}

// redirect reports the loopback address a dial should go to instead, and
// whether it should be redirected at all.
func (o *options) redirect(addr string) (string, bool) {
	if o.onFly() {
		// Production. 6PN resolves *.internal to the Machine that serves it,
		// so redirecting to loopback here would break traffic that works.
		return "", false
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", false
	}
	if port != strconv.FormatUint(uint64(o.proxyPort), 10) {
		return "", false
	}
	// Hostnames are case-insensitive, and a resolver-style trailing dot
	// names the same host.
	host = strings.TrimSuffix(host, ".")
	if !strings.HasSuffix(strings.ToLower(host), Suffix) {
		return "", false
	}
	return net.JoinHostPort(proxyHost, port), true
}

// proxyHost is where voya listens. Loopback and not configurable: ADR 002
// binds all three listeners to 127.0.0.1 explicitly and never 0.0.0.0, and a
// dialer that could be pointed elsewhere would make that boundary moot from
// the other side.
const proxyHost = "127.0.0.1"

// Transport returns a clone of [http.DefaultTransport] that routes *.internal
// through the local proxy. Prefer [RoundTripper] for services that build
// their own clients: it is this transport plus the client half of ADR 006's
// rate-limit contract, which is what [Join] installs on
// [http.DefaultTransport]. This one is the bare *http.Transport — reach for
// it only when something needs the concrete type to configure, and a 429
// will reach that caller unhandled (#224).
//
// On Fly it is the clone and nothing else — same defaults, no redirect — so
// this is the one call a service makes in both environments.
func Transport(opts ...Option) *http.Transport {
	o := resolve(opts)
	return o.transport()
}

func (o *options) transport() *http.Transport {
	base, isTransport := http.DefaultTransport.(*http.Transport)
	if !isTransport {
		// Join has already run and installed this package's own 429 wrapper
		// (#224). Its base is the clone whose settings a second Transport or
		// RoundTripper call should still inherit — without this, that call
		// would log the "two opinions about outbound HTTP" warning below
		// about a transport this very package installed.
		if tt, ok := http.DefaultTransport.(*throttleTransport); ok {
			base, isTransport = tt.base.(*http.Transport)
		}
	}

	if o.onFly() {
		// Production: *.internal resolves over 6PN and there is nothing to
		// route. Hand back the defaults untouched rather than a transport
		// that dials loopback for the fleet's own names.
		if isTransport {
			return base.Clone()
		}
		return &http.Transport{}
	}

	if !isTransport {
		// Something already replaced DefaultTransport with a different
		// implementation. Build a plain one rather than silently dropping
		// whatever it configured — and say so, since it means this
		// process has two opinions about outbound HTTP.
		o.logf("voya: http.DefaultTransport is %T, not *http.Transport; using a fresh transport for *.internal", http.DefaultTransport)
		return &http.Transport{DialContext: o.dialContext}
	}
	clone := base.Clone()
	clone.DialContext = o.dialContext
	clone.Proxy = o.proxyFor(clone.Proxy)
	return clone
}

// proxyFor wraps a transport's proxy function so that the names this package
// redirects never go through an HTTP proxy.
//
// http.DefaultTransport carries Proxy: ProxyFromEnvironment, and the proxy
// decision happens before DialContext: with HTTP_PROXY set and .internal
// absent from NO_PROXY — the default on a machine behind a corporate proxy —
// the transport would dial the proxy instead, hand it an absolute-form URI
// for a name it cannot resolve, and the redirect below would never run. The
// whole fleet on that machine would fail with an error naming the proxy.
//
// Only addresses this package would have redirected are exempted, so a
// service's real proxy configuration still governs everything else.
func (o *options) proxyFor(base func(*http.Request) (*url.URL, error)) func(*http.Request) (*url.URL, error) {
	if base == nil {
		return nil
	}
	return func(req *http.Request) (*url.URL, error) {
		if _, ok := o.redirect(req.URL.Host); ok {
			return nil, nil // no proxy: this one is going to loopback
		}
		return base(req)
	}
}

// installed guards http.DefaultTransport against being patched twice.
var installed struct {
	sync.Mutex
	done bool

	// proxyPort is what the install that won routes to, kept so that a later
	// Join asking for a different one is told it did not get it.
	proxyPort uint32
}

// installDialer points http.DefaultTransport at the *.internal dialer, so
// that http.DefaultClient — and every client that leaves Transport nil —
// reaches fleet services without the service knowing voya exists.
//
// This mutates process-global state, which is why it happens in Join, at
// startup, before the service serves anything. Doing it later races every
// in-flight request.
func installDialer(o *options) {
	installed.Lock()
	defer installed.Unlock()
	if installed.done {
		if installed.proxyPort != o.proxyPort {
			// Two services in one process asking for different proxy ports.
			// The first install stands — replacing it would race whatever is
			// already in flight through it — but silence here would leave
			// the second service's outbound *.internal traffic going
			// somewhere it did not ask for, with nothing to read.
			o.logf("voya: http.DefaultTransport already routes *.internal to port %d, so this call's port %d applies only to transports from voya.Transport()",
				installed.proxyPort, o.proxyPort)
		}
		return
	}
	// The wrapper, not the bare transport: http.DefaultClient is how most
	// fleet services call each other, and ADR 006 puts the client half of
	// the rate-limit contract — wait out the Retry-After on a 429 voya's own
	// limiter marked, and only those (#241) — on exactly that path (#224).
	// [RoundTripper] documents what it does and its bound.
	http.DefaultTransport = o.roundTripper()
	installed.done = true
	installed.proxyPort = o.proxyPort
}
