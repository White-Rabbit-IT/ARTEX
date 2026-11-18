package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestSyntheticAgentCalibrationExecutionPolicy(t *testing.T) {
	if SyntheticAgentCalibrationExecutionPolicy == "" {
		t.Fatal("synthetic agent execution policy must be non-empty")
	}
	if !(&Task{ExecutionPolicy: SyntheticAgentCalibrationExecutionPolicy}).IsSyntheticAgentCalibration() {
		t.Fatal("synthetic agent policy task was not recognized")
	}
	if (&Task{}).IsSyntheticAgentCalibration() || (&Task{ExecutionPolicy: "other"}).IsSyntheticAgentCalibration() {
		t.Fatal("ordinary or unknown policy task was misclassified")
	}
	if SyntheticAgentCalibrationTaskTimeoutSeconds != 90 {
		t.Fatalf("synthetic agent calibration task timeout=%d, want static worker wall clock 90", SyntheticAgentCalibrationTaskTimeoutSeconds)
	}
}

func TestEnsureSyntheticAgentCalibrationTaskFixedIntentAndPolicy(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	nonce := time.Now().UnixNano()
	profileID, err := d.SaveProfile(&LLMProfile{
		Name:        fmt.Sprintf("synthetic-agent-calibration-%d", nonce),
		Format:      "hermes-subscription",
		BaseURL:     "http://127.0.0.1:864/v1",
		Model:       "openai/test-synthetic",
		PoolExclude: true,
		Streaming:   false,
	})
	if err != nil {
		t.Fatal(err)
	}

	created, err := d.EnsureSyntheticAgentCalibrationTask(profileID)
	if err != nil {
		t.Fatal(err)
	}
	if created.AlreadyPresent || created.Task == nil || created.Task.ID <= 0 || created.IntentID <= 0 {
		t.Fatalf("unexpected synthetic agent creation result: %+v", created)
	}
	t.Cleanup(func() {
		if _, err := d.DeleteTaskCascade(created.Task.ID, false, false, true); err != nil {
			t.Errorf("cleanup synthetic agent task: %v", err)
		}
		if err := d.DeleteProfile(profileID); err != nil {
			t.Errorf("cleanup synthetic agent profile: %v", err)
		}
	})

	task := created.Task
	if task.Status != "created" || task.Paused || task.Queued ||
		!task.TargetGate || task.CoverageEnabled || task.LLMRotate ||
		task.TimeoutSeconds != SyntheticAgentCalibrationTaskTimeoutSeconds ||
		task.SystemKind != "" || task.ExecutionPolicy != SyntheticAgentCalibrationExecutionPolicy ||
		!task.IsSyntheticAgentCalibration() || task.LLMProfileID == nil || *task.LLMProfileID != profileID ||
		task.ActiveLLMProfileID == nil || *task.ActiveLLMProfileID != profileID ||
		len(task.LLMProfileIDs) != 1 || task.LLMProfileIDs[0] != profileID ||
		len(task.SourceTaskIDs) != 0 || len(task.CompanyIDs) != 0 {
		t.Fatalf("synthetic agent task is not a fixed bounded receipt: %+v", task)
	}

	var originCount, intentCount, relationCount, scopeCount, credentialCount, ruleCount, pinCount int
	if err := d.QueryRow(`
SELECT
  (SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1 AND kind=$2 AND state=$3 AND origin=$4),
  (SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1 AND kind=$5 AND state='open' AND origin=$4),
  (SELECT count(*) FROM task_relations WHERE task_id=$6 OR source_task_id=$6),
  (SELECT count(*) FROM task_scope WHERE task_id=$6),
  (SELECT count(*) FROM task_credentials WHERE task_id=$6),
  (SELECT count(*) FROM task_intercept_rules WHERE task_id=$6),
  (SELECT count(*) FROM target_pins WHERE task_id=$6)`,
		task.ExplorationID, KindFact, StateOrigin, syntheticAgentCalibrationSource, KindIntent, task.ID,
	).Scan(&originCount, &intentCount, &relationCount, &scopeCount, &credentialCount, &ruleCount, &pinCount); err != nil {
		t.Fatal(err)
	}
	if originCount != 1 || intentCount != 1 || relationCount != 0 || scopeCount != 0 || credentialCount != 0 || ruleCount != 0 || pinCount != 0 {
		t.Fatalf("synthetic agent initial topology origin=%d intent=%d relations=%d scope=%d credentials=%d rules=%d pins=%d", originCount, intentCount, relationCount, scopeCount, credentialCount, ruleCount, pinCount)
	}

	replayed, err := d.EnsureSyntheticAgentCalibrationTask(profileID)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.AlreadyPresent || replayed.Task == nil || replayed.Task.ID != task.ID || replayed.IntentID != created.IntentID {
		t.Fatalf("unexpected idempotent replay: %+v (created %+v)", replayed, created)
	}

	// The generic task factory cannot select the execution policy. It retains
	// the database default even when the special lane already exists.
	ordinary, err := d.CreateTaskWithOptions("ordinary task", "ordinary goal", TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.DeleteTaskCascade(ordinary.ID, false, false, true) })
	if ordinary.ExecutionPolicy != "" || ordinary.IsSyntheticAgentCalibration() {
		t.Fatalf("ordinary generic task gained synthetic policy: %+v", ordinary)
	}

	if _, err := d.CreateTaskWithOptions("inherit synthetic task", "must fail", TaskCreateOptions{SourceTaskIDs: []int64{task.ID}}); !errors.Is(err, ErrSyntheticAgentCalibrationImmutable) {
		t.Fatalf("inherit synthetic agent task err=%v, want ErrSyntheticAgentCalibrationImmutable", err)
	}
	off := false
	if _, err := d.UpdateTask(task.ID, TaskPatch{TargetGate: &off}); !errors.Is(err, ErrSyntheticAgentCalibrationImmutable) {
		t.Fatalf("disable synthetic target gate err=%v, want ErrSyntheticAgentCalibrationImmutable", err)
	}
	if err := d.ReplaceTaskLLMProfiles(task.ID, nil, 0); !errors.Is(err, ErrSyntheticAgentCalibrationImmutable) {
		t.Fatalf("replace synthetic profile chain err=%v, want ErrSyntheticAgentCalibrationImmutable", err)
	}
	if err := d.DeleteProfile(profileID); !errors.Is(err, ErrSyntheticAgentCalibrationImmutable) {
		t.Fatalf("delete synthetic profile err=%v, want ErrSyntheticAgentCalibrationImmutable", err)
	}
}

