module github.com/trendvidia/voya/test/trailer

go 1.26.10

require (
	connectrpc.com/connect v1.20.0
	github.com/trendvidia/voya v0.0.0
	google.golang.org/grpc v1.83.0
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/trendvidia/voya/proto v0.2.1 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.45.0 // indirect
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260818201246-1b0934165a6f // indirect
)

// The harness proxies through voya itself (issue #21), and it must test the
// checkout this file sits in, not a published version.
//
// ADR 001 rejects `replace` for the *sibling repos* — chameleon and
// protowire-go — where it pins a build to a directory that may not exist and
// is easy to commit by accident. This one is neither: the target ships in the
// same repository, at a fixed relative path, so it always resolves and can
// never point at a stale checkout.
//
// go.work already makes this import work for every normal invocation. The
// replace is what `make tidy-check` needs, which runs `GOWORK=off go mod tidy`
// precisely to see each module the way a workspace-less machine would. Without
// it, tidy resolves voya to whatever pseudo-version origin/main last
// published, and the harness silently attests to code nobody is running.
replace (
	github.com/trendvidia/voya => ../..
	github.com/trendvidia/voya/proto => ../../proto
)
