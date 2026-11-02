// Package changelog assembles release notes from one-file-per-change
// fragments.
//
// The point is that a change and its release note land in the same commit,
// reviewed together. A single CHANGELOG.md cannot do that here: voya's work
// lands in stacked PRs, and a shared file at the top of a stack conflicts
// with every child on the same lines — which teaches everyone to write the
// note afterwards, from the git log, which is where release notes go to die.
//
// Fragments live in changelog.d/<category>/<slug>.md and hold plain
// markdown. [Render] turns them into a release section and [Insert] puts that
// section into CHANGELOG.md; the fragments are then deleted, since their
// content now lives there.
package changelog

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

// Categories are Keep a Changelog's, in the order a release section lists
// them. A fragment's category is the directory it sits in — no front matter,
// so a fragment is readable markdown and nothing else.
var Categories = []string{"added", "changed", "deprecated", "removed", "fixed", "security"}

// Dir is where unreleased fragments live, relative to the repo root.
//
// No "unreleased" level beneath it: fragments are deleted once their content
// is in CHANGELOG.md, so everything in here is unreleased by definition and
// the extra directory would only ever hold nothing.
const Dir = "changelog.d"

// Fragment is one change's note.
type Fragment struct {
	Category string
	Name     string // "<category>/<file>.md", for diagnostics and ordering
	Body     string
}

// Collect reads every fragment in fsys, which is rooted at [Dir]. The result
// is ordered by category and then by file name, so two runs over the same
// tree produce identical output.
func Collect(fsys fs.FS) ([]Fragment, error) {
	var out []Fragment
	for _, category := range Categories {
		entries, err := fs.ReadDir(fsys, category)
		if err != nil {
			if isNotExist(err) {
				continue // a category nobody has used yet
			}
			return nil, fmt.Errorf("changelog: reading %s: %w", category, err)
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			names = append(names, entry.Name())
		}
		sort.Strings(names)
		for _, name := range names {
			body, err := fs.ReadFile(fsys, path.Join(category, name))
			if err != nil {
				return nil, fmt.Errorf("changelog: reading %s/%s: %w", category, name, err)
			}
			out = append(out, Fragment{
				Category: category,
				Name:     path.Join(category, name),
				Body:     strings.TrimRight(string(body), "\n"),
			})
		}
	}
	return out, nil
}

// Validate reports every problem with the fragment tree, rather than the
// first: someone fixing three misplaced files should learn about all three in
// one run.
//
// What it checks is what silently disappears at release time — a fragment
// filed under a category that is not a category, a file that is not markdown,
// or an empty one. Whether a given change *has* a fragment is a review
// question, not something a linter can know.
func Validate(fsys fs.FS) []error {
	var problems []error

	known := make(map[string]bool, len(Categories))
	for _, category := range Categories {
		known[category] = true
	}

	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		if isNotExist(err) {
			return nil // nothing to validate
		}
		return []error{fmt.Errorf("changelog: reading %s: %w", Dir, err)}
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			if entry.Name() == ".gitkeep" || entry.Name() == "README.md" {
				continue
			}
			problems = append(problems, fmt.Errorf(
				"%s/%s: fragments go in a category directory (%s), not loose",
				Dir, entry.Name(), strings.Join(Categories, ", ")))
			continue
		}
		if !known[entry.Name()] {
			problems = append(problems, fmt.Errorf(
				"%s/%s/: %q is not a category; use one of %s",
				Dir, entry.Name(), entry.Name(), strings.Join(Categories, ", ")))
		}
	}

	for _, category := range Categories {
		files, err := fs.ReadDir(fsys, category)
		if err != nil {
			continue // absent categories are fine; the loop above catches junk
		}
		for _, file := range files {
			name := path.Join(category, file.Name())
			if file.IsDir() {
				problems = append(problems, fmt.Errorf("%s/%s/: categories hold files, not directories", Dir, name))
				continue
			}
			if file.Name() == ".gitkeep" {
				continue
			}
			if !strings.HasSuffix(file.Name(), ".md") {
				problems = append(problems, fmt.Errorf("%s/%s: a fragment must be a .md file", Dir, name))
				continue
			}
			body, err := fs.ReadFile(fsys, name)
			if err != nil {
				problems = append(problems, fmt.Errorf("%s/%s: %w", Dir, name, err))
				continue
			}
			if strings.TrimSpace(string(body)) == "" {
				problems = append(problems, fmt.Errorf("%s/%s: is empty, so the release would not mention it", Dir, name))
			}
		}
	}
	return problems
}

// Render turns fragments into one release section. Returns "" when there is
// nothing to release, which the caller should treat as "do not cut a release"
// rather than writing an empty heading.
func Render(version string, date time.Time, fragments []Fragment) string {
	if len(fragments) == 0 {
		return ""
	}
	// Built as blocks joined by one blank line, rather than appended with
	// separators as it goes: the append-as-you-go version put three newlines
	// before every category heading after the first.
	var (
		blocks  []string
		entries []string
		current string
	)
	flush := func() {
		if current != "" {
			blocks = append(blocks, "### "+title(current)+"\n\n"+strings.Join(entries, "\n\n"))
		}
	}
	for _, fragment := range fragments {
		if fragment.Category != current {
			flush()
			current, entries = fragment.Category, nil
		}
		entries = append(entries, fragment.Body)
	}
	flush()

	return fmt.Sprintf("## [%s] - %s\n\n%s\n",
		version, date.Format(time.DateOnly), strings.Join(blocks, "\n\n"))
}

// Insert places a rendered section into an existing changelog, immediately
// above the newest release section — or after the preamble when there are no
// releases yet. An empty changelog gets [Preamble] first.
func Insert(changelog, section string) string {
	if strings.TrimSpace(changelog) == "" {
		return Preamble + "\n" + section
	}
	lines := strings.Split(changelog, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "## ") {
			head := strings.Join(lines[:i], "\n")
			tail := strings.Join(lines[i:], "\n")
			return strings.TrimRight(head, "\n") + "\n\n" + section + "\n" + tail
		}
	}
	return strings.TrimRight(changelog, "\n") + "\n\n" + section
}

// Preamble heads a changelog that does not exist yet.
const Preamble = `# Changelog

All notable changes to ` + "`voya`" + ` are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Entries are assembled from one-file-per-change fragments under
[changelog.d/](changelog.d/); see that directory's README for how to add one.
`

func title(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }
