package testproc

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// This package exists so that the guards below are written once rather than
// rediscovered (#65), which only works if they are asserted here rather than
// left to the two suites that use it. Both of those drive a daemon or a
// service and would pass with most of this file broken: they wait on voya's
// banner and on a registration, not on the harness reporting what it says it
// reports. So the children here do nothing but exercise the harness.
//
// It re-executes itself the same way its callers do — [TestMain] reads
// [roleEnv] before anything else runs — which makes the package its own
// worked example.

const roleEnv = "VOYA_TESTPROC_ROLE"

const (
	roleStreams   = "streams"
	roleStreamsUp = "streams-up"
	roleWait      = "wait"
	roleStop      = "stop-with-parent"
	roleExitf     = "exitf"
	roleEnvEcho   = "env-echo"
	roleLeak      = "leak-pipes"
	roleSleep     = "sleep"
)

// What the roles write, so the assertions compare against the exact bytes
// rather than a fragment: which stream a line arrived on is the property this
// harness is built to preserve, and a Contains would pass with the two
// swapped.
const (
	streamsOnStdout = "this went to stdout\n"
	streamsOnStderr = "this went to stderr\n"
	tookTheSignal   = "took the signal\n"
)

// exitfName carries a percent verb because [Exitf] takes the name and the
// format from different places, and a name spliced into the format string
// would consume the first argument as a verb of its own.
const exitfName = "od%d"

// echoEnv stands in for FLY_APP_NAME, which test/acceptance clears in the
// child because a developer with it exported would otherwise get a service
// that serves and never registers.
const echoEnv = "VOYA_TESTPROC_ECHO"

// grandchildLifetime is how long the process in [roleLeak] holds the pipes its
// parent left behind. Long enough that a Reap with no bound on it is a hang
// rather than a slow pass, short enough that the stray process is gone well
// before `go test` is.
const grandchildLifetime = 5 * time.Second

// exitBound is what the tests here allow a child that is meant to be leaving.
// Generous by orders of magnitude against a process that writes two lines and
// returns; what it is for is turning a hang into a named failure.
const exitBound = 20 * time.Second

func TestMain(m *testing.M) {
	if role := os.Getenv(roleEnv); role != "" {
		runRole(role) // never returns
	}
	os.Exit(m.Run())
}

// runRole is the child half of every test below, and does not return.
func runRole(role string) {
	switch role {
	case roleStreams:
		fmt.Fprint(os.Stdout, streamsOnStdout)
		fmt.Fprint(os.Stderr, streamsOnStderr)
		os.Exit(0)

	case roleStreamsUp:
		// [roleStreams] with the exit held back: the WaitForOutput tests need a
		// child that has said its lines and is still up, or the exit check would
		// fire before the needle could be found.
		fmt.Fprint(os.Stdout, streamsOnStdout)
		fmt.Fprint(os.Stderr, streamsOnStderr)
		WaitForParent()
		os.Exit(0)

	case roleWait:
		WaitForParent()
		os.Exit(0)

	case roleStop:
		// Notified before [StopWithParent] starts watching, so the parent
		// cannot close stdin into a window where SIGTERM still has its default
		// disposition and the child dies of the signal instead of handling it.
		stopping := make(chan os.Signal, 1)
		signal.Notify(stopping, syscall.SIGTERM)
		StopWithParent()
		<-stopping
		fmt.Fprint(os.Stderr, tookTheSignal)
		os.Exit(0)

	case roleExitf:
		Exitf(exitfName, "boom: %s\n", "why")

	case roleEnvEcho:
		fmt.Fprint(os.Stdout, os.Getenv(echoEnv))
		os.Exit(0)

	case roleLeak:
		leakPipesToAGrandchild()

	case roleSleep:
		time.Sleep(grandchildLifetime)
		os.Exit(0)
	}
	Exitf("testproc", "unknown role %q", role)
}

