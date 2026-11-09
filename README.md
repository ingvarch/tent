<div align="center">

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/images/logo-dark.png">
  <img src="docs/images/logo-light.png" alt="tent" width="112">
</picture>

# tent

</div>

[![CI](https://github.com/ingvarch/tent/actions/workflows/ci.yml/badge.svg)](https://github.com/ingvarch/tent/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-00add8?logo=go&logoColor=white)](go.mod)
[![Release](https://img.shields.io/github/v/release/ingvarch/tent)](https://github.com/ingvarch/tent/releases)
[![License](https://img.shields.io/github/license/ingvarch/tent)](LICENSE)

tent is a command-line tool that builds and operates [HashiCorp Nomad](https://developer.hashicorp.com/nomad) clusters
on cloud providers, what [kops](https://github.com/kubernetes/kops) is for Kubernetes: a nomad pitches a tent anywhere,
and tent pitches a Nomad cluster on any cloud. It is for people who run Nomad
on their own cloud machines: they describe a cluster in a spec, and tent builds a secured cluster from it, then
rolls, scales, upgrades and backs it up. tent is in early development: release v0.2.0 runs on
[Vultr](https://www.vultr.com), [Hetzner Cloud](https://www.hetzner.com/cloud) is next, and AWS is planned. It is
made by [Igor Churmeev](https://github.com/ingvarch).

## Features

- **A declarative spec** for a cluster and its node groups, kept in a local directory or an S3-compatible bucket, with
  a plan before every change.
- **A secured Nomad cluster:** mTLS, gossip encryption, bootstrapped ACLs, and clients that join with an
  introduction token. The cloud is the source of truth, so there is no separate state file.
- **Access with short-lived credentials:** `tent export nomad` points the `nomad` CLI at the cluster and `tent ui`
  serves the Nomad web UI, without handing out the CA key or the bootstrap secret.
- **Rolling updates** that keep the Raft quorum, hand leadership over and drain clients before their machines go.
- **Scaling and upgrades:** groups grow and shrink, and Nomad moves up one minor version at a time once HashiCorp's
  signed checksums verify.
- **Backups** of Nomad's state, and a restore into servers rebuilt after a loss.
- **Safe re-runs:** a run cut halfway finishes when you run it again, and `tent delete cluster` removes everything the
  cluster owns.

## Usage

Install a release with Homebrew, or take an archive, a `.deb` or a `.rpm` from the
[releases](https://github.com/ingvarch/tent/releases). With a Vultr API key:

```sh
brew install --cask ingvarch/tap/tent
export TENT_STATE="file://$HOME/.tent" VULTR_API_KEY="<your API key>"
tent create cluster demo --provider vultr --region ams --machine-type vc2-1c-1gb --combined
tent update cluster demo --yes    # build the VPC, a firewall group, three machines and the Nomad cluster
tent validate cluster demo        # check it against the specs
```

The [quick start guide](docs/quickstart.md) walks through each step with its output, reaches the cluster with the
`nomad` CLI and deletes it again.

## Quick start

You need Go 1.26, golangci-lint v2.14.0 and make, see [docs/development.md](docs/development.md).

```sh
git clone https://github.com/ingvarch/tent.git
cd tent
make check
bin/tent version
```

## Documentation

- [Quick start](docs/quickstart.md): build a small cluster on Vultr, reach the Nomad API, run a job, open the UI,
  delete the cluster.
- [Features](docs/features.md): what each command does, the day-2 operations, the E2E suite, how the release was
  checked.
- [Development](docs/development.md): tools, build, checks, tests, development builds, the E2E suite, releases, project
  structure.
- [Architecture](docs/architecture.md): the domain model, the engine, providers, nodes, security, the state store,
  lifecycle flows, the CLI, testing.
- [Architecture Decision Records](docs/adr/README.md): why the design is the way it is.
- [Roadmap](docs/roadmap.md): milestones, exit criteria, status and log.
- [Platform notes](docs/platform-notes.md): verified facts about Nomad, Vultr, Hetzner Cloud, S3 stores and Ubuntu
  that the design relies on.
- [E2E suite](test/e2e/README.md): the scenarios on Vultr and the janitor.
- [Vultr spike](hack/vultr-spike/README.md): a script that checks undocumented Vultr behaviour on a real account.

## License

tent is licensed under the [Apache License 2.0](LICENSE).

tent installs official Nomad binaries from `releases.hashicorp.com` on the machines it creates. Nomad itself is
licensed by HashiCorp under the Business Source License 1.1. By using tent to install Nomad you accept HashiCorp's
terms for Nomad. tent does not redistribute Nomad.

Nomad is a trademark of HashiCorp. tent is an independent project and is not affiliated with or endorsed by
HashiCorp.
