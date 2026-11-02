package daemon

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"google.golang.org/protobuf/types/known/durationpb"

	configv1 "github.com/trendvidia/voya/proto/voya/config/v1"

	"github.com/lestrrat-go/jwx/v2/jwk"

	"github.com/trendvidia/voya/internal/auth"
	"github.com/trendvidia/voya/internal/config"
	"github.com/trendvidia/voya/internal/proxy"
	"github.com/trendvidia/voya/internal/registry"
)

// testConfig is a daemon on three free ephemeral ports, so these tests never
// collide with a developer's own voya on 9777.
//
// The draw lives in [FreePorts] rather than here. It used to live in both
// this file and the acceptance suite, as the same twenty lines, and until
// #44's review both copies had the same defect — which is the argument for it
// having one home (#46): the fix had to be found twice and applied twice.
func testConfig(t *testing.T, grace time.Duration) *configv1.Config {
	t.Helper()
	// A configuration directory of this test's own, because Start mints the
	// signing key there (#219): without this, every daemon these tests start
	// would write key material into the developer's real ~/.config/voya —
	// or refuse to start on a host with no home directory at all.
	t.Setenv(config.DirEnv, t.TempDir())
	ports, err := FreePorts(t.Context())
	if err != nil {
		t.Fatalf("drawing free ports: %v", err)
	}
	// Start from the committed defaults rather than restating them: a Config
	// built field by field in code is a second copy of base.pxf, and #150
	// (request_log) and #208 (auth) each found the copy missing a field the
	// moment the schema grew. "" is no user layer, so this is deterministic.
	cfg, err := config.Load(t.Context(), "")
	if err != nil {
		t.Fatalf("loading the committed defaults: %v", err)
	}
	cfg.ProxyPort = ports.Proxy
	cfg.DnsPort = ports.DNS
	cfg.RegistrationPort = ports.Registration
	cfg.ShutdownGrace = durationpb.New(grace)
	return cfg
}

// upstream is a registered service whose handler takes as long as it is told
// to, which is what makes a request still be in flight when Close is called.
// It returns the port it registered, for a test that reads the request log.
func upstream(t *testing.T, d *Daemon, name string, work time.Duration, entered chan<- struct{}) uint32 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("upstream Listen: %v", err)
	}
	// h2c, like a real service: voya's upstream leg is cleartext HTTP/2 only
	// (ADR 003). An HTTP/1.1-only server here answers the h2 preface as if
	// "PRI * HTTP/2.0" were a request, which looks like the handler ran.
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{
		Protocols:         protocols,
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			entered <- struct{}{}
			time.Sleep(work)
			_, _ = io.WriteString(w, "finished")
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("upstream address is %T", ln.Addr())
	}
	if _, err := d.Registry().Register(registry.Instance{
		Service: name, Port: uint32(addr.Port), PID: int32(addr.Port),
	}); err != nil {
		t.Fatalf("registering the upstream: %v", err)
	}
	return uint32(addr.Port)
}

// http11Caller speaks HTTP/1.1 to the proxy and pools its connection, which
// is what client.Transport() hands a service (#94) and so the other half of
// what arrives at the proxy.
//
// A client of its own rather than http.DefaultClient, for two reasons.
// DefaultClient has no Timeout, so a proxy that stopped answering — the
// regression these tests sit nearest to — would arrive as the package's own
// ten-minute panic dump instead of as a diagnostic. And its pool is
// process-global: a subtest that finished would leave an idle connection
// pointing at an ephemeral port a later [FreePorts] draw can hand to the
// next daemon.
func http11Caller(t *testing.T) *http.Client {
	t.Helper()
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	tr := &http.Transport{Protocols: protocols}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 30 * time.Second}
}

// h2cCaller speaks cleartext HTTP/2 to the proxy and pools its connection:
// the shape of a registered service's own client transport, which is the peer
// #72 is about. No IdleConnTimeout, deliberately — that peer never lets go of
// the connection from its own side, and since #94 took the inbound timer off,
// the proxy no longer lets go from its side either. A client of its own for
// [http11Caller]'s two reasons.
func h2cCaller(t *testing.T) *http.Client {
	t.Helper()
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	tr := &http.Transport{Protocols: protocols}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 30 * time.Second}
}

// waitUntil polls for a condition a test stages but does not control the
// timing of — the daemon's own bookkeeping settling behind an exchange the
// client has already seen complete. Bounded so a condition that never comes
// is a diagnostic rather than the package timeout's panic dump.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("gave up waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// proxied is one GET through the proxy: the response, fully read, with the
// protocol it actually travelled on — asserted by the callers that stake
// their meaning on which leg was exercised.
type proxied struct {
	body  string
	proto string
	err   error
}

func get(c *http.Client, port uint32, authority string) proxied {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://127.0.0.1:"+strconv.FormatUint(uint64(port), 10)+"/", nil)
	if err != nil {
		return proxied{err: err}
	}
	req.Host = authority
	resp, err := c.Do(req)
	if err != nil {
		return proxied{err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return proxied{body: string(body), proto: resp.Proto, err: err}
}

// TestTheShutdownGraceReachesInFlightRequests is ADR 004's grace period, and
// the reason Close shuts the proxy down before it cancels anything: a request
// a caller is already waiting on gets the grace to finish.
//
// Cancelling first made the grace inert — every in-flight request died at
// once, and the caller got voya's "the connection failed: context canceled"
// diagnostic, which describes a dead service rather than a stopping voya.
//
// Both protocols a caller can arrive on, because they reach the drain
// differently. The request is inside the handler when Close runs either way,
// so the gate cannot skip; what differs is what Shutdown then has to wait
// on — an HTTP/1.1 connection that reads idle once its response is flushed,
// or an h2c one that never reads idle and that nothing reaps now that #94
// took the inbound timer off.
//
// The grace here is a bound and not a runtime, which is what makes 2 seconds
// the right size rather than a cost every run pays. All it has to do is
// outlast the 300ms of work by enough that a descheduled run cannot cut the
// request this test exists to see finish — ADR 004's worst recorded
// scheduling sample for this project's CI host is 106.7ms. Nothing here
// asserts that either row reaches the deadline, and nothing here is about
// what happens if one does — that is
// [TestTheDeadlineTellsBusyFromAttachedCallers]' subject, and staging it
// needs a peer of its own for the reason written down there.
func TestTheShutdownGraceReachesInFlightRequests(t *testing.T) {
	const work = 300 * time.Millisecond

	for _, tc := range []struct {
		name   string
		proto  string
		client func(*testing.T) *http.Client
	}{
		{"http1 caller", "HTTP/1.1", http11Caller},
		{"h2c caller", "HTTP/2.0", h2cCaller},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t, 2*time.Second)
			d, err := Start(context.Background(), cfg, func(string, ...any) {})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			// Every t.Fatalf between here and the timed Close below is a
			// Goexit — [upstream] has three — and without this the subtest's
			// three listeners stay bound and its serve goroutines keep
			// running for the rest of the binary, with the next subtest and
			// every later test competing against a live daemon. Close is
			// sync.Once'd, so this is a no-op after the real one.
			t.Cleanup(func() { _ = d.Close() })

			entered := make(chan struct{}, 1)
			upstream(t, d, "slow", work, entered)

			c := tc.client(t)
			done := make(chan proxied, 1)
			go func() { done <- get(c, cfg.GetProxyPort(), "slow.internal") }()

			<-entered // the request is in the upstream handler; now stop voya under it
			start := time.Now()
			if err := d.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			waited := time.Since(start)

			got := <-done
			if got.err != nil {
				t.Fatalf("the in-flight request failed instead of finishing: %v", got.err)
			}
			if got.body != "finished" {
				t.Fatalf("the in-flight request got %q, want the upstream's answer", got.body)
			}
			// The leg this case exists to exercise, not assumed of the
			// transport's protocol negotiation.
			if got.proto != tc.proto {
				t.Fatalf("the request travelled on %s, want %s — the wrong caller was tested", got.proto, tc.proto)
			}
			// And Close waited for it rather than returning while it ran.
			if waited < work/2 {
				t.Errorf("Close returned in %s, before a request that took %s could finish", waited, work)
			}
		})
	}
}

