package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trendvidia/voya/internal/auth"
	"github.com/trendvidia/voya/internal/registry"
	"github.com/trendvidia/voya/internal/requestlog"
)

// --- #150: the request log ---------------------------------------------------

// recorded is a Recorder for these tests: every record, in the order the
// proxy handed them over, behind a lock because the proxy records from the
// server's goroutines.
type recorded struct {
	mu   sync.Mutex
	recs []requestlog.Request
}

func (r *recorded) Record(req requestlog.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, req)
}

func (r *recorded) all() []requestlog.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.recs)
}

// waitRecorded polls until n records have landed: the record is written after
// the response, so a caller that has read its response can still be ahead of
// it by a scheduling quantum.
func (r *recorded) waitRecorded(t *testing.T, n int) []requestlog.Request {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if recs := r.all(); len(recs) >= n {
			return recs
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d requests were recorded", len(r.all()), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestEveryAnsweredRequestIsRecordedOnce is #150's first criterion, at the
// proxy: a routed request and one answered from each kind of path — a
// diagnostic row, the reserved name's own handler, a preflight — each land as
// exactly one record with all six fields, in the order they were answered.
func TestEveryAnsweredRequestIsRecordedOnce(t *testing.T) {
	reg := registry.New()
	rec := &recorded{}
	payments := registered(t, reg, "payments", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Millisecond) // so the duration is measurably not zero
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "made")
	}))
	up, _ := reg.Lookup("payments")
	addr := serveProxy(t, reg, WithRequestLog(rec), WithInternalHandler(internalStub()))
	c := client(t, addr, false)

	before := time.Now()
	type exchange struct {
		authority, method, path string
		wantStatus              int
		wantPort                uint32
	}
	exchanges := []exchange{
		{payments, http.MethodGet, "/v1/charge", http.StatusCreated, up.Port},                      // routed
		{registry.Authority("nobody"), http.MethodGet, "/v1/x", http.StatusNotFound, 0},            // never registered
		{"api.payments.internal", http.MethodGet, "/", http.StatusBadRequest, 0},                   // not a name
		{registry.Authority(registry.Reserved), http.MethodGet, "/", http.StatusNotImplemented, 0}, // the reserved row
	}
	// And one DiagService serves under the reserved name, which is the one
	// kind of request the log leaves out: made first, and expected nowhere in
	// the records below.
	diagReq, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"http://"+registry.Authority(registry.Reserved)+"/voya.diag.v1.DiagService/GetStatus", nil)
	if err != nil {
		t.Fatal(err)
	}
	diagResp, err := c.Do(diagReq)
	if err != nil {
		t.Fatalf("GetStatus through the proxy: %v", err)
	}
	_ = diagResp.Body.Close()
	if diagResp.StatusCode != http.StatusOK {
		t.Fatalf("the internal stub answered %d; the setup wants it served", diagResp.StatusCode)
	}
	for _, ex := range exchanges {
		req, err := http.NewRequestWithContext(context.Background(), ex.method, "http://"+ex.authority+ex.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", ex.method, ex.authority, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != ex.wantStatus {
			t.Fatalf("%s %s%s answered %d, the setup wants %d", ex.method, ex.authority, ex.path, resp.StatusCode, ex.wantStatus)
		}
	}
	// A CORS preflight is answered by voya without routing, and is a request
	// the proxy answered: it is recorded too.
	pre, err := http.NewRequestWithContext(context.Background(), http.MethodOptions, "http://"+payments+"/v1/charge", nil)
	if err != nil {
		t.Fatal(err)
	}
	pre.Header.Set("Origin", "http://localhost:3000")
	pre.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := c.Do(pre)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	_ = resp.Body.Close()
	exchanges = append(exchanges, exchange{payments, http.MethodOptions, "/v1/charge", http.StatusNoContent, 0})

	recs := rec.waitRecorded(t, len(exchanges))
	if len(recs) != len(exchanges) {
		t.Fatalf("%d records for %d requests: %v", len(recs), len(exchanges), recs)
	}
	for _, r := range recs {
		if strings.Contains(r.Path, "DiagService") {
			t.Errorf("a request DiagService served was recorded: %+v", r)
		}
	}
	for i, ex := range exchanges {
		got := recs[i]
		if got.Authority != ex.authority || got.Method != ex.method || got.Path != ex.path ||
			got.Status != ex.wantStatus || got.UpstreamPort != ex.wantPort {
			t.Errorf("record %d = %+v\n  want authority %q method %q path %q status %d upstream %d",
				i, got, ex.authority, ex.method, ex.path, ex.wantStatus, ex.wantPort)
		}
		if got.At.Before(before) || got.At.After(time.Now()) {
			t.Errorf("record %d: at = %v, not between the first request and now", i, got.At)
		}
		if got.Duration <= 0 {
			t.Errorf("record %d: duration = %v", i, got.Duration)
		}
	}
	// The routed one took at least the handler's sleep; a duration measured
	// before the response was written would not.
	if d := recs[0].Duration; d < 2*time.Millisecond {
		t.Errorf("the routed request's duration is %v, shorter than the 2ms its handler slept", d)
	}
}

