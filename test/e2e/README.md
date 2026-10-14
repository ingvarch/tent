# E2E suite

A black-box suite that builds Nomad clusters on Vultr with `tent`, checks them from outside and deletes them
([ADR-0034](../../docs/adr/0034-e2e-suite-on-vultr.md)). It runs from the maintainer's machine, since the Vultr
service user's key works only from the maintainer's two addresses. It is not part of `make check` and not part of CI.
`make check` runs only its unit tests, which need no cloud and carry no `e2e` build tag.

## What it does

A run has three scenarios, in this order. `smoke` runs first. `ha` and `upgrade` run after it, in parallel with each
other, so a run has at most 10 machines at once: 4 for `smoke`, then up to 6 for `ha` and 4 for `upgrade`. A cluster of
`ha` or `upgrade` is named `e2e-<run>-<scenario>`, and before it creates anything the suite deletes the leftovers of
earlier runs (see the janitor below). In each scenario the first step that fails stops the cluster's steps, and the
cleanup deletes the cluster.

### `smoke`

The scenario makes one cluster per image, `ubuntu-24.04` and `ubuntu-26.04`, in parallel. Each has 1 server and 1
client. A cluster is named `e2e-<run>-<image digits>`, such as `e2e-k3x9qz-2404`. The steps:

1. **create:** `tent create cluster ... --yes` with the run's SSH key and the runner's address as the only address in
   `--ssh-access` and `--api-access`.
2. **validate:** `tent validate cluster --wait 5m --allow-single-server` exits 0.
3. **export:** `tent export nomad`; the suite builds its Nomad client from the files it writes.
4. **service:** the job `e2e-web` runs, its check reports `success` and Nomad lists its service (a docker job with a
   service runs).
5. **metadata:** the batch job `e2e-metadata` has a task in each of three networks: Nomad's bridge, Docker's bridge and
   the host. Each task first reaches one of `deb.debian.org`, `detectportal.firefox.com` and `captive.apple.com`, so
   the network works, then times out on `169.254.169.254` (containers cannot reach the metadata endpoint). Three
   control sites of three owners, since `archive.ubuntu.com` did not answer over IPv4 during a run on 2026-10-07.
6. **intro token:** over SSH, a second Nomad agent, `e2e-rogue`, starts on the client with no intro token. The server
   logs the rejection, the agent logs `Permission denied`, and `/v1/nodes` never lists it (a client without an intro
   token is rejected).
7. **validate again:** `tent validate cluster --wait 5m --allow-single-server` exits 0.
8. **delete:** `tent delete cluster --yes` exits 0, then the Vultr API lists no object of the cluster and
   `tent get clusters` prints `no clusters in` (`delete` leaves nothing behind).

### `ha`

The cluster `e2e-<run>-ha` has 3 servers and 2 workers on the first image of `E2E_IMAGES` and runs the job
`e2e-web` with two allocations. The steps:

1. **create, validate, export, service:** as in `smoke`, with `--servers 3 --workers 2`, and the job `e2e-web` with two
   allocations running and its service healthy.
