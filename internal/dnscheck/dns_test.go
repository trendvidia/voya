package dnscheck

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/trendvidia/voya/internal/dns"
	"github.com/trendvidia/voya/internal/resolverfile"
)

// TestClassifyDNS pins the three-state distinction #6 forbids collapsing —
// and the language, because the language is the deliverable: `voya dns check`
// saying "DNS: OK" is exactly what the issue rules out. The verdict column is
// #99's second axis: the state is what was measured, the verdict is whether
// that evidence answers ADR 005's question — and the unmeasured rows are #96,
// where a probe that could not run must never read as a measured "no".
func TestClassifyDNS(t *testing.T) {
	// The reload clause is the host's answer, not the classifier's (#155),
	// so the table runs as the host that has one: a row that bans "restart
	// systemd-resolved" (#185) is otherwise true on any host without it.
	// TestTheInstalledButNotAnsweringRemedyNamesTheReload holds the other host.
	restoreReload := resolverReload
	t.Cleanup(func() { resolverReload = restoreReload })
	resolverReload = "systemctl restart systemd-resolved.service"

	resolved := ProbeResult{Addrs: []string{"127.0.0.1"}}
	nxdomain := ProbeResult{Err: "lookup voya-dns-check-probe.internal: no such host"}
	// The two ways a system path goes unmeasured: no probe exists for this
	// OS, and the probe's own tooling could not run (#96, #100).
	offOS := ProbeResult{Unmeasured: "no system-path probe on plan9 " +
		"(the wiring ADR 002 specifies is darwin's /etc/resolver and Linux's systemd-resolved drop-in; #155)"}
	toolBroke := ProbeResult{Unmeasured: "dscacheutil: exit status 1: directory service on fire"}
	// The stat that is neither presence nor absence (#104): what
	// Probes.ResolverUnmeasured carries when the file could not be read, and
	// what it carries off the OS that reads the file, where dnsCheckCmd gates
	// the stat away and so establishes nothing about it at all.
	statFailed := "stat " + resolverfile.Path + ": permission denied"
	notConsulted := "no stat was made"
	// A pure-resolver probe with nothing behind it: a caller assembling
	// Probes itself — #134's own way in — or, since #253, ProbePureGo under
	// a budget that fired. The residue must not read that silence as a blind
	// resolver (#96), on either OS.
	pureUnmeasured := ProbeResult{Unmeasured: "the pure-resolver probe was not run"}
	// The same silence on this binary's own resolver, which every trap-shaped
	// row reads as the other half of the divergence (#253, #258): a budget
	// that fires reaches classify as this, and the trap must not be claimed —
	// nor exit 1 taken — off a lookup nobody completed.
	defaultUnmeasured := ProbeResult{Unmeasured: "lookup voya-dns-check-probe.internal: context deadline exceeded"}

	cases := []struct {
		name    string
		probes  Probes
		state   DNSState
		verdict Verdict
		// says is what the developer-facing sentence must contain; notSays
		// guards against a warning leaking into a state it is not true of.
		says    []string
		notSays []string
		// evidence is what the evidence line the detail ends with must
		// carry (#263): a quoted measurement the row's sentences are not
		// expected to repeat belongs here, not in says.
		evidence []string
	}{
		{
			// #296: the file is opt-in, so its absence alone is a clean
			// verdict, on what `go install` builds too — this row is a
			// cgo-enabled binary on purpose, because #99's CGO_ENABLED=0
			// condition is about vouching for an installed path.
			name:    "resolver not installed",
			probes:  Probes{ResolverInstalled: false, OSReadsResolverFile: true, System: nxdomain, Default: nxdomain, PureGo: nxdomain},
			state:   DNSResolverMissing,
			verdict: VerdictOK,
			says: []string{resolverfile.Path, "not the CGO_ENABLED=0 trap", "client dialer", "`sudo voya dns install` writes it",
				"opt-in, so its absence is not a problem"},
		},
		{
			// The pure resolver is the one path the row's prose does not
			// discuss, so it is the one that could carry an answer past a
			// clean verdict unseen: nothing here explains it, and only the
			// evidence line shows it.
			name: "resolver not installed, and only the pure resolver answers",
			probes: Probes{ResolverInstalled: false, OSReadsResolverFile: true, System: nxdomain, Default: nxdomain,
				PureGo: ProbeResult{Addrs: []string{"198.51.100.7"}}},
			state:   DNSResolverMissing,
			verdict: VerdictProblem,
			notSays: []string{"opt-in, so its absence is not a problem"},
		},
		{
			// The same silence as the Go binary's own probe, on the pure
			// resolver: a check nobody finished is not a pass (#104).
			name: "resolver not installed, and the pure resolver went unmeasured",
			probes: Probes{ResolverInstalled: false, OSReadsResolverFile: true, System: nxdomain, Default: nxdomain,
				PureGo: pureUnmeasured},
			state:   DNSResolverMissing,
			verdict: VerdictUnknown,
			says:    []string{"opt-in, so its absence is not a problem"},
		},
		{
			// #185 on the row before the file exists: the check measured the
			// catch-all, and `sudo voya dns install` — what this row ends
			// with — is a command Preflight refuses on such a host, so the
			// row says so rather than sending the developer to find out.
			name: "resolver not installed on a host whose global scope routes every name",
			probes: Probes{ResolverInstalled: false, OSReadsResolverFile: true, GlobalCatchAll: true,
				System: nxdomain, Default: nxdomain, PureGo: nxdomain},
			state:   DNSResolverMissing,
			verdict: VerdictOK,
			says: []string{"`sudo voya dns install` writes it", "will refuse here", "Domains=~.",
				"Drop the catch-all", "dnsmasq"},
		},
		{
			// #104's shape on that row too: a catch-all nobody measured is
			// not one that is there, and predicting a refusal from it would
			// be a claim about a host no resolvectl answered for.
			name: "resolver not installed, and the catch-all went unmeasured",
			probes: Probes{ResolverInstalled: false, OSReadsResolverFile: true, GlobalCatchAll: true,
				GlobalCatchAllUnmeasured: "resolvectl domain: exit status 1: bus wedged",
				System:                   nxdomain, Default: nxdomain, PureGo: nxdomain},
			state:   DNSResolverMissing,
			verdict: VerdictOK,
			says:    []string{"`sudo voya dns install` writes it"},
			notSays: []string{"will refuse here", "Drop the catch-all"},
		},
		{
			// #204 on the same row: a host that runs resolved without
			// resolving through it is refused by Preflight too, and first —
			// so that is the refusal the row spells out, and the way out it
			// names is the one that makes the file worth writing, then the
			// write.
			name: "resolver not installed on a host that runs resolved but does not resolve through it",
			probes: Probes{ResolverInstalled: false, OSReadsResolverFile: true,
				ServiceBypassed: true, ServiceBypassedWhy: "/etc/resolv.conf is not resolved's stub and names no 127.0.0.53",
				System: nxdomain, Default: nxdomain, PureGo: nxdomain},
			state:   DNSResolverMissing,
			verdict: VerdictOK,
			says: []string{"`sudo voya dns install` writes it", "will refuse here", "does not go through systemd-resolved",
				"/etc/resolv.conf is not resolved's stub", "stub-resolv.conf", "dns=systemd-resolved", "dnsmasq",
				"With the system pointed at resolved, `sudo voya dns install` writes the file"},
			notSays: []string{"Domains=~.", "Drop the catch-all", "could not be read"},
		},
		{
			// Both refusals measured: the bypass is the one the developer
			// meets first, so it is the one spelled out — but the promise
			// that pointing the system at resolved gets the file written is
			// false on this host, and a row that made it would send the
			// developer back for the second refusal. The catch-all is named
			// instead, ahead of where it would bite.
			name: "resolver not installed on a host that neither resolves through resolved nor scopes its global routing",
			probes: Probes{ResolverInstalled: false, OSReadsResolverFile: true, GlobalCatchAll: true,
				ServiceBypassed: true, ServiceBypassedWhy: "/etc/resolv.conf is not resolved's stub and names no 127.0.0.53",
				System: nxdomain, Default: nxdomain, PureGo: nxdomain},
			state:   DNSResolverMissing,
			verdict: VerdictOK,
			says: []string{"will refuse here", "does not go through systemd-resolved", "Domains=~.",
				"refuses on next", "that has to go too"},
			notSays: []string{"With the system pointed at resolved, `sudo voya dns install` writes the file",
				"could not be read", "Drop the catch-all"},
		},
		{
			// And a catch-all nobody could read is neither (#104): the
			// promise is not made, because the question behind it went
			// unasked, and the unread question is named with the command
			// that answers it.
			name: "resolver not installed on a bypassed host whose global routing could not be read",
			probes: Probes{ResolverInstalled: false, OSReadsResolverFile: true,
				GlobalCatchAllUnmeasured: "resolvectl domain: exit status 1: bus wedged",
				ServiceBypassed:          true, ServiceBypassedWhy: "/etc/resolv.conf is not resolved's stub and names no 127.0.0.53",
				System: nxdomain, Default: nxdomain, PureGo: nxdomain},
			state:   DNSResolverMissing,
			verdict: VerdictOK,
			says: []string{"will refuse here", "does not go through systemd-resolved", "one more question to put",
				"could not be read here (resolvectl domain: exit status 1: bus wedged)", "resolvectl domain` shows it"},
			notSays: []string{"With the system pointed at resolved, `sudo voya dns install` writes the file",
				"Drop the catch-all"},
		},
		{
			// #206 on the same row: Preflight asks resolved's version before
			// either of the questions above, so on a host running v245 the
			// refusal the developer meets is the version's — with a way out
			// around the file, not through it — whatever the other two
			// would have said. Both are measured true here so the row
			// proves which one is named.
			name: "resolver not installed on a host whose resolved is too old to be told a port",
			probes: Probes{ResolverInstalled: false, OSReadsResolverFile: true,
				ServiceTooOld: true, ServiceTooOldWhy: "systemd-resolved here is v245, and a port in DNS= needs v246 or newer",
				ServiceBypassed: true, ServiceBypassedWhy: "/etc/resolv.conf is not resolved's stub and names no 127.0.0.53",
				GlobalCatchAll: true, System: nxdomain, Default: nxdomain, PureGo: nxdomain},
			state:   DNSResolverMissing,
			verdict: VerdictOK,
			says: []string{"`sudo voya dns install` writes it", "will refuse here",
				"systemd-resolved here is v245, and a port in DNS= needs v246 or newer",
				"Failed to add DNS server address", "dnsmasq", "voya client"},
			notSays: []string{"does not go through systemd-resolved", "Domains=~.", "stub-resolv.conf",
				"writes the file", "could not be read", "first refusal"},
		},
		{
			// And a version nobody could read is not a measured no (#104):
			// the bypass is still the refusal the check can predict, but the
			// command asks the version first and refuses on one it cannot
			// read too, so the prediction no longer claims to be the first
			// refusal — and does not promise the write, since one question
			// the command asks went unanswered.
			name: "resolver not installed on a bypassed host whose resolved version could not be read",
			probes: Probes{ResolverInstalled: false, OSReadsResolverFile: true,
				ServiceTooOld: true, ServiceTooOldUnmeasured: "resolvectl --version: exit status 1: bus wedged",
				ServiceBypassed: true, ServiceBypassedWhy: "/etc/resolv.conf is not resolved's stub and names no 127.0.0.53",
				System: nxdomain, Default: nxdomain, PureGo: nxdomain},
			state:   DNSResolverMissing,
			verdict: VerdictOK,
			says: []string{"will refuse here", "does not go through systemd-resolved", "stub-resolv.conf",
				"Whether that is the first refusal here is not measured",
				"could not be read here (resolvectl --version: exit status 1: bus wedged)", "resolvectl --version` shows it"},
			notSays: []string{"With the system pointed at resolved, `sudo voya dns install` writes the file",
				"v245", "Failed to add DNS server address"},
		},
		{
			// The same unread version beside a measured catch-all: the
			// catch-all refusal is predicted and qualified the same way.
			name: "resolver not installed on a catch-all host whose resolved version could not be read",
			probes: Probes{ResolverInstalled: false, OSReadsResolverFile: true,
				ServiceTooOldUnmeasured: "resolvectl --version: exit status 1: bus wedged",
				GlobalCatchAll:          true, System: nxdomain, Default: nxdomain, PureGo: nxdomain},
			state:   DNSResolverMissing,
			verdict: VerdictOK,
			says: []string{"will refuse here", "Domains=~.", "Whether that is the first refusal here is not measured",
				"resolvectl --version` shows it"},
			notSays: []string{"does not go through systemd-resolved", "v245"},
		},
		{
			// The same shape one question along: the command asks whether
			// this host resolves through resolved before it asks about the
			// scope, and refuses on a bypass — so a catch-all refusal
			// predicted with that unread does not claim to be the first
			// either, and names the two files that answer it.
			name: "resolver not installed on a catch-all host whose own resolution path could not be read",
			probes: Probes{ResolverInstalled: false, OSReadsResolverFile: true,
				ServiceBypassedUnmeasured: "reading /etc/resolv.conf: permission denied",
				GlobalCatchAll:            true, System: nxdomain, Default: nxdomain, PureGo: nxdomain},
			state:   DNSResolverMissing,
			verdict: VerdictOK,
			says: []string{"will refuse here", "Domains=~.", "Whether that is the first refusal here is not measured",
				"could not be read here (reading /etc/resolv.conf: permission denied)", "/etc/nsswitch.conf"},
			notSays: []string{"resolvectl --version", "v245"},
		},
		{
			// And both of them unread: both are named, in the order the
			// command puts them, under one "not measured".
			name: "resolver not installed on a catch-all host with neither the version nor the resolution path read",
			probes: Probes{ResolverInstalled: false, OSReadsResolverFile: true,
				ServiceTooOldUnmeasured:   "resolvectl --version: exit status 1: bus wedged",
				ServiceBypassedUnmeasured: "reading /etc/resolv.conf: permission denied",
				GlobalCatchAll:            true, System: nxdomain, Default: nxdomain, PureGo: nxdomain},
			state:   DNSResolverMissing,
			verdict: VerdictOK,
			says: []string{"will refuse here", "Domains=~.", "Whether that is the first refusal here is not measured",
				"could not be read here (resolvectl --version: exit status 1: bus wedged)",
				"could not be read here (reading /etc/resolv.conf: permission denied)"},
			notSays: []string{"v245"},
		},
		{
			// And with no refusal predicted, an unread version adds nothing:
			// the plain remedy claims no place in line to qualify.
			name: "resolver not installed, and only the resolved version went unmeasured",
			probes: Probes{ResolverInstalled: false, OSReadsResolverFile: true,
				ServiceTooOldUnmeasured: "resolvectl --version: exit status 1: bus wedged",
				System:                  nxdomain, Default: nxdomain, PureGo: nxdomain},
			state:   DNSResolverMissing,
			verdict: VerdictOK,
			says:    []string{"`sudo voya dns install` writes it"},
			notSays: []string{"will refuse here", "first refusal", "resolvectl --version"},
		},
		{
			// And unmeasured is not bypassed (#104): the plain row, with no
			// refusal predicted from a file nobody read.
			name: "resolver not installed, and whether the host resolves through resolved went unmeasured",
			probes: Probes{ResolverInstalled: false, OSReadsResolverFile: true, ServiceBypassed: true,
				ServiceBypassedUnmeasured: "reading /etc/resolv.conf: permission denied",
				System:                    nxdomain, Default: nxdomain, PureGo: nxdomain},
			state:   DNSResolverMissing,
			verdict: VerdictOK,
			says:    []string{"`sudo voya dns install` writes it"},
			notSays: []string{"will refuse here", "does not go through systemd-resolved"},
		},
		{
			// The plain not-installed row says two things that are false the
			// moment the system answers anyway: that nothing resolves, and
			// that this is therefore not the trap. With the Go binary blind
			// beside a resolving system, the developer is in the trap's
			// shape — arriving by a route the missing file cannot explain —
			// and must not be told the opposite.
			name:    "resolver not installed, yet something answers",
			probes:  Probes{ResolverInstalled: false, OSReadsResolverFile: true, System: resolved, Default: nxdomain, PureGo: nxdomain},
			state:   DNSResolverMissing,
			verdict: VerdictProblem,
			says:    []string{"something other than that file is answering", "split ADR 002 documents", "client dialer"},
			notSays: []string{"not the CGO_ENABLED=0 trap", "fail exactly like Go does", "To give curl and browsers the names",
				"opt-in, so its absence is not a problem"},
		},
		{
			// Same unexplained answer, but both paths have it: there is no
			// split, so the split must not be reported.
			name:    "resolver not installed, yet everything answers",
			probes:  Probes{ResolverInstalled: false, OSReadsResolverFile: true, System: resolved, Default: resolved, PureGo: resolved},
			state:   DNSResolverMissing,
			verdict: VerdictProblem,
			says:    []string{"something other than that file is answering"},
			notSays: []string{"split ADR 002 documents", "not the CGO_ENABLED=0 trap", "To give curl and browsers the names"},
		},
		{
			// The plain row mirrored: the system has nothing and the Go
			// binary resolved anyway. "curl and browsers fail exactly like Go
			// does" is a claim about both paths, and this row is the two of
			// them disagreeing — it would contradict the measurements printed
			// three lines below it.
			name:    "resolver not installed, and only the Go binary resolves",
			probes:  Probes{ResolverInstalled: false, OSReadsResolverFile: true, System: nxdomain, Default: resolved, PureGo: resolved},
			state:   DNSResolverMissing,
			verdict: VerdictProblem,
			says:    []string{"curl and browsers get nothing", "the other way round", "`sudo voya dns install` writes it"},
			notSays: []string{"fail exactly like Go does", "something other than that file is answering"},
		},
		{
			// The plain row with the Go side unmeasured (#260): "fail exactly
			// like Go does" is a claim about both paths and only the system's
			// was measured, so the row names what went unmeasured and why.
			// Ruling the trap out stands on the system measurement alone —
			// the trap is the system path resolving while this binary's does
			// not — so the state and verdict are the missing file's as ever.
			name: "resolver not installed, and the Go binary's own probe went unmeasured",
			probes: Probes{ResolverInstalled: false, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: nxdomain, Default: defaultUnmeasured, PureGo: defaultUnmeasured},
			state:   DNSResolverMissing,
			verdict: VerdictUnknown,
			says: []string{"curl and browsers get nothing", "rules out the CGO_ENABLED=0 trap",
				"could not be measured (lookup voya-dns-check-probe.internal: context deadline exceeded)", "`sudo voya dns install` writes it", "client dialer"},
			notSays: []string{"fail exactly like Go does", "the other way round",
				"something other than that file is answering", "not the CGO_ENABLED=0 trap"},
		},
		{
			// The missing file with the system path answering an address
			// that is not voya's: the row said "the system does not resolve
			// *.internal, so curl and browsers get nothing" above an
			// evidence line printing that address — the same claim about an
			// unmeasured probe this issue removes, made about a measured
			// one. The file is still missing, so the state and verdict hold,
			// and the remedy is the one that does not offer curl names it
			// already has.
			name: "resolver not installed, and another resolver answers the system path",
			probes: Probes{ResolverInstalled: false, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: ProbeResult{Addrs: []string{"198.51.100.7"}}, Default: nxdomain, PureGo: nxdomain},
			state:   DNSResolverMissing,
			verdict: VerdictProblem,
			says: []string{"not loopback", "198.51.100.7)", "another resolver, VPN or search domain",
				"path voya sanctions", "`sudo voya dns install` writes it"},
			notSays: []string{"nothing on this machine resolves", "curl and browsers get nothing",
				"fail exactly like Go does", "To give curl and browsers the names",
				"something other than that file is answering"},
		},
		{
			// The plain row with the Go binary answering something that is
			// not voya: "nothing on this machine resolves" and "fail exactly
			// like Go does" were both said of a lookup that produced an
			// address, printed on the evidence line under them. The same
			// distinction classifyUnmeasuredSystem's rows draw; the missing
			// file keeps the state and verdict.
			name: "resolver not installed, and another resolver answers the Go binary",
			probes: Probes{ResolverInstalled: false, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: nxdomain, Default: ProbeResult{Addrs: []string{"198.51.100.7"}}, PureGo: nxdomain},
			state:   DNSResolverMissing,
			verdict: VerdictProblem,
			says: []string{"curl and browsers get nothing", "not loopback", "198.51.100.7)",
				"`sudo voya dns install` writes it"},
			notSays: []string{"fail exactly like Go does", "nothing on this machine resolves",
				"the other way round", "could not be measured"},
		},
		{
			// "yet something answers" with the Go side unmeasured (#260): the
			// split that row names is a divergence between two paths, and
			// this run measured one of them, so the split is what the row
			// says it cannot say — the same hedge the trap-shaped rows carry
			// (#258), on a row the missing file keeps at problem regardless.
			name: "resolver not installed, something answers, and the Go binary's own probe went unmeasured",
			probes: Probes{ResolverInstalled: false, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: resolved, Default: defaultUnmeasured, PureGo: defaultUnmeasured},
			state:   DNSResolverMissing,
			verdict: VerdictProblem,
			says: []string{"something other than that file is answering", "could not be measured (lookup voya-dns-check-probe.internal: context deadline exceeded)",
				"what this run cannot say", "client dialer"},
			notSays: []string{"while this Go binary's own resolver is not", "which is the split ADR 002 documents",
				"not the CGO_ENABLED=0 trap", "fail exactly like Go does"},
		},
		{
			name: "the trap",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: resolved, Default: nxdomain, PureGo: nxdomain},
			state:   DNSTrap,
			verdict: VerdictProblem,
			// The last phrase is the honesty the state needs: nothing here
			// can tell NXDOMAIN from a timeout — the results cross a process
			// boundary as strings — and only the first is the trap.
			says: []string{"resolver trap", "curl", "client dialer", "CGO_ENABLED=0 build",
				"cgo enabled is the other way out", "if it is a timeout", "re-run before acting on this"},
		},
		{
			// The trap's own row with the binary's half unmeasured: the
			// divergence is between two paths and only one of them was
			// measured, so the row that would assert it — and exit 1 for it —
			// says what it cannot say instead (#258). The state is the system
			// measurement's, the way every other unmeasured row classifies.
			name: "the trap's shape, with the Go binary's own probe unmeasured",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: resolved, Default: defaultUnmeasured, PureGo: defaultUnmeasured},
			state:   DNSWorking,
			verdict: VerdictUnknown,
			says: []string{"is installed", "could not be measured", "context deadline exceeded",
				"other half of the split ADR 002 documents", "client dialer", "once the probe can run"},
			notSays: []string{"resolver trap", "never consults " + resolverfile.Path,
				"if it is a timeout", "One residue of the trap remains"},
		},
		{
			// The divergence with the default path answering rather than
			// failing: nothing failed, so there is no failure to hand back
			// and no timeout to re-run for, and the file is plainly not being
			// ignored by a resolver that just produced an address.
			name: "the trap's shape, but the Go binary got an answer of its own",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: resolved, Default: ProbeResult{Addrs: []string{"198.51.100.7"}}, PureGo: nxdomain},
			state:   DNSTrap,
			verdict: VerdictProblem,
			says:    []string{"resolver trap", "not loopback", "198.51.100.7", "client dialer"},
			// Not a bare "never consults": the client dialer sentence carries
			// that phrase in every row, and banning it there would be a test
			// asserting something it does not mean.
			notSays: []string{"never consults " + resolverfile.Path, "if it is a timeout", "re-run before acting on this"},
		},
		{
			// Same divergence, measured from a binary that had cgo all along:
			// the state holds, but "build it with cgo enabled" is no longer
			// advice and must not be given.
			name: "the trap, measured without the CGO_ENABLED=0 build",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: false,
				System: resolved, Default: nxdomain, PureGo: nxdomain},
			state:   DNSTrap,
			verdict: VerdictProblem,
			says:    []string{"resolver trap", "broader than the CGO_ENABLED=0 case", "enabling cgo is not the way out"},
			notSays: []string{"is the other way out"},
		},
		{
			// The file is established present here, so the residue names it —
			// the one wording of the three that may, and the row that has to
			// pin it: without `ignores /etc/resolver/internal` asserted, this
			// row passes just as well on the file-free wording the
			// unestablished rows get (#142).
			name: "working, pure resolver still blind",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: resolved, Default: resolved, PureGo: nxdomain},
			state:   DNSWorking,
			verdict: VerdictOK,
			says: []string{"built with CGO_ENABLED=0", "-tags netgo", "GODEBUG=netdns=go",
				"ignores " + resolverfile.Path, "client dialer is immune"},
		},
		{
			// The same residue with the pure resolver answering rather than
			// failing: it did not reach the listener, so the sentence holds —
			// but "gets NXDOMAIN", which it used to say, would sit directly
			// above an evidence line reading `pure resolver=198.51.100.7`. A
			// timed-out probe is the same misreport, and reaches the report as
			// the same kind of string.
			name: "working, the pure resolver answering something that is not voya",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: resolved, Default: resolved, PureGo: ProbeResult{Addrs: []string{"198.51.100.7"}}},
			state:    DNSWorking,
			verdict:  VerdictOK,
			says:     []string{"-tags netgo", "did not resolve *.internal through voya"},
			notSays:  []string{"gets NXDOMAIN"},
			evidence: []string{"pure resolver=198.51.100.7"},
		},
		{
			// The residue rests on a measurement like every other sentence
			// here (#96): with nothing looked up, the pure resolver is not
			// blind, it is unreported — and the evidence line says so on its
			// own.
			name: "working, and the pure resolver was never measured",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: resolved, Default: resolved, PureGo: pureUnmeasured},
			state:    DNSWorking,
			verdict:  VerdictOK,
			says:     []string{"built with CGO_ENABLED=0"},
			notSays:  []string{"One residue of the trap", "-tags netgo", "GODEBUG=netdns=go"},
			evidence: []string{"pure resolver=not measured (the pure-resolver probe was not run)"},
		},
		{
			// The same silence with no reason behind it (#263): a caller
			// that never ran the pure-resolver probe, read as unmeasured at
			// the door rather than as a blind resolver.
			name: "working, and the pure resolver was never probed",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: resolved, Default: resolved, PureGo: ProbeResult{}},
			state:    DNSWorking,
			verdict:  VerdictOK,
			says:     []string{"built with CGO_ENABLED=0"},
			notSays:  []string{"One residue of the trap", "-tags netgo", "GODEBUG=netdns=go"},
			evidence: []string{"pure resolver=not measured (no probe was made)"},
		},
		{
			name: "working on every path",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: resolved, Default: resolved, PureGo: resolved},
			state:   DNSWorking,
			verdict: VerdictOK,
			says:    []string{"built with CGO_ENABLED=0"},
			notSays: []string{"netgo", "NXDOMAIN", "Re-measure"},
		},
		{
			// The false all-clear #6 exists to prevent: a cgo-enabled binary
			// resolving *.internal is not evidence that a CGO_ENABLED=0 one
			// would, and the report must not spend it as if it were.
			name: "resolving, but not from a CGO_ENABLED=0 build",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: false,
				System: resolved, Default: resolved, PureGo: resolved},
			state:   DNSWorking,
			verdict: VerdictUnknown,
			says:    []string{"not built with CGO_ENABLED=0", "may still get NXDOMAIN", "Re-measure"},
			notSays: []string{"reaches the system resolver without cgo"},
		},
		{
			name: "installed but nothing answering",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: nxdomain, Default: nxdomain, PureGo: nxdomain},
			state:   DNSBroken,
			verdict: VerdictProblem,
			says:    []string{"Nothing is answering", "Is voya running", "startup banner"},
			notSays: []string{"not loopback"},
		},
		{
			// Silence wears more than one shape: a result carrying neither
			// addresses nor an error nor a reason is a probe nobody made —
			// no probe here produces it, so it is a caller assembling Probes
			// itself — and it reads as unmeasured at the door (#263), the
			// way every row reads a reason. Before that it classified with
			// the measured failures, and calling its empty list "an address
			// that is not loopback ()" or "Nothing is answering" would make
			// the check the wrong-answer machine it exists to catch.
			name: "installed, and the system probe was never made",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: ProbeResult{}, Default: nxdomain, PureGo: nxdomain},
			state:   DNSBroken,
			verdict: VerdictUnknown,
			says:    []string{"could not be probed: no probe was made", "once the probe can run"},
			notSays: []string{"not loopback", "Nothing is answering"},
		},
		{
			// #263's own example: the system measured not resolving and
			// nothing else probed. "curl and browsers fail exactly like Go
			// does" is a claim about both paths, and the Go side was never
			// looked up, so the row says it cannot say — the same reading it
			// gives a reason (#260) — and the evidence line names the
			// silence rather than printing `go binary (…)=` and nothing.
			name: "resolver not installed, the system does not resolve, and the Go probes were never made",
			probes: Probes{ResolverInstalled: false, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: nxdomain},
			state:   DNSResolverMissing,
			verdict: VerdictUnknown,
			says: []string{"curl and browsers get nothing", "rules out the CGO_ENABLED=0 trap",
				"could not be measured (no probe was made)", "so this run cannot say"},
			notSays: []string{"fail exactly like Go does", "nothing on this machine resolves"},
			evidence: []string{"go binary (CGO_ENABLED=0)=not measured (no probe was made)",
				"pure resolver=not measured (no probe was made)"},
		},
		{
			// The trap's shape with the binary's half never probed (#263):
			// the same hedge as a reason (#258), never the trap.
			name: "the trap's shape, with the Go binary's own probe never made",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: resolved, Default: ProbeResult{}, PureGo: ProbeResult{}},
			state:   DNSWorking,
			verdict: VerdictUnknown,
			says:    []string{"is installed", "could not be measured (no probe was made)", "once the probe can run"},
			notSays: []string{"resolver trap", "if it is a timeout"},
		},
		{
			// Silence and a wrong answer are different faults with different
			// culprits: telling someone to check whether voya is running,
			// when something is plainly running and answering, wastes the
			// one report that was supposed to beat their other tools.
			name: "answered by something that is not voya",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: ProbeResult{Addrs: []string{"198.51.100.7"}}, Default: nxdomain, PureGo: nxdomain},
			state:   DNSBroken,
			verdict: VerdictProblem,
			says:    []string{"Something answered", "not loopback", "198.51.100.7", "Is voya running"},
			notSays: []string{"Nothing is answering"},
		},
		{
			// #185: the file is installed and names the port voya answers on,
			// resolved was just restarted, and zero packets reach voya —
			// because resolved's global scope routes every name past it. The
			// plain remedy's every clause is wrong here, so it is not given:
			// the cause is the catch-all, and only the catch-all is named.
			name: "installed but nothing answering, and resolved's global scope routes every name",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				GlobalCatchAll: true, System: nxdomain, Default: nxdomain, PureGo: nxdomain},
			state:   DNSBroken,
			verdict: VerdictProblem,
			says: []string{"The file is not the cause", "Domains=~.", "never reaches voya",
				"nothing answered for the name", "Drop the catch-all", "dnsmasq", "voya client",
				// The row names the routing as the cause without claiming
				// voya is healthy: nothing in Probes measured that.
				"Whether voya itself is up is a question this row did not measure"},
			notSays: []string{"restart systemd-resolved", "not live yet", "Is voya running", "startup banner",
				"Nothing is answering where the file points", "not loopback", "Neither the file nor voya"},
		},
		{
			// The catch-all with an upstream that answers: the wrong answer is
			// still named, with its address, but its cause is the catch-all
			// rather than an unnamed resolver "ahead of" the file.
			name: "answered by something that is not voya, through resolved's global catch-all",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				GlobalCatchAll: true, System: ProbeResult{Addrs: []string{"198.51.100.7"}}, Default: nxdomain, PureGo: nxdomain},
			state:   DNSBroken,
			verdict: VerdictProblem,
			says:    []string{"The file is not the cause", "Something did answer here", "not loopback", "198.51.100.7", "Drop the catch-all"},
			// Which server produced that address is not measured — resolved's
			// global servers are where the route leads, but /etc/hosts or an
			// nss module can answer ahead of it — so no culprit is named,
			// with the catch-all's certainty no more than with the old row's.
			notSays: []string{"restart systemd-resolved", "Is voya running", "another resolver, VPN or search domain",
				"nothing answered for the name", "That is what answered here"},
		},
		{
			// A catch-all nobody measured is not one that is absent (#104):
			// off Linux there is no scope to ask, and on Linux resolvectl may
			// not have answered. The plain remedy stands, and a true left in
			// GlobalCatchAll beside the reason is not read.
			name: "installed but nothing answering, and the catch-all went unmeasured",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				GlobalCatchAll: true, GlobalCatchAllUnmeasured: "resolvectl domain: exit status 1: bus wedged",
				System: nxdomain, Default: nxdomain, PureGo: nxdomain},
			state:   DNSBroken,
			verdict: VerdictProblem,
			// The reason is quoted rather than swallowed, the way every
			// other unmeasured thing in this file is: a host whose resolvectl
			// is wedged is exactly #185's host, and silence there hands it the
			// remedy that changes nothing with no sign a question went unput.
			says: []string{"Nothing is answering", "Is voya running", "restart systemd-resolved",
				"could not be read (resolvectl domain: exit status 1: bus wedged)", "resolvectl domain` shows it"},
			notSays: []string{"The file is not the cause", "Drop the catch-all"},
		},
		{
			// #206: the file is installed and read by a resolved too old to
			// be told a port, which rejected the one line naming voya and
			// configured nothing from it — so no restart moves anything,
			// and the command the plain remedy names is refused for exactly
			// this. Named ahead of the bypass and the catch-all, both
			// measured true here, as Preflight refuses on it ahead of both
			// and its way out needs neither of theirs.
			name: "installed but nothing answering, and the resolved that read it is too old to be told a port",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				ServiceTooOld: true, ServiceTooOldWhy: "systemd-resolved here is v245, and a port in DNS= needs v246 or newer",
				ServiceBypassed: true, ServiceBypassedWhy: "/etc/resolv.conf is not resolved's stub and names no 127.0.0.53",
				GlobalCatchAll: true, System: nxdomain, Default: nxdomain, PureGo: nxdomain},
			state:   DNSBroken,
			verdict: VerdictProblem,
			says: []string{"The file is not the cause, and neither is voya",
				"systemd-resolved here is v245, and a port in DNS= needs v246 or newer",
				"Failed to add DNS server address", "nothing answered for the name", "dnsmasq", "voya client",
				"restart the plain remedy prescribes moves none of that",
				// The one claim here that the row did not measure: what the
				// file says. A `DNS=127.0.0.1` this resolved does take is
				// read fine, and then the version stopped nothing.
				"establishes that the file is there, not what it says", "`DNS=127.0.0.1` alone"},
			notSays: []string{"Is voya running?", "restart systemd-resolved", "does not go through systemd-resolved",
				"Domains=~.", "voya dns install"},
		},
		{
			// The same host, with something answering: the address is
			// reported, its author not named, and the cause still the
			// version.
			name: "installed but answering from elsewhere, and the resolved that read it is too old to be told a port",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				ServiceTooOld: true, ServiceTooOldWhy: "systemd-resolved here is v245, and a port in DNS= needs v246 or newer",
				System: ProbeResult{Addrs: []string{"198.51.100.7"}}, Default: nxdomain, PureGo: nxdomain},
			state:   DNSBroken,
			verdict: VerdictProblem,
			says:    []string{"v245", "Something did answer here", "198.51.100.7", "which is where the lookup went instead"},
			notSays: []string{"Is voya running?", "nothing answered for the name"},
		},
		{
			// And a version nobody could read is not too old (#104): the
			// plain row, restart and all.
			name: "installed but nothing answering, and the resolved version went unmeasured",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				ServiceTooOld: true, ServiceTooOldUnmeasured: "resolvectl --version: exit status 1: bus wedged",
				System: nxdomain, Default: nxdomain, PureGo: nxdomain},
			state:   DNSBroken,
			verdict: VerdictProblem,
			says: []string{"Is voya running?", "restart systemd-resolved",
				// Not a measured no either (#104): the remedy stands, and
				// says which question it could not put behind it.
				"could not be read (resolvectl --version: exit status 1: bus wedged)",
				"resolvectl --version` shows it", "the restart above moves none of that"},
			notSays: []string{"v245", "Failed to add DNS server address", "too old"},
		},
		{
			// #204: the file is installed, resolved read it, and this host's
			// own resolution never asks resolved — a static /etc/resolv.conf
			// — so no restart moves anything, and `sudo voya dns install
			// --activate`, the plain remedy's command, is one Preflight
			// refuses on exactly this host. The cause is named, with what
			// resolverfile found in the files, and the refused command is
			// not.
			name: "installed but nothing answering, and the host does not resolve through resolved",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				ServiceBypassed: true, ServiceBypassedWhy: "/etc/resolv.conf is not resolved's stub and names no 127.0.0.53, " +
					"and /etc/nsswitch.conf does not list `resolve` for hosts",
				System: nxdomain, Default: nxdomain, PureGo: nxdomain},
			state:   DNSBroken,
			verdict: VerdictProblem,
			says: []string{"The file is not the cause", "does not go through systemd-resolved at all",
				"/etc/resolv.conf is not resolved's stub", "does not list `resolve` for hosts", "nothing ever asks resolved",
				"nothing answered for the name", "stub-resolv.conf", "dns=systemd-resolved", "dnsmasq", "voya client",
				// The one host two files cannot tell from this one: a
				// forwarder that chains onto resolved. There the restart
				// is the fix, so it is named as that host's condition —
				// which is why "restart systemd-resolved" is absent from
				// notSays below while the plain remedy's own words ("not
				// live yet", the --activate command) stay banned.
				"chains onto resolved", "makes the drop-in it read live", "sudo " + resolverReload,
				"Whether voya itself is up is a question this row did not measure"},
			notSays: []string{"voya dns install --activate", "not live yet", "Is voya running",
				"startup banner", "Nothing is answering where the file points", "not loopback", "Domains=~."},
		},
		{
			// Bypassed and catch-all both measured: the bypass is the more
			// basic cause — a service nothing asks routes nothing, catch-all
			// or not — and the one Preflight refuses on first, so it is the
			// one named.
			name: "installed but nothing answering, resolved bypassed and its global scope routing every name",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				ServiceBypassed: true, ServiceBypassedWhy: "/etc/resolv.conf is missing, and /etc/nsswitch.conf is missing",
				GlobalCatchAll: true, System: nxdomain, Default: nxdomain, PureGo: nxdomain},
			state:   DNSBroken,
			verdict: VerdictProblem,
			says:    []string{"does not go through systemd-resolved at all", "/etc/resolv.conf is missing"},
			notSays: []string{"Domains=~.", "Drop the catch-all", "voya dns install --activate"},
		},
		{
			// The wrong answer through a bypassed resolved: named with its
			// address, and its author is whatever the host does resolve with.
			name: "answered by something that is not voya, on a host that does not resolve through resolved",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				ServiceBypassed: true, ServiceBypassedWhy: "/etc/resolv.conf is not resolved's stub and names no 127.0.0.53",
				System: ProbeResult{Addrs: []string{"198.51.100.7"}}, Default: nxdomain, PureGo: nxdomain},
			state:   DNSBroken,
			verdict: VerdictProblem,
			says: []string{"The file is not the cause", "Something did answer here", "not loopback", "198.51.100.7",
				// Where the lookup went, not who answered it: /etc/hosts or
				// an nss module can answer ahead of whatever this host
				// resolves through, and none of that was measured — the same
				// line catchAllCause's row draws.
				"where the lookup went instead", "stub-resolv.conf"},
			notSays: []string{"voya dns install --activate", "Is voya running", "another resolver, VPN or search domain",
				"that resolver's answer", "nothing answered for the name"},
		},
		{
			// Unmeasured is not bypassed (#104): off Linux there is no
			// service to reach, and on Linux a file may have been unreadable.
			// The plain remedy stands — and it already ends by naming this
			// cause as a possibility — and a true left in ServiceBypassed
			// beside the reason is not read.
			name: "installed but nothing answering, and whether the host resolves through resolved went unmeasured",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				ServiceBypassed: true, ServiceBypassedUnmeasured: "reading /etc/nsswitch.conf: permission denied",
				System: nxdomain, Default: nxdomain, PureGo: nxdomain},
			state:   DNSBroken,
			verdict: VerdictProblem,
			says: []string{"Nothing is answering", "Is voya running", "sudo voya dns install --activate", "restart systemd-resolved",
				"may not go through that service at all"},
			notSays: []string{"The file is not the cause", "does not go through systemd-resolved at all"},
		},
		{
			// #96's rows begin here: a system path that produced no
			// measurement is not a measured "no". On the OS that reads the
			// file, its absence is still a conclusion the stat carries on its
			// own — the probe's silence changes what may be claimed about
			// curl, not the state or the verdict.
			name: "the probe could not run, and the file is not installed",
			probes: Probes{ResolverInstalled: false, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: toolBroke, Default: nxdomain, PureGo: nxdomain},
			state:   DNSResolverMissing,
			verdict: VerdictUnknown,
			says:    []string{resolverfile.Path, "could not be probed", "directory service on fire", "`sudo voya dns install` writes it", "client dialer"},
			notSays: []string{"not the CGO_ENABLED=0 trap", "fail exactly like Go does", "Is voya running", "will refuse here"},
		},
		{
			// The system path is what went unmeasured, and neither question
			// behind the predicted refusal is on it: both are asked of the
			// host wherever the file is read (#185, #204), so this row owes
			// the refusal exactly as the measured one does.
			name: "the probe could not run, the file is not installed, and the host does not resolve through resolved",
			probes: Probes{ResolverInstalled: false, OSReadsResolverFile: true, DefaultCGODisabled: true,
				ServiceBypassed: true, ServiceBypassedWhy: "/etc/resolv.conf is not resolved's stub and names no 127.0.0.53",
				System: toolBroke, Default: nxdomain, PureGo: nxdomain},
			state:   DNSResolverMissing,
			verdict: VerdictUnknown,
			says: []string{"could not be probed", "`sudo voya dns install` writes it", "will refuse here",
				"does not go through systemd-resolved", "stub-resolv.conf",
				"With the system pointed at resolved, `sudo voya dns install` writes the file"},
			notSays: []string{"Is voya running", "The file is not the cause"},
		},
		{
			// And the version first (#206), here as on the measured row.
			name: "the probe could not run, the file is not installed, and the resolved is too old to be told a port",
			probes: Probes{ResolverInstalled: false, OSReadsResolverFile: true, DefaultCGODisabled: true,
				ServiceTooOld: true, ServiceTooOldWhy: "systemd-resolved here is v245, and a port in DNS= needs v246 or newer",
				ServiceBypassed: true, ServiceBypassedWhy: "/etc/resolv.conf is not resolved's stub and names no 127.0.0.53",
				System: toolBroke, Default: nxdomain, PureGo: nxdomain},
			state:   DNSResolverMissing,
			verdict: VerdictUnknown,
			says: []string{"could not be probed", "`sudo voya dns install` writes it", "will refuse here",
				"systemd-resolved here is v245, and a port in DNS= needs v246 or newer"},
			notSays: []string{"does not go through systemd-resolved", "writes the file"},
		},
		{
			// The file is installed and a CGO_ENABLED=0 binary resolves, but
			// whether curl and browsers do is exactly what the broken probe
			// measures: the state is what was seen, and the verdict says the
			// evidence cannot answer for the system path.
			name: "the probe could not run, but a CGO_ENABLED=0 binary resolves",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: toolBroke, Default: resolved, PureGo: resolved},
			state:   DNSWorking,
			verdict: VerdictUnknown,
			says:    []string{"could not be probed", "directory service on fire", "not biting this toolchain", "re-run `voya dns check`"},
			notSays: []string{"reaches the system resolver without cgo", "Is voya running", "One residue of the trap"},
		},
		{
			// The same row with the pure resolver blind: `working` from a
			// row outside the main switch owes the same residue the main
			// switch's working row carries (#134, #142), and this row has
			// the file established present, so the residue may name the
			// file the pure resolver ignores.
			name: "the probe could not run, the binary resolves, and the pure resolver is still blind",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: toolBroke, Default: resolved, PureGo: nxdomain},
			state:   DNSWorking,
			verdict: VerdictUnknown,
			says: []string{"could not be probed", "One residue of the trap", "-tags netgo", "GODEBUG=netdns=go",
				"Go's pure resolver", "ignores " + resolverfile.Path, "client dialer is immune"},
			notSays: []string{"reaches the system resolver without cgo", "Is voya running"},
		},
		{
			// The unmeasured system path owes the same distinction the
			// measured rows draw: an answer that is not loopback is not
			// "nothing resolved", and saying so would contradict the address
			// on the evidence line and send the developer after the wrong
			// two faults.
			name: "the probe could not run, and another resolver claims *.internal",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: toolBroke, Default: ProbeResult{Addrs: []string{"198.51.100.7"}}, PureGo: nxdomain},
			state:   DNSBroken,
			verdict: VerdictUnknown,
			says:    []string{"not loopback", "198.51.100.7", "could not be probed", "client dialer"},
			notSays: []string{"own resolver did not resolve *.internal", "is voya running", "trap ADR 002 documents"},
		},
		{
			// Same claim, with the file missing: the missing file is still
			// the conclusion, and the answer nobody can account for is still
			// worth naming rather than passing over in silence.
			name: "the probe could not run, the file is missing, and something answers anyway",
			probes: Probes{ResolverInstalled: false, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: toolBroke, Default: ProbeResult{Addrs: []string{"198.51.100.7"}}, PureGo: nxdomain},
			state:   DNSResolverMissing,
			verdict: VerdictProblem,
			says:    []string{resolverfile.Path, "not loopback", "198.51.100.7", "`sudo voya dns install` writes it"},
			notSays: []string{"resolved *.internal even so", "not the CGO_ENABLED=0 trap", "Is voya running"},
		},
		{
			// The row #96 quotes: with the probe unable to run, "nothing is
			// answering where the file points" is a claim nothing measured,
			// and the two faults it would tell apart — voya down, or the
			// trap — both stay on the table, named as such.
			name: "the probe could not run, and the binary is blind",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: toolBroke, Default: nxdomain, PureGo: nxdomain},
			state:   DNSBroken,
			verdict: VerdictUnknown,
			says:    []string{"could not be probed", "voya running", "client dialer", "trap ADR 002 documents"},
			notSays: []string{"Nothing is answering"},
		},
		{
			// The row above with the file-port dial answering (#243): "is
			// voya running?" is one of the two faults it sends the developer
			// to tell apart, and this run asked that port and got voya —
			// printed as `file port=127.0.0.1` on the evidence line under it.
			name: "the probe could not run, the binary is blind, and voya answers the file's port",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: toolBroke, Default: nxdomain, PureGo: nxdomain,
				AtFilePort: ProbeResult{Addrs: []string{"127.0.0.1"}}},
			state:   DNSBroken,
			verdict: VerdictUnknown,
			says: []string{"could not be probed", "voya itself is up, at least",
				"trap ADR 002 documents", "client dialer"},
			notSays: []string{"Is voya running", "is voya running", "nothing answering where the file points"},
		},
		{
			// And with that dial finding nothing that is voya: the same fault,
			// measured present rather than left as one of two.
			name: "the probe could not run, the binary is blind, and nothing answers the file's port",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: toolBroke, Default: nxdomain, PureGo: nxdomain,
				AtFilePort: ProbeResult{Err: "read udp 127.0.0.1:53535: connection refused"}},
			state:   DNSBroken,
			verdict: VerdictUnknown,
			says: []string{"could not be probed", "Nothing that is voya is answering on the port the file names",
				"connection refused", "Start voya", "trap ADR 002 documents", "client dialer"},
			notSays: []string{"is voya running", "one of two faults"},
		},
		{
			// Both probes unmeasured (#260): the row above opens on a "no"
			// from this binary's own resolver, and here nothing was looked
			// up, so the row says neither path was measured and keeps every
			// fault on the table — including none.
			name: "the probe could not run, and the Go binary's own probe went unmeasured",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: toolBroke, Default: defaultUnmeasured, PureGo: defaultUnmeasured},
			state:   DNSBroken,
			verdict: VerdictUnknown,
			says: []string{"neither path was measured", "could not be probed", "directory service on fire",
				"could not be measured either (lookup voya-dns-check-probe.internal: context deadline exceeded)",
				"trap ADR 002 documents", "client dialer"},
			notSays: []string{"own resolver did not resolve *.internal", "Nothing is answering", "resolver trap"},
		},
		{
			// The same row with the file-port dial answering (#243): "nothing
			// answering where the file points" is one of the faults the row
			// calls indistinguishable, and this run dialled that port and got
			// voya — printed as `file port=127.0.0.1` on the evidence line
			// under it.
			name: "the probe could not run, the Go binary's own probe too, and voya answers the file's port",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: toolBroke, Default: defaultUnmeasured, PureGo: defaultUnmeasured,
				AtFilePort: ProbeResult{Addrs: []string{"127.0.0.1"}}},
			state:   DNSBroken,
			verdict: VerdictUnknown,
			says: []string{"neither path was measured", "voya itself is up, at least",
				"trap ADR 002 documents from no fault at all", "client dialer"},
			notSays: []string{"nothing answering where the file points", "tell the faults apart",
				"own resolver did not resolve *.internal"},
		},
		{
			// And with the dial finding nothing that is voya: that fault is
			// not indistinguishable either — it was measured present, and a
			// row that says nothing could be told apart says less than its
			// own evidence line.
			name: "the probe could not run, the Go binary's own probe too, and nothing answers the file's port",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: toolBroke, Default: defaultUnmeasured, PureGo: defaultUnmeasured,
				AtFilePort: ProbeResult{Err: "read udp 127.0.0.1:53535: connection refused"}},
			state:   DNSBroken,
			verdict: VerdictUnknown,
			says: []string{"neither path was measured", "One fault was measured even so",
				"nothing that is voya is answering on the port the file names", "Start voya",
				"whether the trap ADR 002 documents is here too", "client dialer"},
			notSays: []string{"tell the faults apart", "voya itself is up",
				"own resolver did not resolve *.internal"},
		},
		{
			// The ordinary host where nothing reads the file: nothing is
			// wrong, nothing was measured on the system path, and the report
			// must say "not checked here" rather than prescribe a file this
			// host never reads (#96). Two hosts land here since #155 — an OS
			// with no wiring at all, and a Linux not running systemd-resolved
			// — and the rows below carry both. The file's absence is
			// established here and still not the state (#134): the state
			// word is a claim about a file nothing here reads, so the
			// row is the one probe that measured — the same reading the
			// unconsulted-file row below gets from the same probes.
			name: "off the OS that reads the file, an ordinary host",
			probes: Probes{ResolverInstalled: false, OSReadsResolverFile: false, DefaultCGODisabled: true,
				System: offOS, Default: nxdomain, PureGo: nxdomain},
			state:   DNSBroken,
			verdict: VerdictUnknown,
			says:    []string{"not checked here", "voya dns install", "expected state here", "client dialer"},
			notSays: []string{resolverfile.Path, "Is voya running", "not the CGO_ENABLED=0 trap", "To give curl and browsers the names"},
		},
		{
			// The ordinary host with the Go side unmeasured (#260): on Windows
			// or FreeBSD the Go probes are the whole report, and "does not
			// resolve, which is the expected state" was said of a lookup
			// nobody completed. The state stays the one probe that can
			// measure here; the sentence says it did not.
			name: "off the OS that reads the file, the Go binary's own probe unmeasured",
			probes: Probes{ResolverInstalled: false, OSReadsResolverFile: false, DefaultCGODisabled: true,
				System: offOS, Default: defaultUnmeasured, PureGo: defaultUnmeasured},
			state:   DNSBroken,
			verdict: VerdictUnknown,
			says: []string{"not checked here", "could not be measured (lookup voya-dns-check-probe.internal: context deadline exceeded)",
				"did not see it either way", "client dialer"},
			notSays: []string{"does not resolve *.internal, which is the expected state here",
				resolverfile.Path, "Is voya running", "worth finding what is answering"},
		},
		{
			// Same host, the file established absent, and the binary
			// resolving: the other state the probes can give an absent file
			// off the OS, so the branch cannot be flipped to one of them and
			// still pass.
			name: "off the OS that reads the file, the file absent and the binary resolving",
			probes: Probes{ResolverInstalled: false, OSReadsResolverFile: false, DefaultCGODisabled: true,
				System: offOS, Default: resolved, PureGo: resolved},
			state:   DNSWorking,
			verdict: VerdictUnknown,
			says:    []string{"not checked here", "worth finding what is answering"},
			notSays: []string{resolverfile.Path, "Is voya running", "One residue of the trap"},
		},
		{
			// The stray file #96 opens with — a bind mount, a copied dotfile
			// — beside a binary that resolves: the old reading was DNSBroken,
			// "Is voya running?", on a host where voya was answering.
			name: "off the OS, a stray resolver file, the binary resolving",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: false, DefaultCGODisabled: true,
				System: offOS, Default: resolved, PureGo: resolved},
			state:   DNSWorking,
			verdict: VerdictUnknown,
			says:    []string{"not checked here", "proves nothing either way", "worth finding what is answering"},
			notSays: []string{"reaches the system resolver without cgo", "Is voya running", "Re-measure", "One residue of the trap"},
		},
		{
			// The binary resolving off the OS while the pure resolver is
			// blind: the residue the reading OS's `working, pure resolver
			// still blind` row carries, said without the file (#134) — a
			// netgo build that is blind here gets a hint of why, and #96's
			// ban on naming the file holds. The banned clause is that row's
			// last one: off the OS the client-dialer sentence follows the
			// residue, so carrying it here says the same thing twice running.
			name: "off the OS, the binary resolving and the pure resolver still blind",
			probes: Probes{ResolverUnmeasured: notConsulted, OSReadsResolverFile: false, DefaultCGODisabled: true,
				System: offOS, Default: resolved, PureGo: nxdomain},
			state:   DNSWorking,
			verdict: VerdictUnknown,
			says: []string{"not checked here", "One residue of the trap", "-tags netgo", "GODEBUG=netdns=go",
				"Go's pure resolver", "did not resolve *.internal through voya here"},
			notSays: []string{"is present", "is not installed", "point " + resolverfile.Path, "ignores " + resolverfile.Path,
				"client dialer is immune"},
		},
		{
			name: "off the OS, a stray resolver file, nothing resolving",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: false, DefaultCGODisabled: true,
				System: offOS, Default: nxdomain, PureGo: nxdomain},
			state:   DNSBroken,
			verdict: VerdictUnknown,
			says:    []string{"not checked here", "proves nothing either way", "expected state here", "client dialer"},
			notSays: []string{"voya running"},
		},
		{
			// The host #155 adds to this bucket, and the one that makes it
			// worth having on a supported OS: Linux without systemd-resolved.
			// The drop-in `voya dns install` writes is there — installed
			// before resolved was stopped or removed, or copied in — and
			// nothing reads it, so the same two bans hold as on Windows: no
			// state word for the file, no remedy naming it. What is different
			// is that the system path *was* measured, because getent goes
			// through NSS whatever routes it, and the report says what NSS
			// answered rather than that it did not look.
			name: "a Linux without systemd-resolved: the drop-in is present and nothing reads it",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: false, DefaultCGODisabled: true,
				System:  ProbeResult{Err: "getent ahosts voya-dns-check-probe.internal: not found by any name service"},
				Default: nxdomain, PureGo: nxdomain},
			state:   DNSBroken,
			verdict: VerdictUnknown,
			says: []string{"nothing on this machine reads a *.internal wiring file", "proves nothing either way",
				"did not resolve *.internal through voya", "client dialer"},
			notSays: []string{"not checked here", "is not installed", "point " + resolverfile.Path,
				"Is voya running"},
		},
		{
			// The same host with the drop-in absent — the default state of
			// every Linux that never ran `voya dns install` — measured blind.
			// The absence gets no sentence at all (#134, #142): it is the
			// universal default here, and a remedy naming the file would send
			// a developer to write one systemd-resolved is not running to read.
			name: "a Linux without systemd-resolved: no drop-in, and NSS resolves nothing",
			probes: Probes{ResolverInstalled: false, OSReadsResolverFile: false, DefaultCGODisabled: true,
				System:  ProbeResult{Err: "getent ahosts voya-dns-check-probe.internal: not found by any name service"},
				Default: nxdomain, PureGo: nxdomain},
			state:   DNSBroken,
			verdict: VerdictUnknown,
			says:    []string{"nothing on this machine reads a *.internal wiring file", "did not resolve *.internal through voya"},
			notSays: []string{"is present", "is not installed", "point " + resolverfile.Path, "Is voya running"},
		},
		{
			// The non-loopback answer is worth naming off the OS too: another
			// resolver claiming *.internal is a fault whatever OS it is on.
			name: "off the OS, another resolver claims *.internal",
			probes: Probes{ResolverInstalled: true, OSReadsResolverFile: false, DefaultCGODisabled: true,
				System: offOS, Default: ProbeResult{Addrs: []string{"198.51.100.7"}}, PureGo: nxdomain},
			state:   DNSBroken,
			verdict: VerdictUnknown,
			says:    []string{"not loopback", "198.51.100.7", "not checked here"},
			notSays: []string{"expected state here", "Is voya running"},
		},
		{
			// #104's rows begin here: a stat on the file that failed — EACCES
			// on a parent directory, a wedged mount — established neither
			// presence nor absence, so DNSResolverMissing is off the table by
			// construction, and the verdict is unknown wherever the file's
			// state is what would have made the evidence conclusive. Here
			// everything resolves through the listener, which with the file
			// installed would be the all-clear — and with it absent would be
			// "something other than that file is answering", a problem.
			name: "the file could not be read, and everything resolves",
			probes: Probes{ResolverUnmeasured: statFailed, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: resolved, Default: resolved, PureGo: resolved},
			state:   DNSWorking,
			verdict: VerdictUnknown,
			says:    []string{"could not be read", "permission denied", "CGO_ENABLED=0 build", "re-run `voya dns check`"},
			notSays: []string{"is not installed", "reaches the system resolver without cgo", "One residue of the trap"},
		},
		{
			// The same row with the pure resolver blind: the residue is
			// owed here too (#134, #142), but the file's state was never
			// established (#104), so the wording names no file — "ignores
			// /etc/resolver/internal" is a sentence about the file, and this
			// row may not say one. The dialer clause stays: nothing else in
			// this row reassures about the client dialer.
			name: "the file could not be read, everything resolves, and the pure resolver is still blind",
			probes: Probes{ResolverUnmeasured: statFailed, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: resolved, Default: resolved, PureGo: nxdomain},
			state:   DNSWorking,
			verdict: VerdictUnknown,
			says: []string{"could not be read", "One residue of the trap", "-tags netgo", "GODEBUG=netdns=go",
				"Go's pure resolver", "did not resolve *.internal through voya here", "client dialer is immune"},
			notSays: []string{"is not installed", "ignores " + resolverfile.Path, "reaches the system resolver without cgo"},
		},
		{
			// The split was measured, and it is a problem whichever state the
			// unread file is in — the trap if installed and ignored, an
			// unaccounted-for resolver if absent — so the verdict holds while
			// the detail refuses to pick between the two causes.
			name: "the file could not be read, and the split is measured anyway",
			probes: Probes{ResolverUnmeasured: statFailed, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: resolved, Default: nxdomain, PureGo: nxdomain},
			state:   DNSTrap,
			verdict: VerdictProblem,
			says:    []string{"could not be read", "permission denied", "split ADR 002 documents", "client dialer"},
			notSays: []string{"is not installed", "never consults " + resolverfile.Path},
		},
		{
			// The same half-measured divergence with the file unread too: two
			// of the three things this row would rest on were not measured,
			// so it names neither the trap nor what routes the system path
			// (#258).
			name: "the file could not be read, and the Go binary's own probe went unmeasured",
			probes: Probes{ResolverUnmeasured: statFailed, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: resolved, Default: defaultUnmeasured, PureGo: defaultUnmeasured},
			state:   DNSWorking,
			verdict: VerdictUnknown,
			says: []string{"could not be read", "permission denied", "what the unread file would have said",
				"could not be measured", "context deadline exceeded", "client dialer"},
			notSays: []string{"resolver trap", "split ADR 002 documents was measured", "is not installed"},
		},
		{
			// Nothing resolves through voya on any measured path: conclusively
			// a problem whether the file is installed (nothing answers where
			// it points) or absent (nothing routes curl at all), and the
			// detail names both faults instead of claiming either.
			name: "the file could not be read, and nothing resolves",
			probes: Probes{ResolverUnmeasured: statFailed, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: nxdomain, Default: nxdomain, PureGo: nxdomain},
			state:   DNSBroken,
			verdict: VerdictProblem,
			says:    []string{"could not be read", "permission denied", "is voya running", "would have told those apart"},
			notSays: []string{"is not installed", "not loopback"},
		},
		{
			// The unmeasured file owes the measured rows' distinction too: an
			// answer that is not loopback is not "nothing resolved".
			name: "the file could not be read, and something else answers the system",
			probes: Probes{ResolverUnmeasured: statFailed, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: ProbeResult{Addrs: []string{"198.51.100.7"}}, Default: nxdomain, PureGo: nxdomain},
			state:   DNSBroken,
			verdict: VerdictProblem,
			says:    []string{"could not be read", "not loopback", "198.51.100.7"},
			notSays: []string{"is not installed", "did not resolve *.internal through voya"},
		},
		{
			// The file and the system path both unread: this binary's own
			// resolver is the run's one measurement, and it is what the report
			// carries — the whole point of not bailing on the stat (#104).
			name: "neither the file nor the system path could be checked, and the binary resolves",
			probes: Probes{ResolverUnmeasured: statFailed, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: toolBroke, Default: resolved, PureGo: resolved},
			state:   DNSWorking,
			verdict: VerdictUnknown,
			says:    []string{"could not be read", "could not be probed", "directory service on fire", "one measurement this run has"},
			notSays: []string{"is not installed", "reaches the system resolver without cgo", "One residue of the trap"},
		},
		{
			// The same row with the pure resolver blind: the third working
			// row outside the main switch, and the second whose file state is
			// unestablished, so the residue is the file-free wording (#142).
			name: "neither the file nor the system path could be checked, the binary resolves, and the pure resolver is still blind",
			probes: Probes{ResolverUnmeasured: statFailed, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: toolBroke, Default: resolved, PureGo: nxdomain},
			state:   DNSWorking,
			verdict: VerdictUnknown,
			says: []string{"could not be read", "could not be probed", "One residue of the trap", "-tags netgo",
				"GODEBUG=netdns=go", "Go's pure resolver", "did not resolve *.internal through voya here", "client dialer is immune"},
			notSays: []string{"is not installed", "ignores " + resolverfile.Path, "reaches the system resolver without cgo"},
		},
		{
			name: "neither the file nor the system path could be checked, and the binary is blind",
			probes: Probes{ResolverUnmeasured: statFailed, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: toolBroke, Default: nxdomain, PureGo: nxdomain},
			state:   DNSBroken,
			verdict: VerdictUnknown,
			says:    []string{"could not be read", "could not be probed", "voya not running", "trap ADR 002 documents", "client dialer"},
			notSays: []string{"is not installed", "Nothing is answering"},
		},
		{
			// And the binary's probe unmeasured too (#260): the file, the
			// system path and the one probe the row above calls "the one
			// measurement this run has" all silent, so the row claims no
			// measurement at all rather than a "no" from the binary.
			name: "neither the file, the system path nor the Go binary's own probe could be measured",
			probes: Probes{ResolverUnmeasured: statFailed, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: toolBroke, Default: defaultUnmeasured, PureGo: defaultUnmeasured},
			state:   DNSBroken,
			verdict: VerdictUnknown,
			says: []string{"could not be read", "could not be probed", "no measurement at all",
				"could not be measured either (lookup voya-dns-check-probe.internal: context deadline exceeded)",
				"trap ADR 002 documents", "client dialer"},
			notSays: []string{"did not resolve *.internal", "one measurement this run has", "is not installed"},
		},
		{
			// The row above with the pure resolver measured after all: every
			// probe carries its own budget (#253), so that one can answer
			// where this binary's own timed out, and "this run has no
			// measurement at all" would be said directly above the answer on
			// the evidence line. What the row has none of is a measurement it
			// rests on.
			name: "neither the file, the system path nor the Go binary's own probe, but the pure resolver measured",
			probes: Probes{ResolverUnmeasured: statFailed, OSReadsResolverFile: true, DefaultCGODisabled: true,
				System: toolBroke, Default: defaultUnmeasured, PureGo: nxdomain},
			state:   DNSBroken,
			verdict: VerdictUnknown,
			says: []string{"could not be read", "could not be probed", "rests on no measurement at all",
				"could not be measured either (lookup voya-dns-check-probe.internal: context deadline exceeded)",
				"trap ADR 002 documents", "client dialer"},
			notSays: []string{"this run has no measurement at all", "did not resolve *.internal",
				"one measurement this run has"},
		},
		{
			// Off the OS that reads the file even its readable states prove
			// nothing, so an unreadable one changes nothing — except that "is
			// present" may not be said, and DNSResolverMissing may not be
			// claimed, of a file whose state was never established.
			name: "off the OS, the file could not be read",
			probes: Probes{ResolverUnmeasured: statFailed, OSReadsResolverFile: false, DefaultCGODisabled: true,
				System: offOS, Default: nxdomain, PureGo: nxdomain},
			state:   DNSBroken,
			verdict: VerdictUnknown,
			says:    []string{"not checked here", "state was not established", "changes nothing"},
			notSays: []string{"is present", "is not installed", "Is voya running"},
		},
		{
			// The row `voya dns check` itself takes off darwin: the stat is gated
			// on the reading OS, so the file is not unreadable — it was never
			// looked at, which leaves its state just as unestablished. Reading
			// that silence as absence would print `resolver-not-installed` on
			// every non-darwin host, file or no file: a state word no probe
			// measured (ADR 005) and a remedy naming a file this OS never
			// reads (#96).
			name: "off the OS, the file was never consulted",
			probes: Probes{ResolverUnmeasured: notConsulted, OSReadsResolverFile: false, DefaultCGODisabled: true,
				System: offOS, Default: nxdomain, PureGo: nxdomain},
			state:   DNSBroken,
			verdict: VerdictUnknown,
			says:    []string{"not checked here", "state was not established", "no stat was made", "changes nothing"},
			notSays: []string{"is present", "is not installed", "point " + resolverfile.Path},
		},
		{
			// Same, with the file present behind the stat nobody made — which
			// the report cannot know, and so must not describe either way. The
			// distinguishing pin: before the gate carried a reason, this host
			// and the one above were reported identically, both as the file's
			// absence.
			name: "off the OS, the file was never consulted while the binary resolves",
			probes: Probes{ResolverUnmeasured: notConsulted, OSReadsResolverFile: false, DefaultCGODisabled: true,
				System: offOS, Default: resolved, PureGo: resolved},
			state:   DNSWorking,
			verdict: VerdictUnknown,
			says:    []string{"not checked here", "state was not established", "has not specified for this machine"},
			notSays: []string{"is present", "is not installed", "point " + resolverfile.Path},
		},
		{
			// Off the reading OS with the system path measured after all — the
			// combination a caller reaches by substituting one of the two
			// darwin-keyed inputs, and `voya dns check`'s own test seam does. It
			// used to fall through to the main switch, which claims the file's
			// state in every branch and prescribes the file in this one. No
			// sentence here may do either, and none may say the system path
			// went unchecked when it did not — and the state is the system
			// measurement's, not the Go binary's failure beside it (#122): the
			// split was measured, and `not-answering` above an evidence line
			// reading `system=127.0.0.1` reported the one path that answered
			// as if it had not.
			name: "off the OS, the file unestablished and the system path measured",
			probes: Probes{ResolverUnmeasured: notConsulted, OSReadsResolverFile: false, DefaultCGODisabled: true,
				System: resolved, Default: nxdomain, PureGo: nxdomain},
			state:   DNSTrap,
			verdict: VerdictUnknown,
			says:    []string{"nothing on this machine reads a *.internal wiring file", "state was not established", "127.0.0.1", "split ADR 002 documents", "client dialer"},
			notSays: []string{"not checked here", "is present", "is not installed", "point " + resolverfile.Path, "did not resolve *.internal through voya"},
		},
		{
			// #122's second half: off the reading OS with the file's state
			// established and the system path measured, nothing routed to
			// the off-OS report — the main switch took the row, named the
			// file's absence and prescribed the file #96 forbids naming there.
			// The file's absence is not the state either: on this OS the file
			// is inert, so what was measured is what classifies — which the
			// `state` column below carries on its own. Banning the word
			// "resolver-not-installed" in notSays would not: notSays is matched
			// against Detail, and no detail string anywhere here contains a
			// state word, so the ban would hold whatever the code did.
			name: "off the OS, the file established absent and the system path measured",
			probes: Probes{ResolverInstalled: false, OSReadsResolverFile: false, DefaultCGODisabled: true,
				System: resolved, Default: nxdomain, PureGo: nxdomain},
			state:   DNSTrap,
			verdict: VerdictUnknown,
			says:    []string{"nothing on this machine reads a *.internal wiring file", "127.0.0.1", "split ADR 002 documents"},
			notSays: []string{"not checked here", "is not installed", "point " + resolverfile.Path},
		},
		{
			// And off that OS, where the trap-shaped row is the third of the
			// three: the verdict was already unknown here, so what the
			// unmeasured probe changes is the state and the sentence (#258).
			name: "off the OS, the system path measured and the Go binary's own probe not",
			probes: Probes{ResolverInstalled: false, OSReadsResolverFile: false, DefaultCGODisabled: true,
				System: resolved, Default: defaultUnmeasured, PureGo: defaultUnmeasured},
			state:   DNSWorking,
			verdict: VerdictUnknown,
			says: []string{"nothing on this machine reads a *.internal wiring file", "127.0.0.1",
				"could not be measured", "context deadline exceeded", "client dialer"},
			notSays: []string{"split ADR 002 documents, arriving by wiring", "is not installed",
				"point " + resolverfile.Path},
		},
		{
			// The system path measured off the OS and every path resolving:
			// working, said of the measurement, with the wiring it went
			// through left to be found and the verdict unknown — ADR 005's
			// question is about the wiring `voya dns install` will write once
			// it is specified for this OS (#155).
			name: "off the OS, the system path measured and everything resolving",
			probes: Probes{ResolverUnmeasured: notConsulted, OSReadsResolverFile: false, DefaultCGODisabled: true,
				System: resolved, Default: resolved, PureGo: resolved},
			state:   DNSWorking,
			verdict: VerdictUnknown,
			says:    []string{"127.0.0.1", "so did this Go binary's own resolver", "CGO_ENABLED=0 build", "worth finding what routes it"},
			notSays: []string{"not checked here", "reaches the system resolver without cgo", "point " + resolverfile.Path, "One residue of the trap"},
		},
		{
			// The system path measured off the OS, everything but the pure
			// resolver resolving: `working`, with the residue the reading OS's
			// row carries said without the file (#134). Off the OS the
			// reading OS's sentence cannot be reused — it says the pure
			// resolver ignores the file, which #96 forbids naming here — and
			// leaving it out gave a developer whose netgo build is blind
			// `working` with no hint of why.
			name: "off the OS, the system path measured and the pure resolver still blind",
			probes: Probes{ResolverUnmeasured: notConsulted, OSReadsResolverFile: false, DefaultCGODisabled: true,
				System: resolved, Default: resolved, PureGo: nxdomain},
			state:   DNSWorking,
			verdict: VerdictUnknown,
			says: []string{"127.0.0.1", "so did this Go binary's own resolver", "One residue of the trap", "-tags netgo",
				"GODEBUG=netdns=go", "Go's pure resolver", "did not resolve *.internal through voya here"},
			notSays: []string{"not checked here", "is present", "is not installed", "point " + resolverfile.Path,
				"ignores " + resolverfile.Path, "client dialer is immune"},
		},
		{
			// The row above with the pure resolver unmeasured instead of
			// blind: off the OS the residue owes the same measurement every
			// other sentence here owes (#96), so it is not said at all — the
			// evidence line carries the probe's silence on its own.
			name: "off the OS, the system path measured and the pure resolver unmeasured",
			probes: Probes{ResolverUnmeasured: notConsulted, OSReadsResolverFile: false, DefaultCGODisabled: true,
				System: resolved, Default: resolved, PureGo: pureUnmeasured},
			state:    DNSWorking,
			verdict:  VerdictUnknown,
			says:     []string{"127.0.0.1", "worth finding what routes it"},
			notSays:  []string{"One residue of the trap", "-tags netgo", "GODEBUG=netdns=go"},
			evidence: []string{"pure resolver=not measured (the pure-resolver probe was not run)"},
		},
		{
			// The system path measured off the OS and answered by something
			// that is not voya: the reading OS's distinction between silence
			// and a wrong answer holds here too, and the file is still not
			// what is blamed.
			name: "off the OS, the system path measured and another resolver answering it",
			probes: Probes{ResolverUnmeasured: notConsulted, OSReadsResolverFile: false, DefaultCGODisabled: true,
				System: ProbeResult{Addrs: []string{"198.51.100.7"}}, Default: nxdomain, PureGo: nxdomain},
			state:   DNSBroken,
			verdict: VerdictUnknown,
			says:    []string{"not loopback", "198.51.100.7", "another resolver, VPN or search domain"},
			notSays: []string{"not checked here", "Is voya running", "did not resolve *.internal through voya", "point " + resolverfile.Path},
		},
		{
			// The system path measured off the OS and silent, while the Go
			// binary resolves: broken, said of the system path, and the
			// binary's own answer named rather than passed over.
			name: "off the OS, the system path measured silent while the binary resolves",
			probes: Probes{ResolverUnmeasured: notConsulted, OSReadsResolverFile: false, DefaultCGODisabled: true,
				System: nxdomain, Default: resolved, PureGo: resolved},
			state:   DNSBroken,
			verdict: VerdictUnknown,
			says:    []string{"did not resolve *.internal through voya", "though this Go binary's own resolver did"},
			notSays: []string{"not checked here", "Is voya running", "not loopback", "point " + resolverfile.Path},
		},
		{
			// The system path measured off the OS and silent, while something
			// that is not voya answers the Go binary. The distinction between
			// silence and a wrong answer is owed to the binary's row too — the
			// same probes with the system path unmeasured say "another resolver
			// is claiming *.internal here", and measuring the system path does
			// not make that address stop needing a reading.
			name: "off the OS, the system path measured silent while another resolver answers the binary",
			probes: Probes{ResolverUnmeasured: notConsulted, OSReadsResolverFile: false, DefaultCGODisabled: true,
				System: nxdomain, Default: ProbeResult{Addrs: []string{"198.51.100.7"}}, PureGo: nxdomain},
			state:   DNSBroken,
			verdict: VerdictUnknown,
			says: []string{"did not resolve *.internal through voya", "not loopback", "198.51.100.7",
				"claiming *.internal for it"},
			notSays: []string{"not checked here", "Is voya running", "point " + resolverfile.Path,
				"though this Go binary's own resolver did"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := ClassifyDNS(tc.probes)
			// The one blanket rule of #104's rows, held on every case so a
			// future row cannot drift past it: DNSResolverMissing is the
			// file's absence, and an unmeasured file never established it.
			if tc.probes.ResolverUnmeasured != "" && report.State == DNSResolverMissing {
				t.Errorf("an unmeasured file classified as %v; absence was never established:\n%s",
					report.State, report.Detail)
			}
			if report.State != tc.state {
				t.Fatalf("state = %v, want %v\n%s", report.State, tc.state, report.Detail)
			}
			if report.Verdict != tc.verdict {
				t.Fatalf("verdict = %v, want %v\n%s", report.Verdict, tc.verdict, report.Detail)
			}
			// says runs against the row's sentences alone (#263): the evidence
			// line quotes every probe's summary, so an entry naming a
			// measurement would pass against the whole detail whether the row
			// said it or not — prose's own warning. notSays stays on the
			// whole detail, the stricter side.
			for _, want := range tc.says {
				if !strings.Contains(prose(report.Detail), want) {
					t.Errorf("detail's prose does not say %q:\n%s", want, report.Detail)
				}
			}
			for _, banned := range tc.notSays {
				if strings.Contains(report.Detail, banned) {
					t.Errorf("detail says %q, which is not true of this state:\n%s", banned, report.Detail)
				}
			}
			// Sliced through [prose] rather than a second LastIndex of the
			// marker: a mutation that drops the evidence line has to fail
			// these pins by name, not panic every row on a -1 index.
			evidence := report.Detail[len(prose(report.Detail)):]
			for _, want := range tc.evidence {
				if !strings.Contains(evidence, want) {
					t.Errorf("the evidence line does not carry %q:%s", want, evidence)
				}
			}
			// The evidence line quotes every probe, and a probe nobody made
			// says so there rather than printing `=` with nothing after it —
			// which reads as a port or a path that answered nothing (#263).
			if strings.Contains(evidence, "=;") || strings.HasSuffix(evidence, "=") {
				t.Errorf("the evidence line prints a probe with nothing after the `=`:%s", evidence)
			}
			// No row may promise a command: `voya dns install` exists since
			// #156, and a remedy that names it names something to run now.
			if strings.Contains(report.Detail, "once it lands") || strings.Contains(report.Detail, "will write") {
				t.Errorf("detail speaks of `voya dns install` in the future tense; it is built (#156):\n%s", report.Detail)
			}
			// Every report ends in its evidence: the check's word has to beat
			// the developer's other tools, and measurements are what earn it.
			if !strings.Contains(report.Detail, "measured:") {
				t.Errorf("detail carries no measurements:\n%s", report.Detail)
			}
		})
	}
}

