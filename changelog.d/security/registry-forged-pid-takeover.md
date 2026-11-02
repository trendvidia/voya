- **A local process can no longer take a service's name without a trace.** voya
  treated any registration quoting the holder's pid as that service reconnecting,
  and the pid is whatever the client says it is. A process could take the name,
  and the traffic and bearer tokens sent to it, with no warning and nothing in
  `voya status`. A claim on another port now evicts the holder like any other:
  a warning on voya's stderr, an entry in `voya status`, and an `Evicted` event on
  the holder's stream. A registration matching both the holder's pid and port
  joins the existing one, so hanging up afterwards no longer frees the name.
