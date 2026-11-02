- **A slow issuer can no longer stall every `authn` validation.** The validator
  held its lock across the JWKS fetch, so any token naming an unknown kid could
  stop every validation in the process for as long as the issuer took to answer.
  Now only callers that need the answer wait: a token with an unknown kid, and
  the one validation that notices the key set has gone stale. They share one
  fetch and give up when their own context ends. The fetch no longer ends with
  the caller that started it, so a client hanging up cannot discard its answer. A
  withdrawn key can outlive the refetch interval by the length of one fetch.
