// The examples are a module of their own for the reason test/acceptance is
// (#236, #293): nobody installs them, and their dependencies have no business
// on the manifest of the module people do install. Keeping them here also
// keeps them compiled — `make ci` walks MODULES, so these programs are built
// for every cross target like everything else, and an example that stopped
// compiling would fail the gate rather than greet the next reader.
//
// The requires below name published tags and the replaces point at the
// checkout, which is the same arrangement test/acceptance uses: the workspace
// resolves the siblings for every ordinary build here, and the tags are what
// the requires would mean to anyone outside this repository. Nothing consumes
// this module, so nothing depends on either being true — but a copy of a
// service's go.mod is one of the things a reader takes from an example, and
// one that named v0.0.0 would teach the wrong thing.
module github.com/trendvidia/voya/examples

go 1.26.10

require (
	connectrpc.com/connect v1.20.0
	github.com/trendvidia/voya/client v0.2.1
	google.golang.org/protobuf v1.36.12
)

require (
	connectrpc.com/grpchealth v1.5.0
	connectrpc.com/grpcreflect v1.3.0 // indirect
	github.com/trendvidia/voya/proto v0.2.1 // indirect
)

replace (
	github.com/trendvidia/voya/client => ../client
	github.com/trendvidia/voya/proto => ../proto
)
