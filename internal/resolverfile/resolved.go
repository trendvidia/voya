package resolverfile

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The Linux half of the wiring is a service, not a directory the kernel
// reads: /etc/systemd/resolved.conf.d/voya.conf means nothing on a host that
// is not running systemd-resolved, and a `DNS=` with a port in it means
// nothing to a resolved older than v246. So Linux is the OS where voya has to
// ask the machine something before it writes, and ask it again before it
// claims the file is live. Everything it asks goes through [runCommand].
const (
	// resolvedUnit is the unit both questions are about. Named with its
	// suffix because `systemctl is-active systemd-resolved` would also match
	// a systemd-resolved.socket on a host that had one.
	resolvedUnit = "systemd-resolved.service"

	// resolvedMinVersion is the first systemd release whose resolved parses a
	// port in `DNS=` (v246, 2020). Below it the drop-in voya writes is a line
	// resolved cannot use, so voya refuses to write it rather than leave a
	// host quietly unwired. Ubuntu 20.04 ships v245 and is the host this
	// refusal is for; 22.04 ships v249 and 24.04 v255.
	//
	// Measured on a real v245 (ubuntu:20.04, systemd 245.4-4ubuntu3.24, as
	// PID 1 in a container) rather than taken from the changelog, because the
	// threshold decides which hosts voya turns away. resolved does not
	// silently drop the port and fall back to :53 — it rejects the whole
	// address and configures no server at all:
	//
	//	systemd-resolved[106]: Failed to add DNS server address
	//	'127.0.0.1:9778', ignoring: Invalid argument
	//
	// which is worse than a wrong port and exactly why this is a refusal.
	resolvedMinVersion = 246

	// CatchAllDomain is the routing domain that makes any scoped wiring
	// inert: "~." routes every name to the scope carrying it, so a name
	// voya's drop-in also claims is answered by whatever that scope's
	// servers say first. See [globalCatchAll]. Exported so `voya dns check`
	// names the same line this package refuses to install beside (#185).
	CatchAllDomain = "~."

	// stubAddress is the loopback address systemd-resolved's stub listener
	// answers on. An /etc/resolv.conf naming it as a nameserver sends the
	// system's own resolution through resolved, whatever file it is.
	stubAddress = "127.0.0.53"

	// resolvedRunDir is where resolved keeps the two resolv.conf files it
	// generates: stub-resolv.conf, naming [stubAddress], and resolv.conf,
	// naming the upstreams directly and still resolved's own. An
	// /etc/resolv.conf that is a symlink into this directory — absolute, or
	// Debian's relative `../run/systemd/resolve/…` — resolves through
	// resolved either way.
	resolvedRunDir = "/run/systemd/resolve"

	// StubResolvConf is the file resolved generates for /etc/resolv.conf to
	// point at, and so the first way out every refusal and every check row
	// on a host that does not resolve through resolved names. Exported for
	// the same reason [CatchAllDomain] is: `voya dns check` names the path this
	// package's own advice names, and one home keeps the two from drifting.
	StubResolvConf = resolvedRunDir + "/stub-resolv.conf"

	// nssResolveModule is what nsswitch.conf's `hosts:` line names when glibc
	// reaches resolved directly through nss-resolve, with no stub file in
	// sight.
	nssResolveModule = "resolve"
)

// etcDir is where the two files [resolvesThroughResolved] reads live:
// resolv.conf and nsswitch.conf. A var for the same reason [runCommand] is
// one — every Linux row, the host that runs resolved and does not resolve
// through it included, is a temp directory a test builds on any OS (#186).
var etcDir = "/etc"

// commandBudget bounds each helper voya runs here. systemctl talks to PID
// 1 over D-Bus, and a wedged bus is a hang rather than a failure — which
// on a command a developer ran with sudo is the worst of the three
// outcomes, because it looks like voya doing something.
//
// It is a ceiling under the caller's context, not a floor: every question
// here derives its deadline from the context it was handed, so a caller with
// less time — or a ^C — ends the wait sooner (#177). No caller has a
// cancellable context to pass today; the seam is what makes one possible.
//
// A var only so a test can shorten it and watch what an expired deadline says;
// nothing in the binary changes it.
var commandBudget = 5 * time.Second

