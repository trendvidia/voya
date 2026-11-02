package registration

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Keepalive settings for the registration connection.
//
// Generous on purpose, and this is the one number in the package that is a
// judgement call rather than a mechanism. ADR 004 chose a stream over active
// probing partly because a service paused on a debugger breakpoint keeps its
// socket open and survives — but noted the limit: Delve pauses the transport
// goroutines that answer HTTP/2 PINGs, so an aggressive keepalive re-creates
// the debugger-eviction problem that killed probing in the first place.
//
// At these values a paused process has to stay paused for over two minutes
// before voya gives up on it, while a machine that went away is still noticed
// within about the same window. Stepping through a handler fits comfortably;
// lunch does not, and should not.
const (
	// PingIdle is how long the connection may be silent before voya pings.
	// The registration stream is silent by design — one message, then
	// nothing for hours — so this fires constantly and must not be cheap
	// enough to matter or short enough to be jumpy.
	PingIdle = 60 * time.Second

	// PingTimeout is how long an unanswered ping is tolerated before the
	// connection is considered dead. This is the backstop for the socket
	// that outlives its process: a machine that was killed hard, or a
	// connection wedged somewhere in between.
	PingTimeout = 60 * time.Second

	// ReadHeaderTimeout bounds only the request headers, never the stream
	// body, so it cannot expire under a healthy long-lived stream.
	ReadHeaderTimeout = 30 * time.Second
)

// Listen binds addr for the registration listener, refusing anything that is
// not loopback.
//
// ADR 002 accepts that registration is unauthenticated and trust-on-first-use
// — any local process may claim a service name — and bounds that by binding
// 127.0.0.1 explicitly and never 0.0.0.0. Since the binding *is* the security
// boundary, this refuses to be the place it gets widened by a typo or by a
// config field somebody adds later.
func Listen(ctx context.Context, addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("registration: %q is not a host:port address: %w", addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf(
			"registration: refusing to listen on %q: the registration stream is unauthenticated, "+
				"so it binds a loopback IP literal (127.0.0.1) and nothing else — see ADR 002", addr)
	}
	// Checked explicitly, because ListenConfig does not do it: ctx reaches
	// only the resolver and the Control hook, and an IP literal — which the
	// guard above just insisted on — skips resolution entirely, so binding a
	// loopback address with an already-cancelled context otherwise succeeds
	// and hands back a listener nobody is left to close. A caller that
	// cancelled during startup would leak port 9779 until the process exits,
	// and the next `voya run` would fail to bind it.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("registration: not binding %s: %w", addr, err)
	}
	var lc net.ListenConfig
	return lc.Listen(ctx, "tcp", addr)
}

// NewHTTPServer wraps s in an http.Server configured for the stream it has to
// carry.
//
// Connect's bidirectional streaming needs HTTP/2; over cleartext that means
// prior-knowledge h2c, which Go 1.24+ serves from the standard library rather
// than golang.org/x/net/http2/h2c (issue #21). HTTP/1.1 stays enabled so that
// a human poking at the port with curl gets a protocol-level explanation
// instead of a connection that dies without saying anything.
//
// ctx is how the daemon ends the streams, and it is not optional. Every
// registration handler blocks until something tells it to stop, so an
// http.Server holding them has no idle connection for Shutdown to wait for:
// called on its own, Shutdown blocks for the whole grace period and then
// returns with every service still registered and every stream still open.
// Cancelling ctx ends each handler through the same path a lost connection
// takes — the registry entry goes, the client sees a clean end and reconnects
// — after which Shutdown returns promptly and the grace period is spent on
// what it is for. Shut down in that order:
//
//	cancel()             // evicts every service, ends every stream
//	srv.Shutdown(grace)  // drains, then returns
func NewHTTPServer(ctx context.Context, s *Server) *http.Server {
	_, handler := s.Handler()

	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)

	return &http.Server{
		// No mux: the generated handler already dispatches on the request
		// path and answers 404 for everything else, so one wrapped in a mux
		// mounted at its own prefix behaves identically for every path.
		Handler:     handler,
		Protocols:   protocols,
		BaseContext: func(net.Listener) context.Context { return ctx },

		// ReadTimeout and WriteTimeout are deliberately left at zero. They
		// bound the whole request and the whole response, and this request
		// lasts as long as the service does — setting either would evict
		// every service on a timer, which is the polling design ADR 004
		// rejected, arrived at by accident.
		ReadHeaderTimeout: ReadHeaderTimeout,

		HTTP2: &http.HTTP2Config{
			SendPingTimeout: PingIdle,
			PingTimeout:     PingTimeout,
		},
	}
}
