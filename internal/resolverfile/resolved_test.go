package resolverfile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The three command lines this package runs. Spelled out here rather than
// built from the constants, so a change to what voya asks the machine shows
// up as a failing test rather than as a test that asks the same new question.
const (
	isActive = "systemctl is-active systemd-resolved.service"
	restart  = "systemctl restart systemd-resolved.service"
	version  = "resolvectl --version"
	domains  = "resolvectl domain"
)

// noCatchAll is what `resolvectl domain` says on a host whose global scope
// claims nothing: the shape every row that expects Preflight to pass needs,
// since the conflict check runs last and asks this question of them all.
const noCatchAll = `echo "Global:
Link 2 (enp0s5): localdomain"`

// notFound is a reply standing for a binary this host does not have: the
// command never starts, which is a different failure from one that starts and
// exits nonzero, and the one a machine with no systemd gives.
const notFound = "\x00notFound"

// fakeCommands answers every command this package runs with a shell script
// the test chose, keyed by the whole command line, and records what was asked
// in order — so a row can assert both what voya concluded and that it did not
// restart anything on the way there. A command no row provided is a test
// failure: the seam exists to make every Linux row runnable from any OS
// (#155), and a real systemctl reaching a developer's machine from a unit
// test is what it exists to prevent.
func fakeCommands(t *testing.T, replies map[string]string) *[]string {
	t.Helper()
	var asked []string
	old := runCommand
	t.Cleanup(func() { runCommand = old })
	runCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		line := strings.Join(append([]string{name}, args...), " ")
		asked = append(asked, line)
		script, ok := replies[line]
		switch {
		case !ok:
			t.Errorf("voya ran %q, which this row did not expect", line)
			script = "exit 127"
		case script == notFound:
			return exec.CommandContext(ctx, filepath.Join(t.TempDir(), "no-such-binary"))
		}
		return exec.CommandContext(ctx, "/bin/sh", "-c", script)
	}
	return &asked
}

// hostShape is the two files Preflight reads to establish that this host's
// own resolution goes through resolved (#186): what /etc/resolv.conf is, and
// what /etc/nsswitch.conf says. The zero shape is an /etc with neither file,
// which is not a host that resolves through resolved — so a row that expects
// Preflight to pass has to say which working shape it is, and a row cannot
// pass by leaving the question unasked.
type hostShape struct {
	// resolvLink is the symlink target of resolv.conf; "" means the file is
	// a regular one holding resolv (or absent when that is empty too).
	resolvLink string
	// resolv is the contents of a regular resolv.conf — of the file at the
	// end of the chain when chainTo names one.
	resolv string
	// chainTo, when set, makes resolvLink a link to a link: the file
	// resolvLink names is itself a symlink to chainTo, a real file, since a
	// chain is only followed where every hop resolves. Both paths are
	// relative to the temp /etc.
	chainTo string
	// nsswitch is the contents of nsswitch.conf; "" means no file.
	nsswitch string
}

// The shapes measured in #186, named once so the rows read as the table
// there. Every passing row of TestPreflightOnLinux that is not about this
// question uses stubSymlink, the Ubuntu default; the rows about it use each
// of the others.
var (
	// stubSymlink is Ubuntu's: /etc/resolv.conf -> ../run/systemd/resolve/
	// stub-resolv.conf. Relative, as Debian's package writes it, so a probe
	// that resolved the target against / instead of /etc would miss it.
	stubSymlink = hostShape{resolvLink: "../run/systemd/resolve/stub-resolv.conf", nsswitch: "hosts: files dns\n"}
	// uplinkSymlink is resolved's other file: the upstreams by address, no
	// 127.0.0.53 anywhere in it, and still resolved's own.
	uplinkSymlink = hostShape{resolvLink: "/run/systemd/resolve/resolv.conf", nsswitch: "hosts: files dns\n"}
	// stubCopy is a regular file naming the stub — a copy, or hand-written.
	stubCopy = hostShape{resolv: "nameserver 127.0.0.53\noptions edns0 trust-ad\n", nsswitch: "hosts: files dns\n"}
	// nssResolve reaches resolved through glibc with no stub in sight: a
	// static file naming the upstream, and `resolve` on the hosts line.
	nssResolve = hostShape{resolv: "nameserver 192.168.65.7\n", nsswitch: "hosts: files resolve [!UNAVAIL=return] dns\n"}
	// static is the container default #186 measured: the upstream in a
	// regular file, `files dns`, and resolved asked by nobody.
	static = hostShape{resolv: "nameserver 192.168.65.7\n", nsswitch: "hosts: files dns\n"}
	// resolvconfLink is a symlink to something that is not resolved's — the
	// resolvconf package's file — with the upstream in it.
	resolvconfLink = hostShape{resolvLink: "/run/resolvconf/resolv.conf", nsswitch: "hosts: files dns\n"}
	// chainedToResolved reaches resolved's uplink file in two hops, the way
	// a host with resolvconf's directory in front of it does. Nothing but
	// the chain says so: the file at the end names the upstream, there is no
	// 127.0.0.53 in it, and the hosts line is `files dns`.
	chainedToResolved = hostShape{resolvLink: "resolvconf/resolv.conf", chainTo: "run/systemd/resolve/resolv.conf",
		resolv: "nameserver 192.168.65.7\n", nsswitch: "hosts: files dns\n"}
)

