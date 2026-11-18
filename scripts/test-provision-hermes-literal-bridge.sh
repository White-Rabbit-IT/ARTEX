#!/usr/bin/env bash
# Offline contract test for literal-bridge installation and firewall planning.
# Every external command is a fixture; it never touches Docker, iptables,
# systemd, Hermes, the host firewall, or a provider.
set -Eeuo pipefail
IFS=$'\n\t'

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly SCRIPT_DIR
readonly INSTALLER="$SCRIPT_DIR/provision-hermes-literal-bridge.sh"
readonly HELPER_SOURCE="$SCRIPT_DIR/libexec/artex-hermes-literal-bridge-firewall.sh"

[[ -x "$INSTALLER" && -x "$HELPER_SOURCE" ]] || {
  printf 'missing literal-bridge scripts\n' >&2
  exit 1
}

TEST_DIR="$(mktemp -d "${TMPDIR:-/tmp}/artex-hermes-literal-bridge-test.XXXXXX")"
readonly TEST_DIR
export TEST_DIR
trap 'rm -rf -- "$TEST_DIR"' EXIT
readonly FAKE_BIN="$TEST_DIR/bin"
readonly IPTABLES_STATE="$TEST_DIR/iptables-state"
readonly IPTABLES_LOG="$TEST_DIR/iptables.log"
readonly SYSTEMCTL_LOG="$TEST_DIR/systemctl.log"
readonly INSTALL_LOG="$TEST_DIR/install.log"
export TEST_DIR INSTALL_LOG SYSTEMCTL_LOG
export FAKE_IPTABLES_STATE="$IPTABLES_STATE"
export FAKE_IPTABLES_LOG="$IPTABLES_LOG"
mkdir -p -- "$FAKE_BIN" "$IPTABLES_STATE"

fail() {
  printf 'test-provision-hermes-literal-bridge: %s\n' "$*" >&2
  exit 1
}

cat >"$FAKE_BIN/docker" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
case "$1" in
  ps) printf 'artex-id\n' ;;
  inspect)
    format="$3"
    target="${!#}"
    case "$format:$target" in
      *Config.Labels*:artex-id|*Config.Labels*:postgres-id) printf 'artex\n' ;;
      *Config.Labels*:foreign-id) printf 'other\n' ;;
      *NetworkSettings.Networks*:artex-id) printf 'artex_default\t172.18.0.3\t172.18.0.1\n' ;;
      *) exit 64 ;;
    esac
    ;;
  network)
    shift
    [[ "$1" == inspect ]] || exit 64
    format="$3"
    case "$format" in
      '{{.Driver}}') printf 'bridge\n' ;;
      '{{.Id}}') printf '%s\n' "${FAKE_NETWORK_ID:-37a981071ae89cc0bc84f48fbe0c232b723d996539b96334d6c4646c72006c75}" ;;
      *bridge.name*)
        case "${FIXTURE_BRIDGE_OPTION:-explicit}" in
          explicit) printf 'br-37a981071ae8\n' ;;
          absent) printf '<no value>\n' ;;
          empty) printf '\n' ;;
          *) printf '%s\n' "${FIXTURE_BRIDGE_OPTION}" ;;
        esac
        ;;
      *Containers*)
        if [[ "${FIXTURE_FOREIGN:-0}" == 1 ]]; then
          printf 'artex-id\nforeign-id\n'
        else
          printf 'artex-id\npostgres-id\n'
        fi
        ;;
      *) exit 64 ;;
    esac
    ;;
  *) exit 64 ;;
esac
EOF

cat >"$FAKE_BIN/ip" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
bridge_iface="${FAKE_BRIDGE_IFACE:-br-37a981071ae8}"
case "$*" in
  "link show dev $bridge_iface")
    ;;
  "-4 -o addr show dev $bridge_iface")
    if [[ "${FIXTURE_GATEWAY_ASSIGNED:-1}" == 1 ]]; then
      printf '8: %s    inet 172.18.0.1/16 brd 172.18.255.255 scope global %s\n' "$bridge_iface" "$bridge_iface"
    else
      printf '8: %s    inet 172.18.0.2/16 brd 172.18.255.255 scope global %s\n' "$bridge_iface" "$bridge_iface"
    fi
    ;;
  *)
    exit 64
    ;;
esac
EOF

cat >"$FAKE_BIN/ss" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
if [[ "${FAKE_LISTENER:-0}" == 1 ]]; then
  printf 'LISTEN 0 4096 172.18.0.1:864 0.0.0.0:*\n'
fi
EOF

cat >"$FAKE_BIN/sysctl" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
[[ "$*" == '-n net.ipv4.ip_unprivileged_port_start' ]] || exit 64
printf '%s\n' "${FAKE_UNPRIVILEGED_PORT_START:-1024}"
EOF

cat >"$FAKE_BIN/stat" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
format=''
if [[ "${1:-}" == -c ]]; then
  format="${2:-}"
fi
path="${!#}"
case "$format:$path" in
  '%u:%g:%a':*route.env) printf '0:0:644\n' ;;
  '%u:%g:%a':*) printf '0:0:755\n' ;;
  '%u:%a':*)
    if [[ "${FAKE_UNSAFE_PATH:-0}" == 1 ]]; then
      printf '0:775\n'
    else
      printf '0:755\n'
    fi
    ;;
  *) exit 64 ;;
esac
EOF

cat >"$FAKE_BIN/chown" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
if [[ "${FAKE_CHOWN_FAIL:-0}" == 1 ]]; then
  exit 47
fi
exit 0
EOF

cat >"$FAKE_BIN/mktemp" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
exec /usr/bin/mktemp "${TEST_DIR:?}/tmp.XXXXXX"
EOF

cat >"$FAKE_BIN/install" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'install' >>"${INSTALL_LOG:?}"
printf ' %q' "$@" >>"${INSTALL_LOG:?}"
printf '\n' >>"${INSTALL_LOG:?}"
target="${!#}"
if [[ -n "${FAKE_INSTALL_FAIL_TARGET:-}" && "$target" == *"${FAKE_INSTALL_FAIL_TARGET}" ]]; then
  exit 47