// The file is installed and the system does not resolve through it. On a host
// whose wiring is a service that reads its file at startup, the likeliest
// cause is that it has not read it — a restart that failed, or a file edited
// by hand — and the macOS-shaped remedy sends that developer to check voya and
// the port instead. The clause is the host's answer, not the classifier's, so
// both hosts are exercised from either machine; and no other state gains it,
// because a file nothing has read is not a story about a file that is missing.
func TestTheInstalledButNotAnsweringRemedyNamesTheReload(t *testing.T) {
	broken := Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
		System: ProbeResult{Err: "no such host"}, Default: ProbeResult{Err: "no such host"},
		PureGo: ProbeResult{Err: "no such host"}}
	missing := broken
	missing.ResolverInstalled = false

	old := resolverReload
	t.Cleanup(func() { resolverReload = old })

	resolverReload = "systemctl restart systemd-resolved.service"
	report := ClassifyDNS(broken)
	if report.State != DNSBroken {
		t.Fatalf("state = %v, want %v", report.State, DNSBroken)
	}
	// The restart — and, where whether this host resolves through resolved
	// went unmeasured, what a restart that changes nothing means: this
	// state's other Linux cause is a host running resolved whose resolution
	// does not go through it, where no number of restarts moves anything.
	// Not on `broken`, which #204's measurement found resolving through
	// resolved (#253): a host it finds bypassed takes its own row, and the
	// hedge here would be a finding off a measurement that said otherwise.
	for _, want := range []string{"not live yet", "sudo " + resolverReload, "sudo voya dns install --activate"} {
		if !strings.Contains(report.Detail, want) {
			t.Errorf("the remedy does not say %q:\n%s", want, report.Detail)
		}
	}
	if strings.Contains(report.Detail, "may not go through that service at all") {
		t.Errorf("a host measured resolving through resolved is told it may not:\n%s", report.Detail)
	}
	if got := ClassifyDNS(missing).Detail; strings.Contains(got, resolverReload) {
		t.Errorf("a file that is not installed is told to restart the service that would read it:\n%s", got)
	}
	// The flush is macOS's commands for macOS's cause; here the restart the
	// remedy already names empties the service's cache on the way (#220).
	if strings.Contains(report.Detail, "dscacheutil -flushcache") {
		t.Errorf("a host whose restart empties the cache was given the macOS flush:\n%s", report.Detail)
	}
	// But only the service's own cache: a dnsmasq or resolvconf in front of
	// resolved — the host the remedy's last sentence is written for — holds
	// its negative answer past that restart, and #220's scenario reproduces
	// there with no flush named. Where whether anything is in front went
	// unmeasured, the row says the cache survives, and that clearing it
	// means restarting the front resolver, which one being in front
	// unmeasured (#243) — said as the condition it is, not a finding.
	unread := broken
	unread.ServiceBypassedUnmeasured = "/etc/resolv.conf: permission denied"
	unreadDetail := ClassifyDNS(unread).Detail
	for _, want := range []string{"may not go through that service at all",
		"If a dnsmasq or resolvconf is in front of resolved",
		"survives that restart", "restarting whatever is in front", "this run did not measure"} {
		if !strings.Contains(unreadDetail, want) {
			t.Errorf("the remedy does not say %q where the bypass went unmeasured:\n%s", want, unreadDetail)
		}
	}
	// And not where the bypass was measured (#253): `broken` is the host
	// #204's measurement found resolving through resolved — nothing in front,
	// as far as that measurement can tell, and a forwarder is one it calls
	// bypassed, which takes its own row — so supposing a front resolver here
	// would be a finding off a measurement that said the opposite. That
	// row's clause is TestTheBypassRowNamesTheFrontResolversCache's.
	if strings.Contains(report.Detail, "in front of resolved") ||
		strings.Contains(report.Detail, "restarting whatever is in front") {
		t.Errorf("a host measured resolving through resolved is told about a front resolver's cache:\n%s",
			report.Detail)
	}

	// A host whose file is live as it lands has no such step, and inventing
	// one would send a Mac developer after a service that is not there.
	resolverReload = ""
	if got := ClassifyDNS(broken).Detail; strings.Contains(got, "not live yet") {
		t.Errorf("a host with nothing to reload was given a reload step:\n%s", got)
	}
	// What it has instead is a cache that outlives a reinstall (#220): the
	// row's detail must name the stale cache as a cause beside the stopped
	// daemon, with the flush commands, or a re-install with voya running
	// leaves the reader checking the one cause that is not it.
	// The port to dig is the file's, not the configured one: the sentence
	// before this warns that changing dns_port means rewriting the file, and
	// a reader who dug voya's new port would be told "stale cache" about the
	// mismatch that sentence just named.
	for _, want := range []string{"Is voya running", "cached before this file landed",
		"the port the file names", "sudo dscacheutil -flushcache; sudo killall -HUP mDNSResponder"} {
		if got := ClassifyDNS(broken).Detail; !strings.Contains(got, want) {
			t.Errorf("the remedy does not say %q:\n%s", want, got)
		}
	}
	if got := ClassifyDNS(missing).Detail; strings.Contains(got, "dscacheutil -flushcache") {
		t.Errorf("a file that is not installed is told to flush a cache the row never measured:\n%s", got)
	}
	// Where something answered, the certainty is not said — but the cached
	// answer is (#243): mDNSResponder can serve a non-loopback answer from
	// before the file landed, which reads identically to the competing
	// resolver the branch names, so both are offered, neither as certain,
	// with the flush as what settles it.
	answered := broken
	answered.System = ProbeResult{Addrs: []string{"198.51.100.7"}}
	answeredDetail := ClassifyDNS(answered).Detail
	for _, want := range []string{"another resolver, VPN or search domain",
		"Or nothing is claiming it at all", "cached before the file landed",
		"sudo dscacheutil -flushcache; sudo killall -HUP mDNSResponder", "survives the flush"} {
		if !strings.Contains(answeredDetail, want) {
			t.Errorf("the answered branch does not say %q:\n%s", want, answeredDetail)
		}
	}
	if strings.Contains(answeredDetail, "the cause is a stale cache") {
		t.Errorf("a lookup that did answer was given the silent branch's certainty:\n%s", answeredDetail)
	}
	// On the service host the hint would be macOS's commands for a cache the
	// restart the same paragraph prescribes already empties.
	resolverReload = "systemctl restart systemd-resolved.service"
	if got := ClassifyDNS(answered).Detail; strings.Contains(got, "dscacheutil") {
		t.Errorf("the service host's answered branch was given the macOS flush:\n%s", got)
	}
	resolverReload = ""
	// Nor the unmeasured-catch-all caveat (#185): where no service reads the
	// file there is no global scope, so the question that went unanswered on
	// Linux was never a question here — and `resolvectl` is not on this host.
	unasked := broken
	unasked.GlobalCatchAllUnmeasured = "configd routes each /etc/resolver file by its own name; " +
		"there is no global scope to ask"
	if got := ClassifyDNS(unasked).Detail; strings.Contains(got, "resolvectl") {
		t.Errorf("a host with no global scope was told a scope went unread:\n%s", got)
	}
	resolverReload = "systemctl restart systemd-resolved.service"
	if got := ClassifyDNS(unasked).Detail; !strings.Contains(got, "resolvectl domain") {
		t.Errorf("a host whose global scope went unread was not told so:\n%s", got)
	}
}

