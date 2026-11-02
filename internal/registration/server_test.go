package registration

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/trendvidia/voya/internal/registry"
	registryv1 "github.com/trendvidia/voya/proto/voya/registry/v1"
	"github.com/trendvidia/voya/proto/voya/registry/v1/registryv1connect"
)

const testProxyPort = 9777

// conns records the TCP connections a client opened, so a test can kill them
// the way an unplugged machine or a SIGKILL'd process would — without the
// HTTP-level goodbye a graceful close sends.
type conns struct {
	mu  sync.Mutex
	all []net.Conn
}

func (c *conns) add(nc net.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.all = append(c.all, nc)
}

func (c *conns) killAll(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.all) == 0 {
		t.Fatal("no connection was recorded; the test would prove nothing")
	}
	for _, nc := range c.all {
		_ = nc.Close()
	}
}

// running is a live registration listener: the server, the address to dial,
// and the cancel that ends every stream it carries.
type running struct {
	*http.Server
	addr   string
	cancel context.CancelFunc
}

// serveRegistration starts the real server on a real loopback port. Going
// through Listen and NewHTTPServer rather than httptest means the h2c setup,
// the timeouts, the shutdown path and the loopback guard are the ones that
// ship.
func serveRegistration(t *testing.T, reg *registry.Registry, opts ...Option) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ln, err := Listen(ctx, "127.0.0.1:0")
	if err != nil {
		cancel()
		t.Fatalf("Listen: %v", err)
	}
	srv := NewHTTPServer(ctx, NewServer(reg, testProxyPort, opts...))

	// Serve's error is carried out on a channel rather than reported from the
	// goroutine: t.Errorf after the test has been marked complete panics and
	// takes the whole test binary down with it, hiding the failure it was
	// trying to report. The cleanup below waits for Serve to return first, so
	// there is no such window.
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
		if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve: %v", err)
		}
	})
	return &running{Server: srv, addr: ln.Addr().String(), cancel: cancel}
}

// newHTTPClient dials with prior-knowledge h2c from the standard library —
// Connect bidi streaming is HTTP/2 only, and there is no TLS to negotiate
// over loopback (issue #21).
func newHTTPClient(t *testing.T, tracked *conns) *http.Client {
	t.Helper()
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	transport := &http.Transport{
		Protocols: protocols,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			nc, err := (&net.Dialer{}).DialContext(ctx, network, address)
			if err == nil && tracked != nil {
				tracked.add(nc)
			}
			return nc, err
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport}
}

func newClient(t *testing.T, addr string, tracked *conns) registryv1connect.RegistrationServiceClient {
	t.Helper()
	return registryv1connect.NewRegistrationServiceClient(newHTTPClient(t, tracked), "http://"+addr)
}

// register opens a stream and sends one registration, returning the stream and
// the acknowledgement.
func register(
	t *testing.T,
	client registryv1connect.RegistrationServiceClient,
	req *registryv1.RegisterRequest,
) (*connect.BidiStreamForClient[registryv1.RegisterRequest, registryv1.RegisterResponse], *registryv1.RegisterResponse) {
	t.Helper()
	stream := client.Register(context.Background())
	if err := stream.Send(req); err != nil {
		t.Fatalf("Send: %v", err)
	}
	resp, err := stream.Receive()
	if err != nil {
		t.Fatalf("Receive acknowledgement: %v", err)
	}
	return stream, resp
}

// waitFor polls until cond holds. Eviction is driven by the transport
// noticing a closed connection, so the test cannot know exactly when — only
// that it has to happen promptly.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func registered(t *testing.T, resp *registryv1.RegisterResponse) *registryv1.Registered {
	t.Helper()
	got := resp.GetRegistered()
	if got == nil {
		t.Fatalf("expected a Registered event, got %v", resp.GetEvent())
	}
	return got
}

func TestRegisterAcknowledgesWithACompleteDialString(t *testing.T) {
	reg := registry.New()
	client := newClient(t, serveRegistration(t, reg).addr, nil)

	stream, resp := register(t, client, &registryv1.RegisterRequest{
		Service: "payments", Port: 51234, Pid: 4242,
	})
	defer stream.CloseRequest()

	ack := registered(t, resp)
	if got, want := ack.GetAuthority(), "payments.internal"; got != want {
		t.Errorf("authority = %q, want %q", got, want)
	}
	if got, want := ack.GetProxyPort(), uint32(testProxyPort); got != want {
		t.Errorf("proxy_port = %d, want %d", got, want)
	}

	up, ok := reg.Lookup("payments")
	if !ok {
		t.Fatal("the service is not in the registry after acknowledgement")
	}
	if up.Port != 51234 || up.PID != 4242 {
		t.Errorf("registry holds %+v, want port 51234 pid 4242", up)
	}
}

