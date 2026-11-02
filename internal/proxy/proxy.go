// Package proxy is voya's data plane: an h2c reverse proxy on 127.0.0.1:9777
// that routes on the :authority / Host header to the ephemeral port the named
// service registered.
//
// It proxies bytes. No descriptors, no payload parsing, no protocol
// translation (ADR 003) — Connect, gRPC and gRPC-Web pass through untouched,
// and the one header it rewrites is the one httputil destroys on its way out.
//
// Most of the shape here, and nearly all of the reasoning about the
// diagnostic, comes from the M0 spike in test/roundtrip, which exists to have
// made these mistakes already.
package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/trendvidia/voya/internal/registry"
	"github.com/trendvidia/voya/internal/requestlog"
	"github.com/trendvidia/voya/proto/voya/diag/v1/diagv1connect"
)

// upstreamKey carries the resolved upstream from the routing decision to the
// rewrite hook, which has no way to report an error of its own.
type upstreamKey struct{}

// spendKey carries the [spend] the limiter charged a forwarded request, from
// the gate that charged it to the ErrorHandler that may refund it (#233).
type spendKey struct{}

// spend is one admitted request's charge against a service's budget: the
// routed name it was keyed on and the window token [limiter.allow] reported,
// which is everything [limiter.refund] needs to hand it back, plus whether
// the request ever reached an upstream, which is what says a refund is owed.
//
// reached is set from the httptrace the gate attaches to the forwarded
// request, off the transport's goroutine, so it is atomic.
type spend struct {
	service  string
	admitted time.Time
	reached  *atomic.Bool
}

// Option configures a proxy.
type Option func(*proxy)

// WithCORS controls the CORS headers ADR 003 asks for in local dev. On by
// default; the option exists so a test can prove the headers are voya's rather
// than the backend's.
func WithCORS(enabled bool) Option {
	return func(p *proxy) { p.cors = enabled }
}

// WithCORSOrigins names origins allowed beyond the local ones
// [proxy.originAllowed] always accepts: the config's cors_origins, which
// validation has already held to scheme://host[:port].
func WithCORSOrigins(origins []string) Option {
	return func(p *proxy) {
		for _, origin := range origins {
			p.corsOrigins[strings.ToLower(origin)] = true
		}
	}
}

// WithErrorLog directs the proxy's own transport errors somewhere. Defaults
// to discarding them: the failure the developer needs is written into the
// response (ADR 005), and httputil's default logger would put a duplicate on
// stderr for every unroutable request.
//
// It covers httputil's own messages too, not only the ones this package
// writes. An option that redirected half the output and left the rest on
// stderr would not be worth having.
//
// A nil logf means the default: [New] makes p.logf unconditionally callable,
// and an option that could undo that would turn the first message the proxy
// writes into a panic — on ErrorHandler's path, or on the unmapped-outcome
// fallback whose whole purpose is to be loud rather than to crash (#128).
func WithErrorLog(logf func(string, ...any)) Option {
	return func(p *proxy) {
		if logf == nil {
			logf = func(string, ...any) {}
		}
		p.logf = logf
	}
}

// WithIdleUpstreamTimeout sets how long this proxy keeps a pooled connection
// to a service that has stopped talking to it. The default is
// [IdleUpstreamTimeout], and that constant is where the number's reasoning
// lives — the option changes who can set the value, not where it is argued
// (#73). It exists so a test can set its own value and measure the reap
// directly rather than bound the constant, which is a proxy for the property.
// Not a *small* value in particular: a reap can only be observed after the
// value it was given, so what a test gets from the option is a value it knows
// the default is not — [TestWithIdleUpstreamTimeoutGovernsTheReap] uses twice
// the default, and a value below it would leave that test's lower bound
// satisfied by any loopback call at all.
//
// Zero disables the reap outright — http.Transport's own convention — which
// is the pre-#52 behaviour and hands back the second that issue measured.
// Pinned by [TestWithIdleUpstreamTimeoutZeroDisablesTheReap], because it is
// the one setting the obvious tidy-up would break: zero also reads as "unset",
// and defaulting it back to [IdleUpstreamTimeout] after the options run is a
// change no other test in the package notices.
func WithIdleUpstreamTimeout(d time.Duration) Option {
	return func(p *proxy) { p.idleUpstream = d }
}

