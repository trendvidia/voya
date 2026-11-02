package proxy

import (
	"context"
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

	"github.com/trendvidia/voya/internal/auth"
	"github.com/trendvidia/voya/internal/registry"
	"github.com/trendvidia/voya/proto/voya/diag/v1/diagv1connect"
)

// --- #223: the local rate limiter --------------------------------------------

// okBackend registers name on a backend that answers 200 with its own name,
// so a test counting answers can tell a forwarded request from a refused one
// by status alone.
func okBackend(t *testing.T, reg *registry.Registry, name string) {
	t.Helper()
	registered(t, reg, name, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, name)
	}))
}

// TestOverTheThresholdAnswers429WithRetryAfter is the limiter's contract on
// the wire (#223, ADR 006): under the threshold every request is forwarded
// untouched, the request over it answers 429 with a Retry-After a client can
// parse — positive, and no longer than the window — and the refusal lands in
// the request log like any other answered request.
func TestOverTheThresholdAnswers429WithRetryAfter(t *testing.T) {
	reg := registry.New()
	okBackend(t, reg, "payments")
	rec := &recorded{}
	c := client(t, serveProxy(t, reg,
		WithRateLimit(2, time.Minute), WithRequestLog(rec)), false)

	for i := range 2 {
		if got := fetch(t, c, "payments.internal:9777", "/v1/charge"); got.status != http.StatusOK {
			t.Fatalf("request %d under the threshold: status = %d, want 200", i+1, got.status)
		}
	}
	res := fetch(t, c, "payments.internal:9777", "/v1/charge")
	if res.status != http.StatusTooManyRequests {
		t.Fatalf("request over the threshold: status = %d, want 429", res.status)
	}
	header := res.header.Get("Retry-After")
	seconds, err := strconv.Atoi(header)
	if err != nil {
		t.Fatalf("Retry-After = %q, not the delta-seconds form a client parses: %v", header, err)
	}
	if seconds <= 0 || seconds > 60 {
		t.Errorf("Retry-After = %d seconds, want within (0, 60] — the wait is the window that refused", seconds)
	}
	// The body carries the same wait: `curl` without -i prints no headers at
	// all, and ADR 005 makes the body the diagnostic that stands alone.
	if !strings.Contains(res.body, "retry in "+header) {
		t.Errorf("the 429 body does not name the %ss the header carries:\n%s", header, res.body)
	}
	// The marker that tells the client wrapper this 429 is voya's own to
	// wait out, not a forwarded verdict to surface (#241);
	// TestAForwardedUpstream429CarriesNoMarker is the other half.
	if res.header.Get(RateLimitedHeader) == "" {
		t.Errorf("no %s on the limiter's refusal — the client wrapper would surface it instead of waiting it out", RateLimitedHeader)
	}

	recs := rec.waitRecorded(t, 3)
	last := recs[len(recs)-1]
	if last.Status != http.StatusTooManyRequests {
		t.Errorf("the refusal was recorded as %d, want 429 — a throttled request is answered, so it is logged", last.Status)
	}
	// The port stays in the record here because this harness's proxy listens
	// on an ephemeral port, so :9777 is a foreign port to recordedAuthority
	// and foreign ports are kept as they arrived (#161).
	if last.Authority != "payments.internal:9777" {
		t.Errorf("the refusal was recorded under %q, want payments.internal:9777", last.Authority)
	}
}

