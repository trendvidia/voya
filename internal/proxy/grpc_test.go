package proxy

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

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/trendvidia/voya/internal/registry"
)

// The gRPC wire protocol carries a call's outcome in HTTP/2 trailers, which
// is the hardest thing for a proxy to get right and the risk ADR 003 was
// written around. #17 requires the trailer cases to pass against this proxy.
//
// These use connect-go's gRPC protocol client rather than grpc-go: the wire
// format under test is the same, and it keeps the root module free of a
// direct grpc-go dependency. test/trailer is the second implementation — the
// version-bound evidence taken with grpc-go itself, which is what grpcurl and
// every non-Go consumer speaks. Its proxyInFront now builds this proxy, and
// the deprecated x/net/http2/h2c import is gone with it (#21 is discharged;
// see ADR 003's amendment of 2026-08-10).

const procedureFail = "/voya.test.v1.TrailerService/Fail"
const procedureOK = "/voya.test.v1.TrailerService/OK"

// trailerBackend answers one procedure with an error carrying a code and
// metadata, and one successfully with trailer metadata.
func trailerBackend() http.Handler {
	mux := http.NewServeMux()

	mux.Handle(procedureFail, connect.NewUnaryHandler(procedureFail,
		func(_ context.Context, _ *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
			err := connect.NewError(connect.CodeNotFound, errors.New("no such record"))
			err.Meta().Set("Trailer-Detail", "from-the-backend")
			return nil, err
		}))

	mux.Handle(procedureOK, connect.NewUnaryHandler(procedureOK,
		func(_ context.Context, _ *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
			resp := connect.NewResponse(&emptypb.Empty{})
			resp.Trailer().Set("Trailer-Detail", "from-the-backend")
			return resp, nil
		}))

	return mux
}

// wireProtocols are the two that carry an outcome out of band: gRPC in HTTP/2
// trailers, gRPC-Web in a trailers frame appended to the body. They are
// different code paths through the proxy, and the changelog claims both.
var wireProtocols = []struct {
	name string
	opt  connect.ClientOption
}{
	{"grpc", connect.WithGRPC()},
	{"grpc-web", connect.WithGRPCWeb()},
}

// wireTransports are those two protocols over the three transports voya has
// to answer on. The third row is the browser's shape: gRPC-Web exists for
// callers that cannot speak prior-knowledge h2c, so the same response has to
// read as Trailers-Only over HTTP/1.1 — where it is headers, a 200 and no
// body.
//
// One declaration because a diagnostic test that dropped a row would still
// pass, silently answering for two transports instead of three: the copy the
// reserved-authority test started as had already lost the assertion that its
// third row ran on HTTP/1.1 at all.
var wireTransports = []struct {
	name   string
	opt    connect.ClientOption
	h2Only bool
}{
	{"grpc", connect.WithGRPC(), true},
	{"grpc-web", connect.WithGRPCWeb(), true},
	{"grpc-web over http/1.1", connect.WithGRPCWeb(), false},
}