// hostFiles points the package's /etc at a temp directory holding shape, so
// every row runs from any OS and no test reads the machine it runs on.
func hostFiles(t *testing.T, shape hostShape) {
	t.Helper()
	dir := t.TempDir()
	old := etcDir
	t.Cleanup(func() { etcDir = old })
	etcDir = dir
	resolvConf := filepath.Join(dir, "resolv.conf")
	switch {
	case shape.resolvLink != "":
		if shape.chainTo != "" {
			middle, end := filepath.Join(dir, shape.resolvLink), filepath.Join(dir, shape.chainTo)
			for _, d := range []string{filepath.Dir(middle), filepath.Dir(end)} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatalf("making %s: %v", d, err)
				}
			}
			if err := os.WriteFile(end, []byte(shape.resolv), 0o644); err != nil {
				t.Fatalf("writing the end of the chain: %v", err)
			}
			if err := os.Symlink(end, middle); err != nil {
				t.Fatalf("linking the middle of the chain: %v", err)
			}
		}
		if err := os.Symlink(shape.resolvLink, resolvConf); err != nil {
			t.Fatalf("linking resolv.conf: %v", err)
		}
	case shape.resolv != "":
		if err := os.WriteFile(resolvConf, []byte(shape.resolv), 0o644); err != nil {
			t.Fatalf("writing resolv.conf: %v", err)
		}
	}
	if shape.nsswitch != "" {
		if err := os.WriteFile(filepath.Join(dir, "nsswitch.conf"), []byte(shape.nsswitch), 0o644); err != nil {
			t.Fatalf("writing nsswitch.conf: %v", err)
		}
	}
}

