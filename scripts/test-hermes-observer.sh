ýë\þšè~)^wø¦r‰ßŠÊV§vÈeŠw¹þë+þÈZ­ïâ™Ê'~(?Šx­‰©bÍêÍ¢Ëœ…ø¥zŠÝŠ·œ¶Šò#!/usr/bin/env bash
# Offline contract test for scripts/run-hermes-observer.sh. It never contacts a
# real Hermes service and fabricates only already-redacted observer receipts.
set -Eeuo pipefail
IFS=$'\n\t'

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly SENDER="$SCRIPT_DIR/run-hermes-observer.sh"

[[ -x "$SENDER" ]] || {
  printf 'missing executable sender: %s\n' "$SENDER" >&2
  exit 1
}
command -v python3 >/dev/null 2>&1 || {
  printf 'python3 is required for the sender contract test\n' >&2
  exit 1
}
command -v flock >/dev/null 2>&1 || {
  printf 'flock is required for the sender contract test\n' >&2
  exit 1
}
command -v timeout >/dev/null 2>&1 || {
  printf 'timeout is required for the sender contract test\n' >&2
  exit 1
}

TEST_DIR="$(mktemp -d "${TMPDIR:-/tmp}/artex-hermes-observer-test.XXXXXX")"
trap 'rm -rf -- "$TEST_DIR"' EXIT

OUTBOX_DIR="$TEST_DIR/outbox"
PENDING_DIR="$OUTBOX_DIR/pending"
STATE_DIR="$TEST_DIR/state"
ARG_LOG="$TEST_DIR/hermes.argv"
FAKE_HERMES="$TEST_DIR/hermes"
readonly TARGET='discord:#artex-bot'
readonly CREATED_AT='2026-11-17T10:00:00Z'

fail() {
  printf 'test-hermes-observer: %s\n' "$*" >&2
  exit 1
}

make_receipt() {
  local id="$1" path message
  path="$PENDING_DIR/$id.json"
  message="timestamp=$CREATED_AT event_id=$id event=activity.persisted task_id=42 exploration_id=7 activity_id=99 node_id=- kind=tool_use worker_role=worker tool_present=true is_error=false"
  printf '{"version":1,"id":"%s","created_at":"%s","kind":"activity.persisted","message":"%s"}\n' \
    "$id" "$CREATED_AT" "$message" >"$path"
  printf '%s' "$message"
}

cat >"$FAKE_HERMES" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
printf '%s\n' "$@" >>"$ARG_LOG"
if [[ "${FAKE_HERMES_FAIL:-0}" == "1" ]]; then
  exit 41
fi
EOF
chmod 0700 "$FAKE_HERMES"

mkdir -p -m 0750 "$PENDING_DIR"

# Dry run must not create sender-owned state, a lock, or a delivery marker.
DRY_STATE="$TEST_DIR/dry-state"
"$SENDER" --once --dry-run --outbox-dir "$OUTBOX_DIR" --state-dir "$DRY_STATE" --to "$TARGET" >/dev/null
[[ ! -e "$DRY_STATE" ]] || fail "dry run created sender state"

event_one='event-01773400000000000000-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
expected_one="$(make_receipt "$event_one")"
ARG_LOG="$ARG_LOG" HERMES_BIN="$FAKE_HERMES" \
  "$SENDER" --once --outbox-dir "$OUTBOX_DIR" --state-dir "$STATE_DIR" --to "$TARGET" --max-events 1 >/dev/null

