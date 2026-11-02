# ADR 003: Protocol Support — Connect-First, No Transcoding in the Proxy

**Status:** Accepted
**Date:** 2026-08-08
**Relates to:** [001](001-core-architecture.md), [006](006-edge-layering.md)

## Context

Backend services need to be reachable by other services, by browser frontends,
and by command-line tools. The original draft of this ADR proposed shipping
protobuf `FileDescriptorSet`s from each service to a control plane, which would
inject them into Envoy's `grpc_json_transcoder` filter to synthesize REST/JSON
endpoints.

Two facts make almost all of that machinery unnecessary:

1. All services are Go on `connect-go`, which natively serves the **Connect,
   gRPC, and gRPC-Web** protocols on a single port, over both HTTP/1.1 and
   HTTP/2 — with no gateway, filter, or descriptor involved.
2. **Connect is codec-agnostic over plain HTTP POST.** The same endpoint serves
   binary protobuf for service-to-service traffic and JSON for browsers, chosen
   per-request by `Content-Type`. No transcoder is required for either.

## Decision

Standardize on the **Connect protocol** for internal traffic, and perform no
protocol translation locally.

* `voya` proxies bytes. It does not parse, translate, or inspect RPC payloads,
  and it holds no protobuf descriptors.
* Services expose Connect / gRPC / gRPC-Web natively via `connect-go`.
* **Service-to-service traffic uses the binary protobuf codec** —
  `github.com/trendvidia/protowire-go/encoding/pb`. JSON is not a wire format
  between our own services.
* **The JSON codec is enabled only on browser-facing endpoints** — an edge
  concern, unmarshaled at the boundary and never propagated inward.
* **Human-readable output is pxf** — `protowire-go/encoding/pxf`, not JSON. This
  covers `voya` CLI output (`voya routes`, registry dumps), request/response
  logging, and golden-file fixtures in tests.
* `voya` injects permissive CORS headers on responses **in local dev only**, so
  browser frontends work without per-service middleware.
* **RESTful resource URLs are served by `connectrpc.com/vanguard`, in-process in
  each service** — see the amendment below. `voya` itself is unaffected: it still
  proxies bytes and holds no descriptors.

## Alternatives considered

**1. Envoy `grpc_json_transcoder` with descriptors shipped at registration.**
*(the original draft)* Rejected. It requires merging N `FileDescriptorSet`s into
one descriptor pool; every service embeds copies of the well-known types, and
services compiled against different `protoc`/`genproto` vintages produce
conflicting definitions for the same filename. A conflicting pool fails to
build, and the blast radius is the whole filter — REST dies for every service,
not just the offender. It also inlines multi-megabyte descriptors into listener
config, forcing a listener rebuild and dropping in-flight connections on every
service restart. Moot once Envoy is dropped (ADR 001), but recorded because the
merge hazard would follow the descriptors into any gateway.

**2. `connectrpc.com/vanguard`, in-process per service.** **Adopted** — see the
amendment below. Its distinct contribution over `connect-go` is precisely the
runtime REST binding that `@http` annotations describe but do not implement.
Keeping it in-process rather than central is what avoids alternative 3's
descriptor-merge hazard: each service holds only its own descriptors.

**3. Vanguard in a central gateway.** Rejected: it still requires shipping
descriptors to the gateway, reproducing alternative 1's merge problem in Go
rather than C++.

## Consequences

* **Positive:** No build-step requirement, no descriptor loading at boot, no
  descriptor merge failures. The obligation the original draft placed on every
  service — emit a `.pb` descriptor and load it on startup — disappears.
* **Positive — and load-bearing:** Connect unary reports errors as an HTTP
  status plus a JSON body, and Connect streaming uses end-of-stream envelopes in
  the body. Neither uses HTTP/2 trailers. This removes the single largest
  correctness risk in owning our own data plane, since `grpc-status` trailer
  forwarding is where hand-rolled Go proxies break subtly.
* **Positive:** Browser `fetch` works against any service with no gateway and no
  annotations, using the JSON codec at the edge only. For command-line poking at
  internal services, `voya call` should speak protowire and render pxf rather
  than pushing developers toward the JSON path.
* **Inherited detail, not a choice:** the Connect protocol returns *unary error*
  bodies as JSON regardless of the request codec. That is fixed by the protocol
  spec. It stays confined to the error envelope and is unmarshaled at the
  boundary like any other edge format.
