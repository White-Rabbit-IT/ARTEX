package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

func TestSyntheticCalibrationEndpointIsAuthenticatedTerminalAndIdempotent(t *testing.T) {
	g := newGateServer(t)

	// CAL-00 is an authenticated administrative operation. It must not become a
	// public unauthenticated task-creation shortcut merely because it is inert.
	unauthenticated := httptest.NewRecorder()
	g.s.Handler().ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodPost, "/api/calibration/synthetic", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d, want %d", unauthenticated.Code, http.StatusUnauthorized)
	}

	// There is no caller-provided prompt or target surface. A nonempty body is
	// rejected before the manager/database helper is reached.
	if code, _ := g.do(t, http.MethodPost, "/api/calibration/synthetic", `{}`); code != http.StatusBadRequest {
		t.Fatalf("nonempty calibration body status=%d, want %d", code, http.StatusBadRequest)
	}

	code, first := g.do(t, http.MethodPost, "/api/calibration/synthetic", "")
	if code != http.StatusCreated || first["kind"] != "synthetic_calibration" || first["runnable"] != false || first["provider_called"] != false || first["already_present"] != false {
		t.Fatalf("first calibration response: status=%d body=%v", code, first)
	}
	taskMap, ok := first["task"].(map[string]any)
	if !ok {
		t.Fatalf("first calibration task=%T, want object", first["task"])
	}
	id, ok := taskMap["id"].(string)
	if !ok || id == "" {
		t.Fatalf("first calibration task id=%v", taskMap["id"])
	}
	numericID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Production code deliberately refuses to delete CAL-00. This test-only
		// cleanup reaches the DB primitive directly so its fixed bundle does not
		// leak into a shared integration database.
		if _, err := g.m.pg.DeleteTaskCascade(numericID, true, true, true); err != nil {
			t.Errorf("cleanup CAL-00: %v", err)
		}
		g.m.forgetTask(id, numericID)
	})

	live, found := g.m.Task(id)
	if !found || live == nil || !live.IsSyntheticCalibration() || live.Status != "done" || live.Paused || live.Queued || !live.targetGate.Load() || live.CoverageEnabled || live.FirstRunAt != 0 {
		t.Fatalf("live CAL-00 task not terminal/inert: found=%t task=%+v", found, live)
	}
	if g.s.engine.Started(id) {
		t.Fatalf("CAL-00 task %s was admitted to the engine", id)
	}
	if stored, err := g.m.pg.GetTask(numericID); err != nil || stored == nil || !stored.IsSyntheticCalibration() || stored.FirstRunAt != nil || stored.Status != "done" {
		t.Fatalf("stored CAL-00 task=%+v err=%v", stored, err)
	}
	var fixtureAssetID int64
	var fixtureBundleID string
	if err := g.m.pg.QueryRow(`
SELECT asset.id, COALESCE(asset.bundle_id,'')
FROM assets asset
JOIN task_asset_links link ON link.asset_id=asset.id
WHERE link.task_id=$1`, numericID).Scan(&fixtureAssetID, &fixtureBundleID); err != nil {
		t.Fatalf("load CAL-00 fixture asset: %v", err)
	}
	if fixtureAssetID <= 0 || fixtureBundleID == "" {
		t.Fatalf("invalid CAL-00 fixture asset id=%d bundle=%q", fixtureAssetID, fixtureBundleID)
	}

	// The task stays inspectable in the ordinary UI but all normal task-write
	// routes are blocked before they can create graph/evidence rows or revive it.
	for _, request := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPatch, "/api/tasks/" + id, `{"name":"must not rename"}`},
		{http.MethodPost, "/api/tasks/" + id + "/goals", `{"text":"must not become a real task"}`},
		{http.MethodPut, "/api/tasks/" + id + "/llm", `{"llm_profile_ids":[]}`},
		{http.MethodPost, "/api/tasks/control/batch", `{"task_ids":["` + id + `"],"action":"pause"}`},
		{http.MethodPost, "/api/tasks/category/batch", `{"task_ids":["` + id + `"],"category_id":null}`},
		{http.MethodPost, "/api/tasks/archive/batch", `{"task_ids":["` + id + `"]}`},
		{http.MethodPost, "/api/chat?task=" + id, `{"message":"must not create activity"}`},
		{http.MethodPost, "/api/exploration/main-session/new?task=" + id, ``},
		{http.MethodDelete, "/api/tasks/" + id, `{}`},
	} {
		if got, body := g.do(t, request.method, request.path, request.body); got != http.StatusConflict {
			t.Fatalf("immutable %s %s: status=%d body=%v, want %d", request.method, request.path, got, body, http.StatusConflict)
		}
	}
	upload := httptest.NewRequest(http.MethodPost, "/api/chat/upload?scope=task&id="+id, nil)
	upload.Header.Set("Authorization", "Bearer "+g.token)
	uploadRec := httptest.NewRecorder()
	g.s.Handler().ServeHTTP(uploadRec, upload)
	if uploadRec.Code != http.StatusConflict {
		t.Fatalf("CAL-00 upload status=%d body=%s, want %d", uploadRec.Code, uploadRec.Body.String(), http.StatusConflict)
	}

	// These are global APIs, not /api/tasks/{id} routes. They must resolve the
	// persistent marker themselves rather than attaching a new asset or deleting
	// CAL-00's one fixture by its discoverable asset ID.
	if got, body := g.do(t, http.MethodPost, "/api/assets", `{"task_id":`+id+`,"assets":[{"type":"root_domain","domain":"calibration-must-not-link.invalid"}]}`); got != http.StatusConflict {
		t.Fatalf("asset link to CAL-00: status=%d body=%v, want %d", got, body, http.StatusConflict)
	}
	if got, body := g.do(t, http.MethodPost, "/api/assets", `{"assets":[{"type":"app","app_name":"must not mutate CAL-00","bundle_id":`+strconv.Quote(fixtureBundleID)+`}]}`); got != http.StatusConflict {
		t.Fatalf("reserved CAL-00 bundle: status=%d body=%v, want %d", got, body, http.StatusConflict)
	}
	if got, body := g.do(t, http.MethodDelete, "/api/assets", `{"ids":[`+strconv.FormatInt(fixtureAssetID, 10)+`]}`); got != http.StatusConflict {
		t.Fatalf("delete CAL-00 fixture: status=%d body=%v, want %d", got, body, http.StatusConflict)
	}
	normal := g.newTask(t, db.TaskCreateOptions{})
	if got, body := g.do(t, http.MethodPost, "/api/tasks/"+normal.ID+"/assets", `{"asset_ids":[`+strconv.FormatInt(fixtureAssetID, 10)+`],"source_summary":"must not reuse the CAL-00 fixture"}`); got != http.StatusConflict {
		t.Fatalf("attach CAL-00 fixture to normal task: status=%d body=%v, want %d", got, body, http.StatusConflict)
	}

	// A normal child task must not turn CAL-00 into inherited task context.
	if got, body := g.do(t, http.MethodPost, "/api/tasks", `{"description":"must not inherit CAL-00","goal":"verify source rejection","source_task_ids":["`+id+`"]}`); got != http.StatusConflict {
		t.Fatalf("inherit CAL-00: status=%d body=%v, want %d", got, body, http.StatusConflict)
	}

	// Approval endpoints are global pending-row routes, so a stale CAL-00 row
	// must be rejected by its durable task ownership rather than route shape.
	var staleApprovalID int64
	if err := g.m.pg.QueryRow(`INSERT INTO intercept_pending(task_id,agent_name,tool_name,tool_input,reason,decision_source,audit)
VALUES($1,'stale','Bash','{}','must remain inert','rule','{}') RETURNING id`, id).Scan(&staleApprovalID); err != nil {
		t.Fatal(err)
	}
	approvalPath := "/api/intercept/pending/" + strconv.FormatInt(staleApprovalID, 10)
	if got, body := g.do(t, http.MethodPost, approvalPath+"/decide", `{"decision":"denied"}`); got != http.StatusConflict {
		t.Fatalf("decide stale CAL-00 approval: status=%d body=%v, want %d", got, body, http.StatusConflict)
	}
	if got, body := g.do(t, http.MethodPost, approvalPath+"/always-allow", ``); got != http.StatusConflict {
		t.Fatalf("always-allow stale CAL-00 approval: status=%d body=%v, want %d", got, body, http.StatusConflict)
	}
	if _, err := g.m.pg.Exec(`DELETE FROM intercept_pending WHERE id=$1`, staleApprovalID); err != nil {
		t.Fatal(err)
	}

	// Numeric aliases are neither ordinary legacy labels nor a valid CAL-00
	// reference. Both global mutation endpoints must leave such a raw row
	// pending, and always-allow must not create a rule from its stored command.
	var aliasApprovalID int64
	aliasTaskID := "000" + id
	if err := g.m.pg.QueryRow(`INSERT INTO intercept_pending(task_id,agent_name,tool_name,tool_input,reason,decision_source,audit)
VALUES($1,'legacy','Bash','{"command":"git status"}','must remain inert','rule','{}') RETURNING id`, aliasTaskID).Scan(&aliasApprovalID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = g.m.pg.Exec(`DELETE FROM intercept_pending WHERE id=$1`, aliasApprovalID) })
	aliasPath := "/api/intercept/pending/" + strconv.FormatInt(aliasApprovalID, 10)
	rulesBefore, err := g.m.pg.ListInterceptRules()
	if err != nil {
		t.Fatal(err)
	}
	for _, decision := range []string{"allowed", "denied"} {
		if got, body := g.do(t, http.MethodPost, aliasPath+"/decide", `{"decision":"`+decision+`"}`); got != http.StatusConflict {
			t.Fatalf("decide numeric CAL-00 alias %s: status=%d body=%v, want %d", decision, got, body, http.StatusConflict)
		}
	}
	if got, body := g.do(t, http.MethodPost, aliasPath+"/always-allow", ``); got != http.StatusConflict {
		t.Fatalf("always-allow numeric CAL-00 alias: status=%d body=%v, want %d", got, body, http.StatusConflict)
	}
	if row, err := g.m.pg.GetInterceptPending(aliasApprovalID); err != nil || row == nil || row.Status != "pending" {
		t.Fatalf("numeric CAL-00 alias row after global routes=%+v err=%v, want pending", row, err)
	}
	rulesAfter, err := g.m.pg.ListInterceptRules()
	if err != nil {
		t.Fatal(err)
	}
	if len(rulesAfter) != len(rulesBefore) {
		t.Fatalf("numeric CAL-00 alias created an always-allow rule: before=%d after=%d", len(rulesBefore), len(rulesAfter))
	}
	if _, err := g.m.pg.Exec(`DELETE FROM intercept_pending WHERE id=$1`, aliasApprovalID); err != nil {
		t.Fatal(err)
	}

	// Cross-task orchestration bypasses the HTTP route prefix, so test its
	// mutable delegate explicitly. A rejected hint must not append graph data.
	var nodesBefore int
	if err := g.m.pg.QueryRow(`SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1`, live.ExpID).Scan(&nodesBefore); err != nil {
		t.Fatal(err)
	}
	hintInput := json.RawMessage(`{"task_id":` + strconv.Quote(id) + `,"text":"must not append a hint"}`)
	result, err := g.s.toolAddHint().Call(context.Background(), hintInput, nil)
	if err != nil || !result.IsError {
		t.Fatalf("cross-task CAL-00 hint result=%+v err=%v, want tool error", result, err)
	}
	var nodesAfter int
	if err := g.m.pg.QueryRow(`SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1`, live.ExpID).Scan(&nodesAfter); err != nil {
		t.Fatal(err)
	}
	if nodesAfter != nodesBefore {
		t.Fatalf("cross-task hint mutated CAL-00 graph: before=%d after=%d", nodesBefore, nodesAfter)
	}

	// Task-create scheduler triggers use this DB event feed. CAL-00 must never
	// appear there, otherwise an enabled trigger could start an unrelated LLM
	// conversation merely because the UI fixture was created.
	events, err := g.m.pg.NewTasksSince(numericID - 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.TaskID == numericID {
			t.Fatalf("CAL-00 reached task-create trigger feed: %+v", event)
		}
	}

	// A view-selection is allowed, but Engine.Run has a hard system-task guard.
	if got, body := g.do(t, http.MethodPost, "/api/active", `{"id":"`+id+`"}`); got != http.StatusOK {
		t.Fatalf("activate CAL-00: status=%d body=%v", got, body)
	}
	g.s.engine.Run(g.s.ctx, live)
	if g.s.engine.Started(id) {
		t.Fatalf("CAL-00 task %s reached Engine.Run", id)
	}
	if _, err := g.s.admitTask(live, "resume"); !errors.Is(err, ErrSystemTaskImmutable) {
		t.Fatalf("admit CAL-00 err=%v, want ErrSystemTaskImmutable", err)
	}

	var activity, sessions, goals int
	if err := g.m.pg.QueryRow(`SELECT count(*) FROM activity WHERE exploration_id=$1`, live.ExpID).Scan(&activity); err != nil {
		t.Fatal(err)
	}
	if err := g.m.pg.QueryRow(`SELECT count(*) FROM main_sessions WHERE exploration_id=$1`, live.ExpID).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := g.m.pg.QueryRow(`SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1 AND kind='goal'`, live.ExpID).Scan(&goals); err != nil {
		t.Fatal(err)
	}
	if activity != 0 || sessions != 0 || goals != 0 {
		t.Fatalf("rejected requests altered CAL-00: activity=%d sessions=%d goals=%d", activity, sessions, goals)
	}

	code, replay := g.do(t, http.MethodPost, "/api/calibration/synthetic", "")
	if code != http.StatusOK || replay["already_present"] != true {
		t.Fatalf("replayed calibration response: status=%d body=%v", code, replay)
	}
	replayTask, ok := replay["task"].(map[string]any)
	if !ok || replayTask["id"] != id {
		t.Fatalf("replayed calibration task=%v, want id=%q", replay["task"], id)
	}
}