// bothPaths runs fn against the backend directly and through the proxy, for
// each wire protocol, mirroring test/trailer's structure. The direct leg is
// the control: if it fails, the problem is the fixture rather than the proxy
// — which is the difference between "trailers are broken" and "this test
// never worked".
func bothPaths(t *testing.T, procedure string, fn func(t *testing.T, c *connect.Client[emptypb.Empty, emptypb.Empty])) {
	t.Helper()
	reg := registry.New()
	backendPort := backend(t, trailerBackend())
	if _, err := reg.Register(registry.Instance{Service: "payments", Port: backendPort}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	proxyAddr := serveProxy(t, reg)
	backendAddr := net.JoinHostPort("127.0.0.1", strconv.FormatUint(uint64(backendPort), 10))

	for _, proto := range wireProtocols {
		t.Run(proto.name, func(t *testing.T) {
			t.Run("direct", func(t *testing.T) {
				fn(t, grpcClientTo(t, backendAddr, "http://"+backendAddr+procedure, proto.opt))
			})
			t.Run("proxied", func(t *testing.T) {
				fn(t, grpcClientTo(t, proxyAddr, "http://payments.internal:9777"+procedure, proto.opt))
			})
		})
	}
}

// grpcClientTo dials dialAddr while addressing url, so the proxied leg can
// carry the .internal authority the proxy routes on.
func grpcClientTo(t *testing.T, dialAddr, url string, protocol connect.ClientOption) *connect.Client[emptypb.Empty, emptypb.Empty] {
	t.Helper()
	return connect.NewClient[emptypb.Empty, emptypb.Empty](
		client(t, dialAddr, true), // both legs h2c, as the proxy's upstream is
		url,
		protocol,
	)
}

// TestGRPCErrorCodeSurvivesTheHop is test/trailer's first case, the one that
// matters most: if trailers are dropped the client sees a stream that ended
// without a status and reports Internal or Unavailable, so a NOT_FOUND reads
// as a broken service rather than a missing record.
func TestGRPCErrorCodeSurvivesTheHop(t *testing.T) {
	bothPaths(t, procedureFail, func(t *testing.T, c *connect.Client[emptypb.Empty, emptypb.Empty]) {
		_, err := c.CallUnary(context.Background(), connect.NewRequest(&emptypb.Empty{}))
		if err == nil {
			t.Fatal("the call succeeded; it must fail with NotFound")
		}
		if got := connect.CodeOf(err); got != connect.CodeNotFound {
			t.Errorf("code = %v, want not_found — trailers did not survive: %v", got, err)
		}
		if !strings.Contains(err.Error(), "no such record") {
			t.Errorf("the message did not survive: %v", err)
		}
	})
}

// TestGRPCErrorMetadataSurvivesTheHop: metadata set on the error rides in the
// same trailers as the status.
func TestGRPCErrorMetadataSurvivesTheHop(t *testing.T) {
	bothPaths(t, procedureFail, func(t *testing.T, c *connect.Client[emptypb.Empty, emptypb.Empty]) {
		_, err := c.CallUnary(context.Background(), connect.NewRequest(&emptypb.Empty{}))
		if err == nil {
			t.Fatal("the call succeeded")
		}
		var connectErr *connect.Error
		if !errors.As(err, &connectErr) {
			t.Fatalf("error is %T, want *connect.Error: %v", err, err)
		}
		if got := connectErr.Meta().Get("Trailer-Detail"); got != "from-the-backend" {
			t.Errorf("Trailer-Detail = %q, want %q", got, "from-the-backend")
		}
	})
}

// TestGRPCTrailersOnSuccessSurviveTheHop: a successful call's trailers are
// the same mechanism, and a proxy can drop them while every error test still
// passes.
func TestGRPCTrailersOnSuccessSurviveTheHop(t *testing.T) {
	bothPaths(t, procedureOK, func(t *testing.T, c *connect.Client[emptypb.Empty, emptypb.Empty]) {
		resp, err := c.CallUnary(context.Background(), connect.NewRequest(&emptypb.Empty{}))
		if err != nil {
			t.Fatalf("CallUnary: %v", err)
		}
		if got := resp.Trailer().Get("Trailer-Detail"); got != "from-the-backend" {
			t.Errorf("Trailer-Detail = %q, want %q — success trailers were dropped", got, "from-the-backend")
		}
	})
}

// TestGRPCReachesTheRightServiceByAuthority: routing and trailers together,
// since gRPC's :authority is the routing key as well as the callee's name.
func TestGRPCRoutesByAuthority(t *testing.T) {
	reg := registry.New()
	registered(t, reg, "payments", trailerBackend())
	proxyAddr := serveProxy(t, reg)

	// An authority nothing has registered gets the diagnostic path, not a
	// route. It surfaces as a Trailers-Only error carrying the diagnostic;
	// the tests below cover what that error says.
	unknown := connect.NewClient[emptypb.Empty, emptypb.Empty](
		client(t, proxyAddr, true),
		"http://paymnets.internal:9777"+procedureOK,
		connect.WithGRPC(),
	)
	if _, err := unknown.CallUnary(context.Background(), connect.NewRequest(&emptypb.Empty{})); err == nil {
		t.Error("a call to an unregistered authority succeeded")
	}

	// And the registered one still works, so the failure above is routing
	// rather than the harness.
	known := grpcClientTo(t, proxyAddr, "http://payments.internal:9777"+procedureOK, connect.WithGRPC())
	if _, err := known.CallUnary(context.Background(), connect.NewRequest(&emptypb.Empty{})); err != nil {
		t.Errorf("the registered authority failed: %v", err)
	}
}

// --- the Trailers-Only diagnostic (ADR 005's amendment, gRPC half) -----------

// TestDiagnosticReachesGRPCCallers is the amendment's point for the two
// trailers-based protocols: a gRPC or gRPC-Web caller reads a call's outcome
// from grpc-status/grpc-message and nowhere else, so a plain-text 404 reached
// them as "unimplemented: HTTP status 404 Not Found" — a transport failure
// with the diagnostic thrown away. The Trailers-Only response is what makes
// it arrive.
//
// The assertions include every line of the diagnostic, because grpc-message
// is percent-encoded and voya's text is multi-line: an encoder that stopped
// at the first newline would still deliver the code and the opening line
// while silently truncating the part that says what would have worked.
func TestDiagnosticReachesGRPCCallers(t *testing.T) {
	reg := registry.New()
	registered(t, reg, "payments", trailerBackend())
	proxyAddr := serveProxy(t, reg)

	for _, proto := range wireTransports {
		t.Run(proto.name, func(t *testing.T) {
			httpClient := client(t, proxyAddr, proto.h2Only)
			seen := &protoSeen{next: httpClient.Transport}
			httpClient.Transport = seen

			c := connect.NewClient[emptypb.Empty, emptypb.Empty](
				httpClient,
				"http://paymnets.internal:9777"+procedureOK,
				proto.opt,
			)
			_, err := c.CallUnary(context.Background(), connect.NewRequest(&emptypb.Empty{}))
			if err == nil {
				t.Fatal("a call to an unregistered authority succeeded")
			}
			t.Logf("%s caller sees: %v", proto.name, err)

			// Which transport ran is the third row's entire subject, and it is
			// chosen for us: Transport.Protocols takes prior-knowledge h2c
			// only when HTTP1 is absent from the set. Unasserted, a change to
			// that preference would leave this row passing as a second h2c run.
			wantProto := "HTTP/1.1"
			if proto.h2Only {
				wantProto = "HTTP/2.0"
			}
			if got := seen.proto(); got != wantProto {
				t.Errorf("the exchange ran on %s, want %s", got, wantProto)
			}

			if got := connect.CodeOf(err); got != connect.CodeUnavailable {
				t.Errorf("code = %v, want unavailable — a name that does not resolve to an endpoint", got)
			}
			for _, want := range []string{
				`voya: no service named "paymnets"`,
				"Nothing has ever registered that name.",
				`Authority received: "paymnets.internal:9777"`,
				"Registered services: payments.internal",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the diagnostic omits %q:\n%v", want, err)
				}
			}
			if !strings.Contains(err.Error(), "\n") {
				t.Errorf("the newlines did not survive percent-encoding:\n%v", err)
			}
		})
	}
}

