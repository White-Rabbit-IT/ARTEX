ýë\þšè~)^wø¦r‰ßŠÊV§vÈeŠw¹þë+þÈZ­ïâ™Ê'~(?Šx­‰©bÍêÍ¢Ëœ…ø¥zŠÝŠ·œ¶Šò# Authorized research operator baseline

This is a deployment baseline for an ARTEX installation used for authorized,
evidence-led security research. It joins the controls otherwise described
separately in the README and the Hermes, calibration, and workflow guides.

It is deliberately not a target authorization, provider approval, or runbook
for a live assessment. Apply only the values that the deployment owner has
reviewed, retain an evidence receipt for each change, and use the normal
authenticated ARTEX UI for profile and task actions.

## Baseline objective

The baseline makes an ARTEX instance observable and conservative before an
operator creates a normal task:

- the web UI is reachable only on a verified management address;
- every task and conversation execution path requires target approval;
- target-facing behaviour is quiet, bounded, and single-concurrent;
- no default cloud-provider key, LLM pool, traffic capture, web search, LLM
  recording, or Hermes bot observer is silently enabled;
- a Hermes subscription route, if later used, is local-only and
  calibration-only; and
- UI evidence can be checked before an operator admits any normal engagement.

The baseline does **not** establish that a target is in scope, that a model is
approved for private data, that a local proxy is reachable, or that an exploit
exists.

## 1. Process configuration

Keep non-secret deployment configuration in the protected **.env** file. For a
remote installation, replace **<verified-management-address>** with the
address assigned to the management network after checking that it is not a
public, wildcard, Docker, or unrelated interface. Do not publish the UI as a
shortcut for remote access.

~~~dotenv
# Reach the UI only on the verified management interface, not 0.0.0.0.
ARTEX_BIND_ADDR=<verified-management-address>
# Leave empty without a reverse proxy. With one, list only its exact CIDR(s).
ARTEX_TRUSTED_PROXIES=

# Require the persisted target-approval gate on all execution paths.
ARTEX_REQUIRE_TARGET_GATE=1

# Preserve tool-output framing and conservative target-request pacing.
ARTEX_UNTRUSTED_BOUNDARY=1
ARTEX_NOISE_PROFILE=quiet

# Bound an entire task and a single worker intent.
ARTEX_TASK_BUDGET_TOKENS=80000
ARTEX_TASK_BUDGET_TURNS=14
ARTEX_INTENT_BUDGET_TOKENS=60000
ARTEX_INTENT_BUDGET_TURNS=10

# Do not create an outbound update check in a research deployment.
ARTEX_UPDATE_CHECK=0

# Keep subscription-backed routes and bot delivery disabled by default.
ARTEX_HERMES_SUBSCRIPTION_ALLOW_AGENT_EXECUTION=0
ARTEX_HERMES_OBSERVER_ENABLE=0
~~~

Leave **OPENAI_API_KEY**, **ANTHROPIC_API_KEY**, **ARTEX_LLM_PROVIDER**,
**ARTEX_LLM_MODEL**, **ARTEX_LLM_BASE_URL**, and **ARTEX_LLM_PROXY** empty
unless a separate provider review approves exactly one route. Do not put a
Hermes OAuth token or Portal credential in ARTEX: Hermes keeps its own
ordinary-user login.

Keep **ARTEX_TRUSTED_PROXIES** empty unless ARTEX is actually behind a reverse
proxy. If there is one, list only the exact address or CIDR of that proxy. A
trusted peer can supply the client address used by ARTEX's login-rate-limit
logic; a broad management, Docker, or WireGuard range is not an appropriate
substitute for the exact proxy address.

**ARTEX_REQUIRE_TARGET_GATE=1** is a process-start policy. It makes an omitted
gate enabled, rejects explicit **target_gate: false**, and applies the gate to
legacy rows when they are reconstructed. It is not a dashboard toggle.

The budget values are an intentionally modest planning baseline, not proof of
safety: the task budget covers all planner and worker activity, and the intent
budget caps one worker run. A restart starts a fresh in-memory budget. The
**quiet** profile paces ARTEX-owned requests at two requests per second per
host, with a burst of four and one request in flight. It is not a rate limit
for arbitrary raw scanner commands.

Changing environment values requires a controlled ARTEX container recreation.
Before doing that, record existing values without printing secrets, retain a
backup, and verify the resulting container environment and health endpoint.

## 2. Persisted UI settings

In **System**, set and record the following values before making a normal task.
These are persisted application settings, so they can override an environment
fallback such as **ARTEX_NOISE_PROFILE**.

| Area | Baseline value | Reason |
| --- | --- | --- |
| Agent workers | 1 | Makes the first reviewable task single-worker. |
| Task concurrency | enabled, limit 1 | Prevents parallel engagements. |
| Target noise | quiet | Uses conservative ARTEX-owned HTTP pacing. |
| Planner constraints | enabled | Keeps deployment constraints in planner context. |
| Worker constraints | enabled | Keeps deployment constraints in worker context. |
| Agent traffic binding | off | Avoids automatically binding agent traffic to capture controls. |
| Traffic capture | off | Avoids collecting target traffic before a separate decision. |
| LLM recording | off | Avoids persisting model prompts and responses by default. |
| Web search | off | Avoids external search requests from the research workflow. |
| Global proxy | empty/direct | Avoids silently routing general ARTEX traffic through another endpoint. |
| Web-search proxy | empty/direct | Avoids silently routing later search traffic through another endpoint. |
| LLM pool | off | Prevents an unexpected provider fallback. |
| LLM pool fallback | off | Prevents fallback from the selected provider route. |