// TestTheGraceIsABoundNotAPromise: a request that outlasts the grace is cut
// off, and that is a correct shutdown rather than an error to report.
func TestTheGraceIsABoundNotAPromise(t *testing.T) {
	cfg := testConfig(t, 100*time.Millisecond)
	d, err := Start(context.Background(), cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	entered := make(chan struct{}, 1)
	upstream(t, d, "glacial", 30*time.Second, entered)

	go func() {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
			"http://127.0.0.1:"+strconv.FormatUint(uint64(cfg.GetProxyPort()), 10)+"/", nil)
		if err != nil {
			return
		}
		req.Host = "glacial.internal"
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()

	<-entered
	start := time.Now()
	if err := d.Close(); err != nil {
		t.Errorf("Close reported a failure for a shutdown that hit its bound: %v", err)
	}
	if waited := time.Since(start); waited > 10*time.Second {
		t.Errorf("Close took %s; the grace is meant to bound it at 100ms", waited)
	}
}

// TestCloseDoesNotWaitOutAnIdleCaller is #72's acceptance, end to end. A
// registered caller is still up and recently used the proxy, and its pooled
// h2c connection — which never reads as idle, and which nothing reaps now
// that #94 took the inbound timer off — is still attached when Close runs.
// Close must finish well inside the grace, with no claim that anything was
// in flight: the gate reads the wire count and the handler count, finds
// nothing for a drain to protect, and skips Shutdown's wait entirely.
// Before the gate (and before the reap it replaced), this Close burned the
// full 250ms three of three and warned "proxy still busy" over an empty
// proxy, which is the misdiagnosis the issue is named for.
//
// An idle HTTP/1.1 caller stays attached too: that connection is the shape
// the wire count actually tracks — StateActive at the request, booked back
// out at StateIdle — so this is also the test that a finished HTTP/1.1
// exchange leaves the count at zero. A count that only ever grew would fail
// here as a Close waiting out the grace behind two quiet callers.
//
// Gated on the median, which is CLAUDE.md's rule and
// [TestAProxiedToServiceShutsDownPromptly]'s shape one package down: what a
// regression here moves is every sample, while a descheduled run moves one.
// The correctness half — that neither deadline message is printed — is
// asserted on every run, because that one is not about the machine at all.
func TestCloseDoesNotWaitOutAnIdleCaller(t *testing.T) {
	const runs = 3
	// The shipped grace, read rather than restated. It is the number #72's
	// measurement was taken against, and CLAUDE.md keeps voya.config.v1's
	// defaults in base.pxf alone — a 250ms written here would go on claiming
	// to be the default after base.pxf stopped saying so, and this test would
	// quietly stop measuring #72 against the grace it names.
	grace := shippedGrace(t)

	took := make([]time.Duration, 0, runs)
	for range runs {
		took = append(took, timeCloseAfterAnIdleCaller(t, grace))
	}
	slices.Sort(took)

	if median := took[len(took)/2]; median >= grace {
		t.Errorf("Close took %s (median of %d: %v): the %s grace was burned waiting out callers "+
			"that were still attached with nothing on the wire and nothing in the handler — "+
			"nothing reaps them since #94, so the gate is the only thing that ends this exit "+
			"early and it did not fire", median, runs, took, grace)
	}
}

// shippedGrace is voya's own shutdown_grace as configured, not as remembered.
// An empty dir means "no user layer", so what comes back is the committed
// base layer — the one home the default is allowed to have.
func shippedGrace(t *testing.T) time.Duration {
	t.Helper()
	cfg, err := config.Load(t.Context(), "")
	if err != nil {
		t.Fatalf("loading the committed defaults: %v", err)
	}
	return cfg.GetShutdownGrace().AsDuration()
}

// timeCloseAfterAnIdleCaller stages one daemon, attaches a real caller on
// each protocol, lets both go idle and reports what Close then costs. It
// fails the test itself on anything that would make the number meaningless —
// a staging request that never reached the backend, a caller that turned out
// to be on the wrong protocol — and on either half of the deadline
// diagnostic appearing, which no run may produce.
//
// Everything is built fresh per call: the connections under test are the
// ones this daemon's proxy accepted, so reusing either end across runs would
// measure the second run against the first one's callers.
func timeCloseAfterAnIdleCaller(t *testing.T, grace time.Duration) time.Duration {
	t.Helper()

	log := NewTranscript(nil)
	cfg := testConfig(t, grace)
	d, err := Start(context.Background(), cfg, log.Logf)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Every t.Fatal below is a Goexit, so the Close at the end of the happy
	// path does not run on any of them. Without this the daemon's three
	// listeners stay bound and its serve goroutines keep running for the
	// rest of the binary. Close is sync.Once'd, so the timed one below still
	// measures the real shutdown and this is a no-op after it.
	t.Cleanup(func() { _ = d.Close() })

	entered := make(chan struct{}, 2)
	upstream(t, d, "payments", 0, entered)

	// Both callers #72 and #94 are about, attached and then idle.
	for _, caller := range []struct {
		client *http.Client
		proto  string
	}{
		{h2cCaller(t), "HTTP/2.0"},
		{http11Caller(t), "HTTP/1.1"},
	} {
		got := get(caller.client, cfg.GetProxyPort(), "payments.internal")
		if got.err != nil || got.body != "finished" {
			t.Fatalf("the staging request did not reach the backend (body %q, err %v), so there is "+
				"no caller connection for this test to be about", got.body, got.err)
		}
		if got.proto != caller.proto {
			t.Fatalf("the staging request travelled on %s, want %s — the caller it stages was "+
				"never attached", got.proto, caller.proto)
		}
		<-entered
	}

	// Wait for the two numbers the gate reads, not for a proxy for them: the
	// client finishing its read does not order the server's own StateIdle
	// transition, which comes a beat apart from the last response byte
	// (after the flush on HTTP/1.1, before it on h2c; see [connActivity]) —
	// and a Close sampling inside that beat would take the drain path and
	// wait out the whole grace on the attached h2c caller.
	waitUntil(t, "the gate's two counts to read zero", func() bool {
		return d.proxyWire.count() == 0 && d.inflight.Load() == 0
	})

	start := time.Now()
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waited := time.Since(start)

	// Neither side of the deadline diagnostic may fire, and they share this
	// phrase: the deadline was never reached, so there was nothing to
	// explain.
	if log.Printed("shutdown grace") {
		t.Errorf("Close reported a shutdown-grace deadline though nothing was on the wire and "+
			"nothing was in the handler:\n%s", log)
	}
	return waited
}

// TestTheDeadlineTellsBusyFromAttachedCallers pins what Close reports when
// the deadline does expire: a request that outlived the grace is the warning,
// a connection that merely had not let go is not.
//
// The two cases need different peers, because reaching the deadline is the
// hard part of staging each. A polite caller no longer gets either side
// there: since the native h2c support this server configures, a Go client
// that received Shutdown's GOAWAY closes its pooled connection as soon as
// its last stream ends, so "attached but idle" resolves in milliseconds —
// which is what the fast-close tests above measure, not a deadline. What
// still holds Shutdown past the grace with the handler empty is a connection
// net/http itself reports non-idle: an exchange in the post-handler body
// drain, the same window TestAnExchangeStillDrainingKeepsCloseOnTheDrainPath
// protects, here left hanging so the deadline fires over it. Each case still
// bans the message the other proves the code can produce.
func TestTheDeadlineTellsBusyFromAttachedCallers(t *testing.T) {
	const grace = 500 * time.Millisecond

	start := func(t *testing.T) (*Transcript, *configv1.Config, *Daemon) {
		t.Helper()
		log := NewTranscript(nil)
		cfg := testConfig(t, grace)
		d, err := Start(context.Background(), cfg, log.Logf)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		// The staging t.Fatal below would otherwise leave this subtest's
		// daemon serving for the rest of the binary; Close is sync.Once'd,
		// so this is a no-op after the real one.
		t.Cleanup(func() { _ = d.Close() })
		return log, cfg, d
	}
	// assertSide is the point of the test: exactly one side of the deadline
	// diagnostic, never both, and the right one for what the grace was
	// actually spent on.
	assertSide := func(t *testing.T, log *Transcript, want, not string) {
		t.Helper()
		if !log.Printed(want) {
			t.Errorf("the deadline fired and the wrong side of #72 was reported.\nwant %q in:\n%s",
				want, log)
		}
		if log.Printed(not) {
			t.Errorf("both sides of the deadline diagnostic fired at once; %q should not be here:\n%s",
				not, log)
		}
	}

	// Still in the upstream handler at the deadline: the warning, carrying
	// its evidence — and naming the same handler the quiet row does, because
	// the count is one number with one meaning.
	t.Run("in flight", func(t *testing.T) {
		log, cfg, d := start(t)

		entered := make(chan struct{}, 1)
		upstream(t, d, "payments", 30*time.Second, entered)
		c := h2cCaller(t)
		go func() { get(c, cfg.GetProxyPort(), "payments.internal") }()
		<-entered // the request is inside the handler: the gate cannot skip

		if err := d.Close(); err != nil {
			t.Errorf("Close reported a failure for a shutdown that hit its bound: %v", err)
		}
		assertSide(t, log,
			"proxy still busy after the 500ms shutdown grace (1 in the proxy handler)",
			"callers still attached")
	})

	// Handler long done, connection still active in the drain of a body
	// that never finishes arriving: the deadline expires with the in-flight
	// count at zero, and announcing "busy" over that was #72's bug. Asserted
	// on the counter's own claim and no larger — see [Daemon.inflight] for
	// what it does not cover.
	t.Run("attached but idle at the deadline", func(t *testing.T) {
		log, _, d := start(t)

		conn, err := net.Dial("tcp", d.ProxyAddr())
		if err != nil {
			t.Fatalf("dialing the proxy: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		if _, err := io.WriteString(conn,
			"POST / HTTP/1.1\r\nHost: ghost.internal\r\nContent-Length: 1000\r\n\r\n"+
				strings.Repeat("a", 400)); err != nil {
			t.Fatalf("writing the request and the first part of its body: %v", err)
		}
		waitUntil(t, "the handler to be done with the connection still active in the drain", func() bool {
			return d.inflight.Load() == 0 && d.proxyWire.count() == 1
		})

		if err := d.Close(); err != nil {
			t.Errorf("Close reported a failure for a shutdown that hit its bound: %v", err)
		}
		assertSide(t, log,
			"callers still attached to the proxy after the 500ms shutdown grace, with no request in the proxy handler",
			"proxy still busy")
	})
}

// TestARequestVoyaCutsAtShutdownIsRecordedAsVoyas is #200: a request still
// inside the handler when the shutdown grace runs out, with its caller still
// attached, is cut by voya — and the record says so, [proxy.
// StatusClosedByShutdown] rather than 499's "client closed request" for a
// caller that had not left. The upstream port stays on the record: the
// request was routed, and nothing failed at the far end.
//
// Driven through the daemon rather than the proxy package because the
// mechanism is the daemon's: Close closes the proxy's http.Server before it
// cancels the run context, so what tells this cut from a caller's is a flag
// set at the top of Close, and not that context. The plausible wrong fix
// reads the run context instead, and at this level it is *racy* rather than
// simply wrong: Close cancels the run context on its own goroutine right
// after closing the server, and the connection's goroutine reaches the
// ErrorHandler after a few hops, so the run context usually reads cancelled
// by then and the wrong fix records 444 by luck of the ordering (measured
// here 20 of 20 under -race). So the test takes the ordering away: the run
// context is held back until the record exists, which is a wait on the
// record and not a sleep, and a record that depended on it comes out 499.
//
// The record's one surface here is the live line at info under the "proxy
// still busy" line: the log ring is behind a diag service that Close has
// already ended, so this is read from the transcript. And it is waited for,
// not asserted at once: the record is written by the connection's goroutine
// after Close has closed the socket under it, and Close does not wait for
// handlers.
func TestARequestVoyaCutsAtShutdownIsRecordedAsVoyas(t *testing.T) {
	const grace = 200 * time.Millisecond
	log := NewTranscript(nil)
	cfg := testConfig(t, grace)
	d, err := Start(context.Background(), cfg, log.Logf)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	entered := make(chan struct{}, 1)
	port := upstream(t, d, "payments", 30*time.Second, entered)
	c := h2cCaller(t)
	go func() { get(c, cfg.GetProxyPort(), "payments.internal") }()
	<-entered // inside the handler: the drain has something to protect

	// The run context goes only once the cut request is on the record, so
	// whatever the record says, it was not read from that context. Close
	// runs on this goroutine, so the wait's own failure is a test failure
	// and not a hang.
	recorded := func() bool { return log.Printed("payments.internal GET / ") }
	cancel := d.cancel
	d.cancel = func() {
		waitUntil(t, "the cut request's record, before the run context is cancelled", recorded)
		cancel()
	}

	if err := d.Close(); err != nil {
		t.Errorf("Close reported a failure for a shutdown that hit its bound: %v", err)
	}
	if !log.Printed("proxy still busy after the 200ms shutdown grace (1 in the proxy handler)") {
		t.Fatalf("the shutdown did not cut a request in flight, so there is nothing to record:\n%s", log)
	}
	if !recorded() {
		t.Fatalf("Close returned with the cut request not on the record:\n%s", log)
	}
	cut := fmt.Sprintf("payments.internal GET / %d ", proxy.StatusClosedByShutdown)
	to := "-> " + strconv.FormatUint(uint64(port), 10)
	if !log.Printed(cut, to) {
		t.Errorf("a request voya cut at shutdown is not recorded as voya's cut with its upstream port.\nwant %q … %q in:\n%s",
			cut, to, log)
	}
	if log.Printed(fmt.Sprintf(" %d ", proxy.StatusClientClosedRequest)) {
		t.Errorf("a request voya cut at shutdown was recorded as the caller's leaving (%d):\n%s",
			proxy.StatusClientClosedRequest, log)
	}
}

// TestAnExchangeStillDrainingKeepsCloseOnTheDrainPath is the half of #94's
// gate the handler count cannot carry: a caller whose exchange is still on
// the wire when Close arrives keeps it whole. net/http holds a connection
// StateActive after the handler has returned, for as long as the caller
// takes to finish sending its request body — a real caller shape: an upload
// the service answered early — and for a body under net/http's 256KB
// post-handler limit the response is hostage to that drain: before writing
// the header, the server consumes the remainder to decide whether the
// connection can be kept alive (chunkWriter.writeHeader), so the answer the
// handler already wrote is not yet on the wire. Inside that window the
// handler count reads zero; a gate reading [Daemon.inflight] alone skips the
// drain and closes the socket, and the caller loses the exchange whole —
// upload cut mid-body, response never sent. That is #94's dropped-request
// diagnosis reintroduced by the fix for it, and the ConnState-derived count
// is what prevents it: the connection reads active, Close takes the drain
// path, and Shutdown leaves the exchange alone until the caller finishes.
//
// The request is answered by voya's own diagnostic — an unregistered
// authority — because that is the one proxy answer produced without touching
// the request body. A routed exchange cannot stage this window: the reverse
// proxy's handler does not return until the upstream leg is done with the
// inbound body, so through a backend the handler count stays nonzero for as
// long as the body does. The diagnostic answers and returns at once, which
// hands the unread body to net/http's pre-header drain — and the caller
// losing this particular response is losing the very diagnostic that
// explains the failure, which is ADR 005's premise applied at shutdown.
//
// The staging is a raw connection because the sequence is the test: send
// part of the body, hold the rest until Close has committed to a path, and
// only then finish the upload and ask for the response. It cannot be read
// earlier — the server holds it back until the body completes — which is
// also why nothing here waits on response bytes to know the handler ran:
// the two counts are polled instead.
func TestAnExchangeStillDrainingKeepsCloseOnTheDrainPath(t *testing.T) {
	log := NewTranscript(nil)
	cfg := testConfig(t, 10*time.Second)
	d, err := Start(context.Background(), cfg, log.Logf)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	conn, err := net.Dial("tcp", d.ProxyAddr())
	if err != nil {
		t.Fatalf("dialing the proxy: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	br := bufio.NewReader(conn)

	// A POST whose body will still be arriving when Close runs: 400 of
	// 1000 bytes sent, the rest withheld until the test says so.
	if _, err := io.WriteString(conn,
		"POST / HTTP/1.1\r\nHost: ghost.internal\r\nContent-Length: 1000\r\n\r\n"+
			strings.Repeat("a", 400)); err != nil {
		t.Fatalf("writing the request and the first part of its body: %v", err)
	}

	// The state the gate must refuse to skip over: handler done, connection
	// still active in the drain. It converges because the drain holds the
	// connection exactly here until the rest of the body arrives.
	waitUntil(t, "the handler to be done with the connection still active in the drain", func() bool {
		return d.inflight.Load() == 0 && d.proxyWire.count() == 1
	})

	closed := make(chan error, 1)
	go func() { closed <- d.Close() }()
	// Both of Close's paths shut the listener first, so a refused dial
	// means the gate has decided — and the drain path is the only one on
	// which the connection is still open for the rest of the body.
	waitUntil(t, "Close to commit to a path", func() bool {
		probe, err := net.Dial("tcp", d.ProxyAddr())
		if err != nil {
			return true
		}
		_ = probe.Close()
		return false
	})
	select {
	case err := <-closed:
		t.Fatalf("Close returned (error %v) while an accepted exchange was still draining", err)
	default:
	}

	// The caller finishes its upload, and only now can the response exist on
	// the wire: the server's pre-header drain completes, the header and body
	// follow, and on the drain path the connection then ends orderly — a
	// clean EOF. A gate that had skipped answers the upload with a closed
	// socket instead, and nothing below arrives at all.
	if _, err := io.WriteString(conn, strings.Repeat("a", 600)); err != nil {
		t.Fatalf("finishing the request body: %v — Close closed the connection under a caller "+
			"mid-upload (#94)", err)
	}
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("the response did not survive Close (%v): the exchange was cut after its "+
			"handler returned, which is the window the ConnState count exists to cover (#94)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("the staging request got status %d, want the diagnostic's 404 — something "+
			"else answered it", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil || len(body) == 0 {
		t.Fatalf("the response body did not survive Close (body %q, err %v) — the caller lost "+
			"the very diagnostic that explains the failure (#94)", body, err)
	}
	if _, err := br.ReadByte(); err != io.EOF {
		t.Fatalf("after the exchange, the connection ended with %v, want a clean EOF — an abrupt "+
			"close resets the socket, and a caller still draining its response loses it (#94)", err)
	}

	if err := <-closed; err != nil {
		t.Errorf("Close: %v", err)
	}
	if log.Printed("shutdown grace") {
		t.Errorf("Close reported a shutdown-grace deadline it never reached:\n%s", log)
	}
}

// TestAnH2cResponseHeldByFlowControlKeepsCloseOnTheDrainPath is the h2c
// twin of the test above: a response whose handler has returned but whose
// body is still on the server keeps Close on the drain path on that leg too,
// and this is the test #130 asked for. net/http's h2 server hands a
// handler's writes to a per-stream 4KB buffer (http2responseWriter's bufio),
// so a response that fits it and is never flushed by hand — the diagnostic's
// one line, written once — reaches the wire only from the flush handlerDone
// makes after the handler has returned, and each DATA frame then waits on
// the caller's flow-control window. A caller that has stopped reading, the
// h2 shape of a full receive buffer, therefore holds a finished response on
// the server with the handler count already at zero. What carries it is the
// ConnState count: net/http's h2 serve loop drives the hook itself,
// StateActive when a connection's first stream opens and StateIdle when its
// last one closes, so the stream reads active until its END_STREAM frame is
// written. Inside that wait the
// gate reads one on the wire and zero in the handler, takes the drain, and
// the response goes out when the caller reads again; a gate reading the
// handler count alone closes the socket with the body still queued, and the
// caller has the headers and never gets the body — #94's cut on the leg its
// own doc used to call blind. See [connActivity] for what that leaves.
//
// The staging is a raw HTTP/2 connection because the sequence is the test,
// and no Go client will hold its window shut for you: SETTINGS announces a
// stream window of zero, the request goes out, and the response's HEADERS
// arriving is the proof the handler has finished — the h2 server writes
// them only from the post-handler flush, and the DATA behind them cannot
// follow until the window opens. Close is called there, the window is
// opened only once Close has committed to a path, and the body's arrival —
// DATA carrying the diagnostic, then END_STREAM — is what a gate that
// skipped the drain cannot deliver.
//
// The answer is voya's own diagnostic — an unregistered authority — for the
// same reason the test above gives, arrived at down a different road. A
// routed exchange used to stage this and no longer does: since #158 the
// reverse proxy flushes every write through rather than at the end of the
// response, so a routed body under a shut window blocks its handler in the
// flush instead of queueing behind a returned one, and the state this test
// is about (zero in the handler, one on the wire) never arrives. That
// flush is a fix, not a regression — it is what delivers an early answer to
// a caller still sending, which internal/proxy's
// TestAnEarlyResponseReachesACallerStillSending holds — and it leaves this
// window reachable only through the answer voya writes itself.
func TestAnH2cResponseHeldByFlowControlKeepsCloseOnTheDrainPath(t *testing.T) {
	log := NewTranscript(nil)
	cfg := testConfig(t, 10*time.Second)
	d, err := Start(context.Background(), cfg, log.Logf)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	conn, err := net.Dial("tcp", d.ProxyAddr())
	if err != nil {
		t.Fatalf("dialing the proxy: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := io.WriteString(conn, http2.ClientPreface); err != nil {
		t.Fatalf("writing the preface: %v", err)
	}
	fr := http2.NewFramer(conn, conn)
	// A stream window of zero: nothing on any stream may be sent until this
	// caller says so, which is the reading pause the test needs to hold.
	if err := fr.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: 0}); err != nil {
		t.Fatalf("writing SETTINGS: %v", err)
	}
	var block bytes.Buffer
	enc := hpack.NewEncoder(&block)
	for _, h := range []hpack.HeaderField{
		{Name: ":method", Value: http.MethodGet},
		{Name: ":scheme", Value: "http"},
		{Name: ":authority", Value: "ghost.internal"},
		{Name: ":path", Value: "/"},
	} {
		if err := enc.WriteField(h); err != nil {
			t.Fatalf("hpack: %v", err)
		}
	}
	if err := fr.WriteHeaders(http2.HeadersFrameParam{
		StreamID: 1, BlockFragment: block.Bytes(), EndStream: true, EndHeaders: true,
	}); err != nil {
		t.Fatalf("writing HEADERS: %v", err)
	}

	// next reads frames until one the caller is waiting for, acknowledging
	// the server's SETTINGS on the way and letting GOAWAY through: on the
	// drain path the server announces it is leaving, which is not the cut.
	dec := hpack.NewDecoder(4096, nil)
	next := func(what string) http2.Frame {
		t.Helper()
		for {
			f, err := fr.ReadFrame()
			if err != nil {
				t.Fatalf("waiting for %s, the connection ended with %v: the response was cut "+
					"after its handler returned, which is the h2c window #130 is about (#94)", what, err)
			}
			switch f := f.(type) {
			case *http2.SettingsFrame:
				if !f.IsAck() {
					_ = fr.WriteSettingsAck()
				}
			case *http2.HeadersFrame, *http2.DataFrame:
				return f
			case *http2.RSTStreamFrame:
				t.Fatalf("waiting for %s, the server sent RST_STREAM (%v): the response was cut (#94)",
					what, f.ErrCode)
			}
		}
	}

	// The response's HEADERS: the handler is done, and its body is queued
	// behind the window this caller holds shut.
	hf, ok := next("the response headers").(*http2.HeadersFrame)
	if !ok {
		t.Fatal("the first response frame was not HEADERS but DATA — the window was not held, " +
			"and this test stages nothing")
	}
	fields, err := dec.DecodeFull(hf.HeaderBlockFragment())
	if err != nil {
		t.Fatalf("decoding the response headers: %v", err)
	}
	if i := slices.IndexFunc(fields, func(f hpack.HeaderField) bool { return f.Name == ":status" }); i < 0 || fields[i].Value != "404" {
		t.Fatalf("the response headers are %v, want the diagnostic's :status 404 — something "+
			"else answered", fields)
	}
	if hf.StreamEnded() {
		t.Fatal("the response ended with its HEADERS: no body is held behind the window, and " +
			"this test stages nothing")
	}
	// The state the gate must refuse to skip over, and which count carries
	// it: the handler is done, and the stream is still open on the wire.
	// If a Go release stops driving the ConnState hook from its h2 serve
	// loop, this is where it shows, and [connActivity]'s doc is then wrong.
	waitUntil(t, "the handler to be done with the h2c stream still active on the wire", func() bool {
		return d.inflight.Load() == 0 && d.proxyWire.count() == 1
	})

	closed := make(chan error, 1)
	go func() { closed <- d.Close() }()
	waitUntil(t, "Close to commit to a path", func() bool {
		probe, err := net.Dial("tcp", d.ProxyAddr())
		if err != nil {
			return true
		}
		_ = probe.Close()
		return false
	})
	select {
	case err := <-closed:
		t.Fatalf("Close returned (error %v) while a response was still held on the wire "+
			"behind the caller's flow-control window", err)
	default:
	}

	// The caller reads again: the window opens, and only now can the body be
	// sent. On the drain path it arrives whole and the stream ends; a gate
	// that skipped closed the socket under it, and nothing below arrives.
	if err := fr.WriteWindowUpdate(1, 1<<20); err != nil {
		t.Fatalf("opening the window: %v — Close closed the connection under a caller with a "+
			"response still queued for it (#94)", err)
	}
	var body bytes.Buffer
	for {
		f, ok := next("the response body").(*http2.DataFrame)
		if !ok {
			t.Fatal("a second HEADERS frame arrived before the body ended")
		}
		body.Write(f.Data())
		if f.StreamEnded() {
			break
		}
	}
	if !strings.Contains(body.String(), "ghost") {
		t.Fatalf("the response body is %q, want the diagnostic naming the authority nothing "+
			"is registered as — the caller lost the sentence that explains the 404 (ADR 005)",
			body.String())
	}
	// The caller lets go, the way a Go client does once GOAWAY has arrived
	// and its last stream has ended; Close then has nothing to wait on.
	_ = conn.Close()

	if err := <-closed; err != nil {
		t.Errorf("Close: %v", err)
	}
	if log.Printed("shutdown grace") {
		t.Errorf("Close reported a shutdown-grace deadline it never reached:\n%s", log)
	}
}

// TestStartCancellationDoesNotOutliveStartup: the context passed to Start
// bounds the bind, not the daemon. A caller that cancels it — `voya run`'s
// signal context does exactly that — must still get a daemon Close can stop
// gracefully, not one whose requests died with the signal.
func TestStartCancellationDoesNotOutliveStartup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	d, err := Start(ctx, testConfig(t, time.Second), func(string, ...any) {})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	entered := make(chan struct{}, 1)
	upstream(t, d, "other", 0, entered)
	cancel()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://"+d.ProxyAddr()+"/", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Host = "other.internal"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("the proxy stopped serving when Start's context was cancelled: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "finished" {
		t.Errorf("the request got %q after Start's context was cancelled", body)
	}
}

// TestStartRefusesAConfigItCannotRun: Start is reached with a Config built in
// code as well as one Load resolved, and the second kind has to be checked
// too — a zero shutdown_grace expires Close's deadline before it is set.
func TestStartRefusesAConfigItCannotRun(t *testing.T) {
	cfg := testConfig(t, time.Second)
	cfg.ShutdownGrace = nil

	d, err := Start(context.Background(), cfg, func(string, ...any) {})
	if err == nil {
		_ = d.Close()
		t.Fatal("Start accepted a config with no shutdown_grace")
	}
	if !strings.Contains(err.Error(), "shutdown_grace") {
		t.Errorf("the error does not name the field: %v", err)
	}
}

// TestEveryBoundAddressIsReported: all three, including DNS — `voya status`
// and the tests both need to know what was actually bound.
func TestEveryBoundAddressIsReported(t *testing.T) {
	d, err := Start(context.Background(), testConfig(t, time.Second), func(string, ...any) {})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	for _, got := range []string{d.ProxyAddr(), d.RegistrationAddr(), d.DNSAddr()} {
		if !strings.HasPrefix(got, "127.0.0.1:") {
			t.Errorf("bound %q, want a loopback address", got)
		}
	}
}

// TestDNSAnswersOverTCPAndCloseDoesNotWaitOnItsClient: the DNS listener
// voya starts answers on TCP as well as UDP (#184), and a TCP client still
// attached at shutdown does not cost the grace — Close ends the connection
// and waits for the goroutine that served it, the same shape as the proxy's
// idle-caller gate, so nothing is left behind.
func TestDNSAnswersOverTCPAndCloseDoesNotWaitOnItsClient(t *testing.T) {
	grace := time.Second
	d, err := Start(context.Background(), testConfig(t, grace), func(string, ...any) {})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = d.Close()
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, network := range []string{"udp", "tcp"} {
		resolver := &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, d.DNSAddr())
			},
		}
		addrs, err := resolver.LookupHost(ctx, "payments.internal")
		if err != nil {
			t.Fatalf("LookupHost over %s: %v", network, err)
		}
		if len(addrs) != 1 || addrs[0] != "127.0.0.1" {
			t.Errorf("LookupHost over %s = %v, want [127.0.0.1]", network, addrs)
		}
	}

	// A client attached and idle — connected, nothing on the wire.
	idle, err := net.Dial("tcp", d.DNSAddr())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer idle.Close()

	start := time.Now()
	closed = true
	if err := d.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if took := time.Since(start); took >= grace {
		t.Errorf("Close took %s with an idle DNS TCP client attached: the %s grace was burned on it", took, grace)
	}
	if err := idle.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	if _, err := idle.Read(make([]byte, 1)); err == nil {
		t.Error("the idle DNS TCP client was not disconnected by Close")
	}
}