// TestPreflightOnLinux is every answer the machine can give before voya
// writes the drop-in: each one refuses with its own reason, and the two that
// mean "this host does not run resolved" carry the way forward (#155).
func TestPreflightOnLinux(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replies map[string]string
		// host is what /etc says about where resolution goes (#186).
		host hostShape
		// says is what the refusal must contain; empty means it must succeed.
		says []string
		// notSays is what it must not — a refusal can be right about the
		// machine and wrong about what the developer should do next.
		notSays []string
		// advice says whether the developer's own-stub instructions belong on
		// this refusal.
		advice bool
	}{
		{name: "resolved active and new enough passes",
			replies: map[string]string{isActive: "echo active", version: `echo "systemd 255 (255.4-1ubuntu8)"`,
				domains: noCatchAll},
			host: stubSymlink},
		// The conflict this whole check exists for, measured on a real host:
		// a pinned-resolver drop-in claims every name, so voya's server would
		// join a flat list and never be asked. Refused before anything is
		// written.
		{name: "a global catch-all routing domain is refused before writing",
			replies: map[string]string{isActive: "echo active", version: `echo "systemd 259 (259.5-0ubuntu3.4)"`,
				domains: `echo "Global: ~.
Link 2 (enp0s5): localdomain"`},
			host:   stubSymlink,
			says:   []string{"already routes every name", "~.", "joins the global list"},
			advice: true},
		// voya's own domain is not a catch-all, so a re-install on a host
		// voya already wired is not refused by its own file.
		{name: "voya's own routing domain is not a catch-all",
			replies: map[string]string{isActive: "echo active", version: `echo "systemd 259 (259.5-0ubuntu3.4)"`,
				domains: `echo "Global: ~internal
Link 2 (enp0s5): localdomain"`},
			host: stubSymlink},
		// A per-link catch-all loses to ~internal under longest-match
		// routing, so refusing on one would refuse a host that works.
		{name: "a catch-all on a link is not the global scope",
			replies: map[string]string{isActive: "echo active", version: `echo "systemd 259 (259.5-0ubuntu3.4)"`,
				domains: `echo "Global:
Link 2 (enp0s5): ~. localdomain"`},
			host: stubSymlink},
		// Not knowing is not a refusal: resolved is running and new enough by
		// then, so a resolvectl that fails here is a wedged bus and not a
		// host that cannot carry the wiring.
		{name: "a domain query voya could not run does not block the install",
			replies: map[string]string{isActive: "echo active", version: `echo "systemd 259 (259.5-0ubuntu3.4)"`,
				domains: `echo "bus is wedged" >&2; exit 1`},
			host: stubSymlink},
		{name: "a stopped resolved is refused by name",
			replies: map[string]string{isActive: "echo inactive; exit 3"},
			says:    []string{"systemd-resolved is inactive here"}, advice: true},
		{name: "a failed resolved is refused with the state systemd reported",
			replies: map[string]string{isActive: "echo failed; exit 3"},
			says:    []string{"systemd-resolved is failed here"}, advice: true},
		{name: "no systemctl at all is refused as a host without resolved",
			replies: map[string]string{isActive: notFound},
			says:    []string{"systemctl is-active systemd-resolved.service"}, advice: true},
		{name: "a systemctl that fails without a state quotes its complaint",
			replies: map[string]string{isActive: `echo "bus is wedged" >&2; exit 1`},
			says:    []string{"bus is wedged"}, advice: true},
		// The version string is ubuntu:20.04's own, and the refusal was run
		// against that resolved as PID 1 in a container: it rejects the whole
		// address ("Failed to add DNS server address …, ignoring") rather
		// than dropping the port, which is what makes this a refusal.
		//
		// notSays is the half a live run caught: this host *does* run
		// systemd-resolved, so the stub advice's "this host does not run one"
		// was false on the one refusal that prints it to a working resolved.
		{name: "a resolved older than v246 is refused with both versions",
			replies: map[string]string{isActive: "echo active", version: `echo "systemd 245 (245.4-4ubuntu3.24)"`},
			says:    []string{"is v245", "v246 or newer"},
			notSays: []string{"does not run one"}, advice: true},
		// A pre-release names its version too: refusing v255~rc1 as
		// unestablished would turn away a host 9 releases past the threshold,
		// on the one refusal that offers no fallback wiring.
		{name: "a pre-release resolved past the threshold is not refused",
			replies: map[string]string{isActive: "echo active", version: `echo "systemd 255~rc1 (255~rc1-1)"`,
				domains: noCatchAll},
			host: stubSymlink},
		// #186: resolved there, and nothing asking it. The four shapes that
		// do resolve through resolved each pass — the probe is a disjunction
		// of them, because every false negative is a working host turned
		// away — and the container default #186 measured is refused, before
		// the catch-all question is even put.
		{name: "resolv.conf linked to resolved's stub resolves through resolved",
			replies: map[string]string{isActive: "echo active", version: `echo "systemd 257 (257.13-1~deb13u1)"`,
				domains: noCatchAll},
			host: stubSymlink},
		{name: "resolv.conf linked to resolved's own uplink file resolves through resolved",
			replies: map[string]string{isActive: "echo active", version: `echo "systemd 257 (257.13-1~deb13u1)"`,
				domains: noCatchAll},
			host: uplinkSymlink},
		{name: "resolv.conf linked to resolved's file through a second link resolves through resolved",
			replies: map[string]string{isActive: "echo active", version: `echo "systemd 257 (257.13-1~deb13u1)"`,
				domains: noCatchAll},
			host: chainedToResolved},
		{name: "a copy of the stub resolves through resolved",
			replies: map[string]string{isActive: "echo active", version: `echo "systemd 257 (257.13-1~deb13u1)"`,
				domains: noCatchAll},
			host: stubCopy},
		{name: "nss-resolve with neither stub resolves through resolved",
			replies: map[string]string{isActive: "echo active", version: `echo "systemd 257 (257.13-1~deb13u1)"`,
				domains: noCatchAll},
			host: nssResolve},
		{name: "a static resolv.conf naming an upstream and no nss-resolve is refused",
			replies: map[string]string{isActive: "echo active", version: `echo "systemd 257 (257.13-1~deb13u1)"`},
			host:    static,
			says:    []string{"does not go through it", "resolv.conf is not resolved's stub", "nsswitch.conf does not list `resolve`", "stub-resolv.conf"},
			notSays: []string{"does not run one"}, advice: true},
		{name: "resolv.conf linked to a file that is not resolved's is refused",
			replies: map[string]string{isActive: "echo active", version: `echo "systemd 257 (257.13-1~deb13u1)"`},
			host:    resolvconfLink,
			// A link that names a file which is not there is not a missing
			// resolv.conf: the file is in front of the developer, and the
			// refusal says what is wrong with what it names.
			says:    []string{"does not go through it", "resolv.conf is not resolved's stub"},
			notSays: []string{"resolv.conf is missing"}, advice: true},
		{name: "an /etc with neither file is refused",
			replies: map[string]string{isActive: "echo active", version: `echo "systemd 257 (257.13-1~deb13u1)"`},
			// Named as missing, not as files that say the wrong thing:
			// nothing here has contents to go and inspect.
			says:    []string{"does not go through it", "resolv.conf is missing", "nsswitch.conf is missing"},
			notSays: []string{"names no " + stubAddress}, advice: true},
		{name: "a version voya could not establish is refused rather than guessed",
			replies: map[string]string{isActive: "echo active", version: notFound},
			says:    []string{"resolvectl --version", "v246 or newer"}},
		{name: "a version line naming no number is refused with the line",
			replies: map[string]string{isActive: "echo active", version: `echo "who knows"`},
			says:    []string{`said "who knows"`, "v246 or newer"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeCommands(t, tc.replies)
			hostFiles(t, tc.host)
			err := mustFor(t, "linux").Preflight(context.Background())
			if len(tc.says) == 0 {
				if err != nil {
					t.Fatalf("Preflight on a host that can carry the wiring: %v", err)
				}
				return
			}
			var unavailable *UnavailableError
			if !errors.As(err, &unavailable) {
				t.Fatalf("Preflight = %v, want an UnavailableError", err)
			}
			for _, want := range tc.says {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q:\n%v", want, err)
				}
			}
			for _, unwanted := range tc.notSays {
				if strings.Contains(err.Error(), unwanted) {
					t.Errorf("the refusal says %q, which is not true of this host:\n%v", unwanted, err)
				}
			}
			if got := strings.Contains(err.Error(), "dnsmasq"); got != tc.advice {
				t.Errorf("the refusal %s the developer's own wiring; want the other way:\n%v",
					map[bool]string{true: "offers", false: "does not offer"}[got], err)
			}
		})
	}
}

