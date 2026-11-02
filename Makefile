# voya developer Makefile.
#
# `make help` lists every target. `make ci` reproduces the GitHub Actions
# workflow locally.
#
# The repo holds six Go modules: the root module, authn, client, proto,
# test/acceptance and test/trailer. `go test ./...` does not span module
# boundaries, so every target that walks the tree walks all six explicitly —
# a harness CI silently skips is a harness that is not guarding anything.

SHELL          := /usr/bin/env bash
.SHELLFLAGS    := -eu -o pipefail -c
.DEFAULT_GOAL  := help

# .SHELLFLAGS needs GNU Make 3.82+, and macOS still ships 3.81 — where it is
# silently ignored, so recipes run without `-e`. A `for mod in ...` loop under
# 3.81 therefore reports only the LAST module's exit status: a failing root
# module with a passing test/trailer exits 0. Every loop below propagates
# failure explicitly with `|| exit $$?` rather than relying on `-e`.

# --- Paths --------------------------------------------------------------------

BIN_DIR        := bin
VOYA_BIN       := $(BIN_DIR)/voya

# Every module in the repo, as directories to run `go` in. This duplicates
# go.work's `use` block, and a module added to one but not the other is walked
# by nothing — the silent skip this file exists to prevent, one level up.
# `make modules-check` fails on that drift; `ci` and `check` both run it.
#
# Not derived from go.work directly: the go.local.work workflow documented at
# the top of go.work points GOWORK at sibling checkouts, and deriving would
# make `make test` walk into chameleon.
MODULES        := . authn client examples proto test/acceptance test/trailer

GO             ?= go
GOFLAGS        ?=

# --- Test knobs (override on the command line) --------------------------------

# 10m, not go's default 10m-by-accident and not the 5m this carried until
# #289. The number is measured rather than picked, and the measurement is the
# argument: `cmd/voya` is the binding package, and on the macOS runner host it
# runs its 109 tests in ~135s uncached — 141s wall, `ok … 135.485s` — while
# another repo's suite was live on the same box at 125% CPU. CI's own quiet
# figure is 2m33s. So 10m is roughly 4x the measured cost, and the run that
# forced this (#289, run 32486866787) died at 300s, about 2.2x that cost.
#
# The timeout bounds the test binary's *execution*, not the build, so a cold
# build cache does not eat into it; the job's own `timeout-minutes: 25` is
# what covers compilation, and 10m here stays well inside it so an overrun
# still fails as a Go test timeout — with the goroutine dump that makes it
# diagnosable — rather than as a job kill, which prints nothing useful.
#
# Raising it is not a weakening of hang detection, which is the reading to
# resist. At 5m the margin over a normal run was about 2x, so the timeout was
# firing on load: a red meant "the box was busy", and a real deadlock and a
# slow afternoon were indistinguishable. Widening the gap between normal and
# limit is what makes a timeout mean something again. What it costs is that a
# genuine hang takes 10m to surface instead of 5m, once, in a job already
# budgeted for 25.
#
# Where the time actually goes, measured the same run — the suite is not
# uniformly slow, it is five tests:
#
#   TestVoyaStatusIsInstantWithAFullRegistry      30.5s
#   TestPlainStatusIsNotSlowedByAWedgedService    27.1s
#   TestDiagCommandsWhenVoyaIsNotRunning          17.8s
#   TestVoyaHealthReportsEachServiceBesideItsEntry 14.9s
#   TestVoyaCallWhenVoyaIsNotRunning              11.2s
#
# ~101s of ~135s, with the other 104 tests in a 1-4s tail. The top two are
# latency tests, which is worth knowing before raising this again: they are
# the ones a loaded box distorts, and past some load they stop being slow and
# start being wrong. A bigger timeout does not help with that, and the note
# on latency assertions in CLAUDE.md is the relevant guidance.
TEST_FLAGS     ?= -race -timeout 10m
COVER_PROFILE  ?= coverage.out

# Pin to the CI version so local + CI lint behavior cannot diverge.
GOLANGCI_LINT_VERSION ?= v2.12.2

# --- Help (default) -----------------------------------------------------------

# Doc strings are read back out of this file as raw text, so they must not
# contain make syntax: `$$GOBIN` and `$(GOLANGCI_LINT_VERSION)` would print
# verbatim rather than as their values. Anything worth showing a value for
# goes in the footer below, where make expands it.
.PHONY: help
help: ## Show this help.
	@printf "Targets:\n"
	@grep -hE '^[a-zA-Z0-9_-]+:.*##' $(MAKEFILE_LIST) \
	  | sort \
	  | awk 'BEGIN{FS=":.*##[[:space:]]?"} {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
	@printf "\nPinned: golangci-lint $(GOLANGCI_LINT_VERSION)  |  modules: $(MODULES)\n"
	@printf "Proto:  buf $(BUF_VERSION)  protoc-gen-go $(PROTOC_GEN_GO_VERSION)  protoc-gen-connect-go $(PROTOC_GEN_CONNECT_GO_VERSION)\n"
	@printf "        breaking-change baseline: $(PROTO_BASELINE)\n"

# --- Build / install ----------------------------------------------------------

# CGO_ENABLED=0, for the reason `cross` states below and for one of its own.
# cgo is on by default wherever a C toolchain is, so without this a Mac with
# Xcode built a cgo-enabled voya while the same source cross-compiled and
# shipped pure — and `voya dns check`'s DNS check is a measurement whose whole
# point is which of those two a binary is (ADR 005 requires it to resolve
# *.internal from a CGO_ENABLED=0 build, because ADR 002's trap is invisible
# to any other one). A locally built check could not answer the question it
# exists to ask, and said so instead of answering (#99).
#
# Hard-set rather than `?=`, matching `cross`: an environment variable is
# already defined as far as make is concerned, so `?=` would defer to the
# CGO_ENABLED=1 in a developer's shell — the case this exists to pin. Building
# with cgo on purpose means invoking `go build` directly, which is also what
# `go install github.com/trendvidia/voya/cmd/voya@latest` does, so the report
# still has to stay honest about a build it did not produce.
.PHONY: build
build: $(VOYA_BIN) ## Build voya into ./bin/ (CGO_ENABLED=0, as voya ships).

# $(MAKEFILE_LIST) is a prerequisite because the build mode lives here rather
# than in the sources: without it, a checkout whose bin/voya was built before
# this pin keeps that cgo-enabled binary — the sources have not changed, so
# `make build` prints "Nothing to be done" and `make ci` never rebuilds it
# either. That is #99's symptom surviving its own fix, on exactly the machines
# that already had it.
$(VOYA_BIN): $(shell find cmd internal client proto -name '*.go' -not -name '*_test.go' 2>/dev/null) $(shell find internal -name '*.pxf' 2>/dev/null) go.mod $(MAKEFILE_LIST)
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $@ ./cmd/voya

.PHONY: install
install: ## go install voya into GOBIN (or GOPATH/bin), CGO_ENABLED=0.
	CGO_ENABLED=0 $(GO) install $(GOFLAGS) ./cmd/voya

# `build` compiles cmd/voya's import closure and nothing else, so a compile
# error in a root-module package outside it — `go list -deps ./cmd/voya` names
# the closure; internal/changelog, internal/tools/changelog, internal/testproc
# and internal/dnscheck/probe are outside it — used to reach install-check first
# and print under its sibling-tag heading (#255). This is the leg that names it
# as a compile error instead: every package of every module, test files
# included, compiled inside the workspace for the host GOOS, so it runs right
# after `build` and before any module-graph gate. Not a substitute for `vet`
# (which runs the analyzers too, later, because they are slower) or for `cross`
# (every target, deliberately last as the expensive leg); its whole job is to
# fail first and under its own name.
#
# The command is `go list -test -export`, not `go build`, because `go build`
# compiles non-test files only and has no flag to do otherwise: a _test.go
# file with an undefined symbol left the #255 leg at exit 0 and reached
# install-check's `GOWORK=off go vet`, which type-checks test files, so the
# error printed under the sibling-tag heading again (#271). `-test` adds each
# package's test variants and its generated `.test` main to the list, and
# `-export` has the go command compile every listed package to produce its
# export data — compile only, no link, which is what makes it cheaper than
# `go test -run '^$'` (links every test binary) and than `go vet` (compiles
# the same packages, then runs the analyzers on each). Measured on a cold
# build cache right after `make build`, root module (#271 has the numbers):
# under two thirds of vet's marginal cost and a quarter of `go test
# -run '^$'`'s. What the listing alone gives up is the link stage: a `main`
# package that compiles but does not link — no `func main()`, a
# `//go:linkname` with no target — is green under `-export` and would fail
# only in `cross`, the last leg, because `build` links `./cmd/voya` alone and
# the root module has two more `main` packages outside that closure
# (internal/tools/changelog, internal/dnscheck/probe). So #255's
# `go build -o /dev/null ./...` follows the listing, for the link stage
# alone; the packages are already compiled, so what it adds is the links.
# Measured warm, six modules, median of three, at load average ~200 (so read
# the ratio): 0.81s for the listing alone, 1.57s with the build — about 2x
# the leg, under a second of `make ci`. Decided in #276, close to a wash and
# tipped by the cost of being wrong each way: re-adding it and finding it
# wasteful is a one-line revert, and declining it is a debugging session the
# day a `main` package stops linking. Deleting `func main()` from
# internal/dnscheck/probe is the mutation that proves it: the listing passes,
# the build fails with the linker's `function main is undeclared in the main
# package` under that package's `# pkg` heading, and the recipe exits 1.
#
# A compile error fails the listing with exit 1 and the compiler's own
# `file:line: undefined: x` on stderr, under a `# pkg [pkg.test]` heading when
# it is in a test file — and under both that heading and the plain `# pkg` one
# when it is not, because the test variant recompiles the package's non-test
# files, which is what the recipe's message says. The listing's stdout
# carries the package list and goes to /dev/null; the build's `-o /dev/null`
# is the one non-directory `go build` accepts for a `./...` that resolves to
# more than one package. That refusal ("cannot write multiple packages to
# non-directory") is about the package count, not about main-ness: proto,
# which has no `main` package at all, refuses `-o <file>` just the same. It
# is also what keeps a module whose `./...` ever resolves to a single `main`
# package from leaving a binary behind the way a bare `go build` would.
# `build` itself is not widened: that target exists to produce the binary the
# way `go install` does, and a failure there keeps exactly one meaning.
#
# test/acceptance, test/trailer and the root's test/roundtrip and test/gates
# have no non-test files at all; their test variants are what this leg compiles
# there, so their `==> go list` lines mean something (#271). Their
# `==> go build` line does not: `go build ./...` drops a test-only package,
# so in test/acceptance and test/trailer that step is green having built
# nothing. Only the root module has `main` packages at all — cmd/voya,
# internal/tools/changelog, internal/dnscheck/probe — so the link stage adds
# coverage there alone, and the other five `==> go build` lines are the loop
# being uniform rather than five more link checks. A non-test file behind a
# build constraint this leg does not satisfy — another GOOS, or cgo, since it
# runs CGO_ENABLED=0 — still lands on install-check first (its GOOS loop),
# which is why that leg's messages keep hedging on `make vet` and `make
# cross`. A _test.go file behind another GOOS's constraint lands there too
# since #267 — but only the root module's: that loop vets rather than builds,
# so it types the root's test files for every target, and its message hedges
# on this leg by name. The other five modules' constrained test files are
# typed here, for the host GOOS, and by nothing else.
.PHONY: compile-check
compile-check: ## Fail if any module's packages, test files included, do not compile in the workspace, or the root's main packages do not link.
	@for mod in $(MODULES); do \
	  echo "==> go list -test -export ./... $$mod"; \
	  (cd $$mod && CGO_ENABLED=0 $(GO) list $(GOFLAGS) -test -export ./... >/dev/null) || { \
	    echo "compile-check: $$mod does not compile inside the workspace. This is an"; \
	    echo "ordinary compile error; the sibling tags and the module graph have"; \
	    echo "nothing to do with it. An error printed only under a '[pkg.test]'"; \
	    echo "heading is in a _test.go file; one in a non-test file prints twice,"; \
	    echo "plain and under that heading too, because the test variant compiles"; \
	    echo "it again."; \
	    exit 1; \
	  }; \
	  echo "==> go build -o /dev/null ./... $$mod"; \
	  (cd $$mod && CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o /dev/null ./...) || { \
	    echo "compile-check: $$mod compiles but does not link inside the workspace."; \
	    echo "The listing above passed, so the link stage is all this step adds:"; \
	    echo "usually a main package named in the '# pkg' heading above, with no"; \
	    echo "func main() or a go:linkname with no target. Only the root module"; \
	    echo "has main packages, so a failure in any other one is the build"; \
	    echo "disagreeing with the listing. The sibling tags and the module"; \
	    echo "graph have nothing to do with it."; \
	    exit 1; \
	  }; \
	done

