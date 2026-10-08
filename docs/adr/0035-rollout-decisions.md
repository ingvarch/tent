# ADR-0035: Rollout decisions

- **Status:** Accepted; the refusal of two voters and of single-server groups is **provisional** (see
  [Server removal](#server-removal)); amended by [ADR-0036](0036-nomad-calls-of-a-roll.md) (the M3.2 follow-up is
  built: `nomadops` reads the Raft IDs, `StableSince`, the failure tolerance, the gossip members and the drain state);
  amended by [ADR-0037](0037-rolling-update-of-client-groups.md) (a new node never takes a name that Nomad lists a
  node of, in any status, which changes the name that a roll's create takes), and the maintainer answered the two open
  questions on 2026-10-08 (decisions 37 and 38, item 15): M3.4 builds the answer, and item 15's refusals stand until
  then
- **Date:** 2026-10-08
- **Deciders:** ingvarch
- **Related:** amends [ADR-0004](0004-layered-architecture.md),
  [ADR-0005](0005-immutable-nodes-and-nomad-aware-rollouts.md),
  [ADR-0016](0016-server-discovery-seed-and-refresh.md) and [ADR-0017](0017-api-driven-server-removal.md), and extends
  [ADR-0021](0021-import-rules.md) (see their Status lines); builds on
  [ADR-0019](0019-combined-server-client-role.md), [ADR-0031](0031-bootstrap-in-update.md) and
  [ADR-0032](0032-joined-label-scrub-and-delete-guard.md);
  [architecture §3.3](../architecture.md#33-api-rules), [§4.1](../architecture.md#41-three-responsibilities),
  [§5](../architecture.md#5-repository-layout-and-dependency-rules),
  [§13.3](../architecture.md#133-tent-rolling-update-cluster---yes),
  [§15](../architecture.md#15-testing), [§18](../architecture.md#18-open-questions),
  [platform notes §1.2](../platform-notes.md#12-features-tent-relies-on)

## Context

M3 adds the Day-2 operations: `tent rolling-update cluster`, and removals of nodes by `update` (decisions 33 to 35 of
[§18](../architecture.md#18-open-questions)). Both stop machines, drain Nomad nodes and remove Raft peers. A run can be
cut at any step, so its decisions must come from what the cloud and Nomad report now, never from memory of what the
last run did.

- **The risk is in the decisions.** A wrong order loses the quorum of the servers or evicts allocations onto a node
  that is about to go. That is what the tests must prove.
- **Facts measured on a local Nomad 2.0.7**, on 2026-10-07 without ACL and TLS, and those dated 2026-10-08 again with
  ACL, mTLS and strict client introduction ([platform notes §1.2](../platform-notes.md#12-features-tent-relies-on)):
  - A new server joins as a nonvoter, is healthy within seconds and votes about 20 s later (12 to 15 s on
    2026-10-08).
  - A leadership transfer resets `StableSince` on every server.
  - A server that is killed stays `alive` and healthy in autopilot's report for about 40 s.
  - A live server whose peer is removed is added to the Raft configuration again as a nonvoter, 39.8 s later on
    2026-10-07 and 4.1 s later on 2026-10-08. Autopilot makes it a voter again 9.6 s and 14 s later: the promotion
    came 49 s and 18 s after the removal.
  - A purged live node registers again without client introduction. Under strict introduction, with its
    introduction token expired, it was refused at every try (2026-10-08).
- **Two voters.** [ADR-0017](0017-api-driven-server-removal.md) orders a stop and then the removal of the peer. With
  two voters a stop leaves one of two alive, which is no quorum, so no leader is left to remove the peer. Two voters
  occur in the last step of a shrink from three servers to one, and in every roll of a single server.
- **Single servers.** [ADR-0005](0005-immutable-nodes-and-nomad-aware-rollouts.md) replaces servers only with a failure
  tolerance of at least 1, and one server always has 0.
- **The maintainer had not answered** these two questions when the decisions were built (asked on 2026-10-07). The
  safe answer is built (item 15).

## Decision

### The shape

1. **The decisions are pure functions of observed state.** `rollout.Next(State, Mode)` returns one `Step`, `Done` when
   nothing is left, or an error that matches `ErrRefused`. It reads only the `State`, which holds what the cloud lists
   and what Nomad reports, with a clock and the refresh interval as input. It calls nothing. A run that was cut is
   finished by the next call on the new observation, with the rest of the same steps.
2. **`internal/app` carries the steps out** from M3.3 on: it observes, calls `Next`, does the step, observes again,
   and polls a wait until the decision moves on or the wait's deadline passes. Nothing calls the decisions in M3.1.
3. **The steps** are `Done`, `Create`, `WaitJoined`, `MarkIneligible`, `Drain`, `WaitDrained`, `TransferLeadership`,
   `Stop`, `WaitServerDown`, `RemovePeer`, `ForceLeave`, `WaitHealthy`, `WaitStable`, `Delete`, `WaitNodeDown` and
   `Purge`. The six waits (`WaitJoined`, `WaitDrained`, `WaitNodeDown`, `WaitServerDown`, `WaitHealthy` and
   `WaitStable`) return `true` from `Action.Waits`. Every step is safe to repeat.
4. **Two modes.** `Roll` replaces outdated machines (`tent rolling-update cluster`). `Shrink` removes the machines
   beyond a group's size and creates nothing (`update`, from M3.6).
5. **The order of groups.** The server or combined group comes first, then the client groups by name. The first group
   that is not done gives the step.
6. **Rules every group shares.**
   - A machine is outdated when its spec hash differs from the group's, when it has none, or when it is forced.
   - Two listed machines of one name in a group are refused.
   - `Roll` never moves a node to an older Nomad: a new node's version older than a server's, or than any node
     that is not down, is refused before anything else. A version that does not parse is refused, naming its owner. A
     server that reports no version, as a server does for a moment after it joins, before autopilot reports it, is
     skipped here; a client group then waits (`WaitHealthy`) before it creates a node or starts a removal.
   - In `Roll` a group with more machines than its size and nothing outdated is left to `update`; a client group short
     of its size gets its missing nodes; a server group short of its size is refused.
   - A joined machine for which the cloud lists no private address cannot be matched to a Nomad node or server, since
     tent matches them by name and address.
     - A client group never deletes it and never takes it as a victim. It counts the machine as not available, goes
       on with its other victims while the budget allows, and is refused (C10) only when something is still outdated
       (`Shrink`: the group is above its size) and no other step is left.
     - A server or combined group refuses a running one at the checks at rest until the cloud lists an address. It
       counts a stopped one as a removal under way: when the group has more machines than its size and the machine may
       go (outdated in `Roll`), it is deleted once autopilot is healthy with one voter fewer.
   - A machine that has not joined gets `WaitJoined`; in `Shrink` it may be deleted instead (rule C7).
7. **The first match of the victim order decides** which machine a group loses: a machine whose removal has started;
   in `Shrink`, one that has not joined; an outdated one; a client that is not available or a server that autopilot
   does not count healthy; a server that does not lead; one in the zone with the most machines of the group; in
   `Roll` the oldest, in `Shrink` the newest. A roll takes the leader last, so it moves the leadership at most once.

### Client groups

8. **The rules are tried in this order:** C1, C2, C4, C5, C3, C6, C7, C8, C9, C10. The first that applies gives the
   step. The rules and their terms are in
   [architecture §13.3](../architecture.md#133-tent-rolling-update-cluster---yes).
9. **C5 (mark a victim ineligible) comes before C3 (drain) so that a batch is marked whole before any of it drains.**
   With `maxSurge` 2, both victims are marked ineligible first, and only then the first drain starts. The allocations
   that the first drain evicts then cannot land on a node that is about to drain. C4 (create) comes before C5, so a
   roll with `maxSurge` 2 creates both new nodes before the first victim is marked. A victim whose node is not
   available costs no availability, so a dead outdated node is replaced without waiting for the budget.
10. **Servers before clients.** A client group whose next step would come from C4 or C5 (`Create`, or the start of a
    new victim's removal) is refused while a server runs an older Nomad than a new node. Removals already under way
    finish first.

### Server removal

11. **With three or more voters the order is [ADR-0017](0017-api-driven-server-removal.md)'s:** after a stop `V - 1`
    voters are alive, which is still a quorum of `V` (`V/2 + 1`) when `V` is 3 or more. What changes is that each step
    is chosen from what is left to do: a peer that autopilot removed first is not removed again, and a server that
    the leader re-added as a nonvoter is stopped and removed again.
12. **The checks at rest come before a removal starts:** autopilot is healthy; every machine of the group is running
    and a voting server (combined: its node is ready, eligible and not draining too); the victim votes. A machine that
    has not joined is waited for before these checks. `Create` of a replacement needs a failure tolerance of at least
    1. A check that fails is a refusal. A combined victim is checked again after its drain (item 16).
13. **A server victim that leads hands the leadership over first,** to the first healthy, up-to-date voter by name
    (`Shrink`: any other healthy voter). With none, the run is refused.
14. **The stability window is `Refresh` plus 10 s.** `Refresh` is the interval of tent-node's `refresh-join` (one
    minute). A removal starts only when every voter other than the victim has had its `StableSince` for at least the
    window, so every node has refreshed its `05-join.hcl` since the servers last changed
    ([ADR-0016](0016-server-discovery-seed-and-refresh.md)). Otherwise the step is `WaitStable` until that time.
    - The window is read from autopilot, so a cut run sees it (see Alternatives for why not a sleep).
    - In a server group a leadership transfer resets `StableSince` on every server, so a roll of a server group waits
      once more after its one transfer: the window is checked again before the stop. In a combined group it is checked
      before the drain only (item 16).
    - A peer removal starts no window: a node keeps a dead address in its join list without harm while live servers
      are in it. A server that joins starts one.
15. **Two voters and single-server groups are refused for now (provisional).**
    - A removal that would take a group from two voters to one is refused: `removing <name> would leave one voter of
      two: tent does not take a group from two voters to one yet`.
    - A roll of a group of one server is refused: `a group of one server cannot roll: its failure tolerance is 0`.
    - Open for the maintainer: the order of a removal from two voters, and whether single servers may roll. The other
      answer is in Alternatives.
    - **Answered on 2026-10-08** (decisions 37 and 38 of [architecture §18](../architecture.md#18-open-questions)): the
      other answer. With two voters the live server's peer is removed first and the machine is stopped at once; a group
      of one server rolls without the failure-tolerance check, through two voters, with `--allow-single-server`. M3.4
      builds it and changes the two refusals and the goldens `server1` and `shrink_servers_3_1`; until then they stand.
16. **A combined group rolls like servers and is drained first.** The victim's node is marked ineligible (or drained at
    once when its drain meta already holds the machine's ID), drained with the group's `drainTimeout` and waited for,
    then the machine is removed as a server. A combined victim that leads hands the leadership over after the drain.
    The window is checked before the drain only: the victim has started with its mark, and neither the drain nor the
    transfer after it changes a member of the Raft configuration, so no window follows the transfer. The deleted
    machine's node becomes an orphan, which is purged once down.
    - A drain may last the whole `drainTimeout`, and a cut run may resume days later. So before a drained victim that
      still runs and votes hands its leadership over or stops, the server half of the checks at rest runs again
      (autopilot is healthy, every machine of the group runs and votes), and with more than two voters the failure
      tolerance must also be at least 1. A server whose machine stopped meanwhile, or that autopilot no longer counts
      healthy, is then a refusal, not a lost quorum.
17. **`Shrink` takes the same rules** with these differences: it creates nothing, it needs no failure tolerance, it
    ignores Nomad versions, and its victims are any machines while a group has more than its size. A machine that
    never joined is deleted or waited for as rule C7 says. A client shrink marks the whole batch before any drain. A
    server group shrinks one server at a time (5 to 3; 3 to 1 stops at two servers for now, item 15). A group of one
    server needs `--allow-single-server`, as the specs' validation asks today.

### Drains and purges

18. **The drain carries the meta `tent_machine=<machine ID>`.** A completed drain counts for a machine only when its
    meta holds that machine's ID, so a completed drain of an earlier machine of the same name never does. The drain's
    deadline is the group's `drainTimeout` (item 20).
19. **Only a node that Nomad lists as down is purged.** A purged live node registered again without client
    introduction (2026-10-07); under strict introduction, with its introduction token expired, it was refused at
    every try in the 2 minutes watched (2026-10-08). A node that is not down is waited for.

### The `rollingUpdate` fields (decision 33)

20. **`NodeGroup.spec.rollingUpdate`** has three fields, with defaults filled in by `SetDefaults` per role
    ([architecture §3.3](../architecture.md#33-api-rules) has the rest):

    | Field | Client group | Combined group | Server group |
    |---|---|---|---|
    | `maxSurge` | default 1 | field error | field error |
    | `maxUnavailable` | default 0 | field error | field error |
    | `drainTimeout` | default `1h` | default `1h` | field error |

    - `maxSurge` and `maxUnavailable` are pointers, since 0 is a value an operator sets on purpose (`maxSurge: 0` with
      `maxUnavailable: 1`). Negative values are errors, and so are both at 0: the group could not roll.
    - `drainTimeout` is a string in Go's duration syntax, parsed by `RollingUpdate.Drain`, and must be above zero,
      since Nomad reads 0 as a drain without a deadline.
    - Server and combined groups roll one node at a time, with one more node first, so a setting that a role would
      ignore is a field error, as `spec.nomad` is for `role=server`. It cannot mislead.
    - **The fields never change the spec hash:** nothing of them reaches NodeConfig. A test in `internal/app` pins it.

### The import rule and the proof

21. **`internal/rollout` imports only the standard library, `api/v1alpha1`, `internal/english` and
    `golang.org/x/mod/semver`.** The depguard rule is `rollout-pure`; tests are exempt.
22. **The helpers that `update`'s planner and the decisions share move into the package:** `NodeName`, `FreeName`,
    `LeastUsedZone` and `CompareCreated`. `planNodes` calls them there and behaves as before.
23. **A simulator proves the decisions.** The tests hold a model of the cloud and of Nomad with a clock of its own, and
    these checks:
    - golden step sequences for the main scenarios and refusals;
    - invariants after every step and every tick (list in [architecture §15](../architecture.md#15-testing));
    - a resume test: a copy of the world after each step and each tick, and from each copy a new run ends in the same
      cluster and prints exactly the rest of the full run; after up to 12 ticks without a step, a new run from each
      copy ends with the same last line in the same cluster, though the steps in between may differ;
    - quorum tests: a voter whose machine stops at any point of a roll, before or after autopilot notices it, never
      leads to a stop or a leadership transfer while it is down.

## Consequences

### Positive

- The order of stops, drains and removals is proven on a simulator from every state a roll passes through, before
  any code calls a cloud.
- `rollout` cannot reach a cloud, Nomad or the state store, so a decision cannot hide a side effect.

### Negative / trade-offs

- **A shrink from three servers to one and a roll of a single server are refused** until M3.4 builds the maintainer's
  answer (decisions 37 and 38).
- **A roll is slow per server.** The window is at least 70 s after a change, and `WaitServerDown` costs about 36 to
  66 s ([platform notes §1.2](../platform-notes.md#12-features-tent-relies-on)): Serf marked a killed server failed
  after 36.7 s, 41.2 s and 65.7 s. The wait proves that the machine stopped before its peer goes.
- **The input types hold more than `nomadops` reports today:** Raft IDs, `StableSince`, the failure tolerance, the
  gossip members and the drain state.

### Follow-ups

- M3.2 reads the missing facts from Nomad and repeats the measurements with ACL and mTLS. M3.3 wires `Roll` into
  `tent rolling-update cluster`; M3.6 wires `Shrink` into `update` and lifts the guard of decision 27 where tent
  drains or removes safely.
- The loop of M3.3 calls `Next` at every poll of a wait, never only once per wait: the voters that a `WaitHealthy`
  names can drop while it waits, when autopilot removes the peer of a dead server.
- The maintainer answered the two questions (item 15). M3.4 changes the two rules and the goldens `server1` and
  `shrink_servers_3_1`. A group of one server then rolls without the tolerance check, with `--allow-single-server`,
  and ADR-0017's order for two voters changes.

## Alternatives considered

- **A planner that calls the cloud and Nomad itself.** It would be one loop instead of a decision and a carrier. Its
  tests would need fakes of both services for every state, and a cut run could not be replayed from a state alone. The
  decisions would also share a package with code that is allowed to reach the cloud.
- **A sleep in place of the stability window.** ADR-0016's guard waits one refresh interval. A cut run cannot tell how
  long it has waited; the window is read from the servers' `StableSince`.
- **Marking and draining one victim at a time:** see item 9.
- **Stop first with two voters, as ADR-0017 orders.** It leaves one of two voters alive, so no leader can remove the
  peer, and the cluster has no quorum.
- **Remove the peer before the stop with two voters.** It kept the quorum on 2026-10-07, and it is the answer that would
  let single servers roll. It is the maintainer's answer of 2026-10-08 (decisions 37 and 38), which M3.4 builds; until
  then item 15 refuses it. The leader re-adds the removed live server as a nonvoter, and autopilot promotes it 18 s
  (2026-10-08) and 49 s (2026-10-07) after the removal. If the promotion lands between the observation and the stop, two
  voters remain with one of them stopped and the quorum is lost. The stop must come before the shortest time seen, or a
  guard must see the promotion.
- **Purge a node that is not down.** Without client introduction a purged live node registers again, so the purge
  would not remove it. Under strict introduction, once its introduction token had expired, it could not register
  again (2026-10-08), so the purge would cut off a node whose agent still runs.
