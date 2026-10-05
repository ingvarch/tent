#!/usr/bin/env bash
# tent Vultr spike: measures undocumented Vultr behaviour that the provider design depends on
# (docs/adr/0018-vultr-provider-design.md, "provisional" items; docs/platform-notes.md §3, items marked 🔬).
#
# It creates REAL, BILLED resources in your Vultr account (at most 3 instances at a time, 4 in total,
# one VPC, up to three firewall groups, up to two SSH keys) and deletes them on exit unless --keep is given.
# The tentnode check runs alone: one instance, one VPC and one SSH key.
# Usage and details: hack/vultr-spike/README.md
#
# Portable bash (3.2+, macOS default), requires: curl, jq 1.6+, ssh, ssh-keygen, awk, od; tentnode also go and gzip.
set -euo pipefail

readonly SPIKE_VERSION="8"
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
readonly SCRIPT_DIR
REPO_DIR=$(cd "$SCRIPT_DIR/../.." && pwd)
readonly REPO_DIR
readonly API_BASE="${VULTR_API_BASE:-https://api.vultr.com/v2}"
# The checks of a run without --only. tentnode runs only when --only names it alone.
readonly DEFAULT_CHECKS="boot,inside,metadata,network,firewall,alias,tags,markers,userdata,scrub,halt,sshdup,lengths,rules,fwinuse,patchtags,vpcpending,halttwice,objstore"
readonly ALL_CHECKS="$DEFAULT_CHECKS,tentnode"
# The checks that log in to an instance over SSH. Without them, instance A gets a firewall group with no rules.
readonly SSH_CHECKS="boot inside metadata network firewall alias scrub halt tentnode"

REGION="${REGION:-ams}"
PLAN="${PLAN:-vc2-1c-1gb}"
OS_NAME="${OS_NAME:-Ubuntu 24.04 LTS x64}"
OS_ID="${OS_ID:-}"
VPC_SUBNET="${VPC_SUBNET:-10.64.0.0}"
VPC_MASKS="${VPC_MASKS:-16 20 24}"
OUT_DIR="${OUT_DIR:-$SCRIPT_DIR/results}"
CHECKS="${CHECKS:-$DEFAULT_CHECKS}"
READY_TIMEOUT="${READY_TIMEOUT:-900}"
VENDOR_TIMEOUT="${VENDOR_TIMEOUT:-2400}"
USERDATA_TARGET="${USERDATA_TARGET:-65536}"
S3_ENDPOINT="${S3_ENDPOINT:-}"
S3_BUCKET="${S3_BUCKET:-}"
S3_ACCESS_KEY="${S3_ACCESS_KEY:-}"
S3_SECRET_KEY="${S3_SECRET_KEY:-}"
S3_REGION="${S3_REGION:-us-east-1}"
# tentnode: the tent-node under test, as hack/tent-node-upload prints it. TENT_NODE_URL carries a signature: the script
# never logs it. TENT_NODE_VERSION defaults to the version of bin/tent from the same make build.
TENT_NODE_VERSION="${TENT_NODE_VERSION:-}"

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
A_ID="" B_ID="" C_ID="" V_ID="" T_ID=""
A_PUB="" B_PUB="" V_PUB="" T_PUB=""
A_VPC_IP="" B_VPC_IP="" T_VPC_IP=""
# shellcheck disable=SC2034 # read indirectly via getv
A_VPC_MAC="" B_VPC_MAC="" T_VPC_MAC=""
# shellcheck disable=SC2034 # read indirectly via getv
A_T0="" B_T0="" V_T0="" T_T0=""
# shellcheck disable=SC2034 # read indirectly via getv
A_T_OK="" B_T_OK="" V_T_OK="" T_T_OK="" A_T_IP="" B_T_IP="" V_T_IP="" T_T_IP=""
# shellcheck disable=SC2034 # read indirectly via getv
A_T_PORT="" B_T_PORT="" V_T_PORT="" T_T_PORT="" A_T_SSH="" B_T_SSH="" V_T_SSH="" T_T_SSH=""
# shellcheck disable=SC2034 # read indirectly via getv
A_T_CI="" B_T_CI="" V_T_CI="" T_T_CI=""
# shellcheck disable=SC2034 # read indirectly via getv
A_CI_STATUS="" B_CI_STATUS="" V_CI_STATUS="" T_CI_STATUS=""
# shellcheck disable=SC2034 # read indirectly via getv
A_LAST=0 B_LAST=0 V_LAST=0 T_LAST=0
V_DONE=""
TN_UD=""        # tentnode: instance T's user data, from hack/tent-node-userdata
TN_JSON_SHA=""  # tentnode: the sha256 of the node.json in it
TN_REBOOT_S=""  # tentnode: seconds from the reboot to SSH on the new boot
TN_COMMENT=""   # tentnode: the comment of tent's nftables table on the first boot
TN_ALLOC=""     # tentnode: the allocation of the job that ran on the first boot
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
  --only LIST        Comma-separated subset of checks (default: all but tentnode):
                     boot,inside,metadata,network,firewall,alias,tags,markers,userdata,scrub,halt,
                     sshdup,lengths,rules,fwinuse,patchtags,vpcpending,halttwice,objstore
                     ("--only objstore" needs no Vultr API key and creates no instances;
                     sshdup, lengths and rules create no instance; fwinuse, patchtags, vpcpending and
                     halttwice need only instance A. Unless a check that needs SSH is selected (boot,
                     inside, metadata, network, firewall, alias, scrub, halt), A gets a firewall group
                     with no rules at creation and the checks start once the API reports A ready)
                     tentnode is not in the default list and runs alone ("--only tentnode"): it boots
                     instance T with a development build of tent-node and checks it over SSH
  --region ID        Vultr region (default: ams; env REGION).
  --plan ID          Instance plan (default: vc2-1c-1gb; env PLAN).
  --out DIR          Report directory (default: hack/vultr-spike/results, git-ignored; env OUT_DIR).
  -h, --help         Show this help.

