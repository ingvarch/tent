# ADR-0028: tent-node agent, units and delivery

- **Status:** Accepted; amended by [ADR-0029](0029-host-firewall-runtime-and-cni-on-nodes.md) (`ExecRunner` stops a
  program's process group; the Vultr client marks its socket; no tent unit is ordered on `cloud-init-main.service`
  either; `systemctl enable` asks PID 1 to reload by itself, and item 11 is about `install`'s own reload;
  `hostfirewall`, `runtime` and `cni` are built, and the other M2.6 follow-ups move to M2.6b) and by
  [ADR-0030](0030-nomad-on-nodes.md) (items 1 and 3: `join`, `nomad`, `verify` with Nomad and `refresh-join` are
  built, and no phase is a stub; `up` and `refresh-join` take a lock on `/run/tent-node.lock`, and `refresh-join` ends
  within 4m45s; item 2: `FS` gains `Open`, `HasContent` and `WriteStream`, and `WriteFile` and `WriteStream` first
  remove the temporary files that a killed write left; `Host` gains a dialer for the Nomad API (`DialContext`), and
  `nodeuptest` zips, an mTLS server, a fake Nomad agent and a hanging dialer; `nomad.service` keeps systemd's stop
  timeout, since clients do not drain at shutdown; the other M2.6 follow-ups are done)
