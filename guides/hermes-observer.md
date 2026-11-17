# Hermes observer sender

The optional Hermes observer publishes a small, metadata-only ARTEX activity
feed to a fixed Hermes bot/channel target. It is deliberately **not** an ARTEX
task runner, OAuth connector, model provider, or general event relay.

It has two separately owned local stores:

```text
ARTEX container                         host / Hermes-login user
----------------                         ------------------------
producer writes receipt                  sender reads receipt only
  /app/hermes-observer/pending   <---    /var/lib/artex-hermes-observer/pending
  root:artex-observer 0640              root:artex-observer 2750
                                                    |
                                                    | hermes send --to <fixed target>
                                                    v
                                             Hermes bot/channel

sender writes only its own marker:
  $XDG_STATE_HOME/artex-hermes-observer/delivered/<event-id>
  $XDG_STATE_HOME/artex-hermes-observer/retry/<event-id>
```

The host directory is outside ARTEX's general `data/` tree and is owned by
root. The Hermes-login user is only in the `artex-observer` group, which gives
it read/search access to `pending/`, not write access. ARTEX never reads
`~/.hermes`, never receives an OAuth token, and never invokes the Hermes CLI.

## Boundary and non-goals

The sender deliberately does **not**:

- run `hermes portal`, inspect `~/.hermes`, or read/copy an OAuth token, API
  key, SSH key, cookie, password, or ARTEX secret;
- start, stop, restart, reconfigure, or proxy Hermes;
- create ARTEX tasks, scan a target, enable traffic capture, or read ARTEX's
  database, evidence, transcripts, `keys/`, or raw artifacts;
- accept a Hermes target from ARTEX, a receipt, or an LLM response;
- move, delete, chmod, or otherwise modify ARTEX receipt files;
- log the delivery message or Hermes stdout/stderr.

The normal Hermes CLI remains responsible for using its existing login state.
Before enabling this sender, manually verify that the same non-root UNIX user
can run a harmless `hermes send` command successfully. The sender never tries
to repair or re-authenticate that state.

## Receipt contract

ARTEX atomically writes this exact pending path:

```text
<observer-dir>/pending/<event-id>.json
```

The schema has exactly five fields:

```json
{
  "version": 1,
  "id": "event-01773400000000000000-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "created_at": "2026-11-17T10:00:00Z",
  "kind": "activity.persisted",
  "message": "timestamp=2026-11-17T10:00:00Z event_id=event-01773400000000000000-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa event=activity.persisted task_id=42 exploration_id=7 activity_id=99 node_id=- kind=tool_use worker_role=worker tool_present=true is_error=false"
}
```

Permitted kinds are exactly `activity.persisted` and `task.created`. The sender
opens the final receipt name relative to a no-follow pending-directory handle,
then verifies its fixed-order grammar, strict UTC RFC3339 timestamp, event ID,
numeric ARTEX database IDs, controlled labels, and error flag. It does not send
a generic free-form status line. Symlinked, oversized, malformed, or
out-of-range receipts are retained and never relayed.

The line contains **no** source text, prompt, model output, target, path,
credential, HTTP traffic, raw error text, content-derived hash, or byte length.
Unsafe/non-opaque labels collapse to `-`; they do not become a digest. Invalid
receipts remain in `pending/` for local review and are never sent.

This automatic boundary protects against accidental export through ARTEX's
ordinary activity fields. It is **not** a confidentiality boundary against a
compromised or prompt-injected ARTEX process that already has write access to
the optional producer mount: such a process could fabricate a syntactically
valid metadata receipt. Keep the observer disabled for private research unless
the ARTEX container, its mounted outbox, and the receiving channel are all in
the approved disclosure scope. The local database and evidence store remain
the authoritative records.

## Delivery and retention behavior

The sender is deliberately at-least-once:

1. It takes an exclusive `flock` in its own state directory, so two manual or
   user-systemd instances cannot process the same receipt concurrently.
2. It validates the JSON receipt before touching Hermes.
3. It invokes Hermes with a fixed argument layout:

   ```text
   hermes send --to <operator-supplied-target> <validated-message>
   ```

   Bash never evaluates a payload as code, and the event cannot supply the
   target. Hermes stdout/stderr are discarded to prevent unexpected provider
   output entering shell history or the system journal.
4. Only after a zero exit status does the sender atomically create its private
   local delivery marker. The ARTEX receipt remains unchanged. A failed send
   creates a sender-owned one-batch retry marker instead. The next batch skips
   that one old receipt without consuming its `--max-events` budget, so a
   permanently failing earliest event cannot starve newer valid status; the
   following batch retries it unless ARTEX has evicted it.

There is an unavoidable crash window after Hermes accepts a message and before
the marker write. A retry can therefore produce a duplicate. Every message
contains the immutable `event_id`, so the receiving workflow can deduplicate.
Deleting the sender's state directory also permits historical receipts to be
sent again. On a normal non-dry sender start, markers whose corresponding
producer receipt has already been evicted are removed automatically; markers
for still-pending receipts remain, so sender state does not grow with ARTEX's
historical database.

ARTEX bounds `pending/` to 1,024 receipts. When it is full, it evicts the
oldest pending receipt before it records a new event. This is intentional:
observability must not block a running task. Treat the feed as live status, not
as the evidence system of record; the ARTEX database and local artifact store
remain authoritative.

## One-time root provisioning

