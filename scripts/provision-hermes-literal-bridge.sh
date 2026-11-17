#!/usr/bin/env bash
# Install (but never enable or start) the reviewed local Hermes bridge files.
set -Eeuo pipefail
IFS=$'\n\t'
export LC_ALL=C
PATH='/usr/sbin:/usr/bin:/sbin:/bin'
unset BASH_ENV ENV CDPATH DOCKER_CONFIG DOCKER_CONTEXT DOCKER_HOST COMPOSE_PROJECT_NAME

SOURCE_PATH="${BASH_SOURCE[0]:-}"
[[ -n "$SOURCE_PATH" ]] || {
  printf '[provision-hermes-literal-bridge] error: run this script from a file, not stdin\n' >&2
  exit 2
}
SCRIPT_DIR="$(cd -- "$(dirname -- "$SOURCE_PATH")" && pwd -P)"
readonly SCRIPT_DIR
readonly HELPER_SOURCE="$SCRIPT_DIR/libexec/artex-hermes-literal-bridge-firewall.sh"
readonly UNIT_DIR="$SCRIPT_DIR/../systemd/hermes-literal-bridge"
readonly HELPER_TARGET='/usr/local/libexec/artex-hermes-literal-bridge-firewall'
readonly FIREWALL_UNIT_TARGET='/etc/systemd/system/artex-hermes-literal-bridge-firewall.service'
readonly PROXY_UNIT_TARGET='/etc/systemd/system/artex-hermes-subscription-proxy.service'
readonly GETENT_BIN='/usr/bin/getent'
readonly ID_BIN='/usr/bin/id'
readonly STAT_BIN='/usr/bin/stat'
readonly MKTEMP_BIN='/usr/bin/mktemp'
readonly RM_BIN='/usr/bin/rm'
readonly SHA256SUM_BIN='/usr/bin/sha256sum'
readonly SYSTEMCTL_BIN='/usr/bin/systemctl'
readonly TMP_ROOT='/run'

HERMES_USER=''
HERMES_BIN=''
HERMES_HOME=''
HERMES_GROUP=''
HERMES_UID=''
PROJECT='artex'
APPLY=0
created_helper=0
created_firewall_unit=0
created_proxy_unit=0
daemon_reload_started=0
firewall_tmp=''
proxy_tmp=''

info() { printf '[provision-hermes-literal-bridge] %s\n' "$*"; }
die() {
  printf '[provision-hermes-literal-bridge] error: %s\n' "$*" >&2
  exit 2
}

usage() {
  cat <<'EOF'
Usage:
  sudo scripts/provision-hermes-literal-bridge.sh \
    --user HERMES_USER --hermes-bin /absolute/path/to/hermes [--project artex] [--apply]

Without --apply, print the installation plan and write nothing. With --apply,
copy a root-owned firewall helper and two systemd unit files, then run only
systemctl daemon-reload. It deliberately does not enable, start, restart, or
stop either service; does not run Hermes; and does not apply firewall rules.

The firewall helper itself remains plan-only until a separate root invocation
uses its own --apply flag after a fresh topology/owner review.
EOF
}

plan() {
  printf '[provision-hermes-literal-bridge] plan:'
  printf ' %q' "$@"
  printf '\n'
}

run() {
  if (( APPLY )); then
    "$@"
  else
    plan "$@"
  fi
}

escape_sed() {
  printf '%s' "$1" | sed 's/[\\&|]/\\&/g'
}

