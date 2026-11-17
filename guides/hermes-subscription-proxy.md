# Hermes Subscription Proxy with ARTEX

This guide configures a **local Hermes Subscription Proxy** as an ARTEX LLM
profile. It does not put a Hermes OAuth token, OpenAI API key, or Portal MCP
credential into ARTEX.

> **Shared/multi-bridge hosts:** Do not use this older manual `:8645` example
> to activate a new route. It cannot reserve listener ownership from an
> unprivileged local process. For the ARTEX VPS topology, use the separately
> reviewed, source-only
> [literal-bridge provisioner](hermes-literal-bridge-provisioner.md), which
> stages a privileged `:864` listener with the minimum bind capability and does
> not activate it. The manual material below remains useful for understanding
> ARTEX's profile format and for read-only preflight, not as a substitute for
> that owner-reviewed deployment path.

The reviewed topology is deliberately private:

```text
ARTEX container
  -> verified private Docker bridge route on :864
  -> Hermes Subscription Proxy on this same host
  -> Hermes-owned authenticated subscription
```

`docker-compose.yml` supplies `host.docker.internal:host-gateway` to the ARTEX
container. It does **not** publish port 864. That alias is only a candidate,
not proof of isolation: on a multi-bridge Docker host it can resolve to the
default bridge instead of ARTEX's project bridge. Hermes' proxy has no separate
inbound authentication, so a bad bind can expose a subscription-backed route to
other local containers. Never bind it to `0.0.0.0`, a WireGuard address, or a
public interface.

## Preconditions

1. ARTEX is installed from this fork and its Compose stack is running.
2. The intended UNIX user already owns a working Hermes login. Run Hermes
   interactive login separately, for example through the normal `hermes portal`
   flow for the installed CLI version.
3. You have authorization for any future ARTEX target. This guide only tests a
   local model route; it does not create a task, target, scan, traffic capture,
   or browser session.

Do not run the Hermes login as root just to reach ARTEX. The login must remain
with the ordinary user that will operate Hermes.

## 1. Run the read-only bridge preflight

From the ARTEX project directory, run:

```bash
scripts/preflight-hermes-subscription-proxy.sh --port 864
```

The helper is intentionally read-only. It does not start Hermes, read a Hermes
login, change Docker or firewall state, make a provider request, create an
ARTEX profile, or call a model. It checks the ARTEX network/gateway, resolves
`host.docker.internal` from the container, counts containers from another
Compose project on either relevant bridge, and checks whether TCP port 864 is
already in use.

If it exits `2` or prints `result=BLOCKED`, do not start a proxy or create an
ARTEX profile from that alias. In particular, do not treat the current alias as
safe when it resolves to a bridge shared by unrelated containers. For a
single-bridge alias route, correct the Docker alias or bridge mapping itself
and re-run the preflight. For the multi-bridge VPS topology, preserve that
blocked alias receipt and use the literal-bridge provisioner below instead; do
not substitute a WireGuard or public address.

An exit status of `0` prints `result=TOPOLOGY_REVIEW_REQUIRED`; it means only
that this static check found no condition it knows how to reject. It does not
authorize a proxy start, an external model request, a task, or agent execution.
Before continuing, review the listener/firewall policy so only the intended
ARTEX project can reach the selected host address. Copy its
`suggested_profile_base_url` only after that review.

### Multi-bridge hosts: reviewed literal bridge route

If the preflight blocks because `host.docker.internal` resolves to a shared
bridge, do **not** point Hermes at that alias and do not paste a guessed private
address into ARTEX. The `artex_network_gateway` line is diagnostic data, not an
approval to use it.

Use the separately reviewed
[literal-bridge provisioner](hermes-literal-bridge-provisioner.md) instead.
It discovers the current literal bridge values at the time of application,
uses a root-owned `iptables-nft` INPUT boundary, and reserves its listener on
TCP 864 with only `CAP_NET_BIND_SERVICE` for the non-root Hermes process. It
still requires separate owner approval before it can be enabled or receive a
public/synthetic qualification request. Docker bridge names, gateway addresses,
and container addresses can change after a Compose network is recreated; the
route must be rediscovered and re-reviewed rather than reused.

## 2. Do not manually start a bridge listener

The former manual `hermes proxy start ... :8645` recipe is retired for a shared
or multi-bridge host. It could leave an unprivileged listener race and had no
coordinated firewall lifecycle. Use the
[literal-bridge provisioner](hermes-literal-bridge-provisioner.md) as the only
reviewed deployment path for this topology. It stages units but still does not
enable or start them; a later owner-approved activation must use its generated
route, privileged port and systemd ordering.