# --- Test ---------------------------------------------------------------------

.PHONY: test
test: ## Run every module's tests with the race detector (mirrors CI).
	@for mod in $(MODULES); do \
	  echo "==> go test $$mod"; \
	  (cd $$mod && $(GO) test $(TEST_FLAGS) ./...) || exit $$?; \
	done

.PHONY: test-short
test-short: ## Quick smoke run: skip -race, every module.
	@for mod in $(MODULES); do \
	  echo "==> go test -short $$mod"; \
	  (cd $$mod && $(GO) test -short ./...) || exit $$?; \
	done

.PHONY: test-trailer
test-trailer: ## Run only the gRPC trailer conformance harness (ADR 003 evidence).
	cd test/trailer && $(GO) test -v $(TEST_FLAGS) ./...

.PHONY: cover
cover: ## Write a coverage profile for the root module and print a summary.
	$(GO) test $(TEST_FLAGS) -coverprofile=$(COVER_PROFILE) -covermode=atomic ./...
	$(GO) tool cover -func=$(COVER_PROFILE) | tail -n 20

.PHONY: cover-html
cover-html: cover ## Open the coverage profile in a browser.
	$(GO) tool cover -html=$(COVER_PROFILE)

# --- Static analysis ----------------------------------------------------------

.PHONY: vet
vet: ## go vet every module.
	@for mod in $(MODULES); do \
	  echo "==> go vet $$mod"; \
	  (cd $$mod && $(GO) vet ./...) || exit $$?; \
	done

.PHONY: lint
lint: ## golangci-lint run over every module (version pinned; see footer).
	@command -v golangci-lint >/dev/null 2>&1 || { \
	  echo "golangci-lint not on PATH — run 'make tools' to install $(GOLANGCI_LINT_VERSION)."; \
	  exit 1; \
	}
	@# The pin above only binds what `make tools` installs. Check what is
	@# actually on PATH, or "local and CI cannot diverge" is an assertion
	@# rather than a guarantee. An unparseable version warns instead of
	@# failing, so a future output format change cannot break `make lint`.
	@# The `|| have=""` is load-bearing: under `.SHELLFLAGS`' `-e -o pipefail`
	@# a non-zero `golangci-lint version` aborts the recipe before the warning
	@# branch can run. That failed silently on GNU Make 4.x (Linux CI) while
	@# warning correctly on macOS 3.81, where .SHELLFLAGS is ignored — the
	@# very 3.81-vs-4.x split this file's header comment exists to close.
	@want=$$(echo '$(GOLANGCI_LINT_VERSION)' | sed 's/^v//'); \
	have=$$(golangci-lint version 2>/dev/null | sed -n 's/.*has version v\{0,1\}\([0-9][^ ]*\).*/\1/p') || have=""; \
	if [ -z "$$have" ]; then \
	  echo "warning: cannot parse the golangci-lint version; expected v$$want."; \
	elif [ "$$have" != "$$want" ]; then \
	  echo "golangci-lint v$$have is on PATH but this repo pins v$$want — run 'make tools'."; \
	  exit 1; \
	fi
	@for mod in $(MODULES); do \
	  echo "==> golangci-lint run $$mod"; \
	  (cd $$mod && golangci-lint run) || exit $$?; \
	done

# --- Protobuf -----------------------------------------------------------------

# The generated .pb.go / .connect.go files are committed: `go install
# github.com/trendvidia/voya/cmd/voya` is how voya is distributed (ADR 001)
# and it runs no code generator. So the thing that can go wrong is not "the
# build lacks a plugin" but "what is committed no longer matches the .proto"
# — which `proto-check` is here to fail on.
#
# Every tool is pinned and installed into ./bin rather than $GOBIN: the
# plugin version decides the bytes of the generated output, so an unpinned
# plugin makes proto-check fail for whoever has a different one, and writing
# into $GOBIN would change what other repos on this machine generate.
#
# protoc-gen-go tracks google.golang.org/protobuf and protoc-gen-connect-go
# tracks connectrpc.com/connect, both in go.mod. Bump them together.
PROTO_DIR                     := proto
PROTOC_GEN_GO_VERSION         ?= v1.36.12
PROTOC_GEN_CONNECT_GO_VERSION ?= v1.20.0
BUF_VERSION                   ?= v1.45.0

# The targets below are receipts, not the binaries themselves. `bin/buf` as a
# file target is satisfied forever by the first install, so bumping a pin
# above would not reinstall: `make generate` would keep running the old
# plugin, `make proto-check` would pass locally, and CI — which always
# installs into an empty ./bin — would produce different bytes and fail on a
# diff nobody can reproduce. That is the failure this whole section exists to
# prevent, so the version goes in the receipt's name: a bump has no receipt
# and forces the install. Stale receipts are swept so ./bin does not
# accumulate one per version ever pinned.
BUF_STAMP                     := $(BIN_DIR)/.buf-$(BUF_VERSION)
PROTOC_GEN_GO_STAMP           := $(BIN_DIR)/.protoc-gen-go-$(PROTOC_GEN_GO_VERSION)
PROTOC_GEN_CONNECT_GO_STAMP   := $(BIN_DIR)/.protoc-gen-connect-go-$(PROTOC_GEN_CONNECT_GO_VERSION)

PROTO_TOOLS := $(BUF_STAMP) $(PROTOC_GEN_GO_STAMP) $(PROTOC_GEN_CONNECT_GO_STAMP)

$(BUF_STAMP):
	GOBIN=$(CURDIR)/$(BIN_DIR) $(GO) install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)
	@rm -f $(BIN_DIR)/.buf-*
	@touch $@

