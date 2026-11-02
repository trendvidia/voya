- **`voya doctor` is gone; its DNS check is `voya dns check`** ([#314](https://github.com/trendvidia/voya/issues/314)).
  The check keeps its exit codes: 0 ok, 1 problem, 2 unknown. The `proxy-port`,
  `duplicates`, `jwks` and `signing-key` checks and `-pxf` are removed, because
  `voya run` and `voya status` already say the same things, and
  `DoctorReport`, `Check` and `Verdict` leave `voya.diag.v1`. With another
  program on the proxy port, `voya status`, `routes`, `log` and `health` now
  name it rather than printing a transport error.
