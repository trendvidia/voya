package diag

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/trendvidia/voya/internal/registry"
	"github.com/trendvidia/voya/internal/requestlog"
	diagv1 "github.com/trendvidia/voya/proto/voya/diag/v1"
	"github.com/trendvidia/voya/proto/voya/diag/v1/diagv1connect"
)

// serve mounts s the way the daemon does — behind an h2c server — and returns
// a client aimed at it. The daemon's own path from the proxy port to this
// handler is internal/proxy's and test/acceptance's to prove; here the wire
// is real and the routing is not.
func serve(t *testing.T, s *Server) diagv1connect.DiagServiceClient {
	t.Helper()
	return serveWith(t, s.Handler())
}

func serveWith(t *testing.T, h http.Handler) diagv1connect.DiagServiceClient {
	t.Helper()
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := httptest.NewUnstartedServer(h)
	srv.Config.Protocols = protocols
	srv.Start()
	t.Cleanup(srv.Close)

	transport := &http.Transport{Protocols: protocols}
	t.Cleanup(transport.CloseIdleConnections)
	return diagv1connect.NewDiagServiceClient(&http.Client{Transport: transport}, srv.URL)
}

var self = Self{
	PID:              4242,
	StartedAt:        time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC),
	ProxyPort:        9777,
	DNSPort:          9778,
	RegistrationPort: 9779,
	Version:          "v1.2.3",
}

// newLog is the request log every server here gets unless a test is about
// not having one: a fresh buffer at the daemon's capacity.
func newLog() *requestlog.Log { return requestlog.New(requestlog.Capacity) }

// record is the i-th request of a test, the way the proxy would record it.
func record(i int) requestlog.Request {
	return requestlog.Request{
		At:           time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Millisecond),
		Authority:    "payments.internal",
		Method:       "POST",
		Path:         "/n/" + strconv.Itoa(i),
		Status:       200,
		Duration:     time.Duration(i+1) * time.Microsecond,
		UpstreamPort: 54321,
	}
}

func mustRegister(t *testing.T, r *registry.Registry, inst registry.Instance) *registry.Registration {
	t.Helper()
	h, err := r.Register(inst)
	if err != nil {
		t.Fatalf("Register(%+v): %v", inst, err)
	}
	return h
}

// TestHandlerClaimsEveryProcedure holds the mux to the schema: every RPC in
// voya.diag.v1.DiagService is claimed, and nothing else under the name is —
// the two facts the proxy's internal router relies on. Swept from the file
// descriptor rather than the list the mux is built from, because a fifth
// RPC added to the .proto and forgotten here would otherwise answer the
// reserved row's unimplemented for a procedure that exists.
func TestHandlerClaimsEveryProcedure(t *testing.T) {
	mux := NewServer(registry.New(), self, newLog()).Handler()
	services := diagv1.File_voya_diag_v1_diag_proto.Services()
	if services.Len() != 1 {
		t.Fatalf("voya.diag.v1 declares %d services, want 1", services.Len())
	}
	methods := services.Get(0).Methods()
	if methods.Len() == 0 {
		t.Fatal("DiagService has no methods; the sweep below would be vacuous")
	}
	for i := range methods.Len() {
		m := methods.Get(i)
		path := "/" + string(services.Get(0).FullName()) + "/" + string(m.Name())
		req := httptest.NewRequest(http.MethodPost, path, nil)
		if _, pattern := mux.Handler(req); pattern == "" {
			t.Errorf("%s is in the schema and the mux does not claim it", path)
		}
	}
	for _, path := range []string{"/", "/voya.diag.v1.DiagService/NoSuchProcedure", "/voya.diag.v1.DiagService/"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if _, pattern := mux.Handler(req); pattern != "" {
			t.Errorf("%s is not a procedure and the mux claims it under %q", path, pattern)
		}
	}
}

