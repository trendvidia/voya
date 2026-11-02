package roundtriptest

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"google.golang.org/protobuf/types/known/emptypb"
)

// newRequest builds one request through the proxy. Body and headers are
// optional so the isConnectUnary table below can vary exactly the part of
// Connect unary's shape each case is about, and nothing else.
func newRequest(t *testing.T, method, url, body string, headers map[string]string) *http.Request {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(testContext(t), method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	return req
}

// roundTrip sends req through the fleet's proxy and reads the whole response.
func (f *fleet) roundTrip(t *testing.T, req *http.Request) (status int, contentType, body string) {
	t.Helper()
	res, err := f.client.Do(req)
	if err != nil {
		t.Fatalf("request to %q: %v", req.URL.Host, err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return res.StatusCode, res.Header.Get("Content-Type"), string(b)
}

// get makes a plain HTTP request through the proxy so the diagnostic can be
// read as a developer with curl would read it: status line and body.
func (f *fleet) get(t *testing.T, authority string) (int, string) {
	t.Helper()
	status, _, body := f.roundTrip(t, newRequest(t, http.MethodGet, "http://"+authority+procAuthority, "", nil))
	return status, body
}

func mustContain(t *testing.T, body string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("diagnostic does not mention %q:\n%s", want, body)
		}
	}
}

// TestUnknownAuthorityIsDiagnosed covers the typo, which is the common case.
// ADR 005 requires the response itself to name what arrived and list what
// would have worked — a bare 502 sends the developer to the wrong service to
// debug a problem that is not there.
func TestUnknownAuthorityIsDiagnosed(t *testing.T) {
	f := newFleet(t)

	status, body := f.get(t, "paymnets.internal:9777")

	if status != http.StatusNotFound {
		t.Errorf("status %d, want %d for a name that never registered", status, http.StatusNotFound)
	}
	mustContain(t, body,
		`"paymnets.internal"`,                                      // the name it resolved to
		`"paymnets.internal:9777"`,                                 // the authority exactly as received
		"Nothing has ever registered",                              // why
		"Registered services: billing.internal, payments.internal", // what would work
	)
	t.Logf("unknown authority, %d:\n%s", status, body)
}

// TestKnownButUnboundIsADifferentMessage is the distinction issue #12 asks
// for. Both are "your request did not arrive", but one means check your
// spelling and the other means check your other terminal. Collapsing them
// costs the developer the twenty minutes the ADR is trying to save.
func TestKnownButUnboundIsADifferentMessage(t *testing.T) {
	f := newFleet(t)

	unboundStatus, unbound := f.get(t, "reports.internal:9777")
	unknownStatus, unknown := f.get(t, "paymnets.internal:9777")

	if unboundStatus != http.StatusServiceUnavailable {
		t.Errorf("status %d for a known-but-unbound service, want %d", unboundStatus, http.StatusServiceUnavailable)
	}
	if unboundStatus == unknownStatus {
		t.Errorf("known-but-unbound and never-registered share status %d; nothing distinguishes them to a client", unboundStatus)
	}
	if unbound == unknown {
		t.Fatal("known-but-unbound and never-registered produced identical bodies")
	}
	mustContain(t, unbound,
		`"reports.internal"`,
		"has no running instance",
		"stream dropped, or it has not started yet",
	)
	if strings.Contains(unbound, "Nothing has ever registered") {
		t.Error("a known service was described as never registered")
	}
	mustContain(t, unknown, "Known but not running: reports.internal")
	t.Logf("known but unbound, %d:\n%s", unboundStatus, unbound)
}

// callAndCatch makes a call expected to fail and returns the client's error.
func (f *fleet) callAndCatch(t *testing.T, authority string, opts ...connect.ClientOption) error {
	t.Helper()
	c := authorityClient(f.client, "http://"+authority, opts...)
	_, err := c.CallUnary(testContext(t), connect.NewRequest(&emptypb.Empty{}))
	if err == nil {
		t.Fatalf("call to %q succeeded, want a routing failure", authority)
	}
	return err
}

// TestDiagnosticIsInvisibleToConnectClients pins a gap between ADR 005 and
// what a Go caller actually experiences — the discovery this spike was worth
// running for.
//
// ADR 005 makes the error response the primary diagnostic surface. It is,
// through curl. But connect-go requires an error body to be JSON: for any
// other content type it takes the HTTP status and discards the body unread
// (protocol_connect.go:1378, protocol_grpc.go:657). So the entire fleet —
// every Go service, which is all of them — sees a bare code where the
// diagnostic should be.
//
// The 404 case is worse than uninformative. connect-go maps HTTP 404 to
// CodeUnimplemented, so a typo'd authority reads as "that service does not
// implement this method" — pointing the developer at the callee's code
// instead of at their spelling.
//
// Pinned rather than fixed here: the remedy makes voya's error path
// protocol-aware, which touches ADR 003 and ADR 005. Demonstrated in
// TestConnectEnvelopeMakesTheDiagnosticVisible, filed as issue #23. When #17
// adopts it, this test fails and that is the signal to delete it.
//
// The gRPC and gRPC-Web rows are gone: their remedy is a Trailers-Only
// response rather than a body, adopted by the real proxy in #25 — the
// invisibility they pinned no longer exists to pin. What remains is the
// Connect row, documenting against this spike's plain-text proxy why the
// envelope had to exist at all.
func TestDiagnosticIsInvisibleToConnectClients(t *testing.T) {
	f := newFleet(t)

	for _, tc := range []struct {
		name string
		opts []connect.ClientOption
		want string
	}{
		{"connect", nil, "unimplemented: 404 Not Found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := f.callAndCatch(t, "paymnets.internal:9777", tc.opts...)
			t.Logf("%s caller sees: %v", tc.name, err)

			if strings.Contains(err.Error(), "paymnets.internal") {
				t.Fatalf("the diagnostic now reaches a %s caller (%v) — issue #23 is fixed, delete this pin", tc.name, err)
			}
			if got := err.Error(); got != tc.want {
				t.Errorf("%s caller sees %q, pinned as %q", tc.name, got, tc.want)
			}
		})
	}
}

