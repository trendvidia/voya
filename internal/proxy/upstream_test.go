package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trendvidia/voya/internal/registry"
)

// serveOn serves handler on ln until the test ends, in the protocols given.
func serveOn(t *testing.T, ln net.Listener, protocols *http.Protocols, handler http.Handler) *http.Server {
	t.Helper()
	srv := &http.Server{Handler: handler, Protocols: protocols, ReadHeaderTimeout: ReadHeaderTimeout}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("backend Serve: %v", err)
		}
	})
	return srv
}

func listenLoopback(t *testing.T) net.Listener {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	return ln
}

func http1Only() *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	return p
}

func h2cOnly() *http.Protocols {
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	return p
}

func registerPort(t *testing.T, reg *registry.Registry, name string, ln net.Listener) {
	t.Helper()
	if _, err := reg.Register(registry.Instance{Service: name, Port: portOf(t, ln)}); err != nil {
		t.Fatalf("Register: %v", err)
	}
}

// reportProto answers with the protocol and authority the backend saw.
var reportProto = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "%s %s", r.Proto, r.Host)
})

func TestEachUpstreamIsSpokenToInItsOwnProtocol(t *testing.T) {
	reg := registry.New()
	h1, h2 := listenLoopback(t), listenLoopback(t)
	serveOn(t, h1, http1Only(), reportProto)
	serveOn(t, h2, h2cOnly(), reportProto)
	registerPort(t, reg, "bucket", h1)
	registerPort(t, reg, "api", h2)
	proxyAddr := serveProxy(t, reg)

	// Both caller protocols, because the proxy bridges them: an h2c caller
	// must reach an HTTP/1.1 upstream too.
	for _, h2Only := range []bool{false, true} {
		c := client(t, proxyAddr, h2Only)
		for _, tc := range []struct{ name, authority, want string }{
			{"bucket", "bucket.internal:9777", "HTTP/1.1 bucket.internal:9777"},
			{"api", "api.internal:9777", "HTTP/2.0 api.internal:9777"},
		} {
			res := fetch(t, c, tc.authority, "/")
			if res.status != http.StatusOK || res.body != tc.want {
				t.Errorf("%s, h2 caller %v: %d %q, want 200 %q", tc.name, h2Only, res.status, res.body, tc.want)
			}
		}
	}
}

// The reason for probing rather than retrying: the body reaches the service
// once, whole, on a first request to an HTTP/1.1 upstream.
func TestABodyToAnHTTP1UpstreamArrivesOnce(t *testing.T) {
	reg := registry.New()
	var calls atomic.Int32
	ln := listenLoopback(t)
	serveOn(t, ln, http1Only(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			calls.Add(1)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, "%d", len(body))
	}))
	registerPort(t, reg, "bucket", ln)
	c := client(t, serveProxy(t, reg), false)

	reader, writer := io.Pipe()
	go func() {
		_, _ = io.WriteString(writer, strings.Repeat("x", 1<<20))
		_ = writer.Close()
	}()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, "http://bucket.internal:9777/o", reader)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer res.Body.Close()
	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if res.StatusCode != http.StatusOK || string(got) != fmt.Sprint(1<<20) {
		t.Errorf("%d %q, want 200 and the whole body", res.StatusCode, got)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("the handler ran %d times, want once", n)
	}
}

func TestGRPCToAnHTTP1UpstreamIsRefusedByName(t *testing.T) {
	reg := registry.New()
	var calls atomic.Int32
	ln := listenLoopback(t)
	serveOn(t, ln, http1Only(), http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			calls.Add(1)
		}
	}))
	registerPort(t, reg, "bucket", ln)
	c := client(t, serveProxy(t, reg), true)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"http://bucket.internal:9777/pkg.Service/Method", strings.NewReader(""))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/grpc")
	res, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()
	if res.Header.Get("Grpc-Status") == "" && res.StatusCode == http.StatusOK {
		t.Errorf("a refused gRPC call reads as success: %v", res.Header)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(res.Header.Get("Grpc-Message")+string(body), "HTTP/1.1") {
		t.Errorf("the refusal doesn't name the cause: headers %v, body %q", res.Header, body)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("the service was called %d times, want none", n)
	}
}