// TestGetStatusRendersTheRegistry: two services, one deposed and reclaimed,
// come back as ADR 005's status — voya itself, the live services by name,
// and the after-the-fact record of the eviction and the reclaim with both
// instances named.
func TestGetStatusRendersTheRegistry(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 30, 0, 0, time.UTC)
	reg := registry.New(registry.WithClock(func() time.Time { return now }))
	c := serve(t, NewServer(reg, self, newLog()))

	mustRegister(t, reg, registry.Instance{Service: "payments", Port: 50001, PID: 111})
	mustRegister(t, reg, registry.Instance{Service: "billing", Port: 50002, PID: 222})
	now = now.Add(time.Minute)
	usurper := mustRegister(t, reg, registry.Instance{Service: "payments", Port: 50003, PID: 333})
	usurper.Close()
	now = now.Add(time.Minute)
	if _, err := reg.RegisterIfFree(registry.Instance{Service: "payments", Port: 50001, PID: 111}); err != nil {
		t.Fatalf("reclaim: %v", err)
	}

	res, err := c.GetStatus(context.Background(), connect.NewRequest(&diagv1.GetStatusRequest{}))
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	got := res.Msg

	s := got.GetSelf()
	if s.GetPid() != self.PID || s.GetProxyPort() != self.ProxyPort || s.GetDnsPort() != self.DNSPort ||
		s.GetRegistrationPort() != self.RegistrationPort || s.GetVersion() != self.Version ||
		!s.GetStartedAt().AsTime().Equal(self.StartedAt) {
		t.Errorf("Self = %v, want %+v", s, self)
	}

	var names []string
	for _, svc := range got.GetServices() {
		names = append(names, svc.GetName())
	}
	if want := []string{"billing", "payments"}; !slices.Equal(names, want) {
		t.Fatalf("services = %v, want %v (sorted by name)", names, want)
	}
	payments := got.GetServices()[1]
	if payments.GetAuthority() != "payments.internal" || payments.GetPort() != 50001 || payments.GetPid() != 111 ||
		payments.GetStream() != diagv1.StreamState_STREAM_STATE_ATTACHED || !payments.GetSince().AsTime().Equal(now) {
		t.Errorf("payments = %v", payments)
	}

	if len(got.GetEvictions()) != 1 {
		t.Fatalf("evictions = %v, want one", got.GetEvictions())
	}
	ev := got.GetEvictions()[0]
	if ev.GetEvicted().GetName() != "payments" || ev.GetEvicted().GetPort() != 50001 || ev.GetEvicted().GetPid() != 111 ||
		ev.GetIncoming().GetPort() != 50003 || ev.GetIncoming().GetPid() != 333 ||
		!ev.GetAt().AsTime().Equal(now.Add(-time.Minute)) {
		t.Errorf("eviction = %v: both instances and the time have to be there", ev)
	}

	if len(got.GetReclaims()) != 1 {
		t.Fatalf("reclaims = %v, want one", got.GetReclaims())
	}
	rc := got.GetReclaims()[0]
	if rc.GetInstance().GetPort() != 50001 || !rc.GetAt().AsTime().Equal(now) ||
		!rc.GetEvictedAt().AsTime().Equal(now.Add(-time.Minute)) {
		t.Errorf("reclaim = %v", rc)
	}
}

// TestUnsetTimesStayUnset: a time the registry left zero — a Self with no
// start time, a reclaim's evicted_at when it followed no recorded eviction —
// is unset on the wire, not year 1. Both go through [timeOrNil]; Self is
// the one a test can stage from outside the registry.
func TestUnsetTimesStayUnset(t *testing.T) {
	reg := registry.New()
	c := serve(t, NewServer(reg, Self{}, newLog()))
	if got := timeOrNil(time.Time{}); got != nil {
		t.Errorf("timeOrNil(zero) = %v, want nil", got)
	}
	if got := timeOrNil(self.StartedAt); !got.AsTime().Equal(self.StartedAt) {
		t.Errorf("timeOrNil(%v) = %v", self.StartedAt, got)
	}
	res, err := c.GetStatus(context.Background(), connect.NewRequest(&diagv1.GetStatusRequest{}))
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if res.Msg.GetSelf().StartedAt != nil {
		t.Errorf("started_at = %v for a zero StartedAt, want unset", res.Msg.GetSelf().GetStartedAt())
	}
}

