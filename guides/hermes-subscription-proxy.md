# Hermes Subscription Proxy with ARTEX

This guide configures a **local Hermes Subscription Proxy** as an ARTEX LLM
profile. It does not put a Hermes OAuth token, OpenAI API key, or Portal MCP
credential into ARTEX.

The supported topology is deliberately private:

```text
ARTEX container
  -> verified private Docker bridge route on :8645
  -> Hermes Subscription Proxy on this same host
  -> Hermes-owned authenticated subscription
```

`docker-compose.yml` supplies `host.docker.internal:host-gateway` to the ARTEX
container. It does **not** publish port 8645. That alias is only a candidate,
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
scripts/preflight-hermes-subscription-proxy.sh
```

The helper is intentionally read-only. It does not start Hermes, read a Hermes
login, change Docker or firewall state, make a provider request, create an
ARTEX profile, or call a model. It checks the ARTEX network/gateway, resolves
`host.docker.internal` from the container, counts containers from another
Compose project on either relevant bridge, and checks whether TCP port 8645 is
already in use.

If it exits `2` or prints `result=BLOCKED`, stop. In particular, do not treat
the current alias as safe when it resolves to a bridge shared by unrelated
containers. Correct the Docker alias or bridge mapping itself, then re-run the
preflight; an allow-only firewall policy is an additional later requirement,
not a replacement for a passing alias check. Do not substitute a WireGuard or
public address.

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

A host owner can instead separately review a literal ARTEX-bridge design. It
has all of these properties at the time it is applied:

```text
Hermes listener:    <current ARTEX bridge gateway>:8645
ARTEX profile URL:  http://<current ARTEX bridge gateway>:8645/v1
Allow rule:          source=<current ARTEX container IPv4>, input=<current ARTEX bridge>,
                     destination=<current ARTEX bridge gateway>, tcp destination port=8645
Drop rule:           every other input to <current ARTEX bridge gateway>:8645
```

The rules belong in the host **INPUT** path, not Docker's `FORWARD` path: a
container connecting to a host-local bridge address terminates on the host. The
allow must precede the drop. Bind only the literal bridge address; never
`0.0.0.0`, a WireGuard address, or a public address.

This is not a copy-and-paste firewall recipe. Docker bridge names, gateway
addresses, and container addresses can change after a Compose network is
recreated. The host owner must preserve the existing firewall configuration,
use its established persistent firewall mechanism, re-run this preflight after
each network recreation, and keep the rule scoped to this one port. ARTEX
accepts a literal private IPv4 `/v1` endpoint for the explicit Hermes profile,
but a literal URL alone does not establish the required isolation.

## 2. Start the Hermes proxy locally

Use the Subscription Proxy command supported by the installed Hermes CLI. The
current Hermes documentation describes a command in this shape:

```bash
hermes proxy start --provider nous --host <verified-private-bridge-address> --port 8645
```

Before starting it, verify the flags for the installed version:

```bash
hermes proxy start --help
```

Run it as the Hermes-login owner. Do not give ARTEX access to `~/.hermes`, do
not bind the proxy publicly, and do not add a Docker volume for Hermes auth.

In a second terminal, verify only the local OpenAI-compatible surface from the
ARTEX container:

```bash
docker compose exec artex curl -fsS http://<verified-private-bridge-address>:8645/v1/models
```

This is a local connectivity check. A successful response proves neither a
model's context-window size nor the provider's retention, privacy, or tool-use
policy.

## 3. Create the ARTEX profile

In **System → LLM Profiles**, create a profile with these values:

| Field | Value |
| --- | --- |
| Format | `Hermes Subscription Proxy (local, OAuth held by Hermes)` |
| Base URL | The exact private `/v1` URL from the non-blocked preflight, after firewall review |
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
entire local port `8645` is reserved for this Hermes integration; changing the
path, query string, or URL fragment does not make it a generic-provider route.
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
