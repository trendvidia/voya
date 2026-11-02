# ADR 000: Production Substrate — Fly.io

**Status:** Accepted
**Date:** 2026-08-08 (recorded retroactively; the decision predates this ADR)
**Relates to:** [001](001-core-architecture.md)

## Context

Our Go services need somewhere to run. That choice is not one decision among
many — it determines whether a service mesh is even a concept in this system,
how discovery works, what "production parity" means locally, and therefore what
`voya` is for. Every ADR from 001 onward derives from it.

It was made before these ADRs existed and was treated as a given throughout
them. This records it, because a constraint that load-bearing should not be
implicit.

## Decision

**Fly.io**, on two grounds:

1. **Cost.** Machines bill per second and can scale to zero with
   `auto_stop_machines`, so idle services cost close to nothing. For a fleet of
   mostly-idle internal services this is the dominant factor — far more than
   per-vCPU rates, since most of our services spend most of their time waiting.
2. **Setup simplicity.** Push a container, get a running Machine. No cluster to
   provision, no node pools, no control plane to upgrade, no CNI. Multi-region is
   close to free in effort.

**No formal comparison was run.** This ADR deliberately does not reconstruct a
bake-off that did not happen — it records the actual drivers, and what a revisit
would need to measure.

## What this buys, concretely

* Per-second billing and scale-to-zero, so idle cost approaches zero.
* **6PN private networking with DNS discovery built in** — services reach each
  other at `<app>.internal:<port>` with no registry and no registration step.
* No cluster to operate. There is no control plane, and no platform component
  whose upgrade is our problem.
* Multi-region deployment as a configuration change rather than a project.

## What we are accepting

* **Single provider**, with no multi-cloud escape hatch beyond "it's containers."
* **No service mesh, and no xDS.** This looked like a limitation and turned out
  to be the opposite: it is what made ADR 001's single Go binary the right answer
  instead of Envoy plus an xDS control plane. Resilience features we do not get —
  retries, circuit breaking, outlier detection — are also features we cannot
  exercise at one instance per service locally, so their absence is parity rather
  than a gap.
* **6PN is IPv6-only.** Services must listen dual-stack; an IPv4-only assumption
  works locally and fails on deploy. This is exactly the divergence class `voya`
  exists to catch, and it is why ADR 002 flags it.
* **Reliability history has been uneven.** Acceptable at our stage; it is a real
  input if uptime becomes a contractual obligation.
* **The managed-Postgres story has shifted more than once.** Plan the database
  independently of the compute decision rather than assuming it comes bundled.

## Alternatives, if this is ever revisited

Not evaluated at the time. Recorded so a future revisit starts from a position
rather than a blank page:

* **Cloud Run** — the closest alternative. Same scale-to-zero economics, same
  push-a-container ergonomics, no mesh. Most of this project's design would
  survive the move; ADR 001's reasoning holds nearly verbatim.
* **Kubernetes** — an *organizational* technology, not a cheaper one. It earns
  its keep with many services across multiple teams needing a standard deploy
  interface, RBAC, and quotas. It would invert discovery (platform observes pods
  rather than services registering), making ADRs 002 and 004 local-only
  scaffolding with no production counterpart.
* **ECS Fargate / EC2** — more operational surface for less ergonomics at our
  size; the Fargate premium is worth it only below a few dozen instances, past
  which the delta funds an engineer.
* **Bare metal** — cheapest compute and egress by a wide margin, and genuinely
  low-maintenance for a small team running a few binaries under systemd. You own
  hardware failure, and there is no multi-AZ story by default.

## When to reopen this

Concrete triggers, so the question gets asked on evidence rather than vibes:

* **Egress becomes a significant line item.** It is the cost that most often
  drives platform moves, and it is invisible until it isn't.
* **Multiple teams need isolation** — RBAC, quotas, separate deploy surfaces.
  That is the point where Kubernetes stops being overhead and starts being the
  answer.
* **A compliance requirement lands** that needs mTLS everywhere or attested
  workload identity.
* **Reliability becomes a business problem** rather than an occasional
  annoyance.

## Downstream impact

If the substrate changes, this is what moves — recorded so the blast radius is
knowable rather than discovered:

| Change | Effect |
|---|---|
| → Cloud Run or similar | Little. No mesh either way; ADRs 001–006 hold nearly unchanged. |
| → Kubernetes | ADR 002 and ADR 004 become dev-only scaffolding with no prod counterpart; discovery inverts to platform-observed. ADR 001's rejection of Envoy/xDS would need re-litigating, since parity would then favour it. |
| → bare metal, no orchestrator | ADR 002 and 004 stop being scaffolding and become the production design. Consul/Nomad becomes worth evaluating seriously — `voya`'s registry model is already that shape. |

Note the asymmetry: moving *down* the stack makes `voya`'s design more
production-relevant, not less. Moving *up* to Kubernetes is the only direction
that would invalidate meaningful parts of it.
