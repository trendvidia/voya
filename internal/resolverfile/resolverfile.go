// Package resolverfile is the one home for ADR 002's resolver wiring: which
// file each operating system reads, where it lives, what voya writes there,
// and how voya tells its own file from one a developer wrote. `voya dns
// install` and `voya dns uninstall` write and remove the file through
// [Wiring.Install] and [Wiring.Uninstall]; `voya dns check` and `voya status`
// read its presence through [Path], and gate what that presence *means* on
// both halves of the question — [For], whether this OS has a wiring at all
// (#156), and [Wiring.Live], whether this machine is running the thing that
// reads it (#155). Neither half answers alone, so neither has a shorthand
// here: one that asked only the OS is what a caller would reach for, and it
// would reintroduce the drop-in nothing reads. The path lived in
// internal/dnscheck while nothing wrote the file; it moved here so the command
// that writes it and the check that reads it cannot disagree about where it
// is.
//
// Two operating systems have a wiring, and [For] is where the difference
// lives (#155):
//
//   - macOS reads /etc/resolver/internal, a scoped resolver: with it in
//     place, the system's resolution machinery — what curl, grpcurl and
//     browsers use — sends every *.internal query to voya's DNS listener,
//     which answers 127.0.0.1 (ADR 002). configd watches the directory, so
//     the file is live as soon as it is written.
//   - Linux reads /etc/systemd/resolved.conf.d/voya.conf, a systemd-resolved
//     drop-in whose `Domains=~internal` makes 127.0.0.1:<dns_port> the
//     resolver for that one domain and nothing else. It is live after
//     systemd-resolved restarts, which [Wiring.Activate] does, and it needs
//     the host to be running resolved at all — [Wiring.Preflight] is what
//     establishes that before anything is written.
//
// The file is the one piece of global state voya puts on a machine, which is
// why every write here is marked and every removal is refused for a file
// without the mark: ADR 002 rejected /etc/hosts management partly for the
// stale global state it leaves behind, and the same objection applies to a
// file voya writes unless voya can take back exactly what it put down and
// nothing else.
//
// One thing [Wiring.Uninstall] deliberately leaves: the directory the file
// sits in, which [Wiring.Install] creates when a machine has none. Nothing
// tells voya afterwards whether the directory was its own or the developer's,
// and an empty /etc/resolver or resolved.conf.d changes nothing about
// resolution, so removing it would risk "something else" for no gain. What it
// does take back is voya's own debris: the temporary files a killed install
// leaves in that directory (#172), which the sweep in [Wiring.Install] and
// [Wiring.Uninstall] removes.
package resolverfile

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/trendvidia/voya/internal/config"
)

const (
	// DarwinPath is the macOS scoped-resolver file that points *.internal at
	// voya's DNS listener (ADR 002).
	DarwinPath = "/etc/resolver/internal"

	// LinuxPath is the systemd-resolved drop-in that does the same on Linux
	// (#155). A drop-in rather than resolved.conf itself, because that file
	// is the distribution's; rather than `resolvectl dns`, because a per-link
	// setting does not survive a reboot or a NetworkManager reconfigure.
	LinuxPath = "/etc/systemd/resolved.conf.d/voya.conf"
)

// Path is the file this machine's wiring uses, and "" on an OS that has
// none. Its presence is read for exactly one distinction — "the DNS path was
// never installed" versus every other way resolution fails — and nothing
// more, because a file can exist and be wrong: whether the path works is
// answered by resolving, not by reading config (ADR 005).
var Path = Here().Path

// MarkerPrefix opens the first line of every file voya writes, and is how
// [Wiring.Install] and [Wiring.Uninstall] tell voya's file from a
// hand-written one: a file whose first line does not begin with it is
// refused, unread beyond that line and unwritten. It is the contract, and it
// is deliberately short so the rest of [Marker] can be reworded without
// orphaning files an earlier voya wrote — a rewording shows up as [Updated]
// on the next install, never as a refusal.
const MarkerPrefix = "# written by voya dns install"

// Marker is the first line [Wiring.Install] writes: [MarkerPrefix], then what
// a developer reading the file needs to know. A `#` line is a comment in the
// format resolver(5) describes for /etc/resolv.conf and /etc/resolver/* alike
// — that page's own EXAMPLES section is a file of `#` lines, and Apple's
// generated /etc/resolv.conf opens with a "# macOS Notice" — and it is a
// comment in systemd.syntax(7)'s ini format too, so it costs neither file
// anything.
const Marker = MarkerPrefix + "; voya dns uninstall removes it (a file without this line is left alone)"

// tempPrefix opens the name of every temporary file [write] creates. It is
// also what the sweep looks for: a kill between os.CreateTemp and os.Rename
// leaves one behind, in the one directory voya writes to, where macOS's
// configd reads it as a scoped resolver for a nonsense domain (#172).
const tempPrefix = ".voya-resolver-"

