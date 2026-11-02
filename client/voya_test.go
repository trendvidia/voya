package voya

// What is tested here is the client's own mechanics: options resolution, the
// dialer install latch, stream-error classification — everything that needs
// this package's unexported names and no registry policy. The daemon-facing
// half of the old suite — eviction, reclaim, the shutdown grace, driven
// against the real registration server — lives in
// internal/registration/join_test.go, because client is its own module (#36)
// and a require of the root module in client/go.mod, which a test import
// creates just as surely as a production one, is exactly the onboarding cost
// the split exists to remove. Where a test here needs an acknowledgement or a
// rejection on the stream, it scripts one; the script pins stream mechanics
// only, and each has a real-daemon counterpart over there keeping the policy
// honest.

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

	registryv1 "github.com/trendvidia/voya/proto/voya/registry/v1"
	"github.com/trendvidia/voya/proto/voya/registry/v1/registryv1connect"
)

// registerScript is one test's registration handler: what to do with the one
// stream a registrar opens. It implements the generated handler interface,
// so a func literal is a whole RegistrationService.
type registerScript func(context.Context, *connect.BidiStream[registryv1.RegisterRequest, registryv1.RegisterResponse]) error

func (f registerScript) Register(
	ctx context.Context,
	stream *connect.BidiStream[registryv1.RegisterRequest, registryv1.RegisterResponse],
) error {
	return f(ctx, stream)
}

// scripted is a registration listener whose handler is the test's script. It
// speaks the same protocol the daemon does — prior-knowledge h2c, the same
// generated handler — but holds no registry and applies no policy, which is
// the point: the tests that stay in this package are about what the client
// does with a stream, not about what voya decides to put on it.
type scripted struct {
	t      *testing.T
	port   uint32
	srv    *http.Server
	cancel context.CancelFunc
	served chan error
	once   sync.Once
}

func startScripted(t *testing.T, script registerScript) *scripted {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	_, handler := registryv1connect.NewRegistrationServiceHandler(script)

	// Connect's bidirectional streaming needs HTTP/2, over cleartext, exactly
	// as internal/registration.NewHTTPServer serves it.
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{
		Handler:           handler,
		Protocols:         protocols,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: 30 * time.Second,
	}

	// Serve's error travels on a channel, not out of the goroutine: t.Errorf
	// after the test completes panics and takes the binary down instead of
	// reporting anything.
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	s := &scripted{t: t, port: portOfListener(t, ln), srv: srv, cancel: cancel, served: served}
	t.Cleanup(s.stop)
	return s
}

// stop is a sync.Once so the cleanup and an explicit call in the test are one
// stop — and so a second caller blocks until the first has joined Serve,
// rather than returning while the server is still going down.
func (s *scripted) stop() {
	s.once.Do(func() {
		s.cancel()
		_ = s.srv.Close()
		if err := <-s.served; err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.t.Errorf("Serve: %v", err)
		}
	})
}

// ackThenHold is the daemon's happy path in miniature: read the registration,
// acknowledge it naming proxyPort, then hold the stream open — silent, the
// way a real registration stream is — until the server is stopped.
func ackThenHold(proxyPort uint32) registerScript {
	return func(ctx context.Context, stream *connect.BidiStream[registryv1.RegisterRequest, registryv1.RegisterResponse]) error {
		if _, err := stream.Receive(); err != nil {
			return err
		}
		if err := stream.Send(&registryv1.RegisterResponse{
			Event: &registryv1.RegisterResponse_Registered{
				Registered: &registryv1.Registered{
					Authority: "payments.internal",
					ProxyPort: proxyPort,
				},
			},
		}); err != nil {
			return err
		}
		<-ctx.Done()
		return nil
	}
}