$(PROTOC_GEN_GO_STAMP):
	GOBIN=$(CURDIR)/$(BIN_DIR) $(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	@rm -f $(BIN_DIR)/.protoc-gen-go-*
	@touch $@

$(PROTOC_GEN_CONNECT_GO_STAMP):
	GOBIN=$(CURDIR)/$(BIN_DIR) $(GO) install connectrpc.com/connect/cmd/protoc-gen-connect-go@$(PROTOC_GEN_CONNECT_GO_VERSION)
	@rm -f $(BIN_DIR)/.protoc-gen-connect-go-*
	@touch $@

.PHONY: proto-tools
proto-tools: $(PROTO_TOOLS) ## Install the pinned buf + protoc plugins into ./bin.

# ./bin goes on PATH first so buf finds the pinned plugins even when other
# versions are installed globally — the usual cause of a spurious diff.
.PHONY: generate
generate: proto-tools ## Regenerate Go bindings from proto/ (pinned toolchain).
	cd $(PROTO_DIR) && PATH="$(CURDIR)/$(BIN_DIR):$$PATH" buf generate

.PHONY: proto-lint
proto-lint: $(BUF_STAMP) ## Lint the .proto files.
	cd $(PROTO_DIR) && $(CURDIR)/$(BIN_DIR)/buf lint

# What a schema change is compared against. A remote-tracking ref, not the
# local `main`: a laptop's `main` is whatever was last pulled, so the check
# would quietly weaken exactly when the branch is behind. Override to compare
# against something else (a tag, a release branch) while working.
PROTO_BASELINE ?= origin/main

.PHONY: proto-breaking
proto-breaking: $(BUF_STAMP) ## Fail if proto/ breaks compatibility with the baseline (see footer).
	@# buf reads the baseline out of .git directly rather than from a
	@# worktree, so this needs the ref to exist locally — which it does not
	@# after a shallow clone. Say that, instead of letting buf report it as
	@# a malformed input.
	@git rev-parse --verify --quiet '$(PROTO_BASELINE)^{commit}' >/dev/null || { \
	  echo "proto-breaking: no such ref '$(PROTO_BASELINE)' — run 'git fetch origin' (CI needs fetch-depth: 0)."; \
	  exit 1; \
	}
	cd $(PROTO_DIR) && $(CURDIR)/$(BIN_DIR)/buf breaking \
	  --against '../.git#ref=$(PROTO_BASELINE),subdir=$(PROTO_DIR)'

.PHONY: proto-check
proto-check: generate ## Fail if the committed bindings are not what proto/ generates.
	@# Scoped to the generator's own output, and compared against HEAD.
	@#
	@# Scoped, because the .proto sources, buf.yaml and buf.gen.yaml are
	@# hand-written: an uncommitted edit to one of them is not stale
	@# generated code, and saying so sends the developer to `make generate`
	@# — which has just run, and changed nothing. Editing a comment in
	@# buf.yaml was enough to fail this target.
	@#
	@# Against HEAD, because "committed" is what the docstring promises and
	@# what `go install` will fetch. `git diff` alone compares the worktree
	@# to the index, so bindings that were staged while stale read as clean.
	@moved=$$( { git diff --name-only HEAD -- $(PROTO_DIR); \
	             git ls-files --others --exclude-standard -- $(PROTO_DIR); } \
	          | grep -E '\.(pb|connect)\.go$$' || true); \
	if [ -n "$$moved" ]; then \
	  echo "Generated code is stale — run 'make generate' and commit the result:"; \
	  printf '  %s\n' $$moved; \
	  exit 1; \
	fi

# --- Formatting ---------------------------------------------------------------

# vendor/ is excluded because `go mod vendor` output is not ours to reformat;
# .gitignore already anticipates it being present.
#
# Driven through `find -exec ... +` rather than a `$(shell find)` word list,
# for two reasons. `gofmt` with no file arguments reads stdin, so an empty
# expansion makes `make fmt-check` — and therefore `make check` — hang on a
# terminal instead of passing or failing. And an unquoted word list splits any
# path containing a space into two nonexistent paths. `-exec +` runs nothing at
# all when nothing matches, and passes each path as one argument.
GO_FIND := find . -name '*.go' -not -path './bin/*' -not -path './vendor/*'

.PHONY: fmt
fmt: ## gofmt -w -s on all Go files.
	@$(GO_FIND) -exec gofmt -w -s {} +

.PHONY: fmt-check
fmt-check: ## Fail if anything is not gofmt-clean.
	@unformatted=$$($(GO_FIND) -exec gofmt -l {} +); \
	if [ -n "$$unformatted" ]; then \
	  echo "Unformatted files (run 'make fmt'):"; echo "$$unformatted"; exit 1; \
	fi

# --- Module hygiene -----------------------------------------------------------

# MODULES is hand-maintained; go.work's `use` block is the real inventory. If
# they disagree, every walking target above silently skips a module and still
# exits 0 — the failure this file was written to prevent. GOWORK is forced to
# the committed go.work so a go.local.work in the shell cannot mask the drift.
.PHONY: modules-check
modules-check: ## Fail if MODULES has drifted from go.work's use block.
	@declared=$$(GOWORK=$(CURDIR)/go.work $(GO) work edit -json \
	  | sed -n 's/.*"DiskPath": "\(.*\)".*/\1/p' | sed 's#^\./##' | sort) || exit $$?; \
	listed=$$(printf '%s\n' $(MODULES) | sed 's#^\./##' | sort); \
	if [ -z "$$declared" ]; then \
	  echo "modules-check: go.work declares no modules — refusing to pass."; exit 1; \
	fi; \
	if [ "$$declared" != "$$listed" ]; then \
	  echo "MODULES and go.work disagree."; \
	  echo "  go.work: $$(echo $$declared)"; \
	  echo "  MODULES: $$(echo $$listed)"; \
	  echo "Update whichever is wrong — and the lint matrix in .github/workflows/ci.yml."; \
	  exit 1; \
	fi; \
	for mod in $(MODULES); do \
	  [ "$$mod" = "." ] && continue; \
	  grep -q "working-directory: $$mod$$" $(CURDIR)/.github/workflows/ci.yml && continue; \
	  echo "$$mod has no golangci-lint step in .github/workflows/ci.yml."; \
	  echo "The lint job is that second, hand-maintained copy of MODULES: a module missing"; \
	  echo "from it is linted by 'make lint' and never by CI, which is how the newest"; \
	  echo "module — always the least reviewed — ends up the only unlinted one."; \
	  exit 1; \
	done

# CLAUDE.md's Commands section prints the `ci` chain, and that listing is the
# same species of hand-maintained second copy as ci.yml's lint matrix above:
# #277 added acceptance-check to `ci:` and to ci.yml, edited a CLAUDE.md
# paragraph ten lines below the listing, and left the listing stale. #279
# decided a gate rather than deleting the listing — it is an index into a
# file this long, and every other must-not-drift copy here is gated
# (modules-check, client-check, acceptance-check, require-check, sync-check)
# rather than removed.
#
# It compares the target NAMES, tokenised, not the text. The CLAUDE.md side is
# a hand-wrapped `#` continuation block, so a literal diff would fire on a
# rewrap that changed nothing, and a gate that cries wolf is one people learn
# to re-run until it passes. Order is compared with the names, because the
# chain's order is the argument #247 makes about which leg fails first.
#
# The first line's prose is stripped as far as its colon (`# the gate:`), so
# rewording that lead-in is free — but the colon is what marks where the prose
# ends, so a lead-in that loses it parses as nothing at all and takes the empty
# refusal below, with a message naming the shape it wanted. Without that, the
# lead-in's English words parse as target names and the failure is reported as
# drift in the listing, which is not what went wrong.
# Both sides empty-check for the reason client-check does: a parse that breaks
# silently would leave `make ci` green while this guards nothing.
.PHONY: ci-listing-check
ci-listing-check: ## Fail if CLAUDE.md's ci chain listing has drifted from ci's prerequisites (#279).
	@target=$$(sed -n 's/^ci:[[:space:]]*//p' $(CURDIR)/Makefile \
	  | sed 's/[[:space:]]*##.*//' | tr -s '[:space:]' '\n' | sed '/^$$/d'); \
	doc=$$(awk '/^\$$ make ci[[:space:]]/ { \
	             sub(/^[^#]*#/, ""); \
	             if (!sub(/^[^:]*:/, "")) exit; \
	             print; seen = 1; next } \
	           seen && /^[[:space:]]+#/ { sub(/^[[:space:]]*#/, ""); print; next } \
	           seen { exit }' $(CURDIR)/CLAUDE.md \
	  | tr -s '[:space:]' '\n' | sed '/^$$/d'); \
	if [ -z "$$target" ]; then \
	  echo "ci-listing-check: parsed no prerequisites out of the Makefile's ci: target."; \
	  echo "That is this gate breaking, not the chain being empty — refusing to pass."; \
	  exit 1; \
	fi; \
	if [ -z "$$doc" ]; then \
	  echo "ci-listing-check: parsed no target names out of CLAUDE.md's '\$$ make ci' block."; \
	  echo "Expected a '# the gate: <names>' line under Commands, continued on lines that"; \
	  echo "start with whitespace and '#'. That is this gate breaking, not the listing"; \
	  echo "being empty — refusing to pass."; \
	  exit 1; \
	fi; \
	if [ "$$target" != "$$doc" ]; then \
	  echo "ci-listing-check: CLAUDE.md's ci chain listing and the ci: target disagree."; \
	  echo "  ci:        $$(echo $$target)"; \
	  echo "  CLAUDE.md: $$(echo $$doc)"; \
	  echo "Only in ci:        $$(comm -23 <(echo "$$target" | sort) <(echo "$$doc" | sort) | tr '\n' ' ')"; \
	  echo "Only in CLAUDE.md: $$(comm -13 <(echo "$$target" | sort) <(echo "$$doc" | sort) | tr '\n' ' ')"; \
	  echo "If both sides hold the same names, they are in a different order: the chain's"; \
	  echo "order is load-bearing (#247 — which leg fails first), so the listing follows it."; \
	  echo "Update whichever is wrong — and .github/workflows/ci.yml, the third copy."; \
	  exit 1; \
	fi

# ci.yml's step list is that third copy, and it is the consequential one: it
# decides what actually runs. A leg added to `ci:` and to CLAUDE.md but not
# here passes every other gate and never runs in CI — #277's shape, one copy
# over, and until #284 it was recorded only inside ci-listing-check's failure
# message, which by construction never prints for a ci.yml-only drift.
#
# Subset, not equality, and order is NOT compared (#284 decided both). `ci:`'s
# single sequence maps onto five parallel jobs — test on two platforms, lint,
# proto, cross — so there is no sequence here to compare it against. Only
# inside the `test` job are the steps sequential, which is the one place
# #247's fail-fast order argument holds; a subsequence check scoped there is a
# separate question, not this leg's. The accepted cost is that a leg moved to
# the wrong job, or run after builds it was meant to precede, still passes.
#
# Comments are stripped from ci.yml before anything is searched, in BOTH
# directions, and that is load-bearing rather than tidiness: `make lint`
# appears twice in this file's own prose explaining why lint does not run that
# way. Counting prose as coverage would let a leg that is documented but never
# wired pass this gate — the exact failure it exists to catch.
#
# Trailing comments are stripped as well as whole-line ones, and that is the
# same argument at line scope: `run: echo disabled  # was: make cross` passed
# this gate while `cross` never ran in CI, which is precisely the drift it
# exists to catch. A `#` counts as a comment where YAML says it does — at the
# start of a line, or after whitespace — so a `#` inside a quoted string is
# stripped too. That direction is the safe one: it can only lose coverage a
# step claimed, never invent it.
#
# The name has to be followed by a non-target character or end of line, not by
# a space alone: `run: "make cross"`, `make cross;` and `(cd x && make vet)`
# all run the leg, and a space-only boundary called each of them missing while
# advising the author to add the step they had already written. `-` stays out
# of the boundary class so `make test` does not match `make test-only`.
#
# Two shapes below are deliberate under `.SHELLFLAGS`' `-e -o pipefail`, and
# both revert to a silent wrong answer if simplified back:
#
#   - `sed …` rather than `grep -v`, because grep exits 1 when it selects
#     nothing, which under `-e` kills the recipe at the assignment — with
#     make's bare `Error 1` instead of the empty-parse refusal below, which
#     was unreachable for the all-comment file it names.
#   - `grep -q … <<<"$yml"` rather than `printf … | grep -q …`, because
#     `grep -q` exits at the first match and SIGPIPEs the writer; with
#     pipefail the pipeline then returns 141 on a *match* once the text
#     exceeds the pipe buffer. Measured at 64 KB — ci.yml is 4 KB of
#     searchable text today, so it was latent, and the reverse loop's branch
#     fails open.
#
# CI_YML_EXEMPT is the legs that deliberately do not run as `make <name>`:
#
#   lint — golangci-lint-action supplies the module cache and the inline PR
#          annotations `make lint` cannot, so the lint job runs five
#          per-module steps instead. ci.yml says so at its lint job.
#
# `test` is NOT exempt and must not be added: it runs as `make test` inside a
# `run: |` block that calls `go version` first, so it is found by a search for
# `make test` and only missed by one for `run: make test`. Measuring with the
# narrower pattern is what made #284 report three divergent legs where there
# is one.
#
# Each exempt entry is checked both ways — still a `ci:` leg, and still absent
# from ci.yml — so an exemption that stops being true fails instead of rotting.
CI_YML_EXEMPT := lint

# Several legs here depend on *which command* the recipe runs, not merely on
# the recipe existing, and each of those choices was argued for and measured
# before it was made. Until #286 nothing failed when one was narrowed back:
# `make ci` stayed green and the leg silently stopped guarding what its own
# comment said it guarded. The repo has had that failure twice — a recipe
# mismerge that left the newer gate looping over an unset variable, and #275,
# where a derived set that shrank dropped the refusals that only fired on what
# it dropped. Both were one-word, both green, both found by reading rather
# than by a gate. The common cause is that a gate's argument lives in a
# comment while its behaviour lives in a recipe, and only the comment gets
# reviewed.
#
# So this leg pins the command. Decided in #286 over the alternative — a
# permanent corpus of build-constrained files each gate must be red on, which
# pins behaviour rather than text but means a leg whose job is to redden
# `make ci`, and roughly doubles the runtime of every leg it covers. What was
# bought instead is a text pin at no measurable cost, and the weakness is
# stated rather than hidden: RECIPE_PINS is hand-maintained, the very drift
# surface #267 and #275 just finished removing from client-check. It gets the
# CI_YML_EXEMPT treatment for that reason — a row naming a target that no
# longer exists, or a command no longer in that target, fails here, so a stale
# row cannot sit in the table looking like coverage.
#
# Two details carry the check, and neither is incidental. What a recipe
# prints is stripped before the count: install-check prints `go vet ./...` in
# its heading and prints the whole `GOOS=… go vet ./...` remedy in its failure
# message, so a search of the raw recipe would find the string three times
# over with the command itself deleted — a pin that passes on its own
# decoration, which is the shape this repo has shipped before in a negative
# control that built its own corpus. The strip covers `echo` and `printf` in
# either quote style, and it takes the quoted string rather than the whole
# line, because `want=$(echo $want)` is a command and not decoration. Its
# residual, recorded rather than closed: an unquoted `echo go vet ./...`, a
# heredoc, or a message assembled in a variable would all still count — so a
# leg that prints its command any of those ways needs the strip widening
# before it is pinned here, or its row will pass on the printing alone.
#
# And the body is extracted per target rather than searched for file-wide,
# which is also what keeps this target's own table — full of the very strings
# it searches for — from counting as matches against the legs it pins.
#
# The count is of matching *lines*, not occurrences, because that is what
# `grep -c` gives; the two matches install-check's row wants are on separate
# lines, so the distinction does not bite today. A leg that grew two matches on one line
# would need the pattern splitting, not the count raising.
#
# What this cannot catch, recorded rather than closed: a change that keeps the
# string and breaks the behaviour around it — `go vet ./...` moved inside a
# branch that never runs, or its exit status discarded. That is the half the
# rejected mutation corpus would have covered, and #286 has the argument.
#
# Each row is target|pattern|minimum matching lines|why. The pattern is a
# fixed string, not a regex, and deliberately carries no `$` so that neither
# make nor the shell rewrites it on the way to grep.
define RECIPE_PINS
compile-check|-test -export|1|#271: go build compiles no _test.go, so the listing is what types them
install-check|vet ./...|2|#246 the host pass and #267 the cross-target loop; go build compiles no _test.go
client-check|vet -modfile=|1|#230: vet types client's test files, a build would not
acceptance-check|vet -modfile=|1|#270: the same question asked through test/acceptance's own go.mod
cross|{{if .GoFiles}}|1|#86: a raw go list keeps a package listed via an unconstrained _test.go
endef
export RECIPE_PINS

.PHONY: recipe-check
recipe-check: ## Fail if a gate's recipe no longer runs the command its argument depends on (#286).
	@fail=0; rows=0; \
	while IFS='|' read -r target pat want why; do \
	  [ -n "$$target" ] || continue; \
	  rows=$$((rows+1)); \
	  case "$$pat" in *[![:space:]]*) ;; *) \
	    echo "recipe-check: row '$$target' has a blank pattern field. grep -F of an"; \
	    echo "empty or all-space string matches nearly every line of the recipe, so"; \
	    echo "the row passes whatever that recipe runs — the same always-true shape"; \
	    echo "the count guard below refuses, reported as coverage just the same."; \
	    echo "Fix the row: target|pattern|minimum lines|why."; \
	    fail=1; continue;; \
	  esac; \
	  case "$$want" in ''|*[!0-9]*|0*) \
	    echo "recipe-check: row '$$target' has a count field of '$$want', which is not a"; \
	    echo "positive number. Both ways of getting that wrong pass silently if this"; \
	    echo "check is not here: a non-number makes bash print 'integer expected' and"; \
	    echo "treat the comparison as false, and a 0 asks the row to find nothing, which"; \
	    echo "it always does. Either way the row reports coverage it is not providing."; \
	    echo "Fix the row: target|pattern|minimum lines|why."; \
	    fail=1; continue;; \
	  esac; \
	  body=$$(awk -v t="$$target" '$$0 ~ "^" t ":" {p=1; next} p && /^\t/ {print; next} p {exit}' \
	    $(CURDIR)/Makefile \
	    | sed -e 's/[[:space:]]*echo "[^"]*"//g' \
	          -e "s/[[:space:]]*echo '[^']*'//g" \
	          -e 's/[[:space:]]*printf "[^"]*"/ printf/g' \
	          -e "s/[[:space:]]*printf '[^']*'/ printf/g"); \
	  if [ -z "$$body" ]; then \
	    echo "recipe-check: RECIPE_PINS names '$$target', which has no recipe in this"; \
	    echo "Makefile. Either the target was renamed and the row was not, or the"; \
	    echo "extraction above broke. A row that matches nothing is not coverage —"; \
	    echo "fix the row or drop it, on purpose."; \
	    fail=1; continue; \
	  fi; \
	  n=$$(printf '%s\n' "$$body" | grep -F -c -- "$$pat") || n=0; \
	  if [ "$$n" -lt "$$want" ]; then \
	    echo "recipe-check: $$target no longer runs '$$pat' ($$n of $$want expected"; \
	    echo "line(s); what the recipe echoes or printfs does not count). Why it"; \
	    echo "must: $$why"; \
	    echo "If the leg was narrowed by accident — a mismerge, or a revert that read"; \
	    echo "as a simplification — restore the command. If it was narrowed on purpose,"; \
	    echo "say so in RECIPE_PINS above and in that target's comment, because the"; \
	    echo "argument written there is what this row exists to keep true."; \
	    fail=1; \
	  fi; \
	done <<<"$$RECIPE_PINS"; \
	if [ "$$rows" -eq 0 ]; then \
	  echo "recipe-check: RECIPE_PINS parsed empty, so this gate just checked nothing"; \
	  echo "and would have passed. That is this gate breaking, not the table being"; \
	  echo "empty on purpose — refusing to pass."; \
	  exit 1; \
	fi; \
	if [ "$$fail" -ne 0 ]; then exit 1; fi; \
	echo "==> recipe-check: $$rows command pin(s) intact"