// TestConnectEnvelopeMakesTheDiagnosticVisible demonstrates the remedy for
// the gap above, so #17 can decide on evidence rather than on argument.
//
// Writing the same text into the Connect error envelope — JSON content type,
// explicit code — costs the proxy nothing it does not already know. It reads
// the request's framing, not its payload, so voya still holds no descriptors
// and decodes no messages (ADR 003).
//
// Both directions are covered, because the remedy is only worth adopting if
// it hits every Connect caller and no one else. The subtests below are the two
// halves: which requests must get the envelope, and which must not.
func TestConnectEnvelopeMakesTheDiagnosticVisible(t *testing.T) {
	f := newFleetWithDiagnostics(t, connectEnvelope)

	// Both shapes a Connect unary caller takes. The GET is what a
	// side-effect-free procedure sends under WithHTTPGet: no content type at
	// all, the protocol marker moved to a connect=v1 query parameter. A
	// content-type check misses it, and the caller is left with the bare
	// "unimplemented: 404" this whole demonstration exists to remove.
	for _, tc := range []struct {
		name string
		opts []connect.ClientOption
	}{
		{"post", nil},
		{"get", []connect.ClientOption{
			connect.WithHTTPGet(),
			connect.WithIdempotency(connect.IdempotencyNoSideEffects),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := f.callAndCatch(t, "paymnets.internal:9777", tc.opts...)
			t.Logf("connect caller sees:\n%v", err)

			if code := connect.CodeOf(err); code != connect.CodeUnavailable {
				t.Errorf("code %v, want %v", code, connect.CodeUnavailable)
			}
			mustContain(t, err.Error(),
				`"paymnets.internal"`,
				"Nothing has ever registered",
				"Registered services: billing.internal, payments.internal",
			)
		})
	}

	// A Connect caller that sent no authority at all, which is the one
	// routing failure that is not about a service name. Nothing can route it
	// and no retry will change that, so the envelope must not report it as
	// unavailable — a caller with a retry policy would loop on a request that
	// can never succeed. This is also what pins that the status the routing
	// decision chose is carried into the envelope rather than discarded.
	t.Run("no authority is not retryable", func(t *testing.T) {
		status, body, err := rawH2Request(t, f.proxyAddr, []hpack.HeaderField{
			{Name: ":method", Value: "GET"},
			{Name: ":path", Value: procAuthority + "?connect=v1&encoding=proto"},
			{Name: ":scheme", Value: "http"},
		})
		if err != nil {
			t.Fatalf("raw HTTP/2 request: %v", err)
		}
		t.Logf("Connect GET with no :authority -> HTTP %s\n%s", status, body)
		if status != "400" {
			t.Errorf("status %s, want 400 — a permanently malformed request was reported as retryable", status)
		}
		mustContain(t, body, `"invalid_argument"`, "no :authority")
	})

	// The other half, and the one the remedy is only worth adopting if it
	// gets right: which requests must *not* be claimed. Each case below
	// carries some part of Connect unary's shape and not the rest, and each
	// is actively harmed by a JSON envelope — a human loses the readable
	// body and the 404/503 distinction ADR 005 draws, and a streaming client
	// gets a bare JSON object where its reader expects a length-prefixed
	// end-of-stream frame.
	const typo = "paymnets.internal:9777"
	for _, tc := range []struct {
		name string
		req  func(*testing.T) *http.Request
	}{{
		// A human with curl: no protocol marker anywhere.
		name: "curl",
		req: func(t *testing.T) *http.Request {
			return newRequest(t, http.MethodGet, "http://"+typo+procAuthority, "", nil)
		},
	}, {
		// A browser fetch or an inbound webhook. It carries Connect unary's
		// content type and none of its protocol, which is exactly the case
		// that tells a content-type check apart from a protocol check.
		name: "non-Connect JSON post",
		req: func(t *testing.T) *http.Request {
			return newRequest(t, http.MethodPost, "http://"+typo+"/webhook", `{"event":"ping"}`,
				map[string]string{"Content-Type": "application/json"})
		},
	}, {
		// A URL that happens to carry ?connect=v1 and none of the rest of
		// the GET form. `connect` is an ordinary query parameter name.
		name: "stray connect=v1 query parameter",
		req: func(t *testing.T) *http.Request {
			return newRequest(t, http.MethodGet, "http://"+typo+"/page?connect=v1", "", nil)
		},
	}, {
		// Streaming Connect: the version marker is there, but the outcome
		// belongs in an end-of-stream envelope, not a JSON body.
		name: "streaming Connect post",
		req: func(t *testing.T) *http.Request {
			return newRequest(t, http.MethodPost, "http://"+typo+procAuthority, "", map[string]string{
				"Content-Type":             "application/connect+proto",
				"Connect-Protocol-Version": "1",
			})
		},
	}, {
		// The marker with no content type at all. Not a unary call anything
		// can recognise, so it falls back to the body a human can read
		// rather than a frame nothing can parse.
		name: "connect marker, no content type",
		req: func(t *testing.T) *http.Request {
			return newRequest(t, http.MethodPost, "http://"+typo+procAuthority, "x",
				map[string]string{"Connect-Protocol-Version": "1"})
		},
	}, {
		// Same, with a content type that does not parse.
		name: "connect marker, unparseable content type",
		req: func(t *testing.T) *http.Request {
			return newRequest(t, http.MethodPost, "http://"+typo+procAuthority, "x", map[string]string{
				"Content-Type":             "application/json; charset",
				"Connect-Protocol-Version": "1",
			})
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			status, contentType, body := f.roundTrip(t, tc.req(t))
			if status != http.StatusNotFound {
				t.Errorf("status %d, want %d — the envelope claimed a caller that is not Connect unary", status, http.StatusNotFound)
			}
			if !strings.HasPrefix(contentType, "text/plain") {
				t.Errorf("content type %q, want text/plain", contentType)
			}
			mustContain(t, body, "Nothing has ever registered")
		})
	}

	// gRPC and gRPC-Web carry no Connect version marker, so the unary
	// envelope leaves them alone. That is correct — their errors live in
	// trailers, and the real proxy answers them with a Trailers-Only
	// response (#25). This spike demonstrates only the Connect half, so here
	// they stay blind to the diagnostic; the pin is that the envelope cannot
	// quietly claim a trailers-based caller.
	for _, tc := range []struct {
		name string
		opts []connect.ClientOption
	}{
		{"grpc", []connect.ClientOption{connect.WithGRPC()}},
		{"grpc-web", []connect.ClientOption{connect.WithGRPCWeb()}},
	} {
		t.Run(tc.name+" is untouched", func(t *testing.T) {
			err := f.callAndCatch(t, typo, tc.opts...)
			t.Logf("%s caller sees: %v", tc.name, err)
			if want := "unimplemented: HTTP status 404 Not Found"; err.Error() != want {
				t.Errorf("%s caller sees %q, want %q — the unary envelope reached a trailers-based protocol", tc.name, err.Error(), want)
			}
		})
	}
}

