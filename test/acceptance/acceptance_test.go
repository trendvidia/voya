// Package acceptance holds the test that is the product in one sentence.
//
//	Two connect-go services are running. One dials other.internal:9777 and
//	the request arrives. Restart the callee — it comes back on a different
//	ephemeral port — and traffic follows, with no configuration change
//	anywhere.
//
// Everything voya does exists to make that paragraph true. The tests here
// run a real daemon, real services using the real client package, and the
// real dial strings; nothing is stubbed, and no sudo or DNS configuration is
// involved, because the client dialer is the resolution path (ADR 002 as
// amended in #10).
package acceptance

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"
	voyareflect "github.com/trendvidia/voya/client/reflection"
	"google.golang.org/protobuf/types/known/wrapperspb"

	voya "github.com/trendvidia/voya/client"
	"github.com/trendvidia/voya/internal/config"
	"github.com/trendvidia/voya/internal/daemon"
	configv1 "github.com/trendvidia/voya/proto/voya/config/v1"
)

// procedure is one unary method: it answers with whatever identity the
// service was built with, so a caller can tell which instance replied.
const procedure = "/voya.acceptance.v1.Echo/Whoami"

// sleepPrefix asks the handler to hold a request open for the duration that
// follows it. Only the subprocess callee in sigkill_test.go is ever sent one:
// it is what makes "killed mid-request" stageable, since the caller is then
// waiting on a response that is never coming.
const sleepPrefix = "sleep:"

// callerTimeout is the client-side timeout every caller in this suite carries.
// It is the outer bound on every "voya answered rather than hanging" claim
// here, so nothing may wait on voya for longer — see [diagnosticDeadline].
const callerTimeout = 20 * time.Second

// diagnosticDeadline is how long a test waits for voya's answer before calling
// it a hang.
//
// Derived from [callerTimeout] rather than written down, because the ordering
// is the whole assertion: past that timeout the error a test sees is the
// caller's own, which is a *url.Error quoting the dial string — so it names
// the service, reads like a diagnostic, and would pass for exactly the hang
// ADR 005 rules out. Half leaves the margin unmistakable.
const diagnosticDeadline = callerTimeout / 2

// --- the fleet ---------------------------------------------------------------

// voyad is a running voya on ephemeral ports, so the suite never collides
// with a developer's own daemon on 9777.
type voyad struct {
	*daemon.Daemon

	// log is everything this daemon printed.
	//
	// Every daemon this suite runs carries one, rather than the tests that
	// assert on voya's output getting a separate start helper (#47). Two
	// reasons. voya's own terminal is a deliverable and not a side effect —
	// ADR 002 obliges an eviction and a reclaim to be announced there — so a
	// suite that starts a daemon and reads nothing it prints has no way to
	// fail when an announcement stops arriving, and several tests here
	// probably should assert on it. And a second start path would be a second
	// copy of [startOnFreePorts]' retry, which is the copy that stops being
	// updated.
	//
	// The two [daemon.Start] calls below that discard their output are the
	// ones expected to fail, and they fail before there is any output to
	// discard: the startup banner is written only once all three listeners
	// are bound.
	//
	// The capture itself is [daemon.Transcript], which is where the reasoning
	// that used to live here — one entry per logf call rather than one per
	// line, the lock against the goroutines this suite does not sequence, and
	// how an announcement is told from an ordinary line — now lives (#57). It
	// used to be written out twice, here and in internal/daemon's own tests,
	// with [daemon.AnnouncementMarker] spelled as a bare literal in both.
	log *daemon.Transcript

	// configDir is the temp directory [startOnFreePorts] pointed
	// $VOYA_CONFIG_DIR at, kept so a test can assert that the signing key
	// the daemon mints at start (#219) landed there and nowhere else.
	configDir string

	proxyPort        uint32
	registrationPort uint32
}

func startVoya(t *testing.T) *voyad {
	t.Helper()
	v, _ := startOnFreePorts(t)
	return v
}

// newVoyaLog is the capture every daemon this suite starts is given: what
// voya printed, kept so a test can assert on it, and teed to t.Logf so that a
// failure still reads in order alongside everything else the test logged.
//
// The teeing is why nothing here renders [daemon.Transcript.String] on
// failure. Every message is already in the test's own output, where a
// `go test` failure prints it.
func newVoyaLog(t *testing.T) *daemon.Transcript {
	return daemon.NewTranscript(t.Logf)
}

// announced reports whether voya made one announcement containing want and
// all of more. See [daemon.Transcript.Announced] for what makes an
// announcement one, and why the first want is not variadic.
func (v *voyad) announced(want string, more ...string) bool { return v.log.Announced(want, more...) }