func TestSyntheticAgentCalibrationBlocksArchiveInterceptAndSchedulerEscapeHatches(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	nonce := time.Now().UnixNano()
	profileID, err := d.SaveProfile(&LLMProfile{
		Name:        fmt.Sprintf("synthetic-agent-guard-%d", nonce),
		Format:      "hermes-subscription",
		BaseURL:     "http://127.0.0.1:864/v1",
		Model:       "openai/test-synthetic",
		PoolExclude: true,
		Streaming:   false,
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := d.EnsureSyntheticAgentCalibrationTask(profileID)
	if err != nil {
		t.Fatal(err)
	}
	if created.AlreadyPresent || created.Task == nil {
		t.Fatalf("unexpected synthetic agent creation result: %+v", created)
	}
	task := created.Task
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM intercept_pending WHERE task_id=$1`, fmt.Sprint(task.ID))
		_, _ = d.Exec(`DELETE FROM task_archives WHERE task_id=$1`, task.ID)
		_, _ = d.DeleteTaskCascade(task.ID, false, false, true)
		_ = d.DeleteProfile(profileID)
	})

	if _, err := d.QueueTaskArchive(task.ID); !errors.Is(err, ErrSyntheticAgentCalibrationImmutable) {
		t.Fatalf("queue synthetic agent archive err=%v, want ErrSyntheticAgentCalibrationImmutable", err)
	}
	if _, err := d.SnapshotTaskArchive(task.ID); !errors.Is(err, ErrSyntheticAgentCalibrationImmutable) {
		t.Fatalf("snapshot synthetic agent archive err=%v, want ErrSyntheticAgentCalibrationImmutable", err)
	}

	// Seed a stale/pre-policy archive row directly. Each queue, worker, and
	// package completion phase must remain inert even when its caller bypasses
	// the normal HTTP admission route.
	var archiveID int64
	if err := d.QueryRow(`INSERT INTO task_archives(task_id,state,phase,task_name,task_description,task_goal,original_status)
VALUES($1,'ready','ready','CAL-AGENT-01','bounded synthetic receipt','fixed receipt','created') RETURNING id`, task.ID).Scan(&archiveID); err != nil {
		t.Fatal(err)
	}
	if immutable, err := d.IsSystemTaskArchive(archiveID); err != nil || !immutable {
		t.Fatalf("stale synthetic archive immutable=%t err=%v, want true/nil", immutable, err)
	}
	if _, err := d.QueueTaskArchiveRestore(archiveID); !errors.Is(err, ErrSyntheticAgentCalibrationImmutable) {
		t.Fatalf("queue synthetic agent restore err=%v, want ErrSyntheticAgentCalibrationImmutable", err)
	}
	if _, err := d.QueueTaskArchiveDelete(archiveID); !errors.Is(err, ErrSyntheticAgentCalibrationImmutable) {
		t.Fatalf("queue synthetic agent delete err=%v, want ErrSyntheticAgentCalibrationImmutable", err)
	}
	snapshot := &TaskArchiveSnapshot{
		FormatVersion: TaskArchiveFormatVersion,
		TaskID:        task.ID,
		ExplorationID: task.ExplorationID,
	}
	if err := d.CompleteTaskArchive(archiveID, snapshot, "/tmp/must-not-write.tar.zst", "irrelevant", 0, 0); !errors.Is(err, ErrSyntheticAgentCalibrationImmutable) {
		t.Fatalf("complete synthetic agent archive err=%v, want ErrSyntheticAgentCalibrationImmutable", err)
	}
	if _, err := d.RestoreTaskArchive(archiveID, snapshot, 0); !errors.Is(err, ErrSyntheticAgentCalibrationImmutable) {
		t.Fatalf("restore synthetic agent archive err=%v, want ErrSyntheticAgentCalibrationImmutable", err)
	}
	if err := d.CompleteTaskArchiveRestore(archiveID); !errors.Is(err, ErrSyntheticAgentCalibrationImmutable) {
		t.Fatalf("consume synthetic agent archive err=%v, want ErrSyntheticAgentCalibrationImmutable", err)
	}
	if err := d.DeleteTaskArchiveStub(archiveID); !errors.Is(err, ErrSyntheticAgentCalibrationImmutable) {
		t.Fatalf("delete synthetic agent archive err=%v, want ErrSyntheticAgentCalibrationImmutable", err)
	}
	if _, err := d.Exec(`UPDATE task_archives SET state=$2,phase='queued' WHERE id=$1`, archiveID, ArchiveQueued); err != nil {
		t.Fatal(err)
	}
	if claimed, err := d.ClaimTaskArchiveJob(context.Background()); err != nil || claimed != nil {
		t.Fatalf("claim stale synthetic archive=%+v err=%v, want nil/nil", claimed, err)
	}
	if stale, err := d.GetTaskArchive(archiveID); err != nil || stale == nil || stale.State != ArchiveFailed || stale.Phase != "blocked" {
		t.Fatalf("stale synthetic archive after claim=%+v err=%v, want archive_failed/blocked", stale, err)
	}
	if _, err := d.Exec(`UPDATE task_archives SET state=$2,phase='starting' WHERE id=$1`, archiveID, Restoring); err != nil {
		t.Fatal(err)
	}
	if err := d.RecoverTaskArchiveJobs(); err != nil {
		t.Fatal(err)
	}
	if stale, err := d.GetTaskArchive(archiveID); err != nil || stale == nil || stale.State != RestoreFailed || stale.Phase != "blocked" {
		t.Fatalf("stale synthetic archive after recovery=%+v err=%v, want restore_failed/blocked", stale, err)
	}

	// Scheduler triggers are generic execution: the policy task must not create
	// a follow-up conversation merely because it has a new task id.
	events, err := d.NewTasksSince(task.ID - 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.TaskID == task.ID {
			t.Fatalf("synthetic agent task reached task-create trigger feed: %+v", event)
		}
	}

	// Target approvals are addressed by global pending-row IDs. Exercise both
	// direct writers and stale rows to prove they cannot add a target pin or
	// settle a generic intercepted call for this policy task.
	if _, _, err := d.FindOrCreateTargetRequest(TargetRequest{
		Scope: TargetScopeRef{TaskID: task.ID}, Host: "cal-agent.invalid", Port: 443,
		AgentName: "worker", ToolName: "WebFetch",
		Audit: &InterceptAudit{Targets: []InterceptTarget{{
			Host: "cal-agent.invalid", Port: 443, ResolvedIPs: []string{"198.51.100.42"},
		}}},
	}); !errors.Is(err, ErrSyntheticAgentCalibrationImmutable) {
		t.Fatalf("create synthetic agent target request err=%v, want ErrSyntheticAgentCalibrationImmutable", err)
	}
	pinTx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeTargetPin(pinTx, TargetScopeRef{TaskID: task.ID}, 0, &InterceptTarget{
		Host: "cal-agent-direct.invalid", Port: 443, ResolvedIPs: []string{"198.51.100.42"},
	}, "test", "192.0.2.1"); !errors.Is(err, ErrSyntheticAgentCalibrationImmutable) {
		_ = pinTx.Rollback()
		t.Fatalf("direct synthetic agent target pin err=%v, want ErrSyntheticAgentCalibrationImmutable", err)
	}
	var pins int
	if err := pinTx.QueryRow(`SELECT count(*) FROM target_pins WHERE task_id=$1`, task.ID).Scan(&pins); err != nil {
		_ = pinTx.Rollback()
		t.Fatal(err)
	}
	if pins != 0 {
		_ = pinTx.Rollback()
		t.Fatalf("direct synthetic agent pin guard wrote %d rows, want 0", pins)
	}
	if err := pinTx.Rollback(); err != nil {
		t.Fatal(err)
	}

	taskText := fmt.Sprint(task.ID)
	const targetAudit = `{"targets":[{"host":"cal-agent-stale.invalid","port":443,"resolved_ips":["198.51.100.42"],"outcome":"requested"}]}`
	var targetPending, classicPending int64
	if err := d.QueryRow(`INSERT INTO intercept_pending(task_id,agent_name,tool_name,tool_input,reason,decision_source,audit,target_key)
VALUES($1,'stale','WebFetch','{}','stale target','target',$2::jsonb,'cal-agent-stale.invalid:443') RETURNING id`, taskText, targetAudit).Scan(&targetPending); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`INSERT INTO intercept_pending(task_id,agent_name,tool_name,tool_input,reason,decision_source,audit)
VALUES($1,'stale','Bash','{}','stale approval','rule','{}') RETURNING id`, taskText).Scan(&classicPending); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := d.ResolveTargetRequest(targetPending, true, "test", "192.0.2.1"); !errors.Is(err, ErrSyntheticAgentCalibrationImmutable) || ok {
		t.Fatalf("resolve synthetic agent target ok=%t err=%v, want false/ErrSyntheticAgentCalibrationImmutable", ok, err)
	}
	if ok, err := d.ResolveInterceptBy(classicPending, "denied", "deny", "must remain pending", "human", "test", "192.0.2.1"); !errors.Is(err, ErrSyntheticAgentCalibrationImmutable) || ok {
		t.Fatalf("resolve synthetic agent intercept ok=%t err=%v, want false/ErrSyntheticAgentCalibrationImmutable", ok, err)
	}
	if err := d.CompleteIntercept(classicPending, "run", "tool", "done", "must remain pending", false); !errors.Is(err, ErrSyntheticAgentCalibrationImmutable) {
		t.Fatalf("complete synthetic agent intercept err=%v, want ErrSyntheticAgentCalibrationImmutable", err)
	}
	if immutable, err := d.IsSystemTaskInterceptPending(classicPending); err != nil || !immutable {
		t.Fatalf("classify synthetic agent intercept immutable=%t err=%v, want true/nil", immutable, err)
	}
	if _, err := d.CreateInterceptPending(0, 0, taskText, "worker", "Bash", []byte(`{}`), "must not create"); !errors.Is(err, ErrSyntheticAgentCalibrationImmutable) {
		t.Fatalf("create synthetic agent intercept err=%v, want ErrSyntheticAgentCalibrationImmutable", err)
	}
	if _, err := d.CreateDecidedIntercept(0, 0, taskText, "worker", "Bash", []byte(`{}`), "denied", "must not create"); !errors.Is(err, ErrSyntheticAgentCalibrationImmutable) {
		t.Fatalf("create synthetic agent decided intercept err=%v, want ErrSyntheticAgentCalibrationImmutable", err)
	}
}

func createSyntheticAgentCalibrationForLifecycleTest(t *testing.T, d *DB, suffix string) (*SyntheticAgentCalibrationResult, int64) {
	t.Helper()
	profileID, err := d.SaveProfile(&LLMProfile{
		Name:        fmt.Sprintf("synthetic-agent-lifecycle-%s-%d", suffix, time.Now().UnixNano()),
		Format:      "hermes-subscription",
		BaseURL:     "http://127.0.0.1:864/v1",
		Model:       "openai/test-synthetic",
		PoolExclude: true,
		Streaming:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := d.EnsureSyntheticAgentCalibrationTask(profileID)
	if err != nil {
		t.Fatal(err)
	}
	if created.AlreadyPresent || created.Task == nil || created.IntentID <= 0 {
		t.Fatalf("unexpected lifecycle fixture: %+v", created)
	}
	t.Cleanup(func() {
		if created.Task != nil {
			_, _ = d.DeleteTaskCascade(created.Task.ID, false, false, true)
		}
		_ = d.DeleteProfile(profileID)
	})
	return created, profileID
}

func TestPrepareSyntheticAgentCalibrationClaimsExactIntentAndExplicitRetry(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	created, profileID := createSyntheticAgentCalibrationForLifecycleTest(t, d, "prepare")

	first, err := d.PrepareSyntheticAgentCalibrationRun(created.Task.ID, profileID, false)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Started || first.Retried || first.IntentID != created.IntentID || first.Attempt != 1 || first.ClaimToken == "" {
		t.Fatalf("initial fixed lease=%+v", first)
	}
	var status, state, owner string
	var token sql.NullString
	if err := d.QueryRow(`
SELECT task.status,node.state,COALESCE(node.owner,''),node.unclaim_token
FROM tasks task JOIN exploration_nodes node ON node.exploration_id=task.exploration_id
WHERE task.id=$1 AND node.id=$2`, created.Task.ID, created.IntentID).Scan(&status, &state, &owner, &token); err != nil {
		t.Fatal(err)
	}
	if status != "running" || state != "running" || owner != syntheticAgentCalibrationSource || !token.Valid || token.String != first.ClaimToken {
		t.Fatalf("prepared task/intent status=%q/%q owner=%q token=%q", status, state, owner, token.String)
	}

	second, err := d.PrepareSyntheticAgentCalibrationRun(created.Task.ID, profileID, false)
	if err != nil {
		t.Fatal(err)
	}
	if second.Started || second.ClaimToken != "" || second.Attempt != 1 {
		t.Fatalf("repeat start must be an observation, got %+v", second)
	}

	store := d.Exploration(created.Task.ExplorationID)
	if err := store.SettleIntentClaim(created.IntentID, first.ClaimToken, "done"); err != nil {
		t.Fatal(err)
	}
	won, err := d.FinishSyntheticAgentCalibration(created.Task.ID, "failed", 0, 2)
	if err != nil || !won {
		t.Fatalf("finish failed attempt won=%t err=%v", won, err)
	}
	if won, err := d.FinishSyntheticAgentCalibration(created.Task.ID, "failed", 0, 2); err != nil || won {
		t.Fatalf("second finish won=%t err=%v, want false/nil", won, err)
	}

	observed, err := d.PrepareSyntheticAgentCalibrationRun(created.Task.ID, profileID, false)
	if err != nil {
		t.Fatal(err)
	}
	if observed.Started || observed.Attempt != 1 {
		t.Fatalf("ordinary repeat after failure must not retry, got %+v", observed)
	}

	retry, err := d.PrepareSyntheticAgentCalibrationRun(created.Task.ID, profileID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !retry.Started || !retry.Retried || retry.Attempt != 2 || retry.IntentID != created.IntentID || retry.ClaimToken == "" || retry.ClaimToken == first.ClaimToken {
		t.Fatalf("explicit retry lease=%+v", retry)
	}
	var retryResets, attemptStarts, finishes int
	if err := d.QueryRow(`
SELECT
  count(*) FILTER (WHERE kind='retry_reset'),
  count(*) FILTER (WHERE kind='attempt_started'),
  count(*) FILTER (WHERE kind='attempt_finished')
FROM activity WHERE exploration_id=$1 AND worker='synthetic-calibration'`, created.Task.ExplorationID).Scan(&retryResets, &attemptStarts, &finishes); err != nil {
		t.Fatal(err)
	}
	if retryResets != 1 || attemptStarts != 2 || finishes != 1 {
		t.Fatalf("attempt receipts retry=%d starts=%d finishes=%d, want 1/2/1", retryResets, attemptStarts, finishes)
	}
}

func TestPrepareSyntheticAgentCalibrationRejectsFreshInterruptedLeaseAndAcceptsStaleOne(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	created, profileID := createSyntheticAgentCalibrationForLifecycleTest(t, d, "stale")
	run, err := d.PrepareSyntheticAgentCalibrationRun(created.Task.ID, profileID, false)
	if err != nil || !run.Started {
		t.Fatalf("initial prepare=%+v err=%v", run, err)
	}
	if won, err := d.MarkSyntheticAgentCalibrationInterrupted(created.Task.ID); err != nil || !won {
		t.Fatalf("mark interrupted won=%t err=%v", won, err)
	}
	if _, err := d.PrepareSyntheticAgentCalibrationRun(created.Task.ID, profileID, true); !errors.Is(err, ErrSyntheticAgentCalibrationConflict) {
		t.Fatalf("fresh interrupted lease retry err=%v, want ErrSyntheticAgentCalibrationConflict", err)
	}
	if _, err := d.Exec(`UPDATE exploration_nodes SET last_heartbeat_at=now()-interval '2 minutes' WHERE id=$1`, created.IntentID); err != nil {
		t.Fatal(err)
	}
	retry, err := d.PrepareSyntheticAgentCalibrationRun(created.Task.ID, profileID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !retry.Started || !retry.Retried || retry.Attempt != 2 {
		t.Fatalf("stale lease retry=%+v", retry)
	}
}

func TestAbortPreparedSyntheticAgentCalibrationFencesExactLeaseAndFailsAtomically(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	created, profileID := createSyntheticAgentCalibrationForLifecycleTest(t, d, "abort")

	run, err := d.PrepareSyntheticAgentCalibrationRun(created.Task.ID, profileID, false)
	if err != nil || !run.Started || run.ClaimToken == "" {
		t.Fatalf("prepare=%+v err=%v", run, err)
	}
	if won, err := d.AbortPreparedSyntheticAgentCalibration(created.Task.ID, run.IntentID, "wrong-"+run.ClaimToken); err != nil || won {
		t.Fatalf("wrong-token abort won=%t err=%v, want false/nil", won, err)
	}
	var runningStatus, runningState string
	var runningToken sql.NullString
	if err := d.QueryRow(`
SELECT task.status,node.state,node.unclaim_token
FROM tasks task JOIN exploration_nodes node ON node.exploration_id=task.exploration_id
WHERE task.id=$1 AND node.id=$2`, created.Task.ID, run.IntentID).Scan(&runningStatus, &runningState, &runningToken); err != nil {
		t.Fatal(err)
	}
	if runningStatus != "running" || runningState != "running" || !runningToken.Valid || runningToken.String != run.ClaimToken {
		t.Fatalf("wrong-token abort mutated task/intent status=%q/%q token=%q", runningStatus, runningState, runningToken.String)
	}

	if won, err := d.AbortPreparedSyntheticAgentCalibration(created.Task.ID, run.IntentID, run.ClaimToken); err != nil || !won {
		t.Fatalf("exact-token abort won=%t err=%v", won, err)
	}
	var terminalStatus, terminalState string
	var terminalToken sql.NullString
	if err := d.QueryRow(`
SELECT task.status,node.state,node.unclaim_token
FROM tasks task JOIN exploration_nodes node ON node.exploration_id=task.exploration_id
WHERE task.id=$1 AND node.id=$2`, created.Task.ID, run.IntentID).Scan(&terminalStatus, &terminalState, &terminalToken); err != nil {
		t.Fatal(err)
	}
	if terminalStatus != "failed" || terminalState != "blocked" || terminalToken.Valid {
		t.Fatalf("exact-token abort did not commit terminal pair status=%q/%q token=%q", terminalStatus, terminalState, terminalToken.String)
	}
	var abortReceipts int
	if err := d.QueryRow(`SELECT count(*) FROM activity WHERE exploration_id=$1 AND worker='synthetic-calibration' AND kind='attempt_aborted'`, created.Task.ExplorationID).Scan(&abortReceipts); err != nil {
		t.Fatal(err)
	}
	if abortReceipts != 1 {
		t.Fatalf("attempt_aborted receipts=%d, want 1", abortReceipts)
	}
	if won, err := d.AbortPreparedSyntheticAgentCalibration(created.Task.ID, run.IntentID, run.ClaimToken); err != nil || won {
		t.Fatalf("repeat abort won=%t err=%v, want false/nil", won, err)
	}
}

func TestMarkSyntheticAgentCalibrationInterruptedRepairsOpenCancellationRollback(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	created, profileID := createSyntheticAgentCalibrationForLifecycleTest(t, d, "open-rollback")

	run, err := d.PrepareSyntheticAgentCalibrationRun(created.Task.ID, profileID, false)
	if err != nil || !run.Started {
		t.Fatalf("prepare=%+v err=%v", run, err)
	}
	// This is the legacy generic runIntent cancellation transition: it returns
	// the claim to open but intentionally leaves its owner/heartbeat provenance.
	// A crash at this point previously made both Finish and restart recovery reject
	// the policy graph forever.
	if err := d.Exploration(created.Task.ExplorationID).SettleIntentClaim(run.IntentID, run.ClaimToken, "open"); err != nil {
		t.Fatal(err)
	}
	if won, err := d.MarkSyntheticAgentCalibrationInterrupted(created.Task.ID); err != nil || !won {
		t.Fatalf("mark interrupted open rollback won=%t err=%v", won, err)
	}
	var status, state string
	if err := d.QueryRow(`
SELECT task.status,node.state
FROM tasks task JOIN exploration_nodes node ON node.exploration_id=task.exploration_id
WHERE task.id=$1 AND node.id=$2`, created.Task.ID, run.IntentID).Scan(&status, &state); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || state != "blocked" {
		t.Fatalf("open rollback recovery status=%q/%q, want failed/blocked", status, state)
	}
	retry, err := d.PrepareSyntheticAgentCalibrationRun(created.Task.ID, profileID, true)
	if err != nil || !retry.Started || !retry.Retried || retry.Attempt != 2 {
		t.Fatalf("retry after recovered open rollback=%+v err=%v", retry, err)
	}
}

func TestPrepareSyntheticAgentCalibrationHasOneConcurrentLeaseWinner(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	created, profileID := createSyntheticAgentCalibrationForLifecycleTest(t, d, "concurrent")

	start := make(chan struct{})
	results := make(chan *SyntheticAgentCalibrationRun, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			run, err := d.PrepareSyntheticAgentCalibrationRun(created.Task.ID, profileID, false)
			if err != nil {
				errs <- err
				return
			}
			results <- run
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var winners int
	for run := range results {
		if run.Started {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent fixed lease winners=%d, want 1", winners)
	}
}

func TestPrepareSyntheticAgentCalibrationRejectsNonCanonicalGraph(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	created, profileID := createSyntheticAgentCalibrationForLifecycleTest(t, d, "tamper")
	if _, err := d.Exec(`
INSERT INTO exploration_nodes(exploration_id,kind,payload,priority,state,origin)
VALUES($1,'intent','{"summary":"extra"}'::jsonb,99,'open','test')`, created.Task.ExplorationID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.PrepareSyntheticAgentCalibrationRun(created.Task.ID, profileID, false); !errors.Is(err, ErrSyntheticAgentCalibrationConflict) {
		t.Fatalf("noncanonical fixed graph prepare err=%v, want ErrSyntheticAgentCalibrationConflict", err)
	}
}
