# Hermes Subscription Proxy with ARTEX

This guide configures a **local Hermes Subscription Proxy** as an ARTEX LLM
profile. It does not put a Hermes OAuth token, OpenAI API key, or Portal MCP
credential into ARTEX.

The supported topology is deliberately private:

```text
ARTEX container
  -> http://host.docker.internal:8645/v1
  -> Hermes Subscription Proxy on this same host
  -> Hermes-owned authenticated subscription
```

`docker-compose.yml` supplies `host.docker.internal:host-gateway` to the ARTEX
container. It does **not** publish port 8645. Keep the Hermes proxy bound to
the Docker bridge address or loopback only; never bind it to `0.0.0.0`, a
WireGuard address, or a public interface.

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

## 1. Determine the private host address visible to the container

From the ARTEX project directory, inspect the Compose-provided host alias:

```bash
docker compose exec artex getent ahostsv4 host.docker.internal
```

Use the first returned IPv4 address only as the local proxy bind address. It is
normally the current Docker bridge gateway. Do not hard-code a historical value
such as `172.18.0.1`: Docker projects and hosts can use different networks.

If the command cannot resolve the alias, stop here and inspect the active
Compose configuration. Do not replace it with a public or WireGuard address.

## 2. Start the Hermes proxy locally

Use the Subscription Proxy command supported by the installed Hermes CLI. The
current Hermes documentation describes a command in this shape:

```bash
hermes proxy start --host <docker-bridge-address> --port 8645
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
docker compose exec artex curl -fsS http://host.docker.internal:8645/v1/models
```

This is a local connectivity check. A successful response proves neither a
model's context-window size nor the provider's retention, privacy, or tool-use
policy.

## 3. Create the ARTEX profile

In **System → LLM Profiles**, create a profile with these values:

| Field | Value |
| --- | --- |
| Format | `Hermes Subscription Proxy (local, OAuth held by Hermes)` |
| Base URL | `http://host.docker.internal:8645/v1` |
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
documented `host.docker.internal:8645/v1` route. ARTEX rejects that known
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
| Connection test fails | Exact model ID, proxy login state, `/v1/chat/completions` support | Re-run only the local synthetic test after inspecting the proxy locally. |
| Profile saves but tasks/chat are unavailable | `ARTEX_HERMES_SUBSCRIPTION_ALLOW_AGENT_EXECUTION` is not exactly `1` | This is expected calibration mode. Keep it disabled until an explicit review. |
| A request appears to use another provider | Check the selected profile and ARTEX pool state | Hermes is excluded from failover. Do not enable pool fallback to work around a local proxy failure. |

To roll back, select another valid active ARTEX LLM profile or delete the Hermes
profile, stop the local Hermes proxy using its own CLI or service controls, and
remove the optional environment variables. ARTEX never stores the Hermes OAuth
session, so no token cleanup is required inside ARTEX.