// TestAForwardedUpstream429CarriesNoMarker: [RateLimitedHeader] is voya's
// own refusal's, never a forwarded answer's (#241). An upstream that answers
// 429 as its own application verdict — an over-quota tenant, say — reaches
// the caller verbatim, its own Retry-After kept and the marker absent, so
// the client wrapper surfaces it on the first attempt instead of waiting it
// out. The limiter is on and under threshold here, so the absence is the
// forwarding path's doing, not the limiter's.
func TestAForwardedUpstream429CarriesNoMarker(t *testing.T) {
	reg := registry.New()
	registered(t, reg, "quota", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, "tenant over quota")
	}))
	c := client(t, serveProxy(t, reg, WithRateLimit(2, time.Minute)), false)

	res := fetch(t, c, "quota.internal:9777", "/v1/charge")
	if res.status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the upstream's 429 forwarded", res.status)
	}
	if got := res.header.Get(RateLimitedHeader); got != "" {
		t.Errorf("%s = %q on a forwarded response, want absent — the marker is the limiter's alone", RateLimitedHeader, got)
	}
	if got := res.header.Get("Retry-After"); got != "7" {
		t.Errorf("Retry-After = %q, want the upstream's own %q forwarded verbatim", got, "7")
	}

	// The control for the sentence above: a limiter that was never gating
	// this authority would pass every assertion so far, and the marker's
	// absence would then say nothing about the forwarding path. The first
	// call spent 1 of the budget's 2, so the third request is the
	// limiter's own refusal — the same authority, so the same budget — and
	// its Retry-After is the window's, never the upstream's 7, which is
	// what tells the refusal apart from one more forwarded answer.
	fetch(t, c, "quota.internal:9777", "/v1/charge")
	refused := fetch(t, c, "quota.internal:9777", "/v1/charge")
	if refused.status != http.StatusTooManyRequests {
		t.Fatalf("the request past the budget: status = %d, want 429 — the limiter never engaged, so the absence above is not the forwarding path's doing", refused.status)
	}
	if got := refused.header.Get("Retry-After"); got == "7" {
		t.Fatalf("Retry-After = %q on the request past the budget — that is the upstream's own, so this 429 was forwarded and the limiter is not the thing under test", got)
	}
	if refused.header.Get(RateLimitedHeader) == "" {
		t.Errorf("no %s on the limiter's own refusal, though the forwarded 429 above reached the caller unmarked — the two are indistinguishable to the client wrapper", RateLimitedHeader)
	}
}

// TestAnUpstreamsOwnMarkerIsForwardedVerbatim pins the cost ADR 005's and
// ADR 006's amendments of 2026-08-20 accept rather than fix: the forwarding
// path is verbatim, so an upstream that stamps [RateLimitedHeader] on its
// own 429 has it reach the caller, and voya's client then waits that 429 out
// as though the limiter had written it. Loopback and not a security boundary
// (ADR 002) — but a documented divergence with no test is one nobody notices
// changing, so this is the test that says those two amendments need rewriting
// the day the proxy starts stripping the marker on the way through.
func TestAnUpstreamsOwnMarkerIsForwardedVerbatim(t *testing.T) {
	reg := registry.New()
	registered(t, reg, "quota", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(RateLimitedHeader, "1")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, "tenant over quota")
	}))
	c := client(t, serveProxy(t, reg, WithRateLimit(2, time.Minute)), false)

	res := fetch(t, c, "quota.internal:9777", "/v1/charge")
	if res.status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the upstream's 429 forwarded", res.status)
	}
	if got := res.header.Get(RateLimitedHeader); got != "1" {
		t.Errorf("%s = %q, want the upstream's own %q through untouched — the proxy forwards responses verbatim, which is the cost the ADRs accept", RateLimitedHeader, got, "1")
	}
	if got := res.header.Get("Retry-After"); got != "7" {
		t.Errorf("Retry-After = %q, want the upstream's own %q forwarded verbatim", got, "7")
	}
}

// TestRateLimitedConnectCallerGetsResourceExhausted: a Connect caller reads
// its outcome from the envelope, so the refusal has to be resource_exhausted
// under Connect's canonical 429 — a bare HTTP status would reach it as a
// transport error with the diagnostic discarded. The Retry-After header
// rides the same response, because it is what the caller is meant to honour
// (ADR 006).
func TestRateLimitedConnectCallerGetsResourceExhausted(t *testing.T) {
	reg := registry.New()
	okBackend(t, reg, "payments")
	c := client(t, serveProxy(t, reg, WithRateLimit(1, time.Minute)), false)

	if got := fetch(t, c, "payments.internal:9777", "/"); got.status != http.StatusOK {
		t.Fatalf("the budget's one request: status = %d, want 200", got.status)
	}
	env, res := connectEnvelope(t, c, "payments.internal:9777", "/payments.v1.PaymentService/Charge")

	if env.Code != "resource_exhausted" {
		t.Errorf("code = %q, want resource_exhausted — gRPC's own name for a spent budget", env.Code)
	}
	if res.status != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 — Connect's canonical status for resource_exhausted", res.status)
	}
	if seconds, err := strconv.Atoi(res.header.Get("Retry-After")); err != nil || seconds <= 0 {
		t.Errorf("Retry-After = %q, want positive delta-seconds on the envelope's response too",
			res.header.Get("Retry-After"))
	}
}

