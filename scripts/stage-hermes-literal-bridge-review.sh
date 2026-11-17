#!/usr/bin/env bash
# Stage non-executable review material for ARTEX's optional literal-bridge
# Hermes proxy design. This is deliberately not a deployment tool.
set -Eeuo pipefail
IFS=$'\n\t'
export LC_ALL=C
PATH='/usr/sbin:/usr/bin:/sbin:/bin'
unset BASH_ENV ENV CDPATH DOCKER_CONFIG DOCKER_CONTEXT DOCKER_HOST COMPOSE_PROJECT_NAME

SOURCE_PATH="${BASH_SOURCE[0]:-}"
[[ -n "$SOURCE_PATH" ]] || {
  printf '[stage-hermes-literal-bridge-review] error: run this script from a file, not stdin\n' >&2
  exit 2
}
SCRIPT_DIR="$(cd -- "$(dirname -- "$SOURCE_PATH")" && pwd -P)"
readonly SCRIPT_DIR
TEMPLATE_DIR="$SCRIPT_DIR/../systemd/hermes-literal-bridge-review"
readonly TEMPLATE_DIR

STAGE_DIR=''
APPLY=0

info() { printf '[stage-hermes-literal-bridge-review] %s\n' "$*"; }
die() {
  printf '[stage-hermes-literal-bridge-review] error: %s\n' "$*" >&2
  exit 2
}

usage() {
  cat <<'EOF'
Usage:
  scripts/stage-hermes-literal-bridge-review.sh --stage-dir ABSOLUTE_DIR [--apply]

Copies non-executable review templates into a non-system directory owned by
the invoking non-root user. Without --apply it prints the exact staging plan
and writes nothing.

This helper never runs Docker, Hermes, systemd, a firewall command, or a
provider request. It refuses root and /etc, /usr, /var, and /run destinations.
It does not create an ARTEX LLM profile or enable agent execution.

The staged *.template files contain deliberately unresolved placeholders. They
are review inputs, not units that may be copied into /etc/systemd/system.
EOF
}

plan() {
  printf '[stage-hermes-literal-bridge-review] plan:'
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

while (($#)); do
  case "$1" in
    --stage-dir)
      (($# >= 2)) || die '--stage-dir requires a value'
      STAGE_DIR="$2"
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

[[ ${EUID:-$(id -u)} -ne 0 ]] || die 'refusing root: this helper only stages untrusted review material'
[[ -n "$STAGE_DIR" ]] || die '--stage-dir is required'
[[ "$STAGE_DIR" == /* ]] || die '--stage-dir must be an absolute path'

case "$STAGE_DIR" in
  /bin|/bin/*|/boot|/boot/*|/etc|/etc/*|/lib|/lib/*|/lib64|/lib64/*|/opt|/opt/*|/root|/root/*|/run|/run/*|/sbin|/sbin/*|/srv|/srv/*|/usr|/usr/*|/var|/var/*)
    die '--stage-dir must not use a protected system prefix'
    ;;
esac

[[ -d "$TEMPLATE_DIR" && ! -L "$TEMPLATE_DIR" ]] || die 'review template directory is missing or symlinked'

readonly -a TEMPLATE_NAMES=(
  README.md
  artex-hermes-proxy-firewall.service.template
  artex-hermes-subscription-proxy.service.template
)

for template_name in "${TEMPLATE_NAMES[@]}"; do
  template_path="$TEMPLATE_DIR/$template_name"
  [[ -f "$template_path" && ! -L "$template_path" ]] || die "review template is missing or symlinked: $template_name"
done

STAGE_PARENT="$(dirname -- "$STAGE_DIR")"
STAGE_LEAF="${STAGE_DIR##*/}"
[[ -n "$STAGE_LEAF" && "$STAGE_LEAF" != '.' && "$STAGE_LEAF" != '..' ]] || die '--stage-dir must name a new directory'
[[ -d "$STAGE_PARENT" && ! -L "$STAGE_PARENT" ]] || die "stage parent is missing or symlinked: $STAGE_PARENT"
STAGE_PARENT="$(cd -- "$STAGE_PARENT" && pwd -P)"
STAGE_DIR="$STAGE_PARENT/$STAGE_LEAF"
[[ ! -e "$STAGE_DIR" && ! -L "$STAGE_DIR" ]] || die "stage directory must not already exist: $STAGE_DIR"

for template_name in "${TEMPLATE_NAMES[@]}"; do
  target_path="$STAGE_DIR/$template_name"
  [[ ! -e "$target_path" && ! -L "$target_path" ]] || die "refusing to overwrite staged file: $target_path"
done

if (( ! APPLY )); then
  info 'dry-run; no files will be created'
fi
run install -d -m 0700 -- "$STAGE_DIR"
for template_name in "${TEMPLATE_NAMES[@]}"; do
  run install -m 0644 -- "$TEMPLATE_DIR/$template_name" "$STAGE_DIR/$template_name"
done

if (( APPLY )); then
  info "review templates staged at $STAGE_DIR"
  info 'nothing was enabled, started, queried, or connected'
else
  info 'dry-run complete; pass --apply only to stage files in the requested non-system directory'
fi
