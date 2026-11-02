- **`authn.New` refuses a Production JWKS URL that is not https.** Keys fetched
  over plain http could be replaced by anyone on the network path. A service
  using such a URL now fails at startup. Development is unchanged.