// InternalRouter is what the proxy hands a request under the reserved
// voya.internal authority to: voya's own diagnostics (ADR 005, #149) and,
// since #209, the JWKS document. It is the one method of *http.ServeMux the
// proxy needs — which pattern, if any, claims the request — because the
// proxy has to know the difference between a path voya serves and one it
// does not. A claimed path is served by the handler; an unclaimed one still
// answers from the reserved row of ADR 005's table, so `GET
// voya.internal:9777/` and a Connect call to a procedure DiagService does
// not have keep reading 501 / 12 / unimplemented, and the table keeps its
// row.
type InternalRouter interface {
	Handler(r *http.Request) (h http.Handler, pattern string)
}

// WithInternalHandler serves voya.internal from rt — the seam the daemon
// mounts DiagService through. Nil, the default, serves nothing there: every
// request under the reserved name answers from the reserved row, which is
// what a proxy built without a daemon does.
func WithInternalHandler(rt InternalRouter) Option {
	return func(p *proxy) { p.internal = rt }
}

// WithRequestLog records every request the proxy answers — routed, or
// answered from a diagnostic row — to rec, once, on the way out: ADR 005's
// request log (#150). What is recorded is [requestlog.Request], metadata
// only; the proxy reads no payload to fill it (ADR 003), and
// TestTheRequestLogCarriesNoPayloadByte holds it to that. Nil, the default,
// records nothing — a proxy built without a daemon, or a daemon whose
// `request_log = false`.
//
// The one exception is a request DiagService itself answered under the
// reserved name: voya reading itself — `voya log`, `voya status` — is not
// what happened to the developer's request, and left in it would fill the
// buffer with the reads of the command that reads it. A request under the
// reserved name that DiagService does not serve is answered from the
// reserved row, and recorded like every other diagnostic.
func WithRequestLog(rec requestlog.Recorder) Option {
	return func(p *proxy) { p.requests = rec }
}

// WithStopping tells the proxy how to learn that the process carrying it is
// on its way out. It decides what a request cut off mid-flight is recorded
// as: the caller's own leaving ([StatusClientClosedRequest]) or voya's
// ([StatusClosedByShutdown]). Nothing else reads it (#200).
//
// A predicate and not a context, and the difference is measured rather than
// stylistic. internal/daemon's Close closes the proxy's http.Server *before*
// it cancels the run context — the drain has to happen while that context is
// live, or shutdown_grace means nothing — and net/http cancels a request's
// context when it closes the connection under it. So at the moment the
// ErrorHandler runs for a request voya cut, the run context reads nil, and a
// check keyed on it leaves every such request recorded as the caller's. What
// the daemon knows at that moment is that it is stopping, and this is how it
// says so. Nil means never stopping, which is [New]'s default.
func WithStopping(stopping func() bool) Option {
	return func(p *proxy) {
		if stopping != nil {
			p.stopping = stopping
		}
	}
}

type proxy struct {
	reg          *registry.Registry
	reverse      *httputil.ReverseProxy
	cors         bool
	corsOrigins  map[string]bool
	logf         func(string, ...any)
	idleUpstream time.Duration
	internal     InternalRouter
	requests     requestlog.Recorder
	stopping     func() bool
	limiter      *limiter
}

