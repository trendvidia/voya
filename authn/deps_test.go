package authn

import (
	"go/parser"
	gotoken "go/token" // authn_test.go's token helper owns the plain name
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The dependency arrow points one way: voya imports authn, never the other
// way around. That is the entire reason authn is its own module (#211) — a
// production service importing this package must link a JOSE library and
// nothing of voya's root module, or chameleon, connect-go, the proxy and
// the registry ride into its binary. The same shape as
// internal/config/deps_test.go: a check over a real corpus, with a negative
// control over a fixture the check did not build.
//
// Source imports rather than go.mod, because the workspace is the hole a
// go.mod check would leave: with go.work active, an import of the root
// module resolves and builds before any require line appears, and the
// require would only surface later, under `GOWORK=off go mod tidy` in
// tidy-check. This fails at the import, where the mistake is made.

// rootModule is what no file here may import from; authn's own module path
// sits under it and is carved back out in rootImportsIn.
const rootModule = "github.com/trendvidia/voya/"

const ownModule = "github.com/trendvidia/voya/authn"

// rootImportsIn parses every .go file under dir — test files included,
// since a test's import puts the root module in authn's go.mod all the same
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
		file, err := parser.ParseFile(gotoken.NewFileSet(), path, nil, parser.ImportsOnly)
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
			found = append(found, path+": "+ip)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return found
}

// TestAuthnImportsNothingOfVoya fails on any import of voya's root module
// from this one.
func TestAuthnImportsNothingOfVoya(t *testing.T) {
	for _, imp := range rootImportsIn(t, ".") {
		t.Errorf("%s — authn must not import voya's root module: the split exists so a production "+
			"service linking authn links a JOSE library and nothing else of voya's (#211). Whatever "+
			"was wanted from there, move it or restate it here.", imp)
	}
}

// TestTheImportCheckCanFail is the negative control: the same walk over
// testdata/imports-root, a fixture package written out by hand — not
// derived from the pattern it exists to trip — that imports the root
// module the way a mistake would. A check that cannot fire there would
// read as "no imports" forever.
func TestTheImportCheckCanFail(t *testing.T) {
	found := rootImportsIn(t, filepath.Join("testdata", "imports-root"))
	if len(found) != 1 || !strings.HasSuffix(found[0], "github.com/trendvidia/voya/internal/config") {
		t.Fatalf("the check found %q over the fixture that imports "+
			"github.com/trendvidia/voya/internal/config — it would not catch a real import either", found)
	}
}
