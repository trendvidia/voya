# ADR 006: Edge Layering — Local Reproducibility Decides Placement

**Status:** Accepted (the edge component itself is deferred)
**Date:** 2026-08-08
**Relates to:** [003](003-protocol-support.md), [004](004-service-lifecycle.md),
[005](005-observability.md)

## Context

Every platform eventually needs public APIs, authentication, rate limits, API
keys, and quotas. Ours will too — after v1 is stable, not now. The seam between
that future edge and today's design is worth deciding while it is still free.

Two risks are obvious. First, the edge might duplicate work services already do:
an API gateway that *also* transcodes REST to RPC would give us two binding
implementations — its own and Vanguard's (ADR 003) — that must agree forever and
will not. Second, edge policy might exist only in production, so rules that are
never exercised locally break at deploy.

The second risk is not hypothetical and not evenly distributed. **Authentication
and authorization are the most frequent and most opaque source of bugs in a
system like this, and chasing them in staging or production is unproductive.**
That observation is what actually decides this ADR. Conventional wisdom puts
authn "at the edge"; conventional wisdom is optimizing for deployment topology,
not for how long it takes to find a bug.

## Decision

### The governing principle

> **If a class of bug is expensive to debug remotely, it must be reproducible
> locally.** This is a requirement, and it outranks conventional placement.

Every responsibility below is sorted by this rule, not by where the industry
usually puts it.

### The seam: binding in the service, policy at the edge

Protocol binding — REST URL to RPC method, path segments to request fields —
derives from the service's own schema, versions with it, and deploys with it. It
stays in Vanguard, in-process (ADR 003).

The edge applies policy and **never re-binds**: it receives `GET /v1/orders/123`,
applies its rules, and forwards it unchanged to a service that already serves
REST natively. The edge holds no descriptors and is never redeployed because a
service added a field.

### Authentication and authorization both live in the service

Token validation is a **library, not a location**. A shared Go package turns a
token into a `Principal`; a Connect interceptor runs it. Whoever imports it runs
it — which means it runs identically in dev and prod.

This puts signature verification, expiry and audience checks, claims-to-principal
mapping, scope derivation, and tenant resolution on the developer's laptop, where
they belong, because that is the code that breaks.

Authorization — may this principal perform this operation — depends on domain
state and was never an edge concern. It sits in an interceptor beside authn.

### voya is a local identity provider

Not a header injector. voya:

* generates a signing key on first run and serves a **JWKS endpoint**;
* mints **real, signed JWTs** via `voya token --as <persona>`, with the persona's
  claims, scopes, and audience drawn from chameleon config;
* can mint **deliberately invalid** tokens — expired, wrong audience, missing
  scope, bad signature — so failure paths are testable rather than theoretical.

Services validate against voya's JWKS locally and the real IdP in production. The
only thing that differs between environments is the issuer and JWKS URL: one
chameleon-resolved config value. The validation code path is identical.

Persona switching survives from the previous design — testing a 403 stays a
one-flag change — but it now exercises the real machinery instead of bypassing
it.

### What the edge keeps