// protoSeen notes the HTTP version a response arrived on. connect-go hands
// back an error rather than the *http.Response, so the transport is where a
// test can still see which protocol carried the exchange.
type protoSeen struct {
	next http.RoundTripper
	mu   sync.Mutex
	last string
}

func (p *protoSeen) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := p.next.RoundTrip(r)
	if resp != nil {
		p.mu.Lock()
		p.last = resp.Proto
		p.mu.Unlock()
	}
	return resp, err
}

func (p *protoSeen) proto() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

// TestTrailersOnlyShapeOnTheWire pins the framing underneath the test above:
// grpc-status and grpc-message ride in the response HEADERS with HTTP 200 and
// an empty body, not in HTTP trailers after one. Go's client sorts the two
// apart — a field sent as a trailer lands in resp.Trailer — so this is the
// assertion that voya wrote Trailers-Only rather than a shape connect-go
// happens to also accept.
//
// The reserved row is #69's, and this is the only place its 12 is read off
// the wire — likewise #88's 3 for a name that can never register and 9 for
// the routing loop. A gRPC client's own code is not evidence for any of
// them: connect-go maps a bare HTTP 404 to unimplemented as well — the
// mapping the #25 fragment records as the reason Trailers-Only was needed at
// all — so a regression that answered plain text would still read as
// unimplemented at [TestReservedAuthorityIsUnimplementedNotUnavailable].
// Here it could not: the status, the content type and the empty body are all
// wrong for it.
func TestTrailersOnlyShapeOnTheWire(t *testing.T) {
	for _, tc := range []struct {
		name      string
		authority string
		// register, when set, puts the fixture the row needs into the
		// registry once the proxy's own address is known — the loop row
		// registers that very address.
		register func(t *testing.T, reg *registry.Registry, proxyAddr string)
		want     string
	}{
		{name: "a name nothing registered", authority: "paymnets.internal:9777", want: "14"},
		{name: "a name that can never register", authority: "api.payments.internal:9777", want: "3"},
		{name: "the reserved authority", authority: registry.Authority(registry.Reserved) + ":9777", want: "12"},
		{name: "a service registered on voya's own port", authority: "loop.internal:9777",
			register: func(t *testing.T, reg *registry.Registry, proxyAddr string) {
				if _, err := reg.Register(registry.Instance{Service: "loop", Port: portOfAddr(t, proxyAddr)}); err != nil {
					t.Fatalf("Register: %v", err)
				}
			}, want: "9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := registry.New()
			proxyAddr := serveProxy(t, reg)
			if tc.register != nil {
				tc.register(t, reg, proxyAddr)
			}
			c := client(t, proxyAddr, true)

			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
				"http://"+tc.authority+procedureOK, strings.NewReader(""))
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			req.Header.Set("Content-Type", "application/grpc")

			resp, err := c.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("reading body: %v", err)
			}

			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200 — gRPC carries the outcome in trailers, never the HTTP status", resp.StatusCode)
			}
			if got := resp.Header.Get("Content-Type"); got != "application/grpc" {
				t.Errorf("Content-Type = %q, want the caller's own", got)
			}
			if got := resp.Header.Get("Grpc-Status"); got != tc.want {
				t.Errorf("grpc-status = %q in the headers, want %s; trailers held %q",
					got, tc.want, resp.Trailer.Get("Grpc-Status"))
			}
			if got := resp.Header.Get("Grpc-Message"); !strings.Contains(got, "%0A") {
				t.Errorf("grpc-message = %q, want the newlines percent-encoded as %%0A", got)
			}
			if len(body) != 0 {
				t.Errorf("a Trailers-Only response carries no body; got %d byte(s): %q", len(body), body)
			}
		})
	}
}