// leakPipesToAGrandchild starts a process that outlives this one holding the
// write ends of its output pipes, and exits.
//
// os.Stdout and os.Stderr are *os.File, so exec hands those descriptors
// straight to the child rather than draining pipes of its own — which is the
// point: the parent's cmd.Wait sees no EOF once this process is gone, and
// waits on a drain that will not finish.
func leakPipesToAGrandchild() {
	self, err := os.Executable()
	if err != nil {
		Exitf("leaker", "finding this test binary: %v", err)
	}
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), roleEnv+"="+roleSleep)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		Exitf("leaker", "starting the grandchild: %v", err)
	}
	os.Exit(0)
}

// --- the parent half ----------------------------------------------------------

func TestOutputKeepsTheStreamsApartAndConcatenatesThemInOrder(t *testing.T) {
	c := Start(t.Context(), t, "streams", roleEnv+"="+roleStreams)
	// cmd.Wait returns only once the copies have finished, so everything the
	// child wrote is readable by the time this returns.
	c.WaitForExit(t, "on its own", exitBound)

	if got := c.Stdout.String(); got != streamsOnStdout {
		t.Errorf("Stdout = %q, want %q", got, streamsOnStdout)
	}
	if got := c.Stderr.String(); got != streamsOnStderr {
		t.Errorf("Stderr = %q, want %q", got, streamsOnStderr)
	}
	if got, want := c.Output(), streamsOnStdout+streamsOnStderr; got != want {
		t.Errorf("Output = %q, want %q", got, want)
	}
	for _, want := range []string{streamsOnStdout, streamsOnStderr} {
		if !c.Wrote(want) {
			t.Errorf("Wrote(%q) = false; the child wrote it", want)
		}
	}
	if c.Wrote("nothing here said this") {
		t.Error("Wrote reported a string the child never wrote")
	}
}

// FailIfExited is what keeps a wait from reporting its own deadline instead of
// the child's explanation, so both halves are asserted: that it fires with the
// output in hand, and that it stays quiet while the child is up.
func TestFailIfExitedQuotesTheChildAndStaysQuietWhileItIsUp(t *testing.T) {
	up := Start(t.Context(), t, "still up", roleEnv+"="+roleWait)
	live := &recordingTB{}
	up.FailIfExited(live, "while it was still up")
	if fatals := live.recorded(&live.fatals); len(fatals) != 0 {
		t.Errorf("FailIfExited failed a test whose child was running: %q", fatals)
	}

	gone := Start(t.Context(), t, "gone", roleEnv+"="+roleStreams)
	<-gone.Exited()
	dead := &recordingTB{}
	gone.FailIfExited(dead, "before anything waited on it")

	fatals := dead.recorded(&dead.fatals)
	if len(fatals) != 1 {
		t.Fatalf("FailIfExited on an exited child reported %d failures, want 1: %q", len(fatals), fatals)
	}
	// The name, the moment, and what the child said — the last is the whole
	// reason this method exists rather than letting the wait time out.
	for _, want := range []string{"gone", "before anything waited on it", streamsOnStderr} {
		if !strings.Contains(fatals[0], want) {
			t.Errorf("the failure does not carry %q:\n%s", want, fatals[0])
		}
	}
}

// The happy path, on both streams, because the stream a line must not arrive
// on is derived as the complement of the one asked for — waiting on each
// stream for the line that really is there is what pins the derivation both
// ways round.
func TestWaitForOutputFindsTheLineOnTheStreamAskedFor(t *testing.T) {
	c := Start(t.Context(), t, "chatty", roleEnv+"="+roleStreamsUp)
	rec := &recordingTB{}
	c.WaitForOutput(rec, Stderr, streamsOnStderr, "the stderr line", exitBound)
	c.WaitForOutput(rec, Stdout, streamsOnStdout, "the stdout line", exitBound)
	if fatals := rec.recorded(&rec.fatals); len(fatals) != 0 {
		t.Errorf("WaitForOutput failed on lines the child wrote where asked: %q", fatals)
	}
}

