# voya

A local emulator of [Fly.io](https://fly.io)'s networking model, in one Go
binary. Services dial `http://payments.internal:9777` on a laptop exactly as
they do on Fly — same dial string, no environment-conditional networking code,
no Docker, no service mesh.

> **Status: walking skeleton.** The design is settled and recorded in
> [`docs/adr/`](docs/adr/); the implementation is being built milestone by
> milestone (roadmap: [#19](https://github.com/trendvidia/voya/issues/19)).
> `voya run` serves all three listeners, `voya status` is ADR 005's one
> screen (voya itself, every registered service, evictions and reclaims
> after the fact), `voya routes` is the routing table, `voya log` prints the
> request log the daemon keeps (`-f` follows it), `voya health` fills the
> status screen's HEALTH column with one `grpc.health.v1` probe per service
> on demand (also `voya status -health`), and `voya call`
> calls a registered service by name with a pxf request and prints its reply
> as pxf, taking the schema from the callee's own reflection. `voya dns
> install` writes the file that gives curl and browsers the names — macOS's
> `/etc/resolver` entry or Linux's systemd-resolved drop-in — `voya dns
> uninstall` takes exactly that file back, and `voya dns check` tests how
> `*.internal` resolves on this host when curl or a browser cannot reach a
> name. `voya token` mints a signed JWT
> for a configured persona — valid, or deliberately not (`--expired`,
> `--bad-signature`) — with no daemon needed, and the daemon serves the
> matching JWKS at `voya.internal:9777/.well-known/jwks.json` (ADR 006).
> With that, every command the ADRs describe exists.

## Quick start

Two example services ship with voya —
[`examples/payments`](examples/payments) and
[`examples/billing`](examples/billing) — so you can see the whole thing work
before writing any of your own. Three terminals, no sudo, no DNS setup, no
Docker.

```console
$ go install github.com/trendvidia/voya/cmd/voya@v0.3.0   # terminal 1
$ voya run
voya: proxy on http://127.0.0.1:9777, registration on 127.0.0.1:9779, dns on 127.0.0.1:9778
```

```console
$ cd examples && go run ./payments                        # terminal 2
payments[first]: serving on 127.0.0.1:61886, reachable at http://payments.internal:9777
payments[first]: voya: payments is serving on http://payments.internal:9777
```

```console
$ cd examples && go run ./billing                         # terminal 3
billing: serving on 127.0.0.1:61893, reachable at http://billing.internal:9777
billing: payments said "payments[first] charged invoice-001"
billing: payments said "payments[first] charged invoice-002"
```

billing is calling `http://payments.internal:9777` — the dial string it would
use unchanged on Fly — while payments is on an ephemeral port nothing was
configured with. That is the whole idea; the rest is watching it hold.

## Basic usage

With those three terminals running, a fourth is where voya answers questions.

**Who is registered, and on what.** ADR 005's one screen:

```console
$ voya status
voya (devel), pid 33104, up 19s since 2026-08-21T21:15:35-07:00
  proxy 127.0.0.1:9777  dns 127.0.0.1:9778  registration 127.0.0.1:9779
  resolver file /etc/resolver/internal: not installed

services (2)
NAME      AUTHORITY          PORT   PID    UPTIME  STREAM    HEALTH
billing   billing.internal   61893  33313  10s     attached
payments  payments.internal  61886  33246  12s     attached
```

**Where a name goes right now**, and what the proxy has answered:

```console
$ voya routes
AUTHORITY          SERVICE   UPSTREAM
billing.internal   billing   127.0.0.1:61893
payments.internal  payments  127.0.0.1:61886

$ voya log -n 3
2026-08-21T21:15:50.491-07:00 payments.internal POST /voya.example.v1.Payments/Charge 200 801µs -> 61886
2026-08-21T21:15:52.491-07:00 payments.internal POST /voya.example.v1.Payments/Charge 200 837µs -> 61886
2026-08-21T21:15:54.492-07:00 payments.internal POST /voya.example.v1.Payments/Charge 200 2.833ms -> 61886
```

`voya log -f` follows it, so you can watch requests as they happen.

**Call a service yourself**, without writing a client. payments serves
reflection, so voya asks it for its own schema and builds the request:

```console
$ voya call payments voya.example.v1.Payments/Charge 'value = "invoice-999"'
value = "payments[first] charged invoice-999"
```

The service name is the bare `payments`, not a URL, and the request is
[pxf](#conventions) rather than JSON — binary protobuf on the wire, the way
the fleet's own clients call each other.

**Ask each service how it is.** `voya health` is the status screen with HEALTH
filled in, from one `grpc.health.v1` check per service:

```console
$ voya health
...
NAME      AUTHORITY          PORT   PID    UPTIME  STREAM    HEALTH
billing   billing.internal   61893  33313  31s     attached  serving
payments  payments.internal  61886  33246  33s     attached  serving
```

**Restart a service and watch traffic follow it.** This is the part that
removes the local-development difference: start a second copy of payments
while the first is running.

```console
$ cd examples && go run ./payments -instance second       # terminal 2 again
```

voya evicts the first instance loudly, in both terminals, and billing keeps
calling one unchanged name:

```console
billing: payments said "payments[second] charged invoice-016"
```

Nothing was reconfigured, and no port was chosen by anyone. Stop the second
copy and the first takes its name back on its own — it has been asking every
15s since it was deposed:

```console
$ voya status
evictions (1)
2026-08-21T21:16:14-07:00  payments.internal: port 61886, pid 33246 was evicted by port 61988, pid 33943

reclaims (1)
2026-08-21T21:16:54-07:00  payments.internal: port 61886, pid 33246 took its name back, 40s after losing it
```

**Stop payments entirely** and billing does not get a bare connection error.
It gets the authority that failed, why, and what would have worked:

```
unavailable: voya: service "payments" is known but has no running instance.
It registered earlier and its stream dropped, or it has not started yet.
Authority received: "payments.internal:9777"

Registered services: billing.internal
Known but not running: payments.internal
```

Everything above works without `voya dns install`, because the dialer
`voya.Join` installs is what resolves `*.internal` for Go callers. Install the
resolver file when you want curl, a browser or Postman to reach the same names.
`voya dns check` says when it is missing, and that alone does not change its
verdict.

## Why

On Fly, every Machine has its own 6PN address and services bind a fixed port,
so `payments.internal:9777` just works and port collision does not exist.
Locally, several services share one machine: ports collide, and tracking which
service owns which port goes stale. voya removes that difference rather than
papering over it — services bind port `0`, register the port they actually got,
and voya routes to them by name.

## How it works

* **Proxy** on `127.0.0.1:9777` routes on the `:authority` / `Host` header to
  the registered ephemeral port of the named service. One name is voya's own:
  `voya.internal` on the same port serves `voya.diag.v1.DiagService` — status,
  routes, the request log — so tooling reads what voya knows instead of
  scraping CLI output (ADR 005). No service can register it.
* **A client dialer** — not DNS — is the primary resolution path. It sends any
  `*.internal` host to `127.0.0.1:9777` while leaving the URL untouched, so the
  authority still names the service. When `FLY_APP_NAME` is set it is a no-op
  and ordinary 6PN resolution applies.
* **DNS** on `127.0.0.1:9778`, UDP and TCP, answers every `*.internal` query
  with `127.0.0.1` — a static wildcard for consumers that are not our Go
  services (`curl`, `grpcurl`, Postman, browsers). Opt-in; no Go service needs
  it.
* **Registration** on `127.0.0.1:9779` — the long-lived bidirectional Connect
  stream each service opens and holds for its lifetime (ADR 004).

All three listeners bind loopback explicitly, never `0.0.0.0`.

voya speaks to each service in the protocol it offers: cleartext HTTP/2 or
HTTP/1.1, on the same port, with nothing to configure. It asks a port once, on
a connection of its own, before sending the first request. A plain HTTP server
therefore works behind a name, as long as it registers. gRPC is the exception:
it needs HTTP/2 end to end, so voya refuses a gRPC call to an HTTP/1.1 service
and says why. Connect and gRPC-Web calls work over either. The check shows up
once in an HTTP/1.1 server's access log as `PRI * HTTP/2.0`, and a Go handler
that isn't an `http.ServeMux` sees it as a request.

A service that serves only TLS works too. The caller stays on
`http://<name>.internal:9777`, and voya connects to the service over TLS. voya
accepts any certificate there, because a local service's can't match
`127.0.0.1`, and it only ever makes that connection to a loopback port. gRPC
works if the service offers HTTP/2 over TLS. A service that requires a client
certificate, or picks its certificate by hostname, won't work, because voya
sends neither. The check adds one malformed request to a plain HTTP/1.1
server's log, and no handler sees that one.

## What voya does not emulate

voya emulates one thing: how services find and reach each other by name over
`.internal`. A clean local run tells you nothing about the rest of a deploy,
and the rest is often where a first deploy goes wrong. A single service that
clients reach at `https://<app>.fly.dev` gets little from voya, because nothing
calls it over `.internal`.

| | on Fly | under voya |
|---|---|---|
| **Public edge** | clients use `https://<app>.fly.dev`, with TLS, `force_https` and Fly's request headers | cleartext `http://<name>.internal:9777`, with none of those, so the local and deployed URLs differ |
| **Secrets** | `fly secrets`, plus those `fly storage create` and `fly mpg attach` set | nothing; set the environment yourself |
| **Postgres and object storage** | managed services you attach | not provided; [ADR 000](docs/adr/000-production-substrate.md) says to plan the database separately |
| **Machine behaviour** | health checks from `fly.toml`, restarts, stopping and scaling | nothing reads `fly.toml`, nothing is restarted, and health is probed only when you run `voya health` |
| **Instances and regions** | several Machines per app, and region-prefixed names such as `iad.<app>.internal` | one instance per name, where a second evicts the first; only single-label names route, so `iad.payments.internal` gets a 400 and `payments.flycast` is refused |
| **Listening** | a fixed port, on 6PN, which is IPv6-only | any loopback port, so a service bound to `127.0.0.1:0` works here and cannot be reached there |

The last row needs a change in your service. The snippets and examples below
bind `127.0.0.1:0` because they are local; on Fly a service binds the port its
`fly.toml` declares, dual-stack (ADR 000).

Two conventions are easy to miss, and voya checks neither:

- **Port.** Every service declares `9777` in `fly.toml` (ADR 001), so the dial
  string stays the same. Fly's default template uses 8080.
- **Health.** Services serve `grpc.health.v1` (ADR 004), which is what a
  `fly.toml` HTTP check can call. The HEALTH column in `voya status` is blank
  until you run `voya health`, and a service with only a plain `/healthz` then
  shows `error`.

## Layout

```
cmd/voya/          CLI + daemon entrypoint
internal/…/        implementation packages
proto/voya/…/v1/   schemas + generated bindings (its own module)
client/            what services import (its own module)
authn/             token validation for services (its own module)
examples/          two runnable services to try voya against (its own module)
changelog.d/       release notes, one file per change
test/acceptance/   two services, a real daemon, the real dial strings (its own module)
test/gates/        the Makefile's gates, run on copies of what they read
test/roundtrip/    the dialer + authority-routing spike
test/trailer/      gRPC trailer conformance harness (its own module)
docs/adr/          the decisions, and what was rejected
```

## Development

```console
$ make help          # every target
$ make test          # all seven modules, race detector
$ make ci            # what CI runs, end to end — the gate before a PR
```

[`CLAUDE.md`](CLAUDE.md) is the working contract: what `make ci` chains, what
counts as done, how things merge, how gaps get filed, the traps worth knowing
before you debug one, and the two rules that are about editing this code rather
than using it. It links back here for what voya is rather than restating it —
one home per fact ([#77](https://github.com/trendvidia/voya/issues/77)). Read
it before the first change, not after the first review.

The repo holds **six Go modules** — the root module, `authn`, `client`,
`proto`, `test/acceptance` and `test/trailer`. `go test ./...` does not cross
module boundaries, so anything that walks the tree walks all six explicitly.
`authn` is what a service imports to turn a token into a principal (ADR 006's
"library, not a location"): a module of its own so that importing it links a
JOSE library and nothing else of voya's, which `authn/deps_test.go` enforces —
the root module may import `authn`, never the reverse. `client` is the same
shape one step earlier ([#36](https://github.com/trendvidia/voya/issues/36)): what every
service imports to join local development, a module of its own so that
importing it links connect-go, the generated schemas and protobuf rather than
the daemon's requirements — chameleon among them, 55 modules against
`client`'s 7 at the time of writing — enforced by `client/deps_test.go`. `proto` holds the generated bindings both
of those arrows stand on: the daemon serves the schemas and `client` speaks
them, so the bindings are a leaf module each can require without requiring
the other. `test/trailer` is version-bound evidence for ADR 003: if a
toolchain bump breaks gRPC trailer forwarding, that has to surface as a red
build rather than as someone debugging a mysterious `Internal`.
`test/acceptance` is a module for the same reason
([#236](https://github.com/trendvidia/voya/issues/236)): its REST fixture
requires a transcoder whose own tests carry grpc-go, and in the root module
that put both on the manifest a person `go install`s. It imports the root
module — the opposite arrow from `client` and `authn`, and the right one for
a suite that starts the daemon. `examples` is a module on the same argument
([#293](https://github.com/trendvidia/voya/issues/293)): nobody installs the
two demo services, so their requirements have no business on the manifest of
the module people do — and being a module here keeps them compiled, since
`make ci` walks every one of them for every cross target.

`go.work` covers those seven modules, and `make modules-check` fails when the
Makefile's own list of them drifts from it. For cross-repo work against local
`chameleon` or `protowire-go` checkouts, use a git-ignored `go.local.work`
selected with `GOWORK` — see the comment at the top of `go.work`. ADR 001
rejects `replace` directives in `go.mod` for those siblings. Intra-repo ones
are the exception — same repository, fixed relative path, so they always
resolve *for a build of this repo*: `test/trailer` carries them for voya
itself and for `proto`, and its own `go.mod` makes the argument; `client`
carries one for `proto`; `test/acceptance` carries four, one per module it
reaches, so that the suite exercises the checkout and not a published tag.

The root module carries none, and the absence is load-bearing: `go install
pkg@version` refuses a module whose `go.mod` has `replace` or `exclude`
directives, and ADR 001 distributes voya that way
([#231](https://github.com/trendvidia/voya/issues/231)), with
`TestRootGoModCarriesNoReplaceOrExclude` in `cmd/voya` as the pin. What that
costs is written where the replaces used to be, in `go.mod`: ordinary builds
and `make test` still resolve the three siblings through `go.work`, but the
`GOWORK=off` pass `make tidy-check` runs resolves the *published* versions, so
a change to `authn`, `client` or `proto` that the daemon depends on needs its
tag cut and the root's require bumped before this module sees it outside the
workspace — `make install-check` gates on it
([#237](https://github.com/trendvidia/voya/issues/237)) by type-checking this
module, test files included, against those tags
([#246](https://github.com/trendvidia/voya/issues/246)) — for the host and
then once per supported GOOS, so a build-constrained file reaches it too
([#267](https://github.com/trendvidia/voya/issues/267)) — and by parsing its
embedded config layers with those bindings, so a stale sibling tag the
daemon's own code names — or that its `base.pxf` defaults do — is a red gate
rather than a wrong installed binary. What that leaves uncovered is written on
the target in the `Makefile`.

A `replace` is ignored in a consumer's build, though, so what a consuming repo
fetches is whatever the `client/go.mod` *of the tag it names* requires. That is
a published version as of
[#226](https://github.com/trendvidia/voya/issues/226): `client` requires
`voya/proto`, tagged directory-prefixed the way a multi-module repo versions
its parts, so a service repo needs neither a workspace nor a `replace` of its
own — where before it required a `v0.0.0` no tag provided. The require in this
tree is `voya/proto v0.2.1`
([#246](https://github.com/trendvidia/voya/issues/246)), and `client/v0.2.1`
is the first cut tag to carry it
([#280](https://github.com/trendvidia/voya/issues/280)); a *further* bump here
reaches a consumer only at the `client` tag cut after it. `client` and `authn`
are tagged as of
[#231](https://github.com/trendvidia/voya/issues/231) — the root requires both
by version now, so `authn`'s untagged `v0.0.0` had nothing left to resolve to
the moment the replaces went — and a consumer names the tag (`go get
github.com/trendvidia/voya/client@v0.3.0`, the version the root requires too).

## Using voya from a service

> **Getting the module needs a tag.** `go get
> github.com/trendvidia/voya/client@v0.3.0` resolves `voya/proto` from another
> repository as of [#226](https://github.com/trendvidia/voya/issues/226),
> which tagged it and pointed the require at the tag; `client` carries a tag of
> its own as of [#231](https://github.com/trendvidia/voya/issues/231). The code
> below is what a service writes whichever version it names.

The snippets below are the pieces and why each is needed.
[`examples/payments`](examples/payments) and
[`examples/billing`](examples/billing) are the same pieces assembled into two
programs that run — and, unlike this section, they are compiled by `make ci`
for every cross target. Read those if you would rather start from something
whole.

One call in `main.go`, and it is a no-op in production:

```go
import voya "github.com/trendvidia/voya/client"

ln, err := net.Listen("tcp", "127.0.0.1:0") // bind port 0; the OS chooses
if err != nil {
	return err
}
// The examples use (*net.ListenConfig).Listen(ctx, …) instead, because
// voya's own lint rules require the context-aware form. Either works here.
stop, err := voya.Join(ctx, "payments", ln.Addr())
if err != nil {
	return err
}
defer stop()
```

Serve reflection beside it, so `voya call payments …` can ask this service
for its own schema (ADR 004; the names are the constants the generated
Connect files declare):

```go
import voyareflect "github.com/trendvidia/voya/client/reflection"

if err := voyareflect.Mount(mux, paymentsv1connect.PaymentsServiceName); err != nil {
	return err
}
```

Both protocol versions are mounted, so `grpcurl` and older tooling reach it
too. It is a package below `client` rather than part of it: importing
`client` alone stays as cheap as it was, and only a service that serves
reflection pays for `connectrpc.com/grpcreflect`.

When `FLY_APP_NAME` is set, `Mount` serves nothing, so this example is safe to
ship as written: your schema is not public in production.

Serve that mux over **cleartext HTTP/2** — reflection is a bidirectional
stream, and a default `http.Server` answers one with `505 HTTP Version Not
Supported`, which every client reads as "this service has no schema":

```go
protocols := new(http.Protocols)
protocols.SetHTTP1(true)
protocols.SetUnencryptedHTTP2(true)
srv := &http.Server{Handler: mux, Protocols: protocols}
```

That does both halves. This service becomes reachable at
`http://payments.internal:9777`, and its own calls to
`http://billing.internal:9777` reach billing — the same dial strings it will
use on Fly, with no build tags and no environment branch in application code.
When `FLY_APP_NAME` is set the call does nothing at all: each Machine has its
own 6PN address and `*.internal` already resolves.

`FLY_APP_NAME` is the only switch, because voya cannot otherwise tell production
from a developer's machine. **On a production host that is not Fly, set
`FLY_APP_NAME` to any value, or do not call `Join`.** Without that, a
`*.internal` call on the proxy port goes to loopback, and whatever is listening
on 9777 there receives it, bearer tokens included.

It never blocks and never fails because voya is not running. The stream is
opened in the background and retried with bounded backoff, so a service
starts and serves normally with no voya at all — degrading only in that
nothing can reach it by name.

Services that build their own `http.Client` can install the dialer
themselves with `voya.RoundTripper()` and pass `voya.WithoutDialer()` to
`Join`. That is what `Join` installs on `http.DefaultTransport`: the dialer
plus the client half of the rate-limit contract below — a 429 from voya's
own limiter, marked `Voya-Rate-Limited`, has its `Retry-After` waited out
rather than surfaced, within the bound `voya.RoundTripper`'s doc states,
while a service's own unmarked 429 reaches the caller on the first attempt,
as it would in production. `voya.Transport()` is the bare
`*http.Transport` underneath, for code that needs the concrete type; a 429
reaches its caller untouched. Both redirect nothing when `FLY_APP_NAME` is
set, so it all stays one code path.

## Configuring voya

Nothing to do: voya runs with no configuration at all. Its defaults live in
`internal/config/layers/base.pxf`, which is committed and embedded in the
binary, and they are the only copy of those values.

To change one, write just that field to `voya.pxf` in your config directory —
`~/.config/voya/voya.pxf`, or `$XDG_CONFIG_HOME/voya/`, or wherever
`$VOYA_CONFIG_DIR` points:

```pxf
log_level      = LOG_LEVEL_WARN
shutdown_grace = 1s
```

Fields not named there keep their default. The file is optional and absent by
default; a missing one is the normal case, not an error.

`log_level` is a verbosity floor, and each level adds a band to the one below
it:

| level | what reaches the terminal |
|---|---|
| `LOG_LEVEL_ERROR` | failures only — a route the proxy could not serve, a DNS reply it could not send |
| `LOG_LEVEL_WARN` | the above, plus a duplicate registration evicting a live instance, and the reclaim when it takes its name back |
| `LOG_LEVEL_INFO` | the default: the above, plus the startup line, what `voya run` prints on the way out, and the request log's live line — one per request the proxy answers: timestamp, authority, method, path, status, duration, and the upstream port or `voya` when voya answered it itself |
| `LOG_LEVEL_DEBUG` | reads identically to `INFO` today; the per-request routing decisions it is for are not written yet |

The live line is a view of the request log, not the log itself: the record
goes into a bounded in-memory ring buffer — the last 1024 requests, metadata
only, never a payload byte (ADR 003) — that `voya log` reads from any
terminal, and `LOG_LEVEL_WARN` quietens the terminal without touching the
buffer. `request_log = false` is what turns the buffer off, and with it every
line.

```console
$ voya log            # the buffer, oldest first; -n 3 for the last three
$ voya log -f         # the same, then each request as it happens; ^C stops
$ voya log -pxf       # each request as one pxf document per line
$ voya log -no-voya   # leave out requests answered under voya's own voya.internal
```

All of it goes to **stderr**: everything voya says about itself is a
diagnostic, and stdout is reserved for what `voya status`, `voya routes` and
`voya log` print. So `voya run > /dev/null` never swallows a failure, and
`log_level` — not the shell — is how you ask voya to be quieter.

`cors_origins` lists browser origins allowed to read responses through the
proxy, each an exact `scheme://host[:port]` as a browser sends it: leave out
the scheme's default port, and write a non-ASCII host as punycode. `localhost`,
`127.0.0.1`, `[::1]`, `*.localhost` and `*.internal` are always allowed, so a
local frontend needs no entry. Any other website gets no CORS headers (ADR 003,
amendment of 2026-11-01).

`rate_limit` is the local stand-in for the production edge's limiter (ADR
006), off by default. `enabled = true` in your `voya.pxf` is the whole
switch: the proxy then forwards at most `requests` per `window` — 5 per
minute as shipped — **per authority**, so two services never share a budget
and both spellings of one name (`payments.internal`,
`payments.internal:9777`) spend from the same one. The request over the
budget answers 429 with a `Retry-After` in whole seconds and a
`Voya-Rate-Limited` header naming the refusal as voya's own, and whether a
client honours that is the bug the limiter exists to catch locally. A Connect
or gRPC caller reads `resource_exhausted` instead of the status, and where its
shape carries that in `grpc-status` or an end-of-stream envelope — gRPC,
gRPC-Web, a Connect stream — the response is an HTTP 200 with the wait but no
marker: the marker qualifies a 429, and there is none there to qualify (ADR
005). A caller through the client
package's own transport (`voya.Join`'s default, or `voya.RoundTripper()`)
already does: it waits the header out and retries, and a wait its deadline
cannot afford fails at once naming the header's value. Only the marked 429
is voya's to wait out: a service answering 429 as its own verdict is
forwarded verbatim and reaches the caller untouched, exactly as it would in
production, where voya is not on the path. voya's own `voya.internal` is
never throttled, so `voya status` and `voya log` keep working
while you investigate — and `voya health`'s probes spend none of a service's
budget, so the report shows its real state with the limiter on. Only a
request that reaches the service costs it: one the proxy forwarded that
never got there — the port refused the dial, or the caller's deadline ran
out first — is refunded, so a misregistered service keeps answering its
diagnostic instead of a 429 however often a client retries.

`auth` is voya's local identity provider (ADR 006): the `iss` it signs as, the
`aud` it stamps, how long a token lives, and the personas `voya token --as
<name>` mints — `has-write-scope`, `has-read-scope`, `no-scopes` and
`wrong-audience` out of the box, named for the case each one tests and never
for a role. Every persona needs a name and a subject; scopes, an audience of
its own and extra claims are optional. Setting one field keeps the rest;
setting `personas` replaces the whole list, so restate the ones you want.
`personas = []` removes them all, and `voya token` then has nothing to mint:

```pxf
auth {
  issuer   = "http://voya.internal:9777"
  personas = [
    { name = "has-tenant" subject = "u-1" scopes = ["read"] claims = { "tenant": "acme" } }
  ]
}
```

The signing key is not configuration. voya generates `signing-key.pem` in the
same directory the first time anything needs it, mode `0600`, and reuses it
from then on — so a token pasted into a terminal stays valid across restarts.
A key file readable by other users is refused, naming the file and the mode it
found; delete it and voya makes another.

```console
$ voya token --as has-write-scope             # a valid token, alone on stdout
$ voya token --as no-scopes                   # ADR 006's missing-scope case
$ voya token --as wrong-audience              # ... and its wrong-audience case
$ voya token --as has-read-scope --expired    # already expired, all else valid
$ voya token --as has-read-scope --bad-signature
```

The token is the only thing on stdout, so `$(voya token --as has-write-scope)`
is a token; `--as` with a name the config does not have lists the ones it
does. No daemon is needed — signing needs the key, not the listeners. A
service validates against the JWKS the daemon serves at
`http://voya.internal:9777/.well-known/jwks.json`: an RFC 7517 JWK Set that
any stock JOSE library's remote-JWKS path reads, which is the point — the
issuer and JWKS URL are the only things that differ between validating voya's
tokens and the production IdP's (ADR 006).

Reading the daemon from another terminal is one screen and one table, both
answered from memory over loopback — nothing is probed, so they stay instant
whatever state a service is in (ADR 005):

```console
$ voya status         # voya itself, every service, then evictions and reclaims
$ voya routes         # authority -> service -> upstream, sorted by authority
$ voya status -pxf    # the same answers as pxf, for tooling; routes -pxf too
```

`voya status` is where an eviction is on record after the warning has
scrolled away — at every `log_level`, including the one that silences the
warning. A voya that is not running is an answer, not a failure: one line
naming the port and `voya run`, exit 1; a voya that accepts and never answers
is a different one, exit 2.

Probing is opt-in, and a separate column from the stream state routing acts
on — their disagreement is the diagnosis (ADR 005):

```console
$ voya health         # the status screen, HEALTH filled: one grpc.health.v1
                      # Check per service, on demand, through the proxy
$ voya status -health # the same screen; -pxf on either for the HealthReport
```

Each cell is the service's own answer (`serving`, `not_serving`, `unknown`),
`timeout` when it accepted and did not answer within a short bound,
`unreachable` when voya could not reach its port — voya's diagnostic printed
under the table — `throttled` when the service answered
`resource_exhausted` — its own limiter shedding load — or `error` when it
answered no verdict at all. The probes
run at once in the CLI process, none outlives the command, and nothing is
ever evicted on the result: a wedged service reporting `timeout` is the answer,
not a reason to deregister it (ADR 004). Exit 0 whatever the answers.

Calling a service from the terminal is the same one call its neighbours make
— binary protobuf over the proxy, under the callee's own authority — with pxf
on both ends of the terminal side (ADR 003):

```console
$ voya call payments payments.v1.Payments/GetBalance 'account = "acct_1"'
$ echo 'account = "acct_1"' | voya call payments ...   # or stdin, or "-"
$ voya call payments payments.v1.Payments/Ping ''      # '' is the empty message
```

The request word is optional and stdin is what stands in for it, so a
procedure that takes no arguments is called with `''` rather than with
nothing: no word means stdin, and at a terminal that is you, until `^D`.

The schema comes from the callee itself: one `grpc.reflection.v1` request for
this call, through voya under the same name, so a service that serves
reflection can be called by anyone who knows its name and nothing else (ADR
004, amended). `-descriptors FILE` reads a compiled `FileDescriptorSet` and
its whole import closure — `buf build -o`, or `protoc --descriptor_set_out
--include_imports`, which writes the named files alone without that flag —
for a service that serves none. Unary procedures only; a streaming one is refused by name. The reply
document is the only thing on stdout, so it pipes; a Connect error is its
code and message on stderr, exit 1, and voya's own routing diagnostic when it
was voya that answered. A malformed request is a parser error naming the line
and the field, exit 1, before anything is sent; a refusal — a streaming
procedure — and a voya that accepted and never answered are exit 2.

The DNS listener is opt-in wiring for the consumers the client dialer does
not cover — `curl`, `grpcurl`, Postman, browsers — and it takes one file,
which voya writes and removes (ADR 002):

```console
$ sudo voya dns install    # -> 127.0.0.1:<dns_port>, and on Linux restarts resolved
$ voya dns check           # the system path reads `working`
$ sudo voya dns uninstall  # remove exactly that file, nothing else
```

Which file, and what else has to happen, is the OS's:

| | macOS | Linux |
|---|---|---|
| the file | `/etc/resolver/internal` | `/etc/systemd/resolved.conf.d/voya.conf` |
| what it says | `nameserver 127.0.0.1`, `port <dns_port>` | `[Resolve]`, `DNS=127.0.0.1:<dns_port>`, `Domains=~internal` |
| what reads it | configd, as the file lands | systemd-resolved, at startup — so a changed file is followed by `systemctl restart systemd-resolved` |
| what has to be true first | nothing | systemd-resolved active; v246 or newer (where a port in `DNS=` became legal); and no global `Domains=~.` — a catch-all routing domain sends `.internal` to whichever global server answers first, so `install` refuses rather than write a file this host cannot use |

The file names the `dns_port` in your configuration, so a moved port means
running `dns install` again. Both commands are idempotent and print one line
saying what they did — including which file that `dns_port` came from, since
`sudo` decides what `$HOME` is and a default port installed against a
configured one is otherwise invisible. An identical file is left alone and
reported as already installed — and on Linux not restarted, since a restart is
a live change to how the machine resolves every name; `--activate` restarts
systemd-resolved whatever the outcome, for a file that is right but was never
read (a restart that failed, a file restored from a backup or copied from
another machine), on `install` and `uninstall` alike. A file voya wrote for
another port is rewritten and the change reported, and a file voya did not
write — one without the `# written by voya dns install` first line — is
refused with its contents shown,
by `install` and `uninstall` alike: you wrote it, voya does not touch it, and
removing it yourself is what hands the file to voya. No file is
nothing to uninstall, exit 0. Without root neither command tries and fails;
each says the `sudo` line to run and exits 1. `sudo` resets most of the
environment but macOS's keeps `HOME`, so `~/.config/voya/voya.pxf` is read as
usual; a config directory named by `$VOYA_CONFIG_DIR` or `$XDG_CONFIG_HOME`
needs passing through (`sudo VOYA_CONFIG_DIR=… voya dns install`), and on
Linux `$HOME` needs it too wherever `sudo` is set to reset it — the install
line says which file it read, so you can check.

On an OS with neither file both commands say the wiring is not specified yet
([#155](https://github.com/trendvidia/voya/issues/155)) and exit 2; nothing is
written. A Linux host not running systemd-resolved gets the same exit 2 from
`install`, with what to do instead: send `*.internal` to voya's DNS listener
with whatever that host does resolve through (`dnsmasq`:
`server=/internal/127.0.0.1#<dns_port>`), or skip DNS and dial through the
voya client, which needs none of it. So does a Linux host that **runs**
systemd-resolved while its own name resolution does not go through it
([#186](https://github.com/trendvidia/voya/issues/186)) — a static
`/etc/resolv.conf` naming real upstream servers, `dnsmasq` or `resolvconf` in
front of resolved, NetworkManager's `dns=default`, the default of many server
images and containers. There resolved would read voya's drop-in and nobody
would ever ask resolved, so `install` refuses before writing, exit 2, and says
how to point the system at resolved (`ln -sf
/run/systemd/resolve/stub-resolv.conf /etc/resolv.conf`, or NetworkManager's
`dns=systemd-resolved`) or how to wire `.internal` around it. What counts as
resolving through resolved is any of: `/etc/resolv.conf` a symlink into
`/run/systemd/resolve/` (either of resolved's two files), `/etc/resolv.conf`
naming `127.0.0.53`, or `resolve` on `/etc/nsswitch.conf`'s `hosts:` line — a
disjunction, because every shape it missed would be a working host refused.
The one it cannot tell apart is a *copy* of resolved's uplink file: upstreams
by address, no stub, no `nss-resolve`. That host is refused too, and the
refusal says how to point it at resolved. `uninstall` asks no such question —
a file voya wrote is voya's to take back whether or not resolved is still
there.

One Linux caveat the install cannot check for you, ending in the same place —
the file is written, and `*.internal` still does not resolve:

- **A global `DNS=` of the host's own.** `DNS=` accumulates across
  `resolved.conf` and its drop-ins into one global list, so resolved asks that
  server first and falls to voya's only on failure or timeout — never on the
  NXDOMAIN it gets for `*.internal`. voya will not reset the list (that would
  take the host's own resolvers away from every other name).

There the host wires `.internal` as a host without resolved does, and
`voya dns check` is what names it: it reports the system path not answering
with the file installed.

Everything voya validates before binding, it explains — which field, which
value, and which layer set it:

```
config: proxy_port and dns_port are both 9777 — voya's three listeners each need their own port
  proxy_port = 9777, set by layer "base" (built into the binary)
  dns_port = 9777, set by layer "user" (/Users/you/.config/voya/voya.pxf)
```

## Conventions

Configuration is [chameleon](https://github.com/trendvidia/chameleon); the
config schema is a `.proto`, resolved from layered `.pxf` files. Serialization
is [protowire](https://github.com/trendvidia/protowire-go) — `encoding/pb` on
the wire, `encoding/pxf` for anything a human reads. **voya's own configuration
and data are never TOML, YAML or JSON.** Another format appears only at a
boundary, where the system on the far side of it defines the format — which is
why what turns up there is not always one of those three. In the repo:
the tool configs (`.golangci.yml`, `.github/workflows/ci.yml`,
`proto/buf.yaml`, `proto/buf.gen.yaml`). Outside
it: the `fly.toml` each service ships, and the two files `voya dns install`
writes — the `resolver(5)` file at `/etc/resolver/internal` and the
systemd-resolved drop-in at `/etc/systemd/resolved.conf.d/voya.conf`, each its
OS's own format and neither one of the three.

Schemas live in `proto/` and their Go bindings are **committed**, because
`go install` runs no code generator. Regenerate with `make generate`, which
pins buf and both protoc plugins into `./bin`; `make proto-check` fails if what
is committed is not what the pinned toolchain produces.

`voya.registry.v1` is a contract other repos link, so `make proto-breaking`
compares `proto/` against `origin/main` under buf's strictest rule set
(`FILE`): renaming a message is a break here, not just renumbering a field,
because the bindings are consumed as Go and a rename that survives the wire
still breaks the fleet's build. `voya.diag.v1` sits under the same rule —
consumed as Go by voya's own CLIs, and landed whole in one change for that
reason (#149) rather than given a per-package exception to explain.

## Design

The ADRs record what was decided **and what was rejected**, which is usually
the more useful half:

| ADR | Subject |
|---|---|
| [000](docs/adr/000-production-substrate.md) | The production substrate is Fly.io |
| [001](docs/adr/001-core-architecture.md) | Single Go binary, authority-routed local proxy |
| [002](docs/adr/002-service-discovery.md) | Ephemeral ports, authority routing, wildcard DNS |
| [003](docs/adr/003-protocol-support.md) | Connect-first, no transcoding in the proxy |
| [004](docs/adr/004-service-lifecycle.md) | Registration stream, and what happens when it drops |
| [005](docs/adr/005-observability.md) | Diagnosis at the point of failure |
| [006](docs/adr/006-edge-layering.md) | Auth belongs in the service, not the edge |

## License

voya is released under the [MIT License](LICENSE). See
[`CONTRIBUTING.md`](CONTRIBUTING.md) to contribute and
[`SECURITY.md`](SECURITY.md) to report a vulnerability.
