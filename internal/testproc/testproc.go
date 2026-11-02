// Package testproc runs the test binary again as a child process, and drives
// it.
//
// Several things voya has to prove cannot be proved in one process. A service
// that stops politely reaches voya as a deregistration rather than as a socket
// dying with its owner, so ADR 004's eviction path needs a process there is
// something to kill (#40). Which stream `voya run` writes to is not a claim
// about a buffer a test chose, so #59 needs the real binary on the real
// os.Stdout and os.Stderr. M2's `voya status` and `voya routes` will want the
// same thing again, for the same reason.
//
// It was written twice before this package existed, and the second copy had to
// rediscover every guard the first had already learned (#65): the argv[0] trap,
// the buffer that cannot be read before Wait, the wait that hangs instead of
// reporting a dead child, the bound that turns a hung shutdown into a named
// failure instead of a package timeout, and the liveness tie that keeps an
// interrupted `go test` from leaving a daemon holding three ports. None of
// those is obvious, each came from a specific failure, and nothing carried
// them across a package boundary except a reviewer remembering the other file
// existed.
//
// # The two halves
//
// A test binary running under testproc is in one of two roles, and it decides
// which by reading an environment variable in TestMain before anything else
// runs. The parent calls [Start]; the child calls [WaitForParent] or
// [StopWithParent] to tie its life to the parent's. What the child does in
// between is the caller's — this package deliberately knows nothing about
// daemons or services.
package testproc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// TB is the part of *testing.T this package uses.
//
// Declared rather than taking testing.TB so that no non-test package imports
// "testing", which registers the test flags into any binary that links it.
// Narrow on purpose: the set below is what a subprocess harness needs, and
// anything wanting more of *testing.T should take it at the call site.
type TB interface {
	Helper()
	Cleanup(func())
	Logf(format string, args ...any)
	Fatalf(format string, args ...any)
	Skipf(format string, args ...any)
}

// Output collects a child's stream while it is still running.
//
// The buffer exec.Cmd fills is written from a goroutine it owns, so reading it
// before Wait has returned needs the lock — and every caller reads while the
// child is up: it is how a harness knows the child came up, and how a wait
// that fails quotes what the child said instead of only how long it waited.
type Output struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (o *Output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.Write(p)
}

func (o *Output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.String()
}

// Child is the test binary running again in a process of its own.
type Child struct {
	cmd  *exec.Cmd
	name string

	// stdin is the write end of the child's liveness pipe. Held open for as
	// long as this process is, which is the whole point of it — see
	// [WaitForParent]. A child that reads a document rather than watching
	// for the close gets it through [Child.WriteStdin], which is why this is
	// the pipe and not only its Close.
	stdin io.WriteCloser

	// Stdout and Stderr are kept apart, even for callers that do not care.
	// One merged buffer cannot answer "which stream did that arrive on",
	// which is the entire question in #59, and [Child.Output] puts them back
	// together for a failure message.
	Stdout *Output
	Stderr *Output

	// exited is closed once cmd.Wait has returned, and waitErr is what it
	// returned. Waited for in the background from the moment the process
	// starts, so that every wait here can watch for the child dying as well as
	// for the thing it is really waiting on. waitErr is read only after a
	// receive on exited.
	exited  chan struct{}
	waitErr error

	logged sync.Once
}

// waitDelay bounds the two hangs cmd.Wait has of its own: a child that will
// not die once the context is cancelled, and a child that has exited leaving
// its output pipes held open by a grandchild that inherited them.
//
// Without it [Child.Reap] is the one wait in this package with no bound, and a
// cleanup meeting either would arrive as the package's own -timeout and a
// goroutine dump — which is the outcome every other bound here exists to
// avoid. Long enough that it can only ever fire on a pathology: what it waits
// for after the process is gone is a pipe drain.
//
// A var so that the test which stages that pathology can lower it — asserting
// the bound exists rather than waiting ten seconds to watch it hold.
var waitDelay = 10 * time.Second

