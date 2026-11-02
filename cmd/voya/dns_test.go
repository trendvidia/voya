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

	"github.com/trendvidia/voya/internal/config"
	"github.com/trendvidia/voya/internal/resolverfile"
)

// asRootAt points both commands at path on the named OS, running as euid, so
// the rows about the file itself run on any OS as any user (#156). The
// host-level seams go with it: a Linux row must not reach a real systemd, and
// on a machine that has one it must not restart it (#155). Everything is
// restored when the test ends.
func asRootAt(t *testing.T, path, on string, euid int) *[]string {
	t.Helper()
	oldGOOS, oldEUID, oldPath := goos, geteuid, resolverPath
	oldPre, oldAct := wiringPreflight, wiringActivate
	var activated []string
	goos, geteuid, resolverPath = on, func() int { return euid }, path
	wiringPreflight = func(resolverfile.Wiring, context.Context) error { return nil }
	// What the real Activate does, without the systemctl it does it with:
	// macOS has configd and needs nothing, Linux restarts resolved and says
	// so first. [resolverfile]'s own tests pin that against a recorded
	// command; the line is stubbed here because a stub that stayed silent
	// would let the commands' stderr be asserted empty on rows where the
	// real code speaks (#188).
	wiringActivate = func(w resolverfile.Wiring, _ context.Context, logf func(string, ...any)) (string, error) {
		activated = append(activated, w.GOOS)
		if w.GOOS == "linux" {
			logf("restarting %s…", "systemd-resolved.service")
			return "systemd-resolved restarted", nil
		}
		return "", nil
	}
	t.Cleanup(func() {
		goos, geteuid, resolverPath = oldGOOS, oldEUID, oldPath
		wiringPreflight, wiringActivate = oldPre, oldAct
	})
	return &activated
}

// withDNSPort gives the commands a configuration whose dns_port is port, in a
// directory of its own so the developer's voya.pxf is never read, and hands
// back the file the install line names.
func withDNSPort(t *testing.T, port uint32) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, config.UserFile)
	if err := os.WriteFile(path, fmt.Appendf(nil, "dns_port = %d\n", port), 0o600); err != nil {
		t.Fatalf("write user layer: %v", err)
	}
	t.Setenv(config.DirEnv, dir)
	return path
}

// wiringFor is the wiring under test, with the test's own fatal for an OS
// that has none.
func wiringFor(t *testing.T, goos string) resolverfile.Wiring {
	t.Helper()
	w, ok := resolverfile.For(goos)
	if !ok {
		t.Fatalf("resolverfile.For(%q) reports no wiring", goos)
	}
	return w
}