// The not-answering row's question — is voya answering on the port the file
// names? — is one the check can settle by dialling that port, and since #243
// [Probes.AtFilePort] carries the dial: the row states the measurement's
// answer instead of sending the developer to `dig` for it. Where no dial was
// made — unmeasured, or the zero value a caller that never dialled leaves —
// the question stays prose, so a "no" nobody measured is never acted on
// (#104). Both hosts are exercised from either machine, as ever.
func TestTheNotAnsweringRowStatesTheFilePortMeasurement(t *testing.T) {
	silent := Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
		System: ProbeResult{Err: "no such host"}, Default: ProbeResult{Err: "no such host"},
		PureGo: ProbeResult{Err: "no such host"}}

	old := resolverReload
	t.Cleanup(func() { resolverReload = old })

	for _, host := range []struct{ name, reload string }{
		{name: "file live as it lands", reload: ""},
		{name: "wiring is a service", reload: "systemctl restart systemd-resolved.service"},
	} {
		t.Run(host.name, func(t *testing.T) {
			resolverReload = host.reload

			// voya answered on the file's port: the question is not asked,
			// the answer is stated, and voya and the file are exonerated.
			answering := silent
			answering.AtFilePort = ProbeResult{Addrs: []string{"127.0.0.1"}}
			got := ClassifyDNS(answering).Detail
			if !strings.Contains(got, "voya is answering on the port the file names") {
				t.Errorf("a measured yes is not stated:\n%s", got)
			}
			// Naming the address it asked, not "that server": the dial is
			// made on loopback at the port the file names, and a file
			// hand-edited to name some other address is dialled on loopback
			// all the same, so "that server" would claim a measurement of a
			// server nothing asked.
			if !strings.Contains(got, "asked 127.0.0.1 on that port directly") {
				t.Errorf("the row does not say which address it dialled:\n%s", got)
			}
			for _, wrong := range []string{"Is voya running?", "If it is running", "dig @127.0.0.1"} {
				if strings.Contains(got, wrong) {
					t.Errorf("the row says %q about a question the dial answered:\n%s", wrong, got)
				}
			}
			if macOS := host.reload == ""; macOS != strings.Contains(got, "The likeliest cause is a stale cache") {
				t.Errorf("naming the stale cache as the likeliest cause is the flushable host's "+
					"clause alone (want %v):\n%s", macOS, got)
			}

			// Nothing that is voya answered there: the row says so with the
			// evidence, and no flush points away from the dead port.
			dead := silent
			dead.AtFilePort = ProbeResult{Err: "read udp 127.0.0.1:53535: connection refused"}
			got = prose(ClassifyDNS(dead).Detail)
			if !strings.Contains(got, "Nothing that is voya is answering on the port the file names") ||
				!strings.Contains(got, "connection refused") ||
				!strings.Contains(got, "asked 127.0.0.1 on that port directly") {
				t.Errorf("a measured no is not stated with its evidence, or does not say which "+
					"address it dialled:\n%s", got)
			}
			if strings.Contains(got, "dscacheutil") {
				t.Errorf("a port measured dead was blamed on a cache:\n%s", got)
			}
			// The service host's cache clause goes the same way and for the
			// same reason: a port with nothing behind it is not a port a
			// front resolver's cache is standing in front of, and the clause
			// would send the developer to restart it while the sentence
			// above says to start voya.
			if strings.Contains(got, "restarting whatever is in front") {
				t.Errorf("a port measured dead was blamed on a front resolver's cache:\n%s", got)
			}
			// An answer that is not loopback is not voya either, quoted.
			held := silent
			held.AtFilePort = ProbeResult{Addrs: []string{"203.0.113.9"}}
			if got := prose(ClassifyDNS(held).Detail); !strings.Contains(got,
				"Nothing that is voya is answering on the port the file names") ||
				!strings.Contains(got, "203.0.113.9") {
				t.Errorf("a non-loopback answer on the file's port reads as voya:\n%s", got)
			}

			// No dial made — carried as unmeasured, or the zero value of a
			// caller that never dialled — keeps the prose question.
			for _, undialled := range []ProbeResult{{Unmeasured: "no file to name one"}, {}} {
				unmeasured := silent
				unmeasured.AtFilePort = undialled
				got := ClassifyDNS(unmeasured).Detail
				if !strings.Contains(got, "Is voya running?") {
					t.Errorf("a dial nobody made (%+v) silenced the question:\n%s", undialled, got)
				}
				if strings.Contains(got, "answering on the port the file names either") {
					t.Errorf("a dial nobody made (%+v) reads as a measured no:\n%s", undialled, got)
				}
			}
		})
	}
}

