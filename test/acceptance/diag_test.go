package acceptance

// --- #149: the voya.internal programmatic surface ------------------------------
//
// ADR 005 puts the same data `voya status` and `voya routes` print under a
// reserved voya.internal authority on the proxy port, so tooling reads what
// voya knows instead of scraping CLI output. The daemon here is a process of
// its own, waited on by its banner — never by a dial — and everything below
// is read from outside it: through the proxy port, with the reserved
// authority, the way every M2 command will.

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"connectrpc.com/connect"

	voya "github.com/trendvidia/voya/client"
	"github.com/trendvidia/voya/internal/registry"
	diagv1 "github.com/trendvidia/voya/proto/voya/diag/v1"
	"github.com/trendvidia/voya/proto/voya/diag/v1/diagv1connect"
)

// diagAuthority is the dial string every consumer of DiagService uses:
// voya.internal on the proxy port — the same shape a service's outbound call
// has, so a working GetStatus also proves the proxy's authority routing for
// the reserved name.
func diagAuthority(proxyPort uint32) string {
	return registry.Authority(registry.Reserved) + ":" + strconv.FormatUint(uint64(proxyPort), 10)
}

// diagClient is DiagService dialled the way a CLI in another process will
// dial it: the client package's dialer, which redirects every *.internal name
// — the reserved one included — to the proxy port, and h2c so a stream can be
// held open and cancelled.
func diagClient(t *testing.T, proxyPort uint32) diagv1connect.DiagServiceClient {
	t.Helper()
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	transport := &http.Transport{
		Protocols:   protocols,
		DialContext: voya.Dialer(voya.WithProxyPort(proxyPort)),
	}
	t.Cleanup(transport.CloseIdleConnections)
	return diagv1connect.NewDiagServiceClient(&http.Client{Transport: transport}, "http://"+diagAuthority(proxyPort))
}

// status is one GetStatus, failing the test on anything but an answer: it
// is the call ADR 005 says must stay instant and infallible.
func status(t *testing.T, c diagv1connect.DiagServiceClient) *diagv1.GetStatusResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), diagnosticDeadline)
	defer cancel()
	res, err := c.GetStatus(ctx, connect.NewRequest(&diagv1.GetStatusRequest{}))
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	return res.Msg
}

// holdsViaDiag is [startServiceOn]'s readiness predicate read through
// DiagService: the daemon lists name on port among its services.
func holdsViaDiag(c diagv1connect.DiagServiceClient, name string) func(port uint32) bool {
	return func(port uint32) bool {
		ctx, cancel := context.WithTimeout(context.Background(), diagnosticDeadline)
		defer cancel()
		res, err := c.GetStatus(ctx, connect.NewRequest(&diagv1.GetStatusRequest{}))
		if err != nil {
			return false
		}
		for _, svc := range res.Msg.GetServices() {
			if svc.GetName() == name && svc.GetPort() == port {
				return true
			}
		}
		return false
	}
}

