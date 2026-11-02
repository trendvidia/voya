package acceptance

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/wrapperspb"

	voya "github.com/trendvidia/voya/client"
	"github.com/trendvidia/voya/internal/registration"
	"github.com/trendvidia/voya/internal/testproc"
)

// registrationKeepaliveBackstop is how long voya takes to give up on a
// registration stream when nothing at the transport level tells it the peer is
// gone. It is the number the eviction below has to beat by a wide margin: if a
// SIGKILL is only noticed on this timer, the socket did not die with the
// process and ADR 004's eviction path is not doing what it claims.
const registrationKeepaliveBackstop = registration.PingIdle + registration.PingTimeout

// evictionBound is how long eviction may take after a SIGKILL before this
// suite calls it a failure. Wall clock on a developer's machine, so it is
// generous by three orders of magnitude against what is actually measured —
// what it has to separate is "the dying socket told voya" from "a timer did",
// and those are milliseconds against [registrationKeepaliveBackstop].
//
// The margin between the two is checked where the bound is used, not assumed
// here: the backstop is a constant in another package, and a pair that met
// would turn this assertion into one a keepalive-driven eviction also passes.
const evictionBound = 2 * time.Second

// --- #40: a callee that is really killed --------------------------------------
//
// Every other service in this suite shares the test binary's process, which
// puts one case out of reach. Closing a simulated service's listener does not
// kill the registration client holding its stream, because that client is this
// process — so voya sees an orderly deregistration, not a socket dying with the
// process that owned it. The difference is the whole of ADR 004's eviction
// path, and #18's "SIGKILL the callee" was only ever tested against a service
// that stopped politely.
//
// So the callee here is a second process: the test binary re-executed with
// [serviceEnv] set, which makes it serve instead of running tests. That is
// enough to stage a real SIGKILL — uncatchable, no deferred anything, the
// socket closed by the kernel and not by the client — and it is also the only
// honest way to kill a service in the middle of a request it is serving.

const (
	// serviceEnv both selects the subprocess role and names the service. Set
	// means "be a service"; TestMain reads it before anything else runs.
	serviceEnv = "VOYA_ACCEPTANCE_SERVICE"

	identityEnv         = "VOYA_ACCEPTANCE_IDENTITY"
	registrationPortEnv = "VOYA_ACCEPTANCE_REGISTRATION_PORT"
	proxyPortEnv        = "VOYA_ACCEPTANCE_PROXY_PORT"
)

// holdingRequest is what the subprocess prints once a request is inside its
// handler and going nowhere. The request that gets there is one carrying
// [sleepPrefix]; this is the announcement that makes the arrival observable
// from the parent rather than guessed at.
const holdingRequest = "holding a request"

// TestMain re-executes this binary as a service — or, for errorband_test.go,
// as a daemon — when the environment says so.
//
// The standard trick, and deliberately the whole of it: no second module, no
// fixture binary to build and find, and the service runs the same client
// package everything else here does.
func TestMain(m *testing.M) {
	if os.Getenv(serviceEnv) != "" {
		runServiceUntilKilled()
		return
	}
	if os.Getenv(daemonEnv) != "" {
		runDaemonUntilStopped()
		return
	}
	os.Exit(m.Run())
}

// runServiceUntilKilled is the subprocess: bind, join, serve, and block until
// something kills it. There is no clean shutdown path on purpose — the point of
// this process is to die without one.
func runServiceUntilKilled() {
	name, identity := requireEnv(serviceEnv), requireEnv(identityEnv)
	registrationPort := envPort(registrationPortEnv)
	proxyPort := envPort(proxyPortEnv)

	// The same handler and the same server the in-process services run, so
	// that what is under test here is the kill and not a second service
	// implementation that drifted from the first. The announcement is the one
	// thing only this process does: stdout is a pipe the parent reads.
	srv := echoServer(echoHandler(identity, func() { fmt.Println(holdingRequest) }))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		exitf("listen: %v", err)
	}
	go func() {
		// Nothing here ever calls Close or Shutdown — that is this process's
		// whole point — so ErrServerClosed cannot arrive. Tolerated anyway,
		// and by identity rather than equality, so that adding a shutdown path
		// later cannot turn a clean stop into a spurious exit(1).
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			exitf("serve: %v", err)
		}
	}()

	// No stop function is kept. A process that is about to be SIGKILLed has no
	// use for one, and holding it would suggest this path ever deregisters
	// politely — which is the thing being tested for its absence.
	if _, err := voya.Join(context.Background(), name, ln.Addr(),
		voya.WithRegistrationPort(registrationPort),
		voya.WithProxyPort(proxyPort),
		voya.WithBackoff(10*time.Millisecond, 200*time.Millisecond),
	); err != nil {
		exitf("join: %v", err)
	}

	// The liveness tie: this service stops when the test run that started it
	// does, so an interrupted `go test` does not leave it holding a port for
	// the rest of the session. Asserted by
	// [TestTheServiceSubprocessExitsWithItsParent], since every other test here
	// would pass just as well if this were a permanent block.
	testproc.WaitForParent()
}