// The wrong stream is a failure of its own, not a timeout: the bound here is
// generous, so only the fail-fast branch can be the one that reports —
// asserted through the message, which names the stream and #59 rather than
// the deadline.
//
// Both directions, because the stream named and the redirect quoted are the
// complement's, and a message naming the stream that was asked for would send
// the reader to the one the line is not on. The subject deliberately contains
// neither stream's name: an assertion on a word this test itself put into the
// message holds whichever stream the helper goes on to name, and the phrases
// below can only have come from the helper.
func TestWaitForOutputFailsFastOnTheWrongStream(t *testing.T) {
	for _, tc := range []struct {
		name    string
		asked   Stream
		want    string
		names   string
		discard string
	}{
		{"asked for stderr, written to stdout", Stderr, streamsOnStdout, "subprocess's stdout", "`> /dev/null`"},
		{"asked for stdout, written to stderr", Stdout, streamsOnStderr, "subprocess's stderr", "`2> /dev/null`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const what = "a line the child put on the other one"
			c := Start(t.Context(), t, "crossed", roleEnv+"="+roleStreamsUp)
			rec := &recordingTB{}
			c.WaitForOutput(rec, tc.asked, tc.want, what, exitBound)

			fatals := rec.recorded(&rec.fatals)
			if len(fatals) != 1 {
				t.Fatalf("WaitForOutput on a crossed stream reported %d failures, want 1: %q", len(fatals), fatals)
			}
			for _, want := range []string{"crossed", what, tc.names, tc.discard, "#59", tc.want} {
				if !strings.Contains(fatals[0], want) {
					t.Errorf("the failure does not carry %q:\n%s", want, fatals[0])
				}
			}
		})
	}
}

func TestWaitForOutputTimesOutNamingBothStreams(t *testing.T) {
	c := Start(t.Context(), t, "silent on this", roleEnv+"="+roleStreamsUp)
	// Both real lines are on their streams before the short bound starts, so
	// the timeout's report of what the streams held has something to hold.
	c.WaitForOutput(t, Stderr, streamsOnStderr, "the stderr line", exitBound)
	c.WaitForOutput(t, Stdout, streamsOnStdout, "the stdout line", exitBound)

	rec := &recordingTB{}
	c.WaitForOutput(rec, Stderr, "a line nothing writes", "the missing line", 50*time.Millisecond)

	fatals := rec.recorded(&rec.fatals)
	if len(fatals) != 1 {
		t.Fatalf("WaitForOutput on an absent line reported %d failures, want 1: %q", len(fatals), fatals)
	}
	// The subject, the bound, and what both streams actually held — the
	// reader's next question either way.
	for _, want := range []string{"silent on this", "the missing line", "50ms", streamsOnStdout, streamsOnStderr} {
		if !strings.Contains(fatals[0], want) {
			t.Errorf("the failure does not carry %q:\n%s", want, fatals[0])
		}
	}
}

// The exit check runs before either stream is read, so a child that wrote the
// line and then died fails as an exit, not as a line found — the caller is
// about to assume a live process.
func TestWaitForOutputReportsAnExitBeforeReadingTheStreams(t *testing.T) {
	c := Start(t.Context(), t, "wrote and died", roleEnv+"="+roleStreams)
	<-c.Exited()
	rec := &recordingTB{}
	c.WaitForOutput(rec, Stderr, streamsOnStderr, "its parting line", exitBound)

	fatals := rec.recorded(&rec.fatals)
	if len(fatals) != 1 {
		t.Fatalf("WaitForOutput on an exited child reported %d failures, want 1; the line being present "+
			"must not outrank the exit: %q", len(fatals), fatals)
	}
	if !strings.Contains(fatals[0], "before it wrote its parting line") {
		t.Errorf("the failure is not the exit check's:\n%s", fatals[0])
	}
}

// And the exit ends the wait rather than being reported once per poll until
// the bound — [recordingTB]'s contract is that every Fatalf in this package is
// the last statement on its path, and a loop is where that stops being free.
// The needle is one nothing writes, so only the exit check can end this wait,
// and the bound is long against the poll so that a loop carrying on is fifty
// failures rather than one.
func TestWaitForOutputStopsAtTheExitRatherThanRepeatingIt(t *testing.T) {
	c := Start(t.Context(), t, "gone", roleEnv+"="+roleStreams)
	<-c.Exited()
	rec := &recordingTB{}
	c.WaitForOutput(rec, Stderr, "a line nothing writes", "the missing line", 500*time.Millisecond)

	if fatals := rec.recorded(&rec.fatals); len(fatals) != 1 {
		t.Errorf("WaitForOutput on an exited child reported %d failures, want 1: %q", len(fatals), fatals)
	}
}

