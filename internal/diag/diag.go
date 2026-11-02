// Package diag is voya describing itself: the DiagService the daemon serves
// under the reserved voya.internal authority on the proxy port (ADR 005,
// #149), so that `voya status`, `voya routes` and the rest read what voya
// knows instead of scraping CLI output.
//
// It reads memory and nothing else. GetStatus is the call a developer makes
// when something is already wrong, so it has to stay instant and infallible
// (ADR 005): every answer here is a copy out of the registry and a handful
// of fields the daemon set at startup, and no RPC dials, probes or waits on
// anything — health's Check calls run in the CLI process, which is
// why their report shape sits in voya.diag.v1 without a procedure serving it.
package diag

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/trendvidia/voya/internal/registry"
	"github.com/trendvidia/voya/internal/requestlog"
	diagv1 "github.com/trendvidia/voya/proto/voya/diag/v1"
	"github.com/trendvidia/voya/proto/voya/diag/v1/diagv1connect"
)

// Self is what the daemon knows about itself and hands the server once at
// startup: GetStatus's Self message, minus nothing.
type Self struct {
	PID              int32
	StartedAt        time.Time
	ProxyPort        uint32
	DNSPort          uint32
	RegistrationPort uint32
	// Version is what `voya version` prints — buildinfo.Version(), which
	// the daemon reads so that this package does not.
	Version string
}

// Server implements voya.diag.v1.DiagService over a registry and the
// request log.
type Server struct {
	reg  *registry.Registry
	self Self
	// requests is ADR 005's ring buffer, or nil when the daemon was
	// configured with `request_log = false` — in which case the two request
	// RPCs say so rather than answer an empty list a developer would read as
	// "no traffic" (#150).
	requests *requestlog.Log

	// done closes when [Server.Close] is called, and ends every WatchRequests
	// stream — see Close for why the daemon needs that.
	done chan struct{}
	stop sync.Once
}

// NewServer returns a DiagService reading reg and requests, answering Self
// from self. requests may be nil: the request log is off, and ListRequests
// and WatchRequests answer failed_precondition saying so.
func NewServer(reg *registry.Registry, self Self, requests *requestlog.Log) *Server {
	return &Server{reg: reg, self: self, requests: requests, done: make(chan struct{})}
}

// Handler mounts the service on a fresh mux claiming exactly DiagService's
// procedure paths, and nothing else — which is what the proxy's internal
// router asks for: a path the mux does not claim answers from the reserved
// row of ADR 005's table, so `GET voya.internal:9777/` and a call to a
// procedure the service does not have keep reading unimplemented, and the
// table keeps its row.
//
// The mux is the caller's to extend, and the daemon does extend it: it
// mounts the JWK Set at /.well-known/jwks.json before handing it to the
// proxy (#209, and ADR 005's 2026-08-19 amendment), so the router sees one
// claimed path more than this function put there. What holds either way is
// the negative — nothing claims a prefix here, so a path nobody mounted
// stays unclaimed and keeps the reserved row's answer.
//
// One connect handler, registered once per procedure rather than under its
// prefix: connect-go's own dispatch answers a plain 404 for an unknown path
// under the prefix, and a 404 is what the pre-#149 proxy was so careful not
// to answer under this name — connect-go reads it as unimplemented, but a
// browser reads "not found" for a name that exists. TestHandlerClaimsEveryProcedure
// holds this list to the schema.
func (s *Server) Handler(opts ...connect.HandlerOption) *http.ServeMux {
	_, h := diagv1connect.NewDiagServiceHandler(s, opts...)
	mux := http.NewServeMux()
	for _, procedure := range procedures {
		mux.Handle(procedure, h)
	}
	return mux
}

// procedures is every DiagService procedure path, from the generated
// constants. Enumerated here because the mux above has to be told each one;
// [TestHandlerClaimsEveryProcedure] fails when the schema has one this list
// does not.
var procedures = []string{
	diagv1connect.DiagServiceGetStatusProcedure,
	diagv1connect.DiagServiceListRoutesProcedure,
	diagv1connect.DiagServiceListRequestsProcedure,
	diagv1connect.DiagServiceWatchRequestsProcedure,
}