// TestAPortConflictIsRecognisedOnEitherTransport. The DNS listener is UDP and
// TCP on one number (#184), so a conflict there is reported by a different
// errno path and looked up with a different lsof selector than the two TCP
// listeners — and a holder on either of its transports has to stop it, since
// a voya answering on one and not the other is the failure #184 removed.
//
// Each case holds the port voya was configured for, rather than drawing one
// of its own and writing that number into the config. Writing one in is what
// this used to do, and it can collide: [FreePorts] draws across both
// transports, so a TCP port held here and the UDP port drawn for DNS are two
// different sockets that can carry the same number, and overwriting would
// leave two of voya's three ports equal. [config.Validate] refuses that
// before anything binds — so the test would meet a validation failure where
// it expects a bind conflict, and report a missing "already in use".
func TestAPortConflictIsRecognisedOnEitherTransport(t *testing.T) {
	for _, tc := range []struct {
		what      string
		transport string
		port      func(*configv1.Config) uint32
		hold      func(addr string) (io.Closer, error)
	}{
		{"proxy", "tcp",
			func(c *configv1.Config) uint32 { return c.GetProxyPort() },
			func(addr string) (io.Closer, error) { return net.Listen("tcp", addr) }},
		{"dns", "udp",
			func(c *configv1.Config) uint32 { return c.GetDnsPort() },
			func(addr string) (io.Closer, error) { return net.ListenPacket("udp", addr) }},
		{"dns", "tcp",
			func(c *configv1.Config) uint32 { return c.GetDnsPort() },
			func(addr string) (io.Closer, error) { return net.Listen("tcp", addr) }},
	} {
		t.Run(tc.what+" "+tc.transport, func(t *testing.T) {
			cfg := testConfig(t, time.Second)
			port := tc.port(cfg)
			taken, err := tc.hold(config.Addr(port))
			if err != nil {
				t.Fatalf("holding the %s port %d: %v", tc.what, port, err)
			}
			defer taken.Close()

			d, err := Start(context.Background(), cfg, func(string, ...any) {})
			if err == nil {
				_ = d.Close()
				t.Fatal("voya started on a port that was already taken")
			}
			// The message is for a developer; ErrPortInUse is what code
			// classifies on, so that improving the wording cannot silently
			// change what callers do — the acceptance suite retries on this
			// and on nothing else.
			if !errors.Is(err, ErrPortInUse) {
				t.Errorf("a port conflict does not match ErrPortInUse: %v", err)
			}
			message := err.Error()
			for _, want := range []string{strconv.FormatUint(uint64(port), 10), tc.what, "already in use"} {
				if !strings.Contains(message, want) {
					t.Errorf("the failure does not mention %q:\n%s", want, message)
				}
			}
			t.Logf("%s conflict reads:\n%s", tc.what, message)
		})
	}
}