// scriptedOpts are the options every scripted-stream test needs: point at the
// script's port, and leave http.DefaultTransport alone so tests do not
// interfere with each other through process-global state.
func scriptedOpts(s *scripted, extra ...Option) []Option {
	return append([]Option{
		WithRegistrationPort(s.port),
		WithoutDialer(),
		WithLogf(func(string, ...any) {}),
		withFlyAppName(func() string { return "" }),
	}, extra...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func tcpAddr(t *testing.T, port int) net.Addr {
	t.Helper()
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
}

// TestJoinWithoutVoyaDoesNotBlockOrSpin covers the acceptance criterion that
// a service starts and runs fine with no voya at all.
func TestJoinWithoutVoyaDoesNotBlockOrSpin(t *testing.T) {
	// A port that accepts and immediately hangs up, so every attempt is a
	// countable failure rather than a connection refused at a closed port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	var attempts atomicCounter
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			attempts.inc()
			_ = conn.Close()
		}
	}()

	var logs atomicCounter
	start := time.Now()
	stop, err := Join(context.Background(), "payments", tcpAddr(t, 51234),
		WithRegistrationPort(portOfListener(t, ln)),
		WithoutDialer(),
		WithBackoff(10*time.Millisecond, 100*time.Millisecond),
		WithLogf(func(string, ...any) { logs.inc() }),
		withFlyAppName(func() string { return "" }),
	)
	if err != nil {
		t.Fatalf("Join returned an error with no voya running: %v", err)
	}
	defer stop()

	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Join blocked for %v with no voya running", elapsed)
	}

	time.Sleep(600 * time.Millisecond)

	// Bounded backoff: without it, 600ms of tight-looping dials would be
	// thousands of attempts. With 10ms doubling to 100ms it is well under 60.
	if n := attempts.get(); n > 60 {
		t.Errorf("%d connection attempts in 600ms — the backoff is not bounding retries", n)
	} else if n == 0 {
		t.Error("no connection attempt was made; the test proved nothing")
	}
	// And it must not narrate every one of them.
	if n := logs.get(); n > 1 {
		t.Errorf("logged %d times while voya was absent; a supported state should be reported once", n)
	}
}

// isolateInstall gives one test its own view of the process-global state
// installDialer writes — http.DefaultTransport and the install latch — and
// puts both back when it ends. Without it a test that installs decides what
// every test after it sees, and the order they run in becomes part of the
// result. It returns the http.DefaultTransport in force at the start, which
// is what "untouched" is compared against.
func isolateInstall(t *testing.T) http.RoundTripper {
	t.Helper()
	saved := http.DefaultTransport
	installed.Lock()
	savedDone, savedPort := installed.done, installed.proxyPort
	installed.done, installed.proxyPort = false, 0
	installed.Unlock()
	t.Cleanup(func() {
		http.DefaultTransport = saved
		installed.Lock()
		installed.done, installed.proxyPort = savedDone, savedPort
		installed.Unlock()
	})
	return saved
}

func TestJoinInstallsTheDialerByDefault(t *testing.T) {
	proxyPort, accepted := acceptAll(t)
	// A second listener for the registration stream. Pointing registration
	// at the proxy's own listener would have the registrar's reconnects
	// signal `accepted`, and the assertion below — that a request through
	// http.DefaultTransport reached the proxy — would pass on traffic that
	// never went near the dialer.
	registrationPort, _ := acceptAll(t)

	isolateInstall(t)

	stop, err := Join(context.Background(), "payments", tcpAddr(t, 51234),
		WithProxyPort(proxyPort),
		WithRegistrationPort(registrationPort),
		WithLogf(func(string, ...any) {}),
		withFlyAppName(func() string { return "" }),
	)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer stop()

	// The assertion is behavioural: a request through the default transport
	// reaches the proxy. Comparing transport identity would pass even if the
	// dialer were wired up wrong. What Join installs is the 429-honouring
	// wrapper (#224) around the dialing transport, so the dial is made
	// through its base.
	wrapper, ok := http.DefaultTransport.(*throttleTransport)
	if !ok {
		t.Fatalf("http.DefaultTransport is %T, not the throttle wrapper Join installs", http.DefaultTransport)
	}
	transport, ok := wrapper.base.(*http.Transport)
	if !ok {
		t.Fatalf("the throttle wrapper's base is %T", wrapper.base)
	}
	conn, err := transport.DialContext(context.Background(), "tcp",
		"payments.internal:"+strconv.FormatUint(uint64(proxyPort), 10))
	if err != nil {
		t.Fatalf("dialing through http.DefaultTransport: %v", err)
	}
	defer conn.Close()

	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("http.DefaultTransport did not route *.internal to the proxy")
	}
}

