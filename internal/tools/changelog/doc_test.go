package main

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/trendvidia/voya/internal/changelog"
)

// main.go's package comment walks through the curation pass on named
// fragments — which one folds into which — and `make changelog VERSION=…`
// deletes every fragment it consumes, so the first release leaves that
// comment naming files that no longer exist. `make changelog-check` validates
// the directory, not the prose about it. This test pins the prose: every
// fragment path the comment names must be a fragment that is really there.
//
// It fails at the release, and that is the point (#136): `make changelog
// VERSION=…` deletes every fragment, so every path the comment names goes at
// once — the case #136 was written for, a fragment the curation pass folded
// away while others remain, is the same failure one path at a time. Either
// way the commit that removes the fragment already rewrites changelog.d/,
// and it is where the comment's worked examples get refreshed for the next
// cycle. Naming no paths at all was considered and rejected — the examples
// are what make the pass concrete — so a comment that names none fails too,
// with one exception decided in #144: after the release's deletion there is
// nothing left to name and both halves could not hold at once. The floor
// therefore stands only while changelog.d/ holds a fragment. The release
// commit rewrites the examples to none and lands green on the emptied
// directory; the first fragment of the next cycle re-arms the floor, and
// that PR restores a worked example or `make ci` is red — the examples come
// back by CI, not by a note. Same drift class and same answer as
// cmd/voya/readme_test.go (#119): a check that fires exactly when the prose
// stops being true, and whoever cuts the release reads a rewrite, not a bug.
//
// The paths come from the comment itself rather than a list kept here, so a
// new example is pinned the moment it is written; the floor is what keeps
// that from passing vacuously on an emptied comment, for as long as
// changelog.d/ holds a fragment. In the window between a release and the next
// fragment it does not, and deleting the worked examples outright is green
// there — the cost #144 weighed and accepted, bounded by that first fragment
// re-arming the floor. No worked example
// is repeated in this file for the same reason: a second copy of a fragment
// path would go stale in the very release this test fires on, with nothing
// pinning it — the same drift, one file over.
func TestPackageCommentNamesFragmentsThatExist(t *testing.T) {
	for _, problem := range commentProblems(fragmentPathsIn(packageDoc(t, "main.go")), fragmentsOnDisk(t)) {
		t.Error(problem)
	}
}

// commentProblems is the pin as a pure function of what the comment names
// and what changelog.d/ holds, so the four states can be tabled below
// without emptying the real directory: the floor applies only while a
// fragment exists (#144), and every named path must exist regardless.
func commentProblems(named []string, onDisk map[string]bool) []string {
	var problems []string
	if len(named) == 0 && len(onDisk) > 0 {
		problems = append(problems, "main.go's package comment names no changelog.d/ fragment while the directory holds some; "+
			"the curation pass is explained on worked examples, and #136 decided it keeps them. "+
			"If this is the first fragment after a release, restore a worked example around it (#144).")
	}
	for _, name := range named {
		if !onDisk[name] {
			problems = append(problems, fmt.Sprintf("main.go's package comment names %s/%s, which is not a fragment on disk.\n"+
				"If a release just consumed it, rewrite the comment's worked examples around fragments "+
				"still on disk for the next cycle — or around none, if none remain — the failure at the release is the point (#136, #144).",
				changelog.Dir, name))
		}
	}
	return problems
}

// TestCommentProblems pins the floor's condition. The empty-directory rows
// are the state `make changelog VERSION=…` leaves behind, which the real
// tree only shows for one commit per release.
func TestCommentProblems(t *testing.T) {
	some := map[string]bool{"added/a.md": true}
	none := map[string]bool{}
	for _, tc := range []struct {
		name   string
		named  []string
		onDisk map[string]bool
		want   int
	}{
		{"names a fragment that exists", []string{"added/a.md"}, some, 0},
		{"names none, directory empty: the release commit", nil, none, 0},
		{"names none, directory holds one: the floor re-arms", nil, some, 1},
		{"names a fragment that is gone", []string{"added/b.md"}, some, 1},
		{"names a fragment, directory empty: no half is waived", []string{"added/a.md"}, none, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := commentProblems(tc.named, tc.onDisk); len(got) != tc.want {
				t.Errorf("commentProblems(%q, %v) = %q, want %d problem(s)", tc.named, tc.onDisk, got, tc.want)
			}
		})
	}
}