// TestNoAuthorityIsInvalidArgumentForGRPC mirrors the Connect envelope's
// rule: nothing supplies a missing authority, so a caller with a retry policy
// must not be told the failure is retryable.
func TestNoAuthorityIsInvalidArgumentForGRPC(t *testing.T) {
	reg := registry.New()
	handler := New(reg)

	req := httptest.NewRequest(http.MethodPost, "http://placeholder/svc/Method", nil)
	req.Host = ""
	req.Header.Set("Content-Type", "application/grpc")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Grpc-Status"); got != "3" {
		t.Errorf("grpc-status = %q, want 3 (invalid_argument): no retry supplies a missing authority", got)
	}
}

// TestReservedAuthorityIsUnimplementedNotUnavailable is #69, over the two
// protocols that read a code rather than the message — across the three
// transports they reach the one writer through.
//
// voya.internal is ADR 005's reserved diagnostics authority: validName
// guarantees nothing can ever register it, and since #149 the daemon serves
// DiagService there and nothing else — so the pin is aimed at a procedure it
// does not have, on a proxy with the internal handler mounted, and it is the
// dispatch that has to leave the row standing. It used to answer unavailable,
// which is the retryable code — so a caller with a retry policy looped on a
// request no retry can ever satisfy, the exact failure ADR 005 names when it
// argues the no-authority case must be invalid_argument. The proxy's own
// message already said "reserved, and this build does not serve it yet"; the
// code said "try again".
//
// The wire form is [TestTrailersOnlyShapeOnTheWire]'s reserved row, and it is
// the assertion that means something here: connect-go reads a bare HTTP 404
// as unimplemented too — the #25 changelog fragment records that mapping as
// the reason gRPC callers needed Trailers-Only in the first place — so
// CodeOf alone would survive a regression to a plain-text 404.
func TestReservedAuthorityIsUnimplementedNotUnavailable(t *testing.T) {
	reg := registry.New()
	proxyAddr := serveProxy(t, reg, WithInternalHandler(internalStub()))
	authority := registry.Authority(registry.Reserved) + ":9777"

	for _, proto := range wireTransports {
		t.Run(proto.name, func(t *testing.T) {
			httpClient := client(t, proxyAddr, proto.h2Only)
			seen := &protoSeen{next: httpClient.Transport}
			httpClient.Transport = seen

			c := connect.NewClient[emptypb.Empty, emptypb.Empty](
				httpClient,
				"http://"+authority+"/voya.diag.v1.DiagService/NoSuchProcedure",
				proto.opt,
			)
			_, err := c.CallUnary(context.Background(), connect.NewRequest(&emptypb.Empty{}))
			if err == nil {
				t.Fatal("a call to a procedure the reserved authority does not have succeeded")
			}

			// Which transport ran is the third row's entire subject, and it is
			// chosen for us rather than asked for: unasserted, a change to
			// net/http's preference would leave that row passing as a second
			// h2c run and the browser's shape untested.
			wantProto := "HTTP/1.1"
			if proto.h2Only {
				wantProto = "HTTP/2.0"
			}
			if got := seen.proto(); got != wantProto {
				t.Errorf("the exchange ran on %s, want %s", got, wantProto)
			}

			if got := connect.CodeOf(err); got != connect.CodeUnimplemented {
				t.Errorf("code = %v, want unimplemented — %s serves DiagService and has no such "+
					"procedure, so no retry can ever satisfy the call", got, authority)
			}
			// The message and the code have to agree. Before #69 they did
			// not, and the message was the half that was right.
			if !strings.Contains(err.Error(), "reserved") {
				t.Errorf("the diagnostic does not say the name is reserved: %v", err)
			}
		})
	}
}

