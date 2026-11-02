package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/trendvidia/protowire-go/encoding/pxf"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	voya "github.com/trendvidia/voya/client"
	"github.com/trendvidia/voya/internal/config"
	"github.com/trendvidia/voya/internal/daemon"
	"github.com/trendvidia/voya/internal/dnscheck"
	"github.com/trendvidia/voya/internal/registry"
	"github.com/trendvidia/voya/internal/resolverfile"
	diagv1 "github.com/trendvidia/voya/proto/voya/diag/v1"
	"github.com/trendvidia/voya/proto/voya/diag/v1/diagv1connect"
)

// --- #151: voya status ---------------------------------------------------------
//
// A client of DiagService in another process, so the tests that read the
// screen run the real binary twice, the way `voya log`'s do: `voya run` on
// ports of its own, and `voya status …` against the same config directory.
// The two not-a-failure answers are pinned for all three commands at once in
// diag_test.go.

// statusPIDs are the pids the two registrations below carry, so the PID
// column and the eviction record can be asserted on values the test chose
// rather than on os.Getpid() twice — the client sends its own pid unless told
// otherwise, and the two would then be indistinguishable in the output.
const (
	incumbentPID int32 = 4242
	incomingPID  int32 = 4343
)

// TestVoyaStatusListsTheFleet is #151's first criterion: two registered
// services print with the right ports and authorities, voya's own three
// ports match the config, and the columns ADR 005 names are there — stream
// state and the blank HEALTH column beside it. Then -pxf on the same daemon:
// the GetStatusResponse and nothing else on stdout.
func TestVoyaStatusListsTheFleet(t *testing.T) {
	skipWithoutSIGTERM(t)
	v := startVoyaRun(t)
	v.waitUntilUp(t)
	onePort := echoService(t, v, "one", voya.WithPID(incumbentPID))
	otherPort := echoService(t, v, "other", voya.WithPID(incomingPID))
	waitRouted(t, v, "one.internal")
	waitRouted(t, v, "other.internal")

	s := startVoyaCmd(t, v, "status")
	if code := s.exitCode(t, "against two services"); code != 0 {
		t.Fatalf("`voya status` exited %d\n%s", code, s.Output())
	}
	if s.Stderr.String() != "" {
		t.Errorf("`voya status` wrote to stderr: %q", s.Stderr.String())
	}
	out := s.Stdout.String()
	lines := s.lines()
	if len(lines) < 4 {
		t.Fatalf("`voya status` printed too little to be the screen:\n%s", out)
	}

	// voya itself: the daemon's pid, and the three ports it was started on.
	if !strings.HasPrefix(lines[0], "voya ") || !strings.Contains(lines[0], fmt.Sprintf(", pid %d, up ", v.PID())) {
		t.Errorf("first line = %q, want voya's version, the daemon's pid %d and its uptime", lines[0], v.PID())
	}
	wantPorts := fmt.Sprintf("proxy 127.0.0.1:%d  dns 127.0.0.1:%d  registration 127.0.0.1:%d",
		v.ports.Proxy, v.ports.DNS, v.ports.Registration)
	if strings.TrimSpace(lines[1]) != wantPorts {
		t.Errorf("second line = %q, want %q", lines[1], wantPorts)
	}
	// The resolver file: said on the OS that reads it, with the state this
	// process measures the same way, and not said anywhere else (#96, #104).
	resolver := strings.Contains(out, "resolver file "+resolverfile.Path)
	// Re-derived rather than taken from the command's own gate: the same two
	// questions #155 makes it ask, put to this host directly, so a host whose
	// answer differs from the screen's is a failure and not a tautology.
	here, hasWiring := resolverfile.For(runtime.GOOS)
	if reads := hasWiring && here.Live(context.Background()); resolver != reads {
		t.Errorf("the screen mentions the resolver file: %v; this OS reads it: %v\n%s", resolver, reads, out)
	} else if reads {
		installed, err := dnscheck.ResolverInstalled()
		want := "not installed"
		switch {
		case err != nil:
			want = "unknown"
		case installed:
			want = ": installed"
		}
		if !strings.Contains(out, want) {
			t.Errorf("the resolver line does not say %q, which is what this process measured:\n%s", want, out)
		}
	}

	// The services table: header, then one row each, in name order, with the
	// port each registered, the pid it carried, the stream state, and the
	// blank HEALTH cell — six fields, not seven.
	header := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "NAME ") })
	if header < 0 || len(lines) < header+3 {
		t.Fatalf("no services table with two rows:\n%s", out)
	}
	if got := strings.Fields(lines[header]); !slices.Equal(got, []string{"NAME", "AUTHORITY", "PORT", "PID", "UPTIME", "STREAM", "HEALTH"}) {
		t.Errorf("table header = %q", lines[header])
	}
	if !strings.Contains(out, "services (2)") {
		t.Errorf("no `services (2)` line:\n%s", out)
	}
	rows := lines[header+1 : header+3]
	for i, want := range []struct {
		name string
		port uint32
		pid  int32
	}{{"one", onePort, incumbentPID}, {"other", otherPort, incomingPID}} {
		f := strings.Fields(rows[i])
		if len(f) != 6 || f[0] != want.name || f[1] != want.name+".internal" ||
			f[2] != strconv.FormatUint(uint64(want.port), 10) || f[3] != strconv.FormatInt(int64(want.pid), 10) ||
			f[5] != "attached" {
			t.Errorf("row %d = %q, want `%s %s.internal %d %d <uptime> attached` and a blank HEALTH cell",
				i, rows[i], want.name, want.name, want.port, want.pid)
		}
		if len(f) == 6 {
			if _, err := time.ParseDuration(f[4]); err != nil {
				t.Errorf("row %d's uptime %q is not a duration: %v", i, f[4], err)
			}
		}
	}
	if !strings.Contains(out, "evictions (0)") || !strings.Contains(out, "reclaims (0)") {
		t.Errorf("a fleet nobody deposed should show zero evictions and reclaims:\n%s", out)
	}

	// -pxf: the document, parsed back, and nothing else on either stream.
	p := startVoyaCmd(t, v, "status", "-pxf")
	if code := p.exitCode(t, "rendering pxf"); code != 0 {
		t.Fatalf("`voya status -pxf` exited %d\n%s", code, p.Output())
	}
	if p.Stderr.String() != "" {
		t.Errorf("`voya status -pxf` wrote to stderr: %q", p.Stderr.String())
	}
	var back diagv1.GetStatusResponse
	if err := pxf.Unmarshal([]byte(p.Stdout.String()), &back); err != nil {
		t.Fatalf("`voya status -pxf` did not print one GetStatusResponse: %v\n%s", err, p.Stdout.String())
	}
	if back.GetSelf().GetProxyPort() != v.ports.Proxy || len(back.GetServices()) != 2 ||
		back.GetServices()[0].GetPort() != onePort || back.GetServices()[1].GetPort() != otherPort {
		t.Errorf("parsed back %v, want proxy %d and the two services on %d and %d", &back, v.ports.Proxy, onePort, otherPort)
	}
	if !strings.HasPrefix(p.Stdout.String(), "self {") {
		t.Errorf("`voya status -pxf` stdout does not begin with the document: %q", p.Stdout.String())
	}
}