// fragmentExtractionCases drive [fragmentPathsIn] on prose written here,
// because the real comment exercises one shape of each rule.
var fragmentExtractionCases = []struct {
	name string
	doc  string
	want []string
}{
	{"one per category", "added/a.md then fixed/b-c.md, changed/d.md; removed/e.md deprecated/f.md security/g.md.",
		[]string{"added/a.md", "changed/d.md", "deprecated/f.md", "fixed/b-c.md", "removed/e.md", "security/g.md"}},
	{"a bare category is not a path", "a fixed/ fragment, and changelog.d/ itself", nil},
	{"an unknown directory is not a fragment", "see docs/adr/004.md and hack/x.md", nil},
	{"named twice, counted once", "added/a.md folds into added/a.md", []string{"added/a.md"}},
	{"a changelog.d/ prefix is not part of the path", "changelog.d/added/a.md", []string{"added/a.md"}},
	{"a re-wrapped path is one path", "it folds into added/a-long-\nname.md, where", []string{"added/a-long-name.md"}},
	{"re-wrapped at the slash, still one path", "it folds into added/\na-long-name.md, where", []string{"added/a-long-name.md"}},
	{"a bare category at the end of a line is still not a path", "a fixed/\nfragment, and a removed/\none", nil},
	{"a name outside the hyphen convention is still a path", "added/A_name.md", []string{"added/A_name.md"}},
}

func TestFragmentPathExtraction(t *testing.T) {
	for _, tc := range fragmentExtractionCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fragmentPathsIn(tc.doc); !slices.Equal(got, tc.want) {
				t.Errorf("fragmentPathsIn(%q) = %q, want %q", tc.doc, got, tc.want)
			}
		})
	}
}

// TestFragmentPathExtractionCoversEveryCategory keeps the case above named
// "one per category" honest. A category added to [changelog.Categories] goes
// into that case's prose by hand, never generated from Categories: prose
// built out of the list the pattern is built from would match whatever the
// pattern became and prove nothing.
func TestFragmentPathExtractionCoversEveryCategory(t *testing.T) {
	covered := map[string]bool{}
	for _, tc := range fragmentExtractionCases {
		for _, named := range tc.want {
			covered[strings.SplitN(named, "/", 2)[0]] = true
		}
	}
	for _, category := range changelog.Categories {
		if !covered[category] {
			t.Errorf("no extraction case names a %s/ path, so the extractor is untested for a category "+
				"changelog.Categories has: add one to the \"one per category\" case's prose", category)
		}
	}
}

// packageDoc returns the package comment of the named file in this directory.
func packageDoc(t *testing.T, file string) string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.PackageClauseOnly|parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}
	if f.Doc == nil {
		t.Fatalf("%s has no package comment", file)
	}
	return f.Doc.Text()
}

// fragmentsOnDisk is the set of category-relative fragment paths under
// changelog.d, read through the same [changelog.Collect] the release runs on.
//
// Membership is an exact string match and not an os.Stat, because a stat is
// case-insensitive on macOS: a path the comment spells added/Voya-Run.md
// would pass here and fail only on the Linux leg of CI.
func fragmentsOnDisk(t *testing.T) map[string]bool {
	t.Helper()
	fragments, err := changelog.Collect(os.DirFS(filepath.Join("..", "..", "..", changelog.Dir)))
	if err != nil {
		t.Fatalf("collecting fragments from %s: %v", changelog.Dir, err)
	}
	onDisk := make(map[string]bool, len(fragments))
	for _, fragment := range fragments {
		onDisk[fragment.Name] = true
	}
	return onDisk
}

// fragmentPathRE matches a fragment path as the comment writes one: a
// category directory from changelog.Categories, a slash, and a file name
// ending in .md.
//
// The name class is the shape [changelog.Validate] accepts rather than the
// lowercase-hyphen convention changelog.d/README.md asks for. Nothing
// enforces that convention, and a class narrowed to it would skip a name
// outside it — leaving the test green about a path it never looked at.
//
// The newlines are for a comment re-wrapped across a path, at the slash or
// inside the name: [go/ast.CommentGroup.Text] keeps the line breaks, so
// without them a wrapped added/voya-run.md drops out of the pinned set with
// nothing failing. cmd/voya/readme_test.go's statusNote joins its lines for
// the same reason. A break still needs a name after it, so a bare category at
// the end of a line stays what it was — prose, not a path.
var fragmentPathRE = regexp.MustCompile(
	`\b(?:` + strings.Join(changelog.Categories, "|") + `)/\n?[A-Za-z0-9._-]+(?:\n[A-Za-z0-9._-]+)*\.md\b`)

// fragmentPathsIn returns the fragment paths named in doc, sorted, each once.
func fragmentPathsIn(doc string) []string {
	var found []string
	for _, match := range fragmentPathRE.FindAllString(doc, -1) {
		found = append(found, strings.ReplaceAll(match, "\n", ""))
	}
	slices.Sort(found)
	return slices.Compact(found)
}
