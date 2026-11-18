#!/usr/bin/env bash
# Root-owned reconciler for the optional, local ARTEX-to-Hermes bridge route.
# It never starts Hermes.  Without --apply it only inspects Docker/iptables and
# prints a plan.  It manages one INPUT jump and one owned iptables-nft chain.
set -Eeuo pipefail
IFS=$'\n\t'
export LC_ALL=C
PATH='/usr/sbin:/usr/bin:/sbin:/bin'
unset BASH_ENV ENV CDPATH DOCKER_CONFIG DOCKER_CONTEXT DOCKER_HOST COMPOSE_PROJECT_NAME

readonly DOCKER_BIN='/usr/bin/docker'
readonly IPTABLES_BIN='/usr/sbin/iptables'
readonly IP_BIN='/usr/sbin/ip'
readonly SS_BIN='/usr/bin/ss'
readonly INSTALL_BIN='/usr/bin/install'
readonly MKTEMP_BIN='/usr/bin/mktemp'
readonly CHOWN_BIN='/usr/bin/chown'
readonly CHMOD_BIN='/usr/bin/chmod'
readonly MV_BIN='/usr/bin/mv'
readonly RM_BIN='/usr/bin/rm'
readonly STAT_BIN='/usr/bin/stat'
readonly ID_BIN='/usr/bin/id'
readonly SYSCTL_BIN='/usr/sbin/sysctl'
readonly CHAIN='ARTEX_HERMES_PROXY'
readonly COMMENT_PREFIX='artex-hermes-literal-bridge'
readonly JUMP_COMMENT="$COMMENT_PREFIX-jump"
readonly ALLOW_COMMENT="$COMMENT_PREFIX-allow"
readonly DROP_COMMENT="$COMMENT_PREFIX-drop"
# This temporary top-priority deny remains until a teardown has completed. It
# prevents a concurrent firewall mutation from exposing the endpoint while the
# ordinary jump/chain are being removed one command at a time.
readonly TEARDOWN_GUARD_COMMENT="$COMMENT_PREFIX-teardown-drop"
readonly ROUTE_FILE='/run/artex-hermes-literal-bridge/route.env'
# A privileged port prevents an arbitrary unprivileged local account from
# winning the listener race between firewall setup and the Hermes service.
# The helper verifies the host's unprivileged-port boundary before it writes.
readonly PROXY_PORT='864'

PROJECT='artex'
APPLY=0
REMOVE=0
removal_guard_should_remain=0
removal_guard_gateway=''
removal_rollback_guard_rule=()

info() { printf '[artex-hermes-literal-bridge] %s\n' "$*"; }
die() {
  printf '[artex-hermes-literal-bridge] error: %s\n' "$*" >&2
  exit 2
}

