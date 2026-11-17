package db

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestSyntheticCalibrationSystemKind(t *testing.T) {
	if SyntheticCalibrationSystemKind == "" {
		t.Fatal("synthetic calibration system kind must be non-empty")
	}
	if !(&Task{SystemKind: SyntheticCalibrationSystemKind}).IsSyntheticCalibration() {
		t.Fatal("synthetic system-kind task was not recognized")
	}
	if (&Task{}).IsSyntheticCalibration() || (&Task{SystemKind: "other"}).IsSyntheticCalibration() {
		t.Fatal("ordinary or unknown system-kind task was misclassified")
	}
}

func TestEnsureSyntheticCalibrationTaskTerminalGraphAndIdempotency(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	spec := syntheticCalibrationTestSpec(time.Now().UnixNano())
	created, err := d.ensureSyntheticCalibrationTask(spec)
	if err != nil {
		t.Fatal(err)
	}
	if created.AlreadyPresent || created.Task == nil || created.Task.ID <= 0 || created.AssetID <= 0 || created.OriginFactID <= 0 || created.IntentID <= 0 {
		t.Fatalf("unexpected creation result: %+v", created)
	}
	t.Cleanup(func() {
		if _, err := d.DeleteTaskCascade(created.Task.ID, true, false); err != nil {
			t.Errorf("cleanup synthetic calibration: %v", err)
		}
	})

	task := created.Task
	if task.Status != "done" || task.Paused || task.Queued || !task.TargetGate || task.CoverageEnabled || task.LLMRotate || task.TimeoutSeconds != 0 || task.FirstRunAt != nil || task.DeadlineAt != nil || task.CompletedAt == nil || task.LLMProfileID != nil || task.ActiveLLMProfileID != nil || len(task.LLMProfileIDs) != 0 || task.SystemKind != SyntheticCalibrationSystemKind || !task.IsSyntheticCalibration() {
		t.Fatalf("synthetic calibration task is runnable or not terminal: %+v", task)
	}

	var (
		assetType, assetName, assetCategory, assetDescription string
		companyNull                                           bool
	)
	if err := d.QueryRow(`
SELECT type, COALESCE(app_name,''), COALESCE(category,''), COALESCE(app_description,''), company_id IS NULL
FROM assets WHERE id=$1`, created.AssetID).Scan(&assetType, &assetName, &assetCategory, &assetDescription, &companyNull); err != nil {
		t.Fatal(err)
	}
	if assetType != "app" || assetName != spec.AssetName || assetCategory != spec.AssetCategory || assetDescription != spec.AssetDescription || !companyNull {
		t.Fatalf("synthetic fixture is not canonical: type=%q name=%q category=%q description=%q company_null=%t", assetType, assetName, assetCategory, assetDescription, companyNull)
	}

	var nodes, edges, anchors int
	if err := d.QueryRow(`
SELECT
  (SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1),
  (SELECT count(*) FROM exploration_edges WHERE exploration_id=$1),
  (SELECT count(*) FROM exploration_anchors anchor JOIN exploration_nodes node ON node.id=anchor.node_id WHERE node.exploration_id=$1 AND anchor.asset_id=$2)`,
		task.ExplorationID, created.AssetID).Scan(&nodes, &edges, &anchors); err != nil {
		t.Fatal(err)
	}
	if want := len(spec.Facts) + 2; nodes != want || anchors != want || edges != len(spec.Facts)+1 {
		t.Fatalf("synthetic graph counts nodes=%d edges=%d anchors=%d, want %d/%d/%d", nodes, edges, anchors, want, len(spec.Facts)+1, want)
	}

	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := verifySyntheticCalibrationNoForbiddenRecords(tx, task.ID, task.ExplorationID); err != nil {
		_ = tx.Rollback()
		t.Fatalf("synthetic calibration created a forbidden ledger row: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	// CAL-00 is not an inheritance source and must not enter scheduler feeds.
	// Both checks live in db so direct callers cannot bypass the server/UI.
	if _, err := d.CreateTaskWithOptions("must not inherit calibration", "fixture safety", TaskCreateOptions{SourceTaskIDs: []int64{task.ID}}); !errors.Is(err, ErrSystemTaskImmutable) {
		t.Fatalf("create child with CAL-00 source err=%v, want ErrSystemTaskImmutable", err)
	}
	var inheritedRelations int
	if err := d.QueryRow(`SELECT count(*) FROM task_relations WHERE source_task_id=$1`, task.ID).Scan(&inheritedRelations); err != nil {
		t.Fatal(err)
	}
	if inheritedRelations != 0 {
		t.Fatalf("CAL-00 unexpectedly has %d inherited child relations", inheritedRelations)
	}
	events, err := d.NewTasksSince(task.ID - 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.TaskID == task.ID {
			t.Fatalf("CAL-00 reached task-create trigger feed: %+v", event)
		}
	}
	if _, err := d.Assets().DeleteByIDs([]int64{created.AssetID}); !errors.Is(err, ErrSyntheticCalibrationAssetImmutable) {
		t.Fatalf("delete CAL-00 fixture err=%v, want ErrSyntheticCalibrationAssetImmutable", err)
	}
	if _, err := d.Assets().UpsertApp(UpsertAppReq{Name: "must not replace CAL-00", BundleID: syntheticCalibrationBundleID}); !errors.Is(err, ErrSyntheticCalibrationAssetImmutable) {
		t.Fatalf("upsert reserved CAL-00 bundle err=%v, want ErrSyntheticCalibrationAssetImmutable", err)
	}
	ordinary, err := d.CreateTaskWithOptions("ordinary attach probe", "fixture isolation", TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.DeleteTaskCascade(ordinary.ID, true, true, true) })
	if _, err := d.Assets().AttachAssetsToTask(ordinary.ID, []int64{created.AssetID}, "must not reuse CAL-00"); !errors.Is(err, ErrSyntheticCalibrationAssetImmutable) {
		t.Fatalf("attach CAL-00 fixture err=%v, want ErrSyntheticCalibrationAssetImmutable", err)
	}
	if _, err := d.QueueTaskArchive(task.ID); !errors.Is(err, ErrSystemTaskImmutable) {
		t.Fatalf("archive CAL-00 err=%v, want ErrSystemTaskImmutable", err)
	}
	// writeTargetPin is intentionally low-level, so it keeps its own final
	// transaction-local system-task check even if a future caller skips the
	// pending-row guard above it.
	pinTx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeTargetPin(pinTx, TargetScopeRef{TaskID: task.ID}, 0, &InterceptTarget{
		Host: "cal-direct-pin.invalid", Port: 443, ResolvedIPs: []string{"198.51.100.42"},
	}, "test", "192.0.2.1"); !errors.Is(err, ErrSystemTaskImmutable) {
		_ = pinTx.Rollback()
		t.Fatalf("direct CAL-00 target pin err=%v, want ErrSystemTaskImmutable", err)
	}
	var directPins int
	if err := pinTx.QueryRow(`SELECT count(*) FROM target_pins WHERE task_id=$1`, task.ID).Scan(&directPins); err != nil {
		_ = pinTx.Rollback()
		t.Fatal(err)
	}
	if directPins != 0 {
		_ = pinTx.Rollback()
		t.Fatalf("direct CAL-00 pin guard wrote %d in-transaction rows, want 0", directPins)
	}
	if err := pinTx.Rollback(); err != nil {
		t.Fatal(err)
	}

	// Simulate a stale/pre-feature compact record without allowing normal code
	// to construct one. Every archive lifecycle entry point must refuse it and
	// the worker claim must quarantine it rather than touching the receipt.
	var staleArchiveID int64
	if err := d.QueryRow(`INSERT INTO task_archives(task_id,state,phase,task_name,task_description,task_goal,original_status)
VALUES($1,'ready','ready','CAL-00','synthetic fixture','fixed receipt','done') RETURNING id`, task.ID).Scan(&staleArchiveID); err != nil {
		t.Fatal(err)
	}
	if system, err := d.IsSystemTaskArchive(staleArchiveID); err != nil || !system {
		t.Fatalf("stale archive system classification=%t err=%v, want true/nil", system, err)
	}
	if _, err := d.QueueTaskArchiveRestore(staleArchiveID); !errors.Is(err, ErrSystemTaskImmutable) {
		t.Fatalf("restore CAL-00 archive err=%v, want ErrSystemTaskImmutable", err)
	}
	if _, err := d.QueueTaskArchiveDelete(staleArchiveID); !errors.Is(err, ErrSystemTaskImmutable) {
		t.Fatalf("delete CAL-00 archive err=%v, want ErrSystemTaskImmutable", err)
	}
	if _, err := d.Exec(`UPDATE task_archives SET state=$2,phase='queued' WHERE id=$1`, staleArchiveID, ArchiveQueued); err != nil {
		t.Fatal(err)
	}
	if claimed, err := d.ClaimTaskArchiveJob(context.Background()); err != nil || claimed != nil {
		t.Fatalf("claim stale CAL-00 archive=%+v err=%v, want nil/nil", claimed, err)
	}
	if stale, err := d.GetTaskArchive(staleArchiveID); err != nil || stale == nil || stale.State != ArchiveFailed || stale.Phase != "blocked" {
		t.Fatalf("stale CAL-00 archive after claim=%+v err=%v, want archive_failed/blocked", stale, err)
	}
	if _, err := d.Exec(`UPDATE task_archives SET state=$2,phase='starting' WHERE id=$1`, staleArchiveID, Restoring); err != nil {
		t.Fatal(err)
	}
	if err := d.RecoverTaskArchiveJobs(); err != nil {
		t.Fatal(err)
	}
	if stale, err := d.GetTaskArchive(staleArchiveID); err != nil || stale == nil || stale.State != RestoreFailed || stale.Phase != "blocked" {
		t.Fatalf("stale CAL-00 archive after recovery=%+v err=%v, want restore_failed/blocked", stale, err)
	}
	if _, err := d.Exec(`DELETE FROM task_archives WHERE id=$1`, staleArchiveID); err != nil {
		t.Fatal(err)
	}

	// Global approval routes are addressed by a pending-row id, not a task URL.
	// Seed stale rows directly and prove that their DB resolution paths cannot
	// settle them, write pins, or create new target-gate receipts for CAL-00.
	taskText := fmt.Sprint(task.ID)
	legacyTaskText := "000" + taskText
	var legacyTargetPending, legacyClassicPending int64
	const legacyTargetAudit = `{"targets":[{"host":"cal-legacy.invalid","port":443,"resolved_ips":["198.51.100.42"],"outcome":"requested"}]}`
	if err := d.QueryRow(`INSERT INTO intercept_pending(task_id,agent_name,tool_name,tool_input,reason,decision_source,audit,target_key)
VALUES($1,'legacy','WebFetch','{}','legacy target','target',$2::jsonb,'cal-legacy.invalid:443') RETURNING id`, legacyTaskText, legacyTargetAudit).Scan(&legacyTargetPending); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`INSERT INTO intercept_pending(task_id,agent_name,tool_name,tool_input,reason,decision_source,audit)
VALUES($1,'legacy','Bash','{}','legacy approval','rule','{}') RETURNING id`, legacyTaskText).Scan(&legacyClassicPending); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := d.ResolveTargetRequest(legacyTargetPending, true, "test", "192.0.2.1"); !errors.Is(err, ErrInvalidInterceptTaskID) || ok {
		t.Fatalf("resolve noncanonical CAL-00 target request ok=%t err=%v, want false/ErrInvalidInterceptTaskID", ok, err)
	}
	if ok, err := d.ResolveInterceptBy(legacyClassicPending, "denied", "deny", "must remain pending", "human", "test", "192.0.2.1"); !errors.Is(err, ErrInvalidInterceptTaskID) || ok {
		t.Fatalf("resolve noncanonical CAL-00 approval ok=%t err=%v, want false/ErrInvalidInterceptTaskID", ok, err)
	}
	if err := d.CompleteIntercept(legacyClassicPending, "legacy-run", "legacy-tool", "done", "must remain pending", false); !errors.Is(err, ErrInvalidInterceptTaskID) {
		t.Fatalf("complete noncanonical CAL-00 approval err=%v, want ErrInvalidInterceptTaskID", err)
	}
	if system, err := d.IsSystemTaskInterceptPending(legacyClassicPending); !errors.Is(err, ErrInvalidInterceptTaskID) || system {
		t.Fatalf("classify noncanonical CAL-00 approval system=%t err=%v, want false/ErrInvalidInterceptTaskID", system, err)
	}
	if _, err := d.CreateInterceptPending(0, 0, legacyTaskText, "worker", "Bash", []byte(`{}`), "must not create"); !errors.Is(err, ErrInvalidInterceptTaskID) {
		t.Fatalf("create noncanonical CAL-00 approval err=%v, want ErrInvalidInterceptTaskID", err)
	}
	if _, err := d.CreateDecidedIntercept(0, 0, legacyTaskText, "worker", "Bash", []byte(`{}`), "denied", "must not create"); !errors.Is(err, ErrInvalidInterceptTaskID) {
		t.Fatalf("create noncanonical decided CAL-00 approval err=%v, want ErrInvalidInterceptTaskID", err)
	}
	var legacyPins, legacyPending int
	if err := d.QueryRow(`SELECT count(*) FROM target_pins WHERE task_id=$1`, task.ID).Scan(&legacyPins); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM intercept_pending WHERE task_id=$1 AND status='pending'`, legacyTaskText).Scan(&legacyPending); err != nil {
		t.Fatal(err)
	}
	if legacyPins != 0 || legacyPending != 2 {
		t.Fatalf("noncanonical CAL-00 row changed: pins=%d pending=%d, want 0/2", legacyPins, legacyPending)
	}
	if _, err := d.ensureSyntheticCalibrationTask(spec); !errors.Is(err, ErrSyntheticCalibrationConflict) {
		t.Fatalf("replay with noncanonical CAL-00 approval err=%v, want ErrSyntheticCalibrationConflict", err)
	}
	if _, err := d.Exec(`DELETE FROM intercept_pending WHERE task_id=$1`, legacyTaskText); err != nil {
		t.Fatal(err)
	}
	// The replay query has a separate SQL branch for signed decimal text. Test
	// it explicitly rather than relying only on the Go parser regression above.
	plusTaskText := "+" + taskText
	var plusLegacyPending int64
	if err := d.QueryRow(`INSERT INTO intercept_pending(task_id,agent_name,tool_name,tool_input,reason,decision_source,audit)
VALUES($1,'legacy','Bash','{}','legacy signed alias','rule','{}') RETURNING id`, plusTaskText).Scan(&plusLegacyPending); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM intercept_pending WHERE id=$1`, plusLegacyPending) })
	if _, err := d.ensureSyntheticCalibrationTask(spec); !errors.Is(err, ErrSyntheticCalibrationConflict) {
		t.Fatalf("replay with signed CAL-00 approval err=%v, want ErrSyntheticCalibrationConflict", err)
	}
	if _, err := d.Exec(`DELETE FROM intercept_pending WHERE id=$1`, plusLegacyPending); err != nil {
		t.Fatal(err)
	}
	// A pre-task-model opaque label cannot ever pin a task, but a human denial
	// remains safe and must be able to release the blocked caller.
	var opaqueTargetPending int64
	if err := d.QueryRow(`INSERT INTO intercept_pending(task_id,agent_name,tool_name,tool_input,reason,decision_source,audit,target_key)
VALUES('legacy-label','legacy','WebFetch','{}','legacy target','target',$1::jsonb,'opaque-legacy.invalid:443') RETURNING id`, legacyTargetAudit).Scan(&opaqueTargetPending); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := d.ResolveTargetRequest(opaqueTargetPending, true, "test", "192.0.2.1"); !errors.Is(err, ErrInvalidInterceptTaskID) || ok {
		t.Fatalf("allow opaque legacy target request ok=%t err=%v, want false/ErrInvalidInterceptTaskID", ok, err)
	}
	if pin, ok, err := d.ResolveTargetRequest(opaqueTargetPending, false, "test", "192.0.2.1"); err != nil || !ok || pin != nil {
		t.Fatalf("deny opaque legacy target request pin=%+v ok=%t err=%v, want nil/true/nil", pin, ok, err)
	}
	if _, err := d.Exec(`DELETE FROM intercept_pending WHERE id=$1`, opaqueTargetPending); err != nil {
		t.Fatal(err)
	}

	var targetPending, classicPending int64
	if err := d.QueryRow(`INSERT INTO intercept_pending(task_id,agent_name,tool_name,tool_input,reason,decision_source,audit,target_key)
VALUES($1,'stale','WebFetch','{}','stale target','target','{}','cal-stale.invalid:443') RETURNING id`, taskText).Scan(&targetPending); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`INSERT INTO intercept_pending(task_id,agent_name,tool_name,tool_input,reason,decision_source,audit)
VALUES($1,'stale','Bash','{}','stale approval','rule','{}') RETURNING id`, taskText).Scan(&classicPending); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := d.ResolveTargetRequest(targetPending, false, "test", "192.0.2.1"); !errors.Is(err, ErrSystemTaskImmutable) || ok {
		t.Fatalf("resolve stale CAL-00 target request ok=%t err=%v, want false/ErrSystemTaskImmutable", ok, err)
	}
	if ok, err := d.ResolveInterceptBy(classicPending, "denied", "deny", "must remain pending", "human", "test", "192.0.2.1"); !errors.Is(err, ErrSystemTaskImmutable) || ok {
		t.Fatalf("resolve stale CAL-00 approval ok=%t err=%v, want false/ErrSystemTaskImmutable", ok, err)
	}
	if _, err := d.CreateInterceptPending(0, 0, taskText, "worker", "Bash", []byte(`{}`), "must not create"); !errors.Is(err, ErrSystemTaskImmutable) {
		t.Fatalf("create CAL-00 approval err=%v, want ErrSystemTaskImmutable", err)
	}
	if _, _, err := d.FindOrCreateTargetRequest(TargetRequest{Scope: TargetScopeRef{TaskID: task.ID}, Host: "cal-must-not-request.invalid", Port: 443, ToolName: "WebFetch"}); !errors.Is(err, ErrSystemTaskImmutable) {
		t.Fatalf("create CAL-00 target request err=%v, want ErrSystemTaskImmutable", err)
	}
	if _, err := d.LogTargetDecision(TargetDecision{Scope: TargetScopeRef{TaskID: task.ID}, ToolName: "WebFetch", Status: "denied"}); !errors.Is(err, ErrSystemTaskImmutable) {
		t.Fatalf("log CAL-00 target decision err=%v, want ErrSystemTaskImmutable", err)
	}
	var pendingCount int
	if err := d.QueryRow(`SELECT count(*) FROM intercept_pending WHERE task_id=$1 AND status='pending'`, taskText).Scan(&pendingCount); err != nil {
		t.Fatal(err)
	}
	if pendingCount != 2 {
		t.Fatalf("CAL-00 approval rows changed: pending=%d, want 2 stale rows only", pendingCount)
	}
	if _, err := d.Exec(`DELETE FROM intercept_pending WHERE task_id=$1`, taskText); err != nil {
		t.Fatal(err)
	}

	replayed, err := d.ensureSyntheticCalibrationTask(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.AlreadyPresent || replayed.Task == nil || replayed.Task.ID != task.ID || replayed.AssetID != created.AssetID || replayed.OriginFactID != created.OriginFactID || replayed.IntentID != created.IntentID {
		t.Fatalf("unexpected idempotent replay: %+v (created %+v)", replayed, created)
	}
	for id, nodeID := range created.FactIDs {
		if replayed.FactIDs[id] != nodeID {
			t.Fatalf("replayed fact %q=%d, want %d", id, replayed.FactIDs[id], nodeID)
		}
	}

	// A replay must fail closed if an operator or a bug turns the immutable UI
	// receipt into something else.  It must not overwrite the task or create a
	// second CAL-00 row.
	if _, err := d.Exec(`UPDATE tasks SET name='tampered CAL-00' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ensureSyntheticCalibrationTask(spec); !errors.Is(err, ErrSyntheticCalibrationConflict) {
		t.Fatalf("replay after task mutation err=%v, want ErrSyntheticCalibrationConflict", err)
	}

	var taskCount int
	if err := d.QueryRow(`SELECT count(*) FROM tasks WHERE exploration_id=$1`, task.ExplorationID).Scan(&taskCount); err != nil {
		t.Fatal(err)
	}
	if taskCount != 1 {
		t.Fatalf("tampered replay created %d task rows, want 1", taskCount)
	}
}

func TestSyntheticCalibrationSpecFailsClosed(t *testing.T) {
	bad := defaultSyntheticCalibrationSpec
	bad.Facts = nil
	if err := validateSyntheticCalibrationSpec(bad); !errors.Is(err, ErrSyntheticCalibrationConflict) {
		t.Fatalf("empty fixed fact list err=%v, want ErrSyntheticCalibrationConflict", err)
	}
	bad = defaultSyntheticCalibrationSpec
	bad.SystemKind = ""
	if err := validateSyntheticCalibrationSpec(bad); !errors.Is(err, ErrSyntheticCalibrationConflict) {
		t.Fatalf("missing fixed system kind err=%v, want ErrSyntheticCalibrationConflict", err)
	}
}

func syntheticCalibrationTestSpec(nonce int64) syntheticCalibrationSpec {
	spec := defaultSyntheticCalibrationSpec
	tag := fmt.Sprintf("test-%d", nonce)
	spec.Marker += "-" + tag
	spec.BundleID += "-" + tag
	spec.Name += " " + tag
	spec.Description += " Test receipt " + tag + "."
	spec.Goal += " Test receipt " + tag + "."
	spec.AssetName += " " + tag
	spec.AssetDescription += " Test receipt " + tag + "."
	spec.Facts = append([]syntheticCalibrationFact(nil), defaultSyntheticCalibrationSpec.Facts...)
	for i := range spec.Facts {
		spec.Facts[i].Detail += " Test receipt " + tag + "."
	}
	return spec
}
