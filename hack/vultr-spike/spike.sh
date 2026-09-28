#!/usr/bin/env bash
# tent Vultr spike: measures undocumented Vultr behaviour that the provider design depends on
# (docs/adr/0018-vultr-provider-design.md, "provisional" items; docs/platform-notes.md §3, items marked 🔬).
#
# It creates REAL, BILLED resources in your Vultr account (at most 3 instances at a time, 4 in total,
# one VPC, up to three firewall groups, up to two SSH keys) and deletes them on exit unless --keep is given.
# Usage and details: hack/vultr-spike/README.md
#
# Portable bash (3.2+, macOS default), requires: curl, jq 1.6+, ssh, ssh-keygen, awk, od.
set -euo pipefail

readonly SPIKE_VERSION="4"
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
readonly SCRIPT_DIR
readonly API_BASE="${VULTR_API_BASE:-https://api.vultr.com/v2}"
readonly ALL_CHECKS="boot,inside,metadata,network,firewall,alias,tags,markers,userdata,scrub,halt,sshdup,lengths,rules,fwinuse,patchtags,vpcpending,halttwice,objstore"
# The checks that log in to an instance over SSH. Without them, instance A gets a firewall group with no rules.
readonly SSH_CHECKS="boot inside metadata network firewall alias scrub halt"

REGION="${REGION:-ams}"
PLAN="${PLAN:-vc2-1c-1gb}"
OS_NAME="${OS_NAME:-Ubuntu 24.04 LTS x64}"
OS_ID="${OS_ID:-}"
VPC_SUBNET="${VPC_SUBNET:-10.64.0.0}"
VPC_MASKS="${VPC_MASKS:-16 20 24}"
OUT_DIR="${OUT_DIR:-$SCRIPT_DIR/results}"
CHECKS="${CHECKS:-$ALL_CHECKS}"
READY_TIMEOUT="${READY_TIMEOUT:-900}"
VENDOR_TIMEOUT="${VENDOR_TIMEOUT:-2400}"
USERDATA_TARGET="${USERDATA_TARGET:-65536}"
S3_ENDPOINT="${S3_ENDPOINT:-}"
S3_BUCKET="${S3_BUCKET:-}"
S3_ACCESS_KEY="${S3_ACCESS_KEY:-}"
S3_SECRET_KEY="${S3_SECRET_KEY:-}"
S3_REGION="${S3_REGION:-us-east-1}"

# SSH pacing: v1 lost SSH after a burst of ~7 connections. At most one new connection to port 22 per host per
# POLL_INTERVAL keeps the spike under ufw's `limit` (6 per 30 s); SSH_MAX_FAILS failures in a row stop further
# attempts during the checks (circuit breaker).
readonly POLL_INTERVAL=15
readonly SSH_MAX_FAILS=3

MODE="run"      # run | preflight | dry-run
ASSUME_YES=0
KEEP=0

# Runtime state (initialised early so the EXIT trap never trips over unset variables).
WORK=""
SOCK_DIR=""
REPORT=""
SUMMARY=""
DETAILS=""
STATE=""
AUTH_CONF=""
API_STATUS=""
API_BODY=""
RUN=""
RUN_TAG=""
SECRET_MARKER=""
SSH_KEY=""
SSH_KEY_ID=""
SSH_FP=""
SSH_OPTS=()
VPC_ID=""
VPC_MASK=""
FG_ID=""
FG_TRIED=""
NEW_FG=""
FG_COUNTS=""    # set by fg_counts
LOCK_FG=""      # the firewall group without rules that instances get when no check needs SSH
NEED_SSH=1
INST_STATE=""   # set by read_state
HOURLY=""
UD_BYTES=""
PAYLOAD_SHA=""
A_ID="" B_ID="" C_ID="" V_ID=""
A_PUB="" B_PUB="" V_PUB=""
A_VPC_IP="" B_VPC_IP=""
# shellcheck disable=SC2034 # read indirectly via getv
A_VPC_MAC="" B_VPC_MAC=""
# shellcheck disable=SC2034 # read indirectly via getv
A_T0="" B_T0="" V_T0=""
# shellcheck disable=SC2034 # read indirectly via getv
A_T_OK="" B_T_OK="" V_T_OK="" A_T_IP="" B_T_IP="" V_T_IP=""
# shellcheck disable=SC2034 # read indirectly via getv
A_T_PORT="" B_T_PORT="" V_T_PORT="" A_T_SSH="" B_T_SSH="" V_T_SSH="" A_T_CI="" B_T_CI="" V_T_CI=""
# shellcheck disable=SC2034 # read indirectly via getv
A_CI_STATUS="" B_CI_STATUS="" V_CI_STATUS=""
# shellcheck disable=SC2034 # read indirectly via getv
A_LAST=0 B_LAST=0 V_LAST=0
V_DONE=""
VPC_MARKER="" SSH_MARKER="" FG_MARKER=""
SSH_NAME_USED=""

usage() {
  cat <<'EOF'
Usage: hack/vultr-spike/spike.sh [options]

Checks undocumented Vultr behaviour for tent's provider design and writes a Markdown report.

Options:
  --preflight        Only query public endpoints (no API key, no resources): region, plan, image.
  --dry-run          Print what would be created and exit (needs no API key).
  --yes              Do not ask for confirmation before creating billed resources.
  --keep             Do not delete resources on exit (you must delete them yourself).
  --only LIST        Comma-separated subset of checks (default: all):
                     boot,inside,metadata,network,firewall,alias,tags,markers,userdata,scrub,halt,
                     sshdup,lengths,rules,fwinuse,patchtags,vpcpending,halttwice,objstore
                     ("--only objstore" needs no Vultr API key and creates no instances;
                     sshdup, lengths and rules create no instance; fwinuse, patchtags, vpcpending and
                     halttwice need only instance A. Unless a check that needs SSH is selected (boot,
                     inside, metadata, network, firewall, alias, scrub, halt), A gets a firewall group
                     with no rules at creation and the checks start once the API reports A ready)
  --region ID        Vultr region (default: ams; env REGION).
  --plan ID          Instance plan (default: vc2-1c-1gb; env PLAN).
  --out DIR          Report directory (default: hack/vultr-spike/results, git-ignored; env OUT_DIR).
  -h, --help         Show this help.

Environment:
  VULTR_API_KEY      Required for a real run. Never printed, passed to curl via a 0600 config file.
  OS_NAME / OS_ID    Image by exact name (default "Ubuntu 24.04 LTS x64") or numeric os_id.
  VPC_SUBNET         VPC network address to try (default 10.64.0.0) with masks VPC_MASKS ("16 20 24").
  READY_TIMEOUT      Seconds to wait for instances A and B to boot (default 900).
  VENDOR_TIMEOUT     Seconds to wait for instance V (no user_data, vendor defaults) to boot (default 2400).
  USERDATA_TARGET    Size in bytes of the user_data of A and B (default 65536, tent's budget).
  S3_ENDPOINT, S3_BUCKET, S3_ACCESS_KEY, S3_SECRET_KEY [, S3_REGION]
                     Optional: run the Object Storage conditional-write check against an EXISTING bucket
                     (e.g. S3_ENDPOINT=https://ams1.vultrobjects.com). Needs curl with --aws-sigv4 (7.75+).
EOF
}

log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }
now() { date +%s; }
need_cmd() { command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"; }
want() { case ",$CHECKS," in *",$1,"*) return 0 ;; *) return 1 ;; esac; }
lower() { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }
upper() { printf '%s' "$1" | tr '[:lower:]' '[:upper:]'; }
setv() { printf -v "$1" '%s' "$2"; }
getv() { local n="$1"; printf '%s' "${!n:-}"; }
b64enc() { base64 | tr -d '\n'; }
b64dec() { if base64 -d </dev/null >/dev/null 2>&1; then base64 -d; else base64 -D; fi; }
urlencode() { jq -rn --arg v "$1" '$v|@uri'; }
oneline() { tr '\n' ' ' | tr -s ' ' | head -c "${1:-300}"; }

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum | awk '{print $1}'; else shasum -a 256 | awk '{print $1}'; fi
}

# rand_chars SET N: N random characters of the tr set SET (tr gets SIGPIPE from head; that is expected).
rand_chars() { LC_ALL=C tr -dc "$1" </dev/urandom 2>/dev/null | head -c "$2" || true; }
rand_alnum() { rand_chars 'A-Za-z0-9' "$1"; }

# uuid4: a lower-case UUID of version 4, the form of tent's operation ids.
uuid4() {
  local h v
  h=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
  v=$(printf '%x' $(((0x${h:16:2} & 0x3f) | 0x80)))
  printf '%s-%s-4%s-%s%s-%s' "${h:0:8}" "${h:8:4}" "${h:13:3}" "$v" "${h:18:2}" "${h:20:12}"
}

# ---------------------------------------------------------------------------------------------------------------
# Report

row() { # row CHECK RESULT IMPACT
  local c r i
  c=$(printf '%s' "$1" | tr '|' '/'); r=$(printf '%s' "$2" | tr '|' '/'); i=$(printf '%s' "$3" | tr '|' '/')
  printf '| %s | %s | %s |\n' "$c" "$r" "$i" >>"$SUMMARY"
  log "RESULT: $c -> $r"
}

detail() { # detail TITLE < content
  { printf '### %s\n\n```\n' "$1"; cat; printf '\n```\n\n'; } >>"$DETAILS"
}

# shellcheck disable=SC2016 # backticks are Markdown, not command substitution
write_report() {
  [ -n "$REPORT" ] && [ -n "$SUMMARY" ] || return 0
  {
    printf '# Vultr spike report\n\n'
    printf -- '- Date (UTC): %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf -- '- Spike version: %s, run id: `%s`\n' "$SPIKE_VERSION" "$RUN"
    printf -- '- Region: `%s`, plan: `%s`, os_id: `%s` (%s)\n' "$REGION" "$PLAN" "${OS_ID:-?}" "$OS_NAME"
    printf -- '- Checks: `%s`\n\n' "$CHECKS"
    printf 'Copy the findings into `docs/platform-notes.md` §3 (replace 🔬 items) and resolve the provisional '
    printf 'items of `docs/adr/0018-vultr-provider-design.md`.\n\n'
    printf '## Summary\n\n| Check | Result | Design impact |\n|---|---|---|\n'
    cat "$SUMMARY"
    printf '\n## Details\n\n'
    cat "$DETAILS"
  } >"$REPORT"
}

# ---------------------------------------------------------------------------------------------------------------
# API client: sets API_STATUS and leaves the response body in the file $API_BODY.
# Retries only on 429 (honouring Retry-After) and, for GET, on transport errors / 5xx. Never retries a failed POST:
# that is exactly the duplicate-creation hazard tent's provider must handle (ADR-0015).

api() { # api METHOD PATH [BODY_FILE]
  local method="$1" path="$2" body="${3:-}" attempt=0 ra
  local hdrs="$WORK/api-headers.txt"
  API_BODY="$WORK/api-body.json"
  while :; do
    attempt=$((attempt + 1))
    local args=(-sS -X "$method" -o "$API_BODY" -D "$hdrs" -w '%{http_code}' --max-time 120
      -H 'Content-Type: application/json' -H 'Accept: application/json')
    if [ -n "$AUTH_CONF" ]; then args+=(-K "$AUTH_CONF"); fi
    if [ -n "$body" ]; then args+=(--data-binary "@$body"); fi
    API_STATUS=$(curl "${args[@]}" "$API_BASE$path" 2>>"$WORK/curl-errors.log" || true)
    [ -n "$API_STATUS" ] || API_STATUS="000"
    if [ "$API_STATUS" = "429" ] && [ "$attempt" -lt 8 ]; then
      ra=$(awk 'tolower($1)=="retry-after:"{print $2}' "$hdrs" 2>/dev/null | tr -d '\r')
      log "rate limited, retrying in ${ra:-2}s"
      sleep "${ra:-2}"
      continue
    fi
    if [ "$method" = "GET" ] && { [ "$API_STATUS" = "000" ] || [ "${API_STATUS:0:1}" = "5" ]; } && [ "$attempt" -lt 4 ]; then
      sleep 2
      continue
    fi
    break
  done
}

api_ok() { case "$API_STATUS" in 200 | 201 | 202 | 204) return 0 ;; *) return 1 ;; esac; }
api_err() { jq -r '.error // empty' "$API_BODY" 2>/dev/null | tr '\n' ' ' | head -c 300 || true; }
answer() { # the last answer as "HTTP <status>[ <error>]"
  local e
  e=$(api_err | sed 's/[[:space:]]*$//')
  printf 'HTTP %s%s' "$API_STATUS" "${e:+ $e}"
}
jqb() { jq -r "$1" "$API_BODY"; }

record_resource() { printf '%s %s\n' "$1" "$2" >>"$STATE"; }