// Start re-executes the test binary with env added to its environment, and
// kills it when the test ends.
//
// name is what the child is called in failure messages. env entries are
// "KEY=value", appended to os.Environ() — including, usefully, "KEY=" to clear
// one the developer has exported.
//
// ctx is a second and independent way the child dies, and t.Context is what to
// pass: it is cancelled just before the cleanups run, so a child outlives
// neither its test nor a cleanup that panicked on its way to [Child.Stop].
// Belt and braces on purpose — an orphaned daemon holds three ports for the
// rest of the session, and the cost of the second guard is a struct field.
//
// The child is started, not waited for. A caller decides what "up" means and
// waits for that: registration appearing in the registry, a banner on stderr,
// a line the child prints. Every such wait should call [Child.FailIfExited]
// each time round, because a child that has exited will never satisfy any of
// them.
//
// Each Start costs the test binary's own start-up, and under the race
// detector that floor is not small: about a second per child on darwin/arm64,
// against milliseconds without it, and it is the race runtime's — an empty
// main built with -race costs the same second before it runs (measured
// 2026-08-18: ~1.07s against ~55ms plain, on a loaded machine), so nothing
// the child does after main can bring it down. A test that spawns in a loop
// — polling a command until its output shows something — pays that floor per
// round; poll the daemon in process and spawn once for the output the test
// asserts on, the way cmd/voya's waitForStatusLine does (#164).
func Start(ctx context.Context, t TB, name string, env ...string) *Child {
	t.Helper()

	// os.Executable, not os.Args[0]. exec.Command runs a PATH lookup on any
	// argv[0] with no separator in it and resolves a relative one against the
	// working directory — so a binary built with `go test -c` and invoked by
	// name, or launched through a wrapper that rewrites argv[0], would
	// re-execute whatever PATH found instead of this test. Under plain `go
	// test` argv[0] is already absolute; this is the version that does not
	// depend on that.
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("finding this test binary: %v", err)
	}

	//nolint:gosec // G204: no shell is involved, and the interpolated value is
	// os.Executable() — the path of the process making the call. There is no
	// input here for a caller to taint.
	cmd := exec.CommandContext(ctx, self)
	cmd.Env = append(os.Environ(), env...)
	cmd.WaitDelay = waitDelay

	c := &Child{
		cmd:    cmd,
		name:   name,
		Stdout: &Output{},
		Stderr: &Output{},
		exited: make(chan struct{}),
	}
	cmd.Stdout, cmd.Stderr = c.Stdout, c.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe for %s: %v", name, err)
	}
	c.stdin = stdin

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the %s subprocess: %v", name, err)
	}
	go func() {
		c.waitErr = cmd.Wait()
		close(c.exited)
	}()
	t.Cleanup(func() { c.Stop(t) })
	return c
}

// PID is the child's process id.
func (c *Child) PID() int { return c.cmd.Process.Pid }

// Exited is closed once the child is gone. [Child.WaitErr] is readable after a
// receive on it, and not before.
func (c *Child) Exited() <-chan struct{} { return c.exited }

// WaitErr is what cmd.Wait returned. Only valid after a receive on
// [Child.Exited].
func (c *Child) WaitErr() error { return c.waitErr }

// Output is everything the child wrote, for a failure message. Stdout first
// and then stderr, so the two are not interleaved as they were emitted — a
// harness that needs the order of a single stream should read that stream.
func (c *Child) Output() string {
	return c.Stdout.String() + c.Stderr.String()
}

// Wrote reports whether the child has written substr to either stream.
func (c *Child) Wrote(substr string) bool {
	return strings.Contains(c.Output(), substr)
}

// FailIfExited fails the test if the child is already gone, quoting what it
// wrote on the way out.
//
// A child that has exited will never satisfy anything waited on, and every way
// it can fail at startup ends with the reason on its output. Without this a
// wait reports its own deadline instead and throws the child's explanation
// away — which is the difference between "voya did not register within 20s"
// and "the port you gave me is not a number".
func (c *Child) FailIfExited(t TB, when string) {
	t.Helper()
	if c.hasExited() {
		t.Fatalf("the %s subprocess exited %s: %v\n%s", c.name, when, c.waitErr, c.Output())
	}
}

// hasExited reports whether the child is already gone, without blocking. A
// wait that has just called [Child.FailIfExited] needs the same answer again
// to know whether to carry on, and that is one method rather than a second
// copy of the select.
func (c *Child) hasExited() bool {
	select {
	case <-c.exited:
		return true
	default:
		return false
	}
}

// Stream names one of the two streams [Start] captures. There are exactly
// two, which is what lets [Child.WaitForOutput] derive the stream a line must
// not arrive on from the one it must — a complement, not a parameter.
type Stream int

const (
	// Stdout is the child's standard output.
	Stdout Stream = iota
	// Stderr is the child's standard error.
	Stderr
)

// outputPoll is how often [Child.WaitForOutput] rereads the child's streams
// between checks.
const outputPoll = 10 * time.Millisecond

