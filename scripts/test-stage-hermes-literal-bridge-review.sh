#!/usr/bin/env bash
# Offline contract test for the non-deploying literal-bridge template stager.
# It invokes no Docker, Hermes, systemd, firewall, or provider command.
set -Eeuo pipefail
IFS=$'\n\t'

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly SCRIPT_DIR
STAGER="$SCRIPT_DIR/stage-hermes-literal-bridge-review.sh"
readonly STAGER

[[ -x "$STAGER" ]] || {
  printf 'missing executable stager: %s\n' "$STAGER" >&2
  exit 1
}

TEST_DIR="$(mktemp -d "${TMPDIR:-/tmp}/artex-hermes-bridge-stage-test.XXXXXX")"
readonly TEST_DIR
trap 'rm -rf -- "$TEST_DIR"' EXIT

fail() {
  printf 'test-stage-hermes-literal-bridge-review: %s\n' "$*" >&2
  exit 1
}

STAGE_DIR="$TEST_DIR/review"
DRY_OUTPUT="$TEST_DIR/dry-run.txt"
"$STAGER" --stage-dir "$STAGE_DIR" >"$DRY_OUTPUT"
[[ ! -e "$STAGE_DIR" ]] || fail 'dry run created the stage directory'
rg -F 'dry-run; no files will be created' "$DRY_OUTPUT" >/dev/null || fail 'dry-run receipt was missing'

set +e
"$STAGER" --stage-dir relative --apply >/dev/null 2>&1
status=$?
set -e
(( status != 0 )) || fail 'relative stage directory was accepted'

set +e
"$STAGER" --stage-dir /etc/artex-hermes-review --apply >/dev/null 2>&1
status=$?
set -e
(( status != 0 )) || fail 'system stage directory was accepted'

EXISTING_DIR="$TEST_DIR/existing"
mkdir -m 0755 -- "$EXISTING_DIR"
existing_mode="$(stat -c '%a' -- "$EXISTING_DIR")"
set +e
"$STAGER" --stage-dir "$EXISTING_DIR" --apply >/dev/null 2>&1
status=$?
set -e
(( status != 0 )) || fail 'existing stage directory was accepted'
[[ "$(stat -c '%a' -- "$EXISTING_DIR")" == "$existing_mode" ]] || fail 'rejected existing directory was modified'

SYMLINK_TARGET="$TEST_DIR/symlink-target"
SYMLINK_DIR="$TEST_DIR/symlink-dir"
mkdir -- "$SYMLINK_TARGET"
ln -s -- "$SYMLINK_TARGET" "$SYMLINK_DIR"
set +e
"$STAGER" --stage-dir "$SYMLINK_DIR" --apply >/dev/null 2>&1
status=$?
set -e
(( status != 0 )) || fail 'symlink stage directory was accepted'
[[ -d "$SYMLINK_TARGET" && -z "$(find "$SYMLINK_TARGET" -mindepth 1 -print -quit)" ]] || fail 'symlink target was modified'

"$STAGER" --stage-dir "$STAGE_DIR" --apply >/dev/null
for expected in README.md artex-hermes-proxy-firewall.service.template artex-hermes-subscription-proxy.service.template; do
  [[ -f "$STAGE_DIR/$expected" && ! -L "$STAGE_DIR/$expected" ]] || fail "missing staged file: $expected"
  [[ "$(stat -c '%a' -- "$STAGE_DIR/$expected")" == '644' ]] || fail "unexpected mode for: $expected"
done
[[ "$(stat -c '%a' -- "$STAGE_DIR")" == '700' ]] || fail 'stage directory did not retain mode 0700'

before_sum="$(sha256sum -- "$STAGE_DIR"/* | sha256sum)"
set +e
"$STAGER" --stage-dir "$STAGE_DIR" --apply >/dev/null 2>&1
status=$?
set -e
(( status != 0 )) || fail 'second apply accepted an existing stage directory'
after_sum="$(sha256sum -- "$STAGE_DIR"/* | sha256sum)"
[[ "$before_sum" == "$after_sum" ]] || fail 'failed reapply changed staged review material'

# The stager is purposely a file-only helper. Do not accidentally grow it into
# an activation path while editing it in the future.
if rg -n '(^|[[:space:]])(systemctl|docker|iptables|nft|hermes)[[:space:]]' "$STAGER" >/dev/null; then
  fail 'stager source contains a prohibited activation command'
fi

printf 'test-stage-hermes-literal-bridge-review: PASS\n'