// New returns the routing handler, backed by reg.
func New(reg *registry.Registry, opts ...Option) http.Handler {
	p := &proxy{reg: reg, cors: true, corsOrigins: map[string]bool{}, logf: func(string, ...any) {}, idleUpstream: IdleUpstreamTimeout,
		stopping: func() bool { return false }}
	for _, opt := range opts {
		opt(p)
	}

	p.reverse = &httputil.ReverseProxy{
		ErrorLog: p.errorLog(),
		// Flush every write through to the caller rather than at the end of
		// the response (#158). httputil already does that for a response it
		// recognises as streaming — text/event-stream, or no Content-Length,
		// which is every gRPC and Connect stream — and this covers the case
		// it does not recognise: a *fixed-length* response to a request whose
		// body is still open. A bidi caller sends nothing more until it has
		// been answered, so an early error to one — a backend's 404 to a
		// procedure it does not serve, and `voya call`'s reflection probe is
		// exactly that — reached the caller only when it gave up. It is the
		// answer that says stop, so waiting for the caller to stop first is a
		// deadlock: TestAnEarlyResponseReachesACallerStillSending times it.
		//
		// The cost is a flush per write on a hop that is loopback-only, which
		// is the trade a local emulator should always take: voya exists to
		// show a developer what their fleet does, and buffering shows them
		// something else.
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			upstream, _ := pr.In.Context().Value(upstreamKey{}).(string)
			pr.SetURL(&url.URL{Scheme: "http", Host: upstream})

			// The single most important line in the package. SetURL sets
			// Out.Host = "", which makes the outbound authority the
			// upstream's ephemeral address: the request still arrives, and
			// the name the whole design routes on is gone. Restoring it is
			// what keeps :authority end to end — which is what lets a
			// backend see the name it was called by, and what makes gRPC
			// server reflection work behind the proxy (ADR 002, ADR 003).
			pr.Out.Host = pr.In.Host

			// [ProbeHeader] is voya's own marker and stops at voya:
			// stripped unconditionally, not only when a limiter is
			// configured, so no fleet service ever learns it exists — an
			// upstream that learned to send it back would be surprising
			// (#233).
			pr.Out.Header.Del(ProbeHeader)

			// Not SetXForwarded: X-Forwarded-For and friends are an edge
			// concern, and voya is not the edge (ADR 006). Adding them here
			// would put headers in front of every local request that
			// production does not, which is the divergence this tool exists
			// to remove.
		},
		Transport: newUpstreamRoundTripper(p.idleUpstream),
		ModifyResponse: func(res *http.Response) error {
			// voya wrote its own CORS headers before proxying, and httputil
			// copies the upstream's response headers in with Add rather than
			// Set. A backend that carries its own CORS middleware — which a
			// browser-facing one must, to work on Fly — would otherwise
			// answer with two Access-Control-Allow-Origin values, and a
			// browser rejects a response carrying two outright. voya's
			// describe this hop, so the upstream's are the ones that go, and
			// for an origin voya refused they go too: a backend's own `*`
			// must not reopen what the allowlist closed.
			if !p.cors || res.Request == nil || res.Request.Header.Get("Origin") == "" {
				return nil
			}
			for name := range res.Header {
				if strings.HasPrefix(name, "Access-Control-") {
					res.Header.Del(name)
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// A request that reached nothing spends nothing (#233): the
			// gate charged the budget before the dial could be tried, and
			// this is where the failure is discovered, so this is where
			// the charge is handed back. An upstream that answered an
			// error, or accepted the connection and then broke it, was
			// reached and keeps its charge — that is exactly what the
			// limiter exists to protect, and refunding it would make the
			// limit unenforceable against a broken-but-listening service.
			//
			// Reached is read from whether the transport ever got a
			// connection, not from the error's shape: a dial that loses a
			// race with a cancelled request context is reported as the
			// cancellation — net/http prefers context.Cause to the dial
			// error it already holds (transport.go, getConn) — so an
			// errors.As for a dial *net.OpError keeps the charge for a
			// request that reached nothing, nondeterministically, which is
			// #233's symptom again. Read before the cancellation branch
			// below, because who is still waiting for the answer does not
			// change what the request cost.
			if s, ok := r.Context().Value(spendKey{}).(spend); ok && !s.reached.Load() {
				p.limiter.refund(s.service, s.admitted)
			}
			if r.Context().Err() != nil {
				// Nobody is left to read the answer, so this is not the
				// upstream's failure to report. Almost always that is the
				// caller having gone away while the service was still
				// working — a deadline it set ran out, a Ctrl-C'd curl, an
				// HTTP/2 stream reset — which is #169: the service never
				// failed, and a diagnostic saying it did went to the
				// daemon's terminal for a request nobody was waiting on.
				// Nothing on the log, then, and the request log records
				// that the client closed it rather than a 5xx laid at the
				// upstream's door. The status reaches no one — the
				// connection or stream that would carry it is what went
				// away — and exists for the record alone.
				//
				// The check reads which side cancelled and not what
				// failed, and one other side can cancel: voya itself. A
				// request still inside the handler when internal/daemon's
				// Close gives up on the shutdown grace is ended by
				// http.Server.Close closing the connection under it, and
				// net/http cancels the request context on the way — not
				// the run context the server's BaseContext carries, which
				// is still live at that moment, since the daemon cancels
				// it only after the proxy is closed. So which side it was
				// is read from [WithStopping], not from any context, and a
				// request voya cut is recorded as voya's cut rather than
				// as a caller that had not left (#200). Not diagnosed on
				// the log either way: Close says once for the run that a
				// request outlived the grace, and per request there is
				// nothing to add.
				//
				// What it costs: a service that accepts and then wedges is
				// proxied in silence to any caller with a deadline, the
				// h2c diagnostic below included. `voya health` is the
				// reader for that — its own bound expires and the row
				// reads `timeout` under the service's name — and the
				// record still carries the port it was routed to.
				if p.stopping() {
					w.WriteHeader(StatusClosedByShutdown)
					return
				}
				w.WriteHeader(StatusClientClosedRequest)
				return
			}
			// A gRPC call to a service that offers only HTTP/1.1. Nothing was
			// sent, and the fix is in the service.
			if errors.Is(err, errGRPCNeedsHTTP2) {
				p.logf("voya: proxying %q: %v", r.Host, err)
				p.diagnose(w, r, outcomeUnreachable, fmt.Sprintf(
					"voya: %s on %s offers only HTTP/1.1, and gRPC needs HTTP/2 end to end.\n"+
						"Serve HTTP/2 there, or call it with Connect or gRPC-Web.",
					quote(r.Host), r.URL.Host))
				return
			}
			// A registered service that will not accept a connection, or
			// accepts it and speaks neither protocol. Both causes are named:
			// the port is the developer's only handle on the first, and the
			// second looks like a framing error that reads as a voya bug.
			p.logf("voya: proxying %q: %v", r.Host, err)
			p.diagnose(w, r, outcomeUnreachable, fmt.Sprintf(
				"voya: %s is registered on %s but the connection failed: %v\n"+
					"Either that process is gone, or it is not serving HTTP/2 or HTTP/1.1 there, in cleartext or over TLS.",
				quote(r.Host), r.URL.Host, err))
		},
	}
	return p
}

// StatusClientClosedRequest is the request log's status for a proxied request
// whose caller went away before the upstream answered: nginx's 499, which is
// the one convention there is for it — no RFC assigns the code, and none of
// the assigned ones say "the client left". It is a record's status and never
// a wire one: by the time the proxy learns the caller is gone there is no
// caller to write to. It is not a routing outcome, so it has no row in ADR
// 005's table; the request was routed, and the upstream port on the record
// says where to.
//
// What distinguishes it from [StatusClosedByShutdown] is who cut the request:
// this is the caller's own leaving, recorded while voya was not stopping. A
// caller that leaves during the shutdown grace is recorded as the other one —
// the smaller and rarer error, since voya is leaving throughout that window.
const StatusClientClosedRequest = 499

// StatusClosedByShutdown is the request log's status for a proxied request
// that voya itself cut off: still in flight when the shutdown grace ran out,
// with the caller still attached, so the connection was closed under it and
// no answer was ever written (#200). nginx's 444, "connection closed without
// response", is the nearest convention to what literally happened, and it
// collides with no row of ADR 005's table — 502 and 503 there are routing
// outcomes, and this request was routed. Like 499 it is a record's status
// and never a wire one, and it names no failure of the upstream: the record
// still carries the port the request was routed to.
//
// It reaches almost no reader through `voya log`: the followers are closed
// first in the daemon's Close, and the daemon is on its way out. Its surface
// is the live line on `voya run`'s stderr at log_level=info, directly under
// the "proxy still busy after the … shutdown grace" line.
const StatusClosedByShutdown = 444

// errorLog is where httputil's own messages go. The ones this package writes
// go through p.logf; without this the rest — a protocol switch that failed, a
// response copy abandoned mid-stream — would still reach stderr through the
// standard logger, which is neither what [WithErrorLog] promises nor, by
// default, wanted at all.
func (p *proxy) errorLog() *log.Logger {
	return log.New(writerFunc(func(line []byte) {
		p.logf("%s", bytes.TrimRight(line, "\n"))
	}), "", 0)
}

// writerFunc adapts a log function to the io.Writer a log.Logger writes
// through.
type writerFunc func([]byte)

func (f writerFunc) Write(line []byte) (int, error) {
	f(line)
	return len(line), nil
}

// ServeHTTP answers one request, and records it if there is a request log.
//
// The record is taken on the way out, whichever path answered — one record
// per request, after the response is written, because that is when the
// status and the duration exist. The status is read off a wrapper around the
// ResponseWriter, which is the one thing net/http offers no other way to
// learn from inside a handler; the wrapper is a pass-through for everything
// else — see [statusWriter].
func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if p.requests == nil {
		p.serve(w, r)
		return
	}
	start := time.Now()
	sw := &statusWriter{ResponseWriter: w}
	upstreamPort, served := p.serve(sw, r)
	if served == byDiagService {
		// What earns the skip is a loop, and only DiagService's procedures
		// have one: a `voya log` whose ListRequests was recorded would write
		// to the buffer it reads, and a `voya log -f` would feed its own
		// terminal one line per line it prints. Not "voya reading itself" in
		// general — a claimed path that is not a procedure has no loop and
		// is recorded like anything else, a validator's GET /.well-known/jwks.json
		// included: #218 decided the log's completeness must not depend on
		// what callers claim about themselves, so leaving voya's own rows
		// out is the reader's choice, made by `voya log -no-voya`.
		return
	}
	p.requests.Record(requestlog.Request{
		At:        start,
		Authority: recordedAuthority(r),
		Method:    r.Method,
		// The path alone, never the query: a Connect GET carries its
		// message there, and that is payload.
		Path:         r.URL.Path,
		Status:       sw.status(),
		Duration:     time.Since(start),
		UpstreamPort: upstreamPort,
	})
}