# workflow-check's second job (#324): every `runs-on` in every workflow has to
# be a GitHub-hosted runner. This repository is public, and a pull request from
# a fork runs the workflow it carries, so on a self-hosted runner that is a
# stranger's code on a real machine. Until #324 only a comment in ci.yml said so.
#
# It is an allowlist rather than a search for the word `self-hosted`, because
# that word is not what reaches a self-hosted runner. `runs-on: voya` does it on
# a custom label alone, and a runner group does it with a mapping. So
# HOSTED_RUNS_ON says what a line may hold: one hosted label, or
# `${{ matrix.runs-on }}`, whose values are `runs-on:` lines themselves and so
# pass through this same check. A hosted label is spelled the way GitHub spells
# them, an OS, then `latest` or a version, then an optional size or architecture,
# because `ubuntu-gpu` is a custom label that only looks like one. Every other
# line that mentions runs-on is refused. That includes shapes that could be
# hosted but cannot be read one line at a time (a list, a block, `${{ matrix.os
# }}`), and a hosted label GitHub adds later in a new spelling. Each of those
# costs one edit, and the wrong pass costs what the paragraph above says.
#
# Every file in .github/workflows is read, not only ci.yml: a new workflow would
# reach a runner without touching this one. Comments are stripped first, by the
# rule the comment above CI_YML_EXEMPT gives and in both directions: prose may
# say `runs-on: self-hosted`, and a trailing comment cannot launder a live line.
# awk does the strip here, with `[ \t]`, because it has to keep each file's name
# and line number for the refusal. YAML's whitespace is those two characters.
#
# test/gates runs this recipe on a copy of the real files, one refused shape at
# a time, so `make test` is what notices the check being narrowed.
#
# What this does not stop, recorded rather than closed:
#
#   - A fork's own pull request. Its run uses the workflow it carries, gate
#     included, so this guards what merges and not what a stranger's PR asks
#     for. That run is stopped only by no self-hosted runner being open to this
#     repository, and by the approval of outside contributors' runs that #309
#     sets.
#   - A matrix built at run time, `matrix: ${{ fromJSON(...) }}`, behind
#     `${{ matrix.runs-on }}`. Its values are not in the file. Refusing the
#     shape would also refuse a hosted job that uses it for anything else.
HOSTED_RUNS_ON := ^[^:]*:[0-9]+:[[:space:]]*(- )?runs-on:[[:space:]]*((ubuntu|macos|windows)-(latest|[0-9.]+)(-(arm|intel|large|xlarge))?|\$$\{\{ matrix\.runs-on \}\})[[:space:]]*$$

