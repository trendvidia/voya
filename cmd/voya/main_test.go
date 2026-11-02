package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/trendvidia/voya/internal/config"
	"github.com/trendvidia/voya/internal/daemon"
	"github.com/trendvidia/voya/internal/testproc"
)

// beVoyaEnv makes this test binary be `voya <its value>` — `run`, `log -f`
// — rather than a test run.
//
// The child goes through the same dispatch a developer's shell reaches, with
// the process's real os.Stdout and os.Stderr, because which of those two voya
// writes to is the whole of what [TestVoyaRunSaysNothingOnStdout] asserts —
// and an in-process call to [run] proves nothing about it, since the test
// would be handing it two buffers of its own choosing. The value is the
// argument list, [beVoyaSep]-separated.
const beVoyaEnv = "VOYA_TEST_BE_VOYA"

// beVoyaSep separates one child argument from the next inside [beVoyaEnv].
//
// A unit separator rather than the space this used to split on, because the
// space stopped being a character no argument contains when `voya call`
// arrived (#158): its request is one argument and every interesting one has
// spaces in it, so `text = "hi"` split on whitespace reaches the child as
// three arguments and it refuses a command line no shell would have
// produced. Nothing else about the encoding changed — an environment
// variable may carry any byte but NUL.
const beVoyaSep = "\x1f"

// beVoyaArgs is one child's [beVoyaEnv] setting: the command and its
// arguments, each kept whole.
func beVoyaArgs(args ...string) string {
	return beVoyaEnv + "=" + strings.Join(args, beVoyaSep)
}

func TestMain(m *testing.M) {
	if args := os.Getenv(beVoyaEnv); args != "" {
		beVoya(strings.Split(args, beVoyaSep))
	}
	os.Exit(m.Run())
}

// beVoya runs this binary as `voya args...`, and does not return.
//
// [main] itself with os.Args rewritten, rather than a copy of its body: the
// dispatch the child goes through, the "voya: %v" it prints when a command
// fails, and the code it exits with are then the real binary's by
// construction. A copy would keep asserting against a binary that had moved
// on the first time any of the three changed.
func beVoya(args []string) {
	// The liveness tie: this child stops when the test run that started it
	// does, so an interrupted `go test` does not leave a daemon holding three
	// ports. SIGTERM to self rather than an exit, so the shutdown path the
	// tests drive is the same one an abandoned child takes. Asserted by
	// [TestVoyaRunExitsWithItsParent], since the test this exists for would
	// pass just as well if it were a permanent block.
	//
	// Not for `call`, and that is the tie's mechanism rather than a
	// preference: [testproc.WaitForParent] reads the child's stdin to EOF,
	// which is where `voya call` reads its request from (#158) — the tie
	// would eat the document and then SIGTERM the process for having been
	// handed one. What the tie protects against is a child outliving an
	// interrupted `go test`, and a `voya call` cannot: a parent that goes
	// away drops the write end of that same pipe, so a read of stdin ends at
	// EOF exactly when the tie would have fired, and everything after the
	// read is bounded by [callDeadline] — on top of the context
	// [testproc.Start] gives every child.
	if len(args) == 0 || args[0] != "call" {
		testproc.StopWithParent()
	}

	os.Args = append([]string{"voya"}, args...)
	main()
	os.Exit(0)
}

func TestHelpNamesTheThreePorts(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"help"}, &stdout, &stderr); err != nil {
		t.Fatalf("help: %v", err)
	}
	for _, want := range []string{"9777", "9778", "9779"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("usage does not mention port %s:\n%s", want, stdout.String())
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("help wrote to stderr: %q", stderr.String())
	}
}

// The usage text above advertises the ports voya binds, and
// internal/config resolves the same three numbers from the committed base
// layer. Two copies, so pin them to each other: a default changed in
// base.pxf and not here would have `voya help` describing a daemon that
// listens somewhere else.
func TestAdvertisedPortsMatchConfigDefaults(t *testing.T) {
	cfg, err := config.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	for _, tc := range []struct {
		name        string
		advertised  uint32
		fromDefault uint32
	}{
		{"proxy", proxyPort, cfg.GetProxyPort()},
		{"dns", dnsPort, cfg.GetDnsPort()},
		{"registration", registrationPort, cfg.GetRegistrationPort()},
	} {
		if tc.advertised != tc.fromDefault {
			t.Errorf("%s: usage says %d, internal/config resolves %d",
				tc.name, tc.advertised, tc.fromDefault)
		}
	}
}

