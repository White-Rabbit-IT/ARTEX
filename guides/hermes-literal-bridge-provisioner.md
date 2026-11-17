# Owner-reviewed literal-bridge provisioner

This optional source-only provisioner supports a future local ARTEX-to-Hermes
route on a multi-bridge Docker host. It is not a shortcut around the normal
preflight, provider review, or ARTEX calibration policy.

The installer defaults to a zero-write plan. Its `--apply` mode writes a
root-owned helper and two systemd units, then performs only `systemctl
daemon-reload`. It does **not** enable, start, restart, or stop a service; it
does not invoke Hermes; and it does not apply firewall rules. The installed
firewall helper is separately plan-only until an owner runs its own `--apply`
after a fresh live topology review.

Run the firewall helper's plan through `sudo` as well. Many hosts require
root/CAP_NET_ADMIN just to read `iptables` INPUT rules; `sudo` alone does not
make the helper change anything. Only the explicit `--apply` flag permits a
mutation.

Before either apply flag, preserve a fresh result from
`scripts/preflight-hermes-subscription-proxy.sh --port 864`, confirm no active
ARTEX task, and review the current Docker bridge/container ownership. On this
multi-bridge topology, the old `host.docker.internal` alias check is supporting
diagnostic evidence only: a blocked alias is expected and must not be bypassed.
The literal helper's fresh no-write plan is the decisive topology check. The
helper refuses
anything except one running `artex` service on one private user-defined bridge
whose members all belong to the requested Compose project. It also proves that
the Docker-reported gateway is actually assigned to that exact bridge, rejects
an existing listener on TCP 864, and refuses any pre-existing unowned INPUT
rule for that port rather than trying to reason around a broader policy. At
apply time it uses only an owned `iptables-nft` INPUT chain: one allow rule for
the current ARTEX source and then a drop rule for all other packets to the
literal bridge gateway on TCP 864.

The firewall unit is bound to `docker.service` and has an explicit narrow
`ExecStop` teardown. Its helper's `--remove --apply` mode is the only removal
path. Before the first destructive operation, it inserts an exact,
top-priority, temporary INPUT `DROP` rule for the literal endpoint. It then
re-inspects exact state before each next transition: allow rule, ordinary
INPUT jump, terminal drop, owned chain, route record, then temporary guard.
If a concurrent firewall change makes a transition unsafe, teardown fails and
the temporary deny remains. If another manager inserted a rule above that
deny, the failure handler prepends one more exact deny before it returns; a
later retry accepts only that bounded two-guard recovery state after the
foreign rule/reference is resolved. More than two guards, malformed rules, or
external chain references are refused. The helper does not flush a global
chain or delete a rule it cannot prove it owns. The default `--remove`
invocation is still a no-write plan.

The installer intentionally refuses to overwrite an existing managed helper or
unit. That makes an upgrade an explicit owner review rather than a silent
replacement. Do not delete an old unit or firewall rule by guess. Preserve the
current file hashes, run the helper's teardown plan, and use the verified
owner-maintained replacement procedure only after reviewing its output.

## Source and lifecycle receipt

Treat `sudo ... --apply` as a small deployment, not as a convenient copy
command. Before it, retain the checked-out commit ID, a clean/dirty Git status,
the installer dry-run output and its source hashes, the exact nonsecret command,
and the user/home/binary metadata that the installer checks. The intended
Hermes user is trusted; no other local account may be able to modify its home,
launcher, or any ancestor directory. The installer rejects a group- or
world-writable component in those paths.

A Compose network recreation is not an in-place update. Stop the bridge units
as one systemd transaction so the proxy stops before the firewall teardown,
remove the old verified route, recreate and inspect the ARTEX network, run a
fresh literal-helper plan, and obtain new owner approval before applying and
enabling a new route. Do not manually invoke `--remove --apply` while Hermes is
still listening: the helper deliberately refuses that state so it cannot remove
the restrictive endpoint boundary under a live proxy.

## Why the reviewed bridge uses TCP 864

Hermes' local OpenAI-compatible listener has no independent inbound peer
authentication. A high port such as TCP 8645 can be claimed by an ordinary
local account in the interval between a firewall preflight and a non-root
Hermes process starting or restarting. The reviewed units instead use TCP 864,
which is privileged on a normal Linux host. Before it writes any route, the
helper checks `net.ipv4.ip_unprivileged_port_start` and refuses if that host
would allow an ordinary account to bind TCP 864. The proxy unit runs as the
ordinary Hermes-login owner but receives only `CAP_NET_BIND_SERVICE`; it does
not run as root and receives no broader capability.

This constrains ordinary local accounts. It is not protection from root, an
administrator who can alter systemd/firewall state, or a principal already able
to control the ARTEX Docker project. Those principals are outside this narrow
listener-ownership control and must be treated as trusted deployment owners.

Never point this at a public, WireGuard, wildcard, or guessed alias address.
Do not enable either installed unit until the owner separately approves the
runtime topology, exact Hermes CLI, route boundary, profile behavior, and one
public/synthetic route qualification. The provisioner never changes ARTEX
profiles or enables `ARTEX_HERMES_SUBSCRIPTION_ALLOW_AGENT_EXECUTION`.