// activateBudget bounds the one command here that is a job systemd runs
// rather than a question it answers: `systemctl restart` blocks until the
// job is done, and stopping a wedged resolved runs to the unit's
// TimeoutStopSec — 90s by systemd's default, and the wedged resolved is
// exactly the one someone reinstalls for. Sized past that, plus a margin
// for systemd to report the job's end over the bus, so a deadline voya
// names is voya's only after systemd has genuinely given up on the stop;
// on [commandBudget] the restart was killed at 5s while systemd's job
// carried on and mostly succeeded, and `dns install` reported a failure
// for a restart that worked (#188).
//
// It is sized against the stop and not against the whole job: a restart is a
// stop then a start, and resolved's start has its own TimeoutStartSec — so a
// host that spends the full stop timeout *and* whose new instance is slow to
// report ready can still outlive this, and gets the deadline message rather
// than a verdict. That is the trade taken: the stop is the half a wedged
// resolved hangs in, and a budget covering both halves in full would be over
// three minutes of waiting before voya said anything about a restart that was
// never coming back. The message it fails with is written for exactly this —
// it says systemd may still be running the job and what to ask.
//
// What made 5s right for the questions — a sudo command must not sit
// silent — holds here too, and is met the other way: [Wiring.Activate]
// says what it is waiting for before it starts waiting.
//
// A var for the same reason as [commandBudget].
var activateBudget = 90*time.Second + 10*time.Second

// runCommand is [exec.CommandContext], substitutable so every Linux row —
// the host without resolved, the resolved that is stopped, the version too
// old, the restart that fails — is testable from any OS on any host (#155).
// internal/dnscheck seams its own dscacheutil the same way (#100).
var runCommand = exec.CommandContext

// waitGrace is how long a killed command's pipes may stay open before voya
// stops waiting for them. It exists because a deadline that kills the process
// does not, on its own, end the call: os/exec waits for stdout and stderr to
// close, and anything the command left behind still holding them keeps the
// call running long past its budget.
//
// Not hypothetical, and the platforms disagree. `/bin/sh -c "sleep 30"` on
// macOS execs into sleep, so killing that pid ends it; on Linux dash forks,
// so the kill lands on the shell and the grandchild keeps the pipes — which
// is how a 300ms budget in TestActivate produced a 30s call on the Linux leg
// and not on macOS. internal/testproc documents the same hazard from the
// other side (leakPipesToAGrandchild) and bounds its Reap for it.
//
// Short, because by the time this matters the process is already killed and
// what is left is a wait on file descriptors nobody is going to close.
//
// A var only so a test can shorten it: it is the floor under how long a row
// whose deadline fires takes, and a row asserting one budget did not reach
// another needs that floor well under the budget it is measuring against.
var waitGrace = 2 * time.Second

// command is [runCommand] with the deadline made to bound the *call*. Every
// helper here goes through it, so no caller can acquire a budget that stops
// at the process and not at the wait.
func command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := runCommand(ctx, name, args...)
	cmd.WaitDelay = waitGrace
	return cmd
}

// UnavailableError is a machine that cannot carry the wiring its OS
// specifies: no systemd-resolved, a stopped one, one too old for a port in
// `DNS=`, or a systemctl voya could not run to find out. Reason is what voya
// established, Advice is what the developer can do instead — because the
// answer "not here" with nothing after it leaves someone who wanted
// *.internal resolving with nowhere to go (option 3 of #155).
type UnavailableError struct {
	Reason string
	Advice string
}

func (e *UnavailableError) Error() string {
	if e.Advice == "" {
		return e.Reason
	}
	return e.Reason + ". " + e.Advice
}

// stubAdvice is what a host without a usable systemd-resolved can do instead:
// the wiring by hand, for the developer whose machine voya cannot write for.
// It names no port because [Wiring.Preflight] runs before the configuration
// is loaded — deliberately, since a host with no resolved has no use for the
// port voya would have written.
const stubAdvice = "voya's Linux wiring is a systemd-resolved drop-in and this host does not run one; " +
	"send *.internal to voya's DNS listener yourself — the address `voya run` prints in its banner — " +
	"with whatever this host does resolve through (dnsmasq: `server=/internal/127.0.0.1#<dns_port>`), " +
	"or skip DNS entirely and dial through the voya client, which needs none of this"

// oldResolvedAdvice is [stubAdvice] for a host that *does* run resolved and
// whose resolved is simply too old to be told a port. Its own sentence
// because stubAdvice opens "this host does not run one", which is false here
// and was printed at a real v245 before this existed.
const oldResolvedAdvice = "voya will not write a line this resolved rejects outright — it logs " +
	"`Failed to add DNS server address …, ignoring: Invalid argument` and configures no server at all. " +
	"Send *.internal to voya's DNS listener yourself — the address `voya run` prints in its banner — " +
	"with whatever this host does resolve through (dnsmasq: `server=/internal/127.0.0.1#<dns_port>`), " +
	"or skip DNS entirely and dial through the voya client, which needs none of this"

