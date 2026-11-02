package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trendvidia/voya/internal/config"
	"github.com/trendvidia/voya/internal/dnscheck"
	"github.com/trendvidia/voya/internal/resolverfile"
)

func TestDNSCheckRejectsExtraArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"dns", "check", "extra-junk"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "extra-junk") {
		t.Fatalf("`voya dns check extra-junk` did not fail naming the argument: %v", err)
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Errorf("no usage on stderr:\n%s", stderr.String())
	}
}

// TestDNSCheckReportsAndExitsByItsVerdict runs the real command against this
// host, whatever its DNS state and whatever holds 9777: the assertions are
// the invariants every verdict shares — the report carries every check
// with its evidence on stdout, ends with the aggregate verdict, says
// nothing on stderr, and the error run returns is exactly the printed
// verdict's exit code (#99). Which verdict comes out is the host's to decide, and the log
// records it.
func TestDNSCheckReportsAndExitsByItsVerdict(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"dns", "check"}, &stdout, &stderr)
	out := stdout.String()

	word := verdictExitsAsPrinted(t, out, err)
	carries(t, "\n"+out, "\ndns: ", "measured:")
	if stderr.Len() != 0 {
		t.Errorf("`voya dns check` wrote to stderr; the report is stdout's product: %q", stderr.String())
	}
	t.Logf("this host's verdict: %s\n%s", word, out)
}

// carries fails unless the report holds every want, naming the first one it
// does not and printing the report that missed it.
func carries(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not carry %q:\n%s", want, out)
		}
	}
}

// printedVerdict reads the aggregate verdict word from the report's last
// line, and its exit code, or fails.
func printedVerdict(t *testing.T, out string) (word string, code int) {
	t.Helper()
	verdicts := map[string]int{"ok": 0, "problem": 1, "unknown": 2}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	last := lines[len(lines)-1]
	for w, c := range verdicts {
		if last == "verdict: "+w {
			return w, c
		}
	}
	t.Fatalf("the report does not end with the aggregate verdict:\n%s", out)
	return "", 0
}

// verdictExitsAsPrinted reads the aggregate verdict the report ends with and
// holds err to exactly that verdict's exit status — the code is the printed
// verdict's product and nobody else's (#99, #104) — returning the word so
// callers can log which row the host exercised.
func verdictExitsAsPrinted(t *testing.T, out string, err error) string {
	t.Helper()
	word, wantCode := printedVerdict(t, out)
	if wantCode == 0 {
		if err != nil {
			t.Fatalf("verdict %q must exit 0, got error %v", word, err)
		}
		return word
	}
	var status *exitStatus
	if !errors.As(err, &status) {
		t.Fatalf("verdict %q returned %v, want an exitStatus", word, err)
	}
	if status.code != wantCode || status.err != nil {
		t.Fatalf("verdict %q returned code %d (err %v), want %d with no message beyond the report",
			word, status.code, status.err, wantCode)
	}
	return word
}