// TestAClaimedNonProcedurePathIsRecorded is #209's request-log criterion:
// the JWKS fetch is a developer's request, not voya reading itself, so
// unlike a DiagService procedure it lands in the log — with its status and
// no upstream port, the shape of every answer voya writes itself.
func TestAClaimedNonProcedurePathIsRecorded(t *testing.T) {
	reg := registry.New()
	rec := &recorded{}
	addr := serveProxy(t, reg, WithRequestLog(rec), WithInternalHandler(internalStub()))
	res := fetch(t, client(t, addr, false), registry.Authority(registry.Reserved), auth.JWKSPath)
	if res.status != http.StatusOK || res.body != internalStubWellKnown {
		t.Fatalf("status %d body %q; the setup wants the router's own answer %q", res.status, res.body, internalStubWellKnown)
	}
	recs := rec.waitRecorded(t, 1)
	got := recs[0]
	if got.Authority != registry.Authority(registry.Reserved) || got.Method != http.MethodGet ||
		got.Path != auth.JWKSPath || got.Status != http.StatusOK || got.UpstreamPort != 0 {
		t.Errorf("record = %+v\n  want authority %q method GET path %q status 200 upstream 0",
			got, registry.Authority(registry.Reserved), auth.JWKSPath)
	}
}

// TestAnUnreachableUpstreamKeepsItsPort: the one diagnostic row whose record
// carries a port. The request was forwarded — to a registered port that
// refused — and the port is the developer's handle on which process is gone.
func TestAnUnreachableUpstreamKeepsItsPort(t *testing.T) {
	reg := registry.New()
	rec := &recorded{}
	port := deadPort(t)
	if _, err := reg.Register(registry.Instance{Service: "gone", Port: port}); err != nil {
		t.Fatal(err)
	}
	addr := serveProxy(t, reg, WithRequestLog(rec))
	got := fetch(t, client(t, addr, false), registry.Authority("gone"), "/")
	if got.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for a dead upstream", got.status)
	}
	recs := rec.waitRecorded(t, 1)
	if recs[0].Status != http.StatusBadGateway || recs[0].UpstreamPort != port {
		t.Errorf("record = %+v, want status 502 and upstream port %d", recs[0], port)
	}
}