# ---------------------------------------------------------------------------------------------------------------
# SSH helpers (ephemeral key, root login as installed by Vultr).
# One multiplexed master connection per host; every command runs over it. -F /dev/null ignores the operator's
# ssh config, and IdentitiesOnly + IdentityAgent=none offer only the spike key.

ssh_init() {
  SOCK_DIR=$(mktemp -d /tmp/tsk.XXXXXX) # short: unix socket paths are limited to ~104 bytes
  SSH_OPTS=(-F /dev/null -i "$SSH_KEY" -o IdentitiesOnly=yes -o IdentityAgent=none -o BatchMode=yes
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=10
    -o ServerAliveInterval=15 -o ServerAliveCountMax=4 -o ControlPath="$SOCK_DIR/%h")
}

port_open() { # port_open IP: TCP connect to port 22 within 5 s (macOS nc -w does not bound the connect: 75 s)
  local pid i
  (exec 3<>"/dev/tcp/$1/22") 2>/dev/null &
  pid=$!
  for i in 1 2 3 4 5 6 7 8 9 10; do
    if ! kill -0 "$pid" 2>/dev/null; then
      wait "$pid"
      return
    fi
    sleep 0.5
  done
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  return 1
}

ssh_reason() { # ssh_reason IP: why the last master connection failed
  cat "$SOCK_DIR/$1.broken" 2>/dev/null || oneline 200 2>/dev/null <"$WORK/ssh-master-$1.err" || true
}
ssh_unknown() { printf 'unknown (ssh failed: %s)' "$(ssh_reason "$1")"; }
ssh_reset() { rm -f "$SOCK_DIR/$1.broken" "$SOCK_DIR/$1.fails"; }
ssh_close() { ssh "${SSH_OPTS[@]}" -O exit "root@$1" >/dev/null 2>&1 || true; }

ssh_master() { # ssh_master IP: make sure a master connection exists; opens at most one new connection
  local ip="$1" err="$WORK/ssh-master-$1.err"
  ssh "${SSH_OPTS[@]}" -O check "root@$ip" >/dev/null 2>&1 && return 0
  [ -f "$SOCK_DIR/$ip.broken" ] && return 1
  rm -f "$SOCK_DIR/$ip" # a stale socket would turn the new master into a plain connection
  if ssh "${SSH_OPTS[@]}" -o ControlMaster=yes -o ControlPersist=yes -fN "root@$ip" </dev/null >/dev/null 2>"$err"; then
    rm -f "$SOCK_DIR/$ip.fails"
    return 0
  fi
  printf 'x' >>"$SOCK_DIR/$ip.fails"
  if [ "$(wc -c <"$SOCK_DIR/$ip.fails" | tr -d ' ')" -ge "$SSH_MAX_FAILS" ]; then
    oneline 200 <"$err" >"$SOCK_DIR/$ip.broken"
    log "SSH to $ip: circuit breaker open ($(ssh_reason "$ip"))"
  fi
  return 1
}

ssh_x() { # ssh_x IP COMMAND... ; runs over the master connection, never opens an extra one
  local ip="$1"
  shift
  ssh_master "$ip" || return 255
  ssh "${SSH_OPTS[@]}" -o ControlMaster=no "root@$ip" "$@"
}
ssh_ok() { ssh_x "$1" true >/dev/null 2>&1; }
ssh_state() { if ssh_ok "$1"; then echo "ssh ok"; else ssh_unknown "$1"; fi; }

wait_ssh() { # wait_ssh IP TIMEOUT ; one attempt per POLL_INTERVAL
  local deadline=$(($(now) + $2))
  while [ "$(now)" -lt "$deadline" ]; do
    if port_open "$1"; then
      ssh_reset "$1"
      ssh_master "$1" && return 0
    fi
    sleep "$POLL_INTERVAL"
  done
  return 1
}

# ---------------------------------------------------------------------------------------------------------------
# Preflight (public endpoints, no API key).

preflight() {
  local plan_type="${PLAN%%-*}" monthly city
  api GET "/regions?per_page=500"
  api_ok || die "GET /regions failed: $API_STATUS $(api_err)"
  city=$(jq -r --arg r "$REGION" '.regions[] | select(.id==$r) | .city' "$API_BODY")
  [ -n "$city" ] || die "unknown region: $REGION"
  api GET "/regions/$REGION/availability?type=$plan_type"
  api_ok || die "GET availability failed: $API_STATUS $(api_err)"
  if [ "$(jq -r --arg p "$PLAN" '.available_plans | index($p) != null' "$API_BODY")" != "true" ]; then
    row "Plan availability" "$PLAN NOT deployable in $REGION right now" "choose another plan/region"
    die "plan $PLAN is not currently available in $REGION"
  fi
  api GET "/plans?per_page=500"
  api_ok || die "GET /plans failed: $API_STATUS $(api_err)"
  monthly=$(jq -r --arg p "$PLAN" '.plans[] | select(.id==$p) | .monthly_cost' "$API_BODY")
  [ -n "$monthly" ] || die "unknown plan: $PLAN"
  HOURLY=$(awk -v m="$monthly" 'BEGIN { printf "%.4f", m / 672 }')
  if [ -z "$OS_ID" ]; then
    api GET "/os?per_page=500"
    api_ok || die "GET /os failed: $API_STATUS $(api_err)"
    OS_ID=$(jq -r --arg n "$OS_NAME" '.os[] | select(.name==$n) | .id' "$API_BODY" | head -1)
    [ -n "$OS_ID" ] || die "image not found by exact name: $OS_NAME (set OS_ID)"
  fi
  row "Preflight" "region $REGION ($city); $PLAN deployable; \$$monthly/month = \$$HOURLY/hour (÷672); os_id $OS_ID" \
    "availability endpoint + /plans work without a key"
}

# ---------------------------------------------------------------------------------------------------------------
# Resource creation

create_ssh_key() {
  local body="$WORK/body-sshkey.json"
  ssh-keygen -t ed25519 -N '' -q -f "$WORK/id_ed25519" -C "$RUN_TAG"
  SSH_KEY="$WORK/id_ed25519"
  # fp as tent computes it: the first 8 hex digits of the SHA-256 of the key data.
  SSH_FP=$(awk '{print $2}' "$SSH_KEY.pub" | b64dec | sha256 | cut -c1-8)
  SSH_MARKER="tent:cluster=$RUN_TAG;kind=ssh-key;fp=$SSH_FP;op=$(uuid4)"
  SSH_NAME_USED="$SSH_MARKER"
  jq -n --arg n "$SSH_MARKER" --rawfile k "$SSH_KEY.pub" '{name: $n, ssh_key: ($k | rtrimstr("\n"))}' >"$body"
  api POST /ssh-keys "$body"
  if ! api_ok; then
    log "SSH key name with marker rejected ($API_STATUS $(api_err)); retrying with a plain name"
    row "Marker in SSH key name" "rejected: $API_STATUS $(api_err)" "use a plain name + client-side prefix for SSH keys"
    SSH_NAME_USED="$RUN_TAG"
    jq -n --arg n "$RUN_TAG" --rawfile k "$SSH_KEY.pub" '{name: $n, ssh_key: ($k | rtrimstr("\n"))}' >"$body"
    api POST /ssh-keys "$body"
    api_ok || die "creating SSH key failed: $API_STATUS $(api_err)"
  fi
  SSH_KEY_ID=$(jqb '.ssh_key.id')
  record_resource ssh-key "$SSH_KEY_ID"
  log "SSH key $SSH_KEY_ID"
}

create_vpc() {
  local mask id results="" body="$WORK/body-vpc.json"
  for mask in $VPC_MASKS; do
    jq -n --arg r "$REGION" --arg d "$VPC_MARKER" --arg s "$VPC_SUBNET" --argjson m "$mask" \
      '{region: $r, description: $d, v4_subnet: $s, v4_subnet_mask: $m}' >"$body"
    api POST /vpcs "$body"
    if api_ok; then
      id=$(jqb '.vpc.id')
      record_resource vpc "$id"
      results="$results /$mask accepted;"
      if [ -z "$VPC_ID" ]; then
        VPC_ID="$id"
        VPC_MASK="$mask"
      else
        api DELETE "/vpcs/$id" # test-only VPC; keep at most 2 at a time (5 per region allowed)
      fi
    else
      results="$results /$mask rejected ($API_STATUS $(api_err));"
    fi
  done
  row "VPC CIDR masks ($VPC_SUBNET)" "${results# }" "default networking.cidr for Vultr = largest accepted mask"
  [ -n "$VPC_ID" ] || die "no VPC could be created"
  log "VPC $VPC_ID ($VPC_SUBNET/$VPC_MASK)"
}

# Runs on the instance (as root, POSIX sh): host firewall, SSH protections, cloud-init and boot timing.
# `KV key=value` lines become report rows. Secrets (passwords, hashes, tokens) are redacted on the instance.
collector_script() {
  cat <<'EOF'
#!/bin/sh
mono() { v=$(systemctl show "$1" -p ActiveEnterTimestampMonotonic --value 2>/dev/null); if [ -n "$v" ] && [ "$v" != 0 ]; then echo "$((v / 1000000))s"; else echo "-"; fi; }
pkg() { if dpkg-query -W -f '${db:Status-Abbrev}' "$1" 2>/dev/null | grep -q '^ii'; then echo installed; else echo absent; fi; }
unit() { printf '%s/%s' "$(systemctl is-enabled "$1" 2>/dev/null || true)" "$(systemctl is-active "$1" 2>/dev/null || true)"; }
sec() { printf '\n## %s\n' "$1"; }
collect() {
  sec summary
  echo "KV uptime=$(cut -d. -f1 /proc/uptime)s"
  echo "KV image=$(sed -n 's/^PRETTY_NAME=//p' /etc/os-release | tr -d '"'), kernel $(uname -r)"
  echo "KV ufw=$(pkg ufw), unit $(unit ufw), $(ufw status 2>/dev/null | head -1)"
  echo "KV ufw_defaults=$(ufw status verbose 2>/dev/null | sed -n 's/^Default: //p')"
  echo "KV ufw_rules=$(ufw status 2>/dev/null | sed '1,/^--/d' | tr -s ' ' | paste -sd ';' -)"
  echo "KV firewalld=$(pkg firewalld), unit $(unit firewalld)"
  echo "KV fail2ban=$(pkg fail2ban), unit $(unit fail2ban)"
  echo "KV sshguard=$(pkg sshguard), unit $(unit sshguard)"
  echo "KV nft_tables=$(nft list tables 2>/dev/null | paste -sd ';' -)"
  echo "KV sshd=$(sshd -T 2>/dev/null | grep -Ei '^(maxstartups|maxauthtries|persourcepenalties|permitrootlogin|passwordauthentication) ' | paste -sd ';' -)"
  echo "KV ssh_units=ssh.socket $(unit ssh.socket) at $(mono ssh.socket); ssh.service $(unit ssh.service) at $(mono ssh.service)"
  echo "KV boot=network-online $(mono network-online.target); cloud-init $(mono cloud-init.service); cloud-config $(mono cloud-config.service); cloud-final $(mono cloud-final.service); multi-user $(mono multi-user.target)"
  echo "KV cloud_init=$(cloud-init status 2>/dev/null | sed -n 's/^status: //p'); instance-id $(cat /var/lib/cloud/data/instance-id 2>/dev/null)"
  echo "KV vendor_scripts=$(find /var/lib/cloud/instance/scripts/vendor -mindepth 1 -printf '%f,' 2>/dev/null)"
  echo "KV runcmd_runs=$(wc -l 2>/dev/null </var/lib/spike/runcmd.log || echo 0)"
  sec "ufw status verbose"; ufw status verbose 2>&1
  sec "iptables-save -c"; iptables-save -c 2>&1 | head -150
  sec "nft list ruleset"; nft list ruleset 2>&1 | head -200
  sec "fail2ban-client status sshd"; fail2ban-client status sshd 2>&1 | head -20
  sec "sshd log (this boot)"; journalctl -b --no-pager -o short-monotonic -t sshd -t sshd-session 2>/dev/null | tail -40
  sec "ssh units log (this boot)"; journalctl -b --no-pager -o short-monotonic -u ssh.socket -u ssh.service 2>/dev/null | tail -20
  sec "cloud-init status --long"; cloud-init status --long 2>&1
  sec "cloud-init analyze blame"; cloud-init analyze blame 2>&1 | head -30
  sec "cloud-init analyze boot"; cloud-init analyze boot 2>&1 | tail -20
  sec "systemd-analyze"; systemd-analyze 2>&1; systemd-analyze blame 2>&1 | head -25
  sec "systemd-analyze critical-chain"; systemd-analyze critical-chain 2>&1 | head -40
  sec "/var/log/cloud-init-output.log (tail)"; tail -80 /var/log/cloud-init-output.log 2>&1
  sec "/var/lib/cloud/instance"; ls -la /var/lib/cloud/instance /var/lib/cloud/instance/scripts /var/lib/cloud/instance/scripts/vendor 2>&1
  sec "vendor-data.txt"; head -300 /var/lib/cloud/instance/vendor-data.txt 2>&1
  sec "vendor-cloud-config.txt"; head -200 /var/lib/cloud/instance/vendor-cloud-config.txt 2>&1
}
collect 2>&1 | sed -E \
  -e '/^KV /!s/.*(pass|secret|token|api[_-]?key).*/<line redacted>/I' \
  -e 's/\$[0-9a-z]{1,2}\$[^ "]*/<redacted-hash>/g' \
  -e 's/^([[:space:]]*-?[[:space:]]*root:).*/\1<redacted>/'
EOF
}