// TestRoutingKeysOnTheHostnameAlone: the port in the authority is the
// caller's business, not a routing input. voya listens on one port, and a
// service is the same service however the caller wrote the dial string.
func TestRoutingKeysOnTheHostnameAlone(t *testing.T) {
	f := newFleet(t)

	for _, authority := range []string{
		"payments.internal:9777",
		"payments.internal:8080",
		"payments.internal",
		"PAYMENTS.INTERNAL:9777",
		"Payments.Internal:9777",
		"payments.internal.:9777",
	} {
		t.Run(authority, func(t *testing.T) {
			before := f.payments.count()
			got, err := f.call(t, authority)
			if err != nil {
				t.Fatalf("call to %q: %v", authority, err)
			}
			if f.payments.count() != before+1 {
				t.Errorf("request for %q did not reach payments", authority)
			}
			// Whatever the caller wrote is what the service sees. Routing
			// normalises; the authority itself is passed through untouched.
			if got != authority {
				t.Errorf("backend saw authority %q, want %q verbatim", got, authority)
			}
		})
	}
}

// TestEmptyAuthorityOverRawHTTP2 covers a client that sends no :authority at
// all. Go's own client always sets one, so this is hand-built frames — the
// shape a non-Go client or a hand-rolled tool can produce.
//
// What this pins is where the request dies: if net/http rejects it before the
// handler runs, voya cannot diagnose it however good its error messages are,
// and that is worth knowing before ADR 005's promises are written into a CLI.
func TestEmptyAuthorityOverRawHTTP2(t *testing.T) {
	f := newFleet(t)

	status, body, err := rawH2Request(t, f.proxyAddr, []hpack.HeaderField{
		{Name: ":method", Value: "GET"},
		{Name: ":path", Value: procAuthority},
		{Name: ":scheme", Value: "http"},
		// No :authority, and no Host header either.
	})
	if err != nil {
		t.Fatalf("raw HTTP/2 request: %v", err)
	}

	t.Logf("no :authority -> HTTP %s\n%s", status, body)
	if status != "400" {
		t.Errorf("status %s, want 400 — an unroutable request should be answered, not dropped", status)
	}
	mustContain(t, body,
		"no :authority",
		"voya routes by service name",
		"Registered services: billing.internal, payments.internal",
	)
	if n := f.payments.count() + f.billing.count(); n != 0 {
		t.Errorf("a request with no authority reached a backend %d time(s)", n)
	}
}

