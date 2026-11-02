package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/trendvidia/voya/internal/config"
	"github.com/trendvidia/voya/internal/resolverfile"
)

// The three things `dns install` and `dns uninstall` ask the machine, as
// seams, so every row of both commands is testable from any OS and any user
// (#156): which OS this is, whether the process is root, and — for the file —
// where it goes. goos picks the wiring, so one substitution moves the whole
// CLI to another OS; goos is also what the check and status gate the file on.
//
// resolverPath overrides the wiring's own path and is empty in production: a
// test points it into a temp directory and watches what the command writes —
// or, for the not-root row, that it writes nothing.
//
// The host-level half of the wiring is seamed for the same reason (#155):
// what systemd says about resolved is the machine's answer, not the OS's, and
// a test of these commands — or of the check's and status's gate, which asks
// [resolverfile.Wiring.Live] the same question — must be able to give every
// one of those answers without a systemd to give them. What the real ones do
// is pinned in [resolverfile]'s own tests, against the same recorded command
// seam.
var (
	goos         = runtime.GOOS
	geteuid      = os.Geteuid
	resolverPath string

	wiringPreflight               = resolverfile.Wiring.Preflight
	wiringActivate                = resolverfile.Wiring.Activate
	wiringLive                    = resolverfile.Wiring.Live
	wiringGlobalCatchAll          = resolverfile.Wiring.GlobalCatchAll
	wiringResolvesThroughResolved = resolverfile.Wiring.ResolvesThroughResolved
	wiringResolvedTooOld          = resolverfile.Wiring.ResolvedTooOld
)

// dnsInstallCmd is `voya dns install` (ADR 002): write the file that sends
// every *.internal query the system resolves — curl, grpcurl, browsers — to
// voya's DNS listener, on the port `voya run` reads from the same
// configuration, and say in one line on stdout what it did. Idempotent, and
// it never overwrites a file it did not write: [resolverfile] makes those
// distinctions and this reports them.
//
// It needs root — /etc is root's — and without it does not try and fail: it
// says the line to run and exits 1, before configuration is loaded, so the
// developer sees one instruction rather than a permission error under a
// stack of resolution. A refusal is exit 1 too, with the file's contents on
// stderr, so what voya left alone is in front of the developer who wrote it.
// On an OS ADR 002 specifies no wiring for, and on a Linux host not running
// systemd-resolved, nothing is written and the exit is 2 — the check's "cannot
// answer here" (#155).
//
// The line names where dns_port came from, because this command is run under
// sudo: a sudo that resets HOME resolves root's configuration directory,
// finds no user layer there, and would otherwise install voya's default port
// while `voya run` binds the developer's (#172).
func dnsInstallCmd(args []string, stdout, stderr io.Writer) error {
	force, done, err := activateFlag("dns install", args, stdout, stderr)
	if done {
		return err
	}
	w, err := resolverWiringHere("dns install", force)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := wiringPreflight(w, ctx); err != nil {
		return dnsFailure("dns install", err)
	}
	dir := config.Dir()
	cfg, layer, err := config.LoadOrigin(ctx, dir, "dns_port")
	if err != nil {
		return err
	}
	res, err := w.Install(cfg.GetDnsPort())
	if err != nil {
		return dnsFailure("dns install", err)
	}
	live, err := activate(ctx, "dns install", w, res.Outcome, force, stderr)
	if err != nil {
		return err
	}
	target := config.Addr(cfg.GetDnsPort())
	var head, unread string
	switch res.Outcome {
	case resolverfile.Installed:
		head = fmt.Sprintf("installed %s: *.internal -> %s", w.Path, target)
	case resolverfile.Unchanged:
		head = fmt.Sprintf("already installed: %s points *.internal at %s", w.Path, target)
		if !force && w.ReloadCommand() != "" {
			// The one line a developer stares at when the file is right and
			// the name still does not resolve — so this is where the way out
			// is named (#177). Not under --activate, which is that way out,
			// and not on an OS whose file is live as it lands.
			unread = activateHint
		}
	case resolverfile.Updated:
		// "was port N" only when N is not the port just written: voya's own
		// file is also rewritten when nothing but the marker's wording moved
		// on, and reporting a port change that did not happen sends the reader
		// looking for one.
		was := "rewrote an earlier voya file"
		if res.PreviousPort != "" && res.PreviousPort != strconv.FormatUint(uint64(cfg.GetDnsPort()), 10) {
			was = "was port " + res.PreviousPort
		}
		head = fmt.Sprintf("updated %s: *.internal -> %s (%s)", w.Path, target, was)
	default:
		return fmt.Errorf("dns install: unexpected outcome %v", res.Outcome)
	}
	fmt.Fprintln(stdout, oneLine(head, portProvenance(dir, layer), live, unread))
	return nil
}