// startOnFreePorts starts a daemon on ports nothing else holds, retrying if
// something took one in between, and stops it when the test ends.
//
// The retry is not defensive programming, it is a race that happens: the
// ports come from [daemon.FreePorts], which binds each one and gives it
// straight back, so there is a window between choosing a port and the daemon
// binding it. It cannot be closed without handing the daemon pre-bound
// listeners, which would mean testing a code path production does not use.
//
// What actually failed a `make ci` run was not that window, though — "port
// 53441 (dns) is already in use, held by Google Chrome Helper". TCP and UDP
// are separate port spaces, the DNS listener binds UDP, and this suite drew
// its DNS port by probing TCP: a UDP port a browser had held for minutes
// looked free on every attempt, and retrying would have re-rolled the same
// blind dice five times. Drawing each port on the transport its listener
// actually binds is what fixes that one; the retry covers what is left.
//
// Only this helper retries. A test that deliberately takes a port and expects
// voya to complain must not.
//
// tweaks edit the configuration before the daemon starts — how the rate-limit
// tests turn the limiter on (#224) — and are re-applied on every retry, since
// each attempt draws fresh ports into a fresh config.
func startOnFreePorts(t *testing.T, tweaks ...func(*configv1.Config)) (*voyad, *configv1.Config) {
	t.Helper()
	// A configuration directory of this test's own, because Start mints the
	// signing key there (#219): without this, every daemon this suite starts
	// would write key material into the developer's real ~/.config/voya —
	// or refuse to start on a host with no home directory at all.
	// TestTheSigningKeyLandsInTheHarnessDir is the proof it takes effect.
	configDir := t.TempDir()
	t.Setenv(config.DirEnv, configDir)
	var lastErr error
	for attempt := range 5 {
		cfg := freeConfig(t)
		for _, tweak := range tweaks {
			tweak(cfg)
		}
		// A fresh capture per attempt, so that a daemon which failed to bind
		// leaves nothing behind for the one that succeeds to be asserted on.
		log := newVoyaLog(t)
		d, err := daemon.Start(context.Background(), cfg, log.Logf)
		if err == nil {
			// Registered here rather than at each call site, so that no
			// caller can leave three listeners bound and three goroutines
			// serving for the rest of the binary by failing before it got
			// round to its own cleanup. Close is idempotent, so a test that
			// stops the daemon itself — the restart one below — still can.
			t.Cleanup(func() {
				if err := d.Close(); err != nil {
					t.Errorf("stopping voya: %v", err)
				}
			})
			return &voyad{
				Daemon:           d,
				log:              log,
				configDir:        configDir,
				proxyPort:        cfg.GetProxyPort(),
				registrationPort: cfg.GetRegistrationPort(),
			}, cfg
		}
		lastErr = err
		// By identity, not by phrase. The message is ADR 001's prose for a
		// developer and is meant to be improved; matching it here would make
		// a reworded diagnostic turn this retry off with nothing to say so.
		if !errors.Is(err, daemon.ErrPortInUse) {
			break // a real failure; retrying would only hide it
		}
		t.Logf("attempt %d: %v; retrying with fresh ports", attempt+1, err)
	}
	t.Fatalf("starting voya: %v", lastErr)
	return nil, nil
}

// freeConfig is a daemon configuration on three ports the OS just handed out.
//
// The draw itself lives in [daemon.FreePorts]. It used to live here and in
// internal/daemon as the same twenty lines, and until #44's review both
// copies drew every port on TCP — including the DNS one, which is bound with
// UDP. Deriving the transport from the daemon's own listener table is what
// stops that being a thing this file can get wrong, or get wrong again (#46).
func freeConfig(t *testing.T) *configv1.Config {
	t.Helper()
	ports, err := daemon.FreePorts(t.Context())
	if err != nil {
		t.Fatalf("drawing free ports: %v", err)
	}
	cfg, err := configOn(ports.Proxy, ports.DNS, ports.Registration)
	if err != nil {
		t.Fatalf("loading the committed defaults: %v", err)
	}
	return cfg
}

// configOn is the configuration every daemon this suite runs gets: the given
// ports on top of the committed defaults. One definition rather than one per
// start path, because the daemon subprocess in errorband_test.go builds its
// config from ports handed over the environment, and a second copy is the one
// that stops being updated. Starting from config.Load rather than restating
// the defaults field by field is the same argument one level down: #150
// (request_log) and #208 (auth) each found the restated copy missing a field
// the moment the schema grew. "" is no user layer, so this is deterministic.
func configOn(proxy, dns, registration uint32) (*configv1.Config, error) {
	cfg, err := config.Load(context.Background(), "")
	if err != nil {
		return nil, err
	}
	cfg.ProxyPort = proxy
	cfg.DnsPort = dns
	cfg.RegistrationPort = registration
	return cfg, nil
}

// loopback is a voya listener address for port, spelled the way every one of
// them is: the 127.0.0.1 literal, never a hostname and never 0.0.0.0 (ADR
// 002).
func loopback(port uint32) string {
	return net.JoinHostPort("127.0.0.1", strconv.FormatUint(uint64(port), 10))
}