// substituteResolverFileSeams points dnsCheckCmd's two file checks at the
// test's own answers and restores them on cleanup: which OS the test machine
// runs must not decide which of #104's rows can be exercised.
func substituteResolverFileSeams(t *testing.T, reads bool, installed func() (bool, error)) {
	t.Helper()
	restoreGOOS, restoreInstalled, restoreLive := goos, resolverInstalled, wiringLive
	restoreCatchAll, restoreThrough, restoreTooOld := wiringGlobalCatchAll, wiringResolvesThroughResolved, wiringResolvedTooOld
	restoreRead, restoreProbeAt := readResolverFile, probeAt
	t.Cleanup(func() {
		goos, resolverInstalled, wiringLive = restoreGOOS, restoreInstalled, restoreLive
		wiringGlobalCatchAll, wiringResolvesThroughResolved, wiringResolvedTooOld = restoreCatchAll, restoreThrough, restoreTooOld
		readResolverFile, probeAt = restoreRead, restoreProbeAt
	})
	// The file-port dial (#243) sits behind the same gate, and a test of the
	// file's rows must neither read the host's real file nor dial its port:
	// unmeasured, so the row asks its question as prose.
	// TestDNSCheckDialsThePortTheFileNames substitutes its own.
	readResolverFile = func() ([]byte, error) { return nil, errors.New("not read by this test") }
	probeAt = func(context.Context, string, string) dnscheck.ProbeResult {
		return dnscheck.ProbeResult{Unmeasured: "not dialled by this test"}
	}
	// The catch-all question (#185) sits behind the same gate, as do
	// whether the host resolves through resolved at all (#204) and whether
	// its resolved is too old to be told a port (#206), and a test of the
	// file's rows is not a test of any: unmeasured, as off Linux for real.
	// TestDNSCheckAsksTheCatchAllOnlyWhereTheFileIsRead,
	// TestDNSCheckAsksWhetherTheHostResolvesThroughResolvedOnlyWhereTheFileIsRead
	// and TestDNSCheckAsksTheResolvedVersionOnlyWhereTheFileIsRead substitute
	// their own.
	wiringGlobalCatchAll = func(resolverfile.Wiring, context.Context) (bool, error) {
		return false, errors.New("not asked by this test")
	}
	wiringResolvesThroughResolved = func(resolverfile.Wiring) (bool, string, error) {
		return false, "", errors.New("not asked by this test")
	}
	wiringResolvedTooOld = func(resolverfile.Wiring, context.Context) (bool, string, error) {
		return false, "", errors.New("not asked by this test")
	}
	// Any OS that does not read the file stands for all of them; the gate is
	// resolverfile.For's, and its own test pins which OS that is. The machine
	// answers with it, since #155 made the gate two questions and either "no"
	// is the same "no stat was made" here — TestTheResolverFileGateIsTheOSAndTheMachine
	// is where the two are told apart.
	goos = "windows"
	if reads {
		goos = "darwin"
	}
	wiringLive = func(resolverfile.Wiring, context.Context) bool { return reads }
	resolverInstalled = installed
}

// TestTheResolverFileGateIsTheOSAndTheMachine pins what #155 added to #96's
// rule: the file is worth a word only where something reads it, and on Linux
// that is a service a host may not be running. An OS with no wiring and a
// Linux without systemd-resolved are the same silence, reached two different
// ways, and macOS never asks the machine at all because configd is the OS.
func TestTheResolverFileGateIsTheOSAndTheMachine(t *testing.T) {
	for _, tc := range []struct {
		goos string
		live bool
		want bool
		asks bool // whether the machine is asked at all
	}{
		{goos: "darwin", live: true, want: true, asks: true},
		{goos: "linux", live: true, want: true, asks: true},
		{goos: "linux", live: false, want: false, asks: true},
		{goos: "windows", live: true, want: false},
		{goos: "freebsd", live: true, want: false},
	} {
		t.Run(fmt.Sprintf("%s/live=%v", tc.goos, tc.live), func(t *testing.T) {
			restoreGOOS, restoreLive := goos, wiringLive
			t.Cleanup(func() { goos, wiringLive = restoreGOOS, restoreLive })
			asked := ""
			goos = tc.goos
			wiringLive = func(w resolverfile.Wiring, _ context.Context) bool { asked = w.GOOS; return tc.live }

			if got := osReadsResolverFile(context.Background()); got != tc.want {
				t.Errorf("osReadsResolverFile() on %s (resolved live: %v) = %v, want %v",
					tc.goos, tc.live, got, tc.want)
			}
			// An OS with no wiring has no machine question to ask: asking one
			// would mean shelling out to systemctl on a host that has none.
			if got := asked != ""; got != tc.asks {
				t.Errorf("the gate asked the machine: %v (as %q), want %v", got, asked, tc.asks)
			}
			if tc.asks && asked != tc.goos {
				t.Errorf("the gate asked about %q while goos is %q", asked, tc.goos)
			}
		})
	}
}

