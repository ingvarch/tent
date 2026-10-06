# ADR-0029: Host firewall, container runtime and CNI plugins on nodes

- **Status:** Accepted; amended by [ADR-0030](0030-nomad-on-nodes.md) (after a reboot `nomad` reports `done` too,
  since only `up` starts Nomad; decision 23: `hack/tent-node-userdata` builds a node's config through
  `app.NodeConfigOf`, `app.HostFirewall` is gone and `app.NodeSystem` is unexported, and the `tentnode` check is
  spike v8; decision 17: the cache checks a cached file as a stream (`FS.HasContent`), rewrites one with a wrong
  mode or owner from itself as a stream, hashes an old one as a stream before it removes it, and gives the phases the
  file's path; decision 19: `cni` reads its archive once with `ReadFile` and checks the sha256 of the bytes it
  unpacks, so a cache file changed after the check fails the phase; decision 13: on the M2.6b VM checks a restart of
  Docker with live-restore restarted Nomad's docker tasks in new containers
  ([platform notes §6.4](../platform-notes.md#64-restarts-of-docker-containerd-and-nomad), ADR-0030's trade-offs);
  the M2.6b follow-ups are built, and their VM check ran on 2026-10-05) and by
  [ADR-0032](0032-joined-label-scrub-and-delete-guard.md) (the user data is scrubbed once a node has joined its cluster,
  so workloads can read it only until then)
- **Date:** 2026-09-29
- **Deciders:** ingvarch
- **Related:** amends [ADR-0007](0007-security-baseline.md) and [ADR-0008](0008-node-credential-delivery.md) (how
  workloads are kept from the metadata service),
  [ADR-0027](0027-nodeconfig-contract-rendering-and-spec-hash.md) (constants for the asset names, rule and asset
  names as DNS labels, the bridge rules on client and combined nodes) and
  [ADR-0028](0028-tent-node-agent-units-and-delivery.md) (`ExecRunner` stops a process group, the Vultr client
  marks its socket, no tent unit is ordered on `cloud-init-main.service` either, the M2.6 follow-ups that M2.6a
  built, reloads of PID 1);
  [ADR-0006](0006-two-binaries-and-nodeconfig.md), [ADR-0019](0019-combined-server-client-role.md),
  [ADR-0026](0026-channels-and-release-assets.md), [architecture §7.1](../architecture.md#71-interfaces),
  [§8.2](../architecture.md#82-tent-node-phases), [§8.3](../architecture.md#83-nodeconfig-contract),
  [§8.5](../architecture.md#85-artifacts-and-verification),
  [§9.4](../architecture.md#94-secrets-on-nodes-threat-model), [§9.6](../architecture.md#96-network-perimeter),
  [§11.5](../architecture.md#115-firewall-and-host-firewall), [§18](../architecture.md#18-open-questions),
  [platform notes §1.2](../platform-notes.md#12-features-tent-relies-on),
  [§3.6](../platform-notes.md#36-firewall-groups), [§6](../platform-notes.md#6-ubuntu-on-nodes)

## Context

M2.6 builds the phases of `tent-node up` that [ADR-0028](0028-tent-node-agent-units-and-delivery.md) left as stubs,
in two parts: M2.6a sets the machine up before Nomad (`hostfirewall`, `runtime` and `cni`), and M2.6b adds `join`,
`nomad` and `refresh-join`. The facts that shaped M2.6a, with their sources in the platform notes:

- **The image's firewall.** Vultr's Ubuntu 24.04 image enables ufw, which blocks every port but 22, on the VPC too
  ([§3.6](../platform-notes.md#36-firewall-groups)). `ufw disable` also sets the policies of the iptables `filter`
  chains to accept, which undoes the drop that Docker puts on `FORWARD`. `nftables.service` is installed but not
  enabled, and its `/etc/nftables.conf` starts with `flush ruleset`, which would wipe Docker's and the CNI plugins'
  tables ([§6.2](../platform-notes.md#62-firewalls-on-the-host)).
- **Other tables.** Docker and Nomad's CNI plugins keep their rules in iptables-nft
  ([§1.2](../platform-notes.md#12-features-tent-relies-on)). In nftables a drop in any base chain is final and an
  accept is not, so a drop in tent's table would win over their accepts.
- **The metadata service** serves the user data, which holds the node's key and, on servers, the gossip key until the
  scrub ([architecture §9.4](../architecture.md#94-secrets-on-nodes-threat-model)). Containers on a bridge reach it
  through the forward chain, since it is routed out of the public NIC; host processes and containers with host
  networking reach it through the output chain. [ADR-0007](0007-security-baseline.md) and
  [ADR-0008](0008-node-credential-delivery.md) blocked it with nftables for forwarded traffic and non-root local
  processes, the first alternative below.
- **Packages.** Ubuntu's `docker.io` 29.1.3 is in 24.04's updates and in 26.04. Its postinst enables and starts
  Docker, and it ships no `/etc/docker` files. dpkg and apt-get take their locks without waiting, and apt-daily or
  unattended-upgrades may hold them on the first boot ([§6](../platform-notes.md#6-ubuntu-on-nodes)).
- **The CNI plugins** come as a gzip tar of about 50 MB without a signature, and NodeConfig carries its sha256
  ([ADR-0026](0026-channels-and-release-assets.md)). GitHub redirects release downloads to URLs that carry a signed
  token in their query.
- **Stopping a command.** `ExecRunner` killed only the program, so a stopped `apt-get` or `dpkg` could leave its
  children running (ADR-0028).

## Decision

### The metadata service (maintainer decision 21)

1. **Only tent-node's socket reaches it.** tent-node's metadata client sets `SO_MARK` to `0x747` (`env.MetadataMark`)
   on its socket before it connects. In tent's table the output chain accepts a packet to NodeConfig's
   `firewall.blockMetadata` only with exactly that mark and drops, with a counter, every other packet to it. The
   forward chain drops, with a counter, every packet to it.
   - The mark shares no bit with the marks of portmap (`0x2000`), kube-proxy (`0x4000`, `0x8000`), Tailscale
     (`0xff0000`) or Calico (`0xffff0000`). The rule matches the whole 32-bit value.
   - It stops root containers with host networking, exec and java tasks, Nomad's artifact fetcher and every process on
     the host, root included.
   - Setting the mark needs CAP_NET_ADMIN or CAP_NET_RAW. On Linux `env/vultr` marks each socket. A failed mark fails
     the try with `mark the socket for the metadata service: … (tent-node needs CAP_NET_ADMIN or CAP_NET_RAW)`, and
     `preflight` tries again until its 3 minutes run out.
2. **What can still reach it:** a workload with CAP_NET_ADMIN or CAP_NET_RAW, which can set the mark itself.
   CAP_NET_RAW alone is enough since Linux 5.17. So:
   - root `raw_exec` tasks and privileged containers, which are root on the node anyway;
   - a task that the operator gives NET_RAW through `allow_caps` or `cap_add`, for ping for example. Nomad gives
     workloads neither capability by default;
   - a container started with a plain `docker run` outside Nomad. Docker's default capabilities include NET_RAW, and
     Nomad's docker driver drops it by default.

### The host firewall

3. **Order.** `hostfirewall` runs three steps, so that the node is never without a host firewall on the first boot:
   1. **firewalld:** `systemctl disable --now firewalld.service` when it is active or enabled. It goes first, since its
      stop may touch other tables. Ubuntu's cloud images do not ship it.
   2. **tent's table:** make `/etc/tent` a directory with mode 0755, owned by `root:root`, which also resets an
      existing one; write `/etc/tent/firewall.nft` (0600, `root:root`); read the kernel's tables with
      `nft -j list tables`; and run `nft -f /etc/tent/firewall.nft` when there is no `inet tent` table or its comment
      differs from the file's.
   3. **ufw, last:** nothing when `ufw.service` is `not-found`. `ufw disable` runs only while `/etc/ufw/ufw.conf` says
      `ENABLED=yes` (the file's last `ENABLED` line, quotes removed, compared case-insensitively), since it would undo
      Docker's drop in `FORWARD` (Context). Then `systemctl disable ufw.service` when the unit is enabled.

   `nftables.service` stays off.
4. **One table, replaced whole.** tent's rules live in the table `inet tent`. After tent-node's header comment the
   file has `table inet tent` and `delete table inet tent`, then defines `table inet tent { … }`. `nft -f` applies
   it as one transaction; the first `table inet tent` makes the table where it is missing, so the delete cannot
   fail; no other table changes. The table's comment is `tent-node <sha256>`, the sha256 of the file's text without
   the comment line, so the comment tells whether the kernel holds this file's rules.
5. **The chains** (goldens in `internal/nodeup/testdata/firewall-*.nft.golden`):
   - `input`, policy drop: established and related traffic; invalid traffic dropped; loopback; ICMPv6 router adverts
     and neighbour solicitations and adverts; DHCP replies (udp 67 to 68); then one line per NodeConfig rule and
     address family, such as `ip saddr { 10.64.0.0/16 } tcp dport 4647 accept comment "nomad-rpc"`. An ICMP rule
     matches `meta l4proto icmp` or `icmpv6`, without ports. A source prefix that another source of the rule contains,
     or a repeated one, is left out, since nft refuses a set whose elements overlap.
   - `forward`, policy accept: only the metadata drop. Docker's and the CNI plugins' chains filter the rest.
   - `output`, policy accept: the marked accept and the counted drop of decision 1.
   - The metadata lines take `ip` or `ip6` by the family of `blockMetadata`.
6. **Every boot loads it again.** The kernel forgets the table at a reboot, so the first `up` after one loads it, and
   `hostfirewall` reports `done`. Until then the node has no host firewall. On the M2.6a VM check, as soon as SSH
   answered after a reboot, only sshd, systemd-resolved (127.0.0.53 and 127.0.0.54), the DHCP client and containerd
   on a loopback port listened; by then `up` had already run, and the time before it was not measured. Nomad starts
   only from `up`, after the table (M2.6b).
7. **nft is required.** Ubuntu's images ship it (1.0.9 on 24.04, 1.1.6 on 26.04), and there is no fallback. Both
   print the table's comment in their JSON.

### The rules from tent

8. **Bridge rules.** On roles that run a client, `internal/app` adds `bridge-http` (tcp, 4646) and `bridge-dynamic`
   (tcp and udp, the dynamic ports) from Nomad's default bridge `172.26.64.0/20` and Docker's default bridge
   `172.17.0.0/16`.
   - Workloads on those bridges reach the host itself when they call the local agent, or a task with host networking,
     at the node's address. Port mappings reached from other hosts are DNAT-ed and forwarded, so they never reach the
     input chain. A container on `docker0` that calls a published port of its own node reaches docker-proxy through
     the input chain; the bridge rules let that through for Nomad's dynamic ports.
   - The rules match source addresses only, no interface: the ports are open to the cluster CIDR anyway, and the cloud
     firewall filters the public NIC.
   - The ports come from the model's constants.
   - The rules change the spec hash of client and combined groups. The form of NodeConfig does not change.
9. **Rule names are DNS labels:** 1 to 63 lower-case letters, digits and dashes, starting and ending with a letter or
   digit, as host names are. They go into nft comments, inside quotes. `Validate` refuses any other name.
   `firewall.blockMetadata` must be a plain IP address, without an IPv6 zone and not IPv4-mapped, and no rule's source
   may be an IPv4-mapped prefix: a mapped one would render as `ip6 daddr` or `ip6 saddr`, which no IPv4 packet
   matches.
10. **Asset names are constants of `nodeconfig`:** `NomadAsset` (`nomad`), `CNIPluginsAsset` (`cni-plugins`) and
    `TentNodeAsset` (`tent-node`). `internal/assets` names its assets with them, so it now imports
    `internal/nodeconfig`. tent-node still never imports `internal/assets`. An asset's name is a DNS label too, since
    tent-node names the asset's file in its cache after it (decision 17).

### The container runtime

11. **When.** `runtime` is skipped with `this node group runs no Docker` when NodeConfig's `system.docker` is false.
12. **Installed or not.** `dpkg-query -W -f=${Status} docker.io` decides. Docker is installed when the third word of
    the status is `installed`, so `hold ok installed` and `deinstall ok installed` count. Every other state, such as
    `config-files` or `half-configured`, means not installed, and so does exit status 1, which `dpkg-query` gives for a
    package that dpkg never had. Any other failure fails the phase.
13. **daemon.json first.** `/etc/docker/daemon.json` (directory 0755, file 0644, `root:root`) holds
    `{"live-restore": true, "log-driver": "json-file", "log-opts": {"max-file": "3", "max-size": "10m"}}`, indented.
    - The query runs before the write, so a failed query cannot leave a changed file that Docker was not restarted to
      read. The file is written before any install: the postinst starts Docker, which then reads it.
    - When Docker was installed before this run and the file changed, `systemctl restart docker.service`. The log
      settings do not reload on SIGHUP, and live-restore keeps the containers of a daemon that started with it. A
      failed restart removes the file, so the next run tries again.
    - Nomad sets its own log limits on the containers it starts, so these cover other containers only.
14. **The install**, when Docker is not installed, each command through `env DEBIAN_FRONTEND=noninteractive`:
    - `dpkg --force-confdef --force-confold --configure -a`, which finishes an install that an earlier run stopped
      halfway, after which apt-get would refuse to install;
    - `apt-get update`, since the image's package lists may be stale or missing;
    - `apt-get install -y --no-install-recommends -o Dpkg::Options::=--force-confdef -o
      Dpkg::Options::=--force-confold docker.io`.

    The two force options keep a configuration file that both the admin and the package changed: dpkg would ask, and
    without a terminal the question fails. None of the three commands waits for a lock (Context), so each is tried
    again every 5 s for up to 10 minutes; a command that still fails then fails the phase with
    `gave up after 10m0s: …`. The phase thus waits at most about 30 minutes, within `up`'s 45. tent-node logs
    `install Docker` before the install and one warning per command at its first failure. When the context ends
    during a wait, the error keeps the command's last error.
15. **Start.** Then `docker.service` is enabled and started where it is not. When it is not active, `runtime` first
    asks `systemctl show -p Job --value docker.service`. A pending job, such as a start queued in the boot
    transaction, a start that runs, or a restart queued after a crash, means Docker starts on its own:
    `systemctl start` still runs and waits for it, and the phase reports no change. Not active with no job is a
    change: inactive, failed, or activating while an automatic restart waits out `RestartSec`, when `systemctl start`
    waits for systemd's own restart on systemd 255 and starts Docker at once on 259
    ([platform notes §3.4](../platform-notes.md#34-user_data-metadata-and-identity)).
    `tent-node.service` is not ordered after `docker.service` on purpose: Ubuntu's `docker.service` has no start
    timeout (`TimeoutSec=0`), so a hung dockerd would hold the boot for ever
    ([platform notes §6.1](../platform-notes.md#61-packages-)).
16. **Docker's defaults stay.** The firewall backend stays iptables: Docker 29's nftables backend is experimental.
    `--no-install-recommends` leaves out pigz, among others, so Docker unpacks layers with Go's gzip, which is slower
    and breaks nothing.

### The CNI plugins and the asset cache

17. **The cache.** tent-node keeps one file per asset, `/var/lib/tent/assets/<name>` (0600; `/var/lib/tent` and
    `assets` 0700; `root:root`). A file whose sha256 matches NodeConfig's is used without a download. Any other
    content, such as an older version or a corrupt file, is removed, and the asset is downloaded, checked and written
    in its place. So each asset has at most one version on disk.
18. **Downloads.** tent-node tries the asset's URLs in turn, through `Host.Transport` (nil means Go's default
    transport and its proxy settings).
    - It follows 301, 302, 303, 307 and 308 itself, at most 10 times, because net/http's client quotes an unparsable
      `Location`, signature included, in its error. A missing or unparsable `Location`, or an eleventh redirect, fails
      the URL without another try, with an error that shows no URL.
    - It asks for the file as it is (`Accept-Encoding: identity`): some servers mark a `.tgz` as gzip-encoded, and a
      transport that asked for gzip would decode it, and the sha256 would not match.
    - Up to 3 tries per URL, 2 s apart, after a failed connection, a 429, a 5xx other than 501, or a body that stalls,
      is cut short or runs out of time. Any other status, a wrong sha256 and the size limit move on to the next URL at
      once.
    - Limits: 10 minutes per try, 60 s for the headers of each answer, 60 s without a byte of the body, and 256 MiB. A
      `Content-Length` above the limit fails before the body is read; one within it sizes the buffer.
    - Errors and logs show URLs without their query or password (`nodeconfig.RedactURL`, which `Asset.String` uses
      too). The error keeps the last failure of each URL.
19. **The `cni` phase.** It is skipped on servers with `servers run no workloads`. Otherwise NodeConfig must carry the
    `cni-plugins` asset; the phase fetches it and checks every tar header before it writes a file.
    - Only `./` and regular files at the top level with plain names (`^[A-Za-z0-9][A-Za-z0-9_.-]*$`, with or without
      `./`) pass. A nested path, `..`, an absolute path, a link, a device, a fifo, any other type, a file above
      256 MiB, or a file that comes twice (`./bridge` and `bridge` are one file) fails the phase before a file is
      written.
    - The files go into `/opt/cni/bin` (directories 0755) with the archive's mode masked to 0755, owned by
      `root:root`. Files that the archive lacks stay.
    - A cache hit unpacks the archive again, which puts back any plugin that was changed.

### The runner

20. **Process groups** (an M2.6 follow-up of ADR-0028). On Unix `ExecRunner` starts each program in a process group of
    its own. When the context ends, it sends SIGTERM to the group. If it sent SIGTERM, it sends SIGKILL to what is left
    of the group once `Run` has stopped waiting for the program and its output. exec's `WaitDelay` (10 s,
    `ExecRunner.WaitDelay`) bounds that wait by killing the program. On Windows it still kills the program alone.

### Units and tooling

21. **`cloud-init-main.service`.** No tent unit is ordered on it either, as a precaution like `cloud-config.service`:
    on Ubuntu 26.04 cloud-init runs every stage in that one service. The ordering test covers it.
22. **Reloads of PID 1.** `systemctl enable` without `--no-reload` asks PID 1 to reload by itself, so a fresh node
    shows reloads requested from `cloud-final.service` while `install` runs. ADR-0028 item 11 ("the first `install`
    needs no reload") still holds: it is about `install`'s own `daemon-reload`, which runs only when
    `NeedDaemonReload` says yes. docker.io's install reloads too, during `up`: the M2.6a VM check counted the
    requests ([platform notes §3.4](../platform-notes.md#34-user_data-metadata-and-identity)).
23. **The VM check tools.** `model.IntraRules`, `app.HostFirewall` and `app.NodeSystem` are exported, so
    `hack/tent-node-userdata` gives a node the rules and system settings of its role (`-role`, `-cidr`) with the same
    code as `internal/app`. The spike's `tentnode` check (spike v6) records the image, the firewall, Docker, the CNI
    plugins, the metadata probes and the reboot ([README](../../hack/vultr-spike/README.md)).

## Consequences

### Positive

- Workloads reach the metadata service only with CAP_NET_ADMIN or CAP_NET_RAW, which Nomad does not give by default.
- On the first boot tent's rules replace ufw's without a gap, and Docker's and the CNI plugins' tables stay as they
  are.
- A second `up` changes nothing and downloads nothing. After a reboot only `hostfirewall` reports `done`.
- An archive with links, nested paths or devices writes nothing.
- A stopped `apt-get` or `dpkg` leaves no children running.
- A lock that apt-daily or unattended-upgrades holds on the first boot delays the install instead of failing it.

### Negative / trade-offs

- The block fails closed: root `curl` to the metadata service on a node, and anything else on the host that needs
  the service, gets no answer.
- The workloads of decision 2 can set the mark and read the user data until the node has joined and is scrubbed.
- Drift inside the kernel's table, such as a manual `nft add rule inet tent …`, keeps the comment and stays until a
  reboot or a config change.
- An operator who turns ufw on again gets `ufw disable` at the next boot, which resets the `FORWARD` policy to accept
  while Docker runs, until Docker restarts.
- On an image with firewalld, a load that fails after firewalld stopped leaves only the cloud firewall.
- An nft that does not print the table's comment in JSON, such as Debian's 1.0.6, would load the table on every run.
- After a reboot the node has no host firewall until `tent-node.service` runs after `network-online.target`.
- The host firewall does not follow an `extraConfig` that changes Nomad's bridge subnet or ports.
- A container on `docker0` that calls a static published port of its own node gets no answer (the bridge rules open
  only 4646 and the dynamic ports).
- A lock held for more than 10 minutes fails the install (decision 14).
- Any pending job of `docker.service` counts, so a stop job that someone else queues just when `runtime` asks also
  counts as no change; only the reported status is wrong.
- On a cache hit every boot reads the CNI archive, about 50 MB, and decompresses it twice. The `cni` phase took
  2.9 s and 3.2 s after the reboots of the M2.6a VM check on `vc2-1c-1gb`; its memory, an estimated 100 MB for a
  moment, was not measured.
- `ExecRunner`:
  - a group member that ignores SIGTERM and holds the output, or a process that left the group (`setsid`) and holds
    it, keeps `Run` waiting until `WaitDelay`;
  - the kernel could reuse the group's id within that time, and the SIGKILL would then reach another group;
  - a leader stuck in D state keeps `Run` waiting until systemd's stop timeout kills the cgroup.

### Follow-ups

- **The M2.6a VM check** ran on 2026-09-30 on Vultr's Ubuntu 24.04 and 26.04 images and passed, with one finding
  (next item) ([platform notes §3.16](../platform-notes.md#316-spike-runs)). No install command waited for a lock on
  the first boot, so one deadline shared by the three commands is not needed.
- Handled: after a reboot `docker.service` may still be inactive, with its start job queued, when `runtime` asks. The
  M2.6a VM check saw it on 24.04, by the timing: `runtime` reported `done` after `systemctl start` waited 2 s for
  Docker. On 26.04 `runtime` reported `unchanged`; it took 1.9 s, so Docker was most likely still activating, which
  the old code counted as no change. Decision 15 now reads the pending job and reports no change. The reruns of the
  check on 2026-09-30 confirmed it: after the reboot `runtime` waited about 2.2 s for Docker on 24.04 and reported
  `unchanged`.
- **M2.6b**, from ADR-0028's follow-ups: `nomad.service` as a NodeConfig file, `join`, `nomad` with the Nomad zip
  through the asset cache (and whether the filesystem needs a streaming write for it), `verify` with Nomad,
  `refresh-join` and its lock with `up`, `/var/lib/nomad/client` with mode 0700 before the intro token, and
  `nomad config validate` of the goldens. Its VM check: Nomad reattaches to its containers after a changed
  `daemon.json` restarts Docker.
- **M2.9:** E2E `smoke` checks that containers cannot reach the metadata service.

## Alternatives considered

- **Drop only forwarded traffic and non-root sockets** (`meta skuid != 0`), roughly what AKS and GKE do. It misses
  root containers with host networking, root `raw_exec` tasks and Nomad's artifact fetcher, which runs as root in
  `nomad.service`: a job with an `artifact` from `169.254.169.254` would get the user data. EKS drops only forwarded
  traffic, and kops relies on IMDSv2's hop limit, which Vultr and Hetzner lack.
- **Match workloads by cgroup** (`socket cgroupv2`). nft resolves the path to an id when it loads the rule, so the
  cgroup must exist then, and a cgroup removed and made again gets a new id, which the rule silently stops matching.
  `nomad.slice` exists only once a client has started. Docker's containers sit in `system.slice` since Nomad 1.7, next
  to `tent-node.service`, and the artifact fetcher runs in `nomad.service`.
- **Load the rules early in the boot, from a unit of tent's or from `nftables.service`.** `/etc/nftables.conf` flushes
  the whole ruleset, Docker's and the CNI plugins' tables included. An early unit is not needed (decision 6).
- **`DPkg::Lock::Timeout`.** `apt-get install` takes the lock of the archives without waiting even with it, so the
  retries are needed anyway. With both, an install could take about 20 minutes and the phase about 40 of `up`'s 45.
- **`nft -j list table inet tent`.** It fails when the table is missing, and tent-node would have to read the error's
  text. `nft -j list tables` answers both cases.
- **Cache files named `<name>-<sha256>`.** tent-node's filesystem cannot list a directory, so older versions could not
  be found and removed.
- **net/http's own redirects.** Its error for an unparsable `Location` quotes it, signature included.
- **Docker's nftables backend.** It is experimental in Docker 29.
