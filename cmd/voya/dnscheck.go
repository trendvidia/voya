package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"sync"
	"time"

	"github.com/trendvidia/voya/internal/config"
	"github.com/trendvidia/voya/internal/dnscheck"
	"github.com/trendvidia/voya/internal/resolverfile"
)

// dnsCheckProbeName is the *.internal name the check resolves. The wildcard
// answers any name under .internal, so this one is chosen to never collide
// with a service a developer might register.
const dnsCheckProbeName = "voya-dns-check-probe.internal"

// dnsCheckProbeBudget bounds each DNS probe on its own, not all of them
// together, for internal/dnscheck/probe's reason: the probes whose answer is
// "no" often spend a resolver timeout finding out, and a shared deadline
// would make those timeouts additive. The probes run concurrently, each
// under its own budget, so the budgets are not additive either (#253): the
// file-port dial is made wherever the file names a port, on hosts where no
// row will quote it, and a port that black-holes UDP used to add one whole
// budget to every run in series. A var so the test with a stub that never
// returns can shrink it.
var dnsCheckProbeBudget = 30 * time.Second

// The file checks are seams rather than direct calls so the gate between
// them is testable from any OS (#104): which of dnsCheckCmd's rows a test can
// exercise must be the test's choice, not the build machine's. The OS is
// [goos], the seam dns.go's commands share (#156), and the machine is
// [wiringLive], the seam it shares with dns.go's other host questions (#155)
// — as is [wiringGlobalCatchAll], the third question, asked behind the same
// gate (#185), [wiringResolvesThroughResolved], the fourth (#204), and
// [wiringResolvedTooOld], the fifth (#206).
var resolverInstalled = dnscheck.ResolverInstalled

// probeSystem is the system path's probe behind the same kind of seam, and
// for the same reason: which DNS row dnsCheckCmd reaches is decided by whether
// *.internal resolves on the build machine, so a test of what a row says —
// #185's catch-all sentence, say — is otherwise a test of the host. The other
// two are seams since #296, when an answer on either began to decide the
// verdict of the file-missing row: what this host's resolvers say must not
// decide whether a test of that row passes.
var (
	probeSystem  = dnscheck.ProbeSystem
	probeDefault = dnscheck.ProbeDefault
	probePureGo  = dnscheck.ProbePureGo
)

// probeAt and readResolverFile are the two halves of the file-port dial
// (#243): where the file is installed, the check reads the port the file names
// back out of it and asks that server directly, so the not-answering row
// states whether voya is answering there from measurement. Seams for
// probeSystem's reason — what the host's file says and what answers on its
// port must be the test's choice — and readResolverFile is the read, not the
// stat: [resolverInstalled] has already settled presence when this runs.
var (
	probeAt          = dnscheck.ProbeAt
	readResolverFile = func() ([]byte, error) { return os.ReadFile(resolverfile.Path) }
)

// osReadsResolverFile is the whole condition for the file meaning anything
// here, and it is two questions rather than one (#155): this OS has a wiring
// ADR 002 specifies, and this machine is running the thing that reads it.
// macOS answers the second with configd, which is the OS; Linux answers it
// with systemd-resolved, which a host may not be running at all — and a
// drop-in in /etc/systemd/resolved.conf.d on such a host is read by nobody, so
// its presence proves nothing and its absence must not be prescribed, exactly
// as #96 refuses to prescribe /etc/resolver on an OS that has no configd.
func osReadsResolverFile(ctx context.Context) bool {
	w, ok := resolverfile.For(goos)
	return ok && wiringLive(w, ctx)
}

// dnsCheckCmd is `voya dns check` (ADR 005, amendment 2026-10-31): the DNS
// check, actively measured — never a report that configuration exists — and
// reported with its state, verdict and evidence, in that language, on stdout,
// because the report is the command's product. The exit code is the verdict's
// — 0 ok, 1 problem, 2 unknown (#99) — and nothing else's.
func dnsCheckCmd(args []string, stdout, stderr io.Writer) error {
	done, err := parseFlags("dns check", dnsCheckUsage, args, stdout, stderr, func(*flag.FlagSet) {})
	if done {
		return err
	}
	report := dnsCheck(context.Background())
	renderDNSCheck(stdout, report)
	if code := report.Verdict.ExitCode(); code != 0 {
		return &exitStatus{code: code}
	}
	return nil
}

