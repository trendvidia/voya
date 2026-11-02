# Security Policy

## Reporting a vulnerability

Please report a vulnerability privately. **Do not open a public issue or pull
request** for one.

1. **GitHub Private Vulnerability Reporting**:
   <https://github.com/trendvidia/voya/security/advisories/new>.
2. **Email**: `security@trendvidia.com`.

Include what you observed, how to reproduce it, the commit or tag it applies
to, and whether you plan to publish a writeup.

We aim to acknowledge a report within 3 business days and to give an initial
assessment within 7 business days.

## What voya is

voya is a development tool. The daemon is meant for a developer's own machine
and listens on loopback only. It has no access control of its own: any process
on the machine can register a name or call a service through it. Do not expose
its ports to a network, and do not run it in production.

Three consequences are worth stating plainly.

- **Any local process can take a running service's name, but not quietly.** voya
  warns on stderr, unless `log_level` is `LOG_LEVEL_ERROR`, records it in `voya
  status`, and tells the instance that lost the name. Traffic to that name,
  bearer tokens included, goes to the new holder. A name nobody holds can be
  claimed without any of that, because nobody lost it.
- **A local process can copy a running service's registration and outlast it.**
  Quoting the service's pid and port joins its registration, and moves no
  traffic. When the service exits, that process still holds the name, and `voya
  status` still shows the service's port and pid. It can then bind that port and
  receive the traffic with no warning and no record. While it holds the name, an
  instance that was deposed earlier cannot take the name back.
- **Nothing reserves voya's ports.** While voya is down, any local user can bind
  9777 to 9779 and answer in its place, and a service's `*.internal` calls,
  bearer tokens included, then go to that process. There is no cheap fix. If
  `voya run` says a port is in use, read what holds it before you stop it. Do
  not rely on voya on a machine you share with a user you do not trust.

## In scope

- A way to reach voya's listeners from off the machine.
- A way for a web page in the developer's browser to reach them beyond what
  the documented local-development CORS behaviour permits.
- Privilege problems in the commands that write system resolver configuration.
- `authn` and `client`, which are linked into production services: a token
  accepted that should not be, or a production binary that trusts voya.
- A crash or unbounded resource use from malformed input to any listener.

## Out of scope

- One local process interfering with another through voya's unauthenticated
  registration, unless it crosses a boundary voya documents. Taking a running
  service's name with no trace does cross one.
- A local user binding voya's ports while voya is down, as described above.
- Vulnerabilities in dependencies that voya's code does not reach.
