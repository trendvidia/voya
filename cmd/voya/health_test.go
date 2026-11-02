package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"
	"github.com/trendvidia/protowire-go/encoding/pxf"

	voya "github.com/trendvidia/voya/client"
	diagv1 "github.com/trendvidia/voya/proto/voya/diag/v1"
)

// --- #154: voya health ---------------------------------------------------------
//
// The same two-process shape as status_test.go: `voya run` on ports of its
// own, fixtures registered through the real client package, and `voya
// health` / `voya status -health` run as the binary against the same config
// directory. The fixtures are what ADR 004 says every service is: an h2c
// server that also serves grpc.health.v1 over Connect — the first local
// consumer of that clause.

// healthService is [echoService] plus a grpc.health.v1 server answering with
// checker, the way ADR 004 says every fleet service serves one; every other
// path still echoes, so [waitRouted] works on it.
func healthService(t *testing.T, v *voyaRun, name string, checker grpchealth.Checker, extra ...voya.Option) (port uint32) {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(grpchealth.NewHandler(checker))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, r.URL.Path) })
	return serveAndJoin(t, v, name, mux, extra...)
}

// wedgedChecker is a health server paused where a debugger would pause it:
// Check never answers until the caller gives up. calls counts the probes it
// received, so a test can assert the probe arrived and was what timed out.
type wedgedChecker struct{ calls chan struct{} }

func (w *wedgedChecker) Check(ctx context.Context, _ *grpchealth.CheckRequest) (*grpchealth.CheckResponse, error) {
	select {
	case w.calls <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// registerClosed joins name with a listener that is closed again before the
// function returns: the registry holds the name and the port, the stream stays
// up, and nothing accepts on the port — the "registered service whose port is
// closed" of #154. The listener is bound only long enough to have an address
// to register.
func registerClosed(t *testing.T, v *voyaRun, name string, pid int32) (port uint32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	addr := ln.Addr()
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	stop, err := voya.Join(t.Context(), name, addr,
		voya.WithRegistrationPort(v.ports.Registration),
		voya.WithProxyPort(v.ports.Proxy),
		voya.WithoutDialer(),
		voya.WithPID(pid),
		voya.WithBackoff(10*time.Millisecond, 200*time.Millisecond),
		voya.WithLogf(func(string, ...any) {}),
	)
	if err != nil {
		t.Fatalf("Join %s: %v", name, err)
	}
	t.Cleanup(stop)
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is %T", addr)
	}
	return uint32(tcp.Port)
}

// serviceRows is the services table of a status screen: the header's fields
// and one field slice per row, in table order.
func serviceRows(t *testing.T, out string) (header []string, rows [][]string) {
	t.Helper()
	lines := strings.Split(out, "\n")
	start := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "NAME ") })
	if start < 0 {
		t.Fatalf("no services table:\n%s", out)
	}
	header = strings.Fields(lines[start])
	for _, l := range lines[start+1:] {
		if l == "" {
			break
		}
		rows = append(rows, strings.Fields(l))
	}
	return header, rows
}

// healthCells is the HEALTH cell of every row, by service name.
func healthCells(t *testing.T, out string) map[string]string {
	t.Helper()
	header, rows := serviceRows(t, out)
	col := slices.Index(header, "HEALTH")
	if col < 0 {
		t.Fatalf("no HEALTH column in %q", header)
	}
	cells := make(map[string]string, len(rows))
	for _, f := range rows {
		if len(f) > col {
			cells[f[0]] = f[col]
		} else {
			cells[f[0]] = ""
		}
	}
	return cells
}

