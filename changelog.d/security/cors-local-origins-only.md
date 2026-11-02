- **CORS is limited to local origins.** voya reflected every origin with
  credentials, so any website could read services through it once
  `voya dns install` was in place. Now only `localhost`, `127.0.0.1`, `[::1]`,
  `*.localhost` and `*.internal` are allowed, plus exact origins in the new
  `cors_origins` config field. A frontend on any other origin must be added.
  voya refuses to start on an entry a browser would never send, such as one
  with a path or its scheme's default port, and says which part is wrong.
