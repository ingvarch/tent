# ADR-0038: Rolling update of server groups

- **Status:** Accepted
- **Date:** 2026-10-10
- **Deciders:** ingvarch
- **Related:** amends [ADR-0005](0005-immutable-nodes-and-nomad-aware-rollouts.md),
  [ADR-0010](0010-state-store-and-locking.md), [ADR-0016](0016-server-discovery-seed-and-refresh.md),
  [ADR-0017](0017-api-driven-server-removal.md), [ADR-0027](0027-nodeconfig-contract-rendering-and-spec-hash.md),
  [ADR-0030](0030-nomad-on-nodes.md), [ADR-0031](0031-bootstrap-in-update.md), [ADR-0035](0035-rollout-decisions.md),
  [ADR-0036](0036-nomad-calls-of-a-roll.md) and [ADR-0037](0037-rolling-update-of-client-groups.md) (see their Status
  lines); builds on [ADR-0015](0015-idempotency-without-unique-names.md); [architecture
  §8.4](../architecture.md#84-nomad-configuration-rendering), [§10.2](../architecture.md#102-layout),
  [§13.3](../architecture.md#133-tent-rolling-update-cluster---yes),
  [§13.7](../architecture.md#137-tent-delete-cluster---yes), [§14](../architecture.md#14-cli),
  [§15](../architecture.md#15-testing), [§18](../architecture.md#18-open-questions) (decisions 37, 38, 43, 44 and 48),
  [platform notes §1.2](../platform-notes.md#12-features-tent-relies-on) and [§3.3](../platform-notes.md#33-instances)

## Context

[ADR-0037](0037-rolling-update-of-client-groups.md) carries out the steps of [ADR-0035](0035-rollout-decisions.md)
for client groups. M3.4 carries out the steps of server groups, which put the quorum of the cluster at risk. A run can
be cut anywhere, and nobody watches the cluster between two runs.

- **Nothing shows a halted server for a while.** Vultr lists a halted instance as running for 4 to 19 s, and
  autopilot counts a killed server healthy until Serf marks it failed, 36 to 66 s later
  ([platform notes §3.3](../platform-notes.md#33-instances) and
  [§1.2](../platform-notes.md#12-features-tent-relies-on)).
- **A leadership transfer can leave autopilot unhealthy for about 2 s.** After 8 of 52 transfers it answered 429 with
  one server unhealthy until 2.07 to 2.29 s after the transfer (2026-10-08, local Nomad 2.0.7 with ACL, mTLS and
  gossip encryption).
- **A new server under the name of a removed server failed to join.** The leader added it as a nonvoter and autopilot
  removed it again within 40 ms, at every reconcile for the 2.6 minutes watched. New names joined and voted in 10 to
  23 s (2026-10-08).
- **A list can miss the highest name.** The rule of decision 43 (the index above the highest listed name) fails once
  nothing lists the highest name. A forced roll of a group of one was cut after its create and forced again. The new
  server became the victim, and after its delete the next create took its name, which fails to join (bullet above). A
  shrink or an operator's delete frees the highest name the same way.
- **A server that joins a cluster of one and has `bootstrap_expect = 1` starts a cluster of its own.** It led alone 1.4
  s after its start and the first server never added it (95 s watched). Without `bootstrap_expect` it joined as a
  nonvoter after 0.5 s and voted after 12.8 s (2026-10-09, local Nomad 2.0.7).
- **Nomad's leader adds a removed server whose member is alive again, every 60 s from the moment it took the
  leadership, and autopilot promotes the server 9.6 to 20 s later, also after its machine has stopped.** In three
  trials the stopped server was promoted 19.9, 10.2 and 20.2 s after the re-add and the leader lost its quorum 0.4 to
  0.5 s later (2026-10-10, local Nomad 2.0.7). With two voters that is a cluster that cannot elect a leader.
- **Every tent release changes the spec hash of every node,** since tent-node's version is in it
  ([ADR-0027](0027-nodeconfig-contract-rendering-and-spec-hash.md)), so each release makes the servers outdated.
- **A healthy worker read down about 19 s after the server it talked to was halted** (real-cloud run, 2026-10-10,
  Nomad's defaults). Its allocation was marked lost and replaced on the same node 19 s later, and the old task was
  killed, while the worker ran well ([platform notes §1.2](../platform-notes.md#12-features-tent-relies-on)).

## Decision

This ADR records maintainer decisions 37, 38, 43, 44 and 48 as built in M3.4. Decision 48 is built for the roll; the
planner of `update` follows with the scale-down part (item 22). Real-cloud runs on Vultr on 2026-10-10 are in
[platform notes §3.16](../platform-notes.md#316-spike-runs).

### The server steps

1. **The loop carries out the steps that `rollout` decides** for a server group, one server at a time with the new one
   first: `Create`, `WaitJoined`, `WaitStable`, `TransferLeadership` when the victim leads (and `WaitStable` once
   more, since a transfer resets `StableSince`), `Stop`, `WaitServerDown`, `RemovePeer`, `ForceLeave`, `WaitHealthy`,
   `Delete`. `update` and the roll create a server through one path. A server has joined once its server votes at its
   private address and autopilot counts it healthy; the loop then scrubs the machine and labels it joined. A server
   votes 10 to 23 s after it starts (2026-10-08), so this wait is the longest of a create.
2. **A stop or a transfer rests on a fresh list.** When `Next` gives `Stop` or `TransferLeadership` on an observation
   that did not list the machines, the loop lists them and asks `Next` again. The list may be a minute old, and a server
   that someone halted meanwhile still reads ready and healthy. A halted server that is outdated becomes the victim, so
   the run removes it and stops no running server meanwhile; a halted server that is up to date fails the checks at
   rest. The first observation of a run counts as one that did not list.
3. **The Nomad API follows the servers.** The loop calls the joined, running machines of the server groups that have a
   public address. It makes the API again after a list that changes them, and before a stop. A new server joins the API
   once it is labelled joined, and a victim leaves it before its stop and before the removal of its peer while it
   runs. `Servers` tries the server that answered last first, and a halted server answers nothing, so the first call
   after a halt would wait the call timeout of 30 s; a deleted server's address can belong to another machine later.

### Stops and waits

4. **A stop is sent once, and a stop that the cloud lists late is waited for.** While a machine that the run stopped is
   listed as running, every observation lists the machines and the stop that `Next` gives again is not sent. The wait
   is no try of the repeat guard and lasts 2 minutes from the stop, since Vultr lists a halted machine as running for 4
   to 19 s and three tries 2 s apart would end the run before. A new run sends the stop again, which Vultr answers with
   204.
5. **Every server wait has a limit and counts from its own first sight.** The waits of a removal name their victim
   (item 8). A stopped server counts healthy until Serf fails it, 36 to 66 s later, so the wait for autopilot to stop
   counting it has a limit of its own. Limits and texts: [architecture
   §13.3](../architecture.md#133-tent-rolling-update-cluster---yes).
6. **A refusal after a write is waited for, up to a minute.** A refusal of the decisions before the run has sent a
   write ends the run at once; the plan shows it too. Once the run has sent a write, whatever the answer, a refusal
   starts a settle wait: every 2 s the loop lists the machines, reads Nomad and asks `Next`, until the decisions give a
   step, a wait or `Done`, or the refusal has lasted a minute from its first showing. It is no try of the repeat guard
   and forgets no deadline. A leadership transfer can leave autopilot unhealthy for 2 s, so without it about one roll
   in seven of a leading victim would end with `autopilot reports the servers unhealthy` right after its transfer.
   - A cut right after a transfer meets the blip at the next run's first decision, and that run ends at once;
     running the command again goes on.
   - The scrub of a `WaitJoined` is no write for this rule: a run that begins at a `WaitJoined`, scrubs, and then
     meets a refusal ends at once. The next run goes on.

### Names and roles

7. **A new server or combined node takes the index above the highest that tent remembers for its group and above the
   highest that a listed machine, a server of the Raft configuration or a member of the gossip pool has** (decisions
   43 and 48, item 22). A roll of three servers makes `prod-servers-3`, `-4` and `-5`, and the next roll `-6`, `-7`
   and `-8`. Client groups keep the rule of [ADR-0037](0037-rolling-update-of-client-groups.md).
8. **The waits of a removal name their victim.** `WaitStable` and the `WaitHealthy` of a server removal carry the
   victim's machine, so their deadlines are per victim. The plan's `next` names the victim in its `node` and `id`
   fields with `-o json` and `-o yaml`; the text line does not. A client group's `WaitHealthy` carries none.
9. **A group whose role changed is refused.** `replace` and `edit` let a role change, the rules go by the group's role
   and machines keep the role they were made with, so a group changed from combined to server would have its machines
   stopped with no drain. `Next` refuses a group that lists a machine of another role. The text is in
   [architecture §13.3](../architecture.md#133-tent-rolling-update-cluster---yes).

### Two voters and single servers (decisions 37 and 38)

10. **A server that joins a cluster of one gets no `bootstrap_expect`.** `bootstrapExpect` is the number of servers of
    the specs, except for a cluster of one server when the node has a seed: then it is 0 and `10-node.hcl` has no
    `server` block. The first server of a cluster has no seed and keeps `bootstrap_expect = 1`; servers of groups of
    three and five keep `bootstrap_expect = N`, with or without a seed. A server with neither is refused:
    `a server with no bootstrap_expect needs servers to join`. The spec hash does not change, since `10-node.hcl` is a
    per-node file. The two joining forms are goldens that the weekly online job validates with `nomad config
    validate`.
11. **A running voter's server neither stops nor loses its peer while the window over the other voters is open** (line
    e1 of [ADR-0035](0035-rollout-decisions.md), item 14). A server victim has not started, so its checks and its window
    come first and this adds nothing. A drained combined victim has started: it waits for the window after its transfer,
    70 s, as the new leader's autopilot gives every server a new `StableSince`. After a leader change Nomad gives every
    node a heartbeat timer of 300 s, so a drained leader waits 70 s for the window ([platform notes
    §1.2](../platform-notes.md#12-features-tent-relies-on)). Beside fewer than two other voters the window also keeps a
    removal from meeting the new leader's reconcile within a second of the transfer, which would undo it at once.
12. **With fewer than two other voters the peer goes first, while the server runs.** The order is: the window, then
    `RemovePeer`, `Stop`, `ForceLeave`, with no `WaitServerDown`. With three or more voters the order stays
    [ADR-0017](0017-api-driven-server-removal.md)'s. A running victim with a server in the Raft configuration gets its
    `RemovePeer` whether the server votes or not, since a nonvoter that autopilot counts healthy was promoted after its
    machine stopped: in the three trials ([platform notes §1.2](../platform-notes.md#12-features-tent-relies-on)) the
    agent was stopped 3.1 to 5.0 s after the re-add and still promoted, while a nonvoter stopped 0.1 s after its re-add,
    before the report listed it healthy, was never promoted. A group of one server passes the tolerance check at
    `Create`, and rolls through two voters with `--allow-single-server`. The two refusals of
    [ADR-0035](0035-rollout-decisions.md) item 15 are gone.
13. **The stop of such a server is held until a reconcile that the run saw.** It applies to a `Stop` of a running
    machine with no server in the Raft configuration, beside fewer than two voters. The leader adds the removed server
    at its next reconcile and autopilot promotes it 10 to 20 s later, and the run does not know when the next
    reconcile comes. The loop sends the stop in one of two cases:
    - a) **Right after a re-add that it saw.** An observation showed the server out of the Raft configuration, the
      next one at most 10 s later showed it as a nonvoter, the run has removed that peer since, and the observation of
      the re-add is at most 10 s old. The stop call then goes out at most 20 s after the reconcile, and the next one
      is 40 s or more away.
    - b) **After 65 s without a re-add,** counted over observations at most 10 s apart, when the victim's member is
      not `alive`. The leader adds only a member that reads alive, and a pass that fails adds nothing for a minute, so
      the 65 s alone prove nothing. The stop goes at the first poll 65 s or more after the first removal, about 66 s on
      the 2 s poll.

    Otherwise the loop polls and sends no stop: it lists the machines, leaves the victim out of the API and forgets the
    step it carried out last, so that the removal of a re-added server is a first try with no poll before it. A nonvoter
    that the run did not see appear is removed and lets no stop go. The holds of one victim last 5 minutes together;
    then the run ends with no stop sent. Ages, a resumed run and a lost answer of a removal are in [architecture
    §13.3](../architecture.md#133-tent-rolling-update-cluster---yes).
14. **What the hold rests on.** After a cut right at the stop call, the cloud carries the halt out before the leader's
    next reconcile, 40 s or more later, or not at all (Vultr halted within 0.24 to 0.47 s after the call began, nine
    halts). While the run stays, the cloud carries the halt out within 2 minutes or not at all, and the run's reads
    answer, so it sees a re-add within a poll and removes the server before a promotion (9.6 s at the earliest). No
    reconcile but the timer's adds the victim between the re-add and the stop: a change of the leader does, and so does
    a restart of the victim's own agent (read in Nomad's source, not run); a run that stays removes such a victim, one
    that was cut does not. In case b) the victim's member stays not alive while its machine is halted.
15. **A held stop whose call fails counts as sent.** Any failure while the run lives, an error or no answer, may still
    be carried out later, and `internal/app` cannot tell a refused halt from an unknown outcome. So the machine stays
    among those being stopped and out of the API, the CLI shows `failed to stop node ...` and one warning, and the run
    lists the machines and removes the server at once if the leader adds it again, until the list shows the machine
    stopped or 2 minutes have passed. If the list shows it stopped before the leader's next reconcile, the run forces
    the member out and the re-add never happens; if the list still shows it running at the reconcile, the run removes
    the server again. A stop beside two or more other voters keeps the older rule: a failed call ends the run, since the
    quorum does not depend on when that machine stops. A halt that the cloud refuses with an answer is shown at once and
    ends the run only after the 2 minutes.
16. **A stopped victim that votes again is refused, and the operator starts its instance.** If the leader added a
    stopped victim as a voter, the servers have no quorum and Nomad answers no read. The decisions refuse it, and when
    no server answers while a joined server machine is listed stopped, the error of the run and of the plan says to
    start that instance again. The cluster had a leader 0.3 to 1.3 s after the stopped agent was started again
    (2026-10-10). `cloud.Nodes` has no start. The texts are in [architecture
    §13.3](../architecture.md#133-tent-rolling-update-cluster---yes). A victim whose member reads alive and that the
    leader never adds again (a failing reconcile; a server in bootstrap mode, which the leader never adds) ends the run
    at the hold's limit: stop the Nomad agent on that node and run the command again, and case b) of item 13 lets the
    stop go.

### Releases and twins

17. **tent-node stays in every node's spec hash** (decision 44), so each tent release makes the servers outdated and
    the default selection rolls them: about 4 minutes per server on Vultr (12 min 5 s for three on 2026-10-10), with
    three new names. The operator who wants to defer them rolls the client groups with `--nodegroups`. With outdated
    servers the default plan now has a next step, and `--exit-code` exits 2 where it was a refusal.
18. **A cut a second after a create can leave a twin server in `update`; a roll makes none.** `update` names a new
    server from the listed machines alone, so an `update` right after a cut run, whose list misses the machine,
    creates a second server of the same name. `rollout` refuses both
    (`node group servers: machines instance-9 and instance-10 share the name prod-servers-3; run tent update cluster
    first`). `update` deletes the twin that has not joined, since its guard asks Nomad and not the
    label. When both booted and entered the Raft configuration, both commands refuse, and the operator deletes one
    machine in the cloud. Whether two live servers of one name can both join the gossip pool is not measured. A roll
    reads the remembered index (item 22), so it makes no twin.

### Clients when a server stops hard

19. **tent renders `rpc { keep_alive_interval = "5s" }` into `00-tent.hcl` of client and combined nodes and
    `heartbeat_grace = "20s"` into the `server` block of server and combined nodes.** Decided on 2026-10-10, on the
    maintainer's instruction to fix the finding inside this part. It is no numbered maintainer decision.
    - **Why.** A client sends every RPC to one server, and its heartbeat RPC has no deadline. With Nomad's defaults
      only yamux's keepalive ends the session to a server that stopped answering: a ping every 30 s that fails after
      10 s. The leader marks the node down after its TTL of 10 to 20 s plus a grace of 10 s. So a client of a halted
      server reads down before it has reached another server, and its allocation is lost and replaced. A keepalive of
      5 s ends the session 10 to 15 s after the halt, and a grace of 20 s leaves the client time to heartbeat another
      server. A server that dies unplanned is covered the same way.
    - **Effect.** In the lab, 0 of 13 clients of a frozen server read down with the two settings and 19 of 24 with the
      defaults. On Vultr, with three servers rolled and three workers with one allocation each, a worker's server was
      halted four times (each halt took the server of one or two workers); every node stayed ready and no allocation
      was lost. The facts are in [platform notes §1.2](../platform-notes.md#12-features-tent-relies-on) and
      [§3.16](../platform-notes.md#316-spike-runs).
    - **Cost.** A killed client reads ready for about 20 to 40 s from the kill, in place of 10 to 30 s (derived, not
      measured), and the hash of every group moves, so the release that carries this rolls every node (decision 44,
      item 17). On a combined node the `rpc` block also sets the keepalive of the server part. An operator can override
      both through `spec.nomad.extraConfig`; the host firewall and tent's waits do not follow an override.

### Progress, log and tests

20. **The progress events and the observation log** (`Service.Log`, one debug line per observation under `-vv`) are in
    [architecture §14](../architecture.md#14-cli).
21. **The tests model what Nomad did.** The model is in [architecture §15](../architecture.md#15-testing).

### The remembered index (decision 48)

22. **tent keeps the highest index that a machine name of each server and combined group has had** in the state store,
    at `<cluster>/names/<nodegroup>` (maintainer decision 48, 2026-10-09). A roll names a new server above it (item 7).
    - **Writes, under the cluster's lock only, and only up:** before every create of a server or combined machine, in
      every apply of `update` that has changes and in every apply of `rolling-update` that has a next step. A plan
      only reads it.
    - **A cut or a failed create leaves a gap in the names,** since the index is written before the create request.
    - **A roll reads and raises only the groups it rolls.** `--nodegroups workers` writes no names object.
    - **`rolling-update --yes` therefore raises `tent-version`** when the running tent is newer, so an older tent is
      refused afterwards.
    - **`delete cluster` deletes the objects** with the other state, also those of a group that the specs no longer
      have.

    Reads, conditional puts and error texts: [architecture §10.2](../architecture.md#102-layout).

## Consequences

### Positive

- A roll of server groups, of a single server included, is finished by the next run after a cut at any write, with as
  many creates as an uninterrupted roll. The quorum holds at every call of the tests, also for a lost answer.
- A refusal that comes from a blip right after a transfer no longer ends a roll.
- A hard stop of a server, by tent or by a failure, no longer costs the allocations of the clients that talk to it.
- A roll never gives a server a name that a server of its group had, also after a cut, a shrink or a delete.

### Negative / trade-offs

- **A roll is slow.** About 4 minutes per server on Vultr, and every tent release makes the servers outdated.
- **A halted server that someone else stops seconds before tent's stop is not seen,** nor a hung agent whose machine
  stays `running`. The window compares the servers' `StableSince` with the clock of the operator's machine, so a clock
  that runs ahead shortens it by as much.
- **The first roll after the upgrade has the old behaviour.** A cluster that an earlier tent built gets
  `heartbeat_grace` and the `rpc` block only as its nodes are replaced. Servers roll before clients, so in that roll
  clients have no `rpc` block and the old leader's grace is 10 s; a client that talks to a stopped server can read down
  and lose its plain allocations. A job with `disconnect { lost_after = ... }` keeps them ([platform notes
  §1.2](../platform-notes.md#12-features-tent-relies-on)). Clusters that this tent builds are not affected.
- Other costs are named in items 6, 13 to 16, 18, 19 and 22.

### Follow-ups

- **M3.5** rolls combined groups with the drain first. The command refuses them until then (`node group nodes: tent
  cannot roll combined groups yet; select client groups with --nodegroups`, see [architecture
  §13.3](../architecture.md#133-tent-rolling-update-cluster---yes)). Line e1 and the tolerance checks apply to them
  already.
- **M3.6** moves `update`'s names into `rollout` so that its planner names a new server above the remembered index
  (item 22), and gives `update`'s delete guard a text that says to delete one of two machines of one name when both
  joined. Until then a growth of a server group through `update` after a roll can take the name of a removed server
  and fail to join (after a roll that made `-3` to `-5`, a new server takes `-0`).

## Alternatives considered

- **A grace of 75 s alone.** A dead client would read ready for 75 to 95 s.
- **Remove the Raft peer before the stop for every group size.** It reworks the roll of three servers, and a re-added
  nonvoter serves clients again.
- **A leadership transfer after each stop.** It gives every node a heartbeat timer of 300 s and hides dead clients for
  5 minutes.
- **Document the limit and change nothing.** A roll would cost allocations.
- **A wait after `TransferLeadership` only.** A cut right after the transfer gives the next run the same refusal, and
  any other short unhealthy report after a step would still end the run (a new server read unhealthy for 2 s right
  after it joined).
- **A change in `rollout` that waits in place of refusing while the servers changed recently.** `StableSince` also moves
  on a health change, so a cluster that is unhealthy at rest would wait instead of being refused, and `rollout` would
  need a notion of "recent" that a cut run cannot see.
- **Take the highest name from the lists alone.** Nothing lists the highest name once it is deleted (see Context).
- **Replace the highest-named server last, so that its successor is the one that stays.** It changes the order of
  [ADR-0035](0035-rollout-decisions.md) (items 7 and 13), and the name of a machine that someone stopped or reverted
  stays open.
- **Reuse the lowest free server name and wait before the create until the old name is safe.** Nomad's API does not
  show when that is, so the wait is a sleep of unknown length that a cut run loses.
- **Reuse the name and accept a failed join.** The run ends at the 10-minute wait for the vote, and `update` deletes the
  server that never joined.
- **Keep tent-node out of the hash of server and combined groups.** A release would roll only the clients, but the
  servers would keep the tent-node they booted with until another change rolls them, a group would run two versions of
  it, and the hash would no longer say all a node runs.
- **Stop at once when the run removed a nonvoter at most 20 s ago.** A nonvoter can be promoted after 20.2 s (item 12).
- **Ignore the first Ctrl-C between a held stop's call and the list that shows the machine stopped,** or wait for two
  re-adds a minute apart before the stop. The first changes the interrupt rule of every command; the second proves the
  timer's phase and costs a minute more per removal.
- **Reuse the event `stable` for the hold.** Its line names a time that the run does not know here.
- **A simulator with a halt that takes a tick and a driver that holds.** It copies the loop's rule into a test. The
  simulator proves the order of the decisions and the flows of `internal/app` prove the hold.
- **Add `Nodes.Start` and let the run start a stopped victim that votes.** It is a new primitive of every provider for a
  rare case; the operator starts the instance.
- **Give `internal/cloud` an error class for a call that the cloud answered with a refusal,** so that a refused halt
  ends the run at once. It changes the provider interface for a rare case.
- **Keep the refusals of two voters and of single servers.** The maintainer answered otherwise (decisions 37 and 38).
