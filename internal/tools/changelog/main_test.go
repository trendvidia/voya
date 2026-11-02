package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trendvidia/voya/internal/changelog"
)

// fragmentTree writes a minimal changelog.d under a temp root and returns the
// root.
func fragmentTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, "changelog.d", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	return root
}

// TestFlagsAfterTheSubcommand is the regression test for the shape of the
// command line. flag.Parse stops at the first non-flag argument, so a single
// FlagSet parsed before the subcommand drops every flag after it — which is
// exactly how the Makefile and a human invoke this.
func TestFlagsAfterTheSubcommand(t *testing.T) {
	root := fragmentTree(t, map[string]string{"added/thing.md": "- **A thing**.\n"})

	var out bytes.Buffer
	if err := run([]string{"release", "-root", root, "-version", "v0.1.0"}, &out); err != nil {
		t.Fatalf("release: %v", err)
	}

	written, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		t.Fatalf("reading CHANGELOG.md: %v", err)
	}
	if !strings.Contains(string(written), "## [v0.1.0]") {
		t.Errorf("the version flag after the subcommand was not applied:\n%s", written)
	}
	if !strings.Contains(string(written), "- **A thing**.") {
		t.Errorf("the fragment did not make it into the changelog:\n%s", written)
	}
}

// TestReleaseConsumesFragments: the content now lives in CHANGELOG.md, and a
// second copy would drift.
func TestReleaseConsumesFragments(t *testing.T) {
	root := fragmentTree(t, map[string]string{
		"added/thing.md": "- **A thing**.\n",
		"fixed/bug.md":   "- **A bug**.\n",
	})

	var out bytes.Buffer
	if err := run([]string{"release", "-root", root, "-version", "v0.1.0"}, &out); err != nil {
		t.Fatalf("release: %v", err)
	}
	for _, name := range []string{"added/thing.md", "fixed/bug.md"} {
		if _, err := os.Stat(filepath.Join(root, "changelog.d", name)); !os.IsNotExist(err) {
			t.Errorf("%s survived the release", name)
		}
	}

	// And releasing again has nothing to say, rather than writing an empty
	// section.
	if err := run([]string{"release", "-root", root, "-version", "v0.2.0"}, &out); err == nil {
		t.Error("a second release with no fragments succeeded")
	}
}

func TestReleaseAppendsAboveThePrevious(t *testing.T) {
	root := fragmentTree(t, map[string]string{"added/first.md": "- **First**.\n"})
	var out bytes.Buffer
	if err := run([]string{"release", "-root", root, "-version", "v0.1.0"}, &out); err != nil {
		t.Fatalf("first release: %v", err)
	}

	if err := os.WriteFile(filepath.Join(root, "changelog.d", "added", "second.md"),
		[]byte("- **Second**.\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := run([]string{"release", "-root", root, "-version", "v0.2.0"}, &out); err != nil {
		t.Fatalf("second release: %v", err)
	}

	written, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		t.Fatalf("reading CHANGELOG.md: %v", err)
	}
	newer := strings.Index(string(written), "## [v0.2.0]")
	older := strings.Index(string(written), "## [v0.1.0]")
	if newer < 0 || older < 0 {
		t.Fatalf("a release section is missing:\n%s", written)
	}
	if newer > older {
		t.Errorf("v0.2.0 landed below v0.1.0:\n%s", written)
	}
	if !strings.Contains(string(written), "- **First**.") {
		t.Errorf("the first release's entries were lost:\n%s", written)
	}
}

func TestCheckReportsProblems(t *testing.T) {
	root := fragmentTree(t, map[string]string{"improved/a.md": "- x\n"})

	var out bytes.Buffer
	err := run([]string{"check", "-root", root}, &out)
	if err == nil {
		t.Fatal("check passed on an unknown category")
	}
	if !strings.Contains(out.String(), "is not a category") {
		t.Errorf("the problem was not reported to the user:\n%s", out.String())
	}
}

