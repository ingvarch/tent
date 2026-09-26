# Vultr spike

`spike.sh` checks the undocumented Vultr behaviour that tent's Vultr provider depends on. It runs against a real
account and writes a Markdown report. It covers the 🔬 items in [platform notes §3](../../docs/platform-notes.md#3-vultr)
and the **provisional** parts of [ADR-0018](../../docs/adr/0018-vultr-provider-design.md).

Run it once before starting milestone M1, and again whenever Vultr changes something relevant.

## What it checks

| Check (`--only`) | Question | Decides |
|---|---|---|
| preflight (always) | Region exists, plan deployable now, price, image `os_id` (public endpoints, no key) | availability preflight, cost |
| `boot` | Time to `active/running/ok`, to VPC IP, to port 22, to SSH login, to cloud-init done: A and B with package upgrades off (as tent does), V with no user_data (Vultr's vendor defaults). How soon `?tag=` lists a fresh instance | E2E timeouts; `package_upgrade: false`; search-before-retry |
| `inside` | Collected on the instance: ufw/firewalld/nftables rules, fail2ban and sshguard, `sshd -T`, `cloud-init analyze`, `systemd-analyze`, the vendor data (secrets redacted). Once from `runcmd` during first boot, once over SSH | the `hostfirewall` phase; where boot time goes |
| `metadata` | `/v1.json` fields, `instance-v2-id`, region code, interfaces; `/latest/user-data` reachability and size; a `write_files` payload in a 64 KiB user_data lands intact | `nodeup/env/vultr`; metadata block; user_data budget |
| `network` | VPC interface name, MTU, netplan config; A ↔ B ping over the VPC | interface matching, CNI MTU |
| `firewall` | Default ufw/firewalld state and whether it blocks `:4646` on the VPC. Whether a **firewall group filters VPC traffic**. How long rules take to propagate. | `FirewallCoversPrivate`; the `hostfirewall` phase |
| `alias` | Is an extra IP added inside the VPC (on the OS) reachable from another instance? | possible fixed slots on Vultr |
| `tags` | Accepted tag syntax (`/`, `=`, `:`, space, case, unicode), maximum length and count; `?tag=` exact or substring, case sensitivity; `?label=` matching | label codec (ADR-0018), server-side filtering |
| `markers` | Are `tent:cluster=…;kind=…` markers stored verbatim in VPC descriptions, SSH key names and firewall group descriptions? | ownership of non-instance resources |
| `userdata` | Maximum `user_data` size via PATCH (binary search) and on create; stored intact? | `MaxUserDataBytes` |
| `scrub` | After a user_data PATCH, does the metadata service serve the new value, and how fast? | `Nodes.ScrubUserData` |
| `halt` | Is `halt` graceful (ACPI) or a hard power-off? Does `start` work? Does cloud-init re-run after a PATCH plus restart? | `GracefulShutdown` (ADR-0017) |
| `objstore` (runs only if `S3_*` is set) | `If-None-Match: *` and `If-Match` on an existing bucket | state-store locking |

## Cost and duration

- **Instances.** At most 3 exist at a time, 4 in total with `userdata`, of the chosen plan: `vc2-1c-1gb` by
  default, $5 per month. Vultr bills at least 1 hour per instance, so a full run costs about **$0.03**.
- **Other resources.** One VPC (plus short-lived test VPCs for the mask check), one SSH key and one firewall group.
  They are free, and all are deleted on exit.
- **Duration.** 20–45 minutes, mostly waiting for boots, the halt/start cycle and instance V (vendor defaults).

## Prerequisites

- **Tools:**
  - bash 3.2+ (the macOS default is fine);
  - `curl`, `jq` 1.6+, `ssh`, `ssh-keygen`, `awk`, `base64`;
  - curl 7.75+ (`--aws-sigv4`) for the optional Object Storage check.
- **A Vultr API key in `VULTR_API_KEY`.**
  - Prefer a dedicated service user whose IAM policy allows compute instances, VPCs, firewalls and SSH keys.
  - If the key has an IP allow-list, include the machine you run from.
  - **Never paste the key into chat or commit it.** The script passes it to curl through a mode-0600 config file,
    never on the command line.
- **Account limits** must allow at least 3 concurrent instances. New Vultr accounts can have tiny limits, so request
  an increase first (Console → Billing → Limits). If the second or third instance fails, the report records the
  error and the checks that need it are skipped.
- **Reachability.** Your machine must reach the instances' public IPs on ports 22 and 4646. The firewall check probes
  port 4646 from your side.

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

# Object Storage conditional-write check only (existing bucket; no Vultr API key, no instances)
S3_ENDPOINT=https://ams1.vultrobjects.com S3_BUCKET=my-bucket \
S3_ACCESS_KEY=... S3_SECRET_KEY=... ./hack/vultr-spike/spike.sh --only objstore
```

Options: `--preflight`, `--dry-run`, `--yes`, `--keep`, `--only LIST`, `--region ID`, `--plan ID`, `--out DIR`.
Environment: `REGION`, `PLAN`, `OS_NAME` / `OS_ID`, `VPC_SUBNET`, `VPC_MASKS`, `READY_TIMEOUT`, `VENDOR_TIMEOUT`,
`USERDATA_TARGET`, `S3_*`. See `--help`.

## Output

The report is written to `hack/vultr-spike/results/vultr-spike-<UTC time>-<region>-<run>.md`. The directory is
git-ignored. It holds a summary table (check, result, design impact) and details: metadata without user data,
network configuration, host firewall rules, and the journal of the halted boot.

**After a run:**
1. Copy the findings into [`docs/platform-notes.md` §3](../../docs/platform-notes.md#3-vultr), replacing each 🔬 with
   the measured fact and the date.
2. Resolve the provisional items of [ADR-0018](../../docs/adr/0018-vultr-provider-design.md). Mark each confirmed item
   on the status line, or write a superseding ADR if a decision changes.
3. Tick the spike items in [`docs/roadmap.md`](../../docs/roadmap.md).

## SSH

Spike v1 lost SSH to its instances after a burst of connections, so v2 is careful:
- One multiplexed connection per host (`ControlMaster`); every command runs over it.
- `-F /dev/null`, `IdentitiesOnly=yes` and `IdentityAgent=none`: your ssh config and agent keys are not used.
- At most one new connection to port 22 per host every 15 s, which stays under ufw's `limit` (6 per 30 s).
- After 3 failed connections in a row a host is skipped, and its results read "unknown (ssh failed: reason)" instead
  of a negative result.

## Safety

- **Tagging.** Every instance is tagged `tent-spike-<run>`. VPCs, SSH keys and firewall groups carry
  `tent:cluster=tent-spike-<run>;…` markers. Created IDs are recorded as soon as they exist.
- **Cleanup on exit.** On any exit, including errors and Ctrl-C, the script writes the report and then deletes
  everything it created, in this order:
  1. instances, plus anything still tagged with the run;
  2. the firewall group;
  3. VPCs;
  4. the SSH key.

  The report records how long the API refused to delete the firewall group and the VPC after the instances were
  gone.
- **`--keep`** skips the deletion and prints the resources, so you can inspect them. Delete them yourself afterwards.
- **Manual cleanup.** If a run was killed with `kill -9`, find leftovers by the `tent-spike-` tag or description in
  the Vultr console.
- **No create retries.** The script never retries a failed create request. This is deliberate, because blind retries
  are how duplicates happen ([ADR-0015](../../docs/adr/0015-idempotency-without-unique-names.md)).
