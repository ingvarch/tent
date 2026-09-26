# tent

**tent** is a command-line tool that provisions and operates [HashiCorp Nomad](https://developer.hashicorp.com/nomad)
clusters on cloud providers. It aims to be what [kops](https://github.com/kubernetes/kops) is for Kubernetes. A nomad
pitches a tent anywhere; tent pitches a Nomad cluster on any cloud.

> **Status: early development (milestone M0); nothing usable yet.** Providers: [Vultr](https://www.vultr.com) first
> (it also hosts the E2E suite), then [Hetzner Cloud](https://www.hetzner.com/cloud). AWS is planned.

## What it will do

- **Keep a declarative cluster spec** (`Cluster` + `NodeGroup`) in a state store: a local directory or an
  S3-compatible bucket.
- **Reconcile cloud infrastructure** against the spec with a plan/apply workflow (`tent update cluster --yes`). No
  separate state file: the cloud is the source of truth.
- **Bootstrap Nomad securely.** mTLS for RPC and HTTP, gossip encryption, ACLs, client introduction and per-node
  certificates.
- **Run day-2 operations with Nomad semantics.**
  - Rolling updates drain clients and replace servers without losing Raft quorum.
  - Upgrades, scaling, validation and backups.
  - Teardown leaves nothing behind.

## Documentation

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
