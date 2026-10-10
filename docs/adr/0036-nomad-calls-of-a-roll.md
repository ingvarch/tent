# ADR-0036: The Nomad calls of a roll

- **Status:** Accepted; amended by [ADR-0038](0038-rolling-update-of-server-groups.md) (M3.4 uses the transfer, the
  peer removal, the members and the force-leave: a call to a server that does not answer waits the call timeout of 30 s
  before `Servers` moves on, so the loop leaves a stopped server, and a server whose peer it removes while the server
  runs, out of its API)
- **Date:** 2026-10-08
- **Deciders:** ingvarch
- **Related:** amends [ADR-0017](0017-api-driven-server-removal.md), [ADR-0031](0031-bootstrap-in-update.md) and
  [ADR-0035](0035-rollout-decisions.md) (see their Status lines); builds on [ADR-0021](0021-import-rules.md);
  [architecture §13.2](../architecture.md#132-tent-update-cluster---yes),
  [§13.3](../architecture.md#133-tent-rolling-update-cluster---yes), [§13.8](../architecture.md#138-backups),
  [§15](../architecture.md#15-testing),
  [platform notes §1.2](../platform-notes.md#12-features-tent-relies-on)

## Context

The steps of [ADR-0035](0035-rollout-decisions.md) need Nomad calls that `internal/nomadops` did not have: node
eligibility, drains, purges, leadership transfers, Raft peer removals, the gossip members, force-leave and snapshots.
The reads also lacked the Raft IDs, the failure tolerance, `StableSince` and the drain state that `rollout.State`
holds. M3.2 adds them. Nothing calls them yet: M3.3 and M3.4 wire the node, Raft and gossip calls into the loop of
`tent rolling-update cluster`, and M3.8 the snapshots into `tent backup`.

- **The loop never takes a write's answer for the cluster's state.** It observes again after every step and asks
  `rollout.Next` for the next one. A wrong answer of a write therefore costs a repeated step, never a skipped one.
- **A write can be repeated after an answer is lost.** `nomadops.Servers` sends the same call to the next server after
  `ErrNotReady`, and the outcome of the first try is then unknown.
- **The facts** were measured on 2026-10-08 on a local Nomad 2.0.7 with ACL, mTLS, gossip encryption and strict client
  introduction. They are in [platform notes §1.2](../platform-notes.md#12-features-tent-relies-on).

## Decision

### The calls

1. **`nomadops.API` gains nine methods.** Each is one Nomad call, made by the Nomad API module over the client's mTLS,
   with `region=<region>` in the query.

   | Method | Request |
   |---|---|
   | `MarkIneligible(ctx, nodeID)` | `PUT /v1/node/<id>/eligibility`, `Eligibility` `ineligible` |
   | `Drain(ctx, nodeID, DrainRequest)` | `PUT /v1/node/<id>/drain`, `Deadline`, `IgnoreSystemJobs` false, `Meta` |
   | `Purge(ctx, nodeID)` | `PUT /v1/node/<id>/purge` |
   | `TransferLeadership(ctx, raftID)` | `PUT /v1/operator/raft/transfer-leadership?id=<raft id>` |
   | `RemovePeer(ctx, raftID)` | `DELETE /v1/operator/raft/peer?id=<raft id>` |
   | `Members(ctx)` | `GET /v1/agent/members` |
   | `ForceLeave(ctx, name)` | `PUT /v1/agent/force-leave?node=<name>&prune=1` |
   | `SaveSnapshot(ctx)` | `GET /v1/operator/snapshot` |
   | `RestoreSnapshot(ctx, snap)` | `PUT /v1/operator/snapshot`, the snapshot as the body |

2. **The reads gain the values that `rollout.State` holds:** the Raft IDs, the failure tolerance, `StableSince` and the
   drain state with its meta. The fields are in [architecture §13.2](../architecture.md#132-tent-update-cluster---yes).
   How `internal/app` maps them into `rollout.State` is built in M3.3.
3. **A transfer's 200 proves nothing.** Nomad answers 200 to a transfer to a nonvoter and another server takes the
   leadership. `TransferLeadership` returns nothing of Nomad's answer, and the loop reads the Raft configuration again.

### Error classes

4. **`ErrGone` is a third class,** beside `ErrNotReady` and the permanent one. It means that the node or the Raft peer
   that the call names is not in the cluster. It is permanent: the same call fails the same way, so `Servers` returns
   it at once. The error keeps the call's text, such as `nomad: PUT /v1/node/<id>/drain: 500: rpc error: node not
   found`; only its class changes.
5. **The match needs the status and the whole message,** after any number of leading `rpc error: ` (a request that is
   forwarded twice carries two). For the Raft calls the message must name the ID that was sent. Every other 5xx and
   429 stays `ErrNotReady`, and every other 4xx stays permanent, a 404 `node not found` included.

   | Call | Status and message that mean gone | Result |
   |---|---|---|
   | `MarkIneligible`, `Drain` | 500 `node not found` | an error that matches `ErrGone` |
   | `Purge` | 500 `node not found` | `nil` |
   | `TransferLeadership` | 400 `id "<id>" was not found in the Raft configuration` | an error that matches `ErrGone` |
   | `RemovePeer` | 500 `id "<id>" was not found in the Raft configuration` | `nil` |

   `Purge` and `RemovePeer` reach their goal when the target is gone, so they return `nil`; the other three would look
   done while doing nothing, so they return `ErrGone`. `ForceLeave` has no gone answer: Nomad answers 200 to any name.
6. **Arguments are checked before any request,** and a bad one is a permanent error with no request sent:
   - a node ID or a Raft ID that is empty or has a character other than an ASCII letter, a digit or `-`, since the
     module puts a node ID into the path as it is and Nomad's IDs are UUIDs;
   - a drain whose deadline is not above zero, since 0 is a drain without a deadline and a negative one stops every
     allocation at once;
   - a force-leave name without its region (`<node name>.<region>`), since Nomad answers 200 to it and changes nothing;
   - an empty snapshot, which would look restorable.

### Servers

7. **Every new method goes through `try`.** Nomad forwards each write but a force-leave to the leader, so no call
   needs the leader's address. The other repeats change nothing, with two exceptions:
   - `Drain`: the repeat moves the force deadline of the drain that runs to the time of the repeat plus the deadline.
     The decisions never drain a draining node (rule C6 waits instead), so only a retry inside one step repeats it.
   - `RestoreSnapshot`: the same state is restored again, and what was written between the two restores is lost
     twice. A snapshot that Nomad refuses is a 500 on every server, so it goes to each server once and the error names
     each cause.

### Snapshots

8. **A snapshot is a secret in memory.** It holds the keyring and the ACL tokens. `SaveSnapshot` returns
   `secret.Secret` and `RestoreSnapshot` takes one, so no error, log line or `Call` shows its bytes.
   - The bytes stay as Nomad sends them, and M3.8 stores them as they are. A cut-off body is `ErrNotReady`, never a
     shorter snapshot.
   - The save fails for good when the `Digest` header is missing, is of a kind that the client does not know, or does
     not match the bytes, and when the answer holds no bytes.
   - **Both snapshot calls get 5 minutes,** the other calls keep 30 seconds: a snapshot moves the whole state of the
     cluster, which grows with its jobs. An error names the bound that applied.
   - Nomad's 500s of a save or a restore are `ErrNotReady`. M3.8 decides what a backup does with the 500 that follows a
     change of the servers.

### Purges

9. **`Purge` takes any node, and `rollout` purges only a node that Nomad lists down** (ADR-0035, item 19). Under strict
   client introduction a purged live client whose introduction token had expired was refused at every try to register
   again in the 2 minutes watched ([platform notes §1.2](../platform-notes.md#12-features-tent-relies-on)). `nomadops`
   does not check the node's status: that rule belongs to the decisions, where the tests prove it.

### The fake

10. **`nomadfake` carries out each write on its own state.** The reactions of a cluster that take time are the test's to
    set, as before. The rules, and what the fake leaves out, are in [architecture §15](../architecture.md#15-testing)
    and in the doc of the package.

## Consequences

### Positive

- Every Nomad call of ADR-0035's steps exists, with the requests, the error classes and the repeat safety under test
  over real mTLS and on the fake, and no snapshot reaches an error, a log line or a `Call` as bytes.

### Negative / trade-offs

- **`ErrGone` rests on exact texts** of Nomad 2.0.7. A release that words them differently reads as `ErrNotReady`
  (for a 500) or as a permanent error (for a 400). Both fail closed: the step repeats or the run stops. The E2E suite
  runs real Nomad.
- **A snapshot holds the whole state in memory.** A cluster with many jobs needs the memory, and 5 minutes may be too
  short for it. The size of a snapshot of a large cluster was not measured (about 11 KB for a small one).
- **A drain that is repeated moves its deadline,** so a retry that comes late gives the node more time than asked for.

### Follow-ups

- **M3.3** maps the reads into `rollout.State`, keeps the key `tent_machine` of the drain meta, carries out the steps
  with the node calls, and observes again after `ErrGone`.
- **M3.4** uses the transfer, the peer removal, the members and force-leave for server removals.
- **M3.8** stores the snapshots.
- **Two voters to one** (ADR-0035, item 15) was answered on 2026-10-08 (decisions 37 and 38), and M3.4 built it
  ([ADR-0038](0038-rolling-update-of-server-groups.md), items 12 and 13); the facts are in platform notes §1.2.
- **Where a restore may go** stays open for the maintainer; the restore facts are in platform notes §1.2.

## Alternatives considered

- **`ErrGone` for every call, or success for every call.** A purge and a peer removal reach their goal when the target
  is gone. A drain, a mark and a transfer do not, and a success there would let a step look done that did nothing.
- **Match "gone" by a part of the message,** or by the status alone. Another 500 (`No cluster leader`, `node not found
  yet`) would then read as done. The whole message and the status leave every other answer as it was.
- **Stream a snapshot to a writer.** A half-written writer stops `Servers` from retrying a save on the next server.
- **A snapshot as `[]byte`.** It holds the keyring and the ACL tokens, so it gets the type that never prints.
- **One bound of 30 seconds for the snapshots.** The state of a cluster grows with its jobs, and a timed-out save looks
  like an unready server.
- **`SetEligibility(id, bool)`.** tent never marks a node eligible, so `MarkIneligible` cannot do the wrong thing.
- **`ForceLeaveWithOptions` of the module.** It takes no `WriteOptions`, so a call could not end with its context;
  `ForceLeave` goes through `Raw().Write`.