// TestVoyaHealthReportsEachServiceBesideItsEntry is #154's four probes on one
// fleet: a service that answers SERVING, one that answers NOT_SERVING, one
// whose port is closed, and one that accepts and never answers — plus one
// that serves no health at all. `voya health` reports each beside its
// registry entry and exits 0; the closed port's detail is voya's own
// diagnostic; the wedged one reads timeout after the short bound and is still
// registered afterwards, stream up; `voya status -health` is the same
// screen; and -pxf on either renders the HealthReport.
func TestVoyaHealthReportsEachServiceBesideItsEntry(t *testing.T) {
	skipWithoutSIGTERM(t)
	v := startVoyaRun(t)
	v.waitUntilUp(t)

	wedge := &wedgedChecker{calls: make(chan struct{}, 8)}
	onePort := healthService(t, v, "one", grpchealth.NewStaticChecker(), voya.WithPID(4001))
	otherPort := healthService(t, v, "other", notServing(), voya.WithPID(4002))
	stuckPort := healthService(t, v, "stuck", wedge, voya.WithPID(4004))
	plainPort := echoService(t, v, "plain", voya.WithPID(4005))
	// Last, after every other fixture holds its port: registerClosed frees
	// the ephemeral port it registers, and a fixture that bound after it
	// could be handed the same one back — which would make `gone` answer
	// `serving` and fail this test with nothing in the code changed. The
	// table is sorted by name, so registration order is not row order.
	gonePort := registerClosed(t, v, "gone", 4003)
	waitRouted(t, v, "one.internal")
	waitRouted(t, v, "other.internal")
	waitRouted(t, v, "stuck.internal")
	waitRouted(t, v, "plain.internal")
	before := waitForStatusLine(t, v, "services (5)")

	// The setup, asserted before the probes: every one of the five is
	// registered with its stream attached, the wedged one included.
	for name, cell := range healthCells(t, before.Stdout.String()) {
		if cell != "" {
			t.Errorf("plain `voya status` filled %s's HEALTH cell with %q", name, cell)
		}
	}
	_, rows := serviceRows(t, before.Stdout.String())
	if len(rows) != 5 || rows[4][0] != "stuck" || rows[4][5] != "attached" {
		t.Fatalf("before the probes the table is not five attached rows with stuck last:\n%s", before.Stdout.String())
	}

	start := time.Now()
	h := startVoyaCmd(t, v, "health")
	if code := h.exitCode(t, "against five services"); code != 0 {
		t.Fatalf("`voya health` exited %d\n%s", code, h.Output())
	}
	took := time.Since(start)
	if h.Stderr.String() != "" {
		t.Errorf("`voya health` wrote to stderr: %q", h.Stderr.String())
	}
	out := h.Stdout.String()
	// The wedged probe is what bounds the command: it must have run out the
	// deadline, and the command must not have waited much past it — the
	// probes run at once, so five services cost one deadline, not five.
	if took < healthDeadline {
		t.Errorf("`voya health` returned in %s, inside healthDeadline %s: the wedged fixture did not wedge it", took, healthDeadline)
	}
	if took > 3*healthDeadline+shutdownBound/2 {
		t.Errorf("`voya health` took %s against one wedged service, want about healthDeadline %s", took, healthDeadline)
	}
	select {
	case <-wedge.calls:
	default:
		t.Errorf("the wedged fixture never received a Check; whatever timed out, it was not the probe")
	}

	// The cells, beside the entries — the same table as `voya status`, with
	// the seventh field filled.
	header, rows := serviceRows(t, out)
	if !slices.Equal(header, []string{"NAME", "AUTHORITY", "PORT", "PID", "UPTIME", "STREAM", "HEALTH"}) {
		t.Errorf("table header = %q", header)
	}
	want := []struct {
		name   string
		port   uint32
		health string
	}{
		{"gone", gonePort, "unreachable"},
		{"one", onePort, "serving"},
		{"other", otherPort, "not_serving"},
		{"plain", plainPort, "error"},
		{"stuck", stuckPort, "timeout"},
	}
	if len(rows) != len(want) {
		t.Fatalf("services table has %d rows, want %d:\n%s", len(rows), len(want), out)
	}
	for i, w := range want {
		f := rows[i]
		if len(f) != 7 || f[0] != w.name || f[1] != w.name+".internal" || f[2] != strconv.FormatUint(uint64(w.port), 10) ||
			f[5] != "attached" || f[6] != w.health {
			t.Errorf("row %d = %q, want `%s %s.internal %d <pid> <uptime> attached %s`", i, f, w.name, w.name, w.port, w.health)
		}
	}
	// The closed port's detail is voya's diagnostic — the body the proxy
	// wrote, read here rather than through a curl — under the table, under
	// the service's name; the wedged one and the healthy ones have none.
	wantDetail := fmt.Sprintf("\ngone: unreachable\n  voya: \"gone.internal:%d\" is registered on 127.0.0.1:%d but the connection failed: ",
		v.ports.Proxy, gonePort)
	if !strings.Contains(out, wantDetail) {
		t.Errorf("the screen lacks voya's diagnostic for the closed port %q:\n%s", wantDetail, out)
	}
	// The service that serves no health answered the probe with 200 and its
	// path — a service that answers every path — which Connect reads as an
	// error with code unknown; that is a cell of its own, not `unknown`,
	// which is a ServingStatus a service can mean.
	if !strings.Contains(out, "\nplain: error\n  unknown: invalid content-type") {
		t.Errorf("the screen lacks the block for the service that serves no health:\n%s", out)
	}
	for _, name := range []string{"one", "other", "stuck"} {
		if strings.Contains(out, "\n"+name+": ") {
			t.Errorf("%s has a detail block it should not:\n%s", name, out)
		}
	}

	// Nothing was evicted on the result: the wedged service is still
	// registered afterwards, stream attached — a wedged service reporting
	// timeout is the answer, not a reason to deregister it (ADR 004, 005).
	after := startVoyaCmd(t, v, "status")
	if code := after.exitCode(t, "after the probes"); code != 0 {
		t.Fatalf("`voya status` after the probes exited %d\n%s", code, after.Output())
	}
	_, rows = serviceRows(t, after.Stdout.String())
	if len(rows) != 5 || len(rows[4]) != 6 || rows[4][0] != "stuck" || rows[4][5] != "attached" {
		t.Errorf("after the probes the table is not five attached rows with stuck last and HEALTH blank:\n%s", after.Stdout.String())
	}
	if !strings.Contains(after.Stdout.String(), "evictions (0)") {
		t.Errorf("a probe evicted something:\n%s", after.Stdout.String())
	}

	// `voya status -health` is the same screen: the same code path, so the
	// same output but for the clocks — uptimes and the first line's "up".
	sh := startVoyaCmd(t, v, "status", "-health")
	if code := sh.exitCode(t, "against five services"); code != 0 {
		t.Fatalf("`voya status -health` exited %d\n%s", code, sh.Output())
	}
	if got, wantShape := screenShape(t, sh.Stdout.String()), screenShape(t, out); got != wantShape {
		t.Errorf("`voya status -health` and `voya health` differ:\n--- status -health\n%s\n--- health\n%s", got, wantShape)
	}

	// -pxf: the HealthReport, parsed back, in table order, and the closed
	// port's detail in it — on both spellings.
	for _, args := range [][]string{{"health", "-pxf"}, {"status", "-health", "-pxf"}} {
		p := startVoyaCmd(t, v, args[0], args[1:]...)
		if code := p.exitCode(t, "rendering pxf"); code != 0 {
			t.Fatalf("`voya %s` exited %d\n%s", strings.Join(args, " "), code, p.Output())
		}
		if p.Stderr.String() != "" {
			t.Errorf("`voya %s` wrote to stderr: %q", strings.Join(args, " "), p.Stderr.String())
		}
		var back diagv1.HealthReport
		if err := pxf.Unmarshal([]byte(p.Stdout.String()), &back); err != nil {
			t.Fatalf("`voya %s` did not print one HealthReport: %v\n%s", strings.Join(args, " "), err, p.Stdout.String())
		}
		if len(back.GetServices()) != len(want) {
			t.Fatalf("`voya %s` reported %d services, want %d: %v", strings.Join(args, " "), len(back.GetServices()), len(want), &back)
		}
		for i, w := range want {
			sh := back.GetServices()[i]
			if sh.GetService().GetName() != w.name || sh.GetService().GetPort() != w.port || sh.GetStatus() != w.health {
				t.Errorf("`voya %s` services[%d] = %v, want %s on %d %s", strings.Join(args, " "), i, sh, w.name, w.port, w.health)
			}
			if hasDetail := sh.GetDetail() != ""; hasDetail != (w.health == "unreachable" || w.health == "error") {
				t.Errorf("`voya %s` services[%d] detail = %q for a %s service", strings.Join(args, " "), i, sh.GetDetail(), w.health)
			}
		}
		if !strings.HasPrefix(p.Stdout.String(), "services = [") {
			t.Errorf("`voya %s` stdout does not begin with the document: %q", strings.Join(args, " "), p.Stdout.String())
		}
	}
}