// answeredBy is who wrote the response serve produced, for the request log's
// sake: voya itself, an upstream it forwarded to, or DiagService under the
// reserved name — the one kind the log leaves out.
type answeredBy int

const (
	byVoya answeredBy = iota
	byUpstream
	byDiagService
)

// serve is the routing decision and the answer. It reports the upstream port
// the request was forwarded to — 0 when voya answered it without forwarding —
// and who answered.
func (p *proxy) serve(w http.ResponseWriter, r *http.Request) (upstreamPort uint32, by answeredBy) {
	if p.cors {
		// Permissive to local origins, and local-dev only by construction:
		// this handler runs nowhere else. ADR 003 accepts that a genuine
		// CORS misconfiguration will not surface until deploy, in exchange
		// for browser frontends working without per-service middleware.
		p.writeCORS(w, r)
		if isPreflight(r) {
			// A preflight is asking voya about voya's policy, not about the
			// upstream's. Answering it here also means an OPTIONS for a
			// service that is not running still gets a usable answer, which
			// is what stops a browser reporting a CORS failure for what is
			// really a routing one.
			//
			// Only a preflight, though. A bare OPTIONS is a request to the
			// service — REST discovery, an Allow: advertisement — and
			// answering it here would make voya the one endpoint on the
			// machine that swallows it, which is the divergence from Fly
			// this tool exists to remove.
			w.WriteHeader(http.StatusNoContent)
			return 0, byVoya
		}
	}

	if r.Host == "" {
		// Nothing to route on. ADR 005: say so, rather than 502.
		p.diagnose(w, r, outcomeNoAuthority,
			"voya: the request arrived with no :authority (HTTP/2) or Host (HTTP/1) header.\n"+
				"voya routes by service name, so there is nothing to route on.")
		return 0, byVoya
	}

	service, ok := registry.ServiceFromAuthority(r.Host)
	if !ok {
		// Two different mistakes. A name that is not voya's at all wants to
		// be told what voya does route; a .internal name that is not shaped
		// like a service name does not — it just used the form the first
		// message would recommend. The client dialer captures every
		// *.internal host on this port, so the second arrives here rather
		// than at DNS, and gets to be told what is actually wrong with it.
		// One outcome for both: neither is a name registry.ValidName can
		// ever accept, so unlike a typo'd single label, no registration can
		// make the request as sent come good (#88). The third shape that
		// cannot — a single label made of characters no service may take —
		// gets past this branch and is caught below.
		var reason string
		if registry.IsInternal(r.Host) {
			reason = fmt.Sprintf(
				"voya: %s ends in %s but is not a service name voya routes.\n"+
					"A service name is a single label — payments%s, not api.payments%s.\n"+
					"Authority received: %s",
				quote(r.Host), quote(registry.Suffix), registry.Suffix, registry.Suffix, quote(r.Host))
		} else {
			reason = fmt.Sprintf(
				"voya: %s is not a name voya routes.\n"+
					"It routes %s names on this port — for example payments%s.\n"+
					"Authority received: %s",
				quote(r.Host), quote("*"+registry.Suffix), registry.Suffix, quote(r.Host))
		}
		p.diagnose(w, r, outcomeNotAName, reason)
		return 0, byVoya
	}

	if service == registry.Reserved {
		// ADR 005 keeps this name for voya's own programmatic diagnostics,
		// and since #149 the daemon serves DiagService there: a path the
		// router claims is voya answering about itself, through the same
		// authority routing every other name takes.
		if p.internal != nil {
			if h, pattern := p.internal.Handler(r); pattern != "" {
				h.ServeHTTP(w, r)
				if strings.HasPrefix(pattern, "/"+diagv1connect.DiagServiceName+"/") {
					return 0, byDiagService
				}
				// A claimed path that is not a DiagService procedure — the
				// JWKS document (#209) — is a developer's own request, not
				// voya reading itself, so it is recorded like every other
				// request voya answers. byVoya, because voya wrote the
				// response and no upstream was involved.
				return 0, byVoya
			}
		}
		// Anything else under the name — a procedure DiagService does not
		// have, a browser's GET / — is a request voya cannot answer and
		// nothing else ever will. Nothing can register the name — validName
		// refuses — so the "you typed it wrong" answer below would be
		// advice the developer cannot act on, given to the one person whose
		// routing is already broken enough to reach for voya's own endpoint.
		p.diagnose(w, r, outcomeReserved, fmt.Sprintf(
			"voya: %s is reserved for voya's own diagnostics and no service can register it (ADR 005).\n"+
				"It serves voya.diag.v1.DiagService and the JWKS document at /.well-known/jwks.json, and nothing else — %s %s is neither.\n"+
				"Authority received: %s",
			quote(registry.Authority(registry.Reserved)), r.Method, r.URL.Path, quote(r.Host)))
		return 0, byVoya
	}

	if err := registry.ValidName(service); err != nil {
		// A single label of the right shape that no service may ever take:
		// pay_ments.internal, -payments.internal, or a label past a DNS
		// label's 63 characters. ServiceFromAuthority passes it — one
		// label, right suffix — and registration refuses it forever, so
		// answering from the never-registered row below would tell a caller
		// with a retry policy to keep trying a request no registration can
		// end, which is #88's whole subject. The reason is the registry's
		// own, never a second copy of the rule.
		reason := "no service may register that name"
		var nameErr *registry.NameError
		if errors.As(err, &nameErr) {
			reason = nameErr.Reason
		}
		p.diagnose(w, r, outcomeNotAName, fmt.Sprintf(
			"voya: %s is not a name any service can register, so nothing can ever answer to it.\n"+
				"Registration refuses it: %s.\n"+
				"Authority received: %s",
			quote(service), reason, quote(r.Host)))
		return 0, byVoya
	}

	upstream, live := p.reg.Lookup(service)
	switch {
	case live:
		addr := net.JoinHostPort(loopback, strconv.FormatUint(uint64(upstream.Port), 10))
		if p.isSelf(r, addr) {
			// Forwarding here would route voya to voya, for as many hops as
			// the machine has file descriptors: the developer gets a hang
			// with no diagnostic at all, which is the one outcome ADR 005
			// rules out. A service reports the port it *bound* after asking
			// for 0 — reporting the port callers dial is how this happens.
			p.diagnose(w, r, outcomeRoutingLoop, fmt.Sprintf(
				"voya: service %s is registered on %s, which is voya's own proxy port.\n"+
					"A service registers the ephemeral port it actually bound after listening on :0, "+
					"not the port its callers dial. Routing this would send voya to itself.\n"+
					"Authority received: %s",
				quote(service), addr, quote(r.Host)))
			return 0, byVoya
		}
		ctx := context.WithValue(r.Context(), upstreamKey{}, addr)
		if p.limiter != nil && r.Header.Get(ProbeHeader) == "" {
			// The last gate before forwarding, deliberately: only a request
			// that would otherwise reach an upstream spends budget, because
			// the limiter exists to protect an upstream and every refusal
			// above never touches one — a typo cannot exhaust a name's
			// budget, and the reserved voya.internal returned before this
			// check existed (#223). voya's own health probes carry
			// [ProbeHeader] and skip the gate entirely (#233): a probe asks
			// after the service's state, and charging the service for the
			// question would make the answer wrong. The key is the routed
			// name, not r.Host, so both spellings of one service spend from
			// one budget.
			ok, wait, admitted := p.limiter.allow(service)
			if !ok {
				// The header is the entire point of the limiter (ADR 006):
				// a 429 without it gives the caller nothing to honour. Set
				// before diagnose so every response shape carries it.
				retry := retryAfterHeader(wait)
				w.Header().Set("Retry-After", retry)
				// The marker that makes this 429 voya's own rather than a
				// forwarded one — what the client wrapper keys its retry on
				// (#241) — is diagnose's to set, not set here: it qualifies
				// the 429 status itself, so only the shapes whose own status
				// line says 429 carry it (#251). Voya's refusal is still the
				// only marked one: the reverse proxy passes an upstream's
				// 429 through untouched.
				// The body names the same seconds the header carries: curl
				// without -i shows no headers at all, and ADR 005 makes the
				// body the diagnostic that has to stand on its own.
				p.diagnose(w, r, outcomeRateLimited, fmt.Sprintf(
					"voya: %s is over its rate limit of %d requests per %s (rate_limit, ADR 006).\n"+
						"The budget is per authority; retry in %ss, the wait the Retry-After header carries.\n"+
						"Authority received: %s",
					quote(registry.Authority(service)), p.limiter.requests, p.limiter.window, retry, quote(r.Host)))
				return 0, byVoya
			}
			// The spend rides the request so the ErrorHandler can hand it
			// back if the request reaches nothing: what reached nothing
			// costs nothing (#233). The trace is how "reached" is known —
			// the transport reports the connection it got, whether it
			// dialled one or took an idle one, and on HTTP/2 as much as on
			// HTTP/1 — and it rides the same context so the transport
			// finds it on the request the reverse proxy clones.
			sp := spend{service: service, admitted: admitted, reached: new(atomic.Bool)}
			ctx = context.WithValue(ctx, spendKey{}, sp)
			ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
				GotConn: func(httptrace.GotConnInfo) { sp.reached.Store(true) },
			})
		}
		p.reverse.ServeHTTP(w, r.WithContext(ctx))
		// Forwarded — including to a port that refused the connection, which
		// is the one diagnostic row that keeps its port: it names the process
		// that is gone.
		return upstream.Port, byUpstream

	case p.reg.Knows(service):
		// Known name, no instance. The developer's service is not the
		// problem; their other terminal is.
		p.diagnose(w, r, outcomeKnownNotRunning, fmt.Sprintf(
			"voya: service %s is known but has no running instance.\n"+
				"It registered earlier and its stream dropped, or it has not started yet.\n"+
				"Authority received: %s",
			quote(service), quote(r.Host)))

	default:
		// Never registered under any name voya has seen. Usually a typo.
		p.diagnose(w, r, outcomeNeverRegistered, fmt.Sprintf(
			"voya: no service named %s.\n"+
				"Nothing has ever registered that name.\n"+
				"Authority received: %s",
			quote(service), quote(r.Host)))
	}
	return 0, byVoya
}

