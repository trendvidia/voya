package proxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// loopback is the only address voya's listeners bind, and the only address
// the proxy dials upstream. Services bind loopback too (ADR 002), so an
// upstream that is not on it is not one of ours.
const loopback = "127.0.0.1"

// ReadHeaderTimeout bounds only the request headers, never a body or a
// stream, so it cannot expire under a long-lived proxied call.
const ReadHeaderTimeout = 30 * time.Second

// Listen binds addr for the proxy, refusing anything that is not loopback.
//
// Same rule and same reason as the registration listener: ADR 002 binds every
// listener to 127.0.0.1 explicitly and never 0.0.0.0, and the bind site is
// where that stays true. It matters more here, if anything — the proxy is a
// front door to every registered service on the machine.
func Listen(ctx context.Context, addr string) (net.Listener, error) {
	if err := ctx.Err(); err != nil {
		// ListenConfig consults ctx only in the resolver and the Control
		// hook, and the IP literal insisted on below skips resolution — so
		// without this a cancelled context still binds a listener nobody is
		// left to close.
		//
		// Wrapped with the address, like the registration listener's: a
		// daemon brings all three up in one sequence and prints what it
		// gets, so a bare "context canceled" does not say which one failed.
		return nil, fmt.Errorf("proxy: not binding %s: %w", addr, err)
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("proxy: %q is not a host:port address: %w", addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf(
			"proxy: refusing to listen on %q: voya's proxy fronts every service registered on this "+
				"machine, so it binds a loopback IP literal (127.0.0.1) and nothing else — see ADR 002", addr)
	}
	var lc net.ListenConfig
	return lc.Listen(ctx, "tcp", addr)
}

// h2cProtocols is the protocol set every server on voya's proxy path serves:
// HTTP/1.1 and cleartext HTTP/2 on one port. gRPC and Connect streaming need
// HTTP/2, and curl, Postman and browsers speak HTTP/1.1. Go 1.24+ serves both
// from the standard library, which supersedes golang.org/x/net/http2/h2c
// (issue #21).
//
// Split out of [NewHTTPServer] so this package's own backendServer can be
// reached the way the proxy reaches a real service without borrowing that
// constructor, whose *timeouts* a backend must not inherit — backendServer
// records what happened the one time it did.
//
// Unexported, because that caller is in-package and the export would be
// surface with nothing behind it: test/acceptance's echoServer and
// internal/daemon's upstream each write the pair out themselves, and a second
// exported constructor beside [NewHTTPServer] is one a caller outside this
// package could reach for in place of the one that ships the proxy's timeouts.
func h2cProtocols() *http.Protocols {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	return protocols
}

// NewHTTPServer wraps handler for the traffic the proxy has to carry, on
// [h2cProtocols].
//
// Cancelling ctx ends in-flight requests, which is what lets a caller shut
// down: Shutdown alone waits for connections to go idle, and a proxied
// long-lived stream never is.
//
// No IdleTimeout, and not as an oversight (#94). The 250ms reap #72 put here
// was one field for both protocols, so it also cut HTTP/1.1 keep-alive
// connections — a leg that never held shutdown up, closed without the GOAWAY
// that made the h2c reap graceful — and a pooled caller reaching for its
// connection in that window lost the request: 193 of 400 POSTs at idle gaps
// of exactly the window, measured in PR #92's review. The shutdown problem
// the timer existed for is solved off the data path instead: internal/daemon's
// Close skips the drain outright when nothing is active, which is faster than
// the reap ever made an exit and costs no caller anything.
// TestAQuietHTTP11KeepAliveCallerKeepsItsConnection is what notices the timer
// coming back.
func NewHTTPServer(ctx context.Context, handler http.Handler) *http.Server {
	return &http.Server{
		Handler:     handler,
		Protocols:   h2cProtocols(),
		BaseContext: func(net.Listener) context.Context { return ctx },

		// ReadTimeout and WriteTimeout stay at zero deliberately. They bound
		// the whole request and the whole response, and this proxy carries
		// streams that outlive any number anyone would pick — a gRPC stream
		// open for an afternoon is a normal thing to be debugging.
		ReadHeaderTimeout: ReadHeaderTimeout,
	}
}