// TestDNSCheckOffTheReadingOSNeverConsultsTheFile pins #104's first half at the
// gate's seam: off the OS that reads /etc/resolver/internal the stat is not
// made at all — the file is inert there (#96), so no stat result, least of
// all a failure, may reach the exit path — and the exit code stays the
// printed verdict's.
//
// And what the gate leaves behind is a state nobody established, which the
// report may not spend: not statting the file is not measuring its absence,
// so `resolver-not-installed` and the remedy naming that file are both off
// the table here whatever the probes found. Without that, gating the stat
// printed the file's absence on every non-darwin host, file or no file.
func TestDNSCheckOffTheReadingOSNeverConsultsTheFile(t *testing.T) {
	// Handed a stat that would have failed: the assertion is that it is
	// never asked, so its answer must not matter.
	consulted := false
	substituteResolverFileSeams(t, false, func() (bool, error) {
		consulted = true
		return false, errors.New("permission denied")
	})

	var stdout, stderr bytes.Buffer
	err := run([]string{"dns", "check"}, &stdout, &stderr)
	out := stdout.String()
	if consulted {
		t.Errorf("dnsCheckCmd consulted %s on an OS that never reads it", resolverfile.Path)
	}
	word := verdictExitsAsPrinted(t, out, err)
	for _, banned := range []string{"resolver-not-installed", "is not installed", "is present",
		"point " + resolverfile.Path} {
		if strings.Contains(out, banned) {
			t.Errorf("the report says %q of a file it never looked at:\n%s", banned, out)
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("`voya dns check` wrote to stderr; the report is stdout's product: %q", stderr.String())
	}
	t.Logf("verdict off the reading OS: %s\n%s", word, out)
}

// TestDNSCheckAsksTheCatchAllOnlyWhereTheFileIsRead pins #185's seam at
// dnsCheckCmd's gate: resolved's global routing is a question about the service
// that reads the file, so it is asked exactly where the file is, and its
// answer reaches the classifier. Both halves are the test's to decide rather
// than the host's: the system probe is substituted with the silence that puts
// the report on the one row the answer is for — the file installed and the
// system path not resolving through voya — so a machine where *.internal
// already resolves cannot quietly skip the half that proves the answer
// arrives.
func TestDNSCheckAsksTheCatchAllOnlyWhereTheFileIsRead(t *testing.T) {
	for _, reads := range []bool{false, true} {
		t.Run(fmt.Sprintf("reads=%v", reads), func(t *testing.T) {
			substituteResolverFileSeams(t, reads, func() (bool, error) { return true, nil })
			restoreProbe := probeSystem
			t.Cleanup(func() { probeSystem = restoreProbe })
			probeSystem = func(context.Context, string) dnscheck.ProbeResult {
				return dnscheck.ProbeResult{Err: "lookup " + dnsCheckProbeName + ": no such host"}
			}
			asked := ""
			wiringGlobalCatchAll = func(w resolverfile.Wiring, _ context.Context) (bool, error) { asked = w.GOOS; return true, nil }

			var stdout, stderr bytes.Buffer
			err := run([]string{"dns", "check"}, &stdout, &stderr)
			out := stdout.String()
			word := verdictExitsAsPrinted(t, out, err)
			if got := asked != ""; got != reads {
				t.Errorf("dnsCheckCmd asked about the catch-all: %v (as %q), want %v", got, asked, reads)
			}
			if reads && asked != goos {
				t.Errorf("dnsCheckCmd asked about %q while goos is %q", asked, goos)
			}
			catchAll := strings.Contains(out, "The file is not the cause")
			notAnswering := strings.Contains("\n"+out, "\ndns: problem — not-answering\n")
			// With the system path silent and the file installed, the row is
			// the classifier's arithmetic, not the machine's — except where
			// nothing reads the file, which is a row of its own (#104).
			if want := reads; notAnswering != want {
				t.Errorf("not-answering row: %v, want %v:\n%s", notAnswering, want, out)
			}
			if catchAll != reads {
				t.Errorf("the report names the catch-all: %v, want %v (asked: %q):\n%s",
					catchAll, reads, asked, out)
			}
			if stderr.Len() != 0 {
				t.Errorf("`voya dns check` wrote to stderr; the report is stdout's product: %q", stderr.String())
			}
			t.Logf("verdict %s\n%s", word, out)
		})
	}
}

// TestDNSCheckDialsThePortTheFileNames pins #243's measurement at dnsCheckCmd's
// gate: where the file is installed, the check reads the port back out of the
// file and dials that server on loopback, so the not-answering row's "is
// voya answering there?" is the dial's answer rather than a dig the
// developer is sent to run — and where the file cannot be read, no dial is
// made and the row keeps its question. The system probe is substituted with
// the silence that puts the report on that row.
func TestDNSCheckDialsThePortTheFileNames(t *testing.T) {
	substituteResolverFileSeams(t, true, func() (bool, error) { return true, nil })
	restoreProbe := probeSystem
	t.Cleanup(func() { probeSystem = restoreProbe })
	probeSystem = func(context.Context, string) dnscheck.ProbeResult {
		return dnscheck.ProbeResult{Err: "lookup " + dnsCheckProbeName + ": no such host"}
	}
	w, ok := resolverfile.For(goos)
	if !ok {
		t.Fatalf("no wiring for %q; substituteResolverFileSeams must pick an OS that reads the file", goos)
	}
	readResolverFile = func() ([]byte, error) { return w.Render(5353), nil }
	dialled, asked := "", ""
	probeAt = func(_ context.Context, server, name string) dnscheck.ProbeResult {
		dialled, asked = server, name
		return dnscheck.ProbeResult{Addrs: []string{"127.0.0.1"}}
	}

	var stdout, stderr bytes.Buffer
	err := run([]string{"dns", "check"}, &stdout, &stderr)
	out := stdout.String()
	verdictExitsAsPrinted(t, out, err)
	if dialled != "127.0.0.1:5353" || asked != dnsCheckProbeName {
		t.Errorf("the check dialled %q for %q, want the file's port 127.0.0.1:5353 for %q",
			dialled, asked, dnsCheckProbeName)
	}
	if !strings.Contains(out, "voya is answering on the port the file names") {
		t.Errorf("the row does not state the dial's answer:\n%s", out)
	}
	if strings.Contains(out, "Is voya running?") {
		t.Errorf("the row asks the question the dial just answered:\n%s", out)
	}

	// A file that cannot be read back names no port: nothing is dialled, and
	// the row asks its question as prose rather than quoting a measurement
	// nobody made.
	readResolverFile = func() ([]byte, error) { return nil, errors.New("open: permission denied") }
	dialled = ""
	stdout.Reset()
	err = run([]string{"dns", "check"}, &stdout, &stderr)
	out = stdout.String()
	verdictExitsAsPrinted(t, out, err)
	if dialled != "" {
		t.Errorf("the check dialled %q off a file it could not read", dialled)
	}
	if !strings.Contains(out, "Is voya running?") {
		t.Errorf("the row does not ask the question no dial answered:\n%s", out)
	}
	if stderr.Len() != 0 {
		t.Errorf("`voya dns check` wrote to stderr; the report is stdout's product: %q", stderr.String())
	}
}

// TestDNSCheckDoesNotDialAServerTheFileNamesOffLoopback pins the other half
// of the read-back (#253): the dial is made on 127.0.0.1, the only address
// voya's listeners bind, so a hand-written file naming some other server —
// which `voya dns install` refuses to overwrite, so it persists — is not
// dialled at 127.0.0.1 in its place. No dial is made, the row keeps its
// prose question, and the evidence line says which address the file names.
func TestDNSCheckDoesNotDialAServerTheFileNamesOffLoopback(t *testing.T) {
	substituteResolverFileSeams(t, true, func() (bool, error) { return true, nil })
	restoreProbe := probeSystem
	t.Cleanup(func() { probeSystem = restoreProbe })
	probeSystem = func(context.Context, string) dnscheck.ProbeResult {
		return dnscheck.ProbeResult{Err: "lookup " + dnsCheckProbeName + ": no such host"}
	}
	w, ok := resolverfile.For(goos)
	if !ok {
		t.Fatalf("no wiring for %q; substituteResolverFileSeams must pick an OS that reads the file", goos)
	}
	// The control: the same file with loopback's address is dialled, so the
	// refusal below is the address's doing and not the shape's.
	dialled := ""
	probeAt = func(_ context.Context, server, _ string) dnscheck.ProbeResult {
		dialled = server
		return dnscheck.ProbeResult{Addrs: []string{"127.0.0.1"}}
	}
	readResolverFile = func() ([]byte, error) { return w.Render(5353), nil }
	if h, _ := w.ServerOf(w.Render(5353)); h != "127.0.0.1" {
		t.Fatalf("Render names %q, and this test rewrites 127.0.0.1 to the foreign address", h)
	}
	foreign := []byte(strings.ReplaceAll(string(w.Render(5353)), "127.0.0.1", "10.0.0.53"))
	if h, p := w.ServerOf(foreign); h != "10.0.0.53" || p != "5353" {
		t.Fatalf("the foreign file reads back as %q, %q; the rewrite missed", h, p)
	}

	var stdout, stderr bytes.Buffer
	err := run([]string{"dns", "check"}, &stdout, &stderr)
	verdictExitsAsPrinted(t, stdout.String(), err)
	if dialled != "127.0.0.1:5353" {
		t.Fatalf("control: a file naming 127.0.0.1 was dialled at %q, want 127.0.0.1:5353", dialled)
	}

	readResolverFile = func() ([]byte, error) { return foreign, nil }
	dialled = ""
	stdout.Reset()
	err = run([]string{"dns", "check"}, &stdout, &stderr)
	out := stdout.String()
	verdictExitsAsPrinted(t, out, err)
	if dialled != "" {
		t.Errorf("a file naming 10.0.0.53 was dialled at %q", dialled)
	}
	if !strings.Contains(out, "Is voya running?") {
		t.Errorf("the row does not ask the question no dial answered:\n%s", out)
	}
	if strings.Contains(out, "asked 127.0.0.1 on that port directly") {
		t.Errorf("the row quotes a dial nobody made:\n%s", out)
	}
	if !strings.Contains(out, "file port=not measured (the file names 10.0.0.53 rather than 127.0.0.1") {
		t.Errorf("the evidence line does not say which server the file names:\n%s", out)
	}
	if stderr.Len() != 0 {
		t.Errorf("`voya dns check` wrote to stderr; the report is stdout's product: %q", stderr.String())
	}
}

// TestLoopbackPortOfSaysWhichAddressTheFileNames pins the order the two
// reasons are asked in (#253): a file naming a server that is not loopback
// names one that cannot be voya's whether or not it also names a port, so
// the address is the reason returned in both shapes — the evidence line
// carries whichever reason this gives, and "the file names no port" would
// leave the address it does name unsaid. Both wirings, so neither OS's
// read-back carries the order alone.
func TestLoopbackPortOfSaysWhichAddressTheFileNames(t *testing.T) {
	foreign := "the file names 10.0.0.53 rather than 127.0.0.1, the only address voya's listeners bind"
	for _, tc := range []struct {
		shape, darwin, linux, port, unmeasured string
	}{
		{shape: "the file's own server", darwin: "nameserver 127.0.0.1\nport 5353\n",
			linux: "[Resolve]\nDNS=127.0.0.1:5353\n", port: "5353"},
		{shape: "another server, with a port", darwin: "nameserver 10.0.0.53\nport 5353\n",
			linux: "[Resolve]\nDNS=10.0.0.53:5353\n", unmeasured: foreign},
		{shape: "another server, no port", darwin: "nameserver 10.0.0.53\n",
			linux: "[Resolve]\nDNS=10.0.0.53\n", unmeasured: foreign},
		{shape: "loopback, no port", darwin: "nameserver 127.0.0.1\n",
			linux: "[Resolve]\nDNS=127.0.0.1\n", unmeasured: "the file names no port"},
		{shape: "no server at all", darwin: "port 5353\n", linux: "[Resolve]\n",
			unmeasured: "the file names no nameserver"},
	} {
		for goos, file := range map[string]string{"darwin": tc.darwin, "linux": tc.linux} {
			w, ok := resolverfile.For(goos)
			if !ok {
				t.Fatalf("no wiring for %q; the table is written for the OSes ADR 002 specifies", goos)
			}
			port, unmeasured := loopbackPortOf(w, []byte(resolverfile.Marker+"\n"+file))
			if port != tc.port || unmeasured != tc.unmeasured {
				t.Errorf("%s, %s: loopbackPortOf = %q, %q, want %q, %q",
					goos, tc.shape, port, unmeasured, tc.port, tc.unmeasured)
			}
		}
	}
}

// TestDNSCheckRunsTheProbesUnderOneBudget pins that the DNS probes' budgets are
// not additive (#253): the file-port dial is made wherever the file names a
// port, on hosts where no row will quote it, and run in series a port that
// answers nothing used to add one whole budget to every `voya dns check`. The
// system probe and the dial are given stubs that each wait for the other to
// be entered: run at once they meet within the run's first moments, run in
// series the first one entered waits out its whole budget alone — so the pin
// is which way each stub's wait ended, and nothing here asserts on the clock:
// [dnscheck.ProbeDefault] and [dnscheck.ProbePureGo] have no seam and resolve for
// real, and [proxiedChecks] runs after them, so the run's duration is not the
// probes' to answer for. The elapsed time is reported with a failure as
// context only.
func TestDNSCheckRunsTheProbesUnderOneBudget(t *testing.T) {
	substituteResolverFileSeams(t, true, func() (bool, error) { return true, nil })
	restoreProbe, restoreBudget := probeSystem, dnsCheckProbeBudget
	t.Cleanup(func() { probeSystem, dnsCheckProbeBudget = restoreProbe, restoreBudget })
	dnsCheckProbeBudget = 5 * time.Second
	w, ok := resolverfile.For(goos)
	if !ok {
		t.Fatalf("no wiring for %q; substituteResolverFileSeams must pick an OS that reads the file", goos)
	}
	readResolverFile = func() ([]byte, error) { return w.Render(5353), nil }

	systemEntered, dialEntered := make(chan struct{}), make(chan struct{})
	// Each stub reports whether it met the other or its budget: written
	// from the probe's goroutine, read after run returns, which waits on
	// every probe before classifying.
	var systemMet, dialMet bool
	probeSystem = func(ctx context.Context, _ string) dnscheck.ProbeResult {
		close(systemEntered)
		select {
		case <-dialEntered:
			systemMet = true
		case <-ctx.Done():
		}
		return dnscheck.ProbeResult{Err: "lookup " + dnsCheckProbeName + ": no such host"}
	}
	probeAt = func(ctx context.Context, _, _ string) dnscheck.ProbeResult {
		close(dialEntered)
		select {
		case <-systemEntered:
			dialMet = true
		case <-ctx.Done():
		}
		return dnscheck.ProbeResult{Addrs: []string{"127.0.0.1"}}
	}

	var stdout, stderr bytes.Buffer
	started := time.Now()
	err := run([]string{"dns", "check"}, &stdout, &stderr)
	elapsed := time.Since(started)
	verdictExitsAsPrinted(t, stdout.String(), err)
	if !systemMet || !dialMet {
		t.Errorf("the probes did not run at once: system met the dial = %v, dial met the system = %v "+
			"(run took %v against a %v budget)", systemMet, dialMet, elapsed, dnsCheckProbeBudget)
	}
}

// TestDNSCheckAsksWhetherTheHostResolvesThroughResolvedOnlyWhereTheFileIsRead
// pins #204's seam at the same gate, the same way: whether this host's own
// resolution reaches the service that reads the file is asked exactly where
// the file is, and a measured "no" — with resolverfile's reason — reaches
// the classifier, which then names the cause and not the command Preflight
// would refuse. The system probe is substituted with the silence that puts
// the report on the one row the answer is for.
func TestDNSCheckAsksWhetherTheHostResolvesThroughResolvedOnlyWhereTheFileIsRead(t *testing.T) {
	const why = "/etc/resolv.conf is not resolved's stub and names no 127.0.0.53"
	for _, reads := range []bool{false, true} {
		t.Run(fmt.Sprintf("reads=%v", reads), func(t *testing.T) {
			substituteResolverFileSeams(t, reads, func() (bool, error) { return true, nil })
			restoreProbe := probeSystem
			t.Cleanup(func() { probeSystem = restoreProbe })
			probeSystem = func(context.Context, string) dnscheck.ProbeResult {
				return dnscheck.ProbeResult{Err: "lookup " + dnsCheckProbeName + ": no such host"}
			}
			asked := ""
			wiringResolvesThroughResolved = func(w resolverfile.Wiring) (bool, string, error) {
				asked = w.GOOS
				return false, why, nil
			}

			var stdout, stderr bytes.Buffer
			err := run([]string{"dns", "check"}, &stdout, &stderr)
			out := stdout.String()
			word := verdictExitsAsPrinted(t, out, err)
			if got := asked != ""; got != reads {
				t.Errorf("dnsCheckCmd asked whether the host resolves through resolved: %v (as %q), want %v", got, asked, reads)
			}
			if reads && asked != goos {
				t.Errorf("dnsCheckCmd asked about %q while goos is %q", asked, goos)
			}
			bypassed := strings.Contains(out, "does not go through systemd-resolved at all") && strings.Contains(out, why)
			if bypassed != reads {
				t.Errorf("the report names the bypassed service with its reason: %v, want %v (asked: %q):\n%s",
					bypassed, reads, asked, out)
			}
			if strings.Contains(out, "voya dns install --activate") {
				t.Errorf("the report prescribes the command Preflight refuses on this host:\n%s", out)
			}
			if stderr.Len() != 0 {
				t.Errorf("`voya dns check` wrote to stderr; the report is stdout's product: %q", stderr.String())
			}
			t.Logf("verdict %s\n%s", word, out)
		})
	}
}

// TestDNSCheckAsksTheResolvedVersionOnlyWhereTheFileIsRead pins #206's seam at
// the same gate, the same way: whether the resolved that reads the file is
// too old to be told a port is asked exactly where the file is, and a
// measured "yes" — with resolverfile's reason, the clause the install's
// refusal carries — reaches the classifier, which names that cause ahead of
// the bypass it would otherwise have named, and not the restart or the
// command Preflight would refuse. The bypass is answered too, so the row
// proves the order and not just the arrival.
func TestDNSCheckAsksTheResolvedVersionOnlyWhereTheFileIsRead(t *testing.T) {
	const why = "systemd-resolved here is v245, and a port in DNS= needs v246 or newer"
	for _, reads := range []bool{false, true} {
		t.Run(fmt.Sprintf("reads=%v", reads), func(t *testing.T) {
			substituteResolverFileSeams(t, reads, func() (bool, error) { return true, nil })
			restoreProbe := probeSystem
			t.Cleanup(func() { probeSystem = restoreProbe })
			probeSystem = func(context.Context, string) dnscheck.ProbeResult {
				return dnscheck.ProbeResult{Err: "lookup " + dnsCheckProbeName + ": no such host"}
			}
			asked := ""
			wiringResolvedTooOld = func(w resolverfile.Wiring, _ context.Context) (bool, string, error) {
				asked = w.GOOS
				return true, why, nil
			}
			wiringResolvesThroughResolved = func(resolverfile.Wiring) (bool, string, error) {
				return false, "/etc/resolv.conf is not resolved's stub and names no 127.0.0.53", nil
			}

			var stdout, stderr bytes.Buffer
			err := run([]string{"dns", "check"}, &stdout, &stderr)
			out := stdout.String()
			word := verdictExitsAsPrinted(t, out, err)
			if got := asked != ""; got != reads {
				t.Errorf("dnsCheckCmd asked the resolved version: %v (as %q), want %v", got, asked, reads)
			}
			if reads && asked != goos {
				t.Errorf("dnsCheckCmd asked about %q while goos is %q", asked, goos)
			}
			tooOld := strings.Contains(out, why) && strings.Contains(out, "Failed to add DNS server address")
			if tooOld != reads {
				t.Errorf("the report names the old resolved with its reason: %v, want %v (asked: %q):\n%s",
					tooOld, reads, asked, out)
			}
			if reads && strings.Contains(out, "does not go through systemd-resolved") {
				t.Errorf("the report names the bypass ahead of the version the install refuses on first:\n%s", out)
			}
			if strings.Contains(out, "voya dns install --activate") {
				t.Errorf("the report prescribes the command Preflight refuses on this host:\n%s", out)
			}
			if stderr.Len() != 0 {
				t.Errorf("`voya dns check` wrote to stderr; the report is stdout's product: %q", stderr.String())
			}
			t.Logf("verdict %s\n%s", word, out)
		})
	}
}

// TestDNSCheckReportsDespiteAnUnreadableResolverFile pins #104's second half
// end to end: on the OS that reads the file, a stat that fails with neither
// presence nor absence no longer kills the command before a probe has run —
// the report prints, quotes the stat's reason, does not call the file
// missing, and exits on the verdict. Which verdict is the host's to decide,
// as in TestDNSCheckReportsAndExitsByItsVerdict; the log records it.
func TestDNSCheckReportsDespiteAnUnreadableResolverFile(t *testing.T) {
	substituteResolverFileSeams(t, true, func() (bool, error) {
		return false, errors.New("stat " + resolverfile.Path + ": permission denied")
	})

	var stdout, stderr bytes.Buffer
	err := run([]string{"dns", "check"}, &stdout, &stderr)
	out := stdout.String()

	word := verdictExitsAsPrinted(t, out, err)
	if !strings.Contains(out, "permission denied") {
		t.Errorf("the report does not quote the stat's reason:\n%s", out)
	}
	for _, banned := range []string{"resolver-not-installed", "is not installed"} {
		if strings.Contains(out, banned) {
			t.Errorf("the report says %q of a file whose absence was never established:\n%s", banned, out)
		}
	}
	carries(t, "\n"+out, "\ndns: ", "measured:")
	if stderr.Len() != 0 {
		t.Errorf("`voya dns check` wrote to stderr; the report is stdout's product: %q", stderr.String())
	}
	t.Logf("verdict with the file unreadable: %s\n%s", word, out)
}

// --- #153: the registry-backed checks -----------------------------------------

// --- #214: the auth surface ---------------------------------------------------

// TestRenderDNSCheckLeadsWithTheVerdict pins the presentation: the verdict —
// the word the exit code derives from — and the state, then the whole detail,
// then the verdict as the last line. "DNS: OK" is exactly what #6 rules out,
// so the header is asserted verbatim.
func TestRenderDNSCheckLeadsWithTheVerdict(t *testing.T) {
	var out bytes.Buffer
	renderDNSCheck(&out, dnscheck.DNSReport{
		State:   dnscheck.DNSWorking,
		Verdict: dnscheck.VerdictOK,
		Detail:  "every sentence of the detail\nmeasured: system=127.0.0.1",
	})
	got := out.String()
	if !strings.HasPrefix(got, "dns: ok — working\n") {
		t.Errorf("report does not lead with the verdict and state:\n%s", got)
	}
	carries(t, got, "every sentence of the detail", "measured:")
	if !strings.HasSuffix(got, "\nverdict: ok\n") {
		t.Errorf("report does not end with the verdict:\n%s", got)
	}
	if strings.Contains(got, "DNS: OK") {
		t.Errorf("report says %q:\n%s", "DNS: OK", got)
	}
}

// TestUsageListsDNSCheck: its line sits under "Commands:", in the name column.
func TestUsageListsDNSCheck(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"help"}, &stdout, &stderr); err != nil {
		t.Fatalf("help: %v", err)
	}
	_, rest, found := strings.Cut(stdout.String(), "Commands:")
	if !found {
		t.Fatalf("usage has no \"Commands:\" block:\n%s", stdout.String())
	}
	block, _, _ := strings.Cut(rest, "\n\n")
	if !listsCommand(block, "dns check") {
		t.Errorf("the \"Commands:\" block gives `dns check` no line of its own:\n%s", stdout.String())
	}
}