// TestClosedStreamEvicts is ADR 004's core claim: the stream is the liveness
// signal, so ending it deregisters the service with nothing else involved.
func TestClosedStreamEvicts(t *testing.T) {
	reg := registry.New()
	client := newClient(t, serveRegistration(t, reg).addr, nil)

	stream, _ := register(t, client, &registryv1.RegisterRequest{Service: "payments", Port: 51234})
	if _, ok := reg.Lookup("payments"); !ok {
		t.Fatal("not registered")
	}

	if err := stream.CloseRequest(); err != nil {
		t.Fatalf("CloseRequest: %v", err)
	}
	if err := stream.CloseResponse(); err != nil {
		t.Fatalf("CloseResponse: %v", err)
	}

	waitFor(t, "the closed stream to evict payments", func() bool {
		_, ok := reg.Lookup("payments")
		return !ok
	})
}

// TestAbruptDisconnectEvicts is the SIGKILL case: the process dies without
// closing anything at the HTTP level, and eviction has to happen as fast as
// TCP close detection allows rather than waiting for a timeout.
func TestAbruptDisconnectEvicts(t *testing.T) {
	reg := registry.New()
	tracked := &conns{}
	client := newClient(t, serveRegistration(t, reg).addr, tracked)

	register(t, client, &registryv1.RegisterRequest{Service: "payments", Port: 51234})
	if _, ok := reg.Lookup("payments"); !ok {
		t.Fatal("not registered")
	}

	tracked.killAll(t) // the machine went away mid-stream

	waitFor(t, "the killed connection to evict payments", func() bool {
		_, ok := reg.Lookup("payments")
		return !ok
	})
}

// TestHalfCloseDoesNotEvict pins a decision that is easy to get wrong and
// silent when wrong. A client may close its request side and stay perfectly
// alive, reading responses; treating that io.EOF as "the stream ended" would
// depose a healthy service.
func TestHalfCloseDoesNotEvict(t *testing.T) {
	reg := registry.New()
	client := newClient(t, serveRegistration(t, reg).addr, nil)

	stream, _ := register(t, client, &registryv1.RegisterRequest{Service: "payments", Port: 51234})
	if err := stream.CloseRequest(); err != nil {
		t.Fatalf("CloseRequest: %v", err)
	}

	// Long enough that an eviction driven by the half-close would have
	// happened; the eviction tests above complete in milliseconds.
	time.Sleep(250 * time.Millisecond)
	if _, ok := reg.Lookup("payments"); !ok {
		t.Fatal("half-closing the request side evicted a live service")
	}

	// Closing the response side too is the service actually leaving.
	if err := stream.CloseResponse(); err != nil {
		t.Fatalf("CloseResponse: %v", err)
	}
	waitFor(t, "the fully closed stream to evict payments", func() bool {
		_, ok := reg.Lookup("payments")
		return !ok
	})
}

// TestDuplicateRegistrationTellsTheIncumbent is ADR 002 over the wire: the
// deposed instance is told, by a typed event it can distinguish from a
// dropped stream (#27), and the message names both instances.
func TestDuplicateRegistrationTellsTheIncumbent(t *testing.T) {
	reg := registry.New()
	addr := serveRegistration(t, reg).addr

	incumbent := newClient(t, addr, nil)
	first, _ := register(t, incumbent, &registryv1.RegisterRequest{
		Service: "payments", Port: 1001, Pid: 111,
	})
	defer first.CloseRequest()

	usurper := newClient(t, addr, nil)
	second, ack := register(t, usurper, &registryv1.RegisterRequest{
		Service: "payments", Port: 1002, Pid: 222,
	})
	defer second.CloseRequest()
	registered(t, ack)

	resp, err := first.Receive()
	if err != nil {
		t.Fatalf("the incumbent was cut off without being told why: %v", err)
	}
	ev := resp.GetEvicted()
	if ev == nil {
		t.Fatalf("expected an Evicted event, got %v", resp.GetEvent())
	}
	if got, want := ev.GetReplacedByPort(), uint32(1002); got != want {
		t.Errorf("replaced_by_port = %d, want %d", got, want)
	}
	if got, want := ev.GetReplacedByPid(), int32(222); got != want {
		t.Errorf("replaced_by_pid = %d, want %d", got, want)
	}
	for _, want := range []string{"payments.internal", "1001", "1002"} {
		if !strings.Contains(ev.GetReason(), want) {
			t.Errorf("reason omits %q:\n%s", want, ev.GetReason())
		}
	}

	// And the winner is the one serving traffic.
	up, ok := reg.Lookup("payments")
	if !ok {
		t.Fatal("nothing is registered after the takeover")
	}
	if up.Port != 1002 {
		t.Errorf("registry holds port %d, want the incoming 1002", up.Port)
	}
}