# Cloud-config of instances A and B: package upgrades off (as tent will do), the collector (run once by runcmd),
# a graceful-shutdown marker, an HTTP listener on :4646 and a random payload that pads the whole user_data to
# USERDATA_TARGET bytes (does cloud-init process tent's 64 KiB budget intact?).
cloud_config_ab() { # cloud_config_ab PAYLOAD_B64_FILE (empty file = no payload)
  cat <<EOF
#cloud-config
# tent spike instance A/B. SPIKE_SECRET_MARKER=${SECRET_MARKER}
package_update: false
package_upgrade: false
write_files:
  - path: /usr/local/sbin/spike-collect
    permissions: "0755"
    content: |
EOF
  sed 's/^/      /' "$WORK/collect.sh"
  cat <<'EOF'
  - path: /etc/systemd/system/spike-stop-marker.service
    permissions: "0644"
    content: |
      [Unit]
      Description=tent spike: record a graceful shutdown
      [Service]
      Type=oneshot
      RemainAfterExit=yes
      ExecStart=/bin/true
      ExecStop=/bin/sh -c 'mkdir -p /var/lib/spike && date -u +%%s > /var/lib/spike/graceful-stop && sync'
      [Install]
      WantedBy=multi-user.target
  - path: /etc/systemd/system/spike-listener.service
    permissions: "0644"
    content: |
      [Unit]
      Description=tent spike: HTTP listener on :4646
      [Service]
      ExecStartPre=/bin/mkdir -p /var/lib/spike/www
      ExecStart=/usr/bin/python3 -m http.server 4646 --bind 0.0.0.0 --directory /var/lib/spike/www
      Restart=always
      [Install]
      WantedBy=multi-user.target
EOF
  if [ -s "$1" ]; then
    printf '  - path: /var/lib/spike/payload.bin\n    permissions: "0644"\n    encoding: b64\n    content: '
    cat "$1"
    printf '\n'
  fi
  cat <<'EOF'
runcmd:
  - [sh, -c, "mkdir -p /var/lib/spike && date -u +%s >> /var/lib/spike/runcmd.log"]
  - [sh, -c, "/usr/local/sbin/spike-collect > /var/lib/spike/boot-capture.txt 2>&1"]
  - [systemctl, daemon-reload]
  - [systemctl, enable, --now, spike-stop-marker.service, spike-listener.service]
EOF
}

build_user_data() { # writes $WORK/cc-ab.yaml of about USERDATA_TARGET bytes; sets UD_BYTES, PAYLOAD_SHA
  local base n
  collector_script >"$WORK/collect.sh"
  : >"$WORK/payload.b64"
  cloud_config_ab "$WORK/payload.b64" >"$WORK/cc-ab.yaml"
  base=$(wc -c <"$WORK/cc-ab.yaml" | tr -d ' ')
  n=$(((USERDATA_TARGET - base - 120) / 4)) # whole base64 quads
  n=$((n * 3))
  if [ "$n" -gt 0 ]; then
    head -c "$n" /dev/urandom >"$WORK/payload.bin"
    PAYLOAD_SHA=$(sha256 <"$WORK/payload.bin")
    b64enc <"$WORK/payload.bin" >"$WORK/payload.b64"
    cloud_config_ab "$WORK/payload.b64" >"$WORK/cc-ab.yaml"
  fi
  UD_BYTES=$(wc -c <"$WORK/cc-ab.yaml" | tr -d ' ')
}

create_instance() { # create_instance NAME [USERDATA_FILE] -> sets NAME_ID, NAME_T0; no file = no user_data
  local name="$1" ud="${2:-}" label op body="$WORK/body-create-$1.json" i found=""
  label="$RUN_TAG-$(lower "$name")"
  op="$RUN_TAG-op-$(lower "$name")"
  if [ -n "$ud" ]; then b64enc <"$ud" >"$WORK/ud-$name.b64"; else : >"$WORK/ud-$name.b64"; fi
  jq -n --arg r "$REGION" --arg p "$PLAN" --argjson os "$OS_ID" --arg l "$label" --arg t "$RUN_TAG" --arg op "$op" \
    --arg k "$SSH_KEY_ID" --arg v "$VPC_ID" --arg fg "$LOCK_FG" --rawfile ud "$WORK/ud-$name.b64" \
    '{region: $r, plan: $p, os_id: $os, label: $l, hostname: $l, tags: [$t, $op], sshkey_id: [$k], attach_vpc: [$v],
      backups: "disabled", activation_email: false}
     + (if $ud != "" then {user_data: ($ud | rtrimstr("\n"))} else {} end)
     + (if $fg != "" then {firewall_group_id: $fg} else {} end)' >"$body"
  setv "${name}_T0" "$(now)"
  api POST /instances "$body"
  if ! api_ok; then
    log "creating instance $name failed: $API_STATUS $(api_err)"
    return 1
  fi
  setv "${name}_ID" "$(jqb '.instance.id')"
  record_resource instance "$(getv "${name}_ID")"
  log "instance $name: $(getv "${name}_ID")"
  # Search-before-retry (ADR-0015) needs a fresh instance to be listable by its operation tag at once.
  for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
    if [ "$(found_by_id "$(getv "${name}_ID")" "tag=$(urlencode "$op")")" = "true" ]; then found="$i"; break; fi
    sleep 1
  done
  if [ -n "$found" ]; then found="found on request #$found (~$((found - 1))s after the create response)"; else found="NOT found within 20 requests"; fi
  row "List by op tag right after create ($name)" "$found" "search-before-retry after a lost create response (ADR-0015)"
}

delete_instance_now() { # delete_instance_now ID ; waits until it is gone (keeps concurrency low)
  local id="$1" deadline
  api DELETE "/instances/$id"
  deadline=$(($(now) + 180))
  while [ "$(now)" -lt "$deadline" ]; do
    api GET "/instances/$id"
    [ "$API_STATUS" = "404" ] && return 0
    sleep 5
  done
  log "instance $id still present after delete"
}

# ---------------------------------------------------------------------------------------------------------------
# Boot timing (A and B: package upgrade disabled, as tent does; V: no user_data, Vultr's vendor defaults) - also
# discovers IPs. Port 22 and SSH are touched at most once per POLL_INTERVAL per instance.

read_state() { # read_state NAME: sets INST_STATE to status/power_status/server_status (or the HTTP error); the first
  # time it reads active/running/ok, records NAME_T_OK and NAME_PUB
  local n="$1"
  api GET "/instances/$(getv "${n}_ID")"
  if api_ok; then
    INST_STATE=$(jqb '.instance.status + "/" + .instance.power_status + "/" + .instance.server_status' 2>/dev/null || echo '?')
  else
    INST_STATE="HTTP $API_STATUS"
  fi
  if [ "$INST_STATE" = "active/running/ok" ] && [ -z "$(getv "${n}_T_OK")" ]; then
    setv "${n}_T_OK" "$(($(now) - $(getv "${n}_T0")))"
    setv "${n}_PUB" "$(jqb '.instance.main_ip')"
  fi
}

note_vpc_ip() { # note_vpc_ip NAME: if the last answer (GET /instances/{id}/vpcs) lists an address other than 0.0.0.0,
  # records NAME_T_IP, NAME_VPC_IP and NAME_VPC_MAC and returns 0
  local n="$1" ip="" mac=""
  read -r ip mac <<<"$(jq -r '[.vpcs[]? | select((.ip_address // "") != "" and .ip_address != "0.0.0.0")][0] // empty
    | "\(.ip_address) \(.mac_address // "")"' "$API_BODY" 2>/dev/null || true)"
  [ -n "$ip" ] || return 1
  setv "${n}_T_IP" "$(($(now) - $(getv "${n}_T0")))"
  setv "${n}_VPC_IP" "$ip"
  setv "${n}_VPC_MAC" "$mac"
}

poll_instance() { # poll_instance NAME ; returns 0 when every milestone is reached
  local n="$1" id t0 ci pub last pending=0
  id=$(getv "${n}_ID")
  [ -n "$id" ] || return 0
  t0=$(getv "${n}_T0")
  if [ -z "$(getv "${n}_T_OK")" ]; then
    read_state "$n"
    [ -n "$(getv "${n}_T_OK")" ] || pending=1
  fi
  if [ -z "$(getv "${n}_T_IP")" ]; then
    api GET "/instances/$id/vpcs"
    note_vpc_ip "$n" || pending=1
  fi
  # A locked-down instance (no check needs SSH) has no milestones beyond the API.
  [ "$NEED_SSH" = 1 ] || return "$pending"
  pub=$(getv "${n}_PUB")
  last=$(getv "${n}_LAST")
  if [ -z "$pub" ] || [ $(($(now) - ${last:-0})) -lt "$POLL_INTERVAL" ]; then
    [ -n "$(getv "${n}_T_CI")" ] || pending=1
    return "$pending"
  fi
  setv "${n}_LAST" "$(now)"
  if [ -z "$(getv "${n}_T_PORT")" ]; then
    if port_open "$pub"; then setv "${n}_T_PORT" "$(($(now) - t0))"; else return 1; fi
  fi
  if [ -z "$(getv "${n}_T_SSH")" ]; then
    ssh_reset "$pub"
    if ssh_master "$pub"; then setv "${n}_T_SSH" "$(($(now) - t0))"; else return 1; fi
  fi
  if [ -z "$(getv "${n}_T_CI")" ]; then
    ci=$(ssh_x "$pub" "cloud-init status 2>/dev/null | sed -n 's/^status: //p'" 2>/dev/null || true)
    case "$ci" in
      done | error | degraded*)
        setv "${n}_T_CI" "$(($(now) - t0))"
        setv "${n}_CI_STATUS" "$ci"
        ;;
      *) pending=1 ;;
    esac
  fi
  return "$pending"
}

boot_row() { # boot_row NAME TITLE
  local n="$1" pub ssh_note=""
  pub=$(getv "${n}_PUB")
  if [ -n "$pub" ] && [ -z "$(getv "${n}_T_SSH")" ]; then ssh_note=" (last ssh error: $(ssh_reason "$pub"))"; fi
  row "$2" "api ok $(getv "${n}_T_OK" || true)s, VPC IP $(getv "${n}_T_IP")s, port 22 open $(getv "${n}_T_PORT")s, SSH login $(getv "${n}_T_SSH")s$ssh_note, cloud-init $(getv "${n}_CI_STATUS") at $(getv "${n}_T_CI")s" \
    "E2E timeouts; the inside rows say where the time goes"
}

measure_boot() {
  local deadline=$(($(now) + READY_TIMEOUT)) a_done b_done
  log "waiting for instances to boot (timeout ${READY_TIMEOUT}s)"
  while :; do
    a_done=0
    b_done=0
    if poll_instance A; then a_done=1; fi
    if poll_instance B; then b_done=1; fi
    poll_vendor
    if [ "$a_done" = 1 ] && [ "$b_done" = 1 ]; then break; fi
    if [ "$(now)" -gt "$deadline" ]; then
      log "timeout waiting for instances"
      break
    fi
    sleep 5
  done
  if want boot; then
    boot_row A "Boot A (package_upgrade: false, ${UD_BYTES}-byte user_data)"
    if [ -n "$B_ID" ]; then boot_row B "Boot B (same user_data as A)"; fi
  fi
  if [ "$NEED_SSH" = 0 ]; then
    [ -n "$A_T_OK" ] || die "instance A never became active/running/ok"
    row "Instance A without SSH" "api ok ${A_T_OK}s, VPC IP ${A_T_IP:-?}s; firewall_group_id \`$(a_fg)\` (the group with no rules: \`$LOCK_FG\`)" \
      "no selected check needs SSH, so A takes no inbound traffic"
    return 0
  fi
  [ -n "$A_PUB" ] && [ -n "$A_T_SSH" ] || die "instance A never became reachable over SSH: $(ssh_reason "$A_PUB")"
}

poll_vendor() { # one gentle poll of V between checks; V is slow on purpose (vendor package upgrade)
  [ -n "$V_ID" ] && [ -z "$V_DONE" ] || return 0
  if poll_instance V; then V_DONE=1; fi
}