Environment:
  VULTR_API_KEY      Required for a real run. Never printed, passed to curl via a 0600 config file.
  OS_NAME / OS_ID    Image by exact name (default "Ubuntu 24.04 LTS x64") or numeric os_id, which wins.
  VPC_SUBNET         VPC network address to try (default 10.64.0.0) with masks VPC_MASKS ("16 20 24").
  READY_TIMEOUT      Seconds to wait for instances A and B to boot (default 900).
  VENDOR_TIMEOUT     Seconds to wait for instance V (no user_data, vendor defaults) to boot (default 2400).
  USERDATA_TARGET    Size in bytes of the user_data of A and B (default 65536, tent's budget).
  S3_ENDPOINT, S3_BUCKET, S3_ACCESS_KEY, S3_SECRET_KEY [, S3_REGION]
                     Optional: run the Object Storage conditional-write check against an EXISTING bucket
                     (e.g. S3_ENDPOINT=https://ams1.vultrobjects.com). Needs curl with --aws-sigv4 (7.75+).
  TENT_NODE_URL, TENT_NODE_SHA256
                     Required by tentnode: the tent-node under test, as "make dev-upload" prints them
                     (hack/tent-node-upload/README.md). The URL is never printed.
  TENT_NODE_VERSION  tentnode: the version of that tent-node (default: bin/tent version, after a check that
                     bin/tent-node_linux_amd64 has the sha256 TENT_NODE_SHA256).
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
oneline() { # oneline [N]: the input on one line, spaces squeezed, without the space of its last line end, N characters
  local s
  s=$(tr '\n' ' ' | tr -s ' ')
  s=${s% }
  printf '%s' "${s:0:${1:-300}}"
}

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
  api GET "/os?per_page=500"
  api_ok || die "GET /os failed: $API_STATUS $(api_err)"
  if [ -z "$OS_ID" ]; then
    OS_ID=$(jq -r --arg n "$OS_NAME" '.os[] | select(.name==$n) | .id' "$API_BODY" | head -1)
    [ -n "$OS_ID" ] || die "image not found by exact name: $OS_NAME (set OS_ID)"
  else
    # The report names the image that OS_ID selects, not the default name.
    OS_NAME=$(jq -r --arg id "$OS_ID" '.os[] | select((.id | tostring) == $id) | .name' "$API_BODY" | head -1)
    [ -n "$OS_NAME" ] || die "unknown os_id: $OS_ID"
  fi
  row "Preflight" "region $REGION ($city); $PLAN deployable; \$$monthly/month = \$$HOURLY/hour (÷672); os_id $OS_ID ($OS_NAME)" \
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
# tentnode: a development build of tent-node on instance T, the combined node of a cluster of one node, booted with the
# user data of hack/tent-node-userdata, checked on a real machine. TENT_NODE_URL carries a signature: nothing logs it,
# and hide_url masks it in what the report takes from the instance. The user data carries the node's key and the
# gossip key of a CA that the tool makes for this run alone, and the ACL token stays in a root-only file on T: the
# report shows none of them, and T is deleted at the end.

# The phases of up in the order they run, as status.json lists them.
readonly TN_PHASES="preflight system hostfirewall runtime cni join nomad verify"
# The plugins of Nomad's bridge network, which the cni phase must put into /opt/cni/bin.
readonly TN_PLUGINS="bridge firewall host-local loopback portmap"
# The files that hold a secret: tn_files records their size, not their sha256. node.json holds the same secrets but
# keeps its sha256, which the checks compare with the user data's: a sha256 of random keys reveals nothing of them.
readonly TN_SECRET_FILES="/etc/nomad.d/01-gossip.hcl /etc/nomad.d/tls/agent-key.pem"
# The files that tent-node's install, up and refresh-join write; of /opt/cni/bin, the plugins of Nomad's bridge
# network. After a reboot, up must change none but status.json.
TN_FILES="/etc/tent/node.json /usr/local/bin/tent-node /etc/systemd/system/tent-node.service
/etc/systemd/system/tent-node-join.service /etc/systemd/system/tent-node-join.timer /etc/modules-load.d/tent.conf
/etc/sysctl.d/99-tent.conf /etc/systemd/journald.conf.d/tent.conf /etc/tent/firewall.nft /etc/docker/daemon.json
/var/lib/tent/assets/cni-plugins $(for p in $TN_PLUGINS; do printf '/opt/cni/bin/%s ' "$p"; done)
/var/lib/tent/assets/nomad /usr/local/bin/nomad /etc/systemd/system/nomad.service /etc/nomad.d/00-tent.hcl
/etc/nomad.d/01-gossip.hcl /etc/nomad.d/05-join.hcl /etc/nomad.d/10-node.hcl /etc/nomad.d/11-instance.hcl
/etc/nomad.d/tls/ca.pem /etc/nomad.d/tls/agent.pem /etc/nomad.d/tls/agent-key.pem /var/lib/tent/peers.json
/var/lib/tent/status.json"
readonly TN_FILES
# The ACL token that the bootstrap on T makes, in a root-only file on T.
readonly TN_TOKEN_FILE="/root/tn-acl-token"
# The start of a script on T that runs the nomad CLI against the local agent over mTLS with the node's certificate,
# and with the ACL token once the bootstrap has made it. The token stays on T: only T's shell reads the file.
readonly TN_NOMAD_SH="export NOMAD_ADDR=https://127.0.0.1:4646 NOMAD_CACERT=/etc/nomad.d/tls/ca.pem \
NOMAD_CLIENT_CERT=/etc/nomad.d/tls/agent.pem NOMAD_CLIENT_KEY=/etc/nomad.d/tls/agent-key.pem
if [ -s $TN_TOKEN_FILE ]; then NOMAD_TOKEN=\$(cat $TN_TOKEN_FILE); export NOMAD_TOKEN; fi
"
# The job of the check: a web server in busybox on Nomad's bridge with a dynamic port, which answers TN_JOB_BODY.
readonly TN_JOB="tn-web"
readonly TN_JOB_BODY="tent-node-check"
# The metadata probes: the image of their containers, the URL they must not reach, and a URL that a container with a
# network reaches, so that a probe that fails for want of any network is not taken for a block. The verdicts match
# the messages of busybox 1.38's wget: "download timed out" and "server returned error".
readonly TN_IMAGE="busybox:1.38"
readonly TN_METADATA_URL="http://169.254.169.254/v1.json"
readonly TN_CONTROL_URL="http://archive.ubuntu.com/ubuntu/"

hide_url() { sed -E 's/(X-Amz-(Signature|Credential|Security-Token))=[^&[:space:]"'"'"']*/\1=[hidden]/g'; }

# prepare_tentnode: checks the variables of the tent-node under test, finds its version, writes instance T's user
# data to TN_UD and sets TN_JSON_SHA to the sha256 of the node.json in it.
prepare_tentnode() {
  local bin="$REPO_DIR/bin/tent-node_linux_amd64" sum bytes cidr
  [ -n "${TENT_NODE_URL:-}" ] && [ -n "${TENT_NODE_SHA256:-}" ] ||
    die "tentnode needs TENT_NODE_URL and TENT_NODE_SHA256: run make dev-upload (hack/tent-node-upload/README.md)"
  if [ -z "$TENT_NODE_VERSION" ]; then
    # bin/tent and bin/tent-node_linux_amd64 come from one make build, so they carry one version, as long as the
    # uploaded tent-node is that build's.
    [ -x "$REPO_DIR/bin/tent" ] && [ -f "$bin" ] || die "set TENT_NODE_VERSION, or run make dev-upload first"
    sum=$(sha256 <"$bin")
    [ "$sum" = "$TENT_NODE_SHA256" ] ||
      die "bin/tent-node_linux_amd64 is not the tent-node with TENT_NODE_SHA256: run make dev-upload again, or set TENT_NODE_VERSION"
    TENT_NODE_VERSION=$("$REPO_DIR/bin/tent" version -o json | jq -r .version) || die "bin/tent version failed"
  fi
  # The user data holds the node's key and the gossip key: it stays in WORK, which only this user reads and cleanup
  # removes.
  TN_UD="$WORK/tentnode-ud.yaml"
  # The name is instance T's host name: create_instance names T $RUN_TAG-t. The CIDR is the VPC's: tentnode makes one
  # VPC, with the first of VPC_MASKS. On Vultr the zone is the region.
  cidr="$VPC_SUBNET/${VPC_MASKS%% *}"
  (cd "$REPO_DIR" && go run ./hack/tent-node-userdata -name "$RUN_TAG-t" -version "$TENT_NODE_VERSION" \
    -cidr "$cidr" -zone "$REGION") >"$TN_UD" || die "hack/tent-node-userdata failed"
  TN_JSON_SHA=$(awk '/encoding: gz\+b64/ {f = 1} f && $1 == "content:" {print $2; exit}' "$TN_UD" | b64dec |
    gzip -dc | sha256) || die "the user data holds no gz+b64 node.json"
  bytes=$(wc -c <"$TN_UD" | tr -d ' ')
  row "tent-node under test" "version $TENT_NODE_VERSION, sha256 $TENT_NODE_SHA256; user data $bytes bytes, node.json sha256 $TN_JSON_SHA; cluster CIDR $cidr, zone $REGION" \
    "the combined node of a cluster of one node: Nomad, the CNI plugins and tent-node, Docker, tent's host firewall for the CIDR, and a CA, certificate and gossip key made for this run"
}

tn_ssh() { ssh_x "$T_PUB" "$@" 2>/dev/null; } # tn_ssh COMMAND...: runs on instance T

tn_out() { # tn_out COMMAND: runs COMMAND on T and prints its output, or why SSH failed when there is none
  local out
  out=$(tn_ssh "$1" || true)
  if [ -n "$out" ]; then printf '%s\n' "$out"; else ssh_unknown "$T_PUB"; fi
}

tn_files() { # the modification time and sha256 of each of TN_FILES on T, one line each; of a secret file, its size
  tn_ssh "for f in $(printf '%s' "$TN_FILES" | tr '\n' ' '); do
    if [ ! -e \"\$f\" ]; then printf '%s missing\n' \"\$f\"; continue; fi
    case ' $TN_SECRET_FILES ' in
      *\" \$f \"*) s=\"size \$(stat -c %s \"\$f\")\" ;;
      *) s=\"sha256 \$(sha256sum <\"\$f\" | cut -d' ' -f1)\" ;;
    esac
    printf '%s mtime %s %s\n' \"\$f\" \"\$(stat -c %Y \"\$f\")\" \"\$s\"
  done"
}

tn_missing() { # tn_missing FILE: the files that a tn_files snapshot in FILE lists as missing, space-separated
  sed -n 's/^\([^ ]*\) missing$/\1/p' "$1" | paste -sd ' ' -
}

tn_status() { # tn_status FILE: reads T's status.json into FILE and prints its phases as "name status (reason)",
  # "missing" when T has none, or why SSH failed
  # Only SSH fails the command: a missing file is an empty answer.
  if ! tn_ssh 'cat /var/lib/tent/status.json 2>/dev/null; true' >"$1"; then
    : >"$1"
    ssh_unknown "$T_PUB"
    return 0
  fi
  [ -s "$1" ] || { printf 'missing'; return 0; }
  jq -r '[.phases[]? | "\(.name) \(.status)" + (if .reason then " (\(.reason))" else "" end)] | join(", ")' "$1" 2>/dev/null ||
    printf 'unreadable'
}

tn_phases_are() { # tn_phases_are FILE STATUS...: are the phases in FILE those of TN_PHASES, in order, with the
  # statuses given in that order?
  local f="$1" want="" p
  shift
  for p in $TN_PHASES; do
    want="$want${want:+, }$p ${1:-?}"
    if [ $# -gt 0 ]; then shift; fi
  done
  [ "$(jq -r '[.phases[]? | "\(.name) \(.status)"] | join(", ")' "$f" 2>/dev/null || true)" = "$want" ]
}

tn_instance() { # tn_instance FILE: the instance and the version in status.json against the API and the tent-node under test
  [ -s "$1" ] || { echo "no status.json"; return 0; }
  jq -r --arg id "$T_ID" --arg zone "$REGION" --arg ip "$T_VPC_IP" --arg v "$TENT_NODE_VERSION" '(.instance // {}) as $i |
    "id \($i.id // "none") (\(if $i.id == $id then "the API id" else "NOT the API id" end)), zone \($i.zone // "none")" +
    " (\(if $i.zone == $zone then "the region" else "NOT the region" end)), private IP \($i.privateIP // "none")" +
    " (\(if $i.privateIP == $ip then "the VPC IP" else "NOT the VPC IP \($ip)" end)); version \(.version // "none")" +
    " (\(if .version == $v then "the tent-node under test" else "NOT \($v)" end))"' "$1" 2>/dev/null || echo "unreadable"
}

tn_metadata_ms() { # tn_metadata_ms BOOT: ms from preflight's "read the metadata service" to its result in boot BOOT
  local f="$WORK/journal$1.json"
  tn_ssh "journalctl -b $1 -u tent-node.service -o json --no-pager" >"$f" ||
    { printf 'unreadable: %s' "$(ssh_state "$T_PUB")"; return 0; }
  jq -rs '
    def at(re): [.[] | select((.MESSAGE | type) == "string" and (.MESSAGE | test(re)))][0].__REALTIME_TIMESTAMP;
    (at("msg=\"read the metadata service\"")) as $a | (at("msg=phase phase=preflight ")) as $b
    | if $a == null or $b == null then "not in the journal"
      else "\((($b | tonumber) - ($a | tonumber)) / 1000 | floor) ms" end' "$f" 2>/dev/null || echo "unreadable"
}

tn_cloud_init() { # tn_cloud_init WHEN SECONDS: waits at most SECONDS for cloud-init on T and records its status
  local out ci code errs res
  out=$(tn_out "timeout $2 cloud-init status --wait --long; echo \"exit \$?\"" | hide_url)
  printf '%s\n' "$out" | detail "cloud-init status --wait --long (T, $1)"
  ci=$(printf '%s\n' "$out" | sed -n 's/^status: //p' | head -1)
  code=$(printf '%s\n' "$out" | sed -n 's/^exit //p' | tail -1)
  errs=$(printf '%s\n' "$out" | sed -n 's/^errors: //p' | head -1)
  if [ -n "$ci" ]; then res="status $ci, exit ${code:-?}, errors ${errs:-?}"; else res=$(printf '%s' "$out" | oneline 300); fi
  row "cloud-init ($1)" "$res" "decision 19: install waits for tent-node.service, which has no ordering on cloud-final"
}

tn_chain() { # tn_chain UNIT WHEN: records the critical chain of UNIT on T (the default target for "") and prints it
  local chain
  chain=$(tn_out "systemd-analyze critical-chain $1")
  printf '%s\n' "$chain" | detail "systemd-analyze critical-chain ${1:-(the default target)} (T, $2)"
  printf '%s\n' "$chain"
}

tn_chain_row() { # tn_chain_row WHEN: the row of tent-node.service's critical chain; UNEXPECTED when it passes cloud-init
  local chain line
  chain=$(tn_chain tent-node.service "$1")
  line=$(printf '%s\n' "$chain" | grep -m1 'tent-node.service' | tr -s ' ' || true)
  [ -n "$line" ] || line=$(printf '%s' "$chain" | oneline 200)
  if printf '%s\n' "$chain" | grep -Eq 'cloud-(final|config)|cloud-init\.target'; then
    line="UNEXPECTED: the chain passes cloud-final, cloud-config or cloud-init.target: $line"
  fi
  row "critical chain of tent-node.service ($1)" "$line" \
    "decision 19: no ordering on cloud-final, cloud-config or cloud-init.target"
}

# tn_boot_order: does boot wait for up? A critical chain follows the slowest dependency alone, so this reads the
# ordering itself: multi-user.target orders after tent-node.service, as a target does after the units it wants, and
# tent-node.service became active before multi-user.target, which did before cloud-final.service started.
# shellcheck disable=SC2016 # the single-quoted command expands on instance T
tn_boot_order() {
  local out after t m f verdict
  out=$(tn_ssh 'a=$(systemctl show -p After --value multi-user.target)
    case " $a " in *" tent-node.service "*) a=yes ;; *) a=no ;; esac
    printf "%s:%s:%s:%s\n" "$a" "$(systemctl show -p ActiveEnterTimestampMonotonic --value tent-node.service)" \
      "$(systemctl show -p ActiveEnterTimestampMonotonic --value multi-user.target)" \
      "$(systemctl show -p InactiveExitTimestampMonotonic --value cloud-final.service)"' || true)
  [ -n "$out" ] || { ssh_unknown "$T_PUB"; return 0; }
  IFS=: read -r after t m f <<<"$out"
  verdict="UNEXPECTED"
  for v in "$t" "$m" "$f"; do case "$v" in "" | 0 | *[!0-9]*) verdict="unknown" ;; esac; done
  if [ "$verdict" != unknown ] && [ "$after" = yes ] && [ "$t" -le "$m" ] && [ "$m" -le "$f" ]; then
    verdict="as expected"
  fi
  printf '%s: multi-user.target After lists tent-node.service: %s; monotonic, tent-node.service active at %s, multi-user.target active at %s, cloud-final.service started at %s' \
    "$verdict" "$after" "$(tn_seconds "$t")" "$(tn_seconds "$m")" "$(tn_seconds "$f")"
}

tn_seconds() { # tn_seconds MICROSECONDS: as seconds with one decimal, or "?"
  case "$1" in "" | *[!0-9]*) echo "?" ;; *) awk -v u="$1" 'BEGIN { printf "%.1fs", u / 1000000 }' ;; esac
}

tn_reboot() { # reboots T from inside and waits for SSH on the new boot; sets TN_REBOOT_S, returns 1 on a failure
  local before boot t0 deadline
  before=$(tn_ssh 'cat /proc/sys/kernel/random/boot_id' || true)
  [ -n "$before" ] || return 1
  tn_ssh 'systemctl reboot' >/dev/null || true
  ssh_close "$T_PUB"
  t0=$(now)
  deadline=$((t0 + READY_TIMEOUT))
  while [ "$(now)" -lt "$deadline" ]; do
    sleep "$POLL_INTERVAL"
    port_open "$T_PUB" || continue
    ssh_reset "$T_PUB"
    boot=$(tn_ssh 'cat /proc/sys/kernel/random/boot_id' || true)
    if [ -n "$boot" ] && [ "$boot" != "$before" ]; then
      TN_REBOOT_S=$(($(now) - t0))
      return 0
    fi
    ssh_close "$T_PUB" # the old boot still answered
  done
  return 1
}

# shellcheck disable=SC2016 # the single-quoted commands expand on instance T
tn_first_boot() { # checks T after the first boot: cloud-init wrote node.json, downloaded tent-node and ran install
  local out sha res s1="$WORK/status-1.json" units="tent-node.service active/enabled; tent-node-join.timer active/enabled; "
  out=$(tn_ssh 'f=/etc/tent/node.json; if [ -f "$f" ]; then sha256sum "$f" | cut -d" " -f1; stat -c "%a %U:%G" "$f"; else echo missing; fi' || true)
  sha=${out%%$'\n'*}
  case "$out" in
    "") res=$(ssh_unknown "$T_PUB") ;;
    missing) res="MISSING: /etc/tent/node.json" ;;
    *) if [ "$sha" = "$TN_JSON_SHA" ]; then res="intact"; else res="MISMATCH: sha256 $sha, want $TN_JSON_SHA"; fi
      res="$res; mode ${out#*$'\n'}" ;;
  esac
  row "node.json from the gz+b64 user data" "$res" "UserData writes node.json with cloud-init's gz+b64 (platform notes §3.4)"
  tn_cloud_init "first boot" 900
  out=$(tn_out '. /etc/os-release; echo "os: $PRETTY_NAME"; timedatectl show -p CanNTP -p NTP -p NTPSynchronized
    echo "ntp-units.d: $(ls /usr/lib/systemd/ntp-units.d/ 2>&1 | paste -sd " " -)"
    for u in systemd-timesyncd chrony; do echo "$u: $(systemctl is-active "$u")"; done')
  printf '%s\n' "$out" | detail "OS and time sync (T, first boot)"
  row "OS and time sync (first boot)" "$(printf '%s' "$out" | oneline 400)" "the system phase runs timedatectl set-ntp true"
  out=$(tn_out 'for u in tent-node.service tent-node-join.timer; do printf "%s %s/%s; " "$u" "$(systemctl is-active "$u")" "$(systemctl is-enabled "$u")"; done')
  if [ "$out" = "$units" ]; then res="as expected: $out"; else res="UNEXPECTED: $out"; fi
  row "tent-node units (active/enabled)" "$res" "want tent-node.service and tent-node-join.timer active and enabled"
  tn_status_row "$s1" "status.json after the first boot" \
    "preflight unchanged; system, hostfirewall, runtime, cni, join and nomad done; verify unchanged, with Nomad healthy" \
    unchanged "done" "done" "done" "done" "done" "done" unchanged
  out=$(tn_out "stat -c '%a %U:%G %n' /var/lib/tent /var/lib/tent/status.json | paste -sd ';' -")
  row "status.json: instance and version" "$(tn_instance "$s1"); modes $out" \
    "the metadata environment on a real Vultr instance (nodeup/env/vultr)"
  { jq . "$s1" 2>/dev/null || cat "$s1"; } | hide_url | detail "/var/lib/tent/status.json (T, first boot)"
  tn_chain_row "first boot"
  out=$(tn_out 'v=$(systemctl --version | head -1); e=$(mktemp); o=$(systemctl is-enabled no-such.service 2>"$e"); rc=$?
    printf "%s: exit %s; stdout [%s]; stderr [%s]\n" "$v" "$rc" "$o" "$(cat "$e")"; rm -f "$e"')
  printf '%s\n' "$out" | detail "systemctl is-enabled no-such.service: exit code, stdout and stderr (T)"
  row "systemctl is-enabled no-such.service" "$(printf '%s' "$out" | oneline 400)" \
    "Systemd.IsEnabled fails on an empty stdout; verify and install depend on it"
  tn_ssh 'tail -n 60 /var/log/cloud-init-output.log' | hide_url | detail "/var/log/cloud-init-output.log tail (T, first boot)" || true
}

tn_first_files() { # records the modification time and sha256 of tent-node's files before the reboot, and that all exist
  local res missing
  tn_files >"$WORK/files-1.txt" || true
  missing=$(tn_missing "$WORK/files-1.txt")
  if [ ! -s "$WORK/files-1.txt" ]; then
    res=$(ssh_unknown "$T_PUB")
  elif [ -n "$missing" ]; then
    res="MISSING: $missing"
  else
    res="all $(wc -l <"$WORK/files-1.txt" | tr -d ' ') present"
  fi
  row "tent-node's files after the first boot" "$res" \
    "install writes the units; system, hostfirewall, runtime, cni, join and nomad their files; up status.json; refresh-join peers.json"
}

tn_mono() { # tn_mono MICROSECONDS: a monotonic timestamp as seconds, or "-" when the event did not happen this boot
  case "$1" in "" | 0) echo "-" ;; *) tn_seconds "$1" ;; esac
}

