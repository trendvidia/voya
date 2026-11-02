package acceptance

// --- #66: the ERROR band's destination -----------------------------------------
//
// Every daemon the rest of this suite starts is in-process, handed a logf the
// test built — so "which of stdout and stderr did that line arrive on" is not
// a fact those tests can be about at all. cmd/voya's suite runs the real
// binary and pins that stdout stays empty, but for a run in which nothing
// failed: what it pins is the INFO band's destination. Today the two bands
// travel together through one logf; the per-message sink #19 will bring is
// exactly the change that can route one band to a different stream while that
// test stays green (internal/daemon/log.go says so in as many words).
//
// So the daemon here is a second process too — the test binary re-executed
// with [daemonEnv] set, on its real os.Stdout and os.Stderr — and the test
// provokes a real failure at it, which none of cmd/voya's tests can: that
// takes a registered service, and the client package is this suite's
// dependency, not that one's.

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	voya "github.com/trendvidia/voya/client"
	"github.com/trendvidia/voya/internal/config"
	"github.com/trendvidia/voya/internal/daemon"
	"github.com/trendvidia/voya/internal/testproc"
)

const (
	// daemonEnv makes this test binary be a voya daemon rather than a test
	// run — [serviceEnv]'s counterpart. TestMain reads it before anything
	// else runs.
	daemonEnv = "VOYA_ACCEPTANCE_DAEMON"

	// dnsPortEnv joins [proxyPortEnv] and [registrationPortEnv] for the
	// daemon role. The service role reads those two as the ports to dial;
	// this role reads all three as the ports to bind.
	dnsPortEnv = "VOYA_ACCEPTANCE_DNS_PORT"
)

// proxyingLine is the beginning of the first line #59 was filed about:
// internal/proxy writes `voya: proxying %q: %v` when a dial to a registered
// upstream fails. Matched by prefix, because the rest of the line is an
// authority and an errno this test has no business pinning.
const proxyingLine = "voya: proxying"

// dialFailed is the response half of the same ErrorHandler that writes
// [proxyingLine]: the proxy answers a request it routed to an upstream that
// would not accept a connection with `voya: … is registered on … but the
// connection failed: …` (ADR 005 — the diagnosis arrives where the developer
// is already looking).
//
// It is what makes the ERROR band's producer observable from this process,
// which is the whole of why it is matched here: the response and the log line
// come from the same two statements, so a caller that has read it knows the
// line was written and this test is looking for where it went rather than for
// whether it happened. Matched on the clause rather than the whole sentence,
// which carries an authority, an ephemeral address and an errno.
const dialFailed = "but the connection failed"

// runDaemonUntilStopped is the daemon subprocess: bind the three ports the
// parent drew, serve until the parent goes away, and stop in order.
//
// The logf is `voya run`'s wiring restated — everything voya says goes to its
// real stderr (#59) — because cmd/voya is package main and its runDaemon
// cannot be imported. Restating it is the point rather than a concession:
// when [daemon.Start]'s one-logf seam becomes a per-message sink under #19,
// this is the second call site the compiler finds, and the test driving it is
// the one that holds the ERROR band to stderr while cmd/voya's holds the INFO
// band.
func runDaemonUntilStopped() {
	cfg, err := configOn(envPort(proxyPortEnv), envPort(dnsPortEnv), envPort(registrationPortEnv))
	if err != nil {
		exitf("loading the committed defaults: %v", err)
	}
	d, err := daemon.Start(context.Background(), cfg,
		func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) })
	if err != nil {
		exitf("starting the daemon: %v", err)
	}

	// The liveness tie: this daemon stops when the test run that started it
	// does, so an interrupted `go test` does not leave one holding three
	// ports. [TestTheErrorBandReachesStderr] ends its child this way rather
	// than by signal, so the tie is asserted every run instead of trusted.
	parentGone := make(chan struct{})
	go func() {
		testproc.WaitForParent()
		close(parentGone)
	}()

	select {
	case <-parentGone:
	case err := <-d.Err():
		// The other half of runDaemon's wait, and restated for the same
		// reason the logf is. A listener that dies leaves the daemon up and
		// no longer working, which ADR 001 rules out; here it would also
		// leave this child alive, so [testproc.Child.FailIfExited] stays
		// quiet and the parent reports whatever it was waiting for as a
		// twenty-second timeout with nothing to say about why.
		// [runServiceUntilKilled] exits on its own Serve failure for exactly
		// this, and voya's errs channel is buffered, so nothing here but a
		// reader turns a listener death into a diagnostic.
		if closeErr := d.Close(); closeErr != nil {
			exitf("a listener died: %v (and shutting down: %v)", err, closeErr)
		}
		exitf("a listener died: %v", err)
	}
	if err := d.Close(); err != nil {
		exitf("stopping the daemon: %v", err)
	}
}

// realDaemon is a voya in a process of its own: real streams, so that which
// of them a message arrived on is a fact about the daemon rather than about a
// buffer this suite chose.
//
// The spawn, the capture that keeps the two streams apart, the background
// wait and the liveness tie are [testproc.Child]'s. What is this suite's is
// what "up" means, and which ports the caller and the registering service
// need — the two a [voyad] answers from its config.
type realDaemon struct {
	*testproc.Child

	proxyPort        uint32
	registrationPort uint32
}