2. **change:** `tent replace` adds an extra Nomad configuration to the specs (a `client` meta value `e2e_roll` with the
   run's id), so every machine is outdated. **update** applies it, and `tent rolling-update cluster --exit-code`
   exits 2.
3. **roll servers:** `tent rolling-update cluster --yes --nodegroups servers` while a probe polls the servers for a
   leader and the service for a healthy allocation (see below). Afterwards Vultr lists 3 new servers and the 2 old
   workers.
4. **restart client:** the old worker with the lowest name restarts over SSH. It must boot again, register with the
   same node ID (the journal of the new boot holds `client: node registration complete`), be `ready` and `eligible`,
   and the service must be served. Its seed servers are dead by now, so only what `refresh-join` wrote leads it to the
   new servers.
5. **roll workers:** `tent rolling-update cluster --yes` with the probe, then `--exit-code` exits 0, Vultr lists only
   new workers, and each worker's node has `e2e_roll` set to the run's id.
6. **validate again.**
7. **backup:** `tent backup create`, then the suite deletes the cluster's 3 server machines through the Vultr API,
   `tent update cluster --yes` builds new ones, and `tent backup restore --yes` restores the backup. The suite waits 6
   minutes, since Nomad lists the backup's nodes as ready for about 5 minutes whether they answer or not, and runs
   `validate cluster`. Only when that fails, it rolls the workers with `--force` and validates again. At the end two
   allocations run and the service is healthy.
8. **delete** and the leak check.

### `upgrade`

The cluster `e2e-<run>-upgrade` has 3 combined nodes on the last image of `E2E_IMAGES`, on the Nomad version of
`E2E_UPGRADE_FROM`. The steps:

1. **create, validate, export, service:** as in `ha`, with `--combined --servers 3 --nomad-version <old>`. A warning
   that the old version is untested is logged and does not fail the step.
2. **versions before:** every node runs the old version.
3. **upgrade:** `tent upgrade cluster --yes` moves the spec to the version that tent's channel recommends, which must
   differ from the old one; `update` applies it, and `rolling-update --exit-code` exits 2.
4. **roll:** `tent rolling-update cluster --yes` with the probe, then `--exit-code` exits 0.
5. **versions after:** Nomad lists 3 nodes on the new version, all `ready` and `eligible`, with 3 `alive` members that
   carry the new version, and Vultr lists only new machines.
6. **validate again:** `tent validate cluster` exits 0 and names the new version.
7. **delete** and the leak check.

### The probe

During each roll a probe polls once a second. It asks every server for the leader (`/v1/status/leader?stale=true`) and
reads the job's allocations, their checks and the service's registrations. A poll counts as `led` or `leaderless` or
`unreached`, and as `served`, `unserved`, `unchecked` (the checks could not be read, which happened for up to 37 s
after a server stopped hard, while the allocations ran and were registered) or `unreached`. The step fails when:

- `leaderless` or `unserved` lasts more than 10 s, `unreached` more than 30 s or `unchecked` more than 60 s;
- one of these outcomes lasts more than twice its limit in all during one roll;
- two polls start more than 30 s apart;
- there are fewer than 10 polls, or the first or the last poll is not both `led` and `served`.

The worker roll of `ha` stops no server, so its `unchecked` limit is 10 s, like `unserved`. When tent exits 0, the
probe goes on polling until a poll is `led` and `served`, for at most the roll's longest limit: 60 s, or 30 s in the
worker roll of `ha`.

The reasons for each limit are in [ADR-0043](../../docs/adr/0043-e2e-scenarios-ha-and-upgrade.md).

## Run it

You need `go`, `ssh` and `ssh-keygen`, and these:

- `VULTR_API_KEY`, the service user's key. Read it inside the one command, so it stays out of the shell's variables and
  history.
- `TENT_DEV_S3_URL`, the URL of the development prefix of the CI bucket.
- `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`, your R2 token's keys, which `make dev-upload` uses
  ([setup](../../hack/tent-node-upload/README.md)). Set them from your password manager, not on the command line.

In fish:

```fish
set -x TENT_DEV_S3_URL (gh variable get TENT_TEST_S3_URL | string replace '/ci?' '/dev?')
set -x AWS_ACCESS_KEY_ID (<password manager command>)
set -x AWS_SECRET_ACCESS_KEY (<password manager command>)
env VULTR_API_KEY=(<password manager command>) \
    make e2e
```

`make e2e` builds `bin/tent` and `bin/tent-node_linux_amd64` with one version, uploads the tent-node (`make
dev-upload`), sets `TENT_NODE_URL`, `TENT_NODE_SHA256` and `E2E_TENT` (the absolute path of `bin/tent`) and runs
`go test -tags e2e -count=1 -v -timeout 180m ./test/e2e`, with `-run "$(E2E_RUN)"` added when `E2E_RUN` is set. A node
refuses a tent-node of another version than its tent's, so the suite stops at once when `TENT_NODE_SHA256` is not the
sha256 of the tent-node next to `E2E_TENT`. Run the suite with `make e2e` for that reason.

To run some scenarios, set `E2E_RUN` to a `go test -run` pattern:

```fish
env VULTR_API_KEY=(<password manager command>) \
    make e2e E2E_RUN='TestHA|TestUpgrade'
```

`make -n e2e` is not a dry run: the recipe calls `$(MAKE)`, so `dev-upload` runs.

The timeout is 180 minutes, since a `go test` that times out runs no cleanup. The context of every step ends 20 minutes
before `go test`'s deadline, so a step that runs long fails and the cleanups still have time to delete the clusters.
Run the suite through `make e2e` for that too: a `-timeout` under 20 minutes (go test's default of 10 included) ends
every step at once, and `-timeout 0` sets no end. The cleanup also deletes a cluster when the end of the step's context
cut its delete step short.

Ctrl-C or SIGTERM ends the running step within seconds; the suite prints `e2e: interrupted; deleting the clusters of
this run` and its cleanups delete the clusters (unless `E2E_KEEP` is set) and the exported files. Ctrl-C reaches the
test process through the terminal; send SIGTERM to the `e2e.test` process itself, since `make` and `go test` do not
pass it on.

### Optional variables

| Variable | Default | Meaning |
|---|---|---|
| `E2E_REGION` | `ams` | the Vultr region; there is no automatic fallback, so pick another by hand after an incident |
| `E2E_PLAN` | `vc2-1c-1gb` | the machine type |
| `E2E_IMAGES` | `ubuntu-24.04,ubuntu-26.04` | the images, comma-separated, each of the form `ubuntu-NN.NN`; `ha` takes the first and `upgrade` the last |
| `E2E_RUN` | empty | the `go test -run` pattern that `make e2e` passes, such as `TestHA\|TestUpgrade`; empty runs every scenario |
| `E2E_UPGRADE_FROM` | `2.0.6` | the Nomad version that `upgrade` builds its cluster on, of the form `N.N.N` |
| `E2E_KEEP` | empty | when set, the suite keeps the clusters, skips the delete step and the leak check, and prints how to delete them |
| `RUNNER_ADDR` | what `https://api.ipify.org` answers | the address that gets SSH and API access to the clusters |

## Results

Each run has an id of 6 characters and a directory `test/e2e/results/<run>/`, which git ignores and the suite never
deletes:

- `<cluster>-<step>.log`: one file for each tent command, with the arguments, stdout, stderr, the exit code and the
  time it took (`smoke` has the steps `create`, `validate`, `export`, `validate-again`, `delete`, `get-clusters` and
  `cleanup-delete`; `ha` and `upgrade` add steps such as `roll-servers`, `roll-workers`, `roll`, `roll-due`,
  `roll-done`, `upgrade`, `backup-create`, `backup-restore` and an `export-...` after each roll);
- `<cluster>-intro-token.txt`: the record of the intro-token step;
- `<cluster>-roll-change.json`: the specs that `ha` replaced, with the extra Nomad configuration;
- `<cluster>-probe-servers.tsv` and `<cluster>-probe-workers.tsv` (`ha`), `<cluster>-probe-nodes.tsv` (`upgrade`): the
  probe's polls, one line each, written as they come, with the columns `start` (UTC), `took_ms`, `leader` (`led`,
  `leaderless` or `unreached`), `leader_address`, `service` (`served`, `unserved`, `unchecked` or `unreached`) and
  `detail`;
- `<cluster>-restart.txt` (`ha`): the machine, the node ID, the boot IDs before and after, the waits, the registration
  lines of the new boot and the content of `/etc/nomad.d/05-join.hcl`;
- `<cluster>-backup.txt` (`ha`): the backup's name and size, the time of each step, the private addresses of the
  servers and workers before and after, and whether the forced roll ran;
- `state-<cluster>/`: the cluster's `file://` state store; after a delete it holds no cluster, only tent's lock
  files;
- `ssh/`: the run's SSH key pair, with the private key at mode 0600;
- `xdg/`: tent's `XDG_CONFIG_HOME` and `XDG_CACHE_HOME` point at `xdg/config` and `xdg/cache`, so no config file of
  yours is read; the directory exists only if tent writes there.

The Nomad files of `tent export nomad` are in `export-<cluster>/` while a cluster's steps run, and the suite removes the
directory when they end.

## Keep a cluster

With `E2E_KEEP` set, the suite logs `keeping cluster <name>; delete it with: ...` for each cluster. Run that command
with `VULTR_API_KEY` set, such as:

```fish
env VULTR_API_KEY=(<password manager command>) \
    (pwd)/bin/tent delete cluster e2e-k3x9qz-2404 --yes \
    --state file://(pwd)/test/e2e/results/k3x9qz/state-e2e-k3x9qz-2404
```

A kept cluster that nobody deletes goes when the janitor finds it older than 4 hours, in the next run or by hand.

## The janitor

The janitor deletes the objects of clusters whose name starts with `e2e-` and whose oldest object is older than 4 hours:
all the cluster's objects, instances first, then firewall groups, VPCs and SSH keys. It finds them by tent's tags and
markers. Objects that carry no tent marker, such as the account's own SSH key `main`, stay.

Without `--yes` it lists what it would delete and deletes nothing:

```fish
env VULTR_API_KEY=(<password manager command>) \
    go run ./hack/e2e-janitor
```

The janitor reads a creation date that Vultr gives without a zone as US Eastern time: on 2026-10-14 the firewall list
gave `2026-10-13 20:46:46` for a group made at 00:46:46 UTC.

Add `--yes` to delete, and `--older-than 1h` to change the age. `--older-than 0` takes every E2E cluster, also one of a
run in progress.

## Cost and time

Vultr bills a machine for at least one hour, and the machines are `vc2-1c-1gb`. `smoke` makes 4 machines and a green
run took 6 to 7 minutes on 2026-10-07: `create` 4 to 6.5 minutes, `delete` about 20 s, the leak check up to 2 minutes on
the cluster whose check runs while the other is still being built, every other step seconds. In run `ae5dhs`
(2026-10-14) `ha` and `upgrade` took about 40 minutes together, with at most 10 machines: `ha` ran `create` 7 min 43 s,
the server roll 12 min 14 s, the restart of a worker 2 min 35 s, the worker roll 5 min 45 s and the backup step 9 min 36
s (6 minutes of it the wait); `upgrade` ran `create` 4 min 2 s and the roll 14 min 46 s; each `delete` took under 25 s
([platform notes §3.16](../../docs/platform-notes.md#316-spike-runs)).
