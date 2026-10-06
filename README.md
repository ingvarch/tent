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

> **Status: early development (milestone M2 in progress).** `tent update cluster --yes` builds a running Nomad
> cluster on [Vultr](https://www.vultr.com): servers with a leader, ACLs bootstrapped and clients registered, and it
> scrubs a node's keys from its user data once the node has joined. It is not for production yet. Vultr is the first
> provider (it also hosts the E2E suite), then [Hetzner Cloud](https://www.hetzner.com/cloud). AWS is planned.

## What works now

- **A declarative cluster spec** (`Cluster` + `NodeGroup`) in a state store: a local directory or an S3-compatible
  bucket. `tent create`, `get`, `edit` and `replace` manage it.
- **Plan and apply on Vultr** with `tent update cluster [--yes]`: the VPC, the firewall groups, the SSH keys, each
  node group at its size, and Nomad on the machines: mTLS, gossip encryption, ACLs bootstrapped with a secret that
  tent keeps in the state store, and clients that join with an introduction token. Once a node has joined, tent
  replaces its user data with a stub and labels its machine, and it replaces a client that never registered. No
  separate state file: the cloud is the source of truth.
- **Safe re-runs.** A run that stops halfway can run again. tent adopts what it created by the markers on each
  object and never creates a second machine for one node.
- **Full teardown** with `tent delete cluster --yes`: every object with the cluster's markers, then its state.

## What comes next

- **Finish the Nomad bootstrap** (M2).
  - M2.8: `tent export nomad` for the operator's access to the Nomad API, `validate cluster` and `tent ui`.
- **Run day-2 operations with Nomad semantics** (M3).
  - Rolling updates drain clients and replace servers without losing Raft quorum.
  - Upgrades, scaling (before M3 `update` refuses to delete a node that joined, since it cannot drain a node or check
    the Raft quorum) and backups.
- **More providers:** Hetzner Cloud (M4), then AWS (M6).

## Quick start

With Go 1.26 or newer and a Vultr API key. A build from `main` is a development build: its nodes need a tent-node built
from the same commit, which you give with `TENT_NODE_URL` and `TENT_NODE_SHA256`. In a clone of the repository, `make
dev-upload` prints the lines that set them ([details](hack/tent-node-upload/README.md)). A release build finds its
tent-node in its own release, and no release has the bootstrap yet.

```sh
go install github.com/ingvarch/tent/cmd/tent@main
export TENT_NODE_URL="<tent-node URL>" TENT_NODE_SHA256="<its sha256>"
export TENT_STATE="file://$HOME/.tent" VULTR_API_KEY="<your API key>"

tent create cluster demo --provider vultr --region ams --machine-type vc2-1c-1gb --combined
tent update cluster demo          # print the plan
tent update cluster demo --yes    # build the VPC, a firewall group, three machines and the Nomad cluster
tent delete cluster demo --yes    # delete them, the specs and the secrets
```

The [quick start guide](docs/quickstart.md) walks through each step with its output, SSH access and the Nomad API
access list.

## Documentation

- [Quick start](docs/quickstart.md): build a small cluster on Vultr and delete it again.
- [Architecture](docs/architecture.md): the full design.
- [Architecture Decision Records](docs/adr/README.md): why the design is the way it is.
- [Roadmap](docs/roadmap.md): milestones and current status.
- [Platform notes](docs/platform-notes.md): verified facts about Nomad, Vultr, Hetzner Cloud and prior art that the
  design relies on.
- [Vultr spike](hack/vultr-spike/README.md): a script that checks undocumented Vultr behaviour on a real account.

## License

tent is licensed under the [Apache License 2.0](LICENSE).

tent installs official Nomad binaries from `releases.hashicorp.com` on the machines it creates. Nomad itself is
licensed by HashiCorp under the Business Source License 1.1. By using tent to install Nomad you accept HashiCorp's
terms for Nomad. tent does not redistribute Nomad.

Nomad is a trademark of HashiCorp. tent is an independent project and is not affiliated with or endorsed by
HashiCorp.