finish_vendor() {
  [ -n "$V_ID" ] || return 0
  local deadline=$((V_T0 + VENDOR_TIMEOUT))
  log "waiting for instance V (no user_data) until $((deadline - $(now)))s from now"
  while [ -z "$V_DONE" ] && [ "$(now)" -lt "$deadline" ]; do
    poll_vendor
    [ -n "$V_DONE" ] || sleep 5
  done
  boot_row V "Boot V (no user_data: Vultr vendor defaults)"
  if [ -n "$V_T_SSH" ]; then check_inside V; fi
}

# ---------------------------------------------------------------------------------------------------------------
# Checks

check_inside() { # check_inside NAME ; host firewall, SSH protections, cloud-init and boot timing from the instance
  local n="$1" pub f="$WORK/inside-$1.txt" kv
  pub=$(getv "${n}_PUB")
  if ! ssh_x "$pub" 'sh -s' <"$WORK/collect.sh" >"$f" 2>&1; then
    row "Inside $n" "$(ssh_unknown "$pub")" ""
    return 0
  fi
  { grep '^KV ' "$f" || true; } | sed 's/^KV //' | while IFS= read -r kv; do row "Inside $n: ${kv%%=*}" "${kv#*=}" ""; done
  { grep -v '^KV ' "$f" || true; } | detail "inside instance $n (collected over SSH)"
  if [ "$n" != V ]; then
    ssh_x "$pub" 'cat /var/lib/spike/boot-capture.txt 2>&1' >"$f" 2>&1 || true
    { grep '^KV ' "$f" || true; } | sed 's/^KV //' | while IFS= read -r kv; do row "Inside $n at runcmd: ${kv%%=*}" "${kv#*=}" ""; done
    { grep -v '^KV ' "$f" || true; } | detail "inside instance $n (captured by runcmd during first boot)"
  fi
}

check_metadata() {
  local f="$WORK/metadata-a.json" keys idm region tags ifs code size
  ssh_x "$A_PUB" "curl -s -m 5 http://169.254.169.254/v1.json" >"$f" 2>/dev/null || true
  if ! jq -e . "$f" >/dev/null 2>&1; then
    row "Metadata /v1.json" "not reachable or not JSON: $(ssh_state "$A_PUB")" "revisit nodeup/env/vultr"
    return 0
  fi
  keys=$(jq -r 'keys | join(", ")' "$f")
  idm=$(jq -r --arg id "$A_ID" 'if ."instance-v2-id" == $id then "matches API id" else "DIFFERS: \(."instance-v2-id")" end' "$f")
  region=$(jq -r '.region.regioncode // .region // "?"' "$f")
  tags=$(jq -c '.tags // "absent"' "$f")
  ifs=$(jq -c '[.interfaces[]? | {type: ."network-type", mac, ipv4: .ipv4.address}]' "$f")
  code=$(ssh_x "$A_PUB" "curl -s -m 5 -o /dev/null -w '%{http_code}' http://169.254.169.254/latest/user-data || true" 2>/dev/null || true)
  size=$(ssh_x "$A_PUB" "curl -s -m 10 http://169.254.169.254/latest/user-data | wc -c" 2>/dev/null | tr -d ' ' || true)
  row "Metadata /v1.json" "keys: $keys; instance-v2-id $idm; region $region; tags $tags (instance has 2)" "Environment implementation"
  row "Metadata interfaces" "$ifs" "private IP/MAC discovery on the node"
  row "Metadata /latest/user-data" "HTTP $code, $size bytes (sent $UD_BYTES)" "must be blocked for workloads (nftables)"
  jq 'del(."user-data", ."vendor-data", ."startup-script")' "$f" | detail "metadata /v1.json (instance A, user/vendor data removed)"
  if [ -n "$PAYLOAD_SHA" ]; then
    size=$(ssh_x "$A_PUB" "sha256sum /var/lib/spike/payload.bin 2>/dev/null | cut -d' ' -f1" 2>/dev/null || true)
    if [ "$size" = "$PAYLOAD_SHA" ]; then size="intact"; else size="MISMATCH or missing (${size:-no file})"; fi
    row "write_files payload in ${UD_BYTES}-byte user_data" "$size" "tent's 64 KiB user_data budget works end to end"
  fi
}

check_network() {
  local n pub out iface mtu ping
  for n in A B; do
    pub=$(getv "${n}_PUB")
    [ -n "$pub" ] || continue
    out=$(ssh_x "$pub" 'ip -o link; echo; ip -o -4 addr; echo; ip route; echo; cat /etc/netplan/*.yaml 2>/dev/null; echo; networkctl list 2>/dev/null' 2>/dev/null || ssh_unknown "$pub")
    printf '%s\n' "$out" | detail "network on instance $n"
  done
  iface=$(ssh_x "$A_PUB" "ip -o -4 addr | awk -v ip='$A_VPC_IP' '\$4 ~ \"^\" ip \"/\" {print \$2}'" 2>/dev/null || true)
  mtu=$(ssh_x "$A_PUB" "cat /sys/class/net/$iface/mtu 2>/dev/null || true" 2>/dev/null || true)
  row "VPC interface (A)" "name ${iface:-?}, MTU ${mtu:-?}, IP $A_VPC_IP, MAC $A_VPC_MAC (see details for netplan)" \
    "match by MAC/CIDR; MTU for CNI"
  if [ -n "$B_VPC_IP" ]; then
    ping=$(ssh_x "$A_PUB" "ping -c 3 -W 2 $B_VPC_IP >/dev/null 2>&1 && echo ok || echo fail" 2>/dev/null || ssh_unknown "$A_PUB")
    row "A -> B over VPC (ping, image firewall defaults)" "$ping" "baseline for firewall/alias checks"
    ping=$(ssh_x "$B_PUB" "ping -c 3 -W 2 $A_VPC_IP >/dev/null 2>&1 && echo ok || echo fail" 2>/dev/null || ssh_unknown "$B_PUB")
    row "B -> A over VPC (ping, image firewall defaults)" "$ping" ""
  fi
}

listener_a() { # the listener is a systemd unit started by runcmd; report whether it listens
  ssh_x "$A_PUB" 'systemctl start spike-listener.service; sleep 1; ss -ltn "sport = :4646" | grep -q 4646 && echo listening || echo "not listening"' 2>/dev/null || ssh_unknown "$A_PUB"
}

http_from_b() { ssh_x "$B_PUB" "curl -s -m 5 -o /dev/null -w '%{http_code}' http://$1:4646/ || true" 2>/dev/null || ssh_unknown "$B_PUB"; }
http_from_here() { curl -s -m 5 -o /dev/null -w '%{http_code}' "http://$1:4646/" 2>/dev/null || true; }

disable_host_firewall() { # disable_host_firewall IP
  ssh_x "$1" 'ufw --force disable >/dev/null 2>&1 || true; systemctl stop firewalld >/dev/null 2>&1 || true' >/dev/null 2>&1 || true
}

check_firewall_defaults() {
  local vpc pub lis
  lis=$(listener_a)
  vpc=$(http_from_b "$A_VPC_IP")
  pub=$(http_from_here "$A_PUB")
  row "Image defaults: :4646 over VPC (B->A)" "HTTP $vpc (listener on A: $lis)" "does the default host firewall block Nomad ports on the VPC?"
  row "Image defaults: :4646 public (here->A)" "HTTP $pub" ""
  disable_host_firewall "$A_PUB"
  disable_host_firewall "$B_PUB"
  vpc=$(http_from_b "$A_VPC_IP")
  pub=$(http_from_here "$A_PUB")
  row "Host firewall disabled: :4646 VPC / public" "HTTP $vpc / HTTP $pub" "baseline without any firewall"
}

add_fw_rule() { # add_fw_rule IPTYPE PROTO SUBNET SIZE [PORT] ; prints the outcome
  local body="$WORK/body-fwrule.json" proto="$2"
  jq -n --arg t "$1" --arg p "$proto" --arg s "$3" --argjson z "$4" --arg port "${5:-}" \
    '{ip_type: $t, protocol: $p, subnet: $s, subnet_size: $z, notes: "tent spike"} + (if $port != "" then {port: $port} else {} end)' >"$body"
  api POST "/firewalls/$FG_ID/rules" "$body"
  if ! api_ok; then
    proto=$(upper "$proto")
    jq -n --arg t "$1" --arg p "$proto" --arg s "$3" --argjson z "$4" --arg port "${5:-}" \
      '{ip_type: $t, protocol: $p, subnet: $s, subnet_size: $z, notes: "tent spike"} + (if $port != "" then {port: $port} else {} end)' >"$body"
    api POST "/firewalls/$FG_ID/rules" "$body"
  fi
  if api_ok; then printf '%s %s %s/%s %s: ok' "$1" "$proto" "$3" "$4" "${5:-}"; else printf '%s %s %s/%s %s: %s %s' "$1" "$proto" "$3" "$4" "${5:-}" "$API_STATUS" "$(api_err)"; fi
}

create_fg() { # create_fg DESCRIPTION -> sets NEW_FG and records it for cleanup at once
  local body="$WORK/body-fw.json"
  NEW_FG=""
  jq -n --arg d "$1" '{description: $d}' >"$body"
  api POST /firewalls "$body"
  api_ok || return 1
  NEW_FG=$(jqb '.firewall_group.id')
  record_resource firewall "$NEW_FG"
}

# The lockdown group: no rules, so no inbound traffic, and the instances' root password login stays unreachable.
# Only when no selected check needs SSH; this script does not know the machine's public IP to allow it alone.
create_lockdown_fg() {
  create_fg "$RUN_TAG-lockdown" || die "cannot create the firewall group for instance A: $API_STATUS $(api_err)"
  LOCK_FG="$NEW_FG"
  log "firewall group without rules for the instances: $LOCK_FG"
}

relock_a() { # attaches the lockdown group to A again right after each check that detaches or deletes A's group
  local body="$WORK/body-relock.json"
  [ -n "$LOCK_FG" ] || return 0
  jq -n --arg f "$LOCK_FG" '{firewall_group_id: $f}' >"$body"
  api PATCH "/instances/$A_ID" "$body"
  api_ok || row "Attach the group with no rules to A again" "failed: $API_STATUS $(api_err)" "A takes inbound traffic until cleanup"
}

ensure_fg() { # the run's firewall group FG_ID for the checks without instances; one create attempt, never retried
  [ -z "$FG_ID" ] || return 0
  [ -z "$FG_TRIED" ] || return 1
  FG_TRIED=1
  if create_fg "$FG_MARKER"; then
    FG_ID="$NEW_FG"
    return 0
  fi
  row "Firewall group create" "failed: $API_STATUS $(api_err)" "check account/ACL"
  return 1
}

check_firewall_group() {
  local body="$WORK/body-fw.json" deadline t0 pub vpc ssh_state waited rules before
  if ! create_fg "$FG_MARKER"; then
    row "Firewall group create" "failed: $API_STATUS $(api_err)" "check account/ACL"
    return 0
  fi
  FG_ID="$NEW_FG"
  rules="$(add_fw_rule v4 tcp 0.0.0.0 0 22); $(add_fw_rule v6 tcp :: 0 22); $(add_fw_rule v4 icmp 0.0.0.0 0)"
  row "Firewall group rules" "$rules" "rule syntax for the provider"
  before=$(http_from_here "$A_PUB")
  jq -n --arg f "$FG_ID" '{firewall_group_id: $f}' >"$body"
  t0=$(now)
  api PATCH "/instances/$A_ID" "$body"
  if ! api_ok; then
    row "Attach firewall group" "failed: $API_STATUS $(api_err)" ""
    return 0
  fi
  # Wait until the public port is filtered (propagation time), at most 180 s.
  deadline=$((t0 + 180))
  pub=$(http_from_here "$A_PUB")
  while [ "$pub" = "200" ] && [ "$(now)" -lt "$deadline" ]; do
    sleep 5
    pub=$(http_from_here "$A_PUB")
  done
  waited=$(($(now) - t0))
  vpc=$(http_from_b "$A_VPC_IP")
  ssh_close "$A_PUB" # a NEW connection proves that the group's port 22 rule works
  ssh_reset "$A_PUB"
  if ssh_ok "$A_PUB"; then ssh_state="new SSH connection ok"; else ssh_state="new SSH connection $(ssh_unknown "$A_PUB")"; fi
  row "Firewall group (SSH + ICMP only) attached to A" "public :4646 HTTP $before before, HTTP $pub after ${waited}s; $ssh_state" "propagation time"
  row "Firewall group filters VPC traffic?" "B->A :4646 over VPC with the group attached: HTTP $vpc (200 = not filtered)" \
    "FirewallCoversPrivate capability"
  # Detach, so the later checks do not depend on the group.
  jq -n '{firewall_group_id: ""}' >"$body"
  api PATCH "/instances/$A_ID" "$body"
  row "Detach firewall group (PATCH firewall_group_id \"\")" "HTTP $API_STATUS $(api_err)" ""
  if ! ssh_ok "$A_PUB"; then wait_ssh "$A_PUB" 300 || true; fi
}