# tn_image: what Vultr's image brings and what tent-node found: the packages, the units, the iptables alternative, the
# apt sources, and whether apt's timers ran while up did.
# shellcheck disable=SC2016 # the single-quoted commands expand on instance T
tn_image() {
  local out res u s a e
  tn_out 'dpkg -l nftables iptables firewalld docker.io 2>&1; echo; apt-cache policy docker.io 2>&1' |
    detail "dpkg -l nftables iptables firewalld docker.io; apt-cache policy docker.io (T, first boot)"
  out=$(tn_out 'dpkg-query -W -f "\${Package} \${Version} \${db:Status-Abbrev}\n" nftables iptables firewalld docker.io 2>&1')
  row "Packages (first boot)" "$(printf '%s' "$out" | oneline 400)" \
    "nft must be there; docker.io comes from the updates pocket once apt-get update ran"
  out=$(tn_out 'for u in nftables ufw docker; do printf "%s %s; " "$u" "$(systemctl is-enabled "$u" 2>&1)"; done')
  row "Units enabled: nftables, ufw, docker (first boot)" "$(printf '%s' "$out" | oneline 300)" \
    "nftables.service stays off (its config flushes every table); ufw disabled; docker enabled"
  out=$(tn_out 'update-alternatives --query iptables 2>&1')
  printf '%s\n' "$out" | detail "update-alternatives --query iptables (T)"
  res=$(printf '%s\n' "$out" | sed -n 's/^Value: //p' | head -1)
  row "iptables alternative" "${res:-$(printf '%s' "$out" | oneline 200)}" "Docker's iptables rules land in nftables with iptables-nft"
  tn_out 'for f in /etc/apt/sources.list.d/*.sources /etc/apt/sources.list.d/*.list /etc/apt/sources.list; do [ -f "$f" ] && { echo "== $f"; cat "$f"; }; done' |
    detail "apt sources (T)"
  if ! out=$(tn_ssh 'grep -hE "^(Components:|deb )" /etc/apt/sources.list.d/*.sources /etc/apt/sources.list.d/*.list \
    /etc/apt/sources.list 2>/dev/null | sort -u; true'); then
    res=$(ssh_unknown "$T_PUB")
  else
    case "$out" in *universe*) res="universe listed" ;; *) res="NO universe" ;; esac
    res="$res: $(printf '%s' "${out:-no Components or deb line}" | oneline 300 || true)"
  fi
  row "apt sources: components" "$res" "docker.io is in universe"
  tn_out 'ls -la --time-style=full-iso /var/lib/apt/lists 2>&1 | head -20; du -sh /var/lib/apt/lists 2>&1' |
    detail "ls -la /var/lib/apt/lists; du -sh /var/lib/apt/lists (T, first boot)"
  out=$(tn_out 'd=/var/lib/apt/lists; n=$(find "$d" -maxdepth 1 -type f ! -name lock 2>/dev/null | wc -l)
    m=$(TZ=UTC0 find "$d" -maxdepth 1 -type f ! -name lock -printf "%T+ %f\n" 2>/dev/null | sort | tail -1)
    echo "$n files; newest ${m:-none} (UTC)"')
  row "apt lists" "$(printf '%s' "$out" | oneline 300)" \
    "a record: runtime's apt-get update ran before this, so older files are the image's and the newest may be the update's"
  tn_out 'journalctl -b -u apt-daily.service -u apt-daily-upgrade.service -u unattended-upgrades.service --no-pager -o short-monotonic 2>&1 | tail -n 40' |
    detail "journalctl -u apt-daily -u apt-daily-upgrade -u unattended-upgrades (T, first boot)"
  out=$(tn_ssh 'for u in apt-daily.service apt-daily-upgrade.service unattended-upgrades.service tent-node.service; do
      echo "$u $(systemctl show -p InactiveExitTimestampMonotonic --value "$u") $(systemctl show -p ActiveEnterTimestampMonotonic --value "$u") $(systemctl show -p InactiveEnterTimestampMonotonic --value "$u")"
    done' || true)
  res=""
  while read -r u s a e; do
    if [ -n "$u" ]; then res="$res$u started $(tn_mono "$s"), active $(tn_mono "$a"), stopped $(tn_mono "$e"); "; fi
  done <<<"$out"
  row "apt's timers and up (first boot, monotonic)" "${res:-$(ssh_unknown "$T_PUB")}" \
    "runtime retries each apt command for up to 10 min while apt-daily or unattended-upgrades holds dpkg's locks"
}

# tn_upgrade_records: what can restart Docker or containerd on a node without tent, as records: needrestart and its
# mode, which restarts services after upgrades; the switches of unattended upgrades; the docker.io package's question
# whether its upgrades restart Docker; and how docker.service depends on containerd, which says whether a restart of
# containerd restarts Docker.
# shellcheck disable=SC2016 # the single-quoted commands expand on instance T
tn_upgrade_records() {
  local out
  tn_out 'dpkg -l needrestart 2>&1; echo; grep -r "^\$nrconf{restart}" /etc/needrestart/ 2>&1' |
    detail "dpkg -l needrestart; its restart mode (T, first boot)"
  out=$(tn_out 'dpkg-query -W -f "\${Package} \${Version} \${db:Status-Abbrev}\n" needrestart 2>&1
    if [ -d /etc/needrestart ]; then
      grep -rh "^\$nrconf{restart}" /etc/needrestart/ 2>/dev/null || echo "no \$nrconf{restart} line: the package default"
    fi')
  row "needrestart (first boot)" "$(printf '%s' "$out" | oneline 300)" \
    "a record: needrestart restarts services after upgrades by its mode; containerd and Docker among them"
  out=$(tn_out 'cat /etc/apt/apt.conf.d/20auto-upgrades 2>&1')
  printf '%s\n' "$out" | detail "/etc/apt/apt.conf.d/20auto-upgrades (T, first boot)"
  row "Unattended upgrades (first boot)" "$(printf '%s' "$out" | oneline 300)" \
    "a record: whether apt upgrades packages, containerd's among them, by itself every day"
  out=$(tn_out 'debconf-show docker.io 2>&1')
  printf '%s\n' "$out" | detail "debconf-show docker.io (T, first boot)"
  row "docker.io's debconf answers (first boot)" "$(printf '%s' "$out" | oneline 300)" \
    "a record: docker.io/restart says whether the package restarts Docker at its upgrades"
  out=$(tn_out 'systemctl show -p Restart,Requires,BindsTo,PartOf,Wants,After docker.service 2>&1')
  printf '%s\n' "$out" | detail "systemctl show -p Restart,Requires,BindsTo,PartOf,Wants,After docker.service (T, first boot)"
  row "docker.service and containerd (first boot)" "$(printf '%s' "$out" | oneline 400)" \
    "a record: Requires=, BindsTo= or PartOf= on containerd.service would restart Docker with it; Wants= and After= do not"
}

tn_tables() { # tn_tables FILE: reads nft -j list tables on T into FILE and prints its tables as "family name (comment)"
  tn_ssh 'nft -j list tables' >"$1" 2>/dev/null || true
  jq -r '[.nftables[]?.table? // empty | "\(.family) \(.name)" + (if .comment then " (\(.comment))" else "" end)]
    | join(", ")' "$1" 2>/dev/null || true
}

tn_comment() { # tn_comment FILE: the comment of tent's table in FILE, as tn_tables read it; empty when there is none
  jq -r '[.nftables[]?.table? // empty | select(.family == "inet" and .name == "tent") | .comment // ""][0] // ""' \
    "$1" 2>/dev/null || true
}

tn_drops() { # tn_drops CHAIN: the packets that the metadata drop of CHAIN in tent's table on T counted, or "?"
  local n
  n=$(tn_ssh "nft -j list chain inet tent $1" | jq -r '[.nftables[]?.rule? // empty
    | select(any(.expr[]?; type == "object" and has("drop"))) | .expr[] | select(type == "object" and has("counter"))
    | .counter.packets][0] // empty' 2>/dev/null || true)
  printf '%s' "${n:-?}"
}

# tn_machine: what hostfirewall, runtime and cni left on T: tent's table among the others, ufw off, Docker with tent's
# daemon.json and its FORWARD policy, and the CNI plugins.
# shellcheck disable=SC2016 # the single-quoted commands expand on instance T
tn_machine() {
  local out res f="$WORK/tables-1.json" info="$WORK/docker-info.json" state missing
  tn_out 'nft list ruleset 2>&1' | detail "nft list ruleset (T, first boot)"
  res=$(tn_tables "$f")
  { jq . "$f" 2>/dev/null || cat "$f"; } | detail "nft -j list tables (T, first boot)"
  TN_COMMENT=$(tn_comment "$f")
  if printf '%s' "$TN_COMMENT" | grep -Eq '^tent-node [0-9a-f]{64}$' &&
    jq -e '[.nftables[]?.table? // empty | "\(.family) \(.name)"] | index("ip filter") != null and index("ip nat") != null' \
      "$f" >/dev/null 2>&1; then
    res="as expected: $res"
  else
    res="UNEXPECTED: ${res:-$(ssh_unknown "$T_PUB")}"
  fi
  row "nftables tables (first boot)" "$res" "tent's table with the ruleset's sha256 in its comment, next to Docker's ip filter and ip nat"
  tn_out 'cat /etc/ufw/ufw.conf 2>&1' | detail "/etc/ufw/ufw.conf (T, first boot)"
  if ! out=$(tn_ssh 'if [ -z "$(systemctl list-unit-files --no-legend ufw.service 2>/dev/null)" ]; then echo "no ufw unit"
    else printf "%s; unit %s/%s\n" "$(sed -n "/^ENABLED=/p" /etc/ufw/ufw.conf 2>&1 | tail -1)" \
      "$(systemctl is-enabled ufw 2>&1)" "$(systemctl is-active ufw 2>&1)"; fi'); then
    res=$(ssh_unknown "$T_PUB")
  else
    case "$out" in
      "no ufw unit") res="as expected: no ufw unit, so nothing to turn off" ;;
      "ENABLED=no; unit disabled/"*) res="as expected: $out" ;;
      *) res="UNEXPECTED: $out" ;;
    esac
  fi
  row "ufw (first boot)" "$res" "hostfirewall runs ufw disable while ufw.conf says ENABLED=yes, then disables the unit"
  tn_ssh 'docker info --format "{{json .}}"' >"$info" 2>/dev/null || true
  tn_out 'docker info 2>&1' | detail "docker info (T, first boot)"
  state=$(tn_ssh 'systemctl is-active docker' || true)
  res=$(jq -r '"server \(.ServerVersion // "?"), storage driver \(.Driver // "?"), cgroup driver \(.CgroupDriver // "?")"
    + " (cgroup v\(.CgroupVersion // "?")), live-restore \(.LiveRestoreEnabled | tostring), logging driver"
    + " \(.LoggingDriver // "?"), firewall backend \(.FirewallBackend.Driver // "not reported")"' "$info" 2>/dev/null || true)
  if [ -z "$res" ]; then
    res="UNKNOWN: docker info printed no JSON; docker.service ${state:-$(ssh_unknown "$T_PUB")}"
  elif [ "$state" = active ] && jq -e '.LiveRestoreEnabled == true and .LoggingDriver == "json-file"' "$info" >/dev/null 2>&1; then
    res="as expected: docker.service active; $res"
  else
    res="UNEXPECTED: docker.service ${state:-?}; $res"
  fi
  row "Docker (first boot)" "$res" "runtime: docker.io with daemon.json (live-restore, json-file logs); firewall backend iptables"
  out=$(tn_out 'cat /etc/docker/daemon.json 2>&1')
  printf '%s\n' "$out" | detail "/etc/docker/daemon.json (T)"
  row "/etc/docker/daemon.json" "$(printf '%s\n' "$out" | jq -c . 2>/dev/null || printf '%s' "$out" | oneline 300)" \
    "written before the install, so the first start reads it"
  out=$(tn_out 'iptables -S FORWARD 2>&1 | head -1; ip6tables -S FORWARD 2>&1 | head -1')
  row "iptables and ip6tables FORWARD policy" "$(printf '%s' "$out" | oneline 200)" \
    "Docker sets DROP when it turns ip_forward on; ufw disable would set ACCEPT"
  tn_out 'ls -l /opt/cni/bin /var/lib/tent/assets 2>&1; stat -c "%a %U:%G %n" /opt/cni /opt/cni/bin /var/lib/tent/assets 2>&1' |
    detail "ls -l /opt/cni/bin /var/lib/tent/assets (T, first boot)"
  out=$(tn_ssh "for p in $TN_PLUGINS; do [ -x \"/opt/cni/bin/\$p\" ] || printf '%s ' \"\$p\"; done; echo; ls /opt/cni/bin | wc -l" || true)
  missing=$(printf '%s\n' "$out" | sed -n 1p | sed 's/ *$//')
  if [ -z "$out" ]; then
    res=$(ssh_unknown "$T_PUB")
  elif [ -n "$missing" ]; then
    res="MISSING: $missing"
  else
    res="as expected: $(printf '%s\n' "$out" | sed -n 2p | tr -d ' ') files, among them $TN_PLUGINS, executable"
  fi
  row "CNI plugins in /opt/cni/bin" "$res" "cni: the plugins of Nomad's bridge network, from the cached cni-plugins asset"
}

# tn_reloads: which units asked PID 1 for a reload during the first boot, and how often, as PID 1 logs them. install
# runs in cloud-final.service, or cloud-init-main.service where cloud-init runs every stage in one process, and its
# systemctl enable asks for a reload by itself; apt's reloads during up come from tent-node.service.
tn_reloads() {
  local out res units need
  if ! out=$(tn_ssh 'journalctl -b _PID=1 -o short-monotonic --no-pager | grep -i reload; true'); then
    row "Reloads of systemd during the first boot" "$(ssh_unknown "$T_PUB")" ""
    return 0
  fi
  printf '%s\n' "${out:-no reload in the PID 1 journal}" | detail "journalctl -b _PID=1 | grep -i reload (T, first boot)"
  # systemd 255 logs "Reloading requested from client …", 259 "Reload requested from client …".
  units=$(printf '%s\n' "$out" | sed -nE 's/.*Reload(ing)? requested from client.*\(unit ([^)]*)\).*/\2/p' | sort | uniq -c |
    awk '{printf "%s%s x%s", (NR > 1 ? ", " : ""), $2, $1}')
  if [ -z "$out" ]; then
    res="none"
  elif [ -n "$units" ]; then
    res="asked by $units"
  else
    res="PID 1 names no unit: $(printf '%s' "$out" | oneline 300 || true)"
  fi
  row "Reloads of systemd during the first boot" "$res" \
    "a record: install's systemctl enable asks for one from cloud-init's unit, apt's from tent-node.service"
  if ! out=$(tn_ssh 'systemctl status tent-node.service --no-pager -l 2>&1 | head -n 30; true'); then
    row "systemctl status tent-node.service: changed on disk" "$(ssh_unknown "$T_PUB")" ""
    return 0
  fi
  out=$(printf '%s\n' "$out" | hide_url)
  printf '%s\n' "$out" | detail "systemctl status tent-node.service (T, first boot)"
  need=$(tn_ssh 'systemctl show -p NeedDaemonReload --value tent-node.service' || true)
  case "$out" in
    *"changed on disk"*) res="UNEXPECTED: systemctl status warns that the unit changed on disk; NeedDaemonReload ${need:-?}" ;;
    *) case "$need" in
        no) res="as expected: no warning; NeedDaemonReload no" ;;
        "") res=$(ssh_unknown "$T_PUB") ;;
        *) res="UNEXPECTED: NeedDaemonReload $need" ;;
      esac ;;
  esac
  row "systemctl status tent-node.service: changed on disk" "$res" \
    "apt's reloads during up would clear the warning"
}