// isSelf reports whether addr is the address this very request arrived on.
//
// The proxy is not told its own address — it is handed a listener — so it
// asks the connection, which net/http puts in every request's context. A
// request that did not come off a listener (a handler test) has none, and
// there is no loop to find.
func (p *proxy) isSelf(r *http.Request, addr string) bool {
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	return ok && local.String() == addr
}

// isPreflight reports whether a request is a CORS preflight rather than an
// ordinary OPTIONS the service itself should answer. The Fetch standard sends
// both headers on every preflight and neither on anything else.
func isPreflight(r *http.Request) bool {
	return r.Method == http.MethodOptions &&
		r.Header.Get("Origin") != "" &&
		r.Header.Get("Access-Control-Request-Method") != ""
}

// writeCORS answers a browser on an allowed origin permissively, and any other
// with no CORS headers, which is how a browser is told to withhold the
// response. The reflected origin plus credentials is what a local frontend on
// some other port needs; a wildcard origin would forbid credentials outright.
func (p *proxy) writeCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" || !p.originAllowed(origin) {
		return // not a browser request, or not one voya permits
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Allow-Credentials", "true")
	h.Set("Vary", "Origin")

	if requested := r.Header.Get("Access-Control-Request-Headers"); requested != "" {
		h.Set("Access-Control-Allow-Headers", requested)
	}
	if requested := r.Header.Get("Access-Control-Request-Method"); requested != "" {
		h.Set("Access-Control-Allow-Methods", requested)
	}
	// gRPC-Web reads its outcome from these, and a browser cannot see a
	// response header that is not exposed. Omitting them turns every
	// grpc-web call into a client-side error with no status. Retry-After
	// rides the same rule: it is not CORS-safelisted, so without it here a
	// browser caller reads the limiter's 429 and cannot read the wait —
	// which is the client-side handling the limiter exists to exercise
	// (ADR 006, #223). And [RateLimitedHeader] is the other half of that
	// signal — the wait, and whose refusal it is — so without it a browser
	// sees how long to wait but not whether the 429 is voya's own to wait
	// out (#241, #251).
	h.Set("Access-Control-Expose-Headers",
		"Grpc-Status, Grpc-Message, Grpc-Status-Details-Bin, Retry-After, "+RateLimitedHeader)
	h.Set("Access-Control-Max-Age", "86400")
}