// echoHandler is the one service handler this suite serves, in this process
// and in the subprocess callee alike. It answers with the identity it was
// built with and the authority the request arrived on, so a caller can assert
// on both which instance replied and what name it was dialled by.
//
// A payload starting with [sleepPrefix] is held for the duration that follows
// it instead. onHold, when non-nil, is called once the request is inside the
// handler: the subprocess announces it on stdout, which is how a test staging
// a kill mid-request waits for the request to actually be there rather than
// guessing at a delay.
func echoHandler(identity string, onHold func()) http.Handler {
	mux := http.NewServeMux()
	// grpc.health.v1 over Connect, answering SERVING for the server as a
	// whole: ADR 004 says every fleet service serves one, and `voya health`
	// (#154) is its first local consumer — TestHealthCheckReachesAServiceByName
	// is the proof that ADR 004's "a plain HTTP POST to
	// /grpc.health.v1.Health/Check works" holds through voya.
	mux.Handle(grpchealth.NewHandler(grpchealth.NewStaticChecker()))
	// grpc.reflection.v1, the second obligation ADR 004 puts on a fleet
	// service (amended 2026-08-16, #158): it is where `voya call` gets the
	// schema for a name it was given, so a fixture without one would be a
	// fleet that command cannot be pointed at. Both versions, as every
	// reflection server serves: v1alpha is what older tooling asks for first.
	//
	// Through client/reflection (#174), which is what a fleet service is
	// told to write — so the documented one-liner and the fixture are the
	// same code, and the fixture notices if the helper breaks. A panic, the
	// way [echoSchema] answers the same class: this constructor has no
	// *testing.T to fail — in this process and in the subprocess callee
	// alike — and both causes are wiring visible on this line.
	if err := voyareflect.MountFrom(mux, echoSchema(), echoService); err != nil {
		panic("acceptance fixture: mounting reflection: " + err.Error())
	}
	mux.Handle(procedure, connect.NewUnaryHandler(procedure,
		func(ctx context.Context, req *connect.Request[wrapperspb.StringValue]) (*connect.Response[wrapperspb.StringValue], error) {
			if after, ok := strings.CutPrefix(req.Msg.GetValue(), sleepPrefix); ok {
				held, err := time.ParseDuration(after)
				if err != nil {
					return nil, connect.NewError(connect.CodeInvalidArgument, err)
				}
				if onHold != nil {
					onHold()
				}
				select {
				case <-time.After(held):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return connect.NewResponse(wrapperspb.String(identity + "|" + req.Header().Get("X-Seen-Authority"))), nil
		}))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-Seen-Authority", r.Host)
		mux.ServeHTTP(w, r)
	})
}

// echoServer is the HTTP server every simulated service in this suite runs.
// One constructor rather than one per call site: the protocol pair below is
// what lets a single dial string serve a Go caller, a grpcurl and a browser
// (ADR 003), and a copy of it that quietly lost h2c would fail as a framing
// error somewhere else entirely.
func echoServer(h http.Handler) *http.Server {
	return &http.Server{Handler: h, Protocols: httpProtocols(), ReadHeaderTimeout: 10 * time.Second}
}

// httpProtocols is HTTP/1.1 plus cleartext HTTP/2, which every leg of this
// suite speaks — voya's upstream leg is h2c only, because gRPC needs HTTP/2
// end to end.
func httpProtocols() *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	return p
}

// service is one connect-go service, exactly as a fleet service is written:
// bind port 0, hand the bound address to voya.Join, serve.
type service struct {
	// t is the test that started this service, so that shutdown can report a
	// drain that did not finish. Held rather than passed, because shutdown is
	// what t.Cleanup runs and a cleanup takes no arguments.
	t *testing.T

	name     string
	identity string
	stop     func()
	srv      *http.Server
	ln       net.Listener
}

// startService starts one simulated service and registers it. extra is
// appended to the options every service here shares, so a test that needs one
// thing different — an eviction hook, a reclaim it can wait on — asks for it
// rather than getting a second copy of this helper. The eviction hook had one
// until this change, and a copy is the thing that stops being updated.
//
// extra goes last, so it wins. Everything below is a default a test may want
// to override except the pid: [portAndPID] reconstructs that from the
// listener, so a voya.WithPID in extra would leave every assertion on an
// announcement matching a pid the service never registered.
func startService(t *testing.T, v *voyad, name, identity string, extra ...voya.Option) *service {
	t.Helper()
	// This instance holding the name, not merely the name being held. A
	// second instance of a live name — which is how this suite stages an
	// eviction — finds the name already taken by the incumbent, so a check
	// for liveness alone returns before the newcomer has registered at all
	// and leaves this helper's "and registers it" untrue on return. Both
	// callers that do it happen to re-sequence on a later waitFor of their
	// own; one that trusted the helper and read the port straight back would
	// get the instance it just replaced.
	return startServiceOn(t, v.proxyPort, v.registrationPort, name, identity, func(port uint32) bool {
		up, live := v.Registry().Lookup(name)
		return live && up.Port == port
	}, extra...)
}

