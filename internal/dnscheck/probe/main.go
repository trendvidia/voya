// Command probe measures how one build of a Go binary resolves a *.internal
// name, printing one line per path:
//
//	system ok 127.0.0.1
//	default fail lookup voya-dns-check-probe.internal: no such host
//	purego fail lookup voya-dns-check-probe.internal on 10.0.0.1:53: no such host
//
// It exists because the measurement ADR 005 requires is a property of a
// build mode — CGO_ENABLED=0 — and a test cannot choose how the test binary
// itself was built. The dns check tests build this command with the mode under
// test and read the lines back (#6); `voya dns check` will run the same probes
// in process. The output is a private seam between this command and those
// tests, not a surface anything else may parse.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/trendvidia/voya/internal/dnscheck"
)

func main() {
	if len(os.Args) < 2 || len(os.Args) > 3 {
		fmt.Fprintln(os.Stderr, "usage: probe <name.internal> [dns-server-addr]")
		os.Exit(2)
	}
	name := os.Args[1]

	// One budget each, not one shared between them. Generous on purpose —
	// nothing here measures latency, and the probes whose answer is "no"
	// often spend a resolver timeout finding out — but a shared deadline
	// makes those timeouts additive, so a slow negative would starve the
	// probes after it and the positive control, which runs last, would be
	// the first to go. A starved control reads to the caller as broken probe
	// machinery, which is the one conclusion it exists to rule out.
	probe := func(path string, f func(context.Context) dnscheck.ProbeResult) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		report(path, f(ctx))
	}

	probe("system", func(ctx context.Context) dnscheck.ProbeResult { return dnscheck.ProbeSystem(ctx, name) })
	probe("default", func(ctx context.Context) dnscheck.ProbeResult { return dnscheck.ProbeDefault(ctx, name) })
	probe("purego", func(ctx context.Context) dnscheck.ProbeResult { return dnscheck.ProbePureGo(ctx, name) })
	if len(os.Args) == 3 {
		// The caller's positive control: resolve against the listener it
		// knows is up, so the negatives above are measurements rather than
		// a broken probe agreeing with itself.
		probe("direct", func(ctx context.Context) dnscheck.ProbeResult {
			return dnscheck.ProbeAt(ctx, os.Args[2], name)
		})
	}
}

func report(path string, r dnscheck.ProbeResult) {
	switch {
	case r.Unmeasured != "":
		// Its own verb, because "the probe could not run" read back as a
		// failure is exactly the misreading #96 removed from the reports.
		fmt.Printf("%s unmeasured %s\n", path, oneLine(r.Unmeasured))
	case r.Err != "":
		fmt.Printf("%s fail %s\n", path, oneLine(r.Err))
	default:
		fmt.Printf("%s ok %s\n", path, strings.Join(r.Addrs, ","))
	}
}

// oneLine keeps free text from breaking the one-line-per-probe format.
func oneLine(s string) string {
	return strings.ReplaceAll(s, "\n", " ")
}