// Preflight is what the wiring needs of this machine, established before
// anything is written and each failure said in its own words (#155). macOS
// needs nothing: configd is the OS. Linux needs systemd-resolved running, and
// new enough to understand the port voya puts in `DNS=`.
//
// Only install calls it. Uninstall does not: a file voya wrote is voya's to
// take back whether or not resolved is still running to read it, and a host
// that has since dropped resolved is exactly where a stranded file would sit
// forever.
//
// It establishes two things of resolved, and the second is the one that
// comes apart from the first on ordinary hosts (#186): that resolved is
// *there* and can carry the wiring, and that the host's own name resolution
// *goes through* it. A machine can run systemd-resolved while /etc/resolv.conf
// is a static file naming real upstream servers, or while dnsmasq or
// resolvconf sits in front of it — the default of many server images and
// containers, and NetworkManager's `dns=default` — and there resolved reads
// voya's drop-in that nothing ever asks: the install succeeds and *.internal
// still does not resolve. Measured 2026-08-18 in a debian:stable-slim
// container with systemd 257 as PID 1: `resolvectl query payments.internal`
// answered from the drop-in while `getent ahosts payments.internal` — the
// path curl, browsers and getaddrinfo take — answered nothing, and pointing
// /etc/resolv.conf at the stub was the one change that made it resolve. Such
// a host is refused, as the other three are: the file voya would write is a
// claim that *.internal resolves, and a warning would leave that claim
// standing on a success line. What the probe is, and why it is a disjunction
// of every shape that does work rather than a test for one blessed
// configuration, is [resolvesThroughResolved].
func (w Wiring) Preflight(ctx context.Context) error {
	if w.GOOS != "linux" {
		return nil
	}
	state, err := resolvedState(ctx)
	if err != nil {
		// A question the caller ended is the caller's to report — a ^C or a
		// deadline of its own is not a host that lacks resolved, and advice
		// on wiring around one would be advice about the wrong thing (#177).
		// [commandBudget] running out is not this: that context is ours,
		// and the caller's is still open.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return &UnavailableError{Reason: err.Error(), Advice: stubAdvice}
	}
	if state != "active" {
		return &UnavailableError{
			Reason: fmt.Sprintf("systemd-resolved is %s here (systemctl is-active %s)", state, resolvedUnit),
			Advice: stubAdvice,
		}
	}
	version, err := resolvedVersion(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return &UnavailableError{Reason: err.Error(),
			Advice: fmt.Sprintf("voya will not write DNS=127.0.0.1:<dns_port> without establishing that "+
				"resolved is v%d or newer, which is where a port in DNS= became legal", resolvedMinVersion)}
	}
	if version < resolvedMinVersion {
		return &UnavailableError{Reason: tooOldReason(version), Advice: oldResolvedAdvice}
	}
	if through, why, err := resolvesThroughResolved(); err == nil && !through {
		return &UnavailableError{
			Reason: "systemd-resolved is running here, but this host's own name resolution does not go " +
				"through it: " + why + " — so resolved would read voya's drop-in and nothing would " +
				"ever ask resolved",
			Advice: notThroughResolvedAdvice,
		}
	}
	// A question voya could not put is not a refusal. resolved is running and
	// new enough by here, so a resolvectl that fails this late is a wedged
	// bus rather than a host without the wiring — and blocking an install on
	// an unanswered question would strand a developer whose machine is fine.
	if catchAll, err := globalCatchAll(ctx); err == nil && catchAll {
		return &UnavailableError{
			Reason: "systemd-resolved's global scope already routes every name (" + CatchAllDomain +
				"), so the drop-in would be inert: DNS= and Domains= are separate lists there, " +
				"so voya's server joins the global list rather than binding to *.internal, and the " +
				"catch-all sends *.internal to whichever global server answers first",
			Advice: catchAllAdvice,
		}
	}
	return nil
}