// startServiceOn is [startService] for a daemon known only by its two ports
// and by what it reports — the daemon subprocess of errorband_test.go and
// diag_test.go, whose registry this process cannot read. holds reports
// whether the daemon has this instance, on that port, holding the name; the
// helper returns only once it does.
func startServiceOn(t *testing.T, proxyPort, registrationPort uint32, name, identity string,
	holds func(port uint32) bool, extra ...voya.Option) *service {
	t.Helper()
	return startHandlerOn(t, proxyPort, registrationPort, name, identity, echoHandler(identity, nil), holds, extra...)
}

// startHandlerOn is [startServiceOn] with the handler as a parameter rather
// than [echoHandler] baked in. The REST fixture (rest_test.go, #225) is the
// one service in this suite that serves something else — a Vanguard
// transcoder — and a start helper of its own would be a second copy of the
// registration options below, which is the copy that stops being updated.
func startHandlerOn(t *testing.T, proxyPort, registrationPort uint32, name, identity string,
	h http.Handler, holds func(port uint32) bool, extra ...voya.Option) *service {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("%s: Listen: %v", name, err)
	}
	srv := echoServer(h)

	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	options := []voya.Option{
		voya.WithRegistrationPort(registrationPort),
		// Routing-wise this is inert — the dialer is its only reader and the
		// next line turns the dialer off (#85) — but it is not spare: the
		// registration path compares it against the acknowledged port and
		// warns when they differ (#116), and this daemon's proxy port is
		// ephemeral rather than the 9777 convention. Without it every
		// service in this suite logs a mismatch that is not one; with it the
		// harness says what is true, which is that callerVia dials these
		// services on exactly this port.
		voya.WithProxyPort(proxyPort),
		voya.WithoutDialer(),
		voya.WithBackoff(10*time.Millisecond, 200*time.Millisecond),
		// Labelled with the identity rather than the name, because the two
		// tests that matter most here run two instances of one name: under
		// name, an eviction and the reclaim that undoes it are three lines
		// all reading "other: " and the transcript cannot say which instance
		// spoke. identity is unique per service by construction.
		voya.WithLogf(func(format string, args ...any) { t.Logf(identity+": "+format, args...) }),
		// Every simulated service needs its own pid. They share this test's
		// process, and the registry treats one pid as one process — rightly:
		// a client that reconnects and re-registers is not evicting itself.
		// Without this, two "instances" here are indistinguishable from one
		// service reconnecting, and no eviction happens at all.
		voya.WithPID(fakePID(ln)),
	}
	stop, err := voya.Join(context.Background(), name, ln.Addr(), append(options, extra...)...)
	if err != nil {
		t.Fatalf("%s: Join: %v", name, err)
	}

	s := &service{t: t, name: name, identity: identity, stop: stop, srv: srv, ln: ln}
	t.Cleanup(func() {
		s.shutdown()
		if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("%s: Serve: %v", name, err)
		}
	})
	waitFor(t, identity+" to register", func() bool { return holds(s.port()) })
	return s
}

// shutdown is the orderly stop a service does on SIGTERM, in the order ADR 004
// asks for: leave the registry and wait for voya to act on it, and only then
// drain.
//
// The order is the whole point (#7). stop returns once voya has had the
// shutdown grace to stop routing here, so by the time the listener closes there
// is nothing left being sent to it; the drain that follows is for requests
// already accepted. Doing it the other way round — or closing outright, which
// is what this used to do — cuts off whatever voya routed in between, which is
// the connection reset a developer reads as flakiness in their own service.
//
// The drain's own cost used to be a second for any service the proxy had
// called even once: voya's upstream transport set no idle-conn timeout, so it
// held a pooled h2c connection and Shutdown waited out the HTTP/2 server's
// one-second GOAWAY timeout for a peer that would not close it. Fixed in #52
// by proxy.IdleUpstreamTimeout, measured here at 1.050-1.081s before against
// 265-270ms after.
//
// It is bounded rather than gone, and this helper is where you see why: stop
// returns one shutdown grace — 50ms — after the last call, which is inside the
// 250ms window, so a service restarted under load waits out the remainder. One
// left quiet longer than the window drains in microseconds. See the note on
// [TestARestartUnderLoadCutsNothingOff] for what that is worth in a run.
func (s *service) shutdown() {
	s.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.srv.Shutdown(ctx); err != nil {
		// Reported rather than swallowed: this helper's doc comment claims the
		// drain happened, and a Shutdown that hit its deadline is that claim
		// being wrong. Close so the port comes back either way — the next
		// generation binds a fresh one, but a half-shut server would leak into
		// every test after this.
		s.t.Errorf("%s: draining did not finish in 10s (%v); closing outright", s.name, err)
		_ = s.srv.Close()
	}
}