// TestABrowserCallerCanReadRetryAfter: Retry-After is not a CORS-safelisted
// response header, so a browser reads it only if voya exposes it. Without
// that a fetch() caller sees status 429 and null for the header — exactly
// the retry handling ADR 006 built the limiter to exercise locally.
func TestABrowserCallerCanReadRetryAfter(t *testing.T) {
	reg := registry.New()
	okBackend(t, reg, "payments")
	c := client(t, serveProxy(t, reg, WithRateLimit(1, time.Minute)), false)

	if got := fetch(t, c, "payments.internal:9777", "/"); got.status != http.StatusOK {
		t.Fatalf("the budget's one request: status = %d, want 200", got.status)
	}
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

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("no Retry-After on the refusal")
	}
	if got := resp.Header.Get("Access-Control-Expose-Headers"); !strings.Contains(got, "Retry-After") {
		t.Errorf("Expose-Headers = %q, want Retry-After among them — a browser cannot read it otherwise", got)
	}
}

// TestABrowserCallerCanReadTheRateLimitMarker: [RateLimitedHeader] and
// Retry-After are one signal — the wait, and whose refusal it is — and the
// marker is no more CORS-safelisted than the wait, so a fetch() caller reads
// it only if voya exposes it. Without that the browser sees how long to wait
// but not whether the 429 is voya's own to wait out, which is the distinction
// the marker exists to carry (#241, #251).
func TestABrowserCallerCanReadTheRateLimitMarker(t *testing.T) {
	reg := registry.New()
	okBackend(t, reg, "payments")
	c := client(t, serveProxy(t, reg, WithRateLimit(1, time.Minute)), false)

	if got := fetch(t, c, "payments.internal:9777", "/"); got.status != http.StatusOK {
		t.Fatalf("the budget's one request: status = %d, want 200", got.status)
	}
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

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get(RateLimitedHeader); got != "1" {
		t.Fatalf("%s = %q on the limiter's 429, want %q — there is nothing to expose", RateLimitedHeader, got, "1")
	}
	if got := resp.Header.Get("Access-Control-Expose-Headers"); !strings.Contains(got, RateLimitedHeader) {
		t.Errorf("Expose-Headers = %q, want %s among them — a browser cannot read it otherwise", got, RateLimitedHeader)
	}
}

