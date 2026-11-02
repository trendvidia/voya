- **voya can route to plain HTTP/1.1 servers**
  ([#315](https://github.com/trendvidia/voya/issues/315)). The proxy now finds
  out what a port speaks before its first request, so a service that doesn't
  offer cleartext HTTP/2 works behind its name instead of getting a 502. gRPC
  still needs HTTP/2, and a gRPC call to an HTTP/1.1 service is refused with a
  message saying so.
