#!/usr/bin/env bash
# tent Vultr spike: measures undocumented Vultr behaviour that the provider design depends on
# (docs/adr/0018-vultr-provider-design.md, "provisional" items; docs/platform-notes.md §3, items marked 🔬).
#
# It creates REAL, BILLED resources in your Vultr account (at most 3 instances at a time, 4 in total,
# one VPC, up to three firewall groups, up to two SSH keys) and deletes them on exit unless --keep is given.
# The tentnode check runs alone: one instance, one VPC and one SSH key. The cluster check runs alone too: it builds a
# cluster of five instances (six with --unregistered, never more than five at once) with tent itself (see the cluster
# section).
# Usage and details: hack/vultr-spike/README.md
#
# Portable bash (3.2+, macOS default), requires: curl, jq 1.6+, ssh, ssh-keygen, awk, od; tentnode also go and gzip;
# cluster also openssl, mkfifo and find (and nomad, which only adds a row).
set -euo pipefail

readonly SPIKE_VERSION="13"
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
readonly SCRIPT_DIR
REPO_DIR=$(cd "$SCRIPT_DIR/../.." && pwd)
readonly REPO_DIR
readonly API_BASE="${VULTR_API_BASE:-https://api.vultr.com/v2}"
# The checks of a run without --only. tentnode and cluster run only when --only names one of them alone.
readonly DEFAULT_CHECKS="boot,inside,metadata,network,firewall,alias,tags,markers,userdata,scrub,halt,sshdup,lengths,rules,fwinuse,patchtags,vpcpending,halttwice,objstore"
readonly ALL_CHECKS="$DEFAULT_CHECKS,tentnode,cluster"
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
# cluster: the address that tent opens SSH to (default: what api.ipify.org says this machine's address is).
RUNNER_ADDR="${RUNNER_ADDR:-}"

# SSH pacing: v1 lost SSH after a burst of ~7 connections. At most one new connection to port 22 per host per
# POLL_INTERVAL keeps the spike under ufw's `limit` (6 per 30 s); SSH_MAX_FAILS failures in a row stop further
# attempts during the checks (circuit breaker).
readonly POLL_INTERVAL=15
readonly SSH_MAX_FAILS=3

MODE="run"      # run | preflight | dry-run
ASSUME_YES=0
KEEP=0
UNREG=0         # cluster: also check a client that never registers (about 30 minutes more)

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
CL_NAME=""      # cluster: the name of the cluster that tent builds
CL_STATE=""     # cluster: the directory of its file:// state store
CL_URL=""       # cluster: the URL of that store
CL_TENT=""      # cluster: the tent under test (bin/tent)
CL_OPDIR=""     # cluster: the directory tent export nomad writes the operator's files to
CL_VERSION=""   # cluster: the version of bin/tent
CL_RUNNER=""    # cluster: the address that tent opens SSH to
CL_NOMAD_NAME="server.global.nomad" # cluster: the TLS server name of the Nomad API, as tent export nomad prints it
CL_CONF=""      # cluster: the curl config file that holds the ACL token
CL_NM_STATUS="" # cluster: the HTTP status of the last Nomad API call
CL_RC=0         # cluster: the exit code of the last tent run
CL_SECS=0       # cluster: its duration in seconds
CL_T0=0         # cluster: the epoch second it started
CL_OUT=""       # cluster: the files of its stdout and stderr
CL_ERR=""
CL_PUBS=""      # cluster: the public addresses that SSH connected to, for the exit trap
CL_IP=""
CL_STARTED=0    # cluster: tent create has started
CL_DELETED=0    # cluster: tent delete cluster has exited 0
CL_SECRETS=0    # cluster: the secret files the end of the run looks for in its records
CL_SECRET_NAMES="" # cluster: what those files hold, as a list
CL_SEARCHED=0   # cluster: the run's own secret search has written its row
KEEP_WORK=0     # the exit trap leaves the temporary directory in place
CL_LEFT_INST="" CL_LEFT_VPC="" CL_LEFT_FW="" CL_LEFT_KEY=""
CL_SPECS=""     # cluster: the file with the specs that tent get printed before the guard check changed them
CL_SPECS_CHANGED=0 # cluster: the specs in the state store are not the saved ones
CL_STOPPED=""   # cluster: the node whose Nomad the check stopped and whose joined tag it took off
CL_SRV_IP=""    # cluster: the public address of the first server, which the Nomad API calls go to
CL_TOKEN_END="" # cluster: the end of the token of tent export nomad, an epoch second
CL_DOWN=""      # cluster: the client whose Nomad the validate check stopped and has not started again
CL_UI_PID=""    # cluster: the process of tent ui while it runs
CL_UI_STATUS="" # cluster: the HTTP status of the last request to tent ui
CL_UI_RC=0      # cluster: curl's exit code of that request, or the exit code of tent ui
CL_UI_ENDED=0   # cluster: the seconds that tent ui took to end after SIGINT
CL_UI_BAD=""    # cluster: what the tent ui check found wrong, one text after the other
CL_UNREG_ID=""  # cluster, --unregistered: the id of the instance of <name>-workers-1 before tent replaces it
CL_OLD_IP=""    # cluster, --unregistered: its private address
CL_NEW_IP=""    # cluster, --unregistered: the private address of the instance that replaces it
CL_BUILD_CREATE_SECS="" # cluster: the seconds that the create of the first client took in the build, or empty
SSH_NAME_USED=""

usage() {
  cat <<'EOF'
Usage: hack/vultr-spike/spike.sh [options]

Checks undocumented Vultr behaviour for tent's provider design and writes a Markdown report.

Options:
  --preflight        Only query region, plan and image (no resources; the API key is used for /plans if set).
  --dry-run          Print what would be created and exit (needs no API key).
  --yes              Do not ask for confirmation before creating billed resources.
  --keep             Do not delete resources on exit (you must delete them yourself).
  --unregistered     With "--only cluster": also check a client that never registers. The run stops Nomad on
                     <name>-workers-1, purges its node, takes tent/joined=true off the instance, waits until the
                     instance is 32 minutes old (tent's limit is 31: the intro token's 30 and a minute of leeway),
                     and checks that tent update replaces the client. About 30 minutes more; one more instance is
                     billed for the replacement.
  --only LIST        Comma-separated subset of checks (default: all but tentnode and cluster):
                     boot,inside,metadata,network,firewall,alias,tags,markers,userdata,scrub,halt,
                     sshdup,lengths,rules,fwinuse,patchtags,vpcpending,halttwice,objstore
                     ("--only objstore" needs no Vultr API key and creates no instances;
                     sshdup, lengths and rules create no instance; fwinuse, patchtags, vpcpending and
                     halttwice need only instance A. Unless a check that needs SSH is selected (boot,
                     inside, metadata, network, firewall, alias, scrub, halt), A gets a firewall group
                     with no rules at creation and the checks start once the API reports A ready)
                     tentnode is not in the default list and runs alone ("--only tentnode"): it boots
                     instance T with a development build of tent-node and checks it over SSH
                     cluster is not in the default list either and runs alone ("--only cluster"): it builds a
                     cluster of 3 servers and 2 clients with bin/tent (5 instances at once, which the account's
                     limit must allow) and checks it through the Vultr API, the Nomad API and SSH
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
  RUNNER_ADDR        cluster: the public IPv4 address of this machine, which tent lets reach SSH (default: what
                     api.ipify.org answers). The cluster check needs TENT_NODE_URL and TENT_NODE_SHA256 too, and
                     bin/tent and bin/tent-node_linux_amd64 of the same make build.
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
# Preflight (public endpoints; the key is sent with /plans only. Vultr answered /plans with HTTP 500 without a key on
# 2026-10-05 and with 200 on 2026-10-06, so the row says what this run saw).

# write_auth_conf: puts VULTR_API_KEY into a mode-0600 curl config and points AUTH_CONF at it; the key never goes on a
# command line.
write_auth_conf() {
  AUTH_CONF="$WORK/auth.conf"
  (
    umask 077
    printf 'header = "Authorization: Bearer %s"\n' "$VULTR_API_KEY" >"$AUTH_CONF"
  )
}

# cost_text COUNT: the cost of COUNT instances for the minimum hour, for what the run says before it creates them.
cost_text() {
  if [ -z "$HOURLY" ]; then printf 'price unknown (/plans did not answer without a key)'; return 0; fi
  awk -v h="$HOURLY" -v n="$1" 'BEGIN { printf "about $%.3f", h * n }'
}

preflight() {
  local plan_type="${PLAN%%-*}" monthly="" city price conf="$AUTH_CONF" plans_note
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
  if [ -n "${VULTR_API_KEY:-}" ]; then write_auth_conf; fi
  api GET "/plans?per_page=500"
  AUTH_CONF="$conf"
  if [ -n "${VULTR_API_KEY:-}" ]; then
    plans_note="/plans read with the key"
  elif api_ok; then
    plans_note="/plans answered without a key"
  else
    plans_note="/plans answered HTTP $API_STATUS without a key"
  fi
  if api_ok; then
    monthly=$(jq -r --arg p "$PLAN" '.plans[] | select(.id==$p) | .monthly_cost' "$API_BODY")
    [ -n "$monthly" ] || die "unknown plan: $PLAN"
    HOURLY=$(awk -v m="$monthly" 'BEGIN { printf "%.4f", m / 672 }')
    price="\$$monthly/month = \$$HOURLY/hour (÷672)"
  elif [ -n "${VULTR_API_KEY:-}" ]; then
    die "GET /plans failed: $(answer)"
  else
    price="plan and price not read: GET /plans without a key answered $(answer)"
  fi
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
  row "Preflight" "region $REGION ($city); $PLAN deployable; $price; os_id $OS_ID ($OS_NAME)" \
    "the availability endpoint works without a key; $plans_note"
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
  row "Metadata serves PATCHed user_data (/latest/user-data)" "$seen (secret marker lines before: $before)" "Nodes.MarkJoined (ADR-0018)"
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
# cluster: a cluster of three servers and two clients that tent builds itself with `tent create cluster --yes`, checked
# from outside (the Vultr API, the Nomad API with the operator certificate of tent export nomad) and over SSH. tent
# makes the VPC, the firewall groups, the SSH key and the instances; the script makes only a key pair and a state store
# in a temporary directory. The token of tent export nomad reaches curl only through a mode-0600 config file, the ACL
# bootstrap token is read only to compare sha256 sums, and no secret goes to the terminal or the report: the end of the
# run looks for the cluster's secrets in everything it recorded.

readonly CL_SERVERS=3
readonly CL_WORKERS=2
# Where a client keeps its intro token (internal/nodeconfig).
readonly CL_INTRO_TOKEN="/var/lib/nomad/client/intro_token.jwt"
# A line of a secret file is looked for in the report only from this length: shorter lines would match ordinary text.
readonly CL_SECRET_MIN=16

# cl_runner_addr: the address tent opens SSH to: RUNNER_ADDR, or what api.ipify.org answers.
cl_runner_addr() {
  local re='^[0-9]{1,3}(\.[0-9]{1,3}){3}$'
  CL_RUNNER="${RUNNER_ADDR:-}"
  if [ -z "$CL_RUNNER" ]; then CL_RUNNER=$(curl -sS -m 10 https://api.ipify.org 2>/dev/null || true); fi
  [[ $CL_RUNNER =~ $re ]] || die "cannot find the address of this machine: set RUNNER_ADDR to its public IPv4 address"
}

# prepare_cluster: checks the tent under test, finds the address of this machine and writes the first row.
prepare_cluster() {
  local bin="$REPO_DIR/bin/tent-node_linux_amd64" sum
  [ -n "${TENT_NODE_URL:-}" ] && [ -n "${TENT_NODE_SHA256:-}" ] ||
    die "cluster needs TENT_NODE_URL and TENT_NODE_SHA256: run make dev-upload (hack/tent-node-upload/README.md)"
  CL_TENT="$REPO_DIR/bin/tent"
  [ -x "$CL_TENT" ] && [ -f "$bin" ] || die "run make dev-upload first: cluster needs bin/tent and bin/tent-node_linux_amd64 of one build"
  sum=$(sha256 <"$bin")
  [ "$sum" = "$TENT_NODE_SHA256" ] ||
    die "bin/tent-node_linux_amd64 is not the tent-node with TENT_NODE_SHA256: run make dev-upload again"
  CL_VERSION=$("$CL_TENT" version -o json | jq -r .version) || die "bin/tent version failed"
  cl_runner_addr
  row "tent under test" "version $CL_VERSION, tent-node sha256 $TENT_NODE_SHA256; cluster $CL_NAME: $CL_SERVERS servers and $CL_WORKERS clients of $PLAN in $REGION; SSH from $CL_RUNNER/32" \
    "bin/tent builds the cluster with the development tent-node; the Nomad API takes the default access (mTLS and an ACL token from anywhere)"
}

# cl_stamp FIFO: each line that comes through FIFO with the epoch second in front, URL signatures hidden; the line
# also goes to the terminal.
cl_stamp() {
  local line
  while IFS= read -r line; do
    line=$(printf '%s' "$line" | hide_url)
    printf '%s %s\n' "$(now)" "$line"
    log "tent: $line"
  done <"$1"
}

# cl_run LABEL TENT_ARGS...: runs tent on the run's state store. Sets CL_RC, CL_SECS and CL_T0 (the epoch second of the
# start); stdout goes to cl-LABEL.out and stderr to cl-LABEL.err with the second of each line in front.
cl_run() {
  local label="$1" fifo reader
  shift
  fifo="$WORK/cl-$label.fifo"
  CL_OUT="$WORK/cl-$label.out"
  CL_ERR="$WORK/cl-$label.err"
  mkfifo "$fifo"
  cl_stamp "$fifo" >"$CL_ERR" &
  reader=$!
  CL_T0=$(now)
  CL_RC=0
  "$CL_TENT" "$@" --state "$CL_URL" >"$CL_OUT.raw" 2>"$fifo" </dev/null || CL_RC=$?
  CL_SECS=$(($(now) - CL_T0))
  wait "$reader" || true
  rm -f "$fifo"
  hide_url <"$CL_OUT.raw" >"$CL_OUT"
  rm -f "$CL_OUT.raw"
}

# cl_last_line: the last line of tent's stderr in CL_ERR, without the second in front.
cl_last_line() { tail -n 1 "$CL_ERR" 2>/dev/null | cut -d' ' -f2- | oneline 200 || true; }

# cl_out_detail TITLE: tent's stdout and stderr of the last cl_run as details.
cl_out_detail() {
  { printf 'stdout:\n'; cat "$CL_OUT"; printf '\nstderr:\n'; cut -d' ' -f2- "$CL_ERR"; } | detail "$1"
}

# cl_collect_secrets FILE...: adds the long lines of the secret files that exist to the patterns that cl_secrets_row
# looks for, and counts the files.
cl_collect_secrets() {
  local f
  for f in "$@"; do
    [ -s "$f" ] || continue
    { grep -v '^-----' "$f" || true; } | awk -v n="$CL_SECRET_MIN" 'length($0) >= n' >>"$WORK/secret-patterns.txt"
    CL_SECRETS=$((CL_SECRETS + 1))
    case "$f" in
      */ca.key) f="CA key" ;;
      */gossip.key) f="gossip key" ;;
      */acl-bootstrap-token) f="ACL bootstrap token" ;;
      */token) f="operator token" ;;
      */ui-token) f="ui token" ;;
      *) f="operator key" ;;
    esac
    CL_SECRET_NAMES="$CL_SECRET_NAMES${CL_SECRET_NAMES:+, }$f"
  done
}

