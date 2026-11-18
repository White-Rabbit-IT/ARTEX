package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// syntheticAgentMutationSurfaceServer deliberately has no Engine, Store, side
// service, or database.  Every handler below must reject the product-owned
// policy task before it can parse caller input, touch its graph, or call an
// execution service.  Keeping this as a direct-handler test also protects the
// invariant if routing middleware is rearranged later.
func syntheticAgentMutationSurfaceServer() *Server {
	task := &Task{
		ID:              "7",
		executionPolicy: db.SyntheticAgentCalibrationExecutionPolicy,
	}
	return &Server{m: &Manager{tasks: map[string]*Task{task.ID: task}}}
}

func TestSyntheticAgentCalibrationRejectsMutableTaskSurfacesBeforeInputOrEngine(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Server, http.ResponseWriter, *http.Request)
	}{
		{"add constraint", (*Server).addConstraint},
		{"edit constraint", (*Server).editConstraint},
		{"delete constraint", (*Server).deleteConstraint},
		{"add goal", (*Server).addGoal},
		{"edit goal", (*Server).editGoal},
		{"delete goal", (*Server).deleteGoal},
		{"control intent", (*Server).controlIntent},
		{"rerun intent", (*Server).rerunIntent},
		{"rerun blocked", (*Server).rerunBlocked},
		// The malformed body has a distinct 400 response in sendWorkerMessage.
		// A 409 here proves the task-policy check wins before body parsing.
		{"send worker message", (*Server).sendWorkerMessage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := syntheticAgentMutationSurfaceServer()
			req := httptest.NewRequest(http.MethodPost, "/api/tasks/7", strings.NewReader("{"))
			req.SetPathValue("id", "7")
			// An invalid intent id makes rerun/control visibly fail with 400 if
			// a future edit moves their policy check below route parsing.
			req.SetPathValue("iid", "not-an-intent")
			req.SetPathValue("cid", "not-a-constraint")
			req.SetPathValue("gid", "not-a-goal")
			rec := httptest.NewRecorder()

			tc.call(s, rec, req)

			if rec.Code != http.StatusConflict {
				t.Fatalf("status=%d body=%q, want immutable policy conflict before input/engine", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), syntheticAgentCalibrationImmutableMessage) {
				t.Fatalf("response=%q, want synthetic-agent immutable message", rec.Body.String())
			}
		})
	}
}

func TestSyntheticAgentCalibrationRejectsTaskSideQuestionBeforeServiceAccess(t *testing.T) {
	s := syntheticAgentMutationSurfaceServer()
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/7/chat/side-questions", strings.NewReader("{"))
	req.SetPathValue("id", "7")
	rec := httptest.NewRecorder()

	_, ok := s.sideParent(rec, req, "main")
	if ok {
		t.Fatal("synthetic agent task unexpectedly obtained a side-question parent")
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%q, want immutable policy conflict before side service access", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), syntheticAgentCalibrationImmutableMessage) {
		t.Fatalf("response=%q, want synthetic-agent immutable message", rec.Body.String())
	}
}

func TestSyntheticAgentCalibrationRejectsGlobalFindingOwnerAndTaskPathGuard(t *testing.T) {
	s := syntheticAgentMutationSurfaceServer()
	taskID := int64(7)

	ownerRec := httptest.NewRecorder()
	if s.requireMutableFindingOwner(ownerRec, &db.DBFinding{TaskID: &taskID}) {
		t.Fatal("synthetic-owned global finding was unexpectedly mutable")
	}
	if ownerRec.Code != http.StatusConflict || !strings.Contains(ownerRec.Body.String(), syntheticAgentCalibrationImmutableMessage) {
		t.Fatalf("owner guard status=%d body=%q, want immutable policy conflict", ownerRec.Code, ownerRec.Body.String())
	}

	called := false
	guarded := s.systemTaskMutationGuard(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/7/scope", nil)
	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, req)
	if called {
		t.Fatal("task-scoped mutation reached handler for synthetic agent policy")
	}
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), syntheticAgentCalibrationImmutableMessage) {
		t.Fatalf("path guard status=%d body=%q, want immutable policy conflict", rec.Code, rec.Body.String())
	}
}