ensure_parent() {
  local directory="$1" metadata owner mode
  [[ ! -L "$directory" ]] || die "refusing symlinked system directory: $directory"
  if [[ -e "$directory" ]]; then
    [[ -d "$directory" ]] || die "system path exists but is not a directory: $directory"
    metadata="$("$STAT_BIN" -c '%u:%a' -- "$directory" 2>/dev/null)" || die "could not inspect system directory: $directory"
    owner="${metadata%%:*}"
    mode="${metadata#*:}"
    [[ "$owner" == '0' && "$mode" =~ ^[0-7]{3,4}$ ]] || die "system directory has unsafe owner or mode: $directory"
    (( (8#$mode & 8#022) == 0 )) || die "system directory is group- or world-writable: $directory"
    return 0
  fi
  run install -d -o root -g root -m 0755 -- "$directory"
}

assert_secure_path_ancestry() {
  local path="$1" label="$2" current metadata owner mode
  current="$path"
  while :; do
    [[ -e "$current" && ! -L "$current" ]] || die "$label path component is missing or symlinked: $current"
    metadata="$("$STAT_BIN" -c '%u:%a' -- "$current" 2>/dev/null)" || die "could not inspect $label path component: $current"
    IFS=':' read -r owner mode <<<"$metadata"
    [[ "$owner" =~ ^[0-9]+$ && "$mode" =~ ^[0-7]{3,4}$ ]] || die "$label path component had invalid metadata: $current"
    (( (8#$mode & 8#022) == 0 )) || die "$label path component is group- or world-writable: $current"
    if [[ "$current" == "$path" ]]; then
      [[ "$owner" == '0' || "$owner" == "$HERMES_UID" ]] || die "$label must be owned by root or the Hermes user"
    fi
    [[ "$current" == '/' ]] && break
    current="$(dirname -- "$current")"
  done
}

rollback_install() {
  local status=$?
  trap - EXIT
  if (( status != 0 )); then
    if (( created_proxy_unit )); then
      "$RM_BIN" -f -- "$PROXY_UNIT_TARGET" >/dev/null 2>&1 || true
    fi
    if (( created_firewall_unit )); then
      "$RM_BIN" -f -- "$FIREWALL_UNIT_TARGET" >/dev/null 2>&1 || true
    fi
    if (( created_helper )); then
      "$RM_BIN" -f -- "$HELPER_TARGET" >/dev/null 2>&1 || true
    fi
    "$RM_BIN" -f -- "${firewall_tmp:-}" "${proxy_tmp:-}" >/dev/null 2>&1 || true
    if (( daemon_reload_started )); then
      "$SYSTEMCTL_BIN" daemon-reload >/dev/null 2>&1 || true
    fi
  fi
  exit "$status"
}

print_source_receipt() {
  local source_file source_hash
  [[ -x "$SHA256SUM_BIN" ]] || die "required source-hash tool is unavailable: $SHA256SUM_BIN"
  for source_file in "$HELPER_SOURCE" "$UNIT_DIR/artex-hermes-literal-bridge-firewall.service.in" "$UNIT_DIR/artex-hermes-subscription-proxy.service.in"; do
    source_hash="$("$SHA256SUM_BIN" -- "$source_file")"
    source_hash="${source_hash%%[[:space:]]*}"
    [[ "$source_hash" =~ ^[0-9a-f]{64}$ ]] || die "could not calculate a safe source hash for: ${source_file##*/}"
    info "source_sha256 file=${source_file##*/} sha256=$source_hash"
  done
}

while (($#)); do
  case "$1" in
    --user)
      (($# >= 2)) || die '--user requires a value'
      HERMES_USER="$2"
      shift 2
      ;;
    --hermes-bin)
      (($# >= 2)) || die '--hermes-bin requires a value'
      HERMES_BIN="$2"
      shift 2
      ;;
    --project)
      (($# >= 2)) || die '--project requires a value'
      PROJECT="$2"
      shift 2
      ;;
    --apply)
      APPLY=1
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

[[ "$HERMES_USER" =~ ^[a-z_][a-z0-9_-]*\$?$ ]] || die '--user must be a local UNIX account name'
[[ "$HERMES_BIN" == /* && "$HERMES_BIN" =~ ^[A-Za-z0-9_./+-]+$ ]] || die '--hermes-bin must be an absolute safe path'
[[ "$PROJECT" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*$ ]] || die '--project has unsafe syntax'
for source_file in "$HELPER_SOURCE" "$UNIT_DIR/artex-hermes-literal-bridge-firewall.service.in" "$UNIT_DIR/artex-hermes-subscription-proxy.service.in"; do
  [[ -f "$source_file" && ! -L "$source_file" ]] || die "required source is missing or symlinked: $source_file"
done
print_source_receipt

if (( APPLY )); then
  [[ ${EUID:-$("$ID_BIN" -u)} -eq 0 ]] || die '--apply requires root'
  [[ -x "$GETENT_BIN" && -x "$ID_BIN" && -x "$STAT_BIN" && -x "$MKTEMP_BIN" && -x "$RM_BIN" && -x "$SYSTEMCTL_BIN" ]] || die 'required account/filesystem tools are unavailable'
  HERMES_UID="$("$ID_BIN" -u -- "$HERMES_USER" 2>/dev/null)" || die "user does not exist: $HERMES_USER"
  [[ "$HERMES_UID" =~ ^[0-9]+$ && "$HERMES_UID" != '0' ]] || die '--user must not be root'
  [[ -f "$HERMES_BIN" && -x "$HERMES_BIN" && ! -L "$HERMES_BIN" ]] || die '--hermes-bin must be an executable regular non-symlink file'
  passwd_record="$("$GETENT_BIN" passwd "$HERMES_USER" 2>/dev/null)" || die "could not determine home directory for: $HERMES_USER"
  IFS=':' read -r _ _ _ _ _ HERMES_HOME _ <<<"$passwd_record"
  [[ "$HERMES_HOME" == /* && "$HERMES_HOME" =~ ^[A-Za-z0-9_./+-]+$ && -d "$HERMES_HOME" && ! -L "$HERMES_HOME" ]] || die 'Hermes user home must be an existing non-symlink safe absolute path'
  HERMES_HOME="$(cd -- "$HERMES_HOME" && pwd -P)"
  HERMES_GROUP="$("$ID_BIN" -gn -- "$HERMES_USER" 2>/dev/null)" || die "could not determine primary group for: $HERMES_USER"
  [[ "$HERMES_GROUP" =~ ^[a-z_][a-z0-9_-]*\$?$ ]] || die 'Hermes user primary group has unsafe syntax'
  assert_secure_path_ancestry "$HERMES_HOME" 'Hermes home'
  assert_secure_path_ancestry "$HERMES_BIN" 'Hermes executable'
fi

for target in "$HELPER_TARGET" "$FIREWALL_UNIT_TARGET" "$PROXY_UNIT_TARGET"; do
  [[ ! -e "$target" && ! -L "$target" ]] || die "refusing to overwrite existing managed path: $target"
done

if (( APPLY )); then
  created_helper=0
  created_firewall_unit=0
  created_proxy_unit=0
  daemon_reload_started=0
  firewall_tmp=''
  proxy_tmp=''
  trap rollback_install EXIT
  [[ -d "$TMP_ROOT" && ! -L "$TMP_ROOT" ]] || die 'safe runtime temporary directory is unavailable'
  firewall_tmp="$("$MKTEMP_BIN" "$TMP_ROOT/artex-hermes-firewall.XXXXXX")"
  proxy_tmp="$("$MKTEMP_BIN" "$TMP_ROOT/artex-hermes-proxy.XXXXXX")"
  project_escaped="$(escape_sed "$PROJECT")"
  user_escaped="$(escape_sed "$HERMES_USER")"
  home_escaped="$(escape_sed "$HERMES_HOME")"
  group_escaped="$(escape_sed "$HERMES_GROUP")"
  bin_escaped="$(escape_sed "$HERMES_BIN")"
  sed "s|__ARTEX_PROJECT__|$project_escaped|g" "$UNIT_DIR/artex-hermes-literal-bridge-firewall.service.in" >"$firewall_tmp"
  sed -e "s|__HERMES_USER__|$user_escaped|g" -e "s|__HERMES_HOME__|$home_escaped|g" -e "s|__HERMES_GROUP__|$group_escaped|g" -e "s|__HERMES_BIN__|$bin_escaped|g" \
    "$UNIT_DIR/artex-hermes-subscription-proxy.service.in" >"$proxy_tmp"

  ensure_parent /usr/local/libexec
  ensure_parent /etc/systemd/system
  install -o root -g root -m 0755 -- "$HELPER_SOURCE" "$HELPER_TARGET"
  created_helper=1
  install -o root -g root -m 0644 -- "$firewall_tmp" "$FIREWALL_UNIT_TARGET"
  created_firewall_unit=1
  install -o root -g root -m 0644 -- "$proxy_tmp" "$PROXY_UNIT_TARGET"
  created_proxy_unit=1
  daemon_reload_started=1
  "$SYSTEMCTL_BIN" daemon-reload
  trap - EXIT
  "$RM_BIN" -f -- "$firewall_tmp" "$proxy_tmp"
  info 'installed only; services remain disabled and stopped'
else
  ensure_parent /usr/local/libexec
  ensure_parent /etc/systemd/system
  info 'dry-run; no files will be created'
  plan install -o root -g root -m 0755 -- "$HELPER_SOURCE" "$HELPER_TARGET"
  plan install -o root -g root -m 0644 -- "$UNIT_DIR/artex-hermes-literal-bridge-firewall.service.in" "$FIREWALL_UNIT_TARGET"
  plan install -o root -g root -m 0644 -- "$UNIT_DIR/artex-hermes-subscription-proxy.service.in" "$PROXY_UNIT_TARGET"
  plan "$SYSTEMCTL_BIN" daemon-reload
  info 'dry-run complete; --apply stages files plus daemon-reload only'
fi