func TestNoArgsIsHelpNotAnError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(nil, &stdout, &stderr); err != nil {
		t.Fatalf("bare `voya`: %v", err)
	}
	if !strings.Contains(stdout.String(), "Usage:") {
		t.Errorf("bare `voya` printed no usage:\n%s", stdout.String())
	}
}

// `-h` and `--help` are requests for help, not failures: usage on stdout, no
// error, so `voya --help` exits 0 and can be piped like any other output.
func TestHelpFlagIsNotAnError(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := run([]string{arg}, &stdout, &stderr); err != nil {
				t.Fatalf("%s: %v", arg, err)
			}
			if !strings.Contains(stdout.String(), "Usage:") {
				t.Errorf("%s printed no usage to stdout:\n%s", arg, stdout.String())
			}
			if stderr.Len() != 0 {
				t.Errorf("%s wrote to stderr: %q", arg, stderr.String())
			}
		})
	}
}

// The flag package writes its own diagnostic, and main wraps the returned
// error in "voya: %v" — reporting only one of the two keeps the CLI from
// printing the same failure twice.
func TestUnknownFlagIsReportedOnce(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"-teleport"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("unknown flag returned nil error")
	}
	if !strings.Contains(err.Error(), "teleport") {
		t.Errorf("error does not name the flag received: %v", err)
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Errorf("unknown flag printed no usage to stderr:\n%s", stderr.String())
	}
	if strings.Contains(stderr.String(), "not defined") {
		t.Errorf("flag package diagnostic leaked to stderr; main also prints it:\n%s", stderr.String())
	}
}

// ADR 001: voya must fail loudly rather than quietly doing nothing.
func TestUnknownCommandFails(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"teleport"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("unknown command returned nil error")
	}
	if !strings.Contains(err.Error(), `"teleport"`) {
		t.Errorf("error does not name the command received: %v", err)
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Errorf("unknown command printed no usage to stderr:\n%s", stderr.String())
	}
}

// Dispatch tries the two-word join before the bare first word (#90), because
// "dns install" is one command whose name has a space, not a `dns` subtree:
// `voya dns install` must reach its table row — with or without trailing
// words, which the row's own handling then judges — while a first word the
// table does not know stays a typo named by that word, whatever follows it.
// The row runs since #156, so reaching it shows as its own gates speaking:
// off darwin, the not-specified line; the trailing word, its refusal.
func TestTwoWordCommandDispatch(t *testing.T) {
	old := goos
	goos = "plan9"
	t.Cleanup(func() { goos = old })
	for _, tc := range []struct {
		args  []string
		want  string
		usage bool // dispatch's own refusals print usage; a command that ran and declined does not
	}{
		{[]string{"dns", "install"}, "dns install: the resolver wiring on plan9 is not specified yet (#155)", false},
		{[]string{"dns", "install", "now"}, `dns install takes no arguments, got "now"`, true},
		{[]string{"dns", "uninstall"}, "dns uninstall: the resolver wiring on plan9 is not specified yet (#155)", false},
		{[]string{"dns"}, `unknown command "dns"`, true},
		{[]string{"dns", "instal"}, `unknown command "dns"`, true},
		{[]string{"teleport", "install"}, `unknown command "teleport"`, true},
	} {
		t.Run(strings.Join(tc.args, "_"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run(tc.args, &stdout, &stderr)
			if err == nil || err.Error() != tc.want {
				t.Errorf("run(%q) = %v, want %q", tc.args, err, tc.want)
			}
			if got := strings.Contains(stderr.String(), "Usage:"); got != tc.usage {
				t.Errorf("run(%q) printed usage to stderr = %v, want %v:\n%s", tc.args, got, tc.usage, stderr.String())
			}
		})
	}
}

// `help` stays permissive across the #90 dispatch rewrite: extra words after
// it are a request for help either way, so it is the one command the
// extra-arguments check skips.
func TestHelpIgnoresExtraWords(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"help", "dns", "install"}, &stdout, &stderr); err != nil {
		t.Fatalf("`voya help dns install`: %v", err)
	}
	if !strings.Contains(stdout.String(), "Usage:") {
		t.Errorf("`voya help dns install` printed no usage to stdout:\n%s", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("`voya help dns install` wrote to stderr: %q", stderr.String())
	}
}

