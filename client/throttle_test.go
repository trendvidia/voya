package voya

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// throttledServer is a fake voya proxy for [RoundTripper]'s half of the
// contract: it refuses with whatever the script says and records when each
// request arrived, so the test goroutine can assert on the retry's timing
// after the fact rather than from a handler goroutine nothing synchronises.
type throttledServer struct {
	mu       sync.Mutex
	arrivals []time.Time
	refused  int

	// answer decides one request, given how many arrived before it. status
	// 0 means 200.
	answer func(n int) (status int, retryAfter string)

	// unmarked answers 429 without [RateLimitedHeader] — the shape of an
	// upstream's own verdict the proxy forwarded. The default is the
	// limiter's marked refusal, the only 429 the wrapper retries (#241).
	unmarked bool
}

func (s *throttledServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	s.mu.Lock()
	n := len(s.arrivals)
	s.arrivals = append(s.arrivals, time.Now())
	status, retryAfter := s.answer(n)
	if status == http.StatusTooManyRequests {
		s.refused++
	}
	s.mu.Unlock()
	if status == 0 {
		fmt.Fprint(w, "ok")
		return
	}
	if retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	if status == http.StatusTooManyRequests && !s.unmarked {
		w.Header().Set(RateLimitedHeader, "1")
	}
	w.WriteHeader(status)
	fmt.Fprint(w, "voya: over the limit")
}

func (s *throttledServer) snapshot() (arrivals []time.Time, refused int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.arrivals...), s.refused
}

// throttleFixture is one fake proxy plus a client whose transport is the
// wrapper under test, with the log captured. The URL carries a *.internal
// name on the fake's own port, which is what makes the wrapper treat the
// traffic as fleet traffic at all.
func throttleFixture(t *testing.T, s *throttledServer, extra ...Option) (url string, client *http.Client, logged func() []string) {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	port := portOfListener(t, srv.Listener)

	var mu sync.Mutex
	var lines []string
	opts := append([]Option{
		WithProxyPort(port),
		WithLogf(func(format string, args ...any) {
			mu.Lock()
			lines = append(lines, fmt.Sprintf(format, args...))
			mu.Unlock()
		}),
	}, extra...)
	client = &http.Client{Transport: RoundTripper(opts...)}
	url = "http://payments.internal:" + strconv.FormatUint(uint64(port), 10) + "/v1/charge"
	return url, client, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}
}

func mustRead(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response: %v", err)
	}
	return string(b)
}

// TestAFirstCall429IsWaitedOutAndRetried is the contract's happy half, on
// the hardest footing: the very first request — no warmed connection, no
// prior success — is refused with a delta-seconds Retry-After, and the
// caller gets an answer anyway, no earlier than the header allowed.
func TestAFirstCall429IsWaitedOutAndRetried(t *testing.T) {
	s := &throttledServer{answer: func(n int) (int, string) {
		if n == 0 {
			return http.StatusTooManyRequests, "1"
		}
		return 0, ""
	}}
	url, client, logged := throttleFixture(t, s)

	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	if body := mustRead(t, resp); resp.StatusCode != http.StatusOK || body != "ok" {
		t.Fatalf("got %d %q; the 429 was not retried into the answer", resp.StatusCode, body)
	}

	arrivals, _ := s.snapshot()
	if len(arrivals) != 2 {
		t.Fatalf("the server saw %d requests, want 2: one refusal, one honoured retry", len(arrivals))
	}
	if gap := arrivals[1].Sub(arrivals[0]); gap < time.Second {
		t.Errorf("the retry arrived %v after the refusal, inside the 1s the Retry-After named", gap)
	}

	// Which name, and for how long — once. Per episode, not per retry, is
	// asserted where there are several retries to collapse
	// (TestConcurrentCallersRecoverAndAreToldOnce).
	throttleLines := 0
	for _, line := range logged() {
		if strings.Contains(line, "rate limited") {
			throttleLines++
			if !strings.Contains(line, "payments.internal") || !strings.Contains(line, "Retry-After") {
				t.Errorf("the throttle report names neither the name nor the header: %q", line)
			}
		}
	}
	if throttleLines != 1 {
		t.Errorf("the throttle was reported %d times, want once", throttleLines)
	}
}