// nextEvent is stream.Receive with a limit. A registered stream is silent by
// design, so without one a test expecting an event that never comes hangs until
// the binary's own timeout instead of failing.
func nextEvent(
	t *testing.T,
	stream *connect.BidiStreamForClient[registryv1.RegisterRequest, registryv1.RegisterResponse],
) *registryv1.RegisterResponse {
	t.Helper()
	type result struct {
		resp *registryv1.RegisterResponse
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := stream.Receive()
		got <- result{resp, err}
	}()
	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("the stream ended without saying why: %v", r.err)
		}
		return r.resp
	case <-time.After(10 * time.Second):
		t.Fatal("nothing arrived on the stream")
		return nil
	}
}

// TestAForgedPIDOverTheWireDeposesLoudly is #308 through the real handler: pid
// is a field the client fills in, so naming the holder's and a port of your own
// has to reach the holder as the same typed event any other takeover does.
func TestAForgedPIDOverTheWireDeposesLoudly(t *testing.T) {
	reg := registry.New()
	addr := serveRegistration(t, reg).addr

	holder, ack := register(t, newClient(t, addr, nil), &registryv1.RegisterRequest{
		Service: "payments", Port: 1001, Pid: 111,
	})
	defer func() { _ = holder.CloseRequest() }()
	registered(t, ack)

	forger, ack := register(t, newClient(t, addr, nil), &registryv1.RegisterRequest{
		Service: "payments", Port: 1002, Pid: 111,
	})
	defer func() { _ = forger.CloseRequest() }()
	registered(t, ack)

	if resp := nextEvent(t, holder); resp.GetEvicted() == nil {
		t.Fatalf("expected an Evicted event, got %v", resp.GetEvent())
	}
	if got := len(reg.Evictions()); got != 1 {
		t.Errorf("recorded %d evictions, want 1", got)
	}
}

// TestACopyHangingUpLeavesTheHolderRegistered is the two-step takeover over the
// wire: a stream identical to the holder's hangs up, the name stays, and the
// claim that follows has to depose the holder in the open.
func TestACopyHangingUpLeavesTheHolderRegistered(t *testing.T) {
	reg := registry.New()
	addr := serveRegistration(t, reg).addr
	client := newClient(t, addr, nil)
	req := &registryv1.RegisterRequest{Service: "payments", Port: 1001, Pid: 111}

	holder, ack := register(t, newClient(t, addr, nil), req)
	defer func() { _ = holder.CloseRequest() }()
	registered(t, ack)

	copied, ack := register(t, client, req)
	registered(t, ack)
	marker, ack := register(t, client, &registryv1.RegisterRequest{Service: "marker", Port: 2001})
	registered(t, ack)

	// CloseRequest alone is a half-close, which voya holds through on purpose.
	// The marker hangs up second on the same connection, and its name going
	// away shows voya has reached the teardown, so the check is not just early.
	for _, stream := range []*connect.BidiStreamForClient[registryv1.RegisterRequest, registryv1.RegisterResponse]{copied, marker} {
		if err := stream.CloseRequest(); err != nil {
			t.Fatalf("CloseRequest: %v", err)
		}
		if err := stream.CloseResponse(); err != nil {
			t.Fatalf("CloseResponse: %v", err)
		}
	}
	waitFor(t, "voya to tear the hung-up streams down", func() bool {
		_, ok := reg.Lookup("marker")
		return !ok
	})
	for deadline := time.Now().Add(50 * time.Millisecond); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		if _, ok := reg.Lookup("payments"); !ok {
			t.Fatal("the copy hanging up took the name from a holder still connected")
		}
	}

	claimant, ack := register(t, newClient(t, addr, nil), &registryv1.RegisterRequest{
		Service: "payments", Port: 1002, Pid: 666,
	})
	defer func() { _ = claimant.CloseRequest() }()
	registered(t, ack)
	if resp := nextEvent(t, holder); resp.GetEvicted() == nil {
		t.Fatalf("expected an Evicted event, got %v", resp.GetEvent())
	}
	if got := len(reg.Evictions()); got != 1 {
		t.Errorf("recorded %d evictions, want 1", got)
	}
}