// TestDiagServiceDescribesTheFleet is #149 end to end: a real `voya run`,
// two registered services, and status, routes, an eviction and a reclaim
// all read from another process through voya.internal on the proxy port.
func TestDiagServiceDescribesTheFleet(t *testing.T) {
	v := startRealDaemon(t)
	c := diagClient(t, v.proxyPort)

	// Before anything registers, the daemon still answers, and about itself:
	// Self carries the ports it was started with, which is what tells a
	// `voya status` that reached some daemon that it reached this one.
	fresh := status(t, c)
	self := fresh.GetSelf()
	if self.GetProxyPort() != v.proxyPort || self.GetRegistrationPort() != v.registrationPort {
		t.Errorf("Self ports = proxy %d, registration %d; the daemon was started on %d and %d",
			self.GetProxyPort(), self.GetRegistrationPort(), v.proxyPort, v.registrationPort)
	}
	if self.GetPid() == 0 || self.GetVersion() == "" || self.GetStartedAt() == nil {
		t.Errorf("Self is incomplete: %v", self)
	}
	if n := len(fresh.GetServices()); n != 0 {
		t.Fatalf("a fresh daemon lists %d services", n)
	}

	one := startServiceOn(t, v.proxyPort, v.registrationPort, "one", "one/first", holdsViaDiag(c, "one"))
	other := startServiceOn(t, v.proxyPort, v.registrationPort, "other", "other/incumbent", holdsViaDiag(c, "other"),
		// The incumbent will ask for its name back once deposed below; the
		// window and interval are the ones TestEvictionAndReclaimReachVoyasTerminal
		// argues for.
		voya.WithReclaim(3*waitDeadline, 10*time.Millisecond))

	// GetStatus: both services, by name, with the authority and the port
	// each registered.
	got := status(t, c)
	if len(got.GetServices()) != 2 {
		t.Fatalf("services = %v, want one and other", got.GetServices())
	}
	for i, want := range []*service{one, other} {
		svc := got.GetServices()[i]
		if svc.GetName() != want.name || svc.GetAuthority() != want.name+".internal" ||
			svc.GetPort() != want.port() || svc.GetPid() != fakePID(want.ln) ||
			svc.GetStream() != diagv1.StreamState_STREAM_STATE_ATTACHED {
			t.Errorf("services[%d] = %v, want %s on port %d, attached", i, svc, want.name, want.port())
		}
		if svc.GetSince() == nil || svc.GetSince().AsTime().After(time.Now()) {
			t.Errorf("services[%d].since = %v", i, svc.GetSince())
		}
	}

	// ListRoutes: authority -> service -> upstream, the same two.
	routes, err := c.ListRoutes(context.Background(), connect.NewRequest(&diagv1.ListRoutesRequest{}))
	if err != nil {
		t.Fatalf("ListRoutes: %v", err)
	}
	if len(routes.Msg.GetRoutes()) != 2 {
		t.Fatalf("routes = %v, want two", routes.Msg.GetRoutes())
	}
	for i, want := range []*service{one, other} {
		route := routes.Msg.GetRoutes()[i]
		if route.GetAuthority() != want.name+".internal" || route.GetService() != want.name ||
			route.GetUpstream() != loopback(want.port()) {
			t.Errorf("routes[%d] = %v, want %s.internal -> %s -> %s", i, route, want.name, want.name, loopback(want.port()))
		}
	}

	// Depose the incumbent. The eviction is announced on the daemon's stderr
	// at the moment; the record is what a developer reads after the warning
	// has scrolled — both instances named, so they can find the process that
	// stopped receiving traffic (ADR 002, ADR 005).
	usurper := startServiceOn(t, v.proxyPort, v.registrationPort, "other", "other/usurper", holdsViaDiag(c, "other"))
	afterEviction := status(t, c)
	if len(afterEviction.GetEvictions()) != 1 {
		t.Fatalf("evictions = %v, want the one just staged", afterEviction.GetEvictions())
	}
	ev := afterEviction.GetEvictions()[0]
	if ev.GetEvicted().GetName() != "other" || ev.GetEvicted().GetPort() != other.port() || ev.GetEvicted().GetPid() != fakePID(other.ln) ||
		ev.GetIncoming().GetPort() != usurper.port() || ev.GetIncoming().GetPid() != fakePID(usurper.ln) || ev.GetAt() == nil {
		t.Errorf("eviction = %v, want other's port %d/pid %d deposed by port %d/pid %d",
			ev, other.port(), fakePID(other.ln), usurper.port(), fakePID(usurper.ln))
	}
	if len(afterEviction.GetReclaims()) != 0 {
		t.Errorf("a reclaim is recorded before anything was reclaimed: %v", afterEviction.GetReclaims())
	}

	// The usurper goes away and the incumbent, asking all along, gets its
	// name back — and that is recorded too, timed from its own eviction.
	usurper.shutdown()
	var reclaimed *diagv1.GetStatusResponse
	waitFor(t, "the reclaim to be recorded", func() bool {
		reclaimed = status(t, c)
		return len(reclaimed.GetReclaims()) > 0
	})
	rc := reclaimed.GetReclaims()[0]
	if rc.GetInstance().GetName() != "other" || rc.GetInstance().GetPort() != other.port() ||
		rc.GetEvictedAt() == nil || !rc.GetEvictedAt().AsTime().Equal(ev.GetAt().AsTime()) || rc.GetAt() == nil {
		t.Errorf("reclaim = %v, want other's port %d, evicted at %v", rc, other.port(), ev.GetAt().AsTime())
	}
	// And the eviction record survives the reclaim: `voya status` shows the
	// whole story, not the last chapter.
	if len(reclaimed.GetEvictions()) != 1 {
		t.Errorf("the eviction record went with the reclaim: %v", reclaimed.GetEvictions())
	}
	if n := len(reclaimed.GetServices()); n != 2 || reclaimed.GetServices()[1].GetPort() != other.port() {
		t.Errorf("after the reclaim services = %v, want one and other on port %d", reclaimed.GetServices(), other.port())
	}
}

