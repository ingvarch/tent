# ADR-0043: E2E scenarios ha and upgrade

- **Status:** Accepted
- **Date:** 2026-10-14
- **Deciders:** ingvarch
- **Related:** amends [ADR-0034](0034-e2e-suite-on-vultr.md) (see its Status line); builds on
  [ADR-0038](0038-rolling-update-of-server-groups.md), [ADR-0039](0039-rolling-update-of-combined-groups.md),
  [ADR-0041](0041-upgrading-a-clusters-nomad-version.md) and
  [ADR-0042](0042-backups-in-the-state-store-and-restore-after-a-loss-of-the-servers.md);
  [architecture §15](../architecture.md#15-testing), [§18](../architecture.md#18-open-questions) (decisions 39 and
  51), [platform notes §3.16](../platform-notes.md#316-spike-runs)

## Context

The exit criterion of M3 in [the roadmap](../roadmap.md) is "E2E `ha` and `upgrade` pass on Vultr, with no quorum loss
and no job downtime beyond the configured migrations". The maintainer allows runs on a real cloud of at most ten
machines at once. Decision 39 puts `ha` on ubuntu-24.04 and `upgrade` on ubuntu-26.04 in one run, with the backup step
in `ha`.

[ADR-0034](0034-e2e-suite-on-vultr.md) made the suite a black box: it drives `bin/tent`, reads Vultr with its own client
and Nomad with its own mTLS client, and imports nothing under `internal/`. The scenarios keep that rule. Two things in
them are new. A roll runs for tens of minutes, so the suite needs a way to see a quorum loss or a job outage during the
roll. And `go test` panics on its timeout and runs no cleanup, while the step timeouts of a
long scenario add up to more than any `-timeout`.

The facts that the design rests on, from the source of Nomad 1.11.3 and from runs
([platform notes §1.6](../platform-notes.md#16-the-agent-on-a-node), [§3.16](../platform-notes.md#316-spike-runs)):

- A read of the leader without `stale=true` is forwarded, and Nomad holds it for up to 5 s before it answers `No
  cluster leader`.
- A read of an allocation's checks goes over the client's connection to a server. tent stops a server with a hard
  power-off. In the runs, the checks could not be read for up to 37 s after some of those stops (20.1 s in `ae5dhs` and
  7.1 s in `qkmdrs`, both in `upgrade`), and other stops caused no such poll. By a reading of the source, a client keeps
  that connection until its keepalive fails and its next heartbeat registers the node on another server.
- A client that restarts keeps its node ID, and Nomad's client logs `client: node registration complete` at INFO after
  each registration.

## Decision

1. **Three top-level tests, in this order.** `TestSmoke` keeps its steps and is not parallel. `TestHA` and
   `TestUpgrade` call `t.Parallel()` first. Go runs the tests that are not parallel first and releases the parallel
   ones after them, so a full run builds smoke's 4 machines and then `ha` and `upgrade` together: `ha` has 3 servers
   and 2 workers (6 during a roll or the forced roll), `upgrade` has 3 combined nodes (4 during its roll), so at most
   10 machines are up at once.
2. **Names and images.** The clusters are `e2e-<run>-ha` and `e2e-<run>-upgrade`; both fit the cluster name rule of
   `api/v1alpha1/validation.go`, and the janitor needs no change. `ha` takes the first image of `E2E_IMAGES` and
   `upgrade` the last: with the default list ubuntu-24.04 and ubuntu-26.04, with one image in the list both take it.
3. **Variables of the run.** `E2E_RUN`, when set, is the `-run` pattern that `make e2e` passes, for example
   `make e2e E2E_RUN='TestHA|TestUpgrade'`. `E2E_UPGRADE_FROM`, of the form `N.N.N`, is the Nomad version that `upgrade`
   builds on; it defaults to `2.0.6`. The suite does not know the channel: it reads the target version from the specs
   after `tent upgrade cluster`, and checks it against the version that the command prints.
4. **Timeouts.** `make e2e` passes `-timeout 180m`. A step's context also ends 20 minutes before the test's deadline
   (`cleanupReserve`: the delete of a cluster and a margin), so a step that runs long fails and the cleanups still
   delete the clusters. A `-timeout` under 20 minutes ends every step at once. The cleanup also deletes a cluster when
   the end of the step's context cut its delete step short. The janitor deletes a cluster once it is older than 4 hours,
   above the 3 hours of a run, so a sweep cannot take the clusters of a run that is near its end, whatever the clock
   skew between Vultr and the runner.
5. **The job.** `testdata/web-pair.nomad.hcl` is the job `e2e-web` with `count = 2` and a `migrate` block with Nomad's
   defaults written out (`max_parallel = 1`, `health_check = "checks"`, `min_healthy_time = "10s"`,
   `healthy_deadline = "5m"`), since the exit criterion speaks of the configured migrations. A drain stops one
   allocation, waits until its replacement's checks have passed for 10 s and then stops the next, so one allocation is
   healthy at every moment. Both scenarios submit it without a purge, so it stays until `delete cluster`.
6. **The probe.** While a roll runs, a probe polls every second. It lists the cluster's server machines (the combined
   machines in `upgrade`) through Vultr every 10 s (in a goroutine of its own, so a slow Vultr API never holds up a
   poll) and asks all of them at once:
   - **The leader**, with `GET /v1/status/leader?stale=true` and a 3 s timeout. With `stale=true` each server answers
     from its own Raft view at once. The outcome is `led` when any server names a leader, `leaderless` when at least one
     server answered and none named a leader, and `unreached` when no server answered. An empty answer never makes a
     poll `leaderless` while another server names a leader, since a server outside the quorum, such as a new one before
     it joins, answers an empty leader.
   - **The service**, through a server that named a leader or else one that answered, with a 7 s timeout (above Nomad's
     5 s hold of a forwarded read): the allocations of the job, the checks of each running allocation and the
     registrations of the service. The outcome is `served` when one running allocation has a successful check of the
     service and a registration. It is `unserved` when the reads answered and show no running allocation, a running
     allocation without a registration, or no healthy allocation among the checks that answered. It is `unreached` when
     no server answered the leader read, or when the allocations or the registrations could not be read. It is
     `unchecked` when those answered, a running allocation has a registration, and every checks read failed.
   - **The verdict** fails the step when:
     - `leaderless` lasts more than 10 s (a transfer takes about 1 s, a lost quorum lasts until a person acts);
     - `unserved` lasts more than 10 s (two check intervals of the job);
     - `unreached` lasts more than 30 s, for the leader or the service;
     - `unchecked` lasts more than 60 s (a stretch of 37.2 s after a hard stop of a server in run `w28exz`, with a
       margin);
     - two polls start more than 30 s apart (the probe itself stalled, so it saw nothing);
     - the stretches of one bad outcome add up to more than twice its limit in one roll (`leaderless` and `unserved`
       20 s, `unreached` 60 s, `unchecked` 120 s), which catches a leader that is lost every other second;
     - there are fewer than 10 polls, or the first and the last poll are not both `led` and `served` (the probe started
       too late or stopped too early).
     A stretch lasts from the first poll of an outcome to the start of the next poll with another outcome. The
     summary of every verdict, passing or not, is logged with the count of each outcome, the longest stretch and the
     total time of each, and the longest gap between polls.
   - **The end of a roll.** tent exits 40 to 47 s after the last hard stop of a server, and the checks may stay
     unreadable for up to 37 s after it. When tent exits 0, the probe goes on polling until a poll is `led` and
     `served`, for at most the roll's longest stretch limit (60 s, or 30 s in the worker roll of `ha`), and the log
     says how long it polled. A stretch that is still open then is judged against its own limit, and a roll whose
     probe never got a good poll back fails on the last-poll rule.
   - **The limits of each roll.** The server roll of `ha` and the roll of `upgrade`, which stops combined nodes, use
     the limits above. The worker roll of `ha` stops no server, so an `unchecked` poll there means that no client that
     runs an allocation answers while Nomad still lists them: its `unchecked` limit is the `unserved` one (10 s, 20 s
     in all).
   - **The first poll** uses the server addresses that the step listed and checked before it started the probe, so a
     failed list at the start cannot fail the first poll.
   - **The records** go to `results/<run>/<cluster>-probe-<roll>.tsv` as they come, so a run that is cut short keeps
     them.
7. **`ha`, step by step.**
   1. `create` with 3 servers and 2 workers; `validate`; `export`; `service` (the pair job, two allocations running and
      the service healthy).
   2. `change`: `tent get clusters -o json`, then a document with `spec.nomad.extraConfig` added (a comment for the
      servers and a `client { meta { e2e_roll = "<run>" } }` block for the workers), `tent replace -f`. The change moves
      the spec hash of every machine, and the new workers carry the run id in their node meta.
   3. `update`, then `rolling-update --exit-code` exits 2.
   4. `roll-servers`: `rolling-update --yes --nodegroups servers` (timeout 60 minutes) with the probe; afterwards Vultr
      lists no old server, 3 servers and the old workers.
   5. `restart-client`: export again, then the old worker with the lowest label restarts over SSH
      (`systemd-run --on-active=2 --unit=e2e-reboot systemctl reboot`) and must come back with a new boot ID, the same
      node ID, the line
      `client: node registration complete` in the journal of its new boot, `ready` and `eligible` in Nomad, and the
      service served. The worker was built with the old servers, so its seed is dead and only what `refresh-join` wrote
      leads it to the new ones; the record holds `05-join.hcl`.
   6. `roll-workers`: `rolling-update --yes` (timeout 45 minutes) with the probe; `rolling-update --exit-code` exits 0;
      Vultr lists no old worker; every worker's node has `e2e_roll` set to the run id.
   7. `validate-again`.
   8. `backup`: `backup create`, `backup list` (one backup, of the servers that run now); the suite deletes the
      cluster's server machines through the Vultr API (only machines tagged with the cluster and the role `server`, and
      no empty result); `update` builds new servers; the backup is `earlier`; `backup restore`; the job `e2e-web`
      answers; the suite waits 6 minutes, since Nomad lists the backup's nodes as ready for about 5 minutes whether
      they answer or not; `validate cluster` runs, and only when it fails, `rolling-update --yes --force --nodegroups
      workers` and `validate --wait 5m` run; finally two allocations run and the service is healthy.
   9. `delete` and the leak check, unless `E2E_KEEP` is set.

   The roll is in two runs for #106: a roll of the whole cluster replaces the workers too, and a worker built with the
   new servers would not test a dead seed.
8. **`upgrade`, step by step.** `create` with `--combined --servers 3 --nomad-version <E2E_UPGRADE_FROM>` (the warning
   that the version is untested is logged and does not fail the step); `validate`; `export`; `service`;
   `versions-before` (3 nodes on the old version); `upgrade` (`tent upgrade cluster --yes` with a timeout of 5 minutes,
   since it downloads and verifies the release's signed checksums; the target read from the specs, which must differ
   from the old version and be the version that the command prints; then `update` and `rolling-update --exit-code`
   exits 2); `roll` with the probe (timeout 60 minutes), then exit code 0; `versions-after` (3 nodes on the target,
   `ready` and `eligible`; 3 alive members named after the current machines with the target in their `build` tag);
   `validate-again` (exit 0, and the output names the target); `delete` and the leak check.
9. **Records.** Each run writes these files into `results/<run>/` beside the logs of ADR-0034:
   `<cluster>-roll-change.json`, `<cluster>-probe-servers.tsv` and `<cluster>-probe-workers.tsv` (`ha`),
   `<cluster>-probe-nodes.tsv` (`upgrade`), `<cluster>-restart.txt` and `<cluster>-backup.txt` (`ha`). A probe file has
   the columns `start`, `took_ms`, `leader`, `leader_address`, `service` and `detail`.

## Consequences

### Positive

- The exit criterion is checked at every poll of two rolls and a restore, on two images, in one run.
- A run that fails leaves the probe's records, so the gap that failed it can be read.
- A step that runs past its time fails while the cleanups can still delete the clusters.

### Negative / trade-offs

- A run of `ha` and `upgrade` took 39 minutes (run `ae5dhs`): `ha` ran create 7 min 43 s, the
  server roll 12 min 14 s, the worker roll 5 min 45 s and the backup step 9 min 36 s (6 minutes of it the wait);
  `upgrade` ran create 4 min 2 s and the roll 14 min 46 s. The full run `qkmdrs`, with `smoke` first, took about 45
  minutes. Vultr bills each machine for
  at least one hour; the plan's estimate was under 2 US dollars for a run.
- The probe cannot read a check for up to 37 s after a hard stop of a server. In those polls it knows that the
  allocations run and are registered, and counts the poll `unchecked`, with a limit of 60 s (120 s in all) in the rolls
  that stop a server. A job that fails its checks in that window would pass.
- The suite depends on Nomad's text `client: node registration complete`. A release that changes it fails the
  `restart-client` step.
- The probe asks the servers' public addresses, which the run's `--api-access` opens to the runner. It cannot see a
  fault that only shows on the service's own port.

### Follow-ups

- Runs:
  - `w28exz` (2026-10-14, `ha` and `upgrade`) found two faults of the suite, both fixed: Vultr's firewall list gave a
    creation date without a zone, which failed the leak check, and the probe counted polls without readable checks as
    `unreached` (see Alternatives).
  - `ae5dhs` (2026-10-14, `ha` and `upgrade`): both passed. The probe saw no `leaderless` and no `unserved` poll; the
    longest `unchecked` stretch was 20.1 s and the longest `unreached` stretch 7.1 s.
  - `qkmdrs` (2026-10-14, the full `make e2e`: `smoke`, `ha` and `upgrade`): every scenario passed. The probes saw no
    `leaderless`, `unserved` or `unreached` poll; the longest `unchecked` stretch was 7.1 s.
  - `hik34s` (2026-10-14, the full `make e2e` with the fixes of the final review): every scenario passed, and every
    poll of every probe was `led` and `served`.
- The channel lists 2.0.6 as tested since `ae5dhs` ([ADR-0041](0041-upgrading-a-clusters-nomad-version.md)).
- Hetzner E2E and runs in CI wait as [architecture §15](../architecture.md#15-testing) says.

## Alternatives considered

- **A probe of the service's port from outside.** The cluster's firewall opens no job port to the runner, and opening
  one would change what the cluster under test is. The probe reads the service through Nomad's API instead.
- **`rolling-update --force` in place of a change of the specs.** `--force` replaces machines whose specs did not
  change, so no roll would be due from a changed spec, and no new worker would carry a value that the suite can read
  back. The change of `extraConfig` gives both.
- **A reboot through Vultr's API.** The step tests that the agent finds the new servers after a boot of its own
  machine. A reboot over SSH needs only the machine and the runner's SSH access, which the suite already has; Vultr's
  API adds a call whose timing is not documented.
- **One roll of the whole cluster with a restart of a new worker.** A worker built with the new servers has the new
  servers as its seed, so it would come back even if `refresh-join` never wrote anything. #106 asks for a worker whose
  seed is dead, which needs the two rolls.
- **Counting a failed checks read as `unreached` with a limit of 30 s.** Run `w28exz` failed on it: after a hard stop of
  a server, one allocations read through that server timed out (7.1 s), then every checks read failed for 30.2 s while
  the allocations and registrations answered and both allocations ran, one `unreached` stretch of 37.2 s in all. The
  outcome `unchecked` keeps a poll that cannot read the checks apart from a poll that reached nothing.
