#!/usr/bin/env bash
# Read-only topology preflight for ARTEX's optional local Hermes Subscription
# Proxy profile. It does not start Hermes, read ~/.hermes, make a provider
# request, change Docker state, create an ARTEX profile, or alter a firewall.
#
# Hermes' proxy has no inbound authentication of its own. On a multi-bridge
# Docker host, Docker's host-gateway alias can resolve to the default bridge
# rather than ARTEX's project bridge. Binding there can expose a
# subscription-backed endpoint to unrelated local containers, so fail closed
# before an operator follows the profile guide.
set -Eeuo pipefail
IFS=$'\n\t'
LC_ALL=C

SOURCE_PATH="${BASH_SOURCE[0]:-}"
if [[ -n "$SOURCE_PATH" ]]; then
  SCRIPT_DIR="$(cd -- "$(dirname -- "$SOURCE_PATH")" && pwd -P)"
else
  # `bash -s` has no BASH_SOURCE entry. The caller must then supply
  # --project-dir; use the current directory only for the unused default.
  SCRIPT_DIR="$(pwd -P)"
fi
readonly SCRIPT_DIR
DEFAULT_PROJECT_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd -P)"
readonly DEFAULT_PROJECT_DIR

PROJECT_DIR="$DEFAULT_PROJECT_DIR"
PROXY_PORT='8645'
BLOCKED=0

info() { printf '[hermes-proxy-preflight] %s\n' "$*"; }
warn() { printf '[hermes-proxy-preflight] warning: %s\n' "$*" >&2; }
block() {
  BLOCKED=1
  warn "$*"
}
die() {
  printf '[hermes-proxy-preflight] error: %s\n' "$*" >&2
  exit 2
}

usage() {
  cat <<'EOF'
Usage:
  scripts/preflight-hermes-subscription-proxy.sh [--project-dir DIR] [--port PORT]

Read-only safety preflight for a local Hermes Subscription Proxy used by the
ARTEX Compose project. It records only nonsecret Docker topology metadata:

  - ARTEX's one attached network and its IPv4 gateway;
  - host.docker.internal resolution inside ARTEX;
  - counts of foreign-project containers on either relevant bridge;
  - whether the selected local TCP port is already listening.

`--port` defaults to the legacy documented local route, TCP 8645. A reviewed
literal-bridge plan can pass its distinct port (currently TCP 864) for the same
read-only listener check. This script does not approve either binding.

It does not run `hermes`, inspect ~/.hermes, read credentials, start a
container, change a firewall, create an ARTEX LLM profile, or call a model.

Exit status:
  0  No condition this static topology check knows how to reject was found.
  2  Unsafe, incomplete, or ambiguous topology. Do not start a Hermes proxy.

An exit status of 0 is not authorization to start a proxy, send a model request,
or enable ARTEX agent execution. It still requires a separate owner/firewall
review and a public-or-synthetic provider-route qualification.
EOF
}