// TestTheBypassRowNamesTheFrontResolversCache pins where the front
// resolver's cache clause lives (#253): the host it is written for — a
// dnsmasq or resolvconf in front of resolved — is one #204's measurement
// cannot tell from a static /etc/resolv.conf, so it is measured bypassed and
// takes the bypass row, whose forwarder sentence already names it as a
// condition and prescribes the restart that empties only resolved's cache.
// The clause follows that sentence there, and is gated the way the
// not-answering row's is: gone where the dial measured the port with nothing
// that is voya behind it, where the system answered, and on the host whose
// file is live as it lands.
func TestTheBypassRowNamesTheFrontResolversCache(t *testing.T) {
	old := resolverReload
	t.Cleanup(func() { resolverReload = old })
	resolverReload = "systemctl restart systemd-resolved.service"

	nxdomain := ProbeResult{Err: "no such host"}
	bypassed := Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
		System: nxdomain, Default: nxdomain, PureGo: nxdomain,
		ServiceBypassed: true, ServiceBypassedWhy: "/etc/resolv.conf is not resolved's stub and names no 127.0.0.53",
		AtFilePort: ProbeResult{Unmeasured: "no file was read for a port"}}
	report := ClassifyDNS(bypassed)
	if report.State != DNSBroken {
		t.Fatalf("state = %v, want %v", report.State, DNSBroken)
	}
	if !strings.Contains(report.Detail, "systemd-resolved at all") {
		t.Fatalf("the probes did not reach the bypass row:\n%s", report.Detail)
	}
	for _, want := range []string{"If a dnsmasq or resolvconf is in front of resolved",
		"survives that restart", "restarting whatever is in front", "this run did not measure"} {
		if !strings.Contains(report.Detail, want) {
			t.Errorf("the bypass row does not say %q:\n%s", want, report.Detail)
		}
	}
	// After the sentence that names the host, not before it: "the sentence
	// above" has to be the forwarder's.
	forwarder := strings.Index(report.Detail, "if that is this host")
	cache := strings.Index(report.Detail, "in front of resolved")
	if forwarder < 0 || cache < forwarder {
		t.Errorf("the cache clause (at %d) does not follow the forwarder sentence (at %d):\n%s",
			cache, forwarder, report.Detail)
	}

	// The same gates as the not-answering row's clause, for the same
	// reasons, each one mutated on its own.
	for _, tc := range []struct {
		name string
		edit func(p *Probes)
	}{
		{name: "port measured with nothing that is voya behind it", edit: func(p *Probes) {
			p.AtFilePort = ProbeResult{Err: "read udp 127.0.0.1:53535: connection refused"}
		}},
		{name: "system answered", edit: func(p *Probes) {
			p.System = ProbeResult{Addrs: []string{"198.51.100.7"}}
		}},
		{name: "file live as it lands", edit: func(*Probes) { resolverReload = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(func() { resolverReload = "systemctl restart systemd-resolved.service" })
			p := bypassed
			tc.edit(&p)
			got := prose(ClassifyDNS(p).Detail)
			if strings.Contains(got, "restarting whatever is in front") || strings.Contains(got, "survives that restart") {
				t.Errorf("the cache clause is said where it points away from the row:\n%s", got)
			}
		})
	}
	// Measured answering keeps it: voya is up, the restart is the fix on the
	// forwarder host, and the cache is what survives that fix.
	up := bypassed
	up.AtFilePort = ProbeResult{Addrs: []string{"127.0.0.1"}}
	if got := ClassifyDNS(up).Detail; !strings.Contains(got, "restarting whatever is in front") {
		t.Errorf("a port measured answering lost the cache clause:\n%s", got)
	}
}

