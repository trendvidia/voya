# CLAUDE.md

How work gets done in this repo, human or headless. **What voya is and how it
is configured lives in [`README.md`](README.md); why it is built that way lives
in [`docs/adr/`](docs/adr/).** This file does not restate either — it carries
the working contract and links to the rest.

That split is deliberate (#77). An invariant written down twice drifts, and it
already did: the PR that added this file needed a second commit to re-sync
README after a review corrected one copy, and a third correction landed in both
places after that. One home per fact, and this is not the home for most of them.

## Where the facts are

Read the linked section before changing behaviour it covers.

| you need | it is in |
|---|---|
| the three listeners, their ports, and loopback-only | [README — How it works](README.md#how-it-works) |
| configuration, `log_level`, and that everything voya says goes to stderr | [README — Configuring voya](README.md#configuring-voya) |
| the serialization rule, and which files sit at a boundary | [README — Conventions](README.md#conventions) |
| committed proto bindings, `proto-check`, `proto-breaking`'s `FILE` rule set | [README — Conventions](README.md#conventions) |
| the seven modules, `go.work`, and why no `replace` directives | [README — Development](README.md#development) |
| why any of it is the way it is, and what was rejected | [`docs/adr/`](docs/adr/), indexed in [README — Design](README.md#design) |

Two things a working session needs that README does not say, because they are
about editing the code rather than using it:

- **`(pxf.default)` is not available on the schema at all**, and not merely
  discouraged as a second copy of a default: voya links chameleon, whose
  bindings already register `pxf/annotations.proto`, and a copy generated here
  would register that file path and the same extensions twice, which panics
  protoregistry at init. `proto/buf.yaml` makes the argument. So defaults live
  only in `internal/config/layers/base.pxf`, and a `voya.config.v1` default
  written again in Go is a bug — with one documented exception, `client`, which
  loads no config at all and so restates 9777/9779 as the fleet convention ADR
  001 fixes. `client/voya.go` makes that argument; do not "fix" it.
- **The ADRs carry dated amendments**, and the amendment is the current
  decision. ADR 002 on the reclaim contract, ADR 003 on the REST surface its
  own decision deferred, ADR 004 on the shutdown grace and the upstream idle
  timeout, ADR 005 on stderr and on the status-to-code table.

## Commands

```console
$ make ci      # the gate: modules-check ci-listing-check workflow-check
               # recipe-check
               # build compile-check tidy-check sync-check require-check
               # install-check client-check acceptance-check changelog-check
               # proto-lint proto-breaking proto-check vet test lint cross
$ make check   # fast pre-commit: modules-check fmt-check vet test
$ make test    # all seven modules, race detector
$ make help    # everything else
```

Three traps worth knowing before you debug one:

- **`make ci` is not offline.** Since #240 `require-check` lists origin's tags
  live, so `make ci` needs origin reachable — and prompt-free: it runs the
  listing with terminal and ssh prompting off, and a missing credential
  refuses with a named cause rather than hanging. `client-check` and
  `acceptance-check` are the second, and `client-check` twice over: it derives
  *which* modules it checks from that same live listing (a module tagged on
  origin that also requires a sibling, replace or not — #267, widened in
  #275), and both resolve a sibling from its *tag* — everything else reads
  `../proto` and friends through a replace or the workspace — so on a cold
  module cache they fetch, and their failure messages tell a fetch apart from
  a stale tag. Everything else in the chain still runs against a warm module
  cache alone. Settled in #249, extended knowingly in #267, #270 and #275: the
  cost is accepted and there is no opt-out, deliberately — do not re-file it.
- **`GOWORK` changes what resolves.** `make tidy-check` runs each module
  outside the workspace deliberately — that is the check, not a bug in it.
- **`golangci-lint`'s cache is shared between worktrees and replays results
  from deleted ones.** `make lint` failing on paths like `../pr-64/...` is the
  cache, not a regression: re-run with
  `GOLANGCI_LINT_CACHE=$(mktemp -d) make lint` to confirm.

## Definition of done

`make ci` green, and:

- **Nothing is done by assertion.** Say what you ran and what it showed.
  Failures verbatim, never summarized into softer words.
- **A behaviour claim in a comment needs a test behind it.** If you cannot
  write the test, write a weaker comment.
- **Test-only changes are proved by mutation.** Break the thing the test exists
  to catch and show it failing. A control whose input is derived from the
  pattern it searches for — or whose transform is a no-op under the function it
  feeds — is unconditionally true and proves nothing. This repo has shipped
  both; the reviews caught them, not the author.
- **Assert, do not log.** No `t.Errorf` from a goroutine nothing synchronises
  — hand the observation back over a channel or a `WaitGroup` and fail on the
  test goroutine, the way `internal/daemon/daemon_test.go` does.
- **Latency assertions gate on the median.** A wait in the code narrows the
  spread and a busy machine widens it, so only the median is about the code.
  Check `uptime` before publishing a number.
- **Wait for the thing, not for a proxy for it.** A test that spawns `voya run`
  waits for the startup banner, never for a dial on the proxy port: `Start`
  binds the proxy first, so that port answers while the registration and DNS
  listeners are still unbound. The banner reaches stderr because `cmd/voya`
  points `Start`'s `logf` there — in process the sink is whatever the test
  passed, and `nil` means say nothing.

## Merge policy

- **Merge commits, not squash.** `main` is merge commits — 35 of 119 at the
  time of writing. Squashing a stack parent replaces its commits with a new
  one, and every child then re-shows the parent's diff and conflicts on
  retarget.
- **A stack needs one of two merge orders, never neither.** Merging the bottom
  first strands the children: they merge into their base *branch* rather than
  into `main` while GitHub still reports them MERGED. Auto-delete-branch does
  not save you — GitHub will not delete a branch another open PR targets, so no
  retarget fires and the next merge lands in the stale branch. Either retarget
  as you go (`gh pr edit N --base main` *before* merging N — the order
  `ci.yml`'s `edited` trigger exists for), or merge top-down so the stack
  collapses into the bottom branch and only that one PR reaches `main`. Then
  check `git ls-tree main` for the files you expect; `git cherry main <branch>`
  is what finds stranded work, because cherry-picks change SHAs and
  `git merge-base --is-ancestor` gives false negatives.
- **Merging is the owner's call.** A session never merges, by hand or
  otherwise, and nothing merges automatically. Anything that tags, releases,
  deletes, or reaches outside this repo is the owner's too.
- **`changelog.d/`, one fragment per change worth a release note**, in the
  category directory it belongs to. **Not every PR needs one** — a refactor
  nobody outside the repo can observe, a CI fix, a test have nothing to tell a
  user. Nothing enforces it because nothing can tell the difference; that
  judgement is part of review. `make changelog-check` only catches what would
  silently vanish at release time.

## Filing

- **Every gap ends the turn fixed, filed, or pinned.** Noticed-but-unrecorded
  is the only forbidden state. Deliberately-unfixed edges get their reasoning
  written where the next person looks.
- **Prefer mechanism-level issues with edge checklists** over one issue per
  symptom. An issue defines the problem, not the perimeter of the diff.
- **Groups are labels**, `grp/<slug>`. M0–M5 are phase milestones, so
  milestones are not free for grouping.
- **An open question in an issue is the owner's to settle.** Do not close it by
  picking the sensible option and reporting it as decided — surface it with a
  recommendation and the tradeoff, and wait. Ordinary implementation judgement
  stays yours; the marker is that someone wrote the question down as open.
- **Never let a closing keyword sit in front of an issue number you are not
  closing.** "Filed rather than fixed: #65" closed #65 — GitHub's parser
  matched `fixed: #65` inside the sentence saying the work had *not* been done.
  "Filed, not fixed — #65" survives only because an em dash is not one of the
  separators GitHub accepts between the keyword and the `#`; a hyphen or a
  colon typed in its place re-arms the bug. Safest is to keep the number off
  the keyword's right-hand side entirely: "#65 — filed, not fixed".
