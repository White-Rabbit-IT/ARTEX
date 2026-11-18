package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

func operatorReadinessRequest(t *testing.T, g *gateServer, authenticated bool) (int, operatorReadinessResponse, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/operator-readiness", nil)
	if authenticated {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	rec := httptest.NewRecorder()
	g.s.Handler().ServeHTTP(rec, req)
	var out operatorReadinessResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode readiness response: %v; body=%s", err, rec.Body.String())
		}
	}
	return rec.Code, out, rec.Body.String()
}

func operatorReadinessCounts(t *testing.T, g *gateServer) map[string]int {
	t.Helper()
	tables := []string{"tasks", "explorations", "activity", "task_templates", "llm_profiles"}
	counts := make(map[string]int, len(tables))
	for _, table := range tables {
		var count int
		if err := g.m.pg.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		counts[table] = count
	}
	return counts
}

func TestOperatorReadinessBudgetCap(t *testing.T) {
	if got := operatorReadinessBudgetCap(agent.Unlimited); got != nil {
		t.Fatalf("unlimited cap=%v, want nil", *got)
	}
	const bounded int64 = 17
	got := operatorReadinessBudgetCap(bounded)
	if got == nil || *got != bounded {
		t.Fatalf("bounded cap=%v, want %d", got, bounded)
	}
}

func TestOperatorReadinessIsAuthenticatedNonSecretAndReadOnly(t *testing.T) {
	t.Setenv(RequireTargetGateEnv, "1")
	t.Setenv("ARTEX_UNTRUSTED_BOUNDARY", "") // enabled by default
	t.Setenv("ARTEX_TASK_BUDGET_TOKENS", "80000")
	t.Setenv("ARTEX_TASK_BUDGET_TURNS", "14")
	t.Setenv("ARTEX_INTENT_BUDGET_TOKENS", "60000")
	t.Setenv("ARTEX_INTENT_BUDGET_TURNS", "10")
	t.Setenv(HermesSubscriptionAgentExecutionEnv, "0")
	g := newGateServer(t)

	if code, _, _ := operatorReadinessRequest(t, g, false); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated readiness status=%d, want %d", code, http.StatusUnauthorized)
	}

	// The row deliberately contains distinctive values. The response may say
	// only that a Hermes-format row exists; it must never echo any of this data.
	profile := &db.LLMProfile{
		Name:    "operator-readiness-profile-name",
		Format:  "hermes-subscription",
		BaseURL: "http://operator-readiness-endpoint.invalid/v1",
		Proxy:   "http://user:operator-readiness-proxy-secret@proxy.invalid:8443",
		Model:   "operator-readiness-model",
		APIKey:  "operator-readiness-api-key-secret",
	}
	profileID, err := g.m.pg.SaveProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := g.m.pg.DeleteProfile(profileID); err != nil {
			t.Errorf("cleanup profile: %v", err)
		}
	})

	before := operatorReadinessCounts(t, g)
	code, got, raw := operatorReadinessRequest(t, g, true)
	if code != http.StatusOK {
		t.Fatalf("readiness status=%d body=%s", code, raw)
	}
	after := operatorReadinessCounts(t, g)
	for table, want := range before {
		if gotCount := after[table]; gotCount != want {
			t.Fatalf("readiness GET mutated %s: %d -> %d", table, want, gotCount)
		}
	}

	if !got.Policy.TargetGateRequired || !got.Policy.UntrustedOutputBoundary {
		t.Fatalf("policy=%+v, want target gate and untrusted boundary enabled", got.Policy)
	}
	if got.Policy.TaskBudget.Tokens == nil || *got.Policy.TaskBudget.Tokens != 80000 || got.Policy.TaskBudget.Turns == nil || *got.Policy.TaskBudget.Turns != 14 {
		t.Fatalf("task budget=%+v, want 80000/14", got.Policy.TaskBudget)
	}
	if got.Policy.IntentBudget.Tokens == nil || *got.Policy.IntentBudget.Tokens != 60000 || got.Policy.IntentBudget.Turns == nil || *got.Policy.IntentBudget.Turns != 10 {
		t.Fatalf("intent budget=%+v, want 60000/10", got.Policy.IntentBudget)
	}
	if got.Bootstrap.SyntheticCalibration != db.OperatorReadinessCalibrationMissing || got.Bootstrap.WorkflowTemplates.Required != len(researchWorkflowTemplateInputs) || got.Bootstrap.WorkflowTemplates.Matching != 0 || got.Bootstrap.WorkflowTemplates.Complete || got.Bootstrap.WorkflowTemplates.Conflict {
		t.Fatalf("unexpected empty bootstrap status=%+v", got.Bootstrap)
	}
	if !got.Hermes.SubscriptionProfilePresent || got.Hermes.AgentExecutionEnabled {
		t.Fatalf("hermes=%+v, want present profile and disabled execution", got.Hermes)
	}
	for _, forbidden := range []string{
		profile.Name,
		profile.BaseURL,
		profile.Proxy,
		profile.Model,
		profile.APIKey,
		"operator-readiness-proxy-secret",
	} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("readiness response leaked %q: %s", forbidden, raw)
		}
	}
}