This is the only observer setup step that needs `sudo`. It does not touch
Hermes credentials and does not start ARTEX, Docker, or Hermes. Inspect the
script first, then run it with the non-root account that owns the already
working Hermes login:

```bash
cd /home/ubuntu/ARTEX
sudo scripts/provision-hermes-observer.sh --user ubuntu
```

It creates:

```text
/var/lib/artex-hermes-observer          root:artex-observer 0750
/var/lib/artex-hermes-observer/pending  root:artex-observer 2750
```

and adds `ubuntu` to the `artex-observer` group. Start a new login session
(or otherwise refresh group membership) before running the sender. Do **not**
replace this with `chmod 777`, a directory under `ARTEX/data`, or a root sender
that reads the Hermes home directory.

At ARTEX startup on the supported Unix/Docker path, the observer also verifies
that both mount directories are owned by the ARTEX runtime user, share a group,
are not group/world writable, and that `pending/` retains its setgid bit. A
directory owned by the Hermes-login user is rejected. Do not add a write ACL
for that user: POSIX ACLs are an administrative deployment boundary and cannot
be proven safe by the container-side check.

For a non-mutating preflight of the provisioning plan:

```bash
sudo scripts/provision-hermes-observer.sh --user ubuntu --dry-run
```

## Enable the optional Compose mount

The ordinary `docker-compose.yml` intentionally has no observer bind mount.
In ARTEX's `.env`, set only these two observer values:

```dotenv
ARTEX_HERMES_OBSERVER_ENABLE=1
ARTEX_HERMES_OBSERVER_HOST_DIR=/var/lib/artex-hermes-observer
```

Render the opt-in configuration before changing the running container:

```bash
docker compose -f docker-compose.yml -f docker-compose.hermes-observer.yml config
```

The overlay fails if the root provisioned path is missing rather than creating
a mutable fallback. After reviewing the rendered bind mount, recreate ARTEX:

```bash
docker compose -f docker-compose.yml -f docker-compose.hermes-observer.yml \
  up -d --force-recreate artex
```

No network port is added by this overlay. Inside the container, ARTEX sees only
the dedicated `/app/hermes-observer` directory; it cannot see the Hermes home
directory or the sender's delivery markers.

## Manual sender operation

Run this as the **same non-root UNIX user** that owns the working Hermes login:

```bash
cd /home/ubuntu/ARTEX
scripts/run-hermes-observer.sh \
  --once \
  --outbox-dir /var/lib/artex-hermes-observer \
  --to discord:#artex-bot \
  --max-events 20
```

Preview a bounded batch without calling Hermes, creating sender state, creating
a lock, or writing a delivery marker:

```bash
scripts/run-hermes-observer.sh \
  --once --dry-run \
  --outbox-dir /var/lib/artex-hermes-observer \
  --to discord:#artex-bot \
  --max-events 20
```

For a supervised long-running sender, use `--watch`. The batch cap applies on
every poll cycle; the default interval is ten seconds:

```bash
HERMES_BIN=/home/ubuntu/.local/bin/hermes \
scripts/run-hermes-observer.sh \
  --watch \
  --outbox-dir /var/lib/artex-hermes-observer \
  --to discord:#artex-bot \
  --max-events 20 \
  --interval 10
```

`--max-events` is hard-bounded to `1..100`; `--interval` is hard-bounded to
`1..3600`. Targets can use the Hermes CLI's compact channel notation, for
example `discord:#artex-bot`, but cannot contain whitespace or shell
metacharacters. This is deliberate: a target is an operator-selected routing
decision, not ARTEX data.

## Optional systemd user service

Create the unit as the Hermes-owning user, not with `sudo`. Substitute only
real local paths and an already known target. Do not put tokens or OAuth values
in this file.

```ini
# ~/.config/systemd/user/artex-hermes-observer.service
[Unit]
Description=ARTEX metadata-only Hermes observer sender
After=default.target

[Service]
Type=simple
WorkingDirectory=/home/ubuntu/ARTEX
Environment=HERMES_BIN=/home/ubuntu/.local/bin/hermes
ExecStart=/home/ubuntu/ARTEX/scripts/run-hermes-observer.sh --watch --outbox-dir /var/lib/artex-hermes-observer --to discord:#artex-bot --max-events 20 --interval 10
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true

[Install]
WantedBy=default.target
```

After reviewing the rendered unit, enable it with the ordinary user's systemd
instance:

```bash
systemctl --user daemon-reload
systemctl --user enable --now artex-hermes-observer.service
systemctl --user status artex-hermes-observer.service
```

`systemctl --user` does not guarantee persistence after logout on every host.
If a persistent user service is desired, configure user lingering through the
host's normal administrative process after considering the security impact.

## Recovery and review

```bash
# Inspect only receipt filenames and sender marker names; do not paste receipt
# contents into chat.
find /var/lib/artex-hermes-observer/pending -maxdepth 1 -type f -printf '%f\n'
find "${XDG_STATE_HOME:-$HOME/.local/state}/artex-hermes-observer/delivered" -maxdepth 1 -type f -printf '%f\n'
find "${XDG_STATE_HOME:-$HOME/.local/state}/artex-hermes-observer/retry" -maxdepth 1 -type f -printf '%f\n'

# Stop the optional user service without touching Hermes itself.
systemctl --user stop artex-hermes-observer.service
```

An invalid receipt is intentionally retained until ARTEX's bounded queue needs
space. Repair the producer rather than weakening the sender. Never copy raw
ARTEX artifacts into a status message merely to make a bot update more
descriptive.