// fakePID gives a simulated service an identity distinct from its siblings.
// The bound port is unique per service and never zero, which is all the
// registry needs to tell two processes apart.
func fakePID(ln net.Listener) int32 {
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0
	}
	return int32(addr.Port)
}

func (s *service) port() uint32 {
	addr, ok := s.ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0
	}
	return uint32(addr.Port)
}

// --- callers -----------------------------------------------------------------

// caller is a service making a call, using voya's dialer exactly as
// application code would: the URL names the callee, and only the TCP
// destination changes.
//
// "other" is the callee nearly every test here restarts, so it is the default
// rather than an argument repeated at each call site. [callerTo] is the same
// client aimed somewhere else.
func caller(t *testing.T, v *voyad, protocol ...connect.ClientOption) *connect.Client[wrapperspb.StringValue, wrapperspb.StringValue] {
	t.Helper()
	return callerTo(t, v, "other", protocol...)
}

// callerTo is [caller] aimed at a named service.
func callerTo(t *testing.T, v *voyad, name string, protocol ...connect.ClientOption) *connect.Client[wrapperspb.StringValue, wrapperspb.StringValue] {
	t.Helper()
	return callerVia(t, v.proxyPort, name, protocol...)
}

// callerVia is the same caller aimed at a daemon known only by its proxy port,
// which is all the subprocess daemon in errorband_test.go has to be known by.
//
// One definition of how this fleet dials. A load generator that built its own
// transport, dialer and URL inline would be a second copy of the same four
// lines, and the copy is the one that stops being updated.
func callerVia(t *testing.T, proxyPort uint32, name string, protocol ...connect.ClientOption) *connect.Client[wrapperspb.StringValue, wrapperspb.StringValue] {
	t.Helper()
	transport := &http.Transport{
		Protocols:   httpProtocols(),
		DialContext: voya.Dialer(voya.WithProxyPort(proxyPort)),
	}
	t.Cleanup(transport.CloseIdleConnections)

	url := "http://" + name + ".internal:" + strconv.FormatUint(uint64(proxyPort), 10) + procedure
	return connect.NewClient[wrapperspb.StringValue, wrapperspb.StringValue](
		&http.Client{Transport: transport, Timeout: callerTimeout}, url, protocol...)
}

func call(t *testing.T, c *connect.Client[wrapperspb.StringValue, wrapperspb.StringValue]) (string, error) {
	t.Helper()
	resp, err := c.CallUnary(context.Background(), connect.NewRequest(wrapperspb.String("ping")))
	if err != nil {
		return "", err
	}
	return resp.Msg.GetValue(), nil
}

// assertToldPromptly makes a call that must fail, and holds voya to ADR 005:
// an answer rather than a hang, and an answer that names the service.
//
// The call goes in a goroutine so that a hang is this test's failure and not
// the whole binary's timeout — the caller's own [callerTimeout] would
// eventually end it, but by then it is indistinguishable from the diagnostic.
func assertToldPromptly(t *testing.T, c *connect.Client[wrapperspb.StringValue, wrapperspb.StringValue], service string) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := call(t, c)
		done <- err
	}()
	select {
	case err := <-done:
		assertVoyaDiagnostic(t, err, service)
	case <-time.After(diagnosticDeadline):
		t.Fatalf("the call was still waiting after %s; ADR 005 requires a diagnostic, not a hang", diagnosticDeadline)
	}
}

// assertVoyaDiagnostic holds err to being voya's answer about service, rather
// than any failure that mentions it.
//
// The service name alone does not distinguish the two. Every net/http failure
// is a *url.Error quoting the dial string, and this suite's dial string is
// http://other.internal:<port><procedure> — so a caller that hung until its
// own timeout produces "other" as surely as the diagnostic does, and a check
// for the name would pass for the hang it was written to catch. The "voya:"
// prefix is what separates them: internal/proxy puts it on every diagnostic it
// writes, and nothing on the transport path has any reason to say it.
func assertVoyaDiagnostic(t *testing.T, err error, service string) {
	t.Helper()
	if err == nil {
		t.Fatal("the call succeeded, though nothing was serving that name by then")
	}
	if !strings.Contains(err.Error(), "voya:") {
		t.Errorf("the failure is not voya's diagnostic, so nothing here rules out a hang: %v", err)
	}
	if !strings.Contains(err.Error(), service) {
		t.Errorf("the failure does not name the service: %v", err)
	}
}

// pollInterval is how often [waitFor] looks. Named because anything timed by
// watching for a condition here is quantised to it: an event that took no time
// at all still reads as one interval, so a duration measured through waitFor
// is an upper bound and has to be published as one.
const pollInterval = 5 * time.Millisecond

// waitDeadline is how long [waitFor] gives a condition before calling it a
// failure. Named rather than written into waitFor, because a test that also
// puts a bound on the *service* it is waiting for — a reclaim window, say —
// has to outlast this one, and a magic number in two places is how the two
// stop agreeing (see [TestEvictionAndReclaimReachVoyasTerminal]).
const waitDeadline = 20 * time.Second

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitDeadline)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(pollInterval)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// --- the acceptance test -----------------------------------------------------

