// This require list is the point of the module (#36): every service in the
// fleet imports client, so what stands here is what they all inherit —
// connect-go, the generated schemas, protobuf, and nothing of the daemon's.
// The root module is absent by design and must stay absent: a require of it
// would put chameleon and the rest of the daemon's graph on every consumer's.
// client/deps_test.go enforces that at the import, test files included.
module github.com/trendvidia/voya/client

go 1.26.10

require (
	connectrpc.com/connect v1.20.0
	connectrpc.com/grpcreflect v1.3.0
	github.com/trendvidia/voya/proto v0.3.0
	google.golang.org/protobuf v1.36.12
)

// Intra-repo, for the GOWORK=off case `make tidy-check` runs — the same
// argument test/trailer/go.mod writes out: ADR 001 rejects `replace` for the
// sibling repos, and proto ships in this repository at a fixed relative path,
// so this one always resolves here and can never point at a stale checkout.
//
// Only here, and it changes nothing for a consumer: a replace in a
// dependency's go.mod is ignored in the consumer's build, so what an
// out-of-repo `go get` of this module fetches is the version required above.
// That version is a real tag as of #226, and proto/v0.2.1 since #246 — cut at
// df38a6d2 on main — so the fetch resolves rather than failing on a v0.0.0
// nothing publishes. Bumping the require is therefore the whole of what a
// consumer sees, and it reaches one only from the next client tag on, because
// a tag carries the go.mod of the commit it names: client/v0.2.0 still
// requires proto/v0.1.0, and client/v0.2.1 — the tag the root requires as of
// #280 — is the first cut that carries this one. The replace stays for the
// GOWORK=off build.
//
// It also hides the require going stale: every module here reads ../proto, so
// no ordinary build fetches the tag, and a proto change client needs would be
// green here and undefined in a consumer. `make client-check` is the leg that
// looks (#230): it compiles this module GOWORK=off through a copy of this file
// with the replace below dropped, so proto resolves from the tag the require
// names, exactly as on a consumer's machine. Red there with the workspace
// green means the ordering is tag first — land the proto change, have the tag
// cut on main, then bump the require together with the client code that
// needs it.
replace github.com/trendvidia/voya/proto => ../proto