// TestWithProxyPortIsInertUnderWithoutDialer pins the claim [WithProxyPort]'s
// doc makes: routing is the dialer's alone, so combined with [WithoutDialer]
// the port moves no traffic (#85) — the one thing left reading it is #116's
// acknowledgement check, which only logs. Asserted from both ends — nothing
// was installed, and nothing ever connected to the port the caller named —
// with the registration acknowledged in between, so the option being inert is
// not the whole Join having been one.
//
// The acknowledgement comes from a scripted stream naming the option's own
// port, because inertness is a property of the client's wiring and needs no
// policy behind the ack; #116's comparison itself is tested against the real
// daemon in internal/registration's TestAMismatchedAcknowledgedProxyPortWarns.
func TestWithProxyPortIsInertUnderWithoutDialer(t *testing.T) {
	// A live listener stands in for the configured port, so "never consulted"
	// is observed rather than inferred: any connection here is the claim
	// failing.
	proxyPort, accepted := acceptAll(t)

	s := startScripted(t, ackThenHold(proxyPort))

	saved := isolateInstall(t)

	var log logLines
	stop, err := Join(context.Background(), "payments", tcpAddr(t, 51234),
		WithProxyPort(proxyPort),
		WithRegistrationPort(s.port),
		WithoutDialer(),
		WithLogf(log.logf),
		withFlyAppName(func() string { return "" }),
	)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	// stop is a sync.OnceFunc, so this and the explicit call below are one
	// call. The defer is for the path where waitFor fatals in between, which
	// would otherwise leave the registrar running for the rest of the binary.
	defer stop()

	// The setup, asserted: this Join did everything except install the dialer,
	// so what follows is WithoutDialer's doing and not a registration that
	// never happened. The "is serving on" line is written only after the
	// acknowledgement is read, so matching it is waiting on the registration.
	waitFor(t, "the acknowledgement to be logged", func() bool {
		return len(log.matching("is serving on")) >= 1
	})
	stop()

	if http.DefaultTransport != saved {
		t.Error("Join replaced http.DefaultTransport despite WithoutDialer")
	}
	installed.Lock()
	done := installed.done
	installed.Unlock()
	if done {
		t.Error("installDialer ran despite WithoutDialer")
	}

	// The doc's second claim, and all the sentinel can carry on its own:
	// registration never dials the configured port, across the whole Join and
	// stop above. Checked before anything in this test dials, so a connection
	// here is the registrar's and nothing else's.
	select {
	case <-accepted:
		t.Error("something connected to the configured proxy port; the registration path has no use for it")
	default:
	}

	// The first claim needs a dial to be falsifiable at all: nothing above
	// sends a request, so a listener nobody talks to stays silent whether or
	// not the port was honoured. Issuing the fleet's own dial string through
	// http.DefaultTransport is what separates the two outcomes — left alone it
	// goes to the resolver, which has no address for payments.internal, while
	// a transport carrying this port would send it to the sentinel instead.
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("http.DefaultTransport is %T", http.DefaultTransport)
	}
	port := strconv.FormatUint(uint64(proxyPort), 10)

	// That inference holds only where the name does not resolve to loopback,
	// and on the machine README describes it does: /etc/resolver/internal
	// sends *.internal to voya's DNS, which answers 127.0.0.1 for every name
	// in it with no per-service records (internal/dns). The fleet's dial
	// string then reaches the sentinel through the DNS path — the one ADR 002
	// keeps separate from the dialer, and immune to it — so the assertion
	// below would blame WithProxyPort for the resolver doing its job. Probed
	// rather than assumed, and reported rather than skipped quietly: on CI,
	// where nothing answers .internal, the assertion runs.
	//
	// The probe gets its own deadline rather than sharing the dial's, and its
	// error is read rather than folded into "does not resolve to loopback". A
	// resolver that blackholes .internal rather than refusing it burns the
	// whole budget here and then errors, which is not the "no such host" that
	// licenses the dial: the dial's own lookup a moment later can still answer
	// loopback, and the assertion below would then blame WithProxyPort for the
	// resolver doing its job. On a shared context that dial would instead have
	// failed on the exhausted deadline — a failure indistinguishable from the
	// honest "no such host" it passes on, so the assertion vanished with
	// nothing said. Every path here now says which one the run took.
	probeCtx, cancelProbe := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelProbe()
	loopback, probeErr := loopbackResolves(t, probeCtx, "payments.internal")
	switch {
	case loopback:
		t.Log("payments.internal resolves to loopback here, so a dial cannot tell the dialer's redirect " +
			"from the DNS path; the install assertions above still hold and this one is skipped")
		return
	case probeErr != nil && !isNoSuchHost(probeErr):
		t.Logf("the resolver neither answered payments.internal nor said it does not exist (%v), so what the "+
			"dial below would resolve is unknown; the install assertions above still hold and this one is skipped", probeErr)
		return
	}

	dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := transport.DialContext(dialCtx, "tcp", "payments.internal:"+port)
	if err != nil {
		// The expected outcome where nothing answers .internal, and it is
		// logged rather than passed over: this test's transcript should say
		// which of the two paths carried the assertion.
		t.Logf("dialing payments.internal:%s through http.DefaultTransport failed, as an uninstalled dialer requires: %v", port, err)
	}
	if err == nil {
		// Where it went, not whether it got there: a machine whose resolver
		// answers .internal with something that is not loopback would make
		// "the dial failed" a property of the network rather than of this
		// package, and loopback on this port is the one destination
		// WithProxyPort could have produced.
		remote := conn.RemoteAddr().String()
		_ = conn.Close()
		if remote == net.JoinHostPort("127.0.0.1", port) {
			t.Errorf("http.DefaultTransport sent payments.internal to %s; WithProxyPort is not inert under WithoutDialer", remote)
		}
	}
}

