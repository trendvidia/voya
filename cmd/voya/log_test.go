package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/trendvidia/protowire-go/encoding/pxf"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	voya "github.com/trendvidia/voya/client"
	"github.com/trendvidia/voya/internal/auth"
	"github.com/trendvidia/voya/internal/config"
	"github.com/trendvidia/voya/internal/requestlog"
	"github.com/trendvidia/voya/internal/testproc"
	diagv1 "github.com/trendvidia/voya/proto/voya/diag/v1"
	"github.com/trendvidia/voya/proto/voya/diag/v1/diagv1connect"
)

// --- #159: voya log ------------------------------------------------------------
//
// The command is a client of DiagService in another process, so every test
// here runs the real binary twice: `voya run` on ports of its own, and `voya
// log …` pointed at the same config directory. Both are this test binary
// re-executed through [beVoya], on their real streams — which of the two a
// line arrives on is part of what is asserted.

// voyaCmd is a `voya <command> …` in a process of its own, against a
// [voyaRun]'s daemon: `log`, `status` and `routes` all start this way, and
// [TestDiagCommandsWhenVoyaIsNotRunning] drives all three through it.
type voyaCmd struct {
	*testproc.Child
	name string
}

// startVoyaCmd re-executes this test binary as `voya name args...` against
// v's daemon. The child is started, not waited for: a snapshot exits on its
// own, a follow does not.
func startVoyaCmd(t *testing.T, v *voyaRun, name string, args ...string) *voyaCmd {
	t.Helper()
	return &voyaCmd{testproc.Start(t.Context(), t, "voya "+name,
		beVoyaArgs(append([]string{name}, args...)...),
		config.DirEnv+"="+v.configDir,
	), name}
}

// startVoyaLog is [startVoyaCmd] for `voya log`.
func startVoyaLog(t *testing.T, v *voyaRun, args ...string) *voyaCmd {
	t.Helper()
	return startVoyaCmd(t, v, "log", args...)
}

// exitCode waits for the child to be gone and reports its exit code — 0 for
// a clean exit — with what it wrote, for the failure message.
func (l *voyaCmd) exitCode(t *testing.T, after string) int {
	t.Helper()
	select {
	case <-l.Exited():
	case <-time.After(shutdownBound):
		t.Fatalf("`voya %s` was still running %s %s\n%s", l.name, shutdownBound, after, l.Output())
	}
	err := l.WaitErr()
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	t.Fatalf("waiting for `voya %s` %s: %v\n%s", l.name, after, err, l.Output())
	return -1
}

// lines is the child's stdout, one entry per non-empty line.
func (l *voyaCmd) lines() []string {
	var out []string
	for _, line := range strings.Split(l.Stdout.String(), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// echoService is a registered service for the proxy to route to: an h2c
// server answering 200 with its path, joined under name through the real
// client package with extra appended to the options, and gone when the test
// ends. It serves no grpc.health.v1 — [healthService] in health_test.go is
// the fixture that does.
func echoService(t *testing.T, v *voyaRun, name string, extra ...voya.Option) (port uint32) {
	t.Helper()
	return serveAndJoin(t, v, name, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.URL.Path)
	}), extra...)
}

// serveAndJoin is [echoService] for any handler: an h2c server on a port of
// its own, serving h, joined under name and gone when the test ends.
func serveAndJoin(t *testing.T, v *voyaRun, name string, h http.Handler, extra ...voya.Option) (port uint32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{
		Protocols:         protocols,
		ReadHeaderTimeout: 10 * time.Second,
		Handler:           h,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	stop, err := voya.Join(context.Background(), name, ln.Addr(),
		append([]voya.Option{
			voya.WithRegistrationPort(v.ports.Registration),
			voya.WithProxyPort(v.ports.Proxy),
			voya.WithoutDialer(),
			voya.WithBackoff(10*time.Millisecond, 200*time.Millisecond),
			voya.WithLogf(func(format string, args ...any) { t.Logf(name+": "+format, args...) }),
		}, extra...)...,
	)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	t.Cleanup(stop)
	tcp, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is %T", ln.Addr())
	}
	return uint32(tcp.Port)
}

