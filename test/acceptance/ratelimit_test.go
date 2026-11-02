package acceptance

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	voya "github.com/trendvidia/voya/client"
	"github.com/trendvidia/voya/internal/proxy"
	configv1 "github.com/trendvidia/voya/proto/voya/config/v1"
)

// TestTheRateLimitedMarkerIsSpelledTheSameOnBothSides: the client module may
// not import the daemon's (client/deps_test.go), so client/throttle.go
// restates internal/proxy's marker header by name. A divergence would fail
// TestAThrottledCallerWaitsTheRetryAfterAndSucceeds too, as a timeout; this
// names it (#241).
func TestTheRateLimitedMarkerIsSpelledTheSameOnBothSides(t *testing.T) {
	if voya.RateLimitedHeader != proxy.RateLimitedHeader {
		t.Fatalf("client retries on %q, the limiter stamps %q — the wrapper would surface every refusal instead of waiting it out",
			voya.RateLimitedHeader, proxy.RateLimitedHeader)
	}
}

// These tests are #224's end of ADR 006: the limiter of #223 is on, real,
// and refusing, and the thing under test is the client package every fleet
// service imports — does a caller through it wait the Retry-After out and
// survive, rather than failing, retrying early, or hanging. A stub server
// answering 429 would prove nothing about this path, which is why the
// limiter here is voya's own (issue #224's done-when says so in as many
// words); the header-parsing edges a real limiter cannot emit — HTTP dates,
// garbage — are client/throttle_test.go's, against a scripted server.

// limited turns the limiter on for the daemon under test. The budget is per
// authority (ADR 006), so "for one service" and "for this daemon" meet in a
// suite where each test starts its own voya and one callee.
func limited(requests uint32, window time.Duration) func(*configv1.Config) {
	return func(cfg *configv1.Config) {
		cfg.RateLimit = &configv1.RateLimit{
			Enabled:  true,
			Requests: requests,
			Window:   durationpb.New(window),
		}
	}
}

// throttlingCaller is [callerTo] with the transport a fleet service actually
// gets: [voya.RoundTripper], the dialer plus the client half of the
// rate-limit contract. It returns what the transport said through its logf,
// because "which name is throttling, said once" is one of the claims under
// test.
func throttlingCaller(t *testing.T, v *voyad, name string) (*connect.Client[wrapperspb.StringValue, wrapperspb.StringValue], func() []string) {
	t.Helper()
	var mu sync.Mutex
	var lines []string
	rt := voya.RoundTripper(
		voya.WithProxyPort(v.proxyPort),
		voya.WithLogf(func(format string, args ...any) {
			mu.Lock()
			lines = append(lines, fmt.Sprintf(format, args...))
			mu.Unlock()
			t.Logf("caller: "+format, args...)
		}),
	)
	if c, ok := rt.(interface{ CloseIdleConnections() }); ok {
		t.Cleanup(c.CloseIdleConnections)
	}
	url := "http://" + name + ".internal:" + strconv.FormatUint(uint64(v.proxyPort), 10) + procedure
	client := connect.NewClient[wrapperspb.StringValue, wrapperspb.StringValue](
		&http.Client{Transport: rt, Timeout: callerTimeout}, url)
	return client, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}
}

// TestAThrottledCallerWaitsTheRetryAfterAndSucceeds is #224's first
// done-when, end to end: a caller through client/'s transport trips the real
// limiter, waits what the header said — never less — and gets the answer,
// with the throttling name reported to its log once. Since #241 it is also
// the proof that narrowing the wrapper to marked 429s did not disable #224:
// the wrapper retries only a 429 carrying the marker the real limiter
// stamps, so this passes only while the two ends of that contract meet.
func TestAThrottledCallerWaitsTheRetryAfterAndSucceeds(t *testing.T) {
	const window = time.Second
	v, _ := startOnFreePorts(t, limited(1, window))
	startService(t, v, "other", "prime")
	c, logged := throttlingCaller(t, v, "other")

	// Spend the window's whole budget. This is also the call that proves
	// the fixture: a limiter that was not actually on would pass everything
	// below without the mechanism under test ever running.
	if got, err := call(t, c); err != nil || got == "" {
		t.Fatalf("the call inside the budget failed: %v", err)
	}

	started := time.Now()
	got, err := call(t, c)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("the throttled call failed instead of waiting out the Retry-After: %v", err)
	}
	if !strings.HasPrefix(got, "prime|") {
		t.Errorf("the retried call reached %q, want the registered instance", got)
	}
	// The refusal's Retry-After is the window remainder rounded up, so it
	// always names at least one whole second here — a client that retried
	// earlier than its header can only have ignored it.
	if elapsed < window {
		t.Errorf("the throttled call returned after %v, inside the wait its Retry-After named (>= %v)", elapsed, window)
	}

	reports := 0
	for _, line := range logged() {
		if strings.Contains(line, "rate limited") {
			reports++
		}
	}
	if reports != 1 {
		t.Errorf("the throttle was reported %d times, want once per episode; log: %q", reports, logged())
	}
}