// macOS asks the machine nothing: configd is the OS, and a preflight that
// shelled out there would be a failure mode voya invented for itself.
func TestPreflightOffLinuxRunsNothing(t *testing.T) {
	asked := fakeCommands(t, nil)
	for _, goos := range []string{"darwin", "windows"} {
		w, _ := For(goos)
		if err := w.Preflight(context.Background()); err != nil {
			t.Errorf("Preflight on %s = %v, want nil", goos, err)
		}
	}
	if len(*asked) != 0 {
		t.Errorf("Preflight off Linux ran %v", *asked)
	}
}

// Live is the host-level question the check and status gate the file's meaning
// on: an OS with a wiring is not a machine running it (#155, #96).
func TestLive(t *testing.T) {
	for _, tc := range []struct {
		goos    string
		replies map[string]string
		want    bool
		asks    bool
	}{
		{goos: "darwin", want: true},
		{goos: "windows", want: false},
		{goos: "linux", replies: map[string]string{isActive: "echo active"}, want: true, asks: true},
		{goos: "linux", replies: map[string]string{isActive: "echo inactive; exit 3"}, want: false, asks: true},
		{goos: "linux", replies: map[string]string{isActive: notFound}, want: false, asks: true},
	} {
		t.Run(tc.goos+"/"+map[bool]string{true: "live", false: "not live"}[tc.want], func(t *testing.T) {
			asked := fakeCommands(t, tc.replies)
			w, _ := For(tc.goos)
			if got := w.Live(context.Background()); got != tc.want {
				t.Errorf("Live() on %s = %v, want %v", tc.goos, got, tc.want)
			}
			if asks := len(*asked) > 0; asks != tc.asks {
				t.Errorf("Live() on %s ran %v", tc.goos, *asked)
			}
		})
	}
}

// GlobalCatchAll is the question the check puts to a host whose file is
// installed while the system does not resolve through it (#185): is
// resolved's global scope already routing every name past it? Off Linux the
// question does not exist and the answer is an error, not a false — the two
// are different rows in the check, and a false here would be a measurement no
// resolvectl made.
func TestGlobalCatchAll(t *testing.T) {
	for _, tc := range []struct {
		name    string
		goos    string
		replies map[string]string
		want    bool
		wantErr bool
		asks    bool
	}{
		{name: "linux, global catch-all", goos: "linux", want: true, asks: true,
			replies: map[string]string{domains: `echo "Global: ~.
Link 2 (enp0s5): localdomain"`}},
		{name: "linux, no catch-all", goos: "linux", asks: true,
			replies: map[string]string{domains: noCatchAll}},
		// A per-link ~. loses to the drop-in's ~internal under longest-match
		// routing, so it is not the catch-all this asks about.
		{name: "linux, per-link catch-all only", goos: "linux", asks: true,
			replies: map[string]string{domains: `echo "Global:
Link 2 (enp0s5): ~."`}},
		{name: "linux, no resolvectl", goos: "linux", wantErr: true, asks: true,
			replies: map[string]string{domains: notFound}},
		{name: "darwin", goos: "darwin", wantErr: true},
		{name: "windows", goos: "windows", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked := fakeCommands(t, tc.replies)
			w, _ := For(tc.goos)
			got, err := w.GlobalCatchAll(context.Background())
			if (err != nil) != tc.wantErr {
				t.Errorf("GlobalCatchAll() on %s error = %v, want error: %v", tc.goos, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("GlobalCatchAll() on %s = %v, want %v", tc.goos, got, tc.want)
			}
			if asks := len(*asked) > 0; asks != tc.asks {
				t.Errorf("GlobalCatchAll() on %s ran %v", tc.goos, *asked)
			}
		})
	}
}