fi
args=()
while (($#)); do
  case "$1" in
    -o|-g)
      shift 2
      ;;
    *)
      args+=("$1")
      shift
      ;;
  esac
done
exec /usr/bin/install "${args[@]}"
EOF

cat >"$FAKE_BIN/systemctl" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
printf '%s\n' "$*" >>"${SYSTEMCTL_LOG:?}"
[[ "$*" == 'daemon-reload' ]]
EOF

cat >"$FAKE_BIN/hermes" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
exit 0
EOF

cat >"$FAKE_BIN/iptables" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
state="${FAKE_IPTABLES_STATE:?}"
log="${FAKE_IPTABLES_LOG:?}"
printf '%s\n' "$*" >>"$log"

join_args() {
  local out='' arg
  for arg in "$@"; do
    if [[ -n "$out" ]]; then out+=' '; fi
    out+="$arg"
  done
  printf '%s\n' "$out"
}

if [[ "${1:-}" == -w ]]; then shift; fi
command="${1:-}"
shift || true
case "$command" in
  -V)
    printf 'iptables v1.8.10 (nf_tables)\n'
    ;;
  -S)
    target="${1:-}"
    if [[ -z "$target" ]]; then
      printf '%s\n' '-P INPUT ACCEPT'
      if [[ -f "$state/input" ]]; then
        cat "$state/input"
      fi
      if [[ -f "$state/chain" ]]; then
        printf '%s\n' '-N ARTEX_HERMES_PROXY'
        cat "$state/chain"
      fi
    elif [[ "$target" == INPUT ]]; then
      printf '%s\n' '-P INPUT ACCEPT'
      if [[ -f "$state/input" ]]; then
        cat "$state/input"
      fi
    elif [[ "$target" == ARTEX_HERMES_PROXY && -f "$state/chain" ]]; then
      printf '%s\n' '-N ARTEX_HERMES_PROXY'
      cat "$state/chain"
    else
      exit 1
    fi
    ;;
  -N)
    [[ "${1:-}" == ARTEX_HERMES_PROXY ]] || exit 64
    : >"$state/chain"
    ;;
  -A)
    target="$1"
    shift
    rule="-A $target $(join_args "$@")"
    [[ "$target" == ARTEX_HERMES_PROXY ]] || exit 64
    printf '%s\n' "$rule" >>"$state/chain"
    ;;
  -I)
    target="$1"
    position="$2"
    shift 2
    [[ "$target" == INPUT && "$position" == 1 ]] || exit 64
    {
      printf '%s\n' "-A INPUT $(join_args "$@")"
      [[ -f "$state/input" ]] && cat -- "$state/input"
    } >"$state/input.next"
    mv -- "$state/input.next" "$state/input"
    ;;
  -C)
    target="$1"
    shift
    wanted="-A $target $(join_args "$@")"
    case "$target" in
      INPUT) [[ -f "$state/input" ]] && grep -Fqx -- "$wanted" "$state/input" ;;
      ARTEX_HERMES_PROXY) [[ -f "$state/chain" ]] && grep -Fqx -- "$wanted" "$state/chain" ;;
      *) exit 1 ;;
    esac
    ;;
  -D)
    target="$1"
    shift
    wanted="-A $target $(join_args "$@")"
    case "$target" in
      INPUT) file="$state/input" ;;
      ARTEX_HERMES_PROXY) file="$state/chain" ;;
      *) exit 64 ;;
    esac
    [[ -f "$file" ]] || exit 1
    if [[ "$target" == ARTEX_HERMES_PROXY && "${FAKE_INJECT_FOREIGN_ON_FIRST_DELETE:-0}" == 1 && ! -e "$state/foreign-injected" ]]; then
      printf '%s\n' '-A ARTEX_HERMES_PROXY -p tcp -m tcp --dport 9999 -m comment --comment unrelated-manager-rule -j ACCEPT' >>"$state/chain"
      : >"$state/foreign-injected"
    fi
    if [[ "$target" == ARTEX_HERMES_PROXY && "${FAKE_INJECT_FOREIGN_INPUT_PREPEND:-0}" == 1 && "$wanted" == *'--comment artex-hermes-literal-bridge-allow'* && ! -e "$state/foreign-injected" ]]; then
      {
        printf '%s\n' '-A INPUT -d 172.18.0.1 -p tcp -m tcp --dport 864 -m comment --comment unrelated-manager-rule -j ACCEPT'
        cat -- "$state/input"
      } >"$state/input.next"
      mv -- "$state/input.next" "$state/input"
      : >"$state/foreign-injected"
    fi
    if [[ "$target" == INPUT && "${FAKE_INJECT_FOREIGN_ON_INPUT_DELETE:-0}" == 1 && "$wanted" == *'--comment artex-hermes-literal-bridge-jump'* && ! -e "$state/foreign-injected" ]]; then
      printf '%s\n' '-A ARTEX_HERMES_PROXY -p tcp -m tcp --dport 9999 -m comment --comment unrelated-manager-rule -j ACCEPT' >>"$state/chain"
      : >"$state/foreign-injected"
    fi
    removed=0
    : >"$file.next"
    while IFS= read -r current || [[ -n "$current" ]]; do
      if [[ "$current" == "$wanted" && "$removed" == 0 ]]; then
        removed=1
        continue
      fi
      printf '%s\n' "$current" >>"$file.next"
    done <"$file"
    (( removed == 1 )) || exit 1
    mv -- "$file.next" "$file"
    ;;
  -X)
    [[ "${1:-}" == ARTEX_HERMES_PROXY ]] || exit 64
    [[ ! -s "$state/chain" ]] || exit 1
    rm -f -- "$state/chain"
    ;;
  *) exit 64 ;;