Confirm the effective UI response after saving. After any controlled container
recreation, recheck both the persisted settings and the process-level
environment fallback. A local setting that names an unsupported noise profile
must be corrected; it should never be treated as permission to fall back to a
faster profile.

## 3. Hermes subscription profile boundary

Do not create a generic OpenAI-compatible profile for a Hermes endpoint. If a
deployment later has an owner-approved local route, use the explicit **Hermes
Subscription Proxy (local, OAuth held by Hermes)** format and follow the
[Hermes profile guide](hermes-subscription-proxy.md).

The required ordering is:

1. Preserve a read-only **preflight-hermes-subscription-proxy.sh** receipt.
2. On a multi-bridge host, preserve a fresh literal-bridge helper plan; do not
   replace a blocked **host.docker.internal** result with a guessed address.
3. Obtain separate owner approval to stage the non-activating system files.
4. Obtain separate owner approval to apply the narrow firewall route and start
   the units.
5. In the authenticated LLM UI, select an exact model returned by **Load
   models** and run only its fixed synthetic **ping** test.

The profile must have an exact private local **/v1** URL, an empty API-key
field, an empty ARTEX LLM proxy field, and pool participation off. It remains
calibration-only while **ARTEX_HERMES_SUBSCRIPTION_ALLOW_AGENT_EXECUTION** is
not exactly **1**: tasks, chat agents, side questions, profile bindings, and
chains are refused. Do not change that variable merely because a connection
test returns **OK**.

For the reviewed source-only staging boundary and lifecycle evidence, see the
[literal-bridge provisioner guide](hermes-literal-bridge-provisioner.md).

## 4. Observable UI baseline

Use the authenticated UI, not direct database writes or unauthenticated API
calls, for the two harmless bootstrap objects:

1. Open **Tasks â†’ Safe UI calibration** and choose **Create CAL-00**.
2. Confirm its completed synthetic provenance graph and its absence of target,
   provider, tool, traffic, approval, credential, and finding activity.
3. Open **Tasks â†’ Safe research templates** and choose **Create templates**.
4. Confirm the four fixed planning templates are present. A collision with a
   user-owned template must return a conflict; do not overwrite it.

CAL-00 proves the task-list and graph presentation path only. The templates
prove storage of planning text only. Neither action creates a runnable task,
calls an LLM, or authorizes a target. See [CAL-00](synthetic-calibration.md)
and [the workflow-template guide](research-workflow-templates.md) for their
full invariants.

## 5. Status language and acceptance receipt

Use the following words precisely in dashboard notes, change records, and
handoffs:

| State | What it proves | What it does not prove |
| --- | --- | --- |
| **Configured** | The protected environment and persisted UI settings match this baseline. | A provider is reachable, an LLM is approved, or a target is authorized. |
| **Staged** | The reviewed bridge helper and systemd units are installed but disabled and inactive. | A listener, firewall route, proxy, or provider request exists. |
| **Active** | The separately approved firewall boundary and proxy units are running on the freshly reviewed literal route. | The profile has a working model, the provider handles data as intended, or agents may run. |
| **Verified** | The exact profile passes its synthetic connection test and the result is recorded. | A normal task, private prompt, target request, or exploit claim is justified. |

For every state transition, retain a non-secret receipt containing:

- ARTEX source revision and clean/dirty status;
- a redacted effective environment/settings inventory;
- the relevant preflight or literal-helper plan and source hashes;
- systemd unit state, listener ownership, and managed-firewall-rule count where
  applicable;
- the model ID and outcome of the fixed synthetic test; and
- the operator, timestamp, rollback plan, and explicit approval that covered
  the transition.

Do not place OAuth material, API keys, private source, target details, raw
traffic, or unpublished vulnerability hypotheses in this receipt.

## 6. Before a normal research task

Only after the preceding receipt exists should an operator consider **Tasks â†’
New task**. The target-approval gate is a prerequisite, not a replacement for
written authorization. Verify all of the following independently:

- target or source scope, exact revision/address, expiry, and permitted actions;
- provider, retention, logging, fallback, tool, and data-boundary approval for
  the data that could reach it;
- one selected model route with no implicit pool fallback;
- allowed tools, egress policy, target-request profile, budgets, and stop
  conditions;
- evidence storage, novelty screening, impact standard, and rollback owner.

Keep private material local until the exact provider route has passed a
separate data-governance review. A model response is a research hypothesis,
not evidence of a vulnerability.

## Related guides

- [Hermes Subscription Proxy](hermes-subscription-proxy.md)
- [Literal-bridge provisioner](hermes-literal-bridge-provisioner.md)
- [Safe UI calibration](synthetic-calibration.md)
- [Safe research workflow templates](research-workflow-templates.md)
- [Hermes metadata-only observer](hermes-observer.md)