// TestTheMarkerRidesOnlyTheShapesThatAnswer429: [RateLimitedHeader] means
// "this 429 is voya's own", so it rides only the response shapes whose own
// status line says 429 — the plain-text response and the Connect unary
// envelope. A gRPC or gRPC-Web caller's Trailers-Only refusal and a Connect
// stream's end-of-stream envelope answer HTTP 200 with the outcome in
// grpc-status or the envelope: there is no 429 there for the marker to
// qualify, so those shapes carry Retry-After alone (#251, ADR 005). Each
// 200-shaped row asserts resource_exhausted where its caller reads it, so a
// forwarded backend answer — also a 200 without the marker — cannot pass as
// a refusal.
func TestTheMarkerRidesOnlyTheShapesThatAnswer429(t *testing.T) {
	reg := registry.New()
	okBackend(t, reg, "payments")
	c := client(t, serveProxy(t, reg, WithRateLimit(1, time.Minute)), false)

	if got := fetch(t, c, "payments.internal:9777", "/"); got.status != http.StatusOK {
		t.Fatalf("the budget's one request: status = %d, want 200", got.status)
	}

	// post issues the one POST every non-plain shape starts from; the
	// headers are what tell diagnose's detectors apart.
	post := func(t *testing.T, header http.Header) result {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
			"http://payments.internal:9777/payments.v1.PaymentService/Charge", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		for name, values := range header {
			req.Header[name] = values
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		payload, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("reading body: %v", err)
		}
		return result{status: resp.StatusCode, header: resp.Header.Clone(), body: string(payload)}
	}

	tests := []struct {
		name string
		// refused issues one over-budget request in the shape under test
		// and asserts the refusal engaged — not a forwarded answer.
		refused    func(t *testing.T) result
		status     int
		wantMarker bool
	}{
		{
			name: "plain text",
			refused: func(t *testing.T) result {
				return fetch(t, c, "payments.internal:9777", "/")
			},
			status:     http.StatusTooManyRequests,
			wantMarker: true,
		},
		{
			name: "Connect unary",
			refused: func(t *testing.T) result {
				res := post(t, http.Header{
					"Content-Type":             {"application/json"},
					"Connect-Protocol-Version": {"1"},
				})
				if !strings.Contains(res.body, "resource_exhausted") {
					t.Fatalf("the envelope does not carry resource_exhausted, so this is not the limiter's refusal:\n%s", res.body)
				}
				return res
			},
			status:     http.StatusTooManyRequests,
			wantMarker: true,
		},
		{
			name: "gRPC Trailers-Only",
			refused: func(t *testing.T) result {
				res := post(t, http.Header{"Content-Type": {"application/grpc"}})
				if got := res.header.Get("Grpc-Status"); got != "8" {
					t.Fatalf("Grpc-Status = %q, want 8 — this is not the limiter's refusal", got)
				}
				return res
			},
			status:     http.StatusOK,
			wantMarker: false,
		},
		{
			name: "Connect stream",
			refused: func(t *testing.T) result {
				res := post(t, http.Header{
					"Content-Type":             {"application/connect+proto"},
					"Connect-Protocol-Version": {"1"},
				})
				if !strings.Contains(res.body, "resource_exhausted") {
					t.Fatalf("the end-of-stream envelope does not carry resource_exhausted, so this is not the limiter's refusal:\n%s", res.body)
				}
				return res
			},
			status:     http.StatusOK,
			wantMarker: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := tt.refused(t)
			if res.status != tt.status {
				t.Fatalf("status = %d, want %d", res.status, tt.status)
			}
			marker := res.header.Get(RateLimitedHeader)
			if tt.wantMarker && marker != "1" {
				t.Errorf("%s = %q, want %q — this shape's own status is the 429 the marker qualifies", RateLimitedHeader, marker, "1")
			}
			if !tt.wantMarker && marker != "" {
				t.Errorf("%s = %q on an HTTP %d, want absent — there is no 429 here for the marker to qualify", RateLimitedHeader, marker, res.status)
			}
			// Retry-After stays on every shape, deliberately: a caller that
			// reads its outcome from grpc-status or the envelope can still
			// use a wait hint (#251 left it out of scope by design).
			if res.header.Get("Retry-After") == "" {
				t.Errorf("no Retry-After on the %s refusal — the wait rides every shape", tt.name)
			}
		})
	}
}

// TestTwoAuthoritiesDoNotShareABudget: five requests per window to one name
// is five whoever else is busy (#223). Hammering payments to its limit and
// then asking billing tells per-authority keying from a global budget — a
// test that throttles only one name cannot — and the counts are exact on
// both sides, so a budget leaking either way fails it.
func TestTwoAuthoritiesDoNotShareABudget(t *testing.T) {
	reg := registry.New()
	okBackend(t, reg, "payments")
	okBackend(t, reg, "billing")
	c := client(t, serveProxy(t, reg, WithRateLimit(2, time.Minute)), false)

	for i := range 2 {
		if got := fetch(t, c, "payments.internal:9777", "/"); got.status != http.StatusOK {
			t.Fatalf("payments request %d: status = %d, want 200", i+1, got.status)
		}
	}
	if got := fetch(t, c, "payments.internal:9777", "/"); got.status != http.StatusTooManyRequests {
		t.Fatalf("payments over its limit: status = %d, want 429", got.status)
	}
	// billing's budget is untouched by payments' exhaustion — and it is
	// still a budget, so its own limit-plus-first refusal stays exact.
	for i := range 2 {
		if got := fetch(t, c, "billing.internal:9777", "/"); got.status != http.StatusOK {
			t.Errorf("billing request %d after payments was throttled: status = %d, want 200", i+1, got.status)
		}
	}
	if got := fetch(t, c, "billing.internal:9777", "/"); got.status != http.StatusTooManyRequests {
		t.Errorf("billing over its own limit: status = %d, want 429", got.status)
	}
}

