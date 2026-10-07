# Vultr spike

`spike.sh` checks the undocumented Vultr behaviour that tent's Vultr provider depends on. It runs against a real
account and writes a Markdown report. It covers the 🔬 items in
[platform notes §3](../../docs/platform-notes.md#3-vultr), the **provisional** parts of
[ADR-0018](../../docs/adr/0018-vultr-provider-design.md) and the unverified facts of
[ADR-0023](../../docs/adr/0023-vultr-inventory-dedupe-and-images.md) (decision 6).

Run it once before starting milestone M1, and again whenever Vultr changes something relevant.

## What it checks

| Check (`--only`) | Question | Decides |
|---|---|---|
| preflight (always) | Region exists, plan deployable now, price, image `os_id` (public endpoints; `/plans` gets `VULTR_API_KEY` when it is set. Vultr answered it without a key with HTTP 500 on 2026-10-05 and with the plans on 2026-10-06, so the row says what the run saw: that `/plans` was read with the key, or answered without one, or answered with an HTTP error and the price could not be read) | availability preflight, cost |
| `boot` | Time to `active/running/ok`, to VPC IP, to port 22, to SSH login, to cloud-init done: A and B with package upgrades off (as tent does), V with no user_data (Vultr's vendor defaults). How soon `?tag=` lists a fresh instance | E2E timeouts; `package_upgrade: false`; search-before-retry |
| `inside` | Collected on the instance: ufw/firewalld/nftables rules, fail2ban and sshguard, `sshd -T`, `cloud-init analyze`, `systemd-analyze`, the vendor data (secrets redacted). Once from `runcmd` during first boot, once over SSH | the `hostfirewall` phase; where boot time goes |
| `metadata` | `/v1.json` fields, `instance-v2-id`, region code, interfaces; `/latest/user-data` reachability and size; a `write_files` payload in a 64 KiB user_data lands intact | `nodeup/env/vultr`; metadata block; user_data budget |
| `network` | VPC interface name, MTU, netplan config; A ↔ B ping over the VPC | interface matching, CNI MTU |
| `firewall` | Default ufw/firewalld state and whether it blocks `:4646` on the VPC. Whether a **firewall group filters VPC traffic**. How long rules take to propagate. | `FirewallCoversPrivate`; the `hostfirewall` phase |
| `alias` | Is an extra IP added inside the VPC (on the OS) reachable from another instance? | possible fixed slots on Vultr |
| `tags` | Accepted tag syntax (`/`, `=`, `:`, space, case, unicode), maximum length and count; `?tag=` exact or substring, case sensitivity; `?label=` matching | label codec (ADR-0018), server-side filtering |
| `markers` | Are `tent:cluster=…;kind=…` markers stored verbatim in VPC descriptions, SSH key names and firewall group descriptions? | ownership of non-instance resources |
| `userdata` | Maximum `user_data` size via PATCH (binary search) and on create; stored intact? | `MaxUserDataBytes` |
| `scrub` | After a user_data PATCH, does the metadata service serve the new value, and how fast? | `Nodes.MarkJoined` |
| `halt` | Is `halt` graceful (ACPI) or a hard power-off? Does `start` work? Does cloud-init re-run after a PATCH plus restart? | `GracefulShutdown` (ADR-0017) |
| `sshdup` (no instance) | Does Vultr accept a second SSH key with the same key material under another name? | a second cluster with the same operator key |
| `lengths` (no instance) | The longest VPC description, firewall group description and SSH key name stored verbatim: marker-like texts of 64 to 512 characters, updated in place as govultr does it | markers with `op` reach 99 characters |
| `rules` (no instance) | How Vultr lists firewall rules sent in tent's form (`ip_type`, `protocol`, `subnet`, `subnet_size`, `port`), and whether it takes the same rule twice | rule comparison in the firewall task |
| `fwinuse` | Vultr's answer to the delete of a firewall group that instance A uses; the group's `instance_count` in `GET /firewalls` and `GET /firewalls/{id}` while A uses it, read for up to 30 s | which answers tent retries as `ErrInUse`; the firewall group dedupe, which keeps the group with the most instances (ADR-0023) |
| `patchtags` | Does the instance PATCH that govultr sends for a user_data or firewall group change (`"tags": null`, `"ddos_protection": null`) clear the tags? | scrub and firewall PATCHes must resend the tags? |
| `vpcpending` | What `GET /instances/{id}/vpcs` answers while instance A boots, from right after the create answer until it lists an address other than `0.0.0.0` (at most 120 s): each distinct answer (HTTP status, error text or addresses, and A's status) with the seconds since the create | whether tent's List may read a 404 on `/vpcs` as a deleted instance |
| `halttwice` | Halt A, wait until it is stopped (at most 60 s), halt it again: both answers. Runs last and leaves A stopped | whether `Stop` fails when Go's HTTP/2 client sends the bodyless halt twice |
| `objstore` (runs only if `S3_*` is set) | `If-None-Match: *` and `If-Match` on an existing bucket | state-store locking |
| `tentnode` (alone, not in the default list) | A development build of tent-node on instance T, the combined node of a cluster of one node, booted with the user data of `hack/tent-node-userdata` (`-cidr` the VPC's network, `-zone` the region): Nomad, the CNI plugins and tent-node, Docker, tent's host firewall, and a CA, certificate and gossip key that the tool makes for this run. Over SSH after the first boot: node.json arrives intact (its sha256 against the gz+b64 payload), `cloud-init status --wait --long`, the OS and its time sync (`timedatectl`, the NTP units, systemd-timesyncd and chrony), the units active and enabled, `status.json` (system, hostfirewall, runtime, cni, join and nomad done, verify unchanged; instance id, zone, private IP against the API, version), `systemd-analyze critical-chain tent-node.service` (marked UNEXPECTED when it passes cloud-final, cloud-config or cloud-init.target), what `systemctl is-enabled no-such.service` prints on stdout and stderr and its exit code, and the cloud-init output. The image and the machine: `dpkg -l nftables iptables firewalld docker.io` and `apt-cache policy docker.io`, `systemctl is-enabled nftables ufw docker`, `update-alternatives --query iptables`, the apt sources' components (`universe`), the apt lists (file count and newest file; `ls -la` and `du -sh` in the details), when apt-daily, apt-daily-upgrade and unattended-upgrades ran next to tent-node.service and their journal; `nft list ruleset` and `nft -j list tables` (tent's table with the ruleset's sha256 in its comment, next to Docker's `ip filter` and `ip nat`), `/etc/ufw/ufw.conf` and the ufw unit (`ENABLED=no`, disabled), `docker info` (storage and cgroup driver, live-restore, logging driver, firewall backend), `/etc/docker/daemon.json`, the iptables and ip6tables FORWARD policies, `ls -l /opt/cni/bin /var/lib/tent/assets` (the plugins of Nomad's bridge network present and executable); which units asked PID 1 for a reload and how often (a record: install's `systemctl enable` asks for one by itself); records of what can restart Docker or containerd without tent: whether `needrestart` is installed and its `$nrconf{restart}` mode, `/etc/apt/apt.conf.d/20auto-upgrades`, `debconf-show docker.io` (whether `docker.io/restart` restarts Docker at its upgrades), and `systemctl show -p Restart,Requires,BindsTo,PartOf,Wants,After docker.service` (a `Requires=`, `BindsTo=` or `PartOf=` on containerd would restart Docker with it) and whether `systemctl status tent-node.service` warns "changed on disk". The metadata block, a pass or fail row each: root `curl` on the host, a root `busybox:1.38` container on the host network and one on Docker's bridge must not reach 169.254.169.254. tent's rules drop, but a timeout alone may come from a drop elsewhere on the path, so a probe passes only on a timeout that the counter of the metadata drop in its chain of tent's table saw (output on the host and the host's network, forward on the bridge) grow during the probe: curl's exit 28 before any connection (`time_connect` 0), and busybox's "download timed out", which wget prints for a stalled connection as for a dropped one, once the container reached `archive.ubuntu.com`. An answer, an HTTP error among them, a connection that then timed out, or a timeout that the counter did not see fails the check; anything else, such as a refused connection or an unreadable counter, is unknown. The details hold each probe's output and the counters before and after it; `tent-node up` by hand must succeed, with every phase unchanged; the counters of the drops in tent's output and forward chains after the probes (both above 0) and around that `up` (no change). Nomad: `nomad.service` active, `static` for `systemctl is-enabled` (no `[Install]`), `Type=notify`, `After=` with `docker.service` and `network-online.target`, systemd's default stop timeout (`TimeoutStopUSec` `1min 30s`: the client does not drain at shutdown), no restarts by systemd, and when it became active (`systemctl status` in the details); the ACL bootstrap on the node with `nomad acl bootstrap`, whose token goes only into a root-only file on T that the later `nomad` commands read; through the local API over mTLS with the node's certificate, the leader (`/v1/status/leader`, T's private address), `nomad server members` (T alone, alive) and `nomad node status` (one node: ready, eligible, pool `default`, the zone as datacenter, the meta `tent_instance_id` against the API id, `tent_cluster` and `tent_nodegroup`); the lines of Nomad's journal that name client introduction, as a record. A job, `busybox:1.38`'s httpd in bridge mode with a dynamic port: an allocation runs within 3 minutes (client status running, desired status run, its task running and started in this boot); its port, on T's private address within 20000-32000, answers the job's text from the host within 30 s; a probe from inside its container must not reach 169.254.169.254, judged as the probes above from the drop in tent's forward chain. Docker's restart: a harmless key (`"debug": false`) goes into `/etc/docker/daemon.json`, and `tent-node up` by hand must write tent's file again and restart `docker.service` (runtime done, every other phase unchanged). live-restore keeps the container, but Nomad 2.0.7's docker driver loses its wait on it when dockerd restarts and stops the container when the wait breaks (`handle.go`, a guard against a wait that returned incorrectly); the task then starts in a new container after the restart policy's delay (17-19 s on 2026-10-05), which the maintainer accepts. So the row passes when `docker.service` restarted, `nomad.service` did not, and the same allocation runs again within 3 minutes, and it says whether the container was kept or replaced, with the task's restarts before and after and how long after Docker's restart the task ran again; the port answers again. containerd's restart: containerd's package restarts `containerd.service` at every upgrade, which unattended upgrades may run on every node at once, so with the job running the script runs `systemctl restart containerd.service` and watches the task for a minute: it passes only when the container keeps its id and start, the task does not restart, neither `docker.service` nor `nomad.service` restarts, and the port answers, and says what happened otherwise; the lines of `nomad.service` since the restart that name a wait, a termination, an EOF or a restart are in the details. Nomad's restart, after Docker's and containerd's, which check that Nomad was not restarted: with the job running, `systemctl restart nomad.service` by hand, to prove that tasks keep running through a restart of Nomad (`KillMode=process` keeps the task's processes and Docker its container, the client reattaches to them, and with no drain the node stays eligible). It passes when the restart exits 0; `nomad.service` is active again with a later `ActiveEnterTimestamp` and `NRestarts` 0 (systemd counts only the restarts of `Restart=` there, and a restart by hand sets it to 0); after the node reports ready and 15 s for the client to restore, the container keeps its id and start time, the allocation runs with the same task restarts and task start, and the node is ready and eligible; no line of `nomad.service` since the restart names a drain; and the port answers. The row gives how long the restart took; the agent's lines that name a drain, restoring or reattaching are in the details. refresh-join: on a server or combined node it asks the node's own agent first (`https://127.0.0.1:4646/v1/status/peers?stale` over mTLS with the ServerName `server.<region>.nomad`, the first such call on a real node), so once T's Nomad has a leader one run gets T's address back and rewrites `05-join.hcl` once. The script waits up to 3 minutes for that run's log line ("05-join.hcl joins the servers that answered", which the row records), then up to 2 minutes for one more run. It passes when the line appears once, `05-join.hcl` holds `retry_join = ["<T's private address>:4648"]`, `/var/lib/tent/peers.json` is `["<T's private address>"]`, no run failed, and both files keep their modification time and sha256 across the later run; `systemctl list-timers`, the journal of `tent-node-join.service` and both files are in the details. Then that all 29 files tent-node writes exist (of the node's key and `01-gossip.hcl`, the size instead of the sha256; `node.json`, which holds the same secrets, keeps its sha256, which reveals nothing of random keys). After a reboot from inside: what listens (`ss -tulpn`) as soon as SSH answers, and whether up had finished by then; cloud-init and `NTPSynchronized` again, `status.json` again (hostfirewall and nomad done, since the kernel forgets tent's table and only up starts Nomad; every other phase unchanged), tent's table loaded again with the same comment (its rules in the details), the drop counters since then, the modification time and sha256 of tent-node's files before and after (only `status.json` may change), how long preflight's metadata read took after the reboot and on the first boot, the boot order (multi-user.target orders after tent-node.service, which became active before multi-user.target, before cloud-final.service started), the critical chains of cloud-final.service and of the default target as records, and the journal of both boots. Then Nomad again: the unit, the start of `nomad.service` after the hostfirewall phase logged its result (both monotonic, from systemd and tent-node's journal; join's warnings of that boot in the details, since it asks T's address from `peers.json` while Nomad is still stopped), the leader, the server and the client, ready and eligible; the tasks died with the machine without a migration, so after the boot the client restores the allocation or the scheduler replaces it: an allocation of the job, the same or a new one, runs again within 3 minutes, its task started in this boot (its start against the boot time in `/proc/stat`, since the server shows a restored allocation as running until the client reports), and its port answers within 30 s, with the allocations and the node's eligibility in the row and the first boot's allocation (its status and events) in the details whatever the verdict; and the shutdown with the job running, from the journal of the first boot, which also holds Docker's restart: the last stop of `nomad.service` must have begun and ended before the last stop of `docker.service` began, and no line of `nomad.service` from its stop on may name a drain, since tent configures none, so Nomad's stop takes seconds; the row gives when each stop began and how long Nomad's took, and the details the shutdown lines of both units | the M2.5, M2.6a and M2.6b exit checks on a real VM; platform notes §3.4 and §3.16 |
| `cluster` (alone, not in the default list) | Checks the bootstrap in `update` (M2.7a) and what M2.7b added: the scrub of each node's user data once it joined, the `tent/joined` label, the delete guard, and with `--unregistered` the replacement of a client that never registered. tent builds a cluster of 3 servers and 2 clients itself, with `bin/tent create cluster spk-<run> --provider vultr --region … --machine-type … --servers 3 --workers 2 --ssh-key <the run's key> --ssh-access <this machine>/32 --yes` on a temporary `file://` state store and the development tent-node of `make dev-upload`; five instances exist at once. Records: the exit code and time of create, and from its progress lines (stderr, each stamped with the second it came) when Nomad had a leader, the ACL system was bootstrapped, the servers were healthy, the keyring was ready, the seconds from the leader line to each client's registration line, and that each of the five nodes has a `scrubbed the user data of node ...` line (a server's after the ready keyring, a client's after its registration); the cluster's instances in the Vultr API (five, active and running, the labels, the public address of `<name>-servers-0`); `tent export nomad <name> --dir <dir> --shell sh` (exit 0; the directory has mode 700 and its four files 600; stdout is the six lines of the sh form with `NOMAD_ADDR` the public address of a server; the token's sha256 differs from the bootstrap token's and the token is in neither stream; `GET /v1/acl/token/self` with it answers a management token whose name starts with `tent export nomad` and that ends 24 hours after the export; `openssl x509` says `cli.pem` has the subject `CN=cli.global.nomad`, client authentication as its only extended key usage, and ends with the token); then with curl, the exported certificate and token (`--cacert`, `--cert`, `--key`, `--resolve server.global.nomad:4646:<address>`, the token from a mode-0600 config file): `/v1/agent/members` (three alive), `/v1/operator/autopilot/health` (healthy, three voters) and `/v1/nodes` (two ready and eligible clients under their names); the docker job of `tentnode` in bridge mode, registered with `PUT /v1/jobs`, running on a client within 3 minutes, then purged and stopped; when a `nomad` binary is on the PATH, `nomad server members` after the six lines are sourced (three alive servers in the table on its stdout, since the CLI adds a hint on stderr; otherwise the row says it was skipped); `tent validate cluster` (exit 0, `cluster <name> is valid: 3 servers and 2 clients`, and the warnings about the open `access.api` and the one failure domain on stderr); `tent ui --listen 127.0.0.1:0` in the background (the address line on stdout; through its port, with no certificate and no token, `GET /v1/status/leader` answers 200, `GET /v1/acl/token/self` a management token named `tent ui ...` that is not the bootstrap token, compared by sha256, and `GET /ui/` 200; a request with `Host: example.com` gets 403; SIGINT ends it with exit 0 within 15 seconds, else it is killed and the row says so, and the port then refuses connections; the exit trap also ends it); a client whose Nomad stops (`systemctl stop nomad.service` on `<name>-workers-1`, the seconds until Nomad lists the node down, then `tent validate cluster` must exit 2 and name the node with `its Nomad client is down`, then `systemctl start nomad.service` and `tent validate cluster --wait 5m` must exit 0, with the seconds of both); `update cluster --exit-code` (exit 0) and `update cluster --yes` (`cluster <name> is up to date`); the `tent/spec-hash` tag and the `tent/joined=true` tag of each instance; each instance's user data, read from `GET /v2/instances/{id}/user-data`, which equals tent's stub byte for byte (the three lines `#cloud-config`, a comment and `{}`) and holds no `/etc/tent/node.json`. Over SSH, on a client: `status.json` (every phase done or unchanged), `05-join.hcl` (three servers), the journal of `tent-node-join.service` (the first peers call between two machines: runs, failures, rewrites) and the size of the intro token file (a record; the size check of the plan assumes at most 2048 bytes); on a server: `systemctl restart nomad.service` and the unit's state afterwards (a server's agent exits with 1 on SIGTERM because `leave_on_terminate` is off, so the row counts the `Failed with result` lines of the unit's journal), then autopilot from the second server until three voters are healthy again; a reboot of `<name>-workers-0` (the node answers on a new boot, Nomad lists it ready and eligible, `/etc/tent/node.json` has the sha256 it had, and `cloud-init status --wait --long` after the boot reads `status: done`, exit 0 and no recoverable error: a degraded status, such as cloud-init's warning about an empty cloud config, is UNEXPECTED and the row carries the warning's first line; the first boot of a node is not judged). The guard: the specs are saved (`tent get`), the clients' size is set to 1 (`tent replace -f`), and `tent update cluster` and `tent update cluster --yes` must each exit 1 with `update would delete a node that joined Nomad: <name>-workers-1` and leave five instances; the saved specs go back and `update --exit-code` exits 0. With `--unregistered` also a client that never registers: Nomad is stopped on `<name>-workers-1`, its node is purged through the Nomad API (a node that is already gone is fine), `tent/joined=true` is taken off the instance's tags (a PATCH with the whole remaining list), and the script waits until the instance is 32 minutes old; then `tent update cluster` must plan `- node <name>-workers-1 (ID ..., not registered)` and the create, `--yes` must delete, create, register and scrub the node (exit 0: tent retries a create that Vultr's instance limit refuses right after the delete, for up to 2 minutes; the row reports the seconds from the `deleted node` line to the `created node` line, beside the seconds that a client's create took in the build, from `creating node` to `created node`, to show how long the limit held the create back; the verdict does not depend on them), and the new instance must have another id, the label, the stub and a ready node in Nomad at its private address (a record: whether that address is the old one); `update --exit-code` exits 0. Then `delete cluster --yes` (exit 0), nothing in the Vultr API that carries the cluster's tag or marker, and an empty state store; last, a search of everything the run recorded for the CA key, the gossip key, the ACL bootstrap token, the operator's key and token and the token of `tent ui`. The exit trap runs the delete when the run did not, then removes by tag and marker what is left | the M2.7a, M2.7b and M2.8 exit checks on a real cloud: the bootstrap in `update`, the scrub, the delete guard, the replacement of a client that never registered, `tent export nomad`, `tent validate cluster` and `tent ui`; platform notes §1.6 and §3 |

## Cost and duration

- **Instances.** At most 3 exist at a time, 4 in total with `userdata`, of the chosen plan: `vc2-1c-1gb` by
  default, $5 per month. Vultr bills at least 1 hour per instance, so a full run costs about **$0.03**.
- **Other resources.** One VPC (plus short-lived test VPCs for the mask check), one SSH key (two with `sshdup`) and
  up to three firewall groups (`firewall` or the group with no rules, `lengths`/`rules`, `fwinuse`). They are free,
  and all are deleted on exit.
- **Duration.** 20–45 minutes, mostly waiting for boots, the halt/start cycle and instance V (vendor defaults).
- **Checks without SSH.** `fwinuse`, `patchtags`, `vpcpending`, `halttwice`, `tags`, `markers` and `userdata` use only
  the API. If no check that needs SSH is selected, instance A gets a firewall group with no rules at creation (see
  [Safety](#safety)), and the checks start once the API reports A ready, about a minute after the create.
  `--only vpcpending,fwinuse,halttwice` and `--only sshdup,lengths,rules,fwinuse,patchtags,vpcpending,halttwice`
  create one instance (A): about **$0.01** and 5–10 minutes. `--only sshdup,lengths,rules` creates no instance:
  free, 1–3 minutes.
- **cluster.** Five instances at once (three servers and two clients of the chosen plan: the account's instance limit
  must allow five), a VPC, two firewall groups and an SSH key, all made and deleted by tent: about **$0.05** and
  25-35 minutes. With `--unregistered` tent replaces one client, which bills a sixth instance for its 1 hour minimum,
  and the run waits until that client's instance is 32 minutes old: 50-70 minutes in all. `--dry-run` prints this.
- **tentnode.** One instance (T), one VPC (the first of `VPC_MASKS` alone, no test VPCs) and one SSH key: about
  **$0.01** and 10–30 minutes with Docker's and Nomad's install, the job, Docker's restart, the wait for
  refresh-join and the reboot.

## Prerequisites

- **Tools:**
  - bash 3.2+ (the macOS default is fine);
  - `curl`, `jq` 1.6+, `ssh`, `ssh-keygen`, `awk`, `base64`, `tr`, `od`;
  - curl 7.75+ (`--aws-sigv4`) for the optional Object Storage check.
- **A Vultr API key in `VULTR_API_KEY`.**
  - Prefer a dedicated service user whose IAM policy allows compute instances, VPCs, firewalls and SSH keys.
  - If the key has an IP allow-list, include the machine you run from.
  - **Never paste the key into chat or commit it.** The script passes it to curl through a mode-0600 config file,
    never on the command line.
- **Account limits** must allow at least 3 concurrent instances. New Vultr accounts can have tiny limits, so request
  an increase first (Console → Billing → Limits). If the second or third instance fails, the report records the
  error and the checks that need it are skipped.
- **Reachability.** For the checks that need SSH, your machine must reach the instances' public IPs on ports 22 and
  4646. The firewall check probes port 4646 from your side.
- **For `cluster`:** `openssl`, `mkfifo` and `find` (and `nomad`, which only adds a row), and the same development
  build as `tentnode`: `make dev-upload` ([hack/tent-node-upload](../tent-node-upload/README.md)) sets
  `TENT_NODE_URL` and `TENT_NODE_SHA256` and builds `bin/tent` and `bin/tent-node_linux_amd64`, which must be of one
  build (the script checks the sha256). tent itself reads the two variables and `VULTR_API_KEY`, which the script
  passes on through the environment. tent lets only this machine reach SSH: the script asks `api.ipify.org` for its
  public IPv4 address, or takes `RUNNER_ADDR`. The Nomad API takes tent's default access (`0.0.0.0/0`, with mTLS and
  an ACL token), so this machine reaches port 4646 of the servers.
- **For `tentnode`:** `go` and `gzip`, and a development build of tent-node uploaded with `make dev-upload`
  ([hack/tent-node-upload](../tent-node-upload/README.md)), which sets `TENT_NODE_URL` and `TENT_NODE_SHA256`. The
  script takes the version of that tent-node from `bin/tent` of the same build, after it checks that
  `bin/tent-node_linux_amd64` has the sha256 `TENT_NODE_SHA256`; set `TENT_NODE_VERSION` to skip both. The plan must
  be amd64, as the default `vc2-1c-1gb` is. The script never prints the URL, and masks its signature and credential
  in what it records from the instance. Before it creates anything, `hack/tent-node-userdata` reads Nomad's signed
  SHA256SUMS from releases.hashicorp.com. Instance T downloads Docker from Ubuntu's archive, Nomad from
  releases.hashicorp.com, the CNI plugins from GitHub, the `busybox:1.38` image of the metadata probes and the job
  from Docker Hub, and the pause image of Nomad's docker driver from registry.k8s.io; they go with the instance.
  Docker Hub allows 100 anonymous pulls per 6 hours per IPv4 address or IPv6 /64 (its usage docs, read
  2026-09-29); past that the pull fails with 429, and the container probes read unknown and the job fails.

## Usage

```bash
# No key, no resources: region/plan/image preflight
./hack/vultr-spike/spike.sh --preflight

# Show what would be created
./hack/vultr-spike/spike.sh --dry-run

# Full run (asks for confirmation)
export VULTR_API_KEY=...        # from your password manager
./hack/vultr-spike/spike.sh

# Non-interactive, other region/plan, subset of checks
./hack/vultr-spike/spike.sh --yes --region fra --plan vc2-1c-2gb --only tags,markers,userdata

# The ADR-0023 facts: SSH key duplicates, text lengths, rule listing (no instance), then with instance A only
# the delete of a group in use and the PATCH that govultr sends
./hack/vultr-spike/spike.sh --yes --only sshdup,lengths,rules,fwinuse,patchtags

# The node primitives, with instance A only and no SSH: /vpcs while A boots, the delete of a group in use and
# the groups' instance_count, a second halt
./hack/vultr-spike/spike.sh --yes --only vpcpending,fwinuse,halttwice

# Object Storage conditional-write check only (existing bucket; no Vultr API key, no instances)
S3_ENDPOINT=https://ams1.vultrobjects.com S3_BUCKET=my-bucket \
S3_ACCESS_KEY=... S3_SECRET_KEY=... ./hack/vultr-spike/spike.sh --only objstore
```

The tent-node check, in fish:

```fish
make dev-upload | source             # needs TENT_DEV_S3_URL and the R2 token (hack/tent-node-upload)
set -x VULTR_API_KEY (<password manager command>)
./hack/vultr-spike/spike.sh --yes --only tentnode

# The same on Ubuntu 26.04: by the image's exact name, or by its os_id (tent's image table says 2760).
env OS_NAME="Ubuntu 26.04 LTS x64" ./hack/vultr-spike/spike.sh --yes --only tentnode
env OS_ID=2760 ./hack/vultr-spike/spike.sh --yes --only tentnode
```

The cluster check, in fish:

```fish
make dev-upload | source             # sets TENT_NODE_URL and TENT_NODE_SHA256, builds bin/tent
env VULTR_API_KEY=(<password manager command>) ./hack/vultr-spike/spike.sh --yes --only cluster
# with the client that never registers (about 30 minutes more)
env VULTR_API_KEY=(<password manager command>) ./hack/vultr-spike/spike.sh --yes --only cluster --unregistered
```

`--unregistered` needs `--only cluster`. On `<name>-workers-1` the script stops `nomad.service` and does not mask it:
tent-node starts Nomad only at boot, the refresh-join timer never restarts it, the unit is not enabled for boot, and a
unit that `systemctl` stopped is not restarted by `Restart=on-failure`. `systemctl mask` refuses a unit whose file is
in `/etc/systemd/system`, where tent-node writes it. A row checks before the plan that Nomad is still stopped. When
the guard check could not put the saved specs back, one row says that the client check was skipped.

The cluster check uses tent's default image (`ubuntu-24.04`): `OS_NAME` and `OS_ID` only name the image of the
preflight row. With `--keep` the run skips the delete check (its three rows read "skipped: --keep") and the exit trap
deletes nothing: the cluster stays and bills until you delete it, and its state store, which holds the cluster's
secrets, moves to `hack/vultr-spike/results/cluster-<run>-state`. A run that stopped in the guard check puts the saved
specs back first; if that fails, the script saves them as `cluster-<run>-specs.yaml` beside the report. A run that
stopped after Nomad was stopped on `<name>-workers-1` says so, and that the instance's `tent/joined` tag may be off:
without the tag, `tent update cluster <name> --yes` replaces the machine once it is 31 minutes old. When the move
fails, the store and the run's temporary directory stay where they are, without the API key, the token and the private
keys, and the printed commands name the store there. The script prints the `tent delete cluster` command for the kept
cluster and the `tent update cluster <name> --yes --state file://...` command that finishes a build that was
interrupted. A search for the secrets runs at exit when the run did not reach its own.

The script picks the image by `OS_ID` when it is set and by the exact name `OS_NAME` otherwise. The report's header
and preflight row name the image that the run used, looked up by its `os_id` when only `OS_ID` is set.

Options: `--preflight`, `--dry-run`, `--yes`, `--keep`, `--unregistered`, `--only LIST`, `--region ID`, `--plan ID`,
`--out DIR`. Environment: `REGION`, `PLAN`, `OS_NAME` / `OS_ID`, `VPC_SUBNET`, `VPC_MASKS`, `READY_TIMEOUT`,
`VENDOR_TIMEOUT`, `USERDATA_TARGET`, `S3_*`, `TENT_NODE_URL`, `TENT_NODE_SHA256`, `TENT_NODE_VERSION`, `RUNNER_ADDR`.
See `--help`.

## Output

The report is written to `hack/vultr-spike/results/vultr-spike-<UTC time>-<region>-<run>.md`. The directory is
git-ignored. It holds a summary table (check, result, design impact) and details: metadata without user data,
network configuration, host firewall rules, the journal of the halted boot, the `/vpcs` answers while A boots and
the JSON of a firewall group that A uses. For `tentnode`: `status.json`, the nftables ruleset and tables, `docker
info`, `daemon.json`, the CNI directories, the apt sources and journals, PID 1's reloads, the output of each metadata
probe and of each `tent-node up` by hand, `systemctl status nomad.service`, `nomad server members` and `nomad node
status -self`, the job's status, `docker ps -a` and `docker images`, the needrestart, unattended-upgrades, docker.io
debconf and docker.service records, Nomad's lines since Docker's and containerd's restarts that name a wait, a
termination, an EOF or a restart, its lines that name a drain, restoring or reattaching after Nomad's restart, the
join timer and its journal, `05-join.hcl` and `peers.json` after refresh-join, join's warnings after the reboot,
`ss -tulpn` after the reboot, tent-node's journal of both boots, and the shutdown lines of `nomad.service` and
`docker.service`.

For `cluster`: tent's stdout and stderr of create, of export, validate, ui, every update and delete (the signatures
of `TENT_NODE_URL` hidden), the stdout and stderr of `nomad server members`, the cluster's instances, `status.json`
and `05-join.hcl` of a client, the journal of its
`tent-node-join.service`, the state and journal lines of `nomad.service` after the restart of a server,
`cloud-init status --wait --long` after the reboot of a client and, with `--unregistered`, the exit code and the
unit's state after `systemctl stop nomad.service`.

**After a run:**
1. Copy the findings into [`docs/platform-notes.md` §3](../../docs/platform-notes.md#3-vultr), replacing each 🔬 with
   the measured fact and the date.
2. Resolve the provisional items of [ADR-0018](../../docs/adr/0018-vultr-provider-design.md). Mark each confirmed item
   on the status line, or write a superseding ADR if a decision changes.
3. Update the related GitHub issues, for example the Object Storage conditional writes check.

## SSH

Spike v1 lost SSH to its instances after a burst of connections, so v2 is careful:
- One multiplexed connection per host (`ControlMaster`); every command runs over it.
- `-F /dev/null`, `IdentitiesOnly=yes` and `IdentityAgent=none`: your ssh config and agent keys are not used.
- At most one new connection to port 22 per host every 15 s, which stays under ufw's `limit` (6 per 30 s).
- After 3 failed connections in a row a host is skipped, and its results read "unknown (ssh failed: reason)" instead
  of a negative result.

## Safety

- **Tagging.** Every instance is tagged `tent-spike-<run>`. VPCs, SSH keys and firewall groups carry
  `tent:cluster=tent-spike-<run>;…` markers. Created IDs are recorded as soon as they exist. The cluster check is the
  exception: tent makes its objects, so they are named `spk-<run>-…`, its instances carry the tag
  `tent/cluster=spk-<run>`, and its VPC, firewall groups and SSH key carry the marker `cluster=spk-<run>;` in their
  description or name.
- **No inbound traffic without SSH checks.** The Vultr image allows root password login over SSH. If no selected
  check needs SSH, the script first creates a firewall group with no rules (`tent-spike-<run>-lockdown`) and
  creates the instances with it, so they take no inbound traffic. The script does not know your public IP, so it
  does not allow SSH from it. `patchtags` and `fwinuse` detach or delete A's group; the script attaches the
  group with no rules again right after each of them.
- **Cleanup on exit.** On any exit, including errors and Ctrl-C, the script writes the report and then deletes
  everything it created, in this order:
  1. instances, plus anything still tagged with the run;
  2. firewall groups;
  3. VPCs;
  4. SSH keys, plus any key whose name carries the run tag (the second key of `sshdup` when its create answer had
     no usable id).

  The report records how long the API refused to delete each firewall group and VPC after the instances were
  gone.
- **`--keep`** skips the deletion and prints the resources, so you can inspect them. Delete them yourself afterwards.
- **Secrets of the tentnode check.** T's user data holds the node's key and the gossip key of a CA that
  `hack/tent-node-userdata` makes for the run, and T keeps the Nomad ACL token in a root-only file. The report shows
  none of them (of the node's key and the gossip file only their size and modification time; of node.json, which holds
  them too, its sha256, which reveals nothing of random keys), and the script deletes T on exit. With `--keep`, T
  stays with them: delete it yourself.
- **Secrets of the cluster check.** The token of `tent export nomad` reaches curl only through a mode-0600 config
  file (never on a command line), and the script never runs with `set -x`. The ACL bootstrap token stays in the state
  store: the script reads it only to compare sha256 sums and to search the report. The state store of the run, with
  the CA key, the gossip key and the bootstrap token, is a directory in the run's temporary directory (mode 0700), and
  the operator's key and token, made by `tent export nomad`, are in a directory of it; the script removes both on
  exit. `tent ui` makes a second management token, which the script reads once through the proxy, compares by sha256
  and then removes the file that held it. With `--keep` both tokens work until they expire after 24 hours. A row at
  the end searches everything the run recorded (tent's output, the details, the summary) for the long lines of the
  secrets it read (the row names them) and names only the files that hold one. The nodes' keys are in their user data
  until the node joins and tent scrubs it: the script never reads that user data to the terminal or the report, only
  whether it equals tent's stub or holds `/etc/tent/node.json`, and it removes the answer of the API at once.
- **Cleanup of the cluster check.** The exit trap runs `tent delete cluster --yes` when the run did not, and then
  removes by tag (instances: `tent/cluster=<name>`) and marker (VPCs, firewall groups, SSH keys: `cluster=<name>;`)
  what the Vultr API still lists, with the same retries as the other checks. The delete's output gets a second secret
  search, with its own row ("Secrets in the output (after the exit trap's delete)").
- **Manual cleanup.** If a run was killed with `kill -9`, find leftovers by the `tent-spike-` tag or description in
  the Vultr console. For the cluster check search for the tag `tent/cluster=spk-<run>` (five billed instances) and the
  marker `cluster=spk-<run>;`. A killed cluster run also leaves its state store, with the cluster's secrets, and the
  operator's files in `$TMPDIR/tent-spike.*`: run `tent delete cluster spk-<run> --yes --state
  file://$TMPDIR/tent-spike.<id>/state` first, which deletes what tent made, and then remove that directory.
- **No create retries.** The script never retries a failed create request. This is deliberate, because blind retries
  are how duplicates happen ([ADR-0015](../../docs/adr/0015-idempotency-without-unique-names.md)).
