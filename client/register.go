package voya

import (
	"context"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"connectrpc.com/connect"

	registryv1 "github.com/trendvidia/voya/proto/voya/registry/v1"
	"github.com/trendvidia/voya/proto/voya/registry/v1/registryv1connect"
)

// registrar owns one service's registration for the life of the process:
// open the stream, hold it, and reopen it when it breaks.
type registrar struct {
	service string
	port    uint32
	o       *options
	client  registryv1connect.RegistrationServiceClient

	// deposedAt is when this instance lost the name, for the reclaim
	// announcement. Written and read only by run's goroutine.
	deposedAt time.Time

	// routable records whether voya was still sending requests here when the
	// last session ended — which is the only case settle has anything to wait
	// out.
	//
	// Written only by run's goroutine, and read only by settle, which Join
	// calls after that goroutine has returned. The close of Join's done
	// channel is the happens-before edge between the two, so this needs no
	// synchronisation of its own; anything that reads it from elsewhere does.
	routable bool

	// transport is this registrar's own, not a shared one — see newRegistrar —
	// which is what makes release safe.
	transport *http.Transport
}

func newRegistrar(service string, port uint32, o *options) *registrar {
	// A dedicated client, not http.DefaultClient. The registration stream is
	// bidirectional Connect and therefore HTTP/2 only, over cleartext
	// loopback — prior-knowledge h2c, from the standard library (issue #21).
	// Keeping it separate also means a service that swaps its own transport
	// cannot break its registration.
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)

	transport := &http.Transport{Protocols: protocols}
	base := "http://" + net.JoinHostPort(proxyHost, strconv.FormatUint(uint64(o.registrationPort), 10))
	return &registrar{
		service:   service,
		port:      port,
		o:         o,
		transport: transport,
		client: registryv1connect.NewRegistrationServiceClient(
			&http.Client{Transport: transport}, base),
	}
}

// run keeps the registration alive until ctx ends or this instance is
// evicted.
//
// ADR 004's self-healing restart lives here: when voya goes away every
// stream breaks, and each service reopens its own with the same ephemeral
// port it already holds. Nothing is persisted anywhere, and nothing has to
// be told to re-register.
func (r *registrar) run(ctx context.Context) {
	delay := r.o.minBackoff
	// Only report trouble on the way into it. A service running with no voya
	// is a supported state, not an emergency, and one line per retry for
	// hours is how a developer learns to ignore the log.
	reported := false

	for {
		started := time.Now()
		res := r.session(ctx, ordinaryClaim)

		switch {
		case ctx.Err() != nil:
			// Deliberate shutdown; the stream ending is the deregistration.
			return
		case res.evicted:
			// #27, resolved: do not reopen the ordinary way. Re-registering
			// here is what turns two live instances into a pair that depose
			// each other forever. What happens instead is a bounded series
			// of *careful* claims — see reclaim.
			last, reclaimed := r.reclaim(ctx)
			if !reclaimed {
				return
			}
			if ctx.Err() != nil {
				// The same deliberate shutdown the guard above returns on,
				// arrived at one branch later (#56). reclaim returns on
				// cancellation with the name held, so a stop landing while
				// this instance is serving a name it took back comes back
				// here with the cancellation in last.err — and every line
				// below would then report the shutdown the developer asked
				// for as voya having gone away.
				//
				// Re-checked rather than folded into the report, because
				// what is stale is the guard above and not that report: the
				// reclaim window is minutes long, and nothing else in this
				// loop blocks for anywhere near as long between the check
				// and the use.
				return
			}
			// The name came back and has since been given up again — for an
			// ordinary reason, since being deposed a second time does not
			// come back here. From here this is an ordinary registration
			// with an ordinary outage, so fall through to the usual backoff
			// reading the session that just ended rather than the eviction
			// that started all this.
			res = last
			delay = r.o.minBackoff
		case res.permanent:
			// voya was reached and said no. Reconnecting re-sends the same
			// registration, so this is a loop that cannot terminate, and the
			// "cannot reach voya" line below would blame the wrong thing.
			r.o.logf("voya: refused to register %s (%v); it will not be reachable by name until that is fixed and the service restarts",
				r.service, res.err)
			return
		}

		if res.registered {
			// voya was reachable and the name was ours, so the next outage
			// is news again rather than more of the one already reported.
			reported = false
		}
		if res.err != nil && !reported {
			r.o.logf("voya: cannot reach voya on port %d (%v); %s is not reachable by name until it comes back",
				r.o.registrationPort, res.err, r.service+Suffix)
			reported = true
		}

		// A stream that lasted a while means voya is healthy and something
		// else ended it, so the next attempt should be prompt rather than
		// inheriting the delay from an outage an hour ago.
		if time.Since(started) >= r.o.maxBackoff {
			delay = r.o.minBackoff
		}

		select {
		case <-time.After(jitter(delay)):
		case <-ctx.Done():
			return
		}
		delay = min(delay*2, r.o.maxBackoff)
	}
}

