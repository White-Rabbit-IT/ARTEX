#!/usr/bin/env bash
# Install ARTEX from White-Rabbit-IT/ARTEX on a supported Ubuntu 24.04 host.
#
# Default deployment: Docker Engine + Docker Compose, bound to localhost only.
# This script does not run scans or configure targets; use ARTEX only against
# systems for which you have explicit authorization.
set -Eeuo pipefail
IFS=$'\n\t'

readonly REPOSITORY_URL="https://github.com/White-Rabbit-IT/ARTEX.git"
readonly DEFAULT_REF="main"

INSTALL_DIR="${ARTEX_DIR:-$HOME/ARTEX}"
REF="${ARTEX_REF:-$DEFAULT_REF}"
BIND_ADDR="${ARTEX_BIND_ADDR:-127.0.0.1}"
DRY_RUN=0
NO_START=0
SKIP_DOCKER_INSTALL=0
UPDATE_EXISTING=0
BIND_EXPLICIT=0
DOCKER_GROUP_ADDED=0
DOCKER_CMD=()

info() { printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok() { printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn() { printf '\033[33m[!]\033[0m %s\n' "$*" >&2; }
die() { printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'EOF'
Usage:
  scripts/install-ubuntu-24.04.sh [options]

Installs ARTEX with Docker Engine and Docker Compose on Ubuntu 24.04 LTS.
The default deployment binds the ARTEX UI to 127.0.0.1:8787.

Options:
  --dir DIR              Checkout location (default: ~/ARTEX)
  --ref REF              Branch or tag to clone (default: main)
  --bind ADDRESS         ARTEX_BIND_ADDR for a newly created .env
                         (127.0.0.1 or 0.0.0.0)
  --update               Fast-forward a clean, existing checkout on --ref
  --skip-docker-install  Fail if a usable Docker Engine + Compose v2 is absent
  --no-start             Install/configure only; do not build or start ARTEX
  --dry-run              Print privileged, Git and Compose commands without running them
  --help                 Show this help

Environment equivalents:
  ARTEX_DIR, ARTEX_REF, ARTEX_BIND_ADDR

The script never overwrites an existing .env, checkout, data/, keys/, or skills/
directory. Docker package conflicts are reported rather than removed automatically.
EOF
}

print_command() {
  printf '+'
  printf ' %q' "$@"
  printf '\n'
}

run() {
  if (( DRY_RUN )); then
    print_command "$@"
  else
    "$@"
  fi
}

run_sudo() {
  if (( DRY_RUN )); then
    print_command sudo "$@"
  else
    sudo "$@"
  fi
}

require_sudo() {
  (( DRY_RUN == 0 )) || return 0
  command -v sudo >/dev/null 2>&1 || die "sudo is required; run this as a sudo-enabled, non-root user"
  sudo -v
}

require_ubuntu_2404() {
  [[ -r /etc/os-release ]] || die "cannot read /etc/os-release"
  # shellcheck disable=SC1091
  . /etc/os-release
  [[ "${ID:-}" == "ubuntu" && "${VERSION_ID:-}" == "24.04" ]] ||
    die "this installer supports Ubuntu 24.04 LTS only (detected: ${PRETTY_NAME:-unknown})"

  case "$(dpkg --print-architecture)" in
    amd64|arm64) ;;
    *) die "unsupported architecture: $(dpkg --print-architecture); ARTEX Docker builds support amd64 and arm64" ;;
  esac
}

validate_bind_address() {
  case "$BIND_ADDR" in
    127.0.0.1|0.0.0.0) ;;
    *) die "--bind must be either 127.0.0.1 or 0.0.0.0; ARTEX's current Compose port mapping is IPv4-only" ;;
  esac

  case "$BIND_ADDR" in
    0.0.0.0)
      warn "ARTEX will be reachable beyond localhost. Docker-published ports can bypass ordinary UFW rules."
      warn "Put ARTEX behind an authenticated TLS reverse proxy before exposing it publicly."
      ;;
  esac
}

installed_package() {
  dpkg-query -W -f='${db:Status-Status}' "$1" 2>/dev/null | grep -qx installed
}

