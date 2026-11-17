package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

type researchWorkflowTemplatesResponse struct {
	Templates    []db.TaskTemplate `json:"templates"`
	CreatedCount int               `json:"created_count"`
}

func TestResearchWorkflowTemplatesEndpointIsAuthenticatedInertAndIdempotent(t *testing.T) {
	g := newGateServer(t)

	unauthenticated := httptest.NewRecorder()
	g.s.Handler().ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodPost, "/api/research-workflow-templates", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d, want %d", unauthenticated.Code, http.StatusUnauthorized)
	}

	request := func(body string) (int, researchWorkflowTemplatesResponse) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/research-workflow-templates", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+g.token)
		rec := httptest.NewRecorder()
		g.s.Handler().ServeHTTP(rec, req)
		var out researchWorkflowTemplatesResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode response status=%d: %v; body=%s", rec.Code, err, rec.Body.String())
		}
		return rec.Code, out
	}

	if code, _ := request(`{}`); code != http.StatusBadRequest {
		t.Fatalf("nonempty request status=%d, want %d", code, http.StatusBadRequest)
	}

	counts := func() (tasks, explorations, activity int) {
		t.Helper()
		if err := g.m.pg.QueryRow(`SELECT count(*) FROM tasks`).Scan(&tasks); err != nil {
			t.Fatal(err)
		}
		if err := g.m.pg.QueryRow(`SELECT count(*) FROM explorations`).Scan(&explorations); err != nil {
			t.Fatal(err)
		}
		if err := g.m.pg.QueryRow(`SELECT count(*) FROM activity`).Scan(&activity); err != nil {
			t.Fatal(err)
		}
		return tasks, explorations, activity
	}
	beforeTasks, beforeExplorations, beforeActivity := counts()

	code, first := request("")
	if code != http.StatusCreated {
		t.Fatalf("first request status=%d, want %d", code, http.StatusCreated)
	}
	if first.CreatedCount != len(researchWorkflowTemplateInputs) || len(first.Templates) != len(researchWorkflowTemplateInputs) {
		t.Fatalf("first response created=%d templates=%d, want %d", first.CreatedCount, len(first.Templates), len(researchWorkflowTemplateInputs))
	}
	seenIDs := make(map[int64]struct{}, len(first.Templates))
	for index, template := range first.Templates {
		if template.ID <= 0 || template.Name != researchWorkflowTemplateInputs[index].Name || template.Description != researchWorkflowTemplateInputs[index].Description || template.Goal != researchWorkflowTemplateInputs[index].Goal {
			t.Fatalf("template[%d]=%+v, want fixed safe workflow template", index, template)
		}
		if _, duplicate := seenIDs[template.ID]; duplicate {
			t.Fatalf("template[%d] repeats row id %d", index, template.ID)
		}
		seenIDs[template.ID] = struct{}{}
		if template.CategoryID != nil || len(template.InterceptRules) != 0 {
			t.Fatalf("template[%d] unexpectedly carries category or rules: %+v", index, template)
		}
	}
	t.Cleanup(func() {
		for _, template := range first.Templates {
			if _, err := g.m.pg.DeleteTaskTemplate(template.ID); err != nil {
				t.Errorf("cleanup research workflow template %d: %v", template.ID, err)
			}
		}
	})

	afterTasks, afterExplorations, afterActivity := counts()
	if afterTasks != beforeTasks || afterExplorations != beforeExplorations || afterActivity != beforeActivity {
		t.Fatalf("template bootstrap created work rows: tasks %d->%d explorations %d->%d activity %d->%d", beforeTasks, afterTasks, beforeExplorations, afterExplorations, beforeActivity, afterActivity)
	}

	code, replay := request("")
	if code != http.StatusOK || replay.CreatedCount != 0 || len(replay.Templates) != len(first.Templates) {
		t.Fatalf("replay status=%d created=%d templates=%d", code, replay.CreatedCount, len(replay.Templates))
	}
	for index, template := range replay.Templates {
		if template.ID != first.Templates[index].ID {
			t.Fatalf("replay template[%d] id=%d, want %d", index, template.ID, first.Templates[index].ID)
		}
	}
	replayTasks, replayExplorations, replayActivity := counts()
	if replayTasks != beforeTasks || replayExplorations != beforeExplorations || replayActivity != beforeActivity {
		t.Fatalf("replay created work rows: tasks %d->%d explorations %d->%d activity %d->%d", beforeTasks, replayTasks, beforeExplorations, replayExplorations, beforeActivity, replayActivity)
	}
}

