// Package trailertest answers one question with evidence: does the gRPC wire
// protocol survive voya's proxy?
//
// gRPC carries its real outcome in HTTP/2 trailers (`grpc-status`), not in the
// HTTP status, which is always 200. A proxy that drops trailers turns errors
// into transport failures or hangs, and the happy path keeps working — so the
// failure is silent until something goes wrong.
//
// Each case runs twice: DIRECT against the service, and PROXIED through
// voya's own proxy. The direct leg is the control. If direct passes and
// proxied fails, the proxy is at fault and nothing else can be.
//
// The client is grpc-go itself, which is this harness's distinct contribution:
// internal/proxy covers the same wire format with connect-go's gRPC client, so
// what runs here — and nowhere else — is the implementation every non-Go
// consumer will actually use (grpcurl and friends).
//
// No codegen: the service is built from well-known types, and the client uses
// grpc-go's generic Invoke/NewStream. Run with `go test -v ./...`.
package trailertest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/trendvidia/voya/internal/proxy"
	"github.com/trendvidia/voya/internal/registry"
)

// serviceName is what the backend registers with voya, and proxyAuthority is
// what a caller dials. voya routes on the hostname and ignores the port, but
// a real caller sends the one its dialer used (ADR 002), so this carries it.
const serviceName = "probe"

var proxyAuthority = registry.Authority(serviceName) + ":9777"

const (
	procFail     = "/trailer.test.v1.Probe/Fail"
	procOK       = "/trailer.test.v1.Probe/OK"
	procStream   = "/trailer.test.v1.Probe/Stream"
	procFailFast = "/trailer.test.v1.Probe/FailFast"
)

// backend is a connect-go service. connect-go serves Connect, gRPC and
// gRPC-Web on the same handler, so a native gRPC client exercises the
// trailer path.
func backend() http.Handler {
	mux := http.NewServeMux()

	mux.Handle(procFail, connect.NewUnaryHandler(procFail,
		func(context.Context, *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
			// Written to only after a response would normally begin, so the
			// status must ride in trailers.
			return nil, connect.NewError(connect.CodeNotFound, errors.New("no such order"))
		}))

	mux.Handle(procOK, connect.NewUnaryHandler(procOK,
		func(context.Context, *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
			return connect.NewResponse(&emptypb.Empty{}), nil
		}))

	mux.Handle(procStream, connect.NewServerStreamHandler(procStream,
		func(ctx context.Context, _ *connect.Request[emptypb.Empty], s *connect.ServerStream[emptypb.Empty]) error {
			for range 3 {
				if err := s.Send(&emptypb.Empty{}); err != nil {
					return err
				}
			}
			return nil
		}))

	// Fails before any message is sent. This is the Trailers-Only shape:
	// a single HEADERS frame with END_STREAM and no body at all.
	mux.Handle(procFailFast, connect.NewUnaryHandler(procFailFast,
		func(context.Context, *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
			return nil, connect.NewError(connect.CodePermissionDenied, errors.New("denied before body"))
		}))

	return mux
}

// serveH2C starts a service on an ephemeral loopback port, speaking HTTP/1.1
// and cleartext HTTP/2 — the shape every service in the fleet has under ADR
// 002. gRPC clients speak HTTP/2 with prior knowledge, which is what
// UnencryptedHTTP2 accepts: from the standard library since Go 1.24, which
// supersedes golang.org/x/net/http2/h2c (issue #21).
func serveH2C(t *testing.T, h http.Handler) string {
	t.Helper()
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)

	srv := httptest.NewUnstartedServer(h)
	srv.Config.Protocols = protocols
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// voyaProxy is voya's routing handler with backendAddr registered under
// serviceName — the component under test. Returned as a handler rather than an
// address so that [proxyInFront] can wrap it for the negative control.
//
// Built with proxy.WithErrorLog, which is how the daemon builds it
// (internal/daemon): the default discards the proxy's own transport errors,
// and a harness attesting to "what ships" should not be running the one
// configuration that throws that signal away.
func voyaProxy(t *testing.T, backendAddr string, logf func(string, ...any)) http.Handler {
	t.Helper()
	reg := registry.New()
	if _, err := reg.Register(registry.Instance{Service: serviceName, Port: portOf(t, backendAddr)}); err != nil {
		t.Fatalf("Register %s on %s: %v", serviceName, backendAddr, err)
	}
	return proxy.New(reg, proxy.WithErrorLog(logf))
}

// proxyErrors collects what voya reports about its own failures, from
// whichever goroutine is serving — hence the lock.
//
// voya's ErrorHandler is not an option a caller can replace, as the earlier
// httputil harness's was, so a transport failure becomes an ADR 005 diagnostic
// written into the response and reaches the assertions below as a missing or
// wrong grpc-status. [proxy.WithErrorLog] is the hook that survives, and it
// restores the explicit report the deleted ErrorHandler made: a proxy that
// could not reach the backend says so in the test output instead of being
// inferred from a puzzling status code.
type proxyErrors struct {
	mu   sync.Mutex
	msgs []string
}