check_alias() {
  local alias iface base ping http
  [ -n "$B_PUB" ] || { row "Alias IP in VPC" "skipped (no instance B)" ""; return 0; }
  if [ "${VPC_MASK:-24}" -gt 24 ]; then
    row "Alias IP in VPC" "skipped (mask /$VPC_MASK too small)" ""
    return 0
  fi
  base="${VPC_SUBNET%.*}"
  alias="$base.250"
  if [ "$alias" = "$A_VPC_IP" ] || [ "$alias" = "$B_VPC_IP" ]; then alias="$base.249"; fi
  disable_host_firewall "$A_PUB"
  iface=$(ssh_x "$A_PUB" "ip -o -4 addr | awk -v ip='$A_VPC_IP' '\$4 ~ \"^\" ip \"/\" {print \$2}'" 2>/dev/null || true)
  if [ -z "$iface" ]; then
    row "Alias IP in VPC" "skipped (VPC interface not found: $(ssh_state "$A_PUB"))" ""
    return 0
  fi
  ssh_x "$A_PUB" "ip addr add $alias/$VPC_MASK dev $iface" >/dev/null 2>&1 || true
  ping=$(ssh_x "$B_PUB" "ping -c 3 -W 2 $alias >/dev/null 2>&1 && echo ok || echo fail" 2>/dev/null || ssh_unknown "$B_PUB")
  http=$(http_from_b "$alias")
  ssh_x "$A_PUB" "ip addr del $alias/$VPC_MASK dev $iface" >/dev/null 2>&1 || true
  row "Alias IP $alias on A's VPC interface, from B" "ping $ping, HTTP $http" \
    "if reachable: fixed slots possible on Vultr (future ADR); else seed+refresh only"
}

set_tags() { # set_tags JSON_ARRAY
  local body="$WORK/body-tags.json"
  jq -n --argjson t "$1" '{tags: $t}' >"$body"
  api PATCH "/instances/$A_ID" "$body"
}

tags_now() { api GET "/instances/$A_ID"; jq -c '.instance.tags' "$API_BODY" 2>/dev/null || echo '?'; }

found_by_id() { # found_by_id ID QUERY_STRING -> true/false (is the instance in the result)
  api GET "/instances?per_page=500&$2"
  jq -r --arg id "$1" '[.instances[]?.id] | index($id) != null' "$API_BODY" 2>/dev/null || echo "?"
}
found_by() { found_by_id "$A_ID" "$1"; }

check_tags() {
  local cand stored res n arr long accepted_len="" max_count=""
  local candidates="tent/cluster=spike
tent-cluster=spike
tent:cluster=spike
tent.cluster.spike
Tent-Cluster=Spike
tent cluster spike
tent/spec-hash=0123456789abcdef
tent/op=3f1c2d9e-8b7a-4c61-9e2f-5a6b7c8d9e0f
tent-ü"
  while IFS= read -r cand; do
    [ -n "$cand" ] || continue
    set_tags "$(jq -cn --arg r "$RUN_TAG" --arg c "$cand" '[$r, $c]')"
    if api_ok; then
      stored=$(tags_now)
      if [ "$(printf '%s' "$stored" | jq -r --arg c "$cand" 'index($c) != null' 2>/dev/null || echo false)" = "true" ]; then
        res="accepted, stored verbatim"
      else
        res="accepted, stored as $stored"
      fi
    else
      res="rejected: $API_STATUS $(api_err)"
    fi
    row "Tag \`$cand\`" "$res" "label codec (ADR-0018)"
  done <<EOF
$candidates
EOF
  for n in 64 128 255 256 512; do
    long=$(lower "$(rand_alnum "$n")")
    set_tags "$(jq -cn --arg r "$RUN_TAG" --arg c "$long" '[$r, $c]')"
    if api_ok; then accepted_len="$n"; fi
  done
  row "Tag length" "longest accepted of 64/128/255/256/512: ${accepted_len:-none}" "spec-hash/op tags fit?"
  for n in 10 25 50 100; do
    arr=$(jq -cn --arg r "$RUN_TAG" --argjson n "$n" '[$r] + [range(1; $n) | "t\(.)"]')
    set_tags "$arr"
    if api_ok; then max_count="$n (stored $(tags_now | jq -r 'length' 2>/dev/null || echo '?'))"; fi
  done
  row "Tag count" "largest accepted of 10/25/50/100: ${max_count:-none}" "canonical labels need ~6 tags"
  # Filter semantics
  set_tags "$(jq -cn --arg r "$RUN_TAG" '[$r, ($r + "-filterprobe")]')"
  row "Filter ?tag= exact" "$(found_by "tag=$(urlencode "$RUN_TAG-filterprobe")")" "server-side cluster filter"
  row "Filter ?tag= prefix" "$(found_by "tag=$(urlencode "$RUN_TAG-filter")") (true = substring match)" "codec must avoid prefix collisions if true"
  row "Filter ?tag= suffix" "$(found_by "tag=filterprobe") (true = substring match)" ""
  row "Filter ?tag= upper-case" "$(found_by "tag=$(urlencode "$(upper "$RUN_TAG-filterprobe")")") (true = case-insensitive)" ""
  row "Filter ?label= prefix" "$(found_by "label=$(urlencode "$RUN_TAG")") (true = substring match)" "labels only as handles"
  set_tags "$(jq -cn --arg r "$RUN_TAG" '[$r]')"
}

check_markers() {
  local got
  api GET "/vpcs/$VPC_ID"
  got=$(jqb '.vpc.description')
  if [ "$got" = "$VPC_MARKER" ]; then got="stored verbatim"; else got="stored as \`$got\`"; fi
  row "Marker in VPC description" "$got" "ownership marker for non-instance resources"
  if [ "$SSH_NAME_USED" = "$SSH_MARKER" ]; then
    api GET "/ssh-keys/$SSH_KEY_ID"
    got=$(jqb '.ssh_key.name')
    if [ "$got" = "$SSH_MARKER" ]; then got="stored verbatim"; else got="stored as \`$got\`"; fi
    row "Marker in SSH key name" "$got" ""
  fi
  if [ -n "$FG_ID" ]; then
    api GET "/firewalls/$FG_ID"
    got=$(jqb '.firewall_group.description')
    if [ "$got" = "$FG_MARKER" ]; then got="stored verbatim"; else got="stored as \`$got\`"; fi
    row "Marker in firewall group description" "$got" ""
  fi
}

userdata_payload() { # userdata_payload BYTES FILE : "#cloud-config\n# <pad>\n" of exactly BYTES bytes
  local n="$1" f="$2"
  { printf '#cloud-config\n# '; rand_alnum $((n - 17)); printf '\n'; } >"$f"
}

try_userdata() { # try_userdata INSTANCE_ID BYTES -> 0 if accepted; leaves payload in $WORK/ud-try.txt
  local id="$1" n="$2" body="$WORK/body-ud.json"
  userdata_payload "$n" "$WORK/ud-try.txt"
  b64enc <"$WORK/ud-try.txt" >"$WORK/ud-try.b64"
  jq -n --rawfile u "$WORK/ud-try.b64" '{user_data: ($u | rtrimstr("\n"))}' >"$body"
  api PATCH "/instances/$id" "$body"
  api_ok
}

check_userdata() {
  local id lo=0 hi=0 n mid err="" stored sha_want sha_got body c_ud="$WORK/ud-c.txt" target
  if [ -n "$B_ID" ]; then target="B"; id="$B_ID"; else target="A"; id="$A_ID"; fi
  for n in 4096 16384 65536 262144 1048576 4194304; do
    if try_userdata "$id" "$n"; then lo="$n"; else hi="$n"; err="$API_STATUS $(api_err)"; break; fi
  done
  if [ "$hi" = 0 ]; then
    row "user_data max (PATCH)" ">= $lo bytes (no rejection up to 4 MiB)" "MaxUserDataBytes"
  else
    [ "$lo" != 0 ] || lo=256
    while [ $((hi - lo)) -gt 512 ]; do
      mid=$(((lo + hi) / 2 / 64 * 64))
      if try_userdata "$id" "$mid"; then lo="$mid"; else hi="$mid"; err="$API_STATUS $(api_err)"; fi
    done
    row "user_data max (PATCH, instance $target)" "accepted $lo bytes raw (~$((4 * ((lo + 2) / 3))) base64), rejected $hi: $err" \
      "MaxUserDataBytes = measured limit minus 25%"
  fi
  # Stored intact?
  if try_userdata "$id" "$lo"; then
    sha_want=$(sha256 <"$WORK/ud-try.txt")
    cp "$WORK/ud-try.txt" "$c_ud"
    api GET "/instances/$id/user-data"
    stored="$WORK/ud-stored.txt"
    jqb '.user_data.data' | b64dec >"$stored" 2>/dev/null || true
    sha_got=$(sha256 <"$stored")
    if [ "$sha_want" = "$sha_got" ]; then row "user_data round trip at max size" "intact" ""; else row "user_data round trip at max size" "MISMATCH" "limit may be enforced by truncation"; fi
  fi
  # Create with the maximum size (instance C); delete B first to keep at most 3 instances.
  if [ -n "$B_ID" ]; then
    delete_instance_now "$B_ID"
    B_ID=""
  fi
  if create_instance C "$c_ud"; then
    body="$WORK/ud-c-stored.txt"
    api GET "/instances/$C_ID/user-data"
    jqb '.user_data.data' | b64dec >"$body" 2>/dev/null || true
    if [ "$(sha256 <"$c_ud")" = "$(sha256 <"$body")" ]; then stored="intact"; else stored="MISMATCH"; fi
    row "user_data max on create ($lo bytes)" "accepted, stored $stored" "same limit for create and PATCH"
    delete_instance_now "$C_ID"
    C_ID=""
  else
    row "user_data max on create ($lo bytes)" "rejected: $API_STATUS $(api_err)" "create limit differs from PATCH: re-run with smaller sizes"
  fi
}

check_scrub() {
  local stub="$WORK/ud-stub.txt" body="$WORK/body-scrub.json" t0 deadline seen="no" before v1json
  before=$(ssh_x "$A_PUB" "curl -s -m 5 http://169.254.169.254/latest/user-data | grep -c SPIKE_SECRET_MARKER || true" 2>/dev/null || ssh_unknown "$A_PUB")
  printf '#cloud-config\n# scrubbed by tent spike\n' >"$stub"
  jq -n --arg u "$(b64enc <"$stub")" '{user_data: $u}' >"$body"
  t0=$(now)
  api PATCH "/instances/$A_ID" "$body"
  if ! api_ok; then
    row "Scrub user_data via PATCH" "PATCH failed: $API_STATUS $(api_err)" "scrubbing impossible"
    return 0
  fi
  deadline=$((t0 + 300))
  while [ "$(now)" -lt "$deadline" ]; do
    if ssh_x "$A_PUB" "curl -s -m 5 http://169.254.169.254/latest/user-data | grep -q 'scrubbed by tent spike'" >/dev/null 2>&1; then
      seen="yes, after $(($(now) - t0))s"
      break
    fi
    ssh_ok "$A_PUB" || { seen="$(ssh_unknown "$A_PUB")"; break; }
    sleep 5
  done
  v1json=$(ssh_x "$A_PUB" "curl -s -m 5 http://169.254.169.254/v1.json | grep -c SPIKE_SECRET_MARKER || true" 2>/dev/null || ssh_unknown "$A_PUB")
  row "Metadata serves PATCHed user_data (/latest/user-data)" "$seen (secret marker lines before: $before)" "Nodes.ScrubUserData (ADR-0018)"
  row "Secret marker still in /v1.json after scrub" "$v1json (0 = gone)" ""
}