cl_create() {
  local res
  CL_STARTED=1
  (umask 077; : >>"$WORK/secret-patterns.txt")
  cl_run create create cluster "$CL_NAME" --provider vultr --region "$REGION" --machine-type "$PLAN" \
    --servers "$CL_SERVERS" --workers "$CL_WORKERS" --ssh-key "$SSH_KEY.pub" --ssh-access "$CL_RUNNER/32" --yes
  if [ "$CL_RC" = 0 ]; then res="as expected: exit 0 in ${CL_SECS}s"; else res="FAILED: exit $CL_RC in ${CL_SECS}s: $(cl_last_line)"; fi
  row "tent create cluster --yes" "$res" \
    "update builds a fresh cluster: infrastructure, three servers, the ACL bootstrap, two clients, with no run between"
  cl_out_detail "tent create cluster --yes (the output of tent)"
  cl_collect_store_secrets
}

# cl_collect_store_secrets: cl_collect_secrets for the files of the state store that hold the cluster's secrets.
cl_collect_store_secrets() {
  cl_collect_secrets "$CL_STATE/$CL_NAME/pki/private/ca.key" "$CL_STATE/$CL_NAME/secrets/gossip.key" \
    "$CL_STATE/$CL_NAME/secrets/acl-bootstrap-token"
}

# cl_line_time TEXT: the epoch second of the first line of the last cl_run's stderr that holds TEXT, or nothing.
cl_line_time() { awk -v p="$1" 'index($0, p) { print $1; exit }' "$CL_ERR" 2>/dev/null || true; }

# cl_span_secs FROM TO: the seconds from the first line of the last cl_run's stderr that holds FROM to the next line
# that holds TO, or nothing.
cl_span_secs() {
  awk -v a="$1" -v b="$2" 'x == "" && index($0, a) { x = $1; next } x != "" && index($0, b) { print $1 - x; exit }' \
    "$CL_ERR" 2>/dev/null || true
}

# cl_scrub_names: the nodes whose "scrubbed the user data of node NAME" line is in the last run's stderr, one per line.
cl_scrub_names() {
  awk '$2 == "scrubbed" && $3 == "the" && $4 == "user" && $5 == "data" && $6 == "of" && $7 == "node" { print $8 }' \
    "$CL_ERR" 2>/dev/null | sort -u || true
}

# cl_scrub_early: the nodes whose scrub line comes before the line it must follow: a server's, the line that says the
# keyring is ready; a client's, its own registration line. They come out as one sorted list, space-separated.
cl_scrub_early() {
  awk '
    $2 == "node" && $NF == "registered" { reg[$3] = NR }
    index($0, "Nomad'"'"'s keyring is ready") && keyring == "" { keyring = NR }
    $2 == "scrubbed" && $7 == "node" { scrub[$8] = NR }
    END {
      for (n in scrub) {
        after = (n in reg) ? reg[n] : keyring
        if (after != "" && scrub[n] < after) print n
      }
    }' "$CL_ERR" 2>/dev/null | sort | paste -sd ' ' - || true
}

# cl_progress_row: the lines of create's progress, and the seconds from the start of create to the leader, the
# bootstrap, the healthy servers and the ready keyring, and from the leader line to each registration line. Each node
# has a scrub line: a server's after the ready keyring, a client's after its registration.
cl_progress_row() {
  local leader boot healthy keyring regs missing="" res n=0 node t list="" text scrubs early want_nodes=$((CL_SERVERS + CL_WORKERS))
  leader=$(cl_line_time "Nomad has a leader")
  boot=$(cl_line_time "bootstrapped the ACL system")
  healthy=$(cl_line_time "Nomad servers are healthy")
  keyring=$(cl_line_time "Nomad's keyring is ready")
  regs=$(awk '$2 == "node" && $NF == "registered" { print $3, $1 }' "$CL_ERR" 2>/dev/null || true)
  [ -n "$leader" ] || missing="leader"
  [ -n "$boot" ] || missing="$missing${missing:+, }bootstrap"
  [ -n "$healthy" ] || missing="$missing${missing:+, }healthy"
  [ -n "$keyring" ] || missing="$missing${missing:+, }keyring"
  if [ -n "$regs" ]; then n=$(printf '%s\n' "$regs" | wc -l | tr -d ' '); fi
  [ "$n" = "$CL_WORKERS" ] || missing="$missing${missing:+, }$n of $CL_WORKERS registrations"
  scrubs=$(cl_scrub_names | grep -c . || true)
  [ "$scrubs" = "$want_nodes" ] || missing="$missing${missing:+, }$scrubs of $want_nodes scrub lines"
  early=$(cl_scrub_early)
  CL_BUILD_CREATE_SECS=$(cl_span_secs "creating node $CL_NAME-workers-0" "created node $CL_NAME-workers-0")
  while read -r node t; do
    [ -n "$node" ] || continue
    list="$list${list:+, }$node +$((t - ${leader:-$t}))s"
  done <<<"$regs"
  text="Nomad had a leader at +$((${leader:-$CL_T0} - CL_T0))s of create, the ACL system was bootstrapped at +$((${boot:-$CL_T0} - CL_T0))s, the servers were healthy at +$((${healthy:-$CL_T0} - CL_T0))s, the keyring was ready at +$((${keyring:-$CL_T0} - CL_T0))s; registered, after the leader line: ${list:-none}; the user data of $scrubs nodes was scrubbed"
  if [ -n "$missing" ]; then
    res="UNEXPECTED: missing lines: $missing"
  elif [ -n "$early" ]; then
    res="UNEXPECTED: a scrub comes before the line it must follow: $early"
  elif [ "$boot" -lt "$leader" ] || [ "$healthy" -lt "$boot" ] || [ "$keyring" -lt "$healthy" ]; then
    res="UNEXPECTED: the lines are out of order: $text"
  else
    res="as expected: $text"
  fi
  row "Progress of create" "$res" \
    "the deadlines of the waits (10 minutes) against real times; a client registers right after its create, and every node is scrubbed once it joined"
}

# cl_instances: lists the cluster's instances by tag into cl-instances.json; fails when the API does.
cl_instances() {
  api GET "/instances?per_page=500&tag=$(urlencode "tent/cluster=$CL_NAME")"
  api_ok || return 1
  cp "$API_BODY" "$WORK/cl-instances.json"
}

# cl_instance_field LABEL FIELD: a field of the instance with that label in cl-instances.json.
cl_instance_field() {
  { jq -r --arg l "$1" --arg f "$2" '.instances[]? | select(.label == $l) | .[$f]' \
    "$WORK/cl-instances.json" 2>/dev/null | head -n 1; } || true
}

# cl_ip LABEL: the public address of the instance with that label.
cl_ip() { cl_instance_field "$1" main_ip; }

# cl_id LABEL: the id of the instance with that label.
cl_id() { cl_instance_field "$1" id; }

# cl_group_names GROUP COUNT: the names of the nodes of a node group, one per line.
cl_group_names() {
  local i=0
  while [ "$i" -lt "$2" ]; do
    printf '%s-%s-%s\n' "$CL_NAME" "$1" "$i"
    i=$((i + 1))
  done
}

cl_instances_row() {
  local want got n active res
  want=$({ cl_group_names servers "$CL_SERVERS"; cl_group_names workers "$CL_WORKERS"; } | sort | paste -sd , -)
  if ! cl_instances; then
    row "Cluster's instances in the Vultr API" "UNEXPECTED: $(answer)" "tent's labels and tags find its machines"
    return 0
  fi
  got=$(jq -r '[.instances[].label] | sort | join(",")' "$WORK/cl-instances.json")
  n=$(jq -r '.instances | length' "$WORK/cl-instances.json")
  active=$(jq -r '[.instances[] | select(.status == "active" and .power_status == "running")] | length' "$WORK/cl-instances.json")
  if [ "$n" != "$((CL_SERVERS + CL_WORKERS))" ]; then
    res="UNEXPECTED: $n instances, want $((CL_SERVERS + CL_WORKERS)): $got"
  elif [ "$active" != "$n" ]; then
    res="UNEXPECTED: $n instances, not all active and running: $active are"
  elif [ "$got" != "$want" ]; then
    res="UNEXPECTED: labels $got, want $want"
  else
    res="as expected: $n instances, all active and running: $got"
  fi
  row "Cluster's instances in the Vultr API" "$res" "tent's labels and tags find its machines; the public address of a server is the operator's way in"
  jq -r '.instances[] | [.id, .label, .main_ip, .status, .power_status, (.tags | join(","))] | @tsv' "$WORK/cl-instances.json" |
    detail "the cluster's instances (id, label, public address, status, power, tags)"
}

# cl_file_mode PATH: the octal mode of a file (GNU stat, then BSD stat).
cl_file_mode() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1" 2>/dev/null || echo '?'; }

# The slack, in seconds, of the checks of the token's end: against the 24 hours it was asked for, and against the end of
# the certificate, which tent makes a moment before it asks Nomad for the token.
readonly CL_END_SLACK=300 CL_CERT_SLACK=30

# cl_token_sha FILE: the sha256 of the token that FILE holds, without its line end. A secret is compared by this alone.
cl_token_sha() { tr -d '\r\n' <"$1" | sha256; }

# cl_export_lines HOST: the six lines that tent export nomad --shell sh prints when NOMAD_ADDR is the server at HOST.
cl_export_lines() {
  local dir
  dir=$(printf '%s' "$CL_OPDIR" | tr -s /) # tent prints the cleaned path; TMPDIR may end with a slash
  printf "export NOMAD_ADDR='https://%s:4646'\n" "$1"
  printf "export NOMAD_CACERT='%s/ca.pem'\n" "$dir"
  printf "export NOMAD_CLIENT_CERT='%s/cli.pem'\n" "$dir"
  printf "export NOMAD_CLIENT_KEY='%s/cli-key.pem'\n" "$dir"
  printf "export NOMAD_TLS_SERVER_NAME='%s'\n" "$CL_NOMAD_NAME"
  # shellcheck disable=SC2016 # the shell that runs the line expands $(cat ...), not this script
  printf 'export NOMAD_TOKEN="$(cat '"'%s/token'"')"\n' "$dir"
}

# cl_cert_check CERT CN [END]: what is wrong with the client certificate at CERT, one text per line, or nothing: its
# subject must be CN alone, its extended key usage client authentication alone, and, when END (an epoch second) is
# given, it must end within CL_CERT_SLACK seconds of END. openssl's -checkend asks whether a certificate is still valid
# after some seconds, which both openssl and LibreSSL answer, where their dates and -ext differ.
cl_cert_check() {
  local cert="$1" cn="$2" end="${3:-}" text subject eku left
  if ! text=$(openssl x509 -in "$cert" -noout -text 2>&1); then
    printf 'openssl could not read %s\n' "$(basename "$cert")"
    return 0
  fi
  subject=$(printf '%s\n' "$text" | awk '/^[[:space:]]*Subject:/ { sub(/^[[:space:]]*Subject:[[:space:]]*/, ""); gsub(/ /, ""); print; exit }')
  [ "$subject" = "CN=$cn" ] || printf '%s has the subject %s, want CN=%s\n' "$(basename "$cert")" "${subject:-none}" "$cn"
  eku=$(printf '%s\n' "$text" | awk '/Extended Key Usage/ { getline; sub(/^[[:space:]]*/, ""); print; exit }')
  [ "$eku" = "TLS Web Client Authentication" ] ||
    printf 'its extended key usage is %s, want TLS Web Client Authentication only\n' "${eku:-none}"
  [ -z "$end" ] && return 0
  left=$((end - $(now)))
  if ! openssl x509 -in "$cert" -noout -checkend $((left - CL_CERT_SLACK)) >/dev/null 2>&1 ||
    openssl x509 -in "$cert" -noout -checkend $((left + CL_CERT_SLACK)) >/dev/null 2>&1; then
    printf '%s does not end with the token (within %ss)\n' "$(basename "$cert")" "$CL_CERT_SLACK"
  fi
}

# cl_export_token_problems: what the token of tent export nomad has wrong in Nomad's answer to GET /v1/acl/token/self
# (called with that answer in nm-body.json), one text per line: its type, the start of its name and its end, which is
# 24 hours after the export. It sets CL_TOKEN_END to the token's end when the answer has one.
cl_export_token_problems() {
  local type name
  CL_TOKEN_END=""
  type=$(jq -r '.Type // ""' "$WORK/nm-body.json")
  name=$(jq -r '.Name // ""' "$WORK/nm-body.json")
  [ "$type" = management ] || printf 'the token is a %s token, not a management token\n' "${type:-?}"
  case "$name" in "tent export nomad"*) ;; *) printf 'its name does not start with tent export nomad\n' ;; esac
  CL_TOKEN_END=$(jq -r '(.ExpirationTime // empty) | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601' "$WORK/nm-body.json" 2>/dev/null || true)
  if [ -z "$CL_TOKEN_END" ]; then
    printf 'the token has no end\n'
  elif [ "$CL_TOKEN_END" -lt $((CL_T0 + 86400 - CL_END_SLACK)) ] || [ "$CL_TOKEN_END" -gt $((CL_T0 + CL_SECS + 86400 + CL_END_SLACK)) ]; then
    printf 'the token ends %ss after the export, want 86400s (24 hours)\n' "$((CL_TOKEN_END - CL_T0))"
  fi
}

