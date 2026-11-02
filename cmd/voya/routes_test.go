package main

import (
	"bytes"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/trendvidia/protowire-go/encoding/pxf"

	"github.com/trendvidia/voya/internal/registry"
	diagv1 "github.com/trendvidia/voya/proto/voya/diag/v1"
)

// --- #152: voya routes ---------------------------------------------------------

// TestVoyaRoutesPrintsTheTable is #152's criteria in one daemon: empty first
// — the one line, exit 0, and under -pxf that line on stderr with an empty
// document on stdout — then registered services print one row each with the
// authorities and upstream ports right, sorted by authority; and -pxf renders
// the ListRoutesResponse and nothing else, in that same order.
func TestVoyaRoutesPrintsTheTable(t *testing.T) {
	skipWithoutSIGTERM(t)
	v := startVoyaRun(t)
	v.waitUntilUp(t)

	empty := startVoyaCmd(t, v, "routes")
	if code := empty.exitCode(t, "on an empty registry"); code != 0 {
		t.Fatalf("`voya routes` on an empty registry exited %d\n%s", code, empty.Output())
	}
	if got := strings.TrimSpace(empty.Stdout.String()); got != "no services registered" || empty.Stderr.String() != "" {
		t.Errorf("`voya routes` on an empty registry printed stdout %q stderr %q, want the one line on stdout",
			empty.Stdout.String(), empty.Stderr.String())
	}
	emptyPXF := startVoyaCmd(t, v, "routes", "-pxf")
	if code := emptyPXF.exitCode(t, "on an empty registry, as pxf"); code != 0 {
		t.Fatalf("`voya routes -pxf` on an empty registry exited %d\n%s", code, emptyPXF.Output())
	}
	if got := strings.TrimSpace(emptyPXF.Stderr.String()); got != "no services registered" {
		t.Errorf("`voya routes -pxf` on an empty registry said %q on stderr, want the one line there", emptyPXF.Stderr.String())
	}
	var back diagv1.ListRoutesResponse
	if err := pxf.Unmarshal([]byte(emptyPXF.Stdout.String()), &back); err != nil || len(back.GetRoutes()) != 0 {
		t.Errorf("`voya routes -pxf` on an empty registry printed %q on stdout: %v", emptyPXF.Stdout.String(), err)
	}

	// Registered in the reverse of authority order, so the sort is exercised
	// and not merely the order the daemon answers in. `alpha-two` is the
	// case where the two orders genuinely differ rather than only arriving
	// differently: ListRoutes answers in name order — alpha, alpha-two, zed —
	// and '-' sorts below the '.' that opens the suffix, so authority order
	// leads with alpha-two.internal. Both renderings below are held to it.
	zed := echoService(t, v, "zed")
	alpha := echoService(t, v, "alpha")
	alphaTwo := echoService(t, v, "alpha-two")
	waitRouted(t, v, "zed.internal")
	waitRouted(t, v, "alpha.internal")
	waitRouted(t, v, "alpha-two.internal")
	wantRows := []struct {
		name string
		port uint32
	}{{"alpha-two", alphaTwo}, {"alpha", alpha}, {"zed", zed}}

	r := startVoyaCmd(t, v, "routes")
	if code := r.exitCode(t, "against three services"); code != 0 {
		t.Fatalf("`voya routes` exited %d\n%s", code, r.Output())
	}
	if r.Stderr.String() != "" {
		t.Errorf("`voya routes` wrote to stderr: %q", r.Stderr.String())
	}
	lines := r.lines()
	if len(lines) != len(wantRows)+1 {
		t.Fatalf("`voya routes` printed %d lines, want a header and %d rows:\n%s", len(lines), len(wantRows), r.Stdout.String())
	}
	if got := strings.Fields(lines[0]); !slices.Equal(got, []string{"AUTHORITY", "SERVICE", "UPSTREAM"}) {
		t.Errorf("header = %q", lines[0])
	}
	for i, want := range wantRows {
		got := strings.Fields(lines[i+1])
		if !slices.Equal(got, []string{want.name + ".internal", want.name, "127.0.0.1:" + strconv.FormatUint(uint64(want.port), 10)}) {
			t.Errorf("row %d = %q, want %s.internal %s 127.0.0.1:%d", i, lines[i+1], want.name, want.name, want.port)
		}
	}

	p := startVoyaCmd(t, v, "routes", "-pxf")
	if code := p.exitCode(t, "rendering pxf"); code != 0 {
		t.Fatalf("`voya routes -pxf` exited %d\n%s", code, p.Output())
	}
	if p.Stderr.String() != "" {
		t.Errorf("`voya routes -pxf` wrote to stderr: %q", p.Stderr.String())
	}
	// A message of its own rather than the one the empty case parsed into:
	// Unmarshal appends to a repeated field, so counting routes in a reused
	// message counts both answers.
	var back3 diagv1.ListRoutesResponse
	if err := pxf.Unmarshal([]byte(p.Stdout.String()), &back3); err != nil {
		t.Fatalf("`voya routes -pxf` did not print one ListRoutesResponse: %v\n%s", err, p.Stdout.String())
	}
	// The document is in the table's order too, so a pipe and a terminal
	// reading the same registry read the same order.
	if len(back3.GetRoutes()) != len(wantRows) {
		t.Fatalf("parsed back %d routes, want %d:\n%v", len(back3.GetRoutes()), len(wantRows), &back3)
	}
	for i, want := range wantRows {
		if got := back3.GetRoutes()[i]; got.GetService() != want.name ||
			got.GetUpstream() != "127.0.0.1:"+strconv.FormatUint(uint64(want.port), 10) {
			t.Errorf("route %d parsed back as %v, want %s on 127.0.0.1:%d", i, got, want.name, want.port)
		}
	}
	if !strings.HasPrefix(p.Stdout.String(), "routes = [") {
		t.Errorf("`voya routes -pxf` stdout does not begin with the document: %q", p.Stdout.String())
	}
}