// A user-owned template with a reserved safe-workflow name must be preserved.
// Use the last preset so the transaction has already attempted to insert the
// first three when it observes the conflict: this proves the error path is
// atomic, not merely that the colliding row is left alone.
func TestResearchWorkflowTemplatesConflictIsAtomic(t *testing.T) {
	g := newGateServer(t)

	category, err := g.m.pg.CreateTaskCategory("research workflow collision " + time.Now().UTC().Format("20060102150405.000000000"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := g.m.pg.DeleteTaskCategory(category.ID)
		if err != nil {
			t.Errorf("cleanup collision category: %v", err)
		}
	})

	conflictingPreset := researchWorkflowTemplateInputs[len(researchWorkflowTemplateInputs)-1]
	ownedRules := []db.TaskInterceptRuleInput{{
		Enabled: true,
		Action:  "block",
		Kind:    "exact_domain",
		Pattern: "operator-owned.example.test",
		Note:    "operator-owned collision fixture",
	}}
	owned, err := g.m.pg.CreateTaskTemplate(db.TaskTemplateInput{
		Name:           conflictingPreset.Name,
		Description:    "operator-owned description must not be replaced",
		Goal:           "operator-owned goal must not be replaced",
		CategoryID:     &category.ID,
		InterceptRules: ownedRules,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := g.m.pg.DeleteTaskTemplate(owned.ID); err != nil {
			t.Errorf("cleanup colliding workflow template: %v", err)
		}
	})

	counts := func() (tasks, explorations, activity int) {
		t.Helper()
		if err := g.m.pg.QueryRow(`SELECT count(*) FROM tasks`).Scan(&tasks); err != nil {
			t.Fatal(err)
		}
		if err := g.m.pg.QueryRow(`SELECT count(*) FROM explorations`).Scan(&explorations); err != nil {
			t.Fatal(err)
		}
		if err := g.m.pg.QueryRow(`SELECT count(*) FROM activity`).Scan(&activity); err != nil {
			t.Fatal(err)
		}
		return tasks, explorations, activity
	}
	beforeTasks, beforeExplorations, beforeActivity := counts()

	code, _ := g.do(t, http.MethodPost, "/api/research-workflow-templates", "")
	if code != http.StatusConflict {
		t.Fatalf("conflicting request status=%d, want %d", code, http.StatusConflict)
	}

	preserved, err := g.m.pg.GetTaskTemplate(owned.ID)
	if err != nil {
		t.Fatal(err)
	}
	if preserved == nil || preserved.Name != owned.Name || preserved.Description != owned.Description || preserved.Goal != owned.Goal || preserved.CategoryID == nil || *preserved.CategoryID != category.ID || !reflect.DeepEqual(preserved.InterceptRules, ownedRules) {
		t.Fatalf("colliding template was changed: got=%+v", preserved)
	}

	templates, err := g.m.pg.ListTaskTemplates()
	if err != nil {
		t.Fatal(err)
	}
	bySafeName := make(map[string]*db.TaskTemplate, len(researchWorkflowTemplateInputs))
	for _, template := range templates {
		for _, preset := range researchWorkflowTemplateInputs {
			if template.Name == preset.Name {
				bySafeName[template.Name] = template
			}
		}
	}
	if len(bySafeName) != 1 || bySafeName[conflictingPreset.Name] == nil || bySafeName[conflictingPreset.Name].ID != owned.ID {
		t.Fatalf("collision left a partial bootstrap: safe templates=%+v", bySafeName)
	}

	afterTasks, afterExplorations, afterActivity := counts()
	if afterTasks != beforeTasks || afterExplorations != beforeExplorations || afterActivity != beforeActivity {
		t.Fatalf("collision created work rows: tasks %d->%d explorations %d->%d activity %d->%d", beforeTasks, afterTasks, beforeExplorations, afterExplorations, beforeActivity, afterActivity)
	}
}