// proxied makes one request through v's proxy for authority and path, and
// returns the status. The registration is asynchronous, so a caller wanting a
// routed answer polls this until it gets one.
func proxied(t *testing.T, v *voyaRun, authority, path string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://127.0.0.1:"+strconv.FormatUint(uint64(v.ports.Proxy), 10)+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = authority
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s%s: %v", authority, path, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// waitRouted drives requests at name until the proxy routes one — the
// registration having landed — so what follows is counted, not guessed.
func waitRouted(t *testing.T, v *voyaRun, authority string) {
	t.Helper()
	deadline := time.Now().Add(startupBound)
	for proxied(t, v, authority, "/warmup") != http.StatusOK {
		v.FailIfExited(t, "while a registration was awaited")
		if time.Now().After(deadline) {
			t.Fatalf("%s was never routed to", authority)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestVoyaLogPrintsTheSnapshotInOrder is #159's first criterion: three
// proxied requests, then `voya log` prints three lines in order with
// authority, method, path, status, duration and port — the daemon's own live
// line, verbatim — and `-n 2` prints the last two. Plus the empty case, on
// the same daemon before anything is proxied.
func TestVoyaLogPrintsTheSnapshotInOrder(t *testing.T) {
	skipWithoutSIGTERM(t)
	v := startVoyaRun(t)
	v.waitUntilUp(t)

	empty := startVoyaLog(t, v)
	if code := empty.exitCode(t, "on an empty log"); code != 0 {
		t.Fatalf("`voya log` on an empty log exited %d\n%s", code, empty.Output())
	}
	if got := strings.TrimSpace(empty.Stdout.String()); got != "no requests yet" {
		t.Errorf("`voya log` on an empty log printed %q, want \"no requests yet\"", got)
	}

	port := echoService(t, v, "other")
	waitRouted(t, v, "other.internal")
	// The warm-up requests are in the log too; the three counted come after
	// them and are told apart by path.
	for _, path := range []string{"/one", "/two", "/three"} {
		if got := proxied(t, v, "other.internal", path); got != http.StatusOK {
			t.Fatalf("GET %s answered %d", path, got)
		}
	}
	// One voya answered itself, so the port column's other rendering is in
	// the snapshot too.
	if got := proxied(t, v, "nobody.internal", "/four"); got != http.StatusNotFound {
		t.Fatalf("GET nobody.internal/four answered %d", got)
	}

	all := startVoyaLog(t, v)
	if code := all.exitCode(t, "printing the snapshot"); code != 0 {
		t.Fatalf("`voya log` exited %d\n%s", code, all.Output())
	}
	lines := all.lines()
	var counted []string
	for _, line := range lines {
		if !strings.Contains(line, "/warmup") {
			counted = append(counted, line)
		}
	}
	if len(counted) != 4 {
		t.Fatalf("`voya log` printed %d lines besides the warm-ups, want four:\n%s", len(counted), all.Stdout.String())
	}
	for i, want := range []string{
		" other.internal GET /one 200 ",
		" other.internal GET /two 200 ",
		" other.internal GET /three 200 ",
		" nobody.internal GET /four 404 ",
	} {
		if !strings.Contains(counted[i], want) {
			t.Errorf("line %d = %q, want it to carry %q", i, counted[i], want)
		}
	}
	// The whole shape: timestamp, then the fields, then the duration and the
	// port — the daemon's live line, which is on its stderr, verbatim.
	for _, line := range counted {
		if !v.Wrote(line) {
			t.Errorf("`voya log` printed %q, which the daemon's own terminal never showed; the two forms should read the same:\n%s",
				line, v.Stderr.String())
		}
	}
	if !strings.HasSuffix(counted[0], " -> "+strconv.FormatUint(uint64(port), 10)) {
		t.Errorf("a routed line does not end in the upstream port %d: %q", port, counted[0])
	}
	if !strings.HasSuffix(counted[3], " -> voya") {
		t.Errorf("a line voya answered itself does not end in `-> voya`: %q", counted[3])
	}
	if !strings.Contains(counted[0], "µs -> ") && !strings.Contains(counted[0], "ms -> ") {
		t.Errorf("no duration before the port column: %q", counted[0])
	}
	if all.Stderr.String() != "" {
		t.Errorf("`voya log` wrote to stderr: %q", all.Stderr.String())
	}

	// -n 2: the last two, in order.
	two := startVoyaLog(t, v, "-n", "2")
	if code := two.exitCode(t, "printing the last two"); code != 0 {
		t.Fatalf("`voya log -n 2` exited %d\n%s", code, two.Output())
	}
	got := two.lines()
	if len(got) != 2 || got[0] != counted[2] || got[1] != counted[3] {
		t.Errorf("`voya log -n 2` printed:\n%s\nwant the last two of:\n%s", two.Stdout.String(), all.Stdout.String())
	}
}

// followerAttached drives requests at v until the follower prints one:
// WatchRequests never replays, so a line proves the watcher is registered,
// and only then are the requests a test counts made. Returns the paths of
// the probes it made, so a caller can tell them from the counted lines.
func followerAttached(t *testing.T, v *voyaRun, f *voyaCmd) {
	t.Helper()
	deadline := time.Now().Add(startupBound)
	for i := 0; ; i++ {
		f.FailIfExited(t, "while it was being attached")
		v.FailIfExited(t, "while a follower was being attached")
		proxied(t, v, "nobody.internal", "/probe/"+strconv.Itoa(i))
		time.Sleep(20 * time.Millisecond)
		if strings.Contains(f.Stdout.String(), "/probe/") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("`voya log -f` never printed a probe; the follower did not attach\n%s", f.Output())
		}
	}
}

// waitForLine polls the follower's stdout for want, failing if the follower
// or the daemon exits first.
func waitForLine(t *testing.T, v *voyaRun, f *voyaCmd, want string) {
	t.Helper()
	deadline := time.Now().Add(shutdownBound)
	for !strings.Contains(f.Stdout.String(), want) {
		f.FailIfExited(t, "before it printed "+want)
		v.FailIfExited(t, "while a follower waited for "+want)
		if time.Now().After(deadline) {
			t.Fatalf("`voya log -f` did not print %q within %s\n%s", want, shutdownBound, f.Output())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestVoyaLogFollowPrintsRequestsAsTheyHappen: `voya log -f` running when
// two more requests happen prints both, in order, and SIGINT stops it with
// exit 0 and nothing on stderr.
func TestVoyaLogFollowPrintsRequestsAsTheyHappen(t *testing.T) {
	skipWithoutSIGTERM(t)
	v := startVoyaRun(t)
	v.waitUntilUp(t)
	echoService(t, v, "other")
	waitRouted(t, v, "other.internal")

	f := startVoyaLog(t, v, "-f")
	// The snapshot first: the warm-up is in it.
	waitForLine(t, v, f, "/warmup")
	followerAttached(t, v, f)

	for _, path := range []string{"/five", "/six"} {
		if got := proxied(t, v, "other.internal", path); got != http.StatusOK {
			t.Fatalf("GET %s answered %d", path, got)
		}
	}
	waitForLine(t, v, f, " other.internal GET /six 200 ")
	out := f.Stdout.String()
	five, six := strings.Index(out, " other.internal GET /five 200 "), strings.Index(out, " other.internal GET /six 200 ")
	if five < 0 || six < five {
		t.Errorf("the follower printed the two requests out of order or not at all:\n%s", out)
	}
	// Each of the two lines is the daemon's live line, verbatim.
	for _, line := range f.lines() {
		if strings.Contains(line, "/five") || strings.Contains(line, "/six") {
			if !v.Wrote(line) {
				t.Errorf("the follower printed %q, which the daemon's own terminal never showed", line)
			}
		}
	}

	f.Signal(t, syscall.SIGINT)
	if code := f.exitCode(t, "after SIGINT"); code != 0 {
		t.Errorf("`voya log -f` exited %d after SIGINT, want 0\n%s", code, f.Output())
	}
	if f.Stderr.String() != "" {
		t.Errorf("`voya log -f` wrote to stderr on its way out after ^C: %q", f.Stderr.String())
	}
}

// TestVoyaLogFollowEndsWhenTheDaemonStops: the daemon stopping under a
// running `-f` ends it with one line saying so, exit 1 — never nothing and
// exit 0.
func TestVoyaLogFollowEndsWhenTheDaemonStops(t *testing.T) {
	skipWithoutSIGTERM(t)
	v := startVoyaRun(t)
	v.waitUntilUp(t)

	f := startVoyaLog(t, v, "-f")
	followerAttached(t, v, f)

	v.Signal(t, syscall.SIGTERM)
	v.WaitForExit(t, "after SIGTERM under a follower", shutdownBound)
	if code := f.exitCode(t, "after the daemon stopped"); code != 1 {
		t.Errorf("`voya log -f` exited %d after the daemon stopped, want 1\n%s", code, f.Output())
	}
	errLines := strings.Split(strings.TrimSpace(f.Stderr.String()), "\n")
	if len(errLines) != 1 || !strings.Contains(errLines[0], "voya") || !strings.Contains(errLines[0], "shutting down") {
		t.Errorf("`voya log -f` said %q on stderr when the daemon stopped, want one line saying voya went away", f.Stderr.String())
	}
}

// TestVoyaLogFollowPrintsAWindowRecordOnce: a request recorded while the
// watch is open and the snapshot has not answered yet is in *both* answers,
// and `-f` prints it once (#161).
//
// A real daemon cannot be made to land a record there on purpose — with the
// stream opened first the window is a scheduling inversion inside one process,
// microseconds wide, and the test that waited for it would be a flake either
// way. So the command is pointed at a DiagService of its own, which holds
// ListRequests until the watcher has attached and carried the record: the
// overlap the command has to survive, made certain. In process, for the reason
// [TestVoyaCallReadsTheRequestBeforeItDials] gives — what is under test is an
// ordering, not an exit code.
//
// Two records, not one: a dedupe that dropped everything the stream carried
// would pass on the window record alone. The second is what says the follow
// still works after it.
func TestVoyaLogFollowPrintsAWindowRecordOnce(t *testing.T) {
	at := time.Date(2026, 8, 17, 9, 30, 0, 0, time.UTC)
	inWindow := &diagv1.Request{
		At: timestamppb.New(at), Authority: "payments.internal", Method: "GET",
		Path: "/in-the-window", Status: 200, Duration: durationpb.New(3 * time.Millisecond), Sequence: 1,
	}
	// The next record, a second later, so the two cannot be one another's
	// duplicate whatever [keyOf] reads.
	afterIt := &diagv1.Request{
		At: timestamppb.New(at.Add(time.Second)), Authority: "payments.internal", Method: "GET",
		Path: "/after-the-window", Status: 200, Duration: durationpb.New(4 * time.Millisecond), Sequence: 2,
	}
	d := &windowDaemon{
		attached: make(chan struct{}),
		snapshot: []*diagv1.Request{inWindow},
		stream:   []*diagv1.Request{inWindow, afterIt},
	}
	serveDiag(t, d)

	var stdout, stderr bytes.Buffer
	err := logCmd([]string{"-f"}, &stdout, &stderr)

	// The setup before the outcome: the snapshot really did wait for the
	// watcher, so the record really was in both answers. Without this the test
	// passes on a command that never opened the stream at all.
	if !d.snapshotWaited() {
		t.Fatalf("the snapshot answered before the watcher attached, so nothing was in both answers: "+
			"`voya log -f` asked for the snapshot first\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	// The stream ends after the second record, which ends the follow: one line
	// on stderr and exit 1, the same as any daemon that goes away mid-follow.
	var status *exitStatus
	if !errors.As(err, &status) || status.code != 1 {
		t.Errorf("logCmd(-f) = %v, want exit 1 for the stream ending\nstderr:\n%s", err, stderr.String())
	}
	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "/in-the-window") || !strings.Contains(lines[1], "/after-the-window") {
		t.Errorf("`voya log -f` printed:\n%s\nwant the window's request once, then the one after it",
			stdout.String())
	}
	if n := strings.Count(stdout.String(), "/in-the-window"); n != 1 {
		t.Errorf("the request recorded inside the window was printed %d times, want once:\n%s", n, stdout.String())
	}
}

// TestVoyaLogFollowTellsTwoRequestsApartOnACoarseClock: two distinct requests
// to the same authority and path with the same recorded timestamp — what a
// browser's parallel polls read as on Windows, where the wall clock ticks
// every 15.6 ms — are both printed when one is in the snapshot and the other
// arrives on the stream (#191). Only the sequence tells them apart, and the
// dedupe keys on it; keyed on the timestamp, authority and path the second
// was dropped as the first's duplicate.
func TestVoyaLogFollowTellsTwoRequestsApartOnACoarseClock(t *testing.T) {
	at := time.Date(2026, 8, 17, 9, 30, 0, 0, time.UTC)
	poll := func(seq uint64, d time.Duration) *diagv1.Request {
		return &diagv1.Request{
			At: timestamppb.New(at), Authority: "payments.internal", Method: "GET",
			Path: "/health", Status: 200, Duration: durationpb.New(d), Sequence: seq,
		}
	}
	first, second := poll(1, 3*time.Millisecond), poll(2, 4*time.Millisecond)
	d := &windowDaemon{
		attached: make(chan struct{}),
		snapshot: []*diagv1.Request{first},
		stream:   []*diagv1.Request{second},
	}
	serveDiag(t, d)

	var stdout, stderr bytes.Buffer
	err := logCmd([]string{"-f"}, &stdout, &stderr)

	if !d.snapshotWaited() {
		t.Fatalf("the snapshot answered before the watcher attached: `voya log -f` asked for the snapshot first"+
			"\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	var status *exitStatus
	if !errors.As(err, &status) || status.code != 1 {
		t.Errorf("logCmd(-f) = %v, want exit 1 for the stream ending\nstderr:\n%s", err, stderr.String())
	}
	// Both, in order — the durations are what tell the two lines apart.
	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], " 3ms ") || !strings.Contains(lines[1], " 4ms ") {
		t.Errorf("`voya log -f` printed:\n%s\nwant both polls, the snapshot's (3ms) then the stream's (4ms): "+
			"two requests inside one clock tick are not one request", stdout.String())
	}
}

// TestRequestKeyFallsBackToTheTripleWithoutASequence: a daemon that predates
// the sequence field sends 0 for every record, and a key on that alone would
// make every stream record a copy of the first snapshot one. Without a
// sequence the key is the old timestamp, authority and path; with one, only
// the sequence counts.
func TestRequestKeyFallsBackToTheTripleWithoutASequence(t *testing.T) {
	at := timestamppb.New(time.Date(2026, 8, 17, 9, 30, 0, 0, time.UTC))
	old := &diagv1.Request{At: at, Authority: "payments.internal", Path: "/a"}
	if keyOf(old) != keyOf(&diagv1.Request{At: at, Authority: "payments.internal", Path: "/a", Status: 500}) {
		t.Error("without a sequence, two copies of one record differing only in status key differently")
	}
	if keyOf(old) == keyOf(&diagv1.Request{At: at, Authority: "payments.internal", Path: "/b"}) {
		t.Error("without a sequence, two records with different paths share a key")
	}
	if keyOf(old) == keyOf(&diagv1.Request{At: at, Authority: "payments.internal", Path: "/a", Sequence: 1}) {
		t.Error("a record with a sequence shares a key with a record without one")
	}
	seq := &diagv1.Request{At: at, Authority: "payments.internal", Path: "/a", Sequence: 7}
	if keyOf(seq) != keyOf(&diagv1.Request{Sequence: 7}) {
		t.Error("with a sequence, the timestamp, authority and path still count in the key")
	}
}

// TestVoyaLogFollowMergesTheWindowByTime: with `-n 1` and two requests
// recorded inside the window, the snapshot carries only the newer and the
// stream carries both, and `-f` prints them oldest first, each once (#189).
// Before the merge the older one — cut from the snapshot by `-n` — printed
// from the stream *after* the newer, an older line under a newer one.
//
// The window is made certain the way [TestVoyaLogFollowPrintsAWindowRecordOnce]
// makes it, and one step further: the snapshot is held not merely until the
// stream has *sent* both records but until the command has *queued* them,
// through the [followQueued] seam — the merge takes what has queued when the
// snapshot arrives, and a record still crossing from the wire to the queue
// at that instant is the residual race the merge's comment names, which this
// test is not about.
func TestVoyaLogFollowMergesTheWindowByTime(t *testing.T) {
	at := time.Date(2026, 8, 17, 9, 30, 0, 0, time.UTC)
	older := &diagv1.Request{
		At: timestamppb.New(at), Authority: "payments.internal", Method: "GET",
		Path: "/cut-by-n", Status: 200, Duration: durationpb.New(3 * time.Millisecond), Sequence: 1,
	}
	newer := &diagv1.Request{
		At: timestamppb.New(at.Add(time.Second)), Authority: "payments.internal", Method: "GET",
		Path: "/in-the-snapshot", Status: 200, Duration: durationpb.New(4 * time.Millisecond), Sequence: 2,
	}
	d := &windowDaemon{
		attached:       make(chan struct{}),
		snapshot:       []*diagv1.Request{newer}, // what -n 1 leaves of the two
		stream:         []*diagv1.Request{older, newer},
		clientAttaches: true,
	}
	serveDiag(t, d)

	restore := followQueued
	t.Cleanup(func() { followQueued = restore })
	var queued atomic.Int32
	followQueued = func() {
		if queued.Add(1) == int32(len(d.stream)) {
			close(d.attached)
		}
	}

	var stdout, stderr bytes.Buffer
	err := logCmd([]string{"-f", "-n", "1"}, &stdout, &stderr)

	if !d.snapshotWaited() {
		t.Fatalf("the snapshot answered before both records were queued, so the merge had nothing to merge\nstdout:\n%s\nstderr:\n%s",
			stdout.String(), stderr.String())
	}
	var status *exitStatus
	if !errors.As(err, &status) || status.code != 1 {
		t.Errorf("logCmd(-f -n 1) = %v, want exit 1 for the stream ending\nstderr:\n%s", err, stderr.String())
	}
	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "/cut-by-n") || !strings.Contains(lines[1], "/in-the-snapshot") {
		t.Errorf("`voya log -f -n 1` printed:\n%s\nwant the request -n cut from the snapshot first, then the one it kept: oldest first",
			stdout.String())
	}
	for _, path := range []string{"/cut-by-n", "/in-the-snapshot"} {
		if n := strings.Count(stdout.String(), path); n != 1 {
			t.Errorf("%s was printed %d times, want once:\n%s", path, n, stdout.String())
		}
	}
}

// TestFollowerMergeOrdersAndDedupes pins [follower.merge] on its own, apart
// from the wire: the stream's records interleave with the snapshot's by At
// and by sequence within one clock tick, a record in both is one record whose
// key comes off the returned set, a record in the snapshot only stays on it,
// and an empty queue hands the snapshot back untouched.
func TestFollowerMergeOrdersAndDedupes(t *testing.T) {
	at := time.Date(2026, 8, 17, 9, 30, 0, 0, time.UTC)
	rec := func(seq uint64, d time.Duration, path string) *diagv1.Request {
		return &diagv1.Request{At: timestamppb.New(at.Add(d)), Authority: "a.internal", Path: path, Sequence: seq}
	}
	paths := func(rs []*diagv1.Request) []string {
		out := make([]string, len(rs))
		for i, r := range rs {
			out[i] = r.GetPath()
		}
		return out
	}
	s1, s2 := rec(1, 0, "/s1"), rec(2, 0, "/s2") // same tick: sequence orders them
	s3 := rec(3, time.Second, "/s3")
	s5 := rec(5, 3*time.Second, "/s5")
	s4 := rec(4, 2*time.Second, "/s4")

	f := &follower{requests: make(chan *diagv1.Request, requestlog.Capacity)}
	for _, r := range []*diagv1.Request{s1, s2, s4} {
		f.requests <- r
	}
	got, printed := f.merge([]*diagv1.Request{s2, s3, s5})

	want := []string{"/s1", "/s2", "/s3", "/s4", "/s5"}
	if g := paths(got); strings.Join(g, " ") != strings.Join(want, " ") {
		t.Errorf("merge = %v, want %v", g, want)
	}
	if _, still := printed[keyOf(s2)]; still {
		t.Errorf("s2 was in both answers and stays on the dedupe set — a later request repeating its key would be dropped")
	}
	for _, r := range []*diagv1.Request{s3, s5} {
		if _, ok := printed[keyOf(r)]; !ok {
			t.Errorf("%s is in the snapshot only and is off the dedupe set — its stream copy would print twice", r.GetPath())
		}
	}
	if len(f.requests) != 0 {
		t.Errorf("%d records left queued after the merge, want none", len(f.requests))
	}

	// Nothing queued: the snapshot comes back as it was, keys and all.
	same, printed := f.merge([]*diagv1.Request{s3})
	if len(same) != 1 || same[0] != s3 || len(printed) != 1 {
		t.Errorf("merge with nothing queued = %v (%d keys), want the snapshot untouched with its one key", paths(same), len(printed))
	}
}

// windowDaemon is a DiagService that puts one record in both of `-f`'s
// answers: ListRequests does not answer until WatchRequests has attached and
// sent everything it has, so the snapshot is taken with the stream already
// carrying the record it holds. With clientAttaches the test closes attached
// itself, from the command's side of the wire, once the records have queued.
type windowDaemon struct {
	diagv1connect.UnimplementedDiagServiceHandler
	attached       chan struct{} // closed once the stream has sent its records
	snapshot       []*diagv1.Request
	stream         []*diagv1.Request
	clientAttaches bool

	waited atomic.Bool
}

// watcherWait bounds how long the snapshot waits, under [diagDeadline] so that
// a `voya log` which asks for the snapshot first fails here — with a sentence
// naming the inversion — rather than as the client's deadline.
const watcherWait = time.Second

func (d *windowDaemon) ListRequests(ctx context.Context, _ *connect.Request[diagv1.ListRequestsRequest]) (*connect.Response[diagv1.ListRequestsResponse], error) {
	select {
	case <-d.attached:
		d.waited.Store(true)
		return connect.NewResponse(&diagv1.ListRequestsResponse{Requests: d.snapshot}), nil
	case <-ctx.Done():
		return nil, connect.NewError(connect.CodeCanceled, ctx.Err())
	case <-time.After(watcherWait):
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("the snapshot was asked for before the watch was open"))
	}
}

// WatchRequests sends its records and ends, which ends the follow.
func (d *windowDaemon) WatchRequests(_ context.Context, _ *connect.Request[diagv1.WatchRequestsRequest], stream *connect.ServerStream[diagv1.WatchRequestsResponse]) error {
	for _, r := range d.stream {
		if err := stream.Send(&diagv1.WatchRequestsResponse{Request: r}); err != nil {
			return err
		}
	}
	if !d.clientAttaches {
		close(d.attached)
	}
	return connect.NewError(connect.CodeUnavailable, errors.New("that is all"))
}

// snapshotWaited reports whether ListRequests answered out of the watcher
// having attached rather than out of its own timeout.
func (d *windowDaemon) snapshotWaited() bool { return d.waited.Load() }

// TestVoyaLogFollowHoldsTheSnapshotUntilTheWatchIsOnTheWire: [startFollow]
// does not return until the WatchRequests call has been written (#161).
// Spawning the goroutine is not the same as its having run: a startFollow
// that returned before the call reached the wire would leave the snapshot's
// round trip racing the watch's rather than following it, which is the window
// the order exists to close, at its full width.
//
// The dial is what is held here, because it is the one point both calls must
// pass and the test can hold open for as long as it likes. Nothing is written
// while it is held, so a startFollow that returns is one that did not wait.
// The daemon then holds WatchRequests open until its context ends, so that
// what releases startFollow is the trace of the write and not the call
// returning.
func TestVoyaLogFollowHoldsTheSnapshotUntilTheWatchIsOnTheWire(t *testing.T) {
	addr := serveDiag(t, &heldDaemon{})
	dial := make(chan struct{})
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true) // what dialProxy's transport speaks
	tr := &http.Transport{
		Protocols: protocols,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			select {
			case <-dial:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	target := &diagTarget{
		client:    diagv1connect.NewDiagServiceClient(&http.Client{Transport: tr}, "http://"+addr),
		transport: tr,
	}
	t.Cleanup(target.close)

	started := make(chan *follower, 1)
	go func() { started <- startFollow(context.Background(), target) }()

	// Well under diagDeadline, so a pass here is the wait and not the bound
	// startFollow falls through on.
	select {
	case <-started:
		t.Fatal("startFollow returned while the dial was still held: nothing had been written, " +
			"so the snapshot is free to be answered before the watcher attaches")
	case <-time.After(250 * time.Millisecond):
	}

	close(dial)
	select {
	case f := <-started:
		f.stop()
	case <-time.After(startupBound):
		t.Fatalf("startFollow did not return within %s of the dial being let through", startupBound)
	}
}

// TestVoyaLogFollowEndsWithASentenceWhenTheStreamJustStops: a stream the
// daemon ends by returning nothing ends the follow with a line and exit 1,
// not with `voya: the follow ended: <nil>` — the message #163's item 2 exists
// to keep out of this line, arriving by the other door.
//
// [diag.Server.WatchRequests] returns nil only on a branch nothing reaches
// today, so this is the guard tested rather than the path: a daemon of the
// test's own returns it outright.
func TestVoyaLogFollowEndsWithASentenceWhenTheStreamJustStops(t *testing.T) {
	serveDiag(t, &silentDaemon{})

	var stdout, stderr bytes.Buffer
	err := logCmd([]string{"-f"}, &stdout, &stderr)

	var status *exitStatus
	if !errors.As(err, &status) || status.code != 1 {
		t.Errorf("logCmd(-f) = %v, want exit 1 for a stream that stopped\nstderr:\n%s", err, stderr.String())
	}
	if got, want := stderr.String(), "voya: the follow ended\n"; got != want {
		t.Errorf("`voya log -f` said %q when the stream stopped, want %q", got, want)
	}
	if stdout.Len() != 0 {
		t.Errorf("`voya log -f` printed %q on stdout, want the empty log to say nothing there", stdout.String())
	}
}

// heldDaemon holds WatchRequests open until the caller lets go, and answers
// ListRequests with nothing.
type heldDaemon struct {
	diagv1connect.UnimplementedDiagServiceHandler
}

func (*heldDaemon) ListRequests(context.Context, *connect.Request[diagv1.ListRequestsRequest]) (*connect.Response[diagv1.ListRequestsResponse], error) {
	return connect.NewResponse(&diagv1.ListRequestsResponse{}), nil
}

func (*heldDaemon) WatchRequests(ctx context.Context, _ *connect.Request[diagv1.WatchRequestsRequest], _ *connect.ServerStream[diagv1.WatchRequestsResponse]) error {
	<-ctx.Done()
	return nil
}

// silentDaemon ends the watch by returning nothing at all: no records, no
// error, the shape [diag.Server.WatchRequests]'s unreachable branch has.
type silentDaemon struct {
	diagv1connect.UnimplementedDiagServiceHandler
}

func (*silentDaemon) ListRequests(context.Context, *connect.Request[diagv1.ListRequestsRequest]) (*connect.Response[diagv1.ListRequestsResponse], error) {
	return connect.NewResponse(&diagv1.ListRequestsResponse{}), nil
}

func (*silentDaemon) WatchRequests(context.Context, *connect.Request[diagv1.WatchRequestsRequest], *connect.ServerStream[diagv1.WatchRequestsResponse]) error {
	return nil
}

// serveDiag puts h on loopback over h2c and writes a config directory naming
// its port, so that [dialDiag] — and every command that goes through it —
// reaches this handler instead of the developer's own voya. It returns the
// address it is listening on, for the one test that dials it itself.
func serveDiag(t *testing.T, h diagv1connect.DiagServiceHandler) string {
	t.Helper()
	path, handler := diagv1connect.NewDiagServiceHandler(h)
	mux := http.NewServeMux()
	mux.Handle(path, handler)

	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true) // what dialProxy's transport speaks, and only that
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = protocols
	srv.Start()
	t.Cleanup(srv.Close)

	port := srv.Listener.Addr().(*net.TCPAddr).Port
	dir := t.TempDir()
	layer := fmt.Sprintf("proxy_port = %d\n", port)
	if err := os.WriteFile(filepath.Join(dir, config.UserFile), []byte(layer), 0o600); err != nil {
		t.Fatalf("write user layer: %v", err)
	}
	t.Setenv(config.DirEnv, dir)
	return srv.Listener.Addr().String()
}

// TestVoyaLogPXFIsOneDocumentPerLine: `-pxf` renders each request as one
// pxf document on one line, on stdout and nothing else, and every line
// parses back into a Request.
func TestVoyaLogPXFIsOneDocumentPerLine(t *testing.T) {
	skipWithoutSIGTERM(t)
	v := startVoyaRun(t)
	v.waitUntilUp(t)
	port := echoService(t, v, "other")
	waitRouted(t, v, "other.internal")
	if got := proxied(t, v, "other.internal", "/pxf"); got != http.StatusOK {
		t.Fatalf("GET /pxf answered %d", got)
	}

	l := startVoyaLog(t, v, "-pxf")
	if code := l.exitCode(t, "printing pxf"); code != 0 {
		t.Fatalf("`voya log -pxf` exited %d\n%s", code, l.Output())
	}
	if l.Stderr.String() != "" {
		t.Errorf("`voya log -pxf` wrote to stderr: %q", l.Stderr.String())
	}
	lines := l.lines()
	if len(lines) == 0 {
		t.Fatalf("`voya log -pxf` printed nothing")
	}
	var found bool
	for _, line := range lines {
		if !strings.HasPrefix(line, "at = ") || !strings.Contains(line, ` method = "GET" `) {
			t.Errorf("not one pxf document on one line: %q", line)
		}
		var back diagv1.Request
		if err := pxf.Unmarshal([]byte(line), &back); err != nil {
			t.Errorf("pxf.Unmarshal(%q): %v", line, err)
			continue
		}
		if back.GetPath() == "/pxf" {
			found = true
			if back.GetAuthority() != "other.internal" || back.GetStatus() != 200 || back.GetUpstreamPort() != port || back.GetAt() == nil {
				t.Errorf("parsed back %v, want other.internal GET /pxf 200 -> %d", &back, port)
			}
		}
	}
	// Only the parsed-back path counts: a textual fallback would let the
	// test pass on a line the decoder had stopped reading correctly.
	if !found {
		t.Errorf("no pxf line parsed back to the counted request:\n%s", l.Stdout.String())
	}
}

// TestPXFRoundTrip is the -pxf rendering driven directly: a Request whose
// values all lex — a whole-second duration — renders as one line, and that
// line parses back to the same message.
func TestPXFRoundTrip(t *testing.T) {
	r := requestlog.Request{
		At:           time.Date(2026, 8, 15, 17, 46, 15, 123456789, time.UTC),
		Authority:    "payments.internal",
		Method:       "POST",
		Path:         "/voya.acceptance.v1.Echo/Whoami",
		Status:       200,
		Duration:     2 * time.Second,
		UpstreamPort: 54321,
		Sequence:     7,
	}
	line, err := renderPXF(r.Proto())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(line, "\n") {
		t.Errorf("the pxf rendering spans lines: %q", line)
	}
	var back diagv1.Request
	if err := pxf.Unmarshal([]byte(line), &back); err != nil {
		t.Fatalf("pxf.Unmarshal(%q): %v", line, err)
	}
	if got := requestlog.FromProto(&back); !got.At.Equal(r.At) || got.Authority != r.Authority || got.Method != r.Method ||
		got.Path != r.Path || got.Status != r.Status || got.Duration != r.Duration || got.UpstreamPort != r.UpstreamPort ||
		got.Sequence != r.Sequence {
		t.Errorf("round trip: %+v -> %+v", r, got)
	}
	// A path carrying the characters that would break a one-line document if
	// the encoder did not escape them: quotes and a newline.
	r.Path = "/odd\"path\nline"
	line, err = renderPXF(r.Proto())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(line, "\n") {
		t.Errorf("a path with a newline broke the one-line rendering: %q", line)
	}
	if err := pxf.Unmarshal([]byte(line), &back); err != nil || back.GetPath() != r.Path {
		t.Errorf("round trip of an odd path: %v, path %q", err, back.GetPath())
	}
}

// TestPXFDurationRoundTrip: a fractional duration — and every measured
// latency is one — survives the trip through renderPXF and pxf.Unmarshal.
// protowire-go v1.4.0's lexer could not read the `1.234567ms` its own encoder
// wrote (trendvidia/protowire-go#75, fixed in v1.4.1); this is what the pin
// that documented it became once go.mod moved past it (#167).
func TestPXFDurationRoundTrip(t *testing.T) {
	for _, d := range []time.Duration{1234567 * time.Nanosecond, 312500 * time.Nanosecond, 2 * time.Microsecond, 3 * time.Second} {
		m := &diagv1.Request{At: timestamppb.New(time.Unix(0, 0)), Duration: durationpb.New(d)}
		line, err := renderPXF(m)
		if err != nil {
			t.Fatal(err)
		}
		if want := "duration = " + d.String(); !strings.Contains(line, want) {
			t.Errorf("renderPXF(%v) = %q, want it to contain %q", d, line, want)
		}
		var back diagv1.Request
		if err := pxf.Unmarshal([]byte(line), &back); err != nil {
			t.Errorf("pxf.Unmarshal(%q): %v", line, err)
			continue
		}
		if got := back.GetDuration().AsDuration(); got != d {
			t.Errorf("round trip of %v came back as %v", d, got)
		}
	}
}

// TestVoyaLogFollowLeavesTheSnapshotsInterruptAlone: `-f` installs its ^C
// handler after the snapshot, so a signal that arrives while the snapshot is
// still outstanding is the shell's ordinary interrupt and kills the command —
// it does not cancel ListRequests and come back out as a failure of `voya
// log`.
//
// signal.NotifyContext takes the default disposition away for as long as it
// is installed, which is why the placement is a behaviour and not a detail.
// The window is made deterministic rather than raced for: the listener the
// command reaches accepts and never answers, so the snapshot is outstanding
// for the whole of diagDeadline, and the signal is sent only once that accept
// has happened.
func TestVoyaLogFollowLeavesTheSnapshotsInterruptAlone(t *testing.T) {
	skipWithoutSIGTERM(t)
	// A config directory naming ports nothing holds, then a listener of our
	// own on the proxy port — the same shape [TestDiagCommandsWhenVoyaIsNotRunning]
	// uses for its wedged voya.
	v := startVoyaRun(t)
	v.waitUntilUp(t)
	v.Signal(t, syscall.SIGTERM)
	v.WaitForExit(t, "after SIGTERM", shutdownBound)

	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.FormatUint(uint64(v.ports.Proxy), 10))
	if err != nil {
		t.Skipf("the stopped daemon's proxy port was taken in between: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan struct{}, 1)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			select {
			case accepted <- struct{}{}:
			default:
			}
		}
	}()

	f := startVoyaLog(t, v, "-f")
	select {
	case <-accepted:
	case <-time.After(startupBound):
		t.Fatalf("`voya log -f` did not reach the wedged listener within %s\n%s", startupBound, f.Output())
	}
	f.Signal(t, syscall.SIGINT)

	// exec reports -1 for a process a signal killed, which is the whole
	// claim: the command did not survive its own ^C to render one.
	if code := f.exitCode(t, "after SIGINT during the snapshot"); code != -1 {
		t.Errorf("`voya log -f` interrupted during the snapshot exited %d, want to have been killed by the signal\n%s",
			code, f.Output())
	}
	if out := f.Stdout.String() + f.Stderr.String(); out != "" {
		t.Errorf("`voya log -f` interrupted during the snapshot said %q; the signal is the shell's, and voya has nothing to add", out)
	}
}

// TestVoyaLogWhenTheLogIsOff: a daemon started with `request_log = false`
// keeps no buffer, and `voya log` says so rather than "no requests yet".
func TestVoyaLogWhenTheLogIsOff(t *testing.T) {
	skipWithoutSIGTERM(t)
	v := startVoyaRun(t, "request_log = false")
	v.waitUntilUp(t)
	proxied(t, v, "nobody.internal", "/unrecorded")

	l := startVoyaLog(t, v)
	if code := l.exitCode(t, "against a daemon with the log off"); code != 1 {
		t.Errorf("`voya log` with the log off exited %d, want 1\n%s", code, l.Output())
	}
	if l.Stdout.String() != "" {
		t.Errorf("`voya log` with the log off wrote to stdout: %q", l.Stdout.String())
	}
	if !strings.Contains(l.Stderr.String(), "request_log = false") {
		t.Errorf("`voya log` with the log off said %q, want it to name request_log = false", l.Stderr.String())
	}
	// And the daemon printed no live line for the request either.
	if strings.Contains(v.Stderr.String(), "/unrecorded") {
		t.Errorf("the daemon printed a request line with request_log = false:\n%s", v.Stderr.String())
	}
}

// TestVoyaLogNoVoyaLeavesOutTheReservedAuthority is #218's second item, both
// directions: the daemon records a key-set fetch under voya.internal — the log's
// completeness must not depend on what callers claim about themselves — so
// without `-no-voya` the fetch is a row, and with it `voya log` prints the
// developer's rows and not that one. The follow honours the filter too, in
// its snapshot and past it: a fetch under an open `-f -no-voya` adds no
// line, while a request of the developer's made *after* that fetch still does
// — the stream delivers in order, so the later line printing is what says
// the earlier row was dropped rather than still in flight.
func TestVoyaLogNoVoyaLeavesOutTheReservedAuthority(t *testing.T) {
	skipWithoutSIGTERM(t)
	v := startVoyaRun(t)
	v.waitUntilUp(t)
	echoService(t, v, "other")
	waitRouted(t, v, "other.internal")

	if got := proxied(t, v, "voya.internal", auth.JWKSPath); got != http.StatusOK {
		t.Fatalf("the key-set fetch answered %d; there is nothing to filter", got)
	}
	jwksRow := " voya.internal GET " + auth.JWKSPath + " 200 "

	all := startVoyaLog(t, v)
	if code := all.exitCode(t, "without -no-voya"); code != 0 {
		t.Fatalf("`voya log` exited %d\n%s", code, all.Output())
	}
	if out := all.Stdout.String(); !strings.Contains(out, jwksRow) {
		t.Errorf("`voya log` does not carry the key-set fetch:\n%s", out)
	}

	filtered := startVoyaLog(t, v, "-no-voya")
	if code := filtered.exitCode(t, "with -no-voya"); code != 0 {
		t.Fatalf("`voya log -no-voya` exited %d\n%s", code, filtered.Output())
	}
	out := filtered.Stdout.String()
	if strings.Contains(out, "voya.internal") {
		t.Errorf("`voya log -no-voya` still prints a reserved-authority row:\n%s", out)
	}
	if !strings.Contains(out, " other.internal GET /warmup 200 ") {
		t.Errorf("`voya log -no-voya` dropped the developer's own request too:\n%s", out)
	}

	f := startVoyaLog(t, v, "-f", "-no-voya")
	// The follow's snapshot holds the first fetch's row; the warmup line
	// printing says the snapshot has been, with the row filtered from it.
	waitForLine(t, v, f, " other.internal GET /warmup 200 ")
	followerAttached(t, v, f)

	if got := proxied(t, v, "voya.internal", auth.JWKSPath); got != http.StatusOK {
		t.Fatalf("the key-set fetch under a filtered follow answered %d", got)
	}
	if got := proxied(t, v, "other.internal", "/after"); got != http.StatusOK {
		t.Fatalf("GET /after answered %d", got)
	}
	waitForLine(t, v, f, " other.internal GET /after 200 ")
	if out := f.Stdout.String(); strings.Contains(out, "voya.internal") {
		t.Errorf("`voya log -f -no-voya` printed a reserved-authority row:\n%s", out)
	}

	f.Signal(t, syscall.SIGINT)
	if code := f.exitCode(t, "after SIGINT"); code != 0 {
		t.Errorf("`voya log -f -no-voya` exited %d after SIGINT, want 0\n%s", code, f.Output())
	}
}

// TestUnderReservedIsTheRoutersOwnQuestion pins the shapes a recorded
// authority can arrive in against what the router did with the very same
// header, which is what decides whether a row is voya's own. The record is
// the header less voya's own proxy port, so every other thing a client may
// write survives into the column: a foreign port, the root dot resolvers
// send, uppercase. Each of those is routed to the reserved handler and so
// must be filtered; `voya.internal:abc` is not routed there — a
// non-numeric port is no port, and the router answers "not a name voya
// routes" — so that row is the developer's own diagnostic and must survive
// `-no-voya`, which is the direction a filter of its own gets backwards.
func TestUnderReservedIsTheRoutersOwnQuestion(t *testing.T) {
	for _, tc := range []struct {
		authority string
		want      bool
	}{
		{"voya.internal", true},      // the recorded form: voya's port stripped
		{"voya.internal:9777", true}, // a caller that named the port itself
		{"voya.internal:8080", true}, // a foreign port, kept in the column
		{"voya.internal.", true},     // the root dot resolvers and some clients send
		{"VOYA.internal", true},      // hostnames are case-insensitive
		{"voya.internal:abc", false}, // no port; the router refuses the name
		{"api.voya.internal", false}, // a service name is one label
		{"other.internal", false},    // the developer's own service
		{"voya.internal.example.com", false},
		{"voya", false},
		{"", false},
	} {
		if got := underReserved(tc.authority); got != tc.want {
			t.Errorf("underReserved(%q) = %v, want %v", tc.authority, got, tc.want)
		}
	}
}

// TestLogRejectsWhatItDoesNotTake drives the flag parsing in process:
// positional words and unknown flags are refused with usage on stderr, and
// -h is help on stdout.
func TestLogRejectsWhatItDoesNotTake(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		wantErr string
	}{
		{[]string{"log", "junk"}, `log takes no arguments, got "junk"`},
		{[]string{"log", "-bogus"}, "flag provided but not defined: -bogus"},
		{[]string{"log", "-n", "x"}, `invalid value "x" for flag -n`},
	} {
		t.Run(strings.Join(tc.args, "_"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run(tc.args, &stdout, &stderr)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("run(%q) = %v, want %q", tc.args, err, tc.wantErr)
			}
			if !strings.Contains(stderr.String(), "voya log [-n N] [-f] [-pxf] [-no-voya]") {
				t.Errorf("no log usage on stderr:\n%s", stderr.String())
			}
		})
	}
	var stdout, stderr bytes.Buffer
	if err := run([]string{"log", "-h"}, &stdout, &stderr); err != nil {
		t.Errorf("`voya log -h`: %v", err)
	}
	if !strings.Contains(stdout.String(), "voya log [-n N] [-f] [-pxf] [-no-voya]") || stderr.Len() != 0 {
		t.Errorf("`voya log -h` printed stdout %q stderr %q", stdout.String(), stderr.String())
	}
}

// TestUsageListsLogAsBuilt: the row is built, so usage lists it under
// Commands. The "not under Not implemented yet" half retired with the block
// itself (#210); TestEveryCommandIsBuilt in main_test.go pins the block gone.
func TestUsageListsLogAsBuilt(t *testing.T) {
	var stdout bytes.Buffer
	if err := run([]string{"help"}, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !listsCommand(stdout.String(), "log") {
		t.Errorf("usage does not list `log` under Commands:\n%s", stdout.String())
	}
}