// WaitForOutput waits for want to arrive on one of the child's streams, and
// fails immediately if it arrives on the other. A [Child] captures exactly two
// streams, so the stream that must not carry the line is the complement of
// stream rather than a second argument. The wrong stream is a failure of its
// own instead of a timeout: it is the regression #59 was filed about, and
// waiting out the full bound to report the line as never arriving would bury
// the one line naming the fault.
//
// what is the subject of every failure — "the startup banner", "the ERROR
// band" — so that none of them leaves the reader matching a needle back to
// what it meant. within stays with the caller, because the bounds differ
// deliberately per suite and this helper takes no view.
//
// It waits for a needle on a stream, and only that. A wait on a predicate — a
// registration appearing in a registry, a port coming free — is a different
// shape even when its loop looks the same, and belongs to a caller-side loop
// like test/acceptance's waitFor rather than to a parameter here.
//
// The child is checked for having exited before either stream is read: a
// child that wrote the line and then died did not do what the caller is about
// to assume it did, and saying so here beats saying it one scope later,
// against whatever the caller went on to do. So this is a wait for a child
// that is meant to still be up. A child whose whole job is to write the line
// and exit races the poll against cmd.Wait; that one wants [Child.WaitForExit]
// and then a read of the stream, the way this package's own stream test does.
func (c *Child) WaitForOutput(t TB, stream Stream, want, what string, within time.Duration) {
	t.Helper()
	right, wrong, wrongName, discard := c.Stderr, c.Stdout, "stdout", "`> /dev/null`"
	if stream == Stdout {
		right, wrong, wrongName, discard = c.Stdout, c.Stderr, "stderr", "`2> /dev/null`"
	}
	deadline := time.Now().Add(within)
	for {
		c.FailIfExited(t, "before it wrote "+what)
		if c.hasExited() {
			// Unreachable under a real *testing.T, whose Fatalf does not
			// return. Under one whose Fatalf does — this package drives its
			// own failure paths that way — carrying on would report the same
			// exit once per poll until the bound, burying the first.
			return
		}
		if strings.Contains(right.String(), want) {
			return
		}
		if out := wrong.String(); strings.Contains(out, want) {
			// The command is not named: this package knows nothing about what
			// the child runs, and the redirect is the whole of the point.
			t.Fatalf("%s (%q) reached the %s subprocess's %s, so a run with %s would discard it (#59):\n%s",
				what, want, c.name, wrongName, discard, out)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the %s subprocess did not write %s (%q) on either stream within %s\nstderr:\n%s\nstdout:\n%s",
				c.name, what, want, within, c.Stderr.String(), c.Stdout.String())
			return
		}
		time.Sleep(outputPoll)
	}
}

// Signal sends sig to the child.
//
// It returns as soon as the signal is delivered and deliberately does not wait
// for the process. Reaping here would put a waitpid and the drain of two
// output pipes between the signal and whatever the caller measures next, and
// for the SIGKILL tests that interval *is* the measurement. [Child.Stop], run
// by the cleanup, does the waiting.
func (c *Child) Signal(t TB, sig syscall.Signal) {
	t.Helper()
	if err := c.cmd.Process.Signal(sig); err != nil {
		t.Fatalf("sending %v to %s: %v", sig, c.name, err)
	}
}

// CloseStdin closes the parent's end of the liveness pipe, which is what an
// interrupted `go test` does to it by dying. The child should then stop; see
// [WaitForParent].
func (c *Child) CloseStdin() error { return c.stdin.Close() }

// WriteStdin writes document to the child's stdin and closes it — a shell
// pipeline in two steps, for a child that reads one (`voya call` takes its
// pxf request that way when it was given none as an argument).
//
// The close is not optional and not a tidy-up: a command reading to EOF is
// waiting for exactly this. It is the same close [CloseStdin] makes, so a
// child that also treats stdin as the liveness pipe would read it as its
// parent going away — those two uses do not mix, and this one is for a child
// that exits on its own.
func (c *Child) WriteStdin(t TB, document string) {
	t.Helper()
	if _, err := io.WriteString(c.stdin, document); err != nil {
		t.Fatalf("writing %s's stdin: %v", c.name, err)
	}
	if err := c.stdin.Close(); err != nil {
		t.Fatalf("closing %s's stdin: %v", c.name, err)
	}
}

