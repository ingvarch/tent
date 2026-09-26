# ADR-0006: Two binaries and a versioned NodeConfig contract

- **Status:** Accepted
- **Date:** 2026-09-25
- **Deciders:** ingvarch
- **Related:** [ADR-0005](0005-immutable-nodes-and-nomad-aware-rollouts.md), [ADR-0008](0008-node-credential-delivery.md),
  [architecture §8](../architecture.md#8-nodes-tent-node-and-nodeconfig)

## Context

Something must turn a fresh VM into a Nomad agent. The options:

- shell scripts in cloud-init;
- provisioning over SSH from the operator's machine (hashi-up, hetzner-k3s);
- a node agent binary (kops' `nodeup`);
- reusing the CLI binary on nodes.

Constraints:

- **User data size.** Hetzner user data is at most 32 KiB and immutable, so the tent-node binary cannot be embedded
  in it.
- **Self-sufficient nodes.** Nodes must be able to bootstrap without the operator, which is a precondition for
  future autoscaling and auto-repair.
- **Attack surface.** Nodes should carry no code that can mutate cloud resources.
- **HashiCorp APT repository.** Its signing key was rotated on 2026-09-09 after a security incident, which broke
  pinned copies.
- **kops' node-hash lesson.** Environment-dependent inputs such as `KOPS_BASE_URL` change the node hash and trigger
  needless rolls.

## Decision

- **Two binaries from one Go module:**
  - `tent` is the CLI;
  - `tent-node` is the node agent.

  tent-node's version always equals the CLI version that created the node.
- **User data is a minimal cloud-config.** It contains:
  - the NodeConfig, gzip+base64, written to `/etc/tent/node.json` with mode 0600;
  - a download of tent-node from mirrors, with sha256 verification;
  - `tent-node install`, which installs a systemd oneshot unit that runs `tent-node up` on every boot.
- **NodeConfig is a versioned contract** (`apiVersion: tent/v1alpha1`, `kind: NodeConfig`). It carries:
  - assets (name, mirror URLs, sha256);
  - files with mode and owner, marked group-level or per-node;
  - system settings;
  - host firewall rules;
  - the semantic spec hash.
- **The CLI renders the final Nomad agent configuration.** tent-node only lays out files and manages the OS: sysctls,
  modules, container runtime, CNI plugins, systemd. Values known only at runtime use go-sockaddr templates inside the
  Nomad configuration.
- **The spec hash is semantic.**
  - Included: group-level files, asset versions and sha256s, system settings, the host firewall and the tent-node
    version.
  - Excluded: per-node files (certificates, keys, intro token, `10-node.hcl`) and mirror URLs.
- **Downloads are verified on the operator side.**
  - The CLI downloads Nomad's `SHA256SUMS` and verifies its detached signature with HashiCorp's release key, which is
    embedded in tent.
  - Nodes verify only sha256 values from NodeConfig.
  - tent never uses the HashiCorp APT repository.
- **Every tent-node phase is idempotent.** Nomad is restarted only when its files actually changed.
- **Size budget.** Tests fail if the encoded NodeConfig exceeds 24 KiB, which leaves headroom under 32 KiB.

## Consequences

### Positive

- Nodes bootstrap on their own, and the logic is tested Go code instead of shell.
- Nodes carry no cloud-mutating code and no PGP tooling.
- `tent update` can show exact Nomad configuration diffs per group.
- A different download mirror never rolls the cluster.

### Negative / trade-offs

- The release pipeline must publish tent-node for linux/amd64 and linux/arm64.
- Development builds need hosting reachable from the VMs (a presigned object storage URL, `TENT_NODE_URL`).
- A new tent version that changes rendering rolls all nodes. The plan states this explicitly.

### Follow-ups

- `internal/nodeconfig` with golden HCL tests; `internal/nodeup` phases with abstracted fs/exec (M2).
- GoReleaser configuration for both binaries; dev upload helper in `hack/` (M2).

## Alternatives considered

- **Bash in cloud-init.** It is hard to test and to make idempotent, and it grows without bounds.
- **SSH provisioning from the operator.** It needs the operator online and SSH reachable from wherever the CLI runs,
  and it cannot support autoscaling.
- **One binary for both roles.** It is larger, and it puts cloud-mutating code on every node.
- **HashiCorp APT repository for Nomad.** It couples tent to OS packaging and to a signing key that was just rotated
  after an incident.
