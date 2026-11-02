package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"

	"github.com/trendvidia/voya/internal/proxy"
	diagv1 "github.com/trendvidia/voya/proto/voya/diag/v1"
)

// healthDeadline bounds one grpc.health.v1 Check from `voya health`, and so
// the command: every probe runs at once, so a fleet of wedged services costs
// this once, not once each. ADR 005 asks for "a short timeout" and no more; a
// couple of seconds is that. It is the same figure as [diagDeadline] but not
// the same constant, because the argument is different — that one bounds a
// memory read across loopback, this one bounds a service answering a
// question about itself, which a debugger holds a service at a breakpoint
// from doing for as long as the developer likes. Longer than this and `voya
// health` against a fleet with one paused member is a command that hangs;
// shorter and a service busy with a request it will finish reads as wedged.
// The bound is the CLI's own: the proxy holds no timeout of its own on the
// upstream leg, so `timeout` here is always this deadline, and the service
// that missed it is still registered — the result is a report, not an
// eviction (ADR 004).
const healthDeadline = 2 * time.Second

// Health words that are not a ServingStatus name: the two ADR 005 names, the
// one for a service that was reached and answered no verdict, and the one for
// a service that answered "slow down" (its amendment of 2026-08-20).
const (
	// healthTimeout: the service accepted and did not answer within
	// [healthDeadline]. What a service paused on a breakpoint reads as — the
	// answer the developer wanted, ADR 005 says, and not a reason to
	// deregister it.
	healthTimeout = "timeout"
	// healthUnreachable: voya could not reach the registered port — the
	// proxy's own `unavailable`, whose message is the diagnostic voya writes
	// for the request (carried on the Connect envelope's 503 here, and the
	// same text the plain-text path writes under 502). That message is the
	// detail: the error response is ADR 005's primary diagnostic surface, and
	// this is where a developer reads it without a curl.
	healthUnreachable = "unreachable"
	// healthError: the service was reached and answered something other
	// than a health verdict — a 404 from one that serves no grpc.health.v1,
	// a 200 of text from one that answers every path, not_found from a
	// health server that does not know the empty name, or an `unavailable`
	// of the service's own (a 503, a Checker shedding load). The detail is
	// the Connect code and the message. Its own word, because "unreachable"
	// is false of a service that answered, and the code's name would be
	// `unknown` for the text case — a ServingStatus a service can mean.
	healthError = "error"
	// healthThrottled: the service was reached and answered
	// resource_exhausted — its own limiter shedding load (#233, ADR 005's
	// amendment of 2026-08-20). Its own word because `error` sends a
	// developer hunting for a failure when the service is saying "slow
	// down", and it is only ever the *service's* throttle: the probe carries
	// [proxy.ProbeHeader] and spends no budget at voya. That the probe is
	// exempt is what makes it true, and the [voyaDiagnostic] prefix is what
	// makes it structural — voya's own throttle carries the same Connect
	// code (internal/proxy's rate_limit outcome), so a daemon that predates
	// the exemption and throttles a probe of its own reads as [healthError]
	// naming voya, never as this word blaming the service.
	healthThrottled = "throttled"
)

// voyaDiagnostic prefixes every routing diagnostic internal/proxy writes, and
// is what tells voya's own `unavailable` from a service's. Both arrive at the
// probe as connect.CodeUnavailable — the proxy's three unreachable outcomes
// collapse onto that code, and so does a 503 or an `unavailable` a reached
// service answers with itself — so the code alone cannot carry the
// distinction [healthUnreachable] and [healthError] are two words for. The
// prefix is the proxy's, restated here rather than imported because it is
// written inline at each diagnose call site; the acceptance suite's
// assertVoyaDiagnostic keys on the same string, and TestHealthWords pins this
// end of it.
const voyaDiagnostic = "voya: "

