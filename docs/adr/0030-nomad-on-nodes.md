# ADR-0030: Nomad on nodes

- **Status:** Accepted; amended by [ADR-0031](0031-bootstrap-in-update.md) (item 2: `00-tent.hcl` has
  `leave_on_terminate = false` on server and combined agents and `true` on clients, which settles the
  follow-up on servers, and the hash of server and combined groups moved; item 4: SIGTERM makes a server exit
  at once with status 1 and stay a Raft peer, so `nomad.service` on server and combined nodes should end as
  failed after a stop (inferred: the runs had no systemd), and the unit's text is unchanged; item 17: `update`
  builds a node's config through the same node builder as `NodeConfigOf`; the M2.7 follow-ups on `update`, the
  clients after healthy servers and the wait for registration are built; the check of the peers call between
  two VMs is part of the real-cloud check) and by [ADR-0032](0032-joined-label-scrub-and-delete-guard.md) (the scrub
  that the VM check lacked is built for clusters that `update` builds; the VM of `hack/tent-node-userdata` is still not
  scrubbed) and by [ADR-0033](0033-operator-commands.md) (`validate cluster` makes no warning about `drain_on_shutdown`
  in `extraConfig`) and by [ADR-0038](0038-rolling-update-of-server-groups.md) (item 10: the joining forms of
  `10-node.hcl` for server and combined nodes, with no `server` block, are two more goldens that the weekly online job
  validates; Nomad 2.0.7 accepted both on 2026-10-10; item 19: `00-tent.hcl` gains `heartbeat_grace = "20s"` in the
  `server` block of server and combined nodes and `rpc { keep_alive_interval = "5s" }` on client and combined nodes,
  and the hash of every group moved)