// notThroughResolvedAdvice is what a host that runs resolved without
// resolving through it can do instead. It names the two ways of making the
// system ask resolved, because on this host they are the fix, and then the
// same two ways out every refusal offers.
const notThroughResolvedAdvice = "voya will not write a file this host would not consult. Either point the system " +
	"at resolved — `ln -sf " + StubResolvConf + " /etc/resolv.conf`, or NetworkManager's " +
	"`dns=systemd-resolved` — and install again, or send *.internal to voya's DNS listener yourself — the " +
	"address `voya run` prints in its banner — with whatever this host does resolve through (dnsmasq: " +
	"`server=/internal/127.0.0.1#<dns_port>`), or skip DNS entirely and dial through the voya client, " +
	"which needs none of this"

// resolvesThroughResolved reports whether this host's own name resolution —
// getaddrinfo's path, the one curl, browsers and `getent` take — reaches
// systemd-resolved at all. Nothing else in Preflight asks it: the state, the
// version and the catch-all are all questions about resolved, and a host can
// answer every one of them right while nothing on it ever asks resolved.
//
// It is a disjunction of the shapes that do work, not a test for one blessed
// configuration, because every false negative is a working host voya turns
// away (#186):
//
//   - /etc/resolv.conf is a symlink into /run/systemd/resolve — to
//     stub-resolv.conf (Ubuntu's default, naming 127.0.0.53) or to
//     resolv.conf (resolved's other file, naming the upstreams directly and
//     still resolved's own, so its contents alone would look static), and
//     through a chain of links as well as directly, since a host that
//     reaches resolved's file in two hops reaches it;
//   - /etc/resolv.conf, whatever file it is, names 127.0.0.53 as a
//     nameserver — a copy of the stub, or a hand-written one;
//   - /etc/nsswitch.conf lists `resolve` on its hosts line, so glibc reaches
//     resolved through nss-resolve with no stub file in sight.
//
// The one shape it cannot tell from a static file is a *copy* of
// /run/systemd/resolve/resolv.conf: upstreams by address, no stub, no
// nss-resolve. That host is refused, and the refusal's advice says how to
// point the system at resolved; #186 accepts that cost, because the
// alternative is a success line over a machine that will not do what it says.
//
// A file voya could not read for a reason other than its absence is an error
// and not a false: a question voya could not put is not a refusal, the same
// rule the catch-all check follows. A missing file is a false for that half —
// glibc without nsswitch.conf resolves as `files dns`, and without
// resolv.conf asks 127.0.0.1:53, and neither is resolved.
//
// On a false it also returns why, the clause [Wiring.Preflight] puts in the
// refusal, so that what the refusal says about each file is true of the host
// it is describing — "is missing" where there is no file to inspect.
func resolvesThroughResolved() (through bool, why string, err error) {
	resolvConf := filepath.Join(etcDir, "resolv.conf")
	// A link is what it points at, and Readlink is one hop of it. Both hops
	// are asked because they fail on different hosts: EvalSymlinks needs
	// every link in the chain to resolve to something that exists, and a
	// link left dangling still names resolved's directory; Readlink alone
	// stops at the first hop, and /etc/resolv.conf -> /etc/resolvconf/
	// resolv.conf -> /run/systemd/resolve/resolv.conf ends at resolved too.
	linkTarget, linkErr := os.Readlink(resolvConf)
	if linkErr == nil {
		if !filepath.IsAbs(linkTarget) {
			linkTarget = filepath.Join(etcDir, linkTarget)
		}
		if underResolvedRunDir(linkTarget) {
			return true, "", nil
		}
		if chased, err := filepath.EvalSymlinks(resolvConf); err == nil && underResolvedRunDir(chased) {
			return true, "", nil
		}
	}
	// Each half carries the reason it would give, because a refusal has to be
	// true of the host it describes: reporting that a file "names no
	// 127.0.0.53" on a host that has no such file sends the developer to
	// inspect nothing. A dangling symlink is not a missing file — it is there
	// and names something that is not resolved's — so the wording turns on
	// whether resolv.conf is a link at all, not on whether it could be read.
	resolvWhy := resolvConf + " is not resolved's stub and names no " + stubAddress
	contents, err := os.ReadFile(resolvConf)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if linkErr != nil {
			resolvWhy = resolvConf + " is missing"
		}
	case err != nil:
		return false, "", fmt.Errorf("reading %s: %w", resolvConf, err)
	default:
		for line := range strings.Lines(string(contents)) {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == "nameserver" && fields[1] == stubAddress {
				return true, "", nil
			}
		}
	}
	nsswitch := filepath.Join(etcDir, "nsswitch.conf")
	contents, err = os.ReadFile(nsswitch)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, resolvWhy + ", and " + nsswitch + " is missing", nil
	case err != nil:
		return false, "", fmt.Errorf("reading %s: %w", nsswitch, err)
	}
	nssWhy := nsswitch + " does not list `" + nssResolveModule + "` for hosts"
	for line := range strings.Lines(string(contents)) {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "hosts:")
		if !ok {
			continue
		}
		// glibc reads the first hosts line and no other, so this one decides.
		if slices.Contains(strings.Fields(rest), nssResolveModule) {
			return true, "", nil
		}
		break
	}
	return false, resolvWhy + ", and " + nssWhy, nil
}

