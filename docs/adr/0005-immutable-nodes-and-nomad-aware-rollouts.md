# ADR-0005: Immutable nodes and Nomad-aware rolling updates

- **Status:** Accepted; server removal amended by [ADR-0017](0017-api-driven-server-removal.md); amended by
  [ADR-0030](0030-nomad-on-nodes.md) (tent sets no `drain_on_shutdown`: a client that drains itself at shutdown
  comes back ineligible; tent drains a client through the Nomad API before it removes it, ADR-0017) and by
  [ADR-0031](0031-bootstrap-in-update.md) (servers and combined nodes have `leave_on_terminate = false`, so a server
  does not leave Raft when it stops; tent removes it through the API, ADR-0017) and by
  [ADR-0032](0032-joined-label-scrub-and-delete-guard.md) (until M3, `update` refuses to delete a machine that joined
  Nomad, and it deletes a client that never registered before it creates the replacement) and by
  [ADR-0035](0035-rollout-decisions.md) (a removal from two voters to one and the roll of a group of one server are
  refused for now; a server is removed only after the stability window, the refresh interval plus 10 s read from
  autopilot's `StableSince`; clients: every victim of a batch is marked ineligible before any drain, the VM is
  deleted without a shutdown, its node is purged only once Nomad lists it down, and no validate step runs between
  batches) and by [ADR-0037](0037-rolling-update-of-client-groups.md) (clients as built in M3.3: the new nodes first
  when `maxSurge` allows (the default 1), a drain with the meta `tent_machine` within `drainTimeout`, a limit on every
  wait; it refuses until `update` has applied the specs; server groups wait for M3.4 and combined groups for M3.5)
- **Date:** 2026-09-25
- **Deciders:** ingvarch
- **Related:** [ADR-0004](0004-layered-architecture.md), [ADR-0009](0009-server-discovery-fixed-ip-slots.md),
  [architecture §13](../architecture.md#13-lifecycle-flows)

## Context

Several things change a node's configuration: spec edits, Nomad upgrades, tent upgrades that render different
configuration, OS patches and certificate renewal.

Relevant facts:

- **Hetzner `user_data` is immutable after creation.** Only a disk-wiping rebuild can replace it.
- **Nomad officially supports** both in-place upgrades (swap the binary and restart) and rolling replacement.
- **Nomad's upgrade guide** requires servers first, one at a time, with health checks, then clients.
- **Nomad servers keep Raft state.** Losing quorum loses availability, so replacements must preserve quorum.
- **Nomad provides the building blocks:**
  - autopilot health returns HTTP 429 while unhealthy;
  - `transfer-leadership` exists since 1.7;
  - `leave_on_terminate` makes a server leave the peer set gracefully;
  - `drain_on_shutdown` drains a client on shutdown;
  - `client.default_ineligible` exists since 2.0.3.
- **kops on Hetzner makes the opposite choices.** It rolls without surge (`MaxSurge` defaults to 0 outside AWS),
  deletes before recreating, and re-applies the whole cluster plan per replaced node. A crash between delete and
  recreate leaves the group short.

## Decision

- **Nodes are immutable.** Any change of a node's rendered configuration, detected through the semantic
  `tent/spec-hash` label, is applied by replacing the node. tent never mutates live nodes over SSH.
- **`update` and `rolling-update` are separate.** `tent update cluster` never replaces existing nodes; it reports how
  many are outdated and why. `tent rolling-update cluster` performs the replacements.
- **Surge-first.** The replacement is created and healthy before the old node is removed. A crash therefore leaves a
  surplus, never a deficit, and the next run completes the work. One exception: `update` deletes a client that never
  registered before it creates its replacement ([ADR-0032](0032-joined-label-scrub-and-delete-guard.md)).
- **Servers are replaced one at a time**, only while autopilot is healthy and failure tolerance is ≥ 1:
  1. Create the new server in a free IP slot.
  2. Wait until it is a voter and autopilot reports healthy.
  3. If the old server is the leader, transfer leadership to an updated server.
  4. ACPI-shutdown the old server, which leaves Raft gracefully through `leave_on_terminate`.
  5. Wait until the peer count is back to N and autopilot is healthy.
  6. Delete the VM.

  Removing a raft peer by id is only the fallback for servers that died on their own.
- **Clients are replaced per group** with `maxSurge` / `maxUnavailable`:
  1. Create the surge node and wait until it is ready. It optionally starts ineligible and is enabled after node
     checks.
  2. Mark the old node ineligible.
  3. Drain it with a deadline, honouring `migrate {}` blocks.
  4. ACPI-shutdown the old node.
  5. Delete the VM.
  6. Purge the node in Nomad.
  7. Validate before the next batch.
- **Order.** All server groups roll before any client group. The planner refuses any state in which clients would
  run a newer Nomad than servers.
- **Targeted operations.** Replacements call provider `Nodes` primitives directly and never re-apply the full
  infrastructure plan.
- **Later.** In-place Nomad binary upgrades may be added as an explicit, opt-in strategy, because they are much
  faster. Replacement stays the default.

## Consequences

### Positive

- Nodes are reproducible from the spec, and OS patching happens by replacement onto fresh images.
- Interrupted rollouts are safe and resumable.
- Server replacement never drops below quorum.

### Negative / trade-offs

- Rollouts are slower: a new node needs roughly 1–3 minutes to boot and bootstrap.
- Surge needs spare capacity and quota. With Hetzner's default limit of 5 servers, a 3-server + 1-client cluster
  already needs all 5 during a server roll.
- Client node IPs change on replacement. Server IPs stay within the fixed slots.

### Follow-ups

- `internal/rollout` with pure decision functions ("state → next step") and golden sequence tests (M3).
- `nomadops` helpers: autopilot health with the 429 handling, leadership transfer, drain monitoring, node purge
  (M2–M3).

## Alternatives considered

- **In-place configuration management** (SSH/Ansible, or tent-node pulling new config). The drift-prone snowflakes
  immutable infrastructure avoids, plus a live control channel to every node.
- **Delete-then-create** (kops' Hetzner default). Capacity dips, and a crash leaves a missing node.
- **Hetzner rebuild with new user data**, which keeps the server id and IPs. It wipes the disk anyway, so it gives
  no surge and no quorum safety. It may be useful later to keep stable public IPs.
