// Package daemon is `voya run`: the three listeners, the registry behind
// them, and an orderly way to stop.
//
// Nothing here decides anything. The registry is #14, the registration
// stream #15, the proxy #17, the wildcard DNS ADR 002; this package binds
// them in the right order, reports what it bound, and takes them all down
// together. What it does own is failure at startup — a port already in use
// is the failure ADR 001 singles out, and the message for it is the first
// thing a developer sees when voya will not start.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	configv1 "github.com/trendvidia/voya/proto/voya/config/v1"

	"github.com/trendvidia/voya/internal/auth"
	"github.com/trendvidia/voya/internal/buildinfo"
	"github.com/trendvidia/voya/internal/config"
	"github.com/trendvidia/voya/internal/diag"
	"github.com/trendvidia/voya/internal/dns"
	"github.com/trendvidia/voya/internal/lsof"
	"github.com/trendvidia/voya/internal/proxy"
	"github.com/trendvidia/voya/internal/registration"
	"github.com/trendvidia/voya/internal/registry"
	"github.com/trendvidia/voya/internal/requestlog"
)

// Daemon is a running voya.
type Daemon struct {
	cfg  *configv1.Config
	reg  *registry.Registry
	log  *sink
	diag *diag.Server

	proxySrv    *http.Server
	registerSrv *http.Server
	dnsSrv      *dns.Server

	proxyLn    net.Listener
	registerLn net.Listener
	dnsLn      *dns.Listener

	cancel context.CancelFunc
	wg     sync.WaitGroup
	errs   chan error
	stop   sync.Once

	// inflight is how many requests are inside the proxy handler right now.
	// [Daemon.Close] reads it twice: with [Daemon.proxyWire], to decide
	// whether Shutdown's drain has anything to protect at all (#94), and at
	// the deadline, to tell the two reasons the deadline expires apart (#72)
	// — net/http cannot, and Close owes the developer the right one.
	//
	// Handler entry to handler return, which is narrower than the window
	// Shutdown itself waits on: net/http keeps a connection active until the
	// response is flushed and the rest of the request body is drained, and a
	// request may be arriving before anything reaches the handler. For the
	// deadline message that makes this count a floor, not the whole of what
	// is live; for the gate, [Daemon.proxyWire] is what covers the far end
	// on both protocols, and this count is read beside it because it is the
	// floor the deadline already keeps and reading it costs nothing: where
	// the two disagree, the gate errs toward the drain.
	inflight atomic.Int64

	// proxyWire counts the proxy connections net/http itself reports
	// non-idle. The other half of what [Daemon.Close]'s gate reads; see
	// [connActivity] for what it covers on each protocol and for the one
	// tail it leaves.
	proxyWire connActivity

	// stopping is set at the top of [Daemon.Close], before anything is
	// drained or closed, and read by the proxy through [proxy.WithStopping]
	// for every request the shutdown goes on to cut: those are recorded as
	// voya's cut ([proxy.StatusClosedByShutdown]) and not as a caller that
	// left (#200). It is a flag and not the run context because Close closes
	// the proxy server while that context is still live — see the option's
	// doc for the measurement.
	stopping atomic.Bool
}

