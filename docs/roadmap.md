# Roadmap

> **Current status (2026-10-08):** M0 Foundation, M1 Vultr infrastructure and M2 Nomad bootstrap are complete. M3 Day-2
> operations is in progress.
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
> [§13.7](architecture.md#137-tent-delete-cluster---yes)). Until M2.7a the nodes were empty machines without Nomad.
>
> - Vultr is the first provider and the E2E platform ([ADR-0014](adr/0014-vultr-first-provider-and-e2e.md)).
> - Hetzner Cloud is second.
>
> M2 is complete (2026-10-07). M2.1 to M2.6b built the PKI and secrets, the channels and assets, NodeConfig, the
> Nomad client, tent-node and its phases. M2.7a made `tent update cluster --yes` build a Nomad cluster: servers,
> the ACL bootstrap and clients with intro tokens ([ADR-0031](adr/0031-bootstrap-in-update.md)). M2.7b scrubs the user
> data of each node that has joined and labels its machine, replaces a client that never registered, and refuses to
> delete a node that joined ([ADR-0032](adr/0032-joined-label-scrub-and-delete-guard.md)). Its real-cloud check ran on
> Vultr on 2026-10-06 and found two things, now fixed. M2.8 added the operator commands
> ([ADR-0033](adr/0033-operator-commands.md)): `tent export nomad` writes a short-lived certificate and a management
> token and prints the lines that point the `nomad` CLI at them, `tent ui` serves the Nomad UI and API on a loopback
> port, `tent validate cluster` compares the machines and Nomad with the specs and exits with 2 while they differ, and
> a combined cluster gets a warning. `hack/tent-operator` is gone. Its real-cloud check ran on Vultr on 2026-10-07.
> M2.9 added the E2E suite on Vultr ([ADR-0034](adr/0034-e2e-suite-on-vultr.md)), which `make e2e` runs from the
> maintainer's machine. Its runs found four faults, all fixed, and on 2026-10-07 `smoke` passed on ubuntu-24.04 and
> ubuntu-26.04: M2 is done.
>
> M3 is in progress, in parts M3.1 to M3.9. M3.1 built the rollout decisions
> ([ADR-0035](adr/0035-rollout-decisions.md)): `internal/rollout` returns the next step of a rolling update or of a
> removal of nodes from what the cloud and Nomad report, and a simulator proves it from every state a roll passes
> through. It also added the `rollingUpdate` settings of a node group. Nothing calls the decisions yet, and nothing
> of M3 runs against a cloud: M3.3 wires them into `tent rolling-update cluster`.
>
> **Work items live in GitHub:** each milestone below links to its GitHub milestone, and the
> [tent roadmap project][project] shows the open issues. This file keeps the goals and exit criteria; close issues as
> they land, and add a line to the log when a milestone completes.

Each milestone ends in a state that can be demonstrated. A milestone is done when its **exit criteria** hold in CI
and, from M2 on, in the E2E suite on Vultr.

## Maintainer decisions

These come from [architecture §18](architecture.md#18-open-questions). The first six were decided on 2026-09-25, the
seventh to the fifteenth on 2026-09-28, the sixteenth to the twenty-first on 2026-09-29, the twenty-second to the
twenty-fourth on 2026-09-30, the twenty-fifth to the twenty-seventh on 2026-10-05, and the twenty-eighth to the
thirty-second on 2026-10-06, and the thirty-third to the thirty-fifth on 2026-10-07. The maintainer extended the
twenty-third on 2026-10-02.

| # | Question | Decision |
|---|---|---|
| 1 | Label prefix and API group | `tent/` and `tent/v1alpha1` |
| 2 | Default `access.api` | `[0.0.0.0/0]` with mTLS + ACL, and a loud warning while it is open, from `validate cluster` and from the commands that change a cluster ([ADR-0033](adr/0033-operator-commands.md)) |
| 3 | Combined server+client mode | allowed for dev and small clusters through the `combined` role ([ADR-0019](adr/0019-combined-server-client-role.md)), with a warning from `validate cluster` and from the commands that change a cluster ([ADR-0033](adr/0033-operator-commands.md)) |
| 4 | Default OS image | `ubuntu-24.04`; E2E also runs on `ubuntu-26.04` |
| 5 | Consul and Vault | out of v1 |
| 6 | Licence of tent | Apache-2.0 |
| 7 | A cluster's provider and region | never change; a cluster moves by creating a new one |
| 8 | CA validity | 10 years, until CA rotation exists |
| 9 | Nomad's sha256s | checked at run time against the signed `SHA256SUMS`, with HashiCorp's key embedded in tent ([ADR-0026](adr/0026-channels-and-release-assets.md)) |
| 10 | Nomad version of a spec without one | the channel's recommended one, pinned in `cluster.completed.yaml` by the first `update`; any release from the channel's minimum up to the next major is allowed, and untested ones get a warning |
| 11 | What a channel holds | Nomad and the CNI plugins only; images stay in the provider's table and the API default |
| 12 | When NodeConfig reaches `update` | in M2.7, with tent-node, intro tokens and the bootstrap; M2.7a built it, with the asset downloads, the development-variable warning and the Nomad pin before the first node; M2.7b scrubs a node's user data once the node has joined ([ADR-0027](adr/0027-nodeconfig-contract-rendering-and-spec-hash.md), [ADR-0031](adr/0031-bootstrap-in-update.md), [ADR-0032](adr/0032-joined-label-scrub-and-delete-guard.md)) |
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
| 24 | The secrets of the tent-node VM check | `hack/tent-node-userdata` makes a throwaway CA, node certificate and gossip key per run; they sit in the user data of a VM that is deleted after the check, and tent scrubs nothing there, since no cluster owns the VM |
| 25 | Draining at shutdown | clients do not drain themselves when Nomad stops, since Nomad's self-drain leaves them ineligible after their next start; tent's own removals drain a client through the Nomad API ([ADR-0030](adr/0030-nomad-on-nodes.md)) |
| 26 | `leave_on_terminate` by role | server and combined agents set it to `false`, so a stopped server stays a Raft peer, and clients keep `true`; tent removes servers through the Nomad API on every provider ([ADR-0031](adr/0031-bootstrap-in-update.md)) |
| 27 | Deleting nodes that joined | until M3, `update` refuses to delete a node that joined Nomad: a machine with the joined label, a registered client or a server of the Raft configuration; a Nomad node that is `down` does not count as registered (2026-10-06); `delete cluster` works as before; built in M2.7b ([ADR-0032](adr/0032-joined-label-scrub-and-delete-guard.md)) |
| 28 | A client that never registered | `update` deletes a client that did not register before its intro token expired and creates it again, shown as `not registered`; nothing is drained, since it never ran a workload; the delete comes before the create, an exception to creating the replacement first (2026-10-06; [ADR-0032](adr/0032-joined-label-scrub-and-delete-guard.md)) |
| 29 | `tent ui` and the token | the proxy adds the token and mTLS; it listens on a loopback address only, and a request with a foreign `Host` or `Origin` gets 403; every local process that can reach the port acts with a management token while it runs ([ADR-0033](adr/0033-operator-commands.md)) |
| 30 | The exported token | a management token with a TTL, 24 hours by default; tent owns no ACL policy yet ([ADR-0033](adr/0033-operator-commands.md)) |
| 31 | Where `export nomad` writes | `$XDG_CACHE_HOME/tent/<cluster>`, else `~/.cache/tent/<cluster>`; the directory has mode 0700, the files 0600; `--dir` changes the place ([ADR-0033](adr/0033-operator-commands.md)) |
| 32 | Exit codes of `validate cluster` | 2 when the cluster is not valid, with no `Error:` line; 1 when tent could not check ([ADR-0033](adr/0033-operator-commands.md)) |
| 33 | `rollingUpdate` of a node group | `maxSurge` (default 1), `maxUnavailable` (default 0) and `drainTimeout` (default `1h`, Nomad's drain deadline); the first two apply to client groups, and server and combined groups roll one node at a time with one more node first; the settings never change the spec hash; built in M3.1 (2026-10-07; [ADR-0035](adr/0035-rollout-decisions.md)) |
| 34 | Smaller server groups | `update` may shrink a server group one server at a time: leadership moved away, the server stopped, its peer removed through the API, autopilot healthy before the next; refused when the cluster is unhealthy or quorum would be lost; one server needs `--allow-single-server`; clients are drained before a scale-down; lifts decision 27's guard where tent drains or removes safely (2026-10-07; [ADR-0035](adr/0035-rollout-decisions.md)); the decisions are built in M3.1, with two voters and single servers refused until the maintainer chooses |
| 35 | `rolling-update` is its own command | `update` never replaces a node and reports how many are outdated (2026-10-07; [ADR-0035](adr/0035-rollout-decisions.md)) |

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

**Status:** done. The E2E `smoke` ([ADR-0034](adr/0034-e2e-suite-on-vultr.md),
[architecture §15](architecture.md#15-testing)) passed on Vultr on 2026-10-07 on ubuntu-24.04 and ubuntu-26.04, in runs
`r7l48w`, `58sglh` and `g57k85` ([platform notes §3.16](platform-notes.md#316-spike-runs)):
- a docker job with a service ran, and its Nomad health check passed;
- after `tent delete cluster --yes` the Vultr API listed nothing of the cluster, and its state store held no cluster;
- containers on Nomad's bridge, on Docker's bridge and on the host network got no answer from `169.254.169.254`, while
  a control site answered;
- the servers refused a second client agent that had no intro token, and logged `node registration without
  introduction token: enforcement_level=strict`.

## M3 Day-2 operations

[Issues](https://github.com/ingvarch/tent/milestone/4)

**Goal:** Nomad-aware rolling updates, scaling, upgrades and backups.

**Exit criteria:** E2E `ha` and `upgrade` pass on Vultr, with no quorum loss and no job downtime beyond the configured
migrations.

**Status:** in progress; see the note at the top of this file.

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
- 2026-10-06: M2.7b is built. `update` scrubs the user data of each node that has joined and labels its machine
  `tent/joined=true`, replaces a client that never registered, and refuses to delete a node that joined (decisions 27
  and 28, [ADR-0032](adr/0032-joined-label-scrub-and-delete-guard.md)). The real-cloud check ran on Vultr
  (runs `rugw2m`, `sv3vwb` and `rgfckj`, [platform notes §3.16](platform-notes.md#316-spike-runs)). It found two
  things, both fixed in the code: cloud-init read the stub as degraded, and the account's instance limit refused the
  replacement of a client. Closes #93.
- 2026-10-07: M2.8 is built. `tent export nomad`, `tent ui` and `tent validate cluster` exist, a combined cluster gets
  a warning, and `hack/tent-operator` is removed (decisions 29 to 32, [ADR-0033](adr/0033-operator-commands.md)). The
  real-cloud check ran on Vultr the same day (spike v12, run `9pxbqn`,
  [platform notes §3.16](platform-notes.md#316-spike-runs)): the three commands worked against a cluster that tent
  built, and the one unexpected row was the script's own fault. Closes #94 and #98.
- 2026-10-07: M2.9 is built and M2 is complete. `make e2e` runs the E2E suite on Vultr from the maintainer's machine;
  a janitor deletes what earlier runs left ([ADR-0034](adr/0034-e2e-suite-on-vultr.md)). The runs found four faults,
  all fixed: a node refuses a tent-node of another version, the Nomad step did not wait for Nomad's keyring before the
  first intro token, the suite's `validate` lacked `--allow-single-server`, and the metadata probe depended on one
  control site. `smoke` then passed three times on both
  images (runs `r7l48w`, `58sglh` and `g57k85`). Closes #96, #97 and #64.
- 2026-10-08: M3.1 is built. `internal/rollout` decides the steps of rolling updates and of removals of nodes as pure
  functions, and a simulator with golden step sequences, invariants and a resume test proves them. A node group gets
  the `rollingUpdate` settings (decision 33); decisions 33 to 35 are recorded with
  [ADR-0035](adr/0035-rollout-decisions.md). Closes #99.

[project]: https://github.com/users/ingvarch/projects/2
[later]: https://github.com/ingvarch/tent/issues?q=is%3Aissue%20label%3Alater
[i9]: https://github.com/ingvarch/tent/issues/9
[i10]: https://github.com/ingvarch/tent/issues/10
[i11]: https://github.com/ingvarch/tent/issues/11
