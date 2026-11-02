// Command changelog checks, previews and assembles voya's release notes from
// the fragments under changelog.d/.
//
// Reached through the Makefile — `make changelog-check`, `make
// changelog-preview`, `make changelog VERSION=v0.1.0` — rather than run
// directly. It is a Go program and not a shell recipe because assembling text
// files portably in shell means `sed -i`, which takes different arguments on
// macOS and Linux, and because the assembly is worth testing.
//
// Cutting a release starts with a curation pass the tool does not do, and one
// criterion decides what the pass covers: any fragment whose baseline no prior
// release printed. A fixed/ fragment whose subject never shipped — a fix to
// behaviour another fragment in the same unreleased cycle introduced — and a
// changed/ fragment whose "used to" names a state no release ever had both
// describe correcting something the release's readers never ran, and rendered
// as they stand they give the release a Fixed or Changed list about behaviour
// no release ever shipped. Every release carries the exposure, not the first
// alone: changelog.Collect gathers whatever is in changelog.d/ at the time, so
// a first release is only the case where every fragment that has a baseline
// qualifies, and any later cycle that adds a behaviour and then amends it
// before shipping recreates the case for that pair.
//
// The pass runs before `make changelog VERSION=…`, which deletes what it
// consumes. A qualifying fragment folds into the fragment that introduced the
// behaviour — usually the added/ entry for the feature, and sometimes another
// fixed/ entry, when what never shipped was itself a fix made earlier in the
// same cycle. When no fragment introduced it, it is rewritten as an absolute
// statement of what the release does and moved into added/: the directory is
// the category, so a fragment left in changed/ renders under Changed however
// it is worded. The tool renders what it finds, because it cannot reliably
// tell such a fragment from one about released behaviour; that judgement
// stays with whoever cuts the release (#112, #124).
//
// v0.2.0 was the first release and it ran that pass over everything: nothing
// had shipped, so every fixed/ and changed/ fragment on disk qualified — 49 of
// them, folded into 35 added/ entries, leaving a release with an Added section
// and nothing else. Two folds #124 had worked out in advance came out as
// written (voya-speaks-on-stderr into voya-run, measured-shutdown-grace into
// client-register-and-dialer). The pass also caught what this rule does not
// name: added/ fragments whose own tails had gone stale, "DNS-only for now"
// and "two things the install cannot check for you" among them, each made
// false by a later fragment in the same unreleased cycle.
//
// v0.3.0 ran the same pass over 14 fragments and folded one: a fixed/ entry
// about the example services, which that release introduced, went into the
// added/ entry for them. The other 13 stood, being new behaviour or corrections
// to what v0.2.0 shipped, which is the common case. Those fragments went with
// the release, so this comment names none until the next cycle's first fragment
// brings a worked example back (#144).
//
// This is also the mechanism working as #144 designed it. v0.2.0's release
// commit emptied changelog.d/ and rewrote this paragraph to name no fragment
// at all, which doc_test.go permits only while the directory is empty: the
// floor is off in that window and the release lands green. The first fragment
// of the next cycle re-arms it, and the PR adding that fragment fails
// TestPackageCommentNamesFragmentsThatExist until it restores an example —
// which is how the examples came back after v0.2.0 (#136, #144).
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/trendvidia/voya/internal/changelog"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "changelog: %v\n", err)
		os.Exit(1)
	}
}

const usage = "usage: changelog <check|preview|release> [flags]"

// run takes the subcommand first and its flags after it.
//
// That order is not cosmetic: flag.Parse stops at the first non-flag
// argument, so a single FlagSet parsed before the subcommand silently ignores
// everything after it — `changelog -root x release -version v0.1.0` would
// drop the version and report a usage error about argument count. The same
// trap is documented in cmd/voya. Parsing per subcommand is what makes
// `changelog release -version v0.1.0` work, which is how the Makefile and a
// human both write it.
func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", usage)
	}
	command, rest := args[0], args[1:]

	flags := flag.NewFlagSet("changelog "+command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	root := flags.String("root", ".", "repository root")
	version := flags.String("version", "", "version to release, e.g. v0.1.0")
	file := flags.String("file", "CHANGELOG.md", "changelog file, relative to -root")
	if err := flags.Parse(rest); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q\n%s", flags.Arg(0), usage)
	}

	dir := filepath.Join(*root, changelog.Dir)

	switch command {
	case "check":
		return check(dir, stdout)
	case "preview":
		return preview(dir, stdout)
	case "release":
		if *version == "" {
			return fmt.Errorf("release needs -version, e.g. changelog release -version v0.1.0")
		}
		return release(dir, filepath.Join(*root, *file), *version, stdout)
	default:
		return fmt.Errorf("unknown command %q\n%s", command, usage)
	}
}

