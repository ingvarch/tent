# ADR-0042: Backups in the state store and the restore after a loss of the servers

- **Status:** Accepted
- **Date:** 2026-10-13
- **Deciders:** ingvarch
- **Related:** amends [ADR-0010](0010-state-store-and-locking.md), [ADR-0031](0031-bootstrap-in-update.md) and
  [ADR-0036](0036-nomad-calls-of-a-roll.md) (see their Status lines); builds on [ADR-0033](0033-operator-commands.md);
  [architecture §10.2](../architecture.md#102-layout), [§13.8](../architecture.md#138-backups),
  [§18](../architecture.md#18-open-questions) (decisions 46 and 47) and [platform notes
  §1.2](../platform-notes.md#12-features-tent-relies-on)  ## Context  A cluster that loses its servers loses Nomad's
  state: the jobs, the nodes, the ACL tokens, the variables and the keyring. M3.2 built the two snapshot calls
  ([ADR-0036](0036-nomad-calls-of-a-roll.md)). M3.8 needs the commands, and the maintainer decided two things on
  2026-10-09: `delete cluster` deletes the backups (46), and a restore goes only into a cluster whose servers were built
  again from the same state store (47).  Decision 47 rests on a run of 2026-10-09 against a local Nomad 2.0.7 ([platform
  notes §1.2](../platform-notes.md#12-features-tent-relies-on)). A restore into the running cluster answered 200 and
  then did harm: every ACL token made after the snapshot stopped working, the token of the call included; a client that
  registered after the snapshot was refused until its agent restarted; a dead node read `ready` for 5 minutes; and a
  purged job read `running`. A restore into new servers that were bootstrapped with the stored secret kept tent's
  access: the bootstrap secret worked, and the clients of the backup registered again within 7 seconds. A client that
  registered with the new servers before the restore was refused until its agent restarted, as in the running cluster
  (item 14).

## Decision

### The objects

1. **A backup is one object, `<cluster>/backups/<bootstrap>/<taken>.snap`, that holds Nomad's bytes unchanged.**
   `<taken>` and `<bootstrap>` are UTC times as `20261009T230207Z`: digits, `T` and `Z`, so each is a valid path
   segment on every backend and the names sort by time. The backup's name is `<taken>`, and `--from` takes it.
   `<bootstrap>` is the time in the bootstrap mark when the backup was taken. A snapshot is a secret, stored as the
   other secrets are ([architecture §10.3](../architecture.md#103-secrets-at-rest)); tent keeps no metadata beside it.
2. **The bootstrap is in the path so that tent tells a backup of the servers that run now from one of earlier servers by
   an equality test.** `update` writes a new mark when it bootstraps new servers and when the mark is missing (item 15),
   and a roll, a shrink and an upgrade keep it. A comparison of the backup's time with the mark's time would use two
   clocks and would misjudge a backup that waited for `update`'s lock.
3. **A second object, `<cluster>/nomad/restored`, records which backup the servers of one bootstrap are restored
   from.** It holds the backup, the bootstrap, the accessor of the servers' bootstrap token and, once the restore has
   ended, its time. tent never replaces a backup: a second one of a name fails on a store with conditional puts.

### `backup create`

4. **It takes the cluster's lock for the whole run.** The lock keeps a backup from being stored after `delete cluster`
   listed the state, which would leave Nomad's tokens in the store of a deleted cluster. It also keeps a backup out
   while `update` builds new servers, so every stored backup is of the servers whose mark its path names. It raises
   `tent-version`, as `update`, `rolling-update` and the spec commands do.
5. **It waits for a snapshot that Nomad cannot take yet.** After the servers change, Nomad answers 500 until its next
   Raft write, which came after 59 s in one run. tent asks every 2 s for up to 5 minutes, warns once, and then fails
   with a message that says to run the command again.

### `backup restore`: when it refuses

6. **The rules are R1 to R4, in this order. R1 to R3 read the state store and R4 asks Nomad.**
   - R1: the store holds the secrets and the bootstrap mark.
   - R2: the backup is of earlier servers, so one that `update` bootstrapped before the servers that run now.
   - R3: the record of this bootstrap names no other backup whose restore ended.
   - R4: Nomad lists no job in any namespace, unless the record shows that Nomad took the backup (item 12). A job
     that was stopped and not purged counts.
7. **R4 exists because the mark alone passes for a cluster in use.** Servers that were built again long ago and never
   restored have a new mark and no record, so every older backup stays earlier for good, and `latest` would do to that
   cluster what the run of 2026-10-09 shows.
8. **A restore goes once into the servers of a bootstrap (R3).** After a restore the cluster is in use, and another
   restore would wipe what it gained. To restore another backup, the operator deletes the server machines in the cloud
   and runs `update`. The maintainer kept this rule on 2026-10-13.
9. **`latest` takes the newest backup of earlier servers when all of them are of one bootstrap, and refuses
    otherwise.** It lists the newest backup of each earlier bootstrap and says to name one with `--from`. After a loss,
    a rebuild, a scheduled backup of the empty new Nomad and a second rebuild, the newest earlier backup is the empty
    one, and R3 would then block the real one. Backups of the servers that run now are never taken: after a loss a
    scheduled backup succeeds again as soon as `update` has built new servers.

### `backup restore`: the steps

10. **The restore runs under the lock, and the plan is made again under it.** Without `--yes` it prints the plan and
    changes nothing. Under the lock: read the backup, raise `tent-version`, read the accessor of the servers' bootstrap
    token, write the record without a time, send the snapshot, check tent's own access, and write the time.
11. **The record is written before the snapshot is sent,** so a run that is cut after Nomad took the backup can
    tell. Nomad then has the backup's jobs, and R4 would refuse the run that finishes the restore.
12. **A resumed restore applies R4 unless Nomad took the backup.** A run that finds a record of this backup without a
    time reads the accessor of the bootstrap token again. After a restore the bootstrap secret belongs to the token of
    the backup (measured on 2026-10-09), so another accessor than the record's means that Nomad took it: tent sends
    the snapshot again without R4. The same accessor means that it did not, and R4 applies. A record without an
    accessor applies R4. An answer of 403 ends the run with the lock-out error (item 13).
13. **After the restore, tent checks that the stored ACL bootstrap secret is still a management token.** Nomad
    answers 200 to a restore also when the token of the call is gone afterwards. tent sends one bootstrap request,
    which Nomad refuses with 400 whether the secret is a token or none (measured on 2026-10-10), and then reads
    `token/self`. A 403 or a token that is not a management token is a lock-out: the restore replaced every token, and
    the stored secret is none of them. The record stays without a time. The error says to delete the server machines,
    run `update` and restore with `--from NAME`, since `latest` takes the same object again. Only a file put under
    `backups/` by hand can cause it.
14. **tent does not wait for the nodes after a restore.** Nomad listed the backup's nodes as `ready` for about 5
    minutes whether they answered or not after a restore into a running cluster (an inference for new servers), and
    refuses a client that registered with the new servers before the restore.
    tent prints a notice, and the operator runs `validate cluster` and a forced roll.

### What the rules do not see

15. **R1 to R4 miss these cases:** a cluster in use that runs no job (its variables, ACL policies, tokens and node
    registrations are replaced); a mark that someone deletes by hand (the next `update` writes a new one for the same
    servers, and R4 refuses only while they run a job); a restore cut after Nomad took the backup (jobs that someone
    registered before the next run are replaced); and a backup of rebuilt servers taken before the restore, which holds
    an empty Nomad and is refused as current until a second loss, when item 9 applies.

### Reads, `delete cluster` and what is left out

16. **`nomadops` gains `Jobs` (`GET /v1/jobs?namespace=*`) and `TokenAccessor` (`GET /v1/acl/token/self`), and a class
    `ErrDenied` for a 403.** A 403 is permanent, so `Servers` returns it at once.
17. **`delete cluster` lists the backups and the record in its plan and deletes them (decision 46).** Another object
    under `backups/` is unknown and refused without `--force`.
18. **`backup list` reads only the state store,** so it works while the servers are gone.
19. **Left out of M3.8:** removing old backups (a lifecycle rule of the bucket or a delete of the objects does it), a
    restore inside `update`, a backup of a cluster without a leader, scheduled backups (the operator's cron or CI
    calls `backup create`), client-side encryption, and a stream in place of one snapshot in memory.

## Consequences

### Positive

- A cluster that lost its servers gets its jobs, variables and tokens back with one command after `update`, and a
  run that stops is finished by running the command again.
- It ran on Vultr on 2026-10-13: a cluster of one server and one worker, a new server with the old private address,
  a restore in 4 s, and the old worker `ready` at every poll for 7 minutes
  ([platform notes §3.16](../platform-notes.md#316-spike-runs)).

### Negative / trade-offs

- **One restore per bootstrap has a cost.** A second restore needs new servers. In a combined cluster every node is a
  server, so the operator deletes every machine and `update` builds them again.
- **A scheduled `backup create` raises `tent-version`.** The tent of a cron or CI job and the tent of the operator must
  be upgraded together, or the older one is refused.
- **After a restore that needed `--from`, repeating it needs the same `--from`,** since `latest` refuses across
  bootstraps.
- **A snapshot is held in memory,** and tent sets no limit. A snapshot of 2000 jobs was 305 KB.
- **Clients after a restore** need the operator: wait, validate, and roll the nodes that `validate cluster` reports
  (item 14).
- **A tent that is killed while the file backend writes leaves a hidden temp file** `.tent-tmp-*` with snapshot bytes,
  which `List` hides and `delete cluster` does not see. The same holds for the CA key.
- **On a store without conditional puts a second backup in the same second replaces the first.**
- **Not measured:** a snapshot of one Nomad version restored into another (`upgrade` makes it possible), the longest
  time Nomad refuses a snapshot after the servers change, a combined cluster of three, and the s3 backend with a real
  restore (CI runs the store's own tests on R2).

### Follow-ups

- M3.9's `ha` scenario ran a backup and a restore on Vultr in run `ae5dhs` (2026-10-14) with three servers: Vultr gave
  the rebuilt servers one of their old private addresses back, the workers came back on their own, `validate` passed
  after the 6-minute wait and no forced roll was needed ([ADR-0043](0043-e2e-scenarios-ha-and-upgrade.md)).
  `tent backup delete`, `backup create --keep N` and a restore inside `update` wait for issues.

## Alternatives considered

- **No lock for `backup create`.** A backup would work during a roll, but it could be stored after `delete cluster`
  listed the state, and it could be taken from servers whose mark `update` has not written yet.
- **A time comparison in place of the bootstrap in the path.** See item 2.
- **The accessor of the bootstrap token stored with each backup.** It tells the servers exactly, but needs a Nomad call
  and `backup list` would need Nomad.
- **For R4, a time window on the mark's age.** A cluster in use that was built again before the window passes, and a
  loss restored after the window is refused.
- **A warning in the plan with no refusal.** One `--yes` would then wipe a cluster in use.
- **No record, or a record written after the restore.** A cut between the restore and the record leaves no trace,
  and R4 refuses the next run, since the backup's jobs are there.
- **A record without a time that switches R4 off for good.** The first version did this: a failed send, jobs that the
  operator registered on the new servers, and a second run sent the backup over them. The accessor tells whether Nomad
  took the backup (item 12).
- **`latest` takes the newest earlier backup whatever its bootstrap.** See item 9.
- **A restore inside `update`.** It changes `update`'s Nomad step, and the clients that `update` builds in the same run
  register before the restore and are refused.
- **Progress events in place of a warning for the wait.** They would change the progress lines for one wait.
