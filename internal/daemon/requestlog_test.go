package daemon

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/trendvidia/voya/internal/registry"
	"github.com/trendvidia/voya/internal/requestlog"
	configv1 "github.com/trendvidia/voya/proto/voya/config/v1"
	diagv1 "github.com/trendvidia/voya/proto/voya/diag/v1"
	"github.com/trendvidia/voya/proto/voya/diag/v1/diagv1connect"
)

// --- #150: the request log, end to end in the daemon --------------------------

// listRequests reads the buffer back the way `voya log` does: through
// DiagService under voya.internal on the proxy port.
func listRequests(t *testing.T, c diagv1connect.DiagServiceClient, limit uint32) ([]requestlog.Request, error) {
	t.Helper()
	res, err := c.ListRequests(context.Background(), connect.NewRequest(&diagv1.ListRequestsRequest{Limit: limit}))
	if err != nil {
		return nil, err
	}
	out := make([]requestlog.Request, 0, len(res.Msg.GetRequests()))
	for _, m := range res.Msg.GetRequests() {
		out = append(out, requestlog.FromProto(m))
	}
	return out, nil
}

// requestLines counts the live lines: every message the daemon printed that
// is a request log line — the rendering of a record and nothing else — and
// carries want. Matched on the shape (a `-> ` column and no `voya:` prefix)
// so an unrelated INFO line naming the same authority does not count.
func requestLines(log *Transcript, want string) int {
	n := 0
	for _, line := range strings.Split(log.String(), "\n") {
		if strings.Contains(line, " -> ") && !strings.HasPrefix(line, "voya:") && strings.Contains(line, want) {
			n++
		}
	}
	return n
}

// TestEveryRequestReachesTheBufferInOrder is #150's first criterion in the
// daemon: routed requests and requests voya answered from a diagnostic row
// each land once, oldest first, with all six fields, and read back through
// ListRequests with limit honoured. And the live line, at the default INFO:
// one per request, rendered exactly as the record renders.
func TestEveryRequestReachesTheBufferInOrder(t *testing.T) {
	log := NewTranscript(nil)
	cfg := testConfig(t, 250*time.Millisecond)
	d, err := Start(context.Background(), cfg, log.Logf)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	upstream(t, d, "payments", 0, make(chan struct{}, 8))
	up, _ := d.Registry().Lookup("payments")
	c := http11Caller(t)
	before := time.Now()

	// Three exchanges: routed, never registered, not a name.
	for _, ex := range []struct {
		authority string
		status    int
	}{
		{registry.Authority("payments"), http.StatusOK},
		{registry.Authority("nobody"), http.StatusNotFound},
		{"api.payments.internal", http.StatusBadRequest},
	} {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
			"http://127.0.0.1:"+strconv.FormatUint(uint64(cfg.GetProxyPort()), 10)+"/v1/thing", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = ex.authority
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", ex.authority, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != ex.status {
			t.Fatalf("%s answered %d, the setup wants %d", ex.authority, resp.StatusCode, ex.status)
		}
	}

	dc := diagClient(t, cfg.GetProxyPort())
	var recs []requestlog.Request
	waitUntil(t, "the three requests to be recorded", func() bool {
		recs, err = listRequests(t, dc, 0)
		return err == nil && len(recs) >= 3
	})
	// The reads themselves are requests through the proxy, and are the one
	// kind the log leaves out — so the three records are the three
	// exchanges, and nothing follows them.
	if len(recs) != 3 {
		t.Fatalf("ListRequests = %d records, want the three exchanges and not the reads:\n%v", len(recs), recs)
	}
	for i, want := range []struct {
		authority string
		status    int
		port      uint32
	}{
		{"payments.internal", 200, up.Port},
		{"nobody.internal", 404, 0},
		{"api.payments.internal", 400, 0},
	} {
		got := recs[i]
		if got.Authority != want.authority || got.Method != http.MethodGet || got.Path != "/v1/thing" ||
			got.Status != want.status || got.UpstreamPort != want.port {
			t.Errorf("record %d = %+v, want %s GET /v1/thing %d -> %d", i, got, want.authority, want.status, want.port)
		}
		if got.At.Before(before.Truncate(time.Second)) || got.At.After(time.Now()) || got.Duration <= 0 {
			t.Errorf("record %d: at %v duration %v", i, got.At, got.Duration)
		}
	}

	// limit: the newest N, oldest first.
	for _, tc := range []struct {
		limit uint32
		want  []string
	}{
		{1, []string{"api.payments.internal"}},
		{2, []string{"nobody.internal", "api.payments.internal"}},
		{9, []string{"payments.internal", "nobody.internal", "api.payments.internal"}},
	} {
		got, err := listRequests(t, dc, tc.limit)
		if err != nil {
			t.Fatalf("ListRequests(limit %d): %v", tc.limit, err)
		}
		var authorities []string
		for _, r := range got {
			authorities = append(authorities, r.Authority)
		}
		if strings.Join(authorities, ",") != strings.Join(tc.want, ",") {
			t.Errorf("ListRequests(limit %d) = %v, want %v", tc.limit, authorities, tc.want)
		}
	}

	// The live line: once per record, and the same text `voya log` prints.
	for _, r := range recs[:3] {
		if n := requestLines(log, " "+r.Authority+" "); n != 1 {
			t.Errorf("%d live lines for %s, want exactly one:\n%s", n, r.Authority, log)
		}
		if !log.Printed(r.String()) {
			t.Errorf("the live line for %s is not the record's own rendering %q:\n%s", r.Authority, r, log)
		}
	}
}