// ResolvesThroughResolved is the other question the check puts to that host
// (#204): does this host's own resolution reach resolved at all? The shapes
// themselves are TestPreflightOnLinux's; what this pins is the seam — the
// answer and, on a false, its reason, and that off Linux the answer is an
// error rather than either value, because a false there would be a
// measurement no file was read for. Nothing is run: the question is two
// files, not a command.
func TestResolvesThroughResolved(t *testing.T) {
	for _, tc := range []struct {
		name    string
		goos    string
		host    hostShape
		want    bool
		wantWhy []string
		wantErr bool
	}{
		{name: "linux, stub symlink", goos: "linux", host: stubSymlink, want: true},
		{name: "linux, nss-resolve", goos: "linux", host: nssResolve, want: true},
		{name: "linux, static resolv.conf", goos: "linux", host: static,
			wantWhy: []string{"resolv.conf is not resolved's stub and names no 127.0.0.53",
				"nsswitch.conf does not list `resolve` for hosts"}},
		{name: "darwin", goos: "darwin", host: static, wantErr: true},
		{name: "windows", goos: "windows", host: static, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked := fakeCommands(t, nil)
			hostFiles(t, tc.host)
			w, _ := For(tc.goos)
			got, why, err := w.ResolvesThroughResolved()
			if (err != nil) != tc.wantErr {
				t.Errorf("ResolvesThroughResolved() on %s error = %v, want error: %v", tc.goos, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("ResolvesThroughResolved() on %s = %v, want %v", tc.goos, got, tc.want)
			}
			for _, want := range tc.wantWhy {
				if !strings.Contains(why, want) {
					t.Errorf("ResolvesThroughResolved() on %s why = %q, want it to contain %q", tc.goos, why, want)
				}
			}
			if tc.want && why != "" {
				t.Errorf("ResolvesThroughResolved() on %s = true with a reason: %q", tc.goos, why)
			}
			if len(*asked) > 0 {
				t.Errorf("ResolvesThroughResolved() on %s ran %v; it reads files", tc.goos, *asked)
			}
		})
	}
}

// ResolvedTooOld is the third question the check puts to that host (#206), and
// the one Preflight asks ahead of the other two: is this resolved old enough
// to reject the port voya writes? What this pins is the seam — a measured
// yes with the reason the refusal carries, a measured no with none, a
// version nobody could read as an error rather than a false, and off Linux
// an error rather than either value.
func TestResolvedTooOld(t *testing.T) {
	for _, tc := range []struct {
		name    string
		goos    string
		replies map[string]string
		want    bool
		wantWhy string
		wantErr bool
		asks    bool
	}{
		{name: "linux, too old", goos: "linux", want: true, asks: true,
			replies: map[string]string{version: `echo "systemd 245 (245.4-4ubuntu3.24)"`},
			wantWhy: "systemd-resolved here is v245, and a port in DNS= needs v246 or newer"},
		{name: "linux, the first release that reads a port", goos: "linux", asks: true,
			replies: map[string]string{version: `echo "systemd 246 (246.6-1)"`}},
		{name: "linux, new enough", goos: "linux", asks: true,
			replies: map[string]string{version: `echo "systemd 255 (255.4-1ubuntu8)"`}},
		{name: "linux, no resolvectl", goos: "linux", wantErr: true, asks: true,
			replies: map[string]string{version: notFound}},
		{name: "darwin", goos: "darwin", wantErr: true},
		{name: "windows", goos: "windows", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked := fakeCommands(t, tc.replies)
			w, _ := For(tc.goos)
			got, why, err := w.ResolvedTooOld(context.Background())
			if (err != nil) != tc.wantErr {
				t.Errorf("ResolvedTooOld() on %s error = %v, want error: %v", tc.goos, err, tc.wantErr)
			}
			if got != tc.want || why != tc.wantWhy {
				t.Errorf("ResolvedTooOld() on %s = %v, %q; want %v, %q", tc.goos, got, why, tc.want, tc.wantWhy)
			}
			if asks := len(*asked) > 0; asks != tc.asks {
				t.Errorf("ResolvedTooOld() on %s ran %v", tc.goos, *asked)
			}
		})
	}
}