// TestEvictedStreamEndsCleanly: the orphaned stream must not be left open
// forever, and it must not end as a transport error either — a client that
// saw a transport failure would be entitled to retry, which is the ping-pong
// #27 exists to prevent.
func TestEvictedStreamEndsCleanly(t *testing.T) {
	reg := registry.New()
	addr := serveRegistration(t, reg).addr

	incumbent := newClient(t, addr, nil)
	first, _ := register(t, incumbent, &registryv1.RegisterRequest{Service: "payments", Port: 1001})

	usurper := newClient(t, addr, nil)
	second, _ := register(t, usurper, &registryv1.RegisterRequest{Service: "payments", Port: 1002})
	defer second.CloseRequest()

	if _, err := first.Receive(); err != nil {
		t.Fatalf("expected the Evicted event: %v", err)
	}
	_, err := first.Receive()
	if err == nil {
		t.Fatal("the evicted stream stayed open after the eviction event")
	}
	// And it ended cleanly. A clean end of stream surfaces as io.EOF, which
	// carries no code of its own and which connect therefore reports as
	// CodeUnknown; any explicit code here — Unavailable, Internal,
	// DeadlineExceeded — is a transport failure the client is entitled to
	// retry, and retrying means racing the winner for the name, which is the
	// ping-pong #27 exists to prevent.
	if got := connect.CodeOf(err); got != connect.CodeUnknown {
		t.Errorf("the evicted stream ended with code %v (%v), want a clean end: "+
			"a client that sees a transport failure retries and fights for the name (#27)", got, err)
	}
}

// TestStreamThatNeverRegistersIsClosed: an opened stream that says nothing
// must not hold a goroutine and a connection indefinitely.
//
// This one is driven by a raw HTTP request rather than the generated client,
// and the reason is worth writing down. connect-go's client does not issue
// the HTTP request until the first Send, so `client.Register(ctx)` followed
// by silence never reaches the server at all — a first attempt at this test
// hung in BlockUntilResponseReady with no handler ever running. The edge is
// real all the same: anything that opens the HTTP/2 stream and sends no
// message leaves the handler blocked on its first Receive, and that is what
// the timeout is for.
func TestStreamThatNeverRegistersIsClosed(t *testing.T) {
	reg := registry.New()
	addr := serveRegistration(t, reg, WithRegistrationTimeout(150*time.Millisecond)).addr

	// A request body that stays open and never carries a message.
	body, silence := io.Pipe()
	t.Cleanup(func() { _ = silence.Close() })

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"http://"+addr+registryv1connect.RegistrationServiceRegisterProcedure, body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/connect+proto")
	req.Header.Set("Connect-Protocol-Version", "1")

	start := time.Now()
	resp, err := newHTTPClient(t, nil).Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	// The handler has to end the stream by itself; nothing else will.
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %v to close a stream that never registered", elapsed)
	}
	if !strings.Contains(string(payload), "no registration received") {
		t.Errorf("the stream ended without saying why:\n%s", payload)
	}
	if got := len(reg.Services()); got != 0 {
		t.Errorf("registry holds %d services after a stream that never registered", got)
	}
}