# cl_export_checks IP: what is wrong with a successful tent export nomad (its output is in CL_OUT and CL_ERR), one text
# per line: the modes of the directory and the files, NOMAD_ADDR and the six lines, the token (not the bootstrap token,
# in neither stream) and, from Nomad's answer for it through the server at IP, its type, name and end, and the
# certificate. It needs CL_NOMAD_NAME and, for the token file, CL_CONF. Its last line is "checked": the checks run in a
# subshell, where a command that fails would end them without a trace.
cl_export_checks() {
  local ip="$1" f addr host s i known="" streams=""
  for f in ca.pem cli.pem cli-key.pem token; do
    [ "$(cl_file_mode "$CL_OPDIR/$f")" = 600 ] || printf '%s has mode %s\n' "$f" "$(cl_file_mode "$CL_OPDIR/$f")"
  done
  [ "$(cl_file_mode "$CL_OPDIR")" = 700 ] || printf 'the directory has mode %s\n' "$(cl_file_mode "$CL_OPDIR")"
  [ -n "$CL_NOMAD_NAME" ] || printf 'no NOMAD_TLS_SERVER_NAME line\n'
  addr=$(sed -n "s/^export NOMAD_ADDR='\\(.*\\)'\$/\\1/p" "$CL_OUT")
  for i in $(seq 0 $((CL_SERVERS - 1))); do
    s=$(cl_ip "$CL_NAME-servers-$i")
    if [ -n "$s" ] && [ "$addr" = "https://$s:4646" ]; then known=$s; fi
  done
  [ -n "$known" ] || printf 'NOMAD_ADDR %s is not the public address of a server\n' "${addr:-missing}"
  host=${addr#https://}
  host=${host%:4646}
  cl_export_lines "$host" | cmp -s - "$CL_OUT" || printf 'stdout is not the six lines of the sh form for %s\n' "$CL_OPDIR"
  if [ ! -s "$CL_OPDIR/token" ]; then
    printf 'token is missing or empty\nchecked\n'
    return 0
  fi
  [ "$(cl_token_sha "$CL_OPDIR/token")" != "$(cl_token_sha "$CL_STATE/$CL_NAME/secrets/acl-bootstrap-token")" ] ||
    printf 'the token is the bootstrap token\n'
  if grep -Fq -f "$CL_OPDIR/token" "$CL_OUT"; then streams="stdout"; fi
  if grep -Fq -f "$CL_OPDIR/token" "$CL_ERR"; then streams="$streams${streams:+, }stderr"; fi
  [ -z "$streams" ] || printf 'the token is in %s\n' "$streams"
  cl_nm_get "$ip" /v1/acl/token/self
  if [ "$CL_NM_STATUS" != 200 ]; then
    printf 'GET %s\n' "$(cl_nm_error /v1/acl/token/self)"
  else
    cl_export_token_problems
    rm -f "$WORK/nm-body.json" # it holds the token's secret
  fi
  cl_cert_check "$CL_OPDIR/cli.pem" "cli.${CL_NOMAD_NAME#server.}" "${CL_TOKEN_END:-}"
  printf 'checked\n'
}

# cl_export IP: tent export nomad makes the operator's files and the Nomad variables, which the Nomad rows then use
# through the server at IP.
cl_export() {
  local bad="" res p checked=0
  cl_run export export nomad "$CL_NAME" --dir "$CL_OPDIR" --shell sh
  cl_out_detail "tent export nomad (stdout: paths only)"
  if [ "$CL_RC" != 0 ]; then
    res="FAILED: exit $CL_RC in ${CL_SECS}s: $(cl_last_line)"
  else
    CL_NOMAD_NAME=$(sed -n "s/^export NOMAD_TLS_SERVER_NAME='\\(.*\\)'\$/\\1/p" "$CL_OUT")
    if [ -s "$CL_OPDIR/token" ]; then
      # The token goes into a config file that only this user reads: curl never gets it on a command line.
      (umask 077; printf 'header = "X-Nomad-Token: %s"\n' "$(cat "$CL_OPDIR/token")" >"$WORK/nomad-curl.conf")
      CL_CONF="$WORK/nomad-curl.conf"
    fi
    while IFS= read -r p; do
      if [ "$p" = checked ]; then checked=1; else bad="$bad${bad:+; }$p"; fi
    done < <(cl_export_checks "$1")
    [ "$checked" = 1 ] || bad="$bad${bad:+; }the checks did not finish"
    cl_collect_secrets "$CL_OPDIR/cli-key.pem" "$CL_OPDIR/token"
    if [ -n "$bad" ]; then
      res="UNEXPECTED: $bad"
    else
      res="as expected: exit 0 in ${CL_SECS}s; directory 700, four files 600; six lines with NOMAD_ADDR $(sed -n "s/^export NOMAD_ADDR='\\(.*\\)'\$/\\1/p" "$CL_OUT"); the token is not the bootstrap token and is in neither stream; management token named tent export nomad ..., ends 24 hours after the export; cli.pem: CN cli.${CL_NOMAD_NAME#server.}, client authentication only, ends with the token"
    fi
  fi
  row "tent export nomad" "$res" "the operator's access to the Nomad API: a certificate for cli.<region>.nomad and a management token that ends in 24 hours"
}

# cl_need_api TITLE: true when the operator's files exist; else writes the row of TITLE as unknown and fails.
cl_need_api() {
  [ -n "$CL_CONF" ] && return 0
  row "$1" "unknown: no operator files" "tent export nomad failed"
  return 1
}

# cl_nm IP METHOD PATH [BODY_FILE]: one call of the Nomad API of the server at IP with the operator's certificate and
# token; sets CL_NM_STATUS and leaves the body in nm-body.json.
cl_nm() {
  local ip="$1" method="$2" path="$3" body="${4:-}"
  local args=(-sS -X "$method" -o "$WORK/nm-body.json" -w '%{http_code}' --max-time 20 --cacert "$CL_OPDIR/ca.pem"
    --cert "$CL_OPDIR/cli.pem" --key "$CL_OPDIR/cli-key.pem" --resolve "$CL_NOMAD_NAME:4646:$ip" -K "$CL_CONF"
    -H 'Content-Type: application/json')
  if [ -n "$body" ]; then args+=(--data-binary "@$body"); fi
  CL_NM_STATUS=$(curl "${args[@]}" "https://$CL_NOMAD_NAME:4646$path" 2>>"$WORK/curl-errors.log" || true)
  [ -n "$CL_NM_STATUS" ] || CL_NM_STATUS=000
}

# cl_nm_get IP PATH: a GET that tries again twice, 5 s apart, when nothing answered.
cl_nm_get() {
  local i
  for i in 1 2 3; do
    cl_nm "$1" GET "$2"
    [ "$CL_NM_STATUS" = 000 ] || return 0
    [ "$i" = 3 ] || sleep 5
  done
}

# cl_health_answered: true when the last call got a health report: Nomad answers 200 for healthy servers and 429 with the
# same report for unhealthy ones.
cl_health_answered() { [ "$CL_NM_STATUS" = 200 ] || [ "$CL_NM_STATUS" = 429 ]; }

# cl_nm_error PATH: why the last call failed, on one line.
cl_nm_error() { printf '%s answered HTTP %s: %s' "$1" "$CL_NM_STATUS" "$(oneline 150 <"$WORK/nm-body.json" 2>/dev/null)"; }

cl_members_row() {
  local title="Nomad servers (agent/members)" alive total names res
  cl_need_api "$title" || return 0
  cl_nm_get "$1" /v1/agent/members
  if [ "$CL_NM_STATUS" != 200 ]; then
    res="UNEXPECTED: $(cl_nm_error /v1/agent/members)"
  else
    alive=$(jq -r '[.Members[] | select(.Status == "alive")] | length' "$WORK/nm-body.json")
    total=$(jq -r '.Members | length' "$WORK/nm-body.json")
    names=$(jq -r '[.Members[] | select(.Status == "alive") | .Name] | sort | join(",")' "$WORK/nm-body.json")
    if [ "$alive" = "$CL_SERVERS" ] && [ "$total" = "$CL_SERVERS" ]; then res="as expected: $alive alive: $names"; else res="UNEXPECTED: $alive alive of $total: $names"; fi
  fi
  row "$title" "$res" "GET /v1/agent/members over mTLS with the operator's certificate and the token"
}

# cl_health_text: "healthy, N voters" or "not healthy, N voters" from the last autopilot answer; empty when the body is
# not a health report (Nomad's connection limit answers 429 with plain text).
cl_health_text() {
  jq -r '(if .Healthy then "healthy" else "not healthy" end) + ", "
    + ([.Servers[]? | select(.Voter)] | length | tostring) + " voters"' "$WORK/nm-body.json" 2>/dev/null || true
}

cl_health_row() {
  local title="Autopilot health" res
  cl_need_api "$title" || return 0
  cl_nm_get "$1" /v1/operator/autopilot/health
  if cl_health_answered; then res=$(cl_health_text); else res=""; fi
  if [ -z "$res" ]; then
    res="UNEXPECTED: $(cl_nm_error /v1/operator/autopilot/health)"
  elif [ "$res" = "healthy, $CL_SERVERS voters" ]; then
    res="as expected: $res"
  else
    res="UNEXPECTED: $res"
  fi
  row "$title" "$res" "all servers vote at the first call after the bootstrap"
}

cl_nodes_row() {
  local title="Nomad clients (nodes)" n re names want res
  cl_need_api "$title" || return 0
  cl_nm_get "$1" /v1/nodes
  if [ "$CL_NM_STATUS" != 200 ]; then
    row "$title" "UNEXPECTED: $(cl_nm_error /v1/nodes)" "the clients registered with their intro tokens"
    return 0
  fi
  n=$(jq -r 'length' "$WORK/nm-body.json")
  re=$(jq -r '[.[] | select(.Status == "ready" and .SchedulingEligibility == "eligible")] | length' "$WORK/nm-body.json")
  names=$(jq -r '[.[] | .Name] | sort | join(",")' "$WORK/nm-body.json")
  want=$(cl_group_names workers "$CL_WORKERS" | sort | paste -sd , -)
  if [ "$n" != "$CL_WORKERS" ] || [ "$re" != "$CL_WORKERS" ]; then
    res="UNEXPECTED: $n node(s), $re ready and eligible: $names"
  elif [ "$names" != "$want" ]; then
    res="UNEXPECTED: names $names, want $want"
  else
    res="as expected: $n ready and eligible: $names"
  fi
  row "$title" "$res" "GET /v1/nodes: the clients registered with their intro tokens, under their instance labels"
}

# cl_job_json: the job of the check as the HTTP API takes it: what the nomad CLI sends for tn_job_spec's HCL, without the
# fields it leaves null.
cl_job_json() {
  jq -n --arg id "$TN_JOB" --arg image "$TN_IMAGE" --arg body "$TN_JOB_BODY" '{Job: {
    ID: $id, Name: $id, Datacenters: ["*"],
    TaskGroups: [{Name: "web",
      Networks: [{Mode: "bridge", DynamicPorts: [{Label: "http", To: 8080}]}],
      Tasks: [{Name: "web", Driver: "docker",
        Config: {image: $image, command: "sh",
          args: ["-c", ("mkdir -p /www && echo " + $body + " >/www/index.html && exec httpd -f -p 8080 -h /www")]},
        Resources: {CPU: 50, MemoryMB: 32}}]}]}}'
}

# cl_job IP: registers the job with PUT /v1/jobs, waits up to 3 minutes for an allocation that runs on a client, then
# purges the job and waits up to a minute for it to stop.
cl_job() {
  local title="The job $TN_JOB" ip="$1" t0 i alloc node res="" code running
  cl_need_api "$title" || { cl_need_api "Stop of the job $TN_JOB" || true; return 0; }
  cl_job_json >"$WORK/cl-job.json"
  cl_nm "$ip" PUT /v1/jobs "$WORK/cl-job.json"
  if [ "$CL_NM_STATUS" != 200 ]; then
    row "$title" "FAILED: PUT /v1/jobs answered HTTP $CL_NM_STATUS: $(oneline 150 <"$WORK/nm-body.json")" "a docker job in bridge mode runs on a client"
    return 0
  fi
  t0=$(now)
  running=""
  for i in $(seq 1 36); do
    cl_nm_get "$ip" "/v1/job/$TN_JOB/allocations"
    running=$(jq -r '[.[]? | select(.ClientStatus == "running" and .DesiredStatus == "run")][0] | "\(.ID) \(.NodeName)"' "$WORK/nm-body.json" 2>/dev/null || true)
    case "$running" in "" | "null null") running="" ;; *) break ;; esac
    sleep 5
  done
  if [ -n "$running" ]; then
    read -r alloc node <<<"$running"
    case "$node" in
      "$CL_NAME-workers-"*) res="as expected: allocation $alloc runs on $node after $(($(now) - t0))s" ;;
      *) res="UNEXPECTED: allocation $alloc runs on $node, which is not a client" ;;
    esac
  else
    res="FAILED: no allocation ran within 180s: $(jq -c '[.[]? | {ClientStatus, DesiredStatus, NodeName}]' "$WORK/nm-body.json" 2>/dev/null | oneline 200)"
  fi
  row "$title" "$res" "a docker job in bridge mode runs on a client: the CNI plugins, Docker and the registration work together"
  cl_nm "$ip" DELETE "/v1/job/$TN_JOB?purge=true"
  code=$CL_NM_STATUS
  t0=$(now)
  running="?"
  for i in $(seq 1 12); do
    cl_nm_get "$ip" "/v1/job/$TN_JOB/allocations"
    running=$(jq -r '[.[]? | select(.ClientStatus == "running")] | length' "$WORK/nm-body.json" 2>/dev/null || echo '?')
    [ "$running" = 0 ] && break
    sleep 5
  done
  if [ "$code" != 200 ]; then
    res="UNEXPECTED: DELETE answered HTTP $code"
  elif [ "$running" = 0 ]; then
    res="as expected: purged; no allocation runs after $(($(now) - t0))s"
  else
    res="UNEXPECTED: an allocation still runs $(($(now) - t0))s after the purge"
  fi
  row "Stop of the job $TN_JOB" "$res" "DELETE /v1/job/<id>?purge=true stops the allocation"
}

# The title of the nomad CLI row, which check_cluster also writes when no Nomad check ran.
readonly CL_CLI_TITLE="nomad server members (nomad CLI)"

# cl_down_title STEP: the title of the row of a step of the check of a client whose Nomad stops.
cl_down_title() {
  case "$1" in
    stop) printf 'Nomad stopped on %s-workers-1 (for validate)' "$CL_NAME" ;;
    down) printf 'tent validate cluster (client down)' ;;
    back) printf 'tent validate cluster --wait 5m (client back)' ;;
  esac
}

# cl_nomad_cli_row: the nomad CLI, pointed at the cluster by the six lines of tent export nomad, lists the servers. The
# table is the CLI's stdout alone: on stderr the CLI of Nomad 2.0.7 adds a hint with the URL of the UI.
cl_nomad_cli_row() {
  local title="$CL_CLI_TITLE" out rc=0 alive total names res err="$WORK/cl-nomad-cli.err"
  if ! command -v nomad >/dev/null 2>&1; then
    row "$title" "skipped: no nomad binary on the PATH" "the six lines of tent export nomad are what a person runs"
    return 0
  fi
  cl_need_api "$title" || return 0
  : >"$err"
  out=$(
    unset NOMAD_REGION NOMAD_NAMESPACE NOMAD_SKIP_VERIFY NOMAD_CAPATH NOMAD_HTTP_AUTH
    # shellcheck source=/dev/null
    . "$WORK/cl-export.out"
    nomad server members 2>"$err"
  ) || rc=$?
  { printf 'stdout:\n%s\n\nstderr:\n' "$out"; cat "$err"; } | detail "$title"
  if [ "$rc" != 0 ]; then
    res="FAILED: exit $rc: $(oneline 150 <"$err")"
  else
    alive=$(printf '%s\n' "$out" | awk 'NR > 1 && $4 == "alive"' | wc -l | tr -d ' ')
    total=$(printf '%s\n' "$out" | awk 'NR > 1 && NF' | wc -l | tr -d ' ')
    names=$(printf '%s\n' "$out" | awk 'NR > 1 && $4 == "alive" { print $1 }' | sort | paste -sd , -)
    if [ "$alive" = "$CL_SERVERS" ] && [ "$total" = "$CL_SERVERS" ]; then
      res="as expected: $alive alive servers: $names"
    else
      res="UNEXPECTED: $alive alive of $total: $names"
    fi
  fi
  row "$title" "$res" "after eval of the six lines, the nomad CLI works with the exported files"
}