// TestTheRecordedAuthorityDropsVoyasOwnPort is #161's decision at the column
// it is about: the authority recorded is what arrived, less voya's own proxy
// port, so one service reads one way whichever caller asked for it — an h2
// client puts the port in :authority, an HTTP/1.1 client setting Host does not
// — and a port that is *not* voya's is kept, because a caller that dialled
// voya while naming another port is exactly what this column should show.
//
// The caller kind is asserted rather than assumed: what the record says is
// only interesting if the two requests really did arrive over the two
// protocols, and a transport offered both would be free to pick either.
func TestTheRecordedAuthorityDropsVoyasOwnPort(t *testing.T) {
	reg := registry.New()
	rec := &recorded{}
	payments := registered(t, reg, "payments", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	addr := serveProxy(t, reg, WithRequestLog(rec))
	_, ownPort, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("the proxy's address %q has no port: %v", addr, err)
	}
	// A port nothing here listens on, and not voya's: 9777 is the default
	// proxy port, which is the shape of the mistake — a caller that kept the
	// fleet's port while voya moved.
	const foreign = "9777"
	if foreign == ownPort {
		t.Skipf("the proxy drew the port this test uses for a foreign one (%s)", foreign)
	}

	for i, tc := range []struct {
		name      string
		authority string
		h2        bool
		wantProto string
		want      string
	}{
		{"h2 naming voya's port", payments + ":" + ownPort, true, "HTTP/2.0", payments},
		{"HTTP/1.1 naming no port", payments, false, "HTTP/1.1", payments},
		{"h2 naming a foreign port", payments + ":" + foreign, true, "HTTP/2.0", payments + ":" + foreign},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := client(t, addr, true)
			if !tc.h2 {
				c = http1Client(t, addr)
			}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
				"http://"+tc.authority+"/v1/charge", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := c.Do(req)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.authority, err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.Proto != tc.wantProto {
				t.Fatalf("the caller spoke %s, and this case is about %s", resp.Proto, tc.wantProto)
			}
			// Routed, foreign port and all: the port is the record's business
			// and not routing's (registry.ServiceFromAuthority strips it).
			if resp.StatusCode != http.StatusOK || string(body) != "ok" {
				t.Fatalf("%s answered %d %q, want the routed 200", tc.authority, resp.StatusCode, body)
			}
			recs := rec.waitRecorded(t, i+1)
			if got := recs[i].Authority; got != tc.want {
				t.Errorf("a request arriving as %q over %s recorded authority %q, want %q",
					tc.authority, tc.wantProto, got, tc.want)
			}
		})
	}
}

// http1Client dials the proxy over HTTP/1.1 and nothing else — the caller kind
// that sends Host without a port. [client]'s h2Only=false leaves both
// protocols enabled, and which one a transport then picks is not a test's to
// assume.
func http1Client(t *testing.T, proxyAddr string) *http.Client {
	t.Helper()
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	transport := &http.Transport{
		Protocols: protocols,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, proxyAddr)
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 30 * time.Second}
}

// TestWithoutARequestLogNothingIsRecorded is the default, and what a daemon
// with `request_log = false` builds: no recorder, and no wrapper around the
// writer either — the handler sees the server's own, which is what net/http
// optimises for. A nil handed to the option is the default too.
func TestWithoutARequestLogNothingIsRecorded(t *testing.T) {
	reg := registry.New()
	payments := registered(t, reg, "payments", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	for name, h := range map[string]http.Handler{"default": New(reg), "WithRequestLog(nil)": New(reg, WithRequestLog(nil))} {
		p, ok := h.(*proxy)
		if !ok {
			t.Fatalf("New returned %T", h)
		}
		if p.requests != nil {
			t.Errorf("%s: the proxy has a recorder: %v", name, p.requests)
		}
		rw := httptest.NewRecorder()
		p.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "http://"+payments+"/", nil))
		if rw.Code != http.StatusOK || rw.Body.String() != "ok" {
			t.Errorf("%s: %d %q", name, rw.Code, rw.Body.String())
		}
	}
}

