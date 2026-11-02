# Contributing to voya

Thanks for your interest in contributing.

```console
$ git clone https://github.com/trendvidia/voya.git
$ cd voya
$ make check   # format, vet and tests across all seven modules
$ make ci      # the full gate; needs network access to the repository
```

Before opening a pull request:

- Keep it to one concern, with tests for any behaviour it changes.
- Read the ADR in [`docs/adr/`](docs/adr/) that covers the area. If your change
  contradicts one, say so in the PR; the ADR is amended rather than bypassed.
- Add a fragment under [`changelog.d/`](changelog.d/README.md) if users would
  notice the change.
- Commit messages explain why the change was needed.

[`CLAUDE.md`](CLAUDE.md) holds the working conventions in full. Its issue and
PR references, and those in code comments and the changelog, predate this
repository and point at the project's previous, now archived, tracker.

Report security problems as [`SECURITY.md`](SECURITY.md) describes, not in a
public issue. Participation is covered by the
[Code of Conduct](CODE_OF_CONDUCT.md). Contributions are accepted under the
[MIT License](LICENSE).
