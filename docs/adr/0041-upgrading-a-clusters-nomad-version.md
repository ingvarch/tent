# ADR-0041: Upgrading a cluster's Nomad version

- **Status:** Accepted
- **Date:** 2026-10-13
- **Deciders:** ingvarch
- **Related:** amends [ADR-0005](0005-immutable-nodes-and-nomad-aware-rollouts.md),
  [ADR-0026](0026-channels-and-release-assets.md), [ADR-0031](0031-bootstrap-in-update.md),
  [ADR-0033](0033-operator-commands.md), [ADR-0035](0035-rollout-decisions.md),
  [ADR-0037](0037-rolling-update-of-client-groups.md) and [ADR-0040](0040-scale-down-through-the-rollout.md) (see
  their Status lines); builds on [ADR-0036](0036-nomad-calls-of-a-roll.md);
  [architecture §3.3](../architecture.md#33-api-rules), [§8.5](../architecture.md#85-artifacts-and-verification),
  [§13.5](../architecture.md#135-tent-upgrade-cluster---yes), [§18](../architecture.md#18-open-questions) (decisions
  45 and 50) and [platform notes §1.3](../platform-notes.md#13-upgrades-and-version-skew)

## Context

[ADR-0026](0026-channels-and-release-assets.md) lets a cluster run any Nomad release from the channel's minimum up to
the next major version, pins the version in the completed spec at the first `update`, and leaves the move to another
version to M3: `upgrade cluster` moves a pin that the channel no longer allows, and tent refuses downgrades. Until now
`update` pinned a version without reading its release files and refused no downgrade, and only `rolling-update`
refused a node older than one that runs.

Nomad's upgrade guide, read on 2026-10-10, says: "Nomad strives to be backward compatible for at least 2 point
release, so Nomad v1.7.x works with v1.5.x." It also says that Nomad does not support downgrading, and that servers
should be upgraded before clients ([platform notes §1.3](../platform-notes.md#13-upgrades-and-version-skew)). A roll
runs two versions side by side for its whole length.

A local lab on 2026-10-10 ran Nomad 2.0.6 and 2.0.7 together in both directions with no version error (platform notes
§1.3). Nomad 2.0.7 also accepted a 2.0.6 server, so only tent's own rules keep an older node out.

## Decision

### The command

1. **`tent upgrade cluster [NAME] [--yes] [--allow-single-server]` writes the target version into
   `spec.nomad.version` of `cluster.yaml` and raises the store's tent version, as every spec write does; nothing
   changes in the cloud or in Nomad.** `update` pins it and `rolling-update` replaces the nodes.
2. **It reads the state store, the channel embedded in tent and the signed checksums of the target, and no cloud and no
   Nomad.** It is a spec command: it runs where `replace` and `edit` run, with no credentials. What the nodes run is
   checked by `update` and `rolling-update` (items 8 and 11 to 13).
3. **The target is the channel's recommended version.** The cluster's version is `spec.nomad.version` when set, else
   the pin. A cluster with neither has no version yet and its first `update` takes the recommended one. When the
   recommended version equals the cluster's, is older, or the cluster has no version, there is nothing to upgrade: the
   command says so, exits with 0, takes no lock and writes nothing. No flag names the target, no flag names the channel
   (`spec.channel` does, and `stable` is the only one) and there is no `--exit-code`. Another version that the channel
   allows goes into the spec with `edit`, and the same rules check it.
4. **Decision 45 as built: an upgrade that is due writes the version into the spec whether the spec set it before or
   only the pin held it,** so it is explicit from then on. The write goes through the checks of `replace`. A version
   below the channel's minimum is left out of the command's own check, since the upgrade is its way out; `update`,
   `rolling-update`, `validate cluster` and a `replace` or `edit` of a node group alone add `; run tent upgrade
   cluster` to that problem, and a write whose input holds the Cluster does not.
5. **The command reads the release files of the target before it writes** (`assets.CheckNomad`): the signed
   `SHA256SUMS` and its signature, and a line for the linux zip of each architecture. It cannot ask the cloud which
   architecture the machine types have, so it asks for both. It downloads no zip.

### How far a version may move

6. **A version may move up by at most one minor version.** The guide's sentence names two point releases, and one move
   stays inside them with one to spare. `channels.CheckMove` says so; a move to another major version is refused with
   no advice. The command checks the target against the pin and, when the channel allows no release of the minor
   version between, tells the operator to move with an older tent first. `create`, `replace` and `edit` check the
   spec's version against the pin, and so does every command that loads the specs.
7. **A version in the spec still wins over the pin, a lower one too** ([ADR-0026](0026-channels-and-release-assets.md),
   item 3). A refusal of every lower version in the spec writers would shut out the way back (item 11). tent refuses an
   older Nomad beside a newer node: `update` at its plan, `rolling-update` at every decision.
8. **`update` also holds a new pin against the oldest node that Nomad reports,** since the pin does not say what runs:
   a pin that moved twice with no roll between has nodes two moves behind.

### What `update` does with the version

9. **`update` writes a Nomad version into the completed spec only after it has read that version's release files.** A
   pin that was never released leaves a cluster that can create no node. This narrows decision 39, which let `update`
   warn and go on when it could not read the files: it still does, unless the plan would write another version.
10. **The pin moves in the completed spec alone.** Every spec hash changes, so `update` lists every node as outdated,
    creates and deletes nothing, and `rolling-update` does the rest.
11. **A plan that boots a node or moves the pin reads what a roll observes and fails whole when a node would run an
    older Nomad than a server or a node that is not down reports,** with and without `--yes`, before any write. A pin
    lowers only while none of them reports a newer Nomad. That is how an upgrade is taken back: while no node runs the
    new version, the old version in the spec lowers the pin, and once a node runs it the way is forward
    ([architecture §13.5](../architecture.md#135-tent-upgrade-cluster---yes)). When no server answers and the plan has
    an infrastructure change other than a delete, the check moves into the apply, after those changes, because an
    operator whose address changed must be able to fix the firewall with one `update --yes`. The details are in
    [architecture §13.2](../architecture.md#132-tent-update-cluster---yes).
12. **The apply stops before the first client that would run a newer Nomad than a server,** after the servers and the
    Nomad step. A refusal of the whole plan would hold back the servers of the same plan and the completed spec, and
    `rolling-update` needs both (`requireSize`, `checkApplied`). The plan warns that the client waits. The machines
    that never joined are deleted before the stop is returned, so that `update` and `rolling-update` do not loop
    without end beside a server machine that never came up. Two commands follow a stop, and no spec is edited
    (examples in [architecture §13.2](../architecture.md#132-tent-update-cluster---yes)).

### The roll after an upgrade

13. **The roll needs no new rule:** it keeps its order and its refusals (servers before clients, no client newer than a
    server, no node older than one that runs).
14. **A combined group is the one exception.** A new combined node runs a client of the new version while the other
    servers are old, and its create is not held back, since a combined group has no other order. The guide advises
    against a client ahead of its servers because new client features need new servers. The local run covered a newer
    client with older servers and not a combined agent. M3.9's `upgrade` scenario ran it on Vultr
    (`w28exz`, `ae5dhs`): a 2.0.7 combined node joined 2.0.6 ones and the roll ended.
15. **`rollout.CheckNewNode` holds the version rules of a new node** for `Next`, `serversHold` and `update`. A refusal
    says `the cluster's version is`, because the pin can differ from the version that a check holds. `validate cluster`
    takes the same phrase and warns that `update` has not applied the specs while the completed spec differs.
16. **tent reads no channel file from a URL** (decision 50, the maintainer, 2026-10-12): the channel is the one
    embedded in tent, and another Nomad version is set with `spec.nomad.version`. The CNI sha256s stay the ones
    reviewed when tent was released.

## Consequences

### Positive

- A new Nomad version reaches a cluster in three commands, and each checks the part it can see: the release files,
  the pin and Nomad's report.
- No command moves a node to an older Nomad, and no run starts a node of a client group ahead of the servers; a
  combined group is the exception (item 14).
- The upgrade runs on the fakes with versions per machine, and the plan is covered by goldens.

### Negative / trade-offs

- **The command does not know what the nodes run.** It says `nothing to upgrade` while a roll is under way, and it
  plans a second upgrade during one. `update` then holds the new pin against the oldest node (item 8).
- **A plan that boots a node or moves the pin needs the Nomad API** while the servers run: four more reads, and exit
  code 1 when they fail. A cluster that cannot answer gets no check. Behind a firewall that drops packets, a plan of
  three servers waits up to 90 s for M3.6's `Nodes` call and 90 s for the versions' read (30 s for each server that a
  call tries), and an applied update plans twice.
- **The owed check reads once,** so a run can end with the hint and the next run goes through.
- **A reading shows only what has joined Nomad.** A machine that still boots with the new version when the pin is
  lowered joins later ahead of the pin, and `rolling-update` refuses until the version is raised again or `update`
  removes the machine.
- **No command takes an upgrade back once the roll's first new server has joined** (Q14 of
  [architecture §18](../architecture.md#18-open-questions), answered (a) on 2026-10-13: decision 51, with no lab for
  (b)). The way back is by hand: delete that machine in the cloud, wait until its peer is gone, and run `tent update
  cluster`.
- **`validate cluster` exits with 2 from `upgrade --yes` until the roll has ended,** since the nodes differ from the
  specs.
- **Autopilot's report held a version for every Raft peer** at the read before the first client of a first build, in
  the create of the `ha` cluster on Vultr, in both runs `w28exz` and `ae5dhs` (2026-10-14).
- **The tests know two Nomad versions, one patch apart.** A move to the next minor version is tested as a rule and has
  never run.

### Follow-ups

- **M3.9** ran the `upgrade` scenario on Vultr from 2.0.6 to 2.0.7 with a combined cluster (runs `w28exz` and
  `ae5dhs`, [ADR-0043](0043-e2e-scenarios-ha-and-upgrade.md)), and 2.0.6 is in the channel's `tested` list since
  `ae5dhs`.
- A flag for the target version, a flow with a third version and an in-place upgrade of the binary
  ([ADR-0005](0005-immutable-nodes-and-nomad-aware-rollouts.md)) are left for later.

## Alternatives considered

- **`upgrade cluster --version X.Y.Z`.** It would do what `edit` does, with the release checked at once. One way to set
  an explicit version is enough.
- **Write the version on every `upgrade --yes`,** the letter of decision 45. A `replace -f` of a file without a version
  would undo it, and the pin holds the version anyway.
- **Ask the provider for the architectures, or read Nomad and refuse an upgrade during a roll.** Both make a spec
  command depend on credentials or on the cluster's API.
- **Any move that the channel allows, or two minor versions.** Until Nomad 2.2 exists the first has no rule to test,
  and the second leaves no room beside the guide's two point releases.
- **Refuse a lower version in the spec writers.** See item 7.
- **Refuse the whole plan for a client ahead of the servers.** See item 12.
- **Check before the client phase,** so that a client that never registered stays. The roll then stops at that machine
  and names `update`: one command more.
- **Read the servers in the apply only when the plan's reading showed an older server.** It saves two reads in runs
  that boot a client and adds a branch.
- **A channel file from a URL.** See item 16.