func (p *proxyErrors) logf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msgs = append(p.msgs, fmt.Sprintf(format, args...))
}

func (p *proxyErrors) drain() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.msgs
	p.msgs = nil
	return out
}

// proxyInFront puts voya between client and backend, both legs cleartext
// HTTP/2, through the same Listen and NewHTTPServer the daemon uses — so what
// these four cases attest to is what ships, not a re-creation of it.
//
// wrap, when non-nil, goes around voya's handler. Only the negative control
// passes one, and it passes it here rather than standing up a server of its
// own so that "the path being sabotaged is the path under test" covers the
// server too, and not merely the handler.
func proxyInFront(t *testing.T, backendAddr string, wrap func(http.Handler) http.Handler) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ln, err := proxy.Listen(ctx, "127.0.0.1:0")
	if err != nil {
		cancel()
		t.Fatalf("proxy.Listen: %v", err)
	}
	// Registered before anything that can t.Fatalf — voyaProxy does, on a bad
	// name or an unparseable port. t.Fatalf is a Goexit, so a cleanup
	// registered after it would never run and the listener would stay bound
	// for the rest of the test binary's life.
	t.Cleanup(func() { cancel(); _ = ln.Close() })

	var errs proxyErrors
	handler := voyaProxy(t, backendAddr, errs.logf)
	if wrap != nil {
		handler = wrap(handler)
	}
	srv := proxy.NewHTTPServer(ctx, handler)

	// Serve's error goes to a channel rather than straight to t.Errorf: this
	// goroutine outlives the test body, and reporting from it after the test
	// completes panics instead of failing.
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	t.Cleanup(func() {
		// Drained before the shutdown below, not after: closing a server with
		// a connection still open is itself something httputil reports, and
		// that message would be about the teardown rather than about the
		// proxy.
		for _, msg := range errs.drain() {
			t.Errorf("proxy reported: %s", msg)
		}
		_ = srv.Close()
		if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("proxy Serve: %v", err)
		}
	})
	return ln.Addr().String()
}

// portOf is the port a service actually bound after asking for 0, which is
// what it registers (ADR 002).
func portOf(t *testing.T, addr string) uint32 {
	t.Helper()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", addr, err)
	}
	// The host is dropped on purpose — a service registers a port and nothing
	// else — so this is the one place that can notice it was never voya's.
	// voya dials every upstream on 127.0.0.1 (ADR 002), and httptest falls
	// back to [::1] on a host with no IPv4 loopback; the port alone would then
	// name nothing, every proxied leg would fail with voya's 502 and every
	// direct leg would pass, which is exactly the signal this harness reserves
	// for "the proxy is at fault and nothing else can be".
	if host != "127.0.0.1" {
		t.Fatalf("the backend bound %s, but voya dials upstreams on 127.0.0.1 only — "+
			"registering its port alone would send every proxied leg nowhere", addr)
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		t.Fatalf("ParseUint(%q): %v", port, err)
	}
	return uint32(n)
}

// dial opens a gRPC connection to addr. Callers going through voya add
// grpc.WithAuthority: voya routes on :authority, so the name selects the
// backend and the address only decides which door the bytes arrive at.
func dial(t *testing.T, addr string, opts ...grpc.DialOption) *grpc.ClientConn {
	t.Helper()
	cc, err := grpc.NewClient(addr,
		append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, opts...)...)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return cc
}

// bothPaths runs fn against the backend directly and through voya.
// The direct leg is the control: if it fails, the problem is not the proxy.
func bothPaths(t *testing.T, fn func(t *testing.T, cc *grpc.ClientConn)) {
	t.Helper()
	backendAddr := serveH2C(t, backend())
	proxyAddr := proxyInFront(t, backendAddr, nil)

	t.Run("direct", func(t *testing.T) { fn(t, dial(t, backendAddr)) })
	t.Run("proxied", func(t *testing.T) { fn(t, dial(t, proxyAddr, grpc.WithAuthority(proxyAuthority))) })
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}

// Case 1: an error code must survive the hop.
//
// This is the one that matters most. If trailers are dropped, the client sees
// a stream that ended without a status and reports Internal or Unavailable —
// so a NOT_FOUND reads as a broken service rather than a missing record.
func TestUnaryErrorCodeSurvives(t *testing.T) {
	bothPaths(t, func(t *testing.T, cc *grpc.ClientConn) {
		err := cc.Invoke(ctx(t), procFail, &emptypb.Empty{}, &emptypb.Empty{})
		if err == nil {
			t.Fatal("want NotFound, got nil error — the failure was swallowed entirely")
		}
		st := status.Convert(err)
		if st.Code() != codes.NotFound {
			t.Fatalf("want code NotFound, got %v (%q) — trailers likely lost", st.Code(), st.Message())
		}
		if st.Message() != "no such order" {
			t.Errorf("want message %q, got %q", "no such order", st.Message())
		}
	})
}