// TestTheRequestLogCarriesNoPayloadByte is ADR 003 held at the log: a body,
// a query string and a header each carrying a sentinel reach the backend —
// the positive control, asserted first — and none of the three reaches the
// record.
//
// The sentinel is a fixed literal, not derived from anything the check
// searches: the corpus searched is what voya recorded about the request,
// built by voya from the request, and the search is for the string the
// caller put in the request. The control that would prove nothing is a
// search over a corpus made from the pattern itself.
func TestTheRequestLogCarriesNoPayloadByte(t *testing.T) {
	const sentinel = "card=4111111111111111"
	reg := registry.New()
	rec := &recorded{}
	backendSaw := make(chan [3]string, 1)
	payments := registered(t, reg, "payments", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		backendSaw <- [3]string{string(body), r.URL.RawQuery, r.Header.Get("X-Trace-Note")}
		w.WriteHeader(http.StatusOK)
	}))
	addr := serveProxy(t, reg, WithRequestLog(rec))
	c := client(t, addr, false)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"http://"+payments+"/v1/charge?note="+sentinel, strings.NewReader("body:"+sentinel))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Trace-Note", sentinel)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp.Body.Close()

	// The setup: the sentinel travelled, all three ways.
	select {
	case saw := <-backendSaw:
		for i, where := range []string{"body", "query", "header"} {
			if !strings.Contains(saw[i], sentinel) {
				t.Fatalf("the backend did not see the sentinel in the %s (%q); the control never ran", where, saw[i])
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the backend never saw the request")
	}

	// The claim: nothing recorded contains it.
	recs := rec.waitRecorded(t, 1)
	r := recs[0]
	rendered := r.String()
	for name, s := range map[string]string{
		"authority": r.Authority, "method": r.Method, "path": r.Path, "the rendered line": rendered,
	} {
		if strings.Contains(s, sentinel) {
			t.Errorf("%s carries a payload byte: %q", name, s)
		}
	}
	if r.Path != "/v1/charge" {
		t.Errorf("path = %q, want the path alone", r.Path)
	}
	// And the sentinel is not hiding in a field the loop above did not
	// name — the record is small enough to render whole.
	if whole := fmt.Sprintf("%+v", r); strings.Contains(whole, sentinel) {
		t.Errorf("the record carries the sentinel somewhere: %s", whole)
	}
}

// TestTheStatusWriterDoesNotBufferAStream: the wrapper the log needs must
// let a streamed response through as it is written, or the request log
// would cost every server stream its streaming. TestResponseIsNotBuffered
// makes the claim without the log; this is the same claim with it.
func TestTheStatusWriterDoesNotBufferAStream(t *testing.T) {
	reg := registry.New()
	rec := &recorded{}
	release := make(chan struct{})
	payments := registered(t, reg, "payments", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "first\n")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("backend flush: %v", err)
		}
		<-release
		_, _ = io.WriteString(w, "second\n")
	}))
	addr := serveProxy(t, reg, WithRequestLog(rec))
	c := client(t, addr, true)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+payments+"/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 6)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("reading the first chunk while the handler is still held: %v", err)
	}
	if string(buf) != "first\n" {
		t.Fatalf("first chunk = %q", buf)
	}
	// Nothing is recorded while the stream is open: the record is on the
	// way out.
	if n := len(rec.all()); n != 0 {
		t.Errorf("%d records with the stream still open", n)
	}
	close(release)
	rest, _ := io.ReadAll(resp.Body)
	if string(rest) != "second\n" {
		t.Errorf("rest = %q", rest)
	}
	recs := rec.waitRecorded(t, 1)
	if recs[0].Status != http.StatusOK || recs[0].Path != "/stream" {
		t.Errorf("record = %+v", recs[0])
	}
}

// TestAnEarlyHintIsNotTheStatus: an upstream that sends an informational
// response before the real one is recorded with the status its caller saw,
// not with the hint.
//
// ReverseProxy forwards a 1xx by calling WriteHeader on the writer voya
// handed it — [statusWriter] — from its Got1xxResponse hook, so a wrapper
// that latched the first code of any kind would record 103 here while the
// caller read 200. The caller's own status is asserted first: that is what
// makes the record's disagreement with it the thing under test rather than
// a backend that never answered 200 at all.
func TestAnEarlyHintIsNotTheStatus(t *testing.T) {
	reg := registry.New()
	rec := &recorded{}
	hints := registered(t, reg, "hints", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", "</style.css>; rel=preload; as=style")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Del("Link")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "made")
	}))
	addr := serveProxy(t, reg, WithRequestLog(rec))
	got := fetch(t, client(t, addr, false), hints, "/hinted")
	if got.status != http.StatusCreated {
		t.Fatalf("the caller saw %d; the setup wants the backend's 201 through, after its 103", got.status)
	}
	recs := rec.waitRecorded(t, 1)
	if recs[0].Status != got.status {
		t.Errorf("record = %+v, want the status the caller saw, %d", recs[0], got.status)
	}
}

