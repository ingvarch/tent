<div align="center">

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/images/logo-dark.png">
  <img src="docs/images/logo-light.png" alt="tent" width="112">
</picture>

# tent

</div>

**tent** is a command-line tool that provisions and operates [HashiCorp Nomad](https://developer.hashicorp.com/nomad)
clusters on cloud providers. It aims to be what [kops](https://github.com/kubernetes/kops) is for Kubernetes. A nomad
pitches a tent anywhere; tent pitches a Nomad cluster on any cloud.

> **Status: early development, release v0.2.0.** tent builds a secured Nomad cluster on [Vultr](https://www.vultr.com)
> and runs its day-2 operations: rolling updates that keep the Raft quorum and drain clients, scaling up and down,
> upgrades of Nomad, and backups. Each of them has run on Vultr, in the end-to-end suite or in a check of its own
> ([how it was checked](#how-it-was-checked)). Vultr is the only provider so far.
> [Hetzner Cloud](https://www.hetzner.com/cloud) is next, and AWS is planned.

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
- **An end-to-end suite on Vultr** (`make e2e`) with three scenarios ([details](test/e2e/README.md)):
  - `smoke` builds a cluster on Ubuntu 24.04 and one on 26.04. It runs a job with a service, checks that containers
    cannot reach the metadata service and that the servers refuse a node without an intro token, and deletes the
    clusters.
  - `ha` rolls a cluster of 3 servers and 2 workers, restarts a client after every server was replaced, and restores
    a backup into rebuilt servers.
  - `upgrade` moves a combined cluster of 3 nodes from Nomad 2.0.6 to 2.0.7.
  - During each roll a probe asks the servers for a leader and Nomad for a healthy allocation of the service, once a
    second.

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
  [platform notes §3.16](docs/platform-notes.md#316-spike-runs). The unit and flow tests run every roll, scale-down,
  upgrade, backup and restore on fakes of Vultr and Nomad, and they cut runs short to show that a run started again
  finishes the work.

## What comes next

- **More providers:** Hetzner Cloud (M4), then AWS (M6).
- **Provider polish** (M5): load balancers, DNS, cost, CSI addons, a private topology and diagnostics.

## Quick start

With a Vultr API key. Install a release: with Homebrew, or an archive, a `.deb` or a `.rpm` from the
[releases](https://github.com/ingvarch/tent/releases). A release finds the tent-node of its nodes in its own release.

```sh
brew install --cask ingvarch/tap/tent
export TENT_STATE="file://$HOME/.tent" VULTR_API_KEY="<your API key>"

tent create cluster demo --provider vultr --region ams --machine-type vc2-1c-1gb --combined
tent update cluster demo          # print the plan
tent update cluster demo --yes    # build the VPC, a firewall group, three machines and the Nomad cluster
eval "$(tent export nomad demo)"  # point the nomad CLI at it (in fish: tent export nomad demo | source)
nomad server members
tent validate cluster demo        # check it against the specs
tent delete cluster demo --yes    # delete them, the specs and the secrets
```

The [quick start guide](docs/quickstart.md) walks through each step with its output, SSH access, a job with the `nomad`
CLI and the Nomad web UI.

Day-2 operations on the same cluster:

```sh
tent edit cluster demo                  # change the specs in an editor, then:
tent update cluster demo --yes          # apply them; the plan lists the nodes that are outdated
tent rolling-update cluster demo --yes  # replace the outdated nodes
tent upgrade cluster demo --yes         # move to the Nomad version that the channel recommends
tent backup create demo                 # store a snapshot of Nomad's state
```

A build from `main`, such as `go install github.com/ingvarch/tent/cmd/tent@main`, is a development build: its nodes need
a tent-node built from the same commit, which you give with `TENT_NODE_URL` and `TENT_NODE_SHA256`. In a clone of the
repository, `make dev-upload` prints the lines that set them ([details](hack/tent-node-upload/README.md)).

## Documentation

- [Quick start](docs/quickstart.md): build a small cluster on Vultr and delete it again.
- [Architecture](docs/architecture.md): the full design.
- [Architecture Decision Records](docs/adr/README.md): why the design is the way it is.
- [Roadmap](docs/roadmap.md): milestones and current status.
- [Platform notes](docs/platform-notes.md): verified facts about Nomad, Vultr, Hetzner Cloud and prior art that the
  design relies on.
- [E2E suite](test/e2e/README.md): how the end-to-end suite runs on Vultr.
- [Vultr spike](hack/vultr-spike/README.md): a script that checks undocumented Vultr behaviour on a real account.

## License

tent is licensed under the [Apache License 2.0](LICENSE).

tent installs official Nomad binaries from `releases.hashicorp.com` on the machines it creates. Nomad itself is
licensed by HashiCorp under the Business Source License 1.1. By using tent to install Nomad you accept HashiCorp's
terms for Nomad. tent does not redistribute Nomad.

Nomad is a trademark of HashiCorp. tent is an independent project and is not affiliated with or endorsed by
HashiCorp.
