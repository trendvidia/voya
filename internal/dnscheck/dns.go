// Package dnscheck holds the check behind `voya dns check`: an active test of
// ADR 002's resolver trap, never a report of configuration (ADR 005).
// [ClassifyDNS] is a classifier over evidence the command gathers, so every
// row is testable without the thing it measures, built with a test behind it
// because a check written without one would be written to test the wrong
// thing (#6).
//
// The trap: on macOS, /etc/resolver/internal is honoured by the system's
// resolution machinery, which a Go binary may or may not reach depending on
// how it was built — and which path a build lands on has changed across Go
// releases. So nothing here reasons from the toolchain. The probes resolve a
// *.internal name three ways and [ClassifyDNS] speaks only from what they
// measured.
package dnscheck

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/trendvidia/voya/internal/resolverfile"
)

// ResolverInstalled reports whether [resolverfile.Path] exists. The error is
// a stat failure that is neither presence nor absence; it is surfaced rather
// than folded into false because "unreadable" misreported as "missing" would
// send a developer to write a file that is already there. The path itself,
// and which OS reads it, live in [resolverfile] since `voya dns install`
// started writing the file (#156): one home for both, shared with the
// command.
func ResolverInstalled() (bool, error) {
	_, err := os.Stat(resolverfile.Path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// resolverReload is the command that makes a written resolver file live on
// this host, and "" where writing it is enough — [resolverfile.Wiring.
// ReloadCommand]'s answer for the machine the check is running on, read once,
// like [resolverfile.Path] beside it. A var so a test can give the classifier
// either machine's answer from either machine (#100's seam, same reason).
//
// It is a remedy rather than a state: the check measures, and a file that is
// installed while the system does not resolve through it is the one row where
// "the service that reads it has not read it yet" is the likeliest cause on a
// host whose wiring is a service. On macOS configd picks the file up as it
// lands, so there is nothing to say and nothing is said.
var resolverReload = resolverfile.Here().ReloadCommand()

// reloadRemedy is that command as a sentence, or "" — the clause appended to
// the "installed, but the system did not resolve through it" remedy, which is
// otherwise macOS-shaped and would send a Linux developer to check voya and
// the port when what is wrong is that systemd-resolved has not restarted.
//
// It ends by saying what a restart that changes nothing means, because on this
// state's other Linux cause it is the whole answer: a host running
// systemd-resolved whose own resolution does not go through it — a static
// /etc/resolv.conf, dnsmasq or resolvconf in front — reads voya's drop-in with
// nothing ever asking resolved, and no number of restarts moves it. A remedy
// that stopped at the restart would be a remedy for the wrong host, told as
// certainty ("is not live yet") to a developer whose file is live and unasked.
//
// It names `voya dns install --activate` beside the systemctl it stands for
// (#177): a plain `voya dns install` on this host finds the file already
// right and restarts nothing, and the developer reading this remedy is
// exactly the one whose file is right and unread.
func reloadRemedy(p Probes) string {
	if resolverReload == "" {
		return ""
	}
	remedy := " This host reads that file only when the service that reads it starts, so one written or " +
		"changed since then is not live yet: `sudo voya dns install --activate` (or `sudo " + resolverReload +
		"`) restarts it whatever the file's state."
	// The bypass as a possibility, only where it is one (#253): #204's
	// measurement is what would say, and a host it measured resolving
	// through resolved has had that measurement find neither a static
	// /etc/resolv.conf nor a forwarder ahead of it — a host it finds either
	// on takes its own row above this one — so supposing one here would be
	// a finding off a measurement that said the opposite, the way
	// [staleCacheRemedy]'s clause was.
	if p.ServiceBypassedUnmeasured != "" {
		remedy += " If that changes nothing, " +
			"this host's own resolution may not go through that service at all — a static /etc/resolv.conf, " +
			"or dnsmasq or resolvconf ahead of it — and then *.internal needs wiring through whatever it " +
			"does resolve with."
	}
	return remedy
}

// staleCacheRemedy is the second cause the not-answering row owes a host
// that caches (#220): landing the file changes where new lookups go, not
// what a cache already holds, so a negative answer cached before the file
// landed — any *.internal lookup made between a `voya dns uninstall` and the
// next install plants one — is served past the new file while voya answers
// perfectly well, and a remedy that stops at "Is voya running?" sends that
// developer to check the one cause that is not it.
//
// Which cache depends on the host, on [resolverReload]'s split. Where the
// wiring is a service, the restart [reloadRemedy] already prescribes empties
// systemd-resolved's own cache on the way, so no flush is named for it — but
// a dnsmasq or resolvconf in front of resolved, the host that remedy's last
// sentence is written for, holds a cache of its own that survives that
// restart, so its survival is named, [frontResolverCache] (#243). Only where
// the bypass went unmeasured, though (#253): #204's measurement is what would
// say whether anything is in front of resolved, and it cannot tell a
// forwarder from a static /etc/resolv.conf, so the host the clause is
// written for is measured bypassed and takes its own row above this one —
// where the clause is appended beside [notThroughServiceForwarder], the
// sentence that names that host — while a host measured resolving through
// resolved has had the same measurement find nothing in front, and telling
// it about a resolver nothing found would be a finding off a measurement
// that said the opposite. Unmeasured, the supposition is fair, and the
// clause is said the way [reloadRemedy]'s last sentence says it: as a
// condition, not a finding.
//
// The dial [Probes.AtFilePort] carries gates both hosts (#243). Measured not
// answering, [filePortAnswer] has just said nothing that is voya is there,
// and no cache on either host explains a port with nothing behind it, so
// nothing is appended — a flush or a front-resolver restart would point away
// from the measurement. On the host whose file is live as it lands the rest
// follows the dial too: measured answering, the cache is mDNSResponder's and
// is named as the likeliest cause outright; unmeasured, the pre-#243 prose,
// conditional on the `dig` it asks the developer to run.
//
// Gated on silence throughout, and for the same reason it exists: where the
// system answered, the row has just named another resolver, VPN or search
// domain claiming *.internal as the cause, and appending "the cause is a
// stale cache instead" would state two exclusive causes as certainties in
// one paragraph. The cached *positive* answer that branch owes instead is
// [cachedAnswerHint]'s hedged sentence (#243).
func staleCacheRemedy(p Probes) string {
	if p.System.answered() {
		return ""
	}
	// The dial settles it for both hosts: a port measured with nothing that
	// is voya behind it is not a port a cache is standing in front of, and
	// either clause would send the developer after a cache while
	// [filePortAnswer] has just told them to start voya.
	if p.AtFilePort.measured() && !p.AtFilePort.ThroughListener() {
		return ""
	}
	if resolverReload != "" {
		if p.ServiceBypassedUnmeasured != "" {
			return frontResolverCache(p)
		}
		return ""
	}
	switch {
	case p.AtFilePort.ThroughListener():
		return " The likeliest cause is a stale cache: an answer cached before this file landed, " +
			"which any *.internal lookup made between a `voya dns uninstall` and the next install " +
			"plants, is served past the file until `sudo dscacheutil -flushcache; sudo killall " +
			"-HUP mDNSResponder` clears it."
	default:
		return " If it is running and answering on the port the file names — `dig @127.0.0.1 -p " +
			"<the file's port> <name>.internal` asks that server directly — the cause is a stale " +
			"cache instead: an answer cached before this file landed, which any *.internal lookup " +
			"made between a `voya dns uninstall` and the next install plants, is served past the " +
			"file until `sudo dscacheutil -flushcache; sudo killall -HUP mDNSResponder` clears it."
	}
}

// frontResolverCache is the clause a dnsmasq or resolvconf in front of
// systemd-resolved owes the not-answering row and the bypass row alike (#243,
// #253): the restart the sentence before it prescribes empties only
// resolved's own cache, and a negative answer the front resolver cached
// before the file landed survives it. Written to follow a sentence that has
// just named that host as a condition — [reloadRemedy]'s last sentence on
// the not-answering row, [notThroughServiceForwarder] on the bypass row —
// so it names the host the same way. Which resolver is in front is not
// measured, so no flush command is named: the clearing named is restarting
// whatever is in front.
//
// Gated the way [staleCacheRemedy] is, and for its reasons: said only where
// the wiring is a service with a restart to name, only where the system was
// silent (an answer has just been blamed on a competitor, and a cached
// negative answer cannot be the same lookup's cause), and not where the
// dial measured the file's port with nothing that is voya behind it — the
// row has just said to start voya, and a cache clause would point away
// from that.
func frontResolverCache(p Probes) string {
	if resolverReload == "" || p.System.answered() ||
		(p.AtFilePort.measured() && !p.AtFilePort.ThroughListener()) {
		return ""
	}
	return " If a dnsmasq or resolvconf is in front of resolved — the host the sentence above " +
		"names — its cache is its own besides, and a negative answer it cached before this " +
		"file landed survives that restart, which empties only resolved's own cache: " +
		"clearing it means restarting whatever is in front, and which resolver that is, this " +
		"run did not measure."
}

// cachedAnswerHint is what the answered branch of the not-answering row owes
// the host whose file is live as it lands (#243): mDNSResponder can serve a
// cached non-loopback answer from before the file landed, which from here
// reads identically to a competing resolver, so the competitor the row has
// just named is one reading of the measurement rather than the measurement.
// The cached answer is named as the alternative, neither stated as certain,
// with the flush as what settles it. Only where [resolverReload] is empty:
// on the host whose wiring is a service, the restart the same paragraph
// already prescribes empties that service's cache on the way, and macOS's
// flush commands would be a second, wrong command beside it.
func cachedAnswerHint() string {
	if resolverReload != "" {
		return ""
	}
	return "Or nothing is claiming it at all: mDNSResponder can serve an answer it cached before " +
		"the file landed, and from here the two read identically. `sudo dscacheutil -flushcache; " +
		"sudo killall -HUP mDNSResponder` clears the cache and settles it — an answer that " +
		"survives the flush is a live resolver's. "
}

// voyaUpNote is the last sentence of the three rows whose cause is neither
// the file nor voya — the catch-all (#185), the bypass (#204) and the
// too-old resolved (#206). Each ends by refusing to exonerate a process it
// never looked at, and until #243 none of them had: nothing in [Probes]
// measured whether voya was up. The file-port dial does, and it is made
// wherever the file names a port, whichever row then wins, so the sentence
// says what was measured where there is a measurement and keeps its
// refusal — verbatim, it is what these rows have always said — where the
// zero value or an unmeasured reason means no dial was made (#104).
//
// whyUnreached is the row's own reason the lookup does not arrive, because
// that is the half the dial does not change: voya answering its own port
// says nothing about a route that never reaches it.
func voyaUpNote(p Probes, whyUnreached string) string {
	switch {
	case p.AtFilePort.ThroughListener():
		return " voya itself is up, at least: this run asked 127.0.0.1 on the port the file names " +
			"directly and voya's loopback came back — but " + whyUnreached + ", so that is not the " +
			"thing to fix here."
	case p.AtFilePort.measured():
		return " voya is worth starting besides: this run asked 127.0.0.1 on the port the file " +
			"names directly and got " + p.AtFilePort.summary() + ", so nothing that is voya is " +
			"answering there either — though " + whyUnreached + " until the wiring above is fixed."
	default:
		return " Whether voya itself is up is a question this row did not measure: " + whyUnreached + "."
	}
}

// filePortAnswer is the not-answering row's word on voya itself. The row's
// question — is voya running, on the port the file names? — is one the check
// can settle by dialling that port directly, and since #243
// [Probes.AtFilePort] carries that dial where the caller made one, so the
// row states what was measured instead of sending the developer off to
// measure it. Where no dial was made — no port to dial, or a caller that
// never dialled, whose zero value must not read as voya measured silent
// (#104) — the question is asked as prose, the pre-#243 row, and
// [staleCacheRemedy] keeps its conditional `dig` clause.
func filePortAnswer(p Probes) string {
	switch {
	case p.AtFilePort.ThroughListener():
		return "voya is answering on the port the file names — this run asked 127.0.0.1 on that " +
			"port directly, and voya's loopback came back — so voya is running and the file is " +
			"right: the break is between the system's resolution path and that port."
	case p.AtFilePort.measured():
		return "Nothing that is voya is answering on the port the file names either — this run " +
			"asked 127.0.0.1 on that port directly and got: " + p.AtFilePort.summary() + ". " +
			"Start voya, or " +
			"point it back at that port: `voya run` names its DNS listener in the startup banner; " +
			"the file must name that port, and changing dns_port means rewriting the file."
	default:
		return "Is voya running? `voya run` names its DNS listener in the startup banner; the " +
			"file must name that port, and changing dns_port means rewriting the file."
	}
}

// catchAllCause is the "installed, but the system did not resolve through
// voya" row's detail on a host whose resolver already routes every name from
// its global scope (#185): the cause is the catch-all, so the remedy is to
// remove it or route around it, and the file's health is not in question.
// voya's own is not the cause either way: the routing is what stops the
// lookup arriving, however well voya is running, so this row says that and
// leaves what the file-port dial found to [voyaUpNote] (#243). `voya dns
// install` refuses to write the file on such a host, so the one the check meets
// was written by hand or by an older voya.
func catchAllCause(p Probes) string {
	detail := "The file is not the cause, and neither is a restart of the service that read it: " +
		"systemd-resolved's global scope already routes every name (`Domains=" +
		resolverfile.CatchAllDomain + "` in resolved.conf or one of its drop-ins, usually a " +
		"pinned-resolver or VPN policy), and DNS= and Domains= are separate lists there, so the " +
		"server the file names joined a flat global list instead of binding to *.internal, and the " +
		"catch-all sends every *.internal lookup to whichever global server answers first — a lookup " +
		"that never reaches voya, however well it is running. "
	if p.System.answered() {
		// Which server produced that address is not measured — the catch-all
		// is where this name's route ends up, but /etc/hosts, an nss module
		// or a container's own resolver can answer ahead of resolved — so the
		// address is reported and its author is not named.
		detail += "Something did answer here, with an address that is not loopback (" +
			p.System.summary() + "), which is where that route leads. "
	} else {
		detail += "Here nothing answered for the name at all. "
	}
	return detail + catchAllWaysOut() +
		voyaUpNote(p, "the catch-all keeps the lookup from arriving either way")
}

// catchAllWaysOut is the three ways past a global catch-all, shared by the two
// rows that meet one (#185): the file installed and never reached, and the
// file missing on a host [resolverfile.Wiring.Preflight] will refuse to write
// it on.
func catchAllWaysOut() string {
	return "Drop the catch-all, or send *.internal to voya through something that can " +
		"scope a server to a domain (dnsmasq: `server=/internal/127.0.0.1#<dns_port>`), or dial " +
		"through the voya client, which needs none of this."
}

// catchAllRefusal is what the file-missing row owes a host the check has just
// measured carrying a global catch-all (#185): that row ends in `sudo voya dns
// install`, and [resolverfile.Wiring.Preflight] refuses to write on exactly
// this host, because the file would be inert. The refusal explains itself when
// it arrives, so what this saves is a round trip — but the check already has the
// measurement, and a remedy it knows will be refused is not one.
func catchAllRefusal(p Probes) string {
	if p.GlobalCatchAllUnmeasured != "" || !p.GlobalCatchAll {
		return ""
	}
	return " That command will refuse here, though: systemd-resolved's global scope already routes " +
		"every name (`Domains=" + resolverfile.CatchAllDomain + "`), so the file would be inert — " +
		"DNS= and Domains= are separate lists there, so voya's server would join the global list " +
		"rather than binding to *.internal, and the catch-all would send every *.internal lookup to " +
		"whichever global server answers first. " + catchAllWaysOut()
}

// notThroughServiceCause is the "installed, but the system did not resolve
// through voya" row's detail on a host measured running the service that
// reads the file while its own resolution never reaches that service (#204):
// the file was read, nothing asks its reader, and no restart moves that. The
// remedy is the one [resolverfile.Wiring.Preflight] gives when it refuses to
// write the file on such a host — point the system at the service, or wire
// *.internal through what it does resolve with — and it names no `voya dns
// install`, because that command is refused here and the check knows it.
func notThroughServiceCause(p Probes) string {
	detail := "The file is not the cause: this host's own name resolution does not go through " +
		"systemd-resolved at all (" + p.ServiceBypassedWhy + "), so resolved read voya's drop-in " +
		"and nothing ever asks resolved — every *.internal lookup goes to whatever this host does " +
		"resolve with, however well voya is running, and the restart the plain remedy prescribes " +
		"moves none of that. "
	if p.System.answered() {
		// Which resolver produced that address is not measured — the same
		// reason [catchAllCause] gives for not naming one: /etc/hosts, an
		// nss module or a container's own resolver can answer ahead of
		// whatever this host resolves through — so the address is reported
		// and its author is not named.
		detail += "Something did answer here, with an address that is not loopback (" +
			p.System.summary() + "), which is where the lookup went instead. "
	} else {
		detail += "Here nothing answered for the name at all. "
	}
	return detail + notThroughServiceWaysOut() + notThroughServiceForwarder() +
		frontResolverCache(p) + voyaUpNote(p, "the lookup never reaches it either way")
}

// notThroughServiceForwarder is the one host the measurement behind this row
// cannot tell from a bypass: a local forwarder — dnsmasq or unbound with
// resolved's stub upstream — named in /etc/resolv.conf reads exactly like a
// static file, while the lookup reaches resolved after all. #186 took that
// cost deliberately for the refusal, and the check inherits the measurement, so
// the restart this row otherwise withholds is named as the condition it is
// rather than dropped or prescribed: on that host it is the whole fix, and
// stating "no restart moves this" as certainty would be the kind of claim
// this row exists to stop making. Said only where the wiring is a service
// with a restart to name — [resolverReload] is that host. The restart it
// names empties only resolved's own cache, and that host's front resolver
// keeps one of its own, so [frontResolverCache] follows it (#253).
func notThroughServiceForwarder() string {
	if resolverReload == "" {
		return ""
	}
	return " What was read to say that is two files, though, and a local forwarder that chains " +
		"onto resolved — dnsmasq or unbound pointed at resolved's stub — reads exactly like one " +
		"that does not: if that is this host, resolved is asked after all, and `sudo " +
		resolverReload + "` is what makes the drop-in it read live."
}

// notThroughServiceWaysOut is the three ways off a host that runs resolved
// without resolving through it, shared by the two rows that meet one (#204):
// the file installed and never asked for, and the file missing on a host
// [resolverfile.Wiring.Preflight] will refuse to write it on. The first
// makes the file worth having; the other two do without it.
func notThroughServiceWaysOut() string {
	return "Point the system at resolved — `ln -sf " + resolverfile.StubResolvConf + " " +
		"/etc/resolv.conf`, or NetworkManager's `dns=systemd-resolved` — or send *.internal to " +
		"voya's DNS listener through whatever this host does resolve with (dnsmasq: " +
		"`server=/internal/127.0.0.1#<dns_port>`), or dial through the voya client, which needs " +
		"none of this."
}

// installRefusal is what the file-missing row owes a host the check has just
// measured as one [resolverfile.Wiring.Preflight] refuses: that row ends in
// `sudo voya dns install`, and Preflight refuses on a host whose resolved is
// too old to be told a port (#206), on one that does not resolve through
// resolved (#204) and on one whose global scope routes every name (#185), in
// that order. The refusal explains itself when it arrives, so what this saves
// is a round trip — but the check already has the measurement, and a remedy it
// knows will be refused is not one. Only the first refusal Preflight would
// give is spelled out, since it is the one the developer would meet — and
// where the version, the question asked first, went unread, the prediction
// says so rather than claiming a place in line it did not measure.
//
// The way out of the bypass is not a promise that the install then writes
// the file, though, unless the catch-all was measured and answered no: a
// host carrying both is refused twice, and sending a developer to point the
// system at resolved and try again — the round trip this function exists to
// save — is what "the install writes it" would do there. So the catch-all is
// named ahead of the second refusal where it was measured, named as unread
// where it was not, and only a measured no — to it and to the version — earns
// the promise. The version refusal makes no such promise to guard: its way
// out is around the file, not through it, and needs neither of the other two.
func installRefusal(p Probes) string {
	if p.ServiceTooOldUnmeasured == "" && p.ServiceTooOld {
		return tooOldRefusal(p)
	}
	if p.ServiceBypassedUnmeasured != "" || !p.ServiceBypassed {
		return catchAllRefusal(p) + earlierUnread(p, p.GlobalCatchAllUnmeasured == "" && p.GlobalCatchAll)
	}
	refusal := " That command will refuse here, though: this host's own name resolution does not go " +
		"through systemd-resolved (" + p.ServiceBypassedWhy + "), so resolved would read " +
		"the file and nothing would ever ask resolved. " + notThroughServiceWaysOut()
	switch {
	case p.GlobalCatchAllUnmeasured == "" && p.GlobalCatchAll:
		refusal += " Pointing the system at resolved is not the whole of it here: " +
			"systemd-resolved's global scope already routes every name (`Domains=" +
			resolverfile.CatchAllDomain + "`), which the same command refuses on next and which " +
			"would leave the file inert anyway, so that has to go too — the other two ways out " +
			"need neither."
	case p.GlobalCatchAllUnmeasured != "":
		refusal += " With the system pointed at resolved the command has one more question " +
			"to put — whether systemd-resolved's global scope already routes every name " +
			"(`Domains=" + resolverfile.CatchAllDomain + "`), which it refuses on too — and that " +
			"one could not be read here (" + p.GlobalCatchAllUnmeasured + "): `resolvectl domain` " +
			"shows it."
	case p.ServiceTooOldUnmeasured == "":
		// The promise is earned only here: every question the command
		// asks was measured, and each answered no.
		refusal += " With the system pointed at resolved, `sudo voya dns install` writes the file."
	}
	return refusal + earlierUnread(p, true)
}

// tooOldRefusal is the refusal a host running a resolved older than
// [resolverfile]'s minimum meets first (#206), in the refusal's own words,
// with the way out the refusal names: around the file, since no restart,
// re-pointing or scope change makes this resolved read a port. Measured at a
// real v245 by the refusal itself; the check repeats what it will say.
func tooOldRefusal(p Probes) string {
	return " That command will refuse here, though: " + p.ServiceTooOldWhy + ", and voya will not " +
		"write a line this resolved rejects outright — it logs `Failed to add DNS server address …, " +
		"ignoring: Invalid argument` and configures no server at all. " + tooOldWaysOut()
}

// tooOldWaysOut is the two ways past a resolved too old to be told a port,
// shared by the two rows that meet one (#206): the file installed and its
// one line ignored, and the file missing on a host the install refuses.
// Neither goes through the file.
func tooOldWaysOut() string {
	return "Send *.internal to voya's DNS listener through something that takes a port " +
		"(dnsmasq: `server=/internal/127.0.0.1#<dns_port>`), or dial through the voya client, " +
		"which needs none of this."
}

// earlierUnread is what a predicted refusal owes a host where a question the
// command puts *ahead* of the predicted one went unread: resolved's version,
// which it asks before every other (#206), and — where the catch-all is what
// was predicted — whether this host resolves through resolved at all, which
// it asks before the scope (#204). It refuses on a question it cannot read as
// well as on a bad answer to one, so with either of those unread "the refusal
// the developer will meet" is a claim the check cannot make, and it says so
// instead of claiming a place in line it did not measure. Said only where a
// refusal was predicted, since where none was, the plain remedy already
// promises no place in line — and said of the earlier questions only, since a
// question asked after the prediction cannot come first, and [installRefusal]
// names the catch-all itself where that is the one that went unread.
func earlierUnread(p Probes, predicted bool) string {
	if !predicted {
		return ""
	}
	var ahead []string
	if p.ServiceTooOldUnmeasured != "" {
		ahead = append(ahead, "It asks resolved's version before every other question, refusing "+
			"on one too old to be told a port or on one it cannot read, and that could not be "+
			"read here ("+p.ServiceTooOldUnmeasured+"): `resolvectl --version` shows it.")
	}
	if p.ServiceBypassedUnmeasured != "" {
		ahead = append(ahead, "It asks whether this host's own name resolution goes through "+
			"systemd-resolved before it asks about the global scope, refusing on a host where it "+
			"does not, and that could not be read here ("+p.ServiceBypassedUnmeasured+"): "+
			"/etc/resolv.conf and the `hosts:` line of /etc/nsswitch.conf are what answer it.")
	}
	if len(ahead) == 0 {
		return ""
	}
	return " Whether that is the first refusal here is not measured, though. " + strings.Join(ahead, " ")
}

// tooOldCause is the "installed, but the system did not resolve through
// voya" row's detail on a host measured running a resolved older than
// [resolverfile]'s minimum (#206): such a resolved reads the file, rejects
// the one line that names voya — `DNS=` with a port — and configures no
// server from it, so nothing reaches voya however well it is running, and
// the restart the plain remedy prescribes moves none of that. Named ahead of
// the bypass and the catch-all as [resolverfile.Wiring.Preflight] refuses on
// it ahead of both, and because its way out needs neither of theirs.
func tooOldCause(p Probes) string {
	detail := "The file is not the cause, and neither is voya: " + p.ServiceTooOldWhy + ", so resolved " +
		"read voya's drop-in, rejected the one line that names voya — it logs `Failed to add DNS " +
		"server address …, ignoring: Invalid argument` — and configured no server from it. No " +
		"*.internal lookup reaches voya however well it is running, and the restart the plain " +
		"remedy prescribes moves none of that. "
	if p.System.answered() {
		// As on the other two rows: which resolver produced the address is
		// not measured, so it is reported and its author is not named.
		detail += "Something did answer here, with an address that is not loopback (" +
			p.System.summary() + "), which is where the lookup went instead. "
	} else {
		detail += "Here nothing answered for the name at all. "
	}
	return detail + tooOldWaysOut() + tooOldFileUnread() +
		voyaUpNote(p, "the lookup never reaches it either way")
}

// tooOldFileUnread is what [tooOldCause]'s one content-dependent claim owes
// the row it is said on: what was measured is that the file exists, never
// what it says, and the line this resolved rejects is the port in it. Every
// file voya writes names one — the version gate is about exactly that — but a
// file hand-written or left by an older voya may not, and there this resolved
// read the file happily and the version stopped nothing. The two questions
// this row returns ahead of are then the ones to put, so it names them rather
// than leaving the reader with a cause it has just undercut.
func tooOldFileUnread() string {
	return " What was read to say that is a stat, though: this row establishes that the file is " +
		"there, not what it says. Every file voya writes names a port — the version gate is about " +
		"exactly that — but one hand-written as `DNS=127.0.0.1` alone is a line even this resolved " +
		"takes, and if that is this file the version stopped nothing: what to ask then is whether " +
		"this host's own resolution reaches resolved at all, and whether resolved's global scope " +
		"already routes every name."
}

// tooOldUnread is what the not-answering row's plain remedy owes a host whose
// resolved version went unasked (#104's shape once more, #206): a resolved
// too old to be told a port read the file, dropped the one line that names
// voya and configured no server from it, so the restart the remedy prescribes
// moves nothing against that cause and the `--activate` it names is refused
// for it — ahead of every other question this row turns on. Gated on
// [resolverReload] like [catchAllUnread] and for its reason: where nothing
// reads the file there is no service whose version could have gone unread.
func tooOldUnread(p Probes) string {
	if p.ServiceTooOldUnmeasured == "" || resolverReload == "" {
		return ""
	}
	return " Whether the systemd-resolved that read the file is new enough to be told a port in " +
		"`DNS=` could not be read (" + p.ServiceTooOldUnmeasured + "): `resolvectl --version` " +
		"shows it, and one that is not is the cause instead — it dropped the line naming voya as " +
		"it read the file, so the restart above moves none of that and the command it names is " +
		"refused on the version before it is refused on anything else."
}

// catchAllUnread is what the not-answering row's plain remedy owes a host
// whose global routing went unasked (#104's shape again, #185): a `Domains=~.`
// there is a cause this row cannot see, and the remedy prescribes a restart
// that would change nothing against it. Said only where the wiring is a
// service that could have been asked — [resolverReload] is that host — since
// where nothing reads the file there is no global scope and so nothing went
// unanswered.
func catchAllUnread(p Probes) string {
	if p.GlobalCatchAllUnmeasured == "" || resolverReload == "" {
		return ""
	}
	return " Whether systemd-resolved's global scope already routes every name (`Domains=" +
		resolverfile.CatchAllDomain + "`) could not be read (" + p.GlobalCatchAllUnmeasured +
		"): `resolvectl domain` shows it, and a catch-all there is the cause instead — none of the " +
		"above applies to one, because the lookup never reaches voya whatever the file says."
}

// ProbeResult is one attempt to resolve a *.internal name.
type ProbeResult struct {
	// Addrs is what resolution returned; empty when it failed.
	Addrs []string
	// Err is why resolution failed, and "" when it succeeded. A string
	// rather than an error because results cross a process boundary: the
	// measurement ADR 005 requires is about a build mode, so the tests run
	// the probes in a child binary built that way and read the results back.
	Err string
	// Unmeasured is why this path was never measured — the probe does not
	// exist on this OS, its tooling could not be run, or the deadline fired
	// before it answered — and "" when it was. Distinct from Err on purpose
	// (#96): Err is a measurement, the machinery ran and the name did not
	// resolve, while reading "the probe could not run" as that same "no"
	// told hosts where voya was answering perfectly well to restart it.
	Unmeasured string
}

// ThroughListener reports whether this probe reached voya's listener: it
// resolved, and everything it returned is loopback — the only answer the
// wildcard gives. A *.internal answer that is not loopback means some other
// resolver invented one, and for routing traffic into voya that is as much a
// failure as NXDOMAIN.
func (r ProbeResult) ThroughListener() bool {
	if r.Err != "" || len(r.Addrs) == 0 {
		return false
	}
	for _, a := range r.Addrs {
		// netip rather than net.ParseIP, which returns nil for a
		// zone-suffixed literal like `::1%lo0` — a form the system path can
		// hand back for a v6 address — and would score voya's own answer as
		// "some other resolver invented one".
		ip, err := netip.ParseAddr(a)
		if err != nil || !ip.IsLoopback() {
			return false
		}
	}
	return true
}

// answered reports whether anything came back at all — as opposed to a
// failure, or the silence of a result carrying neither addresses nor an
// error. "Something is there and it is wrong" and "nothing is there" are
// different faults with different remedies, and a report that quotes an
// empty address list as the wrong answer has given one of its own.
func (r ProbeResult) answered() bool {
	return r.Err == "" && len(r.Addrs) > 0
}

// measured reports whether this result carries a measurement at all:
// something answered, or the machinery ran and failed. The zero value — no
// addresses, no error, no reason it went unmeasured — is a probe nobody
// made, and [Probes.normalized] names it unmeasured before any row reads it
// (#263), so where this is consulted an unmeasured result is never a
// measured silence: #104's rule, a "no" nothing measured must never be acted
// on as a no.
func (r ProbeResult) measured() bool {
	return r.Unmeasured == "" && (r.Err != "" || len(r.Addrs) > 0)
}

// notProbed is the reason [Probes.normalized] gives a [ProbeResult] that
// carries neither a measurement nor a reason it went unmeasured: the zero
// value, which no probe in this package produces — [probeWith] and the
// system probes each set one of the three fields — and which therefore only
// ever means a caller assembled [Probes] without making that probe (#134's
// own way in).
const notProbed = "no probe was made"

// normalized is where the zero-value [ProbeResult] is decided, once, for
// every probe [ClassifyDNS] reads (#263): a result with no addresses, no
// error and no reason is a probe nobody made, and it leaves here carrying
// [notProbed] as its reason so that every row downstream — each of which
// already reads [ProbeResult.Unmeasured] before it reads
// [ProbeResult.ThroughListener] as a "no" (#96, #253, #258, #260) — reports
// it as unmeasured rather than as a measured failure, and so that the
// evidence line every report ends with never prints `=` with nothing after
// it. #104's rule, a "no" nothing measured must never be acted on as a no,
// held at the door rather than row by row: before this the file-port dial
// was the one probe guarded against that silence, and the other three read
// it as NXDOMAIN's twin.
func (p Probes) normalized() Probes {
	for _, r := range []*ProbeResult{&p.System, &p.Default, &p.PureGo, &p.AtFilePort} {
		if r.Unmeasured == "" && !r.measured() {
			r.Unmeasured = notProbed
		}
	}
	return p
}

// summary renders a result for the evidence line every report carries.
func (r ProbeResult) summary() string {
	if r.Unmeasured != "" {
		return "not measured (" + r.Unmeasured + ")"
	}
	if r.Err != "" {
		return r.Err
	}
	return strings.Join(r.Addrs, ",")
}

// CGODisabled reports whether the binary running this code was built with
// cgo disabled, and whether that could be determined at all — a stripped
// binary carries no build settings, and "unknown" must not be reported as
// "cgo enabled" any more than the reverse.
//
// It exists because ADR 005 requires the check's measurement to come from a
// CGO_ENABLED=0 build, and nothing about `go build ./cmd/voya` guarantees
// one: cgo is on by default wherever a C toolchain is, so the flag has to be
// read back from the binary rather than assumed of it.
func CGODisabled() (disabled, known bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return false, false
	}
	for _, s := range info.Settings {
		if s.Key == "CGO_ENABLED" {
			return s.Value == "0", true
		}
	}
	return false, false
}

// Probes is what was measured on one host — by resolving, not by reading
// configuration: checking that /etc/resolver/internal merely exists would
// pass in exactly the case that breaks (ADR 005).
type Probes struct {
	// ResolverInstalled is [ResolverInstalled]'s answer.
	ResolverInstalled bool
	// ResolverUnmeasured is why [resolverfile.Path]'s state was never
	// established — a stat failure that is neither presence nor absence, an
	// EACCES on a parent directory, a wedged mount, or no stat having been
	// made at all off the OS that reads the file — and "" when it was. It
	// mirrors [ProbeResult.Unmeasured] on purpose (#104): ResolverInstalled
	// has no room for a third state, and an unreadable file folded into false
	// would classify as [DNSResolverMissing] — a state that is the file's
	// absence, which was never established. With this set, ResolverInstalled
	// carries no answer and [ClassifyDNS] ignores it.
	ResolverUnmeasured string
	// OSReadsResolverFile is whether anything on this host reads
	// [resolverfile.Path], carried as an input so [ClassifyDNS] never reasons
	// from runtime.GOOS. It decides what the file's presence or absence
	// *means*: where something reads it, a missing file is a conclusive "the
	// DNS path was never installed" whatever else was measured; anywhere else
	// the file is inert, and the remedy must not name it (#96).
	//
	// Two questions, since #155, and the caller — cmd/voya's
	// osReadsResolverFile — asks both: whether this OS has a wiring ADR 002
	// specifies, and whether this machine is running the thing that reads it.
	// macOS answers the second with configd, which is the OS. Linux answers it
	// with systemd-resolved, which a host may not be running at all, and a
	// drop-in on such a host is as inert as an /etc/resolver file on Windows.
	OSReadsResolverFile bool
	// GlobalCatchAll is whether the service that reads [resolverfile.Path]
	// already routes every name from its global scope — systemd-resolved's
	// `Domains=~.` — [resolverfile.Wiring.GlobalCatchAll]'s answer, and a
	// measurement only when GlobalCatchAllUnmeasured is "". It matters on
	// one row (#185): the file installed and the system not resolving
	// through voya. There the catch-all is a cause the file and voya are
	// both innocent of — DNS= and Domains= are separate lists in resolved,
	// so the server the file names joins a flat global list rather than
	// binding to *.internal, and `~.` sends every lookup to whichever global
	// server answers first — and "is voya running? restart resolved" sends
	// the developer to change nothing, twice.
	GlobalCatchAll bool
	// GlobalCatchAllUnmeasured is why that was never established — the OS
	// has no global scope to ask (macOS's files are scoped by name in the
	// mechanism itself), resolvectl could not be run, or the caller never
	// asked — and "" when it was. #104's shape again: unmeasured is not
	// false, and a false nothing measured would let a Linux host whose
	// resolvectl is wedged read as one measured clear of the catch-all.
	// With this set, GlobalCatchAll carries no answer and [ClassifyDNS]
	// gives that row its plain remedy.
	GlobalCatchAllUnmeasured string
	// ServiceBypassed is whether this host's own name resolution —
	// getaddrinfo's path, the one curl, browsers and `getent` take — never
	// reaches the service that reads [resolverfile.Path]: the negation of
	// [resolverfile.Wiring.ResolvesThroughResolved]'s answer, and a
	// measurement only when ServiceBypassedUnmeasured is "". It matters on
	// the same row the catch-all does (#204): the file installed and the
	// system not resolving through voya. A host running systemd-resolved
	// with a static /etc/resolv.conf, or dnsmasq or resolvconf ahead of it,
	// reads voya's drop-in and nothing ever asks resolved, so the plain
	// remedy's restart moves nothing, and the command it names — `sudo voya
	// dns install --activate` — is one [resolverfile.Wiring.Preflight]
	// refuses on exactly this host. The check has the measurement, so it names
	// the cause instead. Carried in this polarity, as GlobalCatchAll is, so
	// that the zero value is the host with nothing to say.
	ServiceBypassed bool
	// ServiceBypassedWhy is, when that is a measured true, what resolverfile
	// found in the two files it read — the clause its refusal would carry —
	// so the check's cause is true of this host's files rather than a list of
	// the shapes that fail. "" otherwise.
	ServiceBypassedWhy string
	// ServiceBypassedUnmeasured is why that was never established — the OS
	// has no service to reach (macOS resolves through configd by
	// construction), a file could not be read, or the caller never asked —
	// and "" when it was. #104's shape once more: with this set,
	// ServiceBypassed carries no answer, and the row falls back to its plain
	// remedy, whose [reloadRemedy] already ends by naming this cause as a
	// possibility.
	ServiceBypassedUnmeasured string
	// ServiceTooOld is whether the service that reads [resolverfile.Path]
	// predates the release that reads a port in its `DNS=` line —
	// [resolverfile.Wiring.ResolvedTooOld]'s answer, and a measurement only
	// when ServiceTooOldUnmeasured is "". It matters on the same rows the
	// other two do (#206): the file installed and the system not resolving
	// through voya, where such a resolved read the file, dropped the one
	// line that names voya and configured no server from it, so the plain
	// remedy's restart moves nothing; and the file missing, where `sudo voya
	// dns install` is refused, and refused for this before either question
	// [installRefusal] would otherwise predict the refusal from — so a check
	// that has not asked this one names the wrong refusal on a v245 host.
	ServiceTooOld bool
	// ServiceTooOldWhy is, when that is a measured true, the clause the
	// refusal carries — the version found and the one needed — so the check's
	// prediction is the refusal's own words. "" otherwise.
	ServiceTooOldWhy string
	// ServiceTooOldUnmeasured is why that was never established — the OS
	// has no versioned service to ask (macOS's configd takes no such
	// line), resolvectl could not be run, or the caller never asked — and ""
	// when it was. #104's shape once more: with this set, ServiceTooOld
	// carries no answer, and a refusal predicted from the other two
	// questions says it may not be the first one the developer meets.
	ServiceTooOldUnmeasured string
	// System is the operating system's own path — what curl, grpcurl and
	// browsers use. See [ProbeSystem].
	System ProbeResult
	// Default is the probing binary's own default resolver, and it is
	// measured rather than derived because which resolver a given build gets
	// has changed across Go releases (Go 1.20 gave darwin a libc path that
	// needs no cgo).
	Default ProbeResult
	// DefaultCGODisabled says whether the binary that produced Default was
	// built with CGO_ENABLED=0 — [CGODisabled]'s answer when the probe ran
	// in process, and whatever the caller can prove of the binary it ran
	// when it did not (the tests here read `go version -m` off the child
	// they built, which is the same authority [CGODisabled] reads).
	//
	// It decides how [ClassifyDNS] describes the binary that measured
	// Default, never which state it reports — the divergence is what is
	// classified, so [DNSTrap] is reached and named with this false. What
	// changes is the advice and the verdict: only a CGO_ENABLED=0
	// measurement makes "build with cgo enabled" a way out of the trap, and
	// only a CGO_ENABLED=0 measurement makes [DNSWorking] an all-clear —
	// [VerdictOK] rather than [VerdictUnknown] — because a cgo-enabled
	// binary resolving *.internal is not evidence that a cgo-disabled one
	// would (#99). Spending it as if it were is the false all-clear #6
	// exists to prevent, so that row says to re-measure instead.
	DefaultCGODisabled bool
	// PureGo is Go's pure resolver — the one -tags netgo and
	// GODEBUG=netdns=go force, which reads /etc/resolv.conf rather than
	// asking the system's machinery.
	PureGo ProbeResult
	// AtFilePort is the lookup made on loopback at the port the installed
	// file names — [ProbeAt] at [resolverfile.Wiring.ServerOf]'s port — so
	// the "installed, but not answering" row can say from measurement
	// whether voya is answering there rather than sending the developer to
	// `dig` it (#243). Loopback is where voya's listener is and what every
	// file voya writes names; a hand-edited file naming some other address
	// names a server that cannot be voya's, so it is not dialled and the
	// reason says which address the file names (#253) — which is why the
	// rows that quote this say which address they asked rather than "the
	// server the file names". Unmeasured when there was no port to dial: the
	// file absent or unreadable, naming no port or no server, or naming one
	// off loopback; or the caller's budget ran out before the port answered.
	// The zero value reads as unmeasured too, the row falling back to asking
	// the question as prose, because a dial nobody made must not read as
	// voya measured silent (#104).
	AtFilePort ProbeResult
}

// DNSState classifies the *.internal DNS path, distinguishing the three
// situations #6 requires never be collapsed — plus the one where the file is
// installed but nothing answers, which is neither of the failures above and
// would be misdiagnosed as either.
type DNSState int

const (
	// DNSResolverMissing: no [resolverfile.Path], on the OS that reads it —
	// the opt-in route to voya was never written, a conclusion the stat
	// carries on its own, and a different thing from the trap, which the
	// report says. Not that nothing resolves: something answering anyway is
	// unaccounted for rather than absent, so the state holds and the detail
	// names what answered. The state is not a fault by itself (#296):
	// [missingFileVerdict] scores it. Off that OS the state is never reported, absent
	// file or not: the word is a claim about a file nothing there reads, and
	// those rows classify from the probes like every other off-OS row (#96,
	// #134).
	DNSResolverMissing DNSState = iota
	// DNSWorking: every path this run measured resolved *.internal through
	// the listener, bar the pure resolver — it can be blind with the state
	// still holding, because that is the trap's residue rather than the trap,
	// and the detail says so. On the OS that reads the file, that is with the
	// file installed (or its state unestablished, #104); off it, the file
	// decides nothing (#96). Whether the evidence answers ADR 005's question
	// — the system path measured, and the binary built CGO_ENABLED=0 — is
	// [DNSReport.Verdict]'s to say (#99).
	DNSWorking
	// DNSTrap: the system resolves *.internal and the probing binary's own
	// resolver does not. Every diagnostic the developer reaches for reports
	// success while the one path their service uses fails (#6).
	//
	// The state is the measurement, not the build flag: which builds land on
	// which resolver moved under ADR 002's feet once, so the divergence is
	// what is classified and [Probes.DefaultCGODisabled] only decides how the
	// detail describes the binary that measured it.
	DNSTrap
	// DNSBroken: the path this state rests on did not resolve through voya —
	// the system path where it was measured, this binary's own resolver where
	// it was not. Not "nothing resolved": the other path may have answered all
	// the same — this binary's own resolver reaching the listener while the
	// system path did not is one of these rows, and one that says so. On the
	// OS that reads the file, that is with the file installed (or its state
	// unestablished, #104); off it, the file decides nothing (#96).
	DNSBroken
)

// String names the state the way `voya dns check` will print it.
func (s DNSState) String() string {
	switch s {
	case DNSResolverMissing:
		return "resolver-not-installed"
	case DNSWorking:
		return "working"
	case DNSTrap:
		return "cgo-resolver-trap"
	case DNSBroken:
		return "not-answering"
	default:
		return fmt.Sprintf("DNSState(%d)", int(s))
	}
}

// Verdict is a report's second axis, split from the state on purpose (#99):
// the state is what the probes found, the verdict is whether that evidence
// can answer the question ADR 005 asks — *.internal resolving through the
// system path, measured, from a CGO_ENABLED=0 binary. Collapsing the axes
// into one is what made a cgo-enabled binary's "resolving" read as an
// all-clear, and it is what any probe that can be unavailable would repeat by
// inventing an escape-hatch state of its own.
type Verdict int

const (
	// VerdictUnknown: the evidence cannot answer the question — a probe
	// could not run, or the binary that measured cannot speak for the build
	// mode the question is about. Deliberately the zero value, so a report
	// nobody scored reads as "not answered", never as a pass.
	VerdictUnknown Verdict = iota
	// VerdictOK: measured, and the evidence answers the question: yes.
	VerdictOK
	// VerdictProblem: measured, and something is conclusively wrong.
	VerdictProblem
)

// String names the verdict the way `voya dns check` prints it.
func (v Verdict) String() string {
	switch v {
	case VerdictOK:
		return "ok"
	case VerdictProblem:
		return "problem"
	case VerdictUnknown:
		return "unknown"
	default:
		return fmt.Sprintf("Verdict(%d)", int(v))
	}
}

// ExitCode is the check's verdict-to-exit-status table: ok 0, problem 1,
// unknown 2. Three codes because a check that could not be run is not a pass
// and not a failure, and collapsing it into either would re-open #99 one
// level up, in a shell script. This is not ADR 005's status-to-code table —
// that one maps proxy routing outcomes to codes for a caller; the two share
// no rows.
func (v Verdict) ExitCode() int {
	switch v {
	case VerdictOK:
		return 0
	case VerdictProblem:
		return 1
	default:
		return 2
	}
}

// DNSReport is a state and a verdict with the sentences a developer can act
// on. Detail always ends with the raw measurements, because the report exists
// to be believed over the tools that lie here (the browser resolving while
// the service gets NXDOMAIN), and evidence is what earns that.
type DNSReport struct {
	State   DNSState
	Verdict Verdict
	Detail  string
}

// ClassifyDNS turns measurements into the report `voya dns check` gives.
//
// The report carries two axes on purpose (#99): State is what the probes
// measured, and Verdict is whether that evidence can answer ADR 005's
// question. Splitting them is also what keeps a system path with no
// measurement — every OS but darwin until the wiring there is specified
// (#155), and darwin itself when dscacheutil cannot run — from being
// read as a measurement of failure (#96): those rows report what the Go
// probes did measure, with the verdict unknown. [Probes.ResolverUnmeasured]
// gets the same reading (#104): a file whose state was never established —
// its stat failed, or off the reading OS no stat was made — is never claimed
// absent and never claimed present, so those rows classify on the probes and
// never [DNSResolverMissing], whichever OS they came from. So does an
// established absence off the reading OS (#134): the file is inert there, so
// the state word — a claim about the file — is not said of it either.
func ClassifyDNS(p Probes) DNSReport {
	p = p.normalized()
	state, verdict, detail := classify(p)
	build := "CGO_ENABLED=0"
	if !p.DefaultCGODisabled {
		build = "cgo enabled or build settings unreadable"
	}
	detail += fmt.Sprintf("\nmeasured: system=%s; go binary (%s)=%s; pure resolver=%s; file port=%s",
		p.System.summary(), build, p.Default.summary(), p.PureGo.summary(), p.AtFilePort.summary())
	return DNSReport{State: state, Verdict: verdict, Detail: detail}
}

// missingFileVerdict scores [DNSResolverMissing] (#296). The file is opt-in
// (ADR 002) and no Go service needs it, so its absence alone is
// [VerdictOK], from any build: #99's CGO_ENABLED=0 condition is about whether
// a binary can vouch for the installed path, and here no path is installed to
// vouch for. A probe that went unmeasured leaves [VerdictUnknown], #104's rule
// that a check nobody finished is not a pass. Anything that answered
// *.internal without the file stays [VerdictProblem]: that is not an absence
// but an unexplained resolver, which is what the row's detail sends the
// developer to find.
func missingFileVerdict(p Probes) Verdict {
	verdict := VerdictOK
	for _, r := range []ProbeResult{p.System, p.Default, p.PureGo} {
		if r.answered() {
			return VerdictProblem
		}
		if r.Unmeasured != "" {
			verdict = VerdictUnknown
		}
	}
	return verdict
}

// optionalFileNote is the sentence that keeps the [DNSResolverMissing] row
// from reading as a fault where [missingFileVerdict] did not score one, and
// it is said only there: a row at [VerdictProblem] has just named an answer
// nobody can account for, and telling that developer all is well would be the
// false all-clear the verdict exists to prevent.
func optionalFileNote(v Verdict) string {
	if v == VerdictProblem {
		return ""
	}
	return "That file is opt-in, so its absence is not a problem in itself. "
}

// defaultUnmeasuredSplit is what the three trap-shaped rows say in place of
// the trap where this binary's own resolver produced no measurement (#253,
// #258). The trap is a divergence between two paths, so with one of them
// unmeasured the divergence was not measured either: reading the false
// [ProbeResult.ThroughListener] of an unmeasured result as "this binary did
// not resolve" is #104's rule broken — a "no" nobody measured, acted on as
// one — and here it would carry [VerdictProblem] and exit 1 with it, over an
// evidence line reading `go binary (…)=not measured (…)`. Reachable since
// #253 made a probe whose budget fires unmeasured on every probe rather than
// on [Probes.AtFilePort] alone.
//
// It does not end on a space: every caller follows it with a residue clause
// that opens on one.
func defaultUnmeasuredSplit(p Probes) string {
	return "Whether this Go binary's own resolver reaches voya could not be measured (" +
		p.Default.Unmeasured + "), and that is the other half of the split ADR 002 documents: " +
		"whether the trap is biting here is exactly what this run cannot say. Use the voya " +
		"client dialer, which never consults the system resolver, and re-run `voya dns check` " +
		"once the probe can run."
}

func classify(p Probes) (DNSState, Verdict, string) {
	// The OS first, and on nothing else: every branch below reasons about the
	// file — claims its state, prescribes it — and off the OS that reads it
	// the file is inert, so none of that may be said there whatever else was
	// measured (#96). Routing the off-OS rows here only when the file or the
	// system path went unmeasured left the combination with both established
	// falling into the main switch, which named the file's absence and told
	// the developer to write it (#122).
	if !p.OSReadsResolverFile {
		return classifyForeignSystemPath(p)
	}
	// Before the main switch for the same reason: an unestablished file state
	// is never claimed either way, and the switch claims it in every branch.
	if p.ResolverUnmeasured != "" {
		return classifyUnmeasuredResolverFile(p)
	}
	if p.System.Unmeasured != "" {
		return classifyUnmeasuredSystem(p)
	}
	switch {
	case !p.ResolverInstalled:
		detail := resolverfile.Path + " is not installed"
		switch {
		case p.System.ThroughListener():
			// The two things the plain row says — nothing resolves, and so
			// this is not the trap — are both false here, so neither is said.
			// The file is the only sanctioned way *.internal reaches voya, so
			// an answer arriving without it is unaccounted for rather than an
			// all-clear, and the trap's own shape can arrive by that route.
			detail += ", yet the system resolved *.internal anyway, so something other than " +
				"that file is answering — worth finding before it disagrees with voya. "
			if p.Default.Unmeasured != "" {
				// The split is a claim about two paths, and this run measured
				// one of them — [defaultUnmeasuredSplit]'s reading (#258),
				// on a row the unexplained answer keeps at [VerdictProblem]
				// regardless (#260, #296).
				detail += "It is answering for curl and browsers; whether it answers this Go " +
					"binary's own resolver too could not be measured (" + p.Default.Unmeasured +
					"), so whether the split ADR 002 documents is here is what this run cannot " +
					"say — believe the voya client dialer over what curl reports either way. "
			} else if !p.Default.ThroughListener() {
				detail += "It is answering for curl and browsers while this Go binary's own " +
					"resolver is not, which is the split ADR 002 documents whatever is causing " +
					"it here: believe the voya client dialer over what curl reports. "
			}
		case p.System.answered():
			// Measured, and not "nothing resolves": the system path answered
			// with an address that is not voya's loopback, and every branch
			// below it says curl and browsers get nothing — above an
			// evidence line printing that address. The distinction those
			// branches now draw for this binary's own resolver (#260) is
			// owed to the path the file would have configured too, and the
			// row above draws it for an answer that is voya's.
			detail += ", and something is answering *.internal for curl and browsers with an " +
				"address that is not loopback (" + p.System.summary() + "), so it is not voya's " +
				"wildcard — another resolver, VPN or search domain is claiming the name, worth " +
				"finding before it disagrees with voya. That is not the CGO_ENABLED=0 trap " +
				"either, whose shape is the system path reaching voya while this binary's does " +
				"not. Go services using the voya client dialer are unaffected — it never " +
				"consults the system resolver. "
		case p.Default.answered() && !p.Default.ThroughListener():
			// Measured, and not "nothing resolves": this Go binary got an
			// answer, it is just not voya's — the distinction every other
			// row draws ([classifyUnmeasuredSystem] for one), and here
			// "fail exactly like Go does" would contradict the address on
			// the evidence line below it. Not the trap either, whose shape
			// is the system path resolving while this binary's does not.
			detail += ": the system does not resolve *.internal, so curl and browsers get " +
				"nothing — yet this Go binary's own resolver got an answer that is not loopback (" +
				p.Default.summary() + "), so something other than voya is claiming *.internal " +
				"for it; worth finding before it disagrees with voya. That is not the CGO_ENABLED=0 " +
				"trap, whose shape is the system path resolving while this binary's does not. "
		case defaultBlind(p):
			detail += ": nothing on this machine resolves *.internal — curl and browsers fail " +
				"exactly like Go does, so this is not the CGO_ENABLED=0 trap. Go services using " +
				"the voya client dialer are unaffected. "
		case p.Default.Unmeasured != "":
			// "curl and browsers fail exactly like Go does" is a claim about
			// both paths, and the Go side went unmeasured (#260). That it is
			// not the trap still stands: the trap is the system path
			// resolving while this binary's does not, and the system path
			// measured not resolving rules it out on its own.
			detail += ": the system does not resolve *.internal, so curl and browsers get " +
				"nothing — which on its own rules out the CGO_ENABLED=0 trap, the trap being " +
				"the system path resolving while this binary's does not. Whether this Go " +
				"binary's own resolver fails the same way could not be measured (" +
				p.Default.Unmeasured + "), so this run cannot say; Go services using the voya " +
				"client dialer are unaffected either way. "
		default:
			// "curl fails exactly like Go does" is a claim about two paths,
			// and this run measured them disagreeing. It is the split above
			// mirrored — the Go binary has the name and the system does not —
			// so it is not the trap either, but saying nothing resolves would
			// contradict the evidence line printed directly below it.
			detail += ": the system does not resolve *.internal, so curl and browsers get " +
				"nothing — yet this Go binary's own resolver did resolve it, by a route the " +
				"missing file cannot explain. That is ADR 002's divergence the other way " +
				"round, not the trap itself; find what is answering this binary before " +
				"trusting either path. "
		}
		// The remedy is the same file either way, but it is not the same
		// sentence: offering curl the names it has just been measured
		// resolving would read as the check not having looked. Measured
		// resolving is [ProbeResult.answered], not [ProbeResult.ThroughListener]
		// — an address that is not voya's is still an address curl has, and
		// what the file changes there is which path answers.
		remedy := "To give curl and browsers the names, point "
		if p.System.answered() {
			remedy = "To put *.internal on the path voya sanctions rather than whichever one is " +
				"answering, point "
		}
		verdict := missingFileVerdict(p)
		return DNSResolverMissing, verdict, detail + optionalFileNote(verdict) + remedy +
			resolverfile.Path + " at the DNS listener `voya run` names in its startup banner: " +
			"`sudo voya dns install` writes it." + installRefusal(p)

	case !p.System.ThroughListener():
		detail := resolverfile.Path + " is installed, but the system did not resolve " +
			"*.internal through voya. "
		if p.ServiceTooOldUnmeasured == "" && p.ServiceTooOld {
			// The third cause neither the file nor voya is guilty of
			// (#206), and the one asked first because it is asked first
			// by the install too: a resolved that rejects the port reads
			// the file and configures nothing from it, bypassed or not,
			// catch-all or not, so the plain remedy's restart changes
			// nothing and the command it names would be refused for this.
			return DNSBroken, VerdictProblem, detail + tooOldCause(p)
		}
		if p.ServiceBypassedUnmeasured == "" && p.ServiceBypassed {
			// The other cause neither the file nor voya is guilty of
			// (#204), and the one asked first because it is the more
			// basic: a service nothing asks reads the file and is never
			// consulted, catch-all or not, so the plain remedy's restart
			// changes nothing and the command it names would be refused.
			return DNSBroken, VerdictProblem, detail + notThroughServiceCause(p)
		}
		if p.GlobalCatchAllUnmeasured == "" && p.GlobalCatchAll {
			// The one cause here that neither the file nor voya is guilty
			// of (#185): measured on a host with both in order, the file
			// naming the port voya was answering on, and zero packets
			// reaching it across two lookups. Restarting the service that
			// read the file changes nothing, so the plain remedy below —
			// which prescribes exactly that — is not given.
			return DNSBroken, VerdictProblem, detail + catchAllCause(p)
		}
		if p.System.answered() {
			// It answered — with something that is not voya's loopback. A
			// different problem from silence, and "is voya running?" is the
			// wrong question to send the developer off with. A result that
			// carries neither addresses nor an error is silence too, and
			// naming its empty address list as the wrong answer would be a
			// wrong answer of the check's own.
			detail += "Something answered, but with an address that is not loopback (" +
				p.System.summary() + "), so it is not voya's wildcard: another resolver, VPN or " +
				"search domain is claiming *.internal ahead of " + resolverfile.Path + ". " +
				cachedAnswerHint()
		} else {
			detail += "Nothing is answering where the file points. "
		}
		return DNSBroken, VerdictProblem, detail + filePortAnswer(p) + reloadRemedy(p) +
			staleCacheRemedy(p) + tooOldUnread(p) + catchAllUnread(p)

	case p.Default.ThroughListener():
		// The state is what was seen; the verdict is whether the binary that
		// saw it can answer ADR 005's question, which asks specifically
		// about a CGO_ENABLED=0 build (#99).
		verdict := VerdictOK
		detail := "*.internal resolves for the system (curl, browsers) and from this Go binary's " +
			"own resolver."
		if p.DefaultCGODisabled {
			detail += " That binary was built with CGO_ENABLED=0, so this toolchain reaches the " +
				"system resolver without cgo."
		} else {
			verdict = VerdictUnknown
			// The measurement ADR 005 asks for is specifically from a
			// CGO_ENABLED=0 build; claiming its all-clear from any other one
			// is the false all-clear #6 exists to prevent.
			detail += " That binary was not built with CGO_ENABLED=0 (cgo is enabled, or its " +
				"build settings are unreadable), so it says nothing about ADR 002's trap: a " +
				"CGO_ENABLED=0 build may still get NXDOMAIN here. Re-measure from one."
		}
		return DNSWorking, verdict, detail + installedPureResolverResidue(p)

	case p.Default.Unmeasured != "":
		// Not the trap row: the system path resolved through the listener
		// and the other half of the divergence went unmeasured
		// ([defaultUnmeasuredSplit], #258). The state is what was measured.
		return DNSWorking, VerdictUnknown, resolverfile.Path + " is installed, and the system " +
			"(curl, browsers) resolved *.internal through the listener. " +
			defaultUnmeasuredSplit(p) + installedPureResolverResidue(p)

	default:
		detail := "the resolver trap (ADR 002): curl and browsers resolve *.internal through the " +
			"system path, but this Go binary's own resolver does not — "
		if p.Default.answered() {
			// It is not ignoring the file; something handed it an answer. The
			// state holds — the binary still does not reach voya — but the
			// cause below it does not, and neither does the timeout caveat,
			// because nothing here failed.
			detail += "it answered with an address that is not loopback (" + p.Default.summary() +
				"), which means another resolver is claiming *.internal for this binary rather " +
				"than " + resolverfile.Path + " going unread"
		} else {
			detail += "it never consults " + resolverfile.Path
		}
		detail += ", so every diagnostic reporting success is true of a path the " +
			"service does not use. Use the voya client dialer, which never consults the system " +
			"resolver. "
		if p.DefaultCGODisabled {
			detail += "The measurement is from a CGO_ENABLED=0 build, which is the case ADR 002 " +
				"names; building the service with cgo enabled is the other way out."
		} else {
			// Worse than the named case, not better: cgo was available and
			// the lookup still missed, so there is no build flag to flip.
			detail += "The measurement is from a build that is not CGO_ENABLED=0 (cgo is enabled, " +
				"or its build settings are unreadable), so this is broader than the CGO_ENABLED=0 " +
				"case ADR 002 names — enabling cgo is not the way out of it."
		}
		// When the state does rest on a lookup having failed, a lookup that
		// timed out is the same measurement here as one that came back
		// NXDOMAIN: results cross a process boundary as strings, so nothing
		// downstream can tell them apart. Only the second is the trap, so the
		// report hands the failure back rather than narrating past it. There
		// is nothing to hand back when the path answered instead of failing.
		if p.Default.Err != "" {
			detail += " That reading rests on the default path's failure, quoted below: if it " +
				"is a timeout rather than a name that did not resolve, a slow resolver is " +
				"failing in the trap's shape — re-run before acting on this."
		}
		// Conclusive whatever built the binary (#99): the divergence was
		// measured, so the flag changes the advice above, never the verdict.
		return DNSTrap, VerdictProblem, detail
	}
}

// classifyUnmeasuredSystem is every row on the OS that reads
// [resolverfile.Path] where the system path produced no measurement: its
// tooling could not be run, or the deadline fired first. Neither is a "no" —
// reading them as one is how a host where voya was answering perfectly well
// got told "Is voya running?" (#96) — so these rows classify only what was
// measured, and the verdict is [VerdictUnknown] throughout but one row: the
// file's absence is a conclusion the stat carries on its own, no probe
// required, so that row's verdict is [missingFileVerdict]'s. (Off that OS,
// where no probe exists at all, the report is
// [classifyForeignSystemPath]'s.)
func classifyUnmeasuredSystem(p Probes) (DNSState, Verdict, string) {
	couldNot := "the system path (what curl and browsers use) could not be probed: " +
		p.System.Unmeasured + ". "
	switch {
	case !p.ResolverInstalled:
		detail := resolverfile.Path + " is not installed, so curl and browsers have nothing " +
			"pointing *.internal at voya — a conclusion the missing file carries on its own, " +
			"though " + couldNot
		if p.Default.ThroughListener() {
			detail += "This binary's own resolver resolved *.internal even so, by a route the " +
				"missing file cannot explain — worth finding what is answering before trusting " +
				"it. "
		} else if p.Default.answered() {
			detail += "This binary's own resolver got an answer that is not loopback (" +
				p.Default.summary() + "), so something other than voya is claiming *.internal " +
				"for it — worth finding before it disagrees with voya. "
		}
		// The refusals are predicted here as on the row above (#185, #204):
		// what went unmeasured is the system path, and the two questions
		// behind [installRefusal] are asked of the host wherever the file is
		// read, so the check knows this command will be refused and owes the
		// developer the same round trip either way.
		verdict := missingFileVerdict(p)
		return DNSResolverMissing, verdict, detail +
			"Go services using the voya client dialer are unaffected. " + optionalFileNote(verdict) +
			"To give curl and browsers " +
			"the names, point " + resolverfile.Path + " at the DNS listener `voya run` names in " +
			"its startup banner: `sudo voya dns install` writes it." + installRefusal(p)
	case p.Default.ThroughListener():
		detail := resolverfile.Path + " is installed, and this binary's own resolver resolved " +
			"*.internal through the listener"
		if p.DefaultCGODisabled {
			detail += " from a CGO_ENABLED=0 build, so ADR 002's trap is not biting this " +
				"toolchain"
		}
		detail += ". But " + couldNot + "Whether curl and browsers reach voya is exactly what " +
			"that probe measures, so this run cannot say — re-run `voya dns check` once it can."
		return DNSWorking, VerdictUnknown, detail + installedPureResolverResidue(p)
	case p.Default.answered():
		// It resolved — to something that is not voya's loopback. Neither of
		// the two faults the row below names is that one, and "did not
		// resolve *.internal" would contradict the address printed on the
		// evidence line three lines under it. The same distinction
		// [classifyForeignSystemPath] draws, and for the same reason.
		return DNSBroken, VerdictUnknown, resolverfile.Path + " is installed, but this binary's " +
			"own resolver got an answer that is not loopback (" + p.Default.summary() + "), so " +
			"another resolver is claiming *.internal ahead of that file — worth finding before " +
			"it disagrees with voya. And " + couldNot + "Whether curl and browsers reach voya is " +
			"exactly what that probe measures, so this run cannot say. Use the voya client " +
			"dialer, which never consults the system resolver, and re-run `voya dns check` once " +
			"the probe can run."
	case p.Default.Unmeasured != "":
		// Neither path measured (#260): the row below opens on a "no" from
		// this binary's own resolver, and here nothing was looked up. The
		// state is still what the one fact established — the file — leaves
		// the report to say, and the verdict is the unknown every row here
		// carries.
		detail := resolverfile.Path + " is installed, but neither path was measured: " + couldNot +
			"And whether this binary's own resolver resolves *.internal could not be measured " +
			"either (" + p.Default.Unmeasured + "). "
		switch {
		// The file is installed, so the check dialled the port it names
		// (#243) — and one of the faults this row lists as
		// indistinguishable is exactly what that dial settles. Listing it
		// anyway would contradict the `file port=…` printed three lines
		// below, the same way the unmeasured reads above it did.
		case p.AtFilePort.ThroughListener():
			detail += "voya itself is up, at least: this run asked 127.0.0.1 on the port the " +
				"file names directly and voya's loopback came back, so nothing answering there " +
				"is not the fault here. What the two unprobed paths would have told apart is " +
				"the trap ADR 002 documents from no fault at all. "
		case p.AtFilePort.measured():
			detail += "One fault was measured even so: nothing that is voya is answering on the " +
				"port the file names — this run asked 127.0.0.1 on that port directly and got: " +
				p.AtFilePort.summary() + ". Start voya, or point it back at that port: `voya run` " +
				"names its DNS listener in the startup banner. What the two unprobed paths would " +
				"have added is whether the trap ADR 002 documents is here too. "
		default:
			// No dial, or none this row may read as one (#104): the fault
			// stays on the list, where the row already had it.
			detail += "So nothing this run saw can tell the faults apart — nothing answering " +
				"where the file points, the trap ADR 002 documents, or no fault at all. "
		}
		return DNSBroken, VerdictUnknown, detail + "Use the voya client dialer, which never " +
			"consults the system resolver, and re-run `voya dns check` once the probes can run."
	default:
		detail := resolverfile.Path + " is installed, but this binary's own resolver did not " +
			"resolve *.internal — and " + couldNot
		switch {
		// The same dial the row above reads (#243): the file is installed,
		// so the check asked the port it names, and one of the two faults this
		// row sends the developer to tell apart is what that dial settles.
		case p.AtFilePort.ThroughListener():
			detail += "voya itself is up, at least: this run asked 127.0.0.1 on the port the " +
				"file names directly and voya's loopback came back, so nothing answering there " +
				"is not the fault. What the unprobed path would have told apart is what stops " +
				"the system path reaching that port from the trap ADR 002 documents — the " +
				"system path resolving while this binary's does not."
		case p.AtFilePort.measured():
			detail += "Nothing that is voya is answering on the port the file names either — " +
				"this run asked 127.0.0.1 on that port directly and got: " +
				p.AtFilePort.summary() + ". Start voya, or point it back at that port: `voya " +
				"run` names its DNS listener in the startup banner. Whether the trap ADR 002 " +
				"documents is here too — the system path resolving while this binary's does " +
				"not — is what the unprobed path would have told apart."
		default:
			// No dial, or none this row may read as one (#104).
			detail += "So this is one of two faults that probe would have told apart: nothing " +
				"answering where the file points (is voya running? `voya run` names its DNS " +
				"listener in the startup banner), or the system path resolving while this " +
				"binary's does not, which is the trap ADR 002 documents."
		}
		return DNSBroken, VerdictUnknown, detail + " Use the voya client dialer either way, and " +
			"re-run `voya dns check` once the probe can run before acting further."
	}
}

// classifyUnmeasuredResolverFile is every row on the OS that reads
// [resolverfile.Path] where the stat itself failed, so the file is neither
// present nor absent (#104). One state is off the table by construction:
// [DNSResolverMissing] is the file's absence, and absence is exactly what
// was not measured. The rows classify on what the probes did measure, quote
// the stat's reason, and take [VerdictUnknown] wherever the file's state is
// what would have made the evidence conclusive — where every state the file
// could be in reads as a problem, the verdict says so despite the unread
// file, because the verdict is about the evidence, not about completeness.
// (Off that OS the file is inert, so an unestablished state is not the
// missing evidence these rows are about — it is one more thing that would
// have meant nothing; [classifyForeignSystemPath] is that report, and it
// refuses [DNSResolverMissing] outright, established absence included.)
func classifyUnmeasuredResolverFile(p Probes) (DNSState, Verdict, string) {
	couldNot := resolverfile.Path + " could not be read (" + p.ResolverUnmeasured +
		"): neither present nor absent, so nothing below rests on the file's state. "
	if p.System.Unmeasured != "" {
		return classifyNothingButTheBinary(p, couldNot)
	}
	switch {
	case !p.System.ThroughListener():
		detail := couldNot
		if p.System.answered() {
			detail += "Something answered the system path with an address that is not loopback (" +
				p.System.summary() + "), so it is not voya's wildcard: another resolver, VPN or " +
				"search domain is claiming *.internal ahead of voya"
		} else {
			detail += "The system (curl, browsers) did not resolve *.internal through voya"
			if p.Default.ThroughListener() {
				detail += " — though this Go binary's own resolver did, by a route nothing " +
					"measured here explains"
			}
		}
		detail += ", which is a problem whichever state the file is in: installed, nothing usable " +
			"answers where it points (is voya running? `voya run` names its DNS listener in the " +
			"startup banner); absent, curl and browsers have nothing pointing *.internal at voya. " +
			"The unreadable file is what would have told those apart — make it readable and " +
			"re-run `voya dns check`."
		return DNSBroken, VerdictProblem, detail
	case p.Default.ThroughListener():
		detail := couldNot + "*.internal resolves through the listener for the system (curl, " +
			"browsers) and from this Go binary's own resolver"
		if p.DefaultCGODisabled {
			detail += ", measured from a CGO_ENABLED=0 build"
		}
		detail += ". But whether " + resolverfile.Path + " — the one sanctioned way *.internal " +
			"reaches voya — is what routes them is exactly what the unread file would have said: " +
			"absent, something unaccounted for is answering. Make the file readable and re-run " +
			"`voya dns check` before trusting this."
		return DNSWorking, VerdictUnknown, detail + unestablishedPureResolverResidue(p)
	case p.Default.Unmeasured != "":
		// The split is what this row would name, and it was not measured
		// ([defaultUnmeasuredSplit], #258) — nor was the file read, so the
		// wiring behind the one path that did answer is unaccounted for too.
		return DNSWorking, VerdictUnknown, couldNot + "The system (curl, browsers) resolved " +
			"*.internal through the listener, though whether " + resolverfile.Path + " — the " +
			"one sanctioned way *.internal reaches voya — is what routes it is what the unread " +
			"file would have said. " + defaultUnmeasuredSplit(p) +
			unestablishedPureResolverResidue(p)
	default:
		detail := couldNot + "And the split ADR 002 documents was measured: curl and browsers " +
			"resolve *.internal through voya while this Go binary's own resolver does not"
		if p.Default.answered() {
			detail += " — it answered with an address that is not loopback (" + p.Default.summary() +
				"), so another resolver is claiming *.internal for it"
		}
		detail += ". Whether the file is installed and going unread by this binary (the trap), or " +
			"was never installed and something else routes the system, is what the unreadable " +
			"file would have said — but every diagnostic reporting success is true of a path the " +
			"service does not use either way: use the voya client dialer, which never consults " +
			"the system resolver, and make the file readable before re-running `voya dns check`."
		return DNSTrap, VerdictProblem, detail
	}
}

// classifyNothingButTheBinary is [classifyUnmeasuredResolverFile] with the
// system path also unmeasured: this binary's own resolver is the most the
// run has measured, so it is classified alone — and since #253 it too can
// come back unmeasured, which leaves the row nothing measured to rest on
// (#260). Nothing it rests on, not nothing at all: `voya dns check` gives every
// probe its own budget and runs them together, so Go's pure resolver can
// answer where this one timed out, and the evidence line prints that answer
// under a row that must not have called the run unmeasured.
// Every verdict is [VerdictUnknown]: with the file and the system path both
// unread, no state either could be in can be ruled out.
func classifyNothingButTheBinary(p Probes, couldNot string) (DNSState, Verdict, string) {
	detail := couldNot + "And the system path (what curl and browsers use) could not be probed: " +
		p.System.Unmeasured + ". "
	switch {
	case p.Default.ThroughListener():
		detail += "This binary's own resolver resolved *.internal through the listener"
		if p.DefaultCGODisabled {
			detail += " from a CGO_ENABLED=0 build"
		}
		detail += ", the one measurement this run has. Whether curl and browsers reach voya, and " +
			"whether the sanctioned file is what routes anything, is exactly what could not be " +
			"checked — re-run `voya dns check` once it can be."
		return DNSWorking, VerdictUnknown, detail + unestablishedPureResolverResidue(p)
	case p.Default.answered():
		detail += "This binary's own resolver got an answer that is not loopback (" +
			p.Default.summary() + "), so another resolver is claiming *.internal for it — worth " +
			"finding before it disagrees with voya. Use the voya client dialer, which never " +
			"consults the system resolver, and re-run `voya dns check` once the other checks can run."
		return DNSBroken, VerdictUnknown, detail
	case p.Default.Unmeasured != "":
		// Nor this binary's own resolver (#260): the row below opens on a
		// measurement this row does not have. What it has none of is a
		// measurement it rests on — "this run measured nothing" would be a
		// claim about the whole run, and the pure-resolver probe has its own
		// budget, so it can answer where this one did not and be printed
		// saying so on the evidence line below.
		detail += "And whether this binary's own resolver resolves *.internal could not be " +
			"measured either (" + p.Default.Unmeasured + "), so this row rests on no measurement " +
			"at all: the file, the system path and this binary's own resolver are all silent, and " +
			"none of them can say which fault is here, or whether there is one — voya not " +
			"running, the file never installed, or the trap ADR 002 documents. Use the voya client " +
			"dialer either way, and re-run `voya dns check` once the checks can run."
		return DNSBroken, VerdictUnknown, detail
	default:
		detail += "This binary's own resolver did not resolve *.internal, and with the file and " +
			"the system path both unread, nothing can say which fault that is — voya not running, " +
			"the file never installed, or the trap ADR 002 documents. Use the voya client dialer " +
			"either way, and re-run `voya dns check` once the other checks can run."
		return DNSBroken, VerdictUnknown, detail
	}
}

// classifyForeignSystemPath is every row off the OS that reads
// [resolverfile.Path] — the honest "not checked on this OS" report (#96): the
// /etc/resolver mechanism is macOS's, which is what `voya dns install`
// writes, and ADR 002 specifies no wiring for any other OS (#155) — so
// there is no system path to
// measure, and nothing the report may prescribe, because naming a file this
// OS never reads would send a developer to write configuration that does
// nothing. The Go probes did measure, so their rows are still classified and
// reported rather than discarded behind one word; the verdict is
// [VerdictUnknown] on every row, because the question ADR 005 asks is about
// a path this OS does not have yet.
//
// A system path that was measured here all the same — a caller substituting
// one of the two darwin-keyed inputs, or the Linux probe once the wiring it
// would measure exists — is not discarded either: [classifyForeignMeasuredSystem]
// classifies from it (#122). What it may not do is answer for it, because the
// wiring it went through is not the one ADR 005's question is about.
//
// The file decides no state here, absent included (#134). Its absence is
// real when a caller established it, but [DNSResolverMissing] is a claim
// about a file this OS never reads — the same claim the remedy is forbidden
// to make (#96) — and until this function stopped returning it, the same
// absent file classified from the measurement when the system path was
// measured and as the file's absence when it was not. Every row here is the
// probes'.
//
// An established absence is the one state of the file no row here mentions.
// Presence gets a sentence, and so does a state the stat never established
// (#104) — that one says only that it changes nothing. That the third gets
// none is not because a sentence about it would break #96 — "is not
// installed, but nothing on this OS is specified to read it, so its absence
// proves nothing either way" prescribes nothing and claims no state (#142) —
// but because presence and absence are not the same kind of fact here. A
// present file off the OS that reads it is something the developer did:
// installed a macOS-only resolver file on a host that never reads it, by a
// bind mount or a copied dotfile, and the presence sentence answers that
// act. An absent file there
// is the default state of every such host, and a sentence about it would
// print on every Linux machine and answer nothing anyone asked. Presence is
// an action worth answering; absence is the universal default, and it stays
// silent — which is what cmd/voya's off-OS test pins by banning "is not
// installed" outright.
func classifyForeignSystemPath(p Probes) (DNSState, Verdict, string) {
	detail := "nothing on this machine reads a *.internal wiring file — ADR 002 specifies " +
		"macOS's /etc/resolver and, on Linux, a systemd-resolved drop-in, and `voya dns install` " +
		"declines anywhere else. "
	if p.System.Unmeasured != "" {
		// The usual way in. The sentence is conditional because the other way
		// in — a system path measured after all — must not be told it was not
		// checked, which is the kind of claim these rows exist to refuse.
		detail = "the system path (what curl and browsers use) was not checked here: " +
			p.System.Unmeasured + ". " + detail
	}
	// Both sentences name a file, and this bucket includes the hosts that have
	// none to name: [resolverfile.Path] is "" on an OS ADR 002 specifies no
	// wiring for, so on Windows or FreeBSD they would open on an empty string
	// ("'s state was not established"). Nothing is lost by dropping them there
	// — the paragraph above has already said that nothing here reads such a
	// file, which is the whole of what those hosts can be told. The Linux host
	// with no systemd-resolved, the other way into this bucket, does have a
	// path, and keeps them.
	if resolverfile.Path != "" {
		if p.ResolverUnmeasured != "" {
			// Neither present nor absent (#104) — unreadable, or never consulted
			// because nothing here reads it. Which changes nothing here, where
			// even a readable file proves nothing, but saying "is present" of a
			// file whose state was never established would.
			detail += resolverfile.Path + "'s state was not established (" + p.ResolverUnmeasured +
				"), and nothing here reads it, so that changes nothing either. "
		} else if p.ResolverInstalled {
			detail += resolverfile.Path + " is present, but nothing here reads it, so its presence " +
				"proves nothing either way. "
		}
	}
	if p.System.Unmeasured == "" {
		return classifyForeignMeasuredSystem(p, detail)
	}
	switch {
	case p.Default.ThroughListener():
		detail += "This binary's own resolver did resolve *.internal through the listener"
		if p.DefaultCGODisabled {
			detail += ", from a CGO_ENABLED=0 build,"
		}
		detail += " by a route ADR 002 has not specified for this machine — worth finding what is " +
			"answering before trusting it. " + foreignPureResolverResidue(p)
	case p.Default.answered():
		detail += "This binary's own resolver got an answer that is not loopback (" +
			p.Default.summary() + "), so another resolver is claiming *.internal here — worth " +
			"finding before it disagrees with voya. "
	case p.Default.Unmeasured != "":
		// Not resolving is the expected state off this OS, but the row
		// below says it was seen, and here nothing was looked up (#260) —
		// on Windows or FreeBSD the Go probes are the whole report, so this
		// is the whole report saying so.
		detail += "Whether this binary's own resolver resolves *.internal could not be measured (" +
			p.Default.Unmeasured + "); not resolving would be the expected state here until " +
			"this machine carries that wiring, but this run did not see it either way. "
	default:
		detail += "This binary's own resolver does not resolve *.internal, which is the " +
			"expected state here until this machine carries that wiring. "
	}
	detail += "Go services using the voya client dialer are unaffected — it never consults the " +
		"system resolver."
	// The state is the one probe that can measure here, whether or not it
	// did this run: with that probe unmeasured the row keeps [DNSBroken],
	// which #260 left as it found it — the prose above says the lookup was
	// never made, and [VerdictUnknown], which every row here carries, is
	// what says the evidence does not answer ADR 005's question. Not the
	// file's state either way: unmeasured,
	// its absence was never established (#104); established absent, it is
	// still a file nothing on this OS reads (#134).
	if p.Default.ThroughListener() {
		return DNSWorking, VerdictUnknown, detail
	}
	return DNSBroken, VerdictUnknown, detail
}

// pureResolverResidue is the trap's residue: the sentence every [DNSWorking]
// row carries when Go's pure resolver is blind, so that a developer whose
// -tags netgo or GODEBUG=netdns=go build is blind gets a hint of why rather
// than `working` (#134). It is "" when there is no residue — the pure
// resolver resolved — so a working row without it says nothing.
//
// The sentence comes in three wordings, chosen by the two arguments, because
// what it may claim depends on what the row established. namesFile says the
// pure resolver "ignores /etc/resolver/internal", which is a claim about a
// file — true of the pure resolver whatever the file's state, but a sentence
// naming the file is a sentence about it, and a row that never established
// the file's state (#104) or runs on an OS that never reads it (#96) says
// nothing about it. closesWithDialer appends "the voya client dialer is
// immune": the reading OS's rows end on the residue, so it carries the
// reassurance; the off-OS rows follow it with "Go services using the voya
// client dialer are unaffected — it never consults the system resolver", and
// the report would make the same reassurance twice running.
//
// The three callers below name the wordings so a row picks by what it knows,
// not by flags: [installedPureResolverResidue] where the file is established
// present, [unestablishedPureResolverResidue] where the reading OS could not
// establish it, [foreignPureResolverResidue] off the OS that reads it.
func pureResolverResidue(p Probes, namesFile, closesWithDialer bool) string {
	if !pureResolverBlind(p) {
		return ""
	}
	s := "One residue of the trap remains: a binary built with -tags netgo, or run with " +
		"GODEBUG=netdns=go, uses Go's pure resolver, which "
	if namesFile {
		s += "ignores " + resolverfile.Path + " and "
	}
	s += "did not resolve *.internal through voya here"
	if closesWithDialer {
		return s + "; the voya client dialer is immune."
	}
	return s + ". "
}

// installedPureResolverResidue is [pureResolverResidue] for the reading OS's
// rows where [resolverfile.Path] is established present: the residue names the
// file the pure resolver ignores. Its leading space joins it to a detail that
// ends on a full stop, and it is "" when there is nothing to say.
func installedPureResolverResidue(p Probes) string {
	if r := pureResolverResidue(p, true, true); r != "" {
		return " " + r
	}
	return ""
}

// unestablishedPureResolverResidue is [pureResolverResidue] for the reading
// OS's rows where the stat failed and the file is neither present nor absent
// (#104): the same residue, naming no file. Joined like
// [installedPureResolverResidue].
func unestablishedPureResolverResidue(p Probes) string {
	if r := pureResolverResidue(p, false, true); r != "" {
		return " " + r
	}
	return ""
}

// foreignPureResolverResidue is [pureResolverResidue] said off the OS that
// reads [resolverfile.Path]: no file named (#96), and no closing dialer clause,
// because both callers follow it with the dialer sentence of their own. It
// ends on a space so a caller can append the next sentence unconditionally.
func foreignPureResolverResidue(p Probes) string {
	return pureResolverResidue(p, false, false)
}

// pureResolverBlind reports whether Go's pure resolver was measured and did
// not reach the listener — the condition every residue sentence rests on,
// whichever OS says it.
//
// Measured is half the test on purpose (#96): [ProbeResult.Unmeasured] makes
// [ProbeResult.ThroughListener] false without anything having been looked up,
// and the residue would then tell a developer their pure resolver is blind
// directly above an evidence line reading `pure resolver=not measured (…)`.
// [ProbePureGo] reaches it since #253 — a budget that fires before the
// resolver answers comes back unmeasured — and so does a caller assembling
// [Probes] without making this probe, the same way in as #134's rows: since
// #263 that silent zero value arrives here already named [notProbed] by
// [Probes.normalized], so it is this half of the test that catches it.
//
// And "did not reach the listener" is not "gets NXDOMAIN", which is what the
// reading OS's sentence used to say: the probe also fails by answering an
// address that is not loopback, or by timing out, and both arrive at the
// report as the string the evidence line prints two lines below.
func pureResolverBlind(p Probes) bool {
	return p.PureGo.Unmeasured == "" && !p.PureGo.ThroughListener()
}

// defaultBlind is [pureResolverBlind]'s test asked of this binary's own
// resolver (#260): no reason recorded for going unmeasured, and it did not
// reach the listener. The first half is there for the same reason —
// [ProbeResult.Unmeasured] makes [ProbeResult.ThroughListener] false without
// anything having been looked up, and every `else` and `default` beneath a
// bare ThroughListener read then said this binary's own resolver failed,
// above an evidence line reading `go binary (…)=not measured (…)`. Reachable
// since #253 made a probe whose budget fires unmeasured on every probe rather
// than on [Probes.AtFilePort] alone; the trap-shaped rows hedge through
// [defaultUnmeasuredSplit], and the rows this gates are the rest of the same
// reads.
//
// It is deliberately weaker than [ProbeResult.measured], and since #263 that
// costs nothing: the silent zero value — no addresses, no error, no reason —
// is a probe nobody made, and [Probes.normalized] names it [notProbed]
// before any row reads it, so it arrives here unmeasured and is caught by
// the first half rather than read as one of the measured noes. No probe run
// produces it; only a caller assembling [Probes] itself does, and it is
// named at the door.
func defaultBlind(p Probes) bool {
	return p.Default.Unmeasured == "" && !p.Default.ThroughListener()
}

// classifyForeignMeasuredSystem is [classifyForeignSystemPath] with the
// system path measured after all (#122). The state is what was seen, on the
// path it was seen on, the way the reading OS's rows classify it — a system
// path that resolved is never reported from the Go binary's failure beside it,
// which printed `not-answering` above an evidence line saying the system
// answered. What differs from those rows is the file: nothing here reads it,
// so no state rests on it and no remedy names it. What routes the system path
// on this OS is unspecified, so the report says what answered and leaves the
// wiring to be found. The verdict stays [VerdictUnknown] throughout: ADR
// 005's question is about the wiring `voya dns install` will write once ADR
// 002 specifies it for this OS (#155), and a
// measurement through some other wiring answers neither way for it — that row
// becomes conclusive when the wiring does (#96).
func classifyForeignMeasuredSystem(p Probes, detail string) (DNSState, Verdict, string) {
	system := "The system path (what curl and browsers use) "
	unaffected := "Go services using the voya client dialer are unaffected — it never consults the " +
		"system resolver."
	switch {
	case !p.System.ThroughListener():
		if p.System.answered() {
			// The same distinction the reading OS's rows draw: an answer that
			// is not loopback is not silence, and the culprit is another
			// resolver rather than a voya that is not running.
			detail += "Something answered the system path (what curl and browsers use) with an " +
				"address that is not loopback (" + p.System.summary() + "), so it is not voya's " +
				"wildcard: another resolver, VPN or search domain is claiming *.internal here — " +
				"worth finding before it disagrees with voya. "
		} else {
			detail += system + "did not resolve *.internal through voya"
			if p.Default.ThroughListener() {
				detail += " — though this Go binary's own resolver did, by a route ADR 002 has not " +
					"specified for this machine; worth finding what is answering before trusting it"
			} else if p.Default.answered() {
				// The same distinction again, owed to the binary's row too: with
				// the system path unmeasured these probes say "another resolver
				// is claiming *.internal here", and measuring the system path
				// does not make that answer stop needing a name. Leaving it to
				// the evidence line hands the developer an address and no reading
				// of it.
				detail += " — and this Go binary's own resolver got an answer that is not loopback (" +
					p.Default.summary() + "), so another resolver is claiming *.internal for it; " +
					"worth finding before it disagrees with voya"
			}
			detail += ". "
		}
		return DNSBroken, VerdictUnknown, detail + unaffected
	case p.Default.ThroughListener():
		detail += system + "resolved *.internal through the listener (" + p.System.summary() +
			"), and so did this Go binary's own resolver"
		if p.DefaultCGODisabled {
			detail += ", from a CGO_ENABLED=0 build"
		}
		detail += " — by wiring ADR 002 has not specified for this machine, so worth finding what " +
			"routes it before trusting it. " + foreignPureResolverResidue(p)
		return DNSWorking, VerdictUnknown, detail + unaffected
	case p.Default.Unmeasured != "":
		// The split this row would name, unmeasured on the binary's side
		// ([defaultUnmeasuredSplit], #258). The verdict is [VerdictUnknown]
		// here whatever the row, so what the unmeasured probe changes is the
		// state and the sentence, not the answer to ADR 005's question.
		detail += system + "resolved *.internal through the listener (" + p.System.summary() +
			"), by wiring ADR 002 has not specified for this machine, so worth finding what " +
			"routes it before trusting it. " + defaultUnmeasuredSplit(p) + " " +
			foreignPureResolverResidue(p)
		return DNSWorking, VerdictUnknown, detail + unaffected
	default:
		// The split ADR 002 documents, measured: the state is the divergence,
		// whatever wiring it arrived by, because every diagnostic the developer
		// reaches for reports success while the path their service uses fails.
		detail += system + "resolved *.internal through the listener (" + p.System.summary() +
			") while this Go binary's own resolver did not"
		if p.Default.answered() {
			detail += " — it answered with an address that is not loopback (" + p.Default.summary() +
				"), so another resolver is claiming *.internal for it"
		}
		detail += ": the split ADR 002 documents, arriving by wiring ADR 002 has not specified " +
			"for this machine. Every diagnostic reporting success is true of a path the service does " +
			"not use. "
		return DNSTrap, VerdictUnknown, detail + unaffected
	}
}

// ProbeDefault resolves name with this binary's own default resolver — run
// from a CGO_ENABLED=0 build, this is the measurement ADR 005 requires
// `voya dns check` to make.
func ProbeDefault(ctx context.Context, name string) ProbeResult {
	return probeWith(ctx, net.DefaultResolver, name)
}

// ProbePureGo resolves name with Go's pure resolver — the path -tags netgo
// and GODEBUG=netdns=go builds are on regardless of what the default
// resolver measured.
func ProbePureGo(ctx context.Context, name string) ProbeResult {
	return probeWith(ctx, &net.Resolver{PreferGo: true}, name)
}

// ProbeAt resolves name against one specific DNS server, bypassing the
// system's wiring entirely. It is the positive control for every probe
// above: the "no" the others answer is only evidence if the machinery they
// share can answer "yes" against a listener known to be up — and it is what
// separates "voya's listener is broken" from "the wiring to it is".
func ProbeAt(ctx context.Context, server, name string) ProbeResult {
	r := &net.Resolver{
		PreferGo: true,
		Dial:     dialProbeServer(server),
	}
	return probeWith(ctx, r, name)
}

// dialProbeServer is [ProbeAt]'s dial hook: every connection goes to server —
// redirecting where the query lands is the hook's whole job, so the address
// the resolver's own wiring chose is ignored — on whichever network the
// resolver asked for, because which transport a query uses is the resolver's
// half of the contract, not the hook's (#97).
func dialProbeServer(server string) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server)
	}
}