* **Resolved, was a risk:** Consumers that must speak the **gRPC wire protocol**
  (`grpcurl`, future non-Go clients) do exercise the HTTP/2 trailer path through
  the proxy, which is where hand-rolled Go proxies historically broke. Verified
  rather than assumed — see the trailer section below.
* **Negative:** Each service now carries Vanguard and wraps its handler — one
  more line in `main.go`, and a Go-only capability. A non-Go service would still
  get Connect, gRPC and gRPC-Web from its own stack, but no REST surface.
* **Negative:** Permissive local CORS means a genuine CORS misconfiguration will
  not surface until deploy. Accepted as a dev-ergonomics trade; the production
  policy belongs at the Fly edge and must be tested there.

## Amendment 2026-08-08: a RESTful surface is required after all

The decision above deferred RESTful URLs, reasoning that Connect's JSON codec
covers browser and command-line use. That reasoning does not survive contact with
the actual workflow: **Postman-driven API validation against a generated OpenAPI
spec.**

The gap is specific. protowire carries an `@http(method, path, …)` annotation and
renders it into OpenAPI, and its path templates bind `{name}` segments to
same-named request fields exactly as `google.api.http` does. But the annotation
is **documentation metadata only**. Its own fixture states that "binding is a
generator concern, not a compiler one; the compiler only carries the string,"
and that "a port that renders no REST surface parses these annotations and
interprets nothing."

So a spec generated from `@http` describes `GET /orders/{order_id}`, and nothing
serves it. Imported into Postman, every request 404s against a `connect-go`
service, which answers only `POST /fixtures.http.Orders/GetOrder`. A spec that
does not describe the running system is worse than no spec — it produces failures
that look like service bugs.

**Decision:** adopt `connectrpc.com/vanguard` **in-process in each service**.
Vanguard performs the runtime binding — path template to request fields,
remaining fields to query string or body — that `@http` only describes.

Placing it in the service rather than in `voya` matters for three reasons:

1. `voya`'s decision is untouched. It still proxies bytes and holds no
   descriptors, so alternative 1's merge hazard never arises — each service holds
   only its own.
2. It behaves identically on Fly. Each Machine becomes independently
   REST-capable, so Postman validates against production exactly as it does
   locally. A central gateway would exist only in dev and would itself become a
   divergence.
3. It is one wrapper line in `main.go`, beside the registration hook ADR 004
   already requires.

## gRPC trailers through a Go proxy: verified 2026-08-08

gRPC carries its real outcome in HTTP/2 trailers (`grpc-status`), never in the
HTTP status, which is always `200`. A proxy that drops trailers turns errors into
transport failures or hangs — and the happy path keeps working, so the defect is
silent until something goes wrong. This was recorded as the sharpest correctness
risk in owning our own data plane.

**It is not a real risk on current Go.** A `connect-go` service behind
`httputil.ReverseProxy`, both legs cleartext HTTP/2, driven by a native `grpc-go`
client. Every case runs twice — directly against the service as a control, and
through the proxy:

```
TestUnaryErrorCodeSurvives    direct ✓  proxied ✓
TestUnarySuccessCompletes     direct ✓  proxied ✓
TestServerStreamCompletes     direct ✓  proxied ✓
TestTrailersOnlyError         direct ✓  proxied ✓
```

`httputil.ReverseProxy` forwards `te: trailers` upstream, closes the response
body specifically so `Response.Trailer` populates, and — the case that matters
for gRPC, which never announces its trailers — re-emits undeclared trailers via
`http.TrailerPrefix` (`net/http/httputil/reverseproxy.go:629`).

A **negative control** confirms the harness can fail: with trailers deliberately
stripped, `NOT_FOUND` degrades to `Internal / "server closed the stream without
sending trailers"` — precisely the silent corruption described above. Passing
tests that cannot fail would prove nothing.

**Verified against:** Go 1.26.5, `connect-go` v1.20.0, `grpc-go` v1.83.0,
`golang.org/x/net` v0.57.0. The conclusion is version-bound; a toolchain change
warrants a re-run.

