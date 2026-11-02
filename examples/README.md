# examples

Two fleet services, written the way a real one is, so you can watch voya work
before wiring it into anything of your own.

- **`payments/`** registers itself and serves a Connect procedure. It mounts
  reflection, so `voya call` can ask it for its own schema.
- **`billing/`** registers itself *and* calls payments at
  `http://payments.internal:9777` — the dial string it would use unchanged on
  Fly, with no build tag and no environment branch in the code.

Both serve `grpc.health.v1`, so `voya health` shows `serving` for each.

Three terminals, and voya's [Quick start](../README.md#quick-start) walks
through them. What is worth reading in the source is small: one
`voya.Join(ctx, name, ln.Addr())` per service, a listener bound to port 0, and
an `http.Server` that speaks cleartext HTTP/2. Everything else is an ordinary
Go service.

## The one thing here that is not what you would write

`internal/demo` builds a protobuf schema **by hand, from descriptors.** A real
service has a `.proto` and generated bindings, gets the procedure string and
the service name from them, and mounts reflection with the one-liner voya's
README shows:

```go
voyareflect.Mount(mux, paymentsv1connect.PaymentsServiceName)
```

These examples have no code generation step, so they hand `MountFrom` a schema
they assembled themselves. It is confined to that one file, and the file says
so at the top. Copy `payments/main.go` and `billing/main.go`; do not copy
`internal/demo`.

Why no `.proto`: it would need a second buf module, and `proto-lint`,
`proto-breaking` and `proto-check` are all scoped to `proto/`. Example
bindings would either drift with nothing checking them, or those three gates
would have to grow — release-path surface an example does not justify. Same
trade `test/acceptance` makes, for the same reason.

## Running them

From this directory:

```console
$ go run ./payments                 # -instance labels a copy, for eviction
$ go run ./billing                  # -every sets the call interval
```

Both need a `voya run` in another terminal. Neither needs `voya dns install`,
and neither needs sudo: the dialer `Join` installs on
`http.DefaultTransport` is what resolves `*.internal`, so DNS is only for
callers voya does not write — curl, browsers, Postman.

Both are no-ops on Fly. When `FLY_APP_NAME` is set `Join` installs nothing and
registers nothing, because each Machine has its own 6PN address and
`*.internal` already resolves there, and `MountFrom` serves no reflection. That is the property the whole design is
for: the same binary, the same dial strings, in both places.
