# Dialer + authority routing: the round-trip spike

Proves the one seam the whole design rests on: **rewriting only the TCP
destination preserves the authority all the way to the service.**

```console
$ go test -v ./test/roundtrip/
```

A client dials `http://payments.internal:9777`. A `DialContext` sends the
connection to `127.0.0.1:9777` without touching the URL. The proxy routes on
the `:authority` header it finds there. Every link in that chain is somewhere
the authority could quietly be rewritten — and if it is, requests still arrive,
just at the wrong service or at none.

Hardcoded routing table. No registry, no registration stream, no DNS, no
config. The point is to validate the seam, not to build the product. Everything
lives in `_test.go` files so none of it can be mistaken for, or linked into,
the real thing — #16 and #17 build that.

## What it proves

| Test | Claim |
|---|---|
| `TestAuthorityArrivesUnmodified` | The backend sees `payments.internal:9777`, over HTTP/2 on both legs, at the right service |
| `TestThreeProtocolsFromOneClient` | Connect, gRPC and gRPC-Web all work over h2c from one `*http.Client` |
| `TestNamesRouteToTheirOwnService` | Two services sharing a procedure path stay distinct — only the authority separates them |
| `TestConcurrentRequestsDoNotCrossRoutes` | 60 overlapping requests across two names, under `-race`, with no cross-talk |
| `TestDefaultTargetRoundTrip` | The literal `127.0.0.1:9777` from ADR 001, not just an ephemeral stand-in |
| `TestDialerRewritesOnlyTheDestination` | Which names divert to voya and which are left alone — 12 cases |
| `TestDialerIsNoOpUnderFlyAppName` | With `FLY_APP_NAME` set, the same code does ordinary resolution |
| `TestNoOpDialerNeverReachesTheProxy` | …and does not reach voya even when voya is listening |
| `TestRoutingKeysOnTheHostnameAlone` | Port, case and the FQDN trailing dot do not change the route |
| `TestUnknownAuthorityIsDiagnosed` | A typo gets a diagnostic naming the authority and listing what is registered |
| `TestKnownButUnboundIsADifferentMessage` | "Check your spelling" and "check your other terminal" are different answers |
| `TestEmptyAuthorityOverRawHTTP2` | Hand-built frames with no `:authority` get answered, not dropped |
| `TestMissingHostOverHTTP11` | Same for `curl --http1.0` |
| `TestRegisteredButUnreachable` | A registration that outlived its process explains itself |

## The trap it found

`httputil.ProxyRequest.SetURL` sets `Out.Host = ""`, so the outbound authority
becomes the upstream's ephemeral address. Requests still arrive, responses
still come back, and the one property the design rests on is silently gone. One
line prevents it:

```go
pr.SetURL(&url.URL{Scheme: "http", Host: upstream})
pr.Out.Host = pr.In.Host // without this, the name is lost
```

`TestNegativeControl_SetURLDestroysTheAuthority` runs a proxy without that line
and asserts the damage, so the passing tests above cannot pass for the wrong
reason:

```
with SetURL alone the backend saw authority "127.0.0.1:52848"
instead of payments.internal:9777
```

## The gap it found

ADR 005 makes the error response the primary diagnostic surface. It is —
through `curl`. `connect-go` requires an error body to be JSON and discards
anything else unread, so a Go caller sees only:

```
unimplemented: 404 Not Found
```