// IdleUpstreamTimeout is how long the proxy keeps a pooled connection to a
// service that has stopped talking to it.
//
// It exists because voya was making every service it had ever routed to slow
// to shut down (#52). Go's HTTP/2 graceful shutdown sends GOAWAY and then
// waits about a second for the peer to close the connection before forcing
// it, and voya was permanently that peer: one proxied call left a pooled h2c
// connection that nothing ever reaped, so `http.Server.Shutdown` in a service
// went from 74µs to 1.049s and stayed there for the life of the process. That
// second is paid on every restart, which is the workflow ADR 004 is written
// around.
//
// Those two figures are #52's own harness. ADR 004's table measures the same
// pair on the acceptance harness and gets single-digit microseconds against
// 1.05s, which is the same finding on a different rig: the second is voya's
// and reproduces anywhere, the microseconds are the machine's. Compare the
// ratio between the records rather than the absolute numbers.
//
// The value is a dial between two measured costs, not a guess. Above it, a
// service that has been quiet at least this long shuts down in tens of
// microseconds instead of a second; below it, a service stopped sooner than
// this after its last call waits out the remainder. Against that, a reap
// costs the next call one loopback dial and HTTP/2 preface — 176µs cold
// against 59µs pooled, so 117µs, once per this-long of silence.
//
// It does not cut long-lived streams, which is the reason the timeout was
// left off in the first place. IdleConnTimeout reaps only connections the
// transport has booked back in as idle, and a connection carrying a stream is
// not one of them however silent the stream goes — measured against a stream
// held open and quiet for five times this value, and pinned by
// TestASilentStreamSurvivesTheIdleTimeout.
//
// It is the default, not the only setting: [WithIdleUpstreamTimeout] overrides
// it per proxy (#73). The number and its reasoning stay here either way.
const IdleUpstreamTimeout = 250 * time.Millisecond

