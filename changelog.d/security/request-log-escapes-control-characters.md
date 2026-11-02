- **The request log escapes control characters.** A request path, method or
  authority could carry escape sequences or a newline to the developer's
  terminal through `voya run` and `voya log`, and any web page could send one.