// TestDNSInstallAndUninstall is every row of the two commands over the path
// seam, on each OS ADR 002 specifies a file for (#155): what the file was,
// what the command printed and returned, and what the file is afterwards.
// Each row is one of the outcomes [resolverfile] distinguishes or the
// refusal; the sweep at the end says so, so a row deleted here does not take
// its assertion with it silently.
func TestDNSInstallAndUninstall(t *testing.T) {
	const handWritten = "nameserver 127.0.0.1\nport 9778\n"
	for _, base := range []resolverfile.Wiring{wiringFor(t, "darwin"), wiringFor(t, "linux")} {
		w := base
		rows := []struct {
			name   string
			args   []string
			port   uint32 // dns_port in the configuration
			before string // "" for no file
			// stdout is the line the command prints when it succeeds, with %s
			// for the file's path and %s for the user layer the port came
			// from; the restart clause is added below for the OS that has one.
			stdout string
			// changed says the file on disk is different afterwards, which is
			// the whole condition for making it live — unless activate, the
			// row's --activate, says to make it live regardless (#177).
			changed  bool
			activate bool
			errHas   string // what run's error must contain, when it fails; "" for success
			after    string // "" for no file
		}{
			{name: "fresh install writes what this OS reads, with the configured port",
				args: []string{"dns", "install"}, port: 9778, before: "",
				stdout: "installed %s: *.internal -> 127.0.0.1:9778; dns_port from %s", changed: true,
				after: string(w.Render(9778))},
			{name: "install with a moved dns_port writes that port",
				args: []string{"dns", "install"}, port: 5353, before: "",
				stdout: "installed %s: *.internal -> 127.0.0.1:5353; dns_port from %s", changed: true,
				after: string(w.Render(5353))},
			{name: "second install is a no-op that says so and restarts nothing",
				args: []string{"dns", "install"}, port: 9778, before: string(w.Render(9778)),
				stdout: "already installed: %s points *.internal at 127.0.0.1:9778; dns_port from %s",
				after:  string(w.Render(9778))},
			{name: "second install with --activate restarts anyway, for a file that is right and unread",
				args: []string{"dns", "install", "--activate"}, port: 9778, before: string(w.Render(9778)),
				stdout: "already installed: %s points *.internal at 127.0.0.1:9778; dns_port from %s", activate: true,
				after: string(w.Render(9778))},
			{name: "voya's file with another port is rewritten and the change reported",
				args: []string{"dns", "install"}, port: 9778, before: string(w.Render(9777)),
				stdout: "updated %s: *.internal -> 127.0.0.1:9778 (was port 9777); dns_port from %s", changed: true,
				after: string(w.Render(9778))},
			{name: "voya's file under an earlier marker wording is rewritten without claiming a port moved",
				args: []string{"dns", "install"}, port: 9778,
				before: resolverfile.MarkerPrefix + " (an older voya)\n" + body(w.Render(9778)),
				stdout: "updated %s: *.internal -> 127.0.0.1:9778 (rewrote an earlier voya file); dns_port from %s", changed: true,
				after: string(w.Render(9778))},
			{name: "an unmarked file is refused unchanged, its contents shown",
				args: []string{"dns", "install"}, port: 9778, before: handWritten,
				errHas: "dns install: refused — %s is not voya's",
				after:  handWritten},
			{name: "uninstall removes voya's file",
				args: []string{"dns", "uninstall"}, before: string(w.Render(9778)),
				stdout: "removed %s", changed: true,
				after: ""},
			{name: "uninstall refuses an unmarked file",
				args: []string{"dns", "uninstall"}, before: handWritten,
				errHas: "dns uninstall: refused — %s is not voya's",
				after:  handWritten},
			{name: "uninstall with no file is nothing to do",
				args: []string{"dns", "uninstall"}, before: "",
				stdout: "nothing to do: %s is not installed",
				after:  ""},
			{name: "uninstall with --activate restarts even with no file, for one removed by hand and still held",
				args: []string{"dns", "uninstall", "--activate"}, before: "",
				stdout: "nothing to do: %s is not installed", activate: true,
				after: ""},
		}
		for _, tc := range rows {
			t.Run(w.GOOS+"/"+tc.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), filepath.Base(w.Path))
				activated := asRootAt(t, path, w.GOOS, 0)
				layer := ""
				if tc.port != 0 {
					layer = withDNSPort(t, tc.port)
				}
				if tc.before != "" {
					if err := os.WriteFile(path, []byte(tc.before), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				var stdout, stderr bytes.Buffer
				err := run(tc.args, &stdout, &stderr)
				if tc.errHas == "" {
					if err != nil {
						t.Fatalf("run(%q) = %v", tc.args, err)
					}
					want := fmt.Sprintf(strings.ReplaceAll(tc.stdout, "%s", "%[1]s"), path)
					if strings.Contains(tc.stdout, "dns_port from") {
						want = fmt.Sprintf(tc.stdout, path, layer)
					}
					// The seam's Activate speaks only on the OS whose wiring
					// is a service; a row that changed nothing never calls it
					// — unless --activate did (#177). Where that flag would
					// have been the way out, `already installed` names it,
					// on the OS where an unread file is possible.
					if (tc.changed || tc.activate) && w.GOOS == "linux" {
						want += "; systemd-resolved restarted"
					}
					if strings.HasPrefix(tc.stdout, "already installed") && !tc.activate && w.GOOS == "linux" {
						want += "; " + activateHint
					}
					if got := stdout.String(); got != want+"\n" {
						t.Errorf("stdout:\n%q\nwant:\n%q", got, want+"\n")
					}
				} else {
					want := fmt.Sprintf(tc.errHas, path)
					if err == nil || !strings.Contains(err.Error(), want) {
						t.Fatalf("run(%q) = %v, want an error containing %q", tc.args, err, want)
					}
					var status *exitStatus
					if !errors.As(err, &status) || status.code != 1 {
						t.Errorf("a refusal exits %v, want 1", err)
					}
					for line := range strings.Lines(tc.before) {
						if !strings.Contains(err.Error(), "\n  "+strings.TrimRight(line, "\n")) {
							t.Errorf("the refusal does not show the file's line %q:\n%v", line, err)
						}
					}
					if stdout.Len() != 0 {
						t.Errorf("a refusal wrote to stdout: %q", stdout.String())
					}
				}
				if got, want := len(*activated) > 0, tc.changed || tc.activate; got != want {
					t.Errorf("the file %s and --activate was %v, yet the wiring was activated %d time(s)",
						map[bool]string{true: "changed", false: "did not change"}[tc.changed], tc.activate, len(*activated))
				}
				// Nothing but the restart's own line reaches stderr: main
				// prints the error, and the result goes to stdout. On Linux
				// a row that changed the file restarts resolved, and that
				// wait is explained there before it starts (#188).
				wantErr := ""
				if (tc.changed || tc.activate) && w.GOOS == "linux" {
					wantErr = "restarting systemd-resolved.service…\n"
				}
				if got := stderr.String(); got != wantErr {
					t.Errorf("stderr = %q, want %q", got, wantErr)
				}
				after, err := os.ReadFile(path)
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				if string(after) != tc.after {
					t.Errorf("after the command the file is %q, want %q", after, tc.after)
				}
			})
		}
		// The sweep: both commands, and every one of the five outcomes plus the
		// refusal on each command that can reach it.
		seen := map[string]bool{}
		for _, tc := range rows {
			key := tc.args[1] + ":"
			switch {
			case tc.errHas != "":
				key += "refused"
			default:
				key += strings.Fields(tc.stdout)[0]
			}
			seen[key] = true
		}
		for _, want := range []string{"install:installed", "install:already", "install:updated", "install:refused",
			"uninstall:removed", "uninstall:nothing", "uninstall:refused"} {
			if !seen[want] {
				t.Errorf("%s: no row lands on %s", w.GOOS, want)
			}
		}
	}
}