// probeWith is the lookup behind [ProbeDefault], [ProbePureGo] and [ProbeAt].
// A deadline that fires before the resolver answers is unmeasured, not a
// measured "no" — the rule [ProbeSystem] already follows for its tools
// (#100), and since #253 this one too: a port that black-holes UDP used to
// reach the not-answering row as an Err off the caller's fired budget, and
// the row then asserted nothing that is voya was answering there off a
// timeout, where what was measured is that the budget ran out. The context
// is consulted after the failure rather than before the call, so a lookup
// that failed on its own merits under a live deadline keeps its measurement.
func probeWith(ctx context.Context, r *net.Resolver, name string) ProbeResult {
	addrs, err := r.LookupHost(ctx, name)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ProbeResult{Unmeasured: "lookup " + name + ": " + ctxErr.Error()}
		}
		return ProbeResult{Err: err.Error()}
	}
	return ProbeResult{Addrs: addrs}
}

// systemProbeCmd is [exec.CommandContext], substitutable so [ProbeSystem]'s
// own failure modes — a tool that cannot run, a deadline that fires first —
// are testable without breaking a real host's dscacheutil (#100). probeOS is
// [runtime.GOOS] for the same reason (#155): which of the probes below a test
// can exercise must not be the build machine's choice, now that there is more
// than one.
var (
	systemProbeCmd = exec.CommandContext
	probeOS        = runtime.GOOS
)