// TestCodeForHasARowForEveryOutcome pins the whole table in one place, which
// is the point of there being a table (#69) — keyed, since #80, on the
// routing outcome itself rather than the HTTP status a call site had encoded
// it as.
//
// The row is compared as a whole rather than field by field. The four fields
// are written from one switch and read by four different consumers — the
// plain-text response's own status, grpc-status, the Connect envelope's
// code, and the HTTP status that envelope rides on — so a row that changed
// one and left the others would tell a retry policy and a transport
// different things. Field-by-field checks left the envelope status
// unasserted once already: giving the no-authority row
// `status: http.StatusServiceUnavailable` passed the whole package, which
// would have shipped an invalid_argument envelope under a retryable 503.
//
// Retryability is why each row is what it is, so it is said in the row's
// name rather than asserted separately: every field it could be computed
// from is pinned right here, so a check derived from one of them could not
// fail on its own.
//
// The sweep to [numOutcomes] is what retired TestEveryStatusDiagnoseWritesHasARow's
// source scan (#80). A Go switch is not exhaustive to the compiler, so the
// sweep walks every value the type names and asserts rowFor knows it — in
// the test run of whoever added the outcome, rather than on a caller's
// request against codeFor's logged internal fallback. It asserts ok as a
// normal failure because the pre-#91 shape failed by reaching codeFor's
// panic, which aborted the whole package binary and hid every other failure
// in it. The routing-loop and unreachable rows answered alike until #88
// split them — the split #80 kept them distinct outcomes for — and each row
// here pins that its outcome stays its own rather than an alias of another.
//
// What the sweep does not cover, and the deleted scan did: the *call sites*.
// diagnose's parameter is a defined integer type, so an untyped constant
// still converts — p.diagnose(w, r, http.StatusNotFound, …) compiles and
// reaches the fallback. #91 weighed closing that at the type and declined;
// what a request down that path now gets is
// [TestAnUnmappedOutcomeAnswersInternalAndLogs]'s subject.
func TestCodeForHasARowForEveryOutcome(t *testing.T) {
	rows := map[outcome]struct {
		what string
		want codes
	}{
		outcomeNoAuthority: {
			"no authority at all — never retryable",
			codes{routing: http.StatusBadRequest, grpc: "3", connect: "invalid_argument", envelope: http.StatusBadRequest},
		},
		outcomeNotAName: {
			"not a name voya can ever route — never retryable",
			codes{routing: http.StatusBadRequest, grpc: "3", connect: "invalid_argument", envelope: http.StatusBadRequest},
		},
		outcomeNeverRegistered: {
			"never registered — retryable",
			codes{routing: http.StatusNotFound, grpc: "14", connect: "unavailable", envelope: http.StatusServiceUnavailable},
		},
		outcomeReserved: {
			"the reserved authority — never retryable",
			codes{routing: http.StatusNotImplemented, grpc: "12", connect: "unimplemented", envelope: http.StatusNotImplemented},
		},
		outcomeRoutingLoop: {
			"registered on voya's own port — not retryable until the registration is fixed",
			codes{routing: http.StatusBadRequest, grpc: "9", connect: "failed_precondition", envelope: http.StatusBadRequest},
		},
		outcomeUnreachable: {
			"registered but unreachable — retryable",
			codes{routing: http.StatusBadGateway, grpc: "14", connect: "unavailable", envelope: http.StatusServiceUnavailable},
		},
		outcomeKnownNotRunning: {
			"known, not running — retryable",
			codes{routing: http.StatusServiceUnavailable, grpc: "14", connect: "unavailable", envelope: http.StatusServiceUnavailable},
		},
		outcomeRateLimited: {
			"over the rate limit — retryable after Retry-After",
			codes{routing: http.StatusTooManyRequests, grpc: "8", connect: "resource_exhausted", envelope: http.StatusTooManyRequests},
		},
	}

	for i := range numOutcomes {
		oc := outcome(i)
		row, ok := rows[oc]
		if !ok {
			// Errorf, not Fatalf: this test is meant to be the complete
			// inventory, so two unpinned outcomes should both be named in
			// one run rather than one per round trip — and Fatalf would
			// skip the count below, the half that catches the other
			// direction.
			t.Errorf("outcome %d has no row in this test: a new outcome pins its row here first", oc)
			continue
		}
		t.Run(row.what, func(t *testing.T) {
			got, ok := rowFor(oc)
			if !ok {
				t.Fatalf("rowFor(%d) has no row: a caller would get the logged internal fallback", oc)
			}
			if got != row.want {
				t.Errorf("rowFor(%d) = %+v, want %+v", oc, got, row.want)
			}
		})
	}
	// The sweep visits every value below numOutcomes; this catches a row the
	// sweep never visits — one keyed at or past the bound, which is what a
	// misplaced numOutcomes or an explicitly-valued constant leaves behind.
	// A row keyed by a constant the type no longer names is not this: that
	// is a compile error in this file, and never reaches a run.
	if len(rows) != numOutcomes {
		t.Errorf("this test pins %d rows but the type has %d outcomes", len(rows), numOutcomes)
	}
}