// sweepMinAge is how long a temporary file must have sat there before the
// sweep takes it for debris. [write]'s own file exists for the microseconds
// between os.CreateTemp and os.Rename, and the sweep runs in a second process
// as readily as in this one: two `sudo voya dns install` runs overlapping, or
// an uninstall crossing an install, would otherwise have one delete the
// other's file mid-write and leave it failing its rename with an error that
// names nothing the developer did. Debris a killed run left seconds ago
// survives the very next command and goes on the one after it, which is what
// this is worth trading.
const sweepMinAge = time.Minute

// Wiring is one operating system's resolver wiring: the file and the format
// ADR 002 specifies there, plus what that OS needs done to make a written
// file live. [For] is the only constructor — a zero Wiring renders nothing
// and installs nothing.
type Wiring struct {
	// GOOS is the operating system this wiring is for. It decides the file's
	// format and everything around it, so nothing here reads runtime.GOOS and
	// every row is testable from any machine.
	GOOS string
	// Path is the file voya writes. Settable so a test can point a whole
	// wiring at a temp directory, and so `voya dns install`'s own path seam
	// stays one assignment.
	Path string
}

// For is the wiring goos specifies, and false where ADR 002 specifies none —
// on which nothing may be written and nothing may be prescribed (#96).
func For(goos string) (Wiring, bool) {
	switch goos {
	case "darwin":
		return Wiring{GOOS: goos, Path: DarwinPath}, true
	case "linux":
		return Wiring{GOOS: goos, Path: LinuxPath}, true
	}
	return Wiring{GOOS: goos}, false
}

// Here is [For] for the machine this binary is running on. The bool is
// dropped on purpose: a caller that only wants the path gets "" where there
// is no wiring, and a caller that wants to know whether there is one calls
// [For] itself — with [Wiring.Live] beside it, since the OS is only half the
// question (#155).
func Here() Wiring {
	w, _ := For(runtime.GOOS)
	return w
}

// specified is the guard both write paths share: an OS ADR 002 names no
// wiring for has no file to act on, and a zero Wiring has no path — where
// filepath.Dir("") is ".", so an unguarded sweep would run in whatever
// directory the process happens to be in.
func (w Wiring) specified() error {
	if _, ok := For(w.GOOS); !ok {
		return fmt.Errorf("no resolver wiring for %s (#155)", w.GOOS)
	}
	if w.Path == "" {
		return fmt.Errorf("no resolver file for the %s wiring (#155)", w.GOOS)
	}
	return nil
}

// Render is the file voya writes for a DNS listener on port: [Marker] first,
// so voya can take the file back, then what the OS reads.
//
// On macOS that is resolver(5)'s nameserver and its own `port` line, rather
// than the `nameserver 127.0.0.1.9778` form: the same wiring, and the form a
// developer can read without the man page. On Linux it is a `[Resolve]`
// section with `DNS=127.0.0.1:<port>` and `Domains=~internal` — the leading
// `~` is what makes it a routing-only domain, so resolved sends *.internal to
// voya and every other name where it was already going. A wiring [For] never
// returns renders nothing, which is what stops [Wiring.Install] there.
func (w Wiring) Render(port uint32) []byte {
	switch w.GOOS {
	case "darwin":
		return []byte(Marker + "\nnameserver " + config.Loopback +
			"\nport " + strconv.FormatUint(uint64(port), 10) + "\n")
	case "linux":
		return []byte(Marker + "\n[Resolve]\nDNS=" + config.Addr(port) + "\nDomains=~internal\n")
	}
	return nil
}

// ServerOf is the server a file names: on macOS the address on the
// `nameserver` line and the port on the `port` line, on Linux the host and
// port of the first `DNS=` entry that names a port — or of the first entry
// at all, where none does. Either is "" when the file names none.
// Read back for two callers: [Wiring.Install] reports what an [Updated]
// install replaced, and `voya dns check` asks the port the file names whether
// voya is answering there — and asks only where the host is the loopback
// voya's listeners bind, so a hand-written file naming some other server is
// not measured as if it named voya's (#253).
func (w Wiring) ServerOf(contents []byte) (host, port string) {
	for line := range strings.Lines(string(contents)) {
		line = strings.TrimSpace(line)
		switch w.GOOS {
		case "darwin":
			if f := strings.Fields(line); len(f) == 2 {
				switch f[0] {
				case "nameserver":
					if host == "" {
						host = f[1]
					}
				case "port":
					if port == "" {
						port = f[1]
					}
				}
			}
		case "linux":
			if v, ok := strings.CutPrefix(line, "DNS="); ok {
				if f := strings.Fields(v); len(f) > 0 {
					// The first entry that names a port wins, host and port
					// together, and an entry naming none does not end the
					// scan: `DNS=` accumulates across the lines of a
					// hand-edited drop-in, and the port [Wiring.Install]
					// reports as replaced is the one some entry names — the
					// scan the port read-back made before it also read the
					// host back (#253). Where no entry names one, the first
					// entry's host is what the file names.
					if h, p, err := net.SplitHostPort(f[0]); err == nil {
						return h, p
					}
					if host == "" {
						host = f[0]
					}
				}
			}
		}
	}
	return host, port
}