// loopbackResolves reports whether this machine's resolver answers host with a
// loopback address — the one condition under which "the dial landed on
// 127.0.0.1" stops being evidence about voya's dialer.
//
// A resolver that answers with anything else leaves the assertion sound: the
// dial goes somewhere that is not the sentinel, which is the same outcome as
// not resolving at all.
//
// The lookup's error comes back alongside the answer rather than being read as
// "not loopback": only [isNoSuchHost] is the resolver saying so. Anything else
// leaves the caller not knowing what the dial's own lookup will do.
func loopbackResolves(t *testing.T, ctx context.Context, host string) (bool, error) {
	t.Helper()
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return false, err
	}
	for _, addr := range addrs {
		if ip := net.ParseIP(addr); ip != nil && ip.IsLoopback() {
			return true, nil
		}
	}
	return false, nil
}

// isNoSuchHost reports whether err is the resolver's own answer that the name
// does not exist, as opposed to a lookup that did not get one. Unwrapped
// rather than type-asserted: a lookup reached through a dial arrives wrapped
// in *net.OpError.
func isNoSuchHost(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

// TestIsNoSuchHostOnlyAcceptsTheResolversOwnAnswer pins the distinction
// [TestWithProxyPortIsInertUnderWithoutDialer]'s skip rests on: a lookup that
// failed without an answer must not read as one, or a blackholed .internal
// licenses a dial whose loopback answer this package gets blamed for.
func TestIsNoSuchHostOnlyAcceptsTheResolversOwnAnswer(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"no such host", &net.DNSError{Err: "no such host", Name: "payments.internal", IsNotFound: true}, true},
		{"wrapped by the dial", &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", IsNotFound: true}}, true},
		{"timeout", &net.DNSError{Err: "i/o timeout", IsTimeout: true}, false},
		{"deadline exceeded", context.DeadlineExceeded, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNoSuchHost(tc.err); got != tc.want {
				t.Errorf("isNoSuchHost(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}

// TestASecondInstallSaysItDidNotGetItsProxyPort: http.DefaultTransport is
// patched once, so in a process hosting two services the first Join's proxy
// port is the one that wins. Leaving that silent would have the second
// service's outbound *.internal traffic going somewhere it did not ask for,
// with nothing anywhere to read.
func TestASecondInstallSaysItDidNotGetItsProxyPort(t *testing.T) {
	isolateInstall(t)

	var said asked
	collect := WithLogf(func(format string, args ...any) { said.add(fmt.Sprintf(format, args...)) })
	dev := withFlyAppName(func() string { return "" })
	install := func(port uint32) {
		installDialer(resolve([]Option{WithProxyPort(port), collect, dev}))
	}

	install(9777)
	if n := said.count(); n != 0 {
		t.Errorf("the first install logged %d times; it got what it asked for", n)
	}

	install(19777)
	if n := said.count(); n != 1 {
		t.Fatalf("an install that did not get its proxy port logged %d times, want 1", n)
	}
	if line := said.last(); !strings.Contains(line, "9777") || !strings.Contains(line, "19777") {
		t.Errorf("the warning names %q; it has to name both ports to be actionable", line)
	}

	// Two services agreeing on the port is the ordinary case, and it has
	// nothing to report.
	install(9777)
	if n := said.count(); n != 1 {
		t.Errorf("a second install on the same port logged %d times, want 1", n)
	}
}

func TestJoinRejectsAnUnboundPort(t *testing.T) {
	cases := []struct {
		name string
		addr net.Addr
	}{
		{"nil", nil},
		{"port zero", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}},
		// Not a *net.TCPAddr, so it takes portOf's string-parsing fallback.
		// The same mistake has to fail the same way there: letting it
		// through moves the rejection to the registry, which refuses port 0
		// on every reconnect for the life of the process while Join's own
		// documentation says it raises this one itself.
		{"non-TCP address on port zero", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Join(context.Background(), "payments", tc.addr,
				WithoutDialer(), withFlyAppName(func() string { return "" }))
			if err == nil {
				t.Fatal("Join accepted an address the proxy could never reach")
			}
		})
	}
}

// TestPortOfAcceptsANonTCPAddressWithARealPort: the fallback exists for
// listeners that are not *net.TCPAddr but still name a port the proxy can
// dial, so closing off port zero must not have closed off the case itself.
func TestPortOfAcceptsANonTCPAddressWithARealPort(t *testing.T) {
	got, err := portOf(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 51234})
	if err != nil {
		t.Fatalf("portOf: %v", err)
	}
	if got != 51234 {
		t.Errorf("portOf = %d, want 51234", got)
	}
}

// TestZeroBackoffDoesNotSpin: the delay grows by doubling, so a minimum of
// zero doubles to zero forever and jitter(0) is 0 — WithBackoff(0, 0) would
// otherwise ask for a dial loop with no delay in it at all, burning a core
// and flooding the registration port. resolve clamps it instead.
func TestZeroBackoffDoesNotSpin(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	var attempts atomicCounter
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			attempts.inc()
			_ = conn.Close()
		}
	}()

	stop, err := Join(context.Background(), "payments", tcpAddr(t, 51234),
		WithRegistrationPort(portOfListener(t, ln)),
		WithoutDialer(),
		WithBackoff(0, 0),
		WithLogf(func(string, ...any) {}),
		withFlyAppName(func() string { return "" }),
	)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer stop()

	time.Sleep(500 * time.Millisecond)
	// Clamped to DefaultMinBackoff, 500ms holds a handful of attempts.
	// Unclamped it is thousands.
	if n := attempts.get(); n > 10 {
		t.Errorf("%d connection attempts in 500ms with WithBackoff(0, 0); the clamp is not holding", n)
	}
}

