# CAL-00: safe UI calibration

CAL-00 is a fixed, terminal ARTEX record for checking the task-list and exploration-graph path
before an operator admits a real task. It is a **tooling result**, not a scan, a model evaluation,
or a vulnerability result.

## Run it

Sign in to ARTEX, open **Tasks**, select **Safe UI calibration**, and confirm **Create CAL-00**.
The action calls the authenticated `POST /api/calibration/synthetic` endpoint with an empty body.
It is idempotent: later clicks return the same record instead of creating another task.

## What it creates

The server inserts one task directly in its terminal `done` state. It never calls the normal task
creation path, planner, worker, chat agent, model provider, browser, shell, MCP server, traffic
proxy, or target gate. Its fixed graph is:

```text
synthetic origin fact
  -> terminal CAL-00 provenance intent
      -> synthetic fixture fact
      -> limitation fact
```

One explicitly labelled synthetic app asset anchors that graph. It is not a host, URL, target, or
scope entry. Therefore the honest record is **one synthetic fixture asset**, not “zero assets.”

CAL-00 is a persisted ARTEX system record (`system_task_kind: synthetic_calibration`), not an
ordinary engagement. It remains readable and can be selected for inspection, but ARTEX rejects
task edits, goals, scopes, assets, credentials, provider bindings, chat, uploads, archival, deletion,
and agent admission with `409 Conflict`. The engine also has an independent no-run guard, so a
future route cannot convert CAL-00 into planner or worker execution by accident.

Those controls use the persisted task classification rather than only the live task cache. They
also exclude CAL-00 from scheduler-triggered conversations and task inheritance, reject its
fixture asset as ordinary task context, and quarantine any old archive/recovery or global
approval/target-pin record before it can alter the receipt or start provider work after a restart.

## What to verify in the UI

- The task row is named `ARTEX CAL-00 — synthetic UI calibration` and is already **Completed**.
- The task detail graph shows the fixed origin, terminal intent, and two facts.
- The task has no real scope rows, target pins, approvals, credentials, findings, traffic, LLM
  records, tool usage, or planner/worker activity.
- A repeat click reports that the existing calibration record was reused.

The database helper checks this invariant on replay and fails closed if a previously stored CAL-00
record was altered or acquired execution/evidence data.

## Limits

CAL-00 proves only that ARTEX can preserve and display a terminal synthetic provenance graph. It
does **not** prove that an LLM is configured, Hermes is reachable, an agent is safe to run, a target
is authorized, an approval gate works, or a vulnerability exists. A runnable planner/worker
calibration needs a separately isolated lane, a verified provider route, and explicit egress/tool
controls.