// TestUndecodableFirstMessageIsExplained: a first message voya cannot read is
// not the same event as a client that opened the stream and went away, and it
// must not be answered the same way. Ending the stream cleanly tells the client
// it succeeded, so it reconnects and sends the same unreadable bytes forever;
// ADR 005 wants the diagnosis delivered where the failure is.
//
// Raw HTTP for the same reason as [TestStreamThatNeverRegistersIsClosed]: the
// generated client only sends messages that encode.
func TestUndecodableFirstMessageIsExplained(t *testing.T) {
	reg := registry.New()
	addr := serveRegistration(t, reg).addr

	// One Connect streaming envelope — a flags byte, a four-byte big-endian
	// length, then the payload — carrying bytes that are not a
	// RegisterRequest. Wire type 7 does not exist, so this cannot decode as
	// any message.
	garbage := []byte{0xff, 0xff, 0xff, 0xff}
	var body bytes.Buffer
	body.WriteByte(0)
	if err := binary.Write(&body, binary.BigEndian, uint32(len(garbage))); err != nil {
		t.Fatalf("framing the envelope: %v", err)
	}
	body.Write(garbage)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"http://"+addr+registryv1connect.RegistrationServiceRegisterProcedure, &body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/connect+proto")
	req.Header.Set("Connect-Protocol-Version", "1")

	resp, err := newHTTPClient(t, nil).Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}

	if !strings.Contains(string(payload), "invalid_argument") {
		t.Errorf("a message voya could not decode ended the stream without an error the client can act on:\n%s", payload)
	}
	if got := len(reg.Services()); got != 0 {
		t.Errorf("registry holds %d services after an undecodable registration", got)
	}
}

// TestRegisteredStreamOutlivesTheTimeout pins the timeout's scope: it bounds
// getting a first message, and nothing after it. A timer that survived
// registration would evict every healthy service on an interval — the polling
// design ADR 004 rejected, arrived at by accident — and no other test here
// would notice, because they either register within milliseconds and finish or
// never register at all.
func TestRegisteredStreamOutlivesTheTimeout(t *testing.T) {
	const timeout = 100 * time.Millisecond
	reg := registry.New()
	client := newClient(t, serveRegistration(t, reg, WithRegistrationTimeout(timeout)).addr, nil)

	stream, _ := register(t, client, &registryv1.RegisterRequest{Service: "payments", Port: 1001})
	defer stream.CloseRequest()

	time.Sleep(5 * timeout)
	if _, ok := reg.Lookup("payments"); !ok {
		t.Fatal("the registration timeout kept running after the registration and evicted a live service")
	}
}

// TestZeroRegistrationTimeoutMeansNoLimit: zero is what an unset duration
// produces — a config field nobody filled in, a caller passing
// cfg.GetSomething().AsDuration() — and a timer of 0 is ready the instant the
// select runs. Read literally that rejects every registration on the machine,
// so zero has to mean what it means everywhere else in this package: no bound.
func TestZeroRegistrationTimeoutMeansNoLimit(t *testing.T) {
	reg := registry.New()
	client := newClient(t, serveRegistration(t, reg, WithRegistrationTimeout(0)).addr, nil)

	stream, resp := register(t, client, &registryv1.RegisterRequest{Service: "payments", Port: 1001})
	defer stream.CloseRequest()
	registered(t, resp)

	if _, ok := reg.Lookup("payments"); !ok {
		t.Fatal("a zero registration timeout rejected a valid registration")
	}
}

func TestRejectsUnusableRegistrations(t *testing.T) {
	cases := []struct {
		name    string
		req     *registryv1.RegisterRequest
		mention string
	}{
		{"uppercase name", &registryv1.RegisterRequest{Service: "Payments", Port: 1001}, "lowercase"},
		{"reserved name", &registryv1.RegisterRequest{Service: "voya", Port: 1001}, "reserved"},
		{"empty name", &registryv1.RegisterRequest{Service: "", Port: 1001}, "empty"},
		{"port zero", &registryv1.RegisterRequest{Service: "payments", Port: 0}, "actually bound"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := registry.New()
			client := newClient(t, serveRegistration(t, reg).addr, nil)

			stream := client.Register(context.Background())
			defer stream.CloseRequest()
			if err := stream.Send(tc.req); err != nil {
				t.Fatalf("Send: %v", err)
			}
			_, err := stream.Receive()
			if err == nil {
				t.Fatal("the server accepted an unusable registration")
			}
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("code = %v, want InvalidArgument: %v", connect.CodeOf(err), err)
			}
			if !strings.Contains(err.Error(), tc.mention) {
				t.Errorf("error does not explain the rule (%q): %v", tc.mention, err)
			}
			if got := len(reg.Services()); got != 0 {
				t.Errorf("registry holds %d services after a rejected registration", got)
			}
		})
	}
}