// TestAnHTTPDateRetryAfterIsHonoured: RFC 9110 allows the header to be a
// date, and a client that parses only delta-seconds would fall back to its
// own — deliberately tiny — backoff here and hammer out extra refusals. The
// server refuses everything before the named moment, so exactly two arrivals
// is what "the date was read" looks like.
func TestAnHTTPDateRetryAfterIsHonoured(t *testing.T) {
	// Truncated to the second the header can carry: an HTTP date has no
	// fraction, so a window that opened mid-second would tell the client a
	// moment up to a second before itself and refuse the honest retry.
	opens := time.Now().Add(2 * time.Second).Truncate(time.Second)
	s := &throttledServer{answer: func(int) (int, string) {
		if time.Now().Before(opens) {
			return http.StatusTooManyRequests, opens.UTC().Format(http.TimeFormat)
		}
		return 0, ""
	}}
	url, client, _ := throttleFixture(t, s, WithBackoff(10*time.Millisecond, 20*time.Millisecond))

	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	if body := mustRead(t, resp); resp.StatusCode != http.StatusOK || body != "ok" {
		t.Fatalf("got %d %q, want the retried answer", resp.StatusCode, body)
	}
	arrivals, _ := s.snapshot()
	if len(arrivals) != 2 {
		t.Errorf("the server saw %d requests, want 2 — more means the date form fell back to the local backoff", len(arrivals))
	}
}

// TestAMalformedRetryAfterFallsBackToTheBackoff: a header the client cannot
// read is no instruction, and the failure mode this pins out is treating it
// as zero — an instant retry. The client's own backoff has a floor, so the
// retry must arrive no sooner than that floor.
func TestAMalformedRetryAfterFallsBackToTheBackoff(t *testing.T) {
	const floor = 300 * time.Millisecond
	for name, header := range map[string]string{"malformed": "soon", "negative": "-5", "absent": ""} {
		t.Run(name, func(t *testing.T) {
			s := &throttledServer{answer: func(n int) (int, string) {
				if n == 0 {
					return http.StatusTooManyRequests, header
				}
				return 0, ""
			}}
			url, client, logged := throttleFixture(t, s, WithBackoff(floor, 2*floor))

			resp, err := client.Get(url)
			if err != nil {
				t.Fatalf("GET %s: %v", url, err)
			}
			if body := mustRead(t, resp); resp.StatusCode != http.StatusOK || body != "ok" {
				t.Fatalf("got %d %q, want the retried answer", resp.StatusCode, body)
			}
			arrivals, _ := s.snapshot()
			if len(arrivals) != 2 {
				t.Fatalf("the server saw %d requests, want 2", len(arrivals))
			}
			if gap := arrivals[1].Sub(arrivals[0]); gap < floor {
				t.Errorf("the retry arrived after %v, under the %v backoff floor — an unreadable header was treated as zero", gap, floor)
			}
			// And it says so: the wait came from this client's schedule, so
			// a report crediting it to the server's header would send a
			// reader looking for a number no server ever sent.
			for _, line := range logged() {
				if !strings.Contains(line, "rate limited") {
					continue
				}
				if strings.Contains(line, "honouring") {
					t.Errorf("the report claims the header was honoured though it was %s: %q", name, line)
				}
				if !strings.Contains(line, "backing off") {
					t.Errorf("the report does not say the wait is the fallback backoff: %q", line)
				}
				if header != "" && !strings.Contains(line, header) {
					t.Errorf("the report does not quote the unreadable header %q: %q", header, line)
				}
			}
		})
	}
}