// TestEvictionAndReclaimBothReachTheTerminal: voya's own log is where someone
// watching the fleet is looking, and both halves of a name changing hands
// have to arrive there. A reclaim that only the reclaiming service knows
// about is the silent failure #27 was careful to avoid.
func TestEvictionAndReclaimBothReachTheTerminal(t *testing.T) {
	log := NewTranscript(nil)

	d, err := Start(context.Background(), testConfig(t, 100*time.Millisecond), log.Logf)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	reg := d.Registry()

	deposed, err := reg.Register(registry.Instance{Service: "payments", Port: 1001, PID: 111})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	usurper, err := reg.Register(registry.Instance{Service: "payments", Port: 1002, PID: 222})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, gone := deposed.EvictedBy(); !gone {
		t.Fatal("the first instance was not evicted")
	}
	// One call rather than two, so that "both instances" is what is checked:
	// two calls would pass on the two ports being in an announcement each,
	// which is what a reclaim announced after this point would make of them.
	if !log.Announced("port 1001", "port 1002") {
		t.Errorf("the eviction warning does not name both instances:\n%s", log)
	}

	// The replacement goes away and the original takes its name back.
	usurper.Close()
	if _, err := reg.RegisterIfFree(registry.Instance{Service: "payments", Port: 1001, PID: 111}); err != nil {
		t.Fatalf("RegisterIfFree: %v", err)
	}
	// Matched on the reclaim's own sentence, and — through
	// [Transcript.Announced] — on the [AnnouncementMarker] both warnings
	// carry. That marker is the claim #27 makes: the reclaim is announced
	// *as loudly* as the eviction, so a reclaim demoted to an ordinary log
	// line has to fail here rather than pass on the strength of the words
	// still being somewhere in the output.
	if !log.Announced("went back to the instance that was deposed earlier") {
		t.Errorf("the reclaim was not announced in voya's own log:\n%s", log)
	}
}

