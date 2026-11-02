package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/trendvidia/voya/internal/config"
	"github.com/trendvidia/voya/internal/diag"
	"github.com/trendvidia/voya/internal/registry"
	"github.com/trendvidia/voya/internal/resolverfile"
	diagv1 "github.com/trendvidia/voya/proto/voya/diag/v1"
)

// statusCmd is `voya status` (#151): ADR 005's one screen, from one GetStatus
// — voya itself, every registered service, and the after-the-fact record of
// evictions and reclaims — read from memory over loopback, so the command
// stays instant and infallible whatever state the fleet is in. Plain, it
// probes nothing: the HEALTH column is on the screen blank, so the two
// liveness notions have their two places, and `voya status -health` — the
// same screen as `voya health` (#154), by being the same function,
// [statusScreen] — is what fills it, one Check per service on demand under
// [healthDeadline]. The stream column beside it is what routing acts on (ADR
// 004); their disagreement, once both are filled, is the diagnosis. Health
// is opt-in and not the default for the reason ADR 005 gives: a single
// wedged service must not slow the one command reached for when something
// is already wrong.
//
// The screen is the command's product and goes to stdout, the human rendering
// by default and the GetStatusResponse as pxf under -pxf — the whole document,
// as pxf.Marshal writes it, since one answer is not a stream (ADR 003: pxf,
// never JSON). What the command says about itself — a config it could not
// load — goes to stderr. A voya that is not there, and one that accepts and
// never answers, are answers rather than failures: [unary] prints each as one
// line on stdout, exit 1 and 2.
func statusCmd(args []string, stdout, stderr io.Writer) error {
	var health bool
	asPXF, done, err := pxfFlags("status", statusUsage, args, stdout, stderr, func(fs *flag.FlagSet) {
		fs.BoolVar(&health, "health", false, "probe every service over grpc.health.v1 and fill the HEALTH column")
	})
	if done {
		return err
	}
	return statusScreen(stdout, stderr, asPXF, health)
}

// resolverLine is the "whether DNS resolution is installed" line of ADR 005's
// screen, or "" on an OS that never reads the file. Stat'd here, in the CLI
// process, through the same two seams `voya dns check` uses (#104), and said only where
// the file means something (#96): elsewhere a state word for a file this OS
// never consults would be a claim no probe measured. A stat that failed is
// neither presence nor absence and says so, rather than reading as missing.
//
// The human screen alone carries it; `voya status -pxf` renders the
// GetStatusResponse, and that has no field for the file, on purpose (#166):
// the file is a property of the host running the CLI, not of the daemon,
// and a voya.diag.v1 field would invite the daemon to answer for a machine
// that is not necessarily its own. [statusUsage] and ADR 005 both say so,
// and TestStatusPXFOmitsTheResolverLineAndSaysSo holds the schema, the usage
// and the ADR to it.
func resolverLine(ctx context.Context) string {
	if !osReadsResolverFile(ctx) {
		return ""
	}
	installed, err := resolverInstalled()
	switch {
	case err != nil:
		return fmt.Sprintf("resolver file %s: unknown (%v)", resolverfile.Path, err)
	case installed:
		return fmt.Sprintf("resolver file %s: installed", resolverfile.Path)
	default:
		return fmt.Sprintf("resolver file %s: not installed", resolverfile.Path)
	}
}

// statusTimeFormat is how the screen writes an instant: [diag.TimeFormat], so the two
// commands' timestamps read as one clock.
const statusTimeFormat = diag.TimeFormat