// UpstreamTransport dials backends over cleartext HTTP/2.
//
// gRPC needs HTTP/2 on both legs and the backend leg has no TLS to negotiate
// over, so this is prior-knowledge h2c — from the standard library, not
// golang.org/x/net/http2 (issue #21). The proxy uses it for every upstream
// that [probeUpstream] does not find speaking HTTP/1.1.
//
// Exported because the daemon may want to share one, and because a test that
// builds its own client should be able to dial the way the proxy does. It
// always carries the default [IdleUpstreamTimeout], and a caller sharing it
// shares that default: [New] builds a transport per proxy — it always did,
// since this is a constructor rather than a package-level instance — and since
// #73 it builds one through [upstreamTransport], so [WithIdleUpstreamTimeout]
// has somewhere to land. Sharing this one is sharing a dial, never a setting.
func UpstreamTransport() *http.Transport {
	return upstreamTransport(IdleUpstreamTimeout)
}

// upstreamTransport is [UpstreamTransport] with the idle timeout a parameter,
// which is how [WithIdleUpstreamTimeout] reaches the transport [New] builds.
func upstreamTransport(idle time.Duration) *http.Transport {
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	return &http.Transport{
		DialContext: (&net.Dialer{}).DialContext,
		Protocols:   protocols,

		// No response header timeout: a backend may legitimately take hours
		// to send the first byte of a stream, and a deadline here would cut
		// it for no reason voya knows about. That argument covered the idle
		// timeout too until #52 measured it, and it was wrong there — see
		// [IdleUpstreamTimeout].
		IdleConnTimeout: idle,
	}
}

func quote(s string) string { return fmt.Sprintf("%q", s) }