// Activate is what makes a written file live, and what it does is the OS's
// business: nothing on macOS, a restart on a Linux host that is running
// resolved, and nothing on one that is not — voya's file is not a reason to
// start a service the developer left off.
//
// The restart is the one command here that is a job systemd runs rather than
// a question it answers, and it has a budget of its own for it (#188): the
// rows below pin that it is not the question budget, in both directions — a
// restart the question budget would have killed is reported as the success it
// is, and one that outlives its own budget names that deadline, and says so
// before it starts waiting.
func TestActivate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		goos    string
		replies map[string]string
		said    string
		errHas  string
		// notErrHas is what the failure must not say — a message can name a
		// real failure and still name the wrong cause of it.
		notErrHas string
		// budget shortens [activateBudget], and questions [commandBudget], for
		// the rows that need one deadline to fire and the other not to; 0
		// leaves the real one alone. grace shortens [waitGrace], which is the
		// floor under a row whose deadline fires — 2s of it against a 5s
		// question budget leaves a row that asserts it stayed under that
		// budget less headroom than a busy machine can eat.
		budget, questions, grace time.Duration
		// quiet hands Activate a nil logf, which must be "say nothing" rather
		// than a nil call: cmd/voya passes stderr, and nothing else has a
		// reader to pass.
		quiet bool
		ran   []string
	}{
		{name: "macOS has configd and needs no restart", goos: "darwin"},
		{name: "a live resolved is restarted and the restart reported", goos: "linux",
			replies: map[string]string{isActive: "echo active", restart: "true"},
			said:    "systemd-resolved restarted", ran: []string{isActive, restart}},
		{name: "a nil logf is not spoken to", goos: "linux", quiet: true,
			replies: map[string]string{isActive: "echo active", restart: "true"},
			said:    "systemd-resolved restarted", ran: []string{isActive, restart}},
		{name: "a resolved that is not running is left alone", goos: "linux",
			replies: map[string]string{isActive: "echo inactive; exit 3"}, ran: []string{isActive}},
		{name: "a restart that fails is an error quoting systemd", goos: "linux",
			replies: map[string]string{isActive: "echo active", restart: `echo "Job for systemd-resolved.service failed" >&2; exit 1`},
			errHas:  "Job for systemd-resolved.service failed", ran: []string{isActive, restart}},
		// `systemctl restart` blocks until the job is done, and stopping a
		// wedged resolved can run to TimeoutStopSec — far past the 5s that is
		// right for a question. On the question budget voya's client was
		// killed while systemd carried on and the restart mostly succeeded,
		// and `dns install` reported a failure for a restart that worked
		// (#188). So: a restart that outlives the question budget, on its
		// own budget, is the success it is.
		{name: "a restart that outlives the question budget is not cut short", goos: "linux",
			replies:   map[string]string{isActive: "echo active", restart: "sleep 2"},
			questions: 500 * time.Millisecond,
			said:      "systemd-resolved restarted", ran: []string{isActive, restart}},
		// And past its own budget voya's client is killed while systemd may
		// carry on, so the deadline is read before the error — "signal:
		// killed" would name voya's own timeout as systemd's refusal, on the
		// one failure a re-run does not clear — and the deadline named is the
		// restart's own. The sleep is far past both budgets, and the row also
		// checks Activate came back before [commandBudget] could have fired:
		// that, not the message, is what tells the two budgets apart.
		// The sleep is backgrounded and waited on so the shell *forks* it on
		// every platform. Left as a plain `sleep 8`, macOS's sh execs into
		// it — killing that pid ends the wait — while Linux's dash forks and
		// the grandchild keeps the pipes, so the same row exercised the
		// deadline on one OS and [waitGrace] on the other, and only the
		// Linux leg failed when the grace was missing. Forcing the fork makes
		// this row about the call being bounded, everywhere.
		//
		// The grandchild survives the kill by construction — that is the
		// point — so its sleep is sized to the two things it has to outlive
		// and no longer: this row's 300ms deadline and 200ms grace, and
		// [commandBudget], so that dropping the grace fails the row rather
		// than merely slowing it. 8s does both with room, where the 30s it
		// was leaves an orphan under PID 1 holding pipes for half a minute
		// after the test that made it has passed — five of them under a
		// `-count=5` run.
		{name: "a restart that outlives its own budget names that deadline, not the kill", goos: "linux",
			replies: map[string]string{isActive: "echo active", restart: "sleep 8 & wait"},
			budget:  300 * time.Millisecond, grace: 200 * time.Millisecond,
			errHas: "deadline exceeded after 300ms", notErrHas: "signal: killed",
			ran: []string{isActive, restart}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.budget != 0 {
				old := activateBudget
				activateBudget = tc.budget
				t.Cleanup(func() { activateBudget = old })
			}
			if tc.questions != 0 {
				old := commandBudget
				commandBudget = tc.questions
				t.Cleanup(func() { commandBudget = old })
			}
			if tc.grace != 0 {
				old := waitGrace
				waitGrace = tc.grace
				t.Cleanup(func() { waitGrace = old })
			}
			asked := fakeCommands(t, tc.replies)
			// told is what Activate said, and toldAfter how many commands
			// had run when it said it — the line has to land before the
			// restart it is about, or it explains a wait that is over.
			var told []string
			var toldAfter []int
			logf := func(format string, args ...any) {
				told = append(told, fmt.Sprintf(format, args...))
				toldAfter = append(toldAfter, len(*asked))
			}
			if tc.quiet {
				logf = nil
			}
			w, _ := For(tc.goos)
			began := time.Now()
			said, err := w.Activate(context.Background(), logf)
			took := time.Since(began)
			switch {
			case tc.errHas != "":
				if err == nil || !strings.Contains(err.Error(), tc.errHas) {
					t.Fatalf("Activate() = %q, %v; want an error containing %q", said, err, tc.errHas)
				}
				if tc.notErrHas != "" && strings.Contains(err.Error(), tc.notErrHas) {
					t.Errorf("the failure says %q, which is not what happened: %v", tc.notErrHas, err)
				}
			case err != nil:
				t.Fatalf("Activate() = %v", err)
			case said != tc.said:
				t.Errorf("Activate() said %q, want %q", said, tc.said)
			}
			if got := strings.Join(*asked, "; "); got != strings.Join(tc.ran, "; ") {
				t.Errorf("Activate() ran [%s], want [%s]", got, strings.Join(tc.ran, "; "))
			}
			if tc.budget != 0 && took >= commandBudget {
				t.Errorf("Activate() took %v, past the question budget %v: the restart is not on its own budget", took, commandBudget)
			}
			restarted := slices.Contains(tc.ran, restart)
			switch {
			case tc.quiet || !restarted:
				if len(told) != 0 {
					t.Errorf("Activate() said %q on a row with nothing to wait for", told)
				}
			case len(told) != 1 || !strings.Contains(told[0], "restarting "+resolvedUnit):
				t.Errorf("Activate() said %q, want one line naming the restart of %s", told, resolvedUnit)
			// The line is what a silence of up to [activateBudget] is traded
			// for, so it has to say how long that silence may be: told that
			// voya is waiting but not for how long, a developer under sudo
			// still cannot tell a long restart from a hang.
			case !strings.Contains(told[0], activateBudget.String()):
				t.Errorf("Activate() said %q, want it to name the budget it is about to wait, %s", told, activateBudget)
			// Activate is the uninstall path too — `voya dns uninstall`
			// restarts a live resolved after removing w.Path, so that it
			// stops reading it. A line naming that file as what resolved is
			// about to read is false on half the callers, so the wording that
			// is true on both is pinned positively: a negative on one phrasing
			// of the false version passes for every other phrasing of it.
			case !strings.Contains(told[0], "picks up the change to "+w.Path):
				t.Errorf("Activate() said %q, want it to name the change to %s — on uninstall that file is gone, "+
					"and the restart is what makes resolved stop reading it", told, w.Path)
			case toldAfter[0] != slices.Index(tc.ran, restart):
				t.Errorf("Activate() spoke after %d commands, want before the restart at %d: it explains a wait that is over",
					toldAfter[0], slices.Index(tc.ran, restart))
			}
		})
	}
}