func TestOperatorReadinessReportsBootstrapCompletionWithoutRunningWork(t *testing.T) {
	g := newGateServer(t)

	calibration, err := g.m.pg.EnsureSyntheticCalibrationTask()
	if err != nil {
		t.Fatal(err)
	}
	calibrationID := calibration.Task.ID
	t.Cleanup(func() {
		if _, err := g.m.pg.DeleteTaskCascade(calibrationID, true, true, true); err != nil {
			t.Errorf("cleanup CAL-00: %v", err)
		}
		g.m.forgetTask(strconv.FormatInt(calibrationID, 10), calibrationID)
	})
	templates, err := g.m.pg.EnsureTaskTemplates(researchWorkflowTemplateInputs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, template := range templates.Templates {
			if err := g.m.pg.DeleteTaskTemplate(template.ID); err != nil {
				t.Errorf("cleanup workflow template %d: %v", template.ID, err)
			}
		}
	})

	code, got, raw := operatorReadinessRequest(t, g, true)
	if code != http.StatusOK {
		t.Fatalf("readiness status=%d body=%s", code, raw)
	}
	if got.Bootstrap.SyntheticCalibration != db.OperatorReadinessCalibrationPresentTerminal {
		t.Fatalf("CAL-00 readiness=%q, want %q", got.Bootstrap.SyntheticCalibration, db.OperatorReadinessCalibrationPresentTerminal)
	}
	status := got.Bootstrap.WorkflowTemplates
	if status.Required != len(researchWorkflowTemplateInputs) || status.Matching != len(researchWorkflowTemplateInputs) || !status.Complete || status.Conflict {
		t.Fatalf("workflow-template readiness=%+v", status)
	}
	if strings.Contains(raw, researchWorkflowTemplateInputs[0].Goal) || strings.Contains(raw, db.SyntheticCalibrationTaskName) {
		t.Fatalf("bootstrap readiness leaked template or task content: %s", raw)
	}
}

func TestOperatorReadinessIgnoresSoftDeletedCalibration(t *testing.T) {
	g := newGateServer(t)
	calibration, err := g.m.pg.EnsureSyntheticCalibrationTask()
	if err != nil {
		t.Fatal(err)
	}
	calibrationID := calibration.Task.ID
	t.Cleanup(func() {
		if _, err := g.m.pg.DeleteTaskCascade(calibrationID, true, true, true); err != nil {
			t.Errorf("cleanup CAL-00: %v", err)
		}
		g.m.forgetTask(strconv.FormatInt(calibrationID, 10), calibrationID)
	})
	if _, err := g.m.pg.Exec(`UPDATE tasks SET deleted_at=now() WHERE id=$1`, calibrationID); err != nil {
		t.Fatal(err)
	}

	code, got, raw := operatorReadinessRequest(t, g, true)
	if code != http.StatusOK {
		t.Fatalf("readiness status=%d body=%s", code, raw)
	}
	if got.Bootstrap.SyntheticCalibration != db.OperatorReadinessCalibrationMissing {
		t.Fatalf("soft-deleted CAL-00 readiness=%q, want %q", got.Bootstrap.SyntheticCalibration, db.OperatorReadinessCalibrationMissing)
	}
}

func TestOperatorReadinessReportsTemplateConflictWithoutMutatingIt(t *testing.T) {
	g := newGateServer(t)
	input := researchWorkflowTemplateInputs[0]
	input.Description = "operator-owned different template content"
	input.Goal = "operator-owned different template goal"
	conflicting, err := g.m.pg.CreateTaskTemplate(input)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := g.m.pg.DeleteTaskTemplate(conflicting.ID); err != nil {
			t.Errorf("cleanup conflicting template: %v", err)
		}
	})

	before := operatorReadinessCounts(t, g)
	code, got, raw := operatorReadinessRequest(t, g, true)
	if code != http.StatusOK {
		t.Fatalf("readiness status=%d body=%s", code, raw)
	}
	after := operatorReadinessCounts(t, g)
	if before["task_templates"] != after["task_templates"] {
		t.Fatalf("readiness GET changed template count: %d -> %d", before["task_templates"], after["task_templates"])
	}
	status := got.Bootstrap.WorkflowTemplates
	if status.Matching != 0 || status.Complete || !status.Conflict {
		t.Fatalf("conflicting workflow-template readiness=%+v", status)
	}
	if strings.Contains(raw, input.Description) || strings.Contains(raw, input.Goal) {
		t.Fatalf("readiness response leaked conflicting user template content: %s", raw)
	}
}

func TestOperatorReadinessReportsExactHermesAndBoundaryOptIns(t *testing.T) {
	t.Setenv("ARTEX_UNTRUSTED_BOUNDARY", "off")
	t.Setenv(HermesSubscriptionAgentExecutionEnv, "true")
	g := newGateServer(t)
	code, got, raw := operatorReadinessRequest(t, g, true)
	if code != http.StatusOK {
		t.Fatalf("readiness status=%d body=%s", code, raw)
	}
	if got.Policy.UntrustedOutputBoundary {
		t.Fatal("boundary reported enabled for explicit off")
	}
	if got.Hermes.AgentExecutionEnabled {
		t.Fatal("Hermes execution reported enabled for non-literal opt-in")
	}

	t.Setenv(HermesSubscriptionAgentExecutionEnv, "1")
	code, got, raw = operatorReadinessRequest(t, g, true)
	if code != http.StatusOK || !got.Hermes.AgentExecutionEnabled {
		t.Fatalf("literal Hermes opt-in status=%d hermes=%+v body=%s", code, got.Hermes, raw)
	}
}
