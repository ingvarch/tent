# ADR-0019: A combined server+client role for dev and small clusters

- **Status:** Accepted
- **Date:** 2026-09-25
- **Deciders:** ingvarch
- **Related:** [ADR-0007](0007-security-baseline.md), [ADR-0008](0008-node-credential-delivery.md),
  [ADR-0009](0009-server-discovery-fixed-ip-slots.md), [ADR-0017](0017-api-driven-server-removal.md),
  [architecture §3](../architecture.md#3-domain-model-and-api)

## Context

- A tent cluster has dedicated servers and separate clients. The smallest useful cluster is therefore four VMs:
  three servers and one client, or one server and one client without fault tolerance.
- New Vultr and Hetzner accounts have small instance limits ([platform notes §2.2 and §3.14](../platform-notes.md)).
  Dev clusters and E2E runs pay for every VM.
- A Nomad agent can run the server and the client at the same time. HashiCorp does not recommend this for
  production, because workloads then compete with Raft for CPU and memory.
- Several security arguments rely on dedicated servers: servers run no workloads, so the gossip key and the server
  certificate are out of reach of jobs ([ADR-0008](0008-node-credential-delivery.md)).
- Client introduction needs an intro token for every client's first registration. A token can be created only after
  ACL bootstrap, which needs a running server.

## Decision

1. **A third role.** `NodeGroup.spec.role` is `server`, `client` or `combined`. A `combined` node runs one Nomad agent
   with both `server` and `client` enabled. The `tent/role` label takes the same three values.
2. **Exactly one server-capable group.** A cluster has exactly one group whose role is `server` or `combined`. Its
   size must be 1, 3 or 5. Groups with role `client` can be added as usual.
3. **A combined node counts as a server** wherever tent treats servers specially:
   - it gets the server firewall group (Vultr) or firewall (Hetzner), server slots on Hetzner, the gossip key and a
     certificate with both `server.<region>.nomad` and `client.<region>.nomad`;
   - anything that selects servers by role selects `server` and `combined`;
   - rollout and removal follow [ADR-0017](0017-api-driven-server-removal.md), and the node is also drained like a
     client before it is stopped.
4. **Client introduction.** The first combined nodes register their client before any intro token can exist. So for a
   cluster with a `combined` group:
   - the default `nomad.clientIntroduction` is `warn`, and validation rejects `strict`;
   - tent still issues intro tokens to every node it creates once the cluster runs, including combined ones;
   - mTLS stays mandatory, so a client without a certificate from the cluster CA still cannot register.
5. **Warnings.** `validate` and every mutating command warn that a combined cluster is meant for dev and small
   clusters: workloads share nodes with Raft and with the gossip key.

## Consequences

### Positive

- A fault-tolerant cluster fits in three VMs, and a dev cluster in one.
- E2E scenarios need fewer instances, which matters under new-account limits.

### Negative / trade-offs

- On combined nodes a workload that escapes its sandbox can reach the gossip key and the server identity. The
  metadata block for workloads and, on Vultr, user data scrubbing still apply.
- Client introduction is weaker in combined clusters: clients with no token are admitted (`warn`).
- Heavy jobs can starve Raft and cost the cluster its leader.
- Every server code path must handle the combined role, and the E2E suite needs a combined scenario.

### Follow-ups

- M0: the role enum and the validation rules above.
- M2: rendering an agent configuration with both blocks, the certificate SANs and the `warn` default.
- M3: drain before stopping a combined node; an E2E scenario with a combined group.

## Alternatives considered

- **No combined mode.** Simple and safe, but a fault-tolerant cluster costs at least four VMs, which hurts dev use
  and E2E under account limits.
- **Allow it silently as `server` plus a flag.** A flag hides a different security posture behind a familiar role.
  An explicit role makes it visible in every spec, label and plan.
- **Keep `strict` client introduction and roll the combined nodes after bootstrap**, so their clients re-register
  with tokens. Every new cluster would start with a rolling replacement, which costs minutes and VMs for little gain,
  because mTLS already keeps foreign clients out.
