// Command voya is a local emulator of Fly.io's networking model: one process
// that is registry, DNS server, and reverse proxy, so that a service dialing
// http://payments.internal:9777 works identically on a laptop and on Fly.
//
// See docs/adr/ for the design. The subcommands land over M1–M5; [commands]
// is the one table of them, which usage renders and dispatch reads, so a
// command the ADRs describe that is not built yet fails saying so rather
// than in the words a typo gets (#90).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/trendvidia/voya/internal/buildinfo"
	"github.com/trendvidia/voya/internal/config"
	"github.com/trendvidia/voya/internal/daemon"
)

// Ports voya owns on loopback. Fixed by ADR 001 and by fleet convention: every
// service declares 9777 in its fly.toml, so the dial string does not change
// between environments.
const (
	proxyPort        = 9777
	dnsPort          = 9778
	registrationPort = 9779
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		var status *exitStatus
		if errors.As(err, &status) {
			if status.err != nil {
				fmt.Fprintf(os.Stderr, "voya: %v\n", status.err)
			}
			os.Exit(status.code)
		}
		fmt.Fprintf(os.Stderr, "voya: %v\n", err)
		os.Exit(1)
	}
}

// exitStatus carries a specific exit code through run's error return.
// `dns check` needs one: its verdict is the code — 0 ok, 1 problem, 2 unknown
// (#99) — and a non-zero verdict is not a failure of the command, so err is
// nil when the report on stdout has already said everything and main exits
// without adding a line.
type exitStatus struct {
	code int
	err  error
}

func (e *exitStatus) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return fmt.Sprintf("exit status %d", e.code)
}

func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("voya", flag.ContinueOnError)
	// Report parse failures ourselves. Left to the flag package, the message
	// is printed twice — once here and once by main's "voya: %v" wrapper.
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}

	if err := fs.Parse(args); err != nil {
		// `voya -h` is a request for help, not a failure: it belongs on
		// stdout with a zero exit, like the `help` subcommand below.
		if errors.Is(err, flag.ErrHelp) {
			usage(stdout)
			return nil
		}
		usage(stderr)
		return err
	}

	if fs.NArg() == 0 {
		// Bare `voya` is a request for orientation, not a failure.
		usage(stdout)
		return nil
	}
	// The two-word join before the bare first word, so `voya dns install`
	// finds the entry named "dns install" rather than failing on a "dns" no
	// entry has. Six commands do not justify nested flag.FlagSet subcommand
	// machinery; this greedy-longest try is the one thing a plain map lookup
	// could not express. fs.Arg(1) is "" past the end, and no name ends in a
	// space, so the join simply misses when there is no second word.
	c := lookup(fs.Arg(0) + " " + fs.Arg(1))
	if c == nil {
		c = lookup(fs.Arg(0))
	}
	if c == nil {
		usage(stderr)
		return fmt.Errorf("unknown command %q", fs.Arg(0))
	}
	// The words after the command's own name. A command that does not take
	// arguments refuses them here, once, for all of them; one that takes
	// flags — `log`, `status`, `routes`, `health` — parses them itself.
	words := 1 + strings.Count(c.name, " ")
	rest := fs.Args()[words:]
	if !c.permissive {
		if err := noExtraArgs(fs, c); err != nil {
			usage(stderr)
			return err
		}
	}
	return c.run(rest, stdout, stderr)
}

// command is one row of [commands]: the name a shell types — which may
// contain a space, "dns install" — the summary usage prints beside it, and
// how to run it. Every row has a run; nil-run rows were the "Not implemented
// yet" block until #210 built the last of them.
type command struct {
	name    string
	summary string
	// permissive skips the extra-arguments check, for a command that parses
	// its own: `help`, where extra words are a request for help either way
	// and `voya help <command>` is the natural place for per-command usage
	// to land, and `log`, `status` and `routes`, which take flags.
	permissive bool
	// run is the command, handed the words after its own name — empty for
	// every command that is not permissive, since noExtraArgs refused them.
	run func(args []string, stdout, stderr io.Writer) error
}

// commands is the CLI surface, in the order usage prints it: the one table
// usage renders and dispatch reads (#90), so the two cannot disagree about
// which commands exist. Every row has a run since #210 built `token`, the
// last of the commands the ADRs described ahead of the code; the "Not
// implemented yet" machinery — the usage block, the nil-run dispatch
// refusal, and the two tests that held them to the table — retired with it.
// TestEveryCommandIsBuilt in main_test.go is what keeps this true — named
// the way renderCommands names its callers, because a doc link into a
// _test.go file resolves to nothing godoc renders.
var commands = []command{
	{name: "run", summary: fmt.Sprintf("Start the daemon: proxy on :%d, DNS on :%d, registration on :%d.",
		proxyPort, dnsPort, registrationPort),
		run: func(_ []string, _, stderr io.Writer) error { return runDaemon(stderr) }},
	{name: "status", summary: "One screen: voya, every registered service, evictions and reclaims; -health probes; -pxf for pxf.",
		permissive: true, run: statusCmd},
	// Beside `status`, because it is the same screen by being the same
	// function: the row moved here when it stopped being an unbuilt one, so
	// usage prints it among the commands rather than trailing `help` in the
	// slot it held while it had no run (#154).
	{name: "health", summary: "The status screen with HEALTH filled: probe every service over grpc.health.v1 on demand.",
		permissive: true, run: healthCmd},
	{name: "routes", summary: "Show the routing table: authority -> service -> upstream; -pxf for pxf.",
		permissive: true, run: routesCmd},
	{name: "log", summary: "Print the request log; -n N for the last N, -f to follow, -pxf for pxf.",
		permissive: true, run: logCmd},
	{name: "call", summary: "Call a service by name: a pxf request in, a pxf reply out, over the binary codec.",
		permissive: true, run: callCmd},
	// The last row to gain a run (#210): with it built, the "Not implemented
	// yet" block and the machinery that rendered it retired — see the table
	// comment above.
	{name: "token", summary: "Mint a signed JWT for a persona, valid or deliberately not (ADR 006).",
		permissive: true, run: tokenCmd},
	{name: "version", summary: "Print the build version.",
		run: func(_ []string, stdout, _ io.Writer) error { fmt.Fprintln(stdout, buildinfo.Version()); return nil }},
	{name: "help", summary: "Show this message.", permissive: true}, // run is wired in init below
	{name: "dns install", summary: "Point the OS resolver at voya for *.internal (opt-in, ADR 002); needs sudo.",
		permissive: true, run: dnsInstallCmd},
	{name: "dns check", summary: "Test how *.internal resolves on this host; exits 0 ok, 1 problem, 2 unknown.",
		permissive: true, run: dnsCheckCmd},
	{name: "dns uninstall", summary: "Remove the resolver file `dns install` wrote, and only that; needs sudo.",
		permissive: true, run: dnsUninstallCmd},
}