// TestMissingHostOverHTTP11 is the same gap through the HTTP/1 leg, which is
// what `curl --http1.0` produces. The proxy serves both protocols on one
// port, so both need an answer.
func TestMissingHostOverHTTP11(t *testing.T) {
	f := newFleet(t)

	conn, err := net.DialTimeout("tcp", f.proxyAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	// HTTP/1.0 predates the mandatory Host header, so this is a legal request
	// with nothing to route on.
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.0\r\n\r\n", procAuthority); err != nil {
		t.Fatalf("write request: %v", err)
	}
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)

	t.Logf("HTTP/1.0 with no Host -> %s\n%s", res.Status, body)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want %d", res.StatusCode, http.StatusBadRequest)
	}
	mustContain(t, string(body), "no :authority", "Registered services:")
}

// TestRegisteredButUnreachable covers a registration that outlived its
// process: the table says the service is running, the connection says
// otherwise. The proxy's ErrorHandler is the last place this can be explained
// rather than surfacing as a bare 502.
func TestRegisteredButUnreachable(t *testing.T) {
	// Bind and immediately release a port, so the address is well-formed and
	// certain to refuse connections.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	_ = ln.Close()

	reg := &registry{
		upstream: map[string]string{"payments.internal": dead},
		declared: []string{"payments.internal"},
	}
	proxyAddr := serve(t, newProxy(reg))

	// The OS is free to hand a just-freed ephemeral port straight back. If it
	// gave this one to the proxy, the registry now points payments.internal
	// at the proxy itself: the request loops through it, opening a fresh
	// connection per hop until the client's timeout, and the test fails as a
	// timeout rather than as the 502 it is checking for.
	if proxyAddr == dead {
		t.Skipf("the proxy was assigned %s, the port freed for the dead upstream — nothing to fail against", dead)
	}

	req, err := http.NewRequestWithContext(testContext(t), http.MethodGet, "http://payments.internal:9777"+procAuthority, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := voyaClient(t, proxyAddr).Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)

	t.Logf("registered but unreachable -> %s\n%s", res.Status, body)
	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("status %d, want %d", res.StatusCode, http.StatusBadGateway)
	}
	mustContain(t, string(body), "payments.internal:9777", "is registered but the connection failed")
}