// TestOnlyAnnouncementsAreAnnounced pins what [Transcript.Announced] is for.
// Both suites that assert on ADR 002's announcements now ask it the question,
// so a matcher that answered yes to an ordinary log line would take both of
// them with it — silently, since every assertion they make is a positive one.
//
// The ordinary line is the control, and it is the real hazard rather than an
// invented one: the startup banner carries three ephemeral ports, and 51001
// contains "1001". It is a line this test writes rather than one lifted from
// the announcement it is meant to be distinguished from, and [Transcript.Printed]
// confirms it was captured at all — a control the transcript never saw would
// pass the check below for the wrong reason.
func TestOnlyAnnouncementsAreAnnounced(t *testing.T) {
	tr := NewTranscript(nil)
	// Through %s rather than as the format itself: [Transcript.Logf] runs its
	// format through fmt.Sprintf, so a control that grew a percent — a
	// percent-encoded path, a duration written as a rate — would be captured
	// as %!x(MISSING) while [Transcript.Printed] below still passed on the
	// port number, and the check this line exists for would silently be
	// asking about a string voya never saw. internal/daemon/log.go's
	// stdLogger takes the same care.
	tr.Logf("%s", Banner+"127.0.0.1:51001, registration on 51002, dns on 51003")
	evictionWarning(tr.Logf)(registry.Eviction{
		Evicted:  registry.Instance{Service: "payments", Port: 1001, PID: 111},
		Incoming: registry.Instance{Service: "payments", Port: 1002, PID: 222},
	})

	if !tr.Announced("port 1001", "port 1002") {
		t.Errorf("the eviction warning was not recognised as an announcement:\n%s", tr)
	}
	if !tr.Printed("51001") {
		t.Fatalf("the ordinary line was not captured, so the check below proves nothing:\n%s", tr)
	}
	if tr.Announced("51001") {
		t.Errorf("an ordinary log line answered as an announcement:\n%s", tr)
	}
}