// connActivity is a ConnState-derived count of the connections that are
// non-idle right now: marked StateActive by net/http and not yet idle again,
// hijacked, or closed. That span outlives the handler occupancy
// [Daemon.inflight] counts on the far end — a connection stays active until
// its response is flushed *and* what remains of its request body is drained,
// which lasts as long as the caller takes to finish sending it — and that
// tail is what this count exists to keep off the fast path:
// TestAnExchangeStillDrainingKeepsCloseOnTheDrainPath cuts a real response
// off with the handler count alone.
//
// StateNew connections are deliberately not counted, and they are more than
// freshly-accepted sockets: net/http books a connection active only once the
// whole header read returns, so a request half-arrived when Close samples
// reads as new here. Nothing is lost in that: net/http itself refuses a
// request whose read completes after Shutdown has begun, so there is nothing
// on such a connection either path could save — it is the arrival window
// #94's decision accepts.
//
// What it sees of h2c is per stream, and it comes from a different place
// than the HTTP/1.1 transitions. maybeServeUnencryptedHTTP2 marks such a
// connection StateActive once, with the hooks skipped — the finding #72 is
// about — but that mark is net/http's own connection state, the one
// Shutdown polls and the reason an attached h2c caller holds Shutdown to
// its deadline; the h2 serve loop then drives this hook itself, StateActive
// when a connection's first stream opens and StateIdle when its last one
// closes (h2_bundle's newStream and closeStream). So an attached h2c caller
// with nothing running reads idle here, which is what lets Close skip the
// drain on #72's peer (TestCloseDoesNotWaitOutAnIdleCaller), and a stream is
// counted from its request HEADERS until its response's END_STREAM frame is
// written — after the handler has returned, the way the HTTP/1.1 drain is:
// TestAnH2cResponseHeldByFlowControlKeepsCloseOnTheDrainPath holds a
// finished response behind the caller's flow-control window, and the handler
// count alone cuts it (#130).
//
// What that leaves on the h2c leg is one hop. closeStream books the stream
// closed as its END_STREAM frame goes into the connection's buffered writer
// (4KB), and the write of that buffer to the socket is a goroutine the serve
// loop starts next — so up to a buffer of the response's last frames sits
// between this count reading zero and the bytes leaving the process. No
// caller-side pause reaches it: flow control holds a frame before it is
// written, never after, and holding the write itself would take a kernel
// send buffer full to within a frame at exactly that instant, which is not
// a thing a test can arrange — the attempt is recorded on #130. That hop is
// the third thing #94's decision accepts, and the narrowest of them.
type connActivity struct {
	mu     sync.Mutex
	active map[net.Conn]struct{}
}

// hook is installed as the proxy server's ConnState, and has to be assigned
// before Serve: a hook set while connections are already being accepted
// misses the transitions of the ones already in, and the count starts short.
func (a *connActivity) hook(conn net.Conn, state http.ConnState) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch state {
	case http.StateActive:
		if a.active == nil {
			a.active = make(map[net.Conn]struct{})
		}
		a.active[conn] = struct{}{}
	case http.StateIdle, http.StateHijacked, http.StateClosed:
		delete(a.active, conn)
	}
}

func (a *connActivity) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.active)
}

// countInFlight wraps the proxy handler so that [Daemon.inflight] is, at any
// moment, the number of requests inside it.
func (d *Daemon) countInFlight(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.inflight.Add(1)
		defer d.inflight.Add(-1)
		h.ServeHTTP(w, r)
	})
}

// Registry exposes the live registry to whatever runs in this process — the
// test suites, chiefly. Anything in a process of its own reads the same data
// through DiagService under voya.internal on the proxy port (#149), which is
// what the M2 commands do.
func (d *Daemon) Registry() *registry.Registry { return d.reg }

// Infof prints through the same sink the daemon's own messages go through, at
// LOG_LEVEL_INFO.
//
// It exists so that what `voya run` says around the daemon's life honours
// log_level as well: a developer who asked for LOG_LEVEL_ERROR and was still
// told the daemon is stopping would have the field half-read, which is the
// shape #43 was filed about.
func (d *Daemon) Infof(format string, args ...any) { d.log.at(info)(format, args...) }

// ProxyAddr, RegistrationAddr and DNSAddr report what was actually bound,
// which is what a test on ephemeral ports needs and what `voya run` prints.
func (d *Daemon) ProxyAddr() string        { return d.proxyLn.Addr().String() }
func (d *Daemon) RegistrationAddr() string { return d.registerLn.Addr().String() }
func (d *Daemon) DNSAddr() string          { return d.dnsLn.Addr().String() }

// Banner is the fragment of the startup line that means voya is serving:
// [Start] prints it last, after all three listeners are bound and their serve
// goroutines are running. That makes it the readiness signal for anything
// driving a real daemon — [Start] binds the proxy first, so a dial on the
// proxy port answers while the registration and DNS listeners are still
// unbound.
//
// One definition for the reason [AnnouncementMarker] has one (#57): it is
// matched by cmd/voya's harness and test/acceptance's, and a rewording here
// that left the matchers behind would fail both as a daemon that never came
// up, with nothing pointing at the format string.
const Banner = "voya: proxy on http://"