// TestBothSpellingsOfOneAuthoritySpendOneBudget: payments.internal and
// payments.internal:9777 are the same service, so the limiter keys on the
// routed name rather than the raw Host — or the limit doubles for a caller
// who types the port (#223).
func TestBothSpellingsOfOneAuthoritySpendOneBudget(t *testing.T) {
	reg := registry.New()
	okBackend(t, reg, "payments")
	c := client(t, serveProxy(t, reg, WithRateLimit(2, time.Minute)), false)

	if got := fetch(t, c, "payments.internal", "/"); got.status != http.StatusOK {
		t.Fatalf("portless spelling: status = %d, want 200", got.status)
	}
	if got := fetch(t, c, "payments.internal:9777", "/"); got.status != http.StatusOK {
		t.Fatalf("ported spelling: status = %d, want 200", got.status)
	}
	if got := fetch(t, c, "payments.internal", "/"); got.status != http.StatusTooManyRequests {
		t.Errorf("third request across both spellings: status = %d, want 429 — two spellings, one budget", got.status)
	}
}

// TestReservedAuthorityIsNeverThrottled: voya.internal is voya's own surface,
// and throttling it would break `voya status` and `voya dns check` exactly when
// a developer is investigating a throttle (#223). Far past a limit any
// service name would have spent, both of the reserved name's surfaces — a
// DiagService procedure and the JWKS document — keep answering.
//
// Mutation-proved: keying the limiter on r.Host ahead of the reserved-name
// dispatch (counting voya.internal like a service) fails this test at the
// first request past the budget.
func TestReservedAuthorityIsNeverThrottled(t *testing.T) {
	reg := registry.New()
	okBackend(t, reg, "payments")
	c := client(t, serveProxy(t, reg,
		WithRateLimit(1, time.Minute), WithInternalHandler(internalStub())), false)

	if got := fetch(t, c, "payments.internal:9777", "/"); got.status != http.StatusOK {
		t.Fatalf("the budget's one request: status = %d, want 200", got.status)
	}
	if got := fetch(t, c, "payments.internal:9777", "/"); got.status != http.StatusTooManyRequests {
		t.Fatalf("payments over its limit: status = %d, want 429 — the limiter never engaged, so the "+
			"exemption below would pass vacuously", got.status)
	}

	reserved := registry.Authority(registry.Reserved) + ":9777"
	for i := range 3 {
		if got := fetch(t, c, reserved, auth.JWKSPath); got.status != http.StatusOK || got.body != internalStubWellKnown {
			t.Errorf("JWKS fetch %d: status = %d, body = %q — voya's own surface was throttled", i+1, got.status, got.body)
		}
		if got := fetch(t, c, reserved, diagv1connect.DiagServiceGetStatusProcedure); got.status != http.StatusOK {
			t.Errorf("DiagService request %d: status = %d — `voya status` was throttled", i+1, got.status)
		}
	}
}

// TestWithoutARateLimitTheProxyHoldsNoLimiter pins `enabled = false` costing
// nothing: an unconfigured proxy carries no limiter at all — no map and no
// clock read on the hot path, because serve's whole engagement with rate
// limiting is one nil check — and the two threshold shapes config validation
// refuses are defensively off rather than refuse-everything.
func TestWithoutARateLimitTheProxyHoldsNoLimiter(t *testing.T) {
	cases := []struct {
		name string
		opts []Option
	}{
		{"no option", nil},
		{"zero requests", []Option{WithRateLimit(0, time.Minute)}},
		{"zero window", []Option{WithRateLimit(5, 0)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, ok := New(registry.New(), tc.opts...).(*proxy)
			if !ok {
				t.Fatal("New no longer returns *proxy; this pin needs its internals")
			}
			if p.limiter != nil {
				t.Error("the proxy holds a limiter — a disabled rate_limit is meant to cost nothing")
			}
		})
	}
}

// TestWindowBoundary drives the limiter's clock by hand across one reset:
// the last instant of a spent window still refuses, and the first instant of
// the next admits — a boundary a wall clock can only ever straddle (#223).
func TestWindowBoundary(t *testing.T) {
	l := newLimiter(2, time.Minute)
	start := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	now := start
	l.now = func() time.Time { return now }

	for i := range 2 {
		if ok, _, _ := l.allow("payments"); !ok {
			t.Fatalf("request %d of the budget was refused", i+1)
		}
	}
	now = start.Add(time.Minute - time.Nanosecond)
	if ok, _, _ := l.allow("payments"); ok {
		t.Error("allowed immediately before the window's end — the window reset early")
	}
	now = start.Add(time.Minute)
	if ok, _, _ := l.allow("payments"); !ok {
		t.Error("refused immediately after the window's end — the window never reset")
	}
}