// TestTheAnnouncementMarkerIsProminent guards the one value both warnings and
// every matcher now read. ADR 002 asks for prominence, and with a single
// definition nothing else in the repo would notice it being reworded into
// something that is not: a marker matching every line would leave every
// assertion in [TestOnlyAnnouncementsAreAnnounced]'s neighbourhood passing on
// the startup banner.
//
// This is what the two literals used to do for each other, and it is the cost
// of collapsing them: reword one of a pair and both suites went red, so the
// value was checked even though nothing said so. Checked on the shape rather
// than pinned to the string, because the wording is allowed to change and the
// prominence is not — "\n." starts a block and is not blank, and would go
// past unremarked with only the first two of these.
func TestTheAnnouncementMarkerIsProminent(t *testing.T) {
	if !strings.HasPrefix(AnnouncementMarker, "\n") {
		t.Errorf("AnnouncementMarker = %q does not start a new block, so an announcement lands at the end of whatever was printed last",
			AnnouncementMarker)
	}
	if strings.TrimSpace(AnnouncementMarker) == "" {
		t.Errorf("AnnouncementMarker = %q is only whitespace, so nothing distinguishes an announcement from an ordinary line",
			AnnouncementMarker)
	}
	if !strings.HasSuffix(AnnouncementMarker, " ") {
		t.Errorf("AnnouncementMarker = %q does not end in a space, so the mark runs into the first word of the announcement",
			AnnouncementMarker)
	}
	if mark := []rune(strings.TrimSpace(AnnouncementMarker)); len(mark) < 2 {
		t.Errorf("AnnouncementMarker = %q carries %d visible character, which is what an ordinary sentence starts with; ADR 002 asks for a mark that cannot be read past",
			AnnouncementMarker, len(mark))
	}

	// And the rest of the format string, which the marker alone says nothing
	// about: an announcementFormat that had lost its marker would be
	// recognised by nothing, and one that had lost its trailing newline would
	// have whatever voya printed next abut it — cmd/voya's logf appends one
	// newline, and this is the one that turns the pair into the blank line
	// closing the block.
	if !strings.HasPrefix(announcementFormat, AnnouncementMarker) {
		t.Errorf("announcementFormat = %q does not open with AnnouncementMarker, so nothing the two warnings write reads as an announcement",
			announcementFormat)
	}
	if !strings.HasSuffix(announcementFormat, "\n") {
		t.Errorf("announcementFormat = %q does not close the block, so the next line printed lands against the end of the announcement",
			announcementFormat)
	}
}

