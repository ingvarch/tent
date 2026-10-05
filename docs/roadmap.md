# Roadmap

> **Current status (2026-09-28):** M0 Foundation and M1 Vultr infrastructure are complete.
>
> M0 built the Go module, `tent version`, the Makefile, the import rules in golangci-lint, CI on Linux, macOS and
> Windows, the release pipeline for `tent`, the API types with defaults and validation, the spec reader and writer,
> the JSON Schema ([ADR-0022](adr/0022-json-schema-from-go-types.md)), the state store with its `file://` and `s3://`
> backends, version guard and cluster locks ([architecture §10](architecture.md#10-state-store-and-locking)), and the
> spec commands of the CLI ([architecture §14](architecture.md#14-cli)). Renovate updates the dependencies ([#9][i9]).
> The repository is public with the release secrets set ([#10][i10]), and the archives and packages ship third-party
> licence notices while CI checks the licences of the modules tent links ([#11][i11]).
>
> M1 built the reconciliation engine ([architecture §6](architecture.md#6-reconciliation-engine)), the Vultr API
> client and its fake, the Vultr infrastructure tasks and node primitives
> ([architecture §11](architecture.md#11-vultr-provider)), and `tent update cluster` and `tent delete cluster`
> ([architecture §13.2](architecture.md#132-tent-update-cluster---yes),
> [§13.7](architecture.md#137-tent-delete-cluster---yes)). The nodes are empty machines without Nomad.
>
> - Vultr is the first provider and the E2E platform ([ADR-0014](adr/0014-vultr-first-provider-and-e2e.md)).
> - Hetzner Cloud is second.
> - **Next:** M2 Nomad bootstrap.
>
> **Work items live in GitHub:** each milestone below links to its GitHub milestone, and the
> [tent roadmap project][project] shows the open issues. This file keeps the goals and exit criteria; close issues as
> they land, and add a line to the log when a milestone completes.

Each milestone ends in a state that can be demonstrated. A milestone is done when its **exit criteria** hold in CI
and, from M2 on, in the E2E suite on Vultr.

## Maintainer decisions

These come from [architecture §18](architecture.md#18-open-questions). The first six were decided on 2026-09-25, the
seventh to the fifteenth on 2026-09-28, the sixteenth to the twenty-first on 2026-09-29, the twenty-second to the
twenty-fourth on 2026-09-30, and the twenty-fifth on 2026-10-05. The maintainer extended the twenty-third on
2026-10-02.

| # | Question | Decision |
|---|---|---|
| 1 | Label prefix and API group | `tent/` and `tent/v1alpha1` |
| 2 | Default `access.api` | `[0.0.0.0/0]` with mTLS + ACL, and a loud warning while it is open |
| 3 | Combined server+client mode | allowed for dev and small clusters through the `combined` role ([ADR-0019](adr/0019-combined-server-client-role.md)) |
| 4 | Default OS image | `ubuntu-24.04`; E2E also runs on `ubuntu-26.04` |
| 5 | Consul and Vault | out of v1 |
| 6 | Licence of tent | Apache-2.0 |
| 7 | A cluster's provider and region | never change; a cluster moves by creating a new one |
| 8 | CA validity | 10 years, until CA rotation exists |
| 9 | Nomad's sha256s | checked at run time against the signed `SHA256SUMS`, with HashiCorp's key embedded in tent ([ADR-0026](adr/0026-channels-and-release-assets.md)) |
| 10 | Nomad version of a spec without one | the channel's recommended one, pinned in `cluster.completed.yaml` by the first `update`; any release from the channel's minimum up to the next major is allowed, and untested ones get a warning |
| 11 | What a channel holds | Nomad and the CNI plugins only; images stay in the provider's table and the API default |
| 12 | When NodeConfig reaches `update` | in M2.7, with tent-node, intro tokens and the bootstrap; until then nodes boot a placeholder without secrets, and the asset downloads, the development-variable warning and the Nomad pin before the first node move there too ([ADR-0027](adr/0027-nodeconfig-contract-rendering-and-spec-hash.md)) |
| 13 | The instance id | NodeConfig carries the node's name; tent-node reads the instance id from the metadata service and writes it into `11-instance.hcl` |
| 14 | The user data budget | 24 KiB for the whole cloud-config on every provider; a per-provider limit comes when a provider needs less |
| 15 | The host firewall and `access` | the host does not copy `access`: SSH, ICMP and 4646 on servers are open on the host and the cloud firewall filters their sources; Nomad's and the dynamic ports only from the cluster CIDR (since M2.6a also from Nomad's and Docker's default bridges on client and combined nodes; the metadata block is decision 21); a change of `access` never changes the spec hash |
| 16 | The Nomad API module | pinned to the commit of the Nomad tag that the channel recommends (v2.0.7 now) and moved by hand with the channel; Renovate is off for it |
| 17 | Node pools | tent creates none; Nomad creates a pool when its first client registers; pool descriptions or meta come when the spec has such fields |
| 18 | Where development builds of tent-node live | the CI R2 bucket under `dev/`, served by presigned URLs valid for at most 7 days; the maintainer's machine has its own R2 token; a lifecycle rule deletes `dev/` after 8 days ([ADR-0028](adr/0028-tent-node-agent-units-and-delivery.md)) |
| 19 | The handover from cloud-init to tent-node | `install` starts `tent-node.service` and waits for it; the unit is not ordered on `cloud-final.service`, `cloud-init.target`, `cloud-config.service`, `multi-user.target` or `nomad.service` (nor, as a precaution since M2.6a, `cloud-init-main.service`), and has a finite `TimeoutStartSec`; `up` starts Nomad itself, and `verify` checks it without waiting for a leader |
| 20 | The provider on the node | NodeConfig carries `provider`, validated and outside the spec hash; tent-node picks its metadata service by it |
| 21 | The metadata service on a node | only tent-node's socket reaches it, by the socket mark `0x747`; every other packet to it is dropped, from the host and from containers; root `curl` on a node gets no answer, and a workload with CAP_NET_ADMIN or CAP_NET_RAW can still set the mark ([ADR-0029](adr/0029-host-firewall-runtime-and-cni-on-nodes.md)) |
| 22 | The Nomad region on the node | a `region` field in NodeConfig, validated as `spec.nomad.region` is and outside the spec hash; tent-node calls servers as `server.<region>.nomad` ([ADR-0030](adr/0030-nomad-on-nodes.md)) |
| 23 | `nomad config validate` of the golden files | an online test in the weekly CI job, not on every pull request; since 2026-10-02 with both the channel's minimum and its recommended Nomad |
| 24 | The secrets of the tent-node VM check | `hack/tent-node-userdata` makes a throwaway CA, node certificate and gossip key per run; they sit in the user data of a VM that is deleted after the check, with no scrub before M2.7 |
| 25 | Draining at shutdown | clients do not drain themselves when Nomad stops, since Nomad's self-drain leaves them ineligible after their next start; tent's own removals drain a client through the Nomad API ([ADR-0030](adr/0030-nomad-on-nodes.md)) |

New questions for the maintainer are issues with the `decision` label.

## M0 Foundation (no cloud)

[Issues](https://github.com/ingvarch/tent/milestone/1)

**Goal:** a repository skeleton with the API, the state store and spec-only CLI commands, guarded by CI.

**Exit criteria:**
- Lint and tests are green in CI.
- A spec survives `create → get -o yaml → replace` unchanged.
- Invalid specs produce field-path errors.
- Two concurrent mutating commands on the file backend are serialized.

## Spike: Vultr unknowns (done)

`hack/vultr-spike` measured the undocumented Vultr behaviour the provider relies on, in five runs in `ams`: three on
2026-09-25, one on 2026-09-27 and one on 2026-09-28. The results are in
[platform notes §3.16](platform-notes.md#316-spike-runs),
and ADR-0018 is resolved except item 11 (Object Storage conditional writes, tracked as a `later` issue). How to run
it: [`hack/vultr-spike/README.md`](../hack/vultr-spike/README.md).

## M1 Vultr infrastructure

[Issues](https://github.com/ingvarch/tent/milestone/2)

**Goal:** tent creates and deletes the cloud side of a cluster on Vultr, empty VMs included, with a correct plan.

**Exit criteria:**
- `tent update cluster --yes` creates the VPC, the firewall groups and N empty VMs on Vultr, and a re-run is a no-op.
- A simulated lost create response never produces a duplicate VM.
- `tent delete cluster --yes` leaves nothing that carries the cluster markers.

**Status:** done. All three criteria hold in CI, where the integration tests run the Vultr provider on its fake
([architecture §15](architecture.md#15-testing)), and held on a real Vultr account on 2026-09-28
([platform notes §3.16](platform-notes.md#316-spike-runs)):
- `create cluster --yes`, which runs `update cluster --yes`, built the VPC, both firewall groups and two VMs, and
  `update cluster --exit-code` then found no changes.
- In CI, each create, in a run of its own, loses its answer, and a build is cut before and after each of its calls;
  each ends with one instance per node. On the real account, an `update --yes` interrupted after a VM's create call left
  one VM, which the next run waited for instead of creating another.
- `delete cluster --yes` deleted the VMs, the firewall groups, the VPC and the state, and the API then listed nothing
  with the cluster's markers.

## M2 Nomad bootstrap

[Issues](https://github.com/ingvarch/tent/milestone/3)

**Goal:** a working, secured Nomad cluster on Vultr with a running job.

**Exit criteria:** E2E `smoke` passes on Vultr.
- A docker job with a service runs.
- `delete` leaves nothing behind.
- Containers cannot reach the metadata endpoint.
- A client without an intro token is rejected.

## M3 Day-2 operations

[Issues](https://github.com/ingvarch/tent/milestone/4)

**Goal:** Nomad-aware rolling updates, scaling, upgrades and backups.

**Exit criteria:** E2E `ha` and `upgrade` pass on Vultr, with no quorum loss and no job downtime beyond the configured
migrations.

## M4 Hetzner provider

[Issues](https://github.com/ingvarch/tent/milestone/5)

**Goal:** the second provider, implemented once a Hetzner account can create servers reliably (limits raised, no
creation restrictions).

**Exit criteria:** the E2E suite passes on Hetzner, plus `arm` (CAX) and location-spread HA (fsn1/nbg1/hel1).

## M5 Provider polish

[Issues](https://github.com/ingvarch/tent/milestone/6)

**Goal:** load balancers, DNS, cost, CSI addons, private topology and diagnostics.

## M6 AWS provider

[Issues](https://github.com/ingvarch/tent/milestone/7)

**Goal:** the third provider, through the same core.

**Exit criteria:** the E2E suite passes on AWS.

## Later

Work after M6 or not scheduled yet: [issues with the `later` label][later].

## Log

- 2026-09-25: design accepted ([architecture](architecture.md), ADR-0001 to ADR-0013).
- 2026-09-25: Vultr becomes the first provider and the E2E platform; ADR-0014 to ADR-0018 added, and
  `hack/vultr-spike` added.
- 2026-09-25: first Vultr spike run.
  - Verified: tag codec, markers, VPC masks and addressing, and the user_data size.
  - Inconclusive: SSH-dependent checks, which need spike v2.
- 2026-09-25: spike v2 runs. All provisional items of ADR-0018 are resolved except Object Storage conditional writes.
  - Verified: firewall scope and host firewall, scrubbing, halt, 64 KiB user_data, VPC traffic and alias IPs.
  - Boot takes about 2.5 minutes from create, not 6–15.
- 2026-09-25: the maintainer decided all six open questions; ADR-0019 adds the `combined` role.
- 2026-09-26: the M0 skeleton landed (ADR-0020, ADR-0021); CI is green on its first run. The roadmap's work items moved
  to GitHub milestones and issues.
- 2026-09-27: M0 Foundation is complete. The API types, the JSON Schema (ADR-0022), the state store with cluster
  locks and the spec commands of the CLI are built, and `cmd/tent/exit_test.go` checks the round trip, the field-path
  errors and the serialized changes. Renovate updates the Go modules and GitHub Actions (#9), and the archives and
  packages ship third-party licence notices, with a licence check in CI (#11).
- 2026-09-28: M1 Vultr infrastructure is complete. The engine, the Vultr provider, `tent update cluster` and
  `tent delete cluster` are built; the integration tests on the Vultr fake and a run on a real account met the exit
  criteria.

[project]: https://github.com/users/ingvarch/projects/2
[later]: https://github.com/ingvarch/tent/issues?q=is%3Aissue%20label%3Alater
[i9]: https://github.com/ingvarch/tent/issues/9
[i10]: https://github.com/ingvarch/tent/issues/10
[i11]: https://github.com/ingvarch/tent/issues/11