// TestRetryAfterStaysInsideTheWindow: a refusal's wait is how long the
// current window has left, so it is positive — the caller was just refused,
// so the reset has not happened — and never more than the window itself.
// Swept across the window rather than sampled once, because the wrong sign
// or an off-by-window only shows at an edge.
func TestRetryAfterStaysInsideTheWindow(t *testing.T) {
	const window = time.Minute
	l := newLimiter(1, window)
	start := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	now := start
	l.now = func() time.Time { return now }

	if ok, _, _ := l.allow("payments"); !ok {
		t.Fatal("the budget's one request was refused")
	}
	for _, offset := range []time.Duration{
		0, time.Nanosecond, time.Second, 30 * time.Second, window - time.Second, window - time.Nanosecond,
	} {
		now = start.Add(offset)
		ok, wait, _ := l.allow("payments")
		if ok {
			t.Fatalf("allowed at %s into a spent window", offset)
		}
		if wait <= 0 || wait > window {
			t.Errorf("refused at %s: retryAfter = %s, want within (0, %s]", offset, wait, window)
		}
		if header, err := strconv.Atoi(retryAfterHeader(wait)); err != nil || header <= 0 || header > 60 {
			t.Errorf("refused at %s: header = %q, want whole seconds within (0, 60]",
				offset, retryAfterHeader(wait))
		}
	}
}

// TestASubSecondWindowRoundsRetryAfterUpToOneSecond pins the one divergence
// ADR 005's amendment records: Retry-After cannot carry a fraction, so a
// window shorter than a whole second answers 1 — longer than the window
// itself, and the safe side deliberately, since rounding down would name a
// moment still inside the window that refused. Validation accepts any
// positive window, so this is reachable configuration, not a hypothetical.
func TestASubSecondWindowRoundsRetryAfterUpToOneSecond(t *testing.T) {
	const window = 100 * time.Millisecond
	l := newLimiter(1, window)
	start := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	now := start
	l.now = func() time.Time { return now }

	if ok, _, _ := l.allow("payments"); !ok {
		t.Fatal("the budget's one request was refused")
	}
	now = start.Add(window / 2)
	ok, wait, _ := l.allow("payments")
	if ok {
		t.Fatal("allowed inside a spent window")
	}
	if wait <= 0 || wait > window {
		t.Errorf("retryAfter = %s, want within (0, %s]", wait, window)
	}
	if got := retryAfterHeader(wait); got != "1" {
		t.Errorf("Retry-After = %q, want \"1\" — the header rounds a sub-second wait up", got)
	}
}

// TestRetryAfterRoundsUpAwayFromTheWindowThatRefused pins the rounding
// itself, which the sub-second case above cannot: retryAfterHeader floors at
// one second as well as rounding up, so for a wait under a second the two
// mechanisms answer "1" independently and flooring the arithmetic goes
// unnoticed. Above a second they diverge, and the divergence is the harm ADR
// 005's amendment names — a 59.5s wait floored to 59 tells the caller to
// retry at a moment still inside the window that refused, which spends the
// next window's first request on another 429.
func TestRetryAfterRoundsUpAwayFromTheWindowThatRefused(t *testing.T) {
	cases := []struct {
		remaining time.Duration
		want      string
	}{
		{time.Nanosecond, "1"}, // the clamp and the round-up agree
		{time.Second, "1"},     // exact, and not rounded to 2
		{time.Second + time.Nanosecond, "2"},
		{59500 * time.Millisecond, "60"}, // floored, this is 59 — inside the window
		{time.Minute, "60"},
	}
	for _, tc := range cases {
		if got := retryAfterHeader(tc.remaining); got != tc.want {
			t.Errorf("retryAfterHeader(%s) = %q, want %q", tc.remaining, got, tc.want)
		}
	}
}

// TestConcurrentCallersSpendTheBudgetExactly: the count must not be lost to
// a race, and "some were refused" cannot tell a lost increment from a slow
// scheduler — so the assertion is the exact budget, under the race detector,
// from more goroutines than the budget admits.
func TestConcurrentCallersSpendTheBudgetExactly(t *testing.T) {
	const budget, callers = 37, 100
	l := newLimiter(budget, time.Minute)
	fixed := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return fixed }

	var wg sync.WaitGroup
	allowed := make(chan bool, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, _, _ := l.allow("payments")
			allowed <- ok
		}()
	}
	wg.Wait()
	close(allowed)

	var admitted int
	for ok := range allowed {
		if ok {
			admitted++
		}
	}
	if admitted != budget {
		t.Errorf("%d of %d concurrent requests were admitted, want exactly %d", admitted, callers, budget)
	}
}

