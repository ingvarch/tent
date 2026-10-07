# E2E suite

A black-box suite that builds Nomad clusters on Vultr with `tent`, checks them from outside and deletes them
([ADR-0034](../../docs/adr/0034-e2e-suite-on-vultr.md)). It runs from the maintainer's machine, since the Vultr
service user's key works only from the maintainer's two addresses. It is not part of `make check` and not part of CI.
`make check` runs only its unit tests, which need no cloud and carry no `e2e` build tag.

## What it does

The scenario `smoke` makes one cluster per image, `ubuntu-24.04` and `ubuntu-26.04`, in parallel. Each has 1 server and
1 client, so a run makes 4 machines. A cluster is named `e2e-<run>-<image digits>`, such as `e2e-k3x9qz-2404`. The
steps run in order, and the first that fails stops the cluster's steps:

1. **create:** `tent create cluster ... --yes` with the run's SSH key and the runner's address as the only address in
   `--ssh-access` and `--api-access`.
2. **validate:** `tent validate cluster --wait 5m` exits 0.
3. **export:** `tent export nomad`; the suite builds its Nomad client from the files it writes.
4. **service:** the job `e2e-web` runs, its check reports `success` and Nomad lists its service (a docker job with a
   service runs).
5. **metadata:** the batch job `e2e-metadata` has a task in each of three networks: Nomad's bridge, Docker's bridge and
   the host. Each task first reaches one of `deb.debian.org`, `detectportal.firefox.com` and `captive.apple.com`, so
   the network works, then times out on `169.254.169.254` (containers cannot reach the metadata endpoint). Three
   control sites of three owners, since `archive.ubuntu.com` did not answer at all during a run on 2026-10-07.
6. **intro token:** over SSH, a second Nomad agent, `e2e-rogue`, starts on the client with no intro token. The server
   logs the rejection, the agent logs `Permission denied`, and `/v1/nodes` never lists it (a client without an intro
   token is rejected).
7. **validate again:** `tent validate cluster --wait 5m` exits 0.
8. **delete:** `tent delete cluster --yes` exits 0, then the Vultr API lists no object of the cluster and
   `tent get clusters` prints `no clusters in` (`delete` leaves nothing behind).

Before it creates anything, the suite deletes the leftovers of earlier runs (see the janitor below).

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
`go test -tags e2e -count=1 -v -timeout 90m ./test/e2e`. A node refuses a tent-node of another version than its tent's,
so the suite stops at once when `TENT_NODE_SHA256` is not the sha256 of the tent-node next to `E2E_TENT`. Run the suite
with `make e2e` for that reason.

The timeout is 90 minutes, since a `go test` that times out runs no cleanup.

### Optional variables

| Variable | Default | Meaning |
|---|---|---|
| `E2E_REGION` | `ams` | the Vultr region; there is no automatic fallback, so pick another by hand after an incident |
| `E2E_PLAN` | `vc2-1c-1gb` | the machine type |
| `E2E_IMAGES` | `ubuntu-24.04,ubuntu-26.04` | the images, comma-separated, each of the form `ubuntu-NN.NN` |
| `E2E_KEEP` | empty | when set, the suite keeps the clusters, skips the delete step and the leak check, and prints how to delete them |
| `RUNNER_ADDR` | what `https://api.ipify.org` answers | the address that gets SSH and API access to the clusters |

## Results

Each run has an id of 6 characters and a directory `test/e2e/results/<run>/`, which git ignores and the suite never
deletes:

- `<cluster>-<step>.log`: one file for each tent command, with the arguments, stdout, stderr, the exit code and the
  time it took (steps `create`, `validate`, `export`, `validate-again`, `delete`, `get-clusters`, `cleanup-delete`);
- `<cluster>-intro-token.txt`: the record of the intro-token step;
- `state-<cluster>/`: the cluster's `file://` state store; it is empty after a delete;
- `ssh/`: the run's SSH key pair, with the private key at mode 0600;
- `xdg/`: the config and cache directories that tent used, so no config file of yours is read.

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

A kept cluster that nobody deletes goes when the janitor finds it older than 3 hours, in the next run or by hand.

## The janitor

The janitor deletes the objects of clusters whose name starts with `e2e-` and whose oldest object is older than 3 hours:
all the cluster's objects, instances first, then firewall groups, VPCs and SSH keys. It finds them by tent's tags and
markers. Objects that carry no tent marker, such as the account's own VPCs and SSH key, stay.

Without `--yes` it lists what it would delete and deletes nothing:

```fish
env VULTR_API_KEY=(<password manager command>) \
    go run ./hack/e2e-janitor
```

Add `--yes` to delete, and `--older-than 1h` to change the age. `--older-than 0` takes every E2E cluster, also one of a
run in progress.

## Cost and time

A run makes 4 machines of `vc2-1c-1gb`, and Vultr bills a machine for at least one hour. A green run took about 7
minutes on 2026-10-07: `create` 4.5 to 6.5 minutes, `delete` about 20 s, every other step seconds
([platform notes §3.16](../../docs/platform-notes.md#316-spike-runs)).
