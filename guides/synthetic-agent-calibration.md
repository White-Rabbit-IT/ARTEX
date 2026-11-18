# CAL-AGENT-01: bounded synthetic-agent calibration

CAL-AGENT-01 is a product-owned, narrowly scoped route qualification. It
checks the saved local Hermes Subscription Proxy profile and ARTEX's fixed
worker lifecycle. It is not a penetration test, a target check, or approval to
use an external model with private research.

## What it can do

The server creates one immutable task named
`ARTEX CAL-AGENT-01 — bounded synthetic agent calibration`.

The task has one fixed synthetic origin fact and one fixed receipt intent. Its
worker can call only the local `synthetic_receipt` tool. That tool accepts only
an empty JSON object and records one local receipt. It does not read or write
files, start a process, use a shell, contact a target, make a web request, use
a browser, use an MCP server or skill, use traffic capture, or access a proxy.

The worker receives a static prompt. It receives no task name, task ID, goal,
target, URL, source tree, filesystem path, credential, traffic, transcript, or
private research text. It can make at most two provider calls, produce at most
128 response tokens per call, and has a 90-second wall-clock limit.

Normal ARTEX task and chat agents remain disabled. This calibration does not
enable them.

## Prerequisites

Before starting the calibration:

1. Complete the Hermes bridge and firewall review in
   [the Hermes Subscription Proxy guide](hermes-subscription-proxy.md). The
   Hermes route must be private and local. Do not bind it to a WireGuard or
   public interface.
2. In **System → LLM Profiles**, save exactly one eligible Hermes Subscription
   Proxy profile. It must use the reviewed local `/v1` URL, have no ARTEX proxy,
   be excluded from the provider pool, and not be the default profile.
3. Keep `ARTEX_HERMES_SUBSCRIPTION_ALLOW_AGENT_EXECUTION` unset. That broader
   switch enables normal task and chat agent execution and is not needed here.
4. Set the narrow switch in the ARTEX `.env` file:

   ```dotenv
   ARTEX_HERMES_SYNTHETIC_CALIBRATION_ALLOW=1
   ```

5. Recreate or restart the ARTEX service so the container receives the changed
   environment. Confirm the service health before opening the UI.

The endpoint fails closed if zero profiles or more than one profile match. It
also fails closed if the saved profile, fixed task, or fixed graph differs from
the expected form.

## Start one attempt

1. Sign in to ARTEX.
2. Open **Tasks**.
3. Select **Synthetic agent calibration**.
4. Read the confirmation dialog. It must state that the run has no target,
   shell, filesystem, network, scanner, browser, MCP, skill, credential,
   traffic, or proxy access.
5. Select **Start bounded calibration**.

The request body is empty. The UI cannot submit a target, model, prompt, URL,
tool, or budget. A second click on Start only returns the existing attempt. It
does not make another provider request.

## Inspect the result

The task list shows the fixed CAL-AGENT-01 task. The activity trail records:

- `attempt_started`, including the selected profile metadata and attempt number;
- `attempt_finished`, including task state, receipt-call count, and model-call
  count; or
- a terminal interruption or failure receipt.

A successful attempt requires exactly one receipt call and exactly two model
calls. ARTEX marks every other outcome as failed. ARTEX does not automatically
retry after a failure, process restart, timeout, or provider error.

## Retry a failed attempt

Use **Retry failed calibration** only after the local Hermes route or ARTEX
configuration has been inspected. The separate retry dialog creates one new
fixed attempt only for a failed or timed-out CAL-AGENT-01 task. It preserves
prior activity and model records. It rechecks the same saved profile, task
shape, graph, and claim lease before it calls a model.

Do not use retries to add a prompt, a target, source code, logs, credentials,
or Firedancer material. If any private material needs external-model review,
first establish a separately approved data boundary; CAL-AGENT-01 does not do
that work.

## Disable the route

Set or leave this value empty in `.env`:

```dotenv
ARTEX_HERMES_SYNTHETIC_CALIBRATION_ALLOW=
```

Recreate or restart ARTEX. Existing receipts remain readable. No calibration
attempt resumes automatically, and disabling the switch does not change the
Hermes login, bridge listener, firewall, or saved profile.
