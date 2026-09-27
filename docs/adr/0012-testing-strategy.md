# ADR-0012: Testing strategy: fakes, golden integration tests, E2E on Hetzner

- **Status:** Accepted; E2E platform amended by [ADR-0014](0014-vultr-first-provider-and-e2e.md) (Vultr). Decided
  2026-09-27: the Vultr fake is an in-memory fake of `vultr.API` in `internal/cloud/vultr/vultrfake`.
- **Date:** 2026-09-25
- **Deciders:** ingvarch
- **Related:** [ADR-0002](0002-direct-cloud-apis-and-own-engine.md), [ADR-0004](0004-layered-architecture.md),
  [architecture §15](../architecture.md#15-testing)

## Context

- tent mutates real infrastructure and a distributed system. Most bugs would show up only after minutes of real
  cloud time, or only under failure: interruptions, rate limits, missing capacity.
- **Real-cloud testing is constrained:**
  - the Hetzner rate limit is per project;
  - accounts start with a 5-server limit;
  - server types can be unavailable;
  - billing is hourly.
- **kops' strongest safety net is `tests/integration`:** golden outputs of full runs against a fake cloud.
- **hcloud-go test helpers:** it exposes `I*Client` interfaces and an experimental `exp/mockutil`, a scripted
  httptest server that may break between minor releases.

## Decision

A test pyramid in which each layer has a clear job:

1. **Unit tests** next to the code: defaults and validation, PKI, the address plan, Nomad config rendering (golden
   HCL under `testdata/`), and rollout decisions written as pure functions (cluster state → next step).
2. **Provider tests.**
   - Hetzner tasks and `Nodes` run against an in-memory fake of narrow interfaces over hcloud-go.
   - The fake models uniqueness errors, actions, power states, and injected `412 resource_unavailable`, 429 and 5xx
     responses.
   - Every task has an "apply, re-plan, expect no-op" test.
   - A few contract tests use `exp/mockutil`, pinned to the hcloud-go version.
3. **Integration tests without a cloud.**
   - Full `update`, `rolling-update` and `delete` flows run against the fake provider and a fake Nomad API.
   - Golden files hold the plan output and the ordered sequence of operations.
   - Interruption tests cut a flow at every step and assert that the next run converges.
   - This is the main regression suite and runs on every pull request.
4. **tent-node tests.** Phases run against abstracted filesystem and exec. Occasionally they run in a
   systemd-enabled container or a VM.
5. **E2E on Hetzner** (`//go:build e2e`, black box: build `tent` and `tent-node`, drive the CLI). Scenarios:

   | Scenario | Shape and checks |
   |---|---|
   | `smoke` | 1 server + 1 client → validate → a docker job with a service → delete → **zero leaked labelled resources** |
   | `ha` | 3 servers in fsn1/nbg1/hel1 + 1 client; `rolling-update --force` while a probe writes Nomad Variables every second and counts errors; clients rejoin after every server was replaced |
   | `upgrade` | N-1 → N with a running job and no job downtime |
   | `arm` | CAX client group |
   | `security` | containers cannot reach `169.254.169.254`; `strict` client introduction rejects a node without a token |

   How E2E runs:
   - **Separate Hetzner project** with its own token, stored as a CI secret. Runs are serialized because the rate
     limit is per project.
   - **Dev builds of tent-node** are uploaded to object storage and passed to nodes as a presigned URL
     (`TENT_NODE_URL`, `TENT_NODE_SHA256`).
   - **Labels.** Every resource is labelled `tent/e2e=true` and `tent/e2e-run=<id>`. A janitor job deletes E2E
     resources older than 3 hours.
   - **Triggers:** nightly and on a pull request label, not on every commit.
   - **Size.** Every scenario fits the default 5-server account limit, surge included. Tests tolerate
     `resource_unavailable` by retrying in another location before failing.
6. **Static checks.** golangci-lint, including the depguard layer rules, `go test -race` and govulncheck on every
   pull request.

## Consequences

### Positive

- Most regressions are caught in seconds without cloud cost.
- The real-cloud suite targets what fakes cannot prove: boot, networking (MTU 1450, bridge networking), Raft
  behaviour and cleanup.

### Negative / trade-offs

- The fakes must be maintained and kept faithful. Behaviour discovered in E2E must be back-ported into the fakes.
- E2E needs a funded Hetzner project with raised limits for anything beyond 5 servers.

### Follow-ups

- The fake hcloud and fake Nomad API packages under `internal/.../fake` (M1–M2).
- The janitor in `hack/` and the E2E workflow (M2).

## Alternatives considered

- **Mostly E2E.** Slow, flaky, rate-limited and costly, and it proves little about failure paths.
- **Only HTTP-level mocks** (`mockutil`). Brittle scripted exchanges, and an experimental API that may break.
- **A local cloud emulator.** None exists for Hetzner.
