# ADR-0040: Scale down through the rollout

- **Status:** Accepted; amended by
  [ADR-0041](0041-upgrading-a-clusters-nomad-version.md) (a plan that boots a node or moves the Nomad pin reads what a
  roll observes besides the plans of item 6, and fails when the read fails, unless no server answers and the plan
  has an infrastructure change other than a delete: the apply then reads after those changes; item 5 holds for every
  other plan)
- **Date:** 2026-10-12
- **Deciders:** ingvarch
- **Related:** amends [ADR-0005](0005-immutable-nodes-and-nomad-aware-rollouts.md),
  [ADR-0015](0015-idempotency-without-unique-names.md), [ADR-0017](0017-api-driven-server-removal.md),
  [ADR-0031](0031-bootstrap-in-update.md), [ADR-0032](0032-joined-label-scrub-and-delete-guard.md),
  [ADR-0033](0033-operator-commands.md), [ADR-0035](0035-rollout-decisions.md),
  [ADR-0037](0037-rolling-update-of-client-groups.md), [ADR-0038](0038-rolling-update-of-server-groups.md) and
  [ADR-0039](0039-rolling-update-of-combined-groups.md) (see their Status lines); builds on
  [ADR-0036](0036-nomad-calls-of-a-roll.md);
  [architecture §13.2](../architecture.md#132-tent-update-cluster---yes), [§13.4](../architecture.md#134-scaling),
  [§17](../architecture.md#17-risks), [§18](../architecture.md#18-open-questions) (decisions 34 and 48) and
  [platform notes §3.16](../platform-notes.md#316-spike-runs)

## Context

Until now `update` deleted a machine only when it had never joined Nomad, and failed the plan for a node that had
([ADR-0032](0032-joined-label-scrub-and-delete-guard.md), decision 27). Decision 34 lifts that guard where tent drains
or removes a node safely. [ADR-0035](0035-rollout-decisions.md) built the decisions of a removal in the mode `Shrink`,
and [ADR-0037](0037-rolling-update-of-client-groups.md), [ADR-0038](0038-rolling-update-of-server-groups.md) and
[ADR-0039](0039-rolling-update-of-combined-groups.md) built the loop that carries them out for `rolling-update`. The
facts that decide the rest were measured on a local Nomad 2.0.7 with ACL, mTLS, gossip encryption and strict client
introduction ([platform notes §1.2](../platform-notes.md#12-features-tent-relies-on) and
[§1.6](../platform-notes.md#16-the-agent-on-a-node)):

- **A server that tent removed can come back.** A new agent under the name and at the address of a server that tent had
  removed 22 s before was added as a nonvoter and removed by autopilot again at every reconcile of the 110 s watched.
  The group's names must only grow, also after a shrink, and nothing lists the highest name once its server is gone.
- **A node outlives its machine.** A run cut between a machine's delete and its node's purge leaves a node that reads
  `down`, while the group is at its size and no machine shows that anything is left.
- **A drain whose work has nowhere to go can last its whole deadline.** With two allocations of one group on the node
  and `migrate.max_parallel = 1`, the first got `stop` at 3.2 s and was complete at 8.4 s, and the second ran until the
  deadline of 45 s. At 46.1 s the drain read `complete` while that allocation still ran with the desired status `stop`.
- **A server group grows through even numbers of voters.** New servers read as nonvoters at the first read and voted
  after 16.8 to 21.4 s. Four of them logged `peer has a conflicting expect value`, and nothing else followed from it.

## Decision

### What `update` removes

1. **`update` removes the nodes that a smaller spec no longer has through the decisions in the mode `Shrink` and the
   loop of `rolling-update`.** A client is marked ineligible, drained within its group's `drainTimeout`, deleted, and
   its node purged once Nomad lists it down. A server leaves as [ADR-0017](0017-api-driven-server-removal.md) and
   [ADR-0038](0038-rolling-update-of-server-groups.md) order: the leadership moves away when it leads, the machine
   stops, its peer leaves the Raft configuration and its member the gossip pool, and the servers that stay are healthy
   before the next one goes. A combined node does both, with the drain first
   ([ADR-0039](0039-rolling-update-of-combined-groups.md)). A machine that never joined is deleted at once, as before,
   after the guard's question to Nomad. The loop keeps every step, wait, limit and safety rule of a roll. It creates
   nothing, labels nothing `tent/replace`, and runs inside `update`'s lock, which `update` holds through every drain.
2. **`rollout.Surplus` chooses the machines, one at a time.** Each time it takes the first of the machines that are left
   and counts the zones over those, so the zones stay even. Machines whose removal has started go first, so a cut run
   finishes what it began. `Next` takes its victims from `Surplus` itself, so the plan and the loop cannot differ
   ([architecture §13.4](../architecture.md#134-scaling) has the order). A client machine that joined and has no private
   address in the cloud's list goes before all of them, and no step is taken on it: the cloud lists a machine so for a
   moment after it deleted it, and its node cannot be told from a stranger's. It takes a place of the surplus, so no
   machine with an address goes in its place. The rule of [ADR-0035](0035-rollout-decisions.md) left such a machine out
   and still counted it in its group, so a plan made right after a cut at a delete named a worker that stays. While the
   cloud lists the machine its group stays above its size, and the refusal `cannot go on` names it.
3. **The leadership goes to a server that stays, and a shrink to one server keeps its leader.** `Surplus` takes the
   servers that do not lead before the leader, so a group at rest loses followers and moves no leadership. When the
   leader is among the victims, `successor` skips every machine that `Surplus` lists, so the leadership moves at most
   once. The other choice, to make the leader the last victim of a shrink to one server so that a transfer comes right
   before the last removal, does not hold across its own transfer: after it the rule names the other server, so a run
   cut after the transfer would move the leadership back and remove the wrong machine; the simulator's resume tests fail
   on it. A label that outlives a cut (`tent/removing=true`) would fix that at the cost of a provider call, a transfer
   and the window of 70 s in every shrink to one server. The hold of [ADR-0038](0038-rolling-update-of-server-groups.md)
   ties every stop to a reconcile that the run saw, so no stop needs a known phase of the leader's timer.
4. **The order of an apply** ([architecture §13.2](../architecture.md#132-tent-update-cluster---yes)): the creates
   and the Nomad step, then the deletes of machines that never joined, then the removals and the purges, then the
   infrastructure's deletes. So a run that grows one group and shrinks another has the new nodes before the old ones go,
   and the firewall group of the clients goes only after the last client machine.

### What the plan shows and when it reads Nomad

5. **Every plan of a cluster that can answer reads Nomad's nodes once.** A cluster can answer when the store holds the
   bootstrap mark and a server or combined machine has joined, runs and has a public address. The call is the one that
   decision 28 makes for clients that may never have registered, so a plan that needs both asks once. The node of a
   deleted machine shows nowhere else, and a cut run would leave it for ever. A read that fails is a warning when no
   client must be judged and no node that joined may go, and the plan then lists no purge. A plan with no removal and
   no purge needs Nomad for no more than before (the Nomad step, a client, the question before a delete), so an
   `update --yes` that only widens `access.api` still applies.
6. **A plan that may remove a node that joined reads what a roll observes** (`Peers`, `Health`, `Members`, `Nodes`) and
   asks the decisions about the removals, on the whole state and then about each group alone, since `Next` stops at
   the first group with a step. A refusal fails the whole plan, with and without `--yes` and with `--exit-code`, before
   any change, as the guard does: `update cannot remove prod-servers-4 and prod-servers-3: node group servers:
   autopilot reports the servers unhealthy (prod-servers-1); tent removes a server only while every server is healthy`.
   A plan that cannot read fails too. With no server to ask it goes on by the labels only when no removal is among its
   changes, and otherwise fails. Such a plan needs the release files, which give the spec hashes of the order. When
   they cannot be read, a plan that removes fails with the builder's error, and a plan that only purges lists no
   purge; the outdated warning (`tent could not tell which nodes are outdated: <error>`) gives the cause.
7. **The lines of the plan,** the count of purges, the `Removed:` line and the JSON are in
   [architecture §13.2](../architecture.md#132-tent-update-cluster---yes). A removal and a purge are changes, so
   `update --exit-code` exits with 2.

### Which nodes are purged

8. **A node is purged when Nomad lists it down and no machine has it** at its name and address. That covers the nodes of
   client and combined groups of the specs and of a client group that the specs lack, by the group in the node's name
   (`rollout.GroupOfNode`). The rule is `rollout`'s: C1 for a client group ([architecture
   §13.3](../architecture.md#133-tent-rolling-update-cluster---yes)) and `settleOrphans` for a combined group, so the
   plan lists the nodes and the loop's `Next` gives each purge. A node that is not down is never purged, and no plan
   lists it: its machine may only be missing from a list, a purged live node registers again or is cut off, and a node
   can read `ready` for up to 300 s after a leader change. A run that works on the node's group waits up to 6 minutes
   for it to read down, as a roll does (rule C8, [ADR-0035](0035-rollout-decisions.md) item 19), and purges it then. The
   first plan that sees the node down lists it. A down node under the name of a server group of the specs is left alone,
   since tent made no such node. A group that the decisions refuse (two machines of one name, a role that changed) gets
   no purge, so a leftover node never fails an `update` that has other work. A down node whose name and address a create
   of the same plan takes again is not purged.

### The plan and the apply choose the same machines

9. **The loop removes no machine that its plan does not list.** `Surplus` is the same function in both, and the apply
   plans again under the lock and applies that plan. A step for another machine ends the run before anything is sent,
   so the operator sees only what the plan showed. A purge names a node and no machine and always passes
   ([architecture §13.4](../architecture.md#134-scaling) has the text).
10. **The machines that the apply deleted just before the loop count as a write of the run.** The loop starts with them
    as deleted ones: it does not send their delete again, lists until the cloud stops listing them (up to 5 minutes,
    then `the cloud still lists node <name> (ID <id>) 5m0s after its delete`) and waits up to a minute for a refusal
    that rests on them, such as a deleted duplicate that the cloud still lists. Without that a run that has sent no
    write ends at the refusal. The loop's texts name its command, so a removal says `run tent update cluster again`. Its
    log line is `update observed`, where a roll's is `rolling update observed`.
    A run that is cut is finished by the next `update`, also after a lost answer of a write; the tests cut a scale-down
    before and after its writes ([architecture §15](../architecture.md#15-testing)).

### Groups and machines that need a rule of their own

11. **A machine that never joined keeps today's path.** It is deleted after the guard's question to Nomad, which counts
    a `down` node as not joined (decision 27). `rollout`'s own rule counts a `down` node as known, so using it would
    change decision 27 first.
12. **A client group that the specs lack is drained and removed.** The decisions see it with size 0, no limit on
    unavailable nodes and the API's default `drainTimeout`, `1h`, since the group's own setting went with its spec. All
    its nodes are marked and drained together. An operator who wants another deadline sets the group's size to 0 first
    and removes the group after. A server or combined group that the specs lack is refused (item 13), because tent
    removes no whole group of servers.

### The delete guard where it stays

13. **The plan refuses two cases, by the joined label:** a machine that joined in a server or combined group that the
    specs lack, or with no group label (tent removes no whole group of servers), and two machines of one name that both
    joined, in a group of the specs or in a client group that the specs lack (`rollout` tells a node by its name and
    address, so it could not tell the two apart). The words `update deletes only nodes that never joined` and the advice
    to run `rolling-update` to finish a roll that stopped are gone. The apply's guard still asks Nomad before each
    delete of a machine without the label. A machine that it finds joined is scrubbed and labelled, and the step fails
    with the way out: run `tent update cluster` again, which removes it as a node that joined or says why it cannot.
14. **`update` and `rolling-update` no longer point at each other.** After a roll that stopped between a create and its
    victim's delete, the group has one machine more than its size, and `update` removes one: the machine under removal
    first, else an outdated one. Decision 35 says that `update` never replaces a node. It creates none here, and the
    roll goes on with the rest.

### Server groups

15. **A server group shrinks one server at a time, 5 to 3, 5 to 1 or 3 to 1**, by the steps of
    [ADR-0038](0038-rolling-update-of-server-groups.md). From two voters to one the peer of the running victim goes
    first, the stop is held until the leader's reconcile that the run saw, and the stop follows within 10 s of the
    observation that showed the re-add (decision 37). The server that stays keeps the `bootstrap_expect` of its
    creation: the file is no part of the spec hash, so the node is not outdated, and a restart of a server with
    `bootstrap_expect = 3` and a Raft configuration of itself led 2.0 s after its start. Going to one server needs
    `--allow-single-server`, as every command that validates the specs does.
16. **A server group grows through `update`'s creates**, as before. A new server of a group that grows carries the new
    size as `bootstrap_expect`. The cluster has two voters while it grows from one server to three and four while it
    grows from three to five. The creates go back to back, so those states last as short as Nomad allows.

### Names

17. **A new server or combined machine of `update` takes the index above the remembered one**
    ([ADR-0038](0038-rolling-update-of-server-groups.md) item 22, decision 48), which the first bullet of the Context
    requires. The planner reads `names/<nodegroup>` in every plan and names the creates from `rollout.NextIndex` over
    the listed machines and the remembered index, one higher for each further create. A client group keeps the lowest
    free name. Reading the index needs no Nomad. A cluster that tent built before it kept the index (before M3.4),
    whose store holds no `names/<nodegroup>` yet and whose highest server an operator deleted by hand while its member
    is still listed, can so get that name again from `update`, where a roll would not give it.
    - The index is written before the create request. A cut or a failed create after the write leaves a gap in the
      names, and names only grow.
    - A list that misses a server that a run has just created no longer makes two machines of one name. The next run
      creates the next name, and the group has one server more than its size until `update` removes one. The guard
      labels the missed server when it finds it in the Raft configuration, and the run after removes the newest.
    - A deleted server of a built cluster is replaced under the next name, and a cluster whose server machines are all
      gone is built again under the next names: after a shrink from three to one the store keeps `2`, and the growth
      back makes `-3` and `-4`.

### `validate cluster`

18. **`validate` names as surplus the machines that `update` would remove.** It asks Nomad before it plans the machines,
    over the server machines of the cluster that run and have a public address, and plans with the same view as
    `update`. Where `update` plans by the labels (no node that joined may go, or no server that joined runs to ask and
    no removal is among the changes) so does `validate`. Where `update` fails for want of a reading (no server to ask
    with a removal, a Nomad that does not answer: `nomad-not-set-up`, `nomad-no-leader`) `validate` names the surplus by
    the labels too. A surplus machine has one row, `machine-surplus`, and a Raft peer at the address of any listed
    machine is no unknown server. A missing server is named as `update` names it. A down node that no machine has gets
    no row and no warning: it holds no work, and `update`'s plan shows it.

## Consequences

### Positive

- `update --yes` scales a cluster down to its specs, and a run that is cut at a write, or loses the answer of one, is
  finished by running the command again.
- The plan names the machines and the nodes it will remove before anything happens, and the apply removes no other.
- A smaller server group keeps quorum at every call of the simulator and in the runs on Vultr
  ([platform notes §3.16](../platform-notes.md#316-spike-runs)).

### Negative / trade-offs

- **Room for the drained work.** tent checks that the other nodes can take work, and not that they have the memory or
  meet the constraints. A drain of a node that holds more allocations of a group than `migrate.max_parallel` lasts its
  deadline when they have nowhere to go, and Nomad stops them then. A scale-down to too few nodes stops work and holds
  the lock for up to `drainTimeout`.
- **A drain can read complete early.** Nomad reported `complete` while the last allocation still ran with the desired
  status `stop`. tent deletes the machine soon after, so a task's `kill_timeout` can be cut short. This holds for a
  roll since M3.3.
- **A scale-down that is cut and taken back leaves nodes ineligible.** Nothing in tent makes a node eligible again.
  `validate cluster` reports it as `ready but not eligible`, and `nomad node eligibility -enable` brings it back.
- **The node of the machine deleted last in a cut run is purged by the first `update` that sees it down.** That may be
  the run after the next: a plan that comes before the node reads down shows no change.
- **The lock is held for every drain.** Another command waits up to `--lock-timeout`.
- **A plan with a refusal fails whole**, its creates and its infrastructure changes too. The way out is in the text, or
  to take the removal back in the specs and apply the rest first.

### Follow-ups

- **M3.9** builds the E2E scenarios, the one with a combined group among them
  ([ADR-0039](0039-rolling-update-of-combined-groups.md)).
- A step that makes a node eligible again after a scale-down that was taken back, and a check that the nodes that stay
  have room for the work of those that go, are left for later.

## Alternatives considered

- **Move `planNodes` whole into `rollout`,** with a change type of its own. Rejected: the rules that stay read
  `cloud.Instance` (operation ids, creation times, labels) and make `internal/app`'s plan type, and none of them decides
  about a node that joined.
- **Write the index when `Nodes.Create` has returned.** Rejected: a cut after the create and a list that misses the
  machine make the twin again.
- **Read Nomad only in a plan that removes.** Rejected: the node of a deleted machine shows nowhere else, so a cut run
  would leave it for ever.
- **Read Nomad's nodes in every plan that creates a server,** to give the name from the nodes too. Rejected: the
  remembered index covers the cut, and a plan that creates should not need the API.
- **Let the loop follow the order as the cluster changes,** and apply a plan that no longer matches. Rejected: the
  applied plan could list another machine than the run removed.
- **Print the plan and then the refusal, as `rolling-update` does,** or apply the creates and end the run at the
  removals. Rejected: a refusal at the plan, as the guard gives it, changes nothing before it.
- **Read a removed group's `drainTimeout` from the stored completed spec.** Rejected: a run after a cut, when the new
  completed spec is written, would fall back to the default anyway.
- **List the name object as a state write of the plan.** Rejected: the first plan of every cluster built before would
  have a change, and `update --exit-code` would exit with 2 once.