# cl_validate_row: tent validate cluster on the cluster as it was built: exit 0, the valid line and the two warnings
# that a cluster of tent's defaults gets.
cl_validate_row() {
  local title="tent validate cluster" want="cluster $CL_NAME is valid: $CL_SERVERS servers and $CL_WORKERS clients" bad="" res
  cl_run validate validate cluster "$CL_NAME"
  cl_out_detail "$title"
  case "$CL_RC" in
    0)
      grep -Fq -- "$want" "$CL_OUT" || bad="stdout lacks the line \"$want\""
      grep -Fq -- "spec.access.api lets the whole internet" "$CL_ERR" ||
        bad="$bad${bad:+; }stderr lacks the warning about spec.access.api"
      grep -Fq -- "runs in one failure domain" "$CL_ERR" ||
        bad="$bad${bad:+; }stderr lacks the warning about one failure domain"
      if [ -n "$bad" ]; then
        res="UNEXPECTED: exit 0 in ${CL_SECS}s but $bad"
      else
        res="as expected: exit 0 in ${CL_SECS}s: $(oneline 120 <"$CL_OUT"); warnings: open spec.access.api and one failure domain"
      fi
      ;;
    2) res="UNEXPECTED: exit 2 in ${CL_SECS}s: $(oneline 250 <"$CL_OUT")" ;;
    *) res="FAILED: exit $CL_RC in ${CL_SECS}s: $(cl_last_line)" ;;
  esac
  row "$title" "$res" "a cluster that tent built is valid; its open access.api and its one failure domain are warnings"
}

# cl_ui_get URL [CURL_ARG...]: one request to tent ui; sets CL_UI_STATUS and CL_UI_RC and leaves the body in
# ui-body.json.
cl_ui_get() {
  local url="$1"
  shift
  CL_UI_RC=0
  CL_UI_STATUS=$(curl -sS -o "$WORK/ui-body.json" -w '%{http_code}' --max-time 20 "$@" "$url" 2>>"$WORK/curl-errors.log") || CL_UI_RC=$?
}

cl_ui_note() { CL_UI_BAD="$CL_UI_BAD${CL_UI_BAD:+; }$1"; } # cl_ui_note TEXT: adds what the tent ui check found wrong

# cl_ui_wait: waits for tent ui, which has ended or has been killed, and sets CL_UI_RC to its exit code.
cl_ui_wait() {
  CL_UI_RC=0
  wait "$CL_UI_PID" 2>/dev/null || CL_UI_RC=$?
  CL_UI_PID=""
}

# cl_ui_kill: ends tent ui when it still runs; for the exit trap and for a tent ui that does not answer.
cl_ui_kill() {
  [ -n "$CL_UI_PID" ] || return 0
  kill -KILL "$CL_UI_PID" 2>/dev/null || true
  cl_ui_wait
}

# cl_ui_token: judges the token that the proxy used, from GET /v1/acl/token/self through tent ui: a management
# token named tent ui..., not the bootstrap token, in ui-body.json. It reads the secret only to compare its sha256
# and to hide it in the report, and deletes the answer.
cl_ui_token() {
  local type name
  type=$(jq -r '.Type // ""' "$WORK/ui-body.json")
  name=$(jq -r '.Name // ""' "$WORK/ui-body.json")
  (umask 077; jq -j '.SecretID // empty' "$WORK/ui-body.json" >"$WORK/ui-token")
  rm -f "$WORK/ui-body.json"
  [ -s "$WORK/ui-token" ] || cl_ui_note "the answer holds no SecretID"
  [ "$type" = management ] || cl_ui_note "the token is a ${type:-?} token, not a management token"
  case "$name" in "tent ui"*) ;; *) cl_ui_note "its name does not start with tent ui" ;; esac
  if [ "$(cl_token_sha "$WORK/ui-token")" = "$(cl_token_sha "$CL_STATE/$CL_NAME/secrets/acl-bootstrap-token")" ]; then
    cl_ui_note "the proxy used the bootstrap token"
  fi
  cl_collect_secrets "$WORK/ui-token"
  rm -f "$WORK/ui-token"
}

# cl_ui_probe BASE: four requests through tent ui at BASE, which needs no certificate and no token: the leader, the
# token the proxy uses, the UI page, and a request with another Host, which the proxy refuses.
cl_ui_probe() {
  local base="$1"
  cl_ui_get "$base/v1/status/leader"
  [ "$CL_UI_STATUS" = 200 ] || cl_ui_note "GET /v1/status/leader answered HTTP $CL_UI_STATUS"
  cl_ui_get "$base/v1/acl/token/self"
  if [ "$CL_UI_STATUS" = 200 ]; then cl_ui_token; else cl_ui_note "GET /v1/acl/token/self answered HTTP $CL_UI_STATUS"; fi
  rm -f "$WORK/ui-body.json"
  cl_ui_get "$base/ui/"
  [ "$CL_UI_STATUS" = 200 ] || cl_ui_note "GET /ui/ answered HTTP $CL_UI_STATUS"
  cl_ui_get "$base/v1/status/leader" -H 'Host: example.com'
  [ "$CL_UI_STATUS" = 403 ] || cl_ui_note "Host example.com was not refused: HTTP $CL_UI_STATUS"
}

# cl_ui_end BASE: sends SIGINT to tent ui, waits up to 15 seconds for it to end, kills it when it does not, and notes
# whether it exited with 0 and whether its port refuses connections; a tent ui that ended before the signal is noted
# with its exit code. It sets CL_UI_ENDED to the seconds it took.
cl_ui_end() {
  local t0 deadline killed=0
  t0=$(now)
  deadline=$((t0 + 15))
  if ! kill -INT "$CL_UI_PID" 2>/dev/null; then
    cl_ui_wait
    cl_ui_note "it ended by itself with exit $CL_UI_RC before SIGINT"
  else
    while kill -0 "$CL_UI_PID" 2>/dev/null && [ "$(now)" -lt "$deadline" ]; do sleep 1; done
    CL_UI_ENDED=$(($(now) - t0))
    if kill -0 "$CL_UI_PID" 2>/dev/null; then
      killed=1
      kill -KILL "$CL_UI_PID" 2>/dev/null || true
    fi
    cl_ui_wait
    if [ "$killed" = 1 ]; then
      cl_ui_note "it still ran 15s after SIGINT and was killed"
    elif [ "$CL_UI_RC" != 0 ]; then
      cl_ui_note "SIGINT ended it with exit $CL_UI_RC"
    fi
  fi
  cl_ui_get "$1/v1/status/leader"
  [ "$CL_UI_RC" = 7 ] || cl_ui_note "the port still answers after the exit (curl exit $CL_UI_RC, HTTP $CL_UI_STATUS)"
}

# cl_ui_row: tent ui on a free loopback port, in the background: it prints its address, answers without a certificate
# or a token, refuses another Host, and ends with exit 0 on SIGINT and closes its port. A tent ui that does not end is
# killed.
cl_ui_row() {
  local title="tent ui" out="$WORK/cl-ui.out" err="$WORK/cl-ui.err" url="" deadline res
  CL_UI_BAD=""
  CL_UI_ENDED=0
  : >"$out"
  : >"$err"
  "$CL_TENT" ui "$CL_NAME" --listen 127.0.0.1:0 --state "$CL_URL" >"$out" 2>"$err" </dev/null &
  CL_UI_PID=$!
  deadline=$(($(now) + 60))
  while [ "$(now)" -lt "$deadline" ]; do
    url=$(sed -n "s|^Nomad UI of cluster $CL_NAME: \\(http://127\\.0\\.0\\.1:[0-9]*/ui/\\)\$|\\1|p" "$out" | head -n 1)
    [ -z "$url" ] || break
    kill -0 "$CL_UI_PID" 2>/dev/null || break
    sleep 1
  done
  if [ -z "$url" ]; then
    if kill -0 "$CL_UI_PID" 2>/dev/null; then
      res="FAILED: tent ui printed no address within 60s"
    else
      cl_ui_wait
      res="FAILED: tent ui exited $CL_UI_RC before it printed its address: $(oneline 200 <"$err")"
    fi
    cl_ui_kill
  else
    cl_ui_probe "${url%/ui/}"
    cl_ui_end "${url%/ui/}"
    if [ -n "$CL_UI_BAD" ]; then
      res="UNEXPECTED: $CL_UI_BAD"
    else
      res="as expected: GET /v1/status/leader 200; GET /v1/acl/token/self: a management token named tent ui..., not the bootstrap token; GET /ui/ 200; Host example.com 403; SIGINT ended it with exit 0 after ${CL_UI_ENDED}s and the port refuses connections"
    fi
  fi
  { printf 'stdout:\n'; cat "$out"; printf '\nstderr:\n'; cat "$err"; } | hide_url | detail "tent ui"
  row "$title" "$res" "the Nomad UI and API on a loopback port, with a token that only tent knows; the port closes with the process"
}

# cl_down_rows: stops Nomad on a client, waits until Nomad lists its node down, and checks that tent validate cluster
# exits 2 and names the node; then starts Nomad again and checks that validate --wait 5m exits 0.
cl_down_rows() {
  local name="$CL_NAME-workers-1" t_stop t_down t_back out rc active t0 deadline st="" res
  t_stop=$(cl_down_title stop)
  t_down=$(cl_down_title down)
  t_back=$(cl_down_title back)
  if ! cl_need_api "$t_stop"; then
    row "$t_down" "unknown: Nomad was not stopped" "no check ran"
    row "$t_back" "unknown: Nomad was not stopped" "no check ran"
    return 0
  fi
  if ! cl_ssh_reachable "$name"; then
    row "$t_stop" "$(cl_ssh_unknown_text "$name")" "no check ran"
    row "$t_down" "unknown: Nomad was not stopped" "no check ran"
    row "$t_back" "unknown: Nomad was not stopped" "no check ran"
    return 0
  fi
  cl_unit_script stop >"$WORK/cl-unit.sh"
  out=$(ssh_x "$CL_IP" 'sh -s' <"$WORK/cl-unit.sh" 2>/dev/null || true)
  rc=$(tn_key rc "$out")
  active=$(tn_key active "$out")
  printf '%s\n' "$out" | detail "systemctl stop nomad.service on $name (for validate)"
  case "$rc/$active" in
    0/inactive | 0/failed) ;;
    *)
      row "$t_stop" "UNEXPECTED: stop exit ${rc:-?}, unit ${active:-?}" "no check ran"
      row "$t_down" "unknown: Nomad was not stopped" "no check ran"
      row "$t_back" "unknown: Nomad was not stopped" "no check ran"
      return 0
      ;;
  esac
  CL_DOWN="$name"
  t0=$(now)
  deadline=$((t0 + 300))
  while [ "$(now)" -lt "$deadline" ]; do
    cl_nm_get "$CL_SRV_IP" /v1/nodes
    st=$(jq -r --arg n "$name" '[.[]? | select(.Name == $n) | .Status][0] // ""' "$WORK/nm-body.json" 2>/dev/null || true)
    [ "$st" != down ] || break
    sleep 5
  done
  if [ "$st" = down ]; then
    row "$t_stop" "as expected: systemctl stop exit 0, nomad.service $active; Nomad listed the node down after $(($(now) - t0))s" \
      "a client whose Nomad stops is listed down once its heartbeat lapses"
    cl_run validate-down validate cluster "$CL_NAME"
    cl_out_detail "$t_down"
    case "$CL_RC" in
      2)
        if grep -Eq -- "^$name +its Nomad client is down\$" "$CL_OUT"; then
          res="as expected: exit 2 in ${CL_SECS}s, $name: its Nomad client is down"
        else
          res="UNEXPECTED: exit 2 in ${CL_SECS}s but no failure names $name with \"its Nomad client is down\": $(oneline 200 <"$CL_OUT")"
        fi
        ;;
      0) res="UNEXPECTED: exit 0 in ${CL_SECS}s: validate calls the cluster valid with Nomad stopped on $name" ;;
      *) res="FAILED: exit $CL_RC in ${CL_SECS}s: $(cl_last_line)" ;;
    esac
    row "$t_down" "$res" "validate exits 2 and names the node whose Nomad client is down"
  else
    row "$t_stop" "UNEXPECTED: Nomad did not list the node down within 300s (status ${st:-none})" \
      "a client whose Nomad stops is listed down once its heartbeat lapses"
    row "$t_down" "unknown: Nomad did not list the node down" "no check ran"
  fi
  cl_unit_script start >"$WORK/cl-unit.sh"
  out=$(ssh_x "$CL_IP" 'sh -s' <"$WORK/cl-unit.sh" 2>/dev/null || true)
  rc=$(tn_key rc "$out")
  active=$(tn_key active "$out")
  printf '%s\n' "$out" | detail "systemctl start nomad.service on $name"
  if [ "$rc/$active" != 0/active ]; then
    row "$t_back" "unknown: systemctl start nomad.service exit ${rc:-?}, unit ${active:-?}" "no check ran"
    return 0
  fi
  CL_DOWN=""
  cl_run validate-back validate cluster "$CL_NAME" --wait 5m
  cl_out_detail "$t_back"
  case "$CL_RC" in
    0) res="as expected: exit 0 in ${CL_SECS}s after systemctl start nomad.service" ;;
    2) res="UNEXPECTED: exit 2 in ${CL_SECS}s: $(oneline 200 <"$CL_OUT")" ;;
    *) res="FAILED: exit $CL_RC in ${CL_SECS}s: $(cl_last_line)" ;;
  esac
  row "$t_back" "$res" "once Nomad runs again the node registers and the cluster is valid"
}

# cl_exit_code_row LABEL TITLE IMPACT: tent update cluster --exit-code must exit 0 (no changes); the row and the output
# as details.
cl_exit_code_row() {
  local res
  cl_run "$1" update cluster "$CL_NAME" --exit-code
  case "$CL_RC" in
    0) res="as expected: exit 0 in ${CL_SECS}s" ;;
    2) res="UNEXPECTED: exit 2 (the plan has changes): $(oneline 200 <"$CL_OUT")" ;;
    *) res="FAILED: exit $CL_RC: $(cl_last_line)" ;;
  esac
  row "$2" "$res" "$3"
  cl_out_detail "$2"
}

# cl_update_rows: a second run on the built cluster: the plan with --exit-code is empty, and update --yes says so.
cl_update_rows() {
  local res first
  cl_exit_code_row update-plan "tent update cluster --exit-code" "a built cluster has no drift: the second run plans nothing"
  cl_run update-yes update cluster "$CL_NAME" --yes
  first=$(head -n 1 "$CL_OUT")
  if [ "$CL_RC" = 0 ] && [ "$first" = "cluster $CL_NAME is up to date" ]; then
    res="as expected: $first (${CL_SECS}s)"
  elif [ "$CL_RC" = 0 ]; then
    res="UNEXPECTED: ${first:-no output}"
  else
    res="FAILED: exit $CL_RC: $(cl_last_line)"
  fi
  row "tent update cluster --yes" "$res" "a built cluster is up to date, and the run only reads"
  cl_out_detail "tent update cluster --yes (second run)"
}