// help's run prints usage, and usage renders the table above: naming the
// function in the row literal would be an initialization cycle the compiler
// rejects, so it is wired here at run time instead.
func init() {
	lookup("help").run = func(_ []string, stdout, _ io.Writer) error {
		usage(stdout)
		return nil
	}
}

// lookup finds the table row named name, or nil.
func lookup(name string) *command {
	for i := range commands {
		if commands[i].name == name {
			return &commands[i]
		}
	}
	return nil
}

// noExtraArgs rejects anything trailing a subcommand that takes none.
//
// flag.Parse stops at the first non-flag argument, so everything after the
// subcommand — including flags — reaches us uninspected. Without this,
// `voya version --bogus junk` prints the version and exits 0 while `voya
// --bogus` fails, and a mistyped flag on the M1–M5 subcommands would be
// swallowed rather than reported. ADR 001 requires voya to fail loudly rather
// than quietly do nothing. The command's own name is not an extra argument,
// however many words of fs.Args it took to spell.
func noExtraArgs(fs *flag.FlagSet, c *command) error {
	words := 1 + strings.Count(c.name, " ")
	if fs.NArg() > words {
		return fmt.Errorf("%s takes no arguments, got %q", c.name, strings.Join(fs.Args()[words:], " "))
	}
	return nil
}

func usage(w io.Writer) {
	fmt.Fprint(w, `voya — local emulator of Fly.io's networking model.

Usage:
  voya <command> [flags]

Commands:
`)
	renderCommands(w)
	fmt.Fprint(w, "\nDesign: docs/adr/\n")
}

// renderCommands prints the rows of [commands], in table order. One name
// column, sized by the widest name in the table, with at least two spaces
// before the summary — the separator listsCommand in main_test.go cuts on.
func renderCommands(w io.Writer) {
	width := 0
	for _, c := range commands {
		width = max(width, len(c.name))
	}
	for _, c := range commands {
		fmt.Fprintf(w, "  %-*s  %s\n", width, c.name, c.summary)
	}
}

// runDaemon is `voya run`: resolve the configuration, bring the listeners up,
// and stay up until interrupted.
//
// Everything voya says goes to stderr; stdout carries command output only
// (#59). `voya run` has no product on stdout — the startup banner, ADR 002's
// eviction and reclaim announcements, and every failure the proxy, the DNS
// server and both http.Servers report are all diagnostics — so writing them
// there made `voya run > /dev/null`, the reflex for "stop printing the
// banner", also discard `voya: proxying %q: %v` and `voya: dns: replying to
// %s: %v`, which are exactly the lines a developer redirecting output still
// wants. The other redirection was worse: main's fatal path already wrote to
// stderr, so `voya run 2>/dev/null` kept every routine line and discarded only
// the startup failure — the one line nobody would choose to lose.
//
// Three things follow from putting all of it on stderr rather than splitting
// it by severity, which the levels #43 introduced would also have allowed.
// It leaves stdout free for what `voya status` and `voya routes` print in M2,
// so `voya status | jq` is honest without revisiting this. It keeps [Start]'s
// one logf a single seam — a per-band destination needs two functions or a
// caller-built sink, and that is an API change to the seam the acceptance
// suite drives. And it agrees with main's own fatal path, which already
// writes to stderr: voya's output was otherwise split by which layer produced
// it rather than by what it was.
//
// The shell is not how a developer asks voya to be quieter — log_level is
// (#43), and it is the setting that can tell the announcements from the noise.
//
// Signal handling lives here rather than in the daemon package so that the
// daemon stays a library a test can drive — the acceptance test starts and
// stops one directly, and a package that installs signal handlers cannot be
// driven that way.
func runDaemon(stderr io.Writer) error {
	logf := func(format string, args ...any) { fmt.Fprintf(stderr, format+"\n", args...) }

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(ctx, config.Dir())
	if err != nil {
		return err
	}

	d, err := daemon.Start(ctx, cfg, logf)
	if err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		// Through the daemon rather than through logf, so that this line is
		// subject to the log_level the daemon resolved (#43). logf is the raw
		// sink underneath it and prints whatever it is handed.
		d.Infof("voya: stopping (grace %s)", cfg.GetShutdownGrace().AsDuration())
	case err := <-d.Err():
		// A listener died while the process stayed up. ADR 001: voya is a
		// local single point of failure, so it has to fail visibly rather
		// than sit there no longer working.
		if closeErr := d.Close(); closeErr != nil {
			return fmt.Errorf("%w (and shutting down: %w)", err, closeErr)
		}
		return err
	}
	return d.Close()
}