// settle holds the route open after the stream has been torn down, so that
// voya has stopped sending requests here before the caller closes its
// listener (#7).
//
// A sleep and not a wait on anything: once the stream is closed there is
// nothing left to observe voya through, which is what makes the grace a
// duration rather than a handshake. [DefaultShutdownGrace] carries the
// measurement that sets it.
//
// Called only after run's goroutine has returned — see the routable field
// for why that is what makes this safe to read.
func (r *registrar) settle() {
	if r.o.shutdownGrace <= 0 || !r.routable {
		return
	}
	time.Sleep(r.o.shutdownGrace)
}

// release drops the connection the registration stream ran on.
//
// The transport belongs to this registrar alone (see newRegistrar), and both
// halves of the stream are closed by the time run's goroutine returns, so by
// the time stop calls this there is nothing left using it. Without it the
// pooled h2c connection to voya and the read goroutine behind it outlive every
// Join for the life of the process — which a service pays once and does not
// notice, and a test that joins and stops hundreds of times pays hundreds of
// times.
//
// Best-effort by construction: CloseIdleConnections skips a connection the
// transport has not yet booked back in as idle, and a leftover there is
// reclaimed when the process exits, exactly as it was before.
func (r *registrar) release() { r.transport.CloseIdleConnections() }

// sessionResult is how one stream ended. Every field is something run has to
// decide differently on: whether to reopen at all, and whether the developer
// has already been told what they are about to be told.
type sessionResult struct {
	// registered is set once voya acknowledged this instance. It says the
	// daemon was reachable and the name was ours, which is what makes a
	// later failure a new outage rather than the same one continuing.
	registered bool

	// evicted is set when another instance took the name (#27).
	evicted bool

	// permanent is set when voya rejected the registration itself, which no
	// number of retries will change.
	permanent bool

	err error
}

// reclaim asks for the name back, for a bounded while, without ever taking it
// from whoever holds it.
//
// The workflow this exists for: a developer replaces a running service to try
// a fix, the replacement turns out to be worse, they kill it — and the
// original, which still holds its state and its debugger session, goes back
// to serving without being restarted.
//
// Two properties make that safe rather than a slower version of the bug #27
// describes. Every attempt sets OnlyIfUnclaimed, so a claim against a live
// holder loses instead of deposing it; and the whole thing is bounded, so a
// process forgotten overnight does not take a name back the next morning.
//
// Returns the session that ended the reclaim and whether the name was ever
// taken back, so the caller can resume the ordinary loop rather than exiting
// — and can report that session's outcome rather than the eviction's.
func (r *registrar) reclaim(ctx context.Context) (sessionResult, bool) {
	if r.o.reclaimWindow <= 0 {
		return sessionResult{}, false // reclaiming is off; the eviction is final
	}
	deadline := time.Now().Add(r.o.reclaimWindow)
	r.deposedAt = time.Now()
	r.o.logf("voya: %s was taken over; asking for it back every %s for the next %s, and not taking it from whoever has it",
		r.service+Suffix, r.o.reclaimInterval, r.o.reclaimWindow)

	// Claim first, then wait. Asking immediately costs one refused round trip
	// — whoever just deposed this instance is alive by definition — and buys
	// a loop whose every attempt is inside the window by construction:
	// waiting first would spend an interval longer than what is left of the
	// window on one late claim, which is exactly what the bound exists to
	// prevent.
	for time.Now().Before(deadline) {
		// session blocks for as long as the claim holds, so a successful
		// reclaim is announced from inside it — by the time this returns,
		// the name has been held and given up again.
		res := r.session(ctx, carefulClaim)
		switch {
		case ctx.Err() != nil:
			return res, res.registered
		case res.evicted:
			// Deposed again — either while asking, or after the name had
			// come back and this session was holding it. Give up rather than
			// stack another window on top of this one, and in particular do
			// not report success: the caller's ordinary loop would re-register
			// unconditionally and depose whoever is serving now, which is #27
			// arrived at the long way round. Checked before res.registered
			// because a session can only be evicted after it registered, so
			// the other order makes this branch unreachable.
			return res, false
		case res.registered:
			return res, true
		}
		// Refused because someone still holds it, or voya is unreachable:
		// quiet on purpose, since that is the expected case for most of the
		// window.

		select {
		case <-time.After(jitter(r.o.reclaimInterval)):
		case <-ctx.Done():
			return sessionResult{}, false
		}
	}
	r.o.logf("voya: gave up asking for %s back after %s; restart this service if you want it to serve again",
		r.service+Suffix, r.o.reclaimWindow)
	return sessionResult{}, false
}

