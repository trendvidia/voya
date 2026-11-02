package changelog

import (
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

var releaseDate = time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)

func TestCollectOrdersByCategoryThenName(t *testing.T) {
	fsys := fstest.MapFS{
		"fixed/zebra.md":   {Data: []byte("- fixed zebra\n")},
		"fixed/alpha.md":   {Data: []byte("- fixed alpha\n")},
		"added/beta.md":    {Data: []byte("- added beta\n")},
		"security/one.md":  {Data: []byte("- security one\n")},
		"added/notes.txt":  {Data: []byte("ignored: not markdown\n")},
		"changed/.gitkeep": {Data: []byte("")},
	}

	got, err := Collect(fsys)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := []string{"added/beta.md", "fixed/alpha.md", "fixed/zebra.md", "security/one.md"}
	if len(got) != len(want) {
		t.Fatalf("Collect returned %d fragments, want %d: %+v", len(got), len(want), got)
	}
	for i, fragment := range got {
		if fragment.Name != want[i] {
			t.Errorf("fragment %d = %q, want %q — ordering must be deterministic", i, fragment.Name, want[i])
		}
	}
}

func TestCollectOnAnEmptyTree(t *testing.T) {
	got, err := Collect(fstest.MapFS{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Collect on an empty tree returned %d fragments", len(got))
	}
}

func TestRender(t *testing.T) {
	fragments := []Fragment{
		{Category: "added", Name: "added/a.md", Body: "- **A thing** happened.\n\n  With a second paragraph."},
		{Category: "added", Name: "added/b.md", Body: "- **Another thing**."},
		{Category: "fixed", Name: "fixed/c.md", Body: "- **A fix** ([#12](https://example.test/12))."},
	}

	got := Render("v0.1.0", releaseDate, fragments)
	want := `## [v0.1.0] - 2026-08-10

### Added

- **A thing** happened.

  With a second paragraph.

- **Another thing**.

### Fixed

- **A fix** ([#12](https://example.test/12)).
`
	if got != want {
		t.Errorf("Render mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRenderWithNothingToRelease(t *testing.T) {
	if got := Render("v0.1.0", releaseDate, nil); got != "" {
		t.Errorf("Render with no fragments = %q, want empty so no heading is written", got)
	}
}

func TestInsertIntoAnEmptyChangelog(t *testing.T) {
	got := Insert("", "## [v0.1.0] - 2026-08-10\n\n### Added\n\n- first\n")
	if !strings.HasPrefix(got, "# Changelog") {
		t.Errorf("a new changelog did not get the preamble:\n%s", got)
	}
	if !strings.Contains(got, "## [v0.1.0]") {
		t.Errorf("the section is missing:\n%s", got)
	}
}

// TestInsertPutsTheNewestFirst: releases read newest-first, so a new section
// goes above the previous one and below the preamble.
func TestInsertPutsTheNewestFirst(t *testing.T) {
	existing := Preamble + "\n## [v0.1.0] - 2026-08-01\n\n### Added\n\n- older\n"
	got := Insert(existing, "## [v0.2.0] - 2026-08-10\n\n### Added\n\n- newer\n")

	newer := strings.Index(got, "## [v0.2.0]")
	older := strings.Index(got, "## [v0.1.0]")
	preamble := strings.Index(got, "# Changelog")
	switch {
	case newer < 0 || older < 0:
		t.Fatalf("a section went missing:\n%s", got)
	case newer > older:
		t.Errorf("the new section landed below the old one:\n%s", got)
	case preamble > newer:
		t.Errorf("the new section landed above the preamble:\n%s", got)
	}
	if !strings.Contains(got, "- older") {
		t.Error("the previous release's entries were dropped")
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		fsys    fstest.MapFS
		mention string
	}{
		{
			name:    "unknown category",
			fsys:    fstest.MapFS{"improved/a.md": {Data: []byte("- x\n")}},
			mention: "is not a category",
		},
		{
			name:    "loose file",
			fsys:    fstest.MapFS{"a.md": {Data: []byte("- x\n")}},
			mention: "not loose",
		},
		{
			name:    "not markdown",
			fsys:    fstest.MapFS{"added/a.txt": {Data: []byte("- x\n")}},
			mention: "must be a .md file",
		},
		{
			name:    "empty fragment",
			fsys:    fstest.MapFS{"added/a.md": {Data: []byte("   \n")}},
			mention: "is empty",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			problems := Validate(tc.fsys)
			if len(problems) == 0 {
				t.Fatal("Validate accepted it")
			}
			joined := errText(problems)
			if !strings.Contains(joined, tc.mention) {
				t.Errorf("no problem mentions %q:\n%s", tc.mention, joined)
			}
		})
	}

	t.Run("valid tree", func(t *testing.T) {
		fsys := fstest.MapFS{
			"added/a.md":       {Data: []byte("- x\n")},
			"fixed/b.md":       {Data: []byte("- y\n")},
			"changed/.gitkeep": {Data: []byte("")},
			"README.md":        {Data: []byte("docs\n")},
		}
		if problems := Validate(fsys); len(problems) != 0 {
			t.Errorf("Validate rejected a valid tree:\n%s", errText(problems))
		}
	})
}

// TestValidateReportsEveryProblem: fixing three misplaced files should take
// one run, not three.
func TestValidateReportsEveryProblem(t *testing.T) {
	fsys := fstest.MapFS{
		"improved/a.md": {Data: []byte("- x\n")},
		"added/b.txt":   {Data: []byte("- y\n")},
		"added/c.md":    {Data: []byte("\n")},
	}
	if problems := Validate(fsys); len(problems) != 3 {
		t.Errorf("Validate reported %d problems, want 3:\n%s", len(problems), errText(problems))
	}
}

func errText(errs []error) string {
	lines := make([]string, 0, len(errs))
	for _, err := range errs {
		lines = append(lines, err.Error())
	}
	return strings.Join(lines, "\n")
}