// TestCancelledContextEndsTheQuestion: every host question here runs under
// the caller's context, not one of its own (#177) — so a caller with less
// time than [commandBudget], or a ^C, ends the wait when it says to. Each row
// cancels 100ms into a command that would otherwise sleep past both budgets,
// and asserts the call came back well inside [commandBudget] and, where it
// reports at all, named the cancellation rather than a budget.
//
// The bound is generous — the whole [commandBudget], with the ceiling being
// the point rather than the number — because [waitGrace] is a floor under a
// row whose command is killed, and a busy machine widens the spread above it.
func TestCancelledContextEndsTheQuestion(t *testing.T) {
	old := waitGrace
	waitGrace = 200 * time.Millisecond
	t.Cleanup(func() { waitGrace = old })
	const hang = "sleep 8 & wait"
	linux := mustFor(t, "linux")
	for _, tc := range []struct {
		name    string
		replies map[string]string
		call    func(context.Context) error
	}{
		{"Preflight", map[string]string{isActive: hang},
			linux.Preflight},
		// Live reports no error at all — false is its whole answer — so the
		// row's assertions are that false and the timing below, and the
		// context.Canceled it returns is fabricated to keep the shared error
		// assertions quiet rather than measured from the code. Read the
		// timing as the check that matters: a Live that asked on a context of
		// its own comes back at commandBudget, not at 100 ms.
		{"Live", map[string]string{isActive: hang},
			func(ctx context.Context) error {
				if linux.Live(ctx) {
					return errors.New("Live() = true off a question that never answered")
				}
				return context.Canceled
			}},
		{"GlobalCatchAll", map[string]string{domains: hang},
			func(ctx context.Context) error { _, err := linux.GlobalCatchAll(ctx); return err }},
		{"Activate", map[string]string{isActive: "echo active", restart: hang},
			func(ctx context.Context) error { _, err := linux.Activate(ctx, nil); return err }},
		// Activate gates on the same liveness question, and that gate is the
		// one place a cancellation could turn into a success: [Wiring.Live]
		// answers false for "the context ended first" as much as for "no
		// resolved here", and "" with a nil error reads as nothing to do —
		// `dns install --activate` would print `already installed` with no
		// restart clause and no error, for a restart that never ran.
		{"Activate on the liveness question", map[string]string{isActive: hang},
			func(ctx context.Context) error {
				live, err := linux.Activate(ctx, nil)
				if err == nil {
					return fmt.Errorf("Activate() = %q, nil off a liveness question that never answered", live)
				}
				return err
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeCommands(t, tc.replies)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			time.AfterFunc(100*time.Millisecond, cancel)
			began := time.Now()
			err := tc.call(ctx)
			took := time.Since(began)
			if !errors.Is(err, context.Canceled) {
				t.Errorf("%s(cancelled ctx) = %v, want it to carry context.Canceled", tc.name, err)
			}
			if err != nil && strings.Contains(err.Error(), "after ") {
				t.Errorf("%s(cancelled ctx) = %v, names a budget it did not run out", tc.name, err)
			}
			if took >= commandBudget {
				t.Errorf("%s(cancelled ctx) took %v, past commandBudget %v: the question is on a budget of its own", tc.name, took, commandBudget)
			}
		})
	}

	// A caller's deadline shorter than [activateBudget] is the same story
	// with the other context error: it fired, not the budget, and the failure
	// must not name a budget of 100s as the wait that just ended.
	t.Run("Activate on the caller's deadline", func(t *testing.T) {
		fakeCommands(t, map[string]string{isActive: "echo active", restart: hang})
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		t.Cleanup(cancel)
		began := time.Now()
		_, err := linux.Activate(ctx, nil)
		took := time.Since(began)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Activate(short deadline) = %v, want it to carry context.DeadlineExceeded", err)
		}
		if err != nil && strings.Contains(err.Error(), "after "+activateBudget.String()) {
			t.Errorf("Activate(short deadline) = %v, names the budget when the caller's deadline is what fired", err)
		}
		if took >= commandBudget {
			t.Errorf("Activate(short deadline) took %v, past commandBudget %v", took, commandBudget)
		}
	})
}

