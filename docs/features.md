# Features

What tent v0.2.0 does, command by command, and how it was checked. To build a first cluster, start with the
[quick start](quickstart.md); for what comes next, see the [roadmap](roadmap.md).

tent builds a secured Nomad cluster on [Vultr](https://www.vultr.com) and runs its day-2 operations: rolling updates
that keep the Raft quorum and drain clients, scaling up and down, upgrades of Nomad, and backups. Vultr is the only
provider so far. [Hetzner Cloud](https://www.hetzner.com/cloud) is next, and AWS is planned.

## What works now

- **A declarative cluster spec** (`Cluster` + `NodeGroup`) in a state store: a local directory or an S3-compatible
  bucket. `tent create`, `get`, `edit` and `replace` manage it.
- **Plan and apply on Vultr** with `tent update cluster [--yes]`: the VPC, the firewall groups, the SSH keys, each
  node group at its size, and Nomad on the machines: mTLS, gossip encryption, ACLs bootstrapped with a secret that
  tent keeps in the state store, and clients that join with an introduction token. Once a node has joined, tent
  replaces its user data with a stub and labels its machine, and it replaces a client that never registered. No
  separate state file: the cloud is the source of truth.
- **Access to the Nomad API.** `tent export nomad` writes a short-lived client certificate and a management token into
  files and prints the lines that point the `nomad` CLI at them. `tent ui` serves the Nomad web UI and API on a
  loopback port of your machine, through a proxy that adds the certificate and a token. Neither gives you the cluster's
  CA key or its ACL bootstrap secret.
- **A check against the specs.** `tent validate cluster` compares the machines, the Nomad servers and clients, their
  versions and the certificates with the specs, prints what differs and exits with 2 while something does. A combined
  cluster, an open `access.api` and a single failure domain are warnings.
- **Safe re-runs.** A run that stops halfway can run again. tent adopts what it created by the markers on each
  object and never creates a second machine for one node.
- **Rolling updates** with `tent rolling-update cluster [--yes]`. After a change of the specs or of tent, `update`
  lists the nodes that are outdated, and `rolling-update` replaces them with new machines:
  - servers and combined nodes one at a time, with the new one in the cluster first; a leader hands its leadership
    over before it stops;
  - clients by the group's `maxSurge` and `maxUnavailable`, each drained before its machine goes;
  - `--force` replaces nodes whatever their specs, and `--nodegroups` picks the groups.
- **Scaling** with `tent update cluster --yes`. It grows a group, and it shrinks one: a client is drained before it is
  deleted, and a server leaves Raft and the gossip pool first. Server groups go from 3 to 1 and back.
- **Upgrades of Nomad** with `tent upgrade cluster --yes`. It moves the cluster to the version that its release channel
  recommends, once HashiCorp's signed checksums of that release verify, by at most one minor version at a time. Then
  `update` and `rolling-update` replace the servers first and the clients after them.
- **Backups of Nomad's state.** `tent backup create` stores a snapshot of the jobs, tokens and variables in the state
  store, `tent backup list` shows them, and `tent backup restore --yes` puts one back into servers that `tent update
  cluster` built again after a loss.
- **Full teardown** with `tent delete cluster --yes`: every object with the cluster's markers, then its state and its
  backups.
- **An end-to-end suite on Vultr** (`make e2e`) with three scenarios ([details](../test/e2e/README.md)):
  - `smoke` builds a cluster on Ubuntu 24.04 and one on 26.04. It runs a job with a service, checks that containers
    cannot reach the metadata service and that the servers refuse a node without an intro token, and deletes the
    clusters.
  - `ha` rolls a cluster of 3 servers and 2 workers, restarts a client after every server was replaced, and restores
    a backup into rebuilt servers.
  - `upgrade` moves a combined cluster of 3 nodes from Nomad 2.0.6 to 2.0.7.
  - During each roll a probe asks the servers for a leader and Nomad for a healthy allocation of the service, once a
    second.

## Day-2 operations

On a cluster that the [quick start](quickstart.md) built:

```sh
tent edit cluster demo                  # change the specs in an editor, then:
tent update cluster demo --yes          # apply them; the plan lists the nodes that are outdated
tent rolling-update cluster demo --yes  # replace the outdated nodes
tent upgrade cluster demo --yes         # move to the Nomad version that the channel recommends
tent backup create demo                 # store a snapshot of Nomad's state
```

[Architecture §13](architecture.md#13-lifecycle-flows) describes each of them step by step, and
[§14](architecture.md#14-cli) lists every command, flag and exit code.

## How it was checked

- **The end-to-end suite** passed in full on Vultr on 2026-10-14, in runs `qkmdrs` and `hik34s`, the second on the
  code of v0.2.0. In the rolls of `ha` and `upgrade`, no poll found the cluster without a leader, and none found the
  service without a healthy allocation. The restarted client came back with its node ID. After the restore, the
  workers came back on their own.
- **A check on Vultr for each part of the day-2 work:**
  - the roll of clients (2026-10-08);
  - the roll of three servers, with three workers whose allocations kept running, and of a single server
    (2026-10-10);
  - the roll of combined nodes (2026-10-12);
  - workers from 2 to 1 and servers from 3 to 1 and back to 3 (2026-10-12);
  - a backup, the loss of the server and a restore (2026-10-13).
- The runs, with their times and what they found, are in
  [platform notes §3.16](platform-notes.md#316-spike-runs). The unit and flow tests run every roll, scale-down,
  upgrade, backup and restore on fakes of Vultr and Nomad, and they cut runs short to show that a run started again
  finishes the work.