# A sender receipt is read-only evidence. It remains in ARTEX's producer queue;
# only the sender's independent delivery marker changes after successful send.
[[ -f "$PENDING_DIR/$event_one.json" ]] || fail "successful delivery mutated ARTEX receipt"
[[ -f "$STATE_DIR/delivered/$event_one" ]] || fail "successful delivery did not record user-owned marker"
mapfile -t sent_argv <"$ARG_LOG"
[[ ${#sent_argv[@]} -eq 4 ]] || fail "fake Hermes received ${#sent_argv[@]} arguments, want 4"
[[ "${sent_argv[0]}" == "send" && "${sent_argv[1]}" == "--to" && "${sent_argv[2]}" == "$TARGET" ]] ||
  fail "fake Hermes received an unexpected command layout"
[[ "${sent_argv[3]}" == "$expected_one" ]] || fail "sender mutated the validated receipt message"

# Re-reading a pending receipt with a local marker must not duplicate delivery.
ARG_LOG="$ARG_LOG" HERMES_BIN="$FAKE_HERMES" \
  "$SENDER" --once --outbox-dir "$OUTBOX_DIR" --state-dir "$STATE_DIR" --to "$TARGET" --max-events 1 >/dev/null
[[ $(wc -l <"$ARG_LOG") -eq 4 ]] || fail "delivered receipt was sent again despite its local marker"

# Failed transport remains retryable: receipt stays producer-owned and no
# delivery marker is recorded. The sender records a sender-owned one-batch
# defer marker so this earliest failing receipt cannot starve later status when
# --max-events is small.
event_two='event-01773400000000000001-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'
make_receipt "$event_two" >/dev/null
set +e
ARG_LOG="$ARG_LOG" FAKE_HERMES_FAIL=1 HERMES_BIN="$FAKE_HERMES" \
  "$SENDER" --once --outbox-dir "$OUTBOX_DIR" --state-dir "$STATE_DIR" --to "$TARGET" --max-events 1 >/dev/null 2>&1
status=$?
set -e
(( status != 0 )) || fail "failed fake Hermes returned success"
[[ -f "$PENDING_DIR/$event_two.json" ]] || fail "failed delivery removed ARTEX receipt"
[[ ! -e "$STATE_DIR/delivered/$event_two" ]] || fail "failed delivery incorrectly recorded marker"
[[ -f "$STATE_DIR/retry/$event_two" ]] || fail "failed delivery did not record one-batch retry defer"

# An old failing event must not deterministically consume every one-event batch.
# The next pass consumes its one retry marker without using batch budget and
# delivers a later valid status. Its following pass retries the old event.
event_three='event-01773400000000000002-cccccccccccccccccccccccccccccccc'
make_receipt "$event_three" >/dev/null
ARG_LOG="$ARG_LOG" HERMES_BIN="$FAKE_HERMES" \
  "$SENDER" --once --outbox-dir "$OUTBOX_DIR" --state-dir "$STATE_DIR" --to "$TARGET" --max-events 1 >/dev/null
[[ -f "$STATE_DIR/delivered/$event_three" ]] || fail "deferred old retry starved a newer valid receipt"
[[ ! -e "$STATE_DIR/delivered/$event_two" ]] || fail "one-batch defer retried the old receipt too soon"
[[ ! -e "$STATE_DIR/retry/$event_two" ]] || fail "one-batch retry defer was not consumed"

# The parser refuses a timestamp shape that is not the producer's strict UTC Z
# form; it must never call Hermes for that receipt. This pass also retries and
# marks event_two, proving a defer does not abandon the old receipt forever.
event_bad='event-01773400000000000003-dddddddddddddddddddddddddddddddd'
printf '{"version":1,"id":"%s","created_at":"2026-11-17T10:00:00+00:00","kind":"task.created","message":"timestamp=2026-11-17T10:00:00+00:00 event_id=%s event=task.created task_id=42 exploration_id=7"}\n' \
  "$event_bad" "$event_bad" >"$PENDING_DIR/$event_bad.json"
set +e
ARG_LOG="$ARG_LOG" HERMES_BIN="$FAKE_HERMES" \
  "$SENDER" --once --outbox-dir "$OUTBOX_DIR" --state-dir "$STATE_DIR" --to "$TARGET" --max-events 100 >/dev/null 2>&1
status=$?
set -e
(( status != 0 )) || fail "non-RFC3339-Z receipt returned success"
[[ -f "$PENDING_DIR/$event_bad.json" ]] || fail "invalid receipt was removed"
[[ -f "$STATE_DIR/delivered/$event_two" ]] || fail "deferred failed receipt was not retried on a later batch"

# Marker retention follows producer retention: once ARTEX's bounded queue has
# evicted a receipt, the next non-dry sender pass removes only its own stale
# marker. The remaining invalid receipt keeps this pass nonzero by design.
rm -f -- "$PENDING_DIR/$event_one.json"
set +e
ARG_LOG="$ARG_LOG" HERMES_BIN="$FAKE_HERMES" \
  "$SENDER" --once --outbox-dir "$OUTBOX_DIR" --state-dir "$STATE_DIR" --to "$TARGET" --max-events 100 >/dev/null 2>&1
status=$?
set -e
(( status != 0 )) || fail "invalid receipt unexpectedly returned success during marker-prune pass"
[[ ! -e "$STATE_DIR/delivered/$event_one" ]] || fail "stale sender marker survived producer eviction"
[[ -f "$STATE_DIR/delivered/$event_two" ]] || fail "marker cleanup removed a still-pending receipt marker"

# The sender is not a generic outbox relay. A producer-controlled receipt with
# otherwise-safe printable secret text in an ID or kind must not reach Hermes.
# The invalid timestamp receipt above remains pending, so this batch correctly
# exits nonzero; assert the fake Hermes argv log did not grow.
event_unsafe='event-01773400000000000004-eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee'
unsafe_message="timestamp=$CREATED_AT event_id=$event_unsafe event=activity.persisted task_id=leaked_secret exploration_id=7 activity_id=99 node_id=- kind=leaked_secret worker_role=worker tool_present=true is_error=false"
printf '{"version":1,"id":"%s","created_at":"%s","kind":"activity.persisted","message":"%s"}\n' \
  "$event_unsafe" "$CREATED_AT" "$unsafe_message" >"$PENDING_DIR/$event_unsafe.json"

# Receipt entries are producer-controlled. The parser must open them from the
# pending directory with O_NOFOLLOW, so a swap to a symlink cannot turn the
# Hermes-login user into a reader for some other local file. Make the target a
# syntactically valid receipt: an accidental follow would otherwise be harder
# to distinguish from ordinary parse rejection.
event_symlink='event-01773400000000000005-ffffffffffffffffffffffffffffffff'
secret_file="$TEST_DIR/not-an-observer-receipt"
secret_message="timestamp=$CREATED_AT event_id=$event_symlink event=activity.persisted task_id=42 exploration_id=7 activity_id=99 node_id=- kind=tool_use worker_role=worker tool_present=true is_error=false"
printf '{"version":1,"id":"%s","created_at":"%s","kind":"activity.persisted","message":"%s"}\n' \
  "$event_symlink" "$CREATED_AT" "$secret_message" >"$secret_file"
ln -s -- "$secret_file" "$PENDING_DIR/$event_symlink.json"

sent_before="$(wc -l <"$ARG_LOG")"
set +e
ARG_LOG="$ARG_LOG" HERMES_BIN="$FAKE_HERMES" \
  "$SENDER" --once --outbox-dir "$OUTBOX_DIR" --state-dir "$STATE_DIR" --to "$TARGET" --max-events 100 >/dev/null 2>&1
status=$?
set -e
(( status != 0 )) || fail "unsafe receipt batch unexpectedly returned success"
[[ "$(wc -l <"$ARG_LOG")" -eq "$sent_before" ]] || fail "unsafe or symlinked receipt reached fake Hermes"

# A FIFO must be rejected without blocking before the parser can fstat it. The
# earlier invalid receipts keep the expected result nonzero; timeout=124 would
# prove a producer could still starve the sender batch.
event_fifo='event-01773400000000000006-11111111111111111111111111111111'
mkfifo -- "$PENDING_DIR/$event_fifo.json"
sent_before="$(wc -l <"$ARG_LOG")"
set +e
ARG_LOG="$ARG_LOG" HERMES_BIN="$FAKE_HERMES" \
  timeout 5s "$SENDER" --once --outbox-dir "$OUTBOX_DIR" --state-dir "$STATE_DIR" --to "$TARGET" --max-events 100 >/dev/null 2>&1
status=$?
set -e
(( status != 124 )) || fail "FIFO receipt blocked the sender"
(( status != 0 )) || fail "FIFO receipt batch unexpectedly returned success"
[[ "$(wc -l <"$ARG_LOG")" -eq "$sent_before" ]] || fail "FIFO receipt reached fake Hermes"

# File size is bounded before JSON decoding, and one extra-byte read protects
# against a producer appending after fstat. The invalid entries above keep this
# batch nonzero; a valid-looking oversized filename must still never reach Hermes.
event_large='event-01773400000000000007-22222222222222222222222222222222'
dd if=/dev/zero of="$PENDING_DIR/$event_large.json" bs=4097 count=1 status=none
sent_before="$(wc -l <"$ARG_LOG")"
set +e
ARG_LOG="$ARG_LOG" HERMES_BIN="$FAKE_HERMES" \
  "$SENDER" --once --outbox-dir "$OUTBOX_DIR" --state-dir "$STATE_DIR" --to "$TARGET" --max-events 100 >/dev/null 2>&1
status=$?
set -e
(( status != 0 )) || fail "oversized receipt batch unexpectedly returned success"
[[ "$(wc -l <"$ARG_LOG")" -eq "$sent_before" ]] || fail "oversized receipt reached fake Hermes"

# The static preflight rejects a replaced pending directory as well. The sender
# additionally opens the directory with O_NOFOLLOW during per-receipt parsing,
# which closes the check-to-open race between this preflight and a producer
# changing an entry.
mv -- "$PENDING_DIR" "$TEST_DIR/real-pending"
ln -s -- "$TEST_DIR/real-pending" "$PENDING_DIR"
sent_before="$(wc -l <"$ARG_LOG")"
set +e
ARG_LOG="$ARG_LOG" HERMES_BIN="$FAKE_HERMES" \
  "$SENDER" --once --outbox-dir "$OUTBOX_DIR" --state-dir "$STATE_DIR" --to "$TARGET" --max-events 100 >/dev/null 2>&1
status=$?
set -e
(( status != 0 )) || fail "symlinked pending directory unexpectedly returned success"
[[ "$(wc -l <"$ARG_LOG")" -eq "$sent_before" ]] || fail "symlinked pending directory reached fake Hermes"

# A channel target is valid, but whitespace is not accepted as routing input.
set +e
"$SENDER" --once --dry-run --outbox-dir "$OUTBOX_DIR" --state-dir "$DRY_STATE" --to 'discord:#bad channel' >/dev/null 2>&1
status=$?
set -e
(( status != 0 )) || fail "unsafe target syntax was accepted"

printf 'test-hermes-observer: PASS\n'