// A restarted service can reuse a port with the other protocol. One request
// fails, and the next finds out.
func TestAPortThatChangesProtocolIsAskedAgain(t *testing.T) {
	reg := registry.New()
	first := listenLoopback(t)
	addr := first.Addr().String()
	srv := serveOn(t, first, http1Only(), reportProto)
	registerPort(t, reg, "svc", first)
	c := client(t, serveProxy(t, reg), false)

	if res := fetch(t, c, "svc.internal:9777", "/"); !strings.HasPrefix(res.body, "HTTP/1.1") {
		t.Fatalf("first server: %d %q", res.status, res.body)
	}
	_ = srv.Close()
	second, err := new(net.ListenConfig).Listen(context.Background(), "tcp", addr)
	if err != nil {
		t.Skipf("the port went elsewhere: %v", err)
	}
	serveOn(t, second, h2cOnly(), reportProto)

	if res := fetch(t, c, "svc.internal:9777", "/"); res.status != http.StatusBadGateway {
		t.Fatalf("the request that discovers the change: %d %q, want 502", res.status, res.body)
	}
	if res := fetch(t, c, "svc.internal:9777", "/"); !strings.HasPrefix(res.body, "HTTP/2.0") {
		t.Errorf("after the change: %d %q, want HTTP/2.0", res.status, res.body)
	}
}

func TestProbeUpstream(t *testing.T) {
	t.Run("h2c", func(t *testing.T) {
		ln := listenLoopback(t)
		serveOn(t, ln, h2cOnly(), reportProto)
		if got := probeUpstream(context.Background(), ln.Addr().String()); got != upstreamH2C {
			t.Errorf("got %d, want h2c", got)
		}
	})
	t.Run("http1", func(t *testing.T) {
		ln := listenLoopback(t)
		serveOn(t, ln, http1Only(), reportProto)
		if got := probeUpstream(context.Background(), ln.Addr().String()); got != upstreamHTTP1 {
			t.Errorf("got %d, want HTTP/1.1", got)
		}
	})
	t.Run("nothing listening", func(t *testing.T) {
		ln := listenLoopback(t)
		addr := ln.Addr().String()
		_ = ln.Close()
		if got := probeUpstream(context.Background(), addr); got != upstreamUnknown {
			t.Errorf("got %d, want unknown", got)
		}
	})
	t.Run("silent", func(t *testing.T) {
		ln := listenLoopback(t)
		t.Cleanup(func() { _ = ln.Close() })
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if got := probeUpstream(ctx, ln.Addr().String()); got != upstreamUnknown {
			t.Errorf("got %d, want unknown", got)
		}
	})
}

func TestAnUpstreamSpeakingNeitherIsNamed(t *testing.T) {
	reg := registry.New()
	ln := listenLoopback(t)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = io.WriteString(conn, "220 mail.example ESMTP ready\r\n")
			_ = conn.Close()
		}
	}()
	registerPort(t, reg, "mail", ln)
	res := fetch(t, client(t, serveProxy(t, reg), false), "mail.internal:9777", "/")
	if res.status != http.StatusBadGateway || !strings.Contains(res.body, "HTTP/2 or HTTP/1.1 there, in cleartext or over TLS") {
		t.Errorf("%d %q, want a 502 naming what voya tried", res.status, res.body)
	}
}

// awaitProbeClosed consumes the close of the probe's own connection, so that
// the next close on the channel is the pooled one the caller is measuring.
func awaitProbeClosed(t *testing.T, closed <-chan struct{}) {
	t.Helper()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the probe's connection never closed")
	}
}

func TestTheProbeReachesOnlyABareHandler(t *testing.T) {
	var seen atomic.Int32
	count := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { seen.Add(1) })

	bare := listenLoopback(t)
	serveOn(t, bare, http1Only(), count)
	probeUpstream(context.Background(), bare.Addr().String())
	if n := seen.Load(); n != 1 {
		t.Errorf("a bare handler saw %d requests from the probe, want the documented 1", n)
	}

	seen.Store(0)
	mux := http.NewServeMux()
	mux.Handle("/", count)
	muxed := listenLoopback(t)
	serveOn(t, muxed, http1Only(), mux)
	probeUpstream(context.Background(), muxed.Addr().String())
	if n := seen.Load(); n != 0 {
		t.Errorf("a handler behind a ServeMux saw %d requests from the probe, want none", n)
	}
}

func TestGRPCWebIsNotRefused(t *testing.T) {
	for contentType, want := range map[string]bool{
		"application/grpc":           true,
		"application/grpc+proto":     true,
		"application/grpc-web":       false,
		"application/grpc-web+proto": false,
		"application/json":           false,
	} {
		r, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://x/", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Content-Type", contentType)
		if got := needsHTTP2(r); got != want {
			t.Errorf("needsHTTP2(%s) = %v, want %v", contentType, got, want)
		}
	}
}

