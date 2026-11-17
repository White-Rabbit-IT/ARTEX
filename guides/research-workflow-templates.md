# Safe research workflow templates

The safe research workflow is a set of four fixed preparation templates. It is
for planning an authorized engagement before an operator creates a normal ARTEX
task. It is **not** an authorization, a scan, a provider test, or a security
assessment.

## Create the templates

Sign in to ARTEX, open **Tasks**, select **Safe research templates**, and
confirm **Create templates**. The action calls the authenticated
`POST /api/research-workflow-templates` endpoint with an empty body.

The action creates these reusable templates:

1. `01 Authorized research intake — local/synthetic first`
2. `02 Local source attack-surface study`
3. `03 Isolated local validation`
4. `04 Evidence, novelty and impact review`

The first successful call returns `201 Created` and `created_count: 4`. An
exact later call returns `200 OK` and `created_count: 0`, with the same template
records. The operation is idempotent.

## What the action does and does not do

The endpoint writes only the fixed rows in `task_templates`. It does not create
a task, exploration node, activity row, asset, finding, approval, credential,
provider request, model call, tool call, traffic record, browser session, or
target request.

The templates themselves contain no target, credential, provider setting,
category, or intercept/allow rule. They direct the operator to record scope,
source or target pin, data classification, evidence location, budgets, stop
conditions, and local controls before any normal task is considered.

## Conflict protection

The four names are reserved for this fixed content. If an existing user-owned
template has one of those normalized names but different description, goal,
category, or intercept-rule content, ARTEX returns `409 Conflict`. It does not
overwrite that template and rolls back the entire bootstrap transaction, so it
does not leave a partial safe-workflow set behind.

Rename or review the existing template deliberately before trying again. Do not
delete or replace a user-owned template merely to make the bootstrap succeed.

## Using a template later

In **Tasks → New task**, choose a template to copy its description and goal into
the ordinary task form. Selecting a template does not create or start a task.
If an operator later submits that normal task, the ordinary ARTEX task and
provider controls apply; this guide does not bypass them.

Before creating any runnable task, independently verify the written
authorization, precise target or source scope, expiry, permitted and forbidden
actions, source/target revision, data boundary, evidence root, model route and
fallback behavior, tool permissions, budgets, approval path, rollback path,
and stop conditions. Keep private-local data and secrets out of external
providers unless a separate approved data-governance process permits it.

## Limits

The workflow proves only that ARTEX can store and display reusable planning
text. It does not prove that an LLM is configured, Hermes is reachable, a
provider has the intended privacy controls, an agent is safe to run, a target
is in scope, or a vulnerability exists.

Use [CAL-00](synthetic-calibration.md) separately to verify the inert
task-list and graph display path. A runnable calibration requires an isolated
local/synthetic fixture, a verified provider route, explicit egress and tool
controls, and a complete run manifest.