// WaitForExit waits for the child to be gone, within a bound, and fails if it
// exited badly.
//
// Bounded because a shutdown that hangs is exactly the class of bug a test
// driving a signal exists to catch: an unbounded wait would report it as the
// package's own -timeout and a goroutine dump, minutes later, instead of as
// itself with the child's output in hand.
//
// after says what the child was asked to do — "after SIGTERM" — and is quoted
// in both failures, the same way [Child.FailIfExited] quotes when. A wait that
// reports only its own duration leaves the reader of "still running after 20s"
// to work out from the call site what the child was meant to be responding to,
// which for a test whose whole subject is that response is the load-bearing
// half of the sentence.
func (c *Child) WaitForExit(t TB, after string, within time.Duration) {
	t.Helper()
	select {
	case <-c.exited:
		if c.waitErr != nil {
			t.Fatalf("the %s subprocess exited badly %s: %v\n%s", c.name, after, c.waitErr, c.Output())
		}
	case <-time.After(within):
		t.Fatalf("the %s subprocess was still running %s %s\n%s", c.name, within, after, c.Output())
	}
}

// Stop ends the child however it is still running, and is safe to call after a
// kill or after it has already exited on its own.
func (c *Child) Stop(t TB) {
	t.Helper()
	_ = c.stdin.Close()
	_ = c.cmd.Process.Kill()
	c.Reap(t)
}

// Reap waits for the child to be gone and logs anything it wrote.
//
// The waiting itself happens in the goroutine started with the process; this
// is the join. The log is emitted once however many callers arrive, so a test
// that stops the child itself and then hits the cleanup does not print it
// twice.
func (c *Child) Reap(t TB) {
	t.Helper()
	<-c.exited
	c.logged.Do(func() {
		if out := c.Output(); out != "" {
			t.Logf("%s subprocess wrote:\n%s", c.name, out)
		}
	})
}

// SkipWithoutSignals skips a test on platforms where os.Process.Signal takes
// nothing but Kill.
//
// because is appended to the skip message and should say what is lost, since
// the reader deciding whether that matters is the one looking at a skip in the
// output. voya's suites only have to run where developers work (ADR 001).
func SkipWithoutSignals(t TB, because string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skipf("no POSIX signals on Windows; %s", because)
	}
}

// --- the child half ----------------------------------------------------------

// WaitForParent blocks until the parent closes the child's stdin, and is how a
// child avoids outliving the test run that started it.
//
// The tests always stop their children themselves. A `go test` that dies
// without running its cleanup — its own -timeout firing, or a ^C — does not,
// and would otherwise leave a service holding a port or a daemon holding three
// for the rest of the session. The pipe closing is the parent being gone.
//
// Assert it. Every test that kills its child outright passes just as well if
// this read is a permanent block, so a suite relying on it needs one test that
// closes stdin and watches the child go.
func WaitForParent() {
	_, _ = io.Copy(io.Discard, os.Stdin)
}

// StopWithParent is [WaitForParent] for a child whose main work is in the
// foreground: it watches in the background and SIGTERMs this process when the
// parent goes away.
//
// The signal rather than os.Exit, so that a child with a shutdown path runs it
// — there is then one way out of the process rather than two, and the orderly
// exit a test drives is the same one an abandoned child takes.
//
// A signal that will not go through is the tie failing, and it fails silently:
// the goroutine returns, the child stays in its foreground work, and the
// orphan this exists to prevent holds its ports for the rest of the session
// with nothing anywhere saying why. So the error path exits instead — losing
// the orderly shutdown is the lesser of the two, and it says so on the way
// out. [SkipWithoutSignals] keeps tests off the platform where that is
// expected; it cannot make the tie work there.
func StopWithParent() {
	go func() {
		WaitForParent()
		self, err := os.FindProcess(os.Getpid())
		if err == nil {
			err = self.Signal(syscall.SIGTERM)
		}
		if err != nil {
			Exitf("testproc", "stopping with the parent: %v", err)
		}
	}()
}

// Exitf fails a child process loudly and does not return.
//
// A child has no testing.T to report through. Its output is captured by the
// parent and logged there, which is the only place anybody will look, so the
// prefix names which process is talking.
//
// The message is rendered before the prefix is put in front of it, rather than
// name being spliced into the format string: a name carrying a percent sign
// would otherwise consume the first argument as a verb of its own and leave
// the rest as %!v(MISSING) — destroying the one diagnostic this call exists to
// emit, in the one place anybody reads it.
func Exitf(name, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s subprocess: %s\n", name, strings.TrimRight(fmt.Sprintf(format, args...), "\n"))
	os.Exit(1)
}
