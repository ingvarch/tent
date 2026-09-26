# ADR-0014: Implement Vultr first and run the E2E suite on Vultr

- **Status:** Accepted
- **Date:** 2026-09-25
- **Deciders:** ingvarch
- **Related:** [ADR-0004](0004-layered-architecture.md), [ADR-0012](0012-testing-strategy.md) (E2E platform
  amended), [ADR-0015](0015-idempotency-without-unique-names.md), [ADR-0016](0016-server-discovery-seed-and-refresh.md),
  [ADR-0017](0017-api-driven-server-removal.md), [ADR-0018](0018-vultr-provider-design.md),
  [platform notes §3](../platform-notes.md#3-vultr)

## Context

The original plan was Hetzner Cloud first, with E2E tests on Hetzner. As of September 2026 that plan cannot be
executed reliably:

- **Hetzner capacity.**
  - New accounts are limited to 5 servers, and raising the limit requires one month of history and a paid invoice.
  - Since 2026-06-26 Hetzner restricts server creation for new customers and some randomly chosen existing ones.
  - CX and CAX types are frequently unavailable.
- **E2E exercises the provider it runs on.** The cloud-agnostic core (engine, rollout, tent-node, PKI, Nomad
  bootstrap) is exercised by whichever provider runs E2E. The provider code can only be tested on its own cloud.
  Without a cloud that reliably creates VMs, milestones M2–M3 cannot be validated at all.

What we found about Vultr (verified 2026-09-25, see the platform notes):

- **Capacity.** No creation freeze was found.
- **API headroom.** The limit is 30 requests/s per IP, against Hetzner's 3600/hour.
- **Cost.** An E2E run costs about $0.05–0.09 for 5 small VMs, because billing is hourly with a one-hour minimum.
- **Speed.** The median time to a VM that accepts SSH is about 60 s, with a tail up to about 4 min. That is slower
  than Hetzner's roughly 30 s.
- **Account limits** for new accounts are opaque.
- **Deploy and API incidents recur**, more than seven since mid-2025.
- **Architecture.** x86 only.
- **Weaker primitives than Hetzner:**
  - names are not unique;
  - tags are plain strings on instances only;
  - no fixed private IPs;
  - no graceful shutdown in the API;
  - no availability zones and no placement spread.

## Decision

- **Provider order.** Vultr is the first implemented provider. Milestones M1–M3 target Vultr, and the E2E suite runs
  on Vultr: region `ams`, falling back to `fra` or `lhr`.
- **Hetzner second.** Hetzner Cloud becomes the second provider (M4). Its design ([ADR-0009](0009-server-discovery-fixed-ip-slots.md),
  architecture §12) stays accepted and is implemented once an account can create servers reliably.
- **Core built for the weakest primitives.** Idempotency uses operation ids
  ([ADR-0015](0015-idempotency-without-unique-names.md)). Server discovery uses a seed list with refresh
  ([ADR-0016](0016-server-discovery-seed-and-refresh.md)). Server removal goes through the Nomad API
  ([ADR-0017](0017-api-driven-server-removal.md)). Hetzner's richer primitives become optimizations switched on by
  capabilities.
- **Spike first.** `hack/vultr-spike` checks undocumented Vultr behaviour with a real account before M1 provider code
  depends on it. Findings go into the platform notes and resolve the provisional parts of
  [ADR-0018](0018-vultr-provider-design.md).
- **E2E rules on Vultr:**
  - Each run stays under 60 minutes (one-hour billing minimum).
  - A dedicated account, or an IAM service user with minimal permissions.
  - Account limits are raised before CI is wired.
  - Runs are serialized.
  - A janitor deletes resources tagged for E2E that are older than 3 hours.
  - A fallback region is used when a deploy incident hits.

## Consequences

### Positive

- E2E is unblocked now, and all milestones can be validated on a real cloud.
- Building the core against the weaker primitives makes it more robust. Hetzner then only adds optimizations.
- Vultr's API headroom keeps E2E runs short on the API side.

### Negative / trade-offs

- Vultr has one failure domain per region, so E2E cannot test location-spread HA. That remains a Hetzner (and later
  AWS) scenario.
- There is no ARM on Vultr, so the `arm` scenario waits for Hetzner (CAX) or AWS.
- VM boot is slower than on Hetzner, and incidents are more frequent. E2E needs generous timeouts and a fallback
  region.
- Documents that assumed Hetzner-first were revised (architecture, roadmap, CLAUDE.md, README).

### Follow-ups

- Run `hack/vultr-spike`, record its findings in the platform notes and settle the provisional items of ADR-0018.
- Raise the Vultr account limits (at least 8 instances) before wiring E2E into CI.

## Alternatives considered

- **Keep Hetzner first and wait.** It blocks all real-cloud validation for an unknown time.
- **Hetzner first, within the 5-server limit.** Possible for a smoke test, but creation restrictions make it
  unreliable, and surge during HA rolls needs every slot.
- **Implement both providers in M1.** It doubles M1 before the core exists. Hetzner comes right after the core is
  proven.
- **A local VM provider** (QEMU, Incus) for fast core tests. It is useful later as a complement, but it does not test
  a real cloud.