// underResolvedRunDir reports whether path is one of the files resolved
// generates in [resolvedRunDir]. The test is on the tail of the path and not
// on the whole of it because Debian's link is `../run/systemd/resolve/
// stub-resolv.conf`, which joined onto a test's temp /etc lands under that
// directory rather than under /run.
func underResolvedRunDir(path string) bool {
	return strings.HasSuffix(filepath.ToSlash(filepath.Dir(path)), resolvedRunDir)
}

// catchAllAdvice is what a host whose resolver already claims every name can
// do instead. It names the scoped mechanisms rather than a file, because the
// thing this host lacks is not a file — it is any way for resolved.conf to
// bind a server to a domain.
const catchAllAdvice = "voya will not write a file this host cannot use. Either drop the catch-all — " +
	"a `Domains=~.` line in resolved.conf or one of its drop-ins, usually a pinned-resolver or VPN " +
	"policy — or send *.internal to voya through something that can scope a server to a domain " +
	"(dnsmasq: `server=/internal/127.0.0.1#<dns_port>`), or skip DNS entirely and dial through the " +
	"voya client, which needs none of this"

// globalCatchAll reports whether systemd-resolved's global scope already
// routes every name, which is the one configuration voya's drop-in cannot
// work alongside.
//
// Why it is fatal rather than untidy. `resolved.conf`'s `DNS=` and `Domains=`
// are two independent lists, not a scoped pair, and drop-ins accumulate —
// man 5 resolved.conf: "for options which accept a list of values, entries
// are collected as they occur in the sorted files". So voya's `DNS=` joins a
// flat global list instead of binding to *.internal, and a global `~.` sends
// every name, *.internal included, to whichever of those servers answers
// first. An upstream that answers NXDOMAIN for .internal ends the query
// there, because NXDOMAIN is a successful answer. Measured on a host
// configured that way: `resolvectl query payments.internal` not found, and
// zero packets to voya's DNS port across two lookups.
//
// It asks resolved instead of reading the drop-in directory, because the
// answer voya needs is the merged one — resolved.conf, every drop-in, and
// whatever NetworkManager or a VPN pushed. This is not the `resolvectl query`
// internal/dnscheck rejects as a resolution probe: that one asks resolved to
// resolve a name, which is a mechanism the system's own resolution may not
// use, and this asks resolved what it is configured to do, which only
// resolved can answer.
//
// Only the global scope counts. A per-link `~.` loses to voya's more
// specific `~internal` under resolved's longest-match routing, so refusing on
// one would refuse a host the wiring works on.
func globalCatchAll(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, commandBudget)
	defer cancel()
	out, err := command(ctx, "resolvectl", "domain").Output()
	if err != nil {
		// The context before the error, as for every question here: a
		// caller's deadline or cancellation kills the process, and "signal:
		// killed" names what voya did about it rather than why (#177).
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, fmt.Errorf("resolvectl domain: %w", ctxErr)
		}
		return false, fmt.Errorf("resolvectl domain: %w%s", err, quoted(stderrOf(err)))
	}
	for line := range strings.Lines(string(out)) {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "Global:")
		if !ok {
			continue
		}
		return slices.Contains(strings.Fields(rest), CatchAllDomain), nil
	}
	return false, nil
}

// GlobalCatchAll is [globalCatchAll]'s answer for the machine, asked by
// `voya dns check` of a host whose file is installed while the system does not
// resolve through it (#185): on that host the catch-all is a cause the file
// and voya are both innocent of, and the remedy that names them — is voya
// running, restart resolved — sends the developer to change nothing, twice.
// [Wiring.Preflight] refuses to write the file on such a host, so what the check
// meets is one wired by hand or by an older voya.
//
// Off Linux the question does not exist, and the error says so rather than
// answering false: macOS's /etc/resolver files are scoped by name in the
// mechanism itself, and an OS with no wiring has no scope to ask about. A
// false there would be a measurement nothing made (#104), and the check keeps
// the two apart.
func (w Wiring) GlobalCatchAll(ctx context.Context) (bool, error) {
	switch w.GOOS {
	case "linux":
		return globalCatchAll(ctx)
	case "darwin":
		return false, errors.New("configd routes each /etc/resolver file by its own name; there is no global scope to ask")
	}
	return false, fmt.Errorf("%s has no resolver wiring, so no scope to ask", w.GOOS)
}