// TestHealthWords is the HEALTH column's vocabulary at the seam that decides
// it, over the answers a live fleet cannot all be made to produce at once:
// every ServingStatus including one the enum does not name, the deadline in
// its two shapes, and both sides of `unavailable`.
//
// The paired rows are the point. voya's three unreachable outcomes and a
// service's own 503 arrive as one Connect code, so the code alone cannot say
// which happened; only the `voya: ` prefix can, and a service that answered
// must not read as a port voya could not reach. Deleting the prefix test in
// [healthWords] leaves every other row here green and fails "a service's own
// unavailable" — which is the mutation this case exists to catch.
// `resource_exhausted` is paired for the same reason: voya's own throttle
// and a service's carry that one code too (#233).
func TestHealthWords(t *testing.T) {
	const diagnostic = voyaDiagnostic + `"gone.internal:50001" is registered on 127.0.0.1:50001 but the connection failed: dial tcp: connect: connection refused`
	const throttleDiagnostic = voyaDiagnostic + `"busy.internal" is over its rate limit of 5 requests per 1m0s (rate_limit, ADR 006).`
	for _, tc := range []struct {
		what           string
		res            *grpchealth.CheckResponse
		err            error
		status, detail string
	}{
		{what: "serving", res: &grpchealth.CheckResponse{Status: grpchealth.StatusServing}, status: "serving"},
		{what: "not serving", res: &grpchealth.CheckResponse{Status: grpchealth.StatusNotServing}, status: "not_serving"},
		{what: "unknown", res: &grpchealth.CheckResponse{Status: grpchealth.StatusUnknown}, status: "unknown"},
		{what: "a ServingStatus the enum does not name", res: &grpchealth.CheckResponse{Status: grpchealth.Status(3)}, status: "status_3"},
		{what: "the context deadline", err: context.DeadlineExceeded, status: healthTimeout},
		{what: "the deadline as a Connect code", err: connect.NewError(connect.CodeDeadlineExceeded, errors.New("deadline exceeded")), status: healthTimeout},
		{
			what:   "voya's own unavailable",
			err:    connect.NewError(connect.CodeUnavailable, errors.New(diagnostic)),
			status: healthUnreachable, detail: diagnostic,
		},
		{
			what:   "a service's own unavailable",
			err:    connect.NewError(connect.CodeUnavailable, errors.New("the database is down")),
			status: healthError, detail: "unavailable: the database is down",
		},
		{
			what:   "a service that answers every path",
			err:    connect.NewError(connect.CodeUnknown, errors.New(`invalid content-type "text/plain; charset=utf-8"`)),
			status: healthError, detail: `unknown: invalid content-type "text/plain; charset=utf-8"`,
		},
		{
			what:   "a health server that does not know the empty name",
			err:    connect.NewError(connect.CodeNotFound, errors.New("unknown service")),
			status: healthError, detail: "not_found: unknown service",
		},
		{
			// The service's own throttle: the probe carries
			// proxy.ProbeHeader and spends no budget, so this is what a
			// resource_exhausted reaching the probe means (#233).
			what:   "a service shedding load",
			err:    connect.NewError(connect.CodeResourceExhausted, errors.New("try again later")),
			status: healthThrottled, detail: "resource_exhausted: try again later",
		},
		{
			// The exemption is the daemon's, so a daemon older than it can
			// still throttle a probe — and voya's throttle carries the same
			// Connect code as a service's. Read like `unavailable`: the
			// prefix keeps voya's own refusal off the service's name.
			what:   "voya's own throttle, from a daemon that predates the exemption",
			err:    connect.NewError(connect.CodeResourceExhausted, errors.New(throttleDiagnostic)),
			status: healthError, detail: "resource_exhausted: " + throttleDiagnostic,
		},
	} {
		status, detail := healthWords(tc.res, tc.err)
		if status != tc.status || detail != tc.detail {
			t.Errorf("healthWords for %s = %q / %q, want %q / %q", tc.what, status, detail, tc.status, tc.detail)
		}
	}
}