// Start binds all three listeners and begins serving.
//
// Binding happens before any of them serves, so a port conflict fails while
// nothing is half-running: voya either came up or it did not, and there is no
// state in between for a developer to reason about.
//
// ctx bounds startup, not the daemon's life — it is what a caller cancels to
// abandon a bind still in progress, and [Daemon.Close] is what stops a daemon
// that came up. The two are deliberately separate: what the listeners serve
// under is a context this package owns, because deriving it from ctx would
// let a caller's cancellation — the signal context, under `voya run` — end
// every in-flight request before Close got to offer them the shutdown grace.
//
// logf may be nil, which means "say nothing" — the guard is in [newSink], and
// [TestStartAcceptsANilLogf] is what pins it. Otherwise logf is the raw sink:
// it prints whatever it is handed, and cfg's log_level is what decides what it
// is handed.
func Start(ctx context.Context, cfg *configv1.Config, logf func(string, ...any)) (*Daemon, error) {
	// Load validates what it resolves, but Start is also reached with a
	// Config built in code — the acceptance suite does exactly that — and an
	// unvalidated one fails later and worse: a zero shutdown_grace makes
	// Close's deadline expire before it is set, and two listeners sharing a
	// port bind in an order nobody chose. It is also what [newSink] relies on
	// for log_level being a level at all.
	if err := config.Validate(cfg); err != nil {
		return nil, err
	}
	// The signing key is minted here, once, before anything binds or serves
	// (#219, ADR 006 amendment of 2026-08-19): voya is a local identity
	// provider, and a daemon that came up having failed to secure its own key
	// would be an IdP that silently is not one. So every LoadOrGenerate
	// failure — an unwritable directory, a file auth refuses, no resolvable
	// configuration directory at all — refuses the start; auth's errors
	// already name the file (or $VOYA_CONFIG_DIR) and the remedy. This call
	// decides when the key first exists, not who owns reading it: the JWKS
	// handler mounted below still loads per request, so a key deleted mid-run
	// is regenerated rather than served from a stale copy.
	configDir := config.Dir()
	if _, err := auth.LoadOrGenerate(configDir); err != nil {
		return nil, fmt.Errorf("voya: refusing to start without a usable signing key: %w", err)
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	log := newSink(cfg.GetLogLevel(), logf)
	warnf, errorf := log.at(warn), log.at(errorLevel)
	d := &Daemon{
		cfg: cfg,
		reg: registry.New(
			registry.WithEvictionHandler(evictionWarning(warnf)),
			registry.WithReclaimHandler(reclaimWarning(warnf)),
		),
		log:    log,
		cancel: cancel,
		errs:   make(chan error, 3),
	}

	proxyLn, err := proxy.Listen(ctx, config.Addr(cfg.GetProxyPort()))
	if err != nil {
		startErr := startupError(ctx, proxyListener, cfg.GetProxyPort(), err)
		cancel()
		return nil, startErr
	}
	d.proxyLn = proxyLn

	registerLn, err := registration.Listen(ctx, config.Addr(cfg.GetRegistrationPort()))
	if err != nil {
		startErr := startupError(ctx, registrationListener, cfg.GetRegistrationPort(), err)
		cancel()
		_ = proxyLn.Close()
		return nil, startErr
	}
	d.registerLn = registerLn

	dnsLn, err := dns.Listen(ctx, config.Addr(cfg.GetDnsPort()))
	if err != nil {
		startErr := startupError(ctx, dnsListener, cfg.GetDnsPort(), err)
		cancel()
		_ = proxyLn.Close()
		_ = registerLn.Close()
		return nil, startErr
	}
	d.dnsLn = dnsLn

	// The registration server hands every client the proxy port to dial, and
	// it is told the port that was bound rather than the one that was asked
	// for. Validation rules port 0 out, so today those are the same number;
	// reading it off the listener is what keeps it the port a service can
	// actually reach if that ever stops being true.
	//
	// The proxy and the DNS server each write one kind of message and nothing
	// else — a route the proxy could not serve, plus httputil's own failures
	// bridged onto the same function, and a reply the DNS server could not
	// send. Both are voya failing to do its job for one caller, so each is
	// handed a function that already is or is not printing, and neither
	// package has to learn what a level is (#43).
	//
	// DiagService rides the proxy port under the reserved voya.internal
	// authority (ADR 005, #149): the same listener, the same authority
	// routing, so a working `voya status` also proves the proxy routes the
	// reserved name. Every port in Self is read off its listener, for the
	// reason the registration server's is.
	//
	// And ADR 005's request log (#150). The buffer is `request_log`'s: on,
	// the proxy records every request it answers into a ring buffer of
	// [requestlog.Capacity] that ListRequests and WatchRequests read; off,
	// there is no buffer, nothing is recorded, and the two RPCs say so. The
	// live line is log_level's: each record is also printed through the sink
	// at INFO — the same rendering `voya log` prints, so the terminal and the
	// buffer read the same — and a level at or above WARN silences the line
	// without touching the buffer, because a developer who quietened the
	// terminal still wants "what happened to *this* request" answered.
	// [requestRecorder] is where that decision is made, once, at startup.
	var requests *requestlog.Log
	proxyOpts := []proxy.Option{proxy.WithErrorLog(errorf), proxy.WithStopping(d.stopping.Load)}
	// ADR 006's local rate limiter (#223): per authority, and only when a
	// layer turned it on — base.pxf ships it disabled, so an unconfigured
	// voya carries no limiter at all. Validation has already refused an
	// enabled limiter with zero requests or a non-positive window, so the
	// values handed over here are the ones the option's own guard accepts.
	proxyOpts = append(proxyOpts, proxy.WithCORSOrigins(cfg.GetCorsOrigins()))
	if rl := cfg.GetRateLimit(); rl.GetEnabled() {
		proxyOpts = append(proxyOpts, proxy.WithRateLimit(rl.GetRequests(), rl.GetWindow().AsDuration()))
	}
	if cfg.GetRequestLog() {
		requests = requestlog.New(requestlog.Capacity)
		proxyOpts = append(proxyOpts, proxy.WithRequestLog(requestRecorder(log, requests)))
	}
	d.diag = diag.NewServer(d.reg, diag.Self{
		PID:              pidOf(os.Getpid()),
		StartedAt:        time.Now(),
		ProxyPort:        portOf(proxyLn),
		DNSPort:          udpPortOf(dnsLn),
		RegistrationPort: portOf(registerLn),
		Version:          buildinfo.Version(),
	}, requests)
	// The reserved name also serves the JWKS document (#209), on the same
	// mux DiagService's procedures claim — the daemon is where the two
	// surfaces meet, so a proxy built without a daemon serves neither. The
	// handler reads the key per request from the configuration directory,
	// which is read once at the top of Start, the same way `voya token`
	// resolves it — and the key it reads is the one Start already minted.
	internal := d.diag.Handler()
	internal.Handle(auth.JWKSPath, auth.JWKSHandler(configDir))
	proxyOpts = append(proxyOpts, proxy.WithInternalHandler(internal))
	d.proxySrv = proxy.NewHTTPServer(runCtx, d.countInFlight(proxy.New(d.reg, proxyOpts...)))
	d.proxySrv.ConnState = d.proxyWire.hook
	d.registerSrv = registration.NewHTTPServer(runCtx,
		registration.NewServer(d.reg, portOf(proxyLn)))
	d.dnsSrv = dns.NewServer(dnsLn, errorf)

	// And the layer above those two: [proxy.WithErrorLog] reaches what the
	// proxy and httputil write, not what net/http itself does. An http.Server
	// with no ErrorLog writes its own failures to stderr through the standard
	// logger, which would leave them the one class of message log_level did
	// not reach — see [stdLogger].
	d.proxySrv.ErrorLog = stdLogger(errorf)
	d.registerSrv.ErrorLog = stdLogger(errorf)

	d.serve()
	log.at(info)(Banner+"%s, registration on %s, dns on %s",
		proxyLn.Addr(), registerLn.Addr(), dnsLn.Addr())
	return d, nil
}

// requestRecorder is what the proxy records every answered request through:
// the ring buffer, and the live line beside it only when the sink prints at
// [info].
//
// The tee is built once, here, rather than left to a discarding print
// function: above INFO that function throws its argument away, but the
// [requestlog.RecorderFunc] wrapping it still boxes a
// [requestlog.Request] into an interface for every request the proxy answers
// (#163). Cheap, and on the one path #150 promised to keep quiet — so at WARN
// and above the recorder is the buffer alone, which is also what
// [TestRequestRecorderIsTheBufferAloneAboveInfo] asserts by identity.
func requestRecorder(s *sink, buf *requestlog.Log) requestlog.Recorder {
	if !s.allows(info) {
		return buf
	}
	infof := s.at(info)
	return requestlog.Tee(buf, requestlog.RecorderFunc(func(r requestlog.Request) { infof("%s", r) }))
}

func (d *Daemon) serve() {
	d.wg.Add(3)
	go func() {
		defer d.wg.Done()
		if err := d.proxySrv.Serve(d.proxyLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			d.errs <- fmt.Errorf("proxy: %w", err)
		}
	}()
	go func() {
		defer d.wg.Done()
		if err := d.registerSrv.Serve(d.registerLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			d.errs <- fmt.Errorf("registration: %w", err)
		}
	}()
	go func() {
		defer d.wg.Done()
		if err := d.dnsSrv.Serve(); err != nil {
			d.errs <- fmt.Errorf("dns: %w", err)
		}
	}()
}

// Err delivers the first listener failure, if one happens. voya is a local
// single point of failure by design (ADR 001), so a listener dying has to
// surface rather than leave a process that is up and no longer working.
func (d *Daemon) Err() <-chan error { return d.errs }

// Close stops everything and waits.
//
// The order is the whole of it. The two servers get different treatment, and
// the difference is ADR 004's rather than a preference.
func (d *Daemon) Close() error {
	var err error
	d.stop.Do(func() {
		// First, before the watchers go and before the drain: from here on
		// every request the shutdown cuts is voya's doing, and the proxy
		// records it that way (#200).
		d.stopping.Store(true)

		grace := d.cfg.GetShutdownGrace().AsDuration()
		shutdownCtx, done := context.WithTimeout(context.Background(), grace)
		defer done()

		errs := make([]error, 0, 3)

		// The watchers first: a `voya log -f` holding WatchRequests open is a
		// request inside the proxy handler for as long as it stays, and it
		// would hold the drain below to its whole grace and then be counted as
		// work cut off — for a follower that lost nothing. Ending them here is
		// what keeps a follower from changing how voya stops (#149).
		d.diag.Close()

		// The proxy first, and while the serving context is still live. It
		// carries requests a caller is waiting on, so it gets the grace
		// period to finish them (#7 measures whether the number is right;
		// what matters here is that it is bounded). Cancelling before this
		// point would end those requests where they stand and make
		// shutdown_grace mean nothing — and the caller would not even get a
		// clean failure, but voya's "registered … but the connection failed:
		// context canceled" diagnostic, which describes a service that is
		// gone rather than a voya that is stopping.
		//
		// But only when there is something for the grace to protect. An
		// attached caller's pooled h2c connection never reads as idle to
		// Shutdown, so it waits its whole deadline on a peer with nothing
		// running — which was #72, first answered with an inbound idle
		// timer that turned out to cut HTTP/1.1 keep-alive callers'
		// requests on the data path (#94). Now nothing reaps anyone: when
		// net/http reports no connection non-idle and no request is inside
		// the handler, there is nothing a drain could finish, and Close goes
		// straight to the close it would otherwise reach at the deadline.
		// proxyWire is what holds the far end on both protocols — an
		// HTTP/1.1 exchange until its response is flushed and its request
		// body drained, an h2c stream until its response's END_STREAM frame
		// is written, both after the handler count reads zero — and inflight
		// is read beside it because it is the floor the deadline below
		// already keeps. A request that arrives between these reads and the
		// Close is cut without the grace; that window is a shutdown already
		// in progress, and accepting it is the tradeoff #94 records.
		if d.proxyWire.count() == 0 && d.inflight.Load() == 0 {
			if e := d.proxySrv.Close(); e != nil {
				errs = append(errs, fmt.Errorf("proxy: %w", e))
			}
		} else if e := d.proxySrv.Shutdown(shutdownCtx); e != nil {
			if errors.Is(e, context.DeadlineExceeded) {
				// The bound did its job, but reaching it does not mean work
				// was cut off. Shutdown also waits for attached callers whose
				// pooled h2c connections never read as idle, and a peer that
				// was already idle when Shutdown's GOAWAY reached it lets go
				// only when net/http forces it, about a second later — so a
				// grace shorter than that, spent behind such a caller, ends
				// here with nothing in flight at all (#72). The counter is
				// what tells the two apart, and both are correct shutdowns,
				// reported rather than returned as failures: a request that
				// outlived the grace is the [warn], because a caller lost
				// work; callers that had not let go of a voya that is
				// leaving anyway lost nothing, and claiming "busy" for them
				// was #72's bug.
				//
				// Both sides name the handler and not the wire, for the
				// reason the field's own doc gives: a request still arriving
				// and a response still draining both sit outside this count,
				// so either can be live at the deadline while it reads zero.
				// "Nothing in flight" over one of those would be #72's
				// misdiagnosis inverted — the same overclaim, in the
				// direction that reads as an all-clear. The warn side is the
				// same gap understated rather than inverted, since the count
				// is a floor on what is live, and a number announced as the
				// whole of it would be the one a developer counts losses by.
				if n := d.inflight.Load(); n > 0 {
					d.log.at(warn)("voya: proxy still busy after the %s shutdown grace (%d in the proxy handler); closing anyway", grace, n)
				} else {
					d.log.at(info)("voya: callers still attached to the proxy after the %s shutdown grace, with no request in the proxy handler; closing their connections", grace)
				}
			} else {
				errs = append(errs, fmt.Errorf("proxy: %w", e))
			}
			_ = d.proxySrv.Close()
		}

		// Now the serving context goes. This is what makes the rest
		// terminate: every registration handler blocks until its stream
		// ends, so an http.Server carrying them never has an idle connection
		// for Shutdown to wait on.
		d.cancel()

		// The registration server is closed outright. Its streams are meant
		// to break — ADR 004 makes a voya restart a non-event precisely
		// because every stream drops and the services re-register — and
		// waiting for them to go idle cannot work: the clients are still
		// running, so each one reconnects within its backoff and opens a new
		// stream on the connection Shutdown is waiting to see go quiet. It
		// burned the whole grace period and then reported a deadline every
		// time, for a shutdown that was correct.
		if e := d.registerSrv.Close(); e != nil {
			errs = append(errs, fmt.Errorf("registration: %w", e))
		}
		if e := d.dnsSrv.Close(); e != nil {
			errs = append(errs, fmt.Errorf("dns: %w", e))
		}
		d.wg.Wait()
		err = errors.Join(errs...)
	})
	return err
}

// AnnouncementMarker is the whole of what makes an announcement an
// announcement.
//
// ADR 002 obliges an eviction and a reclaim to reach voya's terminal
// *prominently*, and this is the only thing separating them from an ordinary
// log line: a blank line, then two bangs. [evictionWarning] and
// [reclaimWarning] write it, [Transcript.Announced] matches it, and nothing
// else in the repo spells it out (#57).
//
// The single definition is the point. It used to be a bare literal in four
// places — the two warnings and one matcher in each of the two suites that
// assert on them — so rewording it here left both suites matching nothing,
// and neither said the marker had moved: one failed as "the reclaim was not
// announced in voya's own log", the other as a twenty-second timeout waiting
// for voya to announce the eviction. Both pointed at the wiring and neither
// at the format string.
//
// It lives beside the two functions that write it rather than beside the
// capture that reads it, because it is part of what voya prints and
// [Transcript] is scaffolding a test builds around that. Giving up the two
// literals also gave up the way they cross-checked each other, so what a
// rewording is measured against is now written down as a test:
// TestTheAnnouncementMarkerIsProminent.
const AnnouncementMarker = "\n!! "

// announcementFormat is how both warnings are written: the marker, the
// announcement itself, and a newline that cmd/voya's logf — which appends one
// of its own — renders as the blank line closing the block off.
const announcementFormat = AnnouncementMarker + "%s\n"

// evictionWarning is ADR 002's "loudly". The registry records the eviction
// for `voya status`; this is the part that reaches a terminal, and it has to
// read as an event rather than a log line — the failure it prevents is twenty
// minutes spent debugging a build that stopped receiving traffic.
//
// Written at [warn], which is where the config schema puts it: LOG_LEVEL_WARN
// is documented as "failures plus the events ADR 002 requires be impossible to
// miss". So `log_level = LOG_LEVEL_ERROR` does silence this, and that is the
// setting meaning what it says rather than an oversight — ERROR that still
// printed warnings would be a synonym for WARN, and the schema's "verbosity
// floor" would not be true of the one level most likely to be set for quiet.
// The obligation is met at the default and at every level above ERROR.
// The marker that makes it prominent is [AnnouncementMarker], declared just
// above with the reasoning for it having one definition.
func evictionWarning(logf func(string, ...any)) func(registry.Eviction) {
	return func(e registry.Eviction) {
		logf(announcementFormat, e)
	}
}

// reclaimWarning is the other half of the same obligation (#27). A deposed
// instance has taken its name back because whoever replaced it went away, and
// traffic moved without anybody asking.
//
// It is announced exactly as loudly as the eviction, and that symmetry is the
// point: the developer replaced that build *because it was wrong*, and a
// silent return to it is ADR 002's twenty minutes of confusion arrived at
// from the other side. The service announces this in its own log too; this is
// the copy in the terminal where voya is running, which is where somebody
// watching the fleet is looking.
func reclaimWarning(logf func(string, ...any)) func(registry.Reclaim) {
	return func(rc registry.Reclaim) {
		logf(announcementFormat, rc)
	}
}

func portOf(ln net.Listener) uint32 {
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok && tcp.Port > 0 && tcp.Port <= 65535 {
		return uint32(tcp.Port)
	}
	return 0
}

// udpPortOf is [portOf] for the DNS listener, whose address is a UDP one —
// its TCP socket is bound to the same number, which [dns.Listen] guarantees.
func udpPortOf(ln *dns.Listener) uint32 {
	if udp, ok := ln.Addr().(*net.UDPAddr); ok && udp.Port > 0 && udp.Port <= 65535 {
		return uint32(udp.Port)
	}
	return 0
}

// pidOf narrows os.Getpid to the int32 the schema carries. A pid outside
// that range is not a thing any supported OS hands out; reported as 0 —
// "none" on the wire — rather than wrapped into a number that names some
// other process.
func pidOf(pid int) int32 {
	if pid < 0 || pid > 1<<31-1 {
		return 0
	}
	return int32(pid)
}

// listener names one of voya's three for a startup message: what a developer
// calls it, and the transports it occupies — which lsof has to be told, since
// asking about TCP finds nothing at all on the UDP port DNS binds. DNS binds
// both (#184), on one number: it is one listener with two sockets, and both
// have to be free for it to start.
type listener struct {
	name     string
	networks []lsof.Network
}

var (
	proxyListener        = listener{name: "proxy", networks: []lsof.Network{lsof.TCP}}
	registrationListener = listener{name: "registration", networks: []lsof.Network{lsof.TCP}}
	dnsListener          = listener{name: "dns", networks: []lsof.Network{lsof.UDP, lsof.TCP}}
)

// ErrPortInUse marks a [Start] failure caused by another process holding one
// of voya's three ports.
//
// What a developer reads is the message below, and ADR 001 means that to be
// prose — free to be reworded whenever a better wording is found. This is
// what code matches on instead, so that rewording cannot silently change the
// behaviour of anything that classifies the failure.
var ErrPortInUse = errors.New("a port voya needs is already in use")

// portInUseError is that message with [ErrPortInUse] and the errno
// underneath, so errors.Is finds either without the message having to carry
// them: appending "bind: address already in use" to a diagnostic that already
// says which port and what holds it would only tell the developer again, in
// worse words.
type portInUseError struct {
	message string
	cause   error
}

func (e *portInUseError) Error() string   { return e.message }
func (e *portInUseError) Unwrap() []error { return []error{ErrPortInUse, e.cause} }

// startupError turns a bind failure into the message ADR 001 asks for: name
// the port, and where possible the process holding it.
func startupError(ctx context.Context, l listener, port uint32, err error) error {
	if !isAddrInUse(err) {
		return fmt.Errorf("%s listener: %w", l.name, err)
	}
	message := fmt.Sprintf("cannot start: port %d (%s) is already in use", port, l.name)
	if holder := holderOf(ctx, l, port); holder != "" {
		message += "\n  held by: " + holder
	} else {
		message += "\n  nothing could be learned about what holds it"
	}
	message += "\n  voya needs this port; stop the other process, or change it in your voya.pxf"
	return &portInUseError{message: message, cause: err}
}

// holderOf asks lsof about port on each transport l binds, and names the
// first holder it finds. A listener on two transports fails to bind when
// either is taken, and the bind error does not say which — so both are asked.
func holderOf(ctx context.Context, l listener, port uint32) string {
	for _, network := range l.networks {
		if holder := lsof.Holder(ctx, network, port); holder != "" {
			return holder
		}
	}
	return ""
}

func isAddrInUse(err error) bool {
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	// Windows answers with WSAEADDRINUSE, a different errno that cannot be
	// named in portable code, so that one is matched on its message.
	return strings.Contains(err.Error(), "Only one usage of each socket address")
}
