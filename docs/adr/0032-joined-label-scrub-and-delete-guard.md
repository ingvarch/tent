# ADR-0032: The joined label, the scrub and the delete guard

- **Status:** Accepted
- **Date:** 2026-10-06
- **Deciders:** ingvarch
- **Related:** amends [ADR-0005](0005-immutable-nodes-and-nomad-aware-rollouts.md),
  [ADR-0008](0008-node-credential-delivery.md), [ADR-0015](0015-idempotency-without-unique-names.md),
  [ADR-0018](0018-vultr-provider-design.md), [ADR-0019](0019-combined-server-client-role.md),
  [ADR-0027](0027-nodeconfig-contract-rendering-and-spec-hash.md),
  [ADR-0029](0029-host-firewall-runtime-and-cni-on-nodes.md), [ADR-0030](0030-nomad-on-nodes.md) and
  [ADR-0031](0031-bootstrap-in-update.md) (see their Status lines); builds on
  [ADR-0017](0017-api-driven-server-removal.md) and [ADR-0023](0023-vultr-inventory-dedupe-and-images.md);
  [architecture §3.4](../architecture.md#34-naming-and-ownership-markers),
  [§7.1](../architecture.md#71-interfaces), [§9.4](../architecture.md#94-secrets-on-nodes-threat-model),
  [§11.6](../architecture.md#116-user_data), [§13.2](../architecture.md#132-tent-update-cluster---yes),
  [§13.4](../architecture.md#134-scaling), [§18](../architecture.md#18-open-questions),
  [platform notes §1.2](../platform-notes.md#12-features-tent-relies-on),
  [§1.6](../platform-notes.md#16-the-agent-on-a-node), [§3.3](../platform-notes.md#33-instances)

## Context

M2.7a builds a running cluster, and leaves three things to M2.7b ([ADR-0031](0031-bootstrap-in-update.md)): the node
keys, the gossip key and the intro tokens stay in the machines' user data; a ready machine that never registered is not
seen, so a cut run or a client whose token expired leaves a node that never joins; and `update` deletes nodes that
joined Nomad, servers included, without a check. M2.7b closes them. It closes issue #93.

The facts that shaped it. The ones about Nomad were checked on 2026-10-05 and 2026-10-06 on a local Nomad 2.0.7 and in
its source ([platform notes §1.2](../platform-notes.md#12-features-tent-relies-on),
[§1.6](../platform-notes.md#16-the-agent-on-a-node)):

- **A node's address** in `GET /v1/nodes` is the host of the client's `advertise.http`. With tent's configuration that
  is the node's private address.
- **The Raft configuration is the exact list of servers.** The autopilot report is a snapshot rebuilt on a timer. It
  showed a killed server as alive for 41 s and kept a removed one for about 2 s. `GET /v1/operator/raft/configuration`
  has no lag.
- **Nodes of one name pile up.** A killed client reads `down` after 14 to 16 s and stays listed (garbage collection
  waits 24 hours). A new client of the same name registers at once as a second node. A node of a dead machine reads
  `ready` for up to 30 s, and for up to 300 s after a leader change.
- **An intro token works for 60 s after its expiry**: clients started 30 to 58 s after `exp` registered, and at 62 s
  they were refused. An expired token is refused under `warn` too.
- **Vultr** replaces the whole tag set when a PATCH sets `tags` ([platform notes
  §3.3](../platform-notes.md#33-instances)).

## Decision

### The label and the scrub

1. **The label.** A machine whose node has joined its cluster carries `tent/joined=true`
   (`cloud.Instance.Joined`). The name stays as long as the `tent/` prefix does (decision 1 of
   [architecture §18](../architecture.md#18-open-questions)), since labels are how tent finds what it owns.
2. **What joined means** is the Nomad-side proof of the role: a voter at the server's private address, a ready and
   eligible node of the client's name and private address, both for a combined node. A `down` node does not count
   (confirmed on 2026-10-06). The detail is in
   [architecture §9.4](../architecture.md#94-secrets-on-nodes-threat-model).
3. **One provider call.** `Nodes.MarkJoined` replaces `Nodes.ScrubUserData`: it sets the label and replaces the user
   data with a stub where the cloud lets user data change, so no state lies between the scrub and the label. This
   amends [ADR-0018](0018-vultr-provider-design.md) item 5 and [ADR-0008](0008-node-credential-delivery.md). The core
   calls it on every cloud, and a cloud with immutable user data sets the label alone. On Vultr it is one PATCH
   ([architecture §11.6](../architecture.md#116-user_data)), and both fields work together
   ([platform notes §3.3](../platform-notes.md#33-instances)). The stub ends with an empty mapping, since cloud-init
   refuses a cloud-config that holds only comments and then reports a degraded status, with `errors: []` and one
   recoverable error ([architecture §11.6](../architecture.md#116-user_data),
   [platform notes §6.5](../platform-notes.md#65-cloud-init-and-the-stub)). The first real-cloud run found this.
4. **The servers** are scrubbed in the Nomad step, after the health wait, one by one. `update` reads the Raft
   configuration once and checks that a voter sits at the machine's private address before it scrubs the machine, so a
   machine whose server holds no vote at its address, as when a twin holds it, stops the run. A combined node must have
   registered first. The bootstrap mark (the object `nomad/bootstrapped` of the state store) is written after the
   last scrub.
5. **The clients** are scrubbed one by one, right after the wait until the node has registered.
6. **A node is told by its name and its private address together.** `nomadops.WaitNode` and the checks take both. A
   node of the same name at another address, such as a twin or the node of an earlier machine, never counts. With the
   name alone, a stale node would end the wait and the scrub would reach a machine that has not read its user data.

### The wait in the plan

7. **Every machine that stays and has no joined label is waited for**, whatever its role. The plan sees this from the
   labels, without a call to Nomad. The wait repeats the create, with the operation id, only for a machine that the
   cloud reports not ready. So a run cut between a node's create and its join is finished by the next run, and so is a
   cluster built by M2.7a's tent, whose machines have no labels yet.
### A client that never registered (decision 28)

8. **When.** A client without the label that is older than 31 minutes is judged: the token's 30 minutes
   (`nomadops.MaxIntroTTL`) and the minute of leeway (`nomadops.IntroLeeway`). The plan judges every such machine of
   the cluster, whatever it plans for it. It asks Nomad once, through the servers that stay and carry the label, and
   only when one exists. Without one, the cluster is still bootstrapping or has no labels, and the client is waited
   for. A client that Nomad lists at its name and private address and that is not `down` has registered.
9. **What.** The plan deletes a machine that never registered as `not registered` and, unless its group is then
   full, creates its name again, with a new token. It never ran a workload, so nothing is drained. A client that the
   cloud still reports not ready after 31 minutes, or that has no private address, counts as not registered.
10. **Delete before create.** This departs from the rule that a replacement is created before the old node goes
    ([ADR-0005](0005-immutable-nodes-and-nomad-aware-rollouts.md)). The old machine never served, and a second
    machine of its name would be a duplicate (run `sv3vwb` showed the delete and the create work,
    [platform notes §3.16](../platform-notes.md#316-spike-runs)).
    - Vultr counted the deleted machine against the account's instance limit in run `rugw2m`, which refused the
      create ([platform notes §3.14](../platform-notes.md#314-account-limits-terms-and-operations-)). So the Vultr
      provider sends a node create again, every poll interval, while the limit refuses it and less than 2 minutes
      have passed since a delete that Vultr accepted on this provider value
      ([architecture §11.3](../architecture.md#113-creating-a-node)). A create that the limit refuses made no
      instance, so sending it again cannot make a second one, which keeps
      [ADR-0015](0015-idempotency-without-unique-names.md). Nothing else is retried: only the node create, only the
      limit error.
11. **A plan that cannot ask Nomad fails**, with and without `--yes` and with `--exit-code`, before it changes
    anything.

### The delete guard (decision 27)

12. **The plan refuses** to delete a machine that carries the joined label. One error names every such machine, with
    its ID and the reason ([architecture §13.4](../architecture.md#134-scaling)). `delete cluster` works as before.
13. **The apply asks Nomad before each delete**, since a machine may have joined without a label: a client at its
    name and private address that is not `down`, or a server of the Raft configuration at its address, voter or not.
    A machine that joined is scrubbed and labelled, and the step fails, so the next plan refuses the delete at once.
    When Nomad cannot be asked, nothing is deleted.
14. **Duplicates** ([ADR-0015](0015-idempotency-without-unique-names.md)). Of machines with one name, the one that
    stays is chosen in three steps: a joined one; else one that is not among the clients that never registered; else
    the oldest. A twin that registered gets its label from the guard and stays at the next plan. The refusal of a
    duplicate names the machine that stays (`duplicate of ID ...`) and tells the operator to remove one of the two
    machines of that name from Nomad and delete it in the cloud. Surplus machines are chosen by age as before.
15. **Amendments.**
    - [ADR-0019](0019-combined-server-client-role.md): a combined node joins as a server and as a client.
    - [ADR-0031](0031-bootstrap-in-update.md): its three limits are closed.
    - [ADR-0027](0027-nodeconfig-contract-rendering-and-spec-hash.md),
      [ADR-0029](0029-host-firewall-runtime-and-cni-on-nodes.md) and [ADR-0030](0030-nomad-on-nodes.md): "until the
      scrub of M2.7b" now means "until the node has joined".

## Consequences

### Positive

- On Vultr, user data holds no secrets once a node has joined. A run cut anywhere is finished by the next one, with
  one instance per node, each labelled and scrubbed.
- A client whose token expired no longer stays a node that never joins.
- `update` cannot delete a node that joined, a server included, before M3 can drain it.

### Negative / trade-offs

- **The plan needs the Nomad API in one case:** a client without the label that is older than 31 minutes, while a
  server carries the label. Every plan then needs a reachable API, the plan that would repair `access.api` too. The
  way out is to delete that instance in the cloud.
- **A node of an earlier machine at the same name and address** (Context) could let a machine be scrubbed before it
  read its user data. A create takes longer than 30 s on Vultr. The node meta `tent_instance_id` would be exact.
- **The 2 minutes of the retry are a bound, not a measure:** neither run that reached the replacement of a client
  (`rugw2m`, `sv3vwb`) shows how long Vultr counts a deleted machine
  ([platform notes §3.14](../platform-notes.md#314-account-limits-terms-and-operations-)). A delete
  that Vultr settles more slowly gets the limit error with its hint. The window lives in the provider value and does
  not outlive the process, so a rerun of tent right after a run that deleted gets the limit error at once.
- **A tag change** that an operator makes between the read and the PATCH of `MarkJoined`, about a second, is lost.
  Vultr has no call that adds one tag.
- **Smaller limits.** tent trusts the label and never reads user data back. A machine that the cloud lists without a
  private address counts as not joined, and its delete asks Nomad nothing. Surplus goes by age alone, so a joined
  newest machine is refused while an older one of its group has not joined. The scrub lines show on every cloud,
  also where user data cannot change, and Hetzner will need them to depend on `MutableUserData`. The scrub's progress
  line names the node, and the JSON event has the machine's ID.
- **A node that joined and later died** keeps its label, and `update` plans nothing for it. `validate` (M2.8) and M3
  deal with it.
- **A client without the label that Nomad lists as `down`** when the plan asks is replaced without a drain. This
  includes a client of a cluster that M2.7a's tent built, and one cut between its registration and its scrub.
- **A client without the label that is registered and not eligible** fails every run at its wait and blocks the
  changes after it. The guard labels such a node, and the wait does not.
- **An operator's clock that runs ahead** replaces a slow client early; the guard asks Nomad again before the delete. A
  clock that runs behind replaces it late.
- **A server or combined node that never joins** is not replaced. Its secrets stay in its user data, the health wait
  fails on every run, and every client change is blocked.
- **Two machines of one name that both registered**: tent refuses to delete either. The operator removes one from
  Nomad and deletes it in the cloud.
- **Clients that carry the label stay labelled when every server is rebuilt**, and `update` does not notice that
  their nodes are gone.
- **A server group renamed in the specs** is refused once its machines carry the label. A group whose machines lack it
  (built by M2.7a's tent) is built as a new Nomad: run `update` once before the rename.

### Follow-ups

- **M3 (#103):** scale down with drain and server removal lifts the guard.
- **M2.8:** `validate`, which finds a labelled node that died.
- **Later:** the node meta `tent_instance_id` as the exact way to tell a machine's node.

## Alternatives considered

- **`ScrubUserData` and a second call for the label.** Two PATCHes per node and one more point where a run can stop.
- **No label: ask Nomad in every plan.** Every plan would need the API, also while a cluster bootstraps, and a plan
  could fail in a state that only the apply repairs.
- **A node told by its name alone.** See item 6.
- **The node meta `tent_instance_id`** through `/v1/nodes?filter=`: one call per machine, and servers have no such
  meta.
- **The autopilot report for a server's membership.** It lags the Raft configuration (Context).
- **Counting a `down` node as joined.** A stale node of an earlier machine would block the replacement for 24 hours.
- **Waiting for a client for ever, or showing a wait when the plan cannot ask Nomad.** The operator would have to
  delete the instance by hand, and the plan would hide the delete.
- **Labelling the servers together after the health wait, without the vote check.** A voter that tent does not count
  would go unseen.