// ResolvesThroughResolved is [resolvesThroughResolved]'s answer for the
// machine, asked by `voya dns check` of the same host [Wiring.GlobalCatchAll] is
// (#204): the file installed and the system not resolving through voya. On
// a host that runs resolved without resolving through it, the file was read
// and nothing ever asks its reader, so the remedy that names a restart — the
// one [Wiring.Preflight] refuses to prescribe by refusing the install — moves
// nothing, and the check owes the developer the cause instead. On a false the
// second value is the clause the refusal would carry, true of the two files
// as they were found.
//
// Off Linux the question does not exist, and the error says so rather than
// answering either way: macOS resolves through configd by construction, and
// an OS with no wiring has no service to reach. An answer there would be a
// measurement nothing made (#104), and the check keeps the two apart.
func (w Wiring) ResolvesThroughResolved() (through bool, why string, err error) {
	switch w.GOOS {
	case "linux":
		return resolvesThroughResolved()
	case "darwin":
		return false, "", errors.New("configd is the system's own resolver, so every lookup already goes through it; there is nothing to ask")
	}
	return false, "", fmt.Errorf("%s has no resolver wiring, so no service to reach", w.GOOS)
}

// tooOldReason is the clause a resolved older than [resolvedMinVersion] is
// refused with, shared by [Wiring.Preflight] and [Wiring.ResolvedTooOld] so
// that what the check predicts is what the install says.
func tooOldReason(version int) string {
	return fmt.Sprintf("systemd-resolved here is v%d, and a port in DNS= needs v%d or newer", version, resolvedMinVersion)
}

// ResolvedTooOld is whether the resolved this machine runs predates
// [resolvedMinVersion] — the first release whose resolved reads a port in
// `DNS=` — asked by `voya dns check` of the same host [Wiring.GlobalCatchAll]
// and [Wiring.ResolvesThroughResolved] are (#206). [Wiring.Preflight] asks
// it before either of those, so a check that predicts one of their refusals
// without asking this one predicts the wrong refusal on a host running v245:
// the one the developer meets there is the version, with a way out that
// names neither the bypass nor the catch-all. On a true the second value is
// the clause the refusal carries. A version that could not be read is an
// error, not a false: Preflight refuses on one too, but the check keeps a
// question nothing answered apart from a measured no (#104).
//
// It answers the version and nothing else. Preflight asks resolved's *state*
// ahead of it and refuses first on a host not running resolved at all, so a
// true here is the refusal the developer meets first only for a caller that
// has already established that state — `voya dns check` asks this behind
// [Wiring.Live], which is that same question, and a caller that does not owes
// itself the state before reading this answer as a place in line.
//
// Off Linux the question does not exist, and the error says so rather than
// answering either way: configd takes no port in a version-gated line, and
// an OS with no wiring has no service to ask the version of.
func (w Wiring) ResolvedTooOld(ctx context.Context) (tooOld bool, why string, err error) {
	switch w.GOOS {
	case "linux":
		version, err := resolvedVersion(ctx)
		if err != nil {
			return false, "", err
		}
		if version < resolvedMinVersion {
			return true, tooOldReason(version), nil
		}
		return false, "", nil
	case "darwin":
		return false, "", errors.New("configd is the system's own resolver and takes no port in a version-gated line; there is no version to ask")
	}
	return false, "", fmt.Errorf("%s has no resolver wiring, so no service to ask the version of", w.GOOS)
}

// Live reports whether this machine is running the mechanism its OS's wiring
// needs — the question [For] does not ask, because that one is about the OS.
// `voya dns check` and `voya status` gate the file's meaning on both: a
// drop-in on a host with no systemd-resolved is read by nothing, so its
// presence says nothing and its absence must not be prescribed (#96).
//
// A systemctl that could not be run answers false, not an error: from the
// file's point of view a machine voya cannot ask about resolved and a machine
// without resolved are the same machine. A context that ended before it
// answered is the same false — `voya status` must not be slowed by a host
// that is already wrong (ADR 005), and this is where its caller's deadline
// reaches the question (#177).
func (w Wiring) Live(ctx context.Context) bool {
	switch w.GOOS {
	case "darwin":
		return true
	case "linux":
		state, err := resolvedState(ctx)
		return err == nil && state == "active"
	}
	return false
}