// requireEnv reads one of the variables the parent always sets. An empty one
// means the two halves of this file disagree, and saying so here beats serving
// with it: every way this process can be misconfigured otherwise reaches the
// parent as a bare registration timeout.
func requireEnv(key string) string {
	value := os.Getenv(key)
	if value == "" {
		exitf("%s is empty; the parent sets it before starting this process", key)
	}
	return value
}

// envPort reads a port the parent drew.
//
// Zero is rejected here rather than left to the registrar: voya.Join accepts
// the *service's* port range but the ports it dials are this process's only
// way of finding voya, and a 0 would send the registrar retrying 127.0.0.1:0
// on a backoff for the rest of its life. The parent would meet that as a
// twenty-second registration timeout with nothing in the output to explain it,
// which is the failure shape this file works to avoid everywhere else.
func envPort(key string) uint32 {
	raw := requireEnv(key)
	port, err := strconv.ParseUint(raw, 10, 16)
	if err != nil {
		exitf("%s=%q is not a port: %v", key, raw, err)
	}
	if port == 0 {
		exitf("%s=0 is not a port anything listens on", key)
	}
	return uint32(port)
}

// exitf fails the subprocess loudly, naming this suite as the speaker.
func exitf(format string, args ...any) { testproc.Exitf("acceptance", format, args...) }

// killable is a callee running in its own process.
//
// The spawn, the output capture, the background wait, fail-if-exited, the
// signal that does not reap and the liveness tie are [testproc.Child]'s —
// written twice before #65 gave them one home. What stays here is what this
// suite means by a service being ready: registered, under its own pid.
type killable struct {
	*testproc.Child
}

// startKillableService re-executes the test binary as a service and waits for
// it to register.
func startKillableService(t *testing.T, v *voyad, name, identity string) *killable {
	t.Helper()

	k := &killable{testproc.Start(t.Context(), t, name,
		serviceEnv+"="+name,
		identityEnv+"="+identity,
		portEnv(registrationPortEnv, v.registrationPort),
		portEnv(proxyPortEnv, v.proxyPort),
		// Cleared, not inherited. Every call in the client package is a no-op
		// when this is set, so a developer with it in their shell would get a
		// subprocess that serves and never registers — which reaches this test
		// as a twenty-second timeout with nothing to say about why.
		voya.FlyAppEnv+"=",
	)}

	// Watched for exiting as well as for registering. Every way this
	// subprocess can fail at startup — a port it cannot parse, a listener it
	// cannot bind, a Join that refuses the address — ends in exit(1) with the
	// reason on stderr, and without this the parent would poll a registry that
	// will never fill and report a twenty-second timeout instead, with the
	// child's explanation printed somewhere after it. Clearing FLY_APP_NAME
	// above is one instance of that failure shape; this is the mechanism.
	waitFor(t, name+" (pid "+strconv.Itoa(k.PID())+") to register", func() bool {
		k.FailIfExited(t, "before it registered")
		up, live := v.Registry().Lookup(name)
		return live && up.PID == int32(k.PID())
	})
	return k
}

// kill is the SIGKILL: no signal handler runs, no deferred stop function is
// called, and the registration socket closes because the kernel closed it.
//
// [testproc.Child.Signal] deliberately does not reap, which is what this file
// needs: a waitpid and the drain of two output pipes between the signal and
// the first look at the registry would land inside the interval this suite
// measures and publishes as eviction latency. The cleanup does the waiting.
func (k *killable) kill(t *testing.T) {
	t.Helper()
	k.Signal(t, syscall.SIGKILL)
}

// skipWithoutSIGKILL keeps this file to the platforms where the signal means
// what it says.
func skipWithoutSIGKILL(t *testing.T) {
	t.Helper()
	testproc.SkipWithoutSignals(t, "TestAbruptDisconnectEvicts covers the transport half everywhere")
}

