- **voya can route to a service that serves only TLS**
  ([#315](https://github.com/trendvidia/voya/issues/315)). The caller stays on
  `http://<name>.internal:9777`, and voya connects to the service over TLS,
  accepting its local certificate. gRPC works if the service offers HTTP/2.