# cl_listed: true when cl_instances got the instance list.
cl_listed() { [ -s "$WORK/cl-instances.json" ]; }

cl_tags_row() {
  local n have distinct res
  if ! cl_listed; then
    row "Instance tags tent/spec-hash" "unknown: no instance list" "the hash says which spec built a node"
    return 0
  fi
  n=$(jq -r '.instances | length' "$WORK/cl-instances.json" 2>/dev/null || echo 0)
  have=$(jq -r '[.instances[] | select(any(.tags[]?; startswith("tent/spec-hash=")))] | length' "$WORK/cl-instances.json" 2>/dev/null || echo 0)
  distinct=$(jq -r '[.instances[] | .tags[]? | select(startswith("tent/spec-hash="))] | unique | length' "$WORK/cl-instances.json" 2>/dev/null || echo 0)
  if [ "$n" -gt 0 ] && [ "$have" = "$n" ]; then
    res="as expected: $have of $n carry a tent/spec-hash tag; $distinct distinct value(s)"
  else
    res="UNEXPECTED: $have of $n carry a tent/spec-hash tag"
  fi
  row "Instance tags tent/spec-hash" "$res" "the hash says which spec built a node: a later rollout compares it"
}

# cl_joined_row: every instance carries tent/joined=true beside tent/spec-hash.
cl_joined_row() {
  local n have other res
  cl_instances || true # a fresh list: the one from the first row may predate the last PATCH of a tag
  if ! cl_listed; then
    row "Instance tags tent/joined" "unknown: no instance list" "the label says which machines joined their cluster"
    return 0
  fi
  n=$(jq -r '.instances | length' "$WORK/cl-instances.json" 2>/dev/null || echo 0)
  have=$(jq -r '[.instances[] | select(any(.tags[]?; . == "tent/joined=true") and any(.tags[]?; startswith("tent/spec-hash=")))] | length' "$WORK/cl-instances.json" 2>/dev/null || echo 0)
  other=$(jq -r '[.instances[] | select(any(.tags[]?; startswith("tent/joined=") and . != "tent/joined=true"))] | length' "$WORK/cl-instances.json" 2>/dev/null || echo 0)
  if [ "$n" -gt 0 ] && [ "$have" = "$n" ] && [ "$other" = 0 ]; then
    res="as expected: $have of $n carry tent/joined=true beside tent/spec-hash"
  else
    res="UNEXPECTED: $have of $n carry tent/joined=true; $other carry another value"
  fi
  row "Instance tags tent/joined" "$res" "the label says which machines joined their cluster: the next update plans only the others"
}

# cl_stub: the user data that tent leaves on a node that joined, byte for byte (scrubbedUserData in
# internal/cloud/vultr/nodes.go).
cl_stub() {
  printf '%s\n%s\n%s\n' '#cloud-config' "# tent removed this node's user data after the node joined the cluster" '{}'
}

# cl_ud_kind ID: what the user data of instance ID is: "stub" (tent's stub, byte for byte), "config" (it holds
# /etc/tent/node.json), "other" or "unknown" (the API did not answer). It prints no part of the user data.
cl_ud_kind() {
  local sum has
  api GET "/instances/$1/user-data"
  api_ok || { echo unknown; return 0; }
  sum=$(jq -r '.user_data.data // empty' "$API_BODY" | b64dec | sha256)
  has=$(jq -r '.user_data.data // empty' "$API_BODY" | b64dec | grep -Fc '/etc/tent/node.json' || true)
  rm -f "$API_BODY" # a node's user data may hold its secrets: it stays on disk no longer than this needs
  if [ "$has" -gt 0 ]; then
    echo config
  elif [ "$sum" = "$(cl_stub | sha256)" ]; then
    echo stub
  else
    echo other
  fi
}

# cl_userdata_row: the user data of each instance, read from the API, is tent's stub.
cl_userdata_row() {
  local ids id n=0 stub=0 config=0 other=0 unknown=0 res
  if ! cl_listed; then
    row "User data after the build" "unknown: no instance list" "a node's own curl to the metadata service gets no answer, so the row reads the API"
    return 0
  fi
  ids=$(jq -r '.instances[]?.id' "$WORK/cl-instances.json" 2>/dev/null || true)
  for id in $ids; do
    n=$((n + 1))
    case "$(cl_ud_kind "$id")" in
      stub) stub=$((stub + 1)) ;;
      config) config=$((config + 1)) ;;
      other) other=$((other + 1)) ;;
      *) unknown=$((unknown + 1)) ;;
    esac
  done
  if [ "$n" -gt 0 ] && [ "$stub" = "$n" ]; then
    res="as expected: $stub of $n equal the stub byte for byte; none holds /etc/tent/node.json"
  else
    res="UNEXPECTED: $stub of $n equal the stub; $config hold /etc/tent/node.json; $other differ; $unknown could not be read"
  fi
  row "User data after the build" "$res" "the scrub replaced the user data of every node that joined: no secret stays in the cloud"
}

# cl_ssh_reachable NAME: sets CL_IP to the public address of NAME, notes it for the exit trap, and waits up to 2 minutes
# for SSH; fails, and writes no row, when it does not answer.
cl_ssh_reachable() {
  CL_IP=$(cl_ip "$1")
  [ -n "$CL_IP" ] || return 1
  CL_PUBS="$CL_PUBS $CL_IP"
  wait_ssh "$CL_IP" 120
}

# cl_ssh_unknown_text NAME: why a check on NAME did not run: no instance list, no public address, or SSH.
cl_ssh_unknown_text() {
  if [ -n "$CL_IP" ]; then ssh_unknown "$CL_IP"; return 0; fi
  if cl_listed; then printf 'unknown: the Vultr API has no public address for %s' "$1"; else printf 'unknown: no instance list'; fi
}

# cl_ssh_unknown_rows NAME TITLE...: the rows of TITLE on NAME as unknown, when SSH did not answer.
cl_ssh_unknown_rows() {
  local name="$1" t res
  shift
  res=$(cl_ssh_unknown_text "$name")
  for t in "$@"; do row "$t on $name" "$res" "no check ran"; done
}

# cl_client_rows: what a client holds after the build: status.json, the join file with the three servers, the unit of
# the join that ran, and the intro token's size.
cl_client_rows() {
  local name="$CL_NAME-workers-0" ip out res phases runs failed rewrites n size mode owner
  if ! cl_ssh_reachable "$name"; then
    cl_ssh_unknown_rows "$name" "status.json" "05-join.hcl" "tent-node-join.service" "Intro token file"
    return 0
  fi
  ip="$CL_IP"
  ssh_x "$ip" 'cat /var/lib/tent/status.json 2>/dev/null; true' >"$WORK/cl-status.json" 2>/dev/null || : >"$WORK/cl-status.json"
  phases=$(jq -r '[.phases[]? | "\(.name) \(.status)"] | join(", ")' "$WORK/cl-status.json" 2>/dev/null || true)
  if [ -n "$phases" ] && [ "$(jq -r '[.phases[].name] | join(" ")' "$WORK/cl-status.json")" = "$TN_PHASES" ] &&
    [ "$(jq -r '[.phases[].status] | all(. == "done" or . == "unchanged")' "$WORK/cl-status.json")" = true ]; then
    res="as expected: $phases"
  else
    res="UNEXPECTED: ${phases:-no status.json}"
  fi
  row "status.json on $name" "$res" "tent-node ran every phase on a client"
  ssh_x "$ip" 'cat /etc/nomad.d/05-join.hcl 2>/dev/null; true' >"$WORK/cl-join.hcl" 2>/dev/null || : >"$WORK/cl-join.hcl"
  detail "/etc/nomad.d/05-join.hcl on $name" <"$WORK/cl-join.hcl"
  out=$(grep -o '"[^"]*:[0-9]*"' "$WORK/cl-join.hcl" | tr -d '"' | paste -sd ' ' - || true)
  n=$(printf '%s\n' "$out" | wc -w | tr -d ' ')
  if [ "$n" = "$CL_SERVERS" ]; then res="as expected: lists $n servers ($out)"; else res="UNEXPECTED: lists $n servers (${out:-none})"; fi
  row "05-join.hcl on $name" "$res" "a client joins the RPC port of every server, from the seed in NodeConfig"
  ssh_x "$ip" 'journalctl -b -u tent-node-join.service --no-pager -o short-monotonic | tail -n 60' 2>/dev/null | hide_url |
    detail "journalctl -u tent-node-join.service ($name)" || true
  out=$(ssh_x "$ip" 'journalctl -b -u tent-node-join.service --no-pager -o cat' 2>/dev/null || true)
  runs=$(printf '%s\n' "$out" | grep -c '^Finished' || true)
  failed=$(printf '%s\n' "$out" | grep -c 'Failed with result' || true)
  rewrites=$(printf '%s\n' "$out" | grep -Fc '05-join.hcl joins the servers that answered' || true)
  if [ "$runs" -ge 1 ] && [ "$failed" = 0 ]; then res="as expected"; else res="UNEXPECTED"; fi
  row "tent-node-join.service on $name" "$res: $runs run(s), $failed failed; $rewrites line(s) say that the join file was rewritten" \
    "the first peers call between two machines: a client asks a server's agent for the peers"
  out=$(ssh_x "$ip" "stat -c '%s %a %U:%G' $CL_INTRO_TOKEN 2>/dev/null; true" 2>/dev/null || true)
  read -r size mode owner <<<"$out"
  if [ -z "$size" ]; then
    res="UNEXPECTED: no file $CL_INTRO_TOKEN"
  elif [ "$size" -gt 2048 ]; then
    res="UNEXPECTED: $size bytes, more than the 2048-byte stand-in of the plan's size check"
  else
    res="as expected: $size bytes, mode $mode $owner (the plan's size check assumes up to 2048)"
  fi
  row "Intro token file on $name" "$res" "a record: the real size of an intro token for this node name"
}

# cl_restart_script: the script on a server that restarts nomad.service, waits up to a minute for it to be active again,
# and prints how it went.
cl_restart_script() {
  cat <<'EOF'
systemctl restart nomad.service
rc=$?
i=0
while [ "$i" -lt 12 ] && [ "$(systemctl is-active nomad.service)" != active ]; do sleep 5; i=$((i + 1)); done
echo "rc|$rc"
echo "active|$(systemctl is-active nomad.service)"
echo "result|$(systemctl show -p Result --value nomad.service)"
echo "nrestarts|$(systemctl show -p NRestarts --value nomad.service)"
echo "waited|$((i * 5))"
journalctl -b _PID=1 UNIT=nomad.service -o cat --no-pager | grep 'Failed with result' | sed 's/^/journal|/'
true
EOF
}

# cl_server_rows: nomad.service on the first server after systemctl restart (the unit's result: a server's agent exits
# with 1 on SIGTERM), then that the servers vote again, asked from the second server.
cl_server_rows() {
  local name="$CL_NAME-servers-0" ip other out rc active result nrestarts waited journal res i t0 text=""
  local title="nomad.service after systemctl restart on $name"
  if ! cl_ssh_reachable "$name"; then
    row "$title" "$(cl_ssh_unknown_text "$name")" "no check ran"
    return 0
  fi
  ip="$CL_IP"
  other=$(cl_ip "$CL_NAME-servers-1")
  cl_restart_script >"$WORK/cl-restart.sh"
  out=$(ssh_x "$ip" 'sh -s' <"$WORK/cl-restart.sh" 2>/dev/null | hide_url || true)
  rc=$(tn_key rc "$out")
  active=$(tn_key active "$out")
  result=$(tn_key result "$out")
  nrestarts=$(tn_key nrestarts "$out")
  waited=$(tn_key waited "$out")
  journal=$(printf '%s\n' "$out" | grep -c '^journal|' || true)
  if [ -z "$out" ]; then
    res=$(ssh_unknown "$ip")
  elif [ "$rc" = 0 ] && [ "$active" = active ]; then
    res="as expected: restart exit 0, unit active after ${waited}s (Result $result, NRestarts $nrestarts); the journal has $journal 'Failed with result' line(s)"
  else
    res="UNEXPECTED: restart exit ${rc:-?}, unit ${active:-?} after ${waited:-?}s (Result ${result:-?}); the journal has $journal 'Failed with result' line(s)"
  fi
  row "$title" "$res" \
    "a server's agent exits with 1 on SIGTERM (leave_on_terminate is off): a restart works, and the journal says how the stop ended"
  printf '%s\n' "$out" | detail "nomad.service restart on $name (rc, state, journal)" || true
  [ -n "$out" ] || return 0
  cl_need_api "Server voters after the restart" || return 0
  t0=$(now)
  for i in $(seq 1 36); do
    cl_nm "${other:-$ip}" GET /v1/operator/autopilot/health
    text=""
    if cl_health_answered; then
      text=$(cl_health_text)
      [ "$text" != "healthy, $CL_SERVERS voters" ] || break
    fi
    sleep 5
  done
  if [ "$text" = "healthy, $CL_SERVERS voters" ]; then
    res="as expected: $text after $(($(now) - t0))s"
  else
    res="UNEXPECTED: not healthy with $CL_SERVERS voters within 180s (last answer: HTTP $CL_NM_STATUS${text:+, $text})"
  fi
  row "Server voters after the restart" "$res" "a stopped server stays a Raft peer, and the restarted one votes again"
}

# cl_boot_id IP: the boot id of the machine at IP, or nothing.
cl_boot_id() { ssh_x "$1" 'cat /proc/sys/kernel/random/boot_id' 2>/dev/null || true; }

# cl_node_state NAME: the Nomad nodes of that name in the last /v1/nodes answer, one "ADDRESS STATUS ELIGIBILITY" each,
# joined with commas; empty when none is listed.
cl_node_state() {
  jq -r --arg n "$1" '[.[]? | select(.Name == $n) | "\(.Address) \(.Status) \(.SchedulingEligibility)"] | join(", ")' \
    "$WORK/nm-body.json" 2>/dev/null || true
}

# cl_node_ready NAME [ADDRESS]: true when the last /v1/nodes answer lists a ready and eligible node of that name, at
# ADDRESS when it is given.
cl_node_ready() {
  [ "$(jq -r --arg n "$1" --arg a "${2:-}" '[.[]? | select(.Name == $n and .Status == "ready" and
    .SchedulingEligibility == "eligible" and ($a == "" or .Address == $a))] | length' "$WORK/nm-body.json" 2>/dev/null || echo 0)" -gt 0 ]
}