.PHONY: workflow-check
workflow-check: ## Fail if a ci: leg has no step in ci.yml (#284), or a workflow's runs-on is not GitHub-hosted (#324).
	@yml=$$(sed -e 's/^[[:space:]]*#.*$$//' -e 's/[[:space:]]#.*$$//' \
	  $(CURDIR)/.github/workflows/ci.yml | sed '/^[[:space:]]*$$/d'); \
	target=$$(sed -n 's/^ci:[[:space:]]*//p' $(CURDIR)/Makefile \
	  | sed 's/[[:space:]]*##.*//' | tr -s '[:space:]' '\n' | sed '/^$$/d'); \
	if [ -z "$$target" ]; then \
	  echo "workflow-check: parsed no prerequisites out of the Makefile's ci: target."; \
	  echo "That is this gate breaking, not the chain being empty — refusing to pass."; \
	  exit 1; \
	fi; \
	if [ -z "$$yml" ]; then \
	  echo "workflow-check: .github/workflows/ci.yml parsed empty once comments were"; \
	  echo "stripped. That is this gate breaking, not the workflow being empty —"; \
	  echo "refusing to pass."; \
	  exit 1; \
	fi; \
	missing=""; \
	for n in $$target; do \
	  case " $(CI_YML_EXEMPT) " in *" $$n "*) continue ;; esac; \
	  grep -qE "make $$n([^-A-Za-z0-9_]|\$$)" <<<"$$yml" || missing="$$missing $$n"; \
	done; \
	if [ -n "$$missing" ]; then \
	  echo "workflow-check: ci: names leg(s) that .github/workflows/ci.yml never runs:"; \
	  echo "  $$(echo $$missing)"; \
	  echo "CI would be green without ever running them. Add a step that calls"; \
	  echo "\`make <name>\`, or — if the leg runs differently in CI on purpose — add it"; \
	  echo "to CI_YML_EXEMPT in this Makefile with the reason, the way lint is."; \
	  echo "A mention in a ci.yml comment does not count: comments are stripped first."; \
	  exit 1; \
	fi; \
	for n in $(CI_YML_EXEMPT); do \
	  case " $$(echo $$target) " in \
	    *" $$n "*) ;; \
	    *) echo "workflow-check: CI_YML_EXEMPT holds '$$n', which is no longer a ci: leg."; \
	       echo "Drop it — an exemption for a target that does not run is a stale note."; \
	       exit 1 ;; \
	  esac; \
	  if grep -qE "make $$n([^-A-Za-z0-9_]|\$$)" <<<"$$yml"; then \
	    echo "workflow-check: CI_YML_EXEMPT holds '$$n', but ci.yml now runs \`make $$n\`."; \
	    echo "The exemption is obsolete — drop it so the leg is gated like the rest."; \
	    exit 1; \
	  fi; \
	done; \
	hits=$$(awk '{ sub(/^[ \t]*#.*$$/, ""); sub(/[ \t]#.*$$/, "") } \
	  /runs-on/ { n = FILENAME; sub(/.*\//, "", n); print n ":" FNR ":" $$0 }' \
	  "$(CURDIR)"/.github/workflows/*.y*ml); \
	if [ -z "$$hits" ]; then \
	  echo "workflow-check: found no runs-on in .github/workflows. That is this gate"; \
	  echo "breaking, not the jobs having nowhere to run — refusing to pass."; \
	  exit 1; \
	fi; \
	bad=$$(sed -E '/$(HOSTED_RUNS_ON)/d' <<<"$$hits"); \
	if [ -n "$$bad" ]; then \
	  echo "workflow-check: a workflow names a runner this gate cannot show is GitHub-hosted:"; \
	  sed 's/^/  /' <<<"$$bad"; \
	  echo "This repository is public, so a pull request from a fork runs the workflow it"; \
	  echo "carries, and on a self-hosted runner that is a stranger's code on a real"; \
	  echo "machine. A custom label or a runner group reaches one without saying so."; \
	  echo 'Use one hosted label per line (ubuntu-latest, macos-latest, windows-latest),'; \
	  echo 'or $${{ matrix.runs-on }} with each value on a runs-on: line of its own.'; \
	  exit 1; \
	fi; \
	echo "==> workflow-check: $$(echo $$(wc -l <<<"$$hits")) runs-on line(s), all GitHub-hosted"

# Every other check in this file runs inside the workspace, where the go
# command falls back to go.work.sum. A go.sum missing its dependencies'
# /go.mod hashes therefore builds, tests and lints clean here and in CI, while
# `go install github.com/trendvidia/voya/cmd/voya` — how voya is distributed
# (ADR 001) — fails on a machine that has no workspace. GOWORK=off is the
# whole point: it is the only way to see each module the way `go install`
# will.
#
# The remedy printed for "does not contain package" matches on the missing
# package's path, not on the phrase alone: go says the same thing about any
# module that lost a package or never had one, and "land the sibling change
# on main, have its tag cut there" is wrong advice for an external dependency
# — for which `go mod tidy`, or a version bump, is the fix.
.PHONY: tidy-check
tidy-check: ## Fail if a module's go.mod/go.sum is untidy outside the workspace.
	@for mod in $(MODULES); do \
	  echo "==> go mod tidy -diff $$mod"; \
	  out=$$(cd $$mod && GOWORK=off $(GO) mod tidy -diff 2>&1) || { \
	    printf '%s\n' "$$out"; \
	    case "$$out" in \
	      *"does not contain package github.com/trendvidia/voya/"*) \
	        echo "tidy-check: $$mod imports a package of an intra-repo module that the published"; \
	        echo "version does not have (go's 'does not contain package' above names it). Running"; \
	        echo "'go mod tidy' will not fix this and neither will anything else in this PR: the"; \
	        echo "package exists only in the working tree, and a workspace-less build resolves the"; \
	        echo "sibling from its tag. Land the sibling change on main, have its tag cut there,"; \
	        echo "then land the require bump with the code that imports it (#230, #235).";; \
	      *) echo "$$mod is not tidy — run 'GOWORK=off go mod tidy' there and commit go.mod/go.sum.";; \
	    esac; \
	    exit 1; \
	  }; \
	  [ -z "$$out" ] || printf '%s\n' "$$out"; \
	done

# `go work sync` is the half of tidy that writes across modules: it raises
# each module's requires to the version the workspace's build list selects,
# so the GOWORK=off build — the one tidy-check and install-check run here,
# and the one a future tag of authn, client or proto will carry — resolves
# what `make test` ran against rather than an older minimum that happened to
# satisfy MVS. It changes nothing for a consumer today: an out-of-repo build
# resolves the tag it fetched, so a bump reaches consumers when the next tag
# is cut and not before. That makes this target a check as much as a fix:
# when one module bumps a shared dependency and its siblings keep the old
# version, `make tidy` on a clean tree rewrites the siblings (#262: three
# protobuf requires, after the root and test/trailer moved on). A dirty tree
# after `make tidy` is that finding, not noise — sync-check below is the
# same finding as a gate — but do not commit it as `go work sync` left it.
# Measured on #262's branch: for a require it raises, sync writes the
# dependency's h1: line into go.sum and not the matching /go.mod line, which
# is the state tidy-check above exists to fail on. Re-run `GOWORK=off go mod
# tidy` in each module sync rewrote and commit that, with the bump that
# caused it or on its own, rather than under an unrelated change.
.PHONY: tidy
tidy: ## go mod tidy in every module, then raise each module's requires to the workspace's build list.
	@for mod in $(MODULES); do \
	  echo "==> go mod tidy $$mod"; \
	  (cd $$mod && $(GO) mod tidy) || exit $$?; \
	done
	$(GO) work sync

# The drift #262 fixed by hand, as a gate (#265). tidy-check cannot see it:
# a module that requires protobuf v1.36.11 while the workspace's build list
# selects v1.36.12 is tidy GOWORK=off, and so is the sibling that moved on —
# both states pass, and only `go work sync` knows they disagree. There is no
# `go work sync -diff` (the way there is `go mod tidy -diff`), so this leg
# backs every module's go.mod and go.sum up, lets sync write, diffs, and
# restores whatever the outcome — the tree is the same after a red run as
# before it, and the diff it printed is what `make tidy` would have left
# behind. go.work.sum and go.work are both in the set because sync may write
# either: the sum when a hash is missing, go.work's own go/toolchain lines
# when a module's go directive moves ahead of the workspace's. A red here
# means one module bumped a shared dependency and its siblings did not: run
# `make tidy`, then `GOWORK=off go mod tidy` in each module it rewrote (the
# tidy comment above says why that second pass), and commit the result.
#
# The restore is a copy back, not a `git checkout`: the leg must not touch
# an uncommitted edit in a go.mod, and it must behave the same on a tree
# that is not a checkout at all. It hangs off a trap rather than off the
# diff loop because this is the one leg that WRITES, and two exits skip that
# loop entirely: under GNU make's -e (3.82+, so the Linux runner — not the
# 3.81 Mac this was first measured on) a failing `go work sync` kills the
# recipe where it stands, and a Ctrl-C or the workflow's cancel-in-progress
# does the same at any point. Either would otherwise leave every module
# synced and the backup stranded in $TMPDIR. GOWORK is forced to the
# committed go.work for modules-check's reason, one step louder because this
# leg writes: with a go.local.work exported (go.work's header documents that
# workflow) sync would rewrite ../chameleon/go.mod, which no backup covers.
.PHONY: sync-check
sync-check: ## Fail if `go work sync` would rewrite a module's go.mod/go.sum.
	@tmp=$$(mktemp -d) || exit 1; \
	files="go.work go.work.sum"; \
	for mod in $(MODULES); do \
	  files="$$files $$mod/go.mod $$mod/go.sum"; \
	done; \
	files=$$(printf '%s\n' $$files | sed 's#^\./##'); \
	for f in $$files; do \
	  [ -e "$$f" ] || continue; \
	  mkdir -p "$$tmp/$$(dirname "$$f")" && cp -p "$$f" "$$tmp/$$f" || { rm -rf "$$tmp"; exit 1; }; \
	done; \
	restore() { \
	  [ -d "$$tmp" ] || return 0; \
	  for f in $$files; do \
	    if [ -e "$$tmp/$$f" ]; then cp -p "$$tmp/$$f" "$$f"; \
	    elif [ -e "$$f" ]; then rm -f "$$f"; fi; \
	  done; \
	  rm -rf "$$tmp"; \
	}; \
	trap restore EXIT; \
	trap 'restore; echo "sync-check: interrupted; tree restored."; exit 130' INT TERM; \
	echo "==> go work sync (against a backup, restored after)"; \
	if GOWORK=$(CURDIR)/go.work $(GO) work sync; then status=0; else status=$$?; fi; \
	drift=0; \
	for f in $$files; do \
	  if [ -e "$$tmp/$$f" ]; then \
	    diff -u -L "$$f" -L "$$f (after go work sync)" "$$tmp/$$f" "$$f" || drift=1; \
	  elif [ -e "$$f" ]; then \
	    echo "go work sync created $$f"; drift=1; \
	  fi; \
	done; \
	[ $$status -eq 0 ] || { echo "sync-check: go work sync failed (exit $$status); tree restored."; exit $$status; }; \
	[ $$drift -eq 0 ] || { \
	  echo "sync-check: 'go work sync' would rewrite the files above — one module bumped a"; \
	  echo "shared dependency and its siblings still require the older version (#262, #265)."; \
	  echo "Run 'make tidy', then 'GOWORK=off go mod tidy' in each module it rewrote, and"; \
	  echo "commit go.mod/go.sum. The tree is restored to what it was before this check."; \
	  exit 1; \
	}

.PHONY: verify
verify: ## Verify dependency checksums against the module cache.
	@for mod in $(MODULES); do \
	  echo "==> go mod verify $$mod"; \
	  (cd $$mod && $(GO) mod verify) || exit $$?; \
	done

# What tidy-check cannot see (#232, the check #226 closed without). An
# intra-repo require resolves three different ways: go.work for every
# ordinary build, a replace for the GOWORK=off pass tidy-check runs, and —
# for a consumer outside this repo — the tag, alone. The first two kept
# `make ci` green across all five modules for the whole window in which
# `go get .../client` could not resolve, because client/go.mod required a
# proto version no tag provided.
#
# So the rule: an intra-repo require must name a version a `<module>/vX.Y.Z`
# tag provides (`vX.Y.Z` for the root module). One exemption, derived rather
# than listed: a module with no tag of its own cannot be fetched by any
# consumer, so its requires may name an untagged version — test/trailer's
# `voya v0.0.0` — as long as a replace in the same go.mod is what its own
# workspace-less build resolves by. The day such a module gets its first
# tag, its requires start having to be real, with no edit here. A replace in
# a *tagged* module exempts nothing, because a consumer's build ignores it —
# that blindness is exactly #226. A pseudo-version in a tagged module fails
# too, though a consumer could resolve it: a published module pinned to an
# untagged sibling commit is the same drift wearing a hash — cut the tag.
#
# #232 weighed three shapes for this check; this is the third (assert the
# tags, not the build), and why not the other two is part of the record:
#
#   - A scratch module outside the repo that `go get`s client and builds it
#     proves more — the whole consumer graph — but needs private-module
#     credentials and a *pushed* commit, so it can only run against main
#     after the merge: the window #226 lived through. This needs nothing but
#     origin's tag list, runs on the PR, and catches the same defect before
#     it lands.
#   - "Fail only when the require names something unpublished and not
#     introduced by this PR" solves a case that cannot arise: a PR cannot
#     carry a tag. Tags are cut on main, by the owner, after the content
#     merges — so the tag a require needs always exists before the PR that
#     bumps the require, and this check passes there with no diff-awareness.
#     What it refuses is the other ordering, bump-then-tag, deliberately:
#     that ordering IS #226, a green main no consumer can fetch. It also
#     keeps every commit on main taggable as it stands.
#
# The tag database consulted is origin's, live (`git ls-remote`), never the
# local one (#240). Local tags are only what a fetch last brought over, and
# a local read is wrong in both directions: a partly-fetched set clears the
# any-tag guard below and then false-fails on the tags it is missing though
# origin has them, while a tag cut but never pushed passes though it still
# strands every consumer — so this target and the CI leg could disagree with
# no signal that they had. The cost is that this check needs origin
# reachable; when it is not, it refuses rather than guessing from local
# tags, for the same fail-closed reason as the guard below. Unreachable
# includes "would need you to type something": the listing runs with git's
# terminal prompt and ssh's interactive prompts disabled, so a missing
# credential, a passphrase or an unknown host key becomes that refusal rather
# than a `make ci` stopped at step 4 of 13 waiting on a password nobody is
# there to type. It follows that the checkout's depth and tag set no longer
# decide this check — ci.yml says why its go job still fetches at depth 0.
# The always-remote shape is settled, not provisional: #249 (2026-08-20)
# accepted that one network-dependent check makes the whole `make ci` gate
# unavailable to a session without egress, and rejected an opt-out env var
# that CI does not set — a CI-only variant re-opens the local false-fail
# #240 item 1 forbids, and an opt-out that exists is one someone eventually
# sets, leaving the check green while no longer guarding. #267 (2026-08-21)
# took one more step the same way: client-check derives which modules it
# checks from this listing too, so origin now decides that leg's set as well
# as what it fetches — its comment says why a written list lost.
#
# The refusal below when origin has no module release tag at all is
# load-bearing: against a tagless remote every require is vacuously exempt
# and the check guards nothing, so it fails closed instead.
#
# "Release tag" is the semver shape, not everything under the prefix: this
# repo cuts `salvage/...` tags too, and a stray `v...`-prefixed one matched by
# a bare `v*` glob would silently revoke an untagged module's exemption and
# turn `make ci` red on an unmodified tree.
#
# The tag prefix is the module's DIRECTORY, looked up from $(MODULES) by the
# `module` line in each go.mod — not the require path with the repo root
# trimmed off. Those differ the moment a module reaches /v2: Go tags an
# in-place major bump (`proto/go.mod` declaring `.../proto/v2`) `proto/v2.0.0`
# and a major-subdirectory one (`proto/v2/go.mod`) `proto/v2/v2.0.0`, and only
# the directory tells them apart. A require naming an intra-repo path that is
# no module here at all is reported rather than guessed at; `modules-check`,
# which `ci` runs first, is what keeps $(MODULES) honest.
# The listing require-check and client-check both resolve against, and the
# refusal they share when it cannot be had. Sets $$origintags to origin's tag
# names, one per line, and defines reltags: the release tags under a module
# directory prefix, by the semver shape the comment above describes. $(1) is
# the target the refusal speaks as.
define origin-tags
relsemver='v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)([-+][0-9A-Za-z.-]+)?'; \
origintags=$$(GIT_TERMINAL_PROMPT=0 \
  GIT_SSH_COMMAND="$${GIT_SSH_COMMAND:-ssh} -o BatchMode=yes" \
  git ls-remote --tags --refs origin) || { \
  echo "$(1): cannot list origin's tags — git ls-remote failed, its error is"; \
  echo "above. This check resolves against origin, never the local tag database:"; \
  echo "a tag cut but not pushed must fail here, and a stale local fetch must not"; \
  echo "(#240). With origin unreachable it refuses rather than guesses."; \
  exit 1; \
}; \
origintags=$$(printf '%s\n' "$$origintags" | sed 's|.*refs/tags/||'); \
reltags() { printf '%s\n' "$$origintags" | grep -Ex -- "$$1$$relsemver" || true; }
endef
.PHONY: require-check
require-check: ## Fail if an intra-repo require names a version no tag provides (#232).
	@$(call origin-tags,require-check); \
	modmap=""; \
	for mod in $(MODULES); do \
	  case "$$mod" in .) d=".";; *) d="$$mod/";; esac; \
	  mp=$$(awk '$$1=="module"{print $$2; exit}' "$$mod/go.mod") || exit $$?; \
	  if [ -z "$$mp" ]; then echo "require-check: $$mod/go.mod has no module line"; exit 1; fi; \
	  modmap="$$modmap$$mp $$d"$$'\n'; \
	done; \
	anytag=0; \
	for mod in $(MODULES); do \
	  case "$$mod" in .) pfx="";; *) pfx="$$mod/";; esac; \
	  if [ -n "$$(reltags "$$pfx")" ]; then anytag=1; fi; \
	done; \
	if [ "$$anytag" -eq 0 ]; then \
	  echo "require-check: origin has no module release tags at all, so every require"; \
	  echo "would be exempt and the check green while guarding nothing — refusing to"; \
	  echo "pass. This lists origin's tags live, so a fetch will not change the answer:"; \
	  echo "if tags are expected, check 'git remote -v' points where you think it does."; \
	  exit 1; \
	fi; \
	fail=0; \
	for mod in $(MODULES); do \
	  echo "==> require-check $$mod"; \
	  case "$$mod" in .) selfpfx="";; *) selfpfx="$$mod/";; esac; \
	  lines=$$(awk -v V='^github[.]com/trendvidia/voya(/|$$)' \
	    '/^require \(/{r=1;next} /^replace \(/{p=1;next} /^\)/{r=0;p=0;next} r&&$$1~V{print "REQ",$$1,$$2} p&&$$1~V{print "REP",$$1} $$1=="require"&&$$2~V{print "REQ",$$2,$$3} $$1=="replace"&&$$2~V{print "REP",$$2}' \
	    "$$mod/go.mod") || exit $$?; \
	  reps=" $$(printf '%s\n' "$$lines" | awk '$$1=="REP"{printf "%s ", $$2}')"; \
	  published=$$(reltags "$$selfpfx"); \
	  while read -r kind path ver _; do \
	    [ "$$kind" = REQ ] || continue; \
	    dir=$$(printf '%s' "$$modmap" | awk -v p="$$path" '$$1==p{print $$2; exit}'); \
	    case "$$dir" in \
	      "") echo "$$mod/go.mod requires $$path, which is not a module in this repo."; \
	          fail=1; continue;; \
	      .) tag="$$ver";; \
	      *) tag="$$dir$$ver";; \
	    esac; \
	    if printf '%s\n' "$$origintags" | grep -qFx -- "$$tag"; then continue; fi; \
	    if [ -z "$$published" ]; then \
	      case "$$reps" in *" $$path "*) continue;; esac; \
	    fi; \
	    echo "$$mod/go.mod requires $$path $$ver, and no tag '$$tag' provides it."; \
	    fail=1; \
	  done <<< "$$lines"; \
	done; \
	if [ "$$fail" -ne 0 ]; then \
	  echo "A consumer resolves an intra-repo require from its tag alone — go.work and the"; \
	  echo "replaces are invisible to it (#226, #232). Cut and push the tag on main first,"; \
	  echo "then land the require bump. An untagged version is allowed only in a module"; \
	  echo "with no tag of its own that also replaces the target: no consumer can fetch"; \
	  echo "such a module, and the replace is what its GOWORK=off build resolves by."; \
	  echo "This read origin's tag list live: a tag cut locally counts only once pushed."; \
	  exit 1; \
	fi

# What require-check cannot see either (#237): it proves each required tag
# exists, never what the tag contains. And tidy-check resolves the root
# module's requires for real but `go mod tidy` never compiles — so a sibling
# that moved past its tag leaves both green while
# `go install github.com/trendvidia/voya/cmd/voya@<head>` compiles the daemon
# against stale bindings. This is the leg that compiles the root module the
# way `go install` resolves it: this module from the working tree, the
# siblings from their published tags. Red here while the workspace build is
# green means a sibling changed without a tag, and that is a gate rather than
# a warning deliberately (#237).
#
# What it measures, and the prose around it says so: `go vet ./...`
# type-checks every package of this module that builds for the host platform,
# test files included, so it sees a stale tag exactly when anything compiled
# for this GOOS names something the tag does not have. #237 specified
# `go build`, non-test packages only, and #246 widened it: `go build` compiles
# no _test.go, and the root module reaches authn and client from test files
# alone, so the leg was a proto-only gate while client/v0.1.0 sat 4 files
# behind main. The cost #246 accepted is that a sibling change used only by a
# root-module test now waits for a tag too; the acceptance suite is out of
# this leg's reach since #236, because it is a module of its own whose
# replaces read the working tree — acceptance-check below asks it the same
# question through its own go.mod (#270). Two edges the host pass alone could
# not reach are closed by the legs below (#247). The CROSS_TARGETS loop
# repeats the vet for every supported GOOS/GOARCH, because a file behind a
# build constraint is invisible to the host pass, and `cross` cannot stand
# in: it resolves siblings through the workspace, so it re-measures the
# working tree rather than the tags. And the internal/config test leg parses
# the embedded layers/base.pxf with whatever bindings the build linked — under
# GOWORK=off, the published tag's — because that drift has no compile-visible
# face at all: a schema field regenerated and set in base.pxf, with no Go
# accessor named from the root module yet, builds clean everywhere and fails
# in the installed daemon at startup, parsing its own embedded config. It runs
# the whole package suite rather than a -run filter, because a filter matches
# nothing the day a test is renamed and exits 0 — so an unrelated failure in
# deps_test.go trips this leg before `test` reaches it, and the message printed
# below says so. The loop is `go vet`, not `go build`, and that is the whole
# of #267's second item. The intersection of the first two legs used to escape
# every one of them: a _test.go behind a build constraint the host does not
# satisfy. The host vet pass types test files but only for the runner's GOOS,
# and the loop covered every GOOS with `go build`, which compiles no _test.go
# — so a `//go:build windows` test naming a symbol a sibling tag lacks was
# green on both runners and every cross target, and the root module already
# carries constrained test files (internal/auth/owner_unix_test.go,
# internal/resolverfile/mode_unix_test.go). Closing it meant vetting the whole
# matrix rather than building it. #267 measured that before deciding: on the
# mini, `make install-check` warm went 3.5s -> 2.0s and cold-GOCACHE 44.0s ->
# 51.7s, against CI's 6s (macOS) / 12s (Linux) warm. Warm gets *faster* —
# vet's result caches, while `go build ./...` re-checks staleness every run —
# and ~+7s per runner cold is the price. Adding vet *beside* the build was
# rejected at +12s: `cross` already builds every target, so the second build
# is paid twice, and this leg's question is a type-level one about the tags.
# No build coverage is lost by the swap; `cross` is where the matrix is built.
#
# What still escapes, recorded rather than closed: a test file behind a
# constraint no leg sets. `-race` is the live one — internal/registry's
# race_on_test.go is `//go:build race`, this loop does not pass `-race`, and
# it could not pass it for every target anyway (linux/386 has no race
# detector, and -race wants cgo while this leg runs CGO_ENABLED=0). `make
# test` does compile that file, but through the workspace, so it meets the
# working tree's siblings and never the published tags.
#
# The remedy the failure prints is the ordering the require-check comment
# above and go.mod both state, and it does not fit in one PR: a tag is cut on
# main, by the owner, after the sibling content merges. Land the sibling,
# tag it, then land the require bump together with the root code that needs
# it.
.PHONY: install-check
install-check: ## Fail if the root module does not compile against the published sibling tags.
	@echo "==> GOWORK=off go vet ./... (root module, test files included)"
	@GOWORK=off $(GO) vet ./... || { \
	  echo "install-check: the root module does not type-check against the published"; \
	  echo "sibling tags. If 'make vet' is red too, this is an ordinary compile or vet"; \
	  echo "error and the sibling tags have nothing to do with it."; \
	  echo "If 'make vet' is green, a sibling changed without its tag"; \
	  echo "being cut, and the fix does not fit in one PR: land the sibling"; \
	  echo "change on main, have the tag cut there, then land the require bump"; \
	  echo "in go.mod (its comment says how the graph resolves) together with"; \
	  echo "the root code that needs it."; \
	  exit 1; \
	}
	@for target in $(CROSS_TARGETS); do \
	  echo "==> GOWORK=off GOOS=$${target%/*} GOARCH=$${target#*/} go vet ./... (root module, test files included)"; \
	  GOWORK=off CGO_ENABLED=0 GOOS=$${target%/*} GOARCH=$${target#*/} $(GO) vet ./... || { \
	    echo "install-check: the root module does not type-check against the published"; \
	    echo "sibling tags for $$target. No other leg answers whether that is a stale"; \
	    echo "tag: cross builds this target but compiles no test file and reports no"; \
	    echo "vet diagnostic at all, and compile-check types test files for the host"; \
	    echo "GOOS only — so both can stay green whichever it is. The check that"; \
	    echo "discriminates is the same vet inside the workspace, where the siblings"; \
	    echo "come from the working tree instead of their tags:"; \
	    echo ""; \
	    echo "    CGO_ENABLED=0 GOOS=$${target%/*} GOARCH=$${target#*/} go vet ./..."; \
	    echo ""; \
	    echo "(CGO_ENABLED=0 because this leg sets it, and cross-compiling with cgo on"; \
	    echo "fails in runtime/cgo rather than in your code.) Red there too: an ordinary"; \
	    echo "compile or vet error, and the tags have nothing to do with it. Green"; \
	    echo "there: the file names something a sibling's published tag does not have,"; \
	    echo "and the remedy is the one above — land the sibling change on main, have"; \
	    echo "the tag cut there, then land the require bump with the code that needs"; \
	    echo "it."; \
	    exit 1; \
	  }; \
	done
	@echo "==> GOWORK=off go test ./internal/config (embedded base.pxf vs published bindings)"
	@GOWORK=off $(GO) test -count=1 ./internal/config || { \
	  echo "install-check: internal/config's embedded layers do not load with the"; \
	  echo "published proto tag's bindings. If make test is red on these tests too,"; \
	  echo "this is an ordinary test failure. If it is green, a schema field was"; \
	  echo "set in layers/base.pxf without the proto tag that knows it — installed,"; \
	  echo "the daemon would fail at startup parsing its own embedded config. The"; \
	  echo "remedy is the one above: land the schema change on main, have the tag"; \
	  echo "cut there, then land the require bump with the base.pxf that needs it."; \
	  exit 1; \
	}