check_halt() {
  local t0 deadline stopped="?" running="?" start_res mode rc_before rc_after cached ud_after unit iid_before iid_after back
  unit=$(ssh_x "$A_PUB" 'systemctl is-active spike-stop-marker.service; rm -f /var/lib/spike/graceful-stop; sync' 2>/dev/null | head -1 || ssh_unknown "$A_PUB")
  rc_before=$(ssh_x "$A_PUB" 'wc -l < /var/lib/spike/runcmd.log 2>/dev/null || echo 0' 2>/dev/null | tr -d ' ' || echo "?")
  iid_before=$(ssh_x "$A_PUB" 'cat /var/lib/cloud/data/instance-id' 2>/dev/null || echo "?")
  ssh_close "$A_PUB"
  t0=$(now)
  api POST "/instances/$A_ID/halt"
  if ! api_ok; then
    row "halt" "request failed: $API_STATUS $(api_err)" ""
    return 0
  fi
  deadline=$((t0 + 300))
  while [ "$(now)" -lt "$deadline" ]; do
    api GET "/instances/$A_ID"
    if [ "$(jqb '.instance.power_status')" = "stopped" ]; then
      stopped="$(($(now) - t0))s"
      break
    fi
    sleep 2
  done
  t0=$(now)
  api POST "/instances/$A_ID/start"
  start_res="HTTP $API_STATUS $(api_err)"
  deadline=$((t0 + 300))
  while [ "$(now)" -lt "$deadline" ]; do
    api GET "/instances/$A_ID"
    if [ "$(jqb '.instance.power_status + "/" + .instance.server_status')" = "running/ok" ]; then
      running="$(($(now) - t0))s"
      break
    fi
    sleep 3
  done
  if wait_ssh "$A_PUB" 900; then back="$(($(now) - t0))s"; else back="$(ssh_unknown "$A_PUB")"; fi
  row "start after halt" "$start_res; running/ok after $running; SSH after $back" "restart path works"
  if [ -z "$A_T_SSH" ] || ! ssh_ok "$A_PUB"; then
    row "halt semantics" "unknown: SSH did not come back; power_status=stopped after $stopped" ""
    return 0
  fi
  mode=$(ssh_x "$A_PUB" 'test -f /var/lib/spike/graceful-stop && echo graceful || echo hard' 2>/dev/null || echo "?")
  rc_after=$(ssh_x "$A_PUB" 'wc -l < /var/lib/spike/runcmd.log 2>/dev/null || echo 0' 2>/dev/null | tr -d ' ' || echo "?")
  iid_after=$(ssh_x "$A_PUB" 'cat /var/lib/cloud/data/instance-id' 2>/dev/null || echo "?")
  cached=$(ssh_x "$A_PUB" "grep -c 'scrubbed by tent spike' /var/lib/cloud/instance/user-data.txt 2>/dev/null || true" 2>/dev/null || echo "?")
  ud_after=$(ssh_x "$A_PUB" "curl -s -m 5 http://169.254.169.254/latest/user-data | head -c 60" 2>/dev/null | tr '\n' ' ' || true)
  ssh_x "$A_PUB" 'journalctl --list-boots --no-pager 2>/dev/null | tail -3; echo; journalctl -b -1 -n 30 --no-pager -o short-monotonic 2>/dev/null' 2>/dev/null |
    detail "previous boot journal tail (instance A, after halt)" || true
  row "halt semantics" "$mode (ExecStop marker; unit before halt: $unit), power_status=stopped after $stopped" "GracefulShutdown capability (ADR-0017)"
  row "cloud-init after PATCH + restart" "runcmd.log lines $rc_before -> $rc_after; instance-id $iid_before -> $iid_after; stub in instance/user-data.txt: $cached" \
    "must not re-run old or new user_data"
  row "Metadata user_data after restart" "\`$ud_after\`" "scrub persists across reboots?"
}

# ---------------------------------------------------------------------------------------------------------------
# ADR-0023 facts and the node primitives (docs/platform-notes.md §3.3, §3.5, §3.6, §3.10). sshdup, lengths and rules
# need no instance; fwinuse, patchtags, vpcpending and halttwice need only instance A and no SSH.

check_sshdup() { # a second SSH key with the same key material under another name; its name carries RUN_TAG, so
  # cleanup finds it by name when the answer had no usable id
  local body="$WORK/body-sshdup.json" name id res copies impact="a second cluster with the same operator key"
  if [ "$SSH_NAME_USED" = "$SSH_MARKER" ]; then
    name="tent:cluster=$RUN_TAG;kind=ssh-key;fp=$SSH_FP;op=$(uuid4)"
  else
    name="$RUN_TAG-dup"
  fi
  jq -n --arg n "$name" --rawfile k "$SSH_KEY.pub" '{name: $n, ssh_key: ($k | rtrimstr("\n"))}' >"$body"
  api POST /ssh-keys "$body"
  if ! api_ok; then
    row "Second SSH key, same key material" "refused: $API_STATUS $(api_err)" "$impact"
    return 0
  fi
  id=$(jqb '.ssh_key.id // empty' 2>/dev/null || true)
  if [ -n "$id" ] && [ "$id" != "$SSH_KEY_ID" ]; then record_resource ssh-key "$id"; fi
  api GET "/ssh-keys?per_page=500"
  copies=$(jq -r --arg d "$(awk '{print $2}' "$SSH_KEY.pub")" \
    '[.ssh_keys[]? | select(((.ssh_key // "") | split(" ") | .[1] // "") == $d)] | length' "$API_BODY" 2>/dev/null || echo '?')
  if [ "$id" = "$SSH_KEY_ID" ]; then res="accepted, but returned the first key's id"; else res="accepted: second key ${id:-without id}"; fi
  row "Second SSH key, same key material" "$res; the account lists $copies key(s) with this material" "$impact"
}

# marker_text KIND EXTRA N: a tent marker (20-character cluster, EXTRA fields, op) and a trailing ";name=" field of
# lower-case letters, cut to N characters.
marker_text() {
  local t
  t="tent:cluster=$RUN_TAG-ln;kind=$1$2;op=$(uuid4);name=$(rand_chars 'a-z' "$3")"
  printf '%s' "${t:0:$3}"
}

check_lengths() { # the longest text stored verbatim in a VPC description, a firewall group description, an SSH key name
  local kind title id path method key get extra orig n text got prev st res longest first all
  local body="$WORK/body-text.json"
  ensure_fg || true
  for kind in vpc firewall ssh-key; do
    # The methods and bodies govultr v3.33.0 sends: VPC and firewall group PUT, SSH key PATCH.
    case "$kind" in
      vpc) title="VPC description" id="$VPC_ID" method=PUT key=description get=.vpc.description extra="" orig="$VPC_MARKER" ;;
      firewall) title="firewall group description" id="$FG_ID" method=PUT key=description get=.firewall_group.description extra=";role=server" orig="$FG_MARKER" ;;
      ssh-key) title="SSH key name" id="$SSH_KEY_ID" method=PATCH key=name get=.ssh_key.name extra=";fp=$SSH_FP" orig="$SSH_NAME_USED" ;;
    esac
    if [ -z "$id" ]; then
      row "Longest $title stored verbatim" "skipped (no object)" ""
      continue
    fi
    case "$kind" in vpc) path="/vpcs/$id" ;; firewall) path="/firewalls/$id" ;; ssh-key) path="/ssh-keys/$id" ;; esac
    longest="none" first="" all="" prev="$orig"
    for n in 64 99 128 200 255 256 512; do
      text=$(marker_text "$kind" "$extra" "$n")
      jq -n --arg k "$key" --arg t "$text" '{($k): $t}' >"$body"
      api "$method" "$path" "$body"
      st="$API_STATUS"
      got=""
      if ! api_ok; then
        res="rejected: $st $(api_err)"
      else
        api GET "$path"
        got=$(jqb "$get // empty" 2>/dev/null || true)
        if [ "$got" = "$text" ]; then
          res="verbatim"
        elif [ "$got" = "$prev" ]; then
          res="accepted ($st) but not changed"
        elif [ -n "$got" ] && [ "${text:0:${#got}}" = "$got" ]; then
          res="accepted ($st), truncated to ${#got}"
        else
          res="accepted ($st), stored ${#got} other characters"
        fi
        prev="$got"
      fi
      all="$all$n: $res
"
      case "$res" in accepted*) all="$all  sent:   $text
  stored: $got
" ;; esac
      if [ "$res" = "verbatim" ]; then longest="$n" first=""; elif [ -z "$first" ]; then first="$n: $res"; fi
    done
    jq -n --arg k "$key" --arg t "$orig" '{($k): $t}' >"$body"
    api "$method" "$path" "$body"
    api_ok || row "Restore the $title marker" "failed: $API_STATUS $(api_err)" "the markers check reads it"
    row "Longest $title stored verbatim (64-512)" "$longest${first:+; first longer: $first}" "markers with op reach 99 characters"
    printf '%s' "$all" | detail "lengths: $title ($id)"
  done
}

check_rules() { # how Vultr lists rules sent as tent sends them, and whether it takes the same rule twice
  # tent's form (internal/cloud/vultr/firewall.go): lower-case protocol, the port as text, no port for ICMP, no notes.
  local sent='[
    {"ip_type": "v4", "protocol": "tcp", "subnet": "203.0.113.7", "subnet_size": 32, "port": "22"},
    {"ip_type": "v6", "protocol": "tcp", "subnet": "2001:db8::", "subnet_size": 48, "port": "22"},
    {"ip_type": "v4", "protocol": "icmp", "subnet": "0.0.0.0", "subnet_size": 0},
    {"ip_type": "v6", "protocol": "icmp", "subnet": "::", "subnet_size": 0},
    {"ip_type": "v4", "protocol": "tcp", "subnet": "0.0.0.0", "subnet_size": 0, "port": "4646"}]'
  local body="$WORK/body-rule.json" listed="$WORK/rules-listed.json" i=0 n text res st copies ids=()
  local impact="tent compares rules as text; a listed form it does not normalise plans an update every run"
  if ! ensure_fg; then
    row "Rule listing" "skipped (no firewall group)" "$impact"
    return 0
  fi
  n=$(jq -n --argjson s "$sent" '$s | length')
  while [ "$i" -lt "$n" ]; do
    jq -n --argjson s "$sent" --argjson i "$i" '$s[$i]' >"$body"
    api POST "/firewalls/$FG_ID/rules" "$body"
    if api_ok; then ids[i]=$(jqb '.firewall_rule.id // empty' 2>/dev/null || true); else ids[i]="refused: $API_STATUS $(api_err)"; fi
    i=$((i + 1))
  done
  api GET "/firewalls/$FG_ID/rules?per_page=500"
  cp "$API_BODY" "$listed"
  { jq . "$listed" 2>/dev/null || cat "$listed"; } | detail "firewall rules as Vultr lists them (group $FG_ID)"
  i=0
  while [ "$i" -lt "$n" ]; do
    text=$(jq -rn --argjson s "$sent" --argjson i "$i" \
      '$s[$i] | "\(.ip_type) \(.protocol) \(.subnet)/\(.subnet_size)" + (if .port then " \(.port)" else "" end)')
    case "${ids[i]}" in
      refused:*) res="create ${ids[i]}" ;;
      *)
        # Match by the created id, or by position when the create answer had none.
        res=$(jq -r --argjson s "$sent" --argjson i "$i" --arg id "${ids[i]}" '
          $s[$i] as $w
          | (if $id != "" then [.firewall_rules[]? | select((.id | tostring) == $id)][0] else .firewall_rules[$i] end) as $l
          | if $l == null then "not listed"
            else ["ip_type", "protocol", "subnet", "subnet_size", "port"]
              | map(. as $k | {k: $k, w: ($w[$k] // "" | tostring), l: ($l[$k] // "" | tostring)})
              | (map("\(.k)=\(.l)") | join(" ")) + "; "
                + ([.[] | select(.w != .l) | "\(.k) \(.w) -> \(.l)"]
                   | if length == 0 then "same as sent" else "DIFFERS: " + join(", ") end)
            end' "$listed" 2>/dev/null || echo "listing is not JSON (HTTP $API_STATUS)")
        ;;
    esac
    row "Rule listed: $text" "$res" "$impact"
    i=$((i + 1))
  done
  jq -n --argjson s "$sent" '$s[0]' >"$body"
  api POST "/firewalls/$FG_ID/rules" "$body"
  st="$API_STATUS"
  if api_ok; then
    api GET "/firewalls/$FG_ID/rules?per_page=500"
    copies=$(jq -r '[.firewall_rules[]? | select(.subnet == "203.0.113.7")] | length' "$API_BODY" 2>/dev/null || echo '?')
    res="accepted ($st); the group lists $copies rule(s) from 203.0.113.7"
  else
    res="refused: $(answer) (tent counts it as done: $(rule_defined))"
  fi
  row "Same rule added twice (v4 tcp 203.0.113.7/32 22)" "$res" \
    "tent counts 400 \"This rule is already defined\" as done, so a retried rule create succeeds; any other refusal fails it"
}

rule_defined() { # does the last answer match tent's ruleDefined (400, "This rule is already defined" in any case)?
  local msg
  msg=$(jq -r '(.error // "") | gsub("^\\s+|\\s+$"; "") | ascii_downcase' "$API_BODY" 2>/dev/null || true)
  if [ "$API_STATUS" = "400" ] && [ "$msg" = "this rule is already defined" ]; then echo yes; else echo no; fi
}

a_fg() { api GET "/instances/$A_ID"; jqb '.instance.firewall_group_id' 2>/dev/null || echo '?'; }

in_use_rule() { # does the last DELETE answer match tent's ErrInUse (409, 423, or a 4xx saying "are attached"/"in use")?
  local msg
  case "$API_STATUS" in
    409 | 423) echo yes; return 0 ;;
    5??) echo "no (5xx: retried as unavailable)"; return 0 ;;
  esac
  msg=$(api_err)
  if grep -Eiq '(^|[^[:alnum:]_])(are attached|in use)([^[:alnum:]_]|$)' <<<"$msg"; then echo yes; else echo no; fi
}

fg_count() { # fg_count FILE LABEL: instance_count of the group object in FILE: a number, null, "missing", or LABEL
  jq -r --arg l "$2" 'if type != "object" then $l elif has("instance_count") then (.instance_count | tostring) else "missing" end' \
    "$1" 2>/dev/null || echo '?'
}

fg_counts() { # fg_counts ID: sets FG_COUNTS to the group's instance_count in GET /firewalls and GET /firewalls/{id};
  # returns 0 when both are at least 1. Leaves the group's JSON from each in $WORK/fg-list.json and $WORK/fg-one.json
  local list one
  api GET "/firewalls?per_page=500"
  jq --arg id "$1" '[.firewall_groups[]? | select(.id == $id)][0]' "$API_BODY" >"$WORK/fg-list.json" 2>/dev/null ||
    cp "$API_BODY" "$WORK/fg-list.json"
  if api_ok; then list=$(fg_count "$WORK/fg-list.json" "not listed"); else list="HTTP $API_STATUS"; fi
  api GET "/firewalls/$1"
  jq '.firewall_group' "$API_BODY" >"$WORK/fg-one.json" 2>/dev/null || cp "$API_BODY" "$WORK/fg-one.json"
  if api_ok; then one=$(fg_count "$WORK/fg-one.json" "no firewall_group"); else one="HTTP $API_STATUS"; fi
  FG_COUNTS="GET /firewalls $list, GET /firewalls/{id} $one"
  case "$list/$one" in [1-9]*/[1-9]*) return 0 ;; *) return 1 ;; esac
}