// waitUntilProxyAccepts blocks until the proxy port takes a connection, for
// the one daemon whose banner is silenced. It establishes the proxy bind and
// nothing more — [voyaRun.waitUntilUp] says why that is not "up" — so a
// caller follows it with the wait that is about the listener it needs.
func (v *voyaRun) waitUntilProxyAccepts(t *testing.T) {
	t.Helper()
	addr := "127.0.0.1:" + strconv.FormatUint(uint64(v.ports.Proxy), 10)
	deadline := time.Now().Add(startupBound)
	for {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			return
		}
		v.FailIfExited(t, "while its proxy port was awaited")
		if time.Now().After(deadline) {
			t.Fatalf("the proxy port %s never accepted a connection: %v\n%s", addr, err, v.Output())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// diagClientFor is a DiagService client aimed at v's proxy port under the
// reserved authority — what [dialDiag] builds from a config directory, built
// here from the ports the test already knows — for a wait that reads the
// daemon in process rather than by spawning a command at it (#164).
func diagClientFor(t *testing.T, v *voyaRun) diagv1connect.DiagServiceClient {
	t.Helper()
	port := strconv.FormatUint(uint64(v.ports.Proxy), 10)
	addr := net.JoinHostPort(config.Loopback, port)
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	tr := &http.Transport{
		Protocols: protocols,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
	return diagv1connect.NewDiagServiceClient(&http.Client{Transport: tr},
		"http://"+registry.Authority(registry.Reserved)+":"+port)
}

// waitForStatusLine waits until the daemon's status screen carries want, or
// fails after startupBound — the registration and the eviction it records
// land asynchronously, and the screen is the reader this test is about —
// then spawns `voya status` once and returns it, so the caller asserts on
// what the command printed.
//
// The wait polls GetStatus in process and renders the answer with the same
// [renderStatus] the command uses, rather than spawning `voya status` round
// after round: under the race detector every re-exec of this binary costs
// about a second on darwin/arm64 before main runs — the race runtime's own
// start-up, measured against an empty main — so a wait by spawn paid that
// floor per poll (#164). One spawn remains, the one whose output is returned.
//
// That spawn reads the daemon a second later than the poll did, and the state
// can move in between — a want that counts something ("services (5)") stops
// holding if a stream drops. So a command that does not show want is checked
// against the daemon once more: still showing it, the command is wrong and
// the test fails on it; no longer showing it, the wait resumes, which is the
// tolerance the spawn-per-round loop had for free.
func waitForStatusLine(t *testing.T, v *voyaRun, want string) *voyaCmd {
	t.Helper()
	client := diagClientFor(t, v)
	screen := func() (string, error) {
		ctx, cancel := context.WithTimeout(t.Context(), diagDeadline)
		defer cancel()
		res, err := client.GetStatus(ctx, connect.NewRequest(&diagv1.GetStatusRequest{}))
		if err != nil {
			return "", err
		}
		var b bytes.Buffer
		renderStatus(&b, res.Msg, time.Now(), "", nil)
		return b.String(), nil
	}
	deadline := time.Now().Add(startupBound)
	for {
		rendered, err := screen()
		if err == nil && strings.Contains(rendered, want) {
			s := startVoyaCmd(t, v, "status")
			if code := s.exitCode(t, "after the daemon showed "+want); code != 0 {
				t.Fatalf("`voya status` exited %d after GetStatus showed %q\n%s", code, want, s.Output())
			}
			if strings.Contains(s.Stdout.String(), want) {
				return s
			}
			if again, err := screen(); err == nil && strings.Contains(again, want) {
				t.Fatalf("`voya status` does not show %q, which GetStatus carries either side of the spawn\n%s", want, s.Output())
			}
		}
		v.FailIfExited(t, "while its status was awaited for "+want)
		if time.Now().After(deadline) {
			t.Fatalf("the daemon's status never showed %q (last GetStatus error: %v)\n%s", want, err, rendered)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestVoyaStatusRecordsTheEvictionAfterTheFact is #151's second criterion and
// ADR 002's #43 amendment: with `log_level = LOG_LEVEL_ERROR` on the daemon —
// the level that silences the warning — deposing an instance still puts the
// eviction on the screen, both instances named. The setup is asserted: the
// daemon's terminal never showed the warning, so the screen is the only
// record. (That the warning does print at the default level is the
// acceptance suite's TestSecondInstanceEvictsTheFirstLoudly.)
func TestVoyaStatusRecordsTheEvictionAfterTheFact(t *testing.T) {
	skipWithoutSIGTERM(t)
	v := startVoyaRun(t, "log_level = LOG_LEVEL_ERROR")
	// No banner to wait on: LOG_LEVEL_ERROR silences the startup line along
	// with the warning. "Up" here is the thing this test needs and not a
	// proxy for it — the incumbent registered and routed to, which is the
	// registration listener bound and the stream accepted; both waits fail
	// on a daemon that exited instead. Nothing below needs the DNS listener,
	// the last bind, and the cleanup kills rather than asks.
	v.waitUntilProxyAccepts(t)
	incumbent := echoService(t, v, "other", voya.WithPID(incumbentPID))
	waitRouted(t, v, "other.internal")
	incoming := echoService(t, v, "other", voya.WithPID(incomingPID))

	s := waitForStatusLine(t, v, "evictions (1)")
	out := s.Stdout.String()
	want := fmt.Sprintf("other.internal: port %d, pid %d was evicted by port %d, pid %d",
		incumbent, incumbentPID, incoming, incomingPID)
	if !strings.Contains(out, want) {
		t.Errorf("the eviction record does not name both instances; want %q in:\n%s", want, out)
	}
	// The record's line carries when, first.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "was evicted by") {
			at, _, _ := strings.Cut(line, "  ")
			if _, err := time.Parse(statusTimeFormat, at); err != nil {
				t.Errorf("the eviction line does not start with a timestamp: %q (%v)", line, err)
			}
		}
	}
	// The services table lists the incoming instance and not the deposed
	// one: presence is liveness.
	if !strings.Contains(out, "services (1)") || !strings.Contains(out, " "+strconv.FormatUint(uint64(incoming), 10)+" ") {
		t.Errorf("after the eviction the table should hold the incoming instance on %d alone:\n%s", incoming, out)
	}
	if strings.Contains(out, " "+strconv.FormatUint(uint64(incumbent), 10)+"  "+strconv.FormatInt(int64(incumbentPID), 10)) {
		t.Errorf("the deposed instance on %d is still in the services table:\n%s", incumbent, out)
	}
	// The setup: at LOG_LEVEL_ERROR the daemon printed no warning, so the
	// screen is the record.
	if strings.Contains(v.Stderr.String(), daemon.AnnouncementMarker) || strings.Contains(v.Stderr.String(), "was taken over") {
		t.Fatalf("the daemon printed the eviction warning at LOG_LEVEL_ERROR, so this test proves nothing about the record:\n%s",
			v.Stderr.String())
	}
}

// registerOnly joins name with a bound listener nothing serves behind: enough
// for the registry to hold it, which is all a status screen reads.
func registerOnly(t *testing.T, v *voyaRun, name string, pid int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	stop, err := voya.Join(t.Context(), name, ln.Addr(),
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
}

// fullRegistry is how many services the wall-time measurement runs against:
// registry.maxSeen, the cap on the names the registry remembers — restated
// here because the constant is the registry's own. Nothing caps the services
// table itself, so this is a fleet far past what a laptop runs rather than a
// ceiling the screen cannot exceed; the measurement wants a full one, not the
// widest possible one.
const fullRegistry = 128

// TestVoyaStatusIsInstantWithAFullRegistry is #151's measurement: against a
// daemon holding a full registry, the command's wall time — process start to
// exit, the way a shell sees it — has a median well inside diagDeadline. The
// median is the gate (a busy machine widens the spread; only the median is
// about the code), and it is logged so a PR can state it.
//
// The gate is on the command's own cost over the process floor: `voya
// version` spawned the same way, which loads no config and dials nothing.
// The floor is not the code under test — under the race detector `make test`
// runs with, this binary's own start and exit come to about a second on
// macOS, against milliseconds without it — and diagDeadline bounds the call,
// not the exec. Both medians are logged.
func TestVoyaStatusIsInstantWithAFullRegistry(t *testing.T) {
	skipWithoutSIGTERM(t)
	if testing.Short() {
		t.Skip("registers 128 services; skipped under -short")
	}
	v := startVoyaRun(t)
	v.waitUntilUp(t)
	for i := range fullRegistry {
		registerOnly(t, v, fmt.Sprintf("svc%03d", i), int32(1000+i))
	}
	waitForStatusLine(t, v, fmt.Sprintf("services (%d)", fullRegistry))

	const runs = 11
	median := func(name string, check func(*voyaCmd)) time.Duration {
		took := make([]time.Duration, 0, runs)
		for range runs {
			start := time.Now()
			c := startVoyaCmd(t, v, name)
			if code := c.exitCode(t, "against a full registry"); code != 0 {
				t.Fatalf("`voya %s` exited %d\n%s", name, code, c.Output())
			}
			took = append(took, time.Since(start))
			check(c)
		}
		slices.Sort(took)
		t.Logf("`voya %s` against %d services: median %s over %d runs (min %s, max %s)",
			name, fullRegistry, took[runs/2], runs, took[0], took[runs-1])
		return took[runs/2]
	}
	floor := median("version", func(*voyaCmd) {})
	status := median("status", func(c *voyaCmd) {
		if !strings.Contains(c.Stdout.String(), fmt.Sprintf("services (%d)", fullRegistry)) {
			t.Fatalf("`voya status` lost services mid-measurement:\n%s", c.Stdout.String())
		}
	})
	own := max(status-floor, 0)
	t.Logf("`voya status`'s own cost over the process floor: %s", own)
	if own >= diagDeadline/2 {
		t.Errorf("`voya status` costs %s over the process floor; not well inside diagDeadline %s", own, diagDeadline)
	}
}

// TestRenderStatusScreen drives the rendering in process on a fixed answer
// and a fixed clock, for the cells a live daemon cannot hold still: the
// uptime arithmetic, a registration with no pid, a reclaim with and without
// its eviction time, the empty table, and the resolver line's presence being
// the caller's call.
func TestRenderStatusScreen(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	at := func(ago time.Duration) *timestamppb.Timestamp { return timestamppb.New(now.Add(-ago)) }
	res := &diagv1.GetStatusResponse{
		Self: &diagv1.Self{Pid: 7, StartedAt: at(90 * time.Second), ProxyPort: 9777, DnsPort: 9778, RegistrationPort: 9779, Version: "v1.2.3"},
		Services: []*diagv1.Service{
			{Name: "one", Authority: "one.internal", Port: 50001, Pid: 11, Since: at(30 * time.Second), Stream: diagv1.StreamState_STREAM_STATE_ATTACHED},
			{Name: "two", Authority: "two.internal", Port: 50002, Since: at(0)},
		},
		Evictions: []*diagv1.Eviction{{
			Evicted: &diagv1.Instance{Name: "one", Port: 40001, Pid: 10}, Incoming: &diagv1.Instance{Name: "one", Port: 50001, Pid: 11}, At: at(60 * time.Second),
		}},
		Reclaims: []*diagv1.Reclaim{
			{Instance: &diagv1.Instance{Name: "two", Port: 50002}, EvictedAt: at(45 * time.Second), At: at(15 * time.Second)},
			{Instance: &diagv1.Instance{Name: "two", Port: 50002}, At: at(5 * time.Second)},
		},
	}
	var b bytes.Buffer
	renderStatus(&b, res, now, "resolver file /etc/resolver/internal: installed", nil)
	got := b.String()
	for _, want := range []string{
		"voya v1.2.3, pid 7, up 1m30s since ",
		"  proxy 127.0.0.1:9777  dns 127.0.0.1:9778  registration 127.0.0.1:9779\n",
		"  resolver file /etc/resolver/internal: installed\n",
		"services (2)\n",
		"NAME  AUTHORITY     PORT   PID  UPTIME  STREAM       HEALTH\n",
		"one   one.internal  50001  11   30s     attached\n",
		"two   two.internal  50002  -    0s      unspecified\n",
		"evictions (1)\n",
		"  one.internal: port 40001, pid 10 was evicted by port 50001, pid 11\n",
		"reclaims (2)\n",
		"  two.internal: port 50002 took its name back, 30s after losing it\n",
		"  two.internal: port 50002 took its name back\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the screen lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("a doubled blank line:\n%s", got)
	}
	// #164: the blank HEALTH column does not leave the STREAM column's pad
	// trailing on every row — `voya status | grep -c ' $'` prints 0 — and the
	// header is still aligned over it, which the exact rows above pin.
	if strings.Contains(got, " \n") {
		t.Errorf("a line ends in spaces:\n%q", got)
	}

	// No resolver line, an empty registry, and a daemon that set no started_at.
	b.Reset()
	renderStatus(&b, &diagv1.GetStatusResponse{Self: &diagv1.Self{Pid: 7}}, now, "", nil)
	got = b.String()
	if strings.Contains(got, "resolver") {
		t.Errorf("the screen mentions the resolver file when told not to:\n%s", got)
	}
	for _, want := range []string{"voya , pid 7, up - since -\n", "services (0)\nno services registered\n", "evictions (0)\n", "reclaims (0)\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("the empty screen lacks %q:\n%s", want, got)
		}
	}
	// A clock that stepped backwards reads 0s, not a negative uptime.
	if got := uptime(now, timestamppb.New(now.Add(time.Minute))); got != "0s" {
		t.Errorf("uptime under a clock step = %q, want 0s", got)
	}

	// With a report: the HEALTH cell filled per row, in the table, and every
	// probe with a detail explained under the table, indented under its
	// name; a probe without one is a cell and nothing more (#154). The
	// detail is a service's own text, so it carries whitespace voya did not
	// put there — a whitespace-only line and a line with a trailing pad,
	// both of which reach the screen trimmed (#164).
	b.Reset()
	renderStatus(&b, res, now, "", &diagv1.HealthReport{Services: []*diagv1.ServiceHealth{
		{Service: res.Services[0], Status: "serving"},
		{Service: res.Services[1], Status: "unreachable", Detail: "voya: \"two.internal\" is registered on 127.0.0.1:50002 but the connection failed: dial tcp: connection refused\n   \nRegistered services: one.internal, two.internal  \n"},
	}})
	got = b.String()
	for _, want := range []string{
		"NAME  AUTHORITY     PORT   PID  UPTIME  STREAM       HEALTH\n",
		"one   one.internal  50001  11   30s     attached     serving\n",
		"two   two.internal  50002  -    0s      unspecified  unreachable\n",
		"\ntwo: unreachable\n  voya: \"two.internal\" is registered on 127.0.0.1:50002 but the connection failed: dial tcp: connection refused\n\n  Registered services: one.internal, two.internal\n\nevictions (1)\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the screen with a report lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "one: serving") {
		t.Errorf("a probe with no detail got a block under the table:\n%s", got)
	}
	if strings.Contains(got, " \n") {
		t.Errorf("a line of the screen with a report ends in spaces:\n%q", got)
	}
}

// TestResolverLine drives [resolverLine] through the two seams dnscheck.go
// declares, because which of its answers the host can produce is otherwise
// the test machine's to decide: the end-to-end screen above sees one row of
// this table — whichever this OS and this filesystem happen to give — and the
// line's three claims are that it says nothing off the OS that reads the
// file, that it distinguishes installed from not, and that a stat which is
// neither presence nor absence says so rather than reading as missing.
func TestResolverLine(t *testing.T) {
	statFailed := errors.New("stat " + resolverfile.Path + ": permission denied")
	for _, tc := range []struct {
		name      string
		reads     bool
		installed func() (bool, error)
		want      string
	}{
		{"off the reading OS", false, func() (bool, error) { return true, nil }, ""},
		{"installed", true, func() (bool, error) { return true, nil },
			"resolver file " + resolverfile.Path + ": installed"},
		{"not installed", true, func() (bool, error) { return false, nil },
			"resolver file " + resolverfile.Path + ": not installed"},
		{"unreadable", true, func() (bool, error) { return false, statFailed },
			"resolver file " + resolverfile.Path + ": unknown (" + statFailed.Error() + ")"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			consulted := false
			substituteResolverFileSeams(t, tc.reads, func() (bool, error) {
				consulted = true
				return tc.installed()
			})
			if got := resolverLine(context.Background()); got != tc.want {
				t.Errorf("resolverLine() = %q, want %q", got, tc.want)
			}
			// Off the reading OS the file is inert (#96), so the stat is not
			// made at all — the same gate dnsCheckCmd is held to.
			if consulted == !tc.reads {
				t.Errorf("resolverLine() consulted %s: %v, on an OS that reads it: %v",
					resolverfile.Path, consulted, tc.reads)
			}
		})
	}
}

// TestUsageListsStatusAndRoutesAsBuilt: the two rows are built — and `health`
// since #154 — so usage lists them under Commands. The "not under Not
// implemented yet" half retired with the block itself (#210);
// TestEveryCommandIsBuilt in main_test.go pins the block gone.
func TestUsageListsStatusAndRoutesAsBuilt(t *testing.T) {
	var stdout bytes.Buffer
	if err := run([]string{"help"}, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"status", "routes", "health"} {
		if !listsCommand(stdout.String(), name) {
			t.Errorf("usage does not list `%s` under Commands:\n%s", name, stdout.String())
		}
	}
}

// TestStatusPXFOmitsTheResolverLineAndSaysSo is #166's decision, held in
// the three places it lives: the schema `voya status -pxf` renders carries no
// field for the resolver file — walking every message reachable from either
// document, so a field added under Self, under a probe's report or in a new
// sub-message is seen — and both the usage text and ADR 005 say the document
// omits it and why.
//
// The two texts are held to each other by phrase and not by name, unlike
// readme_test's extractions, because there is no identifier here to key on:
// what #166 decided is wording, so wording is what a reader of one place has
// to find in the other. Both sides are collapsed to single-spaced words
// first, so a re-wrap of either paragraph is not a failure and only the words
// are; the usage's own reason is then checked against the ADR's, which is the
// half that would silently drift.
func TestStatusPXFOmitsTheResolverLineAndSaysSo(t *testing.T) {
	// unwrap makes a comparison indifferent to where the lines break: these
	// are prose paragraphs in a Go string and in Markdown, wrapped to
	// different widths and re-wrapped whenever either is edited.
	unwrap := func(s string) string { return strings.Join(strings.Fields(s), " ") }

	// The schema: no field under either document names the resolver file or
	// the state of DNS wiring on the host. Self.dns_port is the daemon's own
	// listener and not that, so the words are matched and not "dns".
	seen := map[protoreflect.FullName]bool{}
	var walk func(md protoreflect.MessageDescriptor)
	walk = func(md protoreflect.MessageDescriptor) {
		if seen[md.FullName()] {
			return
		}
		seen[md.FullName()] = true
		fields := md.Fields()
		for i := range fields.Len() {
			f := fields.Get(i)
			name := strings.ToLower(string(f.Name()))
			for _, word := range []string{"resolver", "resolv_conf", "etc_hosts", "dns_installed"} {
				if strings.Contains(name, word) {
					t.Errorf("%s.%s: the status document carries the resolver file, which #166 decided it does not", md.FullName(), f.Name())
				}
			}
			if f.Message() != nil {
				walk(f.Message())
			}
		}
	}
	// Both documents the two commands render under -pxf: the status response,
	// and the health report `voya status -pxf -health` prints in its place.
	walk((&diagv1.GetStatusResponse{}).ProtoReflect().Descriptor())
	walk((&diagv1.HealthReport{}).ProtoReflect().Descriptor())
	// The setup, asserted: a walk that reached only the two roots — or
	// nothing at all — would satisfy the check above without looking at the
	// messages a resolver field would be added to.
	for _, name := range []protoreflect.FullName{"voya.diag.v1.Self", "voya.diag.v1.ServiceHealth"} {
		if !seen[name] {
			t.Errorf("the walk never reached %s, so the schema check proves nothing (reached %d messages)", name, len(seen))
		}
	}

	// The usage: says the document omits the line, and where the answer is.
	var usage bytes.Buffer
	statusUsage(&usage)
	const omits = "The document omits the resolver file line"
	if !strings.Contains(unwrap(usage.String()), omits) || !strings.Contains(usage.String(), `"voya dns check"`) {
		t.Fatalf("statusUsage does not say the pxf document omits the resolver line and to read `voya dns check`:\n%s", usage.String())
	}

	// The ADR: the same fact, in the same words, and the same reason.
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "adr", "005-observability.md"))
	if err != nil {
		t.Fatalf("reading ADR 005: %v", err)
	}
	adr := unwrap(string(raw))
	for _, want := range []string{
		"omits the resolver line",
		"a property of the host running the CLI, not of the daemon",
		"`voya dns check`'s exit code",
	} {
		if !strings.Contains(adr, want) {
			t.Errorf("ADR 005 does not say %q about `voya status -pxf`", want)
		}
	}
	// And the usage gives the reason in the ADR's words, wrapped its own way.
	if !strings.Contains(unwrap(usage.String()), "a property of the host running this command, not of the daemon") {
		t.Errorf("statusUsage does not give the ADR's reason:\n%s", usage.String())
	}
}