// dnsCheck runs the DNS probes and classifies them.
func dnsCheck(ctx context.Context) dnscheck.DNSReport {
	// Only the OS that reads the file gets the stat: anywhere else the file
	// is inert (#96), so no stat result — least of all a failure — may decide
	// anything there (#104). Where the stat is made and fails, the file goes
	// unmeasured with the stat's reason: neither presence nor absence was
	// established, and the probes below are still worth reporting.
	//
	// Where the stat is not made at all, the file goes unmeasured too, and for
	// the same reason: not looking leaves the state unestablished, and handing
	// that on as ResolverInstalled=false is the "missing" claim #104 exists to
	// refuse — one that would then be made of every non-darwin host, whether
	// the file is there or not, giving `voya dns check` a state word no probe
	// measured and a remedy naming a file this OS never reads (ADR 005).
	//
	// The catch-all is gated the same way and for the same reason (#185): it
	// is a question about the service that reads the file, so where nothing
	// reads it there is no scope to ask, and off Linux
	// [resolverfile.Wiring.GlobalCatchAll] says so itself. Either way the
	// answer arrives as unmeasured, never as false — a false nothing measured
	// would read as a host measured clear of it (#104). Whether the host's
	// own resolution reaches that service at all is the same kind of question
	// (#204), gated and carried the same way — as is whether that service is
	// old enough to reject the port the file names (#206), the question the
	// install asks before either of the other two.
	readsFile := osReadsResolverFile(ctx)
	var installed, catchAll, bypassed, tooOld bool
	var resolverUnmeasured, catchAllUnmeasured, bypassedWhy, bypassedUnmeasured, tooOldWhy, tooOldUnmeasured string
	var filePort, filePortUnmeasured string
	if readsFile {
		var err error
		if installed, err = resolverInstalled(); err != nil {
			resolverUnmeasured = err.Error()
		}
		w, _ := resolverfile.For(goos)
		if catchAll, err = wiringGlobalCatchAll(w, ctx); err != nil {
			catchAllUnmeasured = err.Error()
		}
		var through bool
		if through, bypassedWhy, err = wiringResolvesThroughResolved(w); err != nil {
			bypassedUnmeasured = err.Error()
		} else {
			bypassed = !through
		}
		if tooOld, tooOldWhy, err = wiringResolvedTooOld(w, ctx); err != nil {
			tooOldUnmeasured = err.Error()
		}
		// The file-port dial (#243) needs a port to dial, which only a file
		// whose presence was established can name; each way there is none is
		// carried as unmeasured, never as a silence somebody measured (#104).
		// And it needs the server the file names to be voya's: the dial is
		// made on [config.Loopback], the only address voya's listeners
		// bind, and a hand-written file naming some other server — which
		// `voya dns install` refuses to overwrite, so it persists — would
		// otherwise be measured at an address it does not name (#253).
		switch {
		case resolverUnmeasured != "":
			filePortUnmeasured = "the file's own state was never established"
		case !installed:
			filePortUnmeasured = "no file to name one"
		default:
			if contents, err := readResolverFile(); err != nil {
				filePortUnmeasured = err.Error()
			} else {
				filePort, filePortUnmeasured = loopbackPortOf(w, contents)
			}
		}
	} else {
		resolverUnmeasured = "no stat was made"
		catchAllUnmeasured = "no resolver scope was asked"
		bypassedUnmeasured = "no resolver service was asked"
		tooOldUnmeasured = "no resolver service was asked its version"
		filePortUnmeasured = "no file was read for a port"
	}
	// In process, [dnscheck.CGODisabled] is the caller's whole proof: unknown
	// build settings stay false, and the report says whose measurement it is.
	cgo0, _ := dnscheck.CGODisabled()

	// Each probe under its own budget, all of them at once: a probe that
	// spends its whole budget finding out costs the run one budget, not one
	// per probe (#253). Every result is written by its own goroutine and read
	// only after the wait.
	var system, def, pure, atFilePort dnscheck.ProbeResult
	var wg sync.WaitGroup
	probe := func(dst *dnscheck.ProbeResult, f func(context.Context, string) dnscheck.ProbeResult) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, dnsCheckProbeBudget)
			defer cancel()
			*dst = f(ctx, dnsCheckProbeName)
		}()
	}
	probe(&system, probeSystem)
	probe(&def, probeDefault)
	probe(&pure, probePureGo)
	atFilePort = dnscheck.ProbeResult{Unmeasured: filePortUnmeasured}
	if filePortUnmeasured == "" {
		server := config.Loopback + ":" + filePort
		probe(&atFilePort, func(ctx context.Context, name string) dnscheck.ProbeResult {
			return probeAt(ctx, server, name)
		})
	}
	wg.Wait()
	return dnscheck.ClassifyDNS(dnscheck.Probes{
		ResolverInstalled:         installed,
		ResolverUnmeasured:        resolverUnmeasured,
		OSReadsResolverFile:       readsFile,
		GlobalCatchAll:            catchAll,
		GlobalCatchAllUnmeasured:  catchAllUnmeasured,
		ServiceBypassed:           bypassed,
		ServiceBypassedWhy:        bypassedWhy,
		ServiceBypassedUnmeasured: bypassedUnmeasured,
		ServiceTooOld:             tooOld,
		ServiceTooOldWhy:          tooOldWhy,
		ServiceTooOldUnmeasured:   tooOldUnmeasured,
		System:                    system,
		Default:                   def,
		DefaultCGODisabled:        cgo0,
		PureGo:                    pure,
		AtFilePort:                atFilePort,
	})
}