// body is a rendering without its marker line, so a row can put an older
// marker in front of what this OS reads without restating it.
func body(rendered []byte) string {
	_, rest, _ := strings.Cut(string(rendered), "\n")
	return rest
}

// Without root neither command tries and fails: it names the line to run —
// with the flag the developer typed, since a --activate dropped from it runs
// an install that finds the file unchanged and restarts nothing (#177) —
// exits 1, and writes nothing — the seam is a writable temp directory, so a
// write that was attempted would have landed. Configuration is not loaded
// first either: a broken user layer would otherwise be the error, in front
// of the one instruction that matters.
func TestDNSCommandsWithoutRootSayTheSudoLine(t *testing.T) {
	for _, args := range [][]string{
		{"dns", "install"}, {"dns", "install", "--activate"},
		{"dns", "uninstall"}, {"dns", "uninstall", "--activate"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "internal")
			asRootAt(t, path, "darwin", 501)
			// A configuration that does not parse, so loading it would fail.
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, config.UserFile), []byte("dns_port = \"not a port\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv(config.DirEnv, dir)
			// A voya-marked file, so uninstall would have had something to do.
			if args[1] == "uninstall" {
				if err := os.WriteFile(path, wiringFor(t, "darwin").Render(9778), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			var stdout, stderr bytes.Buffer
			err := run(args, &stdout, &stderr)
			var status *exitStatus
			if !errors.As(err, &status) || status.code != 1 {
				t.Fatalf("run(%q) = %v, want exit 1", args, err)
			}
			if want := "sudo voya " + strings.Join(args, " "); !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not say %q: %v", want, err)
			}
			if stdout.Len() != 0 {
				t.Errorf("wrote to stdout: %q", stdout.String())
			}
			_, statErr := os.Stat(path)
			if args[1] == "install" && statErr == nil {
				t.Errorf("install without root wrote %s", path)
			}
			if args[1] == "uninstall" && statErr != nil {
				t.Errorf("uninstall without root touched %s: %v", path, statErr)
			}
		})
	}
	// The positive control for the configuration claim: as root, the same
	// broken layer is what fails.
	path := filepath.Join(t.TempDir(), "internal")
	asRootAt(t, path, "darwin", 0)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, config.UserFile), []byte("dns_port = \"not a port\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.DirEnv, dir)
	var stdout, stderr bytes.Buffer
	err := run([]string{"dns", "install"}, &stdout, &stderr)
	if err == nil || strings.Contains(err.Error(), "sudo") {
		t.Fatalf("as root with a broken user layer, run = %v; want the layer's own error", err)
	}
}