# tn_dropped BEFORE AFTER PASS: the verdict of a probe that timed out, from the counter of the metadata drop it meets in
# tent's table before and after it: PASS when the counter grew, a failed check when it did not, since then something
# other than tent's rule stopped the probe, and unknown when the counter cannot be read or went down.
tn_dropped() {
  case "$1:$2" in
    *[!0-9:]* | :* | *:) echo "unknown: timed out, but the drop counter is unreadable" ;;
    *) if [ "$2" -gt "$1" ]; then echo "$3"
      elif [ "$2" -eq "$1" ]; then echo "FAILED: timed out, but tent's drop did not count it"
      else echo "unknown: timed out, and the drop counter went down"; fi ;;
  esac
}

# tn_host_probe: root curl on T's host must not reach the metadata service. It prints its exit code, the HTTP code and
# the time it took to connect. tent's rule drops, so a pass is a timeout (28) before any connection that the counter of
# tent's output drop saw; an answer (0), a connection that then timed out, or a timeout that the counter did not see
# is a failed check; anything else, such as a refused connection (7), is unknown.
# shellcheck disable=SC2016 # the single-quoted command expands on instance T
tn_host_probe() {
  local title="Metadata from the host (root curl)" out res before after
  before=$(tn_drops output)
  out=$(tn_ssh 'o=$(curl -sS -m 3 -o /dev/null -w "HTTP %{http_code}, connect %{time_connect}\n" '"$TN_METADATA_URL"' 2>&1)
    echo "exit $? $o"' || true)
  after=$(tn_drops output)
  { printf '%s\n' "${out:-$(ssh_unknown "$T_PUB")}"
    printf "drops in tent's output chain: %s packets before the probe, %s after\n" "$before" "$after"; } |
    detail "$title (T)"
  out=$(printf '%s' "$out" | oneline 200 || true)
  case "$out" in
    "") res=$(ssh_unknown "$T_PUB") ;;
    "exit 0 "*) res="FAILED: reached the metadata service" ;;
    "exit 28 "*"connect 0.000000"*) res=$(tn_dropped "$before" "$after" blocked) ;;
    "exit 28 "*) res="FAILED: connected to the metadata service, then timed out" ;;
    *) res="unknown" ;;
  esac
  row "$title" "$res; drops in tent's output chain $before, then $after${out:+: $out}" \
    "decision 21: the output chain drops what lacks tent-node's mark"
}

# tn_container_probe TITLE PULLED CHAIN [DOCKER_RUN_OPTION]: probes the metadata service from a new container of
# TN_IMAGE on T, as tn_probe does. PULLED is the output of the image's pull.
tn_container_probe() {
  case "$2" in
    *"exit 0") ;;
    *) row "$1" "unknown: docker pull $TN_IMAGE failed: $(printf '%s' "${2:-$(ssh_unknown "$T_PUB")}" | oneline 200)" ""
      return 0 ;;
  esac
  tn_probe "$1" "$3" "docker run --rm ${4:-} $TN_IMAGE"
}

# tn_probe TITLE CHAIN DOCKER: probes the metadata service from a container on T, after the control URL. DOCKER is the
# docker command that runs sh in the container, such as docker run or docker exec. CHAIN is the chain of tent's table
# whose metadata drop the probe meets: output on the host's network, forward on a bridge. The container prints wget's
# exit code and message for the metadata service after "metadata-exit". busybox prints "download timed out" for a
# connection that stalls as for one that is dropped, so a pass is that timeout with a reached control and a drop
# counter that grew during the probe. An answer, an HTTP error among them, or a timeout that the counter did not see is
# a failed check; anything else, such as a refused connection, is unknown.
# shellcheck disable=SC2016 # the single-quoted part expands in the container
tn_probe() {
  local script out res before after
  script="wget -q -T 5 -O /dev/null $TN_CONTROL_URL && echo control-ok || echo control-failed
"'o=$(wget -q -T 3 -O /dev/null '"$TN_METADATA_URL"' 2>&1); echo "metadata-exit $? $o"'
  before=$(tn_drops "$2")
  out=$(tn_ssh "timeout 120 $3 sh -c '$script' 2>&1; echo \"exit \$?\"" || true)
  after=$(tn_drops "$2")
  { printf '%s\n' "${out:-$(ssh_unknown "$T_PUB")}"
    printf "drops in tent's %s chain: %s packets before the probe, %s after\n" "$2" "$before" "$after"; } |
    detail "$1 (T)"
  case "$out" in
    "") res=$(ssh_unknown "$T_PUB") ;;
    *"metadata-exit 0 "*) res="FAILED: reached the metadata service" ;;
    *"metadata-exit "*"server returned error"*) res="FAILED: the metadata service answered with an HTTP error" ;;
    *control-ok*"metadata-exit "*"download timed out"*)
      res=$(tn_dropped "$before" "$after" "blocked; the container reached $TN_CONTROL_URL") ;;
    *control-failed*"metadata-exit "*"download timed out"*)
      res="unknown: timed out, but the container reached no network: $TN_CONTROL_URL failed too" ;;
    *) res="unknown" ;;
  esac
  row "$1" "$res; drops in tent's $2 chain $before, then $after: $(printf '%s' "$out" | oneline 200)" \
    "decision 21: tent's chains drop what lacks tent-node's mark"
}

# tn_metadata_block: nothing on T reaches the metadata service but tent-node: root on the host, a root container on the
# host's network, a container on Docker's bridge; then up by hand, whose read carries the mark, and the counters of
# the two drops.
# shellcheck disable=SC2016 # the single-quoted commands expand on instance T
tn_metadata_block() {
  local out res pulled before after fwd s="$WORK/status-up.json"
  tn_host_probe
  pulled=$(tn_ssh "timeout 300 docker pull -q $TN_IMAGE 2>&1; echo \"exit \$?\"" || true)
  tn_container_probe "Metadata from a root container on the host network" "$pulled" output "--network host"
  tn_container_probe "Metadata from a container on Docker's bridge" "$pulled" forward ""
  before=$(tn_drops output)
  fwd=$(tn_drops forward)
  case "$before:$fwd" in
    *\?* | 0:* | *:0) res="UNEXPECTED: output $before packets, forward $fwd packets" ;;
    *) res="as expected: output $before packets, forward $fwd packets" ;;
  esac
  row "Metadata drops after the probes" "$res" "the host and host-network probes count in output, the bridge probe in forward"
  tn_up_by_hand "tent-node up by hand" "first boot" "its metadata read carries the mark, so the output chain lets it through"
  after=$(tn_drops output)
  if [ "$before" != "?" ] && [ "$before" = "$after" ]; then res="as expected"; else res="UNEXPECTED"; fi
  row "Output drops during up by hand" "$res: $before packets before, $after after" "up sends nothing unmarked to the metadata service"
  tn_status_row "$s" "status.json after up by hand" \
    "every phase unchanged: tent's table is loaded with the same comment, and Nomad runs with the same files" \
    unchanged unchanged unchanged unchanged unchanged unchanged unchanged unchanged
  tn_out 'nft list chain inet tent output 2>&1; nft list chain inet tent forward 2>&1' |
    detail "nft list chain inet tent output and forward (T, after the probes and up by hand)"
}

# tn_up_by_hand TITLE WHEN IMPACT: runs tent-node up on T by hand and records its output and a row, which exit 0 passes.
# shellcheck disable=SC2016 # the single-quoted command expands on instance T
tn_up_by_hand() {
  local out res
  out=$(tn_ssh 'o=$(/usr/local/bin/tent-node up 2>&1); rc=$?; printf "%s\n" "$o" | tail -n 40; echo "exit $rc"' | hide_url || true)
  printf '%s\n' "${out:-$(ssh_unknown "$T_PUB")}" | detail "tent-node up by hand (T, $2)"
  case "$(printf '%s\n' "$out" | tail -1)" in
    "exit 0") res="as expected: exit 0" ;;
    "") res=$(ssh_unknown "$T_PUB") ;;
    *) res="FAILED: $(printf '%s\n' "$out" | tail -3 | oneline 300 || true)" ;;
  esac
  row "$1" "$res" "$3"
}

# tn_status_row FILE TITLE IMPACT STATUS...: reads T's status.json into FILE and records its phases in a row, as
# expected when they have the statuses that tn_phases_are takes.
tn_status_row() {
  local f="$1" title="$2" impact="$3" res
  shift 3
  res=$(tn_status "$f" | hide_url)
  if tn_phases_are "$f" "$@"; then
    res="as expected: $res"
  elif [ -s "$f" ] || [ "$res" = missing ]; then
    res="UNEXPECTED: $res"
  fi
  row "$title" "$res" "$impact"
}

