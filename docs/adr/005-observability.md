# ADR 005: Observability — Diagnosis at the Point of Failure

**Status:** Accepted
**Date:** 2026-08-08
**Relates to:** [001](001-core-architecture.md), [002](002-service-discovery.md), [004](004-service-lifecycle.md), [006](006-edge-layering.md)

## Context

Dropping Envoy (ADR 001) also dropped its admin interface. `/config_dump`,
`/clusters`, and `/stats` were going to be the answer to "why isn't my route
matching," and nothing replaces them. Since voya's entire justification is
developer experience, a mesh that fails opaquely is worse than no mesh.

The design center here is **not** production observability. There is no
retention requirement, no dashboard, no on-call, and no aggregation across
hosts. There is one developer, at one terminal, mid-loop, who needs to know why
the request they just made did not arrive. Optimizing for a human in that moment
produces a different tool than optimizing for machine consumption.

Production observability is Fly's — Fly logs and metrics, plus whatever the
services emit to a real backend. voya has no production counterpart, so this ADR
is dev-only by construction.

The failure modes worth designing against are the ones the other ADRs already
predict:

1. **DNS not resolving** — `/etc/resolver/internal` missing, or the
   `CGO_ENABLED=0` trap (ADR 002). Symptom: the Go service gets NXDOMAIN while
   `dig` and the browser resolve fine. This is the most confusing failure in the
   whole system.
2. **Service not registered** — never started, crashed, or its stream dropped
   (ADR 004).
3. **Wrong build serving a name** — duplicate registration, resolved in ADR 002
   as evict-and-replace.
4. **Port 9777 already bound** — voya cannot start (ADR 001).
5. **Authority mismatch** — a typo, or a client that does not set `:authority`
   the way we assume.
6. **Protocol mismatch** — a gRPC-wire client exercising the trailer path
   through the proxy, which ADR 003 flags as needing explicit testing.
7. **Service alive but wedged** — paused on a breakpoint; the stream is healthy
   and requests simply hang.

## Decision

**The error response is the primary diagnostic surface.** When the proxy cannot
route, it must never return a bare 502. The body names the `:authority` it
received, states why it failed, and lists the services that *are* registered.
The diagnosis arrives where the developer is already looking — in the response
they just got — rather than in a log they have to think to check.

Supporting surfaces, in priority order:

* **`voya status`** — one screen: voya's own health and port, whether DNS
  resolution is installed, and every registered service with its name,
  ephemeral port, uptime, and stream state.
* **`voya health`** (also `voya status --health`) — probes
  `grpc.health.v1.Health/Check` on every registered service **on demand**, with
  a short timeout, and reports each result alongside its registry entry.

  This does not reopen ADR 004. That ADR rejected probing as an **eviction**
  mechanism, because the interval had no good value and a service paused on a
  breakpoint got evicted mid-debug. Probing as a **diagnostic**, only when a
  human asks, shares none of those properties: no interval, no background
  goroutines, and nothing is ever evicted on the result. A wedged service
  reporting `timeout` is the answer the developer wanted, not a reason to
  deregister it.

  Health is opt-in rather than folded into plain `voya status` for a specific
  reason: `status` reads from memory and must stay instant and infallible. If it
  probed, a single wedged service would hang the one command you reach for when
  something is already wrong.
* **`voya doctor`** — actively tests the known traps rather than reporting
  configuration. In particular it must **resolve `*.internal` from a
  `CGO_ENABLED=0` binary** and report the result, because checking that
  `/etc/resolver/internal` merely *exists* would pass in exactly the case that
  breaks (ADR 002). It also checks who holds 9777 and reports duplicate
  registrations.
* **`voya routes`** — the routing table: authority → service → upstream port.
* **Live request log**, on by default: timestamp, authority, method path,
  status, duration, upstream port. Held in a bounded in-memory ring buffer.

Structured output is **pxf** (`protowire-go/encoding/pxf`) with a human-readable
default rendering. The same data is available programmatically under a reserved
`voya.internal` authority on 9777, so tooling does not have to scrape CLI output.

**voya forwards W3C `traceparent` and `tracestate` unchanged.** It neither
creates nor collects spans, but it must not break propagation — otherwise
trace-context plumbing in the services goes unexercised locally and only fails
once deployed.

## Alternatives considered

**1. Prometheus endpoint plus Grafana.** Rejected. Nobody stands up Grafana to
debug a local mesh, and counters answer "how often" when the question in a debug
loop is always "what happened to *this* request." Aggregates are the wrong shape
for n=1.

**2. OTel traces to a local collector.** Rejected as a component to run, but the
underlying concern is real and is why voya forwards trace headers rather than
stripping them. Services should still export to a real backend in production;
voya simply must not be the thing that breaks the chain.

**3. Reimplementing Envoy's admin interface as the primary surface.** Rejected
as the *primary* surface — `/config_dump` is a reference tool for someone who
already knows Envoy's object model, and it answers "what is the config" rather
than "why did my request fail." The programmatic endpoint under `voya.internal`
covers the legitimate machine-readable case without making a config dump the
first thing a developer reaches for.

**4. Persisting logs to disk with rotation.** Rejected. A dev tool's logs are
worthless after the session that produced them; a bounded ring buffer gives the
same debugging value with no files to manage or clean up.

## Consequences

* **Positive:** The most common failures explain themselves in the response
  body. The error message is the documentation.
* **Positive:** No external dependencies and no backend to run — consistent with
  ADR 001's one-binary goal.
* **Positive:** `voya doctor` gives the `CGO_ENABLED=0` DNS trap a permanent
  home. It is the single hardest failure to diagnose from symptoms, and it is
  now checked actively rather than documented and forgotten.
* **Negative:** No history. A voya restart loses the request log. Accepted for a
  dev tool; the ring buffer covers within-session debugging, which is the
  actual need.
* **Positive:** On-demand health gives the `grpc.health.v1` implementations a
  local consumer. Under ADR 004 alone nothing called them until deploy, so a
  broken health service would first surface as a failing `fly.toml` check in
  production — the exact class of divergence this project exists to remove.
* **Negative:** Two liveness notions now coexist — stream state (what routing
  acts on) and health probe (what the developer reads). They can legitimately
  disagree: a service can hold its stream while failing `Check`. `voya status`
  must show them as distinct columns rather than collapsing them into one
  "up/down", since the disagreement is itself the diagnosis.
* **Negative:** `voya doctor` is a knowledge sink that decays if not fed. Adopt
  the rule that any newly discovered failure mode is either fixed or added to
  `doctor` — never merely noted.
* **Negative:** Request logging on by default costs a little latency and screen
  noise. Bounded and toggleable, and worth it as a default in a dev tool where
  the alternative is discovering you needed logs after the fact.