// On an OS ADR 002 specifies no file for, both commands say so, name the
// issue that decides it, exit 2 — the check's "cannot answer here" — and touch
// nothing, root or not.
func TestDNSCommandsOffASpecifiedOSExitTwo(t *testing.T) {
	for _, tc := range []struct {
		args []string
		goos string
		euid int
	}{
		{[]string{"dns", "install"}, "windows", 0},
		{[]string{"dns", "install"}, "freebsd", 501},
		{[]string{"dns", "uninstall"}, "windows", 0},
		{[]string{"dns", "uninstall"}, "freebsd", 501},
	} {
		t.Run(strings.Join(tc.args, "_")+"_"+tc.goos, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "internal")
			asRootAt(t, path, tc.goos, tc.euid)
			withDNSPort(t, 9778)
			if tc.args[1] == "uninstall" {
				if err := os.WriteFile(path, wiringFor(t, "darwin").Render(9778), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			err := run(tc.args, &stdout, &stderr)
			var status *exitStatus
			if !errors.As(err, &status) || status.code != 2 {
				t.Fatalf("run(%q) = %v, want exit 2", tc.args, err)
			}
			want := fmt.Sprintf("%s: the resolver wiring on %s is not specified yet (#155)", strings.Join(tc.args, " "), tc.goos)
			if err.Error() != want {
				t.Errorf("error %q, want %q", err.Error(), want)
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Errorf("wrote something: stdout %q, stderr %q", stdout.String(), stderr.String())
			}
			_, statErr := os.Stat(path)
			if tc.args[1] == "install" && statErr == nil {
				t.Errorf("install off a specified OS wrote %s", path)
			}
			if tc.args[1] == "uninstall" && statErr != nil {
				t.Errorf("uninstall off a specified OS touched %s: %v", path, statErr)
			}
		})
	}
}

// A Linux host that is not running systemd-resolved has an OS with a wiring
// and no machinery to read it: install refuses it the way it refuses an OS
// with no wiring at all — exit 2, nothing written — and the refusal carries
// what the developer can do instead (#155). Uninstall does not ask: a file
// voya wrote is voya's to take back whether or not resolved is still there.
func TestDNSInstallRefusesAHostWithoutResolved(t *testing.T) {
	unavailable := &resolverfile.UnavailableError{
		Reason: "systemd-resolved is inactive here (systemctl is-active systemd-resolved.service)",
		Advice: "wire *.internal yourself",
	}
	t.Run("install", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "voya.conf")
		asRootAt(t, path, "linux", 0)
		wiringPreflight = func(resolverfile.Wiring, context.Context) error { return unavailable }
		withDNSPort(t, 9778)
		var stdout, stderr bytes.Buffer
		err := run([]string{"dns", "install"}, &stdout, &stderr)
		var status *exitStatus
		if !errors.As(err, &status) || status.code != 2 {
			t.Fatalf("run = %v, want exit 2", err)
		}
		for _, want := range []string{"dns install: ", unavailable.Reason, unavailable.Advice} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not say %q:\n%v", want, err)
			}
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("install wrote %s on a host that cannot read it: %v", path, err)
		}
		if stdout.Len() != 0 {
			t.Errorf("wrote to stdout: %q", stdout.String())
		}
	})
	t.Run("uninstall does not preflight", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "voya.conf")
		asRootAt(t, path, "linux", 0)
		asked := false
		wiringPreflight = func(resolverfile.Wiring, context.Context) error { asked = true; return unavailable }
		if err := os.WriteFile(path, wiringFor(t, "linux").Render(9778), 0o644); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if err := run([]string{"dns", "uninstall"}, &stdout, &stderr); err != nil {
			t.Fatalf("uninstall on a host without resolved = %v", err)
		}
		if asked {
			t.Error("uninstall preflighted the host, stranding voya's own file where resolved is gone")
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("uninstall left %s behind: %v", path, err)
		}
	})
}

// What the restart says while it waits goes to stderr, where everything voya
// says about itself goes, and each line on its own: stdout keeps the one result
// line, so `voya dns install` stays parseable by whoever reads it (#188).
func TestDNSInstallSaysWhatTheRestartWaitsForOnStderr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "voya.conf")
	asRootAt(t, path, "linux", 0)
	wiringActivate = func(_ resolverfile.Wiring, _ context.Context, logf func(string, ...any)) (string, error) {
		logf("restarting %s…", "systemd-resolved.service")
		return "systemd-resolved restarted", nil
	}
	withDNSPort(t, 9778)
	var stdout, stderr bytes.Buffer
	if err := run([]string{"dns", "install"}, &stdout, &stderr); err != nil {
		t.Fatalf("run = %v", err)
	}
	if got, want := stderr.String(), "restarting systemd-resolved.service…\n"; got != want {
		t.Errorf("stderr = %q, want %q: the wait is explained there, one line at a time", got, want)
	}
	if strings.Contains(stdout.String(), "restarting") || strings.Count(stdout.String(), "\n") != 1 {
		t.Errorf("stdout = %q, want the one result line and no progress on it", stdout.String())
	}
}