// Activate makes what [Wiring.Install] wrote or [Wiring.Uninstall] removed
// live, and reports in the command's words what it did, or "" when there was
// nothing to do. macOS needs nothing — configd watches /etc/resolver and
// picks the file up as it lands. Linux needs systemd-resolved restarted:
// resolved reads its drop-ins at startup and not again.
//
// Callers restart only when the file changed ([Outcome.Changed]) or when
// asked to outright (`--activate`, #177): a restart is a live change to how
// the machine resolves every name, and an install that found its own file
// already in place has nothing to make live — unless the developer knows
// that file was never read, which is what the flag says. A
// resolved that is not running is not started either — voya's file is not a
// reason to turn on a service the developer left off, and it will be read
// when resolved next starts. That question is the caller's context's to end
// too, and there it is an error rather than "": a cancelled liveness question
// reported as nothing to do would be a restart that never ran, called done.
//
// logf hears one line before the restart, naming what voya is about to wait
// for: the restart runs on [activateBudget], long enough for systemd to give
// up on a wedged resolved, and a sudo command that waits that long without
// a word looks like voya hanging (#188). nil means say nothing — the seam
// cmd/voya points at stderr, and a caller with no reader passes nil.
//
// The line says the restart picks up the change to w.Path rather than that
// resolved will read it, because this is the uninstall path too: `voya dns
// uninstall` restarts a live resolved after removing that file, precisely so
// it stops reading it, and a line naming a file voya has just deleted as the
// reason to wait would be false there.
func (w Wiring) Activate(ctx context.Context, logf func(string, ...any)) (string, error) {
	if w.GOOS != "linux" {
		return "", nil
	}
	if !w.Live(ctx) {
		// [Wiring.Live] folds "voya could not ask" and "the context ended
		// before the answer came" into the same false, which is what a status
		// line wants. Here it is not: under --activate the caller asked for a
		// restart outright, and answering "" and nil off a question that was
		// cancelled reports nothing-to-do for a restart that never ran — the
		// file-is-right-and-unread state the flag exists to leave. So the
		// context is read back out before the silence is taken for an answer,
		// as every other question here does (#177).
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("systemctl is-active %s: %w", resolvedUnit, ctxErr)
		}
		return "", nil
	}
	if logf != nil {
		logf("restarting %s so it picks up the change to %s (up to %s)…", resolvedUnit, w.Path, activateBudget)
	}
	// WithTimeout keeps the earlier of the two deadlines, so which one it
	// kept is remembered here for the failure line below.
	callerDeadline, callerHasOne := ctx.Deadline()
	budgetEnds := time.Now().Add(activateBudget)
	ctx, cancel := context.WithTimeout(ctx, activateBudget)
	defer cancel()
	if out, err := command(ctx, "systemctl", "restart", resolvedUnit).CombinedOutput(); err != nil {
		// The deadline is read before the error, as it is for both questions
		// above: past [activateBudget] voya's client is killed while systemd's
		// job may carry on, so "signal: killed" would name the wrong thing
		// entirely — and this is the one failure a re-run does not clear
		// ([ReloadCommand]), which makes "restarted or not" the question the
		// message has to leave answerable rather than closed the wrong way.
		if ctxErr := ctx.Err(); ctxErr != nil {
			// A caller's own cancellation or deadline is not a budget that
			// ran out, and naming one would send the reader to the wrong
			// number (#177): the budget is named only when it was the
			// deadline that fired — ours is the earlier one unless the caller
			// brought a shorter.
			waited := "after " + activateBudget.String()
			if !errors.Is(ctxErr, context.DeadlineExceeded) || (callerHasOne && callerDeadline.Before(budgetEnds)) {
				waited = "before it finished"
			}
			return "", fmt.Errorf("systemctl restart %s: %w %s — systemd was asked and may still be "+
				"running the job; `systemctl status %s` is what says whether it finished",
				resolvedUnit, ctxErr, waited, resolvedUnit)
		}
		return "", fmt.Errorf("systemctl restart %s: %w%s", resolvedUnit, err, quoted(firstLine(out)))
	}
	return "systemd-resolved restarted", nil
}