check_fwinuse() { # Vultr's answer to the delete of a firewall group that instance A uses
  local body="$WORK/body-fwinuse.json" id t0 deadline attached="not shown within 120 s" counts first="" del in_use fg1 fg2 left
  local impact="tent retries ErrInUse (409, 423, a 4xx that says attached or in use); anything else fails the delete"
  if ! create_fg "tent:cluster=$RUN_TAG;kind=firewall;role=client;op=$(uuid4)"; then
    row "Delete a firewall group in use" "skipped: group create failed: $API_STATUS $(api_err)" "$impact"
    return 0
  fi
  id="$NEW_FG"
  jq -n --arg f "$id" '{firewall_group_id: $f}' >"$body"
  t0=$(now)
  api PATCH "/instances/$A_ID" "$body"
  if ! api_ok; then
    row "Delete a firewall group in use" "skipped: attach failed: $API_STATUS $(api_err)" "$impact"
    return 0
  fi
  deadline=$((t0 + 120))
  while [ "$(now)" -lt "$deadline" ]; do
    if [ "$(a_fg)" = "$id" ]; then
      attached="shown after $(($(now) - t0))s"
      break
    fi
    sleep 3
  done
  # instance_count may lag the attach: read it for up to 30 s until both answers count A.
  t0=$(now)
  deadline=$((t0 + 30))
  until fg_counts "$id" || [ "$(now)" -ge "$deadline" ]; do
    [ -n "$first" ] || first="$FG_COUNTS"
    sleep 3
  done
  if [ -z "$first" ]; then
    counts="$FG_COUNTS"
  elif [ "$first" = "$FG_COUNTS" ]; then
    counts="$FG_COUNTS (the same for $(($(now) - t0))s)"
  else
    counts="at first $first; after $(($(now) - t0))s $FG_COUNTS"
  fi
  row "Firewall group attached to A" "attach $attached; instance_count: $counts" \
    "the dedupe's keep rule counts instances per group (ADR-0023)"
  {
    printf 'GET /firewalls?per_page=500, the entry of this group:\n'
    jq . "$WORK/fg-list.json" 2>/dev/null || cat "$WORK/fg-list.json"
    printf '\nGET /firewalls/%s, .firewall_group:\n' "$id"
    jq . "$WORK/fg-one.json" 2>/dev/null || cat "$WORK/fg-one.json"
  } | detail "firewall group $id while instance A uses it"
  api DELETE "/firewalls/$id"
  del="HTTP $API_STATUS"
  in_use="n/a"
  if ! api_ok; then
    del="$del $(api_err)"
    in_use=$(in_use_rule)
  fi
  fg1=$(a_fg)
  api GET "/firewalls/$id"
  left="$API_STATUS"
  if api_ok; then
    row "Delete a firewall group in use" "$del (matches tent's ErrInUse: $in_use); the group still exists" "$impact"
    jq -n '{firewall_group_id: ""}' >"$body"
    api PATCH "/instances/$A_ID" "$body"
    row "Detach the group from A (PATCH firewall_group_id \"\")" "HTTP $API_STATUS $(api_err)" "cleanup deletes the group"
  else
    sleep 15
    fg2=$(a_fg)
    row "Delete a firewall group in use" "$del, deleted while attached (GET group: HTTP $left); A's firewall_group_id then \`$fg1\`, after 15 s \`$fg2\`" "$impact"
  fi
}

a_state() { # tags, features and firewall group of instance A, as compact JSON
  api GET "/instances/$A_ID"
  jq -c '.instance | {tags, features, firewall_group_id}' "$API_BODY" 2>/dev/null || echo 'null'
}

