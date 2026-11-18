package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmrec"
)

func TestHermesSyntheticAgentCalibrationRequiresLiteralOptIn(t *testing.T) {
	for _, value := range []string{"", "true", "yes", "0", " 1", "1 ", "1\n"} {
		t.Setenv(HermesSyntheticAgentCalibrationExecutionEnv, value)
		if hermesSyntheticAgentCalibrationAllowed() {
			t.Fatalf("%s=%q unexpectedly enabled synthetic calibration", HermesSyntheticAgentCalibrationExecutionEnv, value)
		}
	}
	t.Setenv(HermesSyntheticAgentCalibrationExecutionEnv, "1")
	if !hermesSyntheticAgentCalibrationAllowed() {
		t.Fatalf("%s=1 did not enable the fixed synthetic calibration route", HermesSyntheticAgentCalibrationExecutionEnv)
	}
}

func TestSyntheticAgentCalibrationAttemptContextUsesFixedOuterDeadline(t *testing.T) {
	ctx, cancel := syntheticAgentCalibrationAttemptContext(context.Background())
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("synthetic calibration attempt context has no deadline")
	}
	remaining := time.Until(deadline)
	want := time.Duration(db.SyntheticAgentCalibrationTaskTimeoutSeconds) * time.Second
	if remaining > want || remaining < want-time.Second {
		t.Fatalf("synthetic calibration outer deadline remaining=%v, want approximately %v", remaining, want)
	}
}

func TestSyntheticAgentCalibrationRejectsCallerBodyBeforeStateAccess(t *testing.T) {
	// A nil receiver makes the ordering property observable: the handler must
	// reject caller-authored task data before it can inspect an LLM profile,
	// create a DB record, or start an engine. It also prevents this endpoint
	// from becoming an alternate arbitrary-task creation API in a refactor.
	var s *Server
	req := httptest.NewRequest(http.MethodPost, "/api/calibration/synthetic-agent", strings.NewReader(`{"target":"must-not-be-read","prompt":"must-not-be-read"}`))
	rec := httptest.NewRecorder()
	s.createSyntheticAgentCalibration(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("nonempty synthetic-agent calibration body status=%d body=%q, want %d", rec.Code, rec.Body.String(), http.StatusBadRequest)
	}
	if !strings.Contains(rec.Body.String(), "empty request body") {
		t.Fatalf("body rejection=%q, want explicit empty-body policy", rec.Body.String())
	}
}

func TestSyntheticAgentCalibrationRetryRejectsCallerBodyBeforeStateAccess(t *testing.T) {
	var s *Server
	req := httptest.NewRequest(http.MethodPost, "/api/calibration/synthetic-agent/retry", strings.NewReader(`{"retry":"must-not-be-read"}`))
	rec := httptest.NewRecorder()
	s.retrySyntheticAgentCalibration(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("nonempty synthetic-agent retry body status=%d body=%q, want %d", rec.Code, rec.Body.String(), http.StatusBadRequest)
	}
	if !strings.Contains(rec.Body.String(), "empty request body") {
		t.Fatalf("retry body rejection=%q, want explicit empty-body policy", rec.Body.String())
	}
}

func TestSyntheticAgentCalibrationGateFailsClosedBeforeProfileLookup(t *testing.T) {
	// The route must not probe saved profiles or initialise a provider when the
	// separate synthetic-lane opt-in is absent. A deliberately incomplete
	// Server makes any accidental lookup fail loudly in this regression test.
	t.Setenv(HermesSyntheticAgentCalibrationExecutionEnv, "")
	s := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/api/calibration/synthetic-agent", nil)
	rec := httptest.NewRecorder()
	s.createSyntheticAgentCalibration(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("disabled synthetic-agent calibration status=%d body=%q, want %d", rec.Code, rec.Body.String(), http.StatusConflict)
	}
	if !strings.Contains(rec.Body.String(), HermesSyntheticAgentCalibrationExecutionEnv) {
		t.Fatalf("disabled route response=%q, want explicit opt-in name", rec.Body.String())
	}
}