// notServing is a static health server answering NOT_SERVING for the server
// as a whole — the empty service name, which is what the probe asks about.
func notServing() grpchealth.Checker {
	c := grpchealth.NewStaticChecker()
	c.SetStatus("", grpchealth.StatusNotServing)
	return c
}

// screenShape is a status screen with its clocks removed — the first line
// from ", up " on, and every UPTIME cell — so two renderings of one fleet a
// second apart compare equal.
func screenShape(t *testing.T, out string) string {
	t.Helper()
	lines := strings.Split(out, "\n")
	if len(lines) > 0 {
		lines[0], _, _ = strings.Cut(lines[0], ", up ")
	}
	inTable := false
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "NAME "):
			inTable = true
		case l == "":
			inTable = false
		case inTable:
			f := strings.Fields(l)
			if len(f) >= 5 {
				f[4] = "<uptime>"
			}
			lines[i] = strings.Join(f, " ")
		}
	}
	return strings.Join(lines, "\n")
}

// TestPlainStatusIsNotSlowedByAWedgedService pins ADR 005's reason health is
// opt-in: with a service that accepts and never answers Check registered,
// plain `voya status` runs no probe and stays inside the bound `status`
// promised, while `voya health` on the same fleet — the positive control,
// proving the fixture wedges a probe — takes at least healthDeadline. Both
// medians, over the process floor of `voya version`, the way
// TestVoyaStatusIsInstantWithAFullRegistry measures.
func TestPlainStatusIsNotSlowedByAWedgedService(t *testing.T) {
	skipWithoutSIGTERM(t)
	if testing.Short() {
		t.Skip("runs `voya health` to its deadline several times; skipped under -short")
	}
	v := startVoyaRun(t)
	v.waitUntilUp(t)
	healthService(t, v, "stuck", &wedgedChecker{calls: make(chan struct{}, 64)}, voya.WithPID(4004))
	waitRouted(t, v, "stuck.internal")
	waitForStatusLine(t, v, "services (1)")

	const runs = 5
	median := func(name string, args ...string) time.Duration {
		took := make([]time.Duration, 0, runs)
		for range runs {
			start := time.Now()
			c := startVoyaCmd(t, v, name, args...)
			if code := c.exitCode(t, "against a wedged service"); code != 0 {
				t.Fatalf("`voya %s %s` exited %d\n%s", name, strings.Join(args, " "), code, c.Output())
			}
			took = append(took, time.Since(start))
		}
		slices.Sort(took)
		t.Logf("`voya %s %s` against a wedged service: median %s over %d runs (min %s, max %s)",
			name, strings.Join(args, " "), took[runs/2], runs, took[0], took[runs-1])
		return took[runs/2]
	}
	floor := median("version")
	status := max(median("status")-floor, 0)
	health := max(median("health")-floor, 0)
	t.Logf("own cost over the process floor: status %s, health %s", status, health)
	// Half the deadline, not the deadline: this is a difference of two
	// process-start medians, and the floor's own jitter is tens of
	// milliseconds either way. What is asserted is that the probe was held
	// to its bound, not the bound's exact figure — the cell says "timeout".
	if health < healthDeadline/2 {
		t.Errorf("`voya health` costs %s over the floor against a wedged service; the fixture did not wedge the probe (healthDeadline %s)", health, healthDeadline)
	}
	if status >= healthDeadline/2 {
		t.Errorf("plain `voya status` costs %s over the floor against a wedged service; it is probing (healthDeadline %s)", status, healthDeadline)
	}
}