// TestTheSkeletonWalks is the sentence from #18, executed.
//
// Two services are running. One dials other.internal and the request
// arrives. The callee restarts onto a different ephemeral port, and traffic
// follows — with no configuration change anywhere, no sudo, and no DNS.
func TestTheSkeletonWalks(t *testing.T) {
	v := startVoya(t)
	startService(t, v, "one", "one/first")
	callee := startService(t, v, "other", "other/first")

	client := caller(t, v)

	got, err := call(t, client)
	if err != nil {
		t.Fatalf("the first call failed: %v", err)
	}
	if !strings.HasPrefix(got, "other/first") {
		t.Fatalf("call reached %q, want the first instance of other", got)
	}
	// And it arrived carrying the name it was dialled by, not the ephemeral
	// address it was routed to. That is what makes the dial string portable.
	if !strings.Contains(got, "other.internal") {
		t.Errorf("the callee saw authority %q, want other.internal", got)
	}

	firstPort := callee.port()
	callee.shutdown()
	waitFor(t, "the callee to be evicted", func() bool {
		_, live := v.Registry().Lookup("other")
		return !live
	})

	// It comes back as a new process would: a fresh listener, so a fresh
	// ephemeral port, and nothing anywhere is reconfigured.
	restarted := startService(t, v, "other", "other/second")
	if restarted.port() == firstPort {
		t.Fatalf("the restarted callee reused port %d; the test would prove nothing", firstPort)
	}

	waitFor(t, "traffic to follow the restart", func() bool {
		got, err := call(t, client)
		return err == nil && strings.HasPrefix(got, "other/second")
	})

	got, err = call(t, client)
	if err != nil {
		t.Fatalf("the call after the restart failed: %v", err)
	}
	if !strings.HasPrefix(got, "other/second") {
		t.Errorf("after the restart the call reached %q, want the second instance", got)
	}
	t.Logf("callee moved from port %d to %d; the caller changed nothing", firstPort, restarted.port())
}

// TestEveryProtocolThroughTheSamePort: Connect, gRPC and gRPC-Web all reach
// the service on 9777, which is what lets one dial string serve a Go caller,
// a grpcurl and a browser.
func TestEveryProtocolThroughTheSamePort(t *testing.T) {
	v := startVoya(t)
	startService(t, v, "other", "other/only")

	for _, tc := range []struct {
		name   string
		option []connect.ClientOption
	}{
		{"connect", nil},
		{"grpc", []connect.ClientOption{connect.WithGRPC()}},
		{"grpc-web", []connect.ClientOption{connect.WithGRPCWeb()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := call(t, caller(t, v, tc.option...))
			if err != nil {
				t.Fatalf("%s call failed: %v", tc.name, err)
			}
			if !strings.HasPrefix(got, "other/only") {
				t.Errorf("%s reached %q", tc.name, got)
			}
		})
	}
}

// TestAVanishedCalleeIsEvictedAndThenExplained: the callee goes away and the
// next request gets a diagnostic rather than a hang (ADR 004, ADR 005).
//
// This is the orderly half: these services share this test's process, so what
// voya sees is a deregistration, not a socket dying with the process that
// owned it. Closing a simulated service's listener does not kill the
// registration client holding its stream, because that client is this process.
//
// The real SIGKILL is [TestSIGKILLedCalleeIsEvictedAndExplained], which runs
// the callee as a second process so there is something to kill (#40); the
// transport-level version is TestAbruptDisconnectEvicts in
// internal/registration, which kills the TCP connection with no HTTP-level
// goodbye. Both halves are worth having: this one is the shape of an ordinary
// restart, and that one is the shape of a crash.
func TestAVanishedCalleeIsEvictedAndThenExplained(t *testing.T) {
	v := startVoya(t)
	callee := startService(t, v, "other", "other/doomed")
	client := caller(t, v)

	if _, err := call(t, client); err != nil {
		t.Fatalf("the call before the kill failed: %v", err)
	}

	callee.shutdown()
	waitFor(t, "the vanished service to be evicted", func() bool {
		_, live := v.Registry().Lookup("other")
		return !live
	})

	// The request must fail with something that explains itself, promptly.
	assertToldPromptly(t, client, "other")
}