**Residual, and the reason to keep the harness:** this validates
`httputil.ReverseProxy`, not voya's proxy, which does not exist yet. The four
cases become voya's own regression test by swapping the handler in the middle.
Rejected as unnecessary on this evidence: patching `net/http` in the `goplus`
fork, and pre-emptively routing gRPC through a `grpc.UnknownServiceHandler`
proxy to sidestep trailers entirely.

## Amendment 2026-08-10: the same four cases, now against voya's proxy

The residual above is discharged. `test/trailer` no longer builds a proxy of
its own: it registers the backend in a `registry.Registry`, serves
`proxy.New(reg, proxy.WithErrorLog(…))` — the daemon's own construction —
through the same `proxy.Listen` and `proxy.NewHTTPServer`, and routes by
`:authority` like any other caller. voya's `ErrorHandler` is not replaceable,
so the error log is where a transport failure surfaces as a test failure rather
than as an unexplained status code; anything it reports fails the run.

```
TestUnaryErrorCodeSurvives    direct ✓  proxied ✓
TestUnarySuccessCompletes     direct ✓  proxied ✓
TestServerStreamCompletes     direct ✓  proxied ✓
TestTrailersOnlyError         direct ✓  proxied ✓
```

The negative control was moved rather than kept beside the new path: it runs
the four cases' own path — voya's handler, through the same `proxy.Listen` and
`proxy.NewHTTPServer` — with a wrapper that strips the trailers on the way out,
so the path being sabotaged is the path under test down to the server. It still
observes the corruption — `NOT_FOUND` degrading to `Internal / "server closed
the stream without sending trailers"`.

It now asserts that degradation specifically, which the pre-voya control did
not have to. "Any code but `NOT_FOUND`" was a sufficient assertion when the
sabotaged path was an unconditional `httputil.ReverseProxy` with no routing
decision to get wrong. With voya in the middle it is not: an authority the
registry does not resolve answers `Unimplemented`, out of voya's own 404
diagnostic, with every trailer forwarded intact — so a broken `serviceName` or
a broken `ServiceFromAuthority` would leave the control green while it had
stopped testing trailer loss. The control therefore runs the same path
unsabotaged first and requires `NOT_FOUND` from it, and then pins the sabotaged
leg to `Internal`. Confirmed by mutation: pointing the client at an authority
nothing registers fails the control at the first check.

Two things changed underneath, and both were the point of doing this:

* **The h2c mechanism is now the standard library's.** Both legs are
  `http.Protocols` with `SetUnencryptedHTTP2`, which is what voya ships; the
  harness's `golang.org/x/net/http2/h2c` server and `AllowHTTP` +
  `DialTLSContext` transport are gone, and with them the last SA1019 exclusion
  in `.golangci.yml`. `golang.org/x/net` survives only as grpc-go's own
  indirect dependency.
* **The client is still grpc-go.** `internal/proxy` covers this wire format
  with connect-go's gRPC client, which is a different implementation of the
  same protocol. Keeping grpc-go here is what makes the evidence about the
  protocol rather than about one library's reading of it — and it is what
  `grpcurl` and every non-Go consumer will actually use.

**Verified against:** Go 1.26.5 (`go1.26.5 darwin/arm64`), `connect-go`
v1.20.0, `grpc-go` v1.83.0, voya at `e162f05`. Still version-bound, and now
also voya-bound: this is a regression test, so it re-runs on every change.

## Route annotations: `@http`, and a dependency on protowire

**Resolved 2026-08-08: protowire's `@http` is the source of truth.** It is the
org's schema idiom and is intended as sugar over `google.api.http`, not as a
documentation-only annotation.

That intent is not yet met by the implementation, and voya's REST surface depends
on closing the gap. Verified against protowire's own `@http` fixture, compiled
with `pxf build`:

```
fixtures.http.Orders/ListOrders     opts=[50400]  google.api.http=false
fixtures.http.Orders/GetOrder       opts=[50400]  google.api.http=false
fixtures.http.Orders/CreateOrder    opts=[50400]  google.api.http=false

3 method(s); 0 carry google.api.http (72295728)
```

`@http` lowers to `protowire.schema.v1.http` inside carrier option `50400`. The
routing data is complete and faithful — the image contains `/orders`,
`/orders/{order_id}`, `GET`, `POST` — but the standard `google.api.http`
extension (`MethodOptions` field `72295728`) is never emitted.