# cl_reboot_row: reboots a client over SSH after the scrub. The node must come back ready and eligible in Nomad, keep
# /etc/tent/node.json, and cloud-init must end done, exit 0, with no recoverable error: the scrubbed user data must not
# make a later boot degraded.
cl_reboot_row() {
  local name="$CL_NAME-workers-0" title ip before after sum_before sum_after t0 deadline back="" i res bad="" ci errs
  local nomad_s="" out ext code warn
  title="Reboot of $name after the scrub"
  if ! cl_ssh_reachable "$name"; then
    row "$title" "$(cl_ssh_unknown_text "$name")" "no check ran"
    return 0
  fi
  ip="$CL_IP"
  before=$(cl_boot_id "$ip")
  sum_before=$({ ssh_x "$ip" 'sha256sum /etc/tent/node.json' 2>/dev/null || true; } | awk '{print $1}')
  if [ -z "$before" ] || [ -z "$sum_before" ]; then
    row "$title" "unknown: could not read the boot id and the sha256 of /etc/tent/node.json before the reboot" "no check ran"
    return 0
  fi
  ssh_x "$ip" 'systemctl reboot' >/dev/null 2>&1 || true
  ssh_close "$ip"
  t0=$(now)
  deadline=$((t0 + READY_TIMEOUT))
  while [ "$(now)" -lt "$deadline" ]; do
    sleep "$POLL_INTERVAL"
    port_open "$ip" || continue
    ssh_reset "$ip"
    after=$(cl_boot_id "$ip")
    if [ -n "$after" ] && [ "$after" != "$before" ]; then
      back="$(($(now) - t0))"
      break
    fi
    ssh_close "$ip" # the old boot still answered
  done
  if [ -z "$back" ]; then
    row "$title" "UNEXPECTED: SSH did not answer on a new boot within ${READY_TIMEOUT}s" "the scrubbed user data must not stop a node from booting"
    return 0
  fi
  sum_after=$({ ssh_x "$ip" 'sha256sum /etc/tent/node.json' 2>/dev/null || true; } | awk '{print $1}')
  [ "$sum_after" = "$sum_before" ] || bad="the sha256 of /etc/tent/node.json is ${sum_after:-missing} after the reboot, was $sum_before"
  out=$(ssh_x "$ip" 'timeout 300 cloud-init status --wait --long; echo "exit $?"' 2>/dev/null || true)
  # --wait prints a dot for each wait before the status, on the same line: "..status: done".
  ci=$(printf '%s\n' "$out" | sed -n 's/^\.*status: //p' | head -n 1)
  errs=$(printf '%s\n' "$out" | sed -n 's/^errors: //p' | head -n 1)
  ext=$(printf '%s\n' "$out" | sed -n 's/^extended_status: //p' | head -n 1)
  code=$(printf '%s\n' "$out" | sed -n 's/^exit //p' | tail -n 1)
  warn=$(printf '%s\n' "$out" | awk '
    /^recoverable_errors:/ { f = 1; next }
    f && /^[[:space:]]+- / { sub(/^[[:space:]]+- /, ""); print; exit }')
  if [ "$ci" != "done" ] || [ "$errs" != "[]" ]; then
    bad="$bad${bad:+; }cloud-init status ${ci:-?} (errors ${errs:-?})"
  elif [ "${code:-?}" != 0 ] || [ -n "$warn" ] ||
    { [ -n "$ext" ] && [ "$ext" != "done" ]; }; then
    bad="$bad${bad:+; }cloud-init is degraded (extended status ${ext:-?}, exit ${code:-?})${warn:+: $warn}"
  fi
  if cl_need_api "Nodes after the reboot of $name"; then
    for i in $(seq 1 60); do
      cl_nm_get "$CL_SRV_IP" /v1/nodes
      if [ "$CL_NM_STATUS" = 200 ] && cl_node_ready "$name"; then nomad_s="$(($(now) - t0))"; break; fi
      sleep 5
    done
    if [ -z "$nomad_s" ]; then
      bad="$bad${bad:+; }Nomad does not list $name ready and eligible within 300s after cloud-init ended ($(cl_node_state "$name"))"
    fi
  else
    bad="$bad${bad:+; }Nomad was not asked: no operator files"
  fi
  if [ -n "$bad" ]; then
    res="UNEXPECTED: $bad"
  else
    res="as expected: SSH answered on a new boot after ${back}s; Nomad lists the node ready and eligible ${nomad_s}s"
    res="$res after the reboot; /etc/tent/node.json has the sha256 it had"
    res="$res; cloud-init status done, exit 0, no recoverable error"
  fi
  row "$title" "$res" "tent-node up runs again on a node whose user data is the stub, and the node rejoins without a new intro token"
  printf '%s\n' "$out" | detail "cloud-init status --wait --long ($name, after the reboot)"
}

# cl_instance_count: how many instances carry the cluster's tag, or "?".
cl_instance_count() {
  cl_instances || { echo '?'; return 0; }
  jq -r '.instances | length' "$WORK/cl-instances.json"
}

# cl_unknown_rows REASON TITLE...: one unknown row for each TITLE.
cl_unknown_rows() {
  local why="$1" t
  shift
  for t in "$@"; do row "$t" "unknown: $why" "no check ran"; done
}

# cl_one_client_specs IN OUT: writes IN to OUT with the size of the group of clients set to 1.
cl_one_client_specs() {
  awk '
    function flush(   i, l) {
      for (i = 1; i <= n; i++) {
        l = doc[i]
        if (client && l ~ /^  size: [0-9]+$/) l = "  size: 1"
        print l
      }
      n = 0
      client = 0
    }
    /^---$/ { flush(); print; next }
    { doc[++n] = $0; if ($0 == "  role: client") client = 1 }
    END { flush() }' "$1" >"$2"
}

# cl_guard_run LABEL TITLE ARGS...: tent update cluster with ARGS, which must fail with exit 1 and the error that names
# the surplus client, and leave all instances in place.
cl_guard_run() {
  local label="$1" title="$2" want=$((CL_SERVERS + CL_WORKERS)) text n line res
  text="update would delete a node that joined Nomad: $CL_NAME-workers-1"
  shift 2
  cl_run "$label" update cluster "$CL_NAME" "$@"
  n=$(cl_instance_count)
  line=$({ grep -Fh -- "$text" "$CL_ERR" "$CL_OUT" || true; } | head -n 1 | sed 's/^[0-9]* //' | oneline 200)
  if [ "$CL_RC" != 1 ]; then
    res="UNEXPECTED: exit $CL_RC, want 1: ${line:-$(cl_last_line)}"
  elif [ -z "$line" ]; then
    res="UNEXPECTED: exit 1 without the line '$text': $(cl_last_line)"
  else
    res="as expected: exit 1 in ${CL_SECS}s: $line"
  fi
  if [ "$n" = "$want" ]; then
    res="$res; the API lists $n instances"
  else
    case "$res" in "as expected"*) res="UNEXPECTED${res#as expected}" ;; esac
    res="$res; the API lists $n instances, want $want"
  fi
  row "$title" "$res" "until M3.6 update cannot drain: it refuses to delete a joined node, with or without --yes"
  cl_out_detail "$title"
}

# cl_guard_rows: with one client in the specs, update refuses to delete the other one; with the saved specs back it
# plans nothing. The surplus client is <cluster>-workers-1: planNodes deletes the newest machines of a group that is too
# big, and tent creates the clients one after the other, so workers-1 is younger than workers-0.
cl_guard_rows() {
  local t_save="Specs saved with tent get" t_one="Specs with one client in the state store"
  local t_plan="tent update cluster (one client in the specs)" t_yes="tent update cluster --yes (one client in the specs)"
  local t_back="Specs restored" t_exit="tent update cluster --exit-code (specs restored)" res docs
  CL_SPECS="$WORK/cl-specs.yaml"
  cl_run get get "$CL_NAME"
  docs=$(($(grep -c '^---$' "$CL_OUT" || true) + 1))
  if [ "$CL_RC" != 0 ]; then
    row "$t_save" "FAILED: exit $CL_RC: $(cl_last_line)" "the guard check changes the specs and puts these back"
    cl_unknown_rows "the specs were not saved" "$t_one" "$t_plan" "$t_yes" "$t_back" "$t_exit"
    return 0
  fi
  cp "$CL_OUT" "$CL_SPECS"
  if [ "$docs" = $((1 + 2)) ]; then
    res="as expected: $docs documents"
  else
    res="UNEXPECTED: $docs documents, want 3 (the cluster and two node groups)"
  fi
  row "$t_save" "$res" "the guard check changes the specs and puts these back"
  cl_one_client_specs "$CL_SPECS" "$WORK/cl-specs-one.yaml"
  if cmp -s "$CL_SPECS" "$WORK/cl-specs-one.yaml"; then
    row "$t_one" "UNEXPECTED: the specs have no client group with a size to change" "no check ran"
    cl_unknown_rows "no specs with one client" "$t_plan" "$t_yes" "$t_back" "$t_exit"
    return 0
  fi
  CL_SPECS_CHANGED=1
  cl_run specs-one replace -f "$WORK/cl-specs-one.yaml"
  if [ "$CL_RC" != 0 ]; then
    CL_SPECS_CHANGED=0
    row "$t_one" "FAILED: exit $CL_RC: $(cl_last_line)" "tent replace -f changes the specs in the state store only"
    cl_unknown_rows "the specs did not change" "$t_plan" "$t_yes" "$t_back" "$t_exit"
    return 0
  fi
  row "$t_one" "as expected: exit 0 in ${CL_SECS}s" "tent replace -f changes the specs in the state store only"
  cl_guard_run guard-plan "$t_plan"
  cl_guard_run guard-yes "$t_yes" --yes
  cl_run specs-back replace -f "$CL_SPECS"
  if [ "$CL_RC" != 0 ]; then
    row "$t_back" "FAILED: exit $CL_RC: $(cl_last_line)" "the saved specs go back into the state store"
    cl_unknown_rows "the saved specs were not put back" "$t_exit"
    return 0
  fi
  CL_SPECS_CHANGED=0
  row "$t_back" "as expected: exit 0 in ${CL_SECS}s" "the saved specs go back into the state store"
  cl_exit_code_row guard-exit "$t_exit" "the refused update changed nothing: the saved specs plan nothing"
}

# --unregistered: a client that never registers. The pieces below run in order, and a piece that fails stops the rest.

# The age at which tent replaces a client that has not joined: the intro token's 30 minutes (nomadops.MaxIntroTTL) and
# a minute of leeway that Nomad gives a token after it expired, plus a minute so that no clock difference decides.
readonly CL_UNREG_AGE=1920

# cl_unreg_title STEP: the title of the row of a step.
cl_unreg_title() {
  local name="$CL_NAME-workers-1"
  case "$1" in
    stop) printf 'Nomad stopped on %s' "$name" ;;
    purge) printf 'Purge of %s in Nomad' "$name" ;;
    tag) printf 'Tag tent/joined taken off %s' "$name" ;;
    wait) printf 'Wait for %s to be 32 minutes old' "$name" ;;
    before) printf 'nomad.service on %s before the plan' "$name" ;;
    plan) printf 'tent update cluster (client never registered)' ;;
    apply) printf 'tent update cluster --yes (client never registered)' ;;
    replacement) printf 'Replacement of %s' "$name" ;;
    node) printf 'Nomad node of %s after the replacement' "$name" ;;
    after) printf 'tent update cluster --exit-code (after the replacement)' ;;
  esac
}

# cl_unit_script VERB: the script on the client that runs systemctl VERB on nomad.service and prints how it went.
cl_unit_script() {
  cat <<EOF
systemctl $1 nomad.service
echo "rc|\$?"
echo "active|\$(systemctl is-active nomad.service)"
echo "enabled|\$(systemctl is-enabled nomad.service 2>&1)"
true
EOF
}

# cl_unreg_stop: stops Nomad on the client. Nothing starts it again before tent replaces the machine: tent-node starts
# nomad.service only at boot (tent-node.service runs up), the refresh-join timer never restarts it, nomad.service is not
# enabled for boot, and its Restart=on-failure does not apply to a unit that systemctl stopped. The unit is not masked:
# systemctl mask refuses a unit whose file is in /etc/systemd/system, and nothing starts the unit again.
cl_unreg_stop() {
  local name="$CL_NAME-workers-1" title out rc active
  title=$(cl_unreg_title stop)
  CL_UNREG_ID=$(cl_id "$name")
  if [ -z "$CL_UNREG_ID" ]; then
    row "$title" "unknown: the cluster has no instance $name" "no check ran"
    return 1
  fi
  if ! cl_ssh_reachable "$name"; then
    row "$title" "$(cl_ssh_unknown_text "$name")" "no check ran"
    return 1
  fi
  CL_STOPPED="$name"
  cl_unit_script stop >"$WORK/cl-stop.sh"
  out=$(ssh_x "$CL_IP" 'sh -s' <"$WORK/cl-stop.sh" 2>/dev/null || true)
  rc=$(tn_key rc "$out")
  active=$(tn_key active "$out")
  printf '%s\n' "$out" | detail "systemctl stop nomad.service on $name"
  case "$rc/$active" in
    0/inactive | 0/failed)
      row "$title" "as expected: systemctl stop exit 0, nomad.service $active" \
        "a client whose Nomad is stopped registers no more: to tent it is a client that never registered"
      ;;
    *)
      row "$title" "UNEXPECTED: stop exit ${rc:-?}, unit ${active:-?}" "no check ran"
      return 1
      ;;
  esac
}

# cl_unreg_purge: removes the client's node from Nomad through the API. A client with leave_on_terminate may have left
# by itself at the stop, or between the list and the purge (Nomad answers a purge of a node that is gone with HTTP 500
# "node not found"). So the row is judged by the list after the purge: no node of the name is listed.
cl_unreg_purge() {
  local name="$CL_NAME-workers-1" title ids nid answers="" left
  title=$(cl_unreg_title purge)
  cl_need_api "$title" || return 1
  cl_nm_get "$CL_SRV_IP" /v1/nodes
  if [ "$CL_NM_STATUS" != 200 ]; then
    row "$title" "UNEXPECTED: $(cl_nm_error /v1/nodes)" "no check ran"
    return 1
  fi
  ids=$(jq -r --arg n "$name" '.[]? | select(.Name == $n) | .ID' "$WORK/nm-body.json")
  for nid in $ids; do
    cl_nm "$CL_SRV_IP" POST "/v1/node/$nid/purge"
    answers="$answers${answers:+; }the purge of $nid answered HTTP $CL_NM_STATUS"
    if [ "$CL_NM_STATUS" != 200 ]; then answers="$answers: $(oneline 100 <"$WORK/nm-body.json")"; fi
  done
  cl_nm_get "$CL_SRV_IP" /v1/nodes
  if [ "$CL_NM_STATUS" != 200 ]; then
    row "$title" "UNEXPECTED: the list after the purge failed: $(cl_nm_error /v1/nodes)${answers:+; $answers}" \
      "the node of a client that never registered is not in Nomad"
    return 1
  fi
  left=$(cl_node_state "$name")
  if [ -n "$left" ]; then
    row "$title" "UNEXPECTED: the node is still listed: $left${answers:+; $answers}" \
      "the node of a client that never registered is not in Nomad"
    return 1
  fi
  if [ -z "$ids" ]; then
    row "$title" "as expected: no node of that name was listed after the stop (the client left by itself)" \
      "the node of a client that never registered is not in Nomad"
  else
    row "$title" "as expected: no node of that name is listed after the purge; $answers" \
      "the node of a client that never registered is not in Nomad"
  fi
}