// TestEveryCommandIsBuilt is what remains of the "Not implemented yet"
// machinery, pinned from the other side: #210 built `token`, the last
// command the ADRs described ahead of the code, so every row of [commands]
// now runs and usage admits no gap. The two tests that held the block to the
// table — TestUsageAdmitsTheUnbuiltCommands and
// TestTheUnbuiltCommandsStillHaveRows — retired with it, as their own
// positive controls demanded; if a command is ever again added ahead of its
// implementation, this test is the one that says the machinery has to come
// back with it.
func TestEveryCommandIsBuilt(t *testing.T) {
	for _, c := range commands {
		if c.run == nil {
			t.Errorf("the %q row has no run func, and the \"Not implemented yet\" machinery that "+
				"admitted such a row (usage block, dispatch refusal, and their tests) retired with #210 "+
				"— bring it back before shipping a command that does not exist", c.name)
		}
	}
	var stdout bytes.Buffer
	if err := run([]string{"help"}, &stdout, io.Discard); err != nil {
		t.Fatalf("help: %v", err)
	}
	if strings.Contains(stdout.String(), "Not implemented yet") {
		t.Errorf("usage still renders a \"Not implemented yet\" block:\n%s", stdout.String())
	}
}

// listsCommand reports whether block gives cmd an entry of its own: a line
// whose first column — everything before the run of spaces separating a
// command from its description — is exactly cmd. A name buried in some other
// line's description does not count, which is the whole point of asking.
func listsCommand(block, cmd string) bool {
	for _, line := range strings.Split(block, "\n") {
		if name, _, ok := strings.Cut(strings.TrimSpace(line), "  "); ok && name == cmd {
			return true
		}
	}
	return false
}

// flag.Parse stops at the first non-flag argument, so anything after the
// subcommand — junk words and flags alike — arrives uninspected. Accepting it
// would make `voya version --bogus` exit 0 while `voya --bogus` exits 1.
func TestExtraArgsAfterSubcommandFail(t *testing.T) {
	for _, args := range [][]string{
		{"version", "extra-junk"},
		{"version", "--bogus-flag"},
		{"version", "--bogus-flag", "extra-junk"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run(args, &stdout, &stderr)
			if err == nil {
				t.Fatalf("run(%q) returned nil error; stdout=%q", args, stdout.String())
			}
			if !strings.Contains(err.Error(), args[1]) {
				t.Errorf("error does not name the offending argument %q: %v", args[1], err)
			}
			if !strings.Contains(stderr.String(), "Usage:") {
				t.Errorf("no usage on stderr:\n%s", stderr.String())
			}
		})
	}
}

func TestVersionPrintsSomething(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"version"}, &stdout, &stderr); err != nil {
		t.Fatalf("version: %v", err)
	}
	if strings.TrimSpace(stdout.String()) == "" {
		t.Error("version printed nothing")
	}
}

// TestVoyaRunSaysNothingOnStdout is #59: `voya run > /dev/null` is the reflex
// for "stop printing the banner", and it used to discard the proxy's and the
// DNS server's failures with it, while `voya run 2>/dev/null` kept every
// routine line and discarded only the startup failure. Everything voya says
// is a diagnostic, so all of it goes to stderr and stdout is left for what
// `voya status` and `voya routes` print in M2.
//
// Run as a subprocess on the real streams. In-process, [run] would be handed
// two buffers this test picked, which is the one thing the claim is not about.
//
// The stderr checks are the control, and they bracket the daemon's whole life
// — the banner it prints once bound, and the line it prints on its way out.
// Without them an empty stdout would pass just as well for a voya that never
// started. The first half is spent by [voyaRun.waitUntilUp], which is what
// this test waits on rather than a dial, and the second is checked below.
func TestVoyaRunSaysNothingOnStdout(t *testing.T) {
	skipWithoutSIGTERM(t)

	v := startVoyaRun(t)
	v.waitUntilUp(t)

	// ^C, and the orderly shutdown that follows it.
	v.Signal(t, syscall.SIGTERM)
	v.WaitForExit(t, "after SIGTERM", shutdownBound)

	if out := v.Stdout.String(); out != "" {
		t.Errorf("`voya run` wrote to stdout, so `voya run > /dev/null` discards it:\n%s", out)
	}
	// The other end of the daemon's life. Reported rather than fatal, so that
	// a run which put both ends back on stdout fails saying that too, rather
	// than only that stderr is missing a line.
	if want := "voya: stopping"; !strings.Contains(v.Stderr.String(), want) {
		t.Errorf("stderr does not carry %q; voya said it somewhere else\nstderr:\n%s", want, v.Stderr.String())
	}
}