// Case 2: success must terminate cleanly.
//
// Even an OK call needs `grpc-status: 0` in the trailers. Drop it and the
// client waits — this test fails by timeout rather than by assertion.
func TestUnarySuccessCompletes(t *testing.T) {
	bothPaths(t, func(t *testing.T, cc *grpc.ClientConn) {
		if err := cc.Invoke(ctx(t), procOK, &emptypb.Empty{}, &emptypb.Empty{}); err != nil {
			t.Fatalf("want OK, got %v (code %v)", err, status.Code(err))
		}
	})
}

// Case 3: server streaming must deliver every message and then a clean status.
//
// io.EOF from RecvMsg means the stream ended with OK, which is carried in
// trailers after the final DATA frame.
func TestServerStreamCompletes(t *testing.T) {
	bothPaths(t, func(t *testing.T, cc *grpc.ClientConn) {
		cs, err := cc.NewStream(ctx(t), &grpc.StreamDesc{StreamName: "Stream", ServerStreams: true}, procStream)
		if err != nil {
			t.Fatalf("NewStream: %v", err)
		}
		if err := cs.SendMsg(&emptypb.Empty{}); err != nil {
			t.Fatalf("SendMsg: %v", err)
		}
		if err := cs.CloseSend(); err != nil {
			t.Fatalf("CloseSend: %v", err)
		}
		got := 0
		for {
			var out emptypb.Empty
			err := cs.RecvMsg(&out)
			if errors.Is(err, io.EOF) {
				break // clean end == trailers arrived with grpc-status 0
			}
			if err != nil {
				t.Fatalf("RecvMsg after %d message(s): %v (code %v)", got, err, status.Code(err))
			}
			got++
		}
		if got != 3 {
			t.Errorf("want 3 messages, got %d", got)
		}
	})
}

// Case 4: Trailers-Only — failure before any message is written.
//
// The response is a single HEADERS frame with END_STREAM carrying
// `grpc-status`, and no body. A proxy that assumes a body exists before
// trailers can mishandle this shape specifically.
func TestTrailersOnlyError(t *testing.T) {
	bothPaths(t, func(t *testing.T, cc *grpc.ClientConn) {
		err := cc.Invoke(ctx(t), procFailFast, &emptypb.Empty{}, &emptypb.Empty{})
		if err == nil {
			t.Fatal("want PermissionDenied, got nil error")
		}
		if got := status.Code(err); got != codes.PermissionDenied {
			t.Fatalf("want PermissionDenied, got %v (%q)", got, status.Convert(err).Message())
		}
	})
}

// Case 5: a Trailers-Only response voya *originates* rather than forwards.
//
// An authority nothing registered never reaches a backend, so the outcome is
// voya's to report — and to a grpc-go caller it used to arrive as
// `Unimplemented: unexpected HTTP status code 404`, with the diagnostic
// buried in an escaped `data:` blob (#25). What this attests, with the
// implementation grpcurl and every non-Go consumer speaks: the code is
// Unavailable, and the multi-line diagnostic survives grpc-message's
// percent-encoding byte for byte.
//
// No direct leg: there is no voya to misroute on the direct path, so the
// control is instead that the registered name still answers through the same
// proxy — the failure under test is routing, not the harness.
func TestUnknownAuthorityDiagnosticIsTrailersOnly(t *testing.T) {
	backendAddr := serveH2C(t, backend())
	proxyAddr := proxyInFront(t, backendAddr, nil)

	typo := dial(t, proxyAddr, grpc.WithAuthority("prboe.internal:9777"))
	err := typo.Invoke(ctx(t), procOK, &emptypb.Empty{}, &emptypb.Empty{})
	if err == nil {
		t.Fatal("a call to an unregistered authority succeeded")
	}
	st := status.Convert(err)
	t.Logf("grpc-go caller sees: %v: %s", st.Code(), st.Message())
	if st.Code() != codes.Unavailable {
		t.Fatalf("want Unavailable, got %v (%q) — the status did not arrive in trailers", st.Code(), st.Message())
	}
	for _, want := range []string{
		`voya: no service named "prboe"`,
		"Nothing has ever registered that name.",
		`Authority received: "prboe.internal:9777"`,
		"Registered services: probe.internal",
	} {
		if !strings.Contains(st.Message(), want) {
			t.Errorf("the diagnostic omits %q:\n%s", want, st.Message())
		}
	}
	if !strings.Contains(st.Message(), "\n") {
		t.Errorf("the newlines did not survive percent-encoding:\n%s", st.Message())
	}

	known := dial(t, proxyAddr, grpc.WithAuthority(proxyAuthority))
	if err := known.Invoke(ctx(t), procOK, &emptypb.Empty{}, &emptypb.Empty{}); err != nil {
		t.Errorf("the registered authority failed through the same proxy: %v", err)
	}
}