// PortOf is the port a file voya wrote names — [Wiring.ServerOf]'s port.
func (w Wiring) PortOf(contents []byte) string {
	_, port := w.ServerOf(contents)
	return port
}

// IsVoyas reports whether contents is a file voya wrote: its first line
// begins with [MarkerPrefix]. Nothing else about the contents is judged —
// a developer who edited the body but kept the line has kept voya's claim on
// the file, and the next install rewrites it. The marker is the same on
// every OS, so a file carried between them is still voya's.
func IsVoyas(contents []byte) bool {
	first, _, _ := bytes.Cut(contents, []byte("\n"))
	return bytes.HasPrefix(first, []byte(MarkerPrefix))
}

// Outcome is what [Wiring.Install] or [Wiring.Uninstall] did, so the command
// can say so in one line.
type Outcome int

const (
	// Installed: there was no file, and voya wrote one.
	Installed Outcome = iota
	// Unchanged: the file voya would write is the file already there.
	Unchanged
	// Updated: voya's own file was there with different contents — another
	// port, or an earlier wording of [Marker] — and was rewritten.
	Updated
	// Removed: voya's own file was there and is gone.
	Removed
	// Absent: there was no file to remove.
	Absent
)

// String is the outcome as the commands print it.
func (o Outcome) String() string {
	switch o {
	case Installed:
		return "installed"
	case Unchanged:
		return "already installed"
	case Updated:
		return "updated"
	case Removed:
		return "removed"
	case Absent:
		return "not installed"
	}
	return fmt.Sprintf("Outcome(%d)", int(o))
}

// Changed reports whether the file on disk is different from what it was, so
// the command knows whether there is anything for [Wiring.Activate] to make
// live.
func (o Outcome) Changed() bool {
	return o == Installed || o == Updated || o == Removed
}

// Result is what one call did: the [Outcome], and for [Updated] the port the
// file named before, so the command can report the change rather than only
// the new state.
type Result struct {
	Outcome Outcome
	// PreviousPort is the value the rewritten file's port carried, or "" when
	// it had none or the outcome is not [Updated].
	PreviousPort string
}

// NotVoyasError is a refusal: the file at the wiring's path exists and its
// first line does not begin with [MarkerPrefix], so a developer wrote it and
// voya neither rewrites nor removes it. Contents is the whole file, so the
// message can show what voya found and left alone.
type NotVoyasError struct {
	Path     string
	Contents []byte
}

func (e *NotVoyasError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s is not voya's — its first line does not begin with %q, so voya leaves it alone; "+
		"remove it yourself to hand the file to voya, or keep it as your own wiring",
		e.Path, MarkerPrefix)
	// The way out is named because there is no other one: the file is refused
	// by install and by uninstall alike, so a message that only said what voya
	// would not do would leave the developer — including everyone whose
	// /etc/resolver/internal predates this command — with nothing to run.
	body := strings.TrimRight(string(e.Contents), "\n")
	if body == "" {
		// Nothing to show, and saying so beats trailing off after a colon: a
		// zero-byte file is not voya's either, since no first line begins with
		// the marker.
		return b.String() + ". The file is empty"
	}
	b.WriteString(". Its contents:")
	for line := range strings.Lines(body) {
		b.WriteString("\n  ")
		b.WriteString(strings.TrimRight(line, "\n"))
	}
	return b.String()
}

// Install writes [Wiring.Render](port) at the wiring's path, creating the
// directory above it when a machine has none — a Mac that never had a scoped
// resolver has no /etc/resolver, and a host whose resolved was never
// configured has no resolved.conf.d — and reports which of the three things
// it did: [Installed] when there was no file, [Unchanged] when the identical
// file was already there, [Updated] when voya's own file named something
// else. A file that is not voya's is refused with [NotVoyasError] and nothing
// is written. Any error reading, creating or writing is returned as it came.
//
// It does not make the file live: [Wiring.Activate] does, when the outcome
// [Outcome.Changed], and [Wiring.Preflight] is what establishes beforehand
// that this machine can carry the wiring at all.
func (w Wiring) Install(port uint32) (Result, error) {
	if err := w.specified(); err != nil {
		return Result{}, err
	}
	want := w.Render(port)
	have, err := os.ReadFile(w.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := makeDir(filepath.Dir(w.Path)); err != nil {
			return Result{}, err
		}
		defer sweep(filepath.Dir(w.Path))
		return Result{Outcome: Installed}, write(w.Path, want)
	case err != nil:
		return Result{}, err
	}
	defer sweep(filepath.Dir(w.Path))
	switch {
	case bytes.Equal(have, want):
		return Result{Outcome: Unchanged}, nil
	case !IsVoyas(have):
		return Result{}, &NotVoyasError{Path: w.Path, Contents: have}
	}
	return Result{Outcome: Updated, PreviousPort: w.PortOf(have)}, write(w.Path, want)
}

