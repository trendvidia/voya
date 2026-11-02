# Changelog

All notable changes to `voya` are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Entries are assembled from one-file-per-change fragments under
[changelog.d/](changelog.d/); see that directory's README for how to add one.

## [v0.2.0] - 2026-08-21

### Added

- **The auth config shape and the signing key** ([#208](https://github.com/trendvidia/voya/issues/208)).
  ADR 006's local identity provider gets its configuration: `auth` in
  `voya.pxf` carries the `iss` voya signs as, the `aud` it stamps, a token
  lifetime, and the personas `voya token --as <name>` will mint — shipped with
  `has-write-scope`, `has-read-scope`, `no-scopes` and `wrong-audience`, named
  for the case each one tests and never for a role. A configuration that would
  silently mint useless tokens — no issuer, no audience, a lifetime of zero, a
  nameless or subject-less persona or two sharing a name, a scope with a space
  in it, a claim with no name, a persona restating a claim voya stamps — is
  refused before anything runs. The signing
  key itself, RS256 in `signing-key.pem` beside `voya.pxf`, is generated at
  mode `0600` when `voya run` starts — or by `voya token` when it runs first
  on a fresh machine — and reused from then on; a key
  file readable by other users, owned by another user, or that is not a key is
  refused naming the file.

  ADR 006's "on first run" is literal: `voya run` mints the key once before it
  serves ([#219](https://github.com/trendvidia/voya/issues/219)), so a fresh
  machine has `signing-key.pem` at `0600` before the first JWKS fetch arrives
  — a validator's fetch, and `voya doctor`'s, is a pure read and never what
  creates the key it reports on. The other half is deliberate too: a key voya
  cannot secure — an unwritable directory, a file at the wrong mode or owner,
  no resolvable configuration directory at all — stops `voya run` entirely,
  with the error naming the file (or `$VOYA_CONFIG_DIR`) and the remedy. voya
  is a local identity provider, and one that cannot keep a key says so rather
  than serving as if it could. A key deleted while the daemon runs is still
  regenerated on the next fetch.

- **`authn/interceptor`, the Connect interceptors that run the validation**
  ([#212](https://github.com/trendvidia/voya/issues/212)). `interceptor.New`
  takes a `Validator` and returns an `Auth` with two interceptors beside each
  other, because `unauthenticated` and `permission_denied` are different
  diagnoses and a caller that cannot tell them apart retries the wrong one:
  `Authn()` turns the `Authorization` header into a `Principal` on the request
  context and refuses a call that has none, and `Authz(policy)` asks the
  service's own policy whether that principal may perform this operation.
  Authentication is required by default — a procedure is opened only by naming
  it to `Exempt`, so forgetting to annotate one leaves it closed rather than
  open — and an exempt procedure still carries whatever identity the caller
  presented, because "no token required" is not "identity forbidden". Both run
  on unary and on all three streaming shapes; the header shapes a caller can
  get wrong (missing, empty, wrong scheme, `Bearer` with nothing after it, two
  headers) are each refused by name rather than as one indistinguishable
  failure — and never by quoting the credential back, since a rejection that
  echoed the header would put a live token in the caller's error and in the
  logs. A key set that could not be fetched is `unavailable` rather than
  `unauthenticated`: the issuer being down is not a bad credential, and the
  remedy is to retry rather than to go and get a new token. Installed in
  either order they behave the same — which is one `Auth` per handler, since
  the Principal already in the context is what makes the order not matter —
  and on a client they are inert.

- **`github.com/trendvidia/voya/authn`**, the shared validation library, as a
  third module ([#211](https://github.com/trendvidia/voya/issues/211)). One
  `Validator` per service — environment, issuer, JWKS URL, audience, and the
  issuer is a single string by construction — fetches and caches the JWKS,
  refetches on an unknown `kid` and whenever the cached set is older than the
  interval (rate-limited either way) so both halves of a rotation propagate
  without a restart, the new key and the withdrawn one; verifies signature,
  `exp` (required: a token carrying none is refused), `nbf` and `iat` (30s
  leeway each way), `iss` and `aud`; and yields a `Principal` with subject,
  scopes and tenant. Rejections are distinguishable by `errors.Is` — expired,
  wrong audience, bad signature, missing scope and the rest each have a
  sentinel — and everything fails closed: an unreachable or empty JWKS is a
  rejection, `alg: none` and alg-confusion die on a pinned RS256, and ADR
  006's sharp edge is enforced at startup — `authn.New(authn.Production, ...)`
  refuses an issuer *or a JWKS URL* in voya's `.internal` zone by name, in any
  spelling DNS resolves the same way. Its own module so importing it links a
  JOSE library and nothing else of voya's; `authn/deps_test.go` keeps the
  arrow pointing that way.

- **`changelog.d/` — one release-note file per change**. Release notes are written and reviewed with the diff they describe, instead of reconstructed from the git log at release time. `make changelog VERSION=…` assembles them; see [changelog.d/README.md](changelog.d/README.md).

- **The client package honours 429 and `Retry-After`**
  ([#224](https://github.com/trendvidia/voya/issues/224)). `voya.Join` now
  installs — and the new `voya.RoundTripper()` exposes — a transport that
  treats a 429 from voya's limiter as the instruction it is: it waits out the
  `Retry-After`, in either RFC 9110 form, and retries, telling you which
  name is throttling once per episode rather than per retry. The waiting is
  bounded by the request's own deadline — a header the deadline cannot
  afford fails at once, naming the header's value — or, with no deadline, by
  `voya.DefaultThrottleBudget`. A header that is missing or unreadable falls
  back to the client's own backoff instead of being treated as zero, and a
  call whose body cannot be replayed (a client stream) gets the refusal
  itself. Only voya's own refusals are retried, told apart by the
  `Voya-Rate-Limited` header the limiter stamps
  ([#241](https://github.com/trendvidia/voya/issues/241)): a fleet service's
  own 429, forwarded verbatim, reaches your `resource_exhausted` handling on
  the first attempt — exactly as it would in production, where voya is not
  on the path. `voya.Transport()` stays the bare `*http.Transport` for code
  that needs the concrete type.

  Reaching `http.DefaultClient` is what makes that work without wiring, so
  after `Join` the global `http.DefaultTransport` holds voya's retrying round
  tripper rather than an `*http.Transport`
  ([#224](https://github.com/trendvidia/voya/issues/224)). Code that reaches
  into the global and type-asserts it needs to know:
  `http.DefaultTransport.(*http.Transport).Clone()` panics after `Join`. Call
  `voya.Transport()` when you need the concrete type and
  `voya.RoundTripper()` when any transport will do. Nothing changes on Fly,
  where `Join` installs nothing.

- **`voya/client` — one call in a service's `main.go`** ([#16](https://github.com/trendvidia/voya/issues/16)):

  ```go
  ln, err := net.Listen("tcp", "127.0.0.1:0") // bind port 0; the OS chooses
  stop, err := voya.Join(ctx, "payments", ln.Addr())
  defer stop()
  ```

  Registers the port the service actually bound, holds the stream open, and reconnects with bounded backoff when voya restarts — the service comes back under the same port with nothing coordinating it. It also routes the service's own `*.internal` calls through the local proxy with the URL untouched, so `Host` still names the callee and the dial string is the one it will use on Fly.

  Every entry point is a no-op when `FLY_APP_NAME` is set. It never blocks and never fails because voya is not running: a service starts and serves normally with no voya at all, degrading only in that nothing can reach it by name.

  Being evicted by another instance stops the registration rather than reopening it, so two builds of the same service cannot take the name from each other in a loop.

  **`stop` holds a measured shutdown grace before it returns** — `voya.DefaultShutdownGrace`, 50 ms ([#7](https://github.com/trendvidia/voya/issues/7)). ADR 004 had guessed 250 ms at how long a service should wait between leaving the registry and shutting itself down, and said it was a guess. What had to be measured is eviction propagation: from a service closing its registration stream to voya no longer handing that service out. Over 200 restarts per condition, through the real client, a real daemon and a real proxy, that is 216 µs at p99 on an idle machine and 1.0 ms with voya proxying; on a machine with 32 spinning processes on 16 cores — harsher than the parallel compile a restart competes with — the worst of 800 samples was 32.8 ms. 50 ms clears all of it on a machine somebody is working at. It does not clear a build host running three CI jobs at once, where the tail reaches 107 ms; that is accepted rather than paid for by every restart on every laptop, and ADR 004 carries the reasoning along with the protocol change that would stop the number being a bet on how busy the machine is.

  The wait is also something that happens rather than a note in a document: `stop` returning means voya has had long enough to stop routing to you, so a service can shut its server down the moment it returns. Under continuous load across four generations of a service that is 0 requests cut off; with the grace turned off the same test fails every run, 2 to 4 dying on the wire each time. It is skipped where it buys nothing — a service whose registration never reached the wire, or one that was evicted, is not being routed to. `voya.WithShutdownGrace(d)` overrides it and zero turns it off.

  Draining is a different wait and stays the service's own: once a request has been accepted it belongs to `http.Server.Shutdown`, whose bound depends on that service's handlers rather than on anything voya can measure. Deregister first, drain second — `defer stop()` alone only gets that order right while nothing is deferred after it.

- **The client warns when voya acknowledges a proxy port it is not dialing**
  ([#116](https://github.com/trendvidia/voya/issues/116)). With `proxy_port`
  moved in voya's configuration, a service joining with the defaults printed
  `is serving on http://payments.internal:<the new port>` while its outbound
  `*.internal` calls dialed the old convention — a loopback port nothing
  answers. Inbound kept working, outbound failed, and the log's own line
  carried the number that would have explained it. The registration
  acknowledgement already names the port voya actually serves, so the client
  now compares it against its own on every acknowledgement and warns beside
  the "serving on" line, naming both ports and the remedy: revert voya's
  `proxy_port`, or move the fleet to the new one in both places — the
  dialer redirects only the port `WithProxyPort` names, so the `*.internal`
  dial strings have to name it too.

- **`voya.config.v1` and `voya.registry.v1`** ([#13](https://github.com/trendvidia/voya/issues/13)). voya's own configuration — the three ports, log level and shutdown grace — and ADR 004's registration stream messages. Generated Go bindings are committed, because `go install` runs no code generator.

  Configuration resolves through chameleon from layered `.pxf` files, and every default lives in one committed, embedded `base.pxf`. A machine with no configuration at all resolves to those defaults rather than to an error: a dev tool that needs a config file before it starts has already failed at being a dev tool.

  Validation runs before anything binds and names the field, the value and the layer that set it — by file path, since a layer id is not something you can open.

- **`voya.internal` on the proxy port serves `voya.diag.v1.DiagService`**
  ([#149](https://github.com/trendvidia/voya/issues/149)). `GetStatus` — voya
  itself, every registered service, and the eviction *and* reclaim history
  `voya status` will show — `ListRoutes`, and the request log's `ListRequests`
  and `WatchRequests`, which answer nothing until the ring buffer lands
  ([#150](https://github.com/trendvidia/voya/issues/150)) — a follower whose
  stream the daemon ends on its way down is told so, with `unavailable`, so a
  shutdown does not read as its own `^C`. Tooling reads what
  voya knows instead of scraping CLI output (ADR 005). A path under the name
  that is not a procedure still answers `unimplemented`, and the diagnostic no
  longer claims the build does not serve the name. The schema also carries the
  `DoctorReport` and `HealthReport` shapes the CLIs will render as pxf, decided
  once with the RPCs.

- **`voya dns install --activate` (and `dns uninstall --activate`) restarts
  systemd-resolved whatever the outcome**
  ([#177](https://github.com/trendvidia/voya/issues/177)). A file that is
  right can still be one resolved has never read — a restart that failed and
  whose message scrolled away, a file restored from a backup, one copied from
  another machine — and a plain `voya dns install` finds it already installed
  and, by design, restarts nothing. voya cannot tell that state from the file,
  so the flag is the developer saying so; the `already installed` line, the
  command's own usage and `voya doctor`'s not-answering remedy all name it.
  Nothing to do on macOS, where configd reads the file as it lands.

- **Fleet services serve reflection as well as health** (ADR 004, amended
  2026-08-16, [#158](https://github.com/trendvidia/voya/issues/158)). An
  obligation of the same kind as `grpc.health.v1`, arrived at for a different
  reason: health transfers to production unchanged, reflection is what makes a
  service answerable from a shell rather than only from code compiled against
  it. Two handlers — `grpcreflect.NewHandlerV1` and `NewHandlerV1Alpha` — are
  the whole of it, and the acceptance fixtures now serve them; a service that
  skips them costs its callers a `buf build` before every `voya call`.

- **`go install github.com/trendvidia/voya/cmd/voya@v0.2.0` installs the
  daemon**, which is how ADR 001 says voya is distributed
  ([#231](https://github.com/trendvidia/voya/issues/231)). `go install
  pkg@version` refuses a module whose `go.mod` carries `replace` directives,
  so the root module carries none and requires published versions of its
  siblings instead; `go.work` overrides those for every ordinary build and
  every `make test`. voya is a private repository, so `GOPRIVATE` and git
  credentials covering `github.com/trendvidia` are needed to fetch it.

  What that costs is worth knowing before you send a patch: `make tidy-check`
  runs `GOWORK=off` and therefore reads the *published* siblings, so a change
  to `authn`, `client` or `proto` that the daemon depends on needs its tag cut
  and the root's require bumped before this module sees it outside the
  workspace. Where the sibling change adds a package the daemon imports, that
  ordering is hard rather than a convention — the sibling lands first and the
  code importing it second.

- **The repository is six modules, and the ones a service imports are thin**
  ([#36](https://github.com/trendvidia/voya/issues/36),
  [#236](https://github.com/trendvidia/voya/issues/236)).
  `github.com/trendvidia/voya/client` is its own module standing on
  `github.com/trendvidia/voya/proto` as a leaf, so a service that imports the
  client gets connect-go, grpcreflect, protobuf and `voya/proto` — not the
  daemon's whole `go.mod`, chameleon included, which is private and needs
  credentialed CI. The import path is unchanged, so nothing a service wrote
  moves. `test/acceptance` is a module of its own too, the way `test/trailer`
  is, which keeps its REST fixture's `connectrpc.com/vanguard` (and the
  grpc-go vanguard's own tests carry) off the manifest of the module people
  install.

  Every intra-repo requirement names a published tag rather than a version
  only this checkout can resolve
  ([#226](https://github.com/trendvidia/voya/issues/226),
  [#246](https://github.com/trendvidia/voya/issues/246),
  [#280](https://github.com/trendvidia/voya/issues/280)): a `replace` in a
  dependency's `go.mod` is ignored in the consumer's build, so `go get
  github.com/trendvidia/voya/client` resolves what it fetches without a
  workspace or a `replace` of its own. `client/v0.2.1` requires
  `proto/v0.2.1`, and the root module requires `authn/v0.1.1`, `proto/v0.2.1`
  and `client/v0.2.1` — so a service naming `client` and proto's own types
  (`configv1.RateLimit`) has `proto/v0.2.1` selected for it transitively.

- **The JWKS endpoint** ([#209](https://github.com/trendvidia/voya/issues/209)).
  `GET http://voya.internal:9777/.well-known/jwks.json` serves the public half
  of the signing key as an RFC 7517 JWK Set — `kid`, `alg` and `use` set, so a
  stock JOSE library's remote-JWKS path validates voya's tokens with no
  voya-specific code, which is ADR 006's point. The document is read from disk
  per request and served with `Cache-Control: no-store`, so deleting the key
  file rotates what is served on the next fetch; a key that cannot be read is
  a 500 naming the file, never an empty key set for a client to cache as
  "this issuer has no keys". Everything else under `voya.internal` still
  answers ADR 005's reserved row, and the fetch lands in the request log like
  any other answered request.

- **The proxy: authority routing on `127.0.0.1:9777`** ([#17](https://github.com/trendvidia/voya/issues/17)). Reads `:authority`/`Host`, forwards to the ephemeral port that service registered, and proxies bytes only — no descriptors, no payload parsing, no protocol translation. Connect, gRPC and gRPC-Web pass through untouched, including gRPC's outcome in HTTP/2 trailers, and `traceparent`/`tracestate` cross unchanged so trace plumbing is exercised locally rather than first at deploy.

  The authority survives the hop, which is the mechanism the whole design rests on: a backend sees the name it was called by, not the ephemeral address it happens to be listening on.

  An unroutable request never gets a bare 502. It gets the authority it arrived with, why it failed, and what would have worked — and voya distinguishes a name nobody ever registered from one that registered earlier and stopped, because those send a developer to different terminals. **Every protocol gets that diagnostic in the form its own client will read**, since each reads outcomes from somewhere different: for a Connect caller it goes inside the protocol's error envelope, because connect-go discards any other error body; for gRPC and gRPC-Web it is a Trailers-Only response carrying `grpc-status`/`grpc-message` ([#25](https://github.com/trendvidia/voya/issues/25)), which is what makes `grpcurl` — the tool you reach for when a request is not arriving — say something useful; and a Connect *streaming* caller, whose client rejects any unexpected HTTP status before reading the body, gets it in the end-of-stream envelope ([#173](https://github.com/trendvidia/voya/issues/173)).

  **The status is chosen so a retrying caller does the right thing.** A name no retry can ever satisfy — multi-label like `api.payments.internal`, not `.internal` at all, or a single label no service may register such as `pay_ments.internal` — answers 400 / `invalid_argument` rather than 404 / `unavailable`, because no registration, restart or retry changes what the name is; a service that registered voya's own proxy port answers 400 / `failed_precondition`, gRPC status 9 being defined as "do not retry until the system state has been explicitly fixed" ([#88](https://github.com/trendvidia/voya/issues/88)); and the reserved `voya.internal` authority, which nothing can ever register, answers `unimplemented` — gRPC status 12, Connect with HTTP 501 ([#69](https://github.com/trendvidia/voya/issues/69)). All three used to put a retrying caller in a loop it could never leave.

  An answer that arrives before the request finishes reaches the caller rather than being held until the request ends ([#158](https://github.com/trendvidia/voya/issues/158)) — a client waiting for a reply is exactly the client that has not finished sending, so holding it hung both sides where ADR 005 promises a diagnostic. And a caller that gives up mid-flight is recorded as its own leaving rather than as the service failing ([#169](https://github.com/trendvidia/voya/issues/169)): a Ctrl-C'd curl, or `voya health` probing a wedged service past its deadline, is not an upstream fault.

  Permissive CORS in local dev only, including the `Grpc-Status` exposure a browser needs to read a gRPC-Web outcome at all. voya's headers replace the backend's rather than joining them: two `Access-Control-Allow-Origin` values is a response every browser rejects. A CORS preflight is answered here; a plain `OPTIONS` is the service's own to answer, and is forwarded.

  **The upstream leg is prior-knowledge h2c, so a registered service has to serve cleartext HTTP/2.** gRPC needs HTTP/2 end to end and cleartext has nothing to negotiate over, so there is no falling back to HTTP/1.1 per backend. A service on a default `http.Server` — HTTP/1.1 only — is therefore reachable on Fly and not here, which is a divergence this tool exists to remove; it is called out in the 502 rather than left as an HTTP/2 framing error that reads as a bug in voya. Teaching the proxy to speak HTTP/1.1 to backends that cannot do h2c is not decided yet.

- **The proxy can rate-limit, the way the production edge will**
  ([#223](https://github.com/trendvidia/voya/issues/223)). `rate_limit
  { enabled = true }` in `voya.pxf` caps forwarding at `requests` per
  `window` (five per minute as shipped) **per authority** — both spellings of
  one name share a budget, two names never do, and voya's own
  `voya.internal` is never throttled. The request over the budget answers
  429 with a parseable `Retry-After` and a `Voya-Rate-Limited` marker
  ([#241](https://github.com/trendvidia/voya/issues/241)) —
  `resource_exhausted` to Connect and gRPC callers — so a client's retry
  handling is testable on a laptop (ADR 006). An upstream's own 429 is
  forwarded verbatim, without the marker, and the marker joins
  `Access-Control-Expose-Headers` beside `Retry-After` so a `fetch()` caller
  can tell voya's own refusal from a service's forwarded one rather than
  reading how long to wait without knowing whose wait it is
  ([#251](https://github.com/trendvidia/voya/issues/251)). Off by default:
  nothing changes until a layer turns it on.

  The budget is spent only on requests that actually reach the service
  ([#233](https://github.com/trendvidia/voya/issues/233)). A forwarded request
  that reached no upstream is refunded, so a service that registered a wrong
  port keeps answering its misregistration diagnostic however often a client
  retries instead of hiding the real problem behind a 429; and `voya health` /
  `voya status -health` spend none of it, because their probes carry a marker
  the limiter honours.

- **A deposed service can get its name back** ([#27](https://github.com/trendvidia/voya/issues/27)). Replace a running service to try a fix, find the replacement worse, kill it — and the original resumes serving, with its state and its debugger session intact, without being restarted.

  It asks rather than takes. Every attempt sets the new `only_if_unclaimed` on the registration message, so voya refuses instead of evicting whoever holds the name; a deposed instance can never depose its replacement. That is what stops a retry loop from becoming the two-instances-fighting bug at a slower period. Ordinary registration is unchanged and still last-writer-wins, because a service restarting has to take its name back from the copy that has not finished dying.

  Bounded at five minutes, then it gives up: long enough for "I killed it, give me the old one back", expired long before anyone walks away from their desk. `voya.WithReclaim(window, interval)` tunes it, and a zero window makes an eviction final.

  Coming back is announced as loudly as being deposed was — traffic has returned to a build somebody replaced because it was wrong, and nobody asked for that.

  Stopping an instance that had reclaimed its name is quiet ([#56](https://github.com/trendvidia/voya/issues/56)): an ordinary `SIGTERM` shutdown after a reclaim signs off without `voya: cannot reach voya on port …; … is not reachable by name until it comes back`, which reported an outage where nothing had gone wrong.

- **`client/reflection` mounts `grpc.reflection.v1` for a service in one
  line** ([#174](https://github.com/trendvidia/voya/issues/174)). ADR 004
  asks a fleet service to serve reflection — it is where `voya call` gets the
  schema for a name, since voya holds none itself — and until now every
  service wrote the same two handler lines by hand and chose between the `v1`
  and `v1alpha` versions. `voyareflect.Mount(mux, pkgv1connect.ThingServiceName)`
  serves both, resolving descriptors from the file set the binary was compiled
  with; `MountFrom` takes an explicit resolver for a process that builds its
  schema at run time. Mounting no names — or a blank one, an unset variable
  read straight into the call — is refused at startup rather than accepted,
  because such a reflector answers "not found" for every symbol the service
  really serves. It is a package below `client` rather than part
  of it, so importing `client` still costs what it always did and only the
  services that serve reflection carry `connectrpc.com/grpcreflect`.

- **The registration stream on `127.0.0.1:9779`** ([#15](https://github.com/trendvidia/voya/issues/15)). A long-lived bidirectional Connect stream where the stream itself is the liveness signal — no polling, no health probes, no TTL. A service that dies is evicted as fast as TCP close detection allows; voya restarting breaks every stream at once and the registry refills as clients reconnect.

  Keepalive is deliberately generous — 60s idle plus 60s to answer — because Delve stops answering HTTP/2 pings while paused, and an aggressive keepalive would re-create the debugger-eviction problem that ruled out active probing in the first place.

  Framing is plain Connect streaming: `protowire-go/envelope` was evaluated for this and rejected ([#8](https://github.com/trendvidia/voya/issues/8)).

- **The request log** ([#150](https://github.com/trendvidia/voya/issues/150)).
  ADR 005's fifth surface: every request the proxy answers — routed, or
  answered from a diagnostic row — is recorded once, on the way out, with
  its timestamp, authority, method, path, status, duration and upstream port,
  into a bounded in-memory ring buffer of the last 1024 requests. The
  authority column is the service name without voya's own port, whatever the
  caller's HTTP version ([#161](https://github.com/trendvidia/voya/issues/161)):
  an HTTP/2 caller sends the port in `:authority` and an HTTP/1.1 one usually
  omits it from `Host`, and recording whichever arrived made two callers of
  the same service into two different rows. A request voya cuts off when the
  shutdown grace runs out, with its caller still waiting, is recorded as
  voya's cut — 444, nginx's "connection closed without response", with the
  upstream port — and never as the caller's own 499
  ([#200](https://github.com/trendvidia/voya/issues/200)); it is a record's
  status and never a wire one, and it shows under the "proxy still busy after
  the … shutdown grace" line at `log_level=info`. Metadata
  only: no body, no query string, no header beyond what the line names (ADR
  003). `voya run` prints each record as it lands, at `LOG_LEVEL_INFO`, in
  the same rendering `voya log` uses; `LOG_LEVEL_WARN` silences the line and
  leaves the buffer filling, because a developer who quietened the terminal
  still wants "what happened to *this* request" answered. `request_log =
  false` in `voya.pxf` turns the buffer off. `DiagService.ListRequests` and
  `WatchRequests` now read it — and say so with `failed_precondition` when it
  is off, rather than answering an empty list that reads as no traffic.

- **The service registry, with ADR 002's evict-and-replace built in** ([#14](https://github.com/trendvidia/voya/issues/14)). Name → ephemeral port, in memory, no persistence: services re-register when their stream breaks, so a restart heals itself and there is no stale state on disk to outlive the process that wrote it.

  Registering a name that is already held deposes the incumbent and tells it, and the deposed instance's stream teardown cannot delete its replacement — every registration carries a generation, and a name is replaced by a single write rather than a delete followed by an insert, so it never blinks out while a service restarts.

- **`voya` is a 17 MB static binary, built `CGO_ENABLED=0`**
  ([#28](https://github.com/trendvidia/voya/issues/28),
  [#99](https://github.com/trendvidia/voya/issues/99)). Resolving two
  plaintext `.pxf` layers would otherwise link every cloud KMS SDK — AWS,
  Azure, GCP, Vault and Huawei — because chameleon's resolver imported its
  sops backend at package level; chameleon v0.23.0 puts decryption behind a
  decrypter a backend registers and ships sops separately, so a program that
  decrypts nothing carries nothing: **350 modules to 64**, gRPC out of the
  binary entirely, and 66 MB down to 17 MB. Nothing in voya earned it.

  `make build` and `make install` pin `CGO_ENABLED=0`, the way `make cross`
  always did, so a voya built from this repository is the voya that ships. It
  is not a packaging detail: cgo is on by default wherever a C toolchain is,
  so a Mac with Xcode produced a cgo-enabled binary locally and a pure one
  when cross-compiled — and `voya doctor`'s DNS check is a measurement of ADR
  002's resolver trap, which is invisible to anything but a `CGO_ENABLED=0`
  build (ADR 005 requires the check to be made from one). A locally built
  doctor could not answer the question it exists to ask. A doctor from `go
  install …@latest` still may not, since that path never reads this Makefile,
  and doctor's report stays honest about which kind of build took the
  measurement. Building with cgo on purpose still works — invoke `go build`
  directly.

  One consequence worth stating because it is a trap rather than a gap:
  marking a config layer `encrypted` used to work only because the sops
  backend was linked whether anything asked for it or not. It now fails with
  chameleon's `encrypted layer without Decrypter`, naming the layer, until a
  backend is registered — one blank import of
  `github.com/trendvidia/chameleon/sops`, and the 49 MB comes back with it.
  `voya.pxf` holds ports and log levels and no secrets, so nothing should be
  encrypting one.

- **voya announces a reclaim in its own terminal**, not just in the reclaiming service's log ([#27](https://github.com/trendvidia/voya/issues/27)). A name changing hands is two events — losing it and getting it back — and both now reach the place somebody watching the fleet is looking, with the same prominence. A reclaim only the reclaiming service knew about would be the silent failure that decision was careful to avoid.

- **`voya call`, a service call from the terminal**
  ([#158](https://github.com/trendvidia/voya/issues/158)). `voya call payments
  payments.v1.Payments/GetBalance 'account = "acct_1"'` sends that pxf
  document to the registered service `payments` and prints its reply as pxf on
  stdout: binary protobuf on the wire, through the proxy, under
  `payments.internal` — the same call its neighbours make, so a reply is also
  proof that voya routes the name. The request is the third word, and stdin
  when there is no word or it is `-` — so a procedure that takes no arguments
  is called with an empty word, `''`, which is the empty message.

  The schema comes from the callee's own `grpc.reflection.v1`, fetched for the
  one call and forgotten: voya holds no descriptors and gains none (ADR 003),
  so a service that serves reflection can be called by anyone who knows its
  name and nothing else. `-descriptors FILE` reads a compiled
  `FileDescriptorSet` instead, for a service that serves none.

  Unary procedures only — a streaming one is refused by name, exit 2. Stdout
  is the reply document and nothing else, so it pipes; a Connect error is its
  code and message on stderr with exit 1, and voya's own routing diagnostic
  when it was voya that answered — an unregistered name reads as the proxy's
  sentence about it, not as a bare 404. A request that does not parse is the
  parser's own answer, naming the line and the field, with nothing sent. An
  absent voya and one that accepted and never answered are the two lines
  `voya status` gives, exit 1 and exit 2 (ADR 005).

- **`voya dns install` works on Linux** ([#155](https://github.com/trendvidia/voya/issues/155)).
  The mechanism ADR 002 left open is a systemd-resolved drop-in: `sudo voya
  dns install` writes `/etc/systemd/resolved.conf.d/voya.conf` — voya's marker
  line, then `[Resolve]`, `DNS=127.0.0.1:<dns_port>` and `Domains=~internal`,
  routing-only so nothing gains `.internal` as a search suffix — and restarts
  systemd-resolved, which reads its drop-ins only at startup. Same rules as
  macOS: an identical file is already installed, voya's own file for another
  port is rewritten, a file voya did not write is refused with its contents
  shown, and `uninstall` removes only the marked one. The host is checked
  before anything is written — resolved has to be active, and v246 or newer,
  where a port in `DNS=` became legal — so a host that cannot carry the wiring
  is told so, and told what to do instead (wire `.internal` to voya's listener
  through whatever it does resolve with, or dial through the voya client),
  rather than left with a file nothing reads. That includes a host whose
  resolver already claims every name — a pinned-resolver or VPN policy with
  `Domains=~.` — where the drop-in cannot work at all, because
  `resolved.conf` has no way to bind a server to a domain: voya refuses
  rather than writing a file that would report success and resolve nothing. `voya doctor` and `voya status`
  follow: the system path is now probed on Linux too, with `getent ahosts` —
  NSS, the path `getaddrinfo` itself takes — and the resolver file is only
  spoken of where something actually reads it, which on Linux means a host
  that is running resolved. A host that runs resolved without resolving
  *through* it — a static `/etc/resolv.conf` naming real upstream servers,
  `dnsmasq` or `resolvconf` in front, NetworkManager's `dns=default`, the
  default of many server images and containers — is refused as well
  ([#186](https://github.com/trendvidia/voya/issues/186)): the install exits 2
  and writes nothing rather than leaving a file resolved reads and nobody
  asks. A host resolves through resolved when `/etc/resolv.conf` is a symlink
  into `/run/systemd/resolve/`, names `127.0.0.53`, or `/etc/nsswitch.conf`
  lists `resolve` for hosts. The one thing the install still cannot check for
  you is a global `DNS=` of the host's own, and the README says so.

  The install reports what it did with enough detail to debug it
  ([#172](https://github.com/trendvidia/voya/issues/172)): it names the
  configuration file `dns_port` came from — `sudo` decides what `$HOME` is, so
  reading *root's* config and installing the default port while `voya run`
  binds the configured one was previously invisible — or says no user layer
  was found and where it looked. The resolver file is written through a
  temporary file and renamed, and both commands sweep any `.voya-resolver-*`
  left by an interrupted run, touching nothing else in that directory. The
  systemd-resolved restart gets its own 100 s budget rather than the one meant
  for voya's three questions, since a stop running toward systemd's default
  90 s `TimeoutStopSec` is exactly the wedged resolved people reinstall for
  ([#188](https://github.com/trendvidia/voya/issues/188)); because a `sudo`
  command must not sit silent that long, it says so on stderr before it waits,
  and a restart that outlives even that names the deadline and how to find out
  whether the job finished.

- **`voya dns install` and `voya dns uninstall`**
  ([#156](https://github.com/trendvidia/voya/issues/156)). ADR 002's opt-in
  DNS wiring on macOS, written and taken back by voya: `sudo voya dns install`
  writes `/etc/resolver/internal` — a first line marking it as voya's, then
  `nameserver 127.0.0.1` and `port <dns_port>` from the same configuration
  `voya run` reads — so curl, grpcurl, Postman and browsers resolve
  `*.internal` through voya and `voya doctor`'s system path goes green.
  Idempotent: an identical file is left alone and reported as already
  installed, voya's own file for another port is rewritten and the change
  reported, and a file voya did not write is refused with its contents shown
  and nothing written. `sudo voya dns uninstall` removes exactly the marked
  file, refuses an unmarked one the same way, and treats no file as nothing
  to do. Without root each says the `sudo` line and exits 1; off macOS each
  says the wiring is not specified yet
  ([#155](https://github.com/trendvidia/voya/issues/155)) and exits 2. Both
  print one line to stdout saying what they did. `voya doctor`'s remedies now
  name the command; the resolver file's path and the OS gate moved to
  `internal/resolverfile`, one home for the check that reads the file and the
  command that writes it.

- **`voya doctor` covers the auth surface**
  ([#214](https://github.com/trendvidia/voya/issues/214)). Two checks join
  the report. `jwks` fetches
  `voya.internal:<proxy_port>/.well-known/jwks.json` the way a stock token
  validator would: 200 with a non-empty key set is `served`, ok; any other
  answer is a `problem` with the response — status and body — verbatim,
  because the handler's refusal names the key file it could not read; a
  connection nothing accepted leaves the endpoint unmeasured, since a voya
  that is not running is a fact, not a fault. `signing-key` reads the key
  file as voya will — presence, mode, owner, parse — and never generates
  one itself: a loadable key is `usable` with its mode in the evidence, a file
  voya would refuse is `unusable` with the refusal and its remedy quoted
  and the mode beside it, and no key yet is `absent`, ok, because voya
  generates one on first use — which the `jwks` fetch above counts as, so
  against a running daemon the answer is `usable`. Both feed the exit-code
  verdict like the checks before them.

- **`voya doctor` actively tests ADR 002's DNS resolver traps**
  ([#6](https://github.com/trendvidia/voya/issues/6)). It resolves
  `*.internal` three ways — the system path curl and browsers use, this
  binary's own resolver, and Go's pure resolver — rather than checking that
  `/etc/resolver/internal` merely exists, which would pass in exactly the case
  that breaks. Every report carries the state the probes measured, the
  evidence, and a verdict — `ok`, `problem` or `unknown` — and the exit code
  is the verdict's: 0, 1 or 2, because a check that could not run is neither a
  pass nor a failure
  ([#99](https://github.com/trendvidia/voya/issues/99)). A system path that
  could not be probed — a dscacheutil that would not run, or a probe that spent
  its budget — reads as "not measured", never as a failed one
  ([#96](https://github.com/trendvidia/voya/issues/96),
  [#258](https://github.com/trendvidia/voya/issues/258),
  [#260](https://github.com/trendvidia/voya/issues/260)): ADR 002's resolver
  trap is a divergence between two paths, so it is asserted only when both
  halves were actually measured, and an unmeasured half carries the unknown
  verdict rather than the problem one.

  The DNS check diagnoses the causes it can tell apart rather than always
  prescribing the install. Where the resolver file is present it reads the
  port back out of the file and dials it, so "voya is answering there" is a
  measurement ([#243](https://github.com/trendvidia/voya/issues/243),
  [#253](https://github.com/trendvidia/voya/issues/253)); on Linux it names a
  global catch-all routing domain that makes voya's drop-in inert
  ([#185](https://github.com/trendvidia/voya/issues/185)), a systemd-resolved
  the host does not actually resolve through
  ([#204](https://github.com/trendvidia/voya/issues/204)), and a resolved
  older than v246, which cannot be told a port at all
  ([#206](https://github.com/trendvidia/voya/issues/206)) — each instead of
  prescribing an install or a restart that would be refused or would change
  nothing. On macOS it names a stale mDNSResponder cache beside the remedy.
  An `/etc/resolver/internal` that cannot be stat'ed still produces a report
  rather than no report.

- **`voya doctor` runs all three of ADR 005's checks, and `-pxf` renders the
  report** ([#153](https://github.com/trendvidia/voya/issues/153)). Beside the
  DNS check, `proxy-port` says who holds `proxy_port` — voya (its DiagService
  answers, and the detail names its pid), nothing (`free`, ok: an absent voya
  is a fact, not a fault), or another process (`held-by-other`, problem, named
  where lsof can name it) — and `duplicates` reads the registry's eviction
  record, naming both instances of every eviction, whether the deposed
  instance took the name back, and the reclaims that followed, at every
  `log_level`. A check that could not run at all — both of these when
  `voya.pxf` does not parse, so there is no port to ask about — is reported
  `unread`, verdict `unknown`, beside the DNS check that still ran, rather
  than replacing the report with the configuration error. The report shows
  each check's verdict, state and evidence, then the aggregate verdict
  (`problem` > `unknown` > `ok`), which is the exit code: 0, 1, 2. `voya
  doctor -pxf` prints the
  `voya.diag.v1.DoctorReport` instead — the first non-human consumer of the
  report, the trigger the #102 amendment named. The "not yet checked" line is
  gone.

- **`voya health`** ([#154](https://github.com/trendvidia/voya/issues/154)).
  ADR 005's second supporting surface: the `voya status` screen with its
  HEALTH column filled — one `grpc.health.v1.Health/Check` per registered
  service, on demand, through the proxy under the service's own authority so
  a green cell also proves voya routes the name, all at once and each within
  a short bound (two seconds). A cell is the service's own answer
  (`serving`, `not_serving`, `unknown`), `timeout` when it accepted and did
  not answer, `unreachable` when voya could not reach its port — with voya's
  diagnostic under the table — or `error` when it answered no verdict, code
  and message under the table. `voya status -health` is the same screen by
  being the same code; `-pxf` on either renders the `HealthReport`. Nothing
  is evicted on the result and the daemon runs no probe of its own (ADR 004);
  exit 0 whatever the answers, and `status`'s two not-a-failure exits for a
  voya that is absent or not answering. Plain `voya status` still probes
  nothing.

- **`voya log -no-voya` leaves out requests answered under the reserved
  `voya.internal`** ([#218](https://github.com/trendvidia/voya/issues/218)) —
  `voya doctor`'s key-set fetch, a token validator reading
  `/.well-known/jwks.json`. The daemon keeps recording those rows on purpose:
  a log whose completeness depended on what callers claim about themselves
  would not be a log of what happened, so the filter is the reader's, applied
  to the snapshot and to `-f` alike. `-n` counts before the filter.

- **`voya log`** ([#159](https://github.com/trendvidia/voya/issues/159)).
  The request log from any terminal on the machine, read through
  `voya.internal` on the proxy port: `voya log` prints the buffer oldest
  first, one line per request in the same rendering `voya run` prints live,
  `-n N` the last N, and `voya log -f` follows it — the snapshot, then each
  request as it happens, until `^C` (exit 0); the daemon going away ends the
  follow with one line saying so and exit 1. `-pxf` renders each request as
  one pxf document on one line, so a pipe reads them as they come. voya not
  running is an answer, not a failure — one line naming the port and `voya
  run`, exit 1 — and a voya that accepts and never answers is a different
  one, exit 2. Those are answers rather than failures, so under `-pxf` they go
  to stderr and stdout carries nothing but documents
  ([#163](https://github.com/trendvidia/voya/issues/163)) — the same for
  `status`, `routes` and `health`. Every `-pxf` line parses back through the
  reference pxf decoder ([#167](https://github.com/trendvidia/voya/issues/167)).

  The follow loses nothing and prints in order, which is the whole promise of
  the command: it opens the watch *before* taking the snapshot, so a request
  answered in the loopback round trip between the two is in both rather than
  neither, and the duplicate is dropped by sequence number rather than by
  timestamp-and-path, which two parallel polls of the same endpoint can share
  ([#161](https://github.com/trendvidia/voya/issues/161),
  [#191](https://github.com/trendvidia/voya/issues/191)). With `-n N` the
  window still prints oldest-first, rather than replaying requests the
  snapshot truncated after the newer ones it kept
  ([#189](https://github.com/trendvidia/voya/issues/189)). A follower the
  daemon drops for falling behind — the request log will not make the proxy
  wait on a slow reader — is told so, with a code and a message, instead of
  the stream just ending ([#163](https://github.com/trendvidia/voya/issues/163)).

- **`voya routes`** ([#152](https://github.com/trendvidia/voya/issues/152)).
  The routing table of a running voya, one row per registered service —
  authority, service, upstream address — sorted by authority; "no services
  registered" when the registry is empty. `-pxf` renders the
  `ListRoutesResponse` as pxf, in that same order — the daemon answers in
  name order, which differs once a name holds a hyphen. Same dial, deadline
  and not-running answers as `voya status`.

- **`voya run` — the daemon** ([#18](https://github.com/trendvidia/voya/issues/18)). Resolves the configuration, binds all three listeners, and serves: proxy on 9777, wildcard DNS on 9778, registration stream on 9779. All three bind before any of them serves, so a port conflict fails while nothing is half-running.

  A port already in use names the port and, where it can, the process holding it — the failure ADR 001 singles out is the first thing a developer sees when voya will not start. `SIGINT`/`SIGTERM` takes everything down together, and the two halves are not treated alike: the proxy gets the shutdown grace to finish requests a caller is already waiting on, while the registration streams are cut outright — ADR 004 makes them breaking the ordinary case, which is what lets every service re-register on its own.

  With nothing on the data path the exit is immediate rather than paying out the whole grace ([#72](https://github.com/trendvidia/voya/issues/72), [#94](https://github.com/trendvidia/voya/issues/94)), and it gets there without putting a timer on any caller's connection — nothing reaps a caller, so a pooled client pausing between requests cannot lose one. Where the grace *does* expire with callers still attached, the message names what it counted: "with no request in the proxy handler", and "(2 in the proxy handler)" on the other branch, because handler occupancy is a floor on what is live and not the whole of it — a request whose headers are still arriving has not reached the handler, and a response draining to a slow reader has already left it.

  On the upstream side the proxy drops a connection to a service after 250 ms idle ([#52](https://github.com/trendvidia/voya/issues/52)), so a service voya has routed to shuts down in microseconds instead of waiting out Go's one-second GOAWAY grace for a peer that will not let go. Long-lived streams are untouched, silent ones included: an idle timeout reaps only connections with no request on them.

  A duplicate registration prints the eviction warning ADR 002 asks for, naming both instances, where a developer will see it.

  **Everything `voya run` says goes to stderr** ([#59](https://github.com/trendvidia/voya/issues/59)) — the banner, ADR 002's eviction and reclaim announcements, `voya: proxying …`, `voya: dns: replying to …`, and every failure. All of it is diagnostic output, so `voya run 2>/dev/null` is the one switch that silences it and `voya run > /dev/null` keeps it; stdout is reserved for what `voya status` and `voya routes` print. Being quieter is `log_level`'s job rather than the shell's, and `log_level` is honoured by the daemon's own lines as well as the request log ([#43](https://github.com/trendvidia/voya/issues/43)).

- **Wildcard DNS on 9778**. Every `*.internal` name answers `127.0.0.1`, with no per-service records and no update path — there is nothing to keep in sync and nothing to lose. Names outside `.internal` are refused rather than denied, since this server has no standing to claim a name does not exist anywhere. Every query gets an answer, including the ones voya will not serve — a resolver told nothing waits out its own timeout and reports only that.

  UDP and TCP both, on the one `dns_port`, loopback only and from one answering function, so the same five answers hold on each ([#184](https://github.com/trendvidia/voya/issues/184)) — `dig +tcp`, a local resolver forwarding a TCP client, and anything retrying a truncated UDP answer are all served. A name too long for a 512-byte datagram is answered whole on the TCP retry the truncation flag asks for, while one longer than a domain name may legally be (255 bytes) is answered FORMERR rather than with a truncation flag no retry can satisfy. Both sockets or neither: a `dns_port` free on UDP and held on TCP is a startup failure naming the port and, where `lsof` can tell, the holder. At most 128 TCP connections are held open at once ([#195](https://github.com/trendvidia/voya/issues/195)) — past that the next is closed unserved and the refusal logged once until a slot frees, so a stalled client cannot cost a descriptor and a 64 KB buffer with nothing to stop it.

  This is not how voya's services find each other; the client dialer is (ADR 002 as amended). DNS is opt-in and exists for the consumers that are not our Go services — curl, grpcurl, Postman, browsers.

- **The acceptance test: the skeleton walks** ([#18](https://github.com/trendvidia/voya/issues/18)). Two connect-go services run against a real daemon, using the real client package and the real dial strings. One dials `other.internal:9777` and the request arrives, carrying the name it was dialled by. The callee restarts onto a different ephemeral port and traffic follows, with no configuration change anywhere, no sudo and no DNS.

  Alongside it: Connect, gRPC and gRPC-Web all reaching the service through the same port; a second instance evicting the first loudly, with traffic going to the newcomer rather than alternating; voya itself restarting as a non-event, every service re-registering on its own; and a port conflict that says which port and what holds it.

- **`voya status`** ([#151](https://github.com/trendvidia/voya/issues/151)).
  ADR 005's one screen, from a running voya's memory over loopback: voya
  itself (version, pid, uptime, the three ports, and on macOS whether the
  resolver file is installed), every registered service with its name,
  authority, port, pid, uptime and stream state — HEALTH is a column, blank
  until `voya status --health` — then the evictions and reclaims recorded
  since voya started, oldest first, both instances named, at every
  `log_level` including the one that silences the warning. `-pxf` renders
  the `GetStatusResponse` as pxf. Nothing is probed, so the command stays
  instant whatever state a service is in; voya not running is an answer (one
  line, exit 1), a voya that never answers another (exit 2).

- **`voya token --as <persona>`** ([#210](https://github.com/trendvidia/voya/issues/210)).
  Mints a signed JWT for a persona from `auth.personas`, no daemon needed; the
  token is the only thing on stdout, so `$(voya token --as has-write-scope)` is
  a token. `--as` with a missing or unknown name lists the personas that exist.
  `--expired` and `--bad-signature` mint deliberately invalid tokens a stock
  validator rejects for exactly that reason — one defect at a time; the
  wrong-audience and no-scopes cases are personas, not flags (ADR 006). With
  `token` built, every command the ADRs describe exists, and the "Not
  implemented yet" block is gone from `voya help`.
