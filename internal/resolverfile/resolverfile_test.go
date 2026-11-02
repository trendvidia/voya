package resolverfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trendvidia/voya/internal/config"
)

// wirings is every OS ADR 002 specifies a file for, so each table below runs
// on all of them: the marker contract, the idempotence rules and the refusals
// are one mechanism, and Linux joining macOS (#155) must not mean a second
// set of rules nobody tests.
func wirings(t *testing.T) []Wiring {
	t.Helper()
	return []Wiring{mustFor(t, "darwin"), mustFor(t, "linux")}
}

// mustFor is [For] with the test's own fatal for an OS that has no wiring:
// every table here is about the OSes that have one.
func mustFor(t *testing.T, goos string) Wiring {
	t.Helper()
	w, ok := For(goos)
	if !ok {
		t.Fatalf("For(%q) reports no wiring", goos)
	}
	return w
}

// at points a wiring at a file of its own name in a temp directory: the seam
// every row uses, so nothing here needs /etc or root.
func at(t *testing.T, w Wiring) Wiring {
	t.Helper()
	w.Path = filepath.Join(t.TempDir(), filepath.Base(w.Path))
	return w
}

// TestInstallAndUninstall is every row the two commands can land on, each
// against a file the row put in a temp directory first (#156), on each OS
// that has a wiring (#155): what was there, what the call did, and what is
// there afterwards. The table is swept below so a row cannot vanish
// unnoticed.
func TestInstallAndUninstall(t *testing.T) {
	const handWritten = "nameserver 127.0.0.1\nport 9778\n"
	for _, base := range wirings(t) {
		w := base
		rows := []struct {
			name string
			// before is the file's contents before the call; "" for no file.
			before string
			// call is what to run against the wiring.
			call func(w Wiring) (Result, error)
			// outcome is expected when the call succeeds; refused is expected
			// instead when the file is not voya's.
			outcome Outcome
			refused bool
			// after is the file's contents after the call; "" for no file.
			after        string
			previousPort string
		}{
			{name: "fresh install writes what the OS reads",
				before: "", call: install(9778), outcome: Installed,
				after: string(w.Render(9778))},
			{name: "second install of the same file is a no-op that says so",
				before: string(w.Render(9778)), call: install(9778), outcome: Unchanged,
				after: string(w.Render(9778))},
			{name: "voya's file with another port is rewritten and the old port reported",
				before: string(w.Render(9777)), call: install(9778), outcome: Updated, previousPort: "9777",
				after: string(w.Render(9778))},
			{name: "voya's file under an earlier marker wording is rewritten",
				before: MarkerPrefix + " (an older voya)\n" + trimFirstLine(string(w.Render(9778))), call: install(9778),
				outcome: Updated, previousPort: "9778",
				after: string(w.Render(9778))},
			{name: "a hand-written file is refused unchanged by install",
				before: handWritten, call: install(9778), refused: true,
				after: handWritten},
			{name: "a hand-written file with a comment that is not the marker is refused",
				before: "# my resolver\n" + handWritten, call: install(9778), refused: true,
				after: "# my resolver\n" + handWritten},
			{name: "uninstall removes voya's file",
				before: string(w.Render(9778)), call: Wiring.Uninstall, outcome: Removed,
				after: ""},
			{name: "uninstall removes voya's file whatever port it names",
				before: string(w.Render(5353)), call: Wiring.Uninstall, outcome: Removed,
				after: ""},
			{name: "uninstall refuses a hand-written file and leaves it",
				before: handWritten, call: Wiring.Uninstall, refused: true,
				after: handWritten},
			{name: "uninstall with no file is a clean no-op",
				before: "", call: Wiring.Uninstall, outcome: Absent,
				after: ""},
		}
		for _, tc := range rows {
			t.Run(w.GOOS+"/"+tc.name, func(t *testing.T) {
				w := at(t, w)
				if tc.before != "" {
					if err := os.WriteFile(w.Path, []byte(tc.before), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				res, err := tc.call(w)
				var refused *NotVoyasError
				switch {
				case tc.refused && !errors.As(err, &refused):
					t.Fatalf("got %+v, %v; want a NotVoyasError", res, err)
				case tc.refused:
					if refused.Path != w.Path || string(refused.Contents) != tc.before {
						t.Errorf("the refusal names %q with contents %q; want %q with %q", refused.Path, refused.Contents, w.Path, tc.before)
					}
					msg := refused.Error()
					if !strings.Contains(msg, w.Path) {
						t.Errorf("the refusal's message does not name the path:\n%s", msg)
					}
					for line := range strings.Lines(tc.before) {
						if !strings.Contains(msg, "\n  "+strings.TrimRight(line, "\n")+"\n") && !strings.HasSuffix(msg, "\n  "+strings.TrimRight(line, "\n")) {
							t.Errorf("the refusal's message does not show the file's line %q:\n%s", line, msg)
						}
					}
				case err != nil:
					t.Fatalf("unexpected error: %v", err)
				case res.Outcome != tc.outcome || res.PreviousPort != tc.previousPort:
					t.Errorf("got %+v, want outcome %v with previous port %q", res, tc.outcome, tc.previousPort)
				}
				after, err := os.ReadFile(w.Path)
				switch {
				case errors.Is(err, os.ErrNotExist):
					after = nil
				case err != nil:
					t.Fatal(err)
				}
				if string(after) != tc.after {
					t.Errorf("after the call the file is %q, want %q", after, tc.after)
				}
			})
		}
		// The sweep: the table must cover every outcome and the refusal, on both
		// calls, or a row deleted above takes its assertion with it silently.
		seen := map[string]bool{}
		for _, tc := range rows {
			if tc.refused {
				seen["refused"] = true
			} else {
				seen[tc.outcome.String()] = true
			}
		}
		for _, o := range []Outcome{Installed, Unchanged, Updated, Removed, Absent} {
			if !seen[o.String()] {
				t.Errorf("%s: no row lands on %v", w.GOOS, o)
			}
		}
		if !seen["refused"] {
			t.Errorf("%s: no row is refused", w.GOOS)
		}
	}
}

func install(port uint32) func(Wiring) (Result, error) {
	return func(w Wiring) (Result, error) { return w.Install(port) }
}

// trimFirstLine drops a rendering's marker line, so a row can put an older
// marker in front of the body this OS reads without restating the body.
func trimFirstLine(s string) string {
	_, rest, _ := strings.Cut(s, "\n")
	return rest
}

// A machine that never had the wiring has no directory for it — a Mac with no
// scoped resolver has no /etc/resolver, a host whose resolved was never
// configured has no resolved.conf.d: install creates it, and uninstall leaves
// it (the package doc says why).
func TestInstallCreatesTheDirectoryAndUninstallLeavesIt(t *testing.T) {
	for _, base := range wirings(t) {
		t.Run(base.GOOS, func(t *testing.T) {
			w := base
			dir := filepath.Join(t.TempDir(), "wiring")
			w.Path = filepath.Join(dir, filepath.Base(base.Path))
			if res, err := w.Install(9778); err != nil || res.Outcome != Installed {
				t.Fatalf("Install into a missing directory: %+v, %v", res, err)
			}
			if res, err := w.Uninstall(); err != nil || res.Outcome != Removed {
				t.Fatalf("Uninstall: %+v, %v", res, err)
			}
			if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
				t.Errorf("the directory install created is gone after uninstall: %v", err)
			}
		})
	}
}

// A zero-byte file is not voya's — no first line of it begins with the
// marker — so both calls refuse it and leave it exactly where it is, and the
// refusal says the file is empty rather than trailing off after a colon with
// nothing to show. Reachable without anyone writing one by hand: an install
// interrupted between creating the file and filling it.
func TestAnEmptyFileIsRefusedAndSaidToBeEmpty(t *testing.T) {
	for name, call := range map[string]func(Wiring) (Result, error){"install": install(9778), "uninstall": Wiring.Uninstall} {
		t.Run(name, func(t *testing.T) {
			w := at(t, mustFor(t, "darwin"))
			if err := os.WriteFile(w.Path, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			res, err := call(w)
			var refused *NotVoyasError
			if !errors.As(err, &refused) {
				t.Fatalf("got %+v, %v; want a NotVoyasError", res, err)
			}
			if msg := refused.Error(); !strings.HasSuffix(msg, "The file is empty") {
				t.Errorf("the refusal does not say the file is empty:\n%s", msg)
			}
			if fi, err := os.Stat(w.Path); err != nil || fi.Size() != 0 {
				t.Errorf("the refused file is not where it was: %v", err)
			}
		})
	}
}

// Every refusal names what the developer can do about it, because voya
// refuses the file from both sides: nothing voya offers will touch it again.
func TestTheRefusalSaysHowToGetOutOfIt(t *testing.T) {
	msg := (&NotVoyasError{Path: DarwinPath, Contents: []byte("nameserver 127.0.0.1\n")}).Error()
	if !strings.Contains(msg, "remove it yourself") {
		t.Errorf("the refusal offers no way forward:\n%s", msg)
	}
}

// Render is what each system will read: the marker first, then the lines that
// OS's own machinery parses, and nothing else. Pinned literally, because a
// change to either file's shape is a change to what the OS does with it — and
// on Linux `Domains=~internal` without its leading `~` would make voya the
// resolver for every name on the machine rather than for *.internal.
func TestRenderIsTheMarkerAndTheOSsOwnLines(t *testing.T) {
	for _, tc := range []struct {
		goos string
		want string
	}{
		{"darwin", Marker + "\nnameserver 127.0.0.1\nport 9778\n"},
		{"linux", Marker + "\n[Resolve]\nDNS=127.0.0.1:9778\nDomains=~internal\n"},
	} {
		w := mustFor(t, tc.goos)
		if got := string(w.Render(9778)); got != tc.want {
			t.Errorf("%s Render(9778) =\n%s\nwant\n%s", tc.goos, got, tc.want)
		}
		if !IsVoyas(w.Render(1)) {
			t.Errorf("%s: IsVoyas does not recognise what Render writes", tc.goos)
		}
		if IsVoyas([]byte("nameserver 127.0.0.1\n" + Marker + "\n")) {
			t.Error("IsVoyas accepts the marker on a line other than the first")
		}
		if got := w.PortOf(w.Render(5353)); got != "5353" {
			t.Errorf("%s: PortOf(Render(5353)) = %q, want \"5353\"", tc.goos, got)
		}
		if got := w.PortOf([]byte(Marker + "\n")); got != "" {
			t.Errorf("%s: PortOf on a file naming no port = %q, want \"\"", tc.goos, got)
		}
		if host, port := w.ServerOf(w.Render(5353)); host != config.Loopback || port != "5353" {
			t.Errorf("%s: ServerOf(Render(5353)) = %q, %q, want %q, \"5353\"", tc.goos, host, port, config.Loopback)
		}
	}
	if !strings.HasPrefix(Marker, "# ") {
		t.Errorf("Marker %q is not a comment line in either format", Marker)
	}
	// An OS with no wiring renders nothing, and install says so rather than
	// writing an empty file the OS would read as something.
	w, ok := For("windows")
	if ok || len(w.Render(9778)) != 0 {
		t.Errorf("For(%q) = %+v, %v; want no wiring and no rendering", "windows", w, ok)
	}
	w.Path = filepath.Join(t.TempDir(), "voya.conf")
	if _, err := w.Install(9778); err == nil || !strings.Contains(err.Error(), "no resolver wiring for windows") {
		t.Errorf("Install on an OS with no wiring = %v, want a refusal naming the OS", err)
	}
	if _, err := os.Stat(w.Path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Install on an OS with no wiring wrote %s", w.Path)
	}
}

// TestServerOfReadsTheHostBesideThePort pins the read-back `voya dns check`
// dials by (#253): a hand-written file naming some server other than
// loopback must come back naming it, so the dial is not made at 127.0.0.1
// in its place. Each OS's own shapes: macOS's two lines, in either order and
// either alone; Linux's `DNS=` with a port, without one, in brackets, and
// as the first of a list — and, since `DNS=` accumulates across a drop-in's
// lines, an entry naming no port followed by one that does.
func TestServerOfReadsTheHostBesideThePort(t *testing.T) {
	for _, tc := range []struct {
		goos, file, host, port string
	}{
		{"darwin", Marker + "\nnameserver 10.0.0.53\nport 5353\n", "10.0.0.53", "5353"},
		{"darwin", "port 5353\nnameserver 10.0.0.53\n", "10.0.0.53", "5353"},
		{"darwin", Marker + "\nport 5353\n", "", "5353"},
		{"darwin", Marker + "\nnameserver 10.0.0.53\n", "10.0.0.53", ""},
		{"darwin", Marker + "\n", "", ""},
		{"linux", Marker + "\n[Resolve]\nDNS=10.0.0.53:5353\nDomains=~internal\n", "10.0.0.53", "5353"},
		{"linux", "[Resolve]\nDNS=10.0.0.53\n", "10.0.0.53", ""},
		{"linux", "[Resolve]\nDNS=[::1]:5353 10.0.0.53\n", "::1", "5353"},
		{"linux", "[Resolve]\nDNS=127.0.0.1\nDNS=127.0.0.1:9779\n", "127.0.0.1", "9779"},
		{"linux", "[Resolve]\nDNS=10.0.0.53\nDNS=8.8.8.8\n", "10.0.0.53", ""},
		{"linux", "[Resolve]\nDNS=\n", "", ""},
		{"linux", Marker + "\n", "", ""},
	} {
		w := mustFor(t, tc.goos)
		host, port := w.ServerOf([]byte(tc.file))
		if host != tc.host || port != tc.port {
			t.Errorf("%s: ServerOf(%q) = %q, %q, want %q, %q", tc.goos, tc.file, host, port, tc.host, tc.port)
		}
		if got := w.PortOf([]byte(tc.file)); got != tc.port {
			t.Errorf("%s: PortOf(%q) = %q, want %q", tc.goos, tc.file, got, tc.port)
		}
	}
}

// The OS gate has one home, and this is the answer it gives: the two OSes
// ADR 002 specifies a file for, and nothing else (#155).
func TestForIsTheTwoOSesWithAWiring(t *testing.T) {
	for goos, want := range map[string]bool{"darwin": true, "linux": true, "windows": false, "freebsd": false} {
		w, ok := For(goos)
		if ok != want || (w.Path != "") != want {
			t.Errorf("For(%q) = %+v, %v; want a path only where the wiring is specified", goos, w, ok)
		}
	}
	if Path != Here().Path {
		t.Errorf("Path = %q, but this machine's wiring is %q", Path, Here().Path)
	}
}

// Neither call acts on a wiring that names no file, and uninstall is the one
// that had nothing stopping it: filepath.Dir("") is ".", so an unguarded
// uninstall swept the directory the process happened to be in — voya's own
// temporary files there, but nobody asked it to tidy that directory, and on a
// developer's machine it is wherever they ran the binary from.
func TestNeitherCallActsOnAWiringWithNoFile(t *testing.T) {
	for name, call := range map[string]func(Wiring) (Result, error){"install": install(9778), "uninstall": Wiring.Uninstall} {
		for _, tc := range []struct {
			row  string
			w    Wiring
			says string
		}{
			{row: "an OS with no wiring", w: Wiring{GOOS: "windows", Path: filepath.Join(t.TempDir(), "voya.conf")},
				says: "no resolver wiring for windows"},
			{row: "a zero Wiring", w: Wiring{}, says: "no resolver wiring for"},
			{row: "a wiring whose path was never set", w: Wiring{GOOS: "darwin"},
				says: "no resolver file for the darwin wiring"},
		} {
			w := tc.w
			t.Run(name+"/"+tc.row, func(t *testing.T) {
				// The sweep's own target, planted where an unguarded call would
				// reach it: the working directory for the wiring with no path.
				dir := t.TempDir()
				t.Chdir(dir)
				debris := filepath.Join(dir, tempPrefix+"2718281828")
				if err := os.WriteFile(debris, []byte("someone's\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				old := time.Now().Add(-time.Hour)
				if err := os.Chtimes(debris, old, old); err != nil {
					t.Fatal(err)
				}
				res, err := call(w)
				if err == nil || !strings.Contains(err.Error(), tc.says) {
					t.Fatalf("%s on %+v = %+v, %v; want an error containing %q", name, w, res, err, tc.says)
				}
				if _, err := os.Stat(debris); err != nil {
					t.Errorf("%s swept a directory it was never pointed at: %v", name, err)
				}
			})
		}
	}
}

// A kill between os.CreateTemp and os.Rename leaves a temporary file in the
// one directory voya writes to — where configd reads it as a scoped resolver
// for a nonsense domain, and where nothing used to remove it (#172). Both
// commands sweep it, and neither touches anything else in the directory: not a
// file that is not voya's, and not a temporary file young enough to be another
// voya's write in flight rather than a dead one's debris.
func TestBothCommandsSweepVoyasLeftoverTemporaryFiles(t *testing.T) {
	for name, call := range map[string]func(Wiring) (Result, error){
		"install":   install(9778),
		"uninstall": Wiring.Uninstall,
	} {
		t.Run(name, func(t *testing.T) {
			w := at(t, mustFor(t, "darwin"))
			dir := filepath.Dir(w.Path)
			debris := filepath.Join(dir, tempPrefix+"3141592653")
			inFlight := filepath.Join(dir, tempPrefix+"1414213562")
			theirs := filepath.Join(dir, "someone-elses")
			for _, f := range []string{debris, inFlight, theirs} {
				if err := os.WriteFile(f, []byte("leftover\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			// Debris is what an interrupted run left behind, so it is dated as
			// such; inFlight keeps the mtime it was just written with, which is
			// what another install's temporary file has between CreateTemp and
			// Rename.
			old := time.Now().Add(-2 * sweepMinAge)
			if err := os.Chtimes(debris, old, old); err != nil {
				t.Fatal(err)
			}
			// A file of voya's own to act on, so the sweep is not the only
			// thing the call does.
			if err := os.WriteFile(w.Path, w.Render(9778), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := call(w); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if _, err := os.Stat(debris); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s left voya's temporary file behind: %v", name, err)
			}
			if _, err := os.Stat(inFlight); err != nil {
				t.Errorf("%s removed a temporary file young enough to be another install's write in flight: %v", name, err)
			}
			if _, err := os.Stat(theirs); err != nil {
				t.Errorf("%s removed a file that is not voya's: %v", name, err)
			}
		})
	}
}
