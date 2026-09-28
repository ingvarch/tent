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
| `sshdup` (no instance) | Does Vultr accept a second SSH key with the same key material under another name? | a second cluster with the same operator key |
| `lengths` (no instance) | The longest VPC description, firewall group description and SSH key name stored verbatim: marker-like texts of 64 to 512 characters, updated in place as govultr does it | markers with `op` reach 99 characters |
| `rules` (no instance) | How Vultr lists firewall rules sent in tent's form (`ip_type`, `protocol`, `subnet`, `subnet_size`, `port`), and whether it takes the same rule twice | rule comparison in the firewall task |
| `fwinuse` | Vultr's answer to the delete of a firewall group that instance A uses; the group's `instance_count` in `GET /firewalls` and `GET /firewalls/{id}` while A uses it, read for up to 30 s | which answers tent retries as `ErrInUse`; the firewall group dedupe, which keeps the group with the most instances (ADR-0023) |
| `patchtags` | Does the instance PATCH that govultr sends for a user_data or firewall group change (`"tags": null`, `"ddos_protection": null`) clear the tags? | scrub and firewall PATCHes must resend the tags? |
| `vpcpending` | What `GET /instances/{id}/vpcs` answers while instance A boots, from right after the create answer until it lists an address other than `0.0.0.0` (at most 120 s): each distinct answer (HTTP status, error text or addresses, and A's status) with the seconds since the create | whether tent's List may read a 404 on `/vpcs` as a deleted instance |
| `halttwice` | Halt A, wait until it is stopped (at most 60 s), halt it again: both answers. Runs last and leaves A stopped | whether `Stop` fails when Go's HTTP/2 client sends the bodyless halt twice |
| `objstore` (runs only if `S3_*` is set) | `If-None-Match: *` and `If-Match` on an existing bucket | state-store locking |

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

Options: `--preflight`, `--dry-run`, `--yes`, `--keep`, `--only LIST`, `--region ID`, `--plan ID`, `--out DIR`.
Environment: `REGION`, `PLAN`, `OS_NAME` / `OS_ID`, `VPC_SUBNET`, `VPC_MASKS`, `READY_TIMEOUT`, `VENDOR_TIMEOUT`,
`USERDATA_TARGET`, `S3_*`. See `--help`.

## Output

The report is written to `hack/vultr-spike/results/vultr-spike-<UTC time>-<region>-<run>.md`. The directory is
git-ignored. It holds a summary table (check, result, design impact) and details: metadata without user data,
network configuration, host firewall rules, the journal of the halted boot, the `/vpcs` answers while A boots and
the JSON of a firewall group that A uses.

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
  `tent:cluster=tent-spike-<run>;…` markers. Created IDs are recorded as soon as they exist.
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
- **Manual cleanup.** If a run was killed with `kill -9`, find leftovers by the `tent-spike-` tag or description in
  the Vultr console.
- **No create retries.** The script never retries a failed create request. This is deliberate, because blind retries
  are how duplicates happen ([ADR-0015](../../docs/adr/0015-idempotency-without-unique-names.md)).