// TestSecondInstanceEvictsTheFirstLoudly is ADR 002's policy end to end: the
// name goes to the newcomer, the incumbent is told, and the warning names
// both so a developer can find the process that stopped receiving traffic.
func TestSecondInstanceEvictsTheFirstLoudly(t *testing.T) {
	v := startVoya(t)

	var (
		mu       sync.Mutex
		warnings []string
	)
	first := startService(t, v, "other", "other/incumbent", voya.WithOnEvicted(func(e voya.Eviction) {
		mu.Lock()
		defer mu.Unlock()
		warnings = append(warnings, e.Reason)
	}))

	second := startService(t, v, "other", "other/usurper")

	waitFor(t, "the incumbent to be told it was evicted", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(warnings) > 0
	})

	mu.Lock()
	warning := warnings[0]
	mu.Unlock()
	for _, want := range []string{"other.internal", strconv.FormatUint(uint64(first.port()), 10),
		strconv.FormatUint(uint64(second.port()), 10)} {
		if !strings.Contains(warning, want) {
			t.Errorf("the eviction warning omits %q:\n%s", want, warning)
		}
	}

	// And traffic goes to the newcomer, not round-robin between them.
	client := caller(t, v)
	for range 10 {
		got, err := call(t, client)
		if err != nil {
			t.Fatalf("call after eviction: %v", err)
		}
		if !strings.HasPrefix(got, "other/usurper") {
			t.Fatalf("a call reached %q — traffic is being shared between two builds", got)
		}
	}
}

// TestEvictionAndReclaimReachVoyasTerminal is the composition #47 found
// missing. Every link in a name changing hands was pinned and the whole path
// was not.
//
// The registry's announcements are pinned in internal/registry and the
// daemon's rendering of them in internal/daemon — but that test calls
// RegisterIfFree directly, and in production that call is reached only
// through the registration handler. Here a real service registers over the
// Connect stream, a second instance deposes it, and the first reclaims
// through internal/registration, with voya's own terminal as the assertion.
//
// That is the deliverable rather than a convenient observation point: ADR 002
// obliges both halves to be announced, and #27's whole argument is that a name
// changing hands silently is the failure mode. A handler that stopped calling
// RegisterIfFree, or a logf that stopped being threaded through, would leave
// every other test on this path green.
func TestEvictionAndReclaimReachVoyasTerminal(t *testing.T) {
	v := startVoya(t)

	// The incumbent asks for its name back on an interval this suite can wait
	// on. The default is every 15s for five minutes, which is a bound on a
	// developer's patience and would make this test a minute long.
	//
	// The window is three [waitDeadline]s because it has to outlast every
	// wait this test itself grants after the eviction that starts the clock:
	// one waitDeadline for the eviction announcement, up to ten seconds for
	// [service.shutdown]'s drain, and another waitDeadline for the reclaim.
	// A shorter window lets a slow machine end the client's asking first, and
	// the test then fails as "voya never announced the reclaim" — blaming the
	// daemon for a client that had already given up.
	incumbent := startService(t, v, "other", "other/incumbent",
		voya.WithReclaim(3*waitDeadline, 10*time.Millisecond))
	usurper := startService(t, v, "other", "other/usurper")

	waitFor(t, "voya to announce the eviction", func() bool {
		return v.announced("was taken over by another instance",
			portAndPID(incumbent), portAndPID(usurper))
	})

	// The replacement goes away, and the incumbent — which has been asking
	// for its name back all along, without ever taking it — gets it.
	usurper.shutdown()
	waitFor(t, "voya to announce the reclaim", func() bool {
		return v.announced("went back to the instance that was deposed earlier",
			portAndPID(incumbent))
	})

	// And the name really did change hands, rather than only being talked
	// about: the announcement is the deliverable, but an announcement about
	// nothing would be worse than silence.
	//
	// One client, built outside the poll: [caller] registers a cleanup that
	// closes its transport's idle connections, so building one per attempt
	// would leave a few hundred of them for the rest of the test.
	client := caller(t, v)
	waitFor(t, "traffic to follow the reclaim", func() bool {
		got, err := call(t, client)
		return err == nil && strings.HasPrefix(got, "other/incumbent")
	})
}

// portAndPID is how both announcements name an instance — "port 51001, pid
// 51001" for a simulated service, whose pid is its port ([fakePID]).
//
// Matched as one string rather than as a port on its own, because ephemeral
// port numbers are each other's prefixes: "port 5100" is inside "port 51001",
// and the two instances here are drawn from the same range moments apart.
func portAndPID(s *service) string {
	return fmt.Sprintf("port %d, pid %d", s.port(), fakePID(s.ln))
}