// A restart that fails is the command's failure: the drop-in is on disk and
// nothing has read it, which is the one outcome a developer must not take for
// a working install. The file stays — voya wrote it, and removing it would
// leave the host in a third state nobody asked for — and because it stays,
// running the command again finds it [resolverfile.Unchanged] and restarts
// nothing. So the failure has to hand over the restart itself, or the way out
// of this state is written down nowhere (#155).
func TestDNSInstallFailsWhenTheRestartFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "voya.conf")
	asRootAt(t, path, "linux", 0)
	wiringActivate = func(resolverfile.Wiring, context.Context, func(string, ...any)) (string, error) {
		return "", errors.New("systemctl restart systemd-resolved.service: exit status 1: Job failed")
	}
	withDNSPort(t, 9778)
	var stdout, stderr bytes.Buffer
	err := run([]string{"dns", "install"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "dns install: systemctl restart") {
		t.Fatalf("run = %v, want the restart's own failure under the command's name", err)
	}
	for _, want := range []string{
		"The file is written",
		"sudo " + wiringFor(t, "linux").ReloadCommand(),
		"sudo voya dns install --activate",
		"running `voya dns install` again without --activate finds nothing left to change and restarts nothing",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not say %q, so the way out of it is nowhere:\n%v", want, err)
		}
	}
	if stdout.Len() != 0 {
		t.Errorf("a failed restart printed a success line: %q", stdout.String())
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(wiringFor(t, "linux").Render(9778)) {
		t.Errorf("the file voya wrote is %q, %v; want it left as written", got, err)
	}
	// The clause is the wiring's, not the command's: on macOS a failed
	// activation would have nothing to hand over, and inventing a restart
	// there would send a developer to a service the OS does not have.
	if got := reactivation("dns install", wiringFor(t, "darwin"), resolverfile.Installed); got != "" {
		t.Errorf("a macOS activation failure would say %q, and macOS has nothing to run", got)
	}
}

