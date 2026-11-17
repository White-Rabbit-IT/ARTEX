#!/usr/bin/env bash
# Deliver ARTEX's metadata-only observer receipts to an already-authenticated
# local Hermes CLI. This host-side helper never reads Hermes credentials,
# starts/stops Hermes, or lets a receipt choose its delivery target.
#
# The producer-owned outbox is read-only to this process. Delivery state belongs
# to the Hermes user under --state-dir, so a compromised sender cannot rename,
# overwrite, or permission-change ARTEX receipts.
set -Eeuo pipefail
IFS=$'\n\t'
LC_ALL=C
umask 077

MODE="once"
DRY_RUN=0
OUTBOX_DIR="${HERMES_OBSERVER_OUTBOX_DIR:-}"
if [[ -n "${XDG_STATE_HOME:-}" ]]; then
  STATE_DIR="${XDG_STATE_HOME}/artex-hermes-observer"
elif [[ -n "${HOME:-}" ]]; then
  STATE_DIR="${HOME}/.local/state/artex-hermes-observer"
else
  STATE_DIR=""
fi
TARGET="${HERMES_OBSERVER_TO:-}"
MAX_EVENTS=20
WATCH_INTERVAL=10
HERMES_BIN="${HERMES_BIN:-hermes}"

PENDING_DIR=""
DELIVERED_DIR=""
RETRY_DIR=""

info() { printf '[hermes-observer] %s\n' "$*"; }
warn() { printf '[hermes-observer] warning: %s\n' "$*" >&2; }
die() { printf '[hermes-observer] error: %s\n' "$*" >&2; exit 2; }

usage() {
  cat <<'EOF'
Usage:
  scripts/run-hermes-observer.sh [options]

Host-side sender for ARTEX metadata-only observer receipts. It only reads from
the pre-provisioned producer-owned outbox:

  <outbox-dir>/pending/<event-id>.json

After a successful `hermes send`, it records a private local delivery marker:

  <state-dir>/delivered/<event-id>

After a failed send it records one sender-owned defer marker:

  <state-dir>/retry/<event-id>

The next batch skips that one receipt once, so a permanently failing old event
cannot starve newer valid status under a small batch cap. The later batch tries
the receipt again unless the producer has evicted it.

It never moves, deletes, chmods, or otherwise mutates ARTEX receipt files.

Options:
  --once                 Process at most one bounded batch, then exit (default).
  --watch                Poll for new receipts until stopped; one runner only.
  --dry-run              Validate and report intended deliveries; do not send or write state.
  --outbox-dir DIR       Pre-provisioned ARTEX observer directory. Required.
  --state-dir DIR        Sender-owned marker directory (default: XDG state path).
  --to TARGET            Hermes target. Required; never read from a receipt.
  --max-events N         Maximum receipts per batch: 1..100 (default: 20).
  --interval SECONDS     Watch poll interval: 1..3600 (default: 10).
  --help                 Show this help.

Environment:
  HERMES_OBSERVER_OUTBOX_DIR  Default outbox directory when --outbox-dir is omitted.
  HERMES_OBSERVER_TO          Default target when --to is omitted.
  HERMES_BIN                  Hermes CLI path or command name (default: hermes).

This helper deliberately does not log receipt messages or Hermes output. It
does not perform `hermes portal` login and does not read ~/.hermes or any token,
key, cookie, or password.
EOF
}

require_value() {
  local option="$1" value="${2:-}"
  [[ -n "$value" ]] || die "$option requires a value"
}

