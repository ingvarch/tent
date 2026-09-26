# ADR-0009: Nomad server discovery on Hetzner through fixed private IP slots

- **Status:** Accepted (Hetzner-specific); the generic strategy is [ADR-0016](0016-server-discovery-seed-and-refresh.md)
- **Date:** 2026-09-25
- **Deciders:** ingvarch
- **Related:** [ADR-0005](0005-immutable-nodes-and-nomad-aware-rollouts.md), [ADR-0008](0008-node-credential-delivery.md),
  [architecture §12.2–12.4](../architecture.md#122-address-plan)

## Context

Nomad agents need an initial list of servers to join (`server_join { retry_join = [...] }`). The obvious routes do
not fit Hetzner:

- **No cloud auto-join.** go-discover has no Hetzner provider; the issue has been open since 2018.
- **The documented workaround puts a token on every node.** Nomad's docs suggest `exec=` with the hcloud CLI, which
  means an API token on every node. That contradicts [ADR-0008](0008-node-credential-delivery.md).
- **The join list must survive server replacement.** Clients re-read it on restart. A static list of the servers'
  current IPs goes stale after rolling replacements, and a client that restarts after all servers have been replaced
  cannot find the cluster.

Relevant Hetzner and Nomad facts:

- `POST /servers` accepts network IDs only, and the IP is assigned automatically.
- `POST /servers/{id}/actions/attach_to_network` accepts an explicit `ip`, or an `ip_range` to pick the subnet for
  automatic assignment.
- Servers can be created powered off (`start_after_create: false`).
- Servers with no public IP at all must receive their network at creation.
- `server.retry_join` is removed in Nomad 2.1; only the `server_join {}` block remains.

## Decision

- **Two subnets.** The cluster network (default `10.64.0.0/16`) has:
  - a `control` subnet (`10.64.0.0/24`), where every IP is assigned explicitly;
  - a `nodes` subnet (`10.64.16.0/20`), where client IPs are assigned automatically.
- **Seven fixed slots for Nomad servers:** `10.64.0.10`–`10.64.0.16`. That is enough for the maximum server group
  size of 5 plus 2 surge. A server VM is named `<cluster>-<group>-<slot>` and labelled `tent/slot=<n>`.
- **Server creation sequence:**
  1. create powered off, without networks;
  2. `attach_to_network {ip: <slot IP>}`;
  3. power on.

  Clients use `attach_to_network {ip_range: <nodes subnet>}` the same way.
- **Every agent joins through all slots.** The rendered config is
  `server_join { retry_join = [<all 7 slot IPs>] }`. Unused slots only cause harmless connection timeouts.
- **Rolling replacement** puts the new server into a free slot. The old server frees its slot when it is deleted.
- **Joining is a `JoinStrategy` chosen by the provider:**
  - Hetzner (public topology): static slots.
  - Hetzner private topology (later): an internal load balancer (`public_interface: false`, label-selector targets)
    at `10.64.0.2`.
  - AWS: cloud auto-join by tags.

## Consequences

### Positive

- No token on nodes and no extra cost.
- The join list never goes stale.
- Concurrent or repeated creation of the same slot collides on the unique server name.
- Replacement maps naturally onto free slots.

### Negative / trade-offs

- Two extra API actions per server, attach and power on, count against the rate limit.
- The number of slots caps the server group size at 5 in v1.
- The address plan is part of the cluster's identity. Changing `networking.cidr` means rebuilding the cluster.

### Follow-ups

- The address planner in `internal/model`, and a Hetzner `Nodes.Create` that resumes after interruption (M1).
- An E2E check that clients rejoin after all servers have been replaced (M3).

## Alternatives considered

- **`exec=` + hcloud CLI.** Needs an API token on every node, and Hetzner tokens are project-wide.
- **Internal load balancer as the default.** About €7 per month and an extra dependency for every cluster. Kept for
  private topology.
- **DNS SRV (`srv`, Nomad ≥ 2.0.4) with Hetzner DNS.** Needs a domain, and it publishes private IPs in public DNS.
  Possible later as an option.
- **Static list of current server IPs.** Goes stale after replacements (see Context).
- **Alias IPs as floating identities.** They must be configured on the host by hand, and they add complexity without
  a benefit over slots.
