module github.com/trendvidia/voya

go 1.26.10

require (
	connectrpc.com/connect v1.20.0
	connectrpc.com/grpchealth v1.5.0
	connectrpc.com/grpcreflect v1.3.0
	github.com/lestrrat-go/jwx/v2 v2.1.7
	github.com/trendvidia/chameleon v0.27.1
	github.com/trendvidia/protowire-go v1.13.0
	github.com/trendvidia/voya/authn v0.2.0
	github.com/trendvidia/voya/client v0.3.0
	github.com/trendvidia/voya/proto v0.3.0
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/awnumar/memcall v0.2.0 // indirect
	github.com/awnumar/memguard v0.22.5 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/decred/dcrd/dcrec/secp256k1/v4 v4.4.1 // indirect
	github.com/goccy/go-json v0.10.6 // indirect
	github.com/lestrrat-go/blackmagic v1.0.4 // indirect
	github.com/lestrrat-go/httpcc v1.0.1 // indirect
	github.com/lestrrat-go/httprc v1.0.6 // indirect
	github.com/lestrrat-go/iter v1.0.2 // indirect
	github.com/lestrrat-go/option v1.0.1 // indirect
	github.com/segmentio/asm v1.2.1 // indirect
	github.com/trendvidia/protos v0.1.4 // indirect
	go.opentelemetry.io/otel v1.45.0 // indirect
	go.opentelemetry.io/otel/metric v1.45.0 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260819154853-08b0e4226688 // indirect
)

require (
	golang.org/x/net v0.60.0
	golang.org/x/text v0.43.0 // indirect
)

// No `replace` here, deliberately, and this module is the one place in the
// repository where that is load-bearing: `go install pkg@version` refuses a
// module whose go.mod carries replace or exclude directives, and ADR 001 says
// voya is distributed that way (#231). test/trailer and client keep theirs —
// nobody installs them — so the rule is about this module, not the repo.
//
// What resolves the three siblings instead:
//
//   - Ordinary builds and `make test`: go.work, which `use`s all six modules,
//     so the working tree is what compiles.
//   - `GOWORK=off`, which `make tidy-check` runs: the *published* versions
//     above. That is the cost this buys and it is not free — tidy-check and
//     the workspace stop reading the same code the moment a sibling changes
//     without a tag. proto/v0.2.0 exists because exactly that had already
//     happened: v0.1.0 predates the rate limiter's schema (#229).
//
// So a change to authn, client or proto that the daemon depends on needs its
// tag cut and the require above bumped, in that order, before this module can
// see it outside the workspace. `make install-check` gates on the part of
// that divergence a compiler can see (#237, #246): it type-checks this
// module GOWORK=off, test files included, so a stale sibling tag that
// anything here names is a red build rather than a wrong installed binary.
// The target's comment in the Makefile says what that leaves out.
//
// The divergence is not always the quiet kind. Import a sibling *package* the
// published version does not have — a new generated binding directory, say —
// and the workspace build is green while tidy-check is red with "module
// github.com/trendvidia/voya/proto@latest found (v0.2.0), but does not contain
// package ...", which nothing in that PR can fix: the tag can only be cut once
// the sibling's content is on main. Land the sibling, tag it, then land the
// code that imports it.
