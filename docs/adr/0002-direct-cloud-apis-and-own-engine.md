# ADR-0002: Call cloud APIs directly and reconcile with an in-house engine

- **Status:** Accepted
- **Date:** 2026-09-25
- **Deciders:** ingvarch
- **Related:** [ADR-0003](0003-cloud-is-source-of-truth.md), [ADR-0004](0004-layered-architecture.md),
  [architecture §6](../architecture.md#6-reconciliation-engine), [platform notes: kops](../platform-notes.md#41-kops)

## Context

tent must create, update and delete cloud infrastructure and show a plan before it changes anything. The candidates:

- Wrap an infrastructure-as-code tool (Terraform/OpenTofu, Pulumi) and drive it from tent.
- Use a cloud-native template service (CloudFormation-style), which Hetzner does not have.
- Call the provider SDKs directly and implement reconciliation ourselves, as kops and hetzner-k3s do.

Relevant forces:

- **Node lifecycle must understand Nomad.** It drains before delete and checks Raft quorum. This logic cannot live
  inside an IaC tool anyway.
- **Hetzner rate limit.** It allows 3600 requests per hour per project, so we need tight control over the number of
  API calls.
- **Distribution.** We want one static binary, without an external tool or a second state file to manage and lock.
- **Small surface.** Hetzner has fewer than ten resource kinds that tent manages.
- **kops' `fi` framework works but is costly.**
  - Reflection-based method contracts are checked only at runtime.
  - Dependency discovery by walking struct fields.
  - "nil means don't care" cannot express removing a field.
  - Deletion code is separate from creation code and drifts from it.
  - Imperfect normalisation of what `Find` reads causes perpetual diffs and needless rolling updates.

## Decision

- tent talks to cloud APIs directly through their official Go SDKs: `hcloud-go/v2` now, `aws-sdk-go-v2` later.
- tent has a small, typed reconciliation engine (`internal/engine`):
  - A `Task` has a stable `Key`, explicit `Deps`, and `Plan`, `Apply` and `Delete` methods. There is no
    reflection.
  - Discovery is snapshot-based. One labelled list call per resource kind is made per run, and `Plan` never calls
    the cloud.
  - `Apply` runs in parallel with a bounded worker pool. A failure cancels only its dependents, and all errors are
    reported together.
  - Every task normalises cloud values and has an "apply, re-plan, expect no-op" test.
  - Mutating commands are dry-run by default and print a plan (`+`, `~`, `-`, `-/+` with field diffs). `--yes`
    applies it.
- Machines (nodes) are not engine tasks. They are handled by the Nomad-aware `rollout` package
  ([ADR-0005](0005-immutable-nodes-and-nomad-aware-rollouts.md)).
- An export target for OpenTofu/Terraform, like kops' `--target=terraform`, may be added later as an extra
  rendering of the same tasks. It is not a v1 goal.

## Consequences

### Positive

- One binary, no external runtime, no second state.
- Full control over ordering, retries, concurrency and API call volume.
- Precise, readable plans. Nomad-aware flows fit naturally.

### Negative / trade-offs

- We own the diff and normalisation logic for every resource kind.
- We must build and maintain fakes of the cloud APIs for tests.
- No free ecosystem of providers: every cloud is real work. That is acceptable because each provider needs deep,
  cloud-specific behaviour anyway (identity, discovery, limits).

### Follow-ups

- `internal/engine` with golden plan tests (M1).
- An hcloud wrapper with adaptive rate limiting and batched action waits (M1).

## Alternatives considered

- **Terraform/OpenTofu under the hood.**
  - It needs an external binary and a tfstate that must be stored and locked alongside tent's own state.
  - Nomad-aware rolling operations would sit outside it anyway.
  - The UX is a wrapper around another tool's output.
- **Pulumi Automation API.** It brings a heavy runtime and plugin downloads, and has the same state duplication
  problem.
- **Crossplane / Cluster API style controllers.** They require a Kubernetes management cluster, which is absurd for
  a Nomad tool.
- **Porting kops' `fi` framework.** It is proven, but its pain points (see Context) are exactly what we want to
  avoid.