func TestListRoutesIsTheRoutingTable(t *testing.T) {
	reg := registry.New()
	c := serve(t, NewServer(reg, self, newLog()))
	mustRegister(t, reg, registry.Instance{Service: "payments", Port: 50001})
	mustRegister(t, reg, registry.Instance{Service: "billing", Port: 50002})

	res, err := c.ListRoutes(context.Background(), connect.NewRequest(&diagv1.ListRoutesRequest{}))
	if err != nil {
		t.Fatalf("ListRoutes: %v", err)
	}
	routes := res.Msg.GetRoutes()
	if len(routes) != 2 {
		t.Fatalf("routes = %v, want two", routes)
	}
	if routes[0].GetAuthority() != "billing.internal" || routes[0].GetService() != "billing" || routes[0].GetUpstream() != "127.0.0.1:50002" {
		t.Errorf("routes[0] = %v", routes[0])
	}
	if routes[1].GetAuthority() != "payments.internal" || routes[1].GetService() != "payments" || routes[1].GetUpstream() != "127.0.0.1:50001" {
		t.Errorf("routes[1] = %v", routes[1])
	}
}

// TestListRequestsReadsTheBufferOldestFirst is #150's reader: everything the
// buffer holds, in order, field for field; the newest limit of them when a
// limit is set; and an empty list from an empty buffer.
func TestListRequestsReadsTheBufferOldestFirst(t *testing.T) {
	l := newLog()
	c := serve(t, NewServer(registry.New(), self, l))

	res, err := c.ListRequests(context.Background(), connect.NewRequest(&diagv1.ListRequestsRequest{}))
	if err != nil {
		t.Fatalf("ListRequests on an empty buffer: %v", err)
	}
	if n := len(res.Msg.GetRequests()); n != 0 {
		t.Errorf("an empty buffer lists %d requests", n)
	}

	for i := range 5 {
		l.Record(record(i))
	}
	for _, tc := range []struct {
		limit uint32
		want  []int
	}{
		{0, []int{0, 1, 2, 3, 4}},
		{2, []int{3, 4}},
		{99, []int{0, 1, 2, 3, 4}},
	} {
		res, err := c.ListRequests(context.Background(), connect.NewRequest(&diagv1.ListRequestsRequest{Limit: tc.limit}))
		if err != nil {
			t.Fatalf("ListRequests(limit %d): %v", tc.limit, err)
		}
		got := res.Msg.GetRequests()
		if len(got) != len(tc.want) {
			t.Fatalf("ListRequests(limit %d) = %d requests, want %d", tc.limit, len(got), len(tc.want))
		}
		for i, n := range tc.want {
			want := record(n)
			back := requestlog.FromProto(got[i])
			if !back.At.Equal(want.At) || back.Authority != want.Authority || back.Method != want.Method ||
				back.Path != want.Path || back.Status != want.Status || back.Duration != want.Duration ||
				back.UpstreamPort != want.UpstreamPort {
				t.Errorf("ListRequests(limit %d)[%d] = %+v, want %+v", tc.limit, i, back, want)
			}
		}
	}
}