# cl_unreg_tag: takes tent/joined=true off the instance's tags: a PATCH with the whole remaining list.
cl_unreg_tag() {
  local title rest now_tags want
  title=$(cl_unreg_title tag)
  api GET "/instances/$CL_UNREG_ID"
  if ! api_ok; then
    row "$title" "UNEXPECTED: GET answered $(answer)" "no check ran"
    return 1
  fi
  rest=$(jq -c '[.instance.tags[]? | select(. != "tent/joined=true")]' "$API_BODY")
  if ! jq -e --arg c "tent/cluster=$CL_NAME" 'any(.[]; . == $c)' <<<"$rest" >/dev/null; then
    row "$title" "UNEXPECTED: the instance's tags do not hold tent/cluster=$CL_NAME: not patched" "no check ran"
    return 1
  fi
  jq -n --argjson t "$rest" '{tags: $t}' >"$WORK/cl-tags.json"
  api PATCH "/instances/$CL_UNREG_ID" "$WORK/cl-tags.json"
  if ! api_ok; then
    row "$title" "FAILED: PATCH answered $(answer)" "the check needs a machine that tent sees as one that has not joined"
    return 1
  fi
  api GET "/instances/$CL_UNREG_ID"
  now_tags=$(jq -c '.instance.tags | sort' "$API_BODY" 2>/dev/null || echo '?')
  want=$(jq -c 'sort' <<<"$rest")
  if [ "$now_tags" != "$want" ]; then
    row "$title" "UNEXPECTED: the tags are $now_tags after the PATCH, want $want" "no check ran"
    return 1
  fi
  row "$title" "as expected: PATCH with the remaining $(jq 'length' <<<"$rest") tags; the instance has $now_tags" \
    "the check needs a machine that tent sees as one that has not joined"
}

# cl_created_epoch ID: the epoch second of the instance's creation date, or nothing.
cl_created_epoch() {
  api GET "/instances/$1"
  api_ok || return 0
  jq -r '(.instance.date_created // "") | sub("\\+00:00$"; "Z") | sub("\\.[0-9]+Z$"; "Z") | try fromdateiso8601 catch empty' \
    "$API_BODY" 2>/dev/null || true
}

# cl_unreg_wait: waits until the instance is 32 minutes old, which tent judges to be older than its intro token.
cl_unreg_wait() {
  local title created target left t0
  title=$(cl_unreg_title wait)
  created=$(cl_created_epoch "$CL_UNREG_ID")
  if [ -z "$created" ]; then
    row "$title" "unknown: the Vultr API gives no creation time for the instance" "no check ran"
    return 1
  fi
  target=$((created + CL_UNREG_AGE))
  t0=$(now)
  left=$((target - t0))
  if [ "$left" -gt 0 ]; then log "waiting ${left}s until $CL_NAME-workers-1 is 32 minutes old"; fi
  while [ "$(now)" -lt "$target" ]; do sleep 30; done
  row "$title" "as expected: waited $(($(now) - t0))s; the instance is $(($(now) - created))s old (tent replaces a client that has not joined after 31 minutes)" \
    "a client that registers within the lifetime of its intro token is waited for; after it, replaced"
}

# cl_unreg_before: Nomad must still be stopped on the client: nothing started it again.
cl_unreg_before() {
  local name="$CL_NAME-workers-1" title out
  title=$(cl_unreg_title before)
  if ! cl_ssh_reachable "$name"; then
    row "$title" "$(cl_ssh_unknown_text "$name")" "no check ran"
    return 1
  fi
  out=$(ssh_x "$CL_IP" 'systemctl is-active nomad.service || true' 2>/dev/null || true)
  case "$out" in
    inactive | failed) row "$title" "as expected: $out" "nothing started Nomad again before tent judged the client" ;;
    *)
      row "$title" "UNEXPECTED: ${out:-no answer}: something started Nomad again" "the client registers again, so tent waits for it"
      return 1
      ;;
  esac
}

# cl_private_ip ID: the first private address of instance ID in its VPC, or nothing.
cl_private_ip() {
  api GET "/instances/$1/vpcs"
  jq -r '[.vpcs[]? | select((.ip_address // "") != "" and .ip_address != "0.0.0.0")][0].ip_address // empty' \
    "$API_BODY" 2>/dev/null || true
}

# cl_unreg_plan: the plan deletes the client as not registered and creates it again.
cl_unreg_plan() {
  local name="$CL_NAME-workers-1" title res
  title=$(cl_unreg_title plan)
  CL_OLD_IP=$(cl_private_ip "$CL_UNREG_ID")
  cl_run unreg-plan update cluster "$CL_NAME"
  cl_out_detail "$title"
  if [ "$CL_RC" != 0 ]; then
    row "$title" "FAILED: exit $CL_RC: $(cl_last_line)" "a client that did not register within its token's lifetime is replaced"
    return 1
  fi
  if grep -Fq -- "- node $name (ID $CL_UNREG_ID, not registered)" "$CL_OUT" && grep -Fq -- "+ node $name (client" "$CL_OUT"; then
    row "$title" "as expected: exit 0 in ${CL_SECS}s; the plan deletes $name (ID $CL_UNREG_ID, not registered) and creates it" \
      "a client that did not register within its token's lifetime is replaced"
    return 0
  fi
  res="UNEXPECTED: exit 0 in ${CL_SECS}s, but the plan has no 'not registered' delete and create of $name: $(oneline 200 <"$CL_OUT")"
  row "$title" "$res" "a client that did not register within its token's lifetime is replaced"
  return 1
}

# cl_line_no TEXT: the number of the first line of the last cl_run's stderr that holds TEXT, or nothing.
cl_line_no() { awk -v p="$1" 'index($0, p) { print NR; exit }' "$CL_ERR" 2>/dev/null || true; }

# cl_unreg_apply: update --yes deletes the machine, creates another of the name, waits for its registration and scrubs
# it.
cl_unreg_apply() {
  local name="$CL_NAME-workers-1" title missing="" last=0 n t order=1 held build
  title=$(cl_unreg_title apply)
  cl_run unreg-yes update cluster "$CL_NAME" --yes
  cl_out_detail "$title"
  if [ "$CL_RC" != 0 ]; then
    row "$title" "FAILED: exit $CL_RC in ${CL_SECS}s: $(cl_last_line)" "the replacement of a client that never registered"
    return 1
  fi
  for t in "deleted node $name" "created node $name" "node $name registered" "scrubbed the user data of node $name"; do
    n=$(cl_line_no "$t")
    if [ -z "$n" ]; then
      missing="$missing${missing:+, }'$t'"
    else
      if [ "$n" -lt "$last" ]; then order=0; fi
      last="$n"
    fi
  done
  if [ -n "$missing" ]; then
    row "$title" "UNEXPECTED: exit 0 in ${CL_SECS}s, but these progress lines are missing: $missing" "the replacement of a client that never registered"
    return 1
  fi
  if [ "$order" = 0 ]; then
    row "$title" "UNEXPECTED: exit 0 in ${CL_SECS}s, but the progress lines are out of order (delete, create, registered, scrubbed)" \
      "the replacement of a client that never registered"
    return 1
  fi
  CL_STOPPED=""
  held=$(cl_span_secs "deleted node $name" "created node $name")
  build="a client's create took ${CL_BUILD_CREATE_SECS}s in the build"
  [ -n "$CL_BUILD_CREATE_SECS" ] || build="how long a client's create took in the build is unknown"
  row "$title" "as expected: exit 0 in ${CL_SECS}s; deleted, created, registered and scrubbed $name; $name was \
created ${held}s after its delete; $build" \
    "the replacement of a client that never registered; the two times show how long the instance limit held the create"
}

# cl_unreg_replacement: the new instance has another id, the label, the stub and a private address.
cl_unreg_replacement() {
  local name="$CL_NAME-workers-1" title newid n kind joined same bad=""
  title=$(cl_unreg_title replacement)
  if ! cl_instances; then
    row "$title" "UNEXPECTED: $(answer)" "no check ran"
    return 1
  fi
  n=$(jq -r '.instances | length' "$WORK/cl-instances.json")
  newid=$(cl_id "$name")
  if [ -z "$newid" ]; then
    row "$title" "UNEXPECTED: no instance of $name is listed: $n instances" "no check ran"
    return 1
  fi
  if [ "$newid" = "$CL_UNREG_ID" ]; then
    row "$title" "UNEXPECTED: the instance of $name still has the old id $newid" "no check ran"
    return 1
  fi
  [ "$n" = $((CL_SERVERS + CL_WORKERS)) ] || bad="$n instances, want $((CL_SERVERS + CL_WORKERS))"
  joined=$(jq -r --arg i "$newid" '[.instances[] | select(.id == $i) | .tags[]? | select(. == "tent/joined=true")] | length' \
    "$WORK/cl-instances.json")
  [ "$joined" = 1 ] || bad="$bad${bad:+; }tent/joined=true is missing"
  kind=$(cl_ud_kind "$newid")
  [ "$kind" = stub ] || bad="$bad${bad:+; }the user data is not the stub ($kind)"
  CL_NEW_IP=$(cl_private_ip "$newid")
  [ -n "$CL_NEW_IP" ] || bad="$bad${bad:+; }the instance has no private address"
  if [ "$CL_NEW_IP" = "$CL_OLD_IP" ]; then same=same; else same=different; fi
  if [ -n "$bad" ]; then
    row "$title" "UNEXPECTED: $bad" "the new machine took the node's name and joined"
    [ -n "$CL_NEW_IP" ] || return 1
    return 0
  fi
  row "$title" "as expected: new instance $newid (old $CL_UNREG_ID), tent/joined=true, the stub; private address $CL_NEW_IP (the old one was ${CL_OLD_IP:-unknown}: $same)" \
    "the new machine took the node's name and joined; a record: whether the private address is reused"
}

# cl_unreg_node: Nomad lists a ready node of the name at the new instance's private address.
cl_unreg_node() {
  local name="$CL_NAME-workers-1" title i state=""
  title=$(cl_unreg_title node)
  cl_need_api "$title" || return 1
  if [ -z "$CL_NEW_IP" ]; then
    row "$title" "unknown: the new instance's private address is not known" "no check ran"
    return 1
  fi
  for i in $(seq 1 12); do
    cl_nm_get "$CL_SRV_IP" /v1/nodes
    if [ "$CL_NM_STATUS" = 200 ] && cl_node_ready "$name" "$CL_NEW_IP"; then
      row "$title" "as expected: ready and eligible at $CL_NEW_IP" "tent told the new machine's node by its name and address"
      return 0
    fi
    sleep 5
  done
  state=$(cl_node_state "$name")
  row "$title" "UNEXPECTED: no ready and eligible node of that name at $CL_NEW_IP; Nomad lists: ${state:-none}" \
    "tent told the new machine's node by its name and address"
  return 1
}

# cl_unreg_after: the cluster is converged again.
cl_unreg_after() {
  cl_exit_code_row unreg-after "$(cl_unreg_title after)" "the replacement converged: the next plan has no change"
}

# cl_unregistered_rows: stop Nomad on <name>-workers-1, purge its node, take the joined tag off, wait until the instance
# is 32 minutes old, and check that update replaces the client.
cl_unregistered_rows() {
  local step ok=1
  for step in stop purge tag wait before plan apply replacement node after; do
    if [ "$ok" = 1 ]; then
      "cl_unreg_$step" || ok=0
    else
      row "$(cl_unreg_title "$step")" "unknown: an earlier step of this check did not go on" "no check ran"
    fi
  done
}

# cl_redact FILE: replaces every secret line that cl_collect_secrets noted, wherever it stands in FILE, with [hidden].
cl_redact() {
  awk 'NR == FNR { pat[NR] = $0; n = NR; next }
    {
      for (i = 1; i <= n; i++) {
        while ((k = index($0, pat[i])) > 0) $0 = substr($0, 1, k - 1) "[hidden]" substr($0, k + length(pat[i]))
      }
      print
    }' "$WORK/secret-patterns.txt" "$1" >"$1.redacted" && mv "$1.redacted" "$1"
}

# cl_secrets_row [WHEN]: looks in everything the run recorded for the long lines of the cluster's secrets, and says only
# which files hold one. A secret that is found is hidden in the report at once. WHEN is added to the row's title.
cl_secrets_row() {
  local title="Secrets in the output${1:+ ($1)}" files="$SUMMARY $DETAILS $WORK/curl-errors.log" f found res
  CL_SEARCHED=1
  for f in "$WORK"/cl-*.out "$WORK"/cl-*.err; do
    if [ -e "$f" ]; then files="$files $f"; fi
  done
  if [ "$CL_SECRETS" = 0 ] || [ ! -s "$WORK/secret-patterns.txt" ]; then
    row "$title" "unknown: no secret file was read" "tent and the script keep secrets out of what a person reads"
    return 0
  fi
  # shellcheck disable=SC2086 # the names have no spaces
  found=$({ grep -Fl -f "$WORK/secret-patterns.txt" $files 2>/dev/null || true; } | while IFS= read -r f; do basename "$f"; done | paste -sd , -)
  if [ -z "$found" ]; then
    res="as expected: none of the $CL_SECRETS secrets read ($CL_SECRET_NAMES) appears in tent's output, the details or this table"
  else
    cl_redact "$SUMMARY"
    cl_redact "$DETAILS"
    res="FAILED: a secret appears in $found (hidden in this report)"
  fi
  row "$title" "$res" "tent and the script keep secrets out of what a person reads"
}

# cl_found: sets CL_LEFT_INST, CL_LEFT_VPC, CL_LEFT_FW and CL_LEFT_KEY to the ids of what the Vultr API still lists for
# the cluster: instances by tag, the others by the marker in their description or name. Fails when a list fails.
cl_found() {
  local m="cluster=$CL_NAME;"
  api GET "/instances?per_page=500&tag=$(urlencode "tent/cluster=$CL_NAME")"
  api_ok || return 1
  CL_LEFT_INST=$(jq -r '.instances[]?.id' "$API_BODY" | paste -sd ' ' -)
  api GET "/vpcs?per_page=500"
  api_ok || return 1
  CL_LEFT_VPC=$(jq -r --arg m "$m" '.vpcs[]? | select((.description // "") | contains($m)) | .id' "$API_BODY" | paste -sd ' ' -)
  api GET "/firewalls?per_page=500"
  api_ok || return 1
  CL_LEFT_FW=$(jq -r --arg m "$m" '.firewall_groups[]? | select((.description // "") | contains($m)) | .id' "$API_BODY" | paste -sd ' ' -)
  api GET "/ssh-keys?per_page=500"
  api_ok || return 1
  CL_LEFT_KEY=$(jq -r --arg m "$m" '.ssh_keys[]? | select((.name // "") | contains($m)) | .id' "$API_BODY" | paste -sd ' ' -)
}