// getentNotFound is the exit status getent(1) uses for "the key was not found
// in the database" — a measurement, and the one this probe most often makes:
// *.internal not resolving is exactly what `voya dns check` is asked about. Every
// other nonzero status (1 = bad usage or unknown database, 3 = enumeration
// unsupported) means the question never got asked, which is unmeasured.
const getentNotFound = 2

// ProbeSystem asks the operating system itself, the way curl and browsers do
// — which is the only probe that can see the wiring `voya dns install` writes,
// since it is the OS resolver that reads that file and not this binary.
//
// On macOS that is dscacheutil, which queries the same directory service
// getaddrinfo uses, including /etc/resolver. On Linux it is `getent ahosts`,
// which goes through NSS — the path getaddrinfo itself takes, so it sees
// nss-resolve and systemd-resolved's drop-ins, and equally a host wired with
// dnsmasq or a plain resolv.conf instead. `resolvectl query` was the
// alternative and is the wrong tool for this question: it talks to resolved
// over D-Bus, which is a bypass of NSS, so it would answer for a mechanism
// the system's own name resolution might not be using (#155).
//
// On an OS with neither, the path is reported unmeasured rather than guessed
// (#96): a probe that does not exist must never read as a measured "no".
//
// A tool that could not be run — or a deadline that fired before it answered
// — is unmeasured too, with the tool's own complaint quoted, bounded, because
// "not measured" is only actionable if the reason is (#100).
func ProbeSystem(ctx context.Context, name string) ProbeResult {
	switch probeOS {
	case "darwin":
		return probeDirectoryService(ctx, name)
	case "linux":
		return probeNameServiceSwitch(ctx, name)
	}
	return ProbeResult{Unmeasured: "no system-path probe on " + probeOS +
		" (the wiring ADR 002 specifies is darwin's /etc/resolver and Linux's systemd-resolved drop-in; #155)"}
}