// TestTheBannerMarksOnlyTheStartupLine is [TestTheAnnouncementMarkerIsProminent]
// for the repo's other collapsed literal (#66). [Banner] is what both
// real-binary harnesses — cmd/voya's and test/acceptance's — take as "all
// three listeners are bound", and collapsing the copies gave up the way they
// cross-checked each other, exactly as #57's did for the marker.
//
// The hazard is not a wording nothing prints: that fails both harnesses on
// their next run, loudly. It is a Banner that matches *too early*. Both waits
// are a strings.Contains over everything voya has written so far, so a Banner
// shortened to the "voya: " prefix every line shares returns on the first
// thing voya says rather than after the third bind — which is the flake the
// banner exists in place of a dial to prevent — and an empty one is contained
// in every string, so both return before voya has bound anything at all.
//
// Checked against a daemon's other output rather than pinned to the wording,
// which is ADR 001's prose and free to be improved.
func TestTheBannerMarksOnlyTheStartupLine(t *testing.T) {
	if strings.TrimSpace(Banner) == "" {
		t.Fatalf("Banner = %q is contained in every string, so both harnesses' readiness waits return before voya has bound anything",
			Banner)
	}

	log := NewTranscript(nil)
	d, err := Start(context.Background(), testConfig(t, 100*time.Millisecond), log.Logf)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	if !log.Printed(Banner) {
		t.Fatalf("Start did not print Banner, so nothing waiting for it ever comes up:\n%s", log)
	}

	// The rest of what one daemon says, so that "marks the startup line" is
	// measured against a corpus rather than asserted: the INFO band `voya run`
	// writes around the daemon's life, and ADR 002's announcement. A Banner
	// carrying nothing but the prefix all three share matches all three.
	d.Infof("voya: stopping (grace %s)", 100*time.Millisecond)
	evictionWarning(log.Logf)(registry.Eviction{
		Evicted:  registry.Instance{Service: "payments", Port: 1001, PID: 111},
		Incoming: registry.Instance{Service: "payments", Port: 1002, PID: 222},
	})
	if got := strings.Count(log.String(), Banner); got != 1 {
		t.Errorf("Banner = %q appears %d times in one daemon's output; a harness waiting for it cannot tell the startup line from the rest:\n%s",
			Banner, got, log)
	}
}

// TestAnnouncedWantsOneAnnouncement pins the half of [Transcript.Announced]'s
// contract that [AnnouncementMarker] says nothing about: every want inside
// the *same* announcement.
//
// It is what makes the acceptance suite's reclaim assertion an assertion.
// That test waits on "went back to the instance that was deposed earlier"
// together with the incumbent's port and PID — and the eviction that came
// first names that same instance, so a matcher that looked each want up
// across the whole transcript would let the eviction answer for a reclaim
// that never happened. Both suites now ask one implementation, so the
// question is asked once, here, with both announcements staged.
func TestAnnouncedWantsOneAnnouncement(t *testing.T) {
	tr := NewTranscript(nil)
	evictionWarning(tr.Logf)(registry.Eviction{
		Evicted:  registry.Instance{Service: "payments", Port: 1001, PID: 111},
		Incoming: registry.Instance{Service: "payments", Port: 1002, PID: 222},
	})
	reclaimWarning(tr.Logf)(registry.Reclaim{
		Instance: registry.Instance{Service: "payments", Port: 1001, PID: 111},
	})

	// Each sentence is in an announcement of its own, which is what makes the
	// check below a check: a matcher that spanned the two would find both.
	const (
		eviction = "was taken over by another instance"
		reclaim  = "went back to the instance that was deposed earlier"
	)
	if !tr.Announced(eviction) || !tr.Announced(reclaim) {
		t.Fatalf("both announcements have to be on the transcript for this test to mean anything:\n%s", tr)
	}
	if tr.Announced(eviction, reclaim) {
		t.Errorf("two announcements answered together, so an eviction can answer for the reclaim that has not happened yet:\n%s", tr)
	}
}

// TestLogLevelDecidesWhatIsPrinted is #43: log_level has been a real,
// validated field since #13 and was read by nothing, so a developer who asked
// voya to be quiet got a clean resolve, a clean validation, and exactly the
// output they had before. A field that validates and does nothing is worse
// than an absent one — the validation is what makes it look wired up.
//
// The table is its own control. Every row stages the identical eviction from
// the same three lines, so a row asserting the announcement is *absent*
// cannot be passing because the staging quietly did nothing: the rows below
// it print it.
//
// LOG_LEVEL_ERROR silencing the eviction is the deliberate reading of a
// conflict, not an oversight. The config schema documents LOG_LEVEL_WARN as
// "failures plus the events ADR 002 requires be impossible to miss", which
// puts the announcement at WARN; an ERROR that still printed it would be a
// synonym for WARN, and "verbosity floor" would not be true of the level most
// likely to be set for quiet. ADR 002's obligation is met at the default and
// at every level above ERROR.
func TestLogLevelDecidesWhatIsPrinted(t *testing.T) {
	for _, tc := range []struct {
		level configv1.LogLevel
		// atInfo is the LOG_LEVEL_INFO band: the startup line, and what
		// [Daemon.Infof] prints for `voya run` around the daemon's life.
		atInfo bool
		// eviction is ADR 002's announcement, written at LOG_LEVEL_WARN.
		eviction bool
	}{
		{configv1.LogLevel_LOG_LEVEL_ERROR, false, false},
		{configv1.LogLevel_LOG_LEVEL_WARN, false, true},
		{configv1.LogLevel_LOG_LEVEL_INFO, true, true},
		{configv1.LogLevel_LOG_LEVEL_DEBUG, true, true},
	} {
		t.Run(tc.level.String(), func(t *testing.T) {
			cfg := testConfig(t, 100*time.Millisecond)
			cfg.LogLevel = tc.level

			log := NewTranscript(nil)
			d, err := Start(context.Background(), cfg, log.Logf)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(func() {
				if err := d.Close(); err != nil {
					t.Errorf("Close: %v", err)
				}
			})

			// The staging, identical in every row: a name taken over by a
			// second instance, which is the event ADR 002 obliges voya to
			// announce.
			reg := d.Registry()
			if _, err := reg.Register(registry.Instance{Service: "payments", Port: 1001, PID: 111}); err != nil {
				t.Fatalf("Register: %v", err)
			}
			if _, err := reg.Register(registry.Instance{Service: "payments", Port: 1002, PID: 222}); err != nil {
				t.Fatalf("Register: %v", err)
			}

			// What `voya run` says on the way out, through the same sink
			// (cmd/voya/main.go). Asserted here because it is the half of the
			// field that used to be missed: a developer told the daemon is
			// stopping after asking for LOG_LEVEL_ERROR has log_level
			// half-read, which is the shape #43 was filed about.
			d.Infof("voya: stopping (grace %s)", 100*time.Millisecond)

			if got := log.Printed(Banner); got != tc.atInfo {
				t.Errorf("startup banner printed = %v, want %v at %s:\n%s",
					got, tc.atInfo, tc.level, log)
			}
			if got := log.Printed("voya: stopping"); got != tc.atInfo {
				t.Errorf("Daemon.Infof printed = %v, want %v at %s:\n%s",
					got, tc.atInfo, tc.level, log)
			}
			if got := log.Announced("was taken over by another instance"); got != tc.eviction {
				t.Errorf("eviction announced = %v, want %v at %s:\n%s",
					got, tc.eviction, tc.level, log)
			}
		})
	}
}