esac
EOF
chmod 0700 "$FAKE_BIN"/*

FAKE_HELPER_NONROOT="$TEST_DIR/firewall-helper-nonroot"
sed \
  -e "s|readonly DOCKER_BIN='/usr/bin/docker'|readonly DOCKER_BIN='$FAKE_BIN/docker'|" \
  -e "s|readonly IPTABLES_BIN='/usr/sbin/iptables'|readonly IPTABLES_BIN='$FAKE_BIN/iptables'|" \
  -e "s|readonly IP_BIN='/usr/sbin/ip'|readonly IP_BIN='$FAKE_BIN/ip'|" \
  -e "s|readonly SS_BIN='/usr/bin/ss'|readonly SS_BIN='$FAKE_BIN/ss'|" \
  -e "s|readonly SYSCTL_BIN='/usr/sbin/sysctl'|readonly SYSCTL_BIN='$FAKE_BIN/sysctl'|" \
  -e "s|readonly INSTALL_BIN='/usr/bin/install'|readonly INSTALL_BIN='$FAKE_BIN/install'|" \
  -e "s|readonly CHOWN_BIN='/usr/bin/chown'|readonly CHOWN_BIN='$FAKE_BIN/chown'|" \
  -e "s|readonly STAT_BIN='/usr/bin/stat'|readonly STAT_BIN='$FAKE_BIN/stat'|" \
  -e "s|readonly ROUTE_FILE='[^']*'|readonly ROUTE_FILE='$TEST_DIR/runtime/route.env'|" \
  "$HELPER_SOURCE" >"$FAKE_HELPER_NONROOT"
chmod 0700 "$FAKE_HELPER_NONROOT"

set +e
nonroot_output="$("$FAKE_HELPER_NONROOT" --project artex --apply 2>&1)"
nonroot_status=$?
set -e
(( nonroot_status != 0 )) || fail 'unmodified helper accepted non-root apply'
[[ "$nonroot_output" == *'--apply requires root'* ]] || fail 'non-root apply did not explain refusal'
[[ ! -e "$IPTABLES_STATE/chain" && ! -e "$IPTABLES_STATE/input" ]] || fail 'non-root apply mutated fixture firewall state'

FAKE_HELPER="$TEST_DIR/firewall-helper"
sed "/--apply requires root/s|^.*$|true|" "$FAKE_HELPER_NONROOT" >"$FAKE_HELPER"
chmod 0700 "$FAKE_HELPER"

run_helper() {
  "$FAKE_HELPER" "$@"
}

plan_output="$(run_helper --project artex 2>&1)"
[[ "$plan_output" == *'artex_network_gateway=172.18.0.1'* ]] || fail 'safe fixture omitted gateway'
[[ "$plan_output" == *'artex_bridge_interface=br-37a981071ae8'* ]] || fail 'safe fixture omitted bridge'
[[ "$plan_output" == *'artex_hermes_proxy_port=864'* ]] || fail 'safe fixture omitted the privileged Hermes port'
[[ "$plan_output" == *'result=PLAN_ONLY'* ]] || fail 'helper did not default to plan-only'
[[ "$plan_output" != *'artex-id'* && "$plan_output" != *'postgres-id'* ]] || fail 'helper leaked container identifiers'
[[ ! -e "$TEST_DIR/runtime" ]] || fail 'plan mode created a route directory'

default_bridge_output="$(FIXTURE_BRIDGE_OPTION=absent run_helper --project artex 2>&1)"
[[ "$default_bridge_output" == *'artex_bridge_interface=br-37a981071ae8'* ]] || fail 'default Compose bridge was not derived from Docker network ID'
[[ "$default_bridge_output" == *'result=PLAN_ONLY'* ]] || fail 'default Compose bridge did not stay plan-only'

empty_option_output="$(FIXTURE_BRIDGE_OPTION=empty run_helper --project artex 2>&1)"
[[ "$empty_option_output" == *'artex_bridge_interface=br-37a981071ae8'* ]] || fail 'empty Compose bridge option was not derived from Docker network ID'

explicit_custom_bridge_output="$(FIXTURE_BRIDGE_OPTION=br-artex FAKE_BRIDGE_IFACE=br-artex run_helper --project artex 2>&1)"
[[ "$explicit_custom_bridge_output" == *'artex_bridge_interface=br-artex'* ]] || fail 'explicit custom Docker bridge was not retained'
[[ "$explicit_custom_bridge_output" == *'result=PLAN_ONLY'* ]] || fail 'explicit custom Docker bridge did not stay plan-only'

set +e
malformed_network_id_output="$(FIXTURE_BRIDGE_OPTION=absent FAKE_NETWORK_ID=not-a-docker-id run_helper --project artex 2>&1)"
malformed_network_id_status=$?
set -e
(( malformed_network_id_status != 0 )) || fail 'malformed Docker network ID was accepted for bridge derivation'
[[ "$malformed_network_id_output" == *'network ID is not a canonical Docker ID'* ]] || fail 'malformed Docker network ID did not explain refusal'
[[ ! -e "$TEST_DIR/runtime" ]] || fail 'malformed Docker network ID created a route directory'

set +e
gateway_output="$(FIXTURE_GATEWAY_ASSIGNED=0 run_helper --project artex 2>&1)"
gateway_status=$?
set -e
(( gateway_status != 0 )) || fail 'unassigned Docker gateway was accepted'
[[ "$gateway_output" == *'not assigned to the discovered bridge'* ]] || fail 'gateway mismatch did not explain refusal'

set +e
foreign_output="$(FIXTURE_FOREIGN=1 run_helper --project artex 2>&1)"
foreign_status=$?
set -e
(( foreign_status != 0 )) || fail 'foreign network member was accepted'
[[ "$foreign_output" == *'outside the requested Compose project'* ]] || fail 'foreign topology did not explain refusal'

set +e
port_boundary_output="$(FAKE_UNPRIVILEGED_PORT_START=864 run_helper --project artex 2>&1)"
port_boundary_status=$?
set -e
(( port_boundary_status != 0 )) || fail 'a non-privileged proxy port was accepted'
[[ "$port_boundary_output" == *'not privileged on this host'* ]] || fail 'unsafe port boundary did not explain refusal'

apply_output="$(run_helper --project artex --apply 2>&1)"
[[ "$apply_output" == *'result=APPLIED'* ]] || fail 'fixture apply did not report success'
[[ -f "$IPTABLES_STATE/chain" && -f "$IPTABLES_STATE/input" ]] || fail 'fixture apply did not create owned rules'
mapfile -t chain_lines <"$IPTABLES_STATE/chain"
[[ ${#chain_lines[@]} -eq 2 ]] || fail 'owned chain did not contain exactly two rules'
[[ "${chain_lines[0]}" == *'artex-hermes-literal-bridge-allow'* && "${chain_lines[0]}" == *'-j ACCEPT' ]] || fail 'allow rule was not first'
[[ "${chain_lines[1]}" == *'artex-hermes-literal-bridge-drop'* && "${chain_lines[1]}" == *'-j DROP' ]] || fail 'drop rule was not second'
grep -Fq -- '-I INPUT 1' "$IPTABLES_LOG" || fail 'INPUT jump was not inserted at position one'
if grep -Fq -- '-A INPUT ' "$IPTABLES_LOG"; then
  fail 'helper appended an INPUT rule'
fi
grep -Fx -- 'ARTEX_HERMES_BRIDGE_GATEWAY=172.18.0.1' "$TEST_DIR/runtime/route.env" >/dev/null || fail 'route file was not atomically written'
grep -Fx -- 'ARTEX_HERMES_CONTAINER_IPV4=172.18.0.3' "$TEST_DIR/runtime/route.env" >/dev/null || fail 'route file omitted the ARTEX container address'
grep -Fx -- 'ARTEX_HERMES_BRIDGE_INTERFACE=br-37a981071ae8' "$TEST_DIR/runtime/route.env" >/dev/null || fail 'route file omitted the ARTEX bridge interface'
grep -Fx -- 'ARTEX_HERMES_PROXY_PORT=864' "$TEST_DIR/runtime/route.env" >/dev/null || fail 'route file omitted the privileged Hermes port'

mutations_before="$(grep -Ec '(^| )-(N|A|I)( |$)' "$IPTABLES_LOG" || true)"
run_helper --project artex --apply >/dev/null
mutations_after="$(grep -Ec '(^| )-(N|A|I)( |$)' "$IPTABLES_LOG" || true)"
[[ "$mutations_before" == "$mutations_after" ]] || fail 'second apply duplicated firewall rules'

set +e
removal_listener_output="$(FAKE_LISTENER=1 run_helper --project artex --remove --apply 2>&1)"
removal_listener_status=$?
set -e
(( removal_listener_status != 0 )) || fail 'teardown accepted a still-listening privileged endpoint'
[[ "$removal_listener_output" == *'already listening'* ]] || fail 'teardown listener refusal did not explain the active endpoint'
[[ -f "$IPTABLES_STATE/chain" && -s "$IPTABLES_STATE/input" && -f "$TEST_DIR/runtime/route.env" ]] || fail 'teardown listener refusal altered the active boundary'

set +e
listener_output="$(FAKE_LISTENER=1 run_helper --project artex --apply 2>&1)"
listener_status=$?
set -e
(( listener_status != 0 )) || fail 'existing TCP/864 listener was accepted'
[[ "$listener_output" == *'already listening'* ]] || fail 'listener collision did not explain refusal'
mutations_after_listener="$(grep -Ec '(^| )-(N|A|I)( |$)' "$IPTABLES_LOG" || true)"
[[ "$mutations_after_listener" == "$mutations_after" ]] || fail 'listener collision mutated firewall'

readonly UNOWNED_INPUT_RULE='-A INPUT -d 172.18.0.1/32 -p tcp -m tcp --dport 864 -j ACCEPT'
printf '%s\n' "$UNOWNED_INPUT_RULE" >>"$IPTABLES_STATE/input"
set +e
collision_output="$(run_helper --project artex 2>&1)"
collision_status=$?
set -e
(( collision_status != 0 )) || fail 'unowned endpoint rule was accepted'
[[ "$collision_output" == *'unowned INPUT rule'* ]] || fail 'unowned rule did not explain refusal'
grep -Fvx -- "$UNOWNED_INPUT_RULE" "$IPTABLES_STATE/input" >"$IPTABLES_STATE/input.next"
mv -- "$IPTABLES_STATE/input.next" "$IPTABLES_STATE/input"

removal_plan="$(run_helper --project artex --remove 2>&1)"
[[ "$removal_plan" == *'owned_chain_exists=1'* ]] || fail 'teardown plan did not recognize the owned chain'
[[ "$removal_plan" == *'owned_input_jump_count=1'* ]] || fail 'teardown plan did not recognize the owned INPUT jump'
[[ "$removal_plan" == *'owned_route_file_exists=1'* ]] || fail 'teardown plan did not recognize the route file'
[[ "$removal_plan" == *'result=PLAN_ONLY'* ]] || fail 'teardown default was not plan-only'
[[ -f "$IPTABLES_STATE/chain" && -s "$IPTABLES_STATE/input" ]] || fail 'teardown plan mutated firewall state'

removal_output="$(run_helper --project artex --remove --apply 2>&1)"
[[ "$removal_output" == *'result=REMOVED'* ]] || fail 'teardown apply did not report removal'
[[ ! -e "$IPTABLES_STATE/chain" ]] || fail 'teardown did not remove the owned chain'
[[ ! -s "$IPTABLES_STATE/input" ]] || fail 'teardown did not remove the owned INPUT jump'
[[ ! -e "$TEST_DIR/runtime/route.env" ]] || fail 'teardown did not remove the generated route file'
grep -Fq -- '-D INPUT -d 172.18.0.1 -p tcp -m tcp --dport 864 -m comment --comment artex-hermes-literal-bridge-jump -j ARTEX_HERMES_PROXY' "$IPTABLES_LOG" || fail 'teardown did not remove the exact owned INPUT jump'
grep -Fq -- '-D ARTEX_HERMES_PROXY -d 172.18.0.1 -p tcp -m tcp --dport 864 -m comment --comment artex-hermes-literal-bridge-drop -j DROP' "$IPTABLES_LOG" || fail 'teardown did not remove the exact owned drop rule'
grep -Fq -- '-D ARTEX_HERMES_PROXY -i br-37a981071ae8 -s 172.18.0.3/32 -d 172.18.0.1 -p tcp -m tcp --dport 864 -m comment --comment artex-hermes-literal-bridge-allow -j ACCEPT' "$IPTABLES_LOG" || fail 'teardown did not remove the exact owned allow rule'
grep -Fq -- '-X ARTEX_HERMES_PROXY' "$IPTABLES_LOG" || fail 'teardown did not remove the validated owned chain'
if grep -Fq -- '-F ARTEX_HERMES_PROXY' "$IPTABLES_LOG"; then
  fail 'teardown used a broad owned-chain flush'
fi

# Simulate another firewall manager adding a rule after the helper has inspected
# its chain but before it starts exact deletes. The helper may leave a degraded
# owned route for a human to resolve, but it must never erase the foreign rule.
run_helper --project artex --apply >/dev/null
set +e
race_output="$(FAKE_INJECT_FOREIGN_ON_FIRST_DELETE=1 run_helper --project artex --remove --apply 2>&1)"
race_status=$?
set -e
(( race_status != 0 )) || fail 'teardown accepted a concurrent foreign chain mutation'
[[ "$race_output" != *'result=REMOVED'* ]] || fail 'foreign chain mutation reported a successful teardown'
[[ -f "$TEST_DIR/runtime/route.env" ]] || fail 'failed teardown removed its route record after a foreign mutation'
grep -Fqx -- '-A INPUT -d 172.18.0.1 -p tcp -m tcp --dport 864 -m comment --comment artex-hermes-literal-bridge-jump -j ARTEX_HERMES_PROXY' "$IPTABLES_STATE/input" || fail 'foreign-mutation teardown removed the restrictive INPUT jump'
grep -Fqx -- '-A ARTEX_HERMES_PROXY -d 172.18.0.1 -p tcp -m tcp --dport 864 -m comment --comment artex-hermes-literal-bridge-drop -j DROP' "$IPTABLES_STATE/chain" || fail 'foreign-mutation teardown removed the terminal drop'
[[ "$(sed -n '1p' "$IPTABLES_STATE/input")" == '-A INPUT -d 172.18.0.1 -p tcp -m tcp --dport 864 -m comment --comment artex-hermes-literal-bridge-teardown-drop -j DROP' ]] || fail 'foreign-mutation teardown did not retain the top-priority temporary drop guard'
if grep -Fq -- 'artex-hermes-literal-bridge-allow' "$IPTABLES_STATE/chain"; then
  fail 'foreign-mutation teardown restored an allow rule after entering restrictive state'
fi
grep -Fqx -- '-A ARTEX_HERMES_PROXY -p tcp -m tcp --dport 9999 -m comment --comment unrelated-manager-rule -j ACCEPT' "$IPTABLES_STATE/chain" || fail 'teardown erased a foreign chain rule'
if grep -Fq -- '-F ARTEX_HERMES_PROXY' "$IPTABLES_LOG"; then
  fail 'foreign-mutation teardown used a broad owned-chain flush'
fi
grep -Fvx -- '-A ARTEX_HERMES_PROXY -p tcp -m tcp --dport 9999 -m comment --comment unrelated-manager-rule -j ACCEPT' "$IPTABLES_STATE/chain" >"$IPTABLES_STATE/chain.next" || true
mv -- "$IPTABLES_STATE/chain.next" "$IPTABLES_STATE/chain"
rm -f -- "$IPTABLES_STATE/foreign-injected"
run_helper --project artex --remove --apply >/dev/null
[[ ! -e "$IPTABLES_STATE/chain" && ! -s "$IPTABLES_STATE/input" && ! -e "$TEST_DIR/runtime/route.env" ]] || fail 'teardown did not recover after a foreign-rule refusal was cleared'

# This is the narrow final-window case: a foreign chain rule arrives after the
# helper's most recent inspection, while it is deleting the ordinary INPUT
# jump. The temporary guard must remain first and keep TCP/864 denied even
# though the ordinary jump/drop can no longer be retained as a pair.
run_helper --project artex --apply >/dev/null
set +e
late_race_output="$(FAKE_INJECT_FOREIGN_ON_INPUT_DELETE=1 run_helper --project artex --remove --apply 2>&1)"
late_race_status=$?
set -e
(( late_race_status != 0 )) || fail 'teardown accepted a final-window foreign mutation'
[[ "$late_race_output" != *'result=REMOVED'* ]] || fail 'final-window foreign mutation reported a successful teardown'
[[ -f "$TEST_DIR/runtime/route.env" ]] || fail 'final-window foreign mutation removed the route record'
[[ "$(sed -n '1p' "$IPTABLES_STATE/input")" == '-A INPUT -d 172.18.0.1 -p tcp -m tcp --dport 864 -m comment --comment artex-hermes-literal-bridge-teardown-drop -j DROP' ]] || fail 'final-window foreign mutation did not retain the top-priority drop guard'
grep -Fqx -- '-A INPUT -d 172.18.0.1 -p tcp -m tcp --dport 864 -m comment --comment artex-hermes-literal-bridge-teardown-drop -j DROP' "$IPTABLES_STATE/input" || fail 'final-window foreign mutation removed the exact temporary drop guard'
grep -Fqx -- '-A ARTEX_HERMES_PROXY -p tcp -m tcp --dport 9999 -m comment --comment unrelated-manager-rule -j ACCEPT' "$IPTABLES_STATE/chain" || fail 'final-window foreign mutation erased the foreign chain rule'
if grep -Fq -- '-F ARTEX_HERMES_PROXY' "$IPTABLES_LOG"; then
  fail 'final-window foreign-mutation teardown used a broad owned-chain flush'
fi
grep -Fvx -- '-A ARTEX_HERMES_PROXY -p tcp -m tcp --dport 9999 -m comment --comment unrelated-manager-rule -j ACCEPT' "$IPTABLES_STATE/chain" >"$IPTABLES_STATE/chain.next" || true
mv -- "$IPTABLES_STATE/chain.next" "$IPTABLES_STATE/chain"
rm -f -- "$IPTABLES_STATE/foreign-injected"
run_helper --project artex --remove --apply >/dev/null
[[ ! -e "$IPTABLES_STATE/chain" && ! -s "$IPTABLES_STATE/input" && ! -e "$TEST_DIR/runtime/route.env" ]] || fail 'teardown did not resume from the exact guarded final-window state'

# A foreign ACCEPT can also be inserted at INPUT position one after the guard
# was inspected but while the helper is deleting its allow rule. The EXIT path
# must prepend a second exact guard above it; after the foreign rule is removed,
# the helper can consume the bounded duplicate-guard state safely.
run_helper --project artex --apply >/dev/null
set +e
input_prepend_output="$(FAKE_INJECT_FOREIGN_INPUT_PREPEND=1 run_helper --project artex --remove --apply 2>&1)"
input_prepend_status=$?
set -e
(( input_prepend_status != 0 )) || fail 'teardown accepted a final-window foreign INPUT prepend'
[[ "$input_prepend_output" == *'temporary teardown guard is not first in INPUT'* ]] || fail 'foreign INPUT prepend did not explain the refusal'
[[ -f "$TEST_DIR/runtime/route.env" ]] || fail 'foreign INPUT prepend removed the route record'
[[ "$(sed -n '1p' "$IPTABLES_STATE/input")" == '-A INPUT -d 172.18.0.1 -p tcp -m tcp --dport 864 -m comment --comment artex-hermes-literal-bridge-teardown-drop -j DROP' ]] || fail 'foreign INPUT prepend did not restore a top-priority drop guard'
[[ "$(grep -Fc -- '-A INPUT -d 172.18.0.1 -p tcp -m tcp --dport 864 -m comment --comment artex-hermes-literal-bridge-teardown-drop -j DROP' "$IPTABLES_STATE/input")" == '2' ]] || fail 'foreign INPUT prepend did not retain a bounded duplicate-guard state'
grep -Fqx -- '-A INPUT -d 172.18.0.1 -p tcp -m tcp --dport 864 -m comment --comment unrelated-manager-rule -j ACCEPT' "$IPTABLES_STATE/input" || fail 'foreign INPUT prepend was erased'
grep -Fvx -- '-A INPUT -d 172.18.0.1 -p tcp -m tcp --dport 864 -m comment --comment unrelated-manager-rule -j ACCEPT' "$IPTABLES_STATE/input" >"$IPTABLES_STATE/input.next"
mv -- "$IPTABLES_STATE/input.next" "$IPTABLES_STATE/input"
rm -f -- "$IPTABLES_STATE/foreign-injected"
run_helper --project artex --remove --apply >/dev/null
[[ ! -e "$IPTABLES_STATE/chain" && ! -s "$IPTABLES_STATE/input" && ! -e "$TEST_DIR/runtime/route.env" ]] || fail 'teardown did not recover from the guarded foreign INPUT prepend state'

# A clean-state port collision must fail before the helper creates any owned
# rule. This complements the above collision check against an existing route.
printf '%s\n' "$UNOWNED_INPUT_RULE" >"$IPTABLES_STATE/input"
set +e
clean_collision_output="$(run_helper --project artex --apply 2>&1)"
clean_collision_status=$?
set -e
(( clean_collision_status != 0 )) || fail 'clean-state unowned endpoint rule was accepted'
[[ "$clean_collision_output" == *'unowned INPUT rule'* ]] || fail 'clean-state collision did not explain refusal'
[[ ! -e "$IPTABLES_STATE/chain" ]] || fail 'clean-state collision created an owned chain'
[[ "$(cat "$IPTABLES_STATE/input")" == "$UNOWNED_INPUT_RULE" ]] || fail 'clean-state collision altered the foreign rule'
rm -f -- "$IPTABLES_STATE/input"

set +e
clean_listener_output="$(FAKE_LISTENER=1 run_helper --project artex --apply 2>&1)"
clean_listener_status=$?
set -e
(( clean_listener_status != 0 )) || fail 'clean-state listener collision was accepted'
[[ "$clean_listener_output" == *'already listening'* ]] || fail 'clean-state listener collision did not explain refusal'
[[ ! -e "$IPTABLES_STATE/chain" && ! -e "$IPTABLES_STATE/input" ]] || fail 'clean-state listener collision mutated firewall state'

# Make the final route-file ownership step fail after a chain/jump was created.
# The helper must roll back exactly its newly-created state and publish no route.
set +e
rollback_output="$(FAKE_CHOWN_FAIL=1 run_helper --project artex --apply 2>&1)"
rollback_status=$?
set -e
(( rollback_status != 0 )) || fail 'forced route-file ownership failure was accepted'
[[ "$rollback_output" != *'result=APPLIED'* ]] || fail 'route-file ownership failure reported a successful apply'
[[ ! -e "$IPTABLES_STATE/chain" && ! -s "$IPTABLES_STATE/input" ]] || fail 'route-file failure did not roll back owned firewall state'
[[ ! -e "$TEST_DIR/runtime/route.env" ]] || fail 'route-file failure published a route'

# Teardown must refuse malformed or duplicated owned state rather than using a
# broad flush. Restore the fixture manually only after proving the refusal.
run_helper --project artex --apply >/dev/null
cp -- "$TEST_DIR/runtime/route.env" "$TEST_DIR/runtime/route.env.good"
{
  printf '%s\n' 'ARTEX_HERMES_BRIDGE_GATEWAY=172.18.0.9'
  printf '%s\n' 'ARTEX_HERMES_CONTAINER_IPV4=172.18.0.3'
  printf '%s\n' 'ARTEX_HERMES_BRIDGE_INTERFACE=br-37a981071ae8'
  printf '%s\n' 'ARTEX_HERMES_PROXY_PORT=864'
} >"$TEST_DIR/runtime/route.env"
set +e
route_mismatch_output="$(run_helper --project artex --remove 2>&1)"
route_mismatch_status=$?
set -e
(( route_mismatch_status != 0 )) || fail 'teardown accepted a route record that did not match its owned rules'
[[ "$route_mismatch_output" == *'unexpected allow rule'* ]] || fail 'route mismatch did not explain refusal'
[[ -s "$IPTABLES_STATE/input" && -s "$IPTABLES_STATE/chain" ]] || fail 'route mismatch teardown check mutated firewall state'
mv -- "$TEST_DIR/runtime/route.env.good" "$TEST_DIR/runtime/route.env"

cp -- "$IPTABLES_STATE/chain" "$IPTABLES_STATE/chain.good"
{
  sed -n '2p' "$IPTABLES_STATE/chain.good"
  sed -n '1p' "$IPTABLES_STATE/chain.good"
} >"$IPTABLES_STATE/chain"
set +e
malformed_output="$(run_helper --project artex --remove 2>&1)"
malformed_status=$?
set -e
(( malformed_status != 0 )) || fail 'malformed owned chain was accepted for teardown'
[[ "$malformed_output" == *'unexpected allow rule'* ]] || fail 'malformed chain did not explain refusal'
[[ -s "$IPTABLES_STATE/input" && -s "$IPTABLES_STATE/chain" ]] || fail 'malformed teardown check mutated firewall state'
mv -- "$IPTABLES_STATE/chain.good" "$IPTABLES_STATE/chain"

cp -- "$IPTABLES_STATE/input" "$IPTABLES_STATE/input.one"
cat -- "$IPTABLES_STATE/input.one" >>"$IPTABLES_STATE/input"
set +e
duplicate_output="$(run_helper --project artex --remove 2>&1)"
duplicate_status=$?
set -e
(( duplicate_status != 0 )) || fail 'duplicate owned INPUT jump was accepted for teardown'
[[ "$duplicate_output" == *'exactly one removable INPUT jump'* ]] || fail 'duplicate INPUT jump did not explain refusal'
[[ "$(wc -l <"$IPTABLES_STATE/input")" == '2' ]] || fail 'duplicate teardown check altered firewall state'
mv -- "$IPTABLES_STATE/input.one" "$IPTABLES_STATE/input"

run_helper --project artex --remove --apply >/dev/null
[[ ! -e "$IPTABLES_STATE/chain" && ! -s "$IPTABLES_STATE/input" && ! -e "$TEST_DIR/runtime/route.env" ]] || fail 'final teardown did not leave an empty owned state'

FAKE_ROOT="$TEST_DIR/installer-root"
FAKE_INSTALLER="$TEST_DIR/installer"
current_user="$(id -un)"
sed \
  -e "/^readonly HELPER_SOURCE=/c\\readonly HELPER_SOURCE='$HELPER_SOURCE'" \
  -e "/^readonly UNIT_DIR=/c\\readonly UNIT_DIR='$SCRIPT_DIR/../systemd/hermes-literal-bridge'" \
  -e "s|readonly HELPER_TARGET='[^']*'|readonly HELPER_TARGET='$FAKE_ROOT/libexec/artex-hermes-literal-bridge-firewall'|" \
  -e "s|readonly FIREWALL_UNIT_TARGET='[^']*'|readonly FIREWALL_UNIT_TARGET='$FAKE_ROOT/systemd/artex-hermes-literal-bridge-firewall.service'|" \
  -e "s|readonly PROXY_UNIT_TARGET='[^']*'|readonly PROXY_UNIT_TARGET='$FAKE_ROOT/systemd/artex-hermes-subscription-proxy.service'|" \
  -e "s|readonly STAT_BIN='/usr/bin/stat'|readonly STAT_BIN='$FAKE_BIN/stat'|" \
  -e "s|readonly MKTEMP_BIN='/usr/bin/mktemp'|readonly MKTEMP_BIN='$FAKE_BIN/mktemp'|" \
  -e "s|readonly SYSTEMCTL_BIN='/usr/bin/systemctl'|readonly SYSTEMCTL_BIN='$FAKE_BIN/systemctl'|" \
  -e "s|PATH='/usr/sbin:/usr/bin:/sbin:/bin'|PATH='$FAKE_BIN:/usr/sbin:/usr/bin:/sbin:/bin'|" \
  -e "s|ensure_parent /usr/local/libexec|ensure_parent '$FAKE_ROOT/libexec'|" \
  -e "s|ensure_parent /etc/systemd/system|ensure_parent '$FAKE_ROOT/systemd'|" \
  -e "/--apply requires root/s|^.*$|true|" \
  "$INSTALLER" >"$FAKE_INSTALLER"
chmod 0700 "$FAKE_INSTALLER"

run_installer() {
  "$FAKE_INSTALLER" --user "$current_user" --hermes-bin "$FAKE_BIN/hermes" --project artex "$@"
}

installer_plan="$(run_installer 2>&1)"
[[ "$installer_plan" == *'dry-run; no files will be created'* ]] || fail 'installer did not default to dry-run'
[[ "$installer_plan" == *'source_sha256 file=artex-hermes-literal-bridge-firewall.sh sha256='* ]] || fail 'installer dry-run did not emit the helper source receipt'
[[ "$installer_plan" == *'source_sha256 file=artex-hermes-literal-bridge-firewall.service.in sha256='* ]] || fail 'installer dry-run did not emit the firewall-unit source receipt'
[[ "$installer_plan" == *'source_sha256 file=artex-hermes-subscription-proxy.service.in sha256='* ]] || fail 'installer dry-run did not emit the proxy-unit source receipt'
[[ ! -e "$FAKE_ROOT" ]] || fail 'installer dry-run created files'

set +e
unsafe_path_output="$(FAKE_UNSAFE_PATH=1 run_installer --apply 2>&1)"
unsafe_path_status=$?
set -e
(( unsafe_path_status != 0 )) || fail 'installer accepted a group-writable Hermes path component'
[[ "$unsafe_path_output" == *'group- or world-writable'* ]] || fail 'unsafe Hermes path did not explain refusal'
[[ ! -e "$FAKE_ROOT/libexec/artex-hermes-literal-bridge-firewall" && ! -e "$FAKE_ROOT/systemd/artex-hermes-literal-bridge-firewall.service" && ! -e "$FAKE_ROOT/systemd/artex-hermes-subscription-proxy.service" ]] || fail 'unsafe Hermes path wrote a managed target'

set +e
partial_install_output="$(FAKE_INSTALL_FAIL_TARGET='artex-hermes-subscription-proxy.service' run_installer --apply 2>&1)"
partial_install_status=$?
set -e
(( partial_install_status != 0 )) || fail 'installer accepted a forced final-target install failure'
[[ "$partial_install_output" != *'installed only; services remain disabled and stopped'* ]] || fail 'partial installer failure reported a successful install'
[[ ! -e "$FAKE_ROOT/libexec/artex-hermes-literal-bridge-firewall" && ! -e "$FAKE_ROOT/systemd/artex-hermes-literal-bridge-firewall.service" && ! -e "$FAKE_ROOT/systemd/artex-hermes-subscription-proxy.service" ]] || fail 'partial installer failure left a managed target behind'
[[ ! -s "$SYSTEMCTL_LOG" ]] || fail 'partial installer failure unexpectedly reloaded systemd'

run_installer --apply >/dev/null
[[ -x "$FAKE_ROOT/libexec/artex-hermes-literal-bridge-firewall" ]] || fail 'installer did not stage root helper'
[[ -f "$FAKE_ROOT/systemd/artex-hermes-literal-bridge-firewall.service" ]] || fail 'installer did not stage firewall unit'
[[ -f "$FAKE_ROOT/systemd/artex-hermes-subscription-proxy.service" ]] || fail 'installer did not stage proxy unit'
[[ "$(cat "$SYSTEMCTL_LOG")" == 'daemon-reload' ]] || fail 'installer invoked a systemctl action other than daemon-reload'
if grep -Eq '(^|[[:space:]])(enable|start|restart|stop)([[:space:]]|$)' "$SYSTEMCTL_LOG"; then
  fail 'installer activated or interrupted a unit'
fi
if grep -Fq -- '__' "$FAKE_ROOT/systemd/artex-hermes-literal-bridge-firewall.service" || grep -Fq -- '__' "$FAKE_ROOT/systemd/artex-hermes-subscription-proxy.service"; then
  fail 'installer left unresolved unit placeholders'
fi
grep -Fq -- 'BindsTo=artex-hermes-literal-bridge-firewall.service' "$FAKE_ROOT/systemd/artex-hermes-subscription-proxy.service" || fail 'proxy unit is not bound to firewall unit'

firewall_unit="$FAKE_ROOT/systemd/artex-hermes-literal-bridge-firewall.service"
proxy_unit="$FAKE_ROOT/systemd/artex-hermes-subscription-proxy.service"
grep -Fx -- 'Requires=docker.service' "$firewall_unit" >/dev/null || fail 'firewall unit lacks Docker requirement'
grep -Fx -- 'BindsTo=docker.service' "$firewall_unit" >/dev/null || fail 'firewall unit does not stop with Docker'
grep -Fx -- 'After=docker.service' "$firewall_unit" >/dev/null || fail 'firewall unit lacks Docker ordering'
grep -Fx -- 'Before=artex-hermes-subscription-proxy.service' "$firewall_unit" >/dev/null || fail 'firewall unit lacks proxy ordering'
grep -F -- 'ExecStop=/usr/local/libexec/artex-hermes-literal-bridge-firewall --project artex --remove --apply' "$firewall_unit" >/dev/null || fail 'firewall unit lacks a narrow teardown hook'
grep -Fx -- 'RuntimeDirectory=artex-hermes-literal-bridge' "$firewall_unit" >/dev/null || fail 'firewall unit lacks dedicated runtime directory'
grep -Fx -- 'ReadWritePaths=/run/artex-hermes-literal-bridge /run/xtables.lock' "$firewall_unit" >/dev/null || fail 'firewall unit has an incomplete write boundary'
grep -Fx -- 'EnvironmentFile=/run/artex-hermes-literal-bridge/route.env' "$proxy_unit" >/dev/null || fail 'proxy unit does not consume the generated route'
grep -F -- "ExecStart=$FAKE_BIN/hermes proxy start --provider nous --host \${ARTEX_HERMES_BRIDGE_GATEWAY} --port \${ARTEX_HERMES_PROXY_PORT}" "$proxy_unit" >/dev/null || fail 'proxy unit endpoint is not derived from the route file'
grep -Fx -- 'AmbientCapabilities=CAP_NET_BIND_SERVICE' "$proxy_unit" >/dev/null || fail 'proxy unit does not retain the minimum privileged-bind capability'
grep -Fx -- 'CapabilityBoundingSet=CAP_NET_BIND_SERVICE' "$proxy_unit" >/dev/null || fail 'proxy unit capability bounding set is broader than the privileged-bind capability'
if grep -Eq -- '--host[[:space:]]+(0\.0\.0\.0|10\.|172\.(1[6-9]|2[0-9]|3[0-1])\.|192\.168\.)' "$proxy_unit"; then
  fail 'proxy unit renders a literal public, wildcard, or private host'
fi

if grep -Eq 'systemctl[[:space:]]+(enable|start|restart|stop)' "$INSTALLER"; then
  fail 'installer source contains service activation'
fi
if grep -Eq '(iptables-save|iptables-restore|nftables\.conf|DOCKER-)' "$HELPER_SOURCE"; then
  fail 'helper source contains prohibited global firewall/Docker-chain control'
fi

printf 'test-provision-hermes-literal-bridge: PASS\n'