// TestVoyaHealthProbesSpendNoBudget is #233's first done-when, end to end:
// with the rate limiter on at one request per window and a healthy service
// whose budget is already spent, `voya status -health` still reads `serving`
// — the probe carries voya's marker, spends nothing, and reports the
// service's real state. The 429 before it is the control that the limiter
// was really gating: without it, `serving` would also pass with the limiter
// off, and the test would prove nothing.
func TestVoyaHealthProbesSpendNoBudget(t *testing.T) {
	skipWithoutSIGTERM(t)
	v := startVoyaRun(t, "rate_limit {", "  enabled  = true", "  requests = 1", "  window   = 1m", "}")
	v.waitUntilUp(t)

	healthService(t, v, "payments", grpchealth.NewStaticChecker())
	// waitRouted's own successful warm-up request is the one forwarded call
	// the budget admits — the spend the done-when asks for.
	waitRouted(t, v, "payments.internal")
	if got := proxied(t, v, "payments.internal", "/spent"); got != http.StatusTooManyRequests {
		t.Fatalf("a counted request after the budget was spent answered %d, want 429 — the limiter is not gating, so the probe result below proves nothing", got)
	}

	h := startVoyaCmd(t, v, "status", "-health")
	if code := h.exitCode(t, "with the budget spent"); code != 0 {
		t.Fatalf("`voya status -health` exited %d\n%s", code, h.Output())
	}
	if cells := healthCells(t, h.Stdout.String()); cells["payments"] != "serving" {
		t.Errorf("HEALTH cell for payments = %q, want serving — the probe spent the service's budget, or was throttled by it:\n%s",
			cells["payments"], h.Stdout.String())
	}
}