// TestWatchRequestsStreamsWhatFollowsTheCall: the follower gets each request
// recorded after its call, in order and with every field, and none of the
// history — that is ListRequests's.
func TestWatchRequestsStreamsWhatFollowsTheCall(t *testing.T) {
	l := newLog()
	l.Record(record(0)) // history
	s := NewServer(registry.New(), self, l)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A follower whose arrival at the handler is observable, the way
	// [watching] does it, so the records below are provably after the call.
	in := make(chan struct{})
	mux := s.Handler()
	c := serveWith(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == diagv1connect.DiagServiceWatchRequestsProcedure {
			close(in)
		}
		mux.ServeHTTP(w, r)
	}))
	received := make(chan requestlog.Request, 64)
	ended := make(chan error, 1)
	go func() {
		stream, err := c.WatchRequests(ctx, connect.NewRequest(&diagv1.WatchRequestsRequest{}))
		if err != nil {
			ended <- err
			return
		}
		for stream.Receive() {
			received <- requestlog.FromProto(stream.Msg().GetRequest())
		}
		ended <- stream.Err()
	}()
	select {
	case <-in:
	case <-time.After(10 * time.Second):
		t.Fatal("the watch never reached the handler")
	}
	// The handler is running but may not have registered its watcher yet,
	// and a record made before it did is history to the follower. So probe:
	// record until one arrives — that is the setup, asserted — and only then
	// record the two this test counts.
	probes := 0
	attached := false
	for deadline := time.Now().Add(10 * time.Second); !attached && time.Now().Before(deadline); {
		probes++
		l.Record(requestlog.Request{Path: "/probe/" + strconv.Itoa(probes)})
		select {
		case r := <-received:
			if !strings.HasPrefix(r.Path, "/probe/") {
				t.Fatalf("the follower received %+v before any probe", r)
			}
			attached = true
		case err := <-ended:
			t.Fatalf("the follower's stream ended during the setup: %v", err)
		case <-time.After(time.Millisecond):
		}
	}
	if !attached {
		t.Fatal("the follower never received a probe; the watcher never attached")
	}
	l.Record(record(1))
	l.Record(record(2))
	var counted []requestlog.Request
	for len(counted) < 2 {
		select {
		case r := <-received:
			if strings.HasPrefix(r.Path, "/probe/") {
				continue // a probe recorded after the first one arrived
			}
			counted = append(counted, r)
		case err := <-ended:
			t.Fatalf("the follower's stream ended before both records arrived: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatalf("the follower received %d of the two records", len(counted))
		}
	}
	if counted[0].Path != "/n/1" || counted[1].Path != "/n/2" {
		t.Fatalf("streamed %+v, want records 1 and 2 in order", counted)
	}
	want, back := record(2), counted[1]
	if !back.At.Equal(want.At) || back.Authority != want.Authority || back.Method != want.Method ||
		back.Status != want.Status || back.Duration != want.Duration || back.UpstreamPort != want.UpstreamPort {
		t.Errorf("streamed record = %+v, want %+v", back, want)
	}
	// No history: the record made before the call never arrives. Everything
	// the follower saw was a probe or one of the two.
	select {
	case r := <-received:
		if !strings.HasPrefix(r.Path, "/probe/") {
			t.Errorf("the follower received %+v, which was recorded before it called", r)
		}
	default:
	}
	// Done following; the end is our own.
	cancel()
	select {
	case err := <-ended:
		if connect.CodeOf(err) != connect.CodeCanceled {
			t.Errorf("the follower's stream ended with %v, want its own cancellation", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the follower's call never returned after cancel")
	}
}

// TestWithoutABufferTheRequestRPCsSaySo: a daemon started with
// `request_log = false` keeps no buffer, and both readers answer
// failed_precondition naming the setting — never an empty list a developer
// would read as "no traffic".
func TestWithoutABufferTheRequestRPCsSaySo(t *testing.T) {
	c := serve(t, NewServer(registry.New(), self, nil))
	_, err := c.ListRequests(context.Background(), connect.NewRequest(&diagv1.ListRequestsRequest{}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "request_log = false") {
		t.Errorf("ListRequests with no buffer: %v, want failed_precondition naming request_log = false", err)
	}
	stream, err := c.WatchRequests(context.Background(), connect.NewRequest(&diagv1.WatchRequestsRequest{}))
	if err == nil {
		for stream.Receive() {
		}
		err = stream.Err()
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "request_log = false") {
		t.Errorf("WatchRequests with no buffer: %v, want failed_precondition naming request_log = false", err)
	}
}

// watching opens a WatchRequests stream and reports two things a client
// cannot see from its end: that the request reached the handler, and that
// the handler returned — the fact "ends cleanly" is about, since a stream
// that ends because the client cancelled reads the same to the client whether
// the handler returned or is leaked. Both come from middleware around the
// mux.
//
// What the follower itself sees is asserted too, on the way out: wantCode is
// the code its stream ends with, which is how a `voya log -f` tells its own
// ^C from the daemon stopping under it (#159).
//
// The call itself runs in a goroutine, because connect-go's server-streaming
// client blocks in the call until the response headers arrive, and the
// handler sends those with its first message or its return — for a stream
// that streams nothing, that is its end. The daemon-side comment on
// WatchRequests records the same for #159.
func watching(t *testing.T, s *Server, ctx context.Context, wantCode connect.Code) (handlerDone <-chan struct{}) {
	t.Helper()
	in, out := make(chan struct{}), make(chan struct{})
	mux := s.Handler()
	c := serveWith(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == diagv1connect.DiagServiceWatchRequestsProcedure {
			close(in)
			defer close(out)
		}
		mux.ServeHTTP(w, r)
	}))
	// Handed back over the channel and asserted here, never from the
	// goroutine (CLAUDE.md).
	type followed struct {
		streamed int
		err      error
	}
	returned := make(chan followed, 1)
	go func() {
		var f followed
		stream, err := c.WatchRequests(ctx, connect.NewRequest(&diagv1.WatchRequestsRequest{}))
		if err != nil {
			// connect-go's server-streaming client can surface the end of a
			// stream that carried no message from the call itself rather than
			// from Receive; either way it is the same end.
			f.err = err
		} else {
			for stream.Receive() {
				f.streamed++
			}
			f.err = stream.Err()
		}
		returned <- f
	}()
	t.Cleanup(func() {
		select {
		case f := <-returned:
			if f.streamed != 0 {
				t.Errorf("WatchRequests streamed %d requests with no buffer behind it", f.streamed)
			}
			if got := connect.CodeOf(f.err); got != wantCode {
				t.Errorf("the follower's stream ended with %v (%v), want %v", got, f.err, wantCode)
			}
		case <-time.After(10 * time.Second):
			t.Error("the follower's call never returned")
		}
	})
	select {
	case <-in:
	case <-time.After(10 * time.Second):
		t.Fatal("the watch never reached the handler")
	}
	return out
}

// TestWatchRequestsEndsWhenTheClientCancels: a `voya log -f` that hits ^C
// hands its stream back, and the handler goes with it rather than leaking a
// goroutine per follower.
func TestWatchRequestsEndsWhenTheClientCancels(t *testing.T) {
	s := NewServer(registry.New(), self, newLog())
	ctx, cancel := context.WithCancel(context.Background())
	done := watching(t, s, ctx, connect.CodeCanceled)

	select {
	case <-done:
		t.Fatal("the handler returned before the client cancelled — it is not holding the stream open")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler did not return after the client cancelled")
	}
}

// TestWatchRequestsDropsAFollowerThatFellBehind is the third of the three ends
// WatchRequests has, and the one that had no test: a follower that let a whole
// buffer's worth pile up unread is dropped rather than waited on — the writer
// is the proxy handler, and no follower gets to slow a request — and the
// handler says so with resource_exhausted, naming ListRequests as where the
// last records are.
//
// Which end it is comes from [requestlog.Watcher.Lagged] and not from the
// channel having closed (#163): the flag is the log's own record of what it
// did, and this test is what fails if the handler stops reading it — a close
// without the flag now ends the stream with nothing, which is not this code.
//
// A buffer one record deep, so the drop needs the recorder to outpace the
// handler exactly once rather than a volume of traffic; records go in until
// the stream ends, because when that outpacing happens is the scheduler's to
// decide and not a number this test may assume.
func TestWatchRequestsDropsAFollowerThatFellBehind(t *testing.T) {
	l := requestlog.New(1)
	c := serve(t, NewServer(registry.New(), self, l))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handed back over the channel, never asserted from the goroutine
	// (CLAUDE.md). The call itself blocks until the first message, so the
	// stream is not open until the first record below lands.
	ended := make(chan error, 1)
	go func() {
		stream, err := c.WatchRequests(ctx, connect.NewRequest(&diagv1.WatchRequestsRequest{}))
		if err != nil {
			ended <- err
			return
		}
		for stream.Receive() {
		}
		ended <- stream.Err()
	}()

	for i := 0; ; i++ {
		select {
		case err := <-ended:
			if got := connect.CodeOf(err); got != connect.CodeResourceExhausted {
				t.Fatalf("the follow of a dropped follower ended with %v (%v), want resource_exhausted", got, err)
			}
			if !strings.Contains(err.Error(), "ListRequests") {
				t.Errorf("the drop said %q, want it to name ListRequests as where the last records are", err)
			}
			return
		default:
		}
		if i > 1_000_000 {
			t.Fatalf("%d records into a one-record buffer never dropped the follower: "+
				"the log kept a follower that read none of them", i)
		}
		l.Record(record(i))
	}
}

// TestCloseEndsEveryWatcher: the daemon stopping ends a follower's stream
// before the proxy's drain starts, so a `voya log -f` cannot hold voya's
// shutdown to its grace — and the follower is told which of the two ends it
// is, unavailable rather than the clean end its own ^C produces. A stream
// that ended with no error would have `voya log -f` exit 0 without a word
// while the daemon it was following stops.
func TestCloseEndsEveryWatcher(t *testing.T) {
	s := NewServer(registry.New(), self, newLog())
	done := watching(t, s, context.Background(), connect.CodeUnavailable)

	select {
	case <-done:
		t.Fatal("the handler returned before Close — it is not holding the stream open")
	case <-time.After(50 * time.Millisecond):
	}
	s.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler did not return after Close")
	}
	s.Close() // idempotent: the daemon's Close is, and calls this from it
}