// renderStatus writes the human screen: voya itself, the services table, then
// evictions and reclaims oldest first, so the newest — the one the developer
// whose instance was just deposed is looking for — is last and nearest the
// prompt. now is when the screen is rendered, for the two uptime columns;
// resolver is [resolverLine]'s answer, or "" to say nothing about the file;
// health is [probeHealth]'s report, one entry per service in s's order, or
// nil for the blank HEALTH column of a screen that probed nothing.
func renderStatus(w io.Writer, s *diagv1.GetStatusResponse, now time.Time, resolver string, health *diagv1.HealthReport) {
	self := s.GetSelf()
	fmt.Fprintf(w, "voya %s, pid %d, up %s since %s\n", self.GetVersion(), self.GetPid(),
		uptime(now, self.GetStartedAt()), diag.Instant(self.GetStartedAt()))
	fmt.Fprintf(w, "  proxy %s  dns %s  registration %s\n",
		config.Addr(self.GetProxyPort()), config.Addr(self.GetDnsPort()),
		config.Addr(self.GetRegistrationPort()))
	if resolver != "" {
		fmt.Fprintf(w, "  %s\n", resolver)
	}

	fmt.Fprintf(w, "\nservices (%d)\n", len(s.GetServices()))
	if len(s.GetServices()) == 0 {
		fmt.Fprintln(w, "no services registered")
	} else {
		// The table is rendered whole and then written a line at a time
		// with its trailing spaces cut: tabwriter pads every cell it
		// aligns, so a row whose HEALTH cell is blank — every row of a
		// plain `voya status` — would otherwise end in the STREAM column's
		// pad, and `voya status | grep ' $'` would match every service.
		// The header stays aligned over the column because its own STREAM
		// cell is padded the same way; only what follows the last
		// non-blank cell goes (#164).
		var table bytes.Buffer
		tw := tabwriter.NewWriter(&table, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tAUTHORITY\tPORT\tPID\tUPTIME\tSTREAM\tHEALTH")
		for i, svc := range s.GetServices() {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n", svc.GetName(), svc.GetAuthority(), svc.GetPort(),
				pidColumn(svc.GetPid()), uptime(now, svc.GetSince()), streamWord(svc.GetStream()), healthCell(health, i))
		}
		_ = tw.Flush() // into a bytes.Buffer, which does not fail
		for line := range strings.Lines(table.String()) {
			fmt.Fprintln(w, strings.TrimRight(line, " \n"))
		}
		renderHealthDetails(w, health)
	}

	fmt.Fprintf(w, "\nevictions (%d)\n", len(s.GetEvictions()))
	for _, ev := range s.GetEvictions() {
		fmt.Fprintf(w, "%s  %s: %s was evicted by %s\n", diag.Instant(ev.GetAt()),
			registry.Authority(ev.GetEvicted().GetName()), diag.InstanceWords(ev.GetEvicted()), diag.InstanceWords(ev.GetIncoming()))
	}
	fmt.Fprintf(w, "\nreclaims (%d)\n", len(s.GetReclaims()))
	for _, rc := range s.GetReclaims() {
		line := fmt.Sprintf("%s  %s: %s took its name back", diag.Instant(rc.GetAt()),
			registry.Authority(rc.GetInstance().GetName()), diag.InstanceWords(rc.GetInstance()))
		if rc.GetEvictedAt() != nil {
			line += fmt.Sprintf(", %s after losing it", rc.GetAt().AsTime().Sub(rc.GetEvictedAt().AsTime()).Round(time.Second))
		}
		fmt.Fprintln(w, line)
	}
}

// pidColumn is the PID cell: the pid, or "-" for a registration that carried
// none, so the column never reads a missing pid as process 0.
func pidColumn(pid int32) string {
	if pid == 0 {
		return "-"
	}
	return strconv.FormatInt(int64(pid), 10)
}

// streamWord is the STREAM cell: the enum's own value name, lower-cased and
// without the prefix, so a value added later renders itself without a table
// here going stale.
func streamWord(st diagv1.StreamState) string {
	return strings.ToLower(strings.TrimPrefix(st.String(), "STREAM_STATE_"))
}

// uptime is now minus since, whole seconds, or "-" when since is unset. A
// clock that ran backwards — the daemon's and the CLI's are the same
// machine's, but nothing forbids a step in between — reads 0s rather than a
// negative uptime.
func uptime(now time.Time, since *timestamppb.Timestamp) string {
	if since == nil {
		return "-"
	}
	return max(now.Sub(since.AsTime()), 0).Round(time.Second).String()
}

func statusUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  voya status [-health] [-pxf]

One screen from the running voya, read from memory over loopback: voya itself
(version, pid, uptime, the three ports, and on macOS whether the resolver
file is installed), every registered service with its name, authority, port,
pid, uptime and stream state, then the evictions and reclaims recorded since
voya started, oldest first. HEALTH is blank unless asked for: -health probes
every service over grpc.health.v1, on demand, and is the same as "voya health".

  -health  fill the HEALTH column: one Check per service, through the proxy,
           within a short bound; nothing is evicted on the result
  -pxf     render the GetStatusResponse as pxf instead of the screen — the
           HealthReport under -health. The document omits the resolver
           file line: that file is a property of the host running this
           command, not of the daemon, and is not in voya.diag.v1; a
           script checking DNS wiring reads "voya dns check"'s exit code
`)
}