// TestStatusWriterReadsTheStatusOffTheWire drives the wrapper directly for
// the ways a handler ends: an explicit status, an implicit 200 from a bare
// Write, a return with nothing written — which net/http answers 200 — and
// the informational responses that precede any of them.
func TestStatusWriterReadsTheStatusOffTheWire(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(w http.ResponseWriter)
		want int
	}{
		{"explicit", func(w http.ResponseWriter) { w.WriteHeader(http.StatusTeapot) }, http.StatusTeapot},
		{"implicit from Write", func(w http.ResponseWriter) { _, _ = w.Write([]byte("x")) }, http.StatusOK},
		{"nothing written", func(http.ResponseWriter) {}, http.StatusOK},
		{"second WriteHeader is ignored", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusAccepted)
			w.WriteHeader(http.StatusTeapot)
		}, http.StatusAccepted},
		{"1xx before the status", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusEarlyHints)
			w.WriteHeader(http.StatusTeapot)
		}, http.StatusTeapot},
		{"100-continue before an implicit 200", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusContinue)
			_, _ = w.Write([]byte("x"))
		}, http.StatusOK},
		{"1xx and nothing else", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusEarlyHints)
		}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sw := &statusWriter{ResponseWriter: httptest.NewRecorder()}
			tc.do(sw)
			if got := sw.status(); got != tc.want {
				t.Errorf("status() = %d, want %d", got, tc.want)
			}
		})
	}
	// And the controller reaches the real writer through it.
	inner := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: inner}
	if err := http.NewResponseController(sw).Flush(); err != nil {
		t.Errorf("Flush through the controller: %v", err)
	}
	if !inner.Flushed {
		t.Error("the flush did not reach the underlying writer")
	}
}

// BenchmarkRequestLogOverhead is #150's measurement: median request latency
// through the proxy with the log on against off. Reported as median-µs/op
// alongside the mean the harness reports, because only the median is about
// the code (CLAUDE.md). Run with:
//
//	go test -run '^$' -bench RequestLogOverhead -benchtime 5000x ./internal/proxy
//
// on an idle machine (`uptime` first).
//
// Three cases, because two of them were not what ships (#163): the daemon at
// LOG_LEVEL_INFO — its default — records through a tee of the buffer *and* the
// live line, and the write to the terminal beside the ring buffer is the
// larger of the two costs. "on+live" is that tee, and the case #150's
// published number should be read against.
func BenchmarkRequestLogOverhead(b *testing.B) {
	for _, tc := range []struct {
		name string
		opts func(*testing.B) []Option
	}{
		{"off", func(*testing.B) []Option { return nil }},
		{"on", func(*testing.B) []Option {
			return []Option{WithRequestLog(requestlog.New(requestlog.Capacity))}
		}},
		{"on+live", func(b *testing.B) []Option {
			live := drainedPipe(b)
			return []Option{WithRequestLog(requestlog.Tee(
				requestlog.New(requestlog.Capacity),
				requestlog.RecorderFunc(func(r requestlog.Request) { fmt.Fprintf(live, "%s\n", r) }),
			))}
		}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			opts := tc.opts(b)
			reg := registry.New()
			ln, err := Listen(context.Background(), "127.0.0.1:0")
			if err != nil {
				b.Fatal(err)
			}
			srv := NewHTTPServer(context.Background(), New(reg, opts...))
			go func() { _ = srv.Serve(ln) }()
			b.Cleanup(func() { _ = srv.Close() })
			backendSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			backendSrv.EnableHTTP2 = true
			protocols := new(http.Protocols)
			protocols.SetUnencryptedHTTP2(true)
			backendSrv.Config.Protocols = protocols
			backendSrv.Start()
			b.Cleanup(backendSrv.Close)
			if _, err := reg.Register(registry.Instance{Service: "payments", Port: portOfAddr(b, backendSrv.Listener.Addr().String())}); err != nil {
				b.Fatal(err)
			}
			c := client(b, ln.Addr().String(), true)
			url := "http://" + registry.Authority("payments") + "/"
			// Warm the pooled connections on both legs before timing.
			for range 20 {
				if _, err := get(c, url); err != nil {
					b.Fatal(err)
				}
			}
			samples := make([]time.Duration, 0, b.N)
			b.ResetTimer()
			for range b.N {
				start := time.Now()
				if _, err := get(c, url); err != nil {
					b.Fatal(err)
				}
				samples = append(samples, time.Since(start))
			}
			b.StopTimer()
			sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
			b.ReportMetric(float64(samples[len(samples)/2])/float64(time.Microsecond), "median-µs/op")
		})
	}
}