// The two kinds of registration attempt, named because session(ctx, true)
// says nothing about which one of them is the careful one.
const (
	ordinaryClaim = false // last-writer-wins: take the name from whoever holds it
	carefulClaim  = true  // OnlyIfUnclaimed: lose to whoever holds it (#27)
)

// session opens one stream and holds it until it ends. A careful session asks
// voya to refuse rather than evict, which is how a deposed instance retries
// without deposing anybody (#27).
func (r *registrar) session(ctx context.Context, careful bool) sessionResult {
	// Nothing is routed here until this session has asked. Cleared up front
	// rather than only on the way out, so that a stop arriving before the
	// registration goes out does not inherit the previous session's answer.
	r.routable = false
	defer func() {
		// A session that ended while the context was still live ended by
		// itself: the stream broke, or voya went away. Either way voya has
		// already stopped routing here, and settle has nothing left to wait
		// for. Only a session torn down by stop leaves this set.
		//
		// The context is the only signal available here, and it is a racy one:
		// a stop landing while a session is in its own teardown makes this
		// branch miss, and the grace gets paid for a route that is already
		// gone. That way round costs one wait; the other drops a request. The
		// eviction case, where the miss would otherwise be likeliest, does not
		// rely on this at all — see the Evicted arm below.
		if ctx.Err() == nil {
			r.routable = false
		}
	}()

	stream := r.client.Register(ctx)

	// Closing the response side is what ends a blocked Receive, and it has
	// to happen on cancellation too.
	//
	// Cancelling the context passed to Register does *not* unblock a Receive
	// already waiting on the response body — measured against connect-go
	// v1.20.0 over stdlib h2c, deterministically, in
	// TestCancelDoesNotUnblockReceive. Only closing the response side ends
	// it, with "canceled: http2: response body closed". Without this watcher
	// the goroutine outlives Join's stop function, which waits for it.
	closeResponse := sync.OnceFunc(func() { _ = stream.CloseResponse() })
	watching := make(chan struct{})
	defer close(watching)
	go func() {
		select {
		case <-ctx.Done():
			closeResponse()
		case <-watching:
		}
	}()

	defer func() {
		// Both halves: the request side is what the server watches for
		// liveness, and leaving the response side open holds the connection.
		_ = stream.CloseRequest()
		closeResponse()
	}()

	if err := stream.Send(&registryv1.RegisterRequest{
		Service:         r.service,
		Port:            r.port,
		Pid:             r.o.pid,
		OnlyIfUnclaimed: careful,
	}); err != nil {
		return sessionResult{permanent: refused(err), err: err}
	}
	// Routable from here, not from the acknowledgement. voya registers a
	// service before it answers, so between the two there is a window in which
	// it is already routing here and this client does not yet know — and a stop
	// landing in it would skip the grace precisely when the grace is needed.
	// The cost of being early is a wait that buys nothing; the cost of being
	// late is the dropped request #7 is about.
	r.routable = true
	// The request side stays open from here. Not half-closing is ADR 004's
	// contract — the stream is the liveness signal, and this is the half the
	// server can watch.

	registered := false
	for {
		resp, err := stream.Receive()
		if err != nil {
			return sessionResult{registered: registered, permanent: refused(err), err: err}
		}
		switch event := resp.GetEvent().(type) {
		case *registryv1.RegisterResponse_Registered:
			if careful {
				// Announced here rather than when the session ends: this is
				// the moment traffic goes back to this instance, and the
				// developer needs to know at that moment, not afterwards.
				r.o.onReclaimed(Reclaim{Service: r.service, DeposedFor: time.Since(r.deposedAt)})
			}
			registered = true
			r.o.logf("voya: %s is serving on http://%s:%d",
				r.service, event.Registered.GetAuthority(), event.Registered.GetProxyPort())
			// The acknowledgement names the port voya actually serves the
			// fleet on. A dialer captures the port its own options named —
			// wired before this acknowledgement existed, and never re-wired
			// from it — so when the two disagree, a voya whose proxy_port
			// moved is routing this service's inbound traffic fine while the
			// fleet's dial string misses the proxy entirely, one line under
			// the message naming the live port. Say so beside it (#116) —
			// the loud counterpart of installDialer's warning to a second
			// Join that lost.
			//
			// What the message must not do is offer WithProxyPort on its own
			// as the remedy. redirect matches on the port it was handed
			// (TestDialerIgnoresInternalOnOtherPorts), so a service told only
			// that moves its dialer to voya's port and stops matching the
			// *.internal:<convention> URLs the fleet actually dials: the same
			// calls fail the same way, with this line now silent. The two
			// have to move together, which is why reverting voya's
			// proxy_port — the one remedy that is complete on its own — is
			// named first.
			//
			// It says "still dialing that port" rather than "will fail" for
			// the same reason: under WithoutDialer this client routes
			// nothing, and a service that built its own transport from
			// Dialer(WithProxyPort(...)) with voya's real port is reached by
			// this comparison and is not broken. Naming the port that goes
			// nowhere is true either way; asserting the outcome was not.
			if ack := event.Registered.GetProxyPort(); ack != r.o.proxyPort {
				r.o.logf("voya: this client is wired for the fleet's proxy on port %d, but voya is serving it on port %d — nothing answers on *.internal:%d, so every outbound call still dialing that port fails; revert voya's proxy_port to %d, or move the fleet to %d in both places, since WithProxyPort(%d) redirects only the port it names and the dial strings have to move with it",
					r.o.proxyPort, ack, r.o.proxyPort, r.o.proxyPort, ack, ack)
			}

		case *registryv1.RegisterResponse_Evicted:
			// Not routable from here, and said outright rather than left to
			// the deferred ctx.Err() check. This message *is* voya reporting
			// that the name went elsewhere, so it settles the question no
			// matter what the context is doing — and onEvicted below is a
			// caller's hook, which can take as long as it likes. Leaving it to
			// the defer would let a stop arriving while that hook runs pay the
			// full grace for a route that moved before the hook was even
			// called, which is the one restart already going badly.
			r.routable = false
			r.o.onEvicted(Eviction{
				Reason:         event.Evicted.GetReason(),
				ReplacedByPort: event.Evicted.GetReplacedByPort(),
				ReplacedByPID:  event.Evicted.GetReplacedByPid(),
			})
			return sessionResult{registered: registered, evicted: true}

		default:
			// A voya newer than this client. Ignoring it is right: the
			// stream's job is to stay open, and an unknown event is not a
			// reason to give up the registration.
			r.o.logf("voya: ignoring an unrecognised event from voya (%T); this client may be older than the daemon", event)
		}
	}
}

// refused reports whether voya rejected this registration rather than
// failing to receive it.
//
// InvalidArgument is the one code the registration server uses for that: the
// registry would not accept the name or the port, or the client broke the
// one-message contract. Every one of those is a property of what this client
// sends, and a reconnect sends exactly the same thing — so it is the
// difference between "voya is not up yet" and "voya is up and this will
// never work".
//
// [connect.IsWireError] is load-bearing rather than belt-and-braces. A stream
// cut mid-frame — which is every stream, every time voya restarts — surfaces
// client-side as "invalid_argument: protocol error: incomplete envelope:
// unexpected EOF", carrying the same code voya uses to reject a name. On the
// code alone, ADR 004's headline claim inverts: a voya restart would look
// permanent to every service at once and the whole fleet would stay
// deregistered until each was restarted by hand.
// [TestOnlyAWireErrorCountsAsARefusal] pins both halves.
func refused(err error) bool {
	return connect.IsWireError(err) && connect.CodeOf(err) == connect.CodeInvalidArgument
}

// jitter spreads reconnects so that a fleet of services restarted together
// does not retry in lockstep. Between half and all of d.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	//nolint:gosec // G404: this picks a retry delay, not a secret.
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}