// A wait that reports only its own duration leaves the reader to work out from
// the call site what the child was meant to be responding to.
func TestWaitForExitSaysWhatTheChildWasAskedToDo(t *testing.T) {
	c := Start(t.Context(), t, "unresponsive", roleEnv+"="+roleWait)

	rec := &recordingTB{}
	c.WaitForExit(rec, "after SIGTERM", 50*time.Millisecond)

	fatals := rec.recorded(&rec.fatals)
	if len(fatals) != 1 {
		t.Fatalf("WaitForExit on a running child reported %d failures, want 1: %q", len(fatals), fatals)
	}
	for _, want := range []string{"unresponsive", "after SIGTERM", "50ms"} {
		if !strings.Contains(fatals[0], want) {
			t.Errorf("the failure does not carry %q:\n%s", want, fatals[0])
		}
	}
}

// The liveness tie, asserted here as well as in both suites: a `go test` that
// dies without running its cleanups closes this pipe by dying, and the child
// has to take that as the parent being gone.
func TestTheChildStopsWhenTheParentClosesItsStdin(t *testing.T) {
	c := Start(t.Context(), t, "tied", roleEnv+"="+roleWait)
	if err := c.CloseStdin(); err != nil {
		t.Fatalf("closing the child's stdin: %v", err)
	}
	// Also asserts the exit is clean: EOF on stdin is an ordinary stop.
	c.WaitForExit(t, "with its parent's end of the pipe closed", exitBound)
}

// StopWithParent signals rather than exits, so that a child whose work is in
// the foreground runs its shutdown path — one way out of the process rather
// than two. The line the handler prints is what separates the two.
func TestStopWithParentSignalsRatherThanExiting(t *testing.T) {
	SkipWithoutSignals(t, "StopWithParent has no signal to send there")

	c := Start(t.Context(), t, "foreground", roleEnv+"="+roleStop)
	if err := c.CloseStdin(); err != nil {
		t.Fatalf("closing the child's stdin: %v", err)
	}
	c.WaitForExit(t, "with its parent's end of the pipe closed", exitBound)

	if !strings.Contains(c.Stderr.String(), tookTheSignal) {
		t.Errorf("the child's SIGTERM handler did not run; StopWithParent left the process some "+
			"other way and a child with a shutdown path would skip it\nstderr:\n%s", c.Stderr.String())
	}
}

// Reap is reachable twice — a test that stops its child and then hits the
// cleanup — and the child's output is long enough that printing it twice is a
// nuisance rather than a curiosity.
func TestReapLogsWhatTheChildWroteExactlyOnce(t *testing.T) {
	c := Start(t.Context(), t, "streams", roleEnv+"="+roleStreams)

	rec := &recordingTB{}
	c.Reap(rec)
	c.Stop(rec)
	c.Reap(rec)

	logs := rec.recorded(&rec.logs)
	if len(logs) != 1 {
		t.Fatalf("three joins logged %d times, want 1: %q", len(logs), logs)
	}
	if !strings.Contains(logs[0], streamsOnStdout) {
		t.Errorf("the log does not carry what the child wrote:\n%s", logs[0])
	}
}

// Exitf is a child's only way to say why it is going, on the only stream the
// parent will quote back.
func TestExitfNamesTheSpeakerAndSurvivesAPercentInTheName(t *testing.T) {
	c := Start(t.Context(), t, "exitf", roleEnv+"="+roleExitf)
	<-c.Exited()

	if c.WaitErr() == nil {
		t.Error("Exitf exited 0; a child that failed has to fail its parent too")
	}
	if want := exitfName + " subprocess: boom: why\n"; c.Stderr.String() != want {
		t.Errorf("stderr = %q, want %q", c.Stderr.String(), want)
	}
	if out := c.Stdout.String(); out != "" {
		t.Errorf("Exitf wrote to stdout: %q", out)
	}
}

