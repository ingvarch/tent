# ADR-0017: Remove Nomad servers through the Nomad API; ACPI shutdown is an optimization

- **Status:** Accepted; amended by [ADR-0030](0030-nomad-on-nodes.md) (clients have no `drain_on_shutdown`, so the
  drain through the Nomad API is the only drain of a client before its removal; whether servers keep
  `leave_on_terminate` is decided in M2.7) and by [ADR-0031](0031-bootstrap-in-update.md) (decision 26: server and
  combined agents run with `leave_on_terminate = false`, so a stopped server stays a Raft peer on every provider,
  an ACPI shutdown included, until step 5 removes it through the API or autopilot's `cleanup_dead_servers` does
  first; clients keep `true`) and by [ADR-0035](0035-rollout-decisions.md) (the order holds with three or more voters;
  with two voters a stop would leave no quorum, so tent refuses the removal for now, and the other answer, to remove
  the live server's peer before its stop, is not built; a server is stopped only after the stability window; the
  leadership goes to a healthy, up-to-date voter; a client's VM is deleted after its drain and its node purged only
  once Nomad lists it down)
- **Date:** 2026-09-25
- **Deciders:** ingvarch
- **Related:** amends [ADR-0005](0005-immutable-nodes-and-nomad-aware-rollouts.md); [ADR-0016](0016-server-discovery-seed-and-refresh.md),
  [architecture §13.3](../architecture.md#133-tent-rolling-update-cluster---yes)

## Context

[ADR-0005](0005-immutable-nodes-and-nomad-aware-rollouts.md) removes an old server gracefully with an ACPI shutdown.
Systemd then stops Nomad, and `leave_on_terminate` makes the server leave the Raft peer set.

That does not carry over to every cloud:

- **Vultr** has no graceful shutdown in its API. `halt` is a hard power-off, and `DELETE` destroys a running
  instance immediately.
- **Nomad** has no HTTP endpoint that makes a remote agent leave gracefully. What exists:
  - `DELETE /v1/operator/raft/peer?id=` removes a Raft peer;
  - `PUT /v1/agent/force-leave?node=…&prune=true` (`agent:write`) removes a member from the Serf gossip pool;
  - if the member is still alive, it rejoins.
- **Surge-first keeps quorum through an abrupt stop.** The replacement is a voter before the old server goes, so
  when one of four servers stops abruptly, three of four are still alive.

## Decision

- **One removal path for every provider:**
  1. The surge replacement is a healthy Raft voter.
  2. If the old server is the leader, transfer leadership to an updated server
     (`PUT /v1/operator/raft/transfer-leadership`).
  3. Stop the old VM.
     - Providers with `GracefulShutdown` (Hetzner) send an ACPI shutdown, so `leave_on_terminate` performs a graceful
       leave.
     - The others (Vultr) stop the VM hard or delete it.
  4. Wait until autopilot no longer reports the old server as a healthy voter.
  5. If it is still a Raft peer, `DELETE /v1/operator/raft/peer?id=<raft id>`. Then
     `PUT /v1/agent/force-leave?node=<name>&prune=true`.
  6. Wait until the peer count is back to N and autopilot is healthy. Delete the VM if that has not happened yet.
- **Clients:**
  1. Drain through the Nomad API. This already happens before the stop, so `drain_on_shutdown` is only a safety net.
  2. Stop or delete the VM.
  3. Purge the node.
- **Capability `GracefulShutdown`.** Providers declare whether they support it. The core treats a graceful stop as an
  optimization: steps 4–6 run either way, and are no-ops when the leave was graceful.

## Consequences

### Positive

- One tested flow for every provider, with no SSH and no agents or tokens on nodes.
- Explicit verification replaces trust in shutdown hooks.

### Negative / trade-offs

- **A short window of failed-member alerts** on hard-stop providers until the peer is removed. Autopilot's
  `cleanup_dead_servers` may race the explicit removal, which is harmless because both are idempotent.
- **A hard stop gives Nomad no chance to flush.** Acceptable: the server's Raft data is discarded with the VM, and the
  surviving servers keep quorum.

### Follow-ups

- `nomadops`: leadership transfer, peer removal, force-leave with prune, and autopilot health that handles 429 (M3).
- Golden sequence tests for both flavours, graceful and hard (M3).

## Alternatives considered

- **SSH into the server and stop Nomad.** Adds an SSH dependency to the lifecycle, which
  [ADR-0006](0006-two-binaries-and-nodeconfig.md) rejects.
- **tent-node watches for a "retire" signal** (a Nomad Variable, a metadata flag). It needs a token or polling on
  every node and adds complexity for little gain.
- **Remove the Raft peer before stopping the VM.** The leader's reconciliation would re-add a live server.
- **Keep ACPI only.** Impossible on Vultr.
