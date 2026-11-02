package voya

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The dependency arrow points one way: voya imports client, never the other
// way around. That is the entire reason client is its own module (#36) —
// every service in the fleet imports this package, so a root-module import
// here puts the daemon's whole go.mod on every service's build graph,
// chameleon included. The same
// shape as authn/deps_test.go: a check over a real corpus, with a negative
// control over a fixture the check did not build.
//
// Source imports rather than go.mod, because the workspace is the hole a
// go.mod check would leave: with go.work active, an import of the root
// module resolves and builds before any require line appears, and the
// require would only surface later, under `GOWORK=off go mod tidy` in
// tidy-check. This fails at the import, where the mistake is made.

// rootModule is what no file here may import from; the module paths carved
// back out in rootImportsIn sit under it.
const rootModule = "github.com/trendvidia/voya/"

const ownModule = "github.com/trendvidia/voya/client"

// protoModule is the one voya module client may import: the generated
// registration schema is what the package exists to speak, and proto is a
// leaf whose requirements are connect-go and protobuf — nothing a fleet
// service does not already carry.
const protoModule = "github.com/trendvidia/voya/proto"

// rootImportsIn parses every .go file under dir — test files included,
// since a test's import puts the root module in client's go.mod all the same
// — and reports each import of the root module, as "file: path". testdata
// is skipped: that is where the negative control's corpus lives.
func rootImportsIn(t *testing.T, dir string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" && path != dir {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range file.Imports {
			ip, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if !strings.HasPrefix(ip, rootModule) {
				continue
			}
			if ip == ownModule || strings.HasPrefix(ip, ownModule+"/") {
				continue
			}
			if ip == protoModule || strings.HasPrefix(ip, protoModule+"/") {
				continue
			}
			found = append(found, path+": "+ip)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return found
}

// TestClientImportsOnlyProtoOfVoya fails on any import of voya's root module
// from this one.
func TestClientImportsOnlyProtoOfVoya(t *testing.T) {
	for _, imp := range rootImportsIn(t, ".") {
		t.Errorf("%s — client must not import voya's root module: the split exists so a fleet "+
			"service linking client links connect-go, the schemas and nothing of the daemon's (#36). "+
			"A test that needs the real daemon belongs in internal/registration/join_test.go, where "+
			"the arrow already points this way; anything else, move it or restate it here.", imp)
	}
}

// TestTheImportCheckCanFail is the negative control: the same walk over
// testdata/imports-root, a fixture package written out by hand — not
// derived from the pattern it exists to trip — that imports the root
// module the way a mistake would, an allowed proto import beside it to
// prove the carve-out does not swallow the hit. A check that cannot fire
// there would read as "no imports" forever.
func TestTheImportCheckCanFail(t *testing.T) {
	found := rootImportsIn(t, filepath.Join("testdata", "imports-root"))
	if len(found) != 1 || !strings.HasSuffix(found[0], "github.com/trendvidia/voya/internal/registry") {
		t.Fatalf("the check found %q over the fixture that imports "+
			"github.com/trendvidia/voya/internal/registry — it would not catch a real import either", found)
	}
}