// Close ends every WatchRequests stream, and is idempotent.
//
// The daemon calls it first thing on its own Close: a follower's stream is a
// request inside the proxy handler for as long as the client stays, and
// left running it would hold the proxy's shutdown drain to its whole grace
// and then be reported as work cut off — for a `voya log -f` that lost
// nothing. Ending the watchers before the drain starts is what keeps a
// follower from changing how voya stops.
func (s *Server) Close() {
	s.stop.Do(func() { close(s.done) })
}

// GetStatus is what `voya status` prints. Reads memory only.
//
// One [registry.Snapshot] rather than three reads, so the three halves of a
// status cannot disagree: read separately, an eviction landing between two of
// them would put an instance in the services list and the record deposing it
// in the history of the same answer.
func (s *Server) GetStatus(context.Context, *connect.Request[diagv1.GetStatusRequest]) (*connect.Response[diagv1.GetStatusResponse], error) {
	snap := s.reg.Snapshot()
	services := make([]*diagv1.Service, 0, len(snap.Services))
	for _, up := range snap.Services {
		services = append(services, service(up))
	}
	evictions := make([]*diagv1.Eviction, 0, len(snap.Evictions))
	for _, ev := range snap.Evictions {
		evictions = append(evictions, &diagv1.Eviction{
			Evicted:  instance(ev.Evicted),
			Incoming: instance(ev.Incoming),
			At:       timestamppb.New(ev.At),
		})
	}
	reclaims := make([]*diagv1.Reclaim, 0, len(snap.Reclaims))
	for _, rc := range snap.Reclaims {
		reclaims = append(reclaims, &diagv1.Reclaim{
			Instance:  instance(rc.Instance),
			EvictedAt: timeOrNil(rc.EvictedAt),
			At:        timestamppb.New(rc.At),
		})
	}
	return connect.NewResponse(&diagv1.GetStatusResponse{
		Self: &diagv1.Self{
			Pid:              s.self.PID,
			StartedAt:        timeOrNil(s.self.StartedAt),
			ProxyPort:        s.self.ProxyPort,
			DnsPort:          s.self.DNSPort,
			RegistrationPort: s.self.RegistrationPort,
			Version:          s.self.Version,
		},
		Services:  services,
		Evictions: evictions,
		Reclaims:  reclaims,
	}), nil
}

// ListRoutes is the routing table: authority -> service -> upstream, one row
// per live registration, in the registry's name order.
func (s *Server) ListRoutes(context.Context, *connect.Request[diagv1.ListRoutesRequest]) (*connect.Response[diagv1.ListRoutesResponse], error) {
	live := s.reg.Services()
	routes := make([]*diagv1.Route, 0, len(live))
	for _, up := range live {
		routes = append(routes, &diagv1.Route{
			Authority: registry.Authority(up.Service),
			Service:   up.Service,
			Upstream:  upstreamAddr(up.Port),
		})
	}
	return connect.NewResponse(&diagv1.ListRoutesResponse{Routes: routes}), nil
}

// errRequestLogOff is what both request RPCs answer when the daemon keeps
// no buffer. failed_precondition, because it is the daemon's configuration
// and not the request that is wrong: `voya log` renders it as the one line
// naming the setting.
var errRequestLogOff = connect.NewError(connect.CodeFailedPrecondition,
	errors.New("the request log is off: voya was started with request_log = false"))

// ListRequests reads the request ring buffer, oldest first — everything it
// holds, or the newest limit records when limit is set (#150). A copy out of
// memory; it never blocks on the proxy.
func (s *Server) ListRequests(_ context.Context, req *connect.Request[diagv1.ListRequestsRequest]) (*connect.Response[diagv1.ListRequestsResponse], error) {
	if s.requests == nil {
		return nil, errRequestLogOff
	}
	limit := int(min(req.Msg.GetLimit(), requestlog.Capacity))
	recs := s.requests.Snapshot(limit)
	out := make([]*diagv1.Request, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Proto())
	}
	return connect.NewResponse(&diagv1.ListRequestsResponse{Requests: out}), nil
}