// TestProbesUnderAFiredDeadlineAreUnmeasured pins probeWith's side of #100's
// rule (#253): a deadline that fires before the resolver answers is not a
// measured "no". Before it, a port that black-holes UDP reached the
// not-answering row as an Err off the caller's fired budget, and the row
// asserted nothing that is voya was answering there off a timeout. All three
// probes share probeWith, so all three are exercised; the control is the
// same dead port under a live deadline, whose refusal stays a measurement.
func TestProbesUnderAFiredDeadlineAreUnmeasured(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a dead port: %v", err)
	}
	dead := conn.LocalAddr().String()
	_ = conn.Close()

	live, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if control := ProbeAt(live, dead, "voya-dns-check-probe.internal"); control.Err == "" || control.Unmeasured != "" {
		t.Fatalf("control: ProbeAt against nothing under a live deadline reported %+v, want an Err", control)
	}

	fired, cancelFired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelFired()
	for _, tc := range []struct {
		name  string
		probe func(context.Context, string) ProbeResult
	}{
		{name: "ProbeAt", probe: func(ctx context.Context, name string) ProbeResult { return ProbeAt(ctx, dead, name) }},
		{name: "ProbeDefault", probe: ProbeDefault},
		{name: "ProbePureGo", probe: ProbePureGo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.probe(fired, "voya-dns-check-probe.internal")
			if r.Unmeasured == "" || r.Err != "" || len(r.Addrs) != 0 {
				t.Fatalf("a fired deadline produced %+v, want Unmeasured alone", r)
			}
			if !strings.Contains(r.Unmeasured, "deadline") {
				t.Errorf("a fired deadline does not name itself: %q", r.Unmeasured)
			}
		})
	}

	// And the row it feeds keeps its question: the budget running out is a
	// dial that measured nothing, so the not-answering row asks as prose
	// rather than quoting a no, and the evidence line says why.
	silent := Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
		System: ProbeResult{Err: "no such host"}, Default: ProbeResult{Err: "no such host"},
		PureGo:     ProbeResult{Err: "no such host"},
		AtFilePort: ProbeAt(fired, dead, "voya-dns-check-probe.internal")}
	got := ClassifyDNS(silent).Detail
	if !strings.Contains(got, "Is voya running?") {
		t.Errorf("a dial whose budget ran out silenced the question:\n%s", got)
	}
	if strings.Contains(got, "answering on the port the file names either") {
		t.Errorf("a dial whose budget ran out reads as a measured no:\n%s", got)
	}
	if !strings.Contains(got, "file port=not measured (lookup voya-dns-check-probe.internal: context deadline exceeded)") {
		t.Errorf("the evidence line does not carry the fired deadline:\n%s", got)
	}
}

