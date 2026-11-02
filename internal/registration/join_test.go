// These are the client package's daemon-integration tests: [voya.Join]
// driven against the real registration server and the real registry, because
// what they pin is the pairing — eviction, reclaim, the shutdown grace, the
// refusal that ends retrying — and a scripted stand-in for the daemon would
// drift from the policy it fakes without either side noticing.
//
// They live in this package rather than in client/ because client is its own
// module (#36), and a require of the root module in client/go.mod — which a
// test import creates just as surely as a production one — is exactly the
// onboarding cost the split exists to remove; client/deps_test.go enforces
// that at the import. The root module already requires client for cmd/voya's
// tests and test/acceptance, so the arrow this file adds points the way it
// already pointed. What stayed behind in client/voya_test.go is everything
// about the client's own mechanics — options resolution, the dialer install
// latch, stream-error classification — which needs that package's unexported
// names and no registry policy.
package registration_test

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

	voya "github.com/trendvidia/voya/client"
	"github.com/trendvidia/voya/internal/registration"
	"github.com/trendvidia/voya/internal/registry"
)

// voyad is a real registration server on a known loopback port, so the client
// under test reaches it the same way it reaches the real daemon.
//
// The port is fixed at construction rather than per-listener because restart
// has to rebind the same one: a service reconnecting to "voya" that moved to
// a different port would not be testing what ADR 004 claims.
type voyad struct {
	t    *testing.T
	port uint32

	// proxyPort is what the registration server names in its acknowledgement
	// — voya's side of #116's comparison. Nothing here listens on it.
	proxyPort uint32

	mu     sync.Mutex
	reg    *registry.Registry
	srv    *http.Server
	cancel context.CancelFunc
	served chan error
}

func startVoya(t *testing.T) *voyad {
	t.Helper()
	return startVoyaServing(t, voya.DefaultProxyPort)
}

// startVoyaServing is startVoya with the acknowledged proxy port chosen by
// the test: a daemon whose proxy_port was moved acknowledges the port it
// really bound, not the fleet convention (#116).
func startVoyaServing(t *testing.T, proxyPort uint32) *voyad {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	port := portOfListener(t, ln)
	v := &voyad{t: t, port: port, proxyPort: proxyPort}
	v.serve(ln)
	t.Cleanup(v.stop)
	return v
}

func (v *voyad) serve(ln net.Listener) {
	v.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	reg := registry.New()
	srv := registration.NewHTTPServer(ctx, registration.NewServer(reg, v.proxyPort))

	// Serve's error travels on a channel, not out of the goroutine: t.Errorf
	// after the test completes panics and takes the binary down instead of
	// reporting anything.
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	v.mu.Lock()
	defer v.mu.Unlock()
	v.reg, v.srv, v.cancel, v.served = reg, srv, cancel, served
}

func (v *voyad) registry() *registry.Registry {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.reg
}

func (v *voyad) stop() {
	v.mu.Lock()
	cancel, srv, served := v.cancel, v.srv, v.served
	v.srv = nil
	v.mu.Unlock()
	if srv == nil {
		return
	}
	cancel()
	_ = srv.Close()
	if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
		v.t.Errorf("Serve: %v", err)
	}
}

