package voya

import (
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// DefaultThrottleBudget bounds the total time one request with no context
// deadline spends waiting out 429s before [RoundTripper] gives up and fails
// the call. A request that carries a deadline is bounded by that instead —
// the caller has already said how long they are willing to wait — so this
// constant exists for the caller who set none: honouring a Retry-After is
// not the same as waiting forever, and without some bound a generous header
// turns a fire-and-forget call into a hang (#224).
//
// Thirty seconds is [DefaultMaxBackoff]'s reasoning applied to the other
// direction of traffic: long enough to sit out any window base.pxf suggests,
// short enough that a developer watching a terminal sees the failure while
// they still remember sending the request.
const DefaultThrottleBudget = 30 * time.Second

// RateLimitedHeader is the response header voya's limiter stamps on its own
// 429s, and the only 429s [RoundTripper] waits out and retries (#241). A 429
// without it is a forwarded upstream's own answer — an over-quota tenant,
// say — and reaches the caller on the first attempt, exactly as it would in
// production, where no voya sits on the path. A service that stamps this
// header on its own 429 gets it retried by voya's client: not a security
// boundary — loopback, ADR 002 — but worth knowing before choosing it.
//
// This restates internal/proxy's RateLimitedHeader the way voya.go restates
// the ports: the dependency arrow points the other way (deps_test.go), and
// test/acceptance pins the two spellings equal.
const RateLimitedHeader = "Voya-Rate-Limited"

// RoundTripper is [Transport] plus the client half of ADR 006's rate-limit
// contract: a 429 voya's own limiter wrote — the ones carrying
// [RateLimitedHeader] — is an instruction to wait, not a failure.
// It is what [Join] installs as http.DefaultTransport, exported for services
// that build their own http.Client — use it where the README used to say
// [Transport], which returns the bare *http.Transport and treats a 429 like
// any other response.
//
// For a request this package routes — a *.internal name on the proxy port,
// the only requests voya's limiter can refuse — a 429 answer carrying
// [RateLimitedHeader], the limiter's own refusal, is retried after the wait
// the Retry-After header names, in either RFC 9110 form (delta-seconds or an
// HTTP date). Only the limiter's, note (#241): the proxy forwards an
// upstream's own answer verbatim, so a callee that answers 429 as its own
// application verdict — no marker — reaches the caller on the first attempt,
// the same answer its resource_exhausted branch sees in production, where no
// voya sits on the path. The header is the server's instruction
// and outranks any local schedule; only when it is absent or unreadable does
// the client fall back to its own doubling backoff, bounded by
// [WithBackoff]. Every wait has [DefaultMinBackoff]'s floor and a little
// additive jitter, so a valid header of zero cannot spin and concurrent
// callers released together do not all retry in the same instant.
//
// The retrying is bounded, and the bound is the request's own context
// deadline when it has one: a Retry-After that asks for more than the time
// remaining fails the call at once, naming the header's value, rather than
// sleeping into a deadline the caller will read as a hang. A request with no
// deadline is bounded by [DefaultThrottleBudget] of total waiting instead.
//
// Two kinds of request are answered rather than retried: one whose body
// cannot be replayed — a client-streaming or bidi call's body is a live
// stream with no way back — and one this package would not have routed,
// which includes everything on Fly, where no call of this package touches
// traffic. Both get the 429 exactly as the server sent it.
//
// A throttled name is reported through [WithLogf] once per episode — from
// the first refusal until an answer gets through — rather than once per
// retry, the way this package reports a moved proxy port.
func RoundTripper(opts ...Option) http.RoundTripper {
	o := resolve(opts)
	return o.roundTripper()
}

func (o *options) roundTripper() http.RoundTripper {
	return &throttleTransport{
		base:     o.transport(),
		o:        o,
		episodes: make(map[string]bool),
	}
}

// throttleTransport is [RoundTripper]'s implementation. base is always the
// *http.Transport [Transport] built; the field is a RoundTripper only so
// tests can script the far side without a listener.
type throttleTransport struct {
	base http.RoundTripper
	o    *options

	// episodes tracks which authorities are mid-throttle, so the developer
	// is told once per episode rather than once per retry. Keyed on the
	// authority as the request spelt it, which redirect has already matched
	// to the proxy port.
	mu       sync.Mutex
	episodes map[string]bool
}

// CloseIdleConnections forwards to the underlying transport, so an
// http.Client holding this wrapper can still release its pool.
func (t *throttleTransport) CloseIdleConnections() {
	if c, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

func (t *throttleTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if _, routed := t.o.redirect(req.URL.Host); !routed {
		// Not fleet traffic — a database, a third-party API, anything on
		// Fly. The dialer leaves it alone and so does this.
		return t.base.RoundTrip(req)
	}
	authority := req.URL.Host
	// However this call ends — an answer, a bound exceeded, a cancellation,
	// a body that cannot be replayed — the episode it may have started ends
	// with it. Latching on the failure paths would leave the authority
	// marked mid-throttle for the life of the process and silence every
	// episode after this one.
	defer t.endEpisode(authority)

	// A body with no GetBody cannot be re-sent: it was a live stream and the
	// first attempt consumed it. connect-go sets GetBody on every unary and
	// server-streaming request; what lands here is a client-streaming or
	// bidi call, whose refusal has to reach the caller as the code the
	// server chose.
	replayable := req.Body == nil || req.Body == http.NoBody || req.GetBody != nil

	attempt := req
	backoff := t.o.minBackoff
	var waited time.Duration
	for {
		resp, err := t.base.RoundTrip(attempt)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusTooManyRequests {
			return resp, nil
		}
		if resp.Header.Get(RateLimitedHeader) == "" {
			// A 429 the limiter did not mark is the upstream's own verdict,
			// forwarded verbatim: the caller's business, not this wrapper's.
			return resp, nil
		}
		if !replayable {
			return resp, nil
		}

		header := resp.Header.Get("Retry-After")
		wait, honoured := parseRetryAfter(header, time.Now())
		if !honoured {
			// Absent or unreadable: the server gave no instruction, so the
			// client's own schedule applies — never zero, which would turn
			// a malformed header into a hot loop.
			wait = backoff
			backoff = min(backoff*2, t.o.maxBackoff)
		}
		// The floor and the jitter both round up, the safe side: a retry
		// may arrive late into the window that refused it, never early into
		// it — and not in lockstep with every other caller throttled in the
		// same window.
		wait = max(wait, t.o.minBackoff)
		wait += rand.N(wait/8 + 1) //nolint:gosec // G404: jitter spreads retries, nothing guards on it

		if err := t.bound(req, header, honoured, wait, waited, authority); err != nil {
			drain(resp)
			return nil, err
		}
		t.startEpisode(authority, header, honoured, wait)
		drain(resp)

		select {
		case <-time.After(wait):
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
		waited += wait

		next := req.Clone(req.Context())
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, fmt.Errorf("voya: replaying the request %s refused: %w", authority, err)
			}
			next.Body = body
		}
		attempt = next
	}
}

// bound decides whether a wait fits what the caller can afford: the
// request's own deadline when it set one, [DefaultThrottleBudget] of total
// waiting when it did not. The error names the header's value, because "the
// server asked for more than you have" is the one fact the caller needs and
// the response carrying it is about to be discarded — and says so only when
// the header is what the wait came from, since attributing this client's own
// fallback backoff to a header it refused to read sends the reader after the
// wrong server.
func (t *throttleTransport) bound(req *http.Request, header string, honoured bool, wait, waited time.Duration, authority string) error {
	var ask string
	switch {
	case honoured:
		ask = fmt.Sprintf("its Retry-After of %q asks for %s", header, wait.Round(time.Millisecond))
	case header == "":
		ask = fmt.Sprintf("it carries no Retry-After and the fallback backoff is %s", wait.Round(time.Millisecond))
	default:
		ask = fmt.Sprintf("its Retry-After of %q is unreadable and the fallback backoff is %s", header, wait.Round(time.Millisecond))
	}
	if deadline, ok := req.Context().Deadline(); ok {
		if remaining := time.Until(deadline); wait >= remaining {
			return fmt.Errorf("voya: %s is rate limited and %s, more than this request's remaining %s; failing now rather than sleeping past the deadline",
				authority, ask, remaining.Round(time.Millisecond))
		}
		return nil
	}
	if waited+wait > DefaultThrottleBudget {
		return fmt.Errorf("voya: %s is rate limited and %s, past the %s a request with no deadline may spend waiting (DefaultThrottleBudget); failing now rather than hanging",
			authority, ask, DefaultThrottleBudget)
	}
	return nil
}

// startEpisode says which name is throttling and for how long, once per
// episode: the first refusal speaks, every retry after it is silent until
// endEpisode clears the slate. Whether the wait is the header's or this
// client's own is honoured's to say, not the header's presence: a header
// that arrived and did not parse is a fallback like an absent one, and
// reporting it as honoured would tell the reader a wait they can find
// nowhere on the wire.
func (t *throttleTransport) startEpisode(authority, header string, honoured bool, wait time.Duration) {
	t.mu.Lock()
	running := t.episodes[authority]
	t.episodes[authority] = true
	t.mu.Unlock()
	if running {
		return
	}
	switch {
	case honoured:
		t.o.logf("voya: %s is rate limited; honouring its Retry-After of %s", authority, wait.Round(time.Millisecond))
	case header == "":
		t.o.logf("voya: %s is rate limited with no Retry-After; backing off %s", authority, wait.Round(time.Millisecond))
	default:
		t.o.logf("voya: %s is rate limited and its Retry-After %q is unreadable; backing off %s", authority, header, wait.Round(time.Millisecond))
	}
}

func (t *throttleTransport) endEpisode(authority string) {
	t.mu.Lock()
	delete(t.episodes, authority)
	t.mu.Unlock()
}

// parseRetryAfter reads both shapes RFC 9110 gives the header: delta-seconds
// and an HTTP date, which resolves to the delta between it and now — zero
// when the date has already passed, since a moment in the past means "retry
// now", not "never retry". Anything else, a negative delta included, is not
// an instruction: ok is false and the caller falls back to its own backoff
// rather than treating garbage as zero.
func parseRetryAfter(v string, now time.Time) (wait time.Duration, ok bool) {
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseInt(v, 10, 32); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if at, err := http.ParseTime(v); err == nil {
		return max(at.Sub(now), 0), true
	}
	return 0, false
}

// drain empties and closes a response about to be retried, so its connection
// goes back to the pool instead of being torn down under the retry. Bounded:
// a 429's body is one diagnostic, and anything larger is not worth reading
// to save a dial.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}