// activateHint is what `already installed` says on Linux, where a file that
// is right can still be one systemd-resolved has never read: a restart that
// failed and whose message has scrolled away, a file restored from a backup,
// one copied between machines. voya cannot tell that state from the file —
// an mtime against the unit's ActiveEnterTimestamp lies at exactly those
// edges — so the developer says it, with the flag, and this is where the flag
// is named for someone in that state (#177).
const activateHint = "not restarted; if *.internal still does not resolve, `sudo voya dns install --activate` restarts systemd-resolved anyway"

// activateFlag parses the one flag both commands take: --activate restarts
// the service that reads the file whatever the outcome — `already installed`
// and `nothing to do` included — for a file that is right and unread (#177).
// Everything else about the parse is [parseFlags]'s, the same contract -pxf
// is parsed under: -h prints usage on stdout and exits 0, an unknown flag or
// a stray word prints it on stderr and fails.
func activateFlag(name string, args []string, stdout, stderr io.Writer) (force, done bool, err error) {
	var forceFlag *bool
	done, err = parseFlags(name, func(w io.Writer) { dnsUsage(w, name) }, args, stdout, stderr, func(fs *flag.FlagSet) {
		forceFlag = fs.Bool("activate", false, "restart the resolver even when the file did not change")
	})
	if done {
		return false, true, err
	}
	return *forceFlag, false, nil
}

// dnsUsage is `voya dns install -h` and `voya dns uninstall -h`: what the
// command does, and the one flag, named where a developer whose file is
// right and unread is already looking (#177).
func dnsUsage(w io.Writer, name string) {
	what := `Write the file that sends every *.internal query the system resolves to
voya's DNS listener, on the dns_port your configuration names, and on Linux
restart systemd-resolved so it reads the file. Idempotent: an identical file
is left alone, reported as already installed, and — since a restart is a live
change to how the machine resolves every name — not restarted.`
	if name == "dns uninstall" {
		what = `Remove the file dns install wrote, and only that: a file without voya's
marker is refused with its contents shown. On Linux a live systemd-resolved is
restarted after the removal so it stops reading the file. No file is nothing
to do, exit 0.`
	}
	fmt.Fprintf(w, `Usage:
  sudo voya %s [--activate]

%s

  --activate  restart systemd-resolved whatever the outcome, unchanged file
              included — for a file that is right but was never read: a
              restart that failed, a file restored from a backup or copied
              from another machine. Nothing to do on macOS, where configd
              reads the file as it lands.
`, name, what)
}

// dnsUninstallCmd is `voya dns uninstall`: remove the file `dns install`
// wrote, and only that — a file without voya's marker is refused with its
// contents shown, exactly as install refuses to overwrite it — so voya takes
// back exactly what it put down (ADR 002's objection to stale global state
// applies to voya's own file as much as to /etc/hosts). No file is nothing
// to do, said on stdout, exit 0. Root, refusal and the OS gate are as for
// install.
//
// What it does not do is preflight: a host that has stopped or removed
// systemd-resolved since the install is exactly where voya's file would
// otherwise sit forever, and removing it needs nothing of resolved.
func dnsUninstallCmd(args []string, stdout, stderr io.Writer) error {
	force, done, err := activateFlag("dns uninstall", args, stdout, stderr)
	if done {
		return err
	}
	w, err := resolverWiringHere("dns uninstall", force)
	if err != nil {
		return err
	}
	res, err := w.Uninstall()
	if err != nil {
		return dnsFailure("dns uninstall", err)
	}
	live, err := activate(context.Background(), "dns uninstall", w, res.Outcome, force, stderr)
	if err != nil {
		return err
	}
	var head string
	switch res.Outcome {
	case resolverfile.Removed:
		head = "removed " + w.Path
	case resolverfile.Absent:
		head = "nothing to do: " + w.Path + " is not installed"
	default:
		return fmt.Errorf("dns uninstall: unexpected outcome %v", res.Outcome)
	}
	fmt.Fprintln(stdout, oneLine(head, "", live))
	return nil
}

// resolverWiringHere is the two gates both commands share, in the order a
// developer should meet them: the OS first, because on one ADR 002 has not
// specified there is nothing sudo would help with; then root, because
// without it there is nothing to load configuration for.
//
// force is what the developer typed, so the sudo line can hand it back: a
// `--activate` dropped from that line sends them to a plain install, which
// finds the file [resolverfile.Unchanged] and restarts nothing — the state
// they ran the flag to leave, one round trip later (#177).
func resolverWiringHere(cmd string, force bool) (resolverfile.Wiring, error) {
	w, ok := resolverfile.For(goos)
	if !ok {
		return w, &exitStatus{code: 2, err: fmt.Errorf(
			"%s: the resolver wiring on %s is not specified yet (#155)", cmd, goos)}
	}
	if resolverPath != "" {
		w.Path = resolverPath
	}
	if geteuid() != 0 {
		again := cmd
		if force {
			again += " --activate"
		}
		return w, &exitStatus{code: 1, err: fmt.Errorf(
			"%s: %s is root's to touch; run:\n  sudo voya %s", cmd, w.Path, again)}
	}
	return w, nil
}

