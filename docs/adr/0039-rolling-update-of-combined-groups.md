# ADR-0039: Rolling update of combined groups

- **Status:** Accepted; amended by
  [ADR-0040](0040-scale-down-through-the-rollout.md) (a combined group shrinks in `update` with
  `Shrink`; follow-up built)
- **Date:** 2026-10-12
- **Deciders:** ingvarch
- **Related:** amends [ADR-0005](0005-immutable-nodes-and-nomad-aware-rollouts.md),
  [ADR-0017](0017-api-driven-server-removal.md), [ADR-0019](0019-combined-server-client-role.md),
  [ADR-0031](0031-bootstrap-in-update.md), [ADR-0032](0032-joined-label-scrub-and-delete-guard.md),
  [ADR-0035](0035-rollout-decisions.md), [ADR-0037](0037-rolling-update-of-client-groups.md) and
  [ADR-0038](0038-rolling-update-of-server-groups.md) (see their Status lines); builds on
  [ADR-0036](0036-nomad-calls-of-a-roll.md); [architecture §9.3](../architecture.md#93-client-introduction),
  [§13.3](../architecture.md#133-tent-rolling-update-cluster---yes), [§14](../architecture.md#14-cli),
  [§15](../architecture.md#15-testing), [§17](../architecture.md#17-risks),
  [§18](../architecture.md#18-open-questions) (decision 49),
  [platform notes §1.2](../platform-notes.md#12-features-tent-relies-on),
  [§1.6](../platform-notes.md#16-the-agent-on-a-node) and [§3.16](../platform-notes.md#316-spike-runs)

## Context

[ADR-0037](0037-rolling-update-of-client-groups.md) rolls client groups and
[ADR-0038](0038-rolling-update-of-server-groups.md) server groups. A combined node is both.
[ADR-0035](0035-rollout-decisions.md) item 16 already gives a combined victim a drain, a leadership transfer and
the server steps, but the loop carried out none of them. The facts that decide the rest were measured on a local
Nomad 2.0.7 with ACL, mTLS, gossip encryption and client introduction on `warn`:

- **A new combined node registers about 15 s before its server votes** ([platform notes
  §1.6](../platform-notes.md#16-the-agent-on-a-node)). The roll scrubbed a machine that is no server once its node was
  ready, so a combined machine would have been labelled joined early and the next decision would have refused it:
  `node prod-all-3 is not a voting server`.
- **A drain can last the group's `drainTimeout`, one hour by default,** and the polls of a drain list nothing. The
  checks at rest run only before a removal starts. A run cut after the mark can resume at any time, and nothing
  looked at the other nodes: one of them could be down or ineligible meanwhile, and the drain would stop allocations
  that have nowhere to go.
- **A node of a leader that stops soon after its leadership transfer reads `ready` for 300 s.** After a leader change
  Nomad gives every node a heartbeat timer of `failover_heartbeat_ttl`, 300 s, until its next heartbeat. Stopped 70 s
  after the transfer, the node reads `down` in about 15 s ([platform notes
  §1.2](../platform-notes.md#12-features-tent-relies-on)). Without the rule of item 5, tent would wait about four
  minutes for the node of a drained leader, once per roll.
- **`replace` and `edit` let a node group's role change,** while the rules go by the group's role and a machine keeps
  the role it was made with. Since M3.4 `rollout` refuses such a group
  ([ADR-0038](0038-rolling-update-of-server-groups.md) item 9), so the operator learns of the change only at the next
  roll. Without that guard a group changed from combined to server would have its machines stopped with no drain, and
  one changed from server to combined would fail the checks at rest (`node prod-servers-0 has no node in Nomad`).

## Decision

This ADR records maintainer decision 49 (2026-10-12) and builds decisions 33, 38, 43 and 44 for combined groups.
Real-cloud runs on Vultr on 2026-10-12 are in [platform notes §3.16](../platform-notes.md#316-spike-runs).

### The steps

1. **A combined group rolls one node at a time, with one more node first, and the loop carries out every step that
   `rollout` decides** ([ADR-0038](0038-rolling-update-of-server-groups.md)), with a drain before the transfer and the
   stop. The refusal of combined groups that ADR-0038 states as built is gone from the loop and from the plan. The group
   takes its `drainTimeout` and none of `maxSurge` and `maxUnavailable`. The new node gets the seed of the listed server
   and combined machines and no intro token. The flow is in [architecture
   §13.3](../architecture.md#133-tent-rolling-update-cluster---yes) (decisions: `combined3.golden`).
2. **A combined node has joined when its server votes at its private address and autopilot counts it healthy, and its
   node is ready and eligible at its name and private address.** Both halves are needed because the node is ready
   about 15 s before the server votes (Context). The roll labels and scrubs the machine after both, as `update` does
   ([ADR-0032](0032-joined-label-scrub-and-delete-guard.md)). A server and a client keep their rules. The wait has the
   event `join` (`NomadJoin`) and a limit of 10 minutes; at the limit the error names the half that is missing. A
   combined node is never refused for its age, since it has no intro token.

### The drained victim

3. **A combined group is marked and drained only while the other nodes can take the work.** `MarkIneligible` and
   `Drain` are refused unless every other machine of the group runs and has a node that is ready, eligible and not
   draining. Reason: a resumed run can find the other nodes changed during a drain that lasts up to `drainTimeout`,
   and the checks at rest do not run again. A drain that has begun is waited for, since Nomad runs it already, and the
   servers are not looked at. The rule applies to `Roll` and `Shrink`; a client group has `maxUnavailable` and a server
   group has no nodes. A victim whose machine someone stopped while Nomad still lists its node up and eligible gets the
   same refusal while another node cannot take work, and goes on with the server steps once Nomad lists its node down.
   The texts are in [architecture §13.3](../architecture.md#133-tent-rolling-update-cluster---yes).
4. **Before a leadership transfer or a stop the loop lists the machines and reads Nomad again,** because a drain can
   last an hour. A machine halted meanwhile ends the run before any write. The settle of
   [ADR-0038](0038-rolling-update-of-server-groups.md) item 6 applies: a run that has sent a write waits up to a
   minute for the refusal to clear (a transfer's blip of about 2 s included), and a run that resumes at the drained
   victim ends at once.
5. **A drained victim that led waits the stability window after its transfer, for every group size.** Line e1 of
   [ADR-0035](0035-rollout-decisions.md) ([ADR-0038](0038-rolling-update-of-server-groups.md) item 11) is the rule: the
   window is 70 s from the transfer, since the new leader gives every server a new `StableSince`, and the victim's
   client heartbeats to the new leader before its machine stops. Its node then reads down about 20 s after its stop
   and not 300 s after the transfer. The window is decided from `StableSince`, so a resumed run waits what is left.

### Names and the old node

6. **A new combined node takes the index above the highest name of its group** (decisions 43 and 48,
   [ADR-0038](0038-rolling-update-of-server-groups.md) items 7 and 22): a roll of `prod-all-0` to `-2` makes `-3` to
   `-5`. Its Nomad node has the machine's name. A new machine never has an old node's name, also when Vultr gives it a
   deleted machine's private address, and a machine has a node only at its own name and address.
7. **The old machine's node is purged once Nomad lists it down,** before the next `Create`
   ([ADR-0035](0035-rollout-decisions.md) item 19). A node of the group's name pattern that no listed machine has at its
   name and address is an orphan. `rollout.NextIndex` reads no nodes: the purge comes before the create, so Nomad lists
   a node of the pattern only for a listed machine.
8. **A group of one node rolls through two voters** (decision 38) with `--allow-single-server`, after the new node has
   joined; the steps are those of [ADR-0038](0038-rolling-update-of-server-groups.md) items 12 to 15 with a drain
   first (`combined1.golden`, [architecture §13.3](../architecture.md#133-tent-rolling-update-cluster---yes)). No wait
   for the server to go down stands between the stop and the delete, so the node reads `down` after the delete and
   `WaitNodeDown` waits for it. The drain needs the new node: the rule of item 3 refuses the mark and the drain of the
   only node into an empty cluster, also in a run that resumes.

### A group keeps its role (decision 49)

9. **`replace` and `edit` refuse a change of a node group's role.** The maintainer decided it on 2026-10-12 (decision
   49). tent has no way to change the role of a stored group: a cluster has exactly one server or combined group and no
   command deletes a node group, so a cluster of the other role is a new cluster. The text of the field error and the
   limits of the check are in [architecture §14](../architecture.md#14-cli).
   - **The guard stays.** `rollout` still refuses a group that lists a machine of another role, for a cluster that was
     changed before this refusal existed (see [ADR-0038](0038-rolling-update-of-server-groups.md) item 9). The
     texts of both refusals are in [architecture §13.3](../architecture.md#133-tent-rolling-update-cluster---yes) and
     [§14](../architecture.md#14-cli). Such a cluster cannot get its old role back, since `replace` and `edit` refuse
     that change too: it is replaced by a new cluster, and the refusal says so.

### A group back at its size

10. **Before a create, the node of an outdated machine whose removal has started is not asked to take work.** A group
    at its size has no victim, and the checks before a create asked every node to be ready, eligible and not
    draining. So a roll stopped for good when its group came back to its size with a marked node: an old machine that
    fails while another drains is removed by the run, and every later run was refused with `node prod-all-1 is not
    eligible`, until someone made the node eligible by hand. The create still asks that every machine runs and votes,
    that autopilot reports the servers healthy and that the cluster can lose a voter. The marked machine is the
    victim as soon as the new node has joined. tent cannot tell its own mark from an operator's, so an outdated node
    that someone made ineligible or drains no longer holds a roll back and is replaced first; an up-to-date node that
    is ineligible keeps the refusal. The final review of the part found the fault with a probe.

## Consequences

### Positive

- A combined cluster, one made of a single node included, rolls with its drain first, and a run that is cut at a
  write, or loses the answer of one, is finished by running the command again.
- A resumed run does not drain a node while another node cannot take work, and a machine halted from outside during a
  drain keeps the run from sending the transfer or the stop.
- No roll waits four minutes for the old leader's node.
- A group's role cannot change by accident.

### Negative / trade-offs

- **Room for the drained allocations.** tent checks that the other nodes are ready, eligible and not draining, and not
  that they have the memory or meet the constraints of the allocations. A job bound to the old node by a constraint or
  a host volume waits for the drain's deadline, up to `drainTimeout`, and is stopped then.
- **A system task may be cut at a stop,** since a drain marks a system allocation `stop` before its task ends (platform
  notes §1.2). This holds for client rolls since M3.3 as well, and it was not seen on Vultr.
- **A roll takes long** (about 6 min for one node, 14 min for three, [platform notes
  §3.16](../platform-notes.md#316-spike-runs)), and every release makes combined nodes outdated. An operator who wants
  to defer it rolls the client groups with `--nodegroups`.
- **Two voters for the whole drain in a roll of one node.** The cluster survives the loss of neither for up to
  `drainTimeout`. A short `drainTimeout` bounds it.

### Follow-ups

- **M3.9** builds the E2E scenario with a combined group (`upgrade`), which
  [ADR-0019](0019-combined-server-client-role.md) asked for and issue #100 lists.
- **M3.6** lets `update` shrink a combined group with `Shrink` and name a new combined node above the remembered index.
  Until then `update`'s planner names from the machines alone.

## Alternatives considered

- **Stop the old leader at once after its transfer and wait for its node to read down.** Rejected: the node reads
  `ready` for 300 s, so every roll waits about four minutes (Context).
- **Wait for the node of every combined victim to read down before its delete.** Rejected: after a stop with the window
  the node reads down before the delete in a roll of three, and the wait that exists, `WaitNodeDown`, covers the roll of
  one.
- **Scrub a combined machine once its node is ready, as for a client.** Rejected: it is labelled before its server
  votes (Context).
- **Check the other nodes at the observation before the mark, with a new list.** Rejected: a mark and a drain cost no
  quorum, but the window before them (55 to 70 s) passes without a new list, so the check would be stale at the drain.
- **Refuse the drain in the loop and not in `rollout`.** Rejected: the decisions are pure and carry the rule for `Roll`
  and `Shrink` alike, and the simulator proves them.
- **Let the role change and rely on the guard in `rollout`.** Rejected: the guard refuses the roll only, and an
  operator would find out at the next roll. The maintainer chose the refusal at `replace` and `edit`.
- **A way back for a cluster whose role changed before the refusal:** a flag on `replace` and `edit` that lets a
  change through, or the guard in `update`, which lists the machines, in place of the one in `replace` and `edit`.
  Rejected by the maintainer on 2026-10-12: only a cluster changed by tent v0.1.0 or by hand is in that state, and it
  is replaced by a new cluster.