// TestSIGKILLedCalleeIsEvictedAndExplained is #18's acceptance criterion with
// nothing simulated: a real process is killed outright, and voya notices
// because its socket died.
//
// #40 exists because what landed for #18 asserted the second half — the
// diagnostic — against a service that *stopped*, which reaches voya as an
// orderly deregistration. The eviction path ADR 004 is built around is the
// other one, and this is it end to end.
func TestSIGKILLedCalleeIsEvictedAndExplained(t *testing.T) {
	skipWithoutSIGKILL(t)

	v := startVoya(t)
	callee := startKillableService(t, v, "other", "other/subprocess")
	client := caller(t, v)

	got, err := call(t, client)
	if err != nil {
		t.Fatalf("the call before the kill failed: %v", err)
	}
	if !strings.HasPrefix(got, "other/subprocess") {
		t.Fatalf("the call reached %q, want the subprocess", got)
	}

	killed := time.Now()
	callee.kill(t)

	waitFor(t, "the killed process to be evicted", func() bool {
		_, live := v.Registry().Lookup("other")
		return !live
	})
	evictedIn := time.Since(killed)

	// ADR 004's claim is that eviction is as fast as TCP close detection
	// allows, with keepalive only as the backstop for a socket that outlives
	// its process. [evictionBound] is evidence for that only while it sits far
	// below the backstop: if the two ever met, a run where the dying socket
	// said nothing and the timer did all the work would pass this test with
	// the comment above still claiming otherwise. The backstop is a constant
	// in another package, so the margin is checked rather than described.
	if registrationKeepaliveBackstop < 10*evictionBound {
		t.Fatalf("the keepalive backstop is %s and this test allows %s for eviction; "+
			"the bound no longer rules out an eviction that only the timer noticed",
			registrationKeepaliveBackstop, evictionBound)
	}
	if evictedIn > evictionBound {
		t.Errorf("eviction took %s after SIGKILL; TCP close detection should be immediate and "+
			"keepalive (%s) is only the backstop for a socket that outlives its process",
			evictedIn, registrationKeepaliveBackstop)
	}
	// Reported as the upper bound it is. Eviction is observed by polling the
	// registry every [pollInterval], so most of this number is the poller
	// waking up — publishing it as a latency would be claiming a precision
	// nothing here has.
	t.Logf("evicted within %s of the SIGKILL (polled every %s) against a %s keepalive backstop",
		evictedIn.Round(time.Microsecond), pollInterval, registrationKeepaliveBackstop)

	// And the next call is told, promptly, by name.
	assertToldPromptly(t, client, "other")
}

// TestTheServiceSubprocessExitsWithItsParent pins the orphan prevention the
// rest of this file leans on.
//
// The subprocess reads stdin to EOF and then exits, so a `go test` that is
// itself interrupted takes its services with it rather than leaving one
// holding a port for the rest of the session. Every other test here kills the
// child outright, and would pass just as well if that read were a permanent
// block — so the property is asserted rather than left to the comment on it.
func TestTheServiceSubprocessExitsWithItsParent(t *testing.T) {
	v := startVoya(t)
	callee := startKillableService(t, v, "other", "other/orphan")

	// Closing the pipe is what an interrupted `go test` does to it by dying.
	if err := callee.CloseStdin(); err != nil {
		t.Fatalf("closing the subprocess's stdin: %v", err)
	}
	select {
	case <-callee.Exited():
		if callee.WaitErr() != nil {
			t.Errorf("the subprocess exited with %v; EOF on stdin is an ordinary stop, not a failure", callee.WaitErr())
		}
	case <-time.After(evictionBound * 5):
		t.Fatal("the subprocess is still running with its parent's end of the pipe closed; " +
			"an interrupted `go test` would leave it holding a port")
	}
}

// TestACalleeKilledMidRequestFailsTheCaller is the case that could not be
// staged at all before: the callee dies while it is serving.
//
// The caller is waiting on a response that will never arrive, and what it must
// not get is a hang — it has to be told, and told something that names the
// service, so a developer looking at a stuck frontend learns which backend
// went away rather than watching a spinner until a client timeout they set
// themselves.
func TestACalleeKilledMidRequestFailsTheCaller(t *testing.T) {
	skipWithoutSIGKILL(t)

	v := startVoya(t)
	callee := startKillableService(t, v, "other", "other/doomed")
	client := caller(t, v)

	// A request the callee will hold far longer than this test will wait, so
	// the kill lands with the response outstanding rather than racing it.
	answered := make(chan error, 1)
	go func() {
		_, err := client.CallUnary(context.Background(),
			connect.NewRequest(wrapperspb.String(sleepPrefix+"60s")))
		answered <- err
	}()

	// The request has to be in the handler before the kill, or this is the
	// previous test with extra steps. Both ways the announcement can fail to
	// arrive are reported as themselves: a call that came back early — a
	// routing failure, a registration race, an h2c negotiation that did not
	// take — would otherwise sit unread in `answered` while this waits twenty
	// seconds and blames the handler.
	waitFor(t, "the request to reach the callee's handler", func() bool {
		select {
		case err := <-answered:
			t.Fatalf("the call finished before it reached the callee's handler: %v", err)
		default:
		}
		callee.FailIfExited(t, "before the request reached its handler")
		return callee.Wrote(holdingRequest)
	})

	killed := time.Now()
	callee.kill(t)

	select {
	case err := <-answered:
		assertVoyaDiagnostic(t, err, "other")
		t.Logf("caller was told %s after the kill: %v", time.Since(killed).Round(time.Millisecond), err)
	case <-time.After(diagnosticDeadline):
		t.Fatalf("the caller was still waiting %s after its callee was killed; "+
			"ADR 005 requires a diagnostic, not a hang", diagnosticDeadline)
	}
}