while (($#)); do
  case "$1" in
    --project-dir)
      (($# >= 2)) || die '--project-dir requires a value'
      PROJECT_DIR="$2"
      shift 2
      ;;
    --port)
      (($# >= 2)) || die '--port requires a value'
      PROXY_PORT="$2"
      shift 2
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      die "unknown option: $1"
      ;;
  esac
done

[[ "$PROXY_PORT" =~ ^[0-9]{1,5}$ ]] || die '--port must be a TCP port number'
(( 10#$PROXY_PORT >= 1 && 10#$PROXY_PORT <= 65535 )) || die '--port must be in the range 1..65535'
[[ -d "$PROJECT_DIR" ]] || die "project directory does not exist: $PROJECT_DIR"
PROJECT_DIR="$(cd -- "$PROJECT_DIR" && pwd -P)"
[[ -f "$PROJECT_DIR/docker-compose.yml" ]] || die "docker-compose.yml is missing from: $PROJECT_DIR"
command -v docker >/dev/null 2>&1 || die 'docker is required'
command -v ss >/dev/null 2>&1 || die 'ss is required'

compose() {
  (
    cd -- "$PROJECT_DIR"
    docker compose "$@"
  )
}

network_gateway() {
  docker network inspect -f '{{range .IPAM.Config}}{{println .Gateway}}{{end}}' "$1"
}

network_container_ids() {
  docker network inspect -f '{{range $id, $container := .Containers}}{{println $id}}{{end}}' "$1"
}

network_foreign_container_count() {
  local network="$1" project="$2" raw id owner count=0
  local -a ids=()
  raw="$(network_container_ids "$network" 2>/dev/null)" || return 1
  mapfile -t ids < <(printf '%s\n' "$raw" | sed '/^$/d')
  for id in "${ids[@]}"; do
    owner="$(docker inspect -f '{{ index .Config.Labels "com.docker.compose.project" }}' "$id" 2>/dev/null)" || return 1
    if [[ "$owner" != "$project" ]]; then
      ((count += 1))
    fi
  done
  printf '%s\n' "$count"
}

is_ipv4() {
  local value="$1" item
  local -a octets=()
  [[ "$value" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] || return 1
  IFS='.' read -r -a octets <<<"$value"
  ((${#octets[@]} == 4)) || return 1
  for item in "${octets[@]}"; do
    ((10#$item <= 255)) || return 1
  done
}

if ! artex_ids_raw="$(compose ps -q artex 2>/dev/null)"; then
  die 'could not query the ARTEX Compose service'
fi
mapfile -t ARTEX_IDS < <(printf '%s\n' "$artex_ids_raw" | sed '/^$/d')
((${#ARTEX_IDS[@]} == 1)) || die 'exactly one running ARTEX service container is required'
ARTEX_ID="${ARTEX_IDS[0]}"

if ! ARTEX_PROJECT="$(docker inspect -f '{{ index .Config.Labels "com.docker.compose.project" }}' "$ARTEX_ID" 2>/dev/null)"; then
  die 'could not identify the ARTEX Compose project'
fi
[[ -n "$ARTEX_PROJECT" && "$ARTEX_PROJECT" != '<no value>' ]] || die 'could not identify the ARTEX Compose project'

if ! networks_raw="$(docker inspect -f '{{range $name, $cfg := .NetworkSettings.Networks}}{{printf "%s\t%s\t%s\n" $name $cfg.IPAddress $cfg.Gateway}}{{end}}' "$ARTEX_ID" 2>/dev/null)"; then
  die 'could not inspect ARTEX network metadata'
fi
mapfile -t ARTEX_NETWORK_ROWS < <(printf '%s\n' "$networks_raw" | sed '/^$/d')
((${#ARTEX_NETWORK_ROWS[@]} == 1)) || die 'ARTEX must be attached to exactly one Docker network for this preflight'
IFS=$'\t' read -r ARTEX_NETWORK ARTEX_CONTAINER_IP ARTEX_GATEWAY <<<"${ARTEX_NETWORK_ROWS[0]}"
[[ -n "$ARTEX_NETWORK" && -n "$ARTEX_CONTAINER_IP" && -n "$ARTEX_GATEWAY" ]] || die 'could not determine the ARTEX network metadata'
is_ipv4 "$ARTEX_GATEWAY" || die "ARTEX network gateway is not IPv4: $ARTEX_GATEWAY"

if ! ARTEX_FOREIGN_COUNT="$(network_foreign_container_count "$ARTEX_NETWORK" "$ARTEX_PROJECT")"; then
  die 'could not count containers on the ARTEX network'
fi
[[ "$ARTEX_FOREIGN_COUNT" =~ ^[0-9]+$ ]] || die 'invalid ARTEX-network container count'

info "project_dir=$PROJECT_DIR"
info "compose_project=$ARTEX_PROJECT"
info "artex_network=$ARTEX_NETWORK"
info "artex_container_ipv4=$ARTEX_CONTAINER_IP"
info "artex_network_gateway=$ARTEX_GATEWAY"
info "foreign_containers_on_artex_network=$ARTEX_FOREIGN_COUNT"
if ((ARTEX_FOREIGN_COUNT > 0)); then
  block "ARTEX network contains $ARTEX_FOREIGN_COUNT container(s) outside Compose project $ARTEX_PROJECT"
fi

if ! alias_raw="$(compose exec -T artex getent ahostsv4 host.docker.internal </dev/null 2>/dev/null)"; then
  alias_raw=""
  block 'could not resolve host.docker.internal inside ARTEX'
fi
mapfile -t ALIAS_IPS < <(printf '%s\n' "$alias_raw" | awk '/^[0-9]+(\.[0-9]+){3}[[:space:]]/ {print $1}' | sort -u)

if ((${#ALIAS_IPS[@]} != 1)); then
  block 'host.docker.internal must resolve to exactly one IPv4 address inside ARTEX'
  info 'host_alias_ipv4=unresolved-or-ambiguous'
else
  HOST_ALIAS_IP="${ALIAS_IPS[0]}"
  info "host_alias_ipv4=$HOST_ALIAS_IP"

  if ! all_networks_raw="$(docker network ls --format '{{.Name}}' 2>/dev/null)"; then
    die 'could not enumerate Docker networks'
  fi
  mapfile -t ALL_NETWORKS < <(printf '%s\n' "$all_networks_raw" | sed '/^$/d')
  ((${#ALL_NETWORKS[@]} > 0)) || die 'could not enumerate Docker networks'

  ALIAS_NETWORKS=()
  for candidate_network in "${ALL_NETWORKS[@]}"; do
    if ! gateways_raw="$(network_gateway "$candidate_network" 2>/dev/null)"; then
      die "could not inspect Docker network: $candidate_network"
    fi
    while IFS= read -r gateway; do
      [[ -n "$gateway" ]] || continue
      if [[ "$gateway" == "$HOST_ALIAS_IP" ]]; then
        ALIAS_NETWORKS+=("$candidate_network")
      fi
    done <<<"$gateways_raw"
  done

  if ((${#ALIAS_NETWORKS[@]} != 1)); then
    block "host alias $HOST_ALIAS_IP does not map to exactly one Docker bridge gateway"
    info 'host_alias_network=unresolved-or-ambiguous'
  else
    HOST_ALIAS_NETWORK="${ALIAS_NETWORKS[0]}"
    if ! ALIAS_FOREIGN_COUNT="$(network_foreign_container_count "$HOST_ALIAS_NETWORK" "$ARTEX_PROJECT")"; then
      die 'could not count containers on the host-alias network'
    fi
    [[ "$ALIAS_FOREIGN_COUNT" =~ ^[0-9]+$ ]] || die 'invalid host-alias-network container count'
    info "host_alias_network=$HOST_ALIAS_NETWORK"
    info "foreign_containers_on_host_alias_network=$ALIAS_FOREIGN_COUNT"

    if [[ "$HOST_ALIAS_IP" != "$ARTEX_GATEWAY" ]]; then
      block "host alias $HOST_ALIAS_IP is not ARTEX's own bridge gateway $ARTEX_GATEWAY"
    fi
    if ((ALIAS_FOREIGN_COUNT > 0)); then
      block "host alias network contains $ALIAS_FOREIGN_COUNT container(s) outside Compose project $ARTEX_PROJECT"
    fi
  fi
fi

if ! listener_raw="$(ss -H -ltn "sport = :$PROXY_PORT" 2>/dev/null)"; then
  die "could not inspect TCP listener state for port $PROXY_PORT"
fi
mapfile -t LISTENER_ROWS < <(printf '%s\n' "$listener_raw" | sed '/^$/d')
info "tcp_${PROXY_PORT}_listener_count=${#LISTENER_ROWS[@]}"
if ((${#LISTENER_ROWS[@]} > 0)); then
  block "TCP port $PROXY_PORT is already listening; do not start or rebind a proxy until its owner and bind scope are reviewed"
fi

if ((BLOCKED)); then
  info 'result=BLOCKED'
  info 'next=Do not start Hermes proxy or create a Hermes ARTEX profile. Correct the Docker alias or bridge mapping, then re-run this read-only preflight. A separate allow-only firewall review is still required after it passes.'
  exit 2
fi

info "suggested_profile_base_url=http://$ARTEX_GATEWAY:$PROXY_PORT/v1"
info 'result=TOPOLOGY_REVIEW_REQUIRED'
info 'next=Before any proxy start, verify the installed Hermes CLI flags, an allow-only bridge/firewall policy, active ARTEX work, and the public-or-synthetic route receipt. This script did not start or qualify a provider.'