ensure_base_packages() {
  run_sudo apt-get update
  run_sudo env DEBIAN_FRONTEND=noninteractive apt-get install -y \
    ca-certificates curl git openssl
}

set_docker_command() {
  command -v docker >/dev/null 2>&1 || return 1
  docker compose version >/dev/null 2>&1 || return 1

  if docker info >/dev/null 2>&1; then
    DOCKER_CMD=(docker)
  elif (( DRY_RUN == 0 )) && sudo docker info >/dev/null 2>&1; then
    DOCKER_CMD=(sudo docker)
  else
    return 1
  fi
}

ensure_docker() {
  if set_docker_command; then
    ok "Docker Engine and Compose v2 are available"
    return
  fi

  (( SKIP_DOCKER_INSTALL == 0 )) ||
    die "Docker Engine with Compose v2 is required but unavailable"

  if (( DRY_RUN )); then
    info "Docker Engine is unavailable; dry-run will show the Docker CE installation path"
    print_command sudo apt-get update
    print_command sudo install -m 0755 -d /etc/apt/keyrings
    print_command curl --proto '=https' --tlsv1.2 -fsSL https://download.docker.com/linux/ubuntu/gpg -o /tmp/docker.asc
    print_command sudo install -m 0644 /tmp/docker.asc /etc/apt/keyrings/docker.asc
    printf '%s\n' "+ cat > /tmp/docker.sources <<'EOF'" \
      'Types: deb' \
      'URIs: https://download.docker.com/linux/ubuntu' \
      'Suites: noble' \
      'Components: stable' \
      "Architectures: $(dpkg --print-architecture)" \
      'Signed-By: /etc/apt/keyrings/docker.asc' \
      'EOF'
    print_command sudo install -m 0644 /tmp/docker.sources /etc/apt/sources.list.d/docker.sources
    print_command sudo apt-get update
    print_command sudo env DEBIAN_FRONTEND=noninteractive apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
    DOCKER_CMD=(sudo docker)
    return
  fi

  local -a conflicts=()
  local package
  for package in docker.io docker-compose docker-compose-v2 docker-doc docker-buildx podman-docker containerd runc; do
    if installed_package "$package"; then
      conflicts+=("$package")
    fi
  done
  if (( ${#conflicts[@]} > 0 )); then
    die "conflicting Docker packages detected: ${conflicts[*]}. They were not removed; review the existing deployment before installing Docker CE."
  fi

  info "Installing Docker Engine from Docker's official APT repository"
  local keyring=/etc/apt/keyrings/docker.asc
  local source_list=/etc/apt/sources.list.d/docker.sources
  local temporary architecture expected
  architecture="$(dpkg --print-architecture)"

  run_sudo install -m 0755 -d /etc/apt/keyrings
  if [[ ! -s "$keyring" ]]; then
    temporary="$(mktemp)"
    curl --proto '=https' --tlsv1.2 -fsSL https://download.docker.com/linux/ubuntu/gpg -o "$temporary"
    run_sudo install -m 0644 "$temporary" "$keyring"
    rm -f "$temporary"
  fi

  if [[ ! -f "$source_list" ]]; then
    temporary="$(mktemp)"
    cat >"$temporary" <<EOF
Types: deb
URIs: https://download.docker.com/linux/ubuntu
Suites: noble
Components: stable
Architectures: $architecture
Signed-By: $keyring
EOF
    run_sudo install -m 0644 "$temporary" "$source_list"
    rm -f "$temporary"
  else
    for expected in \
      "URIs: https://download.docker.com/linux/ubuntu" \
      "Suites: noble" \
      "Components: stable" \
      "Architectures: $architecture" \
      "Signed-By: $keyring"; do
      grep -Fqx "$expected" "$source_list" ||
        die "existing Docker APT source does not match the expected Ubuntu Noble configuration: $source_list"
    done
    info "Reusing existing Docker APT source: $source_list"
  fi

  run_sudo apt-get update
  run_sudo env DEBIAN_FRONTEND=noninteractive apt-get install -y \
    docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin

  if command -v systemctl >/dev/null 2>&1 && [[ "$(ps -p 1 -o comm=)" == "systemd" ]]; then
    run_sudo systemctl enable --now docker
  else
    warn "systemd is unavailable; make sure the Docker daemon is running before continuing"
  fi

  if ! getent group docker >/dev/null 2>&1; then
    run_sudo groupadd docker
  fi
  if ! id -nG "$USER" | tr ' ' '\n' | grep -qx docker; then
    run_sudo usermod -aG docker "$USER"
    DOCKER_GROUP_ADDED=1
  fi

  # The newly added group is not active in this shell. Use sudo for this initial
  # deployment, then let the user re-login before subsequent non-sudo commands.
  DOCKER_CMD=(sudo docker)
  "${DOCKER_CMD[@]}" info >/dev/null 2>&1 ||
    die "Docker is installed but its daemon is not reachable; start it and rerun the script"
  "${DOCKER_CMD[@]}" compose version >/dev/null 2>&1 ||
    die "Docker Compose v2 is unavailable after installation"
  ok "Docker Engine and Compose v2 installed"
}

normalise_install_dir() {
  INSTALL_DIR="$(realpath -m "$INSTALL_DIR")"
  case "$INSTALL_DIR" in
    /|"$HOME") die "refusing unsafe install directory: $INSTALL_DIR" ;;
  esac
}

is_artex_origin() {
  local origin
  origin="$(git -C "$INSTALL_DIR" remote get-url origin 2>/dev/null || true)"
  case "$origin" in
    https://github.com/White-Rabbit-IT/ARTEX.git|git@github.com:White-Rabbit-IT/ARTEX.git) return 0 ;;
    *) return 1 ;;
  esac
}

prepare_checkout() {
  if [[ ! -e "$INSTALL_DIR" ]]; then
    run mkdir -p "$(dirname "$INSTALL_DIR")"
    run git clone --depth 1 --branch "$REF" "$REPOSITORY_URL" "$INSTALL_DIR"
    (( DRY_RUN == 0 )) || return 0
    ok "Cloned ARTEX into $INSTALL_DIR"
    return
  fi

  [[ -d "$INSTALL_DIR/.git" ]] ||
    die "$INSTALL_DIR already exists but is not a Git checkout; choose another --dir"
  is_artex_origin ||
    die "$INSTALL_DIR is not a checkout of $REPOSITORY_URL; refusing to modify it"

  if (( UPDATE_EXISTING )); then
    [[ -z "$(git -C "$INSTALL_DIR" status --porcelain)" ]] ||
      die "$INSTALL_DIR has local changes; review or commit them before using --update"
    # A depth-1 clone made with --branch normally fetches only that branch.
    # Fetch all heads explicitly so --update --ref can change to another
    # remote branch as documented; tags remain supported too.
    run git -C "$INSTALL_DIR" fetch --prune --tags origin '+refs/heads/*:refs/remotes/origin/*'
    if git -C "$INSTALL_DIR" show-ref --verify --quiet "refs/remotes/origin/$REF"; then
      if git -C "$INSTALL_DIR" show-ref --verify --quiet "refs/heads/$REF"; then
        run git -C "$INSTALL_DIR" checkout "$REF"
      else
        run git -C "$INSTALL_DIR" checkout -b "$REF" "origin/$REF"
      fi
      run git -C "$INSTALL_DIR" pull --ff-only origin "$REF"
    else
      run git -C "$INSTALL_DIR" checkout --detach "$REF"
      info "Checked out immutable ref $REF; no pull was performed"
    fi
    ok "Updated existing checkout at $INSTALL_DIR"
  else
    info "Reusing existing checkout at $INSTALL_DIR (use --update to fast-forward it)"
  fi
}

replace_new_env_value() {
  local file="$1" key="$2" value="$3"
  if grep -q "^$key=" "$file"; then
    sed -i "s|^$key=.*|$key=$value|" "$file"
  else
    printf '\n%s=%s\n' "$key" "$value" >>"$file"
  fi
}

prepare_environment() {
  local env_file="$INSTALL_DIR/.env"
  if [[ -f "$env_file" ]]; then
    info "Reusing existing .env without modification"
    if grep -qx 'POSTGRES_PASSWORD=change-me-please' "$env_file"; then
      warn "Existing .env still uses the example PostgreSQL password; change it before exposing ARTEX."
    fi
    (( BIND_EXPLICIT == 0 )) ||
      warn "--bind was not applied because $env_file already exists"
  else
    if (( DRY_RUN )); then
      info "Would create a new .env with a generated PostgreSQL password and mode 0600"
      print_command cp "$INSTALL_DIR/.env.example" "$env_file"
      print_command chmod 0600 "$env_file"
      return
    fi
    [[ -f "$INSTALL_DIR/.env.example" ]] ||
      die "missing .env.example in $INSTALL_DIR"
    umask 077
    cp "$INSTALL_DIR/.env.example" "$env_file"
    local database_password
    database_password="$(openssl rand -hex 24)"
    replace_new_env_value "$env_file" POSTGRES_PASSWORD "$database_password"
    replace_new_env_value "$env_file" ARTEX_BIND_ADDR "$BIND_ADDR"
    unset database_password
    chmod 0600 "$env_file"
    ok "Created $env_file with a generated PostgreSQL password"
  fi
}

prepare_runtime_directories() {
  local data_dir="$INSTALL_DIR/data"
  local key_dir="$INSTALL_DIR/keys"
  [[ ! -e "$data_dir" || -d "$data_dir" ]] ||
    die "$data_dir exists but is not a directory"
  [[ ! -e "$key_dir" || -d "$key_dir" ]] ||
    die "$key_dir exists but is not a directory"

  # These are bind-mounted runtime directories. Existing contents and
  # permissions are left untouched; a newly created keys/ starts private.
  [[ -e "$data_dir" ]] || run mkdir -p "$data_dir"
  [[ -e "$key_dir" ]] || run install -d -m 0700 "$key_dir"
}

compose() {
  "${DOCKER_CMD[@]}" compose \
    --project-directory "$INSTALL_DIR" \
    --env-file "$INSTALL_DIR/.env" \
    "$@"
}

start_artex() {
  (( NO_START == 0 )) || {
    info "Installation completed without starting ARTEX (--no-start)"
    return
  }

  info "Building the local ARTEX image and starting PostgreSQL + ARTEX"
  if (( DRY_RUN )); then
    print_command "${DOCKER_CMD[@]}" compose --project-directory "$INSTALL_DIR" --env-file "$INSTALL_DIR/.env" up -d --build
    return
  fi
  compose up -d --build
  compose ps

  ok "ARTEX is starting at http://$BIND_ADDR:8787"
  info "Read the one-time setup token locally with:"
  printf '    cd %q && ' "$INSTALL_DIR"
  printf '%q ' "${DOCKER_CMD[@]}" compose logs --no-color artex
  printf '| grep -- %q\n' "Setup token"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dir)
      [[ $# -ge 2 ]] || die "--dir requires a directory"
      INSTALL_DIR="$2"
      shift 2
      ;;
    --ref)
      [[ $# -ge 2 ]] || die "--ref requires a branch or tag"
      REF="$2"
      shift 2
      ;;
    --bind)
      [[ $# -ge 2 ]] || die "--bind requires an address"
      BIND_ADDR="$2"
      BIND_EXPLICIT=1
      shift 2
      ;;
    --update) UPDATE_EXISTING=1; shift ;;
    --skip-docker-install) SKIP_DOCKER_INSTALL=1; shift ;;
    --no-start) NO_START=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    --help|-h) usage; exit 0 ;;
    *) die "unknown option: $1 (use --help)" ;;
  esac
done

[[ "${EUID:-$(id -u)}" -ne 0 ]] ||
  die "run this script as a normal sudo-enabled user, not as root"

require_ubuntu_2404
validate_bind_address
normalise_install_dir
require_sudo
ensure_base_packages
ensure_docker
prepare_checkout
prepare_environment
prepare_runtime_directories
start_artex

if (( DOCKER_GROUP_ADDED )); then
  warn "You were added to the docker group. Log out and back in before using Docker without sudo."
  warn "Membership of the docker group is equivalent to root-level access on this host."
fi

ok "Installation complete"