// ReloadCommand is what a developer runs to make a written file live when
// voya did not — the restart [Wiring.Activate] does on Linux — and "" on an OS
// where the file is live as it lands, macOS's configd watching the directory.
//
// It exists because a failed [Wiring.Activate] is not recoverable by running
// the command again: the file on disk is already what voya would write, so the
// next install is [Unchanged] and activates nothing (deliberately — a restart
// is a live change to how the machine resolves every name). Whoever hits that
// failure has to make the file live themselves, so the failure says with what
// (#155). It names the command [Wiring.Activate] runs, which the test beside
// that one pins.
func (w Wiring) ReloadCommand() string {
	if w.GOOS == "linux" {
		return "systemctl restart " + resolvedUnit
	}
	return ""
}

// resolvedState is what systemd says the unit is: "active", "inactive" —
// which is also what a unit no systemd-resolved package ever installed
// answers — "failed", "activating". The error is for the other question,
// "voya could not ask": no systemctl on this host, or a deadline that fired
// before PID 1 answered.
//
// systemctl prints the state on stdout and exits nonzero for every state but
// active, so the exit status is read as a state and not as a failure: it is
// the empty answer that means the question never got through.
func resolvedState(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandBudget)
	defer cancel()
	cmd := command(ctx, "systemctl", "is-active", resolvedUnit)
	out, err := cmd.Output()
	if state := strings.TrimSpace(string(out)); state != "" {
		return state, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", fmt.Errorf("systemctl is-active %s: %w", resolvedUnit, ctxErr)
	}
	if err == nil {
		return "", fmt.Errorf("systemctl is-active %s said nothing", resolvedUnit)
	}
	return "", fmt.Errorf("systemctl is-active %s: %w%s", resolvedUnit, err, quoted(stderrOf(err)))
}

// resolvedVersion is the systemd release resolvectl reports, which is
// resolved's own: the two ship as one build, and resolvectl is in the same
// package as the daemon — so a host with resolved running has it. Its first
// line is `systemd 255 (255.4-1ubuntu8)`, and the release is the leading run
// of digits of the first field that has one.
//
// The leading run rather than the whole field, because PROJECT_VERSION is not
// always an integer: a pre-release prints `systemd 250-rc1 (250.rc1-1-arch)`
// or `systemd 255~rc1 (255~rc1-1)`, and reading either as "names no version"
// would refuse a host whose resolved is well past [resolvedMinVersion] — with
// the one refusal that carries no fallback wiring at all, since a version voya
// could not establish is not a host voya can tell what to do instead.
func resolvedVersion(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, commandBudget)
	defer cancel()
	out, err := command(ctx, "resolvectl", "--version").Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, fmt.Errorf("resolvectl --version: %w", ctxErr)
		}
		return 0, fmt.Errorf("resolvectl --version: %w%s", err, quoted(stderrOf(err)))
	}
	line := firstLine(out)
	for _, f := range strings.Fields(line) {
		if v, ok := leadingNumber(strings.TrimPrefix(f, "v")); ok {
			return v, nil
		}
	}
	return 0, fmt.Errorf("resolvectl --version said %q, which names no version", line)
}

// leadingNumber is the run of digits a field opens with, and false when it
// opens with anything else — so `255` and `250-rc1` are both read, and
// `systemd` and `(255.4-1ubuntu8)` are both passed over.
func leadingNumber(f string) (int, bool) {
	end := 0
	for end < len(f) && f[end] >= '0' && f[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	v, err := strconv.Atoi(f[:end])
	return v, err == nil
}

// stderrOf is what a failed command complained about, or "" — the tool's own
// words are the actionable half of "voya could not ask".
func stderrOf(err error) string {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return firstLine(exit.Stderr)
	}
	return ""
}

// firstLine is a tool's output bounded for quoting: the first non-blank line,
// capped, because the string is pasted into a message a developer reads and a
// screen of systemctl output would make it worse than the bare exit status.
// The cap backs up to a rune boundary — a localised tool's complaint is not
// ASCII, and half a rune reaches the message as a replacement character.
func firstLine(b []byte) string {
	const limit = 200
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if len(s) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

// quoted renders a complaint for the tail of a message, and nothing at all
// when there is none — an error ending in a bare colon is worse than one that
// stops.
func quoted(complaint string) string {
	if complaint == "" {
		return ""
	}
	return ": " + complaint
}
