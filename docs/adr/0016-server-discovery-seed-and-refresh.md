# ADR-0016: Nomad server discovery with a seed list and tent-node refresh

- **Status:** Accepted; amended by [ADR-0027](0027-nodeconfig-contract-rendering-and-spec-hash.md)
  (tent sends the seed in NodeConfig, and tent-node renders `05-join.hcl`, the seed included; a combined node joins as
  a server does) and by [ADR-0030](0030-nomad-on-nodes.md) (the `join` phase of `up` refreshes at boot, before Nomad
  starts; a refresh asks the servers of the last answer (`/var/lib/tent/peers.json`), then the seed, for
  `/v1/status/peers?stale` with the TLS name `server.<region>.nomad`; on server and combined nodes `refresh-join` asks
  the node's own agent first; `05-join.hcl` is rewritten only when the rendering changes, and an empty answer changes
  nothing) and by [ADR-0031](0031-bootstrap-in-update.md) (the seed as built: the private addresses of every other
  server that the run knows, by name; the first server has none; a server that is not ready has no address, so a
  wait runs before the creates of its role; a client gets every known server) and by
  [ADR-0035](0035-rollout-decisions.md) (the rollout guard is the stability window: a server is removed only when
  every other voter's `StableSince` is at least the refresh interval plus 10 s old; before a server roll tent checks
  the servers, not every node)
- **Date:** 2026-09-25
- **Deciders:** ingvarch
- **Related:** complements [ADR-0009](0009-server-discovery-fixed-ip-slots.md) (Hetzner slots become a provider
  optimization); [ADR-0008](0008-node-credential-delivery.md), [ADR-0017](0017-api-driven-server-removal.md),
  [architecture §11.2](../architecture.md#112-server-discovery-seed-and-refresh)

## Context

Nomad agents need a list of servers to join, `server_join { retry_join = [...] }`. `server.retry_join` is removed in
Nomad 2.1. On Vultr:

- **No fixed private IPs.** The VPC attach call takes only `vpc_id`; VPC 2.0's `ip_address` parameter is gone.
  [ADR-0009](0009-server-discovery-fixed-ip-slots.md)'s slots cannot be built.
- **No cloud auto-join.** go-discover has no Vultr provider.
- **Load balancers are always public** and pick targets by instance ID.
- **Vultr's own Nomad guide** puts the servers' static private IPs into `retry_join`.

Static lists go stale:
- After a full rolling replacement of the servers, every IP baked into a client's configuration is gone.
- A running client learns new servers from heartbeats, but it keeps them only in memory. After a restart it has only
  its configuration, so a client that restarts later cannot rejoin.

Nomad facts, verified:
- **`GET /v1/status/peers`** returns the Raft peers' RPC addresses and requires **no ACL token**. mTLS still applies,
  so a node can call it with its own certificate.
- **`GET /v1/agent/servers`** needs `agent:read`.
- **Configuration files** in the agent's directory are read at start.

## Decision

- **A new `JoinStrategy`, `SeedAndRefresh`.** It is the default for providers that have neither `FixedPrivateIPs`
  nor `CloudAutoJoin`.
  1. **Seed.** When tent creates a node, it renders the private IPs of the servers that already exist into the
     node's `/etc/nomad.d/05-join.hcl`.
     - The IPs come from the cloud API; on Vultr, from `GET /v2/instances/{id}/vpcs`.
     - On first bootstrap, `server-0` is created first, and the remaining servers are seeded with `[server-0]`. Serf
       join is transitive, so every server meets every other one and `bootstrap_expect` completes.
  2. **Refresh.** tent-node runs `refresh-join` at boot, before Nomad starts, and from a systemd timer every
     60 seconds.
     - It calls `GET https://<known server>:4646/v1/status/peers` with the node's own certificate.
     - It rewrites `05-join.hcl` atomically when the peer set changes. Servers write the addresses with the Serf port
       (4648), clients with the RPC port (4647).
     - The refresh never restarts Nomad. It only guarantees that the next start finds the current servers.
  3. **Rollout guard.** Before replacing servers, every node must be healthy. Between server replacements, tent
     waits at least one refresh interval.
  4. **Certificates.** Node certificates carry the `clientAuth` extended key usage.
- **Other strategies.**
  - `FixedSlots` on Hetzner ([ADR-0009](0009-server-discovery-fixed-ip-slots.md)) renders the slot list into the
    same `05-join.hcl`. The refresh timer runs there too; it is harmless and covers slot changes.
  - `CloudAutoJoin` on AWS uses tag-based cloud auto-join.
- **`05-join.hcl` is per node and dynamic**, so it is excluded from the spec hash.

## Consequences

### Positive

- No tokens or cloud credentials on nodes, no extra cost, and no public exposure of RPC.
- The strategy does not depend on the provider, so it works on any cloud with a private network.

### Negative / trade-offs

- **A moving part on every node:** a timer, plus HTTP access from nodes to servers on port 4646 over the private
  network.
- **A node that was offline for an entire server roll** comes back with a stale list and cannot join. `validate`
  reports it as not ready, and the fix is to replace it, in line with immutable nodes.
- **Initial server creation is partly sequential:** `server-0` is created first, which adds about a minute on first
  bootstrap.

### Follow-ups

- `tent-node refresh-join` with its timer, and the rendering of `05-join.hcl` (M2).
- An E2E check: after every server has been replaced, restart a client and assert that it rejoins (M3).

## Alternatives considered

- **A public load balancer in front of the servers.** $10/month, public exposure of RPC (TLS-protected), and
  membership that must be updated by instance ID on every replacement.
- **DNS SRV** (the `srv` provider, Nomad ≥ 2.0.4) with Vultr DNS. Needs a domain, publishes private IPs in public DNS,
  and has slow propagation (6–12 hours documented).
- **`exec=` with a Vultr API lookup by tag.** It puts an API key on every node, which violates
  [ADR-0008](0008-node-credential-delivery.md), even with IAM narrowed to listing.
- **An alias IP configured on the VPC interface.** It relies on undocumented behaviour (legacy docs say private
  ranges are not enforced). The spike confirmed on 2026-09-25 that such an address is reachable from other instances,
  but nothing reserves it, so Vultr may assign it to a new instance later
  ([platform notes §3.5](../platform-notes.md#35-vpc)). A future ADR may bring slots to Vultr as an optimization.
- **Re-rolling clients after server changes.** Too expensive.