// TestAForgedPIDIsAnnouncedAndRecorded is #308's criterion for the takeover:
// logged and recorded. The registry's own tests prove it counts as an eviction.
// This one proves that reaches the terminal and the history `voya status` reads.
func TestAForgedPIDIsAnnouncedAndRecorded(t *testing.T) {
	log := NewTranscript(nil)
	d, err := Start(context.Background(), testConfig(t, 100*time.Millisecond), log.Logf)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	reg := d.Registry()
	if _, err := reg.Register(registry.Instance{Service: "payments", Port: 1001, PID: 111}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := reg.Register(registry.Instance{Service: "payments", Port: 1002, PID: 111}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if !log.Announced("was taken over by another instance") {
		t.Errorf("the takeover was not announced:\n%s", log)
	}
	got := reg.Snapshot().Evictions
	if len(got) != 1 || got[0].Evicted.Port != 1001 || got[0].Incoming.Port != 1002 {
		t.Errorf("recorded evictions = %+v, want the one from port 1001 to port 1002", got)
	}
}

// TestStartAcceptsANilLogf pins what the nil guard in [newSink] is for. Start
// has taken a nil logf as "say nothing" since #18, and moving the guard into
// the sink is where that could have been dropped silently — nothing else here
// passes nil, so the first caller to find out would be a panic on the startup
// banner.
func TestStartAcceptsANilLogf(t *testing.T) {
	d, err := Start(context.Background(), testConfig(t, 100*time.Millisecond), nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	// Both bands, since the banner and the announcements reach the sink by
	// different routes: one through at() at startup, one through a registry
	// handler bound at construction.
	if _, err := d.Registry().Register(registry.Instance{Service: "payments", Port: 1001, PID: 111}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := d.Registry().Register(registry.Instance{Service: "payments", Port: 1002, PID: 222}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	d.Infof("and this one too")
}

// TestHTTPServersWriteThroughTheSink pins the wiring in [Start] that keeps
// net/http's own messages inside log_level.
//
// [proxy.WithErrorLog] covers what the proxy and httputil write; the
// http.Server above them is a separate logger, and one left nil writes to
// stderr through the standard logger at every configured level. Nothing in
// this package's tests can provoke those messages reliably — they want a
// broken h2 preface or a panicking handler — so the assertion is on the
// wiring: both servers have an ErrorLog, and what it is handed arrives here.
func TestHTTPServersWriteThroughTheSink(t *testing.T) {
	log := NewTranscript(nil)
	d, err := Start(context.Background(), testConfig(t, 100*time.Millisecond), log.Logf)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	for _, srv := range []struct {
		name string
		srv  *http.Server
	}{
		{"proxy", d.proxySrv},
		{"registration", d.registerSrv},
	} {
		if srv.srv.ErrorLog == nil {
			t.Errorf("the %s server has no ErrorLog, so net/http writes its own failures "+
				"to stderr whatever log_level says", srv.name)
			continue
		}
		// The shape net/http actually writes, minus the parts that vary.
		srv.srv.ErrorLog.Printf("http: panic serving %s", srv.name)
		if !log.Printed("http: panic serving " + srv.name) {
			t.Errorf("the %s server's ErrorLog did not reach voya's sink:\n%s", srv.name, log)
		}
	}
}

// TestTheReservedNameServesTheJWKS is #209 at the daemon, which is where the
// two internal surfaces meet: the same mux that claims DiagService's
// procedures serves the JWKS document, and exactly [auth.JWKSPath] is
// claimed — the trailing-slash variant and the parent /.well-known/ answer
// ADR 005's reserved row unchanged, and a POST to the claimed path is the
// handler's 405, not the row's 501.
func TestTheReservedNameServesTheJWKS(t *testing.T) {
	cfg := testConfig(t, time.Second) // isolates the configuration directory too

	d, err := Start(context.Background(), cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("starting voya: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("stopping voya: %v", err)
		}
	})

	c := http11Caller(t)
	do := func(method, path string) (int, http.Header, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), method,
			"http://127.0.0.1:"+strconv.FormatUint(uint64(cfg.GetProxyPort()), 10)+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = registry.Authority(registry.Reserved)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("reading %s %s: %v", method, path, err)
		}
		return resp.StatusCode, resp.Header, string(body)
	}

	status, header, body := do(http.MethodGet, auth.JWKSPath)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200; body:\n%s", auth.JWKSPath, status, body)
	}
	if got := header.Get("Content-Type"); got != auth.JWKSContentType {
		t.Errorf("content-type = %q, want %q", got, auth.JWKSContentType)
	}
	if set, err := jwk.Parse([]byte(body)); err != nil || set.Len() != 1 {
		t.Errorf("the body is not a one-key JWK Set (err %v):\n%s", err, body)
	}

	if status, _, body := do(http.MethodPost, auth.JWKSPath); status != http.StatusMethodNotAllowed {
		t.Errorf("POST %s = %d, want the handler's 405 — neither the reserved row's 501 nor a 200; body:\n%s",
			auth.JWKSPath, status, body)
	}
	for _, path := range []string{auth.JWKSPath + "/", "/.well-known/"} {
		if status, _, _ := do(http.MethodGet, path); status != http.StatusNotImplemented {
			t.Errorf("GET %s = %d, want the reserved row's 501 — only %s itself is claimed",
				path, status, auth.JWKSPath)
		}
	}
}

// TestRateLimitConfigReachesTheProxy is #223's config seam, end to end: a
// daemon whose layer flipped `rate_limit.enabled` throttles at base.pxf's
// own thresholds — the budget spends, the next request answers 429 with a
// Retry-After no longer than the window — while the reserved voya.internal
// keeps answering past any budget, which is what lets `voya status` and
// `voya dns check` investigate the very throttle. The limiter's edges live in
// internal/proxy's own tests; this proves the daemon hands the configured
// values to the proxy at all.
func TestRateLimitConfigReachesTheProxy(t *testing.T) {
	cfg := testConfig(t, time.Second)
	// Only the switch: the budget and window stay base.pxf's, so this test
	// spends the shipped default and drifts with it on purpose.
	cfg.RateLimit.Enabled = true
	budget := int(cfg.GetRateLimit().GetRequests())
	window := cfg.GetRateLimit().GetWindow().AsDuration()

	d, err := Start(context.Background(), cfg, func(string, ...any) {})
	if err != nil {
		t.Fatalf("starting voya: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("stopping voya: %v", err)
		}
	})
	entered := make(chan struct{}, budget+1)
	upstream(t, d, "payments", 0, entered)

	c := http11Caller(t)
	status := func(authority, path string) (int, http.Header) {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
			"http://127.0.0.1:"+strconv.FormatUint(uint64(cfg.GetProxyPort()), 10)+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = authority
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("GET %s%s: %v", authority, path, err)
		}
		defer resp.Body.Close()
		if _, err := io.ReadAll(resp.Body); err != nil {
			t.Fatalf("reading %s%s: %v", authority, path, err)
		}
		return resp.StatusCode, resp.Header
	}

	for i := range budget {
		if got, _ := status("payments.internal", "/"); got != http.StatusOK {
			t.Fatalf("request %d of the budget: status = %d, want 200", i+1, got)
		}
	}
	got, header := status("payments.internal", "/")
	if got != http.StatusTooManyRequests {
		t.Fatalf("request over the budget: status = %d, want 429", got)
	}
	seconds, err := strconv.Atoi(header.Get("Retry-After"))
	if err != nil || seconds <= 0 || time.Duration(seconds)*time.Second > window {
		t.Errorf("Retry-After = %q, want whole seconds within (0, %s]", header.Get("Retry-After"), window)
	}

	for i := range budget + 1 {
		if got, _ := status(registry.Authority(registry.Reserved), auth.JWKSPath); got != http.StatusOK {
			t.Errorf("JWKS fetch %d past the budget: status = %d, want 200 — voya's own surface was throttled", i+1, got)
		}
	}
}

// TestConfiguredCORSOriginsReachTheProxy proves the config field is wired to
// the running daemon: an origin outside the local set is refused until
// cors_origins names it.
func TestConfiguredCORSOriginsReachTheProxy(t *testing.T) {
	const origin = "https://app.example.test"
	for _, tc := range []struct {
		name string
		list []string
		want string
	}{
		{"unnamed", nil, ""},
		{"named", []string{origin}, origin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t, time.Second)
			cfg.CorsOrigins = tc.list
			d, err := Start(context.Background(), cfg, func(string, ...any) {})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(func() { _ = d.Close() })

			url := fmt.Sprintf("http://127.0.0.1:%d/", cfg.GetProxyPort())
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Host = "payments.internal"
			req.Header.Set("Origin", origin)
			res, err := http11Caller(t).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = res.Body.Close() }()
			if got := res.Header.Get("Access-Control-Allow-Origin"); got != tc.want {
				t.Errorf("Allow-Origin = %q, want %q", got, tc.want)
			}
		})
	}
}