// prose is a report's detail without the evidence line it ends with. Every
// probe's summary is quoted there, so an assertion that a sentence names a
// measurement has to be made against the sentences alone — against the whole
// detail it passes whether the row quoted the measurement or not, which is
// the unconditionally-true control this repo has shipped before.
func prose(detail string) string {
	if i := strings.LastIndex(detail, "\nmeasured: "); i >= 0 {
		return detail[:i]
	}
	return detail
}

// The three rows whose cause is neither the file nor voya — the catch-all
// (#185), the bypass (#204) and the too-old resolved (#206) — used to end by
// refusing to say whether voya was up, because nothing in Probes measured it.
// Since #243 the file-port dial is made wherever the file names a port,
// whichever row then wins, so each row states that measurement where there is
// one and keeps the refusal, word for word, where there is not (#104).
func TestTheNotVoyasFaultRowsStateTheFilePortDial(t *testing.T) {
	old := resolverReload
	t.Cleanup(func() { resolverReload = old })
	resolverReload = "systemctl restart systemd-resolved.service"

	nxdomain := ProbeResult{Err: "no such host"}
	base := Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
		System: nxdomain, Default: nxdomain, PureGo: nxdomain}
	catchAll, bypassed, tooOld := base, base, base
	catchAll.GlobalCatchAll = true
	bypassed.ServiceBypassed = true
	bypassed.ServiceBypassedWhy = "/etc/resolv.conf is not resolved's stub and names no 127.0.0.53"
	tooOld.ServiceTooOld = true
	tooOld.ServiceTooOldWhy = "systemd-resolved is v245, older than v246"

	for _, row := range []struct {
		name   string
		probes Probes
	}{
		{name: "catch-all", probes: catchAll},
		{name: "bypassed", probes: bypassed},
		{name: "too old", probes: tooOld},
	} {
		t.Run(row.name, func(t *testing.T) {
			// The dial nobody made: the sentence these rows have always
			// ended on, unchanged.
			for _, undialled := range []ProbeResult{{}, {Unmeasured: "no file to name one"}} {
				p := row.probes
				p.AtFilePort = undialled
				got := ClassifyDNS(p).Detail
				if !strings.Contains(got, "Whether voya itself is up is a question this row did not measure") {
					t.Errorf("a dial nobody made (%+v) is reported as one:\n%s", undialled, got)
				}
				if strings.Contains(got, "asked 127.0.0.1 on the port the file names") {
					t.Errorf("a dial nobody made (%+v) is quoted:\n%s", undialled, got)
				}
			}

			// Measured answering: voya is up, and the row still says the
			// lookup does not reach it — the half the dial says nothing
			// about, and this row's whole cause.
			up := row.probes
			up.AtFilePort = ProbeResult{Addrs: []string{"127.0.0.1"}}
			got := ClassifyDNS(up).Detail
			for _, want := range []string{"voya itself is up, at least",
				"asked 127.0.0.1 on the port the file names", "not the thing to fix here"} {
				if !strings.Contains(got, want) {
					t.Errorf("the row does not say %q of a port that answered:\n%s", want, got)
				}
			}
			if strings.Contains(got, "this row did not measure") {
				t.Errorf("the row refuses to say what it measured:\n%s", got)
			}

			// Measured with nothing that is voya behind it: said with the
			// evidence, and still not offered as this row's fix.
			down := row.probes
			down.AtFilePort = ProbeResult{Err: "read udp 127.0.0.1:53535: connection refused"}
			got = prose(ClassifyDNS(down).Detail)
			for _, want := range []string{"voya is worth starting besides", "connection refused",
				"until the wiring above is fixed"} {
				if !strings.Contains(got, want) {
					t.Errorf("the row does not say %q of a port that did not answer:\n%s", want, got)
				}
			}
			if strings.Contains(got, "this row did not measure") {
				t.Errorf("the row refuses to say what it measured:\n%s", got)
			}
		})
	}
}

// The evidence line every report ends with carries the file-port dial too:
// DNSReport's contract is that the raw measurements are there to be believed
// over the tools that lie, and until #243's dial joined them the row's
// strongest new claim — voya is running and the file is right — was the one
// claim with nothing quoted behind it. A dial nobody made says so, with its
// reason where there is one, and the zero value's reasonless silence reads as
// unmeasured rather than as a port that answered nothing (#104) — named at
// the door with the other three probes since #263.
func TestTheEvidenceLineCarriesTheFilePortDial(t *testing.T) {
	nxdomain := ProbeResult{Err: "no such host"}
	base := Probes{ResolverInstalled: true, OSReadsResolverFile: true, DefaultCGODisabled: true,
		System: nxdomain, Default: nxdomain, PureGo: nxdomain}

	for _, tc := range []struct {
		name string
		at   ProbeResult
		want string
	}{
		{name: "answered", at: ProbeResult{Addrs: []string{"127.0.0.1"}}, want: "file port=127.0.0.1"},
		{name: "failed", at: ProbeResult{Err: "connection refused"}, want: "file port=connection refused"},
		{name: "unmeasured with a reason", at: ProbeResult{Unmeasured: "the file names no port"},
			want: "file port=not measured (the file names no port)"},
		{name: "never dialled", at: ProbeResult{}, want: "file port=not measured (no probe was made)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			p.AtFilePort = tc.at
			got := ClassifyDNS(p).Detail
			measured := got[strings.LastIndex(got, "\nmeasured: ")+1:]
			if !strings.Contains(measured, tc.want) {
				t.Errorf("the evidence line does not carry %q:\n%s", tc.want, measured)
			}
		})
	}
}

