# ADR-0037: Rolling update of client groups

- **Status:** Accepted
- **Date:** 2026-10-08
- **Deciders:** ingvarch
- **Related:** amends [ADR-0005](0005-immutable-nodes-and-nomad-aware-rollouts.md),
  [ADR-0032](0032-joined-label-scrub-and-delete-guard.md) and [ADR-0035](0035-rollout-decisions.md) (see their Status
  lines); builds on [ADR-0015](0015-idempotency-without-unique-names.md),
  [ADR-0031](0031-bootstrap-in-update.md) and [ADR-0036](0036-nomad-calls-of-a-roll.md);
  [architecture §13.2](../architecture.md#132-tent-update-cluster---yes),
  [§13.3](../architecture.md#133-tent-rolling-update-cluster---yes), [§13.4](../architecture.md#134-scaling),
  [§13.6](../architecture.md#136-tent-validate-cluster---wait-duration), [§14](../architecture.md#14-cli),
  [§15](../architecture.md#15-testing), [§18](../architecture.md#18-open-questions) (decisions 39 to 42),
  [platform notes §1.2](../platform-notes.md#12-features-tent-relies-on),
  [§1.6](../platform-notes.md#16-the-agent-on-a-node) and [§3.3](../platform-notes.md#33-instances)

## Context

[ADR-0035](0035-rollout-decisions.md) decides the steps of a roll and [ADR-0036](0036-nomad-calls-of-a-roll.md) holds
the Nomad calls. M3.3 carries the steps out for client groups: `tent rolling-update cluster`. Server groups wait for
M3.4 and combined groups for M3.5. A run can be cut anywhere, so every step must be safe to repeat and every decision
must come from what the cloud and Nomad report now.

- **Vultr lists a new instance by tag up to about 1 s after the answer of its create**
  ([platform notes §3.3](../platform-notes.md#33-instances)). A decision made on such a list sees the slot as empty.
- **A killed client reads `down` after 14 to 19 s, and up to 300 s after a leader change**
  (`failover_heartbeat_ttl`; read in Nomad's code, not run;
  [platform notes §1.2](../platform-notes.md#12-features-tent-relies-on) and
  [§1.6](../platform-notes.md#16-the-agent-on-a-node)). Nomad also listed two nodes of one name: a new client with an
  empty data directory registered beside the down one.
- **A deleted machine's address comes back.** `vultrfake` gives the next machine the lowest free address, so the one
  just deleted. Vultr gave new machines the addresses of machines deleted about 5 minutes earlier, and a fresh one to
  a machine created a second after a delete (2026-10-08, [platform notes §3.16](../platform-notes.md#316-spike-runs)).
- **Every tent release changes every node's spec hash,** since tent-node's version and sha256 are in it
  ([ADR-0027](0027-nodeconfig-contract-rendering-and-spec-hash.md)).
- **A real-cloud check on Vultr on 2026-10-08 passed** ([platform notes §3.16](../platform-notes.md#316-spike-runs)).

## Decision

This ADR records maintainer decisions 39 to 42 as built in M3.3. Decisions 37 and 38 are in
[ADR-0035](0035-rollout-decisions.md) (item 15), and 43 and 44 are named in items 12 and 14; M3.4 builds all four.

### The loop

1. **`internal/app` observes, maps, decides and carries out.** It reads the cloud and Nomad, maps what they report into
   `rollout.State`, asks `rollout.Next` for one step, carries the step out and observes again. `internal/rollout`
   stays pure, and `nomadops` and `nomadfake` do not change for it; `cloud.Nodes` gains only `MarkReplace` (item 9).
   Nomad is read in a fixed set at every observation, so M3.4 adds no read.
2. **`Next` is asked at every poll of a wait,** never once per wait, since what a wait watches can change while it
   waits ([ADR-0035](0035-rollout-decisions.md), Follow-ups).
3. **A drain carries the meta `tent_machine=<machine ID>`,** so a node counts as drained only for its own machine.
4. **Each wait has a limit, and a wait without one is an error of the code,** so M3.4 cannot forget one. `WaitNodeDown`
   has 6 minutes because a dead client can read `ready` for 300 s after a leader change; the 2 minutes of the overview
   would end a normal roll. `WaitDrained` has the group's `drainTimeout` plus 5 minutes, so Nomad can stop what remains
   and complete the drain. The limit counts from the first time this run met the wait, or from its new start when it
   comes again after it ended, since a new run cannot know when the last one started. The table of limits is in
   [architecture §13.3](../architecture.md#133-tent-rolling-update-cluster---yes).

### The safe repeats

5. **The loop makes the operation id of each create,** and the machine that the create returns is kept as pending
   until a list shows its ID. Without it, a list that misses the new machine would show the slot as empty and the next
   decision would create a second machine. A run cut during or after a create finds the machine by its `tent/op`
   label.
6. **The repeat guard.** A step that the decisions give again right after it was carried out waits one poll and counts
   a try. The third try in a row ends the run with `<step> had no effect after 3 tries`, or with the write's last
   error. The guard stops a write whose effect a read does not show yet from turning into a loop. A write that fails
   with `ErrGone`, or with `ErrNotReady` after every server, and a create whose intro token request fails with
   `ErrNotReady` before it sent anything (tried with a new operation id), count as tries too; any other failure ends
   the run. The loop adds no retry beyond the guard: `Servers` and the provider already repeat what is safe.
7. **A delete that the cloud took is never sent again.** The loop lists at every poll until the cloud stops listing
   the machine.

### The forced set (decision 40)

8. **`--force` labels each machine of the selected groups `tent/replace=true` before the first step,** under the lock,
   one write per machine, after the plan is accepted. Every run replaces the labelled machines of its selected groups,
   with or without `--force`. The run reads the set once, from its first list, so the machines it creates carry no
   label and the run ends. A cut forced run is finished by a plain `rolling-update`, which replaces every labelled
   machine; a run cut while it writes the labels needs `--force` again for the rest.
9. **The label is a new primitive, `cloud.Nodes.MarkReplace`,** described in
   [architecture §7.1](../architecture.md#71-interfaces) and [§11.6](../architecture.md#116-user_data). `update`'s
   outdated line and `validate`'s warning name a labelled machine as outdated; in `update`'s JSON
   `outdated` its reason is `forced` when its hash is current.

### The preconditions (decision 42)

10. **`rolling-update` refuses until `update` has applied the current specs,** also after an upgrade of tent, whose
    defaults change the completed spec. `update` writes the completed spec, which pins the Nomad version, and applies
    the infrastructure that new nodes join (SSH keys, firewall rules); a roll before it would give new nodes a Nomad
    version and an infrastructure that the specs no longer give. A refusal takes no lock and changes nothing, with or
    without `--yes`: the plan finds what the first poll would find, because both call `joinCheck`. The order of the
    checks is in [architecture §13.3](../architecture.md#133-tent-rolling-update-cluster---yes).

### Server and combined groups

11. **A step of a server or combined group ends the run until M3.4 (servers) and M3.5 (combined).** An up-to-date
    server group has no step, so one rule covers the default selection and a named one, and M3.4 deletes one check.

### Names (decision 41)

12. **A new node never takes a name that a listed machine has or that Nomad lists a node of, in any status.** A purge
    frees the name. Names can pass the group's range (`prod-workers-4` for a group of 3 with `maxSurge` 1) and keep
    them. Without the rule the new machine matched the old node, which Nomad still listed `ready` and ineligible, so
    the group read one node short and a `maxUnavailable` of 0 refused the next step. `update`'s planner names from
    machines only until M3.6, and the names of server and combined groups (decision 43) are M3.4's.

### The shared create path

13. **`update` and `rolling-update` create client nodes through one path** (intro token, user data with the seed,
    `Nodes.Create` with an operation id, the scrub), so a fix to a create reaches both.

### The outdated report and the exit codes (decision 39)

14. **`update` reports outdated nodes and never replaces one.** They are no change, since `update --exit-code` would
    otherwise exit 2 after every release. `validate cluster` warns about them, and `rolling-update --exit-code` exits
    2 while a next step is due, without `--yes` only. When the release files cannot be read, `validate` and an
    `update` that creates no node warn and go on, since the report changes nothing that `update` does; an `update`
    that creates a node fails, as before. After every tent release the servers
    are outdated (decision 44), so the default selection is refused until M3.4, and `--nodegroups` rolls the clients.

### The delete guard

15. **The text of `update`'s delete guard names `rolling-update`,** since tent can drain a node now. A roll that
    stopped between a create and the delete of its victim leaves one machine above the group's size, and `update`
    would delete the newest, joined machine. M3.6 lifts the guard where tent drains or removes a node safely
    (decision 34). The exact text is in [architecture §13.4](../architecture.md#the-delete-guard).

### The tests

16. **The cut tests cut a roll at its writes and at the first read of each observation.** A cut at a later read leaves
    what the first one leaves. Cutting at every call made the package take 285 s with `-race` on the maintainer's
    machine, which at the ratio of CI's Windows runner on main (361 s there) would be about 680 s, over the 10 minutes
    that `go test` gives a package. A test fails when a write of a roll is missing
    from the list of writes, so M3.4's Raft calls cannot go uncut. The roll's tests run in parallel, and
    `TestRollTestsRunInParallel` fails for a top-level test that does not call `t.Parallel()`.

## Consequences

### Positive

- A roll of client groups is finished by the next run after a cut at any write or observation, with as many creates
  as an uninterrupted roll (except the twin below), and after a lost answer of any write.

### Negative / trade-offs

- **A cut a second after a create can leave a twin.** If a run is cut less than a second after a create's answer and
  the next run's list misses the machine, the cloud holds two machines of one name. `rollout` refuses them. `update`
  deletes the twin that has not joined; when both joined, the delete guard refuses and the operator removes one from
  Nomad and deletes it in the cloud.
- **`update` and `validate` read the release files** (`update` in every plan, `validate` when a machine stays and the
  store holds the secrets): per CPU architecture, Nomad's SHA256SUMS and its signature, and for a release build
  tent's checksums.txt, plus a call to the provider's `Arch` per machine type; and a warning when they fail.
- **A forced run writes one label per machine** before its first step, a cloud write that can fail.
- **The cut tests do not cut at the state store's writes and lock calls.** The roll writes nothing to the store but
  its lock, and a run cut at the lock is the next run's first observation.

### Follow-ups

- **M3.4** rolls server groups with the steps of [ADR-0035](0035-rollout-decisions.md), makes the Nomad API follow the
  servers as they change, shares the server create path with `applyServer`, and builds decisions 37, 38, 43 and 44.
- **M3.5** rolls combined groups. **M3.6** moves `update`'s names into `rollout` and lifts the guard where tent
  drains or removes a node safely (decision 34).

## Alternatives considered

- **Keep the forced set in the run's memory.** A forced run cut after it marked a node, and started again without
  `--force`, found that machine up to date by its hash and left its node ineligible and empty. With `maxUnavailable`
  0 the group could not roll until `--force` ran again. A drain may last an hour, so a cut is likely.
- **Keep the forced set in an object of the state store.** `delete cluster` would have to know it, and the cloud would
  not hold what the next step depends on.
- **A limit of 2 minutes for `WaitNodeDown`.** It ends a normal roll after a leader change (item 4).
- **A retry inside the loop beyond the guard.** `Servers` and the provider already repeat what is safe, and a second
  retry would hide a step whose effect never shows.
- **Names within `size + maxSurge`:** a new node waits until the old node of the name is purged. Every batch then
  waits for a node to go down, 14 to 19 s and up to 300 s after a leader change.
- **Roll from specs that `update` has not applied, with a warning,** or refuse only when the Nomad version or the
  infrastructure differs. Neither gives the operator one order of commands.
- **Count outdated nodes as changes of `update`.** `update --exit-code` would exit 2 after every tent release.
- **Fail `validate`, and an `update` that creates no node, when the release files cannot be read.** The report
  changes nothing that `update` does.
