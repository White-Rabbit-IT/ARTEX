# Staging a Hermes literal-bridge design for review

This is an optional, **non-deploying** companion to the local Hermes Subscription
Proxy guide. It creates a review bundle only. It is useful when the regular
`host.docker.internal` preflight blocks on a multi-bridge host and an owner
wants to document a possible literal ARTEX-bridge route without changing that
host.

The stager has a safe default: it prints a plan and creates no files. With
`--apply`, it only copies three static `*.template` / Markdown files to a
new private review directory selected by the invoking non-root user. It cannot
run Docker, Hermes, systemd, a firewall tool, or a provider request. It refuses
root and protected prefixes including `/etc`, `/usr`, `/var`, `/run`, `/opt`,
and `/srv`. Its destination must be a new directory under an existing
non-symlinked parent; it refuses to alter, reuse, or chmod an existing
directory.

```bash
# No files are written.
scripts/stage-hermes-literal-bridge-review.sh \
  --stage-dir "$HOME/artex-hermes-literal-bridge-review"

# Only stages the review bundle under the current user's home directory.
scripts/stage-hermes-literal-bridge-review.sh \
  --stage-dir "$HOME/artex-hermes-literal-bridge-review" --apply
```

The staged bundle is intentionally incomplete. Its `*.template` files have
unresolved placeholders and must not be copied to `/etc/systemd/system` or
enabled. The bundle itself includes no root firewall helper, no service
installer, no port listener, and no profile automation. The separately reviewed
provisioner named below is the only source path that can stage system files,
and it remains non-activating.

## What a later owner-reviewed activation would have to prove

1. Run `scripts/preflight-hermes-subscription-proxy.sh --port 864` against the
   live ARTEX Compose project. The result must be preserved with its timestamp;
   on a multi-bridge host, its host-alias result is supporting evidence while a
   fresh literal-helper plan remains the decisive topology check.
2. Rediscover, rather than reuse, the one ARTEX container address, its exclusive
   Docker bridge interface, and that bridge's gateway. They can change after a
   Compose network recreation.
3. Bind Hermes only to the literal bridge gateway on TCP 864. The reviewed
   service gives the ordinary Hermes user only `CAP_NET_BIND_SERVICE`, after
   verifying that the host keeps TCP 864 privileged. Never use
   `0.0.0.0`, a public address, a WireGuard address, or an unverified
   `host.docker.internal` mapping.
4. Before the proxy starts, install a dynamically generated host **INPUT**
   boundary that allows only the current ARTEX container source on that bridge,
   then drops every other input to the gateway's port 864. Do not alter Docker
   chains, flush a firewall table, or enable a replacement global firewall
   service.
5. Keep the Hermes process under the ordinary user that owns the interactive
   login. ARTEX must not receive that user's home directory, OAuth state, token,
   or portal credentials.
6. After an explicit owner approval, perform one public/synthetic route check.
   Keep the ARTEX profile inactive and agent execution disabled until the route,
   fallback behavior, model identity, tool policy, and data boundary are all
   independently recorded.

This guidance does not authorize an ARTEX target task, scanning, exploitation,
traffic capture, or a private-research prompt.

For the separately opt-in, source-only installer that stages (but does not
enable or start) reviewed system files, see
[the literal-bridge provisioner guide](hermes-literal-bridge-provisioner.md).
