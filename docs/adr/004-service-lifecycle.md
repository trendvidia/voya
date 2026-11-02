# ADR 004: Service Lifecycle — Registration as a Long-Lived Stream

**Status:** Accepted
**Date:** 2026-08-08
**Relates to:** [002](002-service-discovery.md)

## Context

Local services are terminated abruptly and constantly — `Ctrl+C`, panics, and
recompile loops. Because routing depends on a registry of ephemeral ports, a
stale entry sends traffic to a dead port.

Two failure modes must be handled, and the original draft addressed only the
first:

1. **A service dies** and leaves a zombie registry entry.
2. **`voya` itself restarts**, leaving every live service registered nowhere,
   with nothing to trigger re-registration.

## Decision

Registration is a **long-lived bidirectional Connect stream** on
`127.0.0.1:9779`. The stream *is* the liveness signal.

* On startup, a service opens the stream and sends its name and actual
  ephemeral port, then holds the stream open for its lifetime.
* When the stream closes for any reason, `voya` evicts the service immediately.
  There is no polling and there are no health probes. **Amended 2026-11-02
  (#339): the name goes when the instance's last stream closes.** See the
  amendment at the end of this ADR.
* When `voya` restarts, every stream breaks; services reconnect with backoff and
  re-register automatically. The registry self-heals with no persistence.
* On `SIGINT`/`SIGTERM`, a service closes the stream, waits a grace period for
  `voya` to stop routing to it, and only then shuts its own server down and
  drains what it has already accepted. The two waits are separate and only the
  first is `voya`'s: see the Consequences.
* Services additionally serve `grpc.health.v1.Health` over Connect — not for
  `voya`, but because it transfers to production unchanged (see Consequences).

  **Amended 2026-08-16 (#158): and `grpc.reflection.v1.ServerReflection`,
  which is an obligation of the same kind but for a different reason.** Health
  is served for production; reflection is served for the developer holding the
  terminal. `voya call <service> <Package.Service/Method>` has no descriptors
  of its own and voya holds none either (ADR 003 — voya routes bytes and does
  not know the schemas), so a schema for a name comes from the only party that
  has one: the service. The request goes over the proxy under the callee's own
  authority, so serving reflection is what makes a service *reachable by name*
  from a shell rather than only from code that was compiled against it. A
  service that does not serve it is not broken — `voya call --descriptors
  FILE` takes a compiled `FileDescriptorSet` instead — but it costs its
  callers a build step to ask it anything. Serve both versions: `v1` and
  `v1alpha`, which is what `grpcreflect.NewHandlerV1` and `NewHandlerV1Alpha`
  are for and what every reflection client falls back through.

  **Amended 2026-08-16 (#174): the obligation has a one-liner, and it is not
  in `client`.** `client/reflection`'s `Mount` serves both versions for the
  names a service passes, so an obligation stated in this ADR costs a service
  the same one line the health one does — and nothing has to choose between
  the two handler versions per service, which is how a fleet ends up half
  answering `v1alpha` callers with an unimplemented that reads as "no
  schema". It sits one import below `client` on purpose: that package's
  virtue is what it does *not* pull in, and `connectrpc.com/grpcreflect`
  belongs only to the services that serve reflection — the thinness claim #36
  exists to enforce when `client` becomes a module of its own. The acceptance
  fixtures mount through it, so the documented path and the tested path are
  the same code.

  **Amended 2026-11-02 (#332): `Mount` and `MountFrom` serve nothing on Fly.**
  This obligation is for the developer holding the terminal, and it was the one
  piece of this scaffolding with no gate, so a service following README
  published its full schema in production. They use the same test as `Join`,
  `FLY_APP_NAME` being set. A wiring mistake is still refused on Fly, so it
  fails the same way everywhere. This assumes no service relies on reflection
  on Fly through this package; one that needs it mounts grpcreflect's handlers
  itself. Checked in this repository only, where every caller is an example or
  a test. Pinned by `TestMountServesNothingOnFly`.

Stream messages are binary protobuf on the wire
(`github.com/trendvidia/protowire-go/encoding/pb`). Any human-readable rendering
of registry state — `voya routes`, logs, test fixtures — is pxf
(`protowire-go/encoding/pxf`).

**Framing: plain Connect streaming. `protowire-go/envelope` was evaluated and
rejected (2026-08-09, issue #8).** The question this ADR posed — whether to
hand-roll message boundaries on top of Connect's framing — does not arise:
Connect already frames and types every message on a streaming RPC, so there
are no boundaries to draw. `envelope` also turned out not to be a framing
library: it is an API *response* wrapper (status, transport error, opaque
`Data []byte`, application error with codes and localization args), and its
stream helpers frame `io.Reader`s, which a Connect stream is not. Adopting it
would have meant carrying `RegisterRequest` as opaque bytes inside an
`Envelope`, which erases the schema that `buf breaking` checks. It is not the
house convention for streams either: fleet-wide it is used to normalize HTTP
response bodies, and the only other Connect bidi stream in the fleet sends
typed messages directly.

## Alternatives considered

**1. Unary `RegisterService` plus control-plane active health checking over
`grpc.health.v1`.** *(the original draft)* Rejected on three counts. (a) The
probe interval has no good value: aggressive enough to clear zombies within a
recompile loop is also aggressive enough to **evict a service paused on a
debugger breakpoint**, which is a normal workflow — the developer stops on a
line and their next request 404s. A relaxed interval leaves a zombie window
across every rebuild, which is the *main* workflow. (b) It requires a background
probe goroutine per registered service. (c) It does not address `voya`
restarting at all.

**2. Envoy-side health checking via CDS `grpc_health_check`.** Free and already
built, but moot once Envoy is dropped (ADR 001).

**3. TTL heartbeats, Consul-style.** Workable, and better than probing since the
service drives it. Rejected as strictly more machinery than a stream for the
same signal — it still polls, just in the other direction, and it needs TTL
tuning that the stream does not.

**4. Persisting the registry to disk.** Rejected: reconnect-on-restart makes it
unnecessary, and a stale on-disk registry is a new failure mode that outlives
the process that created it. *This is a rule about caches of live state, not
about the disk: ADR 006's amendment of 2026-08-19 (#208) puts the signing key
on disk for the mirror-image reason — regenerating it is the failure mode —
and says why the two decisions do not conflict.*

## Consequences

* **Positive:** `voya` restarting is a non-event — services reconnect on their
  own. This was unsolved in the original draft.
* **Positive:** No probe traffic and no per-service goroutine beyond the stream
  handler. The "concurrent background workers" cost the original draft accepted
  is gone.
* **Positive:** A debugger pause holds the TCP socket open, so this is far more
  forgiving than active probing. **Not immune, though:** Delve pauses the
  transport goroutines that answer HTTP/2 keepalive PINGs, so a long enough
  pause still trips keepalive. Set generous keepalive timeouts on the local
  stream so the margin is tens of seconds rather than one probe interval.
* **Positive:** Serving `grpc.health.v1` over Connect means a plain HTTP POST to
  `/grpc.health.v1.Health/Check` works as a Fly `fly.toml` HTTP check. This is
  the one piece of the lifecycle that transfers to production unchanged — Fly
  has no first-class gRPC probe.
* **Negative:** Every service must open the stream at startup. This is a
  lifecycle hook with **no production counterpart**, since Fly discovers via 6PN
  DNS. Deliberate dev-only scaffolding: it should be one call in `main.go` and a
  no-op when `FLY_APP_NAME` is set.
* **Negative:** The shutdown grace period is a race, and the same one
  Kubernetes solves with a `preStop` sleep — the mechanism differs, the problem
  does not. This ADR originally guessed 250ms at it. **Measured and replaced
  with 50ms on 2026-08-10 (issue #7);** the guess and the measurement are
  recorded separately below because the two numbers were never the same
  quantity.

  What has to be waited out is **eviction propagation**: the interval from the
  service closing its stream to `voya` no longer handing that service out. The
  proxy reads the registry once per request, so this is where routing decisions
  stop selecting the departing instance; a request already past that read is
  still in flight to the old port, a residual of one handler dispatch that the
  grace's two orders of magnitude of headroom cover rather than measure.
  Sampled over 200 restarts per condition, through the real client, a real
  daemon and a real proxy (Apple M-series, 16 cores, `-race`):

  | machine | `voya` | p50 | p90 | p99 | max |
  |---|---|---|---|---|---|
  | idle | idle | 142µs | 167µs | 216µs | 246µs |
  | idle | proxying | 229µs | 450µs | 1.0ms | 1.1ms |
  | loaded | idle | 250µs | 700µs | 6.0ms | 15.8ms |
  | loaded | proxying | 530µs | 11ms | 28ms | 32.8ms |
  | shared CI host | proxying | 5.2ms | 28.7ms | 66.8ms | **106.7ms** |

  The idle rows are the committed test's own output, so they can be checked by
  running it. "loaded" is 32 spinning processes on a 16-core machine — harsher
  than the parallel compile a restart actually competes with, and the condition
  that produces the tail; those two rows are the worst of two runs. **50ms**
  clears the worst sample of every condition on a developer's machine, and
  costs a fifth of the guess in a recompile loop.

  **It does not clear the last row, and that row is the interesting one.** It is
  `voya-trader`, the self-hosted Linux runner, which builds five
  cross-compilation targets and runs `golangci-lint` on the same host at the
  same time as the test job. There, one restart in a hundred takes longer than
  the grace and would drop the requests `voya` routed after the stream closed.

  That is accepted rather than fixed by raising the number, because `voya` is a
  tool that runs on the machine a developer is working at (ADR 001) and sizing
  its constant for a three-job build host would tax every restart on every
  laptop for a machine class the tool does not target. What the row establishes
  is the shape of the limit: **any fixed grace is a bet on how contended the
  machine is**, and the bet gets worse the busier the machine — which is
  precisely when restarts are most frequent.

  The eviction path itself did not need making faster: at p99 it is already
  sub-millisecond on a machine that is not being starved, and the CI row is a
  scheduling tail rather than work — a wait added to the eviction path would
  shift every sample and *narrow* the spread, while that row's p99 is thirteen
  times its p50. So the grace is a wait on the scheduler, and the way to stop
  betting on the scheduler is to stop waiting on a duration at all — see
  **Open: replacing the grace with an acknowledgement** below.

  `voya.DefaultShutdownGrace` carries the number, `voya.WithShutdownGrace`
  overrides it, and `TestEvictionPropagationStaysInsideTheGrace` keeps the
  measurement honest — asserting the median under every condition, since that
  is what a regression on the eviction path moves, and the tail only with
  `voya` idle, since that is what the machine moves.

  **Amended 2026-08-14 (#82): the tail gates skip when the host's 1-minute
  load average exceeds one runnable thread per core; the median gates
  unconditionally, everywhere, always.** A Makefile-and-comment-only change
  tripped the max gate at 182.6ms with the machine at load 38 and the median
  at 644µs — 78× inside the grace — and an immediate re-run passed. The
  numbers this section's gates were settled against (PR #71's review) were
  taken at 1-minute loads of 2.69–3.10 on the 16-core machine above, 0.17–0.19
  runnable threads per core; the spurious trip ran at 38.05, 2.4 per core.
  Normalising by core count puts both runs on the correct side of the same
  threshold, 1.0, on their respective machines — a raw load number would be a
  bet on how many cores the host has, the same class of mistake as the fixed
  grace this section already argues about. The skip is logged with the gate
  names and the load read verbatim, so a skipped gate is visible rather than
  silent.

  **Amended 2026-08-14 (#113): the ratio's two halves are not measured over
  the same set of CPUs, and that asymmetry is an accepted assumption rather
  than an oversight.** The numerator is the *host's* load average — every
  runnable thread on the machine — while the denominator, `runtime.NumCPU`,
  is the number of CPUs *this process* may schedule on: on Linux the runtime
  takes it from `sched_getaffinity`, so under a `taskset` mask or a cpuset the
  denominator shrinks while the numerator does not. The ratio then inflates
  and the tail gates skip on a machine that is not contended for this process
  — the direction that silently disarms an assertion, the one the rest of the
  mechanism is careful about.

  **A CFS CPU quota is the other hazard and not this one.** #113's decision
  text listed it beside the mask and the cpuset; it does not belong there, and
  the correction is worth keeping because the two point in opposite
  directions. A quota lives in `cpu.max`, which is not part of the affinity
  mask, so `runtime.NumCPU` never sees it — measured under Docker 29.4.1, in a
  16-CPU Linux VM: `--cpus=1` reports `NumCPU=16` with `cpu.max` at
  `100000 100000`, while `--cpuset-cpus=0` reports `NumCPU=1`. (Go's
  container-aware default does read the quota, but into `GOMAXPROCS`, which
  read 2 in the same run; this guard divides by `NumCPU`.) Under a quota the
  denominator therefore stays the host's full width while the process is
  throttled to a fraction of it, so the ratio *under*-states contention and
  the tail gates stay armed on a machine that is starving this process — the
  arming direction, which is the class the spurious trip in #82 belongs to
  rather than the disarming one above.

  Both are accepted, and for one reason: both self-hosted CI legs are bare
  metal, so no current leg runs constrained either way, and a cgroup reader
  would be new mechanism for a hazard no leg has. A documented divergence,
  not a bug to fix on sight; to revisit if a leg ever runs constrained — a
  Docker acceptance leg is the affinity shape when it pins CPUs
  (`--cpuset-cpus`) and the quota shape when it caps them (`--cpus`).

  **Draining is a different wait and is not this one.** Once a request has been
  accepted it belongs to the service's own `http.Server.Shutdown`, whose bound
  is a property of that service's handlers rather than anything `voya` can
  measure. `voya`'s own `shutdown_grace` (250ms, `internal/config`) is a third
  number again — how long the *daemon* lets in-flight proxied requests finish
  when `voya` itself stops — and stays a judgement call for the same reason.

  That drain used to cost about a second for any service `voya` had proxied to
  at least once. `UpstreamTransport` set no `IdleConnTimeout`, so the proxy
  held a pooled h2c connection to the service indefinitely and
  `Server.Shutdown` waited out the HTTP/2 server's one-second GOAWAY timeout
  for a peer that never closed it. **Fixed 2026-08-12 in #52**, by
  `proxy.IdleUpstreamTimeout` — 250ms.

  The reasoning that had left the timeout off was that it would cut the
  long-lived streams this ADR's debugger-pause case depends on. It does not:
  `IdleConnTimeout` reaps only connections the transport has booked back in as
  idle, and one carrying a stream is not idle however quiet the stream goes.
  Measured, and pinned by `TestASilentStreamSurvivesTheIdleTimeout`.

  What it buys, on the acceptance harness, for a service restarted under
  continuous load:

  | | before | after |
  |---|---|---|
  | drain, service proxied to | 1.050–1.081s | 265–270ms |
  | drain, service never proxied to | 1.9–8.2µs | 1.2–9.9µs |
  | `TestARestartUnderLoadCutsNothingOff` | 4.5s | 1.3s |

  Every cell there is a measurement on this host, and a slower machine moves
  all of them; `IdleUpstreamTimeout` is the only number in this section that is
  a constant. Compare the ratios between the columns rather than carrying the
  absolute figures anywhere.

  **The 265–270ms in the first row is the residual, and it is the point.** A
  timer bounds the wait at `IdleUpstreamTimeout` rather than removing it: a
  service stopped *inside* that window — which is every service in a
  restart-under-load test, since the grace stops it 50ms after its last call —
  waits out the remainder. A service quiet longer than the window pays tens of
  microseconds. Removing the residual needs the eviction-driven alternative #52
  weighed and rejected: dropping the proxy's pooled connections when the
  registry entry goes, which is exact but couples the registry to the proxy's
  transport and, lacking a per-host close in `http.Transport`, drops every idle
  upstream connection on every eviction.

  **The same defect had a twin one hop up, and the sufferer was `voya`'s own
  exit. Fixed 2026-08-12 in #72 — by a reap since removed: the 2026-08-14
  amendment at the end of this entry is the current decision, and everything
  between here and there reads as that amendment's history.** Nothing reaped
  a caller's pooled h2c
  connection to the proxy either, and `http.Server.Shutdown` waits for
  connections to go idle — which an h2c connection never reads as, however
  long it sits quiet — so `Daemon.Close` burned its entire `shutdown_grace`
  on every exit with nothing in flight, measured at 251ms of 250ms three of
  three, and then warned "proxy still busy", which was false. Fixed
  symmetrically: `proxy.IdleCallerTimeout`, the same 250ms applied to the
  proxy's inbound server as `IdleTimeout`, reaps a caller's connection once
  it has been quiet that long, after which Close finishes well inside the
  grace with nothing to report. The safety argument transfers unchanged — an
  idle timeout arms only on a connection with no stream on it — and was
  checked for this leg rather than assumed from #52's:
  `TestASilentStreamSurvivesTheCallerIdleTimeout` holds a proxied stream
  silent for 1.5s — six times the window, and also past the whole
  GOAWAY-then-force-close sequence, whichever of the two is longer, so that
  lowering the constant cannot quietly shorten the silence to nothing — and
  the daemon's in-flight tests stop `voya` under an h2c request longer than
  the window. net/http makes the guarantee a strong one: a connection's idle
  timer is stopped when a stream opens and reset only when the last one
  closes, and the force-close that follows a GOAWAY is armed only once no
  stream is open.

  Two things #52 did not have to record. `SetKeepAlivesEnabled(false)` before
  `Shutdown` — the standard library's intended lever for exactly this — does
  nothing here: it closes idle HTTP/1 connections only, the HTTP/2 half being
  a TODO in net/http as of Go 1.26, measured three of three while fixing #72.
  And the reap cannot make the residual window small: a server can only ask,
  and a peer that ignores GOAWAY is forced closed about one second later — a
  constant hard-coded in net/http — so a caller active within the last
  ~1.25s (`IdleCallerTimeout` plus that second) still holds its connection
  when Close arrives, and that exit still pays the grace. What it no longer
  gets is the misdiagnosis: Close counts the requests actually inside the
  proxy handler, warns "still busy" only when that count is nonzero, and
  otherwise reports callers still attached with nothing in flight, at info —
  a correct shutdown described correctly, which is ADR 005's premise applied
  to voya's own exit. **Amended 2026-08-14 (PR #109): "nothing in flight" is
  itself more than the count knows, and the message now says "with no request
  in the proxy handler".** Handler occupancy is the whole of what it measures
  — a request whose headers are still arriving has not reached the handler,
  and a response still draining to a slow reader has left it, so either can
  be on the connection at the deadline while the count reads zero. Announcing
  an empty wire over one of those is #72's own overclaim inverted: the same
  gap between what is measured and what is announced, pointing at an
  all-clear instead of at an alarm.

  **Amended 2026-08-14 (issue #94): the reap lost, and this paragraph is the
  record of what it cost.** `http.Server.IdleTimeout` is one field for both
  protocols, so the 250ms reap also cut HTTP/1.1 keep-alive connections —
  connections that never held Close up, since they report idle and Shutdown
  closes them itself — and without the GOAWAY that made the h2c reap safe:
  the server just closed, and a caller reaching for the same pooled
  connection in that window lost its request. The band was narrow and the
  traffic that lands in it is not: a caller pausing at about the window — a
  poller, a retry loop — through `client.Transport()`'s own HTTP/1.1, lost
  about half its requests at exactly the window (48% on both rigs that
  measured it, ~1 in 400 at uniform gaps, zero just either side of the
  band; PR #92's review), and Go's transport retries only replayable
  requests, so a GET came back transparently and a POST was lost. No
  variant of the timer spares that leg — the field is not per-protocol, and
  a hand-rolled reap reproduces the race — so the timer is gone and the
  shutdown problem is solved off the data path instead: `Daemon.Close`
  reads net/http's own account of non-idle proxy connections (a `ConnState`
  hook) together with the handler count, and when both say quiet goes
  straight to the close it would otherwise reach at the deadline —
  microseconds for every caller, faster than the reap ever made an exit,
  with nothing on the data path at all. What the gate gives up is
  `Shutdown`'s drain for a request that arrives between those reads and the
  close; that window is a shutdown already in progress, and accepting it is
  the tradeoff this amendment records. The two counts cover each other's
  blind sides — the hook holds an HTTP/1.1 exchange until its response is
  flushed and its request body drained, where the handler count already
  reads zero, and the handler count sees h2c streams, which net/http's
  `ConnState` machinery does not report — and each side is pinned:
  `TestAQuietHTTP11KeepAliveCallerKeepsItsConnection` is what notices the
  timer coming back, `TestAnExchangeStillDrainingKeepsCloseOnTheDrainPath`
  the drain the handler count alone would cut, and
  `TestCloseDoesNotWaitOutAnIdleCaller` the fast path itself. (The reap's
  own pins — `TestAnIdleHTTP11CallerIsReapedToo` and
  `TestASilentStreamSurvivesTheCallerIdleTimeout` — retired with it.)

  **Corrected 2026-08-15 (issue #130): the hook does report h2c.** The
  sentence above about `ConnState` and h2c streams was wrong, and the
  attempt to stage the gap it implied is what found it: net/http skips the
  hook only for the one `StateActive` it sets on the connection when the
  h2 preface is recognised — that is the connection state `Shutdown` polls,
  and why an attached h2c caller holds it to the deadline — while its h2
  serve loop then drives the hook itself, per stream, active from a
  request's HEADERS until its response's END_STREAM frame is written. So the
  far end of an h2c exchange is inside the gate as well, and
  `TestAnH2cResponseHeldByFlowControlKeepsCloseOnTheDrainPath` pins it: a
  finished response held behind the caller's flow-control window keeps
  Close on the drain, and the handler count alone cuts it. What stays
  outside on that leg is the flush of the connection's buffered writer
  after the stream is booked closed — one goroutine hop, at most 4KB, and
  not something a caller-side pause can hold open — which is the third
  accepted gap, narrowed to what it actually is.
* **Negative:** Eviction is now as fast as TCP close detection, which is good,
  but a service killed with `SIGKILL` on a wedged machine may leave the socket
  open until the OS reaps it. Keepalive is the backstop.

## Open: replacing the grace with an acknowledgement

**Undecided, and tracked as issue #53. Raised 2026-08-10 by the measurement in
#7; needs a call before it is worth building.**

Everything above accepts that the shutdown grace is a *duration*, and the
measurements are what a duration costs: 50ms is two hundred times the p99 on an
idle machine and still not enough on a busy one. A constant cannot be both. The
question this raises is whether the client should wait on an **event** instead —
on `voya` confirming the name is out of the registry — which is correct on any
machine at any load, and finishes in the microseconds the eviction actually
takes.

Today it cannot. The client's only way to trigger eviction is to close the
stream, and closing it is also what blinds the client: there is nothing left to
receive the confirmation on. Half-closing does not work either, deliberately —
the server treats `io.EOF` on the request side as a client that is still alive
and reading, because evicting there would depose a healthy service
(`TestHalfCloseDoesNotEvict`).

So the shape of the change would be a **deregistration message**: the client
says it is leaving, `voya` evicts and ends the stream cleanly, and the client's
blocked `Receive` returning is the confirmation. The eviction already happens
before the stream ends — `handle.Close()` is deferred in the handler, so it runs
before Connect writes end-of-stream — which means the ordering needed is the
ordering there already is.

What makes it a decision rather than a task: **it changes this ADR's protocol.**
The registration stream currently carries exactly one message, and a second one
is an error the server returns `InvalidArgument` for, on the reasoning that a
client sending two has disagreed with the server about what the stream is. That
rule would have to become "one registration, then optionally one goodbye". It
also needs a fallback — a client talking to an older `voya` gets the
`InvalidArgument` and must fall back to the timer — so the grace does not go
away, it becomes the path not normally taken.

**Recommendation: worth doing, not yet.** The grace is correct on the machine
`voya` targets, and the cost of being wrong is one dropped request during a
restart, which the developer sees explained. It should be reconsidered when
something raises the stakes — a service where a dropped request is expensive, or
evidence that developers hit it on ordinary machines rather than on build hosts.
Until then this is written down so the next person meeting the constant knows it
was a choice and knows what the alternative was, and #53 carries the same
argument somewhere it will be seen rather than only somewhere it can be found.

## Amendment 2026-11-02 (issue #339): the name goes with the instance's last stream

The Decision says a closed stream evicts immediately. That is still true of an
instance with one stream, which is the normal case. An entry can now hold more
than one: ADR 002's amendment of this date lets a registration matching the
holder's pid and port join it, because a client that reconnects can win the race
against voya tearing down the stream it lost. The name goes when the last stream
on the entry closes. There is still no polling and there are no health probes.

The count is what closed #308's two-step takeover. Without it, a process could
copy the holder's registration and hang up, and that hang-up would free the
name for the next registration to claim with no eviction to record.

**Decided by the owner on 2026-11-02, with these costs accepted.** voya is a
development tool for a developer's own machine, and `SECURITY.md` says not to
rely on it on a machine shared with a user you do not trust.

1. **A copy can outlast the service.** A local process quotes the holder's pid
   and port and holds its stream open. When the service exits the name stays, and
   `voya status` still shows the service's port and pid. The process can then
   bind that port and receive the traffic, bearer tokens included, with no
   warning and no record. Pinned by `TestACopyOutlastsTheHolder`.
2. **A copy can block a reclaim.** In the same state, an instance deposed earlier
   gets `ErrNameHeld` for its whole five-minute window, so ADR 002's "kill the
   replacement, the original resumes" fails. Pinned by the same test.
3. **A late notice can leave a dead route.** While voya has not noticed that a
   stream is dead, the name stays, so a service in its `SIGTERM` sequence is still
   routed to, which is what the grace above exists to prevent. The cause is the
   late notice and not the join: the #340 probe measured the same failure with
   the join refused. No test in the suite pins it.

**Rejected: going back to the first stream closing the name.** That reopens the
two-step takeover, and #308's done-when was that a takeover is logged and
recorded.

**Measured, not adopted: refusing a matching registration instead of joining it.**
The #340 probe ran the real client at its default backoff against a real daemon
and proxy on macOS and Linux, with a relay controlling when voya learned of a
broken stream. The client keeps retrying a refusal, and traffic keeps flowing
until voya drops the old stream. Refusing would remove the first two costs and
not the third.

Without an injected delay neither path ever ran: 440 trials, no join and no
refusal, because voya noticed before the client's first retry. Delays of up to
100ms behaved the same. Past the first retry the two differ. Joining had no
outage. Refusing left the name absent from when voya dropped the stream until the
client's next retry: a median of 0.1s after a 500ms delay, 2.1 to 2.4s after 10s,
and 10 to 24s for a stream only the keepalive ends. So the join is a benefit only
if voya can notice late, and the probe had to inject that delay.