// TestRenderRoutesSortsByAuthority drives the table in process on an answer
// out of authority order, because a test through the daemon cannot tell a
// sort from a pass-through for the names it registers.
func TestRenderRoutesSortsByAuthority(t *testing.T) {
	var b bytes.Buffer
	renderRoutes(&b, &diagv1.ListRoutesResponse{Routes: []*diagv1.Route{
		{Authority: "zed.internal", Service: "zed", Upstream: "127.0.0.1:2"},
		{Authority: "alpha.internal", Service: "alpha", Upstream: "127.0.0.1:1"},
	}})
	want := "AUTHORITY       SERVICE  UPSTREAM\nalpha.internal  alpha    127.0.0.1:1\nzed.internal    zed      127.0.0.1:2\n"
	if b.String() != want {
		t.Errorf("renderRoutes printed:\n%s\nwant:\n%s", b.String(), want)
	}

	// The order the daemon does give, and the reason the sort is not a
	// restatement of it: `api` and `api-v2` are both names registry.ValidName
	// accepts, ListRoutes answers them in that name order, and '-' sorts
	// below the '.' opening the suffix — so the authority order is the other
	// way round.
	if err := registry.ValidName("api-v2"); err != nil {
		t.Fatalf("the case rests on `api-v2` being registrable: %v", err)
	}
	b.Reset()
	renderRoutes(&b, &diagv1.ListRoutesResponse{Routes: []*diagv1.Route{
		{Authority: registry.Authority("api"), Service: "api", Upstream: "127.0.0.1:1"},
		{Authority: registry.Authority("api-v2"), Service: "api-v2", Upstream: "127.0.0.1:2"},
	}})
	want = "AUTHORITY        SERVICE  UPSTREAM\napi-v2.internal  api-v2   127.0.0.1:2\napi.internal     api      127.0.0.1:1\n"
	if b.String() != want {
		t.Errorf("renderRoutes printed:\n%s\nwant:\n%s", b.String(), want)
	}
}