func TestThroughListener(t *testing.T) {
	cases := []struct {
		name string
		r    ProbeResult
		want bool
	}{
		{"loopback v4", ProbeResult{Addrs: []string{"127.0.0.1"}}, true},
		{"loopback v6", ProbeResult{Addrs: []string{"::1"}}, true},
		// net.ParseIP rejects the zone; the system path can hand one back.
		{"loopback v6 with a zone", ProbeResult{Addrs: []string{"::1%lo0"}}, true},
		{"link-local with a zone", ProbeResult{Addrs: []string{"fe80::1%lo0"}}, false},
		{"failed", ProbeResult{Err: "no such host"}, false},
		{"resolved nothing", ProbeResult{}, false},
		{"not loopback", ProbeResult{Addrs: []string{"198.51.100.7"}}, false},
		{"loopback among strangers", ProbeResult{Addrs: []string{"127.0.0.1", "198.51.100.7"}}, false},
		{"unparseable", ProbeResult{Addrs: []string{"not-an-ip"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.ThroughListener(); got != tc.want {
				t.Fatalf("ThroughListener(%+v) = %v, want %v", tc.r, got, tc.want)
			}
		})
	}
}

// TestCGODisabledMatchesTheBuild holds the helper against the authority on
// the question — the build settings `go version -m` reads out of the binary
// that is running this test. It asserts agreement rather than a fixed answer,
// so it is a real check whichever way the test binary was built: `go test
// -race` forces cgo on, plain `go test` on a host without a C toolchain
// leaves it off, and the helper has to be right about both.
func TestCGODisabledMatchesTheBuild(t *testing.T) {
	disabled, known := CGODisabled()
	if !known {
		t.Fatalf("CGODisabled could not read this binary's build settings; a `go test` binary always carries them")
	}
	out, err := exec.Command("go", "version", "-m", os.Args[0]).Output()
	if err != nil {
		t.Fatalf("go version -m %s: %v", os.Args[0], err)
	}
	want := strings.Contains(string(out), "build\tCGO_ENABLED=0")
	if disabled != want {
		t.Fatalf("CGODisabled() = %v, but go version -m says CGO_ENABLED=0 is %v:\n%s", disabled, want, out)
	}
	t.Logf("this test binary was built with cgo disabled=%v, and the helper agrees", disabled)
}

// TestVerdictExitCodes pins #99's table: ok 0, problem 1, unknown 2 — three
// codes because a check that could not be run is not a pass and not a
// failure. The zero value must be unknown, so a report nobody scored exits as
// "not answered" rather than as either.
func TestVerdictExitCodes(t *testing.T) {
	var zero Verdict
	if zero != VerdictUnknown {
		t.Fatalf("the zero Verdict is %v; a forgotten verdict must read as unknown, never as a pass", zero)
	}
	for _, tc := range []struct {
		v    Verdict
		code int
		word string
	}{
		{VerdictOK, 0, "ok"},
		{VerdictProblem, 1, "problem"},
		{VerdictUnknown, 2, "unknown"},
	} {
		if got := tc.v.ExitCode(); got != tc.code {
			t.Errorf("%v.ExitCode() = %d, want %d", tc.v, got, tc.code)
		}
		if got := tc.v.String(); got != tc.word {
			t.Errorf("Verdict.String() = %q, want %q", got, tc.word)
		}
	}
}

// The off-OS bucket holds two kinds of host, and only one of them has a file
// to name: [resolverfile.Path] is "" on an OS ADR 002 specifies no wiring for,
// where both of the file sentences would open on an empty string — "'s state
// was not established (no stat was made)", which is what `voya dns check` printed
// on Windows and FreeBSD once the path became a var. No row in the table above
// can catch it, because the path is derived from runtime.GOOS and every machine
// this suite runs on has a wiring; so the var itself is the seam.
//
// The Linux host with no systemd-resolved is the other way into the bucket and
// does have a path, which the table's own two rows pin — this asserts only
// that the sentences go when there is nothing for them to name, and that what
// is left still says why the file is beside the point here.
func TestOffTheOSWithNoFileToNameSaysNothingAboutOne(t *testing.T) {
	old := resolverfile.Path
	t.Cleanup(func() { resolverfile.Path = old })
	resolverfile.Path = ""

	nxdomain := ProbeResult{Err: "lookup voya-dns-check-probe.internal: no such host"}
	offOS := ProbeResult{Unmeasured: "no system-path probe on windows (#155)"}
	for _, tc := range []struct {
		name   string
		probes Probes
	}{
		{name: "no stat was made", probes: Probes{ResolverUnmeasured: "no stat was made",
			DefaultCGODisabled: true, System: offOS, Default: nxdomain, PureGo: nxdomain}},
		{name: "a file a caller reported present", probes: Probes{ResolverInstalled: true,
			DefaultCGODisabled: true, System: offOS, Default: nxdomain, PureGo: nxdomain}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			detail := ClassifyDNS(tc.probes).Detail
			for _, unwanted := range []string{"'s state was not established", "is present, but nothing here reads it"} {
				if strings.Contains(detail, unwanted) {
					t.Errorf("a host with no wiring file is told %q about one:\n%s", unwanted, detail)
				}
			}
			if !strings.Contains(detail, "nothing on this machine reads a *.internal wiring file") {
				t.Errorf("the report no longer says why the file is beside the point here:\n%s", detail)
			}
		})
	}
}

// TestProbeSystemOffTheOSIsUnmeasured pins #96's first bucket: where no
// system probe exists, the result is unmeasured — never an error a
// classifier could read as "the name did not resolve". Driven through the OS
// seam rather than skipped off darwin, since #155 gave Linux a probe too and
// the OSes with none are now the ones no CI leg runs on.
func TestProbeSystemOffTheOSIsUnmeasured(t *testing.T) {
	for _, goos := range []string{"windows", "freebsd", "plan9"} {
		t.Run(goos, func(t *testing.T) {
			asked := false
			substituteSystemProbeOn(t, goos, func() { asked = true }, "/bin/sh", "-c", "echo unreachable")
			r := ProbeSystem(context.Background(), "voya-dns-check-probe.internal")
			if r.Unmeasured == "" || r.Err != "" || len(r.Addrs) != 0 {
				t.Fatalf("ProbeSystem on %s = %+v, want Unmeasured alone", goos, r)
			}
			if !strings.Contains(r.Unmeasured, goos) {
				t.Errorf("the reason does not name the OS it is about: %q", r.Unmeasured)
			}
			// The gate is before the tool, not after: an OS with no probe has
			// no command to run, and running one would be a measurement made
			// by a tool voya has not decided is the right one here.
			if asked {
				t.Errorf("ProbeSystem ran a command on %s, which has no system probe", goos)
			}
		})
	}
}

// substituteSystemProbeOn swaps both of [ProbeSystem]'s seams: the OS it
// dispatches on and the command it runs (#100, #155), so every row of every
// OS's probe is exercisable from one machine — and notes that the command was
// consulted at all, which is what the rows about OSes with no probe assert
// did not happen.
func substituteSystemProbeOn(t *testing.T, goos string, ran func(), name string, args ...string) {
	t.Helper()
	restoreCmd, restoreOS := systemProbeCmd, probeOS
	t.Cleanup(func() { systemProbeCmd, probeOS = restoreCmd, restoreOS })
	probeOS = goos
	systemProbeCmd = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		if ran != nil {
			ran()
		}
		return exec.CommandContext(ctx, name, args...)
	}
}

// substituteSystemProbe is [substituteSystemProbeOn] for macOS's probe, which
// is the one whose failure modes #100 pinned.
func substituteSystemProbe(t *testing.T, name string, args ...string) {
	t.Helper()
	substituteSystemProbeOn(t, "darwin", nil, name, args...)
}

// TestProbeSystemToolFailureIsUnmeasured pins #96's second bucket and #100's
// quoting: a dscacheutil that could not run is unmeasured, not a measured
// "no", and the tool's own complaint is quoted — first stderr line only,
// because the string is pasted verbatim into the report.
func TestProbeSystemToolFailureIsUnmeasured(t *testing.T) {
	substituteSystemProbe(t, "/bin/sh", "-c",
		`echo "directory service on fire" >&2; echo "second line" >&2; exit 1`)
	r := ProbeSystem(context.Background(), "voya-dns-check-probe.internal")
	if r.Unmeasured == "" || r.Err != "" {
		t.Fatalf("a failing tool produced %+v, want Unmeasured alone", r)
	}
	if !strings.Contains(r.Unmeasured, "directory service on fire") {
		t.Errorf("the tool's own complaint is not quoted: %q", r.Unmeasured)
	}
	if strings.Contains(r.Unmeasured, "second line") {
		t.Errorf("more than the first stderr line is quoted: %q", r.Unmeasured)
	}
}

// TestProbeSystemDeadlineIsUnmeasured pins #96's third bucket, with #100's
// reading: the context is consulted first, so an expired deadline names
// itself rather than surfacing as the "signal: killed" of the process it
// took down. Both probes, because Linux's has one more way out — getent's
// "not found" exit — and a deadline that fires while getent is running must
// not take it (#155).
func TestProbeSystemDeadlineIsUnmeasured(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			substituteSystemProbeOn(t, goos, nil, "sleep", "5")
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			r := ProbeSystem(ctx, "voya-dns-check-probe.internal")
			if r.Unmeasured == "" || r.Err != "" {
				t.Fatalf("an expired deadline produced %+v, want Unmeasured alone", r)
			}
			if !strings.Contains(r.Unmeasured, "deadline") {
				t.Errorf("an expired deadline does not name itself: %q", r.Unmeasured)
			}
			if strings.Contains(r.Unmeasured, "signal: killed") {
				t.Errorf("the deadline reads as the kill it caused: %q", r.Unmeasured)
			}
		})
	}
}

// TestProbeSystemOnLinuxReadsTheNameServiceSwitch is Linux's probe row by row
// (#155): what getent(1) prints, what it exits with, and which of those are
// measurements. The split that matters is getent's exit 2 — "not found by any
// name service" is the answer `voya dns check` most often has to give on an
// unwired host, and reporting it as a tool failure would cost the report its
// whole Linux answer; every other nonzero status is a question that never got
// asked.
func TestProbeSystemOnLinuxReadsTheNameServiceSwitch(t *testing.T) {
	for _, tc := range []struct {
		name string
		// script is what stands in for getent: its stdout, stderr and status.
		script     string
		addrs      []string
		errHas     string
		unmeasured string
	}{
		{name: "one address, printed once per socket type, is one answer",
			script: `printf '127.0.0.1       STREAM voya-dns-check-probe.internal\n127.0.0.1       DGRAM\n127.0.0.1       RAW\n'`,
			addrs:  []string{"127.0.0.1"}},
		{name: "several addresses are all reported, in the order NSS gave them",
			script: `printf '::1     STREAM localhost\n::1     DGRAM\n127.0.0.1       STREAM\n127.0.0.1       DGRAM\n'`,
			addrs:  []string{"::1", "127.0.0.1"}},
		{name: "a name no name service knows is a measured no, not a tool failure",
			script: "exit 2",
			errHas: "not found by any name service"},
		{name: "a getent that is not installed is unmeasured",
			script: "", unmeasured: "getent ahosts"},
		{name: "an unknown database is unmeasured with getent's own complaint",
			script:     `echo "Unknown database: ahosts" >&2; exit 1`,
			unmeasured: "Unknown database: ahosts"},
		{name: "an empty answer with a zero status is a measured no",
			script: "true",
			errHas: "returned no addresses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.script == "" {
				substituteSystemProbeOn(t, "linux", nil, filepath.Join(t.TempDir(), "no-such-getent"))
			} else {
				substituteSystemProbeOn(t, "linux", nil, "/bin/sh", "-c", tc.script)
			}
			r := ProbeSystem(context.Background(), "voya-dns-check-probe.internal")
			switch {
			case tc.unmeasured != "":
				if r.Unmeasured == "" || r.Err != "" || len(r.Addrs) != 0 {
					t.Fatalf("ProbeSystem = %+v, want Unmeasured alone", r)
				}
				if !strings.Contains(r.Unmeasured, tc.unmeasured) {
					t.Errorf("the reason does not say %q: %q", tc.unmeasured, r.Unmeasured)
				}
			case tc.errHas != "":
				if r.Err == "" || r.Unmeasured != "" || len(r.Addrs) != 0 {
					t.Fatalf("ProbeSystem = %+v, want a measured failure alone", r)
				}
				if !strings.Contains(r.Err, tc.errHas) {
					t.Errorf("the measurement does not say %q: %q", tc.errHas, r.Err)
				}
			default:
				if r.Err != "" || r.Unmeasured != "" {
					t.Fatalf("ProbeSystem = %+v, want addresses alone", r)
				}
				if !slices.Equal(r.Addrs, tc.addrs) {
					t.Errorf("ProbeSystem = %v, want %v", r.Addrs, tc.addrs)
				}
			}
		})
	}
}

// The Linux probe is `getent ahosts <name>` and not `resolvectl query`: the
// first goes through NSS, which is the path getaddrinfo takes and therefore
// the one curl and browsers take, and the second talks to systemd-resolved
// over D-Bus — a bypass of NSS that would answer for a mechanism the system's
// own resolution might not be using (#155). The name is asked about as given.
func TestProbeSystemOnLinuxAsksNSSAndNotResolved(t *testing.T) {
	restoreCmd, restoreOS := systemProbeCmd, probeOS
	t.Cleanup(func() { systemProbeCmd, probeOS = restoreCmd, restoreOS })
	probeOS = "linux"
	var asked []string
	systemProbeCmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		asked = append([]string{name}, args...)
		return exec.CommandContext(ctx, "/bin/sh", "-c", "exit 2")
	}
	ProbeSystem(context.Background(), "voya-dns-check-probe.internal")
	if want := []string{"getent", "ahosts", "voya-dns-check-probe.internal"}; !slices.Equal(asked, want) {
		t.Errorf("the Linux probe ran %v, want %v", asked, want)
	}
}

func TestFirstLine(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"one line", "boom\n", "boom"},
		{"first of several", "boom\nand more\nand more", "boom"},
		{"surrounding space", "  boom  \nx", "boom"},
		{"capped", strings.Repeat("a", 300), strings.Repeat("a", 200) + "…"},
		// The cap is in bytes and the tool's complaint need not be ASCII, so
		// the byte it lands on can sit inside a rune. Cutting there would
		// paste half a character into the report verbatim; the last whole
		// rune before the cap is what is kept instead. Here the 200th byte is
		// the middle of a three-byte rune that starts at byte 199.
		{"capped inside a rune", strings.Repeat("a", 199) + strings.Repeat("€", 5), strings.Repeat("a", 199) + "…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := firstLine([]byte(tc.in), 200)
			if got != tc.want {
				t.Fatalf("firstLine(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("firstLine(%q) = %q, which is not valid UTF-8; it is pasted into the report verbatim", tc.in, got)
			}
		})
	}
}

// serveWildcard starts the real DNS server on an ephemeral loopback port and
// returns its address.
func serveWildcard(t *testing.T) string {
	t.Helper()
	ln, err := dns.Listen(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("dns.Listen: %v", err)
	}
	srv := dns.NewServer(ln, nil)
	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()
	t.Cleanup(func() {
		_ = srv.Close()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return ln.Addr().String()
}

// TestProbeAt proves the probe machinery can answer "yes": pointed at a live
// wildcard listener it must resolve loopback, and pointed at a dead port it
// must fail. Without the first, every negative the trap test asserts would
// also pass with a probe that simply cannot resolve anything.
func TestProbeAt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	live := ProbeAt(ctx, serveWildcard(t), "payments.internal")
	if !live.ThroughListener() {
		t.Fatalf("ProbeAt against a live listener did not resolve loopback: %+v", live)
	}

	// A port that was just now free: bound, read, released. The race with
	// another process grabbing it exists and loses — anything that answers
	// a DNS query with something other than loopback still fails the probe.
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a dead port: %v", err)
	}
	dead := conn.LocalAddr().String()
	_ = conn.Close()

	gone := ProbeAt(ctx, dead, "payments.internal")
	if gone.ThroughListener() || gone.Err == "" {
		t.Fatalf("ProbeAt against nothing reported %+v, want an error", gone)
	}
}

// TestProbeAtDialHonorsNetwork pins the dial hook's contract (#97): the hook
// redirects where a query goes, never how — asked for "tcp", it must produce
// a TCP connection to the configured server. A hook that hardcodes "udp"
// returns a datagram socket here instead, because a UDP "dial" to a TCP-only
// address succeeds without anything on the other end agreeing to it.
func TestProbeAtDialHonorsNetwork(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()
	// The observation crosses back over a channel; the goroutine asserts
	// nothing itself. Buffered, so the send after ln.Close() cannot leak it.
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// The address argument is what the resolver's own wiring chose; the hook
	// exists to ignore it, so handing it a nameserver nothing here runs proves
	// the redirect as well as the network.
	conn, err := dialProbeServer(ln.Addr().String())(ctx, "tcp", "192.0.2.53:53")
	if err != nil {
		t.Fatalf("the dial hook could not dial %s over tcp: %v", ln.Addr(), err)
	}
	defer conn.Close()
	// The type first: under the old hardcoded "udp" this is a *net.UDPConn,
	// and failing here keeps the receive below from waiting on an accept that
	// will never come.
	if _, ok := conn.(*net.TCPConn); !ok {
		t.Fatalf("asked for tcp, the hook returned a %T", conn)
	}
	c := <-accepted
	if c == nil {
		t.Fatalf("the listener accepted nothing; the hook's connection went elsewhere")
	}
	defer c.Close()
	if got, want := c.RemoteAddr().String(), conn.LocalAddr().String(); got != want {
		t.Errorf("the accepted connection is from %s, not from the hook's socket %s", got, want)
	}
	if got, want := conn.RemoteAddr().String(), ln.Addr().String(); got != want {
		t.Errorf("the hook dialed %s, not the configured server %s", got, want)
	}
}