// TestSecondRegistrationMessageIsRejected: ADR 004's protocol is one message
// then silence, so a second one is the client and server disagreeing about
// what the stream is.
func TestSecondRegistrationMessageIsRejected(t *testing.T) {
	reg := registry.New()
	client := newClient(t, serveRegistration(t, reg).addr, nil)

	stream, _ := register(t, client, &registryv1.RegisterRequest{Service: "payments", Port: 1001})
	defer stream.CloseRequest()

	if err := stream.Send(&registryv1.RegisterRequest{Service: "payments", Port: 1002}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	_, err := stream.Receive()
	if err == nil {
		t.Fatal("a second registration message on an established stream was accepted")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument: %v", connect.CodeOf(err), err)
	}
	waitFor(t, "the rejected stream to evict payments", func() bool {
		_, ok := reg.Lookup("payments")
		return !ok
	})
}

// registerAll opens one stream per name against the same server, the way a
// fleet of services comes up.
func registerAll(t *testing.T, addr string, names ...string) {
	t.Helper()
	for _, name := range names {
		stream, _ := register(t, newClient(t, addr, nil), &registryv1.RegisterRequest{
			Service: name, Port: 1001,
		})
		t.Cleanup(func() { _ = stream.CloseRequest() })
	}
}

// TestServerShutdownEvictsEverything is the voya-restart half of ADR 004:
// every stream breaks at once, and the registry is left holding nothing to go
// stale. The clients reconnecting is #16.
func TestServerShutdownEvictsEverything(t *testing.T) {
	reg := registry.New()
	srv := serveRegistration(t, reg)

	registerAll(t, srv.addr, "payments", "billing")
	if got := len(reg.Services()); got != 2 {
		t.Fatalf("registry holds %d services, want 2", got)
	}

	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitFor(t, "shutdown to evict every service", func() bool {
		return len(reg.Services()) == 0
	})
}

// TestGracefulShutdownEvictsAndReturns is the SIGINT half, and it is the one
// that constrains the API. Every registration handler blocks until something
// ends it, so an http.Server carrying them never has an idle connection for
// Shutdown to wait on: Shutdown alone burns the whole grace period, gives up,
// and leaves every service registered and every stream open. Cancelling the
// context NewHTTPServer was built with is what ends them, after which the
// grace period is spent draining rather than waiting for streams that were
// never going to close.
func TestGracefulShutdownEvictsAndReturns(t *testing.T) {
	reg := registry.New()
	srv := serveRegistration(t, reg)

	registerAll(t, srv.addr, "payments", "billing")
	if got := len(reg.Services()); got != 2 {
		t.Fatalf("registry holds %d services, want 2", got)
	}

	srv.cancel()

	// Generous relative to what this should take, and short enough that the
	// "Shutdown hangs for the whole grace period" failure is a failure and
	// not a slow pass: a hung Shutdown returns context.DeadlineExceeded here.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown did not finish: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Shutdown took %v; it waited on streams instead of ending them", elapsed)
	}
	if got := len(reg.Services()); got != 0 {
		t.Errorf("registry holds %d services after a graceful shutdown", got)
	}
}

func TestListenRefusesAnythingButLoopback(t *testing.T) {
	refused := []string{"0.0.0.0:0", "192.168.1.10:0", ":0", "localhost:0"}
	for _, addr := range refused {
		t.Run(addr, func(t *testing.T) {
			ln, err := Listen(context.Background(), addr)
			if err == nil {
				_ = ln.Close()
				t.Fatalf("Listen(%q) bound a non-loopback address", addr)
			}
			if !strings.Contains(err.Error(), "loopback") {
				t.Errorf("error does not say why: %v", err)
			}
		})
	}

	// The guard must accept every loopback address, not just 127.0.0.1.
	// Whether the host can actually bind one is a different question:
	// Linux gives all of 127/8 to lo, macOS aliases only 127.0.0.1, and
	// ::1 depends on whether IPv6 is configured. So this separates our
	// refusal — which always says "loopback" — from the OS's.
	for _, addr := range []string{"127.0.0.1:0", "127.0.0.2:0", "[::1]:0"} {
		t.Run(addr, func(t *testing.T) {
			ln, err := Listen(context.Background(), addr)
			if err == nil {
				_ = ln.Close()
				return
			}
			if strings.Contains(err.Error(), "loopback") {
				t.Fatalf("Listen(%q) refused a loopback address: %v", addr, err)
			}
			t.Logf("guard accepted it; the host cannot bind it: %v", err)
		})
	}
}