`TestDiagnosticIsInvisibleToConnectClients` pins that verbatim for a Connect
caller. (It originally pinned gRPC and gRPC-Web too; those rows are deleted,
their trailers-based remedy having landed in the real proxy as **#25**.)
`TestConnectEnvelopeMakesTheDiagnosticVisible` demonstrates the remedy — the
same text in the Connect error envelope — and the caller gets the whole
diagnostic. Filed as **#23**; the decision belongs to #17.

The remedy has to key on the **protocol marker**, not the content type:
`Connect-Protocol-Version: 1`, which becomes a `connect=v1` query parameter on
the GET a side-effect-free procedure sends under `WithHTTPGet`. A content-type
check gets both ends wrong — it misses that GET, which carries no content type
at all, and it claims any `application/json` POST, so a browser or a webhook
hitting a mistyped authority would get a JSON envelope instead of the
human-readable body.

Each direction needs **two** markers, for the same reason in each: one marker
that another kind of caller also happens to carry claims that caller. The GET
form requires the codec (`encoding=`) alongside `connect=v1`, since `connect`
is otherwise just a query parameter name any URL may use. The POST form
requires a content type that parses and is not `application/connect+*`,
because a streaming caller handed a bare JSON object where its reader expects
a length-prefixed end-of-stream frame gets a parse error in place of the
diagnostic. Both directions are covered by subtests: two that must get the
envelope, six that must not, and gRPC / gRPC-Web pinned as untouched — their
remedy is trailers-based and #23 does not attempt it; #25 landed it in the
real proxy.

The envelope also carries the status the routing decision chose rather than a
fixed one. Every failure that names a service is `unavailable`, following
gRPC's convention for a name that does not resolve to an endpoint; a request
that arrived with **no** authority is `invalid_argument`, because no retry will
supply one and reporting it as retryable puts a caller with a retry policy into
a loop it cannot leave.

## Design notes for #16 and #17

* **h2c is stdlib now.** `http.Server.Protocols` and `http.Transport.Protocols`
  with `UnencryptedHTTP2` serve and dial cleartext HTTP/2 with no `x/net`
  involved. This matters beyond tidiness: the dialer sits on the ordinary
  `DialContext`, which is the seam ADR 002 specifies, rather than behind
  `DialTLSContext` with `AllowHTTP` (issue #21). `golang.org/x/net/http2`
  appears here only to hand-build frames no Go client will produce.
* **The client transport must be `UnencryptedHTTP2` without `HTTP1`.** That is
  what makes it prior-knowledge h2c rather than an upgrade dance, and gRPC
  needs HTTP/2 end to end. This needs an assertion on the **inbound** leg to
  be worth anything: the proxy dials upstream over h2c whatever the client
  spoke, so a client that quietly downgraded to HTTP/1.1 still leaves every
  backend-side `HTTP/2.0` check passing. An observer on the proxy is what
  catches it.
* **Route on the hostname alone.** Then a port in the authority is not a
  special case, and neither is case or the FQDN trailing dot.
* **Keep the normalization rule single-sourced.** The dialer decides on it
  whether a name diverts to voya and the proxy decides on it what to route to;
  disagree and a name diverts and then fails to route. Here that is
  `normalizeHost`, shared. Splitting #16 and #17 into separate packages is
  exactly when two copies of one rule stop being edited together.
* **Resolve the upstream per request, not onto the proxy.** Hoisting it to a
  field looks identical under any sequential test and corrupts routing the
  moment two requests overlap, so the guard has to be a concurrent one.
* **Keep `declared` separate from `running`.** Without both sets, a service
  whose stream dropped is indistinguishable from a typo, and those two
  failures want completely different advice.
* **`*.internal` is wider than our HTTP services — open question for #16.**
  ADR 002 says the dialer diverts "any `*.internal` host", and the spike does
  exactly that. But a Fly Postgres or Redis address is also `.internal`, and
  diverting it puts a database connection on voya's h2c port, where it fails
  as an HTTP error rather than a Postgres one — on Fly the same dial reaches
  the database. `TestDialerRewritesOnlyTheDestination` pins the current
  behaviour so the divergence is recorded rather than discovered. Narrowing it
  means choosing what to narrow on — a port allowlist, a registry lookup, an
  explicit opt-out — and that is a decision, not a detail.

## Its shelf life

Verified against Go 1.26.5, `connect-go` v1.20.0, `x/net` v0.57.0, on
darwin/arm64 and linux/amd64. Two of the findings are facts about specific
versions of `net/http/httputil` and `connect-go`, so a toolchain bump warrants
a re-run — the same standing that `test/trailer` has for ADR 003.

Superseded when #16 and #17 land. What should outlive it is the negative
control and the `#23` pin; both belong in the real proxy's suite.