// TestGetStatusIsInstant is ADR 005's "must stay instant": the median of N
// calls over the wire against a full registry — every name the seen list
// holds, the eviction history at its cap — is well under the deadline
// `voya status` will dial with. The median, because a wait in the code
// narrows the spread and a busy machine widens it, so only the median is
// about the code (CLAUDE.md).
//
// The registry is filled to its own caps rather than to constants copied
// from it: names are registered until Known() stops growing, evictions until
// Evictions() does, so a change to either cap moves this test with it.
func TestGetStatusIsInstant(t *testing.T) {
	reg := registry.New()
	c := serve(t, NewServer(reg, self, newLog()))

	// Every service is live and every name is remembered: the seen list's
	// cap is when Known() stops growing.
	names := 0
	for known := -1; known != len(reg.Known()); names++ {
		known = len(reg.Known())
		mustRegister(t, reg, registry.Instance{Service: "svc-" + strconv.Itoa(names), Port: uint32(20000 + names)})
	}
	// And the eviction history at its cap: depose one name until the
	// history stops growing.
	for evictions := -1; evictions != len(reg.Evictions()); {
		evictions = len(reg.Evictions())
		mustRegister(t, reg, registry.Instance{Service: "svc-0", Port: uint32(30000 + evictions)})
	}
	if len(reg.Services()) < 100 || len(reg.Evictions()) < 32 {
		t.Fatalf("the registry is not full: %d services, %d evictions", len(reg.Services()), len(reg.Evictions()))
	}

	const calls = 50
	durations := make([]time.Duration, 0, calls)
	for range calls {
		start := time.Now()
		res, err := c.GetStatus(context.Background(), connect.NewRequest(&diagv1.GetStatusRequest{}))
		durations = append(durations, time.Since(start))
		if err != nil {
			t.Fatalf("GetStatus: %v", err)
		}
		if len(res.Msg.GetServices()) != len(reg.Services()) {
			t.Fatalf("GetStatus listed %d services of %d", len(res.Msg.GetServices()), len(reg.Services()))
		}
	}
	slices.Sort(durations)
	median := durations[calls/2]
	t.Logf("GetStatus over %d services and %d evictions: median %s, min %s, max %s",
		len(reg.Services()), len(reg.Evictions()), median, durations[0], durations[calls-1])
	// `voya status` will dial with a deadline on the order of a second; a
	// loopback call that copies a few hundred records is a few hundred
	// microseconds. 50ms is two orders under the one and two over the other.
	if limit := 50 * time.Millisecond; median > limit {
		t.Errorf("GetStatus median = %s, over %s — it is meant to be instant (ADR 005)", median, limit)
	}
}