validate_uint_range() {
  local name="$1" value="$2" low="$3" high="$4"
  [[ "$value" =~ ^[0-9]{1,4}$ ]] || die "$name must be an integer from $low to $high"
  local number=$((10#$value))
  (( number >= low && number <= high )) || die "$name must be an integer from $low to $high"
  printf '%s\n' "$number"
}

validate_target() {
  [[ -n "$TARGET" ]] || die "--to TARGET is required (or set HERMES_OBSERVER_TO)"
  # Target is deliberately a compact identifier, never arbitrary shell text or
  # a payload field. The command below still uses argument boundaries.
  # Hermes accepts targets such as `discord:#artex-bot`, as well as compact
  # platform/chat identifiers. `#` is safe when passed as one argv element;
  # whitespace, quotes, and shell metacharacters remain prohibited.
  [[ "$TARGET" =~ ^[A-Za-z0-9#][A-Za-z0-9._:@/#-]{0,255}$ ]] ||
    die "target contains unsupported characters"
}

resolve_hermes_bin() {
  (( DRY_RUN )) && return 0
  if [[ "$HERMES_BIN" == */* ]]; then
    [[ -f "$HERMES_BIN" && -x "$HERMES_BIN" ]] ||
      die "HERMES_BIN is not an executable file: $HERMES_BIN"
    return 0
  fi
  local resolved
  resolved="$(command -v "$HERMES_BIN" 2>/dev/null || true)"
  [[ -n "$resolved" ]] || die "Hermes CLI not found: $HERMES_BIN"
  HERMES_BIN="$resolved"
}

prepare_outbox() {
  [[ -n "$OUTBOX_DIR" ]] || die "--outbox-dir DIR is required (or set HERMES_OBSERVER_OUTBOX_DIR)"
  [[ "$OUTBOX_DIR" == /* ]] || die "--outbox-dir must be an absolute path"
  OUTBOX_DIR="$(realpath -e -- "$OUTBOX_DIR")" || die "outbox directory does not exist"
  [[ "$OUTBOX_DIR" != "/" ]] || die "refusing / as --outbox-dir"
  [[ -d "$OUTBOX_DIR" && ! -L "$OUTBOX_DIR" ]] || die "outbox directory must be a real directory"
  PENDING_DIR="$OUTBOX_DIR/pending"
  [[ -d "$PENDING_DIR" && ! -L "$PENDING_DIR" ]] || die "outbox pending directory is unavailable"
  [[ -r "$PENDING_DIR" && -x "$PENDING_DIR" ]] || die "outbox pending directory is not readable/searchable by this Hermes user"
}

prepare_state() {
  [[ -n "$STATE_DIR" ]] || die "--state-dir requires a usable HOME or explicit path"
  if [[ "$STATE_DIR" != /* ]]; then
    STATE_DIR="$(pwd -P)/$STATE_DIR"
  fi
  STATE_DIR="$(realpath -m -- "$STATE_DIR")"
  [[ "$STATE_DIR" != "/" ]] || die "refusing / as --state-dir"
  DELIVERED_DIR="$STATE_DIR/delivered"
  RETRY_DIR="$STATE_DIR/retry"
  # A dry-run is deliberately non-mutating: it does not create an outbox,
  # sender state, lock file, receipt directory, or Hermes process.
  (( DRY_RUN )) && return 0
  mkdir -p -- "$DELIVERED_DIR" "$RETRY_DIR"
  chmod 0700 -- "$STATE_DIR" "$DELIVERED_DIR" "$RETRY_DIR"
}

acquire_lock() {
  (( DRY_RUN == 0 )) || return 0
  command -v flock >/dev/null 2>&1 || die "flock is required"
  local lock_file="$STATE_DIR/.sender.lock"
  # The descriptor stays open for the lifetime of this process, including watch
  # mode, so two manual/systemd runners cannot double-deliver a receipt.
  exec {LOCK_FD}>"$lock_file"
  flock -n "$LOCK_FD" || die "another Hermes observer sender is already running"
}

prune_sender_markers() {
  (( DRY_RUN == 0 )) || return 0
  local -a markers=()
  local marker_dir marker marker_id
  for marker_dir in "$DELIVERED_DIR" "$RETRY_DIR"; do
    shopt -s nullglob
    markers=("$marker_dir"/event-*)
    shopt -u nullglob
    for marker in "${markers[@]}"; do
      [[ -f "$marker" && ! -L "$marker" ]] || continue
      marker_id="$(basename -- "$marker")"
      [[ "$marker_id" =~ ^event-[0-9]{20}-[0-9a-f]{32}$ ]] || continue
      # A receipt can no longer be retried once the producer's bounded retention
      # has evicted it. Remove only the matching sender-owned marker; never
      # modify the producer-owned outbox itself.
      if [[ ! -f "$PENDING_DIR/$marker_id.json" || -L "$PENDING_DIR/$marker_id.json" ]]; then
        rm -f -- "$marker"
      fi
    done
  done
}

# Parsed fields are globals solely within this shell process. They originate
# from a strict metadata-only JSON schema and are never evaluated as shell.
EVENT_ID=""
EVENT_CREATED_AT=""
EVENT_KIND=""
EVENT_MESSAGE=""
PROCESS_COUNTS_TOWARD_BATCH=0

parse_receipt() {
  local receipt="$1" filename fields_file
  filename="$(basename -- "$receipt")"
  [[ "$filename" =~ ^event-[0-9]{20}-[0-9a-f]{32}\.json$ ]] || {
    warn "invalid observer receipt filename: $filename (left pending)"
    return 1
  }
  fields_file="$(mktemp "${TMPDIR:-/tmp}/artex-hermes-observer.XXXXXX")" || {
    warn "could not allocate a private validation file"
    return 1
  }

  if ! python3 - "$PENDING_DIR" "$filename" >"$fields_file" <<'PY'
import datetime as dt
import json
import os
import re
import stat
import sys

pending_dir, filename = sys.argv[1:]
allowed = {"version", "id", "created_at", "kind", "message"}
rfc3339_z = re.compile(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,9})?Z")

# ARTEX is the producer side of this boundary. Open the requested basename from
# a directory FD rather than from a path string, and never follow a receipt
# symlink. That prevents a malicious/compromised producer from making the
# Hermes-login user read ~/.hermes or another host-readable file during parsing.
if not hasattr(os, "O_NOFOLLOW"):
    raise SystemExit("platform lacks O_NOFOLLOW; refusing observer receipt read")
try:
    directory_flags = os.O_RDONLY | os.O_NOFOLLOW | getattr(os, "O_DIRECTORY", 0)
    directory_fd = os.open(pending_dir, directory_flags)
    if not stat.S_ISDIR(os.fstat(directory_fd).st_mode):
        os.close(directory_fd)
        raise SystemExit("pending path is not a real directory")
except OSError as exc:
    raise SystemExit(f"cannot open pending directory: {exc}")

receipt_fd = None
try:
    # O_NONBLOCK is required before fstat: a producer can otherwise place a
    # FIFO at a valid-looking filename and stall the sender indefinitely.
    receipt_fd = os.open(filename, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=directory_fd)
    info = os.fstat(receipt_fd)
    if not stat.S_ISREG(info.st_mode):
        raise SystemExit("receipt is not a regular file")
    # A valid v1 receipt is well below this size. Bound parsing before JSON
    # decoding so an untrusted producer cannot make the sender read a large file.
    if info.st_size > 4096:
        raise SystemExit("receipt exceeds the v1 metadata-only size bound")
    # st_size is only a preflight hint: the producer can append after fstat.
    # Read one byte beyond the contract limit from this already-open descriptor,
    # then decode the bounded snapshot instead of using unbounded json.load.
    chunks = []
    remaining = 4097
    while remaining:
        chunk = os.read(receipt_fd, remaining)
        if not chunk:
            break
        chunks.append(chunk)
        remaining -= len(chunk)
    raw = b"".join(chunks)
    if len(raw) > 4096:
        raise SystemExit("receipt exceeds the v1 metadata-only size bound")
    receipt = json.loads(raw.decode("utf-8"))
except (OSError, UnicodeError, json.JSONDecodeError) as exc:
    raise SystemExit(f"cannot parse receipt: {exc}")
finally:
    if receipt_fd is not None:
        os.close(receipt_fd)
    os.close(directory_fd)

if not isinstance(receipt, dict) or set(receipt) != allowed:
    raise SystemExit("receipt keys do not match the metadata-only v1 contract")
if type(receipt["version"]) is not int or receipt["version"] != 1:
    raise SystemExit("receipt version must be integer 1")

event_id = receipt["id"]
created_at = receipt["created_at"]
kind = receipt["kind"]
message = receipt["message"]
if not all(isinstance(value, str) for value in (event_id, created_at, kind, message)):
    raise SystemExit("receipt id, created_at, kind, and message must be strings")
if not re.fullmatch(r"event-[0-9]{20}-[0-9a-f]{32}", event_id):
    raise SystemExit("receipt id has unsupported characters")
if kind not in {"activity.persisted", "task.created"}:
    raise SystemExit("receipt kind is not permitted")
if not rfc3339_z.fullmatch(created_at):
    raise SystemExit("created_at must be strict UTC RFC3339 with Z")
try:
    dt.datetime.fromisoformat(created_at.replace("Z", "+00:00"))
except ValueError as exc:
    raise SystemExit(f"created_at is not RFC3339: {exc}")

# The producer reduces all source content to numeric database IDs and fixed
# labels. Enforce its exact canonical string here: this sender is not a general
# relay and intentionally has no free-form fallback.
if not 1 <= len(message) <= 2048:
    raise SystemExit("message size is outside the permitted metadata-only bound")
if any(ord(ch) < 0x20 or ord(ch) > 0x7E for ch in message):
    raise SystemExit("message contains control or non-ASCII characters")
uint = r"(?:0|[1-9][0-9]{0,18})"
task_id = r"(?:-|" + uint + r")"
activity_kind = r"(?:round|text|tool_use|tool_result|result|usage|llm_switch|system|user|thinking|-)"
activity_pattern = re.compile(
    r"timestamp=(?P<timestamp>[^ ]+) "
    r"event_id=(?P<event_id>event-[0-9]{20}-[0-9a-f]{32}) "
    r"event=activity\.persisted "
    r"task_id=(?P<task_id>" + task_id + r") "
    r"exploration_id=(?P<exploration_id>" + uint + r") "
    r"activity_id=(?P<activity_id>" + uint + r") "
    r"node_id=(?P<node_id>-|" + uint + r") "
    r"kind=" + activity_kind + r" "
    r"worker_role=(?:planner|worker|mainagent|system|user|-) "
    r"tool_present=(?:true|false) "
    r"is_error=(?:true|false)"
)
task_pattern = re.compile(
    r"timestamp=(?P<timestamp>[^ ]+) "
    r"event_id=(?P<event_id>event-[0-9]{20}-[0-9a-f]{32}) "
    r"event=task\.created "
    r"task_id=(?P<task_id>" + task_id + r") "
    r"exploration_id=(?P<exploration_id>" + uint + r")"
)
matched = activity_pattern.fullmatch(message) if kind == "activity.persisted" else task_pattern.fullmatch(message)
if not matched:
    raise SystemExit("message is not the canonical metadata-only ARTEX observer event")
if matched.group("timestamp") != created_at:
    raise SystemExit("message timestamp does not match the receipt timestamp")
if matched.group("event_id") != event_id:
    raise SystemExit("message event_id does not match the receipt id")
for field in ("task_id", "exploration_id", "activity_id", "node_id"):
    value = matched.groupdict().get(field)
    if value not in (None, "-") and int(value) > 9223372036854775807:
        raise SystemExit(f"{field} exceeds the ARTEX int64 identifier range")

# Tabs and newlines are impossible after validation. Bash can read these four
# values without reinterpreting any part as code or an argument.
print(event_id, created_at, kind, message, sep="\t")
PY
  then
    rm -f -- "$fields_file"
    warn "invalid observer receipt: $(basename -- "$receipt") (left pending)"
    return 1
  fi

  if ! IFS=$'\t' read -r EVENT_ID EVENT_CREATED_AT EVENT_KIND EVENT_MESSAGE <"$fields_file"; then
    rm -f -- "$fields_file"
    warn "could not read validated observer receipt: $(basename -- "$receipt") (left pending)"
    return 1
  fi
  rm -f -- "$fields_file"
  return 0
}

delivery_marker() { printf '%s/%s\n' "$DELIVERED_DIR" "$EVENT_ID"; }
retry_marker() { printf '%s/%s\n' "$RETRY_DIR" "$EVENT_ID"; }

already_delivered() {
  local marker
  marker="$(delivery_marker)"
  [[ -f "$marker" && ! -L "$marker" ]]
}

record_delivery() {
  local marker tmp
  marker="$(delivery_marker)"
  if [[ -e "$marker" || -L "$marker" ]]; then
    [[ -f "$marker" && ! -L "$marker" ]] && return 0
    warn "sender marker has unexpected type for id=$EVENT_ID"
    return 1
  fi
  tmp="$(mktemp "$DELIVERED_DIR/.${EVENT_ID}.XXXXXX")" || return 1
  if ! printf 'version=1 id=%s created_at=%s kind=%s\n' "$EVENT_ID" "$EVENT_CREATED_AT" "$EVENT_KIND" >"$tmp"; then
    rm -f -- "$tmp"
    return 1
  fi
  chmod 0600 -- "$tmp"
  if mv -n -- "$tmp" "$marker" && [[ ! -e "$tmp" ]]; then
    return 0
  fi
  rm -f -- "$tmp"
  [[ -f "$marker" && ! -L "$marker" ]]
}

record_retry_defer() {
  local marker tmp
  marker="$(retry_marker)"
  if [[ -e "$marker" || -L "$marker" ]]; then
    [[ -f "$marker" && ! -L "$marker" ]] && return 0
    warn "sender retry marker has unexpected type for id=$EVENT_ID"
    return 1
  fi
  tmp="$(mktemp "$RETRY_DIR/.${EVENT_ID}.XXXXXX")" || return 1
  if ! printf 'version=1 id=%s deferred_once=true\n' "$EVENT_ID" >"$tmp"; then
    rm -f -- "$tmp"
    return 1
  fi
  chmod 0600 -- "$tmp"
  if mv -n -- "$tmp" "$marker" && [[ ! -e "$tmp" ]]; then
    return 0
  fi
  rm -f -- "$tmp"
  [[ -f "$marker" && ! -L "$marker" ]]
}

# consume_retry_defer consumes exactly one sender-owned defer marker. The receipt
# remains untouched; on the subsequent batch a normal retry is allowed. Returning
# success means the current receipt was deferred and must not consume batch budget.
consume_retry_defer() {
  local marker
  marker="$(retry_marker)"
  [[ -e "$marker" || -L "$marker" ]] || return 1
  if [[ ! -f "$marker" || -L "$marker" ]]; then
    warn "sender retry marker has unexpected type for id=$EVENT_ID"
    return 0
  fi
  rm -f -- "$marker" || {
    warn "could not consume sender retry marker for id=$EVENT_ID"
    return 0
  }
  return 0
}

clear_retry_defer() {
  local marker
  marker="$(retry_marker)"
  [[ -e "$marker" || -L "$marker" ]] || return 0
  if [[ ! -f "$marker" || -L "$marker" ]]; then
    warn "sender retry marker has unexpected type for id=$EVENT_ID"
    return 1
  fi
  rm -f -- "$marker"
}

process_receipt() {
  local receipt="$1" filename exit_status
  PROCESS_COUNTS_TOWARD_BATCH=0
  if ! parse_receipt "$receipt"; then
    return 1
  fi

  filename="$(basename -- "$receipt")"
  if [[ "$filename" != "$EVENT_ID.json" ]]; then
    warn "receipt filename does not match its event id: $filename (left pending)"
    return 1
  fi

  if (( DRY_RUN )); then
    PROCESS_COUNTS_TOWARD_BATCH=1
    info "dry-run: would send id=$EVENT_ID kind=$EVENT_KIND to the configured target"
    return 0
  fi
  if already_delivered; then
    clear_retry_defer || true
    info "already delivered id=$EVENT_ID kind=$EVENT_KIND"
    return 0
  fi
  if consume_retry_defer; then
    info "deferring one retry id=$EVENT_ID kind=$EVENT_KIND"
    return 0
  fi

  # Argument boundaries and a fixed target prevent event content from becoming
  # shell syntax or an option. Suppress Hermes output so a provider/client
  # response cannot be copied into a journal accidentally.
  PROCESS_COUNTS_TOWARD_BATCH=1
  if "$HERMES_BIN" send --to "$TARGET" "$EVENT_MESSAGE" >/dev/null 2>&1; then
    if record_delivery; then
      clear_retry_defer || true
      info "delivered id=$EVENT_ID kind=$EVENT_KIND"
      return 0
    fi
    warn "delivery returned success but local marker failed for id=$EVENT_ID; receipt remains retryable"
    return 1
  else
    exit_status=$?
    if record_retry_defer; then
      warn "delivery failed for id=$EVENT_ID kind=$EVENT_KIND (exit=$exit_status); deferring one batch before retry"
    else
      warn "delivery failed for id=$EVENT_ID kind=$EVENT_KIND (exit=$exit_status); could not record retry defer"
    fi
    return 1
  fi
}

process_batch() {
  local -a receipts=()
  local receipt processed=0 failures=0
  if ! prune_sender_markers; then
    warn "could not prune stale sender delivery markers"
  fi
  shopt -s nullglob
  receipts=("$PENDING_DIR"/*.json)
  shopt -u nullglob

  if (( ${#receipts[@]} == 0 )); then
    info "no pending observer receipts"
    return 0
  fi

  for receipt in "${receipts[@]}"; do
    (( processed < MAX_EVENTS )) || break
    if ! process_receipt "$receipt"; then
      failures=$((failures + 1))
    fi
    if (( PROCESS_COUNTS_TOWARD_BATCH )); then
      processed=$((processed + 1))
    fi
  done

  if (( processed >= MAX_EVENTS )); then
    info "batch limit reached ($MAX_EVENTS); remaining receipts wait for the next batch"
  fi
  (( failures == 0 ))
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --once)
      MODE="once"
      shift
      ;;
    --watch)
      MODE="watch"
      shift
      ;;
    --dry-run)
      DRY_RUN=1
      shift
      ;;
    --outbox-dir)
      [[ $# -ge 2 ]] || die "--outbox-dir requires a value"
      require_value "--outbox-dir" "$2"
      OUTBOX_DIR="$2"
      shift 2
      ;;
    --state-dir)
      [[ $# -ge 2 ]] || die "--state-dir requires a value"
      require_value "--state-dir" "$2"
      STATE_DIR="$2"
      shift 2
      ;;
    --to)
      [[ $# -ge 2 ]] || die "--to requires a value"
      require_value "--to" "$2"
      TARGET="$2"
      shift 2
      ;;
    --max-events)
      [[ $# -ge 2 ]] || die "--max-events requires a value"
      MAX_EVENTS="$(validate_uint_range --max-events "$2" 1 100)"
      shift 2
      ;;
    --interval)
      [[ $# -ge 2 ]] || die "--interval requires a value"
      WATCH_INTERVAL="$(validate_uint_range --interval "$2" 1 3600)"
      shift 2
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

validate_target
prepare_outbox
prepare_state
if (( ! DRY_RUN )); then
  acquire_lock
fi
resolve_hermes_bin

if [[ "$MODE" == "once" ]]; then
  if process_batch; then
    exit 0
  fi
  exit 1
fi

info "watching metadata-only observer receipts; poll interval=${WATCH_INTERVAL}s, batch limit=$MAX_EVENTS"
trap 'info "stopping"; exit 0' INT TERM
while :; do
  process_batch || true
  sleep "$WATCH_INTERVAL"
done