# What install-check cannot see, one module over (#230): client is the module
# every fleet service fetches, and its go.mod requires proto by tag while a
# `replace` points the GOWORK=off build at ../proto — so tidy-check, the
# workspace legs and install-check all compile client against the working
# tree's proto, and nothing here ever compiles it the way a consumer resolves
# it. A consumer's build ignores that replace (a replace is local to the go.mod
# it stands in), fetches the tag the require names, and is the first build to
# find that client uses a symbol, or a whole package, that tag does not have:
# `make ci` green, `go get .../client` red with undefined. #226 lived through
# the version-shaped form of this; this is the contents-shaped one.
#
# So this leg compiles each such module through a copy of its own go.mod with
# its intra-repo replaces dropped, GOWORK=off: the published tags are what
# resolve, exactly as on the consumer's machine. `go vet ./...` rather than
# `go build`, so the module's test files are typed too — a consumer does not
# compile those, but the remedy is the same tag either way and the narrower
# gate is #246's lesson. The copy is built `-mod=mod`, because its go.sum has
# no entries for a sibling nobody fetched before, and -mod=mod is also the one
# way this could lie: asked for a package the required tag lacks but a newer
# tag has, go quietly moves the require in the copy and compiles green. The
# loop therefore diffs the copy's intra-repo requires against the original
# after the build, and a moved require is the failure it reports. A package no
# tag has at all fails in go's own words ("does not contain package"); a
# missing symbol fails as undefined.
#
# It is also a leg that fetches a sibling's tag — acceptance-check below is
# the other: every other build here resolves siblings through a replace or the
# workspace, so on a cold module cache this leg wants the proxy, and the
# failure it prints says so rather than blaming a stale tag.
#
# Which modules, derived rather than listed (#267, widened #275): every module
# with a release tag on origin AND an intra-repo require in its go.mod,
# replace or not — plus, so the inert-replace refusal below keeps firing, a
# tagged module that replaces a sibling and requires nothing from it, which is
# red before it reaches any compile. An untagged module stays out because
# nobody can fetch it, so its requires resolve from the working tree for
# everyone (require-check's comment); a tagged module with neither a sibling
# require nor a sibling replace has nothing a tag could contradict. #267 keyed
# the second half on the replace instead, on the reading that a tagged module
# requiring a sibling *without* one already resolved it from the tag in every
# build here. Measured, it does not: under go.work a `use`d module resolves
# its siblings from the working tree whether or not it has a replace — `use`
# beats a bare require, and `go list -m` on the root module answers
# `replaced=no dir=<worktree>/proto` — so the workspace legs never see the
# drift, `go mod tidy` resolves the tag but never type-checks, and
# require-check proves the tag exists, not what it contains.
# The replace was never what hid the require; the tag plus the require is.
# Decided 2026-08-21 with the cost named: the root module requires all three
# siblings with no replace, so it enters this set the day it is tagged, and
# install-check already compiles it GOWORK=off against those tags. It is
# checked twice then, on purpose — a rule keyed on "another leg happens to
# cover this one" goes stale silently when that leg changes.
#
# What it still does not cover: a sibling imported with no require at all
# (tidy-check's — `go mod tidy` GOWORK=off adds the require and the diff is
# red); an untagged module, whatever it requires, because nobody can fetch one
# — test/acceptance is that case and acceptance-check below asks it the same
# question through its own go.mod (#270); and anything outside the module's
# own tree — it compiles the working tree of each module against the sibling
# tags its go.mod names, which is what the module's *next* tag ships, not what
# an already-cut tag contains.
#
# Today the set is client alone, and the target keeps that name; the day authn
# or proto requires a sibling by tag, replace or not, it is checked with
# nothing to remember. #264 shipped this as a literal list with "add the next
# one here" in its comment — the hand-maintained set the file header warns
# about — and #267 chose derivation over a drift check beside the list. The
# tag half is origin's live listing, the same one require-check reads, so the
# network dependency #249 accepted now decides which modules this checks as
# well as what it fetches; the require half is a local read of each go.mod,
# by the same module-path match require-check uses, and the two agree about
# paths because both take them verbatim from go.mod. Two refusals keep the
# derivation honest. An empty derived set is red, not green: a broken require
# parse or tag glob would otherwise empty it silently and leave `make ci`
# green while this guards nothing — the day it is legitimately empty, this
# target is removed on purpose. And a replace with no require behind it is red
# too — the second half of the derivation admits such a module for this alone:
# there is nothing for the copy to resolve from a tag with it dropped,
# `go mod tidy` never prunes one, and the module compiles here against a
# sibling a consumer never sees, so the message says to drop it.
#
# `intrareps` yields each replace's whole left-hand side, version included,
# because `replace <path> v0.1.0 => ../path` is legal and
# `go mod edit -dropreplace=<path>` does not match it — dropping needs
# `<path>@v0.1.0`. Passing the left-hand side back verbatim covers both forms;
# without it the `left` guard fires on a valid go.mod, which fails closed but
# blames the parse rather than the version. `intrareqs` yields each intra-repo
# require as `<path>@<version>`, from a block or a one-line require, indirect
# ones included — an indirect sibling is fetched by a consumer the same as a
# direct one. The -mod=mod drift diff reads the scratch copy through it as
# well, rather than grepping for the module path: a one-line require does not
# start its line with that path, so a grep would find nothing before and
# nothing after and pass a moved one-line require green.
.PHONY: client-check
client-check: ## Fail if a tagged module that requires or replaces a sibling does not compile against the sibling tags its go.mod requires (#230, #267, #275).
	@$(call origin-tags,client-check); \
	V='^github[.]com/trendvidia/voya(/|$$)'; \
	intrareps() { awk -v V="$$V" '/^replace \(/{p=1;next} /^\)/{p=0;next} p&&$$1~V{print $$1 ($$2=="=>" ? "" : "@" $$2)} $$1=="replace"&&$$2~V{print $$2 ($$3=="=>" ? "" : "@" $$3)}' "$$1"; }; \
	intrareqs() { awk -v V="$$V" '/^require \(/{r=1;next} /^\)/{r=0;next} r&&$$1~V{print $$1 "@" $$2} $$1=="require"&&$$2~V{print $$2 "@" $$3}' "$$1"; }; \
	checked=""; \
	for mod in $(MODULES); do \
	  case "$$mod" in .) pfx="";; *) pfx="$$mod/";; esac; \
	  [ -n "$$(reltags "$$pfx")" ] || continue; \
	  reqs=$$(intrareqs "$$mod/go.mod") || exit $$?; \
	  reps=$$(intrareps "$$mod/go.mod") || exit $$?; \
	  [ -n "$$reqs$$reps" ] || continue; \
	  checked="$$checked $$mod"; \
	done; \
	if [ -z "$$checked" ]; then \
	  echo "client-check: no module is both tagged on origin and requires (or replaces) a"; \
	  echo "sibling in its go.mod, so the set this check derives is empty — refusing to pass"; \
	  echo "rather than guard nothing. Today that set is client (tagged, requires proto):"; \
	  echo "if client shed both its proto require and its proto replace on purpose, or its"; \
	  echo "tag, remove this target on purpose too — dropping the require alone keeps client"; \
	  echo "in the set and is refused above as an inert replace. Otherwise the derivation"; \
	  echo "broke, in the require or replace parse above or in origin's tag listing."; \
	  exit 1; \
	fi; \
	echo "==> client-check derives:$$checked"; \
	tmp=$$(mktemp -d) || exit $$?; \
	trap 'rm -rf "$$tmp"' EXIT; \
	for mod in $$checked; do \
	  d="$$tmp/$$mod"; mkdir -p "$$d" || exit $$?; \
	  cp "$$mod/go.mod" "$$d/" || exit $$?; \
	  if [ -f "$$mod/go.sum" ]; then cp "$$mod/go.sum" "$$d/" || exit $$?; fi; \
	  mp=$$(awk '$$1=="module"{print $$2; exit}' "$$mod/go.mod") || exit $$?; \
	  want=$$(intrareqs "$$mod/go.mod") || exit $$?; \
	  want=$$(echo $$want); \
	  for spec in $$(intrareps "$$mod/go.mod"); do \
	    path=$${spec%%@*}; \
	    case " $$want " in *" $$path@"*) ;; *) \
	      echo "client-check: $$mod/go.mod replaces $$path but requires nothing from it, so"; \
	      echo "the replace is inert — nothing resolves from a tag with it dropped, and go mod"; \
	      echo "tidy never prunes one. Drop the replace, or add the require it stands for."; \
	      exit 1;; \
	    esac; \
	    $(GO) mod edit -dropreplace="$$spec" "$$d/go.mod" || exit $$?; \
	  done; \
	  left=$$(intrareps "$$d/go.mod") || exit $$?; \
	  [ -z "$$left" ] || { echo "client-check: could not drop $$mod/go.mod's intra-repo replace from the scratch copy: $$(echo $$left)"; exit 1; }; \
	  before=$$(intrareqs "$$d/go.mod") || exit $$?; \
	  echo "==> GOWORK=off go vet -modfile=<$$mod/go.mod minus its replaces> ./... ($$mod vs $$want)"; \
	  (cd "$$mod" && GOWORK=off $(GO) vet -modfile="$$d/go.mod" -mod=mod ./...) || { \
	    echo "client-check: $$mod does not compile against $$want, the version(s) its go.mod"; \
	    echo "requires. If the workspace build is red too, this is an ordinary compile error."; \
	    echo "If the message above is about fetching rather than compiling (module lookup"; \
	    echo "disabled, no route to the proxy, no credential), nothing is stale: this leg"; \
	    echo "downloads a sibling's tag (acceptance-check is the other that does) — every"; \
	    echo "other build in the repo reads the sibling through a replace or the workspace"; \
	    echo "— so on a cold module cache it needs the network, as require-check does"; \
	    echo "(CLAUDE.md names them)."; \
	    echo "If the workspace build is green, $$mod uses something $$want does not have:"; \
	    echo "every build in this repo reads the sibling from the working tree, through"; \
	    echo "$$mod/go.mod's replace or the workspace's use, but a consumer's \`go get $$mp\`"; \
	    echo "sees neither and fetches the tag, so it fails where this just did. The fix"; \
	    echo "does not fit in one PR: land the sibling change on main, have its tag cut"; \
	    echo "there, then bump $$mod/go.mod's require together with the $$mod code that"; \
	    echo "needs it (#230)."; \
	    exit 1; \
	  }; \
	  after=$$(intrareqs "$$d/go.mod") || exit $$?; \
	  if [ "$$before" != "$$after" ]; then \
	    echo "client-check: compiling $$mod against $$want moved an intra-repo require:"; \
	    echo "  before: $$(echo $$before)"; \
	    echo "  after:  $$(echo $$after)"; \
	    echo "$$mod imports a sibling package the required tag does not contain but a newer"; \
	    echo "tag does, and go resolved it by upgrading the scratch copy. A consumer fetches"; \
	    echo "the version $$mod/go.mod names, not the newest — bump the require to the tag"; \
	    echo "that has the package, with the $$mod code that needs it (#230)."; \
	    exit 1; \
	  fi; \
	done

