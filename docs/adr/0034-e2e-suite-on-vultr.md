# ADR-0034: The E2E suite on Vultr

- **Status:** Accepted
- **Date:** 2026-10-07
- **Deciders:** ingvarch
- **Related:** amends [ADR-0012](0012-testing-strategy.md), [ADR-0014](0014-vultr-first-provider-and-e2e.md),
  [ADR-0018](0018-vultr-provider-design.md), [ADR-0021](0021-import-rules.md) and
  [ADR-0031](0031-bootstrap-in-update.md) (see their Status lines); builds on
  [ADR-0032](0032-joined-label-scrub-and-delete-guard.md) and [ADR-0033](0033-operator-commands.md);
  [architecture §5](../architecture.md#5-repository-layout-and-dependency-rules),
  [§15](../architecture.md#15-testing), [§17](../architecture.md#17-risks),
  [platform notes §1.2](../platform-notes.md#12-features-tent-relies-on),
  [§1.6](../platform-notes.md#16-the-agent-on-a-node),
  [§3.1](../platform-notes.md#31-api-basics-and-access-control)

## Context

[ADR-0014](0014-vultr-first-provider-and-e2e.md) chose Vultr as the E2E platform and set rules for it: a janitor, a
fallback region, a dedicated account or a service user with minimal rights, a run of at most 60 minutes, serialized
runs and CI. M2.9 builds the suite, and the rules meet the facts of 2026-10-06 and 2026-10-07:

- **Where it runs.** The service user's key works only from two addresses of the maintainer (decided on 2026-10-06). A
  GitHub runner cannot use it.
- **How E2E clusters are told apart.** `internal/cloud` declared the labels `tent/e2e` and `tent/e2e-run`, and nothing
  set them. The maintainer decided on 2026-10-07 that a name prefix does the job.
- **Lint.** Issue #97 expected exemptions from `providers-only-in-cmd` for E2E helpers and a Go janitor that import a
  provider. The maintainer decided on 2026-10-07 for a rule that keeps the suite away from tent's internals instead.
- **Versions.** A node refuses a tent-node of another version than its tent's; the first run built the two from
  different builds and both clusters failed ([platform notes §1.6](../platform-notes.md#16-the-agent-on-a-node)).
- **The exit of M2** ([roadmap](../roadmap.md)): E2E `smoke` passes on Vultr, on ubuntu-24.04 and ubuntu-26.04, and
  checks the four criteria that the table below maps to steps.

## Decision

### Where and how it runs

1. **The suite runs from the maintainer's machine with `make e2e`.** It is not in `make check` and not in CI. Only its
   unit tests, which need no cloud and carry no `e2e` build tag, run in `make check`.
2. **`make e2e` runs `make dev-upload` first,** so tent and the uploaded tent-node come from one build and carry one
   version. It then runs `go test -tags e2e -count=1 -v -timeout 90m ./test/e2e` with `TENT_NODE_URL`,
   `TENT_NODE_SHA256` and `E2E_TENT` (the absolute path of `bin/tent`) set. `TestMain` stops at once when
   `TENT_NODE_SHA256` is not the sha256 of the `tent-node_linux_amd64` next to `E2E_TENT`.
3. **The `go test` timeout is 90 minutes.** The limits of the steps add up to more than an hour; a run that passes
   takes 6 to 7 minutes (`r7l48w`, `58sglh`, `g57k85`). A `go test` that times out runs no cleanup, so the timeout
   must be longer than any run can last. This replaces ADR-0014's limit of 60 minutes. ADR-0014's rule that runs are
   serialized is dropped too: nothing enforces it.
4. **The region is chosen by hand:** `E2E_REGION`, default `ams`. The automatic fallback to `fra` or `lhr` that
   ADR-0014 asked for waits until deploy incidents make it worth its code.
5. **The service user has broad rights and an address allow-list.** ADR-0014 asked for minimal rights, but IAM policies
   have no tag conditions, so a policy cannot limit the key to tent's objects; the allow-list limits where the key
   works ([platform notes §3.1](../platform-notes.md#31-api-basics-and-access-control)).

### What marks an E2E object

6. **A cluster of the suite is named `e2e-<run>-<image digits>`** (24.04 gives 2404), such as `e2e-k3x9qz-2404`. The
   labels `tent/e2e` and `tent/e2e-run` are gone from `internal/cloud` and from the architecture's label table.

### The janitor

7. **The janitor is `test/e2e/janitor`, run by `go run ./hack/e2e-janitor` and by `TestMain` before a run creates
   anything.** It reads tent's own tags and markers with a reader of its own:
   - an instance by its tag `tent/cluster=`;
   - a VPC or a firewall group by the field `cluster=` of a `tent:` marker in its description;
   - an SSH key by that marker in its name.
8. **An object belongs to E2E when its cluster's name starts with `e2e-`.** Objects with no marker, such as the
   account's own SSH key `main`, are left alone, and so are those of another tool.
9. **It deletes whole clusters, by age.** A cluster is left over when its oldest E2E object is older than 3 hours
   (`--older-than`); all its objects go, younger ones too. A run in progress is younger, so it is safe.
10. **It deletes instances first, then firewall groups, VPCs and SSH keys, and repeats a refused VPC delete,** because
    Vultr refused a VPC delete for up to 73 s after the instances were gone
    ([platform notes §3.5](../platform-notes.md#35-vpc)). Without `--yes` it deletes nothing. `TestMain` sweeps with
    3 hours, and a failed sweep fails the run.

### The suite is a black box

11. **The suite imports none of tent's internal packages.** It runs the `tent` binary, reads Nomad over mTLS with the
    files of `tent export nomad`, reaches the nodes with `ssh`, and talks to Vultr through its own client
    (`test/e2e/vultrapi`, plain `net/http`). The depguard rule `e2e-black-box` denies
    `github.com/ingvarch/tent/internal` to `test/e2e/**` and `hack/e2e-janitor/**`. The existing rules keep govultr
    and the Nomad modules out. Only tests import `test/e2e/janitor/janitortest`.

### The smoke scenario

12. **One cluster per image, the images in parallel,** each with 1 server and 1 client, so 4 machines. The checks of
    ADR-0012's `security` scenario run in `smoke`, as the exit of M2 asks. The images are `ubuntu-24.04` and
    `ubuntu-26.04` unless `E2E_IMAGES` says otherwise. The steps run in order, and the first that
    fails stops the cluster's steps. A cleanup deletes the cluster unless `E2E_KEEP` is set. The details of each step
    are in [`test/e2e/README.md`](../../test/e2e/README.md).

    | Step | Proves |
    |---|---|
    | create | tent builds a Nomad cluster from flags on a real cloud |
    | validate | the machines and Nomad agree with the specs |
    | export | an operator reaches the API with a short-lived certificate and token |
    | service | criterion 1: a docker job with a service runs |
    | metadata | criterion 3: containers cannot reach the metadata endpoint, with a control request that shows the network works |
    | intro token | criterion 4: a client without an intro token is rejected |
    | validate again | the checks left the cluster valid |
    | delete | tent deletes the cluster |
    | leftovers | criterion 2: `delete` leaves nothing behind |

### What the runs changed in tent

13. **The Nomad step of `update` waits until Nomad's keyring has an active key,** after the health wait and before the
    scrubs, the mark and any intro token. Nomad makes the keyring's first key after it elects a leader and refuses to
    sign an intro token until then; with one server, run `4bjbp8` asked in that gap and failed
    ([platform notes §1.2](../platform-notes.md#12-features-tent-relies-on)). This amends the order of the Nomad step
    in [ADR-0031](0031-bootstrap-in-update.md).

## Consequences

### Positive

- The suite checks the four exit criteria of M2 on a real cloud, on both Ubuntu images.
- tent carries no label that exists only for tests, and the suite cannot depend on tent's internals by accident.
- A cluster that a failed run leaves (its own cleanup failed, or `go test` timed out) is deleted by the next run's
  `TestMain` or by the janitor command once it is older than 3 hours.

### Negative / trade-offs

- A run needs the maintainer's machine, the Vultr key, the R2 keys and `TENT_DEV_S3_URL`. Only the maintainer can run
  it, and nothing runs it on a schedule.
- The janitor leaves a cluster that failed its own delete for at least 3 hours, until the next run or a run of the
  janitor command, and it reads names, tags and markers only. An object that tent made with another name prefix is
  never touched, which is the intent.
- The key's rights are broader than the suite needs, and the allow-list is what limits them.
- A run makes 2 VPCs and 4 machines. Vultr allows 5 VPCs per region, so a region holds two runs at a time, kept and
  leftover clusters included; a third fails at `create`. The account's own VPCs `live_vpc` and `dev_vpc` in `ams` were
  deleted at the maintainer's request on 2026-10-07 to leave that room. The instance limit of 20, which Vultr raised
  from 5 at the maintainer's request ([platform notes §3.1](../platform-notes.md#31-api-basics-and-access-control)),
  would allow five runs.
- `make e2e` uploads a tent-node to R2 each time.
- A deploy incident in the region fails the run.

### Follow-ups

- Nightly runs and runs on a PR label, when a runner can reach the account.
- The automatic fallback region, and a policy for the service user that filters by tag, when IAM can do that.
- The scenarios `ha` and `upgrade` ([architecture §15](../architecture.md#15-testing)).

## Alternatives considered

- **tent sets `tent/e2e` and `tent/e2e-run` from an environment variable.** It puts a test feature into the code that
  users run. Vultr's VPCs, firewall groups and SSH keys have no tags, so the labels would need a second encoding in
  descriptions. A name prefix needs no code in tent.
- **A janitor through `internal/cloud/vultr`, with lint exemptions.** It shares tent's codec, so a wrong reading in
  tent is wrong in the janitor too, and it needs lint exemptions.
- **CI on GitHub runners.** A runner's address is not in the key's allow-list.