// TestProbeSystemSeesWhatTheOSSees is the parsing control for the dscacheutil
// seam: localhost is in every macOS hosts database, so a probe that cannot
// see it is a probe whose every *.internal measurement would read as
// NXDOMAIN — turning the check into the wrong-answer machine it exists to catch.
//
// It asks for a loopback address among the answers rather than for
// [ProbeResult.ThroughListener], which wants them all. That predicate is
// right about *.internal, where a non-loopback answer means some other
// resolver got there first — and wrong about localhost, whose extra answers
// come from the hosts file: the legacy `fe80::1%lo0 localhost` line shipped
// in older macOS and survives an upgrade, so dscacheutil hands back a
// link-local address (rendered `fe80::1%lo0` or `fe80:1::1`, neither of
// which is loopback) beside ::1 and 127.0.0.1. Demanding all-loopback here
// would fail this control on those hosts with nothing whatsoever wrong,
// which is the opposite of what a control is for.
func TestProbeSystemSeesWhatTheOSSees(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skipf("ProbeSystem is macOS-only for now (the /etc/resolver mechanism; ADR 002)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	r := ProbeSystem(ctx, "localhost")
	if r.Err != "" {
		t.Fatalf("ProbeSystem(localhost) failed: %s", r.Err)
	}
	loopback := false
	for _, a := range r.Addrs {
		if ip, err := netip.ParseAddr(a); err == nil && ip.IsLoopback() {
			loopback = true
			break
		}
	}
	if !loopback {
		t.Fatalf("ProbeSystem(localhost) = %v, want a loopback address among them", r.Addrs)
	}
}

// TestResolverTrapFromCGODisabledBinary is #6's pin: build a binary with
// CGO_ENABLED=0, resolve *.internal from it, and hold what happens against
// what the host's /etc/resolver state says should happen.
//
// Which of the three rows runs is the host's to decide, not the test's: the
// two installed rows need /etc/resolver/internal, which needs sudo, so an
// unattended run exercises only the not-installed row — and says so, rather
// than reporting a pass that covered one row as if it covered three. What
// every host gets unconditionally is the positive control: the CGO_ENABLED=0
// binary resolving *.internal through a live listener it was pointed at.
func TestResolverTrapFromCGODisabledBinary(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skipf("the mechanism under test is macOS's /etc/resolver (ADR 002); %s resolves nothing through it", runtime.GOOS)
	}

	bin := buildProbe(t)
	installed, err := ResolverInstalled()
	if err != nil {
		t.Fatalf("ResolverInstalled: %v", err)
	}

	const name = "voya-dns-check-probe.internal"

	control := serveWildcard(t)
	answering := false
	if installed {
		answering = ensureAnswering(t, resolverFileTarget(t), name)
	}

	m := runProbe(t, bin, nil, name, control)
	if !m["direct"].ThroughListener() {
		t.Fatalf("the CGO_ENABLED=0 probe could not resolve %q against the live listener on %s: %+v — no negative below would mean anything", name, control, m["direct"])
	}

	// buildProbe proved the build setting, so this is a measurement and not
	// an assumption about how the child was compiled. OSReadsResolverFile is
	// true by the skip above: the test runs on darwin alone.
	probes := Probes{ResolverInstalled: installed, OSReadsResolverFile: true, DefaultCGODisabled: true,
		System: m["system"], Default: m["default"], PureGo: m["purego"]}
	report := ClassifyDNS(probes)
	t.Logf("host: resolver installed=%v; the check would report %v:\n%s", installed, report.State, report.Detail)

	if !installed {
		if report.State != DNSResolverMissing {
			t.Fatalf("no %s, yet the report says %v:\n%s", resolverfile.Path, report.State, report.Detail)
		}
		// #296 on a real host: with every path measured and none answering,
		// absence is a clean verdict. A host where something does answer, or
		// a path went unmeasured, is a different row, pinned in TestClassifyDNS.
		quiet := true
		for _, r := range []ProbeResult{probes.System, probes.Default, probes.PureGo} {
			quiet = quiet && r.Unmeasured == "" && !r.answered()
		}
		if quiet && report.Verdict != VerdictOK {
			t.Errorf("a missing opt-in resolver file with nothing answering is not a problem, yet the verdict is %v:\n%s",
				report.Verdict, report.Detail)
		}
		// The state must actually hold, not merely be plausible: a probe
		// resolving *.internal on a host with no resolver file means the
		// host is not in the row this run claims to exercise.
		for _, path := range []string{"system", "default", "purego"} {
			if m[path].ThroughListener() {
				t.Errorf("%s resolved %q with no %s in place: %+v", path, name, resolverfile.Path, m[path])
			}
		}
		// Only of the row the loop above has just held this host to. classify
		// withdraws that sentence the moment something resolves without the
		// file, because then it is not true — so demanding it unconditionally
		// would fail a host for a report that was right about it, on top of
		// the error the loop already raised.
		//
		// And only where the system path was measured at all. With it
		// unmeasured — a host whose dscacheutil will not run — classify says
		// the honest "could not be probed" instead and withholds this
		// sentence on purpose, which is the row TestClassifyDNS bans it in;
		// demanding it here would be this test making #96's misreading, one
		// level up, on the very hosts the issue is about.
		if m["system"].Unmeasured == "" && !m["system"].ThroughListener() &&
			!strings.Contains(report.Detail, "not the CGO_ENABLED=0 trap") {
			t.Errorf("the not-installed report must distinguish itself from the trap:\n%s", report.Detail)
		}
		t.Logf("row exercised: resolver-not-installed only. The two installed rows need %s (sudo) and run on hosts that have it; TestClassifyDNS pins their language regardless.", resolverfile.Path)
		return
	}

	// Installed, with a wildcard answering where the file points: the row is
	// whichever the measurements say — this toolchain's CGO_ENABLED=0 builds
	// either reach the system resolver or they do not, and pinning which is
	// the point of measuring instead of assuming.
	if !answering {
		// Neither ours nor anyone else's wildcard is reachable where the file
		// points, so the rows below have no premise. Skipping says that; the
		// alternative was a failure blaming the host for setup this test did
		// not manage to do.
		t.Skipf("%s points at a wildcard this test could neither bind nor reach; the installed rows need one answering there", resolverfile.Path)
	}
	if probes.System.Unmeasured != "" {
		// The probe never ran, so there is no measurement to hold the host
		// to — and reading its silence as "the system did not resolve" is
		// exactly #96's misreading, which the assertion below would make in
		// the check's own voice, quoting "not measured (…)" as the answer.
		t.Skipf("the system path produced no measurement (%s); the installed rows need one", probes.System.Unmeasured)
	}
	if !probes.System.ThroughListener() {
		t.Fatalf("%s is installed and a wildcard answers where it points, yet the system did not resolve %q: %s", resolverfile.Path, name, probes.System.summary())
	}
	switch report.State {
	case DNSWorking:
		// The one row whose evidence answers ADR 005's question with a yes:
		// system measured, and the measuring binary proved CGO_ENABLED=0.
		if report.Verdict != VerdictOK {
			t.Errorf("working, measured from a CGO_ENABLED=0 build, yet the verdict is %v:\n%s",
				report.Verdict, report.Detail)
		}
		t.Logf("row exercised: installed + CGO_ENABLED=0 resolving — this toolchain reaches the system resolver without cgo, so the trap is confined to netgo/GODEBUG=netdns=go builds here")
	case DNSTrap:
		if report.Verdict != VerdictProblem {
			t.Errorf("the trap was measured, yet the verdict is %v:\n%s", report.Verdict, report.Detail)
		}
		t.Logf("row exercised: the trap — the system resolves while CGO_ENABLED=0 gets NXDOMAIN")
	default:
		t.Fatalf("installed host with the system resolving classified as %v:\n%s", report.State, report.Detail)
	}

	// The deterministic half of the trap on every installed host: forcing
	// the pure resolver must blind the same binary that just measured above
	// — the system path keeps resolving while this lookup fails, which is
	// the confusion #6 documents. Its own control repeats, because this
	// run is a different resolver configuration entitled to break its own
	// plumbing.
	forced := runProbe(t, bin, []string{"GODEBUG=netdns=go"}, name, control)
	if !forced["direct"].ThroughListener() {
		t.Errorf("the GODEBUG=netdns=go run could not resolve against the live listener either: %+v — its negative below means nothing", forced["direct"])
	} else if forced["default"].ThroughListener() {
		t.Errorf("GODEBUG=netdns=go resolved %q through the listener — the pure resolver is honouring %s, which contradicts the trap ADR 002 documents; re-measure before trusting the check's language", name, resolverfile.Path)
	}
}

// buildProbe builds ./probe with CGO_ENABLED=0 and proves it: the build mode
// is this test's subject, and go build obeys its environment silently.
func buildProbe(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "voya-dns-probe")
	cmd := exec.Command("go", "build", "-o", bin, "./probe")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./probe (CGO_ENABLED=0): %v\n%s", err, out)
	}
	settings, err := exec.Command("go", "version", "-m", bin).Output()
	if err != nil {
		t.Fatalf("go version -m: %v", err)
	}
	if !strings.Contains(string(settings), "build\tCGO_ENABLED=0") {
		t.Fatalf("the probe did not get built with CGO_ENABLED=0; go version -m says:\n%s", settings)
	}
	return bin
}

// runProbe runs the built probe and reads its lines back. env entries win
// over the inherited environment, which is how the netdns=go run is forced.
func runProbe(t *testing.T, bin string, env []string, name, server string) map[string]ProbeResult {
	t.Helper()
	// Room for the probe's own budget — four paths at 30s each, if every one
	// of them times out — so a probe that is merely slow is reported as its
	// own "fail" line rather than as a killed child with no output at all.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, name, server)
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: %v\nstderr: %s", bin, err, stderr.String())
	}

	m := make(map[string]ProbeResult)
	for line := range strings.Lines(string(out)) {
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 3)
		if len(parts) != 3 || parts[2] == "" {
			t.Fatalf("unparseable probe line %q in:\n%s", line, out)
		}
		if _, dup := m[parts[0]]; dup {
			t.Fatalf("two %q lines in:\n%s", parts[0], out)
		}
		switch parts[1] {
		case "ok":
			m[parts[0]] = ProbeResult{Addrs: strings.Split(parts[2], ",")}
		case "fail":
			m[parts[0]] = ProbeResult{Err: parts[2]}
		case "unmeasured":
			m[parts[0]] = ProbeResult{Unmeasured: parts[2]}
		default:
			t.Fatalf("unparseable probe line %q in:\n%s", line, out)
		}
	}
	for _, want := range []string{"system", "default", "purego", "direct"} {
		if _, ok := m[want]; !ok {
			t.Fatalf("the probe printed no %q line; output:\n%s", want, out)
		}
	}
	return m
}

// resolverFileTarget reads where /etc/resolver/internal points, defaulting
// the pieces the way the system does: a nameserver or port that is missing —
// or a port that does not parse — means the default resolver on the default
// DNS port.
//
// A resolver(5) file may name up to three nameservers, and — in that page's
// words — "if there are multiple servers, the resolver library queries them
// in the order listed". This takes the first for that reason: keeping the
// last would send the caller to serve or probe an address the system reaches
// only after the one it actually consults, and the assertion below it would
// then blame the host for a wildcard that is answering fine.
func resolverFileTarget(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(resolverfile.Path)
	if err != nil {
		t.Fatalf("reading %s on a host that has it: %v", resolverfile.Path, err)
	}
	return parseResolverTarget(string(b))
}

// parseResolverTarget is resolverFileTarget's parsing, split out so the
// first-wins rule is provable on hosts that have no such file to read.
func parseResolverTarget(file string) string {
	host, port, inline := "127.0.0.1", "53", ""
	haveHost, havePort := false, false
	for line := range strings.Lines(file) {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if f[0] == "nameserver" && !haveHost {
			host, inline = splitInlinePort(f[1])
			haveHost = true
		}
		if f[0] == "port" && !havePort {
			// An unparseable port value is ignored — the default stands —
			// rather than handed onward for dns.Listen to choke on as
			// "127.0.0.1:nonsense" (#100).
			if _, err := strconv.ParseUint(f[1], 10, 16); err == nil {
				port, havePort = f[1], true
			}
		}
	}
	if inline != "" {
		// resolver(5): a port written into the address "override[s] the
		// default or the port number specified as a value for this keyword",
		// wherever the keyword sits relative to it.
		port = inline
	}
	return net.JoinHostPort(host, port)
}

// splitInlinePort separates the `10.0.0.17.55` form resolver(5) allows on a
// nameserver line, where a trailing dot and port follow the address. An
// address that parses whole has no such suffix — 127.0.0.1 is not 127.0.0 on
// port 1 — so that is tried first, and anything that fits neither shape is
// handed back untouched for JoinHostPort to render as written.
func splitInlinePort(ns string) (host, port string) {
	if _, err := netip.ParseAddr(ns); err == nil {
		return ns, ""
	}
	i := strings.LastIndex(ns, ".")
	if i < 0 {
		return ns, ""
	}
	addr, tail := ns[:i], ns[i+1:]
	if _, err := netip.ParseAddr(addr); err != nil {
		return ns, ""
	}
	if _, err := strconv.ParseUint(tail, 10, 16); err != nil {
		return ns, ""
	}
	return addr, tail
}

func TestParseResolverTarget(t *testing.T) {
	cases := []struct {
		name string
		file string
		want string
	}{
		{"empty", "", "127.0.0.1:53"},
		// 9778 is dns_port; the file names the DNS listener, never 9779,
		// which is where registration answers (ADR 002, base.pxf).
		{"the file voya writes", "domain internal\nnameserver 127.0.0.1\nport 9778\n", "127.0.0.1:9778"},
		{"no port", "nameserver 127.0.0.1\n", "127.0.0.1:53"},
		// resolver(5) allows up to three, queried in the order listed, so the
		// first is the one the system's answer comes from.
		{"two nameservers", "nameserver 127.0.0.1\nport 9778\nnameserver 127.0.0.2\n", "127.0.0.1:9778"},
		{"two ports", "nameserver 127.0.0.1\nport 9778\nport 5353\n", "127.0.0.1:9778"},
		{"keywords that carry no value", "nameserver\nport\nnameserver 127.0.0.9\n", "127.0.0.9:53"},
		// #100: an unparseable port value is ignored rather than adopted —
		// and being ignored, it does not shadow a later line that parses.
		{"an unparseable port", "nameserver 127.0.0.1\nport nonsense\n", "127.0.0.1:53"},
		{"an unparseable port does not shadow a real one", "nameserver 127.0.0.1\nport nonsense\nport 9778\n", "127.0.0.1:9778"},
		{"a port out of range", "nameserver 127.0.0.1\nport 99999\n", "127.0.0.1:53"},
		// resolver(5)'s other way to write the port, and its precedence: the
		// address carries it, and it wins over the keyword either side of it.
		{"an inline port", "nameserver 127.0.0.1.9778\n", "127.0.0.1:9778"},
		{"an inline port beats the keyword", "port 53\nnameserver 127.0.0.1.9778\n", "127.0.0.1:9778"},
		{"a v6 nameserver", "nameserver ::1\nport 9778\n", "[::1]:9778"},
		{"something that is neither shape", "nameserver not.an.address\n", "not.an.address:53"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseResolverTarget(tc.file); got != tc.want {
				t.Fatalf("parseResolverTarget(%q) = %s, want %s", tc.file, got, tc.want)
			}
		})
	}
}

// ensureAnswering makes sure a wildcard answers on addr for the duration of
// the test — ours if the port is free, whatever already holds it (a running
// voya) if not — and reports whether it managed to.
//
// Whether it managed to is the return value rather than a log line, because
// the caller's next assertion says "a wildcard answers where the file
// points": binding failures are ordinary here (port 53 needs root, and the
// port may be held by something that is not voya at all), so the assertion
// has to know whether that premise holds instead of asserting it.
func ensureAnswering(t *testing.T, addr, name string) bool {
	t.Helper()

	ln, err := dns.Listen(context.Background(), addr)
	if err == nil {
		srv := dns.NewServer(ln, nil)
		served := make(chan error, 1)
		go func() { served <- srv.Serve() }()
		t.Cleanup(func() {
			_ = srv.Close()
			if err := <-served; err != nil {
				t.Errorf("Serve on %s: %v", addr, err)
			}
		})
		t.Logf("serving the wildcard on %s for this test", addr)
		return true
	}

	// Something holds the port. Ask it directly rather than assume it is a
	// voya answering *.internal.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	held := ProbeAt(ctx, addr, name)
	if !held.ThroughListener() {
		t.Logf("could not bind %s (%v) and whatever holds it does not resolve %q through a wildcard: %s", addr, err, name, held.summary())
		return false
	}
	t.Logf("not binding %s (%v) — but a wildcard there resolves %q to %v", addr, err, name, held.Addrs)
	return true
}