- **Date:** 2026-10-02
- **Deciders:** ingvarch
- **Related:** amends [ADR-0005](0005-immutable-nodes-and-nomad-aware-rollouts.md) and
  [ADR-0017](0017-api-driven-server-removal.md) (clients do not drain themselves at shutdown),
  [ADR-0016](0016-server-discovery-seed-and-refresh.md) (the `join` phase refreshes at boot; what a refresh asks and
  keeps), [ADR-0027](0027-nodeconfig-contract-rendering-and-spec-hash.md) (NodeConfig gains `region`;
  `nomad.service` is a NodeConfig file; `00-tent.hcl` turns off the update check and Consul auto-join and drops
  `drain_on_shutdown`), [ADR-0028](0028-tent-node-agent-units-and-delivery.md) (`join`, `nomad`, `verify` with Nomad
  and `refresh-join` are built; the lock between `up` and `refresh-join`) and
  [ADR-0029](0029-host-firewall-runtime-and-cni-on-nodes.md) (after a reboot `nomad` reports `done` too; the VM check
  tools; its M2.6b follow-ups are built); [ADR-0006](0006-two-binaries-and-nodeconfig.md),
  [ADR-0007](0007-security-baseline.md), [ADR-0019](0019-combined-server-client-role.md),
  [ADR-0026](0026-channels-and-release-assets.md), [architecture §8.1](../architecture.md#81-bootstrap-chain),
  [§8.2](../architecture.md#82-tent-node-phases), [§8.3](../architecture.md#83-nodeconfig-contract),
  [§8.4](../architecture.md#84-nomad-configuration-rendering), [§8.5](../architecture.md#85-artifacts-and-verification),
  [§9.1](../architecture.md#91-pki), [§15](../architecture.md#15-testing), [§18](../architecture.md#18-open-questions),
  [platform notes §1.4](../platform-notes.md#14-downloads-and-verification),
  [§1.6](../platform-notes.md#16-the-agent-on-a-node)

## Context

M2.6b builds the rest of `tent-node up` and `refresh-join`: `join` and `nomad` replace their stubs, and `verify`
checks the Nomad agent. The facts that shaped it, with their sources in
[platform notes §1.4](../platform-notes.md#14-downloads-and-verification) and
[§1.6](../platform-notes.md#16-the-agent-on-a-node):

- **Stopping.** HashiCorp's stock unit stops Nomad with SIGINT, which leaves the cluster only with
  `leave_on_interrupt`. On SIGTERM with `leave_on_terminate`, a client that has `drain_on_shutdown` drains itself
  and marks itself ineligible, and it stays ineligible after its next start; Nomad has no setting that undoes it.
  Nomad's graceful wait is 5 s plus that drain's deadline, and systemd kills a stopping service after 90 s by default.
- **Restarts.** Nomad reads `retry_join` only at start, and SIGHUP does not reload it.
- **Health without a leader.** A server's `/v1/agent/health` needs a leader, which a new cluster lacks until enough
  servers have booted; `/v1/status/leader?stale` answers 200 without one. A client is healthy once it knows a server,
  before it has registered. Clients forward `/v1/status/peers`, but block until they have registered.
- **The region.** Server certificates name no node address, only `127.0.0.1`
  ([architecture §9.1](../architecture.md#91-pki)), so a node that calls a server at its private address must name
  `server.<region>.nomad`. NodeConfig did not carry the region; only `00-tent.hcl` did.
- **The Nomad zip.** The linux zips of Nomad 2.0.0 and 2.0.7 take 53 to 59 MB and hold `LICENSE.txt` and `nomad`, a
  binary of 137 to 147 MB. Nodes may have 1 GB of memory.
- **Defaults that do not fit.** Consul auto-join is on by default and looks for Consul, which tent never runs, and the
  agent asks HashiCorp for updates.
- **The boot.** `up` loads tent's host firewall at every boot (ADR-0029), and it runs inside `tent-node.service`, so
  an order between that unit and `nomad.service` would deadlock (ADR-0028).
- **`nomad config validate`** merges a configuration directory as the agent does and refuses unknown keys. It does
  not resolve go-sockaddr templates, and a missing TLS file is only a warning.

## Decision

### NodeConfig and the agent configuration

1. **The region (maintainer decision 22).** NodeConfig gains `region`, the Nomad region, which `internal/app` sets
   from `spec.nomad.region`. `Validate` requires it and checks it with the rule of `spec.nomad.region`
   (`v1alpha1.RegionOK`: lower-case letters, digits and dashes, starting with a letter or a digit). It stays out of
   the spec hash: `00-tent.hcl`, which is in the hash, carries it already.
2. **`00-tent.hcl`** gains two settings on every role:
   - `disable_update_check = true`;
   - a `consul` block with `server_auto_join = false` and `client_auto_join = false`. With them off the agent does not
     look for Nomad servers in Consul. It still asks Consul otherwise: a client's Consul fingerprinter runs every
     15 s.

   `leave_on_terminate = true` stays on every role; whether servers keep it is decided in M2.7 (follow-ups). The
   goldens of the three roles change, and the spec hash keeps format 1: the hash covers the file's content, so every
   group gets a new hash; since no node runs NodeConfig before M2.7, no cluster rolls.
3. **No drain at shutdown** (maintainer decision 25, 2026-10-05). `00-tent.hcl` sets no `drain_on_shutdown` on client
   and combined nodes, so a client leaves without a drain when Nomad stops, and does not come back ineligible from a
   stop (Context, hashicorp/nomad#17093).
   - A restart of Nomad does not drain, and the tasks keep running through it (decision 4; seen on the M2.6b VM
     checks).
   - tent's own removals drain a client through the Nomad API first (ADR-0017).
   - tent does not support a `drain_on_shutdown` in `extraConfig.client`: it would leave the nodes ineligible.
4. **`nomad.service`.** tent renders it (`nodeconfig.RenderNomadService`, golden `nomad.service.golden`) as a
   NodeConfig file of every role: `/etc/systemd/system/nomad.service`, 0644, `root:root`, group-level and not secret,
   so it is in the spec hash, and a change of it rolls the group. Every node has the same unit. Compared with
   HashiCorp's stock unit:
   - `Type=notify`, as stock: `systemctl start` returns when Nomad sends `READY=1`, once the agent is set up, without
     waiting for a join or a leader.
   - `ExecStart=/usr/local/bin/nomad agent -config /etc/nomad.d`, where `nomad` puts the binary.
   - `ExecReload=/bin/kill -HUP $MAINPID`, as stock, so that `systemctl reload nomad` still works for an operator.
     tent-node never reloads Nomad.
   - `KillMode=process`, as stock: systemd signals the agent alone, so that the executor and logmon processes of
     running tasks, which stay in the unit's cgroup, survive a restart.
   - `KillSignal=SIGTERM`, where the stock unit has SIGINT: with `leave_on_terminate` the agent stops gracefully, and
     a server leaves the cluster.
   - No `TimeoutStopSec`: systemd's default of 90 s covers Nomad's 5-second graceful wait, since no client drains.
   - `Wants=` and `After=network-online.target`, as stock: the go-sockaddr templates need the private address.
   - `After=docker.service`, and nothing else on Docker. systemd stops units in the reverse order of their start, so
     at a shutdown Nomad stops before Docker, while the Docker daemon still answers it. The order does not cover the
     containers: live-restore keeps them through a stop or restart of Docker (Nomad's docker driver then stops them;
     Negative), and at a shutdown systemd stops their scope units on its own. A restart of Docker leaves Nomad
     running. On a node without Docker the order does nothing.
   - `Restart=on-failure`, `RestartSec=2`, `LimitNOFILE=65536`, `LimitNPROC=infinity`, `TasksMax=infinity` and
     `OOMScoreAdjust=-1000`, as stock.
   - Left out: `User=` and `Group=root`, the defaults, and `EnvironmentFile=-/etc/nomad.d/nomad.env`, since tent
     writes none.
   - **No `[Install]` section**, so it is never enabled and `systemctl is-enabled` prints `static`; `up` starts it
     (decision 10).
   - **The start timeout** stays systemd's default of 90 s. `00-tent.hcl` leaves out the cloud fingerprinters
     (`fingerprint.denylist`), which only slow the start on Vultr and Hetzner. A review of the source puts driver
     fingerprinting at about 55 s at worst.
   - No tent unit is ordered before `nomad.service`, and `nomad.service` is not ordered on `tent-node.service`. The
     ordering test of ADR-0028 item 7 covers `nomad.service` too, and checks that it has no `[Install]`.

### The `join` phase

5. **The known servers** are those in `/var/lib/tent/peers.json` (0600, in a 0700 directory), which holds the last
   answer, then the seed from NodeConfig's `join.servers`, without duplicates. A missing file is no error. A file that
   does not parse, or an entry that is not an address, is logged and left out.
6. **The question.** `join` asks only when the node's TLS files exist: on the first boot `nomad` writes them after
   `join`, so that boot renders the seed alone. It asks each known server in turn for
   `GET https://<address>:4646/v1/status/peers?stale` over tent-node's mTLS client (decision 13), with the TLS name
   `server.<region>.nomad`. It never asks `127.0.0.1`. On server and combined nodes the peers file holds the node's
   own address; `join` asks it at every boot and is refused, since Nomad is not running yet.
   - The first 200 with a JSON list of addresses wins. Their ports and duplicates are dropped and they are sorted, so
     the same set in another order changes nothing.
   - An empty list is no answer: a server answers `[]` before the bootstrap, and an empty set would replace the known
     servers with none.
   - `?stale` lets a server answer from its own Raft configuration without a leader.
   - On a failed call, another status, a list that does not parse or an empty list, `join` logs a warning with the
     server's address and the status or error, and asks the next server.
   - Only servers are asked: the seed and the answers hold only servers.
7. **The file.** With an answer, `join` renders the answer and keeps it in the peers file; without one, the known
   servers. On server and combined nodes `nodeconfig.RenderJoin` puts the addresses into
   `server { server_join { retry_join } }` with the Serf port 4648; on clients, into
   `client { server_join { retry_join } }` with the RPC port 4647. tent-node writes `/etc/nomad.d/05-join.hcl` (0644,
   `root:root`) through `FS.WriteFile` and never reads it back. `join` is `done` when the file, the peers file or a
   directory changed. The file sets no `retry_interval`, so Nomad's default holds: a try every 30 s, without a limit.

### The `nomad` phase

8. **The binary.** NodeConfig must carry the `nomad` asset, and the phase fetches it through the asset cache
   (ADR-0029 decision 17, as this ADR amends it). The phase opens the zip in its cache file through
   `FS.Open`, which reads at any offset, as `archive/zip` needs, so it holds neither the zip nor the binary whole. A
   download holds the zip in memory while it checks its sha256.
   - The binary is the one regular file named `nomad` at the top of the zip, and other entries, such as `LICENSE.txt`,
     are ignored. No `nomad`, two of them, one that is not a regular file, or one whose declared size is above
     256 MiB fails the phase before it writes the binary or any of Nomad's files. `archive/zip` fails a read past the
     declared size, or with a CRC-32 that does not match the header's.
   - The binary goes into `/usr/local/bin`, which the phase makes a 0755 directory owned by `root:root`.
   - The phase reads the entry once to hash it, and `FS.HasContent` compares its size, sha256, mode 0755 and owner
     `root:root` with `/usr/local/bin/nomad`, which it reads as a stream. An unchanged binary is not written.
   - Otherwise the phase writes the restart mark (decision 10) and reads the entry again into `FS.WriteStream`, which
     writes a temporary file beside the target while it hashes the copy, flushes it, renames it over the old file and
     flushes the directory. `WriteStream` and `WriteFile` first remove the regular files `.<base>.tmp*` beside the
     target, which a write killed by SIGKILL or a power loss leaves; a directory that cannot be listed fails the
     write.
9. **The files.** The phase makes `/etc/nomad.d` and `/etc/nomad.d/tls` (0755), `/var/lib/nomad` (0755) and, on client
   and combined nodes, `/var/lib/nomad/client` (0700) before the intro token goes into it. On client and combined
   nodes it renders `11-instance.hcl` with the instance id from the metadata service (`nodeconfig.RenderInstance`).
   It compares that file and every file of NodeConfig, `nomad.service` among them, with the one on disk through
   `FS.HasContent`, and writes each one whose content, mode or owner differs. A changed directory makes the phase
   `done` without a restart: Nomad reads no directory's mode or owner.
10. **systemd and the restart.**
    - `systemctl daemon-reload` runs only when `systemctl show -p NeedDaemonReload --value nomad.service` says yes, as
      in `install` (ADR-0028 item 11). A reload alone makes the phase `done` without a restart.
    - When Nomad is not active (anything but active or reloading: inactive, failed, activating or deactivating), the
      phase runs `systemctl start nomad.service`, which waits for `READY=1`. When it is active and the restart mark is
      there, the phase runs `systemctl restart nomad.service`; otherwise it runs neither. tent-node never enables it
      and looks for no queued job (Alternatives).
    - **The restart mark**, `/var/lib/tent/nomad-restart` (0600), records a change that the running Nomad has not
      read. The phase writes it before any write that needs a restart: of the binary, a NodeConfig file or
      `11-instance.hcl`. A failed mark fails the phase and leaves the file unwritten, so the next run writes it and
      restarts. A successful start or restart removes the mark. A failed one fails the phase and leaves the mark, so
      the next run starts or restarts Nomad again instead of finding nothing changed.
    - So Nomad restarts only after a change of its binary, of a NodeConfig file or of `11-instance.hcl`; a changed
      `05-join.hcl` never restarts it.
    - After a reboot Nomad is inactive, so `nomad` starts it and reports `done`, as `hostfirewall` does.

### `verify`

11. **The checks.** `verify` still checks that `tent-node.service` and `tent-node-join.timer` are enabled. Then it
    asks the node's own agent over tent-node's mTLS client at `https://127.0.0.1:4646`, with the TLS name
    `localhost`, which node certificates carry. The agent listens there on every role: a server on every address, a
    client on 127.0.0.1 and its private address.
    - Server and combined nodes: `GET /v1/status/leader?stale` (Context). A wait for a leader would hold each server's
      boot until a quorum has booted (ADR-0028 item 10).
    - Client and combined nodes: `GET /v1/agent/health?type=client`. A combined node's client knows its own server, so
      it is healthy at once.

    The checks run in this order, and each repeats until it answers 200 (decision 12). `verify` changes nothing and
    reports `unchanged`.
12. **Retries.** `verify` tries again 2 s later after a 500, which a client's health check answers until the client
    knows a server, or after a lost connection (refused, reset, or closed before the whole answer), as while systemd
    starts or restarts the agent.
    - After 60 failed tries in all, across the checks, about 2 minutes plus the time of the calls, the phase fails
      with `check the Nomad agent: not healthy after 60 tries: …`.
    - Everything else fails at once: another status, a TLS failure such as another cluster's certificate, a call that
      runs out its 5 s on a stuck agent, a missing TLS file.
    - The first failure logs one warning, `the Nomad agent is not healthy yet, trying again`, with the error, the wait
      and the number of tries. An error names the path and the status and quotes at most 200 characters of the answer,
      on one line, without characters that do not print.

### What the phases share

13. **tent-node's mTLS client** (`internal/nodeup/api.go`) serves `join`, `refresh-join` and `verify`. It uses
    `net/http` and `crypto/tls` alone.
    - TLS 1.2 or newer, the cluster's CA bundle from `/etc/nomad.d/tls/ca.pem` as the only roots, the node's
      certificate and key, and the TLS name of the call.
    - No proxy, whatever the environment says, and no redirects.
    - 5 s per call, the body included. An answer over 1 MiB fails. The client closes its idle connections when the
      phase is done with it.
    - Errors and logs never show the URL.
14. **The lock** between `up` and `refresh-join` (`nodeup.Lock`): an exclusive `flock` on `/run/tent-node.lock`,
    created with mode 0600 where it is missing.
    - `/run` is a tmpfs, so a reboot leaves no lock file behind, and the kernel frees the lock of a process that exits.
    - A caller that finds it held tries again every second until its context ends, and then fails with the context's
      cause.
    - `up` takes it after it has read NodeConfig, before the first phase, and holds it to the end. A second `up`
      waits until the first ends or its own context ends, as when systemd stops `tent-node.service` after 45 minutes.
    - `refresh-join` takes it after it has read NodeConfig (decision 15).
    - At boot the timer's job waits in systemd's queue behind `tent-node.service` (`After=`): `TimeoutStartSec`
      counts only from `ExecStart`, and a job waits without a limit. So at boot the timer's refresh does not wait for
      the lock. It waits only for a run by hand, and an `up` started by hand or by `systemctl start tent-node.service`
      waits for a running refresh.
    - It uses `syscall.Flock` on Linux and macOS. Elsewhere it does nothing, so the tests build on Windows; tent-node
      runs only on Linux, and `preflight` refuses anything else.

### `refresh-join`

15. **`refresh-join`** runs from `tent-node-join.timer` every minute (ADR-0028), and by hand.
    - It reads NodeConfig first; a config that cannot be read or decoded fails before the lock.
    - One deadline bounds the run: `RefreshTimeout`, 4m45s, 15 s less than the 5-minute `TimeoutStartSec` of
      `tent-node-join.service`, so it ends before systemd stops it. Within it, it waits for the lock at most 4 minutes
      (`RefreshLockWait`).
    - Without the TLS files it changes nothing. On server and combined nodes it asks the node's own agent first, at
      `https://127.0.0.1:4646/v1/status/peers?stale` with the TLS name `server.<region>.nomad`, which the node's
      certificate carries: the cluster's first server has an empty seed, so only its own agent can name its peers.
      Then it asks the servers of the peers file and the seed. A client asks the known servers only. The peers file
      never holds `127.0.0.1`.
    - The first non-empty answer wins, as in `join`. It writes `05-join.hcl` through `FS.WriteFile`, which leaves an
      unchanged rendering alone, and stores the peer set; without an answer it changes nothing.
    - It runs no command, never restarts Nomad and never touches the restart mark. When the deadline comes, it stops
      asking.
    - **Exit codes.** A refresh exits 0 when it succeeds, and when it changes nothing for want of TLS files, a known
      server or an answer. Of its failures, only the end of its own lock wait or deadline exits 0, with one info line,
      and the next refresh tries again; every other failure exits 1, even one after that deadline. SIGTERM ends the
      run from outside, and the error names the signal, such as
      `Error: lock /run/tent-node.lock: terminated signal received` or
      `Error: refresh 05-join.hcl: terminated signal received`. A wrong command line exits 2.
    - **Logs.** A refresh whose first server answers, and which changes nothing, logs nothing. Otherwise one of:
      - `05-join.hcl joins the servers that answered`, only when `05-join.hcl` changed, with `known` (the number of
        servers in the peers file and the seed), `servers` and `peers`; a change of the peers file alone logs nothing;
      - `no TLS files yet; 05-join.hcl stays until the next refresh`, `no server is known; …`, or
        `no server answered; …` with `asked`, the local agent included;
      - the warning `no mTLS client of the Nomad API; the known servers stay`, with no other line;
      - `another tent-node run holds the lock; the next refresh tries again` with `waited=4m0s`;
      - `the refresh ran out of time; the next refresh tries again` with `within=4m45s`.

      A server that does not answer adds a warning, as in `join`.

### Checks outside the node

16. **`nomad config validate` (maintainer decision 23).** `TestNomadConfigValidateOnline` lives in `internal/assets`
    (`nomadvalidate_test.go`), which imports `nodeconfig` already; the tests of `nodeconfig` may not import
    `internal/assets` or `internal/channels` (ADR-0026). Its name ends in `Online`, and it runs only with
    `TENT_TEST_ONLINE=1`: in the weekly `online` CI job and by hand, never on a pull request.
    - It takes the `stable` channel's minimum and recommended Nomad, 2.0.0 and 2.0.7 on 2026-10-02, once when they
      are equal. `internal/assets` resolves each for the system the test runs on and checks the signed `SHA256SUMS`;
      the test downloads the zip, checks its sha256 and unpacks `nomad`.
    - It builds one directory per role from the goldens of `internal/nodeconfig/testdata`: `<role>_<file>.golden`
      goes to that role as `<file>`, and `11-instance.hcl.golden` to client and combined nodes. `nomad config validate
      <dir>` must exit 0 for each. A server's directory with an extra file that sets `tent_unknown_key` must fail and
      name the key.
    - A server or combined node that joins a cluster of one has the joining form of `10-node.hcl` (no `server` block,
      [ADR-0038](0038-rolling-update-of-server-groups.md), item 10). `server_10-node.joining.hcl.golden` and
      `combined_10-node.joining.hcl.golden` take the place of `10-node.hcl` in a second directory of the role.
    - `TestNomadAgentFiles` runs on every pull request: a golden that no role takes, or a role without goldens, fails
      it, so a new golden must be given its roles.
    - It passed on darwin/arm64 on 2026-10-02 ([platform notes §1.6](../platform-notes.md#16-the-agent-on-a-node)).
17. **The VM check (maintainer decision 24).**
    - `internal/app` exports `NewNode` and `NodeConfigOf(ctx, NewNode)`, which build a new node's config with tent's
      own steps: `model.New`, the assets, the group's template and the node's parts. Its only own step is to refuse a
      missing Nomad version or group before any request. `update` must build a node's config the same way (M2.7).
    - `hack/tent-node-userdata` prints the user data of the only node of a cluster `tent-node-check`: group `nodes`,
      one `combined` node, validated with `AllowSingleServer`, client introduction `warn`, `bootstrap_expect = 1`, no
      seed and no intro token. Each run makes a throwaway CA, the node's certificate and a gossip key; the CA's key
      never leaves the tool. The secrets sit in the user data of a VM that is deleted after the check; tent scrubs
      nothing there, since no cluster owns the VM. Its flags: [README](../../hack/tent-node-userdata/README.md).
    - `hack/vultr-spike` v8 (`--only tentnode`) boots that node, reboots it and records the phases, Nomad's unit, a
      docker job across a restart of Docker (its container kept or replaced), of containerd and of Nomad and across
      the reboot, and `refresh-join`'s first mTLS call; it also records needrestart, `20auto-upgrades`,
      `debconf-show docker.io` and `docker.service`'s relations ([README](../../hack/vultr-spike/README.md)).

## Consequences

### Positive

- At every boot Nomad starts from `up`, after tent's host firewall, and the same run reports its health in
  `status.json`.
- A second `up` changes nothing: no download, no write of the binary, no restart.
- A changed binary, NodeConfig file or `11-instance.hcl` restarts Nomad once, even when the run that wrote it failed
  before the restart (decision 10).
- A rebooted client comes back eligible. A shutdown stops Nomad before Docker (both seen on the M2.6b VM checks).
- A node finds the servers it last saw after its seed is gone, and the cluster's first server learns its peers from
  its own agent.
- tent-node links no Nomad module.
- Each week the `online` job runs the goldens through `nomad config validate` of the oldest and the recommended Nomad
  that the channel allows.

### Negative / trade-offs

- A reboot kills a client's tasks without a migration. They come back when the client restarts them, if it is back
  first, or when the scheduler replaces them after the missed heartbeats.
- On server and combined nodes `join` logs `a server did not answer` for the node's own address at every boot.
- A failed removal of the mark costs one more restart, and so does a failed write after the mark.
- A change of a Nomad file's mode or owner alone, which only tampering makes, restarts Nomad.
- A client whose servers are not up within about 2 minutes fails `verify` and `up`, and on the first boot `install`
  and cloud-init, though its Nomad joins later.
- A server or combined agent that is not the leader of a cluster of several servers can exit 1 on a stop, since its
  removal from Raft can outlast Nomad's 5-second wait; the stop and a restart still complete
  ([platform notes §1.6](../platform-notes.md#16-the-agent-on-a-node)).
- A long `up` run by hand holds the lock, and a refresh that needs it gives up after 4 minutes.
- `join` asks the known servers one at a time, up to 5 s each, so a boot whose servers are all down waits up to 5 s per
  known server before it renders them.
- A restart of Docker restarts the node's docker tasks: Nomad's docker driver stops the containers that live-restore
  kept ([platform notes §6.4](../platform-notes.md#64-restarts-of-docker-containerd-and-nomad)). tent-node restarts
  Docker only when `daemon.json` differs from tent's, which on a running node takes a hand edit and an `up` by hand,
  since a change of NodeConfig replaces the node; that `up` restarts the docker tasks once. A crash of dockerd does
  the same, outside tent's control. Accepted on 2026-10-05.
- A refresh takes an answer without checking that it lists the server it asked, so a server that was removed from the
  cluster but still runs can keep a stale set on the nodes until tent deletes it.
- At every boot `nomad` reads the zip from the cache and hashes it, then hashes the binary in it and the one on disk:
  about 3 to 4 s on `vc2-1c-1gb` (by the timing, on the M2.6b VM checks).
- `nomad.service` keeps systemd's 90-second start timeout. A combined node started in 1.0 to 1.4 s on the M2.6b VM
  checks; a client alone was not measured.
- The validate test runs the release for the system it runs on, so a run on macOS only approximates the linux binary
  of the nodes.

### Follow-ups

- **The M2.6b VM checks** ran on 2026-10-05 on Vultr's Ubuntu 24.04 and 26.04 and passed but for one finding, the
  restart of Docker that Negative accepts ([platform notes §3.16](../platform-notes.md#316-spike-runs)). Their node
  asked only itself for peers; the peers call between two VMs waits for clusters of several nodes in M2.7 and E2E.
- **Node maintenance** (a later milestone,
  [platform notes §6.4](../platform-notes.md#64-restarts-of-docker-containerd-and-nomad)): unattended-upgrades
  upgrades docker.io from `-security` without restarting Docker, so a security fix of Docker takes effect only at
  the next reboot or restart of Docker; an upgrade of containerd restarts it, across a fleet within an hour, and
  leaves Nomad's tasks running (the M2.6b VM reruns); needrestart, on Vultr's images, restarts `nomad.service` after
  a `libc6` upgrade.
- The validate test has not run on Linux yet. The `online` job runs it there.
- **`leave_on_terminate` on server and combined agents:** decide with the server rollout in M2.7 (ADR-0017), from the
  facts in [platform notes §1.6](../platform-notes.md#16-the-agent-on-a-node).
- tent does not check `extraConfig.client` for `drain_on_shutdown`: a warning needs HCL parsing outside `api/` and
  `nodeconfig`. M2.8's `validate cluster` does not make it either ([ADR-0033](0033-operator-commands.md)).
- **M2.7:** `update` builds NodeConfig through `NodeConfigOf` or the same steps. The rollout creates clients after
  the servers are healthy and judges a client by its registration in Nomad. By the M2.6b VM checks (an inference
  from one combined node), a client registered 21 to 24 s after a fresh server's leadership and 20 to 23 s after a
  restart of the server's agent; the rollout's wait for registration must allow for that.
- **M3:** ADR-0016's E2E check that a client rejoins after every server has been replaced.

## Alternatives considered

- **The region from the node's certificate**, which names `server.<region>.nomad` or `client.<region>.nomad`. It is
  implicit, the names differ by role, and tent-node would depend on how tent names certificates. A validated field
  costs nothing.
- **`drain_on_shutdown`, with the node marking itself eligible again after its start**, through its own secret ID.
  Nomad no longer uses the secret ID as its primary authentication, and keeps it for servers that are not upgraded.
- **The same with a `node:write` token on every node.** `node:write` has no per-node scope: it covers the drain,
  eligibility, purge and meta of every node, the garbage collection of allocations and intro tokens.
- **The same with tent marking nodes eligible during its own runs.** A node that reboots between two runs of tent
  stays out of scheduling until the next one.
- **`internal/nomadops` in tent-node.** It would link `nomad/api` and its modules into tent-node, against the `go list`
  test (ADR-0028 item 19), for three GET calls.
- **Enabling `nomad.service` for boot.** Nomad would start before `up` loads tent's host firewall, so workloads could
  run without its rules, and before `up` writes the files of a changed NodeConfig.
- **`Before=nomad.service` on `tent-node.service`, with `up` starting Nomad without waiting.** ADR-0028 rejected it:
  `up` could not tell whether Nomad started, and `verify` could not check it.
- **`Delegate=yes`.** Nomad writes the root `cgroup.subtree_control` and makes `nomad.slice` itself.
- **Holding the binary in memory next to the zip:** about 200 MB on a node of 1 GB, while the rest of `up` runs.
- **`nomad config validate` on every pull request.** Every pull request would download Nomad from
  releases.hashicorp.com. The goldens change rarely, and the weekly job finds a drift within a week.
- **Looking for a queued job of `nomad.service`** (`systemctl list-jobs`, or `show -p Job` as `runtime` does for
  Docker). No boot queues one for a unit that is not enabled, and `systemctl start` waits for a job that someone else
  queued.
- **Stopping Nomad around tent-node's restart of Docker.** Logs written meanwhile would be missing from
  `nomad alloc logs`, a server agent would leave and rejoin Raft, the CNI check of the pause container is
  unverified, and the trigger is narrow: a hand edit of `daemon.json`.
- **Reading `05-join.hcl` back** for the last known servers. The peers file holds plain addresses, and tent-node needs
  no HCL parser.