cl_count() { printf '%s\n' "$1" | wc -w | tr -d ' '; } # cl_count IDS: how many ids

cl_left_text() {
  printf '%s instance(s), %s VPC(s), %s firewall group(s), %s SSH key(s)' "$(cl_count "$CL_LEFT_INST")" \
    "$(cl_count "$CL_LEFT_VPC")" "$(cl_count "$CL_LEFT_FW")" "$(cl_count "$CL_LEFT_KEY")"
}

# cl_delete [WHO]: tent delete cluster --yes; writes its row, and notes that the cluster is gone when it exits 0.
cl_delete() {
  local res
  cl_run delete delete cluster "$CL_NAME" --yes
  if [ "$CL_RC" = 0 ]; then
    CL_DELETED=1
    res="as expected: exit 0 in ${CL_SECS}s${1:+ ($1)}"
  else
    res="FAILED: exit $CL_RC in ${CL_SECS}s${1:+ ($1)}: $(cl_last_line)"
  fi
  row "tent delete cluster --yes" "$res" "delete finds the cluster's objects by their markers, deletes the nodes first and the state last"
  cl_out_detail "tent delete cluster --yes${1:+ ($1)}"
}

cl_after_delete_rows() {
  local res files n
  if cl_found; then
    if [ -z "$CL_LEFT_INST$CL_LEFT_VPC$CL_LEFT_FW$CL_LEFT_KEY" ]; then
      res="as expected: no instance, VPC, firewall group or SSH key is left"
    else
      res="FAILED: $(cl_left_text) are left; the exit trap removes them by tag"
    fi
  else
    res="UNEXPECTED: $(answer)"
  fi
  row "Cluster's objects in the Vultr API after the delete" "$res" "delete leaves nothing that carries the cluster's tag or marker"
  # Names that start with .tent- belong to the store's backend: its lock files stay after a delete.
  files=$(cd "$CL_STATE" && find . -type f ! -path './.tent-*' | sed 's|^\./||' | sort)
  n=$(printf '%s' "$files" | grep -c . || true)
  if [ "$n" = 0 ]; then res="as expected: empty"; else res="UNEXPECTED: $n file(s): $(printf '%s\n' "$files" | paste -sd , - | cut -c1-200)"; fi
  row "State store after the delete" "$res" "delete removes the cluster's specs, secrets, lock and marks"
}

check_cluster() {
  local ip t res
  cl_create
  cl_progress_row
  if [ "$CL_RC" != 0 ]; then
    log "create failed: the exit trap deletes the cluster"
    cl_secrets_row
    return 0
  fi
  cl_instances_row
  ip=$(cl_ip "$CL_NAME-servers-0")
  if [ -z "$ip" ]; then
    res="unknown: no instance list"
    if cl_listed; then res="unknown: the Vultr API has no public address for $CL_NAME-servers-0"; fi
    for t in "tent export nomad" "Nomad servers (agent/members)" "Autopilot health" "Nomad clients (nodes)" "The job $TN_JOB" \
      "Stop of the job $TN_JOB" "$CL_CLI_TITLE" "tent validate cluster" "tent ui" "$(cl_down_title stop)" \
      "$(cl_down_title down)" "$(cl_down_title back)"; do
      row "$t" "$res" "no Nomad check ran"
    done
  else
    CL_SRV_IP="$ip"
    cl_export "$ip"
    cl_members_row "$ip"
    cl_health_row "$ip"
    cl_nodes_row "$ip"
    cl_job "$ip"
    cl_nomad_cli_row
    cl_validate_row
    cl_ui_row
    cl_down_rows
  fi
  cl_update_rows
  cl_tags_row
  cl_joined_row
  cl_userdata_row
  cl_client_rows
  cl_server_rows
  cl_reboot_row
  cl_guard_rows
  if [ "$UNREG" != 1 ]; then
    row "Client that never registers (--unregistered)" "skipped: --unregistered not given" \
      "the check takes about 30 minutes more: it waits until an instance is 32 minutes old"
  elif [ "$CL_SPECS_CHANGED" = 1 ]; then
    row "Client that never registers (--unregistered)" "skipped: the saved specs were not put back" \
      "the check needs the specs as tent built them: with one client in the specs the plan would not create the client again"
  else
    cl_unregistered_rows
  fi
  if [ "$KEEP" = 1 ]; then
    for t in "tent delete cluster --yes" "Cluster's objects in the Vultr API after the delete" "State store after the delete"; do
      row "$t" "skipped: --keep" "--keep leaves the cluster in place"
    done
  else
    cl_delete
    cl_after_delete_rows
  fi
  cl_secrets_row
}

# cl_remove_leftovers: removes by tag and marker what the Vultr API still lists for the cluster after tent delete.
cl_remove_leftovers() {
  local id left t
  [ -n "$AUTH_CONF" ] || return 0
  cl_found || return 0
  [ -n "$CL_LEFT_INST$CL_LEFT_VPC$CL_LEFT_FW$CL_LEFT_KEY" ] || return 0
  left=$(cl_left_text)
  for id in $CL_LEFT_INST; do api DELETE "/instances/$id"; done
  t=$(($(now) + 300))
  while [ "$(now)" -lt "$t" ]; do
    cl_found || break
    [ -n "$CL_LEFT_INST" ] || break
    sleep 5
  done
  cl_found || true
  for id in $CL_LEFT_FW; do delete_retrying firewall "$id"; done
  for id in $CL_LEFT_VPC; do delete_retrying vpc "$id"; done
  for id in $CL_LEFT_KEY; do delete_retrying ssh-key "$id"; done
  row "Leftovers removed by tag" "removed: $left" "tent delete did not remove them: the exit trap did, by tag and marker"
}

# cl_cleanup: the cluster's part of the exit trap. With --keep the run never deleted the cluster and still does not: the
# secret search runs if the run did not reach its own, and the state store moves out of the temporary directory (when
# the move fails, the store stays there and so does the directory). Otherwise tent delete runs when the run did not
# delete the cluster, the secret search runs over what that delete printed, and then whatever the Vultr API still lists
# for the cluster goes by tag and marker.
cl_cleanup() {
  local kept="$OUT_DIR/cluster-$RUN-state"
  cl_ui_kill
  [ "$CL_STARTED" = 1 ] || return 0
  # A run stopped during create has read none: read what the store holds before anything deletes it.
  [ "$CL_SECRETS" != 0 ] || cl_collect_store_secrets
  if [ "$KEEP" = 1 ] && [ "$CL_SPECS_CHANGED" = 1 ]; then
    # The guard check changed the specs and the run stopped before it put them back: tent update would refuse the
    # kept cluster. Without --keep the delete below does not look at the specs.
    cl_run specs-back replace -f "$CL_SPECS"
    if [ "$CL_RC" = 0 ]; then
      CL_SPECS_CHANGED=0
    else
      cp "$CL_SPECS" "$OUT_DIR/cluster-$RUN-specs.yaml" || true
      log "--keep: could not put the saved specs back: tent replace -f $OUT_DIR/cluster-$RUN-specs.yaml"
    fi
  fi
  if [ "$KEEP" = 1 ] && [ -n "$CL_STOPPED" ]; then
    log "--keep: Nomad stopped on $CL_STOPPED and its tent/joined tag may be off: tent update cluster $CL_NAME --yes replaces it 31 minutes after its creation"
  fi
  if [ "$KEEP" = 1 ] && [ -n "$CL_DOWN" ]; then
    log "--keep: Nomad is stopped on $CL_DOWN: run systemctl start nomad.service on it; tent update does not replace a node that joined"
  fi
  if [ "$KEEP" = 1 ]; then
    [ "$CL_SEARCHED" = 1 ] || cl_secrets_row "at exit"
    if mv "$CL_STATE" "$kept"; then
      log "--keep: cluster $CL_NAME stays; its state store, with the cluster's secrets, is $kept"
    else
      kept="$CL_STATE"
      KEEP_WORK=1
      rm -rf "$AUTH_CONF" "$WORK/nomad-curl.conf" "$CL_OPDIR" "$WORK/secret-patterns.txt" "$SSH_KEY" "$SSH_KEY.pub" "$API_BODY" \
        "$WORK/nm-body.json" "$WORK/ui-body.json" "$WORK/ui-token"
      log "--keep: could not move the state store: it stays in $kept, and so does the directory $WORK"
      log "the directory holds no API key, token or private key any more: only the store has the cluster's secrets"
    fi
    log "finish an interrupted build with: $CL_TENT update cluster $CL_NAME --yes --state file://$kept"
    log "delete the cluster with: $CL_TENT delete cluster $CL_NAME --yes --state file://$kept"
    return 0
  fi
  if [ "$CL_DELETED" != 1 ]; then
    cl_delete "run by the exit trap"
    cl_secrets_row "after the exit trap's delete"
  fi
  cl_remove_leftovers
}

# ---------------------------------------------------------------------------------------------------------------
# Cleanup (EXIT trap): write the report, then delete everything this run created unless --keep.
# Records how long the API refuses to delete the firewall group and the VPC after the instances are gone
# (the retry loop `tent delete cluster` needs).

# delete_retrying TYPE ID: deletes a firewall group, VPC or SSH key, which the API refuses while an instance that is
# going away still uses it, and tries again every 5 s for up to 5 minutes. The row records how long that took.
delete_retrying() {
  local type="$1" id="$2" deadline t0 tries=0 first_err=""
  deadline=$(($(now) + 300))
  t0=$(now)
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
}

cleanup() {
  local rc=$? type t id deadline left ip
  set +e
  write_report
  # CL_PUBS is a list of addresses without spaces.
  for ip in "$A_PUB" "$B_PUB" "$V_PUB" "$T_PUB" $CL_PUBS; do [ -n "$ip" ] && [ -n "$SOCK_DIR" ] && ssh_close "$ip"; done
  if [ "$MODE" = "run" ] && want cluster; then
    cl_cleanup
    write_report
  fi
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
          delete_retrying "$type" "$id"
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
  if [ -n "$WORK" ] && [ "$KEEP_WORK" != 1 ]; then rm -rf "$WORK"; fi
  exit "$rc"
}

# ---------------------------------------------------------------------------------------------------------------

# authenticate_and_confirm: reads VULTR_API_KEY into a mode-0600 config file for curl, asks for the go-ahead unless
# --yes is given, and checks the key against the API.
authenticate_and_confirm() {
  local ans
  [ -n "${VULTR_API_KEY:-}" ] || die "VULTR_API_KEY is not set"
  write_auth_conf
  if [ "$ASSUME_YES" != 1 ]; then
    [ -t 0 ] || die "not a terminal: pass --yes to confirm"
    read -r -p "Proceed? [y/N] " ans
    case "$ans" in y | Y | yes) ;; *) die "aborted" ;; esac
  fi
  api GET /account
  api_ok || die "authentication failed: $API_STATUS $(api_err) (check the key and its IP allow-list)"
}

# main_cluster: the cluster check: says what it creates, and on a real run builds the cluster with tent and checks it.
# tent makes everything in the cloud itself; the run only makes a key pair and a state store in WORK.
main_cluster() {
  local instances=$((CL_SERVERS + CL_WORKERS)) cost end_note duration unreg_note=""
  CL_NAME="spk-$RUN"
  CL_STATE="$WORK/state"
  CL_URL="file://$CL_STATE"
  CL_OPDIR="$WORK/operator"
  duration="25-35 minutes"
  if [ "$UNREG" = 1 ]; then
    instances=$((instances + 1))
    duration="50-70 minutes: the last check waits until an instance is 32 minutes old, about 25-35 minutes after the other checks"
    unreg_note="
  - --unregistered: the run stops Nomad on $CL_NAME-workers-1, purges its node, takes tent/joined=true off the
    instance and waits until the instance is 32 minutes old (31 is tent's limit); then tent replaces the machine,
    which bills one more instance for its 1 hour minimum"
  fi
  cost=$(cost_text "$instances")
  end_note="the run deletes the cluster with tent delete cluster, and then removes by tag what is left"
  if [ "$KEEP" = 1 ]; then end_note="the cluster is NOT deleted: --keep given, so it stays and bills until you delete it"; fi
  cat >&2 <<EOF

This run builds a cluster named $CL_NAME with tent in your Vultr account ($REGION):
  - $CL_SERVERS servers and $CL_WORKERS clients of $PLAN, all created at once (the account's instance limit must
    allow $((CL_SERVERS + CL_WORKERS)) instances), 1 hour minimum each; $instances billed in all: $cost
  - 1 VPC, 2 firewall groups and 1 SSH key, made by tent and removed by tent delete cluster
  - after the build: tent export nomad (a certificate and a management token for 24 hours), the nomad CLI when it is
    installed, tent validate cluster, tent ui on a free loopback port (another management token for 24 hours; both
    tokens stay in Nomad until they expire), then Nomad stopped on $CL_NAME-workers-1 until validate sees the node
    down and started again; a reboot of one client, and a scale down of the clients to one in the specs, which
    update must refuse (the specs go back afterwards)$unreg_note
  - $end_note
Expected duration: $duration. Report: $REPORT

EOF
  [ "$MODE" = "dry-run" ] && { log "dry run: nothing created"; return 0; }
  prepare_cluster
  authenticate_and_confirm
  mkdir -m 700 "$CL_STATE"
  ssh-keygen -t ed25519 -N '' -q -f "$WORK/id_ed25519" -C "$RUN_TAG"
  SSH_KEY="$WORK/id_ed25519"
  ssh_init
  check_cluster
  log "all checks finished"
}

main() {
  local c
  while [ $# -gt 0 ]; do
    case "$1" in
      --preflight) MODE="preflight" ;;
      --dry-run) MODE="dry-run" ;;
      --yes) ASSUME_YES=1 ;;
      --keep) KEEP=1 ;;
      --unregistered) UNREG=1 ;;
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
  if want cluster && [ "$CHECKS" != cluster ]; then die "cluster runs alone: --only cluster"; fi
  if [ "$UNREG" = 1 ] && [ "$CHECKS" != cluster ]; then die "--unregistered needs --only cluster"; fi
  # tentnode needs one VPC: it tries the first mask alone and makes no test VPCs.
  if want tentnode; then VPC_MASKS="${VPC_MASKS%% *}"; fi

  for c in curl jq awk base64 tr od; do need_cmd "$c"; done
  if [ "$MODE" = "run" ] && want tentnode; then need_cmd go; need_cmd gzip; fi
  if [ "$MODE" = "run" ] && want cluster; then
    for c in openssl ssh ssh-keygen mkfifo find; do need_cmd "$c"; done
  fi
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
  OUT_DIR=$(cd "$OUT_DIR" && pwd)
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
  if want cluster; then
    main_cluster
    return 0
  fi
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
  cost=$(cost_text "$instances")
  case "$instances" in
    0) billed="no instances (nothing billed)" duration="1-3 minutes" ;;
    1) billed="1 instance of $PLAN, 1 hour minimum: $cost" duration="5-15 minutes" ;;
    *) billed="$instances instances of $PLAN (at most 3 at a time), 1 hour minimum each: $cost" duration="20-45 minutes" ;;
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
  authenticate_and_confirm

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
