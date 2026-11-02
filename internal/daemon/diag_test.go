package daemon

import (
	"context"
	"net"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/trendvidia/voya/internal/buildinfo"
	"github.com/trendvidia/voya/internal/registry"
	diagv1 "github.com/trendvidia/voya/proto/voya/diag/v1"
	"github.com/trendvidia/voya/proto/voya/diag/v1/diagv1connect"
)

// diagClient dials DiagService the way every consumer will: the proxy port,
// with the reserved authority as the URL's host — the same path a service's
// outbound call takes through the client package's dialer, so a working
// GetStatus also proves the proxy routes the reserved name (#149). h2c only,
// as the fleet's clients are. The dial goes to 127.0.0.1:port whatever the
// URL says, which is what that dialer does for a *.internal name and what
// keeps this package from importing it.
func diagClient(t *testing.T, proxyPort uint32) diagv1connect.DiagServiceClient {
	t.Helper()
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	addr := net.JoinHostPort("127.0.0.1", strconv.FormatUint(uint64(proxyPort), 10))
	tr := &http.Transport{
		Protocols: protocols,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
	return diagv1connect.NewDiagServiceClient(&http.Client{Transport: tr},
		"http://"+registry.Authority(registry.Reserved)+":"+strconv.FormatUint(uint64(proxyPort), 10))
}

func TestDiagServiceIsServedUnderTheReservedName(t *testing.T) {
	log := NewTranscript(nil)
	cfg := testConfig(t, 250*time.Millisecond)
	before := time.Now()
	d, err := Start(context.Background(), cfg, log.Logf)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	upstream(t, d, "payments", 0, nil)
	c := diagClient(t, cfg.GetProxyPort())

	res, err := c.GetStatus(context.Background(), connect.NewRequest(&diagv1.GetStatusRequest{}))
	if err != nil {
		t.Fatalf("GetStatus through the proxy port under %s: %v", registry.Authority(registry.Reserved), err)
	}
	self := res.Msg.GetSelf()
	if self.GetProxyPort() != cfg.GetProxyPort() || self.GetDnsPort() != cfg.GetDnsPort() ||
		self.GetRegistrationPort() != cfg.GetRegistrationPort() {
		t.Errorf("Self ports = %d/%d/%d, want the config's %d/%d/%d",
			self.GetProxyPort(), self.GetDnsPort(), self.GetRegistrationPort(),
			cfg.GetProxyPort(), cfg.GetDnsPort(), cfg.GetRegistrationPort())
	}
	if self.GetPid() != int32(os.Getpid()) {
		t.Errorf("Self.pid = %d, want this process's %d", self.GetPid(), os.Getpid())
	}
	if self.GetVersion() != buildinfo.Version() {
		t.Errorf("Self.version = %q, want what voya version prints, %q", self.GetVersion(), buildinfo.Version())
	}
	if at := self.GetStartedAt().AsTime(); at.Before(before) || at.After(time.Now()) {
		t.Errorf("Self.started_at = %v, not between Start (%v) and now", at, before)
	}
	if n := len(res.Msg.GetServices()); n != 1 || res.Msg.GetServices()[0].GetName() != "payments" {
		t.Errorf("services = %v, want the one registered", res.Msg.GetServices())
	}
}

// TestAFollowerDoesNotHoldCloseToTheGrace: a `voya log -f` with its
// WatchRequests open is a request inside the proxy handler for as long as it
// stays. Close ends it before the drain, so it neither burns the grace nor
// gets reported as work cut off.
func TestAFollowerDoesNotHoldCloseToTheGrace(t *testing.T) {
	const grace = 2 * time.Second
	log := NewTranscript(nil)
	cfg := testConfig(t, grace)
	d, err := Start(context.Background(), cfg, log.Logf)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	c := diagClient(t, cfg.GetProxyPort())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// connect-go's server-streaming client blocks in the call until the
	// response headers arrive, and the handler sends those when it ends; so
	// the call is the follower, run in a goroutine, and "attached" is the
	// daemon's own count of requests in the proxy handler.
	streamEnded := make(chan error, 1)
	go func() {
		stream, err := c.WatchRequests(ctx, connect.NewRequest(&diagv1.WatchRequestsRequest{}))
		if err != nil {
			streamEnded <- err
			return
		}
		for stream.Receive() {
		}
		streamEnded <- stream.Err()
	}()
	waitUntil(t, "the follower to be inside the proxy handler", func() bool { return d.inflight.Load() == 1 })

	start := time.Now()
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waited := time.Since(start)

	if waited >= grace {
		t.Errorf("Close took %s with a follower attached: the %s grace was burned on a stream Close itself ends", waited, grace)
	}
	if log.Printed("shutdown grace") {
		t.Errorf("Close reported the follower as work at the deadline:\n%s", log)
	}
	select {
	case err := <-streamEnded:
		if err != nil {
			// The daemon closed the connection under the follower once the
			// stream ended, so an error here is expected; what is not is a
			// stream that never ended.
			t.Logf("the follower's stream ended with: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the follower's stream did not end after Close")
	}
}