// TestOnlyAWireErrorCountsAsARefusal pins the connect-go behaviour that
// makes refused() more than a code comparison, because the trap is invisible
// in the code that avoids it.
//
// A stream cut mid-frame reaches the client as "invalid_argument: protocol
// error: incomplete envelope: unexpected EOF" — the same code voya sends to
// reject a service name. Classifying on the code alone would make every voya
// restart look like a permanent refusal to every service at once, which is
// ADR 004's headline claim failing in exactly the way nothing else here
// would catch: the tests would still pass, and the fleet would stay
// deregistered until each service was restarted by hand.
//
// Both halves run against a scripted stream, because what is under test is
// the classifier's reading of connect-go, not voya's policy: the rejection
// script sends the same connect.NewError(CodeInvalidArgument, ...) the real
// server does, and the pairing with voya's own refusal is pinned end to end
// by internal/registration's TestARefusedRegistrationStopsRetrying, which
// fails if either side of it drifts.
func TestOnlyAWireErrorCountsAsARefusal(t *testing.T) {
	// A rejection the server sent: it arrives over the wire, so it is a
	// refusal.
	t.Run("a rejection the server sent", func(t *testing.T) {
		s := startScripted(t, func(ctx context.Context, stream *connect.BidiStream[registryv1.RegisterRequest, registryv1.RegisterResponse]) error {
			if _, err := stream.Receive(); err != nil {
				return err
			}
			return connect.NewError(connect.CodeInvalidArgument, errors.New("the name is not usable"))
		})
		err := lastStreamError(t, s, "Payments", false)
		if !refused(err) {
			t.Errorf("refused(%v) = false; a rejection voya sent would be retried forever", err)
		}
	})

	// The same code, from a stream that broke.
	t.Run("the server dying mid-stream", func(t *testing.T) {
		s := startScripted(t, ackThenHold(DefaultProxyPort))
		err := lastStreamError(t, s, "payments", true)
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Skipf("a broken stream no longer reports invalid_argument (%v); refused() may be able to drop IsWireError", err)
		}
		if refused(err) {
			t.Errorf("refused(%v) = true; every voya restart would strand the service", err)
		}
	})
}