// activate makes a changed file live and hands back what to say about it, or
// "" when the OS needed nothing done. Only a file that changed is activated,
// unless force says otherwise: on Linux this restarts systemd-resolved, a
// live change to how the machine resolves every name, and an install that
// found its own file already in place has nothing to make live — except when
// the developer knows that file was never read, which is what `--activate`
// says (#177). Under the flag the restart happens whatever the outcome,
// `Unchanged` and `Absent` included: uninstall's argument is the same, a
// file removed by hand that a running resolved still holds.
//
// A failed restart is the command's failure, not a footnote on a success
// line: the file is written but nothing is reading it yet, which is the one
// outcome a developer must not take for a working install. It is also the one
// failure running the command again does not clear, which is why it says so
// itself — see [reactivation].
//
// The restart says what it is about to wait for, on stderr, before it waits:
// it runs on a budget long enough for systemd to give up on a wedged resolved,
// and everything voya says about itself goes to stderr — stdout keeps the one
// result line (#188).
func activate(ctx context.Context, cmd string, w resolverfile.Wiring, outcome resolverfile.Outcome, force bool, stderr io.Writer) (string, error) {
	if !outcome.Changed() && !force {
		return "", nil
	}
	live, err := wiringActivate(w, ctx, func(format string, args ...any) { fmt.Fprintf(stderr, format+"\n", args...) })
	if err != nil {
		return "", fmt.Errorf("%s: %w%s", cmd, err, reactivation(cmd, w, outcome))
	}
	return live, nil
}

// reactivation is the way back from a failed activation, said at the moment it
// fails because nothing says it afterwards: the file on disk is already what
// voya would write, so a second run of the same command finds nothing to
// change and — by design, since a restart is a live change to how the machine
// resolves every name — restarts nothing, and reports the file as already
// installed. Without this line that reads as success on a host where the file
// has never been read (#155). `--activate` is the run that does restart
// (#177), so the line names it, beside the systemctl it stands for.
//
// It names what happened on disk, because that is the half of a failed
// activation nothing else reports: the error is all the developer gets, no
// success line is printed, and an uninstall whose restart failed *did* remove
// the file — an install-shaped "the change is on disk" would leave them
// re-running the command to find out whether the drop-in is still there.
func reactivation(cmd string, w resolverfile.Wiring, outcome resolverfile.Outcome) string {
	reload := w.ReloadCommand()
	if reload == "" {
		return ""
	}
	did := "The file is written"
	if outcome == resolverfile.Removed {
		did = "The file is removed"
	}
	return fmt.Sprintf(". %s and nothing has read that yet: fix the cause and run "+
		"`sudo voya %s --activate` (or `sudo %s` yourself) — running `voya %s` again without "+
		"--activate finds nothing left to change and restarts nothing",
		did, cmd, reload, cmd)
}

// oneLine is what either command prints: what it did, then the clauses that
// are not always there — where the port came from, what was restarted —
// joined so the line reads as one sentence and greps as one line.
func oneLine(head string, clauses ...string) string {
	for _, c := range clauses {
		if c != "" {
			head += "; " + c
		}
	}
	return head
}

// portProvenance is the install line's answer to "which dns_port is this?"
// (#172). The layer that set it is chameleon's own answer, so the line names
// the developer's file when the developer's file is what voya read — and when
// it is not, says which file voya looked for and did not find, because the
// case this exists for is a sudo that reset HOME: `sudo -H voya dns install`,
// a sudoers with always_set_home, or a sudo wrapper resolves root's
// configuration directory, and voya's default port is written while `voya
// run` binds the one the developer configured. Nothing else in the line would
// give that away.
//
// Any layer but the user's is voya's committed default: base.pxf is the only
// other one in the chain, and config's own test pins that it sets every field
// of the schema.
func portProvenance(dir, layer string) string {
	if layer == config.UserLayer {
		return "dns_port from " + filepath.Join(dir, config.UserFile)
	}
	const fromBase = "dns_port is voya's built-in default"
	if dir == "" {
		return fromBase + "; this process has no configuration directory ($VOYA_CONFIG_DIR, $XDG_CONFIG_HOME and $HOME are all unset)"
	}
	path := filepath.Join(dir, config.UserFile)
	if _, err := os.Stat(path); err == nil {
		return fromBase + " (" + path + " sets no dns_port)"
	}
	return fromBase + " (no " + path + " on this HOME)"
}

// dnsFailure is a failure of either command as main prints it: a refusal
// carries the file's contents so the developer sees what voya would not
// touch, a host that cannot carry the wiring is the same exit 2 as an OS that
// has none — the check's "cannot answer here" — and any other error is the OS's
// own words under the command's name.
func dnsFailure(cmd string, err error) error {
	var refused *resolverfile.NotVoyasError
	if errors.As(err, &refused) {
		return &exitStatus{code: 1, err: fmt.Errorf("%s: refused — %w", cmd, err)}
	}
	var unavailable *resolverfile.UnavailableError
	if errors.As(err, &unavailable) {
		return &exitStatus{code: 2, err: fmt.Errorf("%s: %w", cmd, err)}
	}
	return fmt.Errorf("%s: %w", cmd, err)
}