// TestAnEpisodeThatFailsDoesNotSilenceTheNext: the say-so-once guard is per
// episode, and an episode ends however the call ends. A first call that
// gives up on the bound must not leave the authority latched mid-throttle,
// or every later throttle in the process passes in silence — the failure
// paths are exactly the ones a developer is trying to read about.
func TestAnEpisodeThatFailsDoesNotSilenceTheNext(t *testing.T) {
	const floor = 20 * time.Millisecond
	s := &throttledServer{answer: func(n int) (int, string) {
		switch n {
		case 0, 2:
			// Refused and waited out: this is where the episode starts.
			return http.StatusTooManyRequests, "0"
		case 1:
			// Refused again, now past DefaultThrottleBudget: the first call
			// ends in an error with its episode still running.
			return http.StatusTooManyRequests, "3600"
		}
		return 0, ""
	}}
	url, client, logged := throttleFixture(t, s, WithBackoff(floor, 2*floor))

	if _, err := client.Get(url); err == nil { //nolint:bodyclose // the bound fails the call; there is no body
		t.Fatal("the call succeeded though its Retry-After was past the budget")
	}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("the second call failed: %v", err)
	}
	if body := mustRead(t, resp); body != "ok" {
		t.Fatalf("got %q, want the retried answer", body)
	}

	reports := 0
	for _, line := range logged() {
		if strings.Contains(line, "rate limited") {
			reports++
		}
	}
	if reports != 2 {
		t.Errorf("%d throttle reports, want one per episode and there were two; log: %q", reports, logged())
	}
}

// TestARetryAfterPastTheDeadlineFailsNamingIt is the bound: honouring the
// header is not waiting forever, and a wait the caller's own deadline cannot
// afford must come back as an error naming the header's value — at once,
// not after sleeping the deadline out.
func TestARetryAfterPastTheDeadlineFailsNamingIt(t *testing.T) {
	s := &throttledServer{answer: func(int) (int, string) {
		return http.StatusTooManyRequests, "30"
	}}
	url, client, _ := throttleFixture(t, s)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	started := time.Now()
	resp, err := client.Do(req)
	if err == nil {
		mustRead(t, resp)
		t.Fatal("the call succeeded; a Retry-After past the deadline must fail")
	}
	if elapsed := time.Since(started); elapsed >= 500*time.Millisecond {
		t.Errorf("the failure took %v; it slept toward the deadline instead of failing at once", elapsed)
	}
	for _, want := range []string{"Retry-After", `"30"`, "payments.internal", "deadline"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not carry %q: %v", want, err)
		}
	}
	if arrivals, _ := s.snapshot(); len(arrivals) != 1 {
		t.Errorf("the server saw %d requests, want 1 — nothing should retry into a wait that cannot fit", len(arrivals))
	}
}

// TestABoundFailureDoesNotBlameAnUnreadableHeader is the same bound with
// garbage on the wire: the wait is then this client's own backoff, and an
// error saying the server "asks for" it sends the reader looking through the
// callee for a number it never sent. The header still gets quoted, because
// "your server sent this and it is not a Retry-After" is the fix.
func TestABoundFailureDoesNotBlameAnUnreadableHeader(t *testing.T) {
	s := &throttledServer{answer: func(int) (int, string) {
		return http.StatusTooManyRequests, "soon"
	}}
	url, client, _ := throttleFixture(t, s, WithBackoff(time.Second, time.Second))

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := client.Do(req)
	if err == nil {
		mustRead(t, resp)
		t.Fatal("the call succeeded though the fallback backoff was past the deadline")
	}
	if got := err.Error(); !strings.Contains(got, "unreadable") || !strings.Contains(got, `"soon"`) {
		t.Errorf("the error does not name the header as unreadable: %v", err)
	}
	if strings.Contains(err.Error(), "asks for") {
		t.Errorf("the error credits the fallback backoff to a header it could not read: %v", err)
	}
}