// TestCheckRequiresTheDirectory: check is the CI gate, and changelog.d/ is
// committed, so its absence is an accident that must turn the gate red — the
// window is otherwise one release cycle wide, while no fragment exists for the
// doc pin to name. The tolerance is Validate's, and release and preview keep
// it; and the real post-release state, every category directory present and
// empty, still passes (#147).
func TestCheckRequiresTheDirectory(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		root := t.TempDir()
		var out bytes.Buffer
		err := run([]string{"check", "-root", root}, &out)
		if err == nil {
			t.Fatal("check passed with no changelog.d/ at all")
		}
		if !strings.Contains(err.Error(), "changelog.d/ is missing") {
			t.Errorf("the error does not name the missing directory: %v", err)
		}
	})

	t.Run("all categories empty", func(t *testing.T) {
		files := map[string]string{"README.md": "# fragments\n"}
		for _, category := range []string{"added", "changed", "deprecated", "removed", "fixed", "security"} {
			files[category+"/.gitkeep"] = ""
		}
		root := fragmentTree(t, files)
		var out bytes.Buffer
		if err := run([]string{"check", "-root", root}, &out); err != nil {
			t.Fatalf("check failed on the post-release state: %v\n%s", err, out.String())
		}
	})

	t.Run("preview keeps the tolerance", func(t *testing.T) {
		root := t.TempDir()
		var out bytes.Buffer
		if err := run([]string{"preview", "-root", root}, &out); err != nil {
			t.Fatalf("preview failed with no changelog.d/: %v", err)
		}
		if !strings.Contains(out.String(), "No unreleased fragments.") {
			t.Errorf("preview did not report an empty tree:\n%s", out.String())
		}
	})
}

// TestEveryCategoryKeepsAGitkeep pins the premise the gate above argues from:
// check's error message and changelog.d/README.md both say the directory is
// committed with a .gitkeep per category, which is what makes "every category
// present and empty" the state git keeps across a release rather than a state
// only this checkout has.
//
// Nothing else can see it. Validate and Collect skip .gitkeep, `make changelog
// VERSION=…` deletes fragments and never the keepers, and the stat check in
// check stops at changelog.d/ itself — verified: check exits 0 on a
// changelog.d/ holding only README.md, every category directory gone. So a
// category added to changelog.Categories without a .gitkeep, or one deleted,
// silently restores the bug this PR's changelog.d/added/.gitkeep fixed: the
// release consumes that category's last fragment and git drops the directory
// (#147). This is the real tree, because that is the tree the claim is about.
func TestEveryCategoryKeepsAGitkeep(t *testing.T) {
	if len(changelog.Categories) == 0 {
		t.Fatal("changelog.Categories is empty, so the loop below would pin nothing")
	}
	for _, category := range changelog.Categories {
		keep := filepath.Join("..", "..", "..", changelog.Dir, category, ".gitkeep")
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("%s/%s/ has no .gitkeep, so the release that consumes its last fragment drops it from git: %v",
				changelog.Dir, category, err)
		}
	}
}

func TestReleaseRefusesInvalidFragments(t *testing.T) {
	root := fragmentTree(t, map[string]string{
		"added/good.md": "- **Good**.\n",
		"improved/a.md": "- x\n",
	})

	var out bytes.Buffer
	if err := run([]string{"release", "-root", root, "-version", "v0.1.0"}, &out); err == nil {
		t.Fatal("release ran with a misfiled fragment, which would have silently dropped it")
	}
	if _, err := os.Stat(filepath.Join(root, "CHANGELOG.md")); !os.IsNotExist(err) {
		t.Error("a refused release still wrote CHANGELOG.md")
	}
}

func TestUsageErrors(t *testing.T) {
	cases := [][]string{
		{},
		{"nonsense"},
		{"release"},           // no -version
		{"check", "leftover"}, // stray positional
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var out bytes.Buffer
			if err := run(args, &out); err == nil {
				t.Errorf("run(%q) returned no error", args)
			}
		})
	}
}
