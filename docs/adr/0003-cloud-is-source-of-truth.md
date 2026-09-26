# ADR-0003: The cloud is the source of truth: ownership labels and deterministic names

- **Status:** Accepted (label prefix `tent/` and API group `tent/v1alpha1` confirmed by the maintainer on 2026-09-25); extended by [ADR-0015](0015-idempotency-without-unique-names.md)
- **Date:** 2026-09-25
- **Deciders:** ingvarch
- **Related:** [ADR-0002](0002-direct-cloud-apis-and-own-engine.md), [ADR-0009](0009-server-discovery-fixed-ip-slots.md),
  [architecture §3.4](../architecture.md#34-naming-and-ownership-markers)

## Context

- tent must survive interruptions (Ctrl-C, crashes, network failures) and concurrent operators without corrupting
  the cluster.
- kops states the principle "do not store cloud state that can be re-read from the cloud", and we agree with it.
  A separate state file for actual infrastructure drifts, needs locking and can be corrupted.
- **Hetzner specifics:**
  - create calls are not idempotent, and there are no client tokens;
  - resource names *are* unique per project;
  - labels follow Kubernetes-style rules: an optional DNS-subdomain prefix, and names/values of at most 63
    characters;
  - the `hetzner.cloud/` prefix is reserved.
- **kops on Hetzner uses random server names** (`<ig>-<random hex>`). A retried create can therefore produce a
  duplicate, and two concurrent runs can over-provision.
- **CAPH and hetzner-k3s use deterministic names** and adopt existing resources on conflict.

## Decision

- **No state file for actual infrastructure.** Each run discovers the cluster's resources by the label selector
  `tent/cluster=<name>`.
- **Ownership labels.** Every resource tent creates carries `tent/cluster=<name>`, plus, where applicable:
  - `tent/nodegroup`;
  - `tent/role` (`server` / `client`);
  - `tent/spec-hash` (16 hex characters);
  - `tent/slot` (Nomad server slot).

  The label is the only ownership signal.
- **Deterministic names:**
  - network: `<cluster>`;
  - firewalls: `<cluster>-nodes`, `<cluster>-servers`;
  - placement groups: `<cluster>-<group>-<shard>`;
  - load balancers: `<cluster>-api`, `<cluster>-internal`;
  - servers: `<cluster>-<group>-<index>`. For Nomad servers the index is the slot; for clients it is the lowest free
    index.
- **Adopt only what is ours.** A `uniqueness_error` on create means "look it up by name and adopt it" **only if**
  its labels say it belongs to this cluster. Otherwise tent fails with a clear error.
- **Never delete what tent did not create.** This covers pre-existing SSH keys (Hetzner keys are unique per
  fingerprint, so tent adopts them without labelling) and volumes created by CSI drivers.
- **Name limits.** Cluster and node group names match `^[a-z][a-z0-9-]{0,18}[a-z0-9]$`, so every derived name stays
  a valid hostname of at most 63 characters.
- **Label prefix and API group:** `tent/` and `tent/v1alpha1`, confirmed by the maintainer on 2026-09-25 (instead of
  a domain the maintainer controls). Changing the prefix after clusters exist requires a migration that relabels
  every resource.

## Consequences

### Positive

- Every command is re-runnable and converges after interruptions.
- A duplicated create collides on the unique name instead of creating a second resource.
- Several clusters can share one Hetzner project, although a project per cluster is recommended.

### Negative / trade-offs

- Short cluster and group names.
- Client node names are reused over time. `prod-workers-3` today may be a different VM than last week. Nomad node
  ids still differ.
- The label prefix is effectively permanent.
- Resources created outside tent are invisible unless they carry the labels.

### Follow-ups

- Label and name helpers with validation in `internal/cloud/hetzner` (M1).
- The label prefix was decided on 2026-09-25: `tent/`.

## Alternatives considered

- **A tfstate-like state file** for actual infrastructure: drift, locking and corruption. It duplicates what the
  cloud already knows.
- **Random name suffixes** (kops): non-idempotent creation and duplicates under concurrency.
- **Name-prefix ownership** without labels: prefixes collide across clusters (`prod` vs `prod-eu`) and cannot
  express roles or hashes.