TLS termination (mostly fly-proxy's job already), rate limiting, quotas, WAF,
and *optionally* fast-fail rejection of obviously-bad tokens as cheap protection
against garbage traffic. That last one is defence in depth and **never the source
of truth** — the service's verdict is authoritative.

### Rate limiting runs locally too, with dev thresholds

Applying the principle rather than waving at staging: run the real limiter
locally with a small threshold — say five requests per minute. Production-scale
behaviour is not reproducible on a laptop, but the bugs are not there. They are
in whether the *client* handles 429 and honours `Retry-After`, and that is fully
testable locally.

### What genuinely is not reproducible locally

Named explicitly, because a short honest list is worth more than a pretence of
full parity:

* **IdP integration** — the actual Auth0/Clerk/etc. handshake. Irreducibly
  external.
* **Quota accounting across instances** — needs shared state and real traffic.
* **WAF rules** — meaningful only against real hostile traffic.

### Forward compatibility

* **REST paths carry a version prefix** (`/v1/...`) from the first endpoint, so
  the edge can route by prefix later without touching services.
* **Annotations remain the only source of routes.** A route must never exist
  solely in edge configuration, or the generated spec stops describing the
  running system — the failure this project already found once in `@http`
  (ADR 003), one layer up.

## Alternatives considered

**1. Edge-injected trusted identity header, stubbed locally by voya.** *(the
first version of this ADR)* Rejected. Injecting a principal directly bypasses
signature verification, claims mapping, and scope derivation — precisely where
auth bugs live — so the most bug-prone subsystem in the platform would have been
exercised only in staging. It also required a deliberate authentication bypass
(`X-Voya-As`) whose safety rested on two invariants — the edge stripping inbound
headers, and services never having a public IP — that would have to hold forever,
with a full auth bypass as the failure mode.

**2. An edge that also transcodes** (Envoy plus `grpc_json_transcoder`).
Rejected. Two binding implementations that must agree forever; the
descriptor-merge hazard documented in ADR 003, where one service compiled against
a different `genproto` vintage breaks REST for every service; and an edge
redeploy on every schema change. It also buys no parity this design lacks — both
schemes have identical dev and prod code paths.

**3. Run the real IdP locally** (a shared dev tenant). Rejected: a network
dependency in the inner loop, shared mutable state between developers, and dev
tenants have their own rate limits. voya-as-IdP has none of those and needs no
account.

**4. Stub or mock the validation library in dev.** Rejected for the same reason
as alternative 1 — the validation *is* the thing that breaks. A mock proves
nothing about the code that will run in production.

**5. No local identity; services allow-all in dev.** Rejected. Authorization
paths would go entirely unexercised until deploy.

## Consequences

* **Positive:** Signature verification, claims mapping, and scope derivation run
  locally on every request, not in staging.
* **Positive:** Failure paths become testable. Expired, wrong-audience, and
  missing-scope tokens are one flag away, so the half of auth that nobody tests
  gets tested.
* **Positive:** The authentication bypass is gone. With no trusted-header model,
  services are safe even if directly reachable — zero-trust rather than
  perimeter — and the two security invariants from the previous version stop
  being load-bearing.
* **Positive:** The edge stays thin and swappable: Envoy, Fly's proxy, a small Go
  service, or a commercial gateway are interchangeable, because it is only
  policy.
* **Negative:** Every service now validates tokens, so every service needs the
  shared library and JWKS caching, and key rotation must propagate. Cheap in Go,
  but real in operations.
* **Negative — the sharp edge:** voya holds a signing key, and **a service that
  accepted both issuers would accept voya-signed tokens in production.** The
  issuer allowlist must therefore be single-valued and environment-resolved, and
  a production build configured with voya's issuer must **fail closed at
  startup**. This replaces the previous version's two topology invariants with a
  single config assertion, which is both narrower and testable.
* **Negative:** IdP integration remains unexercised locally. Small, named, and
  accepted.

## Personas are fixtures, not a model

**Resolved 2026-08-08: personas are test fixtures describing cases, not a mirror
of the live authorization model.**

The alternative — deriving personas from the same role definitions services
compile against — makes drift impossible, but also makes personas useless for the
work they exist to support. A derived persona can only express a state the
current model permits, so it cannot reproduce a bug report from an older version,
a principal carrying a scope combination the model no longer issues, or the
malformed identity that caused an incident. Fixtures can express all of those.
That is the point of them.

The accepted cost is that a fixture can describe a principal no real user could
ever be — a `viewer` carrying a scope the real viewer role does not grant.
Testing against it proves the code works for someone who does not exist.

Note the asymmetry, because it decides how bad this is: the *opposite* direction
is safe. A fixture that lacks a newly required scope fails loudly, locally, on
the next run — which is the outcome we want. Only over-permissive fixtures
mislead, and they mislead by passing.

Two cheap habits keep this honest without giving up the flexibility:

* **Name fixtures after cases, not roles** — `has-write-scope`, `no-scopes`,
  `expired-token`, `wrong-audience`, rather than `admin` and `viewer`. The name
  should make it self-evident that the thing is a test input, not a description
  of production. Role-shaped names are what invite the mistake.
* **Real tokens remain the backstop** for "does this match an actual user."
  That question is answered in staging against the real IdP. This ADR does not
  pretend otherwise — it claims only that *auth mechanics* are locally
  reproducible, not that the identity model is.

## Amendment 2026-08-19 (issue #208): the signing key is on-disk state, and where it lives

The decision above says voya *"generates a signing key on first run"*. "First
run" is persistence: a key regenerated on every `voya run` would silently
invalidate every token a developer already pasted into a terminal, and the
symptom — a 401 from a service that was working a minute ago — reads as a bug
in the service, which is exactly the class of confusion this ADR exists to
remove. So the key is written to disk, and this amendment records where, how,
and why that is not the on-disk state ADR 004 refused.

**Where.** `signing-key.pem` in the configuration directory — `$VOYA_CONFIG_DIR`,
else `$XDG_CONFIG_HOME/voya`, else `~/.config/voya`, the same resolution
`internal/config.Dir` gives `voya.pxf`. Beside the config rather than in a
cache or state directory so that two configurations never share a key and
`$VOYA_CONFIG_DIR` moves both together. It is the first thing voya writes to
that directory, which it otherwise only reads, and the directory is created on
the way when it does not exist — "voya runs with no configuration at all" still
holds. When no directory can be determined at all (`Dir` answers `""`, which
`Load` reads as "no user layer"), key generation is an error naming
`$VOYA_CONFIG_DIR` as the remedy, and never a key in the process working
directory.

**How.** RS256, 2048 bits, one PKCS #8 PEM block — the algorithm and the
encoding every third-party JOSE library and `openssl pkey` read unmodified,
which is the whole point of minting real tokens. The file is created at mode
`0600` and **a wider mode is refused**, naming the file and the mode it found:
a signing key readable by other local users is this ADR's sharp edge pointed
at the developer's own machine. A key **owned by another uid** is refused for
the same reason — the mode bits only protect the key if voya is the user they
name, and a configuration directory voya did not create can be one another
local user writes to. A file that exists but is not a key — truncated, empty,
a directory, a symlink whose target is not there — is refused too, naming the
path, because the remedy is to delete it; voya does not silently replace a
file it did not understand. Generation happens **on first use, not on first `run`** *(the daemon half of
this sentence is superseded by the #219 amendment below: `Start` now mints
once before serving)*: `voya token` must work without a daemon, so the key
belongs to a shared package (`internal/auth`) that the daemon's JWKS endpoint
and the CLI both call. Two processes generating at once — `voya run` and `voya token`
in another terminal on a fresh machine — each write a private temporary file
and then link it into place; the link is atomic and fails for whichever comes
second, which discards its key and reads the winner's. No path leaves a
half-written key, and every caller holds the key that is on disk.

**Why ADR 004's refusal does not apply.** ADR 004 rejected persisting the
registry because *"a stale on-disk registry is a new failure mode that outlives
the process that created it"*: the registry is a cache of live state, and a
copy that survives the process can disagree with the world. The key is the
opposite shape. It is not a cache of anything — there is no truer copy
elsewhere for the file to be stale against — and its surviving the process is
the requirement, not the hazard: the failure mode is *regenerating* it, which
is what the in-memory design ADR 004 chose would do on every start. The two
decisions share a rule and reach opposite conclusions from it: state that is
derived from something live stays in memory; state that *is* the thing stays
on disk. ADR 006 is the later and more specific decision and wins for the
key; ADR 004 stands unchanged for the registry.

Rotation is not decided here. The file can be deleted and voya makes another,
which is the whole of the mechanism today; every token minted with the old
key then stops validating, and the message that refuses a bad key file says
so. A rotation that overlaps keys needs the JWKS endpoint serving more than
one, which is that issue's to weigh.

## Amendment 2026-08-19 (issue #219): the key is minted when the daemon starts, and a start that cannot secure one fails

The amendment above said generation happens *"on first use, not on first
`run`"*, and the implementation followed it to its edge: on a fresh machine
the first `GET /.well-known/jwks.json` — an unauthenticated request on a
loopback port — was what created the private key file, `voya doctor`'s own
JWKS probe included. This amendment supersedes that sentence's daemon half
and makes the Decision section's *"generates a signing key on first run"*
literally true.

**`voya run` calls the key loader once, before anything serves.** On a fresh
machine the key is on disk at `0600` before the first request arrives, so a
validator's JWKS fetch is a pure read of a key that already
exists, never what mints it. The rest of the first-use design stands
unchanged: `voya token` still generates without a daemon, the atomic-link
race between two generators still converges on one key, and the JWKS handler
still loads per request, so a key deleted mid-run is regenerated rather than
served from a stale copy.

**A key-generation failure is a startup failure — every one, `ErrNoDir`
included.** voya holds signing key material (this ADR's sharp edge), and a
daemon that came up having failed to secure its own key would be a local IdP
that silently is not one; failing closed at startup is the same discipline
this ADR already demands of a production validator pointed at voya's issuer.
The accepted cost is a real regression in reach, recorded deliberately: a key
file voya refuses — checked into a dotfiles repo at `0644`, owned by another
user, a dangling symlink — used to break only auth and now stops `voya run`
entirely, so a developer loses the proxy, DNS and the registry over a file
none of the three use; and a host where no configuration directory resolves
at all (some containers, some CI images, some `sudo` invocations) no longer
runs voya. The startup error therefore names the file — or `$VOYA_CONFIG_DIR`,
for the no-directory case — and the remedy, exactly as the loader's own
refusals do.

## Amendment 2026-08-19 (issue #223): the local limiter counts per authority, and voya's own surface is exempt

"Rate limiting runs locally too" above named the thresholds and left the
limiter's key undecided. Built, it is decided: **the budget is keyed on the
authority the request was routed to** — the routed service name, so
`payments.internal` and `payments.internal:9777` spend from one budget rather
than doubling the limit for a caller who types the port. Five requests per
window to one name is five whoever asks, and two authorities never share a
budget. Rejected: per caller+authority — on loopback the caller is
`127.0.0.1` with a varying port, so that key is really per-connection, and it
buys isolation nobody asked for — and global, which makes the limit a
property of the daemon, so a busy fixture starves an unrelated test.

**The accepted cost of that key:** two of a developer's services calling one
busy name share its budget, which a production edge keyed on the caller would
not do. That is the price of a limit whose behaviour you can predict from the
name you typed.

**The reserved `voya.internal` is never counted.** It is voya's own surface —
DiagService, and the JWKS since #209 — and throttling it would break
`voya status` and `voya log` exactly when a developer is
investigating a throttle. Structurally, the limiter counts **only requests
the proxy would otherwise forward**: the limiter exists to protect an
upstream, and a request refused for routing — the reserved name, a typo, a
name nothing has registered, a known-but-stopped service — never reaches one,
so none of those can exhaust a name's budget.

The refusal itself is ADR 005's: the status-to-code table gains the
`resource_exhausted` / 429 row (its amendment of this date), always with a
`Retry-After` in whole seconds, because whether the *client* honours it is
the bug this limiter exists to make reproducible locally. The knobs are
`rate_limit` in `voya.config.v1.Config` — `enabled`, `requests`, `window`,
fixed windows per authority — shipped **off** in `base.pxf` with the
suggested five-per-minute thresholds in place, so an unconfigured voya
answers exactly as before and turning the limiter on is one line in a user
layer.

## Amendment 2026-08-20 (issue #233): the limiter spends budget only on requests that reach an upstream

The #223 amendment above says the limiter counts **only requests the proxy
would otherwise forward**. Still true, but no longer the whole truth: a
request the proxy *did* forward can still reach nothing — a service that
registered a port nothing listens on refuses every dial — and the gate runs
before the dial, so such a request spent budget that protected nobody. A
client with a retry policy against that misregistration, ADR 005's own
scenario, burned the whole budget on failures and was then told "rate limit"
instead of "your registration is wrong". So: **a forwarded request that reached
no upstream is refunded**, in the proxy's ErrorHandler, where the failure is
discovered. An upstream that answered an error, or accepted the connection
and then broke it, was reached and keeps its charge — that is exactly what
the limiter exists to protect, and refunding it would make the limit
unenforceable against a broken-but-listening service.

Reached is read from whether the transport ever got a connection, through an
httptrace the gate attaches, and deliberately not from the error's shape: a
dial that loses a race with a cancelled request context is reported as the
cancellation, net/http preferring `context.Cause` to the dial error it
already holds, so a rule keyed on a dial `*net.OpError` would keep the charge
for a request that reached nothing — nondeterministically, for any caller
that sets a deadline, which is this amendment's own symptom again. The refund
carries the window token its admission reported, so it can never credit a
window other than the one that was charged: not a successor of a window that
has since reset, and never a count below zero.

**voya's own health probes are exempt, and spend nothing.** `voya health`
and `voya status -health` probe through the proxy under each service's own
authority — deliberately, so a green cell also proves routing — which made
every probe a forwarded request spending the service's budget: at
`base.pxf`'s five per minute, the sixth `voya health` inside a minute
reported a false word for a perfectly healthy service. The probes now carry
a marker header, `Voya-Probe`, and a request carrying it skips the gate; the
proxy strips the header before forwarding, so no fleet service ever sees
voya's internal marker and the probe stays the call a real caller makes.
Any loopback process could send the marker, and that is accepted the way
ADR 002 accepts unauthenticated registration: voya's trust boundary is
loopback and nothing else, and a local process that could forge the
exemption could already register itself under the service's name and take
its traffic — strictly more power than skipping a limiter. One caller has
less power than that and can still forge the marker: a page in the
developer's browser, because voya's CORS preflight reflects whatever headers
a caller asks for and so permits this one. What that caller gains is skipping
a dev-time limiter on a service CORS already let it call, which is inside
what this ADR accepts of the browser surface, not a new reach. The word a
throttled probe would have shown is ADR 005's to fix, and its amendment of
this date gives `resource_exhausted` its own cell, `throttled`.

## Amendment 2026-08-20 (issue #241): the client wrapper retries only voya's own 429s

"Rate limiting runs locally too" above says the bug this limiter exists to
catch is whether the *client* handles 429 and honours `Retry-After`. Built,
that sentence hid an over-reach: the wrapper acted on **every** 429 on the
routed path, and the limiter's is not the only one there. The proxy forwards
an upstream's response verbatim, so a fleet service answering 429 as its own
application verdict had that waited out and retried locally — while in
production, where no voya sits on the path, the same answer reaches the
caller's `resource_exhausted` branch on the first attempt. RFC 9110 says a
429 with `Retry-After` means wait and retry whoever sent it, so honouring
both was defensible; what outweighs it is this ADR's own governing principle
— a class of bug that is expensive to debug remotely must be reproducible
locally — and a caller's handling of a callee's own refusal was exactly the
path the local environment lied about.

So: **the limiter stamps `Voya-Rate-Limited` on the refusals voya writes
itself, and the wrapper waits out only a 429 that carries it.** An unmarked
429 is returned to the caller as sent, `Retry-After` and all — honouring it
is the caller's business, not the transport's. The header is ADR 005's
status-to-code table's to define (its amendment of this date), since that
table is the record of what voya says on the wire; the forwarding path stays
verbatim, so an upstream that stamps the marker on its own 429 gets it
retried by voya's client, the accepted cost of a response marker. Pinned on
both ends: `client/throttle_test.go` proves an unmarked 429 surfaces on the
first attempt, and `test/acceptance`'s
`TestAThrottledCallerWaitsTheRetryAfterAndSucceeds` proves the limiter's own
marked refusal is still waited out — the test that stops this amendment from
quietly disabling #224.

## Amendment 2026-11-02 (issue #308): a Production validator fetches keys over https

"The JWKS URL is the actual trust anchor" is why `authn.New` refuses a
Production JWKS URL in voya's zone. The same reasoning covers the transport,
and the code did not: a Production validator accepted `http://`, so anyone on
the network path could substitute the keys and sign tokens the service then
accepted. So **`authn.New` refuses a Production JWKS URL that is not https, at
startup.** Development is unchanged, because voya's own JWKS is
`http://voya.internal:9777`.

The assumption is that production has no identity provider reachable only over
plain http. Nothing depended on it when this was decided: no project checked out
beside voya required `authn`. The issuer is not held to https, because it is a string
compared with the `iss` claim and is never fetched. Pinned by
`TestProductionRefusesAJWKSURLThatIsNotHTTPS`. A redirect from https to http
during the fetch is not refused.