Vanguard reads `72295728`. Given these descriptors it finds no rules, binds no
routes, and **reports nothing**: every REST URL 404s and reads as an
unimplemented endpoint rather than a toolchain gap. The same applies to
grpc-gateway, Envoy's transcoder, and buf's OpenAPI plugins.

Filed as **trendvidia/protowire#213**, proposing that `pxf build` emit
`google.api.http` *alongside* the carrier option — the carrier keeps the enriched
operation metadata (`summary`, `operation_id`, `tags`, `security`) that
`HttpRule` has no room for, while the standard extension carries the routing
skeleton every downstream consumer needs. The transform is mechanical, since
`@http`'s model is already 1:1 with `HttpRule`.

**Consequence for voya:** the REST surface adopted in the amendment above is
blocked on that issue. The fallback — a descriptor post-processor in each service
that rewrites carrier `50400` into `google.api.http` before constructing
Vanguard — works, but it is an adapter per service and should be a last resort,
not a plan. Connect, gRPC, and gRPC-Web are unaffected and need no annotations at
all; only the RESTful URL surface waits on this.

Rejected alternatives: **annotating with `google.api.http` directly** (works
today with no protowire change, but abandons the org's schema idiom and splits
the fleet's conventions), and **annotating with both** (no code, but two sources
of truth that drift silently until a route 404s).