// lastStreamError opens one registration stream and returns the error that
// ended it, either because the script rejected the name or because the
// server stopped.
func lastStreamError(t *testing.T, s *scripted, service string, killServer bool) error {
	t.Helper()
	r := newRegistrar(service, 51234, resolve(scriptedOpts(s)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := r.client.Register(ctx)
	if err := stream.Send(&registryv1.RegisterRequest{Service: service, Port: 51234}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if killServer {
		if _, err := stream.Receive(); err != nil {
			t.Fatalf("Receive acknowledgement: %v", err)
		}
		go func() {
			time.Sleep(50 * time.Millisecond)
			s.stop()
		}()
	}
	_, err := stream.Receive()
	if err == nil {
		t.Fatal("the stream did not end")
	}
	return err
}

// logLines keeps what the client printed, so a test can assert on the words
// a developer reads rather than on a count — and render the transcript when
// the assertion fails.
//
// The lock is the same argument as [atomicCounter]'s: every line here is
// written by the registrar's own goroutine, which is running until stop
// joins it.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logLines) matching(substr string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range l.lines {
		if strings.Contains(line, substr) {
			out = append(out, line)
		}
	}
	return out
}

// atomicCounter keeps the race detector happy about counts written by a
// background goroutine and read by the test.
type atomicCounter struct {
	mu sync.Mutex
	n  int
}

func (c *atomicCounter) inc() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
}