// TestTheRequestLogIsReadFromAnotherProcess is #150 end to end: a call
// proxied by a real `voya run` is in ListRequests, with the fields the
// caller can vouch for, and a follower attached through WatchRequests
// receives the next one as it happens — then ends cleanly on its own cancel.
func TestTheRequestLogIsReadFromAnotherProcess(t *testing.T) {
	v := startRealDaemon(t)
	c := diagClient(t, v.proxyPort)
	other := startServiceOn(t, v.proxyPort, v.registrationPort, "other", "other/only", holdsViaDiag(c, "other"))

	if _, err := call(t, callerVia(t, v.proxyPort, "other")); err != nil {
		t.Fatalf("the call through the proxy failed: %v", err)
	}
	// The record lands after the response is written; the caller can be
	// ahead of it by a scheduling quantum.
	//
	// The authority is matched whole and without a port, which is the end of
	// #161 seen from outside: this caller is h2 and names voya's own port in
	// `:authority`, an HTTP/1.1 one would name none, and the record reads the
	// same either way.
	var got *diagv1.Request
	waitFor(t, "the proxied call to be recorded", func() bool {
		res, err := c.ListRequests(context.Background(), connect.NewRequest(&diagv1.ListRequestsRequest{}))
		if err != nil {
			return false
		}
		for _, r := range res.Msg.GetRequests() {
			if r.GetAuthority() == "other.internal" {
				got = r
				return true
			}
		}
		return false
	})
	if got.GetMethod() != http.MethodPost || got.GetPath() != procedure || got.GetStatus() != http.StatusOK ||
		got.GetUpstreamPort() != other.port() || got.GetDuration().AsDuration() <= 0 || got.GetAt() == nil {
		t.Errorf("recorded %v, want POST %s 200 -> %d with a timestamp and a duration", got, procedure, other.port())
	}

	// The follower. connect-go's client blocks in the call until the first
	// message arrives, so the call runs in a goroutine and what it received
	// comes back over a channel. Its watcher registers when the handler
	// runs, which nothing on this side can observe — so drive calls until
	// one is streamed (the setup, asserted), then count the next.
	ctx, cancel := context.WithCancel(context.Background())
	received := make(chan *diagv1.Request, 64)
	ended := make(chan error, 1)
	go func() {
		stream, err := c.WatchRequests(ctx, connect.NewRequest(&diagv1.WatchRequestsRequest{}))
		if err != nil {
			ended <- err
			return
		}
		for stream.Receive() {
			received <- stream.Msg().GetRequest()
		}
		ended <- stream.Err()
	}()
	caller := callerVia(t, v.proxyPort, "other")
	waitFor(t, "the follower to stream a call", func() bool {
		if _, err := call(t, caller); err != nil {
			t.Fatalf("the call through the proxy failed: %v", err)
		}
		select {
		case r := <-received:
			if r.GetPath() != procedure {
				t.Fatalf("the follower streamed %v first", r)
			}
			return true
		case err := <-ended:
			t.Fatalf("the follower's stream ended during the setup: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
		return false
	})
	// Drain what the setup's calls left, then one call, one record.
	for len(received) > 0 {
		<-received
	}
	if _, err := call(t, caller); err != nil {
		t.Fatalf("the call through the proxy failed: %v", err)
	}
	select {
	case r := <-received:
		if r.GetPath() != procedure || r.GetStatus() != http.StatusOK || r.GetUpstreamPort() != other.port() {
			t.Errorf("the follower streamed %v, want the call just made", r)
		}
	case err := <-ended:
		t.Fatalf("the follower's stream ended: %v", err)
	case <-time.After(diagnosticDeadline):
		t.Fatalf("the follower did not stream the call within %s", diagnosticDeadline)
	}
	cancel()
	select {
	case err := <-ended:
		if connect.CodeOf(err) != connect.CodeCanceled {
			t.Errorf("the follower's stream ended with %v, want its own cancellation", err)
		}
	case <-time.After(diagnosticDeadline):
		t.Fatalf("the follower's stream did not end within %s of cancelling", diagnosticDeadline)
	}
	v.FailIfExited(t, "while the request log was being read")
}