// rawH2Request speaks HTTP/2 by hand so a request can omit pseudo-headers
// that no Go client will omit for you. golang.org/x/net/http2 appears here
// for its framer only — the servers and clients under test are stdlib h2c.
func rawH2Request(t *testing.T, addr string, headers []hpack.HeaderField) (status, body string, err error) {
	t.Helper()

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return "", "", fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	if _, err := io.WriteString(conn, http2.ClientPreface); err != nil {
		return "", "", fmt.Errorf("preface: %w", err)
	}
	fr := http2.NewFramer(conn, conn)
	if err := fr.WriteSettings(); err != nil {
		return "", "", fmt.Errorf("settings: %w", err)
	}

	var block bytes.Buffer
	enc := hpack.NewEncoder(&block)
	for _, h := range headers {
		if err := enc.WriteField(h); err != nil {
			return "", "", fmt.Errorf("hpack: %w", err)
		}
	}
	if err := fr.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      1,
		BlockFragment: block.Bytes(),
		EndStream:     true,
		EndHeaders:    true,
	}); err != nil {
		return "", "", fmt.Errorf("headers: %w", err)
	}

	dec := hpack.NewDecoder(4096, nil)
	var payload bytes.Buffer
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			return status, payload.String(), fmt.Errorf("read frame: %w", err)
		}
		switch f := f.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				_ = fr.WriteSettingsAck()
			}
		case *http2.HeadersFrame:
			fields, err := dec.DecodeFull(f.HeaderBlockFragment())
			if err != nil {
				return status, payload.String(), fmt.Errorf("decode headers: %w", err)
			}
			for _, hf := range fields {
				if hf.Name == ":status" {
					status = hf.Value
				}
			}
			if f.StreamEnded() {
				return status, payload.String(), nil
			}
		case *http2.DataFrame:
			payload.Write(f.Data())
			if f.StreamEnded() {
				return status, payload.String(), nil
			}
		case *http2.RSTStreamFrame:
			// The server rejected the stream outright: the handler never ran,
			// so no diagnostic of ours could have reached the caller.
			return status, payload.String(), fmt.Errorf("server sent RST_STREAM (%v) — request rejected below the handler", f.ErrCode)
		case *http2.GoAwayFrame:
			return status, payload.String(), fmt.Errorf("server sent GOAWAY (%v): %s", f.ErrCode, f.DebugData())
		}
	}
}