// TestAnUnmappedOutcomeAnswersInternalAndLogs is #91's decision: an outcome
// rowFor does not name — a new constant missing its case, or a stale call
// site handing diagnose an untyped http.Status* constant, which still
// compiles — answers the non-retryable internal row and is named on the
// proxy's log, rather than panicking mid-handler into a reset stream with
// nothing attached. Internal and not a retryable code, because an unmapped
// outcome is voya's own bug and retryability is precisely what nobody
// decided (#69's loop, otherwise).
func TestAnUnmappedOutcomeAnswersInternalAndLogs(t *testing.T) {
	var logged []string
	p := &proxy{logf: func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}}

	// The setup, asserted: outcome(numOutcomes) is one past the last named
	// value, so it must have no row — otherwise this test exercises the
	// mapped path twice and proves nothing about the fallback.
	unmapped := outcome(numOutcomes)
	if _, ok := rowFor(unmapped); ok {
		t.Fatalf("rowFor(%d) has a row; the fallback needs an unmapped outcome to be observable", unmapped)
	}

	want := codes{routing: http.StatusInternalServerError, grpc: "13", connect: "internal", envelope: http.StatusInternalServerError}
	if got := p.codeFor(unmapped); got != want {
		t.Errorf("codeFor(%d) = %+v, want the internal row %+v", unmapped, got, want)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], strconv.Itoa(int(unmapped))) {
		t.Errorf("the unmapped outcome is not named on the log: %q", logged)
	}

	// The control: a mapped outcome answers its own row and says nothing.
	logged = nil
	if got := p.codeFor(outcomeNoAuthority); got.connect != "invalid_argument" {
		t.Errorf("codeFor(outcomeNoAuthority).connect = %q, want invalid_argument", got.connect)
	}
	if len(logged) != 0 {
		t.Errorf("a mapped outcome reached the fallback's log line: %q", logged)
	}
}

func TestIsGRPC(t *testing.T) {
	post := func(contentType string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "http://h/s/M", nil)
		r.Header.Set("Content-Type", contentType)
		return r
	}
	cases := []struct {
		name   string
		build  func() *http.Request
		expect bool
	}{
		{"grpc", func() *http.Request { return post("application/grpc") }, true},
		{"grpc with codec", func() *http.Request { return post("application/grpc+proto") }, true},
		{"grpc-web", func() *http.Request { return post("application/grpc-web") }, true},
		{"grpc-web with codec", func() *http.Request { return post("application/grpc-web+proto") }, true},
		{"grpc-web-text", func() *http.Request { return post("application/grpc-web-text") }, true},
		{
			// The prefix alone is not the marker: an unknown subtype is some
			// other protocol's, and claiming it hands its caller a Trailers-Only
			// response nothing in it will parse.
			"an invented grpc-ish subtype",
			func() *http.Request { return post("application/grpc-flavoured") },
			false,
		},
		{"connect unary JSON", func() *http.Request {
			r := post("application/json")
			r.Header.Set("Connect-Protocol-Version", "1")
			return r
		}, false},
		{"no content type", func() *http.Request {
			return httptest.NewRequest(http.MethodPost, "http://h/s/M", nil)
		}, false},
		{
			// Neither protocol has a GET form; a GET carrying this content
			// type is not a gRPC call, whatever it is.
			"GET with a grpc content type",
			func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "http://h/s/M", nil)
				r.Header.Set("Content-Type", "application/grpc")
				return r
			},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isGRPC(tc.build()); got != tc.expect {
				t.Errorf("isGRPC = %v, want %v", got, tc.expect)
			}
		})
	}
}