// TestNoDeadlineIsBoundedByTheBudget: a request that set no deadline still
// may not hang on a generous header. DefaultThrottleBudget is the named
// bound, and a header past it fails immediately, naming its value.
func TestNoDeadlineIsBoundedByTheBudget(t *testing.T) {
	s := &throttledServer{answer: func(int) (int, string) {
		return http.StatusTooManyRequests, "3600"
	}}
	url, client, _ := throttleFixture(t, s)

	started := time.Now()
	resp, err := client.Get(url)
	if err == nil {
		mustRead(t, resp)
		t.Fatal("the call succeeded; an hour-long Retry-After must fail, not hang")
	}
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Errorf("the failure took %v; the budget check must not sleep first", elapsed)
	}
	for _, want := range []string{`"3600"`, "DefaultThrottleBudget"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not carry %q: %v", want, err)
		}
	}
	if arrivals, _ := s.snapshot(); len(arrivals) != 1 {
		t.Errorf("the server saw %d requests, want 1", len(arrivals))
	}
}

// TestConcurrentCallersRecoverAndAreToldOnce is the stampede edge: several
// callers in one process throttled at once must every one wait the header
// out — a retry inside the window earns another refusal here, so the
// refusal count is the assertion — and the developer is told about the
// episode once, not once per caller per retry.
func TestConcurrentCallersRecoverAndAreToldOnce(t *testing.T) {
	const callers = 4
	var opens time.Time
	var once sync.Once
	s := &throttledServer{}
	s.answer = func(int) (int, string) {
		// The window starts at the first arrival, the way a fixed window
		// does; everything inside it is refused with the time left.
		once.Do(func() { opens = time.Now().Add(time.Second) })
		if remaining := time.Until(opens); remaining > 0 {
			return http.StatusTooManyRequests, strconv.Itoa(int((remaining + time.Second - 1) / time.Second))
		}
		return 0, ""
	}
	url, client, logged := throttleFixture(t, s)

	errs := make(chan error, callers)
	for range callers {
		go func() {
			resp, err := client.Get(url)
			if err == nil {
				if body := mustRead(t, resp); resp.StatusCode != http.StatusOK || body != "ok" {
					err = fmt.Errorf("got %d %q", resp.StatusCode, body)
				}
			}
			errs <- err
		}()
	}
	for range callers {
		if err := <-errs; err != nil {
			t.Errorf("a throttled caller did not recover: %v", err)
		}
	}

	_, refused := s.snapshot()
	if refused != callers {
		t.Errorf("the server refused %d requests for %d callers; more than one each means someone retried into the window it was told to wait out", refused, callers)
	}
	throttleLines := 0
	for _, line := range logged() {
		if strings.Contains(line, "rate limited") {
			throttleLines++
		}
	}
	if throttleLines != 1 {
		t.Errorf("the episode was reported %d times across %d callers, want once", throttleLines, callers)
	}
}

// TestAnUnreplayableBodyGetsTheRefusalItself: a request whose body was a
// live stream — no GetBody, which is what a client-streaming call's pipe
// looks like — cannot be re-sent, so the 429 must reach the caller as the
// answer rather than the wrapper guessing at a replay. The streaming shape
// end to end, with connect's own error code on it, is
// test/acceptance's TestAStreamRefusedAtTheStartIsRefusedVisibly.
func TestAnUnreplayableBodyGetsTheRefusalItself(t *testing.T) {
	s := &throttledServer{answer: func(int) (int, string) {
		return http.StatusTooManyRequests, "1"
	}}
	url, client, _ := throttleFixture(t, s)

	// io.MultiReader hides the concrete type from http.NewRequest, which
	// would otherwise set GetBody for a *strings.Reader and make this body
	// replayable after all — the setup the test needs is its absence.
	req, err := http.NewRequest(http.MethodPost, url, io.MultiReader(strings.NewReader("a live stream")))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if req.GetBody != nil {
		t.Fatal("the fixture's body is replayable; the case under test needs GetBody to be nil")
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	mustRead(t, resp)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("got %d, want the 429 surfaced untouched", resp.StatusCode)
	}
	if arrivals, _ := s.snapshot(); len(arrivals) != 1 {
		t.Errorf("the server saw %d requests, want 1 — a body that cannot be replayed must not be retried", len(arrivals))
	}
}