// startRealDaemon re-executes this test binary as a daemon on ports nothing
// else holds, and waits for it to say it is serving.
//
// Unlike [startOnFreePorts] it does not retry the draw-to-bind race: a lost
// race surfaces as the child exiting with "already in use", quoted by
// FailIfExited below — the same exposure cmd/voya's harness accepts, because
// retrying would mean classifying a child's exit message across a process
// boundary.
func startRealDaemon(t *testing.T) *realDaemon {
	t.Helper()

	cfg := freeConfig(t)
	v := &realDaemon{
		Child: testproc.Start(t.Context(), t, "voya daemon",
			daemonEnv+"=1",
			portEnv(proxyPortEnv, cfg.GetProxyPort()),
			portEnv(dnsPortEnv, cfg.GetDnsPort()),
			portEnv(registrationPortEnv, cfg.GetRegistrationPort()),
			// The child mints the signing key at start (#219); a directory
			// of this test's own keeps it out of the developer's real
			// ~/.config/voya, and passed explicitly rather than trusted to
			// the inherited environment.
			config.DirEnv+"="+t.TempDir(),
		),
		proxyPort:        cfg.GetProxyPort(),
		registrationPort: cfg.GetRegistrationPort(),
	}

	// [daemon.Banner] is what "up" means, never a dial on the proxy port: the
	// proxy binds first, so its port answers while two listeners are still
	// unbound.
	v.WaitForOutput(t, testproc.Stderr, daemon.Banner, "its startup banner", waitDeadline)
	return v
}

// portEnv renders one port for a child's environment — [envPort]'s inverse.
func portEnv(key string, port uint32) string {
	return key + "=" + strconv.FormatUint(uint64(port), 10)
}

// TestTheErrorBandReachesStderr is #66: `voya: proxying`, provoked for real,
// arrives on the daemon's stderr and nothing arrives on its stdout — so
// `voya run > /dev/null`, the reflex for "stop printing the banner", cannot
// discard the one line that explains a failing call (#59).
func TestTheErrorBandReachesStderr(t *testing.T) {
	v := startRealDaemon(t)

	// A registered service whose upstream is gone — the cheapest real
	// failure. The listener is bound so Join registers a port that genuinely
	// was one, then closed so the proxy's dial is refused. The registration
	// stream is this process and the close does not touch it, so no eviction
	// happens: the name stays live, the request is routed rather than
	// answered with a routing diagnostic, and the dial's failure is what
	// reaches the proxy's ErrorHandler.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("binding the doomed upstream: %v", err)
	}
	// Registered at the bind rather than only closed below, because the Join
	// in between can fail: this listener would then be held for the rest of
	// the binary, on a machine where [daemon.FreePorts] is already drawing
	// ephemeral ports against everything else running. The close below is the
	// staging and stays checked; this one is the safety net, and a second
	// Close on a closed listener is an error nobody needs told about.
	t.Cleanup(func() { _ = ln.Close() })

	stop, err := voya.Join(context.Background(), "other", ln.Addr(),
		voya.WithRegistrationPort(v.registrationPort),
		// The port the caller below dials this service on, so the
		// acknowledgement's comparison agrees rather than reporting the
		// suite's ephemeral proxy port as a misconfigured fleet (#116).
		voya.WithProxyPort(v.proxyPort),
		voya.WithoutDialer(), // the caller below builds its own
		voya.WithBackoff(10*time.Millisecond, 200*time.Millisecond),
		voya.WithLogf(func(format string, args ...any) { t.Logf("doomed upstream: "+format, args...) }),
	)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	// Cleanup rather than inline: the registrar reconnects on its backoff
	// once the daemon below is gone, logging through t.Logf while it does,
	// and stop is what ends that before this test is over.
	t.Cleanup(stop)
	if err := ln.Close(); err != nil {
		t.Fatalf("closing the doomed upstream: %v", err)
	}

	// The mechanism first, and from this process rather than from the child's
	// output: drive calls until one is *routed* to the doomed upstream and
	// fails there, which is [dialFailed] in the caller's own error.
	//
	// It is the precondition the assertion below cannot see. Until the
	// registration lands the proxy answers "no service named", a different
	// path that writes nothing to the daemon's log at all — so a test that
	// went straight to watching for [proxyingLine] would report a
	// registration that never happened (a developer with FLY_APP_NAME
	// exported makes every voya.Join in this process a no-op) as an ERROR
	// band that never arrived, blaming the destination for the producer.
	// Asserting the setup is what tells those two apart.
	client := callerVia(t, v.proxyPort, "other")
	waitFor(t, "a call to be routed to the doomed upstream and fail there", func() bool {
		v.FailIfExited(t, "while calls were being driven at its proxy")
		_, err := call(t, client)
		if err == nil {
			// Nothing should be able to answer: this test closed that
			// listener. Something that took the freed ephemeral port would
			// otherwise leave the wait below timing out on an ERROR band
			// nothing ever had reason to write.
			t.Fatalf("a call to the doomed upstream succeeded, so something else took %s; "+
				"nothing here would provoke %q", ln.Addr(), proxyingLine)
		}
		return strings.Contains(err.Error(), dialFailed)
	})

	// So the line was written. This is the whole of #66: which stream it went
	// to.
	v.WaitForOutput(t, testproc.Stderr, proxyingLine, "the ERROR band's `"+proxyingLine+"` line", waitDeadline)

	// And nothing else reached stdout either, over the daemon's whole life —
	// [TestVoyaRunSaysNothingOnStdout]'s claim, now for a run in which
	// something failed. The pipe closing is how an interrupted `go test`
	// ends the child, so the orderly exit here is also the liveness tie
	// asserted rather than trusted.
	if err := v.CloseStdin(); err != nil {
		t.Fatalf("closing the daemon's stdin: %v", err)
	}
	v.WaitForExit(t, "after its parent's end of the pipe closed", waitDeadline)
	if out := v.Stdout.String(); out != "" {
		t.Errorf("the daemon wrote to stdout, so `voya run > /dev/null` discards it:\n%s", out)
	}
}
