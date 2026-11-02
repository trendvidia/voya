# ADR 001: Core Architecture — Single Go Binary, Authority-Routed Local Proxy

**Status:** Accepted
**Date:** 2026-08-08
**Relates to:** [000](000-production-substrate.md), [002](002-service-discovery.md), [003](003-protocol-support.md), [004](004-service-lifecycle.md), [005](005-observability.md), [006](006-edge-layering.md)

## Context

Local development of our Go microservices suffers from port collision, tedious
tracking of which service runs where, and orchestration overhead (Docker
Compose, Kubernetes) disproportionate to the problem.

The production target is **Fly.io** (ADR 000): services run as Firecracker Machines on a
flat WireGuard private network (6PN), reachable by internal DNS at
`<app>.internal:<port>`. Fly has **no service mesh, no sidecars, and no xDS**.
Discovery is DNS; there is no registry and no registration step. Every Machine
has its own IP, so port collision does not exist in production.

All backend services are Go, built on `connect-go`.

This narrows the goal considerably. We are not building a service mesh. We are
building a **local emulator of Fly's networking model**, so that application
code — in particular dial strings — is byte-identical in both environments.

## Decision

A single Go binary, `voya`, that is registry, DNS server, and reverse proxy in
one process. No Envoy, no containers, no orchestrator.

* **Proxy** on `127.0.0.1:9777`, routing by `:authority` / `Host` header to the
  registered ephemeral port of the named service.
* **DNS** on `127.0.0.1:9778`, answering every `*.internal` query with
  `127.0.0.1` (see ADR 002 — this is a static wildcard, never updated).
* **Registration** on `127.0.0.1:9779` (see ADR 004).
* Distributed via `go install`; cross-compiles to every platform Go supports.

### Formats

`voya`'s own configuration is **chameleon** (`github.com/trendvidia/chameleon`).
Config is a typed protobuf message resolved from layered `.pxf` files against a
ChainSpec — so the config schema is a `.proto` under `proto/`, not a file format
decision. Start with `chameleon.Load`; move to `resolver.New` if we ever need
multiple sources or encrypted layers.

Serialization is **protowire** (`github.com/trendvidia/protowire-go`):
`encoding/pb` for binary wire, `encoding/pxf` for anything a human reads. See
ADR 003.

TOML, YAML, and JSON appear only where an external system mandates them at the
boundary — `fly.toml` because Fly requires it, `/etc/resolver/internal` because
macOS defines the format. Neither is a format we chose, and neither is read by
anything beyond the adapter that parses it.

Fleet convention: every service declares port `9777` in its `fly.toml`, so
`http://payments.internal:9777` resolves and connects unchanged in both
environments.

## Alternatives considered

**1. Envoy data plane + Go xDS control plane.** *(the original draft of this ADR)*
Rejected. The motivating argument was that Envoy mirrors production — but
production is Fly, which has no mesh and no xDS, so the parity is illusory. The
costs are concrete: an xDS translation layer (snapshot cache, LDS/RDS/CDS/EDS
construction, version management) is the single largest chunk of the
implementation; Envoy's practical laptop install story is usually a container,
contradicting the no-Docker goal; and inlining config forces listener rebuilds
that drop connections on every service restart. Envoy's resilience features
(retries, circuit breaking, outlier detection, LB policies) cannot be exercised
at one instance per service.

**2. `connectrpc.com/vanguard` as a central transcoding gateway.** Rejected as
unnecessary — see ADR 003.

**3. Docker Compose.** Rejected. The rebuild/restart cycle is slower than
`go run`, and containers-on-a-laptop mirror Fly Machines only superficially.

**4. Nothing — fixed ports, dial `localhost:PORT` directly.** Rejected. This is
the status quo pain, and it forces dial strings to differ between dev and prod,
which is the specific divergence this project exists to remove.

## Consequences

* **Positive:** One binary, `go install`-able, works anywhere Go cross-compiles
  — including Windows, which the Envoy design did not.
* **Positive:** Application dial strings are identical dev and prod. No
  environment-conditional networking code.
* **Positive:** We can set a breakpoint in our own data plane. We could not in
  Envoy.
* **Negative:** We own the proxy's correctness. Materially mitigated by the
  Connect-first decision in ADR 003, which removes the need to forward HTTP/2
  trailers — the hardest part of proxying gRPC.
* **Negative:** No retries, circuit breaking, load balancing, or mTLS locally.
  Accepted deliberately: Fly does not provide these either, so their absence is
  parity rather than a gap.
* **Negative:** `voya` is a local single point of failure — if it dies, all
  inter-service traffic stops. Acceptable for a dev tool, but it must fail
  loudly and visibly rather than hanging.
* **Negative:** If `9777` is already bound, `voya` cannot start. It must exit
  with a message naming the port and, where possible, the process holding it.
* **Practical:** `chameleon` and `protowire-go` are sibling repos under
  `trendvidia/`. Local cross-repo development wants a `go.work` — `protowire-go`
  already carries one — rather than `replace` directives in `go.mod`.

**Amendment 2026-08-20 (issue #237): the root module's `go.mod` carries no
`replace` and no `exclude` at all, of any kind.** The rejection above names
only the sibling repos, which is why intra-repo replaces looked permissible
and landed in the root module (#36) in good faith. The constraint that holds
is stricter and follows from "distributed via `go install`" above:
`go install pkg@version` refuses a module whose `go.mod` carries `replace` or
`exclude` directives, so the module a person installs — the root, home of
`cmd/voya` — carries none. The rule is about that module, not the repository:
`client`, `test/trailer` and `test/acceptance` keep their intra-repo replaces,
and must — nobody installs them. `TestRootGoModCarriesNoReplaceOrExclude` in
`cmd/voya` is the pin; the root `go.mod`'s comment carries the cost (a sibling change the
daemon depends on needs its tag cut and the require bumped before the root
module sees it outside the workspace), and `make install-check` turns the
compile-visible part of that divergence into a red build.