func (c *atomicCounter) get() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// TestCancelDoesNotUnblockReceive pins the connect-go behaviour that
// registrar.session is built around, because the code reads as if it were
// working around nothing.
//
// Cancelling the context passed to Register does not end a Receive already
// waiting on the response body; only closing the response side does. If a
// future connect-go or Go release changes that, this test fails and the
// watcher goroutine in session can go.
func TestCancelDoesNotUnblockReceive(t *testing.T) {
	s := startScripted(t, ackThenHold(DefaultProxyPort))
	o := resolve(scriptedOpts(s))
	r := newRegistrar("payments", 51234, o)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := r.client.Register(ctx)
	if err := stream.Send(&registryv1.RegisterRequest{Service: "payments", Port: 51234}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := stream.Receive(); err != nil {
		t.Fatalf("Receive acknowledgement: %v", err)
	}

	blocked := make(chan error, 1)
	go func() {
		_, err := stream.Receive() // the server sends nothing more
		blocked <- err
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-blocked:
		t.Fatalf("cancellation now unblocks Receive (%v) — session's watcher goroutine is no longer needed", err)
	case <-time.After(2 * time.Second):
	}

	go func() { _ = stream.CloseResponse() }()
	select {
	case <-blocked:
	case <-time.After(10 * time.Second):
		t.Fatal("closing the response side did not unblock Receive either; session would hang forever")
	}
}

// timeStop runs stop and reports how long it took, failing rather than hanging
// the suite if it never returns.
func timeStop(t *testing.T, stop func()) time.Duration {
	t.Helper()
	start := time.Now()
	returned := make(chan struct{})
	go func() { stop(); close(returned) }()
	select {
	case <-returned:
		return time.Since(start)
	case <-time.After(30 * time.Second):
		t.Fatal("stop did not return; the registration goroutine is stuck")
		return 0
	}
}

// TestStopSkipsTheGraceWithNoVoya: the wait buys nothing when nothing was ever
// routed here, and a service that ran without voya at all should not pay for
// it on the way out.
func TestStopSkipsTheGraceWithNoVoya(t *testing.T) {
	// A port nothing is listening on. Registration never succeeds, so there is
	// no route to wait out.
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	port := portOfListener(t, dead)
	if err := dead.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	const grace = 10 * time.Second
	stop, err := Join(context.Background(), "payments", tcpAddr(t, 51234),
		WithRegistrationPort(port),
		WithoutDialer(),
		WithBackoff(10*time.Millisecond, 50*time.Millisecond),
		WithLogf(func(string, ...any) {}),
		WithShutdownGrace(grace),
		withFlyAppName(func() string { return "" }),
	)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}

	// Generous by two orders of magnitude against the grace, and still far
	// under it: this fails if the wait happened at all, not if it was slow.
	if elapsed := timeStop(t, stop); elapsed > time.Second {
		t.Errorf("stop took %v with no voya to deregister from; the grace was paid for nothing", elapsed)
	}
}

// TestZeroShutdownGraceIsHonoured: unlike the backoff knobs, zero is a
// coherent request here and is not clamped to a default.
//
// The clamp is asserted on the resolved options, not on the clock — this is
// the half that needs resolve's unexported result, which is why it lives
// here. internal/registration's TestZeroShutdownGraceSkipsTheWait is the
// clock half, and carries only what its bound can carry: stop against a live
// daemon does not wait out anything on the order of a second with the grace
// off. Separating a zero from the 50ms default on the clock is not
// affordable, so this half is what proves the zero is not clamped.
func TestZeroShutdownGraceIsHonoured(t *testing.T) {
	if got := resolve([]Option{WithShutdownGrace(0)}).shutdownGrace; got != 0 {
		t.Errorf("WithShutdownGrace(0) resolved to %v, want it left at zero", got)
	}
	// Negative is the same request, and is documented as honoured too.
	if got := resolve([]Option{WithShutdownGrace(-time.Second)}).shutdownGrace; got > 0 {
		t.Errorf("WithShutdownGrace(-1s) resolved to %v, want the grace left off", got)
	}
}

// TestZeroReclaimIntervalIsClamped: the wait between claims is jittered and
// jitter(0) is 0, so WithReclaim(window, 0) asks for a claim loop with no
// delay in it at all — a fresh stream per round trip for the whole window,
// which is the spin [WithBackoff]'s clamp exists to prevent. A zero *window*
// is how reclaiming is turned off; a zero interval is not a second way to ask
// for that.
//
// Asserted on the resolved options rather than by counting connections: the
// registration client pools its HTTP/2 connection, so a spin does not show up
// as accepts on the listener.
func TestZeroReclaimIntervalIsClamped(t *testing.T) {
	for _, interval := range []time.Duration{0, -time.Second} {
		o := resolve([]Option{WithReclaim(time.Minute, interval)})
		if o.reclaimInterval != DefaultReclaimInterval {
			t.Errorf("WithReclaim(1m, %s) left an interval of %s, want the default %s",
				interval, o.reclaimInterval, DefaultReclaimInterval)
		}
		if o.reclaimWindow != time.Minute {
			t.Errorf("WithReclaim(1m, %s) changed the window to %s", interval, o.reclaimWindow)
		}
	}

	// And turning reclaiming off stays off, rather than picking up a default
	// interval that nothing will ever wait for.
	if off := resolve([]Option{WithReclaim(0, 0)}); off.reclaimWindow != 0 {
		t.Errorf("WithReclaim(0, 0) left a window of %s", off.reclaimWindow)
	}
}