// TestVoyaRestartIsANonEvent: voya itself goes away and comes back on the
// same ports. Every stream breaks, every service re-registers on its own, and
// the caller — which knows only a name — never learns it happened (ADR 004).
func TestVoyaRestartIsANonEvent(t *testing.T) {
	// The same config across the restart, since that is the thing under test:
	// whichever ports the first daemon managed to bind are the ones the
	// second one has to come back on.
	v, cfg := startOnFreePorts(t)
	startService(t, v, "other", "other/only")

	client := caller(t, v)
	if _, err := call(t, client); err != nil {
		t.Fatalf("the call before the restart failed: %v", err)
	}

	// voya dies. Its registry dies with it: nothing is persisted, which is
	// the whole of ADR 004's answer to a stale registry.
	if err := v.Close(); err != nil {
		t.Fatalf("stopping voya: %v", err)
	}

	// The replacement gets its own capture and v takes both it and the daemon
	// over, so that v goes on describing the voya that is running. Left
	// pointing at the one that just died, v would answer [voyad.announced]
	// out of a capture nothing writes to any more — and an assertion added
	// here later would time out saying voya never announced something voya
	// announced fine.
	//
	// One capture across the retries rather than one each, unlike
	// [startOnFreePorts]: a Start that fails here leaves nothing behind to
	// confuse the one that succeeds, because the startup banner is written
	// only once all three listeners are bound.
	log := newVoyaLog(t)
	var second *daemon.Daemon
	waitFor(t, "the port to come free for a new voya", func() bool {
		d, err := daemon.Start(context.Background(), cfg, log.Logf)
		if err != nil {
			return false
		}
		second = d
		return true
	})
	v.Daemon, v.log = second, log
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Errorf("stopping the second voya: %v", err)
		}
	})

	waitFor(t, "the service to re-register with the new voya", func() bool {
		_, live := v.Registry().Lookup("other")
		return live
	})
	waitFor(t, "traffic to resume", func() bool {
		got, err := call(t, client)
		return err == nil && strings.HasPrefix(got, "other/only")
	})
}

// TestPortAlreadyInUseSaysWhich is ADR 001's startup failure: name the port,
// and where possible what holds it.
//
// What this test holds is voya's own proxy port, rather than a port drawn
// separately and written into the config. Writing one in can collide:
// [daemon.FreePorts] draws across both transports, so a TCP port held here
// and the UDP port drawn for DNS are two sockets that can carry the same
// number, and the overwrite would then leave proxy_port == dns_port. voya
// refuses that at validation, before it binds anything — a different failure
// from the one under test, which this test would report as a missing
// "already in use".
func TestPortAlreadyInUseSaysWhich(t *testing.T) {
	// Isolated even though this Start is meant to fail: the key is minted
	// before the ports bind (#219), so the failure under test would otherwise
	// leave a key in the developer's real configuration directory.
	t.Setenv(config.DirEnv, t.TempDir())
	cfg := freeConfig(t)
	taken, err := net.Listen("tcp", loopback(cfg.GetProxyPort()))
	if err != nil {
		t.Fatalf("holding voya's proxy port %d: %v", cfg.GetProxyPort(), err)
	}
	defer taken.Close()

	d, err := daemon.Start(context.Background(), cfg, func(string, ...any) {})
	if err == nil {
		_ = d.Close()
		t.Fatal("voya started on a port that was already taken")
	}
	message := err.Error()
	if !strings.Contains(message, strconv.FormatUint(uint64(cfg.GetProxyPort()), 10)) {
		t.Errorf("the failure does not name the port:\n%s", message)
	}
	if !strings.Contains(message, "already in use") {
		t.Errorf("the failure does not say what is wrong:\n%s", message)
	}
	t.Logf("startup failure reads:\n%s", message)
}

// TestNothingIsLeftBoundAfterAFailedStart: a conflict on the second listener
// must not leave the first one held, or the next attempt fails on a port the
// developer just freed.
func TestNothingIsLeftBoundAfterAFailedStart(t *testing.T) {
	t.Setenv(config.DirEnv, t.TempDir()) // as on [TestPortAlreadyInUseSaysWhich]
	cfg := freeConfig(t)
	// voya's own registration port, so that the conflict is the second bind —
	// and held rather than written into the config, for the reason given on
	// [TestPortAlreadyInUseSaysWhich].
	taken, err := net.Listen("tcp", loopback(cfg.GetRegistrationPort()))
	if err != nil {
		t.Fatalf("holding voya's registration port %d: %v", cfg.GetRegistrationPort(), err)
	}
	defer taken.Close()

	proxyPort := cfg.GetProxyPort()
	if d, err := daemon.Start(context.Background(), cfg, func(string, ...any) {}); err == nil {
		_ = d.Close()
		t.Fatal("voya started despite the registration port being taken")
	}

	// The proxy port must be free again.
	probe, err := net.Listen("tcp", loopback(proxyPort))
	if err != nil {
		t.Fatalf("the proxy port is still held after a failed start: %v", err)
	}
	_ = probe.Close()
}

// TestEveryListenerIsOnLoopback: all three, and never 0.0.0.0. Registration
// is unauthenticated and trust-on-first-use, and binding loopback explicitly
// is the whole of what bounds it (ADR 002).
func TestEveryListenerIsOnLoopback(t *testing.T) {
	v := startVoya(t)
	for what, addr := range map[string]string{
		"proxy":        v.ProxyAddr(),
		"registration": v.RegistrationAddr(),
		"dns":          v.DNSAddr(),
	} {
		if !strings.HasPrefix(addr, "127.0.0.1:") {
			t.Errorf("%s bound %q, want a loopback address", what, addr)
		}
	}
}