// TestARetryAfterPastTheCallersDeadlineFailsInsteadOfHanging is the bound:
// with a one-minute window the header asks for more than this caller's
// half-second deadline can afford, and the call must come back at once with
// the header's value in the error — not sleep into a hang the caller reads
// as voya's.
func TestARetryAfterPastTheCallersDeadlineFailsInsteadOfHanging(t *testing.T) {
	v, _ := startOnFreePorts(t, limited(1, time.Minute))
	startService(t, v, "other", "prime")
	c, _ := throttlingCaller(t, v, "other")

	if _, err := call(t, c); err != nil {
		t.Fatalf("the call inside the budget failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := c.CallUnary(ctx, connect.NewRequest(wrapperspb.String("ping")))
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("the call succeeded though its deadline could not afford the Retry-After")
	}
	if elapsed >= diagnosticDeadline {
		t.Fatalf("the failure took %v; a wait the deadline cannot afford must fail at once", elapsed)
	}
	for _, want := range []string{"rate limited", "Retry-After", "other.internal"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not carry %q: %v", want, err)
		}
	}
	// The header's value, not a literal "60": the limiter renders the
	// remainder of the window, which is 59 as soon as a second of the
	// minute has gone on the dial and the first call — a race this test
	// would otherwise lose on a busy machine. What is under test is that
	// the number the server sent reaches the caller, so assert its shape
	// and that it is in the window it came from.
	named := retryAfterInError.FindStringSubmatch(err.Error())
	if named == nil {
		t.Fatalf("the error names no Retry-After value: %v", err)
	}
	if secs, convErr := strconv.Atoi(named[1]); convErr != nil || secs < 50 || secs > 60 {
		t.Errorf("the error names a Retry-After of %q, want the remainder of the one-minute window: %v", named[1], err)
	}
}

// retryAfterInError reads the header value out of the transport's error,
// whose shape is client/throttle.go's bound: ... Retry-After of "60" ...
var retryAfterInError = regexp.MustCompile(`Retry-After of "(\d+)"`)

// TestThrottledCallersDrainOnePerWindow is the stampede edge: three callers
// in one process, all throttled at once, must each sit its own header out —
// so the successes come one per window, never two into the same one — and
// all of them must get through.
func TestThrottledCallersDrainOnePerWindow(t *testing.T) {
	const window = time.Second
	v, _ := startOnFreePorts(t, limited(1, window))
	startService(t, v, "other", "prime")
	c, _ := throttlingCaller(t, v, "other")

	const callers = 3
	type done struct {
		at  time.Time
		err error
	}
	results := make(chan done, callers)
	for range callers {
		go func() {
			_, err := call(t, c)
			results <- done{at: time.Now(), err: err}
		}()
	}
	var finished []time.Time
	for range callers {
		r := <-results
		if r.err != nil {
			t.Errorf("a throttled caller did not survive: %v", r.err)
			continue
		}
		finished = append(finished, r.at)
	}
	if len(finished) != callers {
		t.Fatal("not every caller got an answer; the spacing below would assert on the survivors only")
	}
	// One success per window is what the limiter admits, so honest waiting
	// shows up as successes at least a window apart, minus only the
	// measurement's own jitter. Two inside one window would mean a caller
	// retried before the moment its Retry-After named.
	sort.Slice(finished, func(i, j int) bool { return finished[i].Before(finished[j]) })
	const slack = 100 * time.Millisecond
	for i := 1; i < len(finished); i++ {
		if gap := finished[i].Sub(finished[i-1]); gap < window-slack {
			t.Errorf("two callers succeeded %v apart, inside one %v window", gap, window)
		}
	}
}

// TestAStreamRefusedAtTheStartIsRefusedVisibly: a client-streaming call's
// body is a live pipe with no way back, so the client cannot replay it and
// must not pretend to — what the caller gets is the refusal itself,
// resource_exhausted with voya's diagnostic on it, promptly (ADR 005: an
// answer, not a hang).
func TestAStreamRefusedAtTheStartIsRefusedVisibly(t *testing.T) {
	v, _ := startOnFreePorts(t, limited(1, time.Minute))
	startService(t, v, "other", "prime")
	c, _ := throttlingCaller(t, v, "other")

	if _, err := call(t, c); err != nil {
		t.Fatalf("the call inside the budget failed: %v", err)
	}

	failed := make(chan error, 1)
	go func() {
		stream := c.CallClientStream(context.Background())
		// Send's own error, if the refusal lands mid-write, still surfaces
		// from CloseAndReceive; only the terminal outcome is asserted on.
		_ = stream.Send(wrapperspb.String("ping"))
		_, err := stream.CloseAndReceive()
		failed <- err
	}()
	select {
	case err := <-failed:
		if err == nil {
			t.Fatal("the stream succeeded though the window's budget was spent")
		}
		if code := connect.CodeOf(err); code != connect.CodeResourceExhausted {
			t.Errorf("the refusal reached the stream as %v, want resource_exhausted: %v", code, err)
		}
		assertVoyaDiagnostic(t, err, "other")
	case <-time.After(diagnosticDeadline):
		t.Fatalf("the stream was still waiting after %v; a refusal it cannot retry must surface, not hang", diagnosticDeadline)
	}
}
