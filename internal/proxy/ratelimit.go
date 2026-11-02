package proxy

import (
	"strconv"
	"sync"
	"time"
)

// WithRateLimit runs ADR 006's local stand-in for the edge's limiter: at
// most requests forwarded per window, per authority. The key is the routed
// service name — what registry.ServiceFromAuthority made of the Host header
// — so payments.internal and payments.internal:9777 spend from one budget,
// and two authorities never share one. The check sits on the forwarding
// path only: a request the proxy refuses for routing never reached an
// upstream, so it costs nothing, and the reserved voya.internal is answered
// before the check exists and can never be throttled (#223). Reaching
// nothing costs nothing on the far side of the gate too: a forwarded request
// that never reached an upstream is refunded where the failure is
// discovered, and voya's own health probes, marked by [ProbeHeader], never
// spend at all (#233).
//
// What the per-authority key accepts, recorded in ADR 006's amendment: two
// of a developer's services calling one busy name share its budget, which a
// production edge keyed on the caller would not do. That is the price of a
// limit whose behaviour you can predict from the name you typed.
//
// requests of 0 or a window of 0 or less leaves the limiter off, the
// default: config validation refuses both on an enabled limiter, so this is
// the seam being defensive rather than a way to spell "refuse everything".
func WithRateLimit(requests uint32, window time.Duration) Option {
	return func(p *proxy) {
		if requests == 0 || window <= 0 {
			return
		}
		p.limiter = newLimiter(requests, window)
	}
}

// ProbeHeader marks a request as voya's own probe of a service — `voya
// health` and `voya status -health` send it — and a request carrying it is
// exempt from the rate limit: a probe asks after a service's state, and
// spending the service's budget to ask makes the report wrong exactly when a
// developer is investigating a throttle (#233). The proxy strips it before
// forwarding, so no fleet service ever sees voya's internal marker and the
// probe stays the call a real caller makes;
// TestAProbeSpendsNoBudgetAndItsMarkerIsStripped pins both halves. Any
// loopback process could send it, and that is accepted the way ADR 002
// accepts unauthenticated registration: a local process that could forge an
// exemption could already register itself under the service's name and take
// its traffic, which is strictly more power than skipping a limiter. One
// caller has less power than that and can still forge it — a page in the
// developer's browser, since [proxy.writeCORS] reflects any requested header
// and so permits this one — and what it gains is skipping a dev-time
// limiter, not reaching anything CORS did not already let it reach.
const ProbeHeader = "Voya-Probe"

// RateLimitedHeader marks a 429 as voya's own refusal — the limiter's, not a
// forwarded answer from an upstream. It qualifies the 429 status itself, so
// [markVoyasOwn429] sets it only on the shapes whose own status line says
// 429; a Trailers-Only or Connect-stream refusal answers HTTP 200 and
// carries Retry-After alone (#251). The client wrapper retries only a 429
// that carries it (#241): a fleet service's own application 429 is forwarded
// verbatim, reaches the caller on the first attempt, and exercises the same
// `resource_exhausted` branch it would in production, where no voya sits on
// the path — ADR 006's parity principle applied to the refusal itself. The
// proxy forwards responses verbatim, so an upstream that stamps this header
// on its own 429 gets it retried by voya's client — not a security boundary
// (loopback, ADR 002), but a surprise worth naming. The client module may
// not import this one (client/deps_test.go), so client/throttle.go restates
// the name; test/acceptance pins the two spellings equal.
const RateLimitedHeader = "Voya-Rate-Limited"

// limiter is a fixed-window counter per authority. Fixed windows rather than
// a token bucket, because the bug this limiter exists to catch is in the
// caller (ADR 006), and a fixed window is the shape whose refusals a test —
// or a developer counting curl calls — can predict exactly: the first
// request starts an authority's window, the next requests-1 within it are
// free, and the reset is one moment rather than a drip.
type limiter struct {
	requests uint32
	window   time.Duration

	// now is the clock, a field so a test can hold it still or move it by
	// exactly one nanosecond; everything else reads time through it.
	now func() time.Time

	mu      sync.Mutex
	windows map[string]*budgetWindow
}

// budgetWindow is one authority's current window: when it started and how
// many requests it has admitted.
type budgetWindow struct {
	start time.Time
	count uint32
}

func newLimiter(requests uint32, window time.Duration) *limiter {
	return &limiter{
		requests: requests,
		window:   window,
		now:      time.Now,
		windows:  make(map[string]*budgetWindow),
	}
}

// allow reports whether one more request to service fits its current
// window. When it does not, retryAfter is how long until the window resets —
// always positive, never more than the window, because the window that
// refused is the one being waited out. When it does, admitted is the start
// of the window that was charged: the token [limiter.refund] needs to hand
// the spend back to that window and no other.
func (l *limiter) allow(service string) (ok bool, retryAfter time.Duration, admitted time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	w, live := l.windows[service]
	if !live || now.Sub(w.start) >= l.window {
		l.windows[service] = &budgetWindow{start: now, count: 1}
		return true, 0, now
	}
	if w.count < l.requests {
		w.count++
		return true, 0, w.start
	}
	return false, w.start.Add(l.window).Sub(now), time.Time{}
}

// refund hands one admitted request back to service's budget, for a request
// that was forwarded and reached nothing (#233): the transport never got a
// connection, so the upstream the limiter exists to protect never saw it. admitted is the
// window token [limiter.allow] reported for the spend, and it is what keeps
// a refund inside the window that was charged — a window that has since
// reset, or a successor already started, is left alone rather than handed
// budget it never collected. The count check keeps a refund from ever taking
// a window below zero, which on a uint32 would wrap into "refuse everything".
func (l *limiter) refund(service string, admitted time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	w, live := l.windows[service]
	if !live || !w.start.Equal(admitted) || w.count == 0 {
		return
	}
	w.count--
}

// retryAfterHeader renders a refusal's wait as the Retry-After header's
// delta-seconds form: the remaining wait rounded up to whole seconds, and
// never below one, since the header cannot carry a fraction and rounding
// down would name a moment still inside the window that just refused. For a
// window of whole seconds — every window base.pxf can suggest — the result
// never exceeds the window; a sub-second remainder rounds up to the next
// second, which over-waits the caller by less than a second, the safe side.
func retryAfterHeader(remaining time.Duration) string {
	seconds := (remaining + time.Second - 1) / time.Second
	if seconds < 1 {
		seconds = 1
	}
	return strconv.FormatInt(int64(seconds), 10)
}