// The same failure on uninstall, where what is on disk is the opposite: the
// file is gone. The error is the whole of the output — no success line is
// printed — so an install-shaped "the file is written" would leave the
// developer re-running the command to find out whether the drop-in is still
// there, on the one failure a re-run reports as nothing to do.
func TestDNSUninstallSaysTheFileIsGoneWhenTheRestartFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "voya.conf")
	asRootAt(t, path, "linux", 0)
	if err := os.WriteFile(path, wiringFor(t, "linux").Render(9778), 0o644); err != nil {
		t.Fatal(err)
	}
	wiringActivate = func(resolverfile.Wiring, context.Context, func(string, ...any)) (string, error) {
		return "", errors.New("systemctl restart systemd-resolved.service: exit status 1: Job failed")
	}
	var stdout, stderr bytes.Buffer
	err := run([]string{"dns", "uninstall"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "dns uninstall: systemctl restart") {
		t.Fatalf("run = %v, want the restart's own failure under the command's name", err)
	}
	if !strings.Contains(err.Error(), "The file is removed") {
		t.Errorf("the failure does not say what happened on disk:\n%v", err)
	}
	if strings.Contains(err.Error(), "The file is written") {
		t.Errorf("a failed uninstall reports a write:\n%v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("uninstall left %s behind, so the message is wrong about disk too: %v", path, err)
	}
	if stdout.Len() != 0 {
		t.Errorf("a failed restart printed a success line: %q", stdout.String())
	}
}

// `voya dns install` runs under sudo, and sudo decides what $HOME is: a
// sudoers with always_set_home, `sudo -H`, or a wrapper resolves root's
// configuration directory, where the developer's voya.pxf is not — and voya
// would write its default port while `voya run` binds the configured one.
// The line says which layer the port came from, and where voya looked when it
// was not the developer's (#172).
func TestDNSInstallSaysWhereThePortCameFrom(t *testing.T) {
	for _, tc := range []struct {
		name string
		// layer is the user layer to write, or "" for no file at all.
		layer string
		// noHome empties every environment variable [config.Dir] reads.
		noHome bool
		port   string
		says   string
	}{
		{name: "the developer's own layer is named", layer: "dns_port = 5353\n", port: "5353",
			says: "dns_port from %s"},
		{name: "no layer under this HOME is said, not silently defaulted", port: "9778",
			says: "dns_port is voya's built-in default (no %s on this HOME)"},
		{name: "a layer that sets no dns_port is said too", layer: "proxy_port = 9777\n", port: "9778",
			says: "dns_port is voya's built-in default (%s sets no dns_port)"},
		{name: "no configuration directory at all names the variables", noHome: true, port: "9778",
			says: "dns_port is voya's built-in default; this process has no configuration directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "internal")
			asRootAt(t, path, "darwin", 0)
			dir := t.TempDir()
			layer := filepath.Join(dir, config.UserFile)
			if tc.layer != "" {
				if err := os.WriteFile(layer, []byte(tc.layer), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.noHome {
				t.Setenv("HOME", "")
				t.Setenv("XDG_CONFIG_HOME", "")
				t.Setenv(config.DirEnv, "")
			} else {
				t.Setenv(config.DirEnv, dir)
			}
			var stdout, stderr bytes.Buffer
			if err := run([]string{"dns", "install"}, &stdout, &stderr); err != nil {
				t.Fatalf("run = %v", err)
			}
			want := tc.says
			if strings.Contains(want, "%s") {
				want = fmt.Sprintf(want, layer)
			}
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("the install line does not say %q:\n%s", want, stdout.String())
			}
			if !strings.Contains(stdout.String(), "127.0.0.1:"+tc.port) {
				t.Errorf("the install line does not name port %s:\n%s", tc.port, stdout.String())
			}
			// One line, whatever it had to say: the provenance is a clause,
			// not a second line a reader can miss.
			if n := strings.Count(stdout.String(), "\n"); n != 1 {
				t.Errorf("the command printed %d lines:\n%s", n, stdout.String())
			}
		})
	}
}

// TestDNSUsageNamesActivate: the flag is named where a developer whose file is
// right and unread is already looking (#177) — the command's own usage, on
// stdout for -h and exit 0, on stderr for a flag it does not take, and
// without root, since asking what a command does is not touching /etc.
func TestDNSUsageNamesActivate(t *testing.T) {
	for _, name := range []string{"install", "uninstall"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "internal")
			asRootAt(t, path, "darwin", 501)
			var stdout, stderr bytes.Buffer
			if err := run([]string{"dns", name, "-h"}, &stdout, &stderr); err != nil {
				t.Fatalf("`voya dns %s -h`: %v", name, err)
			}
			for _, want := range []string{"voya dns " + name + " [--activate]", "--activate  restart systemd-resolved whatever the outcome"} {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("`voya dns %s -h` does not say %q on stdout:\n%s", name, want, stdout.String())
				}
			}
			if stderr.Len() != 0 {
				t.Errorf("`voya dns %s -h` wrote to stderr: %q", name, stderr.String())
			}
			stdout.Reset()
			err := run([]string{"dns", name, "-bogus"}, &stdout, &stderr)
			if err == nil || !strings.Contains(err.Error(), "flag provided but not defined: -bogus") {
				t.Errorf("`voya dns %s -bogus` = %v, want the flag refused", name, err)
			}
			if !strings.Contains(stderr.String(), "voya dns "+name+" [--activate]") || stdout.Len() != 0 {
				t.Errorf("`voya dns %s -bogus` printed stdout %q stderr %q, want usage on stderr alone", name, stdout.String(), stderr.String())
			}
		})
	}
}

// Both are two-word commands that take nothing after their name: the first
// two-word commands that run, so this is where noExtraArgs's word count is
// exercised end to end rather than driven directly (#90).
func TestDNSCommandsRefuseExtraArguments(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"dns", "install", "now"}, `dns install takes no arguments, got "now"`},
		{[]string{"dns", "uninstall", "now"}, `dns uninstall takes no arguments, got "now"`},
		{[]string{"dns", "uninstall", "--force"}, `flag provided but not defined: -force`},
	} {
		path := filepath.Join(t.TempDir(), "internal")
		asRootAt(t, path, "darwin", 0)
		var stdout, stderr bytes.Buffer
		err := run(tc.args, &stdout, &stderr)
		if err == nil || err.Error() != tc.want {
			t.Errorf("run(%q) = %v, want %q", tc.args, err, tc.want)
		}
		if !strings.Contains(stderr.String(), "Usage:") {
			t.Errorf("run(%q) printed no usage to stderr:\n%s", tc.args, stderr.String())
		}
		if _, err := os.Stat(path); err == nil {
			t.Errorf("run(%q) wrote %s", tc.args, path)
		}
	}
}
