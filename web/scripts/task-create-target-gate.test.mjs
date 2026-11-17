import ts from "typescript";

import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import vm from "node:vm";

const web = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

function loadCreateTaskPayload() {
  const source = fs.readFileSync(path.join(web, "src/lib/api.ts"), "utf8");
  const output = ts.transpileModule(source, {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
  }).outputText;
  const exports = {};
  const require = (specifier) => {
    if (specifier === "@/lib/mock/enabled") return { MOCK: false };
    if (specifier === "@/lib/mock/handler")
      return { mockHandle: () => Promise.reject(new Error("unexpected mock call")) };
    throw new Error(`unexpected runtime import: ${specifier}`);
  };
  vm.runInNewContext(output, { exports, require });
  return exports.createTaskPayload;
}

test("browser task payload always enables target approval before task creation", () => {
  const createTaskPayload = loadCreateTaskPayload();
  const input = {
    name: "approved browser task",
    description: "synthetic fixture",
    goal: "verify the create payload only",
  };

  const payload = createTaskPayload(input);
  assert.equal(payload.target_gate, true);
  assert.deepEqual(JSON.parse(JSON.stringify(payload)), {
    name: "approved browser task",
    category_id: null,
    description: "synthetic fixture",
    goal: "verify the create payload only",
    llm_profile_ids: [],
    llm_rotate: false,
    source_task_ids: [],
    company_ids: [],
    timeout_seconds: 0,
    seed_first_intent: false,
    plan_heartbeat_seconds: 0,
    coverage_enabled: true,
    target_gate: true,
    intercept_rules: [],
    credentials: [],
  });

  // Extra, stale form data cannot downgrade the browser payload. The API input
  // type intentionally does not expose a targetGate switch in this rollout.
  assert.equal(createTaskPayload({ ...input, targetGate: false }).target_gate, true);
});