func TestSyntheticAgentCalibrationIsExcludedFromGenericConcurrencyReconciliation(t *testing.T) {
	task := &Task{
		ID:              "7",
		Status:          "running",
		Queued:          true,
		executionPolicy: db.SyntheticAgentCalibrationExecutionPolicy,
	}
	// A nil Engine makes accidental generic scheduler inspection observable.
	// The policy check must return before ReadyFor, IsPaused, or any queue
	// mutation path is reached.
	s := &Server{m: &Manager{tasks: map[string]*Task{task.ID: task}}}
	if s.occupiesConcurrencySlot(task) {
		t.Fatal("synthetic agent calibration occupied an ordinary concurrency slot")
	}
	if s.hasReadyQueuedTask("") {
		t.Fatal("synthetic agent calibration entered the ordinary FIFO backlog")
	}
}

func TestSyntheticAgentCalibrationRejectsGlobalFindingMutationRoutesBeforeInputOrProvider(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer m.Close()

	profileID, err := m.pg.SaveProfile(&db.LLMProfile{
		Name:        fmt.Sprintf("synthetic-agent-finding-guard-%d", time.Now().UnixNano()),
		Format:      "hermes-subscription",
		BaseURL:     "http://127.0.0.1:864/v1",
		Model:       "openai/test-synthetic",
		PoolExclude: true,
		Streaming:   false,
	})
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := m.EnsureSyntheticAgentCalibrationTask(profileID)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := parseTaskID(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = m.pg.Exec(`DELETE FROM findings WHERE task_id=$1`, taskID)
		_ = m.pg.DeleteTask(taskID)
		_ = m.pg.DeleteProfile(profileID)
	})

	nodeID, err := task.Store.AddNode(db.KindFinding, map[string]any{"summary": "stale synthetic finding"}, 0, "confirmed", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	findingID, err := m.pg.AddFinding(taskID, nodeID, "test", "stale synthetic finding", db.SeverityLow, "summary", "", "test", nil)
	if err != nil {
		t.Fatal(err)
	}

	s := &Server{
		m:          m,
		engine:     NewEngine(m),
		ctx:        context.Background(),
		chatBusy:   map[string]bool{},
		chatCancel: map[string]context.CancelCauseFunc{},
	}
	for _, tc := range []struct {
		name   string
		method string
		body   string
		call   func(http.ResponseWriter, *http.Request)
		path   string
	}{
		{"patch finding", http.MethodPatch, "{", s.patchFinding, "/api/exploration/findings/1"},
		{"delete finding", http.MethodDelete, "", s.deleteFinding, "/api/exploration/findings/1"},
		{"start retest", http.MethodPost, "{", s.startFindingRetest, "/api/exploration/findings/1/retests"},
		{"bind traffic", http.MethodPost, "{", s.bindFindingTraffic, "/api/exploration/findings/1/traffic"},
		{"deepen finding", http.MethodPost, "{", s.deepenFinding, "/api/exploration/findings/1/deepen"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.SetPathValue("id", fmt.Sprint(findingID))
			rec := httptest.NewRecorder()

			tc.call(rec, req)

			if rec.Code != http.StatusConflict {
				t.Fatalf("status=%d body=%q, want immutable policy conflict before input/provider", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), syntheticAgentCalibrationImmutableMessage) {
				t.Fatalf("response=%q, want synthetic-agent immutable message", rec.Body.String())
			}
		})
	}

	if finding, err := m.pg.GetFinding(findingID); err != nil || finding == nil {
		t.Fatalf("global mutation guard changed finding=%+v err=%v, want retained finding", finding, err)
	}
	if err := s.agentFindingTrafficAccess(context.Background(), findingID, true); !errors.Is(err, db.ErrSyntheticAgentCalibrationImmutable) {
		t.Fatalf("agent finding-traffic write err=%v, want ErrSyntheticAgentCalibrationImmutable", err)
	}
}