# tn_nomad_unit WHEN: nomad.service as up started it: active, never enabled for boot (it has no [Install], so
# systemctl calls it static), Type=notify, ordered after docker.service and network-online.target, with systemd's
# default stop timeout of 90 s, since the client does not drain at shutdown, and not restarted by systemd. Its status
# and the time it became active are a record.
# shellcheck disable=SC2016 # the single-quoted commands expand on instance T
tn_nomad_unit() {
  local out res typ after stop restarts mono real state enabled docker network
  tn_out 'systemctl status nomad.service --no-pager -l 2>&1 | head -n 40' | hide_url |
    detail "systemctl status nomad.service (T, $1)"
  out=$(tn_ssh 'for p in Type After TimeoutStopUSec NRestarts ActiveEnterTimestampMonotonic ActiveEnterTimestamp; do
      printf "%s|" "$(systemctl show -p "$p" --value nomad.service)"
    done
    printf "%s|%s\n" "$(systemctl is-active nomad.service)" "$(systemctl is-enabled nomad.service 2>&1)"' || true)
  if [ -z "$out" ]; then
    row "nomad.service ($1)" "$(ssh_unknown "$T_PUB")" ""
    return 0
  fi
  IFS='|' read -r typ after stop restarts mono real state enabled <<<"$out"
  case " $after " in *" docker.service "*) docker=yes ;; *) docker=no ;; esac
  case " $after " in *" network-online.target "*) network=yes ;; *) network=no ;; esac
  res="UNEXPECTED"
  if [ "$state" = active ] && [ "$enabled" = static ] && [ "$typ" = notify ] && [ "$stop" = "1min 30s" ] &&
    [ "$restarts" = 0 ] && [ "$docker" = yes ] && [ "$network" = yes ]; then
    res="as expected"
  fi
  row "nomad.service ($1)" "$res: $state, is-enabled $enabled, Type=$typ, TimeoutStopUSec ${stop:-?}, NRestarts ${restarts:-?}; After lists docker.service: $docker, network-online.target: $network; active since ${real:-?}, $(tn_mono "$mono") after boot" \
    "up starts it after the host firewall and never enables it; at shutdown it stops before Docker, within systemd's default 90 s"
}

# tn_acl_bootstrap: bootstraps the cluster's ACLs on T with the nomad CLI. Its output, the token, goes only into
# TN_TOKEN_FILE, a root-only file on T, from which TN_NOMAD_SH gives it to the later nomad commands: it never leaves T.
# The row shows the exit code, nomad's error, the file's mode and whether it holds a UUID.
# shellcheck disable=SC2016 # the single-quoted commands expand on instance T
tn_acl_bootstrap() {
  local out res
  out=$(tn_ssh "$TN_NOMAD_SH"'f='"$TN_TOKEN_FILE"'
    (umask 077; nomad acl bootstrap -t "{{.SecretID}}" >"$f.new") 2>&1
    rc=$?
    if [ "$rc" = 0 ]; then mv "$f.new" "$f"; else rm -f "$f.new"; fi
    u="no UUID"
    if grep -Eqx "[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}" "$f" 2>/dev/null; then u="a UUID"; fi
    echo "token file $(stat -c "%a %U:%G" "$f" 2>/dev/null || echo missing), $u; exit $rc"' | oneline 300 || true)
  case "$out" in
    "") res=$(ssh_unknown "$T_PUB") ;;
    *"token file 600 root:root, a UUID; exit 0") res="as expected: exit 0; the token file is 600 root:root and holds a UUID" ;;
    *) res="FAILED: $out" ;;
  esac
  row "ACL bootstrap on T (nomad acl bootstrap)" "$res" "ACLs work on a real node; the token stays in a root-only file on T"
}

# tn_nomad_cluster WHEN: the cluster through the local agent's API with the node's certificate. The leader, which needs
# no token, is T's RPC address; with the token, T is the only server, alive, and the only client: ready, eligible, in
# the pool default and the datacenter of its zone, with tent's meta, its instance id among them.
# shellcheck disable=SC2016 # the single-quoted commands expand on instance T
tn_nomad_cluster() {
  local out res leader want members nodes self
  leader=$(tn_ssh 'curl -sS -m 10 --cacert /etc/nomad.d/tls/ca.pem --cert /etc/nomad.d/tls/agent.pem \
    --key /etc/nomad.d/tls/agent-key.pem https://127.0.0.1:4646/v1/status/leader 2>&1' | oneline 200 || true)
  want="\"$T_VPC_IP:4647\""
  case "$leader" in
    "") res=$(ssh_unknown "$T_PUB") ;;
    "$want") res="as expected: $leader" ;;
    *) res="UNEXPECTED: $leader, want $want" ;;
  esac
  row "Nomad leader ($1)" "$res" "GET /v1/status/leader over mTLS with the node's certificate: T leads its cluster of one"
  tn_ssh "$TN_NOMAD_SH"'nomad server members 2>&1; echo; nomad node status -self 2>&1' | hide_url |
    detail "nomad server members; nomad node status -self (T, $1)" || true
  out=$(tn_ssh "$TN_NOMAD_SH"'printf "members: %s\n" "$(nomad server members -t "{{range .}}{{.Name}} {{.Status}};{{end}}" 2>&1 | tr "\n" " ")"
    printf "nodes: %s\n" "$(nomad node status -t "{{len .}}" 2>&1 | tr "\n" " ")"
    printf "self: %s\n" "$(nomad node status -self -t "{{.Name}} {{.Status}} {{.SchedulingEligibility}} {{.NodePool}} {{.Datacenter}} {{.Meta.tent_instance_id}} {{.Meta.tent_cluster}} {{.Meta.tent_nodegroup}}" 2>&1 | tr "\n" " ")"' || true)
  # nomad ends its output with a line end, which the script on T turned into a space.
  members=$(printf '%s\n' "$out" | sed -n 's/^members: //p' | oneline)
  nodes=$(printf '%s\n' "$out" | sed -n 's/^nodes: //p' | oneline)
  self=$(printf '%s\n' "$out" | sed -n 's/^self: //p' | oneline)
  if [ -z "$out" ]; then
    res=$(ssh_unknown "$T_PUB")
  elif [ "$members" = "$RUN_TAG-t.global alive;" ]; then
    res="as expected: $members"
  else
    res="UNEXPECTED: ${members:-no answer}"
  fi
  row "Nomad servers ($1)" "$res" "nomad server members: T alone, alive"
  want="$RUN_TAG-t ready eligible default $REGION $T_ID tent-node-check nodes"
  if [ -z "$out" ]; then
    res=$(ssh_unknown "$T_PUB")
  elif [ "$nodes" = 1 ] && [ "$self" = "$want" ]; then
    res="as expected: 1 node: $self"
  else
    res="UNEXPECTED: ${nodes:-?} node(s); this one: ${self:-no answer}; want $want"
  fi
  row "Nomad client ($1)" "$res" \
    "name, status, eligibility, pool, datacenter, and the meta tent_instance_id (11-instance.hcl), tent_cluster and tent_nodegroup"
}

# tn_nomad: Nomad on T after the first boot: the unit, the ACL bootstrap and the cluster, and the lines of Nomad's
# journal that name client introduction, as a record.
tn_nomad() {
  local out res
  tn_nomad_unit "first boot"
  tn_acl_bootstrap
  tn_nomad_cluster "first boot"
  # Only SSH fails the command: no line is an empty answer.
  if out=$(tn_ssh 'journalctl -b -u nomad.service --no-pager -o cat | grep -i "introduction" | head -n 5; true'); then
    res=$(printf '%s' "$out" | oneline 300)
    res=${res:-no line names it}
  else
    res=$(ssh_unknown "$T_PUB")
  fi
  row "Client introduction in Nomad's journal" "$res" \
    "a record: a cluster with a combined group runs enforcement warn, so the client registers without an intro token"
}

# tn_job_spec: the job of the check: busybox's httpd on Nomad's bridge, on port 8080 inside, which a dynamic port of
# the node maps to.
tn_job_spec() {
  cat <<EOF
job "$TN_JOB" {
  datacenters = ["*"]

  group "web" {
    network {
      mode = "bridge"
      port "http" {
        to = 8080
      }
    }

    task "web" {
      driver = "docker"

      config {
        image   = "$TN_IMAGE"
        command = "sh"
        args    = ["-c", "mkdir -p /www && echo $TN_JOB_BODY >/www/index.html && exec httpd -f -p 8080 -h /www"]
      }

      resources {
        cpu    = 50
        memory = 32
      }
    }
  }
}
EOF
}

# tn_alloc_script: the script on T, with the job's name in JOB, that waits up to 3 minutes for an allocation of the
# job that runs, is to keep running, and whose task started in this boot. Right after a reboot the server still shows a
# restored allocation as it was before, running, until the client reports again, so only the task's start time tells.
# It prints "allocs:" and the allocations as "ID CLIENT-STATUS DESIRED-STATUS TASK-STATE STARTED;" each (STARTED in
# seconds since 1970), or nomad's error; "boot:" and when this boot began; and "fresh:" and the allocation it waited for,
# or nothing.
tn_alloc_script() {
  cat <<'EOF'
b=$(awk '/^btime/ {print $2}' /proc/stat)
f=""
for i in $(seq 1 36); do
  a=$(nomad job allocs -t "{{range .}}{{.ID}} {{.ClientStatus}} {{.DesiredStatus}} {{with .TaskStates.web}}{{.State}} {{.StartedAt.Unix}}{{else}}none 0{{end}};{{end}}" "$JOB" 2>&1 | tr "\n" " ")
  f=$(printf "%s" "$a" | tr ";" "\n" |
    awk -v b="$b" '$2 == "running" && $3 == "run" && $4 == "running" && $5 >= b {print $1; exit}')
  if [ -n "$f" ]; then break; fi
  sleep 5
done
echo "allocs: $a"
echo "boot: $b"
echo "fresh: $f"
EOF
}

# tn_wait_alloc: runs tn_alloc_script on T and prints what it prints; nothing when SSH fails.
tn_wait_alloc() {
  { printf '%s' "$TN_NOMAD_SH"; tn_alloc_script; } >"$WORK/wait-alloc.sh"
  tn_ssh "JOB=$TN_JOB sh -s" <"$WORK/wait-alloc.sh" || true
}

# tn_running ALLOCS: the allocation that tn_wait_alloc waited for, or nothing.
tn_running() { printf '%s\n' "$1" | sed -n 's/^fresh: //p'; }

# tn_allocs ALLOCS: the allocations that tn_wait_alloc printed, on one line without the last ";", and when this boot
# began.
tn_allocs() {
  local s
  s=$(printf '%s\n' "$1" | sed -n 's/^allocs: //p' | oneline 300)
  printf '%s (ID, client status, desired status, task state, task start); this boot began at %s' "${s%;}" \
    "$(printf '%s\n' "$1" | sed -n 's/^boot: //p')"
}

# tn_no_alloc VERDICT ALLOCS: the result of a row when no allocation of the job runs, from what tn_wait_alloc printed:
# unknown when SSH failed, else VERDICT with the job's allocations.
tn_no_alloc() {
  if [ -z "$2" ]; then
    ssh_unknown "$T_PUB"
  else
    printf '%s: no allocation of the job started in this boot and runs within 3 minutes: %s' "$1" "$(tn_allocs "$2")"
  fi
}

# tn_port_script: the script on T, with the address in IP, the port in PORT and the job's text in BODY, that asks the
# port up to 7 times, 5 s apart, until it answers BODY, and prints the last answer, curl's exit code and the try.
tn_port_script() {
  cat <<'EOF'
i=1
while :; do
  o=$(curl -sS -m 5 "http://$IP:$PORT/" 2>&1)
  rc=$?
  if { [ "$rc" = 0 ] && [ "$o" = "$BODY" ]; } || [ "$i" -ge 7 ]; then break; fi
  i=$((i + 1))
  sleep 5
done
printf '%s exit %s on try %s\n' "$o" "$rc" "$i"
EOF
}

# tn_job_port ALLOC WHEN: the job's port answers from T's host at T's private address. Nomad gives the port from the
# dynamic ports on the address of the private interface; a pass is TN_JOB_BODY from there, which only the job serves,
# within 30 s, as a task that has just started may need.
# shellcheck disable=SC2016 # the single-quoted command expands on instance T
tn_job_port() {
  local out res label port to ip
  out=$(tn_ssh "$TN_NOMAD_SH"'nomad alloc status -t "{{range .AllocatedResources.Shared.Ports}}{{.Label}} {{.Value}} {{.To}} {{.HostIP}};{{end}}" '"$1"' 2>&1' |
    oneline 200 || true)
  read -r label port to ip <<<"$(printf '%s' "$out" | tr ';' '\n' | awk '$1 == "http"' | head -n 1)"
  case "$port" in
    "" | *[!0-9]*)
      row "The job's port from the host ($2)" "unknown: no http port: ${out:-$(ssh_unknown "$T_PUB")}" ""
      return 0 ;;
  esac
  tn_port_script >"$WORK/port.sh"
  out=$(tn_ssh "IP=$ip PORT=$port BODY=$TN_JOB_BODY sh -s" <"$WORK/port.sh" | oneline 200 || true)
  res="FAILED"
  case "$out" in
    "$TN_JOB_BODY exit 0 on try "*)
      if [ "$ip" = "$T_VPC_IP" ] && [ "$to" = 8080 ] && [ "$port" -ge 20000 ] && [ "$port" -le 32000 ]; then
        res="as expected"
      fi ;;
  esac
  row "The job's port from the host ($2)" "$res: $label $ip:$port to $to answered: ${out:-$(ssh_unknown "$T_PUB")}" \
    "bridge networking: the CNI plugins, portmap and a dynamic port on the private address"
}

