# Hermes literal-bridge review templates

These files are **not deployable units**. They are non-executable review
templates for a future, separately approved, local-only ARTEX-to-Hermes route.
The staging helper copies them to a user-selected non-system directory. It does
not write `/etc`, talk to Docker or Hermes, create a listener, modify a
firewall, enable a service, or create an ARTEX profile.

Do not replace the placeholders and copy these files into a systemd directory.
They intentionally reference a root helper that is not supplied by this
release. A live design needs all of the following to be reviewed against the
host at activation time:

1. A fresh, read-only ARTEX topology preflight.
2. Exactly one ARTEX container on an exclusive bridge, with a freshly resolved
   bridge interface, container IPv4 address, and gateway.
3. A root-owned helper that installs only an owned **INPUT** rule for TCP 8645:
   allow that one ARTEX source first, then drop every other input to the bridge
   gateway and port.
4. A Hermes proxy owned by the ordinary Hermes-login user, bound only to the
   literal bridge gateway, never to a WireGuard, public, or wildcard address.
5. An explicit owner decision before any service enablement, listener start,
   firewall change, profile save, or synthetic provider qualification.

The templates therefore document intended service ownership and ordering, not a
shortcut around those checks.