// TestVoyaRunExitsWithItsParent pins the orphan prevention in [beTheDaemon].
//
// The child reads stdin to EOF and stops itself, so a `go test` that dies
// without running its cleanup takes its daemon with it rather than leaving one
// holding three ports for the rest of the session. The test above kills the
// child outright and would pass just as well if that read were a permanent
// block, so the property is asserted rather than left to the comment on it.
func TestVoyaRunExitsWithItsParent(t *testing.T) {
	skipWithoutSIGTERM(t)

	v := startVoyaRun(t)
	v.waitUntilUp(t)

	// Closing the pipe is what an interrupted `go test` does to it by dying.
	if err := v.CloseStdin(); err != nil {
		t.Fatalf("closing the child's stdin: %v", err)
	}
	select {
	case <-v.Exited():
		if v.WaitErr() != nil {
			t.Errorf("voya exited with %v; EOF on stdin is an ordinary stop, not a failure\nstderr:\n%s",
				v.WaitErr(), v.Stderr.String())
		}
	case <-time.After(shutdownBound):
		t.Fatalf("voya is still running with its parent's end of the pipe closed; an interrupted "+
			"`go test` would leave it holding three ports\nstderr:\n%s", v.Stderr.String())
	}
}

// skipWithoutSIGTERM keeps the two subprocess tests to the platforms where the
// signal means what it says: a voya killed outright has no shutdown line to
// print and no orderly exit to assert.
func skipWithoutSIGTERM(t *testing.T) {
	t.Helper()
	testproc.SkipWithoutSignals(t, "`voya run` cannot be asked to stop politely there")
}

// How long this file waits for the child to come up and to be gone
// after SIGTERM. Wall clock on a loaded developer machine, so both are
// generous by orders of magnitude against three loopback binds and a 250ms
// shutdown grace. What they are for is turning a hang into a named failure
// with voya's output in hand, rather than the package's own -timeout arriving
// minutes later with a goroutine dump.
const (
	startupBound  = 20 * time.Second
	shutdownBound = 20 * time.Second
)

// voyaRun is a `voya run` in a process of its own: the real binary's dispatch,
// on the process's real stdout and stderr.
//
// The spawn, the output capture, the background wait, the bounded waits and
// the liveness tie are [testproc.Child]'s — written twice before #65 gave them
// one home. What is voya's, and stays here, is what "up" means.
type voyaRun struct {
	*testproc.Child

	// configDir is the daemon's config directory: three ports nothing else
	// holds, so a `voya log` pointed at the same directory reads this daemon
	// and not the developer's on 9777.
	configDir string
	ports     daemon.Ports
}

// startVoyaRun re-executes this test binary as `voya run` on ports nothing
// else holds, and stops it when the test ends. extraLayer is appended to the
// user layer, for a test that wants one setting different.
func startVoyaRun(t *testing.T, extraLayer ...string) *voyaRun {
	t.Helper()

	ports, err := daemon.FreePorts(t.Context())
	if err != nil {
		t.Fatalf("drawing free ports: %v", err)
	}
	// A config directory of its own, so this never collides with the
	// developer's own voya on 9777 — and never reads their voya.pxf either.
	dir := t.TempDir()
	layer := fmt.Sprintf("proxy_port = %d\ndns_port = %d\nregistration_port = %d\n",
		ports.Proxy, ports.DNS, ports.Registration) + strings.Join(extraLayer, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, config.UserFile), []byte(layer), 0o600); err != nil {
		t.Fatalf("write user layer: %v", err)
	}

	return &voyaRun{
		Child: testproc.Start(t.Context(), t, "voya run",
			beVoyaArgs("run"),
			config.DirEnv+"="+dir,
		),
		configDir: dir,
		ports:     ports,
	}
}

// waitUntilUp blocks until voya has said it is serving, and fails if it stops
// before it does.
//
// [daemon.Banner] is the readiness signal rather than a dial on the proxy
// port, and the difference is a flake rather than a preference: [daemon.Start]
// binds the proxy first and the DNS socket last, so the proxy port answers a
// dial while two listeners are still unbound — and the SIGTERM that would
// follow cancels the startup context, which registration.Listen and dns.Listen
// both deliberately refuse to bind under. voya would exit 1 on the signal sent
// to stop it cleanly. The banner is printed after all three binds, which is
// the property "up" is meant to name.
func (v *voyaRun) waitUntilUp(t *testing.T) {
	t.Helper()
	v.WaitForOutput(t, testproc.Stderr, daemon.Banner, "its startup line", startupBound)
}
