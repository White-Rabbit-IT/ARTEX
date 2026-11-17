#!/usr/bin/env bash
# Provision the narrow filesystem boundary used by the optional Hermes observer.
# This script intentionally does not start Docker, Hermes, ARTEX, or a service.
# It only creates a root-owned producer directory and grants one non-root Hermes
# user read/search access through a dedicated group.
set -Eeuo pipefail
IFS=$'\n\t'
PATH='/usr/sbin:/usr/bin:/sbin:/bin'

readonly OUTBOX_DIR='/var/lib/artex-hermes-observer'
readonly OBSERVER_GROUP='artex-observer'

HERMES_USER=''
DRY_RUN=0

info() { printf '[provision-hermes-observer] %s\n' "$*"; }
die() { printf '[provision-hermes-observer] error: %s\n' "$*" >&2; exit 2; }

usage() {
  cat <<'EOF'
Usage:
  sudo scripts/provision-hermes-observer.sh --user HERMES_USER [--dry-run]

Creates this dedicated directory tree:

  /var/lib/artex-hermes-observer          root:artex-observer 0750
  /var/lib/artex-hermes-observer/pending  root:artex-observer 2750

The selected non-root user is added to group artex-observer. Group members can
read/list pending metadata receipts, but cannot create, rename, delete, chmod,
or replace ARTEX receipt files. The script does not inspect Hermes credentials,
does not open a network port, and does not start ARTEX or Hermes.
EOF
}

run() {
  if (( DRY_RUN )); then
    printf '[provision-hermes-observer] dry-run:'
    printf ' %q' "$@"
    printf '\n'
    return 0
  fi
  "$@"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --user)
      [[ $# -ge 2 && -n "${2:-}" ]] || die '--user requires a value'
      HERMES_USER="$2"
      shift 2
      ;;
    --dry-run)
      DRY_RUN=1
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

[[ ${EUID:-$(id -u)} -eq 0 ]] || die 'run this provisioning step with sudo/root'
[[ "$HERMES_USER" =~ ^[a-z_][a-z0-9_-]*\$?$ ]] || die '--user must be a local UNIX account name'

user_uid="$(id -u -- "$HERMES_USER" 2>/dev/null)" || die "user does not exist: $HERMES_USER"
[[ "$user_uid" != '0' ]] || die '--user must be the non-root Hermes-login owner'

if ! getent group "$OBSERVER_GROUP" >/dev/null; then
  run groupadd --system "$OBSERVER_GROUP"
fi
run usermod -a -G "$OBSERVER_GROUP" "$HERMES_USER"

# /var/lib is a root-owned system location. Refuse a pre-existing symlink rather
# than following an operator typo into another filesystem tree.
[[ ! -L "$OUTBOX_DIR" ]] || die "refusing symlinked observer directory: $OUTBOX_DIR"
if [[ -e "$OUTBOX_DIR" && ! -d "$OUTBOX_DIR" ]]; then
  die "observer path exists but is not a directory: $OUTBOX_DIR"
fi
# Do not let install(1) follow a pre-existing child symlink either.  This is
# especially important because this provisioning helper normally runs as root.
[[ ! -L "$OUTBOX_DIR/pending" ]] || die "refusing symlinked observer pending directory: $OUTBOX_DIR/pending"
run install -d -o root -g "$OBSERVER_GROUP" -m 0750 -- "$OUTBOX_DIR"
run install -d -o root -g "$OBSERVER_GROUP" -m 2750 -- "$OUTBOX_DIR/pending"
# install(1) is affected by a caller umask on some platforms; explicitly retain
# the no-write group boundary and the setgid inheritance bit for receipt files.
run chown root:"$OBSERVER_GROUP" -- "$OUTBOX_DIR" "$OUTBOX_DIR/pending"
run chmod 0750 -- "$OUTBOX_DIR"
run chmod 2750 -- "$OUTBOX_DIR/pending"

if (( DRY_RUN )); then
  info 'dry-run complete; no user, group, or filesystem state was changed'
  exit 0
fi

cat <<EOF

Provisioning complete for $HERMES_USER.

The user must start a new login/session before its new group membership is
visible. Then, in the ARTEX checkout's .env, set:

  ARTEX_HERMES_OBSERVER_ENABLE=1
  ARTEX_HERMES_OBSERVER_HOST_DIR=$OUTBOX_DIR

Use the optional Compose overlay (do not replace the normal compose file):

  docker compose -f docker-compose.yml -f docker-compose.hermes-observer.yml config
  docker compose -f docker-compose.yml -f docker-compose.hermes-observer.yml up -d --force-recreate artex

Run the sender as $HERMES_USER, not root. See guides/hermes-observer.md.
EOF