// probeDirectoryService is macOS's half: dscacheutil prints one `ip_address:`
// or `ipv6_address:` line per address it found, and nothing at all for a name
// it did not resolve — which is a measured "no", not a failure to measure.
func probeDirectoryService(ctx context.Context, name string) ProbeResult {
	out, err := systemProbeCmd(ctx, "dscacheutil", "-q", "host", "-a", "name", name).Output()
	if err != nil {
		return ProbeResult{Unmeasured: unrunnable(ctx, "dscacheutil", err)}
	}
	var addrs []string
	for line := range strings.Lines(string(out)) {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "ip_address: "); ok {
			addrs = append(addrs, v)
		} else if v, ok := strings.CutPrefix(line, "ipv6_address: "); ok {
			addrs = append(addrs, v)
		}
	}
	if len(addrs) == 0 {
		return ProbeResult{Err: "dscacheutil returned no addresses for " + name}
	}
	return ProbeResult{Addrs: addrs}
}

// probeNameServiceSwitch is Linux's half. `getent ahosts` prints one line per
// address and socket type — `127.0.0.1 STREAM voya.internal`, then DGRAM and
// RAW for the same address — so the first field of each line is the address
// and the repeats are collapsed here rather than reported as three answers.
//
// A name NSS did not resolve exits [getentNotFound] with nothing on stdout,
// and that is the measurement `voya dns check` most needs to be able to make:
// treating it as a tool failure would turn every unwired host into "could not
// measure" and cost the report its whole Linux answer.
func probeNameServiceSwitch(ctx context.Context, name string) ProbeResult {
	out, err := systemProbeCmd(ctx, "getent", "ahosts", name).Output()
	if err != nil {
		var exit *exec.ExitError
		if ctx.Err() == nil && errors.As(err, &exit) && exit.ExitCode() == getentNotFound {
			return ProbeResult{Err: "getent ahosts " + name + ": not found by any name service"}
		}
		return ProbeResult{Unmeasured: unrunnable(ctx, "getent ahosts", err)}
	}
	var addrs []string
	for line := range strings.Lines(string(out)) {
		fields := strings.Fields(line)
		if len(fields) == 0 || slices.Contains(addrs, fields[0]) {
			continue
		}
		addrs = append(addrs, fields[0])
	}
	if len(addrs) == 0 {
		return ProbeResult{Err: "getent ahosts returned no addresses for " + name}
	}
	return ProbeResult{Addrs: addrs}
}

// unrunnable is why a probe's tool produced no measurement, in the words the
// report will carry. The context is consulted first: an expired deadline
// otherwise reads as the "signal: killed" of the process it took down (#100).
func unrunnable(ctx context.Context, tool string, err error) string {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return tool + ": " + ctxErr.Error()
	}
	reason := tool + ": " + err.Error()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if complaint := firstLine(exit.Stderr, 200); complaint != "" {
			reason += ": " + complaint
		}
	}
	return reason
}

// firstLine bounds a tool's stderr for quoting: the first non-blank line,
// capped at limit bytes. The string is pasted verbatim into a report a
// developer reads, and a screen of dscacheutil output would make the report
// less readable than the bare exit status it is there to improve on (#100).
//
// The cap backs up to a rune boundary rather than cutting at the byte: a
// localised tool's complaint is not ASCII, and half a rune reaches the report
// as a replacement character in the middle of the tool's own words — which is
// the same trade the cap exists to make, taken one step too far.
func firstLine(b []byte, limit int) string {
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