// WatchRequests streams each request as it is recorded, from the moment of
// the call, until the client goes away or the server closes. It never
// replays the buffer — a follower that wants history calls ListRequests
// first, which is what `voya log -f` does (#159).
//
// Three ways it ends, told apart by the code, because `voya log -f` has to
// say which. The follower's own cancellation ends it with nothing: connect
// reports the cancellation, and a code from here would only argue with it.
// The daemon closing ends it with unavailable, so a shutdown does not read
// to the follower exactly like its own ^C — the one thing it must never do
// is print nothing and exit 0 while the daemon it was following stops. And
// a follower that stopped reading and let a whole buffer's worth of records
// pile up is ended with resource_exhausted rather than waited on: the writer
// is the proxy handler, and no follower gets to slow a request (see
// [requestlog.Log.Watch]).
//
// One thing for the client to know: connect-go sends a server stream's
// response headers with its first message, and its client blocks in the
// call until they arrive — so `voya log -f` blocks in WatchRequests itself,
// not in Receive, until the first request is recorded or the stream ends.
// There is no primitive to flush the headers early, and an empty first
// message would put a "stream is open" meaning into the schema that
// nothing asked for.
func (s *Server) WatchRequests(ctx context.Context, _ *connect.Request[diagv1.WatchRequestsRequest], stream *connect.ServerStream[diagv1.WatchRequestsResponse]) error {
	if s.requests == nil {
		return errRequestLogOff
	}
	w := s.requests.Watch()
	defer w.Close()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.done:
			return connect.NewError(connect.CodeUnavailable,
				errors.New("voya is shutting down; the request log ends here"))
		case r, ok := <-w.C():
			if !ok {
				// The log ended this watcher, and [requestlog.Watcher.Lagged]
				// is what says why: the closed channel alone would have this
				// handler assert the drop rather than read it, and the flag is
				// the log's own record of what it did (#163). Nothing else
				// closes the channel while the handler holds the watcher — the
				// deferred Close runs after this returns — so a close without
				// the flag would be the log having grown a second way to end a
				// follower, and this ends the stream without naming a cause it
				// no longer knows.
				if !w.Lagged() {
					return nil
				}
				return connect.NewError(connect.CodeResourceExhausted,
					errors.New("the follower fell a whole buffer behind the request log and was dropped; ListRequests has the last records"))
			}
			if err := stream.Send(&diagv1.WatchRequestsResponse{Request: r.Proto()}); err != nil {
				return err
			}
		}
	}
}

// service renders one live registration. The stream state is the one value
// the registry can mean: presence is liveness, so a service in Services() is
// attached (ADR 004).
func service(up registry.Upstream) *diagv1.Service {
	return &diagv1.Service{
		Name:      up.Service,
		Authority: registry.Authority(up.Service),
		Port:      up.Port,
		Pid:       up.PID,
		Since:     timestamppb.New(up.Since),
		Stream:    diagv1.StreamState_STREAM_STATE_ATTACHED,
	}
}

func instance(inst registry.Instance) *diagv1.Instance {
	return &diagv1.Instance{Name: inst.Service, Port: inst.Port, Pid: inst.PID}
}

// timeOrNil leaves an unset time unset on the wire rather than rendering
// the zero time as 0001-01-01: a reclaim that followed no recorded eviction
// has no evicted_at, and a reader should see none.
func timeOrNil(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// upstreamAddr is where the proxy sends a request for a service on port:
// the loopback literal every listener binds and the proxy dials (ADR 002).
func upstreamAddr(port uint32) string {
	return net.JoinHostPort("127.0.0.1", strconv.FormatUint(uint64(port), 10))
}