func TestSyntheticAgentCalibrationProfileEligibilityIsNarrow(t *testing.T) {
	base := &db.LLMProfile{
		Name:        "test Hermes calibration profile",
		Format:      "hermes-subscription",
		BaseURL:     "http://127.0.0.1:864/v1",
		Model:       "openai/test",
		PoolExclude: true,
	}
	valid := agent.ConfigFrom(base.Format, base.Model, base.BaseURL, "", base.Proxy)
	if !syntheticAgentCalibrationProfileAllowed(base, valid) {
		t.Fatal("canonical non-default pool-excluded Hermes profile was rejected")
	}

	for _, tc := range []struct {
		name   string
		mutate func(*db.LLMProfile)
	}{
		{
			name: "default profile",
			mutate: func(p *db.LLMProfile) {
				p.IsDefault = true
			},
		},
		{
			name: "fallback-pool member",
			mutate: func(p *db.LLMProfile) {
				p.PoolExclude = false
			},
		},
		{
			name: "raw persisted proxy",
			mutate: func(p *db.LLMProfile) {
				p.Proxy = "http://proxy.invalid:8080"
			},
		},
		{
			name: "wrong provider format",
			mutate: func(p *db.LLMProfile) {
				p.Format = "openai"
			},
		},
		{
			name: "non-local route",
			mutate: func(p *db.LLMProfile) {
				p.BaseURL = "https://remote.example/v1"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := *base
			tc.mutate(&p)
			// ConfigFrom deliberately clears a Hermes proxy. Pass the normalized
			// config here so the test proves the raw persisted row is checked too.
			cfg := agent.ConfigFrom(p.Format, p.Model, p.BaseURL, "", p.Proxy)
			if syntheticAgentCalibrationProfileAllowed(&p, cfg) {
				t.Fatalf("ineligible profile was accepted: %+v normalized_proxy=%q", p, cfg.Proxy)
			}
		})
	}
}

func TestSyntheticAgentProviderContextDoesNotReattachTaskIdentity(t *testing.T) {
	ctx, _ := agent.NewSyntheticCalibrationContext(context.Background())
	if got := llmrec.TaskIDFrom(taskLLMProviderContext(ctx, "private-task-id")); got != "" {
		t.Fatalf("synthetic provider context task id=%q, want empty", got)
	}
	if got := llmrec.TaskIDFrom(taskLLMProviderContext(context.Background(), " ordinary-task-id ")); got != "ordinary-task-id" {
		t.Fatalf("ordinary provider context task id=%q, want ordinary-task-id", got)
	}
}

func TestSyntheticAgentRuntimeUsesIsolatedNoRetryTwoCallProviderLane(t *testing.T) {
	skipWithoutDSN(t)
	t.Setenv(HermesSyntheticAgentCalibrationExecutionEnv, "1")
	t.Setenv(HermesSubscriptionAgentExecutionEnv, "")

	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	profileID, err := m.pg.SaveProfile(&db.LLMProfile{
		Name:        fmt.Sprintf("synthetic-agent-runtime-%d", time.Now().UnixNano()),
		Format:      "hermes-subscription",
		BaseURL:     "http://127.0.0.1:864/v1",
		Model:       "openai/test-synthetic",
		PoolExclude: true,
		Streaming:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.pg.DeleteProfile(profileID) })

	task, _, err := m.EnsureSyntheticAgentCalibrationTask(profileID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		id, _ := parseTaskID(task.ID)
		_ = m.pg.DeleteTask(id)
	})

	cached := &scriptedLLMProvider{}
	s := &Server{
		m:             m,
		provByProfile: map[int64]*provEntry{profileID: {prov: cached, cfg: hermesExecutionGuardConfig()}},
	}
	runtime := &taskLLMRuntime{s: s, taskID: task.ID, agentKey: "worker"}
	ctx, state := agent.NewSyntheticCalibrationContext(context.Background())

	for attempt := 1; attempt <= agent.SyntheticCalibrationMaxModelCalls; attempt++ {
		selection, err := runtime.currentFor(ctx)
		if err != nil {
			t.Fatalf("synthetic currentFor attempt %d: %v", attempt, err)
		}
		if selection.provider == nil {
			t.Fatalf("synthetic currentFor attempt %d returned no provider", attempt)
		}
		if selection.provider == cached {
			t.Fatal("synthetic runtime reused a generic cached provider")
		}
		if selection.profileID != 0 {
			t.Fatalf("synthetic selection profile id=%d, want 0 to disable generic quota/failover mutation", selection.profileID)
		}
		if selection.retry.ConnectAttempts != -1 || selection.retry.EmptyAttempts != -1 || selection.retry.StreamAttempts != -1 {
			t.Fatalf("synthetic retry=%+v, want every retry layer disabled", selection.retry)
		}
		if retries, _ := sameProviderRetryPolicy(selection.retry); retries != 0 {
			t.Fatalf("synthetic same-provider retries=%d, want 0", retries)
		}
	}
	if got := state.ModelCallCount(); got != agent.SyntheticCalibrationMaxModelCalls {
		t.Fatalf("reserved model calls=%d, want %d", got, agent.SyntheticCalibrationMaxModelCalls)
	}
	if _, err := runtime.currentFor(ctx); err == nil || !strings.Contains(err.Error(), "two-call provider limit") {
		t.Fatalf("third synthetic provider resolution err=%v, want fixed call-limit denial", err)
	}
}

