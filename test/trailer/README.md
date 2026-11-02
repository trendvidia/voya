# gRPC trailer conformance

Proves that the gRPC wire protocol survives **voya's proxy** intact.

```console
$ go test -v ./...
```

No protoc, no codegen, no running daemon — it builds the proxy handler in
process. It is its own Go module so that grpc-go stays out of the root module's
*direct* requirements: no package voya builds imports it.

## What it guards

gRPC carries the real outcome of a call in HTTP/2 **trailers** (`grpc-status`),
never in the HTTP status, which is always `200`. A proxy that drops trailers
turns errors into transport failures or hangs — and the happy path keeps
working, so the defect stays silent until something goes wrong. When it does
surface, it looks like a broken service rather than a broken proxy.

ADR 003 recorded this as the sharpest correctness risk in owning our own data
plane. These tests are what discharged it.

## Design

Four cases, each run **twice** — directly against the service, and through
voya. The direct leg is the control: if it passes and the proxied leg fails,
the proxy is at fault and nothing else can be.

The proxied leg is voya as it ships: the backend registers a port in a
`registry.Registry`, `proxy.New` routes to it — with `proxy.WithErrorLog`, the
way the daemon builds it — and the client dials with
`grpc.WithAuthority("probe.internal:9777")`, so `:authority` selects the backend
exactly as it does for a real service. Anything voya reports about its own
transport fails the test, which is what a `502` diagnostic reaching a gRPC
client would otherwise only hint at.

| Test | Guards against |
|---|---|
| `TestUnaryErrorCodeSurvives` | `NOT_FOUND` degrading to `Internal` — the headline failure |
| `TestUnarySuccessCompletes` | Missing `grpc-status: 0`, which hangs the client |
| `TestServerStreamCompletes` | Trailers lost after a long-lived body |
| `TestTrailersOnlyError` | HEADERS with END_STREAM and no body at all |

### The negative control

`TestNegativeControl_StrippedTrailersAreDetected` runs the four cases' own path
— voya's handler, through the same `proxy.Listen` and `proxy.NewHTTPServer` —
with a wrapper that deliberately discards trailers, and asserts the failure is
observable. Sabotaging the path under test rather than one standing beside it
is what makes the control speak to what the four cases actually exercise:

```
with trailers stripped: code = Internal
  desc = "server closed the stream without sending trailers"
```

It asserts that exact degradation, not merely "some code other than
`NOT_FOUND`". With a routing decision in the path, a weaker assertion passes on
a misrouted authority too — that answers `Unimplemented`, out of voya's own 404
diagnostic, with every trailer forwarded intact. So the control first runs the
same path *unsabotaged* and requires `NOT_FOUND` from it, which leaves trailer
loss as the only thing the second leg can be observing.

A suite that cannot fail proves nothing. This one keeps the other four honest —
if it ever starts passing trivially, the harness has stopped testing anything.

## Result, and its shelf life

All eight subtests pass against voya's proxy. voya routes through
`httputil.ReverseProxy`, and `net/http` re-emits undeclared trailers via
`http.TrailerPrefix` (`net/http/httputil/reverseproxy.go:629`) — the branch gRPC
depends on, since it never announces its trailers. The historical reputation for
dropping them is stale.

**Verified against:** Go 1.26.5, `connect-go` v1.20.0, `grpc-go` v1.83.0. The
conclusion is version-bound — re-run on a toolchain change. See ADR 003's
amendment of 2026-08-10.

## Why grpc-go, when internal/proxy tests this too

`internal/proxy` covers the same wire format with connect-go's gRPC client, and
does it on every root-module test run. This harness is the second
implementation: grpc-go is what `grpcurl` and every non-Go consumer speaks, so
running it here is what keeps the evidence about the *protocol* rather than
about one library's reading of it. Keeping grpc-go in a module of its own is
what stops that reaching the module `go install` builds.

Through chameleon v0.22.1 that was true only of the *declaration*: the root
module got grpc-go regardless, via `internal/config → chameleon/resolver →
chameleon/sops → sops/v4/keyservice`, and linked 159 of its packages into
`cmd/voya`. chameleon v0.23.0 removed that route
([#28](https://github.com/trendvidia/voya/issues/28)), so the split now keeps
the dependency out rather than just its label.

[#225](https://github.com/trendvidia/voya/issues/225) narrowed that once more,
without touching what is built: `test/acceptance`'s REST fixture required
`connectrpc.com/vanguard` in the root module, and vanguard's own tests brought
grpc-go back into that module's *build list* — never into a `require` line, and
never into anything `cmd/voya` links, which `cmd/voya/deps_test.go` pins.
[#236](https://github.com/trendvidia/voya/issues/236) moved that fixture into a
module of its own, the way this harness is, and the root module's build list
is grpc-free again.
