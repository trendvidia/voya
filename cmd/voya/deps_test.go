package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// ADR 003 keeps the descriptors — and the transcoder that reads them — in
// each service rather than in voya: "voya still proxies bytes and holds no
// descriptors". #225 finally exercised that claim, and doing so put
// connectrpc.com/vanguard and google.golang.org/genproto/googleapis/api on
// this module's require list for one test fixture
// (test/acceptance/rest_test.go), which put grpc-go back into the module
// graph behind them. #236 moved the fixture into a module of its own, so the
// manifest is clean again — go.work's paragraph on that is the record — and
// this pin outlives the move on purpose: a manifest says what a module
// requires, not what a binary links, and the day a transcoder reaches
// cmd/voya through any module at all, this is what notices.
//
// So this is where the binary says it: nothing cmd/voya builds may reach a
// transcoder, grpc-go, or the annotation types the transcoder reads. The
// same shape as client/deps_test.go — a real check plus a control the check
// did not build — but over `go list -deps`, which is the build graph rather
// than the manifest: a require line no import reaches is not a link, and the
// link is what this pins.

// forbiddenInBinary is what a voya that held descriptors would drag in. The
// first two are ADR 003's boundary itself; grpc-go is the older claim
// go.work makes (#28), which survives at the binary even though it no longer
// holds of the module graph.
var forbiddenInBinary = []string{
	"connectrpc.com/vanguard",
	"google.golang.org/genproto/googleapis/api",
	"google.golang.org/grpc",
}

// depsOf lists the packages a build of pkg reaches, with the package's own
// tests included when withTests. Package paths rather than ./... relative
// ones, so the caller's directory does not matter. env is appended to the
// `go list`'s environment — the control uses it to ask for a module only the
// workspace resolves.
func depsOf(t *testing.T, pkg string, withTests bool, env ...string) []string {
	t.Helper()
	args := []string{"list", "-deps"}
	if withTests {
		args = append(args, "-test")
	}
	args = append(args, pkg)
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	var deps []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			deps = append(deps, line)
		}
	}
	return deps
}

// graphCarries reports whether any listed package belongs to module.
func graphCarries(deps []string, module string) bool {
	for _, dep := range deps {
		if dep == module || strings.HasPrefix(dep, module+"/") {
			return true
		}
	}
	return false
}

// TestTheBinaryLinksNoTranscoder is ADR 003's boundary as the linker sees it.
func TestTheBinaryLinksNoTranscoder(t *testing.T) {
	binary := depsOf(t, "github.com/trendvidia/voya/cmd/voya", false)
	// The setup, not just the outcome: an empty or truncated list would
	// satisfy every absence below. The binary's graph is in the hundreds.
	if len(binary) < 100 || !graphCarries(binary, "connectrpc.com/connect") {
		t.Fatalf("go list -deps returned %d packages and no connect-go, so it did not describe the binary", len(binary))
	}
	for _, module := range forbiddenInBinary {
		if graphCarries(binary, module) {
			t.Errorf("cmd/voya links %s — ADR 003 puts the transcoder in the service, not in voya", module)
		}
	}

	// The control: the REST fixture's test graph does reach the two modules
	// that are meant to be reachable from somewhere, so an absence above is
	// this binary's and not a query that stopped finding anything. grpc-go
	// is deliberately not controlled here — no package in this repo imports
	// it outside test/trailer, which is a module of its own. The absence
	// asserted above is this binary's, and the control only has to reach the
	// modules from somewhere.
	//
	// GOWORK is emptied — go's own spelling of "find go.work as usual" —
	// rather than inherited: since #236 the fixture is in a module of its
	// own that the root module does not require and no tag publishes, so
	// under GOWORK=off this query fails with "no required module provides
	// package" and a `go get` remedy that cannot work. The pin above is
	// meaningful either way; only the control needs the workspace.
	fixture := depsOf(t, "github.com/trendvidia/voya/test/acceptance", true, "GOWORK=")
	for _, module := range []string{"connectrpc.com/vanguard", "google.golang.org/genproto/googleapis/api"} {
		if !graphCarries(fixture, module) {
			t.Errorf("the control found no %s in test/acceptance's test graph, so this test proves no absence", module)
		}
	}
}