// TestPercentEncode pins the encoder against the gRPC spec directly, because
// the end-to-end tests above decode through clients that forgive what the
// spec merely tolerates: a decoder passes unencoded bytes through, so a
// broken encoder can survive every round trip and still truncate the message
// at the first proxy that enforces the header rules.
func TestPercentEncode(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain text stays put", "plain text stays put"},
		{"line one\nline two", "line one%0Aline two"},
		{"100%", "100%25"},
		{"café", "caf%C3%A9"}, // non-ASCII goes byte by byte, uppercase hex
		{"tab\there", "tab%09here"},
		// Both ends of the printable range the spec draws, where an
		// off-by-one would escape a byte that must stay literal or pass one
		// through that must not.
		{" ~", " ~"},
		{"\x1f\x7f", "%1F%7F"},
	} {
		if got := percentEncode(tc.in); got != tc.want {
			t.Errorf("percentEncode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestConnectStreamingCallerReadsTheDiagnostic is #173. A Connect *streaming*
// call carries its outcome in an end-of-stream envelope, and connect-go's
// streaming client validates the HTTP status before it reads any of the body
// — so the plain-text fallback this shape used to take was discarded unread
// and the caller was told "HTTP status 404 Not Found" and nothing else. ADR
// 005 makes the response the primary diagnostic surface; on this one shape it
// was empty.
//
// It is not an exotic shape: grpcreflect's default client speaks Connect, and
// ServerReflectionInfo is bidi, so `voya call` against an unregistered name
// takes exactly this path — which is why #158 needed a workaround in
// cmd/voya to read the sentence back out by hand.
//
// The assertion is the *message*, not the code: a code survived the old
// behaviour too (connect-go maps 404 to unimplemented on its own), so a test
// that checked only the code would have passed against the bug it exists to
// catch.
func TestConnectStreamingCallerReadsTheDiagnostic(t *testing.T) {
	reg := registry.New()
	if _, err := reg.Register(registry.Instance{Service: "payments", Port: 40404}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	proxyAddr := serveProxy(t, reg)

	// A typo of the registered name, so the diagnostic has a real list to
	// print and the test can prove the whole sentence crossed rather than a
	// bare code.
	c := connect.NewClient[emptypb.Empty, emptypb.Empty](
		client(t, proxyAddr, true), // h2c: bidi needs HTTP/2
		"http://paymnets.internal:9777"+procedureOK,
	)

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"server stream", func() error {
			stream, err := c.CallServerStream(context.Background(), connect.NewRequest(&emptypb.Empty{}))
			if err != nil {
				return err
			}
			defer stream.Close()
			// Drained rather than read: the error is Err's, and a
			// server-stream that never yields a message still has one.
			for stream.Receive() {
			}
			return stream.Err()
		}},
		{"bidi stream", func() error {
			stream := c.CallBidiStream(context.Background())
			// A send that loses the race with the server's close reports
			// io.EOF by protocol convention; the real error is Receive's.
			if err := stream.Send(&emptypb.Empty{}); err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			if err := stream.CloseRequest(); err != nil {
				return err
			}
			_, err := stream.Receive()
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("the call succeeded against a name nothing serves")
			}
			for _, want := range []string{
				`no service named "paymnets"`,
				"Registered services: payments.internal",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the diagnostic did not reach the caller — %q missing from:\n%v", want, err)
				}
			}
			// The row's own code, read off the envelope rather than inferred
			// from an HTTP status: 404 maps to unimplemented on its own, so
			// unavailable here is evidence the envelope was parsed.
			if got := connect.CodeOf(err); got != connect.CodeUnavailable {
				t.Errorf("code = %v, want unavailable — the envelope's code, not the status's", got)
			}
		})
	}
}

// TestIsConnectStreamClaimsOnlyConnectStreams pins the two-marker discipline
// at the seam rather than through a client: the end-to-end test above proves
// the streaming shape is claimed, and these rows prove nothing else is —
// which is the half that a wrong answer makes invisible, by handing some
// other caller a frame it cannot parse.
func TestIsConnectStreamClaimsOnlyConnectStreams(t *testing.T) {
	for _, tc := range []struct {
		name        string
		method      string
		version     string
		contentType string
		want        bool
	}{
		{"a Connect stream", http.MethodPost, "1", "application/connect+proto", true},
		{"a Connect stream, json codec", http.MethodPost, "1", "application/connect+json", true},
		// Unary Connect is isConnectUnary's, and its JSON envelope is a
		// different shape entirely.
		{"Connect unary", http.MethodPost, "1", "application/json", false},
		// The content type alone is not enough: without the version marker
		// this is some other caller that happens to name the media type.
		{"the content type without the version", http.MethodPost, "", "application/connect+proto", false},
		// And the version alone is not enough either — that is unary's row.
		{"the version without the content type", http.MethodPost, "1", "", false},
		{"gRPC", http.MethodPost, "", "application/grpc+proto", false},
		{"gRPC-Web", http.MethodPost, "", "application/grpc-web+proto", false},
		{"an unparseable content type", http.MethodPost, "1", "application/connect+proto; charset=", false},
		// Connect sends only unary calls as GET, so there is no streaming
		// GET to claim.
		{"a Connect GET", http.MethodGet, "1", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "http://payments.internal:9777"+procedureOK, nil)
			if tc.version != "" {
				r.Header.Set("Connect-Protocol-Version", tc.version)
			}
			if tc.contentType != "" {
				r.Header.Set("Content-Type", tc.contentType)
			}
			if got := isConnectStream(r); got != tc.want {
				t.Errorf("isConnectStream = %t, want %t", got, tc.want)
			}
		})
	}
}

