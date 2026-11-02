# changelog.d

One file per change. A PR that is worth mentioning in a release adds a file
here; `make changelog VERSION=…` collects them into `CHANGELOG.md` and deletes
them.

## Adding one

```console
$ cat > changelog.d/added/proxy-authority-routing.md <<'EOF'
- **The proxy routes on `:authority`** ([#17](https://github.com/trendvidia/voya/issues/17)).
  Requests to `payments.internal:9777` reach whatever port payments registered,
  and an unknown name gets the diagnostic rather than a bare 502.
EOF
```

That is the whole format: **plain markdown, in the category directory the
change belongs to.** No front matter and no required fields, because a
fragment is a paragraph of release notes and nothing else — it should be
readable in the PR diff without knowing this tool exists.

Categories are [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)'s, and
the directory *is* the category:

| directory | for |
|---|---|
| `added/` | new capability |
| `changed/` | different behaviour in something that already existed |
| `deprecated/` | still works, will not for long |
| `removed/` | gone |
| `fixed/` | it was broken |
| `security/` | anything a user should upgrade *because of* |

File names only decide the order entries appear within a category, so name
them after the change (`proxy-authority-routing.md`), not after the PR number.

## Why not one CHANGELOG.md

voya's work lands in **stacked PRs**, and a shared file at the top of a stack
conflicts with every child on the same lines. The reliable way to avoid that
is to write release notes afterwards from the git log — which produces notes
that describe commits instead of changes, written by whoever is cutting the
release rather than whoever made the change.

A fragment is reviewed alongside the diff it describes, by the person who
knows why it matters.

## Not every PR needs one

A refactor nobody outside the repo can observe, a CI fix, a test — these have
nothing to tell a user. Skip them. Nothing enforces a fragment per PR because
nothing can tell the difference; that judgement is part of review.

`make changelog-check` only catches what would silently vanish at release
time: a file loose in `changelog.d/`, a directory that is not a category, a
non-`.md` file, or an empty fragment — and a `changelog.d/` that is missing
altogether, because the directory is committed (a `.gitkeep` per category) and
under CI its absence is an accident, not an empty release cycle. Only the
`check` command insists on that; `release` and `preview` still treat no
directory as no fragments.

## Commands

```console
$ make changelog-check              # validate what is here
$ make changelog-preview            # render the next release section
$ make changelog VERSION=v0.1.0     # write it into CHANGELOG.md, delete fragments
```

That last command deletes the fragments it consumed, so the curation pass
every release needs happens before it, not after: the package comment of
[`internal/tools/changelog/main.go`](../internal/tools/changelog/main.go) is
where that pass is written down.