// TestListenRefusesACancelledContext: net.ListenConfig consults ctx only in
// the resolver and the Control hook, and the IP literal the guard above insists
// on skips resolution — so without an explicit check a caller that has already
// given up gets a bound socket back, and port 9779 stays held until the process
// exits.
func TestListenRefusesACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ln, err := Listen(ctx, "127.0.0.1:0")
	if err == nil {
		_ = ln.Close()
		t.Fatal("Listen bound a socket for a caller that had already stopped: nobody is left to close it")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the error does not report the cancellation: %v", err)
	}
}

// TestKeepaliveIsWired guards the settings nothing else can observe. ADR 004
// makes HTTP/2 keepalive the only backstop for a socket that outlives its
// process — a SIGKILL on a wedged machine — while every eviction test here
// drives a close the transport notices at once and so passes without it.
func TestKeepaliveIsWired(t *testing.T) {
	srv := NewHTTPServer(context.Background(), NewServer(registry.New(), testProxyPort))

	if srv.HTTP2 == nil {
		t.Fatal("no HTTP2Config: nothing pings a stream that is silent by design, " +
			"so a hard-killed service is never evicted (ADR 004)")
	}
	if got, want := srv.HTTP2.SendPingTimeout, PingIdle; got != want {
		t.Errorf("SendPingTimeout = %v, want PingIdle (%v)", got, want)
	}
	if got, want := srv.HTTP2.PingTimeout, PingTimeout; got != want {
		t.Errorf("PingTimeout = %v, want PingTimeout (%v)", got, want)
	}
	// The other half of the same bargain: these bound a whole request and a
	// whole response, and this request lasts as long as the service does.
	if srv.ReadTimeout != 0 || srv.WriteTimeout != 0 {
		t.Errorf("ReadTimeout = %v and WriteTimeout = %v; both must stay zero, "+
			"or every service is evicted on a timer", srv.ReadTimeout, srv.WriteTimeout)
	}
}

func TestListenRejectsMalformedAddress(t *testing.T) {
	if _, err := Listen(context.Background(), "not-an-address"); err == nil {
		t.Fatal("Listen accepted a malformed address")
	}
}

// TestOnlyIfUnclaimedIsRefusedWhileTheNameIsHeld is #27 over the wire: the
// deposed instance asks for its name back, somebody else has it, and voya
// says no rather than handing it over.
func TestOnlyIfUnclaimedIsRefusedWhileTheNameIsHeld(t *testing.T) {
	reg := registry.New()
	running := serveRegistration(t, reg)

	holder := newClient(t, running.addr, nil)
	held, _ := register(t, holder, &registryv1.RegisterRequest{
		Service: "payments", Port: 1002, Pid: 222,
	})
	defer held.CloseRequest()

	claimant := newClient(t, running.addr, nil)
	stream := claimant.Register(context.Background())
	defer stream.CloseRequest()
	if err := stream.Send(&registryv1.RegisterRequest{
		Service: "payments", Port: 1001, Pid: 111, OnlyIfUnclaimed: true,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	_, err := stream.Receive()
	if err == nil {
		t.Fatal("the careful claim took a held name")
	}
	if got := connect.CodeOf(err); got != connect.CodeAlreadyExists {
		t.Errorf("code = %v, want already_exists — the answer may differ later, so this is not invalid_argument", got)
	}

	up, live := reg.Lookup("payments")
	if !live || up.Port != 1002 {
		t.Errorf("the holder was displaced: live=%v port=%d", live, up.Port)
	}
}

// TestOnlyIfUnclaimedSucceedsOnAFreeName: the same request when whoever
// deposed it has gone.
func TestOnlyIfUnclaimedSucceedsOnAFreeName(t *testing.T) {
	reg := registry.New()
	running := serveRegistration(t, reg)

	claimant := newClient(t, running.addr, nil)
	stream := claimant.Register(context.Background())
	defer stream.CloseRequest()
	if err := stream.Send(&registryv1.RegisterRequest{
		Service: "payments", Port: 1001, Pid: 111, OnlyIfUnclaimed: true,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	resp, err := stream.Receive()
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if registered(t, resp).GetAuthority() != "payments.internal" {
		t.Errorf("acknowledgement = %v", resp.GetEvent())
	}
	if _, live := reg.Lookup("payments"); !live {
		t.Error("the claim was acknowledged but nothing is registered")
	}
}
