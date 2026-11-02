- **`authn` bounds a JWKS fetch to 10 seconds by default.** A hung issuer could
  otherwise block validation without limit. A client passed to `WithHTTPClient`
  overrides it by setting its own `Timeout`, and one with no `Timeout` gets the
  10 seconds.
