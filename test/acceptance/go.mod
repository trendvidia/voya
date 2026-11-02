// The acceptance suite is a module of its own for the same reason
// test/trailer is (#236, decided 2026-08-20 over keeping it in the root): its
// REST fixture requires connectrpc.com/vanguard, and vanguard's own tests
// pull grpc-go into whatever module carries it. In the root that put a
// transcoder on the manifest a person `go install`s, and falsified go.work's
// written claim that grpc-go is absent from the root module's requirements.
// Here it weighs on nothing: nobody consumes this module.
//
// The require list runs the OPPOSITE way from client's and authn's, and that
// is correct rather than an exception to argue around: client/deps_test.go and
// authn/deps_test.go forbid importing the root module because a fleet service
// inherits whatever those two require. An acceptance suite that starts the
// real daemon is supposed to depend on it. Nothing enforces the arrow here and
// nothing should.
module github.com/trendvidia/voya/test/acceptance

go 1.26.10

require (
	connectrpc.com/connect v1.20.0
	connectrpc.com/grpchealth v1.5.0
	connectrpc.com/grpcreflect v1.3.0
	connectrpc.com/vanguard v0.4.0
	github.com/lestrrat-go/jwx/v2 v2.1.7
	github.com/trendvidia/voya v0.0.0
	github.com/trendvidia/voya/authn v0.1.1
	github.com/trendvidia/voya/client v0.2.1
	github.com/trendvidia/voya/proto v0.2.1
	google.golang.org/genproto/googleapis/api v0.0.0-20260819154853-08b0e4226688
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
	github.com/trendvidia/chameleon v0.27.1 // indirect
	github.com/trendvidia/protos v0.1.4 // indirect
	github.com/trendvidia/protowire-go v1.13.0 // indirect
	go.opentelemetry.io/otel v1.45.0 // indirect
	go.opentelemetry.io/otel/metric v1.45.0 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260818201246-1b0934165a6f // indirect
	google.golang.org/grpc v1.83.0 // indirect
)

// Intra-repo, for the GOWORK=off case `make tidy-check` runs — the argument
// test/trailer/go.mod writes out: ADR 001 rejects `replace` for the sibling
// repos, which may not be on disk; these targets ship in this repository at a
// fixed relative path, so they always resolve and can never point at a stale
// checkout. go.work already makes every ordinary build read the working tree;
// the replaces are what keeps the workspace-less build reading the same code,
// instead of the published tags — this suite exists to exercise the checkout,
// and a harness resolving a tag silently attests to code nobody is running.
//
// All four, not only the root: the root requires authn, client and proto by
// tag, and a replace in a dependency's go.mod is ignored in the consumer's
// build, so without these three the suite would compile the working tree's
// daemon against a published client. Safe for the reason require-check
// already encodes — a module with no tag of its own cannot be fetched by any
// consumer, so the v0.0.0 the root resolves to here strands nobody. Do not
// tag this module.
//
// What the replaces hide is the three sibling requires going stale: every
// build of this suite reads ../../client, so a test naming something
// client/v0.1.0 never had was green here while that was the version written
// above (#246, #270). `make acceptance-check` is the leg that looks: it
// type-checks the suite through a copy of this file with the three sibling
// replaces dropped — the root's stays, nothing publishes it — so authn, client
// and proto resolve from the tags named above. Red there with the workspace
// green means a require here is behind what the suite's code needs.
replace (
	github.com/trendvidia/voya => ../..
	github.com/trendvidia/voya/authn => ../../authn
	github.com/trendvidia/voya/client => ../../client
	github.com/trendvidia/voya/proto => ../../proto
)