// TestDoctorIsGone pins the removal (#314): no alias, so a script that still
// runs it fails rather than reading a different report.
func TestDoctorIsGone(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"doctor"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), `unknown command "doctor"`) {
		t.Fatalf("`voya dns check` = %v, want unknown command", err)
	}
}

// TestDNSCheckIsCleanWithoutTheOptionalResolverFile is #296's done-when: a
// host that reads the file and has none, and nothing answering *.internal, is
// ok and exits 0 — the setup the README promises works without
// `voya dns install`. The second run is the control: an unexplained answer on
// one path makes the same setup a problem again, so the first "ok" came from
// what the resolvers said and not from a check that stopped looking.
func TestDNSCheckIsCleanWithoutTheOptionalResolverFile(t *testing.T) {
	substituteResolverFileSeams(t, true, func() (bool, error) { return false, nil })
	restoreSystem, restoreDefault, restorePure := probeSystem, probeDefault, probePureGo
	t.Cleanup(func() { probeSystem, probeDefault, probePureGo = restoreSystem, restoreDefault, restorePure })
	silent := func(context.Context, string) dnscheck.ProbeResult {
		return dnscheck.ProbeResult{Err: "lookup voya-dns-check-probe.internal: no such host"}
	}
	probeSystem, probeDefault, probePureGo = silent, silent, silent

	var stdout, stderr bytes.Buffer
	err := run([]string{"dns", "check"}, &stdout, &stderr)
	out := stdout.String()
	if word := verdictExitsAsPrinted(t, out, err); word != "ok" {
		t.Errorf("verdict %q with no resolver file on a working setup, want ok:\n%s", word, out)
	}
	carries(t, "\n"+out, "\ndns: ok — resolver-not-installed\n")

	probeDefault = func(context.Context, string) dnscheck.ProbeResult {
		return dnscheck.ProbeResult{Addrs: []string{"198.51.100.7"}}
	}
	stdout.Reset()
	err = run([]string{"dns", "check"}, &stdout, &stderr)
	if word := verdictExitsAsPrinted(t, stdout.String(), err); word != "problem" {
		t.Errorf("verdict %q with something else answering *.internal, want problem:\n%s", word, stdout.String())
	}
}

// TestDNSCheckReadsNoConfiguration: a user layer that does not parse must not
// stop the check, since it asks the host and not voya.
func TestDNSCheckReadsNoConfiguration(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, config.UserFile), []byte("proxy_port: \"9777\"\n"), 0o600); err != nil {
		t.Fatalf("write user layer: %v", err)
	}
	t.Setenv(config.DirEnv, dir)
	var stdout, stderr bytes.Buffer
	err := run([]string{"dns", "check"}, &stdout, &stderr)
	verdictExitsAsPrinted(t, stdout.String(), err)
	carries(t, "\n"+stdout.String(), "\ndns: ", "measured:")
}