// TestA429OffTheFleetPathIsLeftAlone: the wrapper handles only traffic this
// package routes. A third-party API answering 429 keeps whatever meaning
// the service's own code gives it, exactly as the dialer leaves that
// traffic's destination alone.
func TestA429OffTheFleetPathIsLeftAlone(t *testing.T) {
	s := &throttledServer{answer: func(int) (int, string) {
		return http.StatusTooManyRequests, "1"
	}}
	_, client, logged := throttleFixture(t, s)

	external := httptest.NewServer(s)
	t.Cleanup(external.Close)

	resp, err := client.Get(external.URL)
	if err != nil {
		t.Fatalf("GET %s: %v", external.URL, err)
	}
	mustRead(t, resp)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("got %d, want the 429 passed through", resp.StatusCode)
	}
	if arrivals, _ := s.snapshot(); len(arrivals) != 1 {
		t.Errorf("the server saw %d requests, want 1 — nothing off the fleet path may be retried", len(arrivals))
	}
	if lines := logged(); len(lines) != 0 {
		t.Errorf("the wrapper spoke about traffic it does not handle: %q", lines)
	}
}

// TestAnUnmarked429ReturnsOnTheFirstAttempt: a 429 without
// [RateLimitedHeader] is a fleet service's own application verdict the proxy
// forwarded verbatim, not the limiter's refusal, and it reaches the caller
// exactly as sent — no wait, no retry, no throttle report — so the caller's
// resource_exhausted branch fires locally the way it would in production
// (#241). Every other test in this file is the positive control: the same
// fixture with the marker on is waited out and retried.
func TestAnUnmarked429ReturnsOnTheFirstAttempt(t *testing.T) {
	s := &throttledServer{
		unmarked: true,
		answer: func(int) (int, string) {
			return http.StatusTooManyRequests, "1"
		},
	}
	url, client, logged := throttleFixture(t, s)

	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	mustRead(t, resp)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("got %d, want the upstream's 429 surfaced untouched", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want the upstream's %q passed through", got, "1")
	}
	if arrivals, _ := s.snapshot(); len(arrivals) != 1 {
		t.Fatalf("the server saw %d requests, want 1 — a 429 the limiter did not mark must not be retried", len(arrivals))
	}
	if lines := logged(); len(lines) != 0 {
		t.Errorf("the wrapper reported a throttle that was not voya's: %q", lines)
	}
}

// TestParseRetryAfterReadsBothFormsAndOnlyThose pins the parser to RFC
// 9110's two shapes and to refusing everything else — refusing, not zero,
// because zero is an instruction to retry now and garbage is not.
func TestParseRetryAfterReadsBothFormsAndOnlyThose(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"7", 7 * time.Second, true},
		{"0", 0, true},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second, true},
		// A date already past means "retry now", never a negative sleep.
		{now.Add(-time.Minute).Format(http.TimeFormat), 0, true},
		{"", 0, false},
		{"-5", 0, false},
		{"1.5", 0, false},
		{"soon", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseRetryAfter(tc.in, now)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseRetryAfter(%q) = (%v, %t), want (%v, %t)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestAThrottleWaitEndsWithItsContext: a caller that gives up mid-wait gets
// its context's error back promptly instead of sleeping out the header.
func TestAThrottleWaitEndsWithItsContext(t *testing.T) {
	s := &throttledServer{answer: func(int) (int, string) {
		return http.StatusTooManyRequests, "5"
	}}
	url, client, _ := throttleFixture(t, s)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	// No deadline on the context, so the 5s header is inside the budget and
	// the wrapper commits to the wait — which the cancel below must end.
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	started := time.Now()
	resp, err := client.Do(req)
	if err == nil {
		mustRead(t, resp)
		t.Fatal("the call succeeded though its context was cancelled mid-wait")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the failure is not the context's own: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 5*time.Second {
		t.Errorf("cancellation took %v to act; the wait ignored the context", elapsed)
	}
}