// healthCmd is `voya health` (#154): ADR 005's second supporting surface. One
// GetStatus for the list — the same call `status` makes — then one
// grpc.health.v1 Check per registered service, all at once, each under
// [healthDeadline], each through the proxy under the service's own authority
// so that a green cell also proves voya routes that name. Then the `status`
// screen with the HEALTH column filled — `voya status -health` reaches the
// same screen through the same [statusScreen] — or the HealthReport as pxf
// under -pxf.
//
// Three things ADR 004 fixes, because it rejected probing as an eviction
// mechanism: no interval, no background goroutine — every probe is a
// goroutine of this process, joined before the screen is written, and none
// survives the command — and nothing is evicted on the result. The daemon's
// part is GetStatus and routing the probes; it never learns what they found.
//
// Exit 0 whatever the probes said: this is a report, and a `not_serving`
// service is information rather than a command failure. The two exits are
// `status`'s — voya not there, voya not answering — from [unary].
func healthCmd(args []string, stdout, stderr io.Writer) error {
	asPXF, done, err := pxfFlag("health", healthUsage, args, stdout, stderr)
	if done {
		return err
	}
	return statusScreen(stdout, stderr, asPXF, true)
}

// statusScreen is the body `status` and `health` share: GetStatus, the
// probes when asked, and one of the two renderings. `voya status -health`
// and `voya health` are the same screen because they are the same call.
//
// stderr is where [unary]'s two not-a-failure answers go under -pxf, and
// nothing else: see [prose].
func statusScreen(stdout, stderr io.Writer, asPXF, health bool) error {
	ctx := context.Background()
	target, err := dialDiag(ctx)
	if err != nil {
		return err
	}
	defer target.close()
	res, err := unary(ctx, target, prose(asPXF, stdout, stderr), target.client.GetStatus, &diagv1.GetStatusRequest{})
	if err != nil {
		return err
	}
	var report *diagv1.HealthReport
	if health {
		report = probeHealth(ctx, target, res.GetServices())
	}
	if asPXF {
		if health {
			return pxfDocument(stdout, report)
		}
		return pxfDocument(stdout, res)
	}
	renderStatus(stdout, res, time.Now(), resolverLine(ctx), report)
	return nil
}

// probeHealth asks every service in services for its health, all at once,
// each under [healthDeadline], and returns the answers in the same order —
// one ServiceHealth per Service, so the report reads beside the registry
// entry it is about. It returns once every probe has: the deadline bounds
// each, so it bounds the join.
//
// The probe is a Connect unary Check to
// http://<authority>:<proxy_port>/grpc.health.v1.Health/Check over target's
// transport — which dials the proxy port whatever the URL names — so the
// request enters voya as a service's would and is routed by authority. The
// empty service name is the gRPC health protocol's "the server as a whole",
// which is the question voya has about a registered instance.
func probeHealth(ctx context.Context, target *diagTarget, services []*diagv1.Service) *diagv1.HealthReport {
	report := &diagv1.HealthReport{Services: make([]*diagv1.ServiceHealth, len(services))}
	httpClient := &http.Client{Transport: probeMarked{target.transport}}
	var wg sync.WaitGroup
	for i, svc := range services {
		wg.Go(func() {
			url := "http://" + svc.GetAuthority() + ":" + strconv.FormatUint(uint64(target.proxyPort), 10)
			ctx, cancel := context.WithTimeout(ctx, healthDeadline)
			defer cancel()
			res, err := grpchealth.NewClient(httpClient, url).Check(ctx, &grpchealth.CheckRequest{})
			status, detail := healthWords(res, err)
			report.Services[i] = &diagv1.ServiceHealth{Service: svc, Status: status, Detail: detail}
		})
	}
	wg.Wait()
	return report
}

// probeMarked sends every request with [proxy.ProbeHeader], which is what
// keeps a probe from spending the probed service's rate limit budget (#233):
// a report that charged a service for being asked about would show `throttled`
// for a healthy service exactly when a developer is investigating a throttle.
// The header is the probe's one divergence from the call a real caller makes
// — the proxy strips it before forwarding, so the upstream leg diverges not
// at all — and nothing else about routing, codec or headers changes, because
// the probe's value is that it is representative (#218's trap).
type probeMarked struct{ base http.RoundTripper }

func (p probeMarked) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context()) // RoundTrippers must not mutate the request they were handed
	r.Header.Set(proxy.ProbeHeader, "health")
	return p.base.RoundTrip(r)
}