**Amendment 2026-08-15: the dependency is met, and M5 is unblocked.**
trendvidia/protowire#213 closed on 2026-08-09 with the fix this section
proposed — `pxf build` emits `google.api.http` (`MethodOptions` field
`72295728`) alongside carrier option `50400`, the carrier keeping the enriched
operation metadata and the standard extension carrying the routing skeleton
(protowire PR #214, released as v1.10.0). Emission is on by default and can be
opted out per build with `--google-api-http=false`; protowire's own fixture
`testdata/schema-extensions/21_http_operation.proto` now states as expected
behaviour that every `@http` method carries the option, and
`cmd/pxf/httprule_test.go`'s `TestBuildEmitsGoogleAPIHTTP` pins it — run
green against protowire v1.11.0 while writing this. So Vanguard reads the
descriptors it needs, the per-service descriptor post-processor the paragraph
above kept as a last resort is never needed, and the two rejected alternatives
stay rejected for the reasons given. What changes for voya: nothing in the
binary — voya still proxies bytes and holds no descriptors — and the REST
surface adopted above stops being blocked. M5 comes into view after M2 and is
tracked in #19.

The same release also narrows what compiles, and it is the services M5 wires
up that inherit that, not voya. Because the skeleton now lowers to a rule
something will try to serve, five previously-accepted `@http` shapes are
compile errors at the annotation's source position — a `{…}` segment
naming no field of the request message, one naming a `repeated` field, a
relative path, unbalanced braces, and an empty method — and `pxf openapi`
refuses a method whose second or later `@http` binding carries no explicit
`operation_id`. protowire's `STABILITY.md` § v1.10 records this as the first
minor since v1.0 that is not purely additive; migration is mechanical, each
diagnostic names the offending segment, and `--google-api-http=false`
suppresses the option rather than the checks. voya's own schemas are
untouched — they carry no `@http` and are generated with buf, not with
`pxf build`. What a service does owe at M5 is the type registry: it links
`google.golang.org/genproto/googleapis/api` so the emitted extension
resolves, as Vanguard already does.

**Proved end to end 2026-08-19 (issue #225).** "Nothing in the binary" had
never been exercised — voya had never served a REST URL in a test. It now
has: `test/acceptance/rest_test.go` runs a Vanguard-transcoded service
registered under a name of its own and calls it by URL through the proxy — a
`GET /v1/greetings/{name}` with a query-bound field, routed on the same
`:authority` as every Connect call in the suite — and the fixture is the
worked example of the wiring, type-registry obligation included. The claim
held: no voya change was needed. What a REST caller sees when routing fails
is ADR 005's subject, recorded in its amendment of the same date.

## Correction to the original draft

The superseded version claimed ConnectRPC and gRPC server reflection were
"handled natively by Envoy's HTTP/2 routing without custom filters." Reflection
is **not** free under path-based routing: `grpc.reflection.v1.ServerReflection`
is the *same path for every service*, so a single front door routing on method
path can only ever reach one arbitrary backend — `grpcurl … list` would silently
list one service's methods and no others.

Authority-based routing (ADR 002) fixes this as a side effect: the `:authority`
header disambiguates, so reflection resolves to the correct service per request.

## Amendment 2026-11-01: CORS is limited to local origins (issue #308)

The decision above said "permissive" and weighed one cost: a misconfiguration
that surfaces late. It did not weigh a hostile page. voya reflected every origin
with credentials, so once `voya dns install` was in place, any website open in
the developer's browser could read every service through it.

**Decision:** voya grants CORS only to `localhost`, `127.0.0.1`, `[::1]`,
`*.localhost` and `*.internal`, over http or https, plus exact origins named in
the `cors_origins` config field. Any other origin gets no CORS headers, and a
backend's own are removed for it, so a backend's `*` cannot reopen the door.
`null` is never allowed.

**Assumption:** a developer's frontend runs on a local name. A frontend served
from a public domain against local services now has to be named in
`cors_origins`. A frontend behind a tunnel is the case most likely to hit this.

The request still reaches the service: CORS decides whether the page may read
the answer, not whether it is sent. A page can still send simple requests to a
service, which is true of any local HTTP server. `TestCORSAllowsOnlyLocalOrConfiguredOrigins`
and `TestARefusedOriginGetsNoCORSHeaders` fail if this stops holding.

## Amendment 2026-11-01: the upstream leg also speaks HTTP/1.1

This replaces "voya's upstream leg is h2c only". A plain HTTP server got a 502
under voya, so a local stand-in such as daysong's fake bucket could not sit
behind a name (issue #315).

**Decision:** the proxy learns what a port speaks before it sends a request to
it, and then uses h2c or HTTP/1.1. It opens a connection of its own, writes the
HTTP/2 preface, and reads the reply. A SETTINGS frame means h2c. Any other reply
or a close means HTTP/1.1. No reply, or a failed dial, teaches nothing and takes
the h2c path, whose failures are the ones the proxy already explains. A server
offering both stays on h2c, so its gRPC keeps working. Nothing changes in the
registration schema, and a service declares nothing.

**Rejected:** trying h2c and retrying over HTTP/1.1. By the time the attempt
fails, a streamed body is spent and a service may have acted on the request.

**gRPC needs HTTP/2:** a gRPC call to an HTTP/1.1 service is refused without
being sent, by name. gRPC-Web and Connect pass, because neither needs HTTP
trailers.

**Assumptions:** the answer is remembered per address and forgotten when a
request fails, so a port reused with the other protocol costs one failed
request. A Go HTTP/1.1 handler that isn't an `http.ServeMux` sees the preface as
one `PRI *` request on first contact. `TestAPortThatChangesProtocolIsAskedAgain`
and `TestTheProbeReachesOnlyABareHandler` fail if either stops holding.

## Amendment 2026-11-01: the upstream leg also speaks TLS, to loopback only

This extends the previous amendment. A service that serves only TLS got a 400
or a 502, depending on its server. The caller now stays on plain HTTP to
`<name>.internal`, and voya connects to the service over TLS.

**Decision:** a reply to the HTTP/2 preface that isn't a SETTINGS frame, or a
hang-up, no longer means HTTP/1.1 outright. voya then tries a TLS handshake on a
second connection. If that succeeds the port is TLS, and the negotiated protocol
says whether it offers HTTP/2. If it fails the port is HTTP/1.1. A timeout on the
preface still teaches nothing. gRPC follows the negotiated protocol: it passes
over TLS with HTTP/2 and is refused by name over TLS with HTTP/1.1.

**Certificates are not verified.** A local service's certificate can't match
`127.0.0.1`, and the connection doesn't leave the machine.

**Assumption:** the target is always loopback, because registration carries a
port and no host. If a target could ever name another host, skipping verification
would become a real hole, so that change must not reuse this configuration.
`TestUpstreamTLSIsOnlyForLoopback` fails if the dial stops going to the request's
loopback address.

**Not covered:** client certificates, and servers that choose a certificate by
hostname, since voya sends no client certificate and no server name. The local
leg doesn't show a caller TLS: `Secure` cookies aren't sent back over `http://`,
and a service redirecting to its own `https://` address sends the caller
somewhere voya doesn't serve.