// drainedPipe is where the benchmark's live line goes: a real os.Pipe, read by
// a goroutine of its own.
//
// A real fd because that is what the daemon writes to — `voya run`'s stderr —
// and io.Discard would measure the Fprintf and not the write, which is the
// half #163 says the published number missed. Drained because a pipe nobody
// reads fills at 64KiB and would turn the live line into a block on the proxy
// path, benchmarking the reader's absence instead of the write.
func drainedPipe(b *testing.B) io.Writer {
	b.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		b.Fatalf("os.Pipe: %v", err)
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = io.Copy(io.Discard, r)
	}()
	b.Cleanup(func() {
		_ = w.Close() // the reader sees EOF and returns
		<-drained
		_ = r.Close()
	})
	return w
}

// TestACallerThatGoesAwayIsNotAnUpstreamFailure is #169: a request the
// *caller* abandoned — its own deadline ran out, a Ctrl-C'd curl — against a
// service that accepted it and had not answered yet is neither logged as
// "the connection failed" nor recorded as a 5xx laid at the upstream's door.
// The service never failed, and the client is not reading the answer; the
// record says the client closed it, and still names the port it was routed
// to.
//
// Both caller protocols, because the daemon learns the caller is gone by
// two different mechanisms — an HTTP/1.1 connection closing under the
// server's background read, an HTTP/2 stream reset — and each is what
// cancels the request context the ErrorHandler reads. The setup is
// asserted: the backend saw the request before the caller cancelled, so the
// cancel really did land mid-flight and not before the dial.
func TestACallerThatGoesAwayIsNotAnUpstreamFailure(t *testing.T) {
	for name, h2Only := range map[string]bool{"http1": false, "h2c": true} {
		t.Run(name, func(t *testing.T) {
			reg := registry.New()
			rec := &recorded{}
			var (
				mu     sync.Mutex
				logged []string
			)
			logf := func(format string, args ...any) {
				mu.Lock()
				defer mu.Unlock()
				logged = append(logged, fmt.Sprintf(format, args...))
			}
			reached := make(chan struct{}, 1)
			// Accepts, and answers only when the proxy gives up on it: the
			// wedged service `voya health` probes to its deadline.
			port := backend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached <- struct{}{}
				<-r.Context().Done()
			}))
			if _, err := reg.Register(registry.Instance{Service: "stuck", Port: port}); err != nil {
				t.Fatalf("Register: %v", err)
			}
			authority := registry.Authority("stuck")
			addr := serveProxy(t, reg, WithRequestLog(rec), WithErrorLog(logf))
			c := client(t, addr, h2Only)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+authority+"/slow", nil)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				resp, err := c.Do(req)
				if err == nil {
					_ = resp.Body.Close()
				}
				done <- err
			}()
			select {
			case <-reached:
			case <-time.After(10 * time.Second):
				t.Fatal("the backend never saw the request")
			}
			cancel()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("the cancelled request completed with a response")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the cancelled request did not return")
			}

			recs := rec.waitRecorded(t, 1)
			if recs[0].Status != StatusClientClosedRequest || recs[0].UpstreamPort != port {
				t.Errorf("record = %+v, want status %d and upstream port %d", recs[0], StatusClientClosedRequest, port)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(logged) != 0 {
				t.Errorf("the proxy logged the caller's cancel as its own failure:\n%s", strings.Join(logged, "\n"))
			}
		})
	}
}