// tlsBackend serves handler over TLS on loopback, with a certificate for
// another name, as a local service's would be.
func tlsBackend(t *testing.T, http2 bool, handler http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(handler)
	srv.EnableHTTP2 = http2
	srv.Config.ErrorLog = nil
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestATLSOnlyServiceIsReachedOverPlainHTTP(t *testing.T) {
	for _, tc := range []struct {
		name  string
		http2 bool
		proto string
	}{{"http1", false, "HTTP/1.1"}, {"http2", true, "HTTP/2.0"}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := tlsBackend(t, tc.http2, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, "%s %s tls=%v", r.Proto, r.Host, r.TLS != nil)
			}))
			reg := registry.New()
			registerPort(t, reg, "secure", srv.Listener)
			res := fetch(t, client(t, serveProxy(t, reg), false), "secure.internal:9777", "/")
			want := tc.proto + " secure.internal:9777 tls=true"
			if res.status != http.StatusOK || res.body != want {
				t.Errorf("%d %q, want 200 %q", res.status, res.body, want)
			}
		})
	}
}

func grpcCall(t *testing.T, c *http.Client, authority string) (http.Header, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"http://"+authority+"/pkg.Service/Method", strings.NewReader(""))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/grpc")
	res, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.Header, string(body)
}

// gRPC depends on what the handshake offers, not on TLS: it passes over TLS
// HTTP/2 and is refused over TLS HTTP/1.1.
func TestGRPCOverTLSFollowsTheNegotiatedProtocol(t *testing.T) {
	for _, tc := range []struct {
		name    string
		http2   bool
		reached int32
	}{{"http2 is forwarded", true, 1}, {"http1 is refused", false, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := tlsBackend(t, tc.http2, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/grpc")
				w.Header().Set("Grpc-Status", "0")
			}))
			reg := registry.New()
			registerPort(t, reg, "secure", srv.Listener)
			header, body := grpcCall(t, client(t, serveProxy(t, reg), true), "secure.internal:9777")
			if n := calls.Load(); n != tc.reached {
				t.Errorf("the service was called %d times, want %d", n, tc.reached)
			}
			if tc.reached == 0 && !strings.Contains(header.Get("Grpc-Message")+body, "HTTP/1.1") {
				t.Errorf("the refusal doesn't name the cause: %v %q", header, body)
			}
		})
	}
}

func TestProbeUpstreamOverTLS(t *testing.T) {
	for _, tc := range []struct {
		name  string
		http2 bool
		want  upstreamProtocol
	}{{"http1", false, upstreamTLSHTTP1}, {"http2", true, upstreamTLSHTTP2}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := tlsBackend(t, tc.http2, reportProto)
			if got := probeUpstream(context.Background(), srv.Listener.Addr().String()); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
	// Python's TLS server hangs up on the preface rather than answering it.
	t.Run("closes on the preface", func(t *testing.T) {
		ln := listenLoopback(t)
		srv := tlsBackend(t, false, reportProto)
		certs := srv.TLS.Certificates
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			for first := true; ; first = false {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				if first {
					_ = conn.Close()
					continue
				}
				_ = tls.Server(conn, &tls.Config{Certificates: certs, MinVersion: tls.VersionTLS12}).Handshake()
			}
		}()
		if got := probeUpstream(context.Background(), ln.Addr().String()); got != upstreamTLSHTTP1 {
			t.Errorf("got %d, want TLS HTTP/1.1", got)
		}
	})
}

func TestUpstreamTLSIsOnlyForLoopback(t *testing.T) {
	u := newUpstreamRoundTripper(time.Second)
	u.probe = func(context.Context, string) upstreamProtocol { return upstreamTLSHTTP1 }
	var dialed atomic.Value
	transport, ok := u.tls.(*http.Transport)
	if !ok {
		t.Fatal("the TLS transport isn't an *http.Transport")
	}
	transport.DialContext = func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed.Store(addr)
		return nil, errors.New("stop")
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1:1/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := u.RoundTrip(req); err == nil {
		_ = res.Body.Close()
	}
	if got := dialed.Load(); got != "127.0.0.1:1" {
		t.Errorf("dialed %v, want the request's loopback address", got)
	}
	if req.URL.Scheme != "http" {
		t.Errorf("RoundTrip changed the caller's request: scheme %q", req.URL.Scheme)
	}
}