func TestPreparedSyntheticAgentCalibrationCancelledParentRepairsOpenRollback(t *testing.T) {
	skipWithoutDSN(t)
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	profileID, err := m.pg.SaveProfile(&db.LLMProfile{
		Name:        fmt.Sprintf("synthetic-agent-cancelled-parent-%d", time.Now().UnixNano()),
		Format:      "hermes-subscription",
		BaseURL:     "http://127.0.0.1:864/v1",
		Model:       "openai/test-synthetic",
		PoolExclude: true,
		Streaming:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.pg.DeleteProfile(profileID) })

	task, _, err := m.EnsureSyntheticAgentCalibrationTask(profileID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		id, _ := parseTaskID(task.ID)
		_ = m.pg.DeleteTask(id)
	})
	run, err := m.PrepareSyntheticAgentCalibrationRun(task.ID, profileID, false)
	if err != nil || run == nil || !run.Started {
		t.Fatalf("prepare=%+v err=%v", run, err)
	}

	// A cancelled server parent makes generic runIntent take its historical
	// running->open rollback before any worker/provider call.  The dedicated
	// engine must turn that residue into one failed, retryable policy attempt.
	e := NewEngine(m)
	worker := &agent.Worker{}
	e.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) {
		return nil, worker
	})
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	e.executePreparedSyntheticAgentCalibration(parent, run.Task, run.IntentID, run.ClaimToken)

	taskID, err := parseTaskID(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := m.pg.GetTask(taskID)
	if err != nil || stored == nil {
		t.Fatalf("read cancelled calibration task=%+v err=%v", stored, err)
	}
	node, err := run.Task.Store.GetNode(run.IntentID)
	if err != nil || node == nil {
		t.Fatalf("read cancelled calibration intent=%+v err=%v", node, err)
	}
	if stored.Status != "failed" || node.State != "blocked" {
		t.Fatalf("cancelled parent left lifecycle task=%q intent=%q, want failed/blocked", stored.Status, node.State)
	}
	var abortReceipts int
	if err := m.pg.QueryRow(`SELECT count(*) FROM activity WHERE exploration_id=$1 AND worker='synthetic-calibration' AND kind='attempt_aborted'`, stored.ExplorationID).Scan(&abortReceipts); err != nil {
		t.Fatal(err)
	}
	if abortReceipts != 1 {
		t.Fatalf("cancelled parent abort receipts=%d, want 1", abortReceipts)
	}
	if got := e.ActiveLLMCalls(task.ID); got != 0 {
		t.Fatalf("cancelled parent reached provider accounting calls=%d, want 0", got)
	}
}