# tn_job: runs the job on T: an allocation of it runs, its port answers from the host, and its container on Nomad's
# bridge does not reach the metadata service, as tn_probe judges it from the drop in tent's forward chain. Sets
# TN_ALLOC.
# shellcheck disable=SC2016 # the single-quoted commands expand on instance T
tn_job() {
  local out allocs
  tn_job_spec >"$WORK/job.nomad.hcl"
  out=$(tn_ssh "cat >/root/$TN_JOB.nomad.hcl && $TN_NOMAD_SH"'nomad job run -detach /root/'"$TN_JOB"'.nomad.hcl 2>&1
    echo "exit $?"' <"$WORK/job.nomad.hcl" || true)
  allocs=$(tn_wait_alloc)
  TN_ALLOC=$(tn_running "$allocs")
  { printf 'nomad job run -detach:\n%s\n\n' "${out:-$(ssh_unknown "$T_PUB")}"
    tn_ssh "$TN_NOMAD_SH"'nomad job status '"$TN_JOB"' 2>&1; echo; docker ps -a 2>&1; echo; docker images 2>&1' || true; } |
    detail "the job $TN_JOB (T, first boot)"
  if [ -z "$TN_ALLOC" ]; then
    row "The job $TN_JOB" "$(tn_no_alloc FAILED "$allocs")" "a docker job on Nomad's bridge with a dynamic port"
    return 0
  fi
  row "The job $TN_JOB" "as expected: allocation $TN_ALLOC runs" "a docker job on Nomad's bridge with a dynamic port"
  tn_out "$TN_NOMAD_SH"'nomad alloc status '"$TN_ALLOC"' 2>&1' | detail "nomad alloc status (T, first boot)"
  tn_job_port "$TN_ALLOC" "first boot"
  tn_probe "Metadata from the job's container on Nomad's bridge" forward "docker exec web-$TN_ALLOC"
}

# tn_job_state: one line from T: docker.service's monotonic active time | the job's container: id, running, start
# time | the allocation's status, its task's restarts and start in seconds since 1970 | nomad.service's monotonic
# active time | its NRestarts | is-active | the node's status and eligibility.
# shellcheck disable=SC2016 # the single-quoted command expands on instance T
tn_job_state() {
  tn_ssh "$TN_NOMAD_SH"'printf "%s|%s|%s|%s|%s|%s|%s\n" \
    "$(systemctl show -p ActiveEnterTimestampMonotonic --value docker.service)" \
    "$(docker inspect -f "{{.Id}} {{.State.Running}} {{.State.StartedAt}}" web-'"$TN_ALLOC"' 2>&1 | head -n 1)" \
    "$(nomad alloc status -t "{{.ClientStatus}} {{with .TaskStates.web}}{{.Restarts}} {{.StartedAt.Unix}}{{else}}none{{end}}" '"$TN_ALLOC"' 2>&1 | head -n 1)" \
    "$(systemctl show -p ActiveEnterTimestampMonotonic --value nomad.service)" \
    "$(systemctl show -p NRestarts --value nomad.service)" "$(systemctl is-active nomad.service)" \
    "$(nomad node status -self -t "{{.Status}} {{.SchedulingEligibility}}" 2>&1 | head -n 1)"' || true
}

# tn_container_runs CONTAINER: does the container, as tn_job_state shows it, run?
tn_container_runs() { case "$1" in *" true "*) return 0 ;; *) return 1 ;; esac; }

# tn_daemon_edit: the script that adds a harmless key to daemon.json on T, Docker's default "debug": false, and
# prints how many lines have it.
tn_daemon_edit() {
  cat <<'EOF'
sed -i '1s/^{$/{\n  "debug": false,/' /etc/docker/daemon.json
grep -c '"debug": false' /etc/docker/daemon.json
EOF
}

# tn_settle_script: the script on T, with the job's allocation in ALLOC, its task's restarts before in RESTARTS, its
# container's id and start before in CID and CSTART, and the unit that was restarted in UNIT, that watches the task for
# up to 3 minutes. It stops once the task runs in a start after the unit became active again with more restarts (the
# task was replaced), or a minute after that with the task running as before (kept), and prints the allocation's client
# status with the task's state, restarts and start in seconds since 1970, the container's id, running and start, when
# the unit became active again in seconds since 1970, and how long after that the watch ended, a "key|value" line each.
tn_settle_script() {
  cat <<'EOF'
b=$(awk '/^btime/ {print $2}' /proc/stat)
since=$((b + $(systemctl show -p ActiveEnterTimestampMonotonic --value "$UNIT") / 1000000))
i=0
while :; do
  now=$(date +%s)
  t=$(nomad alloc status -t "{{.ClientStatus}} {{with .TaskStates.web}}{{.State}} {{.Restarts}} {{.StartedAt.Unix}}{{else}}none none 0{{end}}" "$ALLOC" 2>&1 | head -n 1)
  c=$(docker inspect -f "{{.Id}} {{.State.Running}} {{.State.StartedAt}}" "web-$ALLOC" 2>&1 | head -n 1)
  set -- $t
  if [ "$1 $2" = "running running" ]; then
    if [ "${3:-x}" -gt "$RESTARTS" ] 2>/dev/null && [ "${4:-x}" -ge "$since" ] 2>/dev/null; then break; fi
    if [ $((now - since)) -ge 60 ] && [ "$3" = "$RESTARTS" ] && [ "$c" = "$CID true $CSTART" ]; then break; fi
  fi
  if [ "$i" -ge 36 ]; then break; fi
  i=$((i + 1))
  sleep 5
done
echo "task|$t"
echo "container|$c"
echo "since|$since"
echo "waited|$((now - since))"
EOF
}

# tn_watch UNIT BEFORE: runs tn_settle_script on T for the unit that was restarted, with the task and the container as
# BEFORE, a tn_job_state line, shows them, and prints what it prints; nothing when SSH fails or BEFORE is not such a
# line.
tn_watch() {
  local c a cid running cstart restarts
  IFS='|' read -r _ c a _ <<<"$2"
  read -r cid running cstart <<<"$c"
  read -r _ restarts _ <<<"$a"
  # Only words that the script on T takes as they are.
  case "$TN_ALLOC" in "" | *[!0-9a-f-]*) return 0 ;; esac
  case "$cid" in "" | *[!0-9a-f]*) return 0 ;; esac
  case "$restarts" in "" | *[!0-9]*) return 0 ;; esac
  case "$cstart" in "" | *[!0-9A-Za-z:.+-]*) return 0 ;; esac
  [ "$running" = true ] || return 0
  { printf '%s' "$TN_NOMAD_SH"; tn_settle_script; } >"$WORK/settle.sh"
  tn_ssh "ALLOC=$TN_ALLOC UNIT=$1 RESTARTS=$restarts CID=$cid CSTART=$cstart sh -s" <"$WORK/settle.sh" || true
}

# tn_outcome WATCH ALLOC CONTAINER UNIT: what tn_watch saw happen to the task, from the allocation and the container
# before, as tn_job_state shows them: "kept" (no restart, the same container), "replaced, back running N s after UNIT
# was active again" (more restarts, a later start, a new container that runs), or nothing when the task does not run or
# the two disagree.
tn_outcome() {
  local task container since cs ts r2 s2 r1
  task=$(tn_key task "$1")
  container=$(tn_key container "$1")
  since=$(tn_key since "$1")
  read -r cs ts r2 s2 <<<"$task"
  read -r _ r1 _ <<<"$2"
  [ "$cs $ts" = "running running" ] || return 0
  case "$r1:$r2:$s2:$since" in *[!0-9:]* | :* | *::* | *:) return 0 ;; esac
  if [ "$r2" = "$r1" ] && [ "$container" = "$3" ]; then
    echo "kept"
  elif [ "$r2" -gt "$r1" ] && [ "$s2" -ge "$since" ] && tn_container_runs "$container" && [ "$container" != "$3" ]; then
    echo "replaced, back running $((s2 - since)) s after $4 was active again"
  fi
}

# tn_nomad_lines SINCE WHEN: records the lines of nomad.service's journal since SINCE, seconds since 1970, that name a
# wait, a termination, an EOF or a restart.
tn_nomad_lines() {
  tn_ssh "{ journalctl -u nomad.service --since @${1:-0} --no-pager -o short-monotonic |
    grep -iE 'wait|terminat|eof|restart' || echo 'no line names a wait, a termination, an EOF or a restart'; } |
    tail -n 40" | hide_url | detail "journalctl -u nomad.service since $2: waits, terminations, EOFs, restarts (T)" ||
    true
}

# tn_docker_restart: a changed daemon.json makes up restart Docker. After a harmless key is added, up by hand must
# write tent's daemon.json again and restart docker.service, and leave nomad.service as it was. live-restore keeps the
# container across the restart, but Nomad 2.0.7's docker driver loses its wait on it when dockerd restarts, and stops it
# when the wait breaks (handle.go, a guard against a wait that returned incorrectly); the task then runs in a new
# container after the restart policy's delay. The maintainer accepts that. So a pass is the same allocation running
# again within 3 minutes, its container kept or replaced, which the row says, with how long the task took to run again.
tn_docker_restart() {
  local out res before after watch outcome d1 c1 a1 n1 d2 n2 restarted
  if [ -z "$TN_ALLOC" ]; then
    row "The job across Docker's restart" "unknown: no allocation of the job runs" ""
    return 0
  fi
  tn_daemon_edit >"$WORK/daemon-edit.sh"
  before=$(tn_job_state)
  out=$(tn_ssh 'sh -s' <"$WORK/daemon-edit.sh" || true)
  if [ "$out" != 1 ]; then
    row "The job across Docker's restart" "unknown: the key did not go into daemon.json: ${out:-$(ssh_unknown "$T_PUB")}" ""
    return 0
  fi
  tn_up_by_hand "tent-node up after a change of daemon.json" "daemon.json changed" \
    "runtime writes tent's daemon.json again and restarts Docker"
  tn_status_row "$WORK/status-docker.json" "status.json after up with a changed daemon.json" \
    "runtime done, with Docker restarted; every other phase unchanged, so Nomad is not restarted" \
    unchanged unchanged unchanged "done" unchanged unchanged unchanged unchanged
  watch=$(tn_watch docker.service "$before")
  after=$(tn_job_state)
  if [ -z "$watch" ] || [ -z "$after" ]; then
    row "The job across Docker's restart" "$(ssh_unknown "$T_PUB")" ""
    return 0
  fi
  IFS='|' read -r d1 c1 a1 n1 _ <<<"$before"
  IFS='|' read -r d2 _ _ n2 _ <<<"$after"
  restarted=no
  if [ -n "$d1" ] && [ -n "$d2" ] && [ "$d1" != "$d2" ]; then restarted=yes; fi
  outcome=$(tn_outcome "$watch" "$a1" "$c1" docker.service)
  res="UNEXPECTED"
  if [ "$restarted" = yes ] && [ -n "$n1" ] && [ "$n1" = "$n2" ] && [ -n "$outcome" ]; then res="as expected"; fi
  row "The job across Docker's restart" "$res: ${outcome:-the task does not run again within 3 minutes, or its restarts and container disagree}; docker.service restarted: $restarted; allocation (status, task state, restarts, start) at the end [$(tn_key task "$watch")], task restarts and start before [${a1#running }]; container before [$c1], at the end [$(tn_key container "$watch")]; watched until $(tn_key waited "$watch") s after docker.service was active again; nomad.service active at $(tn_mono "$n1"), then $(tn_mono "$n2")" \
    "Nomad 2.0.7's docker driver stops the container when its wait breaks (handle.go, a guard against a wait that returned incorrectly), so a restart of dockerd may replace the live-restored container; either way the same allocation runs again, and Nomad is not restarted"
  tn_nomad_lines "$(tn_key since "$watch")" "Docker's restart"
  tn_job_port "$TN_ALLOC" "after Docker's restart"
}

# tn_containerd_restart: containerd's package restarts containerd.service at each of its upgrades, which unattended
# upgrades may run on every node at once. A restart of containerd while the job runs must leave the job's container
# (its id and start), the task (no restart), Docker and Nomad as they were, and the port answering; the row says what
# happened otherwise. The lines of nomad.service since the restart that name a wait, a termination, an EOF or a restart
# are a record.
# shellcheck disable=SC2016 # the single-quoted command expands on instance T
tn_containerd_restart() {
  local out before after watch outcome res d1 c1 a1 n1 d2 n2 title="containerd's restart with the job running"
  if [ -z "$TN_ALLOC" ]; then
    row "$title" "unknown: no allocation of the job runs" ""
    return 0
  fi
  before=$(tn_job_state)
  out=$(tn_ssh 'systemctl restart containerd.service 2>&1; echo "exit $?"' | oneline 300 || true)
  watch=$(tn_watch containerd.service "$before")
  after=$(tn_job_state)
  if [ -z "$out" ] || [ -z "$watch" ] || [ -z "$after" ]; then
    row "$title" "$(ssh_unknown "$T_PUB")" ""
    return 0
  fi
  IFS='|' read -r d1 c1 a1 n1 _ <<<"$before"
  IFS='|' read -r d2 _ _ n2 _ <<<"$after"
  outcome=$(tn_outcome "$watch" "$a1" "$c1" containerd.service)
  res="UNEXPECTED"
  if [ "$out" = "exit 0" ] && [ "$outcome" = kept ] && [ -n "$d1" ] && [ "$d1" = "$d2" ] && [ -n "$n1" ] &&
    [ "$n1" = "$n2" ]; then
    res="as expected"
  fi
  row "$title" "$res: systemctl restart containerd.service: $out; the task: ${outcome:-does not run again within 3 minutes, or its restarts and container disagree}; allocation (status, task state, restarts, start) at the end [$(tn_key task "$watch")], task restarts and start before [${a1#running }]; container before [$c1], at the end [$(tn_key container "$watch")]; watched until $(tn_key waited "$watch") s after containerd.service was active again; docker.service active at $(tn_mono "$d1"), then $(tn_mono "$d2"); nomad.service active at $(tn_mono "$n1"), then $(tn_mono "$n2")" \
    "containerd's package restarts containerd.service at every upgrade, unattended, on every node: the container, the task, Docker and Nomad must stay"
  tn_nomad_lines "$(tn_key since "$watch")" "containerd's restart"
  tn_job_port "$TN_ALLOC" "after containerd's restart"
}