// healthWords turns one Check's answer or failure into the HEALTH cell and
// its detail. An answer is the ServingStatus name, lower-cased the way
// [streamWord] lower-cases the STREAM column — `serving`, `not_serving`,
// `unknown` — with no detail. A deadline that expired is [healthTimeout]. The
// proxy's own `unavailable` — the port refused, or nothing is serving the
// name — is [healthUnreachable], with voya's diagnostic as the detail. A
// `resource_exhausted` is [healthThrottled] — the service's own throttle,
// read with the message the way `unavailable` is, so voya's own (a daemon
// older than the probe exemption) stays [healthError] rather than blaming
// the service (#233). Anything else is [healthError], with the code and the
// service's own message as the detail.
//
// `unavailable` is read together with the message and not on its own, because
// the code is not voya's to claim: a service voya reached and routed to can
// answer 503, or return unavailable from its own Checker, and Connect hands
// the probe the same code for that as for a port voya never reached. Only a
// message carrying [voyaDiagnostic] is voya saying it could not reach the
// port; a service's own unavailable is [healthError], which is the word for
// reached-and-no-verdict — `unreachable` would be false of it, and false in
// the direction that sends a developer to look at the wrong process.
func healthWords(res *grpchealth.CheckResponse, err error) (status, detail string) {
	if err == nil {
		return servingWord(res.Status), ""
	}
	if errors.Is(err, context.DeadlineExceeded) || connect.CodeOf(err) == connect.CodeDeadlineExceeded {
		return healthTimeout, ""
	}
	code, message := connect.CodeOf(err), connectMessage(err).Error()
	if code == connect.CodeUnavailable && strings.HasPrefix(message, voyaDiagnostic) {
		return healthUnreachable, message
	}
	if code == connect.CodeResourceExhausted && !strings.HasPrefix(message, voyaDiagnostic) {
		return healthThrottled, code.String() + ": " + message
	}
	return healthError, code.String() + ": " + message
}

// servingWord is a ServingStatus as the HEALTH cell prints it: the enum's
// value name without the prefix, lower-cased, so it reads beside the STREAM
// column's `attached`. grpchealth's own String is the same three words for
// the three values Check can answer with; a value it does not name — the
// protocol reserves 3 for Watch — renders as `status_N`, which is the shape
// the library's String falls back to too. Restated here rather than called,
// because the ADR fixes this vocabulary and a dependency bump is not a place
// the screen's words may quietly change; TestHealthWords is the pin.
func servingWord(s grpchealth.Status) string {
	switch s {
	case grpchealth.StatusUnknown:
		return "unknown"
	case grpchealth.StatusServing:
		return "serving"
	case grpchealth.StatusNotServing:
		return "not_serving"
	}
	return "status_" + strconv.FormatUint(uint64(s), 10)
}

// healthCell is the HEALTH column for one row: the report's word for that
// service, or "" when the screen carries no report — plain `voya status`,
// which probes nothing.
func healthCell(report *diagv1.HealthReport, i int) string {
	if report == nil || i >= len(report.GetServices()) {
		return ""
	}
	return report.GetServices()[i].GetStatus()
}

// renderHealthDetails writes, after the table, the detail of every probe that
// has one — the proxy's diagnostic for an unreachable port, a service's own
// error — each under the service's name and indented, so the table stays a
// table and the developer still reads why. Nothing is written when no probe
// has a detail.
func renderHealthDetails(w io.Writer, report *diagv1.HealthReport) {
	for _, sh := range report.GetServices() {
		if sh.GetDetail() == "" {
			continue
		}
		fmt.Fprintf(w, "\n%s: %s\n", sh.GetService().GetName(), sh.GetStatus())
		for line := range strings.SplitSeq(strings.TrimRight(sh.GetDetail(), "\n"), "\n") {
			// The indent goes on and then the tail comes off, so a blank
			// line stays blank rather than carrying the indent as trailing
			// spaces — and a line the service ended in whitespace of its
			// own loses that too, which the empty-line check this replaced
			// did not (#164). The detail is the service's text; voya adds
			// no trailing space to it.
			fmt.Fprintln(w, strings.TrimRight("  "+line, " \t"))
		}
	}
}

func healthUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  voya health [-pxf]

The "voya status" screen with the HEALTH column filled: one grpc.health.v1
Check per registered service, on demand, through the proxy under the
service's own authority, all at once and each within a short bound. A cell
reads the service's answer (serving, not_serving, unknown), timeout when it
accepted and did not answer, unreachable when voya could not reach its port —
voya's diagnostic printed under the table — or error when it answered no
verdict at all, the code and message under the table. Nothing is evicted on
the result, and the daemon runs no probe of its own. Exit 0 whatever the
answers; the same as "voya status -health".

  -pxf   render the HealthReport as pxf instead of the screen
`)
}