// check is the CI gate, and it is the one caller that requires changelog.d/
// to exist. changelog.Validate tolerates an absent directory on purpose — a
// tree that never filed a fragment has nothing to validate, and release and
// preview keep that tolerance — but in this repo the directory is committed,
// a .gitkeep per category, so under CI its absence is only ever an accident.
// Without this the accident is invisible for exactly one release cycle: a
// release consumes every fragment, the doc pin in doc_test.go then names no
// path to check, and Validate passes on nothing. A tree whose category
// directories are all empty is the real post-release state and stays green
// (#147).
func check(dir string, stdout io.Writer) error {
	//nolint:gosec // G703: the taint is the -root flag, operator input on a dev machine by design.
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s/ is missing: it is committed, with a .gitkeep per category, so this is an accident, not an empty release cycle", changelog.Dir)
		}
		return fmt.Errorf("reading %s: %w", changelog.Dir, err)
	}
	problems := changelog.Validate(os.DirFS(dir))
	for _, problem := range problems {
		fmt.Fprintln(stdout, problem)
	}
	if len(problems) > 0 {
		return fmt.Errorf("%d fragment problem(s); see %s/README.md", len(problems), changelog.Dir)
	}
	return nil
}

func preview(dir string, stdout io.Writer) error {
	fragments, err := changelog.Collect(os.DirFS(dir))
	if err != nil {
		return err
	}
	if len(fragments) == 0 {
		fmt.Fprintln(stdout, "No unreleased fragments.")
		return nil
	}
	fmt.Fprint(stdout, changelog.Render("Unreleased", time.Now(), fragments))
	return nil
}

func release(dir, path, version string, stdout io.Writer) error {
	if problems := changelog.Validate(os.DirFS(dir)); len(problems) > 0 {
		for _, problem := range problems {
			fmt.Fprintln(stdout, problem)
		}
		return fmt.Errorf("refusing to release with %d fragment problem(s)", len(problems))
	}

	fragments, err := changelog.Collect(os.DirFS(dir))
	if err != nil {
		return err
	}
	if len(fragments) == 0 {
		return fmt.Errorf("no fragments in %s: nothing to release", changelog.Dir)
	}

	existing, err := os.ReadFile(path) //nolint:gosec // path is assembled from documented flags
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading %s: %w", path, err)
	}

	section := changelog.Render(version, time.Now(), fragments)
	if err := os.WriteFile(path, []byte(changelog.Insert(string(existing), section)), 0o644); err != nil { //nolint:gosec // a changelog is world-readable by design
		return fmt.Errorf("writing %s: %w", path, err)
	}

	// The fragments' content now lives in the changelog; keeping them would
	// be a second copy to drift. Git has the originals if a release ever
	// needs rebuilding.
	for _, fragment := range fragments {
		// Rebuilt from a known category and a bare file name rather than
		// joining the collected path: these come from a directory listing
		// and cannot escape the tree today, but this is a delete loop, and
		// it should not depend on that staying true.
		leaf := filepath.Base(fragment.Name)
		if leaf == "." || leaf == string(filepath.Separator) {
			return fmt.Errorf("refusing to remove %q", fragment.Name)
		}
		//nolint:gosec // G703: the traversal taint is the -root flag, which is
		// operator input on a dev machine by design; the leaf is Base()-sanitised
		// and the category comes from this package's own list.
		if err := os.Remove(filepath.Join(dir, fragment.Category, leaf)); err != nil {
			return fmt.Errorf("removing %s: %w", fragment.Name, err)
		}
	}

	fmt.Fprintf(stdout, "%s: %d entries\n", version, len(fragments))
	fmt.Fprintf(stdout, "Review %s, then commit it with the fragments it consumed.\n", path)
	return nil
}
