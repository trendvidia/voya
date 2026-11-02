package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This is the package `go install github.com/trendvidia/voya/cmd/voya@version`
// builds, so this is where its one build-graph precondition is pinned: the
// root module's go.mod may carry no `replace` and no `exclude`. `go install
// pkg@version` refuses a module that does — "it must not contain directives
// (replace and exclude) that would cause it to be interpreted differently than
// if it were the main module" — and ADR 001 says voya is distributed that way.
//
// It had not been true since the module split (#36) and nothing was red for
// four days (#231): `make install` is a local `go install ./cmd/voya`, the
// other mode, where this checkout is the main module and replaces are honoured,
// and `make tidy-check` is happy with a replace as long as go.sum agrees with
// it. Both stay green while the documented route is broken, which is why the
// check has to read the file rather than build something.
//
// The negative control runs the same detector over go.mod files kept in
// testdata rather than assembled here from the pattern they are searched for,
// the way internal/config/deps_test.go keeps its pre-upgrade copy — one per
// form the detector has to match, because a control that covers one arm leaves
// the others free to stop matching.

// moduleDirectives reports every `replace` or `exclude` in mod, as
// "<directive> <argument>", block form included. Comments are cut first, for
// the two cases inside a block: a comment line there would be reported as a
// directive of its own, since the block arm reports every line it sees, and a
// trailing one would ride along in the name. A whole-line comment outside a
// block needs no cutting — it starts with "//", so it never matches a
// directive prefix — which is what lets the go.mod this guards spend a
// paragraph on why it has no replaces. testdata/go.mod.forms carries all of
// it.
func moduleDirectives(mod string) []string {
	var found []string
	block := ""
	for _, raw := range strings.Split(mod, "\n") {
		line := strings.TrimSpace(raw)
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		if block != "" {
			if line == ")" {
				block = ""
				continue
			}
			found = append(found, block+" "+line)
			continue
		}
		for _, directive := range []string{"replace", "exclude"} {
			rest, ok := strings.CutPrefix(line, directive)
			if !ok || (rest != "" && !strings.HasPrefix(rest, " ") &&
				!strings.HasPrefix(rest, "\t") && !strings.HasPrefix(rest, "(")) {
				continue
			}
			switch rest = strings.TrimSpace(rest); rest {
			case "(":
				block = directive
			case "":
			default:
				found = append(found, directive+" "+rest)
			}
			break
		}
	}
	return found
}

// readGoMod reads a go.mod for the detector to run over.
func readGoMod(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

// TestRootGoModCarriesNoReplaceOrExclude is the pin. Siblings that resolve
// through go.work inside the repo have to resolve through a published version
// outside it, so the fix for a failure here is a tag and a require, never a
// replace put back.
func TestRootGoModCarriesNoReplaceOrExclude(t *testing.T) {
	for _, directive := range moduleDirectives(readGoMod(t, filepath.Join("..", "..", "go.mod"))) {
		t.Errorf("the root go.mod carries %q: `go install github.com/trendvidia/voya/cmd/voya@version` "+
			"refuses a module with replace or exclude directives, and ADR 001 distributes voya that "+
			"way (#231). Cut a tag for the sibling and bump the require instead — go.work is what "+
			"makes the working tree compile in here.", directive)
	}
}

// TestTheDirectiveCheckCanFail runs the detector over files that do carry
// directives. Without it, a detector that had quietly stopped matching
// anything would leave the test above passing over nothing — and one fixture
// is not enough for that: with only go.mod.pre-231, dropping "exclude" from
// the directive list and neutering the single-line branch each left both tests
// green, so two of the three arms were pinned by nothing.
func TestTheDirectiveCheckCanFail(t *testing.T) {
	for _, tc := range []struct {
		file string
		want []string
	}{
		// The real root go.mod as it stood before #231: three intra-repo
		// replaces, block form.
		{"go.mod.pre-231", []string{
			"replace github.com/trendvidia/voya/authn => ./authn",
			"replace github.com/trendvidia/voya/client => ./client",
			"replace github.com/trendvidia/voya/proto => ./proto",
		}},
		// The forms no go.mod in the repo carries, so no real file covers
		// them: single-line `replace`, which is what `go mod edit -replace`
		// writes, and `exclude` in both forms.
		{"go.mod.forms", []string{
			"replace github.com/trendvidia/voya/proto => ../proto",
			"exclude google.golang.org/protobuf v1.36.10",
			"exclude golang.org/x/net v0.56.0",
			"exclude golang.org/x/text v0.39.0",
		}},
	} {
		t.Run(tc.file, func(t *testing.T) {
			got := moduleDirectives(readGoMod(t, filepath.Join("testdata", tc.file)))
			if len(got) != len(tc.want) {
				t.Fatalf("detector found %d directives in testdata/%s, want %d: got %q",
					len(got), tc.file, len(tc.want), got)
			}
			for i, w := range tc.want {
				if got[i] != w {
					t.Errorf("directive %d is %q, want %q", i, got[i], w)
				}
			}
		})
	}
}