# tn_restart_script: the script on T, with the unit in UNIT, that restarts it by hand, waits up to 1 minute for the node
# to report ready, and prints the restart's exit code and output, how long it took in milliseconds, when it began in
# seconds since 1970, and how many lines of the unit's journal since then name a drain, a "key|value" line each.
tn_restart_script() {
  cat <<'EOF'
s=$(date +%s)
t0=$(date +%s%N)
o=$(systemctl restart "$UNIT" 2>&1)
rc=$?
t1=$(date +%s%N)
i=0
while [ "$i" -lt 12 ] && [ "$(nomad node status -self -t "{{.Status}}" 2>/dev/null)" != ready ]; do
  sleep 5
  i=$((i + 1))
done
echo "rc|$rc"
echo "out|$(printf '%s' "$o" | tr '\n' ' ')"
echo "took|$(((t1 - t0) / 1000000))"
echo "since|$s"
echo "drain|$(journalctl -u "$UNIT" --since "@$s" -o cat --no-pager | grep -ci drain)"
EOF
}

# tn_nomad_restart: a restart of Nomad by hand while the job runs leaves the job's task running: KillMode=process keeps
# the task's processes and Docker its container, the client reattaches to them, and with no drain the node stays
# eligible. systemd's NRestarts counts only the restarts that Restart= makes, and a restart by hand sets it to 0, so a
# later ActiveEnterTimestamp proves the restart. After it, the node must report ready, and the client has 15 s to
# restore the allocation, since the server shows it as it was until the client reports. A pass is: the restart exits
# 0; nomad.service is active with a later active time and NRestarts 0; the container keeps its id and start time; the
# allocation runs with the same task restarts and task start; the node is ready and eligible; no line of nomad.service
# since the restart names a drain; and the port answers. The agent's lines that name a drain, restoring or
# reattaching are a record.
tn_nomad_restart() {
  local out before after rc said took since drain res c1 a1 n1 c2 a2 n2 r2 s2 node2
  local title="Nomad's restart with the job running"
  if [ -z "$TN_ALLOC" ]; then
    row "$title" "unknown: no allocation of the job runs" ""
    return 0
  fi
  before=$(tn_job_state)
  { printf '%s' "$TN_NOMAD_SH"; tn_restart_script; } >"$WORK/restart.sh"
  out=$(tn_ssh "UNIT=nomad.service sh -s" <"$WORK/restart.sh" | hide_url || true)
  sleep 15
  after=$(tn_job_state)
  if [ -z "$out" ] || [ -z "$after" ]; then
    row "$title" "$(ssh_unknown "$T_PUB")" ""
    return 0
  fi
  rc=$(tn_key rc "$out")
  said=$(tn_key out "$out")
  said=${said% }
  took=$(tn_key took "$out")
  since=$(tn_key since "$out")
  drain=$(tn_key drain "$out")
  IFS='|' read -r _ c1 a1 n1 _ <<<"$before"
  IFS='|' read -r _ c2 a2 n2 r2 s2 node2 <<<"$after"
  res="UNEXPECTED"
  case "$n1:$n2" in
    *[!0-9:]* | :* | *:) ;;
    *) if [ "$rc" = 0 ] && [ "$n2" -gt "$n1" ] && [ "$r2" = 0 ] && [ "$s2" = active ] && tn_container_runs "$c2" &&
      [ "$c1" = "$c2" ] && [ "${a1#running }" != "$a1" ] && [ "$a2" = "$a1" ] && [ "$node2" = "ready eligible" ] &&
      [ "$drain" = 0 ]; then res="as expected"; fi ;;
  esac
  row "$title" "$res: systemctl restart exit ${rc:-?} after ${took:-?} ms${said:+ ($said)}; nomad.service active at $(tn_mono "$n1"), then $(tn_mono "$n2"), NRestarts ${r2:-?}, ${s2:-?}; container before [$c1], after [$c2]; allocation (status, task restarts, task start) before [$a1], after [$a2]; node ${node2:-?}; lines of nomad.service that name a drain since the restart: ${drain:-?}" \
    "KillMode=process: the task keeps running and the client reattaches to it; no drain, so the node stays eligible"
  tn_ssh "{ journalctl -u nomad.service --since @${since:-0} --no-pager -o short-monotonic |
    grep -iE 'drain|reattach|restor' || echo 'no line names a drain, restoring or reattaching'; } | tail -n 40" |
    hide_url | detail "journalctl -u nomad.service since Nomad's restart: drain, restoring and reattaching (T)" || true
  tn_job_port "$TN_ALLOC" "after Nomad's restart"
}

# tn_join_check: the script on T, with the node's private address in IP, that waits up to 3 minutes for a run of
# refresh-join that rewrote 05-join.hcl, then up to 2 minutes for one more run that starts after it, and prints what
# tn_join_timer judges, a "key|value" line each. A run that has started when the wait ends finishes first.
tn_join_check() {
  cat <<'EOF'
j() { journalctl -b _PID=1 UNIT=tent-node-join.service -o cat --no-pager; }
runs() { j | grep -c "^Finished"; }
rewrites() {
  journalctl -b -u tent-node-join.service -o cat --no-pager | grep -F 'msg="05-join.hcl joins the servers that answered"'
}
state() {
  for f in /etc/nomad.d/05-join.hcl /var/lib/tent/peers.json; do
    printf "%s mtime %s sha256 %s; " "$f" "$(stat -c %Y "$f" 2>/dev/null || echo -)" \
      "$(sha256sum 2>/dev/null <"$f" | cut -d" " -f1)"
  done
}
i=0
while [ "$i" -lt 36 ] && [ -z "$(rewrites)" ]; do sleep 5; i=$((i + 1)); done
i=0
while [ "$i" -lt 12 ] && [ "$(systemctl is-active tent-node-join.service)" = activating ]; do sleep 5; i=$((i + 1)); done
before=$(runs)
first=$(state)
i=0
while [ "$i" -lt 24 ] && [ "$(runs)" -le "$before" ]; do sleep 5; i=$((i + 1)); done
echo "rewrite|$(rewrites | tail -n 1)"
echo "rewrites|$(rewrites | wc -l | tr -d ' ')"
echo "runs|$before|$(runs)"
echo "failed|$(j | grep -c "Failed with result")"
echo "result|$(systemctl show -p Result --value tent-node-join.service)"
echo "first|$first"
echo "later|$(state)"
echo "retry|$(grep -cFx "    retry_join = [\"$IP:4648\"]" /etc/nomad.d/05-join.hcl 2>/dev/null)"
echo "peers|$(cat /var/lib/tent/peers.json 2>/dev/null)"
EOF
}

# tn_join_timer: refresh-join keeps 05-join.hcl current. On a server or combined node it asks the node's own agent
# first, over mTLS with the ServerName server.<region>.nomad: once T's Nomad has a leader, the first run gets T's
# address back, rewrites 05-join.hcl once to join it (the server form, port 4648) and writes peers.json; the runs after
# it change nothing. A pass is that rewrite and its log line, the files as it left them across a later run, and no
# failed run. The timer and the journal of tent-node-join.service are records.
tn_join_timer() {
  local out res rewrite rewrites runs failed result first later retry peers want impact
  tn_out 'systemctl list-timers --all --no-pager tent-node-join.timer 2>&1' |
    detail "systemctl list-timers tent-node-join.timer (T, first boot)"
  tn_join_check >"$WORK/join-check.sh"
  out=$(tn_ssh "IP=$T_VPC_IP sh -s" <"$WORK/join-check.sh" | hide_url || true)
  tn_ssh 'journalctl -b -u tent-node-join.service --no-pager -o short-monotonic | tail -n 60' | hide_url |
    detail "journalctl -u tent-node-join.service (T, first boot)" || true
  tn_out 'cat /etc/nomad.d/05-join.hcl /var/lib/tent/peers.json 2>&1' |
    detail "/etc/nomad.d/05-join.hcl and /var/lib/tent/peers.json after refresh-join (T, first boot)"
  rewrite=$(tn_key rewrite "$out")
  rewrites=$(tn_key rewrites "$out")
  runs=$(tn_key runs "$out")
  failed=$(tn_key failed "$out")
  result=$(tn_key result "$out")
  first=$(tn_key first "$out")
  first=${first%; }
  later=$(tn_key later "$out")
  later=${later%; }
  retry=$(tn_key retry "$out")
  peers=$(tn_key peers "$out")
  want="[\"$T_VPC_IP\"]"
  impact="refresh-join asks T's own agent over mTLS (ServerName server.<region>.nomad), rewrites 05-join.hcl once and leaves it"
  if [ -z "$out" ]; then
    row "tent-node-join.timer and refresh-join" "$(ssh_unknown "$T_PUB")" "$impact"
    return 0
  fi
  if [ -z "$rewrite" ]; then
    res="UNEXPECTED: no run of refresh-join rewrote 05-join.hcl within 3 minutes"
  elif [ "$rewrites" = 1 ] && [ "$failed" = 0 ] && [ "$result" = success ] && [ "$retry" = 1 ] &&
    [ "$peers" = "$want" ] && [ "${runs#*|}" != "${runs%|*}" ] && [ "$first" = "$later" ] &&
    [ "${first#*mtime -}" = "$first" ]; then
    res="as expected"
  else
    res="UNEXPECTED"
  fi
  if [ "$first" = "$later" ]; then later="the same"; fi
  row "tent-node-join.timer and refresh-join" "$res: ${rewrite:-no rewrite}; ${rewrites:-?} rewrite line(s); runs ${runs%|*}, then ${runs#*|}, ${failed:-?} failed, last result ${result:-?}; 05-join.hcl joins $T_VPC_IP:4648: ${retry:-?} line(s); peers.json ${peers:-missing}, want $want; across a later run the files were ${first:-?}, then ${later:-?}" \
    "$impact"
}

# tn_key KEY OUTPUT: the value of the line "KEY|value" in OUTPUT.
tn_key() { printf '%s\n' "$2" | sed -n "s/^$1|//p" | head -n 1; }

# tn_start_order: after the reboot, Nomad started only once the hostfirewall phase had loaded tent's table: the start of
# nomad.service against the log line of the hostfirewall phase in tent-node's journal, both monotonic in this boot.
# shellcheck disable=SC2016 # the single-quoted command expands on instance T
tn_start_order() {
  local f="$WORK/journal-up-2.json" fw start res
  tn_ssh 'journalctl -b 0 -u tent-node.service -o json --no-pager' >"$f" || true
  fw=$(jq -rs '[.[] | select((.MESSAGE | type) == "string" and (.MESSAGE | test("msg=phase phase=hostfirewall ")))][0]
    | .__MONOTONIC_TIMESTAMP // ""' "$f" 2>/dev/null || true)
  start=$(tn_ssh 'systemctl show -p InactiveExitTimestampMonotonic --value nomad.service' || true)
  res="UNEXPECTED"
  case "$fw:$start" in
    *[!0-9:]* | :* | *:) res="unknown" ;;
    *) if [ "$start" != 0 ] && [ "$start" -ge "$fw" ]; then res="as expected"; fi ;;
  esac
  row "Nomad's start after the reboot" "$res: hostfirewall logged its result at $(tn_seconds "$fw"), nomad.service started at $(tn_seconds "$start") (monotonic)" \
    "nomad.service has no [Install]: only up starts it, after the host firewall"
  # join asks the servers in peers.json while Nomad is still stopped, so T's own address refuses it.
  { jq -rs '[.[] | select((.MESSAGE | type) == "string" and (.MESSAGE | test("a server did not answer"))) | .MESSAGE]
      | if length == 0 then "no such line" else .[] end' "$f" 2>/dev/null || echo "unreadable"; } | hide_url |
    detail "join's warnings in tent-node's journal (T, after the reboot)"
}

# tn_job_after_reboot: Nomad runs the job again after the reboot. The tasks died with the machine, without a
# migration; after the boot the client restores the allocation or the scheduler replaces it, so a pass is a running
# allocation, the same or a new one, and its port. The allocations with their client and desired status and the
# node's eligibility are in the row, and the first boot's allocation, its status and events, in the details, whatever
# the verdict.
# shellcheck disable=SC2016 # the single-quoted command expands on instance T
tn_job_after_reboot() {
  local allocs alloc which eligibility
  allocs=$(tn_wait_alloc)
  alloc=$(tn_running "$allocs")
  eligibility=$(tn_ssh "$TN_NOMAD_SH"'nomad node status -self -t "{{.SchedulingEligibility}}" 2>&1' | oneline 200 ||
    true)
  tn_out "$TN_NOMAD_SH"'nomad job status '"$TN_JOB"' 2>&1' | detail "nomad job status $TN_JOB (T, after the reboot)"
  if [ -n "$TN_ALLOC" ]; then
    tn_out "$TN_NOMAD_SH"'nomad alloc status '"$TN_ALLOC"' 2>&1' |
      detail "nomad alloc status of the first boot's allocation $TN_ALLOC (T, after the reboot)"
  fi
  if [ -z "$alloc" ]; then
    row "The job after the reboot" "$(tn_no_alloc UNEXPECTED "$allocs"); node eligibility: ${eligibility:-?}" \
      "Nomad runs the job again"
    return 0
  fi
  if [ "$alloc" = "$TN_ALLOC" ]; then which="the one of the first boot"; else which="a new one; the first boot's was $TN_ALLOC"; fi
  row "The job after the reboot" "as expected: allocation $alloc runs, $which; $(tn_allocs "$allocs"); node eligibility: ${eligibility:-?}" \
    "Nomad runs the job again"
  tn_job_port "$alloc" "after the reboot"
}