// "KEY=" clearing an entry the developer exported is what test/acceptance
// relies on for FLY_APP_NAME, and it rests on exec keeping the last of a
// duplicated key rather than the first.
func TestStartClearsAnEntryTheDeveloperExported(t *testing.T) {
	t.Setenv(echoEnv, "from the developer's shell")

	inherited := Start(t.Context(), t, "inherits", roleEnv+"="+roleEnvEcho)
	inherited.WaitForExit(t, "on its own", exitBound)
	if got := inherited.Stdout.String(); got != "from the developer's shell" {
		t.Errorf("the child read %s as %q; env is meant to be inherited", echoEnv, got)
	}

	cleared := Start(t.Context(), t, "cleared", roleEnv+"="+roleEnvEcho, echoEnv+"=")
	cleared.WaitForExit(t, "on its own", exitBound)
	if got := cleared.Stdout.String(); got != "" {
		t.Errorf("the child read %s as %q; %q was meant to clear it", echoEnv, got, echoEnv+"=")
	}
}

// Reap is the one wait here with no bound of its own, so the bound is
// cmd.WaitDelay's. Without it a child that exits leaving its output pipes held
// open elsewhere hangs the cleanup, and `go test` reports the package's own
// -timeout and a goroutine dump minutes later — the outcome every other bound
// in this package exists to avoid.
func TestReapDoesNotHangOnAChildThatLeftItsPipesOpen(t *testing.T) {
	// Lowered so that what is asserted is the bound existing rather than its
	// value: the grandchild outlives what this test allows, so a Start that set
	// no WaitDelay fails here instead of passing slowly.
	restore := waitDelay
	waitDelay = 250 * time.Millisecond
	t.Cleanup(func() { waitDelay = restore })

	c := Start(t.Context(), t, "pipe leaker", roleEnv+"="+roleLeak)

	// Reaped off the test goroutine because the failure being guarded against
	// is that it never returns; nothing is reported from in here.
	reaped := make(chan struct{})
	go func() {
		defer close(reaped)
		c.Reap(&recordingTB{})
	}()

	allowed := grandchildLifetime / 2
	select {
	case <-reaped:
	case <-time.After(allowed):
		t.Fatalf("Reap was still waiting %s after a child exited leaving its output pipes open in "+
			"a grandchild; a cleanup meeting this would hang until `go test` timed the package out", allowed)
	}
}

// The skip is a claim about the platform, so it is asserted whichever platform
// this is: the arm that does not fire here is the one that matters, since a
// SkipWithoutSignals that skipped everywhere would take both suites'
// subprocess tests with it and say nothing.
func TestSkipWithoutSignalsSkipsOnWindowsAndNowhereElse(t *testing.T) {
	const because = "the tie cannot be driven there"

	rec := &recordingTB{}
	SkipWithoutSignals(rec, because)

	skips := rec.recorded(&rec.skips)
	if want := runtime.GOOS == "windows"; (len(skips) == 1) != want {
		t.Fatalf("on %s SkipWithoutSignals skipped %d times, want %t: %q",
			runtime.GOOS, len(skips), want, skips)
	}
	if len(skips) == 1 && !strings.Contains(skips[0], because) {
		t.Errorf("the skip does not say what is lost:\n%s", skips[0])
	}
}

// recordingTB records what the harness reports instead of failing, so the
// failure paths can be asserted rather than described.
//
// Every Fatalf in this package is the last statement on its path, so one that
// returns rather than calling runtime.Goexit leaves the code under test doing
// what it would have done under a real *testing.T. Anything here that grows a
// statement after a Fatalf has to stop being driven this way.
type recordingTB struct {
	mu     sync.Mutex
	fatals []string
	logs   []string
	skips  []string
}

func (r *recordingTB) Helper()        {}
func (r *recordingTB) Cleanup(func()) {}

func (r *recordingTB) Logf(format string, args ...any)   { r.record(&r.logs, format, args...) }
func (r *recordingTB) Fatalf(format string, args ...any) { r.record(&r.fatals, format, args...) }
func (r *recordingTB) Skipf(format string, args ...any)  { r.record(&r.skips, format, args...) }

func (r *recordingTB) record(into *[]string, format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	*into = append(*into, fmt.Sprintf(format, args...))
}

// recorded copies one of the slices above under the lock. Reap can be called
// from a goroutine of the test's choosing, so the reads are not all on the
// test goroutine.
func (r *recordingTB) recorded(from *[]string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), *from...)
}