// restart is voya being killed and started again: the process is new, so the
// registry is empty. ADR 004's claim is that this needs no coordination —
// the streams break and the services put themselves back.
func (v *voyad) restart() {
	v.t.Helper()
	v.stop()

	addr := net.JoinHostPort("127.0.0.1", strconv.FormatUint(uint64(v.port), 10))
	deadline := time.Now().Add(10 * time.Second)
	var (
		ln  net.Listener
		err error
	)
	for time.Now().Before(deadline) {
		ln, err = net.Listen("tcp", addr)
		if err == nil {
			v.serve(ln)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	v.t.Fatalf("could not rebind %s: %v", addr, err)
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

// joinOpts are the options every registration test needs: point at the test
// daemon, and leave http.DefaultTransport alone so tests do not interfere
// with each other through process-global state. The Setenv pins FLY_APP_NAME
// to unset — the in-package tests override how the client reads it, but from
// out here the environment the client really reads is the only knob, and it
// is the honest one.
func joinOpts(t *testing.T, v *voyad, extra ...voya.Option) []voya.Option {
	t.Helper()
	t.Setenv(voya.FlyAppEnv, "")
	return append([]voya.Option{
		voya.WithRegistrationPort(v.port),
		voya.WithoutDialer(),
		voya.WithLogf(func(string, ...any) {}),
	}, extra...)
}

func tcpAddr(t *testing.T, port int) net.Addr {
	t.Helper()
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
}

func portOfListener(t *testing.T, ln net.Listener) uint32 {
	t.Helper()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is %T, want *net.TCPAddr", ln.Addr())
	}
	return uint32(addr.Port)
}

func TestJoinRegistersWithVoya(t *testing.T) {
	v := startVoya(t)

	stop, err := voya.Join(context.Background(), "payments", tcpAddr(t, 51234), joinOpts(t, v)...)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer stop()

	waitFor(t, "payments to appear in the registry", func() bool {
		_, ok := v.registry().Lookup("payments")
		return ok
	})
	up, _ := v.registry().Lookup("payments")
	if up.Port != 51234 {
		t.Errorf("registered port = %d, want the bound port 51234", up.Port)
	}
	if up.PID == 0 {
		t.Error("no pid was registered; an eviction warning could not name this process")
	}
}

func TestStopDeregisters(t *testing.T) {
	v := startVoya(t)

	stop, err := voya.Join(context.Background(), "payments", tcpAddr(t, 51234), joinOpts(t, v)...)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	waitFor(t, "payments to register", func() bool {
		_, ok := v.registry().Lookup("payments")
		return ok
	})

	stop()
	waitFor(t, "stop to deregister payments", func() bool {
		_, ok := v.registry().Lookup("payments")
		return !ok
	})

	stop() // idempotent: a service with both a defer and an explicit stop
}

// TestReconnectsAfterVoyaRestart is ADR 004's headline claim: voya restarting
// is a non-event. Nothing persists, nothing coordinates, and the service
// comes back under the same ephemeral port it never stopped listening on.
func TestReconnectsAfterVoyaRestart(t *testing.T) {
	v := startVoya(t)

	stop, err := voya.Join(context.Background(), "payments", tcpAddr(t, 51234),
		joinOpts(t, v, voya.WithBackoff(5*time.Millisecond, 50*time.Millisecond))...)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer stop()

	waitFor(t, "the first registration", func() bool {
		_, ok := v.registry().Lookup("payments")
		return ok
	})

	v.restart()

	waitFor(t, "payments to re-register with the new voya", func() bool {
		_, ok := v.registry().Lookup("payments")
		return ok
	})
	up, _ := v.registry().Lookup("payments")
	if up.Port != 51234 {
		t.Errorf("re-registered port = %d, want the unchanged 51234", up.Port)
	}
}

// TestEvictionStopsRegistration is #27's failure mode: a client that treats
// eviction as an ordinary disconnect re-registers, deposes the instance that
// deposed it, and the two swap the name forever. The typed event exists so
// the client can tell the difference; this asserts it acts on it.
func TestEvictionStopsRegistration(t *testing.T) {
	v := startVoya(t)

	evictions := make(chan voya.Eviction, 4)
	stop, err := voya.Join(context.Background(), "payments", tcpAddr(t, 1001),
		joinOpts(t, v,
			voya.WithPID(111),
			voya.WithBackoff(5*time.Millisecond, 20*time.Millisecond),
			voya.WithOnEvicted(func(e voya.Eviction) { evictions <- e }),
		)...)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer stop()

	waitFor(t, "payments to register", func() bool {
		up, ok := v.registry().Lookup("payments")
		return ok && up.Port == 1001
	})

	// A second instance takes the name, straight through the registry — the
	// same thing a second `go run` would do through the stream.
	usurper, err := v.registry().Register(registry.Instance{Service: "payments", Port: 1002, PID: 222})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer usurper.Close()

	select {
	case e := <-evictions:
		if e.ReplacedByPort != 1002 {
			t.Errorf("ReplacedByPort = %d, want 1002", e.ReplacedByPort)
		}
		if e.ReplacedByPID != 222 {
			t.Errorf("ReplacedByPID = %d, want 222", e.ReplacedByPID)
		}
		if e.Reason == "" {
			t.Error("the eviction carried no reason for the developer to read")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the deposed client was never told it was evicted")
	}

	// The client must now leave the name alone. With backoff at 5-20ms, a
	// client that reconnected would have taken it back many times over.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		up, ok := v.registry().Lookup("payments")
		if !ok {
			t.Fatal("the name went missing after the eviction")
		}
		if up.Port != 1002 {
			t.Fatalf("the evicted instance took the name back (port %d) — two builds are now fighting for it", up.Port)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestJoinIsANoOpOnFly(t *testing.T) {
	v := startVoya(t)

	saved := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = saved })
	t.Setenv(voya.FlyAppEnv, "payments")

	stop, err := voya.Join(context.Background(), "payments", tcpAddr(t, 51234),
		voya.WithRegistrationPort(v.port),
		voya.WithLogf(func(string, ...any) {}),
	)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer stop()

	// Nothing registers, because on Fly there is nothing to register with.
	time.Sleep(250 * time.Millisecond)
	if got := len(v.registry().Services()); got != 0 {
		t.Errorf("registered %d services while FLY_APP_NAME was set", got)
	}
	// And outbound traffic is untouched: 6PN resolves *.internal itself, and
	// rewriting destinations in production would be a bug with no upside.
	if http.DefaultTransport != saved {
		t.Error("the dialer was installed while FLY_APP_NAME was set")
	}
}

// TestAMismatchedAcknowledgedProxyPortWarns is #116: a dialer captures the
// port its options named, wired before the registration stream opens, and
// voya then acknowledges with the proxy port it actually bound. On a voya
// whose proxy_port was moved, the service's inbound path works while its
// outbound *.internal calls dial a loopback port nothing answers — so the
// acknowledgement, the one value that notices, has to say so beside the
// "serving on" line rather than leave the live number as the only hint.
//
// Driven through joinOpts' WithoutDialer deliberately: the warning is a
// diagnosis, not a routing change, and a service that builds its own
// transports from [voya.Dialer] or [voya.Transport] fails the same way.
func TestAMismatchedAcknowledgedProxyPortWarns(t *testing.T) {
	// Ports chosen so neither number is a substring of the other: a warning
	// naming only the acknowledged port must not pass the option-port check
	// the way 19777 contains 9777.
	const (
		optionPort = 12345
		ackPort    = 54321
	)

	// ackWaited joins, waits for the acknowledgement to have been read —
	// the registry entry exists before the client has read anything, so
	// only the "serving on" line says the comparison ran — and returns the
	// finished transcript, with the registrar's goroutine joined by stop.
	ackWaited := func(t *testing.T, v *voyad, port uint32) *logLines {
		t.Helper()
		var log logLines
		stop, err := voya.Join(context.Background(), "payments", tcpAddr(t, 51234),
			joinOpts(t, v, voya.WithProxyPort(port), voya.WithLogf(log.logf))...)
		if err != nil {
			t.Fatalf("Join: %v", err)
		}
		defer stop()
		waitFor(t, "the acknowledgement to be logged", func() bool {
			return len(log.matching("is serving on")) >= 1
		})
		stop()
		return &log
	}

	t.Run("a moved proxy_port is warned about, naming both ports", func(t *testing.T) {
		log := ackWaited(t, startVoyaServing(t, ackPort), optionPort)

		warnings := log.matching("WithProxyPort")
		if len(warnings) != 1 {
			t.Fatalf("the mismatch was warned about %d times, want once per acknowledgement:\n%s",
				len(warnings), log.transcript())
		}
		for _, says := range []string{"12345", "54321"} {
			if !strings.Contains(warnings[0], says) {
				t.Errorf("the warning does not name %s: %q — without both ports it is not actionable", says, warnings[0])
			}
		}
		// And the one remedy that is complete on its own. WithProxyPort
		// alone moves the dialer to a port the fleet's URLs do not name
		// (TestDialerIgnoresInternalOnOtherPorts), so a message offering
		// only that talks a developer into silencing this line while the
		// calls keep failing.
		if !strings.Contains(warnings[0], "revert voya's proxy_port") {
			t.Errorf("the warning does not name reverting voya's proxy_port: %q — the remedy it does name is not complete on its own", warnings[0])
		}
	})

	t.Run("matching ports stay silent", func(t *testing.T) {
		log := ackWaited(t, startVoyaServing(t, ackPort), ackPort)

		if got := log.matching("WithProxyPort"); len(got) != 0 {
			t.Errorf("agreeing ports were warned about:\n%s", strings.Join(got, "\n"))
		}
	})
}

// countingListener counts the connections a server accepted, which is how
// TestARefusedRegistrationStopsRetrying tells "gave up" from "retried
// quietly".
type countingListener struct {
	net.Listener
	n *atomicCounter
}

func (l countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.n.inc()
	}
	return conn, err
}

// TestARefusedRegistrationStopsRetrying: a name the registry will not accept
// — here an uppercase one, which Fly rejects for app names too — is not a
// reachability problem. Reconnecting re-sends byte-for-byte the same
// registration, so retrying is a loop that cannot terminate, and the "cannot
// reach voya" line it used to log names the wrong cause entirely.
//
// This is also the real-daemon half of the pairing client's
// TestOnlyAWireErrorCountsAsARefusal pins from its side: the refusal
// travelling here is voya's own, so a server that stopped rejecting with a
// wire error — or a classifier that stopped recognising one — both fail this
// test rather than each side's scripted stand-in staying green.
func TestARefusedRegistrationStopsRetrying(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	var conns atomicCounter
	v := &voyad{t: t, port: portOfListener(t, ln), proxyPort: voya.DefaultProxyPort}
	v.serve(countingListener{Listener: ln, n: &conns})
	t.Cleanup(v.stop)

	refusals := make(chan string, 8)
	stop, err := voya.Join(context.Background(), "Payments", tcpAddr(t, 51234),
		joinOpts(t, v, voya.WithBackoff(5*time.Millisecond, 20*time.Millisecond),
			voya.WithLogf(func(format string, args ...any) {
				if strings.Contains(format, "refused to register") {
					select {
					case refusals <- fmt.Sprintf(format, args...):
					default:
					}
				}
			}))...)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer stop()

	select {
	case line := <-refusals:
		if !strings.Contains(line, "Payments") {
			t.Errorf("the refusal did not name the service: %q", line)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("a registration voya refused was never reported as refused")
	}

	// And it must have given up. With backoff at 5-20ms, a client still
	// retrying would open dozens of connections in this window.
	settled := conns.get()
	time.Sleep(300 * time.Millisecond)
	if n := conns.get(); n != settled {
		t.Errorf("%d further connections after voya refused the name; the client is retrying a rejection no retry can fix", n-settled)
	}
}

func TestAnOutageIsReportedAgainAfterReconnecting(t *testing.T) {
	v := startVoya(t)

	// Both counts come from the client's own log, not the registry. The
	// registry holds a name from the moment voya reads the message, which is
	// before the client has read the acknowledgement — and it is the client
	// having seen that acknowledgement, not the entry existing, that re-arms
	// the warning. Waiting on the registry raced that window and killed voya
	// in between.
	var lost, serving atomicCounter
	stop, err := voya.Join(context.Background(), "payments", tcpAddr(t, 51234),
		joinOpts(t, v, voya.WithBackoff(5*time.Millisecond, 50*time.Millisecond),
			voya.WithLogf(func(format string, _ ...any) {
				switch {
				case strings.Contains(format, "cannot reach voya"):
					lost.inc()
				case strings.Contains(format, "is serving on"):
					serving.inc()
				}
			}))...)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer stop()

	waitFor(t, "the first registration", func() bool { return serving.get() >= 1 })

	v.stop()
	waitFor(t, "the first outage to be reported", func() bool { return lost.get() >= 1 })

	v.restart()
	waitFor(t, "the client to re-register with the new voya", func() bool { return serving.get() >= 2 })

	v.stop()
	waitFor(t, "the second outage to be reported", func() bool { return lost.get() >= 2 })
}

// TestStopIsSafeFromTwoGoroutines: a service with both `defer stop()` and a
// stop in its signal handler calls this concurrently. The guard has to be
// synchronised — and it has to make the second caller wait, since a stop
// that returns early tells the caller the name is deregistered while it
// still is not.
func TestStopIsSafeFromTwoGoroutines(t *testing.T) {
	v := startVoya(t)

	stop, err := voya.Join(context.Background(), "payments", tcpAddr(t, 51234), joinOpts(t, v)...)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	waitFor(t, "payments to register", func() bool {
		_, ok := v.registry().Lookup("payments")
		return ok
	})

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stop()
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent stop calls did not all return")
	}

	waitFor(t, "stop to deregister payments", func() bool {
		_, ok := v.registry().Lookup("payments")
		return !ok
	})
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

func (l *logLines) transcript() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
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

// TestStopReturnsPromptly is the regression test for a registration goroutine
// left blocked: stop waits for it, so anything that leaves it stuck hangs the
// service's shutdown rather than failing visibly. The connect-go behaviour
// underneath — cancellation alone does not unblock a waiting Receive — is
// pinned in-package by client's TestCancelDoesNotUnblockReceive.
func TestStopReturnsPromptly(t *testing.T) {
	v := startVoya(t)

	stop, err := voya.Join(context.Background(), "payments", tcpAddr(t, 51234), joinOpts(t, v)...)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	waitFor(t, "payments to register", func() bool {
		_, ok := v.registry().Lookup("payments")
		return ok
	})

	// The assertion is timeStop's own: it fails rather than hanging the suite
	// if stop never returns, which is the whole of what this test wants. How
	// long it took is #7's business, not this test's.
	timeStop(t, stop)
}

// --- #7: the shutdown grace --------------------------------------------------

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

// TestStopHoldsTheGrace is #7's mechanism: stop does not hand the service back
// to its caller the instant the stream is closed, because voya is still
// routing here for a moment after that. The caller's next move is to close its
// listener, and doing it inside that window is what drops requests.
func TestStopHoldsTheGrace(t *testing.T) {
	v := startVoya(t)

	const grace = 300 * time.Millisecond
	stop, err := voya.Join(context.Background(), "payments", tcpAddr(t, 51234),
		joinOpts(t, v, voya.WithShutdownGrace(grace))...)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	waitFor(t, "payments to register", func() bool {
		_, ok := v.registry().Lookup("payments")
		return ok
	})

	if elapsed := timeStop(t, stop); elapsed < grace {
		t.Errorf("stop returned after %v, before the %v grace was up; a service closing "+
			"its listener now would drop requests voya has already routed to it", elapsed, grace)
	}
	if _, ok := v.registry().Lookup("payments"); ok {
		t.Error("payments is still registered after a stop that waited out the whole grace")
	}
}

// TestStopSkipsTheGraceAfterEviction: a deposed instance is not being routed
// to — the newcomer is — so there is nothing to hold open. Waiting here would
// tax the one restart that is already going badly.
func TestStopSkipsTheGraceAfterEviction(t *testing.T) {
	v := startVoya(t)

	const grace = 10 * time.Second
	evictions := make(chan voya.Eviction, 4)
	stop, err := voya.Join(context.Background(), "payments", tcpAddr(t, 1001),
		joinOpts(t, v,
			voya.WithPID(111),
			voya.WithBackoff(5*time.Millisecond, 20*time.Millisecond),
			voya.WithOnEvicted(func(e voya.Eviction) { evictions <- e }),
			voya.WithReclaim(0, 0), // the eviction is final; #27's retry is not what is under test
			voya.WithShutdownGrace(grace),
		)...)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}

	waitFor(t, "payments to register", func() bool {
		up, ok := v.registry().Lookup("payments")
		return ok && up.Port == 1001
	})

	usurper, err := v.registry().Register(registry.Instance{Service: "payments", Port: 1002, PID: 222})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer usurper.Close()

	select {
	case <-evictions:
	case <-time.After(15 * time.Second):
		t.Fatal("the deposed client was never told it was evicted")
	}

	// Stopped the instant the eviction lands, with nothing waited out first.
	// That window — the session still finishing its teardown — is where the
	// deferred ctx.Err() check misses, and it is the realistic case: a
	// developer reads "was taken over" and hits Ctrl-C. session clears
	// routable on the Evicted arm itself, before onEvicted is called, so the
	// property holds here at the source rather than by the test standing back
	// far enough not to see it fail.
	if elapsed := timeStop(t, stop); elapsed > time.Second {
		t.Errorf("stop took %v after this instance had been evicted; nothing was being routed here", elapsed)
	}
}

// TestZeroShutdownGraceSkipsTheWait is the clock half of client's
// TestZeroShutdownGraceIsHonoured, whose resolved-options half stays
// in-package with the unexported options it reads.
//
// The claim here is only as strong as the bound, and the bound is coarse: with
// the grace turned off, stop against a live daemon does not wait out anything
// on the order of a second. It cannot separate a zero from
// [voya.DefaultShutdownGrace] — a settle that ignored the option and slept the
// 50ms default would pass this — because a bound tight enough to catch that
// would have to hold the whole of stop, a stream teardown competing for a core
// with everything else on the machine, under 50ms. That the zero survives
// resolution at all is client's half; this one holds the live path to it not
// becoming a long wait.
func TestZeroShutdownGraceSkipsTheWait(t *testing.T) {
	v := startVoya(t)

	stop, err := voya.Join(context.Background(), "payments", tcpAddr(t, 51234),
		joinOpts(t, v, voya.WithShutdownGrace(0))...)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	waitFor(t, "payments to register", func() bool {
		_, ok := v.registry().Lookup("payments")
		return ok
	})

	if elapsed := timeStop(t, stop); elapsed > time.Second {
		t.Errorf("stop took %v with the grace turned off; it waited out something", elapsed)
	}
}

// TestTheDefaultGraceApplies pins that a service which sets nothing gets the
// protection. The knob existing is not the point of #7 — the default being
// right is.
func TestTheDefaultGraceApplies(t *testing.T) {
	v := startVoya(t)

	stop, err := voya.Join(context.Background(), "payments", tcpAddr(t, 51234), joinOpts(t, v)...)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	waitFor(t, "payments to register", func() bool {
		_, ok := v.registry().Lookup("payments")
		return ok
	})

	if elapsed := timeStop(t, stop); elapsed < voya.DefaultShutdownGrace {
		t.Errorf("stop returned after %v; a service that configures nothing gets no grace at all", elapsed)
	}
}

// --- #27: reclaiming a name after being deposed ------------------------------

// TestReclaimsTheNameWhenTheUsurperLeaves is the workflow #27 chose: replace
// a service to try a fix, find the replacement worse, kill it, and have the
// original — which still holds its state and its debugger session — resume
// serving without being restarted.
func TestReclaimsTheNameWhenTheUsurperLeaves(t *testing.T) {
	v := startVoya(t)

	reclaims := make(chan voya.Reclaim, 4)
	stop, err := voya.Join(context.Background(), "payments", tcpAddr(t, 1001),
		joinOpts(t, v,
			voya.WithPID(111),
			voya.WithReclaim(30*time.Second, 10*time.Millisecond),
			voya.WithOnReclaimed(func(rc voya.Reclaim) { reclaims <- rc }),
		)...)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer stop()

	waitFor(t, "the original to register", func() bool {
		up, ok := v.registry().Lookup("payments")
		return ok && up.Port == 1001
	})

	// Somebody starts a replacement, which takes the name.
	usurper, err := v.registry().Register(registry.Instance{Service: "payments", Port: 1002, PID: 222})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	waitFor(t, "the replacement to hold the name", func() bool {
		up, ok := v.registry().Lookup("payments")
		return ok && up.Port == 1002
	})

	// It is worse, so it gets killed.
	usurper.Close()

	select {
	case rc := <-reclaims:
		if rc.Service != "payments" {
			t.Errorf("reclaim names %q", rc.Service)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the original never took its name back")
	}
	waitFor(t, "traffic to come back to the original", func() bool {
		up, ok := v.registry().Lookup("payments")
		return ok && up.Port == 1001
	})
}

// TestAnOrderlyStopAfterAReclaimIsQuiet is #56. The eviction branch in the
// client's run loop resumed the ordinary loop without re-checking the
// context, and reclaim returns on cancellation with the name held — so a
// service that had taken its name back and was then stopped signed off by
// telling the developer that voya was unreachable and that their name had
// stopped resolving. Nothing had gone wrong: they pressed ^C.
//
// It is the workflow #27 exists for, at its last step. The developer killed
// the replacement, watched the original take the name back, and then stopped
// it — and the last thing voya's client said was that voya was gone.
//
// Two things stop this passing for the wrong reason. The reclaim is waited
// for rather than assumed, because an instance that never reclaimed returns
// at the guard at the top of run's switch, which was always quiet. And the
// reclaim's own line is asserted present, because a [voya.WithLogf] that never
// reached the registrar would leave nothing to match and every check below
// would pass on an empty transcript.
func TestAnOrderlyStopAfterAReclaimIsQuiet(t *testing.T) {
	v := startVoya(t)

	var log logLines
	reclaims := make(chan voya.Reclaim, 4)
	stop, err := voya.Join(context.Background(), "payments", tcpAddr(t, 1001),
		joinOpts(t, v,
			voya.WithPID(111),
			voya.WithReclaim(30*time.Second, 10*time.Millisecond),
			voya.WithOnReclaimed(func(rc voya.Reclaim) { reclaims <- rc }),
			voya.WithLogf(log.logf),
		)...)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	// The stop below is the subject of the test rather than cleanup, but the
	// t.Fatal paths between here and it would otherwise leave the registrar
	// retrying against a daemon this test's cleanup has already stopped, and
	// leave its transport unreleased for the rest of the run. stop is a
	// [sync.OnceFunc], so the deferred call costs nothing once it has run.
	defer stop()

	waitFor(t, "the original to register", func() bool {
		up, ok := v.registry().Lookup("payments")
		return ok && up.Port == 1001
	})

	// Replaced, then the replacement is killed: the original asks for its
	// name back and gets it.
	usurper, err := v.registry().Register(registry.Instance{Service: "payments", Port: 1002, PID: 222})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	usurper.Close()
	select {
	case <-reclaims:
	case <-time.After(20 * time.Second):
		t.Fatal("the original never took its name back, so the stop below would not exercise the reclaim branch")
	}

	// The developer stops the service they have been debugging all along.
	// stop waits for the registration goroutine, so nothing is written to the
	// log after it returns.
	stop()

	if got := log.matching("asking for it back"); len(got) == 0 {
		t.Fatalf("the client logged no eviction at all, so nothing below is being tested:\n%s", log.transcript())
	}
	if got := log.matching("cannot reach voya"); len(got) != 0 {
		t.Errorf("stopping a service that had reclaimed its name reported an outage that did not happen:\n%s",
			strings.Join(got, "\n"))
	}
}

// TestReclaimNeverDeposesTheReplacement is the property that makes the
// retry loop safe rather than a slower version of the bug: while the
// replacement is alive, the deposed instance must keep losing.
func TestReclaimNeverDeposesTheReplacement(t *testing.T) {
	v := startVoya(t)

	stop, err := voya.Join(context.Background(), "payments", tcpAddr(t, 1001),
		joinOpts(t, v,
			voya.WithPID(111),
			voya.WithReclaim(30*time.Second, 5*time.Millisecond),
		)...)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer stop()
	waitFor(t, "the original to register", func() bool {
		up, ok := v.registry().Lookup("payments")
		return ok && up.Port == 1001
	})

	usurper, err := v.registry().Register(registry.Instance{Service: "payments", Port: 1002, PID: 222})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer usurper.Close()

	// With a 5ms retry interval, a claim that could depose would have done so
	// hundreds of times over this window.
	deadline := time.Now().Add(750 * time.Millisecond)
	for time.Now().Before(deadline) {
		up, ok := v.registry().Lookup("payments")
		if !ok {
			t.Fatal("the name went missing while the replacement held it")
		}
		if up.Port != 1002 {
			t.Fatalf("the deposed instance took the name back (port %d) while the replacement was still running", up.Port)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := len(v.registry().Evictions()); got != 1 {
		t.Errorf("Evictions() = %d, want 1 — the retries are deposing somebody", got)
	}
}

// TestReclaimNeverDeposesASecondReplacement is the same property one step
// further on, where it is easiest to lose: an instance that reclaimed its name
// and was then deposed again must still not fight for it. The reclaim window
// is spent, and the ordinary loop it would otherwise fall back into registers
// with last-writer-wins, which is #27 arrived at the long way round.
func TestReclaimNeverDeposesASecondReplacement(t *testing.T) {
	v := startVoya(t)

	reclaims := make(chan voya.Reclaim, 4)
	stop, err := voya.Join(context.Background(), "payments", tcpAddr(t, 1001),
		joinOpts(t, v,
			voya.WithPID(111),
			voya.WithReclaim(30*time.Second, 5*time.Millisecond),
			voya.WithOnReclaimed(func(rc voya.Reclaim) { reclaims <- rc }),
		)...)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer stop()
	waitFor(t, "the original to register", func() bool {
		up, ok := v.registry().Lookup("payments")
		return ok && up.Port == 1001
	})

	// Deposed, and then the replacement is killed, so the name comes back.
	usurper, err := v.registry().Register(registry.Instance{Service: "payments", Port: 1002, PID: 222})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	waitFor(t, "the replacement to hold the name", func() bool {
		up, ok := v.registry().Lookup("payments")
		return ok && up.Port == 1002
	})
	usurper.Close()
	select {
	case <-reclaims:
	case <-time.After(20 * time.Second):
		t.Fatal("the original never took its name back")
	}
	waitFor(t, "traffic to come back to the original", func() bool {
		up, ok := v.registry().Lookup("payments")
		return ok && up.Port == 1001
	})

	// A second replacement now deposes the instance that just got its name
	// back. It is alive, so it keeps it.
	second, err := v.registry().Register(registry.Instance{Service: "payments", Port: 1003, PID: 333})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer second.Close()
	waitFor(t, "the second replacement to hold the name", func() bool {
		up, ok := v.registry().Lookup("payments")
		return ok && up.Port == 1003
	})

	deadline := time.Now().Add(750 * time.Millisecond)
	for time.Now().Before(deadline) {
		up, ok := v.registry().Lookup("payments")
		if !ok {
			t.Fatal("the name went missing while the second replacement held it")
		}
		if up.Port != 1003 {
			t.Fatalf("the twice-deposed instance took the name back (port %d) from a live replacement", up.Port)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := len(v.registry().Evictions()); got != 2 {
		t.Errorf("Evictions() = %d, want 2 — something is still deposing somebody", got)
	}
}

// TestDeposedServiceGivesUpWhenVoyaIsUnreachable pins a divergence rather than
// a decision. The window bounds wall-clock time, not attempts, so an eviction
// that lands shortly before voya restarts spends the whole window failing to
// reach anybody — and the service then stays unregistered for the life of the
// process, even though the name is free and ADR 004's self-healing restart
// would otherwise have put it back.
//
// Whether an unreachable voya should consume the window is open (the ADR's
// bound is written as wall-clock, and unbounding it on outages would let a
// process forgotten overnight reclaim in the morning after all). This records
// what happens today so that changing it is a decision somebody made.
func TestDeposedServiceGivesUpWhenVoyaIsUnreachable(t *testing.T) {
	v := startVoya(t)

	evictions := make(chan voya.Eviction, 4)
	stop, err := voya.Join(context.Background(), "payments", tcpAddr(t, 1001),
		joinOpts(t, v,
			voya.WithPID(111),
			voya.WithReclaim(200*time.Millisecond, 10*time.Millisecond),
			voya.WithOnEvicted(func(e voya.Eviction) { evictions <- e }),
		)...)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer stop()
	waitFor(t, "the original to register", func() bool {
		_, ok := v.registry().Lookup("payments")
		return ok
	})

	if _, err := v.registry().Register(registry.Instance{Service: "payments", Port: 1002, PID: 222}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Wait until the client knows it was deposed, so what follows exercises
	// the reclaim path rather than an ordinary broken stream.
	select {
	case <-evictions:
	case <-time.After(20 * time.Second):
		t.Fatal("the client was never told it had been deposed")
	}

	// voya goes away for longer than the window and comes back with an empty
	// registry, so the name is free the moment it is up.
	v.stop()
	time.Sleep(500 * time.Millisecond)
	v.restart()

	time.Sleep(500 * time.Millisecond)
	if up, ok := v.registry().Lookup("payments"); ok {
		t.Errorf("the service registered again on port %d: the divergence this test pins is gone, so update it", up.Port)
	}
}

// TestReclaimGivesUpWhenTheWindowExpires: a process forgotten overnight must
// not take a name back the next morning.
func TestReclaimGivesUpWhenTheWindowExpires(t *testing.T) {
	v := startVoya(t)

	stop, err := voya.Join(context.Background(), "payments", tcpAddr(t, 1001),
		joinOpts(t, v,
			voya.WithPID(111),
			voya.WithReclaim(150*time.Millisecond, 10*time.Millisecond),
		)...)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer stop()
	waitFor(t, "the original to register", func() bool {
		_, ok := v.registry().Lookup("payments")
		return ok
	})

	usurper, err := v.registry().Register(registry.Instance{Service: "payments", Port: 1002, PID: 222})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Outlast the window, then free the name. Nothing should come back.
	time.Sleep(500 * time.Millisecond)
	usurper.Close()

	time.Sleep(500 * time.Millisecond)
	if up, ok := v.registry().Lookup("payments"); ok {
		t.Errorf("a service reclaimed its name (port %d) after the window had expired", up.Port)
	}
}

// TestReclaimCanBeTurnedOff restores the other answer #27 considered: an
// eviction is final, and the deposed instance leaves the name alone.
func TestReclaimCanBeTurnedOff(t *testing.T) {
	v := startVoya(t)

	stop, err := voya.Join(context.Background(), "payments", tcpAddr(t, 1001),
		joinOpts(t, v, voya.WithPID(111), voya.WithReclaim(0, 0))...)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer stop()
	waitFor(t, "the original to register", func() bool {
		_, ok := v.registry().Lookup("payments")
		return ok
	})

	usurper, err := v.registry().Register(registry.Instance{Service: "payments", Port: 1002, PID: 222})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	usurper.Close()

	time.Sleep(400 * time.Millisecond)
	if up, ok := v.registry().Lookup("payments"); ok {
		t.Errorf("the name came back (port %d) with reclaiming turned off", up.Port)
	}
}