patch_probe() { # patch_probe TITLE BODY_FILE BEFORE_JSON: PATCH A, then compare its state with BEFORE after 15 s
  local st first later verdict seen impact="tent's scrub and firewall PATCHes must send the current tags if null clears them"
  api PATCH "/instances/$A_ID" "$2"
  st="$API_STATUS"
  if ! api_ok; then
    row "$1" "refused: $st $(api_err)" "$impact"
    return 0
  fi
  first=$(a_state)
  sleep 15
  later=$(a_state)
  verdict=$(jq -rn --argjson b "$3" --argjson a "$later" '
    (if (($a.tags // []) | sort) == (($b.tags // []) | sort) then "tags kept"
     elif (($a.tags // []) | length) == 0 then "tags CLEARED" else "tags CHANGED" end)
    + ", " + (if $a.features == $b.features then "features unchanged" else "features CHANGED" end)' 2>/dev/null || echo '?')
  if [ "$first" = "$later" ]; then seen="after \`$later\`"; else seen="right after \`$first\`, after 15 s \`$later\`"; fi
  row "$1" "HTTP $st; $verdict. Before \`$3\`, $seen" "$impact"
}

check_patchtags() { # does the PATCH govultr sends ("tags": null, "ddos_protection": null) clear A's tags?
  local body="$WORK/body-patchtags.json" stub="$WORK/ud-patchtags.txt" tags before ud
  tags=$(jq -cn --arg r "$RUN_TAG" --arg op "$(uuid4)" '[$r, "tent/cluster=\($r)", "tent/op=\($op)"]')
  set_tags "$tags"
  if ! api_ok; then
    row "PATCH as govultr sends it" "skipped: setting tags failed: $API_STATUS $(api_err)" ""
    return 0
  fi
  before=$(a_state)
  printf '#cloud-config\n# tent spike patchtags\n' >"$stub"
  # govultr's InstanceUpdateReq has no omitempty on tags and ddos_protection: unset, they go out as null.
  jq -n --arg u "$(b64enc <"$stub")" '{tags: null, ddos_protection: null, user_data: $u}' >"$body"
  patch_probe "PATCH {tags: null, ddos_protection: null, user_data} (govultr, user_data only)" "$body" "$before"
  api GET "/instances/$A_ID/user-data"
  if [ "$(jqb '.user_data.data // empty' 2>/dev/null | b64dec 2>/dev/null || true)" = "$(cat "$stub")" ]; then ud="applied"; else ud="NOT applied"; fi
  row "user_data after that PATCH" "$ud" ""
  set_tags "$tags"
  before=$(a_state)
  jq -n '{tags: null, ddos_protection: null, firewall_group_id: ""}' >"$body"
  patch_probe "PATCH {tags: null, ddos_protection: null, firewall_group_id: \"\"} (firewall group only)" "$body" "$before"
  # cleanup's tag fallback looks for RUN_TAG
  set_tags "$(jq -cn --arg r "$RUN_TAG" '[$r]')"
  api_ok || row "Restore A's tags" "failed: $API_STATUS $(api_err)" "cleanup deletes A by its recorded id"
}

# What GET /instances/{A}/vpcs answers while A boots, from right after the create answer until it lists an address
# other than 0.0.0.0 (at most 120 s). Records each distinct answer with the instance's state and the seconds since
# the create request. Sets A's VPC IP milestone (and A's API milestone, if reached), so measure_boot keeps them.
check_vpcpending() {
  local deadline=$((A_T0 + 120)) t ans cur last="" seq="" got404="no" found=0 addr
  local raw="$WORK/vpcpending.txt"
  : >"$raw"
  while :; do
    api GET "/instances/$A_ID/vpcs"
    t=$(($(now) - A_T0))
    if api_ok; then
      ans="HTTP $API_STATUS $(jq -r 'if (.vpcs | type) != "array" then "no vpcs array"
        elif (.vpcs | length) == 0 then "vpcs []" else "ip " + ([.vpcs[] | .ip_address // "null"] | join(",")) end' \
        "$API_BODY" 2>/dev/null || echo 'not JSON')"
    else
      ans=$(answer)
    fi
    [ "$API_STATUS" != "404" ] || got404="yes"
    if note_vpc_ip A; then found=1; fi
    printf '%ss: %s\n' "$t" "$(oneline 400 <"$API_BODY")" >"$WORK/vpcpending-last.txt"
    read_state A
    cur="$ans, instance $INST_STATE"
    if [ "$cur" != "$last" ]; then
      seq="$seq${seq:+; }${t}s $cur"
      { cat "$WORK/vpcpending-last.txt"; printf '  instance: %s\n' "$INST_STATE"; } >>"$raw"
      last="$cur"
    fi
    [ "$found" = 0 ] || break
    [ "$(now)" -lt "$deadline" ] || break
    sleep 1
  done
  if [ "$found" = 1 ]; then addr="address after ${A_T_IP}s"; else addr="no address within 120 s"; fi
  row "GET /instances/{id}/vpcs while A boots" "404 before the address: $got404; $addr. Answers: $seq" \
    "tent's List must not treat a 404 on /vpcs as a deleted instance"
  detail "GET /instances/$A_ID/vpcs while A boots (seconds since the create request)" <"$raw"
}

# Halts A, waits until it is stopped (at most 60 s), and halts it again. Leaves A stopped: it runs last.
check_halttwice() {
  local t0 deadline first second stopped="not stopped within 60 s" after
  local impact="Stop sends a bodyless POST that Go 1.26's HTTP/2 client may send twice; a second halt must not be an error"
  [ -z "$A_PUB" ] || ssh_close "$A_PUB"
  t0=$(now)
  api POST "/instances/$A_ID/halt"
  first=$(answer)
  if ! api_ok; then
    row "Halt A twice" "first halt refused: $first" "$impact"
    return 0
  fi
  deadline=$((t0 + 60))
  while [ "$(now)" -lt "$deadline" ]; do
    api GET "/instances/$A_ID"
    if [ "$(jqb '.instance.power_status' 2>/dev/null || true)" = "stopped" ]; then
      stopped="stopped after $(($(now) - t0))s"
      break
    fi
    sleep 2
  done
  api POST "/instances/$A_ID/halt"
  second=$(answer)
  api GET "/instances/$A_ID"
  after=$(jqb '.instance.status + "/" + .instance.power_status + "/" + .instance.server_status' 2>/dev/null || echo '?')
  row "Halt A twice" "first halt $first; $stopped; second halt $second; then $after" "$impact"
}

check_objstore() {
  local conf="$WORK/s3.conf" url c1 c2 c3 c4 c5 etag
  if [ -z "$S3_ENDPOINT" ] || [ -z "$S3_BUCKET" ] || [ -z "$S3_ACCESS_KEY" ] || [ -z "$S3_SECRET_KEY" ]; then
    row "Object Storage conditional writes" "skipped: S3_ENDPOINT/S3_BUCKET/S3_ACCESS_KEY/S3_SECRET_KEY not set" ""
    return 0
  fi
  if ! curl --help all 2>/dev/null | grep -q -- '--aws-sigv4'; then
    row "Object Storage conditional writes" "skipped: curl lacks --aws-sigv4 (need 7.75+)" ""
    return 0
  fi
  (
    umask 077
    printf 'user = "%s:%s"\naws-sigv4 = "aws:amz:%s:s3"\n' "$S3_ACCESS_KEY" "$S3_SECRET_KEY" "$S3_REGION" >"$conf"
  )
  url="${S3_ENDPOINT%/}/$S3_BUCKET/tent-spike/$RUN/lock"
  s3() { curl -sS -K "$conf" -o "$WORK/s3.out" -D "$WORK/s3.hdr" -w '%{http_code}' --max-time 30 "$@" 2>/dev/null || true; }
  c1=$(s3 -X PUT -H 'If-None-Match: *' --data-binary 'one' "$url")
  c2=$(s3 -X PUT -H 'If-None-Match: *' --data-binary 'two' "$url")
  s3 -I "$url" >/dev/null
  etag=$(awk 'tolower($1)=="etag:"{print $2}' "$WORK/s3.hdr" | tr -d '\r')
  c3=$(s3 -X PUT -H 'If-Match: "00000000000000000000000000000000"' --data-binary 'three' "$url")
  c4=$(s3 -X PUT -H "If-Match: $etag" --data-binary 'four' "$url")
  c5=$(s3 -X DELETE "$url")
  row "Object Storage If-None-Match: *" "first PUT $c1, second PUT $c2 (expect 200 then 412)" "state-store lock (ADR-0010)"
  row "Object Storage If-Match" "wrong ETag $c3 (expect 412), right ETag $c4 (expect 200); cleanup DELETE $c5" "optimistic concurrency"
}

# ---------------------------------------------------------------------------------------------------------------
# Cleanup (EXIT trap): write the report, then delete everything this run created unless --keep.
# Records how long the API refuses to delete the firewall group and the VPC after the instances are gone
# (the retry loop `tent delete cluster` needs).

cleanup() {
  local rc=$? type id deadline left t0 tries first_err ip
  set +e
  write_report
  for ip in "$A_PUB" "$B_PUB" "$V_PUB"; do [ -n "$ip" ] && [ -n "$SOCK_DIR" ] && ssh_close "$ip"; done
  if [ "$MODE" = "run" ] && [ -n "$STATE" ] && [ -s "$STATE" ]; then
    if [ "$KEEP" = 1 ]; then
      log "--keep: resources left in place (delete them yourself):"
      cat "$STATE" >&2
    else
      log "cleaning up"
      while read -r type id; do [ "$type" = "instance" ] && api DELETE "/instances/$id"; done <"$STATE"
      # Fallback: anything still tagged with this run (e.g. created while the script was interrupted).
      api GET "/instances?per_page=500&tag=$(urlencode "$RUN_TAG")"
      for id in $(jq -r '.instances[]?.id' "$API_BODY" 2>/dev/null); do api DELETE "/instances/$id"; done
      deadline=$(($(now) + 300))
      while [ "$(now)" -lt "$deadline" ]; do
        api GET "/instances?per_page=500&tag=$(urlencode "$RUN_TAG")"
        left=$(jq -r '.instances | length' "$API_BODY" 2>/dev/null || echo 0)
        [ "$left" = "0" ] && break
        sleep 5
      done
      for type in firewall vpc ssh-key; do
        while read -r t id; do
          [ "$t" = "$type" ] || continue
          deadline=$(($(now) + 300))
          t0=$(now)
          tries=0
          first_err=""
          while :; do
            tries=$((tries + 1))
            case "$type" in
              firewall) api DELETE "/firewalls/$id" ;;
              vpc) api DELETE "/vpcs/$id" ;;
              ssh-key) api DELETE "/ssh-keys/$id" ;;
            esac
            if api_ok; then
              row "Delete $type after instances are gone" "ok after $(($(now) - t0))s, $tries request(s)${first_err:+; refused before with: $first_err}" \
                "retry loop in delete cluster"
              break
            fi
            [ "$API_STATUS" = "404" ] && break
            [ -n "$first_err" ] || first_err="$API_STATUS $(api_err)"
            if [ "$(now)" -gt "$deadline" ]; then
              log "could not delete $type $id: $API_STATUS $(api_err)"
              row "Delete $type after instances are gone" "FAILED after $tries requests: $API_STATUS $(api_err)" "delete it by hand: $id"
              break
            fi
            sleep 5
          done
        done <"$STATE"
      done
      # Fallback: SSH keys whose name carries the run tag but whose id was not recorded (sshdup's second key when
      # the create answer had no usable id).
      api GET "/ssh-keys?per_page=500"
      for id in $(jq -r --arg t "$RUN_TAG" '.ssh_keys[]? | select((.name // "") | contains($t)) | .id' "$API_BODY" 2>/dev/null); do
        api DELETE "/ssh-keys/$id"
        row "Unrecorded SSH key with the run tag" "deleted $id: $(answer)" "sshdup: the create answer had no usable id"
      done
      log "cleanup done"
      write_report
    fi
  fi
  [ -n "$REPORT" ] && [ -f "$REPORT" ] && log "report: $REPORT"
  [ -n "$SOCK_DIR" ] && rm -rf "$SOCK_DIR"
  [ -n "$WORK" ] && rm -rf "$WORK"
  exit "$rc"
}

# ---------------------------------------------------------------------------------------------------------------

main() {
  local c
  while [ $# -gt 0 ]; do
    case "$1" in
      --preflight) MODE="preflight" ;;
      --dry-run) MODE="dry-run" ;;
      --yes) ASSUME_YES=1 ;;
      --keep) KEEP=1 ;;
      --only) CHECKS="$2"; shift ;;
      --region) REGION="$2"; shift ;;
      --plan) PLAN="$2"; shift ;;
      --out) OUT_DIR="$2"; shift ;;
      -h | --help) usage; exit 0 ;;
      *) usage >&2; die "unknown option: $1" ;;
    esac
    shift
  done
  for c in $(printf '%s' "$CHECKS" | tr ',' ' '); do
    case ",$ALL_CHECKS," in *",$c,"*) ;; *) die "unknown check: $c" ;; esac
  done

  for c in curl jq awk base64 tr od; do need_cmd "$c"; done
  # needs_api: the run creates resources (SSH key, VPC); needs_instances: it also creates instance A.
  local needs_api=0 needs_instances=0
  for c in tags markers userdata fwinuse patchtags vpcpending halttwice $SSH_CHECKS; do
    if want "$c"; then needs_instances=1; fi
  done
  needs_api="$needs_instances"
  for c in sshdup lengths rules; do if want "$c"; then needs_api=1; fi; done
  NEED_SSH=0
  for c in $SSH_CHECKS; do if want "$c"; then NEED_SSH=1; fi; done
  if [ "$MODE" = "run" ] && [ "$needs_api" = 1 ]; then need_cmd ssh-keygen; fi
  if [ "$MODE" = "run" ] && [ "$NEED_SSH" = 1 ]; then need_cmd ssh; fi
  jq -n --rawfile x /dev/null '1' >/dev/null 2>&1 || die "jq 1.6+ is required (--rawfile)"

  RUN=$(lower "$(rand_alnum 6)")
  RUN_TAG="tent-spike-$RUN"
  SECRET_MARKER=$(rand_alnum 24)
  # Markers as tent writes them (internal/cloud/vultr/labels.go); create_ssh_key sets SSH_MARKER with the key's fp.
  VPC_MARKER="tent:cluster=$RUN_TAG;kind=vpc;op=$(uuid4)"
  FG_MARKER="tent:cluster=$RUN_TAG;kind=firewall;role=server;op=$(uuid4)"
  WORK=$(mktemp -d "${TMPDIR:-/tmp}/tent-spike.XXXXXX")
  mkdir -p "$OUT_DIR"
  REPORT="$OUT_DIR/vultr-spike-$(date -u +%Y%m%d-%H%M%S)-$REGION-$RUN.md"
  SUMMARY="$WORK/summary.md"
  DETAILS="$WORK/details.md"
  STATE="$WORK/resources.txt"
  : >"$SUMMARY"
  : >"$DETAILS"
  : >"$STATE"
  trap cleanup EXIT
  trap 'exit 130' INT TERM

  log "tent Vultr spike v$SPIKE_VERSION, run $RUN, region $REGION, plan $PLAN"
  preflight
  [ "$MODE" = "preflight" ] && { log "preflight only: done"; return 0; }
  if [ "$needs_api" = 0 ]; then
    if [ "$MODE" = "run" ] && want objstore; then check_objstore; fi
    log "no Vultr resource checks selected: done"
    return 0
  fi

  local needs_b=0 needs_v=0 instances=0 keys=1 groups=0 cost billed duration keep_note="" lock_note=""
  if [ "$needs_instances" = 1 ]; then
    for c in network firewall alias; do if want "$c"; then needs_b=1; fi; done
    if want boot; then needs_v=1; fi
    instances=$((1 + needs_b + needs_v))
    if want userdata; then instances=$((instances + 1)); fi
  fi
  if want sshdup; then keys=2; fi
  if want firewall; then groups=$((groups + 1)); fi
  if want lengths || want rules; then groups=$((groups + 1)); fi
  if want fwinuse; then groups=$((groups + 1)); fi
  if [ "$needs_instances" = 1 ] && [ "$NEED_SSH" = 0 ]; then
    groups=$((groups + 1))
    lock_note="
  - no selected check needs SSH: the instances get a firewall group with no rules (no inbound traffic)"
  fi
  cost=$(awk -v h="$HOURLY" -v n="$instances" 'BEGIN { printf "%.3f", h * n }')
  case "$instances" in
    0) billed="no instances (nothing billed)" duration="1-3 minutes" ;;
    1) billed="1 instance of $PLAN, 1 hour minimum: about \$$cost" duration="5-15 minutes" ;;
    *) billed="$instances instances of $PLAN (at most 3 at a time), 1 hour minimum each: about \$$cost" duration="20-45 minutes" ;;
  esac
  # Without SSH the checks start once the API reports A ready, about a minute after the create.
  if [ "$instances" -gt 0 ] && [ "$NEED_SSH" = 0 ]; then duration="5-10 minutes"; fi
  if [ "$KEEP" = 1 ]; then keep_note=" (NOT deleted: --keep given)"; fi
  cat >&2 <<EOF

This run creates resources in your Vultr account ($REGION):
  - $billed
  - 1 VPC (plus short-lived test VPCs for mask checks), $keys SSH key(s), $groups firewall group(s)$lock_note
  - all tagged/marked with $RUN_TAG and deleted on exit$keep_note
Expected duration: $duration. Report: $REPORT

EOF
  [ "$MODE" = "dry-run" ] && { log "dry run: nothing created"; return 0; }
  [ -n "${VULTR_API_KEY:-}" ] || die "VULTR_API_KEY is not set"
  AUTH_CONF="$WORK/auth.conf"
  (
    umask 077
    printf 'header = "Authorization: Bearer %s"\n' "$VULTR_API_KEY" >"$AUTH_CONF"
  )
  if [ "$ASSUME_YES" != 1 ]; then
    [ -t 0 ] || die "not a terminal: pass --yes to confirm"
    local ans
    read -r -p "Proceed? [y/N] " ans
    case "$ans" in y | Y | yes) ;; *) die "aborted" ;; esac
  fi

  api GET /account
  api_ok || die "authentication failed: $API_STATUS $(api_err) (check the key and its IP allow-list)"

  if want objstore; then check_objstore; fi
  create_ssh_key
  create_vpc
  # Checks without instances, before any instance exists.
  if want sshdup; then check_sshdup; fi
  if want lengths; then check_lengths; fi
  if want rules; then check_rules; fi
  if [ "$needs_instances" = 0 ]; then
    log "no instance checks selected: done"
    return 0
  fi

  ssh_init
  build_user_data
  if [ "$NEED_SSH" = 0 ]; then create_lockdown_fg; fi
  create_instance A "$WORK/cc-ab.yaml" || die "cannot create instance A: $API_STATUS $(api_err)"
  # Right after A's create answer, before B and V: B's and V's timings count from their own create requests.
  if want vpcpending; then check_vpcpending; fi
  if [ "$needs_b" = 1 ]; then
    if ! create_instance B "$WORK/cc-ab.yaml"; then
      row "Second instance" "create failed: $API_STATUS $(api_err)" "account instance limit? B-dependent checks skipped"
    fi
  fi
  if [ "$needs_v" = 1 ]; then
    if ! create_instance V; then
      row "Third instance (V)" "create failed: $API_STATUS $(api_err)" "account instance limit? vendor-default boot not measured"
    fi
  fi
  measure_boot

  if want inside; then check_inside A; fi
  poll_vendor
  if want metadata; then check_metadata; fi
  poll_vendor
  if want network; then check_network; fi
  poll_vendor
  if want firewall && [ -n "$B_PUB" ]; then check_firewall_defaults; fi
  poll_vendor
  if want alias; then check_alias; fi
  poll_vendor
  if want firewall && [ -n "$B_PUB" ]; then check_firewall_group; fi
  poll_vendor
  if want tags; then check_tags; fi
  if want markers; then check_markers; fi
  poll_vendor
  if want userdata; then check_userdata; fi
  poll_vendor
  if want scrub; then check_scrub; fi
  poll_vendor
  if want halt; then check_halt; fi
  poll_vendor
  # Late: they change A's user_data, tags and firewall group. Each removes A's group, so A gets the group with no
  # rules back right after it.
  if want patchtags; then
    check_patchtags
    relock_a
  fi
  poll_vendor
  if want fwinuse; then
    check_fwinuse
    relock_a
  fi
  finish_vendor
  # Last: it leaves A stopped.
  if want halttwice; then check_halttwice; fi
  log "all checks finished"
}

main "$@"