# What client-check cannot see either, one module over again (#270):
# test/acceptance is the largest consumer of authn and client in the
# repository, its go.mod requires all four siblings by version — and then
# replaces every one of them, so the suite compiles against the working tree
# and nothing else. That is right for what the suite is for (#236): it
# exercises the checkout, and a harness resolving a tag would attest to code
# nobody is running. It also means no leg ever compiles the suite against the
# versions its own go.mod names: the module is untagged, so require-check
# exempts it, and client-check's set — tagged on origin AND requiring or
# replacing a sibling since #275 — excludes it on the tag half, which this
# module must never have; install-check types the root module only. #246's
# failure was visible for exactly as long as ratelimit_test.go sat in the
# root module — #236 moved it behind these replaces and the same divergence
# went green unchanged.
#
# So this leg compiles the suite the way client-check compiles client:
# through a copy of its go.mod, GOWORK=off, `go vet ./...` so the test files
# — which is all the suite is — are typed, with the sibling replaces dropped
# so authn, client and proto resolve from the tags the requires name. The
# root module's replace stays: the root is unpublished (v0.0.0, no tag, and
# `go install` builds it from source), so there is no tag it could resolve
# from and the replace is the only thing that can. What this proves is
# narrower than a consumer's fetch — nobody consumes this module, and it must
# never be tagged — and it is not about what anyone fetches at all: it is the
# suite's own go.mod telling the truth about the sibling versions its code
# needs, so a bump there is a measured statement rather than a guess.
#
# `-mod=mod` and the before/after diff are client-check's, for its reasons:
# the copy's go.sum has no entries for siblings nobody fetched before, and
# -mod=mod would quietly raise a require to a newer tag that has a package
# the named one lacks. The kept root replace adds one more way a require can
# move: MVS reads the working tree's root go.mod through it, and a sibling
# the root requires at a higher version than this go.mod names is raised in
# the copy — tidy-check reports that first, as an untidy go.mod, and this leg
# reports it too rather than compile against a version nobody wrote down.
# Like client-check it fetches a tag on a cold module cache, and its failure
# tells a fetch apart from a stale tag for the same reason.
#
# A sibling target rather than client-check widened, deliberately, and it
# stayed one when #275 widened that derivation: client-check asks what a
# consumer fetches, so its set begins with a tag on origin, and test/acceptance
# is the other half — untagged, never fetched, replacing everything including
# the root, whose replace has to stay exactly where client-check drops every
# one. Folding it in would mean an exception inside a derivation; a target of
# its own states the different question it asks. The module and the replace it
# keeps are named here because both are written down as deliberate in
# test/acceptance/go.mod, not derived.
.PHONY: acceptance-check
acceptance-check: ## Fail if test/acceptance does not compile against the sibling tags its go.mod requires (#270).
	@mod=test/acceptance; root=github.com/trendvidia/voya; \
	V='^github[.]com/trendvidia/voya(/|$$)'; \
	intrareps() { awk -v V="$$V" '/^replace \(/{p=1;next} /^\)/{p=0;next} p&&$$1~V{print $$1 ($$2=="=>" ? "" : "@" $$2)} $$1=="replace"&&$$2~V{print $$2 ($$3=="=>" ? "" : "@" $$3)}' "$$1"; }; \
	tmp=$$(mktemp -d) || exit $$?; \
	trap 'rm -rf "$$tmp"' EXIT; \
	cp "$$mod/go.mod" "$$mod/go.sum" "$$tmp/" || exit $$?; \
	want=""; \
	for spec in $$(intrareps "$$mod/go.mod"); do \
	  path=$${spec%%@*}; \
	  [ "$$path" != "$$root" ] || continue; \
	  ver=$$(awk -v p="$$path" '/^require \(/{r=1;next} /^\)/{r=0;next} r&&$$1==p{print $$2; exit} $$1=="require"&&$$2==p{print $$3; exit}' "$$mod/go.mod") || exit $$?; \
	  case "$$ver" in \
	    "") echo "acceptance-check: $$mod/go.mod replaces $$path but requires nothing from it, so"; \
	        echo "the replace is inert — nothing resolves from a tag with it dropped, and go mod"; \
	        echo "tidy never prunes one. Drop the replace, or add the require it stands for."; \
	        exit 1;; \
	    v0.0.0) echo "acceptance-check: $$mod/go.mod requires $$path at v0.0.0, which no tag"; \
	        echo "provides, so with the replace dropped nothing resolves. Name the tag the suite"; \
	        echo "needs; require-check says which exist."; \
	        exit 1;; \
	  esac; \
	  want="$$want$$path@$$ver "; \
	  $(GO) mod edit -dropreplace="$$spec" "$$tmp/go.mod" || exit $$?; \
	done; \
	want=$$(echo $$want); \
	[ -n "$$want" ] || { \
	  echo "acceptance-check: $$mod/go.mod replaces no sibling but the root, so there is nothing"; \
	  echo "to compile against a tag — refusing to pass rather than guard nothing. If the"; \
	  echo "replaces went on purpose, remove this target on purpose too; otherwise the"; \
	  echo "replace parse above broke."; \
	  exit 1; \
	}; \
	left=$$(intrareps "$$tmp/go.mod" || true); \
	[ "$$left" = "$$root" ] || { \
	  echo "acceptance-check: the scratch copy of $$mod/go.mod should replace $$root and"; \
	  echo "nothing else, and it replaces: $$(echo $${left:-nothing}). The root's replace is the"; \
	  echo "one that must stay (nothing publishes it); the siblings' are the ones to drop."; \
	  exit 1; \
	}; \
	before=$$(grep -E '^\s*github\.com/trendvidia/voya(/| )' "$$tmp/go.mod" | grep -v '=>' || true); \
	echo "==> GOWORK=off go vet -modfile=<$$mod/go.mod minus its sibling replaces> ./... ($$mod vs $$want)"; \
	(cd "$$mod" && GOWORK=off $(GO) vet -modfile="$$tmp/go.mod" -mod=mod ./...) || { \
	  echo "acceptance-check: $$mod does not compile against $$want, the versions its"; \
	  echo "go.mod requires. If 'make vet' is red too, this is an ordinary compile or vet error."; \
	  echo "If the message above is about fetching rather than compiling (module lookup"; \
	  echo "disabled, no route to the proxy, no credential), nothing is stale: this leg"; \
	  echo "downloads the sibling tags — every other build of the suite reads them through"; \
	  echo "its replaces or the workspace — so on a cold module cache it needs the network,"; \
	  echo "as client-check and require-check do (CLAUDE.md names them)."; \
	  echo "If 'make vet' is green, the suite uses something those tags do not have: its"; \
	  echo "replaces read the working tree, so nothing else noticed (#270). Either the suite"; \
	  echo "needs a sibling change that is not tagged yet — land that change on main, have"; \
	  echo "its tag cut there, then bump $$mod/go.mod's require with the test that needs"; \
	  echo "it — or the tag exists and the require is simply behind it: bump it."; \
	  exit 1; \
	}; \
	after=$$(grep -E '^\s*github\.com/trendvidia/voya(/| )' "$$tmp/go.mod" | grep -v '=>' || true); \
	if [ "$$before" != "$$after" ]; then \
	  echo "acceptance-check: compiling $$mod against $$want moved an intra-repo require:"; \
	  echo "  before: $$(echo $$before)"; \
	  echo "  after:  $$(echo $$after)"; \
	  echo "Either the suite imports a sibling package the named tag does not contain but a"; \
	  echo "newer one does, or the root module (read from the working tree through its kept"; \
	  echo "replace) requires a sibling past what $$mod/go.mod names; go resolved both by"; \
	  echo "raising the scratch copy. Bump the require to the version go chose, so the"; \
	  echo "suite's go.mod names what it actually compiles against (#270)."; \
	  exit 1; \
	fi

