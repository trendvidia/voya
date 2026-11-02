- **Two runnable example services, and a README quick start built on them**
  ([#293](https://github.com/trendvidia/voya/issues/293)). Until now nothing
  shipped that a person could start to see voya work: the only `main` packages
  were `voya` itself and two internal tools, and the first thing a new user did
  was write a service from README fragments that nothing compiled.
  `examples/payments` registers with `voya.Join` and serves a Connect
  procedure with reflection mounted, so `voya call payments
  voya.example.v1.Payments/Charge 'value = "invoice-1"'` works against it;
  `examples/billing` registers too and calls payments at
  `http://payments.internal:9777`, the dial string it would use unchanged on
  Fly. Three terminals — `voya run`, `go run ./payments`, `go run ./billing` —
  and a second `go run ./payments -instance second` is ADR 002's
  evict-and-replace happening in front of you, with traffic following the new
  instance and the deposed one taking its name back when you stop it. No sudo
  and no `voya dns install`: the dialer `Join` installs is what resolves
  `*.internal` for Go callers.

  They are a module of their own, on `test/acceptance`'s argument — nobody
  installs them, so their requirements stay off the manifest of the module
  people do — and being one keeps them compiled for every cross target by
  `make ci`, so an example that stops building fails the gate rather than
  greeting the next reader. One thing in them is not what a real service
  writes, and it says so where it happens: `examples/internal/demo` builds a
  protobuf schema by hand from descriptors, because a `.proto` of its own
  would need a second buf module while `proto-lint`, `proto-breaking` and
  `proto-check` are all scoped to `proto/`.