* **Follows from ADR 002's evict-and-replace policy:** `voya status` must surface
  evictions after the fact, not only as a warning at the moment they happen. The
  developer who needs that information is precisely the one whose instance was
  deposed and who is now debugging a build receiving no traffic — and by then
  the warning has scrolled away.

## Amendment 2026-08-09: the diagnostic has to be protocol-shaped

The decision above makes the error response the primary diagnostic surface. The
M0 spike (#12) showed that is true through `curl` and **false for every Go
caller** — which is the entire fleet.

The proxy wrote exactly what this ADR asks for. Through `curl`:

```
HTTP/1.1 404 Not Found

voya: no service named "paymnets.internal".
Nothing has ever registered that name.
Authority received: "paymnets.internal:9777"

Registered services: billing.internal, payments.internal
Known but not running: reports.internal
```

The same failure, from a `connect-go` client:

```
connect  caller sees: unimplemented: 404 Not Found
grpc     caller sees: unimplemented: HTTP status 404 Not Found
grpc-web caller sees: unimplemented: HTTP status 404 Not Found
```

`connect-go` requires an error body to be JSON and discards anything else
unread (`protocol_connect.go:1378`, `protocol_grpc.go:657`). And its HTTP→code
mapping sends 404 to `unimplemented`, so a typo'd authority reads as *"that
service does not implement this method"* — pointing the developer at the
callee's code to look for a handler that is fine. That is this ADR's own
failure mode, one level up: the diagnosis is written, and the tool that needs
it throws it away.

**Decision: voya writes its diagnostic into the caller's protocol error
envelope where the protocol has one, and plain text otherwise.** A diagnostic
nobody reads is not a diagnostic.

For Connect unary that is `Content-Type: application/json` with
`{"code": …, "message": …}`, and the caller gets the whole text. Adopted for
Connect in #17. gRPC and gRPC-Web carry outcomes in trailers — never in the
HTTP status or an error body — so they get a **Trailers-Only** response
instead (#25): HTTP 200, the caller's own content type, and
`grpc-status`/`grpc-message` in the response headers, which over HTTP/2 is a
single HEADERS frame with END_STREAM. `grpc-message` is percent-encoded per
the gRPC spec, and that encoding is load-bearing: the diagnostic is
multi-line, and net/http will not carry a raw newline in a header value — over
HTTP/2 it drops the whole field, over HTTP/1.1 it rewrites each newline to a
space. Unencoded, the gRPC caller this amendment exists for would read an
empty `grpc-message` and the gRPC-Web one a flattened line.

**Detection of Connect keys on the protocol marker, never on content type.** A
content-type check is wrong in both directions: it misses the GET that
`WithHTTPGet` issues for side-effect-free procedures, which carries no content
type at all, and it claims any `application/json` POST — so a browser or a
webhook hitting a mistyped authority would get a JSON envelope instead of the
human-readable body. Each direction needs two markers, because a single one is
also carried by callers we must not claim: the GET form requires `connect=v1`
*and* `encoding=`, since `connect` is otherwise an ordinary query parameter
name; the POST form requires `Connect-Protocol-Version: 1` *and* a content type
that is not `application/connect+*`, because a streaming caller handed a bare
JSON object where its reader expects a length-prefixed end-of-stream frame gets
a parse error in place of the diagnostic.

**Detection of gRPC and gRPC-Web is the exception where content type is the
right key.** `application/grpc*` and `application/grpc-web*` mark nothing but
those two protocols, and neither has a GET form for the check to miss — the
two failure modes that rule content type out for Connect do not exist here.

**The code follows the routing decision, not the transport.** Every failure
that names a service is `unavailable`, following gRPC's convention for a name
that does not resolve to an endpoint. A request that arrived with **no**
authority is `invalid_argument`: no retry will supply one, and reporting it as
retryable puts a caller with a retry policy into a loop it cannot leave.

**Amendment 2026-08-12 (issue #69): the rule now has two exceptions, so it is
a table.** The paragraph above says *why*; this says what. `internal/proxy`'s
`codeFor` is the single function both protocol writers read it from.

| routing outcome | routing status | gRPC | Connect | retryable |
|---|---|---|---|---|
| no authority at all | 400 | 3 | `invalid_argument` | no |
| not a name any service may register | 400 | 3 | `invalid_argument` | no |
| never registered | 404 | 14 | `unavailable` | yes |
| the reserved `voya.internal` | 501 | 12 | `unimplemented` | **no** |
| registered on voya's own port | 400 | 9 | `failed_precondition` | not until the registration is fixed |
| registered but unreachable | 502 | 14 | `unavailable` | yes |
| known, not running | 503 | 14 | `unavailable` | yes |
| over the rate limit | 429 | 8 | `resource_exhausted` | after `Retry-After`; marked `Voya-Rate-Limited` where the shape's own status is the 429 |

The outcome is the key every other column derives from — since issue #80's
amendment below, it is literally the type the code switches on — and
only the plain-text path writes the routing status as the response's own. A
gRPC or gRPC-Web caller always gets HTTP 200 — the outcome is in
`grpc-status`, which is this ADR's decision two paragraphs above — and the
Connect envelope rides the routing status on every row that answers something
other than `unavailable`; the three `unavailable` rows all carry 503, the
status that code names.

The 501 row is what #69 added. `voya.internal` is this ADR's reserved
diagnostics authority — `validName` guarantees nothing can ever register it,
and no build serves it — so `unavailable` was the retryable answer to a
request no retry can ever satisfy, the same failure the no-authority case is
argued against above. The proxy's message already said "reserved, and this
build does not serve it yet"; the code contradicted it.

Retryability is the axis, because a code is read by a retry policy and not
only by a human. The three `unavailable` rows can come good on their own —
the developer restarts the process. The other four cannot by retrying: three
never — no retry supplies an authority, repairs a name `validName` can never
accept, or makes a build serve the reserved one — and the routing loop not
until a human fixes the fleet's registration.

All three `unavailable` rows collapse together for a Connect or gRPC caller —
the routing statuses they carry, 404, 502 and 503, are one code and one
envelope status between them — and the plain-text path keeps the distinction.
For those callers it lives in the message, which names the port for the 502
and says "no running instance" for the 503.

**Amendment 2026-08-12 (issue #80): the outcome is the key, not the status.**
The table was keyed the way the code was: `ServeHTTP` encoded its routing
decision as the HTTP status, and `codeFor` re-derived the decision from that
int, answering anything it did not recognise from a default — `unavailable`,
the retryable code, the hole 501 sat in until #69. Now the decision is a
value of its own (`internal/proxy`'s `outcome` type), `diagnose` takes it,
and every column — the routing status included — derives from the outcome,
with no default left for a new outcome to land in unnoticed. The
registered-on-voya's-own-port row is the same change made visible: the int
key could not carry the difference between the two 502s, which are different
decisions — a registered port that is voya's own, and a registered port that
refused the connection. They answer alike today, and the type keeps them
apart, so a build that wants to split their answers changes one row rather
than re-deriving one outcome from two causes.

**Amendment 2026-08-14 (issue #91): an outcome with no row answers
`internal`, loudly.** The amendment above left the table with no default row
on purpose, and #80's implementation made an unmapped outcome — a new
constant missing its case in `codeFor`'s switch, which the compiler does not
check, or a stale call site handing the integer-typed outcome parameter a
bare HTTP status, which Go's untyped constants still permit — panic
mid-handler. That reaches the caller as a reset stream with nothing attached,
which is the one outcome this ADR exists to rule out. So the panic goes:
`codeFor` answers an outcome without a row from a fallback that names the
unmapped value on stderr, and the fallback is `internal` / 13 / 500 — not a
retryable code, because an unmapped outcome is a bug in voya and retryability
is precisely what nobody decided; `unavailable` would reopen the loop #69
closed. The no-default rule stands for the *rows*: the fallback is not a row
in this table, decides nothing about the request, and every arrival on it is
a defect by definition, kept unreachable by the test that sweeps the set.

**Amendment 2026-08-14 (issue #88): the table gains a row and amends
another.** A name `registry.ValidName` can never accept — multi-label like
`api.payments.internal`, not `.internal` at all, or a single label no service
may take such as `pay_ments.internal` — used to answer from the
never-registered row, the retryable answer to a request no registration,
restart or retry can ever make good: the same failure the no-authority case
is argued against above, one header's difference, so it gets that row's
codes as its own outcome. **The criterion is `ValidName`, not the shape check
`ServiceFromAuthority` runs**, and those are not the same set: a single label
of the right shape still reaches the registry's naming rule, and a label that
rule refuses can no more come good than a multi-label one. That is why the
proxy asks `ValidName` itself rather than treating "`ServiceFromAuthority`
said no" as the whole answer. And the registered-on-voya's-own-port row, kept
distinct by #80 exactly so an answer that splits it would be one row's
change, is split: the caller's request is fine and the fleet's registration
is not — `isSelf` refuses it identically until the service re-registers the
port it actually bound — so `invalid_argument` would point the developer at
the wrong end of the wire, and `unavailable` reopens the loop #69 closed.
gRPC 9, `failed_precondition`, is defined as "do not retry until the system
state has been explicitly fixed", which is this case verbatim; its routing
status derives from the code — 400 is Connect's canonical status for it —
never the reverse.

**Amendment 2026-08-16 (issue #173): Connect *streaming* gets a shape of its
own, and it always answers 200.** The detection paragraph above says the POST
form requires `Connect-Protocol-Version: 1` *and* a content type that is not
`application/connect+*`, and that is still the rule — for the *unary*
detector. What changed is what the refused half falls to. It fell to plain
text, which a streaming caller never sees: connect-go's streaming client
validates the HTTP status before it reads any of the body
(`connectStreamingClientConn.validateResponse`), so the diagnostic was
discarded unread and the caller was told `unimplemented: HTTP status 404 Not
Found` — this ADR's primary surface, empty, on the one shape `grpcreflect`'s
default client speaks, which is what `voya call` opens with. `isConnectStream`
claims it now, on the same two markers inverted — the version marker says this
is Connect at all, the content type says which half of it — and
`writeConnectStreamError` answers in the protocol's own framing: HTTP 200, the
caller's content type, and one end-of-stream envelope, a flag byte then a
four-byte big-endian length then the same `{code, message}` pair the unary
envelope carries, nested under `error`.

**So the status column has a third exception, and the paragraph under the
table is narrowed to say so.** "Only the plain-text path writes the routing
status as the response's own" now holds for Connect *unary* alone: a gRPC,
gRPC-Web **or Connect-streaming** caller always gets HTTP 200 and reads the
outcome from `grpc-status` or from the envelope's `code`, which for those
three was always the column that answered. No row of the table moves — what a
row names is what the envelope carries.

The content type voya writes back is the caller's own, rebuilt from the
parsed media type rather than echoed: the request is claimed through
`mime.ParseMediaType`, which lowercases and drops parameters, while connect-go
validates the response's as a raw string. Echoing
`application/connect+proto; charset=utf-8` back verbatim would lose the
diagnostic all over again, one keystroke off canonical.

Three shapes still keep none of their own, deliberately. A content type that
is absent or unparseable is claimed by neither Connect detector, for the
reason the detection paragraph gives: guessing the framing wrong turns the
diagnostic into a parse error, and an unreadable answer is worse than an
unshaped one. Connect GET has no streaming form — the protocol sends only
side-effect-free unary calls that way. And gRPC-Web streaming answers from
the Trailers-Only path with the rest of gRPC, which reads a header-carried
status as a complete outcome.

**This does not reopen ADR 003.** voya inspects request markers — headers and
query parameters — never payloads. It still holds no descriptors, decodes no
messages, and translates no protocols. It writes its own error in an envelope
the caller can read, which it must do anyway, since it is the party failing.

Rejected: **leaving it**, which means the fleet's own clients are the one
audience this ADR does not serve, and the failure looks like a bug in the
callee; and **moving the diagnostic to a log line**, which contradicts the
premise that the diagnosis should arrive where the developer is already
looking.

The gap is pinned by `TestDiagnosticIsInvisibleToConnectClients` in
`test/roundtrip/`, which asserts the broken behaviour verbatim, and the remedy
is demonstrated beside it. Those tests fail when the behaviour changes, which
is the signal to delete them — as #25 did for the gRPC and gRPC-Web rows,
leaving the Connect one as the spike's record of why the envelope had to exist
at all.

**Amendment 2026-08-19 (issue #223): the table gains the rate-limit row.**
ADR 006's local limiter refuses through this diagnostic path like every other
outcome, not by hand: a Connect or gRPC caller reads `resource_exhausted` /
8 — gRPC's own name for a spent budget — under Connect's canonical 429, and a
plain-text caller reads the 429 itself. The retryable column gets its first
timed answer: the response always carries a `Retry-After` header in whole
seconds, always positive and never longer than the window the caller is
waiting out — rounded up, so a window that is not a whole number of seconds
(`100ms`, `1500ms`) answers with the next second instead, which over-waits
the caller rather than naming a moment still inside the window that refused.
The header exists because honouring it is the caller bug ADR 006 built the
limiter to catch. The refusal is voya's own
answer, so it is recorded in the request log like any other. What the limiter
counts and what it exempts — per authority, `voya.internal` never — is ADR
006's amendment of the same date, not this table's subject.

**Amendment 2026-08-19 (issue #225): a REST caller is the fourth shape, and
the plain-text fall-through is its answer — the table gains no row.** The
shapes above were built for Connect, gRPC and gRPC-Web callers; ADR 003's
Vanguard adoption adds a fourth, and nobody had looked at what it gets. The
answer was already right, and this amendment records that it is right *by
design* rather than by luck: a REST caller — `curl`, Postman, a browser URL
bar — sends a GET with no `connect=v1`/`encoding=` pair and no
`application/grpc*` content type, so every detector above declines it and it
falls to plain text, which is exactly what a human behind such a caller
reads. One shape of REST URL does carry that pair, and it is named here
rather than left to be found: a route whose request message binds query
fields *called* `connect` and `encoding` produces `?connect=v1&encoding=json`,
which is a Connect GET at the framing voya inspects — so voya claims it and
answers with the envelope's 503 instead. Separating the two would need the
descriptors ADR 003 keeps out of voya, so it stands as a documented
divergence, pinned like the rule it bends. The fall-through is also the one
shape that keeps the routing status as the response's own, so a REST caller
keeps the 404/502/503 distinction the protocol envelopes collapse. Every
column of the table already derives from the routing outcome, not from the
caller's shape, so no row is added and none moves. Pinned in
`test/acceptance/rest_test.go`
(`TestARESTCallToAnUnregisteredNameReadsAsPlainText`,
`TestARESTCallToAGoneInstanceIsExplained`, and
`TestARESTQueryNamedLikeAConnectGETIsClaimedAsConnect` for the divergence),
which is what notices if a future detector starts claiming the shape.

**Amendment 2026-08-20 (issue #241): the rate-limit row is marked as voya's
own.** The limiter's 429 carries a second header, `Voya-Rate-Limited`, set
beside `Retry-After` on every response shape, and the table's row now says
so — this table is the governing record of what voya writes on the wire. It
exists because voya is not the only thing on the routed path that can answer
429: the proxy forwards an upstream's response verbatim, so a fleet service's
own application 429 — an over-quota tenant, say — is indistinguishable from
the limiter's without it, and the client wrapper was waiting out both. In
production no voya sits on the path and that 429 reaches the caller, so the
local environment was lying about exactly the code path ADR 006 exists to
make testable; the wrapper now retries only a marked 429 and surfaces the
rest on the first attempt (ADR 006's amendment of this date). The accepted
cost of a response marker: an upstream that stamps the header on its own 429
gets it retried by voya's client — not a security boundary (loopback,
ADR 002), but named here rather than discovered. Pinned by
`TestOverTheThresholdAnswers429WithRetryAfter` and
`TestAForwardedUpstream429CarriesNoMarker` in `internal/proxy`.

**Amendment 2026-08-20 (issue #251): the marker rides only the shapes that
answer 429, and browsers can read it.** Two corrections to the amendment
above. First, "on every response shape" was wrong to build: the marker's
meaning is *"this 429 is voya's own"*, and on a gRPC or gRPC-Web caller's
Trailers-Only refusal — or a Connect stream's end-of-stream envelope — the
response's own status is HTTP 200 with the outcome in `grpc-status` or the
envelope, so there is no 429 for the marker to qualify. The marker is now
set where a 429 status is actually written: the plain-text response and the
Connect unary envelope carry it, the two HTTP-200 shapes do not. This keeps
the header literal rather than conventional, and changes nothing for voya's
client wrapper, which keys the marker off a 429 status and never saw the
200 shapes as rate-limited at all. `Retry-After` deliberately still rides
every shape, the 200 shapes included — a caller that reads `grpc-status`
can still use a wait hint. The cost is named rather than left to be
discovered: on those two shapes voya's own refusal is now indistinguishable
on the wire from a fleet service's `resource_exhausted` forwarded verbatim —
same HTTP 200, same code, `Retry-After` on both — so a caller that needs the
distinction has to speak a shape whose status carries it. Accepted, because
the alternative is a header asserting a 429 that is not in the response.
Second, the marker joins
`Access-Control-Expose-Headers` beside `Retry-After`: the two are one
signal — the wait, and whose refusal it is — and neither is
CORS-safelisted, so exposing only the wait left a `fetch()` caller (the
audience ADR 006 names for the local limiter) able to see how long to wait
but not whether the wait is voya's to give. Pinned by
`TestTheMarkerRidesOnlyTheShapesThatAnswer429` and
`TestABrowserCallerCanReadTheRateLimitMarker` in `internal/proxy`.

## Amendment 2026-08-11: voya speaks on stderr, stdout carries command output

Levels arrived with #43: every message voya writes has a severity, and
`log_level` is a floor below which nothing is printed. What did not arrive with
them was a destination — `voya run` wrote every band to stdout, including the
proxy's and the DNS server's failures and both `http.Server`s' `ErrorLog`s.

So `voya run > /dev/null`, which is the reflex for "stop printing the banner
and the announcements", also discarded `voya: proxying %q: %v` and `voya: dns:
replying to %s: %v` — the lines a developer redirecting output still wants.
The other redirection was worse: `main`'s fatal path already wrote to stderr,
so `voya run 2>/dev/null` kept every routine line and discarded only the
startup failure, which is the one line nobody would choose to lose. voya's
output was split across two streams by which layer produced it rather than by
what it was.

**Everything voya says goes to stderr. stdout carries command output only.**

`voya run` has no product on stdout: the banner, ADR 002's two announcements
and every failure are all diagnostics, which is what stderr is for. The stream
that is left is reserved for what `voya status` and `voya routes` print, so
`voya status | jq` is honest when M2 lands, and the request log and the `pxf`
rendering the Decision above calls for have somewhere to go that a developer
can pipe.

**Rejected: splitting by severity** — `LOG_LEVEL_ERROR` to stderr and the rest
to stdout. It is the split the levels make possible, and it keeps `voya run >
/dev/null` printing failures. Three things against it. `Start` takes one
`logf func(string, ...any)`, so a per-band destination needs two functions or a
sink built by the caller, which is an API change to the one seam the acceptance
suite drives. Both test suites capture a single `logf`, so it also means
deciding what they capture. And it would leave the announcements on stdout,
where `voya status | jq` has to contend with them anyway.

Being quiet is `log_level`'s job, not the shell's — it is the setting that can
tell an announcement from noise, and ADR 002's obligation is met at every level
above `LOG_LEVEL_ERROR`.

## Amendment 2026-08-14: `voya doctor` carries a verdict, and ships DNS-only first

`voya doctor` landed with the DNS check alone (#6). The registry-backed checks
the Decision above also asks of it — who holds 9777, and duplicate
registrations — need the registry that arrives beside `voya status`, so they
land with M2's doctor work (#19). The command's own output admits this on
every run, because a partial doctor that does not say it is partial reads as a
finished one.

Each check reports two axes (#99). The **state** is what the probes measured,
and stays that even when the measurement cannot settle the question — a
cgo-enabled binary resolving `*.internal` still reports `working`. The
**verdict** — `ok`, `problem`, `unknown` — is whether that evidence can answer
the question this ADR asks, which needs the system path measured and the
measuring binary provably `CGO_ENABLED=0`. Splitting the axes is what stops
every future probe that can be unavailable from inventing an escape-hatch
state: "could not check" is `unknown` beside whatever *was* measured, never a
fifth state that discards it. So where nothing reads the wiring file ADR 002
specifies — an OS it names none for, or a Linux not running systemd-resolved
(#155) — every DNS verdict is `unknown`, reported with what was measured and
without prescribing a file nothing there reads (#96).

**Amendment 2026-08-16 (#155):** the system path is probed on Linux too, with
`getent ahosts` rather than `resolvectl query`: getent goes through NSS, which
is the path `getaddrinfo` takes and therefore the one curl and browsers take,
where resolvectl reaches systemd-resolved over D-Bus and would answer for a
mechanism the system's own resolution might not be using. getent's exit 2 —
"not found by any name service" — is a **measurement** and not a tool failure,
which matters because it is the answer an unwired host gives and treating it
as unmeasured would cost the report its whole Linux verdict. An OS with no
probe at all stays unmeasured, unchanged (#96).

Doctor's exit code keys on the verdict, never the state: `ok` exits 0,
`problem` 1, `unknown` 2. Three codes, because a check that could not be run
is not a pass and not a failure, and collapsing it into either would rebuild
the bug this amendment removes one level up, in a shell script. This is not
the status-to-code table above (amendment 2026-08-12): that one maps proxy
routing outcomes to gRPC and Connect codes for a caller, and the two tables
share no rows.

**Amendment 2026-10-31 (#296):** a missing resolver file alone is not a
`problem`. ADR 002 makes the file opt-in and says no Go service needs it, so a
machine without it is a correct setup, and reporting `problem` and exiting 1
there sent people looking for a fault that did not exist. The state stays
`resolver-not-installed`, because that is what was measured. The verdict is
`ok` if every probe was measured and none answered `*.internal`, and `unknown`
if a probe went unmeasured. If anything answered without the file, the verdict
stays `problem`, because that is an unexplained resolver and not an absence.
The `CGO_ENABLED=0` condition above does not apply to the `ok` here: it decides
whether a binary can vouch for an installed path, and nothing is installed.
A file that is installed and does not work is `problem` as before.
`TestDoctorIsCleanWithoutTheOptionalResolverFile` and `TestClassifyDNS` pin
this.

## Amendment 2026-08-14 (issue #102): doctor ships the human rendering alone, for now

The Decision says structured output is pxf with a human-readable default
rendering, and that the same data is reachable programmatically under
`voya.internal` on 9777. `voya doctor` ships **only** the human rendering. The
pxf report and the programmatic surface are deferred to M2's doctor work
(#19), and this records why, because a deferral nobody wrote down reads later
as an oversight.

A committed `voya.doctor.v1` falls under `proto-breaking`'s `FILE` rule set the
moment it lands — the strictest set, chosen deliberately but for a different
schema: `proto/buf.yaml` argues it from `voya.registry.v1` being a contract
other repos link as Go. The gate is module-wide, so a doctor schema inherits
that strictness without the reason for it.

The amendment above is what turns that from a cost into a problem: doctor is
**deliberately partial**, DNS-only, with the registry-backed checks arriving
later. Finishing it is not an *addition* to today's shape, which is the case
`FILE` would wave through — a new field or a new message breaks nothing under
it. `DNSReport` is a **one-check report**: a state, a verdict and a detail,
flat at the root, because there is one check. Three checks need those per check
plus a verdict aggregated over them, which moves the existing fields rather
than adding beside them — the rename-and-delete that is exactly what `FILE`
catches. Freezing a report shape now would therefore make *finishing* doctor a
breaking change to a schema nothing consumes, with the gate working exactly as
designed against work the same ADR asks for.

So the shape is decided once, for `doctor`, `status` and `routes` together,
when M2 brings the registry those three share. Deciding it now would be
deciding it three times, twice without the information.

What makes the deferral safe is that nothing parses what would change: the
report's *shape* is read by the human rendering alone, and that is Go, not a
wire format. Doctor's exit code is the one machine-readable thing the command
already publishes, and it is not an exception to this. It reads a single enum
whose three values the amendment above fixed and the changelog published, and
it answers the same whatever the report grows around it — so the shell script
that amendment exists for is not a consumer of the shape, and does not put the
deferral in breach on the day it lands.

The shape is what is deferred, so a reader of the shape is what ends the
deferral — **the first non-human consumer of the report itself is the
trigger**, whether it arrives with M2 or before it. This amendment defers
*when* the pxf surface is paid for, never *whether*; the Decision's rule stands
unchanged, and a doctor that grows such a consumer without a schema is in
breach of it.

## Amendment 2026-08-15 (issue #149): the programmatic surface exists — `voya.diag.v1` under `voya.internal`

The Decision's "same data available programmatically under a reserved
`voya.internal` authority on 9777" is built. The daemon serves
`voya.diag.v1.DiagService` there, on the proxy port, through the same
authority routing every service takes: `GetStatus` (voya itself, every
registered service, and the after-the-fact record of evictions **and
reclaims** the Consequences above and ADR 002 promise), `ListRoutes` (the
routing table), and the request log's two readers, `ListRequests` and
`WatchRequests` (server-streaming, for `voya log -f`), which answer nothing
until the ring buffer lands (#150). Everything reads memory; `GetStatus` is
pinned instant by a median over a full registry.

**The reserved row of the table above keeps its row and loses its excuse.**
`voya.internal` on a path that is not a `DiagService` procedure — a browser's
`GET /`, a Connect call to a procedure the service does not have — still
answers 501 / 12 / `unimplemented`, now literally true rather than "this
build does not serve it yet". The proxy dispatches the reserved name to the
diagnostics handler only for the procedure paths it claims; nothing else under
the name is anybody's to answer.

**The #102 deferral ends here.** That amendment deferred the report *shape* for
`doctor`, `status` and `routes` to the change that brought the registry they
share, so it would be decided once. This is that change: `voya.diag.v1` carries
`DoctorReport`, `HealthReport` and the `Verdict` enum beside the RPCs, decided
together and landed in one change — under `proto-breaking`'s `FILE` rule set
the first landing is effectively final, so the whole package is designed at
once rather than three times. Neither report is served by `DiagService`:
doctor's probes and health's `Check` calls run in the CLI process, and the
messages exist so the CLIs render one schema as pxf. A per-package `WIRE`
override for `voya.diag.v1` was weighed and rejected — one more special case in
`buf.yaml` to explain, for a schema this repo's own CLIs consume as Go.

`StreamState` has one live value on purpose: the registry models liveness as
presence, so a deposed instance is absent from the list, not "detached". The
enum exists so a richer state is an added value later, not a renamed field.

**Amendment 2026-08-15 (issue #150): the ring buffer landed, so the two
readers answer.** The clause above — `ListRequests` and `WatchRequests`
"answer nothing until the ring buffer lands (#150)" — has expired. The proxy
records every request it answers into a bounded in-memory ring buffer of the
last 1024, on the way out, with the six fields the Decision names and no
payload byte (ADR 003); `ListRequests` copies it out oldest first with an
optional limit, and `WatchRequests` streams each record from the moment of the
call, never replaying. The Decision's "on by default" is
`request_log = true` in `base.pxf`; off, the buffer does not exist and both
RPCs answer `failed_precondition` naming the setting rather than an empty list
a developer would read as no traffic. The Consequences' "costs a little
latency" was measured for the buffer — `BenchmarkRequestLogOverhead`, median
over 5000 pooled h2c round trips, buffer off against on — and the difference
is inside the run-to-run noise on the machine it ran on. The live line the
next paragraph describes is a write to stderr per request and was not in that
measurement.

Two things the Decision did not say, decided here. The live line `voya run`
prints per record is `log_level`'s, not `request_log`'s: `LOG_LEVEL_WARN`
quietens the terminal and leaves the buffer filling, because a developer who
asked for a quieter terminal still wants "what happened to *this* request"
answered. And a request `DiagService` itself served under the reserved name is
not recorded — `voya log` and `voya log -f` reading the buffer would otherwise
fill it with the reads of the command that reads it. Anything else under
`voya.internal` is answered from the reserved row and recorded like every
other diagnostic.

**Amendment 2026-08-15 (issues #151, #152): `voya status` and `voya routes`
are built, and stay clients.** The first and fourth supporting surfaces above
exist as commands: each is one unary `DiagService` call from the CLI process
— `GetStatus`, `ListRoutes` — dialled on the proxy port under `voya.internal`
with the same config load `voya run` reads, so a developer who moved
`proxy_port` reaches the voya they run. Neither command probes anything:
`status` shows a HEALTH column that is blank until `voya status --health`
lands, so the two liveness notions have their two places from the start, and
the STREAM column beside it is what routing acts on. Both render the human
screen by default and the response message as pxf under `-pxf` (never JSON,
per ADR 003, whatever the `| jq` aside in the stderr amendment above seemed
to invite); the whole document as `pxf.Marshal` writes it, since one answer
is not a stream. Evictions and reclaims are on the `status` screen after the
fact, oldest first, both instances named by port and pid — the record ADR
002's #43 amendment promises at every `log_level`, including the one that
silences the warning.

Two things decided in the change. "voya is not running on port N" is an
answer and not a failure: one line on stdout naming the port and `voya run`,
exit 1; a listener that accepts and never answers is a different diagnosis,
exit 2 — and the three CLI clients (`log`, `status`, `routes`) share one copy
of the deadline and both answers, so a hung voya cannot read as absent in one
command and not another. And the deadline is the shared two seconds `voya
log` landed with rather than the low hundreds of milliseconds #151 sketched:
generous by orders of magnitude against a memory read across loopback (the
median wall time of `voya status` against a full registry, process start to
exit, is under ten milliseconds), and what it buys is that a command in a
shell pipeline against a stuck daemon ends with a sentence rather than
hanging.

**Amendment 2026-08-15 (issue #153): `voya doctor` is whole, and its report
is pxf.** The two amendments of 2026-08-14 above shipped doctor DNS-only and
deferred its schema; both deferrals end here. Doctor now runs the three checks
the Decision names. `dns` is unchanged. `proxy-port` asks who holds
`proxy_port` by dialling `DiagService.GetStatus` on it under `voya.internal`
— an answer *is* the proof that voya holds the port, and its `Self` says which
voya (pid, version, since), so `lsof` is never asked then; a refused connection
is `free`, verdict `ok`, because doctor is not `status` and an absent voya is
a fact the detail states, not a fault; anything else accepting — a different
program, or a voya that hangs — is `held-by-other`, verdict `problem`, with the
process named where `lsof` can name it (`internal/lsof`, the daemon's own
startup-conflict helper moved out so both can use it); and `unread`, verdict
`unknown`, when `proxy_port` could not be read at all, since the port comes
from the configuration and a user layer that does not parse leaves the check
nothing to ask about — which is #104's rule, not an exception to it: that goes
into the report unmeasured beside the DNS check that did run, rather than
killing the report a developer ran to find out why nothing resolves.
`duplicates` reads the same call's eviction record — ADR 002's, kept at every
`log_level` — and is `ok` when it is empty, `evictions-seen` and `problem`
when it is not, each eviction's two instances named by port and pid and the
reclaims that followed beside them; with no registry answering it is `unread`,
verdict `unknown`, never a clean record. **An eviction the record shows
undone says so on its own line**, and a record wholly undone says that too:
the record is the whole history since voya started, so the present-tense claim
that a deposed instance receives no traffic is false of one that took the name
back, and the reclaim record on the same screen is the evidence against it.
The verdict stays `problem` either way, because ADR 002 gives the reclaim an
announcement as prominent as the eviction's — going back to a build somebody
replaced on purpose is the same twenty minutes of confusion, arrived at from
the other side. The report renders every check with its verdict, state
and detail, then the aggregate verdict — `problem` > `unknown` > `ok` — and
the exit code is the aggregate's under the unchanged table (0, 1, 2). The
"not yet checked" admission is gone from the command, the package doc and
README.

**The #102 deferral is paid.** That amendment named the trigger — the first
non-human consumer of the report — and the schema #149 decided beside the
RPCs is what makes the consumer safe: `voya doctor -pxf` renders
`voya.diag.v1.DoctorReport`, one whole document as `pxf.Marshal` writes it,
the same flag `status` and `routes` take, for the shell script that wants
more than the exit code. `VERDICT_UNSPECIFIED` is `unknown`, the zero value on
both sides, so a verdict nobody scored reads as "not answered" in pxf exactly
as it does in Go.

Decided in the change: the two registry-backed checks read *one* `GetStatus`,
so they see one moment of the registry and cannot disagree about whether voya
is there; and doctor's own dial keeps the answer or the failure for the
classifiers rather than printing `status`'s two not-a-failure lines, because
to doctor nothing on the port and something that never answers are
measurements, not answers.

**Amendment 2026-08-15 (issue #154): `voya health` is built, and probes
nothing the daemon knows about.** The second supporting surface exists as the
Decision describes it — on demand, a short timeout, each result beside its
registry entry — and as `voya status -health`, which is the same screen by
being the same code path: one `GetStatus` for the list, then one
`grpc.health.v1.Health/Check` per registered service, every probe at once,
each bounded to two seconds, all in the CLI process and none outliving the
command. The probe goes through the proxy under the service's own authority,
so a `serving` cell also proves voya routes that name. The daemon's part is
`GetStatus` and routing the probes; it never learns what they found, and the
three constraints above hold as written — no interval, no background
goroutine, nothing evicted on the result. The service that timed out is in
the next `GetStatus` as it was, and the test asserts that.

The HEALTH column's vocabulary, decided here. A cell is the `ServingStatus`
name lower-cased the way STREAM lower-cases its own — `serving`,
`not_serving`, `unknown` — or one of three words that are not a verdict:
`timeout` (accepted, and did not answer within the bound: the paused-on-a-
breakpoint case, "the answer the developer wanted"), `unreachable` (voya could
not reach the registered port — the proxy's own `unavailable`, and its message
is the diagnostic voya writes for the request, printed under the table: the
error response is this ADR's primary diagnostic surface, and this is where it
is read without a curl), and `error` (the service was reached and answered no
verdict — a 404 from one that serves no `grpc.health.v1`, a 200 of text from
one that answers every path; the Connect code and message are the detail).
`error` is its own word rather than the code's name because the code for the
text case is `unknown`, which is a `ServingStatus` a service can mean, and
rather than `unreachable` because that would be false of a service that
answered.

`unavailable` is therefore read together with the message rather than on its
own. The status-to-code table above collapses all three of the proxy's
unreachable outcomes onto `unavailable`, and a service voya *did* reach can
answer with it too — a 503, or a `Checker` shedding load — so the code alone
cannot tell "voya could not reach the port" from "the service said so". The
discriminator is the `voya: ` prefix every diagnostic this ADR's error-response
clause specifies carries: with it the cell is `unreachable`, without it the
cell is `error` and the code rides in the detail. The Connect envelope this
arrives on is a 503 (the table's `envelope` column); the 502 in that same row
is the status the plain-text path writes, carrying the same text.

`voya health -pxf` renders `voya.diag.v1.HealthReport` — one `ServiceHealth`
per registered service, in `GetStatus`'s order, each carrying the entry it
sits beside — the whole document as `pxf.Marshal` writes it. Exit 0 whatever
the cells say: this is a report, and a `not_serving` service is information;
the only non-zero exits are `status`'s two, for a voya that is not there and
one that does not answer. Plain `voya status` still probes nothing, and the
reason the Decision gives is now measured: against a service that accepts and
never answers `Check`, the plain command's own cost over the process floor
stays in the tens of milliseconds while `voya health` on the same fleet
costs the bound, median over five runs each.

**Amendment 2026-08-18 (issue #169): a caller that goes away is not an
upstream failure.** The routing table above keys on the outcome, and
"registered but unreachable" was where every transport error landed — a
request the *caller* abandoned mid-flight included, so `voya health`'s own
probe of a wedged service running out its deadline was logged on the daemon's
stderr as `proxying "stuck.internal:PORT": context canceled` and recorded from
the unreachable row — 503 on the Connect envelope `voya health` reads, 502 to
a plain-text caller — laid at the service's door, though the service never
failed and nobody was reading the answer. The proxy now tells the two apart by
the inbound request's context: cancelled, and the answer is going nowhere — a
deadline the caller set, a Ctrl-C'd curl, an HTTP/2 stream reset — so there is
nothing to diagnose to anyone. Nothing goes on the log, and the request-log
record carries 499, nginx's "client closed request", with the upstream port it
was routed to. 499 is a record's status and never a wire one — by the time the
proxy learns the caller is gone, the connection or stream that would carry a
status is what went away — and it is not a routing outcome, so the table gains
no row: the request was routed, and the record says where.

The test is which side cancelled, not what failed, and voya itself is the
other side that can cancel: a request still inside the handler when the
shutdown grace runs out, with its caller still attached, is cut by voya —
`internal/daemon`'s Close closes the proxy's `http.Server`, and net/http
cancels the request's context as it closes the connection under it. *Amended
2026-08-18 (#200):* that request is recorded as **444**, nginx's "connection
closed without response", and not as 499 for a caller that had not left. The
number is implementation judgement; its distinctness is not — 502 and 503 in
the table above are routing outcomes and this request was routed, so the record
still carries the upstream port. Nothing goes on the log for it either way: the
shutdown is reported once for the run, by the "proxy still busy after the …
shutdown grace" line ADR 004's #72 amendment fixes, and per request there is
nothing to add. What tells the two apart is a flag the daemon sets at the top of
Close, before the drain, and hands to the proxy as `WithStopping` — **not the run
context**, which is what the earlier text of this amendment named through the
server's `BaseContext`. That claim was wrong: Close closes the proxy server
*before* it cancels the run context (the drain has to happen while it is live),
so at the moment the proxy learns the request is cut, the run context reads
nil; measured in `internal/proxy` with one wedged backend and the caller still
attached, `srv.Close()` records 499 with `runCtx.Err()` `<nil>`. Through the
daemon a check on the run context is racy rather than simply wrong — Close
cancels the run context right after closing the server, and the connection's
goroutine reaches the check a few hops later — which is worse, since it passes
by luck of the ordering; the daemon-level test holds the run context back until
the record exists so that ordering cannot rescue it. The cost, accepted: a
caller that genuinely leaves *during* the shutdown grace is recorded as voya's
cut. That is the smaller and rarer error — the window is one grace period, voya
is leaving throughout it — against what shipped before: every shutdown cut
blamed on a caller that had not left, #169 inverted. 444 reaches almost no
`voya log` reader — its followers are closed first in Close and the daemon is on
its way out — so its surface is the live line on `voya run`'s stderr at
`log_level=info`, directly under the "proxy still busy" line. The cost the
first paragraph named stands: a service that accepts and then wedges is proxied
in silence to any caller with a deadline, the "not serving cleartext HTTP/2"
diagnostic included. `voya health` is the reader for that — its own bound
expires and the row reads `timeout` under the service's name — and the request
log still carries the port.

**Amendment 2026-08-18 (issue #166): the pxf status document omits the
resolver line, on purpose.** The `voya status` screen's "whether DNS
resolution is installed" line is stat'd in the CLI process, on the host
running the command, and `voya status -pxf` renders `GetStatusResponse`,
which has no field for it — so the document is one line less informative
than the screen. That stays: the resolver file is a property of the host
running the CLI, not of the daemon, and a `voya.diag.v1` field would invite
the daemon to answer for a machine that is not necessarily its own. A script
checking DNS wiring reads `voya doctor`'s exit code, which is that command's
product; `statusUsage` says the same, and a test holds the schema, the usage
text and this paragraph together.

**Amendment 2026-08-19 (issue #209): the reserved name serves a second thing,
and the row is now about what is *unclaimed* rather than about DiagService.**
The 2026-08-15 (#149) amendment above says "the proxy dispatches the reserved
name to the diagnostics handler only for the procedure paths it claims;
nothing else under the name is anybody's to answer", and the #150 amendment
says "anything else under `voya.internal` is answered from the reserved row".
Both were true of a name that served one thing. `voya.internal:9777` now also
serves the JWK Set of voya's signing key at `/.well-known/jwks.json` (ADR 006's
local identity provider), so the general clauses are restated as: **a path the
internal handler claims is served by it; every other path under the name still
answers 501 / 12 / `unimplemented` from the reserved row**, whose message now
names both surfaces. The row keeps its row, and no service can register the
name either way. Exactly one path is claimed for the key set — the
trailing-slash variant and the bare `/.well-known/` are not, and answer the row
like anything else.

**A claimed path that is not a `DiagService` procedure is recorded in the
request log.** The #150 amendment excludes DiagService's own requests from the
buffer, for the reason it gives: `voya log` reading the log would otherwise
fill it with the reads of the command doing the reading. A JWKS fetch is not
that — it is a developer's own service fetching keys, exactly the traffic the
log exists to show — so it is recorded like any other answer voya writes
itself, with its status and no upstream port. The distinction the proxy draws
is therefore "did voya read itself", not "was the path claimed".

**Amendment 2026-08-19 (issue #214): doctor covers the auth surface.** ADR
006 gave voya a signing key and a JWKS endpoint, and a developer whose stock
validator could not fetch the key set was debugging it by hand — exactly the
kind of trap `voya doctor` exists to test actively. Two checks join the
report, and the aggregate-verdict rule and the exit-code table carry them
unchanged. `jwks` makes the fetch a validator makes — `GET
voya.internal:<proxy_port>/.well-known/jwks.json`, on the same dial as the
GetStatus the registry pair reads: 200 with a non-empty key set is `served`,
verdict `ok`, count and kids in the evidence; any other answer is
`unserved`, verdict `problem`, with the response — status and body —
verbatim, because the handler's 500 names the key file it could not read; a
connection nothing accepted is `unread`, verdict `unknown`, as for
`duplicates`, since a voya that is not running leaves the endpoint
unmeasured rather than broken; and a configuration that does not resolve
leaves it `unread` too, ProxyPortUnread's reason. `signing-key` reads the
key file as `internal/auth` reads it — presence, mode, owner, parse — and
never generates one itself, because a diagnostic must not create the key it
reports on: loadable is `usable`, `ok`, mode in the evidence; a file auth
would refuse is `unusable`, `problem`, the refusal quoted verbatim since it
already names the path and the remedy; no key yet is `absent`, `ok` — the
key appears on first use, so absence before then is a fact, not a fault —
and no known configuration directory is `unread`, `unknown`. The file check
needs only the directory, not the resolved configuration, so it still
measures when a broken `voya.pxf` leaves the three proxied checks unread.

Decided in the change, and overtaken the same day: this PR was written while
the well-known path still answered from the reserved row, and `jwks` reported
that 501 as the problem it is for a validator — doctor measures the daemon in
front of it, not the version of this repo it shipped from. That reasoning is
unchanged and is why the check is written the way it is; the window it
describes is closed, because #209 landed first (PR #215) and the daemon now
serves the key set. On a build of `main` the check reports `served`.

One consequence of that follows through to the check beside it, and is
accepted rather than worked around: the handler loads the key on every
request and mints it when there is none, so the `jwks` fetch — a validator's
fetch, made before the file is read — leaves a key behind on a machine that
had none. `signing-key` therefore reports `usable` against any daemon
sharing the configuration directory, on the very run that created the key,
and `absent` is what a directory no daemon is answering for shows. Doctor's
own reading still never generates (that is what `auth.Inspect` is for); the
generation is the daemon's, on a request it cannot tell from a validator's,
and making it distinguishable would mean a diagnostic that no longer makes
the fetch the check exists to make.

**Amendment 2026-08-19 (issue #218): the log records voya's own rows, and the
reader filters them.** The paragraph above draws the skip as "did voya read
itself", which was the useful shorthand while DiagService was the only thing
under the name. Narrowed: what earns the skip is a *loop* — `voya log`'s own
ListRequests would write to the buffer it reads, and `voya log -f` would feed
its own terminal one line per line it prints — so DiagService's procedures are
skipped whoever calls them, and everything else answered under
`voya.internal`, claimed path or reserved row, is recorded. The completeness
of the log must not depend on what callers claim about themselves.

That leaves the developer who wants their own traffic alone, and `voya log
-no-voya` is the answer: a reader-side filter, off by default, applied to the
snapshot and to `-f` alike. Suppression at the daemon destroys information and
a filter does not, so the choice is the reader's and is made where it can be
unmade. The filter asks `registry.ServiceFromAuthority` of the recorded
authority — the router's own question of the same header, single-sourced for
the reason [IsInternal] is: a second normalisation rule is how a row comes to
be answered under the name and kept by the filter.

## Amendment 2026-08-20 (issue #233): the HEALTH column gains `throttled`

The vocabulary above has no word for `resource_exhausted`, so a throttled
Check fell to `error` — a word that sends the developer hunting for a
failure when the service is saying "slow down". The cell is now `throttled`,
with the code and message as the detail. It is the *service's own* throttle:
voya's probes carry the marker that exempts them from voya's rate limiter
and spend none of the probed service's budget (ADR 006's amendment of this
date), so voya's own `resource_exhausted` does not reach a probe. That the
exemption is the daemon's leaves one way it still could — a `voya run` older
than the exemption, probed by a newer `voya health` — so the code is read
beside its message the way `unavailable` already is: a `voya: `-prefixed
throttle stays `error` naming voya, and the word never claims a service
threw away a question voya itself refused.

## Amendment 2026-10-31 (issue #314): `voya doctor` shrinks to `voya dns check`

`voya doctor` is gone, with no alias. Its DNS check is now `voya dns check`, and
its other four checks and `-pxf` are removed, along with `DoctorReport`, `Check`
and `Verdict` in `voya.diag.v1`. Where this ADR and its amendments say `voya
doctor`, read that history; this amendment is the current decision.

The Decision's reason for `doctor` was the `CGO_ENABLED=0` DNS trap. The dialer
`voya.Join` installs now resolves `*.internal` for Go callers, so that trap only
reaches tools using the host's resolver, such as curl and browsers. The four
other checks repeated what voya already says where the failure occurs, measured
on a real binary:

* **`proxy-port`:** `voya run` refuses to start and names the holder. Since this
  amendment `status`, `routes`, `log` and `health` name it too.
* **`signing-key`:** `voya run` refuses to start, with the same remedy.
* **`duplicates`:** `voya status` prints the same eviction record.
* **`jwks`:** it failed only with another program on the port. No failure was
  produced with voya running.

The Consequences rule that a newly discovered failure mode is "either fixed or
added to `doctor`" becomes: fixed, or stated where it occurs. A separate command
is the place a developer looks last, and this one was 18% of the production Go.

`voya status -pxf` still omits the resolver line, because that is a property of
the host running the CLI, not of the daemon. A script checking DNS wiring reads
`voya dns check`'s exit code: 0 ok, 1 problem, 2 unknown.

**Unmeasured:** the DNS check's diagnosis of an installed file that does not
work was not re-run for this change, because that needs `sudo`. The classifier
is unchanged and its tests moved with it.