// TestConnectStreamErrorAnswersACanonicalContentType covers the one edge
// where the two ends of the content-type negotiation disagree.
// [isConnectStream] claims the request through mime.ParseMediaType, which
// lowercases the media type and drops its parameters; connect-go validates
// the response's by raw string (connectValidateStreamResponseContentType).
// Echoed verbatim, each row below is claimed here and then rejected there —
// losing the diagnostic exactly the way #173 lost it, for a caller one
// keystroke off canonical.
func TestConnectStreamErrorAnswersACanonicalContentType(t *testing.T) {
	for _, ct := range []string{
		"application/connect+proto; charset=utf-8",
		"Application/Connect+Proto",
		"application/connect+proto",
	} {
		t.Run(ct, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://paymnets.internal:9777"+procedureOK, nil)
			r.Header.Set("Connect-Protocol-Version", "1")
			r.Header.Set("Content-Type", ct)

			// The precondition, not the outcome: a row this detector did not
			// claim would take the plain-text path and pass the assertion
			// below for the wrong reason.
			if !isConnectStream(r) {
				t.Fatalf("isConnectStream did not claim %q — this row proves nothing about the writer", ct)
			}

			rec := httptest.NewRecorder()
			New(registry.New()).ServeHTTP(rec, r)

			if got := rec.Code; got != http.StatusOK {
				t.Errorf("status = %d, want 200 — the client validates it before reading the body", got)
			}
			const want = "application/connect+proto"
			if got := rec.Header().Get("Content-Type"); got != want {
				t.Errorf("Content-Type = %q, want %q — connect-go compares this one as a raw string", got, want)
			}
		})
	}
}

// TestGRPCTrailersOnlyAnswersACanonicalContentType is the gRPC half of the
// test above (#181): [isGRPC] claims through mime.ParseMediaType too, so a
// verbatim echo answers "Application/GRPC+Proto" with "Application/GRPC+Proto"
// — a type the caller sent but is not the canonical form its detector saw.
// gRPC clients read the response type more loosely than connect-go, so this
// pins a divergence rather than a lost diagnostic.
func TestGRPCTrailersOnlyAnswersACanonicalContentType(t *testing.T) {
	for _, ct := range []string{
		"application/grpc+proto; charset=utf-8",
		"Application/GRPC+Proto",
		"application/grpc+proto",
	} {
		t.Run(ct, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://paymnets.internal:9777"+procedureOK, nil)
			r.Header.Set("Content-Type", ct)

			// The precondition: a row isGRPC did not claim would take some
			// other writer and pass or fail below for the wrong reason.
			if !isGRPC(r) {
				t.Fatalf("isGRPC did not claim %q — this row proves nothing about the writer", ct)
			}

			rec := httptest.NewRecorder()
			New(registry.New()).ServeHTTP(rec, r)

			if got := rec.Code; got != http.StatusOK {
				t.Errorf("status = %d, want 200 — Trailers-Only rides a 200", got)
			}
			if got := rec.Header().Get("Grpc-Status"); got == "" {
				t.Errorf("Grpc-Status missing — the response is not the Trailers-Only shape this test is about")
			}
			const want = "application/grpc+proto"
			if got := rec.Header().Get("Content-Type"); got != want {
				t.Errorf("Content-Type = %q, want %q — the canonical form isGRPC claimed, not the echo", got, want)
			}
		})
	}
}

// TestDiagnoseAsksGRPCBeforeConnectUnary pins the one edge of diagnose's
// order that decides an answer. A gRPC POST that also carries
// Connect-Protocol-Version: 1 satisfies both detectors — isConnectUnary
// refuses only the application/connect+ prefix — so whichever runs first
// picks the writer, and asking Connect first would hand a gRPC client a JSON
// body where it reads trailers. The two Connect detectors need no such pin:
// they are mutually exclusive by construction, which the assertion below
// also states.
func TestDiagnoseAsksGRPCBeforeConnectUnary(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "http://paymnets.internal:9777"+procedureOK, nil)
	r.Header.Set("Content-Type", "application/grpc+proto")
	r.Header.Set("Connect-Protocol-Version", "1")

	// The precondition the order exists for: both claim this request. If
	// either stops doing so the order stops mattering, and this test should
	// be the thing that says so rather than quietly passing.
	if !isGRPC(r) || !isConnectUnary(r) {
		t.Fatalf("isGRPC = %t, isConnectUnary = %t — want both true, or the order below decides nothing",
			isGRPC(r), isConnectUnary(r))
	}
	// And the pair that does not need an order.
	if isConnectStream(r) && isConnectUnary(r) {
		t.Error("isConnectStream and isConnectUnary both claimed one request — they are meant to be exclusive")
	}

	rec := httptest.NewRecorder()
	New(registry.New()).ServeHTTP(rec, r)

	if got := rec.Header().Get("Grpc-Status"); got == "" {
		t.Errorf("no Grpc-Status: the gRPC caller got %q with body %q",
			rec.Header().Get("Content-Type"), rec.Body.String())
	}
	if got := rec.Body.Len(); got != 0 {
		t.Errorf("body = %q, want empty — Trailers-Only carries the outcome in headers", rec.Body.String())
	}
}