func TestStartTriggeredRunRejectsSyntheticCalibration(t *testing.T) {
	g := newGateServer(t)

	cal, _, err := g.m.EnsureSyntheticCalibrationTask()
	if err != nil {
		t.Fatal(err)
	}
	numericID, err := strconv.ParseInt(cal.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := g.m.pg.DeleteTaskCascade(numericID, true, true, true); err != nil {
			t.Errorf("cleanup CAL-00: %v", err)
		}
		g.m.forgetTask(cal.ID, numericID)
	})

	// This local stub has no credentials or network path. The fixture context is
	// cancelled, so a normal admitted trigger would remain observable in the
	// queue without performing inference.
	g.s.cfgMu.Lock()
	g.s.chatAgent = agent.NewChatAgent(newScopeProbeProvider(), "test-model", g.m.dir, nil, 10000)
	g.s.cfgMu.Unlock()

	// Exercise the durable database fallback, not just a favorable live-cache
	// lookup: StartTriggeredRun receives only a numeric task ID.
	g.m.forgetTask(cal.ID, numericID)

	const triggerKey = "cal-00-direct-trigger"
	beforeConversations, err := g.m.pg.ListConversations()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		g.s.queueMu.Lock()
		delete(g.s.triggerQ, triggerKey)
		delete(g.s.triggerActive, triggerKey)
		delete(g.s.triggerCfg, triggerKey)
		g.s.queueMu.Unlock()
	})

	g.s.StartTriggeredRun(
		triggerKey,
		"CAL-00 must not run",
		"CAL-00 must not create a triggered conversation",
		numericID,
		true,
		"synthetic calibration",
		"fixed UI provenance record",
	)

	g.s.queueMu.Lock()
	queued := len(g.s.triggerQ[triggerKey])
	active := g.s.triggerActive[triggerKey]
	_, cfgRecorded := g.s.triggerCfg[triggerKey]
	g.s.queueMu.Unlock()
	if queued != 0 || active != 0 || cfgRecorded {
		t.Fatalf("CAL-00 entered trigger state: queued=%d active=%d cfg=%t", queued, active, cfgRecorded)
	}

	afterConversations, err := g.m.pg.ListConversations()
	if err != nil {
		t.Fatal(err)
	}
	if len(afterConversations) != len(beforeConversations) {
		t.Fatalf("CAL-00 direct trigger created a conversation: before=%d after=%d", len(beforeConversations), len(afterConversations))
	}
}