// loopbackPortOf is the port the file-port dial asks, or why there is none
// (#253): the port the resolver file names, where the server it names is
// [config.Loopback], the only address voya's listeners bind. A file naming
// no port or no server names nothing to dial; one naming another address
// names a server that cannot be voya's, and dialling loopback in its place
// would measure a server the file does not name — so the dial is skipped
// and the reason quotes the address, which is what the evidence line then
// carries.
func loopbackPortOf(w resolverfile.Wiring, contents []byte) (port, unmeasured string) {
	host, port := w.ServerOf(contents)
	// The server before the port, so a file naming another address and no
	// port says which address it names rather than stopping at the missing
	// port: the address is the reason the dial would not have been made
	// either way, and the evidence line carries whichever reason this
	// returns.
	if host == "" {
		return "", "the file names no nameserver"
	}
	if ip, err := netip.ParseAddr(host); err != nil || ip != netip.MustParseAddr(config.Loopback) {
		return "", "the file names " + host + " rather than " + config.Loopback +
			", the only address voya's listeners bind"
	}
	if port == "" {
		return "", "the file names no port"
	}
	return port, ""
}

// renderDNSCheck writes the report: the check's verdict and state on one line
// — the verdict is the word the exit code derives from — then every sentence
// of its detail, evidence line included, then the verdict on the last line,
// since it is what the exit code is.
func renderDNSCheck(w io.Writer, r dnscheck.DNSReport) {
	fmt.Fprintf(w, "dns: %s — %s\n%s\n\nverdict: %s\n", r.Verdict, r.State, r.Detail, r.Verdict)
}

func dnsCheckUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  voya dns check

Resolve *.internal three ways — the system path, this binary, and Go's pure
resolver — and say what that means, never merely stat the resolver file. Run
it when curl or a browser cannot reach a *.internal name. Go services using
the voya client dialer do not depend on any of this.

The file is opt-in: its absence alone is ok, while one that is installed and
does not work, or an answer from anything else, is a problem. Exit code: 0 ok,
1 problem, 2 unknown (a probe that could not run is neither).
`)
}
