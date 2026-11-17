#!/usr/bin/env bash
# Offline contract test for the Hermes Subscription Proxy topology preflight.
# It uses fake docker/ss commands only; no Docker daemon, Hermes CLI, network,
# provider, credential, or ARTEX service is contacted.
set -Eeuo pipefail
IFS=$'\n\t'

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)" || {
  printf 'could not resolve script directory\n' >&2
  exit 1
}
readonly SCRIPT_DIR
readonly PREFLIGHT="$SCRIPT_DIR/preflight-hermes-subscription-proxy.sh"

[[ -x "$PREFLIGHT" ]] || {
  printf 'missing executable preflight: %s\n' "$PREFLIGHT" >&2
  exit 1
}

TEST_DIR="$(mktemp -d "${TMPDIR:-/tmp}/artex-hermes-proxy-preflight-test.XXXXXX")"
trap 'rm -rf -- "$TEST_DIR"' EXIT
PROJECT_DIR="$TEST_DIR/project"
FAKE_BIN="$TEST_DIR/bin"
mkdir -p -- "$PROJECT_DIR" "$FAKE_BIN"
: >"$PROJECT_DIR/docker-compose.yml"

fail() {
  printf 'test-preflight-hermes-subscription-proxy: %s\n' "$*" >&2
  exit 1
}

cat >"$FAKE_BIN/docker" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail

scenario="${PREFLIGHT_FIXTURE:-unsafe}"
case "$1" in
  compose)
    shift
    case "$1" in
      ps) printf 'artex-id\n' ;;
      exec)
        if [[ "$scenario" == safe ]]; then
          printf '172.18.0.1 STREAM host.docker.internal\n'
        else
          printf '172.17.0.1 STREAM host.docker.internal\n'
        fi
        ;;
      *) exit 64 ;;
    esac
    ;;
  inspect)
    format="$3"
    target="${!#}"
    case "$format:$target" in
      *NetworkSettings.Networks*:artex-id) printf 'artex_default\t172.18.0.3\t172.18.0.1\n' ;;
      *Config.Labels*:artex-id|*Config.Labels*:artex-db-id) printf 'artex\n' ;;
      *Config.Labels*:foreign-id) printf 'other\n' ;;
      *) exit 64 ;;
    esac
    ;;
  network)
    shift
    case "$1" in
      ls) printf 'bridge\nartex_default\n' ;;
      inspect)
        format="$3"
        network="$4"
        case "$format:$network" in
          *IPAM.Config*:bridge) printf '172.17.0.1\n' ;;
          *IPAM.Config*:artex_default) printf '172.18.0.1\n' ;;
          *Containers*:bridge)
            if [[ "$scenario" == unsafe ]]; then
              printf 'foreign-id\n'
            fi
            ;;
          *Containers*:artex_default) printf 'artex-id\nartex-db-id\n' ;;
          *) exit 64 ;;
        esac
        ;;
      *) exit 64 ;;
    esac
    ;;
  *) exit 64 ;;
esac
EOF
chmod 0700 "$FAKE_BIN/docker"

cat >"$FAKE_BIN/ss" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
if [[ "${PREFLIGHT_LISTENER:-0}" == 1 ]]; then
  printf 'LISTEN 0 4096 172.18.0.1:8645 0.0.0.0:*\n'
fi
EOF
chmod 0700 "$FAKE_BIN/ss"

run_preflight() {
  local scenario="$1" listener="$2" output status
  set +e
  output="$(PATH="$FAKE_BIN:$PATH" PREFLIGHT_FIXTURE="$scenario" PREFLIGHT_LISTENER="$listener" \
    "$PREFLIGHT" --project-dir "$PROJECT_DIR" 2>&1)"
  status=$?
  set -e
  printf '%s\n' "$output"
  return "$status"
}

set +e
unsafe_output="$(run_preflight unsafe 0)"
unsafe_status=$?
set -e
((unsafe_status == 2)) || fail "unsafe alias topology returned $unsafe_status, want 2"
[[ "$unsafe_output" == *'artex_network_gateway=172.18.0.1'* ]] || fail 'unsafe fixture omitted ARTEX gateway'
[[ "$unsafe_output" == *'host_alias_ipv4=172.17.0.1'* ]] || fail 'unsafe fixture omitted alias resolution'
[[ "$unsafe_output" == *'foreign_containers_on_host_alias_network=1'* ]] || fail 'unsafe fixture did not count foreign alias container'
[[ "$unsafe_output" == *'result=BLOCKED'* ]] || fail 'unsafe alias topology was not blocked'
[[ "$unsafe_output" != *'suggested_profile_base_url='* ]] || fail 'blocked topology emitted a candidate profile URL'
[[ "$unsafe_output" != *'artex-id'* && "$unsafe_output" != *'artex-db-id'* && "$unsafe_output" != *'foreign-id'* ]] ||
  fail 'preflight leaked a container identifier'

set +e
safe_output="$(run_preflight safe 0)"
safe_status=$?
set -e
((safe_status == 0)) || fail "safe topology returned $safe_status, want 0"
[[ "$safe_output" == *'host_alias_ipv4=172.18.0.1'* ]] || fail 'safe fixture did not use ARTEX gateway alias'
[[ "$safe_output" == *'foreign_containers_on_artex_network=0'* ]] || fail 'safe fixture reported a foreign ARTEX container'
[[ "$safe_output" == *'tcp_8645_listener_count=0'* ]] || fail 'safe fixture did not report a free proxy port'
[[ "$safe_output" == *'result=TOPOLOGY_REVIEW_REQUIRED'* ]] || fail 'safe fixture did not retain manual-review state'
[[ "$safe_output" == *'suggested_profile_base_url=http://172.18.0.1:8645/v1'* ]] || fail 'safe fixture omitted literal candidate URL'

# Docker Compose exec must not inherit the script's own stdin. This keeps a
# safe `bash -s` diagnostic from feeding the rest of its script to the ARTEX
# container, and verifies the no-BASH_SOURCE fallback used by that form.
set +e
stdin_output="$(PATH="$FAKE_BIN:$PATH" PREFLIGHT_FIXTURE=safe PREFLIGHT_LISTENER=0 \
  bash -s -- --project-dir "$PROJECT_DIR" <"$PREFLIGHT" 2>&1)"
stdin_status=$?
set -e
((stdin_status == 0)) || fail "stdin invocation returned $stdin_status, want 0"
[[ "$stdin_output" == *'result=TOPOLOGY_REVIEW_REQUIRED'* ]] || fail 'stdin invocation did not finish the preflight'

set +e
listener_output="$(run_preflight safe 1)"
listener_status=$?
set -e
((listener_status == 2)) || fail "existing listener returned $listener_status, want 2"
[[ "$listener_output" == *'tcp_8645_listener_count=1'* ]] || fail 'listener fixture did not report port use'
[[ "$listener_output" == *'result=BLOCKED'* ]] || fail 'existing listener was not blocked'

printf 'test-preflight-hermes-subscription-proxy: PASS\n'
