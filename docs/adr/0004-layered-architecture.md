# ADR-0004: Layered architecture: a cloud-agnostic core and providers that translate intents

- **Status:** Accepted; provider order changed by [ADR-0014](0014-vultr-first-provider-and-e2e.md) (Vultr first);
  narrowed by [ADR-0021](0021-import-rules.md); amended by [ADR-0035](0035-rollout-decisions.md)
  (`internal/rollout` decides the next step and calls nothing; `internal/app` carries the steps out with the provider
  primitives and `internal/nomadops`)
- **Date:** 2026-09-25
- **Deciders:** ingvarch
- **Related:** [ADR-0002](0002-direct-cloud-apis-and-own-engine.md), [ADR-0005](0005-immutable-nodes-and-nomad-aware-rollouts.md),
  [architecture §4, §5, §7](../architecture.md#4-system-architecture)

## Context

Hetzner Cloud is the first provider and AWS is expected next. The two classic failure modes are:

- **The first provider leaks into the core.** Everything assumes Hetzner-shaped resources, and the second provider
  needs a rewrite.
- **A lowest-common-denominator abstraction.** It hides each cloud's strengths behind generic
  `EnsureNetwork/EnsureFirewall` calls that fit neither cloud well.

The clouds differ in ways that matter to tent:

- managed instance groups (AWS has them, Hetzner does not);
- signed instance identity (AWS has it, Hetzner does not);
- firewalls that filter private traffic (AWS Security Groups do, Hetzner Cloud Firewalls do not);
- cloud auto-join support in Nomad (AWS yes, Hetzner no);
- user data size limits.

## Decision

- **Three responsibilities with a hard boundary between them:**
  1. **Infrastructure** (network, subnets, firewalls, placement groups, load balancers, SSH keys). It does not depend
     on Nomad. Providers implement it as engine tasks.
  2. **Node lifecycle** (create, replace, remove machines). It is Nomad-aware and lives in the core
     (`internal/rollout`). It uses only provider primitives: `Nodes.List`, `Create`, `Shutdown` and `Delete`.
  3. **Nomad configuration** (agent config, TLS, ACL bootstrap, node pools). It does not depend on the cloud and
     lives in the core.
- **Intents.** The core computes a `model.Cluster` of intents, and that is the provider's only input:
  - network CIDR and named subnets;
  - access rules, including intra-cluster traffic;
  - node groups with their NodeConfig templates;
  - join strategy;
  - load balancers.

  Providers never see Nomad specifics.
- **Provider interface** with `Capabilities` flags: `ManagedGroups`, `InstanceIdentity`, `CloudAutoJoin`,
  `FirewallCoversPrivate`, `MaxUserDataBytes`, `CostEstimates`. The core branches on capabilities, never on provider
  names.
- **Provider-specific spec.** Cloud-specific settings live in provider-specific spec blocks (`spec.cloud.hetzner`,
  `spec.hetzner` on node groups; `aws` later). Machine types and images are provider-native strings.
- **Node side.** `tent-node` uses a per-provider `Environment` interface for the metadata service.
- **Enforced import rules** (depguard):
  - `api/` is stdlib-only;
  - core packages never import provider packages or cloud SDKs;
  - `internal/nodeup` never imports `internal/cloud/...`;
  - only `internal/nomadops` imports the Nomad API module.
- **Everything is `internal/` except `api/`.**

## Consequences

### Positive

- Adding AWS means a new package under `internal/cloud/aws` plus a node `Environment`. The core should not change.
- A provider cannot accidentally encode Nomad behaviour, and the core cannot accidentally depend on Hetzner.
- The import rules make the architecture testable by the linter.

### Negative / trade-offs

- Intents must be expressive enough for all providers. For example, intra-cluster rules exist in the model even
  though Hetzner ignores them.
- Some glue code is duplicated across providers.

### Follow-ups

- depguard configuration in M0.
- `model` intents and the Hetzner provider in M1.

## Alternatives considered

- **High-level `EnsureX` provider interface.** Lowest common denominator, and it leaks the first provider's shape.
- **One monolith per provider** that includes Nomad logic. The Nomad bootstrap, rollout and security logic would be
  duplicated per cloud.
- **Plugin architecture with out-of-process providers** (Terraform-style). Premature: RPC boundaries and versioning
  cost a lot for two in-tree providers.