# --- Cross-compilation --------------------------------------------------------

# ADR 001 claims voya works anywhere Go cross-compiles, Windows included. This
# is the check that keeps the claim true as the code grows.
#
# linux/386 is here as the one 32-bit target, not as a platform anyone runs
# voya on: with every entry 64-bit, an int-width mistake compiles everywhere
# the gate looks — PR #179 shipped `len(body) > math.MaxUint32`, which does not
# compile at all where int is 32 bits, and cross was green (#181).
CROSS_TARGETS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 linux/386 windows/amd64

# CGO_ENABLED=0 is not incidental. Once cmd/voya reached the daemon it began
# linking net, and net under cgo drags in runtime/cgo — which for a
# darwin -> linux build needs a C cross-toolchain nobody has, so a developer
# with CGO_ENABLED=1 in their environment saw this target fail on code that
# is perfectly portable. It is also what voya ships as — which is why `build`
# and `install` pin it too, and what the comment above them argues from: voya
# never resolves *.internal itself (it dials loopback addresses), so the
# pure-Go resolver costs it nothing, and ADR 002's cgo resolver trap is about
# the *services*, not about voya.
#
# `./...` in every module, not `./cmd/voya`: the binary is only its own
# transitive closure, and what falls outside it is not incidental. `client`
# is what every consuming service imports and links, so it ships on whatever
# platform that service targets; `internal/tools/changelog` and
# `internal/changelog` predate internal/testproc and have never been
# reachable from cmd/voya either. None of them was compile-checked away from
# the host at all, so a unix-only syscall added to any one of them landed
# green here and surfaced on whoever next built elsewhere (#76).
#
# One boundary stays open on purpose, recorded rather than closed. Test
# *files*: `go build` does not compile them, and whether the suites
# themselves cross-compile is the separate, costlier gate #76 weighs and
# declines. Since #267 the root module's are typed per target after all, by
# install-check's GOOS loop — but that leg answers a different question, with
# the siblings resolved from their published tags rather than the workspace,
# and it is the root module alone. The other five modules' test files still
# compile for the host GOOS only, in compile-check.
#
# The other boundary #83 recorded — build constraints — is what the second
# phase below closes (#86). `./...` skips a package whose files are all
# excluded for the target — silently, exit 0, no output — so moving the unix
# half of a package into a `_unix.go` with no Windows counterpart still
# passed the build loop, which is the idiomatic form of the regression
# above. The phase compares each target's package set against the union of
# every target's and the host's, and a package missing from any target is an
# error: #86 decided strict over an allowlist, because voya's precedent is
# stub-not-vanish (internal/dnscheck's ProbeSystem is a darwin measurement
# plus a fallback that reports non-darwin as unmeasured, not a package that
# disappears off darwin), and relaxing to a CROSS_EXEMPT list the day a
# package truly has no meaningful stub is the cheap, argued-in-the-open
# direction.
#
# Two details carry the check. The set is packages with non-empty GoFiles,
# not raw `go list ./...`: unconstrained _test.go files keep a package
# *listed* after constraints exclude everything `go build` would compile —
# test/trailer is exactly that shape — so a raw list diff stays clean on the
# very mutation #86 opens with. And the baseline is a union, not the host's
# list alone: a package born windows-only never appears on a darwin or linux
# host, so a host baseline could not see it vanish from the other targets.
.PHONY: cross
cross: ## Compile every module for every supported GOOS/GOARCH; fail if a package vanishes under one.
	@for target in $(CROSS_TARGETS); do \
	  for mod in $(MODULES); do \
	    echo "==> go build $$target $$mod"; \
	    (cd $$mod && CGO_ENABLED=0 GOOS=$${target%/*} GOARCH=$${target#*/} $(GO) build -o /dev/null ./...) || exit $$?; \
	  done; \
	done
	@tmp=$$(mktemp -d) || exit $$?; \
	trap 'rm -rf "$$tmp"' EXIT; \
	fail=0; m=0; \
	for mod in $(MODULES); do \
	  m=$$((m+1)); \
	  echo "==> go list per-target packages $$mod"; \
	  for target in host $(CROSS_TARGETS); do \
	    if [ "$$target" = host ]; then os=; arch=; else os=$${target%/*}; arch=$${target#*/}; fi; \
	    pkgs=$$(cd $$mod && CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) list -f '{{if .GoFiles}}{{.ImportPath}}{{end}}' ./...) || exit $$?; \
	    printf '%s\n' "$$pkgs" | sed '/^$$/d' | sort > "$$tmp/$$m-$${target/\//-}" || exit $$?; \
	  done; \
	  sort -u "$$tmp/$$m-"* > "$$tmp/union" || exit $$?; \
	  for target in $(CROSS_TARGETS); do \
	    missing=$$(comm -23 "$$tmp/union" "$$tmp/$$m-$${target/\//-}") || exit $$?; \
	    if [ -n "$$missing" ]; then \
	      echo "cross: build constraints exclude every non-test Go file for $$target in:"; \
	      printf '      %s\n' $$missing; \
	      fail=1; \
	    fi; \
	  done; \
	done; \
	if [ "$$fail" -ne 0 ]; then \
	  echo "cross: a package must not vanish under a target (#86). Say so instead: add a"; \
	  echo "file carrying the complementary build constraint, or — where nothing in the"; \
	  echo "package actually fails to compile elsewhere — keep it unconstrained and branch"; \
	  echo "on runtime.GOOS, the way internal/dnscheck's ProbeSystem reports non-darwin as"; \
	  echo "unmeasured. Or argue in #86 for a CROSS_EXEMPT allowlist."; \
	  exit 1; \
	fi

# --- Tooling ------------------------------------------------------------------

.PHONY: tools
tools: ## Install dev tools: golangci-lint, at the pinned version (see footer).
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

# --- Changelog ----------------------------------------------------------------

# Release notes are one file per change under changelog.d/, assembled at
# release time. See changelog.d/README.md for the format and for why a single
# CHANGELOG.md does not survive a stacked-PR workflow.

.PHONY: changelog-check
changelog-check: ## Validate the changelog.d fragments.
	$(GO) run ./internal/tools/changelog check

.PHONY: changelog-preview
changelog-preview: ## Render the next release section without writing it.
	$(GO) run ./internal/tools/changelog preview

.PHONY: changelog
changelog: ## Assemble changelog.d into CHANGELOG.md; needs VERSION set.
	@if [ -z "$(VERSION)" ]; then \
	  echo "changelog: set VERSION, e.g. make changelog VERSION=v0.1.0"; \
	  exit 1; \
	fi
	$(GO) run ./internal/tools/changelog release -version '$(VERSION)'

# --- Meta ---------------------------------------------------------------------

# `build` and `compile-check` run before the module-graph legs so an ordinary
# compile error surfaces as itself rather than as install-check's sibling-tag
# failure (#247, #255, #271). `build` reaches as far as cmd/voya's import
# closure; `compile-check` reaches the rest of every module's packages, test
# files included, for the host GOOS, and runs the link stage the way #255's
# leg did (#276) — over the root module's three `main` packages, the only
# ones any module has. A root-module file behind a build constraint the host
# does not satisfy — test file or not, since #267 made install-check's GOOS
# loop vet rather than build — still lands on install-check first, which is
# why every leg there prints the workspace-green test rather than asserting a
# stale tag. A sibling module's such file lands on `cross`, which builds every
# module for every target, unless it is a _test.go: those are typed for the
# host GOOS alone, in compile-check.
# `cross` stays last because it is the expensive leg.
#
# `recipe-check` joins the three static legs at the front (#286). It reads this
# file and never the tree, so it costs nothing and it is honest to fail before
# anything is compiled: what it reports is that a later leg is no longer asking
# the question its comment claims, which is worth knowing before that leg runs
# green and is believed.
.PHONY: ci
ci: modules-check ci-listing-check workflow-check recipe-check build compile-check tidy-check sync-check require-check install-check client-check acceptance-check changelog-check proto-lint proto-breaking proto-check vet test lint cross ## Reproduce the CI matrix locally.

.PHONY: check
check: modules-check fmt-check vet test ## Fast pre-commit suite (fmt, vet, test).

.PHONY: clean
clean: ## Remove built binaries and coverage artifacts.
	rm -rf $(BIN_DIR) $(COVER_PROFILE)