- **Date:** 2026-09-29
- **Deciders:** ingvarch
- **Related:** amends [ADR-0006](0006-two-binaries-and-nodeconfig.md) (the units, the provider on the node, where
  development builds live), [ADR-0020](0020-release-channels-and-ci-conventions.md) (how tent-node joins the release,
  the notices), [ADR-0021](0021-import-rules.md) (tent-node's allow list, the `go list` test and
  `s3url-stdlib-and-aws`, in the manner of [ADR-0025](0025-stdlib-only-helper-packages.md)),
  [ADR-0026](0026-channels-and-release-assets.md) (the release file names, development builds on amd64) and
  [ADR-0027](0027-nodeconfig-contract-rendering-and-spec-hash.md) (NodeConfig's `provider`, its M2.5 follow-ups);
  [ADR-0012](0012-testing-strategy.md), [ADR-0013](0013-technology-stack.md),
  [ADR-0016](0016-server-discovery-seed-and-refresh.md), [ADR-0018](0018-vultr-provider-design.md),
  [architecture §5](../architecture.md#5-repository-layout-and-dependency-rules),
  [§7.1](../architecture.md#71-interfaces), [§8.1](../architecture.md#81-bootstrap-chain),
  [§8.2](../architecture.md#82-tent-node-phases), [§8.5](../architecture.md#85-artifacts-and-verification),
  [§16](../architecture.md#16-technology-stack-and-releases), [§18](../architecture.md#18-open-questions),
  [Appendix B](../architecture.md#appendix-b-cloud-init-user-data-sketch),
  [platform notes §3.4](../platform-notes.md#34-user_data-metadata-and-identity)

## Context

M2.5 builds `tent-node`, the agent that cloud-init starts on every node
([ADR-0006](0006-two-binaries-and-nodeconfig.md)), and puts it into the release. M2.6 adds the phases that install
Docker, the CNI plugins and Nomad. Several things had to be settled first:

- **Tests.** The phases change a Linux machine as root. CI runs the tests on Linux, macOS and Windows, without root
  and without systemd.
- **The handover from cloud-init.** The user data runs `tent-node install` through `exec` inside `runcmd`, which
  cloud-init runs in `cloud-final.service` ([Appendix B](../architecture.md#appendix-b-cloud-init-user-data-sketch)).
  systemd facts that constrain the units:
  - A start job of a unit ordered after `cloud-final.service` or `cloud-init.target` waits for cloud-final, which
    waits for `install`: the first boot hangs.
  - `WantedBy=multi-user.target` orders the target after the unit, so an order of the unit after
    `multi-user.target` is a cycle.
  - On Ubuntu, cloud-init's own unit orders `cloud-final.service` after `multi-user.target`; the M2.5 VM check
    confirmed it on Vultr's Ubuntu 24.04 and 26.04 images on 2026-09-29.
  - A oneshot service has no start timeout unless it sets one.
  - Appendix B ordered `tent-node.service` `Before=nomad.service`. If `up` starts Nomad and waits for it, the start
    job of `nomad.service` waits for `tent-node.service`, which waits for `up`: a deadlock until a timeout.
- **Idempotency.** ADR-0006 restarts Nomad only when its files changed. systemd reads a changed unit file only after
  `daemon-reload`.
- **The metadata service.** Each cloud has its own. tent-node must know which one to ask, and on Vultr the document
  that it reads also carries the user data
  ([platform notes §3.4](../platform-notes.md#34-user_data-metadata-and-identity)).
- **The release.** [ADR-0020](0020-release-channels-and-ci-conventions.md) planned tent-node as raw linux binaries.
  The licences of what tent links require their text in every copy, and a bare binary has no room for it.
- **Development builds.** Their nodes download tent-node from a URL that the VMs can reach
  ([ADR-0026](0026-channels-and-release-assets.md)). CI already has an R2 bucket for the state store tests. A presigned
  S3 URL is valid for at most 7 days.
- **Imports.** depguard checks the direct imports of each file, so a package that tent-node reaches through another
  one escapes it ([ADR-0021](0021-import-rules.md)).

## Decision

### The agent

1. **Commands.** `cmd/tent-node` has `install`, `up`, `refresh-join` and `version`. It parses its flags with the
   standard `flag` package, not cobra, to keep its dependencies and notices small.
   - `install`, `up` and `refresh-join` take only `--config`, the path of the NodeConfig: `/etc/tent/node.json` by
     default (`nodeconfig.ConfigPath`), absolute and clean.
   - Exit codes: 0 on success, 1 when the command fails, 2 for a wrong command line.
   - It logs text through `log/slog` to stderr, which systemd puts into the journal. SIGTERM and Ctrl-C cancel the
     command's context. No output holds a secret.
   - `refresh-join` is a stub in M2.5: it logs that it is not built and changes nothing.
2. **The runner.** Phases reach the machine only through `nodeup.Host`: a filesystem (`FS`), a program runner
   (`Runner`), a systemd helper over the runner, and facts: the host name, the effective user id, `GOOS` and `GOARCH`,
   tent-node's version and a clock.
   - `FS.WriteFile(path, data, mode, owner)` reports whether it changed anything. It does nothing when the file
     already has the data, mode and owner. Otherwise it writes a temporary file beside the old one, flushes it,
     renames it over the old one and flushes the directory. `EnsureDir` also fixes the mode and owner of a directory
     that exists, and refuses a symbolic link. Errors never show a file's data.
   - `OSFS` is the disk, under an optional root for tests. On Unix it sets and compares modes and owners; elsewhere it
     does neither, so the tests run on macOS and Windows.
   - `ExecRunner` returns a program's standard output. A program that exits with another status than 0 gives an
     `ExitError` with the status and the last 1 KiB of its standard error. When the context ends, it stops the
     program and waits up to 10 seconds for its output to close. Arguments never hold secrets, since errors and logs
     show them.
   - `internal/nodeup/nodeuptest` holds the fakes, for tests only: an in-memory filesystem that behaves as `OSFS` does
     on Linux and records what changed, a runner that answers commands from a script and records them, a fake Ubuntu
     24.04 whose systemd and `timedatectl` keep state, a host and a metadata service.
3. **Phases.** `nodeup.Up` runs the phases in the order of
   [architecture §8.2](../architecture.md#82-tent-node-phases) and stops at the first that fails. Each phase ends
   `done`, `unchanged`, `skipped` or `failed`, with a reason where one helps. Phases after a failure are skipped with
   the reason `not run: <phase> failed`, and once the context has ended the next phase does not start. Whatever
   happened, `Up` writes `/var/lib/tent/status.json` (directory 0700, file 0600, root only) on every run: tent-node's
   version, the spec hash, the start and end times, the instance (id, zone, private IP) and each phase's result.
   M2.5 builds three phases:
   - `preflight` checks linux on amd64 or arm64, root, that systemd runs the machine, and Ubuntu, with a warning as the
     reason outside 24.04 and 26.04. The host name must be NodeConfig's `name` (maintainer decision 13), and
     tent-node's version that of NodeConfig's `tent-node` asset. Then it reads the metadata service, for at most
     3 minutes. It changes nothing.
   - `system` writes the kernel modules to `/etc/modules-load.d/tent.conf` and loads them with `modprobe`; writes
     the sysctls to `/etc/sysctl.d/99-tent.conf` and applies that file with `sysctl -p`; turns on NTP with
     `timedatectl set-ntp true` where timedatectl can, and elsewhere requires `chrony.service` or
     `systemd-timesyncd.service` to be active; and limits the journal to 1 GiB with a drop-in, restarting journald.
     A command runs only when its file changed. When it fails, the file goes, so that the next run tries again. A
     node without modules or sysctls, such as a server, gets no file for them. `system` does not set the host name.
   - `verify` checks that `tent-node.service` and `tent-node-join.timer` are enabled.
   - `hostfirewall`, `runtime`, `cni`, `join` and `nomad` are stubs until M2.6, skipped with `not built yet`.
4. **Every boot runs every phase.** Each compares the machine with NodeConfig and acts only on a difference, so a
   second run changes nothing but `status.json`. M2.6 downloads nothing when a checked file is already in place.
   A run killed between a file and its command leaves the command undone until the next boot, when systemd reads the
   modules, sysctl and journald files itself.
5. **The units.** `nodeup.RenderUnits` renders three units into `/etc/systemd/system`, mode 0644, owned by
   `root:root`. tent-node is a oneshot service plus a timer, not a long-lived daemon
   ([ADR-0016](0016-server-discovery-seed-and-refresh.md)):
   - `tent-node.service`: `Type=oneshot`, `RemainAfterExit=yes`, `Wants=` and `After=network-online.target` and no
     other ordering, runs `tent-node up`, `TimeoutStartSec=45min`, `KillMode=mixed`, `WantedBy=multi-user.target`.
   - `tent-node-join.service`: `Type=oneshot`, `After=tent-node.service`, runs `tent-node refresh-join`,
     `TimeoutStartSec=5min`. The timer starts it; it has no `[Install]` section.
   - `tent-node-join.timer`: `OnBootSec` and `OnUnitActiveSec` set to NodeConfig's `join.refreshInterval`,
     `AccuracySec=1s`, `WantedBy=timers.target`.

### The handover (maintainer decision 19)

6. **`install` waits for `up`.** It writes the units, has systemd read them again when one needs it, enables
   `tent-node.service` and the timer, starts `tent-node.service` and waits until `up` has run, then starts the timer.
   It enables and starts only what is not enabled or active, so a second `install` only reads. When `up` fails,
   `install` fails, cloud-init reports the error, and the timer starts at the next boot.
7. **No unit waits for cloud-init, for `multi-user.target` or for Nomad.** No unit has `After=`, `Requires=`,
   `Wants=`, `Requisite=` or `BindsTo=` on `cloud-final.service`, `cloud-init.target`, `cloud-config.service` or
   `multi-user.target`, and none has `Before=nomad.service`. An order after cloud-final or `cloud-init.target` would
   hang the first boot and one after `multi-user.target` would be a cycle; `cloud-config.service` is left out as a
   precaution. A unit test checks the rendered units.
8. **Finite timeouts.** 45 minutes for `up` and 5 for a refresh. A hung `up` fails its unit and `install` instead of
   holding the boot for ever.
9. **The reboot.** `tent-node.service` starts with `multi-user.target`, which waits until `up` has run. cloud-final is
   ordered after `multi-user.target`, so it waits too, and `cloud-init status --wait` returns after `up`.
10. **Nomad (M2.6).** `up` starts Nomad itself and `verify` checks it without waiting for a leader: a leader needs
    other servers, which may boot later, and a wait for one would hold each server's boot until a quorum forms. tent
    renders `nomad.service` as a NodeConfig file, so it is in the spec hash, and it has no `After=` or `Requires=` on
    `tent-node.service`.
11. **daemon-reload only when systemd asks for it.** `install` runs `systemctl daemon-reload` only when
    `systemctl show -p NeedDaemonReload` says yes for one of its units. systemd reads a unit it has not read before
    when first asked about it, so the first `install` needs no reload. Asking systemd, rather than trusting its own
    writes, lets the next run finish a run that stopped between a write and the reload.

### The environment (maintainer decision 20)

12. **One read, one snapshot.** `env.Environment` has one method, `Read(ctx) (Instance, error)`, which returns the
    instance id, the zone in lower case and the private IPv4 address. `preflight` calls it once per run and keeps the
    result in the host for the other phases and `status.json`. There is no `UserData`, which nothing reads, and no
    `MetadataEndpoint`: NodeConfig's `firewall.blockMetadata` is the one source of the address.
13. **Vultr** (`env/vultr`) reads `http://169.254.169.254/v1.json`:
    - never through a proxy, whatever the environment says, and without following redirects;
    - 5 seconds per try, the body included, and 1 second between tries, until the context ends;
    - it tries again after a failed connection, a try without a whole answer, a 429 or a 5xx other than 501, the
      set that tent's Vultr API client retries
      ([architecture §11.8](../architecture.md#118-api-client-rate-limits-cost));
    - any other answer fails at once, 404 included: cloud-init read the document earlier in the same boot, so waiting
      would not help;
    - it takes `instance-v2-id`, `region.regioncode` in lower case, and the IPv4 address of exactly one interface with
      `network-type: private`. None, two, an empty address or `0.0.0.0` fails. A node in several VPCs would need a
      choice by CIDR or VPC id;
    - no error holds the document, which carries the user data.
14. **NodeConfig names the provider.** NodeConfig gains `provider`, validated as one of the API's providers and left
    out of the spec hash, since a cluster's provider never changes (maintainer decision 7). `internal/app` sets it
    from the model. tent-node picks its environment by it; for a provider without one it fails with
    `tent-node does not support provider <name> yet`.
    - `Validate` also refuses a kernel module or a sysctl key that does not start with a letter or a digit:
      `modprobe` would read a leading dash as an option, and `sysctl.d` ignores the errors of a line whose key starts
      with one.

### The release

15. **A second build.** GoReleaser builds `tent-node` for linux on amd64 and arm64 with tent's flags and ldflags.
    - The binaries are released bare, named `tent-node_linux_amd64` and `tent-node_linux_arm64`, the names that
      `internal/assets` reads (`assets.TentNodeFile`). A test in `internal/buildconfig` renders the archive's name
      template and compares.
    - `checksums.txt` lists them and is signed keyless with cosign. The binaries get no signature of their own: nodes
      check the sha256 that tent read over TLS (ADR-0026).
    - Each tent-node binary gets an SBOM, as each archive does.
    - tent's archives, packages and cask take only the `tent` build, and only tent is notarized.
16. **One set of notices.** `THIRD_PARTY_NOTICES` covers what tent and tent-node link on every platform the release
    builds for, a little more than tent-node links on Linux. tent's archives and packages still carry it. It is also a
    file of the release, listed in `checksums.txt`, because the bare binaries have no room for it. `make licenses`
    and CI check the licences of both binaries.
17. **CI.** The release snapshot on every pull request fails when `checksums.txt` lacks either tent-node binary.
    `make build` also builds `bin/tent-node_linux_amd64`.

### Development builds (maintainer decision 18)

18. **The CI bucket, under `dev/`.** `hack/tent-node-upload` (`make dev-upload`) uploads a development build's
    tent-node to the CI R2 bucket at `dev/tent-node/<sha256>/tent-node_linux_<arch>`, skipping the upload when a HEAD
    finds the object. It presigns a GET for at most 7 days and prints `TENT_NODE_URL` and `TENT_NODE_SHA256` for fish
    or sh.
    - The maintainer's machine uploads with its own R2 token, not CI's.
    - A lifecycle rule deletes `dev/` objects after 8 days. An object older than a day is uploaded again when a new
      7-day URL needs it.
    - `make build` builds tent-node for amd64 only, and a development build gives every node that one URL, so its
      clusters need amd64 plans. On an arm64 plan the node fails at cloud-init with an exec format error.
    - `hack/tent-node-userdata` prints user data without secrets for a check of a development build on a real VM.
    - `TENT_DEV_S3_URL` has the form of a state store URL, and one package reads both: `internal/s3url`. `Parse`
      refuses any `@`, so no user or password gets through, and its errors never show the URL or a value of its
      query.
      `URL.Client` takes the credentials from the standard AWS chain, prefers the URL's region to the configuration's,
      and validates response checksums only where the API requires them, so a presigned download stays a plain GET.
      The state store's s3 backend uses it too, and its messages stay as they were.

### Imports

19. **The whole dependency tree.** A test in `internal/buildconfig` runs `go list -deps ./cmd/tent-node` for
    linux/amd64 and linux/arm64. It fails when tent-node links `internal/cloud`, `internal/assets`,
    `internal/channels`, `internal/statestore`, `internal/app`, `internal/nomadops`, govultr, hcloud-go,
    `github.com/aws`, `ProtonMail/go-crypto`, `github.com/hashicorp/nomad` or cobra, or any of their subpackages, and
    when it links a module other than tent's own and `golang.org/x/mod`, which `internal/buildinfo` uses.
20. **depguard.**
    - `tent-node-allowed`: code under `internal/nodeup` and `cmd/tent-node`, tests excepted, imports only the standard
      library, `internal/nodeup` and its subpackages, `internal/nodeconfig`, `internal/secret`, `internal/buildinfo`
      and `api/v1alpha1`.
    - `nodeup-no-cloud` covers `cmd/tent-node` too, tests included.
    - `internal/nodeup/nodeuptest` imports `testing`, so only tests import it.
    - `s3url-stdlib-and-aws`: `internal/s3url`, tests excepted, imports only the standard library, aws-sdk-go-v2's
      `aws`, `config` and `service/s3`, and smithy-go's `logging`.
    - `internal/s3url/s3urltest`, which keeps the developer's AWS configuration out of the tests of the state store,
      `internal/s3url` and `hack/tent-node-upload`, imports `testing`, so only tests import it.

## Consequences

### Positive

- The phases are tested on Linux, macOS and Windows, without root, systemd or a network.
- The ordering of tent-node's units cannot hang a boot, and a hung `up` ends within 45 minutes with a failed unit.
- cloud-init's result covers `up`: on the first boot `cloud-init status` fails when `up` fails.
- Every boot brings the machine back to its NodeConfig, and a second run changes nothing.
- tent-node asks exactly one metadata service, the one of the cluster's cloud.
- The binary's whole dependency tree is checked, not only each file's direct imports.
- The signed `checksums.txt` covers the tent-node binaries and the notices.

### Negative / trade-offs

- cloud-final on the first boot, and `multi-user.target` and cloud-final on every boot, wait for `up`: up to
  45 minutes when it hangs.
- When `up` fails on the first boot, the timer does not start until the next boot.
- Every boot reads the metadata service. When it does not answer within 3 minutes, `preflight` fails and the node
  does not come up.
- A node with two private interfaces fails `preflight`.
- The bare binaries carry no licence text; the notices are a separate file of the release and list a little more
  than tent-node links.
- Development clusters need amd64 plans.
- The development token can write the whole CI bucket, `ci/` included: R2 cannot limit a token to a prefix. Anyone
  who has a presigned URL can download that tent-node until the URL expires.
- `ExecRunner` does not kill a program's process group, so a child that holds the output delays a stopped command by
  up to 10 seconds, and a stopped `apt` or `dpkg` can leave its own children running.

### Follow-ups

- **The VM check (M2.5)** ran on 2026-09-29 on Vultr's Ubuntu 24.04 and 26.04 images. It confirmed node.json from
  the gz+b64 payload, the live metadata read, what `systemctl is-enabled` prints for a unit without a file, `CanNTP`
  and the time service, and the reboot path
  ([platform notes §3.4](../platform-notes.md#34-user_data-metadata-and-identity)).
- **M2.6.**
  - The five phases, and `refresh-join`, which the `go list` test keeps from linking `internal/nomadops` today.
  - `nomad.service` rendered by tent as a NodeConfig file: SIGTERM, a `TimeoutStopSec` above the drain deadline, and
    no `After=` or `Requires=` on `tent-node.service`. The ordering test extends to it.
  - `verify` checks Nomad without a leader. `/var/lib/nomad/client` is made with mode 0700 before the intro token.
  - A lock between `up` and `refresh-join`.
  - Before a phase runs `apt` or `dpkg`, `ExecRunner` stops a program by sending SIGTERM to its process group.
  - `nomad config validate` of the golden files, an M2.5 follow-up of ADR-0027 that M2.5 did not do.
  - A VM check that the first `install` runs no reload, which the M2.5 VM check did not record.
- **M4.** `env/hetzner`, with the Hetzner provider.

## Alternatives considered

- **A long-lived daemon.** It would run as root all the time and keep state across its own restarts. A oneshot at
  boot and a timer for the one periodic task, the join refresh, do the job, and nodes are replaced rather than
  repaired.
- **`Before=nomad.service`, with `up` starting Nomad through `systemctl start --no-block`.** It avoids the deadlock,
  but `up` could not tell whether Nomad started, and `verify` could not check it in the same run.
- **`install` not waiting for `up`.** cloud-init would report success before the node is set up, and a failed `up`
  would show only in the journal and `status.json`.
- **Detecting the provider from DMI**, as cloud-init does. The vendor strings are no contract, and tent knows the
  provider for certain; one validated field costs nothing.
- **A public bucket for development builds.** Anyone could list and download every build. A presigned URL opens one
  object for at most 7 days.
- **GitHub pre-releases for development builds.** Every build would need a tag and a release, which stay public until
  deleted and crowd the releases page.
- **A cosign signature per tent-node binary.** Nodes check sha256s and carry no cosign verifier, and tent does not
  check cosign signatures (ADR-0026). The signed `checksums.txt` already covers the binaries.
- **cobra for tent-node.** It would add a module, and its notices, to a binary that has four commands and one flag.