// --- #233: what spends a budget ----------------------------------------------

// TestARequestThatReachedNothingSpendsNoBudget is #233's second item on the
// wire: a service that registered a port nothing listens on answers the
// unreachable diagnostic however many times a retrying client asks — never
// the throttle, which would tell the developer "rate limit" when the truth
// is "your registration is wrong". The gate charges before the dial can be
// tried, so this is the ErrorHandler's refund being exercised, requests+2
// times against a budget of 2.
func TestARequestThatReachedNothingSpendsNoBudget(t *testing.T) {
	reg := registry.New()
	if _, err := reg.Register(registry.Instance{Service: "payments", Port: deadPort(t)}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	c := client(t, serveProxy(t, reg, WithRateLimit(2, time.Minute)), false)

	for i := range 4 {
		res := fetch(t, c, "payments.internal:9777", "/v1/charge")
		if res.status != http.StatusBadGateway {
			t.Fatalf("attempt %d: status = %d, want 502 every time — a request that reached nothing costs nothing:\n%s",
				i+1, res.status, res.body)
		}
		if !strings.Contains(res.body, "but the connection failed") {
			t.Fatalf("attempt %d answered 502 without the misregistration diagnostic:\n%s", i+1, res.body)
		}
	}
}

// TestAnUpstreamThatWasReachedAndFailedStillSpendsBudget pins the refund's
// boundary from the other side: an upstream that accepts the connection and
// then breaks it was reached, and is exactly what the limiter exists to
// protect — refunding it would make the limit unenforceable against a
// broken-but-listening service. Two such requests spend the budget of 2; the
// third is the throttle.
func TestAnUpstreamThatWasReachedAndFailedStillSpendsBudget(t *testing.T) {
	reg := registry.New()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	if _, err := reg.Register(registry.Instance{Service: "payments",
		Port: uint32(ln.Addr().(*net.TCPAddr).Port)}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	c := client(t, serveProxy(t, reg, WithRateLimit(2, time.Minute)), false)

	for i := range 2 {
		if res := fetch(t, c, "payments.internal:9777", "/v1/charge"); res.status != http.StatusBadGateway {
			t.Fatalf("attempt %d: status = %d, want 502 — the upstream accepted and hung up", i+1, res.status)
		}
	}
	if res := fetch(t, c, "payments.internal:9777", "/v1/charge"); res.status != http.StatusTooManyRequests {
		t.Errorf("attempt 3: status = %d, want 429 — a reached upstream's failures were refunded", res.status)
	}
}

// TestADialThatRacedACancelledRequestSpendsNoBudget pins why the refund reads
// a connection rather than an error: a request whose caller has already gone
// is still charged — the gate runs before the dial — but the transport asked
// to reach a dead port with a done context reports the cancellation instead
// of the dial refusal, net/http preferring context.Cause to the dial error it
// already holds. An ErrorHandler that matched a dial *net.OpError would keep
// the charge for a request that reached nothing, so a caller with a deadline
// against a misregistered service would burn its budget after all, which is
// the symptom #233 exists to remove.
//
// Driven directly, not through serveProxy: no client can put an already
// cancelled request on the wire, so the recorder is the only way to hand the
// handler the race's outcome deterministically.
func TestADialThatRacedACancelledRequestSpendsNoBudget(t *testing.T) {
	reg := registry.New()
	if _, err := reg.Register(registry.Instance{Service: "payments", Port: deadPort(t)}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	handler := New(reg, WithErrorLog(nil), WithRateLimit(1, time.Minute))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	gone := httptest.NewRequest(http.MethodGet, "http://payments.internal:9777/v1/charge", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, gone)
	// The setup, not the outcome: the whole budget of 1 must have gone to
	// the cancelled request, or the refund below is not what is under test.
	if rec.Code == http.StatusTooManyRequests {
		t.Fatalf("the cancelled request was itself throttled, so the gate charged nothing and this test pins nothing")
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://payments.internal:9777/v1/charge", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 — the cancelled request reached nothing and must have been refunded:\n%s",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "but the connection failed") {
		t.Errorf("answered 502 without the misregistration diagnostic:\n%s", rec.Body.String())
	}
}

// TestARefundNeverResurrectsAResetWindow: a refund carries the window token
// its spend was admitted under, and a window that has since reset — here,
// been replaced by a successor — is left alone. If the stale refund touched
// the new window, a third request would fit a budget of 2.
func TestARefundNeverResurrectsAResetWindow(t *testing.T) {
	l := newLimiter(2, time.Minute)
	start := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	now := start
	l.now = func() time.Time { return now }

	ok, _, stale := l.allow("payments")
	if !ok {
		t.Fatal("the first request of a fresh window was refused")
	}
	now = start.Add(time.Minute) // the window resets; the next allow starts a successor
	if ok, _, _ := l.allow("payments"); !ok {
		t.Fatal("the first request of the successor window was refused")
	}
	l.refund("payments", stale)
	if ok, _, _ := l.allow("payments"); !ok {
		t.Fatal("the second request of the successor window was refused")
	}
	if ok, _, _ := l.allow("payments"); ok {
		t.Error("a third request fit a budget of 2: the stale refund was credited to the successor window")
	}
}

// TestARefundNeverTakesAWindowBelowZero: one spend refunded twice credits the
// window once. The count is a uint32, so an unguarded second decrement would
// wrap it into a full window — every request refused — which is how the bug
// would present.
func TestARefundNeverTakesAWindowBelowZero(t *testing.T) {
	l := newLimiter(2, time.Minute)
	fixed := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return fixed }

	_, _, admitted := l.allow("payments")
	l.refund("payments", admitted)
	l.refund("payments", admitted)
	for i := range 2 {
		if ok, _, _ := l.allow("payments"); !ok {
			t.Fatalf("request %d of the budget was refused after a double refund — the count underflowed", i+1)
		}
	}
	if ok, _, _ := l.allow("payments"); ok {
		t.Error("a third request fit a budget of 2: the double refund credited the window twice")
	}
}

// probe is [fetch] with [ProbeHeader] set, which is how voya's own health
// probes call (#233).
func probe(t *testing.T, c *http.Client, authority string) result {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://"+authority+"/grpc.health.v1.Health/Check", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set(ProbeHeader, "health")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET http://%s: %v", authority, err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return result{status: resp.StatusCode, header: resp.Header.Clone(), body: string(payload)}
}

// TestAProbeSpendsNoBudgetAndItsMarkerIsStripped is #233's first item at the
// proxy: a request carrying [ProbeHeader] is never charged — three probes
// against a budget of 1 all answer — and the marker stops at voya, so the
// upstream sees the call a real caller makes. The counted budget is asserted
// afterwards, full: the first plain request is forwarded and the second is
// the one refused, which is also the control that the limiter was on at all.
func TestAProbeSpendsNoBudgetAndItsMarkerIsStripped(t *testing.T) {
	reg := registry.New()
	seen := make(chan string, 8) // ProbeHeader as each upstream request carried it
	registered(t, reg, "payments", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get(ProbeHeader)
		fmt.Fprint(w, "payments")
	}))
	c := client(t, serveProxy(t, reg, WithRateLimit(1, time.Minute)), false)

	for i := range 3 {
		if res := probe(t, c, "payments.internal:9777"); res.status != http.StatusOK {
			t.Fatalf("probe %d: status = %d, want 200 — a probe spends no budget", i+1, res.status)
		}
	}
	// The setup, asserted: every probe reached the upstream — the exemption
	// did not short-circuit the forward — and none carried the marker.
	for i := range 3 {
		select {
		case got := <-seen:
			if got != "" {
				t.Errorf("probe %d arrived at the upstream carrying %s: %q — voya's marker must be stripped before forwarding",
					i+1, ProbeHeader, got)
			}
		default:
			t.Fatalf("probe %d never reached the upstream", i+1)
		}
	}
	if res := fetch(t, c, "payments.internal:9777", "/v1/charge"); res.status != http.StatusOK {
		t.Fatalf("first counted request: status = %d, want 200 — the probes spent the budget", res.status)
	}
	if res := fetch(t, c, "payments.internal:9777", "/v1/charge"); res.status != http.StatusTooManyRequests {
		t.Errorf("second counted request: status = %d, want 429 — the limiter was not gating at all", res.status)
	}
}