Do not give ARTEX access to `~/.hermes`, do not bind a proxy publicly, and do
not add a Docker volume for Hermes auth. A local connectivity response would
prove neither a model context window nor a provider's retention, privacy, or
tool-use policy.

## 3. Create the ARTEX profile

In **System → LLM Profiles**, create a profile with these values:

| Field | Value |
| --- | --- |
| Format | `Hermes Subscription Proxy (local, OAuth held by Hermes)` |
| Base URL | The exact private `/v1` URL from the owner-approved literal-bridge route, after firewall review |
| Model | Select an exact ID returned by **Load models** |
| API Key | Leave empty — ARTEX ignores and clears it for this format |
| ARTEX LLM proxy | Leave empty — Hermes is direct-only and rejects it |
| Pool participation | Forced off — Hermes is never an automatic fallback |

Use **Test connection**. The ARTEX test sends only its fixed, synthetic `ping`
and expects a short `OK` response. It does not require a task, source file,
target, credential, capture, or model prompt from an engagement.

ARTEX accepts only a plain local HTTP endpoint for this profile. It rejects a
public hostname, an HTTPS URL, a query string, URL credentials, or a path other
than the OpenAI-compatible `/v1` root.

Use this explicit format, not a generic `OpenAI` profile pointed at the
verified local `/v1` route. ARTEX rejects that known
misconfiguration before it can fall back to a saved or environment API key. The
local ports `864` and legacy `8645` are reserved for this Hermes integration;
changing the path, query string, or URL fragment does not make them a
generic-provider route.
An endpoint URL alone cannot prove what local service owns a custom port, so a
Hermes proxy moved to another port must still be configured with the explicit
`Hermes Subscription Proxy` format.

## 4. Execution policy: calibration first

By default, a Hermes Subscription Proxy profile is **calibration-only**:

- profile model discovery and the fixed connection test are available;
- ARTEX task agents, chat agents, side questions, profile bindings, and task
  profile chains are refused;
- ARTEX does not silently fall back from a blocked Hermes profile to another
  configured provider.

This is intentional. Normal ARTEX agents can use file and shell tools. A
working proxy route alone does not make it appropriate to send task text,
private files, traffic, evidence, or a research workspace to an external model.

The process-level escape hatch is deliberately exact:

```dotenv
ARTEX_HERMES_SUBSCRIPTION_ALLOW_AGENT_EXECUTION=1
```

It is disabled for an unset value and for values such as `true`, `yes`, or `0`.
Set it only after a separate scope, data-boundary, tool-policy, and provider
review. Restart or recreate the ARTEX container for an environment change to
take a predictable effect. It is not a statement that a particular model route
is approved for private research.

## 5. Optional metadata-only Hermes bot feed

The observer is independent of the LLM profile. When explicitly enabled, ARTEX
writes local, redacted activity receipts; a host-side sender then uses the
existing Hermes CLI login to post them to a bot channel.

Follow [the Hermes observer guide](hermes-observer.md). The feed contains
receiver-visible event IDs, opaque task/activity IDs, event type, selected
safe labels, and error state. It does not contain raw prompts, model output,
task goal, traffic, headers, files, paths, evidence, secrets, content hashes,
or content lengths.

## Troubleshooting and rollback

| Symptom | Check | Safe response |
| --- | --- | --- |
| `Load models` fails | Proxy listener, Docker alias, `/v1/models` route | Fix the local proxy binding; do not use a public endpoint as a shortcut. |
| Preflight blocks on a shared host alias | Alias network and foreign-container count | Keep the alias route disabled. Use the separately reviewed literal-bridge design above; do not bypass the warning by pasting an arbitrary private address. |
| Connection test fails | Exact model ID, proxy login state, `/v1/chat/completions` support | Re-run only the local synthetic test after inspecting the proxy locally. |
| Profile saves but tasks/chat are unavailable | `ARTEX_HERMES_SUBSCRIPTION_ALLOW_AGENT_EXECUTION` is not exactly `1` | This is expected calibration mode. Keep it disabled until an explicit review. |
| A request appears to use another provider | Check the selected profile and ARTEX pool state | Hermes is excluded from failover. Do not enable pool fallback to work around a local proxy failure. |

To roll back, select another valid active ARTEX LLM profile or delete the Hermes
profile, stop the local Hermes proxy using its own CLI or service controls, and
remove the optional environment variables. ARTEX never stores the Hermes OAuth
session, so no token cleanup is required inside ARTEX.