// TestTheBufferIsBounded: Capacity+k requests through the daemon leave
// exactly the last Capacity, in order.
func TestTheBufferIsBounded(t *testing.T) {
	cfg := testConfig(t, 250*time.Millisecond)
	cfg.LogLevel = configv1.LogLevel_LOG_LEVEL_WARN // no live line per request; this makes a thousand of them
	d, err := Start(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	c := http11Caller(t)
	const extra = 7
	for i := range requestlog.Capacity + extra {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
			"http://127.0.0.1:"+strconv.FormatUint(uint64(cfg.GetProxyPort()), 10)+"/n/"+strconv.Itoa(i), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = registry.Authority("nobody")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		_ = resp.Body.Close()
	}
	dc := diagClient(t, cfg.GetProxyPort())
	var recs []requestlog.Request
	// A record lands after its response is written, so the last request's
	// may still be landing when the first read arrives; the reads themselves
	// are not recorded.
	lastRequest := "/n/" + strconv.Itoa(requestlog.Capacity+extra-1)
	waitUntil(t, "the last request to be recorded", func() bool {
		recs, err = listRequests(t, dc, 0)
		return err == nil && len(recs) > 0 && recs[len(recs)-1].Path == lastRequest
	})
	if len(recs) != requestlog.Capacity {
		t.Fatalf("the buffer holds %d records, want its capacity %d", len(recs), requestlog.Capacity)
	}
	// Exactly the last Capacity, in order.
	for i, r := range recs {
		if want := "/n/" + strconv.Itoa(extra+i); r.Path != want {
			t.Fatalf("record %d is %s, want %s: the buffer did not keep the last %d in order", i, r.Path, want, requestlog.Capacity)
		}
	}
}

// TestLogLevelSilencesTheLiveLineNotTheBuffer: at LOG_LEVEL_WARN `voya run`
// prints no request line and ListRequests still has them — both pinned,
// beside the INFO row TestEveryRequestReachesTheBufferInOrder covers.
func TestLogLevelSilencesTheLiveLineNotTheBuffer(t *testing.T) {
	for _, tc := range []struct {
		level configv1.LogLevel
		line  bool
	}{
		{configv1.LogLevel_LOG_LEVEL_ERROR, false},
		{configv1.LogLevel_LOG_LEVEL_WARN, false},
		{configv1.LogLevel_LOG_LEVEL_INFO, true},
		{configv1.LogLevel_LOG_LEVEL_DEBUG, true},
	} {
		t.Run(tc.level.String(), func(t *testing.T) {
			log := NewTranscript(nil)
			cfg := testConfig(t, 250*time.Millisecond)
			cfg.LogLevel = tc.level
			d, err := Start(context.Background(), cfg, log.Logf)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(func() { _ = d.Close() })

			if got := get(http11Caller(t), cfg.GetProxyPort(), registry.Authority("nobody")); got.err != nil {
				t.Fatalf("GET: %v", got.err)
			}
			dc := diagClient(t, cfg.GetProxyPort())
			var recs []requestlog.Request
			waitUntil(t, "the request to be recorded", func() bool {
				recs, err = listRequests(t, dc, 0)
				return err == nil && len(recs) >= 1
			})
			if recs[0].Authority != "nobody.internal" || recs[0].Status != http.StatusNotFound {
				t.Errorf("recorded %+v at %s", recs[0], tc.level)
			}
			if got := requestLines(log, "nobody.internal") > 0; got != tc.line {
				t.Errorf("live line printed = %v at %s, want %v:\n%s", got, tc.level, tc.line, log)
			}
		})
	}
}

// TestRequestRecorderIsTheBufferAloneAboveInfo: at WARN and above, the proxy
// records through the ring buffer itself and not through a tee whose second
// recorder throws every record away (#163). Above INFO that recorder's
// function is [discard], so the line is silent either way —
// [TestLogLevelSilencesTheLiveLineNotTheBuffer] is what says that — and what
// is left to assert is the boxing: a [requestlog.RecorderFunc] in the tee
// takes a [requestlog.Request] into an interface per request the proxy answers,
// on the one path #150 promised to keep quiet.
//
// So the tee's presence is asserted by identity, which is the only way it is
// observable at all, and the live line beside it by driving the recorder once:
// the buffer holds the record at every level, and prints it at INFO and below
// only.
func TestRequestRecorderIsTheBufferAloneAboveInfo(t *testing.T) {
	for _, tc := range []struct {
		level configv1.LogLevel
		teed  bool
	}{
		{configv1.LogLevel_LOG_LEVEL_ERROR, false},
		{configv1.LogLevel_LOG_LEVEL_WARN, false},
		{configv1.LogLevel_LOG_LEVEL_INFO, true},
		{configv1.LogLevel_LOG_LEVEL_DEBUG, true},
	} {
		t.Run(tc.level.String(), func(t *testing.T) {
			buf := requestlog.New(requestlog.Capacity)
			printed := 0
			rec := requestRecorder(newSink(tc.level, func(string, ...any) { printed++ }), buf)
			if teed := rec != requestlog.Recorder(buf); teed != tc.teed {
				t.Errorf("requestRecorder at %s is a tee = %v, want %v", tc.level, teed, tc.teed)
			}
			rec.Record(requestlog.Request{Authority: "payments.internal", Path: "/one", Status: 200})
			if got := buf.Len(); got != 1 {
				t.Errorf("the buffer holds %d records after one at %s, want 1", got, tc.level)
			}
			want := 0
			if tc.teed {
				want = 1
			}
			if printed != want {
				t.Errorf("one request printed %d live lines at %s, want %d", printed, tc.level, want)
			}
		})
	}
}

// TestRequestLogOffRecordsNothingAndPrintsNothing: `request_log = false` in
// a config layer keeps no buffer — the readers say so — and prints no line,
// at a level that would print one.
func TestRequestLogOffRecordsNothingAndPrintsNothing(t *testing.T) {
	log := NewTranscript(nil)
	cfg := testConfig(t, 250*time.Millisecond)
	cfg.RequestLog = false
	d, err := Start(context.Background(), cfg, log.Logf)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	if got := get(http11Caller(t), cfg.GetProxyPort(), registry.Authority("nobody")); got.err != nil {
		t.Fatalf("GET: %v", got.err)
	}
	// The setup: the request happened and voya answered it — the diagnostic
	// names the authority.
	if !log.Printed(Banner) {
		t.Fatalf("the level prints nothing at all; the negative below would be vacuous:\n%s", log)
	}
	dc := diagClient(t, cfg.GetProxyPort())
	_, err = listRequests(t, dc, 0)
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "request_log = false") {
		t.Errorf("ListRequests with the log off: %v, want failed_precondition naming request_log = false", err)
	}
	// Two requests went through the proxy above; the log has a line for
	// neither.
	if n := requestLines(log, " -> "); n != 0 {
		t.Errorf("%d request lines printed with request_log = false:\n%s", n, log)
	}
	// And the default is on, read from base.pxf and not restated here:
	// testConfig builds its Config in code, so this is internal/config's
	// TestDefaultsWithNoUserLayer's to prove; what this test pins is that
	// the daemon reads the field rather than a constant of its own.
}