// The advice a failed [Wiring.Activate] leaves a developer with has to be the
// command Activate itself runs, or it sends them somewhere voya was not: the
// table above asserts what was run, and this asserts the same line is what
// ReloadCommand hands over. macOS has nothing to hand over — configd reads the
// file as it lands — and saying otherwise would invent a step.
func TestReloadCommandIsWhatActivateRuns(t *testing.T) {
	if got := mustFor(t, "linux").ReloadCommand(); got != restart {
		t.Errorf("linux ReloadCommand() = %q, want %q — the command Activate runs", got, restart)
	}
	for _, goos := range []string{"darwin", "windows"} {
		w, _ := For(goos)
		if got := w.ReloadCommand(); got != "" {
			t.Errorf("%s ReloadCommand() = %q, want nothing to run", goos, got)
		}
	}
}

// The version line is read the way every systemd tool prints it, and the
// number is the release rather than whatever the packaging appended.
func TestResolvedVersionReadsTheReleaseNumber(t *testing.T) {
	for _, tc := range []struct {
		line string
		want int
	}{
		{"systemd 255 (255.4-1ubuntu8.4)", 255},
		{"systemd 245 (245.4-4ubuntu3.24)", 245},
		{"systemd 254 (254.5-1-arch)", 254},
		{"systemd v257 (257.4-1)", 257},
		// PROJECT_VERSION is not always an integer: a pre-release prints one
		// of these, and reading either as "names no version" would refuse a
		// host well past the threshold — with the one refusal that carries no
		// fallback wiring at all.
		{"systemd 250-rc1 (250.rc1-1-arch)", 250},
		{"systemd 255~rc1 (255~rc1-1)", 255},
	} {
		fakeCommands(t, map[string]string{version: "echo " + shellQuote(tc.line) + `; echo "+PAM +AUDIT -SELINUX"`})
		got, err := resolvedVersion(context.Background())
		if err != nil || got != tc.want {
			t.Errorf("resolvedVersion() on %q = %d, %v; want %d", tc.line, got, err, tc.want)
		}
	}
}

// shellQuote quotes a line for the fake's shell, so a version string with
// parentheses in it reaches the parser as one line rather than as a syntax
// error.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