// Uninstall removes the file at the wiring's path when it is voya's —
// [Removed] — and reports [Absent] when there is none: taking back nothing is
// not a failure. A file that is not voya's is refused with [NotVoyasError]
// and left in place; a read or remove error is returned as it came. Either
// way voya's own leftovers in that directory go with it (#172).
//
// A wiring with no file — an OS ADR 002 specifies none for, or a zero Wiring —
// is refused before anything is read or swept, exactly as [Wiring.Install]
// refuses it: taking nothing back is a no-op, but sweeping the directory of ""
// is the process's working directory, which is nobody's to tidy.
func (w Wiring) Uninstall() (Result, error) {
	if err := w.specified(); err != nil {
		return Result{}, err
	}
	defer sweep(filepath.Dir(w.Path))
	have, err := os.ReadFile(w.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Result{Outcome: Absent}, nil
	case err != nil:
		return Result{}, err
	case !IsVoyas(have):
		return Result{}, &NotVoyasError{Path: w.Path, Contents: have}
	}
	return Result{Outcome: Removed}, os.Remove(w.Path)
}

// makeDir creates the directory the resolver file sits in when a machine has
// none, at 0755 like /etc itself: a resolver directory nobody but root can
// list is one the resolver cannot read, and one a later non-root `voya
// the check` cannot stat the file inside either — it would report the file
// unreadable straight after a successful install (#104). The mode is set
// again after creating it because os.MkdirAll's is masked by the process
// umask, which sudo takes from the invoking developer's shell: a `umask 077`
// there would otherwise leave /etc/resolver at 0700. A directory that is
// already there is left exactly as its owner made it.
func makeDir(dir string) error {
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		return err // nil when it is already there, the stat's own failure otherwise
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: system configuration is world-readable by convention
		return err
	}
	return os.Chmod(dir, 0o755) //nolint:gosec // G302: the same 0755, restated past the umask
}

// write puts contents at path through a temporary file in the same directory
// and a rename, so what a reader sees is either the old file or the new one:
// os.WriteFile truncates before it writes, and the one directory this package
// writes to is watched live by macOS's configd, so an interrupted write there
// would leave a zero-byte file — which by [IsVoyas] is not voya's, and which
// neither install nor uninstall would then touch.
//
// The mode is 0644, the mode of /etc/resolv.conf, of every hand-written
// /etc/resolver file and of systemd's own drop-ins: world-readable
// configuration owned by root. The 0600 gosec would prefer is for a file that
// carries something private, and this one carries a loopback address and a
// port number that `voya run` prints in its banner. It is set explicitly
// rather than at creation because os.CreateTemp opens 0600 and every create
// mode is masked by the umask besides.
func write(path string, contents []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), tempPrefix+"*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // debris only when the rename never happened
	if _, err := tmp.Write(contents); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil { //nolint:gosec // G302: world-readable by convention, as above
		return err
	}
	return os.Rename(name, path)
}

// sweep removes the temporary files voya left in dir. [write]'s deferred
// remove covers every error it returns, so what is left here is what a signal
// or a panic interrupted — rare, and permanent: the debris sits in the one
// directory voya writes to, configd reads it as a scoped resolver for a
// nonsense domain, and an uninstall that removed only `internal` left it
// there forever (#172). Both commands sweep, so the next run of either takes
// it back.
//
// Its failures are dropped rather than returned: this is housekeeping around
// the operation the developer asked for, and a directory voya cannot list is
// one where the debris is out of reach anyway — failing an install that wrote
// its file, or an uninstall that removed one, would be reporting the wrong
// thing. Nothing but the prefix voya's own [write] uses is touched, so a
// developer's files in the same directory are not voya's to remove, and
// nothing younger than [sweepMinAge] is touched either, so the one temporary
// file that is not debris — another voya's, mid-write — is not.
func sweep(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), tempPrefix) {
			continue
		}
		// A file voya cannot stat is one it cannot date, and an undated file
		// is left: the sweep is housekeeping, and the cost of leaving debris
		// for the next run is nothing beside the cost of removing a write in
		// flight.
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < sweepMinAge {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}