usage() {
  cat <<EOF
Usage:
  artex-hermes-literal-bridge-firewall.sh [--project NAME] [--remove] [--apply]

Without --apply, inspect the current Docker topology and print the exact
allow/drop plan.  --apply requires root and manages only this host INPUT rule:

  allow source=<one current ARTEX container> on its own bridge to
        destination=<that bridge gateway>:$PROXY_PORT
  drop  every other INPUT packet to that gateway and port

It refuses ambiguous/non-bridge/shared-network topology. It never starts a
proxy, changes Docker-managed chains or global firewall configuration,
flushes/saves/restores a firewall, reads Hermes credentials, or sends a
provider request.

On hosts that restrict firewall inspection, run even this no-write plan through
sudo so it can read the active INPUT rules. Without --apply it still makes no
firewall, service, Docker, Hermes, or provider change.

'--remove --apply' is the only teardown path. It requires root and removes
only a fully validated instance of this helper's exact INPUT jump, owned chain,
and route file. It installs a temporary top-priority deny before cleanup and
retains that deny if malformed or externally referenced state is detected.
EOF
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

is_private_ipv4() {
  local value="$1" a b _c _d
  is_ipv4 "$value" || return 1
  IFS='.' read -r a b _c _d <<<"$value"
  ((10#$a == 10)) && return 0
  ((10#$a == 172 && 10#$b >= 16 && 10#$b <= 31)) && return 0
  ((10#$a == 192 && 10#$b == 168)) && return 0
  return 1
}

count_lines() {
  local input="$1"
  if [[ -z "$input" ]]; then
    printf '0\n'
  else
    printf '%s\n' "$input" | wc -l | tr -d '[:space:]'
  fi
}

assert_no_port_listener() {
  local listeners
  listeners="$("$SS_BIN" -H -ltn "sport = :$PROXY_PORT" 2>/dev/null)" || die "could not inspect TCP listener state for port $PROXY_PORT"
  [[ -z "$listeners" ]] || die "TCP port $PROXY_PORT is already listening; refusing to expose an unrelated service"
}

assert_proxy_port_is_privileged() {
  local first_unprivileged
  first_unprivileged="$("$SYSCTL_BIN" -n net.ipv4.ip_unprivileged_port_start 2>/dev/null)" || die 'could not inspect the unprivileged TCP port boundary'
  [[ "$first_unprivileged" =~ ^[0-9]{1,5}$ ]] || die 'unprivileged TCP port boundary was invalid'
  (( 10#$first_unprivileged <= 65535 )) || die 'unprivileged TCP port boundary was invalid'
  (( 10#$PROXY_PORT < 10#$first_unprivileged )) || die "TCP port $PROXY_PORT is not privileged on this host; refusing a listener-race-prone deployment"
}

assert_bridge_gateway_is_assigned() {
  local bridge_addresses row found=0
  bridge_addresses="$("$IP_BIN" -4 -o addr show dev "$BRIDGE_IFACE" 2>/dev/null)" || die 'could not inspect ARTEX bridge IPv4 addresses'
  while IFS= read -r row; do
    [[ "$row" == *" inet $GATEWAY/"* ]] && found=1
  done <<<"$bridge_addresses"
  (( found == 1 )) || die 'Docker-reported ARTEX gateway is not assigned to the discovered bridge'
}

prepare_route_path() {
  local metadata owner group mode
  route_dir="$(dirname -- "$ROUTE_FILE")"
  [[ ! -L "$route_dir" ]] || die 'route directory must not be a symlink'
  if [[ -e "$route_dir" && ! -d "$route_dir" ]]; then
    die 'route path exists but is not a directory'
  fi
  "$INSTALL_BIN" -d -o root -g root -m 0755 -- "$route_dir"
  metadata="$("$STAT_BIN" -c '%u:%g:%a' -- "$route_dir" 2>/dev/null)" || die 'could not inspect route directory ownership'
  IFS=':' read -r owner group mode <<<"$metadata"
  [[ "$owner" == '0' && "$group" == '0' && "$mode" == '755' ]] || die 'route directory must be root-owned mode 0755'

  if [[ -L "$ROUTE_FILE" ]]; then
    die 'route file must not be a symlink'
  fi
  if [[ -e "$ROUTE_FILE" ]]; then
    [[ -f "$ROUTE_FILE" ]] || die 'route file exists but is not a regular file'
    metadata="$("$STAT_BIN" -c '%u:%g:%a' -- "$ROUTE_FILE" 2>/dev/null)" || die 'could not inspect route file ownership'
    IFS=':' read -r owner group mode <<<"$metadata"
    [[ "$owner" == '0' && "$group" == '0' && "$mode" == '644' ]] || die 'route file must be root-owned mode 0644'
  fi
}

validate_existing_chain() {
  local -a lines=()
  chain_exists=0
  if chain_rules="$("$IPTABLES_BIN" -w -S "$CHAIN" 2>/dev/null)"; then
    chain_exists=1
    mapfile -t lines < <(printf '%s\n' "$chain_rules" | sed '/^$/d')
    ((${#lines[@]} == 3)) || die 'owned firewall chain exists with an unexpected rule count'
    [[ "${lines[0]}" == "-N $CHAIN" ]] || die 'owned firewall chain has an unexpected declaration'
    [[ "${lines[1]}" == *"$ALLOW_COMMENT"* && "${lines[1]}" == *'-j ACCEPT' ]] || die 'owned firewall chain does not put the allow rule first'
    [[ "${lines[2]}" == *"$DROP_COMMENT"* && "${lines[2]}" == *'-j DROP' ]] || die 'owned firewall chain does not put the terminal drop rule second'
    "$IPTABLES_BIN" -w -C "$CHAIN" "${ALLOW_RULE[@]}" >/dev/null 2>&1 || die 'owned firewall chain has an unexpected allow rule'
    "$IPTABLES_BIN" -w -C "$CHAIN" "${DROP_RULE[@]}" >/dev/null 2>&1 || die 'owned firewall chain has an unexpected drop rule'
  fi
}

reject_unowned_endpoint_rules() {
  local rule
  while IFS= read -r rule; do
    [[ "$rule" == '-A INPUT '* ]] || continue
    if [[ "$rule" == *"--dport $PROXY_PORT"* || "$rule" == *"--destination-port $PROXY_PORT"* ]]; then
      [[ "$rule" == *"$JUMP_COMMENT"* ]] || die "an unowned INPUT rule already affects TCP/$PROXY_PORT; refusing endpoint ambiguity"
    fi
  done <<<"$input_rules"
}

is_removable_allow_rule() {
  local rule="$1" gateway="$2" container_ipv4="$3" bridge_iface="$4"
  [[ "$rule" == "-A $CHAIN "* ]] || return 1
  [[ "$rule" == *" -i $bridge_iface "* && "$rule" == *" -s $container_ipv4/32 "* && "$rule" == *' -d '* ]] || return 1
  [[ "$rule" == *" -d $gateway "* || "$rule" == *" -d $gateway/32 "* ]] || return 1
  [[ "$rule" == *' -p tcp '* && "$rule" == *"--dport $PROXY_PORT"* ]] || return 1
  [[ "$rule" == *"--comment $ALLOW_COMMENT"* && "$rule" == *' -j ACCEPT' ]] || return 1
}

is_removable_drop_rule() {
  local rule="$1" gateway="$2"
  [[ "$rule" == "-A $CHAIN "* ]] || return 1
  [[ "$rule" == *' -d '* && "$rule" == *' -p tcp '* && "$rule" == *"--dport $PROXY_PORT"* ]] || return 1
  [[ "$rule" == *" -d $gateway "* || "$rule" == *" -d $gateway/32 "* ]] || return 1
  [[ "$rule" == *"--comment $DROP_COMMENT"* && "$rule" == *' -j DROP' ]] || return 1
}

is_removable_input_jump() {
  local rule="$1" gateway="$2"
  [[ "$rule" == '-A INPUT '* ]] || return 1
  [[ "$rule" == *' -d '* && "$rule" == *' -p tcp '* && "$rule" == *"--dport $PROXY_PORT"* ]] || return 1
  [[ "$rule" == *" -d $gateway "* || "$rule" == *" -d $gateway/32 "* ]] || return 1
  [[ "$rule" == *"--comment $JUMP_COMMENT"* && "$rule" == *" -j $CHAIN" ]] || return 1
}

is_removable_teardown_guard() {
  local rule="$1" gateway="$2"
  [[ "$rule" == '-A INPUT '* ]] || return 1
  [[ "$rule" == *' -d '* && "$rule" == *' -p tcp '* && "$rule" == *"--dport $PROXY_PORT"* ]] || return 1
  [[ "$rule" == *" -d $gateway "* || "$rule" == *" -d $gateway/32 "* ]] || return 1
  [[ "$rule" == *"--comment $TEARDOWN_GUARD_COMMENT"* && "$rule" == *' -j DROP' ]] || return 1
}

is_canonical_teardown_guard_rendering() {
  local rule="$1" gateway="$2"
  local bare masked
  bare="-A INPUT -d $gateway -p tcp -m tcp --dport $PROXY_PORT -m comment --comment $TEARDOWN_GUARD_COMMENT -j DROP"
  masked="-A INPUT -d $gateway/32 -p tcp -m tcp --dport $PROXY_PORT -m comment --comment $TEARDOWN_GUARD_COMMENT -j DROP"
  [[ "$rule" == "$bare" || "$rule" == "$masked" ]]
}

parse_guard_gateway_without_route() {
  local rule="$1" candidate
  [[ "$rule" =~ ^-A[[:space:]]INPUT[[:space:]]-d[[:space:]]([0-9.]+)(/32)?[[:space:]]-p[[:space:]]tcp[[:space:]]-m[[:space:]]tcp[[:space:]]--dport[[:space:]]${PROXY_PORT}[[:space:]]-m[[:space:]]comment[[:space:]]--comment[[:space:]]${TEARDOWN_GUARD_COMMENT}[[:space:]]-j[[:space:]]DROP$ ]] || return 1
  candidate="${BASH_REMATCH[1]}"
  is_private_ipv4 "$candidate" || return 1
  printf '%s\n' "$candidate"
}

inspect_route_file_for_removal() {
  local metadata owner group mode route_line
  local -a removal_route_lines=()
  removal_route_exists=0
  removal_gateway=''
  removal_container_ipv4=''
  removal_bridge_iface=''
  removal_proxy_port=''
  [[ ! -L "$ROUTE_FILE" ]] || die 'route file must not be a symlink'
  [[ -e "$ROUTE_FILE" ]] || return 0
  [[ -f "$ROUTE_FILE" ]] || die 'route file exists but is not a regular file'
  metadata="$("$STAT_BIN" -c '%u:%g:%a' -- "$ROUTE_FILE" 2>/dev/null)" || die 'could not inspect route file ownership'
  IFS=':' read -r owner group mode <<<"$metadata"
  [[ "$owner" == '0' && "$group" == '0' && "$mode" == '644' ]] || die 'route file must be root-owned mode 0644'
  mapfile -t removal_route_lines < <(sed '/^$/d' "$ROUTE_FILE")
  ((${#removal_route_lines[@]} == 4)) || die 'route file has an unexpected record count'
  route_line="${removal_route_lines[0]}"
  [[ "$route_line" == 'ARTEX_HERMES_BRIDGE_GATEWAY='* ]] || die 'route file has an unexpected first record'
  removal_gateway="${route_line#ARTEX_HERMES_BRIDGE_GATEWAY=}"
  route_line="${removal_route_lines[1]}"
  [[ "$route_line" == 'ARTEX_HERMES_CONTAINER_IPV4='* ]] || die 'route file has an unexpected second record'
  removal_container_ipv4="${route_line#ARTEX_HERMES_CONTAINER_IPV4=}"
  route_line="${removal_route_lines[2]}"
  [[ "$route_line" == 'ARTEX_HERMES_BRIDGE_INTERFACE='* ]] || die 'route file has an unexpected third record'
  removal_bridge_iface="${route_line#ARTEX_HERMES_BRIDGE_INTERFACE=}"
  route_line="${removal_route_lines[3]}"
  [[ "$route_line" == 'ARTEX_HERMES_PROXY_PORT='* ]] || die 'route file has an unexpected fourth record'
  removal_proxy_port="${route_line#ARTEX_HERMES_PROXY_PORT=}"
  is_private_ipv4 "$removal_gateway" || die 'route file gateway is not private IPv4'
  is_private_ipv4 "$removal_container_ipv4" || die 'route file container address is not private IPv4'
  [[ "$removal_bridge_iface" =~ ^br-[A-Za-z0-9]+$ ]] || die 'route file bridge interface is unsafe'
  [[ "$removal_proxy_port" == "$PROXY_PORT" ]] || die 'route file proxy port did not match this helper'
  [[ "$removal_gateway" != "$removal_container_ipv4" ]] || die 'route file container address equals gateway'
  removal_route_exists=1
}

inspect_owned_state_for_removal() {
  local -a lines=()
  local rule reference_lines full_rules
  local reference_count teardown_guard_count=0 first_input_rule='' guard_gateway='' unowned_endpoint_rule=0

  removal_chain_exists=0
  removal_allow_present=0
  removal_drop_present=0
  removal_jump_count=0
  removal_teardown_guard_present=0
  removal_jump_rule=()
  removal_allow_rule=()
  removal_drop_rule=()
  removal_teardown_guard_rule=()
  inspect_route_file_for_removal
  input_rules="$("$IPTABLES_BIN" -w -S INPUT 2>/dev/null)" || die 'could not inspect INPUT rules'

  if chain_rules="$("$IPTABLES_BIN" -w -S "$CHAIN" 2>/dev/null)"; then
    removal_chain_exists=1
    (( removal_route_exists == 1 )) || die 'owned firewall chain exists but its route file is unavailable; refusing teardown'
    mapfile -t lines < <(printf '%s\n' "$chain_rules" | sed '/^$/d')
    ((${#lines[@]} >= 1)) || die 'owned firewall chain did not contain a declaration; refusing teardown'
    [[ "${lines[0]}" == "-N $CHAIN" ]] || die 'owned firewall chain has an unexpected declaration; refusing teardown'
    case "${#lines[@]}" in
      3)
        is_removable_allow_rule "${lines[1]}" "$removal_gateway" "$removal_container_ipv4" "$removal_bridge_iface" || die 'owned firewall chain has an unexpected allow rule; refusing teardown'
        is_removable_drop_rule "${lines[2]}" "$removal_gateway" || die 'owned firewall chain has an unexpected drop rule; refusing teardown'
        removal_allow_present=1
        removal_drop_present=1
        ;;
      2)
        is_removable_drop_rule "${lines[1]}" "$removal_gateway" || die 'owned restrictive firewall state has an unexpected terminal drop; refusing teardown'
        removal_drop_present=1
        ;;
      1)
        # This is a resumable guarded state after the exact terminal drop was
        # removed but a concurrent foreign mutation prevented chain deletion.
        ;;
      *)
        die 'owned firewall chain has an unexpected rule count; refusing teardown'
        ;;
    esac
  fi

  while IFS= read -r rule; do
    [[ "$rule" == '-A INPUT '* ]] || continue
    [[ -n "$first_input_rule" ]] || first_input_rule="$rule"
    if [[ "$rule" == *"$JUMP_COMMENT"* || "$rule" == *"-j $CHAIN"* ]]; then
      (( removal_route_exists == 1 )) || die 'owned INPUT jump exists but its route file is unavailable; refusing teardown'
      is_removable_input_jump "$rule" "$removal_gateway" || die 'owned INPUT jump has unexpected semantics; refusing teardown'
      ((removal_jump_count += 1))
    elif [[ "$rule" == *"$TEARDOWN_GUARD_COMMENT"* ]]; then
      if (( removal_route_exists )); then
        is_removable_teardown_guard "$rule" "$removal_gateway" || die 'temporary teardown guard has unexpected semantics; refusing teardown'
      else
        guard_gateway="$(parse_guard_gateway_without_route "$rule")" || die 'temporary teardown guard has unexpected semantics; refusing teardown'
      fi
      ((teardown_guard_count += 1))
    elif [[ "$rule" == *"--dport $PROXY_PORT"* || "$rule" == *"--destination-port $PROXY_PORT"* ]]; then
      # Continue so a valid guard below a concurrent foreign rule can be
      # remembered and restored to position one by the removal error handler.
      unowned_endpoint_rule=1
    fi
  done <<<"$input_rules"

  (( teardown_guard_count <= 2 )) || die 'too many temporary teardown guards exist; refusing teardown'
  if (( teardown_guard_count >= 1 )); then
    removal_teardown_guard_present=1
    if (( APPLY )); then
      removal_guard_should_remain=1
    fi
    if (( ! removal_route_exists )); then
      removal_gateway="$guard_gateway"
    fi
    removal_teardown_guard_rule=(-d "$removal_gateway" -p tcp -m tcp --dport "$PROXY_PORT" -m comment --comment "$TEARDOWN_GUARD_COMMENT" -j DROP)
    removal_guard_gateway="$removal_gateway"
    removal_rollback_guard_rule=("${removal_teardown_guard_rule[@]}")
    "$IPTABLES_BIN" -w -C INPUT "${removal_teardown_guard_rule[@]}" >/dev/null 2>&1 || die 'temporary teardown guard did not match the route record; refusing teardown'
    is_canonical_teardown_guard_rendering "$first_input_rule" "$removal_gateway" || die 'temporary teardown guard is not first in INPUT; refusing teardown'
  fi
  (( unowned_endpoint_rule == 0 )) || die "an unowned INPUT rule already affects TCP/$PROXY_PORT; refusing teardown"

  if (( removal_chain_exists )); then
    removal_jump_rule=(-d "$removal_gateway" -p tcp -m tcp --dport "$PROXY_PORT" -m comment --comment "$JUMP_COMMENT" -j "$CHAIN")
    removal_allow_rule=(-i "$removal_bridge_iface" -s "$removal_container_ipv4/32" -d "$removal_gateway" -p tcp -m tcp --dport "$PROXY_PORT" -m comment --comment "$ALLOW_COMMENT" -j ACCEPT)
    removal_drop_rule=(-d "$removal_gateway" -p tcp -m tcp --dport "$PROXY_PORT" -m comment --comment "$DROP_COMMENT" -j DROP)
    if (( removal_jump_count == 1 )); then
      (( removal_drop_present == 1 )) || die 'owned firewall chain lost its terminal drop while its INPUT jump remains; refusing teardown'
    else
      (( removal_jump_count == 0 && removal_teardown_guard_present == 1 && removal_allow_present == 0 )) || die 'owned firewall chain does not have exactly one removable INPUT jump or a temporary fail-closed guard'
    fi
    full_rules="$("$IPTABLES_BIN" -w -S 2>/dev/null)" || die 'could not inspect firewall chain references'
    reference_lines="$(printf '%s\n' "$full_rules" | grep -F -- "-j $CHAIN" || true)"
    reference_count="$(count_lines "$reference_lines")"
    (( reference_count == removal_jump_count )) || die 'owned firewall chain has an unexpected external reference; refusing teardown'
    if (( removal_allow_present )); then
      "$IPTABLES_BIN" -w -C "$CHAIN" "${removal_allow_rule[@]}" >/dev/null 2>&1 || die 'owned firewall chain allow rule did not match the route record; refusing teardown'
    fi
    if (( removal_drop_present )); then
      "$IPTABLES_BIN" -w -C "$CHAIN" "${removal_drop_rule[@]}" >/dev/null 2>&1 || die 'owned firewall chain drop rule did not match the route record; refusing teardown'
    fi
  else
    (( removal_jump_count == 0 )) || die 'owned INPUT jump exists without its owned chain; refusing teardown'
  fi
}

ensure_teardown_guard() {
  if (( ! removal_chain_exists && ! removal_teardown_guard_present )); then
    return 0
  fi
  (( removal_route_exists == 1 || removal_teardown_guard_present == 1 )) || die 'cannot retain a teardown guard without its route record'
  if (( removal_teardown_guard_present )); then
    removal_guard_should_remain=1
    return 0
  fi
  removal_teardown_guard_rule=(-d "$removal_gateway" -p tcp -m tcp --dport "$PROXY_PORT" -m comment --comment "$TEARDOWN_GUARD_COMMENT" -j DROP)
  removal_guard_gateway="$removal_gateway"
  removal_rollback_guard_rule=("${removal_teardown_guard_rule[@]}")
  removal_guard_should_remain=1
  "$IPTABLES_BIN" -w -I INPUT 1 "${removal_teardown_guard_rule[@]}"
}

remove_owned_state() {
  while :; do
    # Inspect before every individual transition. Any unexpected state makes
    # the EXIT handler retain the temporary top-priority deny rule.
    inspect_owned_state_for_removal

    if (( ! removal_chain_exists )); then
      if (( removal_route_exists )); then
        "$RM_BIN" -f -- "$ROUTE_FILE"
        continue
      fi
      if (( removal_teardown_guard_present )); then
        assert_no_port_listener
        "$IPTABLES_BIN" -w -D INPUT "${removal_teardown_guard_rule[@]}"
        continue
      fi
      removal_guard_should_remain=0
      return 0
    fi

    (( removal_teardown_guard_present == 1 )) || die 'temporary fail-closed teardown guard is unavailable'
    if (( removal_allow_present )); then
      "$IPTABLES_BIN" -w -D "$CHAIN" "${removal_allow_rule[@]}"
      continue
    fi
    if (( removal_jump_count == 1 )); then
      "$IPTABLES_BIN" -w -D INPUT "${removal_jump_rule[@]}"
      continue
    fi
    if (( removal_drop_present )); then
      "$IPTABLES_BIN" -w -D "$CHAIN" "${removal_drop_rule[@]}"
      continue
    fi
    "$IPTABLES_BIN" -w -X "$CHAIN"
  done
}

rollback_removal_on_error() {
  local status=$?
  local rule first_input_rule=''
  trap - EXIT
  if (( status != 0 && removal_guard_should_remain )) && ((${#removal_rollback_guard_rule[@]} > 0)); then
    while IFS= read -r rule; do
      [[ "$rule" == '-A INPUT '* ]] || continue
      first_input_rule="$rule"
      break
    done < <("$IPTABLES_BIN" -w -S INPUT 2>/dev/null || true)
    if ! is_canonical_teardown_guard_rendering "$first_input_rule" "$removal_guard_gateway"; then
      "$IPTABLES_BIN" -w -I INPUT 1 "${removal_rollback_guard_rule[@]}" >/dev/null 2>&1 || true
    fi
  fi
  exit "$status"
}

rollback_on_error() {
  local status=$?
  trap - EXIT
  if (( status != 0 )); then
    if (( created_jump )); then
      "$IPTABLES_BIN" -w -D INPUT "${JUMP_RULE[@]}" >/dev/null 2>&1 || true
    fi
    if (( created_chain )); then
      "$IPTABLES_BIN" -w -D "$CHAIN" "${DROP_RULE[@]}" >/dev/null 2>&1 || true
      "$IPTABLES_BIN" -w -D "$CHAIN" "${ALLOW_RULE[@]}" >/dev/null 2>&1 || true
      "$IPTABLES_BIN" -w -X "$CHAIN" >/dev/null 2>&1 || true
    fi
    if [[ -n "${route_tmp:-}" ]]; then
      "$RM_BIN" -f -- "$route_tmp" >/dev/null 2>&1 || true
    fi
  fi
  exit "$status"
}

while (($#)); do
  case "$1" in
    --project)
      (($# >= 2)) || die '--project requires a value'
      PROJECT="$2"
      shift 2
      ;;
    --apply)
      APPLY=1
      shift
      ;;
    --remove)
      REMOVE=1
      shift
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      die "unknown option: $1 (use --help)"
      ;;
  esac
done

[[ "$PROJECT" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*$ ]] || die '--project has unsafe syntax'
[[ -x "$IPTABLES_BIN" ]] || die "iptables binary is unavailable: $IPTABLES_BIN"
[[ -x "$RM_BIN" && -x "$STAT_BIN" && -x "$ID_BIN" ]] || die 'required teardown system binary is unavailable'

iptables_version="$("$IPTABLES_BIN" -V 2>/dev/null)" || die 'could not identify iptables backend'
[[ "$iptables_version" == *nf_tables* ]] || die 'iptables-nft backend is required; refusing another backend'

if (( REMOVE )); then
  if (( APPLY )); then
    [[ ${EUID:-$("$ID_BIN" -u)} -eq 0 ]] || die '--remove --apply requires root'
    assert_proxy_port_is_privileged
    assert_no_port_listener
    trap rollback_removal_on_error EXIT
  fi
  inspect_owned_state_for_removal
  info "owned_chain_exists=$removal_chain_exists"
  info "owned_input_jump_count=$removal_jump_count"
  info "owned_route_file_exists=$removal_route_exists"
  if (( ! APPLY )); then
    info 'result=PLAN_ONLY; pass --remove --apply as root only after owner approval'
    exit 0
  fi
  ensure_teardown_guard
  remove_owned_state
  trap - EXIT
  info 'result=REMOVED; no proxy was stopped or started'
  exit 0
fi

[[ -x "$DOCKER_BIN" ]] || die "docker binary is unavailable: $DOCKER_BIN"
  [[ -x "$IP_BIN" && -x "$SS_BIN" && -x "$SYSCTL_BIN" && -x "$INSTALL_BIN" && -x "$MKTEMP_BIN" && -x "$CHOWN_BIN" && -x "$CHMOD_BIN" && -x "$MV_BIN" ]] || die 'required topology/apply system binary is unavailable'

if ! raw_ids="$("$DOCKER_BIN" ps -q --filter "label=com.docker.compose.project=$PROJECT" --filter 'label=com.docker.compose.service=artex' 2>/dev/null)"; then
  die 'could not query the ARTEX Compose service'
fi
mapfile -t artex_ids < <(printf '%s\n' "$raw_ids" | sed '/^$/d')
((${#artex_ids[@]} == 1)) || die 'exactly one running ARTEX service container is required'
ARTEX_ID="${artex_ids[0]}"

if ! label_project="$("$DOCKER_BIN" inspect -f '{{ index .Config.Labels "com.docker.compose.project" }}' "$ARTEX_ID" 2>/dev/null)"; then
  die 'could not inspect ARTEX Compose ownership'
fi
[[ "$label_project" == "$PROJECT" ]] || die 'ARTEX service ownership did not match the requested Compose project'

if ! network_rows="$("$DOCKER_BIN" inspect -f '{{range $name, $cfg := .NetworkSettings.Networks}}{{printf "%s\t%s\t%s\n" $name $cfg.IPAddress $cfg.Gateway}}{{end}}' "$ARTEX_ID" 2>/dev/null)"; then
  die 'could not inspect ARTEX network metadata'
fi
mapfile -t networks < <(printf '%s\n' "$network_rows" | sed '/^$/d')
((${#networks[@]} == 1)) || die 'ARTEX must be attached to exactly one network'
IFS=$'\t' read -r NETWORK ARTEX_IP GATEWAY <<<"${networks[0]}"
[[ -n "$NETWORK" && -n "$ARTEX_IP" && -n "$GATEWAY" ]] || die 'ARTEX network metadata was incomplete'
is_private_ipv4 "$ARTEX_IP" || die 'ARTEX container address is not private IPv4'
is_private_ipv4 "$GATEWAY" || die 'ARTEX bridge gateway is not private IPv4'
[[ "$ARTEX_IP" != "$GATEWAY" ]] || die 'ARTEX container address equals bridge gateway'

driver="$("$DOCKER_BIN" network inspect -f '{{.Driver}}' "$NETWORK" 2>/dev/null)" || die 'could not inspect ARTEX network driver'
[[ "$driver" == 'bridge' ]] || die 'ARTEX network must use Docker bridge driver'
# A normal Compose-created user-defined bridge often does not populate
# com.docker.network.bridge.name.  Docker still names that bridge from the
# first 12 hexadecimal characters of its immutable network ID.  Accept the
# explicit option when Docker provides it, otherwise derive only that canonical
# name and immediately prove both the interface and its gateway below.  Never
# accept an arbitrary host interface (for example docker0, WireGuard, or a
# public NIC) as a fallback.
bridge_option="$("$DOCKER_BIN" network inspect -f '{{index .Options "com.docker.network.bridge.name"}}' "$NETWORK" 2>/dev/null)" || die 'could not inspect ARTEX bridge interface'
if [[ -n "$bridge_option" && "$bridge_option" != '<no value>' ]]; then
  BRIDGE_IFACE="$bridge_option"
  # Preserve supported explicit Docker bridge names.  The interface/gateway
  # proof below remains mandatory, so this does not accept docker0, WireGuard,
  # or a public NIC merely because its name resembles a Docker bridge.
  [[ "$BRIDGE_IFACE" =~ ^br-[A-Za-z0-9]+$ ]] || die 'ARTEX bridge interface is not a user-defined bridge name'
else
  network_id="$("$DOCKER_BIN" network inspect -f '{{.Id}}' "$NETWORK" 2>/dev/null)" || die 'could not inspect ARTEX network ID'
  [[ "$network_id" =~ ^[0-9a-f]{64}$ ]] || die 'ARTEX network ID is not a canonical Docker ID'
  BRIDGE_IFACE="br-${network_id:0:12}"
  [[ "$BRIDGE_IFACE" =~ ^br-[0-9a-f]{12}$ ]] || die 'derived ARTEX bridge interface is not canonical'
fi
"$IP_BIN" link show dev "$BRIDGE_IFACE" >/dev/null 2>&1 || die 'ARTEX bridge interface does not exist on this host'
assert_bridge_gateway_is_assigned
  assert_proxy_port_is_privileged

if ! network_ids_raw="$("$DOCKER_BIN" network inspect -f '{{range $id, $container := .Containers}}{{println $id}}{{end}}' "$NETWORK" 2>/dev/null)"; then
  die 'could not inspect ARTEX network members'
fi
mapfile -t network_ids < <(printf '%s\n' "$network_ids_raw" | sed '/^$/d')
((${#network_ids[@]} > 0)) || die 'ARTEX network has no container members'
for container_id in "${network_ids[@]}"; do
  member_project="$("$DOCKER_BIN" inspect -f '{{ index .Config.Labels "com.docker.compose.project" }}' "$container_id" 2>/dev/null)" || die 'could not inspect ARTEX network member ownership'
  [[ "$member_project" == "$PROJECT" ]] || die 'ARTEX bridge has a container outside the requested Compose project'
done

readonly -a JUMP_RULE=(-d "$GATEWAY" -p tcp -m tcp --dport "$PROXY_PORT" -m comment --comment "$JUMP_COMMENT" -j "$CHAIN")
readonly -a ALLOW_RULE=(-i "$BRIDGE_IFACE" -s "$ARTEX_IP/32" -d "$GATEWAY" -p tcp -m tcp --dport "$PROXY_PORT" -m comment --comment "$ALLOW_COMMENT" -j ACCEPT)
readonly -a DROP_RULE=(-d "$GATEWAY" -p tcp -m tcp --dport "$PROXY_PORT" -m comment --comment "$DROP_COMMENT" -j DROP)

validate_existing_chain

input_rules="$("$IPTABLES_BIN" -w -S INPUT 2>/dev/null)" || die 'could not inspect INPUT rules'
reject_unowned_endpoint_rules
jump_mentions="$(printf '%s\n' "$input_rules" | grep -F -- "$JUMP_COMMENT" || true)"
jump_count="$(count_lines "$jump_mentions")"
((jump_count <= 1)) || die 'multiple owned INPUT jumps exist; refusing to guess'
if ((jump_count == 1)); then
  "$IPTABLES_BIN" -w -C INPUT "${JUMP_RULE[@]}" >/dev/null 2>&1 || die 'owned INPUT jump has unexpected semantics'
fi

info "compose_project=$PROJECT"
info "artex_network=$NETWORK"
info "artex_container_ipv4=$ARTEX_IP"
info "artex_network_gateway=$GATEWAY"
info "artex_bridge_interface=$BRIDGE_IFACE"
info "artex_hermes_proxy_port=$PROXY_PORT"
info "owned_chain_exists=$chain_exists"
info "owned_input_jump_count=$jump_count"
info "plan=allow ${ARTEX_IP}/32 via ${BRIDGE_IFACE} to ${GATEWAY}:$PROXY_PORT, then drop all other INPUT to that endpoint"

if (( ! APPLY )); then
  info 'result=PLAN_ONLY; pass --apply as root only after owner approval'
  exit 0
fi

[[ ${EUID:-$("$ID_BIN" -u)} -eq 0 ]] || die '--apply requires root'
assert_no_port_listener
prepare_route_path

created_chain=0
created_jump=0
route_tmp=''
trap rollback_on_error EXIT

if (( ! chain_exists )); then
  "$IPTABLES_BIN" -w -N "$CHAIN"
  created_chain=1
  "$IPTABLES_BIN" -w -A "$CHAIN" "${ALLOW_RULE[@]}"
  "$IPTABLES_BIN" -w -A "$CHAIN" "${DROP_RULE[@]}"
fi
if (( jump_count == 0 )); then
  "$IPTABLES_BIN" -w -I INPUT 1 "${JUMP_RULE[@]}"
  created_jump=1
fi

route_tmp="$("$MKTEMP_BIN" "$route_dir/.route.XXXXXX")"
printf 'ARTEX_HERMES_BRIDGE_GATEWAY=%s\n' "$GATEWAY" >"$route_tmp"
printf 'ARTEX_HERMES_CONTAINER_IPV4=%s\n' "$ARTEX_IP" >>"$route_tmp"
printf 'ARTEX_HERMES_BRIDGE_INTERFACE=%s\n' "$BRIDGE_IFACE" >>"$route_tmp"
printf 'ARTEX_HERMES_PROXY_PORT=%s\n' "$PROXY_PORT" >>"$route_tmp"
"$CHOWN_BIN" root:root -- "$route_tmp"
"$CHMOD_BIN" 0644 -- "$route_tmp"
"$MV_BIN" -f -- "$route_tmp" "$ROUTE_FILE"
trap - EXIT
info 'result=APPLIED; no proxy was started'