# tn_shutdown: how the reboot stopped Nomad with the job running, from the journal of the boot before it.
# nomad.service orders after docker.service, so its stop must begin and end before Docker's stop begins. The client
# does not drain at shutdown, so Nomad stops in seconds, and no line of nomad.service from its stop on names a drain.
# The boot saw Docker's restart, Nomad's restart by hand and maybe a failed Nomad before, so the last line of each kind
# in PID 1's journal is the shutdown's, and the drain lines count from that last stop on. The shutdown lines of
# nomad.service and docker.service are records.
tn_shutdown() {
  local f="$WORK/journal-pid1-1.json" n="$WORK/journal-nomad-1.json" out ns nd ds res drain=""
  # Only SSH fails the command: a journal without the previous boot is empty.
  if ! tn_ssh 'journalctl -b -1 _PID=1 -o json --no-pager 2>/dev/null; true' >"$f"; then
    res=$(ssh_unknown "$T_PUB")
  else
    out=$(jq -rs 'def at(u; re): [.[] | select(.UNIT == u and (.MESSAGE | type) == "string" and (.MESSAGE | test(re)))]
        | last | .__MONOTONIC_TIMESTAMP // "";
      "\(at("nomad.service"; "^Stopping ")):\(at("nomad.service"; "^Stopped |Deactivated successfully|Failed with result")):\(at("docker.service"; "^Stopping "))"' \
      "$f" 2>/dev/null || true)
    IFS=: read -r ns nd ds <<<"$out"
    case "$ns:$nd:$ds" in
      *[!0-9:]* | :* | *::* | *:)
        res="unknown: the previous boot's journal lacks the stop of nomad.service or docker.service: [$out]" ;;
      *)
        if tn_ssh 'journalctl -b -1 -u nomad.service -o json --no-pager 2>/dev/null; true' >"$n"; then
          drain=$(jq -rs --argjson from "$ns" '[.[] | select((.MESSAGE | type) == "string" and (.MESSAGE | test("drain"; "i"))
            and ((.__MONOTONIC_TIMESTAMP | tonumber) >= $from))] | length' "$n" 2>/dev/null || true)
        fi
        if [ "$nd" -lt "$ns" ]; then
          res="UNEXPECTED: the journal has no end of nomad.service's last stop"
        elif [ "$nd" -gt "$ds" ]; then
          res="UNEXPECTED: Docker's stop began before Nomad had stopped"
        else
          case "$drain" in
            0) res="as expected" ;;
            "" | *[!0-9]*) res="unknown: nomad.service's journal of that boot is unreadable" ;;
            *) res="UNEXPECTED: $drain lines of nomad.service name a drain from its stop on, which tent does not configure" ;;
          esac
        fi
        res="$res: nomad.service's last stop began at $(tn_seconds "$ns"), its last end at $(tn_seconds "$nd") ($(tn_seconds $((nd - ns))) later), docker.service's last stop began at $(tn_seconds "$ds") (monotonic); lines that name a drain from Nomad's stop on: ${drain:-?}" ;;
    esac
  fi
  row "Shutdown with the job running (the reboot)" "$res; SSH answered ${TN_REBOOT_S:-?}s after the reboot began" \
    "no drain at shutdown: Nomad stops in seconds, before Docker begins to stop (After=docker.service)"
  tn_ssh 'journalctl -b -1 -u nomad.service -u docker.service --no-pager -o short-monotonic | tail -n 60' | hide_url |
    detail "journalctl -b -1 -u nomad.service -u docker.service: the shutdown (T)" || true
}

# tn_listeners: what listens on T right after SSH came back after the reboot, and whether up had run by then.
# shellcheck disable=SC2016 # the single-quoted command expands on instance T
tn_listeners() {
  local out sockets
  out=$(tn_ssh 'printf "uptime %ss, tent-node.service %s\n" "$(cut -d" " -f1 /proc/uptime)" "$(systemctl is-active tent-node.service)"
    ss -Htulpn' || true)
  printf '%s\n' "${out:-$(ssh_unknown "$T_PUB")}" | detail "ss -tulpn right after SSH came back (T, after the reboot)"
  sockets=$(printf '%s\n' "$out" | sed 1d | awk '{p = $7; sub(/^users:\(\("/, "", p); sub(/".*/, "", p)
    printf "%s%s %s %s", (NR > 1 ? "; " : ""), $1, $5, p}')
  row "Listening right after SSH came back (after the reboot)" \
    "${out:+$(printf '%s\n' "$out" | sed -n 1p); }${sockets:-$(ssh_unknown "$T_PUB")}" \
    "until up loads tent's table after a reboot, only sshd and systemd's own sockets should listen; Nomad once up started it"
}

tn_second_boot() { # checks T after the reboot: tent-node.service ran up again, which changed nothing but status.json
  local out sys res t1 t2 changed missing s1="$WORK/status-1.json" s2="$WORK/status-2.json" f="$WORK/tables-2.json"
  # First, as close to the boot as SSH allows.
  tn_listeners
  sys=$(tn_ssh 'timeout 600 systemctl is-system-running --wait' || true)
  out=$(tn_ssh 'systemctl is-active tent-node.service' || true)
  row "Reboot T" "SSH on the new boot after ${TN_REBOOT_S}s; systemctl is-system-running: ${sys:-?}; tent-node.service ${out:-?}" \
    "tent-node.service runs up at every boot"
  tn_cloud_init "after the reboot" 600
  out=$(tn_out 'timedatectl show -p NTP -p NTPSynchronized')
  row "Time sync after the reboot" "$(printf '%s' "$out" | oneline 200)" ""
  res=$(tn_status "$s2" | hide_url)
  t1=$(jq -r '.started // ""' "$s1" 2>/dev/null || true)
  t2=$(jq -r '.started // ""' "$s2" 2>/dev/null || true)
  if [ -n "$t2" ] && [ "$t2" != "$t1" ] &&
    tn_phases_are "$s2" unchanged unchanged "done" unchanged unchanged unchanged "done" unchanged; then
    res="as expected: $res"
  elif [ -s "$s2" ] || [ "$res" = missing ]; then
    res="UNEXPECTED: $res"
  fi
  row "status.json after the reboot" "$res; started $t1, then $t2" \
    "every phase unchanged but hostfirewall and nomad done: the kernel forgot tent's table, and only up starts Nomad"
  row "status.json after the reboot: instance and version" "$(tn_instance "$s2")" ""
  { jq . "$s2" 2>/dev/null || cat "$s2"; } | hide_url | detail "/var/lib/tent/status.json (T, after the reboot)"
  res=$(tn_tables "$f")
  { jq . "$f" 2>/dev/null || cat "$f"; } | detail "nft -j list tables (T, after the reboot)"
  out=$(tn_comment "$f")
  if [ -n "$out" ] && [ "$out" = "$TN_COMMENT" ]; then
    res="as expected: loaded again with the comment of the first boot; $res"
  else
    res="UNEXPECTED: comment ${out:-none}, first boot ${TN_COMMENT:-none}; ${res:-$(ssh_unknown "$T_PUB")}"
  fi
  row "tent's table after the reboot" "$res" "the kernel forgets tables at a reboot; up loads tent's again, the same ruleset"
  tn_out 'nft list table inet tent 2>&1' | detail "nft list table inet tent (T, after the reboot)"
  row "Metadata drops after the reboot" "output $(tn_drops output) packets, forward $(tn_drops forward) packets" \
    "what asked the metadata service without tent-node's mark since up loaded the table; cloud-init asks before, if at all"
  tn_files >"$WORK/files-2.txt" || true
  changed=$(diff "$WORK/files-1.txt" "$WORK/files-2.txt" | sed -n 's/^> \([^ ]*\) .*/\1/p' | paste -sd ' ' - || true)
  missing=$(tn_missing "$WORK/files-2.txt")
  if [ ! -s "$WORK/files-2.txt" ]; then
    res=$(ssh_unknown "$T_PUB")
  elif [ -n "$missing" ]; then
    res="MISSING: $missing; changed: ${changed:-nothing}"
  elif [ "$changed" = "/var/lib/tent/status.json" ]; then
    res="only /var/lib/tent/status.json changed"
  else
    res="CHANGED: ${changed:-nothing, not even status.json}"
  fi
  row "tent-node's files after the reboot" "$res" "no downloads or writes when the files are as they should be"
  { printf 'before the reboot:\n'; cat "$WORK/files-1.txt"; printf '\nafter the reboot:\n'; cat "$WORK/files-2.txt"; } |
    detail "tent-node's files: modification time and sha256 (T)"
  row "Metadata read by preflight" "after the reboot: $(tn_metadata_ms 0); first boot: $(tn_metadata_ms -1) (a failed try waits 1 s before the next, so under 1000 ms means the first try succeeded)" \
    "tent-node logs no tries: the time between preflight's two log lines tells"
  tn_chain_row "after the reboot"
  # Records only: the details show where boot spent its time.
  tn_chain cloud-final.service "after the reboot" >/dev/null
  tn_chain "" "after the reboot" >/dev/null
  row "Boot order after the reboot" "$(tn_boot_order)" \
    "boot waits for up: multi-user.target orders after the units it wants, cloud-final after multi-user.target"
  tn_ssh 'journalctl --list-boots --no-pager | tail -3; echo; journalctl -u tent-node.service -u tent-node-join.service --no-pager -o short-precise' |
    hide_url | detail "journalctl -u tent-node.service -u tent-node-join.service (T, both boots)" || true
}

check_tentnode() {
  local deadline
  create_instance T "$TN_UD" || die "cannot create instance T: $API_STATUS $(api_err)"
  deadline=$(($(now) + READY_TIMEOUT))
  log "waiting for instance T to boot (timeout ${READY_TIMEOUT}s)"
  until poll_instance T; do
    if [ "$(now)" -gt "$deadline" ]; then
      log "timeout waiting for instance T"
      break
    fi
    sleep 5
  done
  boot_row T "Boot T (tent-node user data)"
  if [ -z "$T_PUB" ] || [ -z "$T_T_SSH" ]; then
    row "tentnode" "$(ssh_unknown "${T_PUB:-no public IP}")" "no check ran"
    return 0
  fi
  tn_first_boot
  tn_image
  tn_upgrade_records
  tn_machine
  tn_reloads
  tn_metadata_block
  tn_nomad
  tn_job
  # Docker's and containerd's restarts check that Nomad was not restarted, so they come before Nomad's restart.
  tn_docker_restart
  tn_containerd_restart
  tn_nomad_restart
  tn_join_timer
  # After every change by hand, so that the reboot's comparison sees only what up did at boot.
  tn_first_files
  # A reboot runs up again at boot: it must change nothing but status.json, and start Nomad.
  if ! tn_reboot; then
    row "Reboot T" "no boot id before the reboot, or no SSH on a new boot within ${READY_TIMEOUT}s: $(ssh_unknown "$T_PUB")" \
      "the second up was not checked"
    return 0
  fi
  tn_second_boot
  tn_nomad_unit "after the reboot"
  tn_start_order
  tn_nomad_cluster "after the reboot"
  tn_job_after_reboot
  tn_shutdown
}

# ---------------------------------------------------------------------------------------------------------------
# Cleanup (EXIT trap): write the report, then delete everything this run created unless --keep.
# Records how long the API refuses to delete the firewall group and the VPC after the instances are gone
# (the retry loop `tent delete cluster` needs).

cleanup() {
  local rc=$? type id deadline left t0 tries first_err ip
  set +e
  write_report
  for ip in "$A_PUB" "$B_PUB" "$V_PUB" "$T_PUB"; do [ -n "$ip" ] && [ -n "$SOCK_DIR" ] && ssh_close "$ip"; done
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
  if want tentnode && [ "$CHECKS" != tentnode ]; then die "tentnode runs alone: --only tentnode"; fi
  # tentnode needs one VPC: it tries the first mask alone and makes no test VPCs.
  if want tentnode; then VPC_MASKS="${VPC_MASKS%% *}"; fi

  for c in curl jq awk base64 tr od; do need_cmd "$c"; done
  if [ "$MODE" = "run" ] && want tentnode; then need_cmd go; need_cmd gzip; fi
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
  local vpcs="1 VPC (plus short-lived test VPCs for mask checks)"
  if want tentnode; then vpcs="1 VPC"; fi
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
  # tentnode waits for Docker's install and Nomad on the first boot, runs a job, restarts Docker, then reboots.
  if want tentnode; then duration="10-30 minutes"; fi
  if [ "$KEEP" = 1 ]; then keep_note=" (NOT deleted: --keep given)"; fi
  cat >&2 <<EOF

This run creates resources in your Vultr account ($REGION):
  - $billed
  - $vpcs, $keys SSH key(s), $groups firewall group(s)$lock_note
  - all tagged/marked with $RUN_TAG and deleted on exit$keep_note
Expected duration: $duration. Report: $REPORT

EOF
  [ "$MODE" = "dry-run" ] && { log "dry run: nothing created"; return 0; }
  # Before anything is created: the user data needs the tent-node under test.
  if want tentnode; then prepare_tentnode; fi
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
  if want tentnode; then
    check_tentnode
    log "all checks finished"
    return 0
  fi
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