// TestGetStatusOnAnEmptyRegistry: nothing registered is an answer, not an
// error — the fresh-daemon `voya status`.
func TestGetStatusOnAnEmptyRegistry(t *testing.T) {
	c := serve(t, NewServer(registry.New(), self, newLog()))
	res, err := c.GetStatus(context.Background(), connect.NewRequest(&diagv1.GetStatusRequest{}))
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if len(res.Msg.GetServices())+len(res.Msg.GetEvictions())+len(res.Msg.GetReclaims()) != 0 {
		t.Errorf("an empty registry answered %v", res.Msg)
	}
	if res.Msg.GetSelf().GetPid() != self.PID {
		t.Errorf("Self is missing on an empty registry: %v", res.Msg.GetSelf())
	}
}

// TestGetStatusIsOneSnapshot: the three halves of a status describe one
// instant. `voya status` runs against a registry that is being written to —
// the recompile loop is ADR 004's main workflow — and read a call at a time
// the answer tears: the services list from before an eviction beside the
// history from after it, which is voya reporting a port as serving traffic
// and, two lines down, as having lost the name.
//
// The invariant is the one this staging makes total: with one name flipping
// between two ports, whoever is live is the incoming instance of the newest
// eviction. It holds in every snapshot and in no torn read — which is what
// mutation-checking it against three separate reads shows.
func TestGetStatusIsOneSnapshot(t *testing.T) {
	reg := registry.New()
	c := serve(t, NewServer(reg, self, newLog()))

	// One name changing hands as fast as the registry will take it, for as
	// long as the reads run: a tearable read needs an eviction to land inside
	// one, so the writer keeps going until the reader has had its fill.
	stop, flipping := make(chan struct{}), make(chan int, 1)
	go func() {
		evictions := 0
		for ; ; evictions++ {
			select {
			case <-stop:
				flipping <- evictions
				return
			default:
			}
			// Alternating ports, so each registration displaces the other
			// rather than reading as one instance reconnecting.
			if _, err := reg.Register(registry.Instance{Service: "payments", Port: uint32(50001 + evictions%2)}); err != nil {
				flipping <- evictions // asserted below, on the test goroutine
				return
			}
		}
	}()

	const reads = 500
	torn := 0
	for range reads {
		res, err := c.GetStatus(context.Background(), connect.NewRequest(&diagv1.GetStatusRequest{}))
		if err != nil {
			close(stop)
			t.Fatalf("GetStatus: %v", err)
		}
		msg := res.Msg
		if len(msg.GetEvictions()) == 0 || len(msg.GetServices()) != 1 {
			continue // before the first eviction there is nothing to disagree
		}
		newest := msg.GetEvictions()[len(msg.GetEvictions())-1]
		if msg.GetServices()[0].GetPort() != newest.GetIncoming().GetPort() && torn < 3 {
			torn++
			t.Errorf("payments is live on port %d while the newest eviction hands the name to port %d: "+
				"the services list and the history are from different instants",
				msg.GetServices()[0].GetPort(), newest.GetIncoming().GetPort())
		}
	}
	close(stop)

	// The staging is asserted, not assumed: a writer that stopped at its
	// first error would leave this test reading a still registry and passing
	// for want of anything to catch.
	staged := <-flipping
	if staged < reads {
		t.Fatalf("the writer staged %d registrations under %d reads; there was little for a read to tear on", staged, reads)
	}
}
